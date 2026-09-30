// handlers_statements.go — Phase-20 Task 20.3.6 REST surface:
// client statements list/download, trade confirmations, fee invoices.
//
//	GET /api/v1/account/statements                    (registered route)
//	GET /api/v1/account/statements/{id}/download      (handler here;
//	    route row must be registered by the Phase-05 registry owner —
//	    see binding instructions in the task report)
//	GET /api/v1/account/confirmations/{trade_id}      (registered route;
//	    ?format=json|pdf — json returns the confirmation metadata,
//	    pdf streams the stored contract note)
//	GET /api/v1/admin/invoices                        (registered route;
//	    ?account=&month=YYYY-MM&limit=, ?format=csv streams an export)
package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	"exchange/internal/gateway"
)

// statementStore is the narrow handler seam — *analytics.StatementService
// satisfies it; tests substitute a fake.
type statementStore interface {
	List(ctx context.Context, accountID int64, period analytics.StatementPeriod,
		limit int, afterGen *time.Time, afterID int64) ([]analytics.StatementRow, int64, error)
	Fetch(ctx context.Context, accountID, statementID int64, format string) (*analytics.StatementFile, error)
	// DocumentPIN returns the per-account document-open password for
	// encrypted client PDFs (Task 20.3.8); empty when unconfigured.
	DocumentPIN(accountID int64) string
}

// NOTE: GET /api/v1/account/confirmations/{trade_id} is served by
// AccountConfirmation in handlers_confirmations.go — the Task 20.3.8
// delivery/read surface over the Task 20.3.6 generation seam
// (analytics.ConfirmationService writes the trade_confirmations rows).

// invoiceStore — *analytics.InvoiceService.
type invoiceStore interface {
	List(ctx context.Context, accountID int64, month *time.Time, limit int) ([]analytics.Invoice, error)
	FetchFile(ctx context.Context, invoiceID int64, format string) ([]byte, string, error)
}

// AccountStatements — GET /api/v1/account/statements?period=DAILY|MONTHLY
// &limit=&cursor=. Keyset over (generated_at, statement_id) per the §8.8
// envelope convention; the route has no ListSpecs row (registry-owned) so
// the 100/1000 default applies.
func AccountStatements(st statementStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED", "authentication required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		period, err := analytics.ParseStatementPeriod(r.URL.Query().Get("period"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		lp, err := ParseListParams(r, ListSpecFor("/api/v1/account/statements"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var afterGen *time.Time
		var afterID int64
		if lp.Decoded != nil {
			afterGen = &lp.Decoded.CreatedAt
			afterID = lp.Decoded.ID
		}
		rows, total, err := st.List(r.Context(), claims.AccountID, period,
			lp.Limit, afterGen, afterID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		cursors := PageCursors(rows, func(r analytics.StatementRow) (time.Time, int64) {
			return r.GeneratedAt, r.StatementID
		})
		// document_pin lets the portal surface the account's
		// document-open password — emailed PDFs are AES-encrypted and
		// unusable without it.
		WriteJSON(w, http.StatusOK, struct {
			ListEnvelope
			DocumentPIN string `json:"document_pin,omitempty"`
		}{NewListEnvelope(rows, lp, cursors, total), st.DocumentPIN(claims.AccountID)})
	}
}

// AccountStatementDownload — GET /api/v1/account/statements/{id}/download
// ?format=pdf|csv (default pdf). Streams the stored document; a foreign
// statement id is NOT_FOUND (no existence oracle across accounts).
func AccountStatementDownload(st statementStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED", "authentication required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "statement id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		format := strings.ToLower(r.URL.Query().Get("format"))
		f, err := st.Fetch(r.Context(), claims.AccountID, id, format)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		w.Header().Set("Content-Type", f.ContentType)
		ext := ".pdf"
		if strings.HasPrefix(f.ContentType, "text/csv") {
			ext = ".csv"
		}
		w.Header().Set("Content-Disposition",
			`attachment; filename="statement-`+strconv.FormatInt(id, 10)+ext+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.Body)
	}
}

// AdminInvoices — GET /api/v1/admin/invoices?account=&month=YYYY-MM&
// limit=&format=json|csv (Finance Ops role is enforced by the registry
// auth wrapper, not here).
func AdminInvoices(iv invoiceStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var accountID int64
		if raw := q.Get("account"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				WriteError(w, "INVALID_REQUEST", "account must be a positive integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			accountID = id
		}
		var month *time.Time
		if raw := q.Get("month"); raw != "" {
			m, err := time.Parse("2006-01", raw)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "month must be YYYY-MM",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			month = &m
		}
		limit := parseLimitQuery(q.Get("limit"), 100, 1000)
		invs, err := iv.List(r.Context(), accountID, month, limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if strings.EqualFold(q.Get("format"), "csv") {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition",
				`attachment; filename="fee-invoices.csv"`)
			w.WriteHeader(http.StatusOK)
			var b strings.Builder
			b.WriteString("invoice_id,account_id,month,currency,trading_fees,mm_rebates,connectivity_fees,total,status,generated_at,file_ref\n")
			for _, inv := range invs {
				b.WriteString(strings.Join([]string{
					strconv.FormatInt(inv.InvoiceID, 10),
					strconv.FormatInt(inv.AccountID, 10),
					inv.Month.Format("2006-01"),
					inv.Currency,
					inv.TradingFees.StringFixed(8),
					inv.MMRebates.StringFixed(8),
					inv.ConnectivityFees.StringFixed(8),
					inv.Total.StringFixed(8),
					inv.Status,
					inv.GeneratedAt.UTC().Format(time.RFC3339),
					inv.FileRef,
				}, ",") + "\n")
			}
			_, _ = w.Write([]byte(b.String()))
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"invoices": invs})
	}
}
