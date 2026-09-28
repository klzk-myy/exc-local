// Task 5.3.5 — market data REST endpoints (spec §10.3), plus the
// Task 5.3.35 structured-filter surface on /api/v1/instruments.
//
//	GET /api/v1/book/{symbol}?depth=20        — persisted L2 snapshot (100ms cache)
//	GET /api/v1/trades/{symbol}?limit=100     — public recent-trades tape (1s cache)
//	GET /api/v1/ticker/{symbol}               — rolling 24h aggregate (1s cache)
//	GET /api/v1/klines/{symbol}?interval=1m   — pre-materialized OHLCV bars (1s cache)
//	GET /api/v1/instruments                   — reference data + filters (1min cache)
//
// Provenance (fail-closed, spec §2.7): instruments/trades/book read
// PostgreSQL; klines read only the fx_klines materialized aggregate
// (spec §10.3 remediation #38 forbids in-request candle computation).
// Nothing is fabricated: unknown symbols → 404 NOT_FOUND, store failures
// → 503 SERVICE_DEGRADED, quiet markets → honest empty/zero payloads.
package api

import (
	"net/http"
	"strconv"
	"time"

	"exchange/internal/gateway"
	"exchange/internal/marketapi"
)

// MarketDeps bundles the seams the market-data handlers need. In the
// gateway binary both interfaces resolve to the same *marketapi.PgStore;
// tests inject fakes.
type MarketDeps struct {
	Store marketapi.Store
	Book  marketapi.BookSource
	Cache *marketapi.Cache
	Now   func() time.Time
}

func (d *MarketDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *MarketDeps) cache() *marketapi.Cache {
	if d.Cache != nil {
		return d.Cache
	}
	return marketapi.NewCache(nil)
}

// cacheTTLs implement the spec §10.3 cache contract.
const (
	bookCacheTTL        = 100 * time.Millisecond
	marketCacheTTL      = 1 * time.Second
	instrumentsCacheTTL = 1 * time.Minute
)

// cached wraps a store fetch in the shared TTL cache.
func cached[T any](c *marketapi.Cache, key string, ttl time.Duration,
	fn func() (T, error)) (T, error) {
	var zero T
	v, err := c.Do(key, ttl, func() (any, error) { return fn() })
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, errCacheType(key)
	}
	return t, nil
}

type cacheTypeErr string

func (e cacheTypeErr) Error() string { return "cache type mismatch on " + string(e) }
func errCacheType(key string) error  { return cacheTypeErr(key) }

// intParam parses an optional positive integer query parameter.
// Returns (value, ok); ok=false means the caller already owes a 400.
func intParam(raw string, def, min, max int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < int64(min) || n > int64(max) {
		return 0, false
	}
	return int(n), true
}

// msParam parses an optional epoch-millis query parameter into time.Time.
func msParam(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(n), true
}

// ---------------------------------------------------------------------------
// GET /api/v1/book/{symbol}?depth=20 — L2 snapshot, 100ms cache
// ---------------------------------------------------------------------------

// MarketBook serves the aggregated L2 snapshot: top-N bid/ask levels plus
// the persisted book sequence. depth defaults to 20 (spec §10.1 top-20),
// bounded to [1,100] covering the Task 6.3.15 5/10/20 configurable set.
func MarketBook(d *MarketDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.PathValue("symbol")
		depth, ok := intParam(r.URL.Query().Get("depth"), 20, 1, 100)
		if symbol == "" || !ok {
			WriteError(w, "INVALID_REQUEST",
				"symbol required; depth must be an integer in [1,100]",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		snap, err := cached(d.cache(), "book:"+symbol+":"+strconv.Itoa(depth),
			bookCacheTTL, func() (*marketapi.BookSnapshot, error) {
				return d.Book.Snapshot(r.Context(), symbol, depth)
			})
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"market data store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if snap == nil {
			WriteError(w, "NOT_FOUND",
				"unknown symbol", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, snap)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/trades/{symbol}?limit=100 — recent trades, 1s cache
// ---------------------------------------------------------------------------

// MarketTrades serves the public trade tape (newest first). limit defaults
// to 100 and is capped at 1000.
func MarketTrades(d *MarketDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.PathValue("symbol")
		limit, ok := intParam(r.URL.Query().Get("limit"), 100, 1, 1000)
		if symbol == "" || !ok {
			WriteError(w, "INVALID_REQUEST",
				"symbol required; limit must be an integer in [1,1000]",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		type payload struct {
			trades []marketapi.Trade
			known  bool
		}
		p, err := cached(d.cache(), "trades:"+symbol+":"+strconv.Itoa(limit),
			marketCacheTTL, func() (payload, error) {
				inst, err := d.Store.InstrumentBySymbol(r.Context(), symbol)
				if err != nil {
					return payload{}, err
				}
				if inst == nil {
					return payload{}, nil
				}
				trades, err := d.Store.RecentTrades(r.Context(), symbol, limit)
				return payload{trades: trades, known: true}, err
			})
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"market data store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !p.known {
			WriteError(w, "NOT_FOUND",
				"unknown symbol", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"symbol": symbol,
			"data":   p.trades,
			"count":  len(p.trades),
			"limit":  limit,
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/ticker/{symbol} — 24h ticker, 1s cache
// ---------------------------------------------------------------------------

// MarketTicker serves the rolling 24h OHLCV aggregate. A quiet symbol
// reports zero volume and null price fields — never synthesized quotes.
func MarketTicker(d *MarketDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST",
				"symbol required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		now := d.now()
		tk, err := cached(d.cache(), "ticker:"+symbol, marketCacheTTL,
			func() (*marketapi.Ticker, error) {
				return d.Store.Ticker24h(r.Context(), symbol, now)
			})
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"market data store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if tk == nil {
			WriteError(w, "NOT_FOUND",
				"unknown symbol", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, tk)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/klines/{symbol}?interval=1m&limit=500 — OHLCV, 1s cache
// ---------------------------------------------------------------------------

// MarketKlines serves pre-materialized candles from fx_klines only
// (spec §10.3 remediation #38). interval accepts the canonical 13-timeframe
// set; limit defaults to 500, max 1500 (Phase-06 Task 6.3.8 ceiling).
// Optional from/to are epoch millis bounding open_time; next_cursor (when
// present) is the earliest returned open_time — page backwards with
// ?to=<next_cursor>.
func MarketKlines(d *MarketDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.PathValue("symbol")
		q := r.URL.Query()
		interval := q.Get("interval")
		if interval == "" {
			interval = "1m"
		}
		limit, ok := intParam(q.Get("limit"), 500, 1, 1500)
		from, okFrom := msParam(q.Get("from"))
		to, okTo := msParam(q.Get("to"))
		if symbol == "" || !ok || !okFrom || !okTo || !marketapi.KlineIntervals[interval] {
			WriteError(w, "INVALID_REQUEST",
				"symbol required; interval must be one of the canonical 13 "+
					"timeframes; limit in [1,1500]; from/to epoch millis",
				gateway.RequestIDFrom(r.Context()), map[string]any{
					"intervals": klineIntervalList(),
				})
			return
		}
		type payload struct {
			klines []marketapi.Kline
			known  bool
		}
		key := "klines:" + symbol + ":" + interval + ":" +
			strconv.FormatInt(from.UnixMilli(), 10) + ":" +
			strconv.FormatInt(to.UnixMilli(), 10) + ":" + strconv.Itoa(limit)
		p, err := cached(d.cache(), key, marketCacheTTL, func() (payload, error) {
			inst, err := d.Store.InstrumentBySymbol(r.Context(), symbol)
			if err != nil {
				return payload{}, err
			}
			if inst == nil {
				return payload{}, nil
			}
			ks, err := d.Store.Klines(r.Context(), symbol, interval, from, to, limit)
			return payload{klines: ks, known: true}, err
		})
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"market data store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !p.known {
			WriteError(w, "NOT_FOUND",
				"unknown symbol", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		resp := map[string]any{
			"symbol":   symbol,
			"interval": interval,
			"data":     p.klines,
			"count":    len(p.klines),
			"limit":    limit,
		}
		if len(p.klines) == limit && len(p.klines) > 0 {
			// A full page suggests older bars may exist.
			resp["next_cursor"] = strconv.FormatInt(p.klines[0].OpenTimeMs, 10)
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}

func klineIntervalList() []string {
	out := make([]string, 0, len(marketapi.KlineIntervals))
	for k := range marketapi.KlineIntervals {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// GET /api/v1/instruments — public reference data + filters, 1min cache
// ---------------------------------------------------------------------------

// instrumentDoc is the per-row projection of GET /api/v1/instruments —
// the Task 5.3.5 field set plus the Task 5.3.35 structured filters.
type instrumentDoc struct {
	marketapi.Instrument
	Filters      []marketapi.Filter     `json:"filters"`
	TradingHours marketapi.TradingHours `json:"trading_hours"`
	Settlement   string                 `json:"settlement"`
	UpdatedAtMs  int64                  `json:"updated_at_ms"`
}

// Instruments serves the full instrument universe: every lifecycle state
// is published (clients filter on status) — delisted/halted instruments
// stay visible per the §7.1 read contract.
func Instruments(d *MarketDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := cached(d.cache(), "instruments:all", instrumentsCacheTTL,
			func() ([]marketapi.Instrument, error) {
				return d.Store.ListInstruments(r.Context())
			})
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"instrument store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		docs := make([]instrumentDoc, 0, len(list))
		for _, inst := range list {
			docs = append(docs, instrumentDoc{
				Instrument:   inst,
				Filters:      inst.Filters(),
				TradingHours: marketapi.VenueTradingHours(),
				Settlement:   inst.SettlementLabel(),
				UpdatedAtMs:  inst.UpdatedAt.UnixMilli(),
			})
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"data":           docs,
			"count":          len(docs),
			"server_time_ms": d.now().UnixMilli(),
		})
	}
}

// Compile-time interface assertions for the PgStore wiring.
var (
	_ marketapi.Store             = (*marketapi.PgStore)(nil)
	_ marketapi.BookSource        = (*marketapi.PgStore)(nil)
	_ marketapi.AnnouncementStore = (*marketapi.PgStore)(nil)
	_ marketapi.MaintenanceStore  = (*marketapi.PgStore)(nil)
)
