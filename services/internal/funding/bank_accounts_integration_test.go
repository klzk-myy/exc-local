// PostgreSQL integration test for the Task 11.3.7 beneficiary registry
// (migration 040): schema round-trip + the register → verify → withdrawable
// lifecycle against the real table, including the 24h hold stamp.
//
// Gated on EXC_PG_TEST=1; targets EXC_TEST_DSN (see integration_test.go's
// itPool scratch-schema convention).
package funding

import (
	"testing"
	"time"
)

func TestITBankAccountsLifecycle(t *testing.T) {
	ctx, pool, _ := itPool(t)
	if _, err := pool.Exec(ctx, fixtureDDL); err != nil {
		t.Fatalf("fixture ddl: %v", err)
	}
	execSQLFile(t, ctx, pool, "040_bank_accounts.up.sql")

	// Seed one T1 ACTIVE account.
	if _, err := pool.Exec(ctx, `INSERT INTO users (id) VALUES (77001)`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, user_id, kyc_tier, status, base_currency)
		 VALUES (7701, 77001, 'T1', 'ACTIVE', 'USD')`); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	store := NewPgBankAccountStore(pool)
	svc, err := NewBankAccountService(store,
		roleOf(map[int64]string{10: "Finance Ops", 11: "Compliance Officer"}))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })

	iban := "DE89370400440532013000"
	b, err := svc.Register(ctx, 7701, benInput(iban))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if b.Status != BankAcctPending {
		t.Fatalf("status: %s", b.Status)
	}

	// Unverified destination refuses withdrawals.
	if err := svc.AssertWithdrawable(ctx, 7701, iban); codeOf(err) != "BANK_ACCOUNT_NOT_VERIFIED" {
		t.Fatalf("pending destination: %v", err)
	}

	// Dual-controlled verification; approver must differ.
	if _, err := svc.Verify(ctx, 10, 10, b.BankAccountID, "", ""); codeOf(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("self-approve must refuse: %v", err)
	}
	v, err := svc.Verify(ctx, 10, 11, b.BankAccountID, VerifyMethodBankStatement, "10.1.2.3")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Status != BankAcctVerified || v.VerifiedBy == nil || *v.VerifiedBy != 10 {
		t.Fatalf("verified row: %+v", v)
	}
	if v.UnlockedAt == nil || !v.UnlockedAt.After(now) {
		t.Fatalf("24h hold stamp missing: %+v", v.UnlockedAt)
	}

	// Inside the hold → BENEFICIARY_HOLD_ACTIVE.
	if err := svc.AssertWithdrawable(ctx, 7701, iban); codeOf(err) != "BENEFICIARY_HOLD_ACTIVE" {
		t.Fatalf("inside hold: %v", err)
	}
	// Past the hold → withdrawable.
	svc.WithClock(func() time.Time { return now.Add(25 * time.Hour) })
	if err := svc.AssertWithdrawable(ctx, 7701, iban); err != nil {
		t.Fatalf("hold-lapsed must pass: %v", err)
	}

	// The audit row landed in the same tx.
	var audits int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log
		 WHERE action = 'funding.beneficiary.verify' AND target_id = $1`,
		b.BankAccountID).Scan(&audits); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if audits != 1 {
		t.Fatalf("expected 1 verify audit row, got %d", audits)
	}

	// Reject path: second row PENDING → REJECTED with reason.
	b2, _ := svc.Register(ctx, 7701, benInput("FR1420041010050500013M02606"))
	r, err := svc.Reject(ctx, 10, b2.BankAccountID, "name mismatch", "")
	if err != nil || r.Status != BankAcctRejected {
		t.Fatalf("reject: %v %+v", err, r)
	}
	if err := svc.AssertWithdrawable(ctx, 7701, "FR1420041010050500013M02606"); codeOf(err) != "BANK_ACCOUNT_NOT_VERIFIED" {
		t.Fatalf("rejected destination: %v", err)
	}
}

// TestITBankAccountsMigrationRoundTrip proves 040 down removes the table
// and both enum types inside the scratch schema.
func TestITBankAccountsMigrationRoundTrip(t *testing.T) {
	ctx, pool, _ := itPool(t)
	if _, err := pool.Exec(ctx, fixtureDDL); err != nil {
		t.Fatalf("fixture ddl: %v", err)
	}
	execSQLFile(t, ctx, pool, "040_bank_accounts.up.sql")
	execSQLFile(t, ctx, pool, "040_bank_accounts.down.sql")

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name='bank_accounts'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bank_accounts survived down: n=%d err=%v", n, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_type t JOIN pg_namespace ns ON ns.oid = t.typnamespace
		WHERE ns.nspname = current_schema()
		  AND t.typname IN ('bank_account_status_enum','bank_account_rail_enum')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("040 enums survived down: n=%d err=%v", n, err)
	}
}
