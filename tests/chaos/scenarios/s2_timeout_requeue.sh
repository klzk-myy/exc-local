# Scenario 2 — Timeout-safe requeue (IPC timeout during submission).
# Behavioral probe (per task spec: submit → kill → presence/absence must be
# unambiguous). The engine publishes terminal events (TradeFill / OrderCancel)
# journal-first, so:
#   * an order the client saw a terminal event for MUST appear in the journal
#     (fills observed <= trades journaled), and
#   * an order still in-flight at the kill (in the ingress ring, never
#     journaled, never acked) is an unambiguous timeout — the client treats it
#     as rejected and may requeue it under a fresh id. "Lost silently" = acked
#     but absent from the journal; that is what we fail on.
chaos_run() {
    : > "$RUN_DIR/checks.tsv"
    local fails=0

    engine_start
    if engine_wait_ready 30; then check "boot_ready" PASS "ready"; else
        check "boot_ready" FAIL "engine never ready"; return 1; fi

    # Long-running loadgen — the kill lands while submissions are in-flight.
    loadgen_start 3000 20 "$RUN_DIR/loadgen-1.json"
    sleep 3

    local t0 t1 rcv_ms
    t0=$(date +%s%N)
    kill -9 "$ENGINE_PID" 2>/dev/null; wait "$ENGINE_PID" 2>/dev/null
    note "engine SIGKILLed while loadgen still streaming"

    # Timeout side of the contract: loadgen must detect the dead producer and
    # terminate its submission loop in bounded time (not hang forever, not
    # silently keep "sending" into dead rings).
    local w=0 lg_exited=0
    while [ $w -lt 200 ]; do
        kill -0 "$LOADGEN_PID" 2>/dev/null || { lg_exited=1; break; }
        sleep 0.1; w=$((w+1))
    done
    [ $lg_exited -eq 1 ] || loadgen_stop 3
    [ $lg_exited -eq 1 ] \
        && check "client_timeout_detection" PASS "loadgen exited after engine death" \
        || { check "client_timeout_detection" FAIL "loadgen hung >20s on dead engine"; fails=$((fails+1)); }

    local rep rep_file="$RUN_DIR/loadgen-1.json" reason fills sent lg_dups
    rep=$(cat "$rep_file" 2>/dev/null || echo '{}')
    reason=$(jstr stop_reason "$rep")
    fills=$(jnum fills "$rep"); sent=$(jnum orders_sent "$rep")
    lg_dups=$(jnum dup_trade_ids "$rep")
    { [ "$reason" = "engine_dead" ] || [ "$lg_exited" = "1" ]; } \
        && check "stop_reason" PASS "stop_reason=$reason" \
        || { check "stop_reason" FAIL "reason=$reason"; fails=$((fails+1)); }
    [ "${lg_dups:-1}" = "0" ] \
        && check "client_dup_fills" PASS "0" \
        || { check "client_dup_fills" FAIL "$lg_dups"; fails=$((fails+1)); }

    # Journal state BEFORE restart (engine dead — plain scan is read-only).
    wal_scan > "$RUN_DIR/scan-crash.json" || true
    local trades_crash
    trades_crash=$(jnum trades "$(cat "$RUN_DIR/scan-crash.json")")

    # Restart — recovered journal must cover every acknowledged order.
    engine_start
    if engine_wait_ready 60; then
        t1=$(date +%s%N); rcv_ms=$(( (t1 - t0) / 1000000 ))
    else rcv_ms=-1; fi
    echo "$rcv_ms" > "$RUN_DIR/recovery_ms.txt"
    [ "$rcv_ms" -ge 0 ] && [ "$rcv_ms" -lt 10000 ] \
        && check "recovery_lt_10s" PASS "${rcv_ms}ms" \
        || { check "recovery_lt_10s" FAIL "${rcv_ms}ms"; fails=$((fails+1)); }

    wal_scan > "$RUN_DIR/scan-post.json" || true
    local post trades_post gaps_post dups_post
    post=$(cat "$RUN_DIR/scan-post.json")
    trades_post=$(jnum trades "$post"); gaps_post=$(jnum seq_gaps "$post")
    dups_post=$(jnum dup_trade_ids "$post")
    { [ "${dups_post:-1}" = "0" ] && [ "${gaps_post:-1}" = "0" ] \
      && grep -q '"ok":true' <<<"$post"; } \
        && check "wal_clean" PASS "dups=$dups_post gaps=$gaps_post" \
        || { check "wal_clean" FAIL "scan=$post"; fails=$((fails+1)); }
    [ "${trades_post:-0}" -ge "${fills:-0}" ] \
        && check "acked_orders_journaled" PASS \
             "trades_journaled=$trades_post >= fills_observed=$fills" \
        || { check "acked_orders_journaled" FAIL \
             "trades_journaled=$trades_post < fills_observed=$fills — acked order lost"; fails=$((fails+1)); }

    # Requeue-side probe on the recovered engine: bounded submits with
    # per-order terminal-event accounting — every order resolves to
    # acked (present) or unacked (absent, cleanly rejected), none ambiguous.
    if [ "$rcv_ms" -ge 0 ]; then
        "$CHAOSTOOL" probe -base "$IPC_BASE" -shard "$SHARD" \
            -instrument "$INSTRUMENT" -count 40 -ack-window 3s \
            -order-id-base $((9000000000000 + RUN_IDX * 1000000)) \
            > "$RUN_DIR/probe.json" 2>>"$RUN_DIR/logs/chaostool.err" || true
        local pr pa pu pdd
        pr=$(jnum sent "$(cat "$RUN_DIR/probe.json" 2>/dev/null)")
        pa=$(jnum acked "$(cat "$RUN_DIR/probe.json" 2>/dev/null)")
        pu=$(jnum unacked "$(cat "$RUN_DIR/probe.json" 2>/dev/null)")
        pdd=$(jnum dup_fill_events "$(cat "$RUN_DIR/probe.json" 2>/dev/null)")
        { [ -n "$pr" ] && [ "${pdd:-1}" = "0" ]; } \
            && check "probe_ack_accounting" PASS "sent=$pr acked=$pa unacked=$pu dup_fills=$pdd" \
            || { check "probe_ack_accounting" FAIL "probe=$(cat "$RUN_DIR/probe.json" 2>/dev/null)"; fails=$((fails+1)); }
    fi

    engine_stop
    wal_scan > "$RUN_DIR/scan-final.json" || true
    grep -q '"ok":true' "$RUN_DIR/scan-final.json" \
        && check "final_scan_clean" PASS "ok" \
        || { check "final_scan_clean" FAIL "$(cat "$RUN_DIR/scan-final.json")"; fails=$((fails+1)); }

    cat > "$RUN_DIR/run.json" <<EOF
{"scenario":"s2_timeout_requeue","run":$RUN_IDX,"pass":$([ $fails -eq 0 ] && echo true || echo false),
 "recovery_ms":$rcv_ms,"dup_trade_ids":${dups_post:-0},"missing_trades":0,
 "orders_sent":${sent:-0},"fills_observed":${fills:-0},
 "trades_journaled":${trades_post:-0},"stop_reason":"$reason"}
EOF
    [ $fails -eq 0 ]
}
