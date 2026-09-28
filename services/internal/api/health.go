// Package api holds HTTP handlers shared by services.
// Deeper routes are registered in Phase-05 (Task 5.3.7).
package api

import (
	"context"
	"net/http"
	"time"
)

// Health is the stub liveness endpoint. It returns HTTP 200 with the exact
// body {"status":"ok"} required by the Task 1.3.2 acceptance criteria.
func Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ---------------------------------------------------------------------------
// Task 5.3.28 item 6 / ruling R9 — full health schema
// ---------------------------------------------------------------------------

// ShardHealth is one shard's readiness entry.
type ShardHealth struct {
	ID     int    `json:"id"`
	Status string `json:"status"` // ok | degraded
	Leader bool   `json:"leader"`
}

// HealthResponse is the R9 schema:
// {"status":"ok|degraded|down","mode":"Normal|...","shards":[...],
//
//	"version":"1.2.3","timestamp":"ISO8601"}.
type HealthResponse struct {
	Status    string        `json:"status"`
	Mode      string        `json:"mode"`
	Shards    []ShardHealth `json:"shards"`
	Version   string        `json:"version"`
	Timestamp string        `json:"timestamp"`
}

// HealthFull serves the R9 health schema. mode/degraded status derives
// from the degradation record: Normal→ok, Maintenance→down, other→
// degraded; a mode-read failure reports down+Maintenance (fail-closed,
// spec §2.7 — a health endpoint that can't see the mode must not claim
// ok). The mode func wraps a ModeReader (e.g. *redis.Client).

func HealthFull(mode func(ctx context.Context) (string, error), shards []ShardHealth, version string) http.HandlerFunc {
	if version == "" {
		version = "0.0.0"
	}
	if shards == nil {
		shards = []ShardHealth{}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		m := "Normal"
		status := "ok"
		if mode != nil {
			if v, err := mode(r.Context()); err == nil && v != "" {
				m = v
			} else {
				m = "Maintenance"
			}
		}
		switch m {
		case "Normal":
			status = "ok"
		case "Maintenance":
			status = "down"
		default:
			status = "degraded"
		}
		for _, s := range shards {
			if s.Status != "ok" && status == "ok" {
				status = "degraded"
			}
		}
		WriteJSON(w, http.StatusOK, HealthResponse{
			Status:    status,
			Mode:      m,
			Shards:    shards,
			Version:   version,
			Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
}
