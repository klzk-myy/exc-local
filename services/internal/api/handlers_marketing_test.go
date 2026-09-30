// Task 20.3.16 — marketing-ops report handler coverage.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exchange/internal/analytics"
)

type apiPromoStore struct {
	rows []analytics.PromotionRow
	err  error
}

func (f apiPromoStore) Promotions(context.Context) ([]analytics.PromotionRow, error) {
	return f.rows, f.err
}

type apiConsentStore struct {
	rows []analytics.ConsentCohort
	err  error
}

func (f apiConsentStore) ConsentCohorts(context.Context, string) ([]analytics.ConsentCohort, error) {
	return f.rows, f.err
}

func TestAdminPromotionsReportOK(t *testing.T) {
	svc, err := analytics.NewMarketingReportService(
		apiPromoStore{rows: []analytics.PromotionRow{
			{PromotionID: 1, Channel: "WEB", Version: 1, ApprovalStatus: "DRAFT"},
		}},
		apiConsentStore{rows: []analytics.ConsentCohort{
			{Purpose: "MARKETING", Channel: "EMAIL", State: "OPTED_IN", RowCount: 150},
			{Purpose: "MARKETING", Channel: "EMAIL", State: "OPTED_OUT", RowCount: 12},
		}})
	if err != nil {
		t.Fatal(err)
	}
	h := AdminPromotionsReport(svc)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/promotions/report", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"cohort_floor":100`, `"non_scope_statement"`,
		`"INSUFFICIENT_COHORT"`, `"by_status"`, `"by_channel"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("report missing %s:\n%s", want, body)
		}
	}
	// Sub-floor cohort must not leak a count.
	if strings.Contains(body, `"count":12`) {
		t.Fatalf("sub-floor count leaked:\n%s", body)
	}
}

// A missing Phase-21 relation surfaces as a clean 503 — the source
// unavailability degrades, it never fabricates an empty report.
func TestAdminPromotionsReportSourceDown(t *testing.T) {
	svc, err := analytics.NewMarketingReportService(
		apiPromoStore{err: analytics.ErrMarketingSourceUnavailable},
		apiConsentStore{})
	if err != nil {
		t.Fatal(err)
	}
	h := AdminPromotionsReport(svc)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/promotions/report", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if env := decodeErr(t, rec); env.Error != "SERVICE_DEGRADED" {
		t.Fatalf("code=%s", env.Error)
	}
}
