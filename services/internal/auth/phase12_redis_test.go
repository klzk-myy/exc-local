// Redis-gated tests for Phase-12: WebAuthn challenge single-use consume
// and the Task 12.3.12 brute-force lockout Lua path.
//
// Gated: skipped unless EXC_REDIS_TEST=1. Target defaults to the dev
// coordination Redis at 127.0.0.1:16379 (EXC_REDIS_TEST_ADDR overrides).
package auth

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	exchredis "exchange/internal/redis"
)

func testRedisClient(t *testing.T) *exchredis.Client {
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
	return c
}

func TestRedisWebAuthnChallengeSingleUse(t *testing.T) {
	c := testRedisClient(t)
	ctx := redisCtx(t)
	st := NewRedisWebAuthnChallengeStore(c)
	id := "itest-" + fmt.Sprint(time.Now().UnixNano())
	key := webAuthnChallengePrefix + id
	defer func() { _ = c.Del(context.Background(), key) }()

	if err := st.PutChallenge(ctx, id, []byte(`{"k":"v"}`), time.Minute); err != nil {
		t.Fatalf("put: %v", err)
	}
	p, ok, err := st.ConsumeChallenge(ctx, id)
	if err != nil || !ok || string(p) != `{"k":"v"}` {
		t.Fatalf("first consume: %v %v %q", ok, err, p)
	}
	// GETDEL semantics: a second consume (replay) is a hard miss.
	if _, ok, err := st.ConsumeChallenge(ctx, id); err != nil || ok {
		t.Fatalf("replay consume must miss: ok=%v err=%v", ok, err)
	}
	// TTL is armed (~60s callers; prove expiry exists).
	if ttl := c.PTTL(ctx, key).Val(); ttl > 0 {
		t.Fatalf("consumed key must be deleted, ttl=%v", ttl)
	}
	if err := st.PutChallenge(ctx, id+"ttl", []byte("x"), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Del(context.Background(), key+"ttl") }()
	if ttl := c.PTTL(ctx, key+"ttl").Val(); ttl <= 0 || ttl > 91*time.Second {
		t.Fatalf("challenge key must carry its TTL, got %v", ttl)
	}
}

func TestRedisLockoutThresholdAndLock(t *testing.T) {
	c := testRedisClient(t)
	ctx := redisCtx(t)
	svc := NewLockoutService(c)
	uid := "itest-" + fmt.Sprint(time.Now().UnixNano())
	defer func() {
		_ = c.Del(context.Background(), authFailuresKey(uid), authLockKey(uid))
	}()

	if locked, _, err := svc.CheckLock(ctx, uid); err != nil || locked {
		t.Fatalf("fresh id must be unlocked: %v %v", locked, err)
	}
	for i := 1; i <= 4; i++ {
		n, locked, _, err := svc.RecordFailure(ctx, uid)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if locked {
			t.Fatalf("failure %d must not lock", i)
		}
		if n != i {
			t.Fatalf("counter=%d want %d", n, i)
		}
	}
	// Fifth consecutive failure arms the 15-minute lock.
	n, locked, retry, err := svc.RecordFailure(ctx, uid)
	if err != nil {
		t.Fatalf("fifth: %v", err)
	}
	if !locked || n < 5 {
		t.Fatalf("5th failure must lock: n=%d locked=%v", n, locked)
	}
	if retry < 14*time.Minute || retry > 15*time.Minute {
		t.Fatalf("lock ttl ~15min, got %v", retry)
	}
	if locked, retry, err := svc.CheckLock(ctx, uid); err != nil || !locked || retry <= 0 {
		t.Fatalf("CheckLock must report the armed lock: %v %v", locked, retry)
	}
	// A further failure while locked reports locked without growing the
	// counter (counter was reset at lock time).
	_, locked, _, err = svc.RecordFailure(ctx, uid)
	if err != nil || !locked {
		t.Fatalf("locked attempt: %v %v", locked, err)
	}
	// ClearFailures clears the counter only — the lock survives.
	if err := svc.ClearFailures(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if locked, _, err := svc.CheckLock(ctx, uid); err != nil || !locked {
		t.Fatalf("lock must outlive ClearFailures: %v %v", locked, err)
	}
	if v := c.Exists(ctx, authFailuresKey(uid)).Val(); v != 0 {
		t.Fatal("failure counter must be deleted by ClearFailures")
	}
}
