// Task 2.3.6 — X-Degradation-Mode middleware tests.
// Unit cases use a fake DegradationReader; the live case is gated on
// EXC_REDIS_TEST=1 (+ optional EXC_REDIS_TEST_ADDR, default 127.0.0.1:16379)
// matching the internal/redis test convention.
package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	exchredis "exchange/internal/redis"
)

type fakeReader struct {
	st  exchredis.DegradationState
	err error
}

func (f fakeReader) GetDegradationMode(context.Context) (exchredis.DegradationState, error) {
	return f.st, f.err
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func headerFor(t *testing.T, r DegradationReader) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	DegradationModeHeader(r, okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler status = %d", rec.Code)
	}
	return rec.Header().Get(DegradationHeader)
}

func TestDegradationHeader_ReportedModes(t *testing.T) {
	for _, m := range []exchredis.DegradationMode{
		exchredis.ModeNormal, exchredis.ModeReadOnly,
		exchredis.ModeMarketDataOnly, exchredis.ModeSpotOnly,
		exchredis.ModeThrottled, exchredis.ModeMaintenance,
	} {
		got := headerFor(t, fakeReader{st: exchredis.DegradationState{Mode: m}})
		if got != string(m) {
			t.Fatalf("mode %q: header = %q", m, got)
		}
	}
}

func TestDegradationHeader_AbsentKeyIsNormal(t *testing.T) {
	// The reader contract maps a missing key to ModeNormal (spec §2.4).
	got := headerFor(t, fakeReader{st: exchredis.DegradationState{Mode: exchredis.ModeNormal}})
	if got != "Normal" {
		t.Fatalf("header = %q, want Normal", got)
	}
}

func TestDegradationHeader_ReadErrorIsFailClosedMaintenance(t *testing.T) {
	// A lost mode read must never report healthy — Maintenance is the
	// strictest-safe surface (spec §2.7), same choice as the C++ ModeManager.
	got := headerFor(t, fakeReader{err: errors.New("redis down")})
	if got != "Maintenance" {
		t.Fatalf("header = %q, want Maintenance", got)
	}
	if got := headerFor(t, nil); got != "Maintenance" {
		t.Fatalf("nil reader: header = %q, want Maintenance", got)
	}
}

func TestDegradationHeader_LiveRedis(t *testing.T) {
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	c := exchredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("redis coordination instance unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() {
		c.Del(ctx, "system:degradation:mode", "system:degradation:entered_at", "system:degradation:reason")
	})

	// Write a transition the way the C++ ModeManager persists it, then read
	// it back through the middleware.
	if err := c.SetDegradationMode(ctx, exchredis.ModeReadOnly, "redis_slow"); err != nil {
		t.Fatalf("SetDegradationMode: %v", err)
	}
	if got := headerFor(t, c); got != "ReadOnly" {
		t.Fatalf("header = %q, want ReadOnly", got)
	}

	if err := c.SetDegradationMode(ctx, exchredis.ModeNormal, "recovered"); err != nil {
		t.Fatalf("SetDegradationMode: %v", err)
	}
	if got := headerFor(t, c); got != "Normal" {
		t.Fatalf("header = %q, want Normal", got)
	}
}

func TestDegradationHeader_UnreachableLiveAddrIsMaintenance(t *testing.T) {
	// A client pointed at a dead port fails closed on the first request.
	c := exchredis.New("127.0.0.1:1", "", 13)
	t.Cleanup(func() { _ = c.Close() })
	if got := headerFor(t, c); got != "Maintenance" {
		t.Fatalf("header = %q, want Maintenance", got)
	}
}

// ---------------------------------------------------------------------------
// DegradationGate — the enforcement half (Task 8.5.3.3, plan step 2:
// "order placement is rejected while market data continues streaming").
// ---------------------------------------------------------------------------

// gateEmit matches the gateway.Router.WriteError seam — the §8.7
// envelope emitter; tests assert on the emitted registry code.
func gateEmit(w http.ResponseWriter, _ *http.Request, code, message string, _ map[string]any) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":"` + code + `","message":"` + message + `"}`))
}

// gateHit serves one request through the gate and returns (status,
// emitted error code or "" when the request passed through).
func gateHit(t *testing.T, r DegradationReader, method, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	DegradationGate(r, gateEmit)(okHandler()).ServeHTTP(rec, req)
	code := ""
	if rec.Code == http.StatusServiceUnavailable {
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err == nil {
			code = body.Error
		}
	}
	return rec.Code, code
}

func TestDegradationGate_ReadOnlyRejectsWritesKeepsReads(t *testing.T) {
	r := fakeReader{st: exchredis.DegradationState{Mode: exchredis.ModeReadOnly}}

	// Order placement — the plan's leg-2 assertion — rejects 503
	// DEGRADED_MODE while market-data reads keep flowing.
	st, code := gateHit(t, r, http.MethodPost, "/api/v1/orders")
	if st != http.StatusServiceUnavailable || code != "DEGRADED_MODE" {
		t.Fatalf("POST /orders under ReadOnly = %d %q, want 503 DEGRADED_MODE", st, code)
	}
	for _, p := range []string{
		"/api/v1/orders", // reads stay open
		"/api/v1/book/EUR/USD",
		"/api/v1/ticker/EUR/USD",
		"/api/v1/account/balances", // non-market reads also stay open
	} {
		if st, _ := gateHit(t, r, http.MethodGet, p); st != http.StatusOK {
			t.Fatalf("GET %s under ReadOnly = %d, want 200", p, st)
		}
	}
	// The admin control surface stays writable — it is how an operator
	// manages and clears the mode (spec §2.4).
	if st, _ := gateHit(t, r, http.MethodPost, "/api/v1/admin/maintenance/disable"); st != http.StatusOK {
		t.Fatalf("POST /admin/maintenance/disable under ReadOnly = %d, want 200", st)
	}
	// Other mutating verbs are barred too.
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if st, _ := gateHit(t, r, m, "/api/v1/orders/123"); st != http.StatusServiceUnavailable {
			t.Fatalf("%s /orders/123 under ReadOnly = %d, want 503", m, st)
		}
	}
}

func TestDegradationGate_MarketDataOnlyKeepsOnlyMarketReads(t *testing.T) {
	r := fakeReader{st: exchredis.DegradationState{Mode: exchredis.ModeMarketDataOnly}}
	for _, p := range []string{
		"/api/v1/book/EUR/USD", "/api/v1/trades/EUR/USD",
		"/api/v1/ticker/EUR/USD", "/api/v1/klines/EUR/USD",
		"/api/v1/instruments", "/api/v1/exchange-info",
		"/api/v1/time", "/api/v1/fees", "/ws/v1/book",
		"/health", "/ready", "/metrics",
	} {
		if st, _ := gateHit(t, r, http.MethodGet, p); st != http.StatusOK {
			t.Fatalf("GET %s under MarketDataOnly = %d, want 200", p, st)
		}
	}
	// Writes always bar — even on market-data paths.
	st, code := gateHit(t, r, http.MethodPost, "/api/v1/orders")
	if st != http.StatusServiceUnavailable || code != "DEGRADED_MODE" {
		t.Fatalf("POST /orders under MarketDataOnly = %d %q, want 503 DEGRADED_MODE", st, code)
	}
	// Non-market reads (account state) are closed.
	if st, _ := gateHit(t, r, http.MethodGet, "/api/v1/account/balances"); st != http.StatusServiceUnavailable {
		t.Fatalf("GET /account/balances under MarketDataOnly = %d, want 503", st)
	}
}

func TestDegradationGate_MaintenanceClosesAllButHealth(t *testing.T) {
	r := fakeReader{st: exchredis.DegradationState{Mode: exchredis.ModeMaintenance}}
	for _, p := range []string{"/health", "/ready", "/metrics"} {
		if st, _ := gateHit(t, r, http.MethodGet, p); st != http.StatusOK {
			t.Fatalf("GET %s under Maintenance = %d, want 200 (orchestrator probes survive)", p, st)
		}
	}
	for _, p := range []string{"/api/v1/orders", "/api/v1/book/EUR/USD", "/ws/v1/book"} {
		st, code := gateHit(t, r, http.MethodGet, p)
		if st != http.StatusServiceUnavailable || code != "MAINTENANCE_MODE" {
			t.Fatalf("GET %s under Maintenance = %d %q, want 503 MAINTENANCE_MODE", p, st, code)
		}
	}
	st, code := gateHit(t, r, http.MethodPost, "/api/v1/orders")
	if st != http.StatusServiceUnavailable || code != "MAINTENANCE_MODE" {
		t.Fatalf("POST /orders under Maintenance = %d %q", st, code)
	}
}

func TestDegradationGate_NormalThrottledSpotOnlyPass(t *testing.T) {
	// Normal/Throttled/SpotOnly impose no REST gate (Throttled is the
	// rate-limit multiplier's job; SpotOnly has no additional surface —
	// every listed instrument is spot FX).
	for _, m := range []exchredis.DegradationMode{
		exchredis.ModeNormal, exchredis.ModeThrottled, exchredis.ModeSpotOnly,
	} {
		r := fakeReader{st: exchredis.DegradationState{Mode: m}}
		if st, _ := gateHit(t, r, http.MethodPost, "/api/v1/orders"); st != http.StatusOK {
			t.Fatalf("POST /orders under %s = %d, want 200 (gate passes)", m, st)
		}
	}
}

func TestDegradationGate_FailClosedOnUnreadableMode(t *testing.T) {
	// spec §2.7: an unverifiable mode must never read as healthy — a
	// lost read (or absent reader) behaves exactly like Maintenance.
	for _, r := range []DegradationReader{
		fakeReader{err: errors.New("redis down")},
		nil,
	} {
		st, code := gateHit(t, r, http.MethodPost, "/api/v1/orders")
		if st != http.StatusServiceUnavailable || code != "MAINTENANCE_MODE" {
			t.Fatalf("unreadable mode: POST /orders = %d %q, want 503 MAINTENANCE_MODE", st, code)
		}
		if st, _ := gateHit(t, r, http.MethodGet, "/health"); st != http.StatusOK {
			t.Fatalf("unreadable mode: GET /health = %d, want 200", st)
		}
	}
}

func TestDegradationGate_LiveRedisModeRecord(t *testing.T) {
	// Plan step 2 on the real wire path: the Phase-02 ModeManager's
	// Redis record drives the gate — write ReadOnly, verify writes
	// reject while reads continue; clear, verify writes re-admitted.
	if os.Getenv("EXC_REDIS_TEST") != "1" {
		t.Skip("set EXC_REDIS_TEST=1 to run Redis integration tests")
	}
	addr := os.Getenv("EXC_REDIS_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	c := exchredis.New(addr, os.Getenv("EXC_REDIS_TEST_PASSWORD"), 13)
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("redis coordination instance unreachable at %s: %v", addr, err)
	}
	t.Cleanup(func() {
		c.Del(ctx, "system:degradation:mode", "system:degradation:entered_at", "system:degradation:reason")
	})

	if err := c.SetDegradationMode(ctx, exchredis.ModeReadOnly, "pg_replica_lag"); err != nil {
		t.Fatalf("SetDegradationMode: %v", err)
	}
	st, code := gateHit(t, c, http.MethodPost, "/api/v1/orders")
	if st != http.StatusServiceUnavailable || code != "DEGRADED_MODE" {
		t.Fatalf("live ReadOnly: POST /orders = %d %q, want 503 DEGRADED_MODE", st, code)
	}
	if st, _ := gateHit(t, c, http.MethodGet, "/api/v1/time"); st != http.StatusOK {
		t.Fatalf("live ReadOnly: GET /time = %d — reads must continue", st)
	}

	if err := c.SetDegradationMode(ctx, exchredis.ModeNormal, "recovered"); err != nil {
		t.Fatalf("SetDegradationMode: %v", err)
	}
	if st, _ := gateHit(t, c, http.MethodPost, "/api/v1/orders"); st != http.StatusOK {
		t.Fatalf("live Normal: POST /orders = %d, want gate pass-through", st)
	}
}
