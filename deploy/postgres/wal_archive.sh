#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# wal_archive.sh — archive_command indirection (Task 4.3.4)
#
# Invoked by PostgreSQL as:  wal_archive.sh %p %f
#   %p = path of the WAL file to archive (relative to $PGDATA)
#   %f = WAL file name only
#
# Backends (PITR_ARCHIVE_BACKEND):
#   s3    — `aws s3 cp` to ${PITR_S3_URI:-s3://exchange-pitr}/%f.
#           S3-compatible stores (MinIO, R2, Ceph) are reached by setting
#           AWS_ENDPOINT_URL, which is passed through as --endpoint-url.
#   local — copy into ${PITR_ARCHIVE_DIR} (default ./wal_archive next to the
#           calling data directory). Used for dev and by pitr_smoke.sh.
#
# archive_command contract: exit 0 iff the file is durably archived. On
# failure PostgreSQL retries — so this script never exits 0 on a partial or
# mismatched copy.
# -----------------------------------------------------------------------------
set -euo pipefail

src=${1:?'usage: wal_archive.sh <wal-path> <wal-file>'}
file=${2:?'usage: wal_archive.sh <wal-path> <wal-file>'}

backend=${PITR_ARCHIVE_BACKEND:-local}

case "$backend" in
s3)
    s3_uri=${PITR_S3_URI:-s3://exchange-pitr}
    endpoint_args=()
    if [[ -n ${AWS_ENDPOINT_URL:-} ]]; then
        endpoint_args=(--endpoint-url "$AWS_ENDPOINT_URL")
    fi
    aws s3 cp --only-show-errors "${endpoint_args[@]}" "$src" "${s3_uri%/}/$file"
    ;;
local)
    archive_dir=${PITR_ARCHIVE_DIR:-./wal_archive}
    mkdir -p "$archive_dir"
    dest=$archive_dir/$file
    if [[ -f $dest ]]; then
        # PostgreSQL may re-invoke archive_command for a file that already
        # exists (e.g. after a crash). Identical content → already archived.
        if cmp -s "$src" "$dest"; then
            exit 0
        fi
        echo "wal_archive: $dest exists with different content" >&2
        exit 1
    fi
    # Copy to a temp name then rename so a reader never sees a partial file.
    tmp=$dest.tmp.$$
    cp -- "$src" "$tmp"
    mv -- "$tmp" "$dest"
    ;;
*)
    echo "wal_archive: unknown PITR_ARCHIVE_BACKEND '$backend'" >&2
    exit 1
    ;;
esac
