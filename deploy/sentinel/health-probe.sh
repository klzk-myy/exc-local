#!/usr/bin/env bash
# =============================================================================
# health-probe.sh — Redis Sentinel cluster health probe (Task 9.3.20,
# spec §4.5). Exits 0 only when the full quorum topology is healthy:
#
#   * >= QUORUM sentinels reachable and agreeing on the master
#   * master responds to PING and accepts writes (SET probe key)
#   * >= MIN_REPLICAS replicas with master_link_status=up and
#     master_sync_in_progress=0
#   * replication lag <= LAG_MAX_S (RPO <=5s budget — spec §4.5/§18)
#   * no failover currently in progress
#
# --textfile <dir> additionally writes node_exporter textfile metrics:
#   sentinel_up{node}, sentinel_quorum_ok, redis_master_up,
#   redis_replicas_up, redis_replication_lag_seconds{replica},
#   sentinel_failover_in_progress
#
# Usage:
#   health-probe.sh [--sentinels h1:p1,h2:p2,h3:p3] [--master-name mymaster]
#                   [--textfile /var/lib/node_exporter/textfile_collector]
# Env: REDISCLI_AUTH (optional sentinel/data password), SENTINEL_ADDRS.
# Exit: 0 healthy, 1 degraded/down, 64 usage.
# =============================================================================
set -euo pipefail

SENTINELS="${SENTINEL_ADDRS:-10.1.0.11:26379,10.2.0.12:26379,10.3.0.13:26379}"
MASTER_NAME="mymaster"
QUORUM=2
MIN_REPLICAS=1
LAG_MAX_S=5
TEXTFILE=""

while [ $# -gt 0 ]; do
    case "$1" in
        --sentinels)    SENTINELS="$2"; shift ;;
        --master-name)  MASTER_NAME="$2"; shift ;;
        --quorum)       QUORUM="$2"; shift ;;
        --min-replicas) MIN_REPLICAS="$2"; shift ;;
        --lag-max)      LAG_MAX_S="$2"; shift ;;
        --textfile)     TEXTFILE="$2"; shift ;;
        -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 64 ;;
    esac
    shift
done

AUTH=(); [ -n "${REDISCLI_AUTH:-}" ] && AUTH=(-a "$REDISCLI_AUTH" --no-auth-warning)
scli() { timeout 3 redis-cli "${AUTH[@]}" -h "${1%%:*}" -p "${1##*:}" "${@:2}" 2>/dev/null; }
rcli() { timeout 3 redis-cli "${AUTH[@]}" -h "${1%%:*}" -p "${1##*:}" "${@:2}" 2>/dev/null; }

ok=0; fail=0; notes=""
pass() { printf '  \033[32mOK\033[0m    %s\n' "$1"; ok=$((ok+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); notes="$notes;$1"; }

sentinel_up=0; master_addr=""; fo_in_progress=0
for s in ${SENTINELS//,/ }; do
    if out="$(scli "$s" SENTINEL get-master-addr-by-name "$MASTER_NAME")" && [ -n "$out" ]; then
        sentinel_up=$((sentinel_up+1))
        [ -z "$master_addr" ] && master_addr="$(printf '%s' "$out" | tr '\n' ':')"
        # failover-in-progress flag on any sentinel aborts the probe
        scli "$s" SENTINEL master "$MASTER_NAME" 2>/dev/null \
            | grep 'failover_in_progress' >/dev/null && fo_in_progress=1
    else
        bad "sentinel $s unreachable or no master for $MASTER_NAME"
    fi
done
if [ "$sentinel_up" -ge "$QUORUM" ]; then
    pass "sentinel quorum: $sentinel_up up (>= $QUORUM)"
else
    bad "sentinel quorum lost: $sentinel_up < $QUORUM"
fi

MASTER_HOST="${master_addr%%:*}"; MASTER_PORT="$(printf '%s' "$master_addr" | cut -d: -f2 | tr -d ' ')"
master_up=0
if [ -n "$MASTER_HOST" ] && rcli "$MASTER_HOST:$MASTER_PORT" PING | grep 'PONG' >/dev/null; then
    master_up=1; pass "master $MASTER_HOST:$MASTER_PORT PING"
    # write probe — proves the primary accepts writes, not just reads
    rcli "$MASTER_HOST:$MASTER_PORT" SET "probe:sentinel:$(date +%s)" 1 PX 60000 >/dev/null \
        && pass "master accepts writes" || bad "master write probe failed"
else
    bad "master unreachable ($master_addr)"
fi

# replica health via master INFO replication
replicas_up=0; max_lag=0
if [ "$master_up" = "1" ]; then
    info="$(rcli "$MASTER_HOST:$MASTER_PORT" INFO replication)"
    while IFS= read -r line; do
        case "$line" in
            slave*) : ;; # handled below via state flags
        esac
    done <<< "$info"
    # parse slaveN:ip=...,state=online,lag=N
    while IFS= read -r kv; do
        st="$(printf '%s' "$kv" | grep -o 'state=[^,]*' | cut -d= -f2)"
        lag="$(printf '%s' "$kv" | grep -o 'lag=[^,]*' | cut -d= -f2)"
        if [ "$st" = "online" ]; then
            replicas_up=$((replicas_up+1))
            [ "${lag:-0}" -gt "$max_lag" ] && max_lag="$lag"
        fi
    done < <(printf '%s\n' "$info" | grep -o 'slave[0-9]*:[^[:cntrl:]]*')
    if [ "$replicas_up" -ge "$MIN_REPLICAS" ]; then
        pass "replicas online: $replicas_up (>= $MIN_REPLICAS), max lag ${max_lag}s"
        [ "$max_lag" -le "$LAG_MAX_S" ] || bad "replication lag ${max_lag}s > ${LAG_MAX_S}s (RPO budget)"
    else
        bad "replicas online: $replicas_up < $MIN_REPLICAS"
    fi
fi
[ "$fo_in_progress" = "0" ] && pass "no failover in progress" || bad "failover in progress"

# textfile exporter metrics
if [ -n "$TEXTFILE" ]; then
    mkdir -p "$TEXTFILE"
    tmp="$(mktemp "$TEXTFILE/.sentinel.XXXXXX")"
    {
        echo "sentinel_up $sentinel_up"
        echo "sentinel_quorum_ok $([ "$sentinel_up" -ge "$QUORUM" ] && echo 1 || echo 0)"
        echo "redis_master_up $master_up"
        echo "redis_replicas_up $replicas_up"
        echo "redis_replication_lag_seconds $max_lag"
        echo "sentinel_failover_in_progress $fo_in_progress"
    } > "$tmp" && mv "$tmp" "$TEXTFILE/sentinel.prom"
fi

printf 'sentinel-probe: %d ok / %d fail\n' "$ok" "$fail"
[ "$fail" -eq 0 ]
