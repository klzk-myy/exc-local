#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# pitr_smoke.sh — self-contained PITR verification on a scratch cluster
# (Task 4.3.4). Proves on real PostgreSQL binaries:
#   * archive_command (wal_archive.sh, local backend) archives WAL,
#   * pg_basebackup produces a restorable base backup,
#   * restore_command (wal_restore.sh) + recovery_target_time replay WAL and
#     stop at the requested instant — rows committed after the target are
#     absent, rows before it are present.
#
# It exercises the same machinery production uses; only the archive backend
# differs (local dir vs S3) and the production RTO clock depends on data
# volume.
#
#   ./pitr_smoke.sh [--keep] [--pg-bin /path/to/bin]
#
# Env: PGBIN (postgres bin dir; default: /www/server/pgsql/bin then PATH),
#      PITR_SMOKE_DIR (scratch root; default mktemp -d).
# -----------------------------------------------------------------------------
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
keep=0 pg_bin_arg=''
while [[ $# -gt 0 ]]; do
    case $1 in
        --keep)   keep=1; shift;;
        --pg-bin) pg_bin_arg=$2; shift 2;;
        *) echo "pitr_smoke: unknown arg $1" >&2; exit 64;;
    esac
done

pgbin=${pg_bin_arg:-${PGBIN:-}}
if [[ -z $pgbin ]]; then
    for cand in /www/server/pgsql/bin /usr/lib/postgresql/*/bin; do
        [[ -x $cand/initdb ]] && pgbin=$cand && break
    done
fi
initdb=${pgbin:+$pgbin/}initdb
pg_ctl=${pgbin:+$pgbin/}pg_ctl
psql=${pgbin:+$pgbin/}psql
pg_basebackup=${pgbin:+$pgbin/}pg_basebackup
for b in "$initdb" "$pg_ctl" "$psql" "$pg_basebackup"; do
    command -v "$b" >/dev/null || {
        echo "pitr_smoke: SKIP — $b not found (set PGBIN/--pg-bin)" >&2
        exit 77; }
done

if [[ $(id -u) -eq 0 ]]; then
    echo "pitr_smoke: SKIP — PostgreSQL refuses to run as root" >&2
    exit 77
fi

work=${PITR_SMOKE_DIR:-$(mktemp -d /tmp/pitr_smoke.XXXXXX)}
primary=$work/primary
archive=$work/archive
base=$work/base
replica=$work/replica
sock=$work/sock
mkdir -p "$archive" "$sock"

cleanup() {
    "$pg_ctl" -D "$replica" -m immediate stop >/dev/null 2>&1 || true
    "$pg_ctl" -D "$primary" -m immediate stop >/dev/null 2>&1 || true
    if [[ $keep -eq 0 ]]; then rm -rf "$work"; else
        echo "pitr_smoke: kept $work"; fi
}
trap cleanup EXIT

run_psql() { "$psql" -X -h "$sock" -U postgres -d postgres "$@"; }
wait_archive() { # wait until at least N files archived (timeout 30s)
    local want=$1 n=0
    for _ in $(seq 1 60); do
        n=$(find "$archive" -type f -name '0*' | wc -l)
        [[ $n -ge $want ]] && return 0
        sleep 0.5
    done
    echo "pitr_smoke: archive lag — only $n segments after 30s" >&2
    return 1
}

echo "==> initdb scratch primary ($work)"
"$initdb" -D "$primary" -A trust -U postgres -E UTF8 --no-sync >/dev/null

cat >> "$primary/postgresql.conf" <<EOF
listen_addresses = ''
unix_socket_directories = '$sock'
wal_level = replica
archive_mode = on
archive_timeout = 5
archive_command = 'PITR_ARCHIVE_BACKEND=local PITR_ARCHIVE_DIR=$archive $script_dir/wal_archive.sh %p %f'
track_commit_timestamp = on
max_wal_senders = 4
logging_collector = off
EOF

echo "==> start primary"
"$pg_ctl" -D "$primary" -l "$work/primary.log" -w -t 60 start >/dev/null

run_psql -q <<'SQL'
CREATE TABLE pitr_probe (batch int NOT NULL, ts timestamptz NOT NULL DEFAULT now());
INSERT INTO pitr_probe (batch) SELECT 1 FROM generate_series(1, 10);
CHECKPOINT;
SQL

echo "==> base backup"
"$pg_basebackup" -h "$sock" -U postgres -D "$base" -Fp -Xs -c fast -P

# Recovery target: strictly between batch 1 and batch 2 commits.
sleep 3
target=$(run_psql -Atc "SELECT now()")
sleep 3

run_psql -q <<'SQL'
INSERT INTO pitr_probe (batch) SELECT 2 FROM generate_series(1, 10);
SELECT pg_switch_wal();
SQL
wait_archive 2
"$pg_ctl" -D "$primary" -m fast -w stop >/dev/null
n_archived=$(find "$archive" -type f -name '0*' | wc -l)
[[ $n_archived -ge 1 ]] || { echo "FAIL: no WAL archived"; exit 1; }
echo "    archived $n_archived WAL segment(s); target=$target"

echo "==> stage PITR replica at $target"
"$script_dir/restore_pitr.sh" --data-dir "$base" \
    --archive-dir "$archive" --target-time "$target" >/dev/null
# The "replica" is the configured base backup itself (base dir renamed for
# clarity so both clusters are distinguishable in logs).
mv "$base" "$replica"

cat >> "$replica/postgresql.conf" <<EOF
listen_addresses = ''
unix_socket_directories = '$sock'
EOF

echo "==> start replica (archive recovery)"
"$pg_ctl" -D "$replica" -l "$work/replica.log" -w -t 60 start >/dev/null

b1=$(run_psql -Atc "SELECT count(*) FROM pitr_probe WHERE batch = 1")
b2=$(run_psql -Atc "SELECT count(*) FROM pitr_probe WHERE batch = 2")
echo "    replica rows: batch1=$b1 batch2=$b2 (want 10 / 0)"

rc=0
[[ $b1 == 10 ]] || { echo "FAIL: batch-1 rows missing ($b1)"; rc=1; }
[[ $b2 == 0 ]]  || { echo "FAIL: batch-2 rows replayed past target ($b2)"; rc=1; }
grep -q "recovery stopping" "$work/replica.log" \
    && echo "    log: $(grep 'recovery stopping' "$work/replica.log" | tail -1)"

if [[ $rc -eq 0 ]]; then
    echo "PASS: archive + PITR replay verified (target=$target)"
else
    echo "FAIL: see $work/replica.log" >&2
    keep=1
fi
exit $rc
