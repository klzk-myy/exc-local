# Phase 10.5 — Console & Surface Completion (Backoffice + User-Side Coverage)

**Duration:** 30–38 days (34 nominal; single FE engineer — 2 engineers ≈ 18–20 days). Sized against 27 tasks covering ~375 uncovered admin routes + ~136 uncovered non-admin routes (route-registry↔frontend diff, 2026-10-04 audit).
**Dependencies:** Phase 10 complete (UI baseline, input-helper framework Task 10.3.29, RBAC gates Task 10.3.6/10.3.20, RequireAuth/RequireRole Task 10.3.21). Backend routes already mounted — this phase is **surface completion only**; no new backend endpoints except the two seams called out in Tasks 10.5.3.26–27.
**Spec Reference:** §21 (Trader UI), §7.5 (admin consoles), §19.16 (ops), §8.2 (RBAC), §24 acceptance matrix.

**Provenance:** Added 2026-10-05 (functional-completeness crawl audit + route-registry diff — remediation #46). The crawl verified every mounted route answers; the diff showed the UI consumes ~300 of ~646 mounted routes. All tasks below are **UI/console work against already-live endpoints** — do not reimplement backend handlers. Where a task extends an existing Phase-10 surface it says so explicitly (extends Task 10.3.NN) and that task's scope is amended in place, not duplicated.

---

## 10.5.1 Objectives

Close the route-coverage gap: every operationally meaningful mounted route gets a reachable, RBAC-gated, honest-empty/honest-error UI surface. Deliver the three management domains requested by product — **customer management (客户管理), account management (账户管理), funds & asset operations (资金资产管理)** — plus the full admin backoffice (compliance desk, funding ops, settlement ops, venue governance, reg reporting, ops safety controls) and the remaining user-side features (strategies marketplace, advanced order composition, analytics, history/export center, public transparency pages).

Non-goals: new backend business logic (existing routes only); the `/api/v1/test/*` dev-only endpoints stay headless by design; i18n remains out of scope (R5).

---

## 10.5.2 Prerequisites

- Phase 10 complete (scaffold, input helpers, RBAC gates, WS client, bundle budget).
- The `frontend/src/lib/input-helpers/generated/route-contracts.ts` catalog is regenerated before each batch so panels bind to current OpenAPI contracts.
- Admin-role test fixtures (`RequireAdmin` + role badge) and the `signInForTests` helper from the remediation suite.

---

## 10.5.3 Tasks

### Batch A — Operational safety & control plane (P0)

### Task 10.5.3.1: Ops Safety Console

**Objective:** Single control surface for every fail-safe lever — kill switch, circuit breaker, feature flags, IP bans/allowlist, maintenance windows, mass cancel, manual liquidation.

**File Locations:** `frontend/src/features/admin-ops/` (`routes.ts`, `nav.ts`, `OpsSafetyPage.tsx`, panels). ✅ Implemented 2026-10 — route `admin/ops-safety`.

**Implementation:**
1. Kill-switch board — `GET/POST /api/v1/admin/kill-switch`, `POST …/reset`; shows active suspensions (`trading_suspensions`-backed state) with scope (global/instrument/account-class) and dual-control badge.
2. Circuit-breaker control — `POST /api/v1/admin/circuit-breaker/{tier}`, `POST …/{tier}/reset`; five-tier ladder state display (Phase-13 backend).
3. Feature flags CRUD — `GET/POST/PUT/DELETE /api/v1/admin/flags[/{id}]`, per-flag audience/status view.
4. IP security — `GET/PUT/DELETE /api/v1/admin/ip-bans[/{id}]` + `GET …/ip-bans/audit` + `PUT/DELETE /api/v1/admin/ip-allowlist/{id}`; progressive-ban ladder display (418 semantics, Task 5.3.34).
5. Maintenance windows — `GET/POST/PATCH/DELETE /api/v1/admin/maintenance-windows[/{id}]` with countdown banners surfaced to the public status surface (Task 10.5.3.23).
6. Destructive trade ops — `POST /api/v1/admin/orders/mass-cancel`, `POST /api/v1/admin/liquidation/manual`, `POST /api/v1/admin/cache/warm`, `PUT /api/v1/admin/fix-sessions/{id}` — all behind typed confirm modals + dual-control; every action echoes its required role and audit-trail write before submission.
7. All actions land in the admin audit log; every panel renders honest-empty when the backend list is empty.

**DoD (Acceptance Criteria):**
* [x] Kill switch, CB tiers, flags, IP bans/allowlist, maintenance windows operable with dual-control UX
* [x] Mass-cancel/manual-liquidation/fix-session/cache-warm reachable only with required role + typed confirmation
* [x] Audit-log row observable after each action (server-side audit write; surfaced via the Task 10.3.6 AuditLogPanel)

**SDD Checklist:**
- [x] Spec checkpoint: all safety levers reachable, dual-controlled, audit-written — defined first, validated against spec

---

### Task 10.5.3.2: Fleet & Release Actions (extends Task 10.3.20)

**Objective:** Complete the fleet pages with the mutation actions the backend already exposes.

**Implementation:**
1. Host actions — `POST /api/v1/admin/fleet/hosts/{id}/{cordon,decommission,drain}` wired into the fleet grid rows with state preview.
2. Release pipeline actions — promote/rollback buttons calling the existing `POST /api/v1/admin/releases/{id}/promote` family with gate-evidence display.
3. Env-guard carried over unchanged (Task 10.3.20): staging context never calls prod.

**DoD:**
* [x] Host cordon/drain/decommission + release promote callable from UI with dual-control + env guard (host actions pre-existed Task 10.3.20; this task added lifecycle state preview, release registration, and verbatim §19.16.3 gate-evidence rendering on both EXECUTED and FORBIDDEN/BLOCKED paths)

**SDD Checklist:**
- [x] Spec checkpoint: fleet mutations reach backend through context-bound client — defined first, validated against spec

---

### Task 10.5.3.3: Audit, Reconciliation & Archive Console

**Objective:** Read/verify surface for the immutable audit chain, recon runs, order-record export, DLQ and API-deprecation telemetry.

**Implementation:**
1. Audit explorer — `GET /api/v1/admin/audit[/{trail}]`, `GET …/audit/chain`, `GET …/audit/verify` — chain-verification badge per run.
2. Recon runs — `GET /api/v1/admin/reconciliation/runs` + `…/latest`; findings table with mismatch/inconclusive/dead-letter breakdown (migration-286 semantics).
3. Records & archive — `GET /api/v1/admin/order-records/{id}/export`, `GET /api/v1/admin/archive/status`.
4. DLQ view — `GET /api/v1/admin/dlq` (live post-`natsctl dlq init`; render honest-degraded on 503).
5. API deprecation dashboard — `GET/POST /api/v1/admin/api-deprecations`, `GET …/usage` (usage sparkline per deprecated route).

**Implemented:** `frontend/src/features/admin-integrity/` — `IntegrityPage.tsx` (route `/admin/integrity`), `AuditIntegrityPanel.tsx`, `ReconPanel.tsx`, `ArchiveDlqPanel.tsx` (records/archive + DLQ), `DeprecationPanel.tsx`, `api.ts`, `routes.ts`, `nav.ts`, `IntegrityPage.test.tsx`.

**DoD:**
* [x] Audit chain verify + recon runs + order-record export + DLQ + deprecation usage all render live data

**SDD Checklist:**
- [x] Spec checkpoint: audit/recon/DLQ surfaces show authoritative state with honest-degraded fallbacks — defined first, validated against spec

---

### Batch B — Customer management (客户管理)

### Task 10.5.3.4: Customer 360 & Account Lifecycle Console

**Objective:** The per-customer operations home — aggregated profile/KYC/holdings/activity view plus the full lifecycle action set.

**File Locations:** `frontend/src/features/admin-crm/` (`Customer360Page.tsx`, `LifecyclePanel.tsx`); promoted from `admin/UserLookupPanel`.

**Implementation:**
1. Customer 360 — keyed by account id/email search (reuses `UserLookupPanel` query): profile, KYC tier + submission history, balances/positions snapshot, recent orders, open support tickets, active holds/flags, audit excerpt.
2. Lifecycle actions — `POST /api/v1/admin/accounts/{id}/{freeze,unfreeze,close,jurisdiction}`, `PUT …/sub-account-limit` — each shows precondition checks (open positions, pending withdrawals) before submit; all dual-control + audit-written.
3. Support desk integration — `GET /api/v1/admin/support/tickets/{id}`, `POST …/notes`, `GET …/complaints/register`; agent reply/notes panel.
4. KYC desk completion — pending queue (exists) + self-certification review surface wired into the admin KYC card (backend seam added: `GET /api/v1/admin/accounts/{id}/self-certifications` — the client route is user-scoped; audit-logged officer read, Compliance Officer|Support Agent|Super Admin).

**Implemented:** `frontend/src/features/admin-crm/` — `Customer360Page.tsx` (route `/admin/customers`), `DossierPanel.tsx`, `LifecyclePanel.tsx`, `SupportDeskPanel.tsx`, `KycDeskPanel.tsx`, `api.ts`, `routes.ts`, `nav.ts`, `Customer360Page.test.tsx`; backend seam `api.AdminSelfCertList` mounted + registered (routes_v1.go, openapi regenerated — 663 operations).

**DoD:**
* [x] Search → 360 view renders profile/KYC/holdings/activity; freeze/unfreeze/close/jurisdiction/sub-account-limit all functional with preconditions + dual control
* [x] Support notes + complaints register visible per customer

**SDD Checklist:**
- [x] Spec checkpoint: customer lifecycle is fully operable from one surface — defined first, validated against spec

---

### Task 10.5.3.5: Client Compliance Ops Console

**Objective:** Day-to-day compliance actions on customers — screening, travel-rule cures, restricted lists, employee dealing, pre-clearance, enforcement.

**Implementation:**
1. Screening — `POST /api/v1/admin/compliance/screening/accounts/{id}` + `…/screening/adverse-media`; results inline with provider/fail-closed state.
2. Travel rule — `GET /api/v1/admin/travel-rule`, `GET …/{id}`, `POST …/{id}/supply` (MISSING_INFO cure queue).
3. Restricted lists — `GET/POST/DELETE /api/v1/admin/restricted-lists` (member/instrument/counterparty scopes).
4. Employee dealing — `GET /api/v1/admin/employee-dealing/audit` + `GET/POST /api/v1/admin/pre-clearance` (request queue + decisions).
5. Enforcement — `GET /api/v1/admin/enforcement`, `POST …/{id}` — WARN→SUSPEND ladder with order-gate effect preview.

**DoD:**
* [x] Screening, travel-rule cure, restricted-list edit, pre-clearance decide, enforcement ladder all functional

**SDD Checklist:**
- [x] Spec checkpoint: client compliance ops executable with evidence capture — defined first, validated against spec

**Execution record (2026-10-04):** `frontend/src/features/admin-compliance/` — route `/admin/compliance`. Screening: `POST screening/accounts/{id}` outcome card (CLEAN/REVIEW/SANCTIONS HIT/QUARANTINED — never shows a deferred screen as clean) + provider-gate strip from `GET sanctions/status` + adverse-media intake. Travel rule: `?status=` filter defaulting to MISSING_INFO; row select → supply form posting `{originator,beneficiary}` parties to `/{id}/supply`. Restricted lists: create (ALL_STAFF/ROLE/NAMED_ACCOUNTS scopes, RFC3339 window) + retire via `DELETE ?id=` behind ConfirmAction. Employee dealing: `?outcome=` queue, decide posts `{id,approve,note}`, officer-file form, dealing audit tail. Enforcement: `?account_id=` ledger, WARN→THROTTLE→RESTRICT→SUSPEND ladder strip, per-rung order-gate effect preview, `POST /{signal_id}` with action/note/ttl_seconds. Note: restricted-lists DELETE is `?id=` query-form (not `/{id}`); `GET /admin/sanctions/status` added for the fail-closed state display (already mounted). 7 component tests; 647/647 suite green.

---

### Task 10.5.3.6: Surveillance, SAR & AML Case Desk

**Objective:** The Phase-17→21 signal-to-case pipeline UI — alert triage, case management, SAR lifecycle, AML program artifacts, sanctions ops, comms retrieval.

**Implementation:**
1. Surveillance — `GET /api/v1/admin/surveillance/{summary,cases}`, `GET /api/v1/admin/surveillance/cases/{id}`, `POST …/cases/{id}/{assign,disposition,evidence}` — case SLA timers + immutable evidence upload.
2. SAR — `GET/POST /api/v1/admin/sar`, `GET …/{id}`, `POST …/{id}/{approve|reject|file}` — DRAFT→UNDER_REVIEW→APPROVED→FILED four-eyes flow; `GET /api/v1/admin/ctr` report view.
3. AML program — `GET /api/v1/admin/aml/{program,monitoring,artifacts}` + `POST …/artifacts`.
4. Sanctions ops — `GET /api/v1/admin/sanctions/status`, `POST …/refresh`, `POST …/queue/replay` (provider-down replay queue).
5. Comms recording (MiFID II taping) — `GET /api/v1/admin/comms-recordings[/{id}]`, `POST …/verify-day`, `POST …/{id}/retrieve` (dual-controlled retrieval with WORM hash display).

**DoD:**
* [x] Signal→case→disposition→SAR file path walkable end-to-end in UI; comms retrieval is dual-controlled

**SDD Checklist:**
- [x] Spec checkpoint: surveillance case desk + SAR lifecycle + comms WORM retrieval — defined first, validated against spec

**Execution record (2026-10-04):** `frontend/src/features/admin-surveillance/` — route `/admin/surveillance`. Surveillance: monthly summary strip (`?month=`) + case queue (`?status=`) → workspace (`GET cases/{id}`) with immutable evidence list (kind/body/sha256), linked signals, order-audit refs, assign (`{assignee}`, 0=round-robin), evidence attach, terminal disposition (FALSE_POSITIVE/ESCALATE_SAR/ESCALATE_STR/ESCALATE_ACTION — action verb only shown for ESCALATE_ACTION). SAR: `?status=` queue → per-status actions (review/approve {note}, file {filing_ref}, reject {reason}) — four-eyes enforced server-side; manual draft form (source_ref dedup, 200-vs-201 both surfaced); CTR register below. AML: program compliant/breach badges (MSB_COMPLIANCE_BREACH code shown), artifact register + filing form (6 types), monitoring feed (`?account_id=`). Sanctions ops: shared `fetchSanctionsStatus` from admin-compliance + refresh + queue/replay. Comms: `?account_id=` register with sha256/chain-hash display, `verify-day` → INTACT/VIOLATION, retrieval posts `{approver_id, justification, case_ref}` — approver mandatory in UI, distinctness enforced server-side. 5 component tests; 652/652 suite green.

---

### Batch C — Funds & asset operations (资金资产管理)

### Task 10.5.3.7: Funding Operations Queues

**Objective:** Admin work queues for every money-movement review step.

**Implementation:**
1. Deposit ops — `POST /api/v1/admin/funding/deposits`, `POST …/deposits/{id}/{review|confirm}`, `POST …/inbound-wires`, `POST …/returns`; tiered anti-fraud badges (<$10K auto / $10K–50K / >$50K PENDING_REVIEW+4h).
2. Withdrawal approvals — `POST /api/v1/admin/withdrawals/{id}/{approve,reject}` with whitelist/beneficiary/velocity context per request.
3. Quarantine — `GET /api/v1/admin/funding/quarantine`, `POST …/{id}/resolve` (DepositGuard suspense routing surfaced).
4. Bank accounts — `GET /api/v1/admin/funding/bank-accounts`, `POST …/{id}/{verify|reject}`.
5. Funding alerts rail — `GET /api/v1/admin/funding/ops-alerts`.

**DoD:**
* [x] Deposit review/confirm, wire registration, returns, withdrawal approve/reject, quarantine resolve, bank-account verify all operable
* [x] Threshold badges + dual-control on every mutation

**SDD Checklist:**
- [x] Spec checkpoint: funding ops queues enforce review tiers and dual control — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-funding/` — `FundingOpsPage` (`/admin/funding-ops`) + `api.ts` + 5 panels: `OpsAlertsPanel` (durable alert rail — the queue-discovery feed, since the contract mounts no deposit/withdrawal list GET), `DepositOpsPanel` (ingest w/ idempotency, two-source confirm, four-eyes review, inbound-wire registration w/ 202=quarantined disposition, rail returns; tier legend < $10K auto / $10K–50K / >$50K PENDING_REVIEW+4h), `WithdrawalPanel` (id-driven approve/reject four-eyes), `QuarantinePanel` (suspense ledger w/ GL account, name-match score, SLA; RELEASE_TO_CLIENT/RETURN_TO_SOURCE four-eyes), `BankAccountsPanel` (verify {approver_id,method} four-eyes / reject {reason}). Zero backend changes — all endpoints already mounted. 7 component tests; 659/659 suite green; route-completeness green.

---

### Task 10.5.3.8: Treasury, Nostro & Client-Money Console

**Objective:** House-money and segregated-funds operations.

**Implementation:**
1. Nostro — `GET/POST /api/v1/admin/funding/nostro`, `GET/POST …/replenishments[/{id}/decide]`, `GET/POST /api/v1/admin/nostro-accounts`, `GET /api/v1/admin/swift-messages`.
2. Nostro reconciliation — `GET /api/v1/admin/nostro-reconciliation`, `POST …/run`, `POST …/breaks/{id}/resolve`; `GET /api/v1/admin/pb-reconciliation`.
3. Client money — `GET/POST /api/v1/admin/client-money/{audits,certifications}`, `POST …/audits/{id}/evidence-pack`; segregation status + shortfall waterfall position.
4. Treasury — `GET/POST /api/v1/admin/treasury/{own-funds,contingent-capital}`; `GET /api/v1/admin/insurance-fund`; `PUT /api/v1/admin/collateral-schedule`.

**DoD:**
* [x] Nostro registry/recon/replenishment + client-money audit/certification + treasury views functional

**SDD Checklist:**
- [x] Spec checkpoint: segregated-funds state is inspectable and auditable — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-treasury/` — `TreasuryPage` (`/admin/treasury`) + `api.ts` + 4 panels: `NostroPanel` (per-currency coverage w/ deficit flags, NOSTRO/VOSTRO registry, replenishment request→four-eyes decide, immutable SWIFT journal), `ReconPanel` (nostro report/rerun/break INVESTIGATE|RESOLVE + PB give-up recon w/ auto-match rate), `ClientMoneyPanel` (engagement register → system-assembled evidence pack sha256 → four-eyes certification), `TreasuryPanel` (own-funds ledger + freeze flags, contingent-capital waterfall, insurance fund balances/txns, collateral schedule PUT). Contract note: PBReconRun/Break marshal PascalCase (no json tags) — parser dual-cases. Zero backend changes. 6 component tests; 665/665 suite green.

---

### Task 10.5.3.9: Settlement & Allocation Ops Console

**Objective:** Post-trade operations — CLS lifecycle, exceptions, confirmations, allocations, chargebacks.

**Implementation:**
1. CLS PvP — `POST /api/v1/admin/settlement/cls/instructions[/{id}/{amend|dispatch|finality|pay-in|rescind}]` (24-route family) with instruction state machine display.
2. Exceptions — `GET /api/v1/admin/settlement-exceptions/{id}`, `POST …/{id}/resolve` (retry/reverse/write-off per authority matrix).
3. Confirmations — `POST /api/v1/admin/settlement-confirmations`.
4. Allocations — `GET/POST /api/v1/admin/allocations/groups`, `GET /api/v1/admin/allocations/groups/{id}{/allocate,/eligibility,/fills}` + `POST …/escalate` (T+0 escalation incl. LOCKED groups).
5. Chargebacks — `GET/POST /api/v1/admin/chargebacks[/{id}]`, `POST …/{id}/{submit|resolve}`.

**DoD:**
* [x] CLS instruction lifecycle, exception resolution, allocation groups + escalation, chargeback handling all operable

**SDD Checklist:**
- [x] Spec checkpoint: post-trade ops surfaces expose the full state machines — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-settlement/` — `SettlementPage` (`/admin/settlement`) + `api.ts` + 4 panels: `ClsPanel` (paired-instruction submit + ref-driven dispatch/amend/rescind/pay-in/finality/status console w/ lifecycle strip; SETTLED only via authenticated /finality leg), `ExceptionsPanel` (id-driven detail + RETRY|REVERSE|MANUAL resolve → 202 four-eyes PENDING rendered honestly + MT900/910 intake), `AllocationsPanel` (register/eligibility/fills/allocate/submit-lock/leg claim-reject-cancel-correct/T+0 escalate), `ChargebacksPanel` (open under approver_user_id four-eyes + freeze flag, submit, WON|LOST resolve). Contract note: ClsInstruction/SettlementException marshal PascalCase — parser dual-cases. Zero backend changes. 6 component tests; 671/671 suite green.

---

### Task 10.5.3.10: Finance, Fees & Tax-Reporting Admin

**Objective:** Pricing and house-finance administration.

**Implementation:**
1. Funding fees — `GET/POST/PUT/DELETE /api/v1/admin/funding/fees[/{id}[/versions]]`.
2. Promo fees — `GET /api/v1/admin/fees/promos`, `POST …/promo`, `POST …/promo/{id}/{approve|reject}`.
3. Finance — `GET /api/v1/admin/finance/{trial-balance,pnl,balance-sheet}`, `GET /api/v1/admin/invoices`, `GET/POST /api/v1/admin/reporting-values`.
4. Tax reporting — `GET/POST /api/v1/admin/tax-reporting/runs[/{id}]`, `POST …/runs/{id}/{review|approve|reject}` (CRS/FATCA golden-XML runs).

**DoD:**
* [x] Fee schedules + promos + tax runs + finance statements render and mutate correctly

**SDD Checklist:**
- [x] Spec checkpoint: pricing/finance admin complete with approval flows — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-finance/` — `FinancePage` (`/admin/finance`) + `api.ts` + 4 panels: `FeeSchedulesPanel` (rail/all filter, create v1, PUT successor insert — supersedes chain preserved, DELETE retire, per-row version-chain drill-down), `PromosPanel` (window list + create → PENDING_APPROVAL rendered honestly, four-eyes approve/reject — distinct approver enforced claims-side), `FinancePanel` (trial-balance JSON w/ date+currency filters, P&L/balance-sheet CSV export links, invoice register w/ account+month filter, reporting-values list + Compliance-officer upsert), `TaxReportingPanel` (CRS/FATCA runs + generate, DRAFT→UNDER_REVIEW→APPROVED→SUBMITTED lifecycle strip, reject w/ reason, submit w/ submission_ref, integrity-checked /xml artifact link). Full lifecycle incl. XML + submit covered beyond plan's review|approve|reject shorthand. Zero backend changes. 6 component tests; 677/677 suite green.

---

### Batch D — Market & instrument governance

### Task 10.5.3.11: Instrument Lifecycle Console

**Objective:** Full instrument state machine + listing governance + market schedule.

**Implementation:**
1. Instrument ops — `POST /api/v1/admin/instruments/{id}/{halt,cancel-only,activate,delist}` (extends `InstrumentsPanel`), `GET/PUT …/auction-calendar`, `GET /api/v1/admin/listing-proposals` + `POST …[/{id}/review]`.
2. Market schedule — `GET /api/v1/admin/market-schedule[/overrides]`, `POST/PUT/DELETE …/overrides[/{id}]` — 24/5 calendar + holiday overrides.
3. Margin-param changes — `POST /api/v1/admin/margin-param-changes` (`MARGIN_MODEL_UNVALIDATED` gate surfaced); `GET/POST /api/v1/admin/entity-leverage-policy`.

**DoD:**
* [x] Instrument lifecycle transitions, listing proposals, schedule overrides, margin/leverage policy changes all operable

**SDD Checklist:**
- [x] Spec checkpoint: instrument lifecycle console covers halt→delist with dual control — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-instruments/` — `InstrumentGovernancePage` (`/admin/instrument-governance`) + `api.ts` + 3 panels: `ListingPanel` (status-filtered queue, auto_checks rendered, review REVIEW|APPROVE|REJECT — APPROVE → 202 four-eyes rendered pending, intake form w/ reference+risk-defaults JSON), `SchedulePanel` (market-schedule doc w/ version, override create/edit/delete, per-symbol auction-calendar GET + full-replace PUT → 202 pending), `RiskPolicyPanel` (margin-param-changes → 202 w/ §13.12 MARGIN_MODEL_UNVALIDATED approval-time gate note, entity-leverage matrix + cell → 202). Contract note: the instrument list + lifecycle verbs themselves already live on the Admin page (`features/admin/InstrumentsPanel`, Task 15.3.2 — activate/restrict/cancel-only/suspend/halt single-sign + resume/delist dual) — not duplicated; uncross-override is mounted for crossed-book quarantine release but its trigger state is engine-side, so the surface stays out of this console per plan verb list. Zero backend changes. 6 component tests; 683/683 suite green.

---

### Task 10.5.3.12: MM Program, DEA & Algo Governance Console

**Objective:** RTS 6 / DEA / market-maker program administration.

**Implementation:**
1. MM programs — `GET/POST/PUT /api/v1/admin/mm-programs[/{id}]`, `GET …/{id}/compliance`, `POST …/rebates/post`, MMP state display.
2. DEA — `GET/POST /api/v1/admin/dea/controls`, `POST …/controls/{id}/suspend`.
3. Algo certification — `GET/POST /api/v1/admin/algo-certifications`, `POST …/{id}/transition`; RTS 6 self-assessments `GET/POST /api/v1/admin/rts6/self-assessments`.

**DoD:**
* [x] MM program CRUD/compliance/rebates, DEA controls, algo-cert transitions, RTS6 self-assessment all functional

**SDD Checklist:**
- [x] Spec checkpoint: algo-trading governance surfaces complete — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-marketmaking/` — `MarketMakingPage` (`/admin/market-making`) + `api.ts` + 2 panels: `MMProgramsPanel` (account/status-filtered register, enroll/update form w/ full quota-term contract, suspend/resume + §24 #139 MMP reset, per-program compliance sampling + rebate accruals, monthly GL sweep reporting partial_error honestly), `AlgoDeaPanel` (cert register w/ status filter + kill-button intake + CERTIFIED|SUSPENDED|REVOKED transitions — the only legal targets server-side; DEA per-session read/upsert/suspend; RTS6 self-assessment register + filing). Zero backend changes. 6 component tests; 689/689 suite green.

---

### Batch E — Regulatory reporting & conduct

### Task 10.5.3.13: Regulatory Reporting Desk

**Objective:** Submissions lifecycle + RTS 27/28 production + regime reports.

**Implementation:**
1. Submission queue — `GET /api/v1/admin/regreporting/{queue,events,submissions}`, `GET /api/v1/admin/regreporting/events/{id}`, `POST /api/v1/admin/regreporting/{acks,party-identifiers}` + `POST …/breaks/{id}/resolve`; ACK/NACK + repair-resubmit actions.
2. Best execution — `GET/POST /api/v1/admin/bestexec/{rts27,rts28}`, `POST /api/v1/admin/bestexec/{rts27/generate,rts27/materialize,rts28/generate}` — generate→materialize→publish pipeline; `GET /api/v1/admin/{emir-report,mifid-report,basel-report,compliance-report}`.

**DoD:**
* [x] Submission queue, breaks resolution, RTS27/28 generation and regime reports all reachable

**SDD Checklist:**
- [x] Spec checkpoint: reg-reporting desk operates the submissions lifecycle — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-regreport/` — `RegReportingPage` (`/admin/regreporting`) + `api.ts` + 3 panels: `SubmissionsPanel` (merged repair queue — open breaks + NACKED/FAILED transport rows; RESOLVED|WONT_FIX dispositions; submission register + corrected resubmit creating a NEW row while the original stays on record — no silent in-place repair; async ACK/NACK ingest; party-identifier upsert; manual reconcile sweep), `BestExecPanel` (RTS27 materialize-day → generate-quarter → publish pipeline; RTS28 generate-year → publish; DRAFT/PUBLISHED lifecycle badges; zero-activity quarters marked), `RegimePanel` (EMIR-pinned canonical event export via the legacy `/admin/emir-report` mount; Basel III report by period; generic compliance export across MIFID2|EMIR|FINCEN_CTR|FINCEN_SAR|BASEL3|MONTHLY_SUMMARY). `GET /admin/mifid-report`'s combined RTS27+RTS28 view is covered by the dedicated list endpoints (same service seam, strictly richer). Zero backend changes. 6 component tests; 695/695 suite green.

---

### Task 10.5.3.14: Regulated-Venue Governance Console

**Objective:** The 44-route venue governance surface (Phase-21 Task 21.3.15 backend).

**Implementation:**
1. Venue cases — `GET/POST /api/v1/admin/venue/cases[/{id}]`, `POST …/{id}/{evidence,transition}`.
2. Rulebook/CCO reports/member admission — the remaining `admin/venue/*` surfaces (rulebook versions, annual CCO report, licensing, member admission review).
3. Largest single uncovered cluster — split into sub-panels; shares case-desk components with Task 10.5.3.6.

**DoD:**
* [x] All `admin/venue/*` routes reachable through the console; no dead panels

**SDD Checklist:**
- [x] Spec checkpoint: venue governance console covers cases/rulebook/admission/CCO reporting — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-venue/` — `VenuePage` (`/admin/venue`) + `api.ts` + 4 panels covering all 44 mounted routes: `MembersPanel` (member/DEA/sponsored register + detail with immutable lifecycle ledger + review history; application intake idempotent on LEI; all 10 lifecycle actions — DD status, agreements, products/ports, admission decision (server-gated on DD COMPLETED + ≥1 agreement), suspend/reinstate/terminate, appeal + outcome, periodic review with FAIL auto-suspend), `RulebooksPanel` (version register + detail with notices/acks; DRAFT → FILED → APPROVED → ACTIVE lifecycle; regulator verdict ingestion; activation with EMERGENCY flag — server refuses VENUE_RULEBOOK_NOT_APPROVED; participant notices incl. broadcast; member acknowledgement evidence), `OversightPanel` (interventions record/lift; investigation & disciplinary cases with append-only sha256 evidence and the legal transition table encoded client-side — OPEN→INVESTIGATING→CHARGED→SANCTIONED|DISMISSED→CLOSED, terminal moves surface the required outcome field; conflicts-of-interest declare/resolve), `AssurancePanel` (self-assessment file/sign-off; CCO report generate → board sign → regulator file; launch prerequisite checklist + launch gate READY/BLOCKED with missing-item breakdown). Zero backend changes. 6 component tests; 701/701 suite green.

---

### Task 10.5.3.15: Conduct, Governance & DORA Console

**Objective:** Reg-change management, execution policies, FX Global Code, governance packs, recertification, data residency, ICT providers.

**Implementation:**
1. Reg changes — `GET/POST /api/v1/admin/regulatory-changes`, `GET/PUT …/{id}/impact`, `POST …/{id}/correspondence`, `POST …/impacts/{id}/done` (10-business-day SLA timers).
2. Execution policies — `GET/POST /api/v1/admin/execution-policies`, `POST …/{id}/{activate,review}`; product profiles/target markets — `GET/POST/PUT /api/v1/admin/product-profiles[/…]`, `GET/POST /api/v1/admin/product-target-markets[/{id}/review]`.
3. FXGC — `GET/POST /api/v1/admin/fx-global-code/assessments[/{id}]`, `POST …/{id}/{complete,sign,publish}`.
4. Governance/recert/residency/ICT — `GET/POST /api/v1/admin/governance-packs[/{id}][/generate|/release]`, `GET/POST /api/v1/admin/recert[/{id}[/decisions]]`, `GET /api/v1/admin/data-residency/{policies,access-log}`, `GET/POST/PUT/DELETE /api/v1/admin/ict-providers[/{id}[/reviews]]`, `GET …/due`.

**DoD:**
* [x] All conduct/governance surfaces render live state; SLA clocks displayed

**SDD Checklist:**
- [x] Spec checkpoint: conduct & DORA consoles complete — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-conduct/` — `ConductPage` (`/admin/conduct`) + `api.ts` + 4 panels: `RegChangesPanel` (register + intake; per-change impact map with `PUT …/{id}/impact`, `impacts/{id}/done` completion, lifecycle transitions `triage|scope|implement|close|assign_owner`, correspondence/regulator info-hold attach, 10-business-day triage SLA clock rendered per row), `PoliciesPanel` (execution-policy version history + DRAFT upsert + CCO activate — supersedes incumbent, frozen-while-review-overdue surfaced — + periodic review `APPROVE|NARROW|SUSPEND`; product-profile register/create with dual-control PENDING semantics honest-rendered; `PUT …/{id}/target-market` assignment + `product-target-markets?overdue=true` review queue), `FXGCPanel` (annual assessment intake `{period, code_version?}`; detail view over the 55-principle verdict matrix; officer verdict `{principle_id, adherence_status, evidence_summary?, remediation_ref?}`; complete → Statement of Commitment (PENDING verdicts refused server-side); executive sign-off; public-register publish flag), `DoraPanel` (governance packs list/detail with `hash_ok` re-verification + on-demand generate `CEO_DAILY|BOARD_QUARTERLY|BOARD_ADHOC` + distinct-approver `…/{id}/release`; recertification open → auditor report → per-binding `{approve}` decisions; data-residency policy register + cross-border access log; ICT provider register — create/update/retire `DELETE`, due-obligation alerts (overdue reviews/renewals/stale exit tests) banner, per-provider event log + review/renewal/substitution-test record). ICT response bodies are PascalCase (no json tags) — parser accepts both casings; `PUT`/`DELETE` bound to `ApiClient` + `X-Admin-Env` since `BoundAdminApi` exposes only get/post. Zero backend changes. 7 component tests; 708/708 suite green.

---

### Task 10.5.3.16: Content, Promotions & Break-Glass Admin

**Objective:** Announcements, promotions, strategy-template curation, break-glass access, API-key admin singletons.

**Implementation:**
1. Announcements — `GET/POST/PATCH/DELETE /api/v1/admin/announcements[/{id}]`; user-side `GET /api/v1/announcements[/{id}]` inbox surfaced on Home.
2. Promotions — `GET/POST/PUT /api/v1/admin/promotions`, `GET …/report`, `POST …/{id}/{approve,reject}`; render-gate aware.
3. Strategy-template curation — `GET /api/v1/admin/strategy-templates`, `POST …/{id}/{approve,reject}`; copy-strategy suspend `POST /api/v1/admin/copy/strategies/{id}/suspend`.
4. Break-glass — `POST /api/v1/admin/break-glass`, `POST …/{id}/review`; dual-control approval rows `POST /api/v1/admin/dual-control/{id}/{approve,reject}` (extends `DualControlPanel`); `PUT /api/v1/admin/api-keys/{id}/extend-expiry`.
5. Liquidity-provider detail — `GET/PUT /api/v1/admin/liquidity-providers/{id}`, `GET …/{id}/alerts` (extends ConsolesPanel which already lists LPs).
6. Admin webhook management — `GET /api/v1/admin/webhooks/dead-letters` + `POST …/{id}/retransmit` dead-letter review (mounted). Endpoint inspection/disable across accounts is a backend seam gap (the store only lists per-account); if the surface needs it, this task adds the admin route first — register it in routes_v1.go when added (user-side register/delete already shipped in `webhooks/`).

**DoD:**
* [x] Content CRUD + promo lifecycle + break-glass review + dual-control queue all functional

**SDD Checklist:**
- [x] Spec checkpoint: content/engagement/emergency-access admin complete — defined first, validated against spec

**Execution record (2026-10-07):** `frontend/src/features/admin-content/` — `ContentPage` (`/admin/content`) + `api.ts` + 5 panels: `ContentPanel` (announcement register incl. drafts/retracted — create/PATCH-edit/Retract-as-DELETE disclosure-preserving transitions; maintenance windows schedule + cancel-as-DELETE), `PromotionsPanel` (`?status=` queue; create DRAFT; PUT revision lands a new version under the slug keeping the approval chain; submit/approve/reject{reason}/withdraw verbs gated by row status; approve carries the five-element MiFID checklist + `second_approver_id` for claims promos + ≤12mo `approved_until`; marketing report strip with SLA/expired-flagged counters), `CurationPanel` (template review queue `?status=` + approve/reject; copy-strategy suspend `{reason}` misconduct action), `EmergencyPanel` (break-glass mint `{grantee_id, incident_ref, reason, ttl_seconds, second_approver_id|unreachable_approver}` + mandatory post-incident review `{notes}` — self-review refused server-side; API-key `extend-expiry` PUT honestly rendered as a 202 dual-control request, never inline; webhook dead-letter queue + per-delivery retransmit), `LpPanel` (per-LP detail with instrument feed configs, guarded lifecycle PUT `{status, staleness_timeout_ms?, reason}`, performance-alert trail `?open=1`). User-side: `AnnouncementsInbox` on `HomePage` reads the public `/announcements` feed. The dual-control queue itself is already served by `DualControlPanel` on `/admin` — this surface submits into it rather than duplicating it; cross-account webhook endpoint inspection remains the documented backend seam gap. Zero backend changes. 7 component tests; 715/715 suite green.

---

### Batch F — User-side completion (账户管理 user half + remaining features)

### Task 10.5.3.17: Account Self-Service Completion (extends Task 10.3.22)

**Objective:** Close every uncovered `/account/*` route — delegation, approvals, limits, leverage/margin, swap-free, appropriateness, account data views.

**Implementation:**
1. Delegated users & approvals — `GET/POST/PUT/DELETE /api/v1/account/delegated-users[/{id}]`, `POST …/revoke-all`, `GET/PUT/DELETE …/approval-policies[/{id}]`, `GET …/approval-requests`, `POST …/approval-requests/{id}/decide` (M-of-N per Task 12.3.11 backend).
2. Account API keys — `GET/POST/DELETE /api/v1/account/api-keys[/{id}]` + per-sub-account `POST/DELETE …/sub-accounts/{id}/api-keys[/{keyId}]` + `GET …/sub-accounts/aggregate`.
3. Trading parameters — `POST /api/v1/account/leverage`, `POST …/margin-mode`, `GET/POST …/swap-free[/request]`, `GET/POST …/appropriateness`; `GET …/risk-limits` + `GET …/rate-limits` read-only panels.
4. Account data views — `GET …/{liquidations,commission/{symbol},filters/{symbol},cost-preview,confirmations/{trade_id},statements,statements/{id}/download,snapshots,income,pnl,positions}` — folded into Performance/Reports pages where they already exist (extends Task 10.3.18/10.3.28; verify each against existing panels before adding — no duplicate surfaces).

**DoD:**
* [ ] Delegation + approval policies + account api-keys + leverage/margin-mode/swap-free/appropriateness all operable
* [ ] No route in the `/account/*` family remains UI-uncovered (or is documented as intentionally headless)

**SDD Checklist:**
- [x] Spec checkpoint: full account self-service surface — defined first, validated against spec

---

*Execution record (this implementation):* settings feature extension — new `DelegationPanel` + `TradingPanel` wired as lazy tabs into `SettingsPage` (/settings); `features/settings/SelfService.test.tsx` (5 tests). **Delegation tab**: register via `GET …/delegated-users`, `POST` grant with optional `expires_at`, edit-role `PUT`, per-row `DELETE` revoke, `POST …/revoke-all` kill-all; M-of-N approval policies via `GET` + upsert `PUT` (scope enum, `n_of_m` number, `disabled:true` disables) + `DELETE`; request inbox via `GET …/approval-requests` with role-aware `APPROVE|REJECT` votes (`POST …/{id}/decide`) and honest PENDING/FINAL counts — 400 rejection surfaces instead of fabricated confirm. **Trading parameters tab**: per-symbol leverage `PUT …/leverage` (blank symbol = account default), margin-mode `PUT` (409-refused-while-positions-open note), swap-free `GET/PUT` (`attestation_ref` links a `kyc_documents` row; status honest), appropriateness `GET` status + `POST` resubmit (`{questionnaire_version, answers:{qid:0–4}}` — PASS/FAIL is server-derived, never a client toggle), read-only `risk-limits` + `rate-limits` + `instruments/{symbol}/filters` KV rows, `sub-accounts/aggregate` family balances+limits, `liquidations` history with `DIRECT_CLOSE|RAF_DOWNGRADE` badges (StatusBadge humanizes `_`). **Reports**: DownloadCenter tax card moved to canonical `/account/tax-report` (supersedes legacy `/tax/report`; same producer). **Coverage sweep**: remaining `/account/*` rows are pre-covered (api-keys/sessions/safety `ConsentFreezeGdpr` panel/positions `usePositions`/pnl/income/snapshots `PerformancePage`/profile/notifications incl. `consent` PATCH/webauthn/login-history/balances/statements/sub-accounts — Settings panels + Performance); `account/close` POST wired in `settings/api.ts` close-account flow. Contract gaps: `sub-accounts` POST/DELETE and `sub-accounts/{id}/transfer` remain backend-only (sub-account registry managed elsewhere); `GET /account/commission` wire exists in admin surface but is *not* mounted on `/api/v1` — headless. Tests: `frontend vitest` 720/720.
### Task 10.5.3.18: Funding Self-Service Completion (extends Task 10.3.23)

**Objective:** Bank accounts, withdrawal whitelist, currency conversion, rail selection, deposit initiation.

**Implementation:**
1. Bank accounts — `GET/POST/DELETE /api/v1/funding/bank-accounts` + `GET …/rails` + `POST …/rail-selection` (rail picker with fee/cut-off comparison from `POST …/fee-estimate` — already in `FeeEstimator`).
2. Withdrawal whitelist — `GET /api/v1/funding/withdrawal-whitelist`, `POST …/{enable,disable}` (disable window countdown shown).
3. Convert — `POST /api/v1/funding/convert` + `GET …/conversions` history row in HistoryPanel.
4. Deposits — `POST /api/v1/deposits` + `GET …/{currency}` address/instruction display (extends DepositPanel).

**DoD:**
* [ ] Bank-account registration, whitelist toggle, conversion, rail selection, deposit initiation all functional

**SDD Checklist:**
- [x] Spec checkpoint: funding self-service covers all rails — defined first, validated against spec

---

*Execution record (this implementation):* extends the existing `features/funding` (Task 10.3.23) — no new feature dir. New `AccountsPanel` (lazy `accounts` tab on `/funding`): beneficiary registry `GET/POST/DELETE /funding/bank-accounts` (delete is query-form `?id=` per mounted route), PENDING_VERIFICATION/VERIFIED/REJECTED honest badges + rejection_reason, register form emitting the full `BankAccountInput` contract (iban/account_number/swift_bic/bic_routing optional). Whitelist card: `GET …/withdrawal-whitelist` renders mode/lock flags + VERIFIED beneficiary membership; enable/disable POSTs; 24h egress-lock countdown from `withdrawal_lock_until` (`useNow`+`formatCountdown`), reenable_locked surfaced verbatim. `GET /funding/rails` capability matrix (currencies/cutoff_label/settlement_lag/instant cap) + `POST /funding/rail-selection` preview showing chosen rail, value_date, queued_next_day, rejected_rails — fail-closed codes (BANKING_RAIL_UNAVAILABLE/RAIL_CUTOFF_EXCEEDED) surface via ErrorBox. `DepositPanel` gains `DepositIntentForm` — `POST /deposits` with generated Idempotency-Key, honest PENDING status + `replayed`/`flags` rendering. `HistoryPanel` gains `ConvertCard` — `POST /funding/convert` (fail-closed rate source server-side) + `GET /funding/conversions` history (`{items,count,limit}` envelope) rendering mid/spread_bps/rate_applied/rate_source verbatim. Contract notes: `DELETE /funding/bank-accounts` is `?id=` not `/{id}` (route registered that way in Phase-11). Zero backend changes. Tests: 4 new (registry CRUD + ?id= delete, whitelist+lock countdown+rail picker, deposit intent w/ Idempotency-Key, conversion + history); suite 724/724.
### Task 10.5.3.19: Strategy Marketplace & Baskets

**Objective:** Strategy CRUD/pause/resume, template instantiate, baskets, promotions, copy-strategy listing — the user-facing half of the strategy engine.

**Implementation:**
1. Strategies — `GET/POST/DELETE /api/v1/strategies[/{id}]`, `POST …/{id}/{pause,resume}`.
2. Templates — `GET /api/v1/strategy-templates`, `POST …[/{id}/instantiate]` (admin-side approval is Task 10.5.3.16 — no overlap).
3. Baskets & promos — `GET /api/v1/baskets/{id}`, `GET /api/v1/promotions/{id}`; copy-strategy author flow `POST /api/v1/copy/strategies`, `GET …`, `POST …/{id}/list` (extends Task 10.3.26).

**DoD:**
* [ ] Strategy lifecycle + template instantiate + basket view + copy-strategy listing all functional

**SDD Checklist:**
- [x] Spec checkpoint: strategy marketplace user flow complete — defined first, validated against spec

---

*Execution record (this implementation):* extends `features/copy-grid` (Task 10.3.26) — new `MarketplacePanel` as a third tab on the CopyGrid page. **My strategies**: `GET/POST /strategies` (kind-aware create — DCA emits `from/to/amount/schedule`, REBALANCE emits `targets` map + `drift_band_pct`), `POST …/{id}/pause|resume`, `DELETE …/{id}` (cancel + unwind), row-expand detail via `GET …/{id}` rendering run ledger (status + skip_reason + legs). **Templates**: `GET /strategy-templates` (approved catalog only) + `POST …/{id}/instantiate` (config copy → 201 strategy) + `POST /strategy-templates` publish (config JSON parsed client-side; lands PENDING_APPROVAL — admin queue stays in admin-content). **Copy author**: `POST /copy/strategies` (INCUBATING profile: display_name/description/currency/instrument_class/profit_share_pct) + `POST …/{id}/list` — the ≥30d incubation + appropriateness gate refusal surfaces as the coded error verbatim. **Viewers**: `GET /baskets/{op_id}` (op status + per-leg order status/filled) and `GET /promotions/{id}` (render-gated; 410 PROMOTION_NOT_APPROVED flows through ErrorBox). Contract gaps: no "my copy profiles" GET exists (`Discover` is LISTED-only; `StrategiesByManager` store method is unmounted) — the author section renders only session-created/listed rows honestly, recorded here. Zero backend changes. Tests: 5 new MarketplacePanel cases; suite 729/729.
### Task 10.5.3.20: Advanced Order Composition (extends Task 10.3.7/10.3.27)

**Objective:** Wire the remaining mounted order-composition endpoints into the advanced ticket.

**Implementation:**
1. Algo submitters — `POST /api/v1/orders/{twap,vwap,scaled,basket,spread,roll}` (+ `orders/algo` pause/resume/delete already partially wired — verify then extend `AlgoPanel`).
2. Batch & mass ops — `POST/DELETE /api/v1/orders/batch`, `DELETE …/all`, `POST …/cancel-all-after` (dead-man countdown UI already has the endpoint family — verify coverage, add missing).
3. Order detail — `GET/PUT/DELETE /api/v1/orders/{id}`, `PUT …/amend/keep-priority`, `POST …/cancel-replace`, `GET …/amendments` history drawer in `OrderInspectModal`; `GET/DELETE /api/v1/order-lists/{id}` detail view.

**DoD:**
* [x] All six composite order types + batch/cancel-all-after + amendment history reachable from the ticket/orders surfaces

**SDD Checklist:**
- [x] Spec checkpoint: order composition UI covers every mounted submit endpoint — defined first, validated against spec

*Execution record (this implementation):* extends `features/history` (Task 10.3.27) — no new feature dir. `CompositeSubmitPanel` (new, Algo tab) submits all six mounted composite types with the flat `ParseTypedSubmit` body: `POST /orders/{twap,vwap,scaled,spread}` (twap interval_secs+duration_secs+discretion_pips; vwap duration_secs(+interval/discretion); scaled levels+distribution+start_price+spacing_pips; spread legs[]+spread_price) plus `POST /orders/basket` (dynamic multi-leg builder, idempotent) and `POST /orders/roll` (contract_id + new_value_date|tenor + max_roll_price_bps). `BatchOpsPanel` (new, Open-orders tab): `POST /orders/batch` JSON intake `{"orders":[…]}`, `DELETE /orders/batch` `{order_ids|client_order_ids}` with the atomicity note, `DELETE /orders/all` behind a two-click confirm, and scoped `DELETE /orders?symbol=&side=` mass cancel. `OrderInspectModal` gains `AmendmentLedger` — `GET /orders/{id}/amendments` lazily queried behind a disclosure, PascalCase `AuditEntry` rows rendered verbatim (operation/field/old→new/actor/request-id). `OrderListsPanel` gains `OrderListIntake` (POST `/order-lists` — OPO/OPOCO: working BUY + pending SELL leg[s] with type/price/stop_price; pending quantity deliberately unsent, server recomputes net proceeds per §24 #287), per-row Detail (GET `/order-lists/{id}` — legs with role/state/order_id/params + fail_reason alert) and two-click Cancel (DELETE `/order-lists/{id}`, open tab only). **Bug fixed:** `cancelAlgoOrder` previously hit the bulk `DELETE /algo-orders` (ignores a per-id body — silently cancelled EVERY running algo + bots); now per-row cancel uses `DELETE /orders/algo/{id}` and the bulk route is a separate double-confirmed "Stop all algos" (`?symbol=` filter supported). Dead-man seam already covered: `POST /orders/cancel-all-after` is a handler-level alias of `countdown-cancel-all` (`CountdownCancelAll`, same `{countdown_ms, renew}` contract) served by `DeadmanSwitch` — no duplicate UI. Pre-covered elsewhere (verified, not duplicated): `POST /orders/algo` + trailing-stop via the advanced ticket, `GET/PUT/DELETE /orders/{id}` + `amend/keep-priority` + `cancel-replace` via `OrderInspectModal`/`lib/trading`; `POST /positions/close-all` lives in Positions. Zero backend changes. Tests: 9 new (TWAP/basket/roll submit bodies, batch submit+cancel bodies, mass-cancel confirm + scoped query, algo row-vs-bulk route separation, OPOCO intake body, list detail+cancel, PascalCase amendment ledger); suite 739/739. `go test ./internal/gateway` green.

---

### Task 10.5.3.21: Market Analytics & Intelligence Pages

**Objective:** The `/analytics/*` + `/market/*` + `/stats/*` read surfaces — positioning, OI, taker flow, depth stats.

**Implementation:**
1. Analytics panels — `GET /api/v1/analytics/{open-interest,long-short-ratio,taker-flow}/{symbol}`, `GET …/{pnl,volume,stats}` on the Discovery or new `/analytics` route.
2. Market intel — `GET /api/v1/market/{depth,open-interest,positioning,taker-volume,performance}`, `GET /api/v1/stats/24h[/{symbol}]`, `GET /api/v1/market-data/snapshot`, `GET /api/v1/market-data/l3-snapshot/{symbol}` (premium-gated rendering with honest 402/403 states).
3. Every panel renders the backend's publication-delay/freshness metadata (5-min delay badges per Phase-23).

**DoD:**
* [x] Analytics/market intel pages render live data with delay badges and premium gating

**SDD Checklist:**
- [x] Spec checkpoint: market analytics surface complete — defined first, validated against spec

*Execution record (this implementation):* new `features/analytics` — `AnalyticsPage` (`/analytics`, Research nav order 1, auto-glob-registered) + `api.ts` + 3 panels + page-level cards. `SentimentPanel`: `GET /analytics/open-interest/{symbol}?interval=1h|4h|1d` (current OI/notional/positions + `stale` flag + `insufficient_data`), `GET /analytics/long-short-ratio/{symbol}?period=` and `GET /market/taker-volume?symbol=` — `delayed`/`delay_ms`/`as_of_ms` rendered as a "5-min delayed" badge; below-floor cohort buckets render an explicit "suppressed — cohort below floor" row, never silently dropped. `PositioningCard`: `GET /market/positioning?symbol=` cohort counts/notionals + delayed badge; `insufficient_data` honest empty state. `VenuePanel`: `GET /stats/24h` rolling table (server_time_ms header), `GET /market/performance` status card — `held`/`held_age_ms`/`divergent_metrics` and the venue rollup (fill_rate/uptime) verbatim, `GET /analytics/volume?granularity=1h|1d` with `truncated` disclosure, `GET /analytics/stats` fill-rate table. `DepthPanel`: `GET /market/depth?symbol=&limit=` L2 book (seq + updated_at_ms, 15s refresh) + on-demand `GET /market-data/l3-snapshot/{symbol}` — professional-tier refusal (402/403 coded error) surfaces through ErrorBox verbatim. `MyPnlCard`: `GET /analytics/pnl` rows (realized/unrealized/fees/net per day×symbol) rendered only when signed in — the endpoint is TierBasic and the card is absent without a session (verified by test). Contract notes: `/market-data/snapshot?level=L2` shares the same BookSnapshot seam as `/market/depth` (covered there); `/analytics/taker-flow` path-param variant covered by the query-param `/market/taker-volume` surface (same `serveTakerFlow` handler). Zero backend changes. Tests: 4 new (stats+held+delayed+suppressed render, L3 premium refusal, signed-in P&L, signed-out zero-pnl-calls); suite 743/743. `go test ./internal/gateway` green.

---

### Task 10.5.3.22: History Explorer & Export Center

**Objective:** Tick/trade/block-tape history browsing + async export jobs + TCA.

**Implementation:**
1. History explorer — `GET /api/v1/history/{trades,ticks,klines,block-trades}/{symbol}`, `GET …/swap-rates`, `GET …/trades/{symbol}/export` — cursor-paginated tables.
2. Export jobs — `GET /api/v1/export-jobs[/{id}[/download]]` — async job list with status/expiry; folded into the Reports download center (extends Task 10.3.28).
3. TCA — `GET /api/v1/reports/tca/{order_id}` per-order TCA card linked from order inspect.
4. PAMM detail — `GET /api/v1/pamm/pools[/{id}[/statement]]`, `POST …/{id}/{invest,redeem}` (extends Task 10.3.26-adjacent `PammPage`).

**DoD:**
* [x] History queries paginate correctly; export jobs create→poll→download; TCA card renders per order

**SDD Checklist:**
- [x] Spec checkpoint: history + export center complete — defined first, validated against spec

*Execution record (this implementation):* new `features/explorer` — `HistoryExplorerPage` (`/explorer`, Research nav order 2, auto-glob) + `api.ts`. Dataset picker over `GET /history/{trades,ticks,block-trades}/{symbol}`, `GET /history/klines/{symbol}?interval=` (select constrained to the 12 persisted labels incl. `1D`/`1W`/`1M`), `GET /history/swap-rates?symbol=` — all §8.8 keyset-paginated via accumulating `next_cursor` "Load more" (no offset drift); every page renders the envelope's `access_tier`/`delayed`/`degraded` badges verbatim; block-tape renders `bust`/`corrected_by`/`supersedes` lineage + `delay_ms`; swap sheets render the triple-Wednesday `×3` badge + `accrual_count` reconciliation count. Export: `GET /history/trades/{symbol}/export?kind=&format=&async=1` enqueue buttons (csv/json) → 202 job surfaced with id + pointer to Reports. `ExportJobsPanel` folded into Reports→Downloads (below DownloadCenter): `GET /export-jobs` owner list with status/truncated/error, `expires_at` countdown, poll-while-QUEUED/RUNNING (3s), "Load older jobs" cursor paging, download via `downloadFile` on `/export-jobs/{id}/download` only when the server emitted `download_url` (expired links never offered). TCA: `TcaPanel` (new Reports tab) renders `GET /reports/tca/{account_id}?period=&instrument_class=` bucket table; `TcaCard` embedded in `OrderInspectModal` filters the account report to the order's `symbol` — **contract deviation recorded**: the mounted route is per-account (`{account_id}` with FORBIDDEN on foreign ids), not per-order as the task row suggested, so the card is an honest account-scoped per-symbol view with a link to the full report, not a fabricated per-order metric. Fixed a latent DownloadCenter bug in the same seam: the TCA download card resolved master scope to account `0` → INVALID_REQUEST for every non-admin; now resolves the claims account id from the session store. PAMM: `listPools`/`poolStatement` gained keyset `?after=` paging with "Load older pools/entries" (shown only when the last page was full — the envelope carries no cursor). Zero backend changes. Tests: 5 explorer + 2 reports additions; suite 750/750; `go test ./internal/gateway` green.

---

### Task 10.5.3.23: Public Transparency & System Status Pages

**Objective:** No-auth public surfaces — RTS 27/28, venue info, security policy, status/incidents, maintenance schedule, API meta.

**Implementation:**
1. Public reports — `GET /api/v1/venue/info`, `GET /api/v1/venue/best-execution/{rts27,rts28}`, `GET /api/v1/venue/best-execution/{rts27,rts28}/{id}/csv` rendered as the public transparency page set.
2. Status — `GET /api/v1/system/incidents`, `GET /api/v1/maintenance/schedule`, `GET /api/v1/session/status`, `GET /api/v1/meta/{pagination,rate-limits}`, `GET /api/v1/exchange-info` (trading-hours/limits doc page).
3. Security — `GET /api/v1/security/policy` + `POST /api/v1/security/disclosures` (public vulnerability-disclosure form; admin triage is Task 10.5.3.16's `admin/security/*`).

**DoD:**
* [x] Public transparency/status/security pages reachable without auth; disclosure intake posts

**SDD Checklist:**
- [x] Spec checkpoint: public transparency + status surfaces live — defined first, validated against spec

*Execution record (this implementation):* new `features/transparency` — three no-auth routes in a new `Venue` nav section (added to `SECTION_ORDER` between Invest and Account; `manifest.test.tsx` order assertion updated). `/status` (`StatusPage`): `GET /session/status` 24/5 session card (state/market_open/shard_coverage/`consistent:false` badge/next transition/pending_effects), `GET /system/incidents` severity-styled list with postmortem links, `GET /maintenance/schedule` window table — all 30–60s polling, public-safe retry (no retry on 4xx). `/transparency` (`TransparencyPage`): `GET /exchange-info` venue document — 24/5 trading hours, per-symbol directory (tick/lot/min-qty/leverage/settlement T+0..T+2/order-types + `new_orders_allowed:false` disclosure), §13.14 entity leverage ceilings, product profiles; `GET /venue/best-execution/{rts27,rts28}` PUBLISHED-only lists with `zero_activity` badge + `/{id}/csv` artifact download via `downloadFile`; `GET /meta/pagination` (§8.8 envelope shape + sortable/filterable matrix) and `GET /meta/rate-limits` tier table. `/security` (`SecurityPage`): `GET /security/policy` rendered verbatim as markdown (raw fetch — the endpoint is text/markdown, not JSON; `downloadFile`'s Blob.text() isn't jsdom-safe so a plain `res.text()` path is used) + `POST /security/disclosures` intake with title/severity/handle/email/components/reproduction/attribution — the `website` honeypot is visually hidden, tabindex −1 and always POSTed empty; 201 `{report_id, duplicate}` surfaces inline. Zero backend changes. Tests: 3 new (public render without session, RTS27/RTS28 switch + meta tables, disclosure POST contract + honeypot); suite 753/753; `go test ./internal/gateway` green.

---

### Task 10.5.3.24: Auth Flow Completion

**Objective:** The remaining auth surfaces — email verification, 2FA enroll path, passkey assertion, KYC requirements step.

**Implementation:**
1. `POST /api/v1/auth/verify-email` flow (link landing + resend) — complements Task 10.3.21 (login/register/2FA/session already shipped).
2. `POST /api/v1/auth/2fa/enroll` vs `…/setup` distinction verified and both wired; `POST /api/v1/auth/passkey/assert` in the passkey sign-in path.
3. `GET /api/v1/kyc/requirements` drives the KYC form dynamically (extends Task 10.3.24).

**DoD:**
* [x] Verify-email + 2FA enroll + passkey assert all walkable end-to-end

**SDD Checklist:**
- [x] Spec checkpoint: auth flows complete — defined first, validated against spec

*Execution record (this implementation):* four auth surfaces completed, all extending existing seams (zero new feature dirs, zero backend changes). **Email verification** — new `VerifyEmailPage` at `/verify-email` (public route): `?token=` consumed once on mount (StrictMode-guarded ref) via `POST /auth/verify-email {token}` → `{user_id, email_verified}`; manual-paste field when the URL carries no token; expired/invalid tokens surface the backend error verbatim with a retry affordance. **Contract gap recorded:** the route registry exposes *no* resend-verification endpoint — the UI honestly points back to sign-in rather than fabricating a resend call. **2FA enroll vs setup** — `TotpEnrollment` (Settings → Security) now calls `POST /auth/2fa/enroll` for the initial "Set up 2FA" ceremony and `POST /auth/2fa/setup` for the new "Get a new secret" re-stage affordance mid-enrollment; both share the backend's stage-a-candidate semantics (non-destructive until `/2fa/verify` proves the first code); `auth/api.ts` gained `totpEnroll` sharing the `parseTotpSetup` normalizer. **Passkey sign-in** — `LoginPage` gained "Sign in with a passkey": `POST /auth/passkey/assert` (empty body → `{challenge_id, publicKey}`) → `navigator.credentials.get` → second POST `{challenge_id, credential}` → session bundle stored via the same `userFrom`/`setSession` path as password login (`amr ["fido2"]`, `two_factor_verified` server-side). WebAuthn helpers (`toCreationOptions`/`toRequestOptions`/`credentialToJSON`/`webauthnSupported`) extracted to `lib/auth/webauthn.ts` and shared with `WebAuthnPanel` (deduplication). Unsupported browsers fail closed: button disabled + explicit note; no credential data is fabricated or logged. **KYC requirements** — `GET /kyc/requirements` (session-resolved tier+jurisdiction) now drives `UploadWizard`: merged ops-matrix rows become per-document-type wizard steps (required rows mandatory, optional rows attachable, verbatim `max_doc_age_days`/`doc_expiry_lead_days`/`notes`/`doc_group` hints, unknown types rendered verbatim), replacing the hardcoded 3-slot grid; `KycPage` gained a `PolicyCard` rendering the tier policy (liveness/biometric/rescreen cadence/re-verify interval/review SLA/daily caps — nulls rendered "Negotiated"/"Unlimited" verbatim). Loading → "Loading document requirements…" with Next gated; query failure → static Task-10.3.24 grid + explicit "requirements service unavailable" disclosure; empty matrix → honest "no documents required". Tests: +9 (verify-email token/manual/error, passkey challenge→assertion→session + unsupported fail-closed, enroll-vs-setup routing + verify backup codes, matrix-driven checklist + policy card + fallback disclosure); suite 762/762; tsc/eslint/prettier clean on touched files; `go test ./internal/gateway` green.

---

### Batch G — Backend seams & audit closure

### Task 10.5.3.25: JetStream Consumer Startup Resilience

**Objective:** Fix the boot-order race found in the functional audit — `EnsureConsumer` callers (marketdata ×6, analytics ×2, compliance ×1) exited permanently when streams weren't provisioned yet.

**Implementation:**
1. Add bounded retry/backoff (or `EnsureStream`-first + consumer retry) to the consumer-attach path in `services/internal/nats` consumers — every `EnsureConsumer` failure must retry until ctx cancel, with WARN escalation after N attempts (fail-visible, not fail-silent).
2. Owner seam: NATS backbone = Phase-01 Task 1.3.11 / graceful degradation Phase-09; implemented in `internal/nats` shared helper — no per-service duplication.

**DoD:**
* [x] Kill-order test: bring a consumer up before its stream exists → it attaches once the stream appears (no restart) — `EnsureConsumerRetry` shared helper in `internal/nats/consumer.go` (exponential 500ms→15s backoff until ctx cancel, Info→Warn escalation at 6 attempts, opportunistic canonical-stream re-ensure); 12 call sites switched (marketdata ×2 sources, analytics, compliance sar-signals, webhooks ingest, gateway ×5, boot_regreport ×2 — gateway sites bounded to 90s preserving fail-operational fallbacks); live `TestIntegrationEnsureConsumerRetryStreamLate` deletes `l3` mid-attach → self-heals on attempt 2 in 0.63s; `...CtxCancel` honours deadline

**SDD Checklist:**
- [x] Spec checkpoint: consumers self-heal across stream provisioning order — defined first, validated against spec

---

### Task 10.5.3.26: Engine Position-Query Seam (recon POSITIONS leg)

**Objective:** Close the `POSITIONS` INCONCLUSIVE finding — recon can only verify PG↔projection because the C++ core has no admin position-query seam.

**Implementation:**
1. Add a read-only positions snapshot seam on the engine (Aeron admin channel or IPC ring admin opcode), surfaced as an internal query consumed by the reconciliation service.
2. Owner seam: engine-side per Phase-02/Phase-19 (position state) + recon consumer per Phase-24; implementation must not bypass the existing reconciliation findings pipeline.

**DoD:**
* [ ] Recon POSITIONS leg reports conclusive pass/fail on a clean run — no structural INCONCLUSIVE

**SDD Checklist:**
- [ ] Spec checkpoint: positions recon has a verifiable engine leg — defined first, validated against spec

---

### Task 10.5.3.27: Route-Coverage CI Gate + Audit Re-run

**Objective:** Prevent regression — the diff that produced this plan becomes a standing check.

**Implementation:**
1. `route-contracts.ts` regen + a CI check that fails when a mounted non-excluded route has no UI surface reference (exclusion list for `/test/*`, machine-to-machine, and deliberately headless routes, each exclusion commented).
2. Re-run the functional-completeness crawler + rendered audit across all new surfaces (axe, mobile overflow, dead-control probes).

**DoD:**
* [ ] CI gate green with the exclusion list committed; audit re-run clean on new routes

**SDD Checklist:**
- [ ] Spec checkpoint: route coverage is mechanically enforced — defined first, validated against spec

---

## 10.5.4 Deliverables

- `frontend/src/features/admin-ops/`, `admin-crm/`, `admin-funding/`, `admin-settlement/`, `admin-reg/`, `admin-governance/`, `admin-content/` (final split may consolidate — one feature dir per console cluster, routes via `routes.ts`/`nav.ts` convention).
- User-side completions folded into existing feature dirs (`settings/`, `funding/`, `advanced-orders/`, `history/`, `reports/`, `pamm/`, `copy-grid/`, `discovery/`).
- New public routes (no shell auth): transparency/status/security pages.
- `internal/nats` consumer-retry fix + C++ admin position-query seam + recon leg binding.
- CI route-coverage gate + exclusion list.

## 10.5.5 Dependencies

- Phase 10 (UI baseline), Phase 05 (route registry/error registry), Phase 07 (admin audit), Phase 09/13 (ops safety backends), Phase 11/24 (funding/settlement backends), Phase 14/21 (KYC/compliance backends), Phase 15 (instrument lifecycle), Phase 16/20/23 (orders/analytics/history backends).

## 10.5.6 Duration Estimate

| Batch | Tasks | Nominal days |
|-------|-------|--------------|
| A — Ops safety & control | 3 | 4 |
| B — Customer management | 3 | 5 |
| C — Funds & assets | 4 | 6 |
| D — Market/instrument governance | 2 | 3 |
| E — Regulatory & conduct | 4 | 6 |
| F — User-side completion | 8 | 8 |
| G — Seams & audit closure | 3 | 2 |
| **Total** | **27** | **34** |

Batch order is a recommendation, not a hard dependency chain: B→C→E are independent console clusters and can parallelize across engineers; A should land first because it carries the emergency levers; G.25 is independent and can start immediately (backend-only).
