#!/usr/bin/env bash
# Self-freeze: bash lazy-reads script files, so editing monitor.sh mid-run
# corrupts the running interpreter (observed 2026-09-28: exit path died with
# "syntax error near unexpected token" after a mid-run edit — the archive
# step never ran). Re-exec a private frozen copy once, at the very top.
if [ -z "${MONITOR_FROZEN:-}" ]; then
    _frz=$(mktemp /tmp/monitor-frozen.XXXXXX.sh 2>/dev/null) \
        && cp "$0" "$_frz" \
        && exec env MONITOR_FROZEN=1 bash "$_frz" "$@"
    echo "warn: could not freeze script copy; running live file" >&2
fi
# PHASE-02.5 TASK-2.5.3 — 72h soak orchestrator for the matching engine.
#
# Drives the full soak profile end to end; EVERY duration is a flag so the
# same script runs the profile in minutes for smoke testing:
#
#   monitor.sh --engine /path/matching_engine --loadgen /path/loadgen \
#     --workdir DIR --shard N --ipc-base NAME --instrument ID \
#     --rate 50000 --duration 72h \
#     --crash-at "4h 12h 24h 48h 60h" \
#     --audit-interval 1h --sample-interval 5s \
#     --wal-audit /path/wal_audit --metrics-port 9464 [--burst-at "2h 8h"]
#
# Behavior:
#   * engine: -shard -ipc-base -wal-dir $WORKDIR/wal -instrument-id
#     -idle-sleep-ns 0 ; "ready" is the "shard N ready at ..." line in its
#     merged stdout/stderr log (main.cpp prints it on stdout).
#   * loadgen CLI contract (parallel agent): -base -shard -instrument -rate
#     -duration <seconds> -metrics-addr -report -order-id-base -accounts
#     -cross-pct -seed ; metrics on :METRICS_PORT/metrics ; report JSON on
#     SIGTERM exit. Every (re)start bumps -order-id-base by 1e12 so order
#     ids never collide across engine/loadgen restarts.
#   * every --sample-interval: row -> metrics/samples.csv
#     (ts,engine_rss_kb,engine_cpu_pct,loadgen_alive,wal_bytes,wal_tail_seq),
#     plus a Prometheus scrape -> metrics/prom-<ts>.txt. The wal_tail_seq
#     column reuses the last audit's tail (per-sample wal_audit is the
#     expensive --audit-interval path, not the cheap sampler).
#   * every --audit-interval: wal_audit -mode scan -json >> audits.jsonl;
#     ok:false rows are counted and logged to events.jsonl.
#   * --crash-at offsets: kill -9 the engine, restart with identical args,
#     measure kill->ready recovery_ms, restart loadgen with a fresh
#     -order-id-base.
#   * --burst-at offsets: restart loadgen at rate*1.2 for --burst-len (5m)
#     then restore (Task 2.5.3.3 burst schedule).
#   * Shutdown (duration reached or SIGINT/SIGTERM): SIGTERM loadgen (it
#     writes its report), SIGTERM engine (SIGKILL after 5s), final
#     wal_audit -mode recover -json, report.md with verdict vs criteria:
#       throughput >= 45k/s sustained, p99 <= 50us, p999 <= 5ms,
#       RSS < 12GB, zero WAL gaps, recovery < 10s, dup_trade_ids == 0.
#
# All state lives under --workdir. `set -u` only: transient failures
# (scrape timeout, audit race during a crash window) must not kill the
# monitor. No jq dependency — JSON is parsed with grep/sed; jq is used for
# the pretty report only when present.
#
# Signal note: send SIGTERM for supervised shutdowns. SIGINT only works
# when the script runs in a FOREGROUND job — a shell launched detached
# inherits SIGINT ignored, in which case traps cannot install (POSIX).

set -u

# ---------------------------------------------------------------------------
# Args
# ---------------------------------------------------------------------------
ENGINE=""
LOADGEN=""
WAL_AUDIT=""
WORKDIR=""
SHARD=0
IPC_BASE="exchange_ipc"
INSTRUMENT=7
RATE=50000
DURATION="72h"
CRASH_AT="4h 12h 24h 48h 60h"
AUDIT_INTERVAL="1h"
SAMPLE_INTERVAL="5s"
METRICS_PORT=9464
BURST_AT=""
BURST_MULT="1.2"
BURST_LEN="5m"
ACCOUNTS=1000
CROSS_PCT=20
SEED=1
READY_TIMEOUT_S=120
DEV_ACCOUNTS=1    # -dev-all-accounts on the engine; set 0 for prod wiring
ARTIFACTS=""      # optional dir: run summary archived to DIR/<ts>/ + latest/
SNAP_DIR=""       # default WORKDIR/snap — periodic book snapshots bound to
SNAP_INTERVAL_S=60 # recovery (<10s AC at scale requires snapshot+tail replay)

usage() {
    sed -n '2,45p' "$0"
    exit "${1:-2}"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --engine)          ENGINE="$2"; shift 2;;
        --loadgen)         LOADGEN="$2"; shift 2;;
        --wal-audit)       WAL_AUDIT="$2"; shift 2;;
        --workdir)         WORKDIR="$2"; shift 2;;
        --shard)           SHARD="$2"; shift 2;;
        --ipc-base)        IPC_BASE="$2"; shift 2;;
        --instrument)      INSTRUMENT="$2"; shift 2;;
        --rate)            RATE="$2"; shift 2;;
        --duration)        DURATION="$2"; shift 2;;
        --crash-at)        CRASH_AT="${CRASH_AT:+$CRASH_AT }$2"; shift 2;;
        --audit-interval)  AUDIT_INTERVAL="$2"; shift 2;;
        --sample-interval) SAMPLE_INTERVAL="$2"; shift 2;;
        --metrics-port)    METRICS_PORT="$2"; shift 2;;
        --burst-at)        BURST_AT="${BURST_AT:+$BURST_AT }$2"; shift 2;;
        --burst-mult)      BURST_MULT="$2"; shift 2;;
        --burst-len)       BURST_LEN="$2"; shift 2;;
        --accounts)        ACCOUNTS="$2"; shift 2;;
        --dev-accounts)    DEV_ACCOUNTS="$2"; shift 2;;
        --artifacts)       ARTIFACTS="$2"; shift 2;;
        --snap-dir)        SNAP_DIR="$2"; shift 2;;
        --snapshot-interval-s) SNAP_INTERVAL_S="$2"; shift 2;;
        --cross-pct)       CROSS_PCT="$2"; shift 2;;
        --seed)            SEED="$2"; shift 2;;
        --ready-timeout)   READY_TIMEOUT_S="$2"; shift 2;;
        -h|--help)         usage 0;;
        *) echo "unknown arg: $1" >&2; usage 2;;
    esac
done

[ -n "$ENGINE" ] || { echo "--engine required" >&2; usage 2; }
[ -n "$LOADGEN" ] || { echo "--loadgen required" >&2; usage 2; }
[ -n "$WAL_AUDIT" ] || { echo "--wal-audit required" >&2; usage 2; }
[ -n "$WORKDIR" ] || { echo "--workdir required" >&2; usage 2; }
[ -x "$ENGINE" ] || { echo "engine not executable: $ENGINE" >&2; exit 2; }
[ -x "$WAL_AUDIT" ] || { echo "wal_audit not executable: $WAL_AUDIT" >&2; exit 2; }

# ---------------------------------------------------------------------------
# Duration parsing: 90s / 30m / 4h / 72h / 7d / bare seconds (ints or floats)
# ---------------------------------------------------------------------------
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

DURATION_S=$(parse_dur "$DURATION")
AUDIT_S=$(parse_dur "$AUDIT_INTERVAL")
SAMPLE_S=$(parse_dur "$SAMPLE_INTERVAL")
BURST_LEN_S=$(parse_dur "$BURST_LEN")
[ "$DURATION_S" -gt 0 ] || { echo "bad --duration: $DURATION" >&2; exit 2; }
[ "$AUDIT_S" -gt 0 ] || AUDIT_S=3600
[ "$SAMPLE_S" -gt 0 ] || SAMPLE_S=5

BURST_RATE=$(awk "BEGIN{printf \"%d\", $RATE * $BURST_MULT}")

# ---------------------------------------------------------------------------
# Layout + state
# ---------------------------------------------------------------------------
mkdir -p "$WORKDIR"/{wal,logs,metrics}
WALDIR="$WORKDIR/wal/$SHARD"
ENGINE_LOG="$WORKDIR/logs/engine.log"
LOADGEN_LOG="$WORKDIR/logs/loadgen.log"
MONITOR_LOG="$WORKDIR/logs/monitor.log"
SAMPLES_CSV="$WORKDIR/metrics/samples.csv"
AUDITS_JSONL="$WORKDIR/audits.jsonl"
EVENTS_JSONL="$WORKDIR/events.jsonl"
REPORT_MD="$WORKDIR/report.md"

ENGINE_PID=0
LOADGEN_PID=0
LOADGEN_RUN=0
LOADGEN_LAST_START_S=0
CUR_RATE="$RATE"
REQUEST_STOP=0
SHUTTING_DOWN=0

MAX_RSS_KB=0
AUDIT_OK=0
AUDIT_FAIL=0
CRASH_COUNT=0
RECOVERY_MAX_MS=0
LAST_WAL_TAIL=""
PREV_JIFFIES=0
PREV_JIFFY_TS=0
LAST_CPU_PCT="0.0"
CLK_TCK=$(getconf CLK_TCK 2>/dev/null || echo 100)

msg() {
    local line="[$(date '+%Y-%m-%d %H:%M:%S')] $*"
    echo "$line" | tee -a "$MONITOR_LOG" >&2
}

event() {  # event <type> <kv-string>  -> one JSON-ish line
    printf '{"type":"%s","ts":%s%s}\n' "$1" "$(date +%s)" "${2:+,$2}" >> "$EVENTS_JSONL"
}

shm_clean() {
    rm -f "/dev/shm/${IPC_BASE}_${SHARD}_in" "/dev/shm/${IPC_BASE}_${SHARD}_out" 2>/dev/null || true
}

READY_MARK=0
engine_start() {
    shm_clean
    if [ -f "$ENGINE_LOG" ]; then
        READY_MARK=$(wc -l < "$ENGINE_LOG")
    else
        READY_MARK=0
    fi
    # -dev-all-accounts: soak/dev opt-in — the engine's account store is a
    # Phase-03 dependency; without a bound IAccountState the risk pipeline
    # rejects every order (ACCOUNT_INACTIVE) and the soak produces no flow.
    local dev_flag=""
    [ "$DEV_ACCOUNTS" != "0" ] && dev_flag="-dev-all-accounts"
    "$ENGINE" -shard "$SHARD" -ipc-base "$IPC_BASE" -wal-dir "$WORKDIR/wal" \
        -instrument-id "$INSTRUMENT" -idle-sleep-ns 0 $dev_flag \
        -snap-dir "${SNAP_DIR:-$WORKDIR/snap}" \
        -snapshot-interval-s "$SNAP_INTERVAL_S" \
        -poison-log "$WORKDIR/poison_pill.log" >> "$ENGINE_LOG" 2>&1 &
    ENGINE_PID=$!
    # New pid => /proc stat jiffy baseline resets (avoid negative cpu deltas).
    PREV_JIFFIES=0
    PREV_JIFFY_TS=0
    msg "engine started pid=$ENGINE_PID"
}

# Wait for a NEW "ready at" line past READY_MARK; early-exits if the process
# dies first. Returns 0 on ready, 1 on timeout/death.
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
    local grace="${1:-5}" waited=0
    [ "$ENGINE_PID" -gt 0 ] || return 0
    kill -0 "$ENGINE_PID" 2>/dev/null || return 0
    kill -TERM "$ENGINE_PID" 2>/dev/null
    while kill -0 "$ENGINE_PID" 2>/dev/null && [ "$waited" -lt $((grace * 10)) ]; do
        sleep 0.1; waited=$((waited + 1))
    done
    if kill -0 "$ENGINE_PID" 2>/dev/null; then
        kill -9 "$ENGINE_PID" 2>/dev/null
        msg "engine pid=$ENGINE_PID needed SIGKILL after ${grace}s grace"
    fi
    wait "$ENGINE_PID" 2>/dev/null
    msg "engine stopped"
}

loadgen_start() {
    local rate="$1"
    LOADGEN_RUN=$((LOADGEN_RUN + 1))
    local obase=$((1 + LOADGEN_RUN * 1000000000000))
    local report="$WORKDIR/loadgen-report-${LOADGEN_RUN}.json"
    "$LOADGEN" -base "$IPC_BASE" -shard "$SHARD" -instrument "$INSTRUMENT" \
        -rate "$rate" -duration "$DURATION_S" -metrics-addr ":$METRICS_PORT" \
        -report "$report" -order-id-base "$obase" -accounts "$ACCOUNTS" \
        -cross-pct "$CROSS_PCT" -seed "$SEED" >> "$LOADGEN_LOG" 2>&1 &
    LOADGEN_PID=$!
    LOADGEN_LAST_START_S=$(date +%s)
    msg "loadgen started pid=$LOADGEN_PID rate=$rate order-id-base=$obase report=$report"
}

loadgen_stop() {
    local grace="${1:-8}" waited=0
    [ "$LOADGEN_PID" -gt 0 ] || return 0
    kill -0 "$LOADGEN_PID" 2>/dev/null || return 0
    kill -TERM "$LOADGEN_PID" 2>/dev/null
    while kill -0 "$LOADGEN_PID" 2>/dev/null && [ "$waited" -lt $((grace * 10)) ]; do
        sleep 0.1; waited=$((waited + 1))
    done
    kill -9 "$LOADGEN_PID" 2>/dev/null || true
    wait "$LOADGEN_PID" 2>/dev/null
    msg "loadgen stopped"
}

loadgen_alive() {
    [ "$LOADGEN_PID" -gt 0 ] && kill -0 "$LOADGEN_PID" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Sampling + audit
# ---------------------------------------------------------------------------
sample_once() {
    local ts rss=0 wal_bytes=0 alive=0 jiff=0 now_s
    ts=$(date +%s)
    now_s=$ts
    if [ -r "/proc/$ENGINE_PID/status" ]; then
        rss=$(awk '/^VmRSS:/{print $2}' "/proc/$ENGINE_PID/status" 2>/dev/null || echo 0)
        [ -n "$rss" ] || rss=0
    fi
    [ "$rss" -gt "$MAX_RSS_KB" ] 2>/dev/null && MAX_RSS_KB=$rss
    # CPU% from /proc stat jiffy delta between samples.
    if [ -r "/proc/$ENGINE_PID/stat" ]; then
        jiff=$(awk '{print $14+$15}' "/proc/$ENGINE_PID/stat" 2>/dev/null || echo 0)
        if [ "$PREV_JIFFY_TS" -gt 0 ] && [ "$now_s" -gt "$PREV_JIFFY_TS" ] \
           && [ "$jiff" -ge "$PREV_JIFFIES" ]; then
            LAST_CPU_PCT=$(awk "BEGIN{printf \"%.1f\", ($jiff - $PREV_JIFFIES) * 100.0 / ($CLK_TCK * ($now_s - $PREV_JIFFY_TS))}")
        fi
        PREV_JIFFIES=$jiff
        PREV_JIFFY_TS=$now_s
    fi
    loadgen_alive && alive=1
    if [ -d "$WALDIR" ]; then
        wal_bytes=$(du -sb "$WALDIR" 2>/dev/null | awk '{print $1}')
        [ -n "$wal_bytes" ] || wal_bytes=0
        # Live progress per sample: numerically-newest segment's stem (seq
        # base of the active segment — monotonic across rotations; lexical
        # sort freezes past 1e8). Audits record the true wal_tail separately
        # in audits.jsonl; mixing the two would regress the column.
        local seg
        seg=$(ls "$WALDIR"/*.wal 2>/dev/null | sed 's/.*\///;s/\.wal$//' \
              | sort -n | tail -1)
        [ -n "$seg" ] && LIVE_WAL_SEG=$seg
    fi
    echo "${ts},${rss},${LAST_CPU_PCT},${alive},${wal_bytes},${LIVE_WAL_SEG:-$LAST_WAL_TAIL}" >> "$SAMPLES_CSV"
    # Prometheus scrape — never fatal.
    { curl -fsS --max-time 2 "http://localhost:${METRICS_PORT}/metrics" \
        || echo "# scrape_failed $(date -Is)"; } > "$WORKDIR/metrics/prom-${ts}.txt" 2>/dev/null
}

audit_once() {
    local out
    out=$("$WAL_AUDIT" -wal-dir "$WALDIR" -instrument-id "$INSTRUMENT" \
          -mode scan -json 2>>"$WORKDIR/logs/wal_audit.err")
    if [ -z "$out" ]; then
        out='{"ok":false,"detail":"wal_audit produced no output"}'
    fi
    echo "$out" >> "$AUDITS_JSONL"
    LAST_WAL_TAIL=$(echo "$out" | grep -o '"wal_tail":[0-9]*' | head -1 | cut -d: -f2)
    if echo "$out" | grep -q '"ok":false'; then
        AUDIT_FAIL=$((AUDIT_FAIL + 1))
        event "audit_fail" "\"detail\":\"wal_audit scan failed\""
        msg "AUDIT FAIL: $out"
    else
        AUDIT_OK=$((AUDIT_OK + 1))
    fi
}

inject_crash() {
    local off_s="$1" t_kill t_ready rcv_ms
    msg "crash injection at +${off_s}s: kill -9 $ENGINE_PID"
    t_kill=$(date +%s%N)
    kill -9 "$ENGINE_PID" 2>/dev/null
    wait "$ENGINE_PID" 2>/dev/null
    sleep 0.2
    engine_start
    if engine_wait_ready 300; then
        t_ready=$(date +%s%N)
        rcv_ms=$(( (t_ready - t_kill) / 1000000 ))
        CRASH_COUNT=$((CRASH_COUNT + 1))
        [ "$rcv_ms" -gt "$RECOVERY_MAX_MS" ] && RECOVERY_MAX_MS=$rcv_ms
        event "crash" "\"offset_s\":$off_s,\"recovery_ms\":$rcv_ms"
        msg "engine recovered in ${rcv_ms}ms (crash #$CRASH_COUNT)"
    else
        event "crash" "\"offset_s\":$off_s,\"recovery_ms\":-1"
        msg "ENGINE FAILED TO BECOME READY after kill -9"
    fi
    # No reconnect mode in loadgen: restart it with a fresh id base.
    loadgen_stop 4
    loadgen_start "$CUR_RATE"
}

burst_on() {
    local off_s="$1"
    msg "burst at +${off_s}s: rate $CUR_RATE -> $BURST_RATE for ${BURST_LEN_S}s"
    CUR_RATE="$BURST_RATE"
    loadgen_stop 4
    loadgen_start "$CUR_RATE"
    event "burst_start" "\"offset_s\":$off_s,\"rate\":$BURST_RATE"
}

burst_off() {
    msg "burst end: rate -> $RATE"
    CUR_RATE="$RATE"
    loadgen_stop 4
    loadgen_start "$CUR_RATE"
    event "burst_end" "\"rate\":$RATE"
}

# ---------------------------------------------------------------------------
# JSON number extraction (no jq): jget FILE key1 key2 ... -> first match
# ---------------------------------------------------------------------------
jget() {
    local f="$1" k v
    shift
    for k in "$@"; do
        v=$(grep -o "\"$k\"[[:space:]]*:[[:space:]]*[-0-9.e+]*" "$f" 2>/dev/null \
            | head -1 | sed 's/.*:[[:space:]]*//')
        [ -n "$v" ] && { echo "$v"; return 0; }
    done
    echo ""
}

latest_loadgen_report() {
    ls -t "$WORKDIR"/loadgen-report-*.json 2>/dev/null | head -1 || true
}

crit() { # crit <name> <PASS|FAIL|NA> <detail>
    printf '| %-28s | %-4s | %s |\n' "$1" "$2" "$3" >> "$REPORT_MD"
    [ "$2" = "PASS" ] && return 0
    return 1
}

write_report() {
    local end_ts="$1" actual_s lg_report orders fills dups p50 p99 p999 thr
    actual_s=$((end_ts - START_S))
    lg_report=$(latest_loadgen_report)
    orders=""; fills=""; dups=""; p50=""; p99=""; p999=""; thr=""
    if [ -n "$lg_report" ] && [ -f "$lg_report" ]; then
        orders=$(jget "$lg_report" orders orders_sent total_orders)
        fills=$(jget "$lg_report" fills trades total_fills)
        dups=$(jget "$lg_report" dup_trade_ids duplicate_trades)
        p50=$(jget "$lg_report" p50_us latency_p50_us latency_p50 p50)
        p99=$(jget "$lg_report" p99_us latency_p99_us latency_p99 p99)
        p999=$(jget "$lg_report" p999_us latency_p999_us latency_p999 p999)
        thr=$(jget "$lg_report" achieved_rate orders_per_sec throughput avg_rate)
    fi

    local verdict="PASS" checks=0
    {
        echo "# Phase-02.5 soak report"
        echo
        echo "- workdir: \`$WORKDIR\`"
        echo "- shard: $SHARD  instrument: $INSTRUMENT  ipc-base: $IPC_BASE"
        echo "- target rate: $RATE/s  duration: ${DURATION} (${actual_s}s actual)"
        echo "- loadgen runs: $LOADGEN_RUN  (last report: ${lg_report:-none})"
        echo
        echo "## Throughput / latency (loadgen report)"
        echo
        echo "- orders: ${orders:-n/a}   fills: ${fills:-n/a}   dup_trade_ids: ${dups:-n/a}"
        echo "- p50: ${p50:-n/a}us   p99: ${p99:-n/a}us   p999: ${p999:-n/a}us"
        echo "- achieved rate: ${thr:-n/a}/s"
        echo
        echo "## Recovery (crash injection)"
        echo
        echo "- crashes injected: $CRASH_COUNT   max recovery_ms: $RECOVERY_MAX_MS"
        if [ -s "$EVENTS_JSONL" ]; then
            echo
            echo '```json'; cat "$EVENTS_JSONL"; echo '```'
        fi
        echo
        echo "## WAL audits"
        echo
        echo "- ok: $AUDIT_OK   failed: $AUDIT_FAIL"
        echo "- max engine RSS: ${MAX_RSS_KB} KB (limit 12582912 KB = 12GB)"
        echo
        echo "## Acceptance criteria"
        echo
        echo '| criterion                    |      | detail |'
        echo '|------------------------------|------|--------|'
    } > "$REPORT_MD"

    [ -n "$thr" ] && awk "BEGIN{exit !($thr >= 45000)}" \
        && crit "throughput >= 45k/s" PASS "$thr/s" \
        || crit "throughput >= 45k/s" FAIL "${thr:-no data}"; checks=$((checks+$?))
    [ -n "$p99" ] && awk "BEGIN{exit !($p99 <= 50)}" \
        && crit "p99 <= 50us" PASS "${p99}us" \
        || crit "p99 <= 50us" FAIL "${p99:-no data}"; checks=$((checks+$?))
    [ -n "$p999" ] && awk "BEGIN{exit !($p999 <= 5000)}" \
        && crit "p999 <= 5ms" PASS "${p999}us" \
        || crit "p999 <= 5ms" FAIL "${p999:-no data}"; checks=$((checks+$?))
    [ "$MAX_RSS_KB" -lt 12582912 ] \
        && crit "RSS < 12GB" PASS "${MAX_RSS_KB}KB" \
        || crit "RSS < 12GB" FAIL "${MAX_RSS_KB}KB"; checks=$((checks+$?))
    [ "$AUDIT_FAIL" -eq 0 ] \
        && crit "zero WAL gaps/corruption" PASS "audits_ok=$AUDIT_OK" \
        || crit "zero WAL gaps/corruption" FAIL "audits_fail=$AUDIT_FAIL"; checks=$((checks+$?))
    { [ "$CRASH_COUNT" -eq 0 ] || [ "$RECOVERY_MAX_MS" -lt 10000 ]; } \
        && crit "recovery < 10s" PASS "max=${RECOVERY_MAX_MS}ms n=$CRASH_COUNT" \
        || crit "recovery < 10s" FAIL "max=${RECOVERY_MAX_MS}ms"; checks=$((checks+$?))
    { [ -z "$dups" ] || [ "$dups" = "0" ]; } \
        && crit "dup_trade_ids == 0" PASS "${dups:-0}" \
        || crit "dup_trade_ids == 0" FAIL "$dups"; checks=$((checks+$?))

    [ "$checks" -gt 0 ] && verdict="FAIL"
    {
        echo
        echo "## Verdict: **$verdict**"
        if command -v jq >/dev/null 2>&1 && [ -s "$AUDITS_JSONL" ]; then
            echo
            echo "### Final audit"; echo '```json'
            tail -1 "$AUDITS_JSONL" | jq .
            echo '```'
        fi
    } >> "$REPORT_MD"
    msg "verdict=$verdict report=$REPORT_MD"
}

# ---------------------------------------------------------------------------
# Shutdown
# ---------------------------------------------------------------------------
cleanup() {
    # Run exactly once (loop exit or trap-driven fall-through).
    [ "$SHUTTING_DOWN" -eq 0 ] || return 0
    SHUTTING_DOWN=1
    msg "shutdown: stopping loadgen + engine"
    loadgen_stop 8
    engine_stop 5
    # Final full replay audit — engine is stopped, recover is safe and may
    # legitimately truncate a torn tail from a crash window.
    if [ -d "$WALDIR" ]; then
        local fout
        fout=$("$WAL_AUDIT" -wal-dir "$WALDIR" -instrument-id "$INSTRUMENT" \
               -mode recover -json 2>>"$WORKDIR/logs/wal_audit.err")
        [ -n "$fout" ] && echo "$fout" >> "$AUDITS_JSONL"
        if echo "$fout" | grep -q '"ok":false'; then
            AUDIT_FAIL=$((AUDIT_FAIL + 1))
        else
            AUDIT_OK=$((AUDIT_OK + 1))
        fi
    fi
    write_report "$(date +%s)"
    # Artifact archive: the spec-check registry scans
    # tests/soak/artifacts/<run>/ for soak-report.json / events.jsonl /
    # report.md — copy the run record and refresh the `latest` symlink.
    if [ -n "$ARTIFACTS" ]; then
        local adir="$ARTIFACTS/$(date -u +%Y%m%dT%H%M%SZ)-shard$SHARD"
        mkdir -p "$adir"
        cp -f "$REPORT_MD" "$EVENTS_JSONL" "$AUDITS_JSONL" \
              "$WORKDIR/metrics/samples.csv" "$adir/" 2>/dev/null || true
        local last_lg
        last_lg=$(ls -1 "$WORKDIR"/loadgen-report-*.json 2>/dev/null | tail -1)
        [ -n "$last_lg" ] && cp -f "$last_lg" "$adir/soak-report.json"
        [ -f "$WORKDIR/failover-report.json" ] && \
            cp -f "$WORKDIR/failover-report.json" "$adir/"
        ln -sfn "$adir" "$ARTIFACTS/latest"
        msg "artifacts archived to $adir"
    fi
    msg "monitor done"
}

trap 'REQUEST_STOP=1' INT TERM

# ---------------------------------------------------------------------------
# Main schedule
# ---------------------------------------------------------------------------
START_S=$(date +%s)
END_S=$((START_S + DURATION_S))
echo "ts_epoch_s,engine_rss_kb,engine_cpu_pct,loadgen_alive,wal_size_bytes,wal_tail_seq" > "$SAMPLES_CSV"
: > "$EVENTS_JSONL"; : > "$AUDITS_JSONL"
msg "monitor start: duration=${DURATION_S}s rate=$RATE shard=$SHARD instrument=$INSTRUMENT workdir=$WORKDIR"

# Parse crash/burst schedules into second offsets.
CRASH_OFFS=""
for c in $CRASH_AT; do CRASH_OFFS="$CRASH_OFFS $(parse_dur "$c")"; done
BURST_OFFS=""
for b in $BURST_AT; do BURST_OFFS="$BURST_OFFS $(parse_dur "$b")"; done
CRASH_DONE=""
BURST_DONE=""
BURST_UNTIL=0

engine_start
if engine_wait_ready "$READY_TIMEOUT_S"; then
    msg "engine ready"
else
    msg "engine failed to become ready in ${READY_TIMEOUT_S}s — aborting"
    engine_stop 5
    exit 1
fi
loadgen_start "$CUR_RATE"

NEXT_SAMPLE=$((START_S + SAMPLE_S))
NEXT_AUDIT=$((START_S + AUDIT_S))

while [ "$REQUEST_STOP" -eq 0 ]; do
    now=$(date +%s)
    [ "$now" -ge "$END_S" ] && break

    # Scheduled crashes.
    for off in $CRASH_OFFS; do
        case " $CRASH_DONE " in *" $off "*) continue;; esac
        if [ "$now" -ge $((START_S + off)) ]; then
            CRASH_DONE="$CRASH_DONE $off"
            inject_crash "$off"
            now=$(date +%s)
        fi
    done

    # Scheduled bursts.
    for off in $BURST_OFFS; do
        case " $BURST_DONE " in *" $off "*) continue;; esac
        if [ "$now" -ge $((START_S + off)) ]; then
            BURST_DONE="$BURST_DONE $off"
            BURST_UNTIL=$((now + BURST_LEN_S))
            burst_on "$off"
        fi
    done
    if [ "$BURST_UNTIL" -gt 0 ] && [ "$now" -ge "$BURST_UNTIL" ]; then
        BURST_UNTIL=0
        burst_off
    fi

    # Unexpected engine death outside a crash window -> log + restart.
    if ! kill -0 "$ENGINE_PID" 2>/dev/null; then
        msg "engine pid=$ENGINE_PID died unexpectedly — restarting"
        event "engine_died" "\"recovery_ms\":-1"
        engine_start
        engine_wait_ready 300 || msg "engine restart failed"
        loadgen_stop 4; loadgen_start "$CUR_RATE"
    fi
    # Loadgen exited early (e.g. internal duration < soak duration or a
    # crash) -> restart with a fresh id base while time remains. The >=5s
    # gap keeps a fast-crashing loadgen from becoming a fork bomb.
    if ! loadgen_alive && [ $((END_S - now)) -gt 30 ] && \
       [ $((now - LOADGEN_LAST_START_S)) -ge 5 ]; then
        event "loadgen_died" ""
        loadgen_start "$CUR_RATE"
    fi

    if [ "$now" -ge "$NEXT_SAMPLE" ]; then
        sample_once
        NEXT_SAMPLE=$((NEXT_SAMPLE + SAMPLE_S))
        [ "$NEXT_SAMPLE" -le "$now" ] && NEXT_SAMPLE=$((now + SAMPLE_S))
    fi
    if [ "$now" -ge "$NEXT_AUDIT" ]; then
        audit_once
        NEXT_AUDIT=$((NEXT_AUDIT + AUDIT_S))
        [ "$NEXT_AUDIT" -le "$now" ] && NEXT_AUDIT=$((now + AUDIT_S))
    fi

    # Sleep until the nearest pending deadline (1s cap keeps SIGINT snappy).
    next_ev=$NEXT_SAMPLE
    [ "$NEXT_AUDIT" -lt "$next_ev" ] && next_ev=$NEXT_AUDIT
    [ "$BURST_UNTIL" -gt 0 ] && [ "$BURST_UNTIL" -lt "$next_ev" ] && next_ev=$BURST_UNTIL
    [ "$END_S" -lt "$next_ev" ] && next_ev=$END_S
    slp=$((next_ev - $(date +%s)))
    [ "$slp" -gt 1 ] && slp=1
    [ "$slp" -gt 0 ] && sleep "$slp" || sleep 0.05
done

cleanup
exit 0
