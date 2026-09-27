# Phase 11 — Funding via Banking Rails, Suspension, Stats

**Duration:** 7–10 days (supersedes 6–8 — duration itemization completed 2026-09-27, remediation #35: Tasks 11.3.9/11.3.10 added to §11.6) (supersedes prior 5–7 — Tasks 11.3.7–11.3.8 added 2026-09-15)
**Dependencies:** Phases 3, 4, 5, 7, 10
**Spec Reference:** §17 (Backoffice & Settlement), §5.6 (funding_transactions)

---

## 11.1 Objectives

Implement funding via banking rails (SWIFT, SEPA, FedNow, ACH, CHAPS, TARGET2), withdrawal flow with 15min confirmation + review tiers, deposit flow with anti-fraud tiers, trading suspension/kill-switch, and market statistics.

---

## 11.2 Prerequisites

- Phases 3, 4, 5, 7, 10 complete

---

## 11.3 Tasks

### Task 11.3.1: Banking Rails Integration

**Objective:** Integrate SWIFT, SEPA, FedNow, ACH, CHAPS, TARGET2 for deposits and withdrawals.

**File Locations:** `services/internal/funding/banking/`

**Implementation:**
1. **SWIFT:** MT103 (customer payment), MT202 (bank transfer), pacs.009 (credit transfer). SWIFT Alliance or partner API.
2. **SEPA:** SEPA Credit Transfer (SCT), SEPA Instant Credit Transfer (SCT Inst). ISO 20022 pain.001.
3. **FedNow:** FedNow API (instant USD payments).
4. **ACH:** ACH file generation (NACHA format) or partner API (Plaid, Stripe).
5. **CHAPS:** CHAPS (Clearing House Automated Payment System) for UK GBP same-day payments. Bank of England CHAPS API or partner.
6. **TARGET2:** TARGET2 (Trans-European Automated Real-time Gross settlement Express Transfer) for EU EUR real-time payments. ECB TARGET2 API or partner.
7. Each rail: `InitiatePayment`, `GetStatus`, `ConfirmReceipt`.
8. Nostro account per currency per rail.

**Definition of Done (Acceptance Criteria):**
* [ ] SWIFT MT103/MT202 messages generated correctly
* [ ] SEPA SCT and SCT Inst work
* [ ] FedNow instant payment works
* [ ] ACH file generation correct
* [ ] CHAPS same-day GBP payment works
* [ ] TARGET2 real-time EUR payment works
* [ ] Nostro account per currency per rail

**SDD Checklist:**
- [ ] Spec checkpoint: SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2 banking rails — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 11.3.2: Withdrawal Flow

**Objective:** Implement withdrawal with 15min confirmation + review tiers.

**File Locations:** `services/internal/funding/withdrawal.go`

**Implementation:**
1. `POST /api/v1/withdrawals` — create withdrawal (status: PENDING).
2. Confirmation token sent via email/SMS/push.
3. `POST /api/v1/withdrawals/{id}/confirm` — confirm within 15min window.
4. Review tiers:
   - `<$10K` → automatic processing after confirmation
   - `$10K–$50K` → standard checks (allowlist + velocity + sanctions)
   - `>$50K` → `PENDING_REVIEW` status, 4-hour ops SLA, ops alert
5. Withdrawal cooldown: 30min same bank account.
6. New bank account hold: 24h for unverified bank accounts.
7. Withdrawal caps: per-account daily, per-account hourly, exchange-wide daily.
   - **Fiat Limit Segregation Invariant (added 2026-09-27, remediation #38):** Per-account daily withdrawal caps enforce external fiat banking limits (KYC T0/T1/T2 tiers). Internal balance movements, such as PAMM pool unit allocations (`PAMM_INVEST`, `PAMM_REDEEM`) or master-sub account transfers (`TRANSFER`), MUST be strictly segregated and excluded from these fiat withdrawal limits.

**Definition of Done (Acceptance Criteria):**
* [ ] Withdrawal creates with 15min confirmation window
* [ ] `POST /api/v1/admin/withdrawals/{id}/approve|reject` declared for the >$50K PENDING_REVIEW tier (Finance Ops, dual control per §8.1) — endpoint added 2026-09-27, remediation #35: the 4-hour review tier was unactionable with no approval endpoint anywhere in the corpus; registered in Task 5.3.46
* [ ] Confirmation token validated within window; expired → AUTO_CANCELLED
* [ ] Review tiers: <$10K auto / $10K–$50K standard / >$50K PENDING_REVIEW+4h
* [ ] 30min cooldown same bank account enforced
* [ ] 24h hold for new bank accounts
* [ ] Withdrawal caps enforced (daily, hourly, exchange-wide)

**SDD Checklist:**
- [ ] Spec checkpoint: 15min withdrawal confirmation window — defined first, validated against spec
- [ ] Spec checkpoint: review tiers <$10K/$10K–$50K/>$50K+4h — defined first, validated against spec
- [ ] Spec checkpoint: 30min cooldown + 24h new account hold — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 11.3.3: Deposit Flow

**Objective:** Implement deposit with anti-fraud tiers.

**File Locations:** `services/internal/funding/deposit.go`

**Implementation:**
1. Deposit instructions: `GET /api/v1/deposits/{currency}` returns bank details (IBAN, routing, reference).
2. Bank statement polling or webhook from banking partner.
3. On deposit detected: create `funding_transactions` row (status: PENDING).
4. Anti-fraud tiers:
   - `<$10K` → auto-credit immediately
   - `$10K–$50K` → standard checks: double-confirmation + velocity/sanctions
   - `>$50K` → `PENDING_REVIEW` status, 4-hour ops SLA
5. Confirmation: 2 independent bank confirmations required (dual-source verification).
6. **Account-Scoped Idempotency Invariant (added 2026-09-27, remediation #38):**
   - Inbound deposit notifications and client deposit intent requests require an `Idempotency-Key` header strictly namespaced to the account: `idem:{account_id}:{idempotency_key}`.
   - Handlers MUST catch DB unique constraint violations (`23505`) on `(account_id, idempotency_key)` and replay the stored transaction acknowledgment (HTTP 200/201) if the payload matches, rather than failing with an unhandled 500 error.

**Definition of Done (Acceptance Criteria):**
* [ ] Deposit instructions return correct bank details per currency
* [ ] Deposit detected via bank statement polling or webhook
* [ ] Anti-fraud tiers: <$10K auto / $10K–$50K double-confirm / >$50K PENDING_REVIEW+4h
* [ ] Dual-source verification (2 independent confirmations)

**SDD Checklist:**
- [ ] Spec checkpoint: deposit anti-fraud tiers — defined first, validated against spec
- [ ] Spec checkpoint: dual-source bank confirmation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 11.3.4: Trading Suspension & Kill-Switch

**Objective:** Implement global trading halt and kill-switch.

**File Locations:** `services/internal/admin/killswitch.go`

**Implementation:**
1. `POST /api/v1/admin/kill-switch` (Risk Manager+, dual control) — rejects all new orders.
2. Cancels/reads/WS survive kill-switch.
3. Redis `halt:global` flag.
4. C++ core checks flag on every order submission.
5. `POST /api/v1/admin/kill-switch/reset` (Risk Manager+, dual control) — resumes.

**Definition of Done (Acceptance Criteria):**
* [ ] Kill-switch rejects all new orders (TRADING_HALTED)
* [ ] Cancels, reads, WS survive kill-switch
* [ ] Dual control required to activate and reset
* [ ] C++ core checks flag on every order

**SDD Checklist:**
- [ ] Spec checkpoint: global kill-switch with dual control — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 11.3.5: Market Statistics

**Objective:** Implement 24h market statistics.

**File Locations:** `services/internal/marketdata/stats.go`

**Transitive dependency note:** This task extends the market data service created in Phase 6 (Task 6.3.1). Phase 6 is not in Phase 11's explicit dependency list but is transitively available via Phase 10 (which depends on Phase 6).

**Implementation:**
1. 24h: open, high, low, close, volume, quote volume, trade count.
2. Per-symbol stats updated on every trade.
3. `GET /api/v1/stats/24h` — all symbols.
4. `GET /api/v1/stats/24h/{symbol}` — single symbol.

**Definition of Done (Acceptance Criteria):**
* [ ] 24h stats computed correctly per symbol
* [ ] Stats endpoints return correct data
* [ ] Updated on every trade

**SDD Checklist:**
- [ ] Spec checkpoint: 24h market statistics — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 11.3.6: Nostro-Aware Withdrawals

**Objective:** Track bank account balances and replenishment.

**File Locations:** `services/internal/funding/nostro.go`

**Implementation:**
1. Track nostro account balances per currency — **interim tracker superseded by Phase-24 Tasks 24.3.1/24.3.19** (balance tracking + 3-day-outflow-cover threshold methodology; remediation #35 — supersedes the assumption that two replenishment policies coexist after Phase-24).
2. When nostro balance < threshold: alert + auto-replenishment request (dual control).
3. Replenishment: transfer from reserve account to operating nostro.

**Definition of Done (Acceptance Criteria):**
* [ ] Nostro balances tracked per currency
* [ ] Low balance alert fires
* [ ] Replenishment requires dual control

**SDD Checklist:**
- [ ] Spec checkpoint: nostro-aware withdrawals (nostro tracking) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 11.3.7: Beneficiary Bank-Account Registry & Third-Party Deposit Rejection

**Objective:** Implement the verified beneficiary registry (spec §5.23 `bank_accounts`, §8.4 endpoints) that the withdrawal allowlist and `ADDRESS_NOT_ALLOWLISTED` flows reference, plus name-match and third-party deposit rejection (§24 #141, #164). Added 2026-09-15.

**File Locations:** `services/internal/funding/bank_accounts.go`, `services/internal/funding/deposit.go` (extend)

**Implementation:**
1. `bank_accounts` table (migration 040): `bank_account_id`, `account_id`, `currency`, `iban/account_number`, `swift_bic`, `bank_name`, `beneficiary_name`, `status` (PENDING_VERIFICATION|VERIFIED|REJECTED), `verified_at`, `verified_by`.
2. Endpoints: `POST/GET/DELETE /api/v1/funding/bank-accounts` — registration requires KYC T1+; verification via micro-deposit code or bank-statement review (dual control for manual verify). New accounts get the 24h hold (existing policy).
3. Withdrawals may only target `VERIFIED` beneficiaries (existing `ADDRESS_NOT_ALLOWLISTED` error now maps to this registry).
4. Third-party deposit rejection: inbound deposit sender name must fuzzy-match the account holder's KYC legal name — **Jaro-Winkler similarity ≥ 0.85** (single algorithm pinned 2026-09-27, remediation #35; supersedes the prior "≥85% token match" and the inverted ">85% Jaro-Winkler distance" direction in Task 11.3.11 step 2); mismatch → deposit flagged `THIRD_PARTY_DEPOSIT_REJECTED` (spec §23), funds returned, compliance alert.
5. Per-rail cut-off times: each rail carries a daily cut-off (e.g., SWIFT 17:00 UTC, SEPA 16:30 CET); withdrawals requested after cut-off are queued for next value date with `value_date` shown in the response (§24 #164).

**Migration note:** `migrations/040_bank_accounts.up.sql` — `bank_accounts` table (spec §5.23).

**Definition of Done (Acceptance Criteria):**
* [ ] Beneficiary registration + verification flow; withdrawals restricted to VERIFIED accounts
* [ ] Third-party deposit (sender name mismatch) flagged, returned, and alerted (§24 #141)
* [ ] Per-rail cut-off enforced; post-cut-off withdrawals queued with next value_date (§24 #164)
* [ ] 24h new-account hold applies to newly verified beneficiaries

**SDD Checklist:**
- [ ] Spec checkpoint: verified beneficiary registry + third-party rejection (§5.23, §24 #141) — defined first, validated against spec
- [ ] Spec checkpoint: per-rail cut-off enforcement (§24 #164) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: name transliteration variance, corporate vs individual name forms, cut-off boundary at DST transitions

---

### Task 11.3.8: Scoped Kill-Switch

**Objective:** Extend the global kill-switch (Task 11.3.4) with scoped variants per spec §7.2/§24 #152: per-account, per-instrument, and per-FIX-session. Added 2026-09-15.

**File Locations:** `services/internal/admin/killswitch.go` (extend), `core/src/risk/PreTradeChecker.cpp` (scope flags)

**Implementation:**
1. Scopes: `POST /api/v1/admin/kill-switch {scope: GLOBAL|ACCOUNT|INSTRUMENT|FIX_SESSION, target_id}` — Risk Manager+, dual control for GLOBAL; ACCOUNT/INSTRUMENT/FIX_SESSION single-approver allowed.
2. Redis flags: `halt:account:{id}`, `halt:instrument:{symbol}`, `halt:fixsession:{id}`; C++ PreTradeChecker reads account+instrument flags in-process (check 1/2 extension), FIX-session flag enforced at the gateway session layer (flag + admin surface built here; enforcement wired when the FIX gateway lands in Phase-18 — Task 18.3.9 reads `halt:fixsession:{id}`).
3. Scoped kill rejects new orders for the scope only (`TRADING_HALTED` with scope in error detail); cancels still allowed (cancel-only behavior consistent with HALT semantics).
4. All scoped kills logged in admin audit log with scope, target, approver(s).

**Definition of Done (Acceptance Criteria):**
* [ ] Per-account kill: target account rejected, others unaffected
* [ ] Per-instrument kill: symbol rejected, other symbols unaffected
* [ ] Per-FIX-session kill: session's orders rejected; session stays connected for cancels
* [ ] Scoped kills audit-logged; dual control required only for GLOBAL

**SDD Checklist:**
- [ ] Spec checkpoint: scoped kill-switch (§7.2, §24 #152) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: overlapping scopes (account + instrument), kill during active matching, FIX-session kill vs cancel-on-disconnect interaction

---

### Task 11.3.9: Funding Fee Schedule & Currency Conversion Engine

**Objective:** Implement configurable fee schedules for deposits, withdrawals, and cross-currency conversions across all banking rails.

**File Locations:** `services/internal/funding/fee_schedule.go`, `services/internal/funding/currency_conversion.go`

**Implementation:**
1. Fee schedule table: `funding_fee_tiers` with columns `rail` (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2), `currency`, `direction` (DEPOSIT/WITHDRAWAL), `fee_type` (FLAT/PERCENTAGE/FLAT_PLUS_PERCENTAGE), `flat_fee`, `percentage_bps`, `min_fee`, `max_fee`, `free_tier_monthly_count`, `effective_date`.
2. Fee computation: on deposit/withdrawal request, look up applicable tier by (rail, currency, direction, account_tier). Apply formula: `fee = MAX(min_fee, MIN(max_fee, flat_fee + amount × percentage_bps / 10000))`.
3. Free-tier: first N deposits/withdrawals per calendar month are fee-free (configurable per account tier, e.g., T2 gets 5 free withdrawals).
4. Currency conversion: when deposit currency ≠ account currency, convert at PriceOracle mid-rate ± conversion_spread_bps (default: 50bps). Show converted amount and fee separately in UI.
5. Fee GL posting: debit client account, credit funding_fee_revenue account via double-entry (Task 3.3.6).
6. Admin API: CRUD for fee schedules (Phase-07). Effective-date scheduling for fee changes.
7. Display pre-submission fee estimate in Trader UI (Phase-10): `POST /api/v1/funding/fee-estimate {amount, currency, rail, direction}`.

**Definition of Done (Acceptance Criteria):**
* [ ] Fee schedule configurable per rail, currency, direction, and account tier
* [ ] Fee formula applies flat, percentage, min, and max correctly
* [ ] Free-tier monthly allowance tracked and enforced
* [ ] Currency conversion at mid-rate ± spread_bps with clear breakdown
* [ ] Fee GL journal entries posted per double-entry rules
* [ ] Pre-submission fee estimate API returns accurate projection

**SDD Checklist:**
- [ ] Spec checkpoint: funding fee schedule — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: zero-fee rail (SEPA instant < €100k), multi-currency conversion chain, fee exceeds deposit amount (reject)

---

### Task 11.3.10: Withdrawal Whitelist Mode with 24-Hour Timelock & Deactivation Lock

**Objective:** Implement strict withdrawal whitelisting with addition and deactivation safety locks per spec §5.23 and §24 #391 (citation corrected 2026-09-27, remediation #35).

**File Locations:** `services/internal/funding/withdrawal_whitelist.go`, `migrations/078_withdrawal_whitelist_settings.up.sql`

**Implementation:**
1. Account setting `whitelist_only_enabled BOOL` stored in `withdrawal_whitelist_settings` (migration 078).
2. When enabled, withdrawals can only be dispatched to pre-registered bank accounts (IBANs/SSIs).
3. **Addition Timelock:** When a new beneficiary is added, withdrawals to that destination are locked for **24 hours** (`unlocked_at = NOW() + INTERVAL '24 hours'`).
4. **Deactivation Safety Lock:** If the user disables Whitelist-Only mode, withdrawals **for that account** are locked for **24 hours** (`withdrawal_lock_until = NOW() + INTERVAL '24 hours'` on `withdrawal_whitelist_settings` — scoped to the acting account, not platform-wide; remediation #35 supersedes the prior platform-wide lock, a griefing/availability vector that let one user freeze all client fund egress. Re-enable is rate-limited with a documented unlock path).

**Definition of Done (Acceptance Criteria):**
* [ ] Whitelist-only mode restricts withdrawals to approved beneficiaries
* [ ] New beneficiaries subject to 24-hour cooling lock
* [ ] Disabling whitelist mode triggers mandatory 24-hour account-scoped withdrawal lock (platform-wide variant superseded, remediation #35)

**SDD Checklist:**
- [ ] Spec checkpoint: withdrawal whitelist mode with 24-hour addition timelock and 24-hour deactivation safety lock (migration 078, §5.23, §24 #391 — citation corrected, remediation #35)
- [ ] All spec checkpoints pass after implementation

---

### Task 11.3.11: Banking Rails Return Code Mapping & Third-Party Deposit Fraud

**Objective:** Implement comprehensive ISO 20022 and SWIFT return code mapping, wire reject compensation workflows, and third-party deposit fraud quarantine per spec §5.40, §17.12, and §24 #311.

**Implementation:**
1. **Return Code Normalization:** Map payment status reason codes across SEPA (`pacs.002`), FedNow, and SWIFT (`MT199`/`MT299`) to domain errors: `AC01` (incorrect account number) $\rightarrow$ `SETTLEMENT_ACCOUNT_CLOSED`, `AM04` (insufficient funds) $\rightarrow$ `SETTLEMENT_RAIL_REJECTED`, `RR04` (regulatory reason) $\rightarrow$ `SETTLEMENT_RAIL_REJECTED` — mapping corrected 2026-09-27, remediation #35: RR04 is a bank rejection, not a provider outage (`SANCTIONS_SERVICE_UNAVAILABLE` means the external sanctions provider is unreachable and would return a misleading 503). `SETTLEMENT_ACCOUNT_CLOSED`/`SETTLEMENT_RAIL_REJECTED` registered in §23 (emitted here, previously unregistered).
2. **Third-Party Deposit Quarantine & Suspense Routing:** When incoming wire beneficiary/originator name fails fuzzy-match against user's verified KYC legal name (Jaro-Winkler similarity < 0.85 — direction pinned, remediation #35), flag `THIRD_PARTY_DEPOSIT_REJECTED` (HTTP 422), freeze incoming credit, route unreferenced or quarantined deposit funds to the dedicated customer suspense liability account `2099_UNMATCHED_DEPOSITS_SUSPENSE_{CURRENCY}` (migration 108, spec §5.46, §17.16b, Phase-24 Task 24.3.21, remediation #37/#38), and initiate automated return wire (`pacs.004`).
3. **Compensating Ledger Actions:** On outgoing withdrawal rejection, execute automated compensating journal entries returning client balance and releasing locked reserves with audit trail.

**Definition of Done (Acceptance Criteria):**
* [ ] ISO 20022/SWIFT return codes mapped to domain errors
* [ ] Third-party wire mismatches auto-quarantined and returned
* [ ] Failed withdrawals trigger balanced compensating GL journal entries

**SDD Checklist:**
- [ ] Spec checkpoint: Banking rail return code mapping and third-party deposit fraud quarantine (§24 #311) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 11.3.12: Multi-Dimensional Scoped Emergency Kill-Switches

**Objective:** Implement fine-grained scoped emergency kill-switches across counterparty, liquidity provider, and banking rail dimensions without triggering unnecessary global venue halts per spec §7.2b and §24 #409. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/admin/killswitch_service.go`, `services/internal/funding/rail_control.go`

**Implementation:**
1. Extend kill-switch control plane to support three explicit scopes:
   - `SCOPE_COUNTERPARTY`: Immediately halts order entry and cancels resting orders for a targeted Prime Broker, broker-dealer, or institutional user ID.
   - `SCOPE_LP`: Rejects incoming FIX Mass Quotes (Tag 35=i) from designated Liquidity Providers (LPs) quoting stale prices, while leaving retail and participant CLOB trading fully operational.
   - `SCOPE_RAIL`: Suspends deposit and withdrawal processing for a specific troubled payment rail (e.g., SEPA or FedNow) while keeping SWIFT/CHAPS operational.
2. Control messages publish to Aeron control topic `exchange:control:killswitch` and replicate to C++ matching shards in < 5µs.

**Definition of Done (Acceptance Criteria):**
* [ ] Scoped counterparty kill-switch cancels target user's orders within 10µs without affecting other venue participants
* [ ] LP kill-switch halts LP quotes on Tag 35=i while preserving firm CLOB continuous trading
* [ ] Rail suspension halts target rail operations with `SETTLEMENT_RAIL_REJECTED` while sibling rails operate normally

**SDD Checklist:**
- [ ] Spec checkpoint: multi-dimensional scoped emergency kill-switches (§24 #409) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 11.4 Deliverables

- Banking rails integration (SWIFT, SEPA, FedNow, ACH, CHAPS, TARGET2)
- Withdrawal flow with 15min confirmation + review tiers
- Deposit flow with anti-fraud tiers
- Trading suspension / kill-switch
- Market statistics
- Nostro-aware withdrawals (nostro tracking)
- Beneficiary bank-account registry + third-party deposit rejection + per-rail cut-offs
- Scoped kill-switch (account / instrument / FIX session)
- Banking rail return code mapping & automated deposit fraud quarantine (Task 11.3.11)
- Multi-dimensional scoped emergency kill-switches (Task 11.3.12)

---

## 11.5 Dependencies

- Phases 3, 4, 5, 7, 10

---

## 11.6 Duration Estimate

7–10 days (supersedes prior 6–8 — Task 11.3.12 absorbed in range, remediation #37; prior supersedes 6–8 — duration itemization completed 2026-09-27, remediation #35):
- Task 11.3.1 (Banking rails): 2 days
- Task 11.3.2 (Withdrawal): 1 day
- Task 11.3.3 (Deposit): 1 day
- Task 11.3.4 (Kill-switch): 0.5 day
- Task 11.3.5 (Stats): 0.5 day
- Task 11.3.6 (Nostro): 0.5 day
- Task 11.3.7 (Bank registry/3rd-party/cut-offs): 0.5 day
- Task 11.3.8 (Scoped kill-switch): 0.5 day
- Task 11.3.9 (Funding fee schedule): 0.5 day (added to itemization, remediation #35)
- Task 11.3.10 (Withdrawal whitelist mode): 0.5 day (added to itemization, remediation #35)
- Task 11.3.11 (Rail error mapping & fraud quarantine): 0.5 day
- Task 11.3.12 (Multi-dimensional scoped kill-switch): 0.5 day (absorbed in range, remediation #37)
- Testing: 0.5 day

---

## 11.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | SWIFT MT103/MT202 messages generated correctly |
| 2 | SEPA SCT and SCT Inst work |
| 3 | FedNow instant payment works |
| 4 | ACH file generation correct |
| 5 | CHAPS same-day GBP payment works |
| 6 | TARGET2 real-time EUR payment works |
| 7 | Nostro account per currency per rail |
| 8 | Withdrawal creates with 15min confirmation window |
| 9 | Confirmation token validated within window; expired → AUTO_CANCELLED |
| 10 | Review tiers: <$10K auto / $10K–$50K standard / >$50K PENDING_REVIEW+4h |
| 11 | 30min cooldown same bank account enforced |
| 12 | 24h hold for new bank accounts |
| 13 | Withdrawal caps enforced (daily, hourly, exchange-wide) |
| 14 | Deposit instructions return bank details per currency |
| 15 | Deposit detected via polling or webhook |
| 16 | Anti-fraud tiers: <$10K auto / $10K–$50K double-confirm / >$50K PENDING_REVIEW+4h |
| 17 | Dual-source bank confirmation required |
| 18 | Kill-switch rejects all new orders (TRADING_HALTED) |
| 19 | Cancels, reads, WS survive kill-switch |
| 20 | Dual control required to activate/reset kill-switch |
| 21 | 24h market statistics computed correctly per symbol |
| 22 | Stats endpoints return correct data |
| 23 | Nostro balances tracked per currency |
| 24 | Low nostro balance alert fires |
| 25 | Replenishment requires dual control |
| 26 | Withdrawals restricted to VERIFIED beneficiaries (§24 #141) |
| 27 | Third-party deposit (sender name mismatch) flagged + returned + alerted |
| 28 | Per-rail cut-off enforced; post-cut-off queued with next value_date (§24 #164) |
| 29 | Scoped kill-switch: account/instrument/FIX-session scopes reject only their target (§24 #152) |
| 30 | Scoped kills audit-logged; dual control only for GLOBAL scope |
| 31 | Funding fee schedule configurable per rail/currency/direction/tier; formula applies flat + percentage with min/max caps (§24 #224) |
| 32 | Currency conversion at PriceOracle mid-rate ± configurable spread_bps; fee GL entries posted |
| 33 | Withdrawal whitelist mode locks new destinations for 24h and imposes an account-scoped 24h lock on deactivation (Task 11.3.10, §24 #391 — citation and lock scope corrected 2026-09-27, remediation #35; supersedes prior citation of §24 #294, a UI criterion; stable ID `T11-012`) |
| 34 | Banking rail rejection codes (AC01/AM04/RR04) map to domain errors; third-party deposit fraud auto-quarantines and initiates return wire (§24 #311) |
| 35 | Multi-dimensional scoped kill-switches halt counterparty, LP quoting, or specific banking rail without global venue interruption (§24 #409) |
