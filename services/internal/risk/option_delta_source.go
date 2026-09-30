// option_delta_source.go — Phase-19 Task 19.3.25 back-fit: the
// production OptionDeltaSource over the Phase-22 option book
// (option_positions, migration 255; spec §13.12/§15.7, §24 #398).
//
// Delta is NOT persisted anywhere — this source prices every OPEN leg
// at the current market: analytic Garman-Kohlhagen Greeks for EUROPEAN
// contracts (options.VanillaOption.PriceAndGreeks, model "GK"), and
// central-difference bump-and-reprice over the Kamrad–Ritchken
// trinomial lattice for AMERICAN (model "LATTICE") — the same methods
// the Phase-23 greeks feed uses, but bound to the FULL spec §15.2
// lattice budget: the feed's cheaper config is a 10Hz market-data
// concession, never a risk one. Rows past their expiry instant (the
// lifecycle batch gap before the expiry sweep flips status) take the
// intrinsic payoff/delta — deterministic, needing no vol or curve.
//
// Fail-closed contract (spec §2.7, task text "missing mark/delta →
// fail closed"): a missing or stale underlying mark, an unavailable or
// stale discount curve, an unwired/unavailable IV surface, an
// unparseable book row, a non-USD quote without a resolvable USD rate,
// or a BINARY leg (no analytic delta exists on this path) fails the
// whole read — DeltaOptionMarginEvaluator propagates the error to
// MarginService and the account's delta leg is NEVER silently zeroed.
package risk

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/options"
	"exchange/internal/oracle/rates"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Pricer seam — delta + mark per contract
// ---------------------------------------------------------------------------

// OptionPricerResult is one contract's priced outputs at a resolved
// market. Delta is the holder-side per-contract spot delta; Mark is the
// model price in quote (settlement) currency per unit of base notional;
// Model is the provenance label ("GK" | "LATTICE" | "INTRINSIC").
type OptionPricerResult struct {
	Delta float64
	Mark  float64
	Model string
}

// OptionDeltaPricer prices one option contract's delta and mark at a
// resolved market. Production binds OptionsDeltaPricer.
type OptionDeltaPricer interface {
	Price(right options.OptionRight, style options.ExerciseStyle,
		strike float64, m options.Market) (OptionPricerResult, error)
}

// OptionsDeltaPricer is the production pricer: GK closed form for
// EUROPEAN contracts, central-difference bump-and-reprice over the
// trinomial lattice for AMERICAN. A zero Lattice picks
// options.DefaultLatticeConfig — the spec §15.2 bound set (100..10,000
// steps, 1e-4 tolerance).
type OptionsDeltaPricer struct {
	Lattice options.LatticeConfig
}

func (p OptionsDeltaPricer) latticeCfg() options.LatticeConfig {
	if p.Lattice.MinSteps > 0 {
		return p.Lattice
	}
	return options.DefaultLatticeConfig
}

// Price implements OptionDeltaPricer. m.T <= 0 short-circuits to the
// intrinsic payoff and step-function delta (the expired-row path —
// VanillaOption.expiryGreeks semantics without the vol/curve inputs an
// expired contract no longer needs).
func (p OptionsDeltaPricer) Price(right options.OptionRight, style options.ExerciseStyle,
	strike float64, m options.Market) (OptionPricerResult, error) {
	const op = "option delta"
	if !(finite64(strike)) || strike <= 0 {
		return OptionPricerResult{}, excerrors.New(options.CodeOptionPricingInputInvalid,
			fmt.Sprintf("%s: strike must be positive and finite, got %v", op, strike))
	}
	if !(finite64(m.Spot)) || m.Spot <= 0 {
		return OptionPricerResult{}, excerrors.New(options.CodeOptionPricingInputInvalid,
			fmt.Sprintf("%s: underlying spot must be positive and finite, got %v", op, m.Spot))
	}
	if m.T <= 0 {
		return intrinsicOptionPrice(right, strike, m.Spot)
	}
	switch style {
	case options.ExerciseEuropean:
		px, g, err := options.VanillaOption{
			Right: right, Strike: strike, Style: style,
		}.PriceAndGreeks(m)
		if err != nil {
			return OptionPricerResult{}, err
		}
		return OptionPricerResult{Delta: g.Delta, Mark: px, Model: "GK"}, nil
	case options.ExerciseAmerican:
		return p.latticePrice(right, strike, m)
	}
	return OptionPricerResult{}, excerrors.New(options.CodeOptionPricingInputInvalid,
		fmt.Sprintf("%s: unsupported exercise style %q", op, style))
}

// latticePrice computes the American leg's mark and spot delta by
// bump-and-reprice over the lattice (the honest method — the options
// package exposes a lattice price, not analytic American Greeks). Every
// evaluation must return finite; a non-finite leg fails closed.
func (p OptionsDeltaPricer) latticePrice(right options.OptionRight,
	strike float64, m options.Market) (OptionPricerResult, error) {
	cfg := p.latticeCfg()
	price := func(mm options.Market) (float64, error) {
		res, err := options.PriceAmerican(options.VanillaOption{
			Right: right, Strike: strike, Style: options.ExerciseAmerican,
		}, mm, cfg)
		if err != nil {
			return 0, err
		}
		return res.Price, nil
	}
	v0, err := price(m)
	if err != nil {
		return OptionPricerResult{}, err
	}
	h := math.Max(m.Spot*1e-4, 1e-7)
	up, dn := m, m
	up.Spot += h
	dn.Spot -= h
	vUp, err := price(up)
	if err != nil {
		return OptionPricerResult{}, err
	}
	vDn, err := price(dn)
	if err != nil {
		return OptionPricerResult{}, err
	}
	delta := (vUp - vDn) / (2 * h)
	if !finite64(delta) || !finite64(v0) {
		return OptionPricerResult{}, excerrors.New(options.CodeOptionPricingConvergence,
			fmt.Sprintf("option delta: non-finite lattice result (delta %v mark %v)", delta, v0))
	}
	return OptionPricerResult{Delta: delta, Mark: v0, Model: "LATTICE"}, nil
}

// intrinsicOptionPrice is the expired-row path (m.T <= 0): mark =
// payoff at spot, delta = the post-expiry delivery step — +1 for an
// ITM call, −1 for an ITM put, 0 out of the money (mirrors
// VanillaOption.expiryGreeks with DFd = DFf = 1).
func intrinsicOptionPrice(right options.OptionRight, strike, spot float64) (OptionPricerResult, error) {
	switch right {
	case options.OptionCall:
		if spot > strike {
			return OptionPricerResult{Delta: 1, Mark: spot - strike, Model: "INTRINSIC"}, nil
		}
	case options.OptionPut:
		if spot < strike {
			return OptionPricerResult{Delta: -1, Mark: strike - spot, Model: "INTRINSIC"}, nil
		}
	default:
		return OptionPricerResult{}, excerrors.New(options.CodeOptionPricingInputInvalid,
			fmt.Sprintf("option delta: unknown option right %q", right))
	}
	return OptionPricerResult{Model: "INTRINSIC"}, nil
}

// finite64 mirrors options' internal finiteness gate (unexported there).
func finite64(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// ---------------------------------------------------------------------------
// Market seam — the GK pricing environment per contract
// ---------------------------------------------------------------------------

// OptionMarketRequest identifies one contract's pricing environment:
// the underlying FX pair, its expiry instant (tenor anchor), and its
// strike (IV-surface query point).
type OptionMarketRequest struct {
	Underlying string          // canonical pair symbol, e.g. "EUR/USD"
	BaseCCY    string          // underlying base currency (foreign leg)
	QuoteCCY   string          // underlying quote currency (domestic leg)
	Strike     decimal.Decimal // contract strike (quote per base)
	Expiry     time.Time       // expiry instant (option_positions.expiry_at)
}

// OptionDeltaMarketSource resolves options.Market for one contract:
// underlying spot, base/quote discount factors to expiry, implied vol
// at (T, strike). Production binds RedisOptionMarketSource; tests
// inject fakes. Implementations fail loudly — a delta priced off
// fabricated inputs is worse than no delta (spec §2.7).
type OptionDeltaMarketSource interface {
	OptionMarket(ctx context.Context, req OptionMarketRequest) (options.Market, error)
}

// OptionCurveSource reads the per-currency yield curves published by
// the PriceOracle (curve:{ccy}); *rates.Store satisfies it with the 5s
// staleness gate built in. Declared interface-side — risk never imports
// the oracle's own packages (import direction is oracle → risk).
type OptionCurveSource interface {
	GetCurve(ctx context.Context, ccy string) (rates.Curve, error)
}

// OptionVolSource resolves the annualized implied vol for
// (underlying, tenor-years, strike) — the Phase-22 options.IVSurface.Vol
// binding. Nil/unavailable fails the delta leg closed: the margin path
// never substitutes a guessed vol (same posture as the greeks feed's
// frozen-frame rule).
type OptionVolSource interface {
	Vol(ctx context.Context, underlying string, tYears, strike float64) (float64, error)
}

// OptionVolFunc adapts a function to OptionVolSource.
type OptionVolFunc func(ctx context.Context, underlying string, tYears, strike float64) (float64, error)

// Vol implements OptionVolSource.
func (f OptionVolFunc) Vol(ctx context.Context, underlying string, tYears, strike float64) (float64, error) {
	return f(ctx, underlying, tYears, strike)
}

// RedisOptionMarketSource is the production OptionDeltaMarketSource:
// underlying spot from the chained oracle mark provider, discount
// factors from the published rate curves (MarketFromCurves re-validates
// completeness + staleness), vol from the IV-surface seam. Vol may be
// nil — any option leg then fails closed rather than pricing off a
// fabricated vol.
type RedisOptionMarketSource struct {
	Marks  MarkPriceProvider // required — chained oracle + last-trade stub
	Curves OptionCurveSource // required for live tenors — *rates.Store
	Vol    OptionVolSource   // may be nil → coded VOLATILITY_SURFACE_UNAVAILABLE
	Now    func() time.Time  // nil → time.Now
}

// OptionMarket implements OptionDeltaMarketSource.
func (s RedisOptionMarketSource) OptionMarket(ctx context.Context,
	req OptionMarketRequest) (options.Market, error) {
	if s.Marks == nil {
		return options.Market{}, excerrors.New(CodeOracleUnavailable,
			"option delta: mark provider unwired")
	}
	mp, err := s.Marks.GetMarkPriceWithProvenance(req.Underlying)
	if err != nil {
		return options.Market{}, excerrors.Wrap(CodeOracleUnavailable,
			fmt.Sprintf("option delta: mark %s", req.Underlying), err)
	}
	if mp.Stale {
		return options.Market{}, excerrors.New(CodeOracleUnavailable,
			fmt.Sprintf("option delta: stale mark for %s", req.Underlying))
	}
	spot := mp.Price.InexactFloat64()
	if !(spot > 0) || !finite64(spot) {
		return options.Market{}, excerrors.New(CodeOracleUnavailable,
			fmt.Sprintf("option delta: non-positive mark %s for %s", mp.Price, req.Underlying))
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	// Tenor in whole calendar days (the curve's day axis); ceil keeps an
	// unexpired contract at T > 0 until its expiry instant passes.
	days := int(math.Ceil(req.Expiry.Sub(now().UTC()).Hours() / 24))
	if days <= 0 {
		// Expired-but-OPEN row (lifecycle batch gap): intrinsic path —
		// the pricer consumes only Spot here; DF legs are neutral and
		// Vol is deliberately left zero because it is never read.
		return options.Market{Spot: spot, DFd: 1, DFf: 1, T: 0}, nil
	}
	tYears := float64(days) / 365.0
	if s.Vol == nil {
		return options.Market{}, excerrors.New(options.CodeVolatilitySurfaceUnavailable,
			fmt.Sprintf("option delta: IV-surface source unwired for %s", req.Underlying))
	}
	strike := req.Strike.InexactFloat64()
	vol, err := s.Vol.Vol(ctx, req.Underlying, tYears, strike)
	if err != nil {
		return options.Market{}, err
	}
	if s.Curves == nil {
		return options.Market{}, excerrors.New(options.CodeYieldCurveUnavailable,
			"option delta: yield-curve source unwired")
	}
	base, err := s.Curves.GetCurve(ctx, req.BaseCCY)
	if err != nil {
		return options.Market{}, excerrors.Wrap(options.CodeYieldCurveUnavailable,
			fmt.Sprintf("option delta: %s curve", req.BaseCCY), err)
	}
	quote, err := s.Curves.GetCurve(ctx, req.QuoteCCY)
	if err != nil {
		return options.Market{}, excerrors.Wrap(options.CodeYieldCurveUnavailable,
			fmt.Sprintf("option delta: %s curve", req.QuoteCCY), err)
	}
	return options.MarketFromCurves(spot, vol, days, base, quote)
}

// ---------------------------------------------------------------------------
// USD numeraire seam — contract notional in USD
// ---------------------------------------------------------------------------

// FxPairSource resolves {CCY}/USD or USD/{CCY} conversion instruments;
// *PgMarginStore satisfies it (MarginStore.FxPairInstruments).
type FxPairSource interface {
	FxPairInstruments(ctx context.Context, ccys []string) (map[string]FxPair, error)
}

// OptionUSDRateSource resolves currency→USD rates for the notional leg
// (USD per 1 unit of the currency).
type OptionUSDRateSource interface {
	RatesToUSD(ctx context.Context, ccys []string) (map[string]decimal.Decimal, error)
}

// MarginUSDRateSource is the production OptionUSDRateSource — the same
// FxPairInstruments + MarkCache machinery MarginService evaluates
// against (ONE pairs query + ONE MGET per call, spec §13.1 no-N+1).
// Missing instruments or marks fail closed with
// PRICE_ORACLE_UNAVAILABLE — a notional that cannot be valued in USD is
// never understated.
type MarginUSDRateSource struct {
	Pairs FxPairSource
	Marks MarkCache
}

// RatesToUSD implements OptionUSDRateSource.
func (s MarginUSDRateSource) RatesToUSD(ctx context.Context,
	ccys []string) (map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	need := map[string]bool{}
	var list []string
	for _, c := range ccys {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || need[c] {
			continue
		}
		need[c] = true
		if c == "USD" {
			out["USD"] = decimal.NewFromInt(1)
			continue
		}
		list = append(list, c)
	}
	if len(list) == 0 {
		return out, nil
	}
	if s.Pairs == nil || s.Marks == nil {
		return nil, excerrors.New(CodeOracleUnavailable,
			"option delta: USD conversion source unwired")
	}
	pairs, err := s.Pairs.FxPairInstruments(ctx, list)
	if err != nil {
		return nil, excerrors.Wrap(CodeOracleUnavailable, "option delta: fx pairs", err)
	}
	var syms []string
	for _, fp := range pairs {
		syms = append(syms, fp.Symbol)
	}
	marks, err := s.Marks.BatchMarks(ctx, syms)
	if err != nil {
		return nil, excerrors.Wrap(CodeOracleUnavailable, "option delta: conversion marks", err)
	}
	for _, c := range list {
		fp, ok := pairs[c]
		if !ok {
			return nil, excerrors.New(CodeOracleUnavailable,
				fmt.Sprintf("option delta: no USD conversion instrument for %s", c))
		}
		m, ok := marks[fp.Symbol]
		if !ok || !m.IsPositive() {
			return nil, excerrors.New(CodeOracleUnavailable,
				fmt.Sprintf("option delta: no USD conversion mark for %s via %s", c, fp.Symbol))
		}
		if fp.Inverted {
			out[c] = decimal.NewFromInt(1).Div(m)
		} else {
			out[c] = m
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// PgOptionDeltaSource — the production OptionDeltaSource
// ---------------------------------------------------------------------------

// PgOptionDeltaSource loads the account's OPEN option_positions rows
// (migration 255) joined to the option instrument and the deliverable
// underlying, prices each leg's delta at the resolved market, and maps
// rows into the risk.OptionPosition contract the delta-margin
// evaluator consumes.
type PgOptionDeltaSource struct {
	Pool    *pgxpool.Pool
	Markets OptionDeltaMarketSource // required — priced, never fabricated
	Pricer  OptionDeltaPricer       // nil → OptionsDeltaPricer{} (GK/lattice)
	Rates   OptionUSDRateSource     // nil → USD-quoted books only (fail closed else)
}

// NewPgOptionDeltaSource binds the pool and the pricing-market seam —
// both required at construction (a delta source that cannot price must
// not exist; fail closed at build time, spec §2.7). rates may be nil
// for USD-quoted-only books.
func NewPgOptionDeltaSource(pool *pgxpool.Pool, markets OptionDeltaMarketSource,
	rates OptionUSDRateSource) (*PgOptionDeltaSource, error) {
	if pool == nil {
		return nil, fmt.Errorf("option delta source: nil pgx pool")
	}
	if markets == nil {
		return nil, fmt.Errorf("option delta source: nil market source — delta can never be fabricated")
	}
	return &PgOptionDeltaSource{Pool: pool, Markets: markets,
		Pricer: OptionsDeltaPricer{}, Rates: rates}, nil
}

// optionDeltaRow is the scanned option_positions + instruments view.
type optionDeltaRow struct {
	PositionID   int64
	AccountID    int64
	InstrumentID int64
	UnderlyingID int64
	OptionType   string // CALL | PUT | BINARY
	Style        string // EUROPEAN | AMERICAN
	Side         string // LONG holder | SHORT writer
	Quantity     decimal.Decimal
	Strike       decimal.Decimal
	Expiry       time.Time
	ContractSize decimal.Decimal // base-currency units per contract
	UndSymbol    string          // underlying pair symbol for the mark
	UndBase      string
	UndQuote     string
}

// OptionPositions implements OptionDeltaSource.
func (s *PgOptionDeltaSource) OptionPositions(ctx context.Context,
	accountID int64) ([]OptionPosition, error) {
	rows, err := s.loadRows(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil // no option book — zero adjustment, no market reads
	}
	// Quote-currency → USD rates, ONE call for the whole book.
	ccys := map[string]bool{}
	var ccyList []string
	for _, r := range rows {
		if !ccys[r.UndQuote] {
			ccys[r.UndQuote] = true
			ccyList = append(ccyList, r.UndQuote)
		}
	}
	ratesUSD := map[string]decimal.Decimal{"USD": decimal.NewFromInt(1)}
	if len(ccyList) > 0 && !(len(ccyList) == 1 && ccyList[0] == "USD") {
		if s.Rates == nil {
			return nil, excerrors.New(CodeOracleUnavailable,
				fmt.Sprintf("option margin: account %d holds non-USD-quoted options but no USD rate source is bound", accountID))
		}
		ratesUSD, err = s.Rates.RatesToUSD(ctx, ccyList)
		if err != nil {
			return nil, err
		}
	}
	// Market resolution memoized per (underlying, expiry, strike) —
	// identical contracts share one pricing environment.
	type mkey struct {
		und    string
		expiry time.Time
		strike string
	}
	mkCache := map[mkey]options.Market{}
	mkErr := map[mkey]error{}
	pricer := s.Pricer
	if pricer == nil {
		pricer = OptionsDeltaPricer{}
	}
	out := make([]OptionPosition, 0, len(rows))
	for _, r := range rows {
		k := mkey{r.UndSymbol, r.Expiry.UTC(), r.Strike.String()}
		m, ok := mkCache[k]
		if merr, has := mkErr[k]; has {
			return nil, merr
		} else if !ok {
			m, err = s.Markets.OptionMarket(ctx, OptionMarketRequest{
				Underlying: r.UndSymbol, BaseCCY: r.UndBase, QuoteCCY: r.UndQuote,
				Strike: r.Strike, Expiry: r.Expiry,
			})
			if err != nil {
				mkErr[k] = fmt.Errorf("option margin: market %s pos %d: %w",
					r.UndSymbol, r.PositionID, err)
				return nil, mkErr[k]
			}
			mkCache[k] = m
		}
		pos, err := s.mapRow(r, m, ratesUSD[r.UndQuote], pricer)
		if err != nil {
			return nil, err
		}
		out = append(out, pos)
	}
	return out, nil
}

// loadRows scans the account's live option legs. All money columns
// scan ::text into the decimal facade — never float64.
func (s *PgOptionDeltaSource) loadRows(ctx context.Context,
	accountID int64) ([]optionDeltaRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT o.position_id, o.account_id, o.instrument_id,
		       COALESCE(o.underlying_instrument_id, i.id),
		       o.option_type, o.exercise_style, o.side::text,
		       o.quantity::text, o.strike::text, o.expiry_at,
		       COALESCE(i.contract_size, 100000)::text,
		       COALESCE(u.symbol, i.base_currency || '/' || i.quote_currency),
		       COALESCE(u.base_currency, i.base_currency),
		       COALESCE(u.quote_currency, i.quote_currency)
		FROM option_positions o
		JOIN instruments i ON i.id = o.instrument_id
		LEFT JOIN instruments u ON u.id = o.underlying_instrument_id
		WHERE o.account_id = $1 AND o.status = 'OPEN' AND o.quantity > 0
		ORDER BY o.position_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("option positions acct %d: %w", accountID, err)
	}
	defer rows.Close()
	var out []optionDeltaRow
	for rows.Next() {
		var r optionDeltaRow
		var qty, strike, csize string
		if err := rows.Scan(&r.PositionID, &r.AccountID, &r.InstrumentID,
			&r.UnderlyingID, &r.OptionType, &r.Style, &r.Side,
			&qty, &strike, &r.Expiry, &csize,
			&r.UndSymbol, &r.UndBase, &r.UndQuote); err != nil {
			return nil, fmt.Errorf("option positions acct %d scan: %w", accountID, err)
		}
		var perr error
		if r.Quantity, perr = decimal.NewFromString(qty); perr != nil {
			return nil, fmt.Errorf("option position %d quantity %q: %w", r.PositionID, qty, perr)
		}
		if r.Strike, perr = decimal.NewFromString(strike); perr != nil {
			return nil, fmt.Errorf("option position %d strike %q: %w", r.PositionID, strike, perr)
		}
		if r.ContractSize, perr = decimal.NewFromString(csize); perr != nil {
			return nil, fmt.Errorf("option position %d contract_size %q: %w", r.PositionID, csize, perr)
		}
		r.UndBase = strings.ToUpper(r.UndBase)
		r.UndQuote = strings.ToUpper(r.UndQuote)
		out = append(out, r)
	}
	return out, rows.Err()
}

// mapRow prices one scanned row into an OptionPosition: signed delta
// (writer legs carry the negation of the holder delta — the
// OptionDeltaAdjustment sign convention), model mark in quote
// (settlement) currency per unit of base notional, and USD notional
// per contract = contract_size × spot × quote→USD.
func (s *PgOptionDeltaSource) mapRow(r optionDeltaRow, m options.Market,
	quoteToUSD decimal.Decimal, pricer OptionDeltaPricer) (OptionPosition, error) {
	right, err := options.ParseOptionRight(r.OptionType)
	if err != nil {
		return OptionPosition{}, fmt.Errorf(
			"option margin: position %d type %q: %w", r.PositionID, r.OptionType, err)
	}
	style := options.ExerciseStyle(r.Style)
	if !style.Valid() {
		return OptionPosition{}, excerrors.New(options.CodeOptionPricingInputInvalid,
			fmt.Sprintf("option margin: position %d exercise style %q malformed", r.PositionID, r.Style))
	}
	pr, err := pricer.Price(right, style, r.Strike.InexactFloat64(), m)
	if err != nil {
		return OptionPosition{}, fmt.Errorf(
			"option margin: position %d priced: %w", r.PositionID, err)
	}
	// Sanity bound — a per-contract spot delta outside [−1,1] (plus a
	// DF epsilon) can only come from a defective pricer; never let it
	// plant garbage exposure in the book.
	if math.Abs(pr.Delta) > 1.0001 {
		return OptionPosition{}, excerrors.New(options.CodeOptionPricingConvergence,
			fmt.Sprintf("option margin: position %d delta %v out of bounds", r.PositionID, pr.Delta))
	}
	signed := decimal.NewFromFloat(pr.Delta)
	if r.Side == "SHORT" {
		// Writer legs carry the negation of the holder delta.
		signed = signed.Neg()
	}
	if !quoteToUSD.IsPositive() {
		return OptionPosition{}, excerrors.New(CodeOracleUnavailable,
			fmt.Sprintf("option margin: position %d quote %s has no USD rate", r.PositionID, r.UndQuote))
	}
	notionalUSD := r.ContractSize.Mul(decimal.NewFromFloat(m.Spot)).Mul(quoteToUSD).Round(8)
	if !notionalUSD.IsPositive() {
		return OptionPosition{}, excerrors.New(CodeOracleUnavailable,
			fmt.Sprintf("option margin: position %d notional non-positive", r.PositionID))
	}
	return OptionPosition{
		AccountID:    r.AccountID,
		InstrumentID: r.InstrumentID,
		UnderlyingID: r.UnderlyingID,
		Side:         r.Side,
		Quantity:     r.Quantity,
		Delta:        signed.Round(8),
		MarkPrice:    decimal.NewFromFloat(pr.Mark).Round(8),
		NotionalUSD:  notionalUSD,
	}, nil
}

// compile-time seam assertions.
var (
	_ OptionDeltaSource       = (*PgOptionDeltaSource)(nil)
	_ OptionDeltaPricer       = OptionsDeltaPricer{}
	_ OptionDeltaMarketSource = RedisOptionMarketSource{}
	_ OptionUSDRateSource     = MarginUSDRateSource{}
	_ OptionVolSource         = OptionVolFunc(nil)
)
