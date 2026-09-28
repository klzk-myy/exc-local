# Runbook: `AlertDispatchErrors` — the alerting pipeline itself is failing

**Severity:** P1 (pages `pagerduty-p1`) · **Rule:** `increase(exchange_alert_dispatch_errors_total[5m]) > 0` (Prometheus-side only; the in-process evaluator cannot page about its own dispatch failures) · **Domain:** monitoring self-check — a firing `AlertDispatchErrors` means pages may be silently lost.

## Symptom

`ops.alerts.monitoring` publishes are erroring on the labeled service. The in-process evaluator (`services/internal/observability/alerts.go`, `OpsAlertSubject = "ops.alerts.monitoring"`) increments `exchange_alert_dispatch_errors_total` when the NATS publish fails — meaning the NATS-side fail-safe alert path is degraded while the rule still evaluates.

## Diagnosis

1. Check NATS health first: `natsctl health` (`services/cmd/natsctl`) prints per-stream/consumer lag and `MissingStream`; the server-side equivalent is `:8222/jsz` on any `nats-server` node (3-node cluster, ports 4222/6222).
2. Determine which leg failed:
   - Core NATS unreachable → the alert subject is a plain core publish (`Conn().Publish`) — no JetStream needed. Failure implies the NATS cluster or client conn is down entirely.
   - JetStream unhealthy → `ops.alerts.*` still publishes (core), but consumers lagging will also trip `NATSConsumerPendingHigh`.
3. Verify the Prometheus-side path still works: Alertmanager at `alertmanager:9093`, `amtool`/`/api/v1/status`; Prometheus alert rule evaluation continues independently (15s eval interval).
4. Check whether the admin service (`services/cmd/admin`, :8085 — the ops scrape target hosting the NATS monitor + evaluator) is itself the failing service label.

## Mitigation

1. Restore NATS connectivity: 3-node cluster tolerates one node loss (JetStream R3, `StreamReplicas = 3` in `services/internal/nats/streams.go`). Restart the dead node; quorum reforms automatically. Compose topology reference: `docker-compose.dev.yml` (`nats-1/2/3`).
2. If the whole cluster is down, bring it up in order per spec §19.13.2 Stage 0, then run `natsctl init` to reconcile the 7 canonical streams (`trades`, `settlements`, `compliance`, `analytics`, `funding`, `margin-events`, `surveillance`) + `natsctl dlq` for `ops-dlq`.
3. While `ops.alerts.monitoring` is impaired, Prometheus-side alerting is the only paging path — increase scrutiny: watch `shard-health`/`ipc-backbone` Grafana dashboards directly and poll `GET /api/v1/system/status` for the venue mode.
4. After NATS recovers, confirm the evaluator resumed: `exchange_alert_dispatch_errors_total` flat for 5m, and a test rule evaluation publishes (evaluator logs on the service that fired).

## Escalation

- P1: On-call SRE. If NATS cannot be restored within 30min → treat as monitoring outage, re-grade P0-adjacent (you are flying blind on the fail-safe path), and declare per [incident-escalation.md](./incident-escalation.md).
- Pair with [nats-consumer-pending-high.md](./nats-consumer-pending-high.md) when consumer lag accompanies the dispatch failure.
