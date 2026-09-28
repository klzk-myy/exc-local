// Task 5.3.29 item 9 (governance remediation #17) — the gateway is the
// authoritative emitter of the HTTP hardening-header set:
//
//	CORS:   Access-Control-Allow-Origin/Methods/Headers/Max-Age —
//	        allowlist only, NEVER "*" (credentialed API surface)
//	CSP:    Content-Security-Policy on gateway-served assets
//	Always: Strict-Transport-Security, X-Content-Type-Options,
//	        X-Frame-Options, Referrer-Policy, Permissions-Policy
//
// HAProxy MUST NOT also emit these (see infrastructure/gateway/haproxy.cfg
// comments) — single emitter avoids double-header divergence.
package middleware

import (
	"net/http"
	"strconv"
	"strings"
)

// SecurityOptions configures the hardening layer.
type SecurityOptions struct {
	// AllowedOrigins is the CORS allowlist. Empty ⇒ no CORS headers at
	// all (deny cross-origin reads by omission — the strict default).
	AllowedOrigins []string
	// CSP is the Content-Security-Policy value. Empty selects the
	// JSON-API default below — the gateway serves no HTML/JS assets, so
	// "default-src 'none'" is both correct and complete.
	CSP string
	// HSTSMaxAge enables Strict-Transport-Security when > 0 (seconds).
	// Leave 0 on plaintext dev listeners.
	HSTSMaxAge int64
	// CORSMaxAgeS is the preflight cache hint (seconds).
	CORSMaxAgeS int
}

// DefaultCSP is the deny-all policy for a JSON-only gateway.
const DefaultCSP = "default-src 'none'; frame-ancestors 'none'"

// SecurityHeaders emits the full hardening set on every response and
// answers preflight OPTIONS against the allowlist.
func SecurityHeaders(opts SecurityOptions, next http.Handler) http.Handler {
	csp := opts.CSP
	if csp == "" {
		csp = DefaultCSP
	}
	maxAge := opts.CORSMaxAgeS
	if maxAge <= 0 {
		maxAge = 600
	}
	allowed := map[string]bool{}
	for _, o := range opts.AllowedOrigins {
		allowed[strings.TrimSpace(o)] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Content-Security-Policy", csp)
		if opts.HSTSMaxAge > 0 {
			h.Set("Strict-Transport-Security",
				"max-age="+strconv.FormatInt(opts.HSTSMaxAge, 10)+"; includeSubDomains; preload")
		}

		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true")
		}

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			// Preflight: only answer for allowlisted origins; never "*".
			if origin == "" || !allowed[origin] {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, X-API-KEY, X-TIMESTAMP, X-SIGNATURE, Idempotency-Key, X-2FA-Token, X-Request-ID, traceparent")
			h.Set("Access-Control-Max-Age", strconv.Itoa(maxAge))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
