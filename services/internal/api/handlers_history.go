// Phase-20 Tasks 20.3.2/20.3.3 + Phase-23 Tasks 23.3.1/23.3.4/23.3.8 —
// historical tick, kline and trade-record reads off the ClickHouse
// analytics projection (spec §10/§14/§16.1/§16.2, §24 #236/#325):
//
//	GET /api/v1/history/ticks/{symbol}?from=&to=&limit=&cursor=
//	GET /api/v1/history/klines/{symbol}?interval=1m&from=&to=&limit=&cursor=
//	GET /api/v1/history/trades/{symbol}?from=&to=&limit=&cursor=
//
// Data provenance (fail-closed, spec §2.7): every endpoint reads ONLY
// the ClickHouse cold tier (analytics.TickStore / analytics.OHLCVStore /
// marketdata.TradeHistoryStore) — PostgreSQL fx_klines remains the hot
// read model served by MarketKlines and there is intentionally NO
// fallback between tiers: a cold-tier outage answers SERVICE_DEGRADED
// rather than silently substituting a differently-semanticed store.
// Unknown symbols → 404 NOT_FOUND after an instruments lookup, never a
// plausible-looking empty page.
//
// Phase-23 hardening layered on top:
//   - Access tiers (Task 23.3.4, remediation #35): free = Public/Basic
//     → last-30-days window + ≥15-minute publication delay; premium =
//     Professional/Institutional → full history, real-time; staff =
//     compliance → exempt. HistoryTierResolver is a deps seam: nil →
//     free (safe default); resolver error → free + degraded flag.
//   - Query guards (Task 23.3.8, §24 #325): a hard 10s context timeout
//     bounds every CH query → deadline trips answer
//     HISTORICAL_QUERY_TIMEOUT (504) with a narrower-range hint; a
//     best-effort Redis cache (60s TTL, keyed by method+path+sorted
//     query+format+tier) dedups identical CLOSED-interval reads and
//     never blocks the query on a Redis outage; participant fields on
//     the trades projection are masked unconditionally for non-staff
//     tiers and for pre-open window rows (ticks carry no participant
//     columns — schema 001 — so masking lands on trades per the task
//     guidance).
//   - Content negotiation on the ticks endpoint (Task 23.3.4 item 4):
//     Accept: text/csv → CSV rows, Accept: application/x-fix → minimal
//     FIX drop-copy tag=value lines, anything else → the §8.8 JSON
//     envelope {data, next_cursor, limit, total}.
//
// Rate limiting rides the registered route tiers (routes_v1.go):
// ticks → TierBasic (line ~593), klines/trades → TierPublic — the
// per-access-tier data entitlement is orthogonal and enforced here.
//
// from/to are RFC3339 ([from, to) bounds); limit/cursor follow the §8.8
// unified envelope.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/marketapi"
	"exchange/internal/marketdata"
)

// HistoryTickSource is the cold-tier tick read seam —
// *analytics.TickStore satisfies it; tests inject fakes.
type HistoryTickSource interface {
	Query(ctx context.Context, q analytics.TickQuery) ([]analytics.Tick, error)
}

// HistoryKlineSource is the cold-tier candle read seam —
// *analytics.OHLCVStore satisfies it.
type HistoryKlineSource interface {
	Query(ctx context.Context, q analytics.OHLCVQuery) ([]analytics.CandleRow, error)
}

// HistoryTradeSource is the cold-tier enriched-trades read seam —
// *marketdata.TradeHistoryStore satisfies it.
type HistoryTradeSource interface {
	Query(ctx context.Context, q marketdata.TradeHistoryQuery) ([]*marketdata.HistoryTrade, error)
}

// InstrumentResolver resolves a symbol to its instrument row —
// *marketapi.PgStore satisfies it. nil disables the existence check
// (embedders that pre-validate symbols may rely on that).
type InstrumentResolver interface {
	InstrumentBySymbol(ctx context.Context, symbol string) (*marketapi.Instrument, error)
}

// HistoryDeps bundles the seams the history handlers need.
type HistoryDeps struct {
	Ticks       HistoryTickSource
	Klines      HistoryKlineSource
	Trades      HistoryTradeSource
	Instruments InstrumentResolver
	// Tiers resolves the caller's Task-23.3.4 access tier. nil → free
	// (the safe default — under-entitlement is never a correctness
	// violation); a resolver error yields free + the degraded flag.
	Tiers marketdata.HistoryTierResolver
	// Guard carries the Task-23.3.8 timeout/cache/pre-open parameters;
	// the zero value is the §24 #325 contract (10s / 60s / 15min).
	Guard marketdata.HistoryQueryGuard
	// Cache is the best-effort response store — any redis.Cmdable
	// satisfies the seam; nil disables caching (never required).
	Cache marketdata.HistoryKV
	// SessionOpen resolves the open time whose bounded pre-open window
	// applies to staff-visible participant fields (production wiring:
	// the Phase-15 Task 15.3.7 session-lifecycle store). nil → no
	// pre-open masking — historical reads carry no flagged rows.
	SessionOpen func(symbol string) (time.Time, error)
}

// historyKlinesSpec is the pagination contract for the kline history
// route — same shape as the hot /api/v1/klines/{symbol} row
// (500/1500). Declared locally because the published ListSpecs table is
// owned by another task's file; the orchestrator may fold this row into
// pagination.go when the meta surface is next edited.
var historyKlinesSpec = &ListSpec{
	Path: "/api/v1/history/klines/{symbol}", Default: 500, Max: 1500,
	Sortable:   []string{"open_time"},
	Filterable: []string{"interval", "from", "to"},
}

// ---------------------------------------------------------------------------
// Shared Phase-23 plumbing
// ---------------------------------------------------------------------------

// historyEnvelope extends the §8.8 list envelope with the tier metadata
// clients need to know WHICH tape they got: the resolved access tier,
// whether the publication delay applied, and whether tier resolution
// degraded (resolver error → free). The base {data, next_cursor, limit,
// total} shape is untouched — extra keys only.
type historyEnvelope struct {
	ListEnvelope
	AccessTier string `json:"access_tier"`
	Delayed    bool   `json:"delayed"`
	Degraded   bool   `json:"degraded,omitempty"`
}

// resolveHistoryAccess maps the request to its access tier: nil
// resolver → free; resolver error → free + degraded (fail-closed to the
// RESTRICTIVE tier, never premium, spec §2.7).
func resolveHistoryAccess(d *HistoryDeps, r *http.Request) (marketdata.HistoryAccess, bool) {
	if d.Tiers == nil {
		return marketdata.HistoryAccessFree, false
	}
	a, err := d.Tiers(r.Context(), auth.ClaimsFrom(r.Context()))
	if err != nil || a == "" {
		return marketdata.HistoryAccessFree, err != nil
	}
	return a, false
}

// historyTimedOut reports whether a guarded store error is the 10s
// deadline (→ HISTORICAL_QUERY_TIMEOUT) rather than a store outage
// (→ SERVICE_DEGRADED).
func historyTimedOut(qctx context.Context, err error) bool {
	return qctx.Err() == context.DeadlineExceeded ||
		errors.Is(err, context.DeadlineExceeded)
}

// historyCacheAttempt performs the best-effort closed-interval cache
// read. Returns (key, body) where body!=nil is a HIT the caller serves
// verbatim; key!="" with nil body means the caller should CachePut the
// computed response; key=="" means uncacheable (open interval or no
// store wired).
func historyCacheAttempt(d *HistoryDeps, r *http.Request,
	access marketdata.HistoryAccess, format marketdata.HistoryFormat,
	to time.Time) (key string, body []byte) {
	if d.Cache == nil {
		return "", nil
	}
	if !d.Guard.ClosedInterval(to, marketdata.HistoryDelayFor(access)) {
		return "", nil
	}
	key = marketdata.HistoryCacheKey(r.Method, r.URL.Path,
		string(format), string(access), r.URL.Query())
	if b, hit := d.Guard.CacheGet(r.Context(), d.Cache, key); hit {
		return key, b
	}
	return key, nil
}

// writeHistoryBody emits the already-computed body with the negotiated
// content type and the cache-hit marker when served from Redis.
func writeHistoryBody(w http.ResponseWriter, format marketdata.HistoryFormat, body []byte, cached bool) {
	w.Header().Set("Content-Type", format.ContentType())
	if cached {
		w.Header().Set("X-History-Cache", "hit")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// GET /api/v1/history/ticks/{symbol}?from=&to=&limit=&cursor=
// ---------------------------------------------------------------------------

// historyTickDoc is the wire projection of one archived tick — decimal
// strings for money fields (spec §5.3), epoch-millis event time plus the
// RFC3339 form for operator readability.
type historyTickDoc struct {
	TradeID  uint64 `json:"trade_id"`
	EventSeq uint64 `json:"event_seq"`
	ShardID  uint32 `json:"shard_id"`
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
	Side     string `json:"side"`
	TimeMs   int64  `json:"time_ms"`
	Time     string `json:"time"`
}

// HistoryTicks serves the archived trade tape for one symbol, newest
// first. The (ts, trade_id) keyset cursor is opaque per §8.8. Phase-23:
// the free tier is clamped to the last-30-days / ≥15-minute-old window;
// every store call rides the 10s query guard; identical closed-interval
// requests dedup through the Redis response cache; Accept: text/csv and
// Accept: application/x-fix render page-capped CSV / drop-copy bodies.
func HistoryTicks(d *HistoryDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Ticks == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"tick history store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		p, err := ParseListParams(r, ListSpecFor("/api/v1/history/ticks/{symbol}"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		from, err := parseTimeQuery(q.Get("from"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		to, err := parseTimeQuery(q.Get("to"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}

		access, degraded := resolveHistoryAccess(d, r)
		var fromV, toV time.Time
		if from != nil {
			fromV = *from
		}
		if to != nil {
			toV = *to
		}
		win := marketdata.ResolveHistoryWindow(access, fromV, toV, d.Guard.Clock())
		format := marketdata.HistoryFormatFor(r.Header.Get("Accept"))

		render := func(ticks []analytics.Tick) []byte {
			var buf bytes.Buffer
			switch format {
			case marketdata.HistoryFormatCSV, marketdata.HistoryFormatFIX:
				rows := make([]marketdata.TickExportRow, 0, len(ticks))
				for _, t := range ticks {
					rows = append(rows, marketdata.TickExportRow{
						TradeID: t.TradeID, EventSeq: t.EventSeq,
						ShardID: t.ShardID, Symbol: t.Symbol,
						Price: t.Price, Qty: t.Qty,
						Side: t.Side, Ts: t.Ts,
					})
				}
				if format == marketdata.HistoryFormatCSV {
					_ = marketdata.WriteTicksCSV(&buf, rows)
				} else {
					_ = marketdata.WriteTicksFIXDropCopy(&buf, rows)
				}
			default:
				docs := make([]historyTickDoc, 0, len(ticks))
				for _, t := range ticks {
					docs = append(docs, historyTickDoc{
						TradeID:  t.TradeID,
						EventSeq: t.EventSeq,
						ShardID:  t.ShardID,
						Price:    t.Price.StringFixed(8),
						Quantity: t.Qty.StringFixed(8),
						Side:     t.Side,
						TimeMs:   t.Ts.UTC().UnixMilli(),
						Time:     t.Ts.UTC().Format(time.RFC3339),
					})
				}
				env := historyEnvelope{
					ListEnvelope: NewListEnvelope(docs, p,
						PageCursors(ticks, func(t analytics.Tick) (time.Time, int64) {
							return t.Ts, int64(t.TradeID)
						}), int64(len(ticks))),
					AccessTier: string(access),
					Delayed:    marketdata.HistoryDelayFor(access) > 0,
					Degraded:   degraded,
				}
				body, _ := json.Marshal(env)
				return body
			}
			return buf.Bytes()
		}

		// A clamped-empty range is a legitimate empty page — no store
		// round-trip (and nothing is eligible under this tier anyway).
		if win.Empty() {
			writeHistoryBody(w, format, render(nil), false)
			return
		}

		key, cached := historyCacheAttempt(d, r, access, format, win.To)
		if cached != nil {
			writeHistoryBody(w, format, cached, true)
			return
		}

		tq := analytics.TickQuery{Symbol: symbol, Limit: p.Limit}
		if !win.From.IsZero() {
			tq.From = win.From
		}
		if !win.To.IsZero() {
			tq.To = win.To
		}
		if p.Decoded != nil {
			tq.After = &analytics.TickCursor{
				Ts:      p.Decoded.CreatedAt,
				TradeID: uint64(p.Decoded.ID),
			}
		}
		qctx, cancel := d.Guard.QueryContext(r.Context())
		ticks, qerr := d.Ticks.Query(qctx, tq)
		cancel()
		if qerr != nil {
			if historyTimedOut(qctx, qerr) {
				WriteError(w, "HISTORICAL_QUERY_TIMEOUT",
					"historical tick query exceeded the query timeout",
					gateway.RequestIDFrom(r.Context()), map[string]any{
						"hint": "narrow the from/to range or reduce limit",
					})
				return
			}
			WriteError(w, "SERVICE_DEGRADED",
				"tick history store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		body := render(ticks)
		if key != "" {
			d.Guard.CachePut(r.Context(), d.Cache, key, body)
		}
		writeHistoryBody(w, format, body, false)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/history/klines/{symbol}?interval=1m&from=&to=&limit=
// ---------------------------------------------------------------------------

// historyKlineDoc mirrors the marketapi.Kline wire shape so clients can
// consume hot and cold tiers identically.
type historyKlineDoc struct {
	OpenTimeMs  int64  `json:"open_time_ms"`
	Open        string `json:"open"`
	High        string `json:"high"`
	Low         string `json:"low"`
	Close       string `json:"close"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quote_volume"`
	TradeCount  int64  `json:"trade_count"`
	Closed      bool   `json:"closed"`
}

// HistoryKlines serves closed candles from the ClickHouse projection.
// interval is REQUIRED to be one of the 12 persisted timeframes — the
// memory-only 1s bar never crosses the archive boundary, and unknown
// labels are 400 INVALID_REQUEST rather than an empty page (a typo must
// not look like a quiet market). The cursor is the §8.8 keyset token
// over open_time (ascending stream). The §24 #325 timeout + closed-
// interval cache guards apply; the candle surface keeps its Phase-20
// public contract (no per-tier window clamping — aggregate bars are the
// free product).
func HistoryKlines(d *HistoryDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Klines == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"kline history store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		p, err := ParseListParams(r, historyKlinesSpec)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		interval := q.Get("interval")
		if interval == "" {
			interval = "1m"
		}
		if !analytics.PersistedInterval(interval) {
			WriteError(w, "INVALID_REQUEST",
				"interval must be one of the 12 persisted timeframes "+
					"(1s is memory-only)",
				gateway.RequestIDFrom(r.Context()), map[string]any{
					"intervals": analytics.PersistedIntervalLabels(),
				})
			return
		}
		from, err := parseTimeQuery(q.Get("from"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		to, err := parseTimeQuery(q.Get("to"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}

		render := func(bars []analytics.CandleRow) []byte {
			docs := make([]historyKlineDoc, 0, len(bars))
			for _, b := range bars {
				docs = append(docs, historyKlineDoc{
					OpenTimeMs:  b.OpenTime.UTC().UnixMilli(),
					Open:        b.Open.StringFixed(8),
					High:        b.High.StringFixed(8),
					Low:         b.Low.StringFixed(8),
					Close:       b.Close.StringFixed(8),
					Volume:      b.Volume.StringFixed(8),
					QuoteVolume: b.QuoteVolume.StringFixed(8),
					TradeCount:  b.TradeCount,
					Closed:      true, // the archive tier only holds closed bars
				})
			}
			env := NewListEnvelope(docs, p,
				PageCursors(bars, func(b analytics.CandleRow) (time.Time, int64) {
					return b.OpenTime, 0
				}), int64(len(bars)))
			body, _ := json.Marshal(env)
			return body
		}

		var toV time.Time
		if to != nil {
			toV = *to
		}
		key, cached := historyCacheAttempt(d, r,
			marketdata.HistoryAccessPremium, marketdata.HistoryFormatJSON, toV)
		if cached != nil {
			writeHistoryBody(w, marketdata.HistoryFormatJSON, cached, true)
			return
		}

		kq := analytics.OHLCVQuery{
			Symbol: symbol, Interval: interval, Limit: p.Limit,
		}
		if from != nil {
			kq.From = *from
		}
		if to != nil {
			kq.To = *to
		}
		if p.Decoded != nil {
			after := p.Decoded.CreatedAt
			kq.After = &after
		}
		qctx, cancel := d.Guard.QueryContext(r.Context())
		bars, qerr := d.Klines.Query(qctx, kq)
		cancel()
		if qerr != nil {
			if historyTimedOut(qctx, qerr) {
				WriteError(w, "HISTORICAL_QUERY_TIMEOUT",
					"historical kline query exceeded the query timeout",
					gateway.RequestIDFrom(r.Context()), map[string]any{
						"hint": "narrow the from/to range or reduce limit",
					})
				return
			}
			WriteError(w, "SERVICE_DEGRADED",
				"kline history store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		body := render(bars)
		if key != "" {
			d.Guard.CachePut(r.Context(), d.Cache, key, body)
		}
		writeHistoryBody(w, marketdata.HistoryFormatJSON, body, false)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/history/trades/{symbol}?from=&to=&limit=&cursor=
// ---------------------------------------------------------------------------

// historyTradeDoc is the wire projection of one enriched trades row.
// Participant fields are emitted as strings so the masked form is ""
// verbatim (Task 23.3.8: "masked to ”"); 0-valued ids render "" —
// unresolved lineage (instrument_id 0) reads as "" too, never a
// fabricated account.
type historyTradeDoc struct {
	TradeID        uint64 `json:"trade_id"`
	InstrumentID   int64  `json:"instrument_id"`
	EventSeq       uint64 `json:"event_seq"`
	ShardID        uint32 `json:"shard_id"`
	Price          string `json:"price"`
	Quantity       string `json:"quantity"`
	Side           string `json:"side"` // aggressor side
	MakerAccountID string `json:"maker_account_id"`
	TakerAccountID string `json:"taker_account_id"`
	BuyOrderID     string `json:"buy_order_id"`
	SellOrderID    string `json:"sell_order_id"`
	TimeMs         int64  `json:"time_ms"`
	Time           string `json:"time"`
}

// idStr renders a participant id for the wire; 0 → "" (masked/absent).
func idStr(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// HistoryTrades serves the enriched CH `trades` projection (schema 002:
// the public tape plus instrument/order/account lineage), newest first,
// §8.8 keyset-paginated, under the same tier window + delay and
// query-timeout/cache guards as the ticks endpoint.
//
// Participant privacy (Task 23.3.8): the `ticks` schema carries no
// participant columns, so masking lands here. Non-staff tiers get the
// unconditional public-tape mask — counterparty identity never leaves
// the venue on the product endpoint. Staff rows additionally pass the
// bounded pre-open mask (SessionOpen seam; the 24/5 venue's pre-open
// window is minutes, and rows older than the current session are
// untouched). Compliance's fully-unmasked channel is the §21 audit
// surface, not this market-data product.
func HistoryTrades(d *HistoryDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d == nil || d.Trades == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"trades history store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		symbol := r.PathValue("symbol")
		p, err := ParseListParams(r, ListSpecFor("/api/v1/history/trades/{symbol}"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		from, err := parseTimeQuery(q.Get("from"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		to, err := parseTimeQuery(q.Get("to"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !resolveInstrument(w, r, d.Instruments, symbol) {
			return
		}

		access, degraded := resolveHistoryAccess(d, r)
		var fromV, toV time.Time
		if from != nil {
			fromV = *from
		}
		if to != nil {
			toV = *to
		}
		win := marketdata.ResolveHistoryWindow(access, fromV, toV, d.Guard.Clock())

		render := func(rows []*marketdata.HistoryTrade) []byte {
			docs := make([]historyTradeDoc, 0, len(rows))
			for _, t := range rows {
				pf := t.Participants
				docs = append(docs, historyTradeDoc{
					TradeID:        t.TradeID,
					InstrumentID:   t.InstrumentID,
					EventSeq:       t.EventSeq,
					ShardID:        t.ShardID,
					Price:          t.Price.StringFixed(8),
					Quantity:       t.Qty.StringFixed(8),
					Side:           t.AggressorSide,
					MakerAccountID: idStr(pf.MakerAccountID),
					TakerAccountID: idStr(pf.TakerAccountID),
					BuyOrderID:     idStr(int64(pf.BuyOrderID)),
					SellOrderID:    idStr(int64(pf.SellOrderID)),
					TimeMs:         t.Ts.UTC().UnixMilli(),
					Time:           t.Ts.UTC().Format(time.RFC3339),
				})
			}
			env := historyEnvelope{
				ListEnvelope: NewListEnvelope(docs, p,
					PageCursors(rows, func(t *marketdata.HistoryTrade) (time.Time, int64) {
						return t.Ts, int64(t.TradeID)
					}), int64(len(rows))),
				AccessTier: string(access),
				Delayed:    marketdata.HistoryDelayFor(access) > 0,
				Degraded:   degraded,
			}
			body, _ := json.Marshal(env)
			return body
		}

		if win.Empty() {
			writeHistoryBody(w, marketdata.HistoryFormatJSON, render(nil), false)
			return
		}

		key, cached := historyCacheAttempt(d, r, access,
			marketdata.HistoryFormatJSON, win.To)
		if cached != nil {
			writeHistoryBody(w, marketdata.HistoryFormatJSON, cached, true)
			return
		}

		tq := marketdata.TradeHistoryQuery{Symbol: symbol, Limit: p.Limit}
		if !win.From.IsZero() {
			tq.From = win.From
		}
		if !win.To.IsZero() {
			tq.To = win.To
		}
		if p.Decoded != nil {
			tq.After = &marketdata.TradeHistoryCursor{
				Ts:      p.Decoded.CreatedAt,
				TradeID: uint64(p.Decoded.ID),
			}
		}
		qctx, cancel := d.Guard.QueryContext(r.Context())
		rows, qerr := d.Trades.Query(qctx, tq)
		cancel()
		if qerr != nil {
			if historyTimedOut(qctx, qerr) {
				WriteError(w, "HISTORICAL_QUERY_TIMEOUT",
					"historical trades query exceeded the query timeout",
					gateway.RequestIDFrom(r.Context()), map[string]any{
						"hint": "narrow the from/to range or reduce limit",
					})
				return
			}
			WriteError(w, "SERVICE_DEGRADED",
				"trades history store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}

		// Masking ladder (§24 #325): staff keeps participant fields,
		// subject to the bounded pre-open mask; every other tier gets
		// the unconditional public-tape mask.
		if access == marketdata.HistoryAccessStaff {
			if d.SessionOpen != nil {
				open, oerr := d.SessionOpen(symbol)
				if oerr != nil {
					WriteError(w, "SERVICE_DEGRADED",
						"session-open resolver unavailable",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
				marketdata.MaskPreOpenWindowed(rows, open, d.Guard.MaskingWindow())
			}
		} else {
			marketdata.MaskParticipantFields(rows)
		}

		body := render(rows)
		if key != "" {
			d.Guard.CachePut(r.Context(), d.Cache, key, body)
		}
		writeHistoryBody(w, marketdata.HistoryFormatJSON, body, false)
	}
}

// resolveInstrument is the shared unknown-symbol gate: 404 NOT_FOUND for
// a symbol the venue does not list, SERVICE_DEGRADED on a lookup
// failure. A nil resolver skips the check (returns true).
func resolveInstrument(w http.ResponseWriter, r *http.Request,
	res InstrumentResolver, symbol string) bool {
	if res == nil {
		return true
	}
	inst, err := res.InstrumentBySymbol(r.Context(), symbol)
	if err != nil {
		WriteError(w, "SERVICE_DEGRADED",
			"instrument store unavailable",
			gateway.RequestIDFrom(r.Context()), nil)
		return false
	}
	if inst == nil {
		WriteError(w, "NOT_FOUND",
			"unknown symbol", gateway.RequestIDFrom(r.Context()), nil)
		return false
	}
	return true
}
