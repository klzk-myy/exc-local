// Task 7.3.9 — Liquidity Provider admin REST surface.
//
//	GET    /api/v1/admin/liquidity-providers
//	POST   /api/v1/admin/liquidity-providers
//	GET    /api/v1/admin/liquidity-providers/{id}
//	PUT    /api/v1/admin/liquidity-providers          (lp_id in body — pinned route row)
//	PUT    /api/v1/admin/liquidity-providers/{id}     (REST-idiomatic variant)
//	GET    /api/v1/admin/liquidity-providers/{id}/scorecard?window=1h
//	GET    /api/v1/admin/liquidity-providers/{id}/alerts?open=1
//
// Guarantees (task AC):
//   - Role gate via the AdminRoleResolver-shaped seam on admin.LPService —
//     mutations require Risk Manager / Super Admin per the route registry
//     rows; a nil resolver fails closed UNAUTHORIZED_ROLE until the
//     Phase-07 binding store (admin/rbac.go, concurrent) lands.
//   - LP lifecycle ONBOARDING→ACTIVE→SUSPENDED is guarded (INVALID_
//     LIFECYCLE_TRANSITION on out-of-order moves).
//   - Scorecards never fabricate: a wired MetricsSource yields live
//     metrics (persisted + threshold-evaluated); otherwise the last
//     persisted snapshot is served with stale=true.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"

	excerrors "exchange/pkg/errors"
)

// adminActorFrom resolves the caller's admin identity from Bearer claims,
// mirroring ManualLiquidationHandler.
func adminActorFrom(r *http.Request, trustProxy bool) (admin.AdminActor, error) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil {
		return admin.AdminActor{}, excerrors.New("UNAUTHORIZED", "authentication required")
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 {
		return admin.AdminActor{}, excerrors.New("UNAUTHORIZED", "admin identity unresolvable")
	}
	return admin.AdminActor{UserID: id, ClientIP: middleware.ClientIP(r, trustProxy)}, nil
}

// writeSvcErr maps a coded service error onto the §8.7 envelope.
func writeSvcErr(w http.ResponseWriter, r *http.Request, err error) {
	var e *excerrors.Error
	code, msg := "INTERNAL_ERROR", "internal error"
	if errors.As(err, &e) {
		code, msg = e.Code, e.Message
	}
	WriteError(w, code, msg, gateway.RequestIDFrom(r.Context()), nil)
}

// lpPathID reads the {id} path segment as a positive int64 (shared
// helper parsePathID lives in handlers_announce.go).
func lpPathID(r *http.Request) (int64, error) {
	v, ok := parsePathID(r.PathValue("id"))
	if !ok {
		return 0, excerrors.New("INVALID_REQUEST", "id must be a positive integer")
	}
	return v, nil
}

// AdminLPList serves GET /api/v1/admin/liquidity-providers?status=.
func AdminLPList(svc *admin.LPService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		lps, err := svc.List(r.Context(), actor, r.URL.Query().Get("status"))
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"liquidity_providers": lps})
	}
}

// AdminLPCreate serves POST /api/v1/admin/liquidity-providers.
func AdminLPCreate(svc *admin.LPService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var req admin.LPCreate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		lp, err := svc.Create(r.Context(), actor, req)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, lp)
	}
}

// AdminLPGet serves GET /api/v1/admin/liquidity-providers/{id}.
func AdminLPGet(svc *admin.LPService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		lpID, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		lp, err := svc.Get(r.Context(), actor, lpID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, lp)
	}
}

// AdminLPUpdate serves PUT /api/v1/admin/liquidity-providers (lp_id in
// body — the pinned registry row) and PUT …/{id} (path id wins).
func AdminLPUpdate(svc *admin.LPService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var req admin.LPUpdate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if seg := r.PathValue("id"); seg != "" {
			lpID, err := lpPathID(r)
			if err != nil {
				writeSvcErr(w, r, err)
				return
			}
			req.LPID = lpID
		}
		lp, err := svc.Update(r.Context(), actor, req)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, lp)
	}
}

// AdminLPScorecard serves GET …/{id}/scorecard?window=1h. The response
// carries the scorecard plus any threshold alerts it tripped.
func AdminLPScorecard(svc *admin.LPService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		lpID, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		window := time.Duration(0)
		if raw := r.URL.Query().Get("window"); raw != "" {
			window, err = time.ParseDuration(raw)
			if err != nil || window <= 0 || window > 24*time.Hour {
				writeSvcErr(w, r, excerrors.New("INVALID_REQUEST",
					"window must be a positive duration ≤ 24h (e.g. 15m, 1h)"))
				return
			}
		}
		sc, fired, err := svc.Scorecard(r.Context(), actor, lpID, window)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"scorecard": sc, "alerts_fired": fired,
		})
	}
}

// AdminLPAlerts serves GET …/{id}/alerts?open=1.
func AdminLPAlerts(svc *admin.LPService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		lpID, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		openOnly := r.URL.Query().Get("open") == "1" ||
			r.URL.Query().Get("open") == "true"
		alerts, err := svc.Alerts(r.Context(), actor, lpID, openOnly)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"alerts": alerts})
	}
}
