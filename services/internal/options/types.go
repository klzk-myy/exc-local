// Package options implements the Phase-22 Tasks 22.3.4–22.3.6 FX option
// pricing engines (spec §15.1–§15.3, §24 #60–#62):
//
//   - VanillaOption — European FX options priced by the
//     Black-Scholes-Garman-Kohlhagen closed form (Task 22.3.4, §24 #60)
//     with full Greeks (delta, gamma, vega, theta, rho; §24 #62).
//     AMERICAN exercise style is deliberately refused here: spec §15.2
//     prices Americans on a lattice with an early-exercise boundary —
//     that pricer lives in american.go (Task 22.3.15, sibling-owned).
//   - BarrierOption — UP_AND_IN / UP_AND_OUT / DOWN_AND_IN /
//     DOWN_AND_OUT knock options priced by deterministic Monte Carlo
//     (Task 22.3.5, §24 #61, spec §15.2 "Monte Carlo for
//     barrier/binary"), plus BarrierMonitor — the discrete mark-tick
//     knock evaluation contract of spec §15.7 item 3.
//   - BinaryOption — cash-or-nothing / asset-or-nothing European
//     digitals with fixed cash-settled payout (Task 22.3.6), priced by
//     Monte Carlo with a closed-form reference.
//   - MCPricer — seeded, antithetic-variate GBM Monte Carlo
//     (spec §15.2: ≥100,000 simulations for path-dependent products).
//
// Fail-closed contract (spec §2.7, §15.6, §24 #324): every pricing
// entry point validates its inputs, bounds its work, and returns a
// coded *errors.Error — never a NaN, an Inf, or a partial result.
// Numerical failures map to the registered code
// OPTION_PRICING_CONVERGENCE_ERROR (HTTP 422; registry owner
// Phase-22 Task 22.3.14 — this package emits it, the sibling task owns
// the spec row).
//
// Fiat pairs only (spec §1 — no crypto); all quantities are float64
// rate/vol inputs. Callers convert decimal.Decimal market data at the
// boundary (decimal is the wire/persistence standard, spec §5.3; the
// pricing kernel is a float64 numerical domain like oracle/rates).
package options

import (
	stderrors "errors"
	"fmt"
	"math"
	"strings"

	"exchange/internal/oracle/rates"
	pkgerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Error contract (spec §2.7 fail-closed; §15.6; §24 #324)
// ---------------------------------------------------------------------------

// Error-code constants for the codes this package emits — the shared
// block sibling files reference (determinism.go additionally declares
// the two lifecycle codes it owns). OPTION_PRICING_CONVERGENCE_ERROR,
// YIELD_CURVE_UNAVAILABLE, VOLATILITY_SURFACE_ARBITRAGE,
// CONDITIONAL_TRIGGER_ORACLE_STALE and INVALID_REQUEST are spec §23
// rows; OPTION_PRICING_INPUT_INVALID and VOLATILITY_SURFACE_UNAVAILABLE
// ride the errs localCodes append-only list pending §23 transcription.
// Registry owner for the convergence/surface cluster: Phase-22 Task
// 22.3.14 (spec §15.6, §24 #324) — this package emits the codes, that
// task owns the spec rows.
const (
	// CodeOptionPricingConvergence — IV-solver/lattice/MC non-convergence
	// (spec §23: HTTP 422, remediation #15).
	CodeOptionPricingConvergence = "OPTION_PRICING_CONVERGENCE_ERROR"
	// CodeOptionPricingInputInvalid — malformed pricing inputs
	// (non-positive/non-finite spot, strike, tenor, vol, out-of-bounds
	// target price, malformed enum). localCodes: Phase-22 Task 22.3.15.
	CodeOptionPricingInputInvalid = "OPTION_PRICING_INPUT_INVALID"
	// CodeYieldCurveUnavailable — curve fallback hierarchy exhausted
	// (spec §23: HTTP 503, Phase-19.5 Task 19.5.3.5).
	CodeYieldCurveUnavailable = "YIELD_CURVE_UNAVAILABLE"
	// CodeVolatilitySurfaceUnavailable — IV-surface fallback hierarchy
	// exhausted. localCodes: Phase-22 Task 22.3.14.
	CodeVolatilitySurfaceUnavailable = "VOLATILITY_SURFACE_UNAVAILABLE"
	// CodeVolatilitySurfaceArbitrage — calendar/butterfly breach
	// (spec §23: HTTP 422, remediation #15).
	CodeVolatilitySurfaceArbitrage = "VOLATILITY_SURFACE_ARBITRAGE"
	// CodeConditionalTriggerOracleStale — barrier/trigger evaluation on
	// a mark outside the 5s staleness gate (spec §23: HTTP 409).
	CodeConditionalTriggerOracleStale = "CONDITIONAL_TRIGGER_ORACLE_STALE"
)

// ErrNonConvergence is the sentinel wrapped by every numerical failure
// (NaN/Inf result, MC degenerate variance, solver abort). errors.Is
// against it identifies the fail-closed pricing path; the emitted code
// is always CodeOptionPricingConvergence.
var ErrNonConvergence = stderrors.New("options: numerical solver failed to converge")

// ErrInvalidInput is the sentinel wrapped by every input-validation
// failure (non-positive spot/strike/vol, malformed enum, non-finite
// input). The emitted code is CodeOptionPricingInputInvalid.
var ErrInvalidInput = stderrors.New("options: invalid pricing input")

// ErrCurveUnavailable is re-exported so option pricers and their callers
// share one staleness sentinel with oracle/rates (spec §15.6.3).
var ErrCurveUnavailable = rates.ErrCurveUnavailable

// DeclaredCodes lists every error code this package can emit — the
// wiring layer asserts them via errs.Registry.CheckRegistered at
// startup (Task 5.3.21 fail-closed emission gate).
func DeclaredCodes() []string {
	return []string{
		CodeOptionPricingConvergence, CodeOptionPricingInputInvalid,
		CodeYieldCurveUnavailable, CodeVolatilitySurfaceUnavailable,
		CodeVolatilitySurfaceArbitrage, CodeConditionalTriggerOracleStale,
		CodeExerciseCutoffPassed, CodeOptionAssignmentFailed,
	}
}

// invalidInput builds a coded OPTION_PRICING_INPUT_INVALID error
// wrapping the ErrInvalidInput sentinel.
func invalidInput(op, msg string) error {
	return pkgerrors.Wrap(CodeOptionPricingInputInvalid, "options."+op+": "+msg, ErrInvalidInput)
}

// nonConvergence builds a coded OPTION_PRICING_CONVERGENCE_ERROR error
// wrapping the ErrNonConvergence sentinel — the §24 #324 fail path.
func nonConvergence(op, msg string) error {
	return pkgerrors.Wrap(CodeOptionPricingConvergence, "options."+op+": "+msg, ErrNonConvergence)
}

// curveUnavailable builds a coded YIELD_CURVE_UNAVAILABLE error wrapping
// the shared ErrCurveUnavailable sentinel.
func curveUnavailable(op, msg string) error {
	return pkgerrors.Wrap(CodeYieldCurveUnavailable, "options."+op+": "+msg, ErrCurveUnavailable)
}

// finite reports whether x is a usable real number — NaN/±Inf inputs are
// the raw material of garbage pricing and are always rejected.
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// ---------------------------------------------------------------------------
// Enums — spec §5.4 canonical orders-table vocabulary
// ---------------------------------------------------------------------------

// OptionRight is the orders.option_type CALL/PUT vocabulary (spec §5.4).
type OptionRight string

const (
	OptionCall OptionRight = "CALL"
	OptionPut  OptionRight = "PUT"
)

// Valid reports whether r is a declared right.
func (r OptionRight) Valid() bool { return r == OptionCall || r == OptionPut }

// ParseOptionRight normalizes a CALL/PUT string (case-insensitive).
func ParseOptionRight(s string) (OptionRight, error) {
	switch r := OptionRight(strings.ToUpper(strings.TrimSpace(s))); r {
	case OptionCall, OptionPut:
		return r, nil
	}
	return "", invalidInput("ParseOptionRight", fmt.Sprintf("unknown option right %q — want CALL|PUT", s))
}

// ExerciseStyle is the orders.exercise_style vocabulary (spec §5.4).
// Bermudan is deliberately out of scope (spec §5.4 note).
type ExerciseStyle string

const (
	ExerciseEuropean ExerciseStyle = "EUROPEAN"
	ExerciseAmerican ExerciseStyle = "AMERICAN"
)

// Valid reports whether s is a declared exercise style.
func (s ExerciseStyle) Valid() bool { return s == ExerciseEuropean || s == ExerciseAmerican }

// BarrierType is the spec §5.4 orders.barrier_type enum — the canonical
// strings are UP_AND_IN / UP_AND_OUT / DOWN_AND_IN / DOWN_AND_OUT. The
// plan-prose shorthand (UP_IN etc., Phase-22 Task 22.3.9) is accepted on
// parse but never stored or emitted.
type BarrierType string

const (
	BarrierUpAndIn    BarrierType = "UP_AND_IN"
	BarrierUpAndOut   BarrierType = "UP_AND_OUT"
	BarrierDownAndIn  BarrierType = "DOWN_AND_IN"
	BarrierDownAndOut BarrierType = "DOWN_AND_OUT"
)

// Valid reports whether t is a declared barrier type.
func (t BarrierType) Valid() bool {
	switch t {
	case BarrierUpAndIn, BarrierUpAndOut, BarrierDownAndIn, BarrierDownAndOut:
		return true
	}
	return false
}

// ParseBarrierType normalizes a barrier-type string — canonical §5.4
// values plus the UP_IN/UP_OUT/DOWN_IN/DOWN_OUT plan shorthand.
func ParseBarrierType(s string) (BarrierType, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "UP_AND_IN", "UP_IN":
		return BarrierUpAndIn, nil
	case "UP_AND_OUT", "UP_OUT":
		return BarrierUpAndOut, nil
	case "DOWN_AND_IN", "DOWN_IN":
		return BarrierDownAndIn, nil
	case "DOWN_AND_OUT", "DOWN_OUT":
		return BarrierDownAndOut, nil
	}
	return "", invalidInput("ParseBarrierType",
		fmt.Sprintf("unknown barrier type %q — want UP_AND_IN|UP_AND_OUT|DOWN_AND_IN|DOWN_AND_OUT", s))
}

// IsUp reports whether the barrier sits above the reference price.
func (t BarrierType) IsUp() bool { return t == BarrierUpAndIn || t == BarrierUpAndOut }

// IsKnockIn reports whether touching the barrier activates (vs kills)
// the option.
func (t BarrierType) IsKnockIn() bool { return t == BarrierUpAndIn || t == BarrierDownAndIn }

// KnockEventName is the option_barrier_events.event vocabulary written
// when the barrier touches (migration 252).
func (t BarrierType) KnockEventName() string {
	if t.IsKnockIn() {
		return "KNOCK_IN"
	}
	return "KNOCK_OUT"
}

// BinaryType discriminates the two fixed-payout digitals of Task 22.3.6.
type BinaryType string

const (
	// BinaryCashOrNothing pays a fixed Payout when the strike condition
	// holds at expiry (spec §15.1 "fixed payout if barrier hit").
	BinaryCashOrNothing BinaryType = "CASH_OR_NOTHING"
	// BinaryAssetOrNothing pays the underlying's terminal value when the
	// strike condition holds at expiry.
	BinaryAssetOrNothing BinaryType = "ASSET_OR_NOTHING"
)

// Valid reports whether t is a declared binary type.
func (t BinaryType) Valid() bool {
	return t == BinaryCashOrNothing || t == BinaryAssetOrNothing
}

// ---------------------------------------------------------------------------
// Market — the pricing environment
// ---------------------------------------------------------------------------

// Market carries the Garman-Kohlhagen inputs for one option on one FX
// pair. Quote conventions follow oracle/rates (Task 19.5.3.5):
//   - Spot: units of quote currency per unit of base currency.
//   - DFd:  domestic (quote-currency) discount factor to expiry.
//   - DFf:  foreign (base-currency) discount factor to expiry.
//   - Vol:  annualized lognormal volatility, decimal (0.10 = 10%).
//   - T:    time to expiry in years (ACT/365 option-tenor convention;
//     the per-currency ACT/360-vs-ACT/365 accrual split of spec §15.3
//     is already embodied in the discount factors).
//
// Discount factors, not rates, are the input — negative-rate regimes
// (DF > 1) price correctly without special casing.
type Market struct {
	Spot float64
	DFd  float64
	DFf  float64
	Vol  float64
	T    float64
}

// Forward returns the outright forward rate F = S·DFf/DFd — the covered
// interest-parity forward of spec §15.3 expressed in discount factors.
func (m Market) Forward() float64 { return m.Spot * m.DFf / m.DFd }

// Rates recovers the continuously-compounded domestic/foreign rates
// implied by the discount factors: r = −ln(DF)/T. Callers needing
// simple-rate accrual should keep using rates.Curve directly. Requires
// T > 0 — callers validate first (validate() does).
func (m Market) Rates() (rd, rf float64) {
	return -math.Log(m.DFd) / m.T, -math.Log(m.DFf) / m.T
}

// validate enforces the fail-closed input contract for every pricing
// path: strictly positive finite spot/vol, positive finite discount
// factors, non-negative finite tenor. Vol == 0 is rejected rather than
// special-cased — a zero-diffusion option price is a degenerate limit,
// not a tradable quote; §2.7 pessimism refuses it.
func (m Market) validate(op string) error {
	switch {
	case !finite(m.Spot) || m.Spot <= 0:
		return invalidInput(op, fmt.Sprintf("spot must be positive and finite, got %v", m.Spot))
	case !finite(m.DFd) || m.DFd <= 0:
		return invalidInput(op, fmt.Sprintf("domestic discount factor must be positive and finite, got %v", m.DFd))
	case !finite(m.DFf) || m.DFf <= 0:
		return invalidInput(op, fmt.Sprintf("foreign discount factor must be positive and finite, got %v", m.DFf))
	case !finite(m.Vol) || m.Vol <= 0:
		return invalidInput(op, fmt.Sprintf("volatility must be positive and finite, got %v", m.Vol))
	case !finite(m.T) || m.T < 0:
		return invalidInput(op, fmt.Sprintf("time to expiry must be >= 0 and finite, got %v", m.T))
	}
	return nil
}

// checkResult is the single fail-closed gate every pricing result
// passes through — a NaN or ±Inf is reported as ErrNonConvergence, never
// returned as a price.
func checkResult(op string, v float64) (float64, error) {
	if !finite(v) {
		return 0, nonConvergence(op, fmt.Sprintf("non-finite result %v — refusing to emit", v))
	}
	return v, nil
}

// MarketFromCurves builds a Market from the oracle rates.Curve pillars
// (Task 19.5.3.5): base is the base-currency curve (foreign rate), quote
// the quote-currency curve (domestic rate). T uses the ACT/365
// option-tenor convention; the curves' per-currency day-count basis
// (ACT/360 vs ACT/365, spec §15.3) is applied inside DiscountFactor.
//
// Fail closed (spec §15.6.3): incomplete or stale curves return
// ErrCurveUnavailable (→ YIELD_CURVE_UNAVAILABLE, HTTP 503); bad
// day offsets return ErrInvalidInput.
func MarketFromCurves(spot, vol float64, daysToExpiry int, base, quote rates.Curve) (Market, error) {
	const op = "MarketFromCurves"
	if daysToExpiry <= 0 {
		return Market{}, invalidInput(op, fmt.Sprintf("days to expiry must be positive, got %d", daysToExpiry))
	}
	if !base.Complete() || base.Stale {
		return Market{}, curveUnavailable(op,
			fmt.Sprintf("base curve %s incomplete=%v stale=%v", base.Currency, !base.Complete(), base.Stale))
	}
	if !quote.Complete() || quote.Stale {
		return Market{}, curveUnavailable(op,
			fmt.Sprintf("quote curve %s incomplete=%v stale=%v", quote.Currency, !quote.Complete(), quote.Stale))
	}
	m := Market{
		Spot: spot,
		DFd:  quote.DiscountFactor(daysToExpiry),
		DFf:  base.DiscountFactor(daysToExpiry),
		Vol:  vol,
		T:    float64(daysToExpiry) / 365.0,
	}
	if err := m.validate(op); err != nil {
		return Market{}, err
	}
	return m, nil
}

// The shared tick shape MarkTick{Price, At, PostGap, Stale} lives in
// barrier.go — the monitor consumes it there; producers flag staleness
// via MarkStale (determinism.go, Task 22.3.15 sibling file).
