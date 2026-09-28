# Prometheus / Alertmanager deployment assets

Phase-07 Tasks 7.3.4/7.3.8/7.3.10, spec §19.3/§2.7.

## Layout

| Path | Purpose |
|---|---|
| `prometheus.yml` | Scrape config — 15s interval; one job per Go service (ports from `services/config.example.yaml`) + bridge per-shard targets (9100+N) + node-exporter. |
| `rules/exchange-alerts.yml` | Server-side alert rules — mirror of the in-process evaluator (`services/internal/observability/rules.go`); thresholds verbatim from Tasks 7.3.8/7.3.10. |
| `alertmanager.yml` | PagerDuty routing by `severity` label (p0 page / p1 page / p2 ticket+slack / p3 slack) + P0-inhibits-lower inhibition. |

## Secrets

`alertmanager.yml` uses `${VAR}` placeholders — inject at container start
(envsubst / secrets injector). Vault/KMS is the secrets store
(Phase-13.5 Task 13.5.3.5); never commit real keys.

- `PAGERDUTY_P0_SERVICE_KEY`, `PAGERDUTY_P1_SERVICE_KEY`, `PAGERDUTY_P2_SERVICE_KEY`
- `SLACK_OPS_WEBHOOK_URL`

## Two alert paths (deliberate redundancy)

1. **In-process evaluator** (admin service): samples its own registry +
   NATS/bridge/CnC monitors, publishes `Alert` JSON to
   `ops.alerts.monitoring` (JetStream) + log. Works even when Prometheus
   is down.
2. **Prometheus → Alertmanager → PagerDuty**: the rules file uses the
   same thresholds so both paths agree on when an incident is real.

Rule IDs are shared (`l0_errors`, `bridge_buffer_depth`, …) so the two
paths dispatch the same codes (`L0_ERROR_OBSERVED`, `BRIDGE_BUFFER_DEPTH_HIGH`, …).

## Assumptions / deviations (see spec §27 candidate notes)

- `engine_ipc_ring_depth` alert thresholds assume a 1M-entry ring
  (80% = 800k, 95% = 950k per spec §2.7.3 utilization ladder). If the
  deployed ring capacity differs, adjust `IPCRingSaturated`/`IPCRingCritical`.
- `bridge.metrics_addr` shard convention is 9100+N — extend the `bridge`
  job target list when shards are added.
- NATS server-native stats (:8222) are JSON, not exposition format;
  JetStream stream/consumer gauges are exported by the admin service
  (`nats_stream_*`, `nats_consumer_*`). The `nats-exporter` job is
  optional if prometheus-nats-exporter is deployed for server internals.
