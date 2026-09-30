// handlers_finance.go — Phase-20 Task 20.3.7 house-finance surface
// (spec §16.5; Finance Ops role enforced by the route registry):
//
//	GET /api/v1/admin/finance/trial-balance  ?date=YYYY-MM-DD&currency=&format=json|csv
//	GET /api/v1/admin/finance/pnl            ?from=&to=&format=json|csv
//	GET /api/v1/admin/finance/balance-sheet  ?date=YYYY-MM-DD&format=json|csv
//
// date defaults to yesterday (the last completed EOD); the exports carry
// the sub-ledger reconciliation footer (client balances / nostro /
// insurance fund / fee income variance report).
package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/gateway"
)

// financeService is the narrow handler seam — *analytics.TrialBalanceService.
type financeService interface {
	Compute(ctx context.Context, day time.Time) (*analytics.TrialBalance, error)
	Reconcile(ctx context.Context, day time.Time) ([]analytics.ReconCheck, error)
	PeriodPnL(ctx context.Context, start, end time.Time) ([]analytics.PeriodPnL, error)
	ExportCSV(ctx context.Context, kind analytics.ExportKind, day time.Time) ([]byte, *analytics.TrialBalance, []analytics.ReconCheck, error)
}

// parseDateParam resolves ?name=YYYY-MM-DD; empty → fallback.
func parseDateParam(r *http.Request, name string, fallback time.Time) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	return d.UTC(), nil
}

// wantsCSV reports format=csv or an Accept: text/csv preference.
func wantsCSV(r *http.Request) bool {
	f := strings.ToLower(r.URL.Query().Get("format"))
	if f == "csv" {
		return true
	}
	return f == "" && strings.Contains(r.Header.Get("Accept"), "text/csv")
}

// AdminTrialBalance — GET /api/v1/admin/finance/trial-balance.
// ?currency= filters the currency sections; format=csv streams the export
// with the reconciliation footer.
func AdminTrialBalance(fs financeService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		day, err := parseDateParam(r, "date",
			time.Now().UTC().AddDate(0, 0, -1))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "date must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if wantsCSV(r) {
			csv, _, _, err := fs.ExportCSV(r.Context(), analytics.ExportTrialBalance, day)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition",
				`attachment; filename="trial-balance-`+day.Format("2006-01-02")+`.csv"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(csv)
			return
		}
		tb, err := fs.Compute(r.Context(), day)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		ccy := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("currency")))
		if ccy != "" {
			filtered := tb.Currencies[:0]
			for _, c := range tb.Currencies {
				if c.Currency == ccy {
					filtered = append(filtered, c)
				}
			}
			tb.Currencies = filtered
		}
		WriteJSON(w, http.StatusOK, tb)
	}
}

// AdminFinancePnL — GET /api/v1/admin/finance/pnl?from=&to=&format=.
// The P&L is a period flow: ?from=&to= bound the window (defaults: the
// full prior UTC day). Reconciliation checks ride the CSV footer.
func AdminFinancePnL(fs financeService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		yesterday := time.Now().UTC().AddDate(0, 0, -1)
		day := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(),
			0, 0, 0, 0, time.UTC)
		start, err := parseDateParam(r, "from", day)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		end, err := parseDateParam(r, "to", day.AddDate(0, 0, 1))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !end.After(start) {
			WriteError(w, "INVALID_REQUEST", "to must be after from",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		pls, err := fs.PeriodPnL(r.Context(), start, end)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		checks, err := fs.Reconcile(r.Context(), end.Add(-time.Second))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if wantsCSV(r) {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition",
				`attachment; filename="pnl-`+start.Format("2006-01-02")+`.csv"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(analytics.RenderPnLCSV(pls, start, end, checks))
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"period_start":   start.Format("2006-01-02"),
			"period_end":     end.Format("2006-01-02"),
			"pnl":            pls,
			"reconciliation": checks,
		})
	}
}

// AdminFinanceBalanceSheet — GET /api/v1/admin/finance/balance-sheet?date=.
// Cumulative-as-of balance sheet (ASSET/LIABILITY/EQUITY sections + the
// unclosed net-income memo line); CSV export carries the recon footer.
func AdminFinanceBalanceSheet(fs financeService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		day, err := parseDateParam(r, "date",
			time.Now().UTC().AddDate(0, 0, -1))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "date must be YYYY-MM-DD",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if wantsCSV(r) {
			csv, _, _, err := fs.ExportCSV(r.Context(), analytics.ExportBalanceSheet, day)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition",
				`attachment; filename="balance-sheet-`+day.Format("2006-01-02")+`.csv"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(csv)
			return
		}
		tb, err := fs.Compute(r.Context(), day)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		checks, err := fs.Reconcile(r.Context(), day)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		// JSON view: per-currency A/L/E totals + the open-P&L memo.
		type bs struct {
			Currency    string `json:"currency"`
			Assets      string `json:"assets"`
			Liabilities string `json:"liabilities"`
			Equity      string `json:"equity"`
			NetIncome   string `json:"net_income_unclosed"`
			Balanced    bool   `json:"accounting_equation_ok"`
		}
		out := make([]bs, 0, len(tb.Currencies))
		for _, c := range tb.Currencies {
			out = append(out, bs{
				Currency:    c.Currency,
				Assets:      c.Assets.StringFixed(8),
				Liabilities: c.Liabilities.StringFixed(8),
				Equity:      c.Equity.StringFixed(8),
				NetIncome:   c.NetIncome.StringFixed(8),
				Balanced:    c.AccountingEquationOK,
			})
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"business_date":  tb.Day.Format("2006-01-02"),
			"balance_sheet":  out,
			"reconciliation": checks,
		})
	}
}
