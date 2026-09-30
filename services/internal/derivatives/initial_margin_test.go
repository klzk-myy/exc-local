package derivatives

import (
	"context"
	"testing"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func sens(ref int64, base, quote, delta, vega, sigma, naked string, opt bool) IMSensitivity {
	return IMSensitivity{
		PositionRef: ref, Label: base + "/" + quote,
		BaseCCY: base, QuoteCCY: quote,
		DeltaUSD: dec(delta), VegaUSD: dec(vega), Sigma: dec(sigma),
		IsOption: opt, NakedMarginUSD: dec(naked),
	}
}

func TestIMVolGroupFor(t *testing.T) {
	p := DefaultUMRParams()
	if IMVolGroupFor("EUR", "USD", p) != IMVolRegular {
		t.Fatal("EUR/USD not regular")
	}
	if IMVolGroupFor("USD", "BRL", p) != IMVolHigh {
		t.Fatal("USD/BRL not high-vol")
	}
	if IMVolGroupFor("EUR", "TRY", p) != IMVolHigh {
		t.Fatal("EUR/TRY not high-vol")
	}
}

func TestIMDeltaMargin_SinglePair(t *testing.T) {
	p := DefaultUMRParams()
	// Δ=1000 USD per 1% move, regular pair → WS = 73, margin = |WS| = 73.
	res, err := IMAggregate([]IMSensitivity{
		sens(1, "EUR", "USD", "1000", "0", "0", "0", false)}, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DeltaUSD.Equal(dec("73")) {
		t.Fatalf("delta margin %s want 73", res.DeltaUSD)
	}
	if !res.TotalUSD.Equal(dec("73")) {
		t.Fatalf("total %s", res.TotalUSD)
	}
}

func TestIMDeltaMargin_NettingSamePair(t *testing.T) {
	p := DefaultUMRParams()
	// +1000 and −400 on EUR/USD net to 600 → WS 43.8.
	res, err := IMAggregate([]IMSensitivity{
		sens(1, "EUR", "USD", "1000", "0", "0", "0", false),
		sens(2, "EUR", "USD", "-400", "0", "0", "0", false)}, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DeltaUSD.Equal(dec("43.8")) {
		t.Fatalf("netted delta %s want 43.8", res.DeltaUSD)
	}
}

func TestIMDeltaMargin_TwoPairsCorrelation(t *testing.T) {
	p := DefaultUMRParams()
	// ρ=0.5 within the bucket: K = sqrt(w1² + w2² + 2·0.5·w1·w2).
	// w1=73 (EUR/USD Δ1000), w2=43.8 (GBP/USD Δ600) →
	// K² = 5329 + 1917.84 + 6397.68 = 13644.52 → K ≈ 116.81...
	res, err := IMAggregate([]IMSensitivity{
		sens(1, "EUR", "USD", "1000", "0", "0", "0", false),
		sens(2, "GBP", "USD", "600", "0", "0", "0", false)}, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	// Verify it is strictly below the sum (correlation benefit) and above
	// either leg alone.
	if !(res.DeltaUSD.LessThan(dec("116.8")) && res.DeltaUSD.GreaterThan(dec("73"))) {
		t.Fatalf("correlated delta %s outside (73, 116.8)", res.DeltaUSD)
	}
}

func TestIMDeltaMargin_OffsettingPairs(t *testing.T) {
	p := DefaultUMRParams()
	// +1000 EUR/USD vs −1000 GBP/USD → K = 73·sqrt(2−1) = 73.
	res, err := IMAggregate([]IMSensitivity{
		sens(1, "EUR", "USD", "1000", "0", "0", "0", false),
		sens(2, "GBP", "USD", "-1000", "0", "0", "0", false)}, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DeltaUSD.Equal(dec("73")) {
		t.Fatalf("offsetting delta %s want 73", res.DeltaUSD)
	}
}

func TestIMVegaAndCurvature(t *testing.T) {
	p := DefaultUMRParams()
	// Vega 500 USD per +1.00 vol, σ=0.10, option: VR = 500·0.10·0.35 = 17.5.
	// CVR = −500·0.10·0.073² = −0.26645 → bucket ≤ 0 → θ=−0.5 → 0.133225.
	res, err := IMAggregate([]IMSensitivity{
		sens(1, "EUR", "USD", "0", "500", "0.10", "0", true)}, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.VegaUSD.Equal(dec("17.5")) {
		t.Fatalf("vega margin %s want 17.5", res.VegaUSD)
	}
	if !res.CurvatureUSD.Round(6).Equal(dec("0.133225")) {
		t.Fatalf("curvature %s want 0.133225", res.CurvatureUSD)
	}
}

func TestIMSpreadRelief_AppliesAndCaps(t *testing.T) {
	p := DefaultUMRParams()
	s := []IMSensitivity{
		sens(1, "EUR", "USD", "1000", "0", "0", "100", false),
		sens(2, "EUR", "USD", "-1000", "0", "0", "100", false),
	}
	res, err := IMAggregate(s, []IMSpreadOffset{{
		SpreadID: "s1", SpreadType: "VERTICAL_CALL",
		LegRefs: []int64{1, 2}, ReliefUSD: dec("50")}}, p)
	if err != nil {
		t.Fatal(err)
	}
	// §15.7 pre-aggregation ordering (Task 19.3.25): the consumed legs
	// never enter delta aggregation — the pair contributes its bounded
	// margin (naked 200 − relief 50 = 150), NOT the netted-delta 0 AND
	// NOT a double-counted hedge benefit.
	if !res.SpreadMarginUSD.Equal(dec("150")) {
		t.Fatalf("spread margin %s want 150 (naked−relief)", res.SpreadMarginUSD)
	}
	if !res.TotalUSD.Equal(dec("150")) {
		t.Fatalf("total %s want 150 (bounded margin)", res.TotalUSD)
	}
	if !res.DeltaUSD.IsZero() {
		t.Fatalf("residual delta %s want 0 — consumed legs must not aggregate", res.DeltaUSD)
	}
	// Relief beyond naked sum caps at the legs' naked margin; the pair
	// then contributes bound = 100 − 100 = 0.
	res2, err := IMAggregate([]IMSensitivity{
		sens(1, "EUR", "USD", "1000", "0", "0", "100", false),
	}, []IMSpreadOffset{{SpreadID: "s9", SpreadType: "CALENDAR",
		LegRefs: []int64{1}, ReliefUSD: dec("500")}}, p)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.SpreadReliefUSD.Equal(dec("100")) {
		t.Fatalf("relief %s should cap at naked 100", res2.SpreadReliefUSD)
	}
	if !res2.SpreadMarginUSD.IsZero() || !res2.TotalUSD.IsZero() {
		t.Fatalf("capped spread should contribute 0, got margin %s total %s",
			res2.SpreadMarginUSD, res2.TotalUSD)
	}
}

// TestIMSpreadRelief_ResidualLegsStillAggregate pins the no-double-count
// invariant from the other side: consuming the spread pair must not
// strip unrelated legs from the aggregation.
func TestIMSpreadRelief_ResidualLegsStillAggregate(t *testing.T) {
	p := DefaultUMRParams()
	s := []IMSensitivity{
		sens(1, "EUR", "USD", "1000", "0", "0", "100", false),
		sens(2, "EUR", "USD", "-1000", "0", "0", "100", false),
		// Residual leg — NOT in the spread; must aggregate normally.
		sens(3, "GBP", "USD", "500", "0", "0", "40", false),
	}
	res, err := IMAggregate(s, []IMSpreadOffset{{
		SpreadID: "s1", SpreadType: "VERTICAL_CALL",
		LegRefs: []int64{1, 2}, ReliefUSD: dec("50")}}, p)
	if err != nil {
		t.Fatal(err)
	}
	// Residual leg alone: 500·0.073 = 36.5 delta margin; pair bound 150.
	if !res.DeltaUSD.Equal(dec("36.5")) {
		t.Fatalf("residual delta %s want 36.5", res.DeltaUSD)
	}
	if !res.TotalUSD.Equal(dec("186.5")) {
		t.Fatalf("total %s want 186.5 (residual agg + bounded pair)", res.TotalUSD)
	}
}

func TestIMSpreadRelief_DoubleConsumeFailsClosed(t *testing.T) {
	p := DefaultUMRParams()
	s := []IMSensitivity{
		sens(1, "EUR", "USD", "1000", "0", "0", "100", false),
		sens(2, "GBP", "USD", "800", "0", "0", "100", false),
	}
	_, err := IMAggregate(s, []IMSpreadOffset{
		{SpreadID: "a", SpreadType: "VERTICAL_CALL", LegRefs: []int64{1, 2}, ReliefUSD: dec("10")},
		{SpreadID: "b", SpreadType: "STRADDLE", LegRefs: []int64{1}, ReliefUSD: dec("10")},
	}, p)
	if err == nil {
		t.Fatal("double-consumed leg accepted")
	}
	if excerrors.CodeOf(err) != "DERIVATIVE_STATE_CONFLICT" {
		t.Fatalf("expected DERIVATIVE_STATE_CONFLICT, got %v", err)
	}
}

// ---- UMR admission gate -----------------------------------------------

type fakeScope struct {
	inScope bool
	call    *UMRAssessment
	err     error
}

func (f fakeScope) UMRInScope(context.Context, int64) (bool, error) { return f.inScope, f.err }
func (f fakeScope) OutstandingCall(context.Context, int64) (*UMRAssessment, error) {
	return f.call, f.err
}

type fakeDocGate struct{ err error }

func (f fakeDocGate) AdmitOrder(context.Context, int64, string, string, bool) error {
	return f.err
}

func TestUMRAdmissionGate(t *testing.T) {
	ctx := context.Background()
	newGate := func(scope UMRScopeService, docs UMRDocGate) *UMRAdmissionGate {
		g, err := NewUMRAdmissionGate(scope, docs)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}

	// SPOT passes regardless.
	g := newGate(fakeScope{inScope: true}, fakeDocGate{})
	if err := g.AdmitOrder(ctx, 1, "SPOT", "PROFESSIONAL", false); err != nil {
		t.Fatalf("spot rejected: %v", err)
	}
	// NDF, docs missing → LEGAL_DOC_REQUIRED propagates.
	docErr := excerrors.New("LEGAL_DOC_REQUIRED", "missing ISDA")
	g = newGate(fakeScope{inScope: false}, fakeDocGate{err: docErr})
	err := g.AdmitOrder(ctx, 1, "NDF", "PROFESSIONAL", false)
	if excerrors.CodeOf(err) != "LEGAL_DOC_REQUIRED" {
		t.Fatalf("expected LEGAL_DOC_REQUIRED, got %v", err)
	}
	// Out-of-scope + docs OK → admit (standard margin handles it).
	g = newGate(fakeScope{inScope: false}, fakeDocGate{})
	if err := g.AdmitOrder(ctx, 1, "OPTION", "PROFESSIONAL", false); err != nil {
		t.Fatalf("out-of-scope rejected: %v", err)
	}
	// In-scope + open IM call → MARGIN_INSUFFICIENT.
	call := &UMRAssessment{Status: UMRStatusCallIssued, DeficitUSD: dec("1000")}
	g = newGate(fakeScope{inScope: true, call: call}, fakeDocGate{})
	err = g.AdmitOrder(ctx, 1, "NDF", "PROFESSIONAL", false)
	if excerrors.CodeOf(err) != "MARGIN_INSUFFICIENT" {
		t.Fatalf("expected MARGIN_INSUFFICIENT, got %v", err)
	}
	// In-scope, no open call → admit.
	g = newGate(fakeScope{inScope: true}, fakeDocGate{})
	if err := g.AdmitOrder(ctx, 1, "OPTION", "PROFESSIONAL", false); err != nil {
		t.Fatalf("in-scope clean rejected: %v", err)
	}
	// reduceOnly bypasses everything.
	g = newGate(fakeScope{inScope: true, call: call}, fakeDocGate{err: docErr})
	if err := g.AdmitOrder(ctx, 1, "NDF", "PROFESSIONAL", true); err != nil {
		t.Fatalf("reduce-only rejected: %v", err)
	}
	// Scope read error fails closed.
	g = newGate(fakeScope{err: errFixture("db down")}, fakeDocGate{})
	err = g.AdmitOrder(ctx, 1, "NDF", "PROFESSIONAL", false)
	if excerrors.CodeOf(err) != "UMR_IM_EVAL_FAILED" {
		t.Fatalf("expected UMR_IM_EVAL_FAILED, got %v", err)
	}
}

type errFixture string

func (e errFixture) Error() string { return string(e) }

func TestUMRAdmissionGate_NilDepsFailClosed(t *testing.T) {
	if _, err := NewUMRAdmissionGate(nil, fakeDocGate{}); err == nil {
		t.Fatal("nil scope accepted")
	}
	if _, err := NewUMRAdmissionGate(fakeScope{}, nil); err == nil {
		t.Fatal("nil docs accepted")
	}
}
