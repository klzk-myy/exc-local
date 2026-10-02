// lag.go — SURVEILLANCE_LAG_WARNING probe (spec §14.9, §24 #392,
// Phase-21 AC row 72): when the JetStream consumer feeding the signal
// engine falls >10,000 events behind, raise a P2 ops alert and trigger
// the analysis-worker auto-scale hook. Detection continues under
// backlog — the degraded-detection policy is "detect late, never drop":
// the consumer is durable, so a lagging pump replays the full backlog
// rather than losing detection coverage; the alert + latency metric are
// the SLA surface.
package surveillance

import (
	"context"
	"time"

	"exchange/internal/observability"
)

// LagWarningThreshold is the §14.9 breach point (events).
const LagWarningThreshold uint64 = 10_000

// LagWarningCode is the alert code carried on ops.alerts.* payloads.
const LagWarningCode = "SURVEILLANCE_LAG_WARNING"

// AutoscaleSubject is the auto-scale hook — the workers scaler
// subscribes and grows the analysis pool while a lag alert is firing.
const AutoscaleSubject = "ops.autoscale.surveillance-workers"

// LagProbe samples the durable consumer's lag (pending + in-flight).
// Returns the last-good reading when the broker is briefly unreachable —
// a transient info failure must not flap the alert edge.
type LagProbe func(ctx context.Context) (uint64, error)

// DetectionLatency is the most recent event-time→apply gap — the
// §24 #392 detection-latency SLA surface.
func (e *Engine) DetectionLatency() time.Duration {
	return time.Duration(e.lastDetectionNs.Load())
}

// AppliedCount is the lifetime processed-event count.
func (e *Engine) AppliedCount() uint64 { return e.appliedTotal.Load() }

// WatchLag evaluates the probe every interval and drives an
// observability.Evaluator threshold rule — firing publishes
// SURVEILLANCE_LAG_WARNING (P2) to the alert sink; resolution publishes
// the matching "resolved". The autoscaler hook publishes to
// AutoscaleSubject on each firing edge.
func WatchLag(ctx context.Context, probe LagProbe, eval *observability.Evaluator,
	autoscale observability.Publisher, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	var last uint64
	eval.AddThreshold("surveillance-l3-lag", observability.SeverityP2,
		LagWarningCode,
		"surveillance L3 consumer lag > 10,000 — scale analysis workers",
		0,
		func() float64 {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			lag, err := probe(pctx)
			if err == nil {
				last = lag
			}
			return float64(last)
		},
		float64(LagWarningThreshold))

	t := time.NewTicker(interval)
	defer t.Stop()
	wasFiring := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			eval.EvalOnce(time.Now())
			firing := eval.Firing("surveillance-l3-lag")
			if firing && !wasFiring && autoscale != nil {
				// Auto-scale hook — best-effort; the P1/P2 ops alert is
				// already on the wire and is the authoritative page.
				_ = autoscale.Publish(ctx, AutoscaleSubject,
					[]byte(`{"action":"scale-up","reason":"SURVEILLANCE_LAG_WARNING"}`))
			}
			wasFiring = firing
		}
	}
}
