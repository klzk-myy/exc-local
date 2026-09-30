// option_margin.go — Phase-19 Task 19.3.25: Option-Delta Margin Linkage
// (spec §13.12/§15.7, §24 #398; remediation #35).
//
// PHASE-22 STUB — linkage seam, not option mechanics.
//
// Phase-22 (Tasks 22.3.10/22.3.13/22.3.15) owns the option positions,
// writer-delta computation, and the intra-day assignment pipeline. This
// file owns the margin-engine contract Phase-22 plugs into:
//
//	option positions → writer delta per position → delta-adjusted
//	equity add-on → consumed by MarginService evaluation (Tasks
//	19.3.3/19.3.16) without formula changes.
//
// Until Phase-22 lands a real OptionDeltaSource, the Null binding
// contributes zero and the margin path is byte-for-byte identical to
// the pre-linkage behavior — a typed-nil never panics.
package risk

import (
	"context"
	"fmt"

	"exchange/pkg/decimal"
)

// OptionPosition is the Phase-22-facing margin input: one open option
// contract (vanilla or barrier) with the writer's signed delta exposure
// already computed by the derivatives engine (Task 22.3.15). Sign
// convention: writer short the option carries negative delta exposure.
type OptionPosition struct {
	AccountID    int64
	InstrumentID int64
	UnderlyingID int64           // the spot FX pair the option writes on
	Side         string          // LONG|SHORT — holder side
	Quantity     decimal.Decimal // contracts
	Delta        decimal.Decimal // per-contract delta (Task 22.3.15)
	MarkPrice    decimal.Decimal // option mark in settlement currency
	NotionalUSD  decimal.Decimal // contract notional in USD numeraire
}

// OptionDeltaSource supplies open option positions for one account.
// Phase-22 implements this over the derivatives position book; until
// then NullOptionDeltaSource is the only binding.
type OptionDeltaSource interface {
	OptionPositions(ctx context.Context, accountID int64) ([]OptionPosition, error)
}

// NullOptionDeltaSource is the Phase-19 dev/null binding: zero option
// positions, zero adjustment. It is not a fail-closed stall because the
// derivatives stack does not exist yet — there is nothing to withhold.
// Once Phase-22 deploys, replacing this binding is mandatory before
// option books can open (enforced by the instrument-kind gate: OPTION
// instruments are untradable until then).
type NullOptionDeltaSource struct{}

// OptionPositions always reports an empty book.
func (NullOptionDeltaSource) OptionPositions(context.Context, int64) ([]OptionPosition, error) {
	return nil, nil
}

// OptionDeltaAdjustment computes the §15.7 delta-adjusted equity add-on
// for one account's option book: Σ |qty × delta × underlying notional|
// valued at the option mark. The result is signed — writer exposure
// reduces effective equity; holder premium value raises it.
//
// The formula is deliberately narrow: spot-leg margin, spread offsets
// (spread_offsets.go), and SIMM aggregation (Phase-22) compose on top.
func OptionDeltaAdjustment(positions []OptionPosition) (decimal.Decimal, error) {
	total := decimal.Zero
	for _, p := range positions {
		if p.Delta.IsZero() || p.Quantity.IsZero() {
			continue
		}
		if !p.NotionalUSD.IsPositive() {
			return decimal.Zero, fmt.Errorf(
				"option margin: position %d instrument %d missing notional",
				p.AccountID, p.InstrumentID)
		}
		// Writer delta exposure in USD: qty × delta × notional.
		// Sign of Delta carries direction; holders add, writers subtract.
		leg := p.Quantity.Mul(p.Delta).Mul(p.NotionalUSD)
		total = total.Add(leg)
	}
	return total, nil
}

// OptionMarginEvaluator is the seam MarginService consults (via
// MarginOptions) once Phase-22 is live. Phase-19 ships the binding
// contract + the null implementation.
type OptionMarginEvaluator interface {
	// DeltaEquityAdj returns the delta-adjusted equity add-on in the
	// account's USD numeraire. A nil source yields zero.
	DeltaEquityAdj(ctx context.Context, accountID int64) (decimal.Decimal, error)
}

// NullOptionMarginEvaluator contributes zero — the pre-Phase-22
// binding. It lets MarginService hold a non-nil evaluator today without
// behavior change.
type NullOptionMarginEvaluator struct{}

// DeltaEquityAdj always returns zero.
func (NullOptionMarginEvaluator) DeltaEquityAdj(context.Context, int64) (decimal.Decimal, error) {
	return decimal.Zero, nil
}

// DeltaOptionMarginEvaluator is the production seam Phase-22 wires:
// it resolves the account's option book and applies OptionDeltaAdjustment.
type DeltaOptionMarginEvaluator struct {
	Source OptionDeltaSource
}

// DeltaEquityAdj resolves the book then computes the add-on; source
// errors propagate (fail closed — an unreadable option book must not
// evaluate margin without its delta leg).
func (e DeltaOptionMarginEvaluator) DeltaEquityAdj(ctx context.Context, accountID int64) (decimal.Decimal, error) {
	if e.Source == nil {
		return decimal.Zero, nil
	}
	pos, err := e.Source.OptionPositions(ctx, accountID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("option margin: book acct %d: %w", accountID, err)
	}
	return OptionDeltaAdjustment(pos)
}
