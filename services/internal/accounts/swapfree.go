// swapfree.go — Swap-Free (Islamic) Verification Lifecycle
// (Phase-14 Task 14.3.15; spec §12.8, §5.41, §24 #373; migration 095).
//
// Lifecycle: POST /api/v1/account/swap-free/request (attestation ref —
// a kyc_documents row owned by the account, the Task 12.3.4 document
// pipeline) → swapfree_verifications PENDING + accounts.swapfree_status
// PENDING → Compliance Officer approve (→ APPROVED + account VERIFIED)
// or reject (→ REJECTED + account STANDARD); revocation reuses the same
// Compliance-Officer decision path (→ REVOKED + account REVOKED).
//
// Enforcement is prospective-only: VERIFIED accrues zero Tom-Next
// financing — the Phase-03 Task 3.3.19/3.3.23 rollover consumes
// accounts.swapfree_status directly (loadPositions) and IsSwapFreeVerified
// exposes the same fact to any other consumer. REVOKED resumes standard
// accrual from the next roll — never retroactively, no back-billing.
//
// Abuse guard: more than AbuseWindowMax VERIFIED→REVOKED transitions per
// trailing 12 months auto-flag the account for compliance-hold review via
// the HoldPlacer seam (Task 14.3.10's compliance_holds table — the
// durable record lands here; the richer review workflow owns the case).
package accounts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
)

// Swap-free verification statuses (migration 095 CHECK) and the mirrored
// accounts.swapfree_status vocabulary (§5.2/§12.8).
const (
	SwapPending  = "PENDING"
	SwapApproved = "APPROVED" // verification row only — account shows VERIFIED
	SwapRejected = "REJECTED" // verification row only — account shows STANDARD
	SwapRevoked  = "REVOKED"

	AcctSwapStandard = "STANDARD"
	AcctSwapPending  = "PENDING"
	AcctSwapVerified = "VERIFIED"
	AcctSwapRevoked  = "REVOKED"
)

// AbuseWindowMax is the revocation ceiling: >AbuseWindowMax
// VERIFIED→REVOKED transitions inside AbuseWindow months flag the
// account for compliance-hold review.
const (
	AbuseWindowMax    = 2
	AbuseWindowMonths = 12
)

// SwapfreeVerification is one swapfree_verifications row.
type SwapfreeVerification struct {
	ID              int64      `json:"id"`
	VerificationRef string     `json:"verification_ref"`
	AccountID       int64      `json:"account_id"`
	AttestationRef  string     `json:"attestation_ref"`
	Status          string     `json:"status"`
	RequestedAt     time.Time  `json:"requested_at"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
	Verifier        *int64     `json:"verifier,omitempty"`
	DecisionNote    string     `json:"decision_note,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// HoldPlacer is the Task 14.3.10 compliance-hold seam — the canonical
// review-flag channel for the swap-free abuse guard.
type HoldPlacer interface {
	// PlaceHold opens a compliance-hold review row; called inside the
	// revocation transaction so the flag can never be lost.
	PlaceHold(ctx context.Context, tx pgx.Tx, accountID int64,
		triggerSource, reason, evidenceRef string, placedBy int64) (int64, error)
}

// PgxHoldPlacer is the default HoldPlacer: writes compliance_holds
// (migration 215, Phase-14 Task 14.3.10). The richer hold workflow
// (dispositions, SLA breach sweeps) owns the row afterwards.
type PgxHoldPlacer struct{}

// PlaceHold implements HoldPlacer — machine trigger 'UNUSUAL_ACTIVITY',
// 24h SLA (the 4h tier is sanctions-hit specific per Task 14.3.10).
func (PgxHoldPlacer) PlaceHold(ctx context.Context, tx pgx.Tx, accountID int64,
	triggerSource, reason, evidenceRef string, placedBy int64) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO compliance_holds
		    (hold_id, account_id, trigger_source, reason, evidence_ref,
		     sla_deadline, placed_by)
		VALUES ($1, $2, $3, $4, NULLIF($5,''),
		        now() + interval '24 hours', $6)
		RETURNING id`,
		"chold_"+randHex(12), accountID, triggerSource, reason, evidenceRef,
		placedBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("compliance hold insert: %w", err)
	}
	return id, nil
}

// randHex renders n random bytes as lowercase hex.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

// SwapfreeService owns the lifecycle state machine.
type SwapfreeService struct {
	pool     *pgxpool.Pool
	resolver RoleResolver
	holds    HoldPlacer
	now      func() time.Time
}

// NewSwapfreeService wires the service; holds nil disables the abuse
// flag (transitions still complete — the durable flag lands when the
// Task 14.3.10 schema exists).
func NewSwapfreeService(pool *pgxpool.Pool, resolver RoleResolver, holds HoldPlacer) *SwapfreeService {
	return &SwapfreeService{pool: pool, resolver: resolver, holds: holds, now: time.Now}
}

// SetClockForTest overrides the clock; tests only.
func (s *SwapfreeService) SetClockForTest(now func() time.Time) { s.now = now }

// swapfreeDecideRoles may decide/revoke verifications (route pins
// RoleComplianceOfficer; Super Admin inherits per §8.2).
var swapfreeDecideRoles = map[string]bool{
	RoleComplianceOfficer: true,
	RoleSuperAdmin:        true,
}

func (s *SwapfreeService) requireRole(ctx context.Context, adminUserID int64) error {
	if s.resolver == nil {
		return newError(CodeUnauthorizedRole,
			"role resolver not configured — swap-free decisions rejected")
	}
	role, err := s.resolver(ctx, adminUserID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "role lookup: %v", err)
	}
	if !swapfreeDecideRoles[role] {
		return newError(CodeUnauthorizedRole,
			"swap-free decisions require Compliance Officer or Super Admin")
	}
	return nil
}

// Request opens a PENDING verification. attestationRef must resolve to a
// kyc_documents row owned by the account (Task 12.3.4 pipeline) — a bare
// string never attests. Only one live (PENDING|APPROVED) request per
// account; a REVOKED account may re-request (fresh review, abuse guard
// counts the prior revocations regardless).
func (s *SwapfreeService) Request(ctx context.Context, accountID int64, attestationRef string) (*SwapfreeVerification, error) {
	attestationRef = strings.TrimSpace(attestationRef)
	if attestationRef == "" || len(attestationRef) > 128 {
		return nil, newError(CodeInvalidRequest, "attestation_ref required")
	}
	// Attestation must resolve to the account's own document.
	var docOK bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM kyc_documents
		      WHERE account_id = $1 AND (id::text = $2 OR file_url = $2))`,
		accountID, attestationRef).Scan(&docOK); err != nil {
		return nil, errorf("INTERNAL_ERROR", "attestation lookup: %v", err)
	}
	if !docOK {
		return nil, errorf(CodeInvalidRequest,
			"attestation_ref does not resolve to a document on this account")
	}
	var acctStatus, swapStatus string
	err := s.pool.QueryRow(ctx,
		`SELECT status::text, swapfree_status FROM accounts WHERE id = $1`,
		accountID).Scan(&acctStatus, &swapStatus)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeAccountNotFound, "account %d not found", accountID)
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "account read: %v", err)
	}
	if acctStatus == "CLOSED" {
		return nil, errorf(CodeInvalidRequest, "account %d is closed", accountID)
	}
	switch swapStatus {
	case AcctSwapPending:
		return nil, newError(CodeInvalidRequest,
			"a swap-free request is already pending review")
	case AcctSwapVerified:
		return nil, newError(CodeInvalidRequest, "account is already swap-free verified")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := scanVerification(tx.QueryRow(ctx, `
		INSERT INTO swapfree_verifications
		    (verification_ref, account_id, attestation_ref)
		VALUES ($1, $2, $3)
		RETURNING id, verification_ref, account_id, attestation_ref, status,
		          requested_at, decided_at, verifier, COALESCE(decision_note,''),
		          created_at, updated_at`,
		"swv_"+randHex(12), accountID, attestationRef))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, newError(CodeInvalidRequest,
				"a swap-free request is already pending or approved")
		}
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE accounts SET swapfree_status='PENDING', updated_at=now()
		 WHERE id=$1 AND swapfree_status NOT IN ('PENDING','VERIFIED')`,
		accountID); err != nil {
		return nil, errorf("INTERNAL_ERROR", "account status update: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit: %v", err)
	}
	return v, nil
}

func scanVerification(row pgx.Row) (*SwapfreeVerification, error) {
	var v SwapfreeVerification
	err := row.Scan(&v.ID, &v.VerificationRef, &v.AccountID, &v.AttestationRef,
		&v.Status, &v.RequestedAt, &v.DecidedAt, &v.Verifier,
		&v.DecisionNote, &v.CreatedAt, &v.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "verification scan: %v", err)
	}
	return &v, nil
}

// Approve confirms a PENDING request: row → APPROVED, account → VERIFIED
// (prospective — current positions stop accruing from the next roll).
func (s *SwapfreeService) Approve(ctx context.Context, actor AdminActor, verificationID int64) (*SwapfreeVerification, error) {
	return s.decide(ctx, actor, verificationID, SwapApproved, AcctSwapVerified, "")
}

// Reject declines a PENDING request: row → REJECTED, account → STANDARD.
func (s *SwapfreeService) Reject(ctx context.Context, actor AdminActor, verificationID int64, reason string) (*SwapfreeVerification, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, newError(CodeInvalidRequest, "reject requires a reason")
	}
	return s.decide(ctx, actor, verificationID, SwapRejected, AcctSwapStandard, reason)
}

// Revoke withdraws an APPROVED verification: row → REVOKED, account →
// REVOKED (standard accrual resumes prospectively, no back-billing).
// Cross-window abuse (>2 revocations / 12 months) auto-flags the account
// for compliance-hold review inside the same transaction.
func (s *SwapfreeService) Revoke(ctx context.Context, actor AdminActor, verificationID int64, reason string) (*SwapfreeVerification, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, newError(CodeInvalidRequest, "revocation requires a reason")
	}
	return s.decide(ctx, actor, verificationID, SwapRevoked, AcctSwapRevoked, reason)
}

// decide is the shared Compliance-Officer transition. For REVOKED the
// source must be APPROVED (a verified state); for the others PENDING.
func (s *SwapfreeService) decide(ctx context.Context, actor AdminActor,
	verificationID int64, to, acctStatus, note string) (*SwapfreeVerification, error) {
	if err := s.requireRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	from := SwapPending
	if to == SwapRevoked {
		from = SwapApproved
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := scanVerification(tx.QueryRow(ctx, `
		SELECT id, verification_ref, account_id, attestation_ref, status,
		       requested_at, decided_at, verifier, COALESCE(decision_note,''),
		       created_at, updated_at
		  FROM swapfree_verifications WHERE id = $1 FOR UPDATE`, verificationID))
	if err != nil {
		return nil, err
	}
	if before == nil {
		return nil, errorf(CodeNotFound,
			"swap-free verification %d not found", verificationID)
	}
	if before.Status != from {
		return nil, errorf(CodeInvalidRequest,
			"verification %d is %s — %s requires %s",
			verificationID, before.Status, to, from)
	}
	v, err := scanVerification(tx.QueryRow(ctx, `
		UPDATE swapfree_verifications
		   SET status=$2, decided_at=now(), verifier=$3,
		       decision_note=NULLIF($4,''), updated_at=now()
		 WHERE id=$1
		RETURNING id, verification_ref, account_id, attestation_ref, status,
		          requested_at, decided_at, verifier, COALESCE(decision_note,''),
		          created_at, updated_at`,
		verificationID, to, actor.UserID, note))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE accounts SET swapfree_status=$2, updated_at=now()
		 WHERE id=$1`, before.AccountID, acctStatus); err != nil {
		return nil, errorf("INTERNAL_ERROR", "account status update: %v", err)
	}

	// Abuse guard: >2 VERIFIED→REVOKED transitions in the trailing 12
	// months (this one included) auto-flags a compliance-hold review.
	if to == SwapRevoked {
		var revocations int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM swapfree_verifications
			 WHERE account_id=$1 AND status='REVOKED'
			   AND decided_at > now() - interval '12 months'`,
			before.AccountID).Scan(&revocations); err != nil {
			return nil, errorf("INTERNAL_ERROR", "abuse count: %v", err)
		}
		if revocations > AbuseWindowMax && s.holds != nil {
			if _, err := s.holds.PlaceHold(ctx, tx, before.AccountID,
				"UNUSUAL_ACTIVITY",
				fmt.Sprintf("swap-free abuse: %d verified→revoked transitions in 12 months",
					revocations),
				before.VerificationRef, actor.UserID); err != nil {
				return nil, errorf("INTERNAL_ERROR",
					"compliance-hold flag failed — revocation aborted (fail closed): %v", err)
			}
		}
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "swapfree." + strings.ToLower(to),
		TargetType:  "account",
		TargetID:    &before.AccountID,
		BeforeState: before,
		AfterState:  v,
		IPAddress:   actor.ClientIP,
	}); err != nil {
		return nil, errorf("INTERNAL_ERROR", "audit log: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit: %v", err)
	}
	return v, nil
}

// IsSwapFreeVerified is the financing-treatment seam the Phase-03 Task
// 3.3.19/3.3.23 rollover accrual path consumes — VERIFIED ⇒ zero
// Tom-Next financing. (The production rollover reads
// accounts.swapfree_status directly in its position scan; this method
// is the documented consumer seam for any other reader.)
func (s *SwapfreeService) IsSwapFreeVerified(ctx context.Context, accountID int64) (bool, error) {
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT swapfree_status FROM accounts WHERE id=$1`,
		accountID).Scan(&status); err != nil {
		if err == pgx.ErrNoRows {
			return false, errorf(CodeAccountNotFound, "account %d not found", accountID)
		}
		return false, errorf("INTERNAL_ERROR", "swapfree read: %v", err)
	}
	return status == AcctSwapVerified, nil
}

// Status is the client-facing view: mirror status + the newest
// verification row (nil when never requested).
func (s *SwapfreeService) Status(ctx context.Context, accountID int64) (map[string]any, error) {
	var swapStatus string
	err := s.pool.QueryRow(ctx,
		`SELECT swapfree_status FROM accounts WHERE id=$1`,
		accountID).Scan(&swapStatus)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeAccountNotFound, "account %d not found", accountID)
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "account read: %v", err)
	}
	latest, err := scanVerification(s.pool.QueryRow(ctx, `
		SELECT id, verification_ref, account_id, attestation_ref, status,
		       requested_at, decided_at, verifier, COALESCE(decision_note,''),
		       created_at, updated_at
		  FROM swapfree_verifications WHERE account_id=$1
		  ORDER BY id DESC LIMIT 1`, accountID))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"account_id":      accountID,
		"swapfree_status": swapStatus,
		"prospective":     true,
		"back_billing":    false,
		"latest":          latest,
	}, nil
}

// List serves the Compliance verification queue (Phase-07 surface
// consumes it); status filter optional, newest-first.
func (s *SwapfreeService) List(ctx context.Context, actor AdminActor, status string, limit int) ([]SwapfreeVerification, error) {
	if err := s.requireRole(ctx, actor.UserID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, verification_ref, account_id, attestation_ref, status,
	             requested_at, decided_at, verifier, COALESCE(decision_note,''),
	             created_at, updated_at
	        FROM swapfree_verifications`
	args := []any{}
	if status != "" {
		args = append(args, strings.ToUpper(status))
		q += " WHERE status = $1"
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "verification list: %v", err)
	}
	defer rows.Close()
	out := []SwapfreeVerification{}
	for rows.Next() {
		var v SwapfreeVerification
		if err := rows.Scan(&v.ID, &v.VerificationRef, &v.AccountID,
			&v.AttestationRef, &v.Status, &v.RequestedAt, &v.DecidedAt,
			&v.Verifier, &v.DecisionNote, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "verification scan: %v", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
