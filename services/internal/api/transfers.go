// Task 5.3.23 — Internal Transfers + Task 5.3.45 — Transfer History.
//
//	POST /api/v1/transfers  same-user / master↔sub money movement
//	GET  /api/v1/transfers  cursor-paged journal (currency, direction,
//	                        date range, sub-account filters)
//
// The POST enforces ownership, mutability and balance inside
// funding.TransferService; the ledger posting (TRANSFER entry type) is
// the only balance mutation. Idempotency-Key is honored account-scoped.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"exchange/internal/funding"
	"exchange/internal/gateway"
)

// transferService is the create/history seam (*funding.TransferService +
// *funding.HistoryService share the backing store).
type transferService interface {
	Create(ctx context.Context, req funding.CreateTransferRequest) (*funding.TransferResult, error)
}

type transferHistoryService interface {
	Transfers(ctx context.Context, accountID int64, f funding.TransferFilter) ([]funding.TransferRow, string, int64, error)
}

type createTransferBody struct {
	FromAccountID int64  `json:"from_account_id"`
	ToAccountID   int64  `json:"to_account_id"`
	Currency      string `json:"currency"`
	Amount        string `json:"amount"`
}

// CreateTransfer serves POST /api/v1/transfers (AC #43/#44): atomic
// ledger move between caller-owned accounts; cross-user, FROZEN and
// SUSPENDED endpoints reject.
func CreateTransfer(svc transferService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body createTransferBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := svc.Create(r.Context(), funding.CreateTransferRequest{
			CallerAccountID: accountID,
			FromAccountID:   body.FromAccountID,
			ToAccountID:     body.ToAccountID,
			Currency:        body.Currency,
			Amount:          body.Amount,
			IdempotencyKey:  r.Header.Get("Idempotency-Key"),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, res)
	}
}

// TransferHistory serves GET /api/v1/transfers (Task 5.3.45, §24 #363):
// cursor-paged journal with source, destination, amount, GL reference
// (journal_entry_id), status and actor on every row. Filters: currency,
// direction (in|out), from/to (RFC3339), sub_account_id.
func TransferHistory(svc transferHistoryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		f := funding.TransferFilter{
			Currency:  strings.ToUpper(q.Get("currency")),
			Direction: strings.ToUpper(q.Get("direction")),
			Limit:     parseLimitQuery(q.Get("limit"), 50, 500),
		}
		if raw := q.Get("sub_account_id"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 {
				WriteError(w, "INVALID_REQUEST", "invalid sub_account_id",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			f.PerspectiveID = n
		}
		if t, err := parseTimeQuery(q.Get("from")); err != nil {
			WriteError(w, "INVALID_REQUEST", "invalid 'from' timestamp (RFC3339)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		} else {
			f.From = t
		}
		if t, err := parseTimeQuery(q.Get("to")); err != nil {
			WriteError(w, "INVALID_REQUEST", "invalid 'to' timestamp (RFC3339)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		} else {
			f.To = t
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
		rows, next, total, err := svc.Transfers(r.Context(), accountID, f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []funding.TransferRow{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"data":        rows,
			"next_cursor": next,
			"limit":       f.Limit,
			"total":       total,
		})
	}
}
