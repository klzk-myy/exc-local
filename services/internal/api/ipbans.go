// Task 5.3.34 — admin review/override surface for progressive IP bans.
//
//	GET    /api/v1/admin/ip-bans          — active ban list
//	GET    /api/v1/admin/ip-bans/audit    — recent ban/override audit trail
//	GET    /api/v1/admin/ip-bans/{ip}     — single ban record
//	PUT    /api/v1/admin/ip-bans/{ip}     — manual ban {duration_s, reason}
//	DELETE /api/v1/admin/ip-bans/{ip}     — unban; ?pardon=1 also resets strikes
//	PUT    /api/v1/admin/ip-allowlist/{ip}    — ban-machinery exemption
//	DELETE /api/v1/admin/ip-allowlist/{ip}    — remove exemption
//
// Every mutation audits into ip_ban_audit with the admin actor identity.
// Access requires an authenticated claim carrying the "admin" scope —
// Phase-05 Task 5.3.12's RBAC refinement tightens this to the Support
// Agent/Risk Manager role set when that lands.
package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/ratelimit"
)

// BanStore is the admin surface seam over the ratelimit backend.
type BanStore interface {
	ListBans(ctx context.Context) ([]ratelimit.Ban, error)
	BanInfo(ctx context.Context, ip string) (*ratelimit.Ban, error)
	ListAudit(ctx context.Context, n int64) ([]string, error)
}

// AdminIPBans returns handlers for the ban-review surface. admin is the
// override executor; store is the read side (same backend normally).
func AdminIPBans(store BanStore, admin *ratelimit.BanAdmin) (
	list, get, put, del, audit http.HandlerFunc,
) {
	list = func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		bans, err := store.ListBans(r.Context())
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"ban store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"bans": bans})
	}
	get = func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		ip := r.PathValue("ip")
		ban, err := store.BanInfo(r.Context(), ip)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"ban store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if ban == nil {
			WriteError(w, "NOT_FOUND",
				"no active ban for "+ip, gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, ban)
	}
	put = func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		ip := r.PathValue("ip")
		var body struct {
			DurationS int64  `json:"duration_s"`
			Reason    string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST",
				"malformed body", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ban, err := admin.BanIP(r.Context(), ip, body.Reason,
			claims.Subject, time.Duration(body.DurationS)*time.Second)
		if err != nil {
			WriteError(w, "INVALID_REQUEST",
				err.Error(), gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, ban)
	}
	del = func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		ip := r.PathValue("ip")
		if r.URL.Query().Get("pardon") == "1" {
			if err := admin.PardonIP(r.Context(), ip, claims.Subject); err != nil {
				WriteError(w, "SERVICE_DEGRADED",
					"pardon failed", gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"ip": ip, "pardoned": true})
			return
		}
		removed, err := admin.UnbanIP(r.Context(), ip, claims.Subject)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"unban failed", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !removed {
			WriteError(w, "NOT_FOUND",
				"no active ban for "+ip, gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ip": ip, "unbanned": true})
	}
	audit = func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		n := int64(100)
		if q := r.URL.Query().Get("limit"); q != "" {
			if v, err := strconv.ParseInt(q, 10, 64); err == nil && v > 0 && v <= 1000 {
				n = v
			}
		}
		recs, err := store.ListAudit(r.Context(), n)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"audit store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		events := make([]json.RawMessage, 0, len(recs))
		for _, s := range recs {
			events = append(events, json.RawMessage(s))
		}
		WriteJSON(w, http.StatusOK, map[string]any{"audit": events})
	}
	return list, get, put, del, audit
}

// AdminIPAllowlist returns handlers for the ban-exemption set.
func AdminIPAllowlist(admin *ratelimit.BanAdmin) (put, del http.HandlerFunc) {
	put = func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		ip := r.PathValue("ip")
		if err := admin.AllowlistIP(r.Context(), ip, claims.Subject); err != nil {
			WriteError(w, "INVALID_REQUEST",
				err.Error(), gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ip": ip, "allowlisted": true})
	}
	del = func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		ip := r.PathValue("ip")
		if err := admin.UnallowlistIP(r.Context(), ip, claims.Subject); err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"allowlist update failed", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ip": ip, "allowlisted": false})
	}
	return put, del
}

// requireAdmin enforces authenticated-admin access; nil claims ⇒ the
// response was already written (401/403, fail-closed).
func requireAdmin(w http.ResponseWriter, r *http.Request) *auth.Claims {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil {
		WriteError(w, "UNAUTHORIZED",
			"authentication required", gateway.RequestIDFrom(r.Context()), nil)
		return nil
	}
	if !claims.HasScope("admin") {
		WriteError(w, "FORBIDDEN",
			"admin scope required", gateway.RequestIDFrom(r.Context()), nil)
		return nil
	}
	return claims
}

// ValidIP is a shared sanity check for IP path params.
func ValidIP(s string) bool { return net.ParseIP(s) != nil }
