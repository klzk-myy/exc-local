package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"exchange/internal/errs"
)

// fakeClock is a manually advanced clock for deterministic windows.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time      { return f.t }
func (f *fakeClock) add(d time.Duration) { f.t = f.t.Add(d) }

func breakerForTest(f *fakeClock) *Breaker {
	cfg := DefaultBreakerConfig()
	cfg.MinSamples = 10 // shrink for the test; threshold 0.15, window 10s, backoff 5s
	cfg.Now = f.now
	return NewBreaker(cfg)
}

func serveBreaker(b *Breaker, r *Router, handlerStatus int, n int) (last *httptest.ResponseRecorder) {
	h := b.Wrap(r, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(handlerStatus)
	}))
	for i := 0; i < n; i++ {
		last = httptest.NewRecorder()
		h.ServeHTTP(last, httptest.NewRequest("GET", "/x", nil))
	}
	return last
}

func TestBreakerTripsAbove15Percent(t *testing.T) {
	f := &fakeClock{t: time.Unix(1_000_000, 0)}
	b := breakerForTest(f)
	r := NewRouter(http.NewServeMux(), errs.New())

	// Warm the window with successes, then interleave 8 failures among
	// them — cumulative error ratio stays < 15% at every observation:
	// after the k-th failure errs=k of total=60+9k (≤8/132 ≈ 6%).
	serveBreaker(b, r, 200, 60)
	for i := 0; i < 8; i++ {
		serveBreaker(b, r, 500, 1)
		f.add(50 * time.Millisecond)
		serveBreaker(b, r, 200, 8)
		f.add(50 * time.Millisecond)
	}
	if b.State() != "closed" {
		t.Fatalf("breaker tripped below 15%%: %s", b.State())
	}

	// Push failures to >15% of the window total (needs >~19.6 more).
	for i := 0; i < 25; i++ {
		serveBreaker(b, r, 500, 1)
		f.add(10 * time.Millisecond)
	}
	if b.State() != "open" {
		t.Fatalf("breaker did not trip: state=%s", b.State())
	}

	// Open: rejected with SERVICE_DEGRADED 503 + Retry-After.
	rec := serveBreaker(b, r, 200, 1)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("open breaker status %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After %q, want 5", rec.Header().Get("Retry-After"))
	}
	var env Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error != "SERVICE_DEGRADED" {
		t.Fatalf("open envelope code %q", env.Error)
	}
}

func TestBreakerHalfOpenProbe(t *testing.T) {
	f := &fakeClock{t: time.Unix(2_000_000, 0)}
	b := breakerForTest(f)
	r := NewRouter(http.NewServeMux(), errs.New())

	serveBreaker(b, r, 500, 10) // 100% errors → open
	if b.State() != "open" {
		t.Fatalf("expected open, got %s", b.State())
	}

	// Within backoff: still open.
	f.add(2 * time.Second)
	if rec := serveBreaker(b, r, 200, 1); rec.Code != 503 {
		t.Fatalf("during backoff status %d", rec.Code)
	}

	// After backoff: probe admitted; success closes the breaker.
	f.add(4 * time.Second)
	var probeRan atomic.Bool
	h := b.Wrap(r, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probeRan.Store(true)
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if !probeRan.Load() || rec.Code != 200 {
		t.Fatal("half-open probe not admitted")
	}
	if b.State() != "closed" {
		t.Fatalf("successful probe did not close: %s", b.State())
	}
}

func TestBreakerFailedProbeReopens(t *testing.T) {
	f := &fakeClock{t: time.Unix(3_000_000, 0)}
	b := breakerForTest(f)
	r := NewRouter(http.NewServeMux(), errs.New())

	serveBreaker(b, r, 500, 10)
	f.add(6 * time.Second)
	serveBreaker(b, r, 500, 1) // probe fails
	if b.State() != "open" {
		t.Fatalf("failed probe should re-open: %s", b.State())
	}
	// Second probe while first re-opened: rejected.
	if rec := serveBreaker(b, r, 200, 1); rec.Code != 503 {
		t.Fatalf("re-opened breaker admitted request: %d", rec.Code)
	}
}

func TestBreakerIgnores4xx(t *testing.T) {
	f := &fakeClock{t: time.Unix(4_000_000, 0)}
	b := breakerForTest(f)
	r := NewRouter(http.NewServeMux(), errs.New())
	// Client errors are not upstream failures — never trip on 4xx.
	serveBreaker(b, r, 400, 30)
	if b.State() != "closed" {
		t.Fatalf("4xx tripped the breaker: %s", b.State())
	}
}
