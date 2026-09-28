// Unit tests for AuthMiddleware / RequireScope / RequireTwoFactor
// (Tasks 5.3.1, 5.3.10).
package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestAuthMiddlewareMissingToken(t *testing.T) {
	i := testIssuer(t)
	h := AuthMiddleware(i, nil)(okHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer → %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("want RFC 7807 media type, got %s", ct)
	}
	var p struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem decode: %v", err)
	}
	if p.Code != CodeUnauthorized {
		t.Fatalf("code=%s want UNAUTHORIZED", p.Code)
	}
}

func TestAuthMiddlewareValidToken(t *testing.T) {
	i := testIssuer(t)
	var captured Claims
	h := AuthMiddleware(i, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := ClaimsFrom(r.Context())
		if c == nil {
			t.Error("claims must be attached")
			return
		}
		captured = *c
		w.WriteHeader(http.StatusNoContent)
	}))
	tok, _, err := i.Issue("u-1", IssueOptions{AccountID: 7, Scopes: []string{"trade"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid token rejected: %d %s", rec.Code, rec.Body.String())
	}
	if captured.Subject != "u-1" || captured.AccountID != 7 {
		t.Fatalf("claims not propagated: %+v", captured)
	}
}

func TestAuthMiddlewareSessionEnforcement(t *testing.T) {
	// A token bound to a revoked session rejects even though its JWT
	// signature is still valid (Task 5.3.10: revocation works).
	m, _, _ := testManager(t, SessionConfig{})
	ctx := context.Background()
	b, err := m.Issue(ctx, IssueRequest{UserID: "u-1", AccountID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(ctx, b.Session.ID); err != nil {
		t.Fatal(err)
	}
	h := AuthMiddleware(m.issuer, m)(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer "+b.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session → %d, want 401", rec.Code)
	}
}

func TestRequireScope(t *testing.T) {
	inner := RequireScope("trade")(okHandler())
	withClaims := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), Claims{Scopes: []string{"read"}})))
	})
	rec := httptest.NewRecorder()
	withClaims.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing scope → %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), CodeInsufficientScope) {
		t.Fatalf("want INSUFFICIENT_SCOPE, got %s", rec.Body.String())
	}
	// Granted scope passes.
	inner = RequireScope("trade")(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(WithClaims(req.Context(), Claims{Scopes: []string{"read", "trade"}}))
	rec = httptest.NewRecorder()
	inner.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("granted scope rejected: %d", rec.Code)
	}
}

func TestRequireTwoFactor(t *testing.T) {
	inner := RequireTwoFactor()(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/withdrawals", nil)
	// No AMR → 403 TWO_FACTOR_REQUIRED.
	req = req.WithContext(WithClaims(req.Context(), Claims{AMR: []string{"pwd"}}))
	rec := httptest.NewRecorder()
	inner.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CodeTwoFactorRequired) {
		t.Fatalf("want TWO_FACTOR_REQUIRED 403, got %d %s", rec.Code, rec.Body.String())
	}
	// totp/fido2 AMR passes (spec §12.6 elevation).
	for _, amr := range [][]string{{"pwd", "totp"}, {"fido2"}} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/withdrawals", nil)
		req = req.WithContext(WithClaims(req.Context(), Claims{AMR: amr}))
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("amr %v must satisfy 2FA gate: %d", amr, rec.Code)
		}
	}
}
