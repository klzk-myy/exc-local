// Task 5.3.13 — test-environment account reset.
//
//	POST /api/v1/test/reset — wipe the caller's balances/orders/positions
//	                          (+ ledger trail); non-production only;
//	                          1 reset / account / 5 minutes.
//
// Production: the service reports disabled and the handler fails closed
// 403 FORBIDDEN — the route stays registered (registry completeness)
// but can never run against prod data.
package api

import (
	"errors"
	"net/http"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/internal/testenv"
)

// TestReset serves POST /api/v1/test/reset.
func TestReset(svc *testenv.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			WriteError(w, "UNAUTHORIZED",
				"authentication required", gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		counts, err := svc.ResetAccount(r.Context(), claims.AccountID)
		switch {
		case errors.Is(err, testenv.ErrDisabled):
			WriteError(w, "FORBIDDEN",
				"test reset is unavailable in production deployments",
				gateway.RequestIDFrom(r.Context()), nil)
		case errors.Is(err, testenv.ErrCooldown):
			WriteError(w, "RATE_LIMIT_EXCEEDED",
				"one reset per account per 5 minutes",
				gateway.RequestIDFrom(r.Context()),
				map[string]any{"retry_after": int(testenv.ResetCooldown / time.Second)})
		case err != nil:
			WriteError(w, "SERVICE_DEGRADED",
				"reset failed", gateway.RequestIDFrom(r.Context()), nil)
		default:
			WriteJSON(w, http.StatusOK, map[string]any{
				"account_id": claims.AccountID,
				"reset":      counts,
				"cooldown_s": int(testenv.ResetCooldown / time.Second),
			})
		}
	}
}
