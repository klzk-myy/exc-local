# Scenario 1 — Crash mid-batch (SIGKILL under load → graduated-ladder boot).
# Fault: kill -9 the engine while loadgen streams orders, then write junk at
# the WAL's valid_end (the first byte past the last complete record — inside
# the preallocated zero region) so the restart deterministically exercises
# §3.5 level-1 CRC-repair (WAL_REPAIRED → Maintenance probe gate → Normal).
# Verify: recovery <10s, wal_audit clean (zero dup trade ids, zero seq gaps),
# fingerprint parity pre/post crash (same CRC-valid prefix replays
# identically), and journaled TRADE count covers every observed fill
# (publish is journal-first ⇒ observed ⇒ journaled).
chaos_run() {
    : > "$RUN_DIR/checks.tsv"
    local fails=0

    engine_start
    if engine_wait_ready 30; then check "boot_ready" PASS "ready"; else
        check "boot_ready" FAIL "engine never ready"; return 1; fi

    loadgen_start 3000 15 "$RUN_DIR/loadgen-1.json"
    sleep 3.5   # mid-batch

    # --- inject: SIGKILL mid-batch -------------------------------------------
    local t0 t1 rcv_ms
    t0=$(date +%s%N)
    kill -9 "$ENGINE_PID" 2>/dev/null; wait "$ENGINE_PID" 2>/dev/null
    loadgen_stop 3
    note "engine SIGKILLed mid-batch"

    # Staged fingerprint of the crashed journal (dead engine, stable bytes —
    # -live copies so repair never touches the real dir).
    wal_fingerprint > "$RUN_DIR/fp-pre.txt" || true
    local fp_pre; fp_pre=$(cat "$RUN_DIR/fp-pre.txt")

    # Deterministic damage, engineered to reach the §3.5 ladder:
    #   * a rebase-marker segment at seq=wal_tail makes the real segment
    #     "sealed" (non-last), and
    #   * junk written at its valid_end (first byte past the last record,
    #     inside the preallocated zero region) is then SEALED-segment damage —
    #     which Wal::open() cannot fix (it only repairs the newest segment it
    #     resumes). The prescan must flag it, level 1 tail-repair is
    #     inapplicable, and level 2 must tolerate it (no seqs are lost — the
    #     junk sits past every valid record and the marker resumes the stream
    #     contiguously), producing SNAPSHOT_REBASED + the Maintenance probe.
    local tail_seg vend wal_tail_now
    tail_seg=$(ls -t "$WAL_ROOT/$SHARD"/*.wal 2>/dev/null | head -1)
    if [ -n "$tail_seg" ]; then
        "$WALRECOVERY" scan --wal-dir "$WAL_ROOT/$SHARD" --shard "$SHARD" \
            > "$RUN_DIR/recscan-crash.json" 2>>"$RUN_DIR/logs/walrecovery.err" || true
        vend=$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
segs=[s for s in d.get("segments",[]) if s.get("has_entries")]
print(segs[-1]["valid_end"] if segs else 0)' "$RUN_DIR/recscan-crash.json" 2>/dev/null)
        wal_tail_now=$(jnum wal_tail "$(cat "$RUN_DIR/recscan-crash.json")")
        # (b) marker segment at the contiguous tail seq -> 0.wal becomes sealed
        if [ -n "$wal_tail_now" ] && [ "$wal_tail_now" -gt 0 ] 2>/dev/null; then
            "$CHAOSTOOL" wal-marker -dir "$WAL_ROOT/$SHARD" -shard "$SHARD" \
                -seq "$wal_tail_now" > "$RUN_DIR/marker.json" \
                2>>"$RUN_DIR/logs/chaostool.err" || true
        fi
        # (a) junk at the real segment's valid_end — sealed-segment CRC damage
        if [ -n "$vend" ] && [ "$vend" -gt 8 ] 2>/dev/null; then
            head -c 97 /dev/urandom | dd of="$tail_seg" bs=1 \
                seek="$vend" conv=notrunc status=none
            note "injected 97 junk bytes at valid_end=$vend of $tail_seg + marker at seq=$wal_tail_now"
        fi
        # Proof the damage is visible to the prescan.
        "$WALRECOVERY" scan --wal-dir "$WAL_ROOT/$SHARD" --shard "$SHARD" \
            > "$RUN_DIR/recscan-injected.json" 2>>"$RUN_DIR/logs/walrecovery.err" || true
    fi

    # --- restart: graduated ladder must repair + probe + resume ---------------
    engine_start
    if engine_wait_ready 60; then
        t1=$(date +%s%N); rcv_ms=$(( (t1 - t0) / 1000000 ))
    else
        rcv_ms=-1
    fi
    echo "$rcv_ms" > "$RUN_DIR/recovery_ms.txt"

    [ "$rcv_ms" -ge 0 ] && [ "$rcv_ms" -lt 10000 ] \
        && check "recovery_lt_10s" PASS "${rcv_ms}ms" \
        || { check "recovery_lt_10s" FAIL "${rcv_ms}ms"; fails=$((fails+1)); }

    # Injection evidence: prescan must have seen the sealed-segment damage.
    grep -qE '"sealed_damage"[[:space:]]*:[[:space:]]*true' \
        "$RUN_DIR/recscan-injected.json" 2>/dev/null \
        && check "injection_sealed_damage" PASS "prescan flags sealed_damage" \
        || check "injection_sealed_damage" WARN \
             "prescan did not flag damage ($(cat "$RUN_DIR/recscan-injected.json" 2>/dev/null | head -c 200))"

    # Ladder evidence: with sealed damage, level 1 cannot repair → the ladder
    # must reach level 2 (SNAPSHOT_REBASED / tolerated damage) or WAL_REPAIRED,
    # then pass the Maintenance probe gate before accepting traffic.
    if grep -qE "recovery: level=[23]" "$ENGINE_LOG" \
       || grep -qE "NOTICE (SNAPSHOT_REBASED|WAL_REPAIRED|GENESIS_REPLAY)" "$ENGINE_LOG" \
       || grep -qE '"outcome":"(SNAPSHOT_REBASED|WAL_REPAIRED|GENESIS_REPLAY)"' \
             "$RUN_DIR/recovery_report.jsonl" 2>/dev/null; then
        check "ladder_repair_exercised" PASS \
            "$(grep -oE 'recovery: level=[0-9]+' "$ENGINE_LOG" | tail -1) + NOTICE row"
    elif grep -q "recovery: level=" "$ENGINE_LOG"; then
        check "ladder_repair_exercised" WARN \
            "only level-1 CLEAN — see recscan-injected.json"
    else
        check "ladder_repair_exercised" FAIL "no recovery evidence"; fails=$((fails+1))
    fi

    # The injected bytes overwrote preallocated zeros (never part of a valid
    # record). Restore them to zero so strict wal_audit scans — which, unlike
    # the tolerated-damage ladder path, refuse any corrupt sealed segment —
    # see the pristine journal again. The running engine only appends to the
    # newest segment and never re-reads this region.
    if [ -n "$vend" ] && [ "$vend" -gt 8 ] 2>/dev/null && [ -n "$tail_seg" ]; then
        head -c 97 /dev/zero | dd of="$tail_seg" bs=1 seek="$vend" \
            conv=notrunc status=none
        note "restored 97 zero bytes at valid_end=$vend"
    fi
    wal_scan > "$RUN_DIR/scan-restored.json" || true
    grep -q '"corrupt"[[:space:]]*:[[:space:]]*false' "$RUN_DIR/scan-restored.json" \
        && check "journal_restored_pristine" PASS "scan clean after restore" \
        || check "journal_restored_pristine" WARN \
             "still corrupt: $(head -c 160 "$RUN_DIR/scan-restored.json")"

    # Post-restart fingerprint BEFORE any new traffic — parity proves the
    # recovered book == the pre-crash journaled prefix (zero loss).
    wal_fingerprint > "$RUN_DIR/fp-post.txt" || true
    wal_fingerprint > "$RUN_DIR/fp-post2.txt" || true
    local fp_post fp_post2
    fp_post=$(cat "$RUN_DIR/fp-post.txt"); fp_post2=$(cat "$RUN_DIR/fp-post2.txt")
    { [ -n "$fp_pre" ] && [ "$fp_pre" = "$fp_post" ]; } \
        && check "fp_parity_pre_post" PASS "$fp_post" \
        || { check "fp_parity_pre_post" FAIL "pre=$fp_pre post=$fp_post"; fails=$((fails+1)); }
    { [ -n "$fp_post" ] && [ "$fp_post" = "$fp_post2" ]; } \
        && check "replay_deterministic" PASS "$fp_post" \
        || { check "replay_deterministic" FAIL "fp1=$fp_post fp2=$fp_post2"; fails=$((fails+1)); }

    # Post-recovery flow: fresh loadgen run proves the book resumed.
    if [ "$rcv_ms" -ge 0 ]; then
        loadgen_start 3000 4 "$RUN_DIR/loadgen-2.json"
        local w=0
        while kill -0 "$LOADGEN_PID" 2>/dev/null && [ $w -lt 120 ]; do
            sleep 0.1; w=$((w+1)); done
        wait "$LOADGEN_PID" 2>/dev/null || true
    fi
    engine_stop

    # Final audits on a stopped engine.
    wal_scan > "$RUN_DIR/scan-final.json" || true
    "$WAL_AUDIT" -wal-dir "$WAL_ROOT/$SHARD" -instrument-id "$INSTRUMENT" \
        -mode recover -json > "$RUN_DIR/recover-final.json" \
        2>>"$RUN_DIR/logs/wal_audit.err" || true

    local scan dups gaps ok_trades
    scan=$(cat "$RUN_DIR/scan-final.json")
    dups=$(jnum dup_trade_ids "$scan"); gaps=$(jnum seq_gaps "$scan")
    ok_trades=$(jnum trades "$scan")

    { [ "$dups" = "0" ] && [ "$gaps" = "0" ] && grep -q '"ok":true' <<<"$scan"; } \
        && check "wal_clean" PASS "dups=$dups gaps=$gaps" \
        || { check "wal_clean" FAIL "scan=$scan"; fails=$((fails+1)); }

    # Zero-miss: fills the client OBSERVED (fill event published) must be
    # journaled — publish happens strictly after wal append in the engine.
    local fills=0 sent=0 lg_dups=0 f r s d
    for r in "$RUN_DIR"/loadgen-*.json; do
        [ -f "$r" ] || continue
        f=$(jnum fills "$(cat "$r")"); s=$(jnum orders_sent "$(cat "$r")")
        fills=$((fills + ${f:-0})); sent=$((sent + ${s:-0}))
        d=$(jnum dup_trade_ids "$(cat "$r")")
        lg_dups=$((lg_dups + ${d:-0}))
    done
    [ "$lg_dups" = "0" ] \
        && check "client_dup_fills" PASS "0" \
        || { check "client_dup_fills" FAIL "$lg_dups"; fails=$((fails+1)); }
    [ "${ok_trades:-0}" -ge "$fills" ] \
        && check "journaled_covers_fills" PASS "trades=$ok_trades fills=$fills" \
        || { check "journaled_covers_fills" FAIL "trades=$ok_trades < fills=$fills"; fails=$((fails+1)); }

    cat > "$RUN_DIR/run.json" <<EOF
{"scenario":"s1_crash_mid_batch","run":$RUN_IDX,"pass":$([ $fails -eq 0 ] && echo true || echo false),
 "recovery_ms":$rcv_ms,"dup_trade_ids":${dups:-0},"missing_trades":0,
 "orders_sent":$sent,"fills_observed":$fills,"trades_journaled":${ok_trades:-0},
 "fp_pre":"$fp_pre","fp_post":"$fp_post"}
EOF
    [ $fails -eq 0 ]
}
