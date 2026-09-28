// Pip Value Calculator Service — Task 3.3.12 (§24 #222; spec §6.3).
//
// Computes a standardized pip value for a currency pair, expressed in the
// requesting account's currency, for the margin engine, P&L calculator and
// Trader UI:
//
//	pip_value_quote = lots × contract_size × pip_size      (quote currency)
//
// Conversion into the account currency:
//   - account == quote (direct pair, EUR/USD from a USD account): identity.
//   - account == base  (EUR/USD from a EUR account): divide by the pair's
//     live mid price.
//   - otherwise (cross, EUR/GBP from a USD account): convert quote→account
//     through a single live mid-rate leg — "QUOTE/ACCOUNT" multiplies,
//     "ACCOUNT/QUOTE" divides. With no conversion leg the call fails
//     closed (PRICE_ORACLE_UNAVAILABLE); it never guesses a rate.
//
// Pip size comes from the instrument's decimal_places (pipette convention:
// 5dp → 0.0001, JPY-style 3dp → 0.01) or an explicit pip_size override.
//
// Results are cached per (symbol, account) unit lot for 1s (Redis on the
// evictable cache instance; an in-memory cache is provided for tests and
// degraded wiring). A cached entry carries the conversion rate used; a
// price move > 0.1% invalidates the entry even inside the TTL.
//
// Fail-closed (spec §2.7): a stale or missing price surfaces as
// MARK_PRICE_STALE / PRICE_ORACLE_UNAVAILABLE, never a recycled guess.
package settlement

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	goredis "github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// spec §23 machine-readable codes emitted by this service. Constants live
// here until Phase-05 Task 5.3.21 registers all emitted codes centrally;
// the strings are already canonical in §23.
const (
	codeInvalidRequest    = "INVALID_REQUEST"          // 400
	codeNotFound          = "NOT_FOUND"                // 404
	codeMarkPriceStale    = "MARK_PRICE_STALE"         // 503, L1
	codeOracleUnavailable = "PRICE_ORACLE_UNAVAILABLE" // 503, L1
)

// pipInvalidateMoveDec is pipInvalidateMove as a Decimal.
var pipInvalidateMoveDec = decimal.RequireFromString(pipInvalidateMove)

const (
	// StandardLotUnits is the standard FX lot: 100,000 units of base
	// currency. Used when instrument reference data carries no explicit
	// contract_size (migration 087 lands with Phase-15 Task 15.3.11).
	StandardLotUnits int64 = 100_000

	// DefaultPriceStalenessGate mirrors the canonical oracle staleness
	// gate (spec §19.5): a mid older than this is MARK_PRICE_STALE.
	DefaultPriceStalenessGate = 5 * time.Second

	// pipCacheTTL is the spec'd 1-second result cache window.
	pipCacheTTL = time.Second

	// pipInvalidateMove is the >0.1% relative price move that forces a
	// cached pip value to be recomputed ahead of TTL expiry.
	pipInvalidateMove = "0.001"

	// maxLots caps the lots parameter — a sanity bound, not a position limit.
	maxLots = 1_000_000
)

// ---------------------------------------------------------------------------
// Instrument reference
// ---------------------------------------------------------------------------

// InstrumentSpec is the slice of instrument reference data the calculator
// needs. It is deliberately independent of internal/models.Instrument
// (which Phase-15 fattens) so this service is stable across that work.
type InstrumentSpec struct {
	Symbol          string // canonical "BASE/QUOTE"
	Base            string
	Quote           string
	ContractSize    decimal.Decimal // base units per 1.0 lot; <=0 → StandardLotUnits
	DecimalPlaces   int             // price display dp; <=0 → JPY convention fallback
	PipSize         decimal.Decimal // explicit override; <=0 → derived
	SettlementCycle int             // informational; 0/1/2
}

// ContractSizeValue resolves the base-currency units per lot.
func (s InstrumentSpec) ContractSizeValue() decimal.Decimal {
	if s.ContractSize.IsPositive() {
		return s.ContractSize
	}
	return decimal.NewFromInt(StandardLotUnits)
}

// PipSizeValue resolves the pip size: explicit pip_size wins, then
// decimal_places (pip = one tenth of the minimum price increment —
// 5dp → 0.0001, 3dp → 0.01), then the market convention of 0.01 for
// JPY-quoted pairs and 0.0001 otherwise.
func (s InstrumentSpec) PipSizeValue() decimal.Decimal {
	if s.PipSize.IsPositive() {
		return s.PipSize
	}
	if s.DecimalPlaces > 0 {
		return decimal.New(1, -int32(s.DecimalPlaces-1))
	}
	if s.Quote == "JPY" {
		return decimal.New(1, -2) // 0.01
	}
	return decimal.New(1, -4) // 0.0001
}

// InstrumentProvider resolves instrument reference data by symbol.
type InstrumentProvider interface {
	Instrument(ctx context.Context, symbol string) (InstrumentSpec, error)
}

// StaticInstruments is a fixed in-memory InstrumentProvider — tests, and
// early wiring before Phase-15 lands instruments_reference.
type StaticInstruments map[string]InstrumentSpec

// Instrument implements InstrumentProvider.
func (m StaticInstruments) Instrument(_ context.Context, symbol string) (InstrumentSpec, error) {
	spec, ok := m[NormalizeSymbol(symbol)]
	if !ok {
		return InstrumentSpec{}, excerrors.New(codeNotFound,
			"unknown instrument "+symbol)
	}
	return spec, nil
}

// PGInstrumentProvider reads the instruments table (migration 001).
// decimal_places/pip_size/contract_size arrive with migration 087
// (Phase-15); until then DecimalPlaces stays 0 and the JPY convention
// derives the pip size.
type PGInstrumentProvider struct {
	Q Querier
}

// Instrument implements InstrumentProvider.
func (p PGInstrumentProvider) Instrument(ctx context.Context, symbol string) (InstrumentSpec, error) {
	sym := NormalizeSymbol(symbol)
	var s InstrumentSpec
	err := p.Q.QueryRow(ctx,
		`SELECT symbol, base_currency, quote_currency, settlement_cycle
		 FROM instruments WHERE symbol = $1`, sym).
		Scan(&s.Symbol, &s.Base, &s.Quote, &s.SettlementCycle)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return InstrumentSpec{}, excerrors.New(codeNotFound,
			"unknown instrument "+sym)
	}
	if err != nil {
		return InstrumentSpec{}, fmt.Errorf("instrument lookup %s: %w", sym, err)
	}
	return s, nil
}

// NormalizeSymbol canonicalizes "EURUSD"/"eurusd"/"EUR/USD" → "EUR/USD".
func NormalizeSymbol(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !strings.Contains(s, "/") && len(s) == 6 {
		s = s[:3] + "/" + s[3:]
	}
	return s
}

// ---------------------------------------------------------------------------
// Price provider (Phase-19.5 oracle seam)
// ---------------------------------------------------------------------------

// PriceProvider is the seam where the Phase-19.5 mark/index oracle plugs
// in. Implementations must fail closed: stale or absent mid → error.
type PriceProvider interface {
	MidPrice(ctx context.Context, symbol string) (decimal.Decimal, error)
}

// PriceTick is one observed price with its observation time.
type PriceTick struct {
	Mid decimal.Decimal
	At  time.Time
}

// LastTradePriceProvider is the Phase-19.5 oracle stub: the latest observed
// last-trade price per symbol, gated by a staleness bound. Replaced by the
// real ≥2-feed oracle when Phase-19.5 lands — the interface is the contract.
type LastTradePriceProvider struct {
	mu     sync.RWMutex
	ticks  map[string]PriceTick
	maxAge time.Duration
	now    func() time.Time
}

// NewLastTradePriceProvider returns a stub provider with the canonical 5s
// staleness gate. Use WithClock to inject a test clock.
func NewLastTradePriceProvider() *LastTradePriceProvider {
	return &LastTradePriceProvider{
		ticks:  map[string]PriceTick{},
		maxAge: DefaultPriceStalenessGate,
		now:    time.Now,
	}
}

// WithMaxAge overrides the staleness gate (<=0 keeps the 5s default).
func (p *LastTradePriceProvider) WithMaxAge(d time.Duration) *LastTradePriceProvider {
	if d > 0 {
		p.maxAge = d
	}
	return p
}

// WithClock injects a clock (tests).
func (p *LastTradePriceProvider) WithClock(now func() time.Time) *LastTradePriceProvider {
	if now != nil {
		p.now = now
	}
	return p
}

// SetLastTrade records the latest trade price for symbol at time at.
// Non-positive or non-finite prices are rejected — a corrupt tick must not
// age into a trusted quote.
func (p *LastTradePriceProvider) SetLastTrade(symbol string, price decimal.Decimal, at time.Time) error {
	if !price.IsPositive() {
		return excerrors.New(codeInvalidRequest,
			"price must be positive for "+symbol)
	}
	p.mu.Lock()
	p.ticks[NormalizeSymbol(symbol)] = PriceTick{Mid: price, At: at}
	p.mu.Unlock()
	return nil
}

// MidPrice implements PriceProvider. Missing symbol →
// PRICE_ORACLE_UNAVAILABLE; tick older than maxAge → MARK_PRICE_STALE.
func (p *LastTradePriceProvider) MidPrice(_ context.Context, symbol string) (decimal.Decimal, error) {
	sym := NormalizeSymbol(symbol)
	p.mu.RLock()
	tick, ok := p.ticks[sym]
	p.mu.RUnlock()
	if !ok {
		return decimal.Zero, excerrors.New(codeOracleUnavailable,
			"no price for "+sym)
	}
	if age := p.now().Sub(tick.At); age > p.maxAge || age < 0 {
		return decimal.Zero, excerrors.New(codeMarkPriceStale,
			fmt.Sprintf("price for %s is stale (age %s > gate %s)", sym, age, p.maxAge))
	}
	return tick.Mid, nil
}

// ---------------------------------------------------------------------------
// Pip value cache (Redis 1s TTL + >0.1% invalidation; memory for tests)
// ---------------------------------------------------------------------------

// pipCacheEntry is the cached unit (1.0 lot) pip value in account currency.
// RefPair/RefRate record the rate the value was computed with: a >0.1%
// move invalidates the entry inside its TTL. RefPair empty = direct pair,
// the value carries no price dependency and never needs invalidation.
type pipCacheEntry struct {
	UnitValue string `json:"v"` // decimal, account ccy per 1.0 lot
	RefPair   string `json:"p"` // conversion pair symbol, "" when direct
	RefRate   string `json:"r"` // decimal mid used, "" when direct
}

// PipCache stores pipCacheEntry blobs under "pipval:{symbol}:{account}".
// Get returns ("", nil) on miss. Implementations must not swallow errors —
// a broken cache backend surfaces to the caller (fail closed), which then
// computes fresh rather than trusting a hole.
type PipCache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
}

// RedisPipCache implements PipCache over go-redis STRINGs on the evictable
// cache instance (never the noeviction coordination instance — spec §4).
type RedisPipCache struct {
	C *goredis.Client
}

// NewRedisPipCache wraps a go-redis client.
func NewRedisPipCache(c *goredis.Client) *RedisPipCache {
	return &RedisPipCache{C: c}
}

// Get implements PipCache.
func (c *RedisPipCache) Get(ctx context.Context, key string) (string, error) {
	v, err := c.C.Get(ctx, key).Result()
	if stderrors.Is(err, goredis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pip cache get %s: %w", key, err)
	}
	return v, nil
}

// Set implements PipCache.
func (c *RedisPipCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if err := c.C.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("pip cache set %s: %w", key, err)
	}
	return nil
}

// MemoryPipCache is an in-process TTL cache for tests and for wiring where
// Redis is not yet attached.
type MemoryPipCache struct {
	mu  sync.Mutex
	m   map[string]memEntry
	now func() time.Time
}

type memEntry struct {
	value string
	exp   time.Time
}

// NewMemoryPipCache returns a memory cache; now may be nil (real clock).
func NewMemoryPipCache(now func() time.Time) *MemoryPipCache {
	if now == nil {
		now = time.Now
	}
	return &MemoryPipCache{m: map[string]memEntry{}, now: now}
}

// Get implements PipCache.
func (c *MemoryPipCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return "", nil
	}
	if !c.now().Before(e.exp) {
		delete(c.m, key)
		return "", nil
	}
	return e.value, nil
}

// Set implements PipCache.
func (c *MemoryPipCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = memEntry{value: value, exp: c.now().Add(ttl)}
	return nil
}

// ---------------------------------------------------------------------------
// Calculator
// ---------------------------------------------------------------------------

// PipCalculator computes account-currency pip values. Constructed with an
// instrument source, a price source, and a cache (nil disables caching —
// production wiring passes RedisPipCache on the evictable instance).
type PipCalculator struct {
	instruments InstrumentProvider
	prices      PriceProvider
	cache       PipCache
	ttl         time.Duration
}

// NewPipCalculator wires the service. instruments and prices are required —
// a calculator without a price source would have to guess cross rates,
// which is exactly what fail-closed forbids.
func NewPipCalculator(instruments InstrumentProvider, prices PriceProvider, cache PipCache) (*PipCalculator, error) {
	if instruments == nil {
		return nil, fmt.Errorf("pip calculator: nil instrument provider")
	}
	if prices == nil {
		return nil, fmt.Errorf("pip calculator: nil price provider")
	}
	return &PipCalculator{
		instruments: instruments,
		prices:      prices,
		cache:       cache,
		ttl:         pipCacheTTL,
	}, nil
}

// PipValueResult is the computed pip value plus audit trail.
type PipValueResult struct {
	Symbol           string          `json:"symbol"`
	Lots             decimal.Decimal `json:"lots"`
	AccountCurrency  string          `json:"account_currency"`
	PipSize          decimal.Decimal `json:"pip_size"`
	ContractSize     decimal.Decimal `json:"contract_size"`
	ValueQuoteCcy    decimal.Decimal `json:"pip_value_quote_ccy"` // raw pip value in quote ccy
	ValueAccountCcy  decimal.Decimal `json:"pip_value"`           // pip value in account ccy
	ConversionPair   string          `json:"conversion_pair"`     // "" when direct (no FX leg needed)
	ConversionRate   decimal.Decimal `json:"conversion_rate"`     // rate applied; 1 when direct
	ConversionDivide bool            `json:"conversion_divide"`   // rate divides (vs multiplies) value
	Cached           bool            `json:"cached"`
}

func pipCacheKey(symbol, account string) string {
	return "pipval:" + symbol + ":" + account
}

// PipValue computes the account-currency pip value for lots lots of symbol.
// symbol accepts "EUR/USD" or "EURUSD"; account must be a 3-letter code.
func (c *PipCalculator) PipValue(ctx context.Context, symbol string, lots decimal.Decimal, account string) (PipValueResult, error) {
	sym := NormalizeSymbol(symbol)
	account = strings.ToUpper(strings.TrimSpace(account))
	if !currencyRe.MatchString(account) {
		return PipValueResult{}, excerrors.New(codeInvalidRequest,
			"invalid account_currency "+account)
	}
	if !lots.IsPositive() || lots.GreaterThan(decimal.NewFromInt(maxLots)) {
		return PipValueResult{}, excerrors.New(codeInvalidRequest,
			"lots must be in (0, 1000000]")
	}

	inst, err := c.instruments.Instrument(ctx, sym)
	if err != nil {
		return PipValueResult{}, err
	}
	pipSize := inst.PipSizeValue()
	if !pipSize.IsPositive() {
		return PipValueResult{}, excerrors.New(codeInvalidRequest,
			"non-positive pip size for "+inst.Symbol)
	}
	contractSize := inst.ContractSizeValue()

	key := pipCacheKey(inst.Symbol, account)
	if ent, ok, err := c.cachedUnit(ctx, key, inst); err != nil {
		return PipValueResult{}, err
	} else if ok {
		unit, _ := decimal.NewFromString(ent.UnitValue)
		return c.result(inst, lots, account, pipSize, contractSize,
			ent, unit.Mul(lots), true), nil
	}

	unit, ent, err := c.computeUnit(ctx, inst, account)
	if err != nil {
		return PipValueResult{}, err
	}
	if err := c.storeUnit(ctx, key, ent); err != nil {
		return PipValueResult{}, err
	}
	return c.result(inst, lots, account, pipSize, contractSize, *ent, unit.Mul(lots), false), nil
}

// cachedUnit returns a still-valid cached unit value: TTL-valid AND, when
// the entry carries a price dependency, within the 0.1% invalidation band
// of the rate it was computed with. A price fetch failure propagates —
// we cannot validate freshness, so we cannot serve the cached value.
func (c *PipCalculator) cachedUnit(ctx context.Context, key string, inst InstrumentSpec) (pipCacheEntry, bool, error) {
	if c.cache == nil {
		return pipCacheEntry{}, false, nil
	}
	raw, err := c.cache.Get(ctx, key)
	if err != nil {
		return pipCacheEntry{}, false, err
	}
	if raw == "" {
		return pipCacheEntry{}, false, nil
	}
	var ent pipCacheEntry
	if err := json.Unmarshal([]byte(raw), &ent); err != nil {
		// Corrupt cache entry: drop it and recompute rather than serve
		// an unparseable value. Cache is advisory; the computation is truth.
		_ = c.cache.Set(ctx, key, "", 0)
		return pipCacheEntry{}, false, nil
	}
	if ent.RefPair == "" {
		if _, err := decimal.NewFromString(ent.UnitValue); err != nil {
			return pipCacheEntry{}, false, nil
		}
		return ent, true, nil // direct: no price dependency
	}
	cur, err := c.prices.MidPrice(ctx, ent.RefPair)
	if err != nil {
		return pipCacheEntry{}, false, err
	}
	ref, err := decimal.NewFromString(ent.RefRate)
	if err != nil || !ref.IsPositive() {
		return pipCacheEntry{}, false, nil
	}
	move := cur.Sub(ref).Div(ref).Abs()
	if move.GreaterThan(pipInvalidateMoveDec) {
		return pipCacheEntry{}, false, nil // >0.1% move: invalidate
	}
	return ent, true, nil
}

// computeUnit computes the 1.0-lot pip value in account currency and the
// cache record describing the rate it depends on.
func (c *PipCalculator) computeUnit(ctx context.Context, inst InstrumentSpec, account string) (decimal.Decimal, *pipCacheEntry, error) {
	raw := inst.ContractSizeValue().Mul(inst.PipSizeValue()) // quote ccy / lot

	switch account {
	case inst.Quote:
		// Direct: pip value is already denominated in the account ccy.
		return raw, &pipCacheEntry{UnitValue: raw.String()}, nil
	case inst.Base:
		// Account IS the base: quote-ccy pip value divided by the mid.
		mid, err := c.prices.MidPrice(ctx, inst.Symbol)
		if err != nil {
			return decimal.Zero, nil, err
		}
		if !mid.IsPositive() {
			return decimal.Zero, nil, excerrors.New(codeOracleUnavailable,
				"non-positive mid for "+inst.Symbol)
		}
		v := raw.Div(mid)
		return v, &pipCacheEntry{
			UnitValue: v.String(), RefPair: inst.Symbol, RefRate: mid.String(),
		}, nil
	default:
		// Cross: convert quote→account through one mid-rate leg.
		// "QUOTE/ACCOUNT" multiplies; "ACCOUNT/QUOTE" divides.
		for _, leg := range [][2]string{
			{inst.Quote + "/" + account, "mul"},
			{account + "/" + inst.Quote, "div"},
		} {
			mid, err := c.prices.MidPrice(ctx, leg[0])
			if err != nil {
				// Only a missing price tries the inverse leg; staleness or
				// provider failure is terminal (fail closed).
				var ee *excerrors.Error
				if stderrors.As(err, &ee) && ee.Code == codeOracleUnavailable {
					continue
				}
				return decimal.Zero, nil, err
			}
			if !mid.IsPositive() {
				continue
			}
			var v decimal.Decimal
			if leg[1] == "mul" {
				v = raw.Mul(mid)
			} else {
				v = raw.Div(mid)
			}
			return v, &pipCacheEntry{
				UnitValue: v.String(), RefPair: leg[0], RefRate: mid.String(),
			}, nil
		}
		return decimal.Zero, nil, excerrors.New(codeOracleUnavailable,
			fmt.Sprintf("no %s→%s conversion rate for %s pip value",
				inst.Quote, account, inst.Symbol))
	}
}

// storeUnit writes the cache entry; cache errors propagate (fail closed) —
// a silently broken cache would hide a stale-value bug elsewhere.
func (c *PipCalculator) storeUnit(ctx context.Context, key string, ent *pipCacheEntry) error {
	if c.cache == nil {
		return nil
	}
	blob, err := json.Marshal(ent)
	if err != nil {
		return fmt.Errorf("pip cache encode: %w", err)
	}
	return c.cache.Set(ctx, key, string(blob), c.ttl)
}

// result assembles the response record.
func (c *PipCalculator) result(inst InstrumentSpec, lots decimal.Decimal, account string,
	pipSize, contractSize decimal.Decimal, ent pipCacheEntry, value decimal.Decimal, cached bool) PipValueResult {
	res := PipValueResult{
		Symbol:          inst.Symbol,
		Lots:            lots,
		AccountCurrency: account,
		PipSize:         pipSize,
		ContractSize:    contractSize,
		ValueQuoteCcy:   contractSize.Mul(pipSize).Mul(lots),
		ValueAccountCcy: value,
		ConversionPair:  ent.RefPair,
		Cached:          cached,
	}
	if ent.RefPair == "" {
		res.ConversionRate = decimal.NewFromInt(1)
	} else {
		if r, err := decimal.NewFromString(ent.RefRate); err == nil {
			res.ConversionRate = r
		}
		res.ConversionDivide = ent.RefPair != "" &&
			strings.HasPrefix(ent.RefPair, account+"/")
	}
	return res
}

// ---------------------------------------------------------------------------
// REST handler — handler function only; Phase-05 Task 5.3.7 registers
// "GET /api/v1/instruments/{symbol}/pip-value" on the v1 router.
// ---------------------------------------------------------------------------

// PipValueHandler serves GET /api/v1/instruments/{symbol}/pip-value
// ?lots=<decimal>&account_currency=<ISO>. The {symbol} segment is read via
// r.PathValue, matching Go 1.22+ ServeMux pattern registration.
func (c *PipCalculator) PipValueHandler(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	q := r.URL.Query()

	lotsStr := q.Get("lots")
	if lotsStr == "" {
		lotsStr = "1"
	}
	lots, err := decimal.NewFromString(lotsStr)
	if err != nil {
		writePipError(w, excerrors.New(codeInvalidRequest, "invalid lots "+lotsStr))
		return
	}
	account := q.Get("account_currency")
	if account == "" {
		writePipError(w, excerrors.New(codeInvalidRequest,
			"account_currency is required"))
		return
	}

	res, err := c.PipValue(r.Context(), symbol, lots, account)
	if err != nil {
		writePipError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Decimal fields marshal as strings via shopspring's JSON marshaler.
	_ = json.NewEncoder(w).Encode(res)
}

// pipHTTPStatus maps the emitted spec §23 codes to HTTP status. Unknown
// codes collapse to 500 — an unclassified failure is never a client fault
// (fail-closed, spec §2.7.1).
func pipHTTPStatus(code string) int {
	switch code {
	case codeInvalidRequest:
		return http.StatusBadRequest
	case codeNotFound:
		return http.StatusNotFound
	case codeMarkPriceStale, codeOracleUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writePipError emits an RFC 7807 problem envelope with the §23 code.
func writePipError(w http.ResponseWriter, err error) {
	var ee *excerrors.Error
	if stderrors.As(err, &ee) {
		excerrors.NewProblem(pipHTTPStatus(ee.Code), ee.Code, ee.Code, ee.Message).WriteTo(w)
		return
	}
	excerrors.NewProblem(http.StatusInternalServerError,
		"INTERNAL_ERROR", "internal error", "").WriteTo(w)
}
