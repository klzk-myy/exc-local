package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"exchange/internal/ratelimit"
)

func shedderAt(depth int64) *Shedder {
	d := depth
	s := NewShedder(ShedConfig{Depth: func() int64 { return d }})
	s.Sample()
	return s
}

// Task AC: shedding activates at queue depth > 500.
func TestShedActivatesAbove500(t *testing.T) {
	if s := shedderAt(500); s.Stage() != 0 {
		t.Fatalf("depth 500 → stage %d, want 0", s.Stage())
	}
	if s := shedderAt(501); s.Stage() != 1 {
		t.Fatalf("depth 501 → stage %d, want 1", s.Stage())
	}
	if s := shedderAt(2500); s.Stage() != 2 {
		t.Fatalf("depth 2500 → stage %d, want 2", s.Stage())
	}
	if s := shedderAt(9000); s.Stage() != 3 {
		t.Fatalf("depth 9000 → stage %d, want 3", s.Stage())
	}
}

// Task AC: recovery when queue < 250; hysteresis holds between 250–500.
func TestShedHysteresis(t *testing.T) {
	var depth int64 = 3000
	s := NewShedder(ShedConfig{Depth: func() int64 { return depth }})
	s.Sample()
	if s.Stage() != 2 {
		t.Fatalf("stage = %d, want 2", s.Stage())
	}
	depth = 400 // below entry, above exit — stage holds (hysteresis)
	s.Sample()
	if s.Stage() != 2 {
		t.Fatalf("hysteresis band cleared stage %d", s.Stage())
	}
	depth = 249
	s.Sample()
	if s.Stage() != 0 {
		t.Fatalf("depth 249 → stage %d, want 0", s.Stage())
	}
}

// Task AC: lowest tiers shed first; institutional/admin never shed.
func TestShedTierOrder(t *testing.T) {
	s := shedderAt(3000) // stage 2 → public/basic/standard eligible
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", nil)
	s.shedCtr[2].Store(0)
	if !s.ShouldShed(req, ratelimit.TierBasic) {
		t.Fatal("basic not shed at stage 2 (first request in cycle)")
	}
	if !s.ShouldShed(req, ratelimit.TierPublic) {
		t.Fatal("public not shed at stage 2")
	}
	if !s.ShouldShed(req, ratelimit.TierStandard) {
		t.Fatal("standard not shed at stage 2")
	}
	if s.ShouldShed(req, ratelimit.TierInstitutional) {
		t.Fatal("institutional shed — never allowed")
	}
	if s.ShouldShed(req, ratelimit.TierAdmin) {
		t.Fatal("admin shed — never allowed")
	}
	// Stage 1 does not reach standard.
	s1 := shedderAt(600)
	for i := 0; i < 100; i++ {
		if s1.ShouldShed(req, ratelimit.TierStandard) {
			t.Fatal("standard shed at stage 1")
		}
	}
}

// Deterministic fractional shed: 25% band drops every 4th eligible
// request exactly.
func TestShedFractionDeterministic(t *testing.T) {
	s := shedderAt(3000) // stage 2, 25%
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/balances", nil)
	var shed int
	for i := 0; i < 100; i++ {
		if s.ShouldShed(req, ratelimit.TierPublic) {
			shed++
		}
	}
	if shed != 25 {
		t.Fatalf("shed %d/100, want 25", shed)
	}
}

// Cancel requests are unconditionally exempt — at any depth.
func TestCancelExempt(t *testing.T) {
	for _, path := range []string{
		"/api/v1/orders/123", "/api/v1/orders",
		"/api/v1/admin/orders/mass-cancel",
	} {
		s := shedderAt(100_000)
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		for i := 0; i < 50; i++ {
			if s.ShouldShed(req, ratelimit.TierPublic) {
				t.Fatalf("cancel %s shed", path)
			}
		}
	}
	// FIX-originated cancels: header + ctx marker.
	s := shedderAt(100_000)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders/cancel", nil)
	req.Header.Set(HeaderPriority, PriorityCancelExempt)
	if s.ShouldShed(req, ratelimit.TierPublic) {
		t.Fatal("header-tagged cancel shed")
	}
	ctxReq := httptest.NewRequest(http.MethodPost, "/api/v1/orders/cancel", nil)
	ctxReq = ctxReq.WithContext(WithCancelExempt(ctxReq.Context()))
	if s.ShouldShed(ctxReq, ratelimit.TierPublic) {
		t.Fatal("ctx-tagged cancel shed")
	}
}

// §2.7 watermarks: ≥80% capacity → max stage; ≥95% → ENGINE_OVERLOAD.
func TestCapacityWatermarks(t *testing.T) {
	var depth int64
	s := NewShedder(ShedConfig{
		Depth: func() int64 { return depth }, Capacity: 1000})
	depth = 850 // 85% → stage 3 regardless of absolute bands
	s.Sample()
	if s.Stage() != 3 {
		t.Fatalf("85%% capacity → stage %d, want 3", s.Stage())
	}
	depth = 960 // 96% → overload
	s.Sample()
	if !s.Overload() {
		t.Fatal("96% capacity did not enter overload")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/balances", nil)
	if !s.ShouldShed(req, ratelimit.TierInstitutional) {
		t.Fatal("overload must shed even institutional non-cancel traffic")
	}
	cancel := httptest.NewRequest(http.MethodDelete, "/api/v1/orders/9", nil)
	if s.ShouldShed(cancel, ratelimit.TierInstitutional) {
		t.Fatal("overload shed a cancel — zero-drop contract violated")
	}
	depth = 100
	s.Sample()
	if s.Overload() || s.Stage() != 0 {
		t.Fatal("did not recover below exit depth")
	}
}

// Health/metrics/status paths are never shed.
func TestShedObservabilityExempt(t *testing.T) {
	s := shedderAt(50_000)
	for _, p := range []string{"/health", "/ready", "/health/ready", "/metrics",
		"/api/v1/system/status", "/ws/v1"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		if s.ShouldShed(req, ratelimit.TierPublic) {
			t.Fatalf("%s shed", p)
		}
	}
}

// A depth gauge that cannot report holds a conservative shed posture —
// never silently healthy (spec §2.7 pessimism).
func TestShedMissingDepth(t *testing.T) {
	s := NewShedder(ShedConfig{Depth: nil})
	if s.Stage() != 1 {
		t.Fatalf("nil depth source → stage %d, want 1", s.Stage())
	}
	s2 := NewShedder(ShedConfig{Depth: func() int64 { return -1 }})
	s2.Sample()
	if s2.Stage() != 1 || !s2.Stats().DepthErr {
		t.Fatalf("negative depth → stage %d", s2.Stage())
	}
}

// Middleware: 503 CAPACITY_EXCEEDED inside bands, ENGINE_OVERLOAD at 95%.
func TestSheddingMiddlewareCodes(t *testing.T) {
	var captured string
	emit := func(w http.ResponseWriter, _ *http.Request, code, _ string, _ map[string]any) {
		captured = code
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	var depth int64 = 3000
	s := NewShedder(ShedConfig{Depth: func() int64 { return depth }})
	s.Sample()
	h := Shedding(s, ShedOptions{Emit: emit})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/orders", nil))
	}
	if captured != "CAPACITY_EXCEEDED" {
		t.Fatalf("band shed emitted %q", captured)
	}

	depth = 0
	var depth2 int64 = 970
	s2 := NewShedder(ShedConfig{Depth: func() int64 { return depth2 }, Capacity: 1000})
	s2.Sample()
	h2 := Shedding(s2, ShedOptions{Emit: emit})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	rec := httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/account/balances", nil))
	if captured != "ENGINE_OVERLOAD" {
		t.Fatalf("overload emitted %q, want ENGINE_OVERLOAD", captured)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}
