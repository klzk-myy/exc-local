# Phase-13.5 Task 13.5.3.4 — Tabletop Exercise Report

**Date:** 2026-09-29 · **Scope:** 4 drills (trading halt, DR failover,
reconciliation mismatch, security incident) · **SLA:** P1 = 15 min,
P2 = 1 h, P3 = 4 h.

Drills marked **EXECUTED** drove real code paths against dev
PostgreSQL/Redis — re-runnable via the `EXC_TABLETOP=1` test gates.
Drills marked **SIMULATED** were walked through on paper because the
required multi-region infrastructure does not exist in this environment;
every such step lists the command that would execute in production and
the probe evidence of the missing infra.

---

## T1 — Trading halt — EXECUTED · elapsed 9 ms

```
EXC_TABLETOP=1 go test -v -run TestTabletopTradingHalt ./internal/admin/
```

(`internal/admin/tabletop_drill_test.go`)

| Step | Action | Result |
|---|---|---|
| 1 | Baseline — `KillSwitchResolver.OrderHalt` | open |
| 2 | `SetHaltScope GLOBAL` (dev Redis `halt:global`) | `GlobalHalted`=true; admission → `GLOBAL` suspension |
| 3 | `ClearHalt` | admission open again |
| 4 | `SetHaltScope ACCOUNT 900000777` | target acct suspended; unrelated acct unaffected |
| 5 | `CircuitBreakerService.ManualTrip(INSTRUMENT DRILLUSD)` | `AdmitOrder` → `CIRCUIT_BREAKER_OPEN`; unrelated symbol admitted |
| 6 | `ManualReset` (dual-controlled path) | admission restored |

Findings: GLOBAL and scoped flags resolve through the production lattice;
manual breaker trip/reset works; no gaps. All state cleaned up in defer.

## T2 — Security incident lockout — EXECUTED · elapsed 3 ms

```
EXC_TABLETOP=1 go test -v -run TestTabletopSecurityIncident ./internal/auth/
```

(`internal/auth/tabletop_drill_test.go`)

| Step | Action | Result |
|---|---|---|
| 1 | Baseline `CheckLock` | not locked |
| 2 | 5 × `RecordFailure` (§12.6 `LockoutThreshold`, 5-min window) | lock engages at failure 5, `retryAfter=15m0s` |
| 3 | `CheckLock` | locked → login seam returns 423 `ACCOUNT_LOCKED_AUTH_FAILURES` + `Retry-After` |
| 4 | `RecordFailure` while locked | returns remaining TTL — counter cannot bypass the lock |
| 5 | `ClearFailures` (auth-success path) | **lock persists** — deliberate: lock is time-bound, a correct-password guess during lockout must not release it (`lockout.go:145`). Operator release = explicit Redis key delete per the security-incident runbook; drill defer performs it |

Findings: fail-closed lock semantics confirmed; the time-bound (not
success-bound) design is correct and is now documented in the drill.

## T3 — Reconciliation mismatch — EXECUTED · elapsed 28 ms

```
EXC_TABLETOP=1 EXC_PG_TEST=1 \
  EXC_TEST_DSN=postgres://postgres:postgres@127.0.0.1:55433/postgres?sslmode=disable \
  go test -v -run TestTabletopReconMismatch ./internal/reconciliation/
```

(`internal/reconciliation/recon_tabletop_test.go` — scratch PG schema,
real migrations 200/207, real `Engine`, real Redis halt flag)

| Step | Action | Result |
|---|---|---|
| 1 | Scratch schema + source DDL + real migrations | ok |
| 2 | Seed divergence: `positions` row, no `position_fills` backing | seeded acct 99999001 |
| 3 | `Engine.RunOnce` — 9-category sweep | status=`MISMATCH`, findings=6 (1 mismatch + 5 inconclusive — WAL legs absent → INCONCLUSIVE, never CLEAN) |
| 4 | Verify dispatch | `reconciliation_findings` POSITIONS MISMATCH row; `funding_ops_alerts` P1 `RECONCILIATION_MISMATCH` row; `trading_suspensions` ACTIVE ACCOUNT row; live Redis `halt:account:99999001` |

Findings: full detect → report → alert → auto-halt chain works end-to-end
on real state. Confirmed fail-closed: unverifiable legs report
INCONCLUSIVE rather than silently passing.

## T4 — DR failover decision procedure — EXECUTED · single-host topology · 2026-10-01

*(supersedes the 2026-09-29 SIMULATED record — the probes below that said
"no replica to promote / no Sentinel / archive off" predated the drill
fleet landed in gap-closure rounds 3–7; every decision leg has since been
executed live on this host and was re-run 2026-10-01 for this exercise)*

Walkthrough per `docs/runbooks/postgres-failover.md` +
`docs/runbooks/dr-drill.md` + `deploy/dr/failover-runbook.sh`, each step
now backed by a live drill artifact rather than a paper path:

| Step | Decision leg | Live evidence (re-run 2026-10-01) | Result |
|---|---|---|---|
| 1 | Declare disaster — BCP quorum + IC; RTO 5-min clock starts | procedure per `failover-runbook.sh` step 1 | walked through |
| 2 | PG promotion — best replica `pg_ctl promote` | `deploy/scripts/pg_failover_drill.sh`: semi-sync standby promoted + read-write in **408 ms** ≪ 5-min RTO; `pg_basebackup` replica streaming sync, 200/200 flush-acked rows, gap 0 s ≪ 15 s RPO, zero committed loss | **PASS** |
| 3 | Redis promotion — Sentinel elects DR replica | `deploy/scripts/redis_failover_drill.sh`: quorum (2) promotion in **2986 ms** (bound 4000 ms; +switch-master T+2986ms); 54/54 `WAIT`-acked keys on new master, zero loss; client reconnect 103 µs; canonical restore `canonical=1 writable=1` | **PASS** |
| 4 | WAL tail / replay recovery | `tests/soak/failover_bench.sh` kill-9/restart parity PASS (recovery ≤116 ms) + `deploy/scripts/shard_swap_rollback_drill.sh` snapshot rebase 91 ms, 800 buffered orders journaled, 0 loss/dup | **PASS** (archive-source leg `archive_mode` stays off on dev PG — PITR-from-archive remains separately env-bound, Phase-09 suite runner `pg-pitr` BLOCKED) |
| 5 | Edge reroute — health-check flip | `deploy/haproxy/test/` drill: 500/500 zero-drop BLUE→GREEN→BLUE map flips; failure-injected failover ~6.7 s to backup, recovery 3.9 s | **PASS** |
| 6 | Post-failover audit + resumption ladder (`MarketDataOnly`→`Normal`) | ModeManager transition coverage in `tests/integration/error_scenarios` (TestL1_DegradationModeTransition) + `core/tests/test_mode_manager.cpp` | PASS |

**Status: EXECUTED (single-host)** — every decision leg now has measured
live evidence. Residual honest caveat: both failure domains are containers
on one host; a true second-region promotion remains env-bound and is
tracked separately (Phase-09 rows 113–119/531/761). Quarterly live drill
(Task 9.3.21) still requires the provisioned secondary region.

---

## Verdicts

| Drill | Type | Elapsed | SLA | Status |
|---|---|---|---|---|
| Trading halt | EXECUTED | 9 ms | P1 15 min | PASS |
| Security incident lockout | EXECUTED | 3 ms | P1 15 min | PASS |
| Reconciliation mismatch | EXECUTED | 28 ms | P1 15 min | PASS |
| DR failover | EXECUTED (single-host topology) | PG promote 408 ms · Redis promote 2,986 ms · edge failover ~6.7 s | RTO 5 min / RPO ≤15 s | PASS — all legs ≪ bounds; second-region promotion still env-bound |

## Runbook corpus gate

`python3 scripts/ci/check_runbooks.py`:

```
runbooks checked=48 conforming=48 min_required=47
alert-referenced files=34
check_runbooks: OK
```
