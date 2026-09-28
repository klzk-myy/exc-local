package accounts

import (
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"

	excerrors "exchange/pkg/errors"
)

// Identity is the authenticated request context this cluster needs.
// The wiring layer adapts auth.Claims (auth.ClaimsFrom) into it —
// keeping this package free of the auth import while it is mid-flight.
type Identity struct {
	AccountID     int64    // trading account context; 0 = none selected
	UserID        int64    // numeric user id (claims subject)
	Scopes        []string // §8.8 granted scopes
	TwoFactorDone bool     // session AMR elevation (totp/fido2)
}

// HasScope reports whether the identity grants scope.
func (i *Identity) HasScope(scope string) bool {
	for _, s := range i.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// IdentityResolver extracts the authenticated identity from a request.
// nil resolver on Handler fails closed (401 on every endpoint). Wire it
// to auth.ClaimsFrom at composition:
//
//	h.ResolveIdentity = func(r *http.Request) *accounts.Identity {
//	    c := auth.ClaimsFrom(r.Context()); if c == nil { return nil }
//	    uid, _ := strconv.ParseInt(c.Subject, 10, 64)
//	    return &accounts.Identity{AccountID: c.AccountID, UserID: uid,
//	        Scopes: c.Scopes, TwoFactorDone: c.TwoFactorVerified()}
//	}
type IdentityResolver func(r *http.Request) *Identity

// handlers.go — thin net/http handlers for the account-state cluster,
// ready to mount. Final route registration is owned by Task 5.3.7's
// route registry; RouteSpecs() exposes the descriptors it needs
// (method, path, scope, admin role requirement).

// RouteSpec describes one mountable endpoint for the Task 5.3.7 route
// registry.
type RouteSpec struct {
	Method        string
	Path          string
	RequiredScope string // §8.8 scope; "" = authenticated only
	Admin         bool   // requires admin role resolution (freeze/limit)
}

// RouteSpecs lists every endpoint this cluster implements.
func (h *Handler) RouteSpecs() []RouteSpec {
	return []RouteSpec{
		{Method: http.MethodGet, Path: "/api/v1/account/sub-accounts", RequiredScope: ScopeRead},
		{Method: http.MethodGet, Path: "/api/v1/account/sub-accounts/aggregate", RequiredScope: ScopeRead},
		{Method: http.MethodPost, Path: "/api/v1/account/sub-accounts", RequiredScope: ScopeTrade},
		{Method: http.MethodPost, Path: "/api/v1/account/sub-accounts/{id}/api-keys", RequiredScope: ScopeTrade},
		{Method: http.MethodDelete, Path: "/api/v1/account/sub-accounts/{id}/api-keys/{keyId}", RequiredScope: ScopeTrade},
		{Method: http.MethodPut, Path: "/api/v1/admin/accounts/{id}/sub-account-limit", Admin: true},
		{Method: http.MethodPost, Path: "/api/v1/admin/accounts/{id}/freeze", Admin: true},
		{Method: http.MethodPost, Path: "/api/v1/admin/accounts/{id}/unfreeze", Admin: true},
		{Method: http.MethodPost, Path: "/api/v1/orders/countdown-cancel-all", RequiredScope: ScopeTrade},
		{Method: http.MethodPost, Path: "/api/v1/positions/close-all", RequiredScope: ScopeTrade},
	}
}

// EmittedCodes lists every error code this cluster can emit so the
// route registry (Task 5.3.7/5.3.21) can CheckRegistered them at startup —
// an unregistered emission fails closed rather than leaking an undeclared
// code. All entries live in internal/errs (§23 rows or localRows).
func EmittedCodes() []string {
	return []string{
		CodeInvalidRequest, CodeUnauthorized, CodeForbidden, CodeNotFound,
		CodeInsufficientScope, CodeAccountFrozen, CodeTwoFactorRequired,
		CodeDualControlRequired, CodeUnauthorizedRole,
		CodeCountdownInvalid, CodeCountdownAlreadyActive,
		CodeCloseAllPartialFailure, "INTERNAL_ERROR",
	}
}

// Mount attaches every route to mux — a convenience for the wiring layer;
// the canonical registration goes through Task 5.3.7's registry.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/account/sub-accounts", h.ListSubAccounts)
	mux.HandleFunc("GET /api/v1/account/sub-accounts/aggregate", h.AggregateSubAccounts)
	mux.HandleFunc("POST /api/v1/account/sub-accounts", h.CreateSubAccount)
	mux.HandleFunc("POST /api/v1/account/sub-accounts/{id}/api-keys", h.CreateSubAccountAPIKey)
	mux.HandleFunc("DELETE /api/v1/account/sub-accounts/{id}/api-keys/{keyId}", h.RevokeSubAccountAPIKey)
	mux.HandleFunc("PUT /api/v1/admin/accounts/{id}/sub-account-limit", h.SetSubAccountLimit)
	mux.HandleFunc("POST /api/v1/admin/accounts/{id}/freeze", h.FreezeAccount)
	mux.HandleFunc("POST /api/v1/admin/accounts/{id}/unfreeze", h.UnfreezeAccount)
	mux.HandleFunc("POST /api/v1/orders/countdown-cancel-all", h.CountdownCancelAll)
	mux.HandleFunc("POST /api/v1/positions/close-all", h.CloseAllPositions)
}

// Handler bundles the cluster services behind the HTTP surface.
// Any nil service fails its endpoints closed (coded error).
type Handler struct {
	Subs            *SubAccountService
	Keys            *APIKeyService
	Freeze          *FreezeService
	DeadMan         *DeadManService
	CloseAll        *CloseAllService
	ResolveIdentity IdentityResolver
}

// ---- sub-accounts (Task 5.3.11) ----

// ListSubAccounts handles GET /api/v1/account/sub-accounts.
func (h *Handler) ListSubAccounts(w http.ResponseWriter, r *http.Request) {
	masterID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	subs, err := h.Subs.List(r.Context(), masterID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": subs})
}

// AggregateSubAccounts handles GET /api/v1/account/sub-accounts/aggregate —
// the master's rolled-up family view (Task 5.3.11 item 3).
func (h *Handler) AggregateSubAccounts(w http.ResponseWriter, r *http.Request) {
	masterID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	view, err := h.Subs.Aggregate(r.Context(), masterID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// CreateSubAccount handles POST /api/v1/account/sub-accounts.
func (h *Handler) CreateSubAccount(w http.ResponseWriter, r *http.Request) {
	masterID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	sa, err := h.Subs.Create(r.Context(), masterID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sa)
}

// SetSubAccountLimit handles PUT /api/v1/admin/accounts/{id}/sub-account-limit.
// Role gate: Risk Manager / Super Admin (Task 5.3.11 item 6; Phase-07
// replaces the resolver stub).
func (h *Handler) SetSubAccountLimit(w http.ResponseWriter, r *http.Request) {
	admin, ok := h.requireAdmin(w, r, RoleRiskManager, RoleSuperAdmin)
	if !ok {
		return
	}
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, newError(CodeInvalidRequest, "invalid account id"))
		return
	}
	var body struct {
		MaxSubAccounts int `json:"max_sub_accounts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, newError(CodeInvalidRequest, "malformed body"))
		return
	}
	if err := h.Subs.SetLimit(r.Context(), accountID, body.MaxSubAccounts); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":       accountID,
		"max_sub_accounts": body.MaxSubAccounts,
		"adjusted_by":      admin.UserID,
	})
}

// CreateSubAccountAPIKey handles POST /api/v1/account/sub-accounts/{id}/api-keys.
func (h *Handler) CreateSubAccountAPIKey(w http.ResponseWriter, r *http.Request) {
	masterID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	subID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, newError(CodeInvalidRequest, "invalid sub-account id"))
		return
	}
	var body struct {
		Scopes []string `json:"scopes"`
		Label  string   `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, newError(CodeInvalidRequest, "malformed body"))
		return
	}
	issued, err := h.Keys.IssueForSubAccount(r.Context(), masterID, subID,
		body.Scopes, body.Label, h.subjectUserID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

// RevokeSubAccountAPIKey handles DELETE /api/v1/account/sub-accounts/{id}/api-keys/{keyId}.
func (h *Handler) RevokeSubAccountAPIKey(w http.ResponseWriter, r *http.Request) {
	masterID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	subID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, newError(CodeInvalidRequest, "invalid sub-account id"))
		return
	}
	keyID, err := strconv.ParseInt(r.PathValue("keyId"), 10, 64)
	if err != nil {
		writeErr(w, newError(CodeInvalidRequest, "invalid api key id"))
		return
	}
	if err := h.Keys.RevokeForSubAccount(r.Context(), masterID, subID, keyID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": keyID})
}

// ---- FROZEN legal hold (Task 5.3.12) ----

// FreezeAccount handles POST /api/v1/admin/accounts/{id}/freeze.
// Dual control: body.approver_id must be a distinct admin (Phase-05
// stub for Task 7.3.2's four-eyes workflow).
func (h *Handler) FreezeAccount(w http.ResponseWriter, r *http.Request) {
	h.freezeTransition(w, r, true)
}

// UnfreezeAccount handles POST /api/v1/admin/accounts/{id}/unfreeze.
func (h *Handler) UnfreezeAccount(w http.ResponseWriter, r *http.Request) {
	h.freezeTransition(w, r, false)
}

func (h *Handler) freezeTransition(w http.ResponseWriter, r *http.Request, freeze bool) {
	admin, ok := h.requireAdmin(w, r, RoleComplianceOfficer, RoleSuperAdmin)
	if !ok {
		return
	}
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, newError(CodeInvalidRequest, "invalid account id"))
		return
	}
	var body struct {
		Reason     string `json:"reason"`
		ApproverID int64  `json:"approver_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, newError(CodeInvalidRequest, "malformed body"))
		return
	}
	admin.ApproverID = body.ApproverID
	if freeze {
		err = h.Freeze.Freeze(r.Context(), admin, accountID, body.Reason, clientIP(r))
	} else {
		err = h.Freeze.Unfreeze(r.Context(), admin, accountID, body.Reason, clientIP(r))
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": accountID,
		"action":     map[bool]string{true: "freeze", false: "unfreeze"}[freeze],
	})
}

// ---- dead-man switch (Task 5.3.33) ----

// CountdownCancelAll handles POST /api/v1/orders/countdown-cancel-all —
// body {"countdown_ms": <1000..300000|0>, "renew": bool}. renew=false
// is a strict start (COUNTDOWN_ALREADY_ACTIVE when live); renew=true
// (default) is heartbeat refresh.
func (h *Handler) CountdownCancelAll(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	var body struct {
		CountdownMs *int64 `json:"countdown_ms"`
		Renew       *bool  `json:"renew"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, newError(CodeInvalidRequest, "malformed body"))
		return
	}
	if body.CountdownMs == nil {
		writeErr(w, newError(CodeInvalidRequest, "countdown_ms required"))
		return
	}
	renew := true
	if body.Renew != nil {
		renew = *body.Renew
	}
	ack, err := h.DeadMan.Set(r.Context(), accountID, *body.CountdownMs, renew)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

// ---- close-all positions (Task 5.3.36) ----

// CloseAllPositions handles POST /api/v1/positions/close-all —
// body {"symbol"?, "side"?, "max_slippage_bps"?}, header X-2FA-Token.
// Session AMR elevation (totp/fido2) satisfies the 2FA gate.
func (h *Handler) CloseAllPositions(w http.ResponseWriter, r *http.Request) {
	id := h.identity(r)
	if id == nil {
		writeErr(w, newError(CodeUnauthorized, "unauthenticated request"))
		return
	}
	accountID := id.AccountID
	if accountID == 0 {
		writeErr(w, newError(CodeUnauthorized, "account context required"))
		return
	}
	var body CloseAllFilter
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			writeErr(w, newError(CodeInvalidRequest, "malformed body"))
			return
		}
	}
	result, err := h.CloseAll.CloseAll(r.Context(), accountID, body,
		r.Header.Get("X-2FA-Token"), id.TwoFactorDone)
	if err != nil {
		// Partial failure still returns the per-position results body.
		var ce *excerrors.Error
		if stderrors.As(err, &ce) && ce.Code == CodeCloseAllPartialFailure && result != nil {
			writeJSON(w, statusFor(ce.Code), result)
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ---- helpers ----

// identity resolves the request identity through the injected resolver.
func (h *Handler) identity(r *http.Request) *Identity {
	if h.ResolveIdentity == nil {
		return nil
	}
	return h.ResolveIdentity(r)
}

// requireAccount returns the caller's account id for an authenticated
// request, writing the error envelope and returning false on failure.
func (h *Handler) requireAccount(w http.ResponseWriter, r *http.Request) (int64, bool) {
	c := h.identity(r)
	if c == nil {
		writeErr(w, newError(CodeUnauthorized, "unauthenticated request"))
		return 0, false
	}
	if c.AccountID == 0 {
		writeErr(w, newError(CodeUnauthorized, "account context required"))
		return 0, false
	}
	return c.AccountID, true
}

// subjectUserID returns the caller's numeric user id (0 → service falls
// back to the account owner for created_by).
func (h *Handler) subjectUserID(r *http.Request) int64 {
	if c := h.identity(r); c != nil {
		return c.UserID
	}
	return 0
}

// requireAdmin resolves the admin actor: numeric user id + role via the
// FreezeService resolver stub. allowedRoles must include the resolved
// role or the request rejects UNAUTHORIZED_ROLE.
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request, allowed ...string) (AdminActor, bool) {
	c := h.identity(r)
	if c == nil {
		writeErr(w, newError(CodeUnauthorized, "unauthenticated request"))
		return AdminActor{}, false
	}
	if c.UserID == 0 {
		writeErr(w, newError(CodeUnauthorized, "numeric admin user id required"))
		return AdminActor{}, false
	}
	actor := AdminActor{UserID: c.UserID}
	if h.Freeze == nil || h.Freeze.resolver == nil {
		writeErr(w, newError(CodeUnauthorizedRole, "role resolver not configured"))
		return AdminActor{}, false
	}
	role, err := h.Freeze.resolver(r.Context(), c.UserID)
	if err != nil {
		writeErr(w, errorf("INTERNAL_ERROR", "role lookup: %v", err))
		return AdminActor{}, false
	}
	actor.Role = role
	for _, a := range allowed {
		if role == a {
			return actor, true
		}
	}
	writeErr(w, errorf(CodeUnauthorizedRole, "role %q not permitted on this endpoint", role))
	return AdminActor{}, false
}

// clientIP extracts the remote address for audit columns. The real
// X-Forwarded-For normalization is the edge middleware's job (Task
// 5.3.29); this records the direct peer.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := len(host); i > 0 {
		for j := i - 1; j >= 0; j-- {
			if host[j] == ':' {
				return host[:j]
			}
		}
	}
	return host
}

// statusFor maps this cluster's emitted codes to HTTP status — mirrors
// the §23 registry (internal/errs) entries plus the §26-matrix codes it
// does not carry yet (TWO_FACTOR_REQUIRED, DUAL_CONTROL_REQUIRED,
// UNAUTHORIZED_ROLE — Task 5.3.21 will register them; until then they
// must not fall through to a 500).
func statusFor(code string) int {
	switch code {
	case CodeInvalidRequest, CodeCountdownInvalid, CodeDualControlRequired:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden, CodeInsufficientScope, CodeAccountFrozen,
		CodeTwoFactorRequired, CodeUnauthorizedRole:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeCountdownAlreadyActive, CodeCloseAllPartialFailure:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// writeErr serializes err via the spec §8.7 gateway error envelope.
func writeErr(w http.ResponseWriter, err error) {
	code := "INTERNAL_ERROR"
	msg := "internal error"
	var e *excerrors.Error
	if stderrors.As(err, &e) {
		code = e.Code
		msg = e.Message
	}
	writeError(w, statusFor(code), code, msg, nil)
}

// ---- §8.7 response writers ----
//
// Local copies of the gateway envelope (type/error/message/status) so
// this package does not import internal/api — which is owned by the
// route-registry cluster and is mid-flight.

// errorEnvelope mirrors the canonical §8.7 item-1 gateway error body.
type errorEnvelope struct {
	Type    string         `json:"type"`
	Error   string         `json:"error"`
	Message string         `json:"message"`
	Status  int            `json:"status"`
	Details map[string]any `json:"details,omitempty"`
}

// writeJSON serializes a success payload.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError serializes the §8.7 error envelope.
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	body, err := json.Marshal(errorEnvelope{
		Type: "error", Error: code, Message: message, Status: status, Details: details,
	})
	if err != nil {
		body = []byte(`{"type":"error","error":"INTERNAL_ERROR","message":"internal error","status":500}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
