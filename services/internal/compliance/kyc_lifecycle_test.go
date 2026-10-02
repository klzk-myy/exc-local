// Unit tests for Phase-14 Task 14.3.4 — LifecycleService with a fake
// LifecycleStore: role gate, approve/reject decision flow, reverify
// policy resolution and the hourly sweep's idempotent downgrade path.
package compliance

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLifecycleStore implements LifecycleStore for unit tests.
type fakeLifecycleStore struct {
	sub         *Submission
	subErr      error
	policy      *TierPolicy
	policyErr   error
	decideIn    []DecisionTx
	decideRes   *DecisionResult
	decideErr   error
	overdue     []OverdueReverify
	overdueErr  error
	downApplied bool
	downErr     error
	downIn      []DowngradeTx
	pending     []Submission
	pendingErr  error
}

func (f *fakeLifecycleStore) SubmissionByID(context.Context, int64) (*Submission, error) {
	return f.sub, f.subErr
}
func (f *fakeLifecycleStore) DecideSubmissionTx(_ context.Context, p DecisionTx) (*DecisionResult, error) {
	f.decideIn = append(f.decideIn, p)
	return f.decideRes, f.decideErr
}
func (f *fakeLifecycleStore) OverdueReverifications(context.Context, time.Time, int) ([]OverdueReverify, error) {
	return f.overdue, f.overdueErr
}
func (f *fakeLifecycleStore) DowngradeReverifyTx(_ context.Context, p DowngradeTx) (bool, error) {
	f.downIn = append(f.downIn, p)
	return f.downApplied, f.downErr
}
func (f *fakeLifecycleStore) PendingSubmissions(context.Context, int) ([]Submission, error) {
	return f.pending, f.pendingErr
}
func (f *fakeLifecycleStore) TierPolicy(context.Context, string) (*TierPolicy, error) {
	return f.policy, f.policyErr
}

type captureNotifier struct{ calls []string }

func (c *captureNotifier) Notify(_ context.Context, _ int64, event string, _ map[string]any) {
	c.calls = append(c.calls, event)
}

func lifecycleForTest(t *testing.T, store LifecycleStore, n *captureNotifier, alerts *[]string) *LifecycleService {
	t.Helper()
	var alerter Alerter
	if alerts != nil {
		alerter = func(_ context.Context, sev, code, summary string) error {
			*alerts = append(*alerts, code+":"+summary)
			return nil
		}
	}
	svc, err := NewLifecycleService(LifecycleOptions{
		Store:    store,
		Resolver: complianceOfficer,
		Notifier: n,
		Alerter:  alerter,
	})
	if err != nil {
		t.Fatalf("NewLifecycleService: %v", err)
	}
	return svc
}

func pendingSub(tier string) *Submission {
	return &Submission{ID: 11, AccountID: 7, RequestedTier: tier, Status: SubPendingReview}
}

// --- Approve ----------------------------------------------------------------

func TestApprove_TierAssignedAndNotified(t *testing.T) {
	store := &fakeLifecycleStore{
		sub:    pendingSub(TierT2),
		policy: &TierPolicy{Tier: TierT2, ReverifyMonths: 12},
		decideRes: &DecisionResult{
			SubmissionID: 11, AccountID: 7, Decision: SubApproved,
			RequestedTier: TierT2, AssignedTier: TierT2,
		},
	}
	n := &captureNotifier{}
	svc := lifecycleForTest(t, store, n, nil)
	res, err := svc.Approve(context.Background(), ReviewActor{AdminUserID: 5}, 11)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if res.AssignedTier != TierT2 {
		t.Fatalf("T2 request must assign T2, got %s", res.AssignedTier)
	}
	if len(store.decideIn) != 1 || store.decideIn[0].ReverifyMonths != 12 {
		t.Fatalf("decide tx must carry the 12-month T2 horizon, got %+v", store.decideIn)
	}
	if len(n.calls) != 1 || n.calls[0] != EventKYCApproved {
		t.Fatalf("kyc_approved must emit once, got %v", n.calls)
	}
}

func TestApprove_InstitutionalResolves24Months(t *testing.T) {
	store := &fakeLifecycleStore{
		sub:    pendingSub(TierInstitutional),
		policy: &TierPolicy{Tier: TierInstitutional, ReverifyMonths: 24},
		decideRes: &DecisionResult{
			SubmissionID: 11, AccountID: 7, Decision: SubApproved,
			RequestedTier: TierInstitutional, AssignedTier: TierT2,
			ClientCategory: string(CategoryECP),
		},
	}
	svc := lifecycleForTest(t, store, &captureNotifier{}, nil)
	if _, err := svc.Approve(context.Background(), ReviewActor{AdminUserID: 5}, 11); err != nil {
		t.Fatalf("institutional approve: %v", err)
	}
	if store.decideIn[0].ReverifyMonths != 24 {
		t.Fatalf("institutional horizon must be 24 months, got %d",
			store.decideIn[0].ReverifyMonths)
	}
}

func TestApprove_FailClosed(t *testing.T) {
	// Wrong role → UNAUTHORIZED_ROLE, store never touched.
	store := &fakeLifecycleStore{sub: pendingSub(TierT2),
		policy: &TierPolicy{Tier: TierT2, ReverifyMonths: 12}}
	svc, err := NewLifecycleService(LifecycleOptions{
		Store: store, Resolver: supportAgent})
	if err != nil {
		t.Fatalf("NewLifecycleService: %v", err)
	}
	if _, err := svc.Approve(context.Background(), ReviewActor{AdminUserID: 5}, 11); codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("non-compliance role must reject, got %v", err)
	}
	if len(store.decideIn) != 0 {
		t.Fatal("unauthorized approve must not reach the store")
	}
	// Missing submission → INVALID_REQUEST.
	store2 := &fakeLifecycleStore{sub: nil}
	svc2 := lifecycleForTest(t, store2, &captureNotifier{}, nil)
	if _, err := svc2.Approve(context.Background(), ReviewActor{AdminUserID: 5}, 11); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("missing submission must reject INVALID_REQUEST, got %v", err)
	}
	// Missing tier policy → SERVICE_DEGRADED (the horizon comes from the
	// seeded matrix — never guessed).
	store3 := &fakeLifecycleStore{sub: pendingSub(TierT2), policy: nil}
	svc3 := lifecycleForTest(t, store3, &captureNotifier{}, nil)
	if _, err := svc3.Approve(context.Background(), ReviewActor{AdminUserID: 5}, 11); codeOf(t, err) != "SERVICE_DEGRADED" {
		t.Fatalf("missing policy must degrade closed, got %v", err)
	}
}

// --- Reject -----------------------------------------------------------------

func TestReject_ReasonRequiredAndNotified(t *testing.T) {
	store := &fakeLifecycleStore{
		decideRes: &DecisionResult{
			SubmissionID: 11, AccountID: 7, Decision: SubRejected,
			RequestedTier: TierT2,
		},
	}
	n := &captureNotifier{}
	svc := lifecycleForTest(t, store, n, nil)
	if _, err := svc.Reject(context.Background(), ReviewActor{AdminUserID: 5}, 11, "  "); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("empty reason must reject INVALID_REQUEST, got %v", err)
	}
	res, err := svc.Reject(context.Background(), ReviewActor{AdminUserID: 5}, 11, "document illegible")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if res.Decision != SubRejected {
		t.Fatalf("decision must be REJECTED, got %s", res.Decision)
	}
	if len(store.decideIn) != 1 || store.decideIn[0].Reason != "document illegible" || store.decideIn[0].Approve {
		t.Fatalf("decide tx must carry approve=false + reason, got %+v", store.decideIn)
	}
	if len(n.calls) != 1 || n.calls[0] != EventKYCRejected {
		t.Fatalf("kyc_rejected must emit once, got %v", n.calls)
	}
}

// --- SweepReverify ----------------------------------------------------------

func TestSweepReverify_DowngradesAlertsNotifies(t *testing.T) {
	row := OverdueReverify{
		SubmissionID: 11, AccountID: 7, UserID: 3,
		RequestedTier: TierT2, ReverifyDueAt: time.Now().UTC().Add(-time.Hour),
	}
	store := &fakeLifecycleStore{overdue: []OverdueReverify{row}, downApplied: true}
	n := &captureNotifier{}
	var alerts []string
	svc := lifecycleForTest(t, store, n, &alerts)
	applied, err := svc.SweepReverify(context.Background(), 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if applied != 1 || len(store.downIn) != 1 {
		t.Fatalf("one row must apply, got applied=%d calls=%d", applied, len(store.downIn))
	}
	if len(alerts) != 1 {
		t.Fatalf("compliance alert must fire once, got %v", alerts)
	}
	if len(n.calls) != 1 || n.calls[0] != EventKYCDowngraded {
		t.Fatalf("downgrade notification must emit once, got %v", n.calls)
	}
}

func TestSweepReverify_IdempotentSkip(t *testing.T) {
	// applied=false — a concurrent pass or earlier sweep already
	// downgraded the account; no alert/notification must repeat.
	row := OverdueReverify{SubmissionID: 11, AccountID: 7, UserID: 3,
		RequestedTier: TierT2, ReverifyDueAt: time.Now().UTC().Add(-time.Hour)}
	store := &fakeLifecycleStore{overdue: []OverdueReverify{row}, downApplied: false}
	n := &captureNotifier{}
	var alerts []string
	svc := lifecycleForTest(t, store, n, &alerts)
	applied, err := svc.SweepReverify(context.Background(), 100)
	if err != nil || applied != 0 {
		t.Fatalf("skipped row must not count, got applied=%d err=%v", applied, err)
	}
	if len(alerts) != 0 || len(n.calls) != 0 {
		t.Fatalf("skipped row must not alert/notify, got alerts=%v calls=%v", alerts, n.calls)
	}
}

func TestSweepReverify_RowFailureContinues(t *testing.T) {
	rows := []OverdueReverify{
		{SubmissionID: 11, AccountID: 7, UserID: 3, RequestedTier: TierT2,
			ReverifyDueAt: time.Now().UTC().Add(-time.Hour)},
		{SubmissionID: 12, AccountID: 8, UserID: 4, RequestedTier: TierT2,
			ReverifyDueAt: time.Now().UTC().Add(-2 * time.Hour)},
	}
	store := &fakeLifecycleStore{overdue: rows, downApplied: true}
	// First row's downgrade errors — the sweep alerts and continues.
	store2 := &seqDowngrader{fakeLifecycleStore: store, failFirst: true}
	n := &captureNotifier{}
	var alerts []string
	svc, err := NewLifecycleService(LifecycleOptions{
		Store: store2, Resolver: complianceOfficer, Notifier: n,
		Alerter: func(_ context.Context, sev, code, summary string) error {
			alerts = append(alerts, code)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewLifecycleService: %v", err)
	}
	applied, serr := svc.SweepReverify(context.Background(), 100)
	if serr == nil {
		t.Fatal("first row failure must surface")
	}
	if applied != 1 || store2.calls != 2 {
		t.Fatalf("sweep must continue past the failed row, got applied=%d calls=%d",
			applied, store2.calls)
	}
	if len(alerts) != 2 { // failure alert + success alert
		t.Fatalf("failure + success alerts expected, got %v", alerts)
	}
	if len(n.calls) != 1 {
		t.Fatalf("only the successful row notifies, got %v", n.calls)
	}
}

// seqDowngrader fails the first DowngradeReverifyTx call only.
type seqDowngrader struct {
	*fakeLifecycleStore
	failFirst bool
	calls     int
}

func (s *seqDowngrader) DowngradeReverifyTx(ctx context.Context, p DowngradeTx) (bool, error) {
	s.calls++
	if s.failFirst && s.calls == 1 {
		return false, errors.New("tx conflict")
	}
	return s.fakeLifecycleStore.DowngradeReverifyTx(ctx, p)
}

func TestSweepReverify_FeedErrorFailsClosed(t *testing.T) {
	store := &fakeLifecycleStore{overdueErr: errors.New("db down")}
	svc := lifecycleForTest(t, store, &captureNotifier{}, nil)
	if _, err := svc.SweepReverify(context.Background(), 100); codeOf(t, err) != "SERVICE_DEGRADED" {
		t.Fatalf("feed error must degrade closed, got %v", err)
	}
}

func TestPendingQueue_RoleGated(t *testing.T) {
	store := &fakeLifecycleStore{pending: []Submission{{ID: 7, Status: SubPendingReview}}}
	svc := lifecycleForTest(t, store, &captureNotifier{}, nil)
	// No role resolution → fail closed.
	svc.resolver = nil
	if _, err := svc.PendingQueue(context.Background(), ReviewActor{AdminUserID: 9}, 50); codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("nil resolver must fail closed, got %v", err)
	}
	if _, err := svc.PendingQueue(context.Background(), ReviewActor{}, 50); codeOf(t, err) != "UNAUTHORIZED" {
		t.Fatalf("anonymous actor must be rejected, got %v", err)
	}
	svc.resolver = func(context.Context, int64) (string, error) { return "Compliance Officer", nil }
	subs, err := svc.PendingQueue(context.Background(), ReviewActor{AdminUserID: 9}, 50)
	if err != nil || len(subs) != 1 || subs[0].ID != 7 {
		t.Fatalf("queue must return pending rows, got %v err %v", subs, err)
	}
}
