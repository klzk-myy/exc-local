#!/usr/bin/env bash
# =============================================================================
# synthetic-order.sh — synthetic end-to-end order probe (submit + cancel)
# against a gateway base URL. Phase-09 Task 9.3.26, spec §19.10.1, §24 #309.
#
# This is THE mandatory blue-green switchover gate: bluegreen.sh invokes it
# immediately before the HAProxy map flip (and throughout the canary window
# via canary-check.sh --synthetic-url). It is also usable standalone:
#
#   synthetic-order.sh --base-url http://order-gateway-green:8080
#   synthetic-order.sh --base-url http://127.0.0.1:8080 --symbol EUR/USD
#
# Probe semantics (fail closed):
#   - POST /api/v1/orders with a zero-dollar synthetic IOC ticket
#     (X-Synthetic-Probe header + "synthetic":true — risk engines may
#     legitimately reject it; rejection still proves the order path is
#     alive end-to-end).
#   - If a 2xx response carries an order id, DELETE it — the cancel path
#     must work too (spec §24 #238: cancels are unconditionally exempt).
#   - PASS: submit 2xx or 4xx (path alive), cancel 2xx or 4xx or skipped.
#   - FAIL: submit 5xx / timeout / connection error (000), or a 2xx submit
#     followed by a 5xx/000 cancel.
#
# Env: SYNTHETIC_TIMEOUT (default 5s per request).
# Exit: 0 = order path alive end-to-end; 1 = dead/broken; 64 = usage.
# =============================================================================
set -euo pipefail

BASE=""; SYMBOL="EUR/USD"; TIMEOUT="${SYNTHETIC_TIMEOUT:-5}"
while [ $# -gt 0 ]; do
    case "$1" in
        --base-url) BASE="$2"; shift ;;
        --symbol)   SYMBOL="$2"; shift ;;
        --timeout)  TIMEOUT="$2"; shift ;;
        -h|--help)  sed -n '2,26p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 64 ;;
    esac
    shift
done
[ -n "$BASE" ] || { echo "--base-url required" >&2; exit 64; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1" >&2; exit 64; }; }
need curl; need python3

log()  { printf '[synthetic %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

# --- submit -----------------------------------------------------------------
resp="$(curl -sS --max-time "$TIMEOUT" -w '\n%{http_code}' -X POST \
    -H 'Content-Type: application/json' \
    -H 'X-Synthetic-Probe: bluegreen-gate' \
    -d "{\"symbol\":\"$SYMBOL\",\"side\":\"BUY\",\"type\":\"LIMIT\",\"qty_units\":1,\"price_ticks\":1,\"tif\":\"IOC\",\"synthetic\":true}" \
    "$BASE/api/v1/orders" 2>&1)" || resp=$'curl-error\n000'
code="$(printf '%s' "$resp" | tail -n1)"
body="$(printf '%s' "$resp" | sed '$d')"
case "$code" in
    2*|4*) log "submit $BASE -> $code (path alive)" ;;
    *)     fail "submit -> $code" ;;
esac

# --- cancel (only when an order id came back) --------------------------------
oid="$(printf '%s' "$body" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
    for k in ("order_id","id","cl_ord_id","orderId"):
        if d.get(k): print(d[k]); break
except Exception:
    pass' 2>/dev/null || true)"

if [ -n "$oid" ]; then
    ccode="$(curl -sS -o /dev/null -w '%{http_code}' --max-time "$TIMEOUT" -X DELETE \
        -H 'X-Synthetic-Probe: bluegreen-gate' \
        "$BASE/api/v1/orders/$oid" 2>/dev/null || echo 000)"
    case "$ccode" in
        2*|4*) log "cancel $oid -> $ccode" ;;
        *)     fail "cancel $oid -> $ccode" ;;
    esac
else
    # 4xx submit means risk/validation refused the ticket before the book
    # saw it — no live order exists to cancel; the path is still proven.
    log "no order id in submit response — cancel leg skipped (submit path proven)"
fi

log "PASS: synthetic end-to-end order probe on $BASE"
exit 0
