# Runbook: PostgreSQL primary loss / replica promotion

**Severity:** P1 (venue enters `MarketDataOnly` — all trading blocked while market data continues; §2.4) · **Targets:** RPO ≤15s / RTO ≤5min (spec §18.3) · **Mechanisms:** semi-sync replica + Patroni/Pacemaker promotion (`pg_ctl promote`), `deploy/postgres/` scripts, `book_snapshots`/ledger tables are the recoverable state.

## Symptom

Primary OLTP unreachable: `postgres`/`pgbouncer` connection failures (ports 5432/6432), `MarketDataOnly` mode flips, services log pool errors. Go services degrade to market-data reads; order writes reject `DEGRADED_MODE`.

## Diagnosis

1. Scope: process crash vs host loss vs network partition. `pg_isready`, patroni cluster state, and replica `pg_stat_replication`/`pg_last_wal_receive_lsn()` vs `pg_last_wal_replay_lsn()` — the gap is the RPO exposure (target ≤15s).
2. Confirm mode already engaged: `GET system:degradation:mode` = `MarketDataOnly` with reason. If trading writes were still being attempted during primary loss, audit `ledger_entries`/`orders` for the window — write errors at L2 are safe; silent partial commits are not.
3. Identify best replica: highest replayed LSN among replicas; semi-sync guarantees at least one within 15s.
4. Check pgbouncer + service pools: pools hold stale primary connections — they need the new primary endpoint post-promotion.

## Mitigation

1. Promote: `pg_ctl promote` on the chosen replica (Patroni-managed in the §18.6.4 model; manual promote is the documented fallback). Record promotion timestamp — this is the RTO clock stop.
2. Repoint: update pgbouncer/service DSN to the new primary (config via `EXC_POSTGRES_DSN` env override, `services/config.example.yaml` `postgres.dsn`); restart or bounce pools.
3. Verify write path before leaving `MarketDataOnly`: a test `INSERT`/`SELECT` on `ledger_entries` via an admin session; `balances` consistency spot-check via `journal_sums`.
4. Data-gap assessment: the ≤15s replication window may contain committed-then-lost transactions — reconcile via WAL/incremental diff against `orders`/`trades` seqs; unresolved diffs route through the settlement-failure/PB-restitution chains (Phase-24 Tasks 24.3.6/24.3.14 — pending) and are logged for Finance Ops.
5. Rebuild the old primary as a replica (re-sync from new primary; do NOT let it re-enter as primary — single-writer invariant).
6. Exit `MarketDataOnly`: write `system:degradation:mode` → `Normal` via `Client.SetDegradationMode` only after §18.6.5 audit stages re-verify (the ModeManager hysteresis also applies).

## Mitigation — full region loss (RTO ≤5min path)

If the whole primary site is gone, this becomes a §18.6.4 failover: secondary-region replica promotes, `exchange:replay-from-archive` covers WAL segments not yet replicated, and edge reroutes via Anycast health checks (15–30s). The quarterly drill procedure is [dr-drill.md](./dr-drill.md) — run it live, record actual vs target RPO/RTO as incident evidence.

## Escalation

- P1 → DBA/platform on-call; escalate P0 if promotion cannot complete inside 5min or the replica's replayed LSN is >15s behind at promotion (RPO breach — client-facing disclosure likely).
- Post-mortem + RPO/RTO actuals filed to `docs/incidents/`; RPO-breach incidents are DORA-reportable ([../ops/dora-incident-reporting.md](../ops/dora-incident-reporting.md)).
