// handlers_costs.go — Phase-20 Task 20.3.14 (spec §16.8, §24 #374):
// MiFID II costs & charges disclosure.
//
//	GET /api/v1/account/cost-preview?symbol=EUR/USD&side=BUY&quantity=100000
//	    — ex-ante preview: spread cost (configured LP markup model),
//	      explicit commission (product-profile pricing plan), one-night
//	      Tom-Next financing estimate, conversion estimate when the
//	      account currency differs — plus mid/quoted_at/valid_for_s
//	      honesty fields and the no-inducement statement.
//	GET /api/v1/account/cost-preview?annual=2025
//	    — ex-post annual statement for the account: realized costs
//	      aggregated out of ledger_entries (the PG book of record —
//	      reconciles to the income-ledger source by construction) plus
//	      the modeled spread cost over the year's fills.
//
// Both variants share the registered route — routes_v1.go is frozen
// Phase-05 schema, so the annual mode rides the same path on the
// `annual` parameter instead of claiming a new route registration.
package api

import (
	"net/http"
	"strconv"
	"strings"

	"exchange/internal/analytics"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
)

// AccountCostPreview serves both modes of the costs-disclosure route.
func AccountCostPreview(svc *analytics.CostsDisclosureService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED", "authentication required",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()

		// Ex-post mode: ?annual=YYYY produces the yearly statement.
		if raw := q.Get("annual"); raw != "" {
			year, err := strconv.Atoi(raw)
			if err != nil {
				WriteError(w, "INVALID_REQUEST", "annual must be a year (e.g. 2025)",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			disc, err := svc.Annual(r.Context(), claims.AccountID, year)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, disc)
			return
		}

		// Ex-ante mode.
		symbol := strings.TrimSpace(q.Get("symbol"))
		side := strings.TrimSpace(q.Get("side"))
		qtyS := strings.TrimSpace(q.Get("quantity"))
		if symbol == "" || side == "" || qtyS == "" {
			WriteError(w, "INVALID_REQUEST",
				"symbol, side and quantity are required (or ?annual=YYYY for the ex-post statement)",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		qty, err := decimal.NewFromString(qtyS)
		if err != nil || !qty.IsPositive() {
			WriteError(w, "INVALID_REQUEST",
				"quantity must be a positive decimal",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		preview, err := svc.Preview(r.Context(), claims.AccountID, symbol, side, qty)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, preview)
	}
}
