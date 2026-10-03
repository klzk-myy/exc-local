#!/usr/bin/env bash
# =============================================================================
# dev_stack.sh — interactive lifecycle for the exc.local local development stack
#
# Docker-only. There is NO bare-metal mode: every daemon runs as a container
# (docker-compose.app.yml), supervised by compose restart/healthchecks. The
# legacy pid-file/nohup host tier was removed 2026-10-03 with the full docker
# migration — `baremetal_sweep` below fails status/stop if a stack binary is
# found running outside a container.
#
#   Stage 0  clustered infrastructure — docker-compose.dev.yml
#            PostgreSQL 16 · Redis 1 primary + 2 replicas + 3 sentinels + cache
#            · NATS JetStream ×3 · ClickHouse · Trino
#
#   Stage 1-5  application daemons — docker-compose.app.yml, started in the
#            deterministic bootstrap order of spec §19.13.2 (Tier 1 IPC →
#            Tier 2 State → Tier 3 Core Engines → Tier 4 Services →
#            Tier 5 Gateways):
#              aeronmd · watchdogd · matching-engine · bridge · oracle
#              · marketdata · risk · settlement · gateway · compliance
#              · analytics · recovery-orchestrator · sentinel_exporter
#              · admin · fix
#
# Engine + aeronmd containers use host IPC + host networking + /dev/shm +
# WAL volumes — 127.0.0.1 inside a container IS the host, so host-mapped
# addresses behave identically to native (still needs isolcpus/NUMA on the
# host for determinism — containers add jitter, so soak/benchmark numbers
# taken here are NOT production evidence). Only PTP stays host-side —
# docker mode uses the software-clock fallback per §19.13.4.
#
# Images are built from deploy/docker/Dockerfile.* via `dev_stack.sh build`.
#
# Usage:
#   deploy/scripts/dev_stack.sh                 # interactive menu
#   deploy/scripts/dev_stack.sh start all       # infra + every app daemon
#   deploy/scripts/dev_stack.sh start core      # infra + trading-path daemons
#   deploy/scripts/dev_stack.sh start infra     # Stage 0 only
#   deploy/scripts/dev_stack.sh start app       # app daemons only (infra assumed up)
#   deploy/scripts/dev_stack.sh stop all        # app daemons + infra
#   deploy/scripts/dev_stack.sh stop app        # app daemons only
#   deploy/scripts/dev_stack.sh restart         # stop all + verify, then start all
#   deploy/scripts/dev_stack.sh status
#   deploy/scripts/dev_stack.sh logs <daemon>   # tail one daemon's log
#   deploy/scripts/dev_stack.sh build           # (re)build the docker images
#
# Env overrides:
#   EXC_DEV_RUN_DIR   runtime state root      (default /tmp/exc-dev-stack)
#   EXC_WAL_DIR       engine WAL directory    (default $RUN_DIR/wal)
#   EXC_SHM_BASE      shm ring namespace      (default exchange_ipc)
#   EXC_REDIS_ADDR    coordination Redis      (default 127.0.0.1:16379)
#   EXC_DEV_QUIET=1   suppress the verbose trace
#   EXC_APP_COMPOSE=path        app compose file (default docker-compose.app.yml)
#   EXC_ENGINE_IMAGE / EXC_GO_IMAGE / EXC_AERON_IMAGE / EXC_FRONTEND_IMAGE
#                     image tags (default exc-*:local)
# =============================================================================
set -uo pipefail

# ── paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/../.." && pwd)"

# ── configuration ────────────────────────────────────────────────────────────
# dev.env is sourced FIRST so stack-identity vars it may pin
# (EXC_DEV_RUN_DIR, EXC_SHM_BASE, EXC_AERON_DIR, EXC_WAL_DIR) actually
# resolve below — previously load_env ran inside the actions, so dev.env
# could never influence run-dir/shm selection and `stop` against the
# shared defaults silently no-oped on a per-user stack.
DEV_ENV="$REPO/deploy/dev.env"
if [[ -f "$DEV_ENV" ]]; then
  set -a; . "$DEV_ENV"; set +a
elif [[ -f "$DEV_ENV.example" ]]; then
  set -a; . "$DEV_ENV.example"; set +a
fi
RUN_DIR="${EXC_DEV_RUN_DIR:-/tmp/exc-dev-stack}"
LOG_DIR="$RUN_DIR/logs"
WAL_DIR="${EXC_WAL_DIR:-$RUN_DIR/wal}"
SPOOL_DIR="$RUN_DIR/ch-spool"
SHM_BASE="${EXC_SHM_BASE:-exchange_ipc}"
REDIS_ADDR="${EXC_REDIS_ADDR:-127.0.0.1:16379}"
# Docker-mode Aeron dir is pinned by docker-compose.app.yml (--aeron-dir);
# status checks look there, not at the legacy host default.
AERON_DIR="/dev/shm/aeron-exchange"

COMPOSE_FILE="$REPO/docker-compose.dev.yml"
COMPOSE="docker compose -f $COMPOSE_FILE"
APP_COMPOSE_FILE="${EXC_APP_COMPOSE:-$REPO/docker-compose.app.yml}"
# Both files share project `exc-dev`: app services join the infra network and
# resolve redis-primary/nats-1/postgres by service name (fixed 2026-10-02 —
# app-only `-f` broke depends_on; the combined model is the supported path).
APP_COMPOSE="docker compose -f $COMPOSE_FILE -f $APP_COMPOSE_FILE"
ENGINE_DOCKER_IMAGE="${EXC_ENGINE_IMAGE:-exc-matching-engine:local}"
GO_DOCKER_IMAGE="${EXC_GO_IMAGE:-exc-go-service:local}"
AERON_DOCKER_IMAGE="${EXC_AERON_IMAGE:-exc-aeronmd:local}"
FRONTEND_DOCKER_IMAGE="${EXC_FRONTEND_IMAGE:-exc-frontend:local}"

START_GRACE=3          # seconds to wait before declaring a container "up"
VLOG=1
[[ "${EXC_DEV_QUIET:-0}" == "1" ]] && VLOG=0

# ── daemon inventory ─────────────────────────────────────────────────────────
# name|stage|group|port|healthpath
#   group = core → trading path (engine, market data, risk, settlement, gateway)
#           ext  → supporting services (compliance, analytics, admin, fix, …)
#   port  = HTTP bind port (0/- = none) — used for the conflict preflight
#   health= path served on that port (empty = no HTTP health surface)
# Every name maps to a docker-compose.app.yml service via docker_svc().
DAEMONS=(
  "aeronmd|20|core|-|"
  "watchdogd|20|core|9110|"
  "matching-engine|30|core|-|"
  "matching-engine-1|30|core|-|"
  "matching-engine-2|30|core|-|"
  "matching-engine-3|30|core|-|"
  "matching-engine-4|30|core|-|"
  "matching-engine-5|30|core|-|"
  "matching-engine-6|30|core|-|"
  "matching-engine-7|30|core|-|"
  "xshardrelay|30|core|-|"
  "bridge|40|core|9100|/healthz"
  "oracle|40|core|8090|/health/ready"
  "marketdata|40|core|8081|/healthz"
  "risk|50|core|8091|/health/ready"
  "settlement|50|core|8083|/healthz"
  "compliance|50|ext|8084|/healthz"
  "analytics|50|ext|-|"
  "recovery-orchestrator|50|ext|-|"
  "sentinel_exporter|50|ext|-|"
  "admin|60|ext|8085|/health"
  "gateway|60|core|8080|/health"
  "fix|60|ext|8082|"
  "fixsbe|60|ext|8089|"
  "frontend|60|ext|3000|/"
)

# ── container guard ──────────────────────────────────────────────────────────
# Inside a container, /proc and ss see container-local state. Compose calls
# still work when the host docker socket is mounted — so the script is
# usable there, but warn that baremetal_sweep/status see this container's
# namespace, not the host's.
in_container() { [[ -f /.dockerenv ]] || grep -qaE 'docker|kubepods' /proc/1/cgroup 2>/dev/null; }
ensure_docker_cli() {
  command -v docker >/dev/null 2>&1 || die "dev stack needs the docker CLI (in a container, mount the host socket: -v /var/run/docker.sock:/var/run/docker.sock)"
}

# ── pretty output ────────────────────────────────────────────────────────────
C_R=$'\033[31m'; C_G=$'\033[32m'; C_Y=$'\033[33m'; C_B=$'\033[36m'; C_D=$'\033[2m'; C_0=$'\033[0m'
# plain output when not on a TTY (so piping to a log file stays clean)
[[ -t 1 ]] || { C_R=""; C_G=""; C_Y=""; C_B=""; C_D=""; C_0=""; }
ts()   { date +%H:%M:%S; }
vlog() { [[ $VLOG -eq 1 ]] && printf "${C_D}[%s]${C_0} %s\n" "$(ts)" "$*"; return 0; }
info() { printf "${C_B}▸${C_0} %s\n" "$*"; }
ok()   { printf "  ${C_G}✓${C_0} %s\n" "$*"; }
warn() { printf "  ${C_Y}!${C_0} %s\n" "$*"; }
err()  { printf "  ${C_R}✗${C_0} %s\n" "$*"; }
hdr()  { printf "\n${C_B}══ %s ══${C_0}\n" "$*"; }
die()  { err "$*"; exit 1; }

cell() { # colored-text [width] — pad AFTER %b so ANSI escapes don't skew columns
  local s="$1" w="${2:-13}" plain
  plain="$(printf "%b" "$s" | sed $'s/\033\[[0-9;]*m//g')"
  printf "%b%*s" "$s" $((w - ${#plain})) ""
}

# ── small helpers ────────────────────────────────────────────────────────────
daemon_field() { # name field-index(2=stage,3=group,4=port,5=health)
  local n="$1" f="$2" row
  for row in "${DAEMONS[@]}"; do
    [[ "${row%%|*}" == "$n" ]] && { echo "$row" | cut -d'|' -f"$f"; return 0; }
  done
  echo ""
}
daemon_names() { local g="${1:-}" row; for row in "${DAEMONS[@]}"; do
  [[ -z "$g" || "$(echo "$row" | cut -d'|' -f3)" == "$g" ]] && echo "$row" | cut -d'|' -f1
done; }

docker_svc() { echo "${1//_/-}"; }   # sentinel_exporter → sentinel-exporter

docker_container_running() { # name -> its exc-dev-<svc>-N container is up
  docker ps --filter "name=exc-dev-$(docker_svc "$1")-" \
    --filter status=running -q 2>/dev/null | grep -q .
}
docker_container_state() { # name -> compose container status string ("" if absent)
  docker ps -a --filter "name=exc-dev-$(docker_svc "$1")-" \
    --format '{{.Status}}' 2>/dev/null | head -1
}

# Docker-only policy: a stack binary running OUTSIDE a container is a
# migration violation — it shares /dev/shm and WAL paths with the engine
# containers and silently double-owns shard rings. Detected via exe path
# (readlink lands on the repo, not an overlay) + cgroup filter.
baremetal_sweep() { # -> 1 if any bare-metal stack process found
  local pid exe bad=0
  for pid in /proc/[0-9]*; do
    pid="${pid#/proc/}"
    exe="$(readlink "/proc/$pid/exe" 2>/dev/null)" || continue
    case "$exe" in
      "$REPO/services/bin/"*|"$REPO/core/build/matching_engine"|*"/aeron/bin/aeronmd")
        grep -qaE 'docker|kubepods' "/proc/$pid/cgroup" 2>/dev/null && continue
        warn "bare-metal stack process pid $pid ($exe) — docker-only policy, stop it properly"
        bad=1 ;;
    esac
  done
  return "$bad"
}

# ── environment ──────────────────────────────────────────────────────────────
load_env() {
  mkdir -p "$RUN_DIR" "$LOG_DIR" "$WAL_DIR" "$SPOOL_DIR"
  # dev.env carries JWT key + ClickHouse creds + NATS seeds; it is gitignored,
  # so fall back to the committed example when it has not been created yet.
  if [[ -f "$DEV_ENV" ]]; then
    vlog "sourcing $DEV_ENV"
    set -a; . "$DEV_ENV"; set +a
  elif [[ -f "$DEV_ENV.example" ]]; then
    vlog "dev.env missing — sourcing $DEV_ENV.example"
    set -a; . "$DEV_ENV.example"; set +a
  fi
  export REPO
  export EXC_SHM_BASE="$SHM_BASE"
  export EXC_IPC_BASE="$SHM_BASE"        # gateway must share the engine's ring namespace
  export EXC_WAL_DIR="$WAL_DIR"
  export EXC_REDIS_ADDR="$REDIS_ADDR"
  export EXC_CH_SPOOL_DIR="$SPOOL_DIR"
  export EXC_SENTINEL_ADDRS="${EXC_SENTINEL_ADDRS:-127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381}"
  vlog "shm=$EXC_SHM_BASE wal=$EXC_WAL_DIR redis=$EXC_REDIS_ADDR aeron=$AERON_DIR"
  [[ -f "$APP_COMPOSE_FILE" ]] || warn "$APP_COMPOSE_FILE missing — app daemons cannot start (see deploy/docker/)"
}

# ── Stage 0 — infrastructure ─────────────────────────────────────────────────
infra_start() {
  hdr "Stage 0 — infrastructure (docker compose)"
  [[ -f "$COMPOSE_FILE" ]] || die "missing $COMPOSE_FILE"
  vlog "docker compose up -d --wait  (PG, Redis×7, NATS×3, ClickHouse, Trino)"
  if ! $COMPOSE up -d --wait; then
    err "infra did not reach healthy"; return 1
  fi
  infra_status
}

infra_stop() {
  hdr "Stage 0 — stop infrastructure"
  # stop, not down: containers stay created so state + topology survive a
  # stop/start cycle; `down` deletes them (volumes are preserved either
  # way). To remove containers + network entirely use compose directly.
  vlog "docker compose stop"
  $COMPOSE stop
  ok "infra stopped (containers + volumes preserved)"
}

infra_status() {
  echo
  $COMPOSE ps --format "table {{.Service}}\t{{.Status}}\t{{.Ports}}" 2>/dev/null | sed 's/^/  /'
}

# ── build ────────────────────────────────────────────────────────────────────
build_images() {
  hdr "build docker images"
  ensure_docker_cli
  local df
  for df in engine go aeron frontend; do
    info "docker build deploy/docker/Dockerfile.$df"
    case "$df" in
      engine)   docker build -t "$ENGINE_DOCKER_IMAGE"   -f "$REPO/deploy/docker/Dockerfile.engine"   "$REPO" || return 1 ;;
      go)       docker build -t "$GO_DOCKER_IMAGE"       -f "$REPO/deploy/docker/Dockerfile.go"       "$REPO" || return 1 ;;
      aeron)    docker build -t "$AERON_DOCKER_IMAGE"    -f "$REPO/deploy/docker/Dockerfile.aeron"    "$REPO" || return 1 ;;
      frontend) docker build -t "$FRONTEND_DOCKER_IMAGE" -f "$REPO/deploy/docker/Dockerfile.frontend" "$REPO" || return 1 ;;
    esac
  done
  ok "images built"
}

ensure_images() {
  local missing=0 img
  for img in "$ENGINE_DOCKER_IMAGE" "$GO_DOCKER_IMAGE" "$AERON_DOCKER_IMAGE" "$FRONTEND_DOCKER_IMAGE"; do
    docker image inspect "$img" >/dev/null 2>&1 || { warn "image $img missing"; missing=1; }
  done
  (( missing )) && { warn "run: $0 build   (or docker build deploy/docker/)"; return 1; }
  return 0
}

# ── preflight ────────────────────────────────────────────────────────────────
preflight() {
  hdr "preflight"
  ensure_docker_cli
  ensure_images || return 1
  baremetal_sweep || warn "bare-metal stack processes found — docker-only policy, stop them before starting"
  vlog "preflight done"
}

# ── start/stop primitives ────────────────────────────────────────────────────
start_one() { # name — compose-supervised
  ensure_docker_cli
  local n="$1" svc; svc="$(docker_svc "$n")"
  # dev.env must be sourced (load_env does): without EXC_JWT_HS256_KEY_B64
  # the gateway boots keyless and every login fails AUTH_INTERNAL
  # "no active signing key configured" (seen 2026-10-02 after a raw
  # `docker compose up` bypassed dev_stack). Never start app services with
  # bare compose — always go through `dev_stack.sh start app`.
  [[ -n "${EXC_JWT_HS256_KEY_B64:-}" ]] || warn "$n: EXC_JWT_HS256_KEY_B64 empty — gateway logins will fail AUTH_INTERNAL (is deploy/dev.env sourced?)"
  [[ -f "$APP_COMPOSE_FILE" ]] || { err "$n: needs $APP_COMPOSE_FILE (see deploy/docker/)"; return 1; }
  vlog "$n ← $APP_COMPOSE up -d $svc"
  ENGINE_DOCKER_IMAGE="$ENGINE_DOCKER_IMAGE" GO_DOCKER_IMAGE="$GO_DOCKER_IMAGE" \
    AERON_DOCKER_IMAGE="$AERON_DOCKER_IMAGE" FRONTEND_DOCKER_IMAGE="$FRONTEND_DOCKER_IMAGE" \
    SHM_BASE="$SHM_BASE" WAL_DIR="$WAL_DIR" REDIS_ADDR="$REDIS_ADDR" \
    AERON_DIR="$AERON_DIR" \
    $APP_COMPOSE up -d "$svc" || return 1
  sleep "$START_GRACE"
  if $APP_COMPOSE ps --format json 2>/dev/null | grep -q "\"Service\":\"$svc\""; then
    ok "$n up (docker service $svc)"
  else
    $APP_COMPOSE ps "$svc" 2>/dev/null | sed 's/^/      /'
    return 1
  fi
}

stop_one() { # name — compose stop (grace per docker-compose.app.yml)
  local n="$1" svc; svc="$(docker_svc "$n")"
  if ! docker_container_running "$n"; then
    vlog "$n not running"; return 0
  fi
  vlog "$n ← $APP_COMPOSE stop $svc"
  # `compose stop` honors each service's stop_grace_period (30s engines,
  # see docker-compose.app.yml) — snapshot + WAL fsync complete before
  # docker escalates. Containers are stopped, never removed.
  $APP_COMPOSE stop "$svc" 2>/dev/null || true
  ok "$n stopped (docker)"
}

# ── application stages ───────────────────────────────────────────────────────
app_start() { # group(all|core)
  local g="$1" row n stage last_stage="" total=0 i=0
  hdr "Stages 1-5 — application daemons ($g · docker)"
  ensure_images || return 1
  for row in "${DAEMONS[@]}"; do
    [[ "$g" == "core" && "$(echo "$row" | cut -d'|' -f3)" != "core" ]] && continue
    total=$((total+1))
  done
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"
    stage="$(echo "$row" | cut -d'|' -f2)"
    [[ "$g" == "core" && "$(echo "$row" | cut -d'|' -f3)" != "core" ]] && continue
    if [[ "$stage" != "$last_stage" ]]; then
      info "stage priority $stage"
      last_stage="$stage"
    fi
    i=$((i+1))
    info "[$i/$total] $n"
    if ! start_one "$n"; then
      # Fail-fast: later tiers depend on this daemon's rings/ports, so
      # starting them on a dead dependency only produces a degraded,
      # misleading stack. Stop here, surface the failure.
      err "aborting start — $n failed at stage $stage (later daemons depend on it)"
      return 1
    fi
  done
}

app_stop() {
  hdr "Stages 1-5 — stop application daemons (reverse order · docker)"
  local row n i k=0 total=0
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"
    docker_container_running "$n" && total=$((total+1))
  done
  for (( i=${#DAEMONS[@]}-1; i>=0; i-- )); do
    n="$(echo "${DAEMONS[$i]}" | cut -d'|' -f1)"
    docker_container_running "$n" || continue
    k=$((k+1))
    info "[$k/$total] $n"
    stop_one "$n"
  done
}

# ── status ───────────────────────────────────────────────────────────────────
app_status() {
  hdr "Stages 1-5 — application daemons (docker)"
  local row n stage port state pid cst
  printf "  %-22s %-6s %-7s %-12s %s\n" "DAEMON" "STAGE" "PORT" "STATE" "SERVICE"
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"
    stage="$(echo "$row" | cut -d'|' -f2)"
    port="$(echo "$row" | cut -d'|' -f4)"
    cst="$(docker_container_state "$n")"
    case "$cst" in
      Up*)        state="${C_G}running${C_0}" ;;
      "")         state="${C_D}absent${C_0}" ;;
      *)          state="${C_D}${cst%% *}${C_0}" ;;
    esac
    printf "  %-22s %-6s %-7s " "$n" "$stage" "$port"
    cell "$state"
    printf "%s\n" "$(docker_svc "$n")"
  done
  hdr "health"
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"
    port="$(echo "$row" | cut -d'|' -f4)"
    hp="$(echo "$row" | cut -d'|' -f5)"
    [[ "$port" == "-" || -z "$hp" ]] && continue
    if curl -sf -m 2 "http://127.0.0.1:$port$hp" >/dev/null 2>&1; then ok "$n :$port$hp"; else warn "$n :$port$hp unreachable"; fi
  done
  if ls /dev/shm/ 2>/dev/null | grep -q "^${SHM_BASE}"; then ok "engine shm rings present (${SHM_BASE})"; else warn "no engine shm rings (${SHM_BASE})"; fi
  if [[ -e "$AERON_DIR/cnc.dat" ]]; then ok "aeron media driver live ($AERON_DIR)"; else warn "no aeron CnC ($AERON_DIR)"; fi
}

do_status() {
  if in_container; then warn "in-container status: process/port state reflects THIS container's namespace (docker rows via compose are accurate)"; fi
  app_status; infra_status; baremetal_sweep
}

# ── top-level actions ────────────────────────────────────────────────────────
act_start() { # all|core|infra|app
  local what="${1:-all}" rc=0
  load_env
  case "$what" in
    all)   preflight && infra_start && app_start all || rc=1 ;;
    core)  preflight && infra_start && app_start core || rc=1 ;;
    infra) ensure_docker_cli && infra_start || rc=1 ;;
    app)   preflight && app_start all || rc=1 ;;
    *)     die "start: unknown target '$what' (all|core|infra|app)" ;;
  esac
  [[ "$what" != "infra" ]] && { do_status; verify_started "$what" || rc=1; }
  return "$rc"
}

verify_started() { # post-start audit — is each requested daemon actually up?
  local what="$1" row n port bad=0 tries=0
  # Ports can take a few seconds to bind after the container is alive —
  # retry the whole sweep for up to ~15s before declaring failure.
  while (( tries < 60 )); do
    bad=0
    for row in "${DAEMONS[@]}"; do
      n="$(echo "$row" | cut -d'|' -f1)"
      port="$(echo "$row" | cut -d'|' -f4)"
      [[ "$what" == "core" && "$(echo "$row" | cut -d'|' -f3)" != "core" ]] && continue
      docker_container_running "$n" || { bad=1; continue; }
      # Port check: container alive but hasn't bound its listen port yet.
      [[ "$port" != "-" ]] && ! ss -ltn 2>/dev/null | awk '{print $4}' \
        | grep -qE "[:.]${port}\$" && bad=1
    done
    (( bad == 0 )) && break
    sleep 0.25; tries=$((tries+1))
  done
  if (( bad != 0 )); then
    err "verify: daemons not fully up after start —"
    for row in "${DAEMONS[@]}"; do
      n="$(echo "$row" | cut -d'|' -f1)"; port="$(echo "$row" | cut -d'|' -f4)"
      [[ "$what" == "core" && "$(echo "$row" | cut -d'|' -f3)" != "core" ]] && continue
      if ! docker_container_running "$n"; then
        warn "  $n: container not running — see: $APP_COMPOSE logs $(docker_svc "$n")"
      elif [[ "$port" != "-" ]] && ! ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}\$"; then
        warn "  $n: alive but port $port not bound — see: $APP_COMPOSE logs $(docker_svc "$n")"
      fi
    done
    return 1
  fi
  ok "verify: all daemons up"
}

act_stop() { # all|app|infra
  local what="${1:-all}" rc=0
  load_env
  case "$what" in
    all)   app_stop || rc=1; infra_stop || rc=1 ;;
    app)   app_stop || rc=1 ;;
    infra) infra_stop || rc=1 ;;
    *)     die "stop: unknown target '$what' (all|app|infra)" ;;
  esac
  baremetal_sweep || warn "bare-metal stack processes survived — docker-only policy violation"
  verify_stopped "$what" || rc=1
  return "$rc"
}

verify_stopped() { # post-stop audit — did everything actually go down?
  local what="$1" row n bad=0
  if [[ "$what" != "infra" ]]; then
    for row in "${DAEMONS[@]}"; do
      n="$(echo "$row" | cut -d'|' -f1)"
      if docker_container_running "$n"; then
        warn "verify: $n container still running"; bad=1
      fi
    done
  fi
  if [[ "$what" != "app" ]] && [[ -f "$COMPOSE_FILE" ]]; then
    if $COMPOSE ps --filter status=running -q 2>/dev/null | grep -q .; then
      warn "verify: infra containers still running:"; bad=1
      $COMPOSE ps --filter status=running --format '      {{.Name}}  {{.Status}}' 2>/dev/null
    fi
  fi
  if [[ "$bad" == 0 ]]; then ok "verify: stack stopped"; else err "verify: stack NOT fully stopped"; fi
  return "$bad"
}

act_logs() {
  local n="${1:-}"
  [[ -n "$n" ]] || die "logs: name a daemon (e.g. gateway)"
  info "$APP_COMPOSE logs -f $(docker_svc "$n")   (Ctrl-C to stop)"
  $APP_COMPOSE logs -f "$(docker_svc "$n")"
}

# ── interactive menu ─────────────────────────────────────────────────────────
menu_status() { # per-daemon container state + infra summary
  local row n svc dock line cname cst
  # One batched docker query for all exc-dev-* containers, keyed by
  # service name (container is exc-dev-<svc>-<replica>).
  declare -A CST=()
  while read -r line; do
    cname="${line%% *}"; cst="${line#* }"
    svc="${cname#exc-dev-}"; svc="${svc%-*}"
    CST["$svc"]="$cst"
  done < <(docker ps -a --filter "name=exc-dev-" \
    --format '{{.Names}} {{.Status}}' 2>/dev/null)
  printf "  %-22s %-12s %s\n" "DAEMON" "DOCKER" "STAGE"
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"
    svc="$(docker_svc "$n")"
    case "${CST[$svc]:-}" in
      Up*)      dock="${C_G}up${C_0}" ;;
      "")       dock="${C_D}absent${C_0}" ;;
      *)        dock="${C_D}stopped${C_0}" ;;
    esac
    printf "  %-22s " "$n"; cell "$dock"
    printf "%s\n" "$(echo "$row" | cut -d'|' -f2)"
  done
  local up=0 tot=0
  if [[ -f "$COMPOSE_FILE" ]]; then
    tot="$($COMPOSE ps -a -q 2>/dev/null | wc -l)"
    up="$($COMPOSE ps --filter status=running -q 2>/dev/null | wc -l)"
  fi
  printf "  %-22s %-12s %s\n" "infra (stage 0)" "-" "$up/$tot containers up"
  baremetal_sweep >/dev/null 2>&1 || printf "  ${C_Y}! bare-metal stack processes detected (see status)${C_0}\n"
}

menu() {
  load_env
  while true; do
    hdr "exc.local — dev stack   (${C_D}docker-only · shm=$SHM_BASE${C_0})"
    menu_status
    cat <<EOF

  ${C_B}action${C_0}:  [s]tart · sto[p] · [r]estart · stat[u]s · [l]ogs · [q]uit
EOF
    read -rp "  action: " act
    case "$act" in
      s|start)   menu_target start ;;
      p|stop)    menu_target stop ;;
      r|restart) menu_target restart ;;
      u|status)  do_status ;;
      l|logs)    read -rp "  daemon name: " dn; act_logs "$dn" ;;
      q|quit|exit|0|"") info "bye"; exit 0 ;;
      *) warn "unknown action '$act'" ;;
    esac
  done
}

menu_target() { # action(start|stop|restart) → prompt scope
  local action="$1" t
  echo "  ${C_B}target${C_0}:  [a]pp daemons · [c]ore daemons · [i]nfra · [e]verything · cance[l]"
  read -rp "  target: " t
  case "$action:$t" in
    start:a)   act_start app ;;
    start:c)   act_start core ;;
    start:i)   act_start infra ;;
    start:e)   act_start all ;;
    stop:a)    act_stop app ;;
    stop:i)    act_stop infra ;;
    stop:e)    act_stop all ;;
    stop:c)    warn "stop: core-only not supported — app daemons stop as one tier";;
    restart:a) act_stop app   && act_start app ;;
    restart:c) act_stop app   && act_start core ;;
    restart:i) act_stop infra && act_start infra ;;
    restart:e) act_stop all   && act_start all ;;
    *:l|*:c|*:"") info "cancelled" ;;
    *)         warn "unknown target '$t'" ;;
  esac
}

usage() {
  # Print the header comment block: line 2 through the closing banner,
  # located dynamically so the range can't silently truncate doc lines.
  local end; end="$(awk 'NR>2 && /^# =/{print NR; exit}' "$0")"
  sed -n "2,${end:-71}p" "$0" | sed 's/^# \{0,1\}//'
}

# ── entrypoint ───────────────────────────────────────────────────────────────
cmd="${1:-}"
case "$cmd" in
  "")        menu ;;
  start)     shift; act_start "${1:-all}" ;;
  stop)      shift; act_stop "${1:-all}" ;;
  restart)   act_stop all && act_start all ;;
  status)    load_env; do_status ;;
  logs)      shift; act_logs "${1:-}" ;;
  build)     load_env; build_images ;;
  -h|--help|help) usage ;;
  *)         usage; exit 2 ;;
esac
