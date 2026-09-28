#!/usr/bin/env bash
# scripts/data_migration.sh — MONTHLY hot→warm→cold migration driver
# (Phase-09 Task 9.3.24). Runs the tiered lifecycle pass, then the
# retention enforcer in apply mode so drift found after the moves
# (expired purgeable rows, cold-evidence gaps) is corrected and audited.
#
# Order matters: lifecycle first (so the enforcer sees post-move state),
# enforcer apply second, retention dry-run last for a clean report.
#
# Crontab: 0 05 2 * *  /opt/exchange/scripts/data_migration.sh
set -euo pipefail

ARCHIVER_BIN="${ARCHIVER_BIN:-/opt/exchange/bin/archiver}"
RETENTION_BIN="${RETENTION_BIN:-/opt/exchange/bin/retention}"
POLICY="${TIERING_POLICY:-/opt/exchange/infrastructure/data-tiering/tiering_policy.yaml}"
export EXC_POSTGRES_DSN="${EXC_POSTGRES_DSN:?EXC_POSTGRES_DSN required}"
export EXC_S3_ARCHIVE_BUCKET="${EXC_S3_ARCHIVE_BUCKET:-exchange-partition-archive}"

echo "$(date -u +%FT%TZ) data-migration: tier pass"
"$ARCHIVER_BIN" lifecycle --apply --policy="$POLICY" 2>&1

echo "$(date -u +%FT%TZ) data-migration: retention enforce (apply)"
"$RETENTION_BIN" apply --policy="$POLICY" 2>&1 || {
	rc=$?
	# exit 1 = violations reported (audit rows written); propagate
	echo "$(date -u +%FT%TZ) data-migration: enforcer rc=$rc" >&2
	exit $rc
}
echo "$(date -u +%FT%TZ) data-migration: done"
