#!/usr/bin/env bash
# deploy/crons/verify-worm-integrity.sh — MONTHLY integrity drill
# (Phase-09 Task 9.3.17 step 5): randomly samples archived partitions,
# re-downloads them, and verifies SHA-256 (archive + CSV), ETag, manifest
# readability and row-count parity. Writes retention_audit_log rows
# (check_name='worm_integrity'); exits non-zero on any corruption so the
# drill pages on-call — a silent drill failure is a compliance defect.
#
# Crontab: 30 04 1 * *  /opt/exchange/deploy/crons/verify-worm-integrity.sh
set -euo pipefail

ARCHIVER_BIN="${ARCHIVER_BIN:-/opt/exchange/bin/archiver}"
SAMPLE="${WORM_DRILL_SAMPLE:-3}"
export EXC_POSTGRES_DSN="${EXC_POSTGRES_DSN:?EXC_POSTGRES_DSN required}"
export EXC_S3_ARCHIVE_BUCKET="${EXC_S3_ARCHIVE_BUCKET:-exchange-partition-archive}"

echo "$(date -u +%FT%TZ) worm-integrity: sampling ${SAMPLE} archives"
"$ARCHIVER_BIN" verify --sample="$SAMPLE" 2>&1
echo "$(date -u +%FT%TZ) worm-integrity: clean"
