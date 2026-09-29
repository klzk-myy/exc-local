// Phase-13 Tasks 13.3.1 / 13.3.9 — five-tier circuit-breaker admin
// surface (spec §2.6).
//
//	POST /api/v1/admin/circuit-breaker/{symbol}        — manual trip
//	POST /api/v1/admin/circuit-breaker/{symbol}/reset  — manual close
//
// Trip is single-approver (Risk Manager+ via the route registry, enforced
// by admin.Middleware before the handler runs). Reset is four-eyes: the
// handler submits a DualControlService request (operation
// "circuit-breaker-reset", 15-minute approval window); a SECOND distinct
// Risk Manager approves through POST /api/v1/admin/dual-control/{id}/approve
// and the registered executor applies the close inside the approval
// transaction boundary.
//
// Scope selection: the path symbol drives INSTRUMENT:{symbol} by default.
// A request body {scope, target_id} may target the other tiers —
// ACCOUNT:{account_id} and MARKET_WIDE are manual-recovery scopes whose
// only administrative surface is this endpoint; target_id defaults to
// the path symbol and is ignored for MARKET_WIDE.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/gateway"
	"exchange/internal/middleware"

	excerrors "exchange/pkg/errors"
)

// circuitBreakerTripper is the narrow handler seam —
// *risk.CircuitBreakerService satisfies it; tests substitute a fake.
type circuitBreakerTripper interface {
	ManualTrip(ctx context.Context, scope, id, reason string, actorID int64) error
}

// breakerResetter is the executor seam — *risk.CircuitBreakerService.
type breakerResetter interface {
	ManualReset(ctx context.Context, scope, id string, actorID int64) error
}

// dualSubmitter is the four-eyes queue seam — *admin.DualControlService.
type dualSubmitter interface {
	Submit(ctx context.Context, in admin.SubmitInput) (*admin.DualControlRequest, error)
}

// CircuitBreakerDeps wires the breaker admin surface.
type CircuitBreakerDeps struct {
	Breakers   circuitBreakerTripper
	Dual       dualSubmitter
	TrustProxy bool
}

// circuitBreakerBody is the shared payload; scope/target_id default to
// INSTRUMENT:{path symbol}.
type circuitBreakerBody struct {
	Scope    string `json:"scope"`
	TargetID string `json:"target_id"`
	Reason   string `json:"reason"`
}

func (b *circuitBreakerBody) normalize(pathSymbol string) (scope, target string) {
	scope = strings.ToUpper(strings.TrimSpace(b.Scope))
	target = strings.TrimSpace(b.TargetID)
	if scope == "" {
		scope = "INSTRUMENT"
	}
	if target == "" {
		target = pathSymbol
	}
	return scope, target
}

// adminUserID resolves the acting admin from the RBAC identity the
// middleware attached; missing identity fails closed UNAUTHORIZED.
func adminUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id := admin.IdentityFrom(r.Context())
	if id == nil || id.UserID <= 0 {
		WriteError(w, "UNAUTHORIZED", "admin identity required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return id.UserID, true
}

// AdminCircuitBreakerTrip — POST /api/v1/admin/circuit-breaker/{symbol}.
func AdminCircuitBreakerTrip(deps CircuitBreakerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol path parameter required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body circuitBreakerBody
		// Empty body is legal (spec: trip {symbol}); decode is optional.
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				WriteError(w, "INVALID_REQUEST", "malformed JSON body",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		actor, ok := adminUserID(w, r)
		if !ok {
			return
		}
		scope, target := body.normalize(symbol)
		if err := deps.Breakers.ManualTrip(r.Context(), scope, target,
			body.Reason, actor); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"scope": scope, "target_id": target, "state": "OPEN",
		})
	}
}

// AdminCircuitBreakerReset — POST /api/v1/admin/circuit-breaker/{symbol}/reset.
// Dual control per spec §8.2: submits a PENDING four-eyes request; the
// breaker closes only when a distinct Risk Manager approves within the
// 15-minute window (executor registered at boot runs the mutation).
func AdminCircuitBreakerReset(deps CircuitBreakerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		symbol := r.PathValue("symbol")
		if symbol == "" {
			WriteError(w, "INVALID_REQUEST", "symbol path parameter required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body circuitBreakerBody
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				WriteError(w, "INVALID_REQUEST", "malformed JSON body",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		actor, ok := adminUserID(w, r)
		if !ok {
			return
		}
		scope, target := body.normalize(symbol)
		if scope != "MARKET_WIDE" && strings.TrimSpace(target) == "" {
			WriteError(w, "INVALID_REQUEST", "target_id required for scope "+scope,
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req, err := deps.Dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpCircuitBreakerReset,
			TargetType:   "circuit_breaker",
			TargetID:     scope + ":" + target,
			Payload:      map[string]any{"scope": scope, "target_id": target},
			RequiredRole: admin.RoleRiskManager,
			RequestedBy:  actor,
			Reason:       body.Reason,
			ClientIP:     middleware.ClientIP(r, deps.TrustProxy),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"dual_control": "required", "request": req,
		})
	}
}

// RegisterBreakerResetExecutor attaches the four-eyes executor for
// circuit-breaker resets: on the second approver's approval it decodes
// {scope, target_id} and applies the close. A failed close aborts the
// approval transaction — the request stays PENDING for retry rather than
// recording a reset that never happened (fail closed).
func RegisterBreakerResetExecutor(dual *admin.DualControlService, svc breakerResetter) {
	dual.RegisterExecutor(admin.OpCircuitBreakerReset,
		func(ctx context.Context, _ pgx.Tx, req *admin.DualControlRequest) error {
			var p struct {
				Scope    string `json:"scope"`
				TargetID string `json:"target_id"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.New("INVALID_REQUEST",
					"circuit-breaker reset payload not decodable")
			}
			var actor int64
			if req.ApprovedBy != nil {
				actor = *req.ApprovedBy
			}
			return svc.ManualReset(ctx, p.Scope, p.TargetID, actor)
		})
}
