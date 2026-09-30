// Phase-19 Task 19.3.1 — CROSS / ISOLATED / PORTFOLIO margin modes.
//
// The MarginService computes the canonical account margin snapshot
// (equity / used_margin / margin_level_pct / status) in the account's
// USD numeraire (spec §13.1 "USD Numeraire Normalization"):
//
//   - every position's required margin and unrealized P&L is denominated
//     in the instrument's QUOTE currency and MUST be converted to USD at
//     the current mark before summation — heterogeneous currencies are
//     never added directly;
//   - all marks are batch-fetched from Redis via ONE MGET (MarkCache) —
//     per-position lookups are an N+1 defect under spec §13.1;
//   - PORTFOLIO mode applies the §13.6g correlation offset through the
//     injected CorrelationProvider seam (cluster F's 90-day matrix
//     supplies it; the default NoCorrelation yields zero offsets);
//   - ISOLATED mode aggregates positions.isolated_margin_allocated
//     (migration 106, Task 19.3.27) — the account-level liquidation
//     trigger does not apply; per-position liquidation is the
//     isolated-margin engine's job.
//
// Fail-closed (spec §2.7): a required-margin conversion that cannot be
// valued in USD aborts the whole evaluation (PRICE_ORACLE_UNAVAILABLE).
// Balance/upnl legs in currencies with no listed USD pair contribute 0
// to equity — conservative under-valuation — and are disclosed in the
// snapshot's Unvalued list (never silently dropped).
package risk

import (
	"context"
	"fmt"
	"sort"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// MarginMode is the margin_mode_enum domain (migration 013).
type MarginMode string

const (
	ModeIsolated  MarginMode = "ISOLATED"
	ModeCross     MarginMode = "CROSS"
	ModePortfolio MarginMode = "PORTFOLIO"
)

// ParseMarginMode normalizes a request token ("cross", "CROSS", …) into
// the enum; ok=false for anything outside the canonical set.
func ParseMarginMode(s string) (MarginMode, bool) { return normalizeMode(s) }

// ---------------------------------------------------------------------------
// Thresholds — the margin:level status machine mirrors the sibling
// lifecycle exactly (margin_call.go): the §13.3 episode trigger is the
// canonical MarginCallThresholdPct (111.1%) for every tier and stop-out
// is per-tier (§13.6d: retail 50 / professional 30 / ECP 100). The
// §13.6d display tiers (retail warning 120, professional call 80) feed
// the account_margin_thresholds REST surface — cluster D's seam —
// and are resolved here for snapshot consumers.
// ---------------------------------------------------------------------------

// MarginThresholds are percentage points of margin_level_pct (the
// §13.6d tier set for display + order-block context).
type MarginThresholds struct {
	Warning decimal.Decimal // UI highlight
	Call    decimal.Decimal // order block + deposit window opens
	StopOut decimal.Decimal // mandatory liquidation trigger
}

var (
	// ThresholdsRetail — ESMA retail (spec §13.6d): 120/100/50.
	ThresholdsRetail = MarginThresholds{
		Warning: decimal.NewFromInt(120),
		Call:    decimal.NewFromInt(100),
		StopOut: StopOutRetailPct,
	}
	// ThresholdsProfessional — spec §13.6d: 100/80/30.
	ThresholdsProfessional = MarginThresholds{
		Warning: decimal.NewFromInt(100),
		Call:    decimal.NewFromInt(80),
		StopOut: StopOutProfessionalPct,
	}
	// ThresholdsInstitutional — institutional fallback (§13.3
	// 0.90-utilization bound as the call, stop-out 100%).
	ThresholdsInstitutional = MarginThresholds{
		Warning: decimal.NewFromInt(120),
		Call:    MarginCallThresholdPct,
		StopOut: StopOutInstitutionalPct,
	}
)

// ThresholdsFor resolves the tier set from accounts.client_category.
// Unknown/empty categories fail closed to the retail (strictest) set.
func ThresholdsFor(clientCategory string) MarginThresholds {
	switch clientCategory {
	case "PROFESSIONAL":
		return ThresholdsProfessional
	case "ELIGIBLE_COUNTERPARTY":
		return ThresholdsInstitutional
	default:
		return ThresholdsRetail
	}
}

// AccountThresholdsSource is the optional migration-235 per-account
// override seam — cluster D's MarginThresholdService binds here when it
// lands; unbound falls back to ThresholdsFor(category). Errors fail
// closed to the category default would be UNSAFE (a stricter override
// would be silently lost) so an error propagates.
type AccountThresholdsSource interface {
	Thresholds(ctx context.Context, accountID int64) (MarginThresholds, error)
}

// Error codes emitted by the margin files — all §23-registered.
const (
	CodeInvalidRequest        = "INVALID_REQUEST"            // §23 400
	CodeMarginInsufficient    = "MARGIN_INSUFFICIENT"        // §23 400
	CodeIsolatedMarginDeficit = "ISOLATED_MARGIN_DEFICIT"    // §23 409
	CodeMarginCallExceeded    = "MARGIN_CALL_EXCEEDED"       // §23 409
	CodeMarginModeBlocked     = "MARGIN_MODE_SWITCH_BLOCKED" // §23 409
	CodeOracleUnavailable     = "PRICE_ORACLE_UNAVAILABLE"   // §23 503
)

// ---------------------------------------------------------------------------
// CorrelationProvider — the Task 19.3.18 / cluster-F seam
// ---------------------------------------------------------------------------

// CorrelationProvider supplies the 90-day rolling correlation matrix
// (spec §13.6g). Offset returns the correlation between instruments a
// and b expressed in basis points: ρ × 10⁴ (e.g. ρ = −0.82 → −8200).
// ok=false means no estimate — treated as ρ = 0 (no offset).
//
// The engine multiplies by the position-side product internally: a
// hedge exists only when the *positions'* returns are negatively
// correlated (|ρ_positions| > 0.70 gate per §13.6g).
type CorrelationProvider interface {
	Offset(a, b string) (float64, bool)
}

// CorrelationFunc adapts a function to CorrelationProvider.
type CorrelationFunc func(a, b string) (float64, bool)

// Offset implements CorrelationProvider.
func (f CorrelationFunc) Offset(a, b string) (float64, bool) { return f(a, b) }

// NoCorrelation is the unbound default — zero offsets, CROSS-equivalent
// margin. CorrelationMatrix (correlation_offset.go, Task 19.3.18) is the
// production binding.
type NoCorrelation struct{}

// Offset implements CorrelationProvider.
func (NoCorrelation) Offset(_, _ string) (float64, bool) { return 0, false }

// CorrelationFactorProvider is the OPTIONAL per-pair offset-factor seam
// (Task 19.3.18 "offset_factor configurable per group, default 0.5, max
// 0.8"): a provider that also implements it overrides the §13.6g
// default factor for that pair; values are clamped to (0, 0.8].
type CorrelationFactorProvider interface {
	OffsetFactor(a, b string) (float64, bool)
}

// §13.6g constants: |ρ| > 0.70 gate, default offset factor 0.5, offset
// credit cap 0.8 of gross IM, regulatory floor 20% of gross IM.
var (
	corrGateBps    = int64(7000)
	offsetFactor   = decimal.RequireFromString("0.5")
	offsetCapPct   = decimal.RequireFromString("0.8")
	offsetFloorPct = decimal.RequireFromString("0.2")
)

// ---------------------------------------------------------------------------
// Store seam + row types
// ---------------------------------------------------------------------------

// MarginAccount mirrors one margin_accounts row (migration 013).
type MarginAccount struct {
	AccountID  int64
	Mode       MarginMode
	Status     string // NORMAL | MARGIN_CALL | LIQUIDATING
	Equity     decimal.Decimal
	UsedMargin decimal.Decimal
}

// BalanceAmount is one balances row needed for equity.
type BalanceAmount struct {
	Currency  string
	Available decimal.Decimal
	Locked    decimal.Decimal
}

// Total returns available + locked (the generated `total` column).
func (b BalanceAmount) Total() decimal.Decimal { return b.Available.Add(b.Locked) }

// MarginPosition is one open position joined to its instrument.
// UnrealizedPnL/MarginUsed are quote-currency values from the positions
// row; IsolatedAllocated is the account-base-currency collateral locked
// to this leg (migration 106).
type MarginPosition struct {
	ID                int64
	InstrumentID      int64
	Symbol            string
	Side              string // LONG | SHORT
	Quantity          decimal.Decimal
	EntryPrice        decimal.Decimal
	StoredMark        *decimal.Decimal
	MarginUsed        decimal.Decimal // required margin, QUOTE currency (0 = derive)
	IsolatedAllocated decimal.Decimal // base currency (migration 106)
	AutoReplenish     bool
	BaseCurrency      string
	QuoteCurrency     string
	MaxLeverage       int64
}

// PositionMarginEval is one position's evaluated contribution (all USD).
type PositionMarginEval struct {
	PositionID    int64  `json:"position_id"`
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Quantity      string `json:"quantity"`
	Mark          string `json:"mark"`
	MarkSource    string `json:"mark_source"`
	UnrealizedUSD string `json:"unrealized_pnl_usd"`
	MarginUSD     string `json:"margin_usd"`
}

// FxPair is a resolved currency→USD conversion instrument.
// Inverted=false: symbol = {CCY}/USD, rate = mark (USD per 1 CCY).
// Inverted=true:  symbol = USD/{CCY}, rate = 1/mark.
type FxPair struct {
	Symbol   string
	Inverted bool
}

// OpenInterestSource resolves an instrument's aggregate open notional
// (same units as quantity×mark — the liquidation auction denominator)
// for the §13.12 concentration-share add-on. *PgLiquidationStore and
// *PgMarginStore satisfy it. A nil source or a non-positive read takes
// ConcentrationAddon's pessimistic leg (the position is treated as the
// whole book).
type OpenInterestSource interface {
	OpenInterest(ctx context.Context, instrumentID int64) (decimal.Decimal, error)
}

// ADVSource resolves an instrument's average daily traded notional
// (quote ccy) — the §13.12 liquidity add-on's unwind-days denominator
// and the §13.4a/6a liquidation slicing yardstick. A nil source,
// lookup error, or non-positive value means "ADV unknown" →
// LiquidityAddon's 50% cap / no slicing split (§2.7 pessimism lives in
// the margin charge, never in delaying risk reduction).
type ADVSource interface {
	ADV(ctx context.Context, instrumentID int64) (decimal.Decimal, error)
}

// ADVFunc adapts a lookup function to ADVSource.
type ADVFunc func(ctx context.Context, instrumentID int64) (decimal.Decimal, error)

// ADV implements ADVSource.
func (f ADVFunc) ADV(ctx context.Context, instrumentID int64) (decimal.Decimal, error) {
	return f(ctx, instrumentID)
}

// MarginStore is the persistence seam; MarginPgStore implements it over
// pgx — tests substitute fakes.
type MarginStore interface {
	// MarginAccount returns the margin_accounts row or nil when absent.
	MarginAccount(ctx context.Context, accountID int64) (*MarginAccount, error)
	// SetMarginMode upserts margin_accounts.margin_mode for the account.
	SetMarginMode(ctx context.Context, accountID int64, mode MarginMode) error
	// OpenPositionCount counts quantity<>0 positions.
	OpenPositionCount(ctx context.Context, accountID int64) (int64, error)
	// AccountCategory returns accounts.client_category ('RETAIL' default).
	AccountCategory(ctx context.Context, accountID int64) (string, error)
	// Balances returns every currency balance row.
	Balances(ctx context.Context, accountID int64) ([]BalanceAmount, error)
	// MarginPositions returns open positions joined to instruments.
	MarginPositions(ctx context.Context, accountID int64) ([]MarginPosition, error)
	// FxPairInstruments resolves, for each currency in ccys, the ACTIVE
	// SPOT instrument used for the USD rate: {CCY}/USD direct or
	// USD/{CCY} inverse. Currencies with no coverage are absent.
	FxPairInstruments(ctx context.Context, ccys []string) (map[string]FxPair, error)
	// OpenPositionIndex returns instrument symbol → account ids holding a
	// non-zero position (the engine's tick→account fan-out index).
	OpenPositionIndex(ctx context.Context) (map[string][]int64, error)
	// WriteMarginSnapshot persists the evaluation to margin_accounts
	// (status transitions only — the engine calls this on change).
	WriteMarginSnapshot(ctx context.Context, snap MarginSnapshot) error
}

// ---------------------------------------------------------------------------
// MarginService — mode management + the canonical evaluation
// ---------------------------------------------------------------------------

// MarginService computes account margin snapshots and owns the
// margin_accounts.mode lifecycle (Task 19.3.1 endpoint).
type MarginService struct {
	store      MarginStore
	marks      MarkCache
	corr       CorrelationProvider
	collateral CollateralValuator
	vol        IMMultiplierSource
	oi         OpenInterestSource
	adv        ADVSource
	now        func() time.Time
}

// MarginOptions wires the service; Store and Marks are required
// (fail-closed — a margin service without reads must not exist).
type MarginOptions struct {
	Store       MarginStore
	Marks       MarkCache
	Correlation CorrelationProvider // nil ⇒ NoCorrelation (zero offsets)
	// Collateral — Task 19.3.8 §13.6b haircut/concentration valuation.
	// nil keeps the pre-task face-value balance leg as a transition
	// shim; PRODUCTION WIRING MUST bind CollateralService — face value
	// is not a compliant configuration.
	Collateral CollateralValuator
	// Volatility — Task 19.3.28 §13.6e IM scaler. nil ⇒ ×1.0 (neutral).
	Volatility IMMultiplierSource
	// OI / ADV — the §13.12 concentration + liquidity add-on
	// denominators (beyond §13.6f tiered leverage). Both optional; the
	// pessimistic leg applies when absent (§2.7): zero OI ⇒ the
	// position is treated as the whole book, unknown ADV ⇒ the 50%
	// liquidity cap. Add-ons ride on top of the mode aggregation —
	// correlation offsets never erode them.
	OI  OpenInterestSource
	ADV ADVSource
	Now func() time.Time
}

// NewMarginService builds the service.
func NewMarginService(o MarginOptions) (*MarginService, error) {
	if o.Store == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "margin: store is nil")
	}
	if o.Marks == nil {
		return nil, excerrors.New(CodeRiskLimitsInternal, "margin: mark cache is nil")
	}
	corr := o.Correlation
	if corr == nil {
		corr = NoCorrelation{}
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &MarginService{store: o.Store, marks: o.Marks, corr: corr,
		collateral: o.Collateral, vol: o.Volatility,
		oi: o.OI, adv: o.ADV, now: now}, nil
}

// ModeFor resolves the account's effective mode: the margin_accounts row
// when present, else the §13.1 default — PORTFOLIO for institutional
// (PROFESSIONAL / ELIGIBLE_COUNTERPARTY), CROSS for RETAIL.
func (s *MarginService) ModeFor(ctx context.Context, accountID int64) (MarginMode, error) {
	row, err := s.store.MarginAccount(ctx, accountID)
	if err != nil {
		return "", errCode(CodeRiskLimitsInternal, "margin account", err)
	}
	if row != nil {
		return row.Mode, nil
	}
	cat, err := s.store.AccountCategory(ctx, accountID)
	if err != nil {
		return "", errCode(CodeRiskLimitsInternal, "account category", err)
	}
	return DefaultMode(cat), nil
}

// DefaultMode is the §13.1 Task-19.3.1 default-mode rule.
func DefaultMode(clientCategory string) MarginMode {
	switch clientCategory {
	case "PROFESSIONAL", "ELIGIBLE_COUNTERPARTY":
		return ModePortfolio
	default:
		return ModeCross
	}
}

// SetMode implements POST /api/v1/account/margin-mode. The mode change
// requires zero open positions (Task 19.3.1 DoD) — any open position
// rejects with MARGIN_MODE_SWITCH_BLOCKED (409). The first change on an
// account without a margin row materializes the row.
func (s *MarginService) SetMode(ctx context.Context, accountID int64, requested string) (*MarginAccount, error) {
	mode, ok := normalizeMode(requested)
	if !ok {
		return nil, excerrors.New(CodeInvalidRequest,
			fmt.Sprintf("margin-mode: invalid mode %q — want CROSS|ISOLATED|PORTFOLIO", requested))
	}
	cur, err := s.ModeFor(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if mode == cur {
		return &MarginAccount{AccountID: accountID, Mode: mode}, nil // idempotent no-op
	}
	n, err := s.store.OpenPositionCount(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "open position count", err)
	}
	if n > 0 {
		return nil, excerrors.New(CodeMarginModeBlocked,
			fmt.Sprintf("margin-mode: %d open position(s) — close all positions before switching %s → %s",
				n, cur, mode))
	}
	if err := s.store.SetMarginMode(ctx, accountID, mode); err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "set margin mode", err)
	}
	return &MarginAccount{AccountID: accountID, Mode: mode}, nil
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

// MarginSnapshot is one evaluation result — the payload written to the
// margin:level:{account} HASH and to margin_accounts on status change.
type MarginSnapshot struct {
	AccountID       int64
	Mode            MarginMode
	ClientCategory  string
	Equity          decimal.Decimal  // USD
	UsedMargin      decimal.Decimal  // USD
	AvailableMargin decimal.Decimal  // USD (equity − used)
	LevelPct        *decimal.Decimal // nil ⇒ ∞ (used_margin == 0)
	Status          string           // NORMAL | MARGIN_CALL | LIQUIDATING
	Unvalued        []string         // currencies excluded from equity
	Concentrated    []string         // Task 19.3.8: currencies whose §13.6b concentration excess was zero-weighted
	Positions       []PositionMarginEval
	Ts              time.Time
}

// Evaluate computes the account's margin snapshot at current marks —
// the canonical evaluation the event-driven engine, the liquidation
// scanner and the REST view all share.
func (s *MarginService) Evaluate(ctx context.Context, accountID int64) (*MarginSnapshot, error) {
	return s.evaluate(ctx, accountID, nil)
}

// evaluate is the core pass; overlay supplies freshest tick marks that
// supersede the Redis MGET (the engine's PushMark path — the mark that
// triggered the tick must not be lost to a stale cache read).
func (s *MarginService) evaluate(ctx context.Context, accountID int64,
	overlay map[string]decimal.Decimal) (*MarginSnapshot, error) {

	acct, err := s.store.MarginAccount(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "margin account", err)
	}
	cat, err := s.store.AccountCategory(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "account category", err)
	}
	mode := DefaultMode(cat)
	if acct != nil {
		mode = acct.Mode
	}
	balances, err := s.store.Balances(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "balances", err)
	}
	positions, err := s.store.MarginPositions(ctx, accountID)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "positions", err)
	}

	// ---- conversion instruments (ONE query for all currencies) ----
	ccys := map[string]bool{}
	for _, b := range balances {
		ccys[b.Currency] = true
	}
	for _, p := range positions {
		ccys[p.QuoteCurrency] = true
	}
	delete(ccys, "USD")
	ccyList := sortedKeys(ccys)
	pairs, err := s.store.FxPairInstruments(ctx, ccyList)
	if err != nil {
		return nil, errCode(CodeRiskLimitsInternal, "fx pair instruments", err)
	}

	// ---- batch marks: position symbols + conversion pair symbols, ONE
	// MGET (spec §13.1 zero-N+1) ----
	syms := map[string]bool{}
	for _, p := range positions {
		syms[p.Symbol] = true
	}
	for _, fp := range pairs {
		syms[fp.Symbol] = true
	}
	marks, err := s.marks.BatchMarks(ctx, sortedKeys(syms))
	if err != nil {
		return nil, errCode(CodeOracleUnavailable, "mark batch fetch", err)
	}
	for k, v := range overlay {
		if v.IsPositive() {
			marks[k] = v
		}
	}

	// rateToUSD converts one unit of ccy to USD at the resolved pair
	// mark; ok=false when no instrument covers the currency or the mark
	// is absent.
	rateToUSD := func(ccy string) (decimal.Decimal, bool) {
		if ccy == "USD" {
			return decimal.NewFromInt(1), true
		}
		fp, ok := pairs[ccy]
		if !ok {
			return decimal.Zero, false
		}
		m, ok := marks[fp.Symbol]
		if !ok || !m.IsPositive() {
			return decimal.Zero, false
		}
		if fp.Inverted {
			return decimal.NewFromInt(1).Div(m), true
		}
		return m, true
	}

	snap := &MarginSnapshot{
		AccountID:      accountID,
		Mode:           mode,
		ClientCategory: cat,
		Ts:             s.now().UTC(),
	}

	// Equity leg 1 — balances. Unconvertible currencies contribute 0 and
	// are disclosed (conservative under-valuation, §2.7 pessimism for the
	// equity side). When the Task-19.3.8 collateral valuator is bound it
	// REPLACES the face-value leg with the §13.6b haircut/concentration-
	// adjusted valuation; the nil seam keeps legacy face value.
	unv := map[string]bool{}
	if s.collateral != nil {
		val, err := s.collateral.Valuate(ctx, balances, rateToUSD)
		if err != nil {
			return nil, errCode(CodeRiskLimitsInternal, "collateral valuation", err)
		}
		snap.Equity = snap.Equity.Add(val.EquityUSD)
		for _, c := range val.Unpriced {
			unv[c] = true
		}
		for _, c := range val.Ineligible {
			unv[c] = true
		}
		snap.Concentrated = val.Concentrated
	} else {
		for _, b := range balances {
			r, ok := rateToUSD(b.Currency)
			if !ok {
				unv[b.Currency] = true
				continue
			}
			snap.Equity = snap.Equity.Add(b.Total().Mul(r))
		}
	}

	// ---- per-position marks + quote→USD conversion ----
	evals := make([]posEval, 0, len(positions))
	for _, p := range positions {
		mark, src := p.StoredMark, MarkSourceStoredMark
		if m, ok := marks[p.Symbol]; ok && m.IsPositive() {
			mark, src = &m, MarkSourceOracle
		}
		if mark == nil || !mark.IsPositive() {
			mark, src = &p.EntryPrice, MarkSourceEntry
		}
		rate, ok := rateToUSD(p.QuoteCurrency)
		if !ok {
			// Required-margin conversion is fail-closed (§2.7): an
			// unpriced obligation can never be silently understated.
			return nil, excerrors.New(CodeOracleUnavailable,
				fmt.Sprintf("margin: no USD rate for position %d quote currency %s",
					p.ID, p.QuoteCurrency))
		}
		signed := p.Quantity
		if p.Side == "SHORT" {
			signed = signed.Neg()
		}
		upnlUSD := signed.Mul(mark.Sub(p.EntryPrice)).Mul(rate).Round(8)
		// Required margin: the positions.margin_used column (written by
		// the engine-side pre-trade checker at open) is authoritative;
		// when absent/0 the requirement is derived as notional/leverage.
		req := p.MarginUsed
		if !req.IsPositive() {
			lev := decimal.NewFromInt(p.MaxLeverage)
			if p.MaxLeverage <= 0 {
				lev = decimal.NewFromInt(1)
			}
			req = p.Quantity.Mul(*mark).Div(lev)
		}
		// Task 19.3.28 §13.6e volatility scaling: the bound scaler lifts
		// required margin ≤×1.5 when realized vol breaches 2× baseline —
		// applied to the effective requirement whichever leg supplied it
		// (stored margin_used or derived notional/leverage).
		if s.vol != nil {
			if mult := s.vol.IMMultiplier(p.Symbol); mult > 1.0 {
				req = req.Mul(decimal.NewFromFloat(mult))
			}
		}
		marginUSD := req.Mul(rate).Round(8)
		// §13.12 add-ons beyond the tiered band: concentration share of
		// instrument open interest + liquidity unwind days vs ADV, both
		// charged on the position's base requirement. Each add-on applies
		// only when its source is provisioned (nil seam = feature off,
		// matching corr/vol/coll conventions); a provisioned source that
		// errors or returns missing/non-positive data takes the helper's
		// pessimistic leg (§2.7) — OI 0 reads as the whole book, ADV 0
		// caps at 50%.
		notional := p.Quantity.Abs().Mul(*mark) // quote ccy
		var addon decimal.Decimal
		if s.oi != nil {
			var oi decimal.Decimal
			if v, oerr := s.oi.OpenInterest(ctx, p.InstrumentID); oerr == nil {
				oi = v
			}
			addon = addon.Add(ConcentrationAddon(notional, oi, marginUSD))
		}
		if s.adv != nil {
			var adv decimal.Decimal
			if v, aerr := s.adv.ADV(ctx, p.InstrumentID); aerr == nil {
				adv = v
			}
			addon = addon.Add(LiquidityAddon(notional, adv, marginUSD))
		}
		addon = addon.Round(8)
		ev := posEval{p: p, mark: *mark, src: src,
			upnlUSD: upnlUSD, marginUSD: marginUSD, addonUSD: addon}
		snap.Equity = snap.Equity.Add(upnlUSD)
		evals = append(evals, ev)
	}

	// ---- used_margin per mode (USD numeraire) ----
	var used decimal.Decimal
	switch mode {
	case ModeIsolated:
		// ISOLATED: the account-level "used margin" surface is the sum of
		// per-position allocated collateral (already base currency —
		// accounts.base_currency; USD normalization applied when the
		// account base is not USD).
		baseRate := decimal.NewFromInt(1)
		baseCcy, berr := s.baseCurrency(ctx, accountID)
		if berr == nil && baseCcy != "" && baseCcy != "USD" {
			r, ok := rateToUSD(baseCcy)
			if !ok {
				return nil, excerrors.New(CodeOracleUnavailable,
					fmt.Sprintf("margin: no USD rate for base currency %s", baseCcy))
			}
			baseRate = r
		}
		for _, e := range evals {
			used = used.Add(e.p.IsolatedAllocated.Mul(baseRate))
		}
	case ModePortfolio:
		used = portfolioMargin(evals, s.corr)
	default: // CROSS
		for _, e := range evals {
			used = used.Add(e.marginUSD)
		}
	}
	// §13.12: shared-pool modes carry the add-ons on top of the mode
	// aggregation — correlation offsets (PORTFOLIO) never erode them.
	if mode != ModeIsolated {
		for _, e := range evals {
			used = used.Add(e.addonUSD)
		}
	}
	snap.UsedMargin = used.Round(8)
	snap.AvailableMargin = snap.Equity.Sub(snap.UsedMargin)

	if snap.UsedMargin.IsPositive() {
		lvl := snap.Equity.Div(snap.UsedMargin).Mul(decimal.NewFromInt(100))
		snap.LevelPct = &lvl
	}

	// Status mirrors the sibling MarginCallService lifecycle exactly so
	// the hash's `status` field and the lifecycle never disagree:
	// LIQUIDATING at-or-below the tier stop-out, MARGIN_CALL at-or-below
	// the canonical §13.3 111.1% trigger, else NORMAL. ISOLATED accounts
	// still publish a headline level for display but their liquidation
	// trigger is per-position (Task 19.3.27) — the aggregate status caps
	// at MARGIN_CALL for them.
	stopOut := ThresholdsFor(cat).StopOut
	switch {
	case snap.LevelPct == nil:
		snap.Status = "NORMAL"
	case snap.LevelPct.LessThanOrEqual(stopOut) && mode != ModeIsolated:
		snap.Status = "LIQUIDATING"
	case snap.LevelPct.LessThanOrEqual(MarginCallThresholdPct):
		snap.Status = "MARGIN_CALL"
	default:
		snap.Status = "NORMAL"
	}

	snap.Unvalued = sortedKeys(unv)
	for _, e := range evals {
		snap.Positions = append(snap.Positions, PositionMarginEval{
			PositionID:    e.p.ID,
			Symbol:        e.p.Symbol,
			Side:          e.p.Side,
			Quantity:      e.p.Quantity.String(),
			Mark:          e.mark.String(),
			MarkSource:    e.src,
			UnrealizedUSD: e.upnlUSD.String(),
			MarginUSD:     e.marginUSD.String(),
		})
	}
	return snap, nil
}

// baseCurrency resolves accounts.base_currency via the store when it
// exposes it; "" is returned when the store predates the column (tests).
func (s *MarginService) baseCurrency(ctx context.Context, accountID int64) (string, error) {
	if bs, ok := s.store.(interface {
		AccountBaseCurrency(ctx context.Context, accountID int64) (string, error)
	}); ok {
		return bs.AccountBaseCurrency(ctx, accountID)
	}
	return "USD", nil
}

// posEval is one position's evaluated USD contribution (internal).
type posEval struct {
	p         MarginPosition
	mark      decimal.Decimal
	src       string
	upnlUSD   decimal.Decimal
	marginUSD decimal.Decimal
	addonUSD  decimal.Decimal // §13.12 concentration+liquidity add-ons (USD)
}

// portfolioMargin applies the §13.6g correlation offset on top of the
// gross USD IM: for every unordered pair of positions whose position
// returns are negatively correlated beyond |ρ|>0.70, credit
// min(im_i,im_j) × |ρ_pos| × 0.5; total credit is capped at 80% of gross
// and the netted margin may not drop below 20% of gross.
func portfolioMargin(evals []posEval, corr CorrelationProvider) decimal.Decimal {
	var gross decimal.Decimal
	for _, e := range evals {
		gross = gross.Add(e.marginUSD)
	}
	if !gross.IsPositive() || len(evals) < 2 {
		return gross
	}
	var credit decimal.Decimal
	for i := 0; i < len(evals); i++ {
		for j := i + 1; j < len(evals); j++ {
			a, b := evals[i], evals[j]
			raw, ok := corr.Offset(a.p.Symbol, b.p.Symbol)
			if !ok {
				continue
			}
			// Position-return correlation: instrument correlation times
			// the side product (LONG=+1, SHORT=−1). A hedge exists only
			// when position returns are negatively correlated.
			sa, sb := 1.0, 1.0
			if a.p.Side == "SHORT" {
				sa = -1
			}
			if b.p.Side == "SHORT" {
				sb = -1
			}
			rhoPos := raw * sa * sb // bps-scaled
			if rhoPos > -float64(corrGateBps) {
				continue
			}
			min := a.marginUSD
			if b.marginUSD.LessThan(min) {
				min = b.marginUSD
			}
			abs := decimal.NewFromFloat(-rhoPos / 10000) // |ρ_pos| 0..1
			// Per-pair offset factor (Task 19.3.18 configurable groups,
			// §13.6g ≤0.8 ceiling); absent override ⇒ the 0.5 default.
			factor := offsetFactor
			if fp, okF := corr.(CorrelationFactorProvider); okF {
				if f, has := fp.OffsetFactor(a.p.Symbol, b.p.Symbol); has && f > 0 {
					if f > 0.8 {
						f = 0.8
					}
					factor = decimal.NewFromFloat(f)
				}
			}
			credit = credit.Add(min.Mul(abs).Mul(factor))
		}
	}
	// Cap total credit at 80% of gross; net floor at 20% of gross.
	maxCredit := gross.Mul(offsetCapPct)
	if credit.GreaterThan(maxCredit) {
		credit = maxCredit
	}
	net := gross.Sub(credit)
	floor := gross.Mul(offsetFloorPct)
	if net.LessThan(floor) {
		net = floor
	}
	return net
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func normalizeMode(s string) (MarginMode, bool) {
	switch s {
	case "CROSS", "Cross", "cross":
		return ModeCross, true
	case "ISOLATED", "Isolated", "isolated":
		return ModeIsolated, true
	case "PORTFOLIO", "Portfolio", "portfolio":
		return ModePortfolio, true
	}
	return "", false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
