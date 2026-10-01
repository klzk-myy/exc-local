// Integration tests for the Phase-11 flows cluster (Tasks 11.3.2,
// 11.3.3, 11.3.6, 11.3.10) against real PostgreSQL + Redis and the real
// settlement.LedgerService. Same gating/helpers as integration_test.go:
//
//	EXC_PG_TEST=1            enable
//	EXC_TEST_DSN             postgres DSN
//	EXC_REDIS_TEST_ADDR      redis addr (default 127.0.0.1:16379)
//	EXC_REDIS_TEST_PASSWORD  redis password (default redpass)
//
// Each test layers migrations 040 (bank_accounts — the sibling Task
// 11.3.7 registry the whitelist gate reads), 078
// (withdrawal_whitelist_settings) and 199 (funding flow extensions)
// on top of the shared itSchema fixture.
package funding

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// itFlowSchema builds the shared fixture + the flows-cluster tables.
func itFlowSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	itSchema(t, ctx, pool)
	for _, m := range []string{
		"040_bank_accounts.up.sql",
		"078_withdrawal_whitelist_settings.up.sql",
		"199_funding_flow_extensions.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
}

// TestITFlowsMigrationRoundTrip applies 078 + 199 up then down —
// both directions must execute cleanly and drop every object they own.
func TestITFlowsMigrationRoundTrip(t *testing.T) {
	ctx, pool, _ := itPool(t)
	// 199's FKs need accounts + funding_transactions + nostro_accounts.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE users (id BIGSERIAL PRIMARY KEY);
		CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY, user_id BIGINT);
		CREATE TABLE funding_transactions (
		    id BIGSERIAL PRIMARY KEY, account_id BIGINT REFERENCES accounts(id),
		    type VARCHAR(16), status VARCHAR(16), currency VARCHAR(3),
		    amount NUMERIC(28,8), created_at TIMESTAMPTZ DEFAULT now(),
		    review_deadline TIMESTAMPTZ);
		CREATE TABLE nostro_accounts (id BIGSERIAL PRIMARY KEY);`); err != nil {
		t.Fatalf("anchor ddl: %v", err)
	}
	for _, m := range []string{
		"078_withdrawal_whitelist_settings.up.sql",
		"199_funding_flow_extensions.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
	// Up assertions: columns + tables exist in the scratch schema.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name='funding_transactions'
		  AND column_name IN ('hold_until','reviewed_by','reviewed_at','originator_name')`).Scan(&n); err != nil {
		t.Fatalf("post-up columns: %v", err)
	}
	if n != 4 {
		t.Fatalf("199 must add 4 funding_transactions columns, found %d", n)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name IN ('withdrawal_whitelist_settings','deposit_confirmations',
		                     'withdrawal_dispatch_queue','funding_ops_alerts',
		                     'nostro_replenishment_requests','withdrawal_destination_holds')`).Scan(&n); err != nil {
		t.Fatalf("post-up tables: %v", err)
	}
	if n != 6 {
		t.Fatalf("078+199 must create 6 tables, found %d", n)
	}
	for _, m := range []string{
		"199_funding_flow_extensions.down.sql",
		"078_withdrawal_whitelist_settings.down.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name IN ('withdrawal_whitelist_settings','deposit_confirmations',
		                     'withdrawal_dispatch_queue','funding_ops_alerts',
		                     'nostro_replenishment_requests','withdrawal_destination_holds')`).Scan(&n); err != nil {
		t.Fatalf("post-down tables: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d flows tables survived the down migrations", n)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name='funding_transactions'
		  AND column_name IN ('hold_until','reviewed_by','reviewed_at','originator_name')`).Scan(&n); err != nil {
		t.Fatalf("post-down columns: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d 199 columns survived the down migration", n)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_type t JOIN pg_namespace ns ON ns.oid = t.typnamespace
		WHERE ns.nspname = current_schema()
		  AND t.typname = 'withdrawal_whitelist_mode_enum'`).Scan(&n); err != nil {
		t.Fatalf("post-down enum: %v", err)
	}
	if n != 0 {
		t.Fatal("withdrawal_whitelist_mode_enum survived the down migration")
	}
}

// ---------------------------------------------------------------------------
// Task 11.3.10 — whitelist mode + deactivation lock against real PG.
// ---------------------------------------------------------------------------

func TestITWhitelistLifecycle(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFlowSchema(t, ctx, pool)
	store := NewPgStore(pool)
	svc, err := NewWhitelistService(store)
	if err != nil {
		t.Fatalf("whitelist svc: %v", err)
	}

	v, err := svc.Enable(ctx, 1, 100)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if v.Mode != WhitelistModeOnly || !v.WhitelistOnly {
		t.Fatalf("expected WHITELIST_ONLY, got %+v", v)
	}
	// Persisted row.
	var mode string
	if err := pool.QueryRow(ctx,
		`SELECT mode::text FROM withdrawal_whitelist_settings WHERE account_id=1`).Scan(&mode); err != nil {
		t.Fatalf("settings read: %v", err)
	}
	if mode != "WHITELIST_ONLY" {
		t.Fatalf("persisted mode %s", mode)
	}

	v, err = svc.Disable(ctx, 1, 100)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !v.WithdrawalsLocked || !v.ReenableLocked {
		t.Fatalf("disable must latch both locks, got %+v", v)
	}
	var lockUntil time.Time
	if err := pool.QueryRow(ctx,
		`SELECT withdrawal_lock_until FROM withdrawal_whitelist_settings
		 WHERE account_id=1`).Scan(&lockUntil); err != nil {
		t.Fatalf("lock read: %v", err)
	}
	if d := time.Until(lockUntil); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("deactivation lock must be ~24h, got %s", d)
	}
	// Re-enable inside the latch → WHITELIST_CHANGE_LOCKED.
	if _, err := svc.Enable(ctx, 1, 100); err == nil {
		t.Fatal("re-enable inside the 24h latch must refuse")
	}
}

// TestITWithdrawalFlowWhitelistGate — a WHITELIST_ONLY account refuses
// an unregistered destination at create; a verified+unlocked
// beneficiary passes the gate.
func TestITWithdrawalFlowWhitelistGate(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFlowSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "10000")
	store := NewPgStore(pool)
	freeze := itFreeze(t, pool)

	inner, err := NewWithdrawalService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("withdrawal svc: %v", err)
	}
	inner.WithUSDConverter(usdIdentity{})
	flow, err := NewFlowService(inner, store)
	if err != nil {
		t.Fatalf("flow svc: %v", err)
	}
	wsvc, _ := NewWhitelistService(store)
	if _, err := wsvc.Enable(ctx, 1, 100); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// Unregistered destination → WITHDRAWAL_WHITELIST_ONLY.
	_, err = flow.Create(ctx, CreateWithdrawalRequest{
		AccountID: 1, UserID: 100, Currency: "USD", Amount: "50",
		ReferenceAccount: "UNREGISTERED-IBAN", IdempotencyKey: "wl-it-1",
	})
	requireErrCode(t, err, CodeWithdrawalWhitelistOnly)

	// Register + verify a beneficiary whose unlocked_at already lapsed.
	if _, err := pool.Exec(ctx, `
		INSERT INTO bank_accounts (account_id, currency, iban, bank_name,
		    beneficiary_name, rail, status, verified_at, verified_by,
		    unlocked_at)
		VALUES (1, 'USD', 'IBAN-OK-1', 'Test Bank', 'User One', 'SEPA',
		        'VERIFIED', now() - interval '25 hours', 100,
		        now() - interval '1 hour')`); err != nil {
		t.Fatalf("seed beneficiary: %v", err)
	}
	res, err := flow.Create(ctx, CreateWithdrawalRequest{
		AccountID: 1, UserID: 100, Currency: "USD", Amount: "50",
		ReferenceAccount: "IBAN-OK-1", IdempotencyKey: "wl-it-2",
	})
	if err != nil {
		t.Fatalf("create vs verified beneficiary: %v", err)
	}
	if res.Status != FundingPending {
		t.Fatalf("expected PENDING, got %s", res.Status)
	}
}

// TestITUnverifiedDestinationHold — without the beneficiary-registry
// seam on the inner service an unregistered destination is permitted
// under ALLOW_ALL but the withdrawal is stamped with the 24h
// first-seen hold (Task 11.3.2 step 6).
func TestITUnverifiedDestinationHold(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFlowSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "10000")
	store := NewPgStore(pool)
	freeze := itFreeze(t, pool)

	inner, err := NewWithdrawalService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("withdrawal svc: %v", err)
	}
	inner.WithUSDConverter(usdIdentity{})
	flow, err := NewFlowService(inner, store)
	if err != nil {
		t.Fatalf("flow svc: %v", err)
	}
	res, err := flow.Create(ctx, CreateWithdrawalRequest{
		AccountID: 1, UserID: 100, Currency: "USD", Amount: "50",
		ReferenceAccount: "BRAND-NEW-IBAN", IdempotencyKey: "uv-it-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var holdUntil *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT hold_until FROM funding_transactions WHERE id=$1`,
		res.WithdrawalID).Scan(&holdUntil); err != nil {
		t.Fatalf("hold read: %v", err)
	}
	if holdUntil == nil {
		t.Fatal("unverified destination must stamp hold_until")
	}
	if d := time.Until(*holdUntil); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("hold must be ~24h, got %s", d)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM withdrawal_destination_holds
		 WHERE account_id=1 AND destination='BRAND-NEW-IBAN'`).Scan(&n); err != nil {
		t.Fatalf("destination hold row: %v", err)
	}
	if n != 1 {
		t.Fatalf("first-seen row must persist, got %d", n)
	}
	// Second create to the same destination replays the SAME unlocked_at
	// (first-seen registry is not refreshed by re-attempts).
	res2, err := flow.Create(ctx, CreateWithdrawalRequest{
		AccountID: 1, UserID: 100, Currency: "USD", Amount: "50",
		ReferenceAccount: "brand-new-iban", IdempotencyKey: "uv-it-2",
	})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	var hu2 *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT hold_until FROM funding_transactions WHERE id=$1`,
		res2.WithdrawalID).Scan(&hu2); err != nil {
		t.Fatalf("hold2 read: %v", err)
	}
	if hu2 == nil || !hu2.Equal(*holdUntil) {
		t.Fatalf("first-seen unlocked_at must be reused, got %v vs %v", hu2, holdUntil)
	}
}

// ---------------------------------------------------------------------------
// Task 11.3.3 — deposit lifecycle on the real ledger.
// ---------------------------------------------------------------------------

func TestITDepositFlowLifecycle(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFlowSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	store := NewPgStore(pool)
	freeze := itFreeze(t, pool)

	svc, err := NewDepositService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("deposit svc: %v", err)
	}
	svc.WithUSDConverter(usdIdentity{})

	// Client intent, then the detection adopts it by IntentReference.
	intent, err := svc.CreateIntent(ctx, CreateDepositIntentRequest{
		AccountID: 1, UserID: 100, Currency: "USD", Amount: "5000",
		Reference: "CLIENT-WIRE-1", IdempotencyKey: "di-1",
	})
	if err != nil {
		t.Fatalf("intent: %v", err)
	}
	res, err := svc.IngestDetected(ctx, IngestDepositRequest{
		AccountID: 1, Currency: "USD", Amount: "5000",
		Reference: "BANKTX-99", Source: "STATEMENT",
		IntentReference: "CLIENT-WIRE-1", ReceivedBy: 500,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.DepositID != intent.DepositID {
		t.Fatalf("intent %d not adopted (deposit %d)", intent.DepositID, res.DepositID)
	}
	// Hold landed: locked +5000, available 0.
	a, l := balRow(t, ctx, pool, 1, "USD")
	if !a.IsZero() || !l.Equal(decimal.MustFromString("5000")) {
		t.Fatalf("detection hold: avail=%s locked=%s", a, l)
	}
	// Idempotent replay of the same ingest notification.
	rep, err := svc.IngestDetected(ctx, IngestDepositRequest{
		AccountID: 1, Currency: "USD", Amount: "5000",
		Reference: "BANKTX-99", Source: "STATEMENT",
		IdempotencyKey: "dep:BANKTX-99", ReceivedBy: 500,
	})
	if err != nil || !rep.Replayed {
		t.Fatalf("expected ingest replay, got %+v err=%v", rep, err)
	}
	// Second distinct source resolves the AUTO tier → credited.
	out, err := svc.Confirm(ctx, DepositConfirmRequest{
		DepositID: res.DepositID, Source: "WEBHOOK",
		SenderName: "User One", ReceivedBy: 500,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingCompleted {
		t.Fatalf("expected COMPLETED, got %s (flags %v)", out.Status, out.Flags)
	}
	a, l = balRow(t, ctx, pool, 1, "USD")
	if !a.Equal(decimal.MustFromString("5000")) || !l.IsZero() {
		t.Fatalf("post-credit: avail=%s locked=%s", a, l)
	}
	var confN int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deposit_confirmations WHERE funding_transaction_id=$1`,
		res.DepositID).Scan(&confN); err != nil {
		t.Fatalf("confirmations: %v", err)
	}
	if confN != 2 {
		t.Fatalf("two independent source rows expected, got %d", confN)
	}
}

// ---------------------------------------------------------------------------
// Task 11.3.6 — nostro-aware dispatch on the real ledger.
// ---------------------------------------------------------------------------

func TestITNostroDispatch(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itFlowSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "10000")
	store := NewPgStore(pool)
	freeze := itFreeze(t, pool)

	inner, err := NewWithdrawalService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("withdrawal svc: %v", err)
	}
	inner.WithUSDConverter(usdIdentity{})
	flow, err := NewFlowService(inner, store)
	if err != nil {
		t.Fatalf("flow svc: %v", err)
	}
	flow.WithTOTP(staticTOTPProvider{secret: "X"}, alwaysVerify)
	disp, err := NewDispatchService(store, ledgerPosterAdapter{led})
	if err != nil {
		t.Fatalf("dispatch svc: %v", err)
	}
	flow.WithDispatcher(disp)

	// Insufficient nostro → QUEUED (never rejected) + durable alert.
	if _, err := pool.Exec(ctx, `
		INSERT INTO nostro_accounts (currency, bank_name, iban, balance, status)
		VALUES ('USD','Ops Bank','NSTR-1', 1000, 'ACTIVE'),
		       ('USD','Reserve Bank','NSTR-2', 500, 'ACTIVE')`); err != nil {
		t.Fatalf("seed nostro: %v", err)
	}
	// Verified, timelock-lapsed beneficiary for the payout destination —
	// without it the first-seen destination hold queues the withdrawal
	// for a different reason than nostro insufficiency.
	if _, err := pool.Exec(ctx, `
		INSERT INTO bank_accounts (account_id, currency, iban, bank_name,
		    beneficiary_name, rail, status, verified_at, verified_by,
		    unlocked_at)
		VALUES (1, 'USD', 'PAYOUT-IBAN', 'Payout Bank', 'User One', 'SWIFT',
		        'VERIFIED', now() - interval '25 hours', 100,
		        now() - interval '1 hour')`); err != nil {
		t.Fatalf("seed payout beneficiary: %v", err)
	}
	res, err := flow.Create(ctx, CreateWithdrawalRequest{
		AccountID: 1, UserID: 100, Currency: "USD", Amount: "5000",
		ReferenceAccount: "PAYOUT-IBAN", IdempotencyKey: "nd-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := flow.ConfirmStepUp(ctx, StepUpConfirmRequest{
		ConfirmWithdrawalRequest: ConfirmWithdrawalRequest{
			WithdrawalID: res.WithdrawalID, AccountID: 1, UserID: 100,
			Token: res.ConfirmToken,
		},
		SessionTwoFactor: true,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingConfirmed {
		t.Fatalf("expected CONFIRMED, got %s", out.Status)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM withdrawal_dispatch_queue WHERE withdrawal_id=$1`,
		res.WithdrawalID).Scan(&status); err != nil {
		t.Fatalf("queue read: %v", err)
	}
	if status != QueueQueued {
		t.Fatalf("expected QUEUED, got %s", status)
	}
	var code string
	if err := pool.QueryRow(ctx,
		`SELECT code FROM funding_ops_alerts WHERE funding_transaction_id=$1`,
		res.WithdrawalID).Scan(&code); err != nil {
		t.Fatalf("alert read: %v", err)
	}
	if code != "NOSTRO_INSUFFICIENT_FUNDS" {
		t.Fatalf("alert code %s", code)
	}
	// Funds stay locked while queued.
	a, l := balRow(t, ctx, pool, 1, "USD")
	if !a.Equal(decimal.MustFromString("5000")) || !l.Equal(decimal.MustFromString("5000")) {
		t.Fatalf("queued hold: avail=%s locked=%s", a, l)
	}
	// Auto-replenishment request exists for the shortfall.
	var repAmt string
	if err := pool.QueryRow(ctx,
		`SELECT amount::text FROM nostro_replenishment_requests
		 WHERE currency='USD'`).Scan(&repAmt); err != nil {
		t.Fatalf("replenishment read: %v", err)
	}
	if repAmt != "3500.00000000" {
		t.Fatalf("auto-replenish must request the 3500 shortfall, got %s", repAmt)
	}

	// Top up the operating nostro → sweep dispatches + consumes the hold.
	if _, err := pool.Exec(ctx,
		`UPDATE nostro_accounts SET balance = balance + 10000
		 WHERE iban='NSTR-1'`); err != nil {
		t.Fatalf("top-up: %v", err)
	}
	n, err := disp.SweepDue(ctx, 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep must dispatch 1, got %d", n)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM funding_transactions WHERE id=$1`,
		res.WithdrawalID).Scan(&status); err != nil {
		t.Fatalf("status read: %v", err)
	}
	if status != FundingCompleted {
		t.Fatalf("expected COMPLETED, got %s", status)
	}
	a, l = balRow(t, ctx, pool, 1, "USD")
	if !a.Equal(decimal.MustFromString("5000")) || !l.IsZero() {
		t.Fatalf("dispatched consumption: avail=%s locked=%s", a, l)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status FROM withdrawal_dispatch_queue WHERE withdrawal_id=$1`,
		res.WithdrawalID).Scan(&status); err != nil {
		t.Fatalf("queue close: %v", err)
	}
	if status != QueueDispatched {
		t.Fatalf("queue must close DISPATCHED, got %s", status)
	}
	// Idempotent retry — already COMPLETED is a no-op.
	r2, err := disp.Release(ctx, res.WithdrawalID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if r2.Disposition != "SKIPPED" {
		t.Fatalf("completed retry must skip, got %+v", r2)
	}
}
