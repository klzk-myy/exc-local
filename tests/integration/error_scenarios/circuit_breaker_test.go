// Task 8.3.5 scenario 5 — gateway circuit breaker after sustained 5xx
// (spec §2.7.2 tier L1, Task 5.3.41 step 3).
//
// Contract asserted against the REAL gateway.Breaker + emission gate:
//   - ≥20 requests with a >15% 5xx rate inside the rolling window trip
//     the breaker (closed → open).
//   - While open, requests never reach upstream and are rejected
//     503 SERVICE_DEGRADED with Retry-After and the §8.7 envelope.
//   - Below MinSamples, even 100% failure must NOT trip (single-burst
//     protection).
//   - An error ratio at exactly the threshold must NOT trip (the
//     comparator is strict >).
//   - After Backoff the breaker half-opens: one probe admitted; probe
//     success → closed + window reset; probe failure → re-open.
//
// The clock is injected (BreakerConfig.Now) so backoff/half-open are
// deterministic — no real 5s sleeps.
package error_scenarios

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"exchange/internal/gateway"
)

// breakerHarness wires a breaker over an injected clock and a status-
// script upstream.
type breakerHarness struct {
	b       *gateway.Breaker
	h       http.Handler
	calls   *atomic.Int64
	setStat *atomic.Int32
	now     *time.Time // guarded by atomic-free usage (single goroutine)
}

func newBreakerHarness(t *testing.T, cfg gateway.BreakerConfig, status int) *breakerHarness {
	t.Helper()
	base := time.Unix(1_700_000_000, 0)
	h := &breakerHarness{now: &base}
	cfg.Now = func() time.Time { return *h.now }
	h.b = gateway.NewBreaker(cfg)
	h.calls = &atomic.Int64{}
	h.setStat = &atomic.Int32{}
	h.setStat.Store(int32(status))
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.calls.Add(1)
		w.WriteHeader(int(h.setStat.Load()))
	})
	h.h = h.b.Wrap(newTestRouter(), upstream)
	return h
}

func (h *breakerHarness) serve() *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/orders", nil))
	return rec
}

func (h *breakerHarness) advance(d time.Duration) { *h.now = h.now.Add(d) }

// TestCircuitBreaker_TripsOnSustained5xx is the §24 #307 L1 case: a
// sustained upstream 5xx stream trips the local breaker and every later
// request is shed with the degraded envelope.
func TestCircuitBreaker_TripsOnSustained5xx(t *testing.T) {
	cfg := gateway.DefaultBreakerConfig() // 10s window, 15% @ ≥20 samples, 5s backoff
	h := newBreakerHarness(t, cfg, http.StatusInternalServerError)

	// Drive MinSamples consecutive 500s — the "sustained 5xx" fault. The
	// breaker evaluates AFTER each response, so request MinSamples is the
	// last one upstream sees; everything after is shed.
	for i := 0; i < cfg.MinSamples; i++ {
		rec := h.serve()
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("request %d: upstream pass-through broke: %d", i, rec.Code)
		}
	}
	if got := h.b.State(); got != "open" {
		t.Fatalf("breaker state=%q after %d consecutive 5xx, want open",
			got, cfg.MinSamples)
	}
	callsAtOpen := h.calls.Load()

	// Open-state shedding: upstream must NOT be invoked again.
	for i := 0; i < 5; i++ {
		rec := h.serve()
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("shed request %d: status=%d, want 503", i, rec.Code)
		}
		if ra := rec.Header().Get("Retry-After"); ra != "5" {
			t.Fatalf("shed request %d: Retry-After=%q, want 5 (backoff seconds)", i, ra)
		}
		p := decodeProblem(t, rec)
		if p.Error != "SERVICE_DEGRADED" {
			t.Fatalf("shed request %d: code=%s, want SERVICE_DEGRADED", i, p.Error)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("shed request %d: content-type=%q", i, ct)
		}
	}
	if h.calls.Load() != callsAtOpen {
		t.Fatalf("open breaker admitted upstream traffic: calls %d→%d",
			callsAtOpen, h.calls.Load())
	}
}

// TestCircuitBreaker_MinSamplesGuard asserts the fail-safe direction: a
// burst below MinSamples never trips, no matter the error ratio.
func TestCircuitBreaker_MinSamplesGuard(t *testing.T) {
	cfg := gateway.DefaultBreakerConfig()
	h := newBreakerHarness(t, cfg, http.StatusBadGateway)

	for i := 0; i < cfg.MinSamples-1; i++ { // 19 all-502 samples
		h.serve()
	}
	if got := h.b.State(); got != "closed" {
		t.Fatalf("breaker tripped at %d samples (< MinSamples %d): %q",
			cfg.MinSamples-1, cfg.MinSamples, got)
	}
}

// TestCircuitBreaker_ThresholdIsStrict asserts ratio == threshold keeps
// the breaker closed (spec: "exceeds 15%", not "reaches").
func TestCircuitBreaker_ThresholdIsStrict(t *testing.T) {
	cfg := gateway.DefaultBreakerConfig()
	h := newBreakerHarness(t, cfg, http.StatusOK)

	// 17×200 + 3×500 = 3/20 = exactly 15%.
	for i := 0; i < 17; i++ {
		h.serve()
	}
	h.setStat.Store(http.StatusInternalServerError)
	for i := 0; i < 3; i++ {
		h.serve()
	}
	if got := h.b.State(); got != "closed" {
		t.Fatalf("breaker tripped at exactly 15%% error rate: %q", got)
	}

	// One more failure pushes 4/21 ≈ 19% > 15% → trips.
	h.serve()
	if got := h.b.State(); got != "open" {
		t.Fatalf("breaker did not trip once error rate exceeded 15%%: %q", got)
	}
}

// TestCircuitBreaker_HalfOpenProbe walks the full recovery ladder with
// the injected clock: open → backoff → probe admitted → probe 500 →
// re-open → backoff → probe 200 → closed, with the window reset.
func TestCircuitBreaker_HalfOpenProbe(t *testing.T) {
	cfg := gateway.DefaultBreakerConfig()
	cfg.Backoff = 5 * time.Second
	h := newBreakerHarness(t, cfg, http.StatusInternalServerError)

	for i := 0; i < cfg.MinSamples+1; i++ {
		h.serve()
	}
	if h.b.State() != "open" {
		t.Fatalf("precondition: breaker not open, got %q", h.b.State())
	}

	// Inside backoff: still rejecting.
	h.advance(4 * time.Second)
	if rec := h.serve(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("inside backoff: status=%d, want 503", rec.Code)
	}
	callsAtOpen := h.calls.Load()

	// Backoff elapsed → exactly one probe admitted.
	h.advance(2 * time.Second) // now = openedAt + 6s
	h.setStat.Store(http.StatusInternalServerError)
	if rec := h.serve(); rec.Code != http.StatusInternalServerError {
		t.Fatalf("half-open probe: status=%d, want upstream 500", rec.Code)
	}
	if h.calls.Load() != callsAtOpen+1 {
		t.Fatalf("half-open probe did not reach upstream")
	}
	if got := h.b.State(); got != "open" {
		t.Fatalf("failed probe: state=%q, want re-open", got)
	}

	// Second backoff: a concurrent-looking second request while the next
	// probe slot is pending is still rejected... after re-open, shed
	// resumes until the new backoff lapses.
	h.advance(5*time.Second + time.Millisecond)
	h.setStat.Store(http.StatusOK)
	if rec := h.serve(); rec.Code != http.StatusOK {
		t.Fatalf("recovery probe: status=%d, want upstream 200", rec.Code)
	}
	if got := h.b.State(); got != "closed" {
		t.Fatalf("successful probe: state=%q, want closed (window reset)", got)
	}

	// Post-recovery traffic flows and the window is clean: 19 fresh 500s
	// do not re-trip (MinSamples resets with the probe success).
	h.setStat.Store(http.StatusInternalServerError)
	for i := 0; i < cfg.MinSamples-1; i++ {
		h.serve()
	}
	if got := h.b.State(); got != "closed" {
		t.Fatalf("stale window after recovery tripped breaker: %q", got)
	}
}

// TestCircuitBreaker_ShedEnvelopeFields nails the §8.7 envelope of an
// open-state rejection: type/error/status/request_id/timestamp plus a
// numeric retry_after detail.
func TestCircuitBreaker_ShedEnvelopeFields(t *testing.T) {
	cfg := gateway.DefaultBreakerConfig()
	h := newBreakerHarness(t, cfg, http.StatusInternalServerError)
	for i := 0; i < cfg.MinSamples+1; i++ {
		h.serve()
	}

	rec := h.serve()
	p := decodeProblem(t, rec)
	if p.Error != "SERVICE_DEGRADED" || p.Status != http.StatusServiceUnavailable {
		t.Fatalf("shed envelope: code=%s status=%d", p.Error, p.Status)
	}
	ra, ok := p.Details["retry_after"]
	if !ok {
		t.Fatal("shed envelope missing details.retry_after")
	}
	if n, ok := ra.(float64); !ok || int(n) != 5 {
		t.Fatalf("details.retry_after=%v, want 5", ra)
	}
	if rec.Header().Get("Retry-After") != strconv.Itoa(int(cfg.Backoff/time.Second)) {
		t.Fatalf("Retry-After header mismatch: %q", rec.Header().Get("Retry-After"))
	}
}
