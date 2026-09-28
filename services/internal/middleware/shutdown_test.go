package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRunStepsOrderAndBounds(t *testing.T) {
	var seq []string
	steps := []Step{
		{Name: "a", Fn: func(context.Context) error { seq = append(seq, "a"); return nil }},
		{Name: "b", Fn: func(context.Context) error { seq = append(seq, "b"); return errors.New("boom") }},
		{Name: "c", Fn: func(context.Context) error { seq = append(seq, "c"); return nil }},
	}
	err := RunSteps(context.Background(), slog.Default(), steps)
	if len(seq) != 3 || seq[0] != "a" || seq[2] != "c" {
		t.Fatalf("steps out of order or truncated: %v", seq)
	}
	if err == nil {
		t.Fatal("failed step error not reported")
	}
}

// A step that ignores its deadline is still bounded by the caller's ctx;
// a step respecting ctx exits promptly.
func TestRunStepTimeout(t *testing.T) {
	steps := []Step{{
		Name: "hang", Timeout: 50 * time.Millisecond,
		Fn: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		}}}
	start := time.Now()
	_ = RunSteps(context.Background(), slog.Default(), steps)
	if time.Since(start) > 2*time.Second {
		t.Fatal("step outlived its timeout")
	}
}

func TestDrainFlag(t *testing.T) {
	f := &DrainFlag{}
	if f.Draining() {
		t.Fatal("draining before Set")
	}
	f.Set()
	f.Set() // idempotent
	if !f.Draining() {
		t.Fatal("not draining after Set")
	}
}

func TestReadyGate(t *testing.T) {
	f := &DrainFlag{}
	emit := func(w http.ResponseWriter, _ *http.Request, code, _ string, _ map[string]any) {
		if code != "MAINTENANCE_MODE" {
			t.Fatalf("code = %q", code)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	h := ReadyGate(f, emit, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != 200 {
		t.Fatal("ready gate closed before drain")
	}
	f.Set()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatal("readiness stayed healthy during drain")
	}
}

// While draining: new writes refuse, cancels + reads still pass.
func TestRejectWhenDraining(t *testing.T) {
	f := &DrainFlag{}
	f.Set()
	var code string
	emit := func(w http.ResponseWriter, _ *http.Request, c, _ string, _ map[string]any) {
		code = c
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := RejectWhenDraining(f, emit)(ok)

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/api/v1/orders", 503},
		{http.MethodGet, "/api/v1/account/balances", 503},
		{http.MethodDelete, "/api/v1/orders/7", 200}, // cancel survives
		{http.MethodGet, "/api/v1/orders/7", 200},    // status read survives
		{http.MethodGet, "/health", 200},
		{http.MethodGet, "/api/v1/system/status", 200},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Fatalf("%s %s → %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
	_ = code
}
