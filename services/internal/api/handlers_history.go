// Phase-20 Tasks 20.3.2/20.3.3 — historical tick + kline reads off the
// ClickHouse analytics projection (spec §16.1/§16.2):
//
//	GET /api/v1/history/ticks/{symbol}?from=&to=&limit=&cursor=
//	GET /api/v1/history/klines/{symbol}?interval=1m&from=&to=&limit=&cursor=
//
// Data provenance (fail-closed, spec §2.7): both endpoints read ONLY the
// ClickHouse cold tier via analytics.TickStore / analytics.OHLCVStore —
// PostgreSQL fx_klines remains the hot read model served by MarketKlines
// and there is intentionally NO fallback between tiers: a cold-tier
// outage answers SERVICE_DEGRADED rather than silently substituting a
// differently-semanticed store. Unknown symbols → 404 NOT_FOUND after an
// instruments lookup, never a plausible-looking empty page.
//
// from/to are RFC3339 ([from, to) bounds); limit/cursor follow the §8.8
// unified envelope ({data, next_cursor, limit, total}).
package api

import (
	"context"
	"net/http"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/gateway"
	"exchange/internal/marketapi"
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
	Instruments InstrumentResolver
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
// first. The (ts, trade_id) keyset cursor is opaque per §8.8.
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

		tq := analytics.TickQuery{Symbol: symbol, Limit: p.Limit}
		if from != nil {
			tq.From = *from
		}
		if to != nil {
			tq.To = *to
		}
		if p.Decoded != nil {
			tq.After = &analytics.TickCursor{
				Ts:      p.Decoded.CreatedAt,
				TradeID: uint64(p.Decoded.ID),
			}
		}
		ticks, err := d.Ticks.Query(r.Context(), tq)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"tick history store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
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
		env := NewListEnvelope(docs, p,
			PageCursors(ticks, func(t analytics.Tick) (time.Time, int64) {
				return t.Ts, int64(t.TradeID)
			}), int64(len(ticks)))
		WriteJSON(w, http.StatusOK, env)
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
// over open_time (ascending stream).
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
		bars, err := d.Klines.Query(r.Context(), kq)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"kline history store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
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
		WriteJSON(w, http.StatusOK, env)
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
