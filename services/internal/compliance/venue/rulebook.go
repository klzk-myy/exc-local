// Rulebook / product governance — Task 21.3.15 item 2 (spec §14.1b):
// versioned rules and product terms with approval/effective dates,
// participant notices, acknowledgement evidence and regulator
// filing/approval status. EFFECTIVE activation is refused before the
// required approvals stand (VENUE_RULEBOOK_NOT_APPROVED, 409); an
// emergency rule change may activate immediately with its reason
// recorded (SDD edge case), the notice fan-out still enforced.
package venue

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// Rulebook kinds / statuses (migration 250 CHECK mirror).
const (
	RuleKindRulebook     = "RULEBOOK"
	RuleKindProductTerms = "PRODUCT_TERMS"

	RuleStatusDraft      = "DRAFT"
	RuleStatusFiled      = "FILED"
	RuleStatusApproved   = "APPROVED"
	RuleStatusEffective  = "EFFECTIVE"
	RuleStatusSuperseded = "SUPERSEDED"
	RuleStatusWithdrawn  = "WITHDRAWN"
)

// Regulator filing statuses (migration 250 CHECK mirror).
const (
	RegStatusNotRequired = "NOT_REQUIRED"
	RegStatusFiled       = "FILED"
	RegStatusApproved    = "APPROVED"
	RegStatusRejected    = "REJECTED"
)

// Rulebook is one venue_rulebooks row.
type Rulebook struct {
	RulebookID         int64           `json:"rulebook_id"`
	Kind               string          `json:"kind"`
	ScopeKey           string          `json:"scope_key"`
	Version            string          `json:"version"`
	BodyRef            string          `json:"body_ref"`
	Status             string          `json:"status"`
	RequiresRegulator  bool            `json:"requires_regulator_approval"`
	RegulatorStatus    string          `json:"regulator_status"`
	RegulatorFilingRef string          `json:"regulator_filing_ref,omitempty"`
	NoticePeriodDays   int             `json:"notice_period_days"`
	Emergency          bool            `json:"emergency"`
	Detail             json.RawMessage `json:"detail"`
	EffectiveFrom      *time.Time      `json:"effective_from,omitempty"`
	ApprovedBy         *int64          `json:"approved_by,omitempty"`
	ApprovedAt         *time.Time      `json:"approved_at,omitempty"`
	FiledBy            *int64          `json:"filed_by,omitempty"`
	FiledAt            *time.Time      `json:"filed_at,omitempty"`
	ActivatedAt        *time.Time      `json:"activated_at,omitempty"`
	CreatedBy          int64           `json:"created_by"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// RuleNotice is one venue_rule_notices row (participant notice).
type RuleNotice struct {
	NoticeID   int64     `json:"notice_id"`
	RulebookID int64     `json:"rulebook_id"`
	Subject    string    `json:"subject"`
	BodyRef    string    `json:"body_ref,omitempty"`
	MemberID   *int64    `json:"member_id,omitempty"` // nil = broadcast
	IssuedBy   int64     `json:"issued_by"`
	IssuedAt   time.Time `json:"issued_at"`
}

// RuleAck is one venue_rule_acks row — member acknowledgement evidence.
type RuleAck struct {
	AckID          int64           `json:"ack_id"`
	RulebookID     int64           `json:"rulebook_id"`
	NoticeID       *int64          `json:"notice_id,omitempty"`
	MemberID       int64           `json:"member_id"`
	AcknowledgedBy string          `json:"acknowledged_by"`
	Evidence       json.RawMessage `json:"evidence"`
	AcknowledgedAt time.Time       `json:"acknowledged_at"`
	RecordedBy     int64           `json:"recorded_by"`
}

// DraftRulebook files a new versioned rulebook/product-terms draft.
// Idempotent on (kind, scope_key, version) — replay returns the stored
// row with created=false.
func (s *Service) DraftRulebook(ctx context.Context, kind, scopeKey,
	version, bodyRef string, requiresRegulator bool, noticeDays int,
	actor int64) (*Rulebook, bool, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, false, err
	}
	if kind != RuleKindRulebook && kind != RuleKindProductTerms {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"kind must be RULEBOOK|PRODUCT_TERMS")
	}
	if strings.TrimSpace(scopeKey) == "" {
		scopeKey = "VENUE"
	}
	if strings.TrimSpace(version) == "" || strings.TrimSpace(bodyRef) == "" {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"version and body_ref are required")
	}
	if noticeDays < 0 {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"notice_period_days must be >= 0")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO venue_rulebooks
		    (kind, scope_key, version, body_ref,
		     requires_regulator_approval, regulator_status,
		     notice_period_days, created_by)
		VALUES ($1,$2,$3,$4,$5,'NOT_REQUIRED',$6,$7)
		ON CONFLICT (kind, scope_key, version) DO NOTHING
		RETURNING rulebook_id`,
		kind, scopeKey, version, bodyRef, requiresRegulator,
		noticeDays, actor).Scan(&id)
	if err == pgx.ErrNoRows {
		existing, gerr := s.rulebookByKey(ctx, kind, scopeKey, version)
		if gerr != nil {
			return nil, false, gerr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "rulebook insert", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_rulebooks", &id,
		"RULEBOOK_DRAFT", nil); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "rulebook audit", err)
	}
	rb, err := s.GetRulebook(ctx, id)
	return rb, true, err
}

// FileToRegulator records the regulator filing (CFTC SEF rule
// filings / MiFID II competent-authority notifications). The row moves
// DRAFT → FILED with the filing ref; regulator_status → FILED.
func (s *Service) FileToRegulator(ctx context.Context, rulebookID int64,
	filingRef string, actor int64) (*Rulebook, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(filingRef) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "filing_ref required")
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE venue_rulebooks
		SET status='FILED', regulator_status='FILED',
		    regulator_filing_ref=$2, filed_by=$3, filed_at=now(),
		    updated_at=now()
		WHERE rulebook_id=$1 AND status='DRAFT'`,
		rulebookID, filingRef, actor)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rulebook file", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"rulebook not found or not in DRAFT status")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_rulebooks",
		&rulebookID, "RULEBOOK_FILED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rulebook audit", err)
	}
	return s.GetRulebook(ctx, rulebookID)
}

// RecordRegulatorDecision ingests the regulator verdict on a filed
// version — APPROVED (still needs venue approval) or REJECTED.
func (s *Service) RecordRegulatorDecision(ctx context.Context,
	rulebookID int64, approved bool, notes string,
	actor int64) (*Rulebook, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	status := RegStatusRejected
	if approved {
		status = RegStatusApproved
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE venue_rulebooks
		SET regulator_status=$2, detail = detail || $3::jsonb,
		    updated_at=now()
		WHERE rulebook_id=$1 AND status IN ('FILED','APPROVED')`,
		rulebookID, status,
		fmt.Sprintf(`{"regulator_decision":{"status":%q,"notes":%q,"at":%q}}`,
			status, notes, s.now().UTC().Format(time.RFC3339)))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR",
			"regulator decision update", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"rulebook not found or not awaiting a regulator decision")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_rulebooks",
		&rulebookID, "REGULATOR_DECISION", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rulebook audit", err)
	}
	return s.GetRulebook(ctx, rulebookID)
}

// Approve is the CCO/venue approval of a rulebook version —
// DRAFT|FILED → APPROVED. When the version requires regulator approval
// the filing must already be APPROVED (no venue activation ahead of
// the regulator).
func (s *Service) Approve(ctx context.Context, rulebookID int64,
	actor int64) (*Rulebook, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "approve tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status, regStatus string
	var reqReg bool
	err = tx.QueryRow(ctx, `
		SELECT status, regulator_status, requires_regulator_approval
		  FROM venue_rulebooks WHERE rulebook_id=$1 FOR UPDATE`,
		rulebookID).Scan(&status, &regStatus, &reqReg)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "rulebook not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "approve lock", err)
	}
	if status != RuleStatusDraft && status != RuleStatusFiled {
		return nil, excerrors.New("INVALID_REQUEST",
			"rulebook is "+status+" — only DRAFT|FILED can be approved")
	}
	if reqReg && regStatus != RegStatusApproved &&
		regStatus != RegStatusFiled {
		return nil, excerrors.New("INVALID_REQUEST",
			"rulebook requires regulator approval — file it first")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE venue_rulebooks
		SET status='APPROVED', approved_by=$2, approved_at=now(),
		    updated_at=now()
		WHERE rulebook_id=$1`, rulebookID, actor); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "approve update", err)
	}
	if _, err := audit.Append(ctx, tx, "venue_rulebooks", &rulebookID,
		"RULEBOOK_APPROVED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "approve audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "approve commit", err)
	}
	return s.GetRulebook(ctx, rulebookID)
}

// Activate makes an APPROVED version EFFECTIVE — refused
// (VENUE_RULEBOOK_NOT_APPROVED) while a required regulator approval
// stands absent, while a participant notice is still owed (a
// non-emergency change requires at least one issued notice), or before
// effective_from. The incumbent EFFECTIVE row for the same
// (kind, scope_key) supersedes in the same transaction. Emergency
// changes skip the notice gate but record the reason in detail.
func (s *Service) Activate(ctx context.Context, rulebookID int64,
	effectiveFrom time.Time, emergency bool, emergencyReason string,
	actor int64) (*Rulebook, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if effectiveFrom.IsZero() {
		effectiveFrom = now
	}
	if emergency && strings.TrimSpace(emergencyReason) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"emergency activation requires the reason")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "activate tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var rb Rulebook
	err = tx.QueryRow(ctx, `
		SELECT status, requires_regulator_approval, regulator_status,
		       notice_period_days, kind, scope_key
		  FROM venue_rulebooks WHERE rulebook_id=$1 FOR UPDATE`,
		rulebookID).Scan(&rb.Status, &rb.RequiresRegulator,
		&rb.RegulatorStatus, &rb.NoticePeriodDays, &rb.Kind, &rb.ScopeKey)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "rulebook not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "activate lock", err)
	}
	if rb.Status != RuleStatusApproved {
		return nil, excerrors.New(CodeRulebookNotApproved,
			"rulebook is "+rb.Status+" — only APPROVED versions activate")
	}
	if rb.RequiresRegulator && rb.RegulatorStatus != RegStatusApproved {
		return nil, excerrors.New(CodeRulebookNotApproved,
			"regulator approval required before activation (status "+
				rb.RegulatorStatus+")")
	}
	if effectiveFrom.Before(now) && !emergency {
		return nil, excerrors.New("INVALID_REQUEST",
			"effective_from is in the past — use emergency activation with reason")
	}
	if !emergency {
		var notices int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM venue_rule_notices WHERE rulebook_id=$1`,
			rulebookID).Scan(&notices); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "notice probe", err)
		}
		if notices == 0 {
			return nil, excerrors.New(CodeRulebookNotApproved,
				"participant notice required before activation — issue at "+
					"least one venue_rule_notices row")
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE venue_rulebooks
		SET status='SUPERSEDED', updated_at=now()
		WHERE kind=$2 AND scope_key=$3 AND status='EFFECTIVE'
		  AND rulebook_id <> $1`,
		rulebookID, rb.Kind, rb.ScopeKey); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "incumbent supersede", err)
	}
	det, _ := json.Marshal(map[string]any{"activation": map[string]any{
		"at": now.Format(time.RFC3339), "emergency": emergency,
		"emergency_reason": emergencyReason,
		"effective_from":   effectiveFrom.Format(time.RFC3339),
	}})
	if _, err := tx.Exec(ctx, `
		UPDATE venue_rulebooks
		SET status='EFFECTIVE', effective_from=$2, emergency=$3,
		    detail = detail || $4::jsonb, activated_at=now(), updated_at=now()
		WHERE rulebook_id=$1`,
		rulebookID, effectiveFrom, emergency, det); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "activate update", err)
	}
	if _, err := audit.Append(ctx, tx, "venue_rulebooks", &rulebookID,
		"RULEBOOK_EFFECTIVE", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "activate audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "activate commit", err)
	}
	return s.GetRulebook(ctx, rulebookID)
}

// IssueNotice publishes a participant notice for a rulebook version —
// member_id nil broadcasts to all participants.
func (s *Service) IssueNotice(ctx context.Context, rulebookID int64,
	memberID *int64, subject, bodyRef string, actor int64) (*RuleNotice, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(subject) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "subject required")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO venue_rule_notices
		    (rulebook_id, member_id, subject, body_ref, issued_by)
		VALUES ($1,$2,$3,NULLIF($4,''),$5) RETURNING notice_id`,
		rulebookID, memberID, subject, bodyRef, actor).Scan(&id)
	if err != nil {
		return nil, excerrors.Wrap("INVALID_REQUEST",
			"notice insert failed (rulebook/member id?)", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_rule_notices", &id,
		"RULE_NOTICE_ISSUED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "notice audit", err)
	}
	return &RuleNotice{NoticeID: id, RulebookID: rulebookID,
		MemberID: memberID, Subject: subject, BodyRef: bodyRef,
		IssuedBy: actor, IssuedAt: s.now().UTC()}, nil
}

// RecordAck files a member's acknowledgement of a rulebook/notice —
// idempotent on (rulebook_id, member_id, notice_id).
func (s *Service) RecordAck(ctx context.Context, rulebookID, memberID int64,
	noticeID *int64, signatory string, evidence json.RawMessage,
	actor int64) (*RuleAck, bool, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(signatory) == "" {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"acknowledged_by signatory required")
	}
	if len(evidence) == 0 {
		evidence = json.RawMessage("{}")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO venue_rule_acks
		    (rulebook_id, notice_id, member_id, acknowledged_by,
		     evidence, recorded_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (rulebook_id, member_id, notice_id) DO NOTHING
		RETURNING ack_id`,
		rulebookID, noticeID, memberID, signatory, evidence, actor).Scan(&id)
	if err == pgx.ErrNoRows {
		ack, gerr := s.getAck(ctx, rulebookID, memberID, noticeID)
		if gerr != nil {
			return nil, false, gerr
		}
		return ack, false, nil
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INVALID_REQUEST",
			"ack insert failed (rulebook/member id?)", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_rule_acks", &id,
		"RULE_ACK", nil); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "ack audit", err)
	}
	return &RuleAck{AckID: id, RulebookID: rulebookID, NoticeID: noticeID,
		MemberID: memberID, AcknowledgedBy: signatory, Evidence: evidence,
		AcknowledgedAt: s.now().UTC(), RecordedBy: actor}, true, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

const rulebookCols = `
	rulebook_id, kind, scope_key, version, body_ref, status,
	requires_regulator_approval, regulator_status, regulator_filing_ref,
	notice_period_days, emergency, detail, effective_from, approved_by,
	approved_at, filed_by, filed_at, activated_at, created_by,
	created_at, updated_at`

func scanRulebook(row interface{ Scan(dest ...any) error }) (*Rulebook, error) {
	var r Rulebook
	err := row.Scan(&r.RulebookID, &r.Kind, &r.ScopeKey, &r.Version,
		&r.BodyRef, &r.Status, &r.RequiresRegulator, &r.RegulatorStatus,
		&r.RegulatorFilingRef, &r.NoticePeriodDays, &r.Emergency, &r.Detail,
		&r.EffectiveFrom, &r.ApprovedBy, &r.ApprovedAt, &r.FiledBy,
		&r.FiledAt, &r.ActivatedAt, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	return &r, err
}

// GetRulebook reads one version row.
func (s *Service) GetRulebook(ctx context.Context, id int64) (*Rulebook, error) {
	rb, err := scanRulebook(s.pool.QueryRow(ctx,
		`SELECT `+rulebookCols+` FROM venue_rulebooks WHERE rulebook_id=$1`,
		id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "rulebook not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rulebook read", err)
	}
	return rb, nil
}

func (s *Service) rulebookByKey(ctx context.Context, kind, scope,
	version string) (*Rulebook, error) {
	rb, err := scanRulebook(s.pool.QueryRow(ctx,
		`SELECT `+rulebookCols+` FROM venue_rulebooks
		 WHERE kind=$1 AND scope_key=$2 AND version=$3`, kind, scope, version))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("INTERNAL_ERROR",
			"rulebook dedup conflict without stored row")
	}
	return rb, err
}

// ListRulebooks returns the versioned register (?kind=&status=).
func (s *Service) ListRulebooks(ctx context.Context, kind, status string,
	limit int) ([]Rulebook, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + rulebookCols + ` FROM venue_rulebooks WHERE true`
	args := []any{}
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(" AND kind=$%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status=$%d", len(args))
	}
	q += " ORDER BY kind, scope_key, version DESC LIMIT " + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rulebook list", err)
	}
	defer rows.Close()
	var out []Rulebook
	for rows.Next() {
		r, err := scanRulebook(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "rulebook scan", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListNotices returns notices for a rulebook version.
func (s *Service) ListNotices(ctx context.Context,
	rulebookID int64) ([]RuleNotice, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT notice_id, rulebook_id, subject, body_ref, member_id,
		       issued_by, issued_at
		  FROM venue_rule_notices
		 WHERE rulebook_id=$1 ORDER BY notice_id`, rulebookID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "notice list", err)
	}
	defer rows.Close()
	var out []RuleNotice
	for rows.Next() {
		var n RuleNotice
		var bodyRef *string
		if err := rows.Scan(&n.NoticeID, &n.RulebookID, &n.Subject,
			&bodyRef, &n.MemberID, &n.IssuedBy, &n.IssuedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "notice scan", err)
		}
		if bodyRef != nil {
			n.BodyRef = *bodyRef
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListAcks returns acknowledgement evidence for a rulebook version.
func (s *Service) ListAcks(ctx context.Context,
	rulebookID int64) ([]RuleAck, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ack_id, rulebook_id, notice_id, member_id, acknowledged_by,
		       evidence, acknowledged_at, recorded_by
		  FROM venue_rule_acks
		 WHERE rulebook_id=$1 ORDER BY ack_id`, rulebookID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "ack list", err)
	}
	defer rows.Close()
	var out []RuleAck
	for rows.Next() {
		var a RuleAck
		if err := rows.Scan(&a.AckID, &a.RulebookID, &a.NoticeID,
			&a.MemberID, &a.AcknowledgedBy, &a.Evidence, &a.AcknowledgedAt,
			&a.RecordedBy); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "ack scan", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) getAck(ctx context.Context, rulebookID, memberID int64,
	noticeID *int64) (*RuleAck, error) {
	var a RuleAck
	var err error
	if noticeID == nil {
		err = s.pool.QueryRow(ctx, `
			SELECT ack_id, rulebook_id, notice_id, member_id, acknowledged_by,
			       evidence, acknowledged_at, recorded_by
			  FROM venue_rule_acks
			 WHERE rulebook_id=$1 AND member_id=$2 AND notice_id IS NULL`,
			rulebookID, memberID).Scan(&a.AckID, &a.RulebookID, &a.NoticeID,
			&a.MemberID, &a.AcknowledgedBy, &a.Evidence, &a.AcknowledgedAt,
			&a.RecordedBy)
	} else {
		err = s.pool.QueryRow(ctx, `
			SELECT ack_id, rulebook_id, notice_id, member_id, acknowledged_by,
			       evidence, acknowledged_at, recorded_by
			  FROM venue_rule_acks
			 WHERE rulebook_id=$1 AND member_id=$2 AND notice_id=$3`,
			rulebookID, memberID, *noticeID).Scan(&a.AckID, &a.RulebookID,
			&a.NoticeID, &a.MemberID, &a.AcknowledgedBy, &a.Evidence,
			&a.AcknowledgedAt, &a.RecordedBy)
	}
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("INTERNAL_ERROR",
			"ack dedup conflict without stored row")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "ack read", err)
	}
	return &a, nil
}
