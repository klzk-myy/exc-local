package options

import "fmt"

// ---------------------------------------------------------------------------
// BinaryOption — cash-or-nothing / asset-or-nothing European digitals
// (Phase-22 Task 22.3.6; spec §15.1–§15.2). Cash settlement at expiry;
// professional/ECP only at order admission (spec §5.4 note — enforced at
// the gateway, not here). Monte Carlo price is the production path
// (MCPricer.PriceBinary, spec §15.2); PriceClosedForm is the Garman-
// Kohlhagen digital reference used for convergence checks and as the
// deterministic settlement-evaluation fallback.
// ---------------------------------------------------------------------------

// BinaryOption is a fixed-payout digital on an FX pair. The condition is
// European: it evaluates once, at expiry, on the fixing spot.
type BinaryOption struct {
	Kind   BinaryType  // CASH_OR_NOTHING | ASSET_OR_NOTHING
	Right  OptionRight // CALL pays when sT > Strike; PUT when sT < Strike
	Strike float64     // strike rate, quote per base
	// Payout is the fixed quote-currency amount for CASH_OR_NOTHING
	// (per unit of base notional the contract is written on). Ignored —
	// and allowed to be 0 — for ASSET_OR_NOTHING, which pays sT.
	Payout float64
}

// validate enforces the fail-closed contract.
func (o BinaryOption) validate(op string) error {
	switch {
	case !o.Kind.Valid():
		return invalidInput(op, fmt.Sprintf("binary kind must be CASH_OR_NOTHING|ASSET_OR_NOTHING, got %q", o.Kind))
	case !o.Right.Valid():
		return invalidInput(op, fmt.Sprintf("option right must be CALL|PUT, got %q", o.Right))
	case !finite(o.Strike) || o.Strike <= 0:
		return invalidInput(op, fmt.Sprintf("strike must be positive and finite, got %v", o.Strike))
	case o.Kind == BinaryCashOrNothing && (!finite(o.Payout) || o.Payout <= 0):
		return invalidInput(op, fmt.Sprintf("cash-or-nothing payout must be positive and finite, got %v", o.Payout))
	}
	return nil
}

// ExpiryPayoff is the cash-settlement amount per unit of base notional
// at fixing spot sT. The strike is strict (call pays iff sT > K) — the
// standard digital convention; an exact fix on the strike pays nothing.
func (o BinaryOption) ExpiryPayoff(sT float64) float64 {
	inTheMoney := (o.Right == OptionCall && sT > o.Strike) ||
		(o.Right == OptionPut && sT < o.Strike)
	if !inTheMoney {
		return 0
	}
	if o.Kind == BinaryAssetOrNothing {
		return sT
	}
	return o.Payout
}

// PriceClosedForm returns the Garman-Kohlhagen digital premium:
//
//	cash-or-nothing:  Call = Payout·DFd·N(d2)   Put = Payout·DFd·N(−d2)
//	asset-or-nothing: Call = DFd·F·N(d1)         Put = DFd·F·N(−d1)
//
// The MC engine (MCPricer.PriceBinary) converges to this value — the
// two implementations are each other's convergence control.
func (o BinaryOption) PriceClosedForm(m Market) (float64, error) {
	const op = "BinaryOption.PriceClosedForm"
	if err := o.validate(op); err != nil {
		return 0, err
	}
	if err := m.validate(op); err != nil {
		return 0, err
	}
	if m.T == 0 {
		return checkResult(op, o.ExpiryPayoff(m.Spot))
	}
	c := evalGK(m, o.Strike)
	var v float64
	switch {
	case o.Kind == BinaryCashOrNothing && o.Right == OptionCall:
		v = o.Payout * m.DFd * c.nd2
	case o.Kind == BinaryCashOrNothing:
		v = o.Payout * m.DFd * c.nnd2
	case o.Right == OptionCall:
		v = m.DFd * c.f * c.nd1
	default:
		v = m.DFd * c.f * c.nnd1
	}
	return checkResult(op, v)
}
