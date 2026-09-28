package deprecation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Telemetry: announced-route hits report Gone=false; post-sunset
// attempts report Gone=true. The request path and headers are
// unchanged — telemetry is pure observation.
func TestMiddlewareTelemetry(t *testing.T) {
	var hits []Hit
	sink := func(_ context.Context, h Hit) { hits = append(hits, h) }

	announced := futureRule()
	gone := futureRule()
	gone.Path = "/api/v1/old-gone"
	gone.AnnouncedAt = time.Now().Add(-MinNotice - time.Hour)
	gone.SunsetAt = time.Now().Add(-time.Minute)

	src := &staticRules{rules: []Rule{announced, gone}}
	h := MiddlewareWithTelemetry(src, writeProblem, nil, sink)(
		http.HandlerFunc(next200))

	// Announced hit: 200 + headers + telemetry.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", announced.Path, nil))
	if rec.Code != 200 || rec.Header().Get("Sunset") == "" {
		t.Fatalf("announced hit broken: %d", rec.Code)
	}
	// Sunset hit: 410 + telemetry.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", gone.Path, nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("sunset hit → %d", rec.Code)
	}
	// Non-deprecated route: no hit recorded.
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/api/v1/orders", nil))

	if len(hits) != 2 {
		t.Fatalf("hits=%d want 2", len(hits))
	}
	if hits[0].Gone || hits[1].Gone == false {
		t.Fatalf("hit kinds wrong: %+v", hits)
	}
	if hits[0].Rule.ID != announced.ID || hits[1].Rule.ID != gone.ID {
		t.Fatalf("hit rule ids wrong: %+v", hits)
	}
}

// Nil sink keeps the middleware identical to Middleware.
func TestMiddlewareTelemetryNilSink(t *testing.T) {
	src := &staticRules{rules: []Rule{futureRule()}}
	h := MiddlewareWithTelemetry(src, writeProblem, nil, nil)(
		http.HandlerFunc(next200))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/legacy", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Link"), "deprecation") {
		t.Fatal("nil-sink middleware diverged")
	}
}
