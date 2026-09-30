// Phase-20 Task 20.3.13 — daily account snapshot history (spec §16.7,
// §24 #362):
//
//	GET /api/v1/account/snapshots?date=&limit=&cursor=
//
// Reads the retained balance_snapshots chain rows (migration 093) via
// analytics.SnapshotStore — PostgreSQL is the book of record here (the
// chain IS the data, not a projection), so a store outage answers
// SERVICE_DEGRADED and a gap simply isn't in the page. ?date=YYYY-MM-DD
// narrows to exactly one snapshot day; absent it the endpoint pages the
// retained history newest-first over the (snapshot_date, id) §8.8 keyset.
//
// Every row carries its hash + prev_hash so a client (or the
// VerifyAccountChain auditor path) can re-verify the per-account chain
// offline; `positions` passes the stored canonical positions_json
// through verbatim.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/gateway"
)

// snapshotsListSpec — same ownership note as incomeListSpec: local
// declaration pending a pagination.go table edit by that file's owner.
var snapshotsListSpec = &ListSpec{
	Path: "/api/v1/account/snapshots", Default: 100, Max: 500,
	Sortable:   []string{"snapshot_date", "id"},
	Filterable: []string{"date"},
}

// SnapshotHistorySource is the snapshot read seam —
// *analytics.SnapshotStore satisfies it; tests inject fakes.
type SnapshotHistorySource interface {
	History(ctx context.Context, accountID int64, date *time.Time,
		after *analytics.SnapshotCursor, limit int) ([]analytics.SnapshotRow, int64, error)
}

// snapshotDoc is the wire projection of one balance_snapshots row.
// Positions is the stored canonical JSON array passed through as raw
// JSON (re-canonicalized only by the verifier, never re-encoded here).
type snapshotDoc struct {
	ID           int64           `json:"id"`
	SnapshotDate string          `json:"snapshot_date"`
	Currency     string          `json:"currency"`
	Available    string          `json:"available"`
	Locked       string          `json:"locked"`
	Total        string          `json:"total"`
	Positions    json.RawMessage `json:"positions"`
	Hash         string          `json:"hash"`
	PrevHash     string          `json:"prev_hash"`
	CreatedAt    string          `json:"created_at"`
}

// AccountSnapshots serves the authenticated account's retained snapshot
// history (?date=YYYY-MM-DD narrows to one day).
func AccountSnapshots(src SnapshotHistorySource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if src == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"snapshot store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if rejectForeignAccount(w, r, claims, r.URL.Query().Get("account_id")) {
			return
		}
		p, err := ParseListParams(r, snapshotsListSpec)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}

		var date *time.Time
		if raw := strings.TrimSpace(r.URL.Query().Get("date")); raw != "" {
			d, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
			if err != nil {
				WriteError(w, "INVALID_REQUEST",
					"date must be YYYY-MM-DD (UTC)",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			date = &d
		}
		var after *analytics.SnapshotCursor
		if p.Decoded != nil {
			after = &analytics.SnapshotCursor{
				SnapshotDate: p.Decoded.CreatedAt,
				ID:           p.Decoded.ID,
			}
		}

		rows, total, err := src.History(r.Context(), accountID, date, after, p.Limit)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"snapshot store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}

		docs := make([]snapshotDoc, 0, len(rows))
		for _, row := range rows {
			pos := json.RawMessage(row.PositionsJSON)
			if len(pos) == 0 {
				pos = json.RawMessage("[]")
			}
			docs = append(docs, snapshotDoc{
				ID:           row.ID,
				SnapshotDate: row.SnapshotDate.UTC().Format("2006-01-02"),
				Currency:     row.Currency,
				Available:    row.Available.StringFixed(8),
				Locked:       row.Locked.StringFixed(8),
				Total:        row.Available.Add(row.Locked).StringFixed(8),
				Positions:    pos,
				Hash:         row.Hash,
				PrevHash:     row.PrevHash,
				CreatedAt:    row.CreatedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		env := NewListEnvelope(docs, p,
			PageCursors(rows, func(row analytics.SnapshotRow) (time.Time, int64) {
				return row.SnapshotDate, row.ID
			}), total)
		WriteJSON(w, http.StatusOK, env)
	}
}
