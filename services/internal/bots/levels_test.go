// Unit coverage for grid level generation (Task 16.3.19) — pure decimal
// math, no database.
package bots

import (
	"errors"
	"testing"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

func d(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	v, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return v
}

func TestGridLevelsArithmetic(t *testing.T) {
	levels, err := GridLevels(ModeArithmetic,
		d(t, "1.0000"), d(t, "1.1000"), 11, d(t, "0.0001"))
	if err != nil {
		t.Fatalf("levels: %v", err)
	}
	if len(levels) != 11 {
		t.Fatalf("levels %d", len(levels))
	}
	if !levels[0].Equal(d(t, "1.0000")) || !levels[10].Equal(d(t, "1.1000")) {
		t.Fatalf("endpoints %s..%s", levels[0], levels[10])
	}
	step := levels[1].Sub(levels[0])
	if !step.Equal(d(t, "0.0100")) {
		t.Fatalf("step %s", step)
	}
	for i := 1; i < len(levels); i++ {
		if !levels[i-1].LessThan(levels[i]) {
			t.Fatalf("level %d not increasing", i)
		}
	}
}

func TestGridLevelsGeometric(t *testing.T) {
	// r^4 = 1.08243216 exactly for r=1.02 — the bound is 8dp so the
	// tick must be 1e-8 for alignedTick to pass (bounds must sit on
	// tick multiples per GRID_PARAMETERS_INVALID).
	levels, err := GridLevels(ModeGeometric,
		d(t, "1"), d(t, "1.08243216"), 5, d(t, "0.00000001"))
	if err != nil {
		t.Fatalf("levels: %v", err)
	}
	if len(levels) != 5 {
		t.Fatalf("levels %d", len(levels))
	}
	// Each successive ratio ≈ 1.02 within tick granularity.
	for i := 1; i < len(levels); i++ {
		r := levels[i].Div(levels[i-1])
		if r.Sub(d(t, "1.02")).Abs().GreaterThan(d(t, "0.00001")) {
			t.Fatalf("ratio[%d]=%s", i, r)
		}
	}
}

func TestNthRoot(t *testing.T) {
	// Cube root of 8 ≈ 2; Newton on Decimal never leaves the type.
	r := nthRoot(d(t, "8"), 3)
	if r.Sub(d(t, "2")).Abs().GreaterThan(d(t, "0.0000000001")) {
		t.Fatalf("nthRoot(8,3)=%s", r)
	}
}

func TestGridLevelsCollideOnTick(t *testing.T) {
	// Range narrower than tick×(n−1): snapping collapses levels — the
	// generator must reject rather than mint duplicate-price legs.
	_, err := GridLevels(ModeArithmetic,
		d(t, "1.0000"), d(t, "1.0001"), 5, d(t, "0.0001"))
	if err == nil {
		t.Fatal("expected tick-collision rejection")
	}
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != CodeGridParametersInvalid {
		t.Fatalf("code: %v", err)
	}
}

func TestGridLevelsUnalignedBounds(t *testing.T) {
	_, err := GridLevels(ModeArithmetic,
		d(t, "1.00005"), d(t, "1.1000"), 5, d(t, "0.0001"))
	if err == nil {
		t.Fatal("expected unaligned-bound rejection")
	}
}

func TestGridLevelsCountBounds(t *testing.T) {
	for _, n := range []int{0, 4, 201, 1000} {
		if _, err := GridLevels(ModeArithmetic,
			d(t, "1"), d(t, "2"), n, d(t, "0.0001")); err == nil {
			t.Fatalf("grid_count %d accepted", n)
		}
	}
}
