// natsmon.go — JetStream stream/consumer gauges (Task 7.3.8 item 3) and
// the bridge heartbeat staleness watcher (Task 7.3.8 liveness).
//
// The monitor rides internal/nats.Client.JetStreamHealth — the same
// app-side report natsctl health prints — so consumer lag, pending depth
// and stream storage are visible to Prometheus without the
// prometheus-nats-exporter sidecar (server :8222/jsz remains the
// infra-level target).
package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	excnats "exchange/internal/nats"
)

// HealthSource abstracts the JetStream health probe — *nats.Client
// satisfies it in production; tests fake it.
type HealthSource interface {
	JetStreamHealth(ctx context.Context) (*excnats.HealthReport, error)
}

// NATSMonitor mirrors JetStream health into Prometheus gauges.
type NATSMonitor struct {
	Interval time.Duration
	Log      *slog.Logger
	src      HealthSource

	connected   *GaugeVec
	streams     *GaugeVec
	streamBytes *GaugeVec
	pending     *GaugeVec
	ackPending  *GaugeVec
	redelivered *GaugeVec
	last        struct {
		mu  sync.RWMutex
		rep *excnats.HealthReport
	}
}

// NewNATSMonitor builds the monitor over src.
func NewNATSMonitor(reg *Registry, src HealthSource, log *slog.Logger) *NATSMonitor {
	if log == nil {
		log = slog.Default()
	}
	return &NATSMonitor{
		Interval: 15 * time.Second,
		Log:      log,
		src:      src,
		connected: reg.Gauge("nats_connected",
			"1 when the JetStream health probe succeeded on the last sample."),
		streams: reg.Gauge("nats_stream_messages",
			"Messages held in the stream."),
		streamBytes: reg.Gauge("nats_stream_bytes",
			"Bytes stored in the stream."),
		pending: reg.Gauge("nats_consumer_pending",
			"Messages pending delivery for the durable consumer."),
		ackPending: reg.Gauge("nats_consumer_ack_pending",
			"Delivered-but-unacked messages for the durable consumer."),
		redelivered: reg.Gauge("nats_consumer_redelivered_total",
			"Lifetime redelivery count reported by the consumer."),
	}
}

// SampleOnce refreshes all gauges. Errors mark nats_connected 0 and are
// returned (Run logs them at warn; every probe failure is data).
func (m *NATSMonitor) SampleOnce(ctx context.Context) error {
	rep, err := m.src.JetStreamHealth(ctx)
	if err != nil {
		m.connected.With().Set(0)
		return err
	}
	m.connected.With().Set(1)
	for _, s := range rep.Streams {
		m.streams.With("stream", s.Name).Set(float64(s.Messages))
		m.streamBytes.With("stream", s.Name).Set(float64(s.Bytes))
		for _, c := range s.Consumers {
			m.pending.With("stream", s.Name, "consumer", c.Name).
				Set(float64(c.NumPending))
			m.ackPending.With("stream", s.Name, "consumer", c.Name).
				Set(float64(max(c.NumAckPending, 0)))
			m.redelivered.With("stream", s.Name, "consumer", c.Name).
				Set(float64(max(c.NumRedelivered, 0)))
		}
	}
	m.last.mu.Lock()
	m.last.rep = rep
	m.last.mu.Unlock()
	return nil
}

// Report returns the last successful health report (alert evaluator
// source — nil until the first successful sample).
func (m *NATSMonitor) Report() *excnats.HealthReport {
	m.last.mu.RLock()
	defer m.last.mu.RUnlock()
	return m.last.rep
}

// MaxConsumerPending returns the largest NumPending across all consumers
// — the Task 7.3.8 alert source (pending > 50000 → P1).
func (m *NATSMonitor) MaxConsumerPending() float64 {
	rep := m.Report()
	if rep == nil {
		return -1
	}
	var worst uint64
	for _, s := range rep.Streams {
		for _, c := range s.Consumers {
			if c.NumPending > worst {
				worst = c.NumPending
			}
		}
	}
	return float64(worst)
}

// Run samples until ctx is cancelled.
func (m *NATSMonitor) Run(ctx context.Context) {
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		if err := m.SampleOnce(ctx); err != nil {
			m.Log.Warn("nats health sample failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------------------
// bridge heartbeat watcher
// ---------------------------------------------------------------------------

// Bridge heartbeat subject family: "bridge.health.<shard>" (Task 3.3.10,
// 5s cadence). The payload carries nats_connected + buffer_depth +
// published/dropped counters — mirrored so the admin service can alert on
// bridge-side state even when its own /metrics is unreachable.
const BridgeHeartbeatSubjectWildcard = "bridge.health.>"

// SubscribeFunc binds a core-NATS subscription: cb fires per message with
// the concrete subject and payload; the returned func unsubscribes.
type SubscribeFunc func(subject string, cb func(subj string, data []byte)) (unsub func(), err error)

// BridgeHeartbeatWatcher tracks bridge.health.<shard> liveness and
// mirrors the payload counters into Prometheus gauges.
type BridgeHeartbeatWatcher struct {
	Interval time.Duration // staleness recompute cadence
	Log      *slog.Logger

	mu        sync.Mutex
	lastSeen  map[string]time.Time // shard → last heartbeat receipt (wall clock)
	now       func() time.Time
	done      chan struct{}
	closeOnce sync.Once

	received   *CounterVec
	ageSeconds *GaugeVec
	bufDepth   *GaugeVec
	published  *GaugeVec
	dropped    *GaugeVec
	natsUp     *GaugeVec
}

// heartbeatPayload mirrors bridge.go's heartbeat JSON.
type heartbeatPayload struct {
	ShardID     uint32 `json:"shard_id"`
	Connected   bool   `json:"nats_connected"`
	Published   uint64 `json:"events_published_total"`
	BufferDepth int64  `json:"buffer_depth"`
	Dropped     uint64 `json:"events_dropped_total"`
}

// NewBridgeHeartbeatWatcher builds the watcher. sub is the subscription
// seam — production binds cli.Conn().Subscribe; tests inject a fake.
func NewBridgeHeartbeatWatcher(reg *Registry, sub SubscribeFunc, log *slog.Logger) (*BridgeHeartbeatWatcher, error) {
	if sub == nil {
		return nil, fmt.Errorf("observability: nil subscribe func")
	}
	if log == nil {
		log = slog.Default()
	}
	w := &BridgeHeartbeatWatcher{
		Interval: 5 * time.Second,
		Log:      log,
		lastSeen: map[string]time.Time{},
		now:      time.Now,
		done:     make(chan struct{}),
		received: reg.Counter("bridge_health_messages_total",
			"bridge.health heartbeats received, by shard."),
		ageSeconds: reg.Gauge("bridge_heartbeat_age_seconds",
			"Seconds since the last bridge.health heartbeat, by shard."),
		bufDepth: reg.Gauge("bridge_health_buffer_depth",
			"Bridge buffer depth from the heartbeat payload, by shard."),
		published: reg.Gauge("bridge_health_published",
			"events_published_total from the heartbeat payload, by shard."),
		dropped: reg.Gauge("bridge_health_dropped_total",
			"events_dropped_total from the heartbeat payload, by shard."),
		natsUp: reg.Gauge("bridge_health_nats_connected",
			"nats_connected flag from the heartbeat payload, by shard."),
	}
	unsub, err := sub(BridgeHeartbeatSubjectWildcard, w.onMessage)
	if err != nil {
		return nil, fmt.Errorf("observability: subscribe %q: %w",
			BridgeHeartbeatSubjectWildcard, err)
	}
	go w.ageLoop(unsub)
	return w, nil
}

func (w *BridgeHeartbeatWatcher) onMessage(subj string, data []byte) {
	// bridge.health.<shard> — shard label is the subject tail.
	shard := subj
	if i := strings.LastIndexByte(subj, '.'); i >= 0 && i < len(subj)-1 {
		shard = subj[i+1:]
	}
	var p heartbeatPayload
	_ = json.Unmarshal(data, &p) // tolerate non-heartbeat payload: still a liveness tick
	if p.ShardID != 0 {
		shard = strconv.FormatUint(uint64(p.ShardID), 10)
	}
	w.mu.Lock()
	w.lastSeen[shard] = w.now()
	w.mu.Unlock()
	w.received.With("shard", shard).Inc()
	w.published.With("shard", shard).Set(float64(p.Published))
	w.bufDepth.With("shard", shard).Set(float64(p.BufferDepth))
	w.dropped.With("shard", shard).Set(float64(p.Dropped))
	v := 0.0
	if p.Connected {
		v = 1
	}
	w.natsUp.With("shard", shard).Set(v)
}

// RefreshAges recomputes bridge_heartbeat_age_seconds — called by the
// age loop; tests drive it directly.
func (w *BridgeHeartbeatWatcher) RefreshAges() {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for shard, at := range w.lastSeen {
		w.ageSeconds.With("shard", shard).Set(now.Sub(at).Seconds())
	}
}

// MaxHeartbeatAge returns the worst heartbeat staleness in seconds
// across observed shards — the alert source (heartbeat staleness → P1).
// Returns -1 when no heartbeat has ever been observed.
func (w *BridgeHeartbeatWatcher) MaxHeartbeatAge() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.lastSeen) == 0 {
		return -1
	}
	var worst time.Duration
	now := w.now()
	for _, at := range w.lastSeen {
		if d := now.Sub(at); d > worst {
			worst = d
		}
	}
	return worst.Seconds()
}

// MaxBufferDepth returns the largest heartbeat-reported buffer depth —
// the Task 7.3.8 alert source (buffer depth > 10000 → P1).
func (w *BridgeHeartbeatWatcher) MaxBufferDepth() float64 {
	worst := float64(-1)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.lastSeen) == 0 {
		return worst
	}
	// gauges are the source of truth post-receipt; scan the vec cells
	// directly to avoid tracking a second copy
	w.bufDepth.v.mu.RLock()
	for _, c := range w.bufDepth.v.samples {
		if v := fload(c); v > worst {
			worst = v
		}
	}
	w.bufDepth.v.mu.RUnlock()
	return worst
}

// Close stops the age loop and unsubscribes the heartbeat subject.
func (w *BridgeHeartbeatWatcher) Close() { w.closeOnce.Do(func() { close(w.done) }) }

// ageLoop refreshes staleness gauges on Interval until Close; the NATS
// subscription is released by the caller's connection teardown.
func (w *BridgeHeartbeatWatcher) ageLoop(unsub func()) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	defer unsub()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
			w.RefreshAges()
		}
	}
}
