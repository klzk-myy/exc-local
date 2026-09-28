// Task 5.3.19 — realised gain/loss tax report over the trades table.
//
//	GET /api/v1/tax/report          — phase-plan path
//	GET /api/v1/account/tax-report  — canonical client path (spec §21:
//	                                /account/tax-report supersedes
//	                                /tax/report per remediation #35; both
//	                                serve the same producer here)
//
// Query params: year (required), method=FIFO|LIFO|HIFO|AVG_COST (default
// FIFO — the §12.7 canonical method; non-FIFO are labelled planning
// projections), format=json|csv|pdf (or Accept: text/csv /
// application/pdf).
package api

import (
	"net/http"
	"strconv"
	"strings"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/tax"
)

// TaxReport serves both registered tax-report paths.
func TaxReport(svc *tax.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		yearStr := r.URL.Query().Get("year")
		year, err := strconv.Atoi(yearStr)
		if yearStr == "" || err != nil {
			WriteError(w, "INVALID_REQUEST",
				"year query parameter required (e.g. ?year=2026)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		method, err := tax.ParseMethod(r.URL.Query().Get("method"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST",
				err.Error(), gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		format := r.URL.Query().Get("format")
		if format == "" {
			// Content negotiation: explicit Accept wins when it names a
			// binary format; otherwise JSON.
			accept := r.Header.Get("Accept")
			switch {
			case strings.Contains(accept, "text/csv"):
				format = "csv"
			case strings.Contains(accept, "application/pdf"):
				format = "pdf"
			default:
				format = "json"
			}
		}
		rep, err := svc.Report(r.Context(), claims.AccountID, year, method)
		if err != nil {
			if strings.HasPrefix(err.Error(), "tax: ") {
				WriteError(w, "INVALID_REQUEST",
					err.Error(), gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			WriteError(w, "SERVICE_DEGRADED",
				"tax report store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		switch format {
		case "csv":
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition",
				`attachment; filename="tax-report-`+strconv.Itoa(year)+`.csv"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tax.RenderCSV(rep))
		case "pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition",
				`attachment; filename="tax-report-`+strconv.Itoa(year)+`.pdf"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tax.RenderPDF(rep))
		case "json":
			WriteJSON(w, http.StatusOK, rep)
		default:
			WriteError(w, "INVALID_REQUEST",
				"format must be json|csv|pdf", gateway.RequestIDFrom(r.Context()), nil)
		}
	}
}
