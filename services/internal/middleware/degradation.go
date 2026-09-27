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
