# tests/chaos/lib/engine.sh — shared chaos-harness helpers (Task 4.5.3.1).
# Sourced by run.sh and every scenarios/*.sh. Expects these globals set by
# run.sh before a scenario runs:
#   RUN_DIR     per-run evidence dir (created)
#   IPC_BASE    unique shm ring base for this run
#   WAL_ROOT    <RUN_DIR>/wal   SNAP_ROOT <RUN_DIR>/snap
#   ENGINE LOADGEN WAL_AUDIT CHAOSTOOL WALRECOVERY — binary paths
#   SHARD INSTRUMENT — engine args
# State exported: ENGINE_PID LOADGEN_PID ENGINE_LOG LOADGEN_LOG.

set -u

ENGINE_PID=0
LOADGEN_PID=0
READY_MARK=0

note() { echo "[$(date '+%H:%M:%S')] $*" | tee -a "$RUN_DIR/events.log" >&2; }

jnum() { # jnum KEY JSON -> first numeric value
    echo "$2" | grep -o "\"$1\"[[:space:]]*:[[:space:]]*-\?[0-9.e+]*" | head -1 | sed 's/.*:[[:space:]]*//'
}
jstr() { # jstr KEY JSON -> first string value
    echo "$2" | grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -1 | sed 's/.*:.*"\(.*\)"/\1/'
}

CHECK_N=0
check() { # check NAME PASS|FAIL DETAIL -> checks.tsv + summary echo
    CHECK_N=$((CHECK_N + 1))
    printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "$RUN_DIR/checks.tsv"
    note "check[$CHECK_N] $1 = $2 ($3)"
    [ "$2" = "PASS" ]
}

shm_clean() {
    # IPC_BASE is unique per run; the glob covers every ring pair the engine
    # may create under it (in/out/snap/snap_ack) plus suffixed secondary
    # bases used by scenarios (follower "${IPC_BASE}f", fixture "${IPC_BASE}x").
    rm -f /dev/shm/"${IPC_BASE}"* 2>/dev/null || true
}

# engine_start [extra args...] — starts matching_engine on this run's dirs.
engine_start() {
    shm_clean
    if [ -f "$ENGINE_LOG" ]; then
        READY_MARK=$(wc -l < "$ENGINE_LOG")
    else
        READY_MARK=0
    fi
    "$ENGINE" -shard "$SHARD" -ipc-base "$IPC_BASE" -wal-dir "$WAL_ROOT" \
        -instrument-id "$INSTRUMENT" -idle-sleep-ns 0 -dev-all-accounts \
        -snap-dir "$SNAP_ROOT" -snapshot-interval-s "${SNAP_INTERVAL_S:-1}" \
        -snapshot-trades "${SNAP_TRADES:-500}" \
        -poison-log "$RUN_DIR/poison_pill.log" \
        -report-log "$RUN_DIR/recovery_report.jsonl" "$@" \
        >> "$ENGINE_LOG" 2>&1 &
    ENGINE_PID=$!
    note "engine start pid=$ENGINE_PID ipc=$IPC_BASE args=$*"
}

# engine_wait_ready [timeout_s] — waits for a NEW "ready at" past READY_MARK.
# Prints nothing; returns 0 ready / 1 timeout-or-dead.
engine_wait_ready() {
    local timeout_s="${1:-30}" waited_ms=0
    local limit=$((timeout_s * 20))
    while [ "$waited_ms" -lt "$limit" ]; do
        if tail -n "+$((READY_MARK + 1))" "$ENGINE_LOG" 2>/dev/null \
            | grep -q "ready at"; then
            return 0
        fi
        kill -0 "$ENGINE_PID" 2>/dev/null || return 1
        sleep 0.05
        waited_ms=$((waited_ms + 1))
    done
    return 1
}

engine_stop() { # SIGTERM, SIGKILL after 5s grace
    local waited=0
    [ "$ENGINE_PID" -gt 0 ] || return 0
    kill -0 "$ENGINE_PID" 2>/dev/null || { wait "$ENGINE_PID" 2>/dev/null; return 0; }
    kill -TERM "$ENGINE_PID" 2>/dev/null
    while kill -0 "$ENGINE_PID" 2>/dev/null && [ "$waited" -lt 50 ]; do
        sleep 0.1; waited=$((waited + 1))
    done
    kill -9 "$ENGINE_PID" 2>/dev/null || true
    wait "$ENGINE_PID" 2>/dev/null || true
    note "engine stopped"
}

# loadgen_start RATE DUR_S REPORT_PATH — fresh order-id base per launch.
LOADGEN_LAUNCH=0
loadgen_start() {
    LOADGEN_LAUNCH=$((LOADGEN_LAUNCH + 1))
    local obase=$(( (LOADGEN_LAUNCH + RUN_IDX * 100) * 1000000000000 + 1 ))
    "$LOADGEN" -base "$IPC_BASE" -shard "$SHARD" -instrument "$INSTRUMENT" \
        -rate "$1" -duration "$2" -metrics-addr ":$METRICS_PORT" \
        -report "$3" -order-id-base "$obase" -accounts 512 \
        -cross-pct "${CROSS_PCT:-15}" -seed "${SEED:-11}" \
        >> "$LOADGEN_LOG" 2>&1 &
    LOADGEN_PID=$!
    note "loadgen start pid=$LOADGEN_PID rate=$1 dur=$2 obase=$obase report=$3"
}

loadgen_stop() {
    local grace="${1:-6}" waited=0
    [ "$LOADGEN_PID" -gt 0 ] || return 0
    kill -0 "$LOADGEN_PID" 2>/dev/null || { wait "$LOADGEN_PID" 2>/dev/null; return 0; }
    kill -TERM "$LOADGEN_PID" 2>/dev/null
    while kill -0 "$LOADGEN_PID" 2>/dev/null && [ "$waited" -lt $((grace * 10)) ]; do
        sleep 0.1; waited=$((waited + 1))
    done
    kill -9 "$LOADGEN_PID" 2>/dev/null || true
    wait "$LOADGEN_PID" 2>/dev/null || true
}

# wal_scan -> JSON on stdout (read-only; safe on a stopped engine, also live)
wal_scan() {
    "$WAL_AUDIT" -wal-dir "$WAL_ROOT/$SHARD" -instrument-id "$INSTRUMENT" \
        -mode scan -json 2>>"$RUN_DIR/logs/wal_audit.err"
}

# wal_fingerprint -> hex via staged private copy (never mutates live dir)
wal_fingerprint() {
    "$WAL_AUDIT" -wal-dir "$WAL_ROOT/$SHARD" -instrument-id "$INSTRUMENT" \
        -mode fingerprint -live 2>>"$RUN_DIR/logs/wal_audit.err" || true
}

# newest snapshot path for the bound instrument ("" if none)
latest_snapshot() {
    ls -t "$SNAP_ROOT/$SHARD/i$INSTRUMENT"/snap_*.bin 2>/dev/null | head -1
}

# Compare two snapshot files' book payload ignoring the seq cursor.
# File = SnapFileHeader(32B) + payload(WalBookSnapshotHeader 24B + records);
# payload.book_seq is at file offset 48..56. Everything else must be
# byte-identical for the books to be provably equal.
# Semantic book equality for snapshot payloads. Replay stamps orders with the
# journal-envelope (timestamp_ns, ingress_seq) — clamped monotone — NOT the
# original arrival stamps, so a snapshot of a replayed book legitimately
# differs in those fields (RecoveryManager.cpp:820-825). Mask them plus the
# book_seq cursor and compare everything else byte-for-byte.
snapshot_book_equal() {
    python3 - "$1" "$2" <<'PYEOF'
import sys, struct
EXT_SZ = 60          # sizeof(WalSnapshotOrderExt): 6x8 + 4 + 3 + 5pad
def norm(p):
    d = open(p, 'rb').read()
    if len(d) < 32:
        return None
    magic, ver, shard, seq, iid, plen, crc, pad = struct.unpack('<IHHIQIII', d[:32])
    if magic != 0x50414E53 or len(d) != 32 + plen:
        return None
    blob = bytearray(d[32:])
    biid, lc, oc, bseq = struct.unpack('<IIQQ', blob[:24])
    blob[16:24] = b'\x00' * 8                    # book_seq cursor
    ext = 24 + lc * 16 + oc * 48 + 16            # skip levels+orders+ext header
    for i in range(oc):
        base = ext + i * EXT_SZ
        blob[base + 32:base + 48] = b'\x00' * 16 # timestamp_ns + ingress_seq
    return bytes(blob)
a, b = norm(sys.argv[1]), norm(sys.argv[2])
sys.exit(0 if (a is not None and b is not None and a == b) else 1)
PYEOF
}
