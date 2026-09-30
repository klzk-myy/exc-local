#!/usr/bin/env bash
# =============================================================================
# bluegreen_gate_test.sh — evidence suite for the mandatory synthetic e2e
# order gate (Phase-09 Task 9.3.26, spec §19.10.1, §24 #309).
#
# Runs the REAL scripts (synthetic-order.sh, canary-check.sh, bluegreen.sh)
# against a local mock gateway and a stubbed kubectl — no cluster needed:
#
#   1.  standalone probe PASS        (submit 201 + cancel 200)
#   2.  standalone probe PASS        (submit 422 risk-reject — path alive)
#   3.  standalone probe FAIL        (submit 500)
#   4.  standalone probe FAIL        (submit 201, cancel 500)
#   5.  bluegreen gate PASS          (no flip performed — gate-only mode)
#   6.  bluegreen gate FAIL          (probe 500 aborts before flip)
#   7.  bluegreen switch ABORT       (probe fail ⇒ map flip never reached,
#                                     map file untouched)
#   8.  canary-check --require-synthetic without URL ⇒ usage fail
#   9.  deploy: canary window probe fail ⇒ scale-to-0 rollback, exit 1
#  10.  deploy: gates pass ⇒ reaches map_flip, refuses without HAProxy
#       socket (fail closed — never edits the repo map file in place)
#
# Usage: bash deploy/scripts/tests/bluegreen_gate_test.sh   (from repo root)
# Exit: 0 = all assertions passed; 1 = any failed.
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPTS="$(cd "$HERE/.." && pwd)"
TMP="$(mktemp -d)"
trap 'kill $PID_OK $PID_FAIL $PID_REJ $PID_CANF 2>/dev/null; rm -rf "$TMP"' EXIT

PASSED=0; FAILED=0
ok()   { PASSED=$((PASSED+1)); printf 'PASS  %s\n' "$1"; }
bad()  { FAILED=$((FAILED+1)); printf 'FAIL  %s\n' "$1"; }

expect() { # expect <name> <want_rc> <grep-pattern-or-> <cmd...>
    local name="$1" want="$2" pat="$3"; shift 3
    local out rc
    out="$("$@" 2>&1)"; rc=$?
    if [ "$rc" -ne "$want" ]; then
        bad "$name (rc=$rc want=$want)"; printf '%s\n' "$out" | sed 's/^/      /'; return
    fi
    if [ "$pat" != "-" ] && ! grep -q "$pat" <<<"$out"; then
        bad "$name (pattern '$pat' missing)"; printf '%s\n' "$out" | sed 's/^/      /'; return
    fi
    ok "$name"
}

# --- stub kubectl -------------------------------------------------------------
mkdir -p "$TMP/bin"
cat > "$TMP/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "kubectl $*" >> "$KUBECTL_LOG"
case "$*" in
    *"get endpointslices"*) echo "127.0.0.1" ;;
    *"run smoke-"*)         echo "200" ;;
    *"get deployment"*"readyReplicas"*) echo "2" ;;
    *"rollout status"*)     exit 0 ;;
    *"set image"*)          exit 0 ;;
    *"scale"*)              exit 0 ;;
    *"get deploy,hpa"*)     echo "stub"; exit 0 ;;
esac
exit 0
EOF
chmod +x "$TMP/bin/kubectl"
export PATH="$TMP/bin:$PATH"
export KUBECTL_LOG="$TMP/kubectl.log"; : > "$KUBECTL_LOG"
export ACTIVE_COLOR_MAP="$TMP/active_color.map"
printf 'gateway blue\n' > "$ACTIVE_COLOR_MAP"
export HAPROXY_SOCKET="$TMP/no-such.sock"  # map_flip must refuse
export CANARY_WINDOW=2

# --- mock gateways ------------------------------------------------------------
python3 "$HERE/mock_gateway.py" 0 ok         "$TMP/port.ok"    & PID_OK=$!
python3 "$HERE/mock_gateway.py" 0 fail       "$TMP/port.fail"  & PID_FAIL=$!
python3 "$HERE/mock_gateway.py" 0 reject     "$TMP/port.rej"   & PID_REJ=$!
python3 "$HERE/mock_gateway.py" 0 cancelfail "$TMP/port.canf"  & PID_CANF=$!
for f in port.ok port.fail port.rej port.canf; do
    for _ in $(seq 50); do [ -s "$TMP/$f" ] && break; sleep 0.1; done
done
P_OK="http://127.0.0.1:$(cat "$TMP/port.ok")"
P_FAIL="http://127.0.0.1:$(cat "$TMP/port.fail")"
P_REJ="http://127.0.0.1:$(cat "$TMP/port.rej")"
P_CANF="http://127.0.0.1:$(cat "$TMP/port.canf")"
echo "mocks: ok=$P_OK fail=$P_FAIL reject=$P_REJ cancelfail=$P_CANF"

# --- 1-4: standalone probe ----------------------------------------------------
expect "1. standalone probe: submit+cancel pass" 0 "PASS" \
    "$SCRIPTS/synthetic-order.sh" --base-url "$P_OK"
expect "2. standalone probe: risk-reject (4xx) still proves path" 0 "PASS" \
    "$SCRIPTS/synthetic-order.sh" --base-url "$P_REJ"
expect "3. standalone probe: submit 500 fails" 1 "FAIL" \
    "$SCRIPTS/synthetic-order.sh" --base-url "$P_FAIL"
expect "4. standalone probe: cancel 500 fails" 1 "cancel.*500" \
    "$SCRIPTS/synthetic-order.sh" --base-url "$P_CANF"

# --- 5-7: the switchover gate -------------------------------------------------
expect "5. gate --to green: probe passes, no flip" 0 "READY" \
    env SYNTHETIC_URL="$P_OK" "$SCRIPTS/bluegreen.sh" gate --to green
expect "6. gate --to green: probe fails closed" 1 "SYNTHETIC ORDER GATE FAILED" \
    env SYNTHETIC_URL="$P_FAIL" "$SCRIPTS/bluegreen.sh" gate --to green

cp "$ACTIVE_COLOR_MAP" "$TMP/map.before"
expect "7. switch aborts on probe failure" 1 "SYNTHETIC ORDER GATE FAILED" \
    env SYNTHETIC_URL="$P_FAIL" "$SCRIPTS/bluegreen.sh" switch --to green
if cmp -s "$ACTIVE_COLOR_MAP" "$TMP/map.before"; then
    ok "7b. map file untouched after gate abort"
else
    bad "7b. map file CHANGED despite gate failure"
fi

# --- 8: canary-check mandatory flag -------------------------------------------
expect "8. canary-check --require-synthetic without URL is a usage fail" 64 \
    "require-synthetic" \
    "$SCRIPTS/canary-check.sh" --color green --window 1 --require-synthetic

# --- 9-10: deploy pipeline ------------------------------------------------------
# In-window readiness curls the pod endpoint directly (not via the kubectl
# stub) — point it at the ok mock's port so only the synthetic leg differs.
OK_PORT="$(cat "$TMP/port.ok")"
DEAD_PROM="http://127.0.0.1:1"  # instant refuse — error-rate gate self-skips
expect "9. deploy: failing in-window probe rolls back (scale 0) and exits" 1 \
    "DEPLOYMENT_AUTOMATED_ROLLBACK" \
    env SYNTHETIC_URL="$P_FAIL" CANARY_WINDOW=2 EXC_PROBE_PORT="$OK_PORT" \
        PROM_URL="$DEAD_PROM" \
    "$SCRIPTS/bluegreen.sh" deploy --image-tag v9.9.9 --canary-window 2
if grep -q "scale deployment order-gateway-green --replicas=0" "$KUBECTL_LOG"; then
    ok "9b. idle color scaled to 0 on gate failure"
else
    bad "9b. no scale-to-0 rollback in kubectl log"
fi

expect "10. deploy: gates pass, then refuses flip without HAProxy socket" 1 \
    "synthetic order gate passed" \
    env SYNTHETIC_URL="$P_OK" CANARY_WINDOW=2 EXC_PROBE_PORT="$OK_PORT" \
        PROM_URL="$DEAD_PROM" \
    "$SCRIPTS/bluegreen.sh" deploy --image-tag v9.9.9 --canary-window 2

echo
echo "═══════════════════════════════════════════════════════════════════"
printf 'bluegreen gate tests: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
