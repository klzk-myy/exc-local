// Phase-17 Task 17.3.2 — GET /api/v1/market-data/l3-snapshot/{symbol}
//
// Asynchronous WAL-based L3 snapshot: the handler delegates to the
// dedicated L3SnapshotReader (internal/marketdata/l3_snapshot.go) which
// takes a WAL position marker at request time, finds the newest usable
// BOOK_SNAPSHOT for the instrument, replays ORDER_NEW/MODIFY/CANCEL/
// TRADE forward to the marker and serves the point-in-time order-level
// book — cursor-paginated, ≤500ms staleness, 100k-order ceiling.
//
// Error contract (all §23 registered):
//
//	INVALID_REQUEST        400  bad symbol/cursor/limit
//	NOT_FOUND              404  unknown symbol (no instrument resolution)
//	L3_SNAPSHOT_TOO_LARGE  413  >100,000 resting orders
//	SERVICE_DEGRADED       503  WAL unavailable OR the marker-pinned cut
//	                            missed the 500ms staleness budget
package api

import (
	"errors"
	"net/http"
	"strconv"

	"exchange/internal/gateway"
	"exchange/internal/marketdata"
)

// L3SnapshotDeps wires the endpoint to its reconstruction reader.
type L3SnapshotDeps struct {
	Reader *marketdata.L3SnapshotReader
}

// L3SnapshotMaxPage bounds one page (bigger pages just use the cursor).
const L3SnapshotMaxPage = 5000

// MarketDataL3Snapshot serves the paginated WAL-reconstructed book.
// Query params: cursor=<last order_id>, limit=<rows/page, default 1000,
// max 5000>.
func MarketDataL3Snapshot(d *L3SnapshotDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol path parameter required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var cursor uint64
		if raw := r.URL.Query().Get("cursor"); raw != "" {
			n, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST",
					"cursor must be a uint64 order_id",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			cursor = n
		}
		limit, ok := intParam(r.URL.Query().Get("limit"), 1000, 1, L3SnapshotMaxPage)
		if !ok {
			WriteError(w, "INVALID_REQUEST",
				"limit must be an integer in [1,5000]",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if d == nil || d.Reader == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"l3 snapshot reader not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}

		page, err := d.Reader.Snapshot(r.Context(), symbol, cursor, limit)
		if err != nil {
			switch {
			case errors.Is(err, marketdata.ErrL3UnknownSymbol):
				WriteError(w, "NOT_FOUND", "unknown symbol",
					gateway.RequestIDFrom(r.Context()), nil)
			case errors.Is(err, marketdata.ErrL3SnapshotTooLarge):
				WriteError(w, "L3_SNAPSHOT_TOO_LARGE",
					"book exceeds the 100,000 resting-order ceiling",
					gateway.RequestIDFrom(r.Context()),
					map[string]any{"retry_after": 5})
			case errors.Is(err, marketdata.ErrL3SnapshotStale),
				errors.Is(err, marketdata.ErrL3WALUnavailable):
				WriteError(w, "SERVICE_DEGRADED",
					"l3 snapshot could not be served inside the staleness budget",
					gateway.RequestIDFrom(r.Context()),
					map[string]any{"retry_after": 1})
			default:
				WriteError(w, "SERVICE_DEGRADED",
					"l3 snapshot reconstruction failed",
					gateway.RequestIDFrom(r.Context()), nil)
			}
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"symbol":      page.Snapshot.Symbol,
			"wal_seq":     page.Snapshot.WalSeq,
			"l3_seq":      page.Snapshot.Seq,
			"asof_ms":     page.Snapshot.AsOfMs,
			"count":       page.Snapshot.Count,
			"orders":      page.Snapshot.Orders,
			"next_cursor": page.NextCursor,
		})
	}
}
