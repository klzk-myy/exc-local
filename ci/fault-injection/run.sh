#!/usr/bin/env bash
# Task 1.5.3.5 — CI entry point for the negative test suite & fault
# injection harness (spec §2.7, §22.7, §24 #298).
#
# Builds the two helper binaries, then runs every scenario and writes
# fault-report.json. Exits non-zero when any scenario fails — wire this
# into CI as a gate. Each scenario is independent and bounded (<60s).
#
# Env knobs:
#   EXC_POSTGRES_DSN   override dev postgres (default matches compose:
#                      postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable)
#   FAULT_ONLY         comma-separated scenario filter (debugging)
#   GOSUMDB/GOPROXY    honored; offline builds work from a warm module cache
set -euo pipefail

cd "$(dirname "$0")"
HERE=$(pwd)
ROOT=$(cd ../.. && pwd)

mkdir -p bin

echo "== building walverify (core WAL harness) =="
g++ -std=c++20 -O2 -Wall -Wextra -I "$ROOT/core/include" \
    cpp/walverify.cpp \
    "$ROOT/core/src/wal/Wal.cpp" \
    "$ROOT/core/src/wal/WalEntry.cpp" \
    "$ROOT/core/src/utils/TimeUtils.cpp" \
    -o bin/walverify

echo "== building exchange CLI (audit verify path) =="
(cd "$ROOT/services" && go build -o "$HERE/bin/exchange" ./cmd/exchange)

echo "== building faultinject harness =="
go build -o bin/faultinject .

echo "== running fault-injection suite =="
args=(-walverify "$HERE/bin/walverify" -exchange "$HERE/bin/exchange" -out "$HERE/fault-report.json")
if [[ -n "${EXC_POSTGRES_DSN:-}" ]]; then
    args+=(-pg-dsn "$EXC_POSTGRES_DSN")
fi
if [[ -n "${FAULT_ONLY:-}" ]]; then
    args+=(-only "$FAULT_ONLY")
fi
args+=("$@")
./bin/faultinject "${args[@]}"
