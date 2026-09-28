// Task 5.3.19 — PG-backed fill-source test. Gated: EXC_PG_TEST=1, DSN
// via EXC_PG_DSN or EXC_TEST_DSN (default: scratch w2d on the /tmp socket, port 55433).
//
// trades/instruments are mirrored minimal DDL — the real 006 migration
// uses pg_partman partitioning which the scratch server lacks; the
// marketapi precedent establishes mirrored-DDL with identical column
// names/types for the fields under test.
//
// Run: EXC_PG_TEST=1 go test ./internal/tax/ -run Integration -v
package tax

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func taxTestDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	if d := os.Getenv("EXC_TEST_DSN"); d != "" {
		return d
	}
	return "postgres://postgres@/w2d?host=/tmp&port=55433"
}

// taxItest builds a throwaway schema with mirrored trades/instruments
// and returns a pool scoped to it.
func taxItest(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("tax_itest_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(taxTestDSN())
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
	if _, err := conn.Exec(ctx, `
		CREATE TABLE instruments (
		    id            BIGINT PRIMARY KEY,
		    symbol        VARCHAR(20) NOT NULL,
		    base_currency VARCHAR(3)  NOT NULL,
		    quote_currency VARCHAR(3) NOT NULL
		);
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
		    PRIMARY KEY (id, created_at)
		)`); err != nil {
		conn.Close(ctx)
		t.Fatalf("ddl: %v", err)
	}
	conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(taxTestDSN())
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
		c, err := pgx.Connect(ctx, taxTestDSN())
		if err == nil {
			_, _ = c.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			c.Close(ctx)
		}
	})
	return pool, ctx
}

// End-to-end: real rows → PgxSource → Service.Report. Verifies side
// normalization (buyer vs seller side of the same trade), own-side fee
// selection and chronological ordering over the real store.
func TestIntegrationPgxSourceAndReport(t *testing.T) {
	pool, ctx := taxItest(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO instruments (id, symbol, base_currency, quote_currency)
		 VALUES (7, 'EURUSD', 'EUR', 'USD')`); err != nil {
		t.Fatalf("instrument: %v", err)
	}
	ins := func(buyer, seller int64, px, qty, bfee, sfee, ts string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO trades (instrument_id, buy_order_id, sell_order_id,
			    buyer_account_id, seller_account_id, price, quantity,
			    buyer_fee, seller_fee, created_at)
			VALUES (7, 10, 20, $1, $2, $3, $4, $5, $6, $7::timestamptz)`,
			buyer, seller, px, qty, bfee, sfee, ts); err != nil {
			t.Fatalf("trade: %v", err)
		}
	}
	// Account 42 buys 100 @1.10 (fee 0.11) as buyer; sells 100 @1.30
	// (fee 0.13) as seller. Both in 2026.
	ins(42, 99, "1.10", "100", "0.11", "0.09", "2026-01-05T12:00:00Z")
	ins(77, 42, "1.30", "100", "0.05", "0.13", "2026-02-01T12:00:00Z")

	src := NewPgxSource(pool)
	fills, err := src.Fills(ctx, 42, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("fills: %v", err)
	}
	if len(fills) != 2 {
		t.Fatalf("fills=%d want 2", len(fills))
	}
	if fills[0].Side != "BUY" || fills[1].Side != "SELL" {
		t.Fatalf("side normalization wrong: %+v", fills)
	}
	if !fills[0].Fee.Equal(dec(t, "0.11")) || !fills[1].Fee.Equal(dec(t, "0.13")) {
		t.Fatalf("own-side fees wrong: %+v", fills)
	}
	if fills[0].Symbol != "EURUSD" || fills[0].QuoteCurrency != "USD" {
		t.Fatalf("instrument join wrong: %+v", fills[0])
	}

	svc, err := NewService(src)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Report(ctx, 42, 2026, MethodFIFO)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Disposals) != 1 {
		t.Fatalf("disposals=%d want 1: %+v", len(rep.Disposals), rep.Disposals)
	}
	// unitCost = 1.10 + 0.11/100 = 1.1011; proceeds = 130 − 0.13 = 129.87;
	// basis 110.11 → gain 19.76.
	d := rep.Disposals[0]
	if !d.UnitCost.Equal(dec(t, "1.1011")) {
		t.Fatalf("unit cost=%s", d.UnitCost)
	}
	if !d.Gain.Equal(dec(t, "19.76")) {
		t.Fatalf("gain=%s want 19.76", d.Gain)
	}
	// The counterparty sees its own side of trade 1 (seller, fee 0.09)
	// and nothing of trade 2.
	fills99, err := src.Fills(ctx, 99, time.Now().AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(fills99) != 1 || fills99[0].Side != "SELL" ||
		!fills99[0].Fee.Equal(dec(t, "0.09")) {
		t.Fatalf("account 99 fills=%+v — side/fee normalization wrong", fills99)
	}
}
