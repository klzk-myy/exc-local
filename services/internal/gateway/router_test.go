package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exchange/internal/errs"
)

func newTestRouter(t *testing.T) *Router {
	t.Helper()
	r := NewRouter(http.NewServeMux(), errs.New())
	if err := r.MountSeed(); err != nil {
		t.Fatalf("MountSeed: %v", err)
	}
	return r
}

func TestSeedMountsAndDumps(t *testing.T) {
	r := newTestRouter(t)
	routes := r.Routes()
	if len(routes) < 90 {
		t.Fatalf("seed table too small: %d routes", len(routes))
	}
	for _, rt := range routes {
		if rt.Owner == "" {
			t.Errorf("route %s %s has no owner", rt.Method, rt.Path)
		}
	}
	// Spot-check key routes exist with expected metadata.
	rt, ok := r.RouteFor("POST", "/api/v1/orders")
	if !ok || rt.Status != StatusLive || !contains(rt.Auth.Scopes, ScopeTrade) {
		t.Fatalf("POST /api/v1/orders metadata wrong: %+v ok=%v", rt, ok)
	}
	rt, ok = r.RouteFor("POST", "/api/v1/admin/liquidation/manual")
	if !ok || !rt.DualControl || rt.Auth.Role != RoleRiskManager {
		t.Fatalf("manual liquidation metadata wrong: %+v", rt)
	}
}

func TestStubServes501Envelope(t *testing.T) {
	r := newTestRouter(t)
	rec := httptest.NewRecorder()
	// /orders/countdown-cancel-all remains a stub (Task 5.3.33 lands the
	// dead-man REST surface); /orders itself is now live.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/countdown-cancel-all", strings.NewReader(`{}`))
	r.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("stub status %d, want 501", rec.Code)
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope unmarshal: %v", err)
	}
	if env.Error != errs.CodeNotImplemented || env.Type != "error" {
		t.Fatalf("stub envelope wrong: %+v", env)
	}
	if env.Details["owner"] == "" {
		t.Fatal("stub envelope must carry owner detail")
	}
}

func TestRoutesEndpoint(t *testing.T) {
	r := newTestRouter(t)
	rec := httptest.NewRecorder()
	r.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/routes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/routes status %d", rec.Code)
	}
	var body struct {
		Count  int         `json:"count"`
		Routes []routeInfo `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != len(body.Routes) || body.Count < 90 {
		t.Fatalf("route dump count mismatch: count=%d len=%d", body.Count, len(body.Routes))
	}
}

func TestErrorsEndpoint(t *testing.T) {
	r := newTestRouter(t)
	rec := httptest.NewRecorder()
	r.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/errors", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/errors status %d", rec.Code)
	}
	var body struct {
		Count     int            `json:"count"`
		SpecCount int            `json:"spec_count"`
		Codes     []errs.CodeDef `json:"error_codes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.SpecCount != 149 {
		t.Fatalf("spec_count %d, want 149", body.SpecCount)
	}
	if body.Count != len(body.Codes) || body.Count < 149 {
		t.Fatalf("error dump mismatch: count=%d len=%d", body.Count, len(body.Codes))
	}
}

func TestOpenAPIEndpoint(t *testing.T) {
	r := newTestRouter(t)
	rec := httptest.NewRecorder()
	r.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi status %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi version %v", doc["openapi"])
	}
	paths, _ := doc["paths"].(map[string]any)
	if _, ok := paths["/api/v1/orders"]; !ok {
		t.Fatal("openapi doc missing /api/v1/orders")
	}
}

func TestDuplicateAndInvalidRegistration(t *testing.T) {
	r := newTestRouter(t)
	dup := Route{Method: "GET", Path: "/api/v1/orders", Version: "v1",
		RateTier: TierBasic, Owner: "x", Status: StatusStub}
	if err := r.Register(dup, nil); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	bad := dup
	bad.Method = "FROBNICATE"
	bad.Path = "/api/v1/nope"
	if err := r.Register(bad, nil); err == nil {
		t.Fatal("invalid method accepted")
	}
	bad = dup
	bad.Path = "api/v1/nope"
	if err := r.Register(bad, nil); err == nil {
		t.Fatal("relative path accepted")
	}
	live := dup
	live.Path = "/api/v1/nope2"
	live.Status = StatusLive
	if err := r.Register(live, nil); err == nil {
		t.Fatal("live route without handler accepted")
	}
	stubWithHandler := live
	stubWithHandler.Status = StatusStub
	if err := r.Register(stubWithHandler, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
		t.Fatal("stub route with handler accepted")
	}
}

func TestBodySchemaValidation(t *testing.T) {
	r := NewRouter(http.NewServeMux(), errs.New())
	var gotBody bool
	err := r.RegisterFunc(Route{
		Method: "POST", Path: "/api/v1/x", Version: "v1", RateTier: TierBasic,
		Owner: "test", Status: StatusLive,
		Schema: &BodySchema{
			Required: []string{"symbol"},
			Fields:   map[string]string{"symbol": "string", "qty": "number"},
		},
	}, func(w http.ResponseWriter, req *http.Request) {
		gotBody = true
		w.WriteHeader(http.StatusOK)
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Missing required field → INVALID_REQUEST 400.
	rec := httptest.NewRecorder()
	r.Mux().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/x", strings.NewReader(`{"qty": 5}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing required field: status %d, want 400", rec.Code)
	}
	var env Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != "INVALID_REQUEST" {
		t.Fatalf("schema violation code %q, want INVALID_REQUEST", env.Error)
	}

	// Wrong type → 400.
	rec = httptest.NewRecorder()
	r.Mux().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/x", strings.NewReader(`{"symbol":"EURUSD","qty":"five"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong field type: status %d, want 400", rec.Code)
	}

	// Valid → handler runs.
	rec = httptest.NewRecorder()
	r.Mux().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/x", strings.NewReader(`{"symbol":"EURUSD","qty":5}`)))
	if rec.Code != http.StatusOK || !gotBody {
		t.Fatalf("valid body rejected: status %d", rec.Code)
	}

	// Malformed JSON → 400.
	rec = httptest.NewRecorder()
	r.Mux().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/x", strings.NewReader(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status %d", rec.Code)
	}
}

func TestWSRouteMountsAsGET(t *testing.T) {
	r := newTestRouter(t)
	rec := httptest.NewRecorder()
	// /ws/v1 is StatusLive (Task 5.3.26/31): with no handler injected into
	// MountSeed the fail-closed unwired shim answers 503 SERVICE_DEGRADED.
	r.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws/v1", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("WS unwired status %d, want 503", rec.Code)
	}
	rt, ok := r.RouteFor("WS", "/ws/v1")
	if !ok || rt.Method != "WS" {
		t.Fatalf("WS route metadata wrong: %+v ok=%v", rt, ok)
	}
}

func TestEmissionGateStartup(t *testing.T) {
	reg := errs.New()
	r := NewRouter(http.NewServeMux(), reg)
	if err := r.MountSeed(); err != nil {
		t.Fatalf("MountSeed: %v", err)
	}
	// Serve one stub + one meta route, then the emission log must be clean.
	rec := httptest.NewRecorder()
	r.Mux().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/orders", strings.NewReader(`{}`)))
	r.Mux().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/routes", nil))
	if err := reg.ValidateEmissions(); err != nil {
		t.Fatalf("emissions after serving: %v", err)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
