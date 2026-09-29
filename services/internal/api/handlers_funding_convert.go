// Phase-11 Task 11.3.9 item 4 — currency conversion REST surface.
//
//	POST /api/v1/funding/convert      indicative conversion quote (persisted)
//	GET  /api/v1/funding/conversions  the account's conversion history
//
// Deposit currency ≠ account currency converts at the reference mid-rate
// ± conversion_spread_bps; every converted quote lands in
// funding_currency_conversions (migration 198). The rate source is fail-closed:
// unwired/stale/missing sources emit PRICE_ORACLE_UNAVAILABLE rather
// than a fabricated rate. Same-currency requests short-circuit to
// rate 1 with converted=false and no record.
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

// conversionQuoter is the conversion seam (*funding.ConversionService).
type conversionQuoter interface {
	Quote(ctx context.Context, req funding.ConversionRequest) (*funding.ConversionResult, error)
	History(ctx context.Context, accountID int64, limit int) ([]funding.ConversionRecord, error)
}

type conversionRequest struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency,omitempty"` // default: account base currency
	Direction    string `json:"direction,omitempty"`   // default: DEPOSIT
	Amount       string `json:"amount"`
}

// FundingConvert serves POST /api/v1/funding/convert — the indicative
// quote is computed, persisted, and returned with provenance
// (rate_source, rate_valid_at).
func FundingConvert(svc conversionQuoter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		var req conversionRequest
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
		res, err := svc.Quote(r.Context(), funding.ConversionRequest{
			AccountID:    accountID,
			FromCurrency: req.FromCurrency,
			ToCurrency:   req.ToCurrency,
			Direction:    req.Direction,
			Amount:       amount,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}
}

// FundingConversions serves GET /api/v1/funding/conversions?limit= — the
// caller's persisted conversion records, newest first (limit ≤ 500).
func FundingConversions(svc conversionQuoter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		limit := parseLimitQuery(r.URL.Query().Get("limit"), 100, 500)
		rows, err := svc.History(r.Context(), accountID, limit)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"items": rows, "count": len(rows), "limit": limit,
		})
	}
}
