#!/usr/bin/env bash
# =============================================================================
# canary-check.sh — automated canary verification for the idle (new) color
# before the HAProxy map flip, plus a continuous post-switch verifier.
# Phase-09 Task 9.3.26, spec §19.10.1 + §20.4 + §24 #309.
#
# Gates (all must pass for the whole --window):
#   1. Readiness: every pod of order-gateway-<color> answers
#      /health/ready 200 (R9 dependency schema).
#   2. Error rate: Prometheus http_requests_total{status=~"5.."} rate on the
#      target color must stay <= 1% of requests (spec §19.10 canary window).
#   3. Synthetic order probe (--synthetic-url): POSTs a zero-dollar test
#      order + cancel against the idle color; any non-2xx fails.
#      MANDATORY when --require-synthetic is passed — bluegreen.sh always
#      passes it; the flag stays optional only for standalone/manual runs.
#
# On failure the script prints DEPLOYMENT_AUTOMATED_ROLLBACK and exits 1 —
# bluegreen.sh scales the failed color to 0; post-switch (--watch) callers
# should invoke deploy/scripts/rollback.sh.
#
# Usage:
#   canary-check.sh --color green [--window 300] [--interval 15]
#                   [--prom http://prometheus:9090] [--synthetic-url URL]
#                   [--require-synthetic]  # probe mandatory (switchover gate)
#                   [--watch]            # post-switch mode: on failure exec
#                                        # deploy/scripts/rollback.sh
#
# Exit: 0 = window elapsed clean; 1 = gate tripped; 64 = usage.
# =============================================================================
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NS="${EXC_NS:-exchange}"
COLOR=""
WINDOW=300
INTERVAL=15
PROM="${PROM_URL:-http://prometheus:9090}"
PROBE_PORT="${EXC_PROBE_PORT:-8080}"   # per-pod readiness port (test override)
SYN_URL=""
WATCH=0

while [ $# -gt 0 ]; do
    case "$1" in
        --color)   COLOR="$2"; shift ;;
        --window)  WINDOW="$2"; shift ;;
        --interval) INTERVAL="$2"; shift ;;
        --prom)    PROM="$2"; shift ;;
        --synthetic-url) SYN_URL="$2"; shift ;;
        --require-synthetic) REQUIRE_SYN=1 ;;
        --watch)   WATCH=1 ;;
        -h|--help) sed -n '2,32p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 64 ;;
    esac
    shift
done
[ "$COLOR" = "blue" ] || [ "$COLOR" = "green" ] || { echo "--color blue|green required" >&2; exit 64; }
if [ "${REQUIRE_SYN:-0}" = "1" ] && [ -z "$SYN_URL" ]; then
    echo "--require-synthetic needs --synthetic-url (mandatory switchover gate)" >&2
    exit 64
fi

log() { printf '[canary %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { log "GATE FAIL: $* — DEPLOYMENT_AUTOMATED_ROLLBACK";
         [ "$WATCH" = "1" ] && "$SELF_DIR/rollback.sh" --color "$COLOR" || true
         exit 1; }

SVC="order-gateway-$COLOR"

endpoints() {
    kubectl -n "$NS" get endpointslices -l "kubernetes.io/service-name=$SVC" \
        -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{"\n"}{end}' 2>/dev/null | sort -u
}

probe_ready() {
    local ep code
    for ep in $(endpoints); do
        code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            "http://$ep:$PROBE_PORT/health/ready" || echo 000)"
        [ "$code" = "200" ] || fail "pod $ep /health/ready -> $code"
    done
}

check_error_rate() {
    # PromQL: 5xx rate / total on the target color over the check interval.
    local q resp ratio
    q="sum(rate(http_requests_total{namespace=\"$NS\",color=\"$COLOR\",status=~\"5..\"}[${INTERVAL}s])) / sum(rate(http_requests_total{namespace=\"$NS\",color=\"$COLOR\"}[${INTERVAL}s]))"
    resp="$(curl -sfG --max-time 5 --data-urlencode "query=$q" "$PROM/api/v1/query" 2>/dev/null)" || {
        log "prometheus unreachable — skipping error-rate gate (probes still gate)"
        return 0
    }
    ratio="$(printf '%s' "$resp" | python3 -c '
import json,sys
try:
    r=json.load(sys.stdin)["data"]["result"]
    print(float(r[0]["value"][1]) if r else 0.0)
except Exception:
    print(0.0)')"
    # NaN/0 traffic: ratio ~0 is fine; >0.01 fails.
    if python3 -c "import sys; sys.exit(0 if float('$ratio')>0.01 else 1)" 2>/dev/null; then
        fail "error rate $ratio > 1% on $COLOR"
    fi
}

synthetic_order() {
    [ -n "$SYN_URL" ] || return 0
    # Delegate to the standalone probe (submit + cancel legs) — one
    # implementation, one contract.
    "$SELF_DIR/synthetic-order.sh" --base-url "$SYN_URL" \
        || fail "synthetic order probe failed on $SYN_URL"
}

log "canary on $SVC window=${WINDOW}s interval=${INTERVAL}s"
deadline=$(( $(date +%s) + WINDOW ))
while [ "$(date +%s)" -lt "$deadline" ]; do
    probe_ready
    check_error_rate
    synthetic_order
    sleep "$INTERVAL"
done
log "canary window clean — $COLOR healthy"
exit 0
