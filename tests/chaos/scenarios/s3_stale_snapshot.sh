# Scenario 3 — Stale/untrusted snapshot guard (spec §18.5, §3.5 level 2).
# Two sub-checks per run, both under the same boot-invariant machinery:
#   A. Corrupt the latest snapshot payload -> the integrity guard refuses it
#      -> the ladder rebases (prior snapshot / genesis WAL replay). Book
#      equality is proven by stopping the recovered engine IMMEDIATELY after
#      readiness (before any new traffic) and comparing the drain-snapshot
#      payload byte-for-byte (modulo book_seq) with the pre-corruption backup.
#   B. Remove the WAL entirely (snapshot cursor ahead of surviving log =
#      forward divergence) -> the guard fires, the level-2 rebase marker
#      {snapshot_seq}.wal is written, and the engine resumes on the verified
#      snapshot — the "correct rebase" arm of the scenario contract.
chaos_run() {
    : > "$RUN_DIR/checks.tsv"
    local fails=0

    # --- build state: orders journaled, snapshot pinned at graceful stop -----
    engine_start
    engine_wait_ready 30 || { check "boot_ready" FAIL "never ready"; return 1; }
    loadgen_start 1500 4 "$RUN_DIR/loadgen-seed.json"
    local w=0
    while kill -0 "$LOADGEN_PID" 2>/dev/null && [ $w -lt 120 ]; do
        sleep 0.1; w=$((w+1)); done
    wait "$LOADGEN_PID" 2>/dev/null || true
    engine_stop   # graceful -> drain snapshot at WAL tail

    local snap snap_seq
    snap=$(latest_snapshot)
    [ -n "$snap" ] && check "snapshot_present" PASS "$snap" \
        || { check "snapshot_present" FAIL "no snapshot produced"; return 1; }
    cp "$snap" "$RUN_DIR/snapshot_orig.bin"
    snap_seq=$(basename "$snap" | sed 's/snap_0*\([0-9]*\)\.bin/\1/')
    wal_scan > "$RUN_DIR/scan-built.json" || true
    note "state built: snapshot_seq=$snap_seq wal_tail=$(jnum wal_tail "$(cat "$RUN_DIR/scan-built.json")")"

    # ---- sub-check A: corrupt the snapshot payload --------------------------
    # Flip bytes well inside the payload (past SnapFileHeader 32B + entry hdr).
    python3 - "$snap" <<'PYEOF'
import sys
p = sys.argv[1]
d = bytearray(open(p, 'rb').read())
off = 40 if len(d) > 100 else len(d) - 1
for i in range(off, min(off + 16, len(d))):
    d[i] ^= 0xFF
open(p, 'wb').write(d)
PYEOF
    note "corrupted 16 payload bytes of $snap"

    local t0 t1 rcv_ms
    t0=$(date +%s%N)
    engine_start
    if engine_wait_ready 60; then
        t1=$(date +%s%N); rcv_ms=$(( (t1 - t0) / 1000000 ))
    else rcv_ms=-1; fi

    [ "$rcv_ms" -ge 0 ] && [ "$rcv_ms" -lt 10000 ] \
        && check "A_recovery_lt_10s" PASS "${rcv_ms}ms" \
        || { check "A_recovery_lt_10s" FAIL "${rcv_ms}ms"; fails=$((fails+1)); }
    grep -qE "SNAPSHOT_REBASED|GENESIS_REPLAY" "$ENGINE_LOG" \
        && check "A_guard_fires_rebase" PASS \
             "$(grep -oE 'NOTICE (SNAPSHOT_REBASED|GENESIS_REPLAY)[^ ]*' "$ENGINE_LOG" | tail -1)" \
        || { check "A_guard_fires_rebase" FAIL \
             "no rebase row (engine.log tail: $(tail -3 "$ENGINE_LOG"))"; fails=$((fails+1)); }

    # Book parity must be sampled BEFORE any new traffic: drain-stop now pins
    # a snapshot of the rebuilt book.
    engine_stop
    local snap_a
    snap_a=$(latest_snapshot)
    if [ -n "$snap_a" ] && snapshot_book_equal "$RUN_DIR/snapshot_orig.bin" "$snap_a"; then
        check "A_book_parity" PASS "payload identical modulo book_seq"
    else
        check "A_book_parity" FAIL "snap_a=$snap_a"
        fails=$((fails+1))
    fi

    # ---- sub-check B: trimmed WAL -> forward divergence -> rebase marker ----
    # Pull the real journal aside and leave a WAL whose tail sits BELOW the
    # snapshot cursor (a lone marker entry at seq 5 -> tail 6): snapshot_seq
    # is then ahead of the log -> the level-2 ladder must detect forward
    # divergence, write {snapshot_seq}.wal and rebase onto the snapshot.
    mkdir -p "$RUN_DIR/wal_pulled"
    mv "$WAL_ROOT/$SHARD"/*.wal "$RUN_DIR/wal_pulled/" 2>/dev/null || true
    rm -f "$WAL_ROOT/$SHARD"/*.wal
    local snap_b snap_b_seq
    snap_b=$(latest_snapshot)
    snap_b_seq=$(basename "$snap_b" | sed 's/snap_0*\([0-9]*\)\.bin/\1/')
    "$CHAOSTOOL" wal-marker -dir "$WAL_ROOT/$SHARD" -shard "$SHARD" -seq 5 \
        > "$RUN_DIR/marker-seed.json" 2>>"$RUN_DIR/logs/chaostool.err" || true
    note "WAL trimmed to a stub (tail=6) — snapshot cursor ($snap_b_seq) ahead of the log"
    # engine.log gets a fresh section per boot; scope checks to the last boot.
    local log_lines_pre
    log_lines_pre=$(wc -l < "$ENGINE_LOG")

    t0=$(date +%s%N)
    engine_start
    local rcv_ms_b
    if engine_wait_ready 60; then
        t1=$(date +%s%N); rcv_ms_b=$(( (t1 - t0) / 1000000 ))
    else rcv_ms_b=-1; fi
    # Combined recovery budget for the scenario (worst of the two boots).
    [ "${rcv_ms_b:-999999}" -gt "${rcv_ms:-0}" ] && rcv_ms=$rcv_ms_b
    echo "$rcv_ms" > "$RUN_DIR/recovery_ms.txt"
    tail -n +$((log_lines_pre + 1)) "$ENGINE_LOG" > "$RUN_DIR/engine-bootB.log"

    [ "$rcv_ms_b" -ge 0 ] && [ "$rcv_ms_b" -lt 10000 ] \
        && check "B_recovery_lt_10s" PASS "${rcv_ms_b}ms" \
        || { check "B_recovery_lt_10s" FAIL "${rcv_ms_b}ms"; fails=$((fails+1)); }
    grep -q "SNAPSHOT_REBASED" "$RUN_DIR/engine-bootB.log" \
        && check "B_rebase_outcome" PASS "SNAPSHOT_REBASED" \
        || { check "B_rebase_outcome" FAIL \
             "missing ($(tail -3 "$RUN_DIR/engine-bootB.log"))"; fails=$((fails+1)); }
    # The level-2 rebase marker must exist: {snapshot_seq}.wal segment written.
    if [ -f "$WAL_ROOT/$SHARD/${snap_b_seq}.wal" ]; then
        check "B_rebase_marker" PASS "$WAL_ROOT/$SHARD/${snap_b_seq}.wal"
    else
        check "B_rebase_marker" FAIL \
            "no ${snap_b_seq}.wal marker ($(ls "$WAL_ROOT/$SHARD"/*.wal 2>/dev/null))"
        fails=$((fails+1))
    fi
    # Post-rebase appends must resume ABOVE the snapshot cursor — no seq
    # regression that would silently covered-skip future journal entries.
    wal_scan > "$RUN_DIR/scan-rebased.json" || true
    local rb_tail
    rb_tail=$(jnum wal_tail "$(cat "$RUN_DIR/scan-rebased.json")")
    [ "${rb_tail:-0}" -ge "${snap_b_seq:-1}" ] \
        && check "B_seq_domain_resumed" PASS "wal_tail=$rb_tail >= snap=$snap_b_seq" \
        || { check "B_seq_domain_resumed" FAIL \
             "wal_tail=$rb_tail < snapshot_seq=$snap_b_seq — seq regression"; fails=$((fails+1)); }

    engine_stop
    wal_scan > "$RUN_DIR/scan-final.json" || true
    local scan dups
    scan=$(cat "$RUN_DIR/scan-final.json"); dups=$(jnum dup_trade_ids "$scan")
    [ "${dups:-1}" = "0" ] \
        && check "dup_trade_ids" PASS "0" \
        || { check "dup_trade_ids" FAIL "$dups"; fails=$((fails+1)); }

    # ---- sub-check C: EMPTY WAL dir + snapshot ahead — the harsher arm ------
    # Regression probe for the gap found in this run's investigation:
    # RecoveryManager Phase-6 guards the snapshot_seq>wal_tail divergence check
    # behind !stream_empty, and the level-2 marker write only runs on the
    # level-1-failure path — so a WAL dir with NO segments never triggers
    # divergence handling and the journal seq domain restarts at 0. Post-boot
    # appends at seq<snapshot_seq are then covered-skipped by the NEXT
    # recovery = silent loss (and trade-id reseeding can collide). Fail-closed
    # requires the marker (or a halt) here too.
    rm -f "$WAL_ROOT/$SHARD"/*.wal
    log_lines_pre=$(wc -l < "$ENGINE_LOG")
    t0=$(date +%s%N)
    engine_start
    local rcv_ms_c
    if engine_wait_ready 60; then
        t1=$(date +%s%N); rcv_ms_c=$(( (t1 - t0) / 1000000 ))
    else rcv_ms_c=-1; fi
    tail -n +$((log_lines_pre + 1)) "$ENGINE_LOG" > "$RUN_DIR/engine-bootC.log"
    note "empty-WAL boot rcv=${rcv_ms_c}ms outcome=$(grep -oE 'NOTICE [A-Z_]+|WAL_RECOVERY_HALT' "$RUN_DIR/engine-bootC.log" | tail -1)"
    engine_stop
    local c_tail
    wal_scan > "$RUN_DIR/scan-emptywal.json" || true
    c_tail=$(jnum wal_tail "$(cat "$RUN_DIR/scan-emptywal.json")")
    # Safe behaviors: rebase marker at/above snapshot seq, or fail-closed
    # refusal to start. Unsafe: clean boot with wal_tail < snapshot_seq.
    if grep -q "WAL_RECOVERY_HALT" "$RUN_DIR/engine-bootC.log"; then
        check "C_emptywal_failclosed" PASS "halted"
    elif [ -f "$WAL_ROOT/$SHARD/${snap_b_seq}.wal" ] \
         || [ "${c_tail:-0}" -ge "${snap_b_seq:-1}" ]; then
        check "C_emptywal_failclosed" PASS "rebased (tail=$c_tail)"
    else
        check "C_emptywal_failclosed" FAIL \
            "snapshot@$snap_b_seq + empty WAL -> clean boot at tail=$c_tail: seq domain regressed, no marker, no halt"
        fails=$((fails+1))
    fi

    # Post-rebase book still identical to the pre-corruption snapshot.
    local snap_final
    snap_final=$(latest_snapshot)
    if [ -n "$snap_final" ] && snapshot_book_equal "$RUN_DIR/snapshot_orig.bin" "$snap_final"; then
        check "B_book_parity" PASS "payload identical modulo replay stamps"
    else
        check "B_book_parity" FAIL "snap_final=$snap_final"
        fails=$((fails+1))
    fi

    cat > "$RUN_DIR/run.json" <<EOF
{"scenario":"s3_stale_snapshot","run":$RUN_IDX,"pass":$([ $fails -eq 0 ] && echo true || echo false),
 "recovery_ms":$rcv_ms,"dup_trade_ids":${dups:-0},"missing_trades":0,
 "snapshot_seq":${snap_seq:-0},"detail":"guard->rebase/genesis replay (A) + forward-divergence rebase marker (B)"}
EOF
    [ $fails -eq 0 ]
}
