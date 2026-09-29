// Integration tests for the funding package against real PostgreSQL + Redis
// and the real settlement.LedgerService (SERIALIZABLE posting, wallet
// effects, GL lines). Gated:
//
//	EXC_PG_TEST=1            enable
//	EXC_TEST_DSN             postgres DSN (uses EXC_TEST_DSN convention)
//	EXC_REDIS_TEST_ADDR      redis addr (default 127.0.0.1:6379)
//	EXC_REDIS_TEST_PASSWORD  redis password (default redpass)
//
// The scratch schema is created per run, seeded with a minimal fixture +
// the real funding/ledger migrations (007,008,018,036,088,102,108,160,161,162).
package funding

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
	excredis "exchange/internal/redis"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
)

// TestITMigrationRoundTrip applies 160/161/162 up then down in a
// dedicated scratch schema — both directions must execute cleanly.
// (Up migrations also run inside itSchema for the lifecycle tests.)
func TestITMigrationRoundTrip(t *testing.T) {
	ctx, pool, _ := itPool(t)
	// Up migrations have FK dependencies — create minimal anchors.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE users (id BIGSERIAL PRIMARY KEY);
		CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY, user_id BIGINT,
		    status VARCHAR(12) DEFAULT 'ACTIVE');
		CREATE TABLE journal_entries (id BIGSERIAL PRIMARY KEY);
		CREATE TABLE funding_transactions (
		    id BIGSERIAL PRIMARY KEY, account_id BIGINT REFERENCES accounts(id),
		    type VARCHAR(16), status VARCHAR(16), currency VARCHAR(3),
		    amount NUMERIC(28,8), created_at TIMESTAMPTZ DEFAULT now());
		CREATE TABLE withdrawal_confirmations (
		    id BIGSERIAL PRIMARY KEY, withdrawal_id BIGINT);`); err != nil {
		t.Fatalf("anchor ddl: %v", err)
	}
	for _, m := range []string{
		"108_suspense_accounts_routing.up.sql",
		"160_funding_extensions.up.sql",
		"161_internal_transfers.up.sql",
		"162_chargebacks.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
	for _, m := range []string{
		"162_chargebacks.down.sql",
		"161_internal_transfers.down.sql",
		"160_funding_extensions.down.sql",
		"108_suspense_accounts_routing.down.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
	// Post-down: the added columns/tables are gone. The information_schema
	// probes must be scoped to the scratch schema (search_path) — on a
	// fully-migrated database the real public tables legitimately carry
	// these columns and would false-positive the round-trip check.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name='funding_transactions' AND column_name='review_tier'`).Scan(&n); err != nil {
		t.Fatalf("post-down check: %v", err)
	}
	if n != 0 {
		t.Fatal("review_tier survived the down migration")
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = current_schema()
		   AND table_name IN ('transfers','chargebacks','chargeback_evidence')`).Scan(&n); err != nil {
		t.Fatalf("post-down tables: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d Cluster B tables survived the down migrations", n)
	}
	// Migration 108 (Phase-11 suspense + rail payments) round-trip.
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = current_schema()
		   AND table_name IN ('suspense_account_mappings','rail_payments')`).Scan(&n); err != nil {
		t.Fatalf("post-down 108 tables: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d migration-108 tables survived the down migration", n)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_type t JOIN pg_namespace ns ON ns.oid = t.typnamespace
		 WHERE ns.nspname = current_schema()
		   AND t.typname IN ('unmatched_reason_enum','quarantine_status_enum','rail_payment_status_enum')`).Scan(&n); err != nil {
		t.Fatalf("post-down 108 enums: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d migration-108 enum types survived the down migration", n)
	}
}

func itPool(t *testing.T) (context.Context, *pgxpool.Pool, string) {
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
	schema := fmt.Sprintf("w2b_it_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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
	return ctx, pool, schema
}

// execSQLFile applies a migration file verbatim.
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

// fixtureDDL creates the minimal upstream tables the funding/ledger
// migrations and services depend on (users/accounts/balances +
// freeze/audit tables). Enums the real migrations own are applied via the
// real migration files; the fixture keeps its own VARCHAR columns.
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
CREATE TABLE instruments (
    id BIGSERIAL PRIMARY KEY
);
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

func itSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, fixtureDDL); err != nil {
		t.Fatalf("fixture ddl: %v", err)
	}
	for _, m := range []string{
		"036_create_general_ledger.up.sql", // chart_of_accounts + journal_entries + ledger_lines
		"088_gl_chart_of_accounts.up.sql",  // nostro/liability/clearing accounts
		"102_ledger_wallet_shadow.up.sql",  // ledger_entries + journal_sums + balance shadow
		"007_create_funding_transactions.up.sql",
		"008_create_withdrawal_confirmations.up.sql",
		"018_create_nostro_accounts.up.sql",
		"160_funding_extensions.up.sql",
		"161_internal_transfers.up.sql",
		"162_chargebacks.up.sql",
		"108_suspense_accounts_routing.up.sql", // Phase-11 suspense + rail payments
	} {
		execSQLFile(t, ctx, pool, m)
	}
	// Seed users/accounts. Balances are NOT seeded directly — the
	// journal_sums↔wallet invariant requires every wallet total to equal
	// the cumulative ledger sum, so funds land via itSeedDeposit.
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email) VALUES (100,'u100@x'),(200,'u200@x'),(500,'admin@x'),(600,'approver@x');
		INSERT INTO accounts (id, user_id, status) VALUES
		  (1,100,'ACTIVE'),(2,100,'ACTIVE'),(3,200,'ACTIVE'),(4,100,'FROZEN');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// itSeedDeposit funds an account through the real DEPOSIT journal —
// nostro debit, customer-liability credit, wallet effect +amount.
func itSeedDeposit(t *testing.T, ctx context.Context, led *settlement.LedgerService,
	acct int64, ccy, amount string) {
	t.Helper()
	amt := decimal.MustFromString(amount)
	res, err := led.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryDeposit,
		ReferenceID:    900000 + acct,
		Description:    fmt.Sprintf("seed deposit %s %s → acct %d", amount, ccy, acct),
		PostedBy:       "test:fixture",
		IdempotencyKey: fmt.Sprintf("seed-deposit:%d:%s", acct, ccy),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro(ccy), ccy, amt, "nostro credit"),
			ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amt, "client deposit"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID: acct, Currency: ccy, AvailableDelta: amt,
		}},
	})
	// A nil publisher returns BALANCE_EVENT_DISPATCH_FAILED with
	// Committed=true — the journal is durable; only the event fan-out
	// is absent in this fixture.
	if !res.Committed {
		t.Fatalf("seed deposit acct %d not committed: %v %+v", acct, err, res)
	}
}

func itLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *settlement.LedgerService {
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

func itFreeze(t *testing.T, pool *pgxpool.Pool) *accounts.FreezeService {
	return accounts.NewFreezeService(pool, func(_ context.Context, adminID int64) (string, error) {
		return accounts.RoleComplianceOfficer, nil // test resolver grants the role
	})
}

func balRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acct int64, ccy string) (avail, locked decimal.Decimal) {
	t.Helper()
	var a, l string
	if err := pool.QueryRow(ctx,
		`SELECT available::text, locked::text FROM balances WHERE account_id=$1 AND currency=$2`,
		acct, ccy).Scan(&a, &l); err != nil {
		t.Fatalf("balance %d/%s: %v", acct, ccy, err)
	}
	return decimal.MustFromString(a), decimal.MustFromString(l)
}

// ---------------------------------------------------------------------------
// Withdrawal lifecycle — create, reserve, confirm, expiry sweep.
// ---------------------------------------------------------------------------

func TestITWithdrawalLifecycle(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "10000")
	freeze := itFreeze(t, pool)
	store := NewPgStore(pool)

	svc, err := NewWithdrawalService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("svc: %v", err)
	}

	res, err := svc.Create(ctx, CreateWithdrawalRequest{
		AccountID: 1, UserID: 100, Currency: "USD", Amount: "250.5",
		ReferenceAccount: "IBAN-DE001", IdempotencyKey: "wd-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Status != FundingPending {
		t.Fatalf("status %s", res.Status)
	}
	a, l := balRow(t, ctx, pool, 1, "USD")
	if !a.Equal(decimal.MustFromString("9749.5")) || !l.Equal(decimal.MustFromString("250.5")) {
		t.Fatalf("reservation: avail=%s locked=%s", a, l)
	}

	// Wrong token → INVALID_REQUEST (not silently consuming the window).
	_, err = svc.Confirm(ctx, ConfirmWithdrawalRequest{
		WithdrawalID: res.WithdrawalID, AccountID: 1, UserID: 100,
		Token: "deadbeefdeadbeef",
	})
	if err == nil {
		t.Fatal("wrong token accepted")
	}

	// Correct token confirms.
	res2, err := svc.Confirm(ctx, ConfirmWithdrawalRequest{
		WithdrawalID: res.WithdrawalID, AccountID: 1, UserID: 100, Token: res.ConfirmToken,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if res2.Status != FundingConfirmed && res2.Status != FundingPendingReview {
		t.Fatalf("confirm status %s", res2.Status)
	}
	// CONFIRMED keeps the earmark: funds stay locked until the payout rail
	// settles the COMPLETED transition (Phase-11 banking rails).
	a, l = balRow(t, ctx, pool, 1, "USD")
	if !a.Equal(decimal.MustFromString("9749.5")) || !l.Equal(decimal.MustFromString("250.5")) {
		t.Fatalf("post-confirm earmark: avail=%s locked=%s", a, l)
	}
}

// ledgerPosterAdapter adapts *settlement.LedgerService to the Poster seam.
type ledgerPosterAdapter struct{ l *settlement.LedgerService }

func (a ledgerPosterAdapter) Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	return a.l.Post(ctx, j)
}

func TestITWithdrawalExpirySweep(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "10000")
	freeze := itFreeze(t, pool)
	store := NewPgStore(pool)
	svc, err := NewWithdrawalService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	res, err := svc.Create(ctx, CreateWithdrawalRequest{
		AccountID: 1, UserID: 100, Currency: "USD", Amount: "100",
		ReferenceAccount: "IBAN-2", ConfirmMethod: "email",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force expiry: confirmation row already past the 15-minute window.
	if _, err := pool.Exec(ctx,
		`UPDATE withdrawal_confirmations SET expires_at = now() - interval '1 minute'
		 WHERE withdrawal_id = $1`, res.WithdrawalID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	n, err := svc.SweepExpired(ctx, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	var st string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM funding_transactions WHERE id=$1`, res.WithdrawalID).Scan(&st); err != nil {
		t.Fatalf("status read: %v", err)
	}
	if st != "AUTO_CANCELLED" {
		t.Fatalf("status %s", st)
	}
	a, l := balRow(t, ctx, pool, 1, "USD")
	if !a.Equal(decimal.MustFromString("10000")) || !l.IsZero() {
		t.Fatalf("hold released: avail=%s locked=%s", a, l)
	}
	// Late confirm must now fail.
	_, err = svc.Confirm(ctx, ConfirmWithdrawalRequest{
		WithdrawalID: res.WithdrawalID, AccountID: 1, UserID: 100, Token: res.ConfirmToken,
	})
	if err == nil {
		t.Fatal("post-expiry confirm accepted")
	}
}

// ---------------------------------------------------------------------------
// Internal transfer — real ledger posting, both directions, history rows.
// ---------------------------------------------------------------------------

func TestITInternalTransfer(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "10000")
	itSeedDeposit(t, ctx, led, 2, "USD", "500")
	itSeedDeposit(t, ctx, led, 3, "USD", "3000")
	freeze := itFreeze(t, pool)
	store := NewPgStore(pool)
	svc, err := NewTransferService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("svc: %v", err)
	}

	res, err := svc.Create(ctx, CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2,
		Currency: "USD", Amount: "400", IdempotencyKey: "x-fer-1",
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Transfer.Status != TransferCompleted || res.Transfer.JournalEntryID == nil {
		t.Fatalf("result %+v", res.Transfer)
	}
	a1, _ := balRow(t, ctx, pool, 1, "USD")
	a2, _ := balRow(t, ctx, pool, 2, "USD")
	if !a1.Equal(decimal.MustFromString("9600")) || !a2.Equal(decimal.MustFromString("900")) {
		t.Fatalf("post-transfer: a1=%s a2=%s", a1, a2)
	}
	// GL linkage: journal_sums tracks the wallet invariant — acct 1 net
	// must now equal its wallet total (10000 - 400 = 9600).
	var net string
	if err := pool.QueryRow(ctx,
		`SELECT net_balance::text FROM journal_sums WHERE account_id=1 AND currency='USD'`).Scan(&net); err != nil {
		t.Fatalf("journal sums: %v", err)
	}
	if net != "9600.00000000" {
		t.Fatalf("journal_sums net=%s, want 9600", net)
	}
	// And the journal itself is balanced in ledger_lines.
	var dr, cr string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(debit_amount),0)::text, COALESCE(sum(credit_amount),0)::text
		FROM ledger_lines WHERE journal_entry_id=$1`, *res.Transfer.JournalEntryID).Scan(&dr, &cr); err != nil {
		t.Fatalf("ledger lines: %v", err)
	}
	if dr != cr || dr == "0" {
		t.Fatalf("unbalanced or empty journal: dr=%s cr=%s", dr, cr)
	}
	// Cross-user rejected at the DB level too.
	_, err = svc.Create(ctx, CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 3,
		Currency: "USD", Amount: "10",
	})
	if err == nil {
		t.Fatal("cross-user transfer accepted")
	}
	// Frozen source rejected.
	_, err = svc.Create(ctx, CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 4, ToAccountID: 2,
		Currency: "USD", Amount: "10",
	})
	if err == nil {
		t.Fatal("frozen-source transfer accepted")
	}
	// Idempotent replay via store-level unique constraint.
	res2, err := svc.Create(ctx, CreateTransferRequest{
		CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2,
		Currency: "USD", Amount: "400", IdempotencyKey: "x-fer-1",
	})
	if err != nil || !res2.Replayed || res2.Transfer.ID != res.Transfer.ID {
		t.Fatalf("replay %+v %v", res2, err)
	}
}

// ---------------------------------------------------------------------------
// Transfer history — filtering + cursor pagination.
// ---------------------------------------------------------------------------

func TestITTransferHistory(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "10000")
	itSeedDeposit(t, ctx, led, 2, "USD", "500")
	itSeedDeposit(t, ctx, led, 3, "USD", "3000")
	freeze := itFreeze(t, pool)
	store := NewPgStore(pool)
	svc, err := NewTransferService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.Create(ctx, CreateTransferRequest{
			CallerAccountID: 1, FromAccountID: 1, ToAccountID: 2,
			Currency: "USD", Amount: "10", IdempotencyKey: fmt.Sprintf("h-%d", i),
		}); err != nil {
			t.Fatalf("seed transfer %d: %v", i, err)
		}
	}
	hs := NewHistoryService(store)
	rows, next, total, err := hs.Transfers(ctx, 1, TransferFilter{Limit: 2})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(rows) != 2 || next == "" || total != 3 {
		t.Fatalf("page1: rows=%d next=%q total=%d", len(rows), next, total)
	}
	cts, cid, err := DecodeCursor(next)
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	rows2, _, _, err := hs.Transfers(ctx, 1, TransferFilter{Limit: 2, CursorTS: &cts, CursorID: cid})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(rows2) != 1 {
		t.Fatalf("page2 len %d", len(rows2))
	}
	if rows[0].JournalEntryID == nil {
		t.Fatal("missing GL linkage")
	}
	// Foreign perspective must be FORBIDDEN.
	if _, _, _, err := hs.Transfers(ctx, 1, TransferFilter{PerspectiveID: 3}); err == nil {
		t.Fatal("foreign sub_account_id accepted")
	}
	// Incoming direction for account 2's family: user 200 only owns acct 3,
	// so caller=2 resolves user 100 — direction OUT on account 1 perspective.
	rows3, _, _, err := hs.Transfers(ctx, 2, TransferFilter{Direction: "IN", PerspectiveID: 2, Limit: 10})
	if err != nil || len(rows3) != 3 {
		t.Fatalf("incoming: %+v %v", rows3, err)
	}
}

// ---------------------------------------------------------------------------
// Chargeback lifecycle with real FreezeService (dual control via resolver).
// ---------------------------------------------------------------------------

func TestITChargebackLifecycle(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itSchema(t, ctx, pool)
	freeze := itFreeze(t, pool)
	store := NewPgStore(pool)
	svc, err := NewChargebackService(store, freeze)
	if err != nil {
		t.Fatalf("svc: %v", err)
	}

	res, err := svc.Create(ctx, 500, CreateChargebackRequest{
		AccountID: 1, Currency: "USD", Amount: "750", Reason: "card dispute 12.3",
		FreezeAccount: true, ApproverUserID: 600, ClientIP: "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Chargeback.Status != "EVIDENCE_COLLECTED" || !res.AccountFrozen {
		t.Fatalf("create %+v", res.Chargeback)
	}
	var st string
	if err := pool.QueryRow(ctx, `SELECT status FROM accounts WHERE id=1`).Scan(&st); err != nil {
		t.Fatalf("acct status: %v", err)
	}
	if st != "FROZEN" {
		t.Fatalf("account not frozen: %s", st)
	}
	// Evidence was auto-collected at create — verify the bundle exists.
	var ev int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM chargeback_evidence WHERE chargeback_id=$1`,
		res.Chargeback.ID).Scan(&ev); err != nil || ev == 0 {
		t.Fatalf("evidence %d %v", ev, err)
	}
	// Submit for the processor — EVIDENCE_COLLECTED → SUBMITTED.
	if _, err := svc.Submit(ctx, 500, res.Chargeback.ID, "10.0.0.1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	var cst string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM chargebacks WHERE id=$1`, res.Chargeback.ID).Scan(&cst); err != nil {
		t.Fatalf("cb status: %v", err)
	}
	if cst != "SUBMITTED" {
		t.Fatalf("status after submit %s", cst)
	}
	// Resolve as WON — terminal.
	fin, err := svc.Resolve(ctx, 500, res.Chargeback.ID, "WON", "evidence accepted", "10.0.0.1")
	if err != nil || fin.Status != "RESOLVED_WON" {
		t.Fatalf("resolve %+v %v", fin, err)
	}
	// Second resolve must fail (state machine guard).
	if _, err := svc.Resolve(ctx, 500, res.Chargeback.ID, "LOST", "retry", "10.0.0.1"); err == nil {
		t.Fatal("double-resolve accepted")
	}
	// Admin audit trail exists.
	var audits int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE target_type='account' AND target_id=1`).Scan(&audits); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if audits == 0 {
		t.Fatal("no admin audit rows")
	}
}

// ---------------------------------------------------------------------------
// Phase-11 Task 11.3.1/11.3.11 — suspense mappings + rail payments store
// surface against real PostgreSQL (migration 108 tables + enums).
// ---------------------------------------------------------------------------

func TestITSuspenseAndRailPayments(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itSchema(t, ctx, pool)
	store := NewPgStore(pool)

	tx, err := store.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Attributed PENDING_REVIEW deposit row (the quarantine funding row).
	ref := "BANK-IT-1"
	bm := "SEPA"
	orig := "DE999"
	idem := "wire:BANK-IT-1"
	dep, err := store.InsertDepositPending(ctx, tx, DepositRow{
		AccountID: 1, Currency: "EUR", Amount: decimal.MustFromString("2500"),
		Status: FundingPendingReview, BankMethod: &bm, Reference: &ref,
		ReferenceAccount: &orig, IdempotencyKey: &idem,
		ReviewTier: strPtr(ReviewTierPendingReview),
	})
	if err != nil {
		t.Fatalf("insert deposit: %v", err)
	}
	if dep.ID == 0 || dep.Status != FundingPendingReview {
		t.Fatalf("deposit row %+v", dep)
	}
	// bank_tx_id + funding link on the suspense mapping.
	score := 0.42
	rail := "SEPA"
	susp, err := store.InsertSuspenseMapping(ctx, tx, SuspenseRow{
		BankTxID: "BANK-IT-1", FundingTransactionID: &dep.ID,
		AccountID: ptr64(1), Rail: &rail, Currency: "EUR",
		Amount:            decimal.MustFromString("2500"),
		OriginatorName:    strPtr("Mallory"),
		OriginatorAccount: &orig,
		NameMatchScore:    &score,
		UnmatchedReason:   "NAME_MISMATCH",
		GLAccount:         "2150",
		QuarantineStatus:  "QUARANTINED",
		SLAExpiresAt:      time.Now().UTC().Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("insert suspense: %v", err)
	}
	// bank_tx_id UNIQUE — dup insert must surface ErrIdemConflict.
	if _, err := store.InsertSuspenseMapping(ctx, tx, SuspenseRow{
		BankTxID: "BANK-IT-1", Currency: "EUR",
		Amount: decimal.MustFromString("1"), UnmatchedReason: "MISSING_REFERENCE",
		SLAExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != ErrIdemConflict {
		t.Fatalf("dup bank_tx_id: %v", err)
	}
	// Rail payment row — persisted pacs.004 return-wire envelope.
	rp, err := store.InsertRailPayment(ctx, tx, RailPaymentRow{
		SuspenseMappingID: &susp.ID, Direction: RailDirectionReturn,
		Rail: "SEPA", MessageType: MsgPacs004, EndToEndID: "E2E-IT-1",
		Status: RailPaymentPrepared, Envelope: []byte(`{"msg":"pacs.004"}`),
	})
	if err != nil {
		t.Fatalf("insert rail payment: %v", err)
	}
	if err := store.SetSuspenseLinks(ctx, tx, susp.ID, nil, &rp.ID); err != nil {
		t.Fatalf("suspense links: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Read-back paths (outside tx).
	got, err := store.SuspenseByBankTx(ctx, "BANK-IT-1")
	if err != nil || got.ReturnPaymentID == nil || *got.ReturnPaymentID != rp.ID {
		t.Fatalf("suspense readback %+v %v", got, err)
	}
	if got.NameMatchScore == nil || *got.NameMatchScore != 0.42 {
		t.Fatalf("score round-trip %+v", got.NameMatchScore)
	}
	gotRp, err := store.RailPaymentByEndToEndID(ctx, "E2E-IT-1")
	if err != nil || gotRp.Status != RailPaymentPrepared {
		t.Fatalf("rail payment readback %+v %v", gotRp, err)
	}

	// Status transitions inside a second tx.
	tx2, err := store.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	defer tx2.Rollback(ctx) //nolint:errcheck
	if _, err := store.SuspenseForUpdate(ctx, tx2, susp.ID); err != nil {
		t.Fatalf("suspense lock: %v", err)
	}
	if _, err := store.RailPaymentForUpdate(ctx, tx2, rp.ID); err != nil {
		t.Fatalf("rail lock: %v", err)
	}
	if _, err := store.FundingTxForUpdate(ctx, tx2, dep.ID); err != nil {
		t.Fatalf("funding lock: %v", err)
	}
	inv := int64(500)
	notes := "returned to originator"
	now := time.Now().UTC()
	if err := store.SetSuspenseStatus(ctx, tx2, susp.ID,
		"RETURNED_TO_SOURCE", &inv, &notes, &now); err != nil {
		t.Fatalf("suspense status: %v", err)
	}
	if err := store.SetFundingTxStatus(ctx, tx2, dep.ID, FundingFailed, nil); err != nil {
		t.Fatalf("funding status: %v", err)
	}
	rc := "AC04"
	reason := "third-party deposit rejected"
	if err := store.SetRailPaymentStatus(ctx, tx2, rp.ID,
		RailPaymentReturned, &rc, &reason, nil, &now); err != nil {
		t.Fatalf("rail status: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit2: %v", err)
	}

	// List filter — quarantine journal surfaces the row for ops.
	rows, total, err := store.ListSuspense(ctx, SuspenseFilter{
		Status: "RETURNED_TO_SOURCE", Limit: 10,
	})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("list suspense %d %d %v", total, len(rows), err)
	}
	rows0, total0, err := store.ListSuspense(ctx, SuspenseFilter{
		Status: "QUARANTINED", Limit: 10,
	})
	if err != nil || total0 != 0 || len(rows0) != 0 {
		t.Fatalf("quarantined filter leaked %d rows", len(rows0))
	}
}

func ptr64(v int64) *int64 { return &v }
