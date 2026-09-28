package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTracingContinuesValidTraceparent(t *testing.T) {
	var seen *TraceContext
	h := Tracing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TraceIDFrom(r.Context())
		w.WriteHeader(200)
	}))
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("traceparent",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if seen == nil || seen.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id not continued: %+v", seen)
	}
	if !seen.Sampled {
		t.Fatal("sampled flag dropped")
	}
	if len(seen.SpanID) != 16 {
		t.Fatalf("span id: %q", seen.SpanID)
	}
	if w.Header().Get(TraceIDHeader) != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("X-Trace-ID: %q", w.Header().Get(TraceIDHeader))
	}
}

func TestTracingMintsFreshOnBadHeader(t *testing.T) {
	for _, bad := range []string{
		"", // absent
		"garbage",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", // all-zero trace
		"00-XYZ-00f067aa0ba902b7-01",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", // 3 parts
	} {
		var seen *TraceContext
		h := Tracing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = TraceIDFrom(r.Context())
			w.WriteHeader(200)
		}))
		r := httptest.NewRequest("GET", "/x", nil)
		if bad != "" {
			r.Header.Set("traceparent", bad)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("bad traceparent failed request: %q", bad)
		}
		if seen == nil || len(seen.TraceID) != 32 || seen.Sampled {
			t.Fatalf("traceparent %q → %+v", bad, seen)
		}
	}
}
