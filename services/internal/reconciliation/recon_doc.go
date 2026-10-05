// Package reconciliation implements the Phase-13 Task 13.3.2 hourly
// reconciliation engine: nine financial-correctness categories, a jittered
// scheduler, a durable report surface (migration 207), and a mismatch
// dispatcher that raises a P1 ops alert and emits a scoped auto-halt.
//
// Fail-closed contract (spec §2.7 — Strict Fail-Closed Zero-Loss
// Pessimism): a category leg whose inputs cannot be verified produces an
// INCONCLUSIVE finding, never a fabricated pass. Only verified divergences
// are MISMATCH findings; only MISMATCH findings emit halts.
//
// Recorded rulings (per the repo change protocol — design choices where
// the task text meets the codebase as-built):
//
//	R1  Balances leg — "PG balances vs WAL-derived balances" is served by
//	    recovery.RecWalletReconcileDiff: the engine WAL carries no wallet
//	    state (only margin reserve/release intents), so the append-only
//	    ledger_entries journal is the authoritative derived leg —
//	    strictly stronger than a WAL projection for money. journal_sums
//	    drift (ledger == live but the verification cache disagrees) is
//	    INCONCLUSIVE, not a halt: the money is provably right.
//
//	R2  Positions/Orders core leg — no AdminGetPositions/AdminGetOrders
//	    query seam exists against the C++ core, so both categories
//	    reconcile PG against a WAL replay (the WAL is the core's own
//	    journal — authoritative). ORDERS diffs the replayed resting book;
//	    POSITIONS (Task 10.5.3.26) diffs per-(account, instrument) nets
//	    derived from journaled ORDER_NEW owners + TRADE fills, alongside
//	    the position_fills system-of-record leg — the prior standing
//	    "no core seam" marker fires only when the WAL seam is unwired.
//	    Partial journal coverage (lost ranges, corrupt segments, trimmed
//	    stream base, unresolved trade→order refs) degrades the affected
//	    leg to INCONCLUSIVE — a partial journal never produces a
//	    financial verdict.
//
//	R3  Bank-statement leg — no live bank feed exists. FUNDING runs the
//	    internal legs that ARE checkable (rail_payments disposition,
//	    suspense-quarantine consistency) plus a StatementSource seam;
//	    the seam is unwired until Phase-24 lands statement ingestion, so
//	    the statement leg is a standing INCONCLUSIVE marker.
//
//	R4  Auto-halt mechanism — the engine is a machine actor and cannot
//	    wait on the §8.2 dual-control path, so it does not call
//	    admin.KillSwitchService.Set (which requires a resolvable admin
//	    user id). Instead it writes the SAME artifacts the service
//	    commits: a trading_suspensions row (initiated_by = 0, the
//	    documented system sentinel — the column has no FK and operators
//	    can clear it through the normal kill-switch reset path, keeping
//	    dual-control on resume) plus the halt:* Redis enforcement flag
//	    via admin.KillSwitchFlagWriter (internal/redis.Client).
//	    Per-finding scopes are surgical (ACCOUNT / INSTRUMENT / RAIL);
//	    findings with no resolvable scope, or a run producing more than
//	    maxScopedHalts distinct scopes, escalate to GLOBAL.
//
//	R5  Alerting — a MISMATCH run raises a P1 OpsAlert
//	    (settlement.OpsAlert, subject ops.alerts.reconciliation) AND a
//	    durable funding_ops_alerts row (same pattern as the account
//	    emergency-freeze alerter) so the page survives a pager outage.
//	    INCONCLUSIVE-only runs alert at P2. The reconciliation_findings
//	    table is the durable report trail (migration 207).
//
//	R6  Settlement overdue semantics — a PENDING instruction whose
//	    settlement_date has passed is INCONCLUSIVE, not MISMATCH: bank
//	    cut-offs legitimately lag the date edge, so an overdue row is an
//	    ops signal, not a verified divergence.
package reconciliation
