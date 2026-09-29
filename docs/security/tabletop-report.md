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

## T4 — DR failover decision procedure — SIMULATED · infra absent

Walkthrough per `docs/runbooks/postgres-failover.md` +
`docs/runbooks/dr-drill.md` + `deploy/dr/failover-runbook.sh`
(syntax-checked `bash -n` — OK). The runbook is marked *pending-infra:
requires a live secondary region* in the script header itself.

Probe evidence (dev environment, 2026-09-29):

| Check | Command | Result |
|---|---|---|
| PG role | `SELECT pg_is_in_recovery()` | `false` — primary |
| Standbys | `SELECT count(*) FROM pg_stat_replication` | `0` — **no replica exists to promote** |
| WAL archive | `archive_mode` / `archive_command` | `off` / `(disabled)` — `exchange:replay-from-archive` source absent |
| Redis Sentinel | `/dev/tcp/10.1.0.11:26379`, `127.0.0.1:26379` | unreachable / refused — `deploy/crons/redis-failover-drill.sh` cannot run |
| Secondary region | — | none provisioned |

Decision-procedure walkthrough (paper):
1. Declare disaster — BCP quorum + incident commander; RTO 5-min clock starts (`failover-runbook.sh` step 1).
2. PG promotion — `pg_ctlcluster 16 main promote` on the best replica; **blocked**: `pg_stat_replication`=0 means no promotion candidate and unbounded RPO exposure today.
3. Redis promotion — `REPLICAOF NO ONE` on the DR replica; **blocked**: no Sentinel/replica topology.
4. WAL tail — `restore_pitr.sh` + `services/cmd/replay --from-archive`; **blocked**: `archive_mode=off`.
5. Edge reroute — provider health-check flip (documented only).
6. Post-failover audit + resumption ladder (`MarketDataOnly` → `Normal`) — documented.

**Status: PARTIAL** — procedure validated on paper; live execution is
impossible without the Phase-09 secondary region. Related executable
evidence that does exist: `tests/soak/failover_bench.sh` engine
kill-9/restart parity — last artifact
`tests/soak/artifacts/20260928T022857Z-failover/failover-report.json`
`{"verdict":"PASS","trials":2,"recovery_ms_max":116}` (same-host WAL
replay, not cross-region DR).

**Remediation required before the quarterly live drill (Task 9.3.21):**
provision PG semi-sync replica + `archive_mode=on`, Redis Sentinel
quorum, and run `redis-failover-drill.sh` + `failover-runbook.sh` live.

---

## Verdicts

| Drill | Type | Elapsed | SLA | Status |
|---|---|---|---|---|
| Trading halt | EXECUTED | 9 ms | P1 15 min | PASS |
| Security incident lockout | EXECUTED | 3 ms | P1 15 min | PASS |
| Reconciliation mismatch | EXECUTED | 28 ms | P1 15 min | PASS |
| DR failover | SIMULATED (infra absent) | n/a | RTO 5 min / RPO ≤15 s | PARTIAL — live drill deferred to Task 9.3.21 |

## Runbook corpus gate

`python3 scripts/ci/check_runbooks.py`:

```
runbooks checked=48 conforming=48 min_required=47
alert-referenced files=34
check_runbooks: OK
```
