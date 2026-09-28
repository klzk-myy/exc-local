#!/usr/bin/env bash
# tests/chaos/run.sh — Phase-04.5 Task 4.5.3.1 chaos scenario suite.
# Runs the 6 chaos scenarios, each --runs times (default 3 => 18 runs),
# collecting per-run evidence under results/<scenario>/run-<n>/ and emitting
# results.json + CHAOS-REPORT.md.
#
#   ./run.sh [--runs 3] [--scenarios "s1 s2 ..."] [--root DIR]
#            [--engine BIN --loadgen BIN --wal-audit BIN \
#             --wal-recovery BIN --chaostool BIN]
#
# Environment:
#   EXC_PG_DSN  PostgreSQL DSN for scenario 4's persist-reports verification
#             (default: the repo's migverify scratch instance).
#   PSQL        psql binary (default: /www/server/pgsql/bin/psql or PATH).
#
# Per-run contract (each scenario writes results/<sc>/run-<n>/run.json):
#   pass, recovery_ms (-1 = never ready), dup_trade_ids, missing_trades
# plus free-form detail. Fail-closed assertion: dup/miss must be ZERO and
# recovery_ms < 10000 for the run to count as a pass.

set -u
cd "$(dirname "$0")"
CHAOS_ROOT=$(pwd -P)
REPO_ROOT=$(cd "$CHAOS_ROOT/../.." && pwd -P)

RUNS=3
SCENARIOS="s1_crash_mid_batch s2_timeout_requeue s3_stale_snapshot s4_wal_trimmed_pitr s5_pid_conflict s6_warm_recovery"
RESULTS="$CHAOS_ROOT/results"

ENGINE="$REPO_ROOT/core/build/matching_engine"
LOADGEN="$REPO_ROOT/tests/soak/loadgen"
WAL_AUDIT="$REPO_ROOT/core/build/wal_audit"
WALRECOVERY="$REPO_ROOT/services/wal-recovery"
CHAOSTOOL="$CHAOS_ROOT/bin/chaostool"
RESTORE_PITR="$REPO_ROOT/deploy/postgres/restore_pitr.sh"
PITR_SMOKE="$REPO_ROOT/deploy/postgres/pitr_smoke.sh"

EXC_PG_DSN="${EXC_PG_DSN:-postgres://postgres@127.0.0.1:55433/migverify?sslmode=disable}"
PSQL="${PSQL:-$(command -v psql || echo /www/server/pgsql/bin/psql)}"

SHARD=0
INSTRUMENT=7
SEED=${SEED:-11}

usage() { sed -n '2,20p' "$0"; exit "${1:-2}"; }
while [ $# -gt 0 ]; do
    case "$1" in
        --runs)          RUNS="$2"; shift 2;;
        --scenarios)     SCENARIOS="$2"; shift 2;;
        --root)          RESULTS="$2"; shift 2;;
        --engine)        ENGINE="$2"; shift 2;;
        --loadgen)       LOADGEN="$2"; shift 2;;
        --wal-audit)     WAL_AUDIT="$2"; shift 2;;
        --wal-recovery)  WALRECOVERY="$2"; shift 2;;
        --chaostool)     CHAOSTOOL="$2"; shift 2;;
        --shard)         SHARD="$2"; shift 2;;
        --instrument)    INSTRUMENT="$2"; shift 2;;
        -h|--help)       usage 0;;
        *) echo "unknown arg: $1" >&2; usage 2;;
    esac
done

for b in "$ENGINE" "$LOADGEN" "$WAL_AUDIT" "$WALRECOVERY" "$CHAOSTOOL"; do
    [ -x "$b" ] || { echo "missing binary: $b" >&2; exit 2; }
done
export ENGINE LOADGEN WAL_AUDIT WALRECOVERY CHAOSTOOL RESTORE_PITR PITR_SMOKE
export REPO_ROOT EXC_PG_DSN PSQL SHARD INSTRUMENT SEED

source "$CHAOS_ROOT/lib/engine.sh"

mkdir -p "$RESULTS"
: > "$RESULTS/runs.jsonl"   # fresh suite — never append stale rows
SUITE_T0=$(date +%s)
RUN_SEQ=0   # unique shm/metrics allocator

TOTAL=0; PASSED=0
declare -a ROWS=()          # scenario|run|pass|recovery_ms|dups|missing
declare -A SCEN_PASS=() SCEN_TOTAL=()

for scen in $SCENARIOS; do
    SCEN_FILE="$CHAOS_ROOT/scenarios/${scen}.sh"
    [ -f "$SCEN_FILE" ] || { echo "no scenario file: $SCEN_FILE" >&2; continue; }
    SCEN_TOTAL[$scen]=0; SCEN_PASS[$scen]=0
    for run in $(seq 1 "$RUNS"); do
        RUN_SEQ=$((RUN_SEQ + 1))
        RUN_DIR="$RESULTS/$scen/run-$run"
        rm -rf "$RUN_DIR"
        mkdir -p "$RUN_DIR/logs"
        RUN_IDX=$run
        IPC_BASE="chaos-${scen:0:2}r${run}x${RUN_SEQ}"
        METRICS_PORT=$((19600 + RUN_SEQ))
        WAL_ROOT="$RUN_DIR/wal"; SNAP_ROOT="$RUN_DIR/snap"
        ENGINE_LOG="$RUN_DIR/logs/engine.log"
        LOADGEN_LOG="$RUN_DIR/logs/loadgen.log"
        mkdir -p "$WAL_ROOT/$SHARD" "$SNAP_ROOT"
        ENGINE_PID=0; LOADGEN_PID=0; READY_MARK=0; CHECK_N=0
        export RUN_DIR RUN_IDX IPC_BASE METRICS_PORT WAL_ROOT SNAP_ROOT \
               ENGINE_LOG LOADGEN_LOG

        echo "=== $scen run $run/$RUNS (ipc=$IPC_BASE dir=$RUN_DIR)" >&2
        t0=$(date +%s%N)
        # Run in a subshell so a scenario bug can't kill the orchestrator;
        # EXIT trap inside the subshell reaps strays for THIS run only.
        (
            trap 'loadgen_stop 3 2>/dev/null; engine_stop 2>/dev/null; shm_clean' EXIT
            source "$SCEN_FILE"
            if chaos_run; then exit 0; else exit 1; fi
        ) >> "$RUN_DIR/logs/runner.log" 2>&1
        rc=$?
        t1=$(date +%s%N)
        shm_clean
        # strays keyed to this run's ipc base
        pkill -f "chaos-${scen:0:2}r${run}x${RUN_SEQ}" 2>/dev/null || true

        TOTAL=$((TOTAL + 1)); SCEN_TOTAL[$scen]=$((SCEN_TOTAL[$scen] + 1))
        p="false"; rms=-1; dp=0; ms=0
        if [ -f "$RUN_DIR/run.json" ]; then
            p=$(jq -r '.pass' "$RUN_DIR/run.json" 2>/dev/null || echo false)
            rms=$(jq -r '.recovery_ms' "$RUN_DIR/run.json" 2>/dev/null || echo -1)
            dp=$(jq -r '.dup_trade_ids' "$RUN_DIR/run.json" 2>/dev/null || echo 0)
            ms=$(jq -r '.missing_trades' "$RUN_DIR/run.json" 2>/dev/null || echo 0)
        fi
        [ "$rc" -eq 0 ] && [ "$p" = "true" ] || p="false"
        echo "{\"scenario\":\"$scen\",\"run\":$run,\"pass\":$p,\"recovery_ms\":$rms,\"dup_trade_ids\":$dp,\"missing_trades\":$ms,\"wall_ms\":$(( (t1 - t0) / 1000000 )),\"evidence\":\"$RUN_DIR\"}" \
            >> "$RESULTS/runs.jsonl"
        ROWS+=("$scen|$run|$p|$rms|$dp|$ms")
        if [ "$p" = "true" ]; then
            PASSED=$((PASSED + 1)); SCEN_PASS[$scen]=$((SCEN_PASS[$scen] + 1))
            echo "    PASS (${rms}ms recovery, dups=$dp miss=$ms)" >&2
        else
            echo "    FAIL (rc=$rc) — see $RUN_DIR" >&2
        fi
    done
done

# --- suite-level extras -------------------------------------------------------
# Real end-to-end PITR proof for scenario 4 (informational — exit 77 = SKIP
# when pg binaries or non-root constraints don't hold).
PITR_SMOKE_STATUS="skipped"
if [ -x "$PITR_SMOKE" ]; then
    if timeout 300 "$PITR_SMOKE" > "$RESULTS/pitr_smoke.log" 2>&1; then
        PITR_SMOKE_STATUS="pass"
    elif grep -q "SKIP" "$RESULTS/pitr_smoke.log"; then
        PITR_SMOKE_STATUS="skipped"
    else
        PITR_SMOKE_STATUS="fail"
    fi
fi

# --- aggregate -----------------------------------------------------------------
jq -s '{suite:"phase-04.5-task-4.5.3.1", generated_at:(now|todate),
        runs:., total:length, passed:([.[]|select(.pass)]|length)}' \
    "$RESULTS/runs.jsonl" > "$RESULTS.json" 2>/dev/null || \
    cp "$RESULTS/runs.jsonl" "$RESULTS.json"

REPORT="$CHAOS_ROOT/CHAOS-REPORT.md"
{
    echo "# Chaos Scenario Suite — Phase-04.5 Task 4.5.3.1"
    echo
    echo "- generated: $(date -u '+%Y-%m-%d %H:%M:%SZ')"
    echo "- engine: \`$ENGINE\`  (shard $SHARD, instrument $INSTRUMENT)"
    echo "- runs: $RUNS per scenario × ${#SCEN_PASS[@]} scenarios = $TOTAL runs"
    echo "- evidence root: \`$RESULTS/\` (per-run logs, scans, fingerprints, reports)"
    echo "- suite wall time: $(( $(date +%s) - SUITE_T0 ))s"
    echo "- PITR end-to-end smoke (deploy/postgres/pitr_smoke.sh): $PITR_SMOKE_STATUS"
    echo
    echo "## Verdicts"
    echo
    echo '| scenario | runs | pass | verdict |'
    echo '|----------|------|------|---------|'
    for scen in $SCENARIOS; do
        v="PASS"; [ "${SCEN_PASS[$scen]:-0}" != "${SCEN_TOTAL[$scen]:-0}" ] && v="FAIL"
        echo "| $scen | ${SCEN_TOTAL[$scen]:-0} | ${SCEN_PASS[$scen]:-0} | **$v** |"
    done
    echo
    echo "## Per-run results"
    echo
    echo '| scenario | run | verdict | recovery_ms | dup_trades | missing_trades |'
    echo '|----------|-----|---------|-------------|------------|----------------|'
    for row in "${ROWS[@]}"; do
        IFS='|' read -r s r p rm dp ms <<< "$row"
        v="FAIL"; [ "$p" = "true" ] && v="PASS"
        echo "| $s | $r | $v | $rm | $dp | $ms |"
    done
    echo
    echo "## Scenario semantics"
    echo
    cat <<'EOF'
1. **s1_crash_mid_batch** — SIGKILL mid-batch, then deterministic damage
   engineered to reach the §3.5 ladder: a rebase-marker segment is placed at
   the contiguous tail seq (making the real segment "sealed") and junk bytes
   are written at its valid_end — sealed-segment CRC damage that Wal::open()
   cannot fix (it repairs only the resumed tail). Boot must escalate to
   level 2 (SNAPSHOT_REBASED + Maintenance probe → Normal), recover <10s,
   replay the identical CRC-valid prefix (fingerprint parity), and keep
   dup_trade_ids==0 / seq_gaps==0 / journaled TRADEs ≥ observed fills.
2. **s2_timeout_requeue** — loadgen streams while the engine is SIGKILLed:
   the client must detect the dead producer and terminate in bounded time
   (never hang, never silently keep "sending"); every acknowledged order must
   survive recovery; a bounded probe on the recovered engine re-verifies
   per-order terminal-event accounting.
3. **s3_stale_snapshot** — (A) corrupted snapshot payload → integrity guard →
   level-2 fallback to the prior-generation snapshot + WAL tail replay;
   (B) trimmed WAL stub (tail < snapshot_seq) → forward divergence →
   {snapshot_seq}.wal rebase marker + SNAPSHOT_REBASED + seq domain resumes
   above the cursor; (C) EMPTY WAL dir + snapshot ahead — fail-closed probe
   (requires marker or halt; a clean boot at seq 0 regresses the journal
   seq domain — entries appended below snapshot_seq are covered-skipped by
   the NEXT recovery = silent loss). Book parity is proven by comparing
   snapshot payloads modulo the cursor + replay-stamped
   (timestamp_ns, ingress_seq) fields.
4. **s4_wal_trimmed_pitr** — trimmed-WAL fixture (real segments + marker gap)
   → prescan seq_gap → wal-recovery ladder exit 3 WAL_RECOVERY_HALT → C++
   boot on the fixture also halts → empty archive → restore_pitr.sh stages a
   real PITR restore dir → halt row persisted to recovery_reports in PG.
5. **s5_pid_conflict** — second engine on the same wal-dir must exit
   non-zero via the flock guard (core/src/main.cpp — added by this task; no
   guard existed before: shm_open used O_CREAT without O_EXCL and Wal::open
   took no lock → dual-write was previously possible).
6. **s6_warm_recovery** — -follower standby alongside the leader, leader
   SIGKILLed, promotion = restart on the leader WAL+snap dirs (snapshot+tail
   warm replay); fingerprint parity + <10s + zero dup/miss.
EOF
    echo
    echo "## Failed checks / defects"
    echo
    any_fail=0
    for scen in $SCENARIOS; do
        for run in $(seq 1 "$RUNS"); do
            c="$RESULTS/$scen/run-$run/checks.tsv"
            [ -f "$c" ] || continue
            while IFS=$'\t' read -r name verdict detail; do
                [ "$verdict" = "FAIL" ] || continue
                any_fail=1
                echo "- **\`$scen\` run $run — \`$name\`:** $detail"
                echo "  (evidence: \`$RESULTS/$scen/run-$run/\`)"
            done < "$c"
            if [ ! -f "$RESULTS/$scen/run-$run/run.json" ]; then
                any_fail=1
                echo "- **\`$scen\` run $run — \`harness\`:** scenario did not"
                echo "  emit run.json (hard failure; see logs/runner.log)"
            fi
        done
    done
    [ "$any_fail" = "0" ] && echo "None — all per-run checks passed."
    echo
    if [ "$PASSED" -eq "$TOTAL" ]; then
        echo "## Verdict: **PASS** ($PASSED/$TOTAL runs)"
    else
        echo "## Verdict: **FAIL** ($PASSED/$TOTAL runs passed)"
    fi
} > "$REPORT"

echo "suite done: $PASSED/$TOTAL passed — report: $REPORT" >&2
[ "$PASSED" -eq "$TOTAL" ]
