# Chaos Scenario Suite — Phase-04.5 Task 4.5.3.1

- generated: 2026-09-28 14:46:58Z
- engine: `/www/wwwroot/exc.local/core/build/matching_engine`  (shard 0, instrument 7)
- runs: 3 per scenario × 6 scenarios = 18 runs
- evidence root: `/www/wwwroot/exc.local/tests/chaos/results/` (per-run logs, scans, fingerprints, reports)
- suite wall time: 127s
- PITR end-to-end smoke (deploy/postgres/pitr_smoke.sh): pass

## Verdicts

| scenario | runs | pass | verdict |
|----------|------|------|---------|
| s1_crash_mid_batch | 3 | 3 | **PASS** |
| s2_timeout_requeue | 3 | 3 | **PASS** |
| s3_stale_snapshot | 3 | 3 | **PASS** |
| s4_wal_trimmed_pitr | 3 | 3 | **PASS** |
| s5_pid_conflict | 3 | 3 | **PASS** |
| s6_warm_recovery | 3 | 3 | **PASS** |

## Per-run results

| scenario | run | verdict | recovery_ms | dup_trades | missing_trades |
|----------|-----|---------|-------------|------------|----------------|
| s1_crash_mid_batch | 1 | PASS | 824 | 0 | 0 |
| s1_crash_mid_batch | 2 | PASS | 748 | 0 | 0 |
| s1_crash_mid_batch | 3 | PASS | 756 | 0 | 0 |
| s2_timeout_requeue | 1 | PASS | 675 | 0 | 0 |
| s2_timeout_requeue | 2 | PASS | 728 | 0 | 0 |
| s2_timeout_requeue | 3 | PASS | 645 | 0 | 0 |
| s3_stale_snapshot | 1 | PASS | 109 | 0 | 0 |
| s3_stale_snapshot | 2 | PASS | 109 | 0 | 0 |
| s3_stale_snapshot | 3 | PASS | 110 | 0 | 0 |
| s4_wal_trimmed_pitr | 1 | PASS | 59 | 0 | 0 |
| s4_wal_trimmed_pitr | 2 | PASS | 18 | 0 | 0 |
| s4_wal_trimmed_pitr | 3 | PASS | 15 | 0 | 0 |
| s5_pid_conflict | 1 | PASS | 109 | 0 | 0 |
| s5_pid_conflict | 2 | PASS | 110 | 0 | 0 |
| s5_pid_conflict | 3 | PASS | 109 | 0 | 0 |
| s6_warm_recovery | 1 | PASS | 690 | 0 | 0 |
| s6_warm_recovery | 2 | PASS | 741 | 0 | 0 |
| s6_warm_recovery | 3 | PASS | 699 | 0 | 0 |

## Scenario semantics

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

## Failed checks / defects

None — all per-run checks passed.

## Verdict: **PASS** (18/18 runs)
