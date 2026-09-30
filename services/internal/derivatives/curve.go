// curve.go — Phase-22 Tasks 22.3.1/22.3.2 pricing: covered-interest-
// parity forwards and FX-swap points over the Phase-19.5 yield curves
// (services/internal/oracle/rates — REUSE, never re-publish).
//
// Canonical formula (spec §15.3, §24 #57):
//
//	forward_rate = spot × (1 + r_quote·d/DCC_quote)
//	                      / (1 + r_base·d/DCC_base)
//
// where d is the calendar-day span spot_value_date → value_date and
// DCC_ccy is the per-currency money-market basis (ACT/360: USD EUR CHF
// JPY; ACT/365: GBP AUD NZD CAD SGD HKD — rates.DayCount).
//
// Fail-closed (spec §2.7, §15.6): a missing, stale or incomplete curve —
// or a non-positive interpolated rate — rejects with
// YIELD_CURVE_UNAVAILABLE; a missing/stale spot reference rejects with
// PRICE_ORACLE_UNAVAILABLE. No default-zero rate ever prices.
package derivatives

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/oracle"
	"exchange/internal/oracle/rates"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Feed seams — satisfied by *rates.Store and *oracle.Provider.
// ---------------------------------------------------------------------------

// CurveSource reads the per-currency yield curves published by the
// PriceOracle (curve:{ccy}); *rates.Store implements it with the 5s
// staleness gate built in (ErrCurveUnavailable).
type CurveSource interface {
	GetCurve(ctx context.Context, ccy string) (rates.Curve, error)
}

// SpotSource reads the oracle mark (spot reference) for a pair;
// *oracle.Provider implements it. A stale or absent mark fails closed.
type SpotSource interface {
	Mark(ctx context.Context, symbol string) (oracle.MarkView, error)
}

// ForwardCurveTenors is the Task 22.3.1 pillar set (1W/1M/3M/6M/1Y).
var ForwardCurveTenors = []rates.Tenor{
	rates.Tenor1W, rates.Tenor1M, rates.Tenor3M, rates.Tenor6M, rates.Tenor12M,
}

// ---------------------------------------------------------------------------
// Pricing
// ---------------------------------------------------------------------------

// Pricer prices forwards and swap points from the published curves.
// Cal carries the holiday engine for spot/tenor date math (see
// settlement_dates.go); Curves and Spot are the oracle feed seams;
// either may be nil only on code paths that take explicit inputs.
type Pricer struct {
	Curves CurveSource
	Spot   SpotSource
	now    func() time.Time
}

// NewPricer wires the pricing engine. now is injectable for tests.
func NewPricer(curves CurveSource, spot SpotSource) *Pricer {
	return &Pricer{Curves: curves, Spot: spot, now: time.Now}
}

// ForwardQuote is one priced outright forward.
type ForwardQuote struct {
	Pair          Pair
	SpotRate      decimal.Decimal
	ForwardRate   decimal.Decimal
	SwapPoints    decimal.Decimal // ForwardRate − SpotRate
	Days          int             // calendar days spot value date → value date
	SpotValueDate time.Time
	ValueDate     time.Time
	BaseRate      decimal.Decimal // annualized decimal fraction
	QuoteRate     decimal.Decimal
	DayCountBase  rates.DayBasis // 360 or 365
	DayCountQuote rates.DayBasis
	PricedAt      time.Time
}

// ForwardRate is the pure CIP function (spec §15.3). Rates are
// annualized decimal fractions (0.0525 = 5.25%); days is the accrual-day
// count between the spot and forward value dates. All inputs validated —
// a non-positive spot or rate is a coded rejection, never zero-priced.
func ForwardRate(pair Pair, spot, baseRate, quoteRate decimal.Decimal, days int) (decimal.Decimal, error) {
	if !spot.IsPositive() {
		return decimal.Zero, excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("forward pricing %s: spot must be > 0", pair.Symbol()))
	}
	if days <= 0 {
		return decimal.Zero, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"forward pricing %s: accrual days %d must be > 0 (value date after spot date)",
			pair.Symbol(), days))
	}
	if !baseRate.IsPositive() || !quoteRate.IsPositive() {
		return decimal.Zero, excerrors.New(CodeYieldCurveUnavailable, fmt.Sprintf(
			"forward pricing %s: non-positive curve rate (base %s, quote %s)",
			pair.Symbol(), baseRate, quoteRate))
	}
	yfQ := decimal.NewFromInt(int64(days)).Div(
		decimal.NewFromInt(int64(rates.DayCount(pair.Quote))))
	yfB := decimal.NewFromInt(int64(days)).Div(
		decimal.NewFromInt(int64(rates.DayCount(pair.Base))))
	num := decimal.One.Add(quoteRate.Mul(yfQ))
	den := decimal.One.Add(baseRate.Mul(yfB))
	if !den.IsPositive() {
		return decimal.Zero, excerrors.New(CodeYieldCurveUnavailable, fmt.Sprintf(
			"forward pricing %s: base discount factor non-positive (rate %s, days %d)",
			pair.Symbol(), baseRate, days))
	}
	return spot.Mul(num).Div(den), nil
}

// curveRate resolves one currency's interpolated rate at the accrual-day
// offset, failing closed on missing/stale/incomplete curves and
// non-positive rates (no default-zero, spec §2.7/§15.6).
func (p *Pricer) curveRate(ctx context.Context, ccy string, days int) (decimal.Decimal, error) {
	if p.Curves == nil {
		return decimal.Zero, excerrors.New(CodeYieldCurveUnavailable,
			"forward pricing: yield-curve source unwired")
	}
	c, err := p.Curves.GetCurve(ctx, ccy)
	if err != nil {
		return decimal.Zero, excerrors.Wrap(CodeYieldCurveUnavailable,
			fmt.Sprintf("forward pricing: %s curve", ccy), err)
	}
	if !c.Complete() {
		return decimal.Zero, excerrors.New(CodeYieldCurveUnavailable, fmt.Sprintf(
			"forward pricing: %s curve incomplete", ccy))
	}
	if c.Stale {
		return decimal.Zero, excerrors.New(CodeYieldCurveUnavailable, fmt.Sprintf(
			"forward pricing: %s curve stale (as of %s)", ccy, c.AsOf.UTC().Format(time.RFC3339)))
	}
	r := c.Rate(days)
	if !r.IsPositive() {
		return decimal.Zero, excerrors.New(CodeYieldCurveUnavailable, fmt.Sprintf(
			"forward pricing: %s interpolated rate at %dd non-positive", ccy, days))
	}
	return r, nil
}

// PriceForward computes the outright forward for pair at valueDate.
// Callers supply the resolved spot rate and the holiday-adjusted spot /
// forward value dates (see Dates); this function owns only the rate math
// and the curve fetch. The day axis is the actual calendar-day span —
// holiday-adjusted dates therefore shift pricing, not just settlement.
func (p *Pricer) PriceForward(ctx context.Context, pair Pair, spot decimal.Decimal,
	spotValueDate, valueDate time.Time) (*ForwardQuote, error) {
	days := int(normDay(valueDate).Sub(normDay(spotValueDate)).Hours() / 24)
	if days <= 0 {
		return nil, excerrors.New(CodeInvalidRequest, fmt.Sprintf(
			"forward %s: value date %s must be after spot date %s",
			pair.Symbol(), valueDate.Format("2006-01-02"), spotValueDate.Format("2006-01-02")))
	}
	baseRate, err := p.curveRate(ctx, pair.Base, days)
	if err != nil {
		return nil, err
	}
	quoteRate, err := p.curveRate(ctx, pair.Quote, days)
	if err != nil {
		return nil, err
	}
	fwd, err := ForwardRate(pair, spot, baseRate, quoteRate, days)
	if err != nil {
		return nil, err
	}
	return &ForwardQuote{
		Pair:          pair,
		SpotRate:      spot,
		ForwardRate:   fwd,
		SwapPoints:    fwd.Sub(spot),
		Days:          days,
		SpotValueDate: normDay(spotValueDate),
		ValueDate:     normDay(valueDate),
		BaseRate:      baseRate,
		QuoteRate:     quoteRate,
		DayCountBase:  rates.DayCount(pair.Base),
		DayCountQuote: rates.DayCount(pair.Quote),
		PricedAt:      p.now().UTC(),
	}, nil
}

// SpotRate resolves the pair's spot reference through the oracle mark —
// fail-closed on absent or stale marks (PRICE_ORACLE_UNAVAILABLE). Callers
// with an execution price (a matched fill) pass it explicitly instead.
func (p *Pricer) SpotRate(ctx context.Context, pair Pair) (decimal.Decimal, error) {
	if p.Spot == nil {
		return decimal.Zero, excerrors.New(CodePriceOracleUnavailable,
			"forward pricing: spot source unwired")
	}
	mv, err := p.Spot.Mark(ctx, pair.Symbol())
	if err != nil {
		return decimal.Zero, excerrors.Wrap(CodePriceOracleUnavailable,
			fmt.Sprintf("spot mark %s", pair.Symbol()), err)
	}
	if !mv.Found || mv.Stale {
		return decimal.Zero, excerrors.New(CodePriceOracleUnavailable, fmt.Sprintf(
			"spot mark %s absent or stale", pair.Symbol()))
	}
	if !mv.Price.IsPositive() {
		return decimal.Zero, excerrors.New(CodePriceOracleUnavailable, fmt.Sprintf(
			"spot mark %s non-positive", pair.Symbol()))
	}
	return mv.Price, nil
}

// ForwardCurve builds the Task 22.3.1 forward curve: one priced outright
// per pillar tenor (1W/1M/3M/6M/1Y) at holiday-adjusted value dates.
func (p *Pricer) ForwardCurve(ctx context.Context, d *Dates, pair Pair,
	spot decimal.Decimal, tradeDay time.Time, cycleDays int) ([]ForwardQuote, error) {
	spotDate, err := d.SpotDate(pair, tradeDay, cycleDays)
	if err != nil {
		return nil, err
	}
	out := make([]ForwardQuote, 0, len(ForwardCurveTenors))
	for _, t := range ForwardCurveTenors {
		vd, err := d.TenorDate(pair, tradeDay, cycleDays, string(t))
		if err != nil {
			return nil, err
		}
		q, err := p.PriceForward(ctx, pair, spot, spotDate, vd)
		if err != nil {
			return nil, err
		}
		out = append(out, *q)
	}
	return out, nil
}
