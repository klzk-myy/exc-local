// Tasks 5.3.2 + 5.3.27 + 5.3.34 — the HTTP edge middleware that applies
// the ratelimit.Limiter decision and emits the standard headers:
//
//	X-RateLimit-Limit     effective tier req/s (post-throttle)
//	X-RateLimit-Remaining tokens left in the bucket
//	X-RateLimit-Reset     UTC epoch seconds when the 1s window resets
//	                      (spec §8.3 contract — supersedes the task-text
//	                      "seconds remaining" wording)
//	Retry-After           RFC 6585 seconds on 429 and 418
//	X-Ban-Expires         epoch seconds of ban expiry on 418 (Task 5.3.34)
//
// Denied requests are answered with the spec §8.7 envelope carrying
// RATE_LIMIT_TIER_EXCEEDED (bucket), REQUEST_WEIGHT_EXCEEDED (minute
// weight quota), or IP_BANNED (418) — all §23-registered codes. Emissions
// go through the Emit seam — wired to gateway.Router.WriteError so the
// Task 5.3.21 registry gates every code (startup-fatal on unregistered).
//
// Fail-closed: a limiter outage surfaces as 503 SERVICE_DEGRADED rather
// than an un-metered pass (spec §2.7 L3 edge rejection is cheap; an
// unmetered gateway is not).
package middleware

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exchange/internal/auth"
	"exchange/internal/ratelimit"
)

// Header names (spec §8.3 / §24 #192; Task 5.3.34 ban headers).
const (
	HeaderRateLimitLimit     = "X-RateLimit-Limit"
	HeaderRateLimitRemaining = "X-RateLimit-Remaining"
	HeaderRateLimitReset     = "X-RateLimit-Reset"
	HeaderRetryAfter         = "Retry-After"
	HeaderBanExpires         = "X-Ban-Expires"
)

// Emitted §23 codes (registered owners: 5.3.27, 5.3.42, 5.3.34, 5.3.29).
const (
	CodeRateLimitTierExceeded = "RATE_LIMIT_TIER_EXCEEDED" // 429
	CodeRequestWeightExceeded = "REQUEST_WEIGHT_EXCEEDED"  // 429
	CodeIPBanned              = "IP_BANNED"                // 418
	CodeServiceDegraded       = "SERVICE_DEGRADED"         // 503
)

// ErrorEmitter writes a §8.7 error envelope for a registered code —
// gateway.Router.WriteError satisfies it (same signature); nil falls back
// to a local minimal writer so the middleware stays usable outside the
// router.
type ErrorEmitter func(w http.ResponseWriter, r *http.Request, code, message string, details map[string]any)

// TierResolver maps an authenticated request to its rate-limit tier.
// claims is nil for anonymous traffic. Implementations typically read
// the account's fee_tier / API-key tier; nil resolver ⇒ authenticated
// callers get ratelimit.TierBasic (fail-closed against tier confusion).
type TierResolver func(ctx context.Context, claims *auth.Claims) ratelimit.Tier

// RateLimitOptions tunes the middleware.
type RateLimitOptions struct {
	Weights     *ratelimit.WeightTable // nil ⇒ ratelimit.DefaultWeights
	ResolveTier TierResolver           // nil ⇒ basic-if-authenticated
	TrustProxy  bool                   // honor X-Forwarded-For (behind HAProxy only)
	Emit        ErrorEmitter           // nil ⇒ local envelope writer
	// Exempt lists path prefixes that carry no quota (route metadata
	// rate_tier=exempt: /health/*, server time, meta dumps). Nil falls
	// back to DefaultExemptPrefixes.
	Exempt []string
}

// DefaultExemptPrefixes mirrors the registry's TierExempt routes so the
// edge middleware honors the same metadata before mux dispatch.
var DefaultExemptPrefixes = []string{
	"/health", "/api/v1/time", "/api/v1/openapi.json",
	"/api/v1/routes", "/api/v1/errors", "/developer",
}

// RateLimit returns middleware enforcing the tiered edge limit.
func RateLimit(l *ratelimit.Limiter, opts RateLimitOptions) func(http.Handler) http.Handler {
	if opts.Weights == nil {
		opts.Weights = ratelimit.NewWeightTable(nil)
	}
	exempt := opts.Exempt
	if exempt == nil {
		exempt = DefaultExemptPrefixes
	}
	emit := opts.Emit
	if emit == nil {
		emit = defaultEmit
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, p := range exempt {
				if strings.HasPrefix(r.URL.Path, p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			id := resolveIdentity(r, opts)
			weight, isOrder := opts.Weights.Lookup(r.Method, r.URL.Path)
			res, err := l.Check(r.Context(), id, weight, isOrder)
			if err != nil {
				emit(w, r, CodeServiceDegraded,
					"rate limiter unavailable", nil)
				return
			}
			setRateHeaders(w, res)
			switch res.Status {
			case ratelimit.HitOK:
				next.ServeHTTP(w, r)
			case ratelimit.HitRateLimited:
				emit(w, r, CodeRateLimitTierExceeded,
					"rate limit exceeded",
					map[string]any{"retry_after": int(res.RetryAfter)})
			case ratelimit.HitWeightExceeded:
				emit(w, r, CodeRequestWeightExceeded,
					"request weight quota exceeded",
					map[string]any{"retry_after": int(res.RetryAfter)})
			case ratelimit.HitBanned, ratelimit.HitBannedNew:
				if res.Ban != nil {
					w.Header().Set(HeaderBanExpires,
						strconv.FormatInt(res.Ban.ExpiresAt/1000, 10))
				}
				emit(w, r, CodeIPBanned,
					"ip banned for repeated post-429 abuse",
					map[string]any{"retry_after": int(res.RetryAfter)})
			default:
				emit(w, r, "INTERNAL_ERROR", "internal error", nil)
			}
		})
	}
}

// defaultEmit is the standalone §8.7 envelope writer used when no
// registry-gated emitter is wired (unit tests, non-router chains).
func defaultEmit(w http.ResponseWriter, _ *http.Request, code, message string, details map[string]any) {
	status := http.StatusInternalServerError
	switch code {
	case CodeRateLimitTierExceeded, CodeRequestWeightExceeded:
		status = http.StatusTooManyRequests
	case CodeIPBanned:
		status = http.StatusTeapot
	case CodeServiceDegraded:
		status = http.StatusServiceUnavailable
	case CodeUnsupportedProtocolVersion:
		status = http.StatusBadRequest
	}
	env := map[string]any{
		"type":      "error",
		"error":     code,
		"message":   message,
		"status":    status,
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	for k, v := range details {
		env[k] = v // surfaces retry_after at top level per §10.5 item 5
	}
	body, _ := json.Marshal(env)
	w.Header().Set("Content-Type", "application/problem+json")
	if ra, ok := details["retry_after"].(int); ok && ra > 0 {
		w.Header().Set(HeaderRetryAfter, strconv.Itoa(ra))
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// resolveIdentity derives the ratelimit.Identity: claims-bearing
// requests key on accountID (spec §8.3 — Basic/Standard/Professional/
// Institutional are per-account); anonymous requests key on client IP
// as Public.
func resolveIdentity(r *http.Request, opts RateLimitOptions) ratelimit.Identity {
	ip := ClientIP(r, opts.TrustProxy)
	claims := auth.ClaimsFrom(r.Context())
	if claims == nil || claims.AccountID == 0 {
		return ratelimit.Identity{Tier: ratelimit.TierPublic, Key: ip, IP: ip}
	}
	tier := ratelimit.TierBasic
	if opts.ResolveTier != nil {
		tier = opts.ResolveTier(r.Context(), claims)
	}
	return ratelimit.Identity{
		Tier: tier,
		Key:  strconv.FormatInt(claims.AccountID, 10),
		IP:   ip,
	}
}

// ClientIP extracts the client IP. With TrustProxy enabled (gateway
// sits behind HAProxy, which owns the XFF append) the leftmost
// X-Forwarded-For entry wins; otherwise RemoteAddr. Never trusts XFF
// from an untrusted edge — a spoofable source IP would defeat the
// Task 5.3.34 ban machinery.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// setRateHeaders emits the three standard headers (spec §8.3 contract:
// Reset is UTC epoch seconds of window end).
func setRateHeaders(w http.ResponseWriter, res ratelimit.HitResult) {
	w.Header().Set(HeaderRateLimitLimit, strconv.FormatInt(res.Limit, 10))
	w.Header().Set(HeaderRateLimitRemaining, strconv.FormatInt(max64(res.Remaining, 0), 10))
	w.Header().Set(HeaderRateLimitReset, strconv.FormatInt(res.ResetEpoch, 10))
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
