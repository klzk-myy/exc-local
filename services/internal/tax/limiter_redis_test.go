// Task 20.3.10 — Redis-backed daily report cap. Gated:
// EXC_REDIS_TEST=1 + optional EXC_REDIS_TEST_ADDR (default
// 127.0.0.1:16379).
package tax

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func limiterRedisClient(t *testing.T) *goredis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST != 1")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:     addr,
		Password: os.Getenv("EXC_REDIS_TEST_PASSWORD"),
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// The cap is five per account per UTC day: five allowed, sixth denied
// without consuming budget, a sibling account unaffected, and the key
// expires at the UTC day boundary.
func TestRedisDailyLimiter_CapAndIsolation(t *testing.T) {
	rdb := limiterRedisClient(t)
	ctx := context.Background()
	acct := time.Now().UnixNano() // unique key namespace per run
	lim := NewRedisDailyLimiter(rdb, TaxReportsPerDay)

	for i := 0; i < TaxReportsPerDay; i++ {
		rem, err := lim.Allow(ctx, acct)
		if err != nil {
			t.Fatalf("allow %d: %v", i+1, err)
		}
		if rem != TaxReportsPerDay-1-i {
			t.Fatalf("remaining=%d want %d", rem, TaxReportsPerDay-1-i)
		}
	}
	if _, err := lim.Allow(ctx, acct); err != ErrDailyReportLimit {
		t.Fatalf("6th allow = %v, want ErrDailyReportLimit", err)
	}
	// Rejection rolls the counter back — still exactly max recorded.
	key := fmt.Sprintf("tax:reports:%d:%s", acct, time.Now().UTC().Format("20060102"))
	if got := rdb.Get(ctx, key).Val(); got != fmt.Sprint(TaxReportsPerDay) {
		t.Fatalf("counter=%s want %d (rejection must not consume budget)", got, TaxReportsPerDay)
	}
	// TTL ~ remaining-until-midnight.
	ttl := rdb.TTL(ctx, key).Val()
	if ttl <= 0 || ttl > 24*time.Hour {
		t.Fatalf("ttl=%v not within the UTC day", ttl)
	}
	// A different account has its own bucket.
	if _, err := lim.Allow(ctx, acct+1); err != nil {
		t.Fatalf("sibling account blocked: %v", err)
	}
	t.Cleanup(func() {
		rdb.Del(ctx, key,
			fmt.Sprintf("tax:reports:%d:%s", acct+1, time.Now().UTC().Format("20060102")))
	})
}
