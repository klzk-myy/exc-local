# Grafana deployment assets

Phase-07 Task 7.3.5 (+ Task 7.3.8 IPC backbone dashboard), spec §19.3.

## Layout

| Path | Purpose |
|---|---|
| `provisioning/datasources/prometheus.yaml` | Prometheus (default) + optional Loki datasource. `PROMETHEUS_URL`/`LOKI_URL` injected at container start. |
| `provisioning/dashboards/dashboards.yaml` | File provider — loads every `dashboards/*.json` into folder "Exchange Operations". |
| `provisioning/alerting/contactpoints.yaml` | Unified-alerting contact points (PagerDuty p0/p1/p2 + Slack p3) and notification policy (Grafana ≥ 9). |
| `dashboards/system-overview.json` | Golden signals: traffic, errors by L0–L3 tier, p50/p99 latency, saturation; degradation-mode + circuit-breaker status; 99.99% availability stat. |
| `dashboards/shard-health.json` | Per-shard throughput, bridge publish latency, IPC ring depth vs §2.7.3 ladder, WAL lag, heartbeat staleness, drop/malformed rates, host CPU/mem. |
| `dashboards/trading.json` | Event volume by stream, order API flow by status, top streams, L2/L3 rejection mix, reconciliation runs/mismatches. |
| `dashboards/ipc-backbone.json` | Task 7.3.8: Aeron driver/CnC counters (NAKs, backpressure), subscriber lag, bridge buffer/publish health, JetStream consumer lag + stream storage, DLQ depth. |

## Mounting

```yaml
# docker-compose / compose.yml fragment
grafana:
  image: grafana/grafana:11.x
  environment:
    PROMETHEUS_URL: http://prometheus:9090
    LOKI_URL: http://loki:3100          # optional
    PAGERDUTY_P0_SERVICE_KEY: ${PAGERDUTY_P0_SERVICE_KEY}
    PAGERDUTY_P1_SERVICE_KEY: ${PAGERDUTY_P1_SERVICE_KEY}
    PAGERDUTY_P2_SERVICE_KEY: ${PAGERDUTY_P2_SERVICE_KEY}
    SLACK_OPS_WEBHOOK_URL: ${SLACK_OPS_WEBHOOK_URL}
  volumes:
    - ./deploy/grafana/provisioning:/etc/grafana/provisioning:ro
    - ./deploy/grafana/dashboards:/etc/grafana/dashboards:ro
```

## Notes

- All dashboard queries target the metric set exported by
  `services/internal/observability` + `services/internal/bridge`
  (`/metrics`, exposition v0.0.4) — no prometheus client dependency in
  the services.
- Panels referencing `node_*` series require the `node` scrape job;
  they render empty until node-exporter is deployed.
- "Top symbols" fidelity: the bridge routes per-symbol events into
  `{stream}.{shard}.{symbol}` JetStream subjects; per-symbol series are
  exposed via `nats_stream_*`/`nats_consumer_*` labels, so the trading
  dashboard shows top streams rather than per-symbol cardinality (kept
  deliberately bounded — see §27 deviation note candidate).
