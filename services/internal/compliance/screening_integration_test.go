// Phase-21 Tasks 21.3.10/21.3.23 — Redis-backed integration tests for
// the screening flag publisher (C++ SanctionsCache feed) and the
// pending-screen queue. Gated like internal/redis tests:
//
//	EXC_REDIS_TEST=1            enable
//	EXC_REDIS_TEST_ADDR         redis addr (default 127.0.0.1:6379)
//	EXC_REDIS_TEST_PASSWORD     redis password (default redpass)
package compliance

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	excredis "exchange/internal/redis"
)

func screeningRedis(t *testing.T) (*excredis.Client, context.Context) {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	pass := os.Getenv("EXC_REDIS_TEST_PASSWORD")
	if pass == "" {
		pass = "redpass"
	}
	ctx := context.Background()
	rdb := excredis.New(addr, pass, 0)
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis coordination instance unreachable at %s: %v", addr, err)
	}
	return rdb, ctx
}

func TestFlagPublisherRoundTrip(t *testing.T) {
	rdb, ctx := screeningRedis(t)
	acct := int64(424242)
	p := NewFlagPublisher(rdb)
	defer rdb.Del(ctx, flagKey(acct), clearedKey(acct)).Err()
	defer rdb.SRem(ctx, SanctionsFlagSet, strconv.FormatInt(acct, 10))
	defer rdb.SetBit(ctx, SanctionsScreenedKey, acct, 0)
	// Flag → key + inventory set, cleared marker removed.
	if err := p.FlagAccount(ctx, acct, "test"); err != nil {
		t.Fatalf("flag: %v", err)
	}
	if v, err := rdb.Get(ctx, flagKey(acct)).Result(); err != nil || v != "1" {
		t.Fatalf("flag key: %q err=%v", v, err)
	}
	if ok, _ := rdb.SIsMember(ctx, SanctionsFlagSet,
		strconv.FormatInt(acct, 10)).Result(); !ok {
		t.Fatal("flagged inventory missing account")
	}
	// MarkScreened flips the bloom bit.
	if err := p.MarkScreened(ctx, acct); err != nil {
		t.Fatalf("mark screened: %v", err)
	}
	if b, _ := rdb.GetBit(ctx, SanctionsScreenedKey, acct).Result(); b != 1 {
		t.Fatal("screened bit not set")
	}
	// ClearFlag → flag key gone, cleared marker set, inventory removed.
	if err := p.ClearFlag(ctx, acct); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := rdb.Get(ctx, flagKey(acct)).Result(); err == nil {
		t.Fatal("flag key still present after clear")
	}
	if v, err := rdb.Get(ctx, clearedKey(acct)).Result(); err != nil || v != "1" {
		t.Fatalf("cleared marker: %q err=%v", v, err)
	}
	// Heartbeat publishes a fresh unix timestamp.
	if err := p.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	v, err := rdb.Get(ctx, SanctionsHeartbeatKey).Result()
	if err != nil {
		t.Fatalf("heartbeat key: %v", err)
	}
	if ts, _ := strconv.ParseInt(v, 10, 64); ts <= 0 ||
		time.Now().Unix()-ts > 60 {
		t.Fatalf("stale heartbeat value %q", v)
	}
}

func TestRedisScreenQueueLifecycle(t *testing.T) {
	rdb, ctx := screeningRedis(t)
	q := NewRedisScreenQueue(rdb)
	// Clean slate for the test (dev instance — flush only our keys).
	rdb.Del(ctx, keyPending, keyInflight, keyQueueSeq)
	s := PendingScreen{AccountID: 9001, Flow: ScreenFlowWithdrawal,
		Candidates: []string{"Fixture Name"}}
	id, err := q.Enqueue(ctx, s)
	if err != nil || id == 0 {
		t.Fatalf("enqueue: id=%d err=%v", id, err)
	}
	// Dedupe — same (flow, account) collapses.
	if id2, err := q.Enqueue(ctx, s); err != nil || id2 != 0 {
		t.Fatalf("dedupe: id2=%d err=%v", id2, err)
	}
	if d, _ := q.Depth(ctx); d != 1 {
		t.Fatalf("depth: %d", d)
	}
	// Claim → inflight; RecoverInflight returns it; claim again + Ack.
	items, err := q.Claim(ctx, 10)
	if err != nil || len(items) != 1 || items[0].AccountID != 9001 {
		t.Fatalf("claim: %+v err=%v", items, err)
	}
	if n, _ := q.RecoverInflight(ctx); n != 1 {
		t.Fatalf("recover: %d", n)
	}
	items, _ = q.Claim(ctx, 10)
	if len(items) != 1 {
		t.Fatalf("reclaim: %+v", items)
	}
	if err := q.Ack(ctx, items...); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if d, _ := q.Depth(ctx); d != 0 {
		t.Fatalf("post-ack depth: %d", d)
	}
}
