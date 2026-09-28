#!/usr/bin/env bash
# PHASE-02.5 TASK-2.5.3.3 — warm-failover benchmark: kill -9 + restart the
# engine on the same WAL dir, measure kill->"ready" recovery time, and prove
# deterministic-replay parity via wal_audit book fingerprints.
#
#   failover_bench.sh --engine BIN --wal-audit BIN --workdir DIR \
#       --shard N --instrument ID \
#       [--loadgen BIN] [--rate 20000] [--warmup 30s] [--trials 5] \
#       [--ipc-base NAME] [--seed-wal DIR] [--accounts N] [--cross-pct N] \
#       [--seed N] [--metrics-port N]
#
# Per trial:
#   1. Fresh workdir/trialN/wal. If --seed-wal DIR is given its *.wal files
#      are copied into wal/SHARD/ first (pre-populated journal — lets the
#      bench verify NON-EMPTY-book parity without a loadgen).
#   2. Start engine (boot replays the seeded WAL into the book), wait for
#      the "shard N ready" line. Optional warmup: --loadgen feeds orders
#      for --warmup (default 30s) at --rate then gets SIGTERM.
#   3. wal_audit -mode fingerprint -live  -> fp_primary
#      (-live: wal_audit recovers on a staged copy — RecoveryManager's
#      torn-tail repair can never touch the live journal).
#   4. kill -9; restart with identical args; recovery_ms = kill -> ready.
#   5. fingerprint -> fp_standby, fingerprint again -> fp_repeat.
#      Parity requires fp_primary == fp_standby (deterministic replay =>
#      recovered book == replayed book); determinism requires
#      fp_standby == fp_repeat.
#   6. If --loadgen: brief post-recovery run, then wal_audit -mode scan
#      must show wal_tail still advancing with seq_gaps==0 and
#      dup_trade_ids==0.
#
# Aggregation: trials.jsonl + report.md + verdict line.
#   PASS iff every trial recovered in < 3000ms AND fingerprint parity 100%
#   AND every post-recovery scan clean.
#
# NOTE: --loadgen is optional — without it (and without --seed-wal) the
# bench still exercises kill/restart/parity mechanics on an empty book.
# Provide --loadgen or --seed-wal for a meaningful workload.

set -u

ENGINE=""
WAL_AUDIT=""
LOADGEN=""
WORKDIR=""
SHARD=0
INSTRUMENT=7
IPC_BASE="exchange_ipc"
RATE=20000
WARMUP="30s"
TRIALS=5
SEED_WAL=""
ACCOUNTS=1000
CROSS_PCT=20
SEED=1
METRICS_PORT=9464
READY_TIMEOUT_S=300
RECOVERY_TARGET_MS=3000

usage() { sed -n '2,40p' "$0"; exit "${1:-2}"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --engine)        ENGINE="$2"; shift 2;;
        --wal-audit)     WAL_AUDIT="$2"; shift 2;;
        --loadgen)       LOADGEN="$2"; shift 2;;
        --workdir)       WORKDIR="$2"; shift 2;;
        --shard)         SHARD="$2"; shift 2;;
        --instrument)    INSTRUMENT="$2"; shift 2;;
        --ipc-base)      IPC_BASE="$2"; shift 2;;
        --rate)          RATE="$2"; shift 2;;
        --warmup)        WARMUP="$2"; shift 2;;
        --trials)        TRIALS="$2"; shift 2;;
        --seed-wal)      SEED_WAL="$2"; shift 2;;
        --accounts)      ACCOUNTS="$2"; shift 2;;
        --cross-pct)     CROSS_PCT="$2"; shift 2;;
        --seed)          SEED="$2"; shift 2;;
        --metrics-port)  METRICS_PORT="$2"; shift 2;;
        --ready-timeout) READY_TIMEOUT_S="$2"; shift 2;;
        -h|--help)       usage 0;;
        *) echo "unknown arg: $1" >&2; usage 2;;
    esac
done

[ -n "$ENGINE" ] && [ -n "$WAL_AUDIT" ] && [ -n "$WORKDIR" ] || {
    echo "--engine --wal-audit --workdir required" >&2; usage 2; }
[ -x "$ENGINE" ] || { echo "engine not executable: $ENGINE" >&2; exit 2; }
[ -x "$WAL_AUDIT" ] || { echo "wal_audit not executable: $WAL_AUDIT" >&2; exit 2; }
if [ -z "$LOADGEN" ]; then
    echo "failover_bench: NOTE --loadgen not given; running kill/restart/parity" >&2
    echo "  mechanics only (empty book unless --seed-wal provides a journal)." >&2
fi

parse_dur() {
    local v="$1" n
    n="${v%[smhd]}"
    case "$v" in
        *s) awk "BEGIN{printf \"%d\", ($n)+0.0}" ;;
        *m) awk "BEGIN{printf \"%d\", ($n)*60}" ;;
        *h) awk "BEGIN{printf \"%d\", ($n)*3600}" ;;
        *d) awk "BEGIN{printf \"%d\", ($n)*86400}" ;;
        *)  awk "BEGIN{printf \"%d\", ($v)+0.0}" ;;
    esac
}
WARMUP_S=$(parse_dur "$WARMUP")

msg() { echo "[$(date '+%H:%M:%S')] $*" >&2; }

shm_clean() {
    rm -f "/dev/shm/${IPC_BASE}_${SHARD}_in" "/dev/shm/${IPC_BASE}_${SHARD}_out" 2>/dev/null || true
}

ENGINE_PID=0
LOADGEN_PID=0
ENGINE_LOG=""
READY_MARK=0

engine_start() {
    shm_clean
    if [ -f "$ENGINE_LOG" ]; then
        READY_MARK=$(wc -l < "$ENGINE_LOG")
    else
        READY_MARK=0
    fi
    # -dev-all-accounts when the loadgen drives flow — the account store is
    # Phase-03; without it every order rejects ACCOUNT_INACTIVE and the WAL
    # fills with TIME_TICKs only. --seed-wal covers the no-loadgen path.
    local dev_flag=""
    [ -n "$LOADGEN" ] && dev_flag="-dev-all-accounts"
    "$ENGINE" -shard "$SHARD" -ipc-base "$IPC_BASE" -wal-dir "$WAL_ROOT" \
        -instrument-id "$INSTRUMENT" -idle-sleep-ns 0 $dev_flag \
        -snap-dir "$TRIAL_DIR/snap" -snapshot-interval-s 30 \
        -poison-log "$TRIAL_DIR/poison_pill.log" >> "$ENGINE_LOG" 2>&1 &
    ENGINE_PID=$!
}

engine_wait_ready() {
    local timeout_s="${1:-$READY_TIMEOUT_S}" waited=0
    while [ "$waited" -lt "$timeout_s" ]; do
        if tail -n "+$((READY_MARK + 1))" "$ENGINE_LOG" 2>/dev/null \
            | grep -q "ready at"; then
            return 0
        fi
        kill -0 "$ENGINE_PID" 2>/dev/null || return 1
        sleep 0.05
        waited=$((waited + 1))
    done
    return 1
}

engine_stop() {
    local waited=0
    [ "$ENGINE_PID" -gt 0 ] || return 0
    kill -0 "$ENGINE_PID" 2>/dev/null || return 0
    kill -TERM "$ENGINE_PID" 2>/dev/null
    while kill -0 "$ENGINE_PID" 2>/dev/null && [ "$waited" -lt 50 ]; do
        sleep 0.1; waited=$((waited + 1))
    done
    kill -9 "$ENGINE_PID" 2>/dev/null || true
    wait "$ENGINE_PID" 2>/dev/null
}

loadgen_start() {
    local rate="$1" dur_s="$2" report="$3"
    LOADGEN_RUN=$((LOADGEN_RUN + 1))
    local obase=$((1 + LOADGEN_RUN * 1000000000000))
    "$LOADGEN" -base "$IPC_BASE" -shard "$SHARD" -instrument "$INSTRUMENT" \
        -rate "$rate" -duration "$dur_s" -metrics-addr ":$METRICS_PORT" \
        -report "$report" -order-id-base "$obase" -accounts "$ACCOUNTS" \
        -cross-pct "$CROSS_PCT" -seed "$SEED" >> "$LOADGEN_LOG" 2>&1 &
    LOADGEN_PID=$!
}

loadgen_stop() {
    local grace="${1:-5}" waited=0
    [ "$LOADGEN_PID" -gt 0 ] || return 0
    kill -0 "$LOADGEN_PID" 2>/dev/null || return 0
    kill -TERM "$LOADGEN_PID" 2>/dev/null
    while kill -0 "$LOADGEN_PID" 2>/dev/null && [ "$waited" -lt $((grace * 10)) ]; do
        sleep 0.1; waited=$((waited + 1))
    done
    kill -9 "$LOADGEN_PID" 2>/dev/null || true
    wait "$LOADGEN_PID" 2>/dev/null
}

fingerprint() {  # -> hex or "" on failure
    "$WAL_AUDIT" -wal-dir "$WAL_ROOT/$SHARD" -instrument-id "$INSTRUMENT" \
        -mode fingerprint -live 2>>"$TRIAL_DIR/logs/wal_audit.err" || true
}

scan_json() {
    "$WAL_AUDIT" -wal-dir "$WAL_ROOT/$SHARD" -instrument-id "$INSTRUMENT" \
        -mode scan -json 2>>"$TRIAL_DIR/logs/wal_audit.err" || true
}

jnum() { echo "$2" | grep -o "\"$1\"[[:space:]]*:[[:space:]]*[-0-9.]*" | head -1 | sed 's/.*://'; }

# Wait for the WAL tail to stop moving (engine drain after loadgen stops) —
# narrows the torn-tail window before fp_primary. ~0.5s x up to 20 polls.
wait_tail_stable() {
    local last="" cur="" i
    for i in $(seq 1 20); do
        cur=$(scan_json | grep -o '"wal_tail":[0-9]*' | head -1 | cut -d: -f2)
        if [ -n "$cur" ] && [ "$cur" = "$last" ]; then
            return 0
        fi
        last="$cur"
        sleep 0.5
    done
    return 0
}

cleanup() {
    loadgen_stop 3
    engine_stop
    shm_clean
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
mkdir -p "$WORKDIR"
TRIALS_JSONL="$WORKDIR/trials.jsonl"
REPORT_MD="$WORKDIR/report.md"
LOADGEN_RUN=0
PARITY_OK_COUNT=0
MAX_RECOVERY_MS=0
FAIL_REASONS=""
: > "$TRIALS_JSONL"

for t in $(seq 1 "$TRIALS"); do
    TRIAL_DIR="$WORKDIR/trial$t"
    WAL_ROOT="$TRIAL_DIR/wal"
    ENGINE_LOG="$TRIAL_DIR/logs/engine.log"
    LOADGEN_LOG="$TRIAL_DIR/logs/loadgen.log"
    rm -rf "$TRIAL_DIR"
    mkdir -p "$WAL_ROOT/$SHARD" "$TRIAL_DIR/logs"
    if [ -n "$SEED_WAL" ]; then
        cp "$SEED_WAL"/*.wal "$WAL_ROOT/$SHARD"/ 2>/dev/null || true
    fi

    msg "=== trial $t/$TRIALS ==="

    # 1-2. start + optional warmup
    engine_start
    if ! engine_wait_ready "$READY_TIMEOUT_S"; then
        msg "trial $t: engine failed initial ready — abort trial"
        echo "{\"trial\":$t,\"ok\":false,\"detail\":\"initial_ready_timeout\"}" >> "$TRIALS_JSONL"
        FAIL_REASONS="$FAIL_REASONS t$t:ready_timeout"
        continue
    fi
    if [ -n "$LOADGEN" ]; then
        loadgen_start "$RATE" $((WARMUP_S + 30)) "$TRIAL_DIR/loadgen-warm.json"
        sleep "$WARMUP_S"
        loadgen_stop 5
        wait_tail_stable
    fi

    # 3. primary fingerprint
    FP_PRIMARY=$(fingerprint)
    PRE_SCAN=$(scan_json)
    PRE_TAIL=$(jnum wal_tail "$PRE_SCAN")
    PRE_NEW=$(jnum ORDER_NEW "$PRE_SCAN")
    msg "trial $t: fp_primary=$FP_PRIMARY (wal_tail=$PRE_TAIL order_new=$PRE_NEW)"

    # 4. kill -9 + restart, measure recovery
    T_KILL=$(date +%s%N)
    kill -9 "$ENGINE_PID" 2>/dev/null
    wait "$ENGINE_PID" 2>/dev/null
    engine_start
    READY=0
    engine_wait_ready "$READY_TIMEOUT_S" && READY=1
    T_READY=$(date +%s%N)
    RECOVERY_MS=$(( (T_READY - T_KILL) / 1000000 ))
    [ "$READY" -eq 0 ] && RECOVERY_MS=-1
    [ "$RECOVERY_MS" -gt "$MAX_RECOVERY_MS" ] && MAX_RECOVERY_MS=$RECOVERY_MS
    msg "trial $t: recovery_ms=$RECOVERY_MS"

    # 5. standby fingerprint + determinism
    FP_STANDBY=$(fingerprint)
    FP_REPEAT=$(fingerprint)
    PARITY=0
    [ -n "$FP_PRIMARY" ] && [ "$FP_PRIMARY" = "$FP_STANDBY" ] && PARITY=1
    DETERMINISM=0
    [ -n "$FP_STANDBY" ] && [ "$FP_STANDBY" = "$FP_REPEAT" ] && DETERMINISM=1
    [ "$PARITY" -eq 1 ] && [ "$DETERMINISM" -eq 1 ] && \
        PARITY_OK_COUNT=$((PARITY_OK_COUNT + 1))
    msg "trial $t: fp_standby=$FP_STANDBY parity=$PARITY determinism=$DETERMINISM"

    # 6. post-recovery loadgen + monotonic-seq check
    POST_OK="na"; POST_TAIL="$PRE_TAIL"; POST_DUPS=""; POST_GAPS=""
    if [ -n "$LOADGEN" ] && [ "$READY" -eq 1 ]; then
        loadgen_start "$RATE" 15 "$TRIAL_DIR/loadgen-post.json"
        sleep 10
        loadgen_stop 5
        sleep 0.5
    fi
    if [ "$READY" -eq 1 ]; then
        POST_SCAN=$(scan_json)
        POST_TAIL=$(jnum wal_tail "$POST_SCAN")
        POST_NEW=$(jnum ORDER_NEW "$POST_SCAN")
        POST_DUPS=$(jnum dup_trade_ids "$POST_SCAN")
        POST_GAPS=$(jnum seq_gaps "$POST_SCAN")
        POST_OK="ok"
        { [ -z "$POST_GAPS" ] || [ "$POST_GAPS" != "0" ]; } && POST_OK="gaps"
        { [ -z "$POST_DUPS" ] || [ "$POST_DUPS" != "0" ]; } && POST_OK="dups"
        # Tail regression means the writer resumed at a lower seq — never OK.
        if [ -n "$POST_TAIL" ] && [ -n "$PRE_TAIL" ] && \
           [ "$POST_TAIL" -lt "$PRE_TAIL" ]; then
            POST_OK="regress"
        fi
        # Fills resume => new ORDER_NEW journals appear post-recovery
        # (TIME_TICKs would advance wal_tail even with no order flow, so the
        # count check is the meaningful resume signal).
        if [ -n "$LOADGEN" ] && [ -n "$POST_NEW" ] && [ -n "$PRE_NEW" ] && \
           [ "$POST_NEW" -le "$PRE_NEW" ]; then
            POST_OK="stall"
        fi
    fi
    msg "trial $t: post_scan=$POST_OK tail=$PRE_TAIL->$POST_TAIL dups=$POST_DUPS gaps=$POST_GAPS"

    OK="false"
    if [ "$PARITY" -eq 1 ] && [ "$DETERMINISM" -eq 1 ] && \
       [ "$RECOVERY_MS" -ge 0 ] && [ "$RECOVERY_MS" -lt "$RECOVERY_TARGET_MS" ] && \
       { [ "$POST_OK" = "ok" ] || [ "$POST_OK" = "na" ]; }; then
        OK="true"
    else
        FAIL_REASONS="$FAIL_REASONS t$t:rcv=${RECOVERY_MS}ms,parity=$PARITY,det=$DETERMINISM,post=$POST_OK"
    fi
    printf '{"trial":%s,"ok":%s,"recovery_ms":%s,"fp_primary":"%s","fp_standby":"%s","parity":%s,"determinism":%s,"pre_tail":%s,"post_tail":%s,"post_status":"%s","post_dup_trade_ids":%s,"post_seq_gaps":%s}\n' \
        "$t" "$OK" "$RECOVERY_MS" "$FP_PRIMARY" "$FP_STANDBY" \
        "$PARITY" "$DETERMINISM" "${PRE_TAIL:-0}" "${POST_TAIL:-0}" \
        "$POST_OK" "${POST_DUPS:-0}" "${POST_GAPS:-0}" >> "$TRIALS_JSONL"

    engine_stop
done

VERDICT="FAIL"
if [ "$PARITY_OK_COUNT" -eq "$TRIALS" ] && [ "$MAX_RECOVERY_MS" -ge 0 ] && \
   [ "$MAX_RECOVERY_MS" -lt "$RECOVERY_TARGET_MS" ] && [ -z "$FAIL_REASONS" ]; then
    VERDICT="PASS"
fi

# Machine-readable summary consumed by tests/spec/checks/phase02_5.go.
DUP_TOTAL=$(awk -F'"post_dup_trade_ids":' '{n=split($2,a,","); s+=a[1]+0} \
            END{print s+0}' "$TRIALS_JSONL" 2>/dev/null)
printf '{"verdict":"%s","trials":%s,"parity_ok":%s,"parity_fail":%s,"recovery_ms_max":%s,"dup_trade_ids":%s}\n' \
    "$VERDICT" "$TRIALS" "$PARITY_OK_COUNT" "$((TRIALS - PARITY_OK_COUNT))" \
    "$MAX_RECOVERY_MS" "${DUP_TOTAL:-0}" > "$WORKDIR/failover-report.json"

{
    echo "# Phase-02.5 warm-failover benchmark"
    echo
    echo "- engine: \`$ENGINE\`  shard=$SHARD instrument=$INSTRUMENT"
    echo "- trials: $TRIALS   warmup: ${WARMUP} @ ${RATE}/s   seed-wal: ${SEED_WAL:-none}"
    echo "- target: recovery < ${RECOVERY_TARGET_MS}ms, fingerprint parity 100%"
    echo
    echo "## Results"
    echo
    echo "- fingerprint parity: $PARITY_OK_COUNT/$TRIALS"
    echo "- max recovery_ms: $MAX_RECOVERY_MS"
    echo
    echo '```json'; cat "$TRIALS_JSONL"; echo '```'
    echo
    [ -n "$FAIL_REASONS" ] && echo "- failures:$FAIL_REASONS"
    echo
    echo "## Verdict: **$VERDICT**"
} > "$REPORT_MD"

echo "failover_bench verdict=$VERDICT parity=$PARITY_OK_COUNT/$TRIALS max_recovery_ms=$MAX_RECOVERY_MS report=$REPORT_MD"
[ "$VERDICT" = "PASS" ]
