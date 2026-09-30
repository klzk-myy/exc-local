// Task 24.3.5 — Compliance Reporting (spec §17; Phase-24 AC #16–#20).
//
// The export surface (GET /api/v1/admin/compliance-report?type=&from=&to=)
// CONSUMES the Phase-21 stores — it never re-implements report
// generation:
//
//	MIFID2         → reporting.Store.ExportEvents(RegimeMIFID2)     (Task 21.3.14 canonical store)
//	EMIR           → reporting.Store.ExportEvents(RegimeEMIRREFIT)
//	FINCEN_CTR     → compliance.AMLService.ListCTR filtered by business_date
//	FINCEN_SAR     → compliance.SARService.List filtered by detected_at
//	BASEL3         → compliance.BaselService.List filtered by period (Task 21.3.13)
//	MONTHLY_SUMMARY→ aggregate counts across all of the above for the
//	                 calendar month containing `from` (or now).
//
// Fail-closed (spec §2.7): a nil source seam for the requested type is
// SERVICE_DEGRADED 503 — never an empty-success report. Report content
// is never fabricated; exports carry only what the underlying store
// returned.
package backoffice

import (
	"context"
	"fmt"
	"strings"
	"time"

	"exchange/internal/compliance"
	"exchange/internal/compliance/reporting"
	excerrors "exchange/pkg/errors"
)

// Report type vocabulary for ?type=.
const (
	ReportMiFID2         = "MIFID2"
	ReportEMIR           = "EMIR"
	ReportFinCENCTR      = "FINCEN_CTR"
	ReportFinCENSAR      = "FINCEN_SAR"
	ReportBasel3         = "BASEL3"
	ReportMonthlySummary = "MONTHLY_SUMMARY"
)

// ReportRequest is the export query.
type ReportRequest struct {
	Type  string
	From  *time.Time
	To    *time.Time
	Limit int
}

// ---------------------------------------------------------------------------
// Source seams — satisfied by the Phase-21 stores/services.
// ---------------------------------------------------------------------------

// RegEventExporter is the regulatory event export seam —
// reporting.Store.ExportEvents satisfies it.
type RegEventExporter interface {
	ExportEvents(ctx context.Context, regime reporting.Regime, from, to *time.Time, limit int) ([]reporting.Event, error)
}

// CTRSource is the FinCEN CTR seam — *compliance.AMLService.ListCTR.
type CTRSource interface {
	ListCTR(ctx context.Context, status string, limit int) ([]compliance.CTRReport, error)
}

// SARSource is the FinCEN SAR seam — *compliance.SARService.List.
type SARSource interface {
	List(ctx context.Context, status string, limit int) ([]compliance.SARReport, error)
}

// BaselSource is the Basel III seam — *compliance.BaselService.
type BaselSource interface {
	Get(ctx context.Context, period time.Time) (*compliance.BaselReport, error)
	List(ctx context.Context, limit int) ([]compliance.BaselReport, error)
}

// ComplianceExport is the handler payload — only the section matching
// the requested type is populated; nothing is fabricated.
type ComplianceExport struct {
	Type         string                   `json:"type"`
	From         *time.Time               `json:"from,omitempty"`
	To           *time.Time               `json:"to,omitempty"`
	GeneratedAt  time.Time                `json:"generated_at"`
	Events       []reporting.Event        `json:"events,omitempty"`
	CTRReports   []compliance.CTRReport   `json:"ctr_reports,omitempty"`
	SARReports   []compliance.SARReport   `json:"sar_reports,omitempty"`
	BaselReports []compliance.BaselReport `json:"basel_reports,omitempty"`
	Summary      *MonthlySummary          `json:"summary,omitempty"`
}

// MonthlySummary is the monthly compliance roll-up (AC #20): event
// counts per reporting regime, FinCEN CTR/SAR counts (filed vs total)
// and the latest Basel III snapshot for the month.
type MonthlySummary struct {
	Month           string                  `json:"month"` // YYYY-MM
	RegimeCounts    map[string]int          `json:"regime_counts"`
	CTRTotal        int                     `json:"ctr_total"`
	CTRFiled        int                     `json:"ctr_filed"`
	SARTotal        int                     `json:"sar_total"`
	SARFiled        int                     `json:"sar_filed"`
	BaselReport     *compliance.BaselReport `json:"basel_report,omitempty"`
	InputsComplete  bool                    `json:"inputs_complete"`
	MissingSections []string                `json:"missing_sections,omitempty"`
}

// ComplianceReportService fans a report request out to the Phase-21
// stores.
type ComplianceReportService struct {
	events RegEventExporter
	ctr    CTRSource
	sar    SARSource
	basel  BaselSource
	clock  func() time.Time
}

// NewComplianceReportService wires the service. Individual sources may
// be nil — the requested report type fails closed SERVICE_DEGRADED; the
// monthly summary marks missing sections rather than fabricating zeros.
func NewComplianceReportService(events RegEventExporter, ctr CTRSource, sar SARSource, basel BaselSource) (*ComplianceReportService, error) {
	return &ComplianceReportService{
		events: events, ctr: ctr, sar: sar, basel: basel, clock: time.Now}, nil
}

// WithClock overrides the clock (tests).
func (s *ComplianceReportService) WithClock(c func() time.Time) *ComplianceReportService {
	s.clock = c
	return s
}

func reportSourceDegraded(what string) error {
	return excerrors.New("SERVICE_DEGRADED",
		fmt.Sprintf("compliance report source unavailable: %s", what))
}

// Export resolves the request type and returns the typed export.
func (s *ComplianceReportService) Export(ctx context.Context, r ReportRequest) (*ComplianceExport, error) {
	out := &ComplianceExport{
		Type:        strings.ToUpper(strings.TrimSpace(r.Type)),
		From:        r.From,
		To:          r.To,
		GeneratedAt: s.clock().UTC(),
	}
	limit := r.Limit
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	switch out.Type {
	case ReportMiFID2:
		return s.exportRegime(ctx, out, reporting.RegimeMIFID2, limit)
	case ReportEMIR:
		return s.exportRegime(ctx, out, reporting.RegimeEMIRREFIT, limit)
	case ReportFinCENCTR:
		if s.ctr == nil {
			return nil, reportSourceDegraded("FinCEN CTR store")
		}
		rows, err := s.ctr.ListCTR(ctx, "", limit)
		if err != nil {
			return nil, err
		}
		out.CTRReports = filterCTR(rows, r.From, r.To)
		return out, nil
	case ReportFinCENSAR:
		if s.sar == nil {
			return nil, reportSourceDegraded("FinCEN SAR store")
		}
		rows, err := s.sar.List(ctx, "", limit)
		if err != nil {
			return nil, err
		}
		out.SARReports = filterSAR(rows, r.From, r.To)
		return out, nil
	case ReportBasel3:
		if s.basel == nil {
			return nil, reportSourceDegraded("Basel III store")
		}
		rows, err := s.basel.List(ctx, limit)
		if err != nil {
			return nil, err
		}
		out.BaselReports = filterBasel(rows, r.From, r.To)
		return out, nil
	case ReportMonthlySummary:
		return s.monthly(ctx, out, limit)
	default:
		return nil, excerrors.New("INVALID_REQUEST", fmt.Sprintf(
			"type must be one of %s|%s|%s|%s|%s|%s",
			ReportMiFID2, ReportEMIR, ReportFinCENCTR, ReportFinCENSAR,
			ReportBasel3, ReportMonthlySummary))
	}
}

func (s *ComplianceReportService) exportRegime(ctx context.Context, out *ComplianceExport, regime reporting.Regime, limit int) (*ComplianceExport, error) {
	if s.events == nil {
		return nil, reportSourceDegraded("regulatory reporting event store")
	}
	evs, err := s.events.ExportEvents(ctx, regime, out.From, out.To, limit)
	if err != nil {
		return nil, err
	}
	if evs == nil {
		evs = []reporting.Event{}
	}
	out.Events = evs
	return out, nil
}

// inReportRange reports whether t falls inside [from,to) (nil bounds open).
func inReportRange(t time.Time, from, to *time.Time) bool {
	if from != nil && t.Before(*from) {
		return false
	}
	if to != nil && !t.Before(*to) {
		return false
	}
	return true
}

func filterCTR(rows []compliance.CTRReport, from, to *time.Time) []compliance.CTRReport {
	out := []compliance.CTRReport{}
	for _, r := range rows {
		if inReportRange(r.BusinessDate, from, to) {
			out = append(out, r)
		}
	}
	return out
}

func filterSAR(rows []compliance.SARReport, from, to *time.Time) []compliance.SARReport {
	out := []compliance.SARReport{}
	for _, r := range rows {
		if inReportRange(r.DetectedAt, from, to) {
			out = append(out, r)
		}
	}
	return out
}

func filterBasel(rows []compliance.BaselReport, from, to *time.Time) []compliance.BaselReport {
	out := []compliance.BaselReport{}
	for _, r := range rows {
		if inReportRange(r.Period, from, to) {
			out = append(out, r)
		}
	}
	return out
}

// monthly aggregates the calendar month containing r.From (default: the
// current month). Each section degrades independently: a nil source
// marks the section missing rather than fabricating zeros — the
// summary is honest about what it could not see.
func (s *ComplianceReportService) monthly(ctx context.Context, out *ComplianceExport, limit int) (*ComplianceExport, error) {
	anchor := s.clock().UTC()
	if out.From != nil {
		anchor = *out.From
	}
	monthStart := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)

	sum := &MonthlySummary{
		Month:          monthStart.Format("2006-01"),
		RegimeCounts:   map[string]int{},
		InputsComplete: true,
	}
	if s.events != nil {
		for _, regime := range []reporting.Regime{
			reporting.RegimeMIFID2, reporting.RegimeEMIRREFIT,
			reporting.RegimeCFTCP43, reporting.RegimeCFTCP45,
		} {
			evs, err := s.events.ExportEvents(ctx, regime, &monthStart, &monthEnd, limit)
			if err != nil {
				return nil, err
			}
			sum.RegimeCounts[string(regime)] = len(evs)
		}
	} else {
		sum.InputsComplete = false
		sum.MissingSections = append(sum.MissingSections, "regulatory_events")
	}
	if s.ctr != nil {
		rows, err := s.ctr.ListCTR(ctx, "", limit)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if !inReportRange(r.BusinessDate, &monthStart, &monthEnd) {
				continue
			}
			sum.CTRTotal++
			if r.Status == "FILED" {
				sum.CTRFiled++
			}
		}
	} else {
		sum.InputsComplete = false
		sum.MissingSections = append(sum.MissingSections, "fincen_ctr")
	}
	if s.sar != nil {
		rows, err := s.sar.List(ctx, "", limit)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if !inReportRange(r.DetectedAt, &monthStart, &monthEnd) {
				continue
			}
			sum.SARTotal++
			if r.Status == "FILED" {
				sum.SARFiled++
			}
		}
	} else {
		sum.InputsComplete = false
		sum.MissingSections = append(sum.MissingSections, "fincen_sar")
	}
	if s.basel != nil {
		rep, err := s.basel.Get(ctx, monthStart)
		if err != nil {
			return nil, err
		}
		if rep != nil {
			sum.BaselReport = rep
		}
	} else {
		sum.InputsComplete = false
		sum.MissingSections = append(sum.MissingSections, "basel3")
	}
	out.Summary = sum
	return out, nil
}
