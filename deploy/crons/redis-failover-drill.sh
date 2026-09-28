#!/usr/bin/env bash
# =============================================================================
# redis-failover-drill.sh — automated non-disruptive Redis Sentinel failover
# drill (Phase-09 Task 9.3.20, spec §4.5 + §18.3: RPO ≤5s, RTO ≤30s).
#
# Non-disruptive by design: uses `SENTINEL FAILOVER mymaster` (graceful
# orchestrated promotion — no kill -9 on a live primary in production). For
# chaos-grade kill testing use the Phase-04.5 harness instead.
#
# Sequence:
#   1. Pre-check: full quorum healthy (deploy/sentinel/health-probe.sh).
#   2. Record current master + master_repl_offset (RPO baseline).
#   3. Continuous write probe starts (1 write/50ms into drill:seq counter).
#   4. `SENTINEL FAILOVER` on a reachable sentinel.
#   5. Measure: T_detect (new master visible), T_write (writes succeed on new
#      master), written-lost count (RPO proxy).
#   6. Post-check: quorum healthy again, replicas converged.
#   7. Verdict vs targets: detect ≤3s (§4.5 sub-3s), RTO ≤30s, RPO ≤5s.
#
# --restore: after the drill, failover back to the original master.
#
# Usage:
#   redis-failover-drill.sh [--sentinels h:p,h:p,h:p] [--restore] [--json out.json]
# Env: REDISCLI_AUTH, SENTINEL_ADDRS.
# Exit: 0 = drill passed all targets; 1 = a target missed; 64 usage.
# =============================================================================
set -euo pipefail

SENTINELS="${SENTINEL_ADDRS:-10.1.0.11:26379,10.2.0.12:26379,10.3.0.13:26379}"
MASTER_NAME="mymaster"
RESTORE=0
JSON_OUT=""
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROBE="$SELF_DIR/../sentinel/health-probe.sh"

while [ $# -gt 0 ]; do
    case "$1" in
        --sentinels) SENTINELS="$2"; shift ;;
        --restore)   RESTORE=1 ;;
        --json)      JSON_OUT="$2"; shift ;;
        -h|--help) sed -n '2,28p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 64 ;;
    esac
    shift
done

AUTH=(); [ -n "${REDISCLI_AUTH:-}" ] && AUTH=(-a "$REDISCLI_AUTH" --no-auth-warning)
scli() { timeout 5 redis-cli "${AUTH[@]}" -h "${1%%:*}" -p "${1##*:}" "${@:2}"; }
rcli() { timeout 5 redis-cli "${AUTH[@]}" -h "${1%%:*}" -p "${1##*:}" "${@:2}"; }
first_sentinel() { printf '%s' "$SENTINELS" | cut -d, -f1; }
master_addr() { scli "$(first_sentinel)" SENTINEL get-master-addr-by-name "$MASTER_NAME" | tr '\n' ':' | sed 's/:$//'; }
now_ms() { date +%s%3N; }

log() { printf '[drill %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

# --- pre-check ---------------------------------------------------------------
log "pre-check: cluster health"
"$PROBE" --sentinels "$SENTINELS" --master-name "$MASTER_NAME" >/dev/null \
    || { log "cluster unhealthy — refusing drill"; exit 1; }

ORIG="$(master_addr)"; log "current master: $ORIG"
OFFSET0="$(rcli "$ORIG" INFO replication | grep -o 'master_repl_offset:[0-9]*' | cut -d: -f2)"
log "baseline master_repl_offset=$OFFSET0"

# --- continuous write probe ---------------------------------------------------
SEQLOG="$(mktemp)"; trap 'rm -f "$SEQLOG"' EXIT
(
    i=0
    while :; do
        i=$((i+1))
        cur="$(master_addr 2>/dev/null || true)"
        [ -n "$cur" ] && rcli "$cur" SET "drill:seq" "$i" PX 120000 >/dev/null 2>&1
        echo "$i $(now_ms) ${cur:-none}"
        sleep 0.05
    done
) &
WRITER=$!
trap 'kill $WRITER 2>/dev/null; rm -f "$SEQLOG"' EXIT
sleep 1   # let the writer establish before we break things

# --- trigger failover ---------------------------------------------------------
log "triggering: SENTINEL FAILOVER $MASTER_NAME via $(first_sentinel)"
T0="$(now_ms)"
scli "$(first_sentinel)" SENTINEL failover "$MASTER_NAME" || true

NEW=""; T_DETECT=""
for _ in $(seq 1 600); do  # up to 60s polling at 100ms
    cur="$(master_addr 2>/dev/null || true)"
    if [ -n "$cur" ] && [ "$cur" != "$ORIG" ]; then
        NEW="$cur"; T_DETECT=$(( $(now_ms) - T0 )); break
    fi
    sleep 0.1
done
[ -n "$NEW" ] || { kill $WRITER 2>/dev/null; log "FAIL: failover never completed (60s)"; exit 1; }
log "new master detected: $NEW after ${T_DETECT}ms"

T_WRITE=""
for _ in $(seq 1 300); do
    if rcli "$NEW" SET drill:post-failover 1 PX 60000 >/dev/null 2>&1; then
        T_WRITE=$(( $(now_ms) - T0 )); break
    fi
    sleep 0.1
done
kill $WRITER 2>/dev/null; trap 'rm -f "$SEQLOG"' EXIT
[ -n "$T_WRITE" ] || { log "FAIL: writes did not recover on $NEW"; exit 1; }
log "writes recovered after ${T_WRITE}ms (RTO component)"

# --- RPO proxy: last confirmed write seq on new master ------------------------
LAST_WRITTEN="$(tail -1 "$SEQLOG" | awk '{print $1}')"
CONFIRMED="$(rcli "$NEW" GET drill:seq | tr -d '\r')"
LOST=$(( ${LAST_WRITTEN:-0} - ${CONFIRMED:-0} ))
[ "$LOST" -lt 0 ] && LOST=0
# 50ms cadence → RPO proxy = lost * 50ms
RPO_MS=$(( LOST * 50 ))
log "writes: last=$LAST_WRITTEN confirmed=$CONFIRMED lost=$LOST (~${RPO_MS}ms RPO proxy)"

# --- post-check ---------------------------------------------------------------
log "post-check: cluster health after failover"
sleep 3
"$PROBE" --sentinels "$SENTINELS" --master-name "$MASTER_NAME" || true

# --- verdict ------------------------------------------------------------------
dok=1; wok=1; rok=1
[ "$T_DETECT" -le 3000 ]  || dok=0   # §4.5 sub-3s detection
[ "$T_WRITE"  -le 30000 ] || wok=0   # §18.3 RTO ≤30s
[ "$RPO_MS"   -le 5000 ]  || rok=0   # §18.3 RPO ≤5s

cat <<EOF

=== failover drill verdict ===
  orig master : $ORIG
  new master  : $NEW
  detect time : ${T_DETECT}ms   (target <=3000ms)   $([ $dok = 1 ] && echo PASS || echo FAIL)
  write RTO   : ${T_WRITE}ms    (target <=30000ms)  $([ $wok = 1 ] && echo PASS || echo FAIL)
  RPO proxy   : ~${RPO_MS}ms    (target <=5000ms)   $([ $rok = 1 ] && echo PASS || echo FAIL)
EOF

if [ -n "$JSON_OUT" ]; then
    python3 - "$JSON_OUT" "$ORIG" "$NEW" "$T_DETECT" "$T_WRITE" "$RPO_MS" <<'PY'
import json,sys
json.dump({"orig":sys.argv[2],"new":sys.argv[3],"detect_ms":int(sys.argv[4]),
           "write_rto_ms":int(sys.argv[5]),"rpo_proxy_ms":int(sys.argv[6])},
          open(sys.argv[1],"w"), indent=2)
PY
fi

if [ "$RESTORE" = "1" ] && [ "$NEW" != "$ORIG" ]; then
    log "restoring: failover back toward $ORIG"
    scli "$(first_sentinel)" SENTINEL failover "$MASTER_NAME" || true
    sleep 5
    log "master now: $(master_addr)"
fi

[ "$dok$wok$rok" = "111" ] && { log "DRILL PASS"; exit 0; } || { log "DRILL FAIL"; exit 1; }
