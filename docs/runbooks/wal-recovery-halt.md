# Runbook: `WAL_RECOVERY_HALT` — graduated recovery ladder exhausted (P1)

**Severity:** P1 (this is the runbook the Phase-04 fail-closed halt links to — Task 4.3.9 "P1 alert with linked runbook") · **Trigger:** `recovery_reports` row with `outcome = 'WAL_RECOVERY_HALT'` (migration 065) and/or `ops.alerts.recovery` payload from `wal-recovery --nats` · **Domain:** spec §3.5/§18.1/§18.5 graduated WAL recovery ladder; error code `WAL_RECOVERY_HALT` (HTTP 500).

## Symptom

Engine boot recovery could not reconstruct a deterministic book. The ladder already ran: Level 1 (CRC repair of the trailing partial 4KB block) failed, Level 2 (snapshot rebase + replay-forward) failed, Level 3 = fail-closed halt. The shard does not open; venue behavior per §2.4 is `MarketDataOnly` for that shard's instruments.

## Diagnosis

1. Pull the halt record — it is the single source of truth for the loss window:

   ```sql
   SELECT id, created_at, outcome, book_seq, wal_tail, last_valid_seq,
          snapshot_seq, first_divergent_seq, detail
   FROM recovery_reports
   WHERE outcome = 'WAL_RECOVERY_HALT'
   ORDER BY id DESC LIMIT 1;
   ```

   Column semantics (migration 065): `book_seq` recomputed cursor (−1 unknown), `wal_tail` last valid seq + 1, `last_valid_seq` last CRC-valid entry, `snapshot_seq` WAL cursor covered by the snapshot, `first_divergent_seq` first unverifiable seq, `outcome` ∈ `CLEAN | WAL_REPAIRED | SNAPSHOT_REBASED | WAL_RECOVERY_HALT | RECONCILE_*`.

2. Compute the candidate loss window: orders/fills in WAL seqs `(last_valid_seq … first_divergent_seq)` are unverifiable; seqs ≥ `first_divergent_seq` are the divergence region. Map to trades: `trades`/`orders` rows at those seqs (the WAL seq is the same ordering the engine applies).
3. Check the ladder evidence: engine boot log shows which level failed and why (CRC repair bounds exhausted at the trailing block, or snapshot SHA-256/sequence divergence at Level 2). `recovery_digests` (migration 092) carries the per-1,000-trade running digests — a digest mismatch identifies the first divergent region cheaply.
4. Verify PostgreSQL vs WAL ordering per §18.5.2: snapshot seq must match or predate the PG committed trade seq. If `snapshot_seq` > PG committed seq, the snapshot itself is ahead — a *different* defect (snapshot from the future) → escalate before replay.
5. Inspect the raw tail: `wal-recovery` CLI (`services/cmd/wal-recovery`) — the offline file-level ladder; run it read-side on a staged copy of `wal/{shard}/`, never on the live journal (`failover_bench.sh` convention: RecoveryManager's torn-tail repair must never touch the live journal). `wal_audit` (`core/build/wal_audit`) fingerprints the staged book.

## Mitigation

The decision is a **dual-control operator decision** (Task 4.3.9), not a single-operator call:

1. **Option A — accept the loss window:** open against the verified prefix (`last_valid_seq`). Requires documented approval (two operators, one technical + Risk sign-off) recorded in `admin_audit_log` + the incident record. Client-facing: fills in the dropped window must be reconciled per account before clients see balances — coordinate with Finance Ops.
2. **Option B — `exchange:replay-from-archive`:** replay S3-archived segments to extend the verified prefix: `exchange replay-from-archive --symbol=EUR/USD --from=<ts-or-seq> --to=<ts-or-seq>` (`services/cmd/exchange`, or standalone `services/cmd/replay`). Contract per spec §18.2: `--from`/`--to` accept ISO 8601 or WAL seqs; partial replay stops at the first unarchived gap and reports the gap range; production replays require `EXTERNAL_AUDITOR`/`Super Admin` + dual control; logged to `admin_audit_log`/`replay_audit_log` (replay_audit_log table — pending migration).
3. **Never:** hand-edit `.wal` segments, trim past `first_divergent_seq`, or boot with `--skip-verify`-style flags. The zero-loss invariant means the *declared* loss is auditable; silent repair is the failure mode this runbook exists to prevent.
4. After the chosen path: the shard re-runs boot → ladder → 6-stage pre-open integrity audit (§18.6.5) → resumption ladder (`CANCEL_ONLY` 60s → 5s call auction → `Normal`). A failed audit stage aborts with `LEDGER_IMBALANCE_ABORT`/halt — stages 5–6 (nostro/CLS) may defer past 120s per §18.6.7 but stage 1 (zero-sum) never does.
5. File the `recovery_reports` outcome as `RECONCILE_*` once the decision path completes (the table's outcome domain anticipates reconciliation results).

## Escalation

- P1 paging already fired with the halt. If the loss-window decision cannot be made within 1h (P0 territory — trading halted >2min on the affected shard), escalate per [incident-escalation.md](./incident-escalation.md) and open the BCP decision framework ([bcp-standdown.md](./bcp-standdown.md)) only if the shard cannot return.
- Compliance + Finance Ops must co-sign Option A; regulator notification follows the DORA timeline if client positions were affected ([../ops/dora-incident-reporting.md](../ops/dora-incident-reporting.md)).
- Post-mortem within 48h — root cause of the *original* WAL damage (NVMe, mid-fsync crash, trim bug) is a separate finding from the recovery decision.
