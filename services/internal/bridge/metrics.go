package bridge

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// Metrics is the Bridge's instrument panel. The Prometheus client is not a
// vendored dependency, so this is a minimal expvar-style registry plus a
// hand-rolled Prometheus text exposition (format v0.0.4) — compatible with
// a standard scrape config.
//
// Task 3.3.10 mandated names:
//   - bridge_events_published_total (counter)
//   - bridge_buffer_depth           (gauge)
//   - bridge_nats_reconnect_total   (counter)
//
// Additional counters support §24 #210 observability (dropped/unrouted
// events, publish latency mean).
type Metrics struct {
	shard uint32

	received   atomic.Uint64 // fragments decoded & enqueued
	published  atomic.Uint64 // successful stream publishes (per subject)
	dropped    atomic.Uint64 // events evicted by a full buffer
	malformed  atomic.Uint64 // undecodable fragments
	unrouted   atomic.Uint64 // events published on the UNKNOWN symbol token
	reconnects atomic.Uint64 // degraded->recovered publish transitions
	publishErr atomic.Uint64 // failed publish attempts
	heartbeats atomic.Uint64 // heartbeat messages sent

	publishNanosSum   atomic.Uint64
	publishNanosCount atomic.Uint64

	bufferDepth atomic.Int64
}

func newMetrics(shard uint32) *Metrics { return &Metrics{shard: shard} }

func (m *Metrics) incReceived()           { m.received.Add(1) }
func (m *Metrics) incPublished()          { m.published.Add(1) }
func (m *Metrics) incDropped()            { m.dropped.Add(1) }
func (m *Metrics) incMalformed()          { m.malformed.Add(1) }
func (m *Metrics) incReconnects()         { m.reconnects.Add(1) }
func (m *Metrics) incPublishErr()         { m.publishErr.Add(1) }
func (m *Metrics) incHeartbeats()         { m.heartbeats.Add(1) }
func (m *Metrics) setBufferDepth(n int64) { m.bufferDepth.Store(n) }
func (m *Metrics) observePublishNanos(n int64) {
	m.publishNanosSum.Add(uint64(n))
	m.publishNanosCount.Add(1)
}

// Snapshot is a point-in-time copy of the counters, used by tests and the
// heartbeat payload.
type Snapshot struct {
	Received    uint64 `json:"events_received_total"`
	Published   uint64 `json:"events_published_total"`
	Dropped     uint64 `json:"events_dropped_total"`
	Malformed   uint64 `json:"events_malformed_total"`
	Unrouted    uint64 `json:"unrouted_events_total"`
	Reconnects  uint64 `json:"nats_reconnect_total"`
	PublishErr  uint64 `json:"publish_errors_total"`
	Heartbeats  uint64 `json:"heartbeats_total"`
	BufferDepth int64  `json:"buffer_depth"`
	BufferCap   int    `json:"buffer_capacity"`
}

// exposition series; emission order is deterministic.
type metricDef struct {
	name  string
	help  string
	typ   string // counter|gauge
	value func() float64
}

func (m *Metrics) defs() []metricDef {
	return []metricDef{
		{"bridge_events_received_total", "Engine events decoded and enqueued from Aeron.", "counter",
			func() float64 { return float64(m.received.Load()) }},
		{"bridge_events_published_total", "Successful JetStream publishes (one per subject).", "counter",
			func() float64 { return float64(m.published.Load()) }},
		{"bridge_events_dropped_total", "Events evicted because the bounded buffer was full.", "counter",
			func() float64 { return float64(m.dropped.Load()) }},
		{"bridge_events_malformed_total", "Fragments that failed flatbuffers decode.", "counter",
			func() float64 { return float64(m.malformed.Load()) }},
		{"bridge_unrouted_events_total", "Events published on the UNKNOWN symbol token.", "counter",
			func() float64 { return float64(m.unrouted.Load()) }},
		{"bridge_nats_reconnect_total", "NATS publish recovery transitions (degraded -> healthy).", "counter",
			func() float64 { return float64(m.reconnects.Load()) }},
		{"bridge_publish_errors_total", "Failed JetStream publish attempts (retried).", "counter",
			func() float64 { return float64(m.publishErr.Load()) }},
		{"bridge_heartbeats_total", "Heartbeat messages published to bridge.health.<shard>.", "counter",
			func() float64 { return float64(m.heartbeats.Load()) }},
		{"bridge_buffer_depth", "Events currently buffered awaiting publish.", "gauge",
			func() float64 { return float64(m.bufferDepth.Load()) }},
		{"bridge_publish_latency_seconds_sum", "JetStream publish latency sum (divide by _count for mean).", "counter",
			func() float64 { return float64(m.publishNanosSum.Load()) / 1e9 }},
		{"bridge_publish_latency_seconds_count", "JetStream publish latency observation count.", "counter",
			func() float64 { return float64(m.publishNanosCount.Load()) }},
	}
}

// writeText renders the Prometheus v0.0.4 exposition body.
func (m *Metrics) writeText(w io.Writer) {
	label := fmt.Sprintf(`shard="%d"`, m.shard)
	for _, d := range m.defs() {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s{%s} %s\n",
			d.name, d.help, d.name, d.typ, d.name, label,
			strconv.FormatFloat(d.value(), 'g', -1, 64))
	}
}

// String renders the exposition body — used by unit tests without an HTTP
// round-trip.
func (m *Metrics) String() string {
	var sb strings.Builder
	m.writeText(&sb)
	return sb.String()
}

// Handler serves GET /metrics in Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.writeText(w)
	})
}
