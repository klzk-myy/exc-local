#!/usr/bin/env bash
# =============================================================================
# shard_swap_drill.sh — Phase-09 Task 9.3.16 (spec §19.6 / §24 #177)
# LOCAL evidence run of the 8-step bare-metal shard binary swap:
#
#   drain -> in-flight flush -> SIGTERM (snapshot+WAL fsync) -> binary swap via
#   versioned symlink -> restart -> snapshot+WAL-tail replay -> verify -> resume
#
# What runs FOR REAL here (bare metal, /dev/shm, no containers):
#   * core/build/matching_engine on a scratch ipc-base + WAL dir under $WORK
#   * orders injected through the REAL ingress path: FlatBuffers
#     Event{OrderNew} frames produced onto {base}_{shard}_in by
#     services/cmd/swapdrill (the same ipc.Channel API the gateway uses);
#     {base}_{shard}_out drained continuously for fills/L3/book events
#   * the versioned-symlink binary swap mechanism (staging a second release
#     artifact and flipping the live link, as scripts/deploy/shard-swap.sh
#     does against /opt/exchange/bin/matching-engine)
#   * ingress hold during drain (gateway-equivalent) AND shm-ring buffering
#     proof: a bounded burst is written to the _in ring WHILE the engine
#     process is dead — the ring image persists, the new binary re-attaches
#     and journals every buffered order
#
# What is staging-only (annotated, not faked):
#   * systemd unit stop/start (matching-engine@N) — replaced by SIGTERM +
#     waitpid + direct respawn of the symlinked binary
#   * Redis degradation-mode flag (system:degradation:mode) — replaced by the
#     harness hold-file, same gate semantics at the producer
#   * hugepages / isolcpus / NUMA pinning pre-flight — checked and reported,
#     not enforced on a shared dev host
#   * gateway 503 surfacing — swapdrill counts held/shed writes instead
#
# Assertions (any failure => exit 1, evidence kept):
#   A1 swap window < 3000 ms (SIGTERM -> new binary "ready at")
#   A2 WAL seq continuity: no gaps / overlaps / regressions / corruption
#      (wal_audit -mode scan AND swapdrill audit, independent scanners)
#   A3 zero order loss: every ring-accepted OrderNew is journaled
#      (ORDER_NEW(+EX) rows == sent+burst_accepted; per-id coverage clean)
#   A4 zero duplicate executions: trade_ids unique in TradeFill stream AND
#      WAL; no repeated (order_id,trade_id) L3 Fill leg; no repeated
#      TradeFill engine-seq
#   A5 buffered ingress: burst written while engine dead is journaled
#      post-restart; in-ring occupancy drains to 0 after resume
#   A6 graceful drain evidence: "snapshot stored at seq=" + fresh snapshot
#      file; recovery: line on boot2; no WAL_RECOVERY_HALT; no poison pills
#
# Usage:
#   deploy/scripts/shard_swap_drill.sh [--work-dir DIR] [--rate N]
#       [--pre-s N] [--post-s N] [--burst-n N] [--engine BIN] [--keep]
#
# Exit: 0 all assertions pass · 1 assertion/setup failure · 64 usage.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
ENGINE_SRC="$ROOT/core/build/matching_engine"
WAL_AUDIT="$ROOT/core/build/wal_audit"
WORK=""
RATE=1500; PRE_S=3; POST_S=3; BURST_N=1500
SHARD=0; INSTRUMENT=7
KEEP=0
TIMEOUT_READY_S=10
SWAP_WINDOW_MS_LIMIT=3000

while [ $# -gt 0 ]; do
    case "$1" in
        --work-dir) WORK="$2"; shift ;;
        --engine)   ENGINE_SRC="$2"; shift ;;
        --rate)     RATE="$2"; shift ;;
        --pre-s)    PRE_S="$2"; shift ;;
        --post-s)   POST_S="$2"; shift ;;
        --burst-n)  BURST_N="$2"; shift ;;
        --shard)    SHARD="$2"; shift ;;
        --instrument) INSTRUMENT="$2"; shift ;;
        --keep)     KEEP=1 ;;
        -h|--help)  sed -n '2,55p' "$0"; exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 64 ;;
    esac
    shift
done

for b in "$ENGINE_SRC" "$WAL_AUDIT"; do
    [ -x "$b" ] || { echo "missing binary: $b" >&2; exit 64; }
done
command -v go >/dev/null || { echo "go toolchain required" >&2; exit 64; }
command -v jq >/dev/null || { echo "jq required" >&2; exit 64; }

if [ -z "$WORK" ]; then
    WORK="$(mktemp -d /tmp/exc_swap_drill.XXXXXX)"
fi
mkdir -p "$WORK"/{logs,ctrl,bin,releases,wal,snap}
IPC_BASE="excswap$(date +%s%N | tail -c 9)$$"
WAL_DIR="$WORK/wal"
SNAP_DIR="$WORK/snap"
ENGINE_LOG1="$WORK/logs/engine_gen1.log"
ENGINE_LOG2="$WORK/logs/engine_gen2.log"
DRILL_LOG="$WORK/logs/swapdrill.log"
HOLD="$WORK/ctrl/hold"
BURST="$WORK/ctrl/burst"
STATUS="$WORK/ctrl/status.json"
SENT_IDS="$WORK/sent_ids.txt"
EVENTS="$WORK/logs/events.jsonl"
REPORT="$WORK/swapdrill-report.json"
CHECKS="$WORK/checks.tsv"
: > "$CHECKS"

ENGINE_PID=0; DRILL_PID=0
FAILS=0

log() { printf '[drill %s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*" | tee -a "$WORK/drill.log"; }
check() { # check NAME PASS|FAIL detail
    printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "$CHECKS"
    if [ "$2" = "PASS" ]; then
        log "  check $1: PASS — $3"
    else
        log "  check $1: FAIL — $3"
        FAILS=$((FAILS + 1))
    fi
}
now_ns() { date +%s%N; }

cleanup() {
    [ "$DRILL_PID" -gt 0 ] && kill -TERM "$DRILL_PID" 2>/dev/null || true
    [ "$ENGINE_PID" -gt 0 ] && kill -TERM "$ENGINE_PID" 2>/dev/null || true
    sleep 0.3
    [ "$DRILL_PID" -gt 0 ] && kill -9 "$DRILL_PID" 2>/dev/null || true
    [ "$ENGINE_PID" -gt 0 ] && kill -9 "$ENGINE_PID" 2>/dev/null || true
    rm -f /dev/shm/"${IPC_BASE}"* 2>/dev/null || true
}
trap cleanup EXIT

jget() { jq -r "$1" "${2:-/dev/stdin}" 2>/dev/null || echo "null"; }

# status-field poll: wait until status.json field hits target
wait_status() { # field value timeout_s -> 0 on hit
    local field="$1" want="$2" limit_ms=$(( ${3:-5} * 1000 )) waited=0
    while [ "$waited" -lt "$limit_ms" ]; do
        v="$(jget ".$field" "$STATUS")"
        [ "$v" = "$want" ] && return 0
        sleep 0.05; waited=$((waited + 50))
    done
    return 1
}

wait_ready_line() { # logfile mark timeout_s -> 0 when new "ready at" lands
    local f="$1" mark="$2" limit_ms=$(( ${3:-$TIMEOUT_READY_S} * 1000 )) waited=0
    while [ "$waited" -lt "$limit_ms" ]; do
        tail -n "+$((mark + 1))" "$f" 2>/dev/null | grep -q "ready at" && return 0
        sleep 0.02; waited=$((waited + 20))
    done
    return 1
}

log "== shard swap drill — spec §19.6 8-step local execution =="
log "work=$WORK ipc_base=$IPC_BASE shard=$SHARD instrument=$INSTRUMENT"
log "engine_src=$ENGINE_SRC"

# ---------------------------------------------------------------------------
# Build the gateway-side harness (services/cmd/swapdrill).
# ---------------------------------------------------------------------------
log "building swapdrill harness"
(cd "$ROOT/services" && go build -o "$WORK/swapdrill" ./cmd/swapdrill) \
    || { echo "swapdrill build failed" >&2; exit 1; }

# ---------------------------------------------------------------------------
# STEP 1 — pre-flight (spec §19.6 step 1; adapted: symlink layout under $WORK)
# ---------------------------------------------------------------------------
log "step 1/8 pre-flight verification"
SHA_A="$(sha256sum "$ENGINE_SRC" | awk '{print $1}')"
REL_A="$WORK/releases/matching-engine-$SHA_A"
cp "$ENGINE_SRC" "$REL_A"
# The "new" release: identical code plus an appended provenance trailer.
# Appending bytes past the ELF image leaves execution identical but yields a
# distinct sha256 + distinct release path — exercising the real symlink-flip
# mechanism (a same-version re-deploy; a differing build needs no further
# distinction at this layer).
REL_B="$WORK/releases/matching-engine-swap-$(printf '%s' "$SHA_A" | head -c 12)"
cp "$ENGINE_SRC" "$REL_B"
printf '\n# swapped release marker: swap drill %s source sha256=%s\n' \
    "$(date -u +%FT%TZ)" "$SHA_A" >> "$REL_B"
SHA_B="$(sha256sum "$REL_B" | awk '{print $1}')"
chmod 0755 "$REL_A" "$REL_B"
[ "$SHA_A" != "$SHA_B" ] \
    && check preflight_distinct_release PASS "old sha256=$SHA_A new sha256=$SHA_B" \
    || check preflight_distinct_release FAIL "releases identical — flip would be unverifiable"
if command -v readelf >/dev/null; then
    readelf -h "$REL_B" >/dev/null 2>&1 \
        && check preflight_symbols PASS "valid ELF executable on new release" \
        || check preflight_symbols FAIL "new release is not a readable ELF"
fi
mkdir -p "$WORK/bin"
ln -sfn "$REL_A" "$WORK/bin/matching-engine"
HP="$(cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages 2>/dev/null || echo 0)"
ISO="$(cat /sys/devices/system/cpu/isolated 2>/dev/null || echo "")"
log "  informational (staging-gated in shard-swap.sh): nr_hugepages=$HP isolcpus='${ISO:-none}'"
[ -x "$REL_A" ] && check preflight_exec PASS "release artifacts executable"

start_engine() { # logfile — via the live symlink, like the systemd unit would
    "$WORK/bin/matching-engine" -shard "$SHARD" -ipc-base "$IPC_BASE" \
        -wal-dir "$WAL_DIR" -snap-dir "$SNAP_DIR" -instrument-id "$INSTRUMENT" \
        -idle-sleep-ns 0 -dev-all-accounts \
        -snapshot-interval-s 1 -snapshot-trades 500 \
        -poison-log "$WORK/logs/poison_pill.log" \
        -report-log "$WORK/logs/recovery_report.jsonl" \
        >> "$1" 2>&1 &
    ENGINE_PID=$!
}

# scratch rings — unique ipc base; clean only before the FIRST boot
rm -f /dev/shm/"${IPC_BASE}"* 2>/dev/null || true
start_engine "$ENGINE_LOG1"
wait_ready_line "$ENGINE_LOG1" 0 15 \
    && check boot1_ready PASS "generation-1 engine ready (pid $ENGINE_PID)" \
    || { check boot1_ready FAIL "engine did not print 'ready at'"; exit 1; }

# ---------------------------------------------------------------------------
# Baseline load through the real ingress path.
# ---------------------------------------------------------------------------
"$WORK/swapdrill" drive -base "$IPC_BASE" -shard "$SHARD" \
    -instrument "$INSTRUMENT" -rate "$RATE" -cross-pct 15 -accounts 512 \
    -seed 11 -order-id-base 1000000 \
    -hold-file "$HOLD" -burst-file "$BURST" -burst-n "$BURST_N" \
    -sent-ids "$SENT_IDS" -events "$EVENTS" -status "$STATUS" \
    -report "$REPORT" >> "$DRILL_LOG" 2>&1 &
DRILL_PID=$!
wait_status sent 1 5 >/dev/null && sleep 0.3
log "loadgen running (pid $DRILL_PID); baseline ${PRE_S}s @ ${RATE}/s"
sleep "$PRE_S"
PRE_SENT="$(jget .sent "$STATUS")"
PRE_IN_OCC="$(jget .in_occupancy "$STATUS")"
log "baseline: sent=$PRE_SENT in_occupancy=$PRE_IN_OCC"
[ "${PRE_SENT:-0}" -gt 0 ] \
    && check baseline_ingress PASS "$PRE_SENT orders accepted into _in ring" \
    || check baseline_ingress FAIL "no orders flowed"

# ---------------------------------------------------------------------------
# STEP 2 — ingress drain: hold new-order sends (gateway Maintenance hold).
# ---------------------------------------------------------------------------
log "step 2/8 ingress drain — hold-file raised (gateway 503 equivalent)"
touch "$HOLD"

# ---------------------------------------------------------------------------
# STEP 3 — in-flight flush: _in ring occupancy -> 0 (spec §19.6 <= 5s bound)
# ---------------------------------------------------------------------------
FLUSH_T0=$(now_ns)
if wait_status in_occupancy 0 5; then
    FLUSH_MS=$(( ($(now_ns) - FLUSH_T0) / 1000000 ))
    check inflight_flush PASS "_in ring drained in ${FLUSH_MS}ms (<=5000ms bound)"
else
    check inflight_flush FAIL "_in occupancy still >0 after 5s"
fi

# ---------------------------------------------------------------------------
# STEP 4 — snapshot + graceful stop: SIGTERM drives force_snapshot + WAL fsync
# ---------------------------------------------------------------------------
log "step 4/8 SIGTERM -> drain snapshot + WAL fsync"
SNAP_T0=$(now_ns)
SWAP_T0="$SNAP_T0"   # swap window measured drain-start(SIGTERM) -> accepting
kill -TERM "$ENGINE_PID"
TERM_OK=1
for _ in $(seq 1 100); do
    kill -0 "$ENGINE_PID" 2>/dev/null || { TERM_OK=0; break; }
    sleep 0.05
done
wait "$ENGINE_PID" 2>/dev/null || true
ENGINE_PID=0
[ "$TERM_OK" = "0" ] \
    && check sigterm_exit PASS "engine exited on SIGTERM (<=5s)" \
    || check sigterm_exit FAIL "engine still running 5s after SIGTERM"
grep -q "snapshot stored at seq=" "$ENGINE_LOG1" \
    && SNAP_LINE="$(grep 'snapshot stored at seq=' "$ENGINE_LOG1" | tail -1)" \
    && check drain_snapshot PASS "$SNAP_LINE" \
    || check drain_snapshot FAIL "no drain snapshot line in gen-1 log"
LATEST_SNAP="$(ls -t "$SNAP_DIR/$SHARD"/i"$INSTRUMENT"/snap_*.bin 2>/dev/null | head -1 || true)"
[ -n "$LATEST_SNAP" ] \
    && check drain_snapshot_file PASS "$LATEST_SNAP" \
    || check drain_snapshot_file FAIL "no snapshot file under $SNAP_DIR"

# WAL state at the moment of the swap (independent C++ auditor)
PRE_SCAN="$WORK/wal_scan_pre.json"
"$WAL_AUDIT" -wal-dir "$WAL_DIR/$SHARD" -instrument-id "$INSTRUMENT" \
    -mode scan -json > "$PRE_SCAN" 2>>"$WORK/logs/wal_audit.err" || true
PRE_TAIL="$(jget .wal_tail "$PRE_SCAN")"
PRE_ENTRIES="$(jget .entries "$PRE_SCAN")"
PRE_ORDERNEW=$(( $(jget '.types.ORDER_NEW // 0' "$PRE_SCAN") + $(jget '.types.ORDER_NEW_EX // 0' "$PRE_SCAN") ))
log "pre-swap WAL: tail=$PRE_TAIL entries=$PRE_ENTRIES order_new=$PRE_ORDERNEW"

# ---------------------------------------------------------------------------
# STEP 5 — binary swap: versioned symlink flip (same mechanism as prod)
# ---------------------------------------------------------------------------
log "step 5/8 binary swap — symlink flip to $REL_B"
ln -sfn "$REL_B" "$WORK/bin/matching-engine"
LINK_TGT="$(readlink -f "$WORK/bin/matching-engine")"
[ "$LINK_TGT" = "$REL_B" ] \
    && check symlink_flip PASS "matching-engine -> $(basename "$REL_B")" \
    || check symlink_flip FAIL "link target $LINK_TGT != $REL_B"

# Ingress buffering proof: with the engine process DEAD, fire the bounded
# burst — writes land in the persistent /dev/shm _in ring (SPSC images are
# never re-initialized on attach; spec §19.6 swap window).
log "  firing buffered-ingress burst (burst-n=$BURST_N) while engine is down"
: > "$BURST"   # mtime trigger — harness fires burst-n writes immediately
# The burst writer needs a beat to land burst-n records; poll the status file
# instead of a single mid-flight read (first sample raced the write loop).
MID_BACC=""
for _i in $(seq 1 40); do
    MID_BACC="$(jget .burst_accepted "$STATUS")"
    [ "${MID_BACC:-0}" -gt 0 ] 2>/dev/null && break
    sleep 0.25
done
MID_OCC="$(jget .in_occupancy "$STATUS")"
MID_ALIVE="$(jget .producer_alive "$STATUS")"
log "  during-window: in_occupancy=$MID_OCC burst_accepted=$MID_BACC producer_alive=$MID_ALIVE"
[ "${MID_ALIVE}" = "false" ] \
    && check burst_while_dead PASS "burst written with engine producer dead" \
    || check burst_while_dead FAIL "engine producer still alive during burst"
[ "${MID_BACC:-0}" -gt 0 ] \
    && check ring_buffered PASS "$MID_BACC orders buffered in shm ring during window" \
    || check ring_buffered FAIL "no buffered writes accepted"

# ---------------------------------------------------------------------------
# STEP 6 — warm state reload: new binary attaches, replays snapshot + WAL tail
# ---------------------------------------------------------------------------
log "step 6/8 start swapped binary — snapshot + WAL tail replay"
start_engine "$ENGINE_LOG2"
if wait_ready_line "$ENGINE_LOG2" 0 15; then
    SWAP_MS=$(( ($(now_ns) - SWAP_T0) / 1000000 ))
else
    SWAP_MS=-1
fi
REC_LINE="$(grep 'recovery:' "$ENGINE_LOG2" | tail -1 || true)"
log "  recovery: ${REC_LINE:-<none>}"
[ "$SWAP_MS" -ge 0 ] \
    && check gen2_ready PASS "generation-2 ready in ${SWAP_MS}ms since SIGTERM" \
    || check gen2_ready FAIL "new binary never reached 'ready at'"
if [ "$SWAP_MS" -ge 0 ] && [ "$SWAP_MS" -lt "$SWAP_WINDOW_MS_LIMIT" ]; then
    check swap_window_lt_3s PASS "${SWAP_MS}ms < ${SWAP_WINDOW_MS_LIMIT}ms"
else
    check swap_window_lt_3s FAIL "${SWAP_MS}ms >= ${SWAP_WINDOW_MS_LIMIT}ms (or never ready)"
fi
[ -n "$REC_LINE" ] \
    && check wal_replay PASS "$REC_LINE" \
    || check wal_replay FAIL "no 'recovery:' line in gen-2 log"
grep -q "WAL_RECOVERY_HALT" "$ENGINE_LOG2" \
    && check no_recovery_halt FAIL "WAL_RECOVERY_HALT in gen-2 log" \
    || check no_recovery_halt PASS "no WAL_RECOVERY_HALT"
REC_TAIL="$(printf '%s' "$REC_LINE" | grep -o 'tail=[0-9]*' | tail -1 | cut -d= -f2)"
if [ -n "$REC_TAIL" ] && [ -n "$PRE_TAIL" ] && [ "$REC_TAIL" != "null" ] \
        && [ "$PRE_TAIL" != "null" ] && [ "$REC_TAIL" -ge "$PRE_TAIL" ]; then
    check tail_monotonic PASS "recovery tail $REC_TAIL >= pre-swap tail $PRE_TAIL"
else
    check tail_monotonic FAIL "recovery tail $REC_TAIL vs pre-swap tail $PRE_TAIL"
fi

# Buffered orders must drain through the new binary.
if wait_status in_occupancy 0 10; then
    check buffered_drain PASS "buffered burst consumed post-restart (_in -> 0)"
else
    check buffered_drain FAIL "_in occupancy stuck after restart"
fi

# ---------------------------------------------------------------------------
# STEP 7 — verification & health check (local stand-in for verify-shard.sh)
# ---------------------------------------------------------------------------
log "step 7/8 verification"
kill -0 "$ENGINE_PID" 2>/dev/null \
    && check gen2_alive PASS "engine pid $ENGINE_PID running swapped binary" \
    || check gen2_alive FAIL "engine not running"
LIVE_EXE="$(readlink -f "/proc/$ENGINE_PID/exe" 2>/dev/null || echo '?')"
[ "$LIVE_EXE" = "$(readlink -f "$REL_B")" ] \
    && check running_new_binary PASS "proc exe = $LIVE_EXE" \
    || check running_new_binary FAIL "proc exe $LIVE_EXE"
RINGS="$(find /dev/shm -maxdepth 1 -name "${IPC_BASE}*" | wc -l)"
[ "$RINGS" -ge 2 ] \
    && check shm_rings PASS "$RINGS shm objects under /dev/shm" \
    || check shm_rings FAIL "rings missing"
POISON="$(wc -l < "$WORK/logs/poison_pill.log" 2>/dev/null || echo 0)"
[ "${POISON:-0}" -eq 0 ] \
    && check poison_free PASS "poison_pill.log empty" \
    || check poison_free FAIL "$POISON poisoned frames"

# ---------------------------------------------------------------------------
# STEP 8 — traffic unpause: drop the hold, run the post window.
# ---------------------------------------------------------------------------
log "step 8/8 unpause — resuming ingress for ${POST_S}s"
rm -f "$HOLD"
sleep "$POST_S"
POST_SENT="$(jget .sent "$STATUS")"
POST_FILL="$(jget .out_occupancy "$STATUS")"
log "post-window: sent total=$POST_SENT out_occupancy=$POST_FILL"
[ "${POST_SENT:-0}" -gt "${PRE_SENT:-0}" ] \
    && check resume_ingress PASS "production resumed ($PRE_SENT -> $POST_SENT)" \
    || check resume_ingress FAIL "no post-swap sends"

# ---------------------------------------------------------------------------
# Teardown + assertions
# ---------------------------------------------------------------------------
log "stopping harness + engine for final audit"
kill -TERM "$DRILL_PID" 2>/dev/null || true
for _ in $(seq 1 200); do
    kill -0 "$DRILL_PID" 2>/dev/null || break
    sleep 0.05
done
wait "$DRILL_PID" 2>/dev/null || true
DRILL_PID=0
[ -f "$REPORT" ] || { check harness_report FAIL "no swapdrill report"; exit 1; }

if [ "$ENGINE_PID" -gt 0 ]; then
    kill -TERM "$ENGINE_PID" 2>/dev/null || true
    for _ in $(seq 1 100); do
        kill -0 "$ENGINE_PID" 2>/dev/null || break
        sleep 0.05
    done
    wait "$ENGINE_PID" 2>/dev/null || true
    ENGINE_PID=0
fi

# --- Journal-level audit -----------------------------------------------------
POST_SCAN="$WORK/wal_scan_post.json"
"$WAL_AUDIT" -wal-dir "$WAL_DIR/$SHARD" -instrument-id "$INSTRUMENT" \
    -mode scan -json > "$POST_SCAN" 2>>"$WORK/logs/wal_audit.err" || true
AUDIT_JSON="$WORK/wal_audit_coverage.json"
AUDIT_RC=0
"$WORK/swapdrill" audit -wal-dir "$WAL_DIR/$SHARD" \
    -expect-ids "$SENT_IDS" > "$AUDIT_JSON" || AUDIT_RC=$?

R_ACCEPTED="$(jget .ring_accepted_total "$REPORT")"
W_ORDERNEW=$(( $(jget '.by_type.ORDER_NEW // 0' "$AUDIT_JSON") + $(jget '.by_type.ORDER_NEW_EX // 0' "$AUDIT_JSON") ))
W_GAPS="$(jget '.seq_gaps | length' "$AUDIT_JSON")"
W_OVERLAPS="$(jget '.seq_overlaps | length' "$AUDIT_JSON")"
W_CORRUPT="$(jget .corrupt_segments "$AUDIT_JSON")"
W_MISS="$(jget .missing_order_count "$AUDIT_JSON")"
W_DUPT="$(jget .dup_trade_ids "$AUDIT_JSON")"
W_TRADES="$(jget .journaled_trade_ids "$AUDIT_JSON")"
A_GAPS="$(jget .seq_gaps "$POST_SCAN")"
A_REGR="$(jget .seq_regressions "$POST_SCAN")"
A_DUPT="$(jget .dup_trade_ids "$POST_SCAN")"
A_OK="$(jget .ok "$POST_SCAN")"

H_SENT="$(jget .orders_sent "$REPORT")"
H_BACC="$(jget .burst_accepted "$REPORT")"
H_BDEAD="$(jget .burst_while_engine_dead "$REPORT")"
H_SHED="$(jget .send_shed "$REPORT")"
H_BSHED="$(jget .burst_shed "$REPORT")"
H_FILLS="$(jget .fills "$REPORT")"
H_DUPT="$(jget .dup_trade_ids "$REPORT")"
H_DUPL3="$(jget .dup_l3_fills "$REPORT")"
H_DUPFS="$(jget .dup_fill_seqs "$REPORT")"
H_L3GAPS="$(jget .l3_seq_gaps "$REPORT")"
H_L3RESET="$(jget .l3_seq_resets "$REPORT")"
H_L3ADD="$(jget .l3_adds "$REPORT")"
H_OUTDROP="$(jget .out_ring_drops "$REPORT")"
H_DECODE="$(jget .decode_errors "$REPORT")"
H_SEEN="$(jget .orders_seen_outbound "$REPORT")"

log "counts: ring_accepted=$R_ACCEPTED (sent=$H_SENT burst=$H_BACC) wal_order_new=$W_ORDERNEW"
log "journal: gaps=$W_GAPS overlaps=$W_OVERLAPS corrupt=$W_CORRUPT missing_ids=$W_MISS wal_dup_tids=$W_DUPT"
log "stream : fills=$H_FILLS wal_trades=$W_TRADES dup_tid=$H_DUPT dup_l3fill=$H_DUPL3 dup_fillseq=$H_DUPFS l3_gaps=$H_L3GAPS l3_resets=$H_L3RESET out_drops=$H_OUTDROP"

# A2 — seq continuity, both scanners
if [ "$A_OK" = "true" ] && [ "${A_GAPS:-1}" = "0" ] && [ "${A_REGR:-1}" = "0" ] \
        && [ "$W_GAPS" = "0" ] && [ "$W_OVERLAPS" = "0" ] && [ "$W_CORRUPT" = "0" ]; then
    check wal_seq_continuity PASS "no gaps/overlaps/corruption (wal_audit + swapdrill audit agree)"
else
    check wal_seq_continuity FAIL "wal_audit: ok=$A_OK gaps=$A_GAPS regr=$A_REGR; audit: gaps=$W_GAPS overlaps=$W_OVERLAPS corrupt=$W_CORRUPT"
fi

# A3 — zero order loss: every ring-accepted OrderNew journaled, per-id coverage
if [ "$R_ACCEPTED" = "$W_ORDERNEW" ] && [ "$W_MISS" = "0" ] && [ "$AUDIT_RC" = "0" ]; then
    check zero_order_loss PASS "ring-accepted $R_ACCEPTED == journaled ORDER_NEW $W_ORDERNEW, 0 missing ids"
else
    check zero_order_loss FAIL "accepted=$R_ACCEPTED journaled=$W_ORDERNEW missing=$W_MISS audit_rc=$AUDIT_RC"
fi

# A4 — zero duplicate executions
if [ "$H_DUPT" = "0" ] && [ "$H_DUPL3" = "0" ] && [ "$H_DUPFS" = "0" ] \
        && [ "$W_DUPT" = "0" ] && [ "${A_DUPT:-1}" = "0" ]; then
    check zero_dup_executions PASS "trade_ids unique in stream+WAL; no dup L3 fill legs; no dup engine seqs"
else
    check zero_dup_executions FAIL "dup_tid=$H_DUPT dup_l3=$H_DUPL3 dup_fseq=$H_DUPFS wal_dup=$W_DUPT audit_dup=$A_DUPT"
fi

# A5 — buffered ingress journaled post-restart
if [ "${H_BDEAD:-0}" -gt 0 ]; then
    check ingress_buffered_proof PASS "$H_BDEAD orders accepted while engine dead, all journaled (see zero_order_loss)"
else
    check ingress_buffered_proof FAIL "no orders were buffered while engine dead"
fi

# L3 stream sanity: per-generation contiguity; resets expected == 1 (restart)
if [ "$H_L3GAPS" = "0" ] && [ "${H_L3RESET:-0}" -le 1 ] && [ "$H_OUTDROP" = "0" ]; then
    check l3_stream_integrity PASS "0 gaps, $H_L3RESET generation reset(s), 0 outbound drops"
else
    check l3_stream_integrity FAIL "l3_gaps=$H_L3GAPS resets=$H_L3RESET out_drops=$H_OUTDROP"
fi
# L3 adds vs journaled orders — every accepted order emits exactly one Add
if [ "$H_OUTDROP" = "0" ] && [ "$H_L3ADD" = "$W_ORDERNEW" ]; then
    check l3_parity PASS "l3_adds=$H_L3ADD == journaled orders=$W_ORDERNEW"
else
    check l3_parity FAIL "l3_adds=$H_L3ADD vs journaled=$W_ORDERNEW (out_drops=$H_OUTDROP)"
fi
# TradeFill stream == WAL TRADE rows (parity when nothing dropped)
if [ "$H_OUTDROP" = "0" ] && [ "$H_FILLS" = "$W_TRADES" ]; then
    check fill_parity PASS "fills observed=$H_FILLS == journaled TRADE rows=$W_TRADES"
else
    check fill_parity FAIL "fills=$H_FILLS vs wal_trades=$W_TRADES (out_drops=$H_OUTDROP)"
fi
[ "$H_DECODE" = "0" ] && [ "$H_SHED" = "0" ] \
    && check harness_clean PASS "decode_errors=0 send_shed=0 burst_shed=$H_BSHED held_slots=$(jget .held_paced_slots "$REPORT")" \
    || check harness_clean FAIL "decode_errors=$H_DECODE send_shed=$H_SHED"

# --- verdict ------------------------------------------------------------------
VERDICT="$WORK/verdict.json"
jq -n \
    --arg task "Phase-09 Task 9.3.16" --arg spec "§19.6 / §24 #177" \
    --arg work "$WORK" --arg ipc "$IPC_BASE" \
    --argjson swap_ms "${SWAP_MS:-0}" \
    --argjson accepted "${R_ACCEPTED:-0}" --argjson journaled "${W_ORDERNEW:-0}" \
    --argjson fills "${H_FILLS:-0}" --argjson wal_trades "${W_TRADES:-0}" \
    --argjson missing "${W_MISS:-0}" --argjson dup "${H_DUPT:-0}" \
    --argjson gaps "${W_GAPS:-0}" --argjson fails "$FAILS" \
    --argjson burst_dead "${H_BDEAD:-0}" \
    '{task:$task,spec:$spec,work_dir:$work,ipc_base:$ipc,
      swap_window_ms:$swap_ms,ring_accepted:$accepted,journaled_order_new:$journaled,
      buffered_while_dead:$burst_dead,fills:$fills,wal_trades:$wal_trades,
      missing_orders:$missing,dup_executions:$dup,seq_gaps:$gaps,
      failed_checks:$fails, pass:($fails==0)}' > "$VERDICT"

echo
log "=== verdict: $([ "$FAILS" -eq 0 ] && echo PASS || echo "FAIL ($FAILS checks)") ==="
log "swap window: ${SWAP_MS}ms | accepted=$R_ACCEPTED journaled=$W_ORDERNEW missing=$W_MISS"
log "dups: trade_ids=$H_DUPT l3_fills=$H_DUPL3 wal_dup=$W_DUPT | seq gaps=$W_GAPS overlaps=$W_OVERLAPS"
log "evidence: $WORK (drill.log, checks.tsv, verdict.json, events.jsonl, wal scans, engine logs)"
column -t -s$'\t' "$CHECKS" 2>/dev/null || cat "$CHECKS"

if [ "$KEEP" = "0" ] && [ "$FAILS" -eq 0 ]; then
    log "PASS — keeping evidence dir anyway (work-dir not auto-removed; rm -rf to clean)"
fi
[ "$FAILS" -eq 0 ]
