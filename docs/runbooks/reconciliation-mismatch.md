# Runbook: `ReconciliationMismatch` — ledger/position divergence (zero-loss violation)

**Severity:** P1 (page) · **Rule:** `increase(reconciliation_mismatches_total[15m]) > 0` · **Domain:** spec §2.7 invariant — Strict Fail-Closed Zero-Loss Pessimism. A reconciliation mismatch means two sources of truth disagree about money.

## Symptom

`reconciliation_mismatches_total{kind=...}` incremented on `{service}`. The `kind` label identifies which of the Phase-13 reconciliation categories diverged (9 categories including General Ledger Zero-Sum — Category 9, Phase-13 Task 13.3.2).

## Diagnosis

1. Identify the mismatch class from `kind` and pull the concrete rows:
   - Balance/ledger: `ledger_entries` (immutable source of truth, §5.3) vs `balances` derived cache — recompute the account's journal sum: `journal_sums` verification path.
   - GL: `journal_entries`/`ledger_lines` (migrations 036/088) — a debit≠credit line triggers `LEDGER_IMBALANCE_ABORT` (HTTP 500); query `SELECT * FROM journal_entries WHERE ...` for the posting window.
   - Positions vs trades: `positions` (migration 014) vs `trades` (migration 006) — use `exchange verify-audit` and the order audit trail `order_audit` (migration 153) to find the divergent fill.
   - External: `nostro_accounts` (migration 018) / `bank_statements` (migration — bank statement ingestion pending Phase-24 Task 24.3.12) vs GL.
2. Check the audit hash chain for tampering vs error: `exchange verify-audit` (verifies `audit_hash_chain`, migration 009) — a broken chain is a security incident, not a reconciliation bug → P0 + Compliance.
3. Scope it: one account vs systemic. Systemic mismatch on a fresh deploy = code regression → halt-and-review; single-account = data event → isolate.
4. Check for an in-flight cause first: mismatch alarms during an active 2PC reservation window (5s timeout, `shard_margin_reservations`) or settlement batch commit can be transient — confirm the mismatch *persists* beyond the transaction boundary before declaring divergence.

## Mitigation

1. **Fail-closed on persistence:** a mismatch that survives its transaction boundary halts the affected path — Phase-13 wires auto-halt on mismatch (P1 alert + auto-halt per Task 13.3.2 AC); until that ships, the operator equivalent is `ReadOnly` via `system:degradation:mode` (see [degradation-mode-active.md](./degradation-mode-active.md) §Mitigation 3) scoped to freeze writes while reads continue.
2. Do NOT hand-edit `balances` or `ledger_entries` — ledger is immutable; corrections go through compensating journal entries (`audit-append`/GL posting path, dual control).
3. Reconciliation engine detail — pending Phase-13 Task 13.3.2 (9-category engine + hourly schedule). Until deployed, the manual equivalents are the per-kind queries above plus `exchange verify-audit` and `exchange merkle` (daily balance Merkle root vs published root).
4. Preserve evidence before any repair: snapshot the mismatching rows, the `kind` label values, and the WAL/snapshot seq at detection (`recovery_reports`-style record in the incident doc).

## Escalation

- P1 always (annotation: "fail-closed review required"). Escalate to P0 if: mismatch is systemic, `audit_hash_chain` verify fails, client money is implicated, or `LEDGER_IMBALANCE_ABORT` fired.
- Mandatory: Finance Ops + Compliance notification; unresolved mismatches block the daily close and are DORA-reportable if service-impacting ([../ops/dora-incident-reporting.md](../ops/dora-incident-reporting.md)).
