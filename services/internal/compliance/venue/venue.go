// Package venue is the Phase-21 Task 21.3.15 regulated-venue governance
// layer (spec §5.32/§14.1b; §24 #174; §27.1 Regulated Venue License
// Governance matrix row): member/DEA/sponsored-access admission and
// annual review, versioned rulebook/product governance, market-control
// and emergency records, investigation/disciplinary workflow, conflicts
// register, annual system-safeguard self-assessment, the CCO annual
// report and the launch-prerequisite gate.
//
// The member register row lives in venue_members (migration 054 —
// spec §5.32 pins it there); migration 250 adds the termination and
// jurisdiction columns plus every satellite table. Member state
// transitions always write three things in one transaction: the
// register row, an append-only venue_member_events ledger row and an
// audit_hash_chain entry — the "suspension/termination and appeals"
// evidence trail the AC requires.
//
// Admission contract (AC #1): a member/DEA/sponsored client may not
// trade before due diligence COMPLETED + >= 1 recorded agreement +
// admission APPROVED + product/port approval — enforced by
// CheckTradingAccess behind the AdmissionGate seam (wrapped inside the
// order-submission product-gate chain). reduceOnly flow bypasses like
// every exposure-increasing gate; probe failures fail closed
// (SERVICE_DEGRADED), never silently admit.
//
// Jurisdiction axis: members carry venue_members.jurisdiction; when a
// required venue_launch_prerequisites LICENSE/REGULATOR_AUTH row exists
// for that jurisdiction and is not EVIDENCED (or is expired), member
// trading rejects with JURISDICTION_UNLICENSED (§27.1 matrix code).
//
// Role gates: mutations require Compliance Officer or Super Admin via
// the resolver (UNAUTHORIZED_ROLE otherwise); reads are handler-gated
// to the Read-Only Auditor surface.
package venue

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/internal/compliance"
	excerrors "exchange/pkg/errors"
)

// Member access models / statuses (migration 054 CHECK mirror + the
// workflow vocabulary this task adds).
const (
	AccessMember    = "MEMBER"
	AccessDEA       = "DEA"
	AccessSponsored = "SPONSORED"

	AdmissionPending  = "PENDING"
	AdmissionApproved = "APPROVED"
	AdmissionDenied   = "DENIED"

	DDPending    = "PENDING"
	DDInProgress = "IN_PROGRESS"
	DDCompleted  = "COMPLETED"
	DDRejected   = "REJECTED"
)

// venue_member_events event_type vocabulary (migration 250 CHECK mirror).
const (
	EventRegistered     = "REGISTERED"
	EventDDUpdate       = "DD_UPDATE"
	EventAgreement      = "AGREEMENT"
	EventProducts       = "PRODUCTS"
	EventAdmission      = "ADMISSION"
	EventSuspend        = "SUSPEND"
	EventReinstate      = "REINSTATE"
	EventTerminate      = "TERMINATE"
	EventAppeal         = "APPEAL"
	EventAppealDecision = "APPEAL_DECISION"
	EventReview         = "REVIEW"
)

// Alert codes — §27.1 Regulated Venue License Governance matrix codes.
// Both ride ops alerts / payload `code` fields; they are registered in
// errs localCodes with this task as owner.
const (
	CodeJurisdictionUnlicensed   = "JURISDICTION_UNLICENSED"
	CodeAnnualAttestationOverdue = "ANNUAL_ATTESTATION_OVERDUE"
	// CodeRulebookNotApproved is the 409 on EFFECTIVE activation before
	// the required approvals stand (AC: "no activation before required
	// approvals").
	CodeRulebookNotApproved = "VENUE_RULEBOOK_NOT_APPROVED"
)

// Member is one venue_members row (migration 054 + the 250 extensions).
type Member struct {
	MemberID             int64           `json:"member_id"`
	LegalName            string          `json:"legal_name"`
	LEI                  string          `json:"lei"`
	RegulatoryStatus     string          `json:"regulatory_status"`
	AccessModel          string          `json:"access_model"`
	ApprovedProducts     []string        `json:"approved_products"`
	ApprovedPorts        []string        `json:"approved_ports"`
	DueDiligenceStatus   string          `json:"due_diligence_status"`
	DueDiligenceEvidence json.RawMessage `json:"due_diligence_evidence"`
	AdmissionDecision    string          `json:"admission_decision"`
	AnnualReviewDue      *time.Time      `json:"annual_review_due,omitempty"`
	Suspended            bool            `json:"suspended"`
	SuspendedAt          *time.Time      `json:"suspended_at,omitempty"`
	SuspensionReason     string          `json:"suspension_reason,omitempty"`
	Agreements           json.RawMessage `json:"agreements"`
	Jurisdiction         string          `json:"jurisdiction,omitempty"`
	TerminatedAt         *time.Time      `json:"terminated_at,omitempty"`
	TerminatedBy         *int64          `json:"terminated_by,omitempty"`
	TerminationReason    string          `json:"termination_reason,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// MemberEvent is one append-only venue_member_events row.
type MemberEvent struct {
	EventID   int64           `json:"event_id"`
	MemberID  int64           `json:"member_id"`
	EventType string          `json:"event_type"`
	Detail    json.RawMessage `json:"detail"`
	Actor     *int64          `json:"actor,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// MemberReview is one venue_member_reviews row.
type MemberReview struct {
	ReviewID      int64           `json:"review_id"`
	MemberID      int64           `json:"member_id"`
	ReviewType    string          `json:"review_type"`
	Outcome       string          `json:"outcome"`
	Findings      json.RawMessage `json:"findings"`
	NextReviewDue time.Time       `json:"next_review_due"`
	Reviewer      int64           `json:"reviewer"`
	ReviewedAt    time.Time       `json:"reviewed_at"`
}

// Service owns the venue-governance workflows.
type Service struct {
	pool     *pgxpool.Pool
	resolver compliance.HoldRoleResolver
	alerter  compliance.HoldAlerter
	now      func() time.Time
}

// NewService wires the service; pool + resolver are required (a nil
// resolver would make every role check guess — fail closed instead).
func NewService(pool *pgxpool.Pool,
	resolver compliance.HoldRoleResolver) (*Service, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"venue service requires pool")
	}
	if resolver == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	return &Service{pool: pool, resolver: resolver, now: time.Now}, nil
}

// WithAlerter wires the ops channel for P1 pages (overdue reviews,
// lapsed licenses, overdue attestations).
func (s *Service) WithAlerter(a compliance.HoldAlerter) *Service {
	s.alerter = a
	return s
}

// WithClock overrides the clock (tests).
func (s *Service) WithClock(c func() time.Time) *Service {
	s.now = c
	return s
}

// checkRole gates mutations to Compliance Officer / Super Admin.
func (s *Service) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot mutate venue governance records")
	}
	return nil
}

// auditEvent appends a venue_member_events row + audit_hash_chain entry
// inside tx — every member mutation carries both.
func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, memberID int64,
	eventType string, detail map[string]any, actor int64) error {
	raw, _ := json.Marshal(detail)
	var eventID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO venue_member_events (member_id, event_type, detail, actor)
		VALUES ($1,$2,$3,NULLIF($4,0)::bigint) RETURNING event_id`,
		memberID, eventType, raw, actor).Scan(&eventID); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "member event append", err)
	}
	if _, err := audit.Append(ctx, tx, "venue_member_events", &eventID,
		eventType, nil); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "member event audit", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Member register
// ---------------------------------------------------------------------------

// MemberInput is the admission dossier for a new member/DEA/sponsored
// applicant.
type MemberInput struct {
	LegalName        string
	LEI              string
	AccessModel      string // MEMBER|DEA|SPONSORED
	RegulatoryStatus string
	Jurisdiction     string // optional — drives the license prereq gate
}

// validate checks the admission dossier (LEI is ISO 17442
// checksum-validated — the register never stores a malformed LEI).
func (in MemberInput) validate() error {
	if strings.TrimSpace(in.LegalName) == "" {
		return excerrors.New("INVALID_REQUEST", "legal_name required")
	}
	if err := compliance.ValidateLEI(strings.ToUpper(
		strings.TrimSpace(in.LEI))); err != nil {
		return excerrors.New("INVALID_REQUEST", "lei: "+err.Error())
	}
	switch in.AccessModel {
	case AccessMember, AccessDEA, AccessSponsored:
	default:
		return excerrors.New("INVALID_REQUEST",
			"access_model must be MEMBER|DEA|SPONSORED")
	}
	return nil
}

// RegisterMember files a new member application. Idempotent on LEI —
// the replayed call returns the stored row with created=false.
func (s *Service) RegisterMember(ctx context.Context, in MemberInput,
	actor int64) (*Member, bool, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, false, err
	}
	in.LEI = strings.ToUpper(strings.TrimSpace(in.LEI))
	if err := in.validate(); err != nil {
		return nil, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "member tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO venue_members (legal_name, lei, regulatory_status,
		    access_model, jurisdiction)
		VALUES ($1,$2,COALESCE(NULLIF($3,''),'PENDING'),$4,NULLIF($5,''))
		ON CONFLICT (lei) DO NOTHING
		RETURNING member_id`,
		in.LegalName, in.LEI, in.RegulatoryStatus, in.AccessModel,
		strings.ToUpper(strings.TrimSpace(in.Jurisdiction))).Scan(&id)
	if err == pgx.ErrNoRows {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, excerrors.Wrap("INTERNAL_ERROR",
				"member dedup commit", err)
		}
		existing, gerr := s.MemberByLEI(ctx, in.LEI)
		if gerr != nil {
			return nil, false, gerr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "member insert", err)
	}
	if err := s.auditEvent(ctx, tx, id, EventRegistered, map[string]any{
		"legal_name": in.LegalName, "lei": in.LEI,
		"access_model": in.AccessModel, "jurisdiction": in.Jurisdiction,
	}, actor); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "member commit", err)
	}
	m, err := s.GetMember(ctx, id)
	return m, true, err
}

// RecordDueDiligence moves the due-diligence status with its evidence
// (docs refs, screening outcomes). COMPLETED is the admission gate.
func (s *Service) RecordDueDiligence(ctx context.Context, memberID int64,
	status string, evidence json.RawMessage, actor int64) (*Member, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	switch status {
	case DDPending, DDInProgress, DDCompleted, DDRejected:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"status must be PENDING|IN_PROGRESS|COMPLETED|REJECTED")
	}
	if len(evidence) == 0 {
		evidence = json.RawMessage("{}")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "dd tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := tx.Exec(ctx, `
		UPDATE venue_members
		SET due_diligence_status=$2, due_diligence_evidence=$3,
		    updated_at=now()
		WHERE member_id=$1`, memberID, status, evidence)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "dd update", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("NOT_FOUND", "venue member not found")
	}
	if err := s.auditEvent(ctx, tx, memberID, EventDDUpdate, map[string]any{
		"status":   status,
		"evidence": json.RawMessage(evidence),
	}, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "dd commit", err)
	}
	return s.GetMember(ctx, memberID)
}

// AttachAgreement registers an executed agreement (membership, DEA,
// sponsored-access, give-up) — at least one is required before
// admission may be APPROVED.
func (s *Service) AttachAgreement(ctx context.Context, memberID int64,
	kind, ref string, actor int64) (*Member, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(kind) == "" || strings.TrimSpace(ref) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"agreement kind and ref are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "agreement tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	entry, _ := json.Marshal(map[string]string{
		"kind": kind, "ref": ref,
		"recorded_at": s.now().UTC().Format(time.RFC3339),
	})
	res, err := tx.Exec(ctx, `
		UPDATE venue_members
		SET agreements = agreements || $2::jsonb, updated_at=now()
		WHERE member_id=$1`, memberID,
		"["+string(entry)+"]")
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "agreement update", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("NOT_FOUND", "venue member not found")
	}
	if err := s.auditEvent(ctx, tx, memberID, EventAgreement,
		map[string]any{"kind": kind, "ref": ref}, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "agreement commit", err)
	}
	return s.GetMember(ctx, memberID)
}

// ApproveProducts sets the member's approved product/port set —
// admission and the trading gate read these (empty = nothing approved).
func (s *Service) ApproveProducts(ctx context.Context, memberID int64,
	products, ports []string, actor int64) (*Member, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if products == nil {
		products = []string{}
	}
	if ports == nil {
		ports = []string{}
	}
	prow, _ := json.Marshal(products)
	prw, _ := json.Marshal(ports)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "products tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := tx.Exec(ctx, `
		UPDATE venue_members
		SET approved_products=$2, approved_ports=$3, updated_at=now()
		WHERE member_id=$1`, memberID, prow, prw)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "products update", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("NOT_FOUND", "venue member not found")
	}
	if err := s.auditEvent(ctx, tx, memberID, EventProducts, map[string]any{
		"products": products, "ports": ports,
	}, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "products commit", err)
	}
	return s.GetMember(ctx, memberID)
}

// Decide records the admission decision. APPROVED is refused until due
// diligence is COMPLETED and at least one agreement is recorded —
// fail-closed: the AC "no member trades before due diligence,
// agreements and approval" is enforced at decision AND again at the
// trading gate.
func (s *Service) Decide(ctx context.Context, memberID int64,
	approve bool, reason string, reviewDue time.Time,
	actor int64) (*Member, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "decision tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var dd string
	var agreements json.RawMessage
	var current string
	err = tx.QueryRow(ctx, `
		SELECT due_diligence_status, agreements, admission_decision
		  FROM venue_members WHERE member_id=$1 FOR UPDATE`,
		memberID).Scan(&dd, &agreements, &current)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "venue member not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "decision lock", err)
	}
	decision := AdmissionDenied
	if approve {
		decision = AdmissionApproved
		if dd != DDCompleted {
			return nil, excerrors.New("FORBIDDEN",
				"admission requires COMPLETED due diligence (got "+dd+")")
		}
		var aggs []json.RawMessage
		_ = json.Unmarshal(agreements, &aggs)
		if len(aggs) == 0 {
			return nil, excerrors.New("FORBIDDEN",
				"admission requires at least one executed agreement")
		}
		if reviewDue.IsZero() {
			return nil, excerrors.New("INVALID_REQUEST",
				"annual_review_due is required on admission")
		}
	}
	var rd *time.Time
	if !reviewDue.IsZero() {
		rd = &reviewDue
	}
	if _, err := tx.Exec(ctx, `
		UPDATE venue_members
		SET admission_decision=$2, annual_review_due=$3, updated_at=now()
		WHERE member_id=$1`, memberID, decision, rd); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "decision update", err)
	}
	if err := s.auditEvent(ctx, tx, memberID, EventAdmission, map[string]any{
		"decision": decision, "reason": reason,
		"annual_review_due": reviewDue.Format("2006-01-02"),
	}, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "decision commit", err)
	}
	return s.GetMember(ctx, memberID)
}

// Suspend halts the member's trading access (register row + evidence).
func (s *Service) Suspend(ctx context.Context, memberID int64,
	reason string, actor int64) (*Member, error) {
	return s.setSuspended(ctx, memberID, true, reason, actor)
}

// Reinstate restores a suspended member (suspension evidence retained).
func (s *Service) Reinstate(ctx context.Context, memberID int64,
	reason string, actor int64) (*Member, error) {
	return s.setSuspended(ctx, memberID, false, reason, actor)
}

func (s *Service) setSuspended(ctx context.Context, memberID int64,
	suspend bool, reason string, actor int64) (*Member, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "reason required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "suspend tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var res pgconn.CommandTag
	evt := EventReinstate
	if suspend {
		evt = EventSuspend
		res, err = tx.Exec(ctx, `
			UPDATE venue_members
			SET suspended=true, suspended_at=now(), suspension_reason=$2,
			    updated_at=now()
			WHERE member_id=$1`, memberID, reason)
	} else {
		res, err = tx.Exec(ctx, `
			UPDATE venue_members
			SET suspended=false, suspended_at=NULL,
			    suspension_reason=NULL, updated_at=now()
			WHERE member_id=$1`, memberID)
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "suspend update", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("NOT_FOUND", "venue member not found")
	}
	if err := s.auditEvent(ctx, tx, memberID, evt,
		map[string]any{"reason": reason}, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "suspend commit", err)
	}
	return s.GetMember(ctx, memberID)
}

// Terminate ends membership — the row is retained (5y record-keeping)
// with terminated_at set; a terminated member never trades again.
func (s *Service) Terminate(ctx context.Context, memberID int64,
	reason string, actor int64) (*Member, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "reason required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "terminate tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := tx.Exec(ctx, `
		UPDATE venue_members
		SET terminated_at=now(), terminated_by=$2, termination_reason=$3,
		    admission_decision='DENIED', updated_at=now()
		WHERE member_id=$1 AND terminated_at IS NULL`,
		memberID, actor, reason)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "terminate update", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"member not found or already terminated")
	}
	if err := s.auditEvent(ctx, tx, memberID, EventTerminate,
		map[string]any{"reason": reason}, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "terminate commit", err)
	}
	return s.GetMember(ctx, memberID)
}

// Appeal records a member appeal against a suspension/termination —
// the appeal and its decision are ledger rows, never edits.
func (s *Service) Appeal(ctx context.Context, memberID int64,
	grounds string, actor int64) (*MemberEvent, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(grounds) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "grounds required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "appeal tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.auditEvent(ctx, tx, memberID, EventAppeal,
		map[string]any{"grounds": grounds}, actor); err != nil {
		return nil, err
	}
	var ev MemberEvent
	if err := tx.QueryRow(ctx, `
		SELECT event_id, member_id, event_type, detail, actor, created_at
		  FROM venue_member_events
		 WHERE member_id=$1 AND event_type='APPEAL'
		 ORDER BY event_id DESC LIMIT 1`, memberID).
		Scan(&ev.EventID, &ev.MemberID, &ev.EventType, &ev.Detail,
			&ev.Actor, &ev.CreatedAt); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "appeal readback", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "appeal commit", err)
	}
	return &ev, nil
}

// DecideAppeal records the appeal outcome — UPHELD reinstates a
// suspended member (never a terminated one: termination stands pending
// re-admission); REJECTED records the refusal.
func (s *Service) DecideAppeal(ctx context.Context, memberID int64,
	uphold bool, rationale string, actor int64) (*Member, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(rationale) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "rationale required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "appeal decision tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var suspended bool
	var terminated *time.Time
	err = tx.QueryRow(ctx, `
		SELECT suspended, terminated_at FROM venue_members
		 WHERE member_id=$1 FOR UPDATE`, memberID).Scan(&suspended, &terminated)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "venue member not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "appeal lock", err)
	}
	if uphold && terminated != nil {
		return nil, excerrors.New("INVALID_REQUEST",
			"a terminated membership is not reinstated by appeal — "+
				"the member must re-apply")
	}
	if uphold && suspended {
		if _, err := tx.Exec(ctx, `
			UPDATE venue_members
			SET suspended=false, suspended_at=NULL, suspension_reason=NULL,
			    updated_at=now()
			WHERE member_id=$1`, memberID); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "appeal reinstate", err)
		}
	}
	outcome := "REJECTED"
	if uphold {
		outcome = "UPHELD"
	}
	if err := s.auditEvent(ctx, tx, memberID, EventAppealDecision,
		map[string]any{"outcome": outcome, "rationale": rationale},
		actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "appeal commit", err)
	}
	return s.GetMember(ctx, memberID)
}

// Review files the annual/ad-hoc member risk review and rolls
// annual_review_due forward — a FAIL outcome also suspends trading
// (the review verdict is enforced, not advisory).
func (s *Service) Review(ctx context.Context, memberID int64,
	reviewType, outcome string, findings json.RawMessage,
	nextDue time.Time, actor int64) (*MemberReview, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if reviewType == "" {
		reviewType = "ANNUAL"
	}
	if reviewType != "ANNUAL" && reviewType != "AD_HOC" {
		return nil, excerrors.New("INVALID_REQUEST",
			"review_type must be ANNUAL|AD_HOC")
	}
	switch outcome {
	case "PASS", "CONDITIONAL", "FAIL":
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"outcome must be PASS|CONDITIONAL|FAIL")
	}
	if nextDue.IsZero() {
		return nil, excerrors.New("INVALID_REQUEST",
			"next_review_due required")
	}
	if len(findings) == 0 {
		findings = json.RawMessage("{}")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "review tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var reviewID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO venue_member_reviews
		    (member_id, review_type, outcome, findings, next_review_due,
		     reviewer)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING review_id`,
		memberID, reviewType, outcome, findings, nextDue, actor).
		Scan(&reviewID)
	if err != nil {
		return nil, excerrors.Wrap("INVALID_REQUEST",
			"review insert failed (member id?)", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE venue_members SET annual_review_due=$2, updated_at=now()
		WHERE member_id=$1`, memberID, nextDue); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "review due update", err)
	}
	if outcome == "FAIL" {
		if _, err := tx.Exec(ctx, `
			UPDATE venue_members
			SET suspended=true, suspended_at=now(),
			    suspension_reason='annual review outcome FAIL', updated_at=now()
			WHERE member_id=$1`, memberID); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "review suspend", err)
		}
	}
	if err := s.auditEvent(ctx, tx, memberID, EventReview, map[string]any{
		"review_id": reviewID, "review_type": reviewType, "outcome": outcome,
	}, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "review commit", err)
	}
	return &MemberReview{ReviewID: reviewID, MemberID: memberID,
		ReviewType: reviewType, Outcome: outcome, Findings: findings,
		NextReviewDue: nextDue, Reviewer: actor,
		ReviewedAt: s.now().UTC()}, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

const memberCols = `
	member_id, legal_name, lei, regulatory_status, access_model,
	approved_products, approved_ports, due_diligence_status,
	due_diligence_evidence, admission_decision, annual_review_due,
	suspended, suspended_at, suspension_reason, agreements,
	jurisdiction, terminated_at, terminated_by, termination_reason,
	created_at, updated_at`

func scanMember(row interface{ Scan(dest ...any) error }) (*Member, error) {
	var m Member
	var products, ports []byte
	var jur, susReason, termReason *string
	err := row.Scan(&m.MemberID, &m.LegalName, &m.LEI, &m.RegulatoryStatus,
		&m.AccessModel, &products, &ports, &m.DueDiligenceStatus,
		&m.DueDiligenceEvidence, &m.AdmissionDecision, &m.AnnualReviewDue,
		&m.Suspended, &m.SuspendedAt, &susReason, &m.Agreements,
		&jur, &m.TerminatedAt, &m.TerminatedBy, &termReason,
		&m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	m.ApprovedProducts = []string{}
	m.ApprovedPorts = []string{}
	_ = json.Unmarshal(products, &m.ApprovedProducts)
	_ = json.Unmarshal(ports, &m.ApprovedPorts)
	if jur != nil {
		m.Jurisdiction = *jur
	}
	if susReason != nil {
		m.SuspensionReason = *susReason
	}
	if termReason != nil {
		m.TerminationReason = *termReason
	}
	return &m, nil
}

// GetMember reads one register row.
func (s *Service) GetMember(ctx context.Context, memberID int64) (*Member, error) {
	m, err := scanMember(s.pool.QueryRow(ctx,
		`SELECT `+memberCols+` FROM venue_members WHERE member_id=$1`,
		memberID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "venue member not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "member read", err)
	}
	return m, nil
}

// MemberByLEI reads one register row by LEI.
func (s *Service) MemberByLEI(ctx context.Context, lei string) (*Member, error) {
	m, err := scanMember(s.pool.QueryRow(ctx,
		`SELECT `+memberCols+` FROM venue_members WHERE lei=$1`,
		strings.ToUpper(strings.TrimSpace(lei))))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "venue member not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "member read", err)
	}
	return m, nil
}

// ListMembers returns the register (audit surface), optionally filtered.
func (s *Service) ListMembers(ctx context.Context, accessModel string,
	limit int) ([]Member, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var rows pgx.Rows
	var err error
	if accessModel != "" {
		rows, err = s.pool.Query(ctx,
			`SELECT `+memberCols+` FROM venue_members
			 WHERE access_model=$1 ORDER BY member_id LIMIT `+
				fmt.Sprint(limit), accessModel)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT `+memberCols+` FROM venue_members
			 ORDER BY member_id LIMIT `+fmt.Sprint(limit))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "member list", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "member scan", err)
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// MemberEvents returns the immutable lifecycle ledger for a member.
func (s *Service) MemberEvents(ctx context.Context, memberID int64,
	limit int) ([]MemberEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT event_id, member_id, event_type, detail, actor, created_at
		  FROM venue_member_events
		 WHERE member_id=$1 ORDER BY event_id LIMIT `+fmt.Sprint(limit),
		memberID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "member events", err)
	}
	defer rows.Close()
	var out []MemberEvent
	for rows.Next() {
		var e MemberEvent
		if err := rows.Scan(&e.EventID, &e.MemberID, &e.EventType,
			&e.Detail, &e.Actor, &e.CreatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "event scan", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MemberReviews returns the review history for a member.
func (s *Service) MemberReviews(ctx context.Context,
	memberID int64) ([]MemberReview, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT review_id, member_id, review_type, outcome, findings,
		       next_review_due, reviewer, reviewed_at
		  FROM venue_member_reviews
		 WHERE member_id=$1 ORDER BY review_id`, memberID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "member reviews", err)
	}
	defer rows.Close()
	var out []MemberReview
	for rows.Next() {
		var r MemberReview
		if err := rows.Scan(&r.ReviewID, &r.MemberID, &r.ReviewType,
			&r.Outcome, &r.Findings, &r.NextReviewDue, &r.Reviewer,
			&r.ReviewedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "review scan", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Trading admission gate (AC #1)
// ---------------------------------------------------------------------------

// CheckTradingAccess is the member/DEA/sponsored admission check the
// order-submission product-gate chain consults. An account that
// resolves to no venue member (party_identifiers has no LEI) is a
// client account, not a member — admitted by this gate (member rules
// do not apply). A member account must hold: APPROVED admission +
// COMPLETED due diligence + >= 1 agreement + not suspended + not
// terminated + annual review in date + instrument product type in
// approved_products (when a list is configured) + jurisdiction license
// prerequisites evidenced. Probe failures fail closed.
func (s *Service) CheckTradingAccess(ctx context.Context, accountID int64,
	productType string) error {
	var lei *string
	if err := s.pool.QueryRow(ctx,
		`SELECT lei FROM party_identifiers WHERE account_id=$1`,
		accountID).Scan(&lei); err != nil && err != pgx.ErrNoRows {
		return excerrors.Wrap("SERVICE_DEGRADED",
			"member registry probe failed — order rejected (fail closed)", err)
	}
	if lei == nil {
		return nil // not a venue member — member rules do not apply
	}
	m, err := scanMember(s.pool.QueryRow(ctx,
		`SELECT `+memberCols+` FROM venue_members WHERE lei=$1`, *lei))
	if err == pgx.ErrNoRows {
		// An LEI on file without a register row = unadmitted member.
		return excerrors.New("FORBIDDEN",
			"account holds a party LEI with no venue member admission — "+
				"member/DEA access requires the Task 21.3.15 register")
	}
	if err != nil {
		return excerrors.Wrap("SERVICE_DEGRADED",
			"member register read failed — order rejected (fail closed)", err)
	}
	if m.TerminatedAt != nil {
		return excerrors.New("FORBIDDEN",
			"venue membership terminated — trading access closed")
	}
	if m.Suspended {
		return excerrors.New("FORBIDDEN",
			"venue member suspended ("+m.SuspensionReason+")")
	}
	if m.AdmissionDecision != AdmissionApproved ||
		m.DueDiligenceStatus != DDCompleted {
		return excerrors.New("FORBIDDEN",
			"venue member not admitted (admission="+m.AdmissionDecision+
				", due_diligence="+m.DueDiligenceStatus+")")
	}
	var aggs []json.RawMessage
	_ = json.Unmarshal(m.Agreements, &aggs)
	if len(aggs) == 0 {
		return excerrors.New("FORBIDDEN",
			"venue member has no executed agreement on file")
	}
	if m.AnnualReviewDue != nil {
		today := s.now().UTC()
		if m.AnnualReviewDue.Before(time.Date(today.Year(), today.Month(),
			today.Day(), 0, 0, 0, 0, time.UTC)) {
			return excerrors.New("FORBIDDEN",
				"venue member annual review is overdue — trading suspended "+
					"pending review")
		}
	}
	if len(m.ApprovedProducts) > 0 {
		ok := false
		for _, p := range m.ApprovedProducts {
			if strings.EqualFold(p, productType) {
				ok = true
				break
			}
		}
		if !ok {
			return excerrors.New("PRODUCT_NOT_PERMITTED",
				"product type "+productType+
					" is not in the member's approved product set")
		}
	}
	// Jurisdictional licensing: a required LICENSE/REGULATOR_AUTH
	// prerequisite scoped to the member's jurisdiction must be
	// EVIDENCED and unexpired — a missing/expired one rejects with the
	// §27.1 matrix code.
	if m.Jurisdiction != "" {
		var missing bool
		err := s.pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM venue_launch_prerequisites
				WHERE scope=$1 AND required
				  AND kind IN ('LICENSE','REGULATOR_AUTH')
				  AND (status <> 'EVIDENCED'
				       OR (expires_at IS NOT NULL AND expires_at < now())))`,
			m.Jurisdiction).Scan(&missing)
		if err != nil {
			return excerrors.Wrap("SERVICE_DEGRADED",
				"jurisdiction license probe failed — order rejected (fail closed)",
				err)
		}
		if missing {
			return excerrors.New(CodeJurisdictionUnlicensed,
				"venue authorization for jurisdiction "+m.Jurisdiction+
					" is absent or expired — member trading blocked")
		}
	}
	return nil
}

// AdmissionGate composes the member trading check behind the inner
// product-gate chain — it satisfies the orders.ProductGate seam
// (AdmitOrder shape) without importing orders. reduceOnly bypasses like
// every exposure-increasing gate (close-only posture).
type AdmissionGate struct {
	Inner interface {
		AdmitOrder(ctx context.Context, accountID int64,
			symbol, instrumentClass string, reduceOnly bool) error
	}
	Members *Service
}

// AdmitOrder runs the member access check (suspended/expelled members
// are blocked even on reduce-only closes — the venue obligation
// outranks the close-only posture) then the inner gate.
func (g *AdmissionGate) AdmitOrder(ctx context.Context, accountID int64,
	symbol, instrumentClass string, reduceOnly bool) error {
	if g.Members == nil {
		return excerrors.New("SERVICE_DEGRADED",
			"venue member gate unavailable — order rejected (fail closed)")
	}
	if err := g.Members.CheckTradingAccess(ctx, accountID,
		instrumentClass); err != nil {
		return err
	}
	if g.Inner != nil {
		return g.Inner.AdmitOrder(ctx, accountID, symbol,
			instrumentClass, reduceOnly)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Launch prerequisites + launch gate (AC #5)
// ---------------------------------------------------------------------------

// Prereq kinds (migration 250 CHECK mirror).
const (
	PrereqLicense          = "LICENSE"
	PrereqRegulatorAuth    = "REGULATOR_AUTH"
	PrereqLegalOpinion     = "LEGAL_OPINION"
	PrereqBoardAppointment = "BOARD_APPOINTMENT"
	PrereqCCOAppointment   = "CCO_APPOINTMENT"
	PrereqMinFinancialRes  = "MIN_FINANCIAL_RESOURCES"
)

// Prerequisite is one venue_launch_prerequisites row.
type Prerequisite struct {
	PrereqID    int64      `json:"prereq_id"`
	Kind        string     `json:"kind"`
	Scope       string     `json:"scope"`
	Required    bool       `json:"required"`
	Status      string     `json:"status"`
	Description string     `json:"description"`
	EvidenceRef string     `json:"evidence_ref,omitempty"`
	EvidencedBy *int64     `json:"evidenced_by,omitempty"`
	EvidencedAt *time.Time `json:"evidenced_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedBy   int64      `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// LaunchGateReport is the launch-gate evaluation — ready only when
// every required GLOBAL prerequisite is EVIDENCED and unexpired.
type LaunchGateReport struct {
	Ready       bool           `json:"ready"`
	Missing     []Prerequisite `json:"missing"`
	EvaluatedAt time.Time      `json:"evaluated_at"`
}

// EvidencePrerequisite marks one prerequisite EVIDENCED with its
// artifact ref (or registers a new scoped row). Idempotent on
// (kind, scope) — re-evidencing updates the same row and is audited.
func (s *Service) EvidencePrerequisite(ctx context.Context, kind, scope,
	description, evidenceRef string, expiresAt *time.Time,
	actor int64) (*Prerequisite, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	switch kind {
	case PrereqLicense, PrereqRegulatorAuth, PrereqLegalOpinion,
		PrereqBoardAppointment, PrereqCCOAppointment, PrereqMinFinancialRes:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"unknown prerequisite kind "+kind)
	}
	scope = strings.ToUpper(strings.TrimSpace(scope))
	if scope == "" {
		scope = "GLOBAL"
	}
	if strings.TrimSpace(evidenceRef) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"evidence_ref is required — prerequisites are evidenced, not claimed")
	}
	if strings.TrimSpace(description) == "" {
		description = kind
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO venue_launch_prerequisites
		    (kind, scope, required, status, description, evidence_ref,
		     evidenced_by, evidenced_at, expires_at, created_by)
		VALUES ($1,$2,true,'EVIDENCED',$3,$4,$5,now(),$6,$7)
		ON CONFLICT (kind, scope) DO UPDATE
		SET status='EVIDENCED', evidence_ref=EXCLUDED.evidence_ref,
		    evidenced_by=EXCLUDED.evidenced_by, evidenced_at=now(),
		    expires_at=EXCLUDED.expires_at, description=EXCLUDED.description,
		    updated_at=now()
		RETURNING prereq_id`,
		kind, scope, description, evidenceRef, actor, expiresAt, actor).
		Scan(&id)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "prerequisite upsert", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_launch_prerequisites",
		&id, "PREREQ_EVIDENCED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "prerequisite audit", err)
	}
	return s.getPrereq(ctx, id)
}

// MarkPrereqExpired retires an evidence row whose authorization lapsed
// (license lapse intraday — SDD edge case: the member-trading gate
// starts rejecting within one read).
func (s *Service) MarkPrereqExpired(ctx context.Context, prereqID int64,
	actor int64) (*Prerequisite, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE venue_launch_prerequisites
		SET status='EXPIRED', updated_at=now()
		WHERE prereq_id=$1`, prereqID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "prereq expire", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("NOT_FOUND", "prerequisite not found")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_launch_prerequisites",
		&prereqID, "PREREQ_EXPIRED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "prereq audit", err)
	}
	return s.getPrereq(ctx, prereqID)
}

// LaunchGate evaluates the production launch gate — ready=false lists
// every required GLOBAL prerequisite still missing or expired.
func (s *Service) LaunchGate(ctx context.Context) (*LaunchGateReport, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT prereq_id, kind, scope, required, status, description,
		       evidence_ref, evidenced_by, evidenced_at, expires_at,
		       created_by, created_at, updated_at
		  FROM venue_launch_prerequisites
		 WHERE scope='GLOBAL' AND required
		   AND (status <> 'EVIDENCED'
		        OR (expires_at IS NOT NULL AND expires_at < now()))
		 ORDER BY kind`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "launch gate", err)
	}
	defer rows.Close()
	rep := &LaunchGateReport{Ready: true, EvaluatedAt: s.now().UTC()}
	for rows.Next() {
		p, err := scanPrereq(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "prereq scan", err)
		}
		rep.Missing = append(rep.Missing, *p)
		rep.Ready = false
	}
	if rep.Missing == nil {
		rep.Missing = []Prerequisite{}
	}
	return rep, rows.Err()
}

// ListPrerequisites returns the full checklist (audit surface).
func (s *Service) ListPrerequisites(ctx context.Context) ([]Prerequisite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT prereq_id, kind, scope, required, status, description,
		       evidence_ref, evidenced_by, evidenced_at, expires_at,
		       created_by, created_at, updated_at
		  FROM venue_launch_prerequisites ORDER BY scope, kind`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "prereq list", err)
	}
	defer rows.Close()
	var out []Prerequisite
	for rows.Next() {
		p, err := scanPrereq(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "prereq scan", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func scanPrereq(row interface{ Scan(dest ...any) error }) (*Prerequisite, error) {
	var p Prerequisite
	var evRef *string
	err := row.Scan(&p.PrereqID, &p.Kind, &p.Scope, &p.Required, &p.Status,
		&p.Description, &evRef, &p.EvidencedBy, &p.EvidencedAt,
		&p.ExpiresAt, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if evRef != nil {
		p.EvidenceRef = *evRef
	}
	return &p, nil
}

func (s *Service) getPrereq(ctx context.Context, id int64) (*Prerequisite, error) {
	p, err := scanPrereq(s.pool.QueryRow(ctx, `
		SELECT prereq_id, kind, scope, required, status, description,
		       evidence_ref, evidenced_by, evidenced_at, expires_at,
		       created_by, created_at, updated_at
		  FROM venue_launch_prerequisites WHERE prereq_id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "prerequisite not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "prereq read", err)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Sweeps
// ---------------------------------------------------------------------------

// SweepOverdue pages P1 for members whose annual_review_due has lapsed
// (ANNUAL_ATTESTATION_OVERDUE — the §27.1 matrix code) and for a launch
// gate that regressed post-evidencing (license lapse intraday). Daily
// cadence; the alert layer dedups on the day's anchor.
func (s *Service) SweepOverdue(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT member_id, legal_name, lei, annual_review_due
		  FROM venue_members
		 WHERE admission_decision='APPROVED' AND terminated_at IS NULL
		   AND annual_review_due IS NOT NULL
		   AND annual_review_due < CURRENT_DATE`)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "overdue sweep", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var name, lei string
		var due time.Time
		if err := rows.Scan(&id, &name, &lei, &due); err != nil {
			return n, excerrors.Wrap("INTERNAL_ERROR", "overdue scan", err)
		}
		n++
		if s.alerter != nil {
			_ = s.alerter.RaiseHold(ctx, compliance.HoldAlert{
				Severity: "P1", Code: CodeAnnualAttestationOverdue,
				Summary: fmt.Sprintf("venue member %d (%s) annual review overdue since %s",
					id, lei, due.Format("2006-01-02")),
				Details: map[string]string{
					"member_id": fmt.Sprint(id), "lei": lei,
					"review_due": due.Format("2006-01-02"),
				},
			})
		}
	}
	return n, rows.Err()
}
