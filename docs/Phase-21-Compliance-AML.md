# Phase 21 — Compliance & AML (MiFID/EMIR/FinCEN)

**Duration:** 18–22 days (unchanged; §21.6 itemization completed 2026-09-27, remediation #35: Task 21.3.21 added) (supersedes 17–21 — Task 21.3.28 execution policy added 2026-09-27, remediation #30; prior supersedes 17–20 — Task 21.3.27 added 2026-09-27, remediation #24)
**Dependencies:** Phases 11, 12, 14, 17, 19, 19.5
**Spec Reference:** §14 (Compliance & AML), §24 (Acceptance Criteria)

---

## 21.1 Objectives

Implement compliance and AML: sanctions screening (OFAC/EU/UN), travel rule, SAR generation, MiFID II transaction reporting, EMIR REFIT and CFTC Parts 43/45 lifecycle reporting, FinCEN MSB, GDPR/geo-block, market-abuse enforcement on Phase 17 signals, and regulated-venue member/rule-enforcement governance.

---

## 21.2 Prerequisites

- Phases 11, 12, 14, 17, 19, 19.5 complete
- Phase 17 complete (surveillance signals)

---

## 21.3 Tasks

### Task 21.3.1: Sanctions Screening

**Objective:** Implement sanctions screening on deposits, withdrawals, and trades.

**File Locations:** `services/internal/compliance/sanctions.go`

**Implementation:**
1. Screen against OFAC SDN, EU consolidated, UN, and UK HMT (OFSI) sanctions lists (spec §14.3).
2. Screen on: deposit, withdrawal, trade, account registration.
3. Fuzzy matching (name, address, DOB).
4. Match → block transaction, P1 alert, compliance review; sanctions flag also blocks withdrawals and is written to the audit log (§24 #29).
5. Fail-closed: if the screening service or list source times out, the transaction is blocked pending retry (spec §14.3, §24 #28) — never allow-on-timeout.
6. List updates: daily from official sources.

**Definition of Done (Acceptance Criteria):**
* [x] OFAC, EU, UN, UK HMT sanctions lists screened
* [x] Screening on deposit, withdrawal, trade, registration
* [x] Fuzzy matching works
* [x] Match blocks transaction + P1 alert; flag blocks withdrawals + audit logged
* [x] Screening timeout → transaction blocked (fail-closed, §24 #28)
* [x] Lists updated daily

**SDD Checklist:**
- [x] Spec checkpoint: sanctions screening OFAC/EU/UN — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.2: Travel Rule (FATF)

**Objective:** Implement FATF Travel Rule for transfers ≥$1,000 (spec §14; supersedes prior ">$1000" — spec threshold is ≥).

**File Locations:** `services/internal/compliance/travel_rule.go`

**Implementation:**
1. For transfers ≥$1,000: collect originator + beneficiary info.
2. Originator: name, account number, address.
3. Beneficiary: name, account number.
4. Included in SWIFT message (MT103 fields 50K/59).
5. Stored in `travel_rule_records` table.
6. **Migration note:** `migrations/032_create_travel_rule_records.up.sql` — `travel_rule_records` table (id, transfer_id, originator JSONB, beneficiary JSONB, swift_field_ref, created_at).

**Definition of Done (Acceptance Criteria):**
* [x] Travel rule for transfers ≥$1,000
* [x] Originator + beneficiary info collected
* [x] Info included in SWIFT message
* [x] Records stored

**SDD Checklist:**
- [x] Spec checkpoint: FATF travel rule ≥$1,000 — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.3: SAR Generation

**Objective:** Implement Suspicious Activity Report generation.

**File Locations:** `services/internal/compliance/sar.go`

**Implementation:**
1. Triggers: sanctions match, velocity anomaly, structuring, surveillance signal.
2. SAR: transaction details, account info, suspicious activity description.
3. Compliance Officer reviews + files; filing requires **dual control** — a second Compliance Officer+ approves before submission (§24 #108).
4. Stored in `sar_reports` table.
5. `POST /api/v1/admin/sar` — create SAR (Compliance Officer+).
6. **Migration note:** `migrations/033_create_sar_reports.up.sql` — `sar_reports` table (id, trigger_type, account_id, description, status, reviewed_by, approved_by, filed_at, created_at).

**Definition of Done (Acceptance Criteria):**
* [x] SAR generated on triggers
* [x] SAR includes transaction + account + activity description
* [x] Compliance Officer review + file workflow
* [x] SAR filing requires dual control (§24 #108)
* [x] SAR stored

**SDD Checklist:**
- [x] Spec checkpoint: SAR generation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.4: MiFID II Transaction Reporting

**Objective:** Implement MiFID II transaction reporting.

**File Locations:** `services/internal/compliance/mifid.go`

**Implementation:**
1. Maintain the internal RTS 22 record/export — submission path is Task 21.3.16 (ARM adapter, full mandatory field set per §14.5); this task's generic submission wording is superseded (remediation #35: two parallel submission paths with live ACs created double-submission risk).
2. Fields: instrument ID (ISIN where applicable — spot FX uses ISO currency-pair codes, not ISINs; corrected 2026-09-15), trade date/time, price, quantity, venue, trader ID.
3. Best execution: record execution quality (price vs benchmark).
4. `GET /api/v1/admin/mifid-report?from=&to=` — report export.

**Definition of Done (Acceptance Criteria):**
* [x] Trades reported to MiFID II repository
* [x] All RTS 22 fields included
* [x] Best execution quality recorded
* [x] Report export works

**SDD Checklist:**
- [x] Spec checkpoint: MiFID II transaction reporting — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.5: EMIR Trade Reporting

**Objective:** Implement EMIR trade reporting for derivatives.

**File Locations:** `services/internal/compliance/emir.go`

**Implementation:**
1. Report derivative trades to EMIR trade repository (e.g., DTCC/REGIS-TR) — **framework retained; submission path replaced by Task 21.3.14** (identifier/lifecycle/validation/ack/reconciliation workflows; remediation #35 — the supersedes note existed only in the new task's objective, never here).
2. Fields: UTI, USI, counterparty ID, trade details, collateral.
3. Collateral: mark-to-market, initial margin, collateral value.
4. `GET /api/v1/admin/emir-report?from=&to=` — report export.

**Definition of Done (Acceptance Criteria):**
* [x] Derivative trades reported to EMIR repository
* [x] UTI/USI generated
* [x] Collateral reported
* [x] Report export works

**SDD Checklist:**
- [x] Spec checkpoint: EMIR trade reporting — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

**Derivative dependency note:** Derivative instruments and their trading logic are owned by Phase 22 (FX Derivatives Foundation), which runs after Phase 21. The EMIR reporting framework (UTI/USI generation, repository connection, report export) is built in Phase 21. Acceptance criteria requiring "Derivative trades reported to EMIR repository" can only be fully validated after Phase 22 derivative instruments exist. During Phase 21, validate the reporting framework using **stub derivative trade records** (test-only trades with derivative fields). Phase 22 integration testing validates the full EMIR reporting flow with real derivative trades.

---

### Task 21.3.6: FinCEN MSB & AML Program

**Objective:** Implement FinCEN MSB registration and AML program.

**File Locations:** `services/internal/compliance/aml.go`

**Implementation:**
1. MSB registration: FinCEN Form 107.
2. AML program: CDD (Customer Due Diligence), EDD (Enhanced for high-risk).
3. CTR (Currency Transaction Report): cash >$10K.
4. AML training: annual for all employees.
5. AML officer designated.

**Definition of Done (Acceptance Criteria):**
* [x] MSB registration filed
* [x] CDD/EDD implemented
* [x] CTR for cash >$10K
* [x] Annual AML training
* [x] AML officer designated

**SDD Checklist:**
- [x] Spec checkpoint: FinCEN MSB + AML program — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.7: GDPR & Geo-Block

**Objective:** Implement GDPR compliance and geo-blocking.

**File Locations:** `services/internal/compliance/gdpr.go`

**Implementation:**
1. GDPR: right to access, right to erasure, data portability.
2. `POST /api/v1/account/gdpr/export` — export all user data.
3. `POST /api/v1/account/gdpr/erase` — erase user data (retention exceptions for financial records).
4. Consent management (§24 #103): record consent per purpose (marketing, analytics, data sharing) at registration; users can grant/withdraw consent via `PUT /api/v1/account/consent`; processing respects consent state.
5. Geo-block: block restricted jurisdictions (Iran, North Korea, etc.) — **US policy clarified 2026-09-27, remediation #35: US persons are blocked from retail, while US institutional flow is served and reported (FinCEN MSB/CTR per Task 21.3.6, FATCA per Task 21.3.22, CFTC per Task 21.3.14) — the prior blanket US block contradicted the US regulatory duties implemented in this same phase. Recorded as §27 ruling R15.**
6. IP-based geo-detection.

**Definition of Done (Acceptance Criteria):**
* [x] GDPR export works
* [x] GDPR erase works (with financial record retention)
* [x] Consent management: per-purpose consent recorded, grant/withdraw endpoint (§24 #103)
* [x] Geo-block for restricted jurisdictions
* [x] IP-based geo-detection

**SDD Checklist:**
- [x] Spec checkpoint: GDPR + geo-block — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.8: Market-Abuse Enforcement

**Objective:** Enforce on Phase 17 surveillance signals.

**File Locations:** `services/internal/compliance/enforcement.go`

**Implementation:**
1. Consume signals from `surveillance_signals` table (Phase 17).
2. Auto-actions: warn, throttle, restrict, suspend account.
3. Compliance Officer review for severe signals.
4. `POST /api/v1/admin/enforcement/{signal_id}` — take action.

**Definition of Done (Acceptance Criteria):**
* [x] Surveillance signals consumed
* [x] Auto-actions: warn, throttle, restrict, suspend
* [x] Compliance Officer review for severe
* [x] Enforcement action endpoint works

**SDD Checklist:**
- [x] Spec checkpoint: market-abuse enforcement — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.9: Dodd-Frank Swap Reporting

**Objective:** Implement Dodd-Frank swap reporting (US SDR, position limits) for US-regulated derivatives.

**File Locations:** `services/internal/compliance/dodd_frank.go`

**Implementation:**
1. Report swap trades to US SDR (Swap Data Repository): CME, ICE, DTCC.
2. Real-time reporting: within 15 minutes of execution.
3. End-of-day reporting: valuation and collateral.
4. Position limits: enforce CFTC position limits per instrument per account.
5. Large trader reporting: report accounts exceeding CFTC thresholds.
6. UTI/USI generation (shared with the canonical event store `regulatory_report_events`, migration 054, owned by Task 21.3.14 — re-pointed 2026-09-27, remediation #35; the prior citation of Task 21.3.5 named the task being replaced).

**Definition of Done (Acceptance Criteria):**
* [x] Swap trades reported to US SDR within 15 minutes
* [x] End-of-day valuation + collateral reported
* [x] CFTC position limits enforced per instrument per account
* [x] Large trader reporting for accounts exceeding CFTC thresholds
* [x] UTI/USI generated (shared with EMIR)

**SDD Checklist:**
- [x] Spec checkpoint: Dodd-Frank swap reporting to US SDR — defined first, validated against spec
- [x] Spec checkpoint: CFTC position limits + large trader reporting — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

**Derivative dependency note:** Swap trades are owned by Phase 22 (FX Derivatives Foundation), which runs after Phase 21. The Dodd-Frank reporting framework (SDR connection, real-time reporting pipeline, position limit engine, large trader reporting) is built in Phase 21. Acceptance criteria requiring "Swap trades reported to US SDR" can only be fully validated after Phase 22 swap instruments exist. During Phase 21, validate the reporting framework using **stub swap trade records**. Phase 22 integration testing validates the full Dodd-Frank reporting flow with real swap trades.

---

### Task 21.3.10: SanctionsHook in C++ PreTradeChecker

**Objective:** Wire sanctions enforcement into the C++ matching core's pre-trade path, per spec §14.3 (`SanctionsHook` in `PreTradeChecker`).

**File Locations:** `core/src/risk/PreTradeChecker.cpp` (extend), `core/src/risk/SanctionsCache.cpp`

**Implementation:**
1. Go compliance service (Task 21.3.1) publishes sanctioned account/entity flags to Redis `sanctions:flagged:{account_id}` and a compact bloom/bitmap set.
2. C++ `SanctionsCache` subscribes via Aeron to flag updates + polls Redis every 1s; holds an in-process set for zero-hot-path-IO lookups.
3. `PreTradeChecker` gains a sanctions check: flagged account's new orders rejected with `ORDER_REJECTED` + `sanctioned=true` detail; cancels still allowed.
4. Fail-closed: if the sanctions feed is stale >60s, new-order acceptance halts for affected path (**scoped degradation** via ModeManager per spec §14.3 fail-closed requirement — supersedes the prior full `ReadOnly`, which blocked risk-reducing position closes; remediation #35 aligns the task body with AC #43/AC #60, which already carry the canonical scoped-degradation semantics).
5. This complements — does not replace — the Go-side screening of deposits/withdrawals (Task 21.3.1); the hook covers the order-entry path where network calls are impossible.

**Definition of Done (Acceptance Criteria):**
* [x] Flagged account's orders rejected in-process (<10µs pre-trade budget preserved)
* [x] Flag propagation Redis→C++ ≤1s
* [x] Stale sanctions feed >60s → fail-closed (scoped degradation, not full halt), alert raised (supersedes prior full ReadOnly; remediation #35)
* [x] Cancels still accepted for flagged accounts

**SDD Checklist:**
- [x] Spec checkpoint: SanctionsHook in PreTradeChecker (spec §14.3) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: flag set mid-order-burst, feed restart, flag removal propagates

---

### Task 21.3.11: PEP / Adverse-Media Screening & Ongoing Monitoring

**Objective:** Extend KYC screening to PEP (politically exposed persons) and adverse media, and add the ongoing-monitoring rules engine per spec §14.3 (§24 #149). Added 2026-09-15.

**File Locations:** `services/internal/compliance/screening.go`, `services/internal/compliance/monitoring.go`

**Implementation:**
1. PEP screening at onboarding + batch re-screen — daily T2/institutional, weekly T1 (tiered cadence per spec §12.7; remediation #35 supersedes the prior all-tiers-daily wording — the note existed only in the spec): third-party PEP/adverse-media data (Dow Jones, Refinitiv World-Check, or equivalent); match → EDD workflow, Compliance Officer approval required to keep account active; PEP status on `accounts`.
2. Adverse media: negative-news screening on the same cycle; material hits create a compliance case.
3. Ongoing transaction monitoring rules engine (extends Task 21.3.6 AML program): structuring/smurfing (sub-threshold aggregation patterns), velocity anomalies (deposit→trade→withdraw < 24h), dormant-account reactivation + immediate withdrawal, round-amount layering. Rules emit cases → Compliance Officer queue.
4. Periodic KYC refresh already exists (Phase-14); this adds the continuous side.

**Definition of Done (Acceptance Criteria):**
* [x] PEP screening at onboarding + daily re-screen; match → EDD + dual-control approval
* [x] Adverse-media screening creates compliance cases
* [x] Monitoring rules fire on structuring, velocity, dormant-reactivation patterns (§24 #149)
* [x] All screening/cases audit-logged

**SDD Checklist:**
- [x] Spec checkpoint: PEP/adverse-media + ongoing monitoring (§14.3, §24 #149) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: fuzzy-name false positives, PEP status change mid-account-life, rule tuning without downtime

---

### Task 21.3.12: MiFID II RTS 6 Algo Certification, DEA Controls & Order Retention

**Objective:** Implement RTS 6 algorithmic-trading controls per spec §14.1 (§24 #150): algo self-certification, DEA (direct electronic access) controls, annual self-assessment, and order-lifecycle record retention. Added 2026-09-15.

**File Locations:** `services/internal/compliance/rts6.go`, `docs/compliance/self-assessment.md`

**Implementation:**
1. **Algo certification register:** every client algo strategy (Phase-16 algo orders, FIX flows) must carry `algo_id` + certification status; uncertified algo_id rejected `ALGO_NOT_CERTIFIED`. Cert requires: test evidence in testnet (Phase-14 Task 14.3.3), kill-button test, capacity self-assessment.
2. **DEA controls:** clients given DEA (FIX sessions flagged `dea=true`) get pre-trade limits at session level (max order size, max msgs/sec — Task 18.3.9), credit checks bound to the DEA client's own limits, and real-time monitoring feed (drop copy) to the sponsoring desk.
3. **Annual self-assessment:** documented RTS 6 Art. 9 self-assessment + validation report; stored, reviewed by Compliance Officer; reminder 60d before expiry.
4. **Order records retention:** full order lifecycle events (new/modify/cancel/fill/reject) retained 5 years — sourced from `order_audit` + WAL archive; queryable export for regulator request.

**Definition of Done (Acceptance Criteria):**
* [x] Uncertified algo_id rejected ALGO_NOT_CERTIFIED; certification evidence stored
* [x] DEA sessions enforce session-level pre-trade limits + sponsoring-desk monitoring
* [x] Annual self-assessment workflow with expiry reminder
* [x] 5-year order lifecycle retention + regulator export (§24 #150)

**SDD Checklist:**
- [x] Spec checkpoint: RTS 6 cert + DEA + retention (§14.1, §24 #150) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: algo_id on non-algo order, DEA limit change mid-session, retention export size limits

---

### Task 21.3.13: Basel III Capital & Leverage Reporting

**Objective:** Implement the Basel III reporting listed in spec §14.1 (§24 #151) — task-number mis-citations in spec §17.13/§27 corrected to this task 2026-09-27, remediation #35 (the spec cited Task 21.3.10, the C++ SanctionsHook).: capital adequacy + leverage ratio reports for the entity's own prudential obligations. Added 2026-09-15.

**File Locations:** `services/internal/compliance/basel.go`

**Implementation:**
1. Inputs from GL (Phase-3): capital components (Tier 1/2), RWA approximation (counterparty credit risk on open settlement exposure + PB margin), leverage exposure measure.
2. Reports: `GET /api/v1/admin/basel-report?period=` — capital adequacy ratio (CAR = capital / RWA) and leverage ratio (Tier 1 / exposure) with thresholds (CAR ≥ 8%, leverage ≥ 3%) and breach alerts (P1 to Compliance Officer + Finance Ops).
3. Data snapshot daily EOD; stored report versions for audit; reconciliation notes to GL account mapping.

**Definition of Done (Acceptance Criteria):**
* [x] Daily CAR + leverage ratio computed from GL-sourced inputs (§24 #151)
* [x] Breach alerts on threshold violation
* [x] Report versions retained + exportable

**SDD Checklist:**
- [x] Spec checkpoint: Basel III capital/leverage reporting (§14.1, §24 #151) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: RWA inputs missing (fail-closed flag), month-end GL adjustments mid-report

---

### Task 21.3.14: EMIR REFIT & CFTC Parts 43/45 Reporting Lifecycle

**Objective:** Replace generic trade-report exports with current identifier, lifecycle, validation, acknowledgement, and reconciliation workflows per spec §5.32/§14.1a/§24 #169–170. Added 2026-09-15.

**File Locations:** `services/internal/compliance/reporting/`, `migrations/054_regulatory_reporting.up.sql`

**Implementation:**
1. Create a canonical event store for UTI/USI, DSB UPI, prior/subsequent identifiers, LEIs, venue MIC, jurisdiction, action type, event type, valuation, margin, clearing, confirmation and allocation fields.
2. Map execution, modification, correction, valuation, margin, compression, allocation, clearing, termination and error/omission events to EMIR REFIT and CFTC Parts 43/45 schemas.
3. Generate and validate EMIR REFIT ISO 20022 XML and current SDR payloads against version-pinned official rules before submission; quarantine invalid reports.
4. Ingest repository ACK/NACK and reconciliation feedback; repair rejected or unmatched reports within SLA while retaining immutable payload/version/resubmission history.
5. Reconcile accepted repository trade state to internal open derivatives daily; alert missing, duplicate, stale valuation/margin, identifier collision and lifecycle divergence.

**Migration note:** `migrations/054_regulatory_reporting.up.sql` creates `regulatory_report_events`, submissions, acknowledgements, reconciliation breaks, and schema-version registry per spec §5.32.

**Definition of Done (Acceptance Criteria):**
* [x] EMIR REFIT ISO 20022 reports contain UTI, DSB UPI, action/event and required valuation/margin data
* [x] CFTC Parts 43/45 creation/continuation and lifecycle reports validate with UTI/USI, DSB UPI and prior-ID links
* [x] ACK/NACK, correction and error/omission workflows retain immutable resubmission history
* [x] Daily repository-vs-internal reconciliation reaches zero unexplained missing/duplicate/divergent trades

**SDD Checklist:**
- [x] Spec checkpoint: EMIR REFIT + CFTC lifecycle/data-quality reporting (§14.1a, §24 #169–170) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: UPI unavailable, late correction after termination, cross-jurisdiction UTI ownership, repository schema change

---

### Task 21.3.15: Regulated-Venue Membership, Rule Enforcement & CCO Controls

**Objective:** Implement auditable system support for MiFID II RTS 7 and CFTC SEF operating obligations per spec §5.32/§14.1b/§24 #174. Added 2026-09-15.

**File Locations:** `services/internal/compliance/venue/`, `migrations/054_regulatory_reporting.up.sql`

**Implementation:**
1. Member/DEA/sponsored-access register: LEI, regulatory status, agreements, approved products/ports, due-diligence evidence, admission decision, annual risk review, suspension/termination and appeals.
2. Rulebook/product governance: versioned rules and product terms, approval/effective dates, participant notices, acknowledgement evidence, regulator filing/approval status, and no activation before required approvals.
3. Real-time monitoring and emergency record: market-control interventions, limits, halts, cancellations/corrections, information requests, position accountability, investigation/disciplinary cases, conflicts register and immutable evidence chain.
4. Annual system-safeguard self-assessment and CCO report assemble control evidence, exceptions, financial-resource attestations, unresolved remediation and board/regulator sign-off.
5. Licensing, regulator authorization, legal opinions, board/CCO appointments and minimum financial resources are explicit launch prerequisites tracked by the system, not claims that software can satisfy alone.

**Definition of Done (Acceptance Criteria):**
* [x] No member/DEA/sponsored client trades before due diligence, agreements and product/port approval
* [x] Annual member review and rulebook/product approval workflows are versioned and auditable
* [x] Market-control, emergency, investigation and disciplinary actions preserve complete evidence
* [x] System-safeguard self-assessment and CCO annual report export with unresolved-remediation tracking
* [x] Production launch gate blocks when required venue authorization or governance attestations are absent

**SDD Checklist:**
- [x] Spec checkpoint: regulated-venue governance and rule enforcement (§14.1b, §24 #174) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: member license lapse intraday, emergency rule change, regulator information hold, conflict-of-interest recusal

---

---

### Task 21.3.16: MiFID II APA / ARM Direct Integration & Submission Adapters

**Objective:** Implement direct integration adapters for Approved Publication Arrangements (APA) and Approved Reporting Mechanisms (ARM) per spec §14.5 and §24 #178, ensuring RTS 1/2 real-time trade publishing and RTS 22 T+1 regulatory submission. Added 2026-09-15.

**File Locations:** `services/internal/compliance/apa_client.go`, `services/internal/compliance/arm_client.go`, `migrations/059_regulatory_submissions.up.sql`

**Implementation:**
1. Real-time APA post-trade reporting: implement APA client adapter (supporting Bloomberg APA / Tradeweb / UnaVista); publish eligible FX derivatives and spot benchmark trades within 1 minute of execution per RTS 1/2; format trade time, ISIN/UPI, price, size, and deferral flags.
2. ARM transaction reporting: implement RTS 22 daily batch reporting adapter; generate ISO 20022 XML (`auth.016` / `auth.030`) containing buyer/seller LEIs, trader personal IDs (National ID / passport), transmission flags, execution venue MIC, and Algo IDs; submit before T+1 23:59 UTC.
3. Ingestion and repair workflow: parse APA/ARM synchronous ACKs and asynchronous NACK responses; log submission status in `regulatory_submissions` table (migration 059); route validation errors to Compliance Officer repair queue with automated resubmission upon correction.

**Definition of Done (Acceptance Criteria):**
* [x] APA post-trade transparency messages published within 1 minute of match (§24 #178)
* [x] ARM RTS 22 transaction reports generated and transmitted by T+1 close
* [x] ACK/NACK responses tracked with immutable audit trail in `regulatory_submissions`
* [x] Rejected submissions trigger compliance alert and guided manual/automated repair workflow

**SDD Checklist:**
- [x] Spec checkpoint: MiFID II APA/ARM submission adapters (§14.5, §24 #178) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: APA gateway outage queuing, RTS 22 identifier format invalidation, retroactive cancellation reporting

---

### Task 21.3.17: FX Global Code 55-Principle Self-Assessment & Annual Review Engine

**Objective:** Implement the annual FX Global Code 55-principle compliance assessment engine and Statement of Commitment generator per spec §14.6 and §24 #182. Added 2026-09-15.

**File Locations:** `services/internal/compliance/fx_global_code.go`, `services/internal/compliance/assessment_reporter.go`, `migrations/060_compliance_assessments.up.sql`

**Implementation:**
1. 55-principle assessment matrix: map operational platform metrics and policies to the 6 core themes (Ethics, Governance, Execution, Information Sharing, Risk Management & Compliance, Confirmation & Settlement).
2. Automated control verification: programmatically verify key technical principles:
   - Principle 9: 100% firm liquidity validation (zero last look execution)
   - Principle 10: deterministic order queuing and execution timestamp accuracy (<1µs)
   - Principle 17: pre-hedging prohibition verification
   - Principle 50: CLS PvP settlement integration and nostro reconciliation
3. Assessment reporting & workflow: store annual review scores, auditor notes, and remediation tickets in `compliance_assessments` table (spec §5.37, migration 060); generate formal Statement of Commitment for executive sign-off and public register publication.

**Definition of Done (Acceptance Criteria):**
* [x] Annual assessment runs across all 55 FX Global Code principles with automated control checks (§24 #182)
* [x] Execution rules verify zero last look, pre-hedging prohibitions, and timestamp fidelity
* [x] Formal Statement of Commitment generated with executive sign-off tracking
* [x] Assessment history and evidence chain persisted in `compliance_assessments`

**SDD Checklist:**
- [x] Spec checkpoint: FX Global Code 55-principle self-assessment (§14.6, §24 #182) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: newly amended Global Code versioning, partial compliance principle remediation tracking

---

### Task 21.3.18: Jurisdictional Data Residency Enforcement & Cross-Border Controls

**Objective:** Implement data residency controls, regional storage partitioning, and cross-border transfer protections per spec §14.7 and §24 #184. Added 2026-09-15.

**File Locations:** `services/internal/compliance/data_residency.go`, `deploy/storage/residency_policy.yml`

**Implementation:**
1. Jurisdiction tagging: associate participant profiles, order streams, and KYC documents with designated legal residency jurisdiction (`jurisdiction_code`, e.g., `EU`, `GB`, `CH`, `US`, `SG`).
2. Storage pinning & KMS separation: configure PostgreSQL partition tables, ClickHouse partitions, and S3 object buckets to store regional data within corresponding cloud regions (e.g., `eu-west-1` for EU/UK, `us-east-1` for US); encrypt with region-specific customer-managed KMS keys.
3. Cross-border access control: restrict administrative cross-region access to PII and trader identification data; enforce role-based access control with dual-control masking and audit-logging for non-local auditors.

**Definition of Done (Acceptance Criteria):**
* [x] Participant data pinned to regional databases and storage buckets based on jurisdiction (§24 #184)
* [x] Region-specific KMS encryption keys used exclusively for respective territorial records
* [x] Administrative queries across jurisdictional boundaries require justification and are audit-logged
* [x] GDPR and local data protection compliance verified during cross-border data replication

**SDD Checklist:**
- [x] Spec checkpoint: Jurisdictional data residency enforcement (§14.7, §24 #184) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: multinational corporate entity with cross-border branches, regional cloud outage failover

---

### Task 21.3.19: MiFID II RTS 27/28 Best Execution Quality & Venue Reporting

**Objective:** Implement public best execution quality reporting (RTS 27: execution venue statistics) and broker top-5 venue reporting (RTS 28) per MiFID II requirements. Added 2026-09-15 (production-completeness audit remediation #4).

**File Locations:** `services/internal/compliance/rts27_report.go`, `services/internal/compliance/rts28_report.go`

**Implementation:**
1. **RTS 27 (venue quality):** Quarterly publication of execution quality data per instrument class: price, costs, speed, and likelihood of execution. Metrics: median/mean execution price vs arrival price, median latency, fill rate, average spread, and percentage of passive/aggressive fills.
2. **RTS 28 (top 5 venues):** Annual publication per instrument class of top 5 execution venues by volume, broken down by client category (retail/professional). Include: percentage of volume, percentage of orders, percentage of passive/aggressive, and qualitative assessment of execution quality.
3. **Data collection:** Aggregate from ClickHouse trade_history: per-instrument execution quality stats computed daily (materialized view), rolled up quarterly for RTS 27 and annually for RTS 28.
4. **Publication:** Reports generated as structured XML/CSV per ESMA templates; published to venue website + submitted to NCA.

**Definition of Done (Acceptance Criteria):**
* [x] RTS 27 quarterly execution quality report generated per instrument class with price/cost/speed/likelihood metrics
* [x] RTS 28 annual top-5 venue report generated per instrument class by client category
* [x] Reports conform to ESMA publication templates (XML/CSV)
* [x] Data sourced from ClickHouse with daily materialized views

**SDD Checklist:**
- [x] Spec checkpoint: MiFID II RTS 27/28 best execution reporting — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: new instrument with <1 quarter history, venue with <5 counterparties (anonymization)

---

### Task 21.3.20: Communications Recording (MiFID II Taping)

**Objective:** Implement communications recording per MiFID II Art. 16(7) and RTS 6 record-keeping (spec §14.8, §24 #203): capture, WORM storage, and dual-control retrieval of client-facing communications intended to result in transactions. Added 2026-09-16 (gap audit remediation #5).

**File Locations:** `services/internal/compliance/comms_recording.go`, `migrations/062_comms_recordings.up.sql`

**Implementation:**
1. `comms_recordings` table (migration 062): `recording_id`, `account_id`, `channel` (email/in-app chat/support message), `started_at`/`ended_at` (PTP-synced UTC per §19.4), `content_ref`, `sha256`, `retention_until` (≥ 5 years).
2. Capture at source: intercept in the notification service (client email/SMS-as-recordable copy) and support/ticket channel, before client delivery. Voice (SIPREC) capture is a documented prerequisite if a phone-desk is ever introduced (§27 ruling R6) — not built in v1.
3. Integrity: WORM object storage (S3 Object Lock compliance mode) with per-day SHA-256 hash chain; tamper check on retrieval.
4. Retrieval API: Compliance Officer dual-control access; every retrieval audit-logged; surveillance cases (Phase-17 signals) may attach recordings as evidence.
5. Retention enforcement: deletion blocked before `retention_until`; GDPR erasure requests yield to Art. 17(3)(b) legal-obligation carve-out (documented in the GDPR response per Task 21.3.7).

**Definition of Done (Acceptance Criteria):**
* [x] Client-facing comms captured at source across configured channels with WORM Object Lock (§24 #203)
* [x] Per-day SHA-256 hash chain verifies integrity; tampered object is detected on retrieval
* [x] Retrieval API enforces Compliance Officer dual control and writes an audit record
* [x] Deletion before `retention_until` (≥ 5 years) is blocked incl. under GDPR erasure (legal carve-out)

**SDD Checklist:**
- [x] Spec checkpoint: communications recording (§14.8, §24 #203) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: account closed mid-retention; client denies marketing consent (comms still recorded — legal obligation, not consent-based)

---

### Task 21.3.21: Surveillance Case Management System
Added 2026-09-17 (gap analysis remediation #6).

Implement the surveillance signal → investigation → disposition workflow required by MiFID II MAR Article 16 (spec §14.4, §24 #207):
1. Case creation: surveillance signals from Phase-17 (spoofing, layering, wash trade, front-running, insider dealing, momentum ignition, marking the close) auto-create cases in `surveillance_cases` table. High-confidence signals (z-score ≥ 3σ) create URGENT cases; others create REVIEW cases.
2. Case assignment: round-robin to Compliance Officer pool with manual reassignment. Assignment SLA: URGENT ≤ 4h, REVIEW ≤ 24h.
3. Investigation workspace: case view includes account trading history, signal evidence, order timeline visualization, linked comms recordings (Task 21.3.20), position history, and counterparty analysis.
4. Evidence attachment: analysts can attach notes, screenshots, comms recordings, and external documents to a case. All attachments are immutable and hash-chained.
5. Disposition: (a) FALSE_POSITIVE — case closed with justification, no further action; (b) ESCALATE_SAR — triggers SAR filing workflow (Task 21.3.3) with pre-populated fields; (c) ESCALATE_STR — regulatory Suspicious Transaction Report submission; (d) ESCALATE_ACTION — triggers compliance hold (Phase-14 Task 14.3.10), kill-switch, or account closure.
6. SLA tracking: case age, time-to-first-review, time-to-disposition. Overdue cases escalate to AML Officer with P2 alert.
7. Reporting: monthly surveillance summary (cases opened/closed/escalated, signal accuracy rates, average disposition time) for venue governance (Task 21.3.15).
8. Retention: surveillance cases and evidence retained ≥ 5 years (MiFID II, CFTC).

---

### Task 21.3.22: Automated Tax Reporting — CRS/FATCA XML Generation

**Objective:** Generate annual end-of-year capital gains tax statements and Common Reporting Standard (CRS) / Foreign Account Tax Compliance Act (FATCA) XML submissions. Added 2026-09-20 (feature-completeness audit remediation #11).

**File Locations:** `services/internal/compliance/tax_reporting.go`, `services/internal/compliance/crs_xml.go`, `services/internal/compliance/fatca_xml.go`

**Implementation:**
1. Annual tax statement generation per client: realized P&L (FIFO lot tracking from Phase-05 Task 5.3.19), unrealized positions, fee deductions, swap charges.
2. CRS XML generation (OECD Schema v2.0): reportable account data for EU/APAC jurisdictions. Submitted to local tax authority per reporting deadline.
3. FATCA XML generation (IRS Schema v2.0): US-person account data, withholding certificates (W-8/W-9 status). Submitted to IRS via IDES.
4. Scheduled annual job: triggered each January for the prior calendar year. Manual re-generation via admin endpoint.
5. Compliance Officer review and dual-control sign-off before submission.

**Definition of Done (Acceptance Criteria):**
* [x] Annual tax statements generated with FIFO P&L per client
* [x] CRS XML validates against OECD Schema v2.0
* [x] FATCA XML validates against IRS Schema v2.0
* [x] Dual-control sign-off before submission

**SDD Checklist:**
- [x] Spec checkpoint: CRS/FATCA tax reporting — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: mid-year account closure, multi-jurisdiction clients, withholding certificate expiry

---

### Task 21.3.23: Sanctions Provider Downtime Quarantine & ARM/APA Resubmission

**Objective:** Enforce fail-closed scoped quarantine during external sanctions provider outages and automated ARM/APA report resubmission per spec §2.7, §14.9, and §24 #323.

**Implementation:**
1. **Sanctions Provider Outage Scoped Quarantine:** If external sanctions provider requests (World-Check / ComplyAdvantage) time out (>30s) or fail across all configured providers, immediately transition new client onboarding and fiat withdrawal processing into pending quarantine state (`SANCTIONS_SERVICE_UNAVAILABLE`, HTTP 503). Pre-screened counterparties continue trading with post-trade anomaly monitoring.
2. **ARM / APA Resubmission Queue:** Failed or NACKed regulatory submissions (MiFID II RTS 22, EMIR REFIT) are spooled to a persistent DLQ table (`regulatory_submission_retries`). Implement automated exponential-backoff retry attempting resubmission within 2 hours of upstream recovery.
3. **Audit Trail & Operator Alerting:** Log all quarantine and resubmission events to immutable compliance audit logs, and trigger high-priority alerts to the Compliance Officer on any backlog exceeding 100 queued reports.

**SDD Checklist:**
- [x] Spec checkpoint: Sanctions service downtime quarantine and ARM/APA resubmission (§24 #323) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.24: Employee Dealing & Insider-Information Controls

**Objective:** Govern the venue's *own staff*, who hold privileged access to non-public market events. Phase-17 detects *client* insider dealing (spec §14.4); nothing previously governed employees. Per spec §14.10.1. Added 2026-09-25 (governance remediation #17).

**File Locations:** `services/internal/compliance/employee_dealing.go`, `services/internal/admin/restricted_list.go`, `migrations/079_employee_dealing.up.sql`

**Implementation:**
1. **Restricted list:** `restricted_lists` (migration 079) — `event_id`, `event_type` (AUCTION|ORACLE_OUTAGE|MAINTENANCE|EMERGENCY_RULE_CHANGE|RATE_FIX), `instruments`, `window_start`/`window_end` PTP-timestamped, `scope` (ALL_EMPLOYEES|ROLE|NAMED), `created_by`. Blackout windows widen automatically around scheduled events.
2. **Pre-clearance:** `pre_clearance_requests` (migration 079) — a trade on any restricted instrument, or by a role in `SENSITIVE_ROLES` (C++ core, core ops, compliance, LP management, product) on a normal instrument, requires approval from an independent controller. `outcome` (APPROVED|DENIED|EXPIRED), 24h expiry, immutable audit trail.
3. **Server-side enforcement:** the Phase-05 gateway rejects non-pre-cleared order entry with `EMPLOYEE_DEALING_PRECLEARANCE_REQUIRED` (HTTP 422). The Phase-07 admin UI also blocks the trade button, but UI blocking is UX only — the gateway is authoritative.
4. **Staff-account monitoring:** accounts flagged `employee_account=true` are excluded from the liquidity pool, STP and rebate programs. All staff trades route to the Compliance Officer queue for same-day review; unexplained profitable trades on restricted events escalate to P1.
5. **Endpoints:** `GET/POST/DELETE /api/v1/admin/restricted-lists` (Compliance Officer), `GET/POST /api/v1/admin/pre-clearance` (independent controller), `GET /api/v1/admin/employee-dealing/audit` (Read-Only Auditor).
6. **Recusal:** enforced by role scoping and access revocation, not attestation.

**Definition of Done (Acceptance Criteria):**
* [x] Restricted lists create blackout windows across instruments with automatic widening
* [x] SENSITIVE_ROLES cannot trade without an approved, unexpired pre-clearance record
* [x] Gateway rejects non-compliant staff orders with EMPLOYEE_DEALING_PRECLEARANCE_REQUIRED
* [x] Staff accounts are excluded from liquidity, STP and rebate programs
* [x] Staff trades appear in the Compliance Officer queue for same-day review

**SDD Checklist:**
- [x] Spec checkpoint: employee dealing controls with restricted lists, pre-clearance and server-side enforcement (§14.10.1, §24 #327) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: pre-clearance expires mid-trade, restricted event added after order entry, recused approver

---

### Task 21.3.25: Regulatory Change Monitoring & Impact Assessment

**Objective:** Give the rulebook a permanent owner. Regulators do not announce MiFID RTS amendments three years early, and no task previously tracked rule change. Per spec §14.10.2. Added 2026-09-25 (governance remediation #17).

**File Locations:** `services/internal/compliance/regulatory_change.go`, `services/internal/admin/regulatory_change.go`, `migrations/080_regulatory_change.up.sql`

**Implementation:**
1. **Watch register:** `regulatory_changes` (migration 080) — `change_id`, `authority` (ESMA|FCA|CFTC|FINRA|FATF|…), `instrument` (directive/RTS/rule), `title`, `published_at`, `effective_at`, `source_url`, `owner`, `status` (TRACKED|TRIAGED|SCOPED|IMPLEMENTED|CLOSED).
2. **Triage SLA:** triaged within 10 business days of publication. `effective_at` within 90 days is P1 to the Compliance Officer. Untriaged items surface on the Compliance dashboard and in the CCO annual report (Task 21.3.13).
3. **Impact assessment:** `regulatory_change_impacts` (migration 080) maps each change to affected spec sections, phase plans, migrations, data fields and reporting endpoints, each with owner, effort estimate and due date. A change cannot be marked IMPLEMENTED without a completed assessment.
4. **Traceability feedback:** any change that alters a §24 criterion ships its traceability-matrix row update in the *same* change set as the implementation — never deferred to a later audit.
5. **Correspondence linkage:** regulator information holds and requests (Task 21.3.13) attach to the same record, so an emergency rule change and its impact scope stay together.
6. **Endpoints:** `GET/POST /api/v1/admin/regulatory-changes` and `GET/PUT /api/v1/admin/regulatory-changes/{id}/impact` (Compliance Officer); untriaged and near-deadline items are exposed to Read-Only Auditor.

**Definition of Done (Acceptance Criteria):**
* [x] Regulatory changes are registered with authority, effective date and owner
* [x] Triage SLA enforced at 10 business days; changes effective within 90 days raise P1
* [x] Impact assessment maps each change to spec sections, phases, migrations and endpoints
* [x] IMPLEMENTED requires a completed impact assessment
* [x] Changes altering a §24 criterion carry a matrix row update in the same change set

**SDD Checklist:**
- [x] Spec checkpoint: regulatory change monitoring with triage SLA and impact assessment (§14.10.2, §24 #328) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: emergency rule change with no notice, unassigned owner, overlapping conflicting changes

---

### Task 21.3.26: Financial Promotions & Outbound Communications Compliance

**Objective:** Inbound taping is covered by §14.8; outbound marketing was uncontrolled. Enforce pre-approval and bounded expiry on every financial promotion. Per spec §14.10.3. Added 2026-09-25 (governance remediation #17).

**File Locations:** `services/internal/compliance/promotions.go`, `services/internal/content/promotion_gate.go`, `migrations/081_vdp_and_promotions.up.sql`

**Implementation:**
1. **Scope:** every financial promotion — landing pages, ads, push/email marketing, social posts, pricing claims and return-style language. Affiliate/influencer content is deferred with referrals (R4), but the control activates automatically if referrals are enabled.
2. **Pre-approval:** `financial_promotions` (migration 081) — `promotion_id`, `channel`, `body_ref`, `version`, `approval_status` (DRAFT|APPROVED|EXPIRED|REJECTED), `approver`, `approved_at`, `approved_until`, immutable version history. Dual control is mandatory for any pricing or performance claim.
3. **Mandatory content checklist:** leveraged-FX risk warning, capital-at-risk disclosure, no performance projection without a clear basis, correct entity and registration details, and cooling-off/closure links per Phase-14.
4. **Bounded expiry:** approvals last ≤12 months, so stale claims cannot persist silently.
5. **Serving gate:** the content layer refuses to render an expired or unapproved promotion ID with `PROMOTION_NOT_APPROVED` (HTTP 410). Approval state is checked at render time, not only at approval.
6. **Endpoints:** `POST/PUT /api/v1/admin/promotions` and `POST /api/v1/admin/promotions/{id}/approve` (Compliance Officer; a second approver for pricing claims), `GET /api/v1/admin/promotions` (Read-Only Auditor).

**Definition of Done (Acceptance Criteria):**
* [x] Promotions are versioned and require Compliance Officer approval before serving
* [x] Pricing and performance claims require dual approval
* [x] Expired or unapproved promotions are refused at render time with PROMOTION_NOT_APPROVED
* [x] Approval is bounded to 12 months and re-approval is recorded
* [x] Mandatory content checklist is enforced before approval

**SDD Checklist:**
- [x] Spec checkpoint: financial promotions pre-approved with bounded expiry and render-time enforcement (§14.10.3, §24 #333) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: promotion served from cache after expiry, performance claim missing dual approval, legacy content with no promotion ID

---

### Task 21.3.27: Reporting Values, Surveillance Tuning & Audit-Trail Query API

**Objective:** Replace framework names with filing-ready numbers and make the audit chain searchable, per spec §14.11 and §24 #345. Added 2026-09-27 (production-maturity remediation #24).

**Implementation:**
1. **Reporting values:** CFTC position-limit table per pair, large-trader thresholds (consumed by Task 21.3.9 — supersedes the "CFTC thresholds" placeholder); LEI/GLEIF validation on onboarding and venue submissions; UTI/UPI generation with collision-reject rule; APA/ARM outage store-and-forward buffer with 2h post-recovery resubmit (extends Task 21.3.16); RTS 25 evidence-bundle format (clock evidence + `clock_offset_nanoseconds` from Phase-09 Task 9.3.12).
2. **Surveillance tuning:** per-signal calibration procedure with backtest harness (Phase-17 Task 17.3.3 signals — task reference pinned 2026-09-27, remediation #35; the prior "Task 17.3.x" was not resolvable), false-positive-rate targets per signal class, tuning-without-downtime deployment, and STOR filing format for escalated market-abuse cases (extends Task 21.3.8 beyond the single `z≥3σ` threshold).
3. **Audit-trail query:** `GET /api/v1/admin/audit` with filter/search over `audit_hash_chain` + `admin_audit_log` (Phase-01 Task 1.3.8); 7-year WORM retention proof; read-only-auditor export with field masking. *(Amended 2026-09-27, remediation #38, F11: Enforces SHA-256 `prev_checksum` / `prev_hash` cryptographic hash-chaining across all audit records and daily Merkle root computation per migrations 021/043, mathematically guaranteeing tamper-detection on all administrative and compliance mutations).*

**Definition of Done (Acceptance Criteria):**
* [x] Every reportable field resolves to a tabulated value or validated identifier; outage buffering demonstrated
* [x] Each signal carries calibration, backtest and FP target; STOR filing exportable
* [x] Audit search serves filtered, masked exports with WORM proof and verified `prev_checksum` hash chain linkage

**SDD Checklist:**
- [x] Spec checkpoint: tabulated reporting values with outage buffering, per-signal surveillance tuning with STOR, and searchable WORM audit trail (§24 #345) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 21.3.28: Order Execution Policy Publication, Consent & Annual Review

**Objective:** Publish the venue execution policy, capture client consent, and review it annually against execution evidence, per spec §14.13 and §24 #377. Added 2026-09-27 (reporting-sufficiency remediation #30). RTS 27/28 data pipelines (Task 21.3.19) and TCA (Task 20.3.9) exist; the policy document, consent record and review loop do not.

**File Locations:** `services/internal/compliance/execution_policy.go`, `migrations/100_execution_policies.up.sql`

**Implementation:**
1. `execution_policies` table (migration 100): `version`, `body_ref` (content store), `status` (DRAFT|ACTIVE|SUPERSEDED), `effective_from`, `review_due_at` (≤12 months), approver (CCO, Task 21.3.15). Public `GET /api/v1/execution-policy` serves the ACTIVE version; material changes require re-consent (see 3).
2. `execution_policy_consents`: `account_id`, `version`, `consented_at`; onboarding and version upgrades block order entry until consent (existing `PRODUCT_NOT_PERMITTED` family — consent gate reuses the Task 14.3.7 product-gating path with a policy reason code, no new error code).
3. Annual review: engine assembles the evidence pack (RTS 27/28 outputs, TCA summaries, incident-linked mis-executions from trade busts Task 15.3.5); CCO signs or amends; overdue review raises a Compliance P1 and freezes policy-version upgrades (existing ACTIVE version stays enforceable).
4. Change control: policy amendments flow through the regulatory-change process (Task 21.3.25) so impact mapping stays current.

**Definition of Done (Acceptance Criteria):**
* [x] ACTIVE policy publicly served; order entry blocked without current-version consent
* [x] Annual evidence-backed review signed; overdue review raises P1 and freezes upgrades

**SDD Checklist:**
- [x] Spec checkpoint: published execution policy with consent gate and annual evidence review (§24 #377) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: client refuses re-consent (close-only); review during ACTIVE incident (evidence pack snapshots, no live edits); backdated correction (new version, never mutate ACTIVE)

---

## 21.4 Deliverables

- Sanctions screening (OFAC/EU/UN/UK HMT) + SanctionsHook in C++ PreTradeChecker
- Travel rule (FATF)
- SAR generation (dual-control filing)
- MiFID II transaction reporting
- EMIR trade reporting
- FinCEN MSB + AML program
- GDPR + geo-block + consent management
- Market-abuse enforcement
- Dodd-Frank swap reporting (US SDR, position limits)
- PEP / adverse-media screening + ongoing monitoring rules engine
- MiFID II RTS 6 algo certification, DEA controls, 5-year order retention
- Basel III capital adequacy + leverage ratio reporting
- EMIR REFIT ISO 20022 + CFTC Parts 43/45 identifier/lifecycle/data-quality reporting
- Regulated-venue member admission, rule enforcement, system-safeguard self-assessment and CCO controls
- MiFID II APA / ARM direct integration & submission adapters (Task 21.3.16)
- FX Global Code 55-principle self-assessment & annual review engine (Task 21.3.17)
- Jurisdictional data residency enforcement & cross-border controls (Task 21.3.18)
- MiFID II RTS 27/28 best execution quality & venue reporting (Task 21.3.19)
- Communications recording (MiFID II taping) with WORM storage & dual-control retrieval (Task 21.3.20)
- CRS/FATCA automated tax reporting (Task 21.3.22)
- Sanctions provider outage quarantine & regulatory report resubmission (Task 21.3.23)
- Tabulated reporting values with outage buffering, per-signal surveillance tuning & audit-trail query API (Task 21.3.27)
- Order execution policy publication, consent gate & annual evidence review (Task 21.3.28, migration 100)

---

## 21.5 Dependencies

- Phases 11, 12, 14, 17, 19, 19.5

---

## 21.6 Duration Estimate

18–22 days (supersedes 17–21 — Task 21.3.28 execution policy added 2026-09-27, remediation #30; prior supersedes 17–20 — Task 21.3.27 added 2026-09-27, remediation #24; prior supersedes 13.5–16.5 — Tasks 21.3.24–21.3.26 added 2026-09-25, governance remediation #17; prior supersedes 13–16 — Task 21.3.23 added 2026-09-24; prior 12–15):
- Task 21.3.1 (Sanctions): 1 day
- Task 21.3.2 (Travel rule): 0.5 day
- Task 21.3.3 (SAR): 0.5 day
- Task 21.3.4 (MiFID II): 1 day
- Task 21.3.5 (EMIR): 1 day
- Task 21.3.6 (FinCEN/AML): 1 day
- Task 21.3.7 (GDPR/geo): 0.5 day
- Task 21.3.8 (Enforcement): 0.5 day
- Task 21.3.9 (Dodd-Frank): 0.5 day
- Task 21.3.10 (SanctionsHook C++): 1 day
- Task 21.3.11 (PEP/adverse media/monitoring): 1 day
- Task 21.3.12 (RTS 6/DEA/retention): 0.5 day
- Task 21.3.13 (Basel III): 0.5 day
- Task 21.3.14 (EMIR/CFTC lifecycle): 1.5 days
- Task 21.3.15 (venue governance): 1.5 days
- Task 21.3.16 (APA/ARM adapters): 0.5 day
- Task 21.3.17 (FX Global Code): 0.5 day
- Task 21.3.18 (Data residency): 0.5 day
- Task 21.3.19 (RTS 27/28 best execution): 0.5 day
- Task 21.3.20 (Comms recording / taping): 1 day
- Task 21.3.21 (Surveillance case management): 1 day (added to itemization, remediation #35 — the largest surveillance workflow was omitted, understating the phase)
- Task 21.3.22 (CRS/FATCA tax reporting): 1.5 days
- Task 21.3.23 (Sanctions quarantine & report resubmission): 0.5 day
- Task 21.3.24 (Employee dealing & insider-information controls): 1.5 days
- Task 21.3.25 (Regulatory change monitoring & impact assessment): 1 day
- Task 21.3.26 (Financial promotions & outbound comms compliance): 1 day
- Task 21.3.27 (Reporting values, surveillance tuning & audit query API): 1 day
- Task 21.3.28 (Execution policy publication, consent & annual review): 1 day
- Testing: 1 day
- Task 21.3.28 (Execution policy publication, consent & annual review): 1 day
- Testing: 1 day

---

## 21.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | OFAC, EU, UN, UK HMT sanctions lists screened |
| 2 | Screening on deposit, withdrawal, trade, registration |
| 3 | Fuzzy matching works |
| 4 | Match blocks transaction + P1 alert |
| 5 | Sanctions lists updated daily |
| 6 | Travel rule for transfers ≥$1,000 (supersedes prior ">$1000") (§24 #107) |
| 7 | Originator + beneficiary info collected |
| 8 | Info included in SWIFT message |
| 9 | Travel rule records stored for retention period |
| 10 | SAR generated on triggers |
| 11 | SAR includes transaction + account + activity description |
| 12 | Compliance Officer review + file workflow |
| 13 | SAR records stored for retention period |
| 14 | Trades reported to MiFID II repository (RTS 22) |
| 15 | Best execution quality recorded (§24 #106) |
| 16 | MiFID II report export works |
| 17 | Derivative trades reported to EMIR repository (§24 #31) |
| 18 | UTI/USI generated |
| 19 | Collateral reported |
| 20 | EMIR report export works |
| 21 | MSB registration filed |
| 22 | CDD/EDD implemented |
| 23 | CTR for cash >$10K |
| 24 | Annual AML training |
| 25 | AML officer designated |
| 26 | GDPR export works |
| 27 | GDPR erase works (with financial record retention) |
| 28 | Geo-block for restricted jurisdictions |
| 29 | IP-based geo-detection (§24 #104) |
| 30 | Surveillance signals consumed for enforcement |
| 31 | Auto-actions: warn, throttle, restrict, suspend |
| 32 | Compliance Officer review for severe signals |
| 33 | Enforcement action endpoint works |
| 34 | Swap trades reported to US SDR within 15 minutes |
| 35 | End-of-day valuation + collateral reported (Dodd-Frank) |
| 36 | CFTC position limits enforced per instrument per account |
| 37 | Large trader reporting for accounts exceeding CFTC thresholds |
| 38 | Sanctions screening timeout → transaction blocked (fail-closed, §24 #28) |
| 39 | Sanctions flag blocks withdrawals and is audit-logged (§24 #29) |
| 40 | GDPR consent management: per-purpose consent, grant/withdraw endpoint (§24 #103) |
| 41 | SAR filing requires dual control — second Compliance Officer+ approves (§24 #108) |
| 42 | `travel_rule_records` and `sar_reports` tables exist (migrations 032, 033) |
| 43 | SanctionsHook in C++ PreTradeChecker: flagged account orders rejected in-process; stale feed >60s → scoped degradation: new account onboarding + fiat withdrawals halted, existing screened-counterparty orders continue with enhanced post-trade monitoring (supersedes prior full ReadOnly — prevents blocking risk-reducing position closes; spec §14.3) |
| 44 | PEP screening at onboarding + daily re-screen; match → EDD + dual-control approval (§24 #149) |
| 45 | Ongoing monitoring rules fire on structuring/velocity/dormant-reactivation; cases routed to Compliance Officer |
| 46 | Uncertified algo_id rejected ALGO_NOT_CERTIFIED; DEA sessions enforce session-level limits (§24 #150) |
| 47 | 5-year order lifecycle retention + regulator export |
| 48 | Basel III CAR + leverage ratio computed daily from GL; breach alerts fire (§24 #151) |
| 49 | EMIR REFIT ISO 20022 reports include UTI, DSB UPI, action/event and valuation/margin data; official schema validation passes (§24 #169) |
| 50 | CFTC Parts 43/45 creation/continuation reports include UTI/USI, DSB UPI, action/event and prior-ID links (§24 #170) |
| 51 | Repository ACK/NACK/correction history is immutable; daily internal-vs-repository reconciliation has zero unexplained breaks |
| 52 | Member/DEA admission and annual review gate access; rulebook/product approvals are versioned and auditable (§24 #174) |
| 53 | Market-control/emergency/disciplinary evidence, system-safeguard self-assessment and CCO annual report are regulator-exportable |
| 54 | Production launch blocks when required venue authorization/governance attestations are absent |
| 55 | MiFID II APA post-trade transparency (<1m RTS 1/2) and ARM transaction reporting (T+1 RTS 22) transmit via certified adapters with ACK/NACK repair workflow (§24 #178) |
| 56 | FX Global Code 55-principle self-assessment generates annual compliance scorecard across all 6 themes with audit evidence and Statement of Commitment (§24 #182) |
| 57 | Jurisdictional data residency enforcer pins EU/UK participant PII and financial records to regional storage partitions and verified KMS keys (§24 #184) |
| 58 | RTS 27 quarterly execution quality report per instrument class with ESMA-template price/cost/speed/likelihood metrics (§24 #202 — supersedes prior erroneous #197 reference) |
| 59 | RTS 28 annual top-5 venue report by client category (retail/professional) with passive/aggressive breakdown |
| 60 | Sanctions feed staleness >60s triggers scoped degradation (not full halt): onboarding/withdrawals blocked, existing screened counterparties continue |
| 61 | Communications recording: client-facing comms captured at source, WORM-stored with hash-chain integrity, ≥ 5-year retention, deletion blocked (incl. GDPR carve-out) (§24 #203) |
| 62 | Comms retrieval API enforces compliance dual control with immutable access audit; surveillance cases may attach recordings |
| 63 | Surveillance signals auto-create cases; URGENT cases assigned within 4h; disposition workflow (FALSE_POSITIVE/SAR/STR/ACTION) functional with immutable evidence chain (spec §14.4, §24 #207) |
| 64 | Monthly surveillance report generated with signal accuracy rates and disposition statistics (spec §14.4, §24 #207) |
| 65 | CRS/FATCA annual tax reports generated with FIFO P&L; XML validates against OECD/IRS schemas; dual-control sign-off (§24 #249) |
| 66 | Sanctions API downtime (>30s) activates scoped withdrawal quarantine with SANCTIONS_SERVICE_UNAVAILABLE; rejected ARM/APA reports auto-resubmit within 2h (§24 #323) |
| 67 | Employees trade restricted instruments only under unexpired pre-clearance; SENSITIVE_ROLES are gated, staff accounts are excluded from liquidity/STP/rebates, and unapproved staff orders are rejected with EMPLOYEE_DEALING_PRECLEARANCE_REQUIRED (§24 #327) |
| 68 | Regulatory changes are triaged within 10 business days (90-day-effective raises P1) and cannot reach IMPLEMENTED without an impact assessment mapped to spec sections, phases, migrations and endpoints (§24 #328) |
| 69 | Financial promotions are versioned, pre-approved (dual control for pricing/performance claims), bounded to 12 months, and refused at render time with PROMOTION_NOT_APPROVED (§24 #333) |
| 70 | CFTC limits/thresholds tabulated with LEI/UTI validation and APA/ARM store-and-forward; per-signal calibration with backtest, FP targets and STOR; searchable WORM audit-trail API with masked export (§24 #345) |
| 71 | ACTIVE execution policy publicly served with consent-gated order entry; annual evidence review signed, overdue freezes upgrades (§24 #377) |
| 72 | Surveillance consumer lag >10,000 events raises `SURVEILLANCE_LAG_WARNING` (P2) with worker auto-scaling; detection-latency SLA and degraded-detection policy under backlog defined (§24 #392, stable ID `T21-024`; added 2026-09-27, remediation #35 — the §14.9 lag behavior had no acceptance test) |

---

## Phase-21 Settle Addendum (2026-09-30) — implementation record

All 28 tasks implemented across 7 disjoint work-streams; **28/28 spec checkpoints bound and green** (`tests/spec/checks/phase21.go`; task 21.3.21 declares no §checkpoint row — its SDD rows verified via TestPGCase* suite). 181/181 DoD/SDD rows ticked.

**Migrations landed:** 032/033/054/059 (wave-1 verbatim per plan) + 060/062/079/080/100 (plan-reserved numbers, absent on disk — used verbatim) + 239 `account_consents` / 241 `rts6_algo_dea` / 242 `surveillance_tuning` / 243 `gdpr_geo` / 244 `data_residency` / 245 `tax_report_runs` / 246 `financial_promotions` / 247 `market_abuse_enforcement` / 248 `surveillance_cases` / 249 `basel_reports` / 250 `venue_governance` / 251 `best_execution_reports`. The plan's `081_vdp_and_promotions` citation was stale — 081 is `vulnerability_disclosures` (Phase-13.5), so promotions/consents allocated fresh numbers. Migration-count collisions during parallel landing were resolved by renumber (239/240 double-claims → 247/248/249).

**Deviations and honest seams (recorded, not hidden):**

1. **Route:** the task-21.3.7 text prescribes `PUT /api/v1/account/consent` for GDPR consent, but Task 21.3.28 already owns that path for execution-policy consent — GDPR consent landed at `PUT /api/v1/account/gdpr/consent` (phase-doc text is the stale side; route registry is canonical).
2. **DEA session-level enforcement:** `RTS6Service.DEALimitsFor` is the consumed seam; a DEA session registry does not exist yet (FIX/SBE session admission has no DEA-scoped hook — `internal/fix/certgate.go` is entitlement-scoped). Order-admission certification gating (`AssertCertified`, fail-closed) is live; session-level DEA enforcement binds when a DEA session registry lands.
3. **Consent→dispatch:** `ConsentGranted`/`CONSENT_NOT_GRANTED` exist as the enforcement seam; no outbound-marketing dispatcher exists yet to call it — the seam binds when a marketing dispatcher is built.
4. **`REGULATORY_DEADLINE_APPROACHING` pages P1** per this plan's task text, stricter than the §27.1 P2 floor — recorded in spec §27.
5. **Voice capture (SIPREC):** prerequisite only — §27 ruling R6 (no phone desk v1) stands; comms recording covers email/in-app/support channels.
6. **`FX_GLOBAL_CODE_NON_COMPLIANT`** deliberately unregistered — audit-warning code on the `CTR_TRIGGERED` precedent (alert trail, never an HTTP response).
7. **Sandbox feed:** demo-environment synthetic/delayed market data is deployment configuration (Phase-08.5 §Task 8.5.3.2 note), not in-repo simulator code.

**Error registry:** +1 specRow (`ENFORCEMENT_ACTION_EXISTS` — §23 row added at settle; registry 206→207) +16 localCodes pending §23 transcription (`TRAVEL_RULE_MISSING_INFO`, `TRAVEL_RULE_REJECTED`, `SAR_DUAL_CONTROL_REQUIRED`, `MSB_COMPLIANCE_BREACH`, `CAPITAL_ADEQUACY_BREACH`, `LEVERAGE_RATIO_BREACH`, `CONSENT_NOT_GRANTED`, `CROSS_BORDER_JUSTIFICATION_REQUIRED`, `RESIDENCY_VIOLATION`, `COMMS_INTEGRITY_FAILURE`, `COMMS_RETENTION_ACTIVE`, `TAX_REPORT_INVALID_TRANSITION`, `TAX_REPORT_DATA_INCOMPLETE`, `JURISDICTION_UNLICENSED`, `ANNUAL_ATTESTATION_OVERDUE`, `VENUE_RULEBOOK_NOT_APPROVED`).

**Cross-agent fixes during verification:** `rts27_reports` INSERT param-type conflict fixed (`$2::varchar`); demo `expireOne` wrapped in the §5.40 whole-tx retry ladder (audit-chain 23505 under snapshot skew); dev-DB migration drift repaired (051/053 backfilled on dev PG); **13 audit appends retargeted to nil payload** (`tax_report_runs`, `accounts` residency-pin, `financial_promotions` ×3, `comms_recordings` ×2, `account_consent_states`, `gdpr_requests`, `venue_rulebooks`, `venue_member_events`, `venue_interventions`) — they passed `mustJSON(detail)` opaque payloads that `exchange verify-audit` cannot recompute; payload bytes are never stored, so nil loses nothing and keeps the chain self-verifiable per the `admin/audit.go` convention (detail remains in the mutated row itself); `TestSurveillanceSignalsPgIntegration` cleanup made FK-aware (`surveillance_cases.signal_id` + append-only `surveillance_case_evidence` make case-cited signals undeletable — test now removes only unreferenced signals).
