// Service-side incident lifecycle operations. Close is the gated path:
// it locks the incident row inside the store transaction, evaluates
// EvaluateClosure, and refuses with INCIDENT_CLOSURE_BLOCKED (409) when
// any block applies — an overdue or pending regulator report, an RCA
// artifact gap on P0/P1, or an unresolved/unaccepted remediation item.
package dora

import (
	"context"
	"fmt"
	"strings"
	"time"

	excerrors "exchange/pkg/errors"
)

// Store is the persistence seam (PgxStore implements it against
// migration 273; tests substitute an in-memory store).
type Store interface {
	// InTx runs fn inside a transaction; the Tx methods lock the
	// incident row so a concurrent report submission or remediation
	// update cannot slip past the gate evaluation.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	InsertIncident(ctx context.Context, i *Incident) (int64, error)
	GetIncident(ctx context.Context, id int64) (*Incident, bool, error)
	ListReports(ctx context.Context, incidentID int64) ([]Report, error)
	ListRemediations(ctx context.Context, incidentID int64) ([]Remediation, error)

	InsertReport(ctx context.Context, r *Report) (int64, error)
	MarkReportSubmitted(ctx context.Context, incidentID int64, kind string,
		by int64, at time.Time, evidenceRef string) (bool, error)

	InsertRemediation(ctx context.Context, m *Remediation) (int64, error)
	UpdateRemediationStatus(ctx context.Context, id int64, status string,
		at time.Time, actor int64) (bool, error)
	UpdateIncidentRCA(ctx context.Context, id int64, rcaStatus, rootCause,
		lessons string) (bool, error)
}

// Tx is the transactional view used by the closure path.
type Tx interface {
	LockIncident(ctx context.Context, id int64) (*Incident, bool, error)
	ListReports(ctx context.Context, incidentID int64) ([]Report, error)
	ListRemediations(ctx context.Context, incidentID int64) ([]Remediation, error)
	SetClosed(ctx context.Context, id, actor int64, at time.Time) error
}

// Service owns the DORA incident lifecycle workflow.
type Service struct {
	store Store
	now   func() time.Time
}

// New wires the service over the store seam.
func New(store Store) (*Service, error) {
	if store == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "dora: store not wired")
	}
	return &Service{store: store, now: func() time.Time { return time.Now().UTC() }}, nil
}

// SetClock overrides the clock (tests / deterministic replay).
func (s *Service) SetClock(f func() time.Time) { s.now = f }

func validSeverity(v string) bool {
	switch v {
	case SeverityP0, SeverityP1, SeverityP2, SeverityP3:
		return true
	}
	return false
}

// Open creates an incident. P0 is always material (DORA-reportable);
// P1 materiality comes from the classification
// (ClientsMateriallyAffected / DataLoss / CriticalServices) or an
// explicit material flag — fail-closed: a P1 with any materiality
// signal is reportable.
func (s *Service) Open(ctx context.Context, inc *Incident) (*Incident, error) {
	if inc == nil {
		return nil, excerrors.New("INVALID_REQUEST", "incident payload required")
	}
	if !validSeverity(inc.Severity) {
		return nil, excerrors.New("INVALID_REQUEST",
			"severity must be P0|P1|P2|P3")
	}
	if strings.TrimSpace(inc.Title) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "title required")
	}
	if inc.DetectedAt.IsZero() {
		inc.DetectedAt = s.now()
	}
	if inc.Severity == SeverityP0 ||
		(inc.Severity == SeverityP1 && (inc.Material || inc.Classification.DataLoss ||
			inc.Classification.ClientsMaterially ||
			len(inc.Classification.CriticalServices) > 0)) {
		inc.Material = true
	}
	inc.Status = StatusOpen
	inc.RCAStatus = RCAOpen
	if inc.Ref == "" {
		inc.Ref = fmt.Sprintf("INC-%s", inc.DetectedAt.UTC().Format("20060102"))
	}
	id, err := s.store.InsertIncident(ctx, inc)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert incident", err)
	}
	inc.ID = id
	return inc, nil
}

// RaiseReport registers a regulator-report obligation with its
// statutory deadline anchored on detection (spec §19.8).
func (s *Service) RaiseReport(ctx context.Context, incidentID int64,
	kind, regulator string) (*Report, error) {
	inc, found, err := s.store.GetIncident(ctx, incidentID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "incident lookup", err)
	}
	if !found {
		return nil, excerrors.New("NOT_FOUND", "incident not found")
	}
	due, err := inc.ReportDueAt(kind)
	if err != nil {
		return nil, excerrors.New("INVALID_REQUEST", err.Error())
	}
	r := &Report{IncidentID: incidentID, Kind: kind,
		Regulator: regulator, DueAt: due}
	r.ID, err = s.store.InsertReport(ctx, r)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert report", err)
	}
	return r, nil
}

// SubmitReport marks a report submitted (idempotent: a resubmission for
// an already-submitted kind is a no-op).
func (s *Service) SubmitReport(ctx context.Context, incidentID int64,
	kind string, by int64, evidenceRef string) error {
	ok, err := s.store.MarkReportSubmitted(ctx, incidentID, kind, by, s.now(), evidenceRef)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "submit report", err)
	}
	if !ok {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("no %s report obligation for incident %d", kind, incidentID))
	}
	return nil
}

// AddRemediation registers a corrective-action item; owner and due date
// are mandatory at registration (Task 9.3.15 item 5) — an unowned or
// undated remediation is refused at intake rather than at the gate.
func (s *Service) AddRemediation(ctx context.Context, m *Remediation) (*Remediation, error) {
	if m == nil || strings.TrimSpace(m.Action) == "" {
		return nil, excerrors.New("INVALID_REQUEST", "remediation action required")
	}
	if strings.TrimSpace(m.Owner) == "" || m.DueAt.IsZero() {
		return nil, excerrors.New("INVALID_REQUEST",
			"remediation requires owner and due_at (Task 9.3.15 item 5)")
	}
	m.Status = RemOpen
	id, err := s.store.InsertRemediation(ctx, m)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert remediation", err)
	}
	m.ID = id
	return m, nil
}

// SetRemediationStatus moves a remediation through
// OPEN→IN_PROGRESS→RESOLVED→ACCEPTED. ACCEPTED records the accepting
// principal — the board/risk acceptance the closure gate checks.
func (s *Service) SetRemediationStatus(ctx context.Context, id int64,
	status string, actor int64) error {
	switch status {
	case RemInProgress, RemResolved:
	case RemAccepted:
		if actor == 0 {
			return excerrors.New("INVALID_REQUEST",
				"acceptance requires the accepting principal id")
		}
	default:
		return excerrors.New("INVALID_REQUEST",
			"remediation status must be IN_PROGRESS|RESOLVED|ACCEPTED")
	}
	ok, err := s.store.UpdateRemediationStatus(ctx, id, status, s.now(), actor)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "update remediation", err)
	}
	if !ok {
		return excerrors.New("NOT_FOUND", "remediation not found")
	}
	return nil
}

// RecordRCA persists the post-mortem artifacts the P0/P1 closure gate
// requires: rca_status COMPLETE + root cause + lessons learned.
func (s *Service) RecordRCA(ctx context.Context, id int64, rcaStatus,
	rootCause, lessons string) error {
	switch rcaStatus {
	case RCAOpen, RCADraft, RCAComplete:
	default:
		return excerrors.New("INVALID_REQUEST", "rca_status must be OPEN|DRAFT|COMPLETE")
	}
	ok, err := s.store.UpdateIncidentRCA(ctx, id, rcaStatus, rootCause, lessons)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "record RCA", err)
	}
	if !ok {
		return excerrors.New("NOT_FOUND", "incident not found")
	}
	return nil
}

// Evaluate loads current state and returns the closure decision without
// mutating — the admin "why can't this close" surface.
func (s *Service) Evaluate(ctx context.Context, incidentID int64) (*Decision, error) {
	inc, found, err := s.store.GetIncident(ctx, incidentID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "incident lookup", err)
	}
	if !found {
		return nil, excerrors.New("NOT_FOUND", "incident not found")
	}
	reports, err := s.store.ListReports(ctx, incidentID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list reports", err)
	}
	rems, err := s.store.ListRemediations(ctx, incidentID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list remediations", err)
	}
	d := EvaluateClosure(inc, reports, rems, s.now())
	return &d, nil
}

// Close transitions the incident to CLOSED under the closure gate.
// The evaluation runs against the row locked inside the transaction so
// a concurrent report submission / remediation update cannot slip past.
// Closing an already-CLOSED incident is an idempotent no-op (replayed
// terminal transitions stay idempotent, matching the settlement/CLS
// convention); every other non-RESOLVED source state is refused.
func (s *Service) Close(ctx context.Context, incidentID, actor int64) (*Incident, error) {
	var out *Incident
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		inc, found, err := tx.LockIncident(ctx, incidentID)
		if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "incident lock", err)
		}
		if !found {
			return excerrors.New("NOT_FOUND", "incident not found")
		}
		if inc.Status == StatusClosed {
			out = inc // idempotent replay
			return nil
		}
		reports, err := tx.ListReports(ctx, incidentID)
		if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "list reports", err)
		}
		rems, err := tx.ListRemediations(ctx, incidentID)
		if err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "list remediations", err)
		}
		if d := EvaluateClosure(inc, reports, rems, s.now()); !d.Allowed {
			return excerrors.New("INCIDENT_CLOSURE_BLOCKED",
				"incident "+inc.Ref+" cannot close: "+d.Reasons())
		}
		at := s.now()
		if err := tx.SetClosed(ctx, incidentID, actor, at); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "close incident", err)
		}
		inc.Status = StatusClosed
		inc.ClosedAt = &at
		inc.ClosedBy = &actor
		out = inc
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
