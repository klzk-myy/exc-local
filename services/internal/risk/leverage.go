// Phase-19 Tasks 19.3.2 / 19.3.17 / 19.3.23 / 19.3.24 — FX leverage
// resolution, entity policy matrix, runtime leverage changes.
//
// Canonical effective-leverage contract (spec §13.13/§13.14): the
// effective leverage of an (account, instrument) pair is the MOST
// RESTRICTIVE of every applicable cap —
//
//	effective = min( entity_leverage_policy cell        (Task 19.3.24)
//	                 regulatory category cap            (ESMA/CFTC)
//	                 leverage_tiers band                (Task 19.3.17)
//	                 instruments.max_leverage
//	                 account_leverage chosen value      (Task 19.3.23) )
//
// Fail-closed (spec §2.7): a missing account or instrument row is a
// NOT_FOUND rejection, a store error rejects, and a missing
// entity-policy cell does NOT open the ceiling — it falls back to the
// strictest seed cap (10:1). Resolved decisions are mirrored into Redis
// under leverage:eff:{acct}:{instr}; any input mutation (policy, tier,
// instrument, chosen leverage) invalidates the affected keys.
package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Error codes emitted by this file. All are registered spec §23 codes.
const (
	CodeLeverageNotFound     = "NOT_FOUND"           // account/instrument row missing
	CodeLeverageInvalid      = "INVALID_REQUEST"     // request shape rejected
	CodeLeverageInsufficient = "MARGIN_INSUFFICIENT" // re-check at new leverage failed
	CodeLeverageUnavailable  = "SERVICE_DEGRADED"    // required input unreadable
)

// Instrument-group and regime labels (spec §13.14 matrix axes).
const (
	GroupMajor  = "MAJOR"
	GroupMinor  = "MINOR"
	GroupExotic = "EXOTIC"

	RegimeESMA         = "ESMA"
	RegimeCFTC         = "CFTC"
	RegimeProfessional = "PROFESSIONAL"

	CategoryRetail          = "RETAIL"
	CategoryProfessional    = "PROFESSIONAL"
	CategoryEligible        = "ELIGIBLE_COUNTERPARTY"
	defaultEntityCode       = "INTL"
	fallbackEntityPolicyCap = 10 // strictest seed cap — missing entity row
	leverageCacheTTL        = 5 * time.Minute
	maxSelectableLeverage   = 10_000
)

// Major FX pairs per the Task 19.3.2 ESMA classification (canonical
// ordering base/quote — the instruments table stores them this way).
var majorPairs = map[string]bool{
	"EURUSD": true, "GBPUSD": true, "USDJPY": true, "USDCHF": true,
	"AUDUSD": true, "USDCAD": true, "NZDUSD": true,
}

// G7 deliverable currencies — a non-major pair of two G7 currencies is
// MINOR; anything else is EXOTIC (USD/MXN, USD/TRY, …).
var g7Currencies = map[string]bool{
	"EUR": true, "USD": true, "GBP": true, "JPY": true,
	"CHF": true, "AUD": true, "CAD": true, "NZD": true,
}

// ClassifyInstrumentGroup resolves the §13.14 instrument-group axis from
// the pair's ISO currency codes.
func ClassifyInstrumentGroup(base, quote string) string {
	base = strings.ToUpper(strings.TrimSpace(base))
	quote = strings.ToUpper(strings.TrimSpace(quote))
	if majorPairs[base+quote] {
		return GroupMajor
	}
	if g7Currencies[base] && g7Currencies[quote] {
		return GroupMinor
	}
	return GroupExotic
}

// RegulatoryRegime maps (entity, category) to the leverage_tiers regime
// axis: retail clients resolve through the ESMA/CFTC retail schedule,
// professional/ECP through the negotiable PROFESSIONAL bands. Unknown
// entities default to ESMA — the strictest retail schedule (fail closed).
func RegulatoryRegime(entityCode, clientCategory string) string {
	switch strings.ToUpper(strings.TrimSpace(clientCategory)) {
	case CategoryProfessional, CategoryEligible:
		return RegimeProfessional
	default:
		if strings.EqualFold(strings.TrimSpace(entityCode), "US-CFTC") {
			return RegimeCFTC
		}
		return RegimeESMA
	}
}

// categoryCap is the statutory retail ceiling for (regime, group) —
// ESMA 30/20/10, CFTC 50/20/10. nil = no statutory cap (professional
// clients negotiate within the entity-policy + tier + instrument caps).
func categoryCap(regime, group string) *int {
	var v int
	switch regime {
	case RegimeESMA:
		switch group {
		case GroupMajor:
			v = 30
		case GroupMinor:
			v = 20
		default:
			v = 10
		}
	case RegimeCFTC:
		switch group {
		case GroupMajor:
			v = 50
		case GroupMinor:
			v = 20
		default:
			v = 10
		}
	default:
		return nil
	}
	return &v
}

// ---------------------------------------------------------------------------
// Types + store seam
// ---------------------------------------------------------------------------

// LeverageAccountView is the account-side resolution input.
type LeverageAccountView struct {
	ID             int64
	EntityCode     string // accounts.entity_code (migration 098)
	ClientCategory string // accounts.client_category (migration 042)
}

// LeverageInstrumentView is the instrument-side resolution input.
type LeverageInstrumentView struct {
	ID            int64
	Symbol        string
	BaseCurrency  string
	QuoteCurrency string
	MaxLeverage   int // <= 0 → not marginable, cap resolves to 1
}

// Group classifies the pair (derived — never stored).
func (v *LeverageInstrumentView) Group() string {
	return ClassifyInstrumentGroup(v.BaseCurrency, v.QuoteCurrency)
}

// ChosenLeverage is one account_leverage row: the account's chosen
// leverage for one instrument (InstrumentID nil = account default).
type ChosenLeverage struct {
	InstrumentID *int64
	Leverage     int
	UpdatedBy    *int64
	UpdatedAt    time.Time
}

// PositionNotional is one open position's gross notional in quote
// currency — the margin re-check input for leverage decreases.
type PositionNotional struct {
	InstrumentID  int64
	Symbol        string
	Side          string
	GrossNotional decimal.Decimal
}

// LeverageStore is the persistence seam (PgLeverageStore in production).
type LeverageStore interface {
	AccountLeverageView(ctx context.Context, accountID int64) (*LeverageAccountView, error)
	InstrumentLeverageView(ctx context.Context, instrumentID int64) (*LeverageInstrumentView, error)
	InstrumentLeverageViewBySymbol(ctx context.Context, symbol string) (*LeverageInstrumentView, error)
	// EntityPolicyCap resolves the newest effective entity_leverage_policy
	// cell; nil = no applicable row (caller falls back, never opens).
	EntityPolicyCap(ctx context.Context, entityCode, clientCategory, group string) (*int, error)
	// TierCap resolves the Task 19.3.17 band cap for (regime, group) at
	// gross notional; nil = no bands configured.
	TierCap(ctx context.Context, regime, group string, notional decimal.Decimal) (*int, error)
	// ChosenLeverage returns the per-symbol and account-default chosen
	// rows (either may be nil).
	ChosenLeverage(ctx context.Context, accountID, instrumentID int64) (perSymbol *ChosenLeverage, accountDefault *ChosenLeverage, err error)
	// SymbolGrossNotional is the account's live gross notional on the
	// instrument (tier-band input — hedge legs sum under HEDGING).
	SymbolGrossNotional(ctx context.Context, accountID, instrumentID int64) (decimal.Decimal, error)
	// OpenPositionNotionals lists the account's open positions (qty<>0)
	// for the leverage-decrease margin re-check.
	OpenPositionNotionals(ctx context.Context, accountID int64) ([]PositionNotional, error)
	// MarginEquity is the account's latest margin_accounts equity; nil =
	// no margin account row.
	MarginEquity(ctx context.Context, accountID int64) (*decimal.Decimal, error)
	// PutChosenLeverage upserts account_leverage and writes the
	// audit row atomically (leverage changes are audited — Task 19.3.23).
	PutChosenLeverage(ctx context.Context, accountID int64, instrumentID *int64,
		leverage int, before *int, userID int64) error
}

// LeverageCache is the Redis mirror seam for resolved decisions.
// Fail-open by design: cache errors degrade to a PG resolve (PostgreSQL
// stays authoritative); a cached value is never trusted past its TTL.
type LeverageCache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, val string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	ScanKeys(ctx context.Context, pattern string) ([]string, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// LeverageResolver is the canonical interface sibling clusters consume:
// margin engines, pre-trade margin checks, exposure tiering and the UI
// effective-leverage display all resolve through Effective.
type LeverageResolver interface {
	Effective(ctx context.Context, accountID, instrumentID int64) (int, error)
}

// LeverageService resolves effective leverage and applies runtime
// leverage changes.
type LeverageService struct {
	store LeverageStore
	cache LeverageCache // may be nil — resolution stays PG-authoritative
	now   func() time.Time
}

// NewLeverageService wires the resolver.
func NewLeverageService(store LeverageStore, cache LeverageCache) *LeverageService {
	return &LeverageService{store: store, cache: cache, now: time.Now}
}

// WithClock injects a test clock.
func (s *LeverageService) WithClock(now func() time.Time) *LeverageService {
	if now != nil {
		s.now = now
	}
	return s
}

// LeverageCapComponent records one cap considered during resolution —
// the audit/debug trail proving most-restrictive-wins.
type LeverageCapComponent struct {
	Source string `json:"source"` // entity_policy | entity_policy_fallback | category_cap | tier_band | instrument_max | account_chosen
	Cap    int    `json:"cap"`
}

// LeverageDecision is the fully resolved answer with provenance.
type LeverageDecision struct {
	AccountID    int64                  `json:"account_id"`
	InstrumentID int64                  `json:"instrument_id"`
	Effective    int                    `json:"effective"`
	Source       string                 `json:"source"` // winning component
	Group        string                 `json:"instrument_group"`
	Regime       string                 `json:"regulatory_regime"`
	Components   []LeverageCapComponent `json:"components"`
	ResolvedAt   time.Time              `json:"resolved_at"`
}

// cacheKey is the Task 19.3.2 canonical key: leverage:eff:{acct}:{instr}.
func leverageCacheKey(accountID, instrumentID int64) string {
	return fmt.Sprintf("leverage:eff:%d:%d", accountID, instrumentID)
}

// Effective is the LeverageResolver contract — most-restrictive cap or
// a coded error (fail closed).
func (s *LeverageService) Effective(ctx context.Context, accountID, instrumentID int64) (int, error) {
	d, err := s.Resolve(ctx, accountID, instrumentID)
	if err != nil {
		return 0, err
	}
	return d.Effective, nil
}

// Resolve computes the full decision (cached under leverage:eff:*).
func (s *LeverageService) Resolve(ctx context.Context, accountID, instrumentID int64) (*LeverageDecision, error) {
	if s.cache != nil {
		if v, err := s.cache.Get(ctx, leverageCacheKey(accountID, instrumentID)); err == nil && v != "" {
			var d LeverageDecision
			if json.Unmarshal([]byte(v), &d) == nil && d.Effective > 0 {
				return &d, nil
			}
		}
	}

	acct, err := s.store.AccountLeverageView(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "account leverage view", err)
	}
	if acct == nil {
		return nil, excerrors.New(CodeLeverageNotFound,
			fmt.Sprintf("account %d not found", accountID))
	}
	inst, err := s.store.InstrumentLeverageView(ctx, instrumentID)
	if err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "instrument leverage view", err)
	}
	if inst == nil {
		return nil, excerrors.New(CodeLeverageNotFound,
			fmt.Sprintf("instrument %d not found", instrumentID))
	}

	notional, err := s.store.SymbolGrossNotional(ctx, accountID, instrumentID)
	if err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "symbol notional", err)
	}

	d, err := s.resolveFor(acct, inst, notional, nil)
	if err != nil {
		return nil, err
	}

	if s.cache != nil {
		if blob, merr := json.Marshal(d); merr == nil {
			// Cache failure degrades to a re-resolve — never returned.
			_ = s.cache.Set(ctx, leverageCacheKey(accountID, instrumentID), string(blob), leverageCacheTTL)
		}
	}
	return d, nil
}

// resolveFor is the pure most-restrictive-wins fold over the cap
// components. chosenOverride replaces the stored account-chosen row
// when set (the runtime-change re-check path).
func (s *LeverageService) resolveFor(acct *LeverageAccountView,
	inst *LeverageInstrumentView, notional decimal.Decimal,
	chosenOverride *int) (*LeverageDecision, error) {

	entity := strings.ToUpper(strings.TrimSpace(acct.EntityCode))
	if entity == "" {
		entity = defaultEntityCode
	}
	cat := strings.ToUpper(strings.TrimSpace(acct.ClientCategory))
	if cat == "" {
		cat = CategoryRetail // missing category = retail (fail closed)
	}
	group := inst.Group()
	regime := RegulatoryRegime(entity, cat)

	var comps []LeverageCapComponent

	// 1. Entity policy matrix (Task 19.3.24 — highest precedence input
	//    into the minimum). Absent row → strictest seed cap.
	pol, err := s.store.EntityPolicyCap(context.Background(), entity, cat, group)
	if err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "entity leverage policy", err)
	}
	if pol == nil {
		comps = append(comps, LeverageCapComponent{"entity_policy_fallback", fallbackEntityPolicyCap})
	} else {
		comps = append(comps, LeverageCapComponent{"entity_policy", *pol})
	}

	// 2. Regulatory category cap (ESMA/CFTC retail; nil for pro/ECP).
	if c := categoryCap(regime, group); c != nil {
		comps = append(comps, LeverageCapComponent{"category_cap", *c})
	}

	// 3. Tiered notional band (Task 19.3.17).
	tier, err := s.store.TierCap(context.Background(), regime, group, notional)
	if err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "leverage tier", err)
	}
	if tier != nil {
		comps = append(comps, LeverageCapComponent{"tier_band", *tier})
	}

	// 4. Instrument ceiling; a non-marginable instrument pins at 1.
	instCap := inst.MaxLeverage
	if instCap <= 0 {
		instCap = 1
	}
	comps = append(comps, LeverageCapComponent{"instrument_max", instCap})

	// 5. Account-chosen leverage (per-symbol row wins over default).
	if chosenOverride != nil {
		comps = append(comps, LeverageCapComponent{"account_chosen", *chosenOverride})
	} else {
		perSym, def, err := s.store.ChosenLeverage(context.Background(), acct.ID, inst.ID)
		if err != nil {
			return nil, excerrors.Wrap(CodeLeverageUnavailable, "chosen leverage", err)
		}
		switch {
		case perSym != nil:
			comps = append(comps, LeverageCapComponent{"account_chosen", perSym.Leverage})
		case def != nil:
			comps = append(comps, LeverageCapComponent{"account_chosen", def.Leverage})
		}
	}

	eff, src := 0, ""
	for _, c := range comps {
		if c.Cap <= 0 {
			continue
		}
		if eff == 0 || c.Cap < eff {
			eff, src = c.Cap, c.Source
		}
	}
	if eff <= 0 {
		return nil, excerrors.New(CodeLeverageUnavailable,
			"leverage resolution produced no cap — rejecting (fail closed)")
	}
	return &LeverageDecision{
		AccountID: acct.ID, InstrumentID: inst.ID,
		Effective: eff, Source: src, Group: group, Regime: regime,
		Components: comps, ResolvedAt: s.now().UTC(),
	}, nil
}

// Invalidate drops the cached decision for one pair — call on every
// input mutation (chosen leverage, instrument, policy, tier).
func (s *LeverageService) Invalidate(ctx context.Context, accountID, instrumentID int64) {
	if s.cache != nil {
		_ = s.cache.Del(ctx, leverageCacheKey(accountID, instrumentID))
	}
}

// InvalidateAccount drops every cached pair for the account (entity or
// default-leverage changes).
func (s *LeverageService) InvalidateAccount(ctx context.Context, accountID int64) {
	s.invalidatePattern(ctx, fmt.Sprintf("leverage:eff:%d:*", accountID))
}

// InvalidateInstrument drops every cached pair for the instrument
// (max_leverage or classification edits).
func (s *LeverageService) InvalidateInstrument(ctx context.Context, instrumentID int64) {
	s.invalidatePattern(ctx, fmt.Sprintf("leverage:eff:*:%d", instrumentID))
}

// InvalidateAll drops the whole effective-leverage cache — entity-policy
// and tier-table changes affect unknown account sets, so the matrix
// publishes a full invalidation (keys are cheap to rebuild).
func (s *LeverageService) InvalidateAll(ctx context.Context) {
	s.invalidatePattern(ctx, "leverage:eff:*")
}

func (s *LeverageService) invalidatePattern(ctx context.Context, pattern string) {
	if s.cache == nil {
		return
	}
	keys, err := s.cache.ScanKeys(ctx, pattern)
	if err != nil || len(keys) == 0 {
		return
	}
	_ = s.cache.Del(ctx, keys...)
}

// ---------------------------------------------------------------------------
// Runtime leverage changes — Task 19.3.23
// ---------------------------------------------------------------------------

// SetLeverageInput is the runtime endpoint request (symbol nil =
// account-default row).
type SetLeverageInput struct {
	AccountID    int64
	InstrumentID *int64 // nil = account default
	Requested    int
	UserID       int64
}

// SetLeverageResult reports the accepted write plus the resolved
// effective value so the caller/UI sees the atomic update.
type SetLeverageResult struct {
	AccountID    int64
	InstrumentID *int64
	Requested    int
	Effective    *LeverageDecision // nil for the default row (per-symbol resolves lazily)
}

// SetLeverage applies a runtime leverage change (spec §13.13):
//   - requested must be positive and within the resolved cap;
//   - with open positions the change requires a successful margin
//     re-check at the new leverage (equity ≥ Σ notional/newEff);
//   - the write + audit row commit atomically; cache is invalidated.
func (s *LeverageService) SetLeverage(ctx context.Context, in SetLeverageInput) (*SetLeverageResult, error) {
	if in.Requested <= 0 || in.Requested > maxSelectableLeverage {
		return nil, excerrors.New(CodeLeverageInvalid,
			fmt.Sprintf("leverage must be in 1..%d", maxSelectableLeverage))
	}
	acct, err := s.store.AccountLeverageView(ctx, in.AccountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "account leverage view", err)
	}
	if acct == nil {
		return nil, excerrors.New(CodeLeverageNotFound,
			fmt.Sprintf("account %d not found", in.AccountID))
	}

	var inst *LeverageInstrumentView
	var capForRequest int
	if in.InstrumentID != nil {
		inst, err = s.store.InstrumentLeverageView(ctx, *in.InstrumentID)
		if err != nil {
			return nil, excerrors.Wrap(CodeLeverageUnavailable, "instrument leverage view", err)
		}
		if inst == nil {
			return nil, excerrors.New(CodeLeverageNotFound,
				fmt.Sprintf("instrument %d not found", *in.InstrumentID))
		}
		// Cap = resolution with the account-chosen input REMOVED (the
		// row being replaced must not cap itself).
		d, err := s.capExcludingChosen(ctx, acct, inst)
		if err != nil {
			return nil, err
		}
		capForRequest = d
	} else {
		d, err := s.defaultCap(ctx, acct)
		if err != nil {
			return nil, err
		}
		capForRequest = d
	}
	if in.Requested > capForRequest {
		return nil, excerrors.New(CodeLeverageInvalid,
			fmt.Sprintf("leverage %d exceeds resolved cap %d", in.Requested, capForRequest))
	}

	// Open-position compatibility (spec §13.13: a change requires no
	// open positions OR a successful re-check at the new leverage).
	positions, err := s.store.OpenPositionNotionals(ctx, in.AccountID)
	if err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "open positions", err)
	}
	if len(positions) > 0 {
		if err := s.recheckMargin(ctx, acct, in, positions); err != nil {
			return nil, err
		}
	}

	// Snapshot the current value for the audit row.
	var before *int
	if in.InstrumentID != nil {
		perSym, _, cerr := s.store.ChosenLeverage(ctx, in.AccountID, *in.InstrumentID)
		if cerr != nil {
			return nil, excerrors.Wrap(CodeLeverageUnavailable, "chosen leverage read", cerr)
		}
		if perSym != nil {
			v := perSym.Leverage
			before = &v
		}
	} else {
		_, def, cerr := s.store.ChosenLeverage(ctx, in.AccountID, 0)
		if cerr != nil {
			return nil, excerrors.Wrap(CodeLeverageUnavailable, "chosen leverage read", cerr)
		}
		if def != nil {
			v := def.Leverage
			before = &v
		}
	}

	if err := s.store.PutChosenLeverage(ctx, in.AccountID, in.InstrumentID,
		in.Requested, before, in.UserID); err != nil {
		return nil, excerrors.Wrap(CodeLeverageUnavailable, "chosen leverage write", err)
	}

	// Input changed → invalidate: the pair for per-symbol writes, every
	// pair for the account default.
	if in.InstrumentID != nil {
		s.Invalidate(ctx, in.AccountID, *in.InstrumentID)
	} else {
		s.InvalidateAccount(ctx, in.AccountID)
	}

	res := &SetLeverageResult{
		AccountID: in.AccountID, InstrumentID: in.InstrumentID,
		Requested: in.Requested,
	}
	if in.InstrumentID != nil {
		if d, err := s.Resolve(ctx, in.AccountID, *in.InstrumentID); err == nil {
			res.Effective = d
		}
	}
	return res, nil
}

// capExcludingChosen resolves min(entity, category, tier, instrument)
// — everything except the account-chosen input.
func (s *LeverageService) capExcludingChosen(ctx context.Context,
	acct *LeverageAccountView, inst *LeverageInstrumentView) (int, error) {

	notional, err := s.store.SymbolGrossNotional(ctx, acct.ID, inst.ID)
	if err != nil {
		return 0, excerrors.Wrap(CodeLeverageUnavailable, "symbol notional", err)
	}
	// Reuse resolveFor with an override that is guaranteed non-binding:
	// maxSelectableLeverage is the schema ceiling, so the min() never
	// picks it unless every real cap is above it — in which case the
	// schema ceiling IS the cap and the value is still correct.
	d, err := s.resolveFor(acct, inst, notional, ptrInt(maxSelectableLeverage))
	if err != nil {
		return 0, err
	}
	return d.Effective, nil
}

// defaultCap bounds the account-default row: the highest cap any
// instrument could resolve for this account (max over groups of the
// non-chosen resolution). Requested defaults above it could never take
// effect anywhere — rejecting them keeps the row meaningful.
func (s *LeverageService) defaultCap(ctx context.Context, acct *LeverageAccountView) (int, error) {
	entity := strings.ToUpper(strings.TrimSpace(acct.EntityCode))
	if entity == "" {
		entity = defaultEntityCode
	}
	cat := strings.ToUpper(strings.TrimSpace(acct.ClientCategory))
	if cat == "" {
		cat = CategoryRetail
	}
	regime := RegulatoryRegime(entity, cat)
	best := 0
	for _, g := range []string{GroupMajor, GroupMinor, GroupExotic} {
		cap := maxSelectableLeverage
		if pol, err := s.store.EntityPolicyCap(ctx, entity, cat, g); err != nil {
			return 0, excerrors.Wrap(CodeLeverageUnavailable, "entity leverage policy", err)
		} else if pol != nil {
			cap = *pol
		} else {
			cap = fallbackEntityPolicyCap
		}
		if c := categoryCap(regime, g); c != nil && *c < cap {
			cap = *c
		}
		if t, err := s.store.TierCap(ctx, regime, g, decimal.Zero); err != nil {
			return 0, excerrors.Wrap(CodeLeverageUnavailable, "leverage tier", err)
		} else if t != nil && *t < cap {
			cap = *t
		}
		if cap > best {
			best = cap
		}
	}
	return best, nil
}

// recheckMargin enforces the §13.13 open-position compatibility rule:
// projected used margin under the new chosen leverage must not exceed
// the account's recorded equity. A missing margin account fails closed.
func (s *LeverageService) recheckMargin(ctx context.Context, acct *LeverageAccountView,
	in SetLeverageInput, positions []PositionNotional) error {

	equity, err := s.store.MarginEquity(ctx, in.AccountID)
	if err != nil {
		return excerrors.Wrap(CodeLeverageUnavailable, "margin equity", err)
	}
	if equity == nil {
		return excerrors.New(CodeLeverageInsufficient,
			"no margin account for leverage change with open positions")
	}
	projected := decimal.Zero
	for _, p := range positions {
		// Per-symbol writes only reprice that instrument's positions;
		// the default row reprices instruments without a per-symbol row.
		affected := in.InstrumentID == nil || *in.InstrumentID == p.InstrumentID
		if in.InstrumentID == nil {
			perSym, _, cerr := s.store.ChosenLeverage(ctx, in.AccountID, p.InstrumentID)
			if cerr != nil {
				return excerrors.Wrap(CodeLeverageUnavailable, "chosen leverage read", cerr)
			}
			if perSym != nil {
				affected = false // per-symbol row wins — unchanged by the default write
			}
		}
		inst, err := s.store.InstrumentLeverageView(ctx, p.InstrumentID)
		if err != nil {
			return excerrors.Wrap(CodeLeverageUnavailable, "instrument leverage view", err)
		}
		if inst == nil {
			return excerrors.New(CodeLeverageNotFound,
				fmt.Sprintf("instrument %d not found", p.InstrumentID))
		}
		capEx, err := s.capExcludingChosen(ctx, acct, inst)
		if err != nil {
			return err
		}
		eff := capEx
		if affected && in.Requested < eff {
			eff = in.Requested
		}
		projected = projected.Add(p.GrossNotional.Div(decimal.NewFromInt(int64(eff))))
	}
	if projected.GreaterThan(*equity) {
		return excerrors.New(CodeLeverageInsufficient,
			fmt.Sprintf("projected used margin %s exceeds equity %s at requested leverage",
				projected, *equity))
	}
	return nil
}

func ptrInt(v int) *int { return &v }

// ---------------------------------------------------------------------------
// PgLeverageStore — PostgreSQL implementation
// ---------------------------------------------------------------------------

// PgLeverageStore implements LeverageStore over pgx.
type PgLeverageStore struct {
	pool *pgxpool.Pool
}

// NewPgLeverageStore wraps pool.
func NewPgLeverageStore(pool *pgxpool.Pool) *PgLeverageStore {
	return &PgLeverageStore{pool: pool}
}

func (s *PgLeverageStore) AccountLeverageView(ctx context.Context, accountID int64) (*LeverageAccountView, error) {
	var v LeverageAccountView
	err := s.pool.QueryRow(ctx, `
		SELECT id, COALESCE(entity_code,'INTL'), COALESCE(client_category::text,'RETAIL')
		  FROM accounts WHERE id = $1`, accountID).
		Scan(&v.ID, &v.EntityCode, &v.ClientCategory)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *PgLeverageStore) InstrumentLeverageView(ctx context.Context, instrumentID int64) (*LeverageInstrumentView, error) {
	var v LeverageInstrumentView
	err := s.pool.QueryRow(ctx, `
		SELECT id, symbol, base_currency, quote_currency, max_leverage
		  FROM instruments WHERE id = $1`, instrumentID).
		Scan(&v.ID, &v.Symbol, &v.BaseCurrency, &v.QuoteCurrency, &v.MaxLeverage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *PgLeverageStore) InstrumentLeverageViewBySymbol(ctx context.Context, symbol string) (*LeverageInstrumentView, error) {
	var v LeverageInstrumentView
	err := s.pool.QueryRow(ctx, `
		SELECT id, symbol, base_currency, quote_currency, max_leverage
		  FROM instruments WHERE symbol = $1`, strings.ToUpper(strings.TrimSpace(symbol))).
		Scan(&v.ID, &v.Symbol, &v.BaseCurrency, &v.QuoteCurrency, &v.MaxLeverage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *PgLeverageStore) EntityPolicyCap(ctx context.Context, entityCode, clientCategory, group string) (*int, error) {
	var v int
	err := s.pool.QueryRow(ctx, `
		SELECT max_leverage FROM entity_leverage_policy
		 WHERE entity_code = $1 AND client_category = $2::client_category_enum
		   AND instrument_group = $3 AND effective_from <= now()
		 ORDER BY effective_from DESC, id DESC LIMIT 1`,
		strings.ToUpper(strings.TrimSpace(entityCode)),
		strings.ToUpper(strings.TrimSpace(clientCategory)), group).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// EntityPolicyRow is one entity_leverage_policy record — the venue-doc
// projection and admin-list row share the shape (§13.14).
type EntityPolicyRow struct {
	EntityCode      string    `json:"entity_code"`
	ClientCategory  string    `json:"client_category"`
	InstrumentGroup string    `json:"instrument_group"`
	MaxLeverage     int       `json:"max_leverage"`
	EffectiveFrom   time.Time `json:"effective_from"`
}

// ListEntityPolicies returns the full matrix ordered for display —
// the §13.14 admin surface and the venue-info publication share it.
func (s *PgLeverageStore) ListEntityPolicies(ctx context.Context) ([]EntityPolicyRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT entity_code, client_category::text, instrument_group,
		       max_leverage, effective_from
		  FROM entity_leverage_policy
		 ORDER BY entity_code, client_category, instrument_group,
		          effective_from DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntityPolicyRow
	for rows.Next() {
		var r EntityPolicyRow
		if err := rows.Scan(&r.EntityCode, &r.ClientCategory,
			&r.InstrumentGroup, &r.MaxLeverage, &r.EffectiveFrom); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertEntityPolicyTx writes one effective-dated policy cell inside
// the caller's transaction — the Task-19.3.24 admin surface runs it in
// the dual-control approval tx so the ceiling change and the four-eyes
// record commit or fail together. maker/approver land on
// created_by/updated_by for the §13.14 audit trail.
func UpsertEntityPolicyTx(ctx context.Context, tx pgx.Tx, row EntityPolicyRow,
	maker, approver int64) error {

	_, err := tx.Exec(ctx, `
		INSERT INTO entity_leverage_policy
		    (entity_code, client_category, instrument_group,
		     max_leverage, effective_from, created_by, updated_by)
		VALUES ($1, $2::client_category_enum, $3, $4, $5, $6, $7)
		ON CONFLICT ON CONSTRAINT entity_leverage_policy_cell_uq
		DO UPDATE SET max_leverage = EXCLUDED.max_leverage,
		              updated_by   = EXCLUDED.updated_by,
		              updated_at   = now()`,
		strings.ToUpper(strings.TrimSpace(row.EntityCode)),
		strings.ToUpper(strings.TrimSpace(row.ClientCategory)),
		row.InstrumentGroup, row.MaxLeverage, row.EffectiveFrom,
		maker, approver)
	return err
}

func (s *PgLeverageStore) TierCap(ctx context.Context, regime, group string, notional decimal.Decimal) (*int, error) {
	var v int
	err := s.pool.QueryRow(ctx, `
		SELECT max_leverage FROM leverage_tiers
		 WHERE instrument_group = $1 AND regulatory_regime = $2
		   AND notional_from <= $3::numeric
		   AND (notional_to IS NULL OR notional_to > $3::numeric)
		 ORDER BY notional_from DESC LIMIT 1`,
		group, regime, notional.String()).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *PgLeverageStore) ChosenLeverage(ctx context.Context, accountID, instrumentID int64) (*ChosenLeverage, *ChosenLeverage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT instrument_id, leverage, updated_by, updated_at
		  FROM account_leverage
		 WHERE account_id = $1 AND (instrument_id = $2 OR instrument_id IS NULL)`,
		accountID, instrumentID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var perSym, def *ChosenLeverage
	for rows.Next() {
		var c ChosenLeverage
		if err := rows.Scan(&c.InstrumentID, &c.Leverage, &c.UpdatedBy, &c.UpdatedAt); err != nil {
			return nil, nil, err
		}
		if c.InstrumentID == nil {
			def = &c
		} else {
			perSym = &c
		}
	}
	return perSym, def, rows.Err()
}

func (s *PgLeverageStore) SymbolGrossNotional(ctx context.Context, accountID, instrumentID int64) (decimal.Decimal, error) {
	var txt string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(ABS(p.quantity) * p.mark_price), 0)::text
		  FROM positions p
		 WHERE p.account_id = $1 AND p.instrument_id = $2 AND p.quantity <> 0`,
		accountID, instrumentID).Scan(&txt)
	if err != nil {
		return decimal.Zero, err
	}
	d, err := decimal.NewFromString(txt)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse gross notional %q: %w", txt, err)
	}
	return d, nil
}

func (s *PgLeverageStore) OpenPositionNotionals(ctx context.Context, accountID int64) ([]PositionNotional, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.instrument_id, i.symbol, p.side,
		       (ABS(p.quantity) * p.mark_price)::text
		  FROM positions p JOIN instruments i ON i.id = p.instrument_id
		 WHERE p.account_id = $1 AND p.quantity <> 0`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PositionNotional
	for rows.Next() {
		var p PositionNotional
		var txt string
		if err := rows.Scan(&p.InstrumentID, &p.Symbol, &p.Side, &txt); err != nil {
			return nil, err
		}
		d, err := decimal.NewFromString(txt)
		if err != nil {
			return nil, fmt.Errorf("parse position notional %q: %w", txt, err)
		}
		p.GrossNotional = d
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PgLeverageStore) MarginEquity(ctx context.Context, accountID int64) (*decimal.Decimal, error) {
	var txt *string
	err := s.pool.QueryRow(ctx, `
		SELECT equity::text FROM margin_accounts
		 WHERE account_id = $1 ORDER BY id DESC LIMIT 1`, accountID).Scan(&txt)
	if errors.Is(err, pgx.ErrNoRows) || txt == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := decimal.NewFromString(*txt)
	if err != nil {
		return nil, fmt.Errorf("parse equity %q: %w", *txt, err)
	}
	return &d, nil
}

// PutChosenLeverage upserts the row and appends the audit entry in one
// transaction — the §13.13/§24 change-audit contract.
func (s *PgLeverageStore) PutChosenLeverage(ctx context.Context, accountID int64,
	instrumentID *int64, leverage int, before *int, userID int64) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_leverage (account_id, instrument_id, leverage, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT ON CONSTRAINT account_leverage_cell_uq
		DO UPDATE SET leverage = EXCLUDED.leverage,
		              updated_by = EXCLUDED.updated_by,
		              updated_at = now()`,
		accountID, instrumentID, leverage, userID); err != nil {
		return err
	}
	beforeJSON := "null"
	if before != nil {
		beforeJSON = fmt.Sprintf(`{"leverage":%d}`, *before)
	}
	afterJSON := fmt.Sprintf(`{"leverage":%d,"instrument_id":%s}`,
		leverage, int64JSON(instrumentID))
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log (admin_user_id, action, target_type, target_id,
		                             before_state, after_state)
		VALUES ($1, 'account.leverage.update', 'account', $2, $3::jsonb, $4::jsonb)`,
		userID, accountID, beforeJSON, afterJSON); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func int64JSON(v *int64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *v)
}

// ---------------------------------------------------------------------------
// RedisLeverageCache — leverage:eff:* mirror
// ---------------------------------------------------------------------------

// RedisLeverageCache mirrors resolved decisions into the coordination
// Redis under leverage:eff:{acct}:{instr} (Task 19.3.2).
type RedisLeverageCache struct {
	rdb *excredis.Client
}

// NewRedisLeverageCache wraps the coordination client.
func NewRedisLeverageCache(rdb *excredis.Client) *RedisLeverageCache {
	return &RedisLeverageCache{rdb: rdb}
}

func (c *RedisLeverageCache) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return "", nil
	}
	return v, err
}

func (c *RedisLeverageCache) Set(ctx context.Context, key, val string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, val, ttl).Err()
}

func (c *RedisLeverageCache) Del(ctx context.Context, keys ...string) error {
	return c.rdb.Del(ctx, keys...).Err()
}

func (c *RedisLeverageCache) ScanKeys(ctx context.Context, pattern string) ([]string, error) {
	var keys []string
	iter := c.rdb.Scan(ctx, 0, pattern, 200).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}
