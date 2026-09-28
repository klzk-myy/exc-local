// Task 2.3.6 — X-Degradation-Mode response header middleware.
//
// Spec §2.4 "Client Visibility": the active degradation mode is surfaced on
// every API response in the X-Degradation-Mode header (Phase-02 C++ core
// owns the mode record under system:degradation:mode; this middleware reads
// it at request time so the gateway always reports the authoritative value).
//
// Fail-closed read semantics (spec §2.7), identical to the C++ ModeManager
// decision: a lost mode read must never imply Normal. When the store is
// unreachable — or no reader is wired — the header reports "Maintenance",
// the strictest-safe value, so clients and edge gates treat the system as
// suspended rather than healthy.
package middleware

import (
	"context"
	"net/http"
	"os"
	"strings"

	exchredis "exchange/internal/redis"
)

// DegradationHeader is the canonical response header name (spec §2.4).
const DegradationHeader = "X-Degradation-Mode"

// DegradationReader is the narrow read seam over the coordination Redis
// client — *redis.Client satisfies it; tests substitute a fake.
type DegradationReader interface {
	GetDegradationMode(ctx context.Context) (exchredis.DegradationState, error)
}

// DegradationModeHeader wraps next and sets X-Degradation-Mode on every
// response. reader==nil or any read error reports Maintenance (fail-closed);
// an absent Redis key reports Normal (spec §2.4 default), handled inside
// GetDegradationMode.
func DegradationModeHeader(reader DegradationReader, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := string(exchredis.ModeMaintenance)
		if reader != nil {
			if st, err := reader.GetDegradationMode(r.Context()); err == nil && st.Mode != "" {
				mode = string(st.Mode)
			}
		}
		w.Header().Set(DegradationHeader, mode)
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// DegradationGate — the enforcement half of Task 2.3.6.
// ---------------------------------------------------------------------------

// GateEmitter matches gateway.Router.WriteError — §8.7 envelope emission.
type GateEmitter func(w http.ResponseWriter, r *http.Request, code, message string, details map[string]any)

// marketDataOnlyPaths are the read surfaces that stay open under
// MarketDataOnly (spec §2.4 — books/trades/tickers/klines/instruments +
// venue metadata; everything else, including all writes, is rejected).
var marketDataOnlyPaths = []string{
	"/api/v1/book/", "/api/v1/trades/", "/api/v1/ticker/",
	"/api/v1/klines/", "/api/v1/instruments",
	"/api/v1/exchange-info", "/api/v1/time", "/api/v1/fees",
	"/ws/", "/health", "/ready", "/metrics",
}

// healthPaths stay reachable even under Maintenance — liveness/readiness
// must keep answering so the orchestrator can observe the suspension.
var healthPaths = []string{"/health", "/ready", "/metrics"}

func pathAllowed(path string, allowed []string) bool {
	for _, p := range allowed {
		if path == p || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// DegradationGate rejects requests the active degradation mode forbids —
// the *enforcement* counterpart of DegradationModeHeader (which only
// reports). Spec §2.4 semantics:
//
//   - Normal / Throttled / SpotOnly → pass (Throttled is the rate-limit
//     multiplier's job; every listed instrument is spot FX so SpotOnly
//     has no additional REST surface to close).
//   - ReadOnly → mutating methods reject 503 DEGRADED_MODE; the
//     /api/v1/admin/ tree stays writable because the admin control
//     surface is how an operator manages and clears the mode.
//   - MarketDataOnly → only the market-data read paths stay open.
//   - Maintenance → everything rejects 503 MAINTENANCE_MODE except the
//     health/metrics probes an orchestrator needs to observe the state.
//
// A mode-read failure is Maintenance (fail-closed, spec §2.7 — identical
// to the header middleware's rule).
func DegradationGate(reader DegradationReader, emit GateEmitter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mode := exchredis.ModeMaintenance
			if reader != nil {
				if st, err := reader.GetDegradationMode(r.Context()); err == nil && st.Mode != "" {
					mode = st.Mode
				}
			}
			path := r.URL.Path
			switch mode {
			case exchredis.ModeMaintenance:
				if !pathAllowed(path, healthPaths) {
					emit(w, r, "MAINTENANCE_MODE",
						"system in maintenance — retry later", nil)
					return
				}
			case exchredis.ModeReadOnly:
				if isWriteMethod(r.Method) && !strings.HasPrefix(path, "/api/v1/admin/") {
					emit(w, r, "DEGRADED_MODE",
						"system is read-only — writes are suspended", nil)
					return
				}
			case exchredis.ModeMarketDataOnly:
				if !pathAllowed(path, marketDataOnlyPaths) || isWriteMethod(r.Method) {
					emit(w, r, "DEGRADED_MODE",
						"market-data-only mode — endpoint suspended", nil)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// DegradationModeFromEnv wires the middleware to the coordination Redis at
// EXC_REDIS_ADDR (default 127.0.0.1:16379 — the dev primary from
// docker-compose.dev.yml). The client is lazy: the connection opens on the
// first request, and every read failure yields Maintenance until it lands.
func DegradationModeFromEnv(next http.Handler) http.Handler {
	addr := os.Getenv("EXC_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	return DegradationModeHeader(exchredis.New(addr, "", 0), next)
}
