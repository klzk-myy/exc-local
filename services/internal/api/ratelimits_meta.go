// Task 5.3.42 item 4 — publish the venue rate-limit contract.
//
// GET /api/v1/meta/rate-limits renders the §8.3 tier table, the
// per-route request weights, the multi-interval counter names
// (RAW_REQUESTS / REQUEST_WEIGHT / ORDERS), the burst-refill rule, the
// progressive 418 ban schedule (with allowlist bypass semantics), and
// the WS-side caps (per-IP connection cap, per-account tier caps,
// subscription ceiling, dedup window) — one machine-readable document
// clients can consult instead of discovering limits by being rejected.
package api

import (
	"net/http"
	"time"

	"exchange/internal/ratelimit"
	"exchange/internal/ws"
)

// RateLimitsDoc is the published hardening table.
type RateLimitsDoc struct {
	// Tiers maps §8.3 tier name → {rate_per_sec, burst_factor,
	// weight_per_min, keyed_by_ip}.
	Tiers map[string]ratelimit.Spec `json:"tiers"`
	// Weights is the per-route REQUEST_WEIGHT table — method+path prefix
	// → weight; order=true paths also feed the ORDERS counters.
	Weights []ratelimit.RouteWeight `json:"weights"`
	// Counters are the multi-interval usage counter names (§8.8 item 4).
	Counters []string `json:"counters"`
	// BurstRefill documents the token-bucket refill rule.
	BurstRefill map[string]any `json:"burst_refill"`
	// Bans documents the Task 5.3.34 progressive ban schedule.
	Bans map[string]any `json:"bans"`
	// WS documents the §10.5/§10.6 socket caps.
	WS map[string]any `json:"websocket"`
	// Sentinel documents counter correctness under failover (the
	// coordination client fails over to in-memory per §4.1/§18.3).
	Sentinel   map[string]any `json:"sentinel_failover"`
	ServerTime int64          `json:"server_time_ms"`
}

// RateLimitsMetaHandler serves the published table. Static data — the
// live usage view is GET /api/v1/account/rate-limits (Task 5.3.40).
func RateLimitsMetaHandler(w http.ResponseWriter, _ *http.Request) {
	banSecs := make([]int64, 0, len(ratelimit.BanSchedule))
	for _, d := range ratelimit.BanSchedule {
		banSecs = append(banSecs, int64(d/time.Second))
	}
	doc := RateLimitsDoc{
		Tiers:    map[string]ratelimit.Spec{},
		Weights:  ratelimit.DefaultWeights,
		Counters: []string{"RAW_REQUESTS", "REQUEST_WEIGHT", "ORDERS"},
		BurstRefill: map[string]any{
			"rule":        "tokens = min(rate_per_sec, tokens + rate_per_sec * elapsed_s)",
			"burst":       "capacity = burst_factor * rate_per_sec",
			"interval_ms": 500,
		},
		Bans: map[string]any{
			"schedule_s":       banSecs,
			"offense_window_s": 60,
			"strike_window_s":  int64((24 * time.Hour) / time.Second),
			"status":           418,
			"code":             "IP_BANNED",
			"allowlist_bypass": true,
			"admin_surface":    "/api/v1/admin/ip-bans",
		},
		WS: map[string]any{
			"per_account_conn_caps":   ws.ConnCapsTable(),
			"per_ip_conn_cap_default": 256,
			"max_subscriptions":       200,
			"control_rate_per_sec":    100,
			"dedup_window_ms":         60000,
			"slow_consumer_drop_s":    2,
			"outbound_buffer_msgs":    1024,
		},
		Sentinel: map[string]any{
			"primary":  "redis",
			"fallback": "in-memory backend (spec §4.1/§18.3 failover)",
			"contract": "counter errors never silently pass requests — limiter outage surfaces SERVICE_DEGRADED",
		},
		ServerTime: time.Now().UnixMilli(),
	}
	for tier, spec := range ratelimit.Specs {
		doc.Tiers[string(tier)] = spec
	}
	WriteJSON(w, http.StatusOK, doc)
}
