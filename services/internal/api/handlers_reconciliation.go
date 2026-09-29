// Phase-13 Task 13.3.2 — reconciliation report surface (read-only).
//
// GET /api/v1/admin/reconciliation/latest  — newest run + its findings
// GET /api/v1/admin/reconciliation/runs    — recent run summaries
//
//	?limit=N   (1..200, default 50)
//	?run_id=N  — drill-down: one run + its findings
//
// RBAC: the route registry stamps adminAuth(RoleReadOnlyAuditor) — this
// surface reports financial-correctness evidence, it never mutates.
package api

import (
	"context"
	"net/http"
	"strconv"

	"exchange/internal/gateway"
	"exchange/internal/reconciliation"
)

// reconciliationReader is the narrow handler seam — *reconciliation.PgStore
// satisfies it; tests substitute a fake.
type reconciliationReader interface {
	LatestRun(ctx context.Context) (*reconciliation.Run, []reconciliation.FindingRow, error)
	ListRuns(ctx context.Context, limit int) ([]reconciliation.Run, error)
	RunFindings(ctx context.Context, runID int64) ([]reconciliation.FindingRow, error)
}

// AdminReconciliationLatest — GET /api/v1/admin/reconciliation/latest.
// 404-shaped response when the engine has never run (no run row yet) is
// avoided deliberately: an empty report is itself evidence — the
// handler returns run=null, findings=[].
func AdminReconciliationLatest(st reconciliationReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, findings, err := st.LatestRun(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"run":      run,
			"findings": findings,
		})
	}
}

// AdminReconciliationRuns — GET /api/v1/admin/reconciliation/runs.
// Default shape is the recent-run list; ?run_id= drills into one run's
// finding set.
func AdminReconciliationRuns(st reconciliationReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if raw := q.Get("run_id"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				WriteError(w, "INVALID_REQUEST", "run_id must be a positive integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			findings, err := st.RunFindings(r.Context(), id)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{
				"run_id":   id,
				"findings": findings,
			})
			return
		}
		limit := 50
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				WriteError(w, "INVALID_REQUEST", "limit must be a positive integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			limit = n
		}
		runs, err := st.ListRuns(r.Context(), limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
	}
}
