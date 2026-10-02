// PgStore implementations for the Phase-14 seams — KYC lifecycle
// (Task 14.3.4) and MiFID II categorization/appropriateness (Task
// 14.3.7). Every mutating method runs its state change + the
// admin_audit_log row + the audit_hash_chain link inside ONE
// serializable transaction (admin.Log), retried on the spec §5.40
// retry classes, so an audited transition can never lose its anchor.
package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"exchange/internal/admin"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Shared retry ladder (same SQLSTATE set as admin.LogAuto — spec §5.40)
// ---------------------------------------------------------------------------

var lifecycleRetryable = map[string]bool{"23505": true, "40001": true, "40P01": true}

func lifecycleIsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return lifecycleRetryable[pgErr.Code]
	}
	return false
}

// inTx runs fn inside a serializable transaction, retrying the whole
// transaction on retryable conflicts (max 3 attempts).
func (s *PgStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return fmt.Errorf("compliance: begin tx: %w", err)
		}
		if err = fn(tx); err == nil {
			if err = tx.Commit(ctx); err == nil {
				return nil
			}
		}
		_ = tx.Rollback(ctx)
		lastErr = err
		if !lifecycleIsRetryable(err) {
			return err
		}
	}
	return excerrors.Wrap("TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
		"compliance transaction failed after 3 attempts", lastErr)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// SubmissionByID loads one submission row or nil.
func (s *PgStore) SubmissionByID(ctx context.Context, submissionID int64) (*Submission, error) {
	var sub Submission
	var jur, reason *string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, account_id, requested_tier, status, jurisdiction,
		       risk_score, submitted_at, sla_due_at, verified_at,
		       reverify_due_at, reviewed_at, reject_reason, appeal_of,
		       created_at, updated_at
		  FROM kyc_submissions
		 WHERE id=$1`, submissionID).Scan(
		&sub.ID, &sub.AccountID, &sub.RequestedTier, &sub.Status, &jur,
		&sub.RiskScore, &sub.SubmittedAt, &sub.SLADueAt, &sub.VerifiedAt,
		&sub.ReverifyDueAt, &sub.ReviewedAt, &reason, &sub.AppealOf,
		&sub.CreatedAt, &sub.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: submission %d: %w", submissionID, err)
	}
	if jur != nil {
		sub.Jurisdiction = *jur
	}
	if reason != nil {
		sub.RejectReason = *reason
	}
	return &sub, nil
}

// PendingSubmissions lists review-queue rows — PENDING_REVIEW or
// UNDER_REVIEW, oldest SLA first (Task 14.3.4 officer queue).
func (s *PgStore) PendingSubmissions(ctx context.Context, limit int) ([]Submission, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, account_id, requested_tier, status, jurisdiction,
		       risk_score, submitted_at, sla_due_at, verified_at,
		       reverify_due_at, reviewed_at, reject_reason, appeal_of,
		       created_at, updated_at
		  FROM kyc_submissions
		 WHERE status IN ('PENDING_REVIEW','UNDER_REVIEW')
		 ORDER BY sla_due_at ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("compliance: pending submissions: %w", err)
	}
	defer rows.Close()
	out := []Submission{}
	for rows.Next() {
		var sub Submission
		var jur, reason *string
		if err := rows.Scan(
			&sub.ID, &sub.AccountID, &sub.RequestedTier, &sub.Status, &jur,
			&sub.RiskScore, &sub.SubmittedAt, &sub.SLADueAt, &sub.VerifiedAt,
			&sub.ReverifyDueAt, &sub.ReviewedAt, &reason, &sub.AppealOf,
			&sub.CreatedAt, &sub.UpdatedAt); err != nil {
			return nil, fmt.Errorf("compliance: pending scan: %w", err)
		}
		if jur != nil {
			sub.Jurisdiction = *jur
		}
		if reason != nil {
			sub.RejectReason = *reason
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// AccountUserID resolves accounts.user_id (audit attribution).
func (s *PgStore) AccountUserID(ctx context.Context, accountID int64) (int64, error) {
	var uid int64
	err := s.Pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id=$1`, accountID).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("compliance: account user: %w", err)
	}
	return uid, nil
}

// ClientCategory reads accounts.client_category + accounts.nbp
// (migration 042). Missing account → ("", false, nil) — the callers
// fail closed on the empty category.
func (s *PgStore) ClientCategory(ctx context.Context, accountID int64) (string, bool, error) {
	var cat string
	var nbp bool
	err := s.Pool.QueryRow(ctx,
		`SELECT client_category::text, nbp FROM accounts WHERE id=$1`,
		accountID).Scan(&cat, &nbp)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("compliance: client category: %w", err)
	}
	return cat, nbp, nil
}

// ---------------------------------------------------------------------------
// Task 14.3.4 — review decision (approve/reject), one atomic tx
// ---------------------------------------------------------------------------

// assignedTier maps a requested tier to the accounts.kyc_tier enum
// value the approval writes. INSTITUTIONAL has no enum value (migration
// 003) — it lands as T2 plus the ECP category (migration-203 boundary
// note).
func assignedTier(requested string) string {
	switch requested {
	case TierT1:
		return TierT1
	case TierT2, TierInstitutional:
		return TierT2
	}
	return ""
}

// tierRank orders the enum for max-tier resolution — an approval only
// RAISES the standing tier: a T1 decision landing on a T2 account keeps
// T2 (the standing tier reflects the highest verification achieved; the
// re-verification sweep is the only path that lowers it).
func tierRank(t string) int {
	switch t {
	case TierT1:
		return 1
	case TierT2:
		return 2
	}
	return 0
}

func maxTier(a, b string) string {
	if tierRank(a) > tierRank(b) {
		return a
	}
	return b
}

// DecideSubmissionTx implements LifecycleStore.DecideSubmissionTx.
func (s *PgStore) DecideSubmissionTx(ctx context.Context, p DecisionTx) (*DecisionResult, error) {
	var res *DecisionResult
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Lock the submission row — the decision is single-writer.
		var (
			sub      Submission
			jur      *string
			curTier  string
			curCat   string
			curNBP   bool
			acctUser int64
		)
		err := tx.QueryRow(ctx, `
			SELECT id, account_id, requested_tier, status, jurisdiction,
			       risk_score, submitted_at, sla_due_at
			  FROM kyc_submissions WHERE id=$1 FOR UPDATE`, p.SubmissionID).Scan(
			&sub.ID, &sub.AccountID, &sub.RequestedTier, &sub.Status, &jur,
			&sub.RiskScore, &sub.SubmittedAt, &sub.SLADueAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("kyc submission %d not found", p.SubmissionID))
		}
		if err != nil {
			return fmt.Errorf("compliance: lock submission %d: %w", p.SubmissionID, err)
		}
		if sub.Status != SubPendingReview && sub.Status != SubUnderReview {
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("submission %d already decided (status %s)", p.SubmissionID, sub.Status))
		}
		// Account snapshot (before-state for the audit row).
		if err := tx.QueryRow(ctx, `
			SELECT user_id, kyc_tier::text, client_category::text, nbp
			  FROM accounts WHERE id=$1 FOR UPDATE`, sub.AccountID).Scan(
			&acctUser, &curTier, &curCat, &curNBP); err != nil {
			return fmt.Errorf("compliance: lock account %d: %w", sub.AccountID, err)
		}

		before := map[string]any{
			"submission_status": sub.Status,
			"kyc_tier":          curTier,
			"client_category":   curCat,
			"nbp":               curNBP,
		}
		after := map[string]any{}
		res = &DecisionResult{
			SubmissionID:  sub.ID,
			AccountID:     sub.AccountID,
			RequestedTier: sub.RequestedTier,
			ReviewedAt:    p.Now,
		}
		action := "kyc.reject"
		if p.Approve {
			action = "kyc.approve"
			tier := assignedTier(sub.RequestedTier)
			if tier == "" {
				return excerrors.New("INVALID_REQUEST",
					fmt.Sprintf("requested_tier %q is not approvable", sub.RequestedTier))
			}
			var reverify *time.Time
			if p.ReverifyMonths > 0 {
				d := p.Now.AddDate(0, p.ReverifyMonths, 0)
				reverify = &d
			}
			if _, err := tx.Exec(ctx, `
				UPDATE kyc_submissions
				   SET status='APPROVED', verified_at=$2, reviewed_at=$2,
				       reviewer_id=$3, reverify_due_at=$4, updated_at=$2
				 WHERE id=$1`, p.SubmissionID, p.Now, p.ReviewerID, reverify); err != nil {
				return fmt.Errorf("compliance: approve submission %d: %w", p.SubmissionID, err)
			}
			// Document rows flip to the verdict.
			if _, err := tx.Exec(ctx, `
				UPDATE kyc_documents
				   SET status='APPROVED', verified_at=$2, verified_by=$3
				 WHERE submission_id=$1 AND status='PENDING'`,
				p.SubmissionID, p.Now, p.ReviewerID); err != nil {
				return fmt.Errorf("compliance: approve documents: %w", err)
			}
			// The single write path to accounts.kyc_tier — raise-only:
			// a lower-tier decision never lowers a standing higher tier
			// (the sweep is the sole downgrade path). INSTITUTIONAL
			// also lands client_category ECP + drops nbp (migration-203
			// boundary: no INSTITUTIONAL kyc_tier enum value exists).
			effective := maxTier(curTier, tier)
			if sub.RequestedTier == TierInstitutional {
				if _, err := tx.Exec(ctx, `
					UPDATE accounts
					   SET kyc_tier=$2, client_category='ELIGIBLE_COUNTERPARTY',
					       nbp=false, updated_at=$3
					 WHERE id=$1`, sub.AccountID, effective, p.Now); err != nil {
					return fmt.Errorf("compliance: institutional assign: %w", err)
				}
				res.ClientCategory = string(CategoryECP)
				after["client_category"] = string(CategoryECP)
				after["nbp"] = false
			} else {
				if _, err := tx.Exec(ctx, `
					UPDATE accounts SET kyc_tier=$2, updated_at=$3
					 WHERE id=$1`, sub.AccountID, effective, p.Now); err != nil {
					return fmt.Errorf("compliance: tier assign: %w", err)
				}
			}
			res.Decision = SubApproved
			res.AssignedTier = effective
			res.ReverifyDueAt = reverify
			after["submission_status"] = SubApproved
			after["kyc_tier"] = effective
			after["reverify_due_at"] = reverify
		} else {
			if strings.TrimSpace(p.Reason) == "" {
				return excerrors.New("INVALID_REQUEST", "reject reason is required")
			}
			if _, err := tx.Exec(ctx, `
				UPDATE kyc_submissions
				   SET status='REJECTED', reviewed_at=$2, reviewer_id=$3,
				       reject_reason=$4, updated_at=$2
				 WHERE id=$1`, p.SubmissionID, p.Now, p.ReviewerID, p.Reason); err != nil {
				return fmt.Errorf("compliance: reject submission %d: %w", p.SubmissionID, err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE kyc_documents
				   SET status='REJECTED', verified_at=$2, verified_by=$3
				 WHERE submission_id=$1 AND status='PENDING'`,
				p.SubmissionID, p.Now, p.ReviewerID); err != nil {
				return fmt.Errorf("compliance: reject documents: %w", err)
			}
			res.Decision = SubRejected
			after["submission_status"] = SubRejected
			after["reject_reason"] = p.Reason
		}

		_, chainSeq, err := admin.Log(ctx, tx, admin.AuditEntry{
			AdminUserID: p.ReviewerID,
			Action:      action,
			TargetType:  "kyc_submission",
			TargetID:    &sub.ID,
			BeforeState: before,
			AfterState:  after,
			IPAddress:   p.ClientIP,
		})
		if err != nil {
			return err
		}
		res.AuditSeq = chainSeq
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Task 14.3.4 — re-verification sweep feed + atomic downgrade
// ---------------------------------------------------------------------------

// OverdueReverifications implements LifecycleStore.OverdueReverifications:
// the account's LATEST APPROVED submission past reverify_due_at while the
// account still sits at T2 (a re-verified account's newer approval wins
// the max(id) and its future due date excludes the row; an already-
// downgraded account fails the kyc_tier predicate — idempotent feed).
func (s *PgStore) OverdueReverifications(ctx context.Context, now time.Time, limit int) ([]OverdueReverify, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT s.id, s.account_id, a.user_id, s.requested_tier, s.reverify_due_at
		  FROM kyc_submissions s
		  JOIN accounts a ON a.id = s.account_id
		 WHERE s.status = 'APPROVED'
		   AND s.reverify_due_at IS NOT NULL
		   AND s.reverify_due_at < $1
		   AND a.kyc_tier = 'T2'
		   AND s.id = (SELECT max(s2.id) FROM kyc_submissions s2
		                WHERE s2.account_id = s.account_id
		                  AND s2.status = 'APPROVED')
		 ORDER BY s.reverify_due_at
		 LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("compliance: overdue reverify feed: %w", err)
	}
	defer rows.Close()
	var out []OverdueReverify
	for rows.Next() {
		var r OverdueReverify
		if err := rows.Scan(&r.SubmissionID, &r.AccountID, &r.UserID,
			&r.RequestedTier, &r.ReverifyDueAt); err != nil {
			return nil, fmt.Errorf("compliance: overdue reverify scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DowngradeReverifyTx implements LifecycleStore.DowngradeReverifyTx.
// applied=false means the account no longer sits at T2 — a concurrent
// transition resolved it; nothing is written (idempotent).
func (s *PgStore) DowngradeReverifyTx(ctx context.Context, p DowngradeTx) (bool, error) {
	applied := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE accounts SET kyc_tier='T1', updated_at=$2
			 WHERE id=$1 AND kyc_tier='T2'`, p.AccountID, p.Now)
		if err != nil {
			return fmt.Errorf("compliance: downgrade account %d: %w", p.AccountID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil // not at T2 anymore — nothing to do
		}
		after := map[string]any{
			"kyc_tier":       TierT1,
			"submission":     SubExpired,
			"downgrade":      "auto_reverify_overdue",
			"requested_tier": p.RequestedTier,
		}
		before := map[string]any{
			"kyc_tier":   TierT2,
			"submission": SubApproved,
		}
		// Institutional lapse: the ECP designation derived from the KYB
		// reverts to RETAIL and nbp re-arms (fail-closed — never leave an
		// unverified institutional posture standing). Conditional on the
		// account actually sitting at ECP — a Compliance Officer may have
		// re-assigned the category after the approval; only the lapsed
		// KYB-derived designation is reverted.
		if p.RequestedTier == TierInstitutional {
			tag, err := tx.Exec(ctx, `
				UPDATE accounts
				   SET client_category='RETAIL', nbp=true
				 WHERE id=$1 AND client_category='ELIGIBLE_COUNTERPARTY'`,
				p.AccountID)
			if err != nil {
				return fmt.Errorf("compliance: institutional downgrade: %w", err)
			}
			if tag.RowsAffected() > 0 {
				before["client_category"] = string(CategoryECP)
				before["nbp"] = false
				after["client_category"] = string(CategoryRetail)
				after["nbp"] = true
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE kyc_submissions
			   SET status='EXPIRED', updated_at=$2
			 WHERE id=$1 AND status='APPROVED'`, p.SubmissionID, p.Now); err != nil {
			return fmt.Errorf("compliance: expire submission %d: %w", p.SubmissionID, err)
		}
		// System-driven transition: the affected principal carries the
		// audit attribution (admin_audit_log.admin_user_id cannot be 0 —
		// the lifecycle.go convention for sweep actors).
		if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
			AdminUserID: p.UserID,
			Action:      "kyc.reverify_downgrade",
			TargetType:  "account",
			TargetID:    &p.AccountID,
			BeforeState: before,
			AfterState:  after,
		}); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// ---------------------------------------------------------------------------
// Task 14.3.7 — appropriateness assessments + category write path
// ---------------------------------------------------------------------------

// LatestAssessment implements CategoryStore.LatestAssessment — the
// newest row for (account, class), any outcome, or nil.
func (s *PgStore) LatestAssessment(ctx context.Context, accountID int64, class string) (*Assessment, error) {
	var a Assessment
	err := s.Pool.QueryRow(ctx, `
		SELECT assessment_id, account_id, instrument_class, outcome, score,
		       answers_json, assessed_at, expires_at
		  FROM appropriateness_assessments
		 WHERE account_id=$1 AND instrument_class=$2
		 ORDER BY assessed_at DESC, assessment_id DESC
		 LIMIT 1`, accountID, class).Scan(
		&a.ID, &a.AccountID, &a.InstrumentClass, &a.Outcome, &a.Score,
		&a.Answers, &a.AssessedAt, &a.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: latest assessment %d/%s: %w",
			accountID, class, err)
	}
	return &a, nil
}

// InsertAssessment implements CategoryStore.InsertAssessment.
func (s *PgStore) InsertAssessment(ctx context.Context, a *Assessment) error {
	answers := a.Answers
	if len(answers) == 0 {
		answers = json.RawMessage("{}") // NOT NULL column — never write NULL
	}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO appropriateness_assessments
		    (account_id, instrument_class, outcome, score, answers_json,
		     assessed_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING assessment_id`,
		a.AccountID, a.InstrumentClass, a.Outcome, a.Score, answers,
		a.AssessedAt, a.ExpiresAt).Scan(&a.ID)
	if err != nil {
		return fmt.Errorf("compliance: insert assessment: %w", err)
	}
	return nil
}

// ListAssessments implements CategoryStore.ListAssessments (newest first).
func (s *PgStore) ListAssessments(ctx context.Context, accountID int64, limit int) ([]Assessment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT assessment_id, account_id, instrument_class, outcome, score,
		       answers_json, assessed_at, expires_at
		  FROM appropriateness_assessments
		 WHERE account_id=$1
		 ORDER BY assessed_at DESC, assessment_id DESC
		 LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("compliance: list assessments: %w", err)
	}
	defer rows.Close()
	var out []Assessment
	for rows.Next() {
		var a Assessment
		if err := rows.Scan(&a.ID, &a.AccountID, &a.InstrumentClass,
			&a.Outcome, &a.Score, &a.Answers, &a.AssessedAt, &a.ExpiresAt); err != nil {
			return nil, fmt.Errorf("compliance: assessment scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetCategoryTx implements CategoryStore.SetCategoryTx — one atomic tx:
// lock the account, detect open derivative exposure (the documented
// close-only-posture fact on downgrades), write client_category + nbp,
// and append the admin audit row + chain link.
func (s *PgStore) SetCategoryTx(ctx context.Context, p SetCategoryTx) (*CategoryChange, error) {
	var ch *CategoryChange
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var curCat, curTier string
		var curNBP bool
		err := tx.QueryRow(ctx, `
			SELECT client_category::text, nbp, kyc_tier::text
			  FROM accounts WHERE id=$1 FOR UPDATE`, p.AccountID).Scan(
			&curCat, &curNBP, &curTier)
		if errors.Is(err, pgx.ErrNoRows) {
			return excerrors.New("ACCOUNT_NOT_FOUND",
				fmt.Sprintf("account %d not found", p.AccountID))
		}
		if err != nil {
			return fmt.Errorf("compliance: lock account %d: %w", p.AccountID, err)
		}
		// Open derivative exposure — recorded on every change so a
		// downgrade's close-only posture is provable in the audit row
		// (positions × instruments.instrument_type <> 'SPOT').
		var openDeriv bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM positions p
				JOIN instruments i ON i.id = p.instrument_id
				WHERE p.account_id=$1 AND p.quantity <> 0
				  AND i.instrument_type <> 'SPOT')`, p.AccountID).Scan(&openDeriv); err != nil {
			return fmt.Errorf("compliance: open-exposure read: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE accounts
			   SET client_category=$2, nbp=$3, updated_at=$4
			 WHERE id=$1`, p.AccountID, string(p.Category), p.NBP, p.Now); err != nil {
			return fmt.Errorf("compliance: set category: %w", err)
		}
		before := map[string]any{
			"client_category": curCat, "nbp": curNBP, "kyc_tier": curTier,
		}
		after := map[string]any{
			"client_category": string(p.Category), "nbp": p.NBP,
			"open_derivative_exposure": openDeriv,
		}
		if p.Evidence != "" {
			after["evidence"] = p.Evidence
		}
		_, chainSeq, err := admin.Log(ctx, tx, admin.AuditEntry{
			AdminUserID: p.ReviewerID,
			Action:      p.Action,
			TargetType:  "account",
			TargetID:    &p.AccountID,
			BeforeState: before,
			AfterState:  after,
			IPAddress:   p.ClientIP,
		})
		if err != nil {
			return err
		}
		ch = &CategoryChange{
			AccountID: p.AccountID, From: ClientCategory(curCat),
			To: p.Category, NBP: p.NBP, Evidence: p.Evidence,
			ChangedBy: p.ReviewerID, AuditSeq: chainSeq,
			OpenExposure: openDeriv,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ch, nil
}
