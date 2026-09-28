// Task 5.3.15 — fee-rate surface and governed promo windows.
//
//	GET  /api/v1/fees                            — caller's effective rates
//	                                               incl. active promos
//	POST /api/v1/admin/fees/promo                — propose a promo window
//	GET  /api/v1/admin/fees/promos               — list windows
//	POST /api/v1/admin/fees/promo/{id}/approve   — four-eyes apply → fee_tiers
//	POST /api/v1/admin/fees/promo/{id}/reject    — reject a pending window
//
// Reads resolve through settlement.PgxFeeStore/FeeTier.RateBps — the
// engine seam already applies promo_* strictly while promo_until > now().
// The write path is promos.Store (fee_promo_windows, migration 181):
// create only proposes; a second Finance-Ops principal applies the promo
// inside the spec §8.2 15-minute four-eyes window.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/promos"
	"exchange/internal/settlement"
)

// ---------------------------------------------------------------------------
// GET /api/v1/fees
// ---------------------------------------------------------------------------

// AccountFees serves the caller's current fee schedule: base maker/taker
// bps plus, when a promo window is live, the effective promo rates and
// promo_until. Both raw and effective rates are returned so clients can
// display "was/is".
func AccountFees(store settlement.FeeStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		tier, found, err := store.FeeTierFor(r.Context(), claims.AccountID)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"fee store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if !found {
			WriteError(w, "FEE_TIER_NOT_FOUND",
				"account has no fee tier", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		now := time.Now().UTC()
		maker, makerPromo, _ := tier.RateBps(settlement.RoleMaker, now)
		taker, takerPromo, _ := tier.RateBps(settlement.RoleTaker, now)
		resp := map[string]any{
			"account_id":          claims.AccountID,
			"tier_id":             tier.ID,
			"tier_name":           tier.TierName,
			"maker_bps":           tier.MakerBps.String(),
			"taker_bps":           tier.TakerBps.String(),
			"effective_maker_bps": maker.String(),
			"effective_taker_bps": taker.String(),
			"promo_active":        makerPromo || takerPromo,
		}
		if tier.PromoUntil != nil && now.Before(*tier.PromoUntil) {
			resp["promo"] = map[string]any{
				"until":     tier.PromoUntil.UTC().Format(time.RFC3339),
				"maker_bps": tier.PromoMakerBps,
				"taker_bps": tier.PromoTakerBps,
			}
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}

// ---------------------------------------------------------------------------
// Admin promo windows
// ---------------------------------------------------------------------------

// promoActor resolves the integer actor id for audit columns — the
// claim's Subject when numeric, else the account id.
func promoActor(claims *auth.Claims) int64 {
	if n, err := strconv.ParseInt(claims.Subject, 10, 64); err == nil && n > 0 {
		return n
	}
	return claims.AccountID
}

// AdminFeePromos returns create/list/approve/reject handlers over the
// promos store.
func AdminFeePromos(store *promos.Store) (create, list, approve, reject http.HandlerFunc) {
	create = func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		var body struct {
			FeeTierID     int64   `json:"fee_tier_id"`
			PromoMakerBps *string `json:"promo_maker_bps"`
			PromoTakerBps *string `json:"promo_taker_bps"`
			EndsAt        string  `json:"ends_at"`
			Note          *string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, "INVALID_REQUEST",
				"malformed body", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var maker, taker *decimal.Decimal
		parse := func(s *string) (*decimal.Decimal, error) {
			if s == nil {
				return nil, nil
			}
			d, err := decimal.NewFromString(*s)
			if err != nil {
				return nil, err
			}
			return &d, nil
		}
		var perr error
		if maker, perr = parse(body.PromoMakerBps); perr != nil {
			WriteError(w, "INVALID_REQUEST",
				"promo_maker_bps must be a decimal string", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if taker, perr = parse(body.PromoTakerBps); perr != nil {
			WriteError(w, "INVALID_REQUEST",
				"promo_taker_bps must be a decimal string", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		ends, err := time.Parse(time.RFC3339, body.EndsAt)
		if err != nil {
			WriteError(w, "INVALID_REQUEST",
				"ends_at must be RFC3339", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		win, err := store.Create(r.Context(), body.FeeTierID, promoActor(claims),
			maker, taker, ends, body.Note)
		switch {
		case errors.Is(err, promos.ErrTierMissing):
			WriteError(w, "NOT_FOUND",
				"fee tier not found", gateway.RequestIDFrom(r.Context()), nil)
		case errors.Is(err, promos.ErrNoRates), errors.Is(err, promos.ErrRateRange),
			errors.Is(err, promos.ErrBadWindow):
			WriteError(w, "INVALID_REQUEST",
				err.Error(), gateway.RequestIDFrom(r.Context()), nil)
		case err != nil:
			WriteError(w, "SERVICE_DEGRADED",
				"promo store unavailable", gateway.RequestIDFrom(r.Context()), nil)
		default:
			WriteJSON(w, http.StatusCreated, win)
		}
	}
	list = func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		wins, err := store.List(r.Context(), r.URL.Query().Get("status"), 200)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"promo store unavailable", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"promo_windows": wins})
	}
	approve = func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST",
				"invalid window id", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		win, err := store.Approve(r.Context(), id, promoActor(claims))
		switch {
		case errors.Is(err, promos.ErrNotFound):
			WriteError(w, "NOT_FOUND",
				"promo window not found", gateway.RequestIDFrom(r.Context()), nil)
		case errors.Is(err, promos.ErrFourEyes):
			WriteError(w, "DUAL_CONTROL_REQUIRED",
				"approver must differ from the window creator", gateway.RequestIDFrom(r.Context()), nil)
		case errors.Is(err, promos.ErrTooLate):
			WriteError(w, "DUAL_CONTROL_REQUIRED",
				"four-eyes window (15m) elapsed — window expired", gateway.RequestIDFrom(r.Context()), nil)
		case errors.Is(err, promos.ErrNotPending), errors.Is(err, promos.ErrBadWindow):
			WriteError(w, "INVALID_REQUEST",
				err.Error(), gateway.RequestIDFrom(r.Context()), nil)
		case errors.Is(err, promos.ErrTierMissing):
			WriteError(w, "NOT_FOUND",
				"fee tier not found", gateway.RequestIDFrom(r.Context()), nil)
		case err != nil:
			WriteError(w, "SERVICE_DEGRADED",
				"promo store unavailable", gateway.RequestIDFrom(r.Context()), nil)
		default:
			WriteJSON(w, http.StatusOK, win)
		}
	}
	reject = func(w http.ResponseWriter, r *http.Request) {
		claims := requireAdmin(w, r)
		if claims == nil {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, "INVALID_REQUEST",
				"invalid window id", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var body struct {
			Reason *string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // reason optional
		win, err := store.Reject(r.Context(), id, promoActor(claims), body.Reason)
		switch {
		case errors.Is(err, promos.ErrNotFound):
			WriteError(w, "NOT_FOUND",
				"promo window not found", gateway.RequestIDFrom(r.Context()), nil)
		case errors.Is(err, promos.ErrFourEyes):
			WriteError(w, "DUAL_CONTROL_REQUIRED",
				"rejector must differ from the window creator", gateway.RequestIDFrom(r.Context()), nil)
		case errors.Is(err, promos.ErrNotPending):
			WriteError(w, "INVALID_REQUEST",
				err.Error(), gateway.RequestIDFrom(r.Context()), nil)
		case err != nil:
			WriteError(w, "SERVICE_DEGRADED",
				"promo store unavailable", gateway.RequestIDFrom(r.Context()), nil)
		default:
			WriteJSON(w, http.StatusOK, win)
		}
	}
	return create, list, approve, reject
}
