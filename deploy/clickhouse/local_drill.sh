#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# local_drill.sh — ClickHouse restore drill on the dev host (Task 4.3.6,
# spec §18.3). Same mechanism as the quarterly RUNBOOK drill, adapted for a
# single-node dev environment:
#
#   * backup lives on a LOCAL S3-compatible gateway (versitygw posix or
#     MinIO) instead of the production bucket;
#   * the "scratch cluster" is a scratch DATABASE on the dev server —
#     restore_remote --restore-database-mapping rewrites
#     exchange_analytics -> $SCRATCH_DB so production data is untouched;
#   * clickhouse-backup needs the ClickHouse DATA DIR locally — on the dev
#     stack the server is a container, so the binary is docker-cp'd in and
#     runs via docker exec (host-side works unchanged on bare metal).
#
# Usage:
#   local_drill.sh <backup-name>           full drill end-to-end
#   local_drill.sh restore <backup-name>   restore+verify only (backup exists)
#
# Env: CH_CONTAINER (default exc-dev-clickhouse-1), CHB_BIN, S3_ENDPOINT,
#      S3_ACCESS/S3_SECRET, SCRATCH_DB, CH_USER/CH_PASSWORD, DRILL_WORKDIR.
# -----------------------------------------------------------------------------
set -euo pipefail

CH_CONTAINER=${CH_CONTAINER:-exc-dev-clickhouse-1}
CHB_BIN=${CHB_BIN:-/tmp/clickhouse-backup}
S3_ENDPOINT=${S3_ENDPOINT:-http://172.18.0.1:17070}   # host from container net
S3_ACCESS=${S3_ACCESS:-testaccess}
S3_SECRET=${S3_SECRET:-testsecret}
S3_BUCKET=${S3_BUCKET:-exchange-ch-backup}
SCRATCH_DB=${SCRATCH_DB:-ch_drill_scratch}
CH_USER=${CH_USER:-exchange}
CH_PASSWORD=${CH_PASSWORD:-exchange_dev}
DRILL_WORKDIR=${DRILL_WORKDIR:-/tmp/ch-drill}
CH_HTTP=${CH_HTTP:-http://localhost:8123}

chq() { curl -sf "$CH_HTTP/" -u "$CH_USER:$CH_PASSWORD" --data-binary "$1"; }

write_ctr_config() {
    mkdir -p "$DRILL_WORKDIR"
    cat > "$DRILL_WORKDIR/config.yml" <<EOF
general:
  remote_storage: s3
  backups_to_keep_remote: 35
  max_concurrency: 4
clickhouse:
  host: 127.0.0.1
  port: 9000
  username: ${CH_USER}
  password: ${CH_PASSWORD}
  secure: false
  skip_tables:
    - system.*
    - INFORMATION_SCHEMA.*
    - information_schema.*
    - ${SCRATCH_DB}.*
  timeout: 15m
s3:
  access_key: ${S3_ACCESS}
  secret_key: ${S3_SECRET}
  bucket: ${S3_BUCKET}
  endpoint: ${S3_ENDPOINT}
  region: us-east-1
  acl: private
  force_path_style: true
  compression_format: zstd
  part_size: 128MiB
EOF
    docker cp "$DRILL_WORKDIR/config.yml" "$CH_CONTAINER:/tmp/drill-config.yml"
}

counts() { # counts <database> <out.tsv>
    chq "SELECT table AS t, sum(rows) FROM system.parts
         WHERE active AND database='$1' GROUP BY t ORDER BY t
         FORMAT TabSeparated" > "$2"
}

cmd=${1:-}
name=${2:-drill-$(date -u +%Y%m%d)}
[[ $cmd == restore ]] && name=$1 && cmd=restore

case "${1:-drill}" in
drill|restore)
    write_ctr_config
    if [[ ${1:-drill} == drill ]]; then
        # clean-slate: a prior drill run may have left a same-named remote
        # backup (resume state) or a half-dropped scratch db (Atomic-engine
        # orphan dirs); both must go before create_remote/restore_remote.
        docker exec "$CH_CONTAINER" \
            "$CHB_BIN" -c /tmp/drill-config.yml delete remote "$name" \
            >/dev/null 2>&1 || true
        chq "DROP DATABASE IF EXISTS $SCRATCH_DB SYNC" || true
        echo ">> create_remote $name"
        docker exec "$CH_CONTAINER" \
            "$CHB_BIN" -c /tmp/drill-config.yml create_remote "$name" \
            --tables "exchange_analytics.*"
    fi
    counts exchange_analytics "$DRILL_WORKDIR/src.tsv"

    echo ">> restore_remote $name -m exchange_analytics:$SCRATCH_DB"
    t0=$(date +%s%3N)
    docker exec "$CH_CONTAINER" \
        "$CHB_BIN" -c /tmp/drill-config.yml restore_remote "$name" \
            -m "exchange_analytics:$SCRATCH_DB"
    t1=$(date +%s%3N)
    echo ">> RTO: $((t1 - t0)) ms (target <= 1800000 ms)"

    counts "$SCRATCH_DB" "$DRILL_WORKDIR/dst_raw.tsv"
    # map scratch db.table back to source names for verify
    awk -F'\t' -v d="$SCRATCH_DB" -v s="exchange_analytics" \
        '{sub("^" d "\\.", s "."); print}' "$DRILL_WORKDIR/dst_raw.tsv" \
        > "$DRILL_WORKDIR/dst.tsv"
    "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/backup.sh" verify \
        "$DRILL_WORKDIR/src.tsv" "$DRILL_WORKDIR/dst.tsv"
    echo ">> drill PASS: $name -> $SCRATCH_DB (drop it: DROP DATABASE $SCRATCH_DB)"
    ;;
*)
    sed -n '2,30p' "$0" >&2
    exit 64
    ;;
esac
