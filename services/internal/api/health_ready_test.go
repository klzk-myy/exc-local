// Unit tests — Phase-07 Task 7.3.6 dependency-aware readiness.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okMode(context.Context) (string, error)    { return "Normal", nil }
func maintMode(context.Context) (string, error) { return "Maintenance", nil }
func roMode(context.Context) (string, error)    { return "ReadOnly", nil }
func failMode(context.Context) (string, error)  { return "", errors.New("redis gone") }
func okProbe(context.Context) error             { return nil }
func failProbe(context.Context) error           { return errors.New("boom") }

func readyReq() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/health/ready", nil)
}

func decodeReady(t *testing.T, rec *httptest.ResponseRecorder) ReadinessResponse {
	t.Helper()
	var r ReadinessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("readiness body: %v", err)
	}
	return r
}

func TestHealthReady_AllOK(t *testing.T) {
	h := HealthReady(okMode, []Dependency{
		{Name: "postgres", Required: true, Probe: okProbe},
		{Name: "redis", Required: true, Probe: okProbe},
		{Name: "nats", Required: false, Probe: okProbe},
	}, nil, "test")
	rec := httptest.NewRecorder()
	h(rec, readyReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy readiness must be 200, got %d", rec.Code)
	}
	r := decodeReady(t, rec)
	if r.Status != "ok" || r.Mode != "Normal" || len(r.Dependencies) != 3 {
		t.Fatalf("payload: %+v", r)
	}
	if r.Timestamp == "" || r.Version != "test" {
		t.Fatalf("R9 fields missing: %+v", r)
	}
}

func TestHealthReady_RequiredDepDown(t *testing.T) {
	h := HealthReady(okMode, []Dependency{
		{Name: "postgres", Required: true, Probe: failProbe},
		{Name: "redis", Required: true, Probe: okProbe},
	}, nil, "test")
	rec := httptest.NewRecorder()
	h(rec, readyReq())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("required dep down must be 503, got %d", rec.Code)
	}
	r := decodeReady(t, rec)
	if r.Status != "down" || r.Dependencies[0].Status != "down" ||
		r.Dependencies[0].Error == "" {
		t.Fatalf("payload: %+v", r)
	}
}

func TestHealthReady_OptionalDepDown(t *testing.T) {
	// NATS down degrades but does NOT pull the pod — ledger dispatch is
	// fail-operational.
	h := HealthReady(okMode, []Dependency{
		{Name: "postgres", Required: true, Probe: okProbe},
		{Name: "nats", Required: false, Probe: failProbe},
	}, nil, "test")
	rec := httptest.NewRecorder()
	h(rec, readyReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("optional dep down must stay 200, got %d", rec.Code)
	}
	if r := decodeReady(t, rec); r.Status != "degraded" {
		t.Fatalf("status: %+v", r)
	}
}

func TestHealthReady_MaintenanceIs503(t *testing.T) {
	h := HealthReady(maintMode, nil, nil, "test")
	rec := httptest.NewRecorder()
	h(rec, readyReq())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Maintenance mode must be 503, got %d", rec.Code)
	}
	if r := decodeReady(t, rec); r.Status != "down" || r.Mode != "Maintenance" {
		t.Fatalf("payload: %+v", r)
	}
}

func TestHealthReady_ModeReadFailureFailsClosed(t *testing.T) {
	h := HealthReady(failMode, nil, nil, "test")
	rec := httptest.NewRecorder()
	h(rec, readyReq())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreadable mode must fail closed 503, got %d", rec.Code)
	}
	if r := decodeReady(t, rec); r.Mode != "Maintenance" {
		t.Fatalf("unreadable mode must report Maintenance, got %q", r.Mode)
	}
}

func TestHealthReady_DegradedModeStays200(t *testing.T) {
	h := HealthReady(roMode, []Dependency{
		{Name: "postgres", Required: true, Probe: okProbe},
	}, nil, "test")
	rec := httptest.NewRecorder()
	h(rec, readyReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("ReadOnly must stay 200 (reads served), got %d", rec.Code)
	}
	if r := decodeReady(t, rec); r.Status != "degraded" || r.Mode != "ReadOnly" {
		t.Fatalf("payload: %+v", r)
	}
}

func TestHealthReady_ShardDownDegrades(t *testing.T) {
	h := HealthReady(okMode, nil, func(context.Context) []ShardHealth {
		return []ShardHealth{{ID: 0, Status: "ok"}, {ID: 1, Status: "down"}}
	}, "test")
	rec := httptest.NewRecorder()
	h(rec, readyReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("shard down degrades, not 503: got %d", rec.Code)
	}
	if r := decodeReady(t, rec); r.Status != "degraded" {
		t.Fatalf("payload: %+v", r)
	}
}

func TestHealthLive_Always200(t *testing.T) {
	// Liveness must not consult dependencies.
	rec := httptest.NewRecorder()
	Health(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("liveness: %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "ok" {
		t.Fatalf("liveness body: %v %s", err, rec.Body.String())
	}
}
