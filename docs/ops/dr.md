# Multi-Region Disaster Recovery — Phase-09 Task 9.3.4

Spec §18.3/§18.4/§18.6.4, §24 #44/#86/#87/#211. This document is the DR
architecture + runbook index; the executable steps live in
`deploy/dr/failover-runbook.sh` and the existing WAL tooling in
`deploy/postgres/`.

**Status:** configuration + procedures landed; live second-region
deployment is **pending-infra** (no secondary region provisioned in this
environment). Every `pending-infra` item below is explicitly unverified
until the monthly DR drill runs it.

## RPO / RTO targets (spec §18.3 — verbatim)

| Component | RPO (max data loss) | RTO (max downtime) |
|---|---|---|
| Order book (in-memory + WAL) | 0 (dual-write WAL) | 10s (warm recovery) |
| PostgreSQL (orders, trades) | 15s | 5min |
| WAL streams | 0 (dual-write + S3 archive) | 30s |
| Market data | 10s | 2min |
| ClickHouse (analytics) | 60s | 30min |
| User data | 5min | 10min |

Site-loss residual risk (spec §18.3, remediation #27): up to 15s of
cross-region fill loss is disclosed, remediated via PB-restitution
(Task 24.3.14) / failed-settlement (24.3.6) chains and DORA incident
reporting (Task 9.3.15) — not silently closed.

## Topology

| Tier | Region | Role |
|---|---|---|
| Primary | dc1 | active — C++ matching core + Go services + PG primary (semi-sync) + Redis Sentinel primary |
| Secondary | dc2 | warm standby — C++ standby replaying WAL, PG semi-sync replica (`deploy/dr/postgres-standby.conf.sample`), Redis replica (`deploy/dr/redis-secondary.conf.sample`), K8s services scaled warm |
| Tertiary | dc3 | cold — PG base backups + WAL archive + Redis RDB only (no serving capacity; restore-on-declaration) |

## Data-path wiring

- **PostgreSQL:** `primary_conninfo` + physical slot `dr_secondary`;
  `synchronous_standby_names = 'FIRST 1 (pg-dr-secondary)'` with
  `synchronous_commit = remote_flush` → flush-ack is the ≤15s RPO bound.
  Patroni/Pacemaker promotes on failover (`pg_ctl promote`).
  *pending-infra: standby build + semi-sync verification.*
- **Redis:** secondary replica via `replicaof`; promote with
  `REPLICAOF NO ONE`; Sentinel re-registers post-promotion. The replica
  is NOT in the in-region Sentinel quorum (WAN partition safety).
  *pending-infra: replica provisioning.*
- **WAL archive:** `deploy/postgres/wal_archive.sh` ships segments to
  the S3 WORM bucket; **CRR rule** `deploy/dr/s3-crr-wal-archive.json`
  replicates `wal/` to `exchange-wal-archive-dr` with 15-min
  ReplicationTime SLA + object-lock preserved. Verify in-region replay
  via `deploy/postgres/wal_restore.sh` + `restore_pitr.sh` (see drill).
- **Matching core:** secondary engine replays WAL to tail (warm
  recovery RTO 10s) via `services/cmd/replay` (`exchange:replay-from-archive`
  on the DR bucket for segments not yet streamed).

## Failover procedure (primary → secondary)

Executable stages in `deploy/dr/failover-runbook.sh` (each step callable
independently for drills):

1. `declare` — disaster declaration, BCP quorum (§18.6.4 step 1), RTO
   clock starts.
2. `pg` — verify replication gap ≤15s, `pg_ctlcluster promote` the
   standby, repoint `synchronous_standby_names`.
3. `redis` — `REPLICAOF NO ONE` on the DR replica, re-register Sentinel.
4. `wal` — replay un-replicated WAL tail from the DR bucket.
5. `edge` — provider health checks / Route53-Cloudflare anycast reroute
   `api.*`/`ws.*` (15–30s per §18.6.4 step 4). FIX is private
   connectivity — not on this edge; FIX sessions re-establish to the
   secondary endpoints separately.
6. `verify` — §18.6.5 six-stage integrity audit before books open;
   §18.6.6 resumption ladder (CANCEL_ONLY 60s → FIX resync → WS resume →
   reopening auction → continuous trading).

## Post-failover reconciliation

- PG: `pg_last_xact_replay_timestamp` gap, table-level count spot-checks
  on `orders`/`trades`, ledger zero-sum (§18.6.5 stage 1).
- Book: `book_seq == wal_tail_seq` (stage 3); `recovery_digests`
  (migration 092) digest match.
- Redis: token-bucket/session warm-up observation (limits rebuild
  in-memory — expected, not a defect).
- Nostro/CLS stages 5–6 before user-facing settle opens.

## WAL archive replay validation (drill step)

```bash
# In the SECONDARY region, fetch + replay one archived segment:
deploy/postgres/wal_restore.sh <segment> /tmp/wal-seg
deploy/postgres/restore_pitr.sh --validate-only   # archive chain intact
# Engine side: replay the segment set into the standby book:
services/cmd/replay --from-archive s3://exchange-wal-archive-dr --dry-run
```
Exit criteria: segment decompresses, CRC32C trailer verifies, replay
lands at the expected sequence. *pending-infra until dc2 exists.*

## DR drill cadence

- **Monthly** failover test (Task 9.3.4 DoD): run `failover-runbook.sh`
  stepwise against the secondary region; record per-stage wall time vs
  the RTO budget.
- **Quarterly** full region failover (§24 #211 / Task 9.3.21): all RPO/
  RTO targets measured live, results filed in the ops log.
- Every drill archives: decision timestamp, per-stage timings, RPO/RTO
  deltas, anomalies + follow-up tickets.

## Pending-infra ledger

| Item | Blocker |
|---|---|
| dc2/dc3 region provisioning | infra not yet allocated |
| PG semi-sync standby live replication | needs dc2 PG host |
| Redis cross-region replica | needs dc2 Redis host |
| S3 CRR rule applied to the live bucket | needs production bucket ARNs |
| End-to-end failover timing (RTO proof) | first monthly drill |
| Edge anycast reroute config | provider account (see deploy/cloudflare/) |
