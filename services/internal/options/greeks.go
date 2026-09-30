package options

import "math"

// ---------------------------------------------------------------------------
// Greeks (spec §15.2 — delta, gamma, vega, theta, rho real-time; §24 #62)
// ---------------------------------------------------------------------------

// Greeks are the first/second-order sensitivities of a vanilla option's
// Garman-Kohlhagen value, in quote currency per unit of base-currency
// notional:
//   - Delta: spot delta ∂V/∂S (includes the foreign discount factor —
//     the FX "premium-adjusted-agnostic" spot delta convention).
//   - Gamma: ∂²V/∂S².
//   - Vega:  ∂V/∂σ per unit absolute volatility (÷100 for a vol point).
//   - Theta: −∂V/∂T per year of decay (÷365 for a day).
//   - RhoDomestic: ∂V/∂rd (quote-currency rate) per unit absolute rate.
//   - RhoForeign:  ∂V/∂rf (base-currency rate) per unit absolute rate —
//     negative for calls, positive for puts.
type Greeks struct {
	Delta       float64 `json:"delta"`
	Gamma       float64 `json:"gamma"`
	Vega        float64 `json:"vega"`
	Theta       float64 `json:"theta"`
	RhoDomestic float64 `json:"rho_domestic"`
	RhoForeign  float64 `json:"rho_foreign"`
}

// ---------------------------------------------------------------------------
// Standard normal helpers + shared Garman-Kohlhagen core.
//
// gkNormCDF uses the Abramowitz-Stegun 26.2.17 erf-based evaluation —
// deterministic and accurate to ~1e-15, adequate for §24 #324's
// numerical-failure domain (no iterative refinement needed).
// ---------------------------------------------------------------------------

// gkNormPDF is φ(x), the standard normal density.
func gkNormPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}

// gkNormCDF is N(x), the standard normal CDF.
func gkNormCDF(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

// gkCore is the one-shot evaluation of the shared Garman-Kohlhagen
// intermediate quantities — computed once per (option, market) so Price
// and Greeks never disagree on d1/d2.
type gkCore struct {
	f    float64 // forward outright = S·DFf/DFd
	d1   float64
	d2   float64
	nd1  float64 // N(d1)
	nd2  float64 // N(d2)
	nnd1 float64 // N(−d1)
	nnd2 float64 // N(−d2)
	pd1  float64 // φ(d1)
	rd   float64 // implied continuous domestic rate
	rf   float64 // implied continuous foreign rate
	sqrt float64 // σ√T
}

// eval builds the gkCore for a validated market and strike. Requires
// m.T > 0 — the T == 0 intrinsic path is handled by callers before this.
func evalGK(m Market, strike float64) gkCore {
	f := m.Forward()
	sqrt := m.Vol * math.Sqrt(m.T)
	d1 := (math.Log(f/strike) + 0.5*m.Vol*m.Vol*m.T) / sqrt
	d2 := d1 - sqrt
	rd, rf := m.Rates()
	return gkCore{
		f:    f,
		d1:   d1,
		d2:   d2,
		nd1:  gkNormCDF(d1),
		nd2:  gkNormCDF(d2),
		nnd1: gkNormCDF(-d1),
		nnd2: gkNormCDF(-d2),
		pd1:  gkNormPDF(d1),
		rd:   rd,
		rf:   rf,
		sqrt: sqrt,
	}
}

// price evaluates the Garman-Kohlhagen premium for the given right:
//
//	Call = DFd·(F·N(d1) − K·N(d2))
//	Put  = DFd·(K·N(−d2) − F·N(−d1))
func (c gkCore) price(m Market, right OptionRight, strike float64) float64 {
	if right == OptionCall {
		return m.DFd * (c.f*c.nd1 - strike*c.nd2)
	}
	return m.DFd * (strike*c.nnd2 - c.f*c.nnd1)
}

// greeks evaluates the analytic Greeks bundle for the given right. The
// formulas are the DF-form Garman-Kohlhagen sensitivities:
//
//	Δ_call = DFf·N(d1)          Δ_put = −DFf·N(−d1)
//	Γ      = DFf·φ(d1)/(S·σ√T)
//	ν      = S·DFf·φ(d1)·√T
//	θ_call = −S·DFf·φ(d1)·σ/(2√T) + rf·S·DFf·N(d1) − rd·K·DFd·N(d2)
//	θ_put  = −S·DFf·φ(d1)·σ/(2√T) − rf·S·DFf·N(−d1) + rd·K·DFd·N(−d2)
//	ρd_call = K·T·DFd·N(d2)     ρd_put = −K·T·DFd·N(−d2)
//	ρf_call = −S·T·DFf·N(d1)    ρf_put = S·T·DFf·N(−d1)
func (c gkCore) greeks(m Market, right OptionRight, strike float64) Greeks {
	gamma := m.DFf * c.pd1 / (m.Spot * c.sqrt)
	vega := m.Spot * m.DFf * c.pd1 * math.Sqrt(m.T)
	decay := -m.Spot * m.DFf * c.pd1 * m.Vol / (2 * math.Sqrt(m.T))
	var g Greeks
	g.Gamma = gamma
	g.Vega = vega
	if right == OptionCall {
		g.Delta = m.DFf * c.nd1
		g.Theta = decay + c.rf*m.Spot*m.DFf*c.nd1 - c.rd*strike*m.DFd*c.nd2
		g.RhoDomestic = strike * m.T * m.DFd * c.nd2
		g.RhoForeign = -m.Spot * m.T * m.DFf * c.nd1
	} else {
		g.Delta = -m.DFf * c.nnd1
		g.Theta = decay - c.rf*m.Spot*m.DFf*c.nnd1 + c.rd*strike*m.DFd*c.nnd2
		g.RhoDomestic = -strike * m.T * m.DFd * c.nnd2
		g.RhoForeign = m.Spot * m.T * m.DFf * c.nnd1
	}
	return g
}
