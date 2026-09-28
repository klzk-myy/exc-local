// Task 5.3.42 — RedisIdemStore integration test.
// Gated: EXC_REDIS_TEST=1, EXC_REDIS_TEST_ADDR / EXC_REDIS_TEST_PASSWORD.
package middleware

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

func TestRedisIdemStoreLifecycle(t *testing.T) {
	rdb := redisTestClient(t)
	ctx := context.Background()
	s := NewRedisIdemStore(rdb)
	key := IdemKey(999001, "018f8b7e-1001-7b2c-9d1e-2f3a4b5c6d7e")
	defer func() { _ = rdb.Del(ctx, key) }()

	res, err := s.Begin(ctx, key, "POST /api/v1/withdrawals", "ph-a", time.Minute)
	if err != nil || res.State != IdemProceed {
		t.Fatalf("begin: %+v %v", res, err)
	}
	res, err = s.Begin(ctx, key, "POST /api/v1/withdrawals", "ph-a", time.Minute)
	if err != nil || res.State != IdemInFlight {
		t.Fatalf("in-flight: %+v %v", res, err)
	}
	res, err = s.Begin(ctx, key, "POST /api/v1/withdrawals", "ph-b", time.Minute)
	if err != nil || res.State != IdemMismatch {
		t.Fatalf("mismatch: %+v %v", res, err)
	}
	if err := s.Complete(ctx, key, "ph-a", 201, "application/json",
		[]byte(`{"withdrawal_id":9}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	res, err = s.Begin(ctx, key, "POST /api/v1/withdrawals", "ph-a", time.Minute)
	if err != nil || res.State != IdemReplay {
		t.Fatalf("replay: %+v %v", res, err)
	}
	if res.HTTPStatus != 201 || string(res.Body) != `{"withdrawal_id":9}` ||
		res.ContentType != "application/json" {
		t.Fatalf("replay payload: %+v", res)
	}

	// status=0 releases the pending claim.
	key2 := IdemKey(999001, "018f8b7e-1002-7b2c-9d1e-2f3a4b5c6d7e")
	defer func() { _ = rdb.Del(ctx, key2) }()
	if _, err := s.Begin(ctx, key2, "POST /x", "ph-c", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, key2, "ph-c", 0, "", nil); err != nil {
		t.Fatalf("release: %v", err)
	}
	res, err = s.Begin(ctx, key2, "POST /x", "ph-c", time.Minute)
	if err != nil || res.State != IdemProceed {
		t.Fatalf("re-begin after release: %+v %v", res, err)
	}
}
