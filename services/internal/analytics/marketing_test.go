// Task 20.3.16 unit tests — in-memory stores (no PG).
package analytics

import (
	"context"
	"testing"
	"time"
)

type fakePromoStore struct {
	rows []PromotionRow
	err  error
}

func (f *fakePromoStore) Promotions(context.Context) ([]PromotionRow, error) {
	return f.rows, f.err
}

type fakeConsentStore struct {
	rows []ConsentCohort
	err  error
}

func (f *fakeConsentStore) ConsentCohorts(context.Context, string) ([]ConsentCohort, error) {
	return f.rows, f.err
}

func mktSvc(t *testing.T, promos []PromotionRow, cohorts []ConsentCohort) *MarketingReportService {
	t.Helper()
	svc, err := NewMarketingReportService(
		&fakePromoStore{rows: promos}, &fakeConsentStore{rows: cohorts})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetClockForTest(func() time.Time {
		return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	})
	return svc
}

func mktTime(daysAgo int) *time.Time {
	t := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).Add(-time.Duration(daysAgo) * 24 * time.Hour)
	return &t
}

// Inventory axes: status/channel buckets, SLA compliance within the
// 5-day review window, 30/90-day expiry horizons, expired-approved
// cache flag, aging drafts.
func TestMarketingReport_InventoryAxes(t *testing.T) {
	approvedOK := PromotionRow{
		PromotionID: 1, Channel: "WEB", Version: 1, ApprovalStatus: "APPROVED",
		CreatedAt: mktTime(3), ApprovedAt: mktTime(1),
		ApprovedUntil: mktTime(-60), // expires in 60d
	}
	approvedLate := PromotionRow{
		PromotionID: 2, Channel: "EMAIL", Version: 2, ApprovalStatus: "APPROVED",
		CreatedAt: mktTime(10), ApprovedAt: mktTime(1), // 9d > 5d window
		ApprovedUntil: mktTime(-10), // expires in 10d
	}
	expired := PromotionRow{
		PromotionID: 3, Channel: "WEB", Version: 1, ApprovalStatus: "APPROVED",
		CreatedAt: mktTime(40), ApprovedAt: mktTime(38),
		ApprovedUntil: mktTime(1), // already past
	}
	staleDraft := PromotionRow{
		PromotionID: 4, Channel: "PUSH", Version: 1, ApprovalStatus: "DRAFT",
		CreatedAt: mktTime(9), // aging past the review window
	}
	svc := mktSvc(t,
		[]PromotionRow{approvedOK, approvedLate, expired, staleDraft}, nil)
	rep, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := rep.Promotions
	if p.ByStatus["APPROVED"] != 3 || p.ByStatus["DRAFT"] != 1 {
		t.Fatalf("by_status=%v", p.ByStatus)
	}
	if p.ByChannel["WEB"] != 2 || p.ByChannel["EMAIL"] != 1 || p.ByChannel["PUSH"] != 1 {
		t.Fatalf("by_channel=%v", p.ByChannel)
	}
	if p.SLACompliant != 2 || p.SLABreached != 1 {
		t.Fatalf("sla=%+v", p)
	}
	if p.ExpiringIn30D != 1 || p.ExpiringIn90D != 2 {
		t.Fatalf("expiry horizons=%+v", p)
	}
	if p.ExpiredFlagged != 1 {
		t.Fatalf("expired flag=%+v", p)
	}
	if p.DraftsAging != 1 {
		t.Fatalf("drafts aging=%+v", p)
	}
	if p.ReviewWindowDays != 5 {
		t.Fatalf("window=%d", p.ReviewWindowDays)
	}
	if rep.NonScope != MarketingNonScopeStatement {
		t.Fatal("non-scope statement missing")
	}
}

// Cohort floor: <100 rows → result INSUFFICIENT_COHORT with the count
// suppressed; ≥100 → the count rides.
func TestMarketingReport_CohortFloor(t *testing.T) {
	cohorts := []ConsentCohort{
		{Purpose: "MARKETING", Channel: "EMAIL", State: "OPTED_IN", RowCount: 250},
		{Purpose: "MARKETING", Channel: "EMAIL", State: "OPTED_OUT", RowCount: 40},
		{Purpose: "MARKETING", Channel: "SMS", State: "OPTED_IN", RowCount: 99},
	}
	svc := mktSvc(t, nil, cohorts)
	rep, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ConsentCohorts) != 3 || rep.CohortFloor != CohortFloor {
		t.Fatalf("cohorts=%+v floor=%d", rep.ConsentCohorts, rep.CohortFloor)
	}
	c0 := rep.ConsentCohorts[0]
	if c0.Result != "OK" || c0.Count == nil || *c0.Count != 250 {
		t.Fatalf("cohort0=%+v", c0)
	}
	for _, c := range rep.ConsentCohorts[1:] {
		if c.Result != "INSUFFICIENT_COHORT" || c.Count != nil {
			t.Fatalf("floor breach leaked count: %+v", c)
		}
	}
}

// Missing Phase-21 relations degrade to ErrMarketingSourceUnavailable —
// the handler maps it to a clean 503.
func TestMarketingReport_SourceUnavailable(t *testing.T) {
	svc, err := NewMarketingReportService(
		&fakePromoStore{err: ErrMarketingSourceUnavailable},
		&fakeConsentStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Report(context.Background()); err == nil {
		t.Fatal("missing source accepted")
	}
	svc2, err := NewMarketingReportService(
		&fakePromoStore{},
		&fakeConsentStore{err: ErrMarketingSourceUnavailable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.Report(context.Background()); err == nil {
		t.Fatal("missing consent source accepted")
	}
}

// Nil stores fail closed at construction.
func TestMarketingReport_NilDeps(t *testing.T) {
	if _, err := NewMarketingReportService(nil, &fakeConsentStore{}); err == nil {
		t.Fatal("nil promo store accepted")
	}
	if _, err := NewMarketingReportService(&fakePromoStore{}, nil); err == nil {
		t.Fatal("nil consent store accepted")
	}
}
