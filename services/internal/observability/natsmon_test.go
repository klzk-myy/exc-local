// natsmon_test.go — NATS monitor gauge emission + bridge heartbeat
// watcher staleness with injected seams (no live cluster).
package observability

import (
	"context"
	"fmt"
	"testing"
	"time"

	excnats "exchange/internal/nats"
)

type fakeHealth struct {
	rep *excnats.HealthReport
	err error
}

func (f fakeHealth) JetStreamHealth(context.Context) (*excnats.HealthReport, error) {
	return f.rep, f.err
}

func TestNATSMonitorGauges(t *testing.T) {
	reg := New()
	src := fakeHealth{rep: &excnats.HealthReport{
		Streams: []excnats.StreamHealth{{
			Name:     "trades",
			Messages: 1200, Bytes: 48000,
			Consumers: []excnats.ConsumerHealth{{
				Name: "archiver", NumPending: 77,
				NumAckPending: 3, NumRedelivered: 9,
			}},
		}},
	}}
	m := NewNATSMonitor(reg, src, nil)
	if err := m.SampleOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, samples := parseExposition(t, reg.String())
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"nats_connected", nil, 1},
		{"nats_stream_messages", map[string]string{"stream": "trades"}, 1200},
		{"nats_stream_bytes", map[string]string{"stream": "trades"}, 48000},
		{"nats_consumer_pending", map[string]string{"stream": "trades", "consumer": "archiver"}, 77},
		{"nats_consumer_ack_pending", map[string]string{"stream": "trades", "consumer": "archiver"}, 3},
		{"nats_consumer_redelivered_total", map[string]string{"stream": "trades", "consumer": "archiver"}, 9},
	}
	for _, c := range checks {
		s, ok := findSample(samples, c.name, c.labels)
		if !ok || s.value != c.want {
			t.Fatalf("%s = %v want %v (found=%v)", c.name, s.value, c.want, ok)
		}
	}
	if m.MaxConsumerPending() != 77 {
		t.Fatalf("MaxConsumerPending = %v", m.MaxConsumerPending())
	}

	// failure → nats_connected 0
	m.src = fakeHealth{err: fmt.Errorf("cluster down")}
	if err := m.SampleOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	_, samples = parseExposition(t, reg.String())
	if s, ok := findSample(samples, "nats_connected", nil); !ok || s.value != 0 {
		t.Fatalf("nats_connected = %v", s.value)
	}
}

func TestBridgeHeartbeatWatcher(t *testing.T) {
	reg := New()
	var deliver func(string, []byte)
	sub := func(subject string, cb func(string, []byte)) (func(), error) {
		if subject != BridgeHeartbeatSubjectWildcard {
			t.Fatalf("subject = %q", subject)
		}
		deliver = cb
		return func() {}, nil
	}
	w, err := NewBridgeHeartbeatWatcher(reg, sub, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	base := time.Now()
	w.now = func() time.Time { return base }
	deliver("bridge.health.0", []byte(
		`{"shard_id":0,"nats_connected":true,"events_published_total":42,"buffer_depth":12000,"events_dropped_total":3}`))
	w.RefreshAges()

	_, samples := parseExposition(t, reg.String())
	if s, ok := findSample(samples, "bridge_health_messages_total",
		map[string]string{"shard": "0"}); !ok || s.value != 1 {
		t.Fatalf("heartbeats = %v", s.value)
	}
	if s, ok := findSample(samples, "bridge_health_buffer_depth",
		map[string]string{"shard": "0"}); !ok || s.value != 12000 {
		t.Fatalf("buffer depth = %v", s.value)
	}
	if s, ok := findSample(samples, "bridge_health_nats_connected",
		map[string]string{"shard": "0"}); !ok || s.value != 1 {
		t.Fatalf("nats_connected = %v", s.value)
	}
	if s, ok := findSample(samples, "bridge_heartbeat_age_seconds",
		map[string]string{"shard": "0"}); !ok || s.value != 0 {
		t.Fatalf("age = %v", s.value)
	}

	// staleness after 20s — the alert source sees age=20.
	w.now = func() time.Time { return base.Add(20 * time.Second) }
	w.RefreshAges()
	if got := w.MaxHeartbeatAge(); got != 20 {
		t.Fatalf("MaxHeartbeatAge = %v want 20", got)
	}
	if got := w.MaxBufferDepth(); got != 12000 {
		t.Fatalf("MaxBufferDepth = %v", got)
	}
	_, samples = parseExposition(t, reg.String())
	if s, ok := findSample(samples, "bridge_heartbeat_age_seconds",
		map[string]string{"shard": "0"}); !ok || s.value != 20 {
		t.Fatalf("age gauge = %v", s.value)
	}
}
