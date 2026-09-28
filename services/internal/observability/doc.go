// Package observability is the shared instrumentation surface for the
// exchange's Go services (Phase-07 Tasks 7.3.4/7.3.5/7.3.8/7.3.10, spec
// §19.3, §2.7, §7.3, §24 #306).
//
// The module does not vendor prometheus/client_golang: Registry is a
// minimal, allocation-light metric set rendered in the Prometheus text
// exposition format (v0.0.4) — the same convention internal/bridge
// established with its hand-rolled /metrics handler. Counters, gauges and
// classic histograms are supported; every sample carries explicit labels.
//
// Layout:
//   - registry.go   — metric types, exposition writer, /metrics handler
//   - http.go       — HTTP middleware (requests, durations, error tiers),
//     WS connection gauges, engine/degradation gauges
//   - aeronmon.go   — Aeron media-driver CnC file counters reader +
//     monitor (Task 7.3.8: backpressure, NAK/loss, subscriber lag)
//   - natsmon.go    — JetStream stream/consumer gauges (Task 7.3.8)
//   - bridge.go     — bridge.health.<shard> heartbeat staleness watcher
//   - alerts.go     — alert-rule evaluator → ops.alerts.monitoring
//   - anomaly.go    — rolling-baseline error-rate anomaly detection
//   - dlq.go        — dead-letter store (JetStream ops-dlq + memory),
//     consumer dead-letter helper, admin HTTP handler
//
// Alerting conventions (Task 7.3.10, spec §2.7/§19.8): severities are the
// P0–P3 incident classes; firing alerts publish JSON on the
// `ops.alerts.monitoring` NATS subject — the same ops.alerts.* seam family
// as ops.alerts.settlement (Task 3.3.18) and ops.alerts.recovery (§27 #41).
package observability
