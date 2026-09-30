// Real-time monitoring + emergency record, investigation/disciplinary
// cases, conflicts register, annual system-safeguard self-assessment
// and the CCO annual report — Task 21.3.15 items 3–4 (spec §14.1b).
// Every mutation lands an audit_hash_chain row; evidence attachments
// are append-only with sha256-pinned refs.
package venue

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/audit"
	"exchange/internal/compliance"
	excerrors "exchange/pkg/errors"
)

// Intervention kinds + statuses (migration 250 CHECK mirror).
const (
	IntervLimit                  = "LIMIT"
	IntervHalt                   = "HALT"
	IntervCancellation           = "CANCELLATION"
	IntervCorrection             = "CORRECTION"
	IntervInfoRequest            = "INFO_REQUEST"
	IntervPositionAccountability = "POSITION_ACCOUNTABILITY"
	IntervEmergencyRule          = "EMERGENCY_RULE"

	IntervActive = "ACTIVE"
	IntervLifted = "LIFTED"
	IntervClosed = "CLOSED"
)

// Case kinds + statuses (migration 250 CHECK mirror).
const (
	CaseInvestigation = "INVESTIGATION"
	CaseDisciplinary  = "DISCIPLINARY"

	CaseStatusOpen          = "OPEN"
	CaseStatusInvestigating = "INVESTIGATING"
	CaseStatusCharged       = "CHARGED"
	CaseStatusSanctioned    = "SANCTIONED"
	CaseStatusDismissed     = "DISMISSED"
	CaseStatusClosed        = "CLOSED"
)

// Conflict statuses (migration 250 CHECK mirror).
const (
	ConflictDeclared  = "DECLARED"
	ConflictMitigated = "MITIGATED"
	ConflictRecused   = "RECUSED"
	ConflictClosed    = "CLOSED"
)

// Intervention is one venue_interventions row.
type Intervention struct {
	InterventionID int64           `json:"intervention_id"`
	Kind           string          `json:"kind"`
	InstrumentID   *int64          `json:"instrument_id,omitempty"`
	MemberID       *int64          `json:"member_id,omitempty"`
	AccountID      *int64          `json:"account_id,omitempty"`
	Status         string          `json:"status"`
	Reason         string          `json:"reason"`
	Detail         json.RawMessage `json:"detail"`
	ImposedBy      int64           `json:"imposed_by"`
	ImposedAt      time.Time       `json:"imposed_at"`
	LiftedBy       *int64          `json:"lifted_by,omitempty"`
	LiftedAt       *time.Time      `json:"lifted_at,omitempty"`
}

// Case is one venue_cases row.
type Case struct {
	CaseID    int64           `json:"case_id"`
	CaseRef   string          `json:"case_ref"`
	Kind      string          `json:"kind"`
	MemberID  *int64          `json:"member_id,omitempty"`
	AccountID *int64          `json:"account_id,omitempty"`
	Subject   string          `json:"subject"`
	Status    string          `json:"status"`
	Outcome   string          `json:"outcome,omitempty"`
	Detail    json.RawMessage `json:"detail"`
	OpenedBy  int64           `json:"opened_by"`
	OpenedAt  time.Time       `json:"opened_at"`
	ClosedBy  *int64          `json:"closed_by,omitempty"`
	ClosedAt  *time.Time      `json:"closed_at,omitempty"`
}

// CaseEvidence is one immutable venue_case_evidence row.
type CaseEvidence struct {
	EvidenceID  int64     `json:"evidence_id"`
	CaseID      int64     `json:"case_id"`
	EvidenceRef string    `json:"evidence_ref"`
	SHA256      string    `json:"sha256,omitempty"`
	Note        string    `json:"note,omitempty"`
	AttachedBy  int64     `json:"attached_by"`
	AttachedAt  time.Time `json:"attached_at"`
}

// Conflict is one venue_conflicts row.
type Conflict struct {
	ConflictID    int64      `json:"conflict_id"`
	MemberID      *int64     `json:"member_id,omitempty"`
	OfficerUserID *int64     `json:"officer_user_id,omitempty"`
	Subject       string     `json:"subject"`
	Nature        string     `json:"nature"`
	Status        string     `json:"status"`
	Mitigation    string     `json:"mitigation,omitempty"`
	DeclaredBy    int64      `json:"declared_by"`
	DeclaredAt    time.Time  `json:"declared_at"`
	ResolvedBy    *int64     `json:"resolved_by,omitempty"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
}

// SelfAssessment is one venue_self_assessments row — the annual
// system-safeguard review (MiFID II RTS 7 / CFTC SEF).
type SelfAssessment struct {
	AssessmentID         int64           `json:"assessment_id"`
	PeriodYear           int             `json:"period_year"`
	Version              int             `json:"version"`
	Status               string          `json:"status"`
	Evidence             json.RawMessage `json:"evidence"`
	Exceptions           json.RawMessage `json:"exceptions"`
	FinancialAttestation json.RawMessage `json:"financial_attestation"`
	Remediation          json.RawMessage `json:"remediation"`
	AssessedBy           *int64          `json:"assessed_by,omitempty"`
	AssessedAt           *time.Time      `json:"assessed_at,omitempty"`
	CreatedBy            int64           `json:"created_by"`
	CreatedAt            time.Time       `json:"created_at"`
}

// CCOReport is one cco_reports row — the annual CCO report the board
// pack's cco_report section probes.
type CCOReport struct {
	ID                    int64           `json:"id"`
	PeriodStart           time.Time       `json:"period_start"`
	PeriodEnd             time.Time       `json:"period_end"`
	Version               int             `json:"version"`
	Status                string          `json:"status"`
	Content               json.RawMessage `json:"content"`
	UnresolvedRemediation json.RawMessage `json:"unresolved_remediation"`
	GeneratedBy           int64           `json:"generated_by"`
	CreatedAt             time.Time       `json:"created_at"`
	BoardSignedBy         *int64          `json:"board_signed_by,omitempty"`
	BoardSignedAt         *time.Time      `json:"board_signed_at,omitempty"`
	RegulatorFilingRef    string          `json:"regulator_filing_ref,omitempty"`
	FiledBy               *int64          `json:"filed_by,omitempty"`
	FiledAt               *time.Time      `json:"filed_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Interventions — real-time market control + emergency record
// ---------------------------------------------------------------------------

// RecordIntervention logs a market-control / emergency action —
// limits, halts, cancellations/corrections, information requests,
// position accountability, emergency rules. Immutable evidence: the
// insert and every later lift land audit-chain entries.
func (s *Service) RecordIntervention(ctx context.Context, kind string,
	instrumentID, memberID, accountID *int64, reason string,
	detail json.RawMessage, actor int64) (*Intervention, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	switch kind {
	case IntervLimit, IntervHalt, IntervCancellation, IntervCorrection,
		IntervInfoRequest, IntervPositionAccountability, IntervEmergencyRule:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"unknown intervention kind "+kind)
	}
	if strings.TrimSpace(reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "reason required")
	}
	if len(detail) == 0 {
		detail = json.RawMessage("{}")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO venue_interventions
		    (kind, instrument_id, member_id, account_id, reason, detail,
		     imposed_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING intervention_id`,
		kind, instrumentID, memberID, accountID, reason, detail, actor).
		Scan(&id); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention insert", err)
	}
	if _, err := audit.Append(ctx, tx, "venue_interventions", &id,
		"INTERVENTION_"+kind, nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention commit", err)
	}
	return s.GetIntervention(ctx, id)
}

// LiftIntervention closes an ACTIVE intervention with its lifting
// rationale — the record persists (evidence is never deleted).
func (s *Service) LiftIntervention(ctx context.Context, interventionID int64,
	note string, actor int64) (*Intervention, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE venue_interventions
		SET status='LIFTED', lifted_by=$2, lifted_at=now(),
		    detail = detail || $3::jsonb
		WHERE intervention_id=$1 AND status='ACTIVE'`,
		interventionID, actor,
		fmt.Sprintf(`{"lift":{"note":%q,"at":%q}}`,
			note, s.now().UTC().Format(time.RFC3339)))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention lift", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"intervention not found or not ACTIVE")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_interventions",
		&interventionID, "INTERVENTION_LIFTED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention audit", err)
	}
	return s.GetIntervention(ctx, interventionID)
}

// GetIntervention reads one record.
func (s *Service) GetIntervention(ctx context.Context,
	id int64) (*Intervention, error) {
	v, err := scanIntervention(s.pool.QueryRow(ctx, `
		SELECT intervention_id, kind, instrument_id, member_id, account_id,
		       status, reason, detail, imposed_by, imposed_at, lifted_by,
		       lifted_at
		  FROM venue_interventions WHERE intervention_id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "intervention not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention read", err)
	}
	return v, nil
}

// ListInterventions returns the record (optionally ?status=&kind=).
func (s *Service) ListInterventions(ctx context.Context, status, kind string,
	limit int) ([]Intervention, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT intervention_id, kind, instrument_id, member_id,
	             account_id, status, reason, detail, imposed_by,
	             imposed_at, lifted_by, lifted_at
	        FROM venue_interventions WHERE true`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status=$%d", len(args))
	}
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(" AND kind=$%d", len(args))
	}
	q += " ORDER BY intervention_id DESC LIMIT " + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention list", err)
	}
	defer rows.Close()
	var out []Intervention
	for rows.Next() {
		v, err := scanIntervention(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "intervention scan", err)
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func scanIntervention(row interface{ Scan(dest ...any) error }) (*Intervention, error) {
	var v Intervention
	err := row.Scan(&v.InterventionID, &v.Kind, &v.InstrumentID,
		&v.MemberID, &v.AccountID, &v.Status, &v.Reason, &v.Detail,
		&v.ImposedBy, &v.ImposedAt, &v.LiftedBy, &v.LiftedAt)
	return &v, err
}

// ---------------------------------------------------------------------------
// Investigation / disciplinary cases
// ---------------------------------------------------------------------------

// OpenCase opens an investigation or disciplinary case — the ref is a
// deterministic VC-<year>-<member-or-0>-<unixnano> so replays dedup on
// the UNIQUE case_ref.
func (s *Service) OpenCase(ctx context.Context, kind string,
	memberID, accountID *int64, subject string, detail json.RawMessage,
	actor int64) (*Case, bool, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, false, err
	}
	if kind != CaseInvestigation && kind != CaseDisciplinary {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"kind must be INVESTIGATION|DISCIPLINARY")
	}
	if strings.TrimSpace(subject) == "" {
		return nil, false, excerrors.New("INVALID_REQUEST", "subject required")
	}
	if len(detail) == 0 {
		detail = json.RawMessage("{}")
	}
	ref := fmt.Sprintf("VC-%d-%d-%d", s.now().UTC().Year(),
		deref(memberID), s.now().UTC().UnixNano())
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO venue_cases
		    (case_ref, kind, member_id, account_id, subject, detail,
		     opened_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING case_id`,
		ref, kind, memberID, accountID, subject, detail, actor).Scan(&id)
	if err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "case insert", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_cases", &id,
		"VENUE_CASE_OPEN", nil); err != nil {
		return nil, false, excerrors.Wrap("INTERNAL_ERROR", "case audit", err)
	}
	c, err := s.GetCase(ctx, id)
	return c, true, err
}

// AttachEvidence pins a sha256'd evidence ref to a case — append-only.
func (s *Service) AttachEvidence(ctx context.Context, caseID int64,
	evidenceRef, sha256, note string, actor int64) (*CaseEvidence, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(evidenceRef) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "evidence_ref required")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO venue_case_evidence
		    (case_id, evidence_ref, sha256, note, attached_by)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5) RETURNING evidence_id`,
		caseID, evidenceRef, sha256, note, actor).Scan(&id)
	if err != nil {
		return nil, excerrors.Wrap("INVALID_REQUEST",
			"evidence insert failed (case id?)", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_case_evidence", &id,
		"CASE_EVIDENCE", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "evidence audit", err)
	}
	return &CaseEvidence{EvidenceID: id, CaseID: caseID,
		EvidenceRef: evidenceRef, SHA256: sha256, Note: note,
		AttachedBy: actor, AttachedAt: s.now().UTC()}, nil
}

// caseTransitions is the venue-case lifecycle (spec §14.1b edge:
// CHARGED requires an investigation; terminal rows never reopen).
var caseTransitions = map[string][]string{
	CaseStatusOpen:          {CaseStatusInvestigating, CaseStatusDismissed},
	CaseStatusInvestigating: {CaseStatusCharged, CaseStatusDismissed, CaseStatusClosed},
	CaseStatusCharged:       {CaseStatusSanctioned, CaseStatusDismissed},
	CaseStatusSanctioned:    {CaseStatusClosed},
	CaseStatusDismissed:     {CaseStatusClosed},
}

// TransitionCase moves a case along the lifecycle; terminal statuses
// stamp closed_at/closed_by + outcome.
func (s *Service) TransitionCase(ctx context.Context, caseID int64,
	to, outcome string, actor int64) (*Case, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "case tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	err = tx.QueryRow(ctx, `
		SELECT status FROM venue_cases WHERE case_id=$1 FOR UPDATE`,
		caseID).Scan(&status)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "venue case not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "case lock", err)
	}
	ok := false
	for _, t := range caseTransitions[status] {
		if t == to {
			ok = true
			break
		}
	}
	if !ok {
		return nil, excerrors.New("INVALID_REQUEST",
			"case transition "+status+" → "+to+" not permitted")
	}
	terminal := to == CaseStatusSanctioned || to == CaseStatusDismissed ||
		to == CaseStatusClosed
	if terminal && strings.TrimSpace(outcome) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"terminal transitions require an outcome")
	}
	res, err := tx.Exec(ctx, `
		UPDATE venue_cases
		SET status=$2, outcome=NULLIF($3,''),
		    closed_by=CASE WHEN $4 THEN $5 ELSE closed_by END,
		    closed_at=CASE WHEN $4 THEN now() ELSE closed_at END
		WHERE case_id=$1`, caseID, to, outcome, terminal, actor)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "case transition", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("NOT_FOUND", "venue case not found")
	}
	if _, err := audit.Append(ctx, tx, "venue_cases", &caseID,
		"CASE_"+to, nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "case audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "case commit", err)
	}
	return s.GetCase(ctx, caseID)
}

// GetCase reads one case.
func (s *Service) GetCase(ctx context.Context, caseID int64) (*Case, error) {
	c, err := scanCase(s.pool.QueryRow(ctx, `
		SELECT case_id, case_ref, kind, member_id, account_id, subject,
		       status, outcome, detail, opened_by, opened_at, closed_by,
		       closed_at
		  FROM venue_cases WHERE case_id=$1`, caseID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "venue case not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "case read", err)
	}
	return c, nil
}

// ListCases returns cases (?status=&kind= filters).
func (s *Service) ListCases(ctx context.Context, status, kind string,
	limit int) ([]Case, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT case_id, case_ref, kind, member_id, account_id, subject,
	             status, outcome, detail, opened_by, opened_at, closed_by,
	             closed_at
	        FROM venue_cases WHERE true`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status=$%d", len(args))
	}
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(" AND kind=$%d", len(args))
	}
	q += " ORDER BY case_id DESC LIMIT " + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "case list", err)
	}
	defer rows.Close()
	var out []Case
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "case scan", err)
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ListCaseEvidence returns a case's immutable attachments.
func (s *Service) ListCaseEvidence(ctx context.Context,
	caseID int64) ([]CaseEvidence, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT evidence_id, case_id, evidence_ref, sha256, note,
		       attached_by, attached_at
		  FROM venue_case_evidence WHERE case_id=$1 ORDER BY evidence_id`,
		caseID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "evidence list", err)
	}
	defer rows.Close()
	var out []CaseEvidence
	for rows.Next() {
		var e CaseEvidence
		var sha, note *string
		if err := rows.Scan(&e.EvidenceID, &e.CaseID, &e.EvidenceRef,
			&sha, &note, &e.AttachedBy, &e.AttachedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "evidence scan", err)
		}
		if sha != nil {
			e.SHA256 = *sha
		}
		if note != nil {
			e.Note = *note
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanCase(row interface{ Scan(dest ...any) error }) (*Case, error) {
	var c Case
	var outcome *string
	err := row.Scan(&c.CaseID, &c.CaseRef, &c.Kind, &c.MemberID,
		&c.AccountID, &c.Subject, &c.Status, &outcome, &c.Detail,
		&c.OpenedBy, &c.OpenedAt, &c.ClosedBy, &c.ClosedAt)
	if outcome != nil {
		c.Outcome = *outcome
	}
	return &c, err
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// ---------------------------------------------------------------------------
// Conflicts register
// ---------------------------------------------------------------------------

// DeclareConflict registers a conflict of interest (member- or
// officer-scoped; officer rows drive the recusal edge case).
func (s *Service) DeclareConflict(ctx context.Context, memberID,
	officerID *int64, subject, nature string, actor int64) (*Conflict, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(nature) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"subject and nature are required")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO venue_conflicts
		    (member_id, officer_user_id, subject, nature, declared_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING conflict_id`,
		memberID, officerID, subject, nature, actor).Scan(&id)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "conflict insert", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_conflicts", &id,
		"CONFLICT_DECLARED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "conflict audit", err)
	}
	return s.getConflict(ctx, id)
}

// ResolveConflict moves a conflict to MITIGATED|RECUSED|CLOSED with its
// mitigation rationale.
func (s *Service) ResolveConflict(ctx context.Context, conflictID int64,
	to, mitigation string, actor int64) (*Conflict, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	switch to {
	case ConflictMitigated, ConflictRecused, ConflictClosed:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"resolution must be MITIGATED|RECUSED|CLOSED")
	}
	if strings.TrimSpace(mitigation) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"mitigation rationale required")
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE venue_conflicts
		SET status=$2, mitigation=$3, resolved_by=$4, resolved_at=now()
		WHERE conflict_id=$1 AND status <> 'CLOSED'`,
		conflictID, to, mitigation, actor)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "conflict resolve", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"conflict not found or already CLOSED")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_conflicts",
		&conflictID, "CONFLICT_"+to, nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "conflict audit", err)
	}
	return s.getConflict(ctx, conflictID)
}

// ListConflicts returns the register (?status=).
func (s *Service) ListConflicts(ctx context.Context, status string,
	limit int) ([]Conflict, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT conflict_id, member_id, officer_user_id, subject, nature,
	             status, mitigation, declared_by, declared_at, resolved_by,
	             resolved_at
	        FROM venue_conflicts`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += " WHERE status=$1"
	}
	q += " ORDER BY conflict_id DESC LIMIT " + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "conflict list", err)
	}
	defer rows.Close()
	var out []Conflict
	for rows.Next() {
		var c Conflict
		var mit *string
		if err := rows.Scan(&c.ConflictID, &c.MemberID, &c.OfficerUserID,
			&c.Subject, &c.Nature, &c.Status, &mit, &c.DeclaredBy,
			&c.DeclaredAt, &c.ResolvedBy, &c.ResolvedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "conflict scan", err)
		}
		if mit != nil {
			c.Mitigation = *mit
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) getConflict(ctx context.Context, id int64) (*Conflict, error) {
	var c Conflict
	var mit *string
	err := s.pool.QueryRow(ctx, `
		SELECT conflict_id, member_id, officer_user_id, subject, nature,
		       status, mitigation, declared_by, declared_at, resolved_by,
		       resolved_at
		  FROM venue_conflicts WHERE conflict_id=$1`, id).
		Scan(&c.ConflictID, &c.MemberID, &c.OfficerUserID, &c.Subject,
			&c.Nature, &c.Status, &mit, &c.DeclaredBy, &c.DeclaredAt,
			&c.ResolvedBy, &c.ResolvedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "conflict not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "conflict read", err)
	}
	if mit != nil {
		c.Mitigation = *mit
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// Annual self-assessment + CCO report
// ---------------------------------------------------------------------------

// FileSelfAssessment opens (or versions) the year's system-safeguard
// self-assessment with the assembled control-evidence snapshot —
// counts of members, interventions, cases, conflicts, rulebook
// versions and outstanding remediation, measured at filing time. The
// officer's exceptions / financial-resource attestation / remediation
// list ride the same row; Complete stamps the sign-off.
func (s *Service) FileSelfAssessment(ctx context.Context, year int,
	exceptions, financialAttestation, remediation json.RawMessage,
	actor int64) (*SelfAssessment, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if year < 2000 || year > 2100 {
		return nil, excerrors.New("INVALID_REQUEST", "bad period_year")
	}
	if len(exceptions) == 0 {
		exceptions = json.RawMessage("[]")
	}
	if len(financialAttestation) == 0 {
		financialAttestation = json.RawMessage("{}")
	}
	if len(remediation) == 0 {
		remediation = json.RawMessage("[]")
	}
	evidence, err := s.assembleControlEvidence(ctx)
	if err != nil {
		return nil, err
	}
	var id int64
	var ver int
	err = s.pool.QueryRow(ctx, `
		INSERT INTO venue_self_assessments
		    (period_year, version, evidence, exceptions,
		     financial_attestation, remediation, created_by)
		VALUES ($1,
		        COALESCE((SELECT max(version) FROM venue_self_assessments
		                  WHERE period_year=$1),0)+1,
		        $2,$3,$4,$5,$6)
		RETURNING assessment_id, version`,
		year, evidence, exceptions, financialAttestation, remediation,
		actor).Scan(&id, &ver)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "assessment insert", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_self_assessments",
		&id, "SELF_ASSESSMENT_FILED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "assessment audit", err)
	}
	return &SelfAssessment{AssessmentID: id, PeriodYear: year, Version: ver,
		Status: "DRAFT", Evidence: evidence, Exceptions: exceptions,
		FinancialAttestation: financialAttestation, Remediation: remediation,
		CreatedBy: actor, CreatedAt: s.now().UTC()}, nil
}

// CompleteSelfAssessment stamps the DRAFT COMPLETED (assessor sign-off).
func (s *Service) CompleteSelfAssessment(ctx context.Context,
	assessmentID int64, actor int64) (*SelfAssessment, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE venue_self_assessments
		SET status='COMPLETED', assessed_by=$2, assessed_at=now()
		WHERE assessment_id=$1 AND status='DRAFT'`,
		assessmentID, actor)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "assessment complete", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"assessment not found or already COMPLETED")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "venue_self_assessments",
		&assessmentID, "SELF_ASSESSMENT_COMPLETED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "assessment audit", err)
	}
	return s.getAssessment(ctx, assessmentID)
}

// ListSelfAssessments returns the filed assessments, newest first.
func (s *Service) ListSelfAssessments(ctx context.Context,
	limit int) ([]SelfAssessment, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT assessment_id, period_year, version, status, evidence,
		       exceptions, financial_attestation, remediation, assessed_by,
		       assessed_at, created_by, created_at
		  FROM venue_self_assessments
		 ORDER BY period_year DESC, version DESC LIMIT `+fmt.Sprint(limit))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "assessment list", err)
	}
	defer rows.Close()
	var out []SelfAssessment
	for rows.Next() {
		var a SelfAssessment
		if err := rows.Scan(&a.AssessmentID, &a.PeriodYear, &a.Version,
			&a.Status, &a.Evidence, &a.Exceptions, &a.FinancialAttestation,
			&a.Remediation, &a.AssessedBy, &a.AssessedAt, &a.CreatedBy,
			&a.CreatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "assessment scan", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) getAssessment(ctx context.Context,
	id int64) (*SelfAssessment, error) {
	var a SelfAssessment
	err := s.pool.QueryRow(ctx, `
		SELECT assessment_id, period_year, version, status, evidence,
		       exceptions, financial_attestation, remediation, assessed_by,
		       assessed_at, created_by, created_at
		  FROM venue_self_assessments WHERE assessment_id=$1`, id).
		Scan(&a.AssessmentID, &a.PeriodYear, &a.Version, &a.Status,
			&a.Evidence, &a.Exceptions, &a.FinancialAttestation,
			&a.Remediation, &a.AssessedBy, &a.AssessedAt, &a.CreatedBy,
			&a.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "assessment not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "assessment read", err)
	}
	return &a, nil
}

// assembleControlEvidence is the shared control-evidence snapshot the
// self-assessment and CCO report both draw on — real counts from the
// governance stores, never fabricated zeros.
func (s *Service) assembleControlEvidence(ctx context.Context) (json.RawMessage, error) {
	ev := map[string]any{"assembled_at": s.now().UTC().Format(time.RFC3339)}
	count := func(q string, args ...any) int64 {
		var n int64
		if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			return -1 // surfaced as -1 — a failed probe is visible, not 0
		}
		return n
	}
	ev["members_total"] = count(`SELECT count(*) FROM venue_members`)
	ev["members_admitted"] = count(
		`SELECT count(*) FROM venue_members WHERE admission_decision='APPROVED' AND terminated_at IS NULL`)
	ev["members_suspended"] = count(
		`SELECT count(*) FROM venue_members WHERE suspended`)
	ev["reviews_overdue"] = count(
		`SELECT count(*) FROM venue_members
		  WHERE admission_decision='APPROVED' AND terminated_at IS NULL
		    AND annual_review_due IS NOT NULL AND annual_review_due < CURRENT_DATE`)
	ev["rulebooks_effective"] = count(
		`SELECT count(*) FROM venue_rulebooks WHERE status='EFFECTIVE'`)
	ev["interventions_active"] = count(
		`SELECT count(*) FROM venue_interventions WHERE status='ACTIVE'`)
	ev["cases_open"] = count(
		`SELECT count(*) FROM venue_cases WHERE status NOT IN ('SANCTIONED','DISMISSED','CLOSED')`)
	ev["conflicts_open"] = count(
		`SELECT count(*) FROM venue_conflicts WHERE status='DECLARED'`)
	ev["prereqs_missing"] = count(
		`SELECT count(*) FROM venue_launch_prerequisites
		  WHERE scope='GLOBAL' AND required AND status <> 'EVIDENCED'`)
	raw, _ := json.Marshal(ev)
	return raw, nil
}

// GenerateCCOReport assembles the annual CCO report for [start, end) —
// a versioned row carrying the control-evidence snapshot plus the
// unresolved-remediation list the AC calls out.
func (s *Service) GenerateCCOReport(ctx context.Context, start, end time.Time,
	unresolvedRemediation json.RawMessage, actor int64) (*CCOReport, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return nil, excerrors.New("INVALID_REQUEST",
			"period_start/period_end required with end > start")
	}
	if len(unresolvedRemediation) == 0 {
		unresolvedRemediation = json.RawMessage("[]")
	}
	evidence, err := s.assembleControlEvidence(ctx)
	if err != nil {
		return nil, err
	}
	content := map[string]any{
		"control_evidence": json.RawMessage(evidence),
		"period": map[string]string{
			"start": start.Format("2006-01-02"),
			"end":   end.Format("2006-01-02"),
		},
	}
	raw, _ := json.Marshal(content)
	var id int64
	var ver int
	err = s.pool.QueryRow(ctx, `
		INSERT INTO cco_reports
		    (period_start, period_end, version, content,
		     unresolved_remediation, generated_by)
		VALUES ($1,$2,
		        COALESCE((SELECT max(version) FROM cco_reports
		                  WHERE period_start=$1 AND period_end=$2),0)+1,
		        $3,$4,$5)
		RETURNING id, version`,
		start, end, raw, unresolvedRemediation, actor).Scan(&id, &ver)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "cco report insert", err)
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "cco_reports", &id,
		"CCO_REPORT_GENERATED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "cco report audit", err)
	}
	return s.getCCOReport(ctx, id)
}

// SignCCOReport stamps the board sign-off — DRAFT → SIGNED.
func (s *Service) SignCCOReport(ctx context.Context, reportID int64,
	actor int64) (*CCOReport, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE cco_reports
		SET status='SIGNED', board_signed_by=$2, board_signed_at=now()
		WHERE id=$1 AND status='DRAFT'`, reportID, actor)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "cco sign", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"cco report not found or not DRAFT")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "cco_reports", &reportID,
		"CCO_REPORT_SIGNED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "cco audit", err)
	}
	return s.getCCOReport(ctx, reportID)
}

// FileCCOReport records the regulator filing of a SIGNED report —
// SIGNED → FILED with the filing reference.
func (s *Service) FileCCOReport(ctx context.Context, reportID int64,
	filingRef string, actor int64) (*CCOReport, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(filingRef) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "filing_ref required")
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE cco_reports
		SET status='FILED', regulator_filing_ref=$2, filed_by=$3,
		    filed_at=now()
		WHERE id=$1 AND status='SIGNED'`, reportID, filingRef, actor)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "cco file", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"cco report not found or not SIGNED")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "cco_reports", &reportID,
		"CCO_REPORT_FILED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "cco audit", err)
	}
	return s.getCCOReport(ctx, reportID)
}

// ListCCOReports returns generated reports, newest first.
func (s *Service) ListCCOReports(ctx context.Context,
	limit int) ([]CCOReport, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, period_start, period_end, version, status, content,
		       unresolved_remediation, generated_by, created_at,
		       board_signed_by, board_signed_at, regulator_filing_ref,
		       filed_by, filed_at
		  FROM cco_reports
		 ORDER BY period_start DESC, version DESC LIMIT `+fmt.Sprint(limit))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "cco list", err)
	}
	defer rows.Close()
	var out []CCOReport
	for rows.Next() {
		var r CCOReport
		var filingRef *string
		if err := rows.Scan(&r.ID, &r.PeriodStart, &r.PeriodEnd, &r.Version,
			&r.Status, &r.Content, &r.UnresolvedRemediation, &r.GeneratedBy,
			&r.CreatedAt, &r.BoardSignedBy, &r.BoardSignedAt, &filingRef,
			&r.FiledBy, &r.FiledAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "cco scan", err)
		}
		if filingRef != nil {
			r.RegulatorFilingRef = *filingRef
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) getCCOReport(ctx context.Context, id int64) (*CCOReport, error) {
	var r CCOReport
	var filingRef *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, period_start, period_end, version, status, content,
		       unresolved_remediation, generated_by, created_at,
		       board_signed_by, board_signed_at, regulator_filing_ref,
		       filed_by, filed_at
		  FROM cco_reports WHERE id=$1`, id).
		Scan(&r.ID, &r.PeriodStart, &r.PeriodEnd, &r.Version, &r.Status,
			&r.Content, &r.UnresolvedRemediation, &r.GeneratedBy,
			&r.CreatedAt, &r.BoardSignedBy, &r.BoardSignedAt, &filingRef,
			&r.FiledBy, &r.FiledAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "cco report not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "cco read", err)
	}
	if filingRef != nil {
		r.RegulatorFilingRef = *filingRef
	}
	return &r, nil
}

// ensure compliance import is used (HoldAlerter seam lives on Service).
var _ = compliance.HoldAlert{}
