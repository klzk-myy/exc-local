// PostgreSQL integration test for the fx_klines writer (migration 173,
// Phase-06 Task 6.3.8 step 4).
//
// Gated: skipped unless EXC_PG_TEST=1. Default DSN targets the scratch
// instance (unix socket /tmp:55433, db w2c); override with EXC_PG_DSN.
// Migrations 001 + 173 are applied into a throwaway schema per test run.
//
// Run: EXC_PG_TEST=1 go test ./internal/marketdata/ohlcv/ -run Integration -v
package ohlcv

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func klineTestDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2c?host=/tmp&port=55433"
}

// klineITest applies migrations 001 + 173 into a fresh schema and returns
// a pool bound to it.
func klineITest(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("ohlcv_itest_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(klineTestDSN())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		conn.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	for _, f := range []string{"001_create_instruments", "173_fx_klines"} {
		sql, err := os.ReadFile("../../db/migrations/" + f + ".up.sql")
		if err != nil {
			conn.Close(ctx)
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			conn.Close(ctx)
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(klineTestDSN())
	if err != nil {
		t.Fatalf("pool dsn: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin, err := pgx.Connect(ctx, klineTestDSN())
		if err == nil {
			_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			admin.Close(ctx)
		}
	})
	return pool, ctx
}

func TestPGStoreKlineLifecycleIntegration(t *testing.T) {
	pool, ctx := klineITest(t)
	store := NewPGStore(pool)
	clk := &clock{t: t0}

	e := NewEngine(Config{
		Intervals:   []Interval{I1m},
		GracePeriod: 10 * time.Second,
		Now:         clk.now,
	}, Deps{Store: store})

	// Live-bar upsert (closed=false) on the throttle cadence.
	e.HandleTrade(ctx, trade("EUR/USD", "1.10000", "100", SideBuy, 1, t0.Add(10*time.Second)))
	var (
		closed   bool
		count    int64
		openPx   string
		rowCount int
	)
	err := pool.QueryRow(ctx, `
		SELECT closed, trade_count, open::text, COUNT(*) OVER()
		FROM fx_klines
		WHERE symbol='EUR/USD' AND timeframe='1m' AND open_time=$1`,
		t0).Scan(&closed, &count, &openPx, &rowCount)
	if err != nil {
		t.Fatalf("read live bar: %v", err)
	}
	if closed || count != 1 || rowCount != 1 {
		t.Fatalf("live bar should be open: closed=%v count=%d rows=%d", closed, count, rowCount)
	}

	e.HandleTrade(ctx, trade("EUR/USD", "1.25000", "50", SideSell, 2, t0.Add(40*time.Second)))
	// Throttled: second in-bucket trade within 500ms does not rewrite.
	err = pool.QueryRow(ctx, `
		SELECT high::text FROM fx_klines
		WHERE symbol='EUR/USD' AND timeframe='1m' AND open_time=$1`,
		t0).Scan(&openPx)
	if err != nil {
		t.Fatal(err)
	}
	if openPx != "1.10000000" {
		t.Fatalf("open persist throttle: high=%s", openPx)
	}

	// Finalize past grace → closed=true row with full aggregation.
	e.HandleTrade(ctx, trade("EUR/USD", "1.30000", "1", SideBuy, 3, t0.Add(90*time.Second)))
	e.Advance(ctx, t0.Add(2*time.Minute))
	var (
		o, h, l, c, v, qv string
	)
	err = pool.QueryRow(ctx, `
		SELECT open::text, high::text, low::text, close::text,
		       volume::text, quote_volume::text, trade_count, closed
		FROM fx_klines
		WHERE symbol='EUR/USD' AND timeframe='1m' AND open_time=$1`,
		t0).Scan(&o, &h, &l, &c, &v, &qv, &count, &closed)
	if err != nil {
		t.Fatalf("read closed bar: %v", err)
	}
	if !closed || o != "1.10000000" || h != "1.25000000" ||
		l != "1.10000000" || c != "1.25000000" ||
		v != "150.00000000" || qv != "172.50000000" || count != 2 {
		t.Fatalf("closed row: o=%s h=%s l=%s c=%s v=%s qv=%s n=%d closed=%v",
			o, h, l, c, v, qv, count, closed)
	}
	// instrument_id resolved from instruments (seeded migration 001).
	var iid int64
	if err := pool.QueryRow(ctx,
		`SELECT instrument_id FROM fx_klines WHERE symbol='EUR/USD' AND timeframe='1m'`).
		Scan(&iid); err != nil || iid == 0 {
		t.Fatalf("instrument_id=%d err=%v", iid, err)
	}

	// Closed-row immutability at the DB layer: an open upsert against the
	// finalized key is a no-op.
	stale := Candle{InstrumentID: iid, Symbol: "EUR/USD", Interval: I1m,
		OpenTime: t0, CloseTime: t0.Add(time.Minute),
		Open: dec("9.9"), High: dec("9.9"), Low: dec("9.9"), Close: dec("9.9"),
		Volume: dec("1"), QuoteVolume: dec("9.9"), TradeCount: 1, Closed: false}
	if err := store.Save(ctx, stale); err != nil {
		t.Fatalf("stale upsert err: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT closed, close::text FROM fx_klines
		WHERE symbol='EUR/USD' AND timeframe='1m' AND open_time=$1`,
		t0).Scan(&closed, &c); err != nil {
		t.Fatal(err)
	}
	if !closed || c != "1.25000000" {
		t.Fatalf("closed candle rewritten: closed=%v close=%s", closed, c)
	}

	if m := e.Metrics(); m.PersistErrors != 0 {
		t.Fatalf("PersistErrors=%d", m.PersistErrors)
	}
}

func TestPGArchiveSinkIntegration(t *testing.T) {
	pool, ctx := klineITest(t)
	store := NewPGStore(pool)
	sink := NewPGArchiveSink(store)

	c := Candle{Symbol: "EUR/USD", Interval: I1m, OpenTime: t0,
		CloseTime: t0.Add(time.Minute),
		Open:      dec("1.1"), High: dec("1.2"), Low: dec("1.0"), Close: dec("1.15"),
		Volume: dec("10"), QuoteVolume: dec("11.5"), TradeCount: 3, Closed: true}
	if err := sink.Archive(ctx, c); err != nil {
		t.Fatalf("archive: %v", err)
	}
	c.Closed = false // open bars are a hot-tier concern — ignored
	if err := sink.Archive(ctx, c); err != nil {
		t.Fatalf("archive open: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM fx_klines`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}
