// Integration tests for the Phase-14 product-governance cluster
// (Tasks 14.3.13/14.3.15/14.3.16) against dev PostgreSQL.
//
// Gated: skipped unless EXC_PG_TEST=1 (see integration_test.go). Requires
// migrations 003/004/014/017/019 + 095/099 (+215 compliance_holds for the
// abuse-guard flag) applied.
//
// Run: EXC_PG_TEST=1 go test ./internal/accounts/ -run 'IntegrationProduct|IntegrationSwapfree|IntegrationTargetMarket' -v
package accounts

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/settlement"
)

// complianceResolver always answers Compliance Officer — the service
// role checks exercise through it.
func complianceResolver() RoleResolver {
	return func(context.Context, int64) (string, error) {
		return RoleComplianceOfficer, nil
	}
}

// supportResolver answers a non-privileged role for negative tests.
func supportResolver() RoleResolver {
	return func(context.Context, int64) (string, error) {
		return "Support Agent", nil
	}
}

// fakeCategorizer returns a fixed client category.
type fakeCategorizer struct{ cat string }

func (f fakeCategorizer) Category(context.Context, int64) (string, error) {
	return f.cat, nil
}

// captureAlerter records raised ops alerts.
type captureAlerter struct{ alerts []settlement.OpsAlert }

func (a *captureAlerter) Raise(_ context.Context, al settlement.OpsAlert) error {
	a.alerts = append(a.alerts, al)
	return nil
}

// productCleanup removes the rows these tests add on top of
// lifecycleCleanup (verification queue, audit rows for the new target
// types, ad-hoc profiles/targets).
func productCleanup(t *testing.T, pool *pgxpool.Pool, aid int64, codes ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = pool.Exec(ctx,
		`DELETE FROM swapfree_verifications WHERE account_id=$1`, aid)
	_, _ = pool.Exec(ctx,
		`DELETE FROM compliance_holds WHERE account_id=$1`, aid)
	_, _ = pool.Exec(ctx,
		`DELETE FROM admin_audit_log WHERE target_type='account' AND target_id=$1`, aid)
	for _, code := range codes {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE target_type='product_target_market'
			   AND target_id IN (SELECT id FROM product_target_markets
			     WHERE profile_id IN (SELECT profile_id FROM account_product_profiles
			       WHERE code=$1))`, code)
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE target_type='account_product_profile'
			   AND target_id IN (SELECT profile_id FROM account_product_profiles
			     WHERE code=$1)`, code)
		_, _ = pool.Exec(ctx,
			`DELETE FROM product_target_markets WHERE profile_id IN
			 (SELECT profile_id FROM account_product_profiles WHERE code=$1)`, code)
		_, _ = pool.Exec(ctx,
			`DELETE FROM account_product_profiles WHERE code=$1`, code)
	}
}

// newTestProfile inserts a profile row directly (dual-control executor
// paths are exercised via ApplyCreateTx in TestIntegrationProductCreateTx).
func newTestProfile(t *testing.T, pool *pgxpool.Pool, code string,
	scope []string, divisor int, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO account_product_profiles
		    (code, pricing_plan, instrument_scope, subunit_divisor, status)
		VALUES ($1,'SPREAD_MARKUP',$2,$3,$4) RETURNING profile_id`,
		code, scope, divisor, status).Scan(&id); err != nil {
		t.Fatalf("seed profile %s: %v", code, err)
	}
	return id
}

func TestIntegrationProductAssignPreconditions(t *testing.T) {
	pool := testPool(t)
	_, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	productCleanup(t, pool, aid, "TST_SPOTONLY", "TST_RET")
	ctx := context.Background()
	actor := AdminActor{UserID: 9001}
	svc := NewProfileService(pool, complianceResolver())

	// Onboarding default: the migration-095 column default lands
	// STANDARD (profile_id 1) without a registration-path change.
	prof, err := svc.AccountProfile(ctx, aid)
	if err != nil {
		t.Fatalf("account profile: %v", err)
	}
	if prof.Code != "STANDARD" || prof.ProfileID != 1 || prof.SubunitDivisor != 1 {
		t.Fatalf("default profile wrong: %+v", prof)
	}

	spotID := newTestProfile(t, pool, "TST_SPOTONLY", []string{"SPOT"}, 1, "ACTIVE")
	retID := newTestProfile(t, pool, "TST_RET", []string{"SPOT"}, 1, "RETIRED")

	// Role gate: a Support Agent cannot assign.
	support := NewProfileService(pool, supportResolver())
	if _, err := support.AssignProfile(ctx, actor, aid, "TST_SPOTONLY"); err == nil {
		t.Fatal("non-privileged assignment succeeded")
	} else {
		requireCode(t, err, "UNAUTHORIZED_ROLE")
	}

	// Same-profile re-assignment rejects.
	if _, err := svc.AssignProfile(ctx, actor, aid, "STANDARD"); err == nil {
		t.Fatal("same-profile re-assignment succeeded")
	}

	// Retired profile takes no new assignment.
	if _, err := svc.AssignProfile(ctx, actor, aid, "TST_RET"); err == nil {
		t.Fatal("retired profile accepted new assignment")
	} else {
		requireCode(t, err, "INVALID_REQUEST")
	}

	// Resting order blocks the switch.
	inst := leveredInst(t, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (account_id, instrument_id, side, order_type,
		     quantity, price, time_in_force, status)
		 VALUES ($1,$2,'BUY','LIMIT',1000,1.05,'GTC','ACTIVE')`, aid, inst); err != nil {
		t.Fatalf("seed resting order: %v", err)
	}
	if _, err := svc.AssignProfile(ctx, actor, aid, "TST_SPOTONLY"); err == nil {
		t.Fatal("assignment succeeded with a resting order")
	} else {
		requireCode(t, err, "INVALID_REQUEST")
	}
	_, _ = pool.Exec(ctx, `DELETE FROM orders WHERE account_id=$1`, aid)

	// Open position blocks the switch.
	if _, err := pool.Exec(ctx,
		`INSERT INTO positions (account_id, instrument_id, side, quantity,
		     entry_price) VALUES ($1,$2,'LONG',5000,1.05)`, aid, inst); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	if _, err := svc.AssignProfile(ctx, actor, aid, "TST_SPOTONLY"); err == nil {
		t.Fatal("assignment succeeded with an open position")
	} else {
		requireCode(t, err, "INVALID_REQUEST")
	}
	_, _ = pool.Exec(ctx, `DELETE FROM positions WHERE account_id=$1`, aid)

	// Pending settlement blocks the switch.
	if _, err := pool.Exec(ctx,
		`INSERT INTO settlement_instructions
		     (trade_id, account_id, currency, amount, direction, status)
		 VALUES (999999001,$1,'USD',100,'PAY','PENDING')`, aid); err != nil {
		t.Fatalf("seed settlement: %v", err)
	}
	if _, err := svc.AssignProfile(ctx, actor, aid, "TST_SPOTONLY"); err == nil {
		t.Fatal("assignment succeeded with a pending settlement")
	} else {
		requireCode(t, err, "INVALID_REQUEST")
	}
	_, _ = pool.Exec(ctx,
		`DELETE FROM settlement_instructions WHERE account_id=$1`, aid)

	// Clean switch to the same-divisor profile succeeds.
	got, err := svc.AssignProfile(ctx, actor, aid, "TST_SPOTONLY")
	if err != nil {
		t.Fatalf("clean assignment failed: %v", err)
	}
	if got.ProfileID != spotID {
		t.Fatalf("assigned profile wrong: %+v", got)
	}

	// Retired-profile grandfathering: force the account onto the retired
	// profile — resolution still works for existing holders.
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET product_profile_id=$2 WHERE id=$1`, aid, retID); err != nil {
		t.Fatalf("force retired profile: %v", err)
	}
	prof, err = svc.AccountProfile(ctx, aid)
	if err != nil || prof.Code != "TST_RET" {
		t.Fatalf("grandfathered read failed: %v %+v", err, prof)
	}
	// Restore STANDARD for subsequent subtests.
	_, _ = pool.Exec(ctx,
		`UPDATE accounts SET product_profile_id=1 WHERE id=$1`, aid)
}

func TestIntegrationProductDivisorSwitchZeroBalance(t *testing.T) {
	pool := testPool(t)
	_, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	productCleanup(t, pool, aid)
	ctx := context.Background()
	svc := NewProfileService(pool, complianceResolver())
	actor := AdminActor{UserID: 9001}

	// A divisor change (STANDARD→CENT) requires every balance row zero.
	if _, err := pool.Exec(ctx,
		`INSERT INTO balances (account_id, currency, available, locked)
		 VALUES ($1,'USD',10,0)`, aid); err != nil {
		t.Fatalf("seed balance: %v", err)
	}
	if _, err := svc.AssignProfile(ctx, actor, aid, "CENT"); err == nil {
		t.Fatal("divisor-changing switch succeeded with non-zero balance")
	} else {
		requireCode(t, err, "INVALID_REQUEST")
	}
	_, _ = pool.Exec(ctx, `DELETE FROM balances WHERE account_id=$1`, aid)

	if _, err := svc.AssignProfile(ctx, actor, aid, "CENT"); err != nil {
		t.Fatalf("zero-balance CENT switch failed: %v", err)
	}
	prof, _ := svc.AccountProfile(ctx, aid)
	if prof.Code != "CENT" || prof.SubunitDivisor != 100 {
		t.Fatalf("CENT switch wrong: %+v", prof)
	}
}

func TestIntegrationProductCreateTx(t *testing.T) {
	pool := testPool(t)
	productCleanup(t, pool, 0, "TST_DUAL")
	ctx := context.Background()
	svc := NewProfileService(pool, complianceResolver())

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	p, err := svc.ApplyCreateTx(ctx, tx, 9001, ProfileInput{
		Code: "tst_dual", PricingPlan: "raw_spread_commission",
		InstrumentScope: []string{"SPOT", "NDF"},
		SubunitDivisor:  1, MinDeposit: "250",
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("ApplyCreateTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if p.Code != "TST_DUAL" || p.PricingPlan != "RAW_SPREAD_COMMISSION" {
		t.Fatalf("created profile wrong: %+v", p)
	}
	// The audit row landed in the same transaction.
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log
		  WHERE action='product_profile.create' AND target_id=$1`,
		p.ProfileID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit row missing: n=%d err=%v", n, err)
	}
	// Duplicate code rejects INVALID_REQUEST.
	tx2, _ := pool.Begin(ctx)
	_, err = svc.ApplyCreateTx(ctx, tx2, 9001, ProfileInput{
		Code: "TST_DUAL", PricingPlan: "SPREAD_MARKUP",
		InstrumentScope: []string{"SPOT"}}, "")
	if err == nil {
		t.Fatal("duplicate code accepted")
	} else {
		requireCode(t, err, "INVALID_REQUEST")
	}
	_ = tx2.Rollback(ctx)
}

func TestIntegrationSwapfreeLifecycle(t *testing.T) {
	pool := testPool(t)
	_, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	productCleanup(t, pool, aid)
	ctx := context.Background()
	svc := NewSwapfreeService(pool, complianceResolver(), PgxHoldPlacer{})
	actor := AdminActor{UserID: 9001}

	// No attestation → INVALID_REQUEST; unresolvable ref → INVALID_REQUEST.
	if _, err := svc.Request(ctx, aid, ""); err == nil {
		t.Fatal("request without attestation accepted")
	}
	if _, err := svc.Request(ctx, aid, "not-a-doc"); err == nil {
		t.Fatal("unresolvable attestation accepted")
	} else {
		requireCode(t, err, "INVALID_REQUEST")
	}

	// Seed a kyc_documents row owned by the account as the attestation.
	var docID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO kyc_documents (account_id, type, file_url)
		 VALUES ($1,'attestation','s3://att/self.pdf') RETURNING id`,
		aid).Scan(&docID); err != nil {
		t.Fatalf("seed kyc doc: %v", err)
	}
	t.Cleanup(func() {
		c2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c2, `DELETE FROM kyc_documents WHERE id=$1`, docID)
	})

	v, err := svc.Request(ctx, aid, fmt.Sprint(docID))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if v.Status != SwapPending {
		t.Fatalf("request status wrong: %+v", v)
	}
	var acctStatus string
	if err := pool.QueryRow(ctx,
		`SELECT swapfree_status FROM accounts WHERE id=$1`, aid).
		Scan(&acctStatus); err != nil || acctStatus != AcctSwapPending {
		t.Fatalf("account not PENDING: %q %v", acctStatus, err)
	}
	// A second live request rejects.
	if _, err := svc.Request(ctx, aid, fmt.Sprint(docID)); err == nil {
		t.Fatal("second live request accepted")
	}

	// Approve → row APPROVED, account VERIFIED, seam true.
	v, err = svc.Approve(ctx, actor, v.ID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if v.Status != SwapApproved {
		t.Fatalf("approve status wrong: %+v", v)
	}
	ok, err := svc.IsSwapFreeVerified(ctx, aid)
	if err != nil || !ok {
		t.Fatalf("IsSwapFreeVerified: %v %v", ok, err)
	}

	// Reject path on a fresh account for coverage.
	_, aid2 := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid2)
	productCleanup(t, pool, aid2)
	var doc2 int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO kyc_documents (account_id, type, file_url)
		 VALUES ($1,'attestation','s3://att/reject.pdf') RETURNING id`,
		aid2).Scan(&doc2); err != nil {
		t.Fatalf("seed doc2: %v", err)
	}
	t.Cleanup(func() {
		c2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c2, `DELETE FROM kyc_documents WHERE id=$1`, doc2)
	})
	v2, _ := svc.Request(ctx, aid2, fmt.Sprint(doc2))
	if _, err := svc.Reject(ctx, actor, v2.ID, "insufficient evidence"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT swapfree_status FROM accounts WHERE id=$1`, aid2).
		Scan(&acctStatus); err != nil || acctStatus != AcctSwapStandard {
		t.Fatalf("rejected account not STANDARD: %q %v", acctStatus, err)
	}

	// Revocation abuse guard: cycle request→approve→revoke thrice on the
	// first account; the 3rd revocation exceeds the 12-month ceiling and
	// must flag a compliance_holds review row in the same transaction.
	var last *SwapfreeVerification
	last = v // currently APPROVED
	for i := 0; i < 3; i++ {
		if i > 0 {
			last, err = svc.Request(ctx, aid, fmt.Sprint(docID))
			if err != nil {
				t.Fatalf("re-request %d: %v", i, err)
			}
			if _, err := svc.Approve(ctx, actor, last.ID); err != nil {
				t.Fatalf("re-approve %d: %v", i, err)
			}
		}
		if _, err := svc.Revoke(ctx, actor, last.ID, "terms breached"); err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}
	var holds int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM compliance_holds
		  WHERE account_id=$1 AND trigger_source='UNUSUAL_ACTIVITY'`,
		aid).Scan(&holds); err != nil {
		t.Fatalf("holds read: %v", err)
	}
	if holds != 1 {
		t.Fatalf("abuse guard: expected 1 compliance hold, got %d", holds)
	}
	// REVOKED resumes accrual: the seam reports false — the rollover
	// consumes accounts.swapfree_status='REVOKED' directly (prospective
	// only; no back-billing row exists anywhere in this path).
	ok, _ = svc.IsSwapFreeVerified(ctx, aid)
	if ok {
		t.Fatal("REVOKED account still reads swap-free")
	}
}

func TestIntegrationTargetMarketGate(t *testing.T) {
	pool := testPool(t)
	_, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	productCleanup(t, pool, aid, "TST_GATE")
	ctx := context.Background()

	pid := newTestProfile(t, pool, "TST_GATE",
		[]string{"SPOT", "FORWARD", "OPTION"}, 1, "ACTIVE")
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET product_profile_id=$2 WHERE id=$1`, aid, pid); err != nil {
		t.Fatalf("force gate profile: %v", err)
	}
	// Seed the profile's RETAIL row: SPOT+FORWARD positive, OPTION
	// negative, fresh review horizon.
	if _, err := pool.Exec(ctx, `
		INSERT INTO product_target_markets
		    (profile_id, client_category, positive_classes,
		     negative_classes, review_due_at)
		VALUES ($1,'RETAIL','{SPOT,FORWARD}','{OPTION}', now() + interval '6 months')`,
		pid); err != nil {
		t.Fatalf("seed target market: %v", err)
	}

	profiles := NewProfileService(pool, complianceResolver())
	targets := NewTargetMarketService(pool, complianceResolver(), &captureAlerter{})
	gate := NewProductGateService(profiles, targets, fakeCategorizer{"RETAIL"})

	// In-target open admits.
	if err := gate.AdmitOrder(ctx, aid, "EUR/USD", "SPOT", false); err != nil {
		t.Fatalf("in-target SPOT open rejected: %v", err)
	}
	// Out-of-scope open rejects (class absent from instrument_scope).
	if err := gate.AdmitOrder(ctx, aid, "USDJPY.SWP", "SWAP", false); err == nil {
		t.Fatal("out-of-scope SWAP open admitted")
	} else {
		requireCode(t, err, "PRODUCT_NOT_PERMITTED")
	}
	// Negative-target open rejects.
	if err := gate.AdmitOrder(ctx, aid, "EURUSD.OPT", "OPTION", false); err == nil {
		t.Fatal("negative-target OPTION open admitted")
	} else {
		requireCode(t, err, "PRODUCT_NOT_PERMITTED")
	}
	// Missing positive class rejects.
	if err := gate.AdmitOrder(ctx, aid, "USDNOK.NDF", "NDF", false); err == nil {
		t.Fatal("non-positive NDF open admitted")
	} else {
		requireCode(t, err, "PRODUCT_NOT_PERMITTED")
	}
	// Non-retail categories skip the target check (PROFESSIONAL
	// completeness rows carry the full class set but the gate does not
	// consult them for admission).
	gatePro := NewProductGateService(profiles, targets, fakeCategorizer{"PROFESSIONAL"})
	if err := gatePro.AdmitOrder(ctx, aid, "EURUSD.OPT", "OPTION", false); err != nil {
		t.Fatalf("professional admission blocked by retail gate: %v", err)
	}
	// reduce_only stays admissible on negative-target instruments —
	// a client can always flatten.
	if err := gate.AdmitOrder(ctx, aid, "EURUSD.OPT", "OPTION", true); err != nil {
		t.Fatalf("reduce-only exit rejected: %v", err)
	}
}

func TestIntegrationTargetMarketOverdue(t *testing.T) {
	pool := testPool(t)
	_, aid := seedMaster(t, pool, "T1")
	lifecycleCleanup(t, pool, aid)
	productCleanup(t, pool, aid, "TST_OD")
	ctx := context.Background()

	pid := newTestProfile(t, pool, "TST_OD", []string{"SPOT", "FORWARD"}, 1, "ACTIVE")
	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET product_profile_id=$2 WHERE id=$1`, aid, pid); err != nil {
		t.Fatalf("force profile: %v", err)
	}
	// Past-due APPROVED row: the gate must fail closed for retail opens
	// BEFORE the sweeper even runs (lazy overdue evaluation).
	if _, err := pool.Exec(ctx, `
		INSERT INTO product_target_markets
		    (profile_id, client_category, positive_classes,
		     negative_classes, review_due_at)
		VALUES ($1,'RETAIL','{SPOT,FORWARD}','{}', now() - interval '1 hour')`,
		pid); err != nil {
		t.Fatalf("seed overdue target market: %v", err)
	}
	profiles := NewProfileService(pool, complianceResolver())
	alerter := &captureAlerter{}
	targets := NewTargetMarketService(pool, complianceResolver(), alerter)
	gate := NewProductGateService(profiles, targets, fakeCategorizer{"RETAIL"})

	if err := gate.AdmitOrder(ctx, aid, "EUR/USD", "SPOT", false); err == nil {
		t.Fatal("overdue review admitted a retail open")
	} else {
		requireCode(t, err, "PRODUCT_NOT_PERMITTED")
	}
	// Close-only: reduce-only legs still pass.
	if err := gate.AdmitOrder(ctx, aid, "EUR/USD", "SPOT", true); err != nil {
		t.Fatalf("overdue review blocked a reduce-only exit: %v", err)
	}

	// The sweeper flips the row and raises exactly one alert.
	n, err := targets.SweepOverdue(ctx, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n < 1 {
		t.Fatal("sweep flipped no rows")
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM product_target_markets
		  WHERE profile_id=$1 AND client_category='RETAIL'`, pid).
		Scan(&status); err != nil || status != "REVIEW_OVERDUE" {
		t.Fatalf("row not swept: %q %v", status, err)
	}
	found := false
	for _, al := range alerter.alerts {
		if al.Code == "TARGET_MARKET_REVIEW_OVERDUE" {
			found = true
		}
	}
	if !found {
		t.Fatal("overdue sweep raised no ops alert")
	}
	// Re-sweep alerts once only (alerted_at stamped).
	if _, err := targets.SweepOverdue(ctx, 100); err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	cnt := 0
	for _, al := range alerter.alerts {
		if al.Code == "TARGET_MARKET_REVIEW_OVERDUE" {
			cnt++
		}
	}
	if cnt != 1 {
		t.Fatalf("alert fired %d times, want 1", cnt)
	}

	// Re-approval via Review(APPROVE) rolls the horizon and re-admits.
	var tmID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM product_target_markets
		  WHERE profile_id=$1 AND client_category='RETAIL'`, pid).
		Scan(&tmID); err != nil {
		t.Fatalf("target id: %v", err)
	}
	if _, err := targets.Review(ctx, AdminActor{UserID: 9001}, tmID,
		ReviewApprove, &TargetMarketInput{
			PositiveClasses: []string{"SPOT"},
			ReviewDueAt:     time.Now().Add(180 * 24 * time.Hour).Format(time.RFC3339),
		}); err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if err := gate.AdmitOrder(ctx, aid, "EUR/USD", "SPOT", false); err != nil {
		t.Fatalf("post-review retail open rejected: %v", err)
	}
}
