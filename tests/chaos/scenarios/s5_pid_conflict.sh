# Scenario 5 — PID/single-writer conflict must fail closed.
# Two engine processes contend for the same shard WAL dir + shm rings. The
# kernel flock on <wal-dir>/<shard>/engine.lock (core/src/main.cpp, added for
# this scenario — no guard existed before) must reject the second process
# before it can attach to either resource: exit != 0, no "ready" line, no
# WAL writes. Then verify the lock is not sticky: after a clean stop, a
# fresh engine acquires it and becomes ready.
chaos_run() {
    : > "$RUN_DIR/checks.tsv"
    local fails=0

    engine_start
    engine_wait_ready 30 || { check "leader_ready" FAIL "never ready"; return 1; }
    loadgen_start 2000 8 "$RUN_DIR/loadgen-1.json"   # background traffic
    sleep 1

    # --- contender: identical resources --------------------------------------
    local contender_log="$RUN_DIR/logs/engine-contender.log" contender_rc=0
    # timeout guards the harness: if the guard ever regressed, the contender
    # would run forever — bounded at 15s, and timeout(124) still counts as
    # "did not fail closed" because the process was alive at the deadline.
    timeout 15 "$ENGINE" -shard "$SHARD" -ipc-base "$IPC_BASE" -wal-dir "$WAL_ROOT" \
        -instrument-id "$INSTRUMENT" -idle-sleep-ns 0 -dev-all-accounts \
        -snap-dir "$SNAP_ROOT" -poison-log "$RUN_DIR/poison-contender.log" \
        -report-log "$RUN_DIR/reports-contender.jsonl" \
        > "$contender_log" 2>&1
    contender_rc=$?
    note "contender exited rc=$contender_rc"

    { [ "$contender_rc" != "0" ] && [ "$contender_rc" != "124" ]; } \
        && check "contender_fail_closed" PASS "exit=$contender_rc" \
        || { check "contender_fail_closed" FAIL "rc=$contender_rc — contender stayed up (dual-write window)"; fails=$((fails+1)); }
    grep -q "FATAL: shard lock unavailable" "$contender_log" \
        && check "contender_reason" PASS "shard lock refused" \
        || { check "contender_reason" FAIL "$(tail -2 "$contender_log")"; fails=$((fails+1)); }
    ! grep -q "ready at" "$contender_log" \
        && check "contender_no_ready" PASS "never reached ingress" \
        || { check "contender_no_ready" FAIL "contender became ready"; fails=$((fails+1)); }

    # --- leader unharmed and still processing --------------------------------
    kill -0 "$ENGINE_PID" 2>/dev/null \
        && check "leader_alive" PASS "pid=$ENGINE_PID" \
        || { check "leader_alive" FAIL "leader died"; fails=$((fails+1)); }
    sleep 2   # let loadgen keep flowing
    wal_scan > "$RUN_DIR/scan-mid.json" || true
    local mid dups_mid
    mid=$(cat "$RUN_DIR/scan-mid.json"); dups_mid=$(jnum dup_trade_ids "$mid")
    { [ "${dups_mid:-1}" = "0" ] && grep -q '"ok":true' <<<"$mid"; } \
        && check "wal_clean_during_contention" PASS "dups=$dups_mid" \
        || { check "wal_clean_during_contention" FAIL "$mid"; fails=$((fails+1)); }

    loadgen_stop 5
    engine_stop   # clean stop releases the flock

    # --- lock is not sticky: a fresh engine must boot -------------------------
    local t0 t1 rcv_ms
    t0=$(date +%s%N)
    engine_start
    if engine_wait_ready 30; then
        t1=$(date +%s%N); rcv_ms=$(( (t1 - t0) / 1000000 ))
    else rcv_ms=-1; fi
    echo "$rcv_ms" > "$RUN_DIR/recovery_ms.txt"
    [ "$rcv_ms" -ge 0 ] && [ "$rcv_ms" -lt 10000 ] \
        && check "reacquire_after_stop" PASS "${rcv_ms}ms" \
        || { check "reacquire_after_stop" FAIL "${rcv_ms}ms"; fails=$((fails+1)); }

    engine_stop
    wal_scan > "$RUN_DIR/scan-final.json" || true
    local fin dups_fin gaps_fin
    fin=$(cat "$RUN_DIR/scan-final.json")
    dups_fin=$(jnum dup_trade_ids "$fin"); gaps_fin=$(jnum seq_gaps "$fin")
    { [ "${dups_fin:-1}" = "0" ] && [ "${gaps_fin:-1}" = "0" ] \
      && grep -q '"ok":true' <<<"$fin"; } \
        && check "final_scan_clean" PASS "dups=$dups_fin gaps=$gaps_fin" \
        || { check "final_scan_clean" FAIL "$fin"; fails=$((fails+1)); }

    cat > "$RUN_DIR/run.json" <<EOF
{"scenario":"s5_pid_conflict","run":$RUN_IDX,"pass":$([ $fails -eq 0 ] && echo true || echo false),
 "recovery_ms":$rcv_ms,"dup_trade_ids":${dups_fin:-0},"missing_trades":0,
 "contender_rc":$contender_rc,"detail":"flock guard on wal/<shard>/engine.lock"}
EOF
    [ $fails -eq 0 ]
}
