// Package dora implements the Phase-09 Task 9.3.15 DORA incident
// lifecycle closure gate (spec §19.5, §19.8, §24 #171): a material
// incident cannot transition to CLOSED while a required regulator
// report is unsubmitted (overdue or still pending) or a remediation
// item is unresolved / unaccepted.
//
// The persistence shape is migration 273 (`incidents`,
// `incident_regulator_reports`, `incident_remediations`); `incidents`
// is also the Task 9.3.18 store the fleet deploy gates and admin packs
// already probe.
//
// DORA regulator-report cadence (spec §19.8 — canonical):
//
//	INITIAL       detected_at + 4h
//	INTERMEDIATE  detected_at + 72h
//	FINAL         detected_at + 1 month
//
// An incident is DORA-reportable when severity is P0, or P1 flagged
// material (spec §19.8: "P0 and material P1").
package dora

import (
	"fmt"
	"time"
)

// Incident lifecycle states. CLOSED is terminal; the fleet deploy gate
// and packs treat RESOLVED/CLOSED as non-open.
const (
	StatusOpen         = "OPEN"
	StatusAcknowledged = "ACKNOWLEDGED"
	StatusMitigated    = "MITIGATED"
	StatusResolved     = "RESOLVED"
	StatusClosed       = "CLOSED"
)

// Severity tiers per spec §19.8 / Task 9.3.18.
const (
	SeverityP0 = "P0"
	SeverityP1 = "P1"
	SeverityP2 = "P2"
	SeverityP3 = "P3"
)

// Regulator report kinds and their deadlines from detection
// (spec §19.8: 4h initial / 72h intermediate / 1-month final).
const (
	ReportInitial      = "INITIAL"
	ReportIntermediate = "INTERMEDIATE"
	ReportFinal        = "FINAL"
)

var reportDeadlines = map[string]time.Duration{
	ReportInitial:      4 * time.Hour,
	ReportIntermediate: 72 * time.Hour,
	// FINAL is one calendar month — applied via AddDate, not Duration.
}

// Remediation item states. ACCEPTED is the only terminal state that
// satisfies the closure gate: it means the corrective action is done
// AND board/risk has accepted it (Task 9.3.15 item 5).
const (
	RemOpen       = "OPEN"
	RemInProgress = "IN_PROGRESS"
	RemResolved   = "RESOLVED" // work done, awaiting board/risk acceptance
	RemAccepted   = "ACCEPTED"
)

// RCA status vocabulary on incidents (spec §19.8 post-mortem workflow;
// the 48h P0/P1 completion SLA is tracked by Task 9.3.18 tooling).
const (
	RCAOpen     = "OPEN"
	RCADraft    = "DRAFT"
	RCAComplete = "COMPLETE"
)

// Incident is the internal incident record (migration 273 `incidents`).
type Incident struct {
	ID             int64          `json:"id"`
	Ref            string         `json:"ref"`
	Severity       string         `json:"severity"`
	Material       bool           `json:"material"`
	Status         string         `json:"status"`
	Title          string         `json:"title"`
	Classification Classification `json:"classification"`
	RCAStatus      string         `json:"rca_status"`
	RootCause      string         `json:"root_cause"`
	Lessons        string         `json:"lessons_learned"`
	DetectedAt     time.Time      `json:"detected_at"`
	AckedAt        *time.Time     `json:"acknowledged_at,omitempty"`
	MitigatedAt    *time.Time     `json:"mitigated_at,omitempty"`
	ResolvedAt     *time.Time     `json:"resolved_at,omitempty"`
	ClosedAt       *time.Time     `json:"closed_at,omitempty"`
	ClosedBy       *int64         `json:"closed_by,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

// Classification carries the Task 9.3.15 item-2 axes: impact, affected
// clients/counterparties, duration, geography, data loss, critical
// services. It is persisted as JSONB.
type Classification struct {
	Impact            string   `json:"impact,omitempty"`            // e.g. "trading_halted"
	AffectedClients   int64    `json:"affected_clients,omitempty"`  // client count estimate
	Counterparties    []string `json:"counterparties,omitempty"`    // LPs / PBs / rails affected
	Geography         []string `json:"geography,omitempty"`         // regions impacted
	DataLoss          bool     `json:"data_loss,omitempty"`         // any confirmed data loss
	CriticalServices  []string `json:"critical_services,omitempty"` // critical functions hit
	DurationMinutes   int      `json:"duration_minutes,omitempty"`  // service-impact duration
	ClientsMaterially bool     `json:"clients_materially_affected"` // materiality input (P1)
}

// Report is one regulator-report obligation row
// (migration 273 `incident_regulator_reports`).
type Report struct {
	ID          int64      `json:"id"`
	IncidentID  int64      `json:"incident_id"`
	Kind        string     `json:"kind"` // INITIAL | INTERMEDIATE | FINAL
	Regulator   string     `json:"regulator"`
	DueAt       time.Time  `json:"due_at"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	SubmittedBy *int64     `json:"submitted_by,omitempty"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	ApprovedBy  *int64     `json:"approved_by,omitempty"`
	EvidenceRef string     `json:"evidence_ref"`
}

// Remediation is one lessons-learned / corrective-action item
// (migration 273 `incident_remediations`).
type Remediation struct {
	ID         int64      `json:"id"`
	IncidentID int64      `json:"incident_id"`
	Action     string     `json:"action"`
	Owner      string     `json:"owner"`
	DueAt      time.Time  `json:"due_at"`
	Status     string     `json:"status"` // OPEN | IN_PROGRESS | RESOLVED | ACCEPTED
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	AcceptedBy *int64     `json:"accepted_by,omitempty"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
}

// Reportable reports whether the incident is DORA-reportable — P0
// always; P1 when flagged material by classification (spec §19.8).
func (i *Incident) Reportable() bool {
	return i.Severity == SeverityP0 || (i.Severity == SeverityP1 && i.Material)
}

// RequiredReports returns the regulator-report kinds a reportable
// incident must complete before closure (all three, per §19.5/§19.8).
func (i *Incident) RequiredReports() []string {
	if !i.Reportable() {
		return nil
	}
	return []string{ReportInitial, ReportIntermediate, ReportFinal}
}

// ReportDueAt computes the regulatory deadline for kind anchored on the
// incident's detection time. Unknown kinds return an error — deadlines
// are never guessed (fail-closed).
func (i *Incident) ReportDueAt(kind string) (time.Time, error) {
	switch kind {
	case ReportFinal:
		return i.DetectedAt.AddDate(0, 1, 0), nil // "1-month final" (§19.8)
	case ReportInitial, ReportIntermediate:
		return i.DetectedAt.Add(reportDeadlines[kind]), nil
	default:
		return time.Time{}, fmt.Errorf("dora: unknown report kind %q", kind)
	}
}

// IsTerminal reports whether the incident is in a terminal state.
func (i *Incident) IsTerminal() bool { return i.Status == StatusClosed }

// openReport maps a report kind to its row (nil when absent).
func findReport(reports []Report, kind string) *Report {
	for j := range reports {
		if reports[j].Kind == kind {
			return &reports[j]
		}
	}
	return nil
}
