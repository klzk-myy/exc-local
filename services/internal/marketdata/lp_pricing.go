// Task 7.3.9 (distribution-side consumer) — per-LP pricing applied to
// market data (spec §5.44 item 3, §24 #227; migration 191
// lp_instrument_configs).
//
// LPs stream quotes into the venue (FIX Mass Quoting ingress lands with
// Phase-18 Task 18.3.7). Task 7.3.9's pricing config — which instruments
// an LP may quote, the per-instrument spread markup/skew, and the
// staleness timeout — is persisted by internal/admin (lp.go). This file
// is the consumer that makes the config real: LPQuoteSource →
// LPPriceFilter → lpBook@{lpID}/{symbol} frames emitted via the shared
// Server.Publish fanout, so the prices clients see carry exactly the
// configured markup/skew and dead quotes are pulled rather than
// distributed.
//
// Gate order (fail-closed per §2.7 — a quote that fails any gate is
// never distributed):
//
//   - configured: (lp_id, instrument_id) must exist in
//     lp_instrument_configs — the row IS the "which instruments they
//     quote" contract;
//   - LP ACTIVE: ONBOARDING/SUSPENDED LPs distribute nothing;
//   - enabled: an enabled=false row withdraws the book;
//   - staleness: the quote's own Ts must be inside
//     staleness_timeout_ms (per-instrument → LP default → 5s, the spec
//     §6.5 staleness gate family);
//   - sanity: adjusted prices must stay positive and the LP book must
//     not cross itself (CROSSED_BOOK_DETECTED lineage — a crossed LP
//     book is withdrawn, never shown).
//
// Adjusted books publish through LPBookProducer on the lpBook channel
// (ClassL2 budget — it IS book distribution, scoped per LP). Sequence,
// fanout and replay-ring resume come free through Server.Publish; the
// producer's Snapshot serves the resume/resync fallback
// (SetSnapshotSource("lpBook", …)).
//
// Config freshness: admins mutate lp_instrument_configs through the
// Task 7.3.9 API at runtime — the producer refreshes the filter's
// snapshot on RefreshInterval (default 30s, operator-tunable) and a
// refresh that withdraws a still-live book emits a stale-clearing frame
// so no client keeps showing a pulled LP's price. A second ticker
// sweeps live books whose quotes aged past their timeout between quote
// arrivals. Refresh failures keep the last-good snapshot (a transient
// PG read must not black out LP distribution); a filter that never
// loaded drops everything (unconfigured — never pass raw prices).
package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// DefaultLPQuoteStaleTimeout is the venue-level fallback staleness gate
// (spec §6.5 price-staleness family). It mirrors
// admin.DefaultLPStalenessTimeoutMS — duplicated because this package
// must not import internal/admin (the admin module already consumes
// nothing from marketdata; keeping the layering acyclic is deliberate).
const DefaultLPQuoteStaleTimeout = 5 * time.Second

// DefaultLPConfigRefresh is the producer's config-snapshot refresh
// cadence; DefaultLPStaleSweep bounds how long a book can outlive its
// quote timeout between arrivals.
const (
	DefaultLPConfigRefresh = 30 * time.Second
	DefaultLPStaleSweep    = time.Second
)

// LPQuoteLevel is one side/level of an LP's quote — decimal prices and
// quantities (FIX/REST quote feeds are decimal; the 1e-8 wire scale is
// an engine artifact that doesn't reach this seam).
type LPQuoteLevel struct {
	Price decimal.Decimal
	Qty   decimal.Decimal
}

// LPQuote is the transport-neutral inbound quote event handed to the
// pricing filter. InstrumentID/Symbol resolution is upstream of this
// seam (either may be populated — the config row's symbol wins on
// conflict). Ts is the LP-side quote timestamp and anchors the
// staleness gate; Seq is the upstream quote sequence (diagnostics —
// lpBook channel seqs are channel-scoped, same as every Wave-2 feed).
type LPQuote struct {
	LPID         int64
	InstrumentID int64
	Symbol       string
	Bids         []LPQuoteLevel // best-first
	Asks         []LPQuoteLevel // best-first
	Seq          uint64
	Ts           time.Time
}

// LPQuoteSource yields raw (unmarked) LP quotes until ctx is cancelled;
// the channel closes on termination. Mirrors DeltaSource/TradeSource —
// implementations own their drop policy.
type LPQuoteSource interface {
	Quotes(ctx context.Context) (<-chan LPQuote, error)
}

// LPQuoteSourceFunc adapts a function to LPQuoteSource.
type LPQuoteSourceFunc func(ctx context.Context) (<-chan LPQuote, error)

// Quotes implements LPQuoteSource.
func (f LPQuoteSourceFunc) Quotes(ctx context.Context) (<-chan LPQuote, error) {
	return f(ctx)
}

// ---------------------------------------------------------------------------
// Pricing config — the lp_instrument_configs consumer
// ---------------------------------------------------------------------------

// LPPricingConfig is one lp_instrument_configs row joined to its LP —
// the venue-side pricing contract for one (lp, instrument) pair.
// Staleness resolution: per-instrument StalenessTimeoutMS wins; 0 falls
// back to the LP-level default; both unset → DefaultLPQuoteStaleTimeout.
type LPPricingConfig struct {
	LPID   int64
	Status string // LP lifecycle: ONBOARDING | ACTIVE | SUSPENDED
	// LPStalenessMS is the LP-level staleness default
	// (liquidity_providers.staleness_timeout_ms).
	LPStalenessMS int

	InstrumentID int64
	Symbol       string // resolved from instruments; "" when unresolvable
	Enabled      bool
	// SpreadMarkupBidBps is added to the LP bid before distribution;
	// SpreadMarkupAskBps is subtracted from the LP ask (stored positive
	// — Task 7.3.9 item 6); SkewBps is the signed mid shift applied to
	// both sides before the markup.
	SpreadMarkupBidBps decimal.Decimal
	SpreadMarkupAskBps decimal.Decimal
	SkewBps            decimal.Decimal
	StalenessTimeoutMS int
}

// staleTimeout resolves the effective staleness gate for this config.
func (c LPPricingConfig) staleTimeout() time.Duration {
	ms := c.StalenessTimeoutMS
	if ms <= 0 {
		ms = c.LPStalenessMS
	}
	if ms <= 0 {
		return DefaultLPQuoteStaleTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// LPConfigSource loads the persisted pricing config. The production
// implementation is PgxLPConfigSource (migration 191); tests substitute
// a static list.
type LPConfigSource interface {
	LPConfigs(ctx context.Context) ([]LPPricingConfig, error)
}

// LPConfigSourceFunc adapts a function to LPConfigSource.
type LPConfigSourceFunc func(ctx context.Context) ([]LPPricingConfig, error)

// LPConfigs implements LPConfigSource.
func (f LPConfigSourceFunc) LPConfigs(ctx context.Context) ([]LPPricingConfig, error) {
	return f(ctx)
}

// PgxLPConfigSource reads lp_instrument_configs JOIN liquidity_providers
// (LEFT JOIN instruments for the display symbol). Every LP status is
// returned — the filter needs suspended/onboarding rows present so a
// refresh can withdraw their books (an absent row would look identical
// to a never-configured LP).
type PgxLPConfigSource struct {
	pool *pgxpool.Pool
}

// NewPgxLPConfigSource binds the pool.
func NewPgxLPConfigSource(pool *pgxpool.Pool) *PgxLPConfigSource {
	return &PgxLPConfigSource{pool: pool}
}

// LPConfigs implements LPConfigSource.
func (s *PgxLPConfigSource) LPConfigs(ctx context.Context) ([]LPPricingConfig, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.lp_id, p.status, p.staleness_timeout_ms,
		       c.instrument_id, COALESCE(i.symbol, ''),
		       c.enabled,
		       c.spread_markup_bid_bps::text, c.spread_markup_ask_bps::text,
		       c.skew_bps::text, c.staleness_timeout_ms
		  FROM lp_instrument_configs c
		  JOIN liquidity_providers p ON p.lp_id = c.lp_id
		  LEFT JOIN instruments i ON i.id = c.instrument_id
		 ORDER BY c.lp_id, c.instrument_id`)
	if err != nil {
		return nil, fmt.Errorf("marketdata: lp config query: %w", err)
	}
	defer rows.Close()
	out := []LPPricingConfig{}
	for rows.Next() {
		var c LPPricingConfig
		var bid, ask, skew string
		if err := rows.Scan(&c.LPID, &c.Status, &c.LPStalenessMS,
			&c.InstrumentID, &c.Symbol, &c.Enabled,
			&bid, &ask, &skew, &c.StalenessTimeoutMS); err != nil {
			return nil, fmt.Errorf("marketdata: lp config scan: %w", err)
		}
		if c.SpreadMarkupBidBps, err = decimal.NewFromString(bid); err != nil {
			return nil, fmt.Errorf("marketdata: lp config bid markup %q: %w", bid, err)
		}
		if c.SpreadMarkupAskBps, err = decimal.NewFromString(ask); err != nil {
			return nil, fmt.Errorf("marketdata: lp config ask markup %q: %w", ask, err)
		}
		if c.SkewBps, err = decimal.NewFromString(skew); err != nil {
			return nil, fmt.Errorf("marketdata: lp config skew %q: %w", skew, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// LPPriceFilter — gate + transform
// ---------------------------------------------------------------------------

// lpPricingKey / lpPricingSymKey index the config snapshot — instrument
// id primary, (lp, symbol) secondary for feeds that key quotes by symbol.
type lpPricingKey struct {
	lpID, instrumentID int64
}
type lpPricingSymKey struct {
	lpID   int64
	symbol string
}

// LP drop reasons — stable vocabulary for metrics, logs and the
// reason field on withdrawn lpBook frames.
const (
	LPDropUnconfigured = "unconfigured" // no (lp, instrument) config row
	LPDropLPInactive   = "lp_inactive"  // LP not ACTIVE
	LPDropDisabled     = "disabled"     // enabled=false
	LPDropStale        = "stale"        // quote older than staleness timeout
	LPDropUnresolved   = "unresolved"   // no distributable symbol
	LPDropCrossed      = "crossed"      // adjusted book crossed itself
	LPDropNonPositive  = "nonpositive"  // markup pushed a price ≤ 0
)

var lpBpsDivisor = decimal.NewFromInt(10_000)

// LPPriceFilter holds the last-good pricing-config snapshot and applies
// it to inbound quotes. An empty snapshot drops everything — the config
// row is what entitles an LP to be distributed, so its absence must not
// pass raw prices.
type LPPriceFilter struct {
	src LPConfigSource
	log *slog.Logger

	mu     sync.RWMutex
	byID   map[lpPricingKey]LPPricingConfig
	bySym  map[lpPricingSymKey]LPPricingConfig
	loaded bool
}

// NewLPPriceFilter binds a config source; src may be nil for tests
// driving SetConfigs directly.
func NewLPPriceFilter(src LPConfigSource, log *slog.Logger) *LPPriceFilter {
	if log == nil {
		log = slog.Default()
	}
	return &LPPriceFilter{
		src: src, log: log,
		byID:  map[lpPricingKey]LPPricingConfig{},
		bySym: map[lpPricingSymKey]LPPricingConfig{},
	}
}

// SetConfigs swaps the snapshot — the test seam and Reload's tail.
func (f *LPPriceFilter) SetConfigs(cfgs []LPPricingConfig) {
	byID := make(map[lpPricingKey]LPPricingConfig, len(cfgs))
	bySym := make(map[lpPricingSymKey]LPPricingConfig, len(cfgs))
	for _, c := range cfgs {
		byID[lpPricingKey{c.LPID, c.InstrumentID}] = c
		if c.Symbol != "" {
			bySym[lpPricingSymKey{c.LPID, c.Symbol}] = c
		}
	}
	f.mu.Lock()
	f.byID, f.bySym, f.loaded = byID, bySym, true
	f.mu.Unlock()
}

// Loaded reports whether any snapshot (even empty) has been installed —
// distinguishes "no rows configured" from "never loaded".
func (f *LPPriceFilter) Loaded() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.loaded
}

// Reload re-reads the config source into the snapshot. On error the
// last-good snapshot is kept (returned err is non-nil so callers can
// count/log the failure) — a transient read must not black out LP
// distribution.
func (f *LPPriceFilter) Reload(ctx context.Context) error {
	if f.src == nil {
		return nil
	}
	cfgs, err := f.src.LPConfigs(ctx)
	if err != nil {
		return fmt.Errorf("marketdata: lp config reload: %w", err)
	}
	f.SetConfigs(cfgs)
	return nil
}

// lookup resolves the config row for a quote. An explicit instrument_id
// wins outright — a positive id that matches no row is unconfigured,
// NOT symbol-fallback material (a mislabeled id must not inherit a
// different instrument's pricing). The (lp, symbol) index serves only
// symbol-keyed feeds that carry no instrument id at all.
func (f *LPPriceFilter) lookup(q LPQuote) (LPPricingConfig, bool) {
	if q.LPID <= 0 {
		return LPPricingConfig{}, false
	}
	if q.InstrumentID > 0 {
		c, ok := f.byID[lpPricingKey{q.LPID, q.InstrumentID}]
		return c, ok
	}
	if q.Symbol != "" {
		if c, ok := f.bySym[lpPricingSymKey{q.LPID, q.Symbol}]; ok {
			return c, true
		}
	}
	return LPPricingConfig{}, false
}

// ConfigFor exposes the resolved row (instrument-id lookup) — admin
// readback surfaces and tests verify what distribution would apply.
func (f *LPPriceFilter) ConfigFor(lpID, instrumentID int64) (LPPricingConfig, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	c, ok := f.byID[lpPricingKey{lpID, instrumentID}]
	return c, ok
}

// Gate reports whether (lp, instrument) is currently distributable —
// config present, LP ACTIVE, enabled. Staleness is NOT part of the gate
// (it is a property of a quote, not the config); the producer uses Gate
// on refresh to withdraw live books whose config went away.
// Returns the config row and "" when distributable.
func (f *LPPriceFilter) Gate(lpID, instrumentID int64) (LPPricingConfig, string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	c, ok := f.byID[lpPricingKey{lpID, instrumentID}]
	if !ok {
		return LPPricingConfig{}, LPDropUnconfigured
	}
	return gateLPConfig(c)
}

func gateLPConfig(c LPPricingConfig) (LPPricingConfig, string) {
	if c.Status != "ACTIVE" {
		return c, LPDropLPInactive
	}
	if !c.Enabled {
		return c, LPDropDisabled
	}
	return c, ""
}

// Apply gates one quote and returns the distribution-ready copy with
// markup/skew applied. Reason "" means the returned quote may be
// distributed; otherwise it is the LPDrop* vocabulary. The input is
// never mutated.
//
// Pricing math (Task 7.3.9 item 6 + migration 191 column notes):
//
//	bid′ = bid × (1 + (skew + markup_bid)/10⁴)
//	ask′ = ask × (1 + (skew − markup_ask)/10⁴)
//
// — skew shifts both sides (signed inventory lean), the bid markup is
// added to the bid and the ask markup subtracted from the ask.
func (f *LPPriceFilter) Apply(q LPQuote, now time.Time) (LPQuote, LPPricingConfig, string) {
	f.mu.RLock()
	cfg, ok := f.lookup(q)
	f.mu.RUnlock()
	if !ok {
		return LPQuote{}, LPPricingConfig{}, LPDropUnconfigured
	}
	return applyLPConfig(cfg, q, now)
}

func applyLPConfig(cfg LPPricingConfig, q LPQuote, now time.Time) (LPQuote, LPPricingConfig, string) {
	var zero LPQuote
	if _, reason := gateLPConfig(cfg); reason != "" {
		return zero, cfg, reason
	}
	if q.Ts.IsZero() || now.Sub(q.Ts) > cfg.staleTimeout() {
		return zero, cfg, LPDropStale
	}

	sym := cfg.Symbol
	if sym == "" {
		sym = q.Symbol
	}
	if sym == "" {
		return zero, cfg, LPDropUnresolved
	}

	bidF := decimal.One.Add(cfg.SkewBps.Add(cfg.SpreadMarkupBidBps).Div(lpBpsDivisor))
	askF := decimal.One.Add(cfg.SkewBps.Sub(cfg.SpreadMarkupAskBps).Div(lpBpsDivisor))

	adj := LPQuote{
		LPID: cfg.LPID, InstrumentID: cfg.InstrumentID, Symbol: sym,
		Bids: make([]LPQuoteLevel, len(q.Bids)),
		Asks: make([]LPQuoteLevel, len(q.Asks)),
		Seq:  q.Seq, Ts: q.Ts,
	}
	for i, l := range q.Bids {
		p := l.Price.Mul(bidF)
		if p.Sign() <= 0 {
			return zero, cfg, LPDropNonPositive
		}
		adj.Bids[i] = LPQuoteLevel{Price: p, Qty: l.Qty}
	}
	for i, l := range q.Asks {
		p := l.Price.Mul(askF)
		if p.Sign() <= 0 {
			return zero, cfg, LPDropNonPositive
		}
		adj.Asks[i] = LPQuoteLevel{Price: p, Qty: l.Qty}
	}
	// A markup/skew combination may cross the LP's own book — a crossed
	// book is withdrawn, never distributed (CROSSED_BOOK_DETECTED
	// lineage; strictly greater, a locked market is still honest).
	if len(adj.Bids) > 0 && len(adj.Asks) > 0 &&
		adj.Bids[0].Price.GreaterThan(adj.Asks[0].Price) {
		return zero, cfg, LPDropCrossed
	}
	return adj, cfg, ""
}

// ---------------------------------------------------------------------------
// lpBook@{lpID}/{symbol} channel + producer
// ---------------------------------------------------------------------------

// LPBookChannel renders the distribution channel for one (lp, symbol)
// pair — lpBook@7/EUR/USD. The numeric LP id prefixes the canonical
// symbol; ParseChannel validates the shape and exposes both halves.
func LPBookChannel(lpID int64, symbol string) string {
	return fmt.Sprintf("lpBook@%d/%s", lpID, symbol)
}

// lpBookFrame is the emitted lpBook@ payload. On a fresh quote it
// carries the adjusted best-first levels; on a withdrawal (stale quote,
// config gate flipped, crossed/nonpositive) it carries empty sides,
// stale:true and a wire-safe reason — the client must never keep
// showing a price the venue can no longer stand behind.
type lpBookFrame struct {
	Event        string     `json:"event"` // "lpBook"
	LPID         int64      `json:"lp_id"`
	InstrumentID int64      `json:"instrument_id"`
	Symbol       string     `json:"symbol"`
	Bids         [][]string `json:"bids"` // [price, qty] decimal strings
	Asks         [][]string `json:"asks"`
	Seq          uint64     `json:"seq"`              // channel-scoped (md:seq domain)
	QuoteSeq     uint64     `json:"quote_seq"`        // upstream LP seq, 0 when unknown
	QuoteTsMs    int64      `json:"quote_ts_ms"`      // LP-side ts — freshness evidence
	Stale        bool       `json:"stale"`            // true on withdrawal frames
	Reason       string     `json:"reason,omitempty"` // LPDrop* vocabulary on stale frames
	TsMs         int64      `json:"ts_ms"`            // emit time
}

// lpBookState is one channel's bookkeeping.
type lpBookState struct {
	lpID, instrumentID int64
	symbol             string
	stale              bool          // last emitted frame was a withdrawal
	seq                uint64        // last emitted channel seq
	quoteTs            time.Time     // last distributed quote's ts
	timeout            time.Duration // resolved staleness gate
	frame              lpBookFrame   // verbatim last frame (Snapshot)
}

// LPBookProducerConfig tunes LPBookProducer.
type LPBookProducerConfig struct {
	Logger      *slog.Logger
	Now         func() time.Time
	InputBuffer int // Push() channel depth; default 8192
	// RefreshInterval re-reads the LPConfigSource — admin pricing changes
	// propagate without restart. Zero picks the 30s default; negative
	// disables the ticker (Refresh stays available as the manual seam).
	RefreshInterval time.Duration
	// StaleSweep clears live books whose quotes aged past their timeout
	// between arrivals (silence is not freshness). Zero picks the 1s
	// default; negative disables.
	StaleSweep time.Duration
}

func (c *LPBookProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.InputBuffer <= 0 {
		c.InputBuffer = 8192
	}
	if c.RefreshInterval < 0 {
		c.RefreshInterval = 0
	} else if c.RefreshInterval == 0 {
		c.RefreshInterval = DefaultLPConfigRefresh
	}
	if c.StaleSweep < 0 {
		c.StaleSweep = 0
	} else if c.StaleSweep == 0 {
		c.StaleSweep = DefaultLPStaleSweep
	}
}

// LPBookProducer consumes LP quotes, applies LPPriceFilter and fans the
// survivors onto lpBook@{lpID}/{symbol}. src may be nil (drive via
// Push); filter must be non-nil (it IS the feature this producer exists
// to apply).
type LPBookProducer struct {
	cfg    LPBookProducerConfig
	src    LPQuoteSource
	filter *LPPriceFilter
	emit   EmitFunc
	seq    *seqAllocator
	in     chan LPQuote

	mu     sync.Mutex
	states map[string]*lpBookState // channel → last emitted state

	// OnDrop observes gated quotes (LPDrop* reason) — metrics wiring is
	// the caller's seam; nil is fine.
	OnDrop func(reason string)
}

// NewLPBookProducer wires the producer; emit is Server.Publish.
func NewLPBookProducer(cfg LPBookProducerConfig, src LPQuoteSource,
	filter *LPPriceFilter, emit EmitFunc) *LPBookProducer {
	cfg.defaults()
	return &LPBookProducer{
		cfg: cfg, src: src, filter: filter, emit: emit,
		seq: newSeqAllocator(), in: make(chan LPQuote, cfg.InputBuffer),
		states: map[string]*lpBookState{},
	}
}

// Push injects a quote directly (tests/embedders). Saturated input
// drops loudly — a dropped quote can hide a top-of-book change.
func (p *LPBookProducer) Push(q LPQuote) {
	select {
	case p.in <- q:
	default:
		p.cfg.Logger.Error("marketdata: lpbook input saturated — quote dropped",
			"lp", q.LPID, "instrument", q.InstrumentID, "symbol", q.Symbol)
		if p.OnDrop != nil {
			p.OnDrop("input_saturation")
		}
	}
}

func renderLPLevels(lv []LPQuoteLevel) [][]string {
	out := make([][]string, 0, len(lv))
	for _, l := range lv {
		out = append(out, []string{l.Price.String(), l.Qty.String()})
	}
	return out
}

// process applies the filter to one quote and emits the result. Gated
// quotes withdraw a live book (the last distributed price is no longer
// one the venue stands behind); gated quotes for never-live channels
// stay silent.
func (p *LPBookProducer) process(q LPQuote, now time.Time) {
	adj, cfg, reason := p.filter.Apply(q, now)

	// Resolve the channel identity: config row wins, quote fields
	// otherwise (covers the unconfigured drop too).
	lpID, instrID, sym := q.LPID, q.InstrumentID, q.Symbol
	if cfg.LPID > 0 {
		lpID, instrID = cfg.LPID, cfg.InstrumentID
		if cfg.Symbol != "" {
			sym = cfg.Symbol
		}
	}
	if sym == "" {
		if p.OnDrop != nil {
			p.OnDrop(LPDropUnresolved)
		}
		return
	}
	ch := LPBookChannel(lpID, sym)

	if reason != "" {
		if p.OnDrop != nil {
			p.OnDrop(reason)
		}
		p.withdraw(ch, lpID, instrID, sym, reason, q.Ts, now)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stateLocked(ch, lpID, instrID, sym)
	seq := p.seq.next(ch)
	frame := lpBookFrame{
		Event: "lpBook", LPID: lpID, InstrumentID: instrID, Symbol: sym,
		Bids: renderLPLevels(adj.Bids), Asks: renderLPLevels(adj.Asks),
		Seq: seq, QuoteSeq: adj.Seq, QuoteTsMs: adj.Ts.UnixMilli(),
		Stale: false, TsMs: now.UnixMilli(),
	}
	st.stale, st.seq, st.quoteTs = false, seq, adj.Ts
	st.timeout = cfg.staleTimeout()
	st.frame = frame
	p.emit(ch, seq, frame)
}

// withdraw emits the stale-clearing frame when the channel currently
// shows a live book; already-withdrawn (or never-live) channels emit
// nothing — silence is honest for a book that was never distributed.
func (p *LPBookProducer) withdraw(ch string, lpID, instrID int64,
	sym, reason string, quoteTs, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[ch]
	if st == nil || st.stale {
		return
	}
	seq := p.seq.next(ch)
	frame := lpBookFrame{
		Event: "lpBook", LPID: lpID, InstrumentID: instrID, Symbol: sym,
		Bids: [][]string{}, Asks: [][]string{},
		Seq: seq, Stale: true, Reason: reason,
		TsMs: now.UnixMilli(),
	}
	if !quoteTs.IsZero() {
		frame.QuoteTsMs = quoteTs.UnixMilli()
	}
	st.stale, st.seq, st.frame = true, seq, frame
	p.emit(ch, seq, frame)
}

// stateLocked returns/creates the channel state. Caller holds p.mu.
func (p *LPBookProducer) stateLocked(ch string, lpID, instrID int64, sym string) *lpBookState {
	st := p.states[ch]
	if st == nil {
		st = &lpBookState{lpID: lpID, instrumentID: instrID, symbol: sym}
		p.states[ch] = st
	}
	return st
}

// Refresh reloads the pricing snapshot and withdraws live books whose
// config is no longer distributable (LP suspended, enabled=false, row
// deleted). Exported so ops can force a reload; Run also calls it on
// RefreshInterval.
func (p *LPBookProducer) Refresh(ctx context.Context) error {
	err := p.filter.Reload(ctx)
	if err != nil {
		p.cfg.Logger.Error("marketdata: lp config refresh failed — keeping last snapshot",
			"err", err)
	}
	// Withdraw now-undistributable live books even when the reload
	// failed — Gate reads the last-good snapshot, which is still the
	// venue's best knowledge of what may be distributed.
	p.mu.Lock()
	type pending struct {
		ch     string
		reason string
		st     *lpBookState
	}
	var clears []pending
	for ch, st := range p.states {
		if st.stale {
			continue
		}
		if _, reason := p.filter.Gate(st.lpID, st.instrumentID); reason != "" {
			clears = append(clears, pending{ch, reason, st})
		}
	}
	for _, c := range clears {
		seq := p.seq.next(c.ch)
		frame := lpBookFrame{
			Event: "lpBook", LPID: c.st.lpID, InstrumentID: c.st.instrumentID,
			Symbol: c.st.symbol, Bids: [][]string{}, Asks: [][]string{},
			Seq: seq, Stale: true, Reason: c.reason,
			QuoteTsMs: c.st.quoteTs.UnixMilli(),
			TsMs:      p.cfg.Now().UnixMilli(),
		}
		c.st.stale, c.st.seq, c.st.frame = true, seq, frame
		p.emit(c.ch, seq, frame)
	}
	p.mu.Unlock()
	return err
}

// sweepStale withdraws live books whose last distributed quote aged
// past its staleness timeout — the no-arrival path (a dead feed never
// sends the stale quote that would trigger process()).
func (p *LPBookProducer) sweepStale(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch, st := range p.states {
		if st.stale || st.timeout <= 0 || st.quoteTs.IsZero() {
			continue
		}
		if now.Sub(st.quoteTs) <= st.timeout {
			continue
		}
		seq := p.seq.next(ch)
		frame := lpBookFrame{
			Event: "lpBook", LPID: st.lpID, InstrumentID: st.instrumentID,
			Symbol: st.symbol, Bids: [][]string{}, Asks: [][]string{},
			Seq: seq, Stale: true, Reason: LPDropStale,
			QuoteTsMs: st.quoteTs.UnixMilli(),
			TsMs:      now.UnixMilli(),
		}
		st.stale, st.seq, st.frame = true, seq, frame
		p.emit(ch, seq, frame)
	}
}

// Run drives the producer until ctx is cancelled: initial config load,
// quote processing, config refresh and the staleness sweep.
func (p *LPBookProducer) Run(ctx context.Context) error {
	var srcCh <-chan LPQuote
	if p.src != nil {
		ch, err := p.src.Quotes(ctx)
		if err != nil {
			return fmt.Errorf("marketdata: lp quote source: %w", err)
		}
		srcCh = ch
	}
	// First load before the first quote — startup ordering keeps a
	// just-booted process from dropping quotes while PG hasn't answered.
	if err := p.filter.Reload(ctx); err != nil {
		p.cfg.Logger.Error("marketdata: initial lp config load failed — "+
			"quotes drop until a snapshot lands", "err", err)
	}
	var refreshC <-chan time.Time
	if p.cfg.RefreshInterval > 0 {
		t := time.NewTicker(p.cfg.RefreshInterval)
		defer t.Stop()
		refreshC = t.C
	}
	var sweepC <-chan time.Time
	if p.cfg.StaleSweep > 0 {
		t := time.NewTicker(p.cfg.StaleSweep)
		defer t.Stop()
		sweepC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case q, ok := <-srcCh:
			if !ok {
				srcCh = nil
				continue
			}
			p.process(q, p.cfg.Now())
		case q := <-p.in:
			p.process(q, p.cfg.Now())
		case <-refreshC:
			_ = p.Refresh(ctx)
		case t := <-sweepC:
			p.sweepStale(t)
		}
	}
}

// Snapshot implements SnapshotSource for channel type "lpBook" — the
// resume/cold-start fallback returns the verbatim last frame (stale
// frames included — a resuming client sees the withdrawal, not a
// fabricated book). No prior emit → no snapshot.
func (p *LPBookProducer) Snapshot(_ context.Context, channel string) (uint64, any, error) {
	typ, rest, ok := strings.Cut(channel, "@")
	if !ok || typ != "lpBook" {
		return 0, nil, fmt.Errorf("marketdata: no lpBook snapshot for %q", channel)
	}
	// Validate the {lp}/{symbol} target shape — a malformed channel
	// must not be served a coincidentally-named state.
	lpTok, sym, ok := strings.Cut(rest, "/")
	if !ok || sym == "" {
		return 0, nil, fmt.Errorf("marketdata: malformed lpBook channel %q", channel)
	}
	if n, err := strconv.ParseInt(lpTok, 10, 64); err != nil || n <= 0 {
		return 0, nil, fmt.Errorf("marketdata: malformed lpBook channel %q", channel)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[channel]
	if st == nil {
		return 0, nil, fmt.Errorf("marketdata: no lpBook state for %q", channel)
	}
	return st.seq, st.frame, nil
}

// ---------------------------------------------------------------------------
// JSON quote ingress — provisional wire contract for the LP feed
// ---------------------------------------------------------------------------

// lpQuoteJSON is the JSON wire shape LP ingress adapters publish on the
// quotes transport — the production producer is the FIX mass-quote path
// (internal/fix JetStreamQuoteSink) on the dedicated "quotes" JetStream
// stream, subject quotes.lp.{lpID}.{symbol-token}. Prices/quantities
// are decimal strings; ts is RFC3339Nano (or ts_ms epoch millis);
// lp_id/symbol may ride the NATS subject instead.
type lpQuoteJSON struct {
	// Kind discriminates updates from explicit withdrawals:
	// "WITHDRAW" pins the decoded book empty regardless of carried
	// levels; "" or "UPDATE" is an ordinary level update.
	Kind         string      `json:"kind"`
	LPID         int64       `json:"lp_id"`
	InstrumentID int64       `json:"instrument_id"`
	Symbol       string      `json:"symbol"`
	Bids         [][2]string `json:"bids"`
	Asks         [][2]string `json:"asks"`
	Seq          uint64      `json:"seq"`
	Ts           string      `json:"ts"`
	TsMs         int64       `json:"ts_ms"`
}

// DecodeLPQuoteJSON parses one wire message. Subject fallback:
// {anything}.{lpID}.{symbol-token} fills lp_id/symbol when the payload
// omits them (SubjectSymbols token map, same convention as the trades
// stream). Malformed input returns an error — callers count it as a
// drop, never a kill.
func DecodeLPQuoteJSON(data []byte, subject string,
	symbols SubjectSymbol) (LPQuote, error) {
	var w lpQuoteJSON
	if err := json.Unmarshal(data, &w); err != nil {
		return LPQuote{}, fmt.Errorf("marketdata: lp quote json: %w", err)
	}
	q := LPQuote{
		LPID: w.LPID, InstrumentID: w.InstrumentID, Symbol: w.Symbol,
		Seq: w.Seq,
	}
	if subject != "" {
		toks := strings.Split(subject, ".")
		if len(toks) >= 2 {
			if q.LPID == 0 {
				if id, err := strconv.ParseInt(toks[len(toks)-2], 10, 64); err == nil {
					q.LPID = id
				}
			}
			if q.Symbol == "" {
				if s, ok := symbols.symbol(subject); ok {
					q.Symbol = s
				}
			}
		}
	}
	switch {
	case w.Ts != "":
		ts, err := time.Parse(time.RFC3339Nano, w.Ts)
		if err != nil {
			return LPQuote{}, fmt.Errorf("marketdata: lp quote ts %q: %w", w.Ts, err)
		}
		q.Ts = ts
	case w.TsMs != 0:
		q.Ts = time.UnixMilli(w.TsMs)
	}
	// kind=WITHDRAW pins the book empty — the withdrawal semantic must
	// survive a producer that still carries stale levels in the payload.
	if strings.EqualFold(w.Kind, "WITHDRAW") {
		return q, nil
	}
	parseSide := func(rows [][2]string) ([]LPQuoteLevel, error) {
		out := make([]LPQuoteLevel, 0, len(rows))
		for i, r := range rows {
			px, err := decimal.NewFromString(r[0])
			if err != nil {
				return nil, fmt.Errorf("level %d price %q: %w", i, r[0], err)
			}
			qty, err := decimal.NewFromString(r[1])
			if err != nil {
				return nil, fmt.Errorf("level %d qty %q: %w", i, r[1], err)
			}
			out = append(out, LPQuoteLevel{Price: px, Qty: qty})
		}
		return out, nil
	}
	var err error
	if q.Bids, err = parseSide(w.Bids); err != nil {
		return LPQuote{}, err
	}
	if q.Asks, err = parseSide(w.Asks); err != nil {
		return LPQuote{}, err
	}
	return q, nil
}

// JSONLPQuoteSource adapts a MsgSource (e.g. JetStreamMsgSource on the
// lp_quotes stream) to LPQuoteSource. Malformed payloads and
// unresolvable subjects are counted via OnDrop and skipped — a corrupt
// message must not kill the feed.
func JSONLPQuoteSource(src MsgSource, symbols SubjectSymbol,
	log *slog.Logger) LPQuoteSource {
	if log == nil {
		log = slog.Default()
	}
	return LPQuoteSourceFunc(func(ctx context.Context) (<-chan LPQuote, error) {
		raw, err := src(ctx)
		if err != nil {
			return nil, err
		}
		out := make(chan LPQuote, 1024)
		go func() {
			defer close(out)
			for {
				select {
				case <-ctx.Done():
					return
				case m, ok := <-raw:
					if !ok {
						return
					}
					q, err := DecodeLPQuoteJSON(m.Data, m.Subject, symbols)
					if err != nil {
						log.Warn("marketdata: malformed lp quote — dropped",
							"subject", m.Subject, "err", err)
						continue
					}
					select {
					case out <- q:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return out, nil
	})
}
