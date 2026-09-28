#!/usr/bin/env bash
# deploy/crons/pg-partition-archive.sh — NIGHTLY (supersedes the spec
# §19.7 weekly cadence; remediation #35 recorded the nightly choice).
#
# Phase-09 Task 9.3.17: identifies partitions older than the class hot
# cutoff (default 90d), moves them through the hot→warm→cold lifecycle
# (detach → warm schema → csv+zstd WORM S3 → verified drop), honoring
# compliance holds. Fail-closed: any pipeline error exits non-zero so the
# cron wrapper pages.
#
# Crontab: 15 03 * * *  /opt/exchange/deploy/crons/pg-partition-archive.sh
set -euo pipefail

ARCHIVER_BIN="${ARCHIVER_BIN:-/opt/exchange/bin/archiver}"
POLICY="${TIERING_POLICY:-/opt/exchange/infrastructure/data-tiering/tiering_policy.yaml}"
export EXC_POSTGRES_DSN="${EXC_POSTGRES_DSN:?EXC_POSTGRES_DSN required}"
export EXC_S3_ARCHIVE_BUCKET="${EXC_S3_ARCHIVE_BUCKET:-exchange-partition-archive}"

echo "$(date -u +%FT%TZ) partition-archive: starting lifecycle pass"
# Plan first (logged for the audit trail), then apply.
"$ARCHIVER_BIN" lifecycle --policy="$POLICY" 2>&1
"$ARCHIVER_BIN" lifecycle --apply --policy="$POLICY" 2>&1
echo "$(date -u +%FT%TZ) partition-archive: done"
