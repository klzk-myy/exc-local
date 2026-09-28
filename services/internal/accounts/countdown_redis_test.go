// Integration test for RedisCountdownStore against dev Redis.
//
// Gated: skipped unless EXC_REDIS_TEST=1. Target defaults to the dev
// coordination primary at 127.0.0.1:16379 (docker-compose.dev.yml);
// override with EXC_REDIS_TEST_ADDR.
//
// Run: EXC_REDIS_TEST=1 go test ./internal/accounts/ -run Redis -v
package accounts

import (
	"context"
	"os"
	"testing"
	"time"

	excredis "exchange/internal/redis"
)

func testRedisStore(t *testing.T) (*RedisCountdownStore, func()) {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	c := excredis.New(addr, "", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		t.Skipf("redis coordination instance unreachable at %s: %v", addr, err)
	}
	store := NewRedisCountdownStore(c.Client)
	return store, func() {
		ctx := context.Background()
		_ = c.Del(ctx, countdownKey(777001), countdownKey(777002), countdownIndexKey).Err()
		_ = c.Close()
	}
}

func TestRedisCountdownArmDisarmPop(t *testing.T) {
	store, cleanup := testRedisStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Arm strict start.
	armed, err := store.Arm(ctx, 777001, now+60000, time.Minute, true)
	if err != nil || !armed {
		t.Fatalf("arm: %v armed=%v", err, armed)
	}
	// NX re-arm while live → armed=false (COUNTDOWN_ALREADY_ACTIVE path).
	armed, err = store.Arm(ctx, 777001, now+90000, time.Minute, true)
	if err != nil || armed {
		t.Fatalf("strict re-arm must fail: %v armed=%v", err, armed)
	}
	// Renewal (NX=false) succeeds and repoints the deadline.
	armed, err = store.Arm(ctx, 777001, now+120000, 2*time.Minute, false)
	if err != nil || !armed {
		t.Fatalf("renew: %v armed=%v", err, armed)
	}
	dl, ok, err := store.Deadline(ctx, 777001)
	if err != nil || !ok || dl != now+120000 {
		t.Fatalf("deadline=%d ok=%v err=%v", dl, ok, err)
	}
	// Disarm clears both key and index.
	if err := store.Disarm(ctx, 777001); err != nil {
		t.Fatalf("disarm: %v", err)
	}
	if _, ok, _ := store.Deadline(ctx, 777001); ok {
		t.Fatal("deadline still present after disarm")
	}
	exp, err := store.PopExpired(ctx, now+10*time.Hour.Milliseconds(), 16)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	for _, id := range exp {
		if id == 777001 {
			t.Fatal("disarmed timer popped as expired")
		}
	}
}

func TestRedisCountdownExpirySweepPath(t *testing.T) {
	store, cleanup := testRedisStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Arm a 1s timer; let it TTL-expire; PopExpired must report it.
	if _, err := store.Arm(ctx, 777002, now+1000, time.Second, false); err != nil {
		t.Fatalf("arm: %v", err)
	}
	exp, err := store.PopExpired(ctx, now+500, 16) // not yet due
	if err != nil {
		t.Fatalf("early pop: %v", err)
	}
	for _, id := range exp {
		if id == 777002 {
			t.Fatal("timer popped before due")
		}
	}
	time.Sleep(1200 * time.Millisecond) // TTL elapses
	exp, err = store.PopExpired(ctx, now+5000, 16)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	found := false
	for _, id := range exp {
		if id == 777002 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expired timer 777002 not in %v", exp)
	}
	// Consumed: second pop must not report it again.
	exp, err = store.PopExpired(ctx, now+6000, 16)
	if err != nil {
		t.Fatalf("second pop: %v", err)
	}
	for _, id := range exp {
		if id == 777002 {
			t.Fatal("expired timer popped twice")
		}
	}
}
