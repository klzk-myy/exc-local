// Phase-14 Task 14.3.10 — compliance hold workflow integration tests
// against the dev schema (migration 215 applied).
//
// Gated: EXC_PG_TEST=1. Targets the dev database (localhost:5433 via
// EXC_PG_DSN) — the scratch-schema itPool helper serves the KYC store
// tests; this workflow needs the wider anchor set (accounts, freeze
// events, audit log) that the dev database already carries.
package compliance

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func holdPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run postgres-gated tests")
	}
	dsn := os.Getenv("EXC_PG_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@localhost:5433/exchange?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// holdSeed inserts a throwaway user + ACTIVE margin account.
func holdSeed(t *testing.T, pool *pgxpool.Pool) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status)
		 VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("hold_it_%d@example.com", time.Now().UnixNano())).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (user_id, account_type, kyc_tier)
		 VALUES ($1,'MARGIN','T1') RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c,
			`DELETE FROM compliance_holds WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c,
			`DELETE FROM account_freeze_events WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c,
			`DELETE FROM admin_audit_log WHERE target_type='account'
			   AND target_id=$1`, aid)
		_, _ = pool.Exec(c,
			`DELETE FROM orders WHERE account_id=$1`, aid)
		_, _ = pool.Exec(c, `DELETE FROM accounts WHERE id=$1`, aid)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id=$1`, uid)
	})
	return uid, aid
}

// fakes ----------------------------------------------------------------------

type fakeRestingCanceller struct {
	calls int
	err   error
}

func (c *fakeRestingCanceller) CancelResting(ctx context.Context,
	accountID int64, reason string) (int, error) {
	c.calls++
	if c.err != nil {
		return 0, c.err
	}
	return 3, nil
}

type fakeHoldAlerter struct {
	alerts []HoldAlert
}

func (a *fakeHoldAlerter) RaiseHold(ctx context.Context, al HoldAlert) error {
	a.alerts = append(a.alerts, al)
	return nil
}

type fakeClosureEscalation struct {
	calls     int
	accountID int64
	reason    string
	err       error
}

func (e *fakeClosureEscalation) RequestForcedClosure(ctx context.Context,
	accountID int64, reason string, requestedBy int64) (int64, error) {
	e.calls++
	e.accountID, e.reason = accountID, reason
	if e.err != nil {
		return 0, e.err
	}
	return 4242, nil
}

func officerRole(ctx context.Context, userID int64) (string, error) {
	return "Compliance Officer", nil
}

func auditorRole(ctx context.Context, userID int64) (string, error) {
	return "Read-Only Auditor", nil
}

func holdSvc(pool *pgxpool.Pool, c RestingCanceller, a *fakeHoldAlerter,
	esc ClosureEscalation, resolver HoldRoleResolver) *HoldService {
	return NewHoldService(pool, c, a, esc, resolver, nil, nil)
}

// ---------------------------------------------------------------------------
// Placement — freeze + resting-order drain + durable row + audit
// ---------------------------------------------------------------------------

func TestIntegrationHoldPlaceFreezesAccount(t *testing.T) {
	pool := holdPool(t)
	uid, aid := holdSeed(t, pool)
	ctx := context.Background()
	canc := &fakeRestingCanceller{}
	alerts := &fakeHoldAlerter{}
	svc := holdSvc(pool, canc, alerts, nil, HoldRoleResolver(officerRole))

	h, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual,
		Reason: "structuring pattern", EvidenceRef: "CASE-9",
		PlacedBy: 77,
	})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if h.Status != HoldStatusOpen || h.AccountStatus != "FROZEN" {
		t.Fatalf("hold wrong: %+v", h)
	}
	// Default SLA = 24h.
	want := time.Now().UTC().Add(24 * time.Hour)
	if h.SLADeadline.Before(want.Add(-time.Hour)) ||
		h.SLADeadline.After(want.Add(time.Hour)) {
		t.Fatalf("SLA deadline %v not ~24h", h.SLADeadline)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id=$1`, aid).Scan(&status); err != nil ||
		status != "FROZEN" {
		t.Fatalf("account not frozen: %s", status)
	}
	// Freeze event + audit landed in the same transaction.
	var reason string
	if err := pool.QueryRow(ctx,
		`SELECT reason FROM account_freeze_events
		  WHERE account_id=$1 AND action='FREEZE'`, aid).
		Scan(&reason); err != nil || reason != "COMPLIANCE_HOLD" {
		t.Fatalf("freeze event: %v %q", err, reason)
	}
	var auditN int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log
		  WHERE target_id=$1 AND action='compliance.hold_place'`, aid).
		Scan(&auditN); err != nil || auditN != 1 {
		t.Fatalf("audit rows=%d", auditN)
	}
	if canc.calls != 1 {
		t.Fatalf("resting cancels=%d, want 1", canc.calls)
	}
	_ = uid
}

// A high-confidence SANCTIONS_HIT (the Phase-21 machine trigger) takes
// the 4h SLA; a stacking second hold on an already-FROZEN account is
// legal and does not emit a second freeze event.
func TestIntegrationHoldSanctionsSLAAndStack(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	ctx := context.Background()
	canc := &fakeRestingCanceller{}
	svc := holdSvc(pool, canc, &fakeHoldAlerter{}, nil,
		HoldRoleResolver(officerRole))

	if _, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual, Reason: "first"}); err != nil {
		t.Fatalf("first hold: %v", err)
	}
	h2, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerSanctionsHit,
		Reason: "screening hit", HighConfidence: true, PlacedBy: 5})
	if err != nil {
		t.Fatalf("sanctions hold: %v", err)
	}
	want := time.Now().UTC().Add(4 * time.Hour)
	if h2.SLADeadline.Before(want.Add(-time.Hour)) ||
		h2.SLADeadline.After(want.Add(time.Hour)) {
		t.Fatalf("sanctions SLA %v not ~4h", h2.SLADeadline)
	}
	var evN int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM account_freeze_events
		  WHERE account_id=$1`, aid).Scan(&evN); err != nil || evN != 1 {
		t.Fatalf("freeze events=%d, want 1 (stacked hold idempotent)", evN)
	}
}

// ---------------------------------------------------------------------------
// Release — four-eyes + last-hold-standing unfreeze
// ---------------------------------------------------------------------------

func TestIntegrationHoldRelease(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	ctx := context.Background()
	svc := holdSvc(pool, &fakeRestingCanceller{}, &fakeHoldAlerter{},
		nil, HoldRoleResolver(officerRole))

	h, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual, Reason: "review"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}

	// Self-approval rejects (four-eyes).
	if _, err := svc.Release(ctx, h.HoldID, 7, 7, "false positive"); err == nil ||
		codeOf(t, err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("self-approve: got %v", err)
	}
	// Missing reason rejects.
	if _, err := svc.Release(ctx, h.HoldID, 7, 8, ""); err == nil ||
		codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("empty reason: got %v", err)
	}

	rel, err := svc.Release(ctx, h.HoldID, 7, 8, "false positive")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if rel.Status != HoldStatusReleased || rel.AccountStatus != "ACTIVE" {
		t.Fatalf("release wrong: %+v", rel)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id=$1`, aid).Scan(&status); err != nil ||
		status != "ACTIVE" {
		t.Fatalf("unfreeze: %s", status)
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM account_freeze_events
		  WHERE account_id=$1 AND action='UNFREEZE'`, aid).Scan(&n); err != nil ||
		n != 1 {
		t.Fatalf("unfreeze events=%d", n)
	}

	// A non-OPEN hold rejects further disposition.
	if _, err := svc.Release(ctx, h.HoldID, 7, 8, "again"); err == nil ||
		codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("re-release: got %v", err)
	}
}

// Role gate: a Read-Only Auditor cannot place/release/escalate.
func TestIntegrationHoldRoleGate(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	ctx := context.Background()
	svc := holdSvc(pool, &fakeRestingCanceller{}, &fakeHoldAlerter{},
		nil, HoldRoleResolver(auditorRole))
	h, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual, Reason: "r"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if _, err := svc.Release(ctx, h.HoldID, 1, 2, "x"); err == nil ||
		codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("auditor release: got %v", err)
	}
	if _, err := svc.EscalateSAR(ctx, h.HoldID, 1, "x"); err == nil ||
		codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("auditor escalate: got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Escalations
// ---------------------------------------------------------------------------

func TestIntegrationHoldEscalateSAR(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	ctx := context.Background()
	svc := holdSvc(pool, &fakeRestingCanceller{}, &fakeHoldAlerter{},
		nil, HoldRoleResolver(officerRole))
	h, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual, Reason: "r"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	esc, err := svc.EscalateSAR(ctx, h.HoldID, 9, "confirmed structuring")
	if err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if esc.Status != HoldStatusEscalatedSAR {
		t.Fatalf("status=%s", esc.Status)
	}
	// Account stays FROZEN — SAR escalation never auto-releases.
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id=$1`, aid).Scan(&status); err != nil ||
		status != "FROZEN" {
		t.Fatalf("account must stay FROZEN: %s", status)
	}
}

func TestIntegrationHoldEscalateToClosure(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	ctx := context.Background()
	esc := &fakeClosureEscalation{}
	svc := holdSvc(pool, &fakeRestingCanceller{}, &fakeHoldAlerter{},
		esc, HoldRoleResolver(officerRole))
	h, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual, Reason: "r"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	out, reqID, err := svc.EscalateToClosure(ctx, h.HoldID, 9, "suspected fraud")
	if err != nil {
		t.Fatalf("escalate-to-closure: %v", err)
	}
	if out.Status != HoldStatusEscalatedClosure || reqID != 4242 {
		t.Fatalf("result wrong: %+v req=%d", out, reqID)
	}
	if esc.calls != 1 || esc.accountID != aid {
		t.Fatalf("closure seam: %+v", esc)
	}
}

// ---------------------------------------------------------------------------
// SLA sweep — breach marking + P1 alert
// ---------------------------------------------------------------------------

func TestIntegrationHoldSLASweep(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	ctx := context.Background()
	alerts := &fakeHoldAlerter{}
	svc := holdSvc(pool, &fakeRestingCanceller{}, alerts, nil,
		HoldRoleResolver(officerRole))
	h, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual, Reason: "r",
		SLAHours: 1})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	// Force the deadline into the past, then sweep.
	if _, err := pool.Exec(ctx,
		`UPDATE compliance_holds SET sla_deadline = now() - interval '1m'
		  WHERE hold_id=$1`, h.HoldID); err != nil {
		t.Fatalf("age deadline: %v", err)
	}
	n, err := svc.SweepSLA(ctx, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept=%d, want 1", n)
	}
	var breached bool
	if err := pool.QueryRow(ctx,
		`SELECT sla_breached FROM compliance_holds
		  WHERE hold_id=$1`, h.HoldID).Scan(&breached); err != nil || !breached {
		t.Fatalf("breach flag: %v %v", breached, err)
	}
	if len(alerts.alerts) != 1 || alerts.alerts[0].Code != "HOLD_SLA_BREACHED" {
		t.Fatalf("P1 alert missing: %+v", alerts.alerts)
	}
	// Idempotent — the flagged hold doesn't re-alert.
	if n, _ := svc.SweepSLA(ctx, 100); n != 0 {
		t.Fatalf("re-sweep alerted %d more", n)
	}
}

// Canceller outage: the hold still commits (legal freeze stands) and
// the failure pages P1.
func TestIntegrationHoldCancelPartialFailure(t *testing.T) {
	pool := holdPool(t)
	_, aid := holdSeed(t, pool)
	ctx := context.Background()
	canc := &fakeRestingCanceller{err: stderrors.New("engine down")}
	alerts := &fakeHoldAlerter{}
	svc := holdSvc(pool, canc, alerts, nil, HoldRoleResolver(officerRole))
	h, err := svc.PlaceHold(ctx, PlaceHoldRequest{
		AccountID: aid, Trigger: HoldTriggerManual, Reason: "r"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if h.Status != HoldStatusOpen {
		t.Fatalf("hold dropped on cancel outage: %+v", h)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id=$1`, aid).Scan(&status); err != nil ||
		status != "FROZEN" {
		t.Fatalf("legal hold must stand: %s", status)
	}
	if canc.calls != 3 {
		t.Fatalf("retry budget=%d, want 3", canc.calls)
	}
	if len(alerts.alerts) != 1 || alerts.alerts[0].Code != "HOLD_CANCEL_FAILED" {
		t.Fatalf("P1 alert missing: %+v", alerts.alerts)
	}
}
