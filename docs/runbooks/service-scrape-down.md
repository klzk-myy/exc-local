# Runbook: scrape target down / redundancy loss (`ScrapeTargetDown`, `BridgeShardDown`, `BridgeShardRedundancyLost`, `NodeExporterDown`)

**Severity:** P1 (service or bridge target down) / P2 (bridge redundancy) / P3 (node-exporter) · **Metric:** `up{job,instance}` (Prometheus synthetic) · **Owner:** SRE

## Symptom

Prometheus cannot scrape a configured target, or too few of the expected per-shard bridge targets are healthy.

## Diagnosis

1. Is it the process or the scrape path? `curl -s http://<target>/healthz` from the Prometheus container's network — services serve `/healthz` + `/metrics` on their configured port (`services/config.example.yaml`: gateway 8080, marketdata 8081, fix 8082, settlement 8083, compliance 8084, admin 8085; bridge per-shard 9100+N).
2. If `/healthz` answers but `/metrics` hangs → exposition wedged (registry lock), not a dead service.
3. Cross-check `ops_status`/`status:component:*` (Task 9.3.25 aggregator) — it probes the same dependencies once per second and may already carry the failure reason.

### bridge-redundancy

`prometheus.yml` scrapes `bridge-0:9100` + `bridge-1:9101` (one bridge per matching shard). One down = the surviving bridge is a single point of failure on the engine→JetStream fan-out. Check `bridge_health_nats_connected`/`bridge_heartbeat_age_seconds` on the dead shard's side before restart.

### node-exporter

`up{job="node"}==0` blinds memory/CPU/disk rules for that host only — services keep running. Restart the exporter (`deploy/ansible` role or systemd unit on bare metal); a dead exporter on a shard host also hides the `WALVolumeNearlyFull` signal, so fix fast anyway.

## Mitigation

1. Dead service: restart per [daemon-supervision runbook](../ops/daemon-supervision.md); if it is `admin`, remember the in-process alert evaluator lives there — alerting itself just lost a leg ([alert-dispatch-errors.md](./alert-dispatch-errors.md)).
2. Dead bridge: restart is safe — the bounded buffer/spool absorbs the gap up to the §2.3.1 cap; check `bridge_buffer_depth` after it rejoins.
3. Redundancy lost: do not take the surviving bridge down for any reason until the dead one is healthy.

## Escalation

- `ScrapeTargetDown`/`BridgeShardDown` are P1 — unreachable-for-2m on a money-path service is indistinguishable from outage until proven otherwise.
- A scrape-down on `admin` that coincides with other alerts is a *monitoring-loss* incident on top of the original — say so in the incident channel.
