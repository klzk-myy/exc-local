// Phase-07 Task 7.3.3 — admin audit log query + one-click integrity
// proof (spec §5.9, §14.11.3; route rows pinned by Task 5.3.7):
//
//	GET /api/v1/admin/audit-log      filtered keyset-paged query
//	GET /api/v1/admin/audit          alias of audit-log (§14.11.3 surface)
//	GET /api/v1/admin/audit/verify   ?date=YYYY-MM-DD → verify report
//
// All three are Read-Only Auditor+ (auditor or Super Admin) behind the
// AdminRoleResolver seam — Phase-07 Task 7.3.1 owns the role store; nil
// fails closed UNAUTHORIZED_ROLE.
package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/gateway"
)

// auditReadRoles may query the admin audit trail (Task 7.3.3 item 3:
// "Read-Only Auditor+").
var auditReadRoles = map[string]bool{
	"Read-Only Auditor": true,
	"Super Admin":       true,
}

// requireAuditRole gates an admin read endpoint.
func requireAuditRole(w http.ResponseWriter, r *http.Request, resolver AdminRoleResolver) (int64, bool) {
	adminID, ok := adminActor(w, r)
	if !ok {
		return 0, false
	}
	if resolver == nil {
		WriteError(w, "UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC stub boundary)",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	role, err := resolver(r.Context(), adminID)
	if err != nil {
		WriteError(w, "INTERNAL_ERROR", "role lookup failed",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	if !auditReadRoles[role] {
		WriteError(w, "UNAUTHORIZED_ROLE",
			"audit access requires Read-Only Auditor or Super Admin",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return adminID, true
}

// AdminAuditLog serves GET /api/v1/admin/audit-log and
// GET /api/v1/admin/audit — filters: admin_user_id, action,
// action_prefix, target_type, target_id, from, to + cursor/limit.
func AdminAuditLog(pool *pgxpool.Pool, resolver AdminRoleResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAuditRole(w, r, resolver); !ok {
			return
		}
		p, err := ParseListParams(r, ListSpecFor("/api/v1/admin/audit-log"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		f := admin.AuditFilter{
			Action:       q.Get("action"),
			ActionPrefix: q.Get("action_prefix"),
			TargetType:   q.Get("target_type"),
		}
		if raw := q.Get("admin_user_id"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "admin_user_id must be an integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.AdminUserID = &id
		}
		if raw := q.Get("target_id"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "target_id must be an integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.TargetID = &id
		}
		if f.From, err = parseTimeQuery(q.Get("from")); err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if f.To, err = parseTimeQuery(q.Get("to")); err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}

		page, err := admin.QueryAudit(r.Context(), pool, f, keysetAfter(p), p.Limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		env := NewListEnvelope(page.Rows, p,
			PageCursors(page.Rows, func(row admin.AuditRow) (time.Time, int64) {
				return row.CreatedAt, row.ID
			}), page.Total)
		WriteJSON(w, http.StatusOK, env)
	}
}

// AdminAuditVerify serves GET /api/v1/admin/audit/verify?date=YYYY-MM-DD
// — the Task 7.3.3 one-click integrity proof: replays the hash chain
// genesis→end-of-date plus the stored daily Merkle root, and returns the
// report (rows checked, violating day+sequence, or a clean bill).
func AdminAuditVerify(pool *pgxpool.Pool, resolver AdminRoleResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAuditRole(w, r, resolver); !ok {
			return
		}
		raw := r.URL.Query().Get("date")
		if raw == "" {
			WriteError(w, "INVALID_REQUEST", "date=YYYY-MM-DD is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		date, err := time.Parse("2006-01-02", raw)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "date must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		// Bounded run — a full-chain replay is heavy; the endpoint fails
		// closed with 504-style INTERNAL_ERROR on timeout rather than
		// hanging the connection.
		ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
		defer cancel()
		rep, err := admin.VerifyDay(ctx, pool, date)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		// Integrity failures are still HTTP 200 — the report IS the
		// answer; ok=false flags the finding (operators page on
		// AUDIT_HASH_CORRUPTION separately).
		WriteJSON(w, http.StatusOK, rep)
	}
}
