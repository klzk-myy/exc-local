// Closure-gate tests — Phase-09 Task 9.3.15 acceptance row:
// "Material incident cannot close with overdue reporting or unresolved
// unaccepted remediation". The three canonical branches:
//  1. overdue regulatory reporting        → INCIDENT_CLOSURE_BLOCKED
//  2. unresolved / unaccepted remediation → INCIDENT_CLOSURE_BLOCKED
//  3. complete reports + accepted actions → CLOSED
package dora

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

// --- in-memory store (test seam) ---------------------------------------------

type memStore struct {
	mu     sync.Mutex
	nextID int64
	incs   map[int64]*Incident
	reps   map[int64][]Report
	rems   map[int64]*Remediation // keyed by remediation id
}

func newMemStore() *memStore {
	return &memStore{incs: map[int64]*Incident{}, reps: map[int64][]Report{},
		rems: map[int64]*Remediation{}}
}

func (m *memStore) remsFor(incidentID int64) []Remediation {
	var out []Remediation
	for _, r := range m.rems {
		if r.IncidentID == incidentID {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *memStore) InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(ctx, memTx{m})
}

type memTx struct{ m *memStore }

func (t memTx) LockIncident(ctx context.Context, id int64) (*Incident, bool, error) {
	i, ok := t.m.incs[id]
	return i, ok, nil
}
func (t memTx) ListReports(ctx context.Context, id int64) ([]Report, error) {
	return append([]Report(nil), t.m.reps[id]...), nil
}
func (t memTx) ListRemediations(ctx context.Context, id int64) ([]Remediation, error) {
	return t.m.remsFor(id), nil
}
func (t memTx) SetClosed(ctx context.Context, id, actor int64, at time.Time) error {
	i := t.m.incs[id]
	if i.Status == StatusClosed {
		return fmt.Errorf("not transitioned")
	}
	i.Status, i.ClosedAt, i.ClosedBy = StatusClosed, &at, &actor
	return nil
}

func (m *memStore) InsertIncident(ctx context.Context, i *Incident) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	i.ID = m.nextID
	i.CreatedAt = i.DetectedAt
	m.incs[i.ID] = i
	return i.ID, nil
}
func (m *memStore) GetIncident(ctx context.Context, id int64) (*Incident, bool, error) {
	i, ok := m.incs[id]
	return i, ok, nil
}
func (m *memStore) ListReports(ctx context.Context, id int64) ([]Report, error) {
	return append([]Report(nil), m.reps[id]...), nil
}
func (m *memStore) ListRemediations(ctx context.Context, id int64) ([]Remediation, error) {
	return m.remsFor(id), nil
}
func (m *memStore) InsertReport(ctx context.Context, r *Report) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	r.ID = m.nextID
	m.reps[r.IncidentID] = append(m.reps[r.IncidentID], *r)
	return r.ID, nil
}
func (m *memStore) MarkReportSubmitted(ctx context.Context, incidentID int64,
	kind string, by int64, at time.Time, evidenceRef string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.reps[incidentID] {
		if m.reps[incidentID][i].Kind == kind {
			if m.reps[incidentID][i].SubmittedAt == nil {
				m.reps[incidentID][i].SubmittedAt = &at
				m.reps[incidentID][i].SubmittedBy = &by
			}
			return true, nil
		}
	}
	return false, nil
}
func (m *memStore) InsertRemediation(ctx context.Context, r *Remediation) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	r.ID = m.nextID
	cp := *r
	m.rems[r.ID] = &cp
	return r.ID, nil
}
func (m *memStore) UpdateRemediationStatus(ctx context.Context, id int64,
	status string, at time.Time, actor int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rems[id]
	if !ok {
		return false, nil
	}
	r.Status = status
	if status == RemResolved || status == RemAccepted {
		r.ResolvedAt = &at
	}
	if status == RemAccepted {
		r.AcceptedBy, r.AcceptedAt = &actor, &at
	}
	return true, nil
}
func (m *memStore) UpdateIncidentRCA(ctx context.Context, id int64,
	rcaStatus, rootCause, lessons string) (bool, error) {
	i, ok := m.incs[id]
	if !ok {
		return false, nil
	}
	i.RCAStatus, i.RootCause, i.Lessons = rcaStatus, rootCause, lessons
	return true, nil
}

// --- fixtures -----------------------------------------------------------------

var t0 = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// now must sit past the FINAL deadline (+1 month) for a "clean" incident.
var tLate = t0.AddDate(0, 1, 1)

func newSvc(t *testing.T, at time.Time) (*Service, *memStore) {
	t.Helper()
	s, err := New(newMemStore())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s.SetClock(func() time.Time { return at })
	return s, s.store.(*memStore)
}

func openP0(t *testing.T, s *Service) *Incident {
	t.Helper()
	inc, err := s.Open(context.Background(), &Incident{
		Severity: SeverityP0, Title: "shard halt", DetectedAt: t0})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return inc
}

// fullyReported raises + submits all three DORA reports.
func fullyReported(t *testing.T, s *Service, inc *Incident) {
	t.Helper()
	for _, k := range []string{ReportInitial, ReportIntermediate, ReportFinal} {
		if _, err := s.RaiseReport(context.Background(), inc.ID, k, "NCA"); err != nil {
			t.Fatalf("raise %s: %v", k, err)
		}
		if err := s.SubmitReport(context.Background(), inc.ID, k, 7, "ev://x"); err != nil {
			t.Fatalf("submit %s: %v", k, err)
		}
	}
}

func completeRCA(t *testing.T, s *Service, inc *Incident) {
	t.Helper()
	if err := s.RecordRCA(context.Background(), inc.ID, RCAComplete,
		"Aeron ring overrun", "add watermark probe"); err != nil {
		t.Fatalf("rca: %v", err)
	}
}

// --- the three gate branches -----------------------------------------------------

// Branch 1: material incident, FINAL report overdue and unsubmitted →
// INCIDENT_CLOSURE_BLOCKED.
func TestClose_BlocksOnOverdueReport(t *testing.T) {
	s, _ := newSvc(t, tLate)
	inc := openP0(t, s)
	// Raise + submit INITIAL and INTERMEDIATE only; FINAL stays open and
	// its deadline (detected+1mo) is in the past at tLate.
	for _, k := range []string{ReportInitial, ReportIntermediate} {
		if _, err := s.RaiseReport(context.Background(), inc.ID, k, "NCA"); err != nil {
			t.Fatalf("raise %s: %v", k, err)
		}
		if err := s.SubmitReport(context.Background(), inc.ID, k, 7, ""); err != nil {
			t.Fatalf("submit %s: %v", k, err)
		}
	}
	if _, err := s.RaiseReport(context.Background(), inc.ID, ReportFinal, "NCA"); err != nil {
		t.Fatalf("raise final: %v", err)
	}
	completeRCA(t, s, inc)

	_, err := s.Close(context.Background(), inc.ID, 99)
	if err == nil {
		t.Fatal("expected closure refusal")
	}
	if code := excerrors.CodeOf(err); code != "INCIDENT_CLOSURE_BLOCKED" {
		t.Fatalf("code = %s, want INCIDENT_CLOSURE_BLOCKED (%v)", code, err)
	}
	if want := "REPORT_OVERDUE"; !contains(err.Error(), want) {
		t.Fatalf("error %q missing %s", err.Error(), want)
	}
	// Status untouched.
	got, _, _ := s.store.GetIncident(context.Background(), inc.ID)
	if got.Status == StatusClosed {
		t.Fatal("incident closed despite overdue report")
	}
}

// Branch 2: unresolved OR unaccepted remediation blocks closure even
// when reporting is complete.
func TestClose_BlocksOnRemediation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endStatus string
		want      string
	}{
		{"open", RemOpen, BlockRemediationOpen},
		{"in_progress", RemInProgress, BlockRemediationOpen},
		{"resolved_unaccepted", RemResolved, BlockRemediationUnaccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newSvc(t, tLate)
			inc := openP0(t, s)
			fullyReported(t, s, inc)
			completeRCA(t, s, inc)
			m, err := s.AddRemediation(context.Background(), &Remediation{
				IncidentID: inc.ID, Action: "throttle Aeron ingress",
				Owner: "sre", DueAt: tLate.Add(24 * time.Hour)})
			if err != nil {
				t.Fatalf("add remediation: %v", err)
			}
			if tc.endStatus != RemOpen {
				if err := s.SetRemediationStatus(context.Background(),
					m.ID, tc.endStatus, 42); err != nil {
					t.Fatalf("set %s: %v", tc.endStatus, err)
				}
			}
			_, err = s.Close(context.Background(), inc.ID, 99)
			if err == nil {
				t.Fatal("expected closure refusal")
			}
			if code := excerrors.CodeOf(err); code != "INCIDENT_CLOSURE_BLOCKED" {
				t.Fatalf("code = %s (%v)", code, err)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %s", err.Error(), tc.want)
			}
		})
	}
}

// Branch 3: fully reported, RCA complete, remediations ACCEPTED → CLOSED.
func TestClose_AllowsCompliantIncident(t *testing.T) {
	s, _ := newSvc(t, tLate)
	inc := openP0(t, s)
	fullyReported(t, s, inc)
	completeRCA(t, s, inc)
	m, err := s.AddRemediation(context.Background(), &Remediation{
		IncidentID: inc.ID, Action: "patch ring buffer", Owner: "core",
		DueAt: tLate.Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("add rem: %v", err)
	}
	if err := s.SetRemediationStatus(context.Background(), m.ID, RemAccepted, 5); err != nil {
		t.Fatalf("accept: %v", err)
	}
	closed, err := s.Close(context.Background(), inc.ID, 99)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Status != StatusClosed || closed.ClosedAt == nil || *closed.ClosedBy != 99 {
		t.Fatalf("bad closed state: %+v", closed)
	}
	// Idempotent replay.
	again, err := s.Close(context.Background(), inc.ID, 99)
	if err != nil || again.Status != StatusClosed {
		t.Fatalf("idempotent reclose: %v", err)
	}
}

// --- edge coverage ---------------------------------------------------------------

// A required report that is unsubmitted but not yet due still blocks —
// spec §19.5 requires completion, not merely non-overdueness.
func TestClose_BlocksOnPendingReport(t *testing.T) {
	s, _ := newSvc(t, t0.Add(2*time.Hour)) // inside the INITIAL window
	inc := openP0(t, s)
	if _, err := s.RaiseReport(context.Background(), inc.ID, ReportInitial, "NCA"); err != nil {
		t.Fatalf("raise: %v", err)
	}
	completeRCA(t, s, inc)
	_, err := s.Close(context.Background(), inc.ID, 99)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if code := excerrors.CodeOf(err); code != "INCIDENT_CLOSURE_BLOCKED" {
		t.Fatalf("code = %s", code)
	}
	if !contains(err.Error(), BlockReportPending) {
		t.Fatalf("want REPORT_PENDING, got %v", err)
	}
}

// Non-reportable incident (P2) with no remediations closes freely.
func TestClose_NonMaterialCloses(t *testing.T) {
	s, _ := newSvc(t, t0.Add(time.Hour))
	inc, err := s.Open(context.Background(), &Incident{
		Severity: SeverityP2, Title: "minor UI", DetectedAt: t0})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if inc.Material {
		t.Fatal("P2 must not be DORA-reportable")
	}
	closed, err := s.Close(context.Background(), inc.ID, 1)
	if err != nil || closed.Status != StatusClosed {
		t.Fatalf("close: %v", err)
	}
}

// P1 becomes reportable when classification shows materiality.
func TestOpen_P1MaterialByClassification(t *testing.T) {
	s, _ := newSvc(t, t0)
	inc, err := s.Open(context.Background(), &Incident{
		Severity: SeverityP1, Title: "partial data loss",
		DetectedAt:     t0,
		Classification: Classification{DataLoss: true}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !inc.Material || !inc.Reportable() {
		t.Fatal("P1 with data loss must be DORA-reportable")
	}
}

// P0/P1 closure requires the RCA artifacts (§19.8 post-mortem).
func TestClose_BlocksOnIncompleteRCA(t *testing.T) {
	s, _ := newSvc(t, tLate)
	inc := openP0(t, s)
	fullyReported(t, s, inc)
	// RCA left OPEN, root_cause/lessons empty.
	_, err := s.Close(context.Background(), inc.ID, 9)
	if err == nil || excerrors.CodeOf(err) != "INCIDENT_CLOSURE_BLOCKED" {
		t.Fatalf("expected INCIDENT_CLOSURE_BLOCKED, got %v", err)
	}
	if !contains(err.Error(), BlockRCAIncomplete) {
		t.Fatalf("want RCA_INCOMPLETE, got %v", err)
	}
}

// Deadlines anchored on detection: 4h / 72h / 1-month (spec §19.8).
func TestReportDeadlines(t *testing.T) {
	inc := &Incident{Severity: SeverityP0, Material: true, DetectedAt: t0}
	got := map[string]time.Time{}
	for _, k := range inc.RequiredReports() {
		d, err := inc.ReportDueAt(k)
		if err != nil {
			t.Fatalf("due %s: %v", k, err)
		}
		got[k] = d
	}
	if got[ReportInitial] != t0.Add(4*time.Hour) {
		t.Fatalf("initial due %v", got[ReportInitial])
	}
	if got[ReportIntermediate] != t0.Add(72*time.Hour) {
		t.Fatalf("intermediate due %v", got[ReportIntermediate])
	}
	if got[ReportFinal] != t0.AddDate(0, 1, 0) {
		t.Fatalf("final due %v", got[ReportFinal])
	}
	// A P2 carries no obligations.
	if n := (&Incident{Severity: SeverityP2}).RequiredReports(); len(n) != 0 {
		t.Fatalf("P2 required reports: %v", n)
	}
}

// Missing incident → NOT_FOUND through the canonical path.
func TestClose_NotFound(t *testing.T) {
	s, _ := newSvc(t, t0)
	_, err := s.Close(context.Background(), 4242, 1)
	if code := excerrors.CodeOf(err); code != "NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
}

// Evaluate is the read-only gate surface.
func TestEvaluate_ReadOnly(t *testing.T) {
	s, _ := newSvc(t, tLate)
	inc := openP0(t, s)
	d, err := s.Evaluate(context.Background(), inc.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Allowed {
		t.Fatal("expected blocks")
	}
	kinds := map[string]int{}
	for _, b := range d.Blocks {
		kinds[b.Kind]++
	}
	if kinds[BlockReportPending] != 3 && kinds[BlockReportOverdue] != 3 {
		t.Fatalf("want 3 report blocks, got %+v", kinds)
	}
}

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }
