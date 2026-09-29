// Service: the delegated-login data model, policy engine and audit
// trail for Task 12.3.11. All mutations run in transactions that append
// client_delegation_events rows — the audit record is part of the
// atomic unit of work, never an afterthought.
package delegation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
)

// SessionTerminator kills a delegated user's live sessions on revoke —
// emergency master revocation must terminate them immediately (task
// item 4). Wired to the session manager's revoke-all path at
// composition; a nil seam means the session-kill step reports the gap
// (the revoke itself still commits — the delegate is already denied by
// the dead binding).
type SessionTerminator interface {
	// KillUserSessions revokes every session owned by userID across the
	// user and all account indexes.
	KillUserSessions(ctx context.Context, userID int64) error
}

// Service is the delegation domain service.
type Service struct {
	pool     *pgxpool.Pool
	sessions SessionTerminator
	now      func() time.Time
}

// NewService wires the service; sessions may be nil (revokes then
// report the gap rather than silently leaving sessions live).
func NewService(pool *pgxpool.Pool, sessions SessionTerminator) *Service {
	return &Service{pool: pool, sessions: sessions, now: time.Now}
}

// audit appends one client_delegation_events row inside tx.
func audit(ctx context.Context, tx pgx.Tx, masterID int64, actorID *int64,
	delegatedID *int64, event, targetType string, targetID *int64, detail map[string]any) error {
	d, err := json.Marshal(detail)
	if err != nil {
		d = []byte(`{}`)
	}
	var tt any
	if targetType != "" {
		tt = targetType
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO client_delegation_events
		   (master_account_id, actor_user_id, delegated_user_id, event,
		    target_type, target_id, detail)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		masterID, actorID, delegatedID, event, tt, targetID, d)
	return err
}

// auditOutside appends an event outside a transaction (best-effort read
// paths — e.g. DELEGATED_LOGIN recorded by the login flow).
func (s *Service) auditOutside(ctx context.Context, masterID int64, actorID *int64,
	delegatedID *int64, event, targetType string, targetID *int64, detail map[string]any) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errorf("INTERNAL_ERROR", "audit tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := audit(ctx, tx, masterID, actorID, delegatedID, event, targetType, targetID, detail); err != nil {
		return errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	return tx.Commit(ctx)
}

// requireMaster confirms actorUserID owns masterID — delegation
// management is master-owner only; a delegate can never mint or mutate
// delegations on its own account.
func (s *Service) requireMaster(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, masterID, actorUserID int64) error {
	var owner int64
	err := q.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id = $1`, masterID).Scan(&owner)
	if err == pgx.ErrNoRows {
		return errorf(CodeNotFound, "account %d not found", masterID)
	}
	if err != nil {
		return errorf("INTERNAL_ERROR", "account owner read: %v", err)
	}
	if owner != actorUserID {
		return newError(CodeForbidden,
			"delegation management requires the master account owner")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Delegated users
// ---------------------------------------------------------------------------

// CreateDelegatedUserRequest is the create payload.
type CreateDelegatedUserRequest struct {
	UserID      int64      `json:"user_id"` // existing users row (login flow owns creation)
	DisplayName string     `json:"display_name"`
	Role        Role       `json:"role"`
	Scope       Scope      `json:"scope"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"` // optional binding expiry
}

// CreateDelegatedUser names a human login under the master account and
// grants it one role+scope binding. The principal is registered in
// principal_role_systems as CLIENT_DELEGATED — a principal already
// holding another role system is rejected (disjoint §8.2a exclusion).
func (s *Service) CreateDelegatedUser(ctx context.Context, masterID, actorUserID int64,
	req CreateDelegatedUserRequest) (*DelegatedUser, error) {

	if req.UserID == 0 {
		return nil, newError(CodeInvalidRequest, "user_id required")
	}
	if req.DisplayName == "" || len(req.DisplayName) > 128 {
		return nil, newError(CodeInvalidRequest, "display_name required (≤128 chars)")
	}
	if !ValidRole(req.Role) {
		return nil, errorf(CodeInvalidRequest, "invalid role %q", req.Role)
	}
	if err := validateScope(req.Scope); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.requireMaster(ctx, tx, masterID, actorUserID); err != nil {
		return nil, err
	}
	if req.UserID == actorUserID {
		return nil, newError(CodeInvalidRequest,
			"the master owner cannot be a delegate of its own account")
	}
	// The delegated principal must be a real user.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, req.UserID).Scan(&exists); err != nil {
		return nil, errorf("INTERNAL_ERROR", "user check: %v", err)
	}
	if !exists {
		return nil, errorf(CodeNotFound, "user %d not found", req.UserID)
	}
	// Scoped account ids must stay inside the master family.
	if err := s.checkScopeAccounts(ctx, tx, masterID, req.Scope); err != nil {
		return nil, err
	}
	// Disjoint role systems (migration 090 exclusion): a VENUE_ADMIN or
	// EXTERNAL_AUDITOR principal can never become a client delegate.
	if _, err := tx.Exec(ctx,
		`INSERT INTO principal_role_systems (principal_id, role_system, first_granted_by)
		 VALUES ($1,'CLIENT_DELEGATED',$2)
		 ON CONFLICT (principal_id) DO NOTHING`, req.UserID, actorUserID); err != nil {
		return nil, errorf("INTERNAL_ERROR", "role-system registration: %v", err)
	}
	var sys string
	if err := tx.QueryRow(ctx,
		`SELECT role_system FROM principal_role_systems WHERE principal_id=$1`,
		req.UserID).Scan(&sys); err != nil {
		return nil, errorf("INTERNAL_ERROR", "role-system read: %v", err)
	}
	if sys != "CLIENT_DELEGATED" {
		return nil, errorf(CodeForbidden,
			"principal %d already belongs to role system %s", req.UserID, sys)
	}

	var du DelegatedUser
	err = tx.QueryRow(ctx,
		`INSERT INTO client_delegated_users
		   (master_account_id, user_id, display_name, created_by)
		 VALUES ($1,$2,$3,$4)
		 RETURNING id, master_account_id, user_id, display_name, status,
		           created_by, created_at`,
		masterID, req.UserID, req.DisplayName, actorUserID).Scan(
		&du.ID, &du.MasterAccount, &du.UserID, &du.DisplayName, &du.Status,
		&du.CreatedBy, &du.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, errorf(CodeInvalidRequest,
				"user %d is already delegated under account %d", req.UserID, masterID)
		}
		return nil, errorf("INTERNAL_ERROR", "delegated user insert: %v", err)
	}

	scopeJSON, _ := json.Marshal(req.Scope)
	err = tx.QueryRow(ctx,
		`INSERT INTO client_role_bindings
		   (delegated_user_id, role, scope, granted_by, expires_at)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		du.ID, string(req.Role), scopeJSON, actorUserID, req.ExpiresAt).Scan(&du.BindingID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "role binding insert: %v", err)
	}
	du.Role, du.Scope, du.BindingExpires = req.Role, req.Scope, req.ExpiresAt

	if err := audit(ctx, tx, masterID, &actorUserID, &du.ID,
		EvDelegationCreated, "delegated_user", &du.ID, map[string]any{
			"user_id": req.UserID, "display_name": req.DisplayName,
			"role": string(req.Role), "scope": req.Scope,
		}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit delegation: %v", err)
	}
	return &du, nil
}

// validateScope enforces the explicit-scope contract: at least the
// account axis must be populated (a binding with no account scope is a
// no-op grant — useless and a red flag).
func validateScope(s Scope) error {
	for _, id := range s.AccountIDs {
		if id <= 0 {
			return errorf(CodeInvalidRequest, "invalid account id %d in scope", id)
		}
	}
	for _, sym := range s.Instruments {
		if len(sym) == 0 || len(sym) > 16 {
			return errorf(CodeInvalidRequest, "invalid instrument %q in scope", sym)
		}
	}
	if len(s.AccountIDs) == 0 {
		return newError(CodeInvalidRequest,
			"scope.account_ids required — explicit account/sub-account scopes")
	}
	return nil
}

// checkScopeAccounts verifies every scoped account id is the master or
// one of its direct sub-accounts.
func (s *Service) checkScopeAccounts(ctx context.Context, tx pgx.Tx,
	masterID int64, scope Scope) error {
	for _, id := range scope.AccountIDs {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM accounts
			    WHERE id=$1 AND (id=$2 OR parent_account_id=$2))`,
			id, masterID).Scan(&ok); err != nil {
			return errorf("INTERNAL_ERROR", "scope account check: %v", err)
		}
		if !ok {
			return errorf(CodeForbidden,
				"scope account %d is outside the master %d family", id, masterID)
		}
	}
	return nil
}

// List returns all delegated users (any status) for the master, each
// with its ACTIVE binding when present.
func (s *Service) List(ctx context.Context, masterID, actorUserID int64) ([]DelegatedUser, error) {
	if err := s.requireMaster(ctx, s.pool, masterID, actorUserID); err != nil {
		return nil, err
	}
	return s.listAll(ctx, masterID)
}

func (s *Service) listAll(ctx context.Context, masterID int64) ([]DelegatedUser, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT du.id, du.master_account_id, du.user_id, du.display_name,
		        du.status, du.created_by, du.created_at,
		        COALESCE(du.suspend_reason,''), du.revoked_at, du.revoked_by,
		        COALESCE(du.revoke_reason,''),
		        b.id, b.role, b.scope, b.expires_at
		   FROM client_delegated_users du
		   LEFT JOIN client_role_bindings b
		     ON b.delegated_user_id = du.id AND b.status = 'ACTIVE'
		  WHERE du.master_account_id = $1
		  ORDER BY du.id`, masterID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "delegated user list: %v", err)
	}
	defer rows.Close()
	out := []DelegatedUser{}
	for rows.Next() {
		var du DelegatedUser
		var bid *int64
		var role *string
		var scopeRaw []byte
		var expires *time.Time
		if err := rows.Scan(&du.ID, &du.MasterAccount, &du.UserID, &du.DisplayName,
			&du.Status, &du.CreatedBy, &du.CreatedAt, &du.SuspendReason,
			&du.RevokedAt, &du.RevokedBy, &du.RevokeReason,
			&bid, &role, &scopeRaw, &expires); err != nil {
			return nil, errorf("INTERNAL_ERROR", "delegated user scan: %v", err)
		}
		if bid != nil {
			du.BindingID = *bid
			du.Role = Role(*role)
			du.BindingExpires = expires
			if len(scopeRaw) > 0 {
				_ = json.Unmarshal(scopeRaw, &du.Scope)
			}
		}
		out = append(out, du)
	}
	return out, rows.Err()
}

// Get returns one delegated user scoped to the master.
func (s *Service) Get(ctx context.Context, masterID, actorUserID, delegatedID int64) (*DelegatedUser, error) {
	if err := s.requireMaster(ctx, s.pool, masterID, actorUserID); err != nil {
		return nil, err
	}
	all, err := s.listAll(ctx, masterID)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == delegatedID {
			return &all[i], nil
		}
	}
	return nil, errorf(CodeNotFound, "delegated user %d not found", delegatedID)
}

// UpdateBinding rewrites the ACTIVE binding's role/scope — the
// scope-change audit event records before/after (task item 4).
func (s *Service) UpdateBinding(ctx context.Context, masterID, actorUserID, delegatedID int64,
	role Role, scope Scope, expiresAt *time.Time) (*DelegatedUser, error) {

	if !ValidRole(role) {
		return nil, errorf(CodeInvalidRequest, "invalid role %q", role)
	}
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.requireMaster(ctx, tx, masterID, actorUserID); err != nil {
		return nil, err
	}
	var duStatus string
	err = tx.QueryRow(ctx,
		`SELECT status FROM client_delegated_users
		  WHERE id=$1 AND master_account_id=$2 FOR UPDATE`,
		delegatedID, masterID).Scan(&duStatus)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeNotFound, "delegated user %d not found", delegatedID)
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "delegated user lock: %v", err)
	}
	if duStatus != StatusActive {
		return nil, errorf(CodeInvalidLifecycle,
			"delegated user %d is %s — scope changes require ACTIVE", delegatedID, duStatus)
	}
	if err := s.checkScopeAccounts(ctx, tx, masterID, scope); err != nil {
		return nil, err
	}
	var bid int64
	var prevRole string
	var prevScope []byte
	err = tx.QueryRow(ctx,
		`SELECT id, role, scope FROM client_role_bindings
		  WHERE delegated_user_id=$1 AND status='ACTIVE' FOR UPDATE`,
		delegatedID).Scan(&bid, &prevRole, &prevScope)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeNotFound,
			"delegated user %d has no active binding", delegatedID)
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "binding lock: %v", err)
	}
	scopeJSON, _ := json.Marshal(scope)
	if _, err := tx.Exec(ctx,
		`UPDATE client_role_bindings
		    SET role=$2, scope=$3, expires_at=$4, updated_at=now()
		  WHERE id=$1`, bid, string(role), scopeJSON, expiresAt); err != nil {
		return nil, errorf("INTERNAL_ERROR", "binding update: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE client_delegated_users SET updated_at=now() WHERE id=$1`,
		delegatedID); err != nil {
		return nil, errorf("INTERNAL_ERROR", "delegated user touch: %v", err)
	}
	var prev map[string]any
	_ = json.Unmarshal(prevScope, &prev)
	if err := audit(ctx, tx, masterID, &actorUserID, &delegatedID,
		EvBindingUpdated, "role_binding", &bid, map[string]any{
			"prev_role": prevRole, "new_role": string(role),
			"prev_scope": prev, "new_scope": scope,
			"expires_at": expiresAt,
		}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit scope update: %v", err)
	}
	return s.Get(ctx, masterID, actorUserID, delegatedID)
}

// Revoke terminates one delegated login: status → REVOKED, ACTIVE
// binding → REVOKED, live sessions killed via the injected terminator.
// The row stays (audit) — revocation is never deletion.
func (s *Service) Revoke(ctx context.Context, masterID, actorUserID, delegatedID int64,
	reason string) error {
	uid, err := s.revokeOne(ctx, masterID, actorUserID, delegatedID, reason)
	if err != nil {
		return err
	}
	// Session kill outside the tx — the binding is already dead, so the
	// delegate cannot act even if the kill races.
	if s.sessions != nil {
		if err := s.sessions.KillUserSessions(ctx, uid); err != nil {
			return errorf("INTERNAL_ERROR",
				"delegated user %d revoked but session termination failed: %v",
				delegatedID, err)
		}
	}
	return nil
}

func (s *Service) revokeOne(ctx context.Context, masterID, actorUserID,
	delegatedID int64, reason string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.requireMaster(ctx, tx, masterID, actorUserID); err != nil {
		return 0, err
	}
	var uid int64
	var st string
	err = tx.QueryRow(ctx,
		`SELECT user_id, status FROM client_delegated_users
		  WHERE id=$1 AND master_account_id=$2 FOR UPDATE`,
		delegatedID, masterID).Scan(&uid, &st)
	if err == pgx.ErrNoRows {
		return 0, errorf(CodeNotFound, "delegated user %d not found", delegatedID)
	}
	if err != nil {
		return 0, errorf("INTERNAL_ERROR", "delegated user lock: %v", err)
	}
	if st == StatusRevoked {
		return 0, errorf(CodeInvalidLifecycle,
			"delegated user %d already revoked", delegatedID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE client_role_bindings
		    SET status='REVOKED', revoked_at=now(), revoked_by=$2,
		        revoke_reason=$3, updated_at=now()
		  WHERE delegated_user_id=$1 AND status='ACTIVE'`,
		delegatedID, actorUserID, reason); err != nil {
		return 0, errorf("INTERNAL_ERROR", "binding revoke: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE client_delegated_users
		    SET status='REVOKED', revoked_at=now(), revoked_by=$2,
		        revoke_reason=$3, updated_at=now()
		  WHERE id=$1`, delegatedID, actorUserID, reason); err != nil {
		return 0, errorf("INTERNAL_ERROR", "delegated user revoke: %v", err)
	}
	if err := audit(ctx, tx, masterID, &actorUserID, &delegatedID,
		EvDelegationRevoked, "delegated_user", &delegatedID,
		map[string]any{"user_id": uid, "reason": reason}); err != nil {
		return 0, errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, errorf("INTERNAL_ERROR", "commit revoke: %v", err)
	}
	return uid, nil
}

// RevokeAll is the emergency master revocation (task item 4): every
// non-terminal delegated user is revoked and all their sessions die
// immediately.
func (s *Service) RevokeAll(ctx context.Context, masterID, actorUserID int64,
	reason string) (int, error) {
	all, err := s.listAll(ctx, masterID)
	if err != nil {
		return 0, err
	}
	if err := s.requireMaster(ctx, s.pool, masterID, actorUserID); err != nil {
		return 0, err
	}
	revoked := 0
	var killErr error
	for _, du := range all {
		if du.Status == StatusRevoked {
			continue
		}
		uid, err := s.revokeOne(ctx, masterID, actorUserID, du.ID, reason)
		if err != nil {
			return revoked, err
		}
		revoked++
		if s.sessions != nil {
			if err := s.sessions.KillUserSessions(ctx, uid); err != nil {
				killErr = err // keep revoking the rest — never stop halfway
			}
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return revoked, errorf("INTERNAL_ERROR", "audit tx: %v", err)
	}
	if err := audit(ctx, tx, masterID, &actorUserID, nil,
		EvRevokeAll, "account", &masterID, map[string]any{
			"reason": reason, "revoked": revoked,
		}); err != nil {
		_ = tx.Rollback(ctx)
		return revoked, errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return revoked, errorf("INTERNAL_ERROR", "commit revoke-all audit: %v", err)
	}
	return revoked, killErr
}

// RecordLogin appends the DELEGATED_LOGIN audit event — the seam the
// cluster-1 login flow calls after credential verification succeeds
// (task item 4: "every delegation, login, … is audit-logged").
func (s *Service) RecordLogin(ctx context.Context, masterID, delegatedID, userID int64,
	ip string) error {
	return s.auditOutside(ctx, masterID, &userID, &delegatedID,
		EvDelegatedLogin, "session", nil, map[string]any{"ip": ip})
}

// RecordAction is the generic DELEGATED_ACTION audit seam — consumers
// (order path, transfer path) record what a delegate did under which
// scope.
func (s *Service) RecordAction(ctx context.Context, masterID, delegatedID, userID int64,
	action string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["action"] = action
	return s.auditOutside(ctx, masterID, &userID, &delegatedID,
		EvDelegatedAction, "", nil, detail)
}

// ---------------------------------------------------------------------------
// Approval policies
// ---------------------------------------------------------------------------

// SetPolicy upserts the (master, operation) M-of-N policy. The master
// owner configures it; required_approvals is M of the M-of-N contract.
func (s *Service) SetPolicy(ctx context.Context, masterID, actorUserID int64,
	op Operation, required int, threshold *decimal.Decimal, thresholdCCY string,
	expiresIn int) (*Policy, error) {

	if !ValidOperation(op) {
		return nil, errorf(CodeInvalidRequest, "invalid operation %q", op)
	}
	if required < 1 {
		return nil, newError(CodeInvalidRequest, "required_approvals must be >= 1")
	}
	if len(thresholdCCY) > 3 {
		return nil, newError(CodeInvalidRequest,
			"threshold_currency must be an ISO 4217 code (≤3 chars)")
	}
	if expiresIn == 0 {
		expiresIn = 3600
	}
	if expiresIn < 60 || expiresIn > 86400 {
		return nil, newError(CodeInvalidRequest,
			"expires_in_seconds must be within 60..86400")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.requireMaster(ctx, tx, masterID, actorUserID); err != nil {
		return nil, err
	}
	var p Policy
	err = tx.QueryRow(ctx,
		`INSERT INTO client_approval_policies
		   (master_account_id, operation, required_approvals,
		    threshold_amount, threshold_currency, expires_in_seconds,
		    status, created_by)
		 VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,'ACTIVE',$7)
		 ON CONFLICT (master_account_id, operation) DO UPDATE
		    SET required_approvals = EXCLUDED.required_approvals,
		        threshold_amount   = EXCLUDED.threshold_amount,
		        threshold_currency = EXCLUDED.threshold_currency,
		        expires_in_seconds = EXCLUDED.expires_in_seconds,
		        status             = 'ACTIVE',
		        updated_at         = now()
		 RETURNING id, master_account_id, operation, required_approvals,
		           threshold_amount, COALESCE(threshold_currency,''),
		           expires_in_seconds, status, created_by, created_at, updated_at`,
		masterID, string(op), required, threshold, thresholdCCY, expiresIn,
		actorUserID).Scan(&p.ID, &p.MasterAccount, &p.Operation,
		&p.RequiredApprovals, &p.ThresholdAmount, &p.ThresholdCurrency,
		&p.ExpiresInSeconds, &p.Status, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "policy upsert: %v", err)
	}
	if err := audit(ctx, tx, masterID, &actorUserID, nil,
		EvPolicySet, "approval_policy", &p.ID, map[string]any{
			"operation": string(op), "required_approvals": required,
			"threshold_amount": threshold, "threshold_currency": thresholdCCY,
			"expires_in_seconds": expiresIn,
		}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit policy: %v", err)
	}
	return &p, nil
}

// ListPolicies returns every policy (any status) for the master.
func (s *Service) ListPolicies(ctx context.Context, masterID, actorUserID int64) ([]Policy, error) {
	if err := s.requireMaster(ctx, s.pool, masterID, actorUserID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, master_account_id, operation, required_approvals,
		        threshold_amount, COALESCE(threshold_currency,''),
		        expires_in_seconds, status, created_by, created_at, updated_at
		   FROM client_approval_policies
		  WHERE master_account_id=$1 ORDER BY operation`, masterID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "policy list: %v", err)
	}
	defer rows.Close()
	out := []Policy{}
	for rows.Next() {
		var p Policy
		var op string
		if err := rows.Scan(&p.ID, &p.MasterAccount, &op, &p.RequiredApprovals,
			&p.ThresholdAmount, &p.ThresholdCurrency, &p.ExpiresInSeconds,
			&p.Status, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "policy scan: %v", err)
		}
		p.Operation = Operation(op)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DisablePolicy marks a policy DISABLED (soft delete — the row stays
// for audit; new requests reject, open requests still decide).
func (s *Service) DisablePolicy(ctx context.Context, masterID, actorUserID, policyID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.requireMaster(ctx, tx, masterID, actorUserID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`UPDATE client_approval_policies
		    SET status='DISABLED', updated_at=now()
		  WHERE id=$1 AND master_account_id=$2 AND status='ACTIVE'`,
		policyID, masterID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "policy disable: %v", err)
	}
	if tag.RowsAffected() == 0 {
		return errorf(CodeNotFound, "active policy %d not found", policyID)
	}
	if err := audit(ctx, tx, masterID, &actorUserID, nil,
		EvPolicyDisabled, "approval_policy", &policyID, nil); err != nil {
		return errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// M-of-N approval engine
// ---------------------------------------------------------------------------

// CheckApprovalRequired reports whether an ACTIVE policy covers this
// operation+amount — the funding withdrawal flow consults it through
// the WithdrawalApprovalGate seam.
func (s *Service) CheckApprovalRequired(ctx context.Context, masterID int64,
	op Operation, amount decimal.Decimal, currency string) (bool, *Policy, error) {
	var p Policy
	var o string
	err := s.pool.QueryRow(ctx,
		`SELECT id, master_account_id, operation, required_approvals,
		        threshold_amount, COALESCE(threshold_currency,''),
		        expires_in_seconds, status, created_by, created_at, updated_at
		   FROM client_approval_policies
		  WHERE master_account_id=$1 AND operation=$2 AND status='ACTIVE'`,
		masterID, string(op)).Scan(&p.ID, &p.MasterAccount, &o,
		&p.RequiredApprovals, &p.ThresholdAmount, &p.ThresholdCurrency,
		&p.ExpiresInSeconds, &p.Status, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, errorf("INTERNAL_ERROR", "policy read: %v", err)
	}
	p.Operation = Operation(o)
	if !p.Applies(amount, currency) {
		return false, nil, nil
	}
	return true, &p, nil
}

// fingerprint deterministically hashes op + account + payload so a
// resubmitted operation matches its approved request.
func fingerprint(masterID int64, op Operation, payload map[string]any) string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(string(op)))
	h.Write([]byte{0})
	h.Write([]byte(decimal.NewFromInt(masterID).String()))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		b, _ := json.Marshal(payload[k])
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RequestApprovalInput describes one request creation.
type RequestApprovalInput struct {
	MasterAccount int64
	Operation     Operation
	Payload       map[string]any
	RequestedBy   int64 // numeric user id of the initiator
}

// RequestApproval opens a PENDING request under the master's ACTIVE
// policy for the operation. Validation: the initiator must be the
// master owner or a delegate whose role can initiate the operation
// (CLIENT_APPROVER can never initiate — task item 2), and the live
// CLIENT_APPROVER pool must satisfy M.
func (s *Service) RequestApproval(ctx context.Context, in RequestApprovalInput) (*ApprovalRequest, error) {
	if in.MasterAccount == 0 || in.RequestedBy == 0 {
		return nil, newError(CodeUnauthorized, "account and user context required")
	}
	if in.Payload == nil {
		in.Payload = map[string]any{}
	}
	ok, policy, err := s.CheckApprovalRequired(ctx, in.MasterAccount, in.Operation,
		amountFromPayload(in.Payload), currencyFromPayload(in.Payload))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errorf(CodeNotFound,
			"no active %s approval policy on account %d", in.Operation, in.MasterAccount)
	}

	// Initiator capability: master owner initiates anything; a delegate
	// must hold a role that can initiate this operation — approvers
	// never can (anti self-approval is enforced again at Decide).
	identity, err := s.ResolveIdentity(ctx, in.MasterAccount, in.RequestedBy)
	if err != nil {
		return nil, err
	}
	var delegatedID *int64
	if identity != nil {
		delegatedID = &identity.DelegatedUserID
		act := opAction[in.Operation]
		if !roleActions[identity.Role][act] {
			return nil, errorf(CodeInsufficientScope,
				"role %s cannot initiate %s", identity.Role, in.Operation)
		}
	}

	// Eligible approver pool = ACTIVE CLIENT_APPROVER bindings under the
	// master (excluding the requester — self-approval never counts).
	n, err := s.eligibleApprovers(ctx, in.MasterAccount, in.RequestedBy)
	if err != nil {
		return nil, err
	}
	if n < policy.RequiredApprovals {
		return nil, errorf(CodeMultiValidatorRequired,
			"policy requires %d approvers but only %d eligible CLIENT_APPROVER(s) exist",
			policy.RequiredApprovals, n)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	fp := fingerprint(in.MasterAccount, in.Operation, in.Payload)
	payloadJSON, _ := json.Marshal(in.Payload)
	expires := s.now().UTC().Add(time.Duration(policy.ExpiresInSeconds) * time.Second)
	var req ApprovalRequest
	err = tx.QueryRow(ctx,
		`INSERT INTO client_approval_requests
		   (master_account_id, policy_id, operation, payload, fingerprint,
		    requested_by_user, requested_by_delegated, required_approvals,
		    expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 RETURNING id, created_at`,
		in.MasterAccount, policy.ID, string(in.Operation), payloadJSON, fp,
		in.RequestedBy, delegatedID, policy.RequiredApprovals, expires).
		Scan(&req.ID, &req.CreatedAt)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "approval request insert: %v", err)
	}
	req.MasterAccount = in.MasterAccount
	req.PolicyID = policy.ID
	req.Operation = in.Operation
	req.Payload = in.Payload
	req.Fingerprint = fp
	req.RequestedByUser = in.RequestedBy
	req.RequestedByDelegated = delegatedID
	req.Status = ReqPending
	req.RequiredApprovals = policy.RequiredApprovals
	req.ExpiresAt = expires

	if err := audit(ctx, tx, in.MasterAccount, &in.RequestedBy, delegatedID,
		EvApprovalRequested, "approval_request", &req.ID, map[string]any{
			"operation": string(in.Operation), "policy_id": policy.ID,
			"required": policy.RequiredApprovals, "payload": in.Payload,
		}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit approval request: %v", err)
	}
	return &req, nil
}

// eligibleApprovers counts distinct ACTIVE CLIENT_APPROVER principals
// under the master, excluding the requester.
func (s *Service) eligibleApprovers(ctx context.Context, masterID, excludeUserID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(DISTINCT du.user_id)
		   FROM client_delegated_users du
		   JOIN client_role_bindings b
		     ON b.delegated_user_id = du.id AND b.status='ACTIVE'
		    AND (b.expires_at IS NULL OR b.expires_at > now())
		  WHERE du.master_account_id=$1 AND du.status='ACTIVE'
		    AND b.role='CLIENT_APPROVER' AND du.user_id<>$2`,
		masterID, excludeUserID).Scan(&n)
	if err != nil {
		return 0, errorf("INTERNAL_ERROR", "approver count: %v", err)
	}
	return n, nil
}

// approverBinding resolves the approver's ACTIVE CLIENT_APPROVER
// binding under the master; nil identity → not an approver.
func (s *Service) approverBinding(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, masterID, userID int64) (*int64, error) {
	var id *int64
	var v int64
	err := q.QueryRow(ctx,
		`SELECT du.id
		   FROM client_delegated_users du
		   JOIN client_role_bindings b
		     ON b.delegated_user_id = du.id AND b.status='ACTIVE'
		    AND (b.expires_at IS NULL OR b.expires_at > now())
		  WHERE du.master_account_id=$1 AND du.user_id=$2
		    AND du.status='ACTIVE' AND b.role='CLIENT_APPROVER'`,
		masterID, userID).Scan(&v)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "approver binding read: %v", err)
	}
	id = &v
	return id, nil
}

// Decide records one approver vote on a PENDING request: identity
// checks (active CLIENT_APPROVER under the master), anti-self-approval,
// expiry, one-vote-per-approver, then the M-of-N tally — APPROVED at
// count ≥ M, REJECTED on any REJECT (conservative).
func (s *Service) Decide(ctx context.Context, requestID, approverUserID int64,
	approve bool, note string) (*ApprovalRequest, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var req ApprovalRequest
	var payloadRaw []byte
	var op string
	err = tx.QueryRow(ctx,
		`SELECT id, master_account_id, policy_id, operation, payload,
		        fingerprint, requested_by_user, requested_by_delegated,
		        status, approvals_count, required_approvals, expires_at,
		        created_at, decided_at, consumed_at
		   FROM client_approval_requests WHERE id=$1 FOR UPDATE`,
		requestID).Scan(&req.ID, &req.MasterAccount, &req.PolicyID, &op,
		&payloadRaw, &req.Fingerprint, &req.RequestedByUser,
		&req.RequestedByDelegated, &req.Status, &req.ApprovalsCount,
		&req.RequiredApprovals, &req.ExpiresAt, &req.CreatedAt,
		&req.DecidedAt, &req.ConsumedAt)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeNotFound, "approval request %d not found", requestID)
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "request lock: %v", err)
	}
	req.Operation = Operation(op)
	_ = json.Unmarshal(payloadRaw, &req.Payload)

	now := s.now().UTC()
	if req.Status != ReqPending {
		return nil, errorf(CodeInvalidLifecycle,
			"approval request %d is %s — only PENDING can be decided", requestID, req.Status)
	}
	if !now.Before(req.ExpiresAt) {
		if _, err := tx.Exec(ctx,
			`UPDATE client_approval_requests
			    SET status='EXPIRED', decided_at=now() WHERE id=$1`, requestID); err != nil {
			return nil, errorf("INTERNAL_ERROR", "expire request: %v", err)
		}
		_ = audit(ctx, tx, req.MasterAccount, nil, nil,
			EvApprovalExpired, "approval_request", &requestID, nil)
		if err := tx.Commit(ctx); err != nil {
			return nil, errorf("INTERNAL_ERROR", "commit expiry: %v", err)
		}
		return nil, errorf(CodeInvalidLifecycle,
			"approval request %d expired at %s", requestID, req.ExpiresAt.Format(time.RFC3339))
	}

	// Approver gate: must hold an ACTIVE CLIENT_APPROVER binding under
	// the master — the master owner is not an implicit approver (the
	// institution configures its approver pool deliberately).
	delegatedID, err := s.approverBinding(ctx, tx, req.MasterAccount, approverUserID)
	if err != nil {
		return nil, err
	}
	if delegatedID == nil {
		return nil, errorf(CodeUnauthorizedRole,
			"approver %d holds no active CLIENT_APPROVER binding on account %d",
			approverUserID, req.MasterAccount)
	}
	// Anti-self-approval (task item 3): the requester can never decide
	// its own request — not even with an approver hat on.
	if approverUserID == req.RequestedByUser {
		return nil, newError(CodeDualControlViolation,
			"approver cannot decide its own request")
	}

	decision := "APPROVE"
	if !approve {
		decision = "REJECT"
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO client_approval_decisions
		   (request_id, approver_user_id, delegated_user_id, decision, note)
		 VALUES ($1,$2,$3,$4,NULLIF($5,''))`,
		requestID, approverUserID, delegatedID, decision, note); err != nil {
		if isUniqueViolation(err) {
			return nil, errorf(CodeInvalidRequest,
				"approver %d already decided request %d", approverUserID, requestID)
		}
		return nil, errorf("INTERNAL_ERROR", "decision insert: %v", err)
	}

	if !approve {
		req.Status = ReqRejected
		req.DecidedAt = &now
		if _, err := tx.Exec(ctx,
			`UPDATE client_approval_requests
			    SET status='REJECTED', decided_at=now() WHERE id=$1`,
			requestID); err != nil {
			return nil, errorf("INTERNAL_ERROR", "request reject: %v", err)
		}
	} else {
		req.ApprovalsCount++
		if req.ApprovalsCount >= req.RequiredApprovals {
			req.Status = ReqApproved
			req.DecidedAt = &now
		}
		if _, err := tx.Exec(ctx,
			`UPDATE client_approval_requests
			    SET approvals_count=$2, status=$3::varchar,
			        decided_at = CASE WHEN $3::varchar='APPROVED'
			                          THEN now() ELSE decided_at END
			  WHERE id=$1`,
			requestID, req.ApprovalsCount, req.Status); err != nil {
			return nil, errorf("INTERNAL_ERROR", "request tally: %v", err)
		}
	}

	ev := EvApprovalApproved
	if !approve {
		ev = EvApprovalRejected
	}
	if err := audit(ctx, tx, req.MasterAccount, &approverUserID, delegatedID,
		ev, "approval_request", &requestID, map[string]any{
			"decision": decision, "note": note,
			"approvals": req.ApprovalsCount, "required": req.RequiredApprovals,
			"status": req.Status,
		}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit decision: %v", err)
	}
	return &req, nil
}

// ListRequests returns approval requests for the master (any status),
// newest first; status filter "" lists all.
func (s *Service) ListRequests(ctx context.Context, masterID, actorUserID int64,
	status string, limit int) ([]ApprovalRequest, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Visibility: the master owner sees everything; a delegate sees only
	// the requests on its own account.
	identity, err := s.ResolveIdentity(ctx, masterID, actorUserID)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		if err := s.requireMaster(ctx, s.pool, masterID, actorUserID); err != nil {
			return nil, err
		}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, master_account_id, policy_id, operation, payload,
		        fingerprint, requested_by_user, requested_by_delegated,
		        status, approvals_count, required_approvals, expires_at,
		        created_at, decided_at, consumed_at
		   FROM client_approval_requests
		  WHERE master_account_id=$1 AND ($2='' OR status=$2)
		  ORDER BY id DESC LIMIT $3`, masterID, status, limit)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "request list: %v", err)
	}
	defer rows.Close()
	out := []ApprovalRequest{}
	for rows.Next() {
		var r ApprovalRequest
		var payloadRaw []byte
		var op string
		if err := rows.Scan(&r.ID, &r.MasterAccount, &r.PolicyID, &op,
			&payloadRaw, &r.Fingerprint, &r.RequestedByUser,
			&r.RequestedByDelegated, &r.Status, &r.ApprovalsCount,
			&r.RequiredApprovals, &r.ExpiresAt, &r.CreatedAt,
			&r.DecidedAt, &r.ConsumedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "request scan: %v", err)
		}
		r.Operation = Operation(op)
		_ = json.Unmarshal(payloadRaw, &r.Payload)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExpireApprovals lapses PENDING requests past their expiry — wired
// into the gateway's 30s lifecycle sweep.
func (s *Service) ExpireApprovals(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx,
		`UPDATE client_approval_requests
		    SET status='EXPIRED', decided_at=now()
		  WHERE id IN (SELECT id FROM client_approval_requests
		                WHERE status='PENDING' AND expires_at <= now()
		                ORDER BY id LIMIT $1)
		  RETURNING id, master_account_id`, limit)
	if err != nil {
		return 0, errorf("INTERNAL_ERROR", "expire sweep: %v", err)
	}
	type ev struct{ id, master int64 }
	var expired []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.id, &e.master); err != nil {
			rows.Close()
			return 0, errorf("INTERNAL_ERROR", "expire scan: %v", err)
		}
		expired = append(expired, e)
	}
	rows.Close()
	for _, e := range expired {
		if err := audit(ctx, tx, e.master, nil, nil,
			EvApprovalExpired, "approval_request", &e.id, nil); err != nil {
			return 0, errorf("INTERNAL_ERROR", "audit insert: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, errorf("INTERNAL_ERROR", "commit expiry sweep: %v", err)
	}
	return len(expired), nil
}

// ---------------------------------------------------------------------------
// Withdrawal gate (funding.DelegationGate seam)
// ---------------------------------------------------------------------------

// CheckWithdrawal implements the funding.WithdrawalApprovalGate
// contract for the Phase-11 withdrawal create path:
//
//   - no covering ACTIVE WITHDRAWAL policy → approved=true;
//   - an APPROVED, unconsumed request matching the operation fingerprint
//     → consumed, approved=true (the resubmitted withdrawal proceeds);
//   - a live PENDING request for the same payload → approved=false with
//     the existing request id;
//   - otherwise a new PENDING request is recorded and approved=false.
//
// Delegated logins cannot initiate withdrawals at all (no client role
// grants ActionWithdrawal — task item 2) — a delegated caller is
// FORBIDDEN before any request exists.
func (s *Service) CheckWithdrawal(ctx context.Context, accountID, userID int64,
	currency string, amount decimal.Decimal, destination string) (bool, int64, error) {

	identity, err := s.ResolveIdentity(ctx, accountID, userID)
	if err != nil {
		return false, 0, err
	}
	if identity != nil {
		return false, 0, errorf(CodeInsufficientScope,
			"delegated login (role %s) cannot initiate withdrawals", identity.Role)
	}

	payload := map[string]any{
		"currency":    currency,
		"amount":      amount.String(),
		"destination": destination,
	}
	required, _, err := s.CheckApprovalRequired(ctx, accountID, OpWithdrawal, amount, currency)
	if err != nil {
		return false, 0, err
	}
	if !required {
		return true, 0, nil
	}
	fp := fingerprint(accountID, OpWithdrawal, payload)

	// Match the newest request with this payload fingerprint: an
	// APPROVED one is consumed (single-use); a live PENDING one is
	// replayed; anything else (REJECTED/EXPIRED/CONSUMED) forces a fresh
	// request.
	var reqID int64
	var status string
	err = s.pool.QueryRow(ctx,
		`SELECT id, status FROM client_approval_requests
		  WHERE master_account_id=$1 AND operation='WITHDRAWAL' AND fingerprint=$2
		  ORDER BY id DESC LIMIT 1`, accountID, fp).Scan(&reqID, &status)
	if err != nil && err != pgx.ErrNoRows {
		return false, 0, errorf("INTERNAL_ERROR", "approval match read: %v", err)
	}
	switch {
	case err == nil && status == ReqApproved:
		tag, uerr := s.pool.Exec(ctx,
			`UPDATE client_approval_requests
			    SET status='CONSUMED', consumed_at=now()
			  WHERE id=$1 AND status='APPROVED'`, reqID)
		if uerr != nil {
			return false, 0, errorf("INTERNAL_ERROR", "consume request %d: %v", reqID, uerr)
		}
		if tag.RowsAffected() == 0 {
			return false, reqID, nil // a racing consumer won — fail closed
		}
		_ = s.auditOutside(ctx, accountID, &userID, nil,
			EvApprovalConsumed, "approval_request", &reqID, payload)
		return true, reqID, nil
	case err == nil && status == ReqPending:
		var live bool
		if qerr := s.pool.QueryRow(ctx,
			`SELECT expires_at > now() FROM client_approval_requests WHERE id=$1`,
			reqID).Scan(&live); qerr == nil && live {
			return false, reqID, nil
		}
		// fell through: pending row is expired — create a fresh request
	}
	req, err := s.RequestApproval(ctx, RequestApprovalInput{
		MasterAccount: accountID, Operation: OpWithdrawal,
		Payload: payload, RequestedBy: userID,
	})
	if err != nil {
		return false, 0, err
	}
	return false, req.ID, nil
}

// amountFromPayload / currencyFromPayload pull the threshold-match
// fields out of an approval payload (empty → zero/"" which matches a
// threshold-less policy; with a threshold set, a missing amount simply
// never reaches the floor).
func amountFromPayload(p map[string]any) decimal.Decimal {
	if v, ok := p["amount"]; ok {
		if s, ok := v.(string); ok {
			if d, err := decimal.NewFromString(s); err == nil {
				return d
			}
		}
	}
	return decimal.Zero
}

func currencyFromPayload(p map[string]any) string {
	if v, ok := p["currency"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// isUniqueViolation reports the PG 23505 unique-violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
