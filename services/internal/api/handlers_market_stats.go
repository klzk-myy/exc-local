// handlers_market_stats.go — Phase-23 Tasks 23.3.6/23.3.10/23.3.11:
// public positioning, sentiment and venue-performance reads
// (spec §10.8, §16.10; §24 #276/#359/#380):
//
//	GET /api/v1/analytics/open-interest/{symbol}?interval={1h|4h|1d}&limit=
//	GET /api/v1/analytics/long-short-ratio/{symbol}?period={5m|15m|1h|4h|24h}&limit=
//	GET /api/v1/analytics/taker-flow/{symbol}?period={5m|15m|1h|4h|24h}&limit=
//	GET /api/v1/market/taker-volume?symbol=&interval={5m|15m|1h|4h|24h}&limit=
//	GET /api/v1/market/positioning?symbol=
//	GET /api/v1/market/performance
//
// Guard model (shared with the sibling history surfaces):
//
//   - Publication delay: the non-premium horizon is now−5m for
//     sentiment/positioning/taker data (§10.8) and the venue
//     performance service enforces its own 15m session delay (§16.10).
//     Premium callers (professional|institutional|admin tier via
//     middleware.TierResolver) read the real-time horizon. A nil
//     resolver fails closed to the delayed public view.
//   - Cohort floor: positioning/long-short/taker responses carry no
//     publishable data below the 100-account anonymity floor — the
//     current-window answer is INSUFFICIENT_COHORT (422, registered
//     §23 code reused from Task 20.3.16) and per-bucket cells inside a
//     series are emitted suppressed=true rather than dropped.
//   - Query guard: every ClickHouse read runs under the Task 23.3.8
//     HistoryQueryGuard 10s timeout (deadline → HISTORICAL_QUERY_TIMEOUT
//     504); identical closed-interval requests hit the best-effort
//     Redis cache — a cache outage passes straight through to the
//     store (Task 23.3.8 contract).
//   - No per-account attribution ever: the wire documents below carry
//     aggregates only — tests assert the serialized output contains no
//     account/order/position identifiers.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/gateway"
	"exchange/internal/marketdata"
	"exchange/internal/middleware"
	"exchange/internal/ratelimit"
)

// ---------------------------------------------------------------------------
// Deps + seams
// ---------------------------------------------------------------------------

// OIAnalyticsSource is the open-interest read seam — *marketdata.
// OIProducer satisfies it (Latest is added alongside the producer in
// marketdata/sentiment.go). History granularity: 1h/4h/1d per the task.
type OIAnalyticsSource interface {
	Latest(symbol string) (marketdata.OISample, bool)
	History(symbol string, bucketSec, limit int) ([]marketdata.OICandle, error)
}

// SentimentAnalyticsSource is the delayed-cohort read seam —
// *marketdata.SentimentProducer satisfies it. Both methods take the
// caller's delay horizon so premium tiering is decided once, here.
type SentimentAnalyticsSource interface {
	LatestCohort(symbol string, horizon time.Time) (marketdata.PositionCohort, bool)
	LongShortSeries(symbol string, bucketSec int, horizon time.Time,
		limit int) ([]marketdata.LongShortPoint, error)
}

// TakerFlowAnalytics is the bucketed taker-flow read seam —
// *marketdata.TakerFlowStore satisfies it.
type TakerFlowAnalytics interface {
	marketdata.TakerFlowSource
	marketdata.TakerFlowSeriesSource
}

// statsKV is the narrow Redis seam for response caching —
// marketdata.HistoryKV's exact shape (goredis.Cmdable satisfies it;
// tests substitute two-command fakes).
type statsKV = marketdata.HistoryKV

// MarketStatsDeps bundles the seams the six handlers need. Nil sources
// fail closed: SERVICE_DEGRADED, never an empty-but-plausible payload.
type MarketStatsDeps struct {
	Instruments InstrumentResolver // nil skips the existence check
	OI          OIAnalyticsSource
	Sentiment   SentimentAnalyticsSource
	Flow        TakerFlowAnalytics
	Performance *marketdata.VenuePerformanceService
	// Cache is the best-effort response cache (Task 23.3.8 60s closed-
	// interval cache + the Task 23.3.11 5m performance payload cache).
	Cache statsKV
	// ResolveTier maps the caller to its §8.3 tier; nil ⇒ every caller
	// is treated as public (the delayed view — fail-closed).
	ResolveTier middleware.TierResolver
	// Guard carries the Task 23.3.8 timeout/cache/masking parameters;
	// the zero value is a valid strict guard.
	Guard marketdata.HistoryQueryGuard
	// Now is the request clock (tests inject); nil → guard clock.
	Now func() time.Time
}

func (d *MarketStatsDeps) now() time.Time {
	if d != nil && d.Now != nil {
		return d.Now().UTC()
	}
	return d.Guard.Clock()
}

// dataHorizon returns the publication horizon for the request: the real
// time for premium tiers, now−SentimentPublicationDelay otherwise.
// delayed reports which was applied.
func (d *MarketStatsDeps) dataHorizon(r *http.Request) (time.Time, bool) {
	if premiumStatsTier(r, d.ResolveTier) {
		return d.now(), false
	}
	return d.now().Add(-marketdata.SentimentPublicationDelay), true
}

// premiumStatsTier reports whether the caller's resolved tier is
// entitled to the real-time (un-delayed) sentiment surface —
// professional and above, the same boundary the L3 feed uses (§8.3).
func premiumStatsTier(r *http.Request, res middleware.TierResolver) bool {
	if res == nil {
		return false
	}
	t := res(r.Context(), auth.ClaimsFrom(r.Context()))
	return t == ratelimit.TierProfessional ||
		t == ratelimit.TierInstitutional || t == ratelimit.TierAdmin
}

// tierLabel names the access tier for cache-key partitioning (the same
// URL serves different bytes per delay tier — caching them apart is the
// whole point of the key's tier component).
func tierLabel(r *http.Request, res middleware.TierResolver) string {
	if res == nil {
		return string(ratelimit.TierPublic)
	}
	return string(res(r.Context(), auth.ClaimsFrom(r.Context())))
}

// statsQuery runs fn under the 10s history-query guard and maps a
// deadline trip to HISTORICAL_QUERY_TIMEOUT. Returns false when it
// already wrote the error response.
func statsQuery[T any](w http.ResponseWriter, r *http.Request,
	g marketdata.HistoryQueryGuard, fn func(ctx context.Context) (T, error)) (T, bool) {
	var zero T
	ctx, cancel := g.QueryContext(r.Context())
	defer cancel()
	v, err := fn(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(ctx.Err(), context.DeadlineExceeded) {
			WriteError(w, "HISTORICAL_QUERY_TIMEOUT",
				"market statistics query exceeded the 10-second bound — "+
					"narrow the symbol/period window",
				gateway.RequestIDFrom(r.Context()), nil)
			return zero, false
		}
		if errors.Is(err, marketdata.ErrUnknownInstrument) {
			WriteError(w, "NOT_FOUND", "unknown symbol",
				gateway.RequestIDFrom(r.Context()), nil)
			return zero, false
		}
		WriteError(w, "SERVICE_DEGRADED",
			"market statistics store unavailable",
			gateway.RequestIDFrom(r.Context()), nil)
		return zero, false
	}
	return v, true
}

// parseMarketStatsLimit bounds ?limit= for the series endpoints
// (default 100, max 500 — bounded windows, no cursor contract).
func parseMarketStatsLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 100, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errors.New("limit must be a positive integer")
	}
	if n > 500 {
		return 0, errors.New("limit exceeds max 500")
	}
	return n, nil
}

// cacheRead serves a cached response body when the interval is closed
// (cache contract: only closed windows may be frozen, Task 23.3.8).
// Returns true when the response was written.
func (d *MarketStatsDeps) cacheRead(w http.ResponseWriter, r *http.Request,
	closed bool) bool {
	if d == nil || d.Cache == nil || !closed {
		return false
	}
	key := marketdata.HistoryCacheKey(r.Method, r.URL.Path, "json",
		tierLabel(r, d.ResolveTier), r.URL.Query())
	payload, ok := d.Guard.CacheGet(r.Context(), d.Cache, key)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
	return true
}

// cachePut stores the serialized response when the interval is closed.
func (d *MarketStatsDeps) cachePut(r *http.Request, closed bool, payload []byte) {
	if d == nil || d.Cache == nil || !closed {
		return
	}
	key := marketdata.HistoryCacheKey(r.Method, r.URL.Path, "json",
		tierLabel(r, d.ResolveTier), r.URL.Query())
	d.Guard.CachePut(r.Context(), d.Cache, key, payload)
}

// ---------------------------------------------------------------------------
// GET /api/v1/analytics/open-interest/{symbol}?interval=&limit=
// ---------------------------------------------------------------------------

// oiIntervals are the Task 23.3.6 granularities (1h/4h/1d).
var oiIntervals = map[string]int{"1h": 3600, "4h": 14400, "1d": 86400}

// AnalyticsOpenInterest serves current open interest plus the candle
// history at the requested granularity. The OI aggregate is already
// anonymized (spec §10.8 item 1) so no cohort gate or delay applies —
// but the history is the Task 6.3.23 producer's in-memory 24h ring
// (a durable ClickHouse oi_history read model is a pending schema-track
// addition; the seam accepts it unchanged).
func AnalyticsOpenInterest(d *MarketStatsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.OI == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"open-interest store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}
		interval := r.URL.Query().Get("interval")
		if interval == "" {
			interval = "1h"
		}
		sec, ok := oiIntervals[interval]
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"interval must be one of 1h|4h|1d",
				gateway.RequestIDFrom(r.Context()),
				map[string]any{"intervals": []string{"1h", "4h", "1d"}})
			return
		}
		limit, err := parseMarketStatsLimit(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}

		data := map[string]any{
			"symbol":   symbol,
			"interval": interval,
		}
		if cur, ok := d.OI.Latest(symbol); ok {
			data["current"] = map[string]any{
				"open_interest":          cur.OpenInterest.StringFixed(8),
				"open_interest_notional": cur.Notional.StringFixed(8),
				"positions":              cur.Positions,
				"stale": d.now().Sub(cur.AsOf) >
					marketdata.DefaultOIStaleGate,
				"as_of_ms": cur.AsOf.UnixMilli(),
			}
		} else {
			// Never a fabricated zero — a never-observed symbol marks
			// insufficient_data rather than inventing an empty book.
			data["current"] = nil
			data["insufficient_data"] = true
		}
		series, err := d.OI.History(symbol, sec, limit)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if series == nil {
			series = []marketdata.OICandle{}
		}
		data["series"] = series
		WriteJSON(w, http.StatusOK, map[string]any{"data": data})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/market/open-interest?symbol= — Phase-06 spec-path spelling
// ---------------------------------------------------------------------------

// MarketOpenInterest is the ?symbol=-query spelling of
// AnalyticsOpenInterest: the Phase-06 open-interest snapshot surface
// (registry route GET /api/v1/market/open-interest) normalizes the query
// parameter onto the analytics handler's {symbol} path contract.
func MarketOpenInterest(d *MarketStatsDeps) http.HandlerFunc {
	inner := AnalyticsOpenInterest(d)
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.URL.Query().Get("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		r.SetPathValue("symbol", symbol)
		inner(w, r)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/analytics/long-short-ratio/{symbol}?period=&limit=
// ---------------------------------------------------------------------------

// AnalyticsLongShortRatio serves the delayed long/short account-ratio
// series. The latest delayed cohort is the publish gate: below the
// 100-account floor the whole response is INSUFFICIENT_COHORT (the
// aggregate cannot be published at all — per-bucket suppression inside
// the series marks cells, a dead endpoint is a 422).
func AnalyticsLongShortRatio(d *MarketStatsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Sentiment == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"sentiment store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}
		period := r.URL.Query().Get("period")
		if period == "" {
			period = "5m"
		}
		sec, ok := marketdata.SentimentPeriodSec(period)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"period must be one of the published widths",
				gateway.RequestIDFrom(r.Context()), map[string]any{
					"periods": marketdata.SentimentPeriodLabels(),
				})
			return
		}
		limit, err := parseMarketStatsLimit(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		horizon, delayed := d.dataHorizon(r)
		// The series is closed-interval for the delayed tier (its tail
		// ends ≥5m in the past) — those responses may be cached; the
		// premium real-time tail is never frozen.
		if d.cacheRead(w, r, delayed) {
			return
		}

		latest, found := d.Sentiment.LatestCohort(symbol, horizon)
		switch {
		case !found:
			// Producer warmup / never-observed symbol: no sample has
			// aged past the delay horizon. That's an insufficient-data
			// marker, not a fault — and never a fabricated zero.
			WriteJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
				"symbol":            symbol,
				"period":            period,
				"delayed":           delayed,
				"insufficient_data": true,
				"points":            []marketdata.LongShortPoint{},
			}})
			return
		case latest.Suppressed(marketdata.SentimentMinCohortAccounts):
			WriteError(w, "INSUFFICIENT_COHORT",
				"symbol cohort below the 100-account anonymity floor",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		points, err := d.Sentiment.LongShortSeries(symbol, sec, horizon, limit)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if points == nil {
			points = []marketdata.LongShortPoint{}
		}
		payload, _ := json.Marshal(map[string]any{"data": map[string]any{
			"symbol":   symbol,
			"period":   period,
			"delay_ms": marketdata.SentimentPublicationDelay.Milliseconds(),
			"delayed":  delayed,
			"as_of_ms": horizon.UnixMilli(),
			"points":   points,
		}})
		d.cachePut(r, delayed, payload)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
}

// ---------------------------------------------------------------------------
// Taker flow / taker volume series (shared engine)
// ---------------------------------------------------------------------------

// takerFlowPoint is the wire cell for both taker endpoints — aggregate
// notional/volume splits plus the distinct-participant cohort and the
// per-cell suppression flag. Ratios are decimal strings; a nil ratio
// stays absent (no fabricated 0/∞).
type takerFlowPoint struct {
	BucketStartMs   int64  `json:"bucket_start_ms"`
	Suppressed      bool   `json:"suppressed"`
	Trades          int64  `json:"trades,omitempty"`
	BuyNotional     string `json:"buy_notional,omitempty"`
	SellNotional    string `json:"sell_notional,omitempty"`
	UnknownNotional string `json:"unknown_notional,omitempty"`
	BuyVolume       string `json:"buy_volume,omitempty"`
	SellVolume      string `json:"sell_volume,omitempty"`
	BuySellRatio    string `json:"buy_sell_ratio,omitempty"`
}

func takerFlowDoc(b marketdata.TakerFlowBucket) takerFlowPoint {
	p := takerFlowPoint{
		BucketStartMs: b.BucketStart.UTC().UnixMilli(),
		Suppressed:    b.Flow.Accounts < marketdata.SentimentMinCohortAccounts,
	}
	if p.Suppressed {
		return p // cohort below floor — the cell publishes nothing else
	}
	p.Trades = b.Flow.Trades
	p.BuyNotional = b.Flow.BuyNotional.StringFixed(8)
	p.SellNotional = b.Flow.SellNotional.StringFixed(8)
	if b.Flow.UnknownNotional.IsPositive() {
		p.UnknownNotional = b.Flow.UnknownNotional.StringFixed(8)
	}
	p.BuyVolume = b.Flow.BuyVolume.StringFixed(8)
	p.SellVolume = b.Flow.SellVolume.StringFixed(8)
	if r := b.Flow.BuySellRatio(); r != nil {
		p.BuySellRatio = r.StringFixed(6)
	}
	return p
}

// serveTakerFlow implements both series endpoints:
//
//	GET /api/v1/analytics/taker-flow/{symbol}?period=
//	GET /api/v1/market/taker-volume?symbol=&interval=
//
// symbolParam selects the source of the symbol (path vs query);
// periodParam selects the bucket-width query key ("period" or
// "interval" — both accept the same {5m|15m|1h|4h|24h} set).
func serveTakerFlow(d *MarketStatsDeps, pathSymbol bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Flow == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"taker-flow store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		if !pathSymbol {
			symbol = r.URL.Query().Get("symbol")
		}
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}
		period := r.URL.Query().Get("period")
		if period == "" {
			period = r.URL.Query().Get("interval")
		}
		if period == "" {
			period = "5m"
		}
		sec, ok := marketdata.SentimentPeriodSec(period)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"period must be one of the published widths",
				gateway.RequestIDFrom(r.Context()), map[string]any{
					"periods": marketdata.SentimentPeriodLabels(),
				})
			return
		}
		limit, err := parseMarketStatsLimit(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		horizon, delayed := d.dataHorizon(r)
		if d.cacheRead(w, r, delayed) {
			return
		}

		// Bounded window: limit buckets of sec width ending at the
		// delay horizon — the 5m delay is enforced by the query bound
		// itself, so no post-hoc trimming can leak fresher data.
		from := horizon.Add(-time.Duration(limit) * time.Duration(sec) * time.Second)
		buckets, ok := statsQuery(w, r, d.Guard, func(ctx context.Context) ([]marketdata.TakerFlowBucket, error) {
			return d.Flow.Buckets(ctx, symbol, from, horizon, sec)
		})
		if !ok {
			return
		}

		points := make([]takerFlowPoint, 0, len(buckets))
		publishable := false
		for _, b := range buckets {
			p := takerFlowDoc(b)
			if !p.Suppressed {
				publishable = true
			}
			points = append(points, p)
		}
		if len(buckets) > 0 && !publishable {
			// Every cell sits below the anonymity floor — the response
			// itself is suppressed rather than a page of redacted cells.
			WriteError(w, "INSUFFICIENT_COHORT",
				"symbol cohort below the 100-account anonymity floor",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		payload, _ := json.Marshal(map[string]any{"data": map[string]any{
			"symbol":   symbol,
			"period":   period,
			"delay_ms": marketdata.SentimentPublicationDelay.Milliseconds(),
			"delayed":  delayed,
			"as_of_ms": horizon.UnixMilli(),
			"points":   points,
		}})
		d.cachePut(r, delayed, payload)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
}

// AnalyticsTakerFlow serves GET /api/v1/analytics/taker-flow/{symbol}.
func AnalyticsTakerFlow(d *MarketStatsDeps) http.HandlerFunc {
	return serveTakerFlow(d, true)
}

// MarketTakerVolume serves GET /api/v1/market/taker-volume?symbol=&interval=.
func MarketTakerVolume(d *MarketStatsDeps) http.HandlerFunc {
	return serveTakerFlow(d, false)
}

// ---------------------------------------------------------------------------
// GET /api/v1/market/positioning?symbol=
// ---------------------------------------------------------------------------

// MarketPositioning serves the delayed long/short account ratio plus
// the top-position concentration bands for one symbol (Task 23.3.10).
// The read is the SentimentProducer's delayed-cohort ring — positions
// are current-state, so the ring is what makes the 5m delay real rather
// than decorative.
func MarketPositioning(d *MarketStatsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Sentiment == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"positioning store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.URL.Query().Get("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}
		horizon, delayed := d.dataHorizon(r)
		if d.cacheRead(w, r, delayed) {
			return
		}

		c, found := d.Sentiment.LatestCohort(symbol, horizon)
		if !found {
			// Same warmup/never-observed semantics as long-short-ratio.
			WriteJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
				"symbol":            symbol,
				"delayed":           delayed,
				"insufficient_data": true,
			}})
			return
		}
		if c.Suppressed(marketdata.SentimentMinCohortAccounts) {
			WriteError(w, "INSUFFICIENT_COHORT",
				"symbol cohort below the 100-account anonymity floor",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		data := map[string]any{
			"symbol":         symbol,
			"as_of_ms":       c.AsOf.UnixMilli(),
			"delay_ms":       marketdata.SentimentPublicationDelay.Milliseconds(),
			"delayed":        delayed,
			"accounts":       c.Accounts,
			"long_accounts":  c.LongAccounts,
			"short_accounts": c.ShortAccounts,
			"long_notional":  c.LongNotional.StringFixed(8),
			"short_notional": c.ShortNotional.StringFixed(8),
			"gross_notional": c.GrossNotional.StringFixed(8),
		}
		if r := c.LongRatio(); r != nil {
			data["long_ratio"] = r.StringFixed(6)
		}
		if r := c.ShortRatio(); r != nil {
			data["short_ratio"] = r.StringFixed(6)
		}
		if r := c.LongShortRatio(); r != nil {
			data["long_short_ratio"] = r.StringFixed(6)
		}
		concentration := map[string]any{}
		if c.Top5Share != nil {
			concentration["top5_pct_share"] = c.Top5Share.StringFixed(6)
		}
		if c.Top10Share != nil {
			concentration["top10_pct_share"] = c.Top10Share.StringFixed(6)
		}
		if c.Top25Share != nil {
			concentration["top25_pct_share"] = c.Top25Share.StringFixed(6)
		}
		data["concentration"] = concentration

		payload, _ := json.Marshal(map[string]any{"data": data})
		d.cachePut(r, delayed, payload)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/market/performance
// ---------------------------------------------------------------------------

// MarketPerformance serves the aggregate-only venue statistics document
// (Task 23.3.11). The response is cached whole in Redis for
// PerformanceCacheTTL (5m) — the key is fixed because the endpoint
// takes no parameters. Cache outage = best-effort pass-through; a held
// (divergent) report is cached too — staleness is disclosed by its
// status/held_age_ms fields either way.
func MarketPerformance(d *MarketStatsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The cache is consulted first: a warm entry keeps serving
		// through a downstream outage (its status fields disclose age).
		if d != nil && d.Cache != nil {
			if payload, ok := cacheGet(r.Context(), d.Cache,
				marketdata.PerformanceCacheKey); ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(payload)
				return
			}
		}
		if d == nil || d.Performance == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"performance statistics not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ctx, cancel := d.Guard.QueryContext(r.Context())
		defer cancel()
		rep, err := d.Performance.Report(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(ctx.Err(), context.DeadlineExceeded) {
				WriteError(w, "HISTORICAL_QUERY_TIMEOUT",
					"performance statistics query exceeded the 10-second bound",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			WriteError(w, "SERVICE_DEGRADED",
				"performance statistics unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		payload, _ := json.Marshal(map[string]any{"data": rep})
		if d.Cache != nil {
			_ = d.Cache.Set(r.Context(), marketdata.PerformanceCacheKey,
				payload, marketdata.PerformanceCacheTTL).Err()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
}

// cacheGet is CacheGet without the guard TTL semantics — the
// performance payload cache lives outside the closed-interval rule (the
// service's own 15m delay + held-state machine govern staleness).
func cacheGet(ctx context.Context, kv statsKV, key string) ([]byte, bool) {
	res, err := kv.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	return res, true
}

// ---------------------------------------------------------------------------
// Wiring adapters (orchestrator binds these in cmd/gateway)
// ---------------------------------------------------------------------------

// venueFillRateSource adapts *analytics.VolumeStatsStore (the
// volume_stats '1d' counter reader) to marketdata.VenueFillRateSource —
// a pure row-shape projection; the CH query lives in the sibling store.
type venueFillRateSource struct {
	store *analytics.VolumeStatsStore
}

// NewVenueFillRateSource binds the volume_stats counter reader.
func NewVenueFillRateSource(store *analytics.VolumeStatsStore) marketdata.VenueFillRateSource {
	return venueFillRateSource{store}
}

func (s venueFillRateSource) FillRates(ctx context.Context, from, to time.Time) ([]marketdata.VenueFillCounter, error) {
	rows, err := s.store.FillRates(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]marketdata.VenueFillCounter, len(rows))
	for i, r := range rows {
		out[i] = marketdata.VenueFillCounter{
			Symbol:          r.Symbol,
			OrdersSubmitted: r.OrdersSubmitted,
			OrdersFilled:    r.OrdersFilled,
		}
	}
	return out, nil
}

// RTS27FiguresSource adapts the compliance RTS-27 service's public
// listing to the marketdata seam. Only PUBLISHED artifacts cross — and
// only their aggregate metrics map; the row's actor columns
// (generated_by/published_by) are dropped here and never re-emitted.
type RTS27FiguresSource struct {
	Svc *compliance.RTS27Service
}

// PublishedFigures implements marketdata.RTS27Source.
func (a RTS27FiguresSource) PublishedFigures(ctx context.Context) ([]marketdata.RTS27Figure, error) {
	if a.Svc == nil {
		return nil, nil
	}
	reps, err := a.Svc.ListPublished(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]marketdata.RTS27Figure, 0, len(reps))
	for _, rep := range reps {
		var metrics map[string]any
		if len(rep.Metrics) > 0 {
			if err := json.Unmarshal(rep.Metrics, &metrics); err != nil {
				return nil, err
			}
		}
		out = append(out, marketdata.RTS27Figure{
			QuarterStart:    rep.QuarterStart,
			InstrumentClass: rep.InstrumentClass,
			Version:         rep.Version,
			DaysCovered:     rep.DaysCovered,
			Metrics:         metrics,
			PublishedAt:     rep.PublishedAt,
		})
	}
	return out, nil
}
