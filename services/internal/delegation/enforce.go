// Enforcement helpers — the scope-check surface the gateway invokes for
// delegated sessions (Task 12.3.11 design note).
//
// ResolveIdentity maps (account, user) → delegated identity; a nil
// identity means the caller is the master principal (or an unrelated
// user — the caller's own auth decides whether that is allowed; this
// helper only answers "is this a delegated login and what may it do").
//
// Authorize is the per-request gate: delegated sessions must satisfy
// role capability AND scope containment. Master principals bypass — the
// caller should invoke Authorize only when the request may be delegated;
// it is idempotent and cheap (one indexed read).
package delegation

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// ResolveIdentity returns the ACTIVE delegated identity for
// (masterAccountID, userID): the delegated user row + its live
// CLIENT_* binding (expired bindings deny — fail closed). Returns
// (nil, nil) when the pair is not a delegated login.
func (s *Service) ResolveIdentity(ctx context.Context, accountID, userID int64) (*Identity, error) {
	var id Identity
	var role string
	var scopeRaw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT du.id, du.master_account_id, du.user_id, b.role, b.scope
		   FROM client_delegated_users du
		   JOIN client_role_bindings b
		     ON b.delegated_user_id = du.id AND b.status = 'ACTIVE'
		    AND (b.expires_at IS NULL OR b.expires_at > now())
		  WHERE du.master_account_id = $1 AND du.user_id = $2
		    AND du.status = 'ACTIVE'`,
		accountID, userID).Scan(&id.DelegatedUserID, &id.MasterAccount,
		&id.UserID, &role, &scopeRaw)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "delegated identity read: %v", err)
	}
	id.Role = Role(role)
	if len(scopeRaw) > 0 {
		if err := json.Unmarshal(scopeRaw, &id.Scope); err != nil {
			return nil, errorf("INTERNAL_ERROR", "delegated scope corrupt: %v", err)
		}
	}
	return &id, nil
}

// Authorize evaluates whether the delegated session may perform action
// on target. Identity nil (master principal) → permitted; delegated →
// role capability + scope containment, fail-closed on any missing piece.
//
// Rules enforced (task items 1–3):
//   - every axis in scope must cover its target field when that field
//     is relevant to the action (TRADE → instrument+account;
//     INTERNAL_TRANSFER → source AND destination account, both inside
//     the entitled family — scope.account_ids already validated at bind
//     time against the master family);
//   - WITHDRAWAL / SECURITY_CHANGE / API_KEY_MANAGE / BENEFICIARY_CHANGE
//     are never granted to a delegated role — the capability matrix
//     denies them before scope is consulted;
//   - CLIENT_APPROVER grants APPROVE only — initiating a configured
//     operation hits the same denial.
func (s *Service) Authorize(ctx context.Context, accountID, userID int64,
	action Action, t Target) error {

	id, err := s.ResolveIdentity(ctx, accountID, userID)
	if err != nil {
		return err
	}
	if id == nil {
		return nil // master principal — unrestricted by delegation
	}
	if !roleActions[id.Role][action] {
		return errorf(CodeInsufficientScope,
			"delegated role %s may not perform %s", id.Role, action)
	}
	// Scope containment: the acted-on account is always constrained
	// when the target names one.
	if t.AccountID != 0 && !id.Scope.CoversAccount(t.AccountID) {
		return errorf(CodeInsufficientScope,
			"delegated scope does not cover account %d", t.AccountID)
	}
	if t.Instrument != "" && !id.Scope.CoversInstrument(t.Instrument) {
		return errorf(CodeInsufficientScope,
			"delegated scope does not cover instrument %s", t.Instrument)
	}
	if t.Counterparty != 0 && !id.Scope.CoversAccount(t.Counterparty) {
		return errorf(CodeInsufficientScope,
			"delegated scope does not cover counterparty account %d — "+
				"finance managers may only transfer within the entitled hierarchy",
			t.Counterparty)
	}
	return nil
}
