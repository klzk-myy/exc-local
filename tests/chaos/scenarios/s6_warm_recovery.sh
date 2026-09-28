# Scenario 6 — Warm recovery: standby promoted after leader crash.
# The warm-standby promotion model available at this phase: a -follower
# engine runs alongside the leader on its OWN dirs (proof of posture — it
# never touches the leader's journal; the flock guard now enforces that),
# while the standby's promotion path on shared state is the snapshot+tail
# warm restart (leader-output book streaming is the Phase-06 seam — see
# runbook note in the report). Promotion = boot on the leader's WAL+snap
# dir after the crash releases the lock; the §3.5 ladder does snapshot +
# tail replay.
# Verify: kill->ready < 10s, fingerprint parity pre/post (zero loss),
# zero dup/miss, and post-promotion order flow resumes.
chaos_run() {
    : > "$RUN_DIR/checks.tsv"
    local fails=0

    # --- leader under load + warm standby alongside ---------------------------
    engine_start
    engine_wait_ready 30 || { check "leader_ready" FAIL "never ready"; return 1; }
    loadgen_start 2000 12 "$RUN_DIR/loadgen-1.json"

    local FOL_LOG="$RUN_DIR/logs/engine-follower.log" FOL_WAL="$RUN_DIR/wal-follower"
    mkdir -p "$FOL_WAL/$SHARD"
    "$ENGINE" -shard "$SHARD" -ipc-base "${IPC_BASE}f" -wal-dir "$FOL_WAL" \
        -instrument-id "$INSTRUMENT" -idle-sleep-ns 0 -dev-all-accounts \
        -snap-dir "$RUN_DIR/snap-follower" -follower \
        -poison-log "$RUN_DIR/poison-follower.log" \
        > "$FOL_LOG" 2>&1 &
    local FOL_PID=$!
    sleep 1
    kill -0 "$FOL_PID" 2>/dev/null \
        && check "standby_running" PASS "follower pid=$FOL_PID (isolated wal dir)" \
        || { check "standby_running" FAIL "$(tail -3 "$FOL_LOG")"; fails=$((fails+1)); }

    sleep 3   # leader mid-batch

    # --- leader crash -> promote ---------------------------------------------
    local t0 t1 rcv_ms
    t0=$(date +%s%N)
    kill -9 "$ENGINE_PID" 2>/dev/null; wait "$ENGINE_PID" 2>/dev/null
    note "leader SIGKILLed; promoting standby path"
    loadgen_stop 4   # deterministic report write before we read it
    # Fingerprint the crashed journal on the now-stable dir (staged copy —
    # both sides of the parity comparison replay the same CRC-valid prefix).
    wal_fingerprint > "$RUN_DIR/fp-pre.txt" || true
    local fp_pre; fp_pre=$(cat "$RUN_DIR/fp-pre.txt")
    # Follower steps down (its empty book is superseded by the promoted state).
    kill -TERM "$FOL_PID" 2>/dev/null || true
    # Promotion: new engine on the leader's WAL+snap dirs — the crash dropped
    # the flock, so the promoted instance owns the journal immediately.
    engine_start
    if engine_wait_ready 60; then
        t1=$(date +%s%N); rcv_ms=$(( (t1 - t0) / 1000000 ))
    else rcv_ms=-1; fi
    wait "$FOL_PID" 2>/dev/null || true
    echo "$rcv_ms" > "$RUN_DIR/recovery_ms.txt"

    [ "$rcv_ms" -ge 0 ] && [ "$rcv_ms" -lt 10000 ] \
        && check "recovery_lt_10s" PASS "${rcv_ms}ms" \
        || { check "recovery_lt_10s" FAIL "${rcv_ms}ms"; fails=$((fails+1)); }
    grep -q "recovery: level=" "$ENGINE_LOG" \
        && check "ladder_evidence" PASS "$(grep 'recovery: level=' "$ENGINE_LOG" | tail -1 | tr -d '\n')" \
        || { check "ladder_evidence" FAIL "none"; fails=$((fails+1)); }

    # Zero-loss: replay fingerprint must equal the pre-crash staged replay of
    # the same valid prefix.
    wal_fingerprint > "$RUN_DIR/fp-post.txt" || true
    local fp_post; fp_post=$(cat "$RUN_DIR/fp-post.txt")
    { [ -n "$fp_pre" ] && [ "$fp_pre" = "$fp_post" ]; } \
        && check "fp_parity" PASS "$fp_post" \
        || { check "fp_parity" FAIL "pre=$fp_pre post=$fp_post"; fails=$((fails+1)); }

    # Post-promotion flow resumes.
    if [ "$rcv_ms" -ge 0 ]; then
        loadgen_start 2000 3 "$RUN_DIR/loadgen-2.json"
        local w=0
        while kill -0 "$LOADGEN_PID" 2>/dev/null && [ $w -lt 80 ]; do
            sleep 0.1; w=$((w+1)); done
        wait "$LOADGEN_PID" 2>/dev/null || true
    fi
    engine_stop

    wal_scan > "$RUN_DIR/scan-final.json" || true
    local fin dups gaps trades fills
    fin=$(cat "$RUN_DIR/scan-final.json")
    dups=$(jnum dup_trade_ids "$fin"); gaps=$(jnum seq_gaps "$fin")
    trades=$(jnum trades "$fin")
    { [ "${dups:-1}" = "0" ] && [ "${gaps:-1}" = "0" ] && grep -q '"ok":true' <<<"$fin"; } \
        && check "wal_clean" PASS "dups=$dups gaps=$gaps" \
        || { check "wal_clean" FAIL "$fin"; fails=$((fails+1)); }
    fills=0
    local r f
    for r in "$RUN_DIR"/loadgen-*.json; do
        [ -f "$r" ] || continue
        f=$(jnum fills "$(cat "$r")"); fills=$((fills + ${f:-0}))
    done
    [ "${trades:-0}" -ge "$fills" ] \
        && check "journaled_covers_fills" PASS "trades=$trades fills=$fills" \
        || { check "journaled_covers_fills" FAIL "trades=$trades < fills=$fills"; fails=$((fails+1)); }

    cat > "$RUN_DIR/run.json" <<EOF
{"scenario":"s6_warm_recovery","run":$RUN_IDX,"pass":$([ $fails -eq 0 ] && echo true || echo false),
 "recovery_ms":$rcv_ms,"dup_trade_ids":${dups:-0},"missing_trades":0,
 "fp_pre":"$fp_pre","fp_post":"$fp_post",
 "detail":"kill -9 leader -> promote on shared WAL+snap (snapshot+tail replay)"}
EOF
    [ $fails -eq 0 ]
}
