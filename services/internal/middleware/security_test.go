package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func ok200(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }

func TestSecurityHeadersAlwaysSet(t *testing.T) {
	h := SecurityHeaders(SecurityOptions{}, http.HandlerFunc(ok200))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	for _, hdr := range []string{
		"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy",
		"Permissions-Policy", "Content-Security-Policy"} {
		if w.Header().Get(hdr) == "" {
			t.Fatalf("missing %s", hdr)
		}
	}
	if w.Header().Get("Content-Security-Policy") != DefaultCSP {
		t.Fatalf("csp: %q", w.Header().Get("Content-Security-Policy"))
	}
	// HSTS off by default (plaintext listener).
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS emitted without enablement")
	}
	// No allowlist → no CORS headers even with an Origin.
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Origin", "https://app.example.com")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS emitted without allowlist")
	}
}

func TestSecurityHeadersHSTSAndCORSAllowlist(t *testing.T) {
	h := SecurityHeaders(SecurityOptions{
		AllowedOrigins: []string{"https://app.example.com"},
		HSTSMaxAge:     63072000,
	}, http.HandlerFunc(ok200))

	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("ACAO: %q", got)
	}
	if w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("credentials flag missing")
	}
	if w.Header().Get("Vary") != "Origin" {
		t.Fatal("Vary: Origin missing")
	}
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS missing when enabled")
	}

	// Non-allowlisted origin: no ACAO — never "*".
	r = httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin echoed")
	}
}

func TestCORSPreflight(t *testing.T) {
	h := SecurityHeaders(SecurityOptions{
		AllowedOrigins: []string{"https://app.example.com"},
	}, http.HandlerFunc(ok200))

	newPreflight := func(origin string) (*httptest.ResponseRecorder, *http.Request) {
		r := httptest.NewRequest("OPTIONS", "/x", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		return httptest.NewRecorder(), r
	}

	w, r := newPreflight("https://app.example.com")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d", w.Code)
	}
	for _, hdr := range []string{
		"Access-Control-Allow-Methods", "Access-Control-Allow-Headers",
		"Access-Control-Max-Age"} {
		if w.Header().Get(hdr) == "" {
			t.Fatalf("preflight missing %s", hdr)
		}
	}
	// Idempotency-Key and traceparent must be allowed through preflight.
	if ah := w.Header().Get("Access-Control-Allow-Headers"); !contains(ah, "Idempotency-Key") || !contains(ah, "traceparent") {
		t.Fatalf("allow-headers: %q", ah)
	}

	// Disallowed origin preflight → 403, never a wildcard.
	w, r = newPreflight("https://evil.example")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("evil preflight: %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Fatal("wildcard ACAO emitted")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
