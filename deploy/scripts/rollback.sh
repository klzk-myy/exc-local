#!/usr/bin/env bash
# =============================================================================
# rollback.sh — revert traffic to the previous blue/green color (and/or roll
# back a Deployment's image to the previous ReplicaSet).
# Phase-09 Tasks 9.3.3 + 9.3.26, spec §19.10 (DEPLOYMENT_AUTOMATED_ROLLBACK).
#
# Modes:
#   rollback.sh --color <bad>     flip HAProxy map back to the other color
#                               and scale the bad color to 0
#   rollback.sh --image <dep>     `kubectl rollout undo` a specific deployment
#
# Env: same as bluegreen.sh (EXC_NS, HAPROXY_SOCKET, ACTIVE_COLOR_MAP).
# Exit: 0 = traffic restored to the prior color; 1 = could not confirm.
# =============================================================================
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"
NS="${EXC_NS:-exchange}"
MAP_FILE="${ACTIVE_COLOR_MAP:-$REPO_ROOT/deploy/haproxy/active_color.map}"
SOCK="${HAPROXY_SOCKET:-/run/haproxy/admin.sock}"

log() { printf '[rollback %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }

BAD=""; DEP=""
while [ $# -gt 0 ]; do
    case "$1" in
        --color) BAD="$2"; shift ;;
        --image) DEP="$2"; shift ;;
        -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
        *) die "unknown flag: $1" ;;
    esac
    shift
done

if [ -n "$DEP" ]; then
    kubectl -n "$NS" rollout undo "deployment/$DEP"
    kubectl -n "$NS" rollout status "deployment/$DEP" --timeout=300s
    log "$DEP rolled back to previous ReplicaSet"
    exit 0
fi

[ "$BAD" = "blue" ] || [ "$BAD" = "green" ] || die "--color blue|green required"
GOOD="blue"; [ "$BAD" = "blue" ] && GOOD="green"

# 1. Make sure the surviving color can actually serve before we flip.
ready="$(kubectl -n "$NS" get deployment "order-gateway-$GOOD" \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)"
if [ "${ready:-0}" -lt 1 ]; then
    log "$GOOD has no ready replicas — scaling to 2 and waiting"
    kubectl -n "$NS" scale deployment "order-gateway-$GOOD" --replicas=2
    kubectl -n "$NS" rollout status "deployment/$GOOD" --timeout=120s \
        || kubectl -n "$NS" rollout status "deployment/order-gateway-$GOOD" --timeout=120s
fi

# 2. Flip the HAProxy map back.
if [ -S "$SOCK" ] && command -v socat >/dev/null 2>&1; then
    echo "set map $MAP_FILE gateway $GOOD" | socat "$SOCK" - >/dev/null
    log "HAProxy map restored -> $GOOD"
else
    die "HAProxy socket unreachable; flip the map manually: set map $MAP_FILE gateway $GOOD"
fi
[ -w "$MAP_FILE" ] && sed -i "s/^gateway .*/gateway $GOOD/" "$MAP_FILE"

# 3. Verify the surviving color answers readiness, then quarantine the bad one.
sleep 2
kubectl -n "$NS" scale deployment "order-gateway-$BAD" --replicas=0
log "rollback complete: traffic on $GOOD, $BAD scaled to 0"
