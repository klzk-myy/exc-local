package recovery

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	exredis "exchange/internal/redis"
)

func TestDigestChainVerification(t *testing.T) {
	h := strings.Repeat("aa", 32)
	mk := func(seq uint64) OrchDigest {
		return OrchDigest{CheckpointSeq: seq, JournalSeq: seq,
			GLZeroSumHash: h, BalanceDeltaHash: h, TradeCount: seq}
	}

	// Healthy chain.
	v := OrchVerifyDigestChain([]OrchDigest{mk(1000), mk(2000), mk(3000)})
	if !v.OK || v.CheckpointSeq != 3000 {
		t.Fatalf("healthy chain rejected: %+v", v)
	}
	// Empty chain is OK (cold start → full scan path).
	if v := OrchVerifyDigestChain(nil); !v.OK {
		t.Fatal("empty chain must verify OK")
	}
	// Gap > interval → broken.
	v = OrchVerifyDigestChain([]OrchDigest{mk(1000), mk(3000)})
	if v.OK || v.FirstDivergent != 3000 {
		t.Fatalf("gap chain accepted: %+v", v)
	}
	// Non-monotonic.
	v = OrchVerifyDigestChain([]OrchDigest{mk(2000), mk(1000)})
	if v.OK {
		t.Fatal("non-monotonic chain accepted")
	}
	// Malformed hash.
	bad := mk(1000)
	bad.GLZeroSumHash = "short"
	if v := OrchVerifyDigestChain([]OrchDigest{bad}); v.OK {
		t.Fatal("malformed hash accepted")
	}
}

func TestCheckpointerBoundariesAndImbalance(t *testing.T) {
	clk := newFakeClock()
	data := healthyData()
	store := NewOrchMemDigestStore()
	cp := &OrchCheckpointer{Store: store, Data: data, Now: clk.Now}

	if _, w, err := cp.MaybeCheckpoint(context.Background(), 0, 999); err != nil || w {
		t.Fatal("non-boundary checkpoint written")
	}
	d, w, err := cp.MaybeCheckpoint(context.Background(), 0, 1000)
	if err != nil || !w {
		t.Fatalf("boundary checkpoint: w=%v err=%v", w, err)
	}
	if d.GLZeroSumHash != OrchGLZeroSumHash([]OrchGLSum{
		{Currency: "USD", Debits: dec("100"), Credits: dec("100")},
	}) {
		t.Fatal("digest hash not canonical")
	}
	stored, found, err := store.LatestDigest(context.Background(), 0)
	if err != nil || !found || stored.CheckpointSeq != 1000 {
		t.Fatal("digest not stored")
	}

	// Imbalanced window → refuse to checkpoint (fail closed).
	data.glRows = append(data.glRows,
		orchGLRow{Journal: 12, Currency: "USD", Debit: dec("1"), Credit: dec("0")})
	if _, _, err := cp.MaybeCheckpoint(context.Background(), 0, 2000); err == nil {
		t.Fatal("imbalanced window was checkpointed")
	}
}

func TestOrchParseLeaderValue(t *testing.T) {
	tok, ep, err := OrchParseLeaderValue("node-1:42")
	if err != nil || tok != "node-1" || ep != 42 {
		t.Fatalf("parse: %q %d %v", tok, ep, err)
	}
	if _, _, err := OrchParseLeaderValue("noepoch"); err == nil {
		t.Fatal("malformed lease value parsed")
	}
}

// ---------------------------------------------------------------------------
// Redis lease adapter — runs only when REDIS_URL is set (real Redis at
// e.g. redis://127.0.0.1:6379). Skipped otherwise; in-memory fakes cover
// the contract.
// ---------------------------------------------------------------------------

func TestRedisLeaseBackendLive(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if raw == "" {
		t.Skip("REDIS_URL not set — in-memory fakes cover the contract")
	}
	addr := strings.TrimPrefix(strings.TrimPrefix(raw, "redis://"), "rediss://")
	if h, _, err := net.SplitHostPort(addr); err != nil || h == "" {
		addr = net.JoinHostPort("127.0.0.1", "6379")
	}
	client := exredis.New(addr, "", 0)
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Skipf("redis at %s unreachable: %v", addr, err)
	}

	leases, err := NewOrchRedisLeases(client)
	if err != nil {
		t.Fatal(err)
	}
	const shard = 4242 // dedicated test shard — avoids clashing with dev leaders
	defer func() {
		// Best-effort cleanup of the test lease.
		_, _ = leases.RevokeLeader(context.Background(), shard, "orch-test:7")
	}()

	acq, err := leases.AcquireLeader(ctx, shard, "orch-test", 7, 2*time.Second)
	if err != nil || !acq {
		t.Fatalf("acquire: %v %v", acq, err)
	}
	v, ok, err := leases.LeaderValue(ctx, shard)
	if err != nil || !ok || v != "orch-test:7" {
		t.Fatalf("leader value %q ok=%v err=%v", v, ok, err)
	}
	// Epoch verify via the parsed value.
	tok, ep, err := OrchParseLeaderValue(v)
	if err != nil || tok != "orch-test" || ep != 7 {
		t.Fatalf("parse: %v", err)
	}
	// Token-guarded revoke: wrong value must NOT delete.
	if n, err := leases.RevokeLeader(ctx, shard, "other:9"); err != nil || n {
		t.Fatal("foreign token revoked the lease")
	}
	if n, err := leases.RevokeLeader(ctx, shard, "orch-test:7"); err != nil || !n {
		t.Fatal("holder revoke failed")
	}
}
