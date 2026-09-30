#!/usr/bin/env bash
# compose_stage_validate.sh — Phase-09 Task 9.3.28 DoD: verify the local
# development environment boots in the deterministic 6-stage topological
# order of spec §19.13.2 and every stage reaches HEALTHY before the next
# gate opens.
#
# In the dev profile, docker-compose.dev.yml carries Stage 0 (clustered
# infrastructure). Stages 1-5 are bare-metal `go run` daemons orchestrated
# by deploy/supervisord.conf — this script validates Stage 0 end-to-end
# (infra DAG order + health + quorum wiring) and then validates that
# supervisord.conf encodes every §19.13.1 daemon at the correct tier
# priority with autorestart (structural DAG check for the process tiers).
#
# Exit non-zero on any ordering or health violation. Usage:
#   deploy/scripts/compose_stage_validate.sh [--no-up]
#     --no-up: assert health/order on the already-running stack (CI probe).
set -euo pipefail
cd "$(dirname "$0")/../.."
COMPOSE="docker compose -f docker-compose.dev.yml"
NO_UP=0; [[ "${1:-}" == "--no-up" ]] && NO_UP=1
fail() { echo "STAGE-VALIDATE FAIL: $*" >&2; exit 1; }

# ── Stage 0 sub-DAG (matches §19.13.2 Stage 0 + compose depends_on edges):
# postgres is independent; redis-primary is independent; replicas depend on
# primary; sentinels depend on primary+replicas; redis-cache independent;
# clickhouse and nats independent.
ORDER=(postgres redis-primary redis-replica-1 redis-replica-2 \
       redis-sentinel-1 redis-sentinel-2 redis-sentinel-3 redis-cache \
       clickhouse nats-1 nats-2 nats-3)

if [[ $NO_UP -eq 0 ]]; then
  echo "== stage0: bringing infra up in declared order (--wait gates health)"
  for svc in "${ORDER[@]}"; do
    # shellcheck disable=SC2086
    $COMPOSE up -d --wait "$svc" >/dev/null 2>&1 || fail "$svc did not reach running/healthy"
  done
fi

echo "== stage0: health assertions"
for svc in "${ORDER[@]}"; do
  cid=$($COMPOSE ps -q -a "$svc" | head -1)
  [[ -n "$cid" ]] || fail "$svc: no container"
  state=$(docker inspect -f '{{.State.Status}}' "$cid")
  [[ "$state" == "running" ]] || fail "$svc: container state=$state"
  h=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid")
  case "$h" in
    healthy|none) ;;  # healthcheck-less services gate on running only
    *) fail "$svc: health=$h" ;;
  esac
  printf '   %-16s %s/%s\n' "$svc" "$state" "$h"
done

echo "== stage0: dependency-order evidence (replicas/sentinels started after primary)"
primary_start=$(docker inspect -f '{{.State.StartedAt}}' "$($COMPOSE ps -q redis-primary | head -1)")
for svc in redis-replica-1 redis-replica-2 redis-sentinel-1 redis-sentinel-2 redis-sentinel-3; do
  st=$(docker inspect -f '{{.State.StartedAt}}' "$($COMPOSE ps -q "$svc" | head -1)")
  [[ "$st" > "$primary_start" || "$st" == "$primary_start" ]] || \
    fail "$svc StartedAt ($st) precedes redis-primary ($primary_start) — DAG violated"
done

echo "== stage0: sentinel topology (quorum sees master mymaster:6379)"
master=$($COMPOSE exec -T redis-sentinel-1 redis-cli -p 26379 SENTINEL get-master-addr-by-name mymaster 2>/dev/null | head -1 | tr -d '\r')
mport=$($COMPOSE exec -T redis-sentinel-1 redis-cli -p 26379 SENTINEL get-master-addr-by-name mymaster 2>/dev/null | tail -1 | tr -d '\r')
[[ -n "$master" && "$mport" == "6379" ]] || fail "sentinel reports master=$master:$mport"
nsent=$($COMPOSE exec -T redis-sentinel-1 redis-cli -p 26379 SENTINEL sentinels mymaster 2>/dev/null | grep -c '^name$' || true)
[[ "$nsent" -ge 2 ]] || fail "sentinel sees only $nsent peers (<2)"
echo "   sentinel: master=$master:$mport peers=$nsent"

echo "== stages1-5: supervisord.conf structural DAG check"
conf=deploy/supervisord.conf
[[ -f "$conf" ]] || fail "$conf missing"
declare -A want=(
  [ptp4l]=10 [aeronmd]=20 [exchange-watchdogd]=20
  [matching-engine-0]=30
  [aeron-nats-bridge]=40 [oracle-service]=40 [marketdata-service]=40
  [risk-coordinator]=50 [liquidation-scanner]=50 [settlement-service]=50
  [compliance-worker]=50 [banking-rails-worker]=50 [regulatory-reporter]=50
  [analytics-spooler]=50 [status-exporter]=50
  [tomnext-rollover]=55 [proof-of-reserves-builder]=55
  [order-gateway]=60 [fix-gateway]=60
)
for prog in "${!want[@]}"; do
  sec=$(awk -v p="[program:$prog]" '$0==p{f=1} f&&/^priority=/{split($0,a,"=");print a[2];exit} f&&/^\[/&&$0!=p{if(NR>1&&seen)exit} {if(f)seen=1}' "$conf")
  [[ "$sec" == "${want[$prog]}" ]] || fail "$prog priority=$sec want ${want[$prog]} (tier drift vs §19.13.2)"
done
# every long-running stage-1..5 program must autorestart (watchdog emulation)
awk '/^\[program:/{p=$0} /autorestart=|exitcodes=/{print p" "$0}' "$conf" | \
  grep -v 'stage0-infra\|tomnext\|proof-of-reserves' | grep -c 'autorestart=true\|exitcodes=0' | \
  { read -r n; [[ "$n" -ge 16 ]] || fail "only $n programs carry restart supervision"; echo "   $n stage-1..5 programs carry autorestart/one-shot-exit supervision"; }
echo "STAGE-VALIDATE PASS: stage0 healthy+ordered; stages1-5 DAG encoded"
