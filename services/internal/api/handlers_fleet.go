// Task 9.3.30 — environment / fleet / promotion-gate admin surface
// (spec §19.16, §24 #350; migration 091).
//
// Routes (registered in Task 5.3.7, live here):
//
//	GET  /api/v1/admin/fleet/environments
//	GET  /api/v1/admin/fleet/hosts
//	POST /api/v1/admin/fleet/hosts/{id}/drain|cordon|decommission
//	GET  /api/v1/admin/fleet/topology?env=
//	GET  /api/v1/admin/releases
//	POST /api/v1/admin/releases
//	POST /api/v1/admin/releases/{id}/promote
//
// Every handler runs inside the session's environment context: the
// Task 7.3.11 RBAC middleware resolved X-Admin-Env into
// admin.Identity.Env (env-scoped routes also require an explicit env
// axis on the binding). Handlers fail closed UNAUTHORIZED when the
// identity is absent — a route that reached here without the middleware
// is a wiring bug, not a request to serve.
//
// Production server actions and promotions are dual-controlled: the
// request body carries approver_id for a distinct Super Admin principal
// (same synchronous four-eyes pattern as Task 5.3.30 manual liquidation);
// both principals land in admin_audit_log.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/fleet"
	"exchange/internal/gateway"
	"exchange/internal/middleware"

	excerrors "exchange/pkg/errors"
)

// fleetActor resolves the admin identity attached by the RBAC middleware
// into a fleet.Actor (env-scoped). approver_id is decoded by the caller
// from the request body.
func fleetActor(w http.ResponseWriter, r *http.Request, trustProxy bool) (fleet.Actor, bool) {
	id := admin.IdentityFrom(r.Context())
	if id == nil || id.UserID <= 0 {
		// Middleware never ran — fail closed rather than trusting claims.
		if c := auth.ClaimsFrom(r.Context()); c == nil {
			WriteError(w, "UNAUTHORIZED", "authentication required",
				gateway.RequestIDFrom(r.Context()), nil)
			return fleet.Actor{}, false
		}
		WriteError(w, "UNAUTHORIZED", "admin identity required",
			gateway.RequestIDFrom(r.Context()), nil)
		return fleet.Actor{}, false
	}
	return fleet.Actor{
		UserID:   id.UserID,
		Role:     id.Role,
		Env:      id.Env,
		ClientIP: middleware.ClientIP(r, trustProxy),
	}, true
}

// writeFleetError maps service errors onto the §8.7 envelope.
func writeFleetError(w http.ResponseWriter, r *http.Request, err error) {
	var e *excerrors.Error
	code, msg := "INTERNAL_ERROR", "internal error"
	if errors.As(err, &e) {
		code, msg = e.Code, e.Message
	}
	WriteError(w, code, msg, gateway.RequestIDFrom(r.Context()), nil)
}

// AdminFleetEnvironments serves GET /api/v1/admin/fleet/environments —
// the environment registry plus the session's current context.
func AdminFleetEnvironments(svc *fleet.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := fleetActor(w, r, trustProxy)
		if !ok {
			return
		}
		envs, err := svc.ListEnvironments(r.Context())
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"environments": envs,
			"session_env":  actor.Env,
		})
	}
}

// AdminFleetHosts serves GET /api/v1/admin/fleet/hosts — inventory scoped
// to the session env; ?state= and ?role= filter.
func AdminFleetHosts(svc *fleet.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := fleetActor(w, r, trustProxy)
		if !ok {
			return
		}
		q := r.URL.Query()
		hosts, err := svc.ListHosts(r.Context(), actor,
			q.Get("state"), q.Get("role"))
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"hosts": hosts, "env": actor.Env, "count": len(hosts)})
	}
}

// AdminFleetTopology serves GET /api/v1/admin/fleet/topology?env= — the
// shard→host / role / health cluster view for the session env.
func AdminFleetTopology(svc *fleet.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := fleetActor(w, r, trustProxy)
		if !ok {
			return
		}
		tv, err := svc.Topology(r.Context(), actor, r.URL.Query().Get("env"))
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, tv)
	}
}

// hostActionRequest is the drain/cordon/decommission body.
type hostActionRequest struct {
	Reason     string          `json:"reason"`
	ApproverID json.RawMessage `json:"approver_id"`
}

// AdminHostAction serves POST /api/v1/admin/fleet/hosts/{id}/{drain|cordon|decommission}.
func AdminHostAction(svc *fleet.Service, action string, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := fleetActor(w, r, trustProxy)
		if !ok {
			return
		}
		hostID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || hostID <= 0 {
			WriteError(w, "INVALID_REQUEST", "invalid host id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req hostActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if approver, present, perr := parseFlexID(req.ApproverID, "approver_id"); perr != nil {
			WriteError(w, "INVALID_REQUEST", perr.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		} else if present {
			actor.ApproverID = approver
		}
		sa, err := svc.RequestAction(r.Context(), actor, hostID, action, req.Reason)
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"server_action": sa})
	}
}

// AdminReleaseList serves GET /api/v1/admin/releases — env-scoped
// registry read; ?env= must equal the session env, ?status= filters.
func AdminReleaseList(svc *fleet.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := fleetActor(w, r, trustProxy)
		if !ok {
			return
		}
		q := r.URL.Query()
		rels, err := svc.ListReleases(r.Context(), actor,
			q.Get("env"), q.Get("status"))
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"releases": rels, "env": actor.Env, "count": len(rels)})
	}
}

// releaseCreateRequest is the POST /admin/releases body.
type releaseCreateRequest struct {
	Component    string          `json:"component"`
	Version      string          `json:"version"`
	ArtifactHash string          `json:"artifact_hash"`
	GateEvidence json.RawMessage `json:"gate_evidence"`
	Notes        string          `json:"notes"`
}

// AdminReleaseCreate serves POST /api/v1/admin/releases — registers an
// artifact (lands DEPLOYED in dev: dev auto-deploys, §19.16.3).
func AdminReleaseCreate(svc *fleet.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := fleetActor(w, r, trustProxy)
		if !ok {
			return
		}
		var req releaseCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rel, err := svc.RegisterRelease(r.Context(), actor, fleet.Release{
			Component: req.Component, Version: req.Version,
			ArtifactHash: req.ArtifactHash, GateEvidence: req.GateEvidence,
			Notes: req.Notes,
		})
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"release": rel})
	}
}

// promoteRequest is the POST /admin/releases/{id}/promote body.
type promoteRequest struct {
	ToEnv      string          `json:"to_env"`
	Reason     string          `json:"reason"`
	ApproverID json.RawMessage `json:"approver_id"`
}

// AdminReleasePromote serves POST /api/v1/admin/releases/{id}/promote —
// direction-enforced promotion (dev→staging→production) with the
// §19.16.3 gate set. Blocked attempts return FORBIDDEN carrying the
// evaluated gate list; the attempt row is persisted either way.
func AdminReleasePromote(svc *fleet.Service, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := fleetActor(w, r, trustProxy)
		if !ok {
			return
		}
		relID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || relID <= 0 {
			WriteError(w, "INVALID_REQUEST", "invalid release id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req promoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if req.ToEnv == "" {
			WriteError(w, "INVALID_REQUEST", "to_env is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if approver, present, perr := parseFlexID(req.ApproverID, "approver_id"); perr != nil {
			WriteError(w, "INVALID_REQUEST", perr.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		} else if present {
			actor.ApproverID = approver
		}
		p, gates, err := svc.Promote(r.Context(), actor, relID, req.ToEnv, req.Reason)
		if err != nil {
			var e *excerrors.Error
			code, msg := "INTERNAL_ERROR", "internal error"
			if errors.As(err, &e) {
				code, msg = e.Code, e.Message
			}
			WriteError(w, code, msg, gateway.RequestIDFrom(r.Context()),
				map[string]any{"gates": gates})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"promotion": p, "gates": gates})
	}
}
