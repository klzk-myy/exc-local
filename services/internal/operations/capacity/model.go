// Package capacity implements the Phase-09 Task 9.3.19 quarterly
// automated capacity report (spec §19.9, §24 #191; model doc
// docs/ops/capacity-planning.md §5).
//
// The generator pulls trailing-window resource series through the
// MetricsSource seam (Prometheus HTTP API in production), recomputes
// growth rates by least-squares regression (the doc's "90-day
// regression"), projects exhaustion via the sizing model's runway
// formula
//
//	runway_days = avail_bytes / daily_growth_bytes
//
// for storage resources and
//
//	runway_days = (alert_threshold − current_utilization) / slope_per_day
//
// for utilization resources (CPU / memory / NIC / pool), then emits a
// JSON report artifact — per-resource utilization, exhaustion runway
// and provisioning recommendations — persisted through the Sink seam
// (capacity_reports, migration 274).
//
// Scheduler seam: no cron runtime exists in-repo; the quarterly cadence
// is the documented contract of NextQuarterStart — the ops scheduler
// (deploy/crons style wrapper) invokes Generator.Generate once per
// quarter, matching the retention enforcer's cmd/retention pattern.
package capacity

import "time"

// Resource kinds understood by the generator.
const (
	// KindStorage — mounts/volumes; runway measured in bytes remaining
	// divided by daily growth.
	KindStorage = "storage"
	// KindUtilization — ratio series (0..1) such as CPU/memory/NIC
	// utilization; runway measured as days until the series crosses
	// the alert threshold.
	KindUtilization = "utilization"
)

// Recommendation levels emitted per resource (worst → best ordering is
// the ActionRank table).
const (
	ActionOK            = "OK"             // runway beyond the horizon
	ActionPlanProvision = "PLAN_PROVISION" // exhaustion inside horizon
	ActionProvision     = "PROVISION"      // runway < 90d — start provisioning
	ActionOrderHardware = "ORDER_HARDWARE" // runway < 60d — order hardware now
	ActionExhausted     = "EXHAUSTED"      // already at/past threshold
	ActionNoData        = "NO_DATA"        // metric series unavailable — fail-visible
)

// Thresholds are the Task 9.3.19 / capacity-planning.md §4 contract.
type Thresholds struct {
	// ProvisionLeadDays — provision when storage runway drops below
	// this (doc §3: "provision when runway_days < 90").
	ProvisionLeadDays int
	// OrderLeadDays — order hardware when runway drops below this
	// (doc §3: "order hardware at < 60").
	OrderLeadDays int
	// HorizonDays — exhaustion projection window ("6 months ahead").
	HorizonDays int
	// WindowDays — trailing metrics window the regression runs over
	// (doc §5: "90-day regression").
	WindowDays int
	// CPUAlert / MemAlert / DiskAlert / NetAlert — the §4 headroom
	// thresholds (0.70 / 0.75 / 0.70 / 0.60 of capacity).
	CPUAlert, MemAlert, DiskAlert, NetAlert float64
	// CPUScaleTrigger — §19.9 shard-split trigger (0.60 sustained 15m),
	// carried for report annotation.
	CPUScaleTrigger float64
}

// DefaultThresholds returns the spec §19.9 / capacity-planning.md §4
// values — the canonical sizing-model contract.
func DefaultThresholds() Thresholds {
	return Thresholds{
		ProvisionLeadDays: 90,
		OrderLeadDays:     60,
		HorizonDays:       180,
		WindowDays:        90,
		CPUAlert:          0.70,
		MemAlert:          0.75,
		DiskAlert:         0.70,
		NetAlert:          0.60,
		CPUScaleTrigger:   0.60,
	}
}

// ResourceSpec declares one monitored resource: which series to pull,
// how to measure runway, and the remediation prose that lands in the
// report when thresholds trip.
type ResourceSpec struct {
	Name string `json:"name" yaml:"name"` // e.g. "postgres_hot_tier"
	Kind string `json:"kind" yaml:"kind"` // KindStorage | KindUtilization

	// SeriesExpr is the metrics query evaluated over the window:
	// used bytes for storage resources, utilization ratio (0..1) for
	// utilization resources.
	SeriesExpr string `json:"series_expr" yaml:"series_expr"`
	// AvailExpr is an instant query returning remaining capacity in
	// bytes — required for KindStorage.
	AvailExpr string `json:"avail_expr,omitempty" yaml:"avail_expr"`
	// CapacityExpr is an instant query returning total capacity in
	// bytes — optional for KindStorage; when empty the report derives
	// capacity as used+avail.
	CapacityExpr string `json:"capacity_expr,omitempty" yaml:"capacity_expr"`

	// Threshold is the alert-ratio runway anchor for KindUtilization
	// resources (e.g. 0.70 for CPU).
	Threshold float64 `json:"threshold,omitempty" yaml:"threshold"`

	// FloorGrowthBytesPerDay optionally floors the measured growth rate
	// for KindStorage (0 = no floor; measured slope rules). Use it to
	// keep a model minimum (e.g. WAL 15 GB/day) visible even during a
	// flat quarter.
	FloorGrowthBytesPerDay float64 `json:"floor_growth_bytes_per_day,omitempty" yaml:"floor_growth_bytes_per_day"`

	// ScaleAction is the provisioning recommendation emitted when the
	// resource approaches exhaustion ("extend volume", "shard split").
	ScaleAction string `json:"scale_action" yaml:"scale_action"`
}

// DefaultResources returns the §4/§5 resource set: the three capacity
// mounts (PostgreSQL, WAL, ClickHouse) plus engine CPU, memory and NIC
// utilization. Expressions are the node_exporter/exchange_* series the
// capacity-alerts.yml pack already scrapes.
func DefaultResources(t Thresholds) []ResourceSpec {
	return []ResourceSpec{
		{Name: "postgres_hot_tier", Kind: KindStorage,
			SeriesExpr:   `sum(node_filesystem_size_bytes{mountpoint="/var/lib/postgresql"} - node_filesystem_avail_bytes{mountpoint="/var/lib/postgresql"})`,
			AvailExpr:    `sum(node_filesystem_avail_bytes{mountpoint="/var/lib/postgresql"})`,
			CapacityExpr: `sum(node_filesystem_size_bytes{mountpoint="/var/lib/postgresql"})`,
			ScaleAction:  "extend PG volume / expedite partition archival (§19.7)"},
		{Name: "wal_volume", Kind: KindStorage,
			SeriesExpr:   `sum(node_filesystem_size_bytes{mountpoint="/var/lib/exchange/wal"} - node_filesystem_avail_bytes{mountpoint="/var/lib/exchange/wal"})`,
			AvailExpr:    `sum(node_filesystem_avail_bytes{mountpoint="/var/lib/exchange/wal"})`,
			CapacityExpr: `sum(node_filesystem_size_bytes{mountpoint="/var/lib/exchange/wal"})`,
			// WAL archives grow ~15 GB/day at design load (doc §3).
			FloorGrowthBytesPerDay: 15 * 1024 * 1024 * 1024,
			ScaleAction:            "extend WAL volume / verify S3 archive drain"},
		{Name: "clickhouse_ticks", Kind: KindStorage,
			SeriesExpr:   `sum(node_filesystem_size_bytes{mountpoint="/var/lib/clickhouse"} - node_filesystem_avail_bytes{mountpoint="/var/lib/clickhouse"})`,
			AvailExpr:    `sum(node_filesystem_avail_bytes{mountpoint="/var/lib/clickhouse"})`,
			CapacityExpr: `sum(node_filesystem_size_bytes{mountpoint="/var/lib/clickhouse"})`,
			ScaleAction:  "extend CH volume / verify TTL + cold-tier export"},
		{Name: "engine_cpu", Kind: KindUtilization,
			SeriesExpr:  `max(1 - rate(node_cpu_seconds_total{mode="idle"}[15m]))`,
			Threshold:   t.CPUAlert,
			ScaleAction: "shard split / new-pair shard (§19.9 trigger: 60% sustained 15m)"},
		{Name: "engine_memory", Kind: KindUtilization,
			SeriesExpr:  `max(1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes))`,
			Threshold:   t.MemAlert,
			ScaleAction: "rebalance order pools / add host (pool util trigger 70%)"},
		{Name: "core_nic", Kind: KindUtilization,
			SeriesExpr:  `max((rate(node_network_transmit_bytes_total[5m]) + rate(node_network_receive_bytes_total[5m])) * 8 / on(instance, device) node_network_speed_bytes)`,
			Threshold:   t.NetAlert,
			ScaleAction: "capacity review — 100GbE uplink / ingress shed"},
	}
}

// NextQuarterStart returns the first instant of the calendar quarter
// following t — the documented scheduler seam for the quarterly run.
func NextQuarterStart(t time.Time) time.Time {
	u := t.UTC()
	// Quarter months start Jan/Apr/Jul/Oct.
	month := ((int(u.Month())-1)/3+1)*3 + 1
	year := u.Year()
	if month > 12 {
		month, year = 1, year+1
	}
	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
}

// Window returns the trailing [now-WindowDays, now] regression window.
func (t Thresholds) Window(now time.Time) (start, end time.Time) {
	return now.UTC().AddDate(0, 0, -t.WindowDays), now.UTC()
}
