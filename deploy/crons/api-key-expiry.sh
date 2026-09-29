#!/usr/bin/env bash
# deploy/crons/api-key-expiry.sh — DAILY API-key privilege-expiry sweep
# (Phase-13 Task 13.3.8): keys older than 90 days without an
# ip_allowlist lose their trade/transfer scopes (the key row and its
# remaining scopes survive); holders are warned at T-7d; a key that
# later gains an allowlist has the stripped scopes restored verbatim.
# expiry_override_until (dual-control PUT
# /api/v1/admin/api-keys/{id}/extend-expiry) postpones the deadline.
#
# The pass is idempotent — the gateway also runs it hourly in-process so
# allowlist restores land promptly; this cron is the scheduled-of-record.
#
# Exit codes: 0 pass completed (per-key failures are logged inside the
# pass); non-zero = sweep-level failure — page on it.
#
# Crontab: 15 22 * * *  /opt/exchange/deploy/crons/api-key-expiry.sh
set -euo pipefail

EXCHANGE_BIN="${EXCHANGE_BIN:-/opt/exchange/bin/exchange}"
export EXC_POSTGRES_DSN="${EXC_POSTGRES_DSN:?EXC_POSTGRES_DSN required}"

echo "$(date -u +%FT%TZ) api-key-expiry: sweeping"
"$EXCHANGE_BIN" api-key-expiry-sweep 2>&1
echo "$(date -u +%FT%TZ) api-key-expiry: done"
