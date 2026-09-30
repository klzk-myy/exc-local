// option_delta_source_test.go — Phase-19 Task 19.3.25 back-fit unit
// coverage: the production OptionDeltaSource's row→OptionPosition
// mapping (holder-positive / writer-negative sign convention), the
// market seam's fail-closed posture (missing/stale mark, unwired vol),
// the USD numeraire source, and the MarginService composition points
// (equity leg + PORTFOLIO spread relief).
package risk

import (
	"context"
	stderrors "errors"
	"fmt"
	"math"
	"testing"
	"time"

	"exchange/internal/options"
	"exchange/internal/oracle/rates"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeOptPricer struct {
	res OptionPricerResult
	err error
}

func (f fakeOptPricer) Price(options.OptionRight, options.ExerciseStyle,
	float64, options.Market) (OptionPricerResult, error) {
	return f.res, f.err
}

type fakeOptMarkets struct {
	m   options.Market
	err error
	req []OptionMarketRequest
}

func (f *fakeOptMarkets) OptionMarket(_ context.Context,
	r OptionMarketRequest) (options.Market, error) {
	f.req = append(f.req, r)
	return f.m, f.err
}

type fakeUSDRates struct {
	m   map[string]decimal.Decimal
	err error
}

func (f fakeUSDRates) RatesToUSD(context.Context,
	[]string) (map[string]decimal.Decimal, error) {
	return f.m, f.err
}

type fakeMarkProv struct {
	mp  MarkPrice
	err error
}

func (f fakeMarkProv) GetMarkPrice(string) (decimal.Decimal, error) {
	if f.err != nil {
		return decimal.Zero, f.err
	}
	return f.mp.Price, nil
}

func (f fakeMarkProv) GetMarkPriceWithProvenance(string) (MarkPrice, error) {
	return f.mp, f.err
}

type fakeCurveSource struct {
	m   map[string]rates.Curve
	err error
}

func (f fakeCurveSource) GetCurve(_ context.Context, ccy string) (rates.Curve, error) {
	if f.err != nil {
		return rates.Curve{}, f.err
	}
	c, ok := f.m[ccy]
	if !ok {
		return rates.Curve{}, fmt.Errorf("no curve for %s", ccy)
	}
	return c, nil
}

// flatCurve is a complete CanonicalTenors curve at a flat rate.
func flatCurve(ccy, rate string) rates.Curve {
	r := map[rates.Tenor]decimal.Decimal{}
	for _, t := range rates.CanonicalTenors {
		r[t] = d(rate)
	}
	return rates.Curve{Currency: ccy, Rates: r, AsOf: time.Now().UTC(),
		SourceFeeds: []string{"A", "B"}}
}

type fakeEval struct {
	adj decimal.Decimal
	err error
}

func (f fakeEval) DeltaEquityAdj(context.Context, int64) (decimal.Decimal, error) {
	return f.adj, f.err
}

type fakeSpreads struct {
	offs  []SpreadOffset
	err   error
	calls int
}

func (f *fakeSpreads) SpreadOffsets(int64) ([]SpreadOffset, error) {
	f.calls++
	return f.offs, f.err
}

var (
	_ OptionDeltaPricer       = fakeOptPricer{}
	_ OptionDeltaMarketSource = (*fakeOptMarkets)(nil)
	_ OptionUSDRateSource     = fakeUSDRates{}
	_ MarkPriceProvider       = fakeMarkProv{}
	_ OptionCurveSource       = fakeCurveSource{}
	_ OptionMarginEvaluator   = fakeEval{}
	_ SpreadOffsetSource      = (*fakeSpreads)(nil)
)

// ---------------------------------------------------------------------------
// mapRow — holder-positive / writer-negative sign convention
// ---------------------------------------------------------------------------

func optRow(side, typ, style string) optionDeltaRow {
	return optionDeltaRow{
		PositionID: 55, AccountID: 7, InstrumentID: 101, UnderlyingID: 1,
		OptionType: typ, Style: style, Side: side,
		Quantity: d("2"), Strike: d("1.10"),
		Expiry:       time.Now().UTC().Add(30 * 24 * time.Hour),
		ContractSize: d("100000"),
		UndSymbol:    "EUR/USD", UndBase: "EUR", UndQuote: "USD",
	}
}

var gkMarket = options.Market{Spot: 1.20, DFd: 0.99, DFf: 0.995, Vol: 0.10, T: 0.25}

func TestOptionDeltaSourceMapRowSignConvention(t *testing.T) {
	src := &PgOptionDeltaSource{}
	pricer := fakeOptPricer{res: OptionPricerResult{Delta: 0.6, Mark: 0.02, Model: "GK"}}
	usd := decimal.NewFromInt(1)

	// Holder (LONG): delta passes through positive.
	pos, err := src.mapRow(optRow("LONG", "CALL", "EUROPEAN"), gkMarket, usd, pricer)
	if err != nil {
		t.Fatalf("holder mapRow: %v", err)
	}
	if !pos.Delta.Equal(d("0.6")) {
		t.Fatalf("holder delta %s, want +0.6", pos.Delta)
	}
	// notional = 100000 contracts units × 1.20 spot × 1 USD/USD.
	if !pos.NotionalUSD.Equal(d("120000")) {
		t.Fatalf("notional %s, want 120000", pos.NotionalUSD)
	}
	if !pos.MarkPrice.Equal(d("0.02")) || pos.Side != "LONG" {
		t.Fatalf("mark/side: %+v", pos)
	}

	// Writer (SHORT): the negation of the holder delta.
	pos, err = src.mapRow(optRow("SHORT", "PUT", "EUROPEAN"), gkMarket, usd, pricer)
	if err != nil {
		t.Fatalf("writer mapRow: %v", err)
	}
	if !pos.Delta.Equal(d("-0.6")) {
		t.Fatalf("writer delta %s, want -0.6", pos.Delta)
	}
}

func TestOptionDeltaSourceMapRowFailClosed(t *testing.T) {
	src := &PgOptionDeltaSource{}
	okPricer := fakeOptPricer{res: OptionPricerResult{Delta: 0.5, Mark: 0.01, Model: "GK"}}
	usd := decimal.NewFromInt(1)

	// Non-positive USD rate → fail closed, never zero notional.
	if _, err := src.mapRow(optRow("LONG", "CALL", "EUROPEAN"),
		gkMarket, decimal.Zero, okPricer); err == nil {
		t.Fatal("missing USD rate must fail closed")
	}
	// Unparseable right (BINARY has no analytic delta on this path).
	if _, err := src.mapRow(optRow("LONG", "BINARY", "EUROPEAN"),
		gkMarket, usd, okPricer); err == nil {
		t.Fatal("BINARY leg must fail closed")
	}
	// Malformed style.
	if _, err := src.mapRow(optRow("LONG", "CALL", "WEEKLY"),
		gkMarket, usd, okPricer); err == nil {
		t.Fatal("unknown exercise style must fail closed")
	}
	// Pricer error propagates.
	if _, err := src.mapRow(optRow("LONG", "CALL", "EUROPEAN"),
		gkMarket, usd, fakeOptPricer{err: stderrors.New("lattice blew up")}); err == nil {
		t.Fatal("pricer error must propagate")
	}
	// Out-of-bounds delta from a defective pricer fails closed.
	if _, err := src.mapRow(optRow("LONG", "CALL", "EUROPEAN"), gkMarket, usd,
		fakeOptPricer{res: OptionPricerResult{Delta: 1.5, Mark: 0.01}}); err == nil {
		t.Fatal("delta outside [-1,1] must fail closed")
	}
	// Non-positive notional fails closed (zero spot edge).
	if _, err := src.mapRow(optRow("LONG", "CALL", "EUROPEAN"),
		options.Market{Spot: 0, T: 0}, usd,
		fakeOptPricer{res: OptionPricerResult{Delta: 0.5, Mark: 0}}); err == nil {
		t.Fatal("non-positive notional must fail closed")
	}
}

// ---------------------------------------------------------------------------
// OptionsDeltaPricer — GK, lattice, intrinsic paths
// ---------------------------------------------------------------------------

func TestOptionsDeltaPricerGK(t *testing.T) {
	res, err := (OptionsDeltaPricer{}).Price(
		options.OptionCall, options.ExerciseEuropean, 1.10, gkMarket)
	if err != nil {
		t.Fatalf("GK price: %v", err)
	}
	if res.Model != "GK" {
		t.Fatalf("model %q, want GK", res.Model)
	}
	if !(res.Delta > 0 && res.Delta < 1) {
		t.Fatalf("call delta %v outside (0,1)", res.Delta)
	}
	// GK call delta at S=1.20, K=1.10, r≈0, σ=0.10, T=0.25 ≈ 0.97.
	if math.Abs(res.Delta-0.97) > 0.05 {
		t.Fatalf("deep-ITM call delta %v, want ≈0.97", res.Delta)
	}
}

func TestOptionsDeltaPricerLatticeAndIntrinsic(t *testing.T) {
	// American — bump-and-reprice over the lattice (small bound set to
	// keep the unit test fast).
	p := OptionsDeltaPricer{Lattice: options.LatticeConfig{
		MinSteps: 50, MaxSteps: 200, Tolerance: 1e-3}}
	res, err := p.Price(options.OptionPut, options.ExerciseAmerican, 1.30, gkMarket)
	if err != nil {
		t.Fatalf("lattice price: %v", err)
	}
	if res.Model != "LATTICE" {
		t.Fatalf("model %q, want LATTICE", res.Model)
	}
	if !(res.Delta < 0 && res.Delta > -1) {
		t.Fatalf("american put delta %v outside (-1,0)", res.Delta)
	}
	// Expired row → intrinsic: ITM put delta −1, mark = strike − spot.
	res, err = p.Price(options.OptionPut, options.ExerciseAmerican, 1.30,
		options.Market{Spot: 1.20, T: 0})
	if err != nil {
		t.Fatalf("intrinsic: %v", err)
	}
	if res.Model != "INTRINSIC" || res.Delta != -1 ||
		math.Abs(res.Mark-0.10) > 1e-9 {
		t.Fatalf("intrinsic %+v, want delta -1 mark ~0.10", res)
	}
	// OTM expired → zero delta/mark.
	res, err = p.Price(options.OptionCall, options.ExerciseEuropean, 1.30,
		options.Market{Spot: 1.20, T: 0})
	if err != nil {
		t.Fatalf("otm intrinsic: %v", err)
	}
	if res.Delta != 0 || res.Mark != 0 {
		t.Fatalf("otm intrinsic %+v, want zeros", res)
	}
}

// ---------------------------------------------------------------------------
// RedisOptionMarketSource — fail-closed market resolution
// ---------------------------------------------------------------------------

func TestRedisOptionMarketSource(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	req := OptionMarketRequest{
		Underlying: "EUR/USD", BaseCCY: "EUR", QuoteCCY: "USD",
		Strike: d("1.10"), Expiry: now.Add(30 * 24 * time.Hour),
	}
	curves := fakeCurveSource{m: map[string]rates.Curve{
		"EUR": flatCurve("EUR", "0.04"), "USD": flatCurve("USD", "0.05"),
	}}

	// Missing mark fails closed with PRICE_ORACLE_UNAVAILABLE.
	_, err := RedisOptionMarketSource{
		Marks:  fakeMarkProv{err: ErrMarkNotFound},
		Curves: curves,
		Now:    func() time.Time { return now },
	}.OptionMarket(context.Background(), req)
	requireCode(t, err, CodeOracleUnavailable)

	// Stale mark fails closed — a stale price is no price.
	_, err = RedisOptionMarketSource{
		Marks:  fakeMarkProv{mp: MarkPrice{Symbol: "EUR/USD", Price: d("1.20"), Stale: true}},
		Curves: curves,
		Now:    func() time.Time { return now },
	}.OptionMarket(context.Background(), req)
	requireCode(t, err, CodeOracleUnavailable)

	// Unwired vol seam → VOLATILITY_SURFACE_UNAVAILABLE for live tenors.
	_, err = RedisOptionMarketSource{
		Marks:  fakeMarkProv{mp: MarkPrice{Price: d("1.20")}},
		Curves: curves,
		Now:    func() time.Time { return now },
	}.OptionMarket(context.Background(), req)
	requireCode(t, err, options.CodeVolatilitySurfaceUnavailable)

	// Expired row → intrinsic market: needs NO vol and NO curves —
	// proving the lifecycle-gap path never fabricates pricing inputs.
	m, err := RedisOptionMarketSource{
		Marks: fakeMarkProv{mp: MarkPrice{Price: d("1.20")}},
		Now:   func() time.Time { return now },
	}.OptionMarket(context.Background(), OptionMarketRequest{
		Underlying: "EUR/USD", BaseCCY: "EUR", QuoteCCY: "USD",
		Strike: d("1.10"), Expiry: now.Add(-time.Hour), // past expiry
	})
	if err != nil {
		t.Fatalf("expired-row market: %v", err)
	}
	if m.Spot != 1.20 || m.T != 0 {
		t.Fatalf("intrinsic market %+v", m)
	}

	// Happy path — mark + vol + curves assemble a GK market.
	m, err = RedisOptionMarketSource{
		Marks:  fakeMarkProv{mp: MarkPrice{Price: d("1.20")}},
		Curves: curves,
		Vol: OptionVolFunc(func(context.Context, string, float64, float64) (float64, error) {
			return 0.11, nil
		}),
		Now: func() time.Time { return now },
	}.OptionMarket(context.Background(), req)
	if err != nil {
		t.Fatalf("live market: %v", err)
	}
	if m.Spot != 1.20 || m.Vol != 0.11 || m.T <= 0 || m.DFd <= 0 || m.DFf <= 0 {
		t.Fatalf("assembled market %+v", m)
	}

	// Stale curve fails closed through MarketFromCurves.
	stale := flatCurve("USD", "0.05")
	stale.Stale = true
	_, err = RedisOptionMarketSource{
		Marks: fakeMarkProv{mp: MarkPrice{Price: d("1.20")}},
		Curves: fakeCurveSource{m: map[string]rates.Curve{
			"EUR": flatCurve("EUR", "0.04"), "USD": stale,
		}},
		Vol: OptionVolFunc(func(context.Context, string, float64, float64) (float64, error) {
			return 0.11, nil
		}),
		Now: func() time.Time { return now },
	}.OptionMarket(context.Background(), req)
	requireCode(t, err, options.CodeYieldCurveUnavailable)
}

// ---------------------------------------------------------------------------
// MarginUSDRateSource — USD numeraire conversion
// ---------------------------------------------------------------------------

func TestMarginUSDRateSource(t *testing.T) {
	pairs := &marginStoreFake{pairs: map[string]FxPair{
		"EUR": {Symbol: "EUR/USD"},                 // direct
		"JPY": {Symbol: "USD/JPY", Inverted: true}, // inverted
	}}
	marks := markCacheFake{m: map[string]decimal.Decimal{
		"EUR/USD": d("1.20"), "USD/JPY": d("150"),
	}}
	src := MarginUSDRateSource{Pairs: pairs, Marks: marks}
	got, err := src.RatesToUSD(context.Background(), []string{"USD", "EUR", "JPY"})
	if err != nil {
		t.Fatalf("rates: %v", err)
	}
	if !got["USD"].Equal(decimal.NewFromInt(1)) || !got["EUR"].Equal(d("1.20")) {
		t.Fatalf("usd/eur rates: %v", got)
	}
	// JPY inverted: 1/150 USD per JPY.
	if !got["JPY"].Mul(decimal.NewFromInt(150)).Round(4).Equal(decimal.NewFromInt(1)) {
		t.Fatalf("inverted jpy rate %s", got["JPY"])
	}

	// Missing pair → fail closed.
	_, err = src.RatesToUSD(context.Background(), []string{"CHF"})
	requireCode(t, err, CodeOracleUnavailable)
	// Missing mark → fail closed.
	_, err = (MarginUSDRateSource{Pairs: pairs, Marks: markCacheFake{}}).
		RatesToUSD(context.Background(), []string{"EUR"})
	requireCode(t, err, CodeOracleUnavailable)
	// Unwired seams → fail closed.
	_, err = (MarginUSDRateSource{}).RatesToUSD(context.Background(), []string{"EUR"})
	requireCode(t, err, CodeOracleUnavailable)
	// USD-only book never touches the seams.
	got, err = (MarginUSDRateSource{}).RatesToUSD(context.Background(), []string{"USD"})
	if err != nil || !got["USD"].Equal(decimal.NewFromInt(1)) {
		t.Fatalf("usd-only rates %v %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// MarginService composition — equity leg 3 + PORTFOLIO spread relief
// ---------------------------------------------------------------------------

func TestMarginEvaluateOptionDeltaLeg(t *testing.T) {
	store, marks := marginEvalFixture() // equity 3500 baseline (see margin_test)

	// Bound evaluator → signed adjustment composes into equity.
	svc := newMarginSvc(t, MarginOptions{
		Store: store, Marks: marks,
		OptionMargin: fakeEval{adj: d("-250")}})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.Equity.Equal(d("3250")) {
		t.Fatalf("equity %s, want 3250 (3500 − 250)", snap.Equity)
	}

	// Nil evaluator → byte-for-byte the pre-linkage evaluation.
	svc = newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err = svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.Equity.Equal(d("3500")) {
		t.Fatalf("nil evaluator equity %s, want 3500 unchanged", snap.Equity)
	}

	// Evaluator error fails the evaluation closed — the delta leg is
	// never silently zeroed.
	svc = newMarginSvc(t, MarginOptions{
		Store: store, Marks: marks,
		OptionMargin: fakeEval{err: stderrors.New("option book unreadable")}})
	_, err = svc.Evaluate(context.Background(), 7)
	requireCode(t, err, CodeRiskLimitsInternal)
}

func TestMarginEvaluatePortfolioSpreadRelief(t *testing.T) {
	// PORTFOLIO account with two naked legs: used = 1000 + 600 = 1600
	// before relief (no correlation bound).
	store := &marginStoreFake{
		account:  &MarginAccount{AccountID: 7, Mode: ModePortfolio, Status: "NORMAL"},
		category: "PROFESSIONAL",
		balances: []BalanceAmount{{Currency: "USD", Available: d("5000")}},
		positions: []MarginPosition{
			{ID: 11, InstrumentID: 1, Symbol: "EUR/USD", Side: "LONG",
				Quantity: d("10000"), EntryPrice: d("1.10"),
				MarginUsed: d("1000"), QuoteCurrency: "USD", MaxLeverage: 30},
			{ID: 22, InstrumentID: 2, Symbol: "GBP/USD", Side: "SHORT",
				Quantity: d("5000"), EntryPrice: d("1.30"),
				MarginUsed: d("600"), QuoteCurrency: "USD", MaxLeverage: 20},
		},
	}
	marks := markCacheFake{m: map[string]decimal.Decimal{
		"EUR/USD": d("1.10"), "GBP/USD": d("1.30"),
	}}
	spreads := &fakeSpreads{offs: []SpreadOffset{
		{AccountID: 7, LongLegID: 11, ShortLegID: 22,
			OffsetUSD: d("400"), Strategy: "VERTICAL_CALL"},
		// Duplicated row — the single-grant book must not double-apply.
		{AccountID: 7, LongLegID: 11, ShortLegID: 22,
			OffsetUSD: d("400"), Strategy: "VERTICAL_CALL"},
	}}
	svc := newMarginSvc(t, MarginOptions{
		Store: store, Marks: marks, SpreadOffsets: spreads})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1200")) {
		t.Fatalf("used %s, want 1200 (1600 − 400, granted once)", snap.UsedMargin)
	}
	if spreads.calls != 1 {
		t.Fatalf("spread source consulted %d times, want 1", spreads.calls)
	}

	// Nil source → zero relief, used unchanged.
	svc = newMarginSvc(t, MarginOptions{Store: store, Marks: marks})
	snap, err = svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1600")) {
		t.Fatalf("nil spreads used %s, want 1600", snap.UsedMargin)
	}

	// Bound source error fails the evaluation closed.
	svc = newMarginSvc(t, MarginOptions{
		Store: store, Marks: marks,
		SpreadOffsets: &fakeSpreads{err: stderrors.New("spread table gone")}})
	_, err = svc.Evaluate(context.Background(), 7)
	requireCode(t, err, CodeRiskLimitsInternal)
}

func TestMarginEvaluateSpreadReliefPortfolioOnly(t *testing.T) {
	// CROSS mode never consults the source — option spreads carry no
	// recognized relief outside PORTFOLIO.
	store, marks := marginEvalFixture()
	spreads := &fakeSpreads{offs: []SpreadOffset{
		{AccountID: 7, LongLegID: 11, ShortLegID: 22, OffsetUSD: d("400")}}}
	svc := newMarginSvc(t, MarginOptions{
		Store: store, Marks: marks, SpreadOffsets: spreads})
	snap, err := svc.Evaluate(context.Background(), 7)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !snap.UsedMargin.Equal(d("1000")) {
		t.Fatalf("cross used %s, want 1000 (no relief)", snap.UsedMargin)
	}
	if spreads.calls != 0 {
		t.Fatalf("CROSS must not consult spread source (%d calls)", spreads.calls)
	}
}
