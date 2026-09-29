// Phase-11 Task 11.3.5 — 24-hour market statistics REST surface.
//
//	GET /api/v1/stats/24h          — one Stats24h row per listed instrument
//	GET /api/v1/stats/24h/{symbol} — the single-symbol variant
//
// Semantics (task AC): the window is the ROLLING [now−24h, now) slice —
// identical to /api/v1/ticker/{symbol} and the marketdata stats@all
// producer. Every listed instrument appears in the all-symbol payload
// (quiet markets report trade_count=0, nil price fields); an unknown
// single symbol is 404. Nothing is fabricated — store failures degrade
// to SERVICE_DEGRADED and the 1s §10.3 read cache applies.
package api

import (
	"net/http"

	"exchange/internal/gateway"
	"exchange/internal/marketapi"
)

// MarketStats24hAll serves GET /api/v1/stats/24h — the venue-wide
// statistics payload: { window, count, data[], server_time_ms }.
func MarketStats24hAll(d *MarketDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := d.now()
		stats, err := cached(d.cache(), "stats24h:all", marketCacheTTL,
			func() ([]marketapi.Stats24h, error) {
				return d.Store.Stats24hAll(r.Context(), now)
			})
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"market statistics store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if stats == nil {
			stats = []marketapi.Stats24h{} // empty dataset, not null
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"window":         "24h",
			"count":          len(stats),
			"data":           stats,
			"server_time_ms": now.UnixMilli(),
		})
	}
}

// MarketStats24h serves GET /api/v1/stats/24h/{symbol} — one instrument's
// rolling 24h statistics row, or 404 NOT_FOUND for an unknown symbol.
func MarketStats24h(d *MarketDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST",
				"symbol required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		now := d.now()
		st, err := cached(d.cache(), "stats24h:"+symbol, marketCacheTTL,
			func() (*marketapi.Stats24h, error) {
				return d.Store.Stats24h(r.Context(), symbol, now)
			})
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"market statistics store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if st == nil {
			WriteError(w, "NOT_FOUND",
				"unknown symbol", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}
}
