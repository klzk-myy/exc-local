// Package api holds HTTP handlers shared by services.
// Deeper routes are registered in Phase-05 (Task 5.3.7).
package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
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

// ---------------------------------------------------------------------------
// Task 7.3.6 — per-dependency readiness (/ready, /health/ready)
// ---------------------------------------------------------------------------

// Dependency is one readiness probe target. Required deps (PostgreSQL,
// Redis, the engine IPC ring) failing mark the service down (HTTP 503);
// optional deps (NATS — ledger dispatch is fail-operational) degrade the
// composite but keep 200 so a working pod is not pulled by the load
// balancer.
type Dependency struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	// Probe returns nil when the dependency is healthy. Called under a
	// per-probe timeout; a hung probe reports down, never blocks.
	Probe func(ctx context.Context) error `json:"-"`
}

// DependencyHealth is one probe result in the readiness payload.
type DependencyHealth struct {
	Name      string  `json:"name"`
	Status    string  `json:"status"` // ok | down
	Required  bool    `json:"required"`
	LatencyMs float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

// ReadinessResponse is the R9 health schema plus the dependency detail
// array (Task 7.3.6 item 2: readiness = dependencies connected).
type ReadinessResponse struct {
	Status       string             `json:"status"` // ok | degraded | down
	Mode         string             `json:"mode"`
	Shards       []ShardHealth      `json:"shards"`
	Dependencies []DependencyHealth `json:"dependencies"`
	Version      string             `json:"version"`
	Timestamp    string             `json:"timestamp"`
}

// defaultProbeTimeout bounds each dependency check so a wedged driver
// cannot stall the probe endpoint past the orchestrator's patience.
const defaultProbeTimeout = 2 * time.Second

// HealthReady serves the dependency-checked readiness probe.
//
// Semantics (fail-closed, spec §2.7):
//   - any REQUIRED dependency down → status "down", HTTP 503;
//   - Maintenance mode → "down", 503 (matching HealthFull's mapping —
//     the pod must leave rotation during maintenance);
//   - mode read failure → "down" + Maintenance reported;
//   - optional-dependency failure or a non-Normal mode → "degraded", 200;
//   - shard degradation degrades the composite the same way.
//
// shardsFn supplies live shard health (engine ring probes in cmd/
// gateway); nil → empty shard list.
func HealthReady(mode func(ctx context.Context) (string, error),
	deps []Dependency, shardsFn func(ctx context.Context) []ShardHealth,
	version string) http.HandlerFunc {
	if version == "" {
		version = "0.0.0"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		m := "Normal"
		if mode != nil {
			if v, err := mode(r.Context()); err == nil && v != "" {
				m = v
			} else {
				m = "Maintenance"
			}
		}

		results := make([]DependencyHealth, len(deps))
		var wg sync.WaitGroup
		for i, d := range deps {
			wg.Add(1)
			go func(i int, d Dependency) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), defaultProbeTimeout)
				defer cancel()
				start := time.Now()
				err := d.Probe(ctx)
				res := DependencyHealth{
					Name:      d.Name,
					Required:  d.Required,
					LatencyMs: float64(time.Since(start).Microseconds()) / 1000.0,
				}
				if err != nil {
					res.Status = "down"
					res.Error = err.Error()
				} else {
					res.Status = "ok"
				}
				results[i] = res
			}(i, d)
		}
		wg.Wait()

		shards := []ShardHealth{}
		if shardsFn != nil {
			if s := shardsFn(r.Context()); s != nil {
				shards = s
			}
		}

		status := "ok"
		httpCode := http.StatusOK
		switch m {
		case "Normal":
		case "Maintenance":
			status = "down"
			httpCode = http.StatusServiceUnavailable
		default:
			status = "degraded"
		}
		for _, d := range results {
			if d.Status != "ok" && d.Required {
				status = "down"
				httpCode = http.StatusServiceUnavailable
			} else if d.Status != "ok" && status == "ok" {
				status = "degraded"
			}
		}
		for _, s := range shards {
			if s.Status != "ok" && status == "ok" {
				status = "degraded"
			}
		}

		WriteJSON(w, httpCode, ReadinessResponse{
			Status:       status,
			Mode:         m,
			Shards:       shards,
			Dependencies: results,
			Version:      version,
			Timestamp:    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
}

// DependencyErr builds a probe error with the dependency name attached —
// the payload's error field then reads self-describing.
func DependencyErr(name string, err error) error {
	return fmt.Errorf("%s: %w", name, err)
}
