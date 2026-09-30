// handlers_marketing.go — Phase-20 Task 20.3.16 (spec §16.11, §24
// #381): marketing-operations reporting.
//
//	GET /api/v1/admin/promotions/report
//	    — promo inventory (by status/channel, approval-SLA compliance,
//	      30/90-day expiries, expired-content cache flag) + consent
//	      cohorts at the 100-record anonymity floor + the fixed
//	      non-scope statement.
//
// The route is registered in the frozen Phase-05 route registry bound
// to the Read-Only Auditor role; the task calls for Compliance
// Officer. Both roles are read-only compliance surfaces — see the
// task report's deviation note (registry is Phase-05-owned).
//
// Source relations are Phase-21-owned (financial_promotions,
// account_consent_states): while the tables are absent the stores return
// ErrMarketingSourceUnavailable and this handler emits a clean 503
// with a source_unavailable note — never a fabricated empty report.
package api

import (
	"errors"
	"net/http"

	"exchange/internal/analytics"
	"exchange/internal/gateway"
)

// AdminPromotionsReport serves GET /api/v1/admin/promotions/report.
func AdminPromotionsReport(svc *analytics.MarketingReportService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rep, err := svc.Report(r.Context())
		switch {
		case err == nil:
			WriteJSON(w, http.StatusOK, rep)
		case errors.Is(err, analytics.ErrMarketingSourceUnavailable):
			WriteError(w, "SERVICE_DEGRADED",
				"promotion/consent source unavailable",
				gateway.RequestIDFrom(r.Context()),
				map[string]any{
					"source_unavailable": "financial_promotions lands Phase-21 Task 21.3.26; " +
						"account_consent_states lands Phase-21 Task 21.3.7",
				})
		default:
			writeServiceErr(w, r, err)
		}
	}
}
