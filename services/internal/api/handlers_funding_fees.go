// Phase-11 Task 11.3.9 — funding fee REST surface.
//
//	POST   /api/v1/funding/fee-estimate                  scheduled fee preview (client)
//	GET    /api/v1/admin/funding/fees                    schedule list (Finance Ops)
//	POST   /api/v1/admin/funding/fees                    create schedule version 1
//	GET    /api/v1/admin/funding/fees/{id}               one schedule row
//	PUT    /api/v1/admin/funding/fees/{id}               successor version insert
//	DELETE /api/v1/admin/funding/fees/{id}               retire (leaves resolution)
//	GET    /api/v1/admin/funding/fees/{id}/versions      full version chain
//
// The client estimate resolves the caller's KYC tier from the account
// store and never consumes the monthly free allowance. The admin CRUD
// is Finance Ops-gated inside funding.FeeScheduleService (defense in
// depth on the route's role pin); every mutation commits the schedule
// row + admin_audit_log + §5.8 hash-chain link in one transaction.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// POST /api/v1/funding/fee-estimate — client preview (read-only)
// ---------------------------------------------------------------------------

// feeEstimator is the estimate seam (*funding.FeeService).
type feeEstimator interface {
	Estimate(ctx context.Context, req funding.FeeEstimateRequest) (*funding.FeeEstimate, error)
}

type feeEstimateRequest struct {
	Rail      string `json:"rail"`
	Currency  string `json:"currency"`
	Direction string `json:"direction"`
	Amount    string `json:"amount"`
}

// FundingFeeEstimate serves the fee preview: rail/currency/direction +
// amount in, scheduled fee + free-tier status + net amount out. Coded
// failures stay coded — FEE_TIER_NOT_FOUND when no schedule resolves,
// FUNDING_FEE_EXCEEDS_AMOUNT when the fee would consume the transfer.
func FundingFeeEstimate(svc feeEstimator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var req feeEstimateRequest
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
		est, err := svc.Estimate(r.Context(), funding.FeeEstimateRequest{
			AccountID: accountID,
			Rail:      req.Rail,
			Currency:  req.Currency,
			Direction: req.Direction,
			Amount:    amount,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, est)
	}
}

// ---------------------------------------------------------------------------
// Admin schedule CRUD — Finance Ops gate inside the service.
// ---------------------------------------------------------------------------

// feeScheduleAdmin is the admin-service seam (*funding.FeeScheduleService).
type feeScheduleAdmin interface {
	List(ctx context.Context, actor funding.FeeAdminActor, f funding.FeeTierFilter) ([]funding.FundingFeeTier, error)
	Get(ctx context.Context, actor funding.FeeAdminActor, id int64) (*funding.FundingFeeTier, error)
	Versions(ctx context.Context, actor funding.FeeAdminActor, id int64) ([]funding.FundingFeeTier, error)
	Create(ctx context.Context, actor funding.FeeAdminActor, req funding.FeeTierCreate) (*funding.FundingFeeTier, error)
	Update(ctx context.Context, actor funding.FeeAdminActor, id int64, req funding.FeeTierUpdate) (*funding.FundingFeeTier, error)
	Retire(ctx context.Context, actor funding.FeeAdminActor, id int64) (*funding.FundingFeeTier, error)
}

// feeAdminActor resolves the authenticated admin into the funding-side
// actor shape (same source as adminActorFrom — Bearer claims + client IP).
func feeAdminActor(r *http.Request, trustProxy bool) (funding.FeeAdminActor, bool) {
	a, err := adminActorFrom(r, trustProxy)
	if err != nil {
		return funding.FeeAdminActor{}, false
	}
	return funding.FeeAdminActor{UserID: a.UserID, ClientIP: a.ClientIP}, true
}

// AdminFundingFeeList serves GET /api/v1/admin/funding/fees with the
// ?rail=&currency=&direction=&tier=&all=&limit= filters.
func AdminFundingFeeList(svc feeScheduleAdmin, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := feeAdminActor(r, trustProxy)
		if !ok {
			WriteError(w, "UNAUTHORIZED", "admin identity required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		f := funding.FeeTierFilter{
			Rail:       strings.ToUpper(strings.TrimSpace(q.Get("rail"))),
			Currency:   strings.ToUpper(strings.TrimSpace(q.Get("currency"))),
			Direction:  strings.ToUpper(strings.TrimSpace(q.Get("direction"))),
			Tier:       strings.ToUpper(strings.TrimSpace(q.Get("tier"))),
			IncludeAll: q.Get("all") == "1" || q.Get("all") == "true",
			Limit:      parseLimitQuery(q.Get("limit"), 200, 500),
		}
		rows, err := svc.List(r.Context(), actor, f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"items": rows, "count": len(rows),
		})
	}
}

// AdminFundingFeeCreate serves POST /api/v1/admin/funding/fees — inserts
// version 1 of a (rail, currency, direction, account_tier) schedule.
func AdminFundingFeeCreate(svc feeScheduleAdmin, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := feeAdminActor(r, trustProxy)
		if !ok {
			WriteError(w, "UNAUTHORIZED", "admin identity required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req funding.FeeTierCreate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := svc.Create(r.Context(), actor, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, t)
	}
}

// fundingFeePathID parses the {id} segment.
func fundingFeePathID(r *http.Request) (int64, bool) {
	return parsePathID(r.PathValue("id"))
}

// AdminFundingFeeGet serves GET /api/v1/admin/funding/fees/{id}.
func AdminFundingFeeGet(svc feeScheduleAdmin, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := feeAdminActor(r, trustProxy)
		if !ok {
			WriteError(w, "UNAUTHORIZED", "admin identity required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, ok := fundingFeePathID(r)
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := svc.Get(r.Context(), actor, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, t)
	}
}

// AdminFundingFeeUpdate serves PUT /api/v1/admin/funding/fees/{id} —
// inserts the successor version (supersedes_id = {id}); the prior row
// stays on the version chain.
func AdminFundingFeeUpdate(svc feeScheduleAdmin, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := feeAdminActor(r, trustProxy)
		if !ok {
			WriteError(w, "UNAUTHORIZED", "admin identity required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, ok := fundingFeePathID(r)
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var req funding.FeeTierUpdate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := svc.Update(r.Context(), actor, id, req)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, t)
	}
}

// AdminFundingFeeRetire serves DELETE /api/v1/admin/funding/fees/{id} —
// retired rows leave resolution but stay on record for audit.
func AdminFundingFeeRetire(svc feeScheduleAdmin, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := feeAdminActor(r, trustProxy)
		if !ok {
			WriteError(w, "UNAUTHORIZED", "admin identity required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, ok := fundingFeePathID(r)
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		t, err := svc.Retire(r.Context(), actor, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, t)
	}
}

// AdminFundingFeeVersions serves GET …/{id}/versions — every version of
// the row's schedule group, newest first.
func AdminFundingFeeVersions(svc feeScheduleAdmin, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := feeAdminActor(r, trustProxy)
		if !ok {
			WriteError(w, "UNAUTHORIZED", "admin identity required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		id, ok := fundingFeePathID(r)
		if !ok {
			WriteError(w, "INVALID_REQUEST", "id must be a positive integer",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, err := svc.Versions(r.Context(), actor, id)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"items": rows, "count": len(rows),
		})
	}
}
