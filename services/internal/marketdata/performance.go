// Phase-23 Task 23.3.11 — public aggregate venue performance
// statistics (spec §16.10, §24 #380).
//
//	GET /api/v1/market/performance
//
// serves: average spread per pair (daily/weekly), median execution
// latency, fill rate, platform uptime, and the quarterly RTS-27-derived
// execution-quality figures (Task 21.3.19 PUBLISHED artifacts).
//
// Contract points implemented here:
//
//   - Aggregate only: the source reads are rts27_daily_stats /
//     volume_stats counters / ops_status_events / published rts27_reports
//     — no account, order or position row ever enters this surface.
//   - Current-session figures are anchored at now−SessionDelay (15m);
//     completed-day figures are final.
//   - Reconciliation: metrics with an independent reference (fill rate,
//     median exec latency, uptime — bound through ReferenceSource to the
//     TCA/SLO stores by the orchestrator) are compared within
//     ReconcileTolerance. Any divergence fires a data-quality alert on
//     the observability.Sink seam and the endpoint HOLDS the last-good
//     payload with a held-age flag — a divergent value is never
//     published silently (Task 23.3.11 edge case).
//   - Insufficient data is a marker, never a zero: a configured symbol
//     with no daily rows reports insufficient_data=true; NULL source
//     columns serialize as null (rts27_daily_stats.gaps documents which
//     metrics a source cannot supply — e.g. spread_avg_bps stays NULL
//     while no persisted quote stream exists).
//
// The 5-minute Redis result cache lives at the handler seam
// (internal/api/handlers_market_stats.go) so this service always
// computes a fresh Report — the cache may be lost without consequence.
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/observability"
	"exchange/pkg/decimal"
)

// §24 #380 canonical values.
const (
	// PerformanceSessionDelay is the publication delay applied to
	// current-session figures (spec §16.10: 15-minute delay).
	PerformanceSessionDelay = 15 * time.Minute
	// PerformanceCacheTTL is the Redis TTL on the rendered payload
	// (Task 23.3.11 item 3).
	PerformanceCacheTTL = 5 * time.Minute
	// PerformanceCacheKey namespaces the cached payload.
	PerformanceCacheKey = "marketdata:performance:v1"
	// performanceAlertCode identifies divergence alerts on the
	// ops.alerts.monitoring channel (internal alert vocabulary, not §23).
	performanceAlertCode = "DATA_QUALITY_DIVERGENCE"
	// performanceAlertRule is the evaluator-style rule id.
	performanceAlertRule = "venue_performance_divergence"
	// opsSystemComponent is the ops_status_events aggregate component
	// (ops.componentWildcard = "system").
	opsSystemComponent = "system"
)

// PairDayStats is one aggregate rts27_daily_stats row (per instrument
// per UTC day — already venue-level; no account data exists in it).
// Pointer fields carry NULL faithfully: nil = the source could not
// supply the metric that day.
type PairDayStats struct {
	Symbol              string
	Day                 time.Time // UTC day
	AvgSpreadBps        *decimal.Decimal
	MedianExecLatencyMs *decimal.Decimal
	FillRate            *decimal.Decimal
	OrdersSubmitted     *int64
	OrdersFilled        *int64
	Fills               int64
	VolumeQuote         decimal.Decimal
}

// VenueDayStatsSource reads the per-pair daily quality materialization.
// The production implementation is PgVenueDayStatsSource over
// rts27_daily_stats (migration 251).
type VenueDayStatsSource interface {
	DailyStats(ctx context.Context, from, to time.Time) ([]PairDayStats, error)
}

// PgVenueDayStatsSource reads rts27_daily_stats. The table already
// carries the symbol label — no instrument map is needed.
type PgVenueDayStatsSource struct {
	pool *pgxpool.Pool
}

// NewPgVenueDayStatsSource binds the daily-stats table.
func NewPgVenueDayStatsSource(pool *pgxpool.Pool) *PgVenueDayStatsSource {
	return &PgVenueDayStatsSource{pool: pool}
}

// DailyStats implements VenueDayStatsSource. [from,to) bounds the UTC
// day column; rows return ordered (symbol, day).
func (s *PgVenueDayStatsSource) DailyStats(ctx context.Context,
	from, to time.Time) ([]PairDayStats, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("marketdata: venue daily stats source not wired")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT symbol, day, spread_avg_bps::text,
		       median_order_to_fill_ms::text, fill_rate::text,
		       orders_submitted, orders_filled, fills,
		       volume_quote::text
		FROM rts27_daily_stats
		WHERE day >= $1::date AND day < $2::date
		ORDER BY symbol, day`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("marketdata: venue daily stats: %w", err)
	}
	defer rows.Close()

	var out []PairDayStats
	for rows.Next() {
		var (
			r           PairDayStats
			spread, lat *string
			fr          *string
			sub, fil    *int64
			volQ        string
		)
		if err := rows.Scan(&r.Symbol, &r.Day, &spread, &lat, &fr,
			&sub, &fil, &r.Fills, &volQ); err != nil {
			return nil, fmt.Errorf("marketdata: venue daily stats scan: %w", err)
		}
		parse := func(s *string) (*decimal.Decimal, error) {
			if s == nil || *s == "" {
				return nil, nil
			}
			d, err := decimal.NewFromString(*s)
			if err != nil {
				return nil, err
			}
			return &d, nil
		}
		if r.AvgSpreadBps, err = parse(spread); err != nil {
			return nil, fmt.Errorf("marketdata: spread parse: %w", err)
		}
		if r.MedianExecLatencyMs, err = parse(lat); err != nil {
			return nil, fmt.Errorf("marketdata: latency parse: %w", err)
		}
		if r.FillRate, err = parse(fr); err != nil {
			return nil, fmt.Errorf("marketdata: fill-rate parse: %w", err)
		}
		if r.VolumeQuote, err = decimal.NewFromString(volQ); err != nil {
			return nil, fmt.Errorf("marketdata: volume parse: %w", err)
		}
		r.OrdersSubmitted, r.OrdersFilled = sub, fil
		r.Day = r.Day.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketdata: venue daily stats rows: %w", err)
	}
	return out, nil
}

// UptimeSource is the platform-uptime seam — *ops.Aggregator satisfies
// it (Uptime over the "system" component of the ops_status_events
// ledger). found=false means the window has no event history — uptime
// is reported null, never 100%.
type UptimeSource interface {
	Uptime(ctx context.Context, component string,
		window time.Duration) (fraction float64, found bool, err error)
}

// VenueFillCounter is one symbol's submitted/filled order counters —
// the aggregate volume_stats '1d' shape (adapted from
// analytics.FillRate by the handler seam; no account data involved).
type VenueFillCounter struct {
	Symbol          string
	OrdersSubmitted int64
	OrdersFilled    int64
}

// VenueFillRateSource supplies the venue order counters used for the
// fill-rate figures and the current-session window.
type VenueFillRateSource interface {
	FillRates(ctx context.Context, from, to time.Time) ([]VenueFillCounter, error)
}

// RTS27Figure is one PUBLISHED quarterly execution-quality figure —
// Metrics is the compliance.RTS27Service artifact verbatim (it is
// already the public ESMA publication; the adapter strips the row's
// actor fields, never the metrics).
type RTS27Figure struct {
	QuarterStart    time.Time      `json:"quarter_start"`
	InstrumentClass string         `json:"instrument_class"`
	Version         int            `json:"version"`
	DaysCovered     int            `json:"days_covered"`
	Metrics         map[string]any `json:"metrics"`
	PublishedAt     *time.Time     `json:"published_at,omitempty"`
}

// RTS27Source supplies the published quarterly figures — the adapter in
// the api package wraps compliance.RTS27Service.ListPublished.
type RTS27Source interface {
	PublishedFigures(ctx context.Context) ([]RTS27Figure, error)
}

// PerformanceReferenceSource is the reconciliation seam: it returns an
// independent measurement for one published metric over the same
// window — the orchestrator binds TCA (Task 20.3.9) and SLO-burn
// (Task 9.3.13) reads here. found=false is an absent reference, NOT a
// divergence — only present references can disagree.
type PerformanceReferenceSource interface {
	Reference(ctx context.Context, metric string,
		from, to time.Time) (value float64, found bool, err error)
}

// reconcilableMetrics are the published venue metrics a reference may
// check. Anything else publishes without a second-source check —
// reconciliation can only verify what two sources both measure.
var reconcilableMetrics = []string{
	"fill_rate_24h", "median_exec_latency_ms", "uptime_24h",
}

// ---------------------------------------------------------------------------
// Wire document
// ---------------------------------------------------------------------------

// PairDay is one completed-day per-pair row. Null fields carry the
// rts27_daily_stats gap semantics: null = the source cannot supply the
// metric (e.g. spread_avg_bps pending a persisted quote stream).
type PairDay struct {
	Day                 string  `json:"day"` // YYYY-MM-DD
	AvgSpreadBps        *string `json:"avg_spread_bps"`
	MedianExecLatencyMs *string `json:"median_exec_latency_ms"`
	FillRate            *string `json:"fill_rate"`
	Fills               int64   `json:"fills"`
	VolumeQuote         string  `json:"volume_quote"`
}

// PairWeek is the ISO-week rollup (fills-weighted means; fill_rate is
// recomputed from summed counters, not averaged ratios).
type PairWeek struct {
	WeekStart           string  `json:"week_start"` // Monday, YYYY-MM-DD
	Days                int     `json:"days"`
	AvgSpreadBps        *string `json:"avg_spread_bps"`
	MedianExecLatencyMs *string `json:"median_exec_latency_ms"`
	FillRate            *string `json:"fill_rate"`
	Fills               int64   `json:"fills"`
}

// PairPerformance is one symbol's block. InsufficientData marks a
// configured symbol with no materialized history — never zero-filled.
type PairPerformance struct {
	Symbol           string     `json:"symbol"`
	InsufficientData bool       `json:"insufficient_data,omitempty"`
	Daily            []PairDay  `json:"daily,omitempty"`
	Weekly           []PairWeek `json:"weekly,omitempty"`
}

// SessionPerf is the current-session window [day_start, asOf] — the
// only block the 15-minute delay applies to. Metrics with no intraday
// source stay null (documented gap; the daily materialization covers
// finalized days).
type SessionPerf struct {
	WindowStartMs int64   `json:"window_start_ms"`
	WindowEndMs   int64   `json:"window_end_ms"`
	DelayMs       int64   `json:"delay_ms"`
	FillRate      *string `json:"fill_rate"`
}

// VenueRollup carries the venue-level figures.
type VenueRollup struct {
	MedianExecLatencyMs *string      `json:"median_exec_latency_ms"` // latest day, fills-weighted
	FillRate24h         *string      `json:"fill_rate_24h"`
	FillRate7d          *string      `json:"fill_rate_7d"`
	Uptime24h           *float64     `json:"uptime_24h"` // fraction 0..1; null = unknown history
	Uptime30d           *float64     `json:"uptime_30d"`
	CurrentSession      *SessionPerf `json:"current_session,omitempty"`
}

// VenuePerformance is the /api/v1/market/performance payload. Status:
//
//   - "ok"                — freshly computed and reconciled
//   - "held"              — source divergence tripped; last-good payload
//     served with held_age_ms (never a silent divergent publish)
//   - "insufficient_data" — divergence with no last-good to hold: the
//     report suppresses its figures rather than publish known-bad values
type VenuePerformance struct {
	AsOfMs    int64             `json:"as_of_ms"`
	DelayMs   int64             `json:"delay_ms"`
	Status    string            `json:"status"`
	HeldAgeMs *int64            `json:"held_age_ms,omitempty"`
	Divergent []string          `json:"divergent_metrics,omitempty"`
	Pairs     []PairPerformance `json:"pairs,omitempty"`
	Venue     *VenueRollup      `json:"venue,omitempty"`
	RTS27     []RTS27Figure     `json:"rts27,omitempty"`
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// VenuePerformanceConfig tunes VenuePerformanceService.
type VenuePerformanceConfig struct {
	Logger *slog.Logger
	Now    func() time.Time
	// SessionDelay bounds current-session figures (default 15m).
	SessionDelay time.Duration
	// DailyDays is the completed-day lookback per pair (default 7).
	DailyDays int
	// WeeklyWeeks bounds the weekly rollup (default 4).
	WeeklyWeeks int
	// ReconcileTolerance is the relative divergence that trips the
	// data-quality hold (default 0.05 = 5%).
	ReconcileTolerance float64
	// Symbols is the configured instrument universe — symbols present
	// here but absent from the daily materialization report
	// insufficient_data (new pairs are marked, never zero-filled).
	Symbols []string
}

// VenuePerformanceDeps bundles the read seams. Daily is required —
// Report errors without it (the per-pair block is the payload's core).
// The rest are optional: an unwired source nulls its section honestly
// rather than failing the whole endpoint.
type VenuePerformanceDeps struct {
	Daily     VenueDayStatsSource
	Fills     VenueFillRateSource
	Uptime    UptimeSource
	RTS27     RTS27Source
	Reference PerformanceReferenceSource
	// Alerts receives the data-quality divergence alert; nil defaults to
	// observability.LogSink so a divergence can never go unnoticed.
	Alerts observability.Sink
}

func (c *VenuePerformanceConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.SessionDelay <= 0 {
		c.SessionDelay = PerformanceSessionDelay
	}
	if c.DailyDays <= 0 {
		c.DailyDays = 7
	}
	if c.WeeklyWeeks <= 0 {
		c.WeeklyWeeks = 4
	}
	if c.ReconcileTolerance <= 0 {
		c.ReconcileTolerance = 0.05
	}
}

// VenuePerformanceService assembles and reconciles the public stats
// document. Report is goroutine-safe; last-good is retained in-process.
type VenuePerformanceService struct {
	cfg  VenuePerformanceConfig
	deps VenuePerformanceDeps

	mu         sync.Mutex
	lastGood   *VenuePerformance
	lastGoodAt time.Time
	divergent  map[string]bool // edge state for alert fire/resolve
}

// NewVenuePerformanceService wires the service.
func NewVenuePerformanceService(cfg VenuePerformanceConfig,
	deps VenuePerformanceDeps) *VenuePerformanceService {
	cfg.defaults()
	if deps.Alerts == nil {
		deps.Alerts = observability.LogSink{Log: cfg.Logger}
	}
	return &VenuePerformanceService{cfg: cfg, deps: deps,
		divergent: map[string]bool{}}
}

func decStr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.StringFixed(8)
	return &s
}

func dayOnly(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// compute builds the fresh document anchored at horizon = now−delay.
func (s *VenuePerformanceService) compute(ctx context.Context,
	now time.Time) (*VenuePerformance, error) {
	if s.deps.Daily == nil {
		return nil, errors.New("marketdata: performance daily stats source not wired")
	}
	horizon := now.Add(-s.cfg.SessionDelay)
	doc := &VenuePerformance{
		AsOfMs:  horizon.UnixMilli(),
		DelayMs: s.cfg.SessionDelay.Milliseconds(),
		Status:  "ok",
	}

	// --- per-pair daily + weekly ------------------------------------
	from := dayOnly(now.AddDate(0, 0, -s.cfg.DailyDays))
	to := dayOnly(now).AddDate(0, 0, 1)
	rows, err := s.deps.Daily.DailyStats(ctx, from, to)
	if err != nil {
		return nil, err
	}
	bySym := map[string][]PairDayStats{}
	for _, r := range rows {
		bySym[r.Symbol] = append(bySym[r.Symbol], r)
	}
	symSet := map[string]bool{}
	for _, sym := range s.cfg.Symbols {
		symSet[sym] = true
	}
	for sym := range bySym {
		symSet[sym] = true
	}
	syms := make([]string, 0, len(symSet))
	for sym := range symSet {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		pp := PairPerformance{Symbol: sym}
		days := bySym[sym]
		if len(days) == 0 {
			// Configured pair with no materialized history — marker,
			// never a zero row.
			pp.InsufficientData = true
			doc.Pairs = append(doc.Pairs, pp)
			continue
		}
		for _, d := range days {
			pp.Daily = append(pp.Daily, PairDay{
				Day:                 d.Day.Format("2006-01-02"),
				AvgSpreadBps:        decStr(d.AvgSpreadBps),
				MedianExecLatencyMs: decStr(d.MedianExecLatencyMs),
				FillRate:            decStr(d.FillRate),
				Fills:               d.Fills,
				VolumeQuote:         d.VolumeQuote.StringFixed(8),
			})
		}
		pp.Weekly = weeklyRollup(days, s.cfg.WeeklyWeeks)
		doc.Pairs = append(doc.Pairs, pp)
	}

	// --- venue rollup ------------------------------------------------
	v := &VenueRollup{}
	doc.Venue = v
	v.MedianExecLatencyMs = decStr(venueLatency(rows))
	if s.deps.Fills != nil {
		if fr, err := s.venueFillRate(ctx, horizon.Add(-24*time.Hour),
			horizon); err == nil {
			v.FillRate24h = decStr(fr)
		} else {
			s.cfg.Logger.Error("marketdata: performance fill-rate 24h",
				"err", err)
		}
		if fr, err := s.venueFillRate(ctx,
			dayOnly(horizon.AddDate(0, 0, -7)), horizon); err == nil {
			v.FillRate7d = decStr(fr)
		} else {
			s.cfg.Logger.Error("marketdata: performance fill-rate 7d",
				"err", err)
		}
		// Current session: today's counters up to the delay horizon.
		if fr, err := s.venueFillRate(ctx, dayOnly(horizon), horizon); err == nil {
			v.CurrentSession = &SessionPerf{
				WindowStartMs: dayOnly(horizon).UnixMilli(),
				WindowEndMs:   horizon.UnixMilli(),
				DelayMs:       s.cfg.SessionDelay.Milliseconds(),
				FillRate:      decStr(fr),
			}
		}
	}
	if s.deps.Uptime != nil {
		if f, ok, err := s.deps.Uptime.Uptime(ctx, opsSystemComponent,
			24*time.Hour); err == nil && ok {
			v.Uptime24h = &f
		} else if err != nil {
			s.cfg.Logger.Error("marketdata: performance uptime 24h",
				"err", err)
		}
		if f, ok, err := s.deps.Uptime.Uptime(ctx, opsSystemComponent,
			30*24*time.Hour); err == nil && ok {
			v.Uptime30d = &f
		} else if err != nil {
			s.cfg.Logger.Error("marketdata: performance uptime 30d",
				"err", err)
		}
	}

	// --- RTS-27 quarterly figures (PUBLISHED artifacts verbatim) -----
	if s.deps.RTS27 != nil {
		figs, err := s.deps.RTS27.PublishedFigures(ctx)
		if err != nil {
			s.cfg.Logger.Error("marketdata: performance rts27 read",
				"err", err)
		} else {
			doc.RTS27 = figs
		}
	}
	return doc, nil
}

// venueFillRate sums the volume_stats order counters over [from,to).
// nil result = nothing submitted (no rate to report, not a 0).
func (s *VenuePerformanceService) venueFillRate(ctx context.Context,
	from, to time.Time) (*decimal.Decimal, error) {
	rows, err := s.deps.Fills.FillRates(ctx, from, to)
	if err != nil {
		return nil, err
	}
	var sub, fil int64
	for _, r := range rows {
		sub += r.OrdersSubmitted
		fil += r.OrdersFilled
	}
	if sub <= 0 {
		return nil, nil
	}
	r := decimal.NewFromInt(fil).Div(decimal.NewFromInt(sub))
	return &r, nil
}

// venueLatency is the latest day's fills-weighted mean of the per-pair
// median order→fill latencies — the venue-level execution-latency
// figure. nil when no day carries latency data.
func venueLatency(rows []PairDayStats) *decimal.Decimal {
	var latest time.Time
	for _, r := range rows {
		if r.MedianExecLatencyMs != nil && r.Day.After(latest) {
			latest = r.Day
		}
	}
	if latest.IsZero() {
		return nil
	}
	var num, den decimal.Decimal
	for _, r := range rows {
		if !r.Day.Equal(latest) || r.MedianExecLatencyMs == nil || r.Fills <= 0 {
			continue
		}
		num = num.Add(r.MedianExecLatencyMs.Mul(decimal.NewFromInt(r.Fills)))
		den = den.Add(decimal.NewFromInt(r.Fills))
	}
	if !den.IsPositive() {
		return nil
	}
	m := num.Div(den)
	return &m
}

// weeklyRollup folds daily rows into ISO-week buckets (Monday start).
func weeklyRollup(days []PairDayStats, maxWeeks int) []PairWeek {
	type acc struct {
		start        time.Time
		days         int
		fills        int64
		spreadNum    decimal.Decimal
		spreadDen    decimal.Decimal
		latNum       decimal.Decimal
		latDen       decimal.Decimal
		sub, fil     int64
		subOK, filOK bool
	}
	byWeek := map[string]*acc{}
	var order []string
	for _, d := range days {
		y, w := d.Day.UTC().ISOWeek()
		key := fmt.Sprintf("%04d-W%02d", y, w)
		a := byWeek[key]
		if a == nil {
			// Monday of the ISO week.
			wd := int(d.Day.Weekday()+6) % 7 // Mon=0
			a = &acc{start: dayOnly(d.Day).AddDate(0, 0, -wd)}
			byWeek[key] = a
			order = append(order, key)
		}
		a.days++
		a.fills += d.Fills
		if d.AvgSpreadBps != nil && d.Fills > 0 {
			a.spreadNum = a.spreadNum.Add(
				d.AvgSpreadBps.Mul(decimal.NewFromInt(d.Fills)))
			a.spreadDen = a.spreadDen.Add(decimal.NewFromInt(d.Fills))
		}
		if d.MedianExecLatencyMs != nil && d.Fills > 0 {
			a.latNum = a.latNum.Add(
				d.MedianExecLatencyMs.Mul(decimal.NewFromInt(d.Fills)))
			a.latDen = a.latDen.Add(decimal.NewFromInt(d.Fills))
		}
		if d.OrdersSubmitted != nil {
			a.sub += *d.OrdersSubmitted
			a.subOK = true
		}
		if d.OrdersFilled != nil {
			a.fil += *d.OrdersFilled
			a.filOK = true
		}
	}
	sort.Strings(order)
	if maxWeeks > 0 && len(order) > maxWeeks {
		order = order[len(order)-maxWeeks:]
	}
	var out []PairWeek
	for _, k := range order {
		a := byWeek[k]
		w := PairWeek{
			WeekStart: a.start.Format("2006-01-02"),
			Days:      a.days,
			Fills:     a.fills,
		}
		if a.spreadDen.IsPositive() {
			v := a.spreadNum.Div(a.spreadDen).StringFixed(8)
			w.AvgSpreadBps = &v
		}
		if a.latDen.IsPositive() {
			v := a.latNum.Div(a.latDen).StringFixed(3)
			w.MedianExecLatencyMs = &v
		}
		if a.subOK && a.filOK && a.sub > 0 {
			v := decimal.NewFromInt(a.fil).Div(decimal.NewFromInt(a.sub)).
				StringFixed(6)
			w.FillRate = &v
		}
		out = append(out, w)
	}
	return out
}

// reconcile compares each reconcilable computed metric with its
// independent reference. Returns the names that diverged beyond
// tolerance. Absent references or absent computed values are skipped —
// an unmeasurable metric cannot diverge.
func (s *VenuePerformanceService) reconcile(ctx context.Context,
	doc *VenuePerformance, now time.Time) []string {
	if s.deps.Reference == nil || doc.Venue == nil {
		return nil
	}
	horizon := now.Add(-s.cfg.SessionDelay)
	computed := map[string]float64{}
	if doc.Venue.FillRate24h != nil {
		if f, err := strconv.ParseFloat(*doc.Venue.FillRate24h, 64); err == nil {
			computed["fill_rate_24h"] = f
		}
	}
	if doc.Venue.MedianExecLatencyMs != nil {
		if f, err := strconv.ParseFloat(*doc.Venue.MedianExecLatencyMs, 64); err == nil {
			computed["median_exec_latency_ms"] = f
		}
	}
	if doc.Venue.Uptime24h != nil {
		computed["uptime_24h"] = *doc.Venue.Uptime24h
	}
	var divergent []string
	for _, metric := range reconcilableMetrics {
		c, ok := computed[metric]
		if !ok {
			continue
		}
		ref, found, err := s.deps.Reference.Reference(ctx, metric,
			horizon.Add(-24*time.Hour), horizon)
		if err != nil {
			s.cfg.Logger.Error("marketdata: performance reference read",
				"metric", metric, "err", err)
			continue
		}
		if !found {
			continue
		}
		denom := ref
		if denom < 0 {
			denom = -denom
		}
		if denom < 1e-9 {
			denom = 1e-9 // near-zero reference → absolute check
		}
		diff := c - ref
		if diff < 0 {
			diff = -diff
		}
		if diff/denom > s.cfg.ReconcileTolerance {
			divergent = append(divergent, metric)
			s.raise(ctx, metric, c, ref)
		} else {
			s.resolve(ctx, metric)
		}
	}
	return divergent
}

// raise dispatches the divergence alert on the sink seam — edge-tracked
// so a sustained divergence fires once, not per request.
func (s *VenuePerformanceService) raise(ctx context.Context, metric string,
	computed, reference float64) {
	s.mu.Lock()
	first := !s.divergent[metric]
	s.divergent[metric] = true
	s.mu.Unlock()
	if !first {
		return
	}
	if err := s.deps.Alerts.Raise(ctx, observability.Alert{
		Rule:     performanceAlertRule,
		Severity: observability.SeverityP2,
		Code:     performanceAlertCode,
		Summary: fmt.Sprintf("venue performance %s diverged from "+
			"reference beyond tolerance", metric),
		Status: "firing",
		Details: map[string]string{
			"metric":    metric,
			"computed":  strconv.FormatFloat(computed, 'g', 8, 64),
			"reference": strconv.FormatFloat(reference, 'g', 8, 64),
			"tolerance": strconv.FormatFloat(s.cfg.ReconcileTolerance, 'g', 4, 64),
		},
		FiredAt: s.cfg.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		s.cfg.Logger.Error("marketdata: performance alert dispatch",
			"metric", metric, "err", err)
	}
}

func (s *VenuePerformanceService) resolve(ctx context.Context, metric string) {
	s.mu.Lock()
	was := s.divergent[metric]
	delete(s.divergent, metric)
	s.mu.Unlock()
	if !was {
		return
	}
	_ = s.deps.Alerts.Raise(ctx, observability.Alert{
		Rule: performanceAlertRule, Severity: observability.SeverityP2,
		Code: performanceAlertCode, Status: "resolved",
		Summary: fmt.Sprintf("venue performance %s reconciliation recovered",
			metric),
		FiredAt: s.cfg.Now().UTC().Format(time.RFC3339Nano),
	})
}

// Report returns the public performance document. On reconciliation
// divergence the last-good payload is held and served with
// status="held" + held_age_ms; with no last-good the report suppresses
// figures (status="insufficient_data") — a divergent value is never
// published silently.
func (s *VenuePerformanceService) Report(ctx context.Context) (*VenuePerformance, error) {
	now := s.cfg.Now().UTC()
	doc, err := s.compute(ctx, now)
	if err != nil {
		return nil, err
	}
	divergent := s.reconcile(ctx, doc, now)

	if len(divergent) == 0 {
		s.mu.Lock()
		s.lastGood = doc
		s.lastGoodAt = now
		s.mu.Unlock()
		return doc, nil
	}

	s.mu.Lock()
	held := s.lastGood
	heldAt := s.lastGoodAt
	s.mu.Unlock()
	if held != nil {
		cp := *held
		age := now.Sub(heldAt).Milliseconds()
		cp.Status = "held"
		cp.HeldAgeMs = &age
		cp.Divergent = divergent
		return &cp, nil
	}
	return &VenuePerformance{
		AsOfMs:    doc.AsOfMs,
		DelayMs:   doc.DelayMs,
		Status:    "insufficient_data",
		Divergent: divergent,
	}, nil
}
