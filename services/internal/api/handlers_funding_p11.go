// Phase-11 funding handlers — rails+returns cluster (Tasks 11.3.1 and
// 11.3.11). Kept in their own file per the phase's file-map contract;
// they extend — never replace — the Phase-05 funding surface in
// funding.go.
//
//	GET  /api/v1/funding/rails                      capability matrix (client)
//	POST /api/v1/funding/rail-selection             selection preview (client)
//	POST /api/v1/admin/funding/inbound-wires        inbound wire ingest (Finance Ops)
//	GET  /api/v1/admin/funding/quarantine           suspense journal (Compliance/Finance)
//	POST /api/v1/admin/funding/quarantine/{id}/resolve  four-eyes resolution (Finance Ops)
//	POST /api/v1/admin/funding/returns              apply bank return code (Finance Ops)
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// GET /api/v1/funding/rails — capability matrix (Task 11.3.1)
// ---------------------------------------------------------------------------

// railMatrixSource is the matrix read seam (*funding.RailService).
type railMatrixSource interface {
	Capabilities() []funding.RailCapability
}

// FundingRails serves the static rail capability matrix — currencies,
// cut-offs, settlement lag, instant caps — so clients can plan wires.
func FundingRails(src railMatrixSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := claimsAccount(w, r); !ok {
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"rails": src.Capabilities(),
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/funding/rail-selection — preview (Task 11.3.1)
// ---------------------------------------------------------------------------

// railSelector is the selection seam (*funding.RailService.Select).
type railSelector interface {
	Select(ctx context.Context, req funding.SelectionRequest) (*funding.Selection, error)
}

type railSelectionRequest struct {
	Currency       string `json:"currency"`
	Amount         string `json:"amount"`
	PreferredRail  string `json:"preferred_rail,omitempty"`
	RequireSameDay bool   `json:"require_same_day,omitempty"`
}

// FundingRailSelection runs the selection matrix for a hypothetical
// payment — returns the chosen rail + value date, or the fail-closed
// coded error (BANKING_RAIL_UNAVAILABLE 503 / RAIL_CUTOFF_EXCEEDED 422).
func FundingRailSelection(sel railSelector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var req railSelectionRequest
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
		out, err := sel.Select(r.Context(), funding.SelectionRequest{
			AccountID:      accountID,
			Currency:       req.Currency,
			Amount:         amount,
			Preferred:      funding.RailID(strings.ToUpper(strings.TrimSpace(req.PreferredRail))),
			RequireSameDay: req.RequireSameDay,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/funding/inbound-wires — deposit guard (Task 11.3.11)
// ---------------------------------------------------------------------------

// depositScreener is the inbound-wire guard seam (*funding.DepositGuard).
type depositScreener interface {
	ScreenInbound(ctx context.Context, w funding.InboundWire) (*funding.ScreenResult, error)
	ResolveSuspense(ctx context.Context, suspenseID int64,
		action string, investigatorID int64, notes string) (*funding.ResolveOutcome, error)
}

type inboundWireRequest struct {
	BankTxID          string `json:"bank_tx_id"`
	Rail              string `json:"rail"`
	Currency          string `json:"currency"`
	Amount            string `json:"amount"`
	OriginatorName    string `json:"originator_name"`
	OriginatorAccount string `json:"originator_account"`
	OriginatorBIC     string `json:"originator_bic,omitempty"`
	Reference         string `json:"reference,omitempty"`
	RemittanceInfo    string `json:"remittance_info,omitempty"`
}

// AdminInboundWire ingests one bank-side credit notification and runs
// the third-party deposit screen. Name-mismatch wires are quarantined
// and the handler emits the registered 422 — the quarantine state is
// committed before the error surfaces (fail-closed: funds are held,
// never dropped).
func AdminInboundWire(scr depositScreener) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req inboundWireRequest
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
		res, err := scr.ScreenInbound(r.Context(), funding.InboundWire{
			BankTxID:          req.BankTxID,
			Rail:              req.Rail,
			Currency:          req.Currency,
			Amount:            amount,
			OriginatorName:    req.OriginatorName,
			OriginatorAccount: req.OriginatorAccount,
			OriginatorBIC:     req.OriginatorBIC,
			Reference:         req.Reference,
			RemittanceInfo:    req.RemittanceInfo,
			ReceivedAt:        time.Now().UTC(),
		})
		if err != nil {
			// The quarantine committed; surface the coded rejection.
			writeServiceErr(w, r, err)
			return
		}
		status := http.StatusOK
		if res.Disposition == funding.DispositionQuarantined {
			status = http.StatusAccepted // held for investigation, not rejected-as-error
		}
		WriteJSON(w, status, res)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/admin/funding/quarantine — suspense journal (§5.46)
// ---------------------------------------------------------------------------

// suspenseLister is the quarantine read seam (funding.Store.ListSuspense).
type suspenseLister interface {
	ListSuspense(ctx context.Context, f funding.SuspenseFilter) ([]funding.SuspenseRow, int64, error)
}

// AdminQuarantineList pages the suspense/quarantine journal — keyset on
// (quarantined_at, id), newest first. ?status=, ?account_id=, ?cursor=,
// ?limit= (≤200).
func AdminQuarantineList(store suspenseLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		q := r.URL.Query()
		f := funding.SuspenseFilter{Limit: 50}
		if v := strings.TrimSpace(q.Get("status")); v != "" {
			f.Status = strings.ToUpper(v)
		}
		if v := strings.TrimSpace(q.Get("account_id")); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || id <= 0 {
				WriteError(w, "INVALID_REQUEST", "account_id must be a positive integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.AccountID = &id
		}
		if v := strings.TrimSpace(q.Get("limit")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				WriteError(w, "INVALID_REQUEST", "limit must be 1–200",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.Limit = n
		}
		if cur := strings.TrimSpace(q.Get("cursor")); cur != "" {
			c, err := DecodeCursor(cur)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "malformed cursor",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.CursorTS = &c.CreatedAt
			f.CursorID = c.ID
		}
		rows, total, err := store.ListSuspense(r.Context(), f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		var next *string
		if len(rows) > f.Limit {
			last := rows[f.Limit-1]
			c := EncodeCursor(Cursor{CreatedAt: last.QuarantinedAt, ID: last.ID})
			next = &c
			rows = rows[:f.Limit]
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"items":       rows,
			"total":       total,
			"next_cursor": next,
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/funding/quarantine/{id}/resolve — four-eyes (§5.46)
// ---------------------------------------------------------------------------

type quarantineResolveRequest struct {
	Action     string `json:"action"` // RELEASE_TO_CLIENT | RETURN_TO_SOURCE
	ApproverID int64  `json:"approver_id"`
	Notes      string `json:"notes,omitempty"`
}

// AdminQuarantineResolve applies the four-eyes resolution. The resolver
// is the acting admin; approver_id must differ (DUAL_CONTROL_VIOLATION).
func AdminQuarantineResolve(scr depositScreener) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := adminActor(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "suspense id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req quarantineResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if req.ApproverID <= 0 {
			WriteError(w, "DUAL_CONTROL_REQUIRED",
				"approver_id required — quarantine resolution is four-eyes",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if req.ApproverID == actor {
			WriteError(w, "DUAL_CONTROL_VIOLATION",
				"approver must differ from the resolving actor",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := scr.ResolveSuspense(r.Context(), id, req.Action, actor, req.Notes)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/admin/funding/returns — return-code mapping (Task 11.3.11)
// ---------------------------------------------------------------------------

// returnApplier is the rail-return seam (*funding.RailService).
type returnApplier interface {
	ApplyReturn(ctx context.Context, endToEndID, rawCode, reason string) (*funding.ReturnOutcome, error)
}

type railReturnRequest struct {
	EndToEndID string `json:"end_to_end_id"`
	ReturnCode string `json:"return_code"`
	Reason     string `json:"reason,omitempty"`
}

// AdminRailReturn records a bank-side return/reject against a persisted
// rail instruction — the pacs.004/MT199/R-code normalization path.
// Unknown codes quarantine (PENDING_REVIEW) + P1 alert; definitive codes
// release the withdrawal hold via the compensating journal.
func AdminRailReturn(ap returnApplier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminActor(w, r); !ok {
			return
		}
		var req railReturnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if strings.TrimSpace(req.EndToEndID) == "" || strings.TrimSpace(req.ReturnCode) == "" {
			WriteError(w, "INVALID_REQUEST", "end_to_end_id and return_code are required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		out, err := ap.ApplyReturn(r.Context(), req.EndToEndID, req.ReturnCode, req.Reason)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, out)
	}
}
