// markprice_test.go — unit + gated integration coverage for the mark-price
// contract cluster (markprice.go; Phase-19 Task 19.3.3 seam, Phase-19.5
// oracle replacement target; spec §13.1/§13.15).
//
// Ungated legs cover StubMarkPriceProvider (Observe/GetMarkPrice/
// provenance/staleness anchor, corrupt-tick rejection, synchronous
// fallback semantics) and the MarkSourceFunc adapter. Gated legs:
//
//	EXC_PG_TEST=1    go test ./internal/risk/ -run 'TestPgLastTrade' -v
//	EXC_REDIS_TEST=1 go test ./internal/risk/ -run 'TestRedisMark' -v
package risk

import (
	"context"
	stderrors "errors"
	"os"
	"sync"
	"testing"
	"time"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// StubMarkPriceProvider — observe/get/provenance
// ---------------------------------------------------------------------------

func TestStubMarkPriceObserveAndGet(t *testing.T) {
	p := NewStubMarkPriceProvider()

	// No observation, no fallback → ErrMarkNotFound (data condition,
	// not an outage).
	if _, err := p.GetMarkPrice("EUR/USD"); err == nil {
		t.Fatal("missing mark must error")
	} else if !stderrors.Is(err, ErrMarkNotFound) {
		t.Fatalf("want ErrMarkNotFound, got %v", err)
	}

	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	p.Observe("EUR/USD", d("1.0850"), at)
	px, err := p.GetMarkPrice("EUR/USD")
	if err != nil {
		t.Fatal(err)
	}
	if !px.Equal(d("1.0850")) {
		t.Fatalf("mark=%s, want 1.0850", px)
	}

	// Provenance: last-trade source, ValidAt = the observation time —
	// the anchor consumers apply their staleness gate against.
	mp, err := p.GetMarkPriceWithProvenance("EUR/USD")
	if err != nil {
		t.Fatal(err)
	}
	if mp.Source != MarkSourceLastTrade || !mp.ValidAt.Equal(at) || mp.Stale {
		t.Fatalf("provenance: %+v", mp)
	}

	// A newer observation wins.
	p.Observe("EUR/USD", d("1.0860"), at.Add(time.Second))
	if px, _ := p.GetMarkPrice("EUR/USD"); !px.Equal(d("1.0860")) {
		t.Fatalf("updated mark=%s, want 1.0860", px)
	}

	// Corrupt feed: non-positive prices can never plant a mark.
	p.Observe("USD/JPY", decimal.Zero, at)
	p.Observe("USD/JPY", d("-150.25"), at)
	if _, err := p.GetMarkPrice("USD/JPY"); !stderrors.Is(err, ErrMarkNotFound) {
		t.Fatalf("non-positive observe must not plant a mark: %v", err)
	}
}

func TestStubMarkPriceStalenessAnchor(t *testing.T) {
	// The stub never marks Stale itself — consumers compare ValidAt
	// against their own staleness gate (oracle phase lands 5s).
	p := NewStubMarkPriceProvider()
	old := time.Now().Add(-10 * time.Second)
	p.Observe("EUR/USD", d("1.10"), old)
	mp, err := p.GetMarkPriceWithProvenance("EUR/USD")
	if err != nil {
		t.Fatal(err)
	}
	if !mp.ValidAt.Equal(old) {
		t.Fatalf("ValidAt=%v, want observation time %v", mp.ValidAt, old)
	}
	// Consumer-side gate arithmetic holds: a 10s-old mark IS stale vs 5s.
	if stale := time.Since(mp.ValidAt) > 5*time.Second; !stale {
		t.Fatal("staleness gate arithmetic broken")
	}
}

func TestStubMarkPriceFallback(t *testing.T) {
	var calls int
	var mu sync.Mutex
	fb := func(symbol string) (decimal.Decimal, bool) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if symbol == "EUR/USD" {
			return d("1.0777"), true
		}
		return decimal.Zero, false
	}
	fixed := time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC)
	p := NewStubMarkPriceProvider().WithFallback(fb).WithClock(func() time.Time { return fixed })

	mp, err := p.GetMarkPriceWithProvenance("EUR/USD")
	if err != nil {
		t.Fatal(err)
	}
	if !mp.Price.Equal(d("1.0777")) || mp.Source != MarkSourceLastTrade {
		t.Fatalf("fallback mark: %+v", mp)
	}
	// Fallback-hit is cached with the injected clock's timestamp.
	if !mp.ValidAt.Equal(fixed) {
		t.Fatalf("fallback ValidAt=%v, want injected clock %v", mp.ValidAt, fixed)
	}
	// Second read serves the cache — the store is not re-hit.
	if _, err := p.GetMarkPrice("EUR/USD"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fallback calls=%d, want 1 (result must be cached)", calls)
	}
	// Fallback miss → ErrMarkNotFound.
	if _, err := p.GetMarkPrice("USD/TRY"); !stderrors.Is(err, ErrMarkNotFound) {
		t.Fatalf("fallback miss: %v", err)
	}
	// A non-positive fallback value is ignored (and not cached).
	calls = 0
	p2 := NewStubMarkPriceProvider().WithFallback(func(string) (decimal.Decimal, bool) {
		calls++
		return decimal.Zero, true
	})
	if _, err := p2.GetMarkPrice("EUR/USD"); !stderrors.Is(err, ErrMarkNotFound) {
		t.Fatalf("non-positive fallback: %v", err)
	}
	if _, err := p2.GetMarkPrice("EUR/USD"); !stderrors.Is(err, ErrMarkNotFound) {
		t.Fatalf("non-positive fallback retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("bad fallback must not cache: calls=%d", calls)
	}
}

func TestMarkSourceFuncAdapter(t *testing.T) {
	ch := make(chan MarkTick, 1)
	ch <- MarkTick{Symbol: "EUR/USD", Price: d("1.1")}
	close(ch)
	src := MarkSourceFunc(func(context.Context) (<-chan MarkTick, error) { return ch, nil })
	out, err := src.Marks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tick := <-out
	if tick.Symbol != "EUR/USD" {
		t.Fatalf("tick: %+v", tick)
	}
}

func TestMarkPriceKeyShape(t *testing.T) {
	if got := MarkPriceKey("EUR/USD"); got != "mark:EUR/USD" {
		t.Fatalf("key %q", got)
	}
	if MarkChannelPattern != "mark:*" {
		t.Fatalf("pattern %q", MarkChannelPattern)
	}
}

// ---------------------------------------------------------------------------
// Gated PostgreSQL — PgLastTradeFallback
// ---------------------------------------------------------------------------

// TestPgLastTradeFallback exercises the synchronous PG cold path. The
// real trades table is pg_partman-partitioned (migration 006) and cannot
// be recreated inside the throwaway schema, so the leg builds the exact
// column shape the fallback query reads (id, instrument_id, price) —
// provenance semantics are what the test asserts.
func TestPgLastTradeFallback(t *testing.T) {
	pool := pgRisk19Fixture(t)
	ctx := context.Background()
	inst := liqSeedInstrument(t, pool) // EUR/USD
	if _, err := pool.Exec(ctx, `
		CREATE TABLE trades (
			id            BIGSERIAL PRIMARY KEY,
			instrument_id BIGINT NOT NULL,
			price         DECIMAL(20,8) NOT NULL
		)`); err != nil {
		t.Fatalf("trades shape: %v", err)
	}
	fb := PgLastTradeFallback(pool)

	// No trades yet → miss, never an error value.
	if px, ok := fb("EUR/USD"); ok || px.IsPositive() {
		t.Fatalf("empty book must miss: %s %v", px, ok)
	}
	// Unknown symbol → miss.
	if _, err := pool.Exec(ctx,
		`INSERT INTO trades (instrument_id, price) VALUES ($1, 1.1000)`, inst); err != nil {
		t.Fatal(err)
	}
	if _, ok := fb("USD/TRY"); ok {
		t.Fatal("unknown symbol must miss")
	}
	// Latest row by id wins.
	if _, err := pool.Exec(ctx,
		`INSERT INTO trades (instrument_id, price) VALUES ($1, 1.1050)`, inst); err != nil {
		t.Fatal(err)
	}
	px, ok := fb("EUR/USD")
	if !ok || !px.Equal(d("1.1050")) {
		t.Fatalf("last trade: %s %v, want 1.1050", px, ok)
	}
	// Wired as the stub's fallback → cold read populates the provider.
	p := NewStubMarkPriceProvider().WithFallback(fb)
	got, err := p.GetMarkPrice("EUR/USD")
	if err != nil || !got.Equal(d("1.1050")) {
		t.Fatalf("provider cold path: %s %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// Gated Redis — RedisMarkCache.BatchMarks + RedisMarkSource
// ---------------------------------------------------------------------------

func redisTestClient(t *testing.T) *excredis.Client {
	t.Helper()
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("EXC_REDIS_TEST not set")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 14)
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		_ = rdb.Close()
		t.Skipf("redis unreachable: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestRedisMarkCacheBatchMarks(t *testing.T) {
	rdb := redisTestClient(t)
	ctx := context.Background()
	cache := NewRedisMarkCache(rdb.Client)
	s1, s2, s3 := "IT/MARK-A", "IT/MARK-B", "IT/MARK-C"
	t.Cleanup(func() {
		_, _ = rdb.Del(ctx, MarkPriceKey(s1), MarkPriceKey(s2), MarkPriceKey(s3)).Result()
	})

	// Empty request → empty map, no round-trip error.
	got, err := cache.BatchMarks(ctx, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty batch: %v %v", got, err)
	}
	// All-absent → empty map (absent is a data condition, not an error).
	got, err = cache.BatchMarks(ctx, []string{s1})
	if err != nil || len(got) != 0 {
		t.Fatalf("absent batch: %v %v", got, err)
	}

	// PublishMarkDelta is the dev/test write path — SET + PUBLISH atomically.
	if err := cache.PublishMarkDelta(ctx, s1, d("1.2345"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cache.PublishMarkDelta(ctx, s2, d("150.125"), time.Now()); err != nil {
		t.Fatal(err)
	}
	// One MGET across the requested keys; duplicates dedup, absent stays out.
	got, err = cache.BatchMarks(ctx, []string{s1, s2, s3, s1, ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[s1].Equal(d("1.2345")) || !got[s2].Equal(d("150.125")) {
		t.Fatalf("batch marks: %+v", got)
	}
	// Malformed payload → coded error, never a silent zero.
	if err := rdb.Set(ctx, MarkPriceKey(s3), "not-a-decimal", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.BatchMarks(ctx, []string{s3}); err == nil {
		t.Fatal("malformed mark must error")
	}
	// Non-positive mark → error (a corrupt feed must not price margin).
	if err := rdb.Set(ctx, MarkPriceKey(s3), "-1.5", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.BatchMarks(ctx, []string{s3}); err == nil {
		t.Fatal("non-positive mark must error")
	}
}

func TestRedisMarkSourceTicks(t *testing.T) {
	rdb := redisTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cache := NewRedisMarkCache(rdb.Client)

	var drops []string
	var mu sync.Mutex
	src := NewRedisMarkSource(rdb.Client, nil)
	src.OnDrop = func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		drops = append(drops, reason)
	}
	ticks, err := src.Marks(ctx)
	if err != nil {
		t.Fatalf("marks: %v", err)
	}

	sym := "IT/TICK-A"
	t.Cleanup(func() { _, _ = rdb.Del(ctx, MarkPriceKey(sym)).Result() })
	if err := cache.PublishMarkDelta(ctx, sym, d("99.5"), time.Now()); err != nil {
		t.Fatal(err)
	}
	// A malformed raw publish is dropped, not fatal.
	if err := rdb.Publish(ctx, MarkPriceKey(sym), "{{{").Err(); err != nil {
		t.Fatal(err)
	}
	if err := cache.PublishMarkDelta(ctx, sym, d("99.6"), time.Now()); err != nil {
		t.Fatal(err)
	}

	var got []MarkTick
	for len(got) < 2 {
		select {
		case tick, ok := <-ticks:
			if !ok {
				t.Fatal("tick channel closed early")
			}
			got = append(got, tick)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for ticks, got %v", got)
		}
	}
	if got[0].Symbol != sym || !got[0].Price.Equal(d("99.5")) ||
		got[1].Symbol != sym || !got[1].Price.Equal(d("99.6")) {
		t.Fatalf("ticks: %+v", got)
	}
	// The malformed frame between them was dropped and counted.
	mu.Lock()
	defer mu.Unlock()
	if len(drops) != 1 || drops[0] != "malformed" {
		t.Fatalf("drops: %v", drops)
	}
}
