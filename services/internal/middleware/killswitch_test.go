// Unit tests for the Task 11.3.4 HTTP admission gate: global-halt
// rejection, the cancel exemption lane, and fail-closed lookup errors.
package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeKillReader struct {
	halted bool
	err    error
}

func (f fakeKillReader) GlobalHalted(context.Context) (bool, error) {
	return f.halted, f.err
}

// runGate drives the gate and reports (reachedInner, emittedCode).
func runGate(t *testing.T, reader KillSwitchReader, method, path string) (bool, string) {
	t.Helper()
	var emitted string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	emit := func(w http.ResponseWriter, r *http.Request, code, msg string, details map[string]any) {
		emitted = code
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	h := KillSwitchGate(reader, emit)(inner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	reached := false
	h.ServeHTTP(rec, req)
	reached = rec.Code == http.StatusNoContent
	return reached, emitted
}

func TestKillSwitchGate_RejectsNewOrdersWhenHalted(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/orders"},
		{http.MethodPost, "/api/v1/orders/batch"},
		{http.MethodPut, "/api/v1/orders/123"},
		{http.MethodPatch, "/api/v1/orders/123"},
	} {
		reached, code := runGate(t, fakeKillReader{halted: true}, tc.method, tc.path)
		if reached || code != "TRADING_HALTED" {
			t.Fatalf("%s %s: halted gate must reject admission, reached=%v code=%q",
				tc.method, tc.path, reached, code)
		}
	}
}

func TestKillSwitchGate_CancelsAndReadsSurvive(t *testing.T) {
	halted := fakeKillReader{halted: true}
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/orders/123"},             // single cancel
		{http.MethodDelete, "/api/v1/orders"},                 // client mass cancel
		{http.MethodPost, "/api/v1/admin/orders/mass-cancel"}, // admin mass cancel
		{http.MethodGet, "/api/v1/orders"},                    // reads
		{http.MethodGet, "/api/v1/orders/123"},
		{http.MethodPost, "/api/v1/orders/test"},                  // dry-run
		{http.MethodPost, "/api/v1/orders/5/amend/keep-priority"}, // qty-down amend
		{http.MethodPost, "/api/v1/admin/kill-switch"},            // control plane
		{http.MethodPost, "/api/v1/funding/withdrawals"},          // funding unaffected
	} {
		reached, code := runGate(t, halted, tc.method, tc.path)
		if !reached {
			t.Fatalf("%s %s: must pass under halt (emitted %q)", tc.method, tc.path, code)
		}
	}
}

func TestKillSwitchGate_CancelExemptHeaderBypass(t *testing.T) {
	// The CANCEL_EXEMPT lane (X-Exc-Priority or ctx marker, Task 5.3.25)
	// must pass even on a POST that otherwise looks like order entry.
	halted := fakeKillReader{halted: true}
	var emitted string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	emit := func(w http.ResponseWriter, r *http.Request, code, msg string, details map[string]any) {
		emitted = code
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	h := KillSwitchGate(halted, emit)(inner)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/cancel-replace", nil)
	req.Header.Set("X-Exc-Priority", PriorityCancelExempt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || emitted != "" {
		t.Fatalf("CANCEL_EXEMPT header must bypass the halt gate (code=%q)", emitted)
	}
}

func TestKillSwitchGate_FailClosed(t *testing.T) {
	// Nil reader and lookup errors both reject admission — an
	// unverifiable halt flag never admits new orders (spec §2.7).
	reached, code := runGate(t, nil, http.MethodPost, "/api/v1/orders")
	if reached || code != "TRADING_HALTED" {
		t.Fatalf("nil reader must fail closed, reached=%v code=%q", reached, code)
	}
	reached, code = runGate(t,
		fakeKillReader{err: errors.New("redis gone")},
		http.MethodPost, "/api/v1/orders")
	if reached || code != "TRADING_HALTED" {
		t.Fatalf("lookup error must fail closed, reached=%v code=%q", reached, code)
	}
	// ...but errors only gate admission paths — reads still pass.
	reached, _ = runGate(t, fakeKillReader{err: errors.New("redis gone")},
		http.MethodGet, "/api/v1/orders")
	if !reached {
		t.Fatal("read path must not consult the halt flag")
	}
}

func TestKillSwitchGate_OpenPasses(t *testing.T) {
	reached, code := runGate(t, fakeKillReader{halted: false},
		http.MethodPost, "/api/v1/orders")
	if !reached || code != "" {
		t.Fatalf("open venue must admit: reached=%v code=%q", reached, code)
	}
}
