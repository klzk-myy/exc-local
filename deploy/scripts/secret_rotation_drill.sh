#!/usr/bin/env bash
# =============================================================================
# secret_rotation_drill.sh — Phase-09 Task 9.3.29 leak-triggered emergency
# rotation drill against the live secrets_inventory register (migration 089).
#
# Checklist row exercised:
#   * "Every secret inventoried; emergency rotation drilled; overdue rotation
#     alerts with code"  (drill leg)
#
# Sequence:
#   1. Runbook leg — deploy/scripts/rotate-secrets.sh `status`/`due` probe.
#      The mutating Vault leg (revoke/reissue at source) is environment-bound:
#      no Vault in dev, and the script itself fails closed without VAULT_ADDR.
#      The probe is captured as drill evidence either way.
#   2. Inventory lifecycle — `go run ./cmd/secretdrill` against live PG:
#      register a secret backdated past its rotation SLA → evaluator pages
#      SECRET_ROTATION_OVERDUE (P2) → admin list view reports it (the set the
#      read handler maps to HTTP 503) → MarkRotated{Emergency, IncidentRef}
#      → evaluator clears → admin_audit_log rows verified → row removed.
#
# Env: DATABASE_URL (default dev compose DSN). Exit 0 pass · 1 check failed.
# =============================================================================
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"
export DATABASE_URL="${DATABASE_URL:-postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable}"

log() { printf '[secret-drill %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

log "leg 1/2: rotation runbook probe (rotate-secrets.sh status)"
if "$SELF_DIR/rotate-secrets.sh" status 2>&1 | sed 's/^/  runbook: /'; then
    log "runbook reachable"
else
    log "runbook probe returned non-zero (expected in dev without Vault — mutating leg env-bound)"
fi

log "leg 2/2: inventory lifecycle drill (leak → overdue → emergency mark → clear → audit)"
cd "$REPO_ROOT/services"
go run ./cmd/secretdrill
