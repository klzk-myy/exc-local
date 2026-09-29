// Task 12.3.10 — unfreeze-request record for self-frozen accounts.
//
// POST /api/v1/account/unfreeze-request opens a durable re-verification
// request (migration 201): the account owner submits an optional
// government-ID document reference and liveness-check artifact reference
// produced by the Phase-14 KYC document pipeline, plus a free-text note.
//
// Honest boundary: this file owns the request RECORD and its
// SUBMITTED state. The actual identity re-verification — document
// verification, liveness adjudication and the mandatory new-2FA setup —
// is the Phase-14 admin workflow; it transitions SUBMITTED →
// DOCS_VERIFIED → UNFROZEN (which is also what lifts accounts.status).
// One open request per account (partial unique index); resubmission
// while a request is open replays the existing row — idempotent.
package accounts

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Unfreeze request statuses (migration 201 CHECK constraint).
const (
	UnfreezeSubmitted    = "SUBMITTED"
	UnfreezeDocsVerified = "DOCS_VERIFIED"
	UnfreezeDone         = "UNFROZEN"
	UnfreezeRejected     = "REJECTED"
	UnfreezeCancelled    = "CANCELLED"
)

// UnfreezeRequest is one re-verification request row.
type UnfreezeRequest struct {
	ID            int64      `json:"id"`
	AccountID     int64      `json:"account_id"`
	UserID        int64      `json:"user_id"`
	Status        string     `json:"status"`
	IDDocumentRef string     `json:"id_document_ref,omitempty"`
	LivenessRef   string     `json:"liveness_ref,omitempty"`
	Note          string     `json:"note,omitempty"`
	DecidedBy     *int64     `json:"decided_by,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	Replayed      bool       `json:"replayed,omitempty"` // true = existing open request returned
}

// UnfreezeService owns the request state machine's client-facing edge.
type UnfreezeService struct {
	pool *pgxpool.Pool
}

// NewUnfreezeService wires the service.
func NewUnfreezeService(pool *pgxpool.Pool) *UnfreezeService {
	return &UnfreezeService{pool: pool}
}

// Request opens (or replays) the account's open unfreeze request.
//
// Preconditions, all fail-closed:
//   - accountID must exist and be FROZEN;
//   - the freeze must be a SELF_FREEZE (a legal-hold freeze is released
//     only through the Compliance Officer admin path — a client request
//     cannot preempt it);
//   - the caller must be the account owner.
func (s *UnfreezeService) Request(ctx context.Context, accountID, userID int64,
	idDocRef, livenessRef, note string) (*UnfreezeRequest, error) {

	if accountID == 0 || userID == 0 {
		return nil, newError(CodeUnauthorized, "account and user context required")
	}
	if len(idDocRef) > 255 || len(livenessRef) > 255 {
		return nil, newError(CodeInvalidRequest,
			"document references exceed 255 characters")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var owner int64
	err = tx.QueryRow(ctx,
		`SELECT status, user_id FROM accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&status, &owner)
	if err == pgx.ErrNoRows {
		return nil, errorf(CodeNotFound, "account %d not found", accountID)
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "lock account %d: %v", accountID, err)
	}
	if owner != userID {
		return nil, newError(CodeForbidden,
			"unfreeze requests are owned by the account holder")
	}
	if status != string(StatusFrozen) {
		return nil, errorf(CodeInvalidRequest,
			"account %d is %s — unfreeze-request applies to FROZEN accounts",
			accountID, status)
	}
	// Only a SELF_FREEZE is reversible through this client path. Any
	// other freeze reason is a legal/compliance hold — reject so a
	// client can never confuse the two regimes.
	var lastReason string
	err = tx.QueryRow(ctx,
		`SELECT reason FROM account_freeze_events
		  WHERE account_id = $1 ORDER BY id DESC LIMIT 1`,
		accountID).Scan(&lastReason)
	if err == pgx.ErrNoRows {
		return nil, errorf("INTERNAL_ERROR",
			"account %d is FROZEN with no freeze event — inconsistent state", accountID)
	}
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "freeze event read: %v", err)
	}
	if lastReason != SelfFreezeReason {
		return nil, errorf(CodeForbidden,
			"account frozen under %q — release requires the Compliance Officer path",
			lastReason)
	}

	// Idempotent replay: an open request satisfies a resubmission (the
	// client may attach updated refs — they are merged into the row).
	var out UnfreezeRequest
	err = tx.QueryRow(ctx,
		`SELECT id, account_id, user_id, status, COALESCE(id_document_ref,''),
		        COALESCE(liveness_ref,''), COALESCE(note,''), decided_by,
		        decided_at, created_at
		   FROM unfreeze_requests
		  WHERE account_id = $1 AND status IN ('SUBMITTED','DOCS_VERIFIED')
		  ORDER BY id DESC LIMIT 1`, accountID).Scan(
		&out.ID, &out.AccountID, &out.UserID, &out.Status, &out.IDDocumentRef,
		&out.LivenessRef, &out.Note, &out.DecidedBy, &out.DecidedAt, &out.CreatedAt)
	if err != nil && err != pgx.ErrNoRows {
		return nil, errorf("INTERNAL_ERROR", "open request read: %v", err)
	}
	if err == nil {
		// Merge newly supplied references into the open request.
		if idDocRef != "" || livenessRef != "" || note != "" {
			if _, uerr := tx.Exec(ctx,
				`UPDATE unfreeze_requests SET
				    id_document_ref = COALESCE(NULLIF($2,''), id_document_ref),
				    liveness_ref    = COALESCE(NULLIF($3,''), liveness_ref),
				    note            = COALESCE(NULLIF($4,''), note),
				    updated_at      = now()
				  WHERE id = $1`, out.ID, idDocRef, livenessRef, note); uerr != nil {
				return nil, errorf("INTERNAL_ERROR", "open request merge: %v", uerr)
			}
			if idDocRef != "" {
				out.IDDocumentRef = idDocRef
			}
			if livenessRef != "" {
				out.LivenessRef = livenessRef
			}
			if note != "" {
				out.Note = note
			}
		}
		out.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return nil, errorf("INTERNAL_ERROR", "commit unfreeze replay: %v", err)
		}
		return &out, nil
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO unfreeze_requests
		   (account_id, user_id, id_document_ref, liveness_ref, note)
		 VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''))
		 RETURNING id, created_at`,
		accountID, userID, idDocRef, livenessRef, note).Scan(&out.ID, &out.CreatedAt)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "unfreeze request insert: %v", err)
	}
	out.AccountID, out.UserID, out.Status = accountID, userID, UnfreezeSubmitted
	out.IDDocumentRef, out.LivenessRef, out.Note = idDocRef, livenessRef, note

	if err := tx.Commit(ctx); err != nil {
		return nil, errorf("INTERNAL_ERROR", "commit unfreeze request: %v", err)
	}
	return &out, nil
}

// List returns the account's unfreeze requests, newest first.
func (s *UnfreezeService) List(ctx context.Context, accountID int64) ([]UnfreezeRequest, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, account_id, user_id, status, COALESCE(id_document_ref,''),
		        COALESCE(liveness_ref,''), COALESCE(note,''), decided_by,
		        decided_at, created_at
		   FROM unfreeze_requests WHERE account_id = $1 ORDER BY id DESC`,
		accountID)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "unfreeze request list: %v", err)
	}
	defer rows.Close()
	out := []UnfreezeRequest{}
	for rows.Next() {
		var r UnfreezeRequest
		if err := rows.Scan(&r.ID, &r.AccountID, &r.UserID, &r.Status,
			&r.IDDocumentRef, &r.LivenessRef, &r.Note, &r.DecidedBy,
			&r.DecidedAt, &r.CreatedAt); err != nil {
			return nil, errorf("INTERNAL_ERROR", "unfreeze request scan: %v", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
