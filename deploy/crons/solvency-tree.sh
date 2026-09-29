#!/usr/bin/env bash
# deploy/crons/solvency-tree.sh — DAILY proof-of-reserves generation
# (Phase-13 Task 13.3.7): builds the salted-leaf Merkle tree over all
# client liabilities, aggregates ACTIVE nostro balances per currency,
# signs the canonical payload and publishes solvency_snapshots +
# solvency_proofs atomically.
#
# Runs at 22:00 UTC — the NY close / FX EOD boundary (spec §24 #186).
# Signing seam (internal/reconciliation/signers.go):
#   EXC_SOLVENCY_SIGNER            gpg | dev-hmac (default: gpg in
#                                    production, dev-hmac elsewhere)
#   EXC_SOLVENCY_GPG_FINGERPRINT   cold-storage key fingerprint —
#                                    REQUIRED when signer=gpg; the
#                                    private key lives on the operator's
#                                    offline keyring/HSM, never here
#   EXC_SOLVENCY_GPG_BIN           gpg binary path (default: gpg)
#   EXC_SOLVENCY_DEV_HMAC_KEY      dev-hmac key (dev/staging only)
#
# Exit codes: 0 published; non-zero = failed run (negative liability,
# signing failure, DB error) — the cron supervisor must page on
# non-zero, a missed attestation is an audit event.
#
# Crontab: 0 22 * * *  /opt/exchange/deploy/crons/solvency-tree.sh
set -euo pipefail

EXCHANGE_BIN="${EXCHANGE_BIN:-/opt/exchange/bin/exchange}"
export EXC_POSTGRES_DSN="${EXC_POSTGRES_DSN:?EXC_POSTGRES_DSN required}"

echo "$(date -u +%FT%TZ) solvency-tree: generating snapshot"
"$EXCHANGE_BIN" solvency-tree 2>&1
echo "$(date -u +%FT%TZ) solvency-tree: published"
