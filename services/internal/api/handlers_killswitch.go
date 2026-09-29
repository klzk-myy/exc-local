// Phase-11 Tasks 11.3.4 / 11.3.8 / 11.3.12 — kill-switch admin surface.
//
// POST /api/v1/admin/kill-switch         {scope, target_id, reason, approver_id}
// POST /api/v1/admin/kill-switch/reset   {scope, target_id, reason?, approver_id}
// GET  /api/v1/admin/kill-switch         — ACTIVE suspension set
//
// Guarantees (task ACs):
//   - Risk Manager+ via the route registry Auth.Role; the service
//     re-verifies actor + approver roles through the admin role
//     resolver — a nil resolver fails closed.
//   - Dual control for GLOBAL (and the destructive COUNTERPARTY scope):
//     a distinct approver_id is required; scoped kills are
//     single-approver per Task 11.3.8.
//   - New orders are rejected TRADING_HALTED while flags are raised;
//     cancels/reads/WS survive (middleware.KillSwitchGate +
//     orders.Service seam).
//   - Every transition is recorded in trading_suspensions +
//     admin_audit_log and broadcast (announcement, WS venue.status,
//     Aeron/NATS exchange:control:killswitch).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"exchange/internal/admin"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
)

// killSwitchService is the narrow handler seam — *admin.KillSwitchService
// satisfies it; tests substitute a fake.
type killSwitchService interface {
	Set(ctx context.Context, actor admin.AdminActor,
		scope, target, reason string) (*admin.Suspension, error)
	Clear(ctx context.Context, actor admin.AdminActor,
		scope, target, reason string) (*admin.Suspension, error)
	Status(ctx context.Context) ([]admin.Suspension, error)
}

// killSwitchBody is the set/reset payload; approver_id accepts a JSON
// number or numeric string (admin consoles differ).
type killSwitchBody struct {
	Scope      string          `json:"scope"`
	TargetID   json.RawMessage `json:"target_id"`
	Reason     string          `json:"reason"`
	ApproverID json.RawMessage `json:"approver_id"`
}

// killSwitchActor builds the service actor from the RBAC identity the
// gateway middleware resolved (WrapRoute) — a missing identity fails
// closed with UNAUTHORIZED rather than fabricating a principal. Named
// distinctly from handlers_support.go's claims-based adminActor —
// this seam needs the RBAC AdminActor shape (approver + client IP).
func killSwitchActor(w http.ResponseWriter, r *http.Request,
	approver json.RawMessage, trustProxy bool) (admin.AdminActor, bool) {
	id := admin.IdentityFrom(r.Context())
	if id == nil || id.UserID <= 0 {
		WriteError(w, "UNAUTHORIZED", "admin identity required",
			gateway.RequestIDFrom(r.Context()), nil)
		return admin.AdminActor{}, false
	}
	ap, err := rawInt64(approver)
	if err != nil {
		WriteError(w, "INVALID_REQUEST", "approver_id must be an integer",
			gateway.RequestIDFrom(r.Context()), nil)
		return admin.AdminActor{}, false
	}
	return admin.AdminActor{
		UserID:     id.UserID,
		ApproverID: ap,
		ClientIP:   middleware.ClientIP(r, trustProxy),
	}, true
}

// rawInt64 parses an int64 from a JSON number or quoted string; empty
// input yields 0 (treated as absent by the service).
func rawInt64(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// rawString parses a JSON string or number into its plain text form —
// target_id is VARCHAR in the record (account ids, symbols, session
// ids, rail names all flow through it).
func rawString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", err
	}
	return n.String(), nil
}

// AdminKillSwitchSet — POST /api/v1/admin/kill-switch.
func AdminKillSwitchSet(svc killSwitchService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body killSwitchBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		target, err := rawString(body.TargetID)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "target_id must be a string or integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, ok := killSwitchActor(w, r, body.ApproverID, trustProxy)
		if !ok {
			return
		}
		rec, err := svc.Set(r.Context(), actor, body.Scope, target, body.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, rec)
	}
}

// AdminKillSwitchReset — POST /api/v1/admin/kill-switch/reset.
func AdminKillSwitchReset(svc killSwitchService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body killSwitchBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		target, err := rawString(body.TargetID)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "target_id must be a string or integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, ok := killSwitchActor(w, r, body.ApproverID, trustProxy)
		if !ok {
			return
		}
		rec, err := svc.Clear(r.Context(), actor, body.Scope, target, body.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, rec)
	}
}

// AdminKillSwitchStatus — GET /api/v1/admin/kill-switch. Lists the
// ACTIVE suspension set (operators + the CANCEL-only enforcement
// surface need visibility into which scopes are halted).
func AdminKillSwitchStatus(svc killSwitchService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := svc.Status(r.Context())
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"suspensions": rows})
	}
}
