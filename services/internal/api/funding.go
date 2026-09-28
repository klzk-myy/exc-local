// Task 5.3.6 — Funding Endpoints.
//
//	GET  /api/v1/deposits/{currency}         deposit instructions (nostro)
//	POST /api/v1/withdrawals                 create withdrawal + 15min window
//	POST /api/v1/withdrawals/{id}/confirm    token confirm → tiered review
//	GET  /api/v1/funding                     funding history (cursor-paged)
//
// All state transitions run through funding.WithdrawalService →
// settlement.LedgerService; handlers do auth + parse + envelope only.
// Idempotency-Key is honored on the money-moving POST (account-scoped,
// spec §8.8); the "required" gate lands with Task 5.3.42 middleware.
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

// ---------------------------------------------------------------------------
// GET /api/v1/deposits/{currency}
// ---------------------------------------------------------------------------

// nostroSource is the deposit-instruction read seam.
type nostroSource interface {
	NostroAccounts(ctx context.Context, currency string) ([]funding.NostroAccount, error)
}

// DepositInstructions serves the per-currency nostro/bank instructions
// (AC #16). The payment reference is account-scoped so the reconciliation
// engine can attribute incoming wires.
func DepositInstructions(src nostroSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		ccy := strings.ToUpper(strings.TrimSpace(r.PathValue("currency")))
		if len(ccy) != 3 {
			WriteError(w, "INVALID_REQUEST", "currency must be a 3-letter ISO code",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accts, err := src.NostroAccounts(r.Context(), ccy)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if len(accts) == 0 {
			WriteError(w, "NOT_FOUND",
				"no deposit instructions for currency "+ccy,
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"currency":   ccy,
			"account_id": accountID,
			// Stable, account-scoped wire reference — quote it verbatim on
			// the bank transfer so settlement reconciliation attributes it.
			"reference":    funding.DepositReference(accountID, ccy),
			"instructions": accts,
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/withdrawals
// ---------------------------------------------------------------------------

// withdrawalService is the create/confirm seam the handlers call.
type withdrawalService interface {
	Create(ctx context.Context, req funding.CreateWithdrawalRequest) (*funding.WithdrawalResult, error)
	Confirm(ctx context.Context, req funding.ConfirmWithdrawalRequest) (*funding.WithdrawalResult, error)
}

type createWithdrawalBody struct {
	Currency         string `json:"currency"`
	Amount           string `json:"amount"`
	ReferenceAccount string `json:"reference_account"`
	BankMethod       string `json:"bank_method"`
	ConfirmMethod    string `json:"confirm_method"`
}

// CreateWithdrawal serves POST /api/v1/withdrawals (AC #17 — 15-minute
// confirmation window minted at create). The confirm token is returned
// once in the response; only its sha256 is persisted.
func CreateWithdrawal(svc withdrawalService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body createWithdrawalBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		userID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		res, err := svc.Create(r.Context(), funding.CreateWithdrawalRequest{
			AccountID:        accountID,
			UserID:           userID,
			Currency:         body.Currency,
			Amount:           body.Amount,
			ReferenceAccount: body.ReferenceAccount,
			BankMethod:       body.BankMethod,
			ConfirmMethod:    body.ConfirmMethod,
			IdempotencyKey:   r.Header.Get("Idempotency-Key"),
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, res)
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/withdrawals/{id}/confirm
// ---------------------------------------------------------------------------

type confirmWithdrawalBody struct {
	Token  string `json:"token"`
	Method string `json:"method"`
}

// ConfirmWithdrawal serves the token-confirm path (AC #18): a valid token
// inside the 15-minute window transitions PENDING → CONFIRMED or
// PENDING_REVIEW per the canonical tier; a late token auto-cancels the
// withdrawal and releases the hold.
func ConfirmWithdrawal(svc withdrawalService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST", "invalid withdrawal id",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body confirmWithdrawalBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		userID, _ := strconv.ParseInt(claims.Subject, 10, 64)
		res, err := svc.Confirm(r.Context(), funding.ConfirmWithdrawalRequest{
			WithdrawalID: id,
			AccountID:    accountID,
			UserID:       userID,
			Token:        body.Token,
			Method:       body.Method,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/funding — funding history (cursor-paged)
// ---------------------------------------------------------------------------

// FundingHistory serves the account's funding journal with the §8.8
// envelope {data, next_cursor, limit, total}. Filters: type, currency,
// status, from/to (RFC3339), cursor, limit (default 50, max 500).
func FundingHistory(svc fundingHistoryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		if rejectForeignAccount(w, r, claims, q.Get("account_id")) {
			return
		}
		f := funding.FundingFilter{
			Type:     strings.ToUpper(q.Get("type")),
			Currency: strings.ToUpper(q.Get("currency")),
			Status:   strings.ToUpper(q.Get("status")),
			Limit:    parseLimitQuery(q.Get("limit"), 50, 500),
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
		rows, next, total, err := svc.Funding(r.Context(), accountID, f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []funding.FundingTxRow{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"data":        rows,
			"next_cursor": next,
			"limit":       f.Limit,
			"total":       total,
		})
	}
}

// fundingHistoryService is the history seam (*funding.HistoryService).
type fundingHistoryService interface {
	Funding(ctx context.Context, accountID int64, f funding.FundingFilter) ([]funding.FundingTxRow, string, int64, error)
}
