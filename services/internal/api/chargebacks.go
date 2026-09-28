// Task 5.3.18 — Chargeback Handling (admin surface, Finance Ops role).
//
//	POST /api/v1/admin/chargebacks                 open dispute (+ optional freeze)
//	GET  /api/v1/admin/chargebacks                 list disputes (cursor-paged)
//	GET  /api/v1/admin/chargebacks/{id}            dispute + evidence bundle
//	POST /api/v1/admin/chargebacks/{id}/submit     EVIDENCE_COLLECTED → SUBMITTED
//	POST /api/v1/admin/chargebacks/{id}/resolve    SUBMITTED → RESOLVED_WON|LOST
//
// Admin access requires the "admin" scope (same enforcement as the IP-ban
// surface); the Finance-Ops role refinement lands with Phase-07 RBAC —
// the route metadata already declares it.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
)

// chargebackService is the lifecycle seam (*funding.ChargebackService).
type chargebackService interface {
	Create(ctx context.Context, adminID int64, req funding.CreateChargebackRequest) (*funding.CreateChargebackResult, error)
	Detail(ctx context.Context, id int64) (*funding.ChargebackDetail, error)
	List(ctx context.Context, f funding.ChargebackFilter) ([]funding.ChargebackRow, string, int64, error)
	Submit(ctx context.Context, adminID, id int64, clientIP string) (*funding.ChargebackRow, error)
	Resolve(ctx context.Context, adminID, id int64, outcome, note, clientIP string) (*funding.ChargebackRow, error)
}

type createChargebackBody struct {
	AccountID            int64  `json:"account_id"`
	FundingTransactionID *int64 `json:"funding_transaction_id"`
	CardNetwork          string `json:"card_network"`
	Currency             string `json:"currency"`
	Amount               string `json:"amount"`
	Reason               string `json:"reason"`
	FreezeAccount        bool   `json:"freeze_account"`
	ApproverUserID       int64  `json:"approver_user_id"`
}

// AdminChargebackCreate serves POST /api/v1/admin/chargebacks.
func AdminChargebackCreate(svc chargebackService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		adminID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		var body createChargebackBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Create(r.Context(), adminID, funding.CreateChargebackRequest{
			AccountID:            body.AccountID,
			FundingTransactionID: body.FundingTransactionID,
			CardNetwork:          body.CardNetwork,
			Currency:             body.Currency,
			Amount:               body.Amount,
			Reason:               body.Reason,
			FreezeAccount:        body.FreezeAccount,
			ApproverUserID:       body.ApproverUserID,
			ClientIP:             middleware.ClientIP(r, false),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, res)
	}
}

// AdminChargebackList serves GET /api/v1/admin/chargebacks — the dispute
// journal paged on (opened_at, id) with optional account_id/status filters.
func AdminChargebackList(svc chargebackService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		q := r.URL.Query()
		f := funding.ChargebackFilter{
			Status: q.Get("status"),
			Limit:  parseLimitQuery(q.Get("limit"), 50, 500),
		}
		if raw := q.Get("account_id"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 {
				WriteError(w, "INVALID_REQUEST", "invalid account_id",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.AccountID = &n
		}
		if cur := q.Get("cursor"); cur != "" {
			ts, id, err := funding.DecodeCursor(cur)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			f.CursorTS = &ts
			f.CursorID = id
		}
		rows, next, total, err := svc.List(r.Context(), f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []funding.ChargebackRow{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"data":        rows,
			"next_cursor": next,
			"limit":       f.Limit,
			"total":       total,
		})
	}
}

// AdminChargebackDetail serves GET /api/v1/admin/chargebacks/{id} — the
// dispute plus its sha256-hashed evidence bundle.
func AdminChargebackDetail(svc chargebackService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "invalid chargeback id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		d, err := svc.Detail(r.Context(), id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, d)
	}
}

// AdminChargebackSubmit serves POST /api/v1/admin/chargebacks/{id}/submit.
func AdminChargebackSubmit(svc chargebackService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "invalid chargeback id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		adminID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		cb, err := svc.Submit(r.Context(), adminID, id, middleware.ClientIP(r, false))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"chargeback": cb})
	}
}

type resolveChargebackBody struct {
	Outcome string `json:"outcome"` // WON | LOST
	Note    string `json:"note"`
}

// AdminChargebackResolve serves POST /api/v1/admin/chargebacks/{id}/resolve.
func AdminChargebackResolve(svc chargebackService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "invalid chargeback id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body resolveChargebackBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		adminID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		cb, err := svc.Resolve(r.Context(), adminID, id, body.Outcome, body.Note,
			middleware.ClientIP(r, false))
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"chargeback": cb})
	}
}
