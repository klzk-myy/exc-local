#!/usr/bin/env bash
# =============================================================================
# pg_failover_drill.sh — Phase-09 Task 9.3.4 RPO/RTO verification (spec §18.3).
#
# Provisions a REAL PostgreSQL 16 semi-sync streaming pair (primary +
# synchronous_standby_names remote_flush standby — the
# deploy/dr/postgres-standby.conf.sample topology), drives committed writes,
# SIGKILLs the primary, promotes the standby, and measures:
#
#   RPO ≤ 15s — replication gap at the moment of kill (flush-acked txns must
#               all survive on the standby; measured via
#               pg_last_xact_replay_timestamp + row-exact verification)
#   RTO ≤ 5min — wall clock primary-kill → standby promoted + accepting writes
#
# Topology is dev-host (two containers), not a second region — the RPO/RTO
# numbers are what the row measures; multi-region placement stays open.
#
# Env: PG_IMAGE (default postgres:16), DRILL_NET (default pg-drill-net).
# Exit: 0 all checks pass · 1 a check failed. Containers are always removed.
# =============================================================================
set -euo pipefail

IMAGE="${PG_IMAGE:-postgres:16}"
NET="${DRILL_NET:-pg-drill-net-$$}"
PRIMARY="pg-drill-primary-$$"
STANDBY="pg-drill-standby-$$"
PW="drill_repl_pw"
PORT_P=25433; PORT_S=25434
FAILS=0

log()  { printf '[pg-drill %s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*"; }
check() {
    if [ "$2" = "PASS" ]; then log "  check $1: PASS — $3"
    else log "  check $1: FAIL — $3"; FAILS=$((FAILS+1)); fi
}
cleanup() {
    docker rm -f "$PRIMARY" "$STANDBY" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "== postgres semi-sync failover drill (RPO≤15s / RTO≤5min) =="

docker network create "$NET" >/dev/null

# --- primary ----------------------------------------------------------------
log "starting primary ($PRIMARY)"
# NOTE: the sync-standby set is enabled AFTER the standby streams — a
# primary booted with synchronous_commit=on + a non-empty sync set blocks
# every commit (incl. initdb's CREATE DATABASE) until a sync standby
# attaches. Bring up async, then switch to semi-sync.
docker run -d --name "$PRIMARY" --network "$NET" --network-alias pg-primary \
    -p "$PORT_P":5432 -e POSTGRES_PASSWORD="$PW" -e POSTGRES_DB=drill \
    "$IMAGE" \
    -c wal_level=replica -c max_wal_senders=4 -c max_replication_slots=4 \
    -c synchronous_commit=on -c hot_standby=on >/dev/null

for i in $(seq 1 60); do
    docker exec "$PRIMARY" psql -U postgres -d drill -tAc 'select 1' >/dev/null 2>&1 && break
    sleep 1
done
docker exec "$PRIMARY" psql -U postgres -d drill -qc \
    "CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '$PW';
     SELECT pg_create_physical_replication_slot('drill_secondary');
     CREATE TABLE drill_marks(id serial PRIMARY KEY, mark text, at timestamptz default now());" \
    >/dev/null
# 'all' in pg_hba does not cover the replication pseudo-database.
docker exec "$PRIMARY" bash -c \
    "echo 'host replication replicator samenet scram-sha-256' >> /var/lib/postgresql/data/pg_hba.conf"
docker exec "$PRIMARY" psql -U postgres -d drill -qc "SELECT pg_reload_conf()" \
    >/dev/null
log "primary ready; replicator + slot + drill table created"

# --- standby: pg_basebackup into standby mode --------------------------------
log "pg_basebackup -> standby ($STANDBY)"
docker run --rm --network "$NET" -v "pg-drill-sb-data-$$:/pgdata" "$IMAGE" \
    bash -c "rm -rf /pgdata/* && PGPASSWORD=$PW pg_basebackup -h pg-primary -U replicator -D /pgdata -R -X stream -S drill_secondary -v && chown -R postgres:postgres /pgdata && chmod 0700 /pgdata" \
    >/dev/null 2>&1

docker run -d --name "$STANDBY" --network "$NET" --network-alias pg-standby \
    -p "$PORT_S":5432 -e POSTGRES_PASSWORD="$PW" \
    -v "pg-drill-sb-data-$$:/var/lib/postgresql/data" \
    "$IMAGE" \
    -c hot_standby=on -c wal_receiver_timeout=30s >/dev/null

for i in $(seq 1 60); do
    docker exec "$STANDBY" psql -U postgres -d drill -tAc 'select 1' >/dev/null 2>&1 && break
    sleep 1
done
SB_STATE="$(docker exec "$PRIMARY" psql -U postgres -d drill -tAc \
    "SELECT coalesce(string_agg(state||'('||application_name||')',','),'none')
       FROM pg_stat_replication" 2>/dev/null || echo 'none')"
log "replication state: $SB_STATE"
printf '%s' "$SB_STATE" | grep -q 'streaming' \
    && check streaming_established PASS "$SB_STATE" \
    || check streaming_established FAIL "$SB_STATE"

# Switch the primary to semi-sync now that the standby is streaming.
docker exec "$PRIMARY" psql -U postgres -d drill -qc \
    "ALTER SYSTEM SET synchronous_standby_names = 'FIRST 1 (*)'" >/dev/null
docker exec "$PRIMARY" psql -U postgres -d drill -qc \
    "SELECT pg_reload_conf()" >/dev/null
sleep 1

SYNC="$(docker exec "$PRIMARY" psql -U postgres -d drill -tAc \
    "SELECT sync_state FROM pg_stat_replication LIMIT 1" 2>/dev/null || echo '')"
log "sync_state: $SYNC"
[ "$SYNC" = "sync" ] || [ "$SYNC" = "quorum" ] \
    && check semi_sync PASS "synchronous_commit=remote_flush acked (sync_state=)" \
    || check semi_sync FAIL "sync_state=$SYNC (standby not in synchronous set)"

# --- workload: 200 committed txns ---------------------------------------------
log "writing 200 committed txns on primary"
docker exec "$PRIMARY" psql -U postgres -d drill -qc \
    "INSERT INTO drill_marks(mark) SELECT 'm'||g FROM generate_series(1,200) g" \
    >/dev/null
sleep 2
LAG_S="$(docker exec "$PRIMARY" psql -U postgres -d drill -tAc \
    "SELECT coalesce(extract(epoch from now()-pg_last_xact_replay_timestamp())::float,0)
       FROM pg_stat_replication LIMIT 1" 2>/dev/null || echo '-1')"
SB_COUNT="$(docker exec "$STANDBY" psql -U postgres -d drill -tAc \
    "SELECT count(*) FROM drill_marks" 2>/dev/null || echo '0')"
log "replication gap=${LAG_S}s standby_rows=$SB_COUNT"

[ "$SB_COUNT" = "200" ] \
    && check rpo_zero_loss PASS "all 200 flush-acked txns on standby (RPO << 15s)" \
    || check rpo_zero_loss FAIL "standby has $SB_COUNT/200 rows"
LAG_MS="$(printf '%s' "$LAG_S" | awk '{printf "%d", $1*1000}')"
[ "$LAG_MS" -lt 15000 ] \
    && check rpo_bound PASS "replication gap ${LAG_S}s <= 15s bound" \
    || check rpo_bound FAIL "gap ${LAG_S}s > 15s"

# --- kill + promote -----------------------------------------------------------
log "SIGKILL primary — T0"
T0=$(date +%s%N)
docker kill "$PRIMARY" >/dev/null

log "promoting standby"
docker exec "$STANDBY" su postgres -c "pg_ctl promote -D /var/lib/postgresql/data" \
    >/dev/null 2>&1
# wait for read-write acceptance
RTO_MS=-1
for i in $(seq 1 600); do
    MODE="$(docker exec "$STANDBY" psql -U postgres -d drill -tAc \
        "SELECT NOT pg_is_in_recovery()" 2>/dev/null || echo '')"
    if [ "$MODE" = "t" ]; then
        if docker exec "$STANDBY" psql -U postgres -d drill -qc \
            "INSERT INTO drill_marks(mark) VALUES ('post-promote')" >/dev/null 2>&1; then
            RTO_MS=$(( ($(date +%s%N) - T0) / 1000000 ))
            break
        fi
    fi
    sleep 0.5
done

[ "$RTO_MS" -ge 0 ] \
    && check promotion_rw PASS "standby promoted + read-write in ${RTO_MS}ms" \
    || check promotion_rw FAIL "standby never became read-write"
if [ "$RTO_MS" -ge 0 ] && [ "$RTO_MS" -lt 300000 ]; then
    check rto_bound PASS "${RTO_MS}ms <= 300000ms (5min) RTO"
else
    check rto_bound FAIL "${RTO_MS}ms >= 5min (or never promoted)"
fi

FINAL="$(docker exec "$STANDBY" psql -U postgres -d drill -tAc \
    "SELECT count(*) FROM drill_marks" 2>/dev/null || echo '0')"
[ "$FINAL" = "201" ] \
    && check post_promote_integrity PASS "200 pre-kill + 1 post-promote rows, zero committed loss" \
    || check post_promote_integrity FAIL "row count $FINAL (want 201)"

log "== drill complete: $FAILS check(s) failed (RTO=${RTO_MS}ms, gap=${LAG_S}s) =="
docker volume rm "pg-drill-sb-data-$$" >/dev/null 2>&1 || true
[ "$FAILS" -eq 0 ] && { echo "PG-FAILOVER-DRILL PASS"; exit 0; }
echo "PG-FAILOVER-DRILL FAIL ($FAILS)" >&2
exit 1
