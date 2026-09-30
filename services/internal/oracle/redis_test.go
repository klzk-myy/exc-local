package oracle

// Redis-gated integration tests (EXC_REDIS_TEST=1, db 14):
//	go test ./internal/oracle/ -run 'TestRedis' -v
//
// These pin the published key contracts — every consumer (margin
// engine, liquidation ladder, order gate, C++ core poller) reads
// these keyspaces, so drift here is a breaking defect.

import (
	"context"
	"os"
	"testing"
	"time"

	"exchange/internal/redis"
	"exchange/pkg/decimal"
)

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	if err := rdb.Ping(context.Background()); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestRedisPublishMarkKeys(t *testing.T) {
	rdb := redisClient(t)
	ctx := context.Background()
	pub := &RedisPublisher{C: rdb.Client}
	at := time.Now()
	res := MarkResult{
		Symbol: "EUR/USD", Mark: decimal.RequireFromString("1.10025"),
		Index: decimal.RequireFromString("1.10010"), At: at, OK: true,
	}
	if err := pub.PublishMark(ctx, res); err != nil {
		t.Fatalf("PublishMark: %v", err)
	}
	// Every contracted keyspace carries the round (Task 19.5.3.2).
	for _, k := range []string{
		"mark:EUR/USD", "mark_price:EUR/USD",
		"oracle:mark:EUR/USD", "oracle:mark:EUR/USD:ts",
		"index_price:EUR/USD", "oracle:index:EUR/USD",
		"oracle:index:EUR/USD:ts",
	} {
		if err := rdb.Get(ctx, k).Err(); err != nil {
			t.Fatalf("key %s missing: %v", k, err)
		}
	}
	// oracle:mark carries the 1e8-scaled int (C++ contract).
	if v, _ := rdb.Get(ctx, "oracle:mark:EUR/USD").Result(); v != "110025000" {
		t.Fatalf("oracle:mark = %q, want 1e8-scaled 110025000", v)
	}
	if v, _ := rdb.Get(ctx, "mark:EUR/USD").Result(); v != "1.10025" {
		t.Fatalf("mark = %q", v)
	}
}

func TestRedisProviderReadAndGate(t *testing.T) {
	rdb := redisClient(t)
	ctx := context.Background()
	pub := &RedisPublisher{C: rdb.Client}
	prov := NewProvider(rdb.Client)
	sym := "USD/JPY"
	if err := pub.PublishMark(ctx, MarkResult{
		Symbol: sym, Mark: decimal.RequireFromString("150.25"),
		Index: decimal.RequireFromString("150.25"), At: time.Now(), OK: true,
	}); err != nil {
		t.Fatalf("PublishMark: %v", err)
	}
	v, err := prov.Mark(ctx, sym)
	if err != nil || !v.Found {
		t.Fatalf("Mark: %+v err=%v", v, err)
	}
	if !v.Price.Equal(decimal.RequireFromString("150.25")) {
		t.Fatalf("provider price %s", v.Price)
	}
	if v.Stale {
		t.Fatal("fresh mark must not read stale")
	}
	// Health gate: never-published symbol is UNAVAILABLE (silent oracle
	// fails closed), published OK health admits.
	if err := prov.GateOrderAdmission(ctx, "GBP/CHF"); err == nil {
		t.Fatal("absent health must fail closed")
	}
	if err := pub.PublishHealth(ctx, sym, HealthOK, 0.5); err != nil {
		t.Fatalf("PublishHealth: %v", err)
	}
	if err := prov.GateOrderAdmission(ctx, sym); err != nil {
		t.Fatalf("OK health must admit, got %v", err)
	}
	if err := pub.PublishHealth(ctx, sym, HealthUnavailable, 30); err != nil {
		t.Fatal(err)
	}
	if err := prov.GateOrderAdmission(ctx, sym); err == nil {
		t.Fatal("UNAVAILABLE health must reject with PRICE_ORACLE_UNAVAILABLE")
	}
	// Stale marks still return the price flagged — consumers apply
	// their own gate (Task 19.5.3.6 ladder reads it).
	old := MarkResult{Symbol: "GBP/USD", Mark: decimal.One,
		Index: decimal.One, At: time.Now().Add(-time.Minute), OK: true}
	if err := pub.PublishMark(ctx, old); err != nil {
		t.Fatal(err)
	}
	v, err = prov.Mark(ctx, "GBP/USD")
	if err != nil || !v.Found || !v.Stale {
		t.Fatalf("stale mark must read with Stale=true, got %+v err=%v", v, err)
	}
}

func TestRedisFallbackPublication(t *testing.T) {
	rdb := redisClient(t)
	ctx := context.Background()
	pub := &RedisPublisher{C: rdb.Client}
	ref := &StaleReference{
		Symbol: "EUR/USD", State: FallbackStalePrice, Tier: TierStaleMedium,
		LastMark: decimal.RequireFromString("1.10"), LastAt: time.Now(),
		SinceAt: time.Now(),
	}
	if err := pub.PublishFallback(ctx, ref); err != nil {
		t.Fatalf("PublishFallback: %v", err)
	}
	if err := rdb.Get(ctx, "oracle:fallback:EUR/USD").Err(); err != nil {
		t.Fatalf("fallback key missing: %v", err)
	}
	// Freeze key only under FLASH_COOL.
	if err := pub.PublishFallback(ctx, &StaleReference{
		Symbol: "EUR/USD", State: FallbackFlashCool, Tier: TierStaleShort,
		LastMark: decimal.One, FreezeUntil: time.Now().Add(5 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Get(ctx, "oracle:fallback:EUR/USD:freeze").Err(); err != nil {
		t.Fatalf("freeze key missing: %v", err)
	}
	if err := pub.ClearFallback(ctx, "EUR/USD"); err != nil {
		t.Fatalf("ClearFallback: %v", err)
	}
	if err := rdb.Get(ctx, "oracle:fallback:EUR/USD").Err(); err == nil {
		t.Fatal("cleared fallback key must be gone")
	}
}
