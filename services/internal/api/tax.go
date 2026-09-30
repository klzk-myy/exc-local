// Task 5.3.19 — realised gain/loss tax report over the trades table.
// Task 20.3.10 deltas — windowed reports, flow lines, Koinly export,
// and the 5-reports-per-account-per-UTC-day cap.
//
//	GET /api/v1/tax/report          — phase-plan path
//	GET /api/v1/account/tax-report  — canonical client path (spec §21:
//	                                /account/tax-report supersedes
//	                                /tax/report per remediation #35; both
//	                                serve the same producer here)
//
// Query params:
//
//	year   — canonical report year (e.g. ?year=2026), OR
//	from,to — explicit UTC window (RFC3339 or YYYY-MM-DD; to is
//	         exclusive and clamps to now when it projects into the
//	         future). from/to take precedence over year.
//	method — FIFO|LIFO|HIFO|AVG_COST (default FIFO — the §12.7
//	         canonical method; non-FIFO are labelled planning
//	         projections).
//	format — json|csv|pdf|koinly (or Accept: text/csv /
//	         application/pdf).
//
// Rate limit: when a tax.DailyLimiter is wired (variadic dep — the
// frozen gateway map calls TaxReport(svc) alone), issuance is capped
// at tax.TaxReportsPerDay (5) per account per UTC day; exceeding
// returns RATE_LIMIT_TIER_EXCEEDED (429).
package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/tax"
)

// TaxReport serves both registered tax-report paths. Optional extra
// deps (variadic — keeps the frozen orchestrator call signature):
// pass a tax.DailyLimiter to enforce the per-day generation cap.
func TaxReport(svc *tax.Service, extra ...any) http.HandlerFunc {
	var lim tax.DailyLimiter
	for _, e := range extra {
		if l, ok := e.(tax.DailyLimiter); ok {
			lim = l
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		from, to, year, err := taxWindow(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST",
				err.Error(), gateway.RequestIDFrom(r.Context()), nil)
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
		// Task 20.3.10 cap — reserve a slot before the expensive read.
		var remaining int
		limited := false
		if lim != nil {
			rem, lerr := lim.Allow(r.Context(), claims.AccountID)
			switch {
			case errors.Is(lerr, tax.ErrDailyReportLimit):
				WriteError(w, "RATE_LIMIT_TIER_EXCEEDED",
					"tax reports are limited to "+strconv.Itoa(tax.TaxReportsPerDay)+
						" per account per UTC day",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			case lerr != nil:
				WriteError(w, "SERVICE_DEGRADED",
					"tax report limiter unavailable", gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			remaining = rem
			limited = true
		}
		rep, err := svc.ReportRange(r.Context(), claims.AccountID, from, to, method)
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
		if limited {
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
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
		case "koinly":
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition",
				`attachment; filename="koinly-export-`+strconv.Itoa(year)+`.csv"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tax.RenderKoinlyCSV(rep))
		case "json":
			WriteJSON(w, http.StatusOK, rep)
		default:
			WriteError(w, "INVALID_REQUEST",
				"format must be json|csv|pdf|koinly", gateway.RequestIDFrom(r.Context()), nil)
		}
	}
}

// taxWindow resolves the report window: explicit ?from&to= wins over
// ?year=; with neither, year is required (the canonical path).
// Returns (from, to, displayYear).
func taxWindow(r *http.Request) (time.Time, time.Time, int, error) {
	q := r.URL.Query()
	parseTS := func(s string) (time.Time, error) {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			return t, nil
		}
		return time.Parse(time.RFC3339, s)
	}
	fromS, toS := q.Get("from"), q.Get("to")
	if fromS != "" || toS != "" {
		from, err := parseTS(fromS)
		if err != nil {
			return time.Time{}, time.Time{}, 0,
				errors.New("from must be YYYY-MM-DD or RFC3339")
		}
		to, err := parseTS(toS)
		if err != nil {
			return time.Time{}, time.Time{}, 0,
				errors.New("to must be YYYY-MM-DD or RFC3339")
		}
		return from.UTC(), to.UTC(), from.UTC().Year(), nil
	}
	yearStr := q.Get("year")
	year, err := strconv.Atoi(yearStr)
	if yearStr == "" || err != nil {
		return time.Time{}, time.Time{}, 0,
			errors.New("year query parameter required (e.g. ?year=2026)")
	}
	return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC), year, nil
}
