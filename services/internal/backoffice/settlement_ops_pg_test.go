// PG-gated replay proof for the Task 24.3.6/.7/.13/.14/.19 schema chain —
// gated on EXC_PG_TEST=1 / EXC_TEST_DSN (same boTestPool convention as
// integration_test.go). Applies the real prerequisite migrations verbatim
// (no stubs) so enum ALTERs, FK references and CHECK constraints are
// exercised exactly as the migrator runs them.
package backoffice

import (
	"context"
	"testing"
	"time"
)

// settlementOpsChain is the verbatim migration chain 084/260 depend on:
// users → accounts → nostro_accounts → settlement_instructions → GL
// (chart_of_accounts/journal_entries/ledger_lines) → prime_brokerage
// (prime_brokers/pb_giveup_trades) → dispatch columns (112: updated_at/
// message_payload/dispatched_at, required by exceptions.go writes) →
// settlement_penalties (084: settlement_fails/buy_in_events + fail_flag)
// → settlement_ops (260).
var settlementOpsChain = []string{
	"002_create_users.up.sql",
	"003_create_accounts.up.sql",
	"018_create_nostro_accounts.up.sql",
	"019_create_settlement_instructions.up.sql",
	"036_create_general_ledger.up.sql",
	"037_create_prime_brokerage.up.sql",
	"112_settlement_dispatch_and_nostro_movements.up.sql",
	"084_settlement_penalties.up.sql",
	"260_settlement_ops.up.sql",
}

// TestPgSettlementOpsSchema_Replay applies the full up-chain, verifies the
// task-critical shapes (PAIR_MISMATCH check, VOID enum, regime enum, GL
// chart additions), then replays 260/084 down and re-up to prove the
// migration is idempotent across a full cycle.
func TestPgSettlementOpsSchema_Replay(t *testing.T) {
	ctx, pool := boTestPool(t)
	execAll(t, ctx, pool, "", settlementOpsChain...)

	// 'VOID' joined settlement_status_enum (Task 24.3.6 reversal target).
	// pg_enum is catalog-wide — scope to the test schema's namespace.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_enum e
		JOIN pg_type t ON t.oid = e.enumtypid
		WHERE t.typname = 'settlement_status_enum' AND e.enumlabel = 'VOID'
		  AND t.typnamespace = current_schema()::regnamespace`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("VOID enum value missing: n=%d err=%v", n, err)
	}

	// PAIR_MISMATCH is a legal pb_recon_breaks.break_type (24.3.7).
	if _, err := pool.Exec(ctx, `
		INSERT INTO prime_brokers (pb_name, bic_code, fix_comp_id)
		VALUES ('Test PB','TESTUS33','PBTEST')`); err != nil {
		t.Fatalf("seed prime_brokers: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pb_recon_runs (prime_broker_id, run_date, source)
		VALUES (1, now()::date, 'AFFIRMATION')`); err != nil {
		t.Fatalf("seed pb_recon_runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pb_recon_breaks
		    (run_id, prime_broker_id, break_type)
		VALUES (1, 1, 'PAIR_MISMATCH')`); err != nil {
		t.Fatalf("PAIR_MISMATCH break rejected by CHECK: %v", err)
	}

	// FX_CLOSEOUT is a first-class fail regime (24.3.19 supersedes CSDR
	// penalty economics for FX — 084 header provenance note).
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_enum e
		JOIN pg_type t ON t.oid = e.enumtypid
		WHERE t.typname = 'settlement_fail_regime_enum'
		  AND e.enumlabel = 'FX_CLOSEOUT'
		  AND t.typnamespace = current_schema()::regnamespace`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("FX_CLOSEOUT regime missing: n=%d err=%v", n, err)
	}

	// §17.14 GL chart additions exist per currency.
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM chart_of_accounts
		WHERE account_code IN ('5990_SETTLEMENT_WRITE_OFF_USD',
		                       '4600_FAIL_INTEREST_REVENUE_USD',
		                       '1020_SETTLEMENT_FAIL_CLAIM_USD',
		                       '1090_SETTLEMENT_FAIL_MEMO_USD',
		                       '2090_PB_CREDIT_MEMO_USD')`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("chart additions missing: n=%d err=%v", n, err)
	}

	// Down-replay: 260 then 084 (reverse dependency order — 260's
	// fx_fail_closeouts FK-references 084's settlement_fails).
	execAll(t, ctx, pool, "",
		"260_settlement_ops.down.sql",
		"084_settlement_penalties.down.sql")
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('settlement_exceptions','pb_recon_breaks',
		                     'settlement_fails','fx_fail_closeouts',
		                     'buy_in_events')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("down-replay left tables behind: n=%d err=%v", n, err)
	}
	// fail_flag column is gone too.
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name='settlement_instructions'
		  AND column_name='fail_flag'`).
		Scan(&n); err != nil || n != 0 {
		t.Fatalf("fail_flag column survived down-replay: n=%d", n)
	}

	// Re-up 084+260 — full-cycle idempotency.
	execAll(t, ctx, pool, "",
		"084_settlement_penalties.up.sql",
		"260_settlement_ops.up.sql")
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('settlement_exceptions','pb_recon_breaks',
		                     'settlement_fails','fx_fail_closeouts',
		                     'buy_in_events')`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("re-up did not restore schema: n=%d err=%v", n, err)
	}
}

// TestPgSettlementOps_FailedLegScan proves the ISD+1 detection query picks
// up both PENDING and already-FAILED legs (the 24.3.13 fix) on the real
// schema — the in-memory fake mirrors this predicate.
func TestPgSettlementOps_FailedLegScan(t *testing.T) {
	ctx, pool := boTestPool(t)
	execAll(t, ctx, pool, "", settlementOpsChain...)

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (email) VALUES ('a@t'),('b@t');
		INSERT INTO accounts (user_id, account_type) VALUES (1,'SPOT'),(2,'SPOT')`); err != nil {
		t.Fatalf("seed users/accounts: %v", err)
	}
	// One late PENDING leg, one late FAILED leg, one settled leg, one
	// not-yet-due leg.
	if _, err := pool.Exec(ctx, `
		INSERT INTO settlement_instructions
		    (trade_id, account_id, currency, amount, direction,
		     settlement_date, status)
		VALUES (1,1,'USD',100,'PAY', now()::date - 1, 'PENDING'),
		       (2,1,'USD',200,'PAY', now()::date - 1, 'FAILED'),
		       (3,1,'USD',300,'PAY', now()::date - 1, 'SETTLED'),
		       (4,1,'USD',400,'PAY', now()::date + 1, 'PENDING')`); err != nil {
		t.Fatalf("seed instructions: %v", err)
	}

	store := NewPgxFailStore(pool)
	var legs []ExceptionLeg
	err := store.InTx(ctx, func(ctx context.Context, tx FailTx) error {
		var err error
		legs, err = tx.LateInstructions(ctx, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatalf("LateInstructions: %v", err)
	}
	if len(legs) != 2 {
		t.Fatalf("want PENDING+FAILED late legs (2), got %d (%+v)", len(legs), legs)
	}
	status := map[string]bool{}
	for _, l := range legs {
		status[l.Status] = true
	}
	if !status["PENDING"] || !status["FAILED"] {
		t.Fatalf("late set missing a status: %v", status)
	}
}
