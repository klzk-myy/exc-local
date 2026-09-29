// Task 12.3.11 — institutional delegated logins + client M-of-N
// multi-validator handlers.
//
//	GET    /api/v1/account/delegated-users                    list
//	POST   /api/v1/account/delegated-users                    create
//	PUT    /api/v1/account/delegated-users/{id}               role/scope update
//	DELETE /api/v1/account/delegated-users/{id}               revoke one
//	POST   /api/v1/account/delegated-users/revoke-all         emergency master revocation
//	GET    /api/v1/account/approval-policies                  list
//	PUT    /api/v1/account/approval-policies                  upsert M-of-N policy
//	DELETE /api/v1/account/approval-policies/{id}             disable policy
//	GET    /api/v1/account/approval-requests                  list (?status=&limit=)
//	POST   /api/v1/account/approval-requests/{id}/decide      approve|reject vote
//
// Management surfaces (users/policies) are master-owner only — the
// service enforces requireMaster; delegate sessions can only read the
// approval-request list and cast Decide votes (CLIENT_APPROVER binding
// required, enforced inside delegation.Service).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/delegation"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// delegationService is the handler-facing seam over
// delegation.Service.
type delegationService interface {
	CreateDelegatedUser(ctx context.Context, masterID, actorUserID int64,
		req delegation.CreateDelegatedUserRequest) (*delegation.DelegatedUser, error)
	List(ctx context.Context, masterID, actorUserID int64) ([]delegation.DelegatedUser, error)
	UpdateBinding(ctx context.Context, masterID, actorUserID, delegatedID int64,
		role delegation.Role, scope delegation.Scope, expiresAt *time.Time) (*delegation.DelegatedUser, error)
	Revoke(ctx context.Context, masterID, actorUserID, delegatedID int64, reason string) error
	RevokeAll(ctx context.Context, masterID, actorUserID int64, reason string) (int, error)
	SetPolicy(ctx context.Context, masterID, actorUserID int64,
		op delegation.Operation, required int, threshold *decimal.Decimal,
		thresholdCCY string, expiresIn int) (*delegation.Policy, error)
	ListPolicies(ctx context.Context, masterID, actorUserID int64) ([]delegation.Policy, error)
	DisablePolicy(ctx context.Context, masterID, actorUserID, policyID int64) error
	ListRequests(ctx context.Context, masterID, actorUserID int64,
		status string, limit int) ([]delegation.ApprovalRequest, error)
	Decide(ctx context.Context, requestID, approverUserID int64,
		approve bool, note string) (*delegation.ApprovalRequest, error)
}

// delegationUserID resolves the numeric principal for delegation calls —
// unlike emergency-freeze (session-bound only), delegated management
// still requires a numeric user subject; non-numeric subs are refused.
func delegationUserID(w http.ResponseWriter, r *http.Request) (accountID, userID int64, ok bool) {
	accountID, claims, ok := claimsAccount(w, r)
	if !ok {
		return 0, 0, false
	}
	userID, err := parseSubjectID(claims.Subject)
	if err != nil || userID <= 0 {
		WriteError(w, "UNAUTHORIZED", "numeric user identity required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, 0, false
	}
	return accountID, userID, true
}

func delegationUnavailable(w http.ResponseWriter, r *http.Request) {
	WriteError(w, "SERVICE_DEGRADED", "delegation service unavailable",
		gateway.RequestIDFrom(r.Context()), nil)
}

// ---------------------------------------------------------------------------
// Delegated users
// ---------------------------------------------------------------------------

// DelegatedUsersList serves GET /api/v1/account/delegated-users.
func DelegatedUsersList(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		rows, err := svc.List(r.Context(), accountID, userID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []delegation.DelegatedUser{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID, "delegated_users": rows,
		})
	}
}

type delegatedUserCreateBody struct {
	UserID      int64            `json:"user_id"`
	DisplayName string           `json:"display_name"`
	Role        delegation.Role  `json:"role"`
	Scope       delegation.Scope `json:"scope"`
	ExpiresAt   *time.Time       `json:"expires_at,omitempty"`
}

// DelegatedUserCreate serves POST /api/v1/account/delegated-users.
func DelegatedUserCreate(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		var body delegatedUserCreateBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		du, err := svc.CreateDelegatedUser(r.Context(), accountID, userID,
			delegation.CreateDelegatedUserRequest{
				UserID:      body.UserID,
				DisplayName: body.DisplayName,
				Role:        body.Role,
				Scope:       body.Scope,
				ExpiresAt:   body.ExpiresAt,
			})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, du)
	}
}

type delegatedUserUpdateBody struct {
	Role      delegation.Role  `json:"role"`
	Scope     delegation.Scope `json:"scope"`
	ExpiresAt *time.Time       `json:"expires_at,omitempty"`
}

// DelegatedUserUpdate serves PUT /api/v1/account/delegated-users/{id} —
// rewrites the ACTIVE binding's role+scope (SCOPE_CHANGED audit).
func DelegatedUserUpdate(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "delegated user id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body delegatedUserUpdateBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		du, err := svc.UpdateBinding(r.Context(), accountID, userID, id,
			body.Role, body.Scope, body.ExpiresAt)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, du)
	}
}

type revokeBody struct {
	Reason string `json:"reason,omitempty"`
}

// DelegatedUserRevoke serves DELETE /api/v1/account/delegated-users/{id}
// — revokes the binding + delegated user and kills its sessions.
func DelegatedUserRevoke(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "delegated user id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body revokeBody
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body) // reason optional
		}
		if err := svc.Revoke(r.Context(), accountID, userID, id, body.Reason); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"delegated_user_id": id, "status": delegation.StatusRevoked,
		})
	}
}

// DelegatedUsersRevokeAll serves POST
// /api/v1/account/delegated-users/revoke-all — emergency master
// revocation: every delegation dies, sessions terminate immediately.
func DelegatedUsersRevokeAll(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		var body revokeBody
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		n, err := svc.RevokeAll(r.Context(), accountID, userID, body.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID, "revoked": n,
		})
	}
}

// ---------------------------------------------------------------------------
// Approval policies
// ---------------------------------------------------------------------------

// ApprovalPoliciesList serves GET /api/v1/account/approval-policies.
func ApprovalPoliciesList(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		rows, err := svc.ListPolicies(r.Context(), accountID, userID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []delegation.Policy{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID, "policies": rows,
		})
	}
}

type approvalPolicyBody struct {
	Operation         string `json:"operation"`
	RequiredApprovals int    `json:"required_approvals"`
	ThresholdAmount   string `json:"threshold_amount,omitempty"`   // decimal text
	ThresholdCurrency string `json:"threshold_currency,omitempty"` // ISO 4217
	ExpiresInSeconds  int    `json:"expires_in_seconds,omitempty"`
}

// ApprovalPolicySet serves PUT /api/v1/account/approval-policies —
// upserts the (account, operation) M-of-N policy.
func ApprovalPolicySet(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		var body approvalPolicyBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var threshold *decimal.Decimal
		if body.ThresholdAmount != "" {
			d, err := decimal.NewFromString(body.ThresholdAmount)
			if err != nil || d.IsNegative() {
				WriteError(w, "INVALID_REQUEST",
					"threshold_amount must be a non-negative decimal",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			threshold = &d
		}
		p, err := svc.SetPolicy(r.Context(), accountID, userID,
			delegation.Operation(body.Operation), body.RequiredApprovals,
			threshold, body.ThresholdCurrency, body.ExpiresInSeconds)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, p)
	}
}

// ApprovalPolicyDisable serves DELETE
// /api/v1/account/approval-policies/{id} — soft-disable (row stays for
// audit; open requests still decide, new requests reject).
func ApprovalPolicyDisable(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "policy id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err := svc.DisablePolicy(r.Context(), accountID, userID, id); err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"policy_id": id, "status": "DISABLED",
		})
	}
}

// ---------------------------------------------------------------------------
// Approval requests
// ---------------------------------------------------------------------------

// ApprovalRequestsList serves GET /api/v1/account/approval-requests
// (?status=&limit=).
func ApprovalRequestsList(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		rows, err := svc.ListRequests(r.Context(), accountID, userID,
			r.URL.Query().Get("status"), parseLimitQuery(r.URL.Query().Get("limit"), 100, 500))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []delegation.ApprovalRequest{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID, "approval_requests": rows,
		})
	}
}

type approvalDecideBody struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note,omitempty"`
}

// ApprovalRequestDecide serves POST
// /api/v1/account/approval-requests/{id}/decide — one CLIENT_APPROVER
// vote; M-of-N tally runs inside the service (anti-self-approval,
// expiry, single-vote enforced).
func ApprovalRequestDecide(svc delegationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, userID, ok := delegationUserID(w, r)
		if !ok {
			return
		}
		if svc == nil {
			delegationUnavailable(w, r)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "approval request id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body approvalDecideBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if len(body.Note) > 512 {
			WriteError(w, "INVALID_REQUEST", "note exceeds 512 chars",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		req, err := svc.Decide(r.Context(), id, userID, body.Approve, body.Note)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, req)
	}
}
