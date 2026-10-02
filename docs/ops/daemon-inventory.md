# Daemon Execution Inventory (Task 9.3.28, spec §19.13.1)

Authoritative runtime inventory: every daemon, its artifact in this repo,
supervisor, watchdog tier, and supervision artifact shipped here. K8s-tier
daemons live in `deploy/k8s/`; bare-metal in `deploy/systemd/`.

## Tier 1–2: Bare-metal core (systemd)

| Daemon | Repo artifact | Unit | Watchdog |
|---|---|---|---|
| `matching-engine` (leader) | `core/build/bin/matching_engine` → `/opt/exchange/bin/matching-engine` | `matching-engine@<shard>` | T1 `WatchdogSec=1s` (sd_notify build) + T2 in-loop `WatchdogThread` (100µs sample, 500µs warn, 2ms halt) + T3 watchdogd lease revocation |
| `matching-engine` (follower) | same binary, `-follower` | `matching-engine-follower@<shard>` | same |
| `aeronmd` | Aeron C media driver | `aeronmd.service` | T1 `WatchdogSec=1s`, conductor heartbeat |
| `ptp4l` / `phc2sys` | linuxptp | `ptp4l.service`, `phc2sys.service` | unit restart + watchdogd offset poll (>100µs → `TIME_SYNC_LOSS_HALT`) |
| `exchange-watchdogd` | `services/cmd/watchdogd` (Tier-3 supervisor) | `exchange-watchdogd.service` (+ K8s DaemonSet on service nodes) | `WatchdogSec=500ms`, drives `/dev/watchdog` indirectly via `RuntimeWatchdogSec=10s` |
| `redis-server` (×3) | `deploy/redis/redis.conf` | `redis-server.service` | `WatchdogSec=2s` + Sentinel quorum |
| `redis-sentinel` (×3) | `deploy/sentinel/sentinel-{1,2,3}.conf` | `redis-sentinel@<n>` | `WatchdogSec=2s`, quorum=2 |
| `postgres` | PG16 + `deploy/postgres/postgresql.conf` | distro unit + `postgresql@16-main.service.d-override.conf` | Patroni/Pacemaker leader election |
| `pgbouncer` | pgbouncer | distro unit + `pgbouncer.service.d-override.conf` | restart |
| `clickhouse-server` | ClickHouse | distro unit + `clickhouse-server.service.d-override.conf` | restart + Keeper health |
| `nats-server` (×3) | `deploy/nats/nats-{1,2,3}.conf` | `nats-server.service` (`NATS_NODE`) | restart + raft quorum check |
| `partition-archival-worker` | `services/cmd/archiver` | `exchange-partition-archival.{service,timer}` (bare-metal twin of the K8s CronJob) | oneshot + timer, checksum verifier |

## Tier 4–5: Kubernetes tier (probes, not systemd)

| Daemon | Repo artifact | Manifest | Supervisor |
|---|---|---|---|
| `order-gateway` | `services/cmd/gateway` :8080 | `deploy/k8s/services/order-gateway.yaml` (blue+green) | R9 `/health/live` `/health/ready`, HPA 2→10 |
| `fix-gateway` | `services/cmd/fix` :9800/8082 | `fix-gateway.yaml` | probes + FIX heartbeat monitor + CoD |
| `marketdata-service` | `services/cmd/marketdata` :8081 | `marketdata-service.yaml` | probes + WS buffer saturation |
| `aeron-nats-bridge` | `services/cmd/bridge` :9101 | `aeron-nats-bridge.yaml` + PVC | probes + JetStream ack timeout |
| `risk-coordinator` | `services/cmd/risk` — real binary (cross-shard margin coordinator, Task 19.3.11; supersedes prior "planned" mark) | `risk-coordinator.yaml` | probes + 500µs RPC timer |
| `liquidation-scanner` | runs **inside `cmd/gateway`** — `liqSvc.ScanOnce`/`ConsumeOnce`/`adlPub.TickOnce` sweep loops (Phase-19); `services/cmd/liquidation_scanner` does not exist | `liquidation-scanner.yaml` | probes + 2s scan timer |
| `settlement-service` | `services/cmd/settlement` :8083 | `settlement-service.yaml` | probes + `GrossNetService.DispatchDue` loop (5m; `EXC_SETTLE_DISPATCH_INTERVAL`) — release-queued legs, rail cut-off re-roll, mode-aware GROSS/NET dispatch; fails closed without `EXC_SENDER_BIC` |
| `tomnext-rollover` | runs **inside `cmd/gateway`** — session-lifecycle `Rollover` seam → `settlement.NewRolloverService` (Task 3.3.7); `services/cmd/tomnext_rollover` does not exist | `cronjobs/tomnext-rollover.yaml` *(placeholder image `:CHANGE_ME`)* | CronJob completion + Redis lock |
| `compliance-worker` | `services/cmd/compliance` :8084 | `compliance-worker.yaml` | probes + NATS consumer lag |
| `regulatory-reporter` | runs **inside `cmd/gateway`** — dispatch sweep + per-regime reconcile + `trades`/`settlements` JetStream consumers (Phase-21); `services/cmd/regulatory_reporter` does not exist | `regulatory-reporter.yaml` | probes + submission ack tracker |
| `banking-rails-worker` | runs **inside `cmd/gateway`** — `funding.NewRailService` + dispatch handoff on CONFIRMED rows (Phase-11); `services/cmd/banking_rails` does not exist | `banking-rails-worker.yaml` | probes + rail return-code tracker |
| `analytics-spooler` | `services/cmd/analytics` — real binary (Phase-20 CH ingest: trades+analytics consumers, spool drain, income poll; supersedes prior "planned" mark) | `analytics-spooler.yaml` + PVC | probes + 5s insert timeout |
| `oracle-service` | `services/cmd/oracle` — real binary (Phase-19.5 mark/index publisher, 5s staleness gate; supersedes prior "planned" mark) | `oracle-service.yaml` | probes + 5s staleness gate |
| `proof-of-reserves-builder` | **out-of-process by design** — `services/cmd/exchange solvency-tree` + `deploy/crons/solvency-tree.sh` (22:00 UTC); keeps the signing key out of the gateway env. `services/cmd/proof_of_reserves` does not exist | `cronjobs/proof-of-reserves-builder.yaml` *(placeholder image `:CHANGE_ME`)* | CronJob completion + SHA256 check |
| `status-exporter` | runs **inside `cmd/gateway`** — `/metrics` + `/health/live|ready` surfaces (Task 9.3.28); `services/cmd/status_exporter` does not exist (`sentinel_exporter` is a separate real binary) | `status-exporter.yaml` | probes + Prometheus scrape |
| `admin` | `services/cmd/admin` :8085 | `admin.yaml` | probes; ops subnet only |
| `fixsbe-gateway` | `services/cmd/fixsbe` — :9890 SBE/TLS listener + :8089 metrics (`fixsbe.enabled=false` → metrics-only) | supervisord `fixsbe-gateway` / dev_stack `fixsbe` / compose `fixsbe` (k8s manifest TODO) | restart + mTLS cert gate |
| `xshardrelay` | `services/cmd/xshardrelay` — cross-shard ctl router for basket 2PC `dst_shard` delivery (spec §2.2a; Aeron IPC is point-to-point per channel alias) | supervisord `xshardrelay` / dev_stack `xshardrelay` / compose `xshardrelay` (k8s manifest TODO) | restart + routed/dropped counters |

## Ops / batch entrypoints (not supervised daemons)

| Binary | Purpose |
|---|---|
| `services/cmd/exchange` | operator CLI: audit hash-chain verify, Merkle root, `replay-from-archive` |
| `services/cmd/natsctl` | JetStream provisioning/health/DLQ ops CLI |
| `services/cmd/archiver` | also runs ad-hoc: `scan`/`run`/`restore` |
| `services/cmd/recovery`, `wal-recovery`, `recovery-orchestrator`, `replay` | WAL/snapshot recovery tooling (Phase-04) |
| `services/cmd/s3-market-data-exporter` | daily 01:00 UTC CH→S3 export (Task 4.3.8) — deployable as CronJob |
| `services/cmd/devs3` | dev-only S3 shim |

## Watchdog tiers (spec §19.13.3)

- **T1 hardware/OS:** BMC `/dev/watchdog` via `system.conf.d/50-exchange-watchdog.conf`
  (`RuntimeWatchdogSec=10s`, `ShutdownWatchdogSec=10min`) + per-unit
  `WatchdogSec`.
- **T2 in-process:** `WatchdogThread` in `matching_engine` — 100µs sampling,
  500µs warn (`MATCHING_LOOP_STALLED`), 2ms fail-closed L0 halt with dirty
  WAL flush + `engine:leader:{shard}` release.
- **T3 platform:** `exchange-watchdogd` — Aeron watermarks, lease/sequence
  stall, 1s synthetic canary orders (250ms ack → `ReadOnly` + P0 page), PTP
  skew, NVMe quota.
