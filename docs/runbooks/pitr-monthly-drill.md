# Runbook: Monthly PostgreSQL PITR drill (Task 14.3.5)

**Owner:** DBA on-call + SRE · **Cadence:** monthly (first Tuesday, 10:00 UTC — after the nightly base backup lands) · **Authority:** spec §4 DR targets — RPO ≤ 15s, RTO ≤ 5min · **Machinery:** `deploy/postgres/{postgresql.conf,wal_archive.sh,wal_restore.sh,backup.sh,restore_pitr.sh,pitr_smoke.sh}`

This is a scheduled **drill**, not an incident response. Its purpose is to prove — every month, against real artifacts — that the last nightly base backup plus the WAL archive can reconstruct the primary to a chosen point in time inside the RTO bound. A skipped or silently-failed drill is a compliance defect: §24 rows 18–21 (WAL archived, nightly base backup, RPO/RTO verified) must stay green.

## Entry conditions / schedule

- Crontab reminder: `0 10 1 * *` (first-of-month 10:00 UTC) → on-call ticket.
- Prerequisites verified on the primary before starting:
  1. `archive_mode = on`, `wal_level = replica`, `archive_timeout = 15` — `SHOW` each on the primary (`deploy/postgres/postgresql.conf`).
  2. WAL archive is draining: newest segment in `PITR_ARCHIVE_DIR` (or `PITR_S3_URI`) < 60s old.
  3. Last night's `backup.sh` base backup exists (`PITR_BACKUP_DIR/YYYYMMDD` and/or `${PITR_S3_URI}/YYYYMMDD.tar.gz`).

## Detection (what the drill measures)

- **RPO leg:** the archive lag observed at drill start must be ≤ 15s (archive_timeout bound, verified by file timestamps).
- **RTO leg:** wall-clock from `restore_pitr.sh` start to the recovered cluster accepting read-only connections must be ≤ 5min for the production data volume. (On a scratch/test volume this proves mechanism correctness; the production-volume number is what goes in the drill report.)
- Any failure → the drill is RED and becomes an incident per Escalation.

## Procedure

1. Pick the recovery target: a timestamp inside the last 24h with known activity (e.g. `now() - interval '1 hour'`).
2. Restore the most recent base backup onto the drill host —
   `deploy/postgres/restore_pitr.sh` performs: copy base backup → configure `restore_command` via `wal_restore.sh` (same backend as production: `PITR_ARCHIVE_BACKEND=s3|local`) → set `recovery_target_time` → start with `recovery.signal`.
3. Watch recovery: `pg_controldata` / log tail for `consistent recovery state reached`, then replay progress to the target.
4. Verify data: connect read-only, check a known row/timestamp table both sides of the target — rows before the target present, rows after absent.
5. Time and record: archive lag (RPO) + restore wall-clock (RTO) go into the drill report + `retention_audit_log` if the archiver supports a check row (`check_name='pitr_drill'`).
6. Tear down: drop the recovered cluster, keep the report.
7. Fast mechanism check (no cluster needed): `deploy/postgres/pitr_smoke.sh` runs the same archive→backup→replay→target loop on real PG binaries on a scratch cluster — acceptable as the drill when a drill host is unavailable; a `SKIP` (exit 77) means the environment lacks PG binaries and does NOT count as a pass — file the ticket anyway.

## Rollback / recovery

The drill is read-only against production: it copies a base backup and replays the WAL archive on a separate host. Nothing writes to the primary, so there is no production rollback path — but never point `restore_pitr.sh` at the live data directory; it overwrites `$PGDATA`.

## Escalation

- Drill RED (any verification fails, archive lag >15s, restore >5min, or a missing nightly backup) → page DBA on-call P1 + file incident ticket; treat as a latent DR breach until resolved.
- Drill SKIPPED (no binaries/host) → P2 ticket; two consecutive skips escalate to P1 — coverage is unproven.
- Root causes most often seen: `wal_archive.sh` backend misconfigured (check archive stalled — [wal-archive-stalled.md](./wal-archive-stalled.md)), expired S3 credentials on the archive role, or `PITR_BACKUP_DIR` disk full so `backup.sh` failed the night before.
