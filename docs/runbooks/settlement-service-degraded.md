# Runbook: settlement service degraded (`SettlementServiceDown`, `SettlementErrorsElevated`, `SettlementP99Slow`, `SettlementTrafficSilent`)

**Severity:** P1 (down) / P2 (errors, latency) / P3 (silent traffic) · **Service:** `cmd/settlement` on :8083 (`settlement` scrape job) · **Owner:** SRE + Finance Ops

## Symptom

The settlement service is unreachable, is emitting L1/L2 errors, is responding above 500ms p99, or has served zero requests for 30m.

## Diagnosis

### down

`up{job="settlement"}==0` for 1m. Probe `http://settlement:8083/healthz` directly — a wedged metrics listener vs a dead process are different incidents. Settlement batches, GL posting (Task 3.3.6) and nostro flows (Phase-11/24) hang on this service.

### errors

`exchange_errors_total{service="settlement",tier=~"L1|L2"}` sustained. L2 rejections on settlement are state-boundary failures (insufficient funds, idempotency conflicts, T+1 window violations) — a batch of them at once usually means a malformed upstream batch, not random client noise. Check `reconciliation_mismatches_total{kind="settlement"|"nostro"}` alongside — settlement errors plus mismatches = divergent ledger, see [reconciliation-mismatch.md](./reconciliation-mismatch.md).

### latency

p99 >500ms on settlement routes — long GL posting transactions holding ledger locks (`journal_sums` upserts serialize per account+currency under §5.3). Check Postgres lock waits before assuming CPU.

### silent

Zero HTTP traffic for 30m on a live process. Expected for the current scaffold (metrics+health only); once Phase-03 settlement ingest handlers land this becomes a stall signal — tighten `for`/severity then.

## Mitigation

1. Dead service: restart per [../ops/daemon-supervision.md](../ops/daemon-supervision.md); in-flight settlement batches must be re-driven idempotently — the ledger's `journal_entries.idempotency_key` (migration 036) dedupes replays, so re-running a half-posted batch is safe by construction.
2. GL imbalance aborts (`LEDGER_IMBALANCE_ABORT` in logs): do not retry the journal — escalate; a zero-sum violation is a correctness defect, not a transient.
3. Settlement-window pressure: T+1 value-date cutoffs are hard deadlines — a settlement outage inside the window escalates one severity level automatically.

## Escalation

- `SettlementServiceDown` is P1. Errors/latency are P2, escalating to P1 if they persist through one full settlement batch window or coincide with reconciliation mismatches.
