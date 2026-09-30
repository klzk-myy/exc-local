// ivsurface.go — Phase-22 Task 22.3.15 item 2 / Task 22.3.14 item 1:
// implied-volatility surface construction with a defined fallback
// hierarchy, plus the GK implied-vol solver the surface build consumes
// (spec §15.2, §15.6, §15.7 item 2, §24 #346).
//
// Construction contract:
//   - ATM convention is forward-ATM: log-moneyness k = ln(K/F_T), the
//     "spot-vs-forward" choice of spec §15.2 — smile points are
//     interpolated on (k, w) where w = σ²·T is total implied variance.
//   - Smile interpolation: natural cubic spline in (k, w) with ≥3
//     points; linear-in-total-variance with exactly 2; flat vol with
//     1. Extrapolation beyond the strike range holds the boundary
//     total variance (flat in w) — never negative, never fabricated.
//   - Term structure: linear interpolation in total variance at fixed
//     k between bracketing tenors; outside the tenor grid the nearest
//     slice's vol smile is applied verbatim (flat-vol extrapolation
//     in T).
//
// Fallback hierarchy (§15.7 item 2; Task brief: "never silently
// default to a constant smile") — each downgrade is recorded in
// IVSurface.Source / .DroppedTenors so callers and audits can see
// which rung served the answer:
//
//	SurfaceFull      — ≥2 usable tenor slices, per-tenor interpolation
//	SurfaceFlatTerm  — exactly 1 usable tenor slice (flat term
//	                   structure, smile still varies in k)
//	SurfaceATMOnly   — no usable slices; single ATM vol for all (k,T)
//	failure          — no slices and no ATM vol → coded error
//	                   VOLATILITY_SURFACE_UNAVAILABLE (503)
//
// The yield-curve feed fallback (primary → secondary vendor →
// last-good with age flag, §15.6 item 3) is ResolveCurveSnapshot —
// the pure selection rule Phase-19.5/22.3.1 binding code feeds
// ordered candidates into.
//
// Shared helpers come from types.go (invalidInput / finite /
// curveUnavailable, evalGK via greeks.go); slice internals carry the
// ivs prefix.
package options

import (
	"fmt"
	"math"
	"sort"
	"time"

	excerrors "exchange/pkg/errors"
)

// SmilePoint is one quoted implied vol on a tenor slice.
type SmilePoint struct {
	Strike float64
	Vol    float64 // implied vol, decimal fraction (0.10 = 10%)
}

// TenorSlice is the smile at one expiry. Forward is the outright
// forward F(T) (forward-ATM convention); Discount the quote-ccy
// discount factor P(0,T) — both come from the caller's curve leg
// (ResolveCurveSnapshot output downstream).
type TenorSlice struct {
	T        float64 // years to expiry, >0
	Forward  float64 // outright forward for T, >0
	Discount float64 // P(0,T) discount factor, >0
	Points   []SmilePoint
}

// SurfaceSource records which rung of the fallback hierarchy built
// the surface — part of the §15.7 audit trail, never silent.
type SurfaceSource int

const (
	// SurfaceFull: two or more tenor slices, per-tenor smile
	// interpolation plus term-structure interpolation in total
	// variance. The normal path.
	SurfaceFull SurfaceSource = iota
	// SurfaceFlatTerm: exactly one usable tenor — its smile applies
	// at every T (flat term structure).
	SurfaceFlatTerm
	// SurfaceATMOnly: no usable smiles; a single ATM vol serves all
	// (k,T). The last explicit rung before coded failure.
	SurfaceATMOnly
)

func (s SurfaceSource) String() string {
	switch s {
	case SurfaceFull:
		return "FULL"
	case SurfaceFlatTerm:
		return "FLAT_TERM"
	case SurfaceATMOnly:
		return "ATM_ONLY"
	default:
		return "UNKNOWN"
	}
}

// SurfaceBuildInput is the quote set for one pair plus the ATM
// fallback vol. Validate runs the Task 22.3.14 arbitrage gate
// (volarb.go) on the built surface — the spec §15.6 quote-update
// rejection path; leave false only for diagnostics tooling.
type SurfaceBuildInput struct {
	Tenors   []TenorSlice
	ATMVol   float64 // fallback ATM vol (e.g. last-good or ATM-only quote)
	HasATM   bool    // ATMVol is authoritative
	Validate bool    // run calendar/butterfly arb validation post-build
}

// SurfaceSlice is the read-only projection of one built tenor —
// exposed for the volarb checks and downstream pricing.
type SurfaceSlice struct {
	T        float64
	Forward  float64
	Discount float64
	Strikes  []float64 // sorted ascending
}

// smileMode selects the intra-slice interpolant by point count.
type smileMode int

const (
	smileSpline smileMode = iota // ≥3 points: natural cubic spline in (k,w)
	smileLinear                  // 2 points: linear in total variance
	smileFlat                    // 1 point: flat vol
)

// ivsSlice is the internal per-tenor interpolant state.
type ivsSlice struct {
	t, f, df float64
	k        []float64 // log-moneyness ln(K/F), sorted
	w        []float64 // total variance σ²·T at each k
	m        []float64 // spline second derivatives (spline mode only)
	strikes  []float64
	mode     smileMode
}

// variance returns total implied variance w(k) for the slice.
// Outside [k₀,k_last] the boundary variance is held (flat-w
// extrapolation — deterministic, non-negative by construction).
func (s *ivsSlice) variance(k float64) float64 {
	n := len(s.k)
	if n == 0 {
		return 0
	}
	if k <= s.k[0] {
		return s.w[0]
	}
	if k >= s.k[n-1] {
		return s.w[n-1]
	}
	// Binary search for the bracketing interval.
	lo, hi := 0, n-1
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if s.k[mid] <= k {
			lo = mid
		} else {
			hi = mid
		}
	}
	x0, x1 := s.k[lo], s.k[hi]
	y0, y1 := s.w[lo], s.w[hi]
	h := x1 - x0
	switch s.mode {
	case smileSpline:
		// Natural cubic spline in (k,w):
		// S(x) = M_i(x1−x)³/6h + M_{i+1}(x−x0)³/6h
		//        + (y_i − M_i·h²/6)(x1−x)/h + (y_{i+1} − M_{i+1}·h²/6)(x−x0)/h
		a := (x1 - k) / h
		b := (k - x0) / h
		return a*y0 + b*y1 +
			((a*a*a-a)*s.m[lo]+(b*b*b-b)*s.m[hi])*h*h/6.0
	default: // smileLinear, and smileFlat is unreachable (n≥2 here)
		return y0 + (y1-y0)*(k-x0)/h
	}
}

func (s *ivsSlice) vol(k float64) float64 {
	return math.Sqrt(math.Max(s.variance(k), 0) / s.t)
}

// IVSurface is a built implied-volatility surface for one FX pair.
// Immutable after BuildIVSurface — all queries are pure functions of
// the embedded slices (deterministic replay, spec §15.7 item 3).
type IVSurface struct {
	slices  []ivsSlice // sorted by T
	source  SurfaceSource
	atmVol  float64
	dropped []float64 // tenors discarded for having no usable points
}

// Source reports which fallback rung produced the surface.
func (s *IVSurface) Source() SurfaceSource { return s.source }

// DroppedTenors lists the input expiries discarded because they
// carried no usable smile points — audit visibility into coverage
// gaps (§15.7: downgrades are never silent).
func (s *IVSurface) DroppedTenors() []float64 {
	out := make([]float64, len(s.dropped))
	copy(out, s.dropped)
	return out
}

// Slices returns the read-only per-tenor projection for consumers
// (volarb checks, downstream pricers).
func (s *IVSurface) Slices() []SurfaceSlice {
	out := make([]SurfaceSlice, len(s.slices))
	for i := range s.slices {
		sl := &s.slices[i]
		strikes := make([]float64, len(sl.strikes))
		copy(strikes, sl.strikes)
		out[i] = SurfaceSlice{
			T: sl.t, Forward: sl.f, Discount: sl.df, Strikes: strikes,
		}
	}
	return out
}

// Vol evaluates the surface at (T, strike). T>0 and strike>0 are
// enforced — a malformed query is a coded rejection, not a NaN.
func (s *IVSurface) Vol(t, strike float64) (float64, error) {
	const op = "IVSurface.Vol"
	if s == nil || (len(s.slices) == 0 && s.source != SurfaceATMOnly) {
		return 0, excerrors.New(CodeVolatilitySurfaceUnavailable,
			"iv surface: query on empty surface")
	}
	if !finite(t) || !finite(strike) || t <= 0 || strike <= 0 {
		return 0, invalidInput(op, fmt.Sprintf(
			"non-positive/non-finite T=%v or strike=%v", t, strike))
	}
	if s.source == SurfaceATMOnly {
		return s.atmVol, nil
	}

	sl := s.bracketing(t)
	// Beyond the grid ends: nearest slice's smile verbatim (flat-vol
	// extrapolation in T — documented build contract).
	if sl < 0 {
		idx := 0
		if t > s.slices[len(s.slices)-1].t {
			idx = len(s.slices) - 1
		}
		k := math.Log(strike / s.slices[idx].f)
		return s.slices[idx].vol(k), nil
	}
	a, b := sl, sl+1
	ua := (t - s.slices[a].t) / (s.slices[b].t - s.slices[a].t)
	// Forward for the intermediate T: log-linear between the
	// bracketing outrights — deterministic and monotone.
	lnF := math.Log(s.slices[a].f) + ua*(math.Log(s.slices[b].f)-math.Log(s.slices[a].f))
	k := math.Log(strike / math.Exp(lnF))
	w := (1-ua)*s.slices[a].variance(k) + ua*s.slices[b].variance(k)
	return math.Sqrt(math.Max(w, 0) / t), nil
}

// TotalVariance evaluates w = σ²·T at (T, log-moneyness k) — the
// quantity the calendar no-arb check (volarb.go) inspects. k is the
// caller's log-moneyness ln(K/F) in the surface's forward-ATM
// convention; slices interpolate their own (k,w) grids directly.
func (s *IVSurface) TotalVariance(t, k float64) (float64, error) {
	const op = "IVSurface.TotalVariance"
	if s == nil || (len(s.slices) == 0 && s.source != SurfaceATMOnly) {
		return 0, excerrors.New(CodeVolatilitySurfaceUnavailable,
			"iv surface: query on empty surface")
	}
	if !finite(t) || !finite(k) || t <= 0 {
		return 0, invalidInput(op, fmt.Sprintf(
			"non-positive/non-finite T=%v or k=%v", t, k))
	}
	if s.source == SurfaceATMOnly {
		return s.atmVol * s.atmVol * t, nil
	}
	sl := s.bracketing(t)
	if sl < 0 {
		idx := 0
		if t > s.slices[len(s.slices)-1].t {
			idx = len(s.slices) - 1
		}
		// Flat-vol extrapolation: rescale the boundary smile's vol to
		// the query tenor's total variance.
		return s.slices[idx].variance(k) / s.slices[idx].t * t, nil
	}
	a, b := sl, sl+1
	ua := (t - s.slices[a].t) / (s.slices[b].t - s.slices[a].t)
	return (1-ua)*s.slices[a].variance(k) + ua*s.slices[b].variance(k), nil
}

// bracketing returns i such that t ∈ [slices[i].t, slices[i+1].t], or
// −1 when t lies outside the tenor grid.
func (s *IVSurface) bracketing(t float64) int {
	for i := 0; i+1 < len(s.slices); i++ {
		if t >= s.slices[i].t && t <= s.slices[i+1].t {
			return i
		}
	}
	return -1
}

// BuildIVSurface constructs the surface under the fallback hierarchy.
// Every rung transition is recorded (Source, DroppedTenors); total
// exhaustion is a coded error — the surface never silently defaults.
func BuildIVSurface(in SurfaceBuildInput) (*IVSurface, error) {
	const op = "BuildIVSurface"
	if len(in.Tenors) == 0 && (!in.HasATM || in.ATMVol <= 0) {
		return nil, excerrors.New(CodeVolatilitySurfaceUnavailable,
			"iv surface: no tenor slices and no ATM fallback")
	}

	// Validate + sort tenors. Duplicate expiries are a malformed
	// surface (ambiguous w(T)); reject rather than merge silently.
	sorted := make([]TenorSlice, len(in.Tenors))
	copy(sorted, in.Tenors)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].T < sorted[j].T })
	var usable []TenorSlice
	var dropped []float64
	for i, sl := range sorted {
		if !finite(sl.T) || sl.T <= 0 {
			return nil, invalidInput(op, fmt.Sprintf(
				"tenor %d non-positive/non-finite T=%v", i, sl.T))
		}
		if i > 0 && sl.T == sorted[i-1].T {
			return nil, invalidInput(op, fmt.Sprintf("duplicate tenor T=%g", sl.T))
		}
		if !finite(sl.Forward) || sl.Forward <= 0 ||
			!finite(sl.Discount) || sl.Discount <= 0 {
			return nil, invalidInput(op, fmt.Sprintf(
				"tenor T=%g bad forward=%v discount=%v", sl.T, sl.Forward, sl.Discount))
		}
		if len(sl.Points) == 0 {
			dropped = append(dropped, sl.T)
			continue
		}
		usable = append(usable, sl)
	}

	surface := &IVSurface{dropped: dropped}
	switch {
	case len(usable) >= 2:
		surface.source = SurfaceFull
	case len(usable) == 1:
		surface.source = SurfaceFlatTerm
	default:
		// ATM-only rung — requires an authoritative ATM vol.
		if in.HasATM && finite(in.ATMVol) && in.ATMVol > 0 {
			surface.source = SurfaceATMOnly
			surface.atmVol = in.ATMVol
			return surface, nil
		}
		return nil, excerrors.New(CodeVolatilitySurfaceUnavailable,
			"iv surface: fallback hierarchy exhausted (no usable slices, no ATM vol)")
	}

	for _, sl := range usable {
		built, err := ivsBuildSlice(sl)
		if err != nil {
			return nil, err
		}
		surface.slices = append(surface.slices, *built)
	}

	if in.Validate {
		if err := ValidateSurface(surface); err != nil {
			return nil, err
		}
	}
	return surface, nil
}

// ivsBuildSlice sorts/dedupes the smile and precomputes the spline.
// Invalid quotes are coded rejections — a bad point must not silently
// reshape the smile.
func ivsBuildSlice(sl TenorSlice) (*ivsSlice, error) {
	const op = "BuildIVSurface.slice"
	pts := make([]SmilePoint, len(sl.Points))
	copy(pts, sl.Points)
	sort.Slice(pts, func(i, j int) bool { return pts[i].Strike < pts[j].Strike })
	out := &ivsSlice{t: sl.T, f: sl.Forward, df: sl.Discount}
	for i, p := range pts {
		if !finite(p.Strike) || p.Strike <= 0 || !finite(p.Vol) || p.Vol <= 0 {
			return nil, invalidInput(op, fmt.Sprintf(
				"tenor %g point %d bad strike=%v vol=%v", sl.T, i, p.Strike, p.Vol))
		}
		k := math.Log(p.Strike / sl.Forward)
		if len(out.k) > 0 && k == out.k[len(out.k)-1] {
			return nil, invalidInput(op, fmt.Sprintf(
				"tenor %g duplicate strike %g", sl.T, p.Strike))
		}
		out.k = append(out.k, k)
		out.w = append(out.w, p.Vol*p.Vol*sl.T)
		out.strikes = append(out.strikes, p.Strike)
	}
	switch len(pts) {
	case 1:
		out.mode = smileFlat
	case 2:
		out.mode = smileLinear
	default:
		out.mode = smileSpline
		out.m = ivsNaturalSpline(out.k, out.w)
	}
	return out, nil
}

// ivsNaturalSpline solves the natural cubic spline second-derivative
// vector M for knots (x,y) — Thomas algorithm on the standard
// tridiagonal system, natural boundary M₀ = M_{n-1} = 0. Deterministic:
// fixed operation count, no pivoting ambiguity for strictly increasing x.
func ivsNaturalSpline(x, y []float64) []float64 {
	n := len(x)
	m := make([]float64, n)
	if n < 3 {
		return m
	}
	h := make([]float64, n-1)
	for i := 0; i < n-1; i++ {
		h[i] = x[i+1] - x[i]
	}
	// Lower/diag/upper tridiagonal rows for i = 1..n-2.
	lower := make([]float64, n-2)
	diag := make([]float64, n-2)
	upper := make([]float64, n-2)
	rhs := make([]float64, n-2)
	for i := 1; i <= n-2; i++ {
		r := i - 1
		lower[r] = h[i-1]
		diag[r] = 2 * (h[i-1] + h[i])
		upper[r] = h[i]
		rhs[r] = 6 * ((y[i+1]-y[i])/h[i] - (y[i]-y[i-1])/h[i-1])
	}
	// Thomas forward sweep + back substitution.
	for i := 1; i < n-2; i++ {
		w := lower[i] / diag[i-1]
		diag[i] -= w * upper[i-1]
		rhs[i] -= w * rhs[i-1]
	}
	if n-2 > 0 {
		m[n-2] = rhs[n-3] / diag[n-3]
		for i := n - 3; i >= 1; i-- {
			r := i - 1
			m[i] = (rhs[r] - upper[r]*m[i+1]) / diag[r]
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// GK implied-vol solver (Task 22.3.14 item 1 — spec §15.6 solver bounds)
// ---------------------------------------------------------------------------

// ivMaxIterations is the spec §15.6 Newton-Raphson ceiling: 100
// iterations, then OPTION_PRICING_CONVERGENCE_ERROR.
const ivMaxIterations = 100

// ImpliedVolGK solves for the implied vol matching a European GK
// option price for (right, strike) under market m — the price→vol
// inversion the surface build performs on quoted option premiums.
// m.Vol is ignored (it is the unknown being solved for).
//
// Newton-Raphson on price with a bisection bracket [1e-9, 10]
// guarding zero-vega / out-of-bracket iterates; both share the
// 100-iteration budget of spec §15.6. Out-of-bounds targets are input
// failures (ErrInvalidInput → INVALID_REQUEST); exhausted budgets are
// convergence failures (ErrNonConvergence →
// OPTION_PRICING_CONVERGENCE_ERROR) — never a NaN vol.
func ImpliedVolGK(target float64, right OptionRight, strike float64, m Market) (float64, error) {
	return ivsImpliedVol(target, right, strike, m, ivMaxIterations)
}

// ivsImpliedVol is the solver core with an explicit iteration budget —
// split out so tests can exercise the non-convergence path without
// waiting for the spec ceiling.
func ivsImpliedVol(target float64, right OptionRight, strike float64, m Market,
	maxIter int) (float64, error) {
	const op = "ImpliedVolGK"
	if !right.Valid() {
		return 0, invalidInput(op, fmt.Sprintf("option right must be CALL|PUT, got %q", right))
	}
	if !finite(strike) || strike <= 0 {
		return 0, invalidInput(op, fmt.Sprintf("strike must be positive and finite, got %v", strike))
	}
	// m.Vol is the solve variable — validate the rest of the market
	// shape (spot/DFs/T) via a benign stand-in so m.validate applies.
	check := m
	check.Vol = 0.10
	if err := check.validate(op); err != nil {
		return 0, err
	}
	if m.T <= 0 {
		return 0, invalidInput(op, "time to expiry must be > 0 for IV solving")
	}
	if !finite(target) || maxIter <= 0 {
		return 0, invalidInput(op, fmt.Sprintf(
			"non-finite target %v or budget %d", target, maxIter))
	}

	// Rational no-arb bounds on the target price (DF-form):
	// call ∈ [max(S·DFf − K·DFd, 0), S·DFf]; put ∈ [max(K·DFd − S·DFf, 0), K·DFd].
	var lo, hi float64
	if right == OptionPut {
		lo = math.Max(strike*m.DFd-m.Spot*m.DFf, 0)
		hi = strike * m.DFd
	} else {
		lo = math.Max(m.Spot*m.DFf-strike*m.DFd, 0)
		hi = m.Spot * m.DFf
	}
	if target < lo-1e-12 || target > hi+1e-12 {
		return 0, invalidInput(op, fmt.Sprintf(
			"target %g outside rational bounds [%g,%g]", target, lo, hi))
	}
	if target <= lo+1e-14 {
		return 0, nonConvergence(op,
			"target at lower bound — vol undefined at the boundary")
	}

	// Hybrid NR+bisection on bracket [loV, hiV].
	priceAt := func(v float64) (float64, gkCore) {
		mk := m
		mk.Vol = v
		c := evalGK(mk, strike)
		return c.price(mk, right, strike), c
	}
	loV, hiV := 1e-9, 10.0
	sig := 0.25
	for i := 0; i < maxIter; i++ {
		p, c := priceAt(sig)
		diff := p - target
		if math.Abs(diff) <= 1e-10*math.Max(1, math.Abs(target)) {
			return sig, nil
		}
		// Maintain the bracket on the sign of the residual.
		if diff > 0 {
			hiV = sig
		} else {
			loV = sig
		}
		// Vega in DF form: S·DFf·φ(d1)·√T — the same gkCore the price
		// came from, so gradient and value never disagree.
		vega := m.Spot * m.DFf * c.pd1 * math.Sqrt(m.T)
		var next float64
		if vega > 1e-12 {
			next = sig - diff/vega
		}
		if !(next > loV && next < hiV) { // NR stepped out or vega stalled
			next = 0.5 * (loV + hiV)
		}
		sig = next
	}
	return 0, nonConvergence(op, fmt.Sprintf(
		"no convergence within %d iterations (bracket [%.3g, %.3g])", maxIter, loV, hiV))
}

// ---------------------------------------------------------------------------
// Yield-curve feed fallback (spec §15.6 item 3, §15.7 item 2)
// ---------------------------------------------------------------------------

// CurveCandidate is one supplier's curve snapshot presented to the
// fallback resolver. Callers order candidates by priority — the
// §15.6 hierarchy is positional: [primary, secondary vendor, ...,
// last-good]. Valid is the producer's completeness flag (a partial
// curve never serves, matching oracle/rates' publish rule).
type CurveCandidate struct {
	Label string
	AsOf  time.Time
	Valid bool
}

// CurveSelection is the resolver's deterministic verdict: which
// candidate serves, and whether it is a flagged stale (last-good)
// fallback. Callers propagate Stale into the quote/surface provenance.
type CurveSelection struct {
	Index int           // index into the candidate slice
	Label string        // winning candidate's provenance label
	Age   time.Duration // now − AsOf
	Stale bool          // true → last-good leg: served past the gate
}

// ResolveCurveSnapshot implements the §15.6 item-3 hierarchy:
// the first valid candidate inside the staleness gate wins fresh;
// otherwise the first valid candidate at any age serves flagged
// Stale (the "last-good with an age flag" leg); no valid candidate
// at all → YIELD_CURVE_UNAVAILABLE (HTTP 503). Candidates are
// evaluated strictly in priority order — deterministic by
// construction; equal inputs always resolve identically.
func ResolveCurveSnapshot(now time.Time, gate time.Duration,
	cands []CurveCandidate) (CurveSelection, error) {
	if gate <= 0 {
		gate = 5 * time.Second // spec §15.6 feed-outage gate
	}
	for i, c := range cands {
		if !c.Valid {
			continue
		}
		age := now.Sub(c.AsOf)
		if age <= gate && age >= -gate {
			return CurveSelection{Index: i, Label: c.Label, Age: age}, nil
		}
	}
	for i, c := range cands {
		if !c.Valid {
			continue
		}
		return CurveSelection{
			Index: i, Label: c.Label, Age: now.Sub(c.AsOf), Stale: true,
		}, nil
	}
	return CurveSelection{}, curveUnavailable("ResolveCurveSnapshot",
		"yield-curve fallback exhausted: no valid candidate snapshot")
}
