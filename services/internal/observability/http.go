// http.go — shared service instrumentation for Task 7.3.4/7.3.10:
// HTTP request counters + duration histograms, WS connection gauges,
// engine-IPC ring gauges, reconciliation counters, degradation-mode and
// circuit-breaker state gauges, and the L0–L3 error-tier counter
// (exchange_errors_total{tier,service}) the alert evaluator consumes.
package observability

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Metric names exported by this package — referenced by the Grafana
// dashboards (deploy/grafana/dashboards) and the alert rules
// (deploy/prometheus/rules/exchange-alerts.yml).
const (
	MetricHTTPRequestsTotal      = "http_requests_total"
	MetricHTTPDurationSeconds    = "http_request_duration_seconds"
	MetricErrorsTotal            = "exchange_errors_total"
	MetricWSConnsActive          = "ws_connections_active"
	MetricWSConnsTotal           = "ws_connections_total"
	MetricDegradationMode        = "exchange_degradation_mode"
	MetricCircuitBreakerOpen     = "exchange_circuit_breaker_open"
	MetricIPCRingDepth           = "engine_ipc_ring_depth"
	MetricIPCLastSeq             = "engine_ipc_last_seq"
	MetricWALLagEntries          = "wal_lag_entries"
	MetricReconcileRunsTotal     = "reconciliation_runs_total"
	MetricReconcileMismatchTotal = "reconciliation_mismatches_total"
	MetricAlertFiring            = "exchange_alert_firing"
)

// Default HTTP latency buckets (seconds) — 5ms→10s covers the §19.3 golden
// signals and the p99 ≤ 50µs engine SLO is tracked separately via
// engine-side histograms; these buckets cover the API surface.
var HTTPDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Error severity tiers (spec §2.7.2). Kept as plain strings so the
// exchange_errors_total{tier} label matches the §2.7 taxonomy verbatim.
const (
	TierL0 = "L0" // critical/fatal — immediate core halt, P0
	TierL1 = "L1" // systemic/infrastructure degradation — P1
	TierL2 = "L2" // transaction/state boundary rejection — P2 on spikes
	TierL3 = "L3" // edge/protocol validation rejection — logged + IP-ban
)

// Metrics is the per-service instrument panel. Build with NewMetrics and
// expose via Reg.Handler() on /metrics.
type Metrics struct {
	Reg     *Registry
	service string

	httpRequests  *CounterVec
	httpDuration  *HistogramVec
	errors        *CounterVec
	wsActive      *GaugeVec
	wsTotal       *CounterVec
	degradation   *GaugeVec
	breakerOpen   *GaugeVec
	ipcRingDepth  *GaugeVec
	ipcLastSeq    *GaugeVec
	walLag        *GaugeVec
	reconRuns     *CounterVec
	reconMismatch *CounterVec
	alertFiring   *GaugeVec
}

// NewMetrics registers the full Task 7.3.4 key-metric set for service
// (the "service" label value on every series).
func NewMetrics(reg *Registry, service string) *Metrics {
	m := &Metrics{Reg: reg, service: service}
	m.httpRequests = reg.Counter(MetricHTTPRequestsTotal,
		"HTTP requests served, by service/method/route/status code.")
	m.httpDuration = reg.Histogram(MetricHTTPDurationSeconds,
		"HTTP request latency in seconds.", HTTPDurationBuckets)
	m.errors = reg.Counter(MetricErrorsTotal,
		"Errors observed by severity tier (spec §2.7.2 L0-L3).")
	m.wsActive = reg.Gauge(MetricWSConnsActive,
		"Currently open WebSocket connections.")
	m.wsTotal = reg.Counter(MetricWSConnsTotal,
		"WebSocket connections opened since boot.")
	m.degradation = reg.Gauge(MetricDegradationMode,
		"Degradation mode in effect (1 = active) — Normal|ReadOnly|MarketDataOnly|SpotOnly|Throttled|Maintenance.")
	m.breakerOpen = reg.Gauge(MetricCircuitBreakerOpen,
		"Circuit breaker state (1 = open/tripped) by breaker name.")
	m.ipcRingDepth = reg.Gauge(MetricIPCRingDepth,
		"Engine Aeron IPC inbound ring depth (entries) by shard.")
	m.ipcLastSeq = reg.Gauge(MetricIPCLastSeq,
		"Last engine sequence number observed on the IPC ring by shard.")
	m.walLag = reg.Gauge(MetricWALLagEntries,
		"WAL entries written but not yet fsync'd/archived, by shard.")
	m.reconRuns = reg.Counter(MetricReconcileRunsTotal,
		"Reconciliation sweeps run, by kind (wallet|ledger|nostro|...).")
	m.reconMismatch = reg.Counter(MetricReconcileMismatchTotal,
		"Reconciliation mismatches found, by kind.")
	m.alertFiring = reg.Gauge(MetricAlertFiring,
		"1 while the named alert rule is firing on this instance.")
	return m
}

// RecordError increments exchange_errors_total{tier,service}.
func (m *Metrics) RecordError(tier string) {
	m.errors.With("tier", tier, "service", m.service).Inc()
}

// ErrorCount returns the current counter value for a tier (alert
// evaluator source; test-visible).
func (m *Metrics) ErrorCount(tier string) float64 {
	c := m.errors.v.cell("tier", tier, "service", m.service)
	return fload(c)
}

// TierForStatus maps an HTTP status to the §2.7.2 severity tier:
//
//	418/429        → L3 (rate-limit tier breach / progressive IP ban)
//	other 4xx      → L3 (edge/protocol validation rejection)
//	409/422        → L2 (transaction/state boundary rejection)
//	5xx            → L1 (systemic/infrastructure degradation surfaces as
//	                  failed responses — L0 faults halt the process and
//	                  never reach HTTP)
//
// Note 409/422 are checked before generic 4xx → L3.
func TierForStatus(code int) string {
	switch {
	case code == http.StatusTeapot || code == http.StatusTooManyRequests:
		return TierL3 // IP ban / rate-limit tier breach
	case code == http.StatusConflict || code == http.StatusUnprocessableEntity:
		return TierL2 // state boundary rejections (margin, lifecycle, idempotency)
	case code >= 400 && code < 500:
		return TierL3 // edge validation rejection
	case code >= 500:
		return TierL1 // systemic degradation
	default:
		return ""
	}
}

// routeLabel returns the low-cardinality route label. Go 1.22+ ServeMux
// populates r.Pattern with the matched pattern ("GET /path/{id}") —
// cardinality stays bounded by the route table, not the request stream.
func routeLabel(r *http.Request) string {
	p := r.Pattern
	if p == "" {
		return "unmatched"
	}
	if i := strings.IndexByte(p, ' '); i >= 0 {
		p = p[i+1:]
	}
	return p
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying ResponseWriter for Flusher/Hijacker
// calls (WS upgrade path).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack forwards WS-upgrade hijacking to the wrapped writer.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("observability: response writer does not support hijack")
}

// Flush forwards streaming flushes (SSE/streaming endpoints).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// HTTPMiddleware instruments the wrapped handler: http_requests_total +
// http_request_duration_seconds per (method, route, status), plus
// exchange_errors_total tier counters derived from the status.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if !sw.wrote {
			status = http.StatusOK
		}
		route := routeLabel(r)
		code := strconv.Itoa(status)
		m.httpRequests.With(
			"service", m.service, "method", r.Method,
			"route", route, "code", code).Inc()
		m.httpDuration.With(
			"service", m.service, "method", r.Method, "route", route).
			Observe(time.Since(start).Seconds())
		if tier := TierForStatus(status); tier != "" {
			m.RecordError(tier)
		}
	})
}

// TrackWSConnect records a WebSocket open and returns the close hook —
// services call the returned func when the connection terminates.
func (m *Metrics) TrackWSConnect() func() {
	m.wsTotal.With("service", m.service).Inc()
	m.wsActive.With("service", m.service).Add(1)
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			m.wsActive.With("service", m.service).Add(-1)
		}
	}
}

// SetWSActive sets the active-connections gauge directly — for services
// that already maintain a connection count.
func (m *Metrics) SetWSActive(n float64) {
	m.wsActive.With("service", m.service).Set(n)
}

// DegradationModes is the canonical §2.7 mode set — order matters for
// deterministic exposition.
var DegradationModes = []string{
	"Normal", "ReadOnly", "MarketDataOnly", "SpotOnly", "Throttled", "Maintenance",
}

// SetDegradationMode marks mode active (1) and all other modes inactive.
// Unknown modes still emit (label preserves the observed value) while
// known modes reset — a foreign mode is visible, never silently dropped.
func (m *Metrics) SetDegradationMode(mode string) {
	seen := map[string]bool{}
	for _, known := range DegradationModes {
		v := 0.0
		if mode == known {
			v = 1
		}
		m.degradation.With("mode", known).Set(v)
		seen[known] = true
	}
	if !seen[mode] {
		m.degradation.With("mode", mode).Set(1)
	}
}

// SetCircuitBreaker records breaker name's open state (1 = open).
func (m *Metrics) SetCircuitBreaker(name string, open bool) {
	v := 0.0
	if open {
		v = 1
	}
	m.breakerOpen.With("name", name).Set(v)
}

// SetIPCRingDepth records the engine inbound ring depth for a shard.
func (m *Metrics) SetIPCRingDepth(shard int, depth int64) {
	m.ipcRingDepth.With("shard", strconv.Itoa(shard)).Set(float64(depth))
}

// SetIPCLastSeq records the last engine sequence observed per shard.
func (m *Metrics) SetIPCLastSeq(shard int, seq uint64) {
	m.ipcLastSeq.With("shard", strconv.Itoa(shard)).Set(float64(seq))
}

// SetWALLag records the fsync/archive lag per shard.
func (m *Metrics) SetWALLag(shard int, entries int64) {
	m.walLag.With("shard", strconv.Itoa(shard)).Set(float64(entries))
}

// ObserveReconciliation counts a reconciliation sweep plus any
// mismatches it found.
func (m *Metrics) ObserveReconciliation(kind string, mismatches int) {
	m.reconRuns.With("kind", kind).Inc()
	if mismatches > 0 {
		m.reconMismatch.With("kind", kind).Add(float64(mismatches))
	}
}

// SetAlertFiring flips the per-rule firing gauge (alert evaluator).
func (m *Metrics) SetAlertFiring(rule, severity string, firing bool) {
	v := 0.0
	if firing {
		v = 1
	}
	m.alertFiring.With("rule", rule, "severity", severity).Set(v)
}
