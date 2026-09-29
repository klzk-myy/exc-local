// Task 14.3.8 — PAMM/MAM internal investment handlers.
//
//	POST /api/v1/pamm/pools                  manager creates a pool
//	POST /api/v1/pamm/pools/{id}/invest      PAMM_INVEST  — investor → pool
//	POST /api/v1/pamm/pools/{id}/redeem      PAMM_REDEEM  — pool → investor
//
// Both movements are dedicated internal investment transactions: they
// post TRANSFER journals between customer (2010) and pooled-investment
// (2170) liabilities and NEVER create funding_transactions nor touch
// daily fiat withdrawal/KYC caps. Amounts are decimal strings.
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"exchange/internal/gateway"
	"exchange/internal/pamm"
)

// pammService is the handler→engine seam (*pamm.Service).
type pammService interface {
	CreatePool(ctx context.Context, managerAccountID int64,
		name, currency, minInvestment string) (*pamm.Pool, error)
	Invest(ctx context.Context, req pamm.MovementRequest) (*pamm.MovementResult, error)
	Redeem(ctx context.Context, req pamm.MovementRequest) (*pamm.MovementResult, error)
}

type createPoolBody struct {
	Name          string `json:"name"`
	Currency      string `json:"currency"`
	MinInvestment string `json:"min_investment"`
}

// PammPoolCreate serves POST /api/v1/pamm/pools — the caller's account
// becomes the pool manager; the pool account is created as its
// sub-account and is structurally barred from fiat rails.
func PammPoolCreate(svc pammService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body createPoolBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := svc.CreatePool(r.Context(), accountID, body.Name,
			body.Currency, body.MinInvestment)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, p)
	}
}

type movementBody struct {
	Amount string `json:"amount"`
}

// PammInvest serves POST /api/v1/pamm/pools/{id}/invest — commits the
// caller's capital to the pool (PAMM_INVEST). Idempotency-Key honored.
func PammInvest(svc pammService) http.HandlerFunc {
	return pammMovement(svc.Invest)
}

// PammRedeem serves POST /api/v1/pamm/pools/{id}/redeem — returns
// invested capital to the caller (PAMM_REDEEM).
func PammRedeem(svc pammService) http.HandlerFunc {
	return pammMovement(svc.Redeem)
}

func pammMovement(fn func(context.Context, pamm.MovementRequest) (*pamm.MovementResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		poolID, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body movementBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		res, err := fn(r.Context(), pamm.MovementRequest{
			PoolID:            poolID,
			InvestorAccountID: accountID,
			Amount:            body.Amount,
			IdempotencyKey:    r.Header.Get("Idempotency-Key"),
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}
