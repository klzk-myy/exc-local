// Cluster D platform-surface handler + OpenAPI tests (Tasks 5.3.8,
// 5.3.13, 5.3.15, 5.3.16, 5.3.17, 5.3.19, 5.3.20).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/deprecation"
	"exchange/internal/errs"
	"exchange/internal/gateway"
	"exchange/internal/promos"
	"exchange/internal/tax"
	"exchange/internal/testenv"
	"exchange/internal/webhooks"
)

// withClaims builds a request carrying Claims as auth middleware would.
func withClaims(t *testing.T, method, target, body string, accountID int64, scopes ...string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return r.WithContext(auth.WithClaims(r.Context(), auth.Claims{
		Subject:   "501",
		AccountID: accountID,
		Scopes:    scopes,
	}))
}

func problemErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response not json: %v\n%s", err, rec.Body.String())
	}
	errCode, _ := env["error"].(string)
	return errCode
}

// ---------------------------------------------------------------------------
// Task 5.3.8 — OpenAPI document.
// ---------------------------------------------------------------------------

func TestOpenAPIDocument(t *testing.T) {
	doc := OpenAPIDocument(gateway.SeedRoutes(), errs.New(), "9.9.9")

	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi=%v", doc["openapi"])
	}
	info, _ := doc["info"].(map[string]any)
	if info["version"] != "9.9.9" {
		t.Fatalf("version=%v", info["version"])
	}
	paths, _ := doc["paths"].(map[string]any)
	nPaths, nOps := DocCount(doc)
	if nPaths == 0 || nOps < nPaths {
		t.Fatalf("paths=%d ops=%d", nPaths, nOps)
	}
	// Every registered route's path must be present; every route must
	// produce an operation.
	seenOps := 0
	for _, rt := range gateway.SeedRoutes() {
		item, ok := paths[rt.Path].(map[string]any)
		if !ok {
			t.Fatalf("registered route %s missing from paths", rt.Path)
		}
		m := strings.ToLower(rt.Method)
		if rt.Method == "WS" {
			m = "get"
		}
		op, ok := item[m].(map[string]any)
		if !ok {
			t.Fatalf("route %s %s missing operation", rt.Method, rt.Path)
		}
		seenOps++
		// Route metadata is carried through.
		if op["x-owner"] != rt.Owner || op["x-status"] != string(rt.Status) {
			t.Fatalf("%s %s metadata lost", rt.Method, rt.Path)
		}
	}
	if seenOps != nOps {
		t.Fatalf("ops=%d want %d", nOps, seenOps)
	}
	// New cluster-D routes must be in the doc.
	for _, p := range []string{
		"/api/v1/openapi.json",
		"/api/v1/test/reset",
		"/api/v1/fees",
		"/api/v1/admin/fees/promo",
		"/api/v1/admin/fees/promo/{id}/approve",
		"/api/v1/developer/api-keys",
		"/api/v1/webhooks",
		"/api/v1/webhooks/{id}",
		"/api/v1/webhooks/{id}/rotate-secret",
		"/api/v1/webhooks/{id}/deliveries",
		"/api/v1/tax/report",
		"/api/v1/account/tax-report",
		"/api/v1/admin/api-deprecations",
	} {
		if _, ok := paths[p]; !ok {
			t.Fatalf("path %s missing", p)
		}
	}
	// Path params surface on templated routes.
	getOp := paths["/api/v1/webhooks/{id}"].(map[string]any)["get"]
	if getOp == nil {
		// try delete — either way the parameters block must exist.
		getOp = paths["/api/v1/webhooks/{id}"].(map[string]any)["delete"]
	}
	params, _ := getOp.(map[string]any)["parameters"].([]map[string]any)
	if len(params) == 0 || params[0]["name"] != "id" || params[0]["in"] != "path" {
		t.Fatalf("path params=%v", params)
	}
	// Security schemes + Problem + error-code table.
	comps := doc["components"].(map[string]any)
	schemes := comps["securitySchemes"].(map[string]any)
	for _, s := range []string{"bearerAuth", "apiKeyAuth", "oauth2"} {
		if schemes[s] == nil {
			t.Fatalf("security scheme %s missing", s)
		}
	}
	if comps["schemas"].(map[string]any)["Problem"] == nil {
		t.Fatal("Problem schema missing")
	}
	codes, _ := comps["x-error-codes"].([]map[string]any)
	foundGone := false
	for _, c := range codes {
		if c["code"] == "ENDPOINT_GONE" {
			foundGone = true
			if c["http_status"] != 410 {
				t.Fatalf("ENDPOINT_GONE status=%v", c["http_status"])
			}
		}
	}
	if !foundGone {
		t.Fatal("ENDPOINT_GONE missing from x-error-codes")
	}
	if len(codes) < 100 {
		t.Fatalf("error-code table=%d rows — registry not loaded", len(codes))
	}
}

func TestDeveloperPortal(t *testing.T) {
	rec := httptest.NewRecorder()
	DeveloperPortal().ServeHTTP(rec, httptest.NewRequest("GET", "/developer", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/api/v1/openapi.json", "SwaggerUIBundle", "/developer/migration"} {
		if !strings.Contains(body, want) {
			t.Fatalf("portal page missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 5.3.19 — tax-report handler.
// ---------------------------------------------------------------------------

type emptyFills struct{}

func (emptyFills) Fills(context.Context, int64, time.Time) ([]tax.Fill, error) {
	return nil, nil
}

func taxSvc(t *testing.T) *tax.Service {
	t.Helper()
	svc, err := tax.NewService(emptyFills{})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestTaxReportHandler(t *testing.T) {
	h := TaxReport(taxSvc(t))

	// Unauthenticated → UNAUTHORIZED.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/tax/report?year=2026", nil))
	if problemErr(t, rec) != "UNAUTHORIZED" {
		t.Fatalf("unauth: %s", rec.Body.String())
	}

	// Missing year → INVALID_REQUEST.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withClaims(t, "GET", "/api/v1/tax/report", "", 7))
	if problemErr(t, rec) != "INVALID_REQUEST" {
		t.Fatalf("missing year: %s", rec.Body.String())
	}

	// Bad method → INVALID_REQUEST.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withClaims(t, "GET", "/api/v1/tax/report?year=2026&method=SPECIFIC_ID", "", 7))
	if problemErr(t, rec) != "INVALID_REQUEST" {
		t.Fatalf("bad method: %s", rec.Body.String())
	}

	// Happy JSON.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withClaims(t, "GET", "/api/v1/tax/report?year=2026", "", 7))
	if rec.Code != 200 {
		t.Fatalf("json status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rep map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep["account_id"] != float64(7) || rep["year"] != float64(2026) || rep["method"] != "FIFO" {
		t.Fatalf("report=%v", rep)
	}

	// Content negotiation — Accept: text/csv and application/pdf.
	for _, tt := range []struct {
		accept, wantCT, wantPrefix string
	}{
		{"text/csv", "text/csv", "account_id,7"},
		{"application/pdf", "application/pdf", "%PDF-1.4"},
	} {
		r := withClaims(t, "GET", "/api/v1/tax/report?year=2026", "", 7)
		r.Header.Set("Accept", tt.accept)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.wantCT) ||
			!strings.HasPrefix(rec.Body.String(), tt.wantPrefix) {
			t.Fatalf("accept=%s ct=%s body=%.40q", tt.accept,
				rec.Header().Get("Content-Type"), rec.Body.String())
		}
		if rec.Header().Get("Content-Disposition") == "" {
			t.Fatal("missing Content-Disposition")
		}
	}
	// format param wins over Accept.
	r := withClaims(t, "GET", "/api/v1/tax/report?year=2026&format=csv", "", 7)
	r.Header.Set("Accept", "application/pdf")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("format param lost to Accept: %s", rec.Header().Get("Content-Type"))
	}
	// Invalid format.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withClaims(t, "GET", "/api/v1/tax/report?year=2026&format=xml", "", 7))
	if problemErr(t, rec) != "INVALID_REQUEST" {
		t.Fatalf("bad format: %s", rec.Body.String())
	}
	// Account 0 claim → UNAUTHORIZED.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withClaims(t, "GET", "/api/v1/tax/report?year=2026", "", 0))
	if problemErr(t, rec) != "UNAUTHORIZED" {
		t.Fatalf("account 0: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Task 5.3.13 — reset handler gating.
// ---------------------------------------------------------------------------

func TestTestResetHandler(t *testing.T) {
	// Unauthenticated.
	h := TestReset(testenv.New(nil, "test"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/test/reset", nil))
	if problemErr(t, rec) != "UNAUTHORIZED" {
		t.Fatalf("unauth: %s", rec.Body.String())
	}
	// Production env fails closed → FORBIDDEN.
	prod := TestReset(testenv.New(nil, "production"))
	rec = httptest.NewRecorder()
	prod.ServeHTTP(rec, withClaims(t, "POST", "/api/v1/test/reset", "{}", 7))
	if problemErr(t, rec) != "FORBIDDEN" || rec.Code != http.StatusForbidden {
		t.Fatalf("prod reset: %d %s", rec.Code, rec.Body.String())
	}
	// Empty env fails closed too.
	unknown := TestReset(testenv.New(nil, ""))
	rec = httptest.NewRecorder()
	unknown.ServeHTTP(rec, withClaims(t, "POST", "/api/v1/test/reset", "{}", 7))
	if problemErr(t, rec) != "FORBIDDEN" {
		t.Fatalf("empty-env reset: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Task 14.3.3 — testnet handler gating. Every path asserted below
// short-circuits before the service reaches PG (nil pool is safe):
// UNAUTHORIZED precedes decode, FORBIDDEN precedes store access, and
// INVALID_REQUEST precedes the seed/fund call.
// ---------------------------------------------------------------------------

func TestTestnetHandlers(t *testing.T) {
	handlers := map[string]http.HandlerFunc{
		"seed":       TestSeed(testenv.New(nil, "production")),
		"reset-seed": TestResetSeed(testenv.New(nil, "production")),
		"deposit":    TestFundDeposit(testenv.New(nil, "production")),
		"withdrawal": TestFundWithdraw(testenv.New(nil, "production")),
	}
	for name, h := range handlers {
		// Unauthenticated → UNAUTHORIZED.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
		if problemErr(t, rec) != "UNAUTHORIZED" {
			t.Fatalf("%s unauth: %s", name, rec.Body.String())
		}
		// Production env fails closed → FORBIDDEN (authed, valid body).
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, withClaims(t, "POST", "/x", "{}", 7))
		if problemErr(t, rec) != "FORBIDDEN" || rec.Code != http.StatusForbidden {
			t.Fatalf("%s prod: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Empty env fails closed too — the testnet/production mismatch case.
	rec := httptest.NewRecorder()
	TestSeed(testenv.New(nil, "")).ServeHTTP(rec,
		withClaims(t, "POST", "/x", "{}", 7))
	if problemErr(t, rec) != "FORBIDDEN" {
		t.Fatalf("empty-env seed: %s", rec.Body.String())
	}
	// Malformed body on an enabled env → INVALID_REQUEST (no PG touch).
	rec = httptest.NewRecorder()
	TestSeed(testenv.New(nil, "testnet")).ServeHTTP(rec,
		withClaims(t, "POST", "/x", "{not json", 7))
	if problemErr(t, rec) != "INVALID_REQUEST" {
		t.Fatalf("malformed seed: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Task 5.3.20 — migration guide + admin auth gating.
// ---------------------------------------------------------------------------

type guideRules struct{ rules []deprecation.Rule }

func (g guideRules) Active(context.Context) ([]deprecation.Rule, error) {
	return g.rules, nil
}

func TestMigrationGuide(t *testing.T) {
	repl := "/api/v1/orders"
	m := "GET"
	h := MigrationGuide(guideRules{rules: []deprecation.Rule{{
		Path:        "/api/v1/legacy",
		Method:      &m,
		Replacement: &repl,
		AnnouncedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SunsetAt:    time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/developer/migration", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Six-month notice", "ENDPOINT_GONE",
		"/api/v1/legacy", "/api/v1/orders", "2026-08-01"} {
		if !strings.Contains(body, want) {
			t.Fatalf("guide missing %q", want)
		}
	}
	// Nil source still renders the policy.
	rec = httptest.NewRecorder()
	MigrationGuide(nil).ServeHTTP(rec, httptest.NewRequest("GET", "/developer/migration", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Six-month notice") {
		t.Fatal("nil-source guide broken")
	}
}

// ---------------------------------------------------------------------------
// Auth gating on the remaining new handlers — a missing claim must 401
// before any store access; the admin scope is enforced for admin routes.
// ---------------------------------------------------------------------------

func TestNewHandlersAuthGating(t *testing.T) {
	reg, list, disable, rotate, deliveries := WebhookHandlers(&webhooks.Store{})
	for name, h := range map[string]http.HandlerFunc{
		"register":   reg,
		"list":       list,
		"disable":    disable,
		"rotate":     rotate,
		"deliveries": deliveries,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/webhooks", nil))
		if problemErr(t, rec) != "UNAUTHORIZED" {
			t.Fatalf("webhook %s unauth: %s", name, rec.Body.String())
		}
	}

	announce, depList := AdminDeprecations(&deprecation.Store{})
	rec := httptest.NewRecorder()
	announce.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil))
	if problemErr(t, rec) != "UNAUTHORIZED" {
		t.Fatalf("deprecation announce unauth: %s", rec.Body.String())
	}
	// Authenticated but non-admin → FORBIDDEN.
	rec = httptest.NewRecorder()
	announce.ServeHTTP(rec, withClaims(t, "POST", "/x", "{}", 7, "read"))
	if problemErr(t, rec) != "FORBIDDEN" {
		t.Fatalf("non-admin announce: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	depList.ServeHTTP(rec, withClaims(t, "GET", "/x", "", 7, "read"))
	if problemErr(t, rec) != "FORBIDDEN" {
		t.Fatalf("non-admin list: %s", rec.Body.String())
	}

	create, pList, approve, reject := AdminFeePromos(&promos.Store{})
	for name, h := range map[string]http.HandlerFunc{
		"create": create, "list": pList, "approve": approve, "reject": reject,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withClaims(t, "POST", "/x", "{}", 7, "read"))
		if problemErr(t, rec) != "FORBIDDEN" {
			t.Fatalf("promo %s non-admin: %s", name, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Task 5.3.16 — developer key helpers.
// ---------------------------------------------------------------------------

func TestRateTierOrDefault(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", "standard"},
		{"BASIC", "basic"},
		{"Professional", "professional"},
		{"institutional", "institutional"},
	} {
		got, ok := rateTierOrDefault(tt.in)
		if !ok || got != tt.want {
			t.Fatalf("rateTierOrDefault(%q)=(%q,%v) want %q", tt.in, got, ok, tt.want)
		}
	}
	// Non-sellable / unknown tiers are rejected, never silently coerced.
	for _, in := range []string{"admin", "public", "gold", "STANDARD PLUS"} {
		if _, ok := rateTierOrDefault(in); ok {
			t.Fatalf("rateTierOrDefault(%q) accepted", in)
		}
	}
}

func TestDecodePublicKey(t *testing.T) {
	pem := "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----"
	if got := decodePublicKey(pem); string(got) != pem {
		t.Fatal("PEM not passed through")
	}
	if got := decodePublicKey("aGVsbG8="); string(got) != "hello" {
		t.Fatalf("base64 decode: %q", got)
	}
	// Pure-hex strings are also valid base64 (base64 wins by design) —
	// a non-base64 blob falls through to raw bytes for the store to
	// reject.
	if got := decodePublicKey("!!!"); len(got) == 0 {
		t.Fatal("garbage input produced empty key")
	}
}
