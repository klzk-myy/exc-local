// rules.go — the canonical Task 7.3.8/7.3.10 rule set. Thresholds are
// verbatim from the phase plan; sources are injected so services bind
// their own counters/monitors.
package observability

import (
	"time"
)

// Task 7.3.8 alert thresholds (verbatim).
const (
	// AeronSubscriberLagAlertBytes — subscriber position lag → P2.
	AeronSubscriberLagAlertBytes = 1000
	// BridgeBufferDepthAlert — bridge buffer depth → P1.
	BridgeBufferDepthAlert = 10000
	// NATSConsumerPendingAlert — JetStream consumer pending → P1.
	NATSConsumerPendingAlert = 50000
	// BridgeHeartbeatStaleAfter — no bridge.health heartbeat for this
	// long means the bridge (or its shard) is down → P1. The heartbeat
	// cadence is 5s; ≥3× missed matches the §2.7.3 fencing convention.
	BridgeHeartbeatStaleAfter = 15 * time.Second
)

// Task 7.3.10 alert thresholds (verbatim).
const (
	// L1SustainFor — L1 systemic errors must persist 30s before P1.
	L1SustainFor = 30 * time.Second
	// L2SpikeWindow — rolling window for the L2 rejection-ratio rule.
	L2SpikeWindow = time.Minute
	// L2SpikeRatio — >5% of requests rejected at L2 → P2.
	L2SpikeRatio = 0.05
	// L2SpikeMinRequests — minimum requests in the window before the
	// ratio rule arms (low-traffic noise never pages).
	L2SpikeMinRequests = 100
)

// StandardSources are the metric sources the canonical rules evaluate —
// inject whatever each service actually observes; nil sources register no
// rule.
type StandardSources struct {
	// L0Errors / L1Errors / L2Errors are cumulative counter getters —
	// typically m.ErrorCount(TierL0) etc.
	L0Errors Source
	L1Errors Source
	L2Errors Source
	// TotalRequests is the denominator for the L2 spike ratio —
	// typically total HTTP requests (cumulative).
	TotalRequests Source
	// AeronSubscriberLag — worst observed pub−sub position lag (bytes).
	// Negative means "no data" (driver unreadable) → rule inert.
	AeronSubscriberLag Source
	// BridgeBufferDepth — worst observed bridge buffer depth (events).
	BridgeBufferDepth Source
	// NATSConsumerPending — worst JetStream consumer pending count.
	NATSConsumerPending Source
	// BridgeHeartbeatAge — worst seconds since a bridge heartbeat.
	// Negative means "none observed yet" → rule inert.
	BridgeHeartbeatAge Source
}

// AddStandardRules registers the canonical rule set on e. Any nil source
// leaves its rule unregistered (the monitor simply isn't wired on that
// host — e.g. no Aeron dir on a pure-API node).
func AddStandardRules(e *Evaluator, s StandardSources) {
	if s.L0Errors != nil {
		e.AddDelta("l0_errors", SeverityP0, "L0_ERROR_OBSERVED",
			"L0 critical/fatal error observed — immediate halt domain",
			s.L0Errors)
	}
	if s.L1Errors != nil {
		e.AddThreshold("l1_errors_sustained", SeverityP1,
			"L1_ERRORS_SUSTAINED",
			"L1 systemic errors sustained >30s — degradation mode territory",
			L1SustainFor, s.L1Errors, 0)
	}
	if s.L2Errors != nil && s.TotalRequests != nil {
		e.AddRatioWindow("l2_rejection_spike", SeverityP2,
			"L2_REJECTION_SPIKE",
			"L2 transaction rejections exceed 5% of requests over 1m",
			L2SpikeWindow, s.L2Errors, s.TotalRequests,
			L2SpikeRatio, L2SpikeMinRequests)
		e.AddErrorRateAnomaly("error_rate_anomaly", "ERROR_RATE_ANOMALY",
			"error rate anomalous vs rolling baseline",
			AnomalyConfig{Severity: SeverityP2},
			s.L2Errors, s.TotalRequests)
	}
	if s.AeronSubscriberLag != nil {
		e.AddThreshold("aeron_subscriber_lag", SeverityP2,
			"AERON_SUBSCRIBER_LAG",
			"Aeron subscriber position lag > 1000 bytes",
			0, s.AeronSubscriberLag, AeronSubscriberLagAlertBytes)
	}
	if s.BridgeBufferDepth != nil {
		e.AddThreshold("bridge_buffer_depth", SeverityP1,
			"BRIDGE_BUFFER_DEPTH_HIGH",
			"Bridge buffer depth > 10000 events — NATS publish backlog",
			0, s.BridgeBufferDepth, BridgeBufferDepthAlert)
	}
	if s.NATSConsumerPending != nil {
		e.AddThreshold("nats_consumer_pending", SeverityP1,
			"NATS_CONSUMER_PENDING_HIGH",
			"JetStream consumer pending > 50000 messages",
			0, s.NATSConsumerPending, NATSConsumerPendingAlert)
	}
	if s.BridgeHeartbeatAge != nil {
		e.AddThreshold("bridge_heartbeat_stale", SeverityP1,
			"BRIDGE_HEARTBEAT_STALE",
			"bridge.health heartbeat older than 15s — bridge/shard down",
			0, s.BridgeHeartbeatAge, BridgeHeartbeatStaleAfter.Seconds())
	}
}
