// Phase-21 Task 21.3.21 — Surveillance Case Management (spec §14.9.2).
//
// The case spine converts surveillance findings into investigations:
//
//	signal/monitoring finding ──▶ surveillance_cases (dedup source_ref)
//	      │                          │
//	      │   severity: URGENT (z≥3σ / HIGH confidence) → SLA 4h
//	      │             REVIEW (otherwise)              → SLA 24h
//	      ▼                          ▼
//	round-robin assignment ◀── SweepUnassigned
//	      │
//	      ▼  workspace = case + evidence + signal detail + order refs
//	AttachEvidence (immutable, hash-chained) / Disposition:
//	  FALSE_POSITIVE   → close with justification
//	  ESCALATE_SAR     → SAR draft pre-populated (source 'case:{id}')
//	  ESCALATE_STR     → STOR payload attachment (Task 21.3.27 format)
//	  ESCALATE_ACTION  → hold / enforcement action through the seams
//
// SLA breach flips sla_breached and pages the AML Officer at P2
// (SweepSLA). Cases + evidence retain ≥5y (Phase-09 retention policy
// class) — evidence rows are append-only by trigger (migration 240).
package compliance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/audit"

	excerrors "exchange/pkg/errors"
)

// Case severities / statuses / dispositions (CHECK mirrors in 240).
const (
	CaseSeverityUrgent = "URGENT"
	CaseSeverityReview = "REVIEW"

	CaseStatusOpen          = "OPEN"
	CaseStatusAssigned      = "ASSIGNED"
	CaseStatusInvestigating = "INVESTIGATING"
	CaseStatusClosedFP      = "CLOSED_FALSE_POSITIVE"
	CaseStatusEscalatedSAR  = "ESCALATED_SAR"
	CaseStatusEscalatedSTR  = "ESCALATED_STR"
	CaseStatusEscalatedAct  = "ESCALATED_ACTION"
)

// Assignment SLAs — spec §14.9.2: URGENT ≤4h, REVIEW ≤24h.
const (
	CaseSLAUrgent = 4 * time.Hour
	CaseSLAReview = 24 * time.Hour
)

// Evidence attachment kinds.
const (
	CaseEvNote           = "NOTE"
	CaseEvAttachment     = "ATTACHMENT"
	CaseEvCommsRecording = "COMMS_RECORDING"
	CaseEvDocument       = "DOCUMENT"
	CaseEvSTOR           = "STOR"
)

// zScoreUrgentThreshold — "high-confidence" per spec §14.9.2 is a
// detector z-score ≥ 3σ carried in the signal evidence.
const zScoreUrgentThreshold = 3.0

// Case is one surveillance_cases row.
type Case struct {
	ID        int64  `json:"id"`
	CaseRef   string `json:"case_ref"`
	SourceRef string `json:"source_ref"`
	SignalID  *int64 `json:"signal_id,omitempty"`
	AccountID *int64 `json:"account_id,omitempty"`
	// account_hash is BIGINT — the engine writes int64(hash); the
	// signed stored form is canonical (matches sar.go subject_ref).
	AccountHash       int64           `json:"account_hash"`
	SignalType        string          `json:"signal_type"`
	Symbol            string          `json:"symbol"`
	Severity          string          `json:"severity"`
	Status            string          `json:"status"`
	AssignedTo        *int64          `json:"assigned_to,omitempty"`
	AssignedAt        *time.Time      `json:"assigned_at,omitempty"`
	FirstReviewedAt   *time.Time      `json:"first_reviewed_at,omitempty"`
	SLADeadline       time.Time       `json:"sla_deadline"`
	SLABreached       bool            `json:"sla_breached"`
	SLANote           string          `json:"sla_note"`
	EscalatedSARID    *int64          `json:"escalated_sar_id,omitempty"`
	EscalationRef     string          `json:"escalation_ref"`
	DispositionReason string          `json:"disposition_reason"`
	Evidence          json.RawMessage `json:"evidence"`
	OpenedAt          time.Time       `json:"opened_at"`
	ClosedAt          *time.Time      `json:"closed_at,omitempty"`
}

// CaseEvidence is one immutable attachment row.
type CaseEvidence struct {
	ID            int64     `json:"id"`
	CaseID        int64     `json:"case_id"`
	Kind          string    `json:"kind"`
	Body          string    `json:"body"`
	AttachmentRef string    `json:"attachment_ref,omitempty"`
	SHA256        string    `json:"sha256,omitempty"`
	AddedBy       int64     `json:"added_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// CaseWorkspace is the investigation view — the case plus its
// workspace lanes: attached evidence, sibling signals on the same
// account_hash (counterparties/confidence), and the account's recent
// order-audit trail refs.
type CaseWorkspace struct {
	Case          *Case          `json:"case"`
	Evidence      []CaseEvidence `json:"evidence"`
	LinkedSignals []int64        `json:"linked_signals"`  // same-account signal ids
	OrderAuditIDs []int64        `json:"order_audit_ids"` // recent order_audit refs for the account
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// CaseSARDrafter pre-populates the SAR report on ESCALATE_SAR —
// SARService.Draft-compatible.
type CaseSARDrafter interface {
	Draft(ctx context.Context, in SARDraftInput) (*SARReport, bool, error)
}

// CaseEnforcer takes the ESCALATE_ACTION path — the same enforcement
// seam Task 21.3.8 exposes (hold / kill-switch inside).
type CaseEnforcer interface {
	Enforce(ctx context.Context, req EnforcementRequest) (*EnforcementAction, error)
}

// CaseAlerter raises the P2 SLA-breach page (AML Officer queue) —
// HoldAlerter-compatible payload shape.
type CaseAlerter interface {
	RaiseHold(ctx context.Context, a HoldAlert) error
}

// CaseService is the surveillance case spine.
type CaseService struct {
	pool     *pgxpool.Pool
	resolver HoldRoleResolver
	resolve  AccountResolver // L3 hash → account
	sar      CaseSARDrafter  // nil → ESCALATE_SAR fails closed
	enf      CaseEnforcer    // nil → ESCALATE_ACTION fails closed
	alerter  CaseAlerter
	now      func() time.Time
	newRef   func() (string, error)
}

// NewCaseService binds dependencies; resolver gates officer actions.
func NewCaseService(pool *pgxpool.Pool, resolver HoldRoleResolver,
	resolve AccountResolver, sar CaseSARDrafter, enf CaseEnforcer,
	alerter CaseAlerter) *CaseService {
	return &CaseService{pool: pool, resolver: resolver, resolve: resolve,
		sar: sar, enf: enf, alerter: alerter,
		now: time.Now, newRef: defaultCaseRef}
}

func defaultCaseRef() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "case_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// WithClock / WithRefSource are test hooks.
func (s *CaseService) WithClock(f func() time.Time) *CaseService {
	if f != nil {
		s.now = f
	}
	return s
}
func (s *CaseService) WithRefSource(f func() (string, error)) *CaseService {
	if f != nil {
		s.newRef = f
	}
	return s
}

func (s *CaseService) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot work surveillance cases")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Intake — signal + monitoring-finding fronts
// ---------------------------------------------------------------------------

// OpenFromSignal creates the case for a surveillance signal.
// Idempotent on source_ref 'signal:{id}' — the second open returns the
// existing case (created=false). Severity: z≥3σ or confidence HIGH in
// the evidence → URGENT else REVIEW.
func (s *CaseService) OpenFromSignal(ctx context.Context,
	signalID int64) (*Case, bool, error) {
	var sig EnforcementSignal
	var ev []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, signal_type, symbol, account_hash, status,
		       evidence, created_at
		  FROM surveillance_signals WHERE id = $1`, signalID).
		Scan(&sig.ID, &sig.SignalType, &sig.Symbol, &sig.AccountHash,
			&sig.Status, &ev, &sig.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, false, excerrors.New("NOT_FOUND", "signal not found")
	}
	if err != nil {
		return nil, false, excerrors.New("INTERNAL_ERROR",
			"signal load: "+err.Error())
	}
	sig.Evidence = ev

	severity := CaseSeverityReview
	if urgentEvidence(ev) || severeSignalTypes[sig.SignalType] {
		severity = CaseSeverityUrgent
	}
	var accountID *int64
	if s.resolve != nil {
		if id, ok := s.resolve(ctx, uint64(sig.AccountHash)); ok {
			accountID = &id
		}
	}
	return s.openCase(ctx, fmt.Sprintf("signal:%d", signalID),
		&signalID, accountID, sig.AccountHash, sig.SignalType,
		sig.Symbol, severity, ev)
}

// OpenCase is the monitoring.CaseSink implementation — findings from
// the AML monitoring engine land as REVIEW cases deduped per
// rule/account/day.
func (s *CaseService) OpenCase(ctx context.Context,
	f MonitoringFinding) (int64, error) {
	bucket := f.DetectedAt.UTC().Format("2006-01-02")
	ref := fmt.Sprintf("monitoring:%s:%d:%s", f.Rule, f.AccountID, bucket)
	ev, _ := json.Marshal(f)
	var acct *int64
	if f.AccountID > 0 {
		acct = &f.AccountID
	}
	severity := CaseSeverityReview
	if f.Severity == "P1" {
		severity = CaseSeverityUrgent
	}
	c, _, err := s.openCase(ctx, ref, nil, acct, 0,
		f.Rule, "", severity, ev)
	if err != nil {
		return 0, err
	}
	return c.ID, nil
}

// openCase is the shared insert — UNIQUE(source_ref) dedup returns the
// existing row untouched.
func (s *CaseService) openCase(ctx context.Context, sourceRef string,
	signalID, accountID *int64, accountHash int64,
	signalType, symbol, severity string,
	evidence json.RawMessage) (*Case, bool, error) {
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}
	ref, err := s.newRef()
	if err != nil {
		return nil, false, excerrors.New("INTERNAL_ERROR",
			"case ref: "+err.Error())
	}
	sla := CaseSLAReview
	if severity == CaseSeverityUrgent {
		sla = CaseSLAUrgent
	}
	deadline := s.now().UTC().Add(sla)

	var c Case
	err = s.pool.QueryRow(ctx, `
		INSERT INTO surveillance_cases
		    (case_ref, source_ref, signal_id, account_id, account_hash,
		     signal_type, symbol, severity, sla_deadline, evidence)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (source_ref) DO NOTHING
		RETURNING id, case_ref, source_ref, signal_id, account_id,
		          account_hash, signal_type, symbol, severity, status,
		          assigned_to, assigned_at, first_reviewed_at,
		          sla_deadline, sla_breached, sla_note,
		          escalated_sar_id, escalation_ref, disposition_reason,
		          evidence, opened_at, closed_at`,
		ref, sourceRef, signalID, accountID, accountHash,
		signalType, symbol, severity, deadline, evidence).
		Scan(&c.ID, &c.CaseRef, &c.SourceRef, &c.SignalID, &c.AccountID,
			&c.AccountHash, &c.SignalType, &c.Symbol, &c.Severity, &c.Status,
			&c.AssignedTo, &c.AssignedAt, &c.FirstReviewedAt,
			&c.SLADeadline, &c.SLABreached, &c.SLANote,
			&c.EscalatedSARID, &c.EscalationRef, &c.DispositionReason,
			&c.Evidence, &c.OpenedAt, &c.ClosedAt)
	if err == pgx.ErrNoRows {
		// Dedup hit — return the pre-existing case.
		existing, gerr := s.getBySource(ctx, sourceRef)
		if gerr != nil {
			return nil, false, gerr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, excerrors.New("INTERNAL_ERROR",
			"case open: "+err.Error())
	}
	// Intake is hash-chained like every compliance artifact.
	_, _ = audit.AppendAuto(ctx, s.pool,
		"surveillance_cases", &c.ID, "INSERT", nil)
	if signalID != nil {
		_, _ = s.pool.Exec(ctx,
			`UPDATE surveillance_signals SET status='CASED', updated_at=now()
			  WHERE id = $1 AND status='OPEN'`, *signalID)
	}
	return &c, true, nil
}

func (s *CaseService) getBySource(ctx context.Context,
	sourceRef string) (*Case, error) {
	var c Case
	err := s.pool.QueryRow(ctx, caseCols+" WHERE source_ref = $1",
		sourceRef).Scan(caseScan(&c)...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"case reload: "+err.Error())
	}
	return &c, nil
}

// urgentEvidence reports whether the Phase-17 evidence blob carries a
// high-confidence marker: z_score|z ≥ 3σ or confidence "HIGH".
func urgentEvidence(ev json.RawMessage) bool {
	var m map[string]any
	if json.Unmarshal(ev, &m) != nil {
		return false
	}
	for _, k := range []string{"z_score", "z", "zscore"} {
		if v, ok := m[k].(float64); ok && v >= zScoreUrgentThreshold {
			return true
		}
	}
	if c, ok := m["confidence"].(string); ok && c == "HIGH" {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Assignment — round-robin over ACTIVE Compliance Officer bindings
// ---------------------------------------------------------------------------

// Assign locks the case and (re)assigns it — officer id is explicit
// (reassign) or picked round-robin (assignee=0). Records assigned_at
// and flips OPEN → ASSIGNED.
func (s *CaseService) Assign(ctx context.Context, actorID, caseID,
	assignee int64) (*Case, error) {
	if err := s.checkRole(ctx, actorID); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assign tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var curAssignee *int64
	if err := tx.QueryRow(ctx,
		`SELECT status, assigned_to FROM surveillance_cases
		  WHERE id = $1 FOR UPDATE`, caseID).
		Scan(&status, &curAssignee); err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "case not found")
	} else if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "case lock: "+err.Error())
	}
	if isCaseTerminal(status) {
		return nil, excerrors.New("INVALID_REQUEST",
			"cannot assign a closed case")
	}
	if assignee <= 0 {
		next, err := s.roundRobin(ctx, tx)
		if err != nil {
			return nil, err
		}
		assignee = next
	}
	now := s.now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE surveillance_cases
		   SET assigned_to = $2, assigned_at = $3,
		       status = CASE WHEN status = 'OPEN' THEN 'ASSIGNED' ELSE status END,
		       updated_at = now()
		 WHERE id = $1`, caseID, assignee, now)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "case assign: "+err.Error())
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorID, Action: "surveillance.case.assign",
		TargetType: "surveillance_case", TargetID: &caseID,
		AfterState: map[string]any{
			"assigned_to": assignee, "assigned_at": now,
			"previous": curAssignee,
		},
	}); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assign audit: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assign commit: "+err.Error())
	}
	return s.Get(ctx, caseID)
}

// roundRobin picks the next officer: ACTIVE 'Compliance Officer'
// bindings ordered by id; the least-recently-assigned wins (MAX
// assigned_at across open cases, NULLs first). Empty pool → error so
// the sweep can alert rather than drop the case.
func (s *CaseService) roundRobin(ctx context.Context,
	tx pgx.Tx) (int64, error) {
	var officer int64
	err := tx.QueryRow(ctx, `
		SELECT b.user_id
		  FROM admin_role_bindings b
		 WHERE b.role = 'Compliance Officer' AND b.status = 'ACTIVE'
		   AND (b.expires_at IS NULL OR b.expires_at > now())
		 ORDER BY (
		   SELECT COALESCE(MAX(c.assigned_at), '-infinity'::timestamptz)
		     FROM surveillance_cases c
		    WHERE c.assigned_to = b.user_id
		      AND c.status IN ('ASSIGNED','INVESTIGATING')) ASC,
		      b.user_id ASC
		 LIMIT 1`).Scan(&officer)
	if err == pgx.ErrNoRows {
		return 0, excerrors.New("SERVICE_DEGRADED",
			"no active Compliance Officer bindings — case stays unassigned")
	}
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR",
			"officer pool: "+err.Error())
	}
	return officer, nil
}

// SweepUnassigned round-robins OPEN cases past the assignment gate —
// the "cases self-assign" path; returns the count newly assigned.
func (s *CaseService) SweepUnassigned(ctx context.Context,
	limit int) (int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM surveillance_cases
		 WHERE status = 'OPEN' AND assigned_to IS NULL
		 ORDER BY severity ASC, sla_deadline ASC  -- URGENT first (text sort)
		 LIMIT $1`, limit)
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR",
			"unassigned scan: "+err.Error())
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, excerrors.New("INTERNAL_ERROR", "case id: "+err.Error())
		}
		ids = append(ids, id)
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		// Sweep actor = system; Assign's role gate skips on resolver
		// failure so the sweep never fabricates authority — use the
		// internal path with the machine actor.
		if _, err := s.assignSystem(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// assignSystem is the sweep-safe round-robin (no officer role check —
// the machine assigns; an officer may still reassign via Assign).
func (s *CaseService) assignSystem(ctx context.Context,
	caseID int64) (*Case, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assign tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()
	officer, err := s.roundRobin(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE surveillance_cases
		   SET assigned_to = $2, assigned_at = $3, status = 'ASSIGNED',
		       updated_at = now()
		 WHERE id = $1 AND status = 'OPEN' AND assigned_to IS NULL`,
		caseID, officer, now); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "case assign: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "assign commit: "+err.Error())
	}
	return s.Get(ctx, caseID)
}

// ---------------------------------------------------------------------------
// Workspace reads
// ---------------------------------------------------------------------------

const caseCols = `
		SELECT id, case_ref, source_ref, signal_id, account_id,
		       account_hash, signal_type, symbol, severity, status,
		       assigned_to, assigned_at, first_reviewed_at,
		       sla_deadline, sla_breached, sla_note,
		       escalated_sar_id, escalation_ref, disposition_reason,
		       evidence, opened_at, closed_at
		  FROM surveillance_cases`

func caseScan(c *Case) []any {
	return []any{&c.ID, &c.CaseRef, &c.SourceRef, &c.SignalID,
		&c.AccountID, &c.AccountHash, &c.SignalType, &c.Symbol,
		&c.Severity, &c.Status, &c.AssignedTo, &c.AssignedAt,
		&c.FirstReviewedAt, &c.SLADeadline, &c.SLABreached, &c.SLANote,
		&c.EscalatedSARID, &c.EscalationRef, &c.DispositionReason,
		&c.Evidence, &c.OpenedAt, &c.ClosedAt}
}

// Get reads one case by id.
func (s *CaseService) Get(ctx context.Context, caseID int64) (*Case, error) {
	var c Case
	err := s.pool.QueryRow(ctx, caseCols+" WHERE id = $1",
		caseID).Scan(caseScan(&c)...)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "case not found")
	}
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "case get: "+err.Error())
	}
	return &c, nil
}

// Workspace returns the full investigation view: case + evidence +
// linked sibling signals on the same account_hash + recent
// order_audit refs for the resolved account.
func (s *CaseService) Workspace(ctx context.Context,
	caseID int64) (*CaseWorkspace, error) {
	c, err := s.Get(ctx, caseID)
	if err != nil {
		return nil, err
	}
	ws := &CaseWorkspace{Case: c}
	rows, err := s.pool.Query(ctx, `
		SELECT id, case_id, kind, body, attachment_ref, sha256,
		       added_by, created_at
		  FROM surveillance_case_evidence
		 WHERE case_id = $1 ORDER BY id ASC`, caseID)
	if err == nil {
		for rows.Next() {
			var e CaseEvidence
			if err := rows.Scan(&e.ID, &e.CaseID, &e.Kind, &e.Body,
				&e.AttachmentRef, &e.SHA256, &e.AddedBy,
				&e.CreatedAt); err != nil {
				rows.Close()
				return nil, excerrors.New("INTERNAL_ERROR",
					"evidence row: "+err.Error())
			}
			ws.Evidence = append(ws.Evidence, e)
		}
		rows.Close()
	}
	if c.AccountHash != 0 {
		_ = s.pool.QueryRow(ctx, `
			SELECT COALESCE(array_agg(id ORDER BY id DESC
			              FILTER (WHERE rn <= 50)), '{}')
			  FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY id DESC) rn
			          FROM surveillance_signals
			         WHERE account_hash = $1 AND id <> $2) t`,
			c.AccountHash, derefInt64(c.SignalID)).Scan(&ws.LinkedSignals)
	}
	if c.AccountID != nil {
		_ = s.pool.QueryRow(ctx, `
			SELECT COALESCE(array_agg(audit_id ORDER BY audit_id DESC), '{}')
			  FROM (SELECT audit_id FROM order_audit
			         WHERE account_id = $1 ORDER BY audit_id DESC
			         LIMIT 100) t`,
			*c.AccountID).Scan(&ws.OrderAuditIDs)
	}
	return ws, nil
}

// List is the queue read — open items order by SLA deadline.
func (s *CaseService) List(ctx context.Context, status, assignee string,
	limit int) ([]Case, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := caseCols + " WHERE 1=1"
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if assignee != "" {
		args = append(args, assignee)
		q += fmt.Sprintf(" AND assigned_to::text = $%d", len(args))
	}
	q += fmt.Sprintf(` ORDER BY CASE WHEN status IN
		('OPEN','ASSIGNED','INVESTIGATING') THEN 0 ELSE 1 END,
		sla_deadline ASC, id DESC LIMIT %d`, limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "case list: "+err.Error())
	}
	defer rows.Close()
	var out []Case
	for rows.Next() {
		var c Case
		if err := rows.Scan(caseScan(&c)...); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR", "case row: "+err.Error())
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Evidence — immutable, hash-chained attachments
// ---------------------------------------------------------------------------

// AttachEvidence appends one attachment; the row is append-only by
// trigger and the insert is hash-chained (audit.AppendAuto) so
// tampering breaks verification.
func (s *CaseService) AttachEvidence(ctx context.Context, actorID,
	caseID int64, kind, body, attachmentRef, sha256 string) (*CaseEvidence, error) {
	if err := s.checkRole(ctx, actorID); err != nil {
		return nil, err
	}
	switch kind {
	case CaseEvNote, CaseEvAttachment, CaseEvCommsRecording,
		CaseEvDocument, CaseEvSTOR:
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"evidence kind must be NOTE|ATTACHMENT|COMMS_RECORDING|DOCUMENT|STOR")
	}
	if body == "" && attachmentRef == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"evidence requires body or attachment_ref")
	}
	c, err := s.Get(ctx, caseID)
	if err != nil {
		return nil, err
	}
	if isCaseTerminal(c.Status) {
		return nil, excerrors.New("INVALID_REQUEST",
			"cannot attach evidence to a closed case")
	}
	var e CaseEvidence
	err = s.pool.QueryRow(ctx, `
		INSERT INTO surveillance_case_evidence
		    (case_id, kind, body, attachment_ref, sha256, added_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, case_id, kind, body, attachment_ref, sha256,
		          added_by, created_at`,
		caseID, kind, body, attachmentRef, sha256, actorID).
		Scan(&e.ID, &e.CaseID, &e.Kind, &e.Body, &e.AttachmentRef,
			&e.SHA256, &e.AddedBy, &e.CreatedAt)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"evidence insert: "+err.Error())
	}
	_, _ = audit.AppendAuto(ctx, s.pool,
		"surveillance_case_evidence", &e.ID, "INSERT", nil)
	// First attachment marks INVESTIGATING (workspace opened) — the
	// first-review timestamp feeds the SLA report.
	_, _ = s.pool.Exec(ctx, `
		UPDATE surveillance_cases
		   SET status = 'INVESTIGATING',
		       first_reviewed_at = COALESCE(first_reviewed_at, now()),
		       updated_at = now()
		 WHERE id = $1 AND status IN ('OPEN','ASSIGNED')`, caseID)
	return &e, nil
}

// ---------------------------------------------------------------------------
// Disposition
// ---------------------------------------------------------------------------

// Disposition is the terminal outcome; spec §14.9.2 vocabulary.
type DispositionRequest struct {
	Disposition string // FALSE_POSITIVE|ESCALATE_SAR|ESCALATE_STR|ESCALATE_ACTION
	Reason      string // mandatory justification
	Action      string // ESCALATE_ACTION → enforcement action verb
}

// Disposition closes the case per spec §14.9.2:
//
//	FALSE_POSITIVE   — close with mandatory justification (accuracy feed)
//	ESCALATE_SAR     — SAR draft pre-populated from the case evidence
//	ESCALATE_STR     — STOR payload generated and hash-chained as
//	                   STOR evidence (Task 21.3.27 format builder)
//	ESCALATE_ACTION  — enforcement action through the Task 21.3.8 seam
//
// Dispositions are terminal — a closed case never reopens (open a new
// signal/case instead; the audit chain keeps the history).
func (s *CaseService) Disposition(ctx context.Context, actorID,
	caseID int64, req DispositionRequest) (*Case, error) {
	if err := s.checkRole(ctx, actorID); err != nil {
		return nil, err
	}
	if req.Reason == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"disposition requires a justification")
	}
	var to string
	switch req.Disposition {
	case "FALSE_POSITIVE":
		to = CaseStatusClosedFP
	case "ESCALATE_SAR":
		to = CaseStatusEscalatedSAR
	case "ESCALATE_STR":
		to = CaseStatusEscalatedSTR
	case "ESCALATE_ACTION":
		to = CaseStatusEscalatedAct
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"disposition must be FALSE_POSITIVE|ESCALATE_SAR|"+
				"ESCALATE_STR|ESCALATE_ACTION")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "disposition tx: "+err.Error())
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c Case
	if err := tx.QueryRow(ctx,
		caseCols+` WHERE id = $1 FOR UPDATE`, caseID).
		Scan(caseScan(&c)...); err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "case not found")
	} else if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR", "case lock: "+err.Error())
	}
	if isCaseTerminal(c.Status) {
		return nil, excerrors.New("INVALID_REQUEST",
			"case is already closed ("+c.Status+")")
	}

	now := s.now().UTC()
	escalationRef := ""
	var sarID *int64

	switch req.Disposition {
	case "ESCALATE_SAR":
		if s.sar == nil {
			return nil, excerrors.New("SERVICE_DEGRADED",
				"SAR seam unwired — cannot escalate")
		}
		desc := fmt.Sprintf("Surveillance case %s escalated: %s signal "+
			"on %s — %s", c.CaseRef, c.SignalType, c.Symbol, req.Reason)
		rep, _, err := s.sar.Draft(ctx, SARDraftInput{
			TriggerType: SARTriggerSurveillance,
			AccountID:   c.AccountID,
			SubjectRef:  fmt.Sprintf("account_hash:%d", c.AccountHash),
			Description: desc,
			Evidence: map[string]any{
				"case_id": c.ID, "case_ref": c.CaseRef,
				"signal_id": c.SignalID, "signal_type": c.SignalType,
				"symbol": c.Symbol, "evidence": c.Evidence,
				"disposition_reason": req.Reason,
			},
			SourceRef:  fmt.Sprintf("case:%d", c.ID),
			DetectedAt: c.OpenedAt,
			CreatedBy:  &actorID,
		})
		if err != nil {
			return nil, err
		}
		sarID = &rep.ID
		escalationRef = fmt.Sprintf("sar:%d", rep.ID)

	case "ESCALATE_STR":
		// UK MAR STOR — payload generated by the Task 21.3.27
		// formatter and pinned as STOR evidence (hash-chained).
		payload := BuildSTORPayload(&c, req.Reason, s.now().UTC())
		pb, _ := json.Marshal(payload)
		var evID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO surveillance_case_evidence
			    (case_id, kind, body, sha256, added_by)
			VALUES ($1,'STOR',$2,$3,$4) RETURNING id`,
			caseID, string(pb), payloadSHA256(pb), actorID).
			Scan(&evID); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"stor evidence: "+err.Error())
		}
		if _, err := audit.Append(ctx, tx,
			"surveillance_case_evidence", &evID, "INSERT", nil); err != nil {
			return nil, excerrors.New("INTERNAL_ERROR",
				"stor chain: "+err.Error())
		}
		escalationRef = fmt.Sprintf("stor:%d", evID)

	case "ESCALATE_ACTION":
		if s.enf == nil {
			return nil, excerrors.New("SERVICE_DEGRADED",
				"enforcement seam unwired — cannot escalate")
		}
		action := req.Action
		if action == "" {
			action = EnfActionSuspend // spec default: account freeze
		}
		var sigID int64
		if c.SignalID != nil {
			sigID = *c.SignalID
		}
		act, err := s.enf.Enforce(ctx, EnforcementRequest{
			SignalID: sigID, CaseID: c.ID,
			AccountID: derefInt64(c.AccountID),
			Action:    action, Source: "MANUAL", ActorID: actorID,
			Note: req.Reason,
		})
		if err != nil {
			return nil, err
		}
		escalationRef = "enforcement:" + act.ActionID
	}

	_, err = tx.Exec(ctx, `
		UPDATE surveillance_cases
		   SET status = $2, closed_at = $3, disposition_reason = $4,
		       escalation_ref = $5, escalated_sar_id = $6,
		       first_reviewed_at = COALESCE(first_reviewed_at, $3),
		       updated_at = now()
		 WHERE id = $1`,
		caseID, to, now, req.Reason, escalationRef, sarID)
	if err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"case disposition: "+err.Error())
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorID, Action: "surveillance.case.disposition",
		TargetType: "surveillance_case", TargetID: &caseID,
		AfterState: map[string]any{
			"disposition": req.Disposition, "to": to,
			"reason": req.Reason, "escalation_ref": escalationRef,
			"sar_id": sarID,
		},
	}); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"disposition audit: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"disposition commit: "+err.Error())
	}
	return s.Get(ctx, caseID)
}

// ---------------------------------------------------------------------------
// SLA sweep + monthly summary
// ---------------------------------------------------------------------------

// SweepSLA flips overdue open cases to sla_breached and pages the AML
// Officer queue at P2 — the "overdue escalation" rung. Returns the
// count newly breached.
func (s *CaseService) SweepSLA(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE surveillance_cases
		   SET sla_breached = true,
		       sla_note = sla_note || ' breach@' || now()::text,
		       updated_at = now()
		 WHERE status IN ('OPEN','ASSIGNED','INVESTIGATING')
		   AND sla_breached = false
		   AND sla_deadline < now()
		RETURNING id, case_ref, severity, assigned_to`)
	if err != nil {
		return 0, excerrors.New("INTERNAL_ERROR", "sla sweep: "+err.Error())
	}
	defer rows.Close()
	var breached []map[string]any
	for rows.Next() {
		var id int64
		var ref, sev string
		var ass *int64
		if err := rows.Scan(&id, &ref, &sev, &ass); err != nil {
			return 0, excerrors.New("INTERNAL_ERROR", "sla row: "+err.Error())
		}
		breached = append(breached, map[string]any{
			"id": id, "ref": ref, "severity": sev, "assigned_to": ass,
		})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, b := range breached {
		if s.alerter != nil {
			_ = s.alerter.RaiseHold(ctx, HoldAlert{
				Severity: "P2", Code: "SURVEILLANCE_SLA_BREACH",
				Summary: fmt.Sprintf("case %v breached assignment SLA (%v)",
					b["ref"], b["severity"]),
				Details: map[string]string{
					"case_ref": fmt.Sprint(b["ref"]),
					"severity": fmt.Sprint(b["severity"]),
					"queue":    "AML Officer",
				},
			})
		}
	}
	return len(breached), nil
}

// MonthlySummary is the surveillance-effectiveness report for the
// monthly governance pack: opened / closed / escalated counts,
// per-signal accuracy (1 − false-positive share) and average
// disposition time.
type CaseMonthlySummary struct {
	Month            string             `json:"month"` // YYYY-MM
	Opened           int                `json:"opened"`
	Closed           int                `json:"closed"`
	Escalated        int                `json:"escalated"`
	FalsePositives   int                `json:"false_positives"`
	AvgDispositionH  float64            `json:"avg_disposition_hours"`
	AccuracyBySignal map[string]float64 `json:"accuracy_by_signal"` // 0..1
	OpenBreached     int                `json:"open_breached"`
}

// MonthlySummary computes the report for "YYYY-MM" (UTC).
func (s *CaseService) MonthlySummary(ctx context.Context,
	month string) (*CaseMonthlySummary, error) {
	if _, err := time.Parse("2006-01", month); err != nil {
		return nil, excerrors.New("INVALID_REQUEST",
			"month must be YYYY-MM")
	}
	m := &CaseMonthlySummary{Month: month,
		AccuracyBySignal: map[string]float64{}}
	if err := s.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE opened_at >= $1::date
		            AND opened_at <  ($1::date + interval '1 month')),
		  count(*) FILTER (WHERE closed_at >= $1::date
		            AND closed_at <  ($1::date + interval '1 month')),
		  count(*) FILTER (WHERE status IN
		            ('ESCALATED_SAR','ESCALATED_STR','ESCALATED_ACTION')
		            AND closed_at >= $1::date
		            AND closed_at <  ($1::date + interval '1 month')),
		  count(*) FILTER (WHERE status = 'CLOSED_FALSE_POSITIVE'
		            AND closed_at >= $1::date
		            AND closed_at <  ($1::date + interval '1 month')),
		  COALESCE(EXTRACT(EPOCH FROM avg(closed_at - opened_at)
		            FILTER (WHERE closed_at >= $1::date
		              AND closed_at <  ($1::date + interval '1 month')))
		            / 3600.0, 0),
		  count(*) FILTER (WHERE sla_breached
		            AND status IN ('OPEN','ASSIGNED','INVESTIGATING'))
		  FROM surveillance_cases`,
		month+"-01").
		Scan(&m.Opened, &m.Closed, &m.Escalated, &m.FalsePositives,
			&m.AvgDispositionH, &m.OpenBreached); err != nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"summary: "+err.Error())
	}
	rows, err := s.pool.Query(ctx, `
		SELECT signal_type,
		       count(*) FILTER (WHERE status <> 'CLOSED_FALSE_POSITIVE')::float
		         / NULLIF(count(*),0) AS accuracy
		  FROM surveillance_cases
		 WHERE closed_at >= $1::date
		   AND closed_at <  ($1::date + interval '1 month')
		 GROUP BY signal_type`, month+"-01")
	if err != nil {
		return m, nil // accuracy lane optional — report the counts
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var acc float64
		if err := rows.Scan(&st, &acc); err == nil {
			m.AccuracyBySignal[st] = acc
		}
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// STOR payload — UK MAR Art. 16 (Task 21.3.27 format seam)
// ---------------------------------------------------------------------------

// STORPayload is the exportable Suspicious Transaction and Order
// Report body pinned to the case (hash-chained evidence row).
type STORPayload struct {
	Format      string          `json:"format"` // "EXC-STOR/1.0"
	CaseRef     string          `json:"case_ref"`
	SignalID    *int64          `json:"signal_id,omitempty"`
	SignalType  string          `json:"signal_type"`
	Instrument  string          `json:"instrument"`
	AccountID   *int64          `json:"account_id,omitempty"`
	AccountHash string          `json:"account_hash"`
	Reason      string          `json:"reason"`
	Evidence    json.RawMessage `json:"evidence"`
	DetectedAt  time.Time       `json:"detected_at"`
	ReportedAt  time.Time       `json:"reported_at"`
}

// BuildSTORPayload renders the STOR body (exported through the STOR
// evidence kind + the reporting adapter).
func BuildSTORPayload(c *Case, reason string, now time.Time) *STORPayload {
	return &STORPayload{
		Format: "EXC-STOR/1.0", CaseRef: c.CaseRef,
		SignalID: c.SignalID, SignalType: c.SignalType,
		Instrument: c.Symbol, AccountID: c.AccountID,
		AccountHash: fmt.Sprintf("%d", c.AccountHash),
		Reason:      reason, Evidence: c.Evidence,
		DetectedAt: c.OpenedAt, ReportedAt: now,
	}
}

func payloadSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func isCaseTerminal(status string) bool {
	switch status {
	case CaseStatusClosedFP, CaseStatusEscalatedSAR,
		CaseStatusEscalatedSTR, CaseStatusEscalatedAct:
		return true
	}
	return false
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
