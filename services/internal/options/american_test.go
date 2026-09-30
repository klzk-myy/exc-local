package options

import (
	"math"
	"testing"

	excerrors "exchange/pkg/errors"
)

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected coded error %s, got nil", code)
	}
	if got := excerrors.CodeOf(err); got != code {
		t.Fatalf("error code %q, want %q (err=%v)", got, code, err)
	}
}

// amOpt builds the American-style option under test.
func amOpt(right OptionRight, strike float64) VanillaOption {
	return VanillaOption{Right: right, Strike: strike, Style: ExerciseAmerican}
}

// gkRef is the closed-form European reference for the same contract.
func gkRef(t *testing.T, right OptionRight, strike float64, m Market) float64 {
	t.Helper()
	p, err := VanillaOption{Right: right, Strike: strike, Style: ExerciseEuropean}.Price(m)
	if err != nil {
		t.Fatalf("GK reference: %v", err)
	}
	return p
}

// TestAmericanGeEuropean: the American leg must never price below the
// matched European leg on the same grid (§15.2 boundary contract).
func TestAmericanGeEuropean(t *testing.T) {
	cases := []struct {
		right OptionRight
		m     Market
		k     float64
	}{
		// Deep-ITM put with high quote rate: real early-exercise premium.
		{OptionPut, mkt(0.90, 0.10, 0.00, 0.15, 1.0), 1.10},
		// ITM call with high base rate.
		{OptionCall, mkt(1.20, 0.01, 0.08, 0.12, 0.5), 1.00},
		// ATM baselines.
		{OptionCall, mkt(1.10, 0.03, 0.02, 0.10, 0.25), 1.10},
		{OptionPut, mkt(1.10, 0.03, 0.02, 0.10, 0.25), 1.10},
	}
	for i, c := range cases {
		res, err := PriceAmerican(amOpt(c.right, c.k), c.m, DefaultLatticeConfig)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if res.Price < res.European-1e-10 {
			t.Fatalf("case %d: american %g < european %g", i, res.Price, res.European)
		}
		// And vs the GK closed form on the same market.
		gk := gkRef(t, c.right, c.k, c.m)
		if res.Price < gk-2e-3 { // lattice discretization tolerance
			t.Fatalf("case %d: american %g < GK %g beyond tolerance", i, res.Price, gk)
		}
	}
}

// TestAmericanStrictPremium: a deep-ITM put under a high quote rate
// must carry a strictly positive early-exercise premium over GK and
// expose a non-empty early-exercise boundary.
func TestAmericanStrictPremium(t *testing.T) {
	m := mkt(0.90, 0.10, 0.00, 0.15, 1.0)
	res, err := PriceAmerican(amOpt(OptionPut, 1.10), m, DefaultLatticeConfig)
	if err != nil {
		t.Fatal(err)
	}
	gk := gkRef(t, OptionPut, 1.10, m)
	if res.Price <= gk+1e-4 {
		t.Fatalf("expected positive early-exercise premium: american %g, GK %g",
			res.Price, gk)
	}
	if len(res.Boundary) == 0 {
		t.Fatal("expected a non-empty early-exercise boundary for deep-ITM put")
	}
	// Boundary ordering is canonical: Time ascending.
	for i := 1; i < len(res.Boundary); i++ {
		if res.Boundary[i].Time < res.Boundary[i-1].Time {
			t.Fatalf("boundary not time-ascending at %d", i)
		}
	}
}

// TestEuropeanLatticeVsGK: when early exercise is never optimal the
// lattice must match the closed form within discretization tolerance,
// and American == European to numerical noise.
func TestEuropeanLatticeVsGK(t *testing.T) {
	o := VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean}
	m := mkt(1.10, 0.03, 0.02, 0.12, 0.5)
	eur, err := PriceEuropeanLattice(o, m, 400)
	if err != nil {
		t.Fatal(err)
	}
	gk := gkRef(t, OptionCall, 1.10, m)
	if math.Abs(eur-gk) > 1e-3 {
		t.Fatalf("lattice euro %g vs GK %g (|diff| %g)", eur, gk, math.Abs(eur-gk))
	}
	// With rd=rf=0 an American call equals the European (no exercise
	// premium): consistency-with-closed-form per §15.2.
	flat := mkt(1.10, 0, 0, 0.12, 0.5)
	res, err := PriceAmerican(amOpt(OptionCall, 1.10), flat, DefaultLatticeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if diff := math.Abs(res.Price - res.European); diff > 1e-9 {
		t.Fatalf("american %g != european %g when no exercise is optimal", res.Price, res.European)
	}
}

// TestLatticeConvergence: spec §15.2 bounds honored — converged step
// count inside [100, 10000], relative change inside tolerance.
func TestLatticeConvergence(t *testing.T) {
	m := mkt(1.10, 0.04, 0.02, 0.11, 1.0)
	res, err := PriceAmerican(amOpt(OptionCall, 1.10), m, DefaultLatticeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps < 100 || res.Steps > 10_000 {
		t.Fatalf("steps %d outside [100,10000]", res.Steps)
	}
	if res.RelChange > 1e-4 {
		t.Fatalf("rel change %g exceeds 1e-4", res.RelChange)
	}
}

// TestLatticeNonConvergence: a config whose ceiling sits below the
// refinement needed must fail closed with the coded error — never a
// NaN price.
func TestLatticeNonConvergence(t *testing.T) {
	m := mkt(1.10, 0.04, 0.02, 0.11, 1.0)
	cfg := LatticeConfig{MinSteps: 4, MaxSteps: 8, Tolerance: 1e-12}
	res, err := PriceAmerican(amOpt(OptionCall, 1.10), m, cfg)
	requireCode(t, err, CodeOptionPricingConvergence)
	if res.Price != 0 || math.IsNaN(res.Price) {
		t.Fatalf("non-convergence returned value %g — must be zero, not NaN", res.Price)
	}
}

// TestLatticeProbabilityBreach: an extreme drift/vol regime that
// breaks the trinomial probability bounds is a coded abort.
func TestLatticeProbabilityBreach(t *testing.T) {
	m := mkt(1.10, 5.0, -4.0, 0.01, 0.01)
	_, err := PriceAmerican(amOpt(OptionCall, 1.10), m,
		LatticeConfig{MinSteps: 4, MaxSteps: 16, Tolerance: 1e-4})
	requireCode(t, err, CodeOptionPricingConvergence)
}

// TestAmericanInvalidInputs: malformed inputs are coded rejections —
// INVALID_REQUEST via the package's invalidInput contract (types.go).
func TestAmericanInvalidInputs(t *testing.T) {
	// Non-American style refuses here (routes to VanillaOption.Price).
	_, err := PriceAmerican(
		VanillaOption{Right: OptionCall, Strike: 1.10, Style: ExerciseEuropean},
		mkt(1.10, 0.01, 0.01, 0.10, 1.0), DefaultLatticeConfig)
	requireCode(t, err, CodeOptionPricingInputInvalid)

	badMarkets := []Market{
		mkt(-1, 0.01, 0.01, 0.10, 1.0), // non-positive spot
		mkt(1.10, 0.01, 0.01, 0.10, 0), // T == 0
		mkt(1.10, 0.01, 0.01, 0, 1.0),  // zero vol
		{Spot: math.NaN(), DFd: 0.99, DFf: 0.99, Vol: 0.1, T: 1},
		{Spot: 1.10, DFd: 0.99, DFf: 0.99, Vol: math.Inf(1), T: 1},
	}
	for i, m := range badMarkets {
		_, err := PriceAmerican(amOpt(OptionCall, 1.10), m, DefaultLatticeConfig)
		if err == nil {
			t.Fatalf("market %d: expected error", i)
		}
		requireCode(t, err, CodeOptionPricingInputInvalid)
	}
}

// TestAmericanDeterminism: identical inputs → byte-identical result.
func TestAmericanDeterminism(t *testing.T) {
	o := amOpt(OptionPut, 1.10)
	m := mkt(0.95, 0.08, 0.01, 0.14, 0.75)
	a, err := PriceAmerican(o, m, DefaultLatticeConfig)
	if err != nil {
		t.Fatal(err)
	}
	b, err := PriceAmerican(o, m, DefaultLatticeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if a.Price != b.Price || a.Steps != b.Steps || a.RelChange != b.RelChange ||
		a.European != b.European || len(a.Boundary) != len(b.Boundary) {
		t.Fatal("non-deterministic lattice result")
	}
	for i := range a.Boundary {
		if a.Boundary[i] != b.Boundary[i] {
			t.Fatalf("boundary[%d] diverged: %+v vs %+v", i, a.Boundary[i], b.Boundary[i])
		}
	}
}
