// Task 14.3.14 — Copy-Trading product handlers.
//
//	GET    /api/v1/copy/strategies                     discovery (public, computed-only)
//	POST   /api/v1/copy/strategies                     manager creates INCUBATING profile
//	POST   /api/v1/copy/strategies/{id}/list           request LISTED (gated: ≥30d + appropriateness)
//	POST   /api/v1/copy/follows                        follow (auth, disclosure acknowledged)
//	DELETE /api/v1/copy/follows/{id}                   unfollow (cancels pending children)
//	POST   /api/v1/admin/copy/strategies/{id}/suspend  Compliance Officer misconduct action
//
// All bodies use decimal strings — no float money ever crosses the API.
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"exchange/internal/copy"
	"exchange/internal/gateway"
)

// copyService is the handler→product seam (*copy.Service).
type copyService interface {
	Discover(ctx context.Context) ([]copy.DiscoveryEntry, error)
	CreateStrategy(ctx context.Context, in copy.CreateStrategyInput) (*copy.Strategy, error)
	List(ctx context.Context, strategyID, callerAccountID int64) (*copy.Strategy, error)
	Suspend(ctx context.Context, strategyID, adminUserID int64, ip, reason string) (*copy.Strategy, error)
	Follow(ctx context.Context, in copy.FollowInput) (*copy.Follow, error)
	Unfollow(ctx context.Context, followID, actorAccountID int64) (*copy.UnfollowResult, error)
	MyFollows(ctx context.Context, accountID int64) ([]copy.FollowView, error)
}

// CopyStrategies serves GET /api/v1/copy/strategies — LISTED strategies
// ranked by computed return; nothing self-reported is ever served.
func CopyStrategies(svc copyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := svc.Discover(r.Context())
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"strategies": entries})
	}
}

type createStrategyBody struct {
	DisplayName     string `json:"display_name"`
	Description     string `json:"description"`
	Currency        string `json:"currency"`
	InstrumentClass string `json:"instrument_class"`
	ProfitSharePct  string `json:"profit_share_pct"`
}

// CopyStrategyCreate serves POST /api/v1/copy/strategies — the caller's
// account becomes the strategy manager; the profile starts INCUBATING.
func CopyStrategyCreate(svc copyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body createStrategyBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		st, err := svc.CreateStrategy(r.Context(), copy.CreateStrategyInput{
			ManagerAccountID: accountID,
			DisplayName:      body.DisplayName,
			Description:      body.Description,
			Currency:         body.Currency,
			InstrumentClass:  body.InstrumentClass,
			ProfitSharePct:   body.ProfitSharePct,
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, st)
	}
}

// CopyStrategyList serves POST /api/v1/copy/strategies/{id}/list —
// requests LISTED; the service enforces ≥30d incubation + appropriateness
// PASS fail-closed (only the strategy's own manager may request).
func CopyStrategyList(svc copyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		st, err := svc.List(r.Context(), id, accountID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}
}

type followBody struct {
	StrategyID         int64  `json:"strategy_id"`
	AllocationNotional string `json:"allocation_notional"`
	SafetyMode         string `json:"safety_mode"`
	StopLossCap        string `json:"stop_loss_cap"`
}

// CopyFollow serves POST /api/v1/copy/follows — binds the caller's
// account to a LISTED strategy. The response echoes the positions
// disclosure (unfollow never auto-closes copied positions).
func CopyFollow(svc copyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var body followBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		f, err := svc.Follow(r.Context(), copy.FollowInput{
			InvestorAccountID:  accountID,
			StrategyID:         body.StrategyID,
			AllocationNotional: body.AllocationNotional,
			SafetyMode:         body.SafetyMode,
			StopLossCap:        body.StopLossCap,
		})
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{
			"follow":     f,
			"disclosure": copy.FollowDisclosure,
		})
	}
}

// CopyMyFollows serves GET /api/v1/copy/follows — the caller's own
// follow list (all statuses) with strategy display fields joined.
func CopyMyFollows(svc copyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		follows, err := svc.MyFollows(r.Context(), accountID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"follows": follows})
	}
}

// CopyUnfollow serves DELETE /api/v1/copy/follows/{id} — pending child
// orders are cancelled; open copied positions stay with the investor
// (the disclosure is returned with the result).
func CopyUnfollow(svc copyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		res, err := svc.Unfollow(r.Context(), id, accountID)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

type suspendBody struct {
	Reason string `json:"reason"`
}

// AdminCopyStrategySuspend serves POST /api/v1/admin/copy/strategies/{id}/suspend
// — Compliance Officer misconduct action (scope breach, stat manipulation).
// Audit-logged BEFORE the status flip; existing follows keep running.
func AdminCopyStrategySuspend(svc copyService, trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := adminActorFrom(r, trustProxy)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		id, err := lpPathID(r)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		var body suspendBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST", "malformed JSON body",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		st, err := svc.Suspend(r.Context(), id, actor.UserID, actor.ClientIP, body.Reason)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, st)
	}
}
