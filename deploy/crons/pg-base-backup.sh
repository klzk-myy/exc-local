#!/usr/bin/env bash
# deploy/crons/pg-base-backup.sh — NIGHTLY base backup for PITR
# (Phase-14 Task 14.3.5; backup machinery is Task 4.3.4).
#
# Thin cron wrapper around deploy/postgres/backup.sh: fixed invocation,
# timestamped log lines, non-zero exit on failure so the cron wrapper
# pages on-call — a silently missed base backup is a hidden RTO breach
# (WAL archive alone can still restore, but replay time balloons past
# the <= 5min target as the gap grows).
#
# Crontab (02:30 UTC, staggered away from the 01:00 market-data export
# and the 02:00 partition-archival timer):
#   30 2 * * *  /opt/exchange/deploy/crons/pg-base-backup.sh >> /var/log/exchange/basebackup.log 2>&1
#
# Environment (passed through to backup.sh):
#   PGBIN             directory containing pg_basebackup
#   PGHOST/PGPORT/PGUSER/PGPASSWORD or PGDSN — libpq connection vars
#   PITR_BACKUP_DIR   local staging dir (default /backups)
#   PITR_S3_URI       when set, tar-gzips and uploads the finished backup
#   PITR_KEEP_DAYS    local retention (default 7)
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
backup=${PITR_BACKUP_SCRIPT:-$script_dir/../postgres/backup.sh}

echo "$(date -u +%FT%TZ) base-backup: starting nightly pg_basebackup"
bash "$backup"
echo "$(date -u +%FT%TZ) base-backup: complete"
