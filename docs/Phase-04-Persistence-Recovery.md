# Phase 4 — Persistence & Recovery

**Duration:** 5–8 days (supersedes prior 5–7 — Tasks 4.3.11–4.3.12 added 2026-09-27, remediation #27; reconciles the header with §4.6, which had drifted to 5–7 while the header still read 4–6)
**Dependencies:** Phase 2, Phase 2.5, Phase 3
**Spec Reference:** §3.4 (Binary WAL), §18 (Recovery & Replay)

---

## 4.1 Objectives

Implement full persistence and recovery: WAL snapshotting to PostgreSQL, WAL S3 archive, replay-from-archive, PostgreSQL PITR (Point-in-Time Recovery), and the recovery manager that orchestrates snapshot load + WAL replay on startup.

---

## 4.2 Prerequisites

- Phase 2 complete (WAL integration)
- Phase 2.5 complete (engine soak passed)
- Phase 3 complete (settlement service)

---

## 4.3 Tasks

### Task 4.3.1: WAL Snapshotting to PostgreSQL

**Objective:** Periodically snapshot the in-memory book state to PostgreSQL.

**File Locations:** `core/src/recovery/SnapshotManager.cpp`, `services/internal/recovery/snapshot_service.go`

**Implementation:**
1. Snapshot cadence: every 100k trades or 5 min, whichever first.
2. C++ core serializes book state (orders, positions) to FlatBuffers — **balances are NOT snapshotted** (boundary pinned 2026-09-27, remediation #35; supersedes the prior "orders, positions, balances" wording: §5.3 balances are mutated exclusively by the Go balance service, and embedding them in the C++ snapshot would create a second source of truth — balances are reconstructed from WAL trade events on recovery).
3. Sends snapshot via Aeron to Go recovery service.
4. Go service writes snapshot to PostgreSQL `book_snapshots` table (bytea column).
5. Snapshot includes `snapshot_seq` (the WAL seq at snapshot time).
6. After snapshot confirmed: WAL can trim entries up to `snapshot_seq`.

**Format note (2026-10-03, snapshot ext v3):** the blob's extension trailer gained an aux side-block (`WalSnapshotAuxHeader` + meta/pending/iceberg/OCO/peg row families, `SnapshotStore.hpp`) carrying engine-private side tables — pending conditional orders (stop/stop-limit/trailing queue), GTD/DAY expiry heap seeds, iceberg hidden reserves, OCO links, peg records. Without it a snapshot-covered restart dropped pending conditionals outright and un-armed expiry on restored orders. Append-only: `aux == nullptr` emits the v2 shape, v1/v2 blobs still parse, and parsed aux is adopted into the replay engine before WAL replay so journaled tail mutations update it identically to live admission.

**Migration note:** Create migration `023_create_book_snapshots.up.sql` with columns: `snapshot_id`, `shard_id`, `snapshot_seq`, `snapshot_data` (bytea), `created_at`. This table was missing from the original Phase 1 migration list (001–020).

**Definition of Done (Acceptance Criteria):**
* [x] Snapshot taken every 100k trades or 5 min
* [x] Snapshot includes all orders, positions with snapshot_seq (balances excluded — reconstructed from WAL trade events; remediation #35)
* [x] Snapshot written to PostgreSQL `book_snapshots` table
* [x] WAL trimmed after snapshot confirmed (no entries before snapshot_seq remain)

**SDD Checklist:**
- [x] Spec checkpoint: snapshot cadence 100k trades / 5 min — defined first, validated against spec
- [x] Spec checkpoint: WAL trim after snapshot confirmed — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: snapshot during high load, snapshot failure, WAL trim race

---

### Task 4.3.2: WAL S3 Archive

**Objective:** Archive WAL segments to S3 before local trim.

**File Locations:** `services/internal/recovery/archive_service.go`

**Implementation:**
1. Before WAL trim: upload WAL segment to S3 (`s3://exchange-wal/{shard}/{date}/{segment}`).
2. S3 upload confirmed (HTTP 200 + ETag) before local trim.
3. Archive index: `s3://exchange-wal/{shard}/index.json` lists all archived segments.
4. Retention: 90 days in S3, then transition to Glacier.
5. CLI: `exchange:archive-status --shard=0` shows archived segments + sizes.
6. Read-only web equivalent (amended 2026-09-27, remediation #26 route-path amendment): `GET /api/v1/admin/archive/status?shard=` returns archived segments + sizes + last-ETag + Glacier transition state for auditor UX (Read-Only Auditor+); the CLI remains the operator path. Registered in Task 5.3.7.

**Definition of Done (Acceptance Criteria):**
* [x] WAL segment uploaded to S3 before local trim
* [x] S3 upload confirmed (ETag verified) before trim
* [x] Archive index maintained in S3
* [x] 90-day retention with Glacier transition
* [x] `exchange:archive-status` CLI works

**SDD Checklist:**
- [x] Spec checkpoint: WAL S3 archive before trim (zero-loss guard) — defined first, validated against spec
- [x] Spec checkpoint: 90-day retention + Glacier — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: S3 upload failure, partial upload, index corruption

---

### Task 4.3.3: Replay from Archive

**Objective:** Replay WAL from S3 archive for historical reconstruction.

**File Locations:** `services/cmd/replay/main.go`

**Implementation:**
1. CLI: `exchange:replay-from-archive --symbol=EUR/USD --from=2026-01-01 --to=2026-01-31`
2. Download WAL segments from S3 for specified date range.
3. Replay entries through a read-only matching engine instance.
4. Output: trade history, book snapshots at intervals, P&L per account.
5. No live trading — read-only reconstruction.

**Definition of Done (Acceptance Criteria):**
* [x] CLI downloads WAL segments from S3 for date range
* [x] Replay produces correct trade history matching PostgreSQL records
* [x] Book snapshots at 5-min intervals match actual snapshots
* [x] P&L per account matches settlement records

**SDD Checklist:**
- [x] Spec checkpoint: replay-from-archive for historical reconstruction — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: missing segments, corrupted segment, partial date range

---

### Task 4.3.4: PostgreSQL PITR (Point-in-Time Recovery)

**Objective:** Set up PostgreSQL continuous archiving for PITR.

**File Locations:** `deploy/postgresql/postgresql.conf`, `deploy/postgresql/backup.sh`

**Implementation:**
1. `wal_level = replica`, `archive_mode = on`, `archive_command = 'aws s3 cp %p s3://exchange-pitr/%f'`
2. Nightly base backup: `pg_basebackup -D /backups/$(date +%Y%m%d) -Fp -Xs -P`
3. PITR restore: `restore_command = 'aws s3 cp s3://exchange-pitr/%f %p'`, `recovery_target_time = '2026-09-14 12:00:00'`
4. RPO ≤ 15s (archive every 15s), RTO ≤ 5 min (restore + replay).

**Definition of Done (Acceptance Criteria):**
* [x] PostgreSQL WAL archived to S3 continuously
* [x] Nightly base backup completes
* [x] PITR restore to specified timestamp succeeds
* [x] RPO ≤ 15s verified (max WAL archive delay)
* [x] RTO ≤ 5 min verified (restore + replay time)

**SDD Checklist:**
- [x] Spec checkpoint: PostgreSQL PITR RPO ≤ 15s / RTO ≤ 5min — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: archive gap, base backup failure, restore to future timestamp

---

### Task 4.3.5: Recovery Manager

**Objective:** Orchestrate snapshot load + WAL replay on startup.

**File Locations:** `core/src/recovery/RecoveryManager.cpp`, `migrations/065_recovery_reports.up.sql`

**Implementation:**
1. On startup: load latest snapshot from PostgreSQL (book state at snapshot_seq).
2. Replay WAL entries from `snapshot_seq + 1` to WAL tail.
3. Verify boot-time invariant: `book_seq == WAL tail`.
4. On mismatch — graduated recovery ladder (amended 2026-09-19, supersedes immediate fail-closed; spec §3.5/§18.1):
   a. **WAL repair:** CRC32-verify entries tail-backwards, truncate at last CRC-valid entry, re-verify invariant against truncated tail; on success resume via `Maintenance` mode + synthetic probe orders before reopening traffic.
   b. **Snapshot rebase:** repair fails → reload latest snapshot, replay forward, re-verify; on success resume as in (a).
   c. **Fail-closed halt (last resort):** both fail → persist `recovery_report` to `recovery_reports` (migration 065: book_seq, wal_tail, last_valid_seq, snapshot_seq, first_divergent_seq, outcome), enter `MarketDataOnly`, P1 alert with linked runbook, dual-control operator decision (accept loss window or `exchange:replay-from-archive`). *(Halt-state vocabulary unified 2026-09-27, remediation #35: Task 4.3.9's `WAL_RECOVERY_HALT` is the canonical halt state for this ladder; §18.6.1's `HALT_LEGAL_FREEZE` is reserved for the legal-freeze audit outcome. This inline ladder is superseded by Task 4.3.9's implementation, which is the single owner of the graduated recovery ladder.)*
5. Idempotent replay: skip entries with seq ≤ already-applied.
6. Warm recovery: follower subscribes to leader's output; no snapshot/replay needed.

**Definition of Done (Acceptance Criteria):**
* [x] Snapshot loaded from PostgreSQL on startup
* [x] WAL replayed from snapshot_seq+1 to tail
* [x] Boot-time invariant verified (book_seq == WAL tail)
* [x] Graduated recovery on mismatch: WAL repair → snapshot rebase → fail-closed halt (spec §3.5 ladder)
* [x] Fail-closed halt persists `recovery_reports` row + P1 alert + runbook link (migration 065)
* [x] Idempotent: replay skips already-applied entries
* [x] Warm recovery: follower skips snapshot/replay

**SDD Checklist:**
- [x] Spec checkpoint: snapshot + WAL replay exact recovery — defined first, validated against spec
- [x] Spec checkpoint: boot-time invariant fail-closed as last resort of graduated recovery ladder — defined first, validated against spec (amended 2026-09-19)
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: no snapshot (cold start), corrupted WAL, partial replay, WAL repair truncation vs book_seq divergence, recovery_report persistence failure

---

### Task 4.3.6: ClickHouse Backup & Disaster Recovery

**Objective:** Give ClickHouse (tick history, analytics) production DR coverage per spec §18.3 — daily S3 backup plus a verified restore path; DR targets RPO ≤ 60s / RTO ≤ 30 min.

**File Locations:** `deploy/clickhouse/backup.sh`, `deploy/clickhouse/clickhouse-backup/config.yml`

**Implementation:**
1. `clickhouse-backup` (or `BACKUP TABLE ... TO S3` on CH 23+): daily full backup of all MergeTree tables to `s3://exchange-ch-backup/{date}/`; incremental backups hourly via `clickhouse-backup create_remote --incremental`.
2. RPO ≤ 60s is met by replication (sharded + replicated cluster per spec §18.2) — S3 backup is the tertiary/cold tier; document that replication covers the RPO and S3 covers region loss.
3. Restore drill: `clickhouse-backup restore_remote` into a scratch cluster; verify row counts vs source for a sampled partition.
4. Backup/restore runbook registered in Phase-9 runbook set; restore drill runs quarterly (drill evidence in §18.3 audit log).

**Definition of Done (Acceptance Criteria):**
* [x] Daily full + hourly incremental ClickHouse backup to S3
* [x] Restore drill: scratch-cluster restore succeeds; sampled partition row counts match — **closed 2026-09-30:** `deploy/scripts/ch_restore_drill.sh` executed end-to-end twice — provisions versitygw S3 gateway (`exc-ch-s3gw`) + scratch CH container (`exc-ch-scratch`, same image tag); `backup.sh full` → `daily-20260930` (18.56MiB, zstd) → `restore_remote` into scratch → `verify` **7/7 partition-202609 tables match** (ticks 1,100,120 / income_ledger 9,732 / ohlcv_1m 21 / tca_results 47 / trades 26 / volume_stats 102 / account_pnl 1); repeatable via the same script (`cleanup` for teardown)
* [x] DR targets met: RPO ≤ 60s (via replication), RTO ≤ 30 min (restore + verify) — **verified 2026-09-30:** RTO leg 356ms restore+verify (ch_restore_drill.sh); RPO leg now proven — `deploy/scripts/ch_replication_drill.sh` provisions embedded `clickhouse keeper` + 2-node ReplicatedMergeTree pair: 200/200 committed rows replicated in **111ms** (≪60s RPO), `docker kill -s KILL` on node1 → node2 served all 200 committed rows + accepted writes within 203ms. Single-keeper single-shard dev topology; multi-AZ placement remains the multi-region row's env-bound leg
* [x] Runbook + quarterly drill scheduled (§24 #159)

**SDD Checklist:**
- [x] Spec checkpoint: ClickHouse S3 backup + restore drill (spec §18.3, §24 #159) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: backup during ingest peak, partial restore, S3 outage mid-backup

---

### Task 4.3.7: PostgreSQL Partition Archival Engine, WORM Object Storage Pipeline & Verification Drill

**Objective:** Implement automated partition detachment, compressed Parquet export to S3 Glacier WORM storage, SHA-256 integrity verification, and restore drills per spec §19.7 / §24 #179.

**File Locations:** `services/cmd/archiver/main.go`, `services/internal/archiver/partition_archiver.go`, `scripts/exchange-restore-partition-archive.sh`

**Implementation:**
1. **Partition Lifecycle Engine:** Query `pg_partman` metadata for partitions of `orders`, `trades`, `order_audit`, and `ledger_lines` older than 90 days. Detach partition table cleanly from parent.
2. **Export & Compression:** Dump detached partition to zstd-compressed Parquet with deterministic column ordering; compute SHA-256 manifest.
3. **WORM S3 Storage:** Upload compressed archive to AWS S3 / Cloudflare R2 bucket configured with Object Lock (compliance mode) ensuring ≥5-year immutable retention for MiFID II / CFTC compliance.
4. **Drop Verification:** Verify S3 ETag and SHA-256 manifest against local export; log archival metadata into `partition_archive_log`; drop detached partition from OLTP database only after confirmation.
5. **Restore Drill CLI:** `exchange:restore-partition-archive --partition=trades_p2026_01_15` downloads, verifies hash, recreates table, and confirms row-count parity.

**Definition of Done (Acceptance Criteria):**
* [x] Partitions older than 90 days detached and exported to compressed Parquet
* [x] S3 Object Lock retention verified; partition drop blocked until upload confirmed
* [x] Restore verification drill restores partition to test instance with zero missing rows

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: PostgreSQL partition archival pipeline (§19.7, §24 #179) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: S3 network drop mid-upload, corrupted Parquet checksum, active queries on detached partition

---

### Task 4.3.8: ClickHouse-to-S3 Daily Batch Compactor & Bulk ZIP Exporter

**Objective:** Implement automated daily batch compaction and export of historical market data from ClickHouse to public S3 archives per spec §16.1 and §24 #419.

**File Locations:** `services/internal/persistence/s3_exporter.go`, `cmd/s3-market-data-exporter/main.go`

**Implementation:**
1. Scheduled cron job executes daily at 01:00 UTC for previous day's data.
2. Extracts raw trade ticks, aggregated trades (`aggTrades`), 1-minute OHLCV klines, and top-of-book depth snapshots from ClickHouse.
3. Compresses data into structured ZIP archives: `{symbol}-trades-YYYY-MM-DD.zip`, `{symbol}-aggTrades-YYYY-MM-DD.zip`, `{symbol}-1m-YYYY-MM.zip`.
4. Uploads archives to public/authenticated S3 storage bucket (`data.{domain}`) with SHA256 checksum manifest.
5. Emits completion event to notify API catalog service (Phase-23 Task 23.3.7).

**Definition of Done (Acceptance Criteria):**
* [x] Daily S3 export runs automatically at 01:00 UTC
* [x] Tick, aggTrade, and kline archives generated and compressed
* [x] SHA256 checksums verified upon S3 upload

**SDD Checklist:**
- [x] Spec checkpoint: ClickHouse-to-S3 daily batch compactor and bulk archive exporter (§16.1, §24 #419)
- [x] All spec checkpoints pass after implementation

---

### Task 4.3.9: Corrupt WAL Repair Ladder & Snapshot Divergence Recovery

**Objective:** Implement automated graduated WAL recovery ladder, snapshot corruption detection, and fail-closed persistence halt per spec §3.5, §18.1, §18.5, and §24 #302.

**Implementation:**
1. **Graduated WAL Recovery Ladder:** Implement Level 1 (CRC repair for partial trailing block) and Level 2 (rebase from latest PostgreSQL book snapshot) recovery workflows in `cmd/wal-recovery/`.
2. **Fail-Closed Halt & Report:** When corruption cannot be deterministically resolved (Level 3), halt recovery immediately (`WAL_RECOVERY_HALT`), write a diagnostic row to `recovery_reports` (migration 065), and page on-call via P1 incident.
3. **Snapshot Divergence Verification:** Verify SHA-256 integrity of loaded snapshots before applying WAL replay; abort startup if snapshot checksum fails or sequence jumps.
4. **Daily Ledger Reconciliation (added 2026-09-27, ledger/wallet/balance specification):** Rebuild all `balances` from immutable `ledger_entries` (§5.3), compare to live `wallets` table, and alert on any mismatch. This is the daily reconciliation job that enforces `wallets.total == journal_sums.net_balance` for all accounts.

**Definition of Done (Acceptance Criteria):**
* [x] Trailing corrupt bytes repaired without dropping confirmed transactions
* [x] Unrepairable corruption triggers fail-closed halt with recovery_reports entry
* [x] Startup invariant rejects mismatched or corrupt snapshot hashes
* [x] Daily reconciliation job rebuilds balances from ledger and verifies zero mismatch

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: Graduated WAL recovery ladder and snapshot divergence fail-closed verification (§24 #302) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 4.3.10: End-to-End Crash Recovery & Cross-Region Disaster Recovery Orchestration Engine

**Objective:** Implement automated end-to-end failover orchestration, split-brain fencing, 6-stage pre-open data integrity audit, and client resynchronization protocols per spec §18.6 and §24 #335.

**Implementation:**
1. **Fencing & Promotion Automation:** Implement `RecoveryOrchestrator` in `cmd/recovery-orchestrator/`. Handle Redis lease epoch verification (`epoch_local == epoch_current`), revoke stale leader keys, promote warm standby within RTO $\le 3\text{s}$, and flush dirty memory-aligned 4KB WAL blocks on termination.
2. **Multi-Region Disaster Recovery Sync:** Automate cross-region PostgreSQL semi-sync failover verification, secondary Redis Sentinel master promotion, and S3 WORM binary WAL archive replay catch-up (`exchange:replay-from-archive`) within RTO $\le 5\text{min}$, RPO $\le 15\text{s}$.
3. **Pre-Open 6-Stage Data Integrity Engine:** Implement pre-open invariant validator asserting:
   - Total Balance Conservation: $\sum\text{Debits} == \sum\text{Credits}$ across all GL and customer lines; settled balances + house equity + insurance fund == nostro cash. Any discrepancy trips `LEDGER_IMBALANCE_ABORT`.
   - Balance Non-Negativity: $\text{balance}_i \ge 0$ for all customer accounts.
   - Deterministic Book-WAL Sequence Equivalence: $\text{book\_seq} == \text{wal\_tail\_seq}$.
   - Monotonic Sequencing & Orphan Check: Every execution in `trades` maps to a valid `orders` record with strictly increasing execution IDs.
   - External Banking Reconciliation: Reconcile MT942 / camt.053 intraday statement movements against internal omnibus accounts.
   - CLS Settlement Finality: Verify CLS PvP gross settlement match confirmations before opening book.
4. **Order Book Resumption Ladder:** Orchestrate reopening sequence:
   - Reopen order book in `CANCEL_ONLY` grace mode for 60 seconds; reject new non-cancel orders with `ORDER_REJECTED_CANCEL_ONLY_MODE`.
   - Broadcast FIX `TradingSessionStatus` (35=h) and resolve client sequence gaps via `ResendRequest` (35=2) and gap-fill `SequenceReset` (35=4).
   - Support WebSocket `{"action":"resume"}` sequence replay from the 60s ring buffer.
   - Conduct 5-second Call Auction uncrossing before transitioning instrument state to `Normal`.

**Definition of Done (Acceptance Criteria):**
* [x] Failover orchestrator fences partitioned nodes, increments lease epoch, and promotes warm standby within 3s
* [x] 6-stage pre-open integrity engine halts fail-closed on any GL ledger imbalance, book-WAL sequence gap, or negative balance
* [x] Order book reopening enforces 60-second CANCEL_ONLY grace period and resolves client sequence gaps before continuous trading resumes

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: End-to-end crash recovery and cross-region DR orchestration engine with 6-stage data integrity audit (§24 #335) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 4.3.11: Time-Boxed Audit with Running Digests & Snapshot Checksums

**Objective:** Bound the 6-stage audit inside the 5-minute RTO and give snapshots the integrity proof WAL frames already have, per spec §18.6.7 and §24 #353. Added 2026-09-27 (disaster-recovery hardening remediation #27).

**File Locations:** `services/internal/recovery/digest.go`, `services/internal/recovery/snapshot_verify.go`, `migrations/092_recovery_digests.up.sql`

**Implementation:**
1. **Running digests (migration 092):** `recovery_digests` — per-shard GL zero-sum hash, book-seq watermark and balance-delta digest, checkpointed every 1,000 trades. The audit verifies digests instead of scanning full tables; any digest mismatch falls back to the full scan for that shard only.
2. **Per-stage time budgets:** budgets per audit stage summing inside RTO ≤ 5min (ledger 60s, book-WAL 30s, monotonicity 60s, nostro 90s, CLS 60s, margin 30s — wall-clock, parallel across shards). Stage overrun fails closed (no stage is ever skipped); overrun raises P1 with the stage timer in the alert.
3. **Snapshot checksums:** snapshot files gain CRC32C trailers verified before load; corrupt snapshot fails over to the previous snapshot + archive replay (extends Task 4.3.9 ladder) rather than into a divergent book.

**Definition of Done (Acceptance Criteria):**
* [x] Audit completes inside RTO on a production-volume fixture; overrun trips P1 fail-closed
* [x] Digest mismatch localizes to one shard with full-scan fallback
* [x] Corrupt snapshot rejected by checksum with automatic rebase

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: time-boxed audit with running digests and snapshot checksums inside RTO (§24 #353) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 4.3.12: Per-Shard Scoped Reopen & External-Feed Fallback

**Objective:** Reopen healthy shards while a failed one stays frozen, and reopen without live bank/CLS feeds when they are the casualty, per spec §18.6.7 and §24 #354. Added 2026-09-27 (disaster-recovery hardening remediation #27).

**Implementation:**
1. **Per-shard verdicts:** the §18.6 state machine executes per shard with a venue-level rollup — a shard failing the audit enters `HALT_LEGAL_FREEZE` alone while healthy shards proceed through CANCEL_ONLY → auction → NORMAL. Cross-shard baskets touching a frozen shard reject with `SERVICE_DEGRADED` until it clears.
2. **External-feed fallback:** if MT942/camt.053 or CLS is unreachable past a 120s deadline, stages 5–6 defer: reopen against the internal ledger with nostro lines suspense-flagged, then auto-reconcile post-open through the Task 24.3.2 aging workflow (T+1 investigate). The deferral is recorded in `recovery_reports` and surfaced on the ops board (Task 15.3.12).
3. **Scope guard:** fallback and scoped reopen never apply to stage 1 (zero-sum) — a ledger imbalance still freezes the affected shard unconditionally.

**Definition of Done (Acceptance Criteria):**
* [x] Failed shard freezes alone; healthy shards reopen with baskets to frozen shards rejected
* [x] Feed outage past deadline reopens with suspense flags + post-open auto-reconcile
* [x] Zero-sum failure always freezes regardless of fallback

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: per-shard scoped reopen with external-feed fallback, zero-sum exempt from fallback (§24 #354) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 4.4 Deliverables

- WAL snapshotting to PostgreSQL
- WAL S3 archive with 90-day retention
- Replay-from-archive CLI
- PostgreSQL PITR (RPO ≤ 15s, RTO ≤ 5min)
- ClickHouse S3 backup + restore drill (RPO ≤ 60s via replication, RTO ≤ 30 min)
- Recovery manager with boot-time invariant
- PostgreSQL partition archival engine, WORM object storage pipeline and restore verification drill (Task 4.3.7)
- ClickHouse-to-S3 daily batch compactor and bulk ZIP exporter (Task 4.3.8)
- Graduated WAL recovery ladder, snapshot corruption guard & recovery_reports generator (Task 4.3.9)
- End-to-end crash recovery and cross-region DR orchestration engine with 6-stage data integrity audit and CANCEL_ONLY reopening ladder (Task 4.3.10)
- Time-boxed audit with running digests & snapshot checksums (Task 4.3.11) and per-shard scoped reopen with feed fallback (Task 4.3.12)

---

## 4.5 Dependencies

- Phase 2, Phase 2.5, Phase 3

---

## 4.6 Duration Estimate

5–8 days (supersedes prior 5–7 — Tasks 4.3.11–4.3.12 audit budgets & scoped reopen added 2026-09-27, remediation #27; prior supersedes 4–6 — Task 4.3.10 added; Tasks 4.3.7–4.3.9 added prior):
- Task 4.3.1 (Snapshotting): 1 day
- Task 4.3.2 (S3 archive): 0.5 day
- Task 4.3.3 (Replay): 1 day
- Task 4.3.4 (PITR): 0.5 day
- Task 4.3.5 (Recovery manager): 1 day
- Task 4.3.6 (ClickHouse backup/DR): 0.5 day
- Task 4.3.7 (PostgreSQL partition archival): 0.5 day
- Task 4.3.8 (ClickHouse-to-S3 bulk exporter): 0.5 day
- Task 4.3.9 (WAL recovery ladder & corruption guards): 0.5 day
- Task 4.3.10 (End-to-end DR orchestration engine & integrity audit): 1 day
- Task 4.3.11 (Time-boxed audit, digests & snapshot checksums): 0.5 day
- Task 4.3.12 (Scoped reopen & feed fallback): 0.5 day
- Testing: 0.5 day

---

## 4.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Snapshot taken every 100k trades or 5 min |
| 2 | Snapshot includes all orders, positions, balances with snapshot_seq |
| 3 | Snapshot written to PostgreSQL book_snapshots table |
| 4 | WAL trimmed after snapshot confirmed |
| 5 | WAL segment uploaded to S3 before local trim |
| 6 | S3 upload confirmed (ETag) before trim |
| 7 | Archive index maintained in S3 |
| 8 | 90-day retention with Glacier transition |
| 9 | exchange:archive-status CLI works |
| 10 | exchange:replay-from-archive downloads segments and replays |
| 11 | Replay trade history matches PostgreSQL records |
| 12 | Book snapshots at 5-min intervals match actual |
| 13 | PostgreSQL WAL archived to S3 continuously |
| 14 | Nightly base backup completes |
| 15 | PITR restore to specified timestamp succeeds |
| 16 | RPO ≤ 15s verified |
| 17 | RTO ≤ 5 min verified |
| 18 | Snapshot loaded on startup; WAL replayed from snapshot_seq+1 |
| 19 | Boot-time invariant: book_seq == WAL tail; graduated recovery ladder on mismatch — WAL repair → snapshot rebase → fail-closed halt with recovery_report (spec §3.5, amended 2026-09-19) |
| 20 | Idempotent replay skips already-applied entries |
| 21 | Warm recovery: follower skips snapshot/replay |
| 22 | ClickHouse daily full + hourly incremental backup to S3 |
| 23 | ClickHouse restore drill: scratch-cluster restore + row-count verification |
| 24 | ClickHouse DR targets met: RPO ≤ 60s, RTO ≤ 30 min (§18.3, §24 #159) |
| 25 | PostgreSQL partition archival: daily partitions older than 90 days detached, compressed, uploaded to WORM S3, verified via SHA-256, and dropped from OLTP (§24 #179) |
| 26 | Daily batch compactor exports historical trades/aggTrades/klines to S3 ZIPs with SHA256 (§24 #419) |
| 27 | Graduated WAL recovery ladder resolves trailing CRC errors, rebases snapshot on Level 2, and halts fail-closed on Level 3 with recovery_reports row (§24 #302) |
| 28 | End-to-end crash recovery & cross-region DR orchestration engine automates split-brain fencing, standby promotion, 6-stage data integrity audit (zero-sum GL, monotonic sequence, book-WAL equality), client resync, and CANCEL_ONLY reopening ladder within RTO targets (§18.6, §24 #335) |
| 29 | Audit stages time-boxed inside RTO with running-digest verification and P1 fail-closed overrun; snapshots carry CRC32C with automatic rebase on corruption (§24 #353) |
| 30 | Per-shard audit verdicts reopen healthy shards while failed shards freeze; feed outage past 120s reopens with suspense flags and post-open reconcile; zero-sum exempt from fallback (§24 #354) |
