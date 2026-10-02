// Task 14.3.8 — PAMM/MAM internal investment handlers.
//
//	GET  /api/v1/pamm/pools                  ACTIVE pool discovery (keyset ?after=)
//	GET  /api/v1/pamm/pools/{id}             pool detail + totals + my allocation
//	GET  /api/v1/pamm/pools/{id}/statement   caller's sub-ledger movements
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
	"strconv"

	"exchange/internal/gateway"
	"exchange/internal/pamm"
)

// pammService is the handler→engine seam (*pamm.Service).
type pammService interface {
	CreatePool(ctx context.Context, managerAccountID int64,
		name, currency, minInvestment string) (*pamm.Pool, error)
	Invest(ctx context.Context, req pamm.MovementRequest) (*pamm.MovementResult, error)
	Redeem(ctx context.Context, req pamm.MovementRequest) (*pamm.MovementResult, error)
	BrowsePools(ctx context.Context, limit int, afterID int64) ([]pamm.Pool, error)
	PoolDetail(ctx context.Context, poolID, callerAccountID int64) (*pamm.PoolSummary, error)
	PoolStatement(ctx context.Context, poolID, callerAccountID int64,
		limit int, afterID int64) ([]pamm.StatementRow, error)
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

// ---------------------------------------------------------------------------
// Read surface (frontend browse / detail / statement)
// ---------------------------------------------------------------------------

// queryAfterID reads the keyset cursor ?after= (entry/pool id, exclusive
// upper bound — lists run newest-first). Bad cursors are a 400, not a
// silent rewind.
func queryAfterID(r *http.Request) (int64, error) {
	q := r.URL.Query().Get("after")
	if q == "" {
		return 0, nil
	}
	return strconv.ParseInt(q, 10, 64)
}

// PammPoolList serves GET /api/v1/pamm/pools — ACTIVE pool discovery,
// keyset-paginated on ?after=<pool_id>.
func PammPoolList(svc pammService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := claimsAccount(w, r); !ok {
			return
		}
		after, err := queryAfterID(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "?after must be an integer cursor",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		pools, err := svc.BrowsePools(r.Context(), queryLimit(r, 50), after)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"pools": pools})
	}
}

// PammPoolDetail serves GET /api/v1/pamm/pools/{id} — pool row + live
// allocation totals + the caller's own allocation.
func PammPoolDetail(svc pammService) http.HandlerFunc {
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
		sum, err := svc.PoolDetail(r.Context(), poolID, accountID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, sum)
	}
}

// PammStatement serves GET /api/v1/pamm/pools/{id}/statement — the
// caller's sub-ledger movements (the pool manager gets the pool-wide
// ledger). Keyset-paginated on ?after=<entry_id>.
func PammStatement(svc pammService) http.HandlerFunc {
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
		after, err := queryAfterID(r)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "?after must be an integer cursor",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := svc.PoolStatement(r.Context(), poolID, accountID,
			queryLimit(r, 50), after)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"entries": rows})
	}
}
