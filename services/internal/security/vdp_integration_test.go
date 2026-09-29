// PostgreSQL integration test — Phase-13.5 Tasks 13.5.3.8/13.5.3.9.
// Gated on EXC_PG_TEST=1; targets EXC_TEST_DSN (default: the dev
// database). Requires migration 081 applied; the test applies it
// idempotently when vulnerability_disclosures is absent.
package security

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/db"
)

func pgGate(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("set EXC_PG_TEST=1 to run Postgres integration tests")
	}
	dsn := os.Getenv("EXC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := db.NewPool(ctx, dsn, 4)
	if err != nil {
		cancel()
		t.Skipf("postgres unreachable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		cancel()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(func() { pool.Close(); cancel() })

	var hasTable bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_name = 'vulnerability_disclosures')`).Scan(&hasTable); err != nil {
		t.Fatalf("table check: %v", err)
	}
	if !hasTable {
		up, err := os.ReadFile("../db/migrations/081_vulnerability_disclosures.up.sql")
		if err != nil {
			t.Fatalf("read migration 081: %v", err)
		}
		if _, err := pool.Exec(ctx, string(up)); err != nil {
			t.Fatalf("apply migration 081: %v", err)
		}
	}
	return pool, ctx
}

type recAlerter struct{ raised [][3]string }

func (a *recAlerter) Raise(_ context.Context, sev, code, msg string) error {
	a.raised = append(a.raised, [3]string{sev, code, msg})
	return nil
}

func complianceResolver(context.Context, int64) (string, error) {
	return "Compliance Officer", nil
}

func submitReq(id string) SubmitRequest {
	return SubmitRequest{
		ReportID:             id,
		Source:               SourceResearcher,
		Title:                "auth bypass on admin endpoint",
		AffectedComponents:   []string{"REST", "ADMIN"},
		Reproduction:         "GET /api/v1/admin/x with forged scope header",
		ReporterHandle:       "whitehat-42",
		ContactEmail:         "w@example.invalid",
		SuggestedSeverity:    SeverityHigh,
		AttributionRequested: true,
	}
}

func TestVDPRegisterLifecycle(t *testing.T) {
	pool, ctx := pgGate(t)
	alerter := &recAlerter{}
	svc := NewService(pool, complianceResolver, alerter)
	adminID := int64(910001)
	rid := "IT-" + time.Now().Format("20060102150405.000000000")

	// --- submit + idempotent resubmit ---
	d, created, err := svc.Submit(ctx, submitReq(rid))
	if err != nil || !created {
		t.Fatalf("submit: created=%v err=%v", created, err)
	}
	if d.Status != StatusTriaged {
		t.Fatalf("new disclosure must land TRIAGED (spec F7): %s", d.Status)
	}
	if d.AckDueAt.Sub(d.SubmittedAt) != AckSLA {
		t.Fatalf("ack deadline = %v after submit, want 72h", d.AckDueAt.Sub(d.SubmittedAt))
	}
	if d.AcknowledgedAt != nil || d.TriagedAt != nil || d.Severity != nil {
		t.Fatal("milestones/severity must be NULL before staff touch")
	}
	dup, created2, err := svc.Submit(ctx, submitReq(rid))
	if err != nil || created2 || dup.ID != d.ID {
		t.Fatalf("idempotent resubmit: created=%v dup_id=%v err=%v", created2, dup.ID, err)
	}

	// --- triage: CVSS-derived severity + ETA ---
	score := 9.1
	tr, err := svc.Triage(ctx, adminID, TriageInput{
		ID:          d.ID,
		CVSSScore:   &score,
		CVSSVector:  "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		AssignToMe:  true,
		BulletinRef: "EXC-SA-2026-099",
		SBOMRef:     "reports/sbom/latest.json",
	}, "10.0.0.9")
	if err != nil {
		t.Fatalf("triage: %v", err)
	}
	if tr.Status != StatusInProgress {
		t.Fatalf("triage must move TRIAGED→IN_PROGRESS: %s", tr.Status)
	}
	if tr.Severity == nil || *tr.Severity != SeverityCritical {
		t.Fatalf("cvss 9.1 must derive CRITICAL: %v", tr.Severity)
	}
	if tr.CVSSScore == nil || *tr.CVSSScore != 9.1 {
		t.Fatal("cvss_score must be stored per report")
	}
	if tr.TriagedAt == nil || tr.AcknowledgedAt == nil {
		t.Fatal("triage stamps triaged_at + acknowledged_at")
	}
	if tr.FixDueAt == nil || tr.FixDueAt.Sub(*tr.TriagedAt) > 8*24*time.Hour {
		t.Fatalf("critical fix ETA must be ≤7d from triage: %v", tr.FixDueAt)
	}
	if tr.AssigneeAdminID == nil || *tr.AssigneeAdminID != adminID {
		t.Fatal("assign_to_me failed")
	}
	if tr.SBOMRef != "reports/sbom/latest.json" {
		t.Fatal("sbom_ref linkage missing")
	}

	// --- disputed path is real: IN_PROGRESS → DISPUTED → IN_PROGRESS → FIXED ---
	disp := "second review requested — repro only works pre-auth"
	upd, err := svc.Update(ctx, adminID, AdminUpdate{
		ID: d.ID, Status: StatusDisputed, DisputeReason: &disp}, "10.0.0.9")
	if err != nil || upd.Status != StatusDisputed || upd.DisputedAt == nil {
		t.Fatalf("dispute: %v %+v", err, upd)
	}
	if _, err := svc.Update(ctx, adminID, AdminUpdate{
		ID: d.ID, Status: StatusInProgress}, "10.0.0.9"); err != nil {
		t.Fatalf("disputed→in-progress must be allowed: %v", err)
	}
	patch := "v1.2.3-sec"
	bulletin := "EXC-SA-2026-099"
	fixed, err := svc.Update(ctx, adminID, AdminUpdate{
		ID: d.ID, Status: StatusFixed, PatchRef: &patch, BulletinRef: &bulletin,
		MarkResearcherAcknowledged: true}, "10.0.0.9")
	if err != nil {
		t.Fatalf("fix transition: %v", err)
	}
	if fixed.FixedAt == nil || fixed.PatchRef != patch ||
		fixed.ResearcherAcknowledgedAt == nil {
		t.Fatalf("fix evidence missing: %+v", fixed)
	}
	// FIXED is terminal.
	if _, err := svc.Update(ctx, adminID, AdminUpdate{
		ID: d.ID, Status: StatusInProgress}, "10.0.0.9"); err == nil {
		t.Fatal("FIXED must be terminal")
	}
}

func TestVDPRejectedRebuttal(t *testing.T) {
	pool, ctx := pgGate(t)
	svc := NewService(pool, complianceResolver, &recAlerter{})
	adminID := int64(910002)
	d, _, err := svc.Submit(ctx, SubmitRequest{
		ReportID: "REJ-" + time.Now().Format("150405.000000000"),
		Title:    "cosmetic stacktrace leak", Reproduction: "see attached",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := svc.Update(ctx, adminID, AdminUpdate{
		ID: d.ID, Status: StatusRejected}, "10.0.0.9"); err != nil {
		t.Fatalf("triaged→rejected: %v", err)
	}
	// Researcher rebuts: REJECTED → DISPUTED is the live rebuttal lane.
	rb, err := svc.Update(ctx, adminID, AdminUpdate{
		ID: d.ID, Status: StatusDisputed}, "10.0.0.9")
	if err != nil || rb.Status != StatusDisputed {
		t.Fatalf("rejected→disputed must be allowed: %v", err)
	}
}

func TestVDPImmutableMilestones(t *testing.T) {
	pool, ctx := pgGate(t)
	svc := NewService(pool, complianceResolver, nil)
	adminID := int64(910003)
	d, _, err := svc.Submit(ctx, SubmitRequest{
		ReportID: "IMM-" + time.Now().Format("150405.000000000"),
		Title:    "x", Reproduction: "y",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Direct hand-edit of written SLA evidence must fail at the DB layer.
	if _, err := pool.Exec(ctx,
		`UPDATE vulnerability_disclosures SET submitted_at = now() - interval '10 days'
		 WHERE id = $1`, d.ID); err == nil {
		t.Fatal("submitted_at rewrite must be rejected by the trigger")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE vulnerability_disclosures SET ack_due_at = now() + interval '10 days'
		 WHERE id = $1`, d.ID); err == nil {
		t.Fatal("ack_due_at rewrite must be rejected")
	}
	// Hand-edit of acknowledged_at after the service stamps it must fail.
	if _, err := svc.Triage(ctx, adminID, TriageInput{
		ID: d.ID, Severity: SeverityLow, CVSSScore: fptr(1.0)}, "10.0.0.9"); err != nil {
		t.Fatalf("triage: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE vulnerability_disclosures SET acknowledged_at = now()
		 WHERE id = $1`, d.ID); err == nil {
		t.Fatal("acknowledged_at rewrite must be rejected once written")
	}
	// But a NULL milestone may still be written once (fixed_at here).
	if _, err := pool.Exec(ctx,
		`UPDATE vulnerability_disclosures SET fixed_at = now()
		 WHERE id = $1`, d.ID); err != nil {
		t.Fatalf("first write to a NULL milestone must succeed: %v", err)
	}
}

func TestVDPSLASweep(t *testing.T) {
	pool, ctx := pgGate(t)
	alerter := &recAlerter{}
	svc := NewService(pool, complianceResolver, alerter)

	// Backdate a submission past the ack deadline — insert directly to
	// place the clock deterministically (the service computes deadlines
	// at intake; ack_due_at is intake-written, so a row inserted stale
	// is indistinguishable from a neglected one).
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO vulnerability_disclosures
		    (report_id, source, title, reproduction, status,
		     submitted_at, ack_due_at, triage_due_at)
		VALUES ($1,'RESEARCHER','sla test','x','TRIAGED',
		        now() - interval '80 hours',
		        now() - interval '8 hours',
		        now() + interval '5 days')
		RETURNING id`, "SLA-"+time.Now().Format("150405.000000000")).Scan(&id); err != nil {
		t.Fatalf("stale insert: %v", err)
	}
	n, err := svc.SweepAlerts(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || len(alerter.raised) != 1 || alerter.raised[0][1] != "VDP_SLA_BREACH" ||
		alerter.raised[0][0] != "P2" {
		t.Fatalf("want exactly one P2 VDP_SLA_BREACH, got n=%d alerts=%v", n, alerter.raised)
	}
	// Once-only: a second sweep must not re-page.
	if n, err := svc.SweepAlerts(ctx, time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("second sweep must claim nothing, got n=%d err=%v", n, err)
	}
}

func TestVDPPentestSameQueue(t *testing.T) {
	pool, ctx := pgGate(t)
	svc := NewService(pool, complianceResolver, nil)
	adminID := int64(910004)
	d, err := svc.IngestPentest(ctx, adminID, SubmitRequest{
		Source: SourcePentest, Title: "pentest: TLS downgrade on FIX",
		Reproduction: "see report §4.2", AffectedComponents: []string{"FIX"},
	}, "10.0.0.9")
	if err != nil {
		t.Fatalf("pentest intake: %v", err)
	}
	if d.Source != SourcePentest || d.Status != StatusTriaged {
		t.Fatalf("pentest finding must land in the same queue: %+v", d)
	}
	if d.AckDueAt.IsZero() || d.TriageDueAt.IsZero() {
		t.Fatal("pentest finding must carry the same SLA clock")
	}
	// Public-intake abuse guard: researcher source is refused on the
	// admin path.
	if _, err := svc.IngestPentest(ctx, adminID, SubmitRequest{
		Source: SourceResearcher, Title: "x", Reproduction: "y"}, ""); err == nil {
		t.Fatal("admin intake must refuse RESEARCHER source")
	}
}

func TestVDPChangeFreezeExpedited(t *testing.T) {
	pool, ctx := pgGate(t)
	alerter := &recAlerter{}
	svc := NewService(pool, complianceResolver, alerter).
		WithChangeFreeze(func(context.Context) bool { return true })
	adminID := int64(910005)
	d, _, err := svc.Submit(ctx, SubmitRequest{
		ReportID: "FRZ-" + time.Now().Format("150405.000000000"),
		Title:    "critical in freeze", Reproduction: "y",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	tr, err := svc.Triage(ctx, adminID, TriageInput{
		ID: d.ID, Severity: SeverityCritical, CVSSScore: fptr(9.5)}, "10.0.0.9")
	if err != nil {
		t.Fatalf("triage: %v", err)
	}
	if !tr.ExpeditedPath {
		t.Fatal("critical triage inside change freeze must arm expedited_path")
	}
	// Expedite notice fires (P1 — the emergency-change lane paging).
	found := false
	for _, a := range alerter.raised {
		if a[1] == "VDP_EXPEDITED_FIX" && a[0] == "P1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected P1 VDP_EXPEDITED_FIX notice, got %v", alerter.raised)
	}
}

func fptr(f float64) *float64 { return &f }
