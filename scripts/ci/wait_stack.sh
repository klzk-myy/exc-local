#!/usr/bin/env bash
# wait_stack.sh — health gate for the ephemeral CI stack
# (Phase-01.5 Task 1.5.3.1 §3 "health-checked before tests run").
#
# `docker compose up -d --wait` already covers services that declare
# healthchecks (postgres, redis-primary, redis-cache, clickhouse). This
# script adds bounded probes for the services WITHOUT compose healthchecks
# (NATS JetStream cluster, sentinel quorum) plus explicit evidence probes
# for the AC — it never returns before the full topology answers.
#
# Env:
#   COMPOSE_FILE      default docker-compose.dev.yml
#   WAIT_TIMEOUT_S    total budget for ALL probes (default 120) — services
#                     start in parallel, so one shared deadline bounds the gate
#   NATS_MON_PORTS    host-side monitoring ports (default "8222 8223 8224")
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.dev.yml}"
WAIT_TIMEOUT_S="${WAIT_TIMEOUT_S:-120}"
NATS_MON_PORTS="${NATS_MON_PORTS:-8222 8223 8224}"

deadline=$((SECONDS + WAIT_TIMEOUT_S))
remain() { echo $((deadline - SECONDS)); }
expired() { (( SECONDS > deadline )); }

svc() { docker compose -f "$COMPOSE_FILE" ps -q "$1"; }
exec_pg() { docker exec "$(svc postgres)" "$@"; }
exec_redis() { docker exec "$(svc redis-primary)" "$@"; }

echo "wait_stack: probing ephemeral services (${WAIT_TIMEOUT_S}s total budget)"

# --- PostgreSQL 16 (pg_isready inside container; also proves exec path) ------
until exec_pg pg_isready -U exchange -d exchange >/dev/null 2>&1; do
    expired && { echo "wait_stack: postgres not ready" >&2; exit 1; }
    sleep 1
done
echo "wait_stack: postgres ready"

# --- Redis 7 primary ----------------------------------------------------------
until exec_redis redis-cli ping 2>/dev/null | grep -q PONG; do
    expired && { echo "wait_stack: redis-primary not ready" >&2; exit 1; }
    sleep 1
done
echo "wait_stack: redis-primary ready"

# --- Sentinel quorum: ≥2 of 3 sentinels resolve mymaster (spec §4.5 topo) ----
# Probes run inside each sentinel container (its own 26379) — the published
# host ports 36379-81 are for external clients, not container-to-container.
ok=0
until [ "$ok" -ge 2 ]; do
    expired && { echo "wait_stack: sentinel quorum not reached ($ok/2)" >&2; exit 1; }
    ok=0
    for sent in redis-sentinel-1 redis-sentinel-2 redis-sentinel-3; do
        addr="$(docker compose -f "$COMPOSE_FILE" exec -T "$sent" \
            redis-cli -p 26379 sentinel get-master-addr-by-name mymaster 2>/dev/null || true)"
        [ -n "$addr" ] && ok=$((ok + 1))
    done
    sleep 1
done
echo "wait_stack: sentinel quorum ok ($ok/3 resolve mymaster)"

# --- NATS JetStream: all 3 nodes /healthz on their monitoring ports -----------
for port in $NATS_MON_PORTS; do
    until curl -fsS "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; do
        expired && { echo "wait_stack: nats node on :$port not healthy" >&2; exit 1; }
        sleep 1
    done
done
echo "wait_stack: NATS JetStream cluster healthy"

# --- ClickHouse HTTP /ping via the published host port ------------------------
until curl -fsS "http://127.0.0.1:8123/ping" 2>/dev/null | grep -qi 'ok'; do
    expired && { echo "wait_stack: clickhouse /ping failing" >&2; exit 1; }
    sleep 1
done
echo "wait_stack: clickhouse ready"

docker compose -f "$COMPOSE_FILE" ps
echo "wait_stack: all probes green"
