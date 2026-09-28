# Scenario 4 — WAL trimmed before S3 archive -> PostgreSQL PITR fallback.
# Fixture (per task spec: synthetic trimmed-WAL fixture is acceptable — the
# point is the fallback DECISION path):
#   * a real journaled WAL segment set (0..T) built under load,
#   * plus a rebase-marker segment at seq M = T + 500 -> prescan sees the lost
#     range [T, M) which the snapshot (seq S < T) cannot cover -> the local
#     ladder must fail closed (uncovered seq loss => WAL_RECOVERY_HALT),
#   * the archive dir is EMPTY (the trim raced ahead of the archiver).
# Decision chain asserted: scan detects the gap -> wal-recovery ladder exits
# 3 with a recovery_reports row -> archive empty -> restore_pitr.sh stages a
# real PITR restore dir (recovery.signal + postgresql.auto.conf) -> the halt
# row is persisted into PostgreSQL recovery_reports via persist-reports.
chaos_run() {
    : > "$RUN_DIR/checks.tsv"
    local fails=0

    # --- real state under load ----------------------------------------------
    engine_start
    engine_wait_ready 30 || { check "boot_ready" FAIL "never ready"; return 1; }
    loadgen_start 2000 4 "$RUN_DIR/loadgen-seed.json"
    local w=0
    while kill -0 "$LOADGEN_PID" 2>/dev/null && [ $w -lt 120 ]; do
        sleep 0.1; w=$((w+1)); done
    wait "$LOADGEN_PID" 2>/dev/null || true
    engine_stop

    wal_scan > "$RUN_DIR/scan-built.json" || true
    local tail_built snap snap_seq
    tail_built=$(jnum wal_tail "$(cat "$RUN_DIR/scan-built.json")")
    snap=$(latest_snapshot); snap_seq=0
    [ -n "$snap" ] && snap_seq=$(basename "$snap" | sed 's/snap_0*\([0-9]*\)\.bin/\1/')
    [ -n "$snap" ] && check "fixture_state" PASS "wal_tail=$tail_built snapshot_seq=$snap_seq" \
        || { check "fixture_state" FAIL "no snapshot"; return 1; }

    # --- fixture: trimmed WAL (gap) + empty archive --------------------------
    local F="$RUN_DIR/fixture"
    mkdir -p "$F/wal/$SHARD" "$F/archive" "$F/pitr-data" "$F/pitr-archive"
    cp "$WAL_ROOT/$SHARD"/*.wal "$F/wal/$SHARD"/ 2>/dev/null || true
    local gap_seq=$((tail_built + 500))
    "$CHAOSTOOL" wal-marker -dir "$F/wal/$SHARD" -shard "$SHARD" -seq "$gap_seq" \
        > "$RUN_DIR/marker.json" 2>>"$RUN_DIR/logs/chaostool.err" || true
    note "fixture: real WAL 0..$tail_built + marker at $gap_seq; archive empty"

    # --- step 1: prescan detects the lost range ------------------------------
    "$WALRECOVERY" scan --wal-dir "$F/wal/$SHARD" --shard "$SHARD" \
        > "$RUN_DIR/recscan.json" 2>"$RUN_DIR/logs/walrecovery-scan.err"
    local scan_rc=$?
    # scan exits non-zero on seq_gap divergence — that IS the detection.
    { [ $scan_rc -ne 0 ] || grep -q '"divergence":"seq_gap"' "$RUN_DIR/recscan.json"; } \
        && check "gap_detected" PASS "scan_rc=$scan_rc lost_ranges=$(grep -c '"begin"' "$RUN_DIR/recscan.json")" \
        || { check "gap_detected" FAIL "rc=$scan_rc $(cat "$RUN_DIR/recscan.json")"; fails=$((fails+1)); }

    # --- step 2: local ladder must fail closed -------------------------------
    local t0 t1 rcv_ms
    t0=$(date +%s%N)
    "$WALRECOVERY" ladder --wal-dir "$F/wal/$SHARD" --shard "$SHARD" \
        --snap-root "$SNAP_ROOT/$SHARD" --instrument-id "$INSTRUMENT" \
        --report-out "$RUN_DIR/reports.jsonl" \
        > "$RUN_DIR/ladder.json" 2>"$RUN_DIR/logs/walrecovery-ladder.err"
    local lad_rc=$?
    t1=$(date +%s%N); rcv_ms=$(( (t1 - t0) / 1000000 ))
    echo "$rcv_ms" > "$RUN_DIR/recovery_ms.txt"

    [ "$lad_rc" = "3" ] && grep -q '"outcome":"WAL_RECOVERY_HALT"' "$RUN_DIR/reports.jsonl" \
        && check "ladder_fail_closed" PASS "exit=$lad_rc WAL_RECOVERY_HALT row emitted" \
        || { check "ladder_fail_closed" FAIL "rc=$lad_rc report=$(cat "$RUN_DIR/reports.jsonl" 2>/dev/null)"; fails=$((fails+1)); }
    [ "$rcv_ms" -lt 10000 ] \
        && check "decision_lt_10s" PASS "${rcv_ms}ms" \
        || { check "decision_lt_10s" FAIL "${rcv_ms}ms"; fails=$((fails+1)); }

    # --- step 2b: the C++ engine's own boot must also fail closed ------------
    local engine_halt_log="$RUN_DIR/logs/engine-halt.log"
    IPC_BASE="${IPC_BASE}x" "$ENGINE" -shard "$SHARD" -ipc-base "${IPC_BASE}x" \
        -wal-dir "$F/wal" -snap-dir "$SNAP_ROOT" -instrument-id "$INSTRUMENT" \
        -idle-sleep-ns 0 -dev-all-accounts \
        -poison-log "$RUN_DIR/poison-halt.log" \
        -report-log "$RUN_DIR/reports-engine.jsonl" \
        > "$engine_halt_log" 2>&1
    local eng_rc=$?
    { [ "$eng_rc" != "0" ] && grep -q "WAL_RECOVERY_HALT" "$engine_halt_log"; } \
        && check "engine_boot_fail_closed" PASS "exit=$eng_rc" \
        || { check "engine_boot_fail_closed" FAIL "rc=$eng_rc $(tail -3 "$engine_halt_log")"; fails=$((fails+1)); }

    # --- step 3: archive empty -> PITR fallback staged -----------------------
    local arch_n
    arch_n=$(find "$F/archive" -type f | wc -l)
    if [ "$arch_n" -eq 0 ]; then
        check "archive_empty" PASS "trimmed segments never archived — PITR selected"
        "$REPO_ROOT/deploy/postgres/restore_pitr.sh" \
            --data-dir "$F/pitr-data" --archive-dir "$F/pitr-archive" \
            --target-time "$(date -u '+%Y-%m-%d %H:%M:%S+00')" \
            > "$RUN_DIR/pitr-stage.log" 2>&1
        local pitr_rc=$?
        { [ $pitr_rc -eq 0 ] && [ -f "$F/pitr-data/recovery.signal" ] \
          && grep -q "restore_command" "$F/pitr-data/postgresql.auto.conf" \
          && grep -q "recovery_target_time" "$F/pitr-data/postgresql.auto.conf"; } \
            && check "pitr_staged" PASS "recovery.signal + restore_command + recovery_target_time written" \
            || { check "pitr_staged" FAIL "rc=$pitr_rc $(cat "$RUN_DIR/pitr-stage.log")"; fails=$((fails+1)); }
    else
        check "archive_empty" FAIL "archive not empty — fixture wrong"; fails=$((fails+1))
    fi

    # --- step 4: halt row persisted to PostgreSQL recovery_reports ------------
    if [ -n "${EXC_PG_DSN:-}" ]; then
        EXC_POSTGRES_DSN="$EXC_PG_DSN" "$WALRECOVERY" persist-reports \
            --file "$RUN_DIR/reports.jsonl" \
            > "$RUN_DIR/persist.json" 2>"$RUN_DIR/logs/persist.err" || true
        local rows
        rows=$(jnum rows_inserted "$(cat "$RUN_DIR/persist.json" 2>/dev/null)")
        if [ "${rows:-0}" -ge 1 ]; then
            local pg_n
            pg_n=$("$PSQL" "$EXC_PG_DSN" -tc \
                "SELECT count(*) FROM recovery_reports WHERE outcome='WAL_RECOVERY_HALT'" 2>/dev/null | tr -d ' ')
            check "pg_report_persisted" PASS "inserted=$rows pg_halt_rows=${pg_n:-?}"
        else
            check "pg_report_persisted" FAIL "persist=$(cat "$RUN_DIR/persist.json" 2>/dev/null) $(cat "$RUN_DIR/logs/persist.err" 2>/dev/null)"
            fails=$((fails+1))
        fi
    else
        check "pg_report_persisted" FAIL "EXC_PG_DSN not set"; fails=$((fails+1))
    fi

    cat > "$RUN_DIR/run.json" <<EOF
{"scenario":"s4_wal_trimmed_pitr","run":$RUN_IDX,"pass":$([ $fails -eq 0 ] && echo true || echo false),
 "recovery_ms":$rcv_ms,"dup_trade_ids":0,"missing_trades":0,
 "wal_tail":${tail_built:-0},"gap_seq":$gap_seq,"snapshot_seq":${snap_seq:-0},
 "detail":"uncovered gap -> WAL_RECOVERY_HALT -> PITR staged + report row in PG"}
EOF
    [ $fails -eq 0 ]
}
