package options

import "fmt"

// ---------------------------------------------------------------------------
// VanillaOption — European FX calls/puts, Garman-Kohlhagen closed form
// (Phase-22 Task 22.3.4; spec §15.1/§15.2; §24 #60, #62)
// ---------------------------------------------------------------------------

// VanillaOption is a European FX option contract: the right to exchange
// Strike units of quote currency per unit of base currency at expiry.
// Quantities are per unit of base-currency notional — callers scale by
// notional for order-level values.
type VanillaOption struct {
	Right  OptionRight   // CALL (buy base / sell quote) or PUT
	Strike float64       // strike rate, quote per base
	Style  ExerciseStyle // only ExerciseEuropean is priced here
}

// validate enforces the fail-closed contract. American style is rejected
// explicitly — spec §15.2 prices it on a lattice with an early-exercise
// boundary (Task 22.3.15, sibling-owned american.go); silently applying
// BSGK to an American option understates value, which §2.7 pessimism
// forbids.
func (o VanillaOption) validate(op string) error {
	switch {
	case !o.Right.Valid():
		return invalidInput(op, fmt.Sprintf("option right must be CALL|PUT, got %q", o.Right))
	case !finite(o.Strike) || o.Strike <= 0:
		return invalidInput(op, fmt.Sprintf("strike must be positive and finite, got %v", o.Strike))
	case o.Style != ExerciseEuropean:
		return invalidInput(op,
			fmt.Sprintf("exercise style %q not priced by Garman-Kohlhagen — American options require the lattice pricer (spec §15.2)", o.Style))
	}
	return nil
}

// Price returns the Garman-Kohlhagen premium in quote currency per unit
// of base notional. At T == 0 the value degenerates to the intrinsic
// payoff at spot (expired-option path).
func (o VanillaOption) Price(m Market) (float64, error) {
	const op = "VanillaOption.Price"
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
	return checkResult(op, c.price(m, o.Right, o.Strike))
}

// Greeks returns the analytic sensitivity bundle (spec §24 #62). At
// T == 0 sensitivities collapse: delta is DFf/−DFf in-the-money and 0
// otherwise; gamma/vega/theta/rho are 0.
func (o VanillaOption) Greeks(m Market) (Greeks, error) {
	const op = "VanillaOption.Greeks"
	if err := o.validate(op); err != nil {
		return Greeks{}, err
	}
	if err := m.validate(op); err != nil {
		return Greeks{}, err
	}
	if m.T == 0 {
		return o.expiryGreeks(m), nil
	}
	c := evalGK(m, o.Strike)
	g := c.greeks(m, o.Right, o.Strike)
	for _, v := range []float64{g.Delta, g.Gamma, g.Vega, g.Theta, g.RhoDomestic, g.RhoForeign} {
		if !finite(v) {
			return Greeks{}, nonConvergence(op, "non-finite greek — refusing to emit")
		}
	}
	return g, nil
}

// PriceAndGreeks evaluates premium and sensitivities in one pass — the
// production path, so a quote and its Greeks never disagree on d1/d2.
func (o VanillaOption) PriceAndGreeks(m Market) (float64, Greeks, error) {
	const op = "VanillaOption.PriceAndGreeks"
	if err := o.validate(op); err != nil {
		return 0, Greeks{}, err
	}
	if err := m.validate(op); err != nil {
		return 0, Greeks{}, err
	}
	if m.T == 0 {
		p, err := checkResult(op, o.ExpiryPayoff(m.Spot))
		if err != nil {
			return 0, Greeks{}, err
		}
		return p, o.expiryGreeks(m), nil
	}
	c := evalGK(m, o.Strike)
	p, err := checkResult(op, c.price(m, o.Right, o.Strike))
	if err != nil {
		return 0, Greeks{}, err
	}
	g := c.greeks(m, o.Right, o.Strike)
	for _, v := range []float64{g.Delta, g.Gamma, g.Vega, g.Theta, g.RhoDomestic, g.RhoForeign} {
		if !finite(v) {
			return 0, Greeks{}, nonConvergence(op, "non-finite greek — refusing to emit")
		}
	}
	return p, g, nil
}

// ExpiryPayoff is the cash-settlement amount per unit of base notional
// when the spot rate fixes at sT — the Task 22.3.10 settlement input
// (physical delivery converts this into the underlying spot trade at
// Strike instead).
func (o VanillaOption) ExpiryPayoff(sT float64) float64 {
	if o.Right == OptionCall {
		return max(sT-o.Strike, 0)
	}
	return max(o.Strike-sT, 0)
}

// expiryGreeks is the degenerate T == 0 sensitivity set.
func (o VanillaOption) expiryGreeks(m Market) Greeks {
	var g Greeks
	if o.Right == OptionCall && m.Spot > o.Strike {
		g.Delta = m.DFf
	}
	if o.Right == OptionPut && m.Spot < o.Strike {
		g.Delta = -m.DFf
	}
	return g
}

// PutCallParityForward returns the parity residual C − P =
// DFd·(F − K) for the market and strike — the no-arbitrage identity the
// pricer must satisfy exactly (both legs share evalGK). Exposed for
// arbitrage checks and tests.
func PutCallParityForward(m Market, strike float64) (float64, error) {
	const op = "PutCallParityForward"
	if err := m.validate(op); err != nil {
		return 0, err
	}
	if !finite(strike) || strike <= 0 {
		return 0, invalidInput(op, fmt.Sprintf("strike must be positive and finite, got %v", strike))
	}
	if m.T == 0 {
		return m.Spot - strike, nil
	}
	return m.DFd * (m.Forward() - strike), nil
}
