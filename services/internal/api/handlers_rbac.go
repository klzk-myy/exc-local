// Phase-07 Tasks 7.3.1/7.3.2/7.3.11/7.3.12 — admin RBAC HTTP surface:
// role catalog, binding grant/revoke (through the four-eyes queue),
// dual-control decide, recertification campaigns and break-glass grants
// + reviews.
//
// The RBACMiddleware (admin.Middleware via Router.SetWrapper) already
// verified identity, binding, role, env + scope before these handlers
// run; handlers re-derive the actor from claims (or the middleware's
// Identity) so they remain correct if exercised without the wrapper in
// tests.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/middleware"

	excerrors "exchange/pkg/errors"
)

// RBACDeps wires the RBAC HTTP surface.
type RBACDeps struct {
	Store      *admin.Store
	SVC        *admin.Service            // lifecycle engine
	Dual       *admin.DualControlService // four-eyes queue
	Routes     []gateway.Route           // registry snapshot for the role matrix
	TrustProxy bool                      // ClientIP via X-Forwarded-For
}

// adminActorID resolves the acting admin: middleware identity first
// (scope/env already enforced), then raw claims for test harnesses.
func adminActorID(r *http.Request) (int64, error) {
	if id := admin.IdentityFrom(r.Context()); id != nil {
		return id.UserID, nil
	}
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil {
		return 0, excerrors.New("UNAUTHORIZED", "authentication required")
	}
	uid, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || uid <= 0 {
		return 0, excerrors.New("UNAUTHORIZED", "admin identity unresolvable")
	}
	return uid, nil
}

func rbacFail(w http.ResponseWriter, r *http.Request, err error) {
	var e *excerrors.Error
	code, msg := "INTERNAL_ERROR", "internal error"
	if errors.As(err, &e) {
		code, msg = e.Code, e.Message
	}
	WriteError(w, code, msg, gateway.RequestIDFrom(r.Context()), nil)
}

// ---------------------------------------------------------------------------
// GET /api/v1/admin/roles — §8.2 role catalog + enforceable route matrix.
// ---------------------------------------------------------------------------

// AdminRoles returns the six §8.2 roles, their normative capability line
// and the generated role × route matrix from the registry snapshot.
func AdminRoles(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type roleRow struct {
			Role        string   `json:"role"`
			Permissions string   `json:"permissions"`
			Routes      []string `json:"routes"` // generated matrix row
		}
		rows := make([]roleRow, 0, len(admin.VenueRoles))
		for _, role := range admin.VenueRoles {
			rows = append(rows, roleRow{
				Role:        role,
				Permissions: admin.RoleSummary[role],
				Routes:      admin.PermittedRoutes(deps.Routes, role),
			})
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"roles":   rows,
			"systems": []string{admin.SystemVenueAdmin, admin.SystemClientDelegated, admin.SystemExternalAuditor},
			"note":    "matrix generated from the Task 5.3.7 route registry (spec §8.2)",
		})
	}
}

// ---------------------------------------------------------------------------
// Bindings: GET list (auditor read), POST grant, POST {id}/revoke —
// mutations flow through the four-eyes queue (§8.2 role change).
// ---------------------------------------------------------------------------

// AdminBindings lists admin_role_bindings (?user_id=&status=&limit=).
func AdminBindings(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var userID int64
		if s := q.Get("user_id"); s != "" {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil || v <= 0 {
				WriteError(w, "INVALID_REQUEST", "user_id must be a positive integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			userID = v
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		rows, err := deps.Store.ListBindings(r.Context(), userID, q.Get("status"), limit)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"bindings": rows})
	}
}

// grantRequest is the POST /admin/bindings body.
type grantRequest struct {
	UserID    int64        `json:"user_id"`
	Role      string       `json:"role"`
	Scope     *admin.Scope `json:"scope,omitempty"`
	ExpiresIn int64        `json:"expires_in_seconds"` // convenience
	ExpiresAt time.Time    `json:"expires_at"`
	Reason    string       `json:"reason"`
}

// AdminGrantBinding submits a role grant to the four-eyes queue — the
// binding lands only after a distinct Super Admin approver confirms
// (spec §8.2: "role change" is a dual-controlled operation).
func AdminGrantBinding(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorID(r)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		var body grantRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		expiresAt := body.ExpiresAt
		if expiresAt.IsZero() && body.ExpiresIn > 0 {
			expiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second).UTC()
		}
		if expiresAt.IsZero() {
			WriteError(w, "INVALID_REQUEST", "expires_at (or expires_in_seconds) is required — bindings are never permanent (spec §8.2b)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req, err := deps.Dual.Submit(r.Context(), admin.SubmitInput{
			Operation:  admin.OpAdminRoleChange,
			TargetType: "user",
			TargetID:   strconv.FormatInt(body.UserID, 10),
			Payload: map[string]any{
				"action": "grant", "user_id": body.UserID, "role": body.Role,
				"scope": body.Scope, "expires_at": expiresAt, "reason": body.Reason,
			},
			RequiredRole: admin.RoleSuperAdmin,
			RequestedBy:  actor,
			Reason:       body.Reason,
			ClientIP:     middleware.ClientIP(r, deps.TrustProxy),
		})
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"dual_control": "required", "request": req,
		})
	}
}

// AdminRevokeBinding submits a binding revocation to the four-eyes queue.
func AdminRevokeBinding(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorID(r)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		bindingID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || bindingID <= 0 {
			WriteError(w, "INVALID_REQUEST", "binding id required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // reason optional but urged
		req, err := deps.Dual.Submit(r.Context(), admin.SubmitInput{
			Operation:    admin.OpAdminRoleChange,
			TargetType:   "admin_role_binding",
			TargetID:     strconv.FormatInt(bindingID, 10),
			Payload:      map[string]any{"action": "revoke", "binding_id": bindingID, "reason": body.Reason},
			RequiredRole: admin.RoleSuperAdmin,
			RequestedBy:  actor,
			Reason:       body.Reason,
			ClientIP:     middleware.ClientIP(r, deps.TrustProxy),
		})
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"dual_control": "required", "request": req,
		})
	}
}

// RegisterRoleChangeExecutor attaches the grant/revoke executor that
// runs inside the approval transaction — the mutation and its
// four-eyes record commit atomically.
func RegisterRoleChangeExecutor(dual *admin.DualControlService, svc *admin.Service) {
	dual.RegisterExecutor(admin.OpAdminRoleChange,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p struct {
				Action    string       `json:"action"`
				UserID    int64        `json:"user_id"`
				Role      string       `json:"role"`
				Scope     *admin.Scope `json:"scope"`
				ExpiresAt time.Time    `json:"expires_at"`
				Reason    string       `json:"reason"`
				BindingID int64        `json:"binding_id"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.New("INVALID_REQUEST", "dual-control payload corrupt")
			}
			approver := int64(0)
			if req.ApprovedBy != nil {
				approver = *req.ApprovedBy
			}
			switch p.Action {
			case "grant":
				_, err := svc.GrantTx(ctx, tx, req.RequestedBy, admin.GrantInput{
					UserID: p.UserID, Role: p.Role, Scope: p.Scope,
					ExpiresAt: p.ExpiresAt, Reason: p.Reason,
				}, approver)
				return err
			case "revoke":
				b, err := svc.RevokeTx(ctx, tx, req.RequestedBy, p.BindingID, p.Reason, "")
				if err == nil {
					// Redis cannot join the tx; kill now — a rolled-back
					// revoke only costs the holder a re-login (fail-safe).
					svc.KillSessions(ctx, b.UserID)
				}
				return err
			}
			return excerrors.New("INVALID_REQUEST", "unknown role-change action")
		})
}

// ---------------------------------------------------------------------------
// Dual-control queue: list + decide.
// ---------------------------------------------------------------------------

// AdminDualControlList returns requests (?status=) for the admin surface.
func AdminDualControlList(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := deps.Dual.List(r.Context(), r.URL.Query().Get("status"), limit)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"requests": rows})
	}
}

// AdminDualControlApprove confirms a pending request — the second,
// distinct approver (the service enforces eligibility + distinctness).
func AdminDualControlApprove(deps *RBACDeps) http.HandlerFunc {
	return dualDecide(deps, true)
}

// AdminDualControlReject turns down a pending request.
func AdminDualControlReject(deps *RBACDeps) http.HandlerFunc {
	return dualDecide(deps, false)
}

func dualDecide(deps *RBACDeps, approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorID(r)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "request id required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ip := middleware.ClientIP(r, deps.TrustProxy)
		var req *admin.DualControlRequest
		if approve {
			req, err = deps.Dual.Approve(r.Context(), id, actor, ip)
		} else {
			req, err = deps.Dual.Reject(r.Context(), id, actor, ip)
		}
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"request": req})
	}
}

// ---------------------------------------------------------------------------
// Recertification campaigns (§8.2b.1 quarterly).
// ---------------------------------------------------------------------------

// AdminRecertStart opens a quarterly campaign: {label:"2026-Q4",
// ends_at:"..."} → snapshots ACTIVE STANDARD bindings into PENDING rows.
func AdminRecertStart(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorID(r)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		var body struct {
			Label  string    `json:"label"`
			EndsAt time.Time `json:"ends_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			body.Label == "" || body.EndsAt.IsZero() {
			WriteError(w, "INVALID_REQUEST", "label and ends_at are required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		c, err := deps.SVC.StartCampaign(r.Context(), body.Label, body.EndsAt,
			actor, middleware.ClientIP(r, deps.TrustProxy))
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"campaign": c})
	}
}

// AdminRecertReport is the Read-Only Auditor campaign export (§8.2b.1).
func AdminRecertReport(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "campaign id required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rep, err := deps.SVC.CampaignReport(r.Context(), id)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, rep)
	}
}

// AdminRecertDecide records one decision: {binding_id, approve, note}.
func AdminRecertDecide(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorID(r)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		campaignID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || campaignID <= 0 {
			WriteError(w, "INVALID_REQUEST", "campaign id required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			BindingID int64  `json:"binding_id"`
			Approve   bool   `json:"approve"`
			Note      string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.BindingID <= 0 {
			WriteError(w, "INVALID_REQUEST", "binding_id is required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := deps.SVC.DecideRecert(r.Context(), campaignID, body.BindingID,
			actor, body.Approve, body.Note,
			middleware.ClientIP(r, deps.TrustProxy)); err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"decision": map[string]any{
			"campaign_id": campaignID, "binding_id": body.BindingID,
			"decision": map[bool]string{true: "APPROVED", false: "SUSPENDED"}[body.Approve],
		}})
	}
}

// ---------------------------------------------------------------------------
// Break-glass (§8.2b.2) — grant + mandatory post-review.
// ---------------------------------------------------------------------------

// AdminBreakGlassGrant mints an incident-confined ≤4h Super Admin
// binding: {grantee_id, incident_ref, reason, ttl_seconds,
// second_approver_id | unreachable_approver, scope?}.
func AdminBreakGlassGrant(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorID(r)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		var body struct {
			GranteeID           int64        `json:"grantee_id"`
			IncidentRef         string       `json:"incident_ref"`
			Reason              string       `json:"reason"`
			TTLSeconds          int64        `json:"ttl_seconds"`
			SecondApproverID    int64        `json:"second_approver_id"`
			UnreachableApprover bool         `json:"unreachable_approver"`
			Scope               *admin.Scope `json:"scope,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		g, err := deps.SVC.GrantBreakGlass(r.Context(), actor, admin.BreakGlassInput{
			GranteeID:           body.GranteeID,
			IncidentRef:         body.IncidentRef,
			Reason:              body.Reason,
			TTL:                 time.Duration(body.TTLSeconds) * time.Second,
			SecondApproverID:    body.SecondApproverID,
			UnreachableApprover: body.UnreachableApprover,
			Scope:               body.Scope,
			ClientIP:            middleware.ClientIP(r, deps.TrustProxy),
		})
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"grant": g})
	}
}

// AdminBreakGlassReview records the mandatory post-incident review:
// {notes}.
func AdminBreakGlassReview(deps *RBACDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorID(r)
		if err != nil {
			rbacFail(w, r, err)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "grant id required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Notes string `json:"notes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := deps.SVC.ReviewBreakGlass(r.Context(), id, actor, body.Notes,
			middleware.ClientIP(r, deps.TrustProxy)); err != nil {
			rbacFail(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"grant_id": id, "status": "REVIEWED"})
	}
}
