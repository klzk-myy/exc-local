# Runbook: BCP stand-down decision — when recovery is impossible or the wrong call

**Phase-09 Task 9.3.27** · **Companion document:** [../policies/business-continuity-plan.md](../policies/business-continuity-plan.md) (the BCP itself — decision framework, quorum, financial-impact table, notification tree) · **Authority:** spec §19.11.1 · **Trigger:** §18.6.4 disaster declaration or any incident where technical recovery is uncertain.

## Symptom / entry conditions

Invoke this runbook when ANY of:

- §18.6.4 disaster declaration is on the table (primary DC severance >30s, structural disaster, power failure).
- A P0 has exceeded its 1h mitigation SLA with recovery still uncertain.
- The recovery path itself is failing: promotion loops, `WAL_RECOVERY_HALT` on multiple shards, PG promotion with >15s RPO breach, irreconcilable `LEDGER_IMBALANCE_ABORT`.
- Continuing to operate would compound client harm (bad prices, unhedged exposure, irreconcilable ledger) — i.e., *recovery is the wrong call*.

## Diagnosis — the four criteria (§19.11.1.1)

Quorum of 2 minimum — one technical (Core Eng Lead or on-call SRE lead), one non-technical (COO/Head of Ops or delegate). Named decision-maker: see BCP §1 roster. Score each criterion GREEN/AMBER/RED with evidence:

| Criterion | Evidence source | Stand-down signal (RED) |
|---|---|---|
| Client-money integrity | `ledger_entries`/`journal_sums` zero-sum check, `reconciliation_mismatches_total`, `recovery_reports` outcomes | imbalance unresolved, or loss window unbounded |
| Oracle/price integrity | feed freshness (<5s gate), `PRICE_ORACLE_UNAVAILABLE` state, mark-vs-last divergence | feeds corrupt/absent AND venues would mark wrong prices |
| Ability to reconcile | `exchange verify-audit` (audit_hash_chain), `wal_audit` fingerprints, bank-statement/CLS stages 5–6 (§18.6.5) | cannot prove what happened — books unauditable |
| Client harm vs delay | open positions exposure, withdrawal queue, pending settlements at cut-off | harm grows faster by running than by halting |

## Mitigation — decision and its mechanics

1. **Quorum convenes** (target ≤30min from invocation; async vote is valid if logged). Record criteria scores + verdict in the incident channel and `admin_audit_log` (migration 010).
2. **If stand-down:** announce venue mode `Maintenance` via `system:degradation:mode` (see [degradation-mode-active.md](./degradation-mode-active.md) §Mitigation 3 — blocks new orders; resting orders cancel per §2.4 deploy semantics). Declare to clients via the notification tree (BCP §5) — status page, direct notice, regulator path per [../ops/dora-incident-reporting.md](../ops/dora-incident-reporting.md) (4h initial clock is already running if the incident is DORA-major).
3. **If go-forward:** pick a mode in [bcp-goforward.md](./bcp-goforward.md) and execute its entry conditions — the decision record must name the chosen mode + expected duration + review checkpoint.
4. **Either way:** freeze non-essential deploys and changes (error-budget freeze semantics apply regardless of budget state, [../ops/slo-policy.md](../ops/slo-policy.md)); preserve evidence — `recovery_reports`, WAL segments, `recovery_digests`, ledger snapshots, alert timelines.
5. **Quorum unreachable edge case:** either member can declare a *temporary* stand-down (fail-closed always defaults to halt, never to trade-on) — permanent go-forward requires full quorum. Log the sole-member decision prominently; ratify within 24h or revert to stand-down.

## Exit conditions

- Stand-down lifted when the four criteria are GREEN and the resumption ladder (§18.6.6) completes through `Normal` — not before.
- If stand-down converts to go-forward mode, its runbook's exit conditions govern instead.
- Mandatory post-incident review within **10 business days** updates the BCP and this runbook before incident close ([../ops/post-incident-review-template.md](../ops/post-incident-review-template.md)).

## Escalation

- Decision-maker + quorum per BCP §1; regulator clocks per DORA run in parallel — delays are logged, not rationalised (§19.11.1.4).
- Stand-down is always P0 governance; a go-forward-mode entry is P0 until its own exit conditions are met.
