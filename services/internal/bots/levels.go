// Grid level generation — ARITHMETIC (equal price spacing) and GEOMETRIC
// (equal ratio spacing), all in shopspring decimal: no float64 ever
// touches a financial quantity.
//
// grid_count is the number of price LEVELS (spec Task 16.3.19 names it
// the grid size). Levels index 0..N-1 from lower_price to upper_price
// inclusive; both bounds must be tick-aligned and every snapped level
// must remain strictly increasing — a grid too fine for the tick is
// rejected with GRID_PARAMETERS_INVALID rather than silently collapsing
// levels onto the same price.
//
// GEOMETRIC needs r = (upper/lower)^(1/(N-1)) — a non-integer root.
// shopspring has no nth-root, so nthRoot implements Newton iteration on
// Decimal directly (integer powers only) — deterministic, exact, and
// float-free.
package bots

import (
	"exchange/pkg/decimal"
)

// nthRoot returns the principal n-th root of a (a>0, n≥1) via Newton on
// r^n − a = 0. The seed 1+(a−1)/n sits above the true root for a>1, where
// the convex iteration converges monotonically downward.
func nthRoot(a decimal.Decimal, n int) decimal.Decimal {
	if n <= 1 || a.Equal(decimal.One) {
		return a
	}
	dn := decimal.NewFromInt(int64(n))
	r := a.Sub(decimal.One).Div(dn).Add(decimal.One)
	if !r.IsPositive() {
		r = decimal.New(1, -8) // a<1 seed floor — iteration climbs from below
	}
	eps := decimal.New(1, -18)
	for i := 0; i < 128; i++ {
		rn := r.Pow(dn)
		denom := r.Pow(decimal.NewFromInt(int64(n - 1))).Mul(dn)
		if denom.IsZero() {
			break
		}
		next := r.Sub(rn.Sub(a).Div(denom))
		if !next.IsPositive() {
			// Overshot below zero — halve and continue.
			r = r.Div(decimal.NewFromInt(2))
			continue
		}
		if next.Sub(r).Abs().LessThan(eps) {
			r = next
			break
		}
		r = next
	}
	return r
}

// snapTick rounds p to the nearest tick multiple (half-up, away from
// zero for positive prices). tick<=0 returns p unchanged — callers must
// have validated tick>0 before relying on alignment.
func snapTick(p, tick decimal.Decimal) decimal.Decimal {
	if !tick.IsPositive() {
		return p
	}
	return p.Div(tick).Round(0).Mul(tick)
}

// alignedTick reports whether p sits exactly on a tick multiple.
func alignedTick(p, tick decimal.Decimal) bool {
	if !tick.IsPositive() {
		return true // no tick constraint configured
	}
	return p.Div(tick).Equal(p.Div(tick).Truncate(0))
}

// GridLevels generates the N tick-aligned price levels. Validation of
// bounds/tick happens in validateCreate; here we enforce the structural
// invariants that can only be checked post-generation (strict increase
// after snapping).
func GridLevels(mode string, lower, upper decimal.Decimal, n int,
	tick decimal.Decimal) ([]decimal.Decimal, error) {

	if n < MinGridCount || n > MaxGridCount {
		return nil, errorf(CodeGridParametersInvalid,
			"grid_count %d outside [%d,%d]", n, MinGridCount, MaxGridCount)
	}
	if !lower.IsPositive() || !lower.LessThan(upper) {
		return nil, errorf(CodeGridParametersInvalid,
			"lower_price must be positive and below upper_price")
	}
	if !alignedTick(lower, tick) || !alignedTick(upper, tick) {
		return nil, errorf(CodeGridParametersInvalid,
			"grid bounds must align to tick_size %s", tick)
	}

	levels := make([]decimal.Decimal, n)
	switch mode {
	case ModeArithmetic:
		step := upper.Sub(lower).Div(decimal.NewFromInt(int64(n - 1)))
		if step.LessThan(tick) {
			return nil, errorf(CodeGridParametersInvalid,
				"grid step %s below tick_size %s — range too tight for %d levels",
				step, tick, n)
		}
		for i := 0; i < n; i++ {
			levels[i] = snapTick(lower.Add(step.Mul(decimal.NewFromInt(int64(i)))), tick)
		}
	case ModeGeometric:
		r := nthRoot(upper.Div(lower), n-1)
		for i := 0; i < n; i++ {
			levels[i] = snapTick(lower.Mul(r.Pow(decimal.NewFromInt(int64(i)))), tick)
		}
	default:
		return nil, errorf(CodeGridParametersInvalid,
			"mode must be ARITHMETIC|GEOMETRIC, got %q", mode)
	}
	// Force the endpoints exactly onto the validated bounds — endpoint
	// snapping can drift a tick inside the range and would silently
	// narrow the grid.
	levels[0] = lower
	levels[n-1] = upper
	for i := 1; i < n; i++ {
		if !levels[i-1].LessThan(levels[i]) {
			return nil, errorf(CodeGridParametersInvalid,
				"grid levels collide at tick granularity near level %d "+
					"(%s ≥ %s) — widen the range or reduce grid_count",
				i, levels[i-1], levels[i])
		}
	}
	return levels, nil
}
