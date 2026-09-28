#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# restore_pitr.sh — stage a PITR restore cluster (Task 4.3.4)
#
# Prepares a data directory restored to an arbitrary timestamp:
#   1. copies (or untars) a base backup produced by backup.sh into the target
#      data directory,
#   2. writes restore_command + recovery_target_* into postgresql.auto.conf,
#   3. touches recovery.signal so the cluster enters archive recovery.
#
# Start the cluster afterwards (example):
#   pg_ctl -D /restore/pitr -l /restore/pitr.log -w start
# PostgreSQL replays archived WAL up to --target-time, then promotes
# (recovery_target_action=promote).
#
# Usage:
#   restore_pitr.sh --base-backup DIR | --base-backup-tgz FILE | --data-dir DIR \
#                 --restore-dir DIR --target-time 'YYYY-MM-DD HH:MM:SS[+TZ]'
#                 [--archive-dir DIR | --s3-uri s3://bucket/prefix]
#                 [--dry-run]
#
# --data-dir skips the copy when the base backup already occupies a directory
# you want to configure in place (scratch restores).
# -----------------------------------------------------------------------------
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

base_backup='' base_tgz='' data_dir='' restore_dir='' target_time=''
archive_dir='' s3_uri='' dry_run=0

usage() { sed -n '2,26p' "$0" >&2; exit 64; }
while [[ $# -gt 0 ]]; do
    case $1 in
        --base-backup)     base_backup=$2; shift 2;;
        --base-backup-tgz) base_tgz=$2; shift 2;;
        --data-dir)        data_dir=$2; shift 2;;
        --restore-dir)     restore_dir=$2; shift 2;;
        --target-time)     target_time=$2; shift 2;;
        --archive-dir)     archive_dir=$2; shift 2;;
        --s3-uri)          s3_uri=$2; shift 2;;
        --dry-run)         dry_run=1; shift;;
        -h|--help)         usage;;
        *) echo "restore_pitr: unknown arg $1" >&2; usage;;
    esac
done

[[ -n $target_time ]] || usage
# Exactly one source of the data directory.
src_count=0
[[ -n $base_backup ]] && src_count=$((src_count+1))
[[ -n $base_tgz ]]    && src_count=$((src_count+1))
[[ -n $data_dir ]]    && src_count=$((src_count+1))
[[ $src_count -eq 1 ]] || usage
# --data-dir configures in place; the other modes need --restore-dir.
if [[ -n $data_dir ]]; then
    restore_dir=$data_dir
else
    [[ -n $restore_dir ]] || usage
fi
[[ -n $archive_dir || -n $s3_uri ]] || usage

run() { echo "+ $*"; if [[ $dry_run -eq 0 ]]; then "$@"; fi; }

# 1. Materialise the base backup at $restore_dir.
if [[ -n $base_backup ]]; then
    run mkdir -p "$restore_dir"
    run cp -a "$base_backup/." "$restore_dir/"
elif [[ -n $base_tgz ]]; then
    run mkdir -p "$restore_dir"
    run tar -xzf "$base_tgz" -C "$restore_dir" --strip-components=1
fi
# (--data-dir: nothing to copy; $restore_dir already points at it.)

# 2. Configure recovery. env vars are expanded by the shell that PostgreSQL
#    invokes for restore_command, so the same scripts serve local and S3.
if [[ -n $s3_uri ]]; then
    backend_env="PITR_ARCHIVE_BACKEND=s3 PITR_S3_URI=$s3_uri"
else
    backend_env="PITR_ARCHIVE_BACKEND=local PITR_ARCHIVE_DIR=$archive_dir"
fi

auto_conf=$restore_dir/postgresql.auto.conf
{
    echo "# PITR restore — written by restore_pitr.sh $(date -u +%FT%TZ)"
    echo "restore_command = '$backend_env $script_dir/wal_restore.sh %f %p'"
    echo "recovery_target_time = '$target_time'"
    echo "recovery_target_action = 'promote'"
    echo "recovery_target_timeline = 'latest'"
} | if [[ $dry_run -eq 1 ]]; then cat; else cat >> "$auto_conf"; fi

if [[ $dry_run -eq 0 ]]; then
    touch "$restore_dir/recovery.signal"
    # Base backups ship permissive copy modes; Postgres wants 700.
    chmod 700 "$restore_dir"
fi

cat <<EOF
restore_pitr: staged $restore_dir
  target time : $target_time
  archive     : ${s3_uri:-$archive_dir}
Start with:   pg_ctl -D $restore_dir -l $restore_dir.recovery.log -w start
Then check:   tail -f $restore_dir.recovery.log  # "recovery stopping before commit of ..."
EOF
