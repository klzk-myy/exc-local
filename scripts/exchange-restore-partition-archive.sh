#!/usr/bin/env bash
# =============================================================================
# exchange-restore-partition-archive.sh — Task 4.3.7 restore verification
# drill wrapper (spec §19.7 / §24 #179).
#
# Downloads an archived partition export (zstd CSV + manifest) from the WORM
# bucket, verifies SHA-256 + ETag, recreates the table under a restore
# schema, COPYs rows back, and asserts row-count parity. Read-only w.r.t.
# the live OLTP schema — restores land in --into-schema (default
# archive_restore).
#
# Usage:
#   ./scripts/exchange-restore-partition-archive.sh --partition=trades_p2025_06
#   ... [--into-schema=archive_restore] [--via=exchange|archiver]
#
# Env: EXC_POSTGRES_DSN, EXC_S3_ENDPOINT, EXC_S3_ARCHIVE_BUCKET,
#      EXC_S3_ACCESS_KEY_ID / EXC_S3_SECRET_ACCESS_KEY, EXC_S3_REGION.
# Exit: 0 = restore + parity verified; 1 = any failure (fail closed).
# =============================================================================
set -euo pipefail

PARTITION=""
INTO_SCHEMA="archive_restore"
VIA="exchange"

for arg in "$@"; do
    case "$arg" in
        --partition=*) PARTITION="${arg#*=}" ;;
        --into-schema=*) INTO_SCHEMA="${arg#*=}" ;;
        --via=*) VIA="${arg#*=}" ;;
        -h|--help)
            sed -n '2,20p' "$0"; exit 0 ;;
        *) echo "unknown arg: $arg" >&2; exit 2 ;;
    esac
done

if [[ -z "$PARTITION" ]]; then
    echo "usage: $0 --partition=<name> [--into-schema=<schema>]" >&2
    exit 2
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT/services"

case "$VIA" in
    exchange)
        exec go run ./cmd/exchange restore-partition-archive \
            --partition="$PARTITION" --into-schema="$INTO_SCHEMA"
        ;;
    archiver)
        exec go run ./cmd/archiver restore \
            --partition="$PARTITION" --into-schema="$INTO_SCHEMA"
        ;;
    *)
        echo "bad --via: $VIA (want exchange|archiver)" >&2
        exit 2 ;;
esac
