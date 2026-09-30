// option_delta_source_pg_test.go — PostgreSQL-gated coverage for
// PgOptionDeltaSource (Task 19.3.25 back-fit): the real
// option_positions (migration 255) row scan → OptionPosition mapping,
// holder/writer sign convention over the live book, non-USD quote
// conversion, market-read failure propagation, and the empty-book
// short-circuit. Gated on EXC_PG_TEST=1 + EXC_TEST_DSN (falls back to
// the docker-compose DSN) — same convention as the derivatives
// lifecycle pg tests.
package risk

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// optDeltaAnchorDDL mirrors the lifecycle pg fixture — the anchor
// parents the verbatim 036/254/255 migrations need — plus the
// instruments.contract_size column the notional leg reads.
const optDeltaAnchorDDL = `
CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY);
CREATE TABLE instruments (
    id BIGSERIAL PRIMARY KEY,
    symbol VARCHAR(32) UNIQUE,
    instrument_type VARCHAR(16) NOT NULL,
    base_currency VARCHAR(3) NOT NULL,
    quote_currency VARCHAR(3) NOT NULL,
    contract_size DECIMAL(28,8) DEFAULT 100000,
    settlement_cycle INT NOT NULL DEFAULT 1,
    max_leverage BIGINT NOT NULL DEFAULT 1,
    status VARCHAR(16) NOT NULL DEFAULT 'ACTIVE');
CREATE TYPE position_side_enum AS ENUM ('LONG','SHORT');
CREATE TABLE positions (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts (id),
    instrument_id BIGINT NOT NULL REFERENCES instruments (id),
    side position_side_enum NOT NULL,
    quantity DECIMAL(28,8) NOT NULL,
    entry_price DECIMAL(28,8) NOT NULL DEFAULT 0,
    mark_price DECIMAL(28,8),
    unrealized_pnl DECIMAL(28,8) NOT NULL DEFAULT 0,
    realized_pnl DECIMAL(28,8) NOT NULL DEFAULT 0,
    margin_used DECIMAL(28,8) NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE settlement_instructions (
    id BIGSERIAL PRIMARY KEY,
    trade_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL,
    amount DECIMAL(28,8) NOT NULL,
    direction VARCHAR(8) NOT NULL,
    settlement_date DATE,
    status VARCHAR(12) NOT NULL DEFAULT 'PENDING',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at TIMESTAMPTZ);
CREATE TABLE balances (
    account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL,
    available DECIMAL(28,8) NOT NULL DEFAULT 0,
    locked DECIMAL(28,8) NOT NULL DEFAULT 0,
    version BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, currency));
CREATE TYPE ledger_direction_enum AS ENUM ('DEBIT','CREDIT');
CREATE TYPE ledger_entry_type_enum AS ENUM
    ('DEPOSIT','WITHDRAWAL','TRADE_FILL','FEE','TRANSFER','SETTLEMENT','ROLLOVER','LIQUIDATION','ADJUSTMENT');
CREATE TABLE ledger_entries (
    id BIGSERIAL PRIMARY KEY,
    entry_type ledger_entry_type_enum NOT NULL,
    reference_id BIGINT,
    account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL,
    direction ledger_direction_enum NOT NULL,
    amount DECIMAL(28,8) NOT NULL CHECK (amount > 0),
    running_balance DECIMAL(28,8) NOT NULL,
    description VARCHAR(255),
    journal_entry_id BIGINT,
    posted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_by VARCHAR(64));
CREATE TABLE journal_sums (
    account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL,
    total_debits DECIMAL(28,8) NOT NULL DEFAULT 0,
    total_credits DECIMAL(28,8) NOT NULL DEFAULT 0,
    net_balance DECIMAL(28,8) GENERATED ALWAYS AS (total_debits - total_credits) STORED,
    entry_count BIGINT NOT NULL DEFAULT 0,
    last_entry_id BIGINT,
    PRIMARY KEY (account_id, currency));
CREATE SEQUENCE audit_hash_chain_id_seq;
CREATE TABLE audit_hash_chain (
    id BIGINT PRIMARY KEY,
    sequence_num BIGINT NOT NULL UNIQUE,
    table_name VARCHAR(64) NOT NULL,
    record_id BIGINT,
    action VARCHAR(16) NOT NULL,
    payload_hash VARCHAR(64) NOT NULL,
    prev_hash VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL)`

func optDeltaExecList(t *testing.T, ctx context.Context, dsn, schema, ddl string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(ctx)
	for _, stmt := range strings.Split(ddl, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture DDL %q: %v", stmt[:min(60, len(stmt))], err)
		}
	}
}

func optDeltaMig(t *testing.T, ctx context.Context, dsn, schema, file string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(ctx)
	sql, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

// optDeltaFixture builds a scratch schema with the verbatim 255
// option_positions schema and returns a search_path-bound pool.
func optDeltaFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("EXC_PG_DSN")
	}
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	schema := fmt.Sprintf("optd_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(c, dsn)
		if err == nil {
			conn.Exec(c, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c)
		}
	})
	boot.Close(ctx)

	optDeltaExecList(t, ctx, dsn, schema, optDeltaAnchorDDL)
	migDir := filepath.Join("..", "db", "migrations")
	optDeltaMig(t, ctx, dsn, schema, filepath.Join(migDir, "036_create_general_ledger.up.sql"))
	optDeltaMig(t, ctx, dsn, schema, filepath.Join(migDir, "254_derivative_contracts.up.sql"))
	optDeltaMig(t, ctx, dsn, schema, filepath.Join(migDir, "255_derivatives_roll_lifecycle.up.sql"))

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// optDeltaSeed books one OPEN option leg and returns its position id.
func optDeltaSeed(t *testing.T, pool *pgxpool.Pool, acct, undID int64,
	undBase, undQuote, side, typ, style, strike, qty string) int64 {
	t.Helper()
	ctx := context.Background()
	var instrID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments (symbol, instrument_type, base_currency,
			quote_currency, contract_size)
		VALUES ($1,'OPTION','OPT','USD',100000) RETURNING id`,
		fmt.Sprintf("OPT-%s-%s-%s", side, typ, time.Now().Format("150405.000000")),
	).Scan(&instrID); err != nil {
		t.Fatalf("seed option instrument: %v", err)
	}
	var posID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity)
		VALUES ($1,$2,$3,$4::decimal) RETURNING id`,
		acct, instrID, side, qty).Scan(&posID); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO option_positions
			(position_id, account_id, instrument_id,
			 underlying_instrument_id, side, option_type, exercise_style,
			 settlement, strike, quantity, expiry_at, status)
		VALUES ($1,$2,$3,$4,$5::position_side_enum,$6,$7,'PHYSICAL',
			$8::decimal,$9::decimal,$10,'OPEN')`,
		posID, acct, instrID, undID, side, typ, style, strike, qty,
		time.Now().UTC().Add(45*24*time.Hour)); err != nil {
		t.Fatalf("seed option_position: %v", err)
	}
	return posID
}

func TestPgOptionDeltaSource(t *testing.T) {
	pool := optDeltaFixture(t)
	ctx := context.Background()

	var acct, undEUR, undJPY int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts DEFAULT VALUES RETURNING id`).Scan(&acct); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	for i, c := range [][2]string{{"EUR", "USD"}, {"USD", "JPY"}} {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO instruments (symbol, instrument_type,
				base_currency, quote_currency)
			VALUES ($1,'SPOT',$2,$3) RETURNING id`,
			c[0]+"/"+c[1], c[0], c[1]).Scan(&id); err != nil {
			t.Fatalf("seed underlying %s: %v", c, err)
		}
		if i == 0 {
			undEUR = id
		} else {
			undJPY = id
		}
	}

	// One LONG EUR/USD-quoted CALL + one SHORT USD/JPY-quoted PUT.
	posCall := optDeltaSeed(t, pool, acct, undEUR, "EUR", "USD",
		"LONG", "CALL", "EUROPEAN", "1.10", "2")
	posPut := optDeltaSeed(t, pool, acct, undJPY, "USD", "JPY",
		"SHORT", "PUT", "AMERICAN", "150", "3")

	markets := &fakeOptMarkets{m: gkMarket}
	pricer := fakeOptPricer{res: OptionPricerResult{
		Delta: 0.60, Mark: 0.025, Model: "GK"}}
	usdRates := fakeUSDRates{m: map[string]decimal.Decimal{
		"USD": decimal.NewFromInt(1), "JPY": d("0.0066666667"),
	}}
	src := &PgOptionDeltaSource{Pool: pool, Markets: markets,
		Pricer: pricer, Rates: usdRates}

	book, err := src.OptionPositions(ctx, acct)
	if err != nil {
		t.Fatalf("OptionPositions: %v", err)
	}
	if len(book) != 2 {
		t.Fatalf("book len %d, want 2", len(book))
	}
	// Ordered by position_id → holder first (seeded first).
	hold, wrt := book[0], book[1]
	if hold.InstrumentID == 0 || hold.UnderlyingID != undEUR ||
		hold.Side != "LONG" || !hold.Quantity.Equal(d("2")) {
		t.Fatalf("holder row mapping: %+v", hold)
	}
	if !hold.Delta.Equal(d("0.6")) {
		t.Fatalf("holder delta %s, want +0.6", hold.Delta)
	}
	// notional = 100000 × 1.20 spot × 1 USD/USD = 120000.
	if !hold.NotionalUSD.Equal(d("120000")) {
		t.Fatalf("holder notional %s, want 120000", hold.NotionalUSD)
	}
	if wrt.Side != "SHORT" || !wrt.Delta.Equal(d("-0.6")) {
		t.Fatalf("writer delta %s, want −0.6 (negated)", wrt.Delta)
	}
	// JPY-quoted: notional = 100000 × 1.20 × 0.0066666667 ≈ 800.
	want := decimal.NewFromInt(100000).Mul(decimal.NewFromFloat(1.20)).
		Mul(d("0.0066666667")).Round(8)
	if !wrt.NotionalUSD.Equal(want) {
		t.Fatalf("writer notional %s, want %s", wrt.NotionalUSD, want)
	}
	_ = posCall
	_ = posPut

	// Market-read failure propagates — never a zeroed book.
	src2 := &PgOptionDeltaSource{Pool: pool,
		Markets: &fakeOptMarkets{err: fmt.Errorf("mark feed down")},
		Pricer:  pricer, Rates: usdRates}
	if _, err := src2.OptionPositions(ctx, acct); err == nil {
		t.Fatal("market error must propagate fail-closed")
	}

	// Empty book → nil positions, ZERO market reads.
	var otherAcct int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts DEFAULT VALUES RETURNING id`).Scan(&otherAcct); err != nil {
		t.Fatal(err)
	}
	mk2 := &fakeOptMarkets{err: fmt.Errorf("must not be consulted")}
	src3 := &PgOptionDeltaSource{Pool: pool, Markets: mk2, Pricer: pricer}
	book, err = src3.OptionPositions(ctx, otherAcct)
	if err != nil || book != nil {
		t.Fatalf("empty book: %v %v", book, err)
	}
	if len(mk2.req) != 0 {
		t.Fatalf("empty book must not consult markets (%d calls)", len(mk2.req))
	}
}

// TestPgOptionDeltaSource_EndToEndThroughEvaluator wires the PG source
// into DeltaOptionMarginEvaluator — the production composition — and
// verifies the equity adjustment over the live book.
func TestPgOptionDeltaSource_EndToEndThroughEvaluator(t *testing.T) {
	pool := optDeltaFixture(t)
	ctx := context.Background()
	var acct, und int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts DEFAULT VALUES RETURNING id`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments (symbol, instrument_type,
			base_currency, quote_currency)
		VALUES ('EUR/USD','SPOT','EUR','USD') RETURNING id`).Scan(&und); err != nil {
		t.Fatal(err)
	}
	optDeltaSeed(t, pool, acct, und, "EUR", "USD",
		"LONG", "CALL", "EUROPEAN", "1.10", "2")

	ev := DeltaOptionMarginEvaluator{Source: &PgOptionDeltaSource{
		Pool:    pool,
		Markets: &fakeOptMarkets{m: gkMarket},
		Pricer: fakeOptPricer{res: OptionPricerResult{
			Delta: 0.5, Mark: 0.02, Model: "GK"}},
	}}
	adj, err := ev.DeltaEquityAdj(ctx, acct)
	if err != nil {
		t.Fatalf("DeltaEquityAdj: %v", err)
	}
	// 2 contracts × 0.5 delta × 120000 notional = 120000.
	if !adj.Equal(d("120000")) {
		t.Fatalf("delta adjustment %s, want 120000", adj)
	}
}
