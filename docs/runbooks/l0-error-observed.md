# Runbook: `L0ErrorObserved` — L0 critical/fatal error

**Severity:** P0 (pages `pagerduty-p0`, group_wait 0s, repeat 15m) · **Rule:** `L0ErrorObserved` in `deploy/prometheus/rules/exchange-alerts.yml` (`increase(exchange_errors_total{tier="L0"}[1m]) > 0`); in-process `l0_errors` via `AddStandardRules` → `ops.alerts.monitoring` · **Domain:** spec §2.7.2 L0 — halt-domain faults. Overarching invariant: **Strict Fail-Closed Zero-Loss Pessimism**.

## Symptom

One or more `L0` errors recorded on a service in the last minute. L0 covers: WAL CRC failure, zero-sum ledger imbalance, PTP clock skew >100µs, matching-loop panic/stall >2ms, memory corruption. Expected accompaniments: core halt (`SIGABRT`/`SIGTERM`), dirty-WAL flush, Redis leader lease release (`engine:leader:{shardId}`), possible `WAL_RECOVERY_HALT` / `LEDGER_IMBALANCE_ABORT` / `TIME_SYNC_LOSS_HALT` error codes.

## Diagnosis

1. Identify service/shard from the alert labels (`service`, `shard`) and the `ops.alerts.monitoring` payload (`service`, `code`, `message`, `occurrences`).
2. Check which L0 sub-condition fired:
   - `recovery_reports` table (migration 065): `SELECT outcome, book_seq, wal_tail, last_valid_seq, snapshot_seq, first_divergent_seq, detail FROM recovery_reports ORDER BY id DESC LIMIT 5;` — a `WAL_RECOVERY_HALT` row ⇒ go to [wal-recovery-halt.md](./wal-recovery-halt.md).
   - Engine journal on the shard host: look for `MATCHING_LOOP_STALLED` warnings then `SIGABRT` (Tier-2 watchdog, >2ms stall) or a panic frame — see [engine-halt-failover.md](./engine-halt-failover.md).
   - `clock_offset_nanoseconds` > 100µs ⇒ [time-sync-loss-halt.md](./time-sync-loss-halt.md).
   - `LEDGER_IMBALANCE_ABORT` (HTTP 500, journal debits ≠ credits) ⇒ treat as P0 data-integrity event; do **not** attempt trade-through.
3. Confirm the halt actually happened: `aeron_driver_up`, engine process absent/restarting, `engine:leader:{shardId}` lease TTL expiring in coordination Redis.
4. Check Grafana dashboards: `shard-health` (per-shard state), `system-overview` (mode), `ipc-backbone` (ring depth at time of halt).

## Mitigation

1. **Do not restart into an unverified state.** L0 recovery runs through the graduated ladder only (§3.5/§18.5): Level 1 CRC repair → Level 2 snapshot rebase → Level 3 fail-closed halt. The `recovery-orchestrator` service (`services/cmd/recovery-orchestrator`) drives promotion + the 6-stage pre-open audit; do not bypass it.
2. If a warm standby was promoted, verify epoch fencing: new `engine:leader:{shardId}` value carries `epoch = N+1`; the demoted primary must be dead (self-`SIGTERM` on stale epoch — §18.6.2). If both are alive, kill the stale one manually and record the fencing breach.
3. For `LEDGER_IMBALANCE_ABORT`: trading stays halted. Escalate to Finance Ops + Risk; the 6-stage audit (`§18.6.5`) must re-verify zero-sum before any reopen. Stage 1 has no fallback (§18.6.7).
4. Resume follows the resumption ladder, not a flat restart: `CANCEL_ONLY` 60s grace → 5s reopening call auction (extension ≤60s at >1% OI) → `Normal` (§18.6.6). Mode transitions land in `system:degradation:*` and the `X-Degradation-Mode` header.
5. After service is stable, run the engine's own verification: `wal_audit` fingerprint parity (see [engine-halt-failover.md](./engine-halt-failover.md) §Diagnosis 3) and `exchange verify-audit` for the audit hash chain if the L0 touched ledger/audit paths.

## Escalation

- Page tree (auto): On-call SRE → Core Eng Lead → VP Eng → CTO (5m unacknowledged → secondary leadership; see [incident-escalation.md](./incident-escalation.md)).
- Status page update ≤30min, affected-client email ≤1h (§19.8 comms protocol).
- Post-mortem within 48h: [../ops/postmortem-template.md](../ops/postmortem-template.md). Archive under `docs/incidents/` — pending creation on first incident.
- DORA: if clients/counterparties or service continuity were materially affected, open an ICT incident record per [../ops/dora-incident-reporting.md](../ops/dora-incident-reporting.md) (4h initial / 72h intermediate / 1-month final clocks).
