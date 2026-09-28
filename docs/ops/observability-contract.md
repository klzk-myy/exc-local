# Production Observability Contract

**Phase-09 Task 9.3.29 (item 1)** · **Authority:** spec §19.14, §19.3 · **Enforcement:** CI checks on metric naming/cardinality, log allowlist, and trace config — enforcement code `services/internal/ops/observability.go` **pending**; this document is the contract the code will enforce.

## 1. Metrics

### Naming convention

- Prefix `exchange_` for platform-domain series (`exchange_errors_total`, `exchange_degradation_mode`, `exchange_circuit_breaker_open`, `exchange_alert_dispatch_errors_total`).
- Component-owned prefixes for infrastructure (`aeron_*`, `bridge_*`, `nats_*`, `wal_*`, `engine_*`, `reconciliation_*`, `clock_offset_nanoseconds`, `daemon_up`, `watchdog_heartbeat_timestamp_seconds`, `loop_latency_microseconds` per Task 9.3.28 telemetry).
- Standard request surface: `http_requests_total` (denominator for all SLI ratios).
- Exposition: Prometheus text v0.0.4, hand-rolled registry `services/internal/observability/registry.go` (`ServeMetrics` — `/metrics` + `/healthz`), no `client_golang` dependency.
- Labels: `service`, `shard`, `tier` (L0–L3), `code`, `kind`, `channel`/`stream_id`, `stream`/`consumer`, `mode`, `breaker`. No unbounded label values (no order IDs, account IDs, IPs, seqs).

### Per-service cardinality budget

| Service (binary, port) | Scrapes today | Budget (series) | Notes |
|---|---|---|---|
| `gateway` :8080 | yes (`gateway` job) | ≤ 3,000 | route labels bounded to registered routes only |
| `marketdata` :8081 | yes | ≤ 2,000 | conflation/WS client metrics; `/readyz` depth reporting stub retained |
| `fix` :8082 | yes | ≤ 2,000 | session metrics land Phase-18 |
| `settlement` :8083 | yes | ≤ 2,000 | ingest/flush series |
| `compliance` :8084 | yes | ≤ 2,000 | screening/surveillance counters land Phase-21 |
| `admin` :8085 | yes — **primary ops target** | ≤ 5,000 | hosts NATS monitor, Aeron CnC monitor, bridge heartbeat watcher, alert evaluator |
| `bridge` 9100+N (per shard) | yes | ≤ 1,500/instance | `bridge_buffer_depth`, `bridge_heartbeat_age_seconds`, `aeron_*` |
| node-exporter :9100 | yes | infra-owned | host memory/CPU for `shard-health` |
| `nats-exporter` :7777 | optional | — | JetStream gauges already exported by `admin`; use only for server-level stats |
| core engine | pending (engine metrics via `engine_ipc_ring_depth` + watchdog telemetry) | ≤ 2,000/shard | `LatencyHistogram` export path pending engine-side exporter |

Scrape/eval cadence: **15s** global (`deploy/prometheus/prometheus.yml`; Task 7.3.4). Exceeding a budget fails CI once `services/internal/ops/observability.go` lands; interim enforcement is PR review against this table.

## 2. Logs

- Format: structured `slog` JSON (Go services) / Boost.Log structured (C++ core) → Loki (spec §19.3).
- Fields always present: `ts`, `level`, `service`, `shard` (where applicable), `trace_id` (§3 below), `msg`, `code` (error-code registry id when rejecting).
- **PII redaction — allowlist model:** per-service allowlisted fields only; denylist redaction is not acceptable for money-movement logs. Until `services/internal/ops/observability.go` lands, the reviewable allowlist per service is:

| Service | Allowlisted log fields (beyond base) |
|---|---|
| gateway | route, tier, error code, latency bucket, client-id hash |
| settlement | journal id, batch id, GL account codes, amounts/currency (no names) |
| bridge | stream, shard, symbol, depths, ack timings |
| engine (C++) | seqs, shard, event types, watermark counters — never order payloads beyond IDs |
| compliance | screening verdict ids, rule ids — subject PII stays in PG, not logs |
| recovery/wal | seqs, offsets, outcomes (`recovery_reports` fields) |

- Retention windows: hot Loki 30d ops / compliance-relevant event journals 5y — unified schedule per [`../compliance/data-retention.md`](../compliance/data-retention.md) (Task 9.3.22; enforcer `services/internal/operations/retention/`).

## 3. Traces

- Collector: OpenTelemetry → Jaeger/Tempo (spec §19.3; collector config `deploy/otel/otel-collector.yaml`, Jaeger dev harness `deploy/otel/jaeger-compose.yaml` — Task 9.3.11); SDK seams in `services/internal/middleware/tracing.go` + engine-side emission pending Task 9.3.29 verification.
- **Sampling policy:** head-based **1%** baseline + **tail-based** capture on errors and slow requests (above per-endpoint SLI). Tail rules keyed on error tier (L0–L2 always captured) and duration.
- **Aeron `trace_id` propagation:** the engine↔services hop carries a `trace_id` header in the IPC frame (completes the T09-003 continuity claim; header format owned by `services/internal/ops/observability.go` — pending). Until that lands, continuity across the Aeron hop is reconstructed from `seq` + shard + timestamp correlation.
- Per-endpoint SLIs beyond the generic 99.99%/5ms: route-level latency histograms keyed by registered route id (registry `services/internal/gateway/routes_v1.go`) — pending `slo_rules.yml` recording rules (Task 9.3.14).

## 4. Alert surfaces (contract with ops)

- Prometheus-side rules: `deploy/prometheus/rules/exchange-alerts.yml` → Alertmanager → PagerDuty (`deploy/prometheus/alertmanager.yml`).
- In-process fail-safe: `ops.alerts.monitoring` (+ siblings `ops.alerts.settlement`, `ops.alerts.recovery`, `ops.alerts.support`) — rule ids match the Prometheus alertnames (`services/internal/observability/rules.go`).
- Every new alert rule must carry: severity label, team label, `runbook:` annotation → `docs/runbooks/` link (wired by Phase-13 Task 13.3.3).
- Monitoring self-check: `exchange_alert_dispatch_errors_total` must stay 0 ([alert-dispatch-errors runbook](../runbooks/alert-dispatch-errors.md)).

## 5. Dashboards of record

`deploy/grafana/dashboards/`: `system-overview` (venue mode + golden signals), `shard-health` (per-shard engine/watchdog/PTP), `ipc-backbone` (Aeron/bridge/NATS), `trading` (orders/fills/rates). Dashboard JSON is provisioned (`deploy/grafana/provisioning/`) — edits go through the same PR path, never UI-only changes.
