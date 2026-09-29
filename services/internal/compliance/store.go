// PgStore — the PostgreSQL implementation of the compliance Store seam
// (migrations 203–205 + kyc_documents from migration 017). Document bytes
// never touch PostgreSQL — only object keys, digests and SSE markers.
//
// PII-F1 remediation (migration 210): tax_self_certifications.tin and the
// fields JSONB document (legal_name/address/entity/treaty) are sealed at
// rest — tin_sealed/fields_sealed BYTEA carry AES-256-GCM nonce‖ct blobs
// via SecretBox. The plaintext columns survive only as a read-fallback
// window for pre-migration rows until SealTaxPIIBackfill NULLs them.
package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// SecretBox seals/unseals field-level PII at rest; satisfied by
// auth.SecretBox (AES-256-GCM over secrets.data_key). Kept as an
// interface so this package does not import the auth cluster — the same
// convention as internal/webhooks. Required: a nil box fails closed at
// construction — plaintext PII must never reach the table.
type SecretBox interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(blob []byte) ([]byte, error)
}

// PgStore implements Store over pgx.
type PgStore struct {
	Pool *pgxpool.Pool
	box  SecretBox
}

// NewPgStore wires the store; pool and box may not be nil. The box is
// mandatory because every tax_self_certifications write carries PII —
// a boxless store could only ever write plaintext, which is exactly the
// PII-F1 defect this seam exists to close.
func NewPgStore(pool *pgxpool.Pool, box SecretBox) (*PgStore, error) {
	if pool == nil {
		return nil, errors.New("compliance: nil pg pool")
	}
	if box == nil {
		return nil, errors.New("compliance: nil secret box — PII sealing required")
	}
	return &PgStore{Pool: pool, box: box}, nil
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

// sealSelfCert produces the at-rest blobs for one certification: the TIN
// sealed into tin_sealed (nil when the form carries no TIN — W-8* TIN is
// optional) and the whole fields document sealed into fields_sealed. The
// plaintext columns are never written post-migration-210; a seal failure
// aborts the insert — fail closed, never fall back to plaintext.
func (s *PgStore) sealSelfCert(c *SelfCert) (tinSealed, fieldsSealed []byte, err error) {
	if s.box == nil {
		return nil, nil, errors.New("compliance: secret box unset — refusing to store plaintext PII")
	}
	if c.TIN != "" {
		if tinSealed, err = s.box.Seal([]byte(c.TIN)); err != nil {
			return nil, nil, fmt.Errorf("compliance: seal tin: %w", err)
		}
	}
	fields := c.Fields
	if len(fields) == 0 {
		fields = json.RawMessage("{}")
	}
	if fieldsSealed, err = s.box.Seal(fields); err != nil {
		return nil, nil, fmt.Errorf("compliance: seal fields: %w", err)
	}
	return tinSealed, fieldsSealed, nil
}

// InsertSelfCert persists one tax self-certification row. tin/fields are
// sealed into tin_sealed/fields_sealed (migration 210, PII-F1); the
// deprecated plaintext columns are written NULL/'{}'.
func (s *PgStore) InsertSelfCert(ctx context.Context, c *SelfCert) error {
	tinSealed, fieldsSealed, err := s.sealSelfCert(c)
	if err != nil {
		return err
	}
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO tax_self_certifications
		    (account_id, form_type, tin, tin_country, tin_kind,
		     fields, tin_sealed, fields_sealed,
		     status, tin_validated_at, created_at)
		VALUES ($1,$2,NULL,NULLIF($3,''),NULLIF($4,''),
		        '{}'::jsonb,$5,$6,$7,$8,$9)
		RETURNING id`,
		c.AccountID, c.FormType, c.TINCountry, c.TINKind,
		tinSealed, fieldsSealed, c.Status, c.TINValidatedAt, c.CreatedAt,
	).Scan(&c.ID)
	if err != nil {
		return fmt.Errorf("compliance: insert self-cert: %w", err)
	}
	return nil
}

// ListSelfCerts returns the account's self-certification history,
// newest first (superseded chain included — Phase-21 reads it all).
// Reads prefer the sealed columns (PII-F1); a NULL sealed column on a
// row that still carries plaintext means a pre-backfill legacy row —
// fall back to plaintext once so the window reads correctly.
func (s *PgStore) ListSelfCerts(ctx context.Context, accountID int64) ([]SelfCert, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, account_id, form_type,
		       tin_sealed, COALESCE(tin,''),
		       COALESCE(tin_country,''), COALESCE(tin_kind,''),
		       fields_sealed, fields,
		       status, tin_validated_at, validated_at,
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
		var tinSealed, fieldsSealed, fieldsRaw []byte
		if err := rows.Scan(&c.ID, &c.AccountID, &c.FormType,
			&tinSealed, &c.TIN,
			&c.TINCountry, &c.TINKind,
			&fieldsSealed, &fieldsRaw, &c.Status,
			&c.TINValidatedAt, &c.ValidatedAt, &c.SupersededBy,
			&c.CreatedAt); err != nil {
			return nil, fmt.Errorf("compliance: self-cert scan: %w", err)
		}
		if (tinSealed != nil || fieldsSealed != nil) && s.box == nil {
			return nil, fmt.Errorf("compliance: secret box unset — sealed cert %d unreadable", c.ID)
		}
		if tinSealed != nil {
			pt, err := s.box.Open(tinSealed)
			if err != nil {
				return nil, fmt.Errorf("compliance: unseal tin (cert %d): %w", c.ID, err)
			}
			c.TIN = string(pt)
		}
		if fieldsSealed != nil {
			pt, err := s.box.Open(fieldsSealed)
			if err != nil {
				return nil, fmt.Errorf("compliance: unseal fields (cert %d): %w", c.ID, err)
			}
			c.Fields = json.RawMessage(pt)
		} else {
			c.Fields = json.RawMessage(fieldsRaw)
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

// ---------------------------------------------------------------------------
// PII-F1 backfill (migration 210) — app-layer crypto, cannot run in SQL
// ---------------------------------------------------------------------------

// sealBackfillBatch bounds one backfill transaction.
const sealBackfillBatch = 200

// SealTaxPIIBackfill re-encrypts legacy plaintext rows written before
// migration 210: any row still carrying tin <> ” or fields <> '{}'
// gets its values sealed into tin_sealed/fields_sealed and the plaintext
// columns reset to NULL/'{}'. Idempotent — already-sealed rows fail the
// WHERE predicate — and bounded by batch transactions. Run via
// `exchange seal-tax-pii`; returns the number of rows migrated.
func (s *PgStore) SealTaxPIIBackfill(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.sealTaxBatch(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n < sealBackfillBatch {
			return total, nil
		}
	}
}

func (s *PgStore) sealTaxBatch(ctx context.Context) (int, error) {
	if s.box == nil {
		return 0, errors.New("compliance: secret box unset — cannot seal")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("compliance: backfill tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, COALESCE(tin,''), fields::text
		  FROM tax_self_certifications
		 WHERE (tin IS NOT NULL AND tin <> '') OR fields <> '{}'::jsonb
		 ORDER BY id
		 LIMIT $1
		 FOR UPDATE`, sealBackfillBatch)
	if err != nil {
		return 0, fmt.Errorf("compliance: backfill select: %w", err)
	}
	type legacyRow struct {
		id     int64
		tin    string
		fields string
	}
	var batch []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.id, &r.tin, &r.fields); err != nil {
			rows.Close()
			return 0, fmt.Errorf("compliance: backfill scan: %w", err)
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("compliance: backfill rows: %w", err)
	}
	for _, r := range batch {
		var tinSealed []byte
		if r.tin != "" {
			if tinSealed, err = s.box.Seal([]byte(r.tin)); err != nil {
				return 0, fmt.Errorf("compliance: backfill seal tin (id %d): %w", r.id, err)
			}
		}
		fieldsSealed, err := s.box.Seal([]byte(r.fields))
		if err != nil {
			return 0, fmt.Errorf("compliance: backfill seal fields (id %d): %w", r.id, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tax_self_certifications
			   SET tin_sealed=$2, fields_sealed=$3,
			       tin=NULL, fields='{}'::jsonb
			 WHERE id=$1`, r.id, tinSealed, fieldsSealed); err != nil {
			return 0, fmt.Errorf("compliance: backfill update (id %d): %w", r.id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("compliance: backfill commit: %w", err)
	}
	return len(batch), nil
}

// UnsealTaxPII restores the plaintext tin/fields columns from the sealed
// blobs — the required pre-step before the 210 down migration, which
// drops tin_sealed/fields_sealed (a privacy regression; run only for a
// deliberate downgrade). Idempotent; leaves the sealed columns in place.
func (s *PgStore) UnsealTaxPII(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.unsealTaxBatch(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n < sealBackfillBatch {
			return total, nil
		}
	}
}

func (s *PgStore) unsealTaxBatch(ctx context.Context) (int, error) {
	if s.box == nil {
		return 0, errors.New("compliance: secret box unset — cannot unseal")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("compliance: unseal tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Rows already carrying plaintext are skipped — only sealed-but-not-
	// yet-restored rows are rewritten, keeping the loop idempotent.
	rows, err := tx.Query(ctx, `
		SELECT id, tin_sealed, fields_sealed
		  FROM tax_self_certifications
		 WHERE (tin_sealed IS NOT NULL AND COALESCE(tin,'') = '')
		    OR (fields_sealed IS NOT NULL AND fields = '{}'::jsonb)
		 ORDER BY id
		 LIMIT $1
		 FOR UPDATE`, sealBackfillBatch)
	if err != nil {
		return 0, fmt.Errorf("compliance: unseal select: %w", err)
	}
	type sealedRow struct {
		id          int64
		tin, fields []byte
	}
	var batch []sealedRow
	for rows.Next() {
		var r sealedRow
		if err := rows.Scan(&r.id, &r.tin, &r.fields); err != nil {
			rows.Close()
			return 0, fmt.Errorf("compliance: unseal scan: %w", err)
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("compliance: unseal rows: %w", err)
	}
	for _, r := range batch {
		var tin string
		if r.tin != nil {
			pt, err := s.box.Open(r.tin)
			if err != nil {
				return 0, fmt.Errorf("compliance: unseal tin (id %d): %w", r.id, err)
			}
			tin = string(pt)
		}
		fields := "{}"
		if r.fields != nil {
			pt, err := s.box.Open(r.fields)
			if err != nil {
				return 0, fmt.Errorf("compliance: unseal fields (id %d): %w", r.id, err)
			}
			fields = string(pt)
		}
		// Only a column whose sealed blob exists is overwritten — the
		// plaintext of the other column is preserved verbatim.
		if _, err := tx.Exec(ctx, `
			UPDATE tax_self_certifications
			   SET tin    = CASE WHEN $4 THEN NULLIF($2,'') ELSE tin END,
			       fields = CASE WHEN $5 THEN $3::jsonb   ELSE fields END
			 WHERE id=$1`, r.id, tin, fields, r.tin != nil, r.fields != nil); err != nil {
			return 0, fmt.Errorf("compliance: unseal update (id %d): %w", r.id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("compliance: unseal commit: %w", err)
	}
	return len(batch), nil
}
