package accounts

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Roles allowed to place/lift a legal hold (Task 5.3.12 item 2:
// "Compliance Officer+"). Names match the §8.2 RBAC vocabulary; Phase-07
// Task 7.3.1 replaces the resolver stub with the real role service.
const (
	RoleComplianceOfficer = "Compliance Officer"
	RoleSuperAdmin        = "Super Admin"
	RoleRiskManager       = "Risk Manager"
)

// RoleResolver resolves an admin user's role. Phase-05 stub seam:
// production wiring lands with Phase-07 RBAC (Task 7.3.1). A nil
// resolver fails closed — every privileged call is rejected.
type RoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// AdminActor is the authenticated administrator performing a freeze
// action: ActorUserID initiates, ApproverUserID is the second
// (four-eyes) approver. Dual control: approver must differ from actor
// and be non-zero — the Phase-05 enforceable stub for Task 7.3.2's
// pending-approval workflow.
type AdminActor struct {
	UserID     int64
	Role       string
	ApproverID int64
	ClientIP   string // audit column (handlers fill from the request)
}

// FreezeService implements the FROZEN legal-hold lifecycle
// (Task 5.3.12): freeze blocks trading and withdrawals through
// AssertMutable; every transition appends an account_freeze_events row
// and a mirrored admin_audit_log row inside one transaction.
type FreezeService struct {
	pool     *pgxpool.Pool
	resolver RoleResolver
}

// NewFreezeService builds the service. resolver may be nil (fail-closed:
// all privileged calls reject with UNAUTHORIZED_ROLE until Phase-07
// wires the real role lookup).
func NewFreezeService(pool *pgxpool.Pool, resolver RoleResolver) *FreezeService {
	return &FreezeService{pool: pool, resolver: resolver}
}

// Freeze transitions accountID to FROZEN. Only ACTIVE or SUSPENDED
// accounts can be frozen; the transition is a guarded UPDATE inside a
// SERIALIZABLE-read committed transaction so a racing unfreeze or close
// cannot interleave.
func (s *FreezeService) Freeze(ctx context.Context, actor AdminActor, accountID int64, reason string, clientIP string) error {
	return s.transition(ctx, actor, accountID, reason, clientIP,
		"FREEZE", StatusFrozen, []AccountStatus{StatusActive, StatusSuspended})
}

// Unfreeze releases the legal hold; the account returns to ACTIVE.
func (s *FreezeService) Unfreeze(ctx context.Context, actor AdminActor, accountID int64, reason string, clientIP string) error {
	return s.transition(ctx, actor, accountID, reason, clientIP,
		"UNFREEZE", StatusActive, []AccountStatus{StatusFrozen})
}

// transition is the shared freeze/unfreeze state machine.
func (s *FreezeService) transition(ctx context.Context, actor AdminActor, accountID int64,
	reason, clientIP, action string, target AccountStatus, from []AccountStatus) error {

	if err := s.checkRole(ctx, actor); err != nil {
		return err
	}
	if actor.ApproverID == 0 || actor.ApproverID == actor.UserID {
		return newError(CodeDualControlRequired,
			"freeze/unfreeze requires a distinct second approver (dual control)")
	}
	if reason == "" {
		return newError(CodeInvalidRequest, "freeze/unfreeze requires a reason")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prev string
	err = tx.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&prev)
	if err == pgx.ErrNoRows {
		return errorf(CodeNotFound, "account %d not found", accountID)
	}
	if err != nil {
		return errorf("INTERNAL_ERROR", "lock account %d: %v", accountID, err)
	}
	allowed := false
	for _, f := range from {
		if prev == string(f) {
			allowed = true
		}
	}
	if !allowed {
		return errorf(CodeInvalidRequest,
			"cannot %s account %d in status %s", action, accountID, prev)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET status = $2, updated_at = now() WHERE id = $1`,
		accountID, string(target)); err != nil {
		return errorf("INTERNAL_ERROR", "%s account %d: %v", action, accountID, err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO account_freeze_events
		   (account_id, action, reason, initiated_by, approved_by, prev_status, new_status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		accountID, action, reason, actor.UserID, actor.ApproverID, prev, string(target)); err != nil {
		return errorf("INTERNAL_ERROR", "freeze event insert: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_audit_log
		   (admin_user_id, action, target_type, target_id, before_state, after_state, ip_address)
		 VALUES ($1, $2, 'account', $3,
		         jsonb_build_object('status', $4::text),
		         jsonb_build_object('status', $5::text, 'reason', $6::text, 'approved_by', $7::bigint),
		         NULLIF($8, '')::inet)`,
		actor.UserID, "account."+strings.ToLower(action), accountID, prev, string(target),
		reason, actor.ApproverID, clientIP); err != nil {
		return errorf("INTERNAL_ERROR", "admin audit insert: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return errorf("INTERNAL_ERROR", "commit %s: %v", action, err)
	}
	return nil
}

// checkRole enforces the "Compliance Officer+" stub (Task 5.3.12 item 2):
// the resolver must return Compliance Officer or Super Admin.
func (s *FreezeService) checkRole(ctx context.Context, actor AdminActor) error {
	if actor.UserID == 0 {
		return newError(CodeUnauthorized, "admin identity required")
	}
	if s.resolver == nil {
		return newError(CodeUnauthorizedRole,
			"role resolver not configured (Phase-07 RBAC stub boundary)")
	}
	role, err := s.resolver(ctx, actor.UserID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "role lookup: %v", err)
	}
	if role != RoleComplianceOfficer && role != RoleSuperAdmin {
		return errorf(CodeUnauthorizedRole,
			"freeze/unfreeze requires Compliance Officer or Super Admin (got %q)", role)
	}
	return nil
}

// AssertMutable rejects with a coded error unless accountID is ACTIVE.
// This is the gate Task 5.3.12 item 1 requires on every trading and
// withdrawal path ("FROZEN: no trading, no withdrawals"): order entry,
// close-all, transfers and funding handlers call it before mutating.
func (s *FreezeService) AssertMutable(ctx context.Context, accountID int64) error {
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1`, accountID).Scan(&status)
	if err == pgx.ErrNoRows {
		return errorf(CodeNotFound, "account %d not found", accountID)
	}
	if err != nil {
		return errorf("INTERNAL_ERROR", "read account status: %v", err)
	}
	if status != string(StatusActive) {
		return errorf(statusCode(status), "account %d is %s", accountID, status)
	}
	return nil
}

// StatusOf returns the account's stored status.
func (s *FreezeService) StatusOf(ctx context.Context, accountID int64) (AccountStatus, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1`, accountID).Scan(&status)
	if err == pgx.ErrNoRows {
		return "", errorf(CodeNotFound, "account %d not found", accountID)
	}
	if err != nil {
		return "", errorf("INTERNAL_ERROR", "read account status: %v", err)
	}
	return AccountStatus(status), nil
}

// FreezeEvent is one legal-hold audit row.
type FreezeEvent struct {
	ID          int64     `json:"id"`
	AccountID   int64     `json:"account_id"`
	Action      string    `json:"action"`
	Reason      string    `json:"reason"`
	InitiatedBy int64     `json:"initiated_by"`
	ApprovedBy  int64     `json:"approved_by"`
	PrevStatus  string    `json:"prev_status"`
	NewStatus   string    `json:"new_status"`
	CreatedAt   time.Time `json:"created_at"`
}

// FreezeHistory returns the legal-hold audit trail for an account,
// newest first.
func (s *FreezeService) FreezeHistory(ctx context.Context, accountID int64) ([]FreezeEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, account_id, action, reason, initiated_by, approved_by,
		        prev_status, new_status, created_at
		   FROM account_freeze_events
		  WHERE account_id = $1 ORDER BY id DESC`, accountID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "freeze history: %v", err)
	}
	defer rows.Close()
	out := []FreezeEvent{}
	for rows.Next() {
		var e FreezeEvent
		if err := rows.Scan(&e.ID, &e.AccountID, &e.Action, &e.Reason,
			&e.InitiatedBy, &e.ApprovedBy, &e.PrevStatus, &e.NewStatus, &e.CreatedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "scan freeze event: %v", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
