// Phase-04 Task 4.3.2 — WAL archive status surface (registry route
// GET /api/v1/admin/archive/status):
//
//	GET /api/v1/admin/archive/status?shard=0
//
// Read model over recovery.ArchiveService.Status: loads
// s3://{wal-bucket}/{shard}/index.json and HEADs each indexed segment —
// an index/object mismatch is an integrity defect the response surfaces
// as present=false rows (never synthesized away). Read-Only Auditor role
// per the registry row. Fail-closed: missing shard param →
// INVALID_REQUEST, unconfigured archive store → SERVICE_DEGRADED.
package api

import (
	"net/http"
	"strconv"

	"exchange/internal/gateway"
	"exchange/internal/recovery"
)

// ArchiveStatusDeps wires the archive read seam; Svc nil → 503.
type ArchiveStatusDeps struct {
	Svc *recovery.ArchiveService
}

// AdminArchiveStatus serves the per-shard WAL archive status report.
func AdminArchiveStatus(d *ArchiveStatusDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		reqID := gateway.RequestIDFrom(r.Context())
		raw := r.URL.Query().Get("shard")
		shard, err := strconv.ParseUint(raw, 10, 16)
		if raw == "" || err != nil {
			WriteError(w, "INVALID_REQUEST",
				"shard query parameter required (uint16)", reqID, nil)
			return
		}
		if d == nil || d.Svc == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"WAL archive store not configured (EXC_S3_WAL_BUCKET)", reqID, nil)
			return
		}
		rows, err := d.Svc.Status(r.Context(), uint16(shard))
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"archive index read failed", reqID, nil)
			return
		}
		if rows == nil {
			rows = []recovery.StatusRow{}
		}
		var total int64
		missing := 0
		for _, row := range rows {
			total += row.SizeBytes
			if !row.Present {
				missing++
			}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"shard":            shard,
			"segments":         rows,
			"segment_count":    len(rows),
			"total_bytes":      total,
			"missing_segments": missing,
		})
	}
}
