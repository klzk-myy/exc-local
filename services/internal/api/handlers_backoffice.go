// Phase-24 backoffice admin surface — Tasks 24.3.1–24.3.5.
//
//	GET  /api/v1/admin/nostro-accounts            registry list (Finance Ops+)
//	POST /api/v1/admin/nostro-accounts            create account (Finance Ops+)
//	GET  /api/v1/admin/nostro-reconciliation      daily recon report (Read-Only Auditor)
//	POST /api/v1/admin/nostro-reconciliation/run  on-demand recon run (Finance Ops+)
//	POST /api/v1/admin/nostro-reconciliation/breaks/{id}/resolve  break workflow (Finance Ops+)
//	GET  /api/v1/admin/swift-messages             immutable SWIFT journal (Finance Ops+)
//	POST /api/v1/admin/settlement-confirmations   MT900/910 ingest (Finance Ops+)
//	GET  /api/v1/admin/compliance-report          compliance export (Compliance Officer)
//
// Role gates ride the route registry; handlers resolve identity via
// adminActor and fail closed (nil service → SERVICE_DEGRADED 503,
// never empty-success).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/backoffice"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// nostro-accounts — Task 24.3.1
// ---------------------------------------------------------------------------

// nostroAccountAdmin is the account-registry seam (*backoffice.NostroService).
type nostroAccountAdmin interface {
	CreateAccount(ctx context.Context, in backoffice.CreateAccountInput) (*backoffice.NostroAccount, error)
	ListAccounts(ctx context.Context, f backoffice.AccountFilter) ([]backoffice.NostroAccount, error)
}

type nostroAccountRequest struct {
	Currency      string `json:"currency"`
	BankName      string `json:"bank_name"`
	BankCode      string `json:"bank_code,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	IBAN          string `json:"iban,omitempty"`
	Role          string `json:"role,omitempty"` // NOSTRO|VOSTRO (default NOSTRO)
}

// AdminNostroAccountCreate serves POST /api/v1/admin/nostro-accounts.
func AdminNostroAccountCreate(svc nostroAccountAdmin) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "nostro service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req nostroAccountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		acct, err := svc.CreateAccount(r.Context(), backoffice.CreateAccountInput{
			Currency:      strings.ToUpper(strings.TrimSpace(req.Currency)),
			BankName:      strings.TrimSpace(req.BankName),
			BankCode:      strings.ToUpper(strings.TrimSpace(req.BankCode)),
			AccountNumber: strings.TrimSpace(req.AccountNumber),
			IBAN:          strings.ToUpper(strings.TrimSpace(req.IBAN)),
			Role:          backoffice.AccountRole(strings.ToUpper(strings.TrimSpace(req.Role))),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"account": acct})
	}
}

// AdminNostroAccountList serves GET /api/v1/admin/nostro-accounts —
// ?currency=&role=&status= filters, balances included.
func AdminNostroAccountList(svc nostroAccountAdmin) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "nostro service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		q := r.URL.Query()
		f := backoffice.AccountFilter{
			Currency: strings.ToUpper(strings.TrimSpace(q.Get("currency"))),
			Role:     backoffice.AccountRole(strings.ToUpper(strings.TrimSpace(q.Get("role")))),
			Status:   strings.ToUpper(strings.TrimSpace(q.Get("status"))),
		}
		if f.Role != "" && f.Role != backoffice.RoleNostro && f.Role != backoffice.RoleVostro {
			WriteError(w, "INVALID_REQUEST", "role must be NOSTRO or VOSTRO",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := svc.ListAccounts(r.Context(), f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"accounts": rows})
	}
}

// ---------------------------------------------------------------------------
// nostro-reconciliation — Task 24.3.2
// ---------------------------------------------------------------------------

// nostroRecon is the reconciliation seam (*backoffice.NostroReconService).
type nostroRecon interface {
	Report(ctx context.Context, date time.Time) (*backoffice.NostroReconReport, error)
	RunDaily(ctx context.Context, date time.Time, accountID *int64) (*backoffice.NostroReconReport, error)
	AssignBreak(ctx context.Context, breakID, actor int64) (*backoffice.NostroReconBreak, error)
	ResolveBreak(ctx context.Context, breakID int64, notes string, actor int64) (*backoffice.NostroReconBreak, error)
}

// parseReconDate accepts ?date=YYYY-MM-DD (default today, UTC).
func parseReconDate(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	q := strings.TrimSpace(r.URL.Query().Get("date"))
	if q == "" {
		return time.Now().UTC().Truncate(24 * time.Hour), true
	}
	t, err := time.Parse("2006-01-02", q)
	if err != nil {
		WriteError(w, "INVALID_REQUEST", "date must be YYYY-MM-DD",
			gateway.RequestIDFrom(r.Context()), nil)
		return time.Time{}, false
	}
	return t.UTC(), true
}

// AdminNostroReconciliation serves GET /api/v1/admin/nostro-reconciliation?date=.
func AdminNostroReconciliation(svc nostroRecon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "nostro recon service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		day, ok := parseReconDate(w, r)
		if !ok {
			return
		}
		rep, err := svc.Report(r.Context(), day)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, rep)
	}
}

// AdminNostroReconRun serves POST /api/v1/admin/nostro-reconciliation/run —
// the on-demand daily pass ({date?, account_id?}).
func AdminNostroReconRun(svc nostroRecon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "nostro recon service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var body struct {
			Date      string `json:"date"`
			AccountID *int64 `json:"account_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		day := time.Now().UTC().Truncate(24 * time.Hour)
		if body.Date != "" {
			t, err := time.Parse("2006-01-02", body.Date)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "date must be YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			day = t.UTC()
		}
		rep, err := svc.RunDaily(r.Context(), day, body.AccountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, rep)
	}
}

// AdminNostroBreakResolve serves POST
// /api/v1/admin/nostro-reconciliation/breaks/{id}/resolve — the break
// investigation workflow ({action: investigate|resolve, notes?}).
func AdminNostroBreakResolve(svc nostroRecon) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "nostro recon service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		breakID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || breakID <= 0 {
			WriteError(w, "INVALID_REQUEST", "break id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Action string `json:"action"` // INVESTIGATE | RESOLVE
			Notes  string `json:"notes,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var brk *backoffice.NostroReconBreak
		switch strings.ToUpper(strings.TrimSpace(body.Action)) {
		case "INVESTIGATE":
			brk, err = svc.AssignBreak(r.Context(), breakID, actor)
		case "RESOLVE":
			brk, err = svc.ResolveBreak(r.Context(), breakID, body.Notes, actor)
		default:
			WriteError(w, "INVALID_REQUEST", "action must be INVESTIGATE or RESOLVE",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"break": brk})
	}
}

// ---------------------------------------------------------------------------
// swift-messages — Task 24.3.4
// ---------------------------------------------------------------------------

// swiftLister is the journal read seam (*backoffice.SwiftTracker).
type swiftLister interface {
	Query(ctx context.Context, f backoffice.SwiftFilter) ([]backoffice.SwiftMessage, error)
}

// AdminSwiftMessages serves GET /api/v1/admin/swift-messages?from=&to=&type=&direction=
// over the immutable journal — keyset-paginated on (created_at,id).
func AdminSwiftMessages(svc swiftLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "swift tracker unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		q := r.URL.Query()
		f := backoffice.SwiftFilter{Limit: 200}
		if v := strings.TrimSpace(q.Get("from")); v != "" {
			t, perr := time.Parse(time.RFC3339, v)
			if perr != nil {
				if d, derr := time.Parse("2006-01-02", v); derr == nil {
					t = d.UTC()
				} else {
					WriteError(w, "INVALID_REQUEST", "from must be RFC3339 or YYYY-MM-DD",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
			}
			f.From = &t
		}
		if v := strings.TrimSpace(q.Get("to")); v != "" {
			t, perr := time.Parse(time.RFC3339, v)
			if perr != nil {
				if d, derr := time.Parse("2006-01-02", v); derr == nil {
					t = d.UTC().Add(24 * time.Hour) // inclusive day
				} else {
					WriteError(w, "INVALID_REQUEST", "to must be RFC3339 or YYYY-MM-DD",
						gateway.RequestIDFrom(r.Context()), nil)
					return
				}
			}
			f.To = &t
		}
		f.Type = strings.TrimSpace(q.Get("type"))
		f.Direction = strings.TrimSpace(q.Get("direction"))
		if v := strings.TrimSpace(q.Get("limit")); v != "" {
			n, perr := strconv.Atoi(v)
			if perr != nil || n < 1 || n > 500 {
				WriteError(w, "INVALID_REQUEST", "limit must be 1–500",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.Limit = n
		}
		if cur := strings.TrimSpace(q.Get("cursor")); cur != "" {
			c, perr := DecodeCursor(cur)
			if perr != nil {
				WriteError(w, "INVALID_REQUEST", "malformed cursor",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.CursorTS = &c.CreatedAt
			f.CursorID = &c.ID
		}
		rows, err := svc.Query(r.Context(), f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var next *string
		if len(rows) > 0 {
			last := rows[len(rows)-1]
			c := EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
			next = &c
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"messages": rows, "next_cursor": next})
	}
}

// ---------------------------------------------------------------------------
// settlement-confirmations — Task 24.3.3 (MT900/MT910 ingest)
// ---------------------------------------------------------------------------

// settlementConfirmer is the confirmation seam (*backoffice.ConfirmationService).
type settlementConfirmer interface {
	ProcessConfirmation(ctx context.Context, c backoffice.SwiftConfirmation) (*backoffice.ConfirmationResult, error)
}

type swiftConfirmationRequest struct {
	MessageType string `json:"message_type"` // MT900 | MT910
	Reference   string `json:"reference"`
	RelatedRef  string `json:"related_reference"`
	Currency    string `json:"currency"`
	Amount      string `json:"amount"`
	ValueDate   string `json:"value_date"` // YYYY-MM-DD
	NostroIBAN  string `json:"nostro_iban,omitempty"`
	RawPayload  string `json:"raw_payload,omitempty"`
}

// AdminSettlementConfirmation serves POST /api/v1/admin/settlement-confirmations —
// records + applies one correspondent MT900/MT910 (manual ops intake;
// the automated rail adapter feeds the same seam).
func AdminSettlementConfirmation(svc settlementConfirmer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "confirmation service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req swiftConfirmationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
		if err != nil || !amount.IsPositive() {
			WriteError(w, "INVALID_REQUEST", "amount must be a positive decimal",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var vd time.Time
		if v := strings.TrimSpace(req.ValueDate); v != "" {
			if t, perr := time.Parse("2006-01-02", v); perr == nil {
				vd = t.UTC()
			} else {
				WriteError(w, "INVALID_REQUEST", "value_date must be YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		res, err := svc.ProcessConfirmation(r.Context(), backoffice.SwiftConfirmation{
			MessageType: strings.ToUpper(strings.TrimSpace(req.MessageType)),
			Reference:   strings.TrimSpace(req.Reference),
			RelatedRef:  strings.TrimSpace(req.RelatedRef),
			Currency:    strings.ToUpper(strings.TrimSpace(req.Currency)),
			Amount:      amount,
			ValueDate:   vd,
			NostroIBAN:  strings.ToUpper(strings.TrimSpace(req.NostroIBAN)),
			ReceivedAt:  time.Now().UTC(),
			RawPayload:  req.RawPayload,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// compliance-report — Task 24.3.5
// ---------------------------------------------------------------------------

// complianceExporter is the export seam (*backoffice.ComplianceReportService).
type complianceExporter interface {
	Export(ctx context.Context, req backoffice.ReportRequest) (*backoffice.ComplianceExport, error)
}

// AdminComplianceReport serves GET /api/v1/admin/compliance-report?type=&from=&to=
// — the regulator export surface consuming the Phase-21 stores.
func AdminComplianceReport(svc complianceExporter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "compliance report service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if _, ok := adminActor(w, r); !ok {
			return
		}
		q := r.URL.Query()
		req := backoffice.ReportRequest{
			Type: strings.TrimSpace(q.Get("type")),
		}
		if req.Type == "" {
			WriteError(w, "INVALID_REQUEST", "type is required (MIFID2|EMIR|FINCEN_CTR|FINCEN_SAR|BASEL3|MONTHLY_SUMMARY)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if v := strings.TrimSpace(q.Get("from")); v != "" {
			t, err := parseReportTime(v)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "from must be RFC3339 or YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			req.From = t
		}
		if v := strings.TrimSpace(q.Get("to")); v != "" {
			t, err := parseReportTime(v)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "to must be RFC3339 or YYYY-MM-DD",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			req.To = t
		}
		if v := strings.TrimSpace(q.Get("limit")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 5000 {
				WriteError(w, "INVALID_REQUEST", "limit must be 1–5000",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			req.Limit = n
		}
		out, err := svc.Export(r.Context(), req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// parseReportTime accepts RFC3339 or YYYY-MM-DD.
func parseReportTime(v string) (*time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		u := t.UTC()
		return &u, nil
	}
	return nil, strconv.ErrSyntax
}
