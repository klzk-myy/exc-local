# Phase 14 — Extended Production Features

**Duration:** 15–19 days (unchanged; §14.4/§14.6 itemizations completed 2026-09-27, remediation #35: Tasks 14.3.10/14.3.11 added) (unchanged; §14.4/§14.6 itemizations completed 2026-09-27, remediation #35: Tasks 14.3.10/14.3.11 added) (supersedes 14–18 — Task 14.3.16 retail target-market governance added 2026-09-27, remediation #30; prior supersedes 11–14 — Tasks 14.3.13–14.3.15 added 2026-09-27, remediation #29)
**Dependencies:** Phases 2–13.5
**Spec Reference:** §6 (Order Types), §14.2 (KYC Tiers), §24 (Acceptance Criteria)

---

## 14.1 Objectives

Extended production features: OCO orders, auto-halt on anomaly, testnet environment, KYC lifecycle management, and additional production hardening.

---

## 14.2 Prerequisites

- Phase 13.5 complete (security audit passed)

---

## 14.3 Tasks

### Task 14.3.1: OCO (One-Cancels-Other) Orders

**Objective:** Implement OCO order type.

**File Locations:** `core/src/matching/OcoOrder.cpp`, `services/internal/api/oco.go`

**Implementation:**
1. OCO: two orders linked; when one fills, the other cancels.
2. `POST /api/v1/orders/oco` — submit OCO pair.
3. C++ core: when either order fills, atomically cancel the other.
4. WAL: OCO link recorded for recovery.

**Definition of Done (Acceptance Criteria):**
* [ ] OCO pair submitted and linked
* [ ] When one fills, other cancels atomically
* [ ] OCO link survives recovery (WAL)

**SDD Checklist:**
- [ ] Spec checkpoint: OCO atomic cancel — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.2: Auto-Halt on Anomaly

**Objective:** Implement automatic trading halt on detected anomalies.

**File Locations:** `services/internal/risk/auto_halt.go`

**Implementation:**
1. Anomaly detection: price spike, volume spike, latency spike, error rate spike.
2. Auto-halt: instrument suspended for 5min.
3. Notification: P1 alert + user notification.
4. Auto-resume: after 5min if anomaly cleared.

**Definition of Done (Acceptance Criteria):**
* [ ] Anomaly detection triggers auto-halt
* [ ] Instrument suspended for 5min
* [ ] P1 alert + user notification
* [ ] Auto-resume after 5min if cleared

**SDD Checklist:**
- [ ] Spec checkpoint: auto-halt on anomaly — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.3: Testnet Environment

**Objective:** Implement separate testnet with reset capability.

**File Locations:** `services/internal/api/testnet.go`

**Implementation:**
1. Separate deployment: testnet.exchange.com.
2. Test accounts with preset balances.
3. `POST /api/v1/test/reset` — reset balances, orders, positions.
4. Rate limited: 1 reset per 5min per account.
5. No real banking rails (simulated deposits/withdrawals).

**Definition of Done (Acceptance Criteria):**
* [ ] Testnet deployment separate from production
* [ ] Test reset works
* [ ] Simulated funding (no real banking)
* [ ] Rate limited

**SDD Checklist:**
- [ ] Spec checkpoint: testnet with reset — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.4: KYC Lifecycle Management

**Objective:** Implement full KYC lifecycle with re-verification.

**File Locations:** `services/internal/compliance/kyc_lifecycle.go`

**Implementation:**
1. Tiers: T0 (no verification), T1 (basic), T2 (full), institutional (manual review).
2. T0 → no trading; T1 → limited (lower leverage, lower limits); T2 → full; institutional → negotiated.
3. Re-verification: 12 months for T2, 24 months for institutional.
4. Auto-downgrade: if re-verification overdue, downgrade to T1.
5. Document storage: S3 encrypted.
6. `POST /api/v1/kyc/submit` — submit documents (owned by Phase-12 Task 12.3.4; this task's boundary note disclaims re-implementing the submission flow — endpoint reference aligned, remediation #35).
7. `GET /api/v1/kyc/status` — check status.
8. Admin: `POST /api/v1/admin/kyc/{id}/approve` and `/reject`.

**Boundary note (Phase 14 extends Phase 12):** Phase 12 (Task 12.3.4) owns the **KYC submission flow**: user uploads documents, system stores them in S3, assigns initial tier. Phase 14 owns the **KYC lifecycle management**: admin approve/reject workflow, auto-downgrade on overdue re-verification, re-verification triggers (document expiry, risk score change, regulatory update). This task extends the Phase 12 submission endpoint with admin workflow and lifecycle automation — it does not re-implement the submission flow.

**Definition of Done (Acceptance Criteria):**
* [ ] 4 KYC tiers with correct trading limits
* [ ] Re-verification: 12mo T2, 24mo institutional
* [ ] Auto-downgrade on overdue re-verification
* [ ] Documents stored encrypted in S3
* [ ] Admin approve/reject workflow

**SDD Checklist:**
- [ ] Spec checkpoint: KYC lifecycle T0/T1/T2/institutional — defined first, validated against spec
- [ ] Spec checkpoint: re-verification 12mo/24mo with auto-downgrade — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.5: PostgreSQL PITR Hardening

**Objective:** Harden PostgreSQL PITR with verified RPO/RTO.

**File Locations:** `deploy/postgresql/`

**Implementation:**
1. `wal_level = replica`, `archive_mode = on`, `archive_command` to S3.
2. Nightly base backup.
3. RPO ≤ 15s, RTO ≤ 5min (supersedes earlier "≤ 5 min / ≤ 30 min" figures — see Phase-09 Task 9.3.4).
4. Monthly PITR drill.

**Definition of Done (Acceptance Criteria):**
* [ ] PostgreSQL WAL archived to S3
* [ ] Nightly base backup
* [ ] RPO ≤ 15s verified
* [ ] RTO ≤ 5min verified
* [ ] Monthly PITR drill documented

**SDD Checklist:**
- [ ] Spec checkpoint: PostgreSQL PITR RPO ≤ 15s / RTO ≤ 5min (supersedes prior ≤ 5min / ≤ 30min) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.6: Additional Production Hardening

**Objective:** Additional hardening: rate limit alerts, slow query logging, connection pool tuning.

**File Locations:** `services/internal/middleware/`, `deploy/postgresql/`

**Implementation:**
1. Rate limit alerts: P2 when >80% of tier limit.
2. Slow query logging: PostgreSQL `log_min_duration_statement = 100ms`.
3. Connection pool: PgBouncer with 100 max connections.
4. Redis: maxmemory policy, eviction alerts.

**Definition of Done (Acceptance Criteria):**
* [ ] Rate limit alerts at 80% threshold
* [ ] Slow queries logged > 100ms
* [ ] PgBouncer configured
* [ ] Redis maxmemory + eviction alerts

**SDD Checklist:**
- [ ] Spec checkpoint: production hardening — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.7: MiFID II Client Categorization & Appropriateness

**Objective:** Implement client categorization (RETAIL / PROFESSIONAL / ELIGIBLE_COUNTERPARTY) per spec §5.2/§14.2 and the appropriateness test that gates leveraged products (§24 #132). KYC tiers (Task 14.3.4) verify identity; this task classifies regulatory category — orthogonal axes. Added 2026-09-15.

**File Locations:** `services/internal/compliance/categorization.go`, `services/internal/api/account.go` (extend)

**Implementation:**
1. `accounts.client_category` column + `appropriateness_assessments` table (migration 042): `assessment_id`, `account_id`, `instrument_class`, `outcome` (PASS|FAIL), `answers_json`, `assessed_at`, `expires_at`.
2. Category assignment: default RETAIL on onboarding; PROFESSIONAL/ECP require admin workflow (Compliance Officer) with documented eligibility evidence (MiFID II Annex II qualitative + quantitative tests); category changes audit-logged.
3. Category drives leverage caps (ESMA 30:1/20:1/10:1 retail vs negotiated professional) and product gating: binary options (Phase-22 Task 22.3.6) are PROFESSIONAL/ECP-only; retail blocked with `PRODUCT_NOT_PERMITTED`.
4. Appropriateness test required before first order on leveraged/derivative instruments; FAIL → product class blocked; assessments expire after 12 months.
5. Negative-balance protection entitlement: RETAIL accounts get NBP flag (enforced in Phase-19 Task 19.3.9).

**Migration note:** `migrations/042_client_categorization.up.sql` — `accounts.client_category`, `appropriateness_assessments` (spec §5.2/§14.2).

**Definition of Done (Acceptance Criteria):**
* [ ] client_category assigned at onboarding; upgrade requires Compliance Officer + evidence
* [ ] Appropriateness test gates leveraged/derivative products; FAIL blocks with PRODUCT_NOT_PERMITTED
* [ ] Binary options rejected for RETAIL clients (§24 #132)
* [ ] Category changes + assessments audit-logged; 12-month assessment expiry enforced

**SDD Checklist:**
- [ ] Spec checkpoint: MiFID client categorization + appropriateness (§14.2, §24 #132) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: category downgrade with open positions, expired assessment mid-session, ECP onboarding (no appropriateness test required)

---


### Task 14.3.8: PAMM/MAM & Copy Trading Framework

**Objective:** Implement Percentage Allocation Management Module (PAMM) and Multi-Account Manager (MAM) to support retail copy trading.

**File Locations:** `services/internal/features/14_3_8.go`

**Implementation:**
1. Create master-sub account relationships for PAMM.
2. Pro-rata allocation of trades from master to sub-accounts.
3. Real-time copy trading engine.
4. **Internal Ledger Taxonomy & Fiat Limit Segregation (added 2026-09-27, remediation #38):**
   - Investor capital allocations and redemptions into PAMM manager pools MUST be recorded using dedicated internal transaction types (`PAMM_INVEST`, `PAMM_REDEEM`, `PAMM_FEE_PERF`, `PAMM_FEE_MGMT`) on internal investment sub-ledgers.
   - Recording PAMM pool subscriptions as generic `WITHDRAWAL` or `DEPOSIT` transactions is strictly prohibited.
   - Internal PAMM investments MUST NOT deduct from or count against the investor's daily fiat banking withdrawal allowance (`daily_fiat_withdrawal_allowance` / KYC T0/T1/T2 daily cash caps), preserving external SWIFT/SEPA banking rail limits for actual external cash movements.

**Definition of Done (Acceptance Criteria):**
* [ ] PAMM/MAM & Copy Trading Framework implementation completed
* [ ] Tests passing for PAMM/MAM & Copy Trading Framework

**SDD Checklist:**
- [ ] Spec checkpoint: PAMM/MAM & Copy Trading Framework — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.9: Account Closure & Offboarding Lifecycle

**Objective:** Implement the account exit workflow per spec §12.5 — client-initiated and forced closure with preconditions, residual funds sweep, and full audit trail (§24 #204). Added 2026-09-16 (gap audit remediation #5).

**File Locations:** `services/internal/account/closure.go`, `migrations/063_account_closures.up.sql`

**Implementation:**
1. `POST /api/v1/account/close` (TOTP 2FA required): returns `ACCOUNT_CLOSE_BLOCKED` with reasons while any open position, open order, pending settlement, or pending funding transaction exists.
2. Residual sweep: remaining balances withdrawn to a verified beneficiary account via the Phase-11 withdrawal pipeline (24h hold applies to newly registered beneficiaries); GL-balanced postings (Task 3.3.6).
3. Closure transition: state → `CLOSED`; revoke all API keys, terminate active sessions/WS, remove from margin-scanner scans; account remains queryable read-only (statements/history) for the retention period.
4. Forced closure (Compliance Officer+, dual control): identical pipeline minus client confirmation, reason mandatory.
5. `account_closures` table (migration 063): `closure_id`, `account_id`, `reason`, `initiated_by`, `preconditions_snapshot` (jsonb), `sweep_refs`, `completed_at`.
6. Retention interplay: KYC/trade/ledger data retained per schedule; GDPR erasure honored except legal-hold carve-outs (§14.7, §14.8). Reopening prohibited — new registration required.

**Definition of Done (Acceptance Criteria):**
* [ ] Closure rejected `ACCOUNT_CLOSE_BLOCKED` while any position/order/settlement is open (§24 #204)
* [ ] Residual balance sweep posts GL-balanced entries and settles via banking rails
* [ ] API keys revoked + sessions terminated at closure; account excluded from margin scans
* [ ] `account_closures` audit record complete for both client-initiated and dual-control forced closures

**SDD Checklist:**
- [ ] Spec checkpoint: account closure & offboarding (§12.5, §24 #204) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: closure requested during pending withdrawal; forced closure with open positions (liquidate first via Phase-19 flow); GDPR erasure on closed account

---

### Task 14.3.10: Compliance Hold Workflow

**Objective:** Implement the compliance-triggered account freeze workflow (spec §24 #219). Added 2026-09-17 (gap analysis remediation #6).

**File Locations:** `services/internal/compliance/hold_workflow.go`

**Implementation:**
1. Trigger: sanctions hit (Phase-21 Task 21.3.1), PEP match (Task 21.3.11), or Compliance Officer manual action.
2. Auto-actions on compliance hold: account state → `FROZEN`; all resting orders cancelled; withdrawal pipeline blocked; new order submission rejected (`ACCOUNT_FROZEN`); existing positions NOT liquidated (preserve evidence).
3. Compliance Officer review: dashboard shows FROZEN accounts with trigger reason, evidence, and timeline. Disposition options: (a) release (clear false positive, restore `ACTIVE`), (b) escalate to SAR filing (Task 21.3.3), (c) escalate to account closure (Task 14.3.9).
4. SLA: high-confidence sanctions hits reviewed within 4 hours; all compliance holds reviewed within 24 hours.
5. Audit trail: all hold/release/escalation actions logged in `admin_audit_log` with Compliance Officer identity and justification.
6. Integration: links to kill-switch (Task 11.3.8), SAR filing (Task 21.3.3), account closure (Task 14.3.9), comms recordings (Task 21.3.20).

**Definition of Done (Acceptance Criteria):**
* [ ] Compliance hold auto-freezes account, cancels resting orders, blocks withdrawals
* [ ] Compliance Officer review dashboard provides release/escalate disposition
* [ ] Audit trail logs all hold/release/escalation events

**SDD Checklist:**
- [ ] Spec checkpoint: Compliance Hold Workflow (§24 #219) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.11: Cooling-off period / responsible trading self-exclusion

Cooling-off period / responsible trading self-exclusion — `POST /api/v1/account/cooling-off` with `duration` parameter ∈ {1d, 3d, 7d, 30d}. Strictly irrevocable once activated — no admin override, no support override, no early termination. Effects: all open leveraged/margin positions market-closed, new margin/leveraged order entry rejected with `COOLING_OFF_ACTIVE`, spot conversions and withdrawals remain available. Stored in `cooling_off_periods` table (migration 070): `{id, user_id, duration, started_at, expires_at, acknowledged_at}`. Requires legal acknowledgment checkbox before activation. Regulatory compliance: ESMA (PS 19/18), FCA, ASIC, CySEC responsible trading mandates.

**SDD Checklist:**
- [ ] Spec checkpoint: cooling-off is irrevocable for its term and blocks leveraged trading (§24 #273) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.12: Cooling-Off Invariant Enforcement & Webhook Delivery Retries

**Objective:** Implement strict fail-closed enforcement of responsible trading cooling-off periods and resilient webhook delivery retry pipelines per spec §2.7, §8.7, §12.6, and §24 #315.

**Implementation:**
1. **Irrevocable Cooling-Off Enforcement:** When cooling-off is activated, execute atomic saga: cancel all resting limit/stop orders on leveraged instruments, submit market orders to flatten open margin positions, and set account flag `COOLING_OFF_ACTIVE` preventing leveraged order submission. Block all administrative or programmatic attempts to shorten or bypass the active window.
2. **Webhook Resilient Delivery Pipeline:** Implement worker consuming from NATS JetStream webhook stream. Deliver events with HMAC-SHA256 signature; on HTTP 5xx or network timeout, retry with exponential backoff (1s, 5s, 30s, 5m, 30m; max 5 attempts).
3. **Dead-Letter Storage:** Webhooks failing all 5 attempts move to `webhook_dead_letters` table for inspection and manual re-transmission.

**Definition of Done (Acceptance Criteria):**
* [ ] Cooling-off irrevocably locks leveraged trading and flattens positions
* [ ] Webhook deliveries retry with exponential backoff up to 5 times
* [ ] Exhausted webhooks persist in dead-letter table with error payload

**SDD Checklist:**
- [ ] Spec checkpoint: Cooling-off invariant enforcement and webhook delivery retries (§24 #315) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 14.3.13: Account Product Profiles (Advantage-Style Segmentation)

**Objective:** Bind each account to an admin-defined product profile governing pricing plan, product scope and unit denomination, per spec §5.41 and §24 #369. Added 2026-09-27 (FXTM-parity remediation #29).

**File Locations:** `services/internal/account/products.go`, `migrations/095_account_products_swapfree.up.sql`

**Implementation:**
1. `account_product_profiles` table (migration 095): `profile_id`, `code` (admin-defined; seed `STANDARD` + `CENT`), `pricing_plan` (`SPREAD_MARKUP`|`RAW_SPREAD_COMMISSION`, consumed by Phase-03 Task 3.3.13), `instrument_scope` (allowlisted classes, e.g. SPOT-only vs full derivatives), `subunit_divisor` (1 standard, 100 cent — consumed by Phase-03 Task 3.3.21), `min_deposit`, `status` (ACTIVE|RETIRED).
2. `accounts.product_profile_id` FK (migration 095); assigned `STANDARD` at onboarding; profile changes rejected with `INVALID_REQUEST` while any open position, resting order or pending settlement exists (checked against Phase-03/Phase-19 state, same read path as Task 14.3.9 preconditions).
3. Enforcement: order entry rejects out-of-scope instruments with existing `PRODUCT_NOT_PERMITTED`; fee engine resolves `pricing_plan` from the profile (account override removed — profile is the single source).
4. Admin: `POST/PUT /api/v1/admin/product-profiles` and `PUT /api/v1/admin/accounts/{id}/product-profile` (pricing/scope changes dual-controlled, audit-logged); venue-info (Phase-05 Task 5.3.44) publishes per-profile scope so clients discover eligibility.

**Definition of Done (Acceptance Criteria):**
* [ ] Out-of-scope instruments rejected; fees follow the profile pricing plan
* [ ] Profile switch with open exposure rejected; retired profiles block new assignment

**SDD Checklist:**
- [ ] Spec checkpoint: product profiles gate pricing, scope and denomination with guarded switching (§24 #369) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: profile retired with live holders (grandfathered, no new assignment); scope narrowed below held positions (close-only, no new opens)

---

### Task 14.3.14: Copy-Trading Product Layer (Discovery, Follows, Safety Mode, Profit Share)

**Objective:** Productize the Task 14.3.8 PAMM/MAM engine into manager discovery, investor follows with safety-mode scaling, and high-water-mark profit-share settlement, per spec §12.9 and §24 #372. Added 2026-09-27 (FXTM-parity remediation #29).

**File Locations:** `services/internal/copy/discovery.go`, `services/internal/copy/follows.go`, `services/internal/copy/profit_share.go`, `migrations/097_copy_trading_product.up.sql`

**Implementation:**
1. `strategy_profiles` table (migration 097): manager `account_id`, display name, description, `status` (INCUBATING|LISTED|SUSPENDED). LISTED requires appropriateness PASS (Task 14.3.7) + ≥30 days INCUBATING track record. Ranking endpoint `GET /api/v1/copy/strategies` serves computed-only stats (return from fills, max drawdown, follower count, AUM via Phase-20/23 reads) — self-reported performance is never stored or served.
2. `copy_follows` table: investor `account_id`, `strategy_id`, allocation notional, `safety_mode` (FULL|HALF_RISK — HALF_RISK scales child quantities ×0.5), investor stop-loss cap, `status`. `POST /api/v1/copy/follows` / `DELETE` (unfollow cancels pending child orders; open copied positions stay with the investor, disclosed at follow time). Copy execution reuses the 14.3.8 pro-rata engine; safety-mode scaling applies before min-notional checks, sub-threshold children are skipped with notice (never silently dropped).
3. `high_water_marks` per follow + `profit_share_accruals`: period P&L computed at month-end (or unfollow); `profit_share_pct` manager-set, admin-capped (default ceiling 50%); payout accrues only above HWM via balanced GL lines (investor P&L → manager revenue, Task 3.3.6); HWM ratchets on payout, never resets on loss months. Disputes route to support tickets (Phase-07 Task 7.3.7).
4. Manager misconduct (scope breach, stat manipulation attempt) suspends the strategy (SUSPENDED: no new follows, existing follows continue) via Compliance Officer action, audit-logged.

**Definition of Done (Acceptance Criteria):**
* [ ] Discovery ranks by computed stats only; gating (appropriateness + incubation) enforced
* [ ] HALF_RISK halves child quantities; profit share accrues only above HWM with balanced GL lines

**SDD Checklist:**
- [ ] Spec checkpoint: copy discovery, safety-scaled follows and HWM profit-share over the PAMM engine (§24 #372) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: manager strategy deleted with live followers (SUSPENDED, no delete); loss month (no accrual, HWM holds); safety-mode rounding below min notional

---

### Task 14.3.15: Swap-Free (Islamic) Verification Lifecycle

**Objective:** Gate zero-financing treatment behind a request → attestation → Compliance-approval lifecycle with revocation and abuse guards, per spec §12.8 and §24 #373. Added 2026-09-27 (FXTM-parity remediation #29).

**File Locations:** `services/internal/account/swapfree.go`, `migrations/095_account_products_swapfree.up.sql` (shared with Task 14.3.13)

**Implementation:**
1. `swapfree_verifications` table (migration 095): `account_id`, attestation ref (stored via the Phase-12 Task 12.3.4 document pipeline), `status` (PENDING|APPROVED|REVOKED), verifier, `decided_at`. `accounts.swapfree_status` mirrors the outcome (`STANDARD`|PENDING|VERIFIED|REVOKED).
2. `POST /api/v1/account/swap-free/request` (attestation ref required) → PENDING; Compliance Officer `POST /api/v1/admin/swap-free/{id}/approve|reject` (single approver + audit log; revocation same path). Requests allowed with open positions — treatment changes apply prospectively from approval, never retroactively.
3. Enforcement: VERIFIED accounts accrue zero Tom-Next financing per §5.21a (Phase-03 Task 3.3.19 consumes the flag); REVOKED resumes standard accrual prospectively with no back-billing (disclosed in the endpoint response and statements).
4. Abuse guard: >2 VERIFIED→REVOKED transitions per 12 months auto-flags the account for compliance-hold review (Task 14.3.10); cooling-off (Tasks 14.3.11–12) is orthogonal and unaffected.
5. Surfaces: Trader UI Settings → Swap-Free panel and the Phase-07 admin verification queue consume these endpoints (no new UI tasks — API contract owned here).

**Definition of Done (Acceptance Criteria):**
* [ ] Zero financing applies only while VERIFIED; revocation resumes standard accrual with no back-billing
* [ ] Repeat-flip abuse routes to compliance-hold review; all transitions audit-logged

**SDD Checklist:**
- [ ] Spec checkpoint: swap-free verification lifecycle with prospective enforcement and abuse guards (§24 #373) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: request with open positions (prospective only); revoke on rollover day (next cycle); attestation document expired (re-verification required)

---

### Task 14.3.16: Retail Product Target-Market Governance

**Objective:** Attach a MiFID II target market (positive + negative) to every product profile and review it periodically, per spec §14.12 and §24 #376. Added 2026-09-27 (reporting-sufficiency remediation #30). Venue rulebook/product approvals (Task 21.3.15, §24 #174) govern members; nothing governs retail distribution — this closes that side.

**File Locations:** `services/internal/account/target_market.go`, `migrations/099_product_target_markets.up.sql`

**Implementation:**
1. `product_target_markets` table (migration 099): `profile_id` (FK → Task 14.3.13 profiles), `client_category` (RETAIL-covered; PROFESSIONAL/ECP recorded for completeness), `knowledge_experience` band, `risk_tolerance` band, `negative_target` (e.g. no-loss-capacity retail for leveraged derivatives), `distribution_strategy` (advised/non-advised), `review_due_at`.
2. Order-entry gate: RETAIL orders on instruments outside the account's positive target (or inside its negative target) reject with existing `PRODUCT_NOT_PERMITTED`, citing the target-market reason; appropriateness (Task 14.3.7) remains the knowledge check, target-market the product check — both must pass.
3. Periodic review: `review_due_at` ≤ 12 months; overdue suspends new RETAIL opens on that profile (close-only) with Compliance Officer alert; review workflow re-approves or narrows the market, audit-logged.
4. Copy-trading strategies (Task 14.3.14) inherit the manager account's profile market; followers blocked where the strategy sits in their negative target.

**Definition of Done (Acceptance Criteria):**
* [ ] Out-of-target retail orders rejected with reason; both appropriateness and target-market must pass
* [ ] Overdue review forces close-only with alert; reviews audit-logged

**SDD Checklist:**
- [ ] Spec checkpoint: per-profile target markets with dual-gate enforcement and periodic review (§24 #376) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: category upgrade RETAIL→PROFESSIONAL mid-review-cycle; profile retired with live target rows; copy-follow where strategy drifts out of follower target

---

## 14.4 Deliverables

- OCO orders
- Auto-halt on anomaly
- Testnet environment
- KYC lifecycle management
- PostgreSQL PITR hardening
- Additional production hardening
- MiFID II client categorization + appropriateness testing + product gating
- PAMM/MAM and Copy Trading engine (Task 14.3.8)
- Account closure & offboarding lifecycle (Task 14.3.9)
- Cooling-off invariant validation & webhook exponential retry queue (Task 14.3.12)
- Account product profiles with pricing/scope/denomination segmentation (Task 14.3.13, migration 095)
- Copy-trading product layer: discovery, safety-scaled follows, HWM profit share (Task 14.3.14, migration 097)
- Swap-free (Islamic) verification lifecycle with prospective enforcement (Task 14.3.15, migration 095)
- Retail product target-market governance with dual-gate enforcement (Task 14.3.16, migration 099)

---

## 14.5 Dependencies

- Phases 2–13.5

---

## 14.6 Duration Estimate

15–19 days (supersedes 14–18 — Task 14.3.16 target-market governance added 2026-09-27, remediation #30; prior supersedes 11–14 — Tasks 14.3.13–14.3.15 added 2026-09-27, remediation #29):
- Task 14.3.1 (OCO): 1.5 days
- Task 14.3.2 (Auto-halt): 1 day
- Task 14.3.3 (Testnet): 1 day
- Task 14.3.4 (KYC lifecycle): 2 days
- Task 14.3.5 (PITR): 1 day
- Task 14.3.6 (Hardening): 1.5 days
- Task 14.3.7 (Client categorization): 1 day
- Task 14.3.8 (PAMM/MAM Copy Trading): 2 days
- Task 14.3.9 (Account closure & offboarding): 1 day
- Task 14.3.10 (Compliance hold workflow): 1 day (added to itemization, remediation #35)
- Task 14.3.11 (Cooling-off / responsible-trading self-exclusion): 0.5 day (added to itemization, remediation #35)
- Task 14.3.12 (Cooling-off enforcement & webhook retries): 0.5 day
- Task 14.3.13 (Account product profiles): 1 day
- Task 14.3.14 (Copy-trading product layer): 2 days
- Task 14.3.15 (Swap-free verification lifecycle): 1 day
- Task 14.3.16 (Retail target-market governance): 1 day
- Testing: 1.5 days

## 14.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | OCO pair submitted and linked |
| 2 | When one OCO order fills, other cancels atomically |
| 3 | OCO link survives recovery (WAL) |
| 4 | Anomaly detection triggers auto-halt |
| 5 | Instrument suspended for 5min on auto-halt |
| 6 | P1 alert + user notification on auto-halt |
| 7 | Auto-resume after 5min if anomaly cleared |
| 8 | Testnet deployment separate from production |
| 9 | Test reset works (balances, orders, positions) |
| 10 | Simulated funding on testnet (no real banking) |
| 11 | Test reset rate limited (1 per 5min) |
| 12 | 4 KYC tiers (T0, T1, T2, institutional) with correct trading limits |
| 13 | T0 → no trading; T1 → limited; T2 → full; institutional → negotiated |
| 14 | Re-verification: 12 months T2, 24 months institutional |
| 15 | Auto-downgrade on overdue re-verification (§24 #101) |
| 16 | KYC documents stored encrypted in S3 |
| 17 | Admin approve/reject KYC workflow |
| 18 | PostgreSQL WAL archived to S3 |
| 19 | Nightly base backup |
| 20 | RPO ≤ 15s verified |
| 21 | RTO ≤ 5min verified (supersedes prior ≤ 5min / ≤ 30min) |
| 22 | Monthly PITR drill documented |
| 23 | Rate limit alerts at 80% threshold |
| 24 | Slow queries logged > 100ms |
| 25 | PgBouncer configured (100 max connections) |
| 26 | Redis maxmemory + eviction alerts |
| 27 | KYC caps enforced per spec §14.2: T1 $10K/day, T2 $100K/day withdrawal; T0 no trading/withdrawal (§24 #27) |
| 28 | client_category (RETAIL/PROFESSIONAL/ECP) assigned at onboarding; upgrade via Compliance Officer with evidence (§24 #132) |
| 29 | Appropriateness test gates leveraged/derivative instruments; FAIL → PRODUCT_NOT_PERMITTED |
| 30 | Binary options rejected for RETAIL clients; assessments expire after 12 months |
| 31 | RETAIL accounts flagged for negative-balance protection (enforced Phase-19 Task 19.3.9) |
| 32 | PAMM/MAM accounts allocate trades pro-rata to sub-accounts |
| 33 | Copy trading engine replicates master trades in real-time |
| 34 | Account closure blocked (ACCOUNT_CLOSE_BLOCKED) while positions/orders/settlements open; residual funds swept, API keys revoked, audit record complete (§24 #204) |
| 35 | Forced closure executes under dual control; closed accounts retain GDPR-compliant records with legal-hold carve-outs and cannot be reopened |
| 36 | Compliance hold workflow auto-freezes accounts on sanctions/PEP hit, blocks withdrawals, and provides compliance officer review dashboard (§24 #219) |
| 37 | Cooling-off is irrevocable for its selected term, closes leveraged exposure, and blocks new leveraged trading (§24 #273) |
| 38 | Cooling-off activation atomically cancels margin orders and market-closes open positions; webhook delivery retries with exponential backoff and dead-lettering (§24 #315) |
| 39 | Product profiles gate pricing plan, instrument scope and unit denomination; guarded switching with open-exposure rejection (§24 #369) |
| 40 | Copy discovery ranks computed-only stats; HALF_RISK safety mode scales children; profit share accrues above HWM with balanced GL lines (§24 #372) |
| 41 | Swap-free VERIFIED status gates zero-financing treatment prospectively; revocation resumes standard accrual without back-billing; flip abuse routes to review (§24 #373) |
| 42 | Per-profile retail target markets dual-gate order entry with appropriateness; overdue review forces close-only (§24 #376) |
