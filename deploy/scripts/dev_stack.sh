#!/usr/bin/env bash
# =============================================================================
# dev_stack.sh — interactive lifecycle for the exc.local local development stack
#
# Brings up (and tears down) BOTH halves of the local topology:
#
#   Stage 0  clustered infrastructure — docker-compose.dev.yml
#            PostgreSQL 16 · Redis 1 primary + 2 replicas + 3 sentinels + cache
#            · NATS JetStream ×3 · ClickHouse · Trino
#
#   Stage 1-5  application daemons, started in the deterministic
#            bootstrap order of spec §19.13.2 (Tier 1 IPC → Tier 2 State →
#            Tier 3 Core Engines → Tier 4 Services → Tier 5 Gateways):
#              aeronmd · watchdogd · matching-engine · bridge · oracle
#              · marketdata · risk · settlement · gateway · compliance
#              · analytics · recovery-orchestrator · sentinel_exporter
#              · admin · fix
#
# Run modes (prod docker redesign, 2026-10-02 — supersedes prior
# "bare-metal only, no containers in hot path" default for local dev):
#   host   (default) — bare-metal binaries with PID files, per spec §19.1/
#            §19.13.1 (C++ core NUMA-pinned, no containers in hot path).
#            Lowest latency jitter; required for p99 ≤50µs validation.
#   docker — every daemon runs as a container via docker-compose.app.yml
#            (images from deploy/docker/; engine + aeronmd use host IPC +
#            host networking + /dev/shm + WAL volumes — 127.0.0.1 inside a
#            container IS the host, so all host-mapped addresses behave
#            exactly like host mode; still needs isolcpus/NUMA on the host
#            for determinism — containers add jitter, so soak/benchmark
#            numbers taken in docker mode are NOT production evidence).
#            Only PTP (ptp4l/phc2sys) stays on the host — hardware clock;
#            docker mode uses the software-clock fallback per §19.13.4.
#   Go daemons are dockerized with EXC_GO_MODE=docker; engine + aeronmd
#   with EXC_ENGINE_MODE=docker; EXC_APP_MODE=docker sets both at once.
#   The trader UI (frontend) is docker-only — no host-mode binary.
#
# Supervisord still required? No — in docker mode compose
# restart/healthchecks supervise every daemon (see docker-compose.app.yml
# + deploy/supervisord.conf header); supervisord remains only for
# bare-metal/host-mode operation.
# This script supervises host-mode daemons directly with PID files
# instead of supervisord, because supervisord is not installed on
# this host and deploy/supervisord.conf references several `cmd/`
# names that have no implementation. Every host-mode daemon here is
# a real, runnable binary.
#
# Usage:
#   deploy/scripts/dev_stack.sh                 # interactive menu
#   deploy/scripts/dev_stack.sh start all       # infra + every app daemon
#   deploy/scripts/dev_stack.sh start core      # infra + trading-path daemons
#   deploy/scripts/dev_stack.sh start infra     # Stage 0 only
#   deploy/scripts/dev_stack.sh start app       # app daemons only (infra assumed up)
#   deploy/scripts/dev_stack.sh stop all        # app daemons + infra
#   deploy/scripts/dev_stack.sh stop app        # app daemons only
#   deploy/scripts/dev_stack.sh restart
#   deploy/scripts/dev_stack.sh status
#   deploy/scripts/dev_stack.sh logs <daemon>   # tail one daemon's log
#   deploy/scripts/dev_stack.sh build           # (re)build the binaries
#
# Env overrides:
#   EXC_DEV_RUN_DIR   runtime state root      (default /tmp/exc-dev-stack)
#   EXC_WAL_DIR       engine WAL directory    (default $RUN_DIR/wal)
#   EXC_SHM_BASE      shm ring namespace      (default exchange_ipc)
#   EXC_REDIS_ADDR    coordination Redis      (default 127.0.0.1:16379)
#   EXC_DEV_QUIET=1   suppress the verbose trace
#   EXC_DEV_FREE_PORTS=1  auto-kill foreign processes holding a needed port
#   EXC_APP_MODE=docker      dockerize engine + Go daemons at once
#   EXC_ENGINE_MODE=host|docker  matching-engine run mode (default host)
#   EXC_GO_MODE=host|docker      Go daemons run mode (default host)
#   EXC_APP_COMPOSE=path        app compose file (default docker-compose.app.yml)
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
PID_DIR="$RUN_DIR/pids"
WAL_DIR="${EXC_WAL_DIR:-$RUN_DIR/wal}"
SPOOL_DIR="$RUN_DIR/ch-spool"
SHM_BASE="${EXC_SHM_BASE:-exchange_ipc}"
REDIS_ADDR="${EXC_REDIS_ADDR:-127.0.0.1:16379}"
AERON_DIR="${EXC_AERON_DIR:-/dev/shm/aeron-${USER:-root}}"

COMPOSE_FILE="$REPO/docker-compose.dev.yml"
COMPOSE="docker compose -f $COMPOSE_FILE"
APP_COMPOSE_FILE="${EXC_APP_COMPOSE:-$REPO/docker-compose.app.yml}"
# Both files share project `exc-dev`: app services join the infra network and
# resolve redis-primary/nats-1/postgres by service name (fixed 2026-10-02 —
# app-only `-f` broke depends_on; the combined model is the supported path).
APP_COMPOSE="docker compose -f $COMPOSE_FILE -f $APP_COMPOSE_FILE"
# Run-mode resolution: EXC_APP_MODE=docker is shorthand for both.
if [[ "${EXC_APP_MODE:-host}" == "docker" ]]; then
  ENGINE_MODE="docker"
  GO_MODE="docker"
else
  ENGINE_MODE="${EXC_ENGINE_MODE:-host}"
  GO_MODE="${EXC_GO_MODE:-host}"
fi
ENGINE_DOCKER_IMAGE="${EXC_ENGINE_IMAGE:-exc-matching-engine:local}"
GO_DOCKER_IMAGE="${EXC_GO_IMAGE:-exc-go-service:local}"
AERON_DOCKER_IMAGE="${EXC_AERON_IMAGE:-exc-aeronmd:local}"
FRONTEND_DOCKER_IMAGE="${EXC_FRONTEND_IMAGE:-exc-frontend:local}"
BIN_DIR="$REPO/services/bin"
ENGINE_BIN="$REPO/core/build/matching_engine"
AERON_BIN="$REPO/core/third_party/aeron/bin/aeronmd"

STOP_GRACE=15          # engines fsync WAL+snapshot on SIGTERM; compose grants them the same 15s
START_GRACE=3          # seconds to wait before declaring a daemon "up"
FREE_PORTS="${EXC_DEV_FREE_PORTS:-0}"
VLOG=1
[[ "${EXC_DEV_QUIET:-0}" == "1" ]] && VLOG=0

# ── daemon inventory ─────────────────────────────────────────────────────────
# name|stage|group|port|healthpath
#   group = core → trading path (engine, market data, risk, settlement, gateway)
#           ext  → supporting services (compliance, analytics, admin, fix, …)
#   port  = HTTP bind port (0/- = none) — used for the conflict preflight
#   health= path served on that port (empty = no HTTP health surface)
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

# ── container guard ────────────────────────────────────────────────────────
# dev_stack.sh is a HOST orchestrator: PID files, /proc scans, ss/pgrep,
# /dev/shm and nohup supervision only mean something on the host. Inside a
# container those read container-local state — every host daemon would
# report "stopped" and `start` would double-start it (verified 2026-10-02:
# golang container + repo mount printed all-stopped while the host stack
# was live). So: inside a container, only full-docker mode is allowed
# (compose talks to the host daemon via the socket; host networking keeps
# addresses valid). Host rows in `status` are still container-local there.
in_container() { [[ -f /.dockerenv ]] || grep -qaE 'docker|kubepods' /proc/1/cgroup 2>/dev/null; }
require_host_or_docker() { # action — refuse host-mode supervision in containers
  local what="$1"
  if in_container && [[ "$ENGINE_MODE" != "docker" || "$GO_MODE" != "docker" ]]; then
    die "in-container $what needs full-docker mode (EXC_APP_MODE=docker): host PID/IPC supervision is meaningless inside a container (needs host namespaces + repo + docker socket for compose)"
  fi
  if in_container; then
    warn "running inside a container: host-mode rows below reflect THIS container, not the host"
  fi
}
ensure_docker_cli() {
  command -v docker >/dev/null 2>&1 || die "docker mode needs the docker CLI (mount the host socket: -v /var/run/docker.sock:/var/run/docker.sock)"
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

# ── small helpers ────────────────────────────────────────────────────────────
pidfile() { echo "$PID_DIR/$1.pid"; }
logfile() { echo "$LOG_DIR/$1.log"; }

pid_alive() {
  local pf; pf="$(pidfile "$1")"
  [[ -f "$pf" ]] || return 1
  local pid; pid="$(cat "$pf" 2>/dev/null)"
  [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null || return 1
  # Identity check: a pid file proves nothing about what the pid *is*.
  # If the daemon died and the pid was reused, the stop path would
  # SIGTERM/SIGKILL an unrelated process. Require the pid to exec one
  # of this stack's binaries before treating it as ours.
  local exe; exe="$(readlink "/proc/$pid/exe" 2>/dev/null)"
  case "$exe" in
    "$BIN_DIR/"*|"$ENGINE_BIN"|"$AERON_BIN") return 0 ;;
    *) warn "$1: pid $pid is $exe, not a dev-stack binary — treating pid file as stale"; rm -f "$pf"; return 1 ;;
  esac
}
pid_of() { cat "$(pidfile "$1")" 2>/dev/null; }

port_holder_pid() { # port -> pid(s) listening (ss → fuser → lsof)
  local pids
  pids="$(ss -ltnp 2>/dev/null | awk -v p=":$1\$" '$4 ~ p {print $NF}' | grep -oE 'pid=[0-9]+' | cut -d= -f2)"
  [[ -z "$pids" ]] && pids="$(fuser -n tcp "$1" 2>/dev/null | tr -s ' ' '\n' | grep -E '^[0-9]+$')"
  [[ -z "$pids" ]] && pids="$(lsof -ti "tcp:$1" -sTCP:LISTEN 2>/dev/null)"
  echo "$pids" | sort -u
}
port_busy() { [[ -n "$(port_holder_pid "$1")" ]] || ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$1\$"; }

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

# Full launch command (env prefix + exec) for a daemon.
daemon_cmd() {
  case "$1" in
    aeronmd)
      echo "exec $AERON_BIN" ;;
    watchdogd)
      echo "exec $BIN_DIR/watchdogd -redis-addr $REDIS_ADDR -aeron-dir $AERON_DIR -ptp=false -ptp-expected=false -no-demote" ;;
    matching-engine|matching-engine-[0-9]*)
      # One process per shard — the gateway readiness probe fails closed
      # unless every shard in config/sharding.yaml stamps a live producer.
      local _s="${1#matching-engine}"; _s="${_s#-}"; _s="${_s:-0}"
      echo "exec $ENGINE_BIN -shard $_s -ipc-base $SHM_BASE -wal-dir $WAL_DIR -redis $REDIS_ADDR -symbol EUR/USD -instrument-id 1 -dev-all-accounts" ;;
    xshardrelay)
      echo "exec $BIN_DIR/xshardrelay -shards ${EXC_XSHARD_SHARDS:-0,1,2,3,4,5,6,7}" ;;
    oracle)
      echo "EXC_ORACLE_SIM=1 EXC_ORACLE_HEALTH_ADDR=127.0.0.1:8090 exec $BIN_DIR/oracle" ;;
    risk)
      echo "EXC_RISK_HEALTH_ADDR=127.0.0.1:8091 exec $BIN_DIR/risk" ;;
    analytics)
      echo "EXC_CH_SPOOL_DIR=$SPOOL_DIR exec $BIN_DIR/analytics" ;;
    settlement)
      echo "EXC_SENDER_BIC=EXCHUS33 exec $BIN_DIR/settlement" ;;
    *)
      echo "exec $BIN_DIR/$1" ;;
  esac
}

# ── environment ──────────────────────────────────────────────────────────────
load_env() {
  mkdir -p "$RUN_DIR" "$LOG_DIR" "$PID_DIR" "$WAL_DIR" "$SPOOL_DIR"
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
  export EXC_ORACLE_HEALTH_ADDR="127.0.0.1:8090"
  export EXC_RISK_HEALTH_ADDR="127.0.0.1:8091"
  export EXC_SENTINEL_ADDRS="${EXC_SENTINEL_ADDRS:-127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381}"
  vlog "shm=$EXC_SHM_BASE wal=$EXC_WAL_DIR redis=$EXC_REDIS_ADDR aeron=$AERON_DIR"
  vlog "mode: engine=$ENGINE_MODE go=$GO_MODE (docker adds jitter — not prod p99 evidence)"
  if [[ "$ENGINE_MODE" == "docker" || "$GO_MODE" == "docker" ]]; then
    [[ -f "$APP_COMPOSE_FILE" ]] || warn "docker mode selected but $APP_COMPOSE_FILE missing — create it (see deploy/docker/) before start"
  fi
}

# ── Stage 0 — infrastructure ─────────────────────────────────────────────────
infra_start() {
  hdr "Stage 0 — infrastructure (docker compose)"
  [[ -f "$COMPOSE_FILE" ]] || die "missing $COMPOSE_FILE"
  vlog "docker compose up -d --wait  (PG, Redis×6, NATS×3, ClickHouse, Trino)"
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
build_apps() {
  hdr "build application binaries"
  info "Go services → services/bin/  (go build -o bin/ ./cmd/...)"
  ( cd "$REPO/services" && go build -o bin/ ./cmd/... ) || die "go build failed"
  ok "Go binaries built"
  [[ -x "$AERON_BIN" ]] || warn "vendored aeronmd missing at $AERON_BIN (bridge/risk will not start)"
  if [[ ! -x "$ENGINE_BIN" ]]; then
    warn "engine binary missing — building C++ core"
    cmake -S "$REPO/core" -B "$REPO/core/build" -DCMAKE_BUILD_TYPE=Release >/dev/null || die "cmake configure failed"
    cmake --build "$REPO/core/build" -j >/dev/null || die "cmake build failed"
  fi
  ok "engine: $ENGINE_BIN"
}

ensure_binaries() {
  local missing=0 n mode
  for n in $(daemon_names); do
    mode="$(daemon_run_mode "$n")"
    # Docker-mode daemons run from images — no host binary needed.
    [[ "$mode" == "docker" ]] && continue
    [[ "$n" == matching-engine* ]] && { [[ -x "$ENGINE_BIN" ]] || missing=1; continue; }
    [[ "$n" == "aeronmd" ]] && { [[ -x "$AERON_BIN" ]] || missing=1; continue; }
    [[ -x "$BIN_DIR/$n" ]] || missing=1
  done
  (( missing )) && build_apps
  return 0
}

# ── preflight ────────────────────────────────────────────────────────────────
free_port() { # port name
  local port="$1" name="$2" pid
  for pid in $(port_holder_pid "$port"); do
    [[ "$pid" == "$$" ]] && continue
    local cmd; cmd="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null | cut -c1-90)"
    # The holder lives inside a container (host-net service): SIGKILL only
    # triggers a compose restart, not a free port — direct the operator to
    # `compose stop` instead of starting a kill/resurrect loop.
    if grep -qaE 'docker|kubepods' "/proc/$pid/cgroup" 2>/dev/null; then
      warn "port $port busy (needed by $name) — held by containerized pid $pid: $cmd"
      warn "  stop the compose service instead of killing it"
      continue
    fi
    if [[ "$FREE_PORTS" == "1" ]]; then
      warn "freeing :$port — killing pid $pid ($cmd)"
      kill -TERM "$pid" 2>/dev/null; sleep 1; kill -KILL "$pid" 2>/dev/null
    else
      warn "port $port busy (needed by $name) — pid $pid: $cmd"
      warn "  re-run with EXC_DEV_FREE_PORTS=1, or stop pid $pid, to free it"
    fi
  done
}

preflight() {
  hdr "preflight"
  local row n port
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"; port="$(echo "$row" | cut -d'|' -f4)"
    [[ "$port" == "-" ]] && continue
    pid_alive "$n" && continue
    port_busy "$port" && free_port "$port" "$n"
  done
  foreign_engine_check
  vlog "preflight done"
}

# A matching_engine started outside this script (e.g. a leftover e2e harness
# run on a different -ipc-base) still contends for the shard leader lock and
# will make order legs time out with GATEWAY_TIMEOUT_MATCHING_ENGINE.
foreign_engine_check() {
  local mine="" pid argv0
  pid_alive matching-engine && mine="$(pid_of matching-engine)"
  for pid in $(pgrep -f 'matching_engine' 2>/dev/null); do
    [[ "$pid" == "$mine" || "$pid" == "$$" || "$pid" == "$PPID" ]] && continue
    # argv[0] must be the engine binary itself — not a shell/bwrap that merely
    # mentions the word (a false positive otherwise).
    argv0="$(tr '\0' '\n' < "/proc/$pid/cmdline" 2>/dev/null | head -1)"
    [[ "$argv0" == *matching_engine ]] || continue
    # A containerized engine (docker mode) shows the same argv0 — its cgroup
    # carries docker/kubepods, so only HOST-mode processes can be foreign.
    grep -qaE 'docker|kubepods' "/proc/$pid/cgroup" 2>/dev/null && continue
    warn "foreign matching_engine pid $pid (not managed here): $argv0"
    warn "  a stray engine on the same shard causes 504 GATEWAY_TIMEOUT_MATCHING_ENGINE"
    warn "  kill it:  kill $pid   (or: pkill -f matching_engine, then re-run)"
  done
}

# ── start / stop app daemons ─────────────────────────────────────────────────
# Docker run-mode routing (full-project docker): aeronmd follows ENGINE_MODE
# (Stage-1 IPC foundation for the containerized engine), watchdogd follows
# GO_MODE, frontend is docker-only (no host binary exists). Supervisord/
# compose mapping: dockerized daemons are supervised by compose
# restart/healthchecks, NOT supervisord or PID files.
daemon_run_mode() { # name -> host|docker
  case "$1" in
    frontend)          echo "docker" ;;
    matching-engine*|aeronmd) echo "$ENGINE_MODE" ;;
    *)                 echo "$GO_MODE" ;;
  esac
}
docker_svc() { echo "${1//_/-}"; }

docker_start_one() { # name — compose-supervised, no PID file
  ensure_docker_cli
  local n="$1" svc; svc="$(docker_svc "$n")"
  # dev.env must be sourced (load_env does): without EXC_JWT_HS256_KEY_B64
  # the gateway boots keyless and every login fails AUTH_INTERNAL
  # "no active signing key configured" (seen 2026-10-02 after a raw
  # `docker compose up` bypassed dev_stack). Never start app services with
  # bare compose — always go through `dev_stack.sh start app`.
  [[ -n "${EXC_JWT_HS256_KEY_B64:-}" ]] || warn "$n: EXC_JWT_HS256_KEY_B64 empty — gateway logins will fail AUTH_INTERNAL (is deploy/dev.env sourced?)"
  [[ -f "$APP_COMPOSE_FILE" ]] || { err "$n: docker mode needs $APP_COMPOSE_FILE (see deploy/docker/)"; return 1; }
  vlog "$n ← $APP_COMPOSE up -d $svc"
  # AERON_DIR pinned to the canonical driver dir in docker mode: the host
  # default (/dev/shm/aeron-${USER}) would disagree with the containerized
  # driver's --aeron-dir and strand every Aeron client (fixed 2026-10-02).
  ENGINE_DOCKER_IMAGE="$ENGINE_DOCKER_IMAGE" GO_DOCKER_IMAGE="$GO_DOCKER_IMAGE" \
    AERON_DOCKER_IMAGE="$AERON_DOCKER_IMAGE" FRONTEND_DOCKER_IMAGE="$FRONTEND_DOCKER_IMAGE" \
    SHM_BASE="$SHM_BASE" WAL_DIR="$WAL_DIR" REDIS_ADDR="$REDIS_ADDR" \
    AERON_DIR="/dev/shm/aeron-exchange" \
    $APP_COMPOSE up -d "$svc" || return 1
  sleep "$START_GRACE"
  if $APP_COMPOSE ps --format json 2>/dev/null | grep -q "\"Service\":\"$svc\""; then
    ok "$n up (docker service $svc)"
  else
    $APP_COMPOSE ps "$svc" 2>/dev/null | sed 's/^/      /'
    return 1
  fi
}

docker_stop_one() { # name
  local n="$1" svc; svc="$(docker_svc "$n")"
  [[ -f "$APP_COMPOSE_FILE" ]] || { vlog "$n: no $APP_COMPOSE_FILE, nothing to stop"; return 0; }
  vlog "$n ← $APP_COMPOSE stop $svc"
  $APP_COMPOSE stop "$svc" 2>/dev/null || true
  ok "$n stopped (docker)"
}

start_one() { # name
  local n="$1"
  if [[ "$(daemon_run_mode "$n")" == "docker" ]]; then docker_start_one "$n"; return $?; fi
  if pid_alive "$n"; then ok "$n already running (pid $(pid_of "$n"))"; return 0; fi
  local cmd log pf
  cmd="$(daemon_cmd "$n")"
  log="$(logfile "$n")"; pf="$(pidfile "$n")"
  vlog "$n ← $cmd"
  : > "$log"
  nohup bash -c "$cmd" >>"$log" 2>&1 &
  echo $! > "$pf"
  sleep "$START_GRACE"
  if pid_alive "$n"; then
    ok "$n up (pid $(pid_of "$n"), stage $(daemon_field "$n" 2))"
  else
    err "$n exited during startup — last log lines:"
    sed 's/^/      /' <(tail -n 6 "$log" 2>/dev/null)
    rm -f "$pf"
    return 1
  fi
}

stop_one() { # name
  local n="$1" pf pid i=0
  if [[ "$(daemon_run_mode "$n")" == "docker" ]]; then docker_stop_one "$n"; return $?; fi
  pf="$(pidfile "$n")"
  if ! pid_alive "$n"; then
    [[ -f "$pf" ]] && rm -f "$pf"
    # Mode mismatch guard: a compose container can be running even when
    # this invocation resolves host mode (started earlier under
    # EXC_APP_MODE=docker). Stop it too rather than report a lie.
    if docker ps --filter "name=exc-dev-$(docker_svc "$n")-" \
        --filter status=running -q 2>/dev/null | grep -q .; then
      docker_stop_one "$n"
    else
      vlog "$n not running"
    fi
    return 0
  fi
  pid="$(pid_of "$n")"
  vlog "$n (pid $pid) ← SIGTERM"
  kill -TERM "$pid" 2>/dev/null
  while kill -0 "$pid" 2>/dev/null && (( i < STOP_GRACE*4 )); do sleep 0.25; i=$((i+1)); done
  if kill -0 "$pid" 2>/dev/null; then
    warn "$n ignored SIGTERM after ${STOP_GRACE}s — SIGKILL"
    kill -KILL "$pid" 2>/dev/null; sleep 0.5
  fi
  rm -f "$pf"
  ok "$n stopped"
}

app_start() { # group(all|core)
  local g="$1" row n stage last_stage=""
  hdr "Stages 1-5 — application daemons ($g)"
  ensure_binaries || return 1
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"
    stage="$(echo "$row" | cut -d'|' -f2)"
    if [[ "$g" == "core" && "$(echo "$row" | cut -d'|' -f3)" != "core" ]]; then continue; fi
    if [[ "$stage" != "$last_stage" ]]; then
      info "stage priority $stage"
      last_stage="$stage"
    fi
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
  hdr "Stages 1-5 — stop application daemons (reverse order)"
  local row n i
  for (( i=${#DAEMONS[@]}-1; i>=0; i-- )); do
    n="$(echo "${DAEMONS[$i]}" | cut -d'|' -f1)"
    stop_one "$n"
  done
}

# ── status ───────────────────────────────────────────────────────────────────
app_status() {
  hdr "Stages 1-5 — application daemons (engine=$ENGINE_MODE go=$GO_MODE)"
  local row n stage port state pid hp mode
  printf "  %-22s %-6s %-7s %-7s %-9s %s\n" "DAEMON" "STAGE" "PORT" "MODE" "STATE" "PID/SERVICE"
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"
    stage="$(echo "$row" | cut -d'|' -f2)"
    port="$(echo "$row" | cut -d'|' -f4)"
    mode="$(daemon_run_mode "$n")"
    if [[ "$mode" == "docker" ]]; then
      # Report the container's real state, not just the configured mode.
      local cst; cst="$(docker ps -a --filter "name=exc-dev-$(docker_svc "$n")-" \
        --format '{{.Status}}' 2>/dev/null | head -1)"
      case "$cst" in
        Up*)        state="${C_G}running${C_0}" ;;
        "")         state="${C_D}absent${C_0}" ;;
        *)          state="${C_D}${cst%% *}${C_0}" ;;
      esac
      pid="$(docker_svc "$n")"
    elif pid_alive "$n"; then state="${C_G}running${C_0}"; pid="$(pid_of "$n")"; else state="${C_D}stopped${C_0}"; pid="-"; fi
    printf "  %-22s %-6s %-7s %-7s %-18b %s\n" "$n" "$stage" "$port" "$mode" "$state" "$pid"
  done
  hdr "health"
  for row in "${DAEMONS[@]}"; do
    n="$(echo "$row" | cut -d'|' -f1)"
    port="$(echo "$row" | cut -d'|' -f4)"
    hp="$(echo "$row" | cut -d'|' -f5)"
    [[ "$port" == "-" || -z "$hp" ]] && continue
    if curl -sf -m 2 "http://127.0.0.1:$port$hp" >/dev/null 2>&1; then ok "$n :$port$hp"; else warn "$n :$port$hp unreachable"; fi
  done
  if [[ -x "$ENGINE_BIN" ]]; then
    if ls /dev/shm/ 2>/dev/null | grep -q "^${SHM_BASE}"; then ok "engine shm rings present (${SHM_BASE})"; else warn "no engine shm rings (${SHM_BASE})"; fi
  fi
  if [[ -e "$AERON_DIR/cnc.dat" ]]; then ok "aeron media driver live ($AERON_DIR)"; else warn "no aeron CnC ($AERON_DIR)"; fi
}

do_status() {
  if in_container; then warn "in-container status: host-mode rows reflect THIS container, not the host (docker rows via compose are accurate)"; fi
  app_status; infra_status; foreign_run_pids
}

# ── top-level actions ────────────────────────────────────────────────────────
act_start() { # all|core|infra|app
  local what="${1:-all}" rc=0
  load_env
  require_host_or_docker "start $what"
  case "$what" in
    all)   preflight; infra_start && app_start all || rc=1 ;;
    core)  preflight; infra_start && app_start core || rc=1 ;;
    infra) infra_start || rc=1 ;;
    app)   preflight; app_start all || rc=1 ;;
    *)     die "start: unknown target '$what' (all|core|infra|app)" ;;
  esac
  [[ "$what" != "infra" ]] && do_status
  return "$rc"
}

act_stop() { # all|app|infra
  local what="${1:-all}" rc=0
  load_env
  require_host_or_docker "stop $what"
  case "$what" in
    all)   app_stop || rc=1; infra_stop || rc=1 ;;
    app)   app_stop || rc=1 ;;
    infra) infra_stop || rc=1 ;;
    *)     die "stop: unknown target '$what' (all|app|infra)" ;;
  esac
  # Daemons started under a DIFFERENT RUN_DIR are invisible to our pid
  # files — that's what made `stop all` appear to no-op. Surface them.
  foreign_run_pids
  return "$rc"
}

foreign_run_pids() { # warn about live daemons owned by other RUN_DIRs
  local f d pid exe
  for f in "${RUN_DIR%/*}"/exc-dev-stack*/pids/*.pid; do
    [[ -f "$f" ]] || continue
    d="${f%/pids/*}"
    [[ "$d" == "$RUN_DIR" ]] && continue
    pid="$(cat "$f" 2>/dev/null)"
    [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null || continue
    exe="$(readlink "/proc/$pid/exe" 2>/dev/null)"
    case "$exe" in
      "$BIN_DIR/"*|"$ENGINE_BIN"|"$AERON_BIN")
        warn "daemon alive under foreign run dir $d: $(basename "$f" .pid) (pid $pid)"
        warn "  stop it via: EXC_DEV_RUN_DIR=$d $0 stop app" ;;
    esac
  done
}

act_logs() {
  local n="${1:-}"
  [[ -n "$n" ]] || die "logs: name a daemon (e.g. gateway)"
  if [[ "$(daemon_run_mode "$n")" == "docker" ]]; then
    info "$APP_COMPOSE logs -f $(docker_svc "$n")   (Ctrl-C to stop)"
    $APP_COMPOSE logs -f "$(docker_svc "$n")"
    return $?
  fi
  local log; log="$(logfile "$n")"
  [[ -f "$log" ]] || die "no log for '$n' ($log)"
  info "tail -f $log   (Ctrl-C to stop)"
  tail -f "$log"
}

menu() {
  load_env
  while true; do
    cat <<EOF

${C_B}exc.local — local dev stack${C_0}   (${C_D}repo: $REPO${C_0})
  1) start  everything   infra + all app daemons
  2) start  core only    infra + trading-path daemons
  3) start  infra only   Stage 0 (docker compose)
  4) start  app only     app daemons (infra assumed up)
  5) stop   app only     app daemons
  6) stop   everything   app daemons + infra
  7) restart             stop everything, then start everything
  8) status
  9) logs                tail a daemon's log
  0) exit
EOF
    read -rp "  choose [1]: " choice; choice="${choice:-1}"
    case "$choice" in
      1) act_start all ;;
      2) act_start core ;;
      3) act_start infra ;;
      4) act_start app ;;
      5) act_stop app ;;
      6) act_stop all ;;
      7) act_stop all && act_start all ;;
      8) do_status ;;
      9) read -rp "  daemon name: " dn; act_logs "$dn" ;;
      0|q|exit) info "bye"; exit 0 ;;
      *) warn "unknown choice '$choice'" ;;
    esac
  done
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
  build)     load_env; build_apps ;;
  -h|--help|help) usage ;;
  *)         usage; exit 2 ;;
esac
