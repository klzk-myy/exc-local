// Task 6.3.2 — md:seq cursor store tests.
package marketdata

import (
	"context"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func TestMemSeqStore(t *testing.T) {
	s := NewMemSeqStore()
	ctx := context.Background()
	if v, err := s.Load(ctx, "EUR/USD"); err != nil || v != 0 {
		t.Fatalf("empty load = %d, %v", v, err)
	}
	if err := s.Store(ctx, "EUR/USD", 41); err != nil {
		t.Fatalf("store: %v", err)
	}
	if v, _ := s.Load(ctx, "EUR/USD"); v != 41 {
		t.Fatalf("load = %d, want 41", v)
	}
	// High-water-mark store: stale writes are ignored, not errors.
	if err := s.Store(ctx, "EUR/USD", 40); err != nil {
		t.Fatalf("stale store errored: %v", err)
	}
	if v, _ := s.Load(ctx, "EUR/USD"); v != 41 {
		t.Fatalf("cursor moved on stale write: %d", v)
	}
}

// TestRedisSeqStore is the Redis integration test. Gated per sibling
// convention: run with
//
//	EXC_REDIS_TEST=1 go test ./internal/marketdata/ -run Redis -v
//
// Optional overrides: EXC_REDIS_TEST_ADDR (default 127.0.0.1:6379),
// EXC_REDIS_TEST_PASSWORD.
func TestRedisSeqStore(t *testing.T) {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST != 1")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:     addr,
		Password: os.Getenv("EXC_REDIS_TEST_PASSWORD"),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	sym := "TEST/SEQ-" + time.Now().Format("150405.000000")
	key := SeqKey(sym)
	if key != "md:seq:"+sym {
		t.Fatalf("SeqKey = %q", key)
	}
	t.Cleanup(func() { rdb.Del(context.Background(), key) })

	s := NewRedisSeqStore(rdb)
	if v, err := s.Load(ctx, sym); err != nil || v != 0 {
		t.Fatalf("cold load = %d, %v", v, err)
	}
	if err := s.Store(ctx, sym, 1234); err != nil {
		t.Fatalf("store: %v", err)
	}
	if v, err := s.Load(ctx, sym); err != nil || v != 1234 {
		t.Fatalf("load = %d, %v — want 1234", v, err)
	}
	// Restart-simulation: a fresh store instance must see the cursor
	// (Task 6.3.22: seq never resets to 0 on restart).
	s2 := NewRedisSeqStore(rdb)
	if v, err := s2.Load(ctx, sym); err != nil || v != 1234 {
		t.Fatalf("post-restart load = %d, %v", v, err)
	}
}
