// volarb.go — Phase-22 Task 22.3.14 item 2: volatility-surface
// arbitrage rejection (spec §15.6 item 1, §24 #324).
//
// Two checks, both evaluated on the built IVSurface (ivsurface.go) so
// the validator sees exactly what downstream pricing would consume:
//
//   - Calendar no-arb: total implied variance w(k,T) = σ²(k,T)·T must
//     be non-decreasing in T at fixed log-moneyness k = ln(K/F_T).
//     Evaluated on the union grid of every slice's quoted k values
//     (sorted, deduped) — deterministic order, deterministic verdict.
//   - Butterfly no-arb: European call prices implied by each slice's
//     smile must be convex in strike: for consecutive strikes
//     K1<K2<K3, C(K2) ≤ λ·C(K1) + (1−λ)·C(K3) with
//     λ = (K3−K2)/(K3−K1). A negative butterfly = free strike
//     arbitrage.
//
// Rejection is fail-closed and coded: VOLATILITY_SURFACE_ARBITRAGE
// (HTTP 422, spec §23). SurfaceViolations returns the full audit list
// in deterministic scan order; ValidateSurface fails on the first.
package options

import (
	"fmt"
	"math"
	"sort"

	excerrors "exchange/pkg/errors"
)

// ArbTolerance carries the numerical floors for the two checks. The
// defaults are deliberately tight (1e-12 on total variance, 1e-10 on
// the DF-scaled call price) — a quote set only "barely" arbitraged is
// still arbitraged.
type ArbTolerance struct {
	Calendar  float64 // allowed negative drift in w(k,·) before rejection
	Butterfly float64 // allowed convexity breach on the absolute call price
}

// DefaultArbTolerance is the production tolerance pair.
func DefaultArbTolerance() ArbTolerance {
	return ArbTolerance{Calendar: 1e-12, Butterfly: 1e-10}
}

// ViolationKind tags the arbitrage dimension.
type ViolationKind string

const (
	ViolationCalendar  ViolationKind = "CALENDAR"
	ViolationButterfly ViolationKind = "BUTTERFLY"
)

// SurfaceViolation is one detected arbitrage breach — the audit unit
// written alongside the VOLATILITY_SURFACE_ARBITRAGE rejection.
type SurfaceViolation struct {
	Kind ViolationKind
	T1   float64 // tenor where the breach was found
	T2   float64 // second tenor (calendar only; 0 for butterfly)
	K    float64 // strike (butterfly middle) or log-moneyness (calendar)
	LHS  float64 // observed value
	RHS  float64 // bound it breached
}

// String renders the violation for operator logs.
func (v SurfaceViolation) String() string {
	switch v.Kind {
	case ViolationCalendar:
		return fmt.Sprintf("calendar arb: w(k=%.6f, T=%g)=%.8g > w(T=%g)=%.8g + tol",
			v.K, v.T2, v.RHS, v.T1, v.LHS)
	default:
		return fmt.Sprintf("butterfly arb at T=%g K=%g: C=%.8g > bound %.8g",
			v.T1, v.K, v.LHS, v.RHS)
	}
}

// SurfaceViolations scans the whole surface and returns every breach
// in deterministic order: all calendar violations first (k-grid outer,
// tenor-pair inner), then all butterfly violations (tenor outer,
// strike-triplet inner). An empty result means the surface is clean.
func SurfaceViolations(s *IVSurface, tol ArbTolerance) []SurfaceViolation {
	if s == nil {
		return nil
	}
	var out []SurfaceViolation
	out = append(out, vaCalendarViolations(s, tol.Calendar)...)
	out = append(out, vaButterflyViolations(s, tol.Butterfly)...)
	return out
}

// ValidateSurface is the quote-update gate: any violation rejects the
// surface with VOLATILITY_SURFACE_ARBITRAGE. The error message names
// the first breach and the total count so the rejection is auditable
// without a second pass.
func ValidateSurface(s *IVSurface) error {
	violations := SurfaceViolations(s, DefaultArbTolerance())
	if len(violations) == 0 {
		return nil
	}
	return excerrors.New(CodeVolatilitySurfaceArbitrage,
		fmt.Sprintf("%d surface violation(s): %s", len(violations), violations[0]))
}

// vaCalendarViolations checks w(k,T) non-decreasing in T on the union
// of quoted log-moneynesses.
func vaCalendarViolations(s *IVSurface, tol float64) []SurfaceViolation {
	slices := s.Slices()
	if len(slices) < 2 {
		return nil // flat/ATM rungs carry no term structure to arb
	}
	// Union grid of k = ln(K/F) across slices, sorted + deduped.
	var ks []float64
	for _, sl := range slices {
		for _, strike := range sl.Strikes {
			ks = append(ks, math.Log(strike/sl.Forward))
		}
	}
	sort.Float64s(ks)
	ks = vaDedupeSorted(ks)

	var out []SurfaceViolation
	for _, k := range ks {
		prevW := math.Inf(-1)
		prevT := 0.0
		for _, sl := range slices {
			w, err := s.TotalVariance(sl.T, k)
			if err != nil {
				continue
			}
			if w < prevW-tol {
				out = append(out, SurfaceViolation{
					Kind: ViolationCalendar,
					T1:   prevT, T2: sl.T, K: k,
					LHS: w, RHS: prevW,
				})
			}
			prevW, prevT = w, sl.T
		}
	}
	return out
}

// vaButterflyViolations checks call-price convexity across each
// slice's consecutive strike triplets.
func vaButterflyViolations(s *IVSurface, tol float64) []SurfaceViolation {
	var out []SurfaceViolation
	for _, sl := range s.Slices() {
		strikes := sl.Strikes
		if len(strikes) < 3 {
			continue
		}
		vols := make([]float64, len(strikes))
		for i, k := range strikes {
			v, err := s.Vol(sl.T, k)
			if err != nil {
				continue
			}
			vols[i] = v
		}
		for i := 1; i+1 < len(strikes); i++ {
			k1, k2, k3 := strikes[i-1], strikes[i], strikes[i+1]
			c1 := vaCallForward(sl.Forward, k1, sl.T, vols[i-1], sl.Discount)
			c2 := vaCallForward(sl.Forward, k2, sl.T, vols[i], sl.Discount)
			c3 := vaCallForward(sl.Forward, k3, sl.T, vols[i+1], sl.Discount)
			lambda := (k3 - k2) / (k3 - k1)
			bound := lambda*c1 + (1-lambda)*c3
			if c2 > bound+tol {
				out = append(out, SurfaceViolation{
					Kind: ViolationButterfly,
					T1:   sl.T, K: k2,
					LHS: c2, RHS: bound,
				})
			}
		}
	}
	return out
}

// vaDedupeSorted collapses exact duplicates in an already-sorted
// float slice — union-grid helper keeping the scan deterministic.
func vaDedupeSorted(in []float64) []float64 {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// vaCallForward prices a European call in forward/Black form
// (DF·(F·N(d1) − K·N(d2))) — the numeraire form the butterfly
// convexity check needs at each smile strike.
func vaCallForward(f, k, t, vol, df float64) float64 {
	if t <= 0 || vol <= 0 {
		return math.Max(f-k, 0) * df
	}
	sqrtT := math.Sqrt(t)
	d1 := (math.Log(f/k) + 0.5*vol*vol*t) / (vol * sqrtT)
	d2 := d1 - vol*sqrtT
	return df * (f*gkNormCDF(d1) - k*gkNormCDF(d2))
}
