// PostgreSQL + Redis integration tests for the file-backed sanctions
// screener — Phase-13.5 Task 13.5.3.3. Convention mirrors
// internal/funding/integration_test.go:
//
//	EXC_PG_TEST=1            enable
//	EXC_TEST_DSN             postgres DSN (default postgres on 127.0.0.1:55433)
//	EXC_REDIS_TEST_ADDR      redis addr (default 127.0.0.1:6379)
//	EXC_REDIS_TEST_PASSWORD  redis password (default redpass)
//
// Each run builds a scratch schema, applies the real migration files,
// wires the real funding.DepositService / funding.WithdrawalService over
// settlement.LedgerService, and screens through the real ListScreener
// loaded from the shipped dev fixture — a listed counterparty must park
// both directions in PENDING_REVIEW with the SANCTIONS_HIT flag.
package compliance

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
	"exchange/internal/funding"
	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// fixtureDDL mirrors internal/funding/integration_test.go — the minimal
// upstream tables the ledger/funding migrations and services need.
const fixtureDDL = `
CREATE TABLE users (
    id    BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL DEFAULT ''
);
CREATE TABLE accounts (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users(id),
    parent_account_id BIGINT REFERENCES accounts(id),
    kyc_tier          VARCHAR(4)  NOT NULL DEFAULT 'T2',
    status            VARCHAR(12) NOT NULL DEFAULT 'ACTIVE',
    base_currency     VARCHAR(3)  NOT NULL DEFAULT 'USD',
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
CREATE TABLE admin_audit_log (
    id            BIGSERIAL PRIMARY KEY,
    admin_user_id BIGINT NOT NULL,
    action        VARCHAR(64) NOT NULL,
    target_type   VARCHAR(32) NOT NULL,
    target_id     BIGINT NOT NULL,
    before_state  JSONB,
    after_state   JSONB,
    ip_address    INET,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE instruments (id BIGSERIAL PRIMARY KEY);
CREATE TABLE trades (
    id                BIGSERIAL PRIMARY KEY,
    instrument_id     BIGINT NOT NULL,
    buyer_account_id  BIGINT NOT NULL,
    seller_account_id BIGINT NOT NULL,
    price             NUMERIC(20,8) NOT NULL,
    quantity          NUMERIC(28,8) NOT NULL,
    buyer_fee         NUMERIC(20,8) NOT NULL DEFAULT 0,
    seller_fee        NUMERIC(20,8) NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE positions (
    id               BIGSERIAL PRIMARY KEY,
    account_id       BIGINT NOT NULL,
    instrument_id    BIGINT NOT NULL,
    side             VARCHAR(8) NOT NULL,
    quantity         NUMERIC(28,8) NOT NULL,
    avg_entry_price  NUMERIC(20,8) NOT NULL,
    unrealized_pnl   NUMERIC(28,8) NOT NULL DEFAULT 0,
    realized_pnl     NUMERIC(28,8) NOT NULL DEFAULT 0,
    opened_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func sanPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@127.0.0.1:55433/postgres?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	schema := fmt.Sprintf("san_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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

func sanExecMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("exec %s: %v", name, err)
	}
}

// sanSchema mirrors itSchema + itFlowSchema — the funding/ledger
// migration chain the deposit+withdrawal services touch.
func sanSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, fixtureDDL); err != nil {
		t.Fatalf("fixture ddl: %v", err)
	}
	for _, m := range []string{
		"036_create_general_ledger.up.sql",
		"088_gl_chart_of_accounts.up.sql",
		"102_ledger_wallet_shadow.up.sql",
		"007_create_funding_transactions.up.sql",
		"008_create_withdrawal_confirmations.up.sql",
		"018_create_nostro_accounts.up.sql",
		"160_funding_extensions.up.sql",
		"161_internal_transfers.up.sql",
		"162_chargebacks.up.sql",
		"108_suspense_accounts_routing.up.sql",
		"040_bank_accounts.up.sql",
		"078_withdrawal_whitelist_settings.up.sql",
		"199_funding_flow_extensions.up.sql",
	} {
		sanExecMigration(t, ctx, pool, m)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email) VALUES (100,'u100@x');
		INSERT INTO accounts (id, user_id, status) VALUES (7,100,'ACTIVE');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func sanLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *settlement.LedgerService {
	t.Helper()
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
		t.Skipf("redis unavailable at %s: %v", addr, err)
	}
	svc, err := settlement.NewLedgerService(pool, rdb, nil)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	return svc
}

// usdIdentity prices any currency 1:1 with USD for tier tests.
type usdIdentity struct{}

func (usdIdentity) ToUSD(_ context.Context, _ string,
	amount decimal.Decimal) (decimal.Decimal, error) {
	return amount, nil
}

func devListScreener(t *testing.T) *ListScreener {
	t.Helper()
	s, err := NewListScreener(filepath.Join("..", "..", "..",
		"deploy", "security", "sanctions-dev"))
	if err != nil {
		t.Fatalf("dev fixture: %v", err)
	}
	return s
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// A STANDARD-tier inbound deposit from a dev-listed originator parks in
// PENDING_REVIEW with the SANCTIONS_HIT flag — never credits.
func TestITSanctionsDepositBlocks(t *testing.T) {
	ctx, pool := sanPool(t)
	sanSchema(t, ctx, pool)
	led := sanLedger(t, ctx, pool)
	store := funding.NewPgStore(pool)
	freeze := accounts.NewFreezeService(pool, func(_ context.Context, _ int64) (string, error) {
		return accounts.RoleComplianceOfficer, nil
	})
	svc, err := funding.NewDepositService(store, led, freeze)
	if err != nil {
		t.Fatalf("deposit svc: %v", err)
	}
	svc.WithUSDConverter(usdIdentity{}).WithSanctions(devListScreener(t))

	res, err := svc.IngestDetected(ctx, funding.IngestDepositRequest{
		AccountID: 7, Currency: "USD", Amount: "20000",
		Reference: "BNK-SAN-1", Source: "CAMT054",
		OriginatorName:    "Suspicious Sender",
		OriginatorAccount: "Suspicious Sender",
		ReceivedBy:        100,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	out, err := svc.Confirm(ctx, funding.DepositConfirmRequest{
		DepositID: res.DepositID, Source: "STATEMENT",
		SenderName: "Suspicious Sender", ReceivedBy: 100,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != funding.FundingPendingReview {
		t.Fatalf("listed originator must park in PENDING_REVIEW, got %s", out.Status)
	}
	if !hasFlag(out.Flags, "SANCTIONS_HIT") {
		t.Fatalf("expected SANCTIONS_HIT flag, got %v", out.Flags)
	}
	// The row must remain uncredited in PG — PENDING_REVIEW persisted.
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM funding_transactions WHERE id=$1`, res.DepositID).Scan(&status); err != nil {
		t.Fatalf("row read: %v", err)
	}
	if status != funding.FundingPendingReview {
		t.Fatalf("persisted status %s, want PENDING_REVIEW", status)
	}
}

// A withdrawal whose registered beneficiary legal name hits the dev
// list parks in PENDING_REVIEW with SANCTIONS_HIT at confirm — the
// destination never reaches dispatch.
func TestITSanctionsWithdrawalBlocks(t *testing.T) {
	ctx, pool := sanPool(t)
	sanSchema(t, ctx, pool)
	led := sanLedger(t, ctx, pool)
	store := funding.NewPgStore(pool)
	freeze := accounts.NewFreezeService(pool, func(_ context.Context, _ int64) (string, error) {
		return accounts.RoleComplianceOfficer, nil
	})
	svc, err := funding.NewWithdrawalService(store, led, freeze)
	if err != nil {
		t.Fatalf("withdrawal svc: %v", err)
	}
	svc.WithUSDConverter(usdIdentity{}).
		WithSanctions(devListScreener(t)).
		WithBeneficiaryResolver(store)

	// Fund the account through the real DEPOSIT journal.
	amt := decimal.MustFromString("10000")
	res0, err := led.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryDeposit,
		ReferenceID:    900007,
		Description:    "seed deposit 10000 USD → acct 7",
		PostedBy:       "test:fixture",
		IdempotencyKey: "seed-deposit:7:USD",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro("USD"), "USD", amt, "nostro credit"),
			ledger.CreditLine(ledger.CustomerLiability("USD"), "USD", amt, "client deposit"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID: 7, Currency: "USD", AvailableDelta: amt,
		}},
	})
	if !res0.Committed {
		t.Fatalf("seed deposit not committed: %v", err)
	}

	// Register a VERIFIED beneficiary whose legal name is dev-listed.
	verified := time.Now().UTC().Add(-25 * time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO bank_accounts (account_id, currency, account_number,
		    beneficiary_name, bank_name, rail, status, verified_at,
		    verified_by, unlocked_at)
		VALUES (7,'USD','IBAN-SAN-BEN','Blocked Beneficiary Trading',
		    'Test Bank','SWIFT','VERIFIED',$1,100,$1)`, verified); err != nil {
		t.Fatalf("beneficiary seed: %v", err)
	}

	res, err := svc.Create(ctx, funding.CreateWithdrawalRequest{
		AccountID: 7, UserID: 100, Currency: "USD", Amount: "500",
		ReferenceAccount: "IBAN-SAN-BEN", IdempotencyKey: "san-it-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := svc.Confirm(ctx, funding.ConfirmWithdrawalRequest{
		WithdrawalID: res.WithdrawalID, AccountID: 7, UserID: 100,
		Token: res.ConfirmToken,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != funding.FundingPendingReview {
		t.Fatalf("listed beneficiary must park in PENDING_REVIEW, got %s", out.Status)
	}
	if !hasFlag(out.Flags, "SANCTIONS_HIT") {
		t.Fatalf("expected SANCTIONS_HIT flag, got %v", out.Flags)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM funding_transactions WHERE id=$1`, res.WithdrawalID).Scan(&status); err != nil {
		t.Fatalf("row read: %v", err)
	}
	if status != funding.FundingPendingReview {
		t.Fatalf("persisted status %s, want PENDING_REVIEW", status)
	}
}

// Counterpart: a clean counterparty passes both screens — the dev
// fixture does not blanket-match.
func TestITSanctionsCleanCounterpartyPasses(t *testing.T) {
	ctx, pool := sanPool(t)
	sanSchema(t, ctx, pool)
	led := sanLedger(t, ctx, pool)
	store := funding.NewPgStore(pool)
	freeze := accounts.NewFreezeService(pool, func(_ context.Context, _ int64) (string, error) {
		return accounts.RoleComplianceOfficer, nil
	})
	svc, err := funding.NewWithdrawalService(store, led, freeze)
	if err != nil {
		t.Fatalf("withdrawal svc: %v", err)
	}
	svc.WithUSDConverter(usdIdentity{}).
		WithSanctions(devListScreener(t)).
		WithBeneficiaryResolver(store)

	amt := decimal.MustFromString("10000")
	res0, err := led.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryDeposit,
		ReferenceID:    900008,
		Description:    "seed deposit 10000 USD → acct 7 (clean run)",
		PostedBy:       "test:fixture",
		IdempotencyKey: "seed-deposit:7:USD:clean",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro("USD"), "USD", amt, "nostro credit"),
			ledger.CreditLine(ledger.CustomerLiability("USD"), "USD", amt, "client deposit"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID: 7, Currency: "USD", AvailableDelta: amt,
		}},
	})
	if !res0.Committed {
		t.Fatalf("seed deposit not committed: %v", err)
	}

	res, err := svc.Create(ctx, funding.CreateWithdrawalRequest{
		AccountID: 7, UserID: 100, Currency: "USD", Amount: "500",
		ReferenceAccount: "IBAN-CLEAN-1", IdempotencyKey: "san-it-clean",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := svc.Confirm(ctx, funding.ConfirmWithdrawalRequest{
		WithdrawalID: res.WithdrawalID, AccountID: 7, UserID: 100,
		Token: res.ConfirmToken,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != funding.FundingConfirmed {
		t.Fatalf("clean counterparty must confirm, got %s flags %v", out.Status, out.Flags)
	}
}
