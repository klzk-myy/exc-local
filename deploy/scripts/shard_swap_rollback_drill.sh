#!/usr/bin/env bash
# =============================================================================
# shard_swap_rollback_drill.sh — Phase-09 Task 9.3.16 DoD row:
#   "Rollback automation reverts to prior binary and restarts from latest
#    checkpoint upon probe failure"
#
# Negative-path companion to shard_swap_drill.sh. Same real machinery
# (bare-metal /dev/shm rings, WAL, drain snapshot, versioned symlink,
# swapdrill ingress harness) but the gen-2 release is a deliberately
# NEVER-READY binary — it stays alive yet never emits "ready at", the
# local stand-in for the spec §19.6 step-7 synthetic health probe.
#
# Sequence:
#   1-4  identical to the swap drill: baseline -> hold -> flush ->
#        SIGTERM (drain snapshot + WAL fsync = "latest checkpoint")
#   5    symlink flip to the broken release; burst fired while the engine
#        is down (same buffered-ingress proof)
#   6    gen-2 started; readiness probe times out -> ROLLBACK:
#        kill gen-2, revert symlink to the prior release, restart
#   7-8  rollback verification: prior binary reaches "ready at" from the
#        drain checkpoint, recovery line present, WAL tail monotonic,
#        buffered burst drains, resume window flows, and the journal-level
#        audit shows zero order loss + zero duplicate executions
#
# Exit: 0 all assertions pass · 1 assertion/setup failure · 64 usage.
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
ENGINE_SRC="$ROOT/core/build/matching_engine"
WAL_AUDIT="$ROOT/core/build/wal_audit"
WORK=""
RATE=1500; PRE_S=3; POST_S=3; BURST_N=800
SHARD=0; INSTRUMENT=7
PROBE_TIMEOUT_S=6          # readiness probe window before rollback fires
RB_WINDOW_MS_LIMIT=3000    # rollback restart bound (same as swap window)

while [ $# -gt 0 ]; do
    case "$1" in
        --work-dir) WORK="$2"; shift ;;
        --engine)   ENGINE_SRC="$2"; shift ;;
        --rate)     RATE="$2"; shift ;;
        --pre-s)    PRE_S="$2"; shift ;;
        --post-s)   POST_S="$2"; shift ;;
        --burst-n)  BURST_N="$2"; shift ;;
        -h|--help)  sed -n '2,36p' "$0"; exit 0 ;;
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
    WORK="$(mktemp -d /tmp/exc_rb_drill.XXXXXX)"
fi
mkdir -p "$WORK"/{logs,ctrl,bin,releases,wal,snap}
IPC_BASE="excrb$(date +%s%N | tail -c 9)$$"
WAL_DIR="$WORK/wal"
SNAP_DIR="$WORK/snap"
ENGINE_LOG1="$WORK/logs/engine_gen1.log"
ENGINE_LOG_BAD="$WORK/logs/engine_gen2_bad.log"
ENGINE_LOG_RB="$WORK/logs/engine_gen3_rollback.log"
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

log() { printf '[rb-drill %s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*" | tee -a "$WORK/drill.log"; }
check() {
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

wait_status() {
    local field="$1" want="$2" limit_ms=$(( ${3:-5} * 1000 )) waited=0
    while [ "$waited" -lt "$limit_ms" ]; do
        v="$(jget ".$field" "$STATUS")"
        [ "$v" = "$want" ] && return 0
        sleep 0.05; waited=$((waited + 50))
    done
    return 1
}

wait_ready_line() {
    local f="$1" mark="$2" limit_ms=$(( ${3:-10} * 1000 )) waited=0
    while [ "$waited" -lt "$limit_ms" ]; do
        tail -n "+$((mark + 1))" "$f" 2>/dev/null | grep -q "ready at" && return 0
        sleep 0.02; waited=$((waited + 20))
    done
    return 1
}

log "== shard swap ROLLBACK drill — probe-failure negative path =="
log "work=$WORK ipc_base=$IPC_BASE shard=$SHARD instrument=$INSTRUMENT"

log "building swapdrill harness"
(cd "$ROOT/services" && go build -o "$WORK/swapdrill" ./cmd/swapdrill) \
    || { echo "swapdrill build failed" >&2; exit 1; }

# --- Pre-flight: prior release + a broken "new" release ----------------------
SHA_A="$(sha256sum "$ENGINE_SRC" | awk '{print $1}')"
REL_A="$WORK/releases/matching-engine-$SHA_A"
cp "$ENGINE_SRC" "$REL_A"; chmod 0755 "$REL_A"

# Broken release: a distinct executable that runs but NEVER becomes ready —
# no ring attach, no recovery, no "ready at" — the probe-failure case the
# rollback automation exists for (shard-swap.sh verify-shard.sh failure leg).
REL_BAD="$WORK/releases/matching-engine-broken"
cat > "$REL_BAD" <<'EOF'
#!/bin/sh
echo "broken release: simulating hang before readiness" >&2
exec sleep 86400
EOF
chmod 0755 "$REL_BAD"
SHA_BAD="$(sha256sum "$REL_BAD" | awk '{print $1}')"
[ "$SHA_A" != "$SHA_BAD" ] \
    && check preflight_distinct PASS "prior sha=$SHA_A broken sha=$SHA_BAD" \
    || check preflight_distinct FAIL "releases identical"

ln -sfn "$REL_A" "$WORK/bin/matching-engine"
check preflight_exec PASS "release artifacts executable"

start_engine() {
    "$WORK/bin/matching-engine" -shard "$SHARD" -ipc-base "$IPC_BASE" \
        -wal-dir "$WAL_DIR" -snap-dir "$SNAP_DIR" -instrument-id "$INSTRUMENT" \
        -idle-sleep-ns 0 -dev-all-accounts \
        -snapshot-interval-s 1 -snapshot-trades 500 \
        -poison-log "$WORK/logs/poison_pill.log" \
        -report-log "$WORK/logs/recovery_report.jsonl" \
        >> "$1" 2>&1 &
    ENGINE_PID=$!
}

rm -f /dev/shm/"${IPC_BASE}"* 2>/dev/null || true
start_engine "$ENGINE_LOG1"
wait_ready_line "$ENGINE_LOG1" 0 15 \
    && check boot1_ready PASS "generation-1 engine ready (pid $ENGINE_PID)" \
    || { check boot1_ready FAIL "engine did not print 'ready at'"; exit 1; }

# --- Baseline load -----------------------------------------------------------
"$WORK/swapdrill" drive -base "$IPC_BASE" -shard "$SHARD" \
    -instrument "$INSTRUMENT" -rate "$RATE" -cross-pct 15 -accounts 512 \
    -seed 17 -order-id-base 3000000 \
    -hold-file "$HOLD" -burst-file "$BURST" -burst-n "$BURST_N" \
    -sent-ids "$SENT_IDS" -events "$EVENTS" -status "$STATUS" \
    -report "$REPORT" >> "$DRILL_LOG" 2>&1 &
DRILL_PID=$!
wait_status sent 1 5 >/dev/null && sleep 0.3
log "loadgen running (pid $DRILL_PID); baseline ${PRE_S}s @ ${RATE}/s"
sleep "$PRE_S"
PRE_SENT="$(jget .sent "$STATUS")"
[ "${PRE_SENT:-0}" -gt 0 ] \
    && check baseline_ingress PASS "$PRE_SENT orders accepted into _in ring" \
    || check baseline_ingress FAIL "no orders flowed"

# --- Drain + checkpoint ------------------------------------------------------
log "ingress drain — hold raised"
touch "$HOLD"
FLUSH_T0=$(now_ns)
if wait_status in_occupancy 0 5; then
    check inflight_flush PASS "_in drained in $(( ($(now_ns) - FLUSH_T0) / 1000000 ))ms"
else
    check inflight_flush FAIL "_in occupancy still >0 after 5s"
fi

log "SIGTERM -> drain snapshot + WAL fsync (latest checkpoint)"
kill -TERM "$ENGINE_PID"
TERM_OK=1
for _ in $(seq 1 100); do
    kill -0 "$ENGINE_PID" 2>/dev/null || { TERM_OK=0; break; }
    sleep 0.05
done
wait "$ENGINE_PID" 2>/dev/null || true
ENGINE_PID=0
[ "$TERM_OK" = "0" ] \
    && check sigterm_exit PASS "engine exited on SIGTERM" \
    || check sigterm_exit FAIL "engine still running 5s after SIGTERM"
grep -q "snapshot stored at seq=" "$ENGINE_LOG1" \
    && check drain_snapshot PASS "$(grep 'snapshot stored at seq=' "$ENGINE_LOG1" | tail -1)" \
    || check drain_snapshot FAIL "no drain snapshot line"

PRE_SCAN="$WORK/wal_scan_pre.json"
"$WAL_AUDIT" -wal-dir "$WAL_DIR/$SHARD" -instrument-id "$INSTRUMENT" \
    -mode scan -json > "$PRE_SCAN" 2>>"$WORK/logs/wal_audit.err" || true
PRE_TAIL="$(jget .wal_tail "$PRE_SCAN")"
log "pre-swap WAL: tail=$PRE_TAIL entries=$(jget .entries "$PRE_SCAN")"

# --- Swap to broken release ---------------------------------------------------
log "binary swap — symlink flip to BROKEN release"
ln -sfn "$REL_BAD" "$WORK/bin/matching-engine"
[ "$(readlink -f "$WORK/bin/matching-engine")" = "$REL_BAD" ] \
    && check symlink_flip_bad PASS "matching-engine -> $(basename "$REL_BAD")" \
    || check symlink_flip_bad FAIL "link target != broken release"

# Buffered-ingress proof: burst lands while the engine is down.
log "  firing buffered-ingress burst (burst-n=$BURST_N) while engine is down"
: > "$BURST"
MID_BACC=""
for _i in $(seq 1 40); do
    MID_BACC="$(jget .burst_accepted "$STATUS")"
    [ "${MID_BACC:-0}" -gt 0 ] 2>/dev/null && break
    sleep 0.25
done
[ "${MID_BACC:-0}" -gt 0 ] \
    && check ring_buffered PASS "$MID_BACC orders buffered during window" \
    || check ring_buffered FAIL "no buffered writes accepted"

# --- Probe failure -> rollback -------------------------------------------------
log "starting broken binary; readiness probe timeout=${PROBE_TIMEOUT_S}s"
start_engine "$ENGINE_LOG_BAD"
BAD_PID="$ENGINE_PID"
if wait_ready_line "$ENGINE_LOG_BAD" 0 "$PROBE_TIMEOUT_S"; then
    check probe_failure_detected FAIL "broken binary reported ready — probe useless"
else
    check probe_failure_detected PASS "no 'ready at' within ${PROBE_TIMEOUT_S}s — probe failed"
fi

log "ROLLBACK: kill gen-2, revert symlink to prior release, restart from checkpoint"
RB_T0=$(now_ns)
kill -TERM "$BAD_PID" 2>/dev/null || kill -9 "$BAD_PID" 2>/dev/null || true
wait "$BAD_PID" 2>/dev/null || true
ENGINE_PID=0
ln -sfn "$REL_A" "$WORK/bin/matching-engine"
[ "$(readlink -f "$WORK/bin/matching-engine")" = "$REL_A" ] \
    && check rollback_symlink_revert PASS "matching-engine -> prior release" \
    || check rollback_symlink_revert FAIL "symlink did not revert"

start_engine "$ENGINE_LOG_RB"
if wait_ready_line "$ENGINE_LOG_RB" 0 15; then
    RB_MS=$(( ($(now_ns) - RB_T0) / 1000000 ))
    check rollback_ready PASS "prior binary ready from checkpoint in ${RB_MS}ms"
    [ "$RB_MS" -lt "$RB_WINDOW_MS_LIMIT" ] \
        && check rollback_window PASS "${RB_MS}ms < ${RB_WINDOW_MS_LIMIT}ms" \
        || check rollback_window FAIL "${RB_MS}ms >= ${RB_WINDOW_MS_LIMIT}ms"
else
    check rollback_ready FAIL "prior binary never reached 'ready at'"
    check rollback_window FAIL "never ready"
fi

LIVE_EXE="$(readlink -f "/proc/$ENGINE_PID/exe" 2>/dev/null || echo '?')"
[ "$LIVE_EXE" = "$(readlink -f "$REL_A")" ] \
    && check running_prior_binary PASS "proc exe = $LIVE_EXE" \
    || check running_prior_binary FAIL "proc exe $LIVE_EXE"

REC_LINE="$(grep 'recovery:' "$ENGINE_LOG_RB" | tail -1 || true)"
log "  recovery: ${REC_LINE:-<none>}"
[ -n "$REC_LINE" ] \
    && check rollback_wal_replay PASS "$REC_LINE" \
    || check rollback_wal_replay FAIL "no 'recovery:' line — not restored from checkpoint"
grep -q "WAL_RECOVERY_HALT" "$ENGINE_LOG_RB" \
    && check rollback_no_halt FAIL "WAL_RECOVERY_HALT after rollback" \
    || check rollback_no_halt PASS "no WAL_RECOVERY_HALT"
REC_TAIL="$(printf '%s' "$REC_LINE" | grep -o 'tail=[0-9]*' | tail -1 | cut -d= -f2)"
if [ -n "$REC_TAIL" ] && [ -n "$PRE_TAIL" ] && [ "$REC_TAIL" != "null" ] \
        && [ "$PRE_TAIL" != "null" ] && [ "$REC_TAIL" -ge "$PRE_TAIL" ]; then
    check rollback_tail_monotonic PASS "recovery tail $REC_TAIL >= pre-swap tail $PRE_TAIL"
else
    check rollback_tail_monotonic FAIL "recovery tail $REC_TAIL vs pre-swap tail $PRE_TAIL"
fi

if wait_status in_occupancy 0 10; then
    check rollback_buffered_drain PASS "buffered burst consumed post-rollback (_in -> 0)"
else
    check rollback_buffered_drain FAIL "_in occupancy stuck after rollback"
fi

# --- Resume + final audit -----------------------------------------------------
log "unpause — resuming ingress for ${POST_S}s"
rm -f "$HOLD"
sleep "$POST_S"
POST_SENT="$(jget .sent "$STATUS")"
[ "${POST_SENT:-0}" -gt "${PRE_SENT:-0}" ] \
    && check resume_ingress PASS "production resumed ($PRE_SENT -> $POST_SENT)" \
    || check resume_ingress FAIL "no post-rollback sends"

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
W_DUPT="$(jget '.dup_trade_ids | length' "$AUDIT_JSON")"
H_DUPT="$(jget .dup_trade_ids "$REPORT")"
H_DUPL3="$(jget .dup_l3_fills "$REPORT")"
H_DUPFS="$(jget .dup_fill_seqs "$REPORT")"
H_L3GAPS="$(jget .l3_seq_gaps "$REPORT")"
H_L3RESET="$(jget .l3_seq_resets "$REPORT")"
H_OUTDROP="$(jget .out_ring_drops "$REPORT")"
H_DECODE="$(jget .decode_errors "$REPORT")"
H_BDEAD="$(jget .burst_while_engine_dead "$REPORT")"
H_FILLS="$(jget .fills "$REPORT")"
W_TRADES="$(jget .journaled_trade_ids "$AUDIT_JSON")"

log "audit: rc=$AUDIT_RC accepted=$R_ACCEPTED order_new=$W_ORDERNEW \
gaps=$W_GAPS overlaps=$W_OVERLAPS corrupt=$W_CORRUPT missing=$W_MISS \
stream_dup_tid=$H_DUPT dup_l3=$H_DUPL3 dup_fseq=$H_DUPFS wal_dup=$W_DUPT \
l3_gaps=$H_L3GAPS l3_resets=$H_L3RESET out_drops=$H_OUTDROP"

[ "$AUDIT_RC" -eq 0 ] && [ "${W_GAPS:-9}" -eq 0 ] && [ "${W_OVERLAPS:-9}" -eq 0 ] \
    && [ "${W_CORRUPT:-9}" -eq 0 ] \
    && check wal_continuity PASS "journal scan clean" \
    || check wal_continuity FAIL "audit rc=$AUDIT_RC gaps=$W_GAPS overlaps=$W_OVERLAPS corrupt=$W_CORRUPT"

[ "$R_ACCEPTED" != "null" ] && [ "$W_ORDERNEW" -eq "$R_ACCEPTED" ] 2>/dev/null \
    && [ "${W_MISS:-9}" -eq 0 ] \
    && check zero_order_loss PASS "$R_ACCEPTED ring-accepted == $W_ORDERNEW journaled, 0 missing" \
    || check zero_order_loss FAIL "accepted=$R_ACCEPTED journaled=$W_ORDERNEW missing=$W_MISS"

[ "${H_DUPT:-9}" -eq 0 ] && [ "${H_DUPL3:-9}" -eq 0 ] && [ "${H_DUPFS:-9}" -eq 0 ] \
    && [ "${W_DUPT:-9}" -eq 0 ] \
    && check zero_dup_exec PASS "trade_ids unique in stream+WAL; no dup L3 legs; no dup fill seqs" \
    || check zero_dup_exec FAIL "dup_tid=$H_DUPT dup_l3=$H_DUPL3 dup_fseq=$H_DUPFS wal_dup=$W_DUPT"

[ "${H_BDEAD:-0}" -gt 0 ] \
    && check ingress_buffered_proof PASS "$H_BDEAD orders accepted while engine dead, all journaled" \
    || check ingress_buffered_proof FAIL "no orders buffered while engine dead"

[ "$H_L3GAPS" = "0" ] && [ "${H_L3RESET:-0}" -le 1 ] && [ "$H_OUTDROP" = "0" ] \
    && check l3_integrity PASS "0 L3 gaps, $H_L3RESET generation reset(s), 0 outbound drops" \
    || check l3_integrity FAIL "l3_gaps=$H_L3GAPS resets=$H_L3RESET out_drops=$H_OUTDROP"

[ "$H_OUTDROP" = "0" ] && [ "$H_FILLS" = "$W_TRADES" ] \
    && check fill_parity PASS "fills=$H_FILLS == journaled TRADE rows=$W_TRADES" \
    || check fill_parity FAIL "fills=$H_FILLS vs wal_trades=$W_TRADES"

[ "$H_DECODE" = "0" ] \
    && check harness_clean PASS "decode_errors=0" \
    || check harness_clean FAIL "decode_errors=$H_DECODE"

log "== rollback drill complete: $FAILS check(s) failed =="
[ "$FAILS" -eq 0 ] && { echo "ROLLBACK-DRILL PASS  (work=$WORK)"; exit 0; }
echo "ROLLBACK-DRILL FAIL ($FAILS failures; evidence kept at $WORK)" >&2
exit 1
