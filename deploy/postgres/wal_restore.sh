#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# wal_restore.sh — restore_command indirection (Task 4.3.4)
#
# Invoked by PostgreSQL during recovery as:  wal_restore.sh %f %p
#   %f = WAL file name Postgres wants
#   %p = destination path to write it to
#
# Mirror of wal_archive.sh. Backends (PITR_ARCHIVE_BACKEND):
#   s3    — `aws s3 cp ${PITR_S3_URI}/%f %p` (AWS_ENDPOINT_URL honoured for
#           S3-compatible stores).
#   local — copy from ${PITR_ARCHIVE_DIR}/%f.
#
# Contract: exit 0 when %p was written; non-zero means "not in archive" —
# PostgreSQL then falls back to pg_wal and eventually ends recovery.
# -----------------------------------------------------------------------------
set -euo pipefail

file=${1:?'usage: wal_restore.sh <wal-file> <dest-path>'}
dest=${2:?'usage: wal_restore.sh <wal-file> <dest-path>'}

backend=${PITR_ARCHIVE_BACKEND:-local}

case "$backend" in
s3)
    s3_uri=${PITR_S3_URI:-s3://exchange-pitr}
    endpoint_args=()
    if [[ -n ${AWS_ENDPOINT_URL:-} ]]; then
        endpoint_args=(--endpoint-url "$AWS_ENDPOINT_URL")
    fi
    # A missing object exits non-zero, which is exactly the "end of archive"
    # signal PostgreSQL expects.
    aws s3 cp --only-show-errors "${endpoint_args[@]}" "${s3_uri%/}/$file" "$dest"
    ;;
local)
    archive_dir=${PITR_ARCHIVE_DIR:-./wal_archive}
    src=$archive_dir/$file
    if [[ ! -f $src ]]; then
        exit 1
    fi
    cp -- "$src" "$dest"
    ;;
*)
    echo "wal_restore: unknown PITR_ARCHIVE_BACKEND '$backend'" >&2
    exit 1
    ;;
esac
