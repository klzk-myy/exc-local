# Runbook: latency SLO breaches (`GatewayP99LatencySLOBreach`, `GatewayP99LatencyDegraded`, `HTTPP99LatencySLOBreach`, `MarketDataWSPushP99High`, `BridgePublishLatencyHigh`)

**Severity:** P2 (SLO watch) / P1 (>50ms gateway p99 — §19.8 P1 example) · **Metric:** `http_request_duration_seconds` histogram (services/internal/observability/http.go), `ws_push_latency_seconds`, `bridge_publish_latency_seconds_{sum,count}` · **Owner:** SRE

## Symptom

Request or push-path latency has exceeded the SLO bound for the alert's `for` window. These are *service-side* latency signals — the matching-engine p99 ≤50µs budget is measured inside the C++ core (EngineLoop histogram), not by these rules.

## Diagnosis

### gateway-p99

1. Per-route breakdown: `sum by (route, le) (rate(http_request_duration_seconds_bucket{service="gateway"}[5m]))` — find the hot route before touching infra.
2. Correlate with load: `load_shed_stage` >0 or `engine_ipc_ring_depth` growth means latency is a *symptom* of ring saturation → [ipc-ring-pressure.md](./ipc-ring-pressure.md).
3. Correlate with host pressure → [host-resource-pressure.md](./host-resource-pressure.md).
4. Check downstream deps the gateway blocks on: Redis coordination (rate-limiter Lua), Postgres, NATS publish. `tracing_spans_dropped` rising alongside latency = the tracer is backpressured too.

### per-service-slo

Same per-route drilldown on the named `{{ $labels.service }}`. Sustained >5ms p99 on a low-traffic service (admin, compliance) usually means a single slow handler — sort by `route`.

### ws-push-latency

`ws_push_latency_seconds{quantile="0.99"}` is the marketdata send-side reservoir snapshot (cmd/marketdata). Elevated p99 → check `ws_connections_active`/`ws_subscriptions_active` growth, `marketdata_frames_dropped_total` (slow-consumer saturation), and host CPU.

### bridge-publish-latency

Mean of `rate(bridge_publish_latency_seconds_sum)/rate(_count)` >250ms on a shard — JetStream RTT or spool fsync cost. Check `bridge_buffer_depth` trajectory and `bridge_publish_errors_total`; on bare metal also check the spool disk (`/var/spool/exchange`, see [host-resource-pressure.md](./host-resource-pressure.md)#disk).

## Mitigation

1. Shed pressure upstream if rings are involved — do **not** add traffic (no replays, no load tests) while a latency alert is firing.
2. Route-scoped slowness with normal deps → suspect a regression in the owning handler; check deploy recency (`deploy_window_open`, blue-green state in [../ops/blue-green-deploy.md](../ops/blue-green-deploy.md)).
3. If latency is caused by an L1 dependency fault, the correct venue response may already be a degradation mode — check `system:degradation:mode` in Redis and follow [degradation-mode-active.md](./degradation-mode-active.md).

## Escalation

- `GatewayP99LatencyDegraded` pages P1 (§19.8 lists gateway p99 >50ms as a P1 symptom). SLO-watch rules (p2/p3) escalate to P1 if latency keeps climbing for 15m after triage starts, or if any route returns 5xx alongside the latency.
