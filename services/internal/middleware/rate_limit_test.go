// Tasks 5.3.2/5.3.27/5.3.34 — HTTP-surface tests for the rate-limit
// middleware: standard headers, 429/418 envelopes, exempt routes, and
// identity keying.
package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"exchange/internal/auth"
	"exchange/internal/ratelimit"
)

// okHandler is shared with degradation_test.go (same package).

func newTestChain(t *testing.T, opts RateLimitOptions) (http.Handler, *ratelimit.MemBackend) {
	t.Helper()
	mem := ratelimit.NewMemBackend()
	l := ratelimit.NewLimiter(mem, ratelimit.LimiterOptions{Fallback: mem})
	return RateLimit(l, opts)(okHandler()), mem
}

func doReq(t *testing.T, h http.Handler, method, path, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRateHeadersOnSuccess(t *testing.T) {
	h, _ := newTestChain(t, RateLimitOptions{})
	rec := doReq(t, h, "GET", "/api/v1/book/EURUSD", "203.0.113.1:1234")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get(HeaderRateLimitLimit); got != "5" {
		t.Fatalf("X-RateLimit-Limit=%q, want 5 (public)", got)
	}
	if got := rec.Header().Get(HeaderRateLimitRemaining); got == "" {
		t.Fatal("X-RateLimit-Remaining missing")
	}
	reset, err := strconv.ParseInt(rec.Header().Get(HeaderRateLimitReset), 10, 64)
	if err != nil || reset <= 0 {
		t.Fatalf("X-RateLimit-Reset=%q, want epoch seconds", rec.Header().Get(HeaderRateLimitReset))
	}
}

func Test429EnvelopeAndRetryAfter(t *testing.T) {
	h, _ := newTestChain(t, RateLimitOptions{})
	// Unlisted path → default weight 1; public capacity is 10 (5×2).
	for i := 0; i < 10; i++ {
		doReq(t, h, "GET", "/api/v1/unlisted", "203.0.113.2:9")
	}
	rec := doReq(t, h, "GET", "/api/v1/unlisted", "203.0.113.2:9")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", rec.Code)
	}
	if ra := rec.Header().Get(HeaderRetryAfter); ra == "" || ra == "0" {
		t.Fatalf("Retry-After=%q, want positive seconds", ra)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope not JSON: %v", err)
	}
	if env["type"] != "error" || env["error"] != CodeRateLimitTierExceeded {
		t.Fatalf("envelope=%v", env)
	}
	if env["retry_after"] == nil {
		t.Fatal("envelope missing retry_after member")
	}
}

func Test418BanWithExpiresHeader(t *testing.T) {
	h, mem := newTestChain(t, RateLimitOptions{})
	// 11 hits arm + consume the post-429 offense marker → 418.
	var rec *httptest.ResponseRecorder
	for i := 0; i < 12; i++ {
		rec = doReq(t, h, "GET", "/api/v1/ticker/EURUSD", "198.51.100.5:1")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status=%d, want 418", rec.Code)
	}
	if rec.Header().Get(HeaderBanExpires) == "" {
		t.Fatal("X-Ban-Expires missing on 418")
	}
	if rec.Header().Get(HeaderRetryAfter) == "" {
		t.Fatal("Retry-After missing on 418")
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env["error"] != CodeIPBanned {
		t.Fatalf("envelope error=%v, want IP_BANNED", env["error"])
	}
	// During the ban, requests keep returning 418 without re-escalating.
	rec2 := doReq(t, h, "GET", "/api/v1/ticker/EURUSD", "198.51.100.5:1")
	if rec2.Code != http.StatusTeapot {
		t.Fatalf("banned follow-up status=%d", rec2.Code)
	}
	if b, _ := mem.BanInfo(context.Background(), "198.51.100.5"); b == nil {
		t.Fatal("ban record must exist")
	}
}

func TestExemptPathsBypassLimiter(t *testing.T) {
	h, _ := newTestChain(t, RateLimitOptions{})
	for i := 0; i < 30; i++ { // far above public quota
		rec := doReq(t, h, "GET", "/health/live", "203.0.113.3:1")
		if rec.Code != http.StatusOK {
			t.Fatalf("exempt path limited at hit %d (status=%d)", i, rec.Code)
		}
	}
}

func TestAuthenticatedAccountKeyedNotIP(t *testing.T) {
	h, _ := newTestChain(t, RateLimitOptions{})
	claims := auth.Claims{Subject: "u1", AccountID: 77, Scopes: []string{"read"}}
	// Two different source IPs, same account → same Basic bucket (cap 40).
	// Unlisted path keeps weight 1 so the quota math stays exact.
	for i := 0; i < 40; i++ {
		req := httptest.NewRequest("GET", "/api/v1/unlisted", nil)
		req.RemoteAddr = "10.0.0.1:1"
		if i%2 == 0 {
			req.RemoteAddr = "10.0.0.2:1"
		}
		req = req.WithContext(auth.WithClaims(req.Context(), claims))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("hit %d status=%d", i, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/unlisted", nil)
	req.RemoteAddr = "10.9.9.9:1"
	req = req.WithContext(auth.WithClaims(req.Context(), claims))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("shared account bucket: status=%d, want 429", rec.Code)
	}
}

func TestLimiterOutageIs503FailClosed(t *testing.T) {
	broken := ratelimit.NewLimiter(errorBackend{ratelimit.NewMemBackend()},
		ratelimit.LimiterOptions{Fallback: errorBackend{ratelimit.NewMemBackend()}})
	h := RateLimit(broken, RateLimitOptions{})(okHandler())
	rec := doReq(t, h, "GET", "/api/v1/trades/EURUSD", "203.0.113.4:1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("limiter outage status=%d, want 503 fail-closed", rec.Code)
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env["error"] != CodeServiceDegraded {
		t.Fatalf("envelope=%v", env)
	}
}

// errorBackend always fails — drives the middleware 503 path.
type errorBackend struct{ *ratelimit.MemBackend }

var errBackendDown = errors.New("down")

func (errorBackend) Hit(context.Context, ratelimit.HitInput) (ratelimit.HitResult, error) {
	return ratelimit.HitResult{}, errBackendDown
}
