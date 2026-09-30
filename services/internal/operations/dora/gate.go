// The material-incident closure gate — Phase-09 Task 9.3.15 item 5 +
// acceptance row: "Material incident cannot close with overdue
// reporting or unresolved unaccepted remediation" (spec §19.5:
// "material incidents cannot be closed until required regulatory
// reports and lessons-learned actions are complete").
//
// EvaluateClosure is pure: callers load the incident, its regulator
// reports and remediation items, and pass the wall clock. Service.Close
// runs the same evaluation inside the row lock, then refuses with
// INCIDENT_CLOSURE_BLOCKED (409) listing every block.
package dora

import (
	"fmt"
	"strings"
	"time"
)

// Block kinds carried in Decision.Blocks and surfaced in the
// INCIDENT_CLOSURE_BLOCKED message.
const (
	// BlockReportOverdue — a required regulator report is past its
	// deadline and still unsubmitted.
	BlockReportOverdue = "REPORT_OVERDUE"
	// BlockReportPending — a required regulator report is unsubmitted
	// but not yet past deadline; spec §19.5 requires completion of
	// required reports, not merely non-overdueness, before CLOSED.
	BlockReportPending = "REPORT_PENDING"
	// BlockRemediationOpen — a corrective-action item is OPEN or
	// IN_PROGRESS (unresolved).
	BlockRemediationOpen = "REMEDIATION_OPEN"
	// BlockRemediationUnaccepted — work is RESOLVED but lacks
	// board/risk acceptance (Task 9.3.15 item 5).
	BlockRemediationUnaccepted = "REMEDIATION_UNACCEPTED"
	// BlockRCAIncomplete — P0/P1 post-mortem artifacts are missing:
	// rca_status must be COMPLETE with root_cause and lessons_learned
	// recorded (spec §19.8 48h post-mortem workflow).
	BlockRCAIncomplete = "RCA_INCOMPLETE"
)

// Block is one closure-gate refusal reason.
type Block struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Decision is the outcome of a closure-gate evaluation.
type Decision struct {
	Allowed bool    `json:"allowed"`
	Blocks  []Block `json:"blocks,omitempty"`
}

// Reasons renders the block details for the error message.
func (d Decision) Reasons() string {
	parts := make([]string, 0, len(d.Blocks))
	for _, b := range d.Blocks {
		parts = append(parts, b.Kind+"("+b.Detail+")")
	}
	return strings.Join(parts, "; ")
}

// EvaluateClosure decides whether inc may transition to CLOSED at now.
//
// Report gate — reportable incidents only (P0, or material P1): every
// required report (INITIAL/INTERMEDIATE/FINAL) must be submitted; an
// unsubmitted report blocks as REPORT_OVERDUE past its deadline and
// REPORT_PENDING before it.
//
// RCA gate — P0/P1 incidents (the §19.8 post-mortem scope): rca_status
// must be COMPLETE with non-empty root_cause and lessons_learned.
//
// Remediation gate — every incident: no item may be OPEN/IN_PROGRESS
// (unresolved) or RESOLVED-without-acceptance (unaccepted); ACCEPTED is
// the only passing state.
func EvaluateClosure(inc *Incident, reports []Report, rems []Remediation, now time.Time) Decision {
	var d Decision

	for _, kind := range inc.RequiredReports() {
		due, err := inc.ReportDueAt(kind)
		if err != nil {
			// Unreachable: RequiredReports only emits known kinds.
			d.Blocks = append(d.Blocks, Block{BlockReportOverdue, kind + ": " + err.Error()})
			continue
		}
		r := findReport(reports, kind)
		if r == nil || r.SubmittedAt == nil {
			b := Block{Kind: BlockReportPending,
				Detail: fmt.Sprintf("%s report unsubmitted (due %s)", kind,
					due.UTC().Format("2006-01-02T15:04Z"))}
			if now.After(due) {
				b.Kind = BlockReportOverdue
				b.Detail = fmt.Sprintf("%s report overdue (due %s, unsubmitted)", kind,
					due.UTC().Format("2006-01-02T15:04Z"))
			}
			d.Blocks = append(d.Blocks, b)
		}
	}

	if inc.Severity == SeverityP0 || inc.Severity == SeverityP1 {
		var missing []string
		if inc.RCAStatus != RCAComplete {
			missing = append(missing, "rca_status="+inc.RCAStatus)
		}
		if strings.TrimSpace(inc.RootCause) == "" {
			missing = append(missing, "root_cause")
		}
		if strings.TrimSpace(inc.Lessons) == "" {
			missing = append(missing, "lessons_learned")
		}
		if len(missing) > 0 {
			d.Blocks = append(d.Blocks, Block{BlockRCAIncomplete,
				"post-mortem incomplete: " + strings.Join(missing, ", ")})
		}
	}

	for _, r := range rems {
		switch r.Status {
		case RemAccepted:
		case RemResolved:
			d.Blocks = append(d.Blocks, Block{BlockRemediationUnaccepted,
				fmt.Sprintf("remediation #%d %q resolved but not accepted by board/risk",
					r.ID, r.Action)})
		default: // OPEN, IN_PROGRESS, anything unexpected — fail closed
			d.Blocks = append(d.Blocks, Block{BlockRemediationOpen,
				fmt.Sprintf("remediation #%d %q still %s", r.ID, r.Action, r.Status)})
		}
	}

	d.Allowed = len(d.Blocks) == 0
	return d
}
