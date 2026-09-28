// Task 9.3.7/9.3.8/9.3.25 — handler-level tests for the Phase-09 ops
// surface. Store-backed paths are covered by the gated integration file
// (handlers_flags_integration_test.go); these exercise auth gates,
// error envelopes, and the infra-free seams.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/cache"
	"exchange/internal/flags"
	"exchange/internal/middleware"
)

func adminClaims() *auth.Claims {
	return &auth.Claims{Subject: "9001", AccountID: 1, Scopes: []string{"admin"}}
}

// Every flag-mutation handler must reject anonymous callers fail-closed
// before touching the store — the zero-value store is never reached.
func TestFlagHandlersRequireAdmin(t *testing.T) {
	list, create, get, update, toggle, del, advance :=
		FlagHandlers(&flags.Store{})
	handlers := []struct {
		name    string
		h       http.HandlerFunc
		method  string
		path    string
		pathKey string
	}{
		{"list", list, "GET", "/api/v1/admin/flags", ""},
		{"create", create, "POST", "/api/v1/admin/flags", ""},
		{"get", get, "GET", "/api/v1/admin/flags/x", "x"},
		{"update", update, "PUT", "/api/v1/admin/flags/x", "x"},
		{"toggle", toggle, "POST", "/api/v1/admin/flags/x", "x"},
		{"delete", del, "DELETE", "/api/v1/admin/flags/x", "x"},
		{"advance", advance, "POST", "/api/v1/admin/flags/x/advance", "x"},
	}
	for _, tc := range handlers {
		req := httptest.NewRequest(tc.method, tc.path,
			strings.NewReader(`{}`))
		if tc.pathKey != "" {
			req.SetPathValue("name", tc.pathKey)
		}
		rec := httptest.NewRecorder()
		tc.h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s anon: status=%d want 401", tc.name, rec.Code)
		}
		// non-admin scope → 403
		req = req.WithContext(auth.WithClaims(req.Context(),
			auth.Claims{Subject: "u", AccountID: 2, Scopes: []string{"read"}}))
		rec = httptest.NewRecorder()
		tc.h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s non-admin: status=%d want 403", tc.name, rec.Code)
		}
	}
}

// Malformed bodies are INVALID_REQUEST, never a panic or store call.
func TestFlagCreateMalformedBody(t *testing.T) {
	_, create, _, _, _, _, _ := FlagHandlers(&flags.Store{})
	req := httptest.NewRequest("POST", "/api/v1/admin/flags",
		strings.NewReader(`{not json`))
	req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
	rec := httptest.NewRecorder()
	create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope parse: %v", err)
	}
	if env.Error != "INVALID_REQUEST" {
		t.Fatalf("code=%q want INVALID_REQUEST", env.Error)
	}
}

// The manual warm endpoint returns the report and 503 when a unit fails
// (fail-closed: an incomplete warm is not a success).
func TestAdminCacheWarm(t *testing.T) {
	ok := cache.Unit{Name: "u_ok", Priority: cache.P0,
		Warm: func(context.Context, *cache.Env) (int, error) { return 3, nil }}
	bad := cache.Unit{Name: "u_bad", Priority: cache.P1, MinKeys: 1,
		Warm: func(context.Context, *cache.Env) (int, error) {
			return 0, errors.New("source down")
		}}

	mk := func(u cache.Unit) *httptest.ResponseRecorder {
		w := cache.New(&cache.Env{Now: func() time.Time {
			return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		}}, []cache.Unit{u})
		req := httptest.NewRequest("POST", "/api/v1/admin/cache/warm", nil)
		req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
		rec := httptest.NewRecorder()
		AdminCacheWarm(w).ServeHTTP(rec, req)
		return rec
	}

	if rec := mk(ok); rec.Code != http.StatusOK {
		t.Fatalf("ok warm: status=%d body=%s", rec.Code, rec.Body)
	}
	rec := mk(bad)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed warm: status=%d want 503", rec.Code)
	}
	var rep cache.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("report parse: %v", err)
	}
	if rep.OK() || rep.Units[0].Status != "error" {
		t.Fatalf("report should mark failed unit: %+v", rep)
	}

	// Anon → 401.
	req := httptest.NewRequest("POST", "/api/v1/admin/cache/warm", nil)
	rec = httptest.NewRecorder()
	AdminCacheWarm(cache.New(nil, nil)).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon warm: status=%d want 401", rec.Code)
	}
}

// With no Redis at all the public status feed still answers truthfully —
// source=gateway-local, status=unknown (never a fabricated
// "operational").
func TestSystemStatusFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	SystemStatus(nil)(rec,
		httptest.NewRequest("GET", "/api/v1/system/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body struct {
		Status string `json:"status"`
		Source string `json:"source"`
		Mode   string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Source != "gateway-local" || body.Status == "operational" {
		t.Fatalf("fallback must not claim operational: %+v", body)
	}
}

// Ops health export requires the audit role — anonymous callers get 401
// and resolvers' denials surface as role errors, all fail-closed.
func TestAdminOpsHealthRequiresAuditor(t *testing.T) {
	d := &OpsExportDeps{}
	// Anon → 401.
	rec := httptest.NewRecorder()
	AdminOpsHealth(d)(rec,
		httptest.NewRequest("GET", "/api/v1/admin/ops/health", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon: status=%d want 401", rec.Code)
	}
	// Admin claims + failing resolver → INTERNAL_ERROR, no payload leak.
	req := httptest.NewRequest("GET", "/api/v1/admin/ops/health", nil)
	req = req.WithContext(auth.WithClaims(req.Context(), *adminClaims()))
	rec = httptest.NewRecorder()
	d.Resolver = func(context.Context, int64) (string, error) {
		return "", errors.New("rbac down")
	}
	AdminOpsHealth(d)(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("resolver err: status=%d want 500", rec.Code)
	}
	// Non-audit role → 403-class role rejection.
	d.Resolver = func(context.Context, int64) (string, error) {
		return "Support Agent", nil
	}
	rec = httptest.NewRecorder()
	AdminOpsHealth(d)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-audit role: status=%d want 403", rec.Code)
	}
	// Audit role → 200 with shed stats merged.
	d.Resolver = func(context.Context, int64) (string, error) {
		return "Read-Only Auditor", nil
	}
	d.ShedStats = func() middleware.ShedStats {
		return middleware.ShedStats{Stage: 2}
	}
	rec = httptest.NewRecorder()
	AdminOpsHealth(d)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auditor: status=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		LoadShedding struct {
			Stage int `json:"stage"`
		} `json:"load_shedding"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.LoadShedding.Stage != 2 {
		t.Fatalf("shed stage not exported: %+v", out)
	}
}
