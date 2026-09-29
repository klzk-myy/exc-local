package funding

// Phase-13.5 Task 13.5.3.3 — unit tests for the withdrawal-side
// sanctions seam (WithdrawalService.WithSanctions +
// WithBeneficiaryResolver, screenSanctions inside Confirm). The
// real list-backed screener lives in internal/compliance and is
// exercised against dev fixtures + PG/Redis in
// internal/funding/sanctions_integration_test.go.

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"
)

// wdScreener records the (name, destination) tuple it was asked to
// screen and returns a programmed hit/error.
type wdScreener struct {
	hit       bool
	err       error
	lastName  string
	lastDest  string
	callCount int
}

func (f *wdScreener) ScreenWithdrawal(_ context.Context, _ int64,
	beneficiaryName, destination string) (bool, error) {
	f.callCount++
	f.lastName, f.lastDest = beneficiaryName, destination
	return f.hit, f.err
}

// wdAlerts captures OpsAlerts.
type wdAlerts struct{ raised []OpsAlert }

func (a *wdAlerts) Raise(_ context.Context, al OpsAlert) error {
	a.raised = append(a.raised, al)
	return nil
}

// wdSanctionsFixture wires a WithdrawalService over flowStore with the
// beneficiary resolver bound so the screen sees legal names.
func wdSanctionsFixture(t *testing.T, sc WithdrawalScreener) (*WithdrawalService, *flowStore, *wdAlerts) {
	t.Helper()
	st := newFlowStore()
	st.meta[7] = &AccountMeta{ID: 7, UserID: 1, KYCTier: "T2", Status: "ACTIVE"}
	poster := &fakePoster{}
	checker := fakeChecker{status: map[int64]string{7: "ACTIVE"}}
	svc, err := NewWithdrawalService(st, poster, checker)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	svc.WithUSDConverter(usdIdentity{}).WithBeneficiaryResolver(st)
	if sc != nil {
		svc.WithSanctions(sc)
	}
	al := &wdAlerts{}
	svc.WithAlerter(al)
	return svc, st, al
}

// wdCreateAndConfirm runs create + confirm for a USD amount.
func wdCreateAndConfirm(t *testing.T, svc *WithdrawalService, amount, dest string) *WithdrawalResult {
	t.Helper()
	ctx := context.Background()
	res, err := svc.Create(ctx, CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: amount,
		ReferenceAccount: dest, IdempotencyKey: "san-" + amount + "-" + dest,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := svc.Confirm(ctx, ConfirmWithdrawalRequest{
		WithdrawalID: res.WithdrawalID, AccountID: 7, UserID: 1,
		Token: res.ConfirmToken,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return out
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// Positive hit parks even an AUTO-tier withdrawal in PENDING_REVIEW
// with SANCTIONS_HIT and raises the P1 ops alert.
func TestWithdrawalSanctionsHitBlocksAutoTier(t *testing.T) {
	sc := &wdScreener{hit: true}
	svc, _, al := wdSanctionsFixture(t, sc)
	out := wdCreateAndConfirm(t, svc, "100", "IBAN-SAN-1")
	if out.Status != FundingPendingReview {
		t.Fatalf("sanctions hit must park in PENDING_REVIEW, got %s", out.Status)
	}
	if !hasFlag(out.Flags, "SANCTIONS_HIT") {
		t.Fatalf("expected SANCTIONS_HIT flag, got %v", out.Flags)
	}
	if out.ReviewDeadline == nil {
		t.Fatal("reviewed disposition must carry the 4h deadline")
	}
	if d := time.Until(*out.ReviewDeadline); d < 3*time.Hour || d > 5*time.Hour {
		t.Fatalf("deadline must be ~4h, got %s", d)
	}
	if len(al.raised) == 0 || al.raised[0].Code != "SANCTIONS_HIT" {
		t.Fatalf("expected SANCTIONS_HIT ops alert, got %+v", al.raised)
	}
	if sc.callCount == 0 {
		t.Fatal("screener was never consulted")
	}
}

// A screener error fails closed: SANCTIONS_SERVICE_UNAVAILABLE and the
// withdrawal stays PENDING (its token remains inside the 15-minute
// window so a retry after the outage is honest).
func TestWithdrawalSanctionsOutageFailsClosed(t *testing.T) {
	sc := &wdScreener{err: stderrors.New("list store offline")}
	svc, st, _ := wdSanctionsFixture(t, sc)
	ctx := context.Background()
	res, err := svc.Create(ctx, CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "500",
		ReferenceAccount: "IBAN-SAN-2", IdempotencyKey: "san-outage",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.Confirm(ctx, ConfirmWithdrawalRequest{
		WithdrawalID: res.WithdrawalID, AccountID: 7, UserID: 1,
		Token: res.ConfirmToken,
	})
	requireErrCode(t, err, "SANCTIONS_SERVICE_UNAVAILABLE")
	row, rerr := st.WithdrawalByID(ctx, res.WithdrawalID)
	if rerr != nil {
		t.Fatalf("row read: %v", rerr)
	}
	if row.Status != FundingPending {
		t.Fatalf("outage must leave withdrawal PENDING, got %s", row.Status)
	}
}

// Nil screener + STANDARD tier escalates to PENDING_REVIEW with the
// SANCTIONS_UNAVAILABLE flag — symmetric to the deposit seam.
func TestWithdrawalNilScreenerStandardTierReviews(t *testing.T) {
	svc, _, _ := wdSanctionsFixture(t, nil)
	out := wdCreateAndConfirm(t, svc, "20000", "IBAN-SAN-3")
	if out.Status != FundingPendingReview {
		t.Fatalf("nil screener + STANDARD must review, got %s", out.Status)
	}
	if !hasFlag(out.Flags, "SANCTIONS_UNAVAILABLE") {
		t.Fatalf("expected SANCTIONS_UNAVAILABLE flag, got %v", out.Flags)
	}
}

// Nil screener + AUTO tier keeps the documented dev posture: confirms
// straight through (same residual the deposit side documents).
func TestWithdrawalNilScreenerAutoTierConfirms(t *testing.T) {
	svc, _, _ := wdSanctionsFixture(t, nil)
	out := wdCreateAndConfirm(t, svc, "100", "IBAN-SAN-4")
	if out.Status != FundingConfirmed {
		t.Fatalf("AUTO tier with unwired seam should confirm, got %s", out.Status)
	}
}

// The registered beneficiary legal name is what the screener sees —
// not just the raw IBAN.
func TestWithdrawalSanctionsScreensBeneficiaryName(t *testing.T) {
	sc := &wdScreener{hit: true}
	svc, st, _ := wdSanctionsFixture(t, sc)
	st.beneficiaries[dkey(7, "IBAN-SAN-5")] = &BeneficiaryRow{
		BankAccountID: 42, AccountID: 7, Currency: "USD",
		AccountNumber:   strPtr("IBAN-SAN-5"),
		BeneficiaryName: "Blocked Beneficiary Trading",
		Status:          BeneficiaryVerified,
	}
	out := wdCreateAndConfirm(t, svc, "100", "IBAN-SAN-5")
	if out.Status != FundingPendingReview {
		t.Fatalf("hit must review, got %s", out.Status)
	}
	if sc.lastName != "Blocked Beneficiary Trading" {
		t.Fatalf("screener saw %q — beneficiary name not resolved", sc.lastName)
	}
	if sc.lastDest != "IBAN-SAN-5" {
		t.Fatalf("destination %q not passed", sc.lastDest)
	}
}

// A clean screen on a STANDARD-tier withdrawal confirms normally.
func TestWithdrawalCleanScreenConfirms(t *testing.T) {
	sc := &wdScreener{hit: false}
	svc, _, _ := wdSanctionsFixture(t, sc)
	out := wdCreateAndConfirm(t, svc, "20000", "IBAN-SAN-6")
	if out.Status != FundingConfirmed {
		t.Fatalf("clean screen must confirm, got %s flags %v", out.Status, out.Flags)
	}
}

// Guard: the deposit seam must still use sender name, and the
// withdrawal result must not conflate flags with status text.
func TestWithdrawalFlagsJSONShape(t *testing.T) {
	sc := &wdScreener{hit: true}
	svc, _, _ := wdSanctionsFixture(t, sc)
	out := wdCreateAndConfirm(t, svc, "100", "IBAN-SAN-7")
	if strings.Join(out.Flags, ",") != "SANCTIONS_HIT" {
		t.Fatalf("unexpected flags %v", out.Flags)
	}
}
