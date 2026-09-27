#!/usr/bin/env bash
# Task 1.3.5 — IPC round-trip benchmark orchestrator.
#
#   Go writes Event{OrderNew} -> C++ helper echoes Event{TradeFill} -> Go
#   measures RTT. Runs both transports (shm fallback + Aeron preferred) at
#   N=10k and gates on the <50µs p99 end-to-end budget.
#
# Usage:  deploy/scripts/bench_ipc.sh [n] [timeout_ms]
# Exit:   0 both transports within budget; 1 setup failure; 2 bench failure.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CORE="$ROOT/core"
SERVICES="$ROOT/services"
N="${1:-10000}"
TIMEOUT_MS="${2:-60000}"

ECHO_BIN="$(mktemp -u /tmp/ipc_echo.XXXXXX)"
GO_BIN="$(mktemp -u /tmp/ipc_bench_go.XXXXXX)"
AERON_DIR="${AERON_DIR:-/tmp/aeron-exc-bench}"
SHM_BASE="exchange_ipc_bench$$"
MD_PID=""
ECHO_PID=""

cleanup() {
    [ -n "$ECHO_PID" ] && kill "$ECHO_PID" 2>/dev/null || true
    [ -n "$MD_PID" ] && kill "$MD_PID" 2>/dev/null || true
    rm -f "/dev/shm/${SHM_BASE}_0_in" "/dev/shm/${SHM_BASE}_0_out"
    rm -rf "$AERON_DIR"
    rm -f "$ECHO_BIN" "$GO_BIN"
}
trap cleanup EXIT

echo "== bench_ipc: building C++ echo helper =="
g++ -std=c++20 -O2 -DEXCH_WITH_AERON=1 \
    -I"$CORE/include" -I"$CORE/proto/gen" \
    -I"$CORE/third_party/aeron/include/cpp" \
    "$CORE/src/ipc/bench/ipc_echo_main.cpp" \
    "$CORE/src/ipc/SharedMemChannel.cpp" \
    "$CORE/src/ipc/AeronChannel.cpp" \
    "$CORE/src/ipc/IpcChannel.cpp" \
    "$CORE/third_party/aeron/lib/libaeron_client.a" \
    -lpthread -lrt -o "$ECHO_BIN"

echo "== bench_ipc: building Go runner =="
(cd "$SERVICES" && go build -o "$GO_BIN" ./internal/ipc/bench)

fail=0

echo "== bench_ipc: shm transport (n=$N) =="
rm -f "/dev/shm/${SHM_BASE}_0_in" "/dev/shm/${SHM_BASE}_0_out"
"$ECHO_BIN" shm "$SHM_BASE" 0 "$N" "$TIMEOUT_MS" &
ECHO_PID=$!
sleep 0.2   # let the helper create/attach the rings (attach side also tolerates racing)
"$GO_BIN" shm "$SHM_BASE" 0 "$N" "$TIMEOUT_MS" || fail=1
wait "$ECHO_PID" 2>/dev/null || fail=1
ECHO_PID=""

echo "== bench_ipc: aeron transport (n=$N, dir=$AERON_DIR) =="
"$CORE/third_party/aeron/bin/aeronmd" \
    -Daeron.dir="$AERON_DIR" -Daeron.dir.delete.on.start=1 &
MD_PID=$!
for i in $(seq 1 100); do
    [ -f "$AERON_DIR/cnc.dat" ] && break
    sleep 0.1
done
if [ ! -f "$AERON_DIR/cnc.dat" ]; then
    echo "aeronmd did not create cnc.dat" >&2
    exit 1
fi
"$ECHO_BIN" aeron "$AERON_DIR" "$N" "$TIMEOUT_MS" &
ECHO_PID=$!
"$GO_BIN" aeron "$AERON_DIR" "$N" "$TIMEOUT_MS" || fail=1
wait "$ECHO_PID" 2>/dev/null || fail=1
ECHO_PID=""
kill "$MD_PID" 2>/dev/null || true
MD_PID=""

if [ "$fail" -ne 0 ]; then
    echo "bench_ipc: FAIL" >&2
    exit 2
fi
echo "bench_ipc: PASS (both transports within <50us p99 budget)"
