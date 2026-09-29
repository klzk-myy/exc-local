// Package security implements the Phase-13.5 Task 13.5.3.8 standing
// Vulnerability Disclosure Program & coordinated bug bounty (spec
// §19.11.2, §24 #332) plus the severity-contract half of Task 13.5.3.9:
// fix-ETA-by-severity (Critical 7d / High 30d / Medium 90d / Low 180d),
// CVSS 3.1 base scoring on every triaged report, and the sbom_ref
// linkage every report carries once triaged.
//
// Intake paths share one register (migration 081) and one SLA clock:
//   - researcher reports via the public POST /api/v1/security/disclosures
//     (unauthenticated, TierPublic-rate-limited, honeypot-filtered,
//     idempotent on report_id);
//   - scheduled external pentest findings via the admin intake endpoint
//     (source=PENTEST — same queue, same clock per spec item 5);
//   - staff-found issues via the same intake (source=INTERNAL).
//
// Status enum is the spec F7 set TRIAGED|IN_PROGRESS|FIXED|DISPUTED|
// REJECTED — intake is implicit in report creation, triage is the first
// explicit status (spec §19.11.2 item 3; the phase doc's older INTAKED
// variant is superseded). DISPUTED is a real path, not a dead end:
// DISPUTED → IN_PROGRESS (report upheld after review), DISPUTED → FIXED
// (fix landed while disputed), DISPUTED → REJECTED (rejected after
// second review); REJECTED → DISPUTED is the researcher's rebuttal lane.
//
// SLA milestones (system-written once-only, DB-trigger-enforced):
// acknowledge ≤72h (ack_due_at), triage ≤5 business days
// (triage_due_at), fix by severity ETA (fix_due_at set at triage). The
// 60s sweeper claims each breached clock once and raises VDP_SLA_BREACH
// (P2, Security/DevOps) through the Alerter seam — an internal ops alert
// on the ops.alerts.* family, deliberately not a §23 API code.
//
// Coordinated patching: dependent disclosures carry the same
// bulletin_ref so they remediate together under one security bulletin
// (spec item 4); the bulletin is a grouping field, not per-report noise.
//
// Change-freeze rule (SDD edge case, documented in
// content/security/policy.md): a CRITICAL disclosure triaged while the
// deployment change-freeze flag (vdp_change_freeze) is active is marked
// expedited_path=TRUE and a P1 expedite notice is raised — the fix ships
// through the emergency change lane rather than waiting out the freeze.
// Non-critical reports keep their normal SLA clocks during a freeze.
//
// Every admin mutation writes admin_audit_log + the audit_hash_chain
// link inside the same transaction via internal/admin.Log — a VDP state
// change cannot commit without its audit trail (spec §5.9/§5.40.3).
package security

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// Sources — one register for researcher reports, pentest findings and
// internally found issues (spec §19.11.2 item 5).
const (
	SourceResearcher = "RESEARCHER"
	SourcePentest    = "PENTEST"
	SourceInternal   = "INTERNAL"
)

// Severities — the Task 13.5.3.9 severity contract. Fix ETAs are
// wall-clock from triage; the canonical public table lives in
// content/security/policy.md and MUST match FixETA below.
const (
	SeverityCritical = "CRITICAL"
	SeverityHigh     = "HIGH"
	SeverityMedium   = "MEDIUM"
	SeverityLow      = "LOW"
)

// Statuses — spec §19.11.2 F7 enum (INTAKED superseded by TRIAGED).
const (
	StatusTriaged    = "TRIAGED"
	StatusInProgress = "IN_PROGRESS"
	StatusFixed      = "FIXED"
	StatusDisputed   = "DISPUTED"
	StatusRejected   = "REJECTED"
)

// Published SLAs (spec §19.11.2 item 1 + Task 13.5.3.9 item 2).
const (
	AckSLA           = 72 * time.Hour // acknowledge ≤72h (wall clock)
	TriageSLAWorkday = 5              // triage ≤5 business days
)

// FixETA returns the fix deadline duration for a severity — the
// Task 13.5.3.9 contract table. Unknown severity → 0, false.
func FixETA(sev string) (time.Duration, bool) {
	switch sev {
	case SeverityCritical:
		return 7 * 24 * time.Hour, true
	case SeverityHigh:
		return 30 * 24 * time.Hour, true
	case SeverityMedium:
		return 90 * 24 * time.Hour, true
	case SeverityLow:
		return 180 * 24 * time.Hour, true
	}
	return 0, false
}

// SeverityFromCVSS maps a CVSS 3.1 base score to the program severity
// band (Task 13.5.3.9). 0.0 returns false — a zero score still needs an
// explicit human severity call at triage (fail closed).
func SeverityFromCVSS(score float64) (string, bool) {
	switch {
	case score <= 0:
		return "", false
	case score < 4.0:
		return SeverityLow, true
	case score < 7.0:
		return SeverityMedium, true
	case score < 9.0:
		return SeverityHigh, true
	case score <= 10.0:
		return SeverityCritical, true
	}
	return "", false
}

// transitions is the disclosure state machine. DISPUTED has real exits
// in both directions (upheld → IN_PROGRESS, confirmed-already-fixed →
// FIXED, second-rejection → REJECTED); REJECTED can be rebutted back
// into DISPUTED; FIXED is terminal.
var transitions = map[string][]string{
	StatusTriaged:    {StatusInProgress, StatusDisputed, StatusRejected},
	StatusInProgress: {StatusFixed, StatusDisputed, StatusRejected},
	StatusDisputed:   {StatusInProgress, StatusFixed, StatusRejected},
	StatusRejected:   {StatusDisputed},
	StatusFixed:      {},
}

// ValidTransition reports whether from→to is an allowed move.
func ValidTransition(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// ValidSeverity / ValidStatus / ValidSource gate input.
func ValidSeverity(s string) bool {
	switch s {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow:
		return true
	}
	return false
}

func ValidStatus(s string) bool {
	switch s {
	case StatusTriaged, StatusInProgress, StatusFixed, StatusDisputed, StatusRejected:
		return true
	}
	return false
}

func ValidSource(s string) bool {
	switch s {
	case SourceResearcher, SourcePentest, SourceInternal:
		return true
	}
	return false
}

// businessDays adds n Mon–Fri days to t (24/5 venue calendar — weekend
// days do not count against the 5-business-day triage SLA).
func businessDays(t time.Time, n int) time.Time {
	t = t.UTC()
	added := 0
	for added < n {
		t = t.Add(24 * time.Hour)
		if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday {
			added++
		}
	}
	return t
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// Disclosure is one vulnerability_disclosures row.
type Disclosure struct {
	ID                   int64      `json:"id"`
	ReportID             string     `json:"report_id"`
	Source               string     `json:"source"`
	Title                string     `json:"title"`
	AffectedComponents   []string   `json:"affected_components"`
	Reproduction         string     `json:"reproduction,omitempty"`
	ReporterHandle       string     `json:"reporter_handle,omitempty"`
	ContactEmail         string     `json:"contact_email,omitempty"`
	SuggestedSeverity    *string    `json:"suggested_severity,omitempty"`
	Severity             *string    `json:"severity,omitempty"`
	CVSSScore            *float64   `json:"cvss_score,omitempty"`
	CVSSVector           string     `json:"cvss_vector,omitempty"`
	Status               string     `json:"status"`
	AssigneeAdminID      *int64     `json:"assignee_admin_id,omitempty"`
	BulletinRef          string     `json:"bulletin_ref,omitempty"`
	PatchRef             string     `json:"patch_ref,omitempty"`
	SBOMRef              string     `json:"sbom_ref,omitempty"`
	FixDueAt             *time.Time `json:"fix_due_at,omitempty"`
	ExpeditedPath        bool       `json:"expedited_path"`
	AttributionRequested bool       `json:"attribution_requested"`
	ResolutionSummary    string     `json:"resolution_summary,omitempty"`
	DisputeReason        string     `json:"dispute_reason,omitempty"`
	// SLA milestones — system-written once-only evidence.
	SubmittedAt              time.Time  `json:"submitted_at"`
	AckDueAt                 time.Time  `json:"ack_due_at"`
	AcknowledgedAt           *time.Time `json:"acknowledged_at,omitempty"`
	TriageDueAt              time.Time  `json:"triage_due_at"`
	TriagedAt                *time.Time `json:"triaged_at,omitempty"`
	FixedAt                  *time.Time `json:"fixed_at,omitempty"`
	DisputedAt               *time.Time `json:"disputed_at,omitempty"`
	RejectedAt               *time.Time `json:"rejected_at,omitempty"`
	ResearcherAcknowledgedAt *time.Time `json:"researcher_acknowledged_at,omitempty"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
	// Computed breach flags (not stored): any SLA clock past due on an
	// unresolved report at read time.
	AckBreached    bool `json:"ack_breached"`
	TriageBreached bool `json:"triage_breached"`
	FixBreached    bool `json:"fix_breached"`
}

// unresolved reports are every status that is not FIXED or REJECTED.
func unresolved(status string) bool {
	return status != StatusFixed && status != StatusRejected
}

func (d *Disclosure) markBreach(now time.Time) {
	if !unresolved(d.Status) {
		return
	}
	d.AckBreached = d.AcknowledgedAt == nil && now.After(d.AckDueAt)
	d.TriageBreached = d.TriagedAt == nil && now.After(d.TriageDueAt)
	d.FixBreached = d.FixDueAt != nil && d.FixedAt == nil && now.After(*d.FixDueAt)
}

// SubmitRequest is the intake payload — shared by the public researcher
// form and the admin pentest/internal intake.
type SubmitRequest struct {
	ReportID             string // idempotency key; generated when empty
	Source               string // RESEARCHER | PENTEST | INTERNAL
	Title                string
	AffectedComponents   []string
	Reproduction         string
	ReporterHandle       string
	ContactEmail         string
	SuggestedSeverity    string
	AttributionRequested bool
	Now                  time.Time
}

// TriageInput is the first-explicit-status mutation: severity + CVSS
// assignment (the Task 13.5.3.9 contract — every report carries CVSS,
// severity ETA and SBOM linkage after triage).
type TriageInput struct {
	ID              int64
	Severity        string   // explicit severity; when empty, derived from CVSSScore
	CVSSScore       *float64 // CVSS 3.1 base score — required on every triaged report
	CVSSVector      string
	AssigneeAdminID *int64
	AssignToMe      bool
	BulletinRef     string
	SBOMRef         string
}

// AdminUpdate is the managed transition/metadata surface.
type AdminUpdate struct {
	ID                         int64
	Status                     string // optional transition target
	AssigneeAdminID            *int64
	AssignToMe                 bool
	BulletinRef                *string // pointer: distinguish "unchanged" from "clear"
	PatchRef                   *string
	SBOMRef                    *string
	ResolutionSummary          *string
	DisputeReason              *string
	Acknowledge                bool   // stamp acknowledged_at = now (first touch already does)
	MarkResearcherAcknowledged bool   // honor attribution / researcher ack → researcher_acknowledged_at
	Severity                   string // re-grade (post-triage severity change re-bases fix_due_at)
	CVSSScore                  *float64
	CVSSVector                 *string
}

// AdminFilter narrows the admin register list.
type AdminFilter struct {
	Status       string
	Severity     string
	Source       string
	BulletinRef  string
	AssigneeID   *int64
	Unassigned   bool
	BreachedOnly bool
}

// RoleResolver resolves an admin's §8.2 role name — the Phase-07 Task
// 7.3.1 seam (same shape as support.RoleResolver); nil fails closed
// with UNAUTHORIZED_ROLE.
type RoleResolver func(ctx context.Context, adminUserID int64) (string, error)

// Alerter raises operational alerts (PagerDuty in production). The SLA
// sweeper reports VDP_SLA_BREACH (P2) through it; nil-safe (log-only).
type Alerter interface {
	Raise(ctx context.Context, severity, code, message string) error
}

// FreezeCheck reports whether the deployment change freeze is active.
// Wired in cmd/gateway to the vdp_change_freeze feature flag; nil means
// "never frozen" (dev/test default).
type FreezeCheck func(ctx context.Context) bool

// vdpRoles: triage/mutation requires the security-operating roles —
// Compliance Officer (disclosure/legal owner) or Super Admin. There is
// no dedicated security-engineer role in the §8.2 canon; Support Agent /
// Risk Manager / Finance Ops never touch the register. Read-Only Auditor
// gets read access for evidence review.
var (
	vdpWriteRoles = map[string]bool{
		"Super Admin": true, "Compliance Officer": true,
	}
	vdpReadRoles = map[string]bool{
		"Super Admin": true, "Compliance Officer": true, "Read-Only Auditor": true,
	}
)

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service is the VDP register workflow. All admin mutations run in one
// transaction: row lock, transition validation, update, admin_audit_log
// + hash-chain link.
type Service struct {
	pool        *pgxpool.Pool
	resolver    RoleResolver
	alerter     Alerter
	freezeCheck FreezeCheck
}

// NewService wires the production service. resolver nil → admin ops
// fail closed UNAUTHORIZED_ROLE; alerter nil → SLA breaches log-only.
func NewService(pool *pgxpool.Pool, resolver RoleResolver, alerter Alerter) *Service {
	return &Service{pool: pool, resolver: resolver, alerter: alerter}
}

// WithChangeFreeze installs the change-freeze probe consulted at triage
// for the critical-disclosure expedited path.
func (s *Service) WithChangeFreeze(fc FreezeCheck) *Service {
	s.freezeCheck = fc
	return s
}

// role resolves the actor's role and fails closed when the seam is
// absent.
func (s *Service) role(ctx context.Context, adminID int64) (string, error) {
	if adminID <= 0 {
		return "", excerrors.New("UNAUTHORIZED", "admin identity required")
	}
	if s.resolver == nil {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC stub boundary)")
	}
	role, err := s.resolver(ctx, adminID)
	if err != nil {
		return "", excerrors.Wrap("INTERNAL_ERROR", "role lookup", err)
	}
	return role, nil
}

func (s *Service) requireWrite(ctx context.Context, adminID int64) (string, error) {
	role, err := s.role(ctx, adminID)
	if err != nil {
		return "", err
	}
	if !vdpWriteRoles[role] {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("VDP triage requires Compliance Officer or Super Admin (got %q)", role))
	}
	return role, nil
}

func (s *Service) requireRead(ctx context.Context, adminID int64) (string, error) {
	role, err := s.role(ctx, adminID)
	if err != nil {
		return "", err
	}
	if !vdpReadRoles[role] {
		return "", excerrors.New("UNAUTHORIZED_ROLE",
			fmt.Sprintf("role %q may not read the VDP register", role))
	}
	return role, nil
}

const disclosureCols = `id, report_id, source::text, title, affected_components,
	reproduction, COALESCE(reporter_handle,''), COALESCE(contact_email,''),
	suggested_severity::text, severity::text, cvss_score, COALESCE(cvss_vector,''),
	status::text, assignee_admin_id, COALESCE(bulletin_ref,''),
	COALESCE(patch_ref,''), COALESCE(sbom_ref,''), fix_due_at, expedited_path,
	attribution_requested, COALESCE(resolution_summary,''), COALESCE(dispute_reason,''),
	submitted_at, ack_due_at, acknowledged_at, triage_due_at, triaged_at,
	fixed_at, disputed_at, rejected_at, researcher_acknowledged_at,
	created_at, updated_at`

// scanDisclosure reads one row. NULL enum scans arrive as *string via
// pgx — suggested_severity/severity arrive as *string already.
func scanDisclosure(row pgx.Row) (*Disclosure, error) {
	var d Disclosure
	err := row.Scan(&d.ID, &d.ReportID, &d.Source, &d.Title, &d.AffectedComponents,
		&d.Reproduction, &d.ReporterHandle, &d.ContactEmail,
		&d.SuggestedSeverity, &d.Severity, &d.CVSSScore, &d.CVSSVector,
		&d.Status, &d.AssigneeAdminID, &d.BulletinRef,
		&d.PatchRef, &d.SBOMRef, &d.FixDueAt, &d.ExpeditedPath,
		&d.AttributionRequested, &d.ResolutionSummary, &d.DisputeReason,
		&d.SubmittedAt, &d.AckDueAt, &d.AcknowledgedAt, &d.TriageDueAt,
		&d.TriagedAt, &d.FixedAt, &d.DisputedAt, &d.RejectedAt,
		&d.ResearcherAcknowledgedAt, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Service) get(ctx context.Context, id int64) (*Disclosure, error) {
	d, err := scanDisclosure(s.pool.QueryRow(ctx,
		`SELECT `+disclosureCols+` FROM vulnerability_disclosures WHERE id = $1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("DISCLOSURE_NOT_FOUND",
			fmt.Sprintf("disclosure %d not found", id))
	}
	if err != nil {
		return nil, fmt.Errorf("disclosure read: %w", err)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Intake (researcher + pentest + internal — one register, one clock)
// ---------------------------------------------------------------------------

// Submit validates and stores a report. Idempotent on report_id: a
// resubmission with the same key returns the existing row with
// created=false — anonymous reporters retry safely on timeouts. SLA
// deadlines are computed at insert so the evidence is written with the
// row (ack ≤72h wall-clock; triage ≤5 business days).
func (s *Service) Submit(ctx context.Context, req SubmitRequest) (*Disclosure, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	d, created, err := s.submitTx(ctx, tx, req)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return d, created, nil
}

func (s *Service) getByReportID(ctx context.Context, reportID string) (*Disclosure, bool, error) {
	d, err := scanDisclosure(s.pool.QueryRow(ctx,
		`SELECT `+disclosureCols+` FROM vulnerability_disclosures
		  WHERE report_id = $1`, reportID))
	if err == pgx.ErrNoRows {
		return nil, false, excerrors.New("DISCLOSURE_NOT_FOUND",
			fmt.Sprintf("report %q not found", reportID))
	}
	if err != nil {
		return nil, false, fmt.Errorf("disclosure read: %w", err)
	}
	return d, false, nil
}

// IngestPentest files a scheduled-external-pentest finding into the same
// register — same queue, same SLA clock (spec item 5). Admin-gated:
// pentest findings arrive through the authorized pentest vendor's report
// or an internal security engineer, never the anonymous form.
func (s *Service) IngestPentest(ctx context.Context, adminID int64, req SubmitRequest, clientIP string) (*Disclosure, error) {
	if _, err := s.requireWrite(ctx, adminID); err != nil {
		return nil, err
	}
	if req.Source != SourcePentest && req.Source != SourceInternal {
		return nil, excerrors.New("INVALID_REQUEST",
			"admin intake source must be PENTEST|INTERNAL (researcher reports use the public form)")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	d, created, err := s.submitTx(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	if created {
		// Staff-filed findings are acknowledged on entry — the ack SLA
		// measures unanswered external reporters, not the team itself.
		if _, err := tx.Exec(ctx, `
			UPDATE vulnerability_disclosures
			   SET acknowledged_at = now(), updated_at = now()
			 WHERE id = $1`, d.ID); err != nil {
			return nil, fmt.Errorf("intake ack stamp: %w", err)
		}
	}
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "vdp.disclosure.intake",
		TargetType:  "vulnerability_disclosure",
		TargetID:    &d.ID,
		AfterState: map[string]any{
			"report_id": d.ReportID, "source": d.Source, "created": created,
		},
		IPAddress: clientIP,
	}); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return d, nil
}

// submitTx is the transaction-scoped insert shared by Submit and the
// audited admin intake.
func (s *Service) submitTx(ctx context.Context, tx pgx.Tx, req SubmitRequest) (*Disclosure, bool, error) {
	if req.Title == "" || len(req.Title) > 255 {
		return nil, false, excerrors.New("INVALID_REQUEST", "title is required (≤255 chars)")
	}
	if strings.TrimSpace(req.Reproduction) == "" {
		return nil, false, excerrors.New("INVALID_REQUEST", "reproduction steps are required")
	}
	if len(req.Reproduction) > 1<<15 {
		return nil, false, excerrors.New("INVALID_REQUEST", "reproduction exceeds 32KB")
	}
	source := req.Source
	if source == "" {
		source = SourceResearcher
	}
	if !ValidSource(source) {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"source must be RESEARCHER|PENTEST|INTERNAL")
	}
	if req.SuggestedSeverity != "" && !ValidSeverity(req.SuggestedSeverity) {
		return nil, false, excerrors.New("INVALID_REQUEST",
			"suggested_severity must be CRITICAL|HIGH|MEDIUM|LOW")
	}
	for _, c := range req.AffectedComponents {
		if len(c) == 0 || len(c) > 64 {
			return nil, false, excerrors.New("INVALID_REQUEST",
				"affected_components entries must be 1–64 chars")
		}
	}
	if len(req.ReportID) > 64 || len(req.ReporterHandle) > 128 ||
		len(req.ContactEmail) > 320 {
		return nil, false, excerrors.New("INVALID_REQUEST", "field length exceeded")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	reportID := req.ReportID
	if reportID == "" {
		reportID = "VDP-" + strconv.FormatInt(now.UnixNano(), 36)
	}
	var suggested *string
	if req.SuggestedSeverity != "" {
		v := req.SuggestedSeverity
		suggested = &v
	}
	components := req.AffectedComponents
	if components == nil {
		components = []string{} // nil slice marshals to NULL, not '{}'
	}
	d, err := scanDisclosure(tx.QueryRow(ctx, `
		INSERT INTO vulnerability_disclosures
		    (report_id, source, title, affected_components, reproduction,
		     reporter_handle, contact_email, suggested_severity,
		     attribution_requested, status, submitted_at,
		     ack_due_at, triage_due_at)
		VALUES ($1,$2,$3,$4,$5, NULLIF($6,''), NULLIF($7,''), $8,
		        $9, 'TRIAGED', $10, $11, $12)
		ON CONFLICT (report_id) DO NOTHING
		RETURNING `+disclosureCols,
		reportID, source, req.Title, components, req.Reproduction,
		req.ReporterHandle, req.ContactEmail, suggested,
		req.AttributionRequested, now,
		now.Add(AckSLA), businessDays(now, TriageSLAWorkday)))
	if err == pgx.ErrNoRows {
		d, serr := scanDisclosure(tx.QueryRow(ctx,
			`SELECT `+disclosureCols+` FROM vulnerability_disclosures
			  WHERE report_id = $1`, reportID))
		if serr != nil {
			return nil, false, fmt.Errorf("disclosure reread: %w", serr)
		}
		return d, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("disclosure insert: %w", err)
	}
	return d, true, nil
}

// ---------------------------------------------------------------------------
// Triage — first explicit status action (Task 13.5.3.9 contract)
// ---------------------------------------------------------------------------

// Triage assigns severity + CVSS + the fix ETA and moves the report to
// IN_PROGRESS. Contract: every report carries a CVSS score at triage —
// severity may be declared explicitly or derived from the score; a 0.0
// score without an explicit severity fails closed. triaged_at is
// write-once SLA evidence (re-triage updates CVSS/severity but never
// rewrites the milestone).
//
// Change-freeze rule: when the deployment freeze is active and the
// report triages CRITICAL, expedited_path is set and a P1 expedite
// notice fires — the fix ships through the emergency change lane.
func (s *Service) Triage(ctx context.Context, adminID int64, in TriageInput, clientIP string) (*Disclosure, error) {
	if in.ID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "disclosure id is required")
	}
	if _, err := s.requireWrite(ctx, adminID); err != nil {
		return nil, err
	}
	sev := in.Severity
	if sev == "" {
		if in.CVSSScore == nil {
			return nil, excerrors.New("INVALID_REQUEST",
				"severity or cvss_score is required — every report carries CVSS (Task 13.5.3.9)")
		}
		derived, ok := SeverityFromCVSS(*in.CVSSScore)
		if !ok {
			return nil, excerrors.New("INVALID_REQUEST",
				"cvss_score 0.0 requires an explicit severity call")
		}
		sev = derived
	} else if !ValidSeverity(sev) {
		return nil, excerrors.New("INVALID_REQUEST",
			"severity must be CRITICAL|HIGH|MEDIUM|LOW")
	}
	if in.CVSSScore == nil {
		return nil, excerrors.New("INVALID_REQUEST",
			"cvss_score is required on every triaged report (Task 13.5.3.9)")
	}
	if *in.CVSSScore < 0 || *in.CVSSScore > 10 {
		return nil, excerrors.New("INVALID_REQUEST", "cvss_score must be 0.0–10.0")
	}
	eta, _ := FixETA(sev)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := scanDisclosure(tx.QueryRow(ctx,
		`SELECT `+disclosureCols+` FROM vulnerability_disclosures
		  WHERE id = $1 FOR UPDATE`, in.ID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("DISCLOSURE_NOT_FOUND",
			fmt.Sprintf("disclosure %d not found", in.ID))
	}
	if err != nil {
		return nil, fmt.Errorf("disclosure lock: %w", err)
	}
	if !unresolved(before.Status) {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("cannot triage a %s disclosure", before.Status))
	}

	now := time.Now().UTC()
	fixDue := now.Add(eta)
	expedited := false
	if sev == SeverityCritical && s.freezeCheck != nil && s.freezeCheck(ctx) {
		// Change-freeze expedite: the critical fix must not wait out the
		// window — flag the emergency change lane.
		expedited = true
	}

	sets := []string{"updated_at = now()"}
	args := []any{}
	next := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	sets = append(sets, "severity = "+next(sev))
	sets = append(sets, "cvss_score = "+next(*in.CVSSScore))
	sets = append(sets, "fix_due_at = "+next(fixDue))
	if in.CVSSVector != "" {
		sets = append(sets, "cvss_vector = "+next(in.CVSSVector))
	}
	if before.TriagedAt == nil {
		sets = append(sets, "triaged_at = "+next(now))
	}
	if before.AcknowledgedAt == nil {
		// Triage is the first staff touch — acknowledge in the same act.
		sets = append(sets, "acknowledged_at = "+next(now))
	}
	if in.AssignToMe {
		sets = append(sets, "assignee_admin_id = "+next(adminID))
	} else if in.AssigneeAdminID != nil {
		sets = append(sets, "assignee_admin_id = "+next(*in.AssigneeAdminID))
	}
	if in.BulletinRef != "" {
		sets = append(sets, "bulletin_ref = "+next(in.BulletinRef))
	}
	if in.SBOMRef != "" {
		sets = append(sets, "sbom_ref = "+next(in.SBOMRef))
	}
	if expedited {
		sets = append(sets, "expedited_path = TRUE")
	}
	// First triage moves the report to IN_PROGRESS; later re-triage keeps
	// the current status (re-grades happen on disputed/in-progress work).
	if before.Status == StatusTriaged {
		sets = append(sets, "status = 'IN_PROGRESS'")
	}

	after, err := scanDisclosure(tx.QueryRow(ctx, `
		UPDATE vulnerability_disclosures SET `+joinCSV(sets)+`
		 WHERE id = `+next(in.ID)+`
		RETURNING `+disclosureCols, args...))
	if err != nil {
		return nil, fmt.Errorf("disclosure triage update: %w", err)
	}

	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "vdp.disclosure.triage",
		TargetType:  "vulnerability_disclosure",
		TargetID:    &in.ID,
		BeforeState: disclosureAuditState(before),
		AfterState:  disclosureAuditState(after),
		IPAddress:   clientIP,
	}); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	if expedited && s.alerter != nil {
		if err := s.alerter.Raise(ctx, "P1", "VDP_EXPEDITED_FIX",
			fmt.Sprintf("disclosure %d (%s) triaged CRITICAL inside a change-freeze window — expedited emergency change lane armed",
				after.ID, after.ReportID)); err != nil {
			// Alert dispatch failure must not wedge the committed triage —
			// logged via the caller; the row carries expedited_path either way.
		}
	}
	return after, nil
}

// ---------------------------------------------------------------------------
// Managed update — transitions, assignment, bulletin grouping, attribution
// ---------------------------------------------------------------------------

// Update applies the admin mutation in one transaction: row lock,
// transition validation, field update, admin_audit_log + chain link.
// Milestone timestamps are only ever stamped by this path (or Triage) —
// never accepted from the client.
func (s *Service) Update(ctx context.Context, adminID int64, u AdminUpdate, clientIP string) (*Disclosure, error) {
	if u.ID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "disclosure id is required")
	}
	if u.Status == "" && u.AssigneeAdminID == nil && !u.AssignToMe &&
		u.BulletinRef == nil && u.PatchRef == nil && u.SBOMRef == nil &&
		u.ResolutionSummary == nil && u.DisputeReason == nil && !u.Acknowledge &&
		!u.MarkResearcherAcknowledged && u.Severity == "" && u.CVSSScore == nil &&
		u.CVSSVector == nil {
		return nil, excerrors.New("INVALID_REQUEST", "no mutation fields supplied")
	}
	if u.Status != "" && !ValidStatus(u.Status) {
		return nil, excerrors.New("INVALID_REQUEST", "invalid status")
	}
	if _, err := s.requireWrite(ctx, adminID); err != nil {
		return nil, err
	}
	if u.Severity != "" && !ValidSeverity(u.Severity) {
		return nil, excerrors.New("INVALID_REQUEST",
			"severity must be CRITICAL|HIGH|MEDIUM|LOW")
	}
	if u.CVSSScore != nil && (*u.CVSSScore < 0 || *u.CVSSScore > 10) {
		return nil, excerrors.New("INVALID_REQUEST", "cvss_score must be 0.0–10.0")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := scanDisclosure(tx.QueryRow(ctx,
		`SELECT `+disclosureCols+` FROM vulnerability_disclosures
		  WHERE id = $1 FOR UPDATE`, u.ID))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("DISCLOSURE_NOT_FOUND",
			fmt.Sprintf("disclosure %d not found", u.ID))
	}
	if err != nil {
		return nil, fmt.Errorf("disclosure lock: %w", err)
	}

	now := time.Now().UTC()
	sets := []string{"updated_at = now()"}
	args := []any{}
	next := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	if u.Status != "" && u.Status != before.Status {
		if !ValidTransition(before.Status, u.Status) {
			return nil, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("invalid status transition %s → %s", before.Status, u.Status))
		}
		sets = append(sets, "status = "+next(u.Status))
		switch u.Status {
		case StatusFixed:
			sets = append(sets, "fixed_at = "+next(now))
		case StatusDisputed:
			sets = append(sets, "disputed_at = "+next(now))
		case StatusRejected:
			sets = append(sets, "rejected_at = "+next(now))
		}
	}
	// Any staff mutation acknowledges the report — the 72h SLA measures
	// silence, and a successful update is proof a human looked.
	// (u.Acknowledge covers the explicit no-change ack case.)
	if before.AcknowledgedAt == nil {
		sets = append(sets, "acknowledged_at = "+next(now))
	}
	if u.AssignToMe {
		sets = append(sets, "assignee_admin_id = "+next(adminID))
	} else if u.AssigneeAdminID != nil {
		sets = append(sets, "assignee_admin_id = "+next(*u.AssigneeAdminID))
	}
	if u.BulletinRef != nil {
		sets = append(sets, "bulletin_ref = "+next(nullableStr(*u.BulletinRef)))
	}
	if u.PatchRef != nil {
		sets = append(sets, "patch_ref = "+next(nullableStr(*u.PatchRef)))
	}
	if u.SBOMRef != nil {
		sets = append(sets, "sbom_ref = "+next(nullableStr(*u.SBOMRef)))
	}
	if u.ResolutionSummary != nil {
		sets = append(sets, "resolution_summary = "+next(*u.ResolutionSummary))
	}
	if u.DisputeReason != nil {
		sets = append(sets, "dispute_reason = "+next(*u.DisputeReason))
	}
	if u.MarkResearcherAcknowledged && before.ResearcherAcknowledgedAt == nil {
		// Attribution honored / researcher acknowledged the resolution —
		// write-once evidence.
		sets = append(sets, "researcher_acknowledged_at = "+next(now))
	}
	// Severity re-grade: allowed on unresolved reports; re-bases
	// fix_due_at (fix_due_at is the one mutable deadline — the audit row
	// carries the before/after image). Re-grade of an untriaged report
	// is a Triage call's job.
	if u.Severity != "" || u.CVSSScore != nil || u.CVSSVector != nil {
		if before.TriagedAt == nil {
			return nil, excerrors.New("INVALID_REQUEST",
				"severity/CVSS set at triage — use the triage endpoint first")
		}
		sev := before.Severity
		if u.Severity != "" {
			sev = &u.Severity
		}
		if u.Severity != "" {
			sets = append(sets, "severity = "+next(u.Severity))
		}
		if u.CVSSScore != nil {
			sets = append(sets, "cvss_score = "+next(*u.CVSSScore))
		}
		if u.CVSSVector != nil {
			sets = append(sets, "cvss_vector = "+next(*u.CVSSVector))
		}
		if sev != nil {
			if eta, ok := FixETA(*sev); ok {
				sets = append(sets, "fix_due_at = "+next(now.Add(eta)))
			}
		}
	}

	after, err := scanDisclosure(tx.QueryRow(ctx, `
		UPDATE vulnerability_disclosures SET `+joinCSV(sets)+`
		 WHERE id = `+next(u.ID)+`
		RETURNING `+disclosureCols, args...))
	if err != nil {
		return nil, fmt.Errorf("disclosure update: %w", err)
	}

	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: adminID,
		Action:      "vdp.disclosure.update",
		TargetType:  "vulnerability_disclosure",
		TargetID:    &u.ID,
		BeforeState: disclosureAuditState(before),
		AfterState:  disclosureAuditState(after),
		IPAddress:   clientIP,
	}); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return after, nil
}

// Get returns one disclosure for an authorized admin reader.
func (s *Service) Get(ctx context.Context, adminID, id int64) (*Disclosure, error) {
	if _, err := s.requireRead(ctx, adminID); err != nil {
		return nil, err
	}
	d, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	d.markBreach(time.Now().UTC())
	return d, nil
}

// List runs the admin register query with the filter set (read roles:
// Super Admin, Compliance Officer, Read-Only Auditor).
func (s *Service) List(ctx context.Context, adminID int64, f AdminFilter,
	after *struct {
		Time time.Time
		ID   int64
	}, limit int) ([]Disclosure, error) {
	if _, err := s.requireRead(ctx, adminID); err != nil {
		return nil, err
	}
	q := `SELECT ` + disclosureCols + ` FROM vulnerability_disclosures WHERE 1=1`
	args := []any{}
	next := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	if f.Status != "" {
		if !ValidStatus(f.Status) {
			return nil, excerrors.New("INVALID_REQUEST", "invalid status filter")
		}
		q += ` AND status = ` + next(f.Status)
	}
	if f.Severity != "" {
		if !ValidSeverity(f.Severity) {
			return nil, excerrors.New("INVALID_REQUEST", "invalid severity filter")
		}
		q += ` AND severity = ` + next(f.Severity)
	}
	if f.Source != "" {
		if !ValidSource(f.Source) {
			return nil, excerrors.New("INVALID_REQUEST", "invalid source filter")
		}
		q += ` AND source = ` + next(f.Source)
	}
	if f.BulletinRef != "" {
		q += ` AND bulletin_ref = ` + next(f.BulletinRef)
	}
	if f.AssigneeID != nil {
		q += ` AND assignee_admin_id = ` + next(*f.AssigneeID)
	}
	if f.Unassigned {
		q += ` AND assignee_admin_id IS NULL`
	}
	if f.BreachedOnly {
		q += ` AND status NOT IN ('FIXED','REJECTED')
		       AND ((acknowledged_at IS NULL AND ack_due_at < ` + next(time.Now().UTC()) + `)
		         OR (triaged_at IS NULL AND triage_due_at < ` + next(time.Now().UTC()) + `)
		         OR (fix_due_at IS NOT NULL AND fixed_at IS NULL AND fix_due_at < ` + next(time.Now().UTC()) + `))`
	}
	if after != nil {
		q += ` AND (created_at, id) < (` + next(after.Time) + ", " + next(after.ID) + ")"
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ` + fmt.Sprintf("%d", limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("disclosure query: %w", err)
	}
	defer rows.Close()
	out := []Disclosure{}
	now := time.Now().UTC()
	for rows.Next() {
		var d Disclosure
		if err := rows.Scan(&d.ID, &d.ReportID, &d.Source, &d.Title,
			&d.AffectedComponents, &d.Reproduction, &d.ReporterHandle,
			&d.ContactEmail, &d.SuggestedSeverity, &d.Severity, &d.CVSSScore,
			&d.CVSSVector, &d.Status, &d.AssigneeAdminID, &d.BulletinRef,
			&d.PatchRef, &d.SBOMRef, &d.FixDueAt, &d.ExpeditedPath,
			&d.AttributionRequested, &d.ResolutionSummary, &d.DisputeReason,
			&d.SubmittedAt, &d.AckDueAt, &d.AcknowledgedAt, &d.TriageDueAt,
			&d.TriagedAt, &d.FixedAt, &d.DisputedAt, &d.RejectedAt,
			&d.ResearcherAcknowledgedAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("disclosure scan: %w", err)
		}
		d.markBreach(now)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// SLA breach sweep — Task 13.5.3.8 item 4 / spec §19.11.2 item 3.
//
// Three clocks are swept: acknowledgment (ack_due_at until
// acknowledged_at), triage (triage_due_at until triaged_at) and fix
// (fix_due_at until fixed_at). Each breach raises VDP_SLA_BREACH (P2,
// Security/DevOps). The *_breach_flagged_at claims make each alert
// once-only per report per clock — UPDATE ... WHERE flag IS NULL is the
// atomic claim so concurrent sweepers cannot double-page.
// ---------------------------------------------------------------------------

type breachClock struct {
	flagCol     string
	deadlineCol string
	doneCol     string // milestone that ends the clock ("" → none beyond status)
}

var (
	ackClock    = breachClock{"ack_breach_flagged_at", "ack_due_at", "acknowledged_at"}
	triageClock = breachClock{"triage_breach_flagged_at", "triage_due_at", "triaged_at"}
	fixClock    = breachClock{"fix_breach_flagged_at", "fix_due_at", "fixed_at"}
)

// claimBreaches atomically flags and returns the disclosures newly
// breached on the given clock.
func (s *Service) claimBreaches(ctx context.Context, c breachClock,
	now time.Time, limit int) ([]Disclosure, error) {
	extra := ""
	if c.doneCol != "" {
		extra = "AND " + c.doneCol + " IS NULL"
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE vulnerability_disclosures
		   SET `+c.flagCol+` = $1, updated_at = now()
		 WHERE id IN (
		     SELECT id FROM vulnerability_disclosures
		      WHERE `+c.flagCol+` IS NULL
		        AND status NOT IN ('FIXED','REJECTED')
		        AND `+c.deadlineCol+` IS NOT NULL
		        AND `+c.deadlineCol+` < $1
		        `+extra+`
		      ORDER BY `+c.deadlineCol+`
		      LIMIT $2
		      FOR UPDATE SKIP LOCKED)
		RETURNING `+disclosureCols, now, limit)
	if err != nil {
		return nil, fmt.Errorf("breach claim: %w", err)
	}
	defer rows.Close()
	out := []Disclosure{}
	for rows.Next() {
		var d Disclosure
		if err := rows.Scan(&d.ID, &d.ReportID, &d.Source, &d.Title,
			&d.AffectedComponents, &d.Reproduction, &d.ReporterHandle,
			&d.ContactEmail, &d.SuggestedSeverity, &d.Severity, &d.CVSSScore,
			&d.CVSSVector, &d.Status, &d.AssigneeAdminID, &d.BulletinRef,
			&d.PatchRef, &d.SBOMRef, &d.FixDueAt, &d.ExpeditedPath,
			&d.AttributionRequested, &d.ResolutionSummary, &d.DisputeReason,
			&d.SubmittedAt, &d.AckDueAt, &d.AcknowledgedAt, &d.TriageDueAt,
			&d.TriagedAt, &d.FixedAt, &d.DisputedAt, &d.RejectedAt,
			&d.ResearcherAcknowledgedAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("breach scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SweepAlerts claims newly breached disclosures on all three SLA clocks
// and raises VDP_SLA_BREACH (P2) per report per clock through the
// Alerter. Returns alerts raised. Call on a timer (the gateway runs it
// every 60s alongside the support-ticket sweep).
func (s *Service) SweepAlerts(ctx context.Context, now time.Time) (int, error) {
	raised := 0
	for _, clock := range []breachClock{ackClock, triageClock, fixClock} {
		rows, err := s.claimBreaches(ctx, clock, now, 500)
		if err != nil {
			return raised, err
		}
		for _, d := range rows {
			if s.alerter == nil {
				raised++
				continue
			}
			var deadline string
			switch clock.flagCol {
			case "ack_breach_flagged_at":
				deadline = d.AckDueAt.Format(time.RFC3339)
			case "triage_breach_flagged_at":
				deadline = d.TriageDueAt.Format(time.RFC3339)
			default:
				if d.FixDueAt != nil {
					deadline = d.FixDueAt.Format(time.RFC3339)
				}
			}
			if err := s.alerter.Raise(ctx, "P2", "VDP_SLA_BREACH",
				fmt.Sprintf("disclosure %d (%s, source=%s, status=%s) %s SLA breached at %s",
					d.ID, d.ReportID, d.Source, d.Status,
					strings.TrimSuffix(clock.deadlineCol, "_at"), deadline)); err != nil {
				return raised, fmt.Errorf("alert dispatch: %w", err)
			}
			raised++
		}
	}
	return raised, nil
}

// disclosureAuditState is the compact before/after image written to
// admin_audit_log — status/severity/assignment/bulletin deltas, never
// the reproduction body.
func disclosureAuditState(d *Disclosure) map[string]any {
	return map[string]any{
		"report_id":         d.ReportID,
		"status":            d.Status,
		"source":            d.Source,
		"severity":          d.Severity,
		"cvss_score":        d.CVSSScore,
		"assignee_admin_id": d.AssigneeAdminID,
		"bulletin_ref":      d.BulletinRef,
		"patch_ref":         d.PatchRef,
		"sbom_ref":          d.SBOMRef,
		"fix_due_at":        d.FixDueAt,
		"expedited_path":    d.ExpeditedPath,
	}
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func joinCSV(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
