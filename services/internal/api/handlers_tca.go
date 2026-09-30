// Task 20.3.9 — TCA reporting surface.
//
// GET /api/v1/reports/tca/{account_id}?period=daily|monthly|quarterly
//
//	&instrument_class=SPOT|FORWARD|SWAP|NDF|OPTION|FX_MAJOR|FX_MINOR|FX_EXOTIC
//	&from=<rfc3339>&to=<rfc3339>
//
// Account scope: the path account must be the claims account or the
// caller must carry admin scope — a foreign account id is FORBIDDEN
// (same contract as the account_* handlers' rejectForeignAccount).
package api

import (
	"net/http"
	"strconv"

	"exchange/internal/analytics"
	"exchange/internal/gateway"
)

// TCAReportDeps wires ReportsTCA.
type TCAReportDeps struct {
	Reports analytics.ReportQuerier
	// Classes resolves instrument_class → instrument ids. Nil disables
	// the class filter (the query param then rejects INVALID_REQUEST —
	// a silently ignored filter is a reporting lie).
	Classes analytics.InstrumentClassResolver
}

// ReportsTCA serves GET /api/v1/reports/tca/{account_id}.
func ReportsTCA(d TCAReportDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claimsID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		acct, err := strconv.ParseInt(r.PathValue("account_id"), 10, 64)
		if err != nil || acct <= 0 {
			WriteError(w, "INVALID_REQUEST", "invalid account_id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		// Admin scope (operations/compliance reads) bypasses the
		// ownership check; a plain foreign id is FORBIDDEN.
		if acct != claimsID && !claims.HasScope(gateway.ScopeAdmin) {
			WriteError(w, "FORBIDDEN", "cannot access another account",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if d.Reports == nil {
			WriteError(w, "SERVICE_DEGRADED", "tca reports unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		period := q.Get("period")
		switch period {
		case "", analytics.PeriodDaily, analytics.PeriodMonthly, analytics.PeriodQuarterly:
		default:
			WriteError(w, "INVALID_REQUEST",
				"period must be daily|monthly|quarterly",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		f := analytics.ReportFilter{AccountID: acct, Period: period}
		if class := q.Get("instrument_class"); class != "" {
			if d.Classes == nil {
				WriteError(w, "INVALID_REQUEST",
					"instrument_class filter unavailable",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			ids, err := d.Classes.InstrumentIDs(r.Context(), class)
			if err != nil {
				WriteError(w, "INVALID_REQUEST",
					"unknown instrument_class (SPOT|FORWARD|SWAP|NDF|OPTION|FX_MAJOR|FX_MINOR|FX_EXOTIC)",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.InstrumentIDs = ids
		}
		from, err := parseTimeQuery(q.Get("from"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		to, err := parseTimeQuery(q.Get("to"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if from != nil {
			f.From = *from
		}
		if to != nil {
			f.To = *to
		}
		rows, err := d.Reports.Aggregate(r.Context(), f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []analytics.AggregateRow{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id":       acct,
			"period":           periodOr(period),
			"instrument_class": q.Get("instrument_class"),
			"buckets":          rows,
		})
	}
}

func periodOr(p string) string {
	if p == "" {
		return analytics.PeriodDaily
	}
	return p
}
