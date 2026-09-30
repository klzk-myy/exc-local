// Phase-23 Task 23.3.6 — market positioning & sentiment analytics
// (spec §10.8, §24 #276):
//
//   - WS channel `sentiment@{symbol}` pushing the long/short account
//     ratio and taker buy/sell flow every 30s.
//   - REST read seams consumed by internal/api/handlers_market_stats.go:
//     `GET /api/v1/analytics/long-short-ratio/{symbol}?period=` and the
//     delayed-cohort snapshot behind `GET /api/v1/market/positioning`.
//   - `GET /api/v1/analytics/open-interest/{symbol}` reads the Task
//     6.3.23 OIProducer through the Latest/History seam added here.
//
// Privacy + delay invariants (§10.8 item 2, §24 #276):
//
//   - Minimum cohort: a symbol publishes positioning/sentiment figures
//     only while >= SentimentMinCohortAccounts (100) distinct accounts
//     hold open positions in it. Below the floor the frame/series point
//     is emitted with suppressed=true and every ratio/notional withheld —
//     a suppressed marker is disclosed, the aggregates are not.
//   - Publication delay: the public surface serves data anchored at
//     now − SentimentPublicationDelay (5m). Positions are current-state
//     rows, so the delay is implemented by snapshotting the cohort
//     aggregate each tick into a per-symbol ring and always reading the
//     newest sample at-or-before the horizon — the same
//     delayed-snapshot model the spec's "time-delayed by 5 minutes"
//     clause requires for a non-replayable source. Taker flow is
//     windowed ClickHouse data, so its delay is a hard upper bound on
//     the query interval instead.
//   - Anonymized aggregates only: no per-account field exists anywhere
//     in the emitted/read types. Concentration bands are computed from
//     per-account rows inside the source and collapse to top-N% shares
//     before leaving it — the wire can never carry an account id.
//
// Channel registration: "sentiment" is added to the public channel-type
// table at package init (RegisterSentimentChannels is invoked from
// init() so the grammar accepts the type without orchestrator wiring;
// the explicit function remains for embedders that rebuild the table).
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// §24 #276 canonical values.
const (
	// SentimentPublicationDelay is the non-premium publication delay on
	// positioning/sentiment figures (spec §10.8: "5-minute delay").
	SentimentPublicationDelay = 5 * time.Minute
	// SentimentMinCohortAccounts is the anonymity floor: below it the
	// symbol's sentiment/positioning data is suppressed, never published.
	SentimentMinCohortAccounts = 100
	// DefaultSentimentTick is the sentiment@ WS push cadence (Task
	// 23.3.6: "30s intervals").
	DefaultSentimentTick = 30 * time.Second
	// DefaultSentimentFlowWindow is the trailing taker-flow window each
	// WS frame aggregates (anchored at the delay horizon).
	DefaultSentimentFlowWindow = 5 * time.Minute
	// defaultSentimentRetention keeps 29h of cohort samples — the 5m
	// delay horizon plus a full 24h of 5m history buckets.
	defaultSentimentRetention = SentimentPublicationDelay + 26*time.Hour
)

// SentimentChannelPrefix prefixes every sentiment channel token.
const SentimentChannelPrefix = "sentiment@"

// sentimentPeriods are the ?period=/interval= bucket widths both the
// long-short-ratio and taker-flow endpoints accept (Task 23.3.6).
var sentimentPeriods = map[string]int{
	"5m": 300, "15m": 900, "1h": 3600, "4h": 14400, "24h": 86400,
}

// SentimentPeriodSec resolves a period token to its bucket width in
// seconds; ok=false for anything outside the published set.
func SentimentPeriodSec(period string) (int, bool) {
	s, ok := sentimentPeriods[period]
	return s, ok
}

// SentimentPeriodLabels returns the accepted period tokens (sorted) for
// error details.
func SentimentPeriodLabels() []string {
	out := make([]string, 0, len(sentimentPeriods))
	for k := range sentimentPeriods {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Sentinel errors mapped by the API layer onto §23 codes.
var (
	// ErrUnknownInstrument is returned by symbol-keyed sources for an
	// instrument the venue does not list → NOT_FOUND.
	ErrUnknownInstrument = errors.New("marketdata: unknown instrument")
	// ErrInsufficientCohort marks a suppressed aggregate — the account
	// cohort is below SentimentMinCohortAccounts → INSUFFICIENT_COHORT.
	ErrInsufficientCohort = errors.New(
		"marketdata: cohort below the 100-account anonymity floor")
)

// RegisterSentimentChannels installs the "sentiment" public channel type
// (ClassNone — uncapped individually, bounded by MaxSubscriptions). Called
// from init; safe to call again (idempotent).
func RegisterSentimentChannels() { channelTypes["sentiment"] = ClassNone }

func init() { RegisterSentimentChannels() }

// ---------------------------------------------------------------------------
// Position cohort aggregates (shared by Task 23.3.10 positioning reads)
// ---------------------------------------------------------------------------

// cohortRow is one account's open position in one instrument — the
// internal aggregation input, never serialized.
type cohortRow struct {
	accountID int64
	side      string // 'LONG'|'SHORT'
	notional  decimal.Decimal
}

// PositionCohort is the anonymized per-symbol positioning aggregate:
// account counts by side, notional by side, and concentration bands
// (share of gross notional held by the top-5%/10%/25% largest holders).
// It contains no per-account data — the fields below are the ONLY
// values that may leave the venue (§10.8 "no per-account attribution").
type PositionCohort struct {
	Symbol        string
	Accounts      int64 // distinct accounts holding open positions
	LongAccounts  int64 // distinct accounts net-long (or holding LONG rows)
	ShortAccounts int64 // distinct accounts holding SHORT rows
	LongNotional  decimal.Decimal
	ShortNotional decimal.Decimal
	GrossNotional decimal.Decimal
	// Concentration bands: nil when Accounts is 0 (never zero-filled).
	Top5Share  *decimal.Decimal // top 5% of holders' share of gross
	Top10Share *decimal.Decimal
	Top25Share *decimal.Decimal
	AsOf       time.Time
}

// Suppressed reports whether the cohort sits below the anonymity floor.
func (c PositionCohort) Suppressed(min int64) bool { return c.Accounts < min }

// LongRatio returns long_accounts / accounts — nil when the cohort is
// empty (a 0/0 ratio would fabricate a signal).
func (c PositionCohort) LongRatio() *decimal.Decimal {
	if c.Accounts <= 0 {
		return nil
	}
	r := decimal.NewFromInt(c.LongAccounts).
		Div(decimal.NewFromInt(c.Accounts))
	return &r
}

// ShortRatio returns short_accounts / accounts (nil on empty cohort).
func (c PositionCohort) ShortRatio() *decimal.Decimal {
	if c.Accounts <= 0 {
		return nil
	}
	r := decimal.NewFromInt(c.ShortAccounts).
		Div(decimal.NewFromInt(c.Accounts))
	return &r
}

// LongShortRatio returns long_accounts / short_accounts — nil when no
// shorts exist (an infinite ratio serializes worse than an absent one).
func (c PositionCohort) LongShortRatio() *decimal.Decimal {
	if c.ShortAccounts <= 0 {
		return nil
	}
	r := decimal.NewFromInt(c.LongAccounts).
		Div(decimal.NewFromInt(c.ShortAccounts))
	return &r
}

// aggregateCohort folds per-account rows into a PositionCohort. An
// account holding both LONG and SHORT rows (hedging mode) counts once
// in Accounts and once in EACH side count — the sides report distinct
// positioned accounts, not exclusive buckets. Concentration ranks
// accounts by gross |notional| and reports the top ceil(n%×N) share;
// with N accounts the top-5% band always includes ≥1 account.
func aggregateCohort(symbol string, rows []cohortRow, now time.Time) PositionCohort {
	c := PositionCohort{Symbol: symbol, AsOf: now.UTC()}
	type acct struct {
		long, short bool
		gross       decimal.Decimal
	}
	byAcct := map[int64]*acct{}
	for _, r := range rows {
		a := byAcct[r.accountID]
		if a == nil {
			a = &acct{}
			byAcct[r.accountID] = a
		}
		switch r.side {
		case "LONG":
			a.long = true
			c.LongNotional = c.LongNotional.Add(r.notional)
		case "SHORT":
			a.short = true
			c.ShortNotional = c.ShortNotional.Add(r.notional)
		}
		a.gross = a.gross.Add(r.notional.Abs())
	}
	c.Accounts = int64(len(byAcct))
	if c.Accounts == 0 {
		return c
	}
	grosses := make([]decimal.Decimal, 0, len(byAcct))
	for _, a := range byAcct {
		if a.long {
			c.LongAccounts++
		}
		if a.short {
			c.ShortAccounts++
		}
		grosses = append(grosses, a.gross)
	}
	c.GrossNotional = c.LongNotional.Abs().Add(c.ShortNotional.Abs())
	sort.Slice(grosses, func(i, j int) bool {
		return grosses[i].GreaterThan(grosses[j])
	})
	share := func(pct int64) *decimal.Decimal {
		if !c.GrossNotional.IsPositive() {
			return nil
		}
		top := (c.Accounts*pct + 99) / 100 // ceil
		if top < 1 {
			top = 1
		}
		if top > c.Accounts {
			top = c.Accounts
		}
		var sum decimal.Decimal
		for i := int64(0); i < top; i++ {
			sum = sum.Add(grosses[i])
		}
		s := sum.Div(c.GrossNotional)
		return &s
	}
	c.Top5Share, c.Top10Share, c.Top25Share = share(5), share(10), share(25)
	return c
}

// PositionCohortSource supplies the anonymized cohort aggregate for one
// symbol. The production implementation is PgxPositionCohortSource over
// the §5.13 positions table (same authoritative source as OI).
type PositionCohortSource interface {
	Cohort(ctx context.Context, symbol string) (PositionCohort, error)
}

// PgxPositionCohortSource aggregates positions per symbol. inst maps
// canonical symbol → instrument_id (the inverse of the OI source map).
type PgxPositionCohortSource struct {
	pool *pgxpool.Pool
	inst map[string]int64
	now  func() time.Time
}

// NewPgxPositionCohortSource binds the position store.
func NewPgxPositionCohortSource(pool *pgxpool.Pool,
	instruments map[string]int64, now func() time.Time) *PgxPositionCohortSource {
	if now == nil {
		now = time.Now
	}
	return &PgxPositionCohortSource{pool: pool, inst: instruments, now: now}
}

// Cohort implements PositionCohortSource. Per-account rows are pulled
// once and folded in-process — the SQL never emits account-level detail
// past this function boundary and the result type has no room for it.
func (s *PgxPositionCohortSource) Cohort(ctx context.Context, symbol string) (PositionCohort, error) {
	if s == nil || s.pool == nil {
		return PositionCohort{}, errors.New("marketdata: cohort source not wired")
	}
	id, ok := s.inst[symbol]
	if !ok {
		return PositionCohort{}, fmt.Errorf("%w: %s", ErrUnknownInstrument, symbol)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT account_id, side::text,
		       COALESCE(SUM(quantity * COALESCE(mark_price, entry_price)),0)::text
		FROM positions
		WHERE instrument_id = $1 AND quantity > 0
		GROUP BY account_id, side`, id)
	if err != nil {
		return PositionCohort{},
			fmt.Errorf("marketdata: cohort query %s: %w", symbol, err)
	}
	defer rows.Close()

	var raw []cohortRow
	for rows.Next() {
		var r cohortRow
		var notional string
		if err := rows.Scan(&r.accountID, &r.side, &notional); err != nil {
			return PositionCohort{},
				fmt.Errorf("marketdata: cohort row scan: %w", err)
		}
		if r.notional, err = decimal.NewFromString(notional); err != nil {
			return PositionCohort{},
				fmt.Errorf("marketdata: cohort notional parse: %w", err)
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return PositionCohort{}, fmt.Errorf("marketdata: cohort rows: %w", err)
	}
	return aggregateCohort(symbol, raw, s.now()), nil
}

// ---------------------------------------------------------------------------
// Delayed-cohort ring + read model
// ---------------------------------------------------------------------------

// cohortSample is one retained snapshot of the cohort aggregate. The
// ring exists because positions are current-state: the only way to
// serve "as of now−5m" honestly is to have sampled it then.
type cohortSample struct {
	at time.Time
	c  PositionCohort
}

// LongShortPoint is one bucket of the long/short-ratio series. A bucket
// below the cohort floor carries Suppressed=true and no ratios; the
// bucket timestamp is still disclosed (a gap would silently truncate
// the timeline — suppression is explicit, never silent).
type LongShortPoint struct {
	BucketStartMs  int64  `json:"bucket_start_ms"`
	Open           bool   `json:"open,omitempty"` // bucket still accumulating past the horizon
	Suppressed     bool   `json:"suppressed"`
	Accounts       int64  `json:"accounts,omitempty"`
	LongRatio      string `json:"long_ratio,omitempty"`
	ShortRatio     string `json:"short_ratio,omitempty"`
	LongShortRatio string `json:"long_short_ratio,omitempty"`
}

// LongShortView / TakerFlowView are the frame sections of the
// sentiment@ push. Suppressed sections disclose the reason and nothing
// else — counts, ratios and notionals are withheld below the floor and
// on source failure (a stale ratio is worse than none).
type LongShortView struct {
	Suppressed     bool   `json:"suppressed"`
	Reason         string `json:"reason,omitempty"` // insufficient_cohort|source_unavailable|delay_horizon
	Accounts       int64  `json:"accounts,omitempty"`
	LongRatio      string `json:"long_ratio,omitempty"`
	ShortRatio     string `json:"short_ratio,omitempty"`
	LongShortRatio string `json:"long_short_ratio,omitempty"`
}

// TakerFlowView is the taker-flow section of the sentiment@ frame.
type TakerFlowView struct {
	Suppressed   bool   `json:"suppressed"`
	Reason       string `json:"reason,omitempty"`
	WindowMs     int64  `json:"window_ms,omitempty"`
	Trades       int64  `json:"trades,omitempty"`
	Accounts     int64  `json:"accounts,omitempty"`
	BuyNotional  string `json:"buy_notional,omitempty"`
	SellNotional string `json:"sell_notional,omitempty"`
	BuySellRatio string `json:"buy_sell_ratio,omitempty"`
}

// sentimentData is the sentiment@{symbol} wire payload.
type sentimentData struct {
	Event     string         `json:"event"` // "sentiment"
	Symbol    string         `json:"symbol"`
	DelayMs   int64          `json:"delay_ms"` // applied publication delay
	AsOfMs    int64          `json:"as_of_ms"` // data horizon actually used
	LongShort *LongShortView `json:"long_short,omitempty"`
	TakerFlow *TakerFlowView `json:"taker_flow,omitempty"`
}

// SentimentProducerConfig tunes SentimentProducer.
type SentimentProducerConfig struct {
	Logger     *slog.Logger
	Now        func() time.Time
	Tick       time.Duration // push cadence; default 30s
	Delay      time.Duration // publication delay; default 5m
	FlowWindow time.Duration // trailing taker window per frame; default 5m
	MinCohort  int64         // anonymity floor; default 100
	Retention  time.Duration // cohort-sample ring horizon; default delay+26h
	Symbols    []string      // universe polled every tick
	// QueryTimeout bounds each per-symbol source read; default 3s (same
	// bound as the OI producer's poll).
	QueryTimeout time.Duration
}

func (c *SentimentProducerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Tick <= 0 {
		c.Tick = DefaultSentimentTick
	}
	if c.Delay <= 0 {
		c.Delay = SentimentPublicationDelay
	}
	if c.FlowWindow <= 0 {
		c.FlowWindow = DefaultSentimentFlowWindow
	}
	if c.MinCohort <= 0 {
		c.MinCohort = SentimentMinCohortAccounts
	}
	if c.Retention <= 0 {
		c.Retention = defaultSentimentRetention
	}
	if c.QueryTimeout <= 0 {
		c.QueryTimeout = 3 * time.Second
	}
}

// SentimentProducer polls the position cohort and taker flow per symbol
// and publishes sentiment@{symbol} every Tick. The emitted data is
// always anchored at now−Delay — this is a PUBLIC channel, so the
// delayed view is the only view it can carry (premium real-time
// variants would need a per-session entitlement channel, which the
// single-broadcast fanout cannot express; that is a documented §27
// candidate, not silent behavior — every frame states delay_ms).
type SentimentProducer struct {
	cfg     SentimentProducerConfig
	pos     PositionCohortSource
	flow    TakerFlowSource
	emit    EmitFunc
	seq     *seqAllocator
	symbols func() []string // optional live-subscription poll seam

	mu      sync.Mutex
	rings   map[string][]cohortSample // symbol → cohort samples (asc by at)
	last    map[string]sentimentData  // symbol → last emitted frame
	lastSeq map[string]uint64
	pending []PositionCohort // Push() seam for tests/embedders
}

// NewSentimentProducer wires the producer. emit is Server.Publish. pos
// may be nil (frames then publish suppressed/source_unavailable); flow
// nil omits the taker_flow section. symbols may be nil — then the tick
// universe is cfg.Symbols ∪ previously-seen.
func NewSentimentProducer(cfg SentimentProducerConfig,
	pos PositionCohortSource, flow TakerFlowSource, emit EmitFunc) *SentimentProducer {
	cfg.defaults()
	return &SentimentProducer{
		cfg: cfg, pos: pos, flow: flow, emit: emit,
		seq:     newSeqAllocator(),
		rings:   map[string][]cohortSample{},
		last:    map[string]sentimentData{},
		lastSeq: map[string]uint64{},
	}
}

// WithSymbolSource binds the live-subscription poll seam (typically
// Server.ActiveSymbolsFor("sentiment")) so only consumed symbols are
// polled. cfg.Symbols always remain in the universe.
func (p *SentimentProducer) WithSymbolSource(f func() []string) *SentimentProducer {
	p.symbols = f
	return p
}

// Push injects a cohort sample directly (tests/embedders). Takes effect
// on the next tick for that symbol — the polled read is skipped.
func (p *SentimentProducer) Push(c PositionCohort) {
	p.mu.Lock()
	p.pending = append(p.pending, c)
	p.mu.Unlock()
}

// universe returns cfg.Symbols ∪ subscribed symbols ∪ known-ring symbols.
func (p *SentimentProducer) universe() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range p.cfg.Symbols {
		add(s)
	}
	if p.symbols != nil {
		for _, s := range p.symbols() {
			add(s)
		}
	}
	p.mu.Lock()
	for s := range p.rings {
		add(s)
	}
	for _, c := range p.pending {
		add(c.Symbol)
	}
	p.mu.Unlock()
	sort.Strings(out)
	return out
}

// tick samples every symbol's cohort, retains the ring, and emits the
// delayed frame. Per-symbol failures suppress that symbol's sections —
// one poisoned symbol never blocks the others.
func (p *SentimentProducer) tick(now time.Time) {
	horizon := now.Add(-p.cfg.Delay)

	p.mu.Lock()
	pending := p.pending
	p.pending = nil
	bySymPending := map[string]PositionCohort{}
	for _, c := range pending {
		bySymPending[c.Symbol] = c
	}
	p.mu.Unlock()

	for _, sym := range p.universe() {
		var (
			cohort    PositionCohort
			cohortErr error
			pushed    bool
		)
		if c, ok := bySymPending[sym]; ok {
			cohort, pushed = c, true
			if cohort.AsOf.IsZero() {
				cohort.AsOf = now
			}
		} else if p.pos != nil {
			ctx, cancel := context.WithTimeout(context.Background(),
				p.cfg.QueryTimeout)
			cohort, cohortErr = p.pos.Cohort(ctx, sym)
			cancel()
			if cohortErr == nil && cohort.AsOf.IsZero() {
				cohort.AsOf = now
			}
		} else {
			cohortErr = errors.New("position cohort source not wired")
		}

		p.mu.Lock()
		if cohortErr == nil {
			ring := append(p.rings[sym],
				cohortSample{at: cohort.AsOf, c: cohort})
			cutoff := now.Add(-p.cfg.Retention)
			i := 0
			for i < len(ring) && ring[i].at.Before(cutoff) {
				i++
			}
			if i > 0 {
				ring = append([]cohortSample(nil), ring[i:]...)
			}
			p.rings[sym] = ring
		}
		// The published cohort is the newest sample at-or-before the
		// delay horizon — never the live value.
		delayed, have := p.delayedLocked(sym, horizon)
		p.mu.Unlock()

		frame := sentimentData{
			Event: "sentiment", Symbol: sym,
			DelayMs: p.cfg.Delay.Milliseconds(),
			AsOfMs:  horizon.UnixMilli(),
		}
		switch {
		case !have:
			// Ring exists but nothing has aged past the delay yet
			// (fresh producer) — or no sample ever landed.
			reason := "delay_horizon"
			if cohortErr != nil {
				reason = "source_unavailable"
			}
			frame.LongShort = &LongShortView{Suppressed: true, Reason: reason}
		case delayed.c.Suppressed(p.cfg.MinCohort):
			frame.AsOfMs = delayed.at.UnixMilli()
			frame.LongShort = &LongShortView{Suppressed: true,
				Reason: "insufficient_cohort"}
		default:
			frame.AsOfMs = delayed.at.UnixMilli()
			frame.LongShort = longShortView(delayed.c)
		}

		if p.flow != nil {
			ctx, cancel := context.WithTimeout(context.Background(),
				p.cfg.QueryTimeout)
			f, ferr := p.flow.TakerFlow(ctx, sym,
				horizon.Add(-p.cfg.FlowWindow), horizon)
			cancel()
			switch {
			case ferr != nil:
				frame.TakerFlow = &TakerFlowView{Suppressed: true,
					Reason: "source_unavailable", WindowMs: p.cfg.FlowWindow.Milliseconds()}
			case f.Accounts < p.cfg.MinCohort:
				frame.TakerFlow = &TakerFlowView{Suppressed: true,
					Reason: "insufficient_cohort", WindowMs: p.cfg.FlowWindow.Milliseconds()}
			default:
				frame.TakerFlow = takerFlowView(f, p.cfg.FlowWindow)
			}
			if ferr != nil {
				p.cfg.Logger.Error("marketdata: sentiment taker flow failed",
					"symbol", sym, "err", ferr)
			}
		}

		if cohortErr != nil && !pushed {
			p.cfg.Logger.Error("marketdata: sentiment cohort failed",
				"symbol", sym, "err", cohortErr)
		}

		ch := SentimentChannelPrefix + sym
		seq := p.seq.next(ch)
		p.mu.Lock()
		p.lastSeq[sym] = seq
		p.last[sym] = frame
		p.mu.Unlock()
		p.emit(ch, seq, frame)
	}
}

func longShortView(c PositionCohort) *LongShortView {
	v := &LongShortView{Accounts: c.Accounts}
	if r := c.LongRatio(); r != nil {
		v.LongRatio = r.StringFixed(6)
	}
	if r := c.ShortRatio(); r != nil {
		v.ShortRatio = r.StringFixed(6)
	}
	if r := c.LongShortRatio(); r != nil {
		v.LongShortRatio = r.StringFixed(6)
	}
	return v
}

func takerFlowView(f TakerFlow, window time.Duration) *TakerFlowView {
	v := &TakerFlowView{
		WindowMs:     window.Milliseconds(),
		Trades:       f.Trades,
		Accounts:     f.Accounts,
		BuyNotional:  f.BuyNotional.StringFixed(8),
		SellNotional: f.SellNotional.StringFixed(8),
	}
	if r := f.BuySellRatio(); r != nil {
		v.BuySellRatio = r.StringFixed(6)
	}
	return v
}

// delayedLocked returns the newest cohort sample with at ≤ horizon.
// Caller holds p.mu.
func (p *SentimentProducer) delayedLocked(sym string, horizon time.Time) (cohortSample, bool) {
	ring := p.rings[sym]
	for i := len(ring) - 1; i >= 0; i-- {
		if !ring[i].at.After(horizon) {
			return ring[i], true
		}
	}
	return cohortSample{}, false
}

// LatestCohort returns the newest cohort sample at-or-before horizon —
// the REST seam for /api/v1/market/positioning. found=false when no
// sample has aged past the delay.
func (p *SentimentProducer) LatestCohort(symbol string, horizon time.Time) (PositionCohort, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.delayedLocked(symbol, horizon.UTC())
	return s.c, ok
}

// LongShortSeries aggregates the retained cohort samples into
// bucketSec-wide points ending at horizon (inclusive). Buckets are
// filled by the newest sample inside them; a bucket straddling the
// horizon is emitted open=true. BucketSec must be one of the published
// period widths (300/900/3600/14400/86400). Below-floor buckets emit
// suppressed points — nothing is fabricated.
func (p *SentimentProducer) LongShortSeries(symbol string, bucketSec int,
	horizon time.Time, limit int) ([]LongShortPoint, error) {
	valid := false
	for _, s := range sentimentPeriods {
		if s == bucketSec {
			valid = true
			break
		}
	}
	if !valid {
		return nil, fmt.Errorf("marketdata: bucket width %ds not in the sentiment period set", bucketSec)
	}
	width := time.Duration(bucketSec) * time.Second
	horizon = horizon.UTC()

	p.mu.Lock()
	src := p.rings[symbol]
	out := make([]LongShortPoint, 0, len(src))
	var cur *LongShortPoint
	var curStart time.Time
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, s := range src {
		if s.at.After(horizon) {
			break // rings are ascending — nothing later qualifies
		}
		bs := s.at.Truncate(width)
		if cur == nil || !bs.Equal(curStart) {
			flush()
			cur = &LongShortPoint{BucketStartMs: bs.UnixMilli()}
			curStart = bs
		}
		// newest sample wins — overwrite on every in-bucket sample
		c := s.c
		cur.Suppressed = c.Suppressed(p.cfg.MinCohort)
		if !cur.Suppressed {
			cur.Accounts = c.Accounts
			if r := c.LongRatio(); r != nil {
				cur.LongRatio = r.StringFixed(6)
			}
			if r := c.ShortRatio(); r != nil {
				cur.ShortRatio = r.StringFixed(6)
			}
			if r := c.LongShortRatio(); r != nil {
				cur.LongShortRatio = r.StringFixed(6)
			}
		} else {
			cur.Accounts, cur.LongRatio = 0, ""
			cur.ShortRatio, cur.LongShortRatio = "", ""
		}
	}
	flush()
	if n := len(out); n > 0 {
		out[n-1].Open = curStart.Add(width).After(horizon)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	p.mu.Unlock()
	return out, nil
}

// Snapshot implements SnapshotSource for channel type "sentiment" — the
// resume/cold-start fallback returns the last emitted frame.
func (p *SentimentProducer) Snapshot(_ context.Context, channel string) (uint64, any, error) {
	sym, ok := strings.CutPrefix(channel, SentimentChannelPrefix)
	if !ok || sym == "" {
		return 0, nil, fmt.Errorf("marketdata: no sentiment snapshot for %q", channel)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f, ok := p.last[sym]
	if !ok {
		return 0, nil, fmt.Errorf("marketdata: no sentiment state for %q", sym)
	}
	return p.lastSeq[sym], f, nil
}

// Run drives the poll loop until ctx is cancelled.
func (p *SentimentProducer) Run(ctx context.Context) error {
	tick := time.NewTicker(p.cfg.Tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-tick.C:
			p.tick(t)
		}
	}
}

// ---------------------------------------------------------------------------
// OI read seam for the REST analytics surface (Task 23.3.6 endpoint a)
// ---------------------------------------------------------------------------

// Latest returns the producer's freshest aggregate for symbol — the
// co-resident read seam behind GET /api/v1/analytics/open-interest.
// found=false when no sample has been observed (never a fabricated
// zero — the handler marks the payload insufficient_data instead).
func (p *OIProducer) Latest(symbol string) (OISample, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.last[symbol]
	return s, ok
}
