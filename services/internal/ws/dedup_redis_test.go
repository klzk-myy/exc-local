// Task 5.3.42 — RedisDedupStore integration test.
// Gated: EXC_REDIS_TEST=1, EXC_REDIS_TEST_ADDR / EXC_REDIS_TEST_PASSWORD.
package ws

import (
	"context"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func redisTestClient(t *testing.T) *goredis.Client {
	t.Helper()
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

func TestRedisDedupStoreLifecycle(t *testing.T) {
	rdb := redisTestClient(t)
	ctx := context.Background()
	s := NewRedisDedupStore(rdb)
	key := DedupKey("a:999", "itest-"+int64str(time.Now().UnixNano()))
	ph := PayloadHash("order.place", []byte(`{"x":1}`))
	defer func() { _ = rdb.Del(ctx, key) }()

	res, err := s.Begin(ctx, key, ph, time.Minute)
	if err != nil || res.State != DedupProceed {
		t.Fatalf("begin: %+v %v", res, err)
	}
	res, err = s.Begin(ctx, key, ph, time.Minute)
	if err != nil || res.State != DedupInFlight {
		t.Fatalf("in-flight: %+v %v", res, err)
	}
	res, err = s.Begin(ctx, key, "other-hash", time.Minute)
	if err != nil || res.State != DedupMismatch {
		t.Fatalf("mismatch: %+v %v", res, err)
	}
	if err := s.Complete(ctx, key, ph, []byte(`{"type":"response"}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	res, err = s.Begin(ctx, key, ph, time.Minute)
	if err != nil || res.State != DedupReplay ||
		string(res.Replay) != `{"type":"response"}` {
		t.Fatalf("replay: %+v %v", res, err)
	}

	// nil-frame complete releases the claim.
	key2 := DedupKey("a:999", "itest2-"+int64str(time.Now().UnixNano()))
	defer func() { _ = rdb.Del(ctx, key2) }()
	if _, err := s.Begin(ctx, key2, ph, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, key2, ph, nil); err != nil {
		t.Fatalf("release: %v", err)
	}
	res, err = s.Begin(ctx, key2, ph, time.Minute)
	if err != nil || res.State != DedupProceed {
		t.Fatalf("re-begin after release: %+v %v", res, err)
	}
}
