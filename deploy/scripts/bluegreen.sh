#!/usr/bin/env bash
# =============================================================================
# bluegreen.sh — blue/green deploy of the K8s Go services, gated on the
# HAProxy color map (deploy/haproxy/active_color.map) and the R9 readiness
# contract. Phase-09 Tasks 9.3.3 + 9.3.26, spec §19.2 step 4 + §19.10.
#
# Flow (canonical, spec §19.10.1):
#   1. Determine ACTIVE color from HAProxy's map (authoritative) or the repo
#      copy when --socket is not reachable (staging).
#   2. Deploy the new image tag to the IDLE color Deployment(s); scale it to
#      the replica floor and wait for /health/ready on every pod.
#   3. Canary gate (Task 9.3.26): canary-check.sh probes the idle color for
#      CANARY_WINDOW (default 300s) — /health/ready + the synthetic order
#      probe (MANDATORY, --require-synthetic) + Prometheus error-rate gate.
#      Failure ⇒ DEPLOYMENT_AUTOMATED_ROLLBACK (idle color scaled back to
#      0, exit 1).
#   4. SYNTHETIC SWITCH GATE (mandatory, fail closed): a final standalone
#      synthetic-order.sh submit+cancel probe must pass immediately before
#      the flip. No passing probe ⇒ no map change, ever.
#   5. Switch: `set map <active_color.map> gateway <idle>` on the HAProxy
#      admin socket — zero-drop flip, no reload.
#   6. Hold the old color warm for HOLD_SECONDS (default 1800 = 30min),
#      then scale it to 0 (unless --keep).
#
# Usage:
#   bluegreen.sh deploy  --image-tag v1.2.3 [--services order-gateway,fix-gateway]
#                        [--synthetic-url http://order-gateway-green:8080]
#   bluegreen.sh switch  --to green [--synthetic-url URL]  # gated map flip
#   bluegreen.sh gate    --to green [--synthetic-url URL]  # gate only, no flip
#   bluegreen.sh status                                   # active color + pods
#
# Env:
#   KUBECONFIG, EXC_NS (default exchange), HAPROXY_SOCKET
#   (default /run/haproxy/admin.sock), ACTIVE_COLOR_MAP
#   (default deploy/haproxy/active_color.map relative to repo root),
#   CANARY_WINDOW (300), HOLD_SECONDS (1800), SMOKE_URL (per-pod health base),
#   SYNTHETIC_URL (default http://order-gateway-<target-color>:8080 —
#   in-cluster service DNS; override for LB-host or tunnel execution)
#
# Exit: 0 success; 1 = failed at a gate (traffic NEVER left the old color
# unless --to was reached); 64 = usage.
# =============================================================================
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"
NS="${EXC_NS:-exchange}"
MAP_FILE="${ACTIVE_COLOR_MAP:-$REPO_ROOT/deploy/haproxy/active_color.map}"
SOCK="${HAPROXY_SOCKET:-/run/haproxy/admin.sock}"
CANARY_WINDOW="${CANARY_WINDOW:-300}"
HOLD_SECONDS="${HOLD_SECONDS:-1800}"

log()  { printf '[bluegreen %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die()  { log "ERROR: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }

active_color() {
    # HAProxy admin socket is authoritative when reachable; else the file.
    if [ -S "$SOCK" ] && command -v socat >/dev/null 2>&1; then
        echo "show map $MAP_FILE gateway" | socat "$SOCK" - 2>/dev/null \
            | awk 'NR==1{print $3}' | grep -E '^(blue|green)$' && return 0
    fi
    awk '$1=="gateway"{print $2}' "$MAP_FILE" | grep -E '^(blue|green)$'
}

other_color() { [ "$1" = "blue" ] && echo green || echo blue; }

wait_ready() { # wait_ready <deployment> <timeout_s>
    local dep="$1" timeout="${2:-300}"
    log "waiting for $dep rollout (timeout ${timeout}s)"
    kubectl -n "$NS" rollout status "deployment/$dep" --timeout="${timeout}s"
}

smoke_ready() { # smoke_ready <service> — every endpoint must answer R9 ready
    local svc="$1" eps
    eps="$(kubectl -n "$NS" get endpointslices -l "kubernetes.io/service-name=$svc" \
        -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{"\n"}{end}' 2>/dev/null | sort -u)"
    [ -n "$eps" ] || die "no ready endpoints for $svc"
    local ep
    for ep in $eps; do
        code="$(kubectl -n "$NS" run "smoke-$svc-$ep" --rm -i --restart=Never \
            --image=curlimages/curl:latest --quiet -- \
            -s -o /dev/null -w '%{http_code}' --max-time 5 \
            "http://$ep:8080/health/ready" 2>/dev/null || echo 000)"
        [ "$code" = "200" ] || die "smoke $svc/$ep /health/ready -> $code"
        log "smoke $svc/$ep /health/ready -> 200"
    done
}

map_flip() { # map_flip <color>
    local color="$1"
    if [ -S "$SOCK" ] && command -v socat >/dev/null 2>&1; then
        echo "set map $MAP_FILE gateway $color" | socat "$SOCK" - >/dev/null
        log "HAProxy map flipped -> $color"
    else
        die "HAProxy admin socket unreachable ($SOCK) — refusing to edit the repo file only; run on the LB host"
    fi
    # Persist the repo-side record so the file and runtime state converge.
    if [ -w "$MAP_FILE" ]; then
        sed -i "s/^gateway .*/gateway $color/" "$MAP_FILE"
    fi
}

scale_color() { # scale_color <color> <replicas>
    kubectl -n "$NS" scale deployment "order-gateway-$1" --replicas="$2"
    log "order-gateway-$1 scaled to $2"
}

# synthetic_url_for <color> — the probe's target. Explicit override wins;
# otherwise the in-cluster service DNS name of the color's gateway.
synthetic_url_for() {
    if [ -n "${SYNTHETIC_URL:-}" ]; then
        printf '%s' "$SYNTHETIC_URL"
    else
        printf 'http://order-gateway-%s:8080' "$1"
    fi
}

# synthetic_gate <color> — THE mandatory pre-switch gate (Task 9.3.26 AC:
# "Blue-green switchover gated on passing synthetic end-to-end order
# test"). Runs synthetic-order.sh standalone against the target color.
# Fail closed: a probe failure aborts the caller before any map change.
synthetic_gate() {
    local color="$1" url
    url="$(synthetic_url_for "$color")"
    log "synthetic e2e order gate on $color ($url)"
    if ! "$SELF_DIR/synthetic-order.sh" --base-url "$url"; then
        log "SYNTHETIC ORDER GATE FAILED on $color — refusing to switch (fail closed)"
        return 1
    fi
    log "synthetic order gate passed on $color"
}

# ---------------------------------------------------------------------------
cmd="${1:-}"; shift || true
case "$cmd" in
    status)
        cur="$(active_color)"; log "active color: $cur"
        need kubectl
        kubectl -n "$NS" get deploy,hpa -l app.kubernetes.io/part-of=exchange
        ;;
    deploy)
        TAG=""; SERVICES="order-gateway"
        while [ $# -gt 0 ]; do
            case "$1" in
                --image-tag) TAG="$2"; shift ;;
                --services)  SERVICES="$2"; shift ;;
                --canary-window) CANARY_WINDOW="$2"; shift ;;
                --hold)      HOLD_SECONDS="$2"; shift ;;
                --synthetic-url) SYNTHETIC_URL="$2"; shift ;;
                --keep)      KEEP=1 ;;
                *) die "unknown flag: $1" ;;
            esac
            shift
        done
        [ -n "$TAG" ] || die "--image-tag required"
        need kubectl
        cur="$(active_color)"; nxt="$(other_color "$cur")"
        log "active=$cur idle=$nxt deploying tag=$TAG services=$SERVICES"

        IFS=',' read -ra svcs <<< "$SERVICES"
        for s in "${svcs[@]}"; do
            dep="$s"; [ "$s" = "order-gateway" ] && dep="order-gateway-$nxt"
            log "set image $dep -> registry.internal/exchange/$s:$TAG"
            kubectl -n "$NS" set image "deployment/$dep" "*=registry.internal/exchange/$s:$TAG"
        done
        scale_color "$nxt" 2
        for s in "${svcs[@]}"; do
            dep="$s"; [ "$s" = "order-gateway" ] && dep="order-gateway-$nxt"
            wait_ready "$dep"
        done
        smoke_ready "order-gateway-$nxt"

        log "canary gate: ${CANARY_WINDOW}s on $nxt"
        if ! "$SELF_DIR/canary-check.sh" --color "$nxt" --window "$CANARY_WINDOW" \
                --require-synthetic --synthetic-url "$(synthetic_url_for "$nxt")"; then
            log "CANARY FAILED — DEPLOYMENT_AUTOMATED_ROLLBACK"
            scale_color "$nxt" 0
            exit 1
        fi

        # Mandatory pre-switch gate: the synthetic end-to-end order test
        # must pass immediately before traffic moves. Fail closed — a
        # dead order path on the new color means no flip, no exceptions.
        if ! synthetic_gate "$nxt"; then
            log "SYNTHETIC GATE FAILED — DEPLOYMENT_AUTOMATED_ROLLBACK"
            scale_color "$nxt" 0
            exit 1
        fi

        map_flip "$nxt"
        log "traffic on $nxt; holding $cur for ${HOLD_SECONDS}s"
        sleep "$HOLD_SECONDS"
        if [ "${KEEP:-0}" != "1" ]; then scale_color "$cur" 0; fi
        log "deploy complete: active=$nxt"
        ;;
    switch)
        TO=""
        while [ $# -gt 0 ]; do
            case "$1" in
                --to) TO="$2"; shift ;;
                --synthetic-url) SYNTHETIC_URL="$2"; shift ;;
                *) die "unknown flag: $1" ;;
            esac
            shift
        done
        [ "$TO" = "blue" ] || [ "$TO" = "green" ] || die "--to blue|green required"
        smoke_ready "order-gateway-$TO"
        synthetic_gate "$TO"   # mandatory — no passing probe, no flip
        map_flip "$TO"
        ;;
    gate)
        # Gate-only pre-check (ops dry-run / CI evidence): readiness +
        # synthetic e2e order probe, NO map flip. Exit 0 = switch would
        # be permitted; 1 = it would abort.
        TO=""
        while [ $# -gt 0 ]; do
            case "$1" in
                --to) TO="$2"; shift ;;
                --synthetic-url) SYNTHETIC_URL="$2"; shift ;;
                *) die "unknown flag: $1" ;;
            esac
            shift
        done
        [ "$TO" = "blue" ] || [ "$TO" = "green" ] || die "--to blue|green required"
        smoke_ready "order-gateway-$TO"
        synthetic_gate "$TO"
        log "gate $TO: READY — switchover would proceed"
        ;;
    *) sed -n '25,41p' "$0"; exit 64 ;;
esac
