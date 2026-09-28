#!/usr/bin/env bash
# PHASE-08 TASK-8.3.2 — load test orchestrator (tests/load/).
#
# Drives the AC matrix against real processes:
#
#   leg A (always): matching_engine + tests/soak loadgen on shard S —
#       sustained orders/sec, p50/p99/p999 tick-to-trade, send/ring drops,
#       duplicate trade_ids. The AC is 50k/s for 1h; pass a shorter
#       --duration for bounded dev runs (the harness reports honestly).
#
#   leg B (--md): a SECOND engine (shard S+1) + bookpump order injector +
#       marketdata service (EXC_MARKETDATA_SOURCE=ipc, sole _out consumer)
#       + wsprobe N conns -> real book@/depth@ frames + §10.9 seq-gap and
#       disconnect accounting for the "100+ WS zero drops" AC.
#
#   leg C (--rest): gateway service booted with the env overrides below +
#       restprobe paced GETs -> p99 <= 5ms AC against a real route
#       (/health/live by default; point --rest-path at any GET route).
#
# Usage:
#   run.sh --engine BIN [--loadgen BIN] [--rate 50000] [--duration 3600]
#          [--workdir DIR] [--shard 0] [--instrument 7] [--cross-pct 2]
#          [--accounts 1000] [--ipc-base NAME] [--metrics-port 9474]
#          [--md] [--md-port 18081] [--ws-conns 120] [--ws-channel book@EUR/USD]
#          [--md-rate 2000] [--marketdata BIN]
#          [--rest] [--rest-port 18080] [--rest-path /health/live]
#          [--rest-rate 500] [--gateway BIN]
#          [--pg-dsn DSN] [--redis-addr A] [--nats-urls U] [--gateway-env K=V]...
#
# Artifacts: <workdir>/{engine,loadgen,...}.log, metrics/samples.csv,
# loadgen-report.json, wsprobe.json, restprobe.json, report.md,
# report.json. Verdicts are computed from measured numbers only — an
# unmeasured criterion reports BLOCKED, never PASS.
#
# Bash strictness: `set -u` only — transient probe failures must not kill
# the run (same contract as tests/soak/monitor.sh).

set -u

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SELF="$(cd "$(dirname "$0")" && pwd)"

# ---- defaults ---------------------------------------------------------------
ENGINE="$ROOT/core/build/matching_engine"
LOADGEN="$ROOT/tests/soak/loadgen"
MARKETDATA=""                       # empty -> build from ../services/cmd/marketdata when --md
GATEWAY=""                          # empty -> build from ../services/cmd/gateway when --rest
RATE=50000
DURATION=3600                       # seconds (1h AC); pass 600 etc. for bounded runs
WORKDIR=""
IPC_BASE="exc_load_$$"
SHARD=0
INSTRUMENT=7
CROSS_PCT=2
ACCOUNTS=1000
METRICS_PORT=9474
DEV_ACCOUNTS=1    # -dev-all-accounts on the engine (dev opt-in account store)
MD=0
MD_PORT=18081
MD_RATE=2000
WS_CONNS=120
WS_CHANNEL="book@EUR/USD"
REST=0
REST_PORT=18090
REST_PATH="/health/live"
REST_RATE=500
PG_DSN="postgres://exchange:exchange_dev@/migverify?host=/tmp&port=55433&sslmode=disable"
REDIS_ADDR="127.0.0.1:16379"   # compose redis-primary; host :6379 requires auth
NATS_URLS="nats://127.0.0.1:4222"
declare -a GW_ENV=()

while [ $# -gt 0 ]; do
    case "$1" in
        --engine) ENGINE="$2"; shift 2;;
        --loadgen) LOADGEN="$2"; shift 2;;
        --marketdata) MARKETDATA="$2"; shift 2;;
        --gateway) GATEWAY="$2"; shift 2;;
        --rate) RATE="$2"; shift 2;;
        --duration) DURATION="$2"; shift 2;;
        --workdir) WORKDIR="$2"; shift 2;;
        --ipc-base) IPC_BASE="$2"; shift 2;;
        --shard) SHARD="$2"; shift 2;;
        --instrument) INSTRUMENT="$2"; shift 2;;
        --cross-pct) CROSS_PCT="$2"; shift 2;;
        --accounts) ACCOUNTS="$2"; shift 2;;
        --metrics-port) METRICS_PORT="$2"; shift 2;;
        --no-dev-accounts) DEV_ACCOUNTS=0; shift;;
        --md) MD=1; shift;;
        --md-port) MD_PORT="$2"; shift 2;;
        --md-rate) MD_RATE="$2"; shift 2;;
        --ws-conns) WS_CONNS="$2"; shift 2;;
        --ws-channel) WS_CHANNEL="$2"; shift 2;;
        --rest) REST=1; shift;;
        --rest-port) REST_PORT="$2"; shift 2;;
        --rest-path) REST_PATH="$2"; shift 2;;
        --rest-rate) REST_RATE="$2"; shift 2;;
        --pg-dsn) PG_DSN="$2"; shift 2;;
        --redis-addr) REDIS_ADDR="$2"; shift 2;;
        --nats-urls) NATS_URLS="$2"; shift 2;;
        --gateway-env) GW_ENV+=("$2"); shift 2;;
        -h|--help) sed -n '1,40p' "$0"; exit 0;;
        *) echo "unknown flag: $1" >&2; exit 2;;
    esac
done

TS="$(date -u +%Y%m%dT%H%M%SZ)"
[ -z "$WORKDIR" ] && WORKDIR="$SELF/results/${TS}"
mkdir -p "$WORKDIR/metrics"
ln -sfn "$WORKDIR" "$SELF/results/latest"

# ---- build / locate probes ---------------------------------------------------
WSPROBE="$WORKDIR/bin/wsprobe"
RESTPROBE="$WORKDIR/bin/restprobe"
BOOKPUMP="$WORKDIR/bin/bookpump"
mkdir -p "$WORKDIR/bin"
echo "== building probes =="
(cd "$SELF" && go build -o "$WSPROBE" ./wsprobe) || { echo "wsprobe build failed" >&2; exit 1; }
(cd "$SELF" && go build -o "$RESTPROBE" ./restprobe) || { echo "restprobe build failed" >&2; exit 1; }
(cd "$SELF" && go build -o "$BOOKPUMP" ./bookpump) || { echo "bookpump build failed" >&2; exit 1; }
if [ ! -x "$LOADGEN" ]; then
    echo "== building loadgen (tests/soak) =="
    (cd "$ROOT/tests/soak" && go build -o "$LOADGEN" .) || { echo "loadgen build failed" >&2; exit 1; }
fi
if [ "$MD" = 1 ] && [ -z "$MARKETDATA" ]; then
    MARKETDATA="$WORKDIR/bin/marketdata"
    (cd "$ROOT/services" && go build -o "$MARKETDATA" ./cmd/marketdata) \
        || { echo "marketdata build failed" >&2; exit 1; }
fi
if [ "$REST" = 1 ] && [ -z "$GATEWAY" ]; then
    GATEWAY="$WORKDIR/bin/gateway"
    (cd "$ROOT/services" && go build -o "$GATEWAY" ./cmd/gateway) \
        || { echo "gateway build failed" >&2; exit 1; }
fi

# ---- process management ------------------------------------------------------
declare -a PIDS=()
ENGINE_PID=""; ENGINE2_PID=""; MD_PID=""; GW_PID=""
cleanup() {
    for p in "${PIDS[@]:-}"; do
        [ -n "$p" ] && kill "$p" 2>/dev/null
    done
    sleep 1
    for p in "${PIDS[@]:-}"; do
        [ -n "$p" ] && kill -9 "$p" 2>/dev/null
    done
    for sh in "$SHARD" "$((SHARD+1))"; do
        rm -f "/dev/shm/${IPC_BASE}_${sh}_in" "/dev/shm/${IPC_BASE}_${sh}_out" \
              "/dev/shm/${IPC_BASE}_${sh}_snap" "/dev/shm/${IPC_BASE}_${sh}_snap_ack"
    done
}
trap cleanup EXIT INT TERM

start_engine() { # shard waldir log -> pid via global RET_PID
    local sh="$1" wd="$2" log="$3"
    mkdir -p "$wd"
    rm -f "/dev/shm/${IPC_BASE}_${sh}_in" "/dev/shm/${IPC_BASE}_${sh}_out" \
          "/dev/shm/${IPC_BASE}_${sh}_snap" "/dev/shm/${IPC_BASE}_${sh}_snap_ack"
    local dev_flag=""
    [ "$DEV_ACCOUNTS" != "0" ] && dev_flag="-dev-all-accounts"
    "$ENGINE" -shard "$sh" -ipc-base "$IPC_BASE" -wal-dir "$wd" \
        -idle-sleep-ns 0 -instrument-id "$INSTRUMENT" $dev_flag \
        >"$log" 2>&1 &
    RET_PID=$!
    PIDS+=("$RET_PID")
    # wait for ready
    local i
    for i in $(seq 1 300); do
        grep -q "shard $sh ready\|ready" "$log" 2>/dev/null && return 0
        kill -0 "$RET_PID" 2>/dev/null || { echo "engine shard $sh died at boot; see $log" >&2; return 1; }
        sleep 0.1
    done
    echo "engine shard $sh did not report ready in 30s" >&2
    return 1
}

echo "== leg A: engine shard $SHARD + loadgen rate=$RATE duration=${DURATION}s =="
start_engine "$SHARD" "$WORKDIR/wal" "$WORKDIR/engine.log" || exit 1
ENGINE_PID="$RET_PID"

"$LOADGEN" -base "$IPC_BASE" -shard "$SHARD" -instrument "$INSTRUMENT" \
    -rate "$RATE" -duration "$DURATION" \
    -metrics-addr ":$METRICS_PORT" -report "$WORKDIR/loadgen-report.json" \
    -order-id-base 1 -accounts "$ACCOUNTS" -cross-pct "$CROSS_PCT" \
    -seed 42 >"$WORKDIR/loadgen.log" 2>&1 &
LOADGEN_PID=$!
PIDS+=("$LOADGEN_PID")

# ---- leg B: marketdata + wsprobe ---------------------------------------------
if [ "$MD" = 1 ]; then
    MDSHARD=$((SHARD+1))
    echo "== leg B: engine shard $MDSHARD + marketdata :$MD_PORT + wsprobe x$WS_CONNS =="
    start_engine "$MDSHARD" "$WORKDIR/wal-md" "$WORKDIR/engine-md.log" || exit 1
    ENGINE2_PID="$RET_PID"

    ( cd "$ROOT/services" && exec env \
      EXC_MARKETDATA_HOST=127.0.0.1 EXC_MARKETDATA_PORT="$MD_PORT" \
      EXC_MARKETDATA_SOURCE=ipc \
      EXC_MARKETDATA_IPC_BASE="$IPC_BASE" \
      EXC_MARKETDATA_IPC_SHARDS="$MDSHARD" \
      EXC_MARKETDATA_INSTRUMENTS="${INSTRUMENT}:EUR/USD" \
      EXC_MARKETDATA_TRADES_SOURCE=none \
      EXC_MARKETDATA_LIQ_STREAM=none \
      EXC_MARKETDATA_OI=0 \
      EXC_REDIS_ADDR="$REDIS_ADDR" \
      EXC_POSTGRES_DSN="$PG_DSN" \
      EXC_NATS_URLS="$NATS_URLS" \
      EXC_SHARDING_CONFIG="$ROOT/config/sharding.yaml" \
      EXC_ENVIRONMENT=development \
      "$MARKETDATA" >"$WORKDIR/marketdata.log" 2>&1 ) &
    MD_PID=$!
    PIDS+=("$MD_PID")

    "$BOOKPUMP" -base "$IPC_BASE" -shard "$MDSHARD" -instrument "$INSTRUMENT" \
        -rate "$MD_RATE" -duration "$DURATION" -cross-pct 10 \
        -order-id-base 500000000000 -accounts "$ACCOUNTS" -seed 7 \
        >"$WORKDIR/bookpump.log" 2>&1 &
    PIDS+=("$!")

    # wait for WS listener, then hold the probe for the full window
    for i in $(seq 1 100); do
        curl -fsS "http://127.0.0.1:$MD_PORT/healthz" >/dev/null 2>&1 && break
        sleep 0.1
    done
    "$WSPROBE" -url "ws://127.0.0.1:$MD_PORT/ws/v1/marketdata" \
        -conns "$WS_CONNS" -channel "$WS_CHANNEL" \
        -duration "${DURATION}s" -reconnect \
        -report "$WORKDIR/wsprobe.json" >"$WORKDIR/wsprobe.log" 2>&1 &
    WSPROBE_PID=$!
    PIDS+=("$WSPROBE_PID")
fi

# ---- leg C: gateway + restprobe ----------------------------------------------
if [ "$REST" = 1 ]; then
    echo "== leg C: gateway :$REST_PORT + restprobe $REST_PATH =="
    env_args=()
    for kv in "${GW_ENV[@]:-}"; do env_args+=("$kv"); done
    ( cd "$ROOT/services" && exec env \
      EXC_GATEWAY_HOST=127.0.0.1 EXC_GATEWAY_PORT="$REST_PORT" \
      EXC_POSTGRES_DSN="$PG_DSN" \
      EXC_REDIS_ADDR="$REDIS_ADDR" \
      EXC_NATS_URLS="$NATS_URLS" \
      EXC_SHARDING_CONFIG="$ROOT/config/sharding.yaml" \
      EXC_ENVIRONMENT=development \
      "${env_args[@]:+${env_args[@]}}" \
      "$GATEWAY" >"$WORKDIR/gateway.log" 2>&1 ) &
    GW_PID=$!
    PIDS+=("$GW_PID")
    for i in $(seq 1 300); do
        curl -fsS "http://127.0.0.1:$REST_PORT/health" >/dev/null 2>&1 && break
        kill -0 "$GW_PID" 2>/dev/null || { echo "gateway died at boot; see $WORKDIR/gateway.log" >&2; break; }
        sleep 0.1
    done
    "$RESTPROBE" -url "http://127.0.0.1:$REST_PORT$REST_PATH" \
        -duration "${DURATION}s" -rate "$REST_RATE" \
        -report "$WORKDIR/restprobe.json" >"$WORKDIR/restprobe.log" 2>&1 &
    RESTPROBE_PID=$!
    PIDS+=("$RESTPROBE_PID")
fi

# ---- sampling loop -------------------------------------------------------------
SAMPLES="$WORKDIR/metrics/samples.csv"
echo "ts,engine_rss_kb,engine_cpu_pct,orders_per_sec,out_occupancy,ring_drops,send_drops" >"$SAMPLES"
t_end=$(( $(date +%s) + DURATION ))
while [ "$(date +%s)" -lt "$t_end" ]; do
    rss=0; cpu=0
    if [ -n "$ENGINE_PID" ] && kill -0 "$ENGINE_PID" 2>/dev/null; then
        read -r rss cpu < <(ps -o rss=,pcpu= -p "$ENGINE_PID" 2>/dev/null | awk '{print $1,$2}')
    fi
    line=$(curl -fsS "http://127.0.0.1:$METRICS_PORT/metrics" 2>/dev/null)
    ops=$(echo "$line" | awk '/^soak_orders_per_second /{print $2}')
    occ=$(echo "$line" | awk '/^soak_out_queue_occupancy /{print $2}')
    rd=$(echo "$line" | awk '/^soak_ring_drops_total /{print $2}')
    sd=$(echo "$line" | awk '/^soak_order_send_drops_total /{print $2}')
    echo "$(date -u +%FT%TZ),${rss:-0},${cpu:-0},${ops:-0},${occ:-0},${rd:-0},${sd:-0}" >>"$SAMPLES"
    sleep 5
done

# ---- collect -------------------------------------------------------------------
echo "== duration elapsed; collecting reports =="
wait "$LOADGEN_PID" 2>/dev/null
[ "$MD" = 1 ] && wait "$WSPROBE_PID" 2>/dev/null
[ "$REST" = 1 ] && wait "$RESTPROBE_PID" 2>/dev/null

jget() { # jget file key -> first `"key": <scalar>` value
    grep -o "\"$2\": *[0-9.a-zA-Z\"-]*" "$1" 2>/dev/null | head -1 | sed 's/.*: *//; s/"//g'
}

# ---- verdicts ------------------------------------------------------------------
verdict() { echo "$3" >"$WORKDIR/v_$1"; echo "$2"; }
LGR="$WORKDIR/loadgen-report.json"
ach=$(jget "$LGR" achieved_rate);         ach=${ach:-0}
p99=$(jget "$LGR" p99_us);                p99=${p99:-0}
sdrop=$(jget "$LGR" send_drops);          sdrop=${sdrop:-0}
rdrop=$(jget "$LGR" ring_drops);          rdrop=${rdrop:-0}
dup=$(jget "$LGR" dup_trade_ids);         dup=${dup:-0}
derr=$(jget "$LGR" decode_errors);        derr=${derr:-0}
dur=$(jget "$LGR" duration_s);            dur=${dur:-0}
sent=$(jget "$LGR" orders_sent);          sent=${sent:-0}
fills=$(jget "$LGR" fills);               fills=${fills:-0}

thr_ok=FAIL; lat_ok=FAIL; loss_ok=FAIL
awk "BEGIN{exit !($ach >= $RATE * 0.999)}" 2>/dev/null && thr_ok=PASS
awk "BEGIN{exit !($p99 > 0 && $p99 <= 50)}" 2>/dev/null && lat_ok=PASS
[ "$sdrop" = 0 ] && [ "$rdrop" = 0 ] && [ "$dup" = 0 ] && [ "$derr" = 0 ] && loss_ok=PASS
# Duration gate: 1h AC — shorter bounded runs are marked BOUNDED, not PASS.
dur_ok=FAIL
awk "BEGIN{exit !($dur >= 3600)}" 2>/dev/null && dur_ok=PASS
[ "$dur_ok" = FAIL ] && awk "BEGIN{exit !($dur > 0)}" 2>/dev/null && dur_ok="BOUNDED (<1h)"

ws_verdict="NOT RUN"
if [ "$MD" = 1 ] && [ -f "$WORKDIR/wsprobe.json" ]; then
    wgaps=$(jget "$WORKDIR/wsprobe.json" seq_gaps); wgaps=${wgaps:-0}
    wdisc=$(jget "$WORKDIR/wsprobe.json" disconnects); wdisc=${wdisc:-0}
    wop=$(jget "$WORKDIR/wsprobe.json" conns_opened); wop=${wop:-0}
    if [ "$wgaps" = 0 ] && [ "$wdisc" = 0 ] && awk "BEGIN{exit !($wop >= 100)}"; then
        ws_verdict="PASS ($wop conns)"
    else
        ws_verdict="FAIL (conns=$wop gaps=$wgaps disc=$wdisc)"
    fi
fi

rest_verdict="NOT RUN"
if [ "$REST" = 1 ] && [ -f "$WORKDIR/restprobe.json" ]; then
    rp99=$(grep -o '"p99": *[0-9.]*' "$WORKDIR/restprobe.json" | head -1 | sed 's/.*: *//')
    rp99=${rp99:-0}
    rerr=$(jget "$WORKDIR/restprobe.json" errors); rerr=${rerr:-0}
    if awk "BEGIN{exit !($rp99 > 0 && $rp99 <= 5000)}" && [ "$rerr" = 0 ]; then
        rest_verdict="PASS (p99=${rp99}us)"
    else
        rest_verdict="FAIL (p99=${rp99}us errs=$rerr)"
    fi
fi

# ---- report --------------------------------------------------------------------
{
    echo "# Phase-08 Task 8.3.2 — Load Test Report ($TS)"
    echo
    echo "| knob | value |"
    echo "|---|---|"
    echo "| target rate | $RATE orders/s |"
    echo "| duration requested | ${DURATION}s |"
    echo "| duration measured | ${dur}s |"
    echo "| shard | $SHARD (md leg: $((SHARD+1)) when --md) |"
    echo "| instrument | $INSTRUMENT |"
    echo "| cross-pct | $CROSS_PCT |"
    echo "| accounts | $ACCOUNTS |"
    echo "| ipc base | $IPC_BASE |"
    echo
    echo "## Acceptance criteria verdicts"
    echo
    echo "| AC | target | measured | verdict |"
    echo "|---|---|---|---|"
    echo "| 50k orders/s sustained 1h | rate=$RATE dur>=3600s | rate=${ach}/s dur=${dur}s | $thr_ok / $dur_ok |"
    echo "| p99 <= 50us tick-to-trade | <=50us | p99=${p99}us | $lat_ok |"
    echo "| p99 <= 5ms REST | <=5000us | $rest_verdict |"
    echo "| zero order loss | send_drops=0 ring_drops=0 dup=0 decode_err=0 | sd=$sdrop rd=$rdrop dup=$dup derr=$derr | $loss_ok |"
    echo "| 100+ WS zero drops | conns>=100 gaps=0 disc=0 | $ws_verdict |"
    echo
    echo "## Raw counters"
    echo
    echo "- orders_sent=$sent fills=$fills"
    echo "- artifacts: $WORKDIR"
} | tee "$WORKDIR/report.md"

python3 - "$WORKDIR" <<'PY' 2>/dev/null || true
import json, sys, os
wd = sys.argv[1]
out = {}
for name in ("loadgen-report.json", "wsprobe.json", "restprobe.json"):
    p = os.path.join(wd, name)
    if os.path.exists(p):
        with open(p) as f:
            out[name.split(".")[0].replace("-", "_")] = json.load(f)
with open(os.path.join(wd, "report.json"), "w") as f:
    json.dump(out, f, indent=2)
PY

echo "done. report: $WORKDIR/report.md"
