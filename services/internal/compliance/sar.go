// SAR generation — Phase-21 Task 21.3.3 (spec §14.1/§14.3; §24 #108;
// §27.1 SAR/CTR matrix row).
//
// A SAR (Suspicious Activity Report, FinCEN SAR form) is drafted from
// deterministic triggers — sanctions match, velocity anomaly,
// structuring, surveillance signal, CTR escalation, hold escalation or
// a manual Compliance Officer report — then walks the four-eyes filing
// lifecycle:
//
//	DRAFT → UNDER_REVIEW (officer reviews) → APPROVED (a DISTINCT
//	second officer approves — SAR_DUAL_CONTROL_REQUIRED on any
//	self-action) → FILED (filed_at + filing_ref recorded).
//	REJECTED is the terminal false-positive disposition.
//
// FILED rows are immutable — migration 033's trigger rejects UPDATE and
// DELETE; corrections file a new row via amends_id. filing_deadline
// = detected_at + 30 days (the FinCEN clock starts at initial
// detection, not at drafting).
//
// Drafting is idempotent on source_ref — deterministic anchors
// ('signal:{id}', 'hold:{id}', 'aml:{dedup_key}', 'manual:{key}') mean
// at-least-once NATS redelivery and sweep re-runs regenerate the same
// row instead of duplicating reports.
package compliance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// Trigger sources (migration 033 CHECK mirror).
const (
	SARTriggerManual        = "MANUAL"
	SARTriggerSanctions     = "SANCTIONS_MATCH"
	SARTriggerVelocity      = "VELOCITY_ANOMALY"
	SARTriggerStructuring   = "STRUCTURING"
	SARTriggerSurveillance  = "SURVEILLANCE_SIGNAL"
	SARTriggerCTR           = "CTR"
	SARTriggerMonitoring    = "MONITORING_RULE"
	SARTriggerHoldEscalated = "HOLD_ESCALATION"
)

// Lifecycle statuses.
const (
	SARStatusDraft       = "DRAFT"
	SARStatusUnderReview = "UNDER_REVIEW"
	SARStatusApproved    = "APPROVED"
	SARStatusFiled       = "FILED"
	SARStatusRejected    = "REJECTED"
)

// SARFilingWindow is the FinCEN deadline from initial detection.
const SARFilingWindow = 30 * 24 * time.Hour

// SARReport is one sar_reports row.
type SARReport struct {
	ID             int64           `json:"id"`
	TriggerType    string          `json:"trigger_type"`
	AccountID      *int64          `json:"account_id,omitempty"`
	SubjectRef     string          `json:"subject_ref,omitempty"`
	Description    string          `json:"description"`
	Evidence       json.RawMessage `json:"evidence"`
	TransactionIDs json.RawMessage `json:"transaction_ids"`
	Status         string          `json:"status"`
	SourceRef      string          `json:"source_ref"`
	DetectedAt     time.Time       `json:"detected_at"`
	FilingDeadline time.Time       `json:"filing_deadline"`
	CreatedBy      *int64          `json:"created_by,omitempty"`
	ReviewedBy     *int64          `json:"reviewed_by,omitempty"`
	ReviewedAt     *time.Time      `json:"reviewed_at,omitempty"`
	ReviewNote     string          `json:"review_note,omitempty"`
	ApprovedBy     *int64          `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time      `json:"approved_at,omitempty"`
	ApprovalNote   string          `json:"approval_note,omitempty"`
	FiledBy        *int64          `json:"filed_by,omitempty"`
	FiledAt        *time.Time      `json:"filed_at,omitempty"`
	FilingRef      string          `json:"filing_ref,omitempty"`
	AmendsID       *int64          `json:"amends_id,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// SARDraftInput is the machine/officer draft request.
type SARDraftInput struct {
	TriggerType    string
	AccountID      *int64 // NULL for unresolved subjects
	SubjectRef     string // opaque ref when account_id is nil
	Description    string
	Evidence       any       // marshalled to JSONB
	TransactionIDs []int64   // funding_transactions covered
	SourceRef      string    // deterministic dedup anchor; "" → generated manual ref
	DetectedAt     time.Time // initial detection; zero → now
	CreatedBy      *int64    // officer id; nil = machine draft
	AmendsID       *int64    // amendment of a FILED report
}

// SARService owns the lifecycle; every mutator runs in one tx with the
// audit hash-chain append.
type SARService struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
	newToken func() (string, error)
	now      func() time.Time
}

// NewSARService wires the service; the pool is required and the role
// resolver gates officer actions (Compliance Officer / Super Admin).
func NewSARService(pool *pgxpool.Pool, resolver HoldRoleResolver) (*SARService, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "sar service requires pool")
	}
	return &SARService{pool: pool, resolver: resolver,
		newToken: defaultSourceToken, now: time.Now}, nil
}

// WithTokenGenerator overrides the manual source_ref generator (tests).
func (s *SARService) WithTokenGenerator(f func() (string, error)) *SARService {
	s.newToken = f
	return s
}

// WithClock overrides the clock (tests).
func (s *SARService) WithClock(c func() time.Time) *SARService {
	s.now = c
	return s
}

var validSARTriggers = map[string]bool{
	SARTriggerManual: true, SARTriggerSanctions: true,
	SARTriggerVelocity: true, SARTriggerStructuring: true,
	SARTriggerSurveillance: true, SARTriggerCTR: true,
	SARTriggerMonitoring: true, SARTriggerHoldEscalated: true,
}

// ---------------------------------------------------------------------------
// Drafting
// ---------------------------------------------------------------------------

// Draft persists a DRAFT report. source_ref dedup makes redelivery and
// re-scan idempotent: a conflicting insert returns the existing row with
// created=false.
func (s *SARService) Draft(ctx context.Context, in SARDraftInput) (*SARReport, bool, error) {
	if !validSARTriggers[in.TriggerType] {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"unknown trigger_type "+in.TriggerType)
	}
	if in.Description == "" {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"SAR description is required")
	}
	// A report without a traceable subject is never persisted.
	if (in.AccountID == nil || *in.AccountID == 0) && in.SubjectRef == "" {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"SAR requires account_id or subject_ref")
	}
	sourceRef := in.SourceRef
	if sourceRef == "" {
		tok, err := s.newToken()
		if err != nil {
			return nil, false, excerrors.Wrap("INTERNAL_ERROR", "source ref", err)
		}
		sourceRef = "manual:" + tok
	}
	if len(sourceRef) > 160 {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"source_ref exceeds 160 chars")
	}
	detected := in.DetectedAt
	if detected.IsZero() {
		detected = s.now().UTC()
	}
	evidence, err := json.Marshal(in.Evidence)
	if err != nil {
		return nil, false, excerrors.Wrap("INVALID_REQUEST", "evidence", err)
	}
	if evidence == nil || string(evidence) == "null" {
		evidence = []byte("{}")
	}
	txIDs := in.TransactionIDs
	if txIDs == nil {
		txIDs = []int64{}
	}
	txJSON, err := json.Marshal(txIDs)
	if err != nil {
		return nil, false, excerrors.Wrap("INVALID_REQUEST", "transaction_ids", err)
	}
	deadline := detected.Add(SARFilingWindow)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "sar draft tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO sar_reports
		    (trigger_type, account_id, subject_ref, description, evidence,
		     transaction_ids, status, source_ref, detected_at,
		     filing_deadline, created_by, amends_id)
		VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,'DRAFT',$7,$8,$9,$10,$11)
		ON CONFLICT (source_ref) DO NOTHING
		RETURNING id`,
		in.TriggerType, in.AccountID, in.SubjectRef, in.Description,
		evidence, txJSON, sourceRef, detected, deadline, in.CreatedBy,
		in.AmendsID).Scan(&id)
	if err == pgx.ErrNoRows {
		// Dedup hit — return the existing row unchanged.
		existing, gerr := s.getInTx(ctx, tx, sourceRef)
		if gerr != nil {
			return nil, false, gerr
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, excerrors.Wrap("INTERNAL_ERROR", "sar dedup commit", err)
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "sar draft insert", err)
	}
	if _, err := audit.Append(ctx, tx, "sar_reports", &id,
		"SAR_DRAFTED", nil); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "sar audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "sar draft commit", err)
	}
	rec, err := s.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// DraftFromSignal drafts a SAR for one surveillance_signals row —
// deterministic source_ref 'signal:{id}' makes redelivery idempotent.
// The signal's anonymized account_hash lands on subject_ref (the case
// workflow resolves it to an account on review).
func (s *SARService) DraftFromSignal(ctx context.Context, signalID int64) (*SARReport, bool, error) {
	var (
		sigType, symbol string
		accountHash     int64
		evidence        []byte
		detectedAt      time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT signal_type, symbol, account_hash, evidence, created_at
		FROM surveillance_signals WHERE id = $1`, signalID).
		Scan(&sigType, &symbol, &accountHash, &evidence, &detectedAt)
	if err == pgx.ErrNoRows {
		return nil, false, excerrors.New("NOT_FOUND",
			fmt.Sprintf("surveillance signal %d not found", signalID))
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "signal load", err)
	}
	return s.Draft(ctx, SARDraftInput{
		TriggerType: SARTriggerSurveillance,
		SubjectRef:  fmt.Sprintf("account_hash:%d", accountHash),
		Description: fmt.Sprintf("surveillance signal %s on %s (id %d) — officer review required",
			sigType, symbol, signalID),
		Evidence:   json.RawMessage(evidence),
		SourceRef:  fmt.Sprintf("signal:%d", signalID),
		DetectedAt: detectedAt,
	})
}

// DraftFromHold drafts a SAR for an escalated compliance hold — the
// Task 14.3.10 'escalate SAR' disposition lands here via
// HoldService.WithSARDraft.
func (s *SARService) DraftFromHold(ctx context.Context, h *Hold) (*SARReport, bool, error) {
	acct := h.AccountID
	return s.Draft(ctx, SARDraftInput{
		TriggerType: SARTriggerHoldEscalated,
		AccountID:   &acct,
		Description: fmt.Sprintf("compliance hold %s escalated to SAR (%s): %s",
			h.HoldID, h.Trigger, h.Reason),
		Evidence: map[string]any{
			"hold_id":      h.HoldID,
			"trigger":      h.Trigger,
			"evidence_ref": h.EvidenceRef,
			"placed_at":    h.PlacedAt,
		},
		SourceRef:  "hold:" + h.HoldID,
		DetectedAt: h.PlacedAt,
	})
}

// ---------------------------------------------------------------------------
// Officer lifecycle — review → approve → file (four-eyes on approval)
// ---------------------------------------------------------------------------

// Review moves DRAFT/UNDER_REVIEW → UNDER_REVIEW with reviewer
// attribution. Compliance Officer or Super Admin.
func (s *SARService) Review(ctx context.Context, id, officer int64,
	note string) (*SARReport, error) {
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	return s.transition(ctx, id, func(rec *SARReport) error {
		if rec.Status != SARStatusDraft && rec.Status != SARStatusUnderReview {
			return excerrors.New("INVALID_REQUEST",
				"SAR is "+rec.Status+" — only DRAFT reports can be reviewed")
		}
		rec.Status = SARStatusUnderReview
		rec.ReviewedBy = &officer
		now := s.now().UTC()
		rec.ReviewedAt = &now
		rec.ReviewNote = note
		return nil
	}, "SAR_REVIEWED", officer)
}

// Approve moves UNDER_REVIEW → APPROVED. The approver must differ from
// both the reviewer and the creator — §24 #108 dual control.
func (s *SARService) Approve(ctx context.Context, id, approver int64,
	note string) (*SARReport, error) {
	if err := s.checkRole(ctx, approver); err != nil {
		return nil, err
	}
	return s.transition(ctx, id, func(rec *SARReport) error {
		if rec.Status != SARStatusUnderReview {
			return excerrors.New("INVALID_REQUEST",
				"SAR is "+rec.Status+" — review must precede approval")
		}
		if rec.ReviewedBy != nil && *rec.ReviewedBy == approver {
			return excerrors.New("SAR_DUAL_CONTROL_REQUIRED",
				"approver must differ from the reviewing officer")
		}
		if rec.CreatedBy != nil && *rec.CreatedBy == approver {
			return excerrors.New("SAR_DUAL_CONTROL_REQUIRED",
				"approver must differ from the drafting officer")
		}
		rec.Status = SARStatusApproved
		rec.ApprovedBy = &approver
		now := s.now().UTC()
		rec.ApprovedAt = &now
		rec.ApprovalNote = note
		return nil
	}, "SAR_APPROVED", approver)
}

// File records the actual FinCEN submission — APPROVED → FILED with the
// BSA e-filing tracking reference. The DB trigger makes the row
// immutable from here; amendments file via a new row (amends_id).
func (s *SARService) File(ctx context.Context, id, officer int64,
	filingRef string) (*SARReport, error) {
	if filingRef == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"filing_ref (BSA e-filing tracking number) is required")
	}
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	return s.transition(ctx, id, func(rec *SARReport) error {
		if rec.Status != SARStatusApproved {
			return excerrors.New("INVALID_REQUEST",
				"SAR is "+rec.Status+" — approval must precede filing")
		}
		rec.Status = SARStatusFiled
		rec.FiledBy = &officer
		now := s.now().UTC()
		rec.FiledAt = &now
		rec.FilingRef = filingRef
		return nil
	}, "SAR_FILED", officer)
}

// Reject closes a false-positive draft — terminal, never filed.
func (s *SARService) Reject(ctx context.Context, id, officer int64,
	reason string) (*SARReport, error) {
	if reason == "" {
		return nil, excerrors.New("INVALID_REQUEST", "rejection reason required")
	}
	if err := s.checkRole(ctx, officer); err != nil {
		return nil, err
	}
	return s.transition(ctx, id, func(rec *SARReport) error {
		if rec.Status == SARStatusFiled {
			return excerrors.New("INVALID_REQUEST", "filed SARs cannot be rejected")
		}
		if rec.Status == SARStatusRejected {
			return excerrors.New("INVALID_REQUEST", "SAR already rejected")
		}
		rec.Status = SARStatusRejected
		rec.ReviewedBy = &officer
		now := s.now().UTC()
		rec.ReviewedAt = &now
		rec.ReviewNote = reason
		return nil
	}, "SAR_REJECTED", officer)
}

// transition locks the row, applies the mutate fn, writes the update +
// audit row in one tx. FILED rows additionally hit the DB immutability
// trigger — defense in depth against non-service writes.
func (s *SARService) transition(ctx context.Context, id int64,
	mutate func(rec *SARReport) error, action string, actor int64) (*SARReport, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "sar tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rec, err := s.lock(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := mutate(rec); err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE sar_reports
		SET status = $2, reviewed_by = $3, reviewed_at = $4,
		    review_note = NULLIF($5,''), approved_by = $6,
		    approved_at = $7, approval_note = NULLIF($8,''),
		    filed_by = $9, filed_at = $10, filing_ref = NULLIF($11,''),
		    updated_at = now()
		WHERE id = $1`,
		rec.ID, rec.Status, rec.ReviewedBy, rec.ReviewedAt, rec.ReviewNote,
		rec.ApprovedBy, rec.ApprovedAt, rec.ApprovalNote,
		rec.FiledBy, rec.FiledAt, rec.FilingRef)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "sar transition", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, excerrors.New("INTERNAL_ERROR", "sar vanished mid-update")
	}
	if _, err := audit.Append(ctx, tx, "sar_reports", &id, action, nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "sar audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "sar commit", err)
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// Get returns one report.
func (s *SARService) Get(ctx context.Context, id int64) (*SARReport, error) {
	rec, err := scanSAR(s.pool.QueryRow(ctx, `
		SELECT `+sarCols+` FROM sar_reports WHERE id = $1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "sar report not found")
	}
	return rec, err
}

// List returns newest-first reports; status "" lists all, "OPEN" lists
// every pre-terminal status.
func (s *SARService) List(ctx context.Context, status string, limit int) ([]SARReport, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + sarCols + ` FROM sar_reports`
	args := []any{}
	switch status {
	case "":
	case "OPEN":
		q += ` WHERE status IN ('DRAFT','UNDER_REVIEW','APPROVED')`
	default:
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY filing_deadline, id LIMIT ` + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "sar list", err)
	}
	defer rows.Close()
	return scanSARs(rows)
}

// Overdue returns open reports past the 30-day FinCEN deadline — the
// sweep surfaces them to the officer dashboard.
func (s *SARService) Overdue(ctx context.Context, limit int) ([]SARReport, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+sarCols+` FROM sar_reports
		WHERE status IN ('DRAFT','UNDER_REVIEW','APPROVED')
		  AND filing_deadline < now()
		ORDER BY filing_deadline LIMIT $1`, limit)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "sar overdue", err)
	}
	defer rows.Close()
	return scanSARs(rows)
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

const sarCols = `
	id, trigger_type, account_id, subject_ref, description, evidence,
	transaction_ids, status, source_ref, detected_at, filing_deadline,
	created_by, reviewed_by, reviewed_at, review_note, approved_by,
	approved_at, approval_note, filed_by, filed_at, filing_ref,
	amends_id, created_at, updated_at`

func (s *SARService) lock(ctx context.Context, tx pgx.Tx, id int64) (*SARReport, error) {
	rec, err := scanSAR(tx.QueryRow(ctx, `
		SELECT `+sarCols+` FROM sar_reports WHERE id = $1 FOR UPDATE`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "sar report not found")
	}
	return rec, err
}

func (s *SARService) getInTx(ctx context.Context, tx pgx.Tx, sourceRef string) (*SARReport, error) {
	rec, err := scanSAR(tx.QueryRow(ctx, `
		SELECT `+sarCols+` FROM sar_reports WHERE source_ref = $1`, sourceRef))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("INTERNAL_ERROR",
			"sar dedup conflict without stored row")
	}
	return rec, err
}

func (s *SARService) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot action SARs")
	}
	return nil
}

func scanSAR(row rowScanner) (*SARReport, error) {
	var r SARReport
	var subject, note, appNote, fref *string
	err := row.Scan(&r.ID, &r.TriggerType, &r.AccountID, &subject,
		&r.Description, &r.Evidence, &r.TransactionIDs, &r.Status,
		&r.SourceRef, &r.DetectedAt, &r.FilingDeadline, &r.CreatedBy,
		&r.ReviewedBy, &r.ReviewedAt, &note, &r.ApprovedBy,
		&r.ApprovedAt, &appNote, &r.FiledBy, &r.FiledAt, &fref,
		&r.AmendsID, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if subject != nil {
		r.SubjectRef = *subject
	}
	if note != nil {
		r.ReviewNote = *note
	}
	if appNote != nil {
		r.ApprovalNote = *appNote
	}
	if fref != nil {
		r.FilingRef = *fref
	}
	return &r, nil
}

func scanSARs(rows pgx.Rows) ([]SARReport, error) {
	var out []SARReport
	for rows.Next() {
		var r SARReport
		var subject, note, appNote, fref *string
		if err := rows.Scan(&r.ID, &r.TriggerType, &r.AccountID, &subject,
			&r.Description, &r.Evidence, &r.TransactionIDs, &r.Status,
			&r.SourceRef, &r.DetectedAt, &r.FilingDeadline, &r.CreatedBy,
			&r.ReviewedBy, &r.ReviewedAt, &note, &r.ApprovedBy,
			&r.ApprovedAt, &appNote, &r.FiledBy, &r.FiledAt, &fref,
			&r.AmendsID, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "sar scan", err)
		}
		if subject != nil {
			r.SubjectRef = *subject
		}
		if note != nil {
			r.ReviewNote = *note
		}
		if appNote != nil {
			r.ApprovalNote = *appNote
		}
		if fref != nil {
			r.FilingRef = *fref
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// defaultSourceToken mints a urlsafe manual-draft dedup token.
func defaultSourceToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "sar_" + base64.RawURLEncoding.EncodeToString(raw), nil
}
