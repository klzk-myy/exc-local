# DORA ICT Asset/Dependency Inventory & Risk Register

**Phase-09 Task 9.3.15 (item 1)** · **Authority:** spec §19.5, §24 #171 · **Owner:** Ops/Resilience (register), Compliance (review) · **Review:** quarterly + on material change (new service, new vendor, post-incident)

Register format: each row = asset → critical function → dependencies → data classification → owner → recovery objectives → risk treatment. Live register rows are maintained here until `services/internal/operations/dora/` (**pending Phase-09 Task 9.3.15**) provides the database-backed register.

## 1. Asset & dependency inventory (critical functions)

| Asset | Binary / config | Critical function | Dependencies | Data class | Owner | RTO/RPO (§18.3) | Key failure mode + treatment |
|---|---|---|---|---|---|---|---|
| Matching engine shards | `core/build/matching_engine` (`/opt/exchange/bin/matching-engine` prod path) | Order matching, pre-trade risk, WAL | Aeron media driver, NVMe, Redis leader lease `engine:leader:{shardId}`, PTP clock | order/trade data (confidential) | Core Eng | 10s warm / RPO 0 | loop stall → watchdog SIGABRT + standby promotion ([engine-halt-failover](../runbooks/engine-halt-failover.md)) |
| Aeron IPC layer | `aeronmd`, `/dev/shm` rings, `config/aeron-low-latency.properties` | Engine↔services IPC | host kernel, NIC | execution events | Core Eng | seconds | driver loss → [aeron-driver-down](../runbooks/aeron-driver-down.md) |
| WAL storage + S3 archive | `wal/`, `exchange archive-*`, `replay-from-archive` | Durable order journal, DR replay | NVMe, S3 (WORM) | ledger-grade | Core Eng | RPO 0 / RTO 30s | corruption → graduated ladder + [wal-recovery-halt](../runbooks/wal-recovery-halt.md) |
| Recovery orchestration | `services/cmd/recovery`, `services/cmd/recovery-orchestrator`, `services/cmd/wal-recovery` | Snapshot persist, promotion, offline ladder | PG `book_snapshots`, Redis leases, `recovery_reports` (065), `recovery_digests` (092) | operational | Core Eng | §18.6 budgets | halt → P1 runbook |
| Order gateway (REST/WS) | `services/cmd/gateway` :8080 | Client order ingress, auth, rate limits | PG, Redis (sessions/mode), Aeron ingress | client PII + orders | Platform | stateless; K8s HPA | rejection storms → L2/L3 runbooks |
| FIX gateway | `services/cmd/fix` :8082 | Institutional order ingress | Aeron (`orders_in`/`orders_out` ipc streams 1001/1002), `fix_sessions`+`fix_messages` (spec §5.20; migrations 030/046 landed) | institutional orders | Platform | heartbeat×2 → CoD 50ms | acceptor+initiator live (Phase-18 Tasks 18.3.1/2/9/16); SBE transport leg still sibling scope |
| Market data distribution | `services/cmd/marketdata` :8081 | WS L2/L3 conflation, feeds | shm rings, Redis seq mirror | public market data | Platform | 10s/2min | seq gaps → WS resume/replay (§10.9) |
| Aeron→NATS bridge | `services/cmd/bridge` 9100+shard | Cold-path event fan-out | Aeron, JetStream, `/var/spool/exchange/` | execution events | SRE | seconds | buffer/spool growth → bridge runbooks |
| NATS JetStream | `nats-server` ×3 (`deploy/nats/*.conf`) | Event backbone, 7 streams + `ops-dlq` + `ops.alerts.*` | cluster quorum | events/ops | SRE | node loss tolerated | consumer stall → [nats-consumer-pending-high](../runbooks/nats-consumer-pending-high.md) |
| PostgreSQL 16 + pgbouncer | `deploy/postgres/` | OLTP, ledger, audit chain | semi-sync replica, WAL archive `wal_archive.sh`, pgbouncer | all persistent data incl. PII | DBA | 15s/5min | primary loss → [postgres-failover](../runbooks/postgres-failover.md) |
| Redis 7 + Sentinel ×3 | `deploy/redis/`, `config/redis-sentinel.yaml` | Sessions, leases, mode, flags, rate limits | sentinel quorum, replica | sessions/coordination | SRE | 5s/30s | failover → [redis-sentinel-failover](../runbooks/redis-sentinel-failover.md) |
| ClickHouse | `deploy/clickhouse/` | Analytics, tick history | S3 backup `exchange-ch-backup` | market/analytics | Data | 60s/30min | restore drill → `deploy/clickhouse/RUNBOOK.md` |
| Settlement service | `services/cmd/settlement` :8083 | GL posting, T+1/T+2 | PG `journal_entries`, JetStream | financial | Finance Ops | — | ingest backlog ~5.7k fills/s bound |
| Compliance worker | `services/cmd/compliance` :8084 | Sanctions/surveillance (scaffold) | `surveillance`/`compliance` streams, sanctions vendor | regulated PII | Compliance | scoped degradation | provider outage → `SANCTIONS_SERVICE_UNAVAILABLE` scoped mode (Phase-21) |
| Admin/monitoring service | `services/cmd/admin` :8085 | Ops metrics, alert evaluator, heartbeat watcher | Aeron CnC, JetStream, Prometheus | ops | SRE | — | `AlertDispatchErrors` |
| Edge/CDN + WAF | Cloudflare/Fastly-class (Task 9.3.13 pending deploy) | Ingress edge, DDoS | DNS/Anycast | public | Security | 15–30s reroute | edge loss → region failover |
| Clock sync | `ptp4l`, `phc2sys`, `services/internal/timesync` | RTS 25 timestamping | grandmaster, PHC NIC | — | Platform | halt at >100µs | [time-sync-loss-halt](../runbooks/time-sync-loss-halt.md) |
| Secrets store | Vault/KMS (Phase-13.5 Task 13.5.3.5) | All credential material | KMS grants, HSM | secrets | Security | DR-decrypt gate | [secret-rotation-overdue](../runbooks/secret-rotation-overdue.md) |

Pending-inventory services (later phases, register on delivery): `oracle` (19.5), `risk-coordinator`/`liquidation-scanner` (19), `banking-rails` (11/24), `analytics` spooler (16/20), `regulatory-reporter` (21), `tomnext-rollover` (3), `proof-of-reserves` (13), `status-exporter` (9.3.25), `watchdogd` (9.3.28).

## 2. Risk treatment register

| Risk ID | Scenario | Likelihood | Impact | Treatment | Residual |
|---|---|---|---|---|---|
| ICT-R1 | Engine halt ≥95% ring latch (perf report §4.3 — permanent halt) | M | H | restart via failover runbook; spec-level hysteresis fix tracked §27 backlog | M until fix |
| ICT-R2 | WAL corruption beyond ladder L2 | L | VH | Level-3 fail-closed + dual-control accept/replay decision | L |
| ICT-R3 | PG primary loss mid-session | M | H | semi-sync + promote runbook (15s/5min) | L |
| ICT-R4 | NATS outage → bridge spool >5GB → ReadOnly | M | M | 3-node cluster, spool bound, `ReadOnly` preserves zero-loss | L |
| ICT-R5 | Redis quorum loss | L | H | 3 sentinels, `quorum=2`, in-memory rate-limit fallback | L |
| ICT-R6 | PTP grandmaster loss | L | H | halt domain; dual-grandmaster config (platform) | M |
| ICT-R7 | Region loss (DC) | L | VH | §18.4 3-region + quarterly D1 drill; accepted residual 15s fill-loss window (§18.3 site-loss note) | accepted |
| ICT-R8 | Secrets leak/rotation failure | M | H | Vault/KMS, 089 inventory, emergency rotation runbook | M |
| ICT-R9 | Third-party ICT outage (see [third-party register](./dora-third-party-register.md)) | M | M–H | exit plans + substitution tests | M |
| ICT-R10 | Reconciliation divergence (ledger vs cache vs nostro) | L | VH | hourly reconciler (Phase-13) + fail-closed review | M |
| ICT-R11 | Monitoring blind spot (Prometheus AND `ops.alerts` path down) | L | H | two independent alert paths; `AlertDispatchErrors` self-check | L |
| ICT-R12 | Capacity breach (5× volatility burst) | M | H | 250k/s burst proof gate (Task 9.3.29) + headroom alerts (9.3.19) | M |

## 3. Governance

- Register changes: PR on this file + DORA evidence snapshot (export procedure pending `services/internal/operations/dora/`).
- Mapping rule: every **critical/important function** must resolve to ≥1 asset row with RTO/RPO and an owner — CI check pending Task 9.3.15 backend; interim is manual review at quarterly cadence with [dr-drill.md](../runbooks/dr-drill.md) evidence.
- Vulnerabilities discovered (VDP/pentest — §19.11.2, `vulnerability_disclosures` migration 081 pending) feed rows as risk-treatment updates.
