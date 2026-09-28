// http_test.go — HTTP middleware + tier classification + service gauges.
package observability

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveViaMiddleware(t *testing.T, m *Metrics, pattern string, code int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	srv := httptest.NewServer(m.HTTPMiddleware(mux))
	defer srv.Close()
	path := pattern
	if i := strings.IndexByte(path, ' '); i >= 0 {
		path = path[i+1:]
	}
	path = strings.ReplaceAll(path, "{id}", "7")
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	resp.Body.Close()
	if resp.StatusCode != code {
		t.Fatalf("status = %d want %d", resp.StatusCode, code)
	}
}

func TestHTTPMiddlewareCountsAndTiers(t *testing.T) {
	reg := New()
	m := NewMetrics(reg, "gateway")

	serveViaMiddleware(t, m, "GET /ok", http.StatusOK)
	serveViaMiddleware(t, m, "GET /ok", http.StatusOK)
	serveViaMiddleware(t, m, "GET /bad", http.StatusUnprocessableEntity)  // L2
	serveViaMiddleware(t, m, "GET /limited", http.StatusTooManyRequests)  // L3
	serveViaMiddleware(t, m, "GET /oops", http.StatusInternalServerError) // L1

	_, samples := parseExposition(t, reg.String())

	s, ok := findSample(samples, MetricHTTPRequestsTotal, map[string]string{
		"service": "gateway", "method": "GET", "route": "/ok", "code": "200"})
	if !ok || s.value != 2 {
		t.Fatalf("/ok requests = %v (found=%v)", s.value, ok)
	}
	// route label uses the registered pattern, not the raw path
	s, ok = findSample(samples, MetricHTTPRequestsTotal, map[string]string{
		"service": "gateway", "code": "422"})
	if !ok || s.labels["route"] != "/bad" || s.value != 1 {
		t.Fatalf("/bad requests = %v labels=%v", s.value, s.labels)
	}
	if v := m.ErrorCount(TierL2); v != 1 {
		t.Fatalf("L2 errors = %v want 1", v)
	}
	if v := m.ErrorCount(TierL3); v != 1 {
		t.Fatalf("L3 errors = %v want 1", v)
	}
	if v := m.ErrorCount(TierL1); v != 1 {
		t.Fatalf("L1 errors = %v want 1", v)
	}
	// duration histogram recorded 5 observations
	s, ok = findSample(samples, MetricHTTPDurationSeconds+"_count", map[string]string{
		"service": "gateway", "method": "GET", "route": "/ok"})
	if !ok || s.value != 2 {
		t.Fatalf("duration count = %v (found=%v)", s.value, ok)
	}
}

func TestHTTPMiddlewareUnmatchedRoute(t *testing.T) {
	reg := New()
	m := NewMetrics(reg, "svc")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /only", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(m.HTTPMiddleware(mux))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/nowhere")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, samples := parseExposition(t, reg.String())
	s, ok := findSample(samples, MetricHTTPRequestsTotal,
		map[string]string{"route": "unmatched", "code": "404"})
	if !ok || s.value != 1 {
		t.Fatalf("unmatched count = %v (found=%v)", s.value, ok)
	}
}

func TestTierForStatusMapping(t *testing.T) {
	cases := map[int]string{
		200: "", 201: "",
		400: TierL3, 401: TierL3, 403: TierL3, 404: TierL3,
		418: TierL3, 429: TierL3,
		409: TierL2, 422: TierL2,
		500: TierL1, 502: TierL1, 503: TierL1,
	}
	for code, want := range cases {
		if got := TierForStatus(code); got != want {
			t.Fatalf("TierForStatus(%d) = %q want %q", code, got, want)
		}
	}
}

func TestServiceGauges(t *testing.T) {
	reg := New()
	m := NewMetrics(reg, "svc")

	m.SetDegradationMode("ReadOnly")
	m.SetCircuitBreaker("ingest", true)
	m.SetIPCRingDepth(2, 512)
	m.SetIPCLastSeq(2, 9001)
	m.SetWALLag(2, 33)
	m.ObserveReconciliation("wallet", 2)
	m.ObserveReconciliation("wallet", 0)

	close1 := m.TrackWSConnect()
	m.TrackWSConnect()()
	close1()

	_, samples := parseExposition(t, reg.String())
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{MetricDegradationMode, map[string]string{"mode": "ReadOnly"}, 1},
		{MetricDegradationMode, map[string]string{"mode": "Normal"}, 0},
		{MetricCircuitBreakerOpen, map[string]string{"name": "ingest"}, 1},
		{MetricIPCRingDepth, map[string]string{"shard": "2"}, 512},
		{MetricIPCLastSeq, map[string]string{"shard": "2"}, 9001},
		{MetricWALLagEntries, map[string]string{"shard": "2"}, 33},
		{MetricReconcileRunsTotal, map[string]string{"kind": "wallet"}, 2},
		{MetricReconcileMismatchTotal, map[string]string{"kind": "wallet"}, 2},
		{MetricWSConnsTotal, map[string]string{"service": "svc"}, 2},
		{MetricWSConnsActive, map[string]string{"service": "svc"}, 0},
	}
	for _, c := range checks {
		s, ok := findSample(samples, c.name, c.labels)
		if !ok || s.value != c.want {
			t.Fatalf("%s %v = %v want %v (found=%v)", c.name, c.labels, s.value, c.want, ok)
		}
	}
}

func ExampleMetrics_HTTPMiddleware() {
	reg := New()
	m := NewMetrics(reg, "demo")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	m.HTTPMiddleware(mux).ServeHTTP(rec, req)
	fmt.Println(rec.Code, m.ErrorCount(TierL1))
	// Output: 503 1
}
