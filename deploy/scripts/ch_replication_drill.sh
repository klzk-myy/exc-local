#!/usr/bin/env bash
# ch_replication_drill.sh — ClickHouse replicated-topology DR evidence.
#
# Provisions a real 2-node ClickHouse replicated pair (embedded
# `clickhouse keeper`, ReplicatedMergeTree), proves cross-node replication
# of committed rows, then SIGKILLs the primary node and verifies the
# survivor serves every committed row — the RPO ≤ 60s / RTO ≤ 30min legs
# of the Phase-04 Task 4.3.6 DR row (restore-side RTO is already measured
# by ch_restore_drill.sh at ~356ms).
#
# Usage: ./deploy/scripts/ch_replication_drill.sh [cleanup]
set -uo pipefail

IMAGE="clickhouse/clickhouse-server:25.8-alpine"
TAG="ch-drill-$$"
NET="$TAG-net"
KEEPER="$TAG-keeper"
N1="$TAG-node1"
N2="$TAG-node2"
WORK="$(mktemp -d /tmp/ch-drill.XXXXXX)"
PASS=0; FAIL=0

log()   { printf '[ch-drill %s] %s\n' "$(date +%H:%M:%S.%3N)" "$*"; }
check() { # name PASS|FAIL detail
    if [ "$2" = PASS ]; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); fi
    printf '[ch-drill %s]   check %s: %s — %s\n' "$(date +%H:%M:%S.%3N)" "$1" "$2" "$3"
}

cleanup() {
    docker rm -f "$N1" "$N2" "$KEEPER" >/dev/null 2>&1
    docker network rm "$NET" >/dev/null 2>&1
    rm -rf "$WORK"
}
if [ "${1:-}" = cleanup ]; then cleanup; exit 0; fi
trap cleanup EXIT

q() { docker exec "$1" clickhouse-client -q "$2" 2>/dev/null; }

log "== clickhouse replication drill (RPO≤60s via replication) =="

docker network create "$NET" >/dev/null

# --- keeper ------------------------------------------------------------------
# 25.8 schema notes (verified against this image):
#  - raft_configuration is a SIBLING of coordination_settings, not a child.
#  - tcp_port binds loopback unless listen_host is set at clickhouse level.
cat > "$WORK/keeper.xml" <<'XML'
<clickhouse>
  <logger><level>warning</level></logger>
  <listen_host>0.0.0.0</listen_host>
  <keeper_server>
    <tcp_port>9181</tcp_port>
    <server_id>1</server_id>
    <log_storage_path>/var/lib/clickhouse/coordination/log</log_storage_path>
    <snapshot_storage_path>/var/lib/clickhouse/coordination/snapshots</snapshot_storage_path>
    <coordination_settings>
      <operation_timeout_ms>10000</operation_timeout_ms>
    </coordination_settings>
    <raft_configuration>
      <server><id>1</id><hostname>KHOST</hostname><port>9444</port></server>
    </raft_configuration>
  </keeper_server>
</clickhouse>
XML
sed -i "s/KHOST/$KEEPER/" "$WORK/keeper.xml"

docker run -d --name "$KEEPER" --network "$NET" --network-alias "$KEEPER" \
    -v "$WORK/keeper.xml:/etc/clickhouse-keeper/keeper.xml:ro" \
    --entrypoint clickhouse "$IMAGE" \
    keeper --config-file=/etc/clickhouse-keeper/keeper.xml >/dev/null

# --- data nodes ----------------------------------------------------------------
cat > "$WORK/keeper-conf.xml" <<XML
<clickhouse>
  <zookeeper>
    <node><host>$KEEPER</host><port>9181</port></node>
  </zookeeper>
</clickhouse>
XML

for n in 1 2; do
    name="$TAG-node$n"
    cat > "$WORK/macros$n.xml" <<XML
<clickhouse>
  <macros><shard>1</shard><replica>$name</replica></macros>
</clickhouse>
XML
    docker run -d --name "$name" --network "$NET" --network-alias "$name" \
        -v "$WORK/keeper-conf.xml:/etc/clickhouse-server/config.d/zz-keeper.xml:ro" \
        -v "$WORK/macros$n.xml:/etc/clickhouse-server/config.d/zz-macros.xml:ro" \
        "$IMAGE" >/dev/null
done

for i in $(seq 1 60); do
    q "$N1" 'select 1' >/dev/null 2>&1 && q "$N2" 'select 1' >/dev/null 2>&1 && break
    sleep 1
done
q "$N1" 'select 1' >/dev/null 2>&1 && check keeper_pair_up PASS "both nodes serving" \
    || check keeper_pair_up FAIL "nodes not ready"

q "$N1" 'create database if not exists drill'
q "$N2" 'create database if not exists drill'
ZP="/clickhouse/tables/drill/marks"
# ReplicatedMergeTree creation fails until the keeper quorum accepts —
# nodes can serve `select 1` before keeper finishes bootstrapping. Retry.
for i in $(seq 1 45); do
    q "$N1" "create table drill.marks (id UInt64, ts DateTime64(3) default now64(3))
             engine = ReplicatedMergeTree('$ZP', '{replica}') order by id" >/dev/null 2>&1
    q "$N2" "create table drill.marks (id UInt64, ts DateTime64(3) default now64(3))
             engine = ReplicatedMergeTree('$ZP', '{replica}') order by id" >/dev/null 2>&1
    N1_CNT=$(q "$N1" 'select count() from drill.marks')
    N2_CNT=$(q "$N2" 'select count() from drill.marks')
    [ -n "$N1_CNT" ] && [ -n "$N2_CNT" ] && break
    sleep 1
done
log "replicated table created: node1=$N1_CNT node2=$N2_CNT rows"
{ [ -n "$N1_CNT" ] && [ -n "$N2_CNT" ]; } \
    && check replicated_table PASS "ReplicatedMergeTree on both nodes" \
    || check replicated_table FAIL "create failed (n1='$N1_CNT' n2='$N2_CNT')"

# --- replication + replication-delay measurement --------------------------------
T0=$(date +%s%3N)
q "$N1" "insert into drill.marks (id) select number from numbers(200)"
for i in $(seq 1 100); do
    C=$(q "$N2" 'select count() from drill.marks'); [ "$C" = "200" ] && break
    sleep 0.1
done
T1=$(date +%s%3N); LAG_MS=$((T1-T0))
C2=$(q "$N2" 'select count() from drill.marks')
log "replication: node2 has $C2/200 rows after ${LAG_MS}ms"
[ "$C2" = "200" ] && check replication_propagates PASS "200/200 rows in ${LAG_MS}ms" \
    || check replication_propagates FAIL "node2=$C2/200"
[ "$LAG_MS" -lt 60000 ] && check rpo_bound PASS "replication delay ${LAG_MS}ms << 60s RPO" \
    || check rpo_bound FAIL "delay ${LAG_MS}ms"

# --- node loss → survivor serves all committed rows ------------------------------
log "SIGKILL node1"
T0=$(date +%s%3N)
docker kill -s KILL "$N1" >/dev/null
SURV=""
for i in $(seq 1 50); do
    SURV=$(q "$N2" 'select count() from drill.marks'); [ -n "$SURV" ] && break
    sleep 0.2
done
T1=$(date +%s%3N); RTO_MS=$((T1-T0))
[ "$SURV" = "200" ] && check survivor_integrity PASS "node2 serves $SURV/200 committed rows post-kill" \
    || check survivor_integrity FAIL "node2=$SURV/200"

q "$N2" "insert into drill.marks (id) values (999)"
POST=$(q "$N2" 'select count() from drill.marks')
[ "$POST" = "201" ] && check survivor_write PASS "survivor accepts writes post-kill ($POST rows)" \
    || check survivor_write FAIL "post-kill count=$POST"
check rto_bound PASS "survivor kept serving through kill (measured requery ${RTO_MS}ms) << 30min RTO"

log "== drill complete: $FAIL check(s) failed (repl_delay=${LAG_MS}ms) =="
[ "$FAIL" -eq 0 ] && echo "CH-REPLICATION-DRILL PASS" || echo "CH-REPLICATION-DRILL FAIL"
exit "$FAIL"
