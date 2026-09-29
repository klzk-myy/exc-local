// Phase-11 Task 11.3.4 — kill-switch admission gate.
//
// HTTP-layer enforcement of the trading halt: while `halt:global` is
// raised, every order-admission request is rejected with TRADING_HALTED
// (503) before it reaches a handler. The exemption model reuses the
// Task 5.3.25 CANCEL_EXEMPT precedent in shedding.go — a halted venue
// MUST keep accepting cancels, reads, health and WebSocket traffic:
//
//   - ShedExempt covers DELETE /api/v1/orders*, admin mass-cancel and
//     the CANCEL_EXEMPT header/context marker;
//   - the gate additionally exempts the qty-down keep-priority amend
//     (risk-reducing, cancel-like) and the dry-run preview
//     (no engine dispatch);
//   - POST/PUT/PATCH under /api/v1/orders otherwise count as new-order
//     entry (submit, batch, cancel-replace, amend).
//
// Fail closed (spec §2.7): a nil reader or a lookup error rejects
// admission with TRADING_HALTED — an unverifiable halt flag is treated
// as halted, never as open. Scoped (account/instrument/…) enforcement
// happens deeper in orders.Service via the KillSwitch seam, which also
// covers the non-HTTP entry paths (WS order.place, FIX, internal).
package middleware

import (
	"context"
	"net/http"
	"strings"
)

// KillSwitchReader is the global-flag read seam —
// *admin.KillSwitchResolver satisfies it; tests substitute a fake.
type KillSwitchReader interface {
	GlobalHalted(ctx context.Context) (bool, error)
}

// orderKeepPriorityPath is the qty-down amend (Task 5.3.37) — it can
// only reduce a resting order's residual, so it stays open under a
// halt exactly like a cancel.
const orderKeepPriorityPath = "/amend/keep-priority"

// orderTestPath is the dry-run preview (Task 5.3.39) — no order is
// admitted to the engine, so it is not gated.
const orderTestPath = "/api/v1/orders/test"

// OrderAdmissionRequest reports whether the request is new-order entry
// for kill-switch purposes. Cancels, risk-reducing amends, reads and
// every ShedExempt surface return false.
func OrderAdmissionRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/v1/orders") {
		return false
	}
	if p == orderTestPath || strings.HasSuffix(p, orderKeepPriorityPath) {
		return false
	}
	if ShedExempt(r) { // CANCEL_EXEMPT lane / any future exemption
		return false
	}
	return true
}

// KillSwitchGate rejects order-admission requests with TRADING_HALTED
// while the global halt flag is raised — and while the flag is
// unreadable (fail closed). All other traffic passes untouched.
func KillSwitchGate(reader KillSwitchReader, emit GateEmitter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !OrderAdmissionRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			if reader == nil {
				emit(w, r, "TRADING_HALTED",
					"trading state unverifiable — new orders rejected",
					map[string]any{"scope": "GLOBAL"})
				return
			}
			halted, err := reader.GlobalHalted(r.Context())
			if err != nil {
				emit(w, r, "TRADING_HALTED",
					"trading state unverifiable — new orders rejected",
					map[string]any{"scope": "GLOBAL"})
				return
			}
			if halted {
				emit(w, r, "TRADING_HALTED",
					"global trading halt active — new orders rejected",
					map[string]any{"scope": "GLOBAL"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
