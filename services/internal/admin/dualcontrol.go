// Task 7.3.2 — dual control (four-eyes), spec §8.2.
//
// The §8.2 sensitive-operation set (kill-switch, balance adjustment,
// manual liquidation, withdrawal override, admin role change, fee-tier
// change, release-suspended-account, deploy-to-production) requires two
// distinct approvers inside a 15-minute window. This file implements the
// durable pending-request workflow on admin_dual_control_requests
// (migration 090): the maker creates a PENDING request; a distinct,
// role-eligible approver confirms (Approve) or turns it down (Reject);
// the window lapse marks it EXPIRED.
//
// Relationship to the Phase-05 synchronous seams (FreezeService,
// ManualLiquidationService): those endpoints take approver_id in the
// call itself — two principals, one request — which is the §8.2 contract
// for ops that cannot wait on a queue. This queue is the maker-checker
// workflow for ops that can (role grants, break-glass, Phase-15 listing
// maintenance etc. plug in via RegisterExecutor). Both paths log both
// principal ids to admin_audit_log.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// ApprovalWindow is the §8.2 four-eyes window — the DB CHECK pins the
// same 15-minute bound.
const ApprovalWindow = 15 * time.Minute

// Sensitive-operation names (§8.2 dual-control set) plus the operations
// this package drives through the queue.
const (
	OpKillSwitch            = "kill-switch"
	OpBalanceAdjustment     = "balance-adjustment"
	OpManualLiquidation     = "manual-liquidation"
	OpWithdrawalOverride    = "withdrawal-override"
	OpAdminRoleChange       = "admin-role-change" // grant AND revoke
	OpFeeTierChange         = "fee-tier-change"
	OpReleaseSuspendedAcct  = "release-suspended-account"
	OpDeployToProduction    = "deploy-to-production"
	OpBreakGlass            = "break-glass"
	OpInstrumentMaintenance = "instrument-maintenance" // Phase-15 maker-checker
	OpCircuitBreakerReset   = "circuit-breaker-reset"  // Phase-13 Task 13.3.1/13.3.9
	OpAPIKeyExpiryExtend    = "api-key-expiry-extend"  // Phase-13 Task 13.3.8 privilege-grace grant
)

// Request statuses.
const (
	ReqPending  = "PENDING"
	ReqApproved = "APPROVED" // confirmed, executor pending/absent
	ReqExecuted = "EXECUTED" // confirmed AND the registered executor ran in-tx
	ReqRejected = "REJECTED"
	ReqExpired  = "EXPIRED"
)

// SensitiveOperation reports whether op is in the §8.2 dual-control set.
func SensitiveOperation(op string) bool {
	switch op {
	case OpKillSwitch, OpBalanceAdjustment, OpManualLiquidation,
		OpWithdrawalOverride, OpAdminRoleChange, OpFeeTierChange,
		OpReleaseSuspendedAcct, OpDeployToProduction, OpBreakGlass,
		OpAPIKeyExpiryExtend, OpCircuitBreakerReset:
		return true
	}
	return false
}

// DualControlRequest is one admin_dual_control_requests row.
type DualControlRequest struct {
	ID           int64           `json:"id"`
	Operation    string          `json:"operation"`
	TargetType   string          `json:"target_type"`
	TargetID     string          `json:"target_id"`
	Payload      json.RawMessage `json:"payload"`
	RequiredRole string          `json:"required_role"`
	RequestedBy  int64           `json:"requested_by"`
	ApprovedBy   *int64          `json:"approved_by,omitempty"`
	Status       string          `json:"status"`
	Reason       string          `json:"reason,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
}

// Executor applies the approved operation inside the approval
// transaction — the mutation and the four-eyes record commit or fail
// together. Register via RegisterExecutor; a nil executor leaves the
// request APPROVED for an external driver.
type Executor func(ctx context.Context, tx pgx.Tx, req *DualControlRequest) error

// DualControlService drives the pending queue.
type DualControlService struct {
	pool      *pgxpool.Pool
	store     *Store
	executors map[string]Executor
	now       func() time.Time
}

// NewDualControlService wires the service. store is required — approver
// role eligibility reads live bindings.
func NewDualControlService(pool *pgxpool.Pool, store *Store) *DualControlService {
	return &DualControlService{
		pool:      pool,
		store:     store,
		executors: map[string]Executor{},
		now:       time.Now,
	}
}

// RegisterExecutor attaches the executor for op. Startup-time wiring —
// not goroutine-guarded.
func (s *DualControlService) RegisterExecutor(op string, fn Executor) {
	s.executors[op] = fn
}

// SetClockForTest overrides the clock; tests only.
func (s *DualControlService) SetClockForTest(now func() time.Time) {
	s.now = now
}

// SubmitInput is the maker side of a four-eyes request.
type SubmitInput struct {
	Operation    string
	TargetType   string
	TargetID     string
	Payload      any    // marshaled to jsonb
	RequiredRole string // §8.2 role the approver must hold
	RequestedBy  int64
	Reason       string
	ClientIP     string
}

// Submit creates a PENDING request. The requester must hold an ACTIVE
// binding satisfying RequiredRole — the maker's own authority is checked
// at submit, not just at approve (spec §8.2: both principals are
// accountable).
func (s *DualControlService) Submit(ctx context.Context, in SubmitInput) (*DualControlRequest, error) {
	if in.RequestedBy <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "maker identity required")
	}
	if in.Operation == "" || in.TargetType == "" {
		return nil, excerrors.New("INVALID_REQUEST", "operation and target_type are required")
	}
	if !ValidRole(in.RequiredRole) {
		return nil, excerrors.New("INVALID_REQUEST",
			"required_role must be one of the six §8.2 roles")
	}
	// Maker eligibility: at least one ACTIVE binding satisfying the
	// required role. Skip for the break-glass solo path — its granter is
	// validated by the lifecycle service itself.
	makerRole, err := s.store.StrongestRole(ctx, in.RequestedBy)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "maker role lookup", err)
	}
	if makerRole == "" || !Permits(in.RequiredRole, makerRole) {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("operation %s requires a binding satisfying %s",
				in.Operation, in.RequiredRole))
	}
	payload, err := json.Marshal(in.Payload)
	if err != nil {
		return nil, excerrors.New("INVALID_REQUEST", "payload not JSON-marshalable")
	}
	if payload == nil {
		payload = []byte("{}")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var req DualControlRequest
	err = tx.QueryRow(ctx, `
		INSERT INTO admin_dual_control_requests
		    (operation, target_type, target_id, payload, required_role,
		     requested_by, reason, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now() + interval '15 minutes')
		RETURNING id, operation, target_type, target_id, payload,
		          required_role, requested_by, status,
		          COALESCE(reason,''), created_at, expires_at`,
		in.Operation, in.TargetType, in.TargetID, payload,
		in.RequiredRole, in.RequestedBy, in.Reason).
		Scan(&req.ID, &req.Operation, &req.TargetType, &req.TargetID,
			&req.Payload, &req.RequiredRole, &req.RequestedBy, &req.Status,
			&req.Reason, &req.CreatedAt, &req.ExpiresAt)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert request", err)
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: in.RequestedBy,
		Action:      "dual_control.submit",
		TargetType:  "dual_control_request",
		TargetID:    &req.ID,
		AfterState: map[string]any{
			"operation": in.Operation, "target_type": in.TargetType,
			"target_id": in.TargetID, "required_role": in.RequiredRole,
			"expires_at": req.ExpiresAt,
		},
		IPAddress: in.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return &req, nil
}

// Approve confirms a PENDING request: approver must differ from the
// maker (DUAL_CONTROL_VIOLATION), hold a satisfying binding, and act
// inside the 15-minute window. A registered executor runs in the same
// transaction → EXECUTED; without one the row lands APPROVED.
func (s *DualControlService) Approve(ctx context.Context, requestID, approverID int64, clientIP string) (*DualControlRequest, error) {
	return s.decide(ctx, requestID, approverID, true, clientIP)
}

// Reject turns down a PENDING request — same distinctness + eligibility
// rules as Approve.
func (s *DualControlService) Reject(ctx context.Context, requestID, approverID int64, clientIP string) (*DualControlRequest, error) {
	return s.decide(ctx, requestID, approverID, false, clientIP)
}

// Cancel withdraws the maker's own PENDING request.
func (s *DualControlService) Cancel(ctx context.Context, requestID, requesterID int64, clientIP string) (*DualControlRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	req, err := s.lockRequest(ctx, tx, requestID)
	if err != nil {
		return nil, err
	}
	if req.RequestedBy != requesterID {
		return nil, excerrors.New("FORBIDDEN", "only the maker may cancel a pending request")
	}
	if req.Status != ReqPending {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("request %d already %s", requestID, req.Status))
	}
	return s.settle(ctx, tx, req, requesterID, ReqRejected, "CANCELLED", clientIP)
}

// decide is the shared approve/reject path.
func (s *DualControlService) decide(ctx context.Context, requestID, approverID int64, approve bool, clientIP string) (*DualControlRequest, error) {
	if approverID <= 0 {
		return nil, excerrors.New("UNAUTHORIZED", "approver identity required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	req, err := s.lockRequest(ctx, tx, requestID)
	if err != nil {
		return nil, err
	}
	switch req.Status {
	case ReqPending:
	case ReqExpired:
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"approval window elapsed — the operation must be re-submitted")
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("request %d already %s", requestID, req.Status))
	}
	// Four-eyes: the approver must be a *distinct* principal.
	if approverID == req.RequestedBy {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"the initiating principal cannot approve their own request")
	}
	// Window: expired ⇒ mark and refuse (§8.2 15-minute bound).
	if !s.now().Before(req.ExpiresAt) {
		if _, err := tx.Exec(ctx,
			`UPDATE admin_dual_control_requests SET status='EXPIRED' WHERE id=$1`,
			requestID); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "expire request", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "commit expiry", err)
		}
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"approval window elapsed — the operation must be re-submitted")
	}
	// Role eligibility on live bindings (satisfying binding, not merely
	// any binding — the approver must be able to carry the operation).
	approverRole, err := s.store.StrongestRole(ctx, approverID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "approver role lookup", err)
	}
	if approverRole == "" || !Permits(req.RequiredRole, approverRole) {
		return nil, excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("approver must hold a binding satisfying %s", req.RequiredRole))
	}

	status := ReqRejected
	action := "dual_control.reject"
	if approve {
		status = ReqApproved
		action = "dual_control.approve"
		// Executor runs inside the approval tx — the mutation and its
		// four-eyes record are atomic.
		if ex, ok := s.executors[req.Operation]; ok {
			// Executor sees the deciding approver id (settle stamps it on
			// the row afterwards — the tx makes both atomic).
			req.ApprovedBy = &approverID
			if err := ex(ctx, tx, req); err != nil {
				return nil, err
			}
			status = ReqExecuted
		}
	}
	return s.settle(ctx, tx, req, approverID, status, action, clientIP)
}

// lockRequest SELECT ... FOR UPDATEs the row.
func (s *DualControlService) lockRequest(ctx context.Context, tx pgx.Tx, id int64) (*DualControlRequest, error) {
	var req DualControlRequest
	err := tx.QueryRow(ctx, `
		SELECT id, operation, target_type, target_id, payload, required_role,
		       requested_by, approved_by, status, COALESCE(reason,''),
		       created_at, expires_at, decided_at
		  FROM admin_dual_control_requests WHERE id = $1 FOR UPDATE`, id).
		Scan(&req.ID, &req.Operation, &req.TargetType, &req.TargetID,
			&req.Payload, &req.RequiredRole, &req.RequestedBy, &req.ApprovedBy,
			&req.Status, &req.Reason, &req.CreatedAt, &req.ExpiresAt,
			&req.DecidedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("dual-control request %d not found", id))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "lock request", err)
	}
	return &req, nil
}

// settle writes the terminal state + audit row and commits.
func (s *DualControlService) settle(ctx context.Context, tx pgx.Tx, req *DualControlRequest,
	deciderID int64, status, action, clientIP string) (*DualControlRequest, error) {

	err := tx.QueryRow(ctx, `
		UPDATE admin_dual_control_requests
		   SET status = $2, approved_by = $3, decided_at = now()
		 WHERE id = $1
		RETURNING id, operation, target_type, target_id, payload,
		          required_role, requested_by, approved_by, status,
		          COALESCE(reason,''), created_at, expires_at, decided_at`,
		req.ID, status, deciderID).
		Scan(&req.ID, &req.Operation, &req.TargetType, &req.TargetID,
			&req.Payload, &req.RequiredRole, &req.RequestedBy, &req.ApprovedBy,
			&req.Status, &req.Reason, &req.CreatedAt, &req.ExpiresAt,
			&req.DecidedAt)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "settle request", err)
	}
	// Audit row carries BOTH principal ids — the §8.2/§24 #26 maker-checker
	// evidence contract.
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: deciderID,
		Action:      action,
		TargetType:  "dual_control_request",
		TargetID:    &req.ID,
		BeforeState: map[string]any{"status": "PENDING"},
		AfterState: map[string]any{
			"status": status, "operation": req.Operation,
			"requested_by": req.RequestedBy, "decided_by": deciderID,
		},
		IPAddress: clientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	return req, nil
}

// Get loads one request.
func (s *DualControlService) Get(ctx context.Context, id int64) (*DualControlRequest, error) {
	var req DualControlRequest
	err := s.pool.QueryRow(ctx, `
		SELECT id, operation, target_type, target_id, payload, required_role,
		       requested_by, approved_by, status, COALESCE(reason,''),
		       created_at, expires_at, decided_at
		  FROM admin_dual_control_requests WHERE id = $1`, id).
		Scan(&req.ID, &req.Operation, &req.TargetType, &req.TargetID,
			&req.Payload, &req.RequiredRole, &req.RequestedBy, &req.ApprovedBy,
			&req.Status, &req.Reason, &req.CreatedAt, &req.ExpiresAt,
			&req.DecidedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("dual-control request %d not found", id))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "read request", err)
	}
	return &req, nil
}

// List returns requests, optionally filtered by status, newest first.
func (s *DualControlService) List(ctx context.Context, status string, limit int) ([]DualControlRequest, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, operation, target_type, target_id, payload, required_role,
	             requested_by, approved_by, status, COALESCE(reason,''),
	             created_at, expires_at, decided_at
	        FROM admin_dual_control_requests`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += " WHERE status = $1"
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list requests", err)
	}
	defer rows.Close()
	out := []DualControlRequest{}
	for rows.Next() {
		var req DualControlRequest
		if err := rows.Scan(&req.ID, &req.Operation, &req.TargetType,
			&req.TargetID, &req.Payload, &req.RequiredRole, &req.RequestedBy,
			&req.ApprovedBy, &req.Status, &req.Reason, &req.CreatedAt,
			&req.ExpiresAt, &req.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

// ExpireDue lapses PENDING requests past their window. Returns the count.
func (s *DualControlService) ExpireDue(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE admin_dual_control_requests
		   SET status = 'EXPIRED', decided_at = now()
		 WHERE status = 'PENDING' AND expires_at <= now()`)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "expire pending", err)
	}
	return tag.RowsAffected(), nil
}
