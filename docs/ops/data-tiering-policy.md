# Hot–Warm–Cold Data Tiering Policy

**Phase-09 Task 9.3.24 · spec §19.7, §19.12 · §24 #179/#212**
Config of record: `infrastructure/data-tiering/tiering_policy.yaml`
Engine: `internal/archiver` (`RunLifecycle`) · Enforcement:
`internal/operations/retention` (nightly enforcer)

---

## 1. Tier definitions

| Tier | Store | Contents | Latency | Notes |
|---|---|---|---|---|
| **HOT** | PostgreSQL 16 primary (NVMe) | open orders, positions, and partitioned data whose range ended ≤ `hot_days` ago (default **90 d**) | ms | fully indexed, OLTP path |
| **WARM** | PostgreSQL `warm` schema (detached partitions) **and** ClickHouse for analytics classes | partitioned data `hot_days`–`warm_days` old (default 90 d–1 y per spec §19.7) | seconds | re-attachable on demand (`ATTACH PARTITION` with the recorded `bound_expr`); ClickHouse holds the analytics copies (ticks/OHLCV) under MergeTree TTL |
| **COLD** | S3 object store, parquet+zstd + JSON manifest, Object Lock COMPLIANCE | data older than `warm_days`; WORM-retained until `retain_days` (5 y order/trade, 7 y audit/finance) | minutes (fetch) | queryable via the Trino/Presto federated layer over the archive bucket; `archiver restore --partition` re-materializes for regulators |

The canonical per-class numbers live in `tiering_policy.yaml`
(`hot_days` / `warm_days` / `retain_days` per class) — this document
describes the mechanism; **the YAML is authoritative on values.**

## 2. Automated migration

`scripts/data_migration.sh` (monthly cron) drives:

1. `archiver lifecycle --apply --policy=tiering_policy.yaml`
   - **hot→warm:** `ALTER TABLE parent DETACH PARTITION` → `SET SCHEMA
     warm` — metadata-only move; row count + relation size captured
     before/after as move evidence.
   - **warm→cold:** COPY → zstd stream → SHA-256 (archive + CSV) → S3 PUT
     with COMPLIANCE Object Lock + retain_until → ETag/HEAD verification
     → `partition_archive_log` → `DROP TABLE`.
2. `retention apply --policy=…` — enforcer closes residual drift
   (purgeable rows, stranded warm partitions) and audits every check.

Every transition writes `partition_tier_state` (current tier, range_end,
archive_id, row_count, checksum) and `partition_tier_log` (append-only
MOVE/HELD/SKIPPED/ERROR history keyed by run_id).

## 3. Verification on move

- hot→warm: row-count parity + `pg_relation_size` evidence (metadata-only
  DDL — the data file cannot change).
- warm→cold: dual SHA-256 digests, S3 ETag=md5 cross-check, HEAD re-read
  of stored metadata + COMPLIANCE lock state, manifest JSON round-trip.
- monthly: `archiver verify --sample=N` downloads random archives and
  re-verifies all digests + CSV row-count parity
  (`deploy/crons/verify-worm-integrity.sh`).

## 4. Compliance holds

`data_retention_holds` rows (partition- or parent-scoped, optional
expiry) block **every** destructive transition — detach, drop, purge.
Managed via `archiver hold add|release|list`; held partitions surface as
`HELD` in `partition_tier_log` and as HELD findings in the enforcer
report. Holds never expire silently: `expires_at` is advisory; release is
an explicit Compliance Officer action.

## 5. Cold-data query path

Cold archives are `parquet+zstd` + manifest under
`s3://{bucket}/{parent}/{partition}/`. Options, in order of preference:

1. `exchange restore-partition-archive` / `archiver restore` — full
   re-materialization with hash + row-count parity (regulator-grade).
2. Trino/Presto federated query over the bucket (CSV/ZSTD or the Parquet
   projection when it lands) — for compliance and support queries.

## 6. Monitoring

- `HotTierGrowthAnomaly` — PG data dir >10%/month ⇒ archival stalled or
  growth above model (`deploy/monitoring/capacity-alerts.yml`).
- `RetentionPolicyViolation` — any enforcer VIOLATION row in 24h.
- Migration job failure = non-zero exit on the cron wrappers (page P2).

## 7. Edge cases handled

- **Migration during peak trading:** detach/set-schema are metadata-only;
  the export COPY runs against the detached table — zero OLTP lock.
- **Hold mid-flight:** hold check runs before each stage; a hold added
  between hot→warm and warm→cold still blocks the drop.
- **Warm-schema name collision:** `SET SCHEMA` failure re-attaches the
  partition with its original bound — never left detached-but-untracked.
- **Stray warm tables:** tables found in the warm schema without
  `partition_tier_state` rows are adopted with unknown `range_end` and
  are never auto-exported (SKIPPED until an operator fixes the record).

## Mitigation — operator response

When a tiering stage fails (non-zero cron exit pages P2): the failed
stage is idempotent — re-run the wrapper after fixing the cause; check
`partition_tier_state` for the stage row that did not advance. A
stray-table adoption (§7) is resolved by inserting the missing
`partition_tier_state` row, never by dropping the table.

## Escalation

Tiering failures page P2 (data growth, not correctness). Escalate to P1
only when a failed export would push a PG volume past the §2.7.3 WAL
watermark or when a compliance-hold block exposes a missing legal hold —
route to Finance Ops + Compliance via `docs/runbooks/incident-escalation.md`.

## Trigger

This runbook is the response target for `HotTierGrowthAnomaly` and
`RetentionPolicyViolation` (`deploy/monitoring/capacity-alerts.yml`),
for a non-zero exit on any `deploy/crons/*tiering*`/`archive` wrapper
(the P2 page), and for operator-initiated tier moves. It is also the
reference when §6 monitoring shows a partition stuck in a tier
transition.
