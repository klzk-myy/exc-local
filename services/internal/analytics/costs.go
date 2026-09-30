// costs.go — Phase-20 Task 20.3.14 (spec §16.8, §24 #374): MiFID II
// Costs & Charges disclosure, ex-ante preview + annual ex-post.
//
// Ex-ante (GET /api/v1/account/cost-preview): the all-in cost estimate
// for an order before entry — spread cost, explicit commission and a
// one-night financing (Tom-Next) estimate, denominated in the
// instrument's quote currency and additionally converted into the
// account's base currency when the two differ.
//
// Estimator honesty (fail-closed §2.7 — a preview is a disclosure, not
// a quote, but it must never fabricate a number):
//
//   - Mid price: live oracle mark (oracle.MarkReader). Missing/stale →
//     PRICE_ORACLE_UNAVAILABLE / MARK_PRICE_STALE — no estimate on a
//     dead mark. The response carries quoted_at + valid_for_s so
//     volatile-spread drift is honest about the quote's freshness.
//   - Spread: oracle marks are mid-only (MarkView carries no bid/ask),
//     so the spread cost is modeled as notional × spread_bps/10⁴ where
//     spread_bps is the per-side LP distribution markup configured for
//     the instrument (MAX over enabled lp_instrument_configs, the
//     conservative bound), overridable by
//     instruments.param_overrides->'cost_preview'->>'spread_bps'. The
//     basis string on the response names the estimator actually used —
//     "cost_preview_override", "lp_markup_max" or "none" (0 disclosed).
//   - Commission: the account's product-profile pricing plan
//     (settlement.FeeModelSource — SPREAD_MARKUP ⇒ 0, the fee is the
//     markup already counted; RAW_SPREAD_COMMISSION ⇒ the Task 3.3.13
//     tier charge resolved from the account's current monthly volume).
//   - Financing: the latest effective swap point row (Task 3.3.11
//     swap_rates) priced as the one-night InterbankSwapCharge — an
//     indicative per-night accrual, never a forecast. No rate → 0 with
//     a note, not an invented figure.
//   - Conversion: quote→account-base via the funding
//     ConversionRateSource seam; the conversion cost estimate applies
//     funding.DefaultConversionSpreadBps (50bps, Task 11.3.9).
//
// Ex-post (Annual): the per-account year statement aggregates realized
// costs out of the wallet ledger (ledger_entries, migration 102 — the
// account's append-only money-movement book of record; PG remains
// authoritative while the Task 20.3.12 ClickHouse projection catches
// up): FEE credits are explicit fees (narrative-prefixed COMMISSION →
// commissions, CONVERSION*/DUST → conversion), FEE debits are rebates
// received, ROLLOVER credits/debits are swap paid/received. Minor-unit
// (CENT profile) amounts are divided by the profile subunit_divisor —
// ledger.DisplayAmount is the single read-boundary helper (Task
// 3.3.21). Spread cost is the ex-ante estimator applied over the year's
// fills — effective_spread is computed at execution but not persisted,
// so ex-post spread is disclosed as a modeled estimate (estimator note
// on the document). The cumulative-effect illustration is
// cost-as-%-of-gross-traded-notional per currency — the honest,
// reproducible MiFID "effect on return" proxy for a spot book.
//
// Both documents carry the fixed no-inducement declaration (R10: the
// venue has no IB/retrocession flow).
package analytics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/funding"
	"exchange/internal/oracle"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// NoInducementStatement is the fixed disclosure line required on both
// the ex-ante preview response and the ex-post document (task item 3).
const NoInducementStatement = "No third-party inducements paid or received (venue has no IB/retrocession flow per R10)."

// CostPreviewValidForS bounds the quote's honesty window — the spec
// §6.5 oracle staleness gate: past it the mark may have drifted and the
// client must re-preview.
const CostPreviewValidForS = 5

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// CostMarkSource is the live-mark seam — *oracle.Provider satisfies it.
type CostMarkSource interface {
	Mark(ctx context.Context, symbol string) (oracle.MarkView, error)
}

// CostInstrument is the instrument fields the estimators need.
type CostInstrument struct {
	ID            int64
	Symbol        string
	BaseCurrency  string
	QuoteCurrency string
	LotSize       decimal.Decimal
	TickSize      decimal.Decimal
}

// CostInstrumentSource resolves the instrument row.
type CostInstrumentSource interface {
	InstrumentBySymbol(ctx context.Context, symbol string) (*CostInstrument, error)
}

// SpreadBpsSource returns the modeled half-spread in basis points plus
// a basis note naming the estimator ("lp_markup_max" |
// "cost_preview_override" | "none").
type SpreadBpsSource interface {
	SpreadBps(ctx context.Context, instrumentID int64) (decimal.Decimal, string, error)
}

// SwapPointSource is the latest Tom-Next rate seam —
// *settlement.PgSwapRateStore satisfies it.
type SwapPointSource interface {
	LatestSwapRate(ctx context.Context, instrumentID int64, onOrBefore time.Time) (settlement.SwapRate, bool, error)
}

// CostActivitySource aggregates the account's in-window ledger activity
// for the ex-post statement. Amounts are expected in MAJOR units —
// the Pg implementation divides by the profile subunit_divisor.
type CostActivitySource interface {
	ActivityTotals(ctx context.Context, accountID int64, from, to time.Time) ([]CostActivityRow, error)
}

// CostActivityRow is one (entry_type, direction, narrative-class,
// currency) ledger bucket.
type CostActivityRow struct {
	Currency  string
	EntryType string // ledger_entries.entry_type (FEE, ROLLOVER, …)
	Direction string // DEBIT = money in, CREDIT = money out
	Class     string // first narrative token (COMMISSION, CONVERSION, …)
	Total     decimal.Decimal
	Count     int64
}

// TradeFillSource lists the account's in-year fills for the modeled
// ex-post spread cost.
type TradeFillSource interface {
	TradeFills(ctx context.Context, accountID int64, from, to time.Time) ([]CostTradeFill, error)
}

// CostTradeFill is one executed leg for spread modeling.
type CostTradeFill struct {
	InstrumentID int64
	Symbol       string
	Quantity     decimal.Decimal // base units
	Price        decimal.Decimal // quote/base
	QuoteCcy     string
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// CostsDeps wires CostsDisclosureService. Marks, Instruments, FeeModels,
// Commissions and Accounts are required for the ex-ante path; the
// ex-post path additionally needs Activity and Trades. Conv may be nil —
// conversion is then only attempted for same-currency totals and any
// cross-currency request fails closed.
type CostsDeps struct {
	Marks       CostMarkSource
	Instruments CostInstrumentSource
	FeeModels   settlement.FeeModelSource
	Commissions settlement.CommissionStore
	Swap        SwapPointSource
	Spread      SpreadBpsSource
	Accounts    funding.AccountMetaSource
	Conv        funding.ConversionRateSource
	Activity    CostActivitySource
	Trades      TradeFillSource
	// ConversionSpreadBps estimates the conversion cost leg;
	// zero → funding.DefaultConversionSpreadBps (50bps).
	ConversionSpreadBps decimal.Decimal
	Now                 func() time.Time
}

// CostsDisclosureService produces the ex-ante preview and the ex-post
// annual statement.
type CostsDisclosureService struct {
	d   CostsDeps
	now func() time.Time
}

// NewCostsDisclosureService validates wiring — Marks, Instruments,
// FeeModels, Commissions and Accounts are mandatory for Preview;
// Annual additionally requires Activity + Trades (checked at call so a
// preview-only wiring stays legal).
func NewCostsDisclosureService(d CostsDeps) (*CostsDisclosureService, error) {
	if d.Marks == nil || d.Instruments == nil || d.FeeModels == nil ||
		d.Commissions == nil || d.Accounts == nil {
		return nil, fmt.Errorf("costs: marks/instruments/fee-models/commissions/accounts are required")
	}
	if d.Spread == nil {
		d.Spread = StaticSpreadBps{} // 0 with "none" basis — honest
	}
	if d.ConversionSpreadBps.IsZero() {
		d.ConversionSpreadBps = funding.DefaultConversionSpreadBps
	}
	s := &CostsDisclosureService{d: d, now: time.Now}
	if d.Now != nil {
		s.now = d.Now
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Ex-ante preview
// ---------------------------------------------------------------------------

// CostPreview is the GET /api/v1/account/cost-preview response.
type CostPreview struct {
	AccountID    int64             `json:"account_id"`
	Symbol       string            `json:"symbol"`
	Side         string            `json:"side"`
	Quantity     decimal.Decimal   `json:"quantity"`
	QuoteCcy     string            `json:"quote_currency"`
	AccountCcy   string            `json:"account_currency"`
	Mid          decimal.Decimal   `json:"mid"`
	QuotedAt     time.Time         `json:"quoted_at"`
	ValidForS    int               `json:"valid_for_s"`
	FeeModel     string            `json:"fee_model"`
	SpreadCost   decimal.Decimal   `json:"spread_cost"`             // quote ccy
	Commission   decimal.Decimal   `json:"commission"`              // quote ccy
	Financing    decimal.Decimal   `json:"financing"`               // quote ccy, signed (neg = charge)
	Conversion   decimal.Decimal   `json:"conversion"`              // quote ccy estimate
	TotalQuote   decimal.Decimal   `json:"total_quote"`             // Σ quote-ccy costs (positive = cost)
	TotalAccount *decimal.Decimal  `json:"total_account,omitempty"` // converted, when ccy differs
	RateUsed     *decimal.Decimal  `json:"rate_used,omitempty"`     // quote→account mid
	Inducement   string            `json:"inducement_statement"`
	Estimators   map[string]string `json:"estimators"` // per-component honesty note
}

// Preview computes the ex-ante estimate. side is BUY|SELL (the
// client's order side — a BUY pays the ask-side half-spread).
func (s *CostsDisclosureService) Preview(ctx context.Context, accountID int64,
	symbol, side string, qty decimal.Decimal) (*CostPreview, error) {

	side = strings.ToUpper(strings.TrimSpace(side))
	if side != "BUY" && side != "SELL" {
		return nil, excerrors.New("INVALID_REQUEST", "side must be BUY|SELL")
	}
	if !qty.IsPositive() {
		return nil, excerrors.New("INVALID_REQUEST", "quantity must be positive")
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, excerrors.New("INVALID_REQUEST", "symbol required")
	}
	if !strings.Contains(symbol, "/") && len(symbol) == 6 {
		symbol = symbol[:3] + "/" + symbol[3:] // EURUSD → EUR/USD
	}

	inst, err := s.d.Instruments.InstrumentBySymbol(ctx, symbol)
	if err != nil {
		return nil, fmt.Errorf("costs: instrument %s: %w", symbol, err)
	}
	if inst == nil {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("instrument %q not found", symbol))
	}
	mv, err := s.d.Marks.Mark(ctx, inst.Symbol)
	if err != nil {
		return nil, fmt.Errorf("costs: mark read %s: %w", inst.Symbol, err)
	}
	if !mv.Found {
		return nil, excerrors.New("PRICE_ORACLE_UNAVAILABLE",
			fmt.Sprintf("no oracle mark for %s", inst.Symbol))
	}
	if mv.Stale {
		return nil, excerrors.New("MARK_PRICE_STALE",
			fmt.Sprintf("oracle mark for %s is stale", inst.Symbol))
	}
	meta, err := s.d.Accounts.AccountMeta(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("costs: account meta %d: %w", accountID, err)
	}

	notes := map[string]string{}
	notional := qty.Mul(mv.Price) // quote ccy

	// Spread — modeled half-spread over the configured LP markup.
	bps, basis, err := s.d.Spread.SpreadBps(ctx, inst.ID)
	if err != nil {
		return nil, fmt.Errorf("costs: spread estimate %s: %w", inst.Symbol, err)
	}
	notes["spread"] = basis
	spreadCost := notional.Mul(bps).Div(decimal.NewFromInt(10_000)).Round(8)

	// Commission — pricing-plan-dependent.
	model, err := s.d.FeeModels.FeeModel(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("costs: fee model acct %d: %w", accountID, err)
	}
	commission := decimal.Zero
	notes["commission"] = "spread_markup_implicit"
	if model == settlement.FeeModelRawSpreadCommission {
		vol, err := s.d.Commissions.MonthlyVolumeUSD(ctx, accountID, s.now())
		if err != nil {
			return nil, fmt.Errorf("costs: monthly volume acct %d: %w", accountID, err)
		}
		tiers, err := s.d.Commissions.LoadCommissionTiers(ctx)
		if err != nil {
			return nil, fmt.Errorf("costs: commission tiers: %w", err)
		}
		tier, ok := settlement.TierForVolume(tiers, vol)
		if !ok {
			return nil, excerrors.New(settlement.CodeCommissionConfigInvalid,
				"costs: no commission tier resolves for the account volume")
		}
		commission = settlement.CommissionTierCharge(tier, qty, inst.LotSize, notional)
		notes["commission"] = fmt.Sprintf("raw_spread_commission tier=%s", tier.TierName)
	}

	// Financing — one-night indicative Tom-Next accrual.
	financing := decimal.Zero
	notes["financing"] = "no_swap_rate"
	if s.d.Swap != nil {
		rate, ok, err := s.d.Swap.LatestSwapRate(ctx, inst.ID, s.now())
		if err != nil {
			return nil, fmt.Errorf("costs: swap rate %s: %w", inst.Symbol, err)
		}
		if ok {
			points := rate.LongPoints
			posSide := "LONG"
			if side == "SELL" {
				points = rate.ShortPoints
				posSide = "SHORT"
			}
			if c, cerr := settlement.InterbankSwapCharge(qty, inst.LotSize, points, 1); cerr == nil {
				financing = c.Neg() // charge shown as a cost (positive)
				notes["financing"] = fmt.Sprintf(
					"indicative_1night_tomnext side=%s points=%s effective=%s",
					posSide, points, rate.EffectiveDate.Format("2006-01-02"))
			} else {
				notes["financing"] = "swap_rate_unusable"
			}
		}
	}

	// Conversion — quote→account base when they differ.
	conversion := decimal.Zero
	var totalAccount, rateUsed *decimal.Decimal
	acctCcy := meta.BaseCurrency
	if acctCcy == "" {
		acctCcy = inst.QuoteCurrency
	}
	totalQuote := spreadCost.Add(commission).Add(financing)
	if acctCcy != inst.QuoteCurrency {
		if s.d.Conv == nil {
			return nil, excerrors.New("PRICE_ORACLE_UNAVAILABLE",
				fmt.Sprintf("conversion rate %s→%s unavailable (no rate source)",
					inst.QuoteCurrency, acctCcy))
		}
		mid, err := s.d.Conv.MidRate(ctx, inst.QuoteCurrency, acctCcy)
		if err != nil || !mid.Rate.IsPositive() {
			return nil, excerrors.Wrap("PRICE_ORACLE_UNAVAILABLE",
				fmt.Sprintf("conversion mid %s→%s", inst.QuoteCurrency, acctCcy),
				fmt.Errorf("%v", err))
		}
		conversion = totalQuote.Mul(s.d.ConversionSpreadBps).
			Div(decimal.NewFromInt(10_000)).Round(8)
		notes["conversion"] = fmt.Sprintf("mid %s→%s +%sbps spread (rate_src=%s)",
			inst.QuoteCurrency, acctCcy, s.d.ConversionSpreadBps, mid.Source)
		r := mid.Rate
		rateUsed = &r
		ta := totalQuote.Add(conversion).Mul(mid.Rate).Round(8)
		totalAccount = &ta
		totalQuote = totalQuote.Add(conversion)
	} else {
		notes["conversion"] = "same_currency"
		ta := totalQuote
		totalAccount = &ta
	}

	return &CostPreview{
		AccountID: accountID, Symbol: inst.Symbol, Side: side,
		Quantity: qty, QuoteCcy: inst.QuoteCurrency, AccountCcy: acctCcy,
		Mid: mv.Price, QuotedAt: mv.ValidAt.UTC(),
		ValidForS: CostPreviewValidForS, FeeModel: string(model),
		SpreadCost: spreadCost, Commission: commission,
		Financing: financing, Conversion: conversion,
		TotalQuote: totalQuote.Round(8), TotalAccount: totalAccount,
		RateUsed: rateUsed, Inducement: NoInducementStatement,
		Estimators: notes,
	}, nil
}

// ---------------------------------------------------------------------------
// Ex-post annual statement
// ---------------------------------------------------------------------------

// CostBucket aggregates one currency's year totals (major units).
type CostBucket struct {
	Currency       string          `json:"currency"`
	SpreadCost     decimal.Decimal `json:"spread_cost"`     // modeled
	Commissions    decimal.Decimal `json:"commissions"`     // explicit
	OtherFees      decimal.Decimal `json:"other_fees"`      // FEE credits ex-commission
	SwapPaid       decimal.Decimal `json:"swap_paid"`       // ROLLOVER credits
	SwapReceived   decimal.Decimal `json:"swap_received"`   // ROLLOVER debits
	Rebates        decimal.Decimal `json:"rebates"`         // FEE debits (negative cost)
	ConversionCost decimal.Decimal `json:"conversion_cost"` // CONVERSION/DUST fee credits
	Total          decimal.Decimal `json:"total"`           // Σ costs net of rebates
	GrossNotional  decimal.Decimal `json:"gross_notional"`  // year turnover
	// CostPctOfTurnover is the cumulative-effect illustration:
	// total / gross_notional × 100 (0 when nothing traded).
	CostPctOfTurnover decimal.Decimal `json:"cost_pct_of_turnover"`
}

// Disclosure is the annual ex-post costs & charges statement.
type Disclosure struct {
	AccountID   int64        `json:"account_id"`
	Year        int          `json:"year"`
	GeneratedAt time.Time    `json:"generated_at"`
	Buckets     []CostBucket `json:"buckets"`
	Inducement  string       `json:"inducement_statement"`
	Notes       []string     `json:"notes"`
}

// Annual aggregates the year's costs. Reconciliation rule: explicit
// legs (commission/fees/swap/rebate/conversion) reconcile to
// ledger_entries; spread is the configured-markup model applied over
// the year's fills — both facts are stated on the document.
func (s *CostsDisclosureService) Annual(ctx context.Context, accountID int64, year int) (*Disclosure, error) {
	if s.d.Activity == nil || s.d.Trades == nil {
		return nil, fmt.Errorf("costs: annual disclosure requires activity + trades sources")
	}
	if accountID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "account id must be positive")
	}
	now := s.now().UTC()
	if year < 1970 || year > now.Year() {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("year %d out of range", year))
	}
	from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC)
	if to.After(now) {
		to = now // in-progress year — the statement is year-to-date
	}

	rows, err := s.d.Activity.ActivityTotals(ctx, accountID, from, to)
	if err != nil {
		return nil, fmt.Errorf("costs: activity totals acct %d: %w", accountID, err)
	}
	fills, err := s.d.Trades.TradeFills(ctx, accountID, from, to)
	if err != nil {
		return nil, fmt.Errorf("costs: trade fills acct %d: %w", accountID, err)
	}

	buckets := map[string]*CostBucket{}
	bucket := func(ccy string) *CostBucket {
		b, ok := buckets[ccy]
		if !ok {
			b = &CostBucket{Currency: ccy}
			buckets[ccy] = b
		}
		return b
	}
	for _, r := range rows {
		// Classify first — non-cost entry types (TRADE_FILL, DEPOSIT, …)
		// must not materialize an empty bucket.
		var field func(*CostBucket)
		switch {
		case r.EntryType == "ROLLOVER" && r.Direction == "CREDIT":
			field = func(b *CostBucket) { b.SwapPaid = b.SwapPaid.Add(r.Total) }
		case r.EntryType == "ROLLOVER" && r.Direction == "DEBIT":
			field = func(b *CostBucket) { b.SwapReceived = b.SwapReceived.Add(r.Total) }
		case r.EntryType == "FEE" && r.Direction == "DEBIT":
			field = func(b *CostBucket) { b.Rebates = b.Rebates.Add(r.Total) }
		case r.EntryType == "FEE" && r.Direction == "CREDIT" && r.Class == "COMMISSION":
			field = func(b *CostBucket) { b.Commissions = b.Commissions.Add(r.Total) }
		case r.EntryType == "FEE" && r.Direction == "CREDIT" &&
			(r.Class == "CONVERSION" || r.Class == "DUST" || r.Class == "FXCONVERT"):
			field = func(b *CostBucket) { b.ConversionCost = b.ConversionCost.Add(r.Total) }
		case r.EntryType == "FEE" && r.Direction == "CREDIT":
			field = func(b *CostBucket) { b.OtherFees = b.OtherFees.Add(r.Total) }
		}
		if field != nil {
			field(bucket(r.Currency))
		}
	}

	// Modeled spread + turnover from the year's fills.
	seenBps := map[int64]decimal.Decimal{}
	for _, f := range fills {
		b := bucket(f.QuoteCcy)
		bps, ok := seenBps[f.InstrumentID]
		if !ok {
			var basis string
			bps, basis, err = s.d.Spread.SpreadBps(ctx, f.InstrumentID)
			if err != nil {
				return nil, fmt.Errorf("costs: spread estimate %s: %w", f.Symbol, err)
			}
			_ = basis
			seenBps[f.InstrumentID] = bps
		}
		notional := f.Quantity.Mul(f.Price)
		b.GrossNotional = b.GrossNotional.Add(notional)
		b.SpreadCost = b.SpreadCost.Add(
			notional.Mul(bps).Div(decimal.NewFromInt(10_000))).Round(8)
	}

	out := &Disclosure{
		AccountID: accountID, Year: year, GeneratedAt: now,
		Inducement: NoInducementStatement,
		Notes: []string{
			"explicit costs reconcile to ledger_entries (PG book of record); " +
				"spread cost is modeled at the configured LP distribution markup " +
				"(effective_spread is computed at execution but not persisted)",
			"cumulative effect = total costs as % of gross traded notional, per currency",
			"amounts are major units — CENT profiles are divisor-adjusted at read",
			"retained 7 years per the reporting retention schedule",
		},
	}
	for _, b := range buckets {
		b.Total = b.SpreadCost.Add(b.Commissions).Add(b.OtherFees).
			Add(b.SwapPaid).Add(b.ConversionCost).Sub(b.Rebates).Round(8)
		if b.GrossNotional.IsPositive() {
			b.CostPctOfTurnover = b.Total.Div(b.GrossNotional).
				Mul(decimal.NewFromInt(100)).Round(6)
		}
		out.Buckets = append(out.Buckets, *b)
	}
	sort.Slice(out.Buckets, func(i, j int) bool {
		return out.Buckets[i].Currency < out.Buckets[j].Currency
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// Pg implementations
// ---------------------------------------------------------------------------

// PgCostInstrumentSource resolves instruments by symbol (accepts both
// "EUR/USD" and "EURUSD" input via a normalized lookup).
type PgCostInstrumentSource struct{ Pool *pgxpool.Pool }

// NewPgCostInstrumentSource binds the pool.
func NewPgCostInstrumentSource(pool *pgxpool.Pool) *PgCostInstrumentSource {
	return &PgCostInstrumentSource{Pool: pool}
}

// InstrumentBySymbol implements CostInstrumentSource; (nil, nil) when
// the symbol is unknown.
func (s *PgCostInstrumentSource) InstrumentBySymbol(ctx context.Context, symbol string) (*CostInstrument, error) {
	var i CostInstrument
	var lot, tick string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, symbol, base_currency, quote_currency,
		       lot_size::text, tick_size::text
		  FROM instruments WHERE symbol = $1`, symbol).
		Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency, &lot, &tick)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var errD error
	if i.LotSize, errD = decimal.NewFromString(lot); errD != nil {
		return nil, fmt.Errorf("instrument %s lot_size %q: %w", i.Symbol, lot, errD)
	}
	if i.TickSize, errD = decimal.NewFromString(tick); errD != nil {
		return nil, fmt.Errorf("instrument %s tick_size %q: %w", i.Symbol, tick, errD)
	}
	return &i, nil
}

// PgSpreadBpsSource models the client half-spread as the maximum
// configured per-side LP distribution markup on the instrument, unless
// instruments.param_overrides->'cost_preview'->>'spread_bps' pins an
// explicit estimate.
type PgSpreadBpsSource struct{ Pool *pgxpool.Pool }

// NewPgSpreadBpsSource binds the pool.
func NewPgSpreadBpsSource(pool *pgxpool.Pool) *PgSpreadBpsSource {
	return &PgSpreadBpsSource{Pool: pool}
}

// SpreadBps implements SpreadBpsSource.
func (s *PgSpreadBpsSource) SpreadBps(ctx context.Context, instrumentID int64) (decimal.Decimal, string, error) {
	var override *string
	if err := s.Pool.QueryRow(ctx, `
		SELECT param_overrides->'cost_preview'->>'spread_bps'
		  FROM instruments WHERE id = $1`, instrumentID).Scan(&override); err != nil {
		return decimal.Zero, "", fmt.Errorf("spread override read: %w", err)
	}
	if override != nil && *override != "" {
		d, err := decimal.NewFromString(*override)
		if err != nil || d.IsNegative() {
			return decimal.Zero, "", fmt.Errorf("spread override %q invalid", *override)
		}
		return d, "cost_preview_override", nil
	}
	var mx *string
	if err := s.Pool.QueryRow(ctx, `
		SELECT MAX(GREATEST(spread_markup_ask_bps, spread_markup_bid_bps))::text
		  FROM lp_instrument_configs
		 WHERE instrument_id = $1 AND enabled`, instrumentID).Scan(&mx); err != nil {
		return decimal.Zero, "", fmt.Errorf("lp markup read: %w", err)
	}
	if mx == nil {
		return decimal.Zero, "none", nil
	}
	d, err := decimal.NewFromString(*mx)
	if err != nil {
		return decimal.Zero, "", fmt.Errorf("lp markup %q invalid", *mx)
	}
	return d, "lp_markup_max", nil
}

// StaticSpreadBps is the no-source estimator: 0 bps with the "none"
// basis note — never a fabricated spread.
type StaticSpreadBps struct{}

// SpreadBps implements SpreadBpsSource.
func (StaticSpreadBps) SpreadBps(context.Context, int64) (decimal.Decimal, string, error) {
	return decimal.Zero, "none", nil
}

// PgCostActivitySource aggregates ledger_entries for the ex-post
// statement. Narrative class is the first whitespace-delimited token of
// the description (the posting engines prefix narratives with the
// posting kind — "COMMISSION trade=…", "CONVERSION …", "DUST …").
// Amounts are divisor-adjusted for CENT profiles via
// account_product_profiles.subunit_divisor (Task 3.3.21 read boundary).
type PgCostActivitySource struct{ Pool *pgxpool.Pool }

// NewPgCostActivitySource binds the pool.
func NewPgCostActivitySource(pool *pgxpool.Pool) *PgCostActivitySource {
	return &PgCostActivitySource{Pool: pool}
}

// ActivityTotals implements CostActivitySource.
func (s *PgCostActivitySource) ActivityTotals(ctx context.Context, accountID int64,
	from, to time.Time) ([]CostActivityRow, error) {

	rows, err := s.Pool.Query(ctx, `
		SELECT le.currency, le.entry_type::text, le.direction::text,
		       COALESCE(split_part(COALESCE(le.description,''), ' ', 1), ''),
		       (SUM(le.amount) / COALESCE(pp.subunit_divisor, 1))::text,
		       COUNT(*)
		  FROM ledger_entries le
		  JOIN accounts a ON a.id = le.account_id
		  LEFT JOIN account_product_profiles pp ON pp.profile_id = a.product_profile_id
		 WHERE le.account_id = $1 AND le.posted_at >= $2 AND le.posted_at < $3
		 GROUP BY le.currency, le.entry_type, le.direction,
		          split_part(COALESCE(le.description,''), ' ', 1)`,
		accountID, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CostActivityRow{}
	for rows.Next() {
		var r CostActivityRow
		var total string
		if err := rows.Scan(&r.Currency, &r.EntryType, &r.Direction,
			&r.Class, &total, &r.Count); err != nil {
			return nil, err
		}
		var err error
		if r.Total, err = decimal.NewFromString(total); err != nil {
			return nil, fmt.Errorf("activity total %q: %w", total, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PgTradeFillSource lists the account's in-window fills for spread
// modeling (same trade-leg projection as the tax fill source).
type PgTradeFillSource struct{ Pool *pgxpool.Pool }

// NewPgTradeFillSource binds the pool.
func NewPgTradeFillSource(pool *pgxpool.Pool) *PgTradeFillSource {
	return &PgTradeFillSource{Pool: pool}
}

// TradeFills implements TradeFillSource.
func (s *PgTradeFillSource) TradeFills(ctx context.Context, accountID int64,
	from, to time.Time) ([]CostTradeFill, error) {

	rows, err := s.Pool.Query(ctx, `
		SELECT t.instrument_id, i.symbol, t.quantity::text, t.price::text,
		       i.quote_currency
		  FROM trades t
		  JOIN instruments i ON i.id = t.instrument_id
		 WHERE (t.buyer_account_id = $1 OR t.seller_account_id = $1)
		   AND t.created_at >= $2 AND t.created_at < $3`,
		accountID, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CostTradeFill{}
	for rows.Next() {
		var f CostTradeFill
		var qty, px string
		if err := rows.Scan(&f.InstrumentID, &f.Symbol, &qty, &px, &f.QuoteCcy); err != nil {
			return nil, err
		}
		var err error
		if f.Quantity, err = decimal.NewFromString(qty); err != nil {
			return nil, fmt.Errorf("fill qty %q: %w", qty, err)
		}
		if f.Price, err = decimal.NewFromString(px); err != nil {
			return nil, fmt.Errorf("fill price %q: %w", px, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
