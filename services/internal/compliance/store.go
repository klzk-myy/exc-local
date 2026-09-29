// PgStore — the PostgreSQL implementation of the compliance Store seam
// (migrations 203–205 + kyc_documents from migration 017). Document bytes
// never touch PostgreSQL — only object keys, digests and SSE markers.
package compliance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// PgStore implements Store over pgx.
type PgStore struct{ Pool *pgxpool.Pool }

// NewPgStore wires the store; pool may not be nil.
func NewPgStore(pool *pgxpool.Pool) (*PgStore, error) {
	if pool == nil {
		return nil, errors.New("compliance: nil pg pool")
	}
	return &PgStore{Pool: pool}, nil
}

// AccountTier reads accounts.kyc_tier — the reviewer-visible tier
// (Phase-14 writes it on approval; 'T0' until then).
func (s *PgStore) AccountTier(ctx context.Context, accountID int64) (string, error) {
	var tier string
	err := s.Pool.QueryRow(ctx,
		`SELECT kyc_tier::text FROM accounts WHERE id=$1`, accountID).Scan(&tier)
	if err != nil {
		return "", fmt.Errorf("compliance: account tier: %w", err)
	}
	return tier, nil
}

// CreateSubmission inserts the intake row and fills sub.ID.
func (s *PgStore) CreateSubmission(ctx context.Context, sub *Submission) error {
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO kyc_submissions
		    (account_id, requested_tier, status, jurisdiction, risk_score,
		     submitted_at, sla_due_at, created_at, updated_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$8)
		RETURNING id`,
		sub.AccountID, sub.RequestedTier, sub.Status, sub.Jurisdiction,
		sub.RiskScore, sub.SubmittedAt, sub.SLADueAt, sub.CreatedAt,
	).Scan(&sub.ID)
	if err != nil {
		return fmt.Errorf("compliance: create submission: %w", err)
	}
	return nil
}

// AttachDocument records one uploaded object's kyc_documents row and
// fills doc.ID. file_url stores the object key (bucket is deployment
// config — the §24 #102 objects carry SSE-KMS, digested + sized here).
func (s *PgStore) AttachDocument(ctx context.Context, doc *Document) error {
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO kyc_documents
		    (account_id, submission_id, type, file_url, status,
		     sha256, size_bytes, sse_algorithm, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		doc.AccountID, doc.SubmissionID, doc.Type, doc.ObjectKey,
		doc.Status, doc.SHA256, doc.SizeBytes, doc.SSEAlgorithm, doc.CreatedAt,
	).Scan(&doc.ID)
	if err != nil {
		return fmt.Errorf("compliance: attach document: %w", err)
	}
	return nil
}

// FailSubmission marks an intake FAILED (submit-path cleanup when an
// object write or document insert aborts mid-flight).
func (s *PgStore) FailSubmission(ctx context.Context, submissionID int64) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE kyc_submissions
		   SET status='FAILED', updated_at=now()
		 WHERE id=$1 AND status='PENDING_REVIEW'`, submissionID)
	if err != nil {
		return fmt.Errorf("compliance: fail submission %d: %w", submissionID, err)
	}
	return nil
}

// LatestSubmission returns the account's most recent submission or nil.
func (s *PgStore) LatestSubmission(ctx context.Context, accountID int64) (*Submission, error) {
	var sub Submission
	var jur, reason *string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, account_id, requested_tier, status, jurisdiction,
		       risk_score, submitted_at, sla_due_at, verified_at,
		       reverify_due_at, reviewed_at, reject_reason, appeal_of,
		       created_at, updated_at
		  FROM kyc_submissions
		 WHERE account_id=$1
		 ORDER BY id DESC
		 LIMIT 1`, accountID).Scan(
		&sub.ID, &sub.AccountID, &sub.RequestedTier, &sub.Status, &jur,
		&sub.RiskScore, &sub.SubmittedAt, &sub.SLADueAt, &sub.VerifiedAt,
		&sub.ReverifyDueAt, &sub.ReviewedAt, &reason, &sub.AppealOf,
		&sub.CreatedAt, &sub.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: latest submission: %w", err)
	}
	if jur != nil {
		sub.Jurisdiction = *jur
	}
	if reason != nil {
		sub.RejectReason = *reason
	}
	return &sub, nil
}

// ListAccountDocuments returns the account's document rows newest-first.
func (s *PgStore) ListAccountDocuments(ctx context.Context, accountID int64) ([]Document, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, account_id, COALESCE(submission_id,0), type, file_url,
		       status::text, COALESCE(sha256,''), COALESCE(size_bytes,0),
		       sse_algorithm, verified_at, created_at
		  FROM kyc_documents
		 WHERE account_id=$1
		 ORDER BY id DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("compliance: list documents: %w", err)
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.AccountID, &d.SubmissionID, &d.Type,
			&d.ObjectKey, &d.Status, &d.SHA256, &d.SizeBytes,
			&d.SSEAlgorithm, &d.VerifiedAt, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("compliance: document scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TierPolicy loads one kyc_tier_policies row; nil when unknown.
func (s *PgStore) TierPolicy(ctx context.Context, tier string) (*TierPolicy, error) {
	var p TierPolicy
	var reverify *int
	var wd, td *string // NUMERIC → decimal via text
	err := s.Pool.QueryRow(ctx, `
		SELECT tier, description, liveness_required, biometric_required,
		       rescreen_cadence, reverify_months, manual_review_sla_hours,
		       step_up_score, decline_score,
		       daily_withdrawal_usd::text, daily_trading_usd::text
		  FROM kyc_tier_policies WHERE tier=$1`, tier).Scan(
		&p.Tier, &p.Description, &p.LivenessRequired, &p.BiometricRequired,
		&p.RescreenCadence, &reverify, &p.ManualReviewSLAHours,
		&p.StepUpScore, &p.DeclineScore, &wd, &td)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: tier policy %q: %w", tier, err)
	}
	if reverify != nil {
		p.ReverifyMonths = *reverify
	}
	if wd != nil {
		d, derr := decimal.NewFromString(*wd)
		if derr != nil {
			return nil, fmt.Errorf("compliance: tier policy withdraw cap: %w", derr)
		}
		p.DailyWithdrawalUSD = &d
	}
	if td != nil {
		d, derr := decimal.NewFromString(*td)
		if derr != nil {
			return nil, fmt.Errorf("compliance: tier policy trading cap: %w", derr)
		}
		p.DailyTradingUSD = &d
	}
	return &p, nil
}

// Matrix returns the merged requirement grid for (tier, jurisdiction):
// every '*' default row plus the exact-jurisdiction overlays. An empty
// jurisdiction returns defaults only. Phase-14/vendor integrations read
// vendor + flags straight off these rows.
func (s *PgStore) Matrix(ctx context.Context, tier, jurisdiction string) ([]MatrixRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, tier, jurisdiction, vendor, document_type, doc_group,
		       required, max_doc_age_days, doc_expiry_lead_days, COALESCE(notes,'')
		  FROM kyc_ops_matrix
		 WHERE tier=$1 AND (jurisdiction='*' OR jurisdiction=$2)
		 ORDER BY jurisdiction <> '*', doc_group, document_type`, tier, jurisdiction)
	if err != nil {
		return nil, fmt.Errorf("compliance: ops matrix %q/%q: %w", tier, jurisdiction, err)
	}
	defer rows.Close()
	var out []MatrixRow
	for rows.Next() {
		var r MatrixRow
		if err := rows.Scan(&r.ID, &r.Tier, &r.Jurisdiction, &r.Vendor,
			&r.DocumentType, &r.DocGroup, &r.Required,
			&r.MaxDocAgeDays, &r.DocExpiryLeadDay, &r.Notes); err != nil {
			return nil, fmt.Errorf("compliance: matrix scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OverdueReviews feeds the manual-review SLA report: PENDING_REVIEW
// submissions whose sla_due_at (submitted_at + 24h) has passed.
// Phase-14 owns the escalation sweeper — this is the query it runs.
func (s *PgStore) OverdueReviews(ctx context.Context, now time.Time, limit int) ([]Submission, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, account_id, requested_tier, status, jurisdiction,
		       risk_score, submitted_at, sla_due_at, verified_at,
		       reverify_due_at, reviewed_at, reject_reason, appeal_of,
		       created_at, updated_at
		  FROM kyc_submissions
		 WHERE status='PENDING_REVIEW' AND sla_due_at < $1
		 ORDER BY sla_due_at
		 LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("compliance: overdue reviews: %w", err)
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		var sub Submission
		var jur, reason *string
		if err := rows.Scan(&sub.ID, &sub.AccountID, &sub.RequestedTier,
			&sub.Status, &jur, &sub.RiskScore, &sub.SubmittedAt,
			&sub.SLADueAt, &sub.VerifiedAt, &sub.ReverifyDueAt,
			&sub.ReviewedAt, &reason, &sub.AppealOf,
			&sub.CreatedAt, &sub.UpdatedAt); err != nil {
			return nil, fmt.Errorf("compliance: overdue scan: %w", err)
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

// InsertSelfCert persists one tax self-certification row.
func (s *PgStore) InsertSelfCert(ctx context.Context, c *SelfCert) error {
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO tax_self_certifications
		    (account_id, form_type, tin, tin_country, tin_kind,
		     fields, status, tin_validated_at, created_at)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),
		        COALESCE(NULLIF($6,'')::jsonb,'{}'::jsonb),$7,$8,$9)
		RETURNING id`,
		c.AccountID, c.FormType, c.TIN, c.TINCountry, c.TINKind,
		string(c.Fields), c.Status, c.TINValidatedAt, c.CreatedAt,
	).Scan(&c.ID)
	if err != nil {
		return fmt.Errorf("compliance: insert self-cert: %w", err)
	}
	return nil
}

// ListSelfCerts returns the account's self-certification history,
// newest first (superseded chain included — Phase-21 reads it all).
func (s *PgStore) ListSelfCerts(ctx context.Context, accountID int64) ([]SelfCert, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, account_id, form_type, COALESCE(tin,''),
		       COALESCE(tin_country,''), COALESCE(tin_kind,''),
		       fields, status, tin_validated_at, validated_at,
		       superseded_by, created_at
		  FROM tax_self_certifications
		 WHERE account_id=$1
		 ORDER BY id DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("compliance: list self-certs: %w", err)
	}
	defer rows.Close()
	var out []SelfCert
	for rows.Next() {
		var c SelfCert
		if err := rows.Scan(&c.ID, &c.AccountID, &c.FormType, &c.TIN,
			&c.TINCountry, &c.TINKind, &c.Fields, &c.Status,
			&c.TINValidatedAt, &c.ValidatedAt, &c.SupersededBy,
			&c.CreatedAt); err != nil {
			return nil, fmt.Errorf("compliance: self-cert scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// HasSelfCert reports whether the account has any self-certification on
// file (the TAX doc_group satisfaction check in the submission flow).
func (s *PgStore) HasSelfCert(ctx context.Context, accountID int64) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM tax_self_certifications
		                WHERE account_id=$1)`, accountID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("compliance: has self-cert: %w", err)
	}
	return ok, nil
}
