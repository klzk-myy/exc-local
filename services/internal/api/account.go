// Task 5.3.4 — Account & Balance Endpoints.
//
// GET /api/v1/account/balances    — all currency balances (avail/locked/total)
// GET /api/v1/positions           — open positions + unrealized P&L
// GET /api/v1/account/risk-limits — effective limits + today's utilisation
//
// Identity is always the authenticated claims account — foreign account
// access is FORBIDDEN, missing claims UNAUTHORIZED (fail-closed §2.7).
// Money values serialize as decimal strings through ::text reads
// (funding.PgStore); no floats anywhere on the read path.
package api

import (
	"context"
	stderrors "errors"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/auth"
	"exchange/internal/funding"
	"exchange/internal/gateway"
	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// balanceSource is the read seam for the balances endpoint
// (funding.PgStore implements it).
type balanceSource interface {
	BalancesFor(ctx context.Context, accountID int64) ([]funding.BalanceRow, error)
}

// positionSource is the read seam for the positions endpoint.
type positionSource interface {
	PositionsFor(ctx context.Context, accountID int64) ([]funding.PositionRow, error)
}

// accountMetaSource resolves the caller's account meta (kyc_tier for the
// limits view, ownership checks elsewhere).
type accountMetaSource interface {
	AccountMeta(ctx context.Context, id int64) (*funding.AccountMeta, error)
}

// claimsAccount resolves the authenticated account id or writes the
// fail-closed error response (nil claims ⇒ 401, no account context ⇒ 401).
func claimsAccount(w http.ResponseWriter, r *http.Request) (int64, *auth.Claims, bool) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil || claims.AccountID == 0 {
		WriteError(w, "UNAUTHORIZED", "authentication required",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, nil, false
	}
	return claims.AccountID, claims, true
}

// rejectForeignAccount guards the account_id query/body override: only a
// value matching the claims account is honored — everything else is
// FORBIDDEN, including malformed ids.
func rejectForeignAccount(w http.ResponseWriter, r *http.Request, claims *auth.Claims, raw string) bool {
	if raw == "" {
		return false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n != claims.AccountID {
		WriteError(w, "FORBIDDEN", "cannot access another account",
			gateway.RequestIDFrom(r.Context()), nil)
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// GET /api/v1/account/balances
// ---------------------------------------------------------------------------

// AccountBalances serves the wallet read — every currency row for the
// authenticated account.
func AccountBalances(src balanceSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if rejectForeignAccount(w, r, claims, r.URL.Query().Get("account_id")) {
			return
		}
		rows, err := src.BalancesFor(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []funding.BalanceRow{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID,
			"balances":   rows,
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/positions
// ---------------------------------------------------------------------------

// AccountPositions serves the open-position read (quantity <> 0) with
// entry/mark prices, unrealized + realized P&L and margin usage.
func AccountPositions(src positionSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if rejectForeignAccount(w, r, claims, r.URL.Query().Get("account_id")) {
			return
		}
		rows, err := src.PositionsFor(r.Context(), accountID)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		if rows == nil {
			rows = []funding.PositionRow{}
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID,
			"positions":  rows,
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/account/risk-limits
// ---------------------------------------------------------------------------

// AccountRiskLimits serves the limits + utilisation view backed by the
// risk.LimitsService (per-account → tier → global row resolution with
// §13.6 defaults). kyc_tier comes from the account meta so a tier-scoped
// limit row resolves correctly.
func AccountRiskLimits(limits *risk.LimitsService, meta accountMetaSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if rejectForeignAccount(w, r, claims, r.URL.Query().Get("account_id")) {
			return
		}
		if limits == nil {
			WriteError(w, "SERVICE_DEGRADED", "risk limits service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		tier := ""
		if meta != nil {
			m, err := meta.AccountMeta(r.Context(), accountID)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			tier = m.KYCTier
		}
		view, err := limits.LimitsView(r.Context(), accountID, tier)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, riskLimitsJSON(view))
	}
}

// riskLimitsJSON projects the service View into a stable API shape —
// money as decimal strings, utilisation percentages when limits exist.
func riskLimitsJSON(v *risk.View) map[string]any {
	lim := v.Limits
	limits := map[string]any{
		"max_order_qty":          decStringPtr(lim.MaxOrderQty),
		"max_daily_volume":       decStringPtr(lim.MaxDailyVolume),
		"max_open_orders":        lim.MaxOpenOrders,
		"daily_withdraw_limit":   decStringPtr(lim.DailyWithdrawLimit),
		"max_withdraw_amount":    decStringPtr(lim.MaxWithdrawAmount),
		"withdraw_rate_per_hour": decStringPtr(lim.WithdrawRatePerHour),
		"max_notional_exposure":  decStringPtr(lim.MaxNotionalExposure),
		"max_short_exposure":     decStringPtr(lim.MaxShortExposure),
		"max_account_notional":   decStringPtr(lim.MaxAccountNotional),
	}
	usage := map[string]any{
		"daily_volume":        v.Usage.Volume.String(),
		"daily_withdrawn":     v.Usage.Withdrawn.String(),
		"open_orders":         v.OpenOrders,
		"daily_volume_pct":    decStringPtr(v.DailyVolumePct),
		"daily_withdrawn_pct": decStringPtr(v.DailyWithdrawnPct),
		"open_orders_pct":     decStringPtr(v.OpenOrdersPct),
	}
	symbols := make([]map[string]any, 0, len(v.Symbols))
	for _, s := range v.Symbols {
		symbols = append(symbols, map[string]any{
			"symbol":             s.Symbol,
			"gross_notional":     s.GrossNotional.String(),
			"short_notional":     s.ShortNotional.String(),
			"max_notional":       decStringPtr(s.MaxNotional),
			"max_short":          decStringPtr(s.MaxShort),
			"gross_notional_pct": decStringPtr(s.GrossNotionalPct),
			"short_notional_pct": decStringPtr(s.ShortNotionalPct),
		})
	}
	return map[string]any{
		"account_id": v.AccountID,
		"day":        v.Day,
		"limits":     limits,
		"usage":      usage,
		"symbols":    symbols,
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func decStringPtr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

// writeServiceErr maps a service error to the §8.7 envelope: coded
// errors keep their registry-resolved status; anything else is
// INTERNAL_ERROR (500, no internals leaked — fail closed).
func writeServiceErr(w http.ResponseWriter, r *http.Request, err error) {
	var e *excerrors.Error
	if stderrors.As(err, &e) {
		WriteError(w, e.Code, e.Message, gateway.RequestIDFrom(r.Context()), nil)
		return
	}
	WriteError(w, "INTERNAL_ERROR", "internal error",
		gateway.RequestIDFrom(r.Context()), nil)
}

// parseLimitQuery clamps the §8.8 page size.
func parseLimitQuery(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > max {
		return def
	}
	return n
}

// parseTimeQuery parses an RFC3339 date-range filter (from/to).
func parseTimeQuery(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
