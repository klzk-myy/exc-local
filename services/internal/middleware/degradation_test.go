// Task 2.3.6 — X-Degradation-Mode middleware tests.
// Unit cases use a fake DegradationReader; the live case is gated on
// EXC_REDIS_TEST=1 (+ optional EXC_REDIS_TEST_ADDR, default 127.0.0.1:16379)
// matching the internal/redis test convention.
package middleware

import (
	"context"
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
	c := exchredis.New(addr, "", 0)
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
	c := exchredis.New("127.0.0.1:1", "", 0)
	t.Cleanup(func() { _ = c.Close() })
	if got := headerFor(t, c); got != "Maintenance" {
		t.Fatalf("header = %q, want Maintenance", got)
	}
}
