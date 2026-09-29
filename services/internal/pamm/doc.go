// Package pamm implements the Phase-14 Task 14.3.8 PAMM/MAM engine
// (spec §5.3 invariant 5 / remediation #38 F6, §24 #195).
//
// Model: each pool owns a dedicated pool account (a real accounts row,
// created as a sub-account of the manager — the §5.3 wallet/ledger
// machinery applies verbatim). Investor capital moves wallet-to-wallet
// (investor → pool) through DoubleEntryLedgerService journals with
// entry_type=TRANSFER and GL lines reclassifying the obligation
// 2010_CUSTOMER_LIABILITY_{ccy} → 2170_PAMM_POOL_LIABILITY_{ccy}
// (migration 216 seeds the code; both live in the client-segregated
// 2000–2199 range).
//
// Internal taxonomy (F6): the SEMANTIC record of every pool money
// movement lives in pamm_subledger_entries with txn_type ∈
// {PAMM_INVEST, PAMM_REDEEM, PAMM_FEE_PERF, PAMM_FEE_MGMT}. These types
// are never recorded as DEPOSIT/WITHDRAWAL — the wallet mutation is a
// TRANSFER journal and the sub-ledger row carries the PAMM semantics
// plus the journal link. Pool accounts are structurally barred from
// fiat funding rails by the pamm_pool_funding_guard trigger, so an
// internal investment can never consume an account's daily fiat
// withdrawal allowance (risk_daily_usage / risk:daily_withdrawn is
// mutated only by the Phase-11 withdrawal pipeline — untouched here).
//
// Fill allocation: the engine consumes master fill events (master =
// pool account), weights each ACTIVE allocation by invested capital and
// assigns the fill's quantity pro-rata in fixed-point (largest-remainder
// distribution at the 1e-8 quantum — see allocation.go for the exact
// deterministic rule). Allocations land in pamm_fill_allocations keyed
// UNIQUE (master_trade_id, allocation_id) so replayed fill events dedup.
//
// The real-time copy product (Task 14.3.14, internal/copy) reuses this
// engine's fill source + allocator; it creates child ORDER intents for
// followers instead of sub-ledger allocation rows.
package pamm
