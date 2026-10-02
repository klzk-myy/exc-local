// Integration tests for the spec §4 key-schema helpers.
//
// Gated: skipped unless EXC_REDIS_TEST=1. Target instance defaults to the
// dev coordination primary at 127.0.0.1:16379 (docker-compose.dev.yml);
// override with EXC_REDIS_TEST_ADDR.
//
// Run: EXC_REDIS_TEST=1 go test ./internal/redis/ -v
package redis

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	c := New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		t.Skipf("redis coordination instance unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// uniq returns a test-namespaced suffix so parallel/leftover keys never
// collide with real data.
func uniq(t *testing.T) string {
	return fmt.Sprintf("t%d", time.Now().UnixNano())
}

func TestPing(t *testing.T) {
	c := testClient(t)
	if err := c.Ping(testCtx(t)); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestSessionRoundTripAndExpiry(t *testing.T) {
	c := testClient(t)
	ctx := testCtx(t)
	token := uniq(t)
	t.Cleanup(func() { c.Del(context.Background(), sessionKey(token)) })

	want := Session{UserID: "u-1", AccountID: "a-9", Tier: "T2"}
	if err := c.SetSession(ctx, token, want); err != nil {
		t.Fatalf("SetSession: %v", err)
	}

	got, err := c.GetSession(ctx, token)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("session mismatch: got %+v want %+v", got, want)
	}

	// Canonical TTL check: should be ~3600s.
	ttl, err := c.PTTL(ctx, sessionKey(token)).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl <= 0 || ttl > SessionTTL {
		t.Fatalf("session TTL %v out of range (0, %v]", ttl, SessionTTL)
	}

	// Expiry check: write a 1s session, wait, expect nil.
	if err := c.setSessionTTL(ctx, token, want, time.Second); err != nil {
		t.Fatalf("setSessionTTL: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	got, err = c.GetSession(ctx, token)
	if err != nil {
		t.Fatalf("GetSession after expiry: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil session after expiry, got %+v", got)
	}
}

func TestIncrRateLimitAtomic(t *testing.T) {
	c := testClient(t)
	ctx := testCtx(t)
	ip, sec := "203.0.113."+uniq(t), time.Now().Unix()
	key := rateLimitKey(ip, sec)
	t.Cleanup(func() { c.Del(context.Background(), key) })

	n, err := c.IncrRateLimit(ctx, ip, sec)
	if err != nil {
		t.Fatalf("IncrRateLimit #1: %v", err)
	}
	if n != 1 {
		t.Fatalf("first increment = %d, want 1", n)
	}

	// TTL must be armed atomically with the INCR (~2000ms).
	pttl, err := c.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if pttl <= 0 || pttl > RateLimitTTL {
		t.Fatalf("rate-limit TTL %v out of range (0, %v]", pttl, RateLimitTTL)
	}

	n, err = c.IncrRateLimit(ctx, ip, sec)
	if err != nil {
		t.Fatalf("IncrRateLimit #2: %v", err)
	}
	if n != 2 {
		t.Fatalf("second increment = %d, want 2", n)
	}
}

func TestLeaderElection(t *testing.T) {
	c := testClient(t)
	ctx := testCtx(t)
	shard := 9000 + int(time.Now().UnixNano()%500) // avoid real shard ids
	key := leaderKey(shard)
	t.Cleanup(func() { c.Del(context.Background(), key) })

	// First caller acquires.
	ok, err := c.TryAcquireLeader(ctx, shard, "node-a", 1, 0)
	if err != nil || !ok {
		t.Fatalf("TryAcquireLeader node-a: ok=%v err=%v", ok, err)
	}

	// Stored value is "{token}:{epoch}", TTL ~2000ms (canonical §18.6.2).
	val, err := c.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("GET leader: %v", err)
	}
	if val != "node-a:1" {
		t.Fatalf("leader value %q, want %q", val, "node-a:1")
	}
	pttl, _ := c.PTTL(ctx, key).Result()
	if pttl <= 0 || pttl > LeaderLeaseTTL {
		t.Fatalf("leader TTL %v out of range (0, %v]", pttl, LeaderLeaseTTL)
	}

	// Second caller is rejected while the lease is held.
	ok, err = c.TryAcquireLeader(ctx, shard, "node-b", 2, 0)
	if err != nil || ok {
		t.Fatalf("TryAcquireLeader node-b: ok=%v err=%v (want false, nil)", ok, err)
	}

	// Wrong token cannot release.
	ok, err = c.ReleaseLeader(ctx, shard, "node-b", 2)
	if err != nil || ok {
		t.Fatalf("ReleaseLeader wrong token: ok=%v err=%v (want false, nil)", ok, err)
	}

	// Refresh by holder succeeds, by non-holder fails.
	ok, err = c.RefreshLeader(ctx, shard, "node-a", 1, 0)
	if err != nil || !ok {
		t.Fatalf("RefreshLeader holder: ok=%v err=%v", ok, err)
	}
	ok, err = c.RefreshLeader(ctx, shard, "node-b", 2, 0)
	if err != nil || ok {
		t.Fatalf("RefreshLeader non-holder: ok=%v err=%v (want false, nil)", ok, err)
	}

	// Correct token+epoch releases; then a new epoch can acquire.
	ok, err = c.ReleaseLeader(ctx, shard, "node-a", 1)
	if err != nil || !ok {
		t.Fatalf("ReleaseLeader holder: ok=%v err=%v", ok, err)
	}
	ok, err = c.TryAcquireLeader(ctx, shard, "node-b", 2, 0)
	if err != nil || !ok {
		t.Fatalf("TryAcquireLeader after release: ok=%v err=%v", ok, err)
	}
}

func TestAccountLock(t *testing.T) {
	c := testClient(t)
	ctx := testCtx(t)
	acct := "acct-" + uniq(t)
	key := accountLockKey(acct)
	t.Cleanup(func() { c.Del(context.Background(), key) })

	ok, err := c.TryLockAccount(ctx, acct, "tx-1", 0)
	if err != nil || !ok {
		t.Fatalf("TryLockAccount: ok=%v err=%v", ok, err)
	}

	// Contended lock rejected; canonical 10s TTL armed.
	ok, err = c.TryLockAccount(ctx, acct, "tx-2", 0)
	if err != nil || ok {
		t.Fatalf("contended TryLockAccount: ok=%v err=%v (want false, nil)", ok, err)
	}
	pttl, _ := c.PTTL(ctx, key).Result()
	if pttl <= 0 || pttl > AccountLockTTL {
		t.Fatalf("account lock TTL %v out of range (0, %v]", pttl, AccountLockTTL)
	}

	// Token-checked release: wrong token fails, right token frees.
	ok, err = c.UnlockAccount(ctx, acct, "tx-2")
	if err != nil || ok {
		t.Fatalf("UnlockAccount wrong token: ok=%v err=%v", ok, err)
	}
	ok, err = c.UnlockAccount(ctx, acct, "tx-1")
	if err != nil || !ok {
		t.Fatalf("UnlockAccount: ok=%v err=%v", ok, err)
	}
}

func TestDegradationMode(t *testing.T) {
	c := testClient(t)
	ctx := testCtx(t)
	for _, k := range []string{keyDegradationMode, keyDegradationEnteredAt, keyDegradationReason} {
		c.Del(ctx, k)
		t.Cleanup(func() { c.Del(context.Background(), k) })
	}

	// Absent key ⇒ Normal (spec §2.4 default).
	st, err := c.GetDegradationMode(ctx)
	if err != nil {
		t.Fatalf("GetDegradationMode: %v", err)
	}
	if st.Mode != ModeNormal {
		t.Fatalf("default mode = %q, want Normal", st.Mode)
	}

	if err := c.SetDegradationMode(ctx, ModeReadOnly, "core slow p50"); err != nil {
		t.Fatalf("SetDegradationMode: %v", err)
	}
	st, err = c.GetDegradationMode(ctx)
	if err != nil {
		t.Fatalf("GetDegradationMode after set: %v", err)
	}
	if st.Mode != ModeReadOnly || st.Reason != "core slow p50" || st.EnteredAt <= 0 {
		t.Fatalf("degradation state mismatch: %+v", st)
	}

	// Invalid mode fails closed — rejected before write.
	if err := c.SetDegradationMode(ctx, "readonly", "bad case"); err == nil {
		t.Fatal("SetDegradationMode accepted invalid mode")
	}
	if err := c.SetDegradationMode(ctx, "Bogus", ""); err == nil {
		t.Fatal("SetDegradationMode accepted bogus mode")
	}
}

func TestCircuitBreaker(t *testing.T) {
	c := testClient(t)
	ctx := testCtx(t)
	scope, id := "symbol", "EURUSD-"+uniq(t)
	key := circuitBreakerKey(scope, id)
	t.Cleanup(func() { c.Del(context.Background(), key) })

	// Absent ⇒ nil (no breaker record).
	cb, err := c.GetCircuitBreaker(ctx, scope, id)
	if err != nil || cb != nil {
		t.Fatalf("GetCircuitBreaker absent: cb=%+v err=%v", cb, err)
	}

	want := CircuitBreaker{
		State:    CircuitOpen,
		Metadata: map[string]string{"tripped_at": "1790500000000", "reason": "spread blowout"},
	}
	if err := c.SetCircuitBreaker(ctx, scope, id, want); err != nil {
		t.Fatalf("SetCircuitBreaker: %v", err)
	}
	cb, err = c.GetCircuitBreaker(ctx, scope, id)
	if err != nil {
		t.Fatalf("GetCircuitBreaker: %v", err)
	}
	if cb == nil || cb.State != want.State || cb.Metadata["reason"] != "spread blowout" {
		t.Fatalf("circuit breaker mismatch: %+v", cb)
	}

	// Replace semantics: stale metadata fields are dropped.
	if err := c.SetCircuitBreaker(ctx, scope, id, CircuitBreaker{State: CircuitHalfOpen}); err != nil {
		t.Fatalf("SetCircuitBreaker half-open: %v", err)
	}
	cb, _ = c.GetCircuitBreaker(ctx, scope, id)
	if cb.State != CircuitHalfOpen || len(cb.Metadata) != 0 {
		t.Fatalf("circuit breaker replace failed: %+v", cb)
	}

	// Invalid state rejected.
	if err := c.SetCircuitBreaker(ctx, scope, id, CircuitBreaker{State: "WIDE_OPEN"}); err == nil {
		t.Fatal("SetCircuitBreaker accepted invalid state")
	}
}

func TestGlobalHalt(t *testing.T) {
	c := testClient(t)
	ctx := testCtx(t)
	c.Del(ctx, keyHaltGlobal)
	t.Cleanup(func() { c.Del(context.Background(), keyHaltGlobal) })

	halted, err := c.IsHalted(ctx)
	if err != nil || halted {
		t.Fatalf("IsHalted initially: %v err=%v", halted, err)
	}
	if err := c.HaltGlobal(ctx, "L0 watchdog trip"); err != nil {
		t.Fatalf("HaltGlobal: %v", err)
	}
	halted, err = c.IsHalted(ctx)
	if err != nil || !halted {
		t.Fatalf("IsHalted after halt: %v err=%v", halted, err)
	}
	if err := c.ClearHalt(ctx); err != nil {
		t.Fatalf("ClearHalt: %v", err)
	}
	halted, _ = c.IsHalted(ctx)
	if halted {
		t.Fatal("IsHalted still true after ClearHalt")
	}
}
