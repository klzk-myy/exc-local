// PG/Redis-gated integration coverage for Task 14.3.14 — runs only when
// EXC_PG_TEST=1; Redis locks ride itLedger's EXC_REDIS_TEST_* envs.
//
//	EXC_PG_TEST=1 EXC_TEST_DSN='postgres://...' EXC_REDIS_TEST_ADDR=127.0.0.1:16379 \
//	    go test ./internal/copy/ -run TestIT -v
package copy

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/accounts"
	"exchange/internal/ledger"
	"exchange/internal/pamm"
	excredis "exchange/internal/redis"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

func itPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	schema := fmt.Sprintf("copy_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
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
	return ctx, pool
}

func execSQLFile(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("exec %s: %v", name, err)
	}
}

const itFixture = `
CREATE TABLE users (
    id    BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL DEFAULT ''
);
CREATE TABLE accounts (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users(id),
    account_type      VARCHAR(16)  NOT NULL DEFAULT 'SPOT',
    kyc_tier          VARCHAR(4)   NOT NULL DEFAULT 'T2',
    status            VARCHAR(12)  NOT NULL DEFAULT 'ACTIVE',
    parent_account_id BIGINT REFERENCES accounts(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE balances (
    account_id BIGINT     NOT NULL REFERENCES accounts(id),
    currency   VARCHAR(3) NOT NULL,
    available  NUMERIC(38,10) NOT NULL DEFAULT 0,
    locked     NUMERIC(38,10) NOT NULL DEFAULT 0,
    total      NUMERIC(38,10) GENERATED ALWAYS AS (available + locked) STORED,
    version    BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, currency)
);
CREATE TABLE account_freeze_events (
    id           BIGSERIAL PRIMARY KEY,
    account_id   BIGINT NOT NULL REFERENCES accounts(id),
    action       VARCHAR(16) NOT NULL,
    reason       TEXT NOT NULL,
    initiated_by BIGINT NOT NULL DEFAULT 0,
    approved_by  BIGINT NOT NULL DEFAULT 0,
    prev_status  VARCHAR(12) NOT NULL DEFAULT 'ACTIVE',
    new_status   VARCHAR(12) NOT NULL DEFAULT 'ACTIVE',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE instruments (
    id                BIGSERIAL PRIMARY KEY,
    symbol            VARCHAR(20) NOT NULL,
    base_currency     VARCHAR(3)  NOT NULL,
    quote_currency    VARCHAR(3)  NOT NULL,
    min_order_qty     NUMERIC(20,8) NOT NULL DEFAULT 1000
);
CREATE TABLE trades (
    id                BIGSERIAL PRIMARY KEY,
    instrument_id     BIGINT NOT NULL REFERENCES instruments(id),
    buyer_account_id  BIGINT NOT NULL,
    seller_account_id BIGINT NOT NULL,
    price             NUMERIC(20,8) NOT NULL,
    quantity          NUMERIC(28,8) NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func itSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, itFixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, m := range []string{
		"036_create_general_ledger.up.sql",
		"088_gl_chart_of_accounts.up.sql",
		"102_ledger_wallet_shadow.up.sql",
		"007_create_funding_transactions.up.sql",
		"009_create_audit_hash_chain.up.sql",
		"010_create_admin_audit_log.up.sql",
		"097_copy_trading_product.up.sql",
		"216_pamm_engine.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id,email) VALUES (1,'mgr@x'),(2,'inv@x'),(9,'co@x');
		INSERT INTO accounts (id,user_id,status) VALUES (10,1,'ACTIVE'),(42,2,'ACTIVE');
		INSERT INTO instruments (id,symbol,base_currency,quote_currency,min_order_qty)
		  VALUES (1,'EUR/USD','EUR','USD',1000);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func itLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *settlement.LedgerService {
	t.Helper()
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	rdb := excredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unavailable at %s: %v", addr, err)
	}
	svc, err := settlement.NewLedgerService(pool, rdb, nil)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	return svc
}

func itSeedDeposit(t *testing.T, ctx context.Context, led *settlement.LedgerService,
	acct int64, ccy, amount string) {
	t.Helper()
	amt := decimal.RequireFromString(amount)
	res, err := led.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryDeposit,
		ReferenceID:    910000 + acct,
		Description:    "seed deposit",
		PostedBy:       "test",
		IdempotencyKey: fmt.Sprintf("seed:%d:%s:%s", acct, ccy, amount),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro(ccy), ccy, amt, "nostro"),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amt, "liability"),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: acct, Currency: ccy, AvailableDelta: amt},
		},
	})
	if !res.Committed {
		t.Fatalf("seed deposit acct %d: %v %+v", acct, err, res)
	}
}

type passApprov struct{}

func (passApprov) Appropriateness(context.Context, int64, string) error { return nil }

func mustPammStore(t *testing.T, pool *pgxpool.Pool) *pamm.PgxStore {
	t.Helper()
	s, err := pamm.NewPgxStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type sameCcyRates struct{}

func (sameCcyRates) IndexRate(_ context.Context, base, quote string) (decimal.Decimal, error) {
	if base == quote {
		return decimal.One, nil
	}
	return decimal.Zero, errorf(CodeServiceDegraded, "no rate")
}

func bal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acct int64, ccy string) decimal.Decimal {
	t.Helper()
	var a string
	if err := pool.QueryRow(ctx,
		`SELECT available::text FROM balances WHERE account_id=$1 AND currency=$2`,
		acct, ccy).Scan(&a); err != nil {
		return decimal.Zero
	}
	return decimal.RequireFromString(a)
}

// IT: full lifecycle — create → age → list (gates) → follow → settle
// profit share with real balanced GL + subledger taxonomy → suspend
// (audit-chained) → new follow blocked, existing follow untouched.
func TestITCopyLifecycle(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 42, "USD", "5000")

	store, err := NewPgxStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	auditor, err := NewAdminAuditor(pool)
	if err != nil {
		t.Fatal(err)
	}
	freeze := accounts.NewFreezeService(pool, func(_ context.Context, id int64) (string, error) {
		return accounts.RoleComplianceOfficer, nil
	})
	subW, err := NewSubledgerWriter(mustPammStore(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, passApprov{}, freeze, sameCcyRates{}, auditor,
		WithJournalPoster(led), WithSubledgerWriter(subW))
	if err != nil {
		t.Fatal(err)
	}

	st, err := svc.CreateStrategy(ctx, CreateStrategyInput{
		ManagerAccountID: 10, DisplayName: "alpha", Currency: "USD",
		InstrumentClass: "FORWARD", ProfitSharePct: "20"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusIncubating {
		t.Fatalf("status %s", st.Status)
	}
	// Listing too young → gated.
	if _, err := svc.List(ctx, st.StrategyID, 10); err == nil {
		t.Fatal("listed before 30d")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE strategy_profiles SET incubating_since = now() - interval '31 days'
		 WHERE strategy_id=$1`, st.StrategyID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.List(ctx, st.StrategyID, 10); err != nil {
		t.Fatal(err)
	}

	f, err := svc.Follow(ctx, FollowInput{
		InvestorAccountID: 42, StrategyID: st.StrategyID,
		AllocationNotional: "5000", SafetyMode: "HALF_RISK"})
	if err != nil {
		t.Fatal(err)
	}

	// Profit share: pnl 1000, pct 20 → accrued 200 investor→manager.
	end := time.Now().UTC()
	res, err := svc.SettleProfitShare(ctx, f.FollowID, end.AddDate(0, -1, 0), end,
		decimal.RequireFromString("1000"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accrued.Equal(d("200")) || !res.Balanced {
		t.Fatalf("settle %+v", res)
	}
	if got := bal(t, ctx, pool, 42, "USD"); !got.Equal(d("4800")) {
		t.Fatalf("investor %s", got)
	}
	if got := bal(t, ctx, pool, 10, "USD"); !got.Equal(d("200")) {
		t.Fatalf("manager %s", got)
	}
	// HWM ratcheted + subledger carries PAMM_FEE_PERF with copy_follow_id.
	var wm string
	if err := pool.QueryRow(ctx,
		`SELECT watermark_pnl::text FROM high_water_marks WHERE follow_id=$1`,
		f.FollowID).Scan(&wm); err != nil {
		t.Fatal(err)
	}
	if wm != "1000.00000000" {
		t.Fatalf("hwm %s", wm)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pamm_subledger_entries
		 WHERE copy_follow_id=$1 AND txn_type='PAMM_FEE_PERF'`, f.FollowID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("fee subledger rows %d", n)
	}
	// Loss month: no journal, HWM holds.
	res2, err := svc.SettleProfitShare(ctx, f.FollowID, end, end.AddDate(0, 1, 0),
		decimal.RequireFromString("-500"))
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Accrued.IsZero() {
		t.Fatalf("accrued %s on loss", res2.Accrued)
	}
	if err := pool.QueryRow(ctx,
		`SELECT watermark_pnl::text FROM high_water_marks WHERE follow_id=$1`,
		f.FollowID).Scan(&wm); err != nil {
		t.Fatal(err)
	}
	if wm != "1000.00000000" {
		t.Fatalf("hwm reset on loss: %s", wm)
	}
	if got := bal(t, ctx, pool, 42, "USD"); !got.Equal(d("4800")) {
		t.Fatalf("investor %s after loss-month settle", got)
	}

	// Suspension: audit-logged, blocks new follows, existing intact.
	if _, err := svc.Suspend(ctx, st.StrategyID, 9, "127.0.0.1", "stat manipulation attempt"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE action='copy.strategy.suspend'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	if _, err := svc.Follow(ctx, FollowInput{
		InvestorAccountID: 43, StrategyID: st.StrategyID,
		AllocationNotional: "1000"}); err == nil {
		t.Fatal("follow on SUSPENDED accepted")
	}
	got, _ := store.FollowByID(ctx, f.FollowID)
	if got.Status != FollowActive {
		t.Fatalf("existing follow %s", got.Status)
	}
}

// IT: fan-out over real tables — pro-rata + HALF_RISK scaling + durable
// child intents (order submitter unbound → children stay PENDING).
func TestITCopyFanout(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 42, "USD", "5000")

	store, _ := NewPgxStore(pool)
	auditor, _ := NewAdminAuditor(pool)
	freeze := accounts.NewFreezeService(pool, func(_ context.Context, id int64) (string, error) {
		return accounts.RoleComplianceOfficer, nil
	})
	svc, _ := NewService(store, passApprov{}, freeze, sameCcyRates{}, auditor)
	st, _ := svc.CreateStrategy(ctx, CreateStrategyInput{
		ManagerAccountID: 10, DisplayName: "a", Currency: "USD"})
	pool.Exec(ctx, `UPDATE strategy_profiles SET incubating_since=now()-interval '31 days'
		WHERE strategy_id=$1`, st.StrategyID)
	svc.List(ctx, st.StrategyID, 10)
	svc.Follow(ctx, FollowInput{InvestorAccountID: 42, StrategyID: st.StrategyID,
		AllocationNotional: "5000", SafetyMode: "HALF_RISK"})

	eng, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.OnManagerFill(ctx, EngineFill{
		StrategyID: st.StrategyID, ManagerAcctID: 10, TradeID: 7001,
		InstrumentID: 1, Side: "BUY", Quantity: "4000", Price: "1.10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Children) != 1 || !res.Children[0].Quantity.Equal(d("2000")) {
		t.Fatalf("children %+v", res.Children)
	}
	// HALF_RISK below min: 1000 × 0.5 = 500 < min 1000 → durable skip.
	res, err = eng.OnManagerFill(ctx, EngineFill{
		StrategyID: st.StrategyID, ManagerAcctID: 10, TradeID: 7002,
		InstrumentID: 1, Side: "BUY", Quantity: "1000", Price: "1.10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped %+v", res.Skipped)
	}
	var status, notice string
	if err := pool.QueryRow(ctx, `
		SELECT status::text, notice FROM copy_child_orders
		 WHERE master_trade_id=7002`).Scan(&status, &notice); err != nil {
		t.Fatal(err)
	}
	if status != "SKIPPED_MIN_NOTIONAL" || notice == "" {
		t.Fatalf("skip row %s %q", status, notice)
	}
}
