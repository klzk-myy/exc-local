// Phase-14 Task 14.3.3 — testnet environment handlers.
//
//	POST /api/v1/test/seed          — write a named balance preset
//	                                  (default "standard"); does not
//	                                  consume the reset cooldown.
//	POST /api/v1/test/reset-seed    — reset → clean slate → preset in one
//	                                  cooldown slot (the canonical testnet
//	                                  provisioning call).
//	POST /api/v1/test/funding/deposit    — simulated faucet credit
//	POST /api/v1/test/funding/withdrawal — simulated debit (INSUFFICIENT_
//	                                      BALANCE path shape)
//
// Safety contract (same fail-closed posture as POST /test/reset):
// the testenv.Service is constructed with the deployment's environment
// label; production, empty, or unknown labels — including a
// testnet/production mismatch — disable every handler 403 FORBIDDEN.
// Simulated funding touches balances rows ONLY: no funding_transactions,
// no internal/funding rail adapter, no banking anything.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/testenv"
)

// testnetCaller resolves the authenticated account or fails the request.
func testnetCaller(w http.ResponseWriter, r *http.Request) (int64, bool) {
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil || claims.AccountID == 0 {
		WriteError(w, "UNAUTHORIZED",
			"authentication required", gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return claims.AccountID, true
}

// testnetError maps testenv failures onto production-shaped codes so
// testnet clients exercise the real error surface.
func testnetError(w http.ResponseWriter, r *http.Request, err error) bool {
	rid := gateway.RequestIDFrom(r.Context())
	switch {
	case err == nil:
		return false
	case errors.Is(err, testenv.ErrDisabled):
		WriteError(w, "FORBIDDEN",
			"test endpoints are unavailable in production deployments",
			rid, nil)
	case errors.Is(err, testenv.ErrCooldown):
		WriteError(w, "RATE_LIMIT_EXCEEDED",
			"one reset per account per 5 minutes",
			rid, map[string]any{"retry_after": int(testenv.ResetCooldown / time.Second)})
	case errors.Is(err, testenv.ErrInvalidPreset):
		WriteError(w, "INVALID_REQUEST",
			"unknown preset — supported: standard", rid, nil)
	case errors.Is(err, testenv.ErrInvalidCurrency):
		WriteError(w, "INVALID_REQUEST",
			"currency must be an ISO-4217 code (3 letters)", rid, nil)
	case errors.Is(err, testenv.ErrInvalidAmount):
		WriteError(w, "INVALID_REQUEST",
			"amount must be a positive decimal within the simulated cap", rid, nil)
	case errors.Is(err, testenv.ErrInsufficientBalance):
		WriteError(w, "INSUFFICIENT_BALANCE",
			"simulated withdrawal exceeds available balance", rid, nil)
	default:
		WriteError(w, "SERVICE_DEGRADED",
			"test operation failed", rid, nil)
	}
	return true
}

type testnetSeedReq struct {
	Preset string `json:"preset"`
}

type testnetFundReq struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

// testnetDecode reads the request body; an empty body decodes to the
// zero value so preset defaults apply to {} and bodyless posts alike.
func testnetDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil &&
		!errors.Is(err, io.EOF) {
		WriteError(w, "INVALID_REQUEST", "malformed JSON body",
			gateway.RequestIDFrom(r.Context()), nil)
		return false
	}
	return true
}

// TestSeed serves POST /api/v1/test/seed — apply a named preset wallet
// set to the caller's test account (additive; does not consume the
// reset cooldown).
func TestSeed(svc *testenv.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := testnetCaller(w, r)
		if !ok {
			return
		}
		var req testnetSeedReq
		if !testnetDecode(w, r, &req) {
			return
		}
		preset := testenv.Preset(req.Preset)
		if req.Preset == "" {
			preset = testenv.PresetStandard
		}
		amounts, err := svc.Seed(r.Context(), accountID, preset)
		if testnetError(w, r, err) {
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID,
			"simulated":  true,
			"preset":     string(preset),
			"balances":   amounts,
		})
	}
}

// TestResetSeed serves POST /api/v1/test/reset-seed — the canonical
// testnet provisioning call: reset (one 5-min cooldown slot) then the
// named preset (default "standard").
func TestResetSeed(svc *testenv.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := testnetCaller(w, r)
		if !ok {
			return
		}
		var req testnetSeedReq
		if !testnetDecode(w, r, &req) {
			return
		}
		preset := testenv.Preset(req.Preset)
		if req.Preset == "" {
			preset = testenv.PresetStandard
		}
		counts, amounts, err := svc.ResetTo(r.Context(), accountID, preset)
		if testnetError(w, r, err) {
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID,
			"simulated":  true,
			"preset":     string(preset),
			"reset":      counts,
			"balances":   amounts,
			"cooldown_s": int(testenv.ResetCooldown / time.Second),
		})
	}
}

// TestFundDeposit serves POST /api/v1/test/funding/deposit — simulated
// faucet credit. Never invokes internal/funding or any banking rail;
// body: {"currency":"USD","amount":"5000.00"}.
func TestFundDeposit(svc *testenv.Service) http.HandlerFunc {
	return testnetFund(svc, true)
}

// TestFundWithdraw serves POST /api/v1/test/funding/withdrawal —
// simulated debit; shortfall maps to the production
// INSUFFICIENT_BALANCE shape. No rail, no review tier, no OTP.
func TestFundWithdraw(svc *testenv.Service) http.HandlerFunc {
	return testnetFund(svc, false)
}

func testnetFund(svc *testenv.Service, deposit bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := testnetCaller(w, r)
		if !ok {
			return
		}
		var req testnetFundReq
		if !testnetDecode(w, r, &req) {
			return
		}
		var available string
		var err error
		if deposit {
			available, err = svc.SimulateDeposit(r.Context(), accountID, req.Currency, req.Amount)
		} else {
			available, err = svc.SimulateWithdrawal(r.Context(), accountID, req.Currency, req.Amount)
		}
		if testnetError(w, r, err) {
			return
		}
		op := "deposit"
		if !deposit {
			op = "withdrawal"
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id": accountID,
			"simulated":  true,
			"operation":  op,
			"currency":   req.Currency,
			"amount":     req.Amount,
			"available":  available,
		})
	}
}
