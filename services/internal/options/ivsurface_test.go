package options

import (
	"math"
	"testing"
	"time"
)

func cleanTenors() []TenorSlice {
	return []TenorSlice{
		{T: 0.25, Forward: 1.10, Discount: 0.9975, Points: []SmilePoint{
			{Strike: 1.05, Vol: 0.115}, {Strike: 1.10, Vol: 0.10}, {Strike: 1.15, Vol: 0.112},
		}},
		{T: 0.50, Forward: 1.11, Discount: 0.9850, Points: []SmilePoint{
			{Strike: 1.05, Vol: 0.120}, {Strike: 1.10, Vol: 0.105}, {Strike: 1.15, Vol: 0.118},
		}},
		{T: 1.00, Forward: 1.12, Discount: 0.9600, Points: []SmilePoint{
			{Strike: 1.05, Vol: 0.130}, {Strike: 1.10, Vol: 0.115}, {Strike: 1.15, Vol: 0.127},
		}},
	}
}

// TestBuildFullSurface: ≥2 usable tenors → SurfaceFull, interpolated
// vols stay inside the bracketing smiles.
func TestBuildFullSurface(t *testing.T) {
	s, err := BuildIVSurface(SurfaceBuildInput{Tenors: cleanTenors(), Validate: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Source() != SurfaceFull {
		t.Fatalf("source %v, want FULL", s.Source())
	}
	// Mid-tenor vol should sit between the bracketing ATM vols.
	v, err := s.Vol(0.375, 1.105)
	if err != nil {
		t.Fatal(err)
	}
	if v <= 0.09 || v >= 0.13 {
		t.Fatalf("interpolated vol %g outside plausible bracket", v)
	}
	// Below the grid: nearest slice's smile verbatim.
	vLow, err := s.Vol(0.10, 1.10)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(vLow-0.10) > 1e-9 {
		t.Fatalf("T<grid vol %g, want first-slice ATM 0.10", vLow)
	}
}

// TestFallbackHierarchy: each downgrade rung must be reached in order
// and recorded — never a silent constant.
func TestFallbackHierarchy(t *testing.T) {
	one := cleanTenors()[:1]
	s, err := BuildIVSurface(SurfaceBuildInput{Tenors: one, ATMVol: 0.10, HasATM: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Source() != SurfaceFlatTerm {
		t.Fatalf("source %v, want FLAT_TERM", s.Source())
	}
	// Smile still varies in k on the flat-term rung.
	v1, _ := s.Vol(0.25, 1.05)
	v2, _ := s.Vol(0.25, 1.10)
	if v1 == v2 {
		t.Fatalf("flat-term surface lost its smile: %g == %g", v1, v2)
	}

	// Empty points → tenor dropped, next rung.
	dropped := []TenorSlice{{T: 0.25, Forward: 1.1, Discount: 0.99, Points: nil}}
	s, err = BuildIVSurface(SurfaceBuildInput{
		Tenors: dropped, ATMVol: 0.1234, HasATM: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Source() != SurfaceATMOnly {
		t.Fatalf("source %v, want ATM_ONLY", s.Source())
	}
	if len(s.DroppedTenors()) != 1 || s.DroppedTenors()[0] != 0.25 {
		t.Fatalf("dropped tenors %v, want [0.25]", s.DroppedTenors())
	}
	v, err := s.Vol(3.0, 99.0)
	if err != nil {
		t.Fatal(err)
	}
	if v != 0.1234 {
		t.Fatalf("ATM-only vol %g, want 0.1234", v)
	}

	// Nothing at all → coded failure, not a default.
	_, err = BuildIVSurface(SurfaceBuildInput{Tenors: dropped})
	requireCode(t, err, CodeVolatilitySurfaceUnavailable)
}

// TestSurfaceInvalidInputs: malformed quotes are coded rejections.
func TestSurfaceInvalidInputs(t *testing.T) {
	bad := cleanTenors()
	bad[0].Points[1].Vol = 0
	if _, err := BuildIVSurface(SurfaceBuildInput{Tenors: bad}); err == nil {
		t.Fatal("zero vol accepted")
	} else {
		requireCode(t, err, CodeOptionPricingInputInvalid)
	}
	dup := cleanTenors()
	dup[0].Points = append(dup[0].Points, SmilePoint{Strike: 1.10, Vol: 0.20})
	if _, err := BuildIVSurface(SurfaceBuildInput{Tenors: dup}); err == nil {
		t.Fatal("duplicate strike accepted")
	} else {
		requireCode(t, err, CodeOptionPricingInputInvalid)
	}
	badT := cleanTenors()
	badT[0].T = -1
	if _, err := BuildIVSurface(SurfaceBuildInput{Tenors: badT}); err == nil {
		t.Fatal("negative tenor accepted")
	}
}

// TestSurfaceDeterminism: identical quote sets build identical
// surfaces — Vol queries agree to the last bit.
func TestSurfaceDeterminism(t *testing.T) {
	in := SurfaceBuildInput{Tenors: cleanTenors()}
	a, err := BuildIVSurface(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildIVSurface(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range [][2]float64{{0.2, 1.08}, {0.375, 1.11}, {1.0, 1.15}, {2.0, 1.0}} {
		va, err := a.Vol(q[0], q[1])
		if err != nil {
			t.Fatal(err)
		}
		vb, err := b.Vol(q[0], q[1])
		if err != nil {
			t.Fatal(err)
		}
		if va != vb {
			t.Fatalf("Vol(%g,%g) diverged: %g vs %g", q[0], q[1], va, vb)
		}
	}
}

// TestImpliedVolRoundTrip: GK price → solver recovers the vol.
func TestImpliedVolRoundTrip(t *testing.T) {
	for _, vol := range []float64{0.05, 0.12, 0.30, 0.60} {
		m := mkt(1.10, 0.03, 0.02, vol, 0.5)
		price := gkRef(t, OptionCall, 1.08, m)
		got, err := ImpliedVolGK(price, OptionCall, 1.08, m)
		if err != nil {
			t.Fatalf("vol %g: %v", vol, err)
		}
		if math.Abs(got-vol) > 1e-6 {
			t.Fatalf("vol %g recovered as %g", vol, got)
		}
	}
	// Put path too.
	m := mkt(1.10, 0.02, 0.04, 0.18, 0.25)
	got, err := ImpliedVolGK(gkRef(t, OptionPut, 1.15, m), OptionPut, 1.15, m)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-0.18) > 1e-6 {
		t.Fatalf("put vol recovered as %g", got)
	}
}

// TestImpliedVolFailures: out-of-bounds targets are input errors;
// an exhausted iteration budget is the coded convergence error —
// never a NaN vol.
func TestImpliedVolFailures(t *testing.T) {
	m := mkt(1.10, 0.03, 0.02, 0.10, 0.5)
	// Impossible call price (above the discounted-spot bound).
	_, err := ImpliedVolGK(2.0, OptionCall, 1.10, m)
	requireCode(t, err, CodeOptionPricingInputInvalid)
	// Budget of one iteration cannot converge from σ₀=0.25 to a
	// far-off target: coded convergence failure, no NaN.
	high := mkt(1.10, 0.03, 0.02, 0.9, 0.5)
	v, err := ivsImpliedVol(gkRef(t, OptionCall, 1.08, high), OptionCall, 1.08, high, 1)
	requireCode(t, err, CodeOptionPricingConvergence)
	if v != 0 || math.IsNaN(v) {
		t.Fatalf("non-convergence returned %g — must be zero, not NaN", v)
	}
	// Negative price → input error.
	_, err = ImpliedVolGK(-0.01, OptionCall, 1.10, m)
	requireCode(t, err, CodeOptionPricingInputInvalid)
}

// TestResolveCurveSnapshot: the §15.6 item-3 hierarchy —
// primary → secondary → flagged last-good → coded failure.
func TestResolveCurveSnapshot(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	gate := 5 * time.Second
	mk := func(label string, age time.Duration, valid bool) CurveCandidate {
		return CurveCandidate{Label: label, AsOf: now.Add(-age), Valid: valid}
	}

	// Primary fresh → wins unflagged.
	sel, err := ResolveCurveSnapshot(now, gate, []CurveCandidate{
		mk("PRIMARY", 1*time.Second, true), mk("SECONDARY", 1*time.Second, true), mk("LAST_GOOD", time.Hour, true),
	})
	if err != nil || sel.Index != 0 || sel.Stale {
		t.Fatalf("primary path: %+v err=%v", sel, err)
	}
	// Primary stale, secondary fresh → secondary fresh.
	sel, err = ResolveCurveSnapshot(now, gate, []CurveCandidate{
		mk("PRIMARY", 10*time.Second, true), mk("SECONDARY", 1*time.Second, true), mk("LAST_GOOD", time.Hour, true),
	})
	if err != nil || sel.Index != 1 || sel.Stale {
		t.Fatalf("secondary path: %+v err=%v", sel, err)
	}
	// All stale → last valid serves with the age flag.
	sel, err = ResolveCurveSnapshot(now, gate, []CurveCandidate{
		mk("PRIMARY", 10*time.Second, true), mk("SECONDARY", 30*time.Second, true),
	})
	if err != nil || sel.Index != 0 || !sel.Stale || sel.Age != 10*time.Second {
		t.Fatalf("last-good path: %+v err=%v", sel, err)
	}
	// Invalid primary skipped even when fresh.
	sel, err = ResolveCurveSnapshot(now, gate, []CurveCandidate{
		mk("PRIMARY", 1*time.Second, false), mk("SECONDARY", 2*time.Second, true),
	})
	if err != nil || sel.Index != 1 {
		t.Fatalf("invalid-skip path: %+v err=%v", sel, err)
	}
	// No valid candidates → coded failure.
	_, err = ResolveCurveSnapshot(now, gate, []CurveCandidate{
		mk("PRIMARY", 1*time.Second, false),
	})
	requireCode(t, err, CodeYieldCurveUnavailable)
	// Determinism: same inputs → same selection.
	staleCands := []CurveCandidate{
		mk("PRIMARY", 10*time.Second, true), mk("SECONDARY", 30*time.Second, true),
	}
	selA, err := ResolveCurveSnapshot(now, gate, staleCands)
	if err != nil {
		t.Fatal(err)
	}
	selB, err := ResolveCurveSnapshot(now, gate, staleCands)
	if err != nil || selB != selA {
		t.Fatalf("non-deterministic selection: %+v vs %+v", selB, selA)
	}
}
