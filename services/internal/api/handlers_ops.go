// Task 9.3.25 + 9.3.6 + 9.3.8 admin surface.
//
//	GET  /api/v1/system/status                  public status feed
//	GET  /api/v1/system/incidents               public incident notices
//	GET  /api/v1/admin/ops/health               admin health export (auditor+)
//	GET  /api/v1/admin/api-deprecations/usage   deprecated-route telemetry
//	POST /api/v1/admin/cache/warm               manual warm trigger
//
// The public status feed reads the exporter's status:current document;
// when it is absent (exporter down) the gateway falls back to a
// truthful local view — mode from the same Redis coordination key —
// marked source=gateway-local and status=unknown rather than a
// fabricated "operational" (spec §2.7 pessimism).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/cache"
	"exchange/internal/deprecation"
	"exchange/internal/gateway"
	"exchange/internal/middleware"
	"exchange/internal/ops"
	exchredis "exchange/internal/redis"
)

// SystemStatus serves GET /api/v1/system/status.
func SystemStatus(rdb *exchredis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if rdb != nil {
			if st, err := ops.Current(ctx, rdb.Client); err == nil {
				WriteJSON(w, http.StatusOK, st)
				return
			}
		}
		// Fallback: local view from the coordination mode only.
		mode := "Normal"
		if rdb != nil {
			if st, err := rdb.GetDegradationMode(ctx); err == nil && st.Mode != "" {
				mode = string(st.Mode)
			} else {
				mode = "Maintenance"
			}
		}
		status := "unknown"
		switch mode {
		case "Maintenance":
			status = "maintenance"
		case "Normal":
		default:
			status = "degraded_performance"
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"status":     status,
			"mode":       mode,
			"source":     "gateway-local",
			"detail":     "status aggregator unavailable — mode read from coordination state",
			"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
}

// SystemIncidents serves GET /api/v1/system/incidents — public incident
// notices + postmortem links (task item: historical post-mortems
// publicly accessible).
func SystemIncidents(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT id, title, severity, status, mode, started_at,
			       resolved_at, summary, postmortem_url
			  FROM ops_incidents WHERE public
			  ORDER BY started_at DESC LIMIT 100`)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED", "incident store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		defer rows.Close()
		type incident struct {
			ID            int64      `json:"id"`
			Title         string     `json:"title"`
			Severity      string     `json:"severity"`
			Status        string     `json:"status"`
			Mode          *string    `json:"mode,omitempty"`
			StartedAt     time.Time  `json:"started_at"`
			ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
			Summary       string     `json:"summary"`
			PostmortemURL *string    `json:"postmortem_url,omitempty"`
		}
		out := []incident{}
		for rows.Next() {
			var i incident
			if err := rows.Scan(&i.ID, &i.Title, &i.Severity, &i.Status,
				&i.Mode, &i.StartedAt, &i.ResolvedAt, &i.Summary,
				&i.PostmortemURL); err != nil {
				WriteError(w, "INTERNAL_ERROR", "incident read failed",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			out = append(out, i)
		}
		WriteJSON(w, http.StatusOK, map[string]any{"incidents": out})
	}
}

// OpsExportDeps carries the admin health-export dependencies.
type OpsExportDeps struct {
	Pool     *pgxpool.Pool
	RDB      *exchredis.Client
	Resolver AdminRoleResolver
	// ShedStats supplies the live load-shedding snapshot; nil when the
	// service runs no shedder.
	ShedStats func() middleware.ShedStats
}

// AdminOpsHealth serves GET /api/v1/admin/ops/health — the operational
// health export: aggregate status, per-component states, recent
// transitions, 24h/30d uptime windows, load-shed stage and cache-warm
// markers.
func AdminOpsHealth(d *OpsExportDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAuditRole(w, r, d.Resolver); !ok {
			return
		}
		ctx := r.Context()
		out := map[string]any{}

		if d.RDB != nil {
			if st, err := ops.Current(ctx, d.RDB.Client); err == nil {
				out["status"] = st
			}
			if ds, err := d.RDB.GetDegradationMode(ctx); err == nil {
				out["mode"] = string(ds.Mode)
				out["mode_since_ms"] = ds.EnteredAt
				out["mode_reason"] = ds.Reason
			}
			out["components"] = componentStates(ctx, d.RDB.Client)
			out["recent_events"] = recentEvents(ctx, d.RDB.Client, 50)
			out["warm_state"] = warmStates(ctx, d.RDB.Client)
		}
		if d.Pool != nil {
			up, err := uptimeWindows(ctx, d.Pool)
			if err == nil {
				out["uptime"] = up
			}
		}
		if d.ShedStats != nil {
			out["load_shedding"] = d.ShedStats()
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// componentStates reads every status:component:* hash.
func componentStates(ctx context.Context, rdb goredis.Cmdable) map[string]any {
	out := map[string]any{}
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "status:component:*", 100).Result()
		if err != nil {
			return out
		}
		for _, k := range keys {
			if f, err := rdb.HGetAll(ctx, k).Result(); err == nil && len(f) > 0 {
				out[strings.TrimPrefix(k, "status:component:")] = f
			}
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	return out
}

func recentEvents(ctx context.Context, rdb goredis.Cmdable, n int64) []json.RawMessage {
	raws, err := rdb.LRange(ctx, "status:events", 0, n-1).Result()
	if err != nil {
		return nil
	}
	out := make([]json.RawMessage, 0, len(raws))
	for _, s := range raws {
		out = append(out, json.RawMessage(s))
	}
	return out
}

func warmStates(ctx context.Context, rdb goredis.Cmdable) map[string]any {
	out := map[string]any{}
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "warm:state:*", 100).Result()
		if err != nil {
			return out
		}
		for _, k := range keys {
			if f, err := rdb.HGetAll(ctx, k).Result(); err == nil && len(f) > 0 {
				out[strings.TrimPrefix(k, "warm:state:")] = f
			}
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	return out
}

// uptimeWindows computes 24h and 30d uptime per component plus the
// aggregate from ops_status_events.
func uptimeWindows(ctx context.Context, pool *pgxpool.Pool) (map[string]any, error) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT component FROM ops_status_events`)
	if err != nil {
		return nil, err
	}
	var comps []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		comps = append(comps, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ag := ops.NewAggregator(pool, nil, nil, nil, nil)
	out := map[string]any{}
	for _, c := range comps {
		entry := map[string]any{}
		if v, okv, err := ag.Uptime(ctx, c, 24*time.Hour); err == nil && okv {
			entry["uptime_24h"] = v * 100
		}
		if v, okv, err := ag.Uptime(ctx, c, 30*24*time.Hour); err == nil && okv {
			entry["uptime_30d"] = v * 100
		}
		if len(entry) > 0 {
			out[c] = entry
		}
	}
	return out, nil
}

// AdminDeprecationUsage serves GET /api/v1/admin/api-deprecations/usage —
// per-rule hit telemetry (Redis counters + durable rollup).
func AdminDeprecationUsage(store *deprecation.Store, rdb *exchredis.Client,
	resolver AdminRoleResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAuditRole(w, r, resolver); !ok {
			return
		}
		rules, err := store.List(r.Context())
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED", "deprecation store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		counts := map[int64]map[string][2]int64{}
		if rdb != nil {
			if c, err := deprecation.UsageCounts(r.Context(), rdb.Client); err == nil {
				counts = c
			}
		}
		type entry struct {
			deprecation.Rule
			Daily map[string][2]int64 `json:"daily"` // day → [hits, gone_hits]
		}
		out := make([]entry, 0, len(rules))
		for _, ru := range rules {
			out = append(out, entry{Rule: ru, Daily: counts[ru.ID]})
		}
		WriteJSON(w, http.StatusOK, map[string]any{"usage": out})
	}
}

// AdminCacheWarm serves POST /api/v1/admin/cache/warm — a synchronous
// manual warm (the same Warm path the trigger channel drives). Runs
// under the P0+P1 budgets (≤ ~35s); the report is the response body.
func AdminCacheWarm(warmer *cache.Warmer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if requireAdmin(w, r) == nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		rep := warmer.Run(ctx, "manual")
		code := http.StatusOK
		if !rep.OK() {
			code = http.StatusServiceUnavailable
		}
		WriteJSON(w, code, rep)
	}
}
