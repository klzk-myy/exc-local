#!/usr/bin/env bash
# =============================================================================
# kind_hpa_drill.sh — Phase-09 :63 HPA-on-live-cluster proof (Task 9.3.29,
# spec §19.13.1). The production HPA row reads "HPA scales on CPU > 70%";
# this drill makes it observable:
#
#   1. provisions a throwaway kind cluster + metrics-server
#      (--kubelet-insecure-tls patch — kind's kubelet serves a self-signed
#      cert metrics-server cannot verify)
#   2. builds ONE representative service image locally — settlement-service,
#      the lightest §19.13.1 HPA'd daemon that builds CGO_ENABLED=0 today —
#      and substitutes it for the registry.internal/...:CHANGE_ME placeholder
#      via deploy/k8s/kind-overlay/ (kustomize; production Deployment/Service/
#      HPA/PDB objects are applied verbatim)
#   3. drives CPU past the 70%-of-request target with an in-pod busy loop and
#      watches the autoscaling/v2 HPA rescale 2 -> maxReplicas
#   4. removes load and reports scale-down posture honestly: stock kubernetes
#      (a) holds the 300s default stabilization window and (b) vetoes
#      scale-down while the custom Pods metric settlement_queue_depth is
#      unfetchable (no custom.metrics.k8s.io adapter in kind). With
#      OBSERVE_SCALE_DOWN=1 the drill temporarily drops that metric from the
#      live HPA — noted in output, restored afterwards — to prove the
#      controller does scale the CPU-only manifest back down.
#
# Env: KIND_CLUSTER (exc-hpa-drill) · KIND_VERSION (v0.27.0) ·
#      KUBECTL_VERSION (stable lookup) · TOOLS_DIR (/tmp/hpa-drill-bin) ·
#      IMAGE (exchange/settlement-service:kind-drill) ·
#      SCALE_UP_TIMEOUT (300s) · SCALE_DOWN_TIMEOUT (420s) ·
#      OBSERVE_SCALE_DOWN (1) · KEEP_CLUSTER (0).
# Exit: 0 all checks pass · 1 a check failed. Cluster is deleted on exit
# unless KEEP_CLUSTER=1.
# =============================================================================
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
OVERLAY="$REPO_ROOT/deploy/k8s/kind-overlay"

KIND_CLUSTER="${KIND_CLUSTER:-exc-hpa-drill}"
KIND_VERSION="${KIND_VERSION:-v0.27.0}"
TOOLS_DIR="${TOOLS_DIR:-/tmp/hpa-drill-bin}"
IMAGE="${IMAGE:-exchange/settlement-service:kind-drill}"
NS=exchange
DEPLOY=settlement-service
SCALE_UP_TIMEOUT="${SCALE_UP_TIMEOUT:-300}"
SCALE_DOWN_TIMEOUT="${SCALE_DOWN_TIMEOUT:-420}"
OBSERVE_SCALE_DOWN="${OBSERVE_SCALE_DOWN:-1}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
METRICS_SERVER_URL="${METRICS_SERVER_URL:-https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml}"
FAILS=0
LOAD_PIDS=""

log()  { printf '[hpa-drill %s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*"; }
check() {
    if [ "$2" = "PASS" ]; then log "  check $1: PASS — $3"
    else log "  check $1: FAIL — $3"; FAILS=$((FAILS+1)); fi
}

cleanup() {
    for p in $LOAD_PIDS; do kill "$p" 2>/dev/null || true; done
    # orphaned in-pod busy loops die with the cluster; for KEEP_CLUSTER runs
    # sweep them so the namespace is left idle.
    if [ "$KEEP_CLUSTER" = "1" ] && command -v "$KUBECTL" >/dev/null 2>&1; then
        for pod in $("$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name="$DEPLOY" \
                     -o name 2>/dev/null); do
            "$KUBECTL" -n "$NS" exec "$pod" -- pkill -f 'while' >/dev/null 2>&1 || true
        done
    fi
    if [ "$KEEP_CLUSTER" != "1" ] && [ -x "$KIND" ]; then
        "$KIND" delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# --- toolchain ---------------------------------------------------------------
mkdir -p "$TOOLS_DIR"
KIND="${KIND_BIN:-$TOOLS_DIR/kind}"
KUBECTL="${KUBECTL_BIN:-$TOOLS_DIR/kubectl}"

if ! command -v "$KIND" >/dev/null 2>&1; then
    log "fetching kind $KIND_VERSION"
    curl -sSL -o "$KIND" "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
    chmod +x "$KIND"
fi
if ! command -v "$KUBECTL" >/dev/null 2>&1; then
    KV="${KUBECTL_VERSION:-$(curl -sSL https://dl.k8s.io/release/stable.txt)}"
    log "fetching kubectl $KV"
    curl -sSL -o "$KUBECTL" "https://dl.k8s.io/release/${KV}/bin/linux/amd64/kubectl"
    chmod +x "$KUBECTL"
fi
export PATH="$TOOLS_DIR:$PATH"

log "== kind HPA drill: $DEPLOY CPU>70% scale-out on a real cluster =="

# --- 1. cluster + metrics-server ----------------------------------------------
if ! "$KIND" get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
    log "creating kind cluster $KIND_CLUSTER"
    "$KIND" create cluster --name "$KIND_CLUSTER" --wait 120s >/dev/null
else
    log "reusing existing kind cluster $KIND_CLUSTER"
fi
"$KUBECTL" cluster-info --context "kind-$KIND_CLUSTER" >/dev/null

"$KUBECTL" apply -f "$METRICS_SERVER_URL" >/dev/null
"$KUBECTL" -n kube-system patch deployment metrics-server --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' \
    >/dev/null
"$KUBECTL" -n kube-system rollout status deployment/metrics-server --timeout=120s >/dev/null
for i in $(seq 1 30); do
    "$KUBECTL" top nodes >/dev/null 2>&1 && break; sleep 5
done
"$KUBECTL" top nodes >/dev/null 2>&1 \
    && check metrics_server PASS "kubectl top nodes returns data" \
    || check metrics_server FAIL "metrics.k8s.io not serving after patch"

# --- 2. local image for the CHANGE_ME placeholder -----------------------------
log "building cmd/settlement (CGO_ENABLED=0) -> kind image $IMAGE"
mkdir -p "$OVERLAY/bin"
(cd "$REPO_ROOT/services" && CGO_ENABLED=0 go build -o "$OVERLAY/bin/settlement" ./cmd/settlement)
# buildx lock perm quirks on some dev hosts: retry through the legacy builder
# with a scratch DOCKER_CONFIG before giving up.
if ! docker build -t "$IMAGE" "$OVERLAY" >/dev/null 2>&1; then
    DOCKER_CONFIG="$(mktemp -d)" DOCKER_BUILDKIT=0 docker build -t "$IMAGE" "$OVERLAY" >/dev/null
fi
"$KIND" load docker-image "$IMAGE" --name "$KIND_CLUSTER" >/dev/null
check image_build PASS "local image $IMAGE loaded into kind (substitutes :CHANGE_ME)"

# --- 3. apply production manifests via the overlay -----------------------------
"$KUBECTL" kustomize "$OVERLAY" --load-restrictor=LoadRestrictionsNone \
    | "$KUBECTL" apply -f - >/dev/null
"$KUBECTL" -n "$NS" rollout status "deployment/$DEPLOY" --timeout=180s >/dev/null
READY=$("$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name="$DEPLOY" \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[*].status.containerStatuses[0].ready}' 2>/dev/null \
        | tr ' ' '\n' | grep -c true || true)
[ "$READY" -ge 2 ] \
    && check pods_ready PASS "$READY pods Ready (R9 probes green)" \
    || check pods_ready FAIL "only $READY pods Ready"

# wait for the CPU metric to populate (metrics-server scrape cadence ~60s)
log "waiting for HPA cpu target to populate"
for i in $(seq 1 40); do
    T=$("$KUBECTL" -n "$NS" get hpa "$DEPLOY" -o jsonpath='{.status.currentMetrics[0].resource.current.averageUtilization}' 2>/dev/null || true)
    [ -n "$T" ] && break; sleep 5
done
[ -n "${T:-}" ] \
    && check hpa_metric_live PASS "cpu utilization reporting (${T}%/70%)" \
    || check hpa_metric_live FAIL "cpu target still <unknown>"
"$KUBECTL" -n "$NS" get hpa "$DEPLOY"

# --- 4. drive CPU, watch the rescale -------------------------------------------
log "starting in-pod CPU busy loops (limit=1 core, request=250m -> ~400% util)"
for pod in $("$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name="$DEPLOY" -o name); do
    "$KUBECTL" -n "$NS" exec "$pod" -- sh -c 'while :; do :; done' >/dev/null 2>&1 &
    LOAD_PIDS="$LOAD_PIDS $!"
done

T0=$(date +%s)
MAX_SEEN=2
while :; do
    R=$("$KUBECTL" -n "$NS" get hpa "$DEPLOY" -o jsonpath='{.status.currentReplicas}' 2>/dev/null || echo 0)
    [ "${R:-0}" -gt "$MAX_SEEN" ] && MAX_SEEN=$R
    [ "${R:-0}" -gt 2 ] && break
    [ $(( $(date +%s) - T0 )) -gt "$SCALE_UP_TIMEOUT" ] && break
    sleep 10
done
log "post-load state:"
"$KUBECTL" -n "$NS" get hpa "$DEPLOY"
"$KUBECTL" -n "$NS" top pods 2>/dev/null | head -12 || true

[ "$MAX_SEEN" -gt 2 ] \
    && check scale_up PASS "HPA rescaled 2 -> ${R} replicas on cpu>70% (cap=10)" \
    || check scale_up FAIL "replicas never left minReplicas=2"

# --- 5. drop load, report scale-down posture ------------------------------------
for p in $LOAD_PIDS; do kill "$p" 2>/dev/null || true; done
for pod in $("$KUBECTL" -n "$NS" get pods -l app.kubernetes.io/name="$DEPLOY" -o name); do
    "$KUBECTL" -n "$NS" exec "$pod" -- pkill -f 'while' >/dev/null 2>&1 || true
done
LOAD_PIDS=""
log "load removed — default scaleDown stabilization is 300s, and the"
log "custom Pods metric settlement_queue_depth is unfetchable without a"
log "custom.metrics.k8s.io adapter, which vetoes downscale on stock k8s."

if [ "$OBSERVE_SCALE_DOWN" = "1" ]; then
    log "temporarily dropping the custom metric from the live HPA to observe"
    log "the controller's scale-down path (full manifest restored afterwards)"
    "$KUBECTL" -n "$NS" patch hpa "$DEPLOY" --type=json \
        -p='[{"op":"remove","path":"/spec/metrics/1"}]' >/dev/null
    T1=$(date +%s)
    DOWN_SEEN=-1
    while :; do
        R=$("$KUBECTL" -n "$NS" get hpa "$DEPLOY" -o jsonpath='{.status.currentReplicas}' 2>/dev/null || echo 0)
        if [ "${R:-0}" -lt "$MAX_SEEN" ]; then DOWN_SEEN=$R; break; fi
        [ $(( $(date +%s) - T1 )) -gt "$SCALE_DOWN_TIMEOUT" ] && break
        sleep 15
    done
    "$KUBECTL" -n "$NS" get hpa "$DEPLOY"
    [ "$DOWN_SEEN" -ge 0 ] \
        && check scale_down PASS "replicas ${MAX_SEEN} -> ${DOWN_SEEN} after load+300s stabilization" \
        || check scale_down FAIL "no downscale within ${SCALE_DOWN_TIMEOUT}s (stabilization 300s)"
    # restore the production metric set verbatim
    "$KUBECTL" kustomize "$OVERLAY" --load-restrictor=LoadRestrictionsNone \
        | "$KUBECTL" apply -f - >/dev/null
    log "full HPA metric set (cpu + settlement_queue_depth) re-applied"
fi

"$KUBECTL" -n "$NS" describe hpa "$DEPLOY" | tail -15 || true

log "== drill complete: $FAILS check(s) failed =="
[ "$FAILS" -eq 0 ] && { echo "KIND-HPA-DRILL PASS"; exit 0; }
echo "KIND-HPA-DRILL FAIL ($FAILS)" >&2
exit 1
