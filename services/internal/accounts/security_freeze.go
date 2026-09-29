package accounts

// security_freeze.go — Phase-12 Task 12.3.12 part 2 freeze seam.
//
// FreezeService's dual-control lifecycle (admin actor + distinct second
// approver) cannot serve an automated credential-clone response: there
// is no human actor, and waiting for approval would leave a cloned key
// usable. SecurityFreezeService is the machine counterpart: it suspends
// the user and freezes every ACTIVE/SUSPENDED account they own in one
// transaction, writing an account_freeze_events row per account with
// metadata {source:"system", trigger:"webauthn_clone"} so the audit
// trail distinguishes it from a legal hold. initiated_by/approved_by
// carry the subject user id — the NOT NULL columns are satisfied while
// metadata marks that no human approved.

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SecurityFreezeService implements auth.SecurityFreezer for automated
// security responses (WebAuthn credential-clone detection).
type SecurityFreezeService struct {
	pool *pgxpool.Pool
}

func NewSecurityFreezeService(pool *pgxpool.Pool) *SecurityFreezeService {
	return &SecurityFreezeService{pool: pool}
}

// FreezeUserAccounts suspends the user and freezes all of their
// mutable accounts in one transaction — the clone-detection response
// required by spec §12.6. Idempotent: accounts already FROZEN/CLOSED
// are skipped; a user with no mutable accounts still gets the
// users.status suspension. A transaction failure propagates (the
// caller logs and still rejects the ceremony — fail closed).
func (s *SecurityFreezeService) FreezeUserAccounts(ctx context.Context, userID int64, reason string) error {
	if userID <= 0 {
		return newError(CodeInvalidRequest, "freeze requires a user id")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errorf("INTERNAL_ERROR", "security freeze tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Suspend the user — users.status has no FROZEN value; SUSPENDED is
	// the account-security suspension (distinct from CLOSED offboarding).
	if _, err := tx.Exec(ctx,
		`UPDATE users SET status = 'SUSPENDED', updated_at = now()
		  WHERE id = $1 AND status = 'ACTIVE'`, userID); err != nil {
		return errorf("INTERNAL_ERROR", "security freeze user %d: %v", userID, err)
	}

	// Freeze every mutable account the user owns.
	rows, err := tx.Query(ctx,
		`SELECT id, status FROM accounts
		  WHERE user_id = $1 AND status IN ('ACTIVE','SUSPENDED')
		  FOR UPDATE`, userID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "security freeze list %d: %v", userID, err)
	}
	type acct struct {
		id     int64
		status string
	}
	var targets []acct
	for rows.Next() {
		var a acct
		if err := rows.Scan(&a.id, &a.status); err != nil {
			rows.Close()
			return errorf("INTERNAL_ERROR", "security freeze scan: %v", err)
		}
		targets = append(targets, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return errorf("INTERNAL_ERROR", "security freeze list %d: %v", userID, err)
	}

	for _, a := range targets {
		if _, err := tx.Exec(ctx,
			`UPDATE accounts SET status = 'FROZEN', updated_at = now()
			  WHERE id = $1`, a.id); err != nil {
			return errorf("INTERNAL_ERROR", "security freeze account %d: %v", a.id, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO account_freeze_events
			   (account_id, action, reason, initiated_by, approved_by,
			    prev_status, new_status, metadata)
			 VALUES ($1, 'FREEZE', $2, $3, $3, $4, 'FROZEN',
			         jsonb_build_object('source','system','trigger','webauthn_clone'))`,
			a.id, reason, userID, a.status); err != nil {
			return errorf("INTERNAL_ERROR", "security freeze event %d: %v", a.id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return errorf("INTERNAL_ERROR", "security freeze commit: %v", err)
	}
	return nil
}
