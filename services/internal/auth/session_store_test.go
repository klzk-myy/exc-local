// Integration tests for redisSessionStore — the Lua index/consume
// scripts and the session:{sid} hash contract — against a live
// coordination Redis.
//
// Gated: skipped unless EXC_REDIS_TEST=1. Target defaults to the dev
// coordination primary at 127.0.0.1:16379; override with
// EXC_REDIS_TEST_ADDR.
//
// Run: EXC_REDIS_TEST=1 go test ./internal/auth/ -run Redis -v
package auth

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	exchredis "exchange/internal/redis"
)

func testRedisStore(t *testing.T) *redisSessionStore {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	c := exchredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		t.Skipf("redis unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &redisSessionStore{c: c}
}

func redisCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRedisSessionHashRoundTrip(t *testing.T) {
	st := testRedisStore(t)
	ctx := redisCtx(t)
	sid := "itest-" + fmt.Sprint(time.Now().UnixNano())
	s := Session{
		ID: sid, UserID: "u-it", AccountID: 42, Tier: "PRO",
		Device: "test", IP: "192.0.2.9", AMR: []string{"pwd", "totp"},
		CreatedAt: time.Now(), LastActiveAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour), refreshHash: "refresh:x",
	}
	if err := st.WriteSession(ctx, s, time.Hour); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteSession(context.Background(), sid) })
	got, ok, err := st.ReadSession(ctx, sid)
	if err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	if got.UserID != "u-it" || got.AccountID != 42 || got.Tier != "PRO" ||
		got.Device != "test" || got.IP != "192.0.2.9" || len(got.AMR) != 2 ||
		got.refreshHash != "refresh:x" {
		t.Fatalf("hash round trip lost fields: %+v", got)
	}
	// Canonical §4.1 fields are present verbatim.
	raw, err := st.c.HGetAll(ctx, "session:"+sid).Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"user_id", "account_id", "tier"} {
		if raw[f] == "" {
			t.Fatalf("canonical field %s missing: %v", f, raw)
		}
	}
	if ttl := st.c.PTTL(ctx, "session:"+sid).Val(); ttl <= 0 {
		t.Fatalf("session hash must carry a TTL (§4.1 3600s), got %v", ttl)
	}
	if _, ok, err := st.ReadSession(ctx, sid+"-nope"); err != nil || ok {
		t.Fatalf("missing sid must be (zero,false,nil): %v %v", ok, err)
	}
}

func TestRedisIndexAddEvictsOldestAtomically(t *testing.T) {
	st := testRedisStore(t)
	ctx := redisCtx(t)
	idx := "sess:acct:itest-" + fmt.Sprint(time.Now().UnixNano())
	defer func() { _ = st.c.Del(context.Background(), idx) }()
	for i := 1; i <= 3; i++ {
		evicted, err := st.IndexAdd(ctx, idx, fmt.Sprint("s", i), float64(i), 3, 0)
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
		if len(evicted) != 0 {
			t.Fatalf("no eviction below cap, got %v", evicted)
		}
	}
	evicted, err := st.IndexAdd(ctx, idx, "s4", 4, 3, 0)
	if err != nil {
		t.Fatalf("add 4: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "s1" {
		t.Fatalf("oldest-first eviction expected s1, got %v", evicted)
	}
	// Prune: members below cutoff drop on next add.
	_, err = st.IndexAdd(ctx, idx, "s5", 100, 3, 3.5) // cutoff between s2(2)/s3(3) and s4(4)/s5(100)
	if err != nil {
		t.Fatal(err)
	}
	members, err := st.IndexMembers(ctx, idx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// s2,s3 pruned by cutoff → only s4,s5 remain (under cap, no eviction).
	for _, m := range members {
		if m == "s2" || m == "s3" {
			t.Fatalf("stale member %s survived cutoff prune: %v", m, members)
		}
	}
	if len(members) != 2 {
		t.Fatalf("index members=%v, want [s4 s5]", members)
	}
}

func TestRedisRefreshConsumeAtomicity(t *testing.T) {
	st := testRedisStore(t)
	ctx := redisCtx(t)
	h := fmt.Sprint(time.Now().UnixNano())
	active := "refresh:" + h
	used := "refresh_used:" + h
	defer func() {
		_ = st.c.Del(context.Background(), active, used)
	}()
	// Missing → RefreshMissing.
	_, res, err := st.ConsumeRefresh(ctx, active, used, time.Hour)
	if err != nil || res != RefreshMissing {
		t.Fatalf("missing: %v %v", res, err)
	}
	if err := st.SetRefresh(ctx, active, "sid-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	// First consume → OK + marks used.
	sid, res, err := st.ConsumeRefresh(ctx, active, used, time.Hour)
	if err != nil || res != RefreshOK || sid != "sid-1" {
		t.Fatalf("consume: %q %v %v", sid, res, err)
	}
	// Second consume of the same token → REUSED (theft signal).
	sid, res, err = st.ConsumeRefresh(ctx, active, used, time.Hour)
	if err != nil || res != RefreshReused || sid != "sid-1" {
		t.Fatalf("reuse: %q %v %v", sid, res, err)
	}
}
