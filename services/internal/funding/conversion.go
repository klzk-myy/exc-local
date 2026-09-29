// Phase-11 Task 11.3.9 — currency conversion engine (task item 4).
//
// When a deposit arrives in currency X while the account's base currency
// is Y, the engine converts at the reference mid-rate ±
// conversion_spread_bps (default 50bps) and persists a
// funding_currency_conversions record (migration 198) so the indicative quote is
// auditable. The client-visible surface shows converted amount and fee
// separately — this service owns the conversion half.
//
// Rate provenance is fail-closed (spec §2.7): the canonical source is the
// Phase-19.5 Price Oracle via marketdata.ReferencePriceSource
// (OracleRateSource — tries "{from}/{to}", then "{to}/{from}" inverted);
// RedisCrossRateSource derives a USD cross from the market-data
// pipeline's fx:rate:{CCY}USD keys when no oracle is wired. Missing,
// stale, or non-positive rates return PRICE_ORACLE_UNAVAILABLE — never a
// fabricated rate.
package funding

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/internal/marketdata"
	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// DefaultConversionSpreadBps is the task-pinned conversion_spread_bps
// default (50bps) applied around the mid-rate.
var DefaultConversionSpreadBps = decimal.NewFromInt(50)

// OracleStalenessGate is the spec oracle staleness gate (5s) applied to
// oracle-sourced mid-rates.
const OracleStalenessGate = 5 * time.Second

// MidRate is one usable reference observation: to_currency units per 1
// from_currency, with provenance + the observation's staleness anchor.
type MidRate struct {
	Rate    decimal.Decimal
	Source  string // oracle provenance enum, or "fx_cross_usd"
	ValidAt time.Time
}

// ConversionRateSource is the indicative-rate seam. Implementations fail
// closed — an error means the rate is genuinely unavailable.
type ConversionRateSource interface {
	MidRate(ctx context.Context, fromCurrency, toCurrency string) (MidRate, error)
}

// ---------------------------------------------------------------------------
// OracleRateSource — Phase-19.5 oracle adapter (preferred source).
// ---------------------------------------------------------------------------

// OracleRateSource adapts marketdata.ReferencePriceSource to the
// conversion seam: "{from}/{to}" is tried first, "{to}/{from}" inverted
// second. The 5s staleness gate applies to the oracle's ValidAt — a
// stale or non-positive observation errors, never converts.
type OracleRateSource struct {
	Src  marketdata.ReferencePriceSource
	Gate time.Duration // staleness gate; default 5s
	Now  func() time.Time
}

// MidRate implements ConversionRateSource.
func (o *OracleRateSource) MidRate(ctx context.Context, from, to string) (MidRate, error) {
	if o.Src == nil {
		return MidRate{}, fmt.Errorf("conversion: oracle source not wired")
	}
	gate := o.Gate
	if gate <= 0 {
		gate = OracleStalenessGate
	}
	now := time.Now()
	if o.Now != nil {
		now = o.Now()
	}
	pair := from + "/" + to
	ref, err := o.Src.Reference(ctx, pair)
	inverted := false
	if err != nil {
		inv, ierr := o.Src.Reference(ctx, to+"/"+from)
		if ierr != nil {
			return MidRate{}, fmt.Errorf("conversion: oracle %s and inverse %s/%s unavailable: %w",
				pair, to, from, ierr)
		}
		ref, inverted = inv, true
	}
	if ref.Price <= 0 {
		return MidRate{}, fmt.Errorf("conversion: oracle %s returned invalid price", pair)
	}
	// Negative age (source clock marginally ahead) is tolerated — only
	// age > gate is stale.
	if age := now.Sub(ref.ValidAt); age > gate {
		return MidRate{}, fmt.Errorf("conversion: oracle %s stale (age %s > %s)", pair, age, gate)
	}
	rate := decimal.NewFromScaled(ref.Price)
	if inverted {
		rate = decimal.One.Div(rate)
	}
	src := ref.Source
	if inverted {
		src += ":inverted"
	}
	return MidRate{Rate: rate, Source: src, ValidAt: ref.ValidAt}, nil
}

// ---------------------------------------------------------------------------
// RedisCrossRateSource — fx:rate:{CCY}USD cross-rate fallback.
// ---------------------------------------------------------------------------

// RedisCrossRateSource derives the {from}→{to} rate from the market-data
// pipeline's USD legs (fx:rate:{CCY}USD — the keys RedisUsdConverter
// consumes). Both legs must be present and positive; the read timestamp
// is the staleness anchor. Missing keys fail closed.
type RedisCrossRateSource struct {
	Rdb *excredis.Client
	Now func() time.Time
}

func (c *RedisCrossRateSource) usdLeg(ctx context.Context, ccy string) (decimal.Decimal, error) {
	if c.Rdb == nil {
		return decimal.Zero, fmt.Errorf("conversion: redis rate source not wired")
	}
	if ccy == "USD" {
		return decimal.One, nil
	}
	txt, err := c.Rdb.Get(ctx, fmt.Sprintf("fx:rate:%sUSD", ccy)).Result()
	if err != nil {
		return decimal.Zero, fmt.Errorf("conversion: fx:rate:%sUSD unavailable", ccy)
	}
	r, err := decimal.NewFromString(txt)
	if err != nil || !r.IsPositive() {
		return decimal.Zero, fmt.Errorf("conversion: fx:rate:%sUSD invalid %q", ccy, txt)
	}
	return r, nil
}

// MidRate implements ConversionRateSource: X→Y = XUSD / YUSD.
func (c *RedisCrossRateSource) MidRate(ctx context.Context, from, to string) (MidRate, error) {
	fromLeg, err := c.usdLeg(ctx, from)
	if err != nil {
		return MidRate{}, err
	}
	toLeg, err := c.usdLeg(ctx, to)
	if err != nil {
		return MidRate{}, err
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	return MidRate{Rate: fromLeg.Div(toLeg), Source: "fx_cross_usd", ValidAt: now}, nil
}

// ---------------------------------------------------------------------------
// Conversion service + store.
// ---------------------------------------------------------------------------

// ConversionRecord is one persisted funding_currency_conversions row.
type ConversionRecord struct {
	ID           int64           `json:"id"`
	AccountID    int64           `json:"account_id"`
	Direction    string          `json:"direction"` // DEPOSIT|WITHDRAWAL
	FromCurrency string          `json:"from_currency"`
	ToCurrency   string          `json:"to_currency"`
	AmountFrom   decimal.Decimal `json:"amount_from"`
	MidRate      decimal.Decimal `json:"mid_rate"`
	SpreadBps    decimal.Decimal `json:"spread_bps"`
	RateApplied  decimal.Decimal `json:"rate_applied"`
	AmountTo     decimal.Decimal `json:"amount_to"`
	RateSource   string          `json:"rate_source"`
	RateValidAt  time.Time       `json:"rate_valid_at"`
	FundingTxID  *int64          `json:"funding_transaction_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// ConversionStore persists + reads conversion records.
type ConversionStore interface {
	InsertConversion(ctx context.Context, rec ConversionRecord) (*ConversionRecord, error)
	// ConversionHistory pages an account's records newest first.
	ConversionHistory(ctx context.Context, accountID int64, limit int) ([]ConversionRecord, error)
}

// ConversionRequest is the POST /api/v1/funding/convert payload plus
// handler-resolved account context.
type ConversionRequest struct {
	AccountID    int64
	FromCurrency string
	ToCurrency   string // "" → the account's base currency
	Direction    string // "" → DEPOSIT
	Amount       decimal.Decimal
}

// ConversionResult is the indicative quote. Same-currency requests
// return Converted=false at rate 1 — no oracle call, no record.
type ConversionResult struct {
	Converted    bool              `json:"converted"`
	Direction    string            `json:"direction"`
	FromCurrency string            `json:"from_currency"`
	ToCurrency   string            `json:"to_currency"`
	AmountFrom   decimal.Decimal   `json:"amount_from"`
	AmountTo     decimal.Decimal   `json:"amount_to"`
	MidRate      decimal.Decimal   `json:"mid_rate"`
	SpreadBps    decimal.Decimal   `json:"spread_bps"`
	RateApplied  decimal.Decimal   `json:"rate_applied"`
	RateSource   string            `json:"rate_source,omitempty"`
	RateValidAt  *time.Time        `json:"rate_valid_at,omitempty"`
	Record       *ConversionRecord `json:"conversion,omitempty"`
}

// ConversionService computes + persists indicative conversions.
// src nil fails closed PRICE_ORACLE_UNAVAILABLE — no source, no rate.
type ConversionService struct {
	src      ConversionRateSource
	store    ConversionStore
	accounts AccountMetaSource
	spread   decimal.Decimal // bps
	now      func() time.Time
}

// ConversionOption customizes the service (spread override, clock).
type ConversionOption func(*ConversionService)

// WithConversionSpreadBps overrides the default 50bps spread.
func WithConversionSpreadBps(bps decimal.Decimal) ConversionOption {
	return func(s *ConversionService) { s.spread = bps }
}

// WithConversionClock overrides the clock (tests).
func WithConversionClock(now func() time.Time) ConversionOption {
	return func(s *ConversionService) { s.now = now }
}

// NewConversionService wires the engine. store and accounts are
// required; src may be nil (every conversion then fails closed
// PRICE_ORACLE_UNAVAILABLE, honestly reporting the unwired source).
func NewConversionService(src ConversionRateSource, store ConversionStore,
	accounts AccountMetaSource, opts ...ConversionOption) (*ConversionService, error) {
	if store == nil || accounts == nil {
		return nil, fmt.Errorf("funding: conversion service requires store + account meta")
	}
	s := &ConversionService{src: src, store: store, accounts: accounts,
		spread: DefaultConversionSpreadBps, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	if s.spread.IsNegative() {
		return nil, fmt.Errorf("funding: conversion spread must be >= 0")
	}
	return s, nil
}

// Quote computes and persists an indicative conversion. DEPOSIT applies
// mid×(1−spread) — the client receives fewer to_currency units than the
// mid implies (spread accrues to the house); WITHDRAWAL applies
// mid×(1+spread). Same-currency requests short-circuit to rate 1 with no
// record — nothing was converted.
func (s *ConversionService) Quote(ctx context.Context, req ConversionRequest) (*ConversionResult, error) {
	from, err := normalizeCurrency(req.FromCurrency)
	if err != nil {
		return nil, errCode("INVALID_REQUEST", "from_currency must be a 3-letter ISO code")
	}
	to := strings.ToUpper(strings.TrimSpace(req.ToCurrency))
	if to == "" {
		meta, err := s.accounts.AccountMeta(ctx, req.AccountID)
		if err != nil {
			return nil, err
		}
		to = meta.BaseCurrency
	}
	if to == "" {
		return nil, errCode("INVALID_REQUEST",
			"to_currency required (account has no base currency)")
	}
	to, err = normalizeCurrency(to)
	if err != nil {
		return nil, errCode("INVALID_REQUEST", "to_currency must be a 3-letter ISO code")
	}
	if !req.Amount.IsPositive() || req.Amount.Round(8).Compare(req.Amount) != 0 {
		return nil, errCode("INVALID_REQUEST", "amount must be positive, ≤8dp")
	}
	dir := strings.ToUpper(strings.TrimSpace(req.Direction))
	if dir == "" {
		dir = "DEPOSIT"
	}
	if !feeDirectionDomain[dir] {
		return nil, errf("INVALID_REQUEST", "direction %q must be DEPOSIT|WITHDRAWAL", dir)
	}

	res := &ConversionResult{
		Direction: dir, FromCurrency: from, ToCurrency: to,
		AmountFrom: req.Amount, AmountTo: req.Amount,
		MidRate: decimal.One, SpreadBps: decimal.Zero, RateApplied: decimal.One,
	}
	if from == to {
		return res, nil
	}
	if s.src == nil {
		return nil, errCode("PRICE_ORACLE_UNAVAILABLE",
			"conversion rate source not wired — no indicative rate available")
	}
	mid, err := s.src.MidRate(ctx, from, to)
	if err != nil || !mid.Rate.IsPositive() {
		return nil, wrapCode("PRICE_ORACLE_UNAVAILABLE",
			"conversion mid-rate unavailable", fmt.Errorf("%s/%s: %v", from, to, err))
	}
	factor := decimal.One.Sub(s.spread.Div(decimal.NewFromInt(10_000)))
	if dir == "WITHDRAWAL" {
		factor = decimal.One.Add(s.spread.Div(decimal.NewFromInt(10_000)))
	}
	applied := mid.Rate.Mul(factor)
	amountTo := req.Amount.Mul(applied).Round(8)
	if !applied.IsPositive() || !amountTo.IsPositive() {
		return nil, errf("FUNDING_RATE_ERROR",
			"conversion %s→%s produced a non-positive result", from, to)
	}

	rec, err := s.store.InsertConversion(ctx, ConversionRecord{
		AccountID: req.AccountID, Direction: dir,
		FromCurrency: from, ToCurrency: to,
		AmountFrom: req.Amount, MidRate: mid.Rate,
		SpreadBps: s.spread, RateApplied: applied.Round(10),
		AmountTo: amountTo, RateSource: mid.Source, RateValidAt: mid.ValidAt,
	})
	if err != nil {
		return nil, err
	}
	res.Converted = true
	res.MidRate = mid.Rate
	res.SpreadBps = s.spread
	res.RateApplied = applied.Round(10)
	res.AmountTo = amountTo
	res.RateSource = mid.Source
	res.RateValidAt = &mid.ValidAt
	res.Record = rec
	return res, nil
}

// History serves GET /api/v1/funding/conversions — the account's
// persisted conversion records, newest first.
func (s *ConversionService) History(ctx context.Context, accountID int64,
	limit int) ([]ConversionRecord, error) {
	rows, err := s.store.ConversionHistory(ctx, accountID, limit)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []ConversionRecord{}
	}
	return rows, nil
}
