// PG/Redis-gated integration coverage for Task 14.3.8 — runs only when
// EXC_PG_TEST=1; Redis locks ride itLedger's EXC_REDIS_TEST_* envs.
//
//	EXC_PG_TEST=1 EXC_TEST_DSN='postgres://...' EXC_REDIS_TEST_ADDR=127.0.0.1:16379 \
//	    go test ./internal/pamm/ -run TestIT -v
package pamm

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/accounts"
	"exchange/internal/ledger"
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
	schema := fmt.Sprintf("pamm_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

// fixtureDDL is the minimal upstream surface the 097/216 migrations and
// the services depend on — VARCHAR stand-ins for enum columns (the code
// casts ::text), the real migrations supply their own enums/tables.
const fixtureDDL = `
CREATE TABLE users (
    id    BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL DEFAULT ''
);
CREATE TABLE fee_tiers (
    id         BIGSERIAL PRIMARY KEY,
    tier_name  VARCHAR(32) NOT NULL UNIQUE,
    maker_bps  NUMERIC(10,4) NOT NULL DEFAULT 0,
    taker_bps  NUMERIC(10,4) NOT NULL DEFAULT 0
);
INSERT INTO fee_tiers (tier_name, maker_bps, taker_bps) VALUES ('STANDARD', 0, 0);
CREATE TABLE accounts (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users(id),
    account_type      VARCHAR(16)  NOT NULL DEFAULT 'SPOT',
    kyc_tier          VARCHAR(4)   NOT NULL DEFAULT 'T2',
    status            VARCHAR(12)  NOT NULL DEFAULT 'ACTIVE',
    parent_account_id BIGINT REFERENCES accounts(id),
    fee_tier_id       BIGINT REFERENCES fee_tiers(id),
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
    initiated_by BIGINT NOT NULL,
    approved_by  BIGINT NOT NULL,
    prev_status  VARCHAR(12) NOT NULL,
    new_status   VARCHAR(12) NOT NULL,
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
	if _, err := pool.Exec(ctx, fixtureDDL); err != nil {
		t.Fatalf("fixture ddl: %v", err)
	}
	for _, m := range []string{
		"036_create_general_ledger.up.sql",
		"088_gl_chart_of_accounts.up.sql",
		"102_ledger_wallet_shadow.up.sql",
		"007_create_funding_transactions.up.sql", // trigger target table
		"097_copy_trading_product.up.sql",
		"216_pamm_engine.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email) VALUES (1,'mgr@x'),(2,'inv@x'),(3,'inv2@x');
		INSERT INTO accounts (id, user_id, status) VALUES
		  (10,1,'ACTIVE'),(42,2,'ACTIVE'),(43,3,'ACTIVE');
		INSERT INTO instruments (id, symbol, base_currency, quote_currency, min_order_qty)
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
	pass := os.Getenv("EXC_REDIS_TEST_PASSWORD")
	rdb := excredis.New(addr, pass, 13)
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
		ReferenceID:    900000 + acct,
		Description:    fmt.Sprintf("seed deposit %s %s → acct %d", amount, ccy, acct),
		PostedBy:       "test",
		IdempotencyKey: fmt.Sprintf("seed:%d:%s:%s", acct, ccy, amount),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro(ccy), ccy, amt, "seed nostro"),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amt, "seed liability"),
		},
		Effects: []ledger.AccountEffect{
			{AccountID: acct, Currency: ccy, AvailableDelta: amt},
		},
	})
	// A nil publisher returns BALANCE_EVENT_DISPATCH_FAILED with
	// Committed=true — the journal is durable; only event fan-out is
	// absent in this fixture.
	if !res.Committed {
		t.Fatalf("seed deposit acct %d not committed: %v %+v", acct, err, res)
	}
}

func bal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acct int64, ccy string) decimal.Decimal {
	t.Helper()
	var a string
	err := pool.QueryRow(ctx,
		`SELECT available::text FROM balances WHERE account_id=$1 AND currency=$2`,
		acct, ccy).Scan(&a)
	if err == pgx.ErrNoRows {
		return decimal.Zero
	}
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return decimal.RequireFromString(a)
}

// IT: invest moves real wallet balances investor→pool via TRANSFER,
// writes PAMM_INVEST sub-ledger rows, and the pool account is barred
// from fiat funding rails by the DB trigger (fiat-cap isolation).
func TestITInvestRedeemAndFiatGuard(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 42, "USD", "1000")

	store, err := NewPgxStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	freeze := accounts.NewFreezeService(pool, func(_ context.Context, id int64) (string, error) {
		return accounts.RoleComplianceOfficer, nil
	})
	svc, err := NewService(store, led, freeze)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreatePool(ctx, 10, "alpha", "USD", "100")
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Invest(ctx, MovementRequest{
		PoolID: p.PoolID, InvestorAccountID: 42, Amount: "400", IdempotencyKey: "it-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Invested != "400" {
		t.Fatalf("invested %s", res.Invested)
	}
	// Wallets actually moved.
	if got := bal(t, ctx, pool, 42, "USD"); !got.Equal(decimal.RequireFromString("600")) {
		t.Fatalf("investor balance %s", got)
	}
	if got := bal(t, ctx, pool, p.PoolAccountID, "USD"); !got.Equal(decimal.RequireFromString("400")) {
		t.Fatalf("pool balance %s", got)
	}
	// Sub-ledger taxonomy is PAMM_INVEST — never a funding row.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pamm_subledger_entries WHERE txn_type='PAMM_INVEST'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("subledger rows %d", n)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM funding_transactions WHERE account_id=$1`, p.PoolAccountID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("pool touched funding_transactions")
	}
	// The structural guard rejects fiat-rail usage outright.
	_, err = pool.Exec(ctx, `
		INSERT INTO funding_transactions (account_id, type, amount)
		VALUES ($1, 'WITHDRAWAL', 1)`, p.PoolAccountID)
	if err == nil {
		t.Fatal("fiat rail accepted on a pool account — cap isolation broken")
	}
	// Redeem returns capital; invested drops.
	rres, err := svc.Redeem(ctx, MovementRequest{
		PoolID: p.PoolID, InvestorAccountID: 42, Amount: "150"})
	if err != nil {
		t.Fatal(err)
	}
	if rres.Invested != "250" {
		t.Fatalf("invested %s", rres.Invested)
	}
	if got := bal(t, ctx, pool, 42, "USD"); !got.Equal(decimal.RequireFromString("750")) {
		t.Fatalf("investor balance %s", got)
	}
}

// IT: pro-rata fill fan-out lands durable allocation rows; a replayed
// master trade dedups via UNIQUE(master_trade_id, allocation_id).
func TestITFillFanout(t *testing.T) {
	ctx, pool := itPool(t)
	itSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 42, "USD", "1000")
	itSeedDeposit(t, ctx, led, 43, "USD", "1000")
	store, _ := NewPgxStore(pool)
	freeze := accounts.NewFreezeService(pool, func(_ context.Context, id int64) (string, error) {
		return accounts.RoleComplianceOfficer, nil
	})
	svc, _ := NewService(store, led, freeze)
	p, _ := svc.CreatePool(ctx, 10, "a", "USD", "0")
	svc.Invest(ctx, MovementRequest{PoolID: p.PoolID, InvestorAccountID: 42, Amount: "300"})
	svc.Invest(ctx, MovementRequest{PoolID: p.PoolID, InvestorAccountID: 43, Amount: "100"})

	eng, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	fill := MasterFill{MasterAccountID: p.PoolAccountID, TradeID: 5001,
		InstrumentID: 1, Side: "BUY", Quantity: "8000", Price: "1.10"}
	res, err := eng.OnPoolFill(ctx, fill)
	if err != nil {
		t.Fatal(err)
	}
	if res.Allocations != 2 {
		t.Fatalf("allocations %d", res.Allocations)
	}
	// 300:100 → 6000:2000.
	var q42, q43 string
	rows, _ := pool.Query(ctx, `
		SELECT investor_account_id, quantity::text FROM pamm_fill_allocations
		 WHERE master_trade_id = 5001 ORDER BY investor_account_id`)
	defer rows.Close()
	for rows.Next() {
		var acct int64
		var q string
		rows.Scan(&acct, &q)
		if acct == 42 {
			q42 = q
		} else {
			q43 = q
		}
	}
	if q42 != "6000.00000000" || q43 != "2000.00000000" {
		t.Fatalf("pro-rata %s / %s", q42, q43)
	}
	// Replay dedups.
	res, err = eng.OnPoolFill(ctx, fill)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate || res.Allocations != 0 {
		t.Fatalf("replay: %+v", res)
	}
}
