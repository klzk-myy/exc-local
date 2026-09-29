# On-Call Runbooks — Index & Operating Contract

**Owner:** SRE on-call rotation · **Phase:** Phase-09 Task 9.3.5 · **Authority:** spec §19.10 (47+ alert SOPs), §2.7 (error tiers), §19.8 (incident severities)

Every runbook in this directory follows one structure, in this order:

1. **Symptom** — what fired (alert rule name, Prometheus expr or in-process rule id, severity label, ops.alerts subject).
2. **Diagnosis** — ordered checks with the exact metric, log line, endpoint, CLI, or table to inspect.
3. **Mitigation** — ordered actions; every action names the mechanism it drives (binary, endpoint, Redis key, migration-backed table, config path).
4. **Escalation** — who to page and when, per the P0–P3 matrix in [incident-escalation.md](./incident-escalation.md).

If a step references a mechanism that does not exist yet, it is marked `— pending <phase/task>` rather than improvised. Nothing in these pages invents tooling.

---

## 1. Alert dispatch paths

Two independent paths carry the same alert identities; both must be checked when an alert is silent:

| Path | Source | Transport | Routing |
|---|---|---|---|
| Prometheus-side | `deploy/prometheus/rules/exchange-alerts.yml` + `deploy/prometheus/alerts.yml` (Task 13.3.3 domain pack) + `deploy/monitoring/{capacity,redis-sentinel}-alerts.yml` (evaluated at 15s, scrape 15s per `deploy/prometheus/prometheus.yml`) | Alertmanager `deploy/prometheus/alertmanager.yml` | `severity: p0` → `pagerduty-p0` (group_wait 0s, repeat 15m); `p1` → `pagerduty-p1` (repeat 1h); `p2` → `pagerduty-p2-ticket` + `#exchange-ops`; `p3` → `pagerduty-p3-ticket` (info-severity PD ticket) + `#exchange-ops`. Inhibit rules: firing p0 suppresses p1–p3 on same `service`+`shard`; p1 suppresses p3. |
| In-process fail-safe | `services/internal/observability/rules.go` + `alerts.go` evaluator | NATS subject `ops.alerts.monitoring` (siblings: `ops.alerts.settlement`, `ops.alerts.recovery`, `ops.alerts.support`) | consumed by the ops alerting bridge — **pending Phase-13 Task 13.3.3** for the full PagerDuty bridge; today these payloads are inspectable via `natsctl` and the `admin` service metrics. |

PagerDuty integration keys are injected at container start from `PAGERDUTY_P0_SERVICE_KEY` / `PAGERDUTY_P1_SERVICE_KEY` / `PAGERDUTY_P2_SERVICE_KEY` / `PAGERDUTY_P3_SERVICE_KEY` and `SLACK_OPS_WEBHOOK_URL` — sourced from Vault/KMS, never committed.

Every alert rule in the loaded files carries a `runbook:` annotation resolved by `scripts/ci/check_alert_rules.py` (CI gate — link must point at a real file/anchor in this repo).

## 2. Alert → runbook matrix (deployed rules)

Runbooks exist for every alert rule currently deployed in `deploy/prometheus/rules/exchange-alerts.yml` and registered in-process by `AddStandardRules` (`services/internal/observability/rules.go`). The Prometheus alert name and the in-process `alert_id` are listed together — they are the same rule evaluated twice.

| Alert (Prometheus) | In-process rule id | Severity | Runbook |
|---|---|---|---|
| `L0ErrorObserved` | `l0_errors` | p0 | [l0-error-observed.md](./l0-error-observed.md) |
| `L1ErrorsSustained` | `l1_errors_sustained` | p1 | [l1-errors-sustained.md](./l1-errors-sustained.md) |
| `L2RejectionSpike` | `l2_rejection_spike` | p2 | [l2-rejection-spike.md](./l2-rejection-spike.md) |
| `ErrorRateAnomaly` | `error_rate_anomaly` | p2 | [l2-rejection-spike.md](./l2-rejection-spike.md) |
| `AlertDispatchErrors` | — | p1 | [alert-dispatch-errors.md](./alert-dispatch-errors.md) |
| `AeronSubscriberLag` | `aeron_subscriber_lag` | p2 | [aeron-subscriber-lag.md](./aeron-subscriber-lag.md) |
| `AeronDriverDown` | — | p1 | [aeron-driver-down.md](./aeron-driver-down.md) |
| `BridgeBufferDepthHigh` | `bridge_buffer_depth` | p1 | [bridge-buffer-depth-high.md](./bridge-buffer-depth-high.md) |
| `BridgeHeartbeatStale` | `bridge_heartbeat_stale` | p1 | [bridge-heartbeat-stale.md](./bridge-heartbeat-stale.md) |
| `BridgeSubscriptionDisconnected` | — | p1 | [bridge-subscription-disconnected.md](./bridge-subscription-disconnected.md) |
| `NATSConsumerPendingHigh` | `nats_consumer_pending` | p1 | [nats-consumer-pending-high.md](./nats-consumer-pending-high.md) |
| `DLQEntriesAccumulating` | — | p3 | [dlq-entries-accumulating.md](./dlq-entries-accumulating.md) |
| `AvailabilityBurnFast` | — | p0 | [availability-slo-burn.md](./availability-slo-burn.md) |
| `AvailabilityBurnSlow` | — | p2 (ticket) | [availability-slo-burn.md](./availability-slo-burn.md) |
| `DegradationModeActive` | — | p2 | [degradation-mode-active.md](./degradation-mode-active.md) |
| `CircuitBreakerOpen` | — | p2 | [circuit-breaker-open.md](./circuit-breaker-open.md) |
| `ReconciliationMismatch` | — | p1 | [reconciliation-mismatch.md](./reconciliation-mismatch.md) |
| `WALLagGrowing` | — | p1 | [wal-lag-growing.md](./wal-lag-growing.md) |
| `IPCRingSaturated` | — | p2 | [ipc-ring-pressure.md](./ipc-ring-pressure.md) |
| `IPCRingCritical` | — | p0 | [ipc-ring-pressure.md](./ipc-ring-pressure.md) |
| `PTPClockOffsetExceeded` / `PTPNotSynchronized` / `PTPStale` / `PTPUnavailable` | — | p1 | [time-sync-loss-halt.md](./time-sync-loss-halt.md) |
| `EdgeDeniesSustained` / `WAFBlocksSurge` / `WAFChallengesSustained` / `EdgeDDoSSuspected` | — | p2/p2/p2/p1 | [../../deploy/edge/ddos-playbook.md](../../deploy/edge/ddos-playbook.md) |
| `RateLimitUtilizationHigh` | — | p2 | [rate-limit-utilization.md](./rate-limit-utilization.md) |
| `RedisEvictionsObserved` / `RedisEvictionsSustained` / `RedisMemoryHeadroomLow` / `RedisMemoryHeadroomCritical` | — | p2/p1/p2/p1 | [redis-eviction-pressure.md](./redis-eviction-pressure.md) |

### Task 13.3.3 domain pack (`deploy/prometheus/alerts.yml`)

| Alert | Severity | Runbook |
|---|---|---|
| `GatewayP99LatencySLOBreach` / `GatewayP99LatencyDegraded` / `HTTPP99LatencySLOBreach` / `MarketDataWSPushP99High` / `BridgePublishLatencyHigh` | p2/p1/p3/p2/p2 | [http-latency-slo.md](./http-latency-slo.md) |
| `GatewayTrafficCollapse` / `MarketDataFramesStalled` / `MarketDataDeltasDropped` | p2 | [throughput-collapse.md](./throughput-collapse.md) |
| `BridgePublishStalled` / `BridgeEventsDropped` | p1 | [bridge-buffer-depth-high.md](./bridge-buffer-depth-high.md), [throughput-collapse.md](./throughput-collapse.md) |
| `NATSConsumerPendingWarn` / `NATSConsumerAckPendingHigh` / `NATSConsumerRedeliveryStorm` / `LoadShedStageActive` / `LoadShedRejectionsOngoing` | p3/p2/p3/p2/p2 | [queue-depth-backlog.md](./queue-depth-backlog.md), [nats-consumer-pending-high.md](./nats-consumer-pending-high.md) |
| `EngineIPCRingDepthWarn` / `EngineIPCSeqStalled` / `EngineIPCTelemetryAbsent` | p3/p1/p3 | [ipc-ring-pressure.md](./ipc-ring-pressure.md) |
| `WALLagCritical` | p1 | [wal-lag-growing.md](./wal-lag-growing.md) |
| `WALLagNotDraining` / `WALLagTelemetryAbsent` | p2/p3 | [wal-archive-stalled.md](./wal-archive-stalled.md) |
| `HostMemoryPressure` / `HostMemoryHigh` / `HostSwapInUse` / `WALVolumeNearlyFull` / `HostCPUSaturated` / `HostCPUCritical` / `HostRunqueueHigh` | p1/p2/p3/p1/p2/p1/p3 | [host-resource-pressure.md](./host-resource-pressure.md) |
| `ClientBlockingModeActive` / `DegradationModeSustained` / `MaintenanceModeProlonged` / `DegradationTelemetryAbsent` | p1/p1/p3/p3 | [degradation-mode-active.md](./degradation-mode-active.md) |
| `CircuitBreakerOpenSustained` / `CircuitBreakerFlapping` / `MultipleCircuitBreakersOpen` / `CircuitBreakerTelemetryAbsent` | p1/p2/p1/p3 | [circuit-breaker-open.md](./circuit-breaker-open.md) |
| `ReconciliationMismatchBurst` / `ReconciliationMismatchMultipleKinds` | p1 | [reconciliation-mismatch.md](./reconciliation-mismatch.md) |
| `ReconciliationSweepMissing` / `ReconciliationTelemetryAbsent` | p2/p3 | [reconciliation-sweep-stalled.md](./reconciliation-sweep-stalled.md) |
| `ScrapeTargetDown` / `BridgeShardDown` / `BridgeShardRedundancyLost` / `NodeExporterDown` | p1/p1/p2/p3 | [service-scrape-down.md](./service-scrape-down.md), [bridge-heartbeat-stale.md](./bridge-heartbeat-stale.md) |
| `NATSBackboneDisconnected` / `AdminMonitorTelemetryAbsent` | p1/p2 | [nats-backbone-down.md](./nats-backbone-down.md) |
| `SettlementServiceDown` / `SettlementErrorsElevated` / `SettlementP99Slow` / `SettlementTrafficSilent` | p1/p2/p2/p3 | [settlement-service-degraded.md](./settlement-service-degraded.md) |
| `FundingMoneyPath5xx` / `FundingRejectionStorm` / `FundingRouteP99Slow` / `FundingAdminOpsErrors` | p1/p2/p2/p3 | [funding-path-errors.md](./funding-path-errors.md) |

Rule-load validation (count ≥47, all 12 domains, severity+runbook on every rule, links resolve): `python3 scripts/ci/check_alert_rules.py` — also validates the alertmanager severity→receiver routing.

Note on `AvailabilityBurnFast`/`Slow` severities: the deployed Prometheus labels are `p0`/`p1`-class pages; Task 9.3.14 phrases the model as 14.4×→P1, 6×→P2, 1×→P3. The deployed labels are stricter on the fast burn; the SLO burn model itself is documented in [../ops/slo-policy.md](../ops/slo-policy.md).

## 3. Condition runbooks (no single alert rule)

These fire as states or table rows rather than threshold alerts; they are the pages the alert-layer rules point at.

| Condition / code | Severity | Runbook |
|---|---|---|
| `WAL_RECOVERY_HALT` + `recovery_reports` row (migration 065) | P1 | [wal-recovery-halt.md](./wal-recovery-halt.md) |
| Matching-loop stall / watchdog trip / standby promotion | P0 | [engine-halt-failover.md](./engine-halt-failover.md) |
| `TIME_SYNC_LOSS_HALT` (PTP offset >100µs, spec §19.4) | P1 | [time-sync-loss-halt.md](./time-sync-loss-halt.md) |
| PostgreSQL primary loss / replica promotion | P1 | [postgres-failover.md](./postgres-failover.md) |
| Redis Sentinel master failover | P1 | [redis-sentinel-failover.md](./redis-sentinel-failover.md) |
| `SECRET_ROTATION_OVERDUE` (spec §19.14; migration 089 pending) | P2 | [secret-rotation-overdue.md](./secret-rotation-overdue.md) |
| Incident classification, paging tree, war-room protocol | all | [incident-escalation.md](./incident-escalation.md) |
| Quarterly DR drill program & post-drill report | scheduled | [dr-drill.md](./dr-drill.md) |
| Monthly PostgreSQL PITR drill (RPO ≤15s / RTO ≤5min proof) | scheduled | [pitr-monthly-drill.md](./pitr-monthly-drill.md) |
| BCP stand-down decision | P0 governance | [bcp-standdown.md](./bcp-standdown.md) |
| BCP go-forward modes (manual capture / withdrawal-only / frozen-reconcilable) | P0 governance | [bcp-goforward.md](./bcp-goforward.md) |

Existing runbooks maintained elsewhere (link, do not duplicate):

- ClickHouse backup/restore & failure modes — [`deploy/clickhouse/RUNBOOK.md`](../../deploy/clickhouse/RUNBOOK.md) (Task 4.3.6).

## 4. Alert categories — Task 13.3.3 status

Task 9.3.5 targets 47+ alert types; the full domain set now loads from `deploy/prometheus/alerts.yml` + `rules/exchange-alerts.yml` + `deploy/monitoring/{capacity,redis-sentinel}-alerts.yml` (95 rules at last `check_alert_rules.py` run, all 12 Task 13.3.3 domains covered). Remaining gaps are emitter-side, not rule-side — rules whose feeders are unwired carry an `EMITTER-GAP` comment and a paired `*TelemetryAbsent` rule that tracks the gap as a p3 ticket. Phase-13.5 Task 13.5.3.4 validates runbook coverage.

| Category | Anchor mechanism (where it exists) | Status |
|---|---|---|
| Matching p99 > 50µs / REST p99 > 5ms latency SLO | `LatencyHistogram` in `core/src/matching/EngineLoop.cpp`; `http_request_duration_seconds` | REST p99 deployed (`GatewayP99LatencySLOBreach`/`Degraded`); engine-side p99 still needs a core exporter — pending Phase-13 |
| Order-gateway 5xx rate / route-level SLI | gateway `/metrics` on :8080 | covered by `L1ErrorsSustained` + funding-domain rules; per-route SLI matrix pending |
| Per-endpoint SLIs beyond generic 99.99%/5ms | spec §19.14 | pending Phase-09 Task 9.3.29 contract ([../ops/observability-contract.md](../ops/observability-contract.md)) |
| PostgreSQL replica lag > 5s | `deploy/postgres/` semi-sync config | runbook exists ([postgres-failover.md](./postgres-failover.md)); needs a postgres exporter job in prometheus.yml — emitter gap, not yet scraped |
| Redis Sentinel quorum loss / lag > 100ms | `deploy/redis/sentinel.conf`, `config/redis-sentinel.yaml` | **deployed** — `deploy/monitoring/redis-sentinel-alerts.yml` is now in `rule_files` (drill harness per Phase-09 Task 9.3.20; exporter emitting `sentinel_*`/`redis_*` series is the remaining gap) |
| Price-oracle staleness > 5s / `PRICE_ORACLE_UNAVAILABLE` | spec §19.13.1 `oracle-service` | pending Phase-19.5 (service + feeds) |
| Liquidation scanner missed 2s cadence | spec §19.13.1 `liquidation-scanner` | pending Phase-19 |
| Settlement ingest lag / GL imbalance (`LEDGER_IMBALANCE_ABORT`) | `services/cmd/settlement` (scaffold), GL migrations 036/088 | service liveness/error/latency deployed (Task 13.3.3 pack); batch/GL-level rules pending Phase-03 emitters |
| Banking-rail return codes / suspense growth | `services/cmd/banking_rails` | pending Phase-11/Phase-24 |
| Swap-free / Tom-Next roll failure (17:00 ET) | `services/cmd/tomnext_rollover` | pending Phase-03 Task 3.3.7 |
| Proof-of-reserves Merkle publish failure | `services/cmd/proof_of_reserves`, `exchange merkle` | partial — `exchange merkle` CLI exists; CronJob pending Phase-13 Task 13.3.7 |
| Canary/blue-green rollback (`DEPLOYMENT_AUTOMATED_ROLLBACK`) | [`../ops/blue-green-deploy.md`](../ops/blue-green-deploy.md) (Task 9.3.3), [`../ops/shard-binary-swap.md`](../ops/shard-binary-swap.md) (Task 9.3.16) | deploy docs exist; canary-rollback automation pending Task 9.3.26 |
| Cache-warming failure (P0 keys >5s / P1 keys >30s) | `exchange warm-cache` CLI | pending Phase-09 Task 9.3.8 (CLI not yet registered) |
| Daemon watchdog trip / `exchange-watchdogd` probe failure | [`../ops/daemon-supervision.md`](../ops/daemon-supervision.md) (Task 9.3.28), [`../ops/daemon-inventory.md`](../ops/daemon-inventory.md) | supervision runbook exists; `services/cmd/watchdogd` binary pending |
| Sanctions provider outage (scoped degradation) | `SANCTIONS_SERVICE_UNAVAILABLE` registered in `services/internal/errs/codes.go` | pending Phase-21 feed handler |
| `RETENTION_POLICY_VIOLATION` (nightly enforcer) | `services/internal/operations/retention/` enforcer exists; policy doc [`../compliance/data-retention.md`](../compliance/data-retention.md) | deployed (Task 9.3.22) — `RetentionPolicyViolation` rule now loaded via `deploy/monitoring/capacity-alerts.yml` (emitter `exchange_retention_violations_total` is the remaining gap) |
| `VDP_SLA_BREACH` (vulnerability disclosure SLA) | `vulnerability_disclosures` (migration 081 — pending) | pending Phase-13.5 Task 13.5.3.8 |
| Clock offset `clock_offset_nanoseconds` > 100µs | spec §19.4, `services/internal/timesync` | runbook exists ([time-sync-loss-halt.md](./time-sync-loss-halt.md)) |

## 5. On-call rotation (PagerDuty)

- Rotation: weekly primary + secondary, handover Mondays 09:00 UTC (aligned to the 24/5 FX week — Sunday 21:00 UTC open → Friday 22:00 UTC close; weekend pages still route to the rotation).
- Services: `pagerduty-p0` (phone/page, repeat 15m), `pagerduty-p1` (page, repeat 1h), `pagerduty-p2-ticket` (ticket + Slack), `pagerduty-p3-ticket` (info-severity PD ticket + `#exchange-ops` — Task 13.3.3 P3=ticket contract).
- Escalation timing and the full matrix: [incident-escalation.md](./incident-escalation.md).
- PagerDuty provider outage fallback: SMS/phone tree maintained in the escalation doc; this is a named edge case of Task 9.3.18.

## 6. Tabletop exercises

Four scenarios run per Task 9.3.5, each inside its SLA (T-labels used deliberately so exercise tiers do not collide with P0–P3 incident severities, per spec §19.8 note):

| Scenario | Exercise target (SLA) | Runbooks exercised | Cadence | Record |
|---|---|---|---|---|
| T1 — WAL tail CRC damage mid-batch (chaos `s1_crash_mid_batch`) | detect → service restored via Level-1 repair ≤ 10s engine RTO | [wal-recovery-halt.md](./wal-recovery-halt.md), [engine-halt-failover.md](./engine-halt-failover.md) | quarterly (with [dr-drill.md](./dr-drill.md)) | time-to-detect, time-to-restore, `recovery_reports.outcome` |
| T2 — PostgreSQL primary loss | promote ≤ 5min RTO, ≤ 15s RPO | [postgres-failover.md](./postgres-failover.md) | quarterly | `pg_ctl promote` timestamp vs last-received WAL LSN |
| T3 — Bridge/JetStream outage → spool growth | spool <5GB, no `ReadOnly` trip unless spool bound hit | [bridge-buffer-depth-high.md](./bridge-buffer-depth-high.md), [nats-consumer-pending-high.md](./nats-consumer-pending-high.md) | quarterly | `bridge_buffer_depth`, `/var/spool/exchange/` usage |
| T4 — Degradation-mode flap drill (ReadOnly enter/exit) | 30s hysteresis honored; no flapping transitions | [degradation-mode-active.md](./degradation-mode-active.md) | quarterly | `system:degradation:*` audit in Redis + alert timeline |

Chaos evidence harness for T1/T4-style injects: `tests/chaos/run.sh --scenarios "s1 s2 s3 s4 s5 s6"` (six scenarios; per-run `run.json` verdicts under `tests/chaos/results/`).

## 7. Change control

These runbooks are controlled documents. Any change to a runbook that alters thresholds or steps must be reconciled with the alert rule that cites it (the `runbook:` annotation in the loaded rule files — `deploy/prometheus/{alerts.yml,rules/exchange-alerts.yml}` and `deploy/monitoring/*.yml` — wired by Task 13.3.3 and link-checked by `scripts/ci/check_alert_rules.py`) and noted in the relevant phase plan. Runbook edits never tick acceptance checkboxes.
