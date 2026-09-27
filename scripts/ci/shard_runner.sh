#!/usr/bin/env bash
# shard_runner.sh — spec-validation shard runner (Phase-01.5 Task 1.5.3.1).
#
#   scripts/ci/shard_runner.sh <shard_idx> <num_shards> <report_dir>
#
# Contract (tests/spec/README.md "CI contract"): builds the validator once,
# runs `validator run --shard=<i> --shards=<n> --report=<dir>/shard-<i>.json`
# and propagates its exit code — non-zero when any checkpoint lands in a
# failing status (fail/timeout/error/missing/vanished; pending & dropped are
# reported but never fail).
#
# Endpoint wiring: defaults below match docker-compose.dev.yml published
# ports (and tests/spec/spec.DefaultEnv). CI may override any EXC_* var via
# the job env; nothing here hard-codes beyond the documented defaults.
#
# Env:
#   EXC_REPO_ROOT        auto-detected from this script's location
#   EXC_CORE_BUILD       ctest/gtest dir for core-delegating checkpoints
#                        (default <root>/core/build — restore the CI cmake
#                        cache or build before invoking)
#   EXC_TEST_DSN / EXC_REDIS_TEST_ADDR / EXC_NATS_URLS /
#   EXC_SENTINEL_ADDRS / EXC_CLICKHOUSE_HTTP   live-dependency endpoints
#   SPEC_FAIL_ON_SKIP    default 1 — treat dependency-skips as failures on CI
#                        (stack is health-checked by scripts/ci/wait_stack.sh
#                        first, so a skip can only mean broken infra). Set 0
#                        for local smoke runs without the full stack.
#   VALIDATOR_FLAGS      extra flags forwarded to `validator run`
#                        (e.g. "--timeout=45s --retries=2")
set -euo pipefail

usage() {
    echo "usage: $0 <shard_idx> <num_shards> <report_dir>" >&2
    exit 2
}
[ $# -eq 3 ] || usage

SHARD="$1"; NUM_SHARDS="$2"; REPORT_DIR="$3"
[[ "$SHARD" =~ ^[0-9]+$ ]] || usage
[[ "$NUM_SHARDS" =~ ^[0-9]+$ ]] || usage
[ "$NUM_SHARDS" -eq 4 ] || { echo "shard_runner: canonical shard grid is 4 (spec.NumShards), got $NUM_SHARDS" >&2; exit 2; }
[ "$SHARD" -lt "$NUM_SHARDS" ] || { echo "shard_runner: shard $SHARD out of range for $NUM_SHARDS shards" >&2; exit 2; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export EXC_REPO_ROOT="${EXC_REPO_ROOT:-$ROOT}"
export EXC_CORE_BUILD="${EXC_CORE_BUILD:-$ROOT/core/build}"
export EXC_TEST_DSN="${EXC_TEST_DSN:-postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable}"
export EXC_REDIS_TEST_ADDR="${EXC_REDIS_TEST_ADDR:-127.0.0.1:16379}"
export EXC_NATS_URLS="${EXC_NATS_URLS:-nats://127.0.0.1:4222,nats://127.0.0.1:4223,nats://127.0.0.1:4224}"
export EXC_SENTINEL_ADDRS="${EXC_SENTINEL_ADDRS:-127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381}"
export EXC_CLICKHOUSE_HTTP="${EXC_CLICKHOUSE_HTTP:-http://127.0.0.1:8123}"
SPEC_FAIL_ON_SKIP="${SPEC_FAIL_ON_SKIP:-1}"

mkdir -p "$REPORT_DIR"
REPORT_DIR="$(cd "$REPORT_DIR" && pwd)"   # absolute — validator resolves cwd paths

cd "$ROOT/tests/spec"
go build -o "$REPORT_DIR/validator" .

args=(run "--shard=$SHARD" "--shards=$NUM_SHARDS" "--report=$REPORT_DIR/shard-$SHARD.json")
[ "$SPEC_FAIL_ON_SKIP" = "1" ] && args+=(--fail-on-skip)
if [ -n "${VALIDATOR_FLAGS:-}" ]; then
    # shellcheck disable=SC2206 # intentional word-splitting of extra flags
    args+=(${VALIDATOR_FLAGS})
fi

echo "shard_runner: validator ${args[*]}"
"$REPORT_DIR/validator" "${args[@]}"
