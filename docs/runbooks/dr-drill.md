# Runbook: Quarterly DR Drill Program

**Phase-09 Task 9.3.21** · **Authority:** spec §18.3 (RPO/RTO targets), §18.4 (region topology), §18.6 (recovery workflow), §19.5 (DORA resilience-testing evidence), §24 #211 · **Cadence:** quarterly live drill + annual full-scale combined DR+BCP+comms exercise (the BCP's test of record, [../policies/business-continuity-plan.md](../policies/business-continuity-plan.md) §6).

## 1. Canonical targets (pass/fail)

| Component | RPO (max data loss) | RTO (max downtime) | Verification mechanism |
|---|---|---|---|
| Order book (in-memory + WAL) | 0 (dual-write WAL) | 10s warm recovery | engine restart → `wal tail` replay; fingerprint parity via `wal_audit` |
| WAL streams | 0 (dual-write + S3 archive) | 30s | `exchange archive-status --shard=N` + `replay-from-archive` |
| PostgreSQL | 15s | 5min | `pg_ctl promote` on semi-sync replica; LSN-gap measurement |
| Redis | 5s | 30s | Sentinel failover; client reconnect timer (target <3s per Task 9.3.20) |
| Market data | 10s | 2min | WS `system.status` + feed resumption; `last_seq` resume replay |
| ClickHouse | 60s | 30min | `deploy/clickhouse/backup.sh restore` + `verify` counts on scratch cluster |
| User data | 5min | 10min | same PG promote path |
| Local shard failover (§18.6.3) | 0 | 3s | `recovery-orchestrator` epoch promotion |

## 2. Quarterly schedule

One drill per quarter, rotating the catalog so every mechanism is exercised at least annually; the failover drill (D1) runs every quarter without exception. Schedule against the 24/5 calendar: drills run **Saturday 00:00–08:00 UTC** (after NY close Friday 22:00 UTC, before Sydney open Sunday 21:00 UTC) or on staging when production-touching.

| Quarter | Mandatory | Rotation focus |
|---|---|---|
| Q1 | D1 region failover | D3 WAL replay-from-archive deep test |
| Q2 | D1 region failover | D4 ClickHouse restore |
| Q3 | D1 region failover | D2 PG failover + D6 chaos suite |
| Q4 | **Annual full-scale**: D1 + BCP go-forward simulation + incident comms | D5 Redis Sentinel + secret-decrypt-in-secondary |

## 3. Drill catalog

Automated orchestration: `scripts/ops/dr_drill_runner.sh` runs the runnable
subset, records RPO/RTO actual vs target per component into `results.jsonl`,
and writes the §4 report to `docs/incidents/drills/YYYY-Qn-drill.md`.
Infra-gated legs report `SKIP` (never silently counted); `--strict` promotes
SKIP → FAIL on hosts claiming full coverage. First committed run:
`../incidents/drills/2026-Q4-drill.md`.

### D1 — Full primary → secondary region failover (mandatory quarterly)

DR architecture and executable failover scripts: [`../ops/dr.md`](../ops/dr.md) (Task 9.3.4 — `deploy/dr/failover-runbook.sh`; second-region deployment is pending-infra per that document).

1. **Pre-drill:** declare maintenance window (`POST /api/v1/admin/maintenance-windows`); snapshot `engine:leader:*` epochs, `system:degradation:*`, `book_snapshots` seqs, PG LSNs; confirm secrets decrypt in the secondary region (DR-critical secret copies — Task 9.3.29 item 5; a missing DR copy **blocks the drill's pass verdict**).
2. **Sever:** simulate primary loss (network severance >30s is the §18.6.4 trigger — firewall/deny rules on the DR path or cluster shutdown on staging).
3. **Observe auto-path:** ingress clamps (503/`INSTRUMENT_HALTED`/`35=j`), `recovery-orchestrator` logs the declaration, BCP quorum notified ([bcp-standdown.md](./bcp-standdown.md) decision framework — the drill exercises the *decision* too).
4. **Promote:** `pg_ctl promote` secondary PG replica (record elapsed + replayed LSN gap = RPO actual); Redis Sentinel promotes secondary master (record <3s); ClickHouse secondary replicas serve reads.
5. **WAL catch-up:** `exchange replay-from-archive --symbol=<pair> --from=<last-replicated-ts>` on the secondary engine for segments not yet replicated (S3 CRR bucket) — record gap range reported.
6. **Resumption ladder:** 6-stage pre-open audit (§18.6.5) → `CANCEL_ONLY` 60s → FIX `TradingSessionStatus` broadcast → WS `resume` → 5s call auction → `Normal`.
7. **Edge reroute:** verify Anycast/health-check reroute of `api.`/`ws.`/FIX endpoints within 15–30s (edge configs `deploy/edge/` + `deploy/cloudflare/README.md`; measure on the staging path and mark any production-timing caveat in the report).
8. **Residency gate:** EU/UK PII partitions must fail over only to adequate jurisdictions (SCC/adequacy check in the failover path — Task 9.3.29 item 3); record the residency verdict.
9. **Failback:** after drill, restore primary per §18.6.4 reverse path; diff `recovery_digests` (migration 092) for both directions.

### D2 — PostgreSQL failover drill

Run [postgres-failover.md](./postgres-failover.md) on staging: kill primary, promote replica, repoint DSN, measure promote elapsed vs 5min RTO and LSN gap vs 15s RPO. Verify `MarketDataOnly` entry/exit + write-path test on `ledger_entries`.

### D3 — WAL replay-from-archive drill

`exchange archive-wal` freshness → `exchange replay-from-archive --symbol=EUR/USD --from=<drill-start>` into a scratch WAL dir; verify replayed entry count vs local segments; `wal-recovery` offline ladder on a staged corrupt tail (synthesize CRC damage — `tests/chaos/scenarios/s1_crash_mid_batch.sh` is the existing harness). Record: entries replayed, gap range, fingerprint parity.

### D4 — ClickHouse restore drill

Follow [`deploy/clickhouse/RUNBOOK.md`](../../deploy/clickhouse/RUNBOOK.md) §Restore drill verbatim on a scratch cluster: `backup.sh counts` before, `restore daily-YYYYMMDD` (or latest `incr-*`), `counts` after, `verify` exit 0. Record elapsed vs 30min RTO and the 5-year-aggregate query check (Task 9.3.29 item 3).

### D5 — Redis Sentinel failover drill

Drill harness + verdict targets documented in [`../ops/redis-sentinel-failover.md`](../ops/redis-sentinel-failover.md) (Task 9.3.20 — `deploy/crons/redis-failover-drill.sh --json`, verdict bounds: detect ≤3s, write RTO ≤30s, RPO proxy ≤5s); incident-side expectations in [redis-sentinel-failover.md](./redis-sentinel-failover.md). Record: promotion elapsed, client reconnect time, key-space continuity (`engine:leader`, `system:degradation:*`, `circuit_breaker:*`).

### D6 — Recovery/chaos scenario suite

`tests/chaos/run.sh --runs 3` — six scenarios (s1 crash-mid-batch, s2 timeout-requeue, s3 stale-snapshot, s4 wal-trimmed-PITR, s5 pid-conflict, s6 warm-recovery); each writes `results/<sc>/run-<n>/run.json` with `recovery_ms`, `dup_trade_ids`, `missing_trades`. Pass = all green with zero dups/missing; scenario-4 SKIP (exit 77) requires `EXC_PG_DSN`.

### D7 — Secrets decrypt-in-secondary (with D1)

Verify every DR-critical `secrets_inventory` row decrypts in the secondary region before promotion is declared — missing copy fails the drill regardless of other results (Task 9.3.29 item 5).

## 4. Post-drill report (required artifact)

File to `docs/incidents/drills/YYYY-Qn-drill.md` (create dir on first drill) — DORA testing evidence (Task 9.3.15 item 3):

```markdown
# DR Drill Report — YYYY-Qn — <drill ids>

| component | RPO target | RPO actual | RTO target | RTO actual | verdict |
|---|---|---|---|---|---|
| order book/WAL | 0 |  | 10s |  |  |
| PostgreSQL | 15s |  | 5min |  |  |
| Redis | 5s |  | 30s |  |  |
| market data | 10s |  | 2min |  |  |
| ClickHouse | 60s |  | 30min |  |  |

## Timeline
- <hh:mm:ss> event → observation (promote start/finish, lease epochs, audit stages)

## Incidents during drill
- unexpected behavior, manual interventions, monitoring gaps

## Evidence
- run.json paths, recovery_reports ids, wal_audit fingerprints, backup verify output, residency verdict, secret-decrypt results

## Remediation items
- owner / due date per gap
```

## 5. Evidence checklist (per drill)

- [ ] RPO/RTO actual vs target recorded for every component exercised
- [ ] `recovery_reports` rows produced during drill reviewed (expected `CLEAN`/`WAL_REPAIRED`, never unexplained `WAL_RECOVERY_HALT`)
- [ ] `wal_audit` fingerprint parity pre/post for each shard touched
- [ ] Resumption ladder completed: `CANCEL_ONLY` → auction → `Normal`, `X-Degradation-Mode` tracked
- [ ] Residency-gate verdict recorded (adequate jurisdiction per partition class)
- [ ] DR-critical secrets decrypt verified in secondary (fail = drill fail)
- [ ] Post-drill report filed + DORA evidence register updated
- [ ] Remediation items ticketed with owners/dates

## Escalation

A drill that reveals an RPO/RTO breach is a P1 finding (not a drill failure to shrug off): open the post-drill report as an incident, and if the breach is in the *primary* data path, treat as a live defect and re-test within 30 days.
