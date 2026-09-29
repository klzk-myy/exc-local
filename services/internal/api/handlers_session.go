// handlers_session.go — Phase-15 Task 15.3.7 public session surface.
//
//	GET /api/v1/session/status — public (TierPublic, no auth)
//
// Returns the §6.7 weekly session view: schedule-implied state, the next
// boundary, and per-shard coverage of session:state:{shard}. The
// schedule clock is authoritative for the headline `state`; per-shard
// divergence surfaces via consistent=false plus the shards array —
// never a fabricated all-green (spec §2.7 pessimism).
package api

import (
	"net/http"

	"exchange/internal/admin"
	"exchange/internal/gateway"
)

// SessionStatus serves GET /api/v1/session/status. A nil service fails
// closed SERVICE_DEGRADED — the route row is Live so a missing wire-up
// must not masquerade as a working endpoint.
func SessionStatus(svc *admin.SessionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"session lifecycle service not wired",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		st, err := svc.Status(r.Context())
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"session state read failed",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}
}
