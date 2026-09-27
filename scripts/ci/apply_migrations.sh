#!/usr/bin/env bash
# apply_migrations.sh — apply services/internal/db/migrations/*.up.sql in
# filename order to an ephemeral PostgreSQL 16 container, then verify the
# schema (Phase-01.5 Task 1.5.3.1 §2; spec §5).
#
# Used by .github/workflows/ci.yml jobs `migrations` and `spec-validation`.
# psql runs INSIDE the postgres container (`docker exec -i`); the migrations
# dir is streamed file-by-file so ON_ERROR_STOP kills the run at the first
# bad statement.
#
# Env:
#   PG_CONTAINER     container id/name — default: resolved via
#                    `docker compose -f $COMPOSE_FILE ps -q $PG_SERVICE`
#   COMPOSE_FILE     default docker-compose.dev.yml
#   PG_SERVICE       compose service name (default postgres)
#   PG_USER / PG_DB  default exchange/exchange (compose POSTGRES_USER/DB)
#   READY_TIMEOUT_S  pg_isready budget before failing (default 90)
#   MAX_APPLY_S      hard fail if the apply loop exceeds it — DoD gate
#                    "migrations apply in < 1 min" (default 60)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.dev.yml}"
PG_SERVICE="${PG_SERVICE:-postgres}"
PG_USER="${PG_USER:-exchange}"
PG_DB="${PG_DB:-exchange}"
READY_TIMEOUT_S="${READY_TIMEOUT_S:-90}"
MAX_APPLY_S="${MAX_APPLY_S:-60}"
MIG_DIR="services/internal/db/migrations"

CID="${PG_CONTAINER:-}"
if [ -z "$CID" ]; then
    CID="$(docker compose -f "$COMPOSE_FILE" ps -q "$PG_SERVICE" 2>/dev/null || true)"
fi
[ -n "$CID" ] || { echo "apply_migrations: cannot resolve postgres container" >&2; exit 2; }

# --- wait for readiness (also covers `docker compose up` without --wait) ----
echo "apply_migrations: waiting for postgres (container ${CID:0:12}, ≤${READY_TIMEOUT_S}s)"
t0=$SECONDS
until docker exec "$CID" pg_isready -U "$PG_USER" -d "$PG_DB" >/dev/null 2>&1; do
    if (( SECONDS - t0 > READY_TIMEOUT_S )); then
        echo "apply_migrations: postgres not ready after ${READY_TIMEOUT_S}s — last container log:" >&2
        docker logs --tail 30 "$CID" >&2 || true
        exit 1
    fi
    sleep 1
done
echo "apply_migrations: postgres ready ($((SECONDS - t0))s)"

# --- extension bootstrap (idempotent) ----------------------------------------
# deploy/postgres/Dockerfile normally wires this via docker-entrypoint-initdb.d
# — keep it here too so the script is correct against a hand-started container
# or a re-created database. Requires the image to ship the partman package.
docker exec -i "$CID" psql -v ON_ERROR_STOP=1 -U "$PG_USER" -d "$PG_DB" -c \
    "CREATE EXTENSION IF NOT EXISTS pg_partman; CREATE EXTENSION IF NOT EXISTS pgcrypto;" \
    >/dev/null || { echo "apply_migrations: extension bootstrap failed (pg_partman package missing from image?)" >&2; exit 1; }

# --- apply ------------------------------------------------------------------
mapfile -t UPS < <(ls "$MIG_DIR"/*.up.sql | sort)
[ "${#UPS[@]}" -gt 0 ] || { echo "apply_migrations: no migrations in $MIG_DIR" >&2; exit 1; }

# Foundational corpus gate: exactly 21 contiguous migrations 001..021
# (Phase-01 §deliverables — the canonical count recorded in the phase doc and
# AGENTS.md). A dropped or renumbered file must fail CI, not drift silently.
EXPECTED_MIGRATIONS=21
if [ "${#UPS[@]}" -ne "$EXPECTED_MIGRATIONS" ]; then
    echo "apply_migrations: expected exactly $EXPECTED_MIGRATIONS .up.sql files, found ${#UPS[@]}:" >&2
    printf '  %s\n' "${UPS[@]}" >&2
    exit 1
fi
for i in "${!UPS[@]}"; do
    want=$(printf '%03d' $((i + 1)))
    base="$(basename "${UPS[$i]}")"
    [[ "$base" == "$want"_*.up.sql ]] || {
        echo "apply_migrations: numbering gap — position $((i + 1)) must be ${want}_*.up.sql, got $base" >&2
        exit 1
    }
done
echo "apply_migrations: corpus check ok — $EXPECTED_MIGRATIONS contiguous migrations (001..021)"

t_apply=$SECONDS
for f in "${UPS[@]}"; do
    echo "apply_migrations: $(basename "$f")"
    docker exec -i "$CID" psql -v ON_ERROR_STOP=1 -q -U "$PG_USER" -d "$PG_DB" \
        -o /dev/null -f - < "$f" \
        || { echo "apply_migrations: FAILED applying $f" >&2; exit 1; }
done
ELAPSED=$((SECONDS - t_apply))
echo "apply_migrations: applied ${#UPS[@]} migrations in ${ELAPSED}s"
if (( ELAPSED > MAX_APPLY_S )); then
    echo "apply_migrations: apply took ${ELAPSED}s > ${MAX_APPLY_S}s budget — DoD violation" >&2
    exit 1
fi

# --- verify schema ----------------------------------------------------------
psql_scalar() { docker exec "$CID" psql -tA -U "$PG_USER" -d "$PG_DB" -c "$1"; }

exts="$(psql_scalar "SELECT count(*) FROM pg_extension WHERE extname IN ('pg_partman','pgcrypto')")"
[ "$exts" = "2" ] || { echo "verify: extensions missing (pg_partman/pgcrypto count=$exts)" >&2; exit 1; }

tables="$(psql_scalar "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'
    AND table_name IN ('instruments','users','accounts','balances','orders','trades',
      'funding_transactions','withdrawal_confirmations','audit_hash_chain','admin_audit_log',
      'risk_limits','fee_tiers','margin_accounts','positions','liquidation_auctions',
      'insurance_fund','kyc_documents','nostro_accounts','settlement_instructions',
      'audit_merkle_roots')")"
[ "$tables" = "20" ] || { echo "verify: expected 20 core tables, found $tables" >&2; exit 1; }

partcfg="$(psql_scalar "SELECT count(*) FROM public.part_config WHERE parent_table='public.trades'")"
[ "$partcfg" = "1" ] || { echo "verify: part_config missing trades row (count=$partcfg)" >&2; exit 1; }

parts="$(psql_scalar "SELECT count(*) FROM pg_inherits WHERE inhparent='public.trades'::regclass")"
[ "$parts" -ge 2 ] || { echo "verify: trades partitions=$parts (<2)" >&2; exit 1; }

seeds="$(psql_scalar "SELECT count(*) FROM instruments WHERE status='ACTIVE'")"
[ "$seeds" -ge 8 ] || { echo "verify: seed instruments=$seeds (<8)" >&2; exit 1; }

echo "apply_migrations: schema verified — 20 core tables, pg_partman+pgcrypto, trades partitioned ($parts children), $seeds seed instruments"
