// PostgreSQL integration tests for PgStore — the Phase-05 wave-2 cluster C
// read model (Tasks 5.3.5/5.3.14/5.3.35/5.3.43/5.3.44).
//
// Gated: skipped unless EXC_PG_TEST=1. The default DSN targets the scratch
// database (unix socket /tmp:55433, db w2c); override with EXC_PG_DSN.
// Each test applies migrations 001/050/170/171/172/173 plus test-local
// orders/trades DDL (mirroring migrations 005/006 sans the accounts FK and
// pg_partman partitioning — the scratch server lacks the extension; column
// names/types are identical) into a throwaway schema.
//
// Run: EXC_PG_TEST=1 go test ./internal/marketapi/ -run Integration -v
package marketapi

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func marketTestDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	// Scratch instance per the wave-2 test environment contract.
	return "postgres://postgres@/w2c?host=/tmp&port=55433"
}

// Test-local DDL mirroring migrations 005/006 for the columns PgStore
// reads — without the accounts FK and pg_partman partitioning (absent on
// the scratch server). Column names/types match the real DDL exactly.
const testOrdersDDL = `
CREATE TABLE orders (
    id              BIGSERIAL PRIMARY KEY,
    account_id      BIGINT NOT NULL,
    instrument_id   BIGINT NOT NULL REFERENCES instruments (id),
    client_order_id VARCHAR(64),
    side            order_side_enum   NOT NULL,
    order_type      order_type_enum   NOT NULL,
    quantity        DECIMAL(28,8)     NOT NULL,
    price           DECIMAL(20,8),
    stop_price      DECIMAL(20,8),
    time_in_force   time_in_force_enum NOT NULL,
    status          order_status_enum  NOT NULL DEFAULT 'PENDING',
    filled_qty      DECIMAL(28,8)     NOT NULL DEFAULT 0,
    avg_fill_price  DECIMAL(20,8),
    shard_id        SMALLINT,
    book_seq        BIGINT,
    order_seq       BIGINT          NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
)`

const testTradesDDL = `
CREATE TABLE trades (
    id                BIGINT GENERATED ALWAYS AS IDENTITY,
    instrument_id     BIGINT NOT NULL,
    buy_order_id      BIGINT NOT NULL,
    sell_order_id     BIGINT NOT NULL,
    buyer_account_id  BIGINT NOT NULL,
    seller_account_id BIGINT NOT NULL,
    price             DECIMAL(20,8) NOT NULL,
    quantity          DECIMAL(28,8) NOT NULL,
    buyer_fee         DECIMAL(20,8),
    seller_fee        DECIMAL(20,8),
    settlement_date   DATE,
    shard_id          SMALLINT,
    trade_seq         BIGINT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
)`

// itest applies the cluster-C migrations plus the mirrored order/trade
// DDL into a fresh schema and returns a pool bound to it.
func itest(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("mkt_itest_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(marketTestDSN())
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
	for _, f := range []string{
		"001_create_instruments", "050_instruments_min_notional",
		"087_instrument_reference",
		"170_instrument_filter_columns",
		"171_announcements", "172_maintenance_windows", "173_fx_klines",
	} {
		sql, err := os.ReadFile("../db/migrations/" + f + ".up.sql")
		if err != nil {
			conn.Close(ctx)
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			conn.Close(ctx)
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	// Enum types needed by the mirrored orders DDL (from migration 005).
	if _, err := conn.Exec(ctx, `
		CREATE TYPE order_side_enum    AS ENUM ('BUY','SELL');
		CREATE TYPE order_type_enum    AS ENUM ('LIMIT','MARKET');
		CREATE TYPE time_in_force_enum AS ENUM ('GTC','IOC','FOK','GTD','DAY');
		CREATE TYPE order_status_enum  AS ENUM ('PENDING','RESERVED','ACTIVE',
		    'PARTIALLY_FILLED','FILLED','CANCELLED','REJECTED','EXPIRED');`); err != nil {
		conn.Close(ctx)
		t.Fatalf("order enums: %v", err)
	}
	if _, err := conn.Exec(ctx, testOrdersDDL); err != nil {
		conn.Close(ctx)
		t.Fatalf("orders ddl: %v", err)
	}
	if _, err := conn.Exec(ctx, testTradesDDL); err != nil {
		conn.Close(ctx)
		t.Fatalf("trades ddl: %v", err)
	}
	conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(marketTestDSN())
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
		admin, err := pgx.Connect(ctx, marketTestDSN())
		if err == nil {
			_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			admin.Close(ctx)
		}
	})
	return pool, ctx
}

func TestPgStoreInstrumentsIntegration(t *testing.T) {
	pool, ctx := itest(t)
	s := NewPgStore(pool)

	list, err := s.ListInstruments(ctx)
	if err != nil {
		t.Fatalf("ListInstruments: %v", err)
	}
	// 8 from 001 + the 4 §2.2 production-seed rows 087 backfills.
	if len(list) != 12 {
		t.Fatalf("seeded instruments=%d, want 12", len(list))
	}
	eu, err := s.InstrumentBySymbol(ctx, "EUR/USD")
	if err != nil || eu == nil {
		t.Fatalf("InstrumentBySymbol: %v %v", eu, err)
	}
	if eu.TickSize != "0.00001000" && eu.TickSize != "0.00001" {
		t.Fatalf("tick_size=%q", eu.TickSize)
	}
	// Migration 170 seeds: min_price=tick_size, majors 50-pip spread cap,
	// 200 resting / 50 algo order ceilings.
	if eu.MinPrice == nil || eu.MaxSpreadPips == nil ||
		eu.MaxOpenOrders == nil || *eu.MaxOpenOrders != 200 {
		t.Fatalf("170 seed missing: %+v", eu)
	}
	if len(eu.Filters()) != 6 {
		t.Fatalf("filters=%d", len(eu.Filters()))
	}
	missing, err := s.InstrumentBySymbol(ctx, "ZZZ/AAA")
	if err != nil || missing != nil {
		t.Fatalf("unknown symbol should be (nil,nil): %v %v", missing, err)
	}
}

func TestPgStoreBookAndTickerIntegration(t *testing.T) {
	pool, ctx := itest(t)
	s := NewPgStore(pool)

	// Resting orders: two bid levels, one ask; a FILLED row contributes
	// nothing; a market order (NULL price) is excluded.
	var iid int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='EUR/USD'`).Scan(&iid); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO orders (account_id, instrument_id, side, order_type,
	        quantity, price, time_in_force, status, filled_qty, book_seq,
	        order_seq)
	        VALUES ($1,$2,$3,'LIMIT',$4,$5,'GTC',$6,$7,$8,$9)`
	for _, r := range []struct {
		side   string
		qty    float64
		price  float64
		status string
		filled float64
		seq    int64
	}{
		{"BUY", 10000, 1.08410, "ACTIVE", 0, 41},
		{"BUY", 5000, 1.08410, "PARTIALLY_FILLED", 2000, 42}, // same level: 3000 left
		{"BUY", 8000, 1.08400, "ACTIVE", 0, 40},
		{"SELL", 6000, 1.08425, "ACTIVE", 0, 43},
		{"BUY", 9000, 1.00000, "FILLED", 9000, 39},
	} {
		if _, err := pool.Exec(ctx, ins, 1, iid, r.side, r.qty, r.price,
			r.status, r.filled, r.seq, r.seq+1000); err != nil {
			t.Fatalf("seed order: %v", err)
		}
	}
	// order_seq=0 rows are never engine-dispatched — a direct-INSERT row
	// must not render as book liquidity (phantom-depth guard).
	if _, err := pool.Exec(ctx, `INSERT INTO orders (account_id,
	        instrument_id, side, order_type, quantity, price, time_in_force,
	        status, filled_qty) VALUES (1,$1,'BUY','LIMIT',7000,0.5,'GTC',
	        'ACTIVE',0)`, iid); err != nil {
		t.Fatalf("seed phantom: %v", err)
	}
	snap, err := s.Snapshot(ctx, "EUR/USD", 20)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Seq != 43 || len(snap.Bids) != 2 || len(snap.Asks) != 1 {
		t.Fatalf("snap=%+v", snap)
	}
	if snap.Bids[0].Price != "1.08410000" || snap.Bids[0].Quantity != "13000.00000000" {
		t.Fatalf("bid level=%+v", snap.Bids[0])
	}
	// depth bound
	top, _ := s.Snapshot(ctx, "EUR/USD", 1)
	if len(top.Bids) != 1 {
		t.Fatalf("depth=1 bids=%d", len(top.Bids))
	}
	miss, err := s.Snapshot(ctx, "ZZZ/AAA", 20)
	if err != nil || miss != nil {
		t.Fatalf("unknown book should be (nil,nil)")
	}

	// Trades inside and outside the 24h window.
	insT := `INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
	         buyer_account_id, seller_account_id, price, quantity, trade_seq, created_at)
	         VALUES ($1,1,2,1,2,$2,$3,$4,$5)`
	now := time.Now()
	if _, err := pool.Exec(ctx, insT, iid, 1.08400, 5000, 1, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insT, iid, 1.08500, 3000, 2, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insT, iid, 9.90000, 7000, 3, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err) // outside window — must not enter the 24h aggregate
	}
	tr, err := s.RecentTrades(ctx, "EUR/USD", 10)
	if err != nil || len(tr) != 3 {
		t.Fatalf("RecentTrades: %v len=%d", err, len(tr))
	}
	tk, err := s.Ticker24h(ctx, "EUR/USD", now)
	if err != nil || tk == nil {
		t.Fatalf("Ticker24h: %v", err)
	}
	if tk.TradeCount != 2 || tk.High == nil || *tk.High != "1.08500000" {
		t.Fatalf("ticker=%+v", tk)
	}
	if tk.PriceChange == nil || tk.PriceChangePct == nil {
		t.Fatalf("ticker change fields=%+v", tk)
	}
	// Quiet symbol: zeroed ticker with nil prices — no fabrication.
	qt, err := s.Ticker24h(ctx, "GBP/USD", now)
	if err != nil || qt == nil || qt.TradeCount != 0 || qt.Last != nil {
		t.Fatalf("quiet ticker=%+v err=%v", qt, err)
	}
}

// TestPgStoreStats24hIntegration covers the Task 11.3.5 rolling-24h
// statistics surface: single-symbol, venue-wide (quiet instruments
// present with zeroed aggregates), unknown-symbol (nil,nil) and the
// window-edge exclusion.
func TestPgStoreStats24hIntegration(t *testing.T) {
	pool, ctx := itest(t)
	s := NewPgStore(pool)

	var iid int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='EUR/USD'`).Scan(&iid); err != nil {
		t.Fatal(err)
	}
	insT := `INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
	         buyer_account_id, seller_account_id, price, quantity, trade_seq, created_at)
	         VALUES ($1,1,2,1,2,$2,$3,$4,$5)`
	now := time.Now()
	// Two in-window trades: 5000@1.08400 then 3000@1.08500.
	if _, err := pool.Exec(ctx, insT, iid, 1.08400, 5000, 1, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insT, iid, 1.08500, 3000, 2, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insT, iid, 9.9, 7000, 3, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err) // outside window
	}

	st, err := s.Stats24h(ctx, "EUR/USD", now)
	if err != nil || st == nil {
		t.Fatalf("Stats24h: %v", err)
	}
	if st.Window != "24h" || st.Symbol != "EUR/USD" || st.TradeCount != 2 {
		t.Fatalf("stats=%+v", st)
	}
	if st.Volume != "8000.00000000" || st.QuoteVolume != "8675.00000000" {
		t.Fatalf("volumes %s/%s", st.Volume, st.QuoteVolume)
	}
	if st.Open == nil || *st.Open != "1.08400000" ||
		st.Last == nil || *st.Last != "1.08500000" ||
		st.High == nil || *st.High != "1.08500000" ||
		st.Low == nil || *st.Low != "1.08400000" {
		t.Fatalf("prices o=%v h=%v l=%v c=%v", st.Open, st.High, st.Low, st.Last)
	}
	if st.PriceChange == nil || *st.PriceChange != "0.001" ||
		st.PriceChangePct == nil {
		t.Fatalf("change %v / %v", st.PriceChange, st.PriceChangePct)
	}
	if st.FirstTradeMs == nil || st.LastTradeMs == nil ||
		*st.LastTradeMs <= *st.FirstTradeMs {
		t.Fatalf("trade bounds %v %v", st.FirstTradeMs, st.LastTradeMs)
	}

	// Venue-wide: every instrument present; EUR/USD carries the trades.
	// 12 seeded (001's 8 + the 4 rows 087 backfills).
	all, err := s.Stats24hAll(ctx, now)
	if err != nil || len(all) != 12 {
		t.Fatalf("Stats24hAll len=%d err=%v", len(all), err)
	}
	found, quiet := false, 0
	for _, r := range all {
		if r.Symbol == "EUR/USD" {
			found = true
			if r.TradeCount != 2 {
				t.Fatalf("all: EUR/USD stats=%+v", r)
			}
		} else if r.TradeCount == 0 && r.Last == nil && r.Volume == "0" {
			quiet++
		}
	}
	if !found || quiet != 11 {
		t.Fatalf("found=%v quiet=%d", found, quiet)
	}

	// Quiet symbol, single-variant: zeroed row, nil price fields.
	qs, err := s.Stats24h(ctx, "GBP/USD", now)
	if err != nil || qs == nil || qs.TradeCount != 0 ||
		qs.Open != nil || qs.Last != nil || qs.PriceChangePct != nil {
		t.Fatalf("quiet stats=%+v err=%v", qs, err)
	}
	// Unknown symbol → (nil, nil), never a fabricated market.
	u, err := s.Stats24h(ctx, "ZZZ/AAA", now)
	if err != nil || u != nil {
		t.Fatalf("unknown=%+v err=%v", u, err)
	}
}

func TestPgStoreKlinesIntegration(t *testing.T) {
	pool, ctx := itest(t)
	s := NewPgStore(pool)

	var iid int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol='EUR/USD'`).Scan(&iid); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO fx_klines
			(instrument_id, symbol, timeframe, open_time,
			 open, high, low, close, volume, quote_volume, trade_count, closed)
			VALUES ($1,'EUR/USD','1m',$2, 1.08,1.09,1.07,1.085, 100,108, 4, true)`,
			iid, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed kline: %v", err)
		}
	}
	ks, err := s.Klines(ctx, "EUR/USD", "1m", base.Add(-time.Hour),
		base.Add(time.Hour), 3)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(ks) != 3 || ks[0].OpenTimeMs != base.Add(2*time.Minute).UnixMilli() {
		t.Fatalf("klines=%+v (want ascending, newest 3)", ks)
	}
	// Back-page via `to`.
	older, err := s.Klines(ctx, "EUR/USD", "1m", base.Add(-time.Hour),
		base.Add(2*time.Minute), 10)
	if err != nil || len(older) != 2 {
		t.Fatalf("back-page=%d err=%v", len(older), err)
	}
	// No writer yet for 5m — honest empty, not an error.
	empty, err := s.Klines(ctx, "EUR/USD", "5m", time.Time{},
		time.Now().Add(time.Hour), 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty tf=%d err=%v", len(empty), err)
	}
}

func TestPgStoreAnnouncementsMaintenanceIntegration(t *testing.T) {
	pool, ctx := itest(t)
	s := NewPgStore(pool)
	now := time.Now().UTC()

	a, err := s.CreateAnnouncement(ctx, Announcement{
		Title: "Fee update", Body: "fees change", Category: "PRODUCT",
		Status: "PUBLISHED", PublishAt: now.Add(-time.Minute), CreatedBy: "ops-1",
	})
	if err != nil || a.ID == 0 {
		t.Fatalf("create: %v %+v", err, a)
	}
	d, err := s.CreateAnnouncement(ctx, Announcement{
		Title: "WIP", Body: "draft", Category: "GENERAL",
		Status: "DRAFT", PublishAt: now,
	})
	if err != nil || d.Status != "DRAFT" {
		t.Fatalf("draft create: %v", err)
	}
	pub, err := s.ListAnnouncements(ctx, AnnouncementFilter{Limit: 50})
	if err != nil || len(pub) != 1 || pub[0].ID != a.ID {
		t.Fatalf("public list=%+v err=%v", pub, err)
	}
	all, err := s.ListAnnouncements(ctx, AnnouncementFilter{IncludeAll: true, Limit: 50})
	if err != nil || len(all) != 2 {
		t.Fatalf("admin list=%d err=%v", len(all), err)
	}
	a.Title = "Fee update v2"
	if _, err := s.UpdateAnnouncement(ctx, *a); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := s.Announcement(ctx, a.ID)
	if got == nil || got.Title != "Fee update v2" {
		t.Fatalf("after update=%+v", got)
	}
	ok, err := s.RetractAnnouncement(ctx, a.ID, "ops-1")
	if err != nil || !ok {
		t.Fatalf("retract: %v ok=%v", err, ok)
	}
	pub, _ = s.ListAnnouncements(ctx, AnnouncementFilter{Limit: 50})
	if len(pub) != 0 {
		t.Fatal("retracted announcement still public")
	}

	m, err := s.CreateMaintenance(ctx, MaintenanceWindow{
		Title: "Gateway upgrade", Scope: "GATEWAY", Status: "SCHEDULED",
		StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour),
		Symbols: []string{"EUR/USD"}, CreatedBy: "ops-1",
	})
	if err != nil || m.ID == 0 {
		t.Fatalf("create window: %v", err)
	}
	up, err := s.UpcomingMaintenance(ctx, now)
	if err != nil || len(up) != 1 {
		t.Fatalf("upcoming=%d err=%v", len(up), err)
	}
	if len(up[0].Symbols) != 1 || up[0].Symbols[0] != "EUR/USD" {
		t.Fatalf("symbols=%v", up[0].Symbols)
	}
	ok, err = s.CancelMaintenance(ctx, m.ID, "ops-1")
	if err != nil || !ok {
		t.Fatalf("cancel: %v", err)
	}
	up, _ = s.UpcomingMaintenance(ctx, now)
	if len(up) != 0 {
		t.Fatal("cancelled window still upcoming")
	}
	ok, err = s.CancelMaintenance(ctx, 99999, "ops-1")
	if err != nil || ok {
		t.Fatalf("cancel unknown: ok=%v err=%v", ok, err)
	}
}
