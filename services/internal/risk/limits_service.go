// Package risk enforces per-account and per-symbol risk limits.
//
// Phase-03 Task 3.3.5 (spec §5.10 risk_limits, §13.6 exposure limits).
// Scope boundary: this package owns the service logic — loading limits,
// enforcing them, caching them in Redis. Phase-05 Task 5.3.4 wires the
// REST route GET /api/v1/account/risk-limits onto LimitsView.
//
// Limit resolution (most specific wins, per column independently):
//
//	account+symbol > account+wildcard > tier+symbol > tier+wildcard >
//	global+symbol > global+wildcard
//
// where wildcard means symbol IS NULL or '*'. Exposure columns
// (max_notional_exposure / max_short_exposure / max_account_notional)
// fall back to the spec §13.6 canonical defaults when no row supplies a
// value; all other columns are unlimited when unset.
//
// Daily usage counters live in the risk_daily_usage table keyed by UTC
// calendar day (migration 109): the 00:00 UTC reset is structural — a new
// day starts at zero with no reset job. Redis mirrors are written for
// zero-latency gateway reads but PostgreSQL stays authoritative.
//
// Fail-closed (spec §2.7): any store error fails the check. A configured
// exposure/daily-volume cap with no usable price is a rejection, never a
// pass.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excredis "exchange/internal/redis"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Error codes emitted by this package. MAX_EXPOSURE_EXCEEDED is a
// registered spec §23 code (HTTP 400); the remainder are scaffold
// placeholders pending canonical registration in Phase-05 Task 5.3.21.
const (
	CodeMaxExposureExceeded = "MAX_EXPOSURE_EXCEEDED" // spec §23, 400/L2
	CodeOrderRejected       = "ORDER_REJECTED"        // spec §23, 400/L2
	CodeRiskLimitsInternal  = "RISK_LIMITS_INTERNAL"  // scaffold — register in 5.3.21
)

// Spec §13.6 canonical defaults, applied when no risk_limits row supplies
// an exposure value.
var (
	DefaultSymbolNotionalCap  = decimal.NewFromInt(10_000_000) // $10M per symbol
	DefaultShortNotionalCap   = decimal.NewFromInt(5_000_000)  // $5M short per symbol
	DefaultAccountNotionalCap = decimal.NewFromInt(50_000_000) // $50M all symbols

	// §13.6a / Task 13.3.6 (MiFID II RTS 9) canonical OTR defaults —
	// applied when no row resolves a value (the columns themselves are
	// NOT NULL DEFAULT after migration 047, so the fallback only guards
	// an entirely empty table).
	DefaultOtrRatio  = decimal.NewFromInt(500)
	DefaultOtrWindow = 60 * time.Second
)

// Redis key layout (spec §4 naming style). risk_limits:rows is the full
// JSON snapshot for gateway read-through; daily keys are mirrors of the
// authoritative risk_daily_usage rows.
const (
	redisRowsKey       = "risk_limits:rows"
	redisDailyUsageTTL = 48 * time.Hour
)

func dailyVolumeKey(accountID int64, day time.Time) string {
	return fmt.Sprintf("risk:daily_volume:%d:%s", accountID, day.Format("20060102"))
}

func dailyWithdrawnKey(accountID int64, day time.Time) string {
	return fmt.Sprintf("risk:daily_withdrawn:%d:%s", accountID, day.Format("20060102"))
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Row mirrors one risk_limits row. NULL key dimensions widen scope:
// AccountID nil = global default, Symbol nil/"*" = all symbols,
// Tier nil = KYC-tier-independent.
type Row struct {
	ID                   int64
	AccountID            *int64
	Symbol               *string
	Tier                 *string // kyc_tier_enum: 'T0'|'T1'|'T2'
	MaxOrderQty          *decimal.Decimal
	MaxDailyVolume       *decimal.Decimal
	MaxOpenOrders        *int32
	DailyWithdrawLimit   *decimal.Decimal
	MaxWithdrawAmount    *decimal.Decimal
	WithdrawRatePerHour  *decimal.Decimal
	MaxNotionalExposure  *decimal.Decimal // migration 109
	MaxShortExposure     *decimal.Decimal // migration 109
	MaxAccountNotional   *decimal.Decimal // migration 109
	MaxOrderToTradeRatio *decimal.Decimal // migration 047 — MiFID II RTS 9
	OtrWindow            *time.Duration   // migration 047 — rolling OTR window
}

// EffectiveLimits is the resolved limit set for one (account, tier,
// symbol) triple. Nil pointers mean unlimited — except the three exposure
// fields and the OTR pair, which are always non-nil after resolution
// because §13.6/§13.6a define canonical defaults.
type EffectiveLimits struct {
	MaxOrderQty         *decimal.Decimal
	MaxDailyVolume      *decimal.Decimal
	MaxOpenOrders       *int32
	DailyWithdrawLimit  *decimal.Decimal
	MaxWithdrawAmount   *decimal.Decimal
	WithdrawRatePerHour *decimal.Decimal
	MaxNotionalExposure *decimal.Decimal
	MaxShortExposure    *decimal.Decimal
	MaxAccountNotional  *decimal.Decimal
	// Task 13.3.6 — MiFID II RTS 9 order-to-trade ratio. Always resolved:
	// a scoped row wins (the MM/program-instrument allowance is simply a
	// higher ratio on an account/symbol-scoped row), else the §13.6a
	// venue default 500 events/trade over a 60s window.
	MaxOrderToTradeRatio *decimal.Decimal
	OtrWindow            time.Duration
}

// Usage is one account's utilisation of its daily counters for one UTC
// day (risk_daily_usage).
type Usage struct {
	Volume    decimal.Decimal // traded notional since 00:00 UTC
	Withdrawn decimal.Decimal // withdrawn since 00:00 UTC
}

// SymbolExposure is one account's live exposure on one symbol.
// Notionals are quote-currency units pending the Phase-19.5 mark oracle;
// per-symbol caps should therefore be configured in quote terms.
type SymbolExposure struct {
	Symbol        string
	GrossNotional decimal.Decimal
	ShortNotional decimal.Decimal // SELL-side positions only
}

// OrderRequest is the pre-trade check input. Price is the limit price —
// or a worst-case estimate for MARKET orders (Phase-02 slippage bound);
// it is required whenever a configured daily-volume or exposure cap must
// convert quantity to notional.
type OrderRequest struct {
	AccountID  int64
	KycTier    string // 'T0' | 'T1' | 'T2'
	Symbol     string
	Side       string // 'BUY' | 'SELL'
	Quantity   decimal.Decimal
	Price      decimal.Decimal
	ReduceOnly bool   // reduces exposure: notional increment checks skipped
	OpenOrders *int64 // caller-known open-order count; nil = store query
}

// Store is the persistence seam. PgStore implements it over pgx; tests
// substitute a fake.
type Store interface {
	LoadLimits(ctx context.Context) ([]Row, error)
	DailyUsage(ctx context.Context, accountID int64, day time.Time) (Usage, error)
	AddDailyVolume(ctx context.Context, accountID int64, day time.Time, delta decimal.Decimal) (decimal.Decimal, error)
	AddDailyWithdrawn(ctx context.Context, accountID int64, day time.Time, delta decimal.Decimal) (decimal.Decimal, error)
	OpenOrderCount(ctx context.Context, accountID int64) (int64, error)
	SymbolExposures(ctx context.Context, accountID int64) ([]SymbolExposure, error)
}

// Cache mirrors limits/usage into Redis for zero-latency gateway reads.
// Writes are best-effort: PostgreSQL remains authoritative, so a cache
// failure degrades gateway reads, never enforcement correctness.
type Cache interface {
	PublishLimits(ctx context.Context, rows []Row) error
	PublishUsage(ctx context.Context, accountID int64, day time.Time, u Usage) error
}

// ---------------------------------------------------------------------------
// Resolution — pure logic, unit-tested directly
// ---------------------------------------------------------------------------

func symbolMatches(rowSym *string, symbol string) bool {
	return rowSym == nil || *rowSym == "*" || *rowSym == symbol
}

// specificity ranks a candidate row for (accountID, tier, symbol):
// account match = 4, tier match = 2, exact symbol = 1. Rows that do not
// match return -1.
func specificity(r Row, accountID int64, tier, symbol string) int {
	if !symbolMatches(r.Symbol, symbol) {
		return -1
	}
	score := 0
	if r.AccountID != nil {
		if *r.AccountID != accountID {
			return -1
		}
		score += 4
	}
	if r.Tier != nil {
		if *r.Tier != tier {
			return -1
		}
		score += 2
	}
	if r.Symbol != nil && *r.Symbol == symbol {
		score += 1
	}
	return score
}

// Resolve computes the effective limits for (accountID, tier, symbol)
// from the loaded rows. Each column takes the value from the most
// specific row that sets it; exposure columns fall back to the spec §13.6
// defaults when nothing sets them.
func Resolve(rows []Row, accountID int64, tier, symbol string) EffectiveLimits {
	type cand struct {
		row   Row
		score int
	}
	var cands []cand
	for _, r := range rows {
		if s := specificity(r, accountID, tier, symbol); s >= 0 {
			cands = append(cands, cand{r, s})
		}
	}
	// Stable sort keeps deterministic precedence between equal scores
	// (lower table id wins, matching Load's ORDER BY id).
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].score > cands[j].score
	})

	var lim EffectiveLimits
	firstDec := func(get func(Row) *decimal.Decimal) *decimal.Decimal {
		for _, c := range cands {
			if v := get(c.row); v != nil {
				return v
			}
		}
		return nil
	}
	lim.MaxOrderQty = firstDec(func(r Row) *decimal.Decimal { return r.MaxOrderQty })
	lim.MaxDailyVolume = firstDec(func(r Row) *decimal.Decimal { return r.MaxDailyVolume })
	lim.DailyWithdrawLimit = firstDec(func(r Row) *decimal.Decimal { return r.DailyWithdrawLimit })
	lim.MaxWithdrawAmount = firstDec(func(r Row) *decimal.Decimal { return r.MaxWithdrawAmount })
	lim.WithdrawRatePerHour = firstDec(func(r Row) *decimal.Decimal { return r.WithdrawRatePerHour })
	for _, c := range cands {
		if c.row.MaxOpenOrders != nil {
			lim.MaxOpenOrders = c.row.MaxOpenOrders
			break
		}
	}

	// §13.6: exposure caps have canonical defaults — always resolved.
	if v := firstDec(func(r Row) *decimal.Decimal { return r.MaxNotionalExposure }); v != nil {
		lim.MaxNotionalExposure = v
	} else {
		d := DefaultSymbolNotionalCap
		lim.MaxNotionalExposure = &d
	}
	if v := firstDec(func(r Row) *decimal.Decimal { return r.MaxShortExposure }); v != nil {
		lim.MaxShortExposure = v
	} else {
		d := DefaultShortNotionalCap
		lim.MaxShortExposure = &d
	}
	if v := firstDec(func(r Row) *decimal.Decimal { return r.MaxAccountNotional }); v != nil {
		lim.MaxAccountNotional = v
	} else {
		d := DefaultAccountNotionalCap
		lim.MaxAccountNotional = &d
	}

	// §13.6a / Task 13.3.6: OTR columns carry canonical defaults too.
	if v := firstDec(func(r Row) *decimal.Decimal { return r.MaxOrderToTradeRatio }); v != nil {
		lim.MaxOrderToTradeRatio = v
	} else {
		d := DefaultOtrRatio
		lim.MaxOrderToTradeRatio = &d
	}
	for _, c := range cands {
		if c.row.OtrWindow != nil {
			lim.OtrWindow = *c.row.OtrWindow
			break
		}
	}
	if lim.OtrWindow <= 0 {
		lim.OtrWindow = DefaultOtrWindow
	}
	return lim
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// LimitsService enforces risk limits against the cached row set.
type LimitsService struct {
	store Store
	cache Cache // may be nil — Redis mirror is optional
	clock func() time.Time

	mu   sync.RWMutex
	rows []Row
}

// NewLimitsService builds the service. Call Load before enforcement.
// clock may be nil (defaults to time.Now) — injected for tests.
func NewLimitsService(store Store, cache Cache, clock func() time.Time) *LimitsService {
	if clock == nil {
		clock = time.Now
	}
	return &LimitsService{store: store, cache: cache, clock: clock}
}

// Load reads the full risk_limits table into memory and mirrors it to
// Redis. A store error is fatal to the load (fail-closed — stale limits
// would silently weaken enforcement); a cache write error only degrades
// gateway reads and is returned as a wrapped, non-fatal error after the
// in-memory set has been committed.
func (s *LimitsService) Load(ctx context.Context) error {
	rows, err := s.store.LoadLimits(ctx)
	if err != nil {
		return excerrors.Wrap(CodeRiskLimitsInternal, "load risk_limits", err)
	}
	s.mu.Lock()
	s.rows = rows
	s.mu.Unlock()
	if s.cache != nil {
		if err := s.cache.PublishLimits(ctx, rows); err != nil {
			return excerrors.Wrap(CodeRiskLimitsInternal, "publish risk_limits cache", err)
		}
	}
	return nil
}

// StartRefresher reloads limits every interval until ctx is cancelled —
// the simple time.Ticker pattern used elsewhere pending the Phase-07/09
// scheduler. Load errors are passed to onErr (e.g. slog) and the loop
// continues on the last good set.
func (s *LimitsService) StartRefresher(ctx context.Context, interval time.Duration, onErr func(error)) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.Load(ctx); err != nil && onErr != nil {
					onErr(err)
				}
			}
		}
	}()
}

// todayUTC returns the UTC calendar day the counters apply to.
func (s *LimitsService) todayUTC() time.Time {
	n := s.clock().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// EffectiveLimits resolves the limit set for (accountID, tier, symbol)
// against the currently loaded rows.
func (s *LimitsService) EffectiveLimits(accountID int64, tier, symbol string) EffectiveLimits {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Resolve(s.rows, accountID, tier, symbol)
}

// internalError wraps store failures. Enforcement callers must treat any
// error as a rejection (fail-closed).
func internalError(op string, err error) error {
	return excerrors.Wrap(CodeRiskLimitsInternal, op, err)
}

// CheckOrder enforces the per-account and per-symbol limits for an
// incoming order. It returns nil when the order is within all configured
// limits, or a coded *errors.Error on breach / internal failure —
// callers must reject on any non-nil result.
//
// Conservative exposure semantics (documented for Phase-19 refinement):
// any non-reduce-only order adds its notional to the symbol gross total
// and to the account total; a SELL additionally adds to the short
// total. Netting/hedging-aware exposure arrives with Task 19.3.15.
func (s *LimitsService) CheckOrder(ctx context.Context, req OrderRequest) error {
	lim := s.EffectiveLimits(req.AccountID, req.KycTier, req.Symbol)

	if !req.Quantity.IsPositive() {
		return excerrors.New(CodeOrderRejected, "order quantity must be positive")
	}
	if lim.MaxOrderQty != nil && req.Quantity.GreaterThan(*lim.MaxOrderQty) {
		return excerrors.New(CodeOrderRejected,
			fmt.Sprintf("order qty %s exceeds max_order_qty %s",
				req.Quantity, *lim.MaxOrderQty))
	}

	// Notional is needed whenever a daily-volume cap is configured or any
	// exposure check runs (exposure caps always resolve — §13.6 defaults —
	// so every non-reduce-only order needs a price).
	notionalNeeded := lim.MaxDailyVolume != nil || !req.ReduceOnly
	var notional decimal.Decimal
	if notionalNeeded {
		if !req.Price.IsPositive() {
			// A configured volume/exposure cap cannot evaluate a
			// price-less order — fail closed.
			return excerrors.New(CodeOrderRejected,
				"price required for notional limit check")
		}
		notional = req.Quantity.Mul(req.Price)
	}

	if lim.MaxDailyVolume != nil {
		u, err := s.store.DailyUsage(ctx, req.AccountID, s.todayUTC())
		if err != nil {
			return internalError("daily usage", err)
		}
		if u.Volume.Add(notional).GreaterThan(*lim.MaxDailyVolume) {
			return excerrors.New(CodeOrderRejected,
				fmt.Sprintf("daily volume %s + %s exceeds max_daily_volume %s",
					u.Volume, notional, *lim.MaxDailyVolume))
		}
	}

	if lim.MaxOpenOrders != nil {
		var open int64
		if req.OpenOrders != nil {
			open = *req.OpenOrders
		} else {
			var err error
			open, err = s.store.OpenOrderCount(ctx, req.AccountID)
			if err != nil {
				return internalError("open order count", err)
			}
		}
		if open+1 > int64(*lim.MaxOpenOrders) {
			return excerrors.New(CodeOrderRejected,
				fmt.Sprintf("open orders %d + 1 exceeds max_open_orders %d",
					open, *lim.MaxOpenOrders))
		}
	}

	if !req.ReduceOnly {
		exps, err := s.store.SymbolExposures(ctx, req.AccountID)
		if err != nil {
			return internalError("symbol exposures", err)
		}
		var sym SymbolExposure
		var accountGross decimal.Decimal
		for _, e := range exps {
			accountGross = accountGross.Add(e.GrossNotional)
			if e.Symbol == req.Symbol {
				sym = e
			}
		}
		if req.Side == "SELL" && lim.MaxShortExposure != nil {
			if sym.ShortNotional.Add(notional).GreaterThan(*lim.MaxShortExposure) {
				return excerrors.New(CodeMaxExposureExceeded,
					fmt.Sprintf("short exposure %s + %s exceeds max_short_exposure %s on %s",
						sym.ShortNotional, notional, *lim.MaxShortExposure, req.Symbol))
			}
		}
		if lim.MaxNotionalExposure != nil {
			if sym.GrossNotional.Add(notional).GreaterThan(*lim.MaxNotionalExposure) {
				return excerrors.New(CodeMaxExposureExceeded,
					fmt.Sprintf("symbol exposure %s + %s exceeds max_notional_exposure %s on %s",
						sym.GrossNotional, notional, *lim.MaxNotionalExposure, req.Symbol))
			}
		}
		if lim.MaxAccountNotional != nil {
			if accountGross.Add(notional).GreaterThan(*lim.MaxAccountNotional) {
				return excerrors.New(CodeMaxExposureExceeded,
					fmt.Sprintf("account exposure %s + %s exceeds max_account_notional %s",
						accountGross, notional, *lim.MaxAccountNotional))
			}
		}
	}
	return nil
}

// RecordFill adds traded notional to today's daily-volume counter and
// mirrors it to Redis. Called by the settlement pipeline on each fill.
func (s *LimitsService) RecordFill(ctx context.Context, accountID int64, notional decimal.Decimal) (decimal.Decimal, error) {
	day := s.todayUTC()
	total, err := s.store.AddDailyVolume(ctx, accountID, day, notional)
	if err != nil {
		return decimal.Zero, internalError("add daily volume", err)
	}
	if s.cache != nil {
		// Mirror failure degrades gateway reads only; PG already committed.
		_ = s.cache.PublishUsage(ctx, accountID, day, Usage{Volume: total})
	}
	return total, nil
}

// CheckWithdrawal enforces max_withdraw_amount (per transaction) and
// daily_withdraw_limit (against today's accumulated withdrawals).
// withdraw_rate_per_hour is surfaced in LimitsView for Phase-11, which
// owns the hourly-rate enforcement window.
func (s *LimitsService) CheckWithdrawal(ctx context.Context, accountID int64, tier string, amount decimal.Decimal) error {
	lim := s.EffectiveLimits(accountID, tier, "*")
	if !amount.IsPositive() {
		return excerrors.New(CodeOrderRejected, "withdrawal amount must be positive")
	}
	if lim.MaxWithdrawAmount != nil && amount.GreaterThan(*lim.MaxWithdrawAmount) {
		return excerrors.New(CodeOrderRejected,
			fmt.Sprintf("withdrawal %s exceeds max_withdraw_amount %s",
				amount, *lim.MaxWithdrawAmount))
	}
	if lim.DailyWithdrawLimit != nil {
		u, err := s.store.DailyUsage(ctx, accountID, s.todayUTC())
		if err != nil {
			return internalError("daily usage", err)
		}
		if u.Withdrawn.Add(amount).GreaterThan(*lim.DailyWithdrawLimit) {
			return excerrors.New(CodeOrderRejected,
				fmt.Sprintf("withdrawn %s + %s exceeds daily_withdraw_limit %s",
					u.Withdrawn, amount, *lim.DailyWithdrawLimit))
		}
	}
	return nil
}

// RecordWithdrawal adds amount to today's withdrawn counter.
func (s *LimitsService) RecordWithdrawal(ctx context.Context, accountID int64, amount decimal.Decimal) (decimal.Decimal, error) {
	day := s.todayUTC()
	total, err := s.store.AddDailyWithdrawn(ctx, accountID, day, amount)
	if err != nil {
		return decimal.Zero, internalError("add daily withdrawn", err)
	}
	if s.cache != nil {
		_ = s.cache.PublishUsage(ctx, accountID, day, Usage{Withdrawn: total})
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// API surface — Phase-05 Task 5.3.4 adapts this to the REST route.
// ---------------------------------------------------------------------------

// SymbolUtilization is one symbol's exposure usage against its caps.
type SymbolUtilization struct {
	Symbol           string
	GrossNotional    decimal.Decimal
	ShortNotional    decimal.Decimal
	MaxNotional      *decimal.Decimal
	MaxShort         *decimal.Decimal
	GrossNotionalPct *decimal.Decimal // nil when unlimited
	ShortNotionalPct *decimal.Decimal
}

// View is the GET /api/v1/account/risk-limits response model: the
// account's effective limits plus today's utilisation.
type View struct {
	AccountID int64
	Day       string // UTC calendar day the counters apply to
	Limits    EffectiveLimits
	Usage     Usage
	// Utilisation percentages; nil when the corresponding limit is unset.
	DailyVolumePct    *decimal.Decimal
	DailyWithdrawnPct *decimal.Decimal
	OpenOrders        int64
	OpenOrdersPct     *decimal.Decimal
	Symbols           []SymbolUtilization
}

func pct(used, max *decimal.Decimal) *decimal.Decimal {
	if used == nil || max == nil || max.IsZero() {
		return nil
	}
	p := used.Div(*max).Mul(decimal.NewFromInt(100))
	return &p
}

// LimitsView builds the limits+utilisation response for the account.
// kycTier selects tier-scoped rows during resolution.
func (s *LimitsService) LimitsView(ctx context.Context, accountID int64, kycTier string) (*View, error) {
	day := s.todayUTC()
	u, err := s.store.DailyUsage(ctx, accountID, day)
	if err != nil {
		return nil, internalError("daily usage", err)
	}
	open, err := s.store.OpenOrderCount(ctx, accountID)
	if err != nil {
		return nil, internalError("open order count", err)
	}
	exps, err := s.store.SymbolExposures(ctx, accountID)
	if err != nil {
		return nil, internalError("symbol exposures", err)
	}

	lim := s.EffectiveLimits(accountID, kycTier, "*")
	v := &View{
		AccountID:         accountID,
		Day:               day.Format("2006-01-02"),
		Limits:            lim,
		Usage:             u,
		DailyVolumePct:    pct(&u.Volume, lim.MaxDailyVolume),
		DailyWithdrawnPct: pct(&u.Withdrawn, lim.DailyWithdrawLimit),
		OpenOrders:        open,
	}
	if lim.MaxOpenOrders != nil {
		m := decimal.NewFromInt(int64(*lim.MaxOpenOrders))
		v.OpenOrdersPct = pct(openPtr(open), &m)
	}
	for _, e := range exps {
		sl := s.EffectiveLimits(accountID, kycTier, e.Symbol)
		v.Symbols = append(v.Symbols, SymbolUtilization{
			Symbol:           e.Symbol,
			GrossNotional:    e.GrossNotional,
			ShortNotional:    e.ShortNotional,
			MaxNotional:      sl.MaxNotionalExposure,
			MaxShort:         sl.MaxShortExposure,
			GrossNotionalPct: pct(&e.GrossNotional, sl.MaxNotionalExposure),
			ShortNotionalPct: pct(&e.ShortNotional, sl.MaxShortExposure),
		})
	}
	sort.Slice(v.Symbols, func(i, j int) bool { return v.Symbols[i].Symbol < v.Symbols[j].Symbol })
	return v, nil
}

func openPtr(v int64) *decimal.Decimal {
	d := decimal.NewFromInt(v)
	return &d
}

// ---------------------------------------------------------------------------
// PgStore — PostgreSQL implementation of Store
// ---------------------------------------------------------------------------

// PgStore implements Store over pgx. Numerics cross the wire as text
// (DECIMAL → ::text on read, string arg → ::numeric on write) because the
// shopspring-decimal pgtype shim is an external dep we do not take.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore wraps pool.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

// parseDec converts a numeric rendered as text into a Decimal.
func parseDec(s *string) (*decimal.Decimal, error) {
	if s == nil {
		return nil, nil
	}
	d, err := decimal.NewFromString(*s)
	if err != nil {
		return nil, fmt.Errorf("parse numeric %q: %w", *s, err)
	}
	return &d, nil
}

// LoadLimits reads every risk_limits row, lowest id first so equal-
// specificity ties resolve deterministically to the earlier row.
func (s *PgStore) LoadLimits(ctx context.Context) ([]Row, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, symbol, tier,
		       max_order_qty::text, max_daily_volume::text, max_open_orders,
		       daily_withdraw_limit::text, max_withdraw_amount::text,
		       withdraw_rate_per_hour::text,
		       max_notional_exposure::text, max_short_exposure::text,
		       max_account_notional::text,
		       max_order_to_trade_ratio::text,
		       EXTRACT(EPOCH FROM otr_window)::text
		FROM risk_limits ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var (
			r                                       Row
			moq, mdv, dwl, mwa, wrph, mne, mse, man *string
			otr, otrw                               *string
		)
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Symbol, &r.Tier,
			&moq, &mdv, &r.MaxOpenOrders, &dwl, &mwa, &wrph,
			&mne, &mse, &man, &otr, &otrw); err != nil {
			return nil, err
		}
		var scanErr error
		if r.MaxOrderQty, scanErr = parseDec(moq); scanErr != nil {
			return nil, scanErr
		}
		if r.MaxDailyVolume, scanErr = parseDec(mdv); scanErr != nil {
			return nil, scanErr
		}
		if r.DailyWithdrawLimit, scanErr = parseDec(dwl); scanErr != nil {
			return nil, scanErr
		}
		if r.MaxWithdrawAmount, scanErr = parseDec(mwa); scanErr != nil {
			return nil, scanErr
		}
		if r.WithdrawRatePerHour, scanErr = parseDec(wrph); scanErr != nil {
			return nil, scanErr
		}
		if r.MaxNotionalExposure, scanErr = parseDec(mne); scanErr != nil {
			return nil, scanErr
		}
		if r.MaxShortExposure, scanErr = parseDec(mse); scanErr != nil {
			return nil, scanErr
		}
		if r.MaxAccountNotional, scanErr = parseDec(man); scanErr != nil {
			return nil, scanErr
		}
		if r.MaxOrderToTradeRatio, scanErr = parseDec(otr); scanErr != nil {
			return nil, scanErr
		}
		if r.OtrWindow, scanErr = parseWindow(otrw); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// parseWindow converts an interval rendered as epoch-seconds text into a
// Duration (LoadLimits reads EXTRACT(EPOCH FROM otr_window)::text so the
// INTERVAL column crosses the wire without pgtype shimming).
func parseWindow(s *string) (*time.Duration, error) {
	if s == nil {
		return nil, nil
	}
	d, err := decimal.NewFromString(*s)
	if err != nil {
		return nil, fmt.Errorf("parse otr_window seconds %q: %w", *s, err)
	}
	w := time.Duration(d.Mul(decimal.NewFromInt(int64(time.Second))).IntPart())
	if w <= 0 {
		return nil, fmt.Errorf("otr_window %q resolves to a non-positive duration", *s)
	}
	return &w, nil
}

// DailyUsage returns today's usage row; an absent row is zero usage, not
// an error (the day simply has not started for this account).
func (s *PgStore) DailyUsage(ctx context.Context, accountID int64, day time.Time) (Usage, error) {
	var vol, wd *string
	err := s.pool.QueryRow(ctx, `
		SELECT volume::text, withdrawn::text
		FROM risk_daily_usage WHERE account_id = $1 AND day = $2`,
		accountID, day).Scan(&vol, &wd)
	if err == pgx.ErrNoRows {
		return Usage{Volume: decimal.Zero, Withdrawn: decimal.Zero}, nil
	}
	if err != nil {
		return Usage{}, err
	}
	v, err := parseDec(vol)
	if err != nil {
		return Usage{}, err
	}
	w, err := parseDec(wd)
	if err != nil {
		return Usage{}, err
	}
	u := Usage{Volume: decimal.Zero, Withdrawn: decimal.Zero}
	if v != nil {
		u.Volume = *v
	}
	if w != nil {
		u.Withdrawn = *w
	}
	return u, nil
}

func (s *PgStore) addDaily(ctx context.Context, accountID int64, day time.Time, col string, delta decimal.Decimal) (decimal.Decimal, error) {
	// Column name is a constant chosen by the caller — never user input.
	var txt string
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO risk_daily_usage (account_id, day, %[1]s)
		VALUES ($1, $2, $3::numeric)
		ON CONFLICT (account_id, day)
		DO UPDATE SET %[1]s = risk_daily_usage.%[1]s + EXCLUDED.%[1]s,
		            updated_at = now()
		RETURNING %[1]s::text`, col),
		accountID, day, delta.String()).Scan(&txt)
	if err != nil {
		return decimal.Zero, err
	}
	d, err := decimal.NewFromString(txt)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse %s %q: %w", col, txt, err)
	}
	return d, nil
}

// AddDailyVolume atomically increments today's volume counter.
func (s *PgStore) AddDailyVolume(ctx context.Context, accountID int64, day time.Time, delta decimal.Decimal) (decimal.Decimal, error) {
	return s.addDaily(ctx, accountID, day, "volume", delta)
}

// AddDailyWithdrawn atomically increments today's withdrawn counter.
func (s *PgStore) AddDailyWithdrawn(ctx context.Context, accountID int64, day time.Time, delta decimal.Decimal) (decimal.Decimal, error) {
	return s.addDaily(ctx, accountID, day, "withdrawn", delta)
}

// OpenOrderCount counts non-terminal orders (PENDING/RESERVED/ACTIVE/
// PARTIALLY_FILLED) — the full set of order statuses that occupy a slot.
func (s *PgStore) OpenOrderCount(ctx context.Context, accountID int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM orders
		WHERE account_id = $1
		  AND status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED')`,
		accountID).Scan(&n)
	return n, err
}

// SymbolExposures sums open positions per symbol. Gross = |qty| ×
// mark_price in quote currency; Short = SELL-side only. Quote-currency
// units until the Phase-19.5 USD-equivalent mark oracle lands — limits
// are configured in the same units, so enforcement stays consistent.
func (s *PgStore) SymbolExposures(ctx context.Context, accountID int64) ([]SymbolExposure, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.symbol,
		       COALESCE(SUM(ABS(p.quantity) * p.mark_price), 0)::text AS gross,
		       COALESCE(SUM(CASE WHEN p.side = 'SHORT'
		                         THEN ABS(p.quantity) * p.mark_price ELSE 0 END), 0)::text AS short
		FROM positions p
		JOIN instruments i ON i.id = p.instrument_id
		WHERE p.account_id = $1 AND p.quantity <> 0
		GROUP BY i.symbol`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SymbolExposure
	for rows.Next() {
		var e SymbolExposure
		var g, sh string
		if err := rows.Scan(&e.Symbol, &g, &sh); err != nil {
			return nil, err
		}
		gd, err := decimal.NewFromString(g)
		if err != nil {
			return nil, fmt.Errorf("parse gross %q: %w", g, err)
		}
		sd, err := decimal.NewFromString(sh)
		if err != nil {
			return nil, fmt.Errorf("parse short %q: %w", sh, err)
		}
		e.GrossNotional, e.ShortNotional = gd, sd
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// RedisLimitsCache — Redis mirror for gateway read-through
// ---------------------------------------------------------------------------

// RedisLimitsCache publishes the limits snapshot and daily counters under
// the risk_limits:* / risk:daily_* keys (spec §4 naming style).
type RedisLimitsCache struct {
	rdb *excredis.Client
}

// NewRedisLimitsCache wraps the coordination Redis client.
func NewRedisLimitsCache(rdb *excredis.Client) *RedisLimitsCache {
	return &RedisLimitsCache{rdb: rdb}
}

// rowJSON is the wire form of Row — pointers carry NULL semantics.
type rowJSON struct {
	ID                  int64   `json:"id"`
	AccountID           *int64  `json:"account_id,omitempty"`
	Symbol              *string `json:"symbol,omitempty"`
	Tier                *string `json:"tier,omitempty"`
	MaxOrderQty         *string `json:"max_order_qty,omitempty"`
	MaxDailyVolume      *string `json:"max_daily_volume,omitempty"`
	MaxOpenOrders       *int32  `json:"max_open_orders,omitempty"`
	DailyWithdrawLimit  *string `json:"daily_withdraw_limit,omitempty"`
	MaxWithdrawAmount   *string `json:"max_withdraw_amount,omitempty"`
	WithdrawRatePerHour *string `json:"withdraw_rate_per_hour,omitempty"`
	MaxNotionalExposure *string `json:"max_notional_exposure,omitempty"`
	MaxShortExposure    *string `json:"max_short_exposure,omitempty"`
	MaxAccountNotional  *string `json:"max_account_notional,omitempty"`
	// Migration 047 (RTS 9). OtrWindow crosses the wire as a Go duration
	// string ("60s") — the INTERVAL column renders the same human unit.
	MaxOrderToTradeRatio *string `json:"max_order_to_trade_ratio,omitempty"`
	OtrWindow            *string `json:"otr_window,omitempty"`
}

func durStr(w *time.Duration) *string {
	if w == nil {
		return nil
	}
	s := w.String()
	return &s
}

func decStr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

// PublishLimits replaces the risk_limits:rows snapshot atomically.
func (c *RedisLimitsCache) PublishLimits(ctx context.Context, rows []Row) error {
	wire := make([]rowJSON, 0, len(rows))
	for _, r := range rows {
		wire = append(wire, rowJSON{
			ID:                   r.ID,
			AccountID:            r.AccountID,
			Symbol:               r.Symbol,
			Tier:                 r.Tier,
			MaxOrderQty:          decStr(r.MaxOrderQty),
			MaxDailyVolume:       decStr(r.MaxDailyVolume),
			MaxOpenOrders:        r.MaxOpenOrders,
			DailyWithdrawLimit:   decStr(r.DailyWithdrawLimit),
			MaxWithdrawAmount:    decStr(r.MaxWithdrawAmount),
			WithdrawRatePerHour:  decStr(r.WithdrawRatePerHour),
			MaxNotionalExposure:  decStr(r.MaxNotionalExposure),
			MaxShortExposure:     decStr(r.MaxShortExposure),
			MaxAccountNotional:   decStr(r.MaxAccountNotional),
			MaxOrderToTradeRatio: decStr(r.MaxOrderToTradeRatio),
			OtrWindow:            durStr(r.OtrWindow),
		})
	}
	blob, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("marshal risk_limits rows: %w", err)
	}
	if err := c.rdb.Set(ctx, redisRowsKey, blob, 0).Err(); err != nil {
		return fmt.Errorf("redis set %s: %w", redisRowsKey, err)
	}
	return nil
}

// PublishUsage mirrors whichever daily fields are non-zero into the
// day-keyed counters with a 48h TTL (covers one full day plus margin).
func (c *RedisLimitsCache) PublishUsage(ctx context.Context, accountID int64, day time.Time, u Usage) error {
	pipe := c.rdb.TxPipeline()
	if !u.Volume.IsZero() {
		k := dailyVolumeKey(accountID, day)
		pipe.Set(ctx, k, u.Volume.String(), redisDailyUsageTTL)
	}
	if !u.Withdrawn.IsZero() {
		k := dailyWithdrawnKey(accountID, day)
		pipe.Set(ctx, k, u.Withdrawn.String(), redisDailyUsageTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis publish usage account %d: %w", accountID, err)
	}
	return nil
}
