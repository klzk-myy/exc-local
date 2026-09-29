// Task 18.3.12 tests — sequence-state CAS semantics, ownership fencing,
// Logon re-synchronization verdicts, failover-store layering and the
// ClOrdID in-flight dedup registry. All hermetic: memSeqStore mirrors
// the Redis Lua CAS/claim contract; Redis-backed coverage is gated
// behind EXC_REDIS_TEST=1 per repo convention (internal/redis tests).
package fix

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// redisTestEnabled gates the integration tests (EXC_REDIS_TEST=1, same
// convention as internal/redis).
func redisTestEnabled() bool { return os.Getenv("EXC_REDIS_TEST") == "1" }

// redisTestClient dials the dev coordination primary
// (127.0.0.1:16379; override with EXC_REDIS_TEST_ADDR).
func redisTestClient(t *testing.T) *goredis.Client {
	t.Helper()
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:     addr,
		Password: os.Getenv("EXC_REDIS_TEST_PASSWORD"),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Skipf("redis coordination instance unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// ---------------------------------------------------------------------------
// fakes — memSeqStore mirrors the Lua CAS + claim contract exactly:
// CAS fences on want.Owner when non-empty and writes only in/out/heard;
// Claim writes owner/owner_exp/epoch and never touches the counters.
// ---------------------------------------------------------------------------

type memSeqStore struct {
	mu   sync.Mutex
	rows map[string]*memSeqRow
}
type memSeqRow struct {
	in, out   int64
	heard     time.Time
	owner     string
	ownerExp  time.Time
	epoch     int64
	hasFields bool // distinguishes "no row" from "claimed, no counters"
}

func newMemSeqStore() *memSeqStore { return &memSeqStore{rows: map[string]*memSeqRow{}} }

func (m *memSeqStore) Load(_ context.Context, id string) (SeqState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || !r.hasFields {
		return SeqState{}, ErrSessionNotFound
	}
	return SeqState{SessionID: id, InSeqNum: r.in, OutSeqNum: r.out,
		LastHeard: r.heard, Owner: r.owner, Epoch: r.epoch}, nil
}

func (m *memSeqStore) CAS(_ context.Context, want, next SeqState) (SeqState, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[want.SessionID]
	if !ok {
		r = &memSeqRow{}
		m.rows[want.SessionID] = r
	}
	if r.in != want.InSeqNum || r.out != want.OutSeqNum ||
		(want.Owner != "" && r.owner != want.Owner) {
		return SeqState{SessionID: want.SessionID, InSeqNum: r.in, OutSeqNum: r.out,
			LastHeard: r.heard, Owner: r.owner, Epoch: r.epoch}, false, nil
	}
	r.in, r.out, r.heard, r.hasFields = next.InSeqNum, next.OutSeqNum, next.LastHeard, true
	return SeqState{SessionID: want.SessionID, InSeqNum: r.in, OutSeqNum: r.out,
		LastHeard: r.heard, Owner: r.owner, Epoch: r.epoch}, true, nil
}

func (m *memSeqStore) Claim(_ context.Context, id, owner string, lease time.Duration, now time.Time) (SeqState, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		r = &memSeqRow{}
		m.rows[id] = r
	}
	if r.owner != "" && r.owner != owner && now.Before(r.ownerExp) {
		return SeqState{SessionID: id, InSeqNum: r.in, OutSeqNum: r.out,
			Owner: r.owner, Epoch: r.epoch}, false, nil
	}
	r.owner = owner
	r.ownerExp = now.Add(lease)
	r.epoch++
	return SeqState{SessionID: id, InSeqNum: r.in, OutSeqNum: r.out,
		LastHeard: r.heard, Owner: r.owner, Epoch: r.epoch}, true, nil
}

// failStore forces the PG-fallback path through FailoverStore.
type failStore struct{ inner *memSeqStore }

func (f *failStore) Load(ctx context.Context, id string) (SeqState, error) {
	return SeqState{}, errors.New("redis down")
}
func (f *failStore) CAS(ctx context.Context, w, n SeqState) (SeqState, bool, error) {
	return SeqState{}, false, errors.New("redis down")
}
func (f *failStore) Claim(ctx context.Context, id, o string, l time.Duration, t time.Time) (SeqState, bool, error) {
	return SeqState{}, false, errors.New("redis down")
}

// memDedup mirrors RedisExecDedup's claim-or-get semantics.
type memDedup struct {
	mu   sync.Mutex
	recs map[string]DedupRecord
}

func newMemDedup() *memDedup { return &memDedup{recs: map[string]DedupRecord{}} }

func (d *memDedup) ClaimOrGet(_ context.Context, sid, clOrdID string, _ time.Duration) (DedupRecord, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := sid + "|" + clOrdID
	if rec, ok := d.recs[k]; ok {
		return rec, false, nil
	}
	rec := DedupRecord{ClOrdID: clOrdID, Pending: true, CreatedAt: time.Now().UnixNano()}
	d.recs[k] = rec
	return rec, true, nil
}

func (d *memDedup) Resolve(_ context.Context, sid, clOrdID string, rec DedupRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := sid + "|" + clOrdID
	if _, ok := d.recs[k]; !ok {
		return errors.New("claim expired")
	}
	rec.Pending = false
	d.recs[k] = rec
	return nil
}

func testNow() func() time.Time {
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		base = base.Add(time.Millisecond)
		return base
	}
}

// ---------------------------------------------------------------------------
// ownership fencing
// ---------------------------------------------------------------------------

func TestClaimSessionFencing(t *testing.T) {
	ctx := context.Background()
	store := newMemSeqStore()
	now := testNow()
	gwA := NewFailover(store, nil, FailoverConfig{GatewayID: "gw-a", LeaseTTL: 2 * time.Second}, now)
	gwB := NewFailover(store, nil, FailoverConfig{GatewayID: "gw-b", LeaseTTL: 2 * time.Second}, now)

	st, err := gwA.ClaimSession(ctx, "FIX.4.4:VENUE->CLIENT")
	if err != nil {
		t.Fatalf("gwA claim: %v", err)
	}
	if st.Epoch != 1 {
		t.Fatalf("first claim epoch = %d, want 1", st.Epoch)
	}
	if _, err := gwB.ClaimSession(ctx, "FIX.4.4:VENUE->CLIENT"); !errors.Is(err, ErrSessionOwnedElsewhere) {
		t.Fatalf("gwB claim while lease live: err=%v, want ErrSessionOwnedElsewhere", err)
	}
	// Lease expiry frees the session — B takes over with a bumped epoch.
	expired := now().Add(3 * time.Second)
	st, ok, err := store.Claim(ctx, "FIX.4.4:VENUE->CLIENT", "gw-b", 2*time.Second, expired)
	if err != nil || !ok {
		t.Fatalf("post-expiry claim: ok=%v err=%v", ok, err)
	}
	if st.Epoch != 2 {
		t.Fatalf("takeover epoch = %d, want 2", st.Epoch)
	}
	// The demoted owner can no longer CAS — owner fencing rejects it.
	cur, _ := store.Load(ctx, "FIX.4.4:VENUE->CLIENT")
	// seed counters so the CAS precondition on seqs is meaningful
	_, ok, err = store.CAS(ctx,
		SeqState{SessionID: "FIX.4.4:VENUE->CLIENT"},
		SeqState{SessionID: "FIX.4.4:VENUE->CLIENT", InSeqNum: 5, OutSeqNum: 9, LastHeard: expired})
	if err != nil || !ok {
		t.Fatalf("seed CAS: ok=%v err=%v", ok, err)
	}
	cur, _ = store.Load(ctx, "FIX.4.4:VENUE->CLIENT")
	cur.Owner = "gw-a" // stale owner fences the swap
	_, ok, err = store.CAS(ctx, cur, SeqState{SessionID: cur.SessionID, InSeqNum: 6, OutSeqNum: 9})
	if err != nil {
		t.Fatalf("fenced CAS: %v", err)
	}
	if ok {
		t.Fatal("demoted owner's CAS landed — fencing broken")
	}
}

// ---------------------------------------------------------------------------
// Logon re-synchronization
// ---------------------------------------------------------------------------

func TestResumeOnLogonFresh(t *testing.T) {
	ctx := context.Background()
	f := NewFailover(newMemSeqStore(), nil, FailoverConfig{GatewayID: "gw"}, testNow())
	plan, err := f.ResumeOnLogon(ctx, "FIX.4.4:VENUE->NEW", 1, false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if plan.Action != ResyncInSync || plan.ExpectedIn != 1 {
		t.Fatalf("fresh logon plan = %+v, want InSync expected=1", plan)
	}
}

func TestResumeOnLogonGapAhead(t *testing.T) {
	ctx := context.Background()
	store := newMemSeqStore()
	f := NewFailover(store, nil, FailoverConfig{GatewayID: "gw"}, testNow())
	sid := "FIX.4.4:VENUE->CLIENT"
	if _, err := f.ClaimSession(ctx, sid); err != nil {
		t.Fatal(err)
	}
	// Simulate the dead primary's last durable state: in=5, out=9.
	_, ok, err := store.CAS(ctx,
		SeqState{SessionID: sid},
		SeqState{SessionID: sid, InSeqNum: 5, OutSeqNum: 9, LastHeard: time.Now().UTC()})
	if err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	plan, err := f.ResumeOnLogon(ctx, sid, 8, false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if plan.Action != ResyncIssueResendRequest {
		t.Fatalf("action = %v, want ResyncIssueResendRequest", plan.Action)
	}
	if plan.BeginSeqNo != 5 || plan.EndSeqNo != 0 {
		t.Fatalf("resend window = [%d,%d], want [5,0]", plan.BeginSeqNo, plan.EndSeqNo)
	}
}

func TestResumeOnLogonLowSeq(t *testing.T) {
	ctx := context.Background()
	store := newMemSeqStore()
	f := NewFailover(store, nil, FailoverConfig{GatewayID: "gw"}, testNow())
	sid := "FIX.4.4:VENUE->CLIENT"
	if _, ok, err := store.CAS(ctx,
		SeqState{SessionID: sid},
		SeqState{SessionID: sid, InSeqNum: 10, OutSeqNum: 3}); err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	plan, err := f.ResumeOnLogon(ctx, sid, 7, false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if plan.Action != ResyncLowSeq {
		t.Fatalf("action = %v, want ResyncLowSeq (fail-closed)", plan.Action)
	}
	if plan.BeginSeqNo != 7 || plan.EndSeqNo != 9 {
		t.Fatalf("low-seq replay window = [%d,%d], want [7,9]", plan.BeginSeqNo, plan.EndSeqNo)
	}
}

func TestResumeOnLogonResetFlag(t *testing.T) {
	ctx := context.Background()
	store := newMemSeqStore()
	f := NewFailover(store, nil, FailoverConfig{GatewayID: "gw"}, testNow())
	sid := "FIX.4.4:VENUE->CLIENT"
	if _, ok, _ := store.CAS(ctx,
		SeqState{SessionID: sid},
		SeqState{SessionID: sid, InSeqNum: 42, OutSeqNum: 77}); !ok {
		t.Fatal("seed failed")
	}
	plan, err := f.ResumeOnLogon(ctx, sid, 1, true)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if plan.Action != ResyncReset || plan.ExpectedIn != 1 {
		t.Fatalf("reset plan = %+v", plan)
	}
	st, _ := store.Load(ctx, sid)
	if st.InSeqNum != 1 || st.OutSeqNum != 1 {
		t.Fatalf("counters after reset = %d/%d, want 1/1", st.InSeqNum, st.OutSeqNum)
	}
}

// ---------------------------------------------------------------------------
// CAS increments + mismatch propagation
// ---------------------------------------------------------------------------

func TestRecordInboundAdvancesAndMismatches(t *testing.T) {
	ctx := context.Background()
	f := NewFailover(newMemSeqStore(), nil, FailoverConfig{GatewayID: "gw"}, testNow())
	sid := "FIX.4.4:VENUE->CLIENT"
	if _, err := f.ResumeOnLogon(ctx, sid, 1, false); err != nil {
		t.Fatal(err)
	}
	st, err := f.RecordInbound(ctx, sid, 1)
	if err != nil {
		t.Fatalf("record inbound: %v", err)
	}
	if st.InSeqNum != 2 {
		t.Fatalf("in seq = %d, want 2", st.InSeqNum)
	}
	if st.LastHeard.IsZero() {
		t.Fatal("last_heard not stamped")
	}
	// seq 4 against expected 2 → mismatch error carrying stored state
	cur, err := f.RecordInbound(ctx, sid, 4)
	if !errors.Is(err, ErrInboundSeqMismatch) {
		t.Fatalf("mismatch err = %v, want ErrInboundSeqMismatch", err)
	}
	if cur.InSeqNum != 2 {
		t.Fatalf("mismatch post-image in = %d, want 2", cur.InSeqNum)
	}
	// The gap verdict is the caller's next step (gapfill.go).
	a := AssessInbound(4, cur.InSeqNum, false)
	if a.Action != InboundResendRequest || a.BeginSeqNo != 2 {
		t.Fatalf("assessment = %+v, want ResendRequest begin=2", a)
	}
}

func TestRecordOutboundAdvances(t *testing.T) {
	ctx := context.Background()
	f := NewFailover(newMemSeqStore(), nil, FailoverConfig{GatewayID: "gw"}, testNow())
	sid := "FIX.4.4:VENUE->CLIENT"
	if _, err := f.ResumeOnLogon(ctx, sid, 1, false); err != nil {
		t.Fatal(err)
	}
	st, err := f.RecordOutbound(ctx, sid, 1)
	if err != nil || st.OutSeqNum != 2 {
		t.Fatalf("outbound: st=%+v err=%v", st, err)
	}
}

// ---------------------------------------------------------------------------
// layered store fallback
// ---------------------------------------------------------------------------

func TestFailoverStoreFallsBack(t *testing.T) {
	ctx := context.Background()
	fallback := newMemSeqStore()
	sid := "FIX.4.4:VENUE->CLIENT"
	// Seed the "PG" side with durable state as the dead primary left it.
	if _, ok, err := fallback.CAS(ctx,
		SeqState{SessionID: sid},
		SeqState{SessionID: sid, InSeqNum: 11, OutSeqNum: 20,
			LastHeard: time.Now().UTC()}); err != nil || !ok {
		t.Fatalf("seed fallback: ok=%v err=%v", ok, err)
	}
	layered := NewFailoverStore(&failStore{}, fallback, nil)

	st, err := layered.Load(ctx, sid)
	if err != nil {
		t.Fatalf("fallback load: %v", err)
	}
	if st.InSeqNum != 11 || st.OutSeqNum != 20 {
		t.Fatalf("fallback state = %+v, want 11/20", st)
	}
	// CAS routes to the fallback on transport error.
	if _, ok, err := layered.CAS(ctx, st,
		SeqState{SessionID: sid, InSeqNum: 12, OutSeqNum: 20}); err != nil || !ok {
		t.Fatalf("fallback CAS: ok=%v err=%v", ok, err)
	}
	// Claim routes too.
	if _, ok, err := layered.Claim(ctx, sid, "gw-b", time.Second, time.Now()); err != nil || !ok {
		t.Fatalf("fallback claim: ok=%v err=%v", ok, err)
	}
	// And with no fallback the same failure is fail-closed.
	if _, err := NewFailoverStore(&failStore{}, nil, nil).Load(ctx, sid); err == nil {
		t.Fatal("expected error with no fallback")
	}
}

// ---------------------------------------------------------------------------
// ClOrdID dedup
// ---------------------------------------------------------------------------

func TestHandleNewOrderDedup(t *testing.T) {
	ctx := context.Background()
	f := NewFailover(newMemSeqStore(), newMemDedup(), FailoverConfig{GatewayID: "gw"}, testNow())
	sid := "FIX.4.4:VENUE->CLIENT"

	// First sight — claimed, caller submits to the engine.
	_, dup, err := f.HandleNewOrder(ctx, sid, "ORD-1", false)
	if err != nil || dup {
		t.Fatalf("first submit: dup=%v err=%v", dup, err)
	}
	// Engine ack binds the report.
	if err := f.ResolveDedup(ctx, sid, "ORD-1", DedupRecord{
		ClOrdID: "ORD-1", ExecID: "EX-9", OrderID: 42,
		Report: []byte("8=FIX.4.4|35=8|..."),
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Replayed NewOrderSingle (PossDupFlag=Y after failover) — the stored
	// report comes back; the engine must never see a second submit.
	rec, dup, err := f.HandleNewOrder(ctx, sid, "ORD-1", true)
	if err != nil || !dup {
		t.Fatalf("replay: dup=%v err=%v", dup, err)
	}
	if rec.ExecID != "EX-9" || rec.OrderID != 42 {
		t.Fatalf("dedup record = %+v", rec)
	}
	if len(rec.Report) == 0 {
		t.Fatal("original ExecutionReport not retained for echo")
	}
}

// ---------------------------------------------------------------------------
// gated Redis integration (EXC_REDIS_TEST=1, repo convention)
// ---------------------------------------------------------------------------

func TestRedisSeqStoreIntegration(t *testing.T) {
	if testing.Short() || !redisTestEnabled() {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	rdb := redisTestClient(t)
	ctx := context.Background()
	sid := "FIX.4.4:IT->" + t.Name()
	store := NewRedisSeqStore(rdb)
	defer func() { _ = rdb.Del(ctx, seqKey(sid)).Err() }()

	now := time.Now().UTC()
	st, ok, err := store.Claim(ctx, sid, "gw-it", 2*time.Second, now)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if st.Epoch < 1 {
		t.Fatalf("epoch = %d", st.Epoch)
	}
	got, ok, err := store.CAS(ctx,
		SeqState{SessionID: sid},
		SeqState{SessionID: sid, InSeqNum: 7, OutSeqNum: 3, LastHeard: now})
	if err != nil || !ok {
		t.Fatalf("cas: ok=%v err=%v", ok, err)
	}
	if got.InSeqNum != 7 || got.OutSeqNum != 3 {
		t.Fatalf("post-image = %+v", got)
	}
	// losing CAS
	_, ok, err = store.CAS(ctx,
		SeqState{SessionID: sid, InSeqNum: 1, OutSeqNum: 1},
		SeqState{SessionID: sid, InSeqNum: 2, OutSeqNum: 2})
	if err != nil || ok {
		t.Fatalf("losing cas: ok=%v err=%v", ok, err)
	}
	// owner fencing: foreign owner can't swap
	cur, _ := store.Load(ctx, sid)
	cur.Owner = "foreign"
	_, ok, err = store.CAS(ctx, cur, SeqState{SessionID: sid, InSeqNum: 8, OutSeqNum: 3})
	if err != nil || ok {
		t.Fatalf("fenced cas: ok=%v err=%v", ok, err)
	}
}
