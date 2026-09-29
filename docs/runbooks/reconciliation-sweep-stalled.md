# Runbook: reconciliation sweeps stalled / telemetry gap (`ReconciliationSweepMissing`, `ReconciliationTelemetryAbsent`)

**Severity:** P2 (sweep missing) / P3 (telemetry gap) · **Metric:** `reconciliation_runs_total{kind}` (registered by `observability.NewMetrics`; `ObserveReconciliation` feeder pending wiring to the sweep loops) · **Owner:** SRE + Ops

## Symptom

No reconciliation sweep has run for `kind` in 2h, or the `reconciliation_runs_total` series renders no samples at all.

## Diagnosis

1. `kinds` are the registered sweep names (wallet|ledger|nostro|… per the NewMetrics help text). A single flatlined kind → that sweep's scheduler/job crashed. All kinds flat → the sweep scheduler itself or its host died.
2. Check the owning process: settlement service (`cmd/settlement`) for ledger/nostro sweeps; admin cron surfaces for wallet sweeps — `deploy/crons/` lists scheduled ops jobs.
3. The *detection* risk is asymmetric: a stalled sweep means divergence accumulates silently — treat "no data" as "unknown", never "clean".

## Mitigation

1. Restart the stalled sweep; then run one manual sweep and confirm `reconciliation_mismatches_total` stays flat — a stalled sweep followed by a mismatch burst means the backlog hid real divergence → [reconciliation-mismatch.md](./reconciliation-mismatch.md).
2. If the sweep runs but reports errors rather than results, check its data source (PG for wallet/ledger, nostro statements for nostro) before re-running — sweeping against a dead source double-counts noise.

## telemetry-gap

`ReconciliationTelemetryAbsent` = `reconciliation_runs_total` emits no samples — `ObserveReconciliation` has no caller yet (Phase-03/Phase-24 sweep loops must call it). Until then reconciliation health is only visible in service logs and the mismatch runbook's manual queries. Wire the counter when the sweep lands; do not silence this rule — it *is* the signal that the domain is blind.

## Escalation

- Sweep missing >2h → P2 ticket. Escalate to P1 if a manual sweep on restart reveals mismatches (zero-loss review required) or the outage spans a settlement window (T+1 cut).
