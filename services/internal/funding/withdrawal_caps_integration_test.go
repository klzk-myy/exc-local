// withdrawal_caps_integration_test.go — PG-gated end-to-end coverage
// for the Phase-11 Task 11.3.2 withdrawal-cap AC row (spec §24 #79 /
// T11-006): POST /api/v1/withdrawals → WithdrawalService.Create →
// risk.LimitsService.CheckWithdrawal against a real PgStore.
//
// Exercises, on a real schema + real ledger:
//   - per-account hourly window (risk_limits.withdraw_rate_per_hour,
//     summed from live funding_transactions rows);
//   - exchange-wide daily ceiling (risk_limits.exchange_daily_withdraw_
//     limit, migration 272 — the global row only).
//
// Gating/fixtures as integration_test.go: EXC_PG_TEST=1, EXC_TEST_DSN.
// The scratch schema adds the risk migrations the store reads
// (011 + 047 + 109 + 272) on top of itSchema; the fixture accounts
// table already carries kyc_tier, and kyc_tier_enum is declared locally
// because the fixture does not apply migration 003.
package funding

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/risk"
)

// itCapSchema builds the shared fixture plus the risk_limits schema the
// withdrawal-cap enforcement reads.
func itCapSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	itSchema(t, ctx, pool)
	// kyc_tier_enum is owned by migration 003, which the funding fixture
	// does not apply (fixture accounts carries VARCHAR kyc_tier) —
	// declare the enum so the verbatim 011 migration applies.
	if _, err := pool.Exec(ctx,
		`CREATE TYPE kyc_tier_enum AS ENUM ('T0','T1','T2')`); err != nil {
		t.Fatalf("kyc_tier_enum fixture: %v", err)
	}
	for _, m := range []string{
		"011_create_risk_limits.up.sql",
		"047_otr_limits.up.sql",
		"109_risk_limits_exposure.up.sql",
		"272_risk_limits_exchange_daily_cap.up.sql",
	} {
		execSQLFile(t, ctx, pool, m)
	}
}

// TestITWithdrawalCaps — the full create path enforces the hourly-rate
// window and the venue-wide daily ceiling with the canonical
// ORDER_REJECTED code, on real limits + real withdrawal rows.
func TestITWithdrawalCaps(t *testing.T) {
	ctx, pool, _ := itPool(t)
	itCapSchema(t, ctx, pool)
	led := itLedger(t, ctx, pool)
	itSeedDeposit(t, ctx, led, 1, "USD", "100000")
	itSeedDeposit(t, ctx, led, 2, "USD", "100000")
	store := NewPgStore(pool)
	freeze := itFreeze(t, pool)

	// Caps: account 1 hourly 1000; venue ceiling 7000 on the global row.
	if _, err := pool.Exec(ctx, `
		INSERT INTO risk_limits (account_id, withdraw_rate_per_hour)
		VALUES (1, 1000)`); err != nil {
		t.Fatalf("hourly row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO risk_limits (exchange_daily_withdraw_limit)
		VALUES (7000)`); err != nil {
		t.Fatalf("venue row: %v", err)
	}

	limits := risk.NewLimitsService(risk.NewPgStore(pool), nil, nil)
	if err := limits.Load(ctx); err != nil {
		t.Fatalf("limits load: %v", err)
	}
	svc, err := NewWithdrawalService(store, ledgerPosterAdapter{led}, freeze)
	if err != nil {
		t.Fatalf("withdrawal svc: %v", err)
	}
	svc.WithLimits(limits).WithUSDConverter(usdIdentity{})

	mkWithdrawal := func(acct int64, amount, key string) error {
		_, err := svc.Create(ctx, CreateWithdrawalRequest{
			AccountID: acct, UserID: 100, Currency: "USD",
			Amount: amount, ReferenceAccount: "IBAN-" + key,
			IdempotencyKey: key,
		})
		return err
	}

	// 600 in → hourly 600/1000, venue 600/7000: PENDING row created.
	if err := mkWithdrawal(1, "600", "cap-1"); err != nil {
		t.Fatalf("first withdrawal: %v", err)
	}
	// 600 + 500 > 1000 hourly → ORDER_REJECTED, and no PENDING row.
	requireErrCode(t, mkWithdrawal(1, "500", "cap-2"), "ORDER_REJECTED")
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM funding_transactions
		WHERE idempotency_key = 'cap-2'`).Scan(&n); err != nil {
		t.Fatalf("rejected-row probe: %v", err)
	}
	if n != 0 {
		t.Fatal("cap rejection must not persist a withdrawal row")
	}
	// 600 + 400 == 1000 → boundary passes.
	if err := mkWithdrawal(1, "400", "cap-3"); err != nil {
		t.Fatalf("hourly boundary must pass: %v", err)
	}
	// The window rolls: backdating the first withdrawal out of the last
	// hour frees the cap — a further 400 for account 1 is allowed.
	if _, err := pool.Exec(ctx, `
		UPDATE funding_transactions SET created_at = $1
		WHERE idempotency_key = 'cap-1'`, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := mkWithdrawal(1, "600", "cap-4"); err != nil {
		t.Fatalf("post-window withdrawal must pass: %v", err)
	}

	// Venue ceiling: the cap-1 backdate (-2h) only stays inside the
	// current UTC day for runs after 02:00 UTC — earlier runs shift it
	// to yesterday, changing the venue sum. Pin the global limit to the
	// actual in-day sum + 5400 so cap-5 lands exactly on the boundary
	// either way. Account 2 has no hourly cap.
	if _, err := pool.Exec(ctx, `
		UPDATE risk_limits SET exchange_daily_withdraw_limit = (
			SELECT COALESCE(SUM(amount), 0) + 5400
			FROM funding_transactions
			WHERE type = 'WITHDRAWAL'
			  AND status IN ('PENDING','CONFIRMED','PENDING_REVIEW','COMPLETED')
			  AND created_at >= $1
		)
		WHERE account_id IS NULL AND tier IS NULL
		  AND (symbol IS NULL OR symbol = '*')`,
		time.Now().UTC().Truncate(24*time.Hour)); err != nil {
		t.Fatalf("venue limit pin: %v", err)
	}
	if err := limits.Load(ctx); err != nil {
		t.Fatalf("limits reload: %v", err)
	}
	if err := mkWithdrawal(2, "5400", "cap-5"); err != nil {
		t.Fatalf("venue boundary must pass: %v", err)
	}
	// day-sum + 1 > limit → ORDER_REJECTED for ANY account.
	requireErrCode(t, mkWithdrawal(2, "1", "cap-6"), "ORDER_REJECTED")
	requireErrCode(t, mkWithdrawal(1, "1", "cap-7"), "ORDER_REJECTED")
}
