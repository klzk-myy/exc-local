// american.go — Phase-22 Task 22.3.15 item 1 / Task 22.3.14 item 1:
// American-exercise FX option pricing on a Kamrad–Ritchken trinomial
// lattice with an explicit early-exercise boundary (spec §15.2
// amendment, §15.7 item 1, §24 #346).
//
// Contract:
//   - GK dynamics: dS/S = (rd−rf)·dt + σ·dW, with rd/rf recovered
//     from the Market discount factors (m.Rates()) — same market
//     convention as the sibling vanilla/MC engines (types.go).
//   - Lattice bounds per spec §15.2: ≥100 steps, ≤10,000 steps,
//     relative convergence tolerance 1e-4 on successive refinements;
//     abort at the ceiling → OPTION_PRICING_CONVERGENCE_ERROR (HTTP
//     422), never a NaN/∞ leak (Strict Fail-Closed, spec §2.7).
//   - The European leg is produced by the identical lattice with the
//     exercise clamp disabled, so American ≥ European is testable on
//     the same discretization; VanillaOption.Price is the closed-form
//     reference when early exercise carries no premium.
//
// VanillaOption.validate rejects AMERICAN by design — that refusal is
// what routes American books here.
package options

import (
	"fmt"
	"math"
)

// LatticeConfig bounds the trinomial refinement loop (spec §15.2:
// minimum 100 steps, maximum 10,000, tolerance 0.01% of option value).
type LatticeConfig struct {
	MinSteps  int     // first lattice depth; spec floor is 100
	MaxSteps  int     // hard abort ceiling; spec cap is 10,000
	Tolerance float64 // |V(n)−V(2n)|/V(n/2) convergence tolerance; spec 1e-4
}

// DefaultLatticeConfig is the spec §15.2 production bound set.
var DefaultLatticeConfig = LatticeConfig{MinSteps: 100, MaxSteps: 10_000, Tolerance: 1e-4}

// ExerciseBoundaryPoint is one node on the discrete early-exercise
// boundary: at Time (years from valuation), exercising is optimal at
// or beyond Spot (at-or-below for a put, at-or-above for a call).
type ExerciseBoundaryPoint struct {
	Time float64 `json:"time"`
	Spot float64 `json:"spot"`
}

// LatticeResult is the priced option plus the audit trail §24 #346
// demands: converged step count, last relative change, the matched
// European leg, and the discrete early-exercise boundary.
type LatticeResult struct {
	Price     float64                 `json:"price"`      // American value, quote ccy per unit base
	Steps     int                     `json:"steps"`      // converged lattice depth
	RelChange float64                 `json:"rel_change"` // |V(n)−V(n/2)|/V(n/2) at convergence
	European  float64                 `json:"european"`   // same lattice, exercise clamp off
	Boundary  []ExerciseBoundaryPoint `json:"boundary"`   // Time-ascending; empty when never optimal
}

// PriceAmerican prices an American-style option on the trinomial
// lattice, refining (doubling) the step count until the relative
// change is inside cfg.Tolerance. o.Style must be ExerciseAmerican —
// European books route to VanillaOption.Price / PriceEuropeanLattice.
// Non-convergence at cfg.MaxSteps returns
// OPTION_PRICING_CONVERGENCE_ERROR — deterministic abort, no partial
// value (spec §15.2 abort rule, §2.7 fail-closed).
func PriceAmerican(o VanillaOption, m Market, cfg LatticeConfig) (LatticeResult, error) {
	const op = "PriceAmerican"
	if err := validateLatticeInput(op, o, m, true); err != nil {
		return LatticeResult{}, err
	}
	if cfg.MinSteps < 4 || cfg.MaxSteps < cfg.MinSteps || !finite(cfg.Tolerance) ||
		cfg.Tolerance <= 0 {
		return LatticeResult{}, invalidInput(op, fmt.Sprintf(
			"lattice config out of bounds: min=%d max=%d tol=%v",
			cfg.MinSteps, cfg.MaxSteps, cfg.Tolerance))
	}

	prev, _, err := amLattice(o, m, cfg.MinSteps, true)
	if err != nil {
		return LatticeResult{}, err
	}
	n := cfg.MinSteps
	for {
		next := 2 * n
		if next > cfg.MaxSteps {
			next = cfg.MaxSteps
		}
		cur, bnd, err := amLattice(o, m, next, true)
		if err != nil {
			return LatticeResult{}, err
		}
		denom := math.Max(math.Abs(prev), 1e-12)
		rel := math.Abs(cur-prev) / denom
		if rel <= cfg.Tolerance || next == cfg.MinSteps {
			eur, _, err := amLattice(o, m, next, false)
			if err != nil {
				return LatticeResult{}, err
			}
			return LatticeResult{
				Price: cur, Steps: next, RelChange: rel,
				European: eur, Boundary: bnd,
			}, nil
		}
		if next >= cfg.MaxSteps {
			return LatticeResult{}, nonConvergence(op, fmt.Sprintf(
				"trinomial lattice failed to converge by the %d-step ceiling (last rel change %g)",
				cfg.MaxSteps, rel))
		}
		prev, n = cur, next
	}
}

// PriceEuropeanLattice prices the European leg on a single lattice of
// the given depth (exercise clamp off) — the like-for-like European
// bound on the same grid; VanillaOption.Price is the closed form.
func PriceEuropeanLattice(o VanillaOption, m Market, steps int) (float64, error) {
	const op = "PriceEuropeanLattice"
	if err := validateLatticeInput(op, o, m, false); err != nil {
		return 0, err
	}
	if steps < 4 {
		return 0, invalidInput(op, fmt.Sprintf("steps must be >= 4, got %d", steps))
	}
	v, _, err := amLattice(o, m, steps, false)
	return v, err
}

// validateLatticeInput enforces the fail-closed contract for both
// lattice entry points: valid right/strike per the shared option
// rules, a validated Market with strictly positive T (a lattice of
// zero duration is a payoff, not a pricing problem), and — when
// requireAmerican is set — the AMERICAN exercise style that routes
// here in the first place.
func validateLatticeInput(op string, o VanillaOption, m Market, requireAmerican bool) error {
	if !o.Right.Valid() {
		return invalidInput(op, fmt.Sprintf("option right must be CALL|PUT, got %q", o.Right))
	}
	if !finite(o.Strike) || o.Strike <= 0 {
		return invalidInput(op, fmt.Sprintf("strike must be positive and finite, got %v", o.Strike))
	}
	if requireAmerican && o.Style != ExerciseAmerican {
		return invalidInput(op, fmt.Sprintf(
			"exercise style %q — PriceAmerican requires AMERICAN; European options price by GK closed form", o.Style))
	}
	if err := m.validate(op); err != nil {
		return err
	}
	if m.T <= 0 {
		return invalidInput(op, "time to expiry must be > 0 for lattice pricing")
	}
	return nil
}

// amLattice runs one Kamrad–Ritchken trinomial pass over X = ln S with
// space step h = σ√(3·dt) and drift ν = (rd−rf) − σ²/2. Moment-matched
// probabilities: pu = 1/6 + ν√(dt/12)/σ, pd = 1/6 − ν√(dt/12)/σ,
// pm = 2/3 (λ = √3 stretch). When american is set the exercise clamp
// max(continuation, intrinsic) applies per node and the boundary is
// collected inline — it must be captured during the write sweep,
// before the level-i values overwrite the level-(i+1) continuation
// inputs the optimality test compares against.
//
// Returns the option value, the discrete early-exercise boundary
// (empty when american is false or no node is exercise-optimal), or a
// coded error on probability violation / non-finite state — the
// trinomial stretch cannot represent |ν|√(dt/12)/σ > 1/6, which is a
// numerical-regime failure per spec §15.2 (abort → convergence code),
// not a client fault.
func amLattice(o VanillaOption, m Market, n int, american bool) (float64, []ExerciseBoundaryPoint, error) {
	const op = "amLattice"
	rd, rf := m.Rates()
	dt := m.T / float64(n)
	h := m.Vol * math.Sqrt(3*dt)
	nu := (rd - rf) - 0.5*m.Vol*m.Vol
	driftTerm := nu * math.Sqrt(dt/12) / m.Vol
	pu := 1.0/6.0 + driftTerm
	pd := 1.0/6.0 - driftTerm
	const pm = 2.0 / 3.0
	if pu < 0 || pd < 0 || pu > 1 || pd > 1 {
		return 0, nil, nonConvergence(op, fmt.Sprintf(
			"trinomial probabilities out of bounds at %d steps (pu=%g pd=%g) — drift/volatility regime unstable",
			n, pu, pd))
	}
	disc := math.Exp(-rd * dt)

	intrinsic := func(spot float64) float64 {
		if o.Right == OptionPut {
			return math.Max(o.Strike-spot, 0)
		}
		return math.Max(spot-o.Strike, 0)
	}

	// Two buffers ping-pong down the induction: a trinomial node j
	// reads level-(i+1) children {j−1, j, j+1}, so a single in-place
	// array corrupts whichever child a same-level write already
	// overwrote. next[j+n] holds the value at node j of the level just
	// completed; cur receives the level being swept.
	next := make([]float64, 2*n+1)
	cur := make([]float64, 2*n+1)
	for j := -n; j <= n; j++ {
		next[j+n] = intrinsic(m.Spot * math.Exp(float64(j)*h))
	}

	// Backward induction. For a put the boundary is the highest
	// exercise-optimal node (bj tracks max j); for a call the lowest
	// (bj tracks min j). ex>0 filters the degenerate deep-OTM nodes
	// where intrinsic == continuation == 0 would fabricate a boundary.
	put := o.Right == OptionPut
	var boundary []ExerciseBoundaryPoint
	for i := n - 1; i >= 0; i-- {
		bj, found := 0, false
		for j := -i; j <= i; j++ {
			idx := j + n
			cont := disc * (pu*next[idx+1] + pm*next[idx] + pd*next[idx-1])
			if american {
				if ex := intrinsic(m.Spot * math.Exp(float64(j)*h)); ex > 0 && ex >= cont {
					if put {
						if !found || j > bj {
							bj, found = j, true
						}
					} else {
						if !found || j < bj {
							bj, found = j, true
						}
					}
					cont = ex
				}
			}
			cur[idx] = cont
		}
		if found {
			boundary = append(boundary, ExerciseBoundaryPoint{
				Time: float64(i) * dt,
				Spot: m.Spot * math.Exp(float64(bj)*h),
			})
		}
		next, cur = cur, next
	}
	if _, err := checkResult(op, next[n]); err != nil {
		return 0, nil, err
	}
	// Boundary collected innermost-step-first → reverse for the
	// canonical Time-ascending audit ordering.
	for l, r := 0, len(boundary)-1; l < r; l, r = l+1, r-1 {
		boundary[l], boundary[r] = boundary[r], boundary[l]
	}
	return next[n], boundary, nil
}
