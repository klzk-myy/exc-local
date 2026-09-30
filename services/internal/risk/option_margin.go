// option_margin.go — Phase-19 Task 19.3.25: Option-Delta Margin Linkage
// (spec §13.12/§15.7, §24 #398; remediation #35).
//
// BACK-FIT NOTE (post-Phase-22): Phase-22 has landed — the option book,
// writer-delta mechanics, and the assignment pipeline are live. This
// file still owns the margin-engine contract Phase-22 plugs into:
//
//	option positions → writer delta per position → delta-adjusted
//	equity add-on → consumed by MarginService evaluation (Tasks
//	19.3.3/19.3.16) via MarginOptions.OptionMargin without formula
//	changes.
//
// The production OptionDeltaSource is PgOptionDeltaSource
// (option_delta_source.go) — it prices each open leg's delta at the
// current market and is wired in cmd/gateway. NullOptionDeltaSource /
// NullOptionMarginEvaluator remain the dev bindings: a nil
// MarginOptions.OptionMargin contributes zero and the margin path is
// byte-for-byte identical to the pre-linkage behavior.
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
// PgOptionDeltaSource (option_delta_source.go) implements it over the
// Phase-22 derivatives position book; NullOptionDeltaSource remains
// the dev/null binding.
type OptionDeltaSource interface {
	OptionPositions(ctx context.Context, accountID int64) ([]OptionPosition, error)
}

// NullOptionDeltaSource is the dev/null binding: zero option
// positions, zero adjustment. It is NOT the production binding —
// cmd/gateway wires PgOptionDeltaSource; the null stays for tests and
// option-disabled deployments.
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

// OptionMarginEvaluator is the seam MarginService consults via
// MarginOptions.OptionMargin (back-fitted post-Phase-22 — gateway
// binds DeltaOptionMarginEvaluator over PgOptionDeltaSource).
type OptionMarginEvaluator interface {
	// DeltaEquityAdj returns the delta-adjusted equity add-on in the
	// account's USD numeraire. A nil source yields zero.
	DeltaEquityAdj(ctx context.Context, accountID int64) (decimal.Decimal, error)
}

// NullOptionMarginEvaluator contributes zero — the dev/test binding.
// It lets MarginService hold a non-nil evaluator without behavior
// change; production binds DeltaOptionMarginEvaluator.
type NullOptionMarginEvaluator struct{}

// DeltaEquityAdj always returns zero.
func (NullOptionMarginEvaluator) DeltaEquityAdj(context.Context, int64) (decimal.Decimal, error) {
	return decimal.Zero, nil
}

// DeltaOptionMarginEvaluator is the production seam (wired in
// cmd/gateway over PgOptionDeltaSource): it resolves the account's
// option book and applies OptionDeltaAdjustment.
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
