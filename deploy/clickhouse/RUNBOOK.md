# ClickHouse Backup & DR Runbook — Task 4.3.6 (spec §18.3, §24 #159)

## Model

| Layer | Mechanism | Covers | Bound |
|---|---|---|---|
| Primary | ReplicatedMergeTree, sharded + replicated cluster (spec §18.2) | node/AZ loss | **RPO ≤ 60s** |
| Tertiary | `clickhouse-backup` → `s3://exchange-ch-backup/` | region loss, logical corruption, accidental drop | RTO ≤ 30 min |

Replication is what meets RPO ≤ 60s — the S3 tier is *not* the RPO mechanism.
The S3 tier exists so that losing an entire region, or a replicated
`DROP TABLE`/corruption event, does not lose tick/analytics history.

## Schedule

```cron
# daily full — 01:05 UTC (after the 01:00 market-data export window)
5 1 * * *   /opt/exchange/deploy/clickhouse/backup.sh full
# hourly incremental — covers intra-day gaps between fulls
20 * * * *  /opt/exchange/deploy/clickhouse/backup.sh incremental
```

Backup names: `daily-YYYYMMDD` (full), `incr-YYYYMMDD-HHMMSS` (incremental).
`backups_to_keep_remote: 35` in `clickhouse-backup/config.yml` bounds storage.

## Restore drill (quarterly — evidence goes into the §18.3 audit log)

1. Provision a scratch ClickHouse node/cluster (never the production cluster).
2. Point a second config at it:
   `CLICKHOUSE_BACKUP_CONFIG=/tmp/chb-scratch.yml` (same `s3:` block,
   scratch `clickhouse:` block).
3. On production, snapshot row counts for a sampled partition:
   `./backup.sh counts /tmp/src.tsv 20260928`
4. On the scratch config: `./backup.sh restore daily-YYYYMMDD`
   (`clickhouse-backup restore_remote daily-YYYYMMDD`).
5. On the scratch cluster: `./backup.sh counts /tmp/dst.tsv 20260928`
6. Verify: `./backup.sh verify /tmp/src.tsv /tmp/dst.tsv` — exit 0 iff every
   table's row count matches; `MISSING`/`MISMATCH` lines are failures.
7. Record drill evidence (backup name, elapsed restore time vs 30-min RTO,
   verify output) in the §18.3 audit log.

## Failure modes

- **S3 outage mid-backup:** `create_remote` fails non-zero; the local backup
  remains (`clickhouse-backup list local`) and uploads on the next run —
  no state corruption. Alert on two consecutive failed runs.
- **Backup during ingest peak:** `max_concurrency: 4` caps upload bandwidth;
  `create` is metadata + hardlinks, safe under load. Incrementals only ship
  changed parts.
- **Partial restore:** re-run `restore_remote`; it is resumable. Verify with
  `backup.sh verify` before trusting the restore.
