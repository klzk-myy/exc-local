// chaos_splitbrain_test.go — Phase-04.5 Task 4.5.3.2 scenarios B & C
// (spec §2.7, §5.40, §24 #303): network-split fencing and standby
// promotion parity against the REAL Redis epoch-lease backend
// (engine:leader:{shard} "{token}:{epoch}", spec §4.2/§18.6.2).
//
// Scenario B (split brain): a leader that loses the fencing race must
// self-terminate — the SIGTERM-equivalent modeled here is a write gate
// that permanently refuses writes once VerifyLeaderEpoch reports the
// lease missing or held at a higher epoch. The Redis primitives under
// test are real: SET NX PX acquire, Lua compare-and-del revoke, Lua
// compare-and-pexpire refresh.
//
// Scenario C (promotion parity): a promoted standby must NOT open order
// ingress until the §18.6 audit validates book_seq == wal_tail; a
// divergent book freezes the shard instead of admitting a single order.
//
// Gated on a reachable Redis: set REDIS_URL or EXC_REDIS_TEST_ADDR.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	exredis "exchange/internal/redis"
	excerrors "exchange/pkg/errors"
)

// chaosLeases returns a live Redis-backed OrchLeaseBackend, skipping when
// no test Redis is reachable.
func chaosLeases(t *testing.T) (*exredis.Client, *OrchRedisLeases) {
	t.Helper()
	addr := ""
	if raw := strings.TrimSpace(os.Getenv("REDIS_URL")); raw != "" {
		addr = strings.TrimPrefix(strings.TrimPrefix(raw, "redis://"), "rediss://")
		if h, _, err := net.SplitHostPort(addr); err != nil || h == "" {
			addr = ""
		}
	}
	if addr == "" {
		addr = os.Getenv("EXC_REDIS_TEST_ADDR")
	}
	if addr == "" {
		t.Skip("set REDIS_URL or EXC_REDIS_TEST_ADDR to run live fencing tests")
	}
	client := exredis.New(addr, "", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		_ = client.Close()
		t.Skipf("redis at %s unreachable: %v", addr, err)
	}
	leases, err := NewOrchRedisLeases(client)
	if err != nil {
		t.Fatalf("lease adapter: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, leases
}

// errSelfTerminated is the SIGTERM-equivalent: once the fencing token is
// lost/superseded the node permanently refuses writes — it never
// "un-fences" itself back into the write path.
var errSelfTerminated = errors.New("fencing token lost — node self-terminated (SIGTERM-equivalent)")

// fencedWriter models the leader's write path: every write is gated on the
// monotonic epoch-lease check (epoch_local == epoch_current). Any lease
// absence or higher epoch observed is terminal for the writer.
type fencedWriter struct {
	orch   *RecoveryOrchestrator
	shard  int
	epoch  uint64
	writes atomic.Int64
	fenced atomic.Bool
}

func (w *fencedWriter) Write(ctx context.Context) error {
	if w.fenced.Load() {
		return errSelfTerminated // latched — a fenced node never resumes writes
	}
	chk, err := w.orch.VerifyLeaderEpoch(ctx, w.shard, w.epoch)
	if err != nil {
		return err // fail-closed: cannot confirm leadership → refuse the write
	}
	if !chk.LeasePresent || !chk.Matches {
		w.fenced.Store(true)
		return errSelfTerminated
	}
	w.writes.Add(1)
	return nil
}

// TestChaosSplitBrainFencesPartitionedLeader — scenario B. Timeline:
//
//	node-a holds engine:leader:{shard} at epoch 5 and is writing
//	network partition → node-a unreachable (liveness=false)
//	orchestrator fences + promotes node-b at epoch 6 (real Redis)
//	node-a heals: heartbeat refresh rejected (Lua compare), NX re-acquire
//	  rejected, epoch check shows 6 > 5 → self-terminates; write count
//	  frozen — zero dual-writes.
//
// A second leg covers lease loss by TTL expiry alone: a partitioned node
// whose lease simply expires must refuse writes even with no successor.
func TestChaosSplitBrainFencesPartitionedLeader(t *testing.T) {
	client, leases := chaosLeases(t)
	ctx := context.Background()
	const shard = 4301
	_ = client.Del(ctx, orchLeaderKey(shard))
	defer client.Del(context.Background(), orchLeaderKey(shard))

	// Node A acquires the leadership lease at epoch 5. The TTL is long:
	// while partitioned, A cannot refresh — but the key itself persists,
	// so fencing must actively revoke it.
	acq, err := leases.AcquireLeader(ctx, shard, "node-a", 5, 30*time.Second)
	if err != nil || !acq {
		t.Fatalf("node-a acquire: acq=%v err=%v", acq, err)
	}

	clk := newFakeClock()
	eng := newTestEngine(clk, healthyData(), &fakeTelemetry{
		book: map[int]uint64{shard: 1}, wal: map[int]uint64{shard: 1},
	}, healthyFeeds())
	o := newTestOrch(t, clk, []int{shard}, eng, func(d *OrchDeps) {
		d.Leases = leases
		d.Standbys = fakeStandbys{token: "node-b"}
		d.Liveness = fakeLiveness{alive: map[string]bool{}} // node-a unreachable across the partition
		d.Now = time.Now                                    // real clock → real promotion latency
	})

	nodeA := &fencedWriter{orch: o, shard: shard, epoch: 5}
	for i := 0; i < 3; i++ {
		if err := nodeA.Write(ctx); err != nil {
			t.Fatalf("healthy leader write %d refused: %v", i, err)
		}
	}
	if nodeA.writes.Load() != 3 {
		t.Fatalf("pre-partition writes %d", nodeA.writes.Load())
	}

	// --- Partition: orchestrator fences the unreachable leader and
	//     promotes the standby at the next monotonic epoch.
	p, err := o.FenceAndPromote(ctx, shard)
	if err != nil {
		t.Fatalf("fence+promote: %v", err)
	}
	t.Logf("fencing+promotion latency: %s (RTO %s)", p.Elapsed, OrchPromotionRTO)
	if !p.Promoted || !p.RevokedStale || p.Token != "node-b" || p.Epoch != 6 {
		t.Fatalf("bad promotion record: %+v", p)
	}
	if !p.WithinRTO || p.Elapsed > OrchPromotionRTO {
		t.Fatalf("promotion exceeded RTO: %s > %s", p.Elapsed, OrchPromotionRTO)
	}
	if v, ok, err := leases.LeaderValue(ctx, shard); err != nil || !ok || v != "node-b:6" {
		t.Fatalf("lease after fencing: %q ok=%v err=%v", v, ok, err)
	}

	// --- Node A heals from the partition. Every fenced path must reject it:
	// heartbeat refresh is token-checked in Lua — the stale {token}:{epoch}
	// no longer matches, so the old leader cannot even extend its lease.
	ok, err := client.RefreshLeader(ctx, shard, "node-a", 5, 30*time.Second)
	if err != nil || ok {
		t.Fatalf("stale leader heartbeat accepted: ok=%v err=%v", ok, err)
	}
	// NX re-acquire at the stale epoch fails — node-b holds the key.
	acq, err = leases.AcquireLeader(ctx, shard, "node-a", 5, 30*time.Second)
	if err != nil || acq {
		t.Fatalf("stale leader re-acquired the lease: acq=%v err=%v", acq, err)
	}
	// The epoch check is the writer's fencing token: local 5 < current 6.
	if err := nodeA.Write(ctx); !errors.Is(err, errSelfTerminated) {
		t.Fatalf("partitioned leader wrote after fencing (writes=%d err=%v)",
			nodeA.writes.Load(), err)
	}
	if nodeA.writes.Load() != 3 {
		t.Fatalf("dual-write: fenced leader committed write %d", nodeA.writes.Load())
	}
	// Fencing latches — a retried write stays refused.
	if err := nodeA.Write(ctx); !errors.Is(err, errSelfTerminated) {
		t.Fatal("fenced writer resumed writes")
	}
	// Node B (epoch 6) is the single writer now.
	nodeB := &fencedWriter{orch: o, shard: shard, epoch: 6}
	if err := nodeB.Write(ctx); err != nil {
		t.Fatalf("promoted leader refused write: %v", err)
	}
	if nodeB.writes.Load() != 1 || nodeA.writes.Load() != 3 {
		t.Fatalf("dual-write detected: A=%d B=%d", nodeA.writes.Load(), nodeB.writes.Load())
	}
}

// TestChaosLeaseExpiryFencesOrphanedLeader — a partitioned node whose
// lease simply expires (no successor yet) must still refuse writes: an
// absent lease is not a mandate.
func TestChaosLeaseExpiryFencesOrphanedLeader(t *testing.T) {
	client, leases := chaosLeases(t)
	ctx := context.Background()
	const shard = 4302
	_ = client.Del(ctx, orchLeaderKey(shard))
	defer client.Del(context.Background(), orchLeaderKey(shard))

	clk := newFakeClock()
	eng := newTestEngine(clk, healthyData(), &fakeTelemetry{
		book: map[int]uint64{shard: 1}, wal: map[int]uint64{shard: 1},
	}, healthyFeeds())
	o := newTestOrch(t, clk, []int{shard}, eng, func(d *OrchDeps) {
		d.Leases = leases
		d.Now = time.Now
	})

	acq, err := leases.AcquireLeader(ctx, shard, "node-c", 2, 250*time.Millisecond)
	if err != nil || !acq {
		t.Fatalf("acquire: %v %v", acq, err)
	}
	nodeC := &fencedWriter{orch: o, shard: shard, epoch: 2}
	if err := nodeC.Write(ctx); err != nil {
		t.Fatalf("healthy write refused: %v", err)
	}
	// The partition outlasts the 250ms lease; nobody refreshes it.
	time.Sleep(400 * time.Millisecond)
	if _, ok, err := leases.LeaderValue(ctx, shard); err != nil || ok {
		t.Fatalf("expired lease still visible: ok=%v err=%v", ok, err)
	}
	if err := nodeC.Write(ctx); !errors.Is(err, errSelfTerminated) {
		t.Fatalf("orphaned leader wrote without a lease (writes=%d)", nodeC.writes.Load())
	}
	if nodeC.writes.Load() != 1 {
		t.Fatalf("phantom write after lease expiry: %d", nodeC.writes.Load())
	}
}

// TestChaosStandbyPromotionParity — scenario C. The promoted standby must
// validate book_seq == wal_tail in the pre-open audit BEFORE the ingress
// gate opens. A divergent promoted book must freeze, never admit.
func TestChaosStandbyPromotionParity(t *testing.T) {
	client, leases := chaosLeases(t)
	ctx := context.Background()
	const shard = 4303
	_ = client.Del(ctx, orchLeaderKey(shard))
	defer client.Del(context.Background(), orchLeaderKey(shard))

	// Dead leader's lease still occupies the key (epoch 3).
	acq, err := leases.AcquireLeader(ctx, shard, "node-a", 3, 30*time.Second)
	if err != nil || !acq {
		t.Fatalf("seed stale lease: %v %v", acq, err)
	}

	clk := newFakeClock()
	// Promoted book diverged from the WAL tail: replay stopped one short.
	tel := &fakeTelemetry{
		book: map[int]uint64{shard: 100},
		wal:  map[int]uint64{shard: 101},
	}
	eng := newTestEngine(clk, healthyData(), tel, healthyFeeds())

	var transMu sync.Mutex
	var transitions []OrchShardState
	recordTransition := func(_ int, _, to OrchShardState) {
		transMu.Lock()
		transitions = append(transitions, to)
		transMu.Unlock()
	}
	o := newTestOrch(t, clk, []int{shard}, eng, func(d *OrchDeps) {
		d.Leases = leases
		d.Standbys = fakeStandbys{token: "node-b"}
		d.Liveness = fakeLiveness{alive: map[string]bool{}}
		d.Now = time.Now // measure real failover latency
		d.OnStateChange = recordTransition
	})

	// --- Promotion under the 3s RTO, measured on the wall clock.
	start := time.Now()
	p, err := o.FenceAndPromote(ctx, shard)
	failoverElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("fence+promote: %v", err)
	}
	t.Logf("promotion epoch 3→%d in %s (wall) / %s (measured), RTO %s",
		p.Epoch, failoverElapsed, p.Elapsed, OrchPromotionRTO)
	if !p.Promoted || p.Epoch != 4 || p.Token != "node-b" {
		t.Fatalf("promotion record: %+v", p)
	}
	if failoverElapsed > OrchPromotionRTO || !p.WithinRTO {
		t.Fatalf("failover latency %s exceeded RTO %s", failoverElapsed, OrchPromotionRTO)
	}

	// --- Parity gate 1: lease acquisition alone does NOT open ingress.
	if st := o.State(shard); st == OrchStateNormal {
		t.Fatal("shard opened ingress without audit")
	}
	if err := o.AdmitOrder(shard, OrchOrderAdmission{}); err == nil {
		t.Fatal("order admitted on a freshly promoted, unaudited shard")
	} else {
		var e *excerrors.Error
		if !errors.As(err, &e) || e.Code != OrchCodeServiceDegraded {
			t.Fatalf("want SERVICE_DEGRADED pre-audit, got %v", err)
		}
	}

	// --- Parity gate 2: book_seq(100) != wal_tail(101) → reopen audit FAIL
	//     → HALT_LEGAL_FREEZE; ingress stays closed.
	v, err := o.ReopenShard(ctx, shard)
	if err != nil || v != OrchVerdictFreeze {
		t.Fatalf("divergent book must freeze: verdict=%s err=%v", v, err)
	}
	if o.State(shard) != OrchStateFrozen {
		t.Fatalf("state %s after failed parity audit", o.State(shard))
	}
	audit := o.Audit(shard)
	if audit == nil || !audit.Fail {
		t.Fatalf("failed audit not recorded: %+v", audit)
	}
	st := stageByID(audit, OrchStageBookWALSeq)
	if st.Verdict != OrchVerdictFail {
		t.Fatalf("book/wal stage verdict %s", st.Verdict)
	}
	if got := fmt.Sprint(st.Detail["book_seq"]); got != "100" {
		t.Fatalf("audit detail missing book_seq: %+v", st.Detail)
	}
	if err := o.AdmitOrder(shard, OrchOrderAdmission{IsCancel: true}); err == nil {
		t.Fatal("frozen shard admitted an order — parity check bypassed")
	}

	// --- Parity gate 3: once the WAL tail is replayed to parity
	//     (book==wal), the reopen ladder runs and ingress opens.
	tel.wal[shard] = 100
	v, err = o.ReopenShard(ctx, shard)
	if err != nil || v != OrchVerdictReopen {
		t.Fatalf("reopen after parity: verdict=%s err=%v", v, err)
	}
	if o.State(shard) != OrchStateNormal {
		t.Fatalf("state %s after clean reopen", o.State(shard))
	}
	if err := o.AdmitOrder(shard, OrchOrderAdmission{}); err != nil {
		t.Fatalf("order rejected on NORMAL shard: %v", err)
	}

	// Transition order proves ingress opened only after a passing audit.
	want := []OrchShardState{
		OrchStateAuditing, OrchStateFrozen,
		OrchStateAuditing, OrchStateCancelOnly, OrchStateAuction, OrchStateNormal,
	}
	transMu.Lock()
	got := append([]OrchShardState(nil), transitions...)
	transMu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("transitions %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transitions %v want %v", got, want)
		}
	}
}
