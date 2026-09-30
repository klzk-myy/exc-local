// curve_test.go — numerical coverage for the CIP forward pricer:
// IRP round-trip consistency, the ACT/360 vs ACT/365 boundary, forward
// curve pillars, and every fail-closed path (spec §15.3, §24 #57/#396).
package derivatives

import (
	"context"
	"testing"
	"time"

	"exchange/internal/oracle"
	"exchange/internal/oracle/rates"
	"exchange/pkg/decimal"
)

func mustPair(t *testing.T, base, quote string) Pair {
	t.Helper()
	p, err := NewPair(base, quote)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return p
}

func decEq(t *testing.T, got decimal.Decimal, want string) {
	t.Helper()
	w := decimal.RequireFromString(want)
	if !got.Equal(w) {
		t.Fatalf("got %s, want %s", got, w)
	}
}

// The spec §15.3 formula computed by hand: F = S·(1+rq·d/365)/(1+rb·d/360)
// for GBP/USD at 90 days (GBP is ACT/365, USD ACT/360 — the DCC boundary).
func TestForwardRateDCCBoundaryGBPUSD(t *testing.T) {
	p := mustPair(t, "GBP", "USD") // base GBP (ACT/365), quote USD (ACT/360)
	spot := decimal.RequireFromString("1.25")
	rb := decimal.RequireFromString("0.04") // GBP 4%, ACT/365
	rq := decimal.RequireFromString("0.05") // USD 5%, ACT/360
	got, err := ForwardRate(p, spot, rb, rq, 90)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	// num = 1 + 0.05*90/360 = 1.0125   (quote USD, ACT/360)
	// den = 1 + 0.04*90/365 = 1.0098630137   (base GBP, ACT/365)
	// F   = 1.25 * num/den ≈ 1.253264; assert against an independent
	// reconstruction via float math.
	num := 1.0 + 0.05*90.0/360.0
	den := 1.0 + 0.04*90.0/365.0
	want := decimal.NewFromFloat(1.25 * num / den)
	if got.Sub(want).Abs().GreaterThan(decimal.NewFromFloat(1e-10)) {
		t.Fatalf("F=%s want≈%s", got, want)
	}
}

// Equal rates collapse to spot — the IRP identity F = S.
func TestForwardRateParityIdentity(t *testing.T) {
	p := mustPair(t, "EUR", "USD")
	spot := decimal.RequireFromString("1.10000")
	r := decimal.RequireFromString("0.05")
	got, err := ForwardRate(p, spot, r, r, 365)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	decEq(t, got, "1.10000")
}

// Covered-IRP round-trip: a 365-day EUR/USD forward repriced
// backwards through the same formula returns spot — F/S consistency.
func TestForwardRateRoundTripConsistency(t *testing.T) {
	p := mustPair(t, "EUR", "USD")
	spot := decimal.RequireFromString("1.10000")
	rb := decimal.RequireFromString("0.0325") // EUR (base), ACT/360
	rq := decimal.RequireFromString("0.0525") // USD (quote), ACT/360
	fwd, err := ForwardRate(p, spot, rb, rq, 365)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	// F = S·(1+rq·t)/(1+rb·t) → S = F·(1+rb·t)/(1+rq·t).
	yf := decimal.NewFromInt(365).Div(decimal.NewFromInt(360))
	s2 := fwd.Mul(decimal.One.Add(rb.Mul(yf))).Div(decimal.One.Add(rq.Mul(yf)))
	if s2.Sub(spot).Abs().GreaterThan(decimal.NewFromFloat(1e-12)) {
		t.Fatalf("round trip: %s vs %s", s2, spot)
	}
	if !fwd.GreaterThan(spot) {
		t.Fatalf("higher quote-ccy rate should lift F: %s vs %s", fwd, spot)
	}
}

// USD/JPY is ACT/360 on both legs — identical DCC.
func TestForwardRateUSDJPYBoth360(t *testing.T) {
	p := mustPair(t, "USD", "JPY")
	spot := decimal.RequireFromString("150.00")
	rb := decimal.RequireFromString("0.055")
	rq := decimal.RequireFromString("0.001")
	got, err := ForwardRate(p, spot, rb, rq, 180)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	num := 1.0 + 0.001*180.0/360.0
	den := 1.0 + 0.055*180.0/360.0
	want := decimal.NewFromFloat(150.0 * num / den)
	if got.Sub(want).Abs().GreaterThan(decimal.NewFromFloat(1e-10)) {
		t.Fatalf("F=%s want≈%s", got, want)
	}
}

// Same pair, same inputs — DayCount sanity at the boundary.
func TestDayCountBoundary(t *testing.T) {
	if rates.DayCount("USD") != rates.Basis360 || rates.DayCount("JPY") != rates.Basis360 {
		t.Fatal("USD/JPY must be ACT/360")
	}
	if rates.DayCount("GBP") != rates.Basis365 || rates.DayCount("AUD") != rates.Basis365 {
		t.Fatal("GBP/AUD must be ACT/365")
	}
}

func TestForwardRateRejectsNonPositiveInputs(t *testing.T) {
	p := mustPair(t, "EUR", "USD")
	one := decimal.RequireFromString("1.10")
	r := decimal.RequireFromString("0.05")
	if _, err := ForwardRate(p, decimal.Zero, r, r, 30); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("zero spot: %v", err)
	}
	if _, err := ForwardRate(p, one, decimal.Zero, r, 30); codeOf(t, err) != CodeYieldCurveUnavailable {
		t.Fatalf("zero base rate must fail closed, not price at 1: %v", err)
	}
	if _, err := ForwardRate(p, one, r, r, 0); codeOf(t, err) != CodeInvalidRequest {
		t.Fatalf("zero days: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pricer over fake feeds
// ---------------------------------------------------------------------------

func testPricer() *Pricer {
	now := time.Now()
	return NewPricer(fakeCurves{map[string]rates.Curve{
		"EUR": completeCurve("EUR", 0.0325, now),
		"USD": completeCurve("USD", 0.0525, now),
		"GBP": completeCurve("GBP", 0.04, now),
		"JPY": completeCurve("JPY", 0.001, now),
		"BRL": completeCurve("BRL", 0.11, now),
		"CAD": completeCurve("CAD", 0.045, now),
		"MXN": completeCurve("MXN", 0.10, now),
	}}, nil)
}

func TestPricerForwardEURUSD(t *testing.T) {
	p := testPricer()
	pair := mustPair(t, "EUR", "USD")
	spot := decimal.RequireFromString("1.10")
	spotDate := day(2026, 1, 9) // Friday (T+1 lands Monday 2026-01-12)
	vd := day(2026, 4, 13)      // ~3M
	q, err := p.PriceForward(context.Background(), pair, spot, spotDate, vd)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	// EUR 3.25% (base) < USD 5.25% (quote) → forward above spot.
	if !q.ForwardRate.GreaterThan(spot) {
		t.Fatalf("EUR/USD fwd should exceed spot: %s vs %s",
			q.ForwardRate, spot)
	}
	if !q.SwapPoints.Equal(q.ForwardRate.Sub(spot)) {
		t.Fatalf("points inconsistent: %s", q.SwapPoints)
	}
	if q.Days <= 0 || q.DayCountBase != rates.Basis360 {
		t.Fatalf("quote fields wrong: %+v", q)
	}
}

func TestPricerMissingCurveFailsClosed(t *testing.T) {
	p := NewPricer(fakeCurves{map[string]rates.Curve{
		"EUR": completeCurve("EUR", 0.03, time.Now()),
	}}, nil)
	pair := mustPair(t, "EUR", "USD")
	_, err := p.PriceForward(context.Background(), pair,
		decimal.RequireFromString("1.10"), day(2026, 1, 12), day(2026, 4, 13))
	if codeOf(t, err) != CodeYieldCurveUnavailable {
		t.Fatalf("missing USD curve must be YIELD_CURVE_UNAVAILABLE: %v", err)
	}
}

func TestPricerStaleCurveFailsClosed(t *testing.T) {
	c := completeCurve("USD", 0.05, time.Now().Add(-time.Hour))
	c.Stale = true
	p := NewPricer(fakeCurves{map[string]rates.Curve{
		"EUR": completeCurve("EUR", 0.03, time.Now()),
		"USD": c,
	}}, nil)
	pair := mustPair(t, "EUR", "USD")
	_, err := p.PriceForward(context.Background(), pair,
		decimal.RequireFromString("1.10"), day(2026, 1, 12), day(2026, 4, 13))
	if codeOf(t, err) != CodeYieldCurveUnavailable {
		t.Fatalf("stale curve: %v", err)
	}
}

func TestPricerIncompleteCurveFailsClosed(t *testing.T) {
	c := completeCurve("USD", 0.05, time.Now())
	delete(c.Rates, rates.Tenor6M) // partial curve — never serves
	p := NewPricer(fakeCurves{map[string]rates.Curve{
		"EUR": completeCurve("EUR", 0.03, time.Now()),
		"USD": c,
	}}, nil)
	pair := mustPair(t, "EUR", "USD")
	_, err := p.PriceForward(context.Background(), pair,
		decimal.RequireFromString("1.10"), day(2026, 1, 12), day(2026, 4, 13))
	if codeOf(t, err) != CodeYieldCurveUnavailable {
		t.Fatalf("incomplete curve: %v", err)
	}
}

func TestPricerSpotGate(t *testing.T) {
	p := testPricer()
	// no spot source wired
	if _, err := p.SpotRate(context.Background(), mustPair(t, "EUR", "USD")); codeOf(t, err) != CodePriceOracleUnavailable {
		t.Fatalf("unwired spot: %v", err)
	}
	p.Spot = fakeSpot{map[string]oracle.MarkView{
		"EUR/USD": {Price: decimal.RequireFromString("1.10"), ValidAt: time.Now()},
	}}
	s, err := p.SpotRate(context.Background(), mustPair(t, "EUR", "USD"))
	if err != nil {
		t.Fatalf("spot: %v", err)
	}
	decEq(t, s, "1.10")
	// stale mark fails closed
	p.Spot = fakeSpot{map[string]oracle.MarkView{
		"EUR/USD": {Price: decimal.RequireFromString("1.10"), ValidAt: time.Now(), Stale: true},
	}}
	if _, err := p.SpotRate(context.Background(), mustPair(t, "EUR", "USD")); codeOf(t, err) != CodePriceOracleUnavailable {
		t.Fatalf("stale mark: %v", err)
	}
}

// ForwardCurve emits the canonical five pillars at holiday-adjusted dates.
func TestForwardCurvePillars(t *testing.T) {
	p := testPricer()
	d := NewDates(testCalendar(t))
	pair := mustPair(t, "EUR", "USD")
	out, err := p.ForwardCurve(context.Background(), d, pair,
		decimal.RequireFromString("1.10"), day(2026, 1, 8), 1)
	if err != nil {
		t.Fatalf("curve: %v", err)
	}
	if len(out) != len(ForwardCurveTenors) {
		t.Fatalf("pillars=%d want %d", len(out), len(ForwardCurveTenors))
	}
	for i := 1; i < len(out); i++ {
		if !out[i].ValueDate.After(out[i-1].ValueDate) {
			t.Fatalf("tenor dates not monotone: %+v", out)
		}
	}
}
