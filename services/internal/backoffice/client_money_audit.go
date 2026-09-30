// client_money_audit.go — Phase-24 Task 24.3.18 items 1/4/5: the
// independent client-money audit engagement register, EXTERNAL_AUDITOR
// access grants and the recorded independence review (spec §17.13.2,
// §24 #330).
//
//   - client_money_audits (mig 083) is the engagement register:
//     engagement year, auditor firm, scope, period, status
//     (SCHEDULED|FIELDWORK|DRAFT|ISSUED), evidence-request log, findings
//     and remediation tickets.
//   - external_auditor_grants are read-only, time-bounded, dual-controlled
//     grants into the assurance surface — every read is appended to the
//     grant's access_log (separately audited).
//   - RecordIndependence carries the §17.13.2 item-5 independence field:
//     the auditor holds no contract with any subsystem operator that
//     maintains client-money balances; reviewed annually.
package backoffice

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	excerrors "exchange/pkg/errors"
)

// Audit engagement statuses (client_money_audits.status).
const (
	AuditScheduled = "SCHEDULED"
	AuditFieldwork = "FIELDWORK"
	AuditDraft     = "DRAFT"
	AuditIssued    = "ISSUED"
)

// AuditorGrant statuses.
const (
	GrantActive  = "ACTIVE"
	GrantExpired = "EXPIRED"
	GrantRevoked = "REVOKED"
)

// ClientMoneyAudit is one client_money_audits engagement row.
type ClientMoneyAudit struct {
	ID                     int64           `json:"id"`
	EngagementYear         int             `json:"engagement_year"`
	AuditorFirm            string          `json:"auditor_firm"`
	Scope                  string          `json:"scope"`
	PeriodStart            time.Time       `json:"period_start"`
	PeriodEnd              time.Time       `json:"period_end"`
	Status                 string          `json:"status"`
	IndependenceConfirmed  bool            `json:"independence_confirmed"`
	IndependenceStatement  string          `json:"independence_statement,omitempty"`
	IndependenceReviewedAt *time.Time      `json:"independence_reviewed_at,omitempty"`
	EvidenceRequests       json.RawMessage `json:"evidence_requests"`
	Findings               json.RawMessage `json:"findings"`
	RemediationTickets     json.RawMessage `json:"remediation_tickets"`
	CreatedBy              int64           `json:"created_by"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

// AuditorGrant is one external_auditor_grants row — a read-only,
// time-bounded, dual-controlled window into the assurance surface.
type AuditorGrant struct {
	ID         int64           `json:"id"`
	AuditID    int64           `json:"audit_id"`
	AuditorRef string          `json:"auditor_ref"` // external identity / provisioned user ref
	GrantedBy  int64           `json:"granted_by"`
	ApprovedBy int64           `json:"approved_by"`
	ValidFrom  time.Time       `json:"valid_from"`
	ValidUntil time.Time       `json:"valid_until"`
	Status     string          `json:"status"`
	AccessLog  json.RawMessage `json:"access_log"`
	CreatedAt  time.Time       `json:"created_at"`
}

// AssuranceService drives the Task 24.3.18 engagement register, auditor
// grants and independence record. Evidence packs and certifications live
// in segregation_cert.go.
type AssuranceService struct {
	store   Store
	alerter OpsAlerter
	resolve RoleResolver
	now     func() time.Time
}

// AssuranceDeps wires the service.
type AssuranceDeps struct {
	Store    Store
	Alerter  OpsAlerter
	Resolver RoleResolver
	Now      func() time.Time
}

// NewAssuranceService constructs the service; Store and Resolver are
// mandatory (fail-closed).
func NewAssuranceService(d AssuranceDeps) (*AssuranceService, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("assurance: nil store")
	}
	if d.Resolver == nil {
		return nil, fmt.Errorf("assurance: nil role resolver")
	}
	s := &AssuranceService{store: d.Store, alerter: d.Alerter, resolve: d.Resolver, now: d.Now}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

func (s *AssuranceService) requireRole(ctx context.Context, userID int64, allowed map[string]bool) error {
	if userID <= 0 {
		return excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	role, err := s.resolve(ctx, userID)
	if err != nil {
		return excerrors.Wrap("UNAUTHORIZED_ROLE", "role lookup failed", err)
	}
	if !allowed[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("role %q lacks permission for this assurance operation", role))
	}
	return nil
}

func (s *AssuranceService) raise(ctx context.Context, severity, code, summary string, details map[string]string) {
	if s.alerter == nil {
		return
	}
	_ = s.alerter.Raise(ctx, OpsAlert{Severity: severity, Code: code, Summary: summary, Details: details})
}

// ---------------------------------------------------------------------------
// Engagement register (item 1)
// ---------------------------------------------------------------------------

// CreateAudit registers a scheduled engagement. Every operating period
// must be covered — CoverageGap reports un-covered years.
func (s *AssuranceService) CreateAudit(ctx context.Context, actor admin.AdminActor, a ClientMoneyAudit) (*ClientMoneyAudit, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	if a.AuditorFirm == "" || a.Scope == "" || a.PeriodEnd.Before(a.PeriodStart) || a.EngagementYear <= 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"audit requires auditor_firm, scope, a valid period and engagement_year")
	}
	a.Status = AuditScheduled
	a.CreatedBy = actor.UserID
	if len(a.EvidenceRequests) == 0 {
		a.EvidenceRequests = json.RawMessage(`[]`)
	}
	if len(a.Findings) == 0 {
		a.Findings = json.RawMessage(`[]`)
	}
	if len(a.RemediationTickets) == 0 {
		a.RemediationTickets = json.RawMessage(`[]`)
	}
	return s.store.InsertAudit(ctx, a)
}

// auditTransitions is the SCHEDULED→FIELDWORK→DRAFT→ISSUED state machine.
var auditTransitions = map[string]map[string]bool{
	AuditScheduled: {AuditFieldwork: true},
	AuditFieldwork: {AuditDraft: true},
	AuditDraft:     {AuditIssued: true},
}

// TransitionAudit advances the engagement lifecycle — strict order, no
// skipping or reopening.
func (s *AssuranceService) TransitionAudit(ctx context.Context, actor admin.AdminActor, auditID int64, to string) (*ClientMoneyAudit, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	var out *ClientMoneyAudit
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		a, err := tx.AuditByID(ctx, auditID)
		if err != nil {
			return err
		}
		if a == nil {
			return excerrors.New("NOT_FOUND", "audit engagement not found")
		}
		if !auditTransitions[a.Status][to] {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("audit %d cannot transition %s → %s", auditID, a.Status, to))
		}
		a.Status = to
		if err := tx.UpdateAudit(ctx, *a); err != nil {
			return err
		}
		out = a
		return nil
	})
	return out, err
}

// appendJSONLog appends one entry to a JSON-array column.
func appendJSONLog(raw json.RawMessage, entry any) (json.RawMessage, error) {
	var arr []json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "audit jsonb decode", err)
		}
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log entry encode", err)
	}
	arr = append(arr, b)
	out, err := json.Marshal(arr)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log encode", err)
	}
	return out, nil
}

// LogEvidenceRequest appends an auditor evidence request to the
// engagement's evidence-request log (item 1).
func (s *AssuranceService) LogEvidenceRequest(ctx context.Context, actor admin.AdminActor, auditID int64, request string) (*ClientMoneyAudit, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	if request == "" {
		return nil, excerrors.New("INVALID_REQUEST", "evidence request text required")
	}
	return s.appendAuditField(ctx, auditID, func(a *ClientMoneyAudit) error {
		log, err := appendJSONLog(a.EvidenceRequests, map[string]any{
			"request": request, "requested_at": s.now().Format(time.RFC3339Nano),
			"logged_by": actor.UserID,
		})
		if err != nil {
			return err
		}
		a.EvidenceRequests = log
		return nil
	})
}

// RecordFinding appends an audit finding plus its remediation ticket
// reference (item 1) — findings never mutate in place.
func (s *AssuranceService) RecordFinding(ctx context.Context, actor admin.AdminActor, auditID int64, finding, remediationTicket string) (*ClientMoneyAudit, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	if finding == "" {
		return nil, excerrors.New("INVALID_REQUEST", "finding text required")
	}
	return s.appendAuditField(ctx, auditID, func(a *ClientMoneyAudit) error {
		f, err := appendJSONLog(a.Findings, map[string]any{
			"finding": finding, "remediation_ticket": remediationTicket,
			"recorded_at": s.now().Format(time.RFC3339Nano), "recorded_by": actor.UserID,
		})
		if err != nil {
			return err
		}
		a.Findings = f
		if remediationTicket != "" {
			t, err := appendJSONLog(a.RemediationTickets, map[string]any{
				"ticket": remediationTicket, "finding": finding,
				"opened_at": s.now().Format(time.RFC3339Nano),
			})
			if err != nil {
				return err
			}
			a.RemediationTickets = t
		}
		return nil
	})
}

func (s *AssuranceService) appendAuditField(ctx context.Context, auditID int64, mutate func(*ClientMoneyAudit) error) (*ClientMoneyAudit, error) {
	var out *ClientMoneyAudit
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		a, err := tx.AuditByID(ctx, auditID)
		if err != nil {
			return err
		}
		if a == nil {
			return excerrors.New("NOT_FOUND", "audit engagement not found")
		}
		if a.Status == AuditIssued {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				"an ISSUED engagement is closed to mutation")
		}
		if err := mutate(a); err != nil {
			return err
		}
		if err := tx.UpdateAudit(ctx, *a); err != nil {
			return err
		}
		out = a
		return nil
	})
	return out, err
}

// ListAudits returns engagements, optionally status-filtered.
func (s *AssuranceService) ListAudits(ctx context.Context, actor admin.AdminActor, status string, limit int) ([]ClientMoneyAudit, error) {
	if err := s.requireRole(ctx, actor.UserID, readRoles); err != nil {
		return nil, err
	}
	return s.store.ListAudits(ctx, status, limit)
}

// ---------------------------------------------------------------------------
// Independence (item 5)
// ---------------------------------------------------------------------------

// RecordIndependence stamps the engagement's independence field — the
// auditor holds no contract with any subsystem operator maintaining
// client-money balances. The annual review cadence is enforced by
// requiring a fresh statement within 12 months of fieldwork.
func (s *AssuranceService) RecordIndependence(ctx context.Context, actor admin.AdminActor, auditID int64, statement string) (*ClientMoneyAudit, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	if statement == "" {
		return nil, excerrors.New("INVALID_REQUEST", "independence statement required")
	}
	return s.appendAuditField(ctx, auditID, func(a *ClientMoneyAudit) error {
		a.IndependenceConfirmed = true
		a.IndependenceStatement = statement
		now := s.now()
		a.IndependenceReviewedAt = &now
		return nil
	})
}

// ---------------------------------------------------------------------------
// EXTERNAL_AUDITOR access (item 4)
// ---------------------------------------------------------------------------

// GrantAuditorAccess issues a read-only, time-bounded grant — dual
// control: granted_by (actor) ≠ approved_by (actor.ApproverID), both
// role-eligible. The grant window is bounded by validUntil.
func (s *AssuranceService) GrantAuditorAccess(ctx context.Context, actor admin.AdminActor,
	auditID int64, auditorRef string, validUntil time.Time) (*AuditorGrant, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	if actor.ApproverID <= 0 {
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			"auditor access grants require a distinct approver_id")
	}
	if actor.ApproverID == actor.UserID {
		return nil, excerrors.New("DUAL_CONTROL_VIOLATION",
			"approver must differ from the granting principal")
	}
	if err := s.requireRole(ctx, actor.ApproverID, complianceOrFinance); err != nil {
		return nil, err
	}
	if auditorRef == "" || !validUntil.After(s.now()) {
		return nil, excerrors.New("INVALID_REQUEST",
			"auditor_ref and a future valid_until are required")
	}
	var out *AuditorGrant
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		a, err := tx.AuditByID(ctx, auditID)
		if err != nil {
			return err
		}
		if a == nil {
			return excerrors.New("NOT_FOUND", "audit engagement not found")
		}
		g, err := tx.InsertAuditorGrant(ctx, AuditorGrant{
			AuditID: auditID, AuditorRef: auditorRef,
			GrantedBy: actor.UserID, ApprovedBy: actor.ApproverID,
			ValidFrom: s.now(), ValidUntil: validUntil,
			Status: GrantActive, AccessLog: json.RawMessage(`[]`),
		})
		if err != nil {
			return err
		}
		out = g
		return nil
	})
	return out, err
}

// AssertAuditorAccess is the read-path gate for EXTERNAL_AUDITOR
// principals: the grant must be ACTIVE and inside its window — expiry
// mid-fieldwork flips it EXPIRED and refuses. Every granted read appends
// to the access_log (separately-audited trail, item 4).
func (s *AssuranceService) AssertAuditorAccess(ctx context.Context, grantID int64, auditorRef, resource string) (*AuditorGrant, error) {
	var out *AuditorGrant
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		g, err := tx.AuditorGrantByID(ctx, grantID)
		if err != nil {
			return err
		}
		if g == nil || g.AuditorRef != auditorRef {
			return excerrors.New("FORBIDDEN", "no external-auditor grant at that id")
		}
		now := s.now()
		if g.Status == GrantActive && !now.Before(g.ValidUntil) {
			g.Status = GrantExpired // window lapsed mid-fieldwork
			if err := tx.UpdateAuditorGrant(ctx, *g); err != nil {
				return err
			}
			return excerrors.New("FORBIDDEN",
				"external-auditor grant expired — access refused and grant closed")
		}
		if g.Status != GrantActive || now.Before(g.ValidFrom) {
			return excerrors.New("FORBIDDEN",
				fmt.Sprintf("external-auditor grant is %s", g.Status))
		}
		log, err := appendJSONLog(g.AccessLog, map[string]any{
			"resource": resource, "at": now.Format(time.RFC3339Nano),
			"auditor_ref": auditorRef,
		})
		if err != nil {
			return err
		}
		g.AccessLog = log
		if err := tx.UpdateAuditorGrant(ctx, *g); err != nil {
			return err
		}
		out = g
		return nil
	})
	return out, err
}

// RevokeAuditorAccess closes a grant early (Finance Ops/Compliance).
func (s *AssuranceService) RevokeAuditorAccess(ctx context.Context, actor admin.AdminActor, grantID int64) (*AuditorGrant, error) {
	if err := s.requireRole(ctx, actor.UserID, complianceOrFinance); err != nil {
		return nil, err
	}
	var out *AuditorGrant
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		g, err := tx.AuditorGrantByID(ctx, grantID)
		if err != nil {
			return err
		}
		if g == nil {
			return excerrors.New("NOT_FOUND", "grant not found")
		}
		if g.Status != GrantActive {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("grant %d already %s", grantID, g.Status))
		}
		g.Status = GrantRevoked
		if err := tx.UpdateAuditorGrant(ctx, *g); err != nil {
			return err
		}
		out = g
		return nil
	})
	return out, err
}

// --- PgStore: client_money_audits + external_auditor_grants -------------------

const auditCols = `id, engagement_year, auditor_firm, scope, period_start,
	period_end, status, independence_confirmed,
	COALESCE(independence_statement,''), independence_reviewed_at,
	evidence_requests, findings, remediation_tickets, created_by,
	created_at, updated_at`

func scanAudit(row pgx.Row) (*ClientMoneyAudit, error) {
	var a ClientMoneyAudit
	if err := row.Scan(&a.ID, &a.EngagementYear, &a.AuditorFirm, &a.Scope,
		&a.PeriodStart, &a.PeriodEnd, &a.Status, &a.IndependenceConfirmed,
		&a.IndependenceStatement, &a.IndependenceReviewedAt,
		&a.EvidenceRequests, &a.Findings, &a.RemediationTickets,
		&a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *PgStore) InsertAudit(ctx context.Context, a ClientMoneyAudit) (*ClientMoneyAudit, error) {
	return scanAudit(s.q.QueryRow(ctx, `
		INSERT INTO client_money_audits
		 (engagement_year, auditor_firm, scope, period_start, period_end,
		  status, evidence_requests, findings, remediation_tickets, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+auditCols,
		a.EngagementYear, a.AuditorFirm, a.Scope, a.PeriodStart, a.PeriodEnd,
		a.Status, a.EvidenceRequests, a.Findings, a.RemediationTickets, a.CreatedBy))
}

func (s *PgStore) AuditByID(ctx context.Context, id int64) (*ClientMoneyAudit, error) {
	a, err := scanAudit(s.q.QueryRow(ctx,
		`SELECT `+auditCols+` FROM client_money_audits WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (s *PgStore) UpdateAudit(ctx context.Context, a ClientMoneyAudit) error {
	_, err := s.q.Exec(ctx, `
		UPDATE client_money_audits
		   SET status=$2, independence_confirmed=$3, independence_statement=$4,
		       independence_reviewed_at=$5, evidence_requests=$6, findings=$7,
		       remediation_tickets=$8, updated_at=now()
		 WHERE id=$1`,
		a.ID, a.Status, a.IndependenceConfirmed, nilIfEmpty(a.IndependenceStatement),
		a.IndependenceReviewedAt, a.EvidenceRequests, a.Findings, a.RemediationTickets)
	return err
}

func (s *PgStore) ListAudits(ctx context.Context, status string, limit int) ([]ClientMoneyAudit, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + auditCols + ` FROM client_money_audits`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += ` WHERE status=$1`
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY engagement_year DESC, id DESC LIMIT $%d`, len(args))
	rows, err := s.q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClientMoneyAudit{}
	for rows.Next() {
		var a ClientMoneyAudit
		if err := rows.Scan(&a.ID, &a.EngagementYear, &a.AuditorFirm, &a.Scope,
			&a.PeriodStart, &a.PeriodEnd, &a.Status, &a.IndependenceConfirmed,
			&a.IndependenceStatement, &a.IndependenceReviewedAt,
			&a.EvidenceRequests, &a.Findings, &a.RemediationTickets,
			&a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const grantCols = `id, audit_id, auditor_ref, granted_by, approved_by,
	valid_from, valid_until, status, access_log, created_at`

func scanGrant(row pgx.Row) (*AuditorGrant, error) {
	var g AuditorGrant
	if err := row.Scan(&g.ID, &g.AuditID, &g.AuditorRef, &g.GrantedBy,
		&g.ApprovedBy, &g.ValidFrom, &g.ValidUntil, &g.Status, &g.AccessLog,
		&g.CreatedAt); err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *PgStore) InsertAuditorGrant(ctx context.Context, g AuditorGrant) (*AuditorGrant, error) {
	return scanGrant(s.q.QueryRow(ctx, `
		INSERT INTO external_auditor_grants
		 (audit_id, auditor_ref, granted_by, approved_by, valid_from,
		  valid_until, status, access_log)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+grantCols,
		g.AuditID, g.AuditorRef, g.GrantedBy, g.ApprovedBy, g.ValidFrom,
		g.ValidUntil, g.Status, g.AccessLog))
}

func (s *PgStore) AuditorGrantByID(ctx context.Context, id int64) (*AuditorGrant, error) {
	g, err := scanGrant(s.q.QueryRow(ctx,
		`SELECT `+grantCols+` FROM external_auditor_grants WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return g, err
}

func (s *PgStore) UpdateAuditorGrant(ctx context.Context, g AuditorGrant) error {
	_, err := s.q.Exec(ctx, `
		UPDATE external_auditor_grants SET status=$2, access_log=$3 WHERE id=$1`,
		g.ID, g.Status, g.AccessLog)
	return err
}
