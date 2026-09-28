#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# backup.sh — nightly PostgreSQL base backup for PITR (Task 4.3.4)
#
# Takes a plain-format base backup into ${PITR_BACKUP_DIR:-/backups}/YYYYMMDD
# using pg_basebackup -Fp -Xs -P. With WAL archiving enabled (postgresql.conf)
# any base backup + the archive gives point-in-time recovery to RPO <= 15s.
#
# Cron (02:30 UTC, staggered away from the 01:00 market-data export):
#   30 2 * * *  /opt/exchange/deploy/postgres/backup.sh >> /var/log/exchange/basebackup.log 2>&1
#
# Environment:
#   PGBIN             directory containing pg_basebackup (default: PATH lookup)
#   PGHOST/PGPORT/PGUSER/PGPASSWORD or PGDSN   standard libpq connection vars
#   PITR_BACKUP_DIR   local staging dir for base backups (default /backups)
#   PITR_S3_URI       if set (e.g. s3://exchange-pitr/base), the finished backup
#                     is tar-gzipped and uploaded to ${PITR_S3_URI}/YYYYMMDD.tar.gz
#                     so a region loss loses neither archive nor base backup
#   AWS_ENDPOINT_URL  honoured for S3-compatible stores
#   PITR_KEEP_DAYS    local retention (default 7); older YYYYMMDD dirs removed
#
# Exit 0 iff the base backup is complete and (when configured) uploaded.
# -----------------------------------------------------------------------------
set -euo pipefail

pgbin=${PGBIN:-}
pg_basebackup=${pgbin:+$pgbin/}pg_basebackup
command -v "$pg_basebackup" >/dev/null || {
    echo "backup.sh: pg_basebackup not found (set PGBIN)" >&2; exit 1; }

backup_dir=${PITR_BACKUP_DIR:-/backups}
keep_days=${PITR_KEEP_DAYS:-7}
stamp=$(date -u +%Y%m%d)
dest=$backup_dir/$stamp

mkdir -p "$backup_dir"
if [[ -d $dest ]]; then
    # Never overwrite a completed backup: a rerun the same day gets a suffix.
    dest=$backup_dir/${stamp}-$(date -u +%H%M%S)
fi

echo "backup.sh: starting base backup -> $dest"

conn_args=()
if [[ -n ${PGDSN:-} ]]; then
    conn_args=(-d "$PGDSN")
fi
if [[ -n ${PGHOST:-} ]]; then
    conn_args+=(-h "$PGHOST")
fi
if [[ -n ${PGPORT:-} ]]; then
    conn_args+=(-p "$PGPORT")
fi
if [[ -n ${PGUSER:-} ]]; then
    conn_args+=(-U "$PGUSER")
fi

# -Fp plain format (restore = directory copy), -Xs stream the WAL needed for
# consistency into the backup, -P progress, -c fast checkpoint now.
"$pg_basebackup" -D "$dest" -Fp -Xs -P -c fast "${conn_args[@]}"

# Manifest + checksum so the restore drill can verify bit integrity.
( cd "$dest" && find . -type f -exec sha256sum {} + | LC_ALL=C sort \
    > "$dest.sha256sums" )
echo "backup.sh: checksums -> $dest.sha256sums"

# Optional off-site copy of the base backup (WAL archive already lives in S3).
if [[ -n ${PITR_S3_URI:-} ]]; then
    endpoint_args=()
    if [[ -n ${AWS_ENDPOINT_URL:-} ]]; then
        endpoint_args=(--endpoint-url "$AWS_ENDPOINT_URL")
    fi
    tgz=$backup_dir/${stamp}.tar.gz
    tar -C "$backup_dir" -czf "$tgz" "$(basename "$dest")"
    aws s3 cp --only-show-errors "${endpoint_args[@]}" \
        "$tgz" "${PITR_S3_URI%/}/${stamp}.tar.gz"
    aws s3 cp --only-show-errors "${endpoint_args[@]}" \
        "$dest.sha256sums" "${PITR_S3_URI%/}/${stamp}.sha256sums"
    rm -f "$tgz"
    echo "backup.sh: uploaded ${PITR_S3_URI%/}/${stamp}.tar.gz"
fi

# Local retention: prune base backups older than PITR_KEEP_DAYS.
if [[ $keep_days -gt 0 ]]; then
    find "$backup_dir" -maxdepth 1 -name '2*' -mtime "+$keep_days" \
        -exec rm -rf {} +
fi

echo "backup.sh: base backup complete -> $dest"
