// Unit tests for Phase-14 Task 14.3.7 — CategorizationService with a
// fake CategoryStore. The gate matrix under test (spec §24 #132):
//
//	SPOT exempt; RETAIL barred from OPTION outright; FORWARD|SWAP|NDF
//	require unexpired PASS (RETAIL + PROFESSIONAL); ECP exempt;
//	unrecognized class, store error or unresolvable category fail closed.
package compliance

import (
	"context"
	"errors"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

// fakeCategoryStore implements CategoryStore for unit tests.
type fakeCategoryStore struct {
	cat       string
	nbp       bool
	catErr    error
	userID    int64
	latest    *Assessment
	latestErr error
	inserted  []*Assessment
	listed    []Assessment
	setCalled bool
	setResult *CategoryChange
	setErr    error
	lastSetTx SetCategoryTx
}

func (f *fakeCategoryStore) ClientCategory(context.Context, int64) (string, bool, error) {
	return f.cat, f.nbp, f.catErr
}
func (f *fakeCategoryStore) AccountUserID(context.Context, int64) (int64, error) {
	return f.userID, nil
}
func (f *fakeCategoryStore) LatestAssessment(context.Context, int64, string) (*Assessment, error) {
	return f.latest, f.latestErr
}
func (f *fakeCategoryStore) InsertAssessment(_ context.Context, a *Assessment) error {
	a.ID = int64(len(f.inserted) + 1)
	f.inserted = append(f.inserted, a)
	return nil
}
func (f *fakeCategoryStore) ListAssessments(context.Context, int64, int) ([]Assessment, error) {
	return f.listed, nil
}
func (f *fakeCategoryStore) SetCategoryTx(_ context.Context, p SetCategoryTx) (*CategoryChange, error) {
	f.setCalled = true
	f.lastSetTx = p
	if f.setErr != nil {
		return nil, f.setErr
	}
	if f.setResult != nil {
		return f.setResult, nil
	}
	return &CategoryChange{AccountID: p.AccountID, From: CategoryRetail,
		To: p.Category, NBP: p.NBP, Evidence: p.Evidence,
		ChangedBy: p.ReviewerID, AuditSeq: 7}, nil
}

func catSvcForTest(t *testing.T, store CategoryStore, resolver RoleResolver) *CategorizationService {
	t.Helper()
	svc, err := NewCategorizationService(store, resolver)
	if err != nil {
		t.Fatalf("NewCategorizationService: %v", err)
	}
	return svc
}

func complianceOfficer(context.Context, int64) (string, error) {
	return "Compliance Officer", nil
}

func supportAgent(context.Context, int64) (string, error) {
	return "Support Agent", nil
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *excerrors.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func passAssessment(now time.Time) *Assessment {
	return &Assessment{
		ID: 1, AccountID: 7, InstrumentClass: ClassForward,
		Outcome: "PASS", Score: 80,
		AssessedAt: now.AddDate(0, -1, 0), ExpiresAt: now.AddDate(0, 11, 0),
	}
}

// --- Appropriateness gate matrix ------------------------------------------

func TestAppropriateness_SpotExemptAllCategories(t *testing.T) {
	for _, cat := range []string{"RETAIL", "PROFESSIONAL", "ELIGIBLE_COUNTERPARTY"} {
		store := &fakeCategoryStore{cat: cat, nbp: cat == "RETAIL"}
		svc := catSvcForTest(t, store, nil)
		if err := svc.Appropriateness(context.Background(), 7, "SPOT"); err != nil {
			t.Fatalf("SPOT must be exempt for %s: %v", cat, err)
		}
		// case-insensitive class normalization
		if err := svc.Appropriateness(context.Background(), 7, "spot"); err != nil {
			t.Fatalf("lowercase spot must normalize: %v", err)
		}
	}
}

func TestAppropriateness_ECPSkipsTest(t *testing.T) {
	store := &fakeCategoryStore{cat: "ELIGIBLE_COUNTERPARTY", nbp: false}
	svc := catSvcForTest(t, store, nil)
	for _, class := range []string{"FORWARD", "SWAP", "NDF", "OPTION"} {
		if err := svc.Appropriateness(context.Background(), 7, class); err != nil {
			t.Fatalf("ECP must skip appropriateness for %s: %v", class, err)
		}
	}
}

func TestAppropriateness_RetailBinaryBlocked(t *testing.T) {
	store := &fakeCategoryStore{cat: "RETAIL", nbp: true,
		latest: passAssessment(time.Now().UTC())} // even a PASS can't admit
	svc := catSvcForTest(t, store, nil)
	err := svc.Appropriateness(context.Background(), 7, "OPTION")
	if codeOf(t, err) != "PRODUCT_NOT_PERMITTED" {
		t.Fatalf("RETAIL OPTION must reject PRODUCT_NOT_PERMITTED, got %v", err)
	}
}

func TestAppropriateness_RetailDerivativeNeedsPass(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name   string
		latest *Assessment
		want   string // "" = admit
	}{
		{"no assessment", nil, "PRODUCT_NOT_PERMITTED"},
		{"fail outcome", &Assessment{Outcome: "FAIL", ExpiresAt: now.Add(time.Hour)}, "PRODUCT_NOT_PERMITTED"},
		{"expired pass", &Assessment{Outcome: "PASS", ExpiresAt: now.Add(-time.Hour)}, "PRODUCT_NOT_PERMITTED"},
		{"valid pass", passAssessment(now), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCategoryStore{cat: "RETAIL", nbp: true, latest: tc.latest}
			svc := catSvcForTest(t, store, nil)
			svc.SetClockForTest(func() time.Time { return now })
			err := svc.Appropriateness(context.Background(), 7, "FORWARD")
			if tc.want == "" && err != nil {
				t.Fatalf("want admit, got %v", err)
			}
			if tc.want != "" && codeOf(t, err) != tc.want {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
		})
	}
}

func TestAppropriateness_ExpiredMidSessionBlocksNextOrder(t *testing.T) {
	// The gate consults the store on EVERY call — a valid session opened
	// before expiry must not grandfather the next order through.
	store := &fakeCategoryStore{cat: "RETAIL", nbp: true,
		latest: passAssessment(time.Now().UTC())}
	svc := catSvcForTest(t, store, nil)
	now := time.Now().UTC()
	svc.SetClockForTest(func() time.Time { return now })
	if err := svc.Appropriateness(context.Background(), 7, "NDF"); err != nil {
		t.Fatalf("fresh pass must admit: %v", err)
	}
	// Clock advances past expiry — the SAME assessment now blocks.
	svc.SetClockForTest(func() time.Time { return now.AddDate(1, 0, 0) })
	if err := svc.Appropriateness(context.Background(), 7, "NDF"); codeOf(t, err) != "PRODUCT_NOT_PERMITTED" {
		t.Fatalf("expired assessment must block the next order, got %v", err)
	}
}

func TestAppropriateness_ProfessionalNeedsPassOnLeveraged(t *testing.T) {
	store := &fakeCategoryStore{cat: "PROFESSIONAL", nbp: false}
	svc := catSvcForTest(t, store, nil)
	if err := svc.Appropriateness(context.Background(), 7, "SWAP"); codeOf(t, err) != "PRODUCT_NOT_PERMITTED" {
		t.Fatalf("PROFESSIONAL without assessment must reject, got %v", err)
	}
	store.latest = passAssessment(time.Now().UTC())
	if err := svc.Appropriateness(context.Background(), 7, "SWAP"); err != nil {
		t.Fatalf("PROFESSIONAL with valid PASS must admit, got %v", err)
	}
}

func TestAppropriateness_FailClosed(t *testing.T) {
	// Unrecognized class → fail closed.
	store := &fakeCategoryStore{cat: "RETAIL", nbp: true}
	svc := catSvcForTest(t, store, nil)
	if err := svc.Appropriateness(context.Background(), 7, "UNKNOWN_CLASS"); codeOf(t, err) != "PRODUCT_NOT_PERMITTED" {
		t.Fatalf("unrecognized class must reject PNP, got %v", err)
	}
	// Store error on the assessment read → SERVICE_DEGRADED.
	store.latestErr = errors.New("db down")
	if err := svc.Appropriateness(context.Background(), 7, "FORWARD"); codeOf(t, err) != "SERVICE_DEGRADED" {
		t.Fatalf("assessment read error must degrade closed, got %v", err)
	}
	// Category read error → propagation (fail closed).
	store.catErr = errors.New("db down")
	if err := svc.Appropriateness(context.Background(), 7, "FORWARD"); err == nil {
		t.Fatal("category read error must not admit")
	}
	// Empty/unresolvable category → SERVICE_DEGRADED, never silent RETAIL.
	store2 := &fakeCategoryStore{cat: "", nbp: false}
	svc2 := catSvcForTest(t, store2, nil)
	if err := svc2.Appropriateness(context.Background(), 7, "FORWARD"); codeOf(t, err) != "SERVICE_DEGRADED" {
		t.Fatalf("unresolvable category must degrade closed, got %v", err)
	}
}

// --- SubmitAssessment -------------------------------------------------------

func TestSubmitAssessment_OutcomeAndExpiry(t *testing.T) {
	store := &fakeCategoryStore{cat: "RETAIL", nbp: true}
	svc := catSvcForTest(t, store, nil)
	now := time.Now().UTC()
	svc.SetClockForTest(func() time.Time { return now })

	pass, err := svc.SubmitAssessment(context.Background(), AssessmentInput{
		AccountID: 7, InstrumentClass: "FORWARD", Score: AppropriatenessPassMark,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if pass.Outcome != "PASS" {
		t.Fatalf("score at pass mark must PASS, got %s", pass.Outcome)
	}
	if !pass.ExpiresAt.Equal(now.AddDate(0, 12, 0)) {
		t.Fatalf("expiry must be assessed_at + 12 months, got %v", pass.ExpiresAt)
	}
	fail, err := svc.SubmitAssessment(context.Background(), AssessmentInput{
		AccountID: 7, InstrumentClass: "forward", Score: AppropriatenessPassMark - 1,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if fail.Outcome != "FAIL" || fail.InstrumentClass != "FORWARD" {
		t.Fatalf("below pass mark must FAIL (normalized class), got %+v", fail)
	}
	if len(store.inserted) != 2 {
		t.Fatalf("store must persist both rows, got %d", len(store.inserted))
	}
}

func TestSubmitAssessment_Validation(t *testing.T) {
	store := &fakeCategoryStore{cat: "RETAIL", nbp: true}
	svc := catSvcForTest(t, store, nil)
	if _, err := svc.SubmitAssessment(context.Background(), AssessmentInput{
		AccountID: 7, InstrumentClass: "SPOT", Score: 90,
	}); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("SPOT must reject (exempt class), got %v", err)
	}
	if _, err := svc.SubmitAssessment(context.Background(), AssessmentInput{
		AccountID: 7, InstrumentClass: "FORWARD", Score: 101,
	}); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("score >100 must reject, got %v", err)
	}
}

// --- SetCategory (Compliance-Officer workflow) ------------------------------

func TestSetCategory_RoleGate(t *testing.T) {
	store := &fakeCategoryStore{cat: "RETAIL", nbp: true}

	// nil resolver → fail closed.
	svc := catSvcForTest(t, store, nil)
	if _, err := svc.SetCategory(context.Background(), ReviewActor{AdminUserID: 5},
		7, "PROFESSIONAL", "evidence"); codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("nil resolver must reject UNAUTHORIZED_ROLE, got %v", err)
	}
	// Wrong role.
	svc = catSvcForTest(t, store, supportAgent)
	if _, err := svc.SetCategory(context.Background(), ReviewActor{AdminUserID: 5},
		7, "PROFESSIONAL", "evidence"); codeOf(t, err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("Support Agent must reject UNAUTHORIZED_ROLE, got %v", err)
	}
	if store.setCalled {
		t.Fatal("unauthorized call must not reach the store")
	}
}

func TestSetCategory_UpgradeNeedsEvidence(t *testing.T) {
	store := &fakeCategoryStore{cat: "RETAIL", nbp: true}
	svc := catSvcForTest(t, store, complianceOfficer)
	if _, err := svc.SetCategory(context.Background(), ReviewActor{AdminUserID: 5},
		7, "PROFESSIONAL", ""); codeOf(t, err) != "INVALID_REQUEST" {
		t.Fatalf("upgrade without evidence must reject, got %v", err)
	}
	ch, err := svc.SetCategory(context.Background(), ReviewActor{AdminUserID: 5},
		7, "PROFESSIONAL", "MiFID II Annex II: 2-of-3 quantitative tests + qualitative review")
	if err != nil {
		t.Fatalf("upgrade with evidence: %v", err)
	}
	if ch.To != CategoryProfessional || ch.NBP {
		t.Fatalf("PROFESSIONAL must drop NBP, got %+v", ch)
	}
	if !store.setCalled || store.lastSetTx.Evidence == "" {
		t.Fatal("store must receive the evidence for the audit row")
	}
}

func TestSetCategory_DowngradeWithOpenPositions(t *testing.T) {
	// The store reports open_derivative_exposure on the change — a
	// downgrade is PERMITTED with open positions (close-only posture is
	// enforced downstream by the admission gate; the audit row carries
	// the flag). No forced close here.
	store := &fakeCategoryStore{cat: "PROFESSIONAL", nbp: false}
	svc := catSvcForTest(t, store, complianceOfficer)
	store.setResult = &CategoryChange{AccountID: 7, From: CategoryProfessional,
		To: CategoryRetail, NBP: true, ChangedBy: 5, AuditSeq: 9, OpenExposure: true}
	ch, err := svc.SetCategory(context.Background(), ReviewActor{AdminUserID: 5},
		7, "RETAIL", "")
	if err != nil {
		t.Fatalf("downgrade must be permitted with open positions: %v", err)
	}
	if !ch.OpenExposure || !ch.NBP {
		t.Fatalf("change must report exposure + re-armed nbp, got %+v", ch)
	}
}

// --- Status / NBP -----------------------------------------------------------

func TestStatusAndNBP(t *testing.T) {
	store := &fakeCategoryStore{cat: "RETAIL", nbp: true,
		listed: []Assessment{*passAssessment(time.Now().UTC())}}
	svc := catSvcForTest(t, store, nil)
	st, err := svc.Status(context.Background(), 7)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.ClientCategory != CategoryRetail || !st.NBP || len(st.Assessments) != 1 {
		t.Fatalf("status projection wrong: %+v", st)
	}
	nbp, err := svc.NBP(context.Background(), 7)
	if err != nil || !nbp {
		t.Fatalf("nbp: %v %v", nbp, err)
	}
	// Missing account (empty category) fails closed.
	store.cat = ""
	if _, err := svc.Status(context.Background(), 7); codeOf(t, err) != "ACCOUNT_NOT_FOUND" {
		t.Fatalf("missing account must surface ACCOUNT_NOT_FOUND, got %v", err)
	}
}
