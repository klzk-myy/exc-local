// Task 5.3.28 — API versioning middleware tests.
package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

func versionChain(cfg VersionConfig, inner http.Handler) http.Handler {
	return APIVersion(cfg)(inner)
}

func TestVersionHeaderOnSupportedMajor(t *testing.T) {
	var gotMajor int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMajor = APIVersionFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := versionChain(VersionConfig{Versions: map[int]string{1: "1.2.3"}}, inner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/book/EURUSD", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get(VersionHeader); got != "1.2.3" {
		t.Fatalf("X-API-Version=%q, want 1.2.3", got)
	}
	if gotMajor != 1 {
		t.Fatalf("context major=%d, want 1", gotMajor)
	}
}

func TestUnsupportedMajorRejected(t *testing.T) {
	h := versionChain(VersionConfig{Versions: map[int]string{1: "1.2.3"}}, okHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v9/book/EURUSD", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if env["error"] != CodeUnsupportedProtocolVersion {
		t.Fatalf("envelope=%v", env)
	}
}

func TestDeprecatedRouteEmitsSunset(t *testing.T) {
	sunset := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h := versionChain(VersionConfig{
		Versions: map[int]string{1: "1.2.3"},
		Deprecated: []DeprecatedRoute{
			{PathPrefix: "/api/v1/legacy", Sunset: sunset},
		},
	}, okHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/legacy/thing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get(SunsetHeader); got != sunset.UTC().Format(http.TimeFormat) {
		t.Fatalf("Sunset=%q", got)
	}
	if rec.Header().Get(DeprecationHeader) == "" {
		t.Fatal("Deprecation header missing")
	}
	// Non-deprecated route carries neither header.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/v1/book/EURUSD", nil))
	if rec2.Header().Get(SunsetHeader) != "" {
		t.Fatal("Sunset leaked onto a live route")
	}
}

func TestNonAPIPathUnaffected(t *testing.T) {
	h := versionChain(VersionConfig{Versions: map[int]string{1: "1.2.3"}}, okHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if rec.Header().Get(VersionHeader) != "" {
		t.Fatal("X-API-Version emitted on unversioned path")
	}
}

func TestWSProtocolVersionCheck(t *testing.T) {
	if err := CheckWSProtocolVersion(WSProtocolVersion); err != nil {
		t.Fatalf("v%d must be accepted: %v", WSProtocolVersion, err)
	}
	err := CheckWSProtocolVersion(99)
	if err == nil {
		t.Fatal("unknown protocol version must be rejected")
	}
	var pe *excerrors.Error
	if !errors.As(err, &pe) || pe.Code != CodeUnsupportedProtocolVersion {
		t.Fatalf("err=%v, want UNSUPPORTED_PROTOCOL_VERSION coded error", err)
	}
}
