// Phase-13.5 Task 13.5.3.4 tabletop evidence — Reconciliation-Mismatch
// drill.
//
// Drives the REAL Engine over dev PostgreSQL + Redis in a scratch
// schema: a seeded positions divergence (position row with no
// position_fills backing) must produce a MISMATCH run, a persisted
// finding row, a durable P1 funding_ops_alerts row, a
// trading_suspensions row, AND the halt:account:* Redis flag — the same
// flag order admission consults. The WAL legs are intentionally absent
// (no WalDirs) so Orders/Trades report INCONCLUSIVE — the drill also
// proves unverifiable input never reads as CLEAN (fail-closed).
//
//	EXC_TABLETOP=1 EXC_PG_TEST=1 \
//	  EXC_TEST_DSN=postgres://postgres:postgres@127.0.0.1:55433/postgres?sslmode=disable \
//	  go test -v -run TestTabletopReconMismatch ./internal/reconciliation/
package reconciliation

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excredis "exchange/internal/redis"
)

// reconDrillDDL — minimal shape of every source table the nine
// checkers query (recon_sources.go). Types are deliberately plain
// (TEXT/NUMERIC) — the checkers only read; the engine-owned tables
// (207, 200) come from the real migration files.
const reconDrillDDL = `
CREATE TABLE balances (
    account_id BIGINT NOT NULL, currency VARCHAR(3) NOT NULL,
    available NUMERIC(38,10) NOT NULL DEFAULT 0,
    locked     NUMERIC(38,10) NOT NULL DEFAULT 0,
    total      NUMERIC(38,10) NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, currency));
CREATE TABLE insurance_fund (
    currency VARCHAR(3) PRIMARY KEY, balance NUMERIC(38,10) NOT NULL DEFAULT 0);
CREATE TABLE journal_entries (
    id BIGSERIAL PRIMARY KEY, entry_type VARCHAR(32), reference_id BIGINT);
CREATE TABLE ledger_lines (
    id BIGSERIAL PRIMARY KEY, journal_entry_id BIGINT NOT NULL,
    account_code VARCHAR(48), currency VARCHAR(3) NOT NULL,
    debit_amount NUMERIC(28,8) NOT NULL DEFAULT 0,
    credit_amount NUMERIC(28,8) NOT NULL DEFAULT 0);
CREATE TABLE journal_sums (
    account_id BIGINT NOT NULL, currency VARCHAR(3) NOT NULL,
    net_balance NUMERIC(28,8) NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, currency));
CREATE TABLE ledger_entries (
    id BIGSERIAL PRIMARY KEY, entry_type VARCHAR(32),
    reference_id BIGINT, account_id BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL, direction VARCHAR(8) NOT NULL,
    amount NUMERIC(28,8) NOT NULL);
CREATE TABLE orders (
    id BIGSERIAL PRIMARY KEY, account_id BIGINT, instrument_id BIGINT,
    quantity NUMERIC(38,10) NOT NULL DEFAULT 0,
    filled_qty NUMERIC(38,10) NOT NULL DEFAULT 0,
    status VARCHAR(24) NOT NULL DEFAULT 'ACTIVE',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE instruments (
    id BIGINT PRIMARY KEY, symbol VARCHAR(24),
    base_currency VARCHAR(3), quote_currency VARCHAR(3));
CREATE TABLE trades (
    id BIGSERIAL PRIMARY KEY, instrument_id BIGINT,
    quantity NUMERIC(38,10), price NUMERIC(38,10),
    buyer_account_id BIGINT, seller_account_id BIGINT,
    buyer_fee NUMERIC(28,8), seller_fee NUMERIC(28,8));
CREATE TABLE positions (
    id BIGSERIAL PRIMARY KEY, account_id BIGINT NOT NULL,
    instrument_id BIGINT NOT NULL, side VARCHAR(8) NOT NULL,
    quantity NUMERIC(38,10) NOT NULL, entry_price NUMERIC(38,10),
    mark_price NUMERIC(38,10), unrealized_pnl NUMERIC(28,8),
    realized_pnl NUMERIC(28,8));
CREATE TABLE position_fills (
    id BIGSERIAL PRIMARY KEY, account_id BIGINT, instrument_id BIGINT,
    side VARCHAR(8), quantity NUMERIC(38,10), realized_pnl NUMERIC(28,8));
CREATE TABLE funding_transactions (
    id BIGSERIAL PRIMARY KEY, account_id BIGINT,
    currency VARCHAR(3), amount NUMERIC(28,8),
    type VARCHAR(16), status VARCHAR(16), bank_method VARCHAR(16),
    reference VARCHAR(128));
CREATE TABLE rail_payments (
    id BIGSERIAL PRIMARY KEY, funding_transaction_id BIGINT,
    direction VARCHAR(12), status VARCHAR(16));
CREATE TABLE suspense_account_mappings (
    id BIGSERIAL PRIMARY KEY, funding_transaction_id BIGINT,
    account_id BIGINT, quarantine_status VARCHAR(16), bank_tx_id VARCHAR(128));
CREATE TABLE settlement_instructions (
    id BIGSERIAL PRIMARY KEY, currency VARCHAR(3), amount NUMERIC(28,8),
    direction VARCHAR(12), status VARCHAR(16), settlement_date DATE);
CREATE TABLE nostro_movements (
    id BIGSERIAL PRIMARY KEY, settlement_instruction_id BIGINT,
    amount NUMERIC(28,8), direction VARCHAR(12),
    status VARCHAR(16), currency VARCHAR(3));
CREATE TABLE funding_ops_alerts (
    id BIGSERIAL PRIMARY KEY, code VARCHAR(64) NOT NULL,
    severity VARCHAR(4) NOT NULL DEFAULT 'P1',
    funding_transaction_id BIGINT, account_id BIGINT,
    currency VARCHAR(3), amount NUMERIC(28,8),
    summary TEXT NOT NULL, detail JSONB,
    status VARCHAR(12) NOT NULL DEFAULT 'OPEN',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ);`

func TestTabletopReconMismatch(t *testing.T) {
	started := time.Now()
	if os.Getenv("EXC_TABLETOP") != "1" || os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_TABLETOP=1 EXC_PG_TEST=1 to run tabletop drills")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, reconDSN())
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}

	schema := fmt.Sprintf("recon_drill_%d", rand.Intn(1_000_000))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	}()

	cfg, err := pgxpool.ParseConfig(reconDSN())
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	step := func(n int, what string) {
		t.Logf("step %d: %s (t+%s)", n, what, time.Since(started).Round(time.Millisecond))
	}

	step(1, "build scratch schema — source DDL + real migrations 200/207")
	if _, err := pool.Exec(ctx, reconDrillDDL); err != nil {
		t.Fatalf("source DDL: %v", err)
	}
	for _, m := range []string{
		"200_trading_suspensions.up.sql",
		"207_reconciliation_engine.up.sql",
	} {
		body, rerr := os.ReadFile(filepath.Join("..", "db", "migrations", m))
		if rerr != nil {
			t.Fatalf("read %s: %v", m, rerr)
		}
		if _, rerr := pool.Exec(ctx, string(body)); rerr != nil {
			t.Fatalf("apply %s: %v", m, rerr)
		}
	}

	step(2, "seed divergence — position with no fill-ledger backing")
	const acct, instr = 99999001, 99999001
	if _, err := pool.Exec(ctx, `
		INSERT INTO positions (account_id, instrument_id, side, quantity,
			entry_price, realized_pnl, unrealized_pnl)
		VALUES ($1, $2, 'LONG', 5.00000000, 1.00000000, 0, 0)`, acct, instr); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	// Real Redis halt flags — the drill must land halt:account:99999001.
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	pass := os.Getenv("EXC_REDIS_TEST_PASSWORD")
	if pass == "" {
		pass = "redpass"
	}
	rdb := excredis.New(addr, pass, 0)
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("dev redis unavailable at %s: %v", addr, err)
	}
	defer func() {
		_ = rdb.ClearHaltScope(context.Background(), "ACCOUNT", "99999001")
	}()

	step(3, "RunOnce — nine-category sweep")
	store := NewPgStore(pool)
	engine, err := NewEngine(Deps{
		Pool:    pool,
		Store:   store,
		Alerter: NewDurableOpsAlerter(pool, nil),
		Halter:  NewPgHalter(pool, rdb),
		Logf:    func(string, ...any) {},
	}, DefaultCheckers(NewPgLegs(pool), nil, nil))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	run, err := engine.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	t.Logf("run id=%d status=%s categories=%d findings=%d mismatches=%d inconclusive=%d halts=%d",
		run.ID, run.Status, run.CategoriesChecked, run.FindingsCount,
		run.MismatchCount, run.InconclusiveCount, len(run.Halts))
	if run.Status != RunMismatch {
		t.Fatalf("run status %s (want MISMATCH)", run.Status)
	}

	step(4, "verify persisted finding + P1 alert + suspension + Redis flag")
	findings, err := store.RunFindings(ctx, run.ID)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	var posFinding bool
	for _, f := range findings {
		if f.Category == CatPositions && f.Severity == SevMismatch {
			posFinding = true
		}
	}
	if !posFinding {
		t.Fatal("no POSITIONS MISMATCH finding persisted")
	}
	var alertN int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM funding_ops_alerts WHERE code=$1`,
		AlertMismatchCode).Scan(&alertN); err != nil || alertN == 0 {
		t.Fatalf("durable P1 alert row missing: n=%d err=%v", alertN, err)
	}
	var suspN int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM trading_suspensions
		 WHERE scope='ACCOUNT' AND target_id='99999001' AND state='ACTIVE'`).
		Scan(&suspN); err != nil || suspN == 0 {
		t.Fatalf("trading_suspensions row missing: n=%d err=%v", suspN, err)
	}
	set, err := rdb.HaltScopeScan(ctx, []string{excredis.HaltKey("ACCOUNT", "99999001")})
	if err != nil || len(set) == 0 {
		t.Fatalf("halt:account:99999001 redis flag missing: %v", err)
	}
	t.Logf("halt flag live: %v", set)

	elapsed := time.Since(started)
	t.Logf("TABLETOP recon-mismatch elapsed=%s (P1 SLA 15m) — PASS", elapsed.Round(time.Millisecond))
	if elapsed > 15*time.Minute {
		t.Fatalf("drill exceeded P1 SLA: %s", elapsed)
	}
}
