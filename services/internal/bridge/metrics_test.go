package bridge

import (
	"strings"
	"testing"
)

// The three Task 3.3.10 mandated metric names must appear in the
// exposition, with the shard label.
func TestMetricsExposition(t *testing.T) {
	m := newMetrics(1)
	m.incReceived()
	m.incPublished()
	m.incPublished()
	m.incDropped()
	m.incReconnects()
	m.setBufferDepth(7)
	m.observePublishNanos(1234)

	out := m.String()
	for _, name := range []string{
		"bridge_events_published_total",
		"bridge_buffer_depth",
		"bridge_nats_reconnect_total",
		"bridge_events_dropped_total",
		"bridge_unrouted_events_total",
		"bridge_heartbeats_total",
		"bridge_publish_latency_seconds_sum",
		"bridge_publish_latency_seconds_count",
	} {
		if !strings.Contains(out, name) {
			t.Fatalf("exposition missing %s:\n%s", name, out)
		}
	}
	if !strings.Contains(out, `bridge_events_published_total{shard="1"} 2`) {
		t.Fatalf("counter value wrong:\n%s", out)
	}
	if !strings.Contains(out, `bridge_buffer_depth{shard="1"} 7`) {
		t.Fatalf("gauge value wrong:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE bridge_buffer_depth gauge") {
		t.Fatal("buffer_depth must be a gauge")
	}
}
