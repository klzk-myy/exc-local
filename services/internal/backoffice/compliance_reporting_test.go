// Task 24.3.5 tests — per-type export over the Phase-21 seams,
// fail-closed nil sources, monthly-summary aggregation honesty.
package backoffice

import (
	"context"
	"testing"
	"time"

	"exchange/internal/compliance"
	"exchange/internal/compliance/reporting"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Fakes for the Phase-21 seams.
// ---------------------------------------------------------------------------

type fakeRegEvents struct {
	byRegime map[reporting.Regime][]reporting.Event
}

func (f *fakeRegEvents) ExportEvents(_ context.Context, regime reporting.Regime,
	from, to *time.Time, limit int) ([]reporting.Event, error) {
	var out []reporting.Event
	for _, e := range f.byRegime[regime] {
		if len(out) >= limit {
			break
		}
		out = append(out, e)
	}
	return out, nil
}

type fakeCTR struct{ rows []compliance.CTRReport }

func (f *fakeCTR) ListCTR(_ context.Context, status string, limit int) ([]compliance.CTRReport, error) {
	return f.rows, nil
}

type fakeSAR struct{ rows []compliance.SARReport }

func (f *fakeSAR) List(_ context.Context, status string, limit int) ([]compliance.SARReport, error) {
	return f.rows, nil
}

type fakeBasel struct{ rows []compliance.BaselReport }

func (f *fakeBasel) Get(_ context.Context, period time.Time) (*compliance.BaselReport, error) {
	for i := range f.rows {
		if f.rows[i].Period.Equal(period) {
			return &f.rows[i], nil
		}
	}
	return nil, nil
}

func (f *fakeBasel) List(_ context.Context, limit int) ([]compliance.BaselReport, error) {
	return f.rows, nil
}

// ---------------------------------------------------------------------------
// Per-type exports.
// ---------------------------------------------------------------------------

func svcWithAll(t *testing.T) (*ComplianceReportService, *fakeRegEvents,
	*fakeCTR, *fakeSAR, *fakeBasel) {
	t.Helper()
	ev := &fakeRegEvents{byRegime: map[reporting.Regime][]reporting.Event{
		reporting.RegimeMIFID2:    {{Regime: reporting.RegimeMIFID2}},
		reporting.RegimeEMIRREFIT: {{Regime: reporting.RegimeEMIRREFIT}},
	}}
	ctr := &fakeCTR{rows: []compliance.CTRReport{{
		BusinessDate: boDay("2026-09-05"), Status: "FILED"},
		{BusinessDate: boDay("2026-08-01"), Status: "TRIGGERED"}}}
	sar := &fakeSAR{rows: []compliance.SARReport{{
		DetectedAt: boDay("2026-09-10"), Status: "FILED"}}}
	basel := &fakeBasel{rows: []compliance.BaselReport{{
		Period: boDay("2026-09-01"), InputsComplete: true}}}
	svc, err := NewComplianceReportService(ev, ctr, sar, basel)
	if err != nil {
		t.Fatal(err)
	}
	return svc, ev, ctr, sar, basel
}

func TestReport_MiFID2(t *testing.T) {
	svc, _, _, _, _ := svcWithAll(t)
	out, err := svc.Export(context.Background(), ReportRequest{Type: "mifid2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 || out.Type != ReportMiFID2 {
		t.Fatalf("export: %+v", out)
	}
}

func TestReport_EMIR(t *testing.T) {
	svc, _, _, _, _ := svcWithAll(t)
	out, err := svc.Export(context.Background(), ReportRequest{Type: ReportEMIR})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("export: %+v", out)
	}
}

func TestReport_FinCEN_CTR_DateFilter(t *testing.T) {
	svc, _, _, _, _ := svcWithAll(t)
	from, to := boDay("2026-09-01"), boDay("2026-09-30")
	out, err := svc.Export(context.Background(), ReportRequest{
		Type: ReportFinCENCTR, From: &from, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.CTRReports) != 1 || out.CTRReports[0].Status != "FILED" {
		t.Fatalf("ctr window: %+v", out.CTRReports)
	}
}

func TestReport_FinCEN_SAR(t *testing.T) {
	svc, _, _, _, _ := svcWithAll(t)
	out, err := svc.Export(context.Background(), ReportRequest{Type: ReportFinCENSAR})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SARReports) != 1 {
		t.Fatalf("sar: %+v", out.SARReports)
	}
}

func TestReport_Basel3(t *testing.T) {
	svc, _, _, _, _ := svcWithAll(t)
	out, err := svc.Export(context.Background(), ReportRequest{Type: ReportBasel3})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.BaselReports) != 1 || !out.BaselReports[0].InputsComplete {
		t.Fatalf("basel: %+v", out.BaselReports)
	}
}

func TestReport_MonthlySummary(t *testing.T) {
	svc, _, _, _, _ := svcWithAll(t)
	sep := boDay("2026-09-15")
	out, err := svc.Export(context.Background(), ReportRequest{
		Type: ReportMonthlySummary, From: &sep})
	if err != nil {
		t.Fatal(err)
	}
	s := out.Summary
	if s == nil || s.Month != "2026-09" {
		t.Fatalf("summary month: %+v", s)
	}
	if s.RegimeCounts["MIFID2"] != 1 || s.RegimeCounts["EMIR_REFIT"] != 1 {
		t.Fatalf("regime counts: %+v", s.RegimeCounts)
	}
	if s.CTRTotal != 1 || s.CTRFiled != 1 {
		t.Fatalf("ctr counts: %+v", s)
	}
	if s.SARTotal != 1 || s.SARFiled != 1 {
		t.Fatalf("sar counts: %+v", s)
	}
	if s.BaselReport == nil || !s.InputsComplete {
		t.Fatalf("basel section: %+v", s)
	}
}

// ---------------------------------------------------------------------------
// Fail-closed nil seams.
// ---------------------------------------------------------------------------

func TestReport_NilSourcesDegrade(t *testing.T) {
	svc, err := NewComplianceReportService(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, typ := range []string{
		ReportMiFID2, ReportEMIR, ReportFinCENCTR, ReportFinCENSAR,
		ReportBasel3} {
		_, err := svc.Export(ctx, ReportRequest{Type: typ})
		if err == nil || excerrors.CodeOf(err) != "SERVICE_DEGRADED" {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	// Monthly summary degrades honestly: missing sections flagged,
	// nothing fabricated.
	sep := boDay("2026-09-15")
	out, err := svc.Export(ctx, ReportRequest{
		Type: ReportMonthlySummary, From: &sep})
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary.InputsComplete || len(out.Summary.MissingSections) != 4 {
		t.Fatalf("summary with all sources absent: %+v", out.Summary)
	}
}

func TestReport_UnknownTypeRejected(t *testing.T) {
	svc, _, _, _, _ := svcWithAll(t)
	if _, err := svc.Export(context.Background(),
		ReportRequest{Type: "ALCAPONE"}); err == nil ||
		excerrors.CodeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("unknown type: %v", err)
	}
}
