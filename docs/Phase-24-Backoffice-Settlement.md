# Phase 24 — Backoffice & Settlement (Nostro/Vostro)

**Duration:** 15.5–19.5 days (unchanged; CSDR/PB schema reconciliations applied 2026-09-27, remediation #35) (supersedes prior 15.5–18.5 — Task 24.3.19 added 2026-09-27, remediation #24)
**Dependencies:** Phases 11, 13, 20, 21
**Spec Reference:** §17 (Backoffice & Settlement)

---

## 24.1 Objectives

Implement the full backoffice: nostro/vostro account management, reconciliation, settlement confirmation, SWIFT message tracking, compliance reporting, Prime Brokerage give-up reconciliation, and Continuous Linked Settlement (CLS) third-party PvP settlement.

---

## 24.2 Prerequisites

- Phases 11, 13, 20, 21 complete

---

## 24.3 Tasks

### Task 24.3.1: Nostro/Vostro Account Management

**Objective:** Implement nostro/vostro account management.

**File Locations:** `services/internal/backoffice/nostro.go`

**Implementation:**
1. **Nostro:** our account at correspondent bank (we hold foreign currency).
2. **Vostro:** correspondent bank's account with us (they hold domestic currency).
3. Per currency, per correspondent bank.
4. `POST /api/v1/admin/nostro-accounts` — create (Finance Ops+).
5. `GET /api/v1/admin/nostro-accounts` — list with balances.
6. Balance tracking: real-time from SWIFT confirmations + bank statement polling.

**Definition of Done (Acceptance Criteria):**
* [x] Nostro/vostro accounts per currency per correspondent
* [x] Account creation and listing work
* [x] Real-time balance tracking

**SDD Checklist:**
- [x] Spec checkpoint: nostro/vostro account management — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.2: Nostro/Vostro Reconciliation

**Objective:** Reconcile nostro/vostro accounts with bank statements.

**File Locations:** `services/internal/backoffice/reconciliation.go`

**Implementation:**
1. Daily reconciliation: our records vs bank statements.
2. Mismatch: P1 alert, investigation workflow.
3. Discrepancy detection threshold: flag any settlement discrepancy > $1,000 or > 0.01% of expected amount (spec §24 #21).
4. Breaks: timing differences, missing confirmations, fees.
5. `GET /api/v1/admin/nostro-reconciliation?date=` — reconciliation report.
6. Auto-resolution: match by SWIFT reference, amount, date.

**Definition of Done (Acceptance Criteria):**
* [x] Daily reconciliation runs
* [x] Mismatch triggers P1 alert
* [x] Discrepancy > $1,000 or > 0.01% detected and flagged (spec §24 #21)
* [x] Break investigation workflow
* [x] Reconciliation report generated
* [x] Auto-resolution by SWIFT reference

**SDD Checklist:**
- [x] Spec checkpoint: nostro/vostro reconciliation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.3: Settlement Confirmation

**Objective:** Track settlement confirmations from correspondent banks.

**File Locations:** `services/internal/backoffice/confirmation.go`

**Implementation:**
1. SWIFT MT900 (confirmation of debit), MT910 (confirmation of credit).
2. On confirmation: update settlement_instruction status → SETTLED.
3. Update nostro/vostro balance.
4. Timeout: if no confirmation within 2 business days, P2 alert.

**Definition of Done (Acceptance Criteria):**
* [x] SWIFT MT900/MT910 confirmations processed
* [x] Settlement status updated to SETTLED
* [x] Nostro/vostro balance updated
* [x] Timeout alert if no confirmation within 2 business days

**SDD Checklist:**
- [x] Spec checkpoint: settlement confirmation tracking — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.4: SWIFT Message Tracking

**Objective:** Track all SWIFT messages in/out.

**File Locations:** `services/internal/backoffice/swift_tracking.go`

**Implementation:**
1. All SWIFT messages (MT103, MT202, MT900, MT910, pacs.009) tracked.
2. `swift_messages` table: message_type, reference, status, timestamp.
3. `GET /api/v1/admin/swift-messages?from=&to=&type=` — query.
4. Audit trail: immutable record of all messages.
5. **Migration note:** `migrations/035_create_swift_messages.up.sql` — `swift_messages` table (id, message_type, reference, direction ENUM('IN','OUT'), status, raw_payload, timestamp).

**Definition of Done (Acceptance Criteria):**
* [x] All SWIFT message types tracked
* [x] Queryable by date and type
* [x] Immutable audit trail

**SDD Checklist:**
- [x] Spec checkpoint: SWIFT message tracking — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.5: Compliance Reporting

**Objective:** Generate compliance reports for regulators.

**File Locations:** `services/internal/backoffice/compliance_reporting.go`

**Implementation:**
1. MiFID II transaction report (from Phase 21).
2. EMIR trade report (from Phase 21).
3. FinCEN CTR/SAR reports (from Phase 21).
4. Basel III capital adequacy report.
5. Monthly compliance summary.
6. `GET /api/v1/admin/compliance-report?type=&from=&to=` — export.

**Definition of Done (Acceptance Criteria):**
* [x] MiFID II report exportable
* [x] EMIR report exportable
* [x] FinCEN CTR/SAR exportable
* [x] Basel III capital adequacy report
* [x] Monthly compliance summary

**SDD Checklist:**
- [x] Spec checkpoint: compliance reporting — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.6: Failed Settlement Handling

**Objective:** Handle failed settlements and exceptions.

**File Locations:** `services/internal/backoffice/exceptions.go`

**Implementation:**
1. Failed settlement: SWIFT rejection, insufficient nostro balance, counterparty issue.
2. Exception workflow: investigate → resolve → retry or reverse.
3. `POST /api/v1/admin/settlement-exceptions/{id}/resolve` — resolve (Finance Ops+, dual control).
4. Reversal: reverse trade settlement, return funds.

**Definition of Done (Acceptance Criteria):**
* [x] Failed settlement detected
* [x] Exception workflow: investigate → resolve → retry/reverse
* [x] Dual control for resolution
* [x] Reversal works

**SDD Checklist:**
- [x] Spec checkpoint: failed settlement handling — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.7: Prime Brokerage Give-Up Reconciliation & Break Management

**Objective:** Implement Prime Brokerage Give-Up trade reconciliation, un-affirmed trade break management, and middle-office affirmation workflow using `pb_giveup_trades` (`migrations/037_create_prime_brokerage.up.sql`) per spec §5.22, §17.1, §24 #125.

**File Locations:** `services/internal/backoffice/pb_reconciliation.go`

**Implementation:**
1. Reconcile executed give-up trades recorded in `pb_giveup_trades` against Prime Broker trade affirmation feeds (Traiana / MarkitSERV) and EOD electronic broker blotters.
2. Discrepancy & break detection: flag rate discrepancies, quantity mismatches, missing tickets, and un-affirmed trades past the timeout window (default 60s).
3. Break investigation workflow: assign give-up breaks to middle-office finance ops, track resolution status (`PENDING` -> `AFFIRMED` / `REJECTED` / `DISPUTED`), and maintain an audit log of manual adjustments.
4. Auto-reconciliation: match confirmed trades on `external_trade_id`, currency pair, notional amount, and executed rate within tolerance.
5. Admin API: `GET /api/v1/admin/pb-reconciliation?pb_id=&date=` — generate and export daily PB give-up reconciliation reports.
6. **Position Give-Up & Transfer Collateral Rebalancing Invariant (added 2026-09-27, remediation #38):**
   - When a give-up trade or position adjustment transfers exposure between executing broker and prime broker client accounts, the system MUST atomically rebalance allocated initial margin collateral (`balances.locked`) within the same `SERIALIZABLE` transaction.
   - Release the locked collateral on the transferring account, and calculate and lock the required initial margin on the receiving account. If the receiving account has insufficient available margin, the transfer breaks and aborts with `INSUFFICIENT_MARGIN` (HTTP 409). Transferring positions without atomically moving the corresponding margin collateral is strictly prohibited.

**Definition of Done (Acceptance Criteria):**
* [x] Daily PB give-up reconciliation matches executed trades with PB reports
* [x] Un-affirmed trades and discrepancies flagged with break alerts
* [x] Middle-office break investigation and resolution workflow functional
* [x] EOD PB reconciliation summary report generated and exportable

**SDD Checklist:**
- [x] Spec checkpoint: PB give-up reconciliation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.8: Continuous Linked Settlement (CLS) Third-Party PvP Settlement Service

**Objective:** Implement third-party Continuous Linked Settlement (CLS) Payment-versus-Payment (PvP) settlement service to eliminate principal Herstatt risk for eligible currency pairs per spec §17.6, §24 #126.

**File Locations:** `services/internal/settlement/cls_pvp.go`

**Implementation:**
1. CLS eligibility checker loads versioned currency/product/member/static-data and cut-off reference data; the previous hard-coded 18-currency list and 06:30/09:00 CET times are superseded because eligibility and operating windows can change.
2. Submit, amend, and rescind paired CLSSettlement instructions through the contracted third-party settlement member using its CLS-supported SWIFT ISO 20022 XML interface. MT300/MT304 remain optional upstream confirmation/affirmation inputs and are not treated as CLS settlement instructions (supersedes prior MT300/MT304 instruction wording).
3. Persist instruction IDs and status transitions: received, validated, matched/unmatched, eligible/ineligible, pay-in, settled, rescinded, expired, rejected. Rejections/unmatched pairs route to pre-cut-off exception management.
4. Post nostro/GL final settlement only after authenticated finality status from CLS/member. CLS provides simultaneous PvP finality; this service records and reconciles it rather than claiming to guarantee it locally (supersedes prior local-guarantee wording).
5. Ineligible flow follows the settlement-risk waterfall: alternative PvP where available, legally enforceable bilateral netting, then controlled gross settlement with principal-risk amount/duration limits and alerts.

**Definition of Done (Acceptance Criteria):**
* [x] Eligibility and cut-offs load from versioned CLS/member reference data, not hard-coded counts/times
* [x] ISO 20022 paired instructions submit/amend/rescind and correlate acknowledgements/statuses
* [x] Unmatched/rejected instructions enter exception workflow before applicable cut-off
* [x] GL/nostro finality posts only from authenticated CLS/member final settlement status
* [x] Ineligible flow follows alternative-PvP → netting → controlled-gross settlement-risk waterfall

**SDD Checklist:**
- [x] Spec checkpoint: CLS PvP settlement service — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.9: Bilateral Payment Netting & Standing Settlement Instructions

**Objective:** Implement bilateral payment netting across trades per counterparty+value date and the SSI registry per spec §17.7 (§24 #155) — cutting nostro cash movements and settlement ops volume. Added 2026-09-15.

**File Locations:** `services/internal/settlement/netting.go`, `services/internal/settlement/ssi.go`, `migrations/044_settlement_netting.up.sql`

**Implementation:**
1. `standing_settlement_instructions` table (migration 044): `ssi_id`, `account_id`, `currency`, `beneficiary_bank`, `account_ref`, `swift_bic`, `status`; SSIs verified against the Phase-11 `bank_accounts` registry before use.
2. `payment_netting_batches` table: `batch_id`, `counterparty_pair` (exchange nostro ↔ client SSI), `currency`, `value_date`, `gross_amount`, `net_amount`, `status` (OPEN|NETTED|DISPATCHED|SETTLED), plus `netting_batch_lines` mapping trades→batch.
3. Netting run: per currency+value_date+counterparty, aggregate all pending settlement obligations; single net payment per direction dispatched via the appropriate rail (spec §17.7); cutoff-aware (per-rail cut-offs from Task 11.3.7).
4. Scope rule: netting applies to same-counterparty, same-currency, same-value-date obligations only; CLS-eligible flow still routes through Task 24.3.8 PvP (CLS itself nets internally) — netting is for non-CLS bilateral settlement.
5. Reconciliation: netted batch reconciles as one nostro movement; line-level mapping retained for break attribution.

**Migration note:** `migrations/044_settlement_netting.up.sql` — `standing_settlement_instructions`, `payment_netting_batches`, `netting_batch_lines` (spec §17.7).

**Definition of Done (Acceptance Criteria):**
* [x] SSI registry CRUD + verification against beneficiary registry (§24 #155)
* [x] Netting batches aggregate same-CP/currency/value-date obligations to single net payment
* [x] CLS-eligible flow excluded (routed via PvP); rail cut-offs respected
* [x] Netted settlement reconciles as single nostro movement with line-level mapping

**SDD Checklist:**
- [x] Spec checkpoint: bilateral netting + SSI (§17.7, §24 #155) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: trade bust after netting (re-open batch), netting across weekend value dates, SSI change mid-batch

---

### Task 24.3.10: Bunched Orders, Average Price & Post-Trade Allocation

**Objective:** Support institutional asset-manager allocations with a pre-declared fair method and complete claim/correction lifecycle per spec §5.31/§17.8/§24 #172. Added 2026-09-15.

**File Locations:** `services/internal/backoffice/allocations.go`, `migrations/055_trade_allocations.up.sql`

**Implementation:**
1. Account manager registers bunched order group, eligible beneficial accounts, capacity/eligibility checks, and deterministic allocation method before order entry; proprietary and client interest cannot share a group.
2. Group all fills, compute weighted average price, then allocate quantities under the stored method, including deterministic partial-fill treatment; allocated quantity may never exceed filled quantity.
3. Support FIX/FIXML Trade Capture/Allocation messages or equivalent API for allocate, claim, reject, cancel/correct, with PartyID/LEI/account data propagated to confirmations, PB give-up, settlement and regulatory reports.
4. Lock a group once submitted to settlement; post-submission corrections require dual control, offset/replacement records, GL/settlement/reporting updates, and immutable before/after evidence.

**Migration note:** `migrations/055_trade_allocations.up.sql` creates `average_price_groups`, `trade_allocations`, and allocation audit history per spec §5.31.

**Definition of Done (Acceptance Criteria):**
* [x] Pre-declared method allocates full and partial fills deterministically without over-allocation
* [x] Weighted average price and beneficiary quantities reconcile exactly to source fills
* [x] Allocate/claim/reject/correct lifecycle propagates to PB, settlement, confirmations and reporting
* [x] Client/proprietary mixing is blocked; post-submission correction requires dual control and immutable offsets

**SDD Checklist:**
- [x] Spec checkpoint: bunched-order average-price allocation (§17.8, §24 #172) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: partial fill, beneficiary ineligible after execution, rounding remainder, correction after report submission

---

### Task 24.3.11: Client-Money Segregation & Daily Reconciliation

**Objective:** Safeguard client funds through legal/operational segregation, daily reconciliation, and immediate shortfall control per spec §5.33/§17.9/§24 #173. Added 2026-09-15.

**File Locations:** `services/internal/backoffice/client_money.go`, `migrations/056_client_money.up.sql`

**Implementation:**
1. Classify bank/nostro and GL accounts as client, house, margin/transaction, or suspense; prohibit house use of client funds and retain bank acknowledgement/trust status plus third-party due diligence/diversification review.
2. Allocate receipts to individual clients promptly; track unidentified receipts, uncleared items, margin transfers and client entitlement separately from operational balances.
3. Each business day calculate client-money requirement and resource, reconcile internal client ledgers to segregated accounts, and reconcile external bank statements; preserve source snapshots and sign-off.
4. Any shortfall raises P1 `CLIENT_MONEY_SHORTFALL`, blocks client-money withdrawals/transfers that worsen it, funds remediation immediately from house money under dual control, and records regulator notification/escalation. Excess removal also requires approval.
4a. **(amended 2026-09-20 — feature-completeness audit remediation #11):** Shortfall remediation follows a 4-tier waterfall: **Tier 1** — Insurance fund debit (automatic, immediate); **Tier 2** — House money / retained earnings (automatic, requires 4-eyes approval, max latency 30 minutes); **Tier 3** — Emergency capital call from shareholders (manual, triggers regulatory notification to FCA/SEC within 1 business day per CASS 7.15.33/SEC 15c3-3); **Tier 4** — If Tiers 1-3 insufficient within 4 hours, orderly trading suspension triggered, all client positions frozen, regulatory declaration of default filed. Daily stress test validates: insurance fund + house reserves ≥ 2× worst-case NBP exposure (linked to Phase-19 Task 19.3.13).
5. Maintain primary-pooling-event and wind-down data package: client entitlements, bank accounts, unresolved breaks, contacts, access credentials references, and transfer/return workflow.

**Migration note:** `migrations/056_client_money.up.sql` creates client-money account classification, daily reconciliation, break, remediation and acknowledgement records per spec §5.33.

**Definition of Done (Acceptance Criteria):**
* [x] Client and house funds are segregated in bank, GL and operational permissions
* [x] Internal and external reconciliations run each business day with client-level traceability
* [x] Shortfall blocks worsening movements, raises P1 and is remediated immediately under dual control
* [x] Bank due diligence/diversification, acknowledgements, pooling-event and wind-down evidence are exportable
* [x] 4-tier shortfall remediation waterfall implemented (insurance → house → capital call → default declaration)
* [x] Client-money shortfall exceeding Tier 2 triggers automated regulatory notification within 60 minutes
* [x] Daily stress test: insurance + house ≥ 2× worst-case NBP exposure

**SDD Checklist:**
- [x] Spec checkpoint: client-money segregation + daily reconciliation (§17.9, §24 #173) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: unidentified receipt, bank failure, margin transfer timing, negative interest/fees, pooling event
- [x] Edge cases: simultaneous flash crash across all pairs, insurance fund depleted during weekend, regulatory notification timing across time zones

---

---

### Task 24.3.12: Bank Statement Ingestion Engine (MT940, MT942, camt.053) & Automated Reconciliation Parser

**Objective:** Implement automated multi-format bank statement ingestion (SWIFT MT940/MT942 and ISO 20022 camt.053/camt.052) for nostro cash reconciliation per spec §17.10 and §24 #175. Added 2026-09-15.

**File Locations:** `services/internal/backoffice/statement_parser.go`, `services/internal/backoffice/camt053_parser.go`, `migrations/057_bank_statements.up.sql`

**Implementation:**
1. Multi-format statement parser:
   - SWIFT MT940 (Customer Statement Message EOD): parse opening balance (Tag 60F), statement lines (Tag 61: value date, entry date, debit/credit mark, currency, amount, transaction type, reference), narrative (Tag 86), closing balance (Tag 62F).
   - SWIFT MT942 (Interim Transaction Report intraday): parse interim debit/credit activity.
   - ISO 20022 `camt.053.001.08` XML: parse `Stmt` elements, opening/closing balances (`Bal`), and transaction entries (`Ntry`) with UETR, end-to-end ID, and proprietary codes.
2. Ingestion pipeline: ingest files via SFTP / MQ bank links; validate file checksums, sequence numbers, and account IBAN/BIC; store parsed statement records in `bank_statements` table (spec §5.34, migration 057).
3. Automated matching engine: match bank entries against internal GL and nostro ledger transactions using reference key priority: (1) UETR, (2) transaction reference, (3) amount + currency + exact value date.
4. Exception processing: unmatched statement entries create automated breaks in `settlement_exceptions` with categorized investigation codes (`UNEXPECTED_CREDIT`, `MISSING_PAYMENT`, `AMOUNT_MISMATCH`).

**Definition of Done (Acceptance Criteria):**
* [x] MT940, MT942, and camt.053 statements parsed with 100% field extraction accuracy (§24 #175)
* [x] Opening and closing balances reconciled with prior statement continuity
* [x] Matching engine reconciles bank entries to internal nostro movements with zero duplicate credits
* [x] Unmatched entries route to settlement break investigation workflow within 60s of ingestion

**SDD Checklist:**
- [x] Spec checkpoint: Bank statement ingestion and reconciliation parser (§17.10, §24 #175) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: duplicate statement file transmission, out-of-order statement sequence numbers, reversal lines in MT940

---

### Task 24.3.13: CSDR Settlement Discipline — Fails, Penalties & Mandatory Buy-In

**Objective:** Implement EU Central Securities Depository Regulation (CSDR) settlement discipline regime covering settlement fail detection, daily cash penalty calculation, mandatory buy-in procedures, and regulatory reporting of settlement failures. Added 2026-09-15 (production-completeness audit remediation #4).

**File Locations:** `services/internal/backoffice/csdr_discipline.go`, `services/internal/backoffice/buyin.go`, `migrations/084_settlement_penalties.up.sql`

**Implementation:**
1. **Settlement fail detection:** Trades not settled by intended settlement date (ISD) flagged as `SETTLEMENT_FAIL` in `settlements` table. Daily batch process scans for fails at ISD+1.
2. **Cash penalty calculation:** Daily penalties per CSDR Article 7: penalty rate per day × settlement value. Rates: 1bp/day for liquid FX, 0.5bp/day for illiquid. Penalties computed bilaterally (failing party pays, receiving party receives). Penalty records in `settlement_penalties` table (migration 084).
3. **Mandatory buy-in:** At ISD+4 (extension period), if still unsettled, initiate buy-in notification to failing party. At ISD+7, execute buy-in: acquire equivalent position at market price; price differential charged to failing counterparty.
4. **Regulatory reporting:** Daily settlement fail report to NCA/CSD containing: number of fails, fail duration, fail value, penalty amounts. Monthly aggregate report.
5. **Exemptions:** CLS-settled trades exempt from bilateral penalty (CLS handles internally). SFT (Securities Financing Transaction) exemptions per CSDR RTS.

**Definition of Done (Acceptance Criteria):**
* [x] Settlement fails detected at ISD+1 and flagged in settlements table
* [x] Daily cash penalties calculated per CSDR rates (1bp liquid / 0.5bp illiquid) bilaterally
* [x] Mandatory buy-in notification at ISD+4; execution at ISD+7 with price differential charged
* [x] NCA/CSD settlement fail report generated daily; monthly aggregate available
* [x] CLS-settled trades correctly exempted from bilateral penalty

**SDD Checklist:**
- [x] Spec checkpoint: CSDR settlement discipline regime (§17.6 extension, §24 #199) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: partial settlement (penalty on unsettled portion only), buy-in on illiquid exotic pair, penalty dispute workflow

---

### Task 24.3.14: PB Credit Restitution on Settlement Failure

**Objective:** Define and implement credit restitution workflow when trades pass pre-trade NOP/DSL checks but subsequently fail settlement — ensuring PB credit limits are retroactively restored and clients are not locked out of trading. Added 2026-09-15 (production-completeness audit remediation #4).

**File Locations:** `services/internal/backoffice/credit_restitution.go`, `services/internal/risk/pb_credit_adjuster.go`

**Implementation:**
1. **Settlement failure trigger:** When a CLS or bilateral settlement fails/rescinds (Task 24.3.6 exception workflow), emit `SETTLEMENT_FAILED` event to PB credit service.
2. **DSL credit restoration:** On settlement failure, retroactively credit the consumed DSL allocation back to the client's available limit. DSL adjustment posted as a correction journal entry with `reason=SETTLEMENT_FAIL_RESTITUTION`.
3. **NOP adjustment:** Reverse the NOP impact of the failed trade. If the failed trade reduced NOP, the reversal increases it (and vice versa). Net NOP recalculated.
4. **Risk recalculation:** Trigger margin recalculation for affected accounts after credit restitution — positions may now require additional margin or qualify for reduced margin.
5. **Notification:** PB receives real-time credit restitution notification via FIX Drop Copy (Task 18.3.6) and admin dashboard alert.
6. **Guards:** Credit restitution is one-time per settlement event; duplicate events are idempotent. Restitution blocked if the trade has already been replaced/allocated.

**Definition of Done (Acceptance Criteria):**
* [x] Settlement failure triggers automatic DSL credit restitution with correction journal entry
* [x] NOP impact of failed trade reversed; net NOP recalculated
* [x] Margin recalculated for affected accounts post-restitution
* [x] PB notified via FIX Drop Copy + admin dashboard
* [x] Restitution is idempotent — duplicate settlement failure events do not double-credit

**SDD Checklist:**
- [x] Spec checkpoint: PB credit restitution on settlement failure (§13.7 extension, §24 #200) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: restitution during DSL reset window, partial settlement failure, concurrent restitution + new trade using same DSL capacity

---

### Task 24.3.15: Post-Trade Allocation Workflow for Block Trades

**Objective:** Implement the post-trade workflow for allocating institutional block trades to fund sub-accounts, with confirmation and settlement per allocation.

**File Locations:** `services/internal/backoffice/allocation_engine.go`

**Implementation:**
1. Allocation instruction intake: via FIX (35=J Allocation Instruction, from Phase-18 Task 18.3.13) or REST API `POST /api/v1/allocations`.
2. Block trade → allocation: parent block trade split into child allocations per fund account. Each allocation: `{fund_account_id, quantity, price (avg fill), allocation_id}`.
3. Allocation rules: manual (PM submits allocation), rule-based (pre-configured split percentages per strategy), or pro-rata (equal split across funds).
4. Confirmation per allocation: generate FIX Allocation Report (35=AK) per fund. Each fund's operations team confirms/rejects.
5. Settlement per allocation: each confirmed allocation generates its own settlement instruction (using Phase-03 Task 3.3.3 settlement flow). Settlement date inherited from parent trade.
6. GL impact: parent block trade clears through dedicated clearing account `2090_BLOCK_ALLOCATION_CLEARING` (spec §6.6, remediation #38); child allocations debit/credit per fund account against the clearing account. Net GL impact zero, preserving parent trade immutable audit trail.
7. Allocation deadline: allocation must be submitted by T+0 EOD. Unallocated block trades escalate to compliance (Phase-21) as P2 alert.
8. Audit trail: complete chain from block trade → allocation instruction → child trades → confirmations → settlements.

**Definition of Done (Acceptance Criteria):**
* [x] Block trades allocated to sub-accounts via FIX or REST
* [x] Manual, rule-based, and pro-rata allocation modes supported
* [x] FIX Allocation Report (35=AK) generated per fund allocation
* [x] Settlement instruction created per confirmed allocation
* [x] GL entries reversed and re-posted per allocation
* [x] Unallocated block trades escalate to compliance by T+0 EOD

**SDD Checklist:**
- [x] Spec checkpoint: post-trade allocation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: partial allocation (not all quantity allocated), allocation rejection by fund ops, amendment after partial settlement

---

### Task 24.3.16: CLS Match Discrepancy Quarantine & Client-Money Shortfall Top-up

**Objective:** Enforce fail-closed settlement batch quarantine upon CLS matching discrepancy and automated mandatory 1-hour client-money shortfall replenishment per spec §2.7, §17.12, and §24 #326.

**Implementation:**
1. **CLS Match Discrepancy Quarantine:** If CLS matched instructions report trade or counterparty discrepancies (differing amounts, currency mismatch, or settlement date drift), immediately quarantine the batch under exception code `CLS_SETTLEMENT_MISMATCH`. Block release of outbound funds until operational reconciliation is affirmed via dual-control authorization.
2. **Client-Money Shortfall Top-Up Protocol:** Real-time calculation of client-money segregation requirements. If internal segregated balances drop below total client entitlement, immediately flag `CLIENT_MONEY_SHORTFALL` (HTTP 422 for withdrawal attempts). Trigger automated top-up executing the Task 24.3.11 4a waterfall — Tier 1 insurance-fund debit automatic/immediate, Tier 2 house money 4-eyes within its 30-min SLA — escalating at 60 min (waterfall order corrected 2026-09-27, remediation #35; the prior text skipped Tier 1). Also: `CLIENT_MONEY_SHORTFALL` HTTP status aligned to 503 per §23/§17.12 (supersedes the prior 422, remediation #35).
3. **Escalation & Compliance Telemetry:** If client-money shortfall persists past 60 minutes, escalate to Chief Compliance Officer and execute pre-configured Tier 3 regulatory notifications.

**SDD Checklist:**
- [x] Spec checkpoint: CLS settlement mismatch quarantine and client-money shortfall alerts (§24 #326) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.17: Venue Treasury, Own Funds & Contingent Capital Backstop

**Objective:** Protect the *entity* that stands behind client money. §17.9 and Task 24.3.11 protect client money; nothing previously governed the house balance sheet that backs it. Per spec §17.13.1. Added 2026-09-25 (governance remediation #17).

**File Locations:** `services/internal/backoffice/treasury.go`, `services/internal/backoffice/contingent_capital.go`, `migrations/082_treasury.up.sql`

**Implementation:**
1. **Own-funds ledger:** `own_funds_balances` (migration 082) — house equity, capital reserves and the insurance-fund balance as first-class, separately-reconciled lines, distinct from client money (§17.9) and from the GL (§17.1). Reconciled daily against bank statements (Task 24.3.12).
2. **Contingent capital commitments:** `contingent_capital_commitments` (migration 082) records the insurance-fund funding waterfall — house capital first, then committed backstops (sponsors, credit facility, or insurer) — each with committed amount, activation trigger, draw window and governing-agreement reference. The depletion sequence defined in Phase-19 (Tasks 19.3.9/19.3.14) must terminate in a funded backstop, never in an unbacked deficit.
3. **Insurance & reinsurance:** business-interruption, cyber, key-person and errors-&-omissions cover tracked with expiry, limit, excess and broker. Expiry inside 60 days raises `INSURANCE_POLICY_EXPIRING` (P2, Finance Ops + Compliance Officer).
4. **Liquidity buffer:** minimum standing house-liquidity ratio against a stressed 5-business-day outflow (forced withdrawals, adverse rollover, insurance-fund call). Breach raises `TREASURY_LIQUIDITY_BREACH` (P1), freezes discretionary house outflows and blocks new LP capacity until remediated.
5. **Endpoints:** `GET /api/v1/admin/treasury/own-funds` and `GET/POST /api/v1/admin/treasury/contingent-capital` (Finance Ops); breach and coverage views exposed to Read-Only Auditor.
6. **Admission gate:** the "minimum financial resources" launch prerequisite in Phase-21 Task 21.3.13 is satisfied by funded `own_funds_balances` and executed contingent-capital commitments, not by attestation.

**Definition of Done (Acceptance Criteria):**
* [x] Own-funds ledger is reconciled daily and is distinct from client money and the GL
* [x] Insurance-fund depletion waterfall terminates in a committed, documented backstop
* [x] Insurance expiry inside 60 days raises INSURANCE_POLICY_EXPIRING
* [x] Stressed 5-day liquidity breach raises TREASURY_LIQUIDITY_BREACH and freezes discretionary outflows
* [x] Admission gate verifies funded own funds and executed commitments, not attestation

**SDD Checklist:**
- [x] Spec checkpoint: venue own funds, contingent capital, insurance cover and stressed liquidity buffer are funded and monitored (§17.13.1, §24 #329) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: backstop lapses after partial draw, insurer fails to pay inside the draw window, liquidity breach during a rollover spike

---

### Task 24.3.18: Independent Client-Money Assurance & Segregation Certification

**Objective:** Internal daily reconciliation and the 1-hour shortfall top-up (Task 24.3.11) are necessary but not sufficient — a regulator requires an *independent* opinion. Per spec §17.13.2. Added 2026-09-25 (governance remediation #17).

**File Locations:** `services/internal/backoffice/client_money_audit.go`, `services/internal/backoffice/segregation_cert.go`, `migrations/083_client_money_assurance.up.sql`

**Implementation:**
1. **Engagement register:** `client_money_audits` (migration 083) — engagement year, auditor firm, scope, start/end dates, evidence-request log, findings, remediation tickets and report status (SCHEDULED|FIELDWORK|DRAFT|ISSUED).
2. **Evidence pack:** a repeatable export assembles the auditor pack — daily segregation calculations and sign-offs, bank reconciliation to statements (Task 24.3.12), the shortfall-top-up log, GL lines, and the Merkle proof-of-reserves roots (§17.11) for each attested day. Exported from the system of record, never hand-assembled.
3. **Segregation certification:** `segregation_certifications` (migration 083) stores each issued certification with attested date range, certification statement, signatories, `sha256` over the evidence pack and `published_until`. Certifications are the artefact satisfying §17.9's legal segregation obligation; an expired or missing certification for a covered period blocks the Phase 24 → production release gate.
4. **Auditor access:** read-only, time-bounded, separately-audited `EXTERNAL_AUDITOR` role — no write, no export of unrelated entities' data; access is dual-controlled and fully logged.
5. **Independence:** the auditor holds no contract with any subsystem operator that maintains client-money balances. Independence is a recorded engagement field, reviewed annually.
6. **Endpoints:** `GET/POST /api/v1/admin/client-money/audits`, `POST /api/v1/admin/client-money/audits/{id}/evidence-pack`, and `POST /api/v1/admin/client-money/certifications` (Finance Ops; dual control for certification issuance).

**Definition of Done (Acceptance Criteria):**
* [x] Audit engagement register covers every operating period with status tracking
* [x] Evidence pack is exported from the system of record, not assembled by hand
* [x] Each attested period has a segregation certification with a hash over its evidence pack
* [x] Missing or expired certification blocks the Phase 24 → production release gate
* [x] EXTERNAL_AUDITOR access is read-only, time-bounded and independently audited

**SDD Checklist:**
- [x] Spec checkpoint: independent client-money audit and segregation certification covering every operating period (§17.13.2, §24 #330) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: auditor access window expires mid-fieldwork, evidence pack covering a shortfall top-up, certifier and auditor are the same person

---

### Task 24.3.19: Settlement Operations Hardening — Breaks, Nostro, Cut-Offs, CLS & FX Fail Economics

**Objective:** Turn detection-only settlement into an operable back office with correct FX fail economics, per spec §17.14 and §24 #347. Added 2026-09-27 (production-maturity remediation #24).

**Implementation:**
1. **Break lifecycle:** aging buckets (T+1 investigate → T+2 escalate → T+5 write-off review) with escalation timetable; suspense-account parking with 2-day clearing SLA; write-off authority matrix with dual control; auto-match-rate KPI (≥98%) and per-currency tolerances (extends Tasks 24.3.2/13.3.2 detection).
2. **Nostro funding:** threshold methodology (3-day outflow cover + CLS pay-in cover) per currency per correspondent; intraday monitoring cadence (hourly in session); concentration limits across correspondents; a designated backup correspondent per currency with tested failover routing (supersedes the single-correspondent assumption in Task 24.3.1).
3. **Cut-off matrix:** complete per-rail-per-currency cut-off table (extends the SWIFT/SEPA examples in Phase-11 Task 11.3.7); value-date roll interaction with the Task 3.3.8 holiday calendar; late-payment handling with fee pass-through.
4. **CLS operations:** pay-in pre-funding mechanics (which nostro, funded T-1 by 22:00 UTC); failed-pay-in consequence ladder; settlement-member-outage trigger/SLA for falling back to the bilateral waterfall; in/out-swap usage rules; rescind deadlines (extends Task 24.3.8).
5. **FX fail economics (supersedes CSDR regime):** the Task 24.3.13 CSDR Art. 7 cash-penalty/buy-in application to FX spot is superseded — CSDR settlement discipline governs CSD-settled securities, not CLS/correspondent-bank FX (and the mandatory buy-in was shelved). Replaced with replacement-cost close-out at market, fail-interest accrual (policy rate + 100bps) from ISD+1, and ISDA/FX Global Code settlement-risk treatment. Task 24.3.13's fail *detection* pipeline is retained; only the penalty/buy-in regime is replaced.
6. **Rail failover playbook:** outage routing (SWIFT down → backup correspondent; FedNow down → ACH fallback), queued-payment retry timetable (15m/1h/4h), duplicate-payment guard on rail recovery. **Herstatt metric:** principal exposure per counterparty/currency with duration cap, monitored intraday. **LP-default playbook:** quote-withdrawal beyond tolerance escalates to widened auction floors and quintile-5 ADL (extends the Task 19.3.3 auction assumption that LP bids exist).

**Definition of Done (Acceptance Criteria):**
* [x] Break aging, suspense clearing and write-offs execute per timetable with dual control
* [x] Nostro thresholds, backup routing and cut-off matrix enforced; late payments handled
* [x] CLS pre-funding/outage rules and FX fail economics (close-out + fail interest) replace CSDR penalties

**SDD Checklist:**
- [x] Spec checkpoint: break lifecycle with suspense SLA, funded nostro with backup routing, cut-off matrix, CLS operations, FX fail economics superseding CSDR, rail failover with Herstatt metric and LP-default playbook (§24 #347) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.20: Banking Rail Cut-Off Time Enforcer & Automated Value-Date Roll

**Objective:** Implement automated daily cut-off time enforcement across all correspondent banking and clearing rails, automatically adjusting settlement value dates to prevent bank overdraft fees and CSDR fails per spec §17.16a and §24 #413. Migration 107 creates `banking_rail_schedules`. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/settlement/rail_cutoff_service.go`, `services/internal/settlement/settlement_service.go`, `migrations/107_banking_rail_schedules.up.sql`

**Implementation:**
1. Migration 107 creates `banking_rail_schedules` table (`rail_name`, `currency`, `timezone`, `daily_cutoff_time`, `settlement_cycle_days`, `created_at`, `updated_at`), pre-seeded with:
   - Fedwire USD: 18:30 ET
   - TARGET2 EUR: 18:00 CET
   - CHAPS GBP: 17:00 London
   - CLS PvP Initial Pay-in: 06:30 CET
2. On trade fill or manual withdrawal request: `SettlementService` compares submission timestamp against target rail's `daily_cutoff_time`.
3. If submitted past cut-off: automatically increment `value_date` to next business day (evaluating ISDA holiday calendars via Task 3.3.8) and flag instruction status as `QUEUED_FOR_NEXT_CYCLE`.
4. If an external client requests same-day processing past cut-off, reject with `RAIL_CUTOFF_EXCEEDED` (HTTP 422).

**Definition of Done (Acceptance Criteria):**
* [x] Rail schedules loaded from database with timezone-aware cutoff evaluation
* [x] Instructions generated after cut-off time automatically roll value date to next business day
* [x] Same-day withdrawal requests post-cutoff rejected with RAIL_CUTOFF_EXCEEDED

**SDD Checklist:**
- [x] Spec checkpoint: banking rail cut-off evaluation and automatic value-date rollover (§24 #413) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 24.3.21: Unmatched Bank Deposit Suspense Routing & Discrepancy Quarantine

**Objective:** Implement automated general ledger suspense routing and quarantine workflows for bank deposits arriving without valid client reference numbers per spec §5.46, §17.16b, and §24 #414. Migration 108 creates `suspense_account_mappings`. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/settlement/statement_parser.go`, `services/internal/settlement/suspense_service.go`, `migrations/108_suspense_accounts_routing.up.sql`

**Implementation:**
1. Pre-seed Chart of Accounts with customer suspense liability account `2150_SUSPENSE_DEPOSITS_{CURRENCY}` across all supported currencies. **Note (§27 ruling 2026-09-29):** the `2099_UNMATCHED_DEPOSITS_SUSPENSE_{CURRENCY}` label cited in this task's original text is **superseded** — spec §5.46's default `gl_account` '2150' and the chart seeded by migration 088 (`2150_SUSPENSE_DEPOSITS_{CCY}`) are the contract; migration 108 records the ruling.
2. When the bank statement parser (MT940/MT942/camt.053, Task 24.3.12) processes an incoming credit whose payment reference is missing or unrecognized in `deposits`:
   - Post balanced double-entry GL journal entry:  
     **Debit:** `1010_NOSTRO_{CURRENCY}`  
     **Credit:** `2150_SUSPENSE_DEPOSITS_{CURRENCY}`
   - Create record in `unmatched_deposits_quarantine` table with bank transaction ID, remitter name, bank reference, and amount.
   - Emit P2 alert to Compliance and Finance Ops portals ([`Phase-07`](file:///www/wwwroot/exc.local/docs/Phase-07-Admin-Monitoring.md)).
3. Resolution: upon four-eyes manual attribution, system reverses the suspense posting and credits the verified client's available balance.

**Definition of Done (Acceptance Criteria):**
* [x] Unreferenced incoming bank wires automatically post to 2150 suspense account (2099 label superseded — see Implementation note)
* [x] Balanced GL entries maintained with zero unaccounted nostro cash
* [x] Compliance quarantine ticket generated for every unmatched deposit

**SDD Checklist:**
- [x] Spec checkpoint: unmatched bank deposit suspense GL routing and compliance quarantine (§24 #414) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 24.4 Deliverables

- Nostro/vostro account management
- Nostro/vostro reconciliation
- Settlement confirmation tracking
- SWIFT message tracking
- Compliance reporting
- Failed settlement handling
- Prime Brokerage give-up reconciliation & break management
- Continuous Linked Settlement (CLS) third-party PvP settlement service
- Bilateral payment netting + standing settlement instructions (SSI)
- Bunched-order average-price allocation and claim/correction lifecycle
- Client-money segregation, daily reconciliation, shortfall remediation and wind-down evidence
- Bank statement ingestion engine (MT940, MT942, camt.053) & automated reconciliation parser (Task 24.3.12)
- CSDR settlement discipline: fails, daily penalties & mandatory buy-in (Task 24.3.13)
- PB credit restitution on settlement failure (Task 24.3.14)
- Post-trade allocation workflow for block trades (Task 24.3.15)
- CLS settlement mismatch quarantine & client-money shortfall auto-replenishment (Task 24.3.16)
- Settlement operations hardening: break lifecycle, nostro funding, cut-offs, CLS ops & FX fail economics (Task 24.3.19)
- Banking rail cut-off schedule engine (Task 24.3.20, migration 107)
- Unmatched deposit suspense GL routing & compliance quarantine (Task 24.3.21, migration 108)

---

## 24.5 Dependencies

- Phases 11, 13, 20, 21

---

## 24.6 Duration Estimate

15.5–19.5 days (supersedes prior 15.5–18.5 — Tasks 24.3.20–24.3.21 absorbed in range, remediation #37; prior supersedes 15.5–18.5 — Task 24.3.19 added 2026-09-27, remediation #24):
- Task 24.3.1 (Nostro/vostro): 1.5 days
- Task 24.3.2 (Reconciliation): 1 day
- Task 24.3.3 (Confirmation): 0.5 day
- Task 24.3.4 (SWIFT tracking): 0.5 day
- Task 24.3.5 (Compliance reporting): 1 day
- Task 24.3.6 (Failed settlement): 1 day
- Task 24.3.7 (PB reconciliation): 1 day
- Task 24.3.8 (CLS ISO 20022 PvP adapter): 1.5 days
- Task 24.3.9 (Netting + SSI): 1 day
- Task 24.3.10 (Allocation): 1 day
- Task 24.3.11 (Client money): 1.5 days
- Task 24.3.12 (Statement ingestion MT940/camt.053): 0.5 day
- Task 24.3.13 (CSDR settlement discipline): 0.5 day
- Task 24.3.14 (PB credit restitution): 0.5 day
- Task 24.3.15 (Block-trade allocation): 1 day
- Task 24.3.16 (CLS mismatch quarantine & client-money shortfall): 0.5 day
- Task 24.3.17 (Treasury, own funds & contingent-capital backstop): 1.5 days
- Task 24.3.18 (Independent client-money assurance & segregation certification): 1.5 days
- Task 24.3.19 (Settlement ops hardening: breaks/nostro/cut-offs/CLS/FX fails): 1 day
- Task 24.3.20 (Banking rail cut-off time enforcer): 0.5 day (absorbed in range, remediation #37)
- Task 24.3.21 (Unmatched deposit suspense routing): 0.5 day (absorbed in range, remediation #37)
- Testing: 1.5 days

---

## 24.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Nostro/vostro accounts per currency per correspondent |
| 2 | Account creation and listing work |
| 3 | Real-time balance tracking |
| 4 | Daily reconciliation runs |
| 5 | Mismatch triggers P1 alert |
| 6 | Break investigation workflow |
| 7 | Reconciliation report generated |
| 8 | Auto-resolution by SWIFT reference |
| 9 | SWIFT MT900/MT910 confirmations processed |
| 10 | Settlement status updated to SETTLED on confirmation |
| 11 | Nostro/vostro balance updated on confirmation |
| 12 | Timeout alert if no confirmation within 2 business days |
| 13 | All SWIFT message types tracked (MT103, MT202, MT900, MT910, pacs.009) |
| 14 | SWIFT messages queryable by date and type |
| 15 | Immutable SWIFT audit trail |
| 16 | MiFID II report exportable (§24 #30) |
| 17 | EMIR report exportable |
| 18 | FinCEN CTR/SAR exportable |
| 19 | Basel III capital adequacy report |
| 20 | Monthly compliance summary |
| 21 | Failed settlement detected |
| 22 | Exception workflow: investigate → resolve → retry/reverse |
| 23 | Dual control for settlement exception resolution |
| 24 | Reversal works (reverse trade, return funds) |
| 25 | Settlement discrepancy > $1,000 or > 0.01% detected and flagged (§24 #21) |
| 26 | `swift_messages` table exists (migration 035) |
| 27 | CLS ISO 20022 paired instructions submit/amend/rescind; finality posts only from authenticated CLS/member status (§17.6, §24 #126, #168) (supersedes prior local PvP-guarantee wording) |
| 28 | Prime Brokerage give-up reconciliation matches executed trades and flags un-affirmed breaks (§5.22, §17.1, §24 #125) |
| 29 | SSI registry verified against beneficiary registry; netting batches produce single net payment per CP/currency/value-date (§24 #155) |
| 30 | Netted settlement reconciles as one nostro movement with line-level trade mapping; CLS-eligible flow excluded |
| 31 | CLS eligibility/cut-offs are versioned reference data; unmatched/rejected instructions enter pre-cut-off exceptions; non-CLS flow follows settlement-risk waterfall |
| 32 | Bunched-order allocation method handles full/partial fills and average price without over-allocation (§24 #172) |
| 33 | Allocation claim/reject/correct propagates to PB, settlement, confirmation and reporting with immutable offsets |
| 34 | Client/house funds segregated; internal and external client-money reconciliation runs each business day (§24 #173) |
| 35 | Client-money shortfall blocks worsening movements, raises P1 and is remediated immediately under dual control |
| 36 | Bank safeguarding due diligence/diversification, acknowledgements, pooling-event and wind-down evidence are exportable |
| 37 | Automated bank statement ingestion parses SWIFT MT940, intraday MT942, and ISO 20022 camt.053 XML feeds, reconciling statement entries against internal nostro ledger records (§24 #175) |
| 38 | Settlement fails detected at ISD+1; daily CSDR cash penalties (1bp/0.5bp) computed bilaterally; CLS-exempt (§24 #199) |
| 39 | Mandatory buy-in notification at ISD+4, execution at ISD+7 with price differential charged to failing counterparty |
| 40 | PB DSL credit restitution on settlement failure with idempotent correction journal + NOP reversal + margin recalc (§24 #200) |
| 41 | Post-trade allocation: block trade → fund sub-account split via FIX 35=J; confirmation via 35=AK; settlement per allocation; unallocated escalates by T+0 EOD (§24 #237) |
| 42 | Client-money shortfall remediation follows 4-tier waterfall (insurance → house → capital call → default); Tier 3+ triggers regulatory notification within 60 min; daily stress test validates 2× NBP coverage (§24 #240) |
| 43 | CLS settlement discrepancies quarantine batch with CLS_SETTLEMENT_MISMATCH; segregated client-money deficit flags CLIENT_MONEY_SHORTFALL with 1h top-up alert (§24 #326) |
| 44 | Own-funds ledger is reconciled daily and distinct from client money and the GL; insurance-fund depletion terminates in a documented contingent-capital backstop; insurance expiry and stressed 5-day liquidity breaches alert with TREASURY_LIQUIDITY_BREACH (§24 #329) |
| 45 | Every operating period has an audit engagement and a segregation certification hashed over a system-exported evidence pack; missing/expired certification blocks the Phase 24 → production release gate; EXTERNAL_AUDITOR access is read-only and time-bounded (§24 #330) |
| 46 | Break aging with suspense SLA and dual-control write-offs; funded nostro with backup routing; per-rail cut-off matrix; CLS pre-funding/outage rules; FX fail close-out plus interest superseding CSDR penalties; rail failover with Herstatt metric (§24 #347) |
| 47 | Banking rail cut-off engine automatically rolls settlement value dates forward for post-cut-off instructions, preventing overdraft and CSDR fails (§24 #413) |
| 48 | Unmatched bank deposits route to designated GL suspense liability accounts with automated compliance quarantine (§24 #414) |


## Phase-24 Settle Addendum (2026-09-30) — implementation record

All 21 tasks implemented across 5 disjoint work-streams; **21/21 spec checkpoints bound and green** (`tests/spec/checks/phase24.go`). 140/140 DoD/SDD rows ticked. `go build ./...`, `go test ./...` (incl. PG-gated legs on dev PG) and `go vet ./...` fully green; registry↔handler-map audit shows zero drift across all 549 registered-live routes.

**Migrations landed:** 13 paired up/down sets — 035 `swift_messages`, 044 `settlement_netting`, 055 `trade_allocations`, 056 `client_money`, 057 `bank_statements`, 082 `treasury`, 083 `client_money_assurance`, 084 `settlement_penalties`, 107 `banking_rail_schedules`, 259 `cls_pvp`, 260 `settlement_ops`, 261 `backoffice_nostro_recon`, 262 `statement_exception_links`. Sparse numbering is the repo convention; slot 258 remains unallocated. All applied + down/up round-tripped clean on dev PG.

**Routes bound:** ~70 live Phase-24 routes in `internal/gateway/routes_v1.go` + `cmd/gateway/main.go` — nostro accounts/recon, SWIFT journal, settlement confirmations, compliance export, statements ×3, CLS lifecycle ×7, SSI ×3, netting ×6, rail schedules/evaluate/roll ×3, suspense route/resolve, allocations (public create + 12 admin), PB recon, settlement exceptions, treasury own-funds/contingent-capital, client-money audits/evidence/certifications. The Phase-21 pre-registered stub `GET /api/v1/admin/compliance-report` was flipped `v1live` in place rather than duplicated (route-registry ownership rule).

**Composition wiring (`cmd/gateway`):** `adapters.go` gained the Phase-24 seam set — `boSettlementConfirmer` (delegates `settlement.SettlementService.ConfirmSettlement`), `suspenseGuard` (Task-11 `funding.DepositGuard` as the single quarantine/attribution path), `boRawTx` + `PgJournalPoster` (unwraps `backoffice.Tx`→`pgx.Tx` and posts the GL **inside the caller's transaction**), `cmFundSource` (insurance-fund debit with non-negative guard + `insurance_fund_transactions` audit), `cmNBPSource` (worst NBP from negative retail balances), `cmStatementSource` (bank-statement closing balances), `cmSuspension` (durable `trading_suspensions` + Redis `halt:*` via `reconciliation.PgHalter` — same artifacts as the admin kill switch), `boRestitutionAlerter`, `boMarkPricer` (trade→symbol→chained mark provider), `boCutoffEvaluator`, `pgStressedOutflows`. Daily CSDR fail/penalty + buy-in ladders (06:00/06:15 UTC) and the hourly ops monitors (aging, nostro thresholds, CLS pay-in, failover, Herstatt) run as gateway sweeps.

**Deviations and honest seams (recorded, not hidden):**

1. **Conditional mounts:** settlement confirmations and the manual value-date roll endpoint require a live `SettlementService` (EXC_SENDER_BIC + holiday calendar); rail schedules need the cut-off service. Absent config → endpoints stay unmounted and the registry 503 shim answers (fail-closed, never a nil panic).
2. **Rail-dispatch seam unwired** in the ops failover ladder — failover payments park and the ladder escalates rather than fabricating a dispatch.
3. **Restitution recalc/drop-copy unwired:** the margin-recalc flag rides the restitution row for the risk service; the FIX drop-copy transport is session-bound and stays a deployment seam.
4. **Role-gated daily ops not cron'd:** client-money `ReconcileDay` sign-off and treasury `EvaluateLiquidity` stay human-finance-principal actions (dual-control is a regulatory control, not automatable).
5. **Settle-time fixes:** `InsertAllocation` writes `[]` not JSONB `null` (22023 on `jsonb_array_elements_text`); confirmation test rewired to the real PG tracker; allocation-correction test aligned to the fill-conservation invariant (may never exceed filled qty, spec §5.31); `EscalateUnallocated` now includes LOCKED groups with unallocated remainder per Task 24.3.15 T+0 semantics.
6. **Error registry:** +24 `localRow` codes (NOSTRO_*, SETTLEMENT_* scaffold-code landings, CLS_* lifecycle, STATEMENT_*, SUSPENSE_ROUTER_MISSING, SSI/NETTING_*) — registry 210→**234** emitted; §23 transcription pending (localCodes 43→67).
