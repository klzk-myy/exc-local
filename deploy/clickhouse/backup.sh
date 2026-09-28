#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# backup.sh — ClickHouse backup & DR driver (Task 4.3.6, spec §18.3)
#
# DR model:
#   * RPO <= 60s is provided by the replicated cluster (ReplicatedMergeTree
#     across AZs, spec §18.2) — a single-node loss is a replication event,
#     not a restore.
#   * These S3 backups are the tertiary/cold tier covering REGION loss and
#     logical corruption. Daily full + hourly incremental; RTO <= 30 min is
#     restore_remote + row-count verification on a scratch cluster.
#
# Schedule (crontab):
#   5 1 * * *   /opt/exchange/deploy/clickhouse/backup.sh full          # daily
#   20 * * * *  /opt/exchange/deploy/clickhouse/backup.sh incremental   # hourly
#
# Usage:
#   backup.sh full                       daily full backup -> S3
#   backup.sh incremental                hourly incremental -> S3
#   backup.sh list                       list remote backups
#   backup.sh restore <name>             restore_remote <name> (run against a
#                                        scratch cluster via a second config)
#   backup.sh counts <out.tsv> [part]    dump table<TAB>rows for a partition
#                                        (default: today's) from this cluster
#   backup.sh verify <a.tsv> <b.tsv>     compare two `counts` files; exit 1 on
#                                        any missing table or count mismatch
#
# Environment:
#   CLICKHOUSE_BACKUP_BIN    clickhouse-backup binary (default: PATH lookup)
#   CLICKHOUSE_BACKUP_CONFIG config file (default: clickhouse-backup/config.yml
#                            next to this script)
#   CLICKHOUSE_CLIENT_BIN    clickhouse-client binary for counts/verify
#   CH_HOST/CH_PORT/CH_USER/CH_PASSWORD    cluster connection for `counts`
#   CH_DATABASE              database to sample (default: default)
# -----------------------------------------------------------------------------
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

chb=${CLICKHOUSE_BACKUP_BIN:-clickhouse-backup}
chb_config=${CLICKHOUSE_BACKUP_CONFIG:-$script_dir/clickhouse-backup/config.yml}
chc=${CLICKHOUSE_CLIENT_BIN:-clickhouse-client}

chb_run() { "$chb" -c "$chb_config" "$@"; }

# Emit "db.table<TAB>total_rows" for active parts in one partition.
emit_counts() {
    local partition=${1:-$(date -u +%Y%m%d)}
    local -a args=(
        --host "${CH_HOST:-127.0.0.1}"
        --port "${CH_PORT:-9000}"
    )
    [[ -n ${CH_USER:-} ]]     && args+=(--user "$CH_USER")
    [[ -n ${CH_PASSWORD:-} ]] && args+=(--password "$CH_PASSWORD")
    "$chc" "${args[@]}" --query "
            SELECT database || '.' || table AS t, sum(rows)
            FROM system.parts
            WHERE active AND database = '${CH_DATABASE:-default}'
                  AND partition_id = '${partition}'
            GROUP BY t ORDER BY t
            FORMAT TabSeparated"
}

# Compare two counts files: every table in the source must exist in the
# restore with an identical row count. Prints each delta; exit 1 on any.
verify_counts() {
    local src=$1 dst=$2 mismatches=0
    while IFS=$'\t' read -r table rows; do
        [[ -n $table ]] || continue
        local got
        got=$(awk -F'\t' -v t="$table" '$1==t{print $2}' "$dst" | head -1)
        if [[ -z $got ]]; then
            echo "verify: MISSING $table in restore" >&2
            mismatches=$((mismatches+1))
        elif [[ $got != "$rows" ]]; then
            echo "verify: MISMATCH $table src=$rows restored=$got" >&2
            mismatches=$((mismatches+1))
        else
            echo "verify: OK $table rows=$rows"
        fi
    done < "$src"
    # Extra tables in the restore are a warning, not a failure (scratch
    # clusters may hold unrelated data).
    while IFS=$'\t' read -r table _; do
        [[ -n $table ]] || continue
        grep -qF "$table	" "$src" || echo "verify: NOTE extra $table in restore" >&2
    done < "$dst"
    return "$mismatches"
}

cmd=${1:-full}
case "$cmd" in
full)
    name=daily-$(date -u +%Y%m%d)
    echo "backup.sh: full backup -> $name"
    chb_run create_remote "$name"
    ;;
incremental)
    name=incr-$(date -u +%Y%m%d-%H%M%S)
    echo "backup.sh: incremental backup -> $name"
    chb_run create_remote --incremental "$name"
    ;;
list)
    chb_run list remote
    ;;
restore)
    name=${2:?'usage: backup.sh restore <backup-name>'}
    echo "backup.sh: restoring $name (point CLICKHOUSE_BACKUP_CONFIG at the scratch cluster)"
    chb_run restore_remote "$name"
    ;;
counts)
    out=${2:?'usage: backup.sh counts <out.tsv> [partition_id]'}
    emit_counts "${3:-}" > "$out"
    echo "backup.sh: wrote $out"
    ;;
verify)
    [[ $# -eq 3 ]] || { echo "usage: backup.sh verify <src.tsv> <dst.tsv>" >&2; exit 64; }
    verify_counts "$2" "$3"
    ;;
*)
    sed -n '2,40p' "$0" >&2
    exit 64
    ;;
esac
