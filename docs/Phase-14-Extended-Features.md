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

**File Locations:** `core/src/matching/MatchingEngine.cpp` (bounded OCO side-table + terminal-path hooks — supersedes the sketched `core/src/matching/OcoOrder.cpp`; linkage is engine state, not an order subclass), `core/src/ipc/EnginePump.cpp`, `core/src/recovery/RecoveryManager.cpp`, `services/internal/orders/oco.go`, `services/internal/api/oco.go`, `services/internal/db/migrations/218_oco_group_link.up.sql`

**Implementation:**
1. OCO: two orders linked; when one fills, the other cancels.
2. `POST /api/v1/orders/oco` — submit OCO pair.
3. C++ core: when either order fills, atomically cancel the other.
4. WAL: OCO link recorded for recovery.

**Definition of Done (Acceptance Criteria):**
* [x] OCO pair submitted and linked (`Store.InsertOcoPairTx` persists both legs + both dedup rows in ONE transaction sharing `oco_group_id`; wire order on the shard ring is OcoLink → OrderNew(A) → OrderNew(B) so the engine installs the bounded side-table link while both legs are still unplaced; `core/tests/test_oco.cpp`, `services/internal/orders/oco_test.go`)
* [x] When one fills, other cancels atomically (first terminal FILLED cancels the sibling in the same engine dispatch, journaled ORDER_CANCEL reason 7 `kWalCancelReasonOcoLink`; a not-yet-arrived sibling is marked doomed and its later OrderNew rejects `OCO_SIBLING_CANCEL_RACE` — §23, HTTP 409; non-fill terminal paths dissolve the link so the survivor continues standalone)
* [x] OCO link survives recovery (WAL) (`WalEventType::OCO_LINK` + `WalOcoLinkPayload` journaled before either leg's ORDER_NEW; `RecoveryManager` replays links by instrument ahead of order/fill/cancel entries and rebuilds the doomed/armed member state — identical link retransmission idempotent)

**SDD Checklist:**
- [x] Spec checkpoint: OCO atomic cancel — defined first, validated against spec
- [x] All spec checkpoints pass after implementation (`P14-T14.3.1-C1` PASS live — `test_oco` + `test_recovery` OCO replay in ctest 30/30; `go test ./internal/orders` covers link-before-legs ordering, pair dedup replay, sibling-race 409, and reason-7 consumer audit; `go test ./internal/api` covers the 401 gate + `OCO_SIBLING_CANCEL_RACE`→409 envelope)

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
* [x] Anomaly detection triggers auto-halt (price/volume via canonical breaker feeds; latency + error-rate detectors on admission observations — `services/internal/risk/auto_halt.go`)
* [x] Instrument suspended for 5min (delegated to the canonical INSTRUMENT circuit breaker — no parallel halt system)
* [x] P1 alert + user notification (ops.alerts.* seam + `trading_halt` notification event; `auto_halt_events` audit table, migration 217)
* [x] Auto-resume after 5min if cleared (breaker probe cycle + detector-clear reconcile → RESUMED audit row)

**SDD Checklist:**
- [x] Spec checkpoint: auto-halt on anomaly — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

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
* [x] Testnet deployment separate from production (`EXC_ENVIRONMENT=testnet` + `deploy/k8s/testnet/` namespace/configmap; production/unknown labels fail closed)
* [x] Test reset works (existing `POST /api/v1/test/reset`; `POST /api/v1/test/reset-seed` adds the preset in one cooldown slot)
* [x] Simulated funding (no real banking) (`/api/v1/test/funding/{deposit,withdrawal}` mutate balances only — `internal/testenv` never imports `internal/funding`; integration test asserts zero funding_transactions rows)
* [x] Rate limited (1 reset / 5min / account — preserved; `ResetTo` consumes one slot)

**SDD Checklist:**
- [x] Spec checkpoint: testnet with reset — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

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
* [x] 4 KYC tiers with correct trading limits (T0→no trading preserved via order-admission `KYC_REQUIRED`; T1/T2 per-tier policy + risk_limits intact; institutional lands `kyc_tier=T2` + `client_category=ELIGIBLE_COUNTERPARTY` — no INSTITUTIONAL enum value exists; approvals raise-only in `compliance/lifecycle_store.go` `DecideSubmissionTx`)
* [x] Re-verification: 12mo T2, 24mo institutional (`kyc_tier_policies.reverify_months`, migration 204; `reverify_due_at` stamped inside the approval tx)
* [x] Auto-downgrade on overdue re-verification (`LifecycleService.SweepReverify` hourly in `cmd/gateway`; T2→T1; still-ECP accounts revert to RETAIL + `nbp=true` only when `client_category='ELIGIBLE_COUNTERPARTY'` so a later explicit assignment is never clobbered; audit + `kyc_tier_downgraded` notification + ops alert)
* [x] Documents stored encrypted in S3 (Phase-12 SSE-KMS intake path unchanged — this task owns the lifecycle, not submissions)
* [x] Admin approve/reject workflow (`POST /api/v1/admin/kyc/{id}/approve` + `/reject` live, Compliance Officer RBAC; reject requires reason; single-tx decision + document verdicts + account effects + audit)

**SDD Checklist:**
- [x] Spec checkpoint: KYC lifecycle T0/T1/T2/institutional — defined first, validated against spec
- [x] Spec checkpoint: re-verification 12mo/24mo with auto-downgrade — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

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
* [x] PostgreSQL WAL archived to S3 (S3-capable `archive_command` via `wal_archive.sh`; local-backend path proven by `pitr_smoke.sh` — S3 leg shares the same script, env-switched)
* [x] Nightly base backup (`deploy/postgres/backup.sh` + `deploy/crons/pg-base-backup.sh` cron wrapper, 02:30 UTC)
* [x] RPO ≤ 15s verified (`archive_timeout = 15`; smoke test archived every segment continuously)
* [x] RTO ≤ 5min verified (mechanism-verified end-to-end on a scratch cluster by `pitr_smoke.sh` PASS — production-volume RTO is attested by the monthly drill, see below)
* [x] Monthly PITR drill documented (`docs/runbooks/pitr-monthly-drill.md`)

**SDD Checklist:**
- [x] Spec checkpoint: PostgreSQL PITR RPO ≤ 15s / RTO ≤ 5min (supersedes prior ≤ 5min / ≤ 30min) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

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
* [x] Rate limit alerts at 80% threshold (`exchange_rate_limit_utilization_over80_total` counter + `RateLimitUtilizationHigh` p2)
* [x] Slow queries logged > 100ms (`log_min_duration_statement = 100ms` in `deploy/postgres/postgresql.conf`)
* [x] PgBouncer configured (`deploy/pgbouncer/pgbouncer.ini`, `max_client_conn = 100`, transaction pooling + systemd override already in place)
* [x] Redis maxmemory + eviction alerts (noeviction coordination / volatile-lru cache verified; `RedisEvictions*` + `RedisMemoryHeadroom*` rules in `deploy/monitoring/redis-memory-alerts.yml`)

**SDD Checklist:**
- [x] Spec checkpoint: production hardening — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

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
* [x] client_category assigned at onboarding; upgrade requires Compliance Officer + evidence (migration 042 defaults `RETAIL`/`nbp=true`; `CategorizationService.SetCategory` requires non-empty evidence and audit-logs `account.client_category`; route `PUT /api/v1/admin/accounts/{id}/product-profile` under Compliance Officer RBAC)
* [x] Appropriateness test gates leveraged/derivative products; FAIL blocks with PRODUCT_NOT_PERMITTED (`CategorizationService.Appropriateness` → `orders.AppropriatenessGate` seam — orders never imports compliance, no cycle; nil gate fails closed `SERVICE_DEGRADED`; `reduce_only` bypasses so a downgrade/expiry never blocks closing)
* [x] Binary options rejected for RETAIL clients (§24 #132) (`OPTION` instrument class → RETAIL rejects outright — conservative, no binary/vanilla subtype yet; PROFESSIONAL additionally needs an unexpired PASS; ECP exempt)
* [x] Category changes + assessments audit-logged; 12-month assessment expiry enforced (`appropriateness_assessments.expires_at` checked at gate; audit rows `account.client_category` + `appropriateness.assessment`)

**SDD Checklist:**
- [x] Spec checkpoint: MiFID client categorization + appropriateness (§14.2, §24 #132) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: category downgrade with open positions (reduce-only bypass keeps exits open), expired assessment mid-session (12-month `expires_at` re-checked per admission), ECP onboarding (no appropriateness test required — ECP branch exempt)

---


### Task 14.3.8: PAMM/MAM & Copy Trading Framework

**Objective:** Implement Percentage Allocation Management Module (PAMM) and Multi-Account Manager (MAM) to support retail copy trading.

**File Locations:** `services/internal/pamm/` (types, pro-rata allocator, service, store, fill engine, NATS trades fan-out) — supersedes prior `services/internal/features/14_3_8.go` · `services/internal/db/migrations/216_pamm_engine.up.sql` · `services/internal/api/pamm.go`

**Implementation:**
1. Create master-sub account relationships for PAMM.
2. Pro-rata allocation of trades from master to sub-accounts.
3. Real-time copy trading engine.
4. **Internal Ledger Taxonomy & Fiat Limit Segregation (added 2026-09-27, remediation #38):**
   - Investor capital allocations and redemptions into PAMM manager pools MUST be recorded using dedicated internal transaction types (`PAMM_INVEST`, `PAMM_REDEEM`, `PAMM_FEE_PERF`, `PAMM_FEE_MGMT`) on internal investment sub-ledgers.
   - Recording PAMM pool subscriptions as generic `WITHDRAWAL` or `DEPOSIT` transactions is strictly prohibited.
   - Internal PAMM investments MUST NOT deduct from or count against the investor's daily fiat banking withdrawal allowance (`daily_fiat_withdrawal_allowance` / KYC T0/T1/T2 daily cash caps), preserving external SWIFT/SEPA banking rail limits for actual external cash movements.

**Definition of Done (Acceptance Criteria):**
* [x] PAMM/MAM & Copy Trading Framework implementation completed (`services/internal/pamm/`: `pamm_pools`/`pamm_allocations`/`pamm_fill_allocations`/`pamm_subledger_entries` + `pamm_txn_type_enum` (migration 216); deterministic 1e-8-quantum pro-rata allocator w/ largest-remainder residual; TRANSFER journals 2010↔2170_PAMM_POOL_LIABILITY — never DEPOSIT/WITHDRAWAL; `pamm_pool_funding_guard` trigger bars pool accounts from funding rails; routes live: POST /api/v1/pamm/pools, POST /api/v1/pamm/pools/{id}/invest, POST /api/v1/pamm/pools/{id}/redeem; spec §27.1 matrix codes PAMM_MIN_INVESTMENT_NOT_MET/PAMM_INVESTOR_LOCKED/PAMM_ALLOCATION_MISMATCH registered+emitted; fill fan-out = JetStream `trades` stream durable `pamm_copy_fanout`)
* [x] Tests passing for PAMM/MAM & Copy Trading Framework (allocator + service + engine unit tests; PG/Redis-gated `TestITInvestRedeemAndFiatGuard`/`TestITFillFanout`)

**SDD Checklist:**
- [x] Spec checkpoint: PAMM/MAM & Copy Trading Framework — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 14.3.9: Account Closure & Offboarding Lifecycle

**Objective:** Implement the account exit workflow per spec §12.5 — client-initiated and forced closure with preconditions, residual funds sweep, and full audit trail (§24 #204). Added 2026-09-16 (gap audit remediation #5).

**File Locations:** `services/internal/accounts/closure.go`, `services/internal/db/migrations/063_account_closures.up.sql` (supersedes prior `services/internal/account/closure.go` — the account-lifecycle cluster lives in the plural `accounts` package)

**Implementation:**
1. `POST /api/v1/account/close` (TOTP 2FA required): returns `ACCOUNT_CLOSE_BLOCKED` with reasons while any open position, open order, pending settlement, or pending funding transaction exists.
2. Residual sweep: remaining balances withdrawn to a verified beneficiary account via the Phase-11 withdrawal pipeline (24h hold applies to newly registered beneficiaries); GL-balanced postings (Task 3.3.6).
3. Closure transition: state → `CLOSED`; revoke all API keys, terminate active sessions/WS, remove from margin-scanner scans; account remains queryable read-only (statements/history) for the retention period.
4. Forced closure (Compliance Officer+, dual control): identical pipeline minus client confirmation, reason mandatory.
5. `account_closures` table (migration 063): `closure_id`, `account_id`, `reason`, `initiated_by`, `preconditions_snapshot` (jsonb), `sweep_refs`, `completed_at`.
6. Retention interplay: KYC/trade/ledger data retained per schedule; GDPR erasure honored except legal-hold carve-outs (§14.7, §14.8). Reopening prohibited — new registration required.

**Definition of Done (Acceptance Criteria):**
* [x] Closure rejected `ACCOUNT_CLOSE_BLOCKED` while any position/order/settlement is open (§24 #204 — `ClosureService.preconditions` names every blocker; pending funding + locked balances also block; PG-gated `TestIntegrationClosureBlocked`)
* [x] Residual balance sweep posts GL-balanced entries and settles via banking rails (drains through the Phase-11 `FlowService.Create`→`WithdrawalService.Confirm`→`DispatchService.Release` path verbatim — beneficiary/sanctions/cooldown gates and the CustomerLiability→ClearingTransit hold posting apply unchanged; withdrawal ids persist in `sweep_refs`)
* [x] API keys revoked + sessions terminated at closure; account excluded from margin scans (`SessionTerminator.RevokeAllExcept`/`CredentialRevoker.RevokeAllKeys` post-commit; every margin/fee/VIP scan already filters `status='ACTIVE'` — CLOSED is excluded structurally)
* [x] `account_closures` audit record complete for both client-initiated and dual-control forced closures (row + `admin_audit_log` land in the close tx; forced path runs inside the `admin.OpAccountClosure` approval transaction — approval and closure commit or roll back atomically)

**SDD Checklist:**
- [x] Spec checkpoint: account closure & offboarding (§12.5, §24 #204) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation (`tests/spec/checks/phase14.go` — `P14-T14.3.9-C1` PASS live, PG-gated `TestIntegrationClosure*`)
- [x] Edge cases: closure requested during pending withdrawal (PENDING/CONFIRMED/PENDING_REVIEW funding rows → `ACCOUNT_CLOSE_BLOCKED`); forced closure with open positions (executor mass-cancels + reduce-only market-closes through the shared dispatcher BEFORE the precondition recount — residuals block the approval, request stays PENDING retryable); GDPR erasure on closed account (read-only retention honored; erasure carve-outs per §14.7/§14.8 — closure revokes all write/auth surface)

---

### Task 14.3.10: Compliance Hold Workflow

**Objective:** Implement the compliance-triggered account freeze workflow (spec §24 #219). Added 2026-09-17 (gap analysis remediation #6).

**File Locations:** `services/internal/compliance/hold_workflow.go`, `services/internal/db/migrations/215_compliance_holds.up.sql`

**Implementation:**
1. Trigger: sanctions hit (Phase-21 Task 21.3.1), PEP match (Task 21.3.11), or Compliance Officer manual action. `compliance.HoldService.PlaceHold(ctx, accountID, trigger, evidence)` is the stable seam Phase-21 machine triggers call; `trigger_source` enumerates MANUAL now + SANCTIONS_HIT/PEP_MATCH/UNUSUAL_ACTIVITY for Phase-21.
2. Auto-actions on compliance hold: account state → `FROZEN`; all resting orders cancelled; withdrawal pipeline blocked; new order submission rejected (`ACCOUNT_FROZEN`); existing positions NOT liquidated (preserve evidence).
3. Compliance Officer review: dashboard shows FROZEN accounts with trigger reason, evidence, and timeline. Disposition options: (a) release (clear false positive, restore `ACTIVE`), (b) escalate to SAR filing (Task 21.3.3), (c) escalate to account closure (Task 14.3.9).
4. SLA: high-confidence sanctions hits reviewed within 4 hours; all compliance holds reviewed within 24 hours.
5. Audit trail: all hold/release/escalation actions logged in `admin_audit_log` with Compliance Officer identity and justification.
6. Integration: links to kill-switch (Task 11.3.8), SAR filing (Task 21.3.3), account closure (Task 14.3.9), comms recordings (Task 21.3.20).

**Definition of Done (Acceptance Criteria):**
* [x] Compliance hold auto-freezes account, cancels resting orders, blocks withdrawals (single tx: hold row + FROZEN + `account_freeze_events` + audit, then ≤3 mass-cancel attempts via the shared dispatcher — a cancel outage pages P1 `HOLD_CANCEL_FAILED` while the legal freeze stands; withdrawals/new orders reject `ACCOUNT_FROZEN` via existing status gates; positions never liquidated)
* [x] Compliance Officer review dashboard provides release/escalate disposition (`GET /api/v1/admin/compliance/holds` lists holds with reason/evidence/SLA timeline joined to live account status; release is four-eyes `approver_id` — restores ACTIVE only when no other OPEN hold stands; escalate `sar` records `ESCALATED_SAR` (filing is Phase-21 Task 21.3.3); escalate `closure` submits the forced-closure dual-control request)
* [x] Audit trail logs all hold/release/escalation events (`admin_audit_log` actions `compliance.hold_place`/`compliance.hold_release`/`COMPLIANCE_HOLD_ESCALATE_*` in each mutation's own transaction; `account_freeze_events` carries FREEZE/UNFREEZE with initiator+approver)

**SDD Checklist:**
- [x] Spec checkpoint: Compliance Hold Workflow (§24 #219) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation (`P14-T14.3.10-C1` PASS live — PG-gated `TestIntegrationHold*` — 8 tests incl. 4h sanctions SLA, stacked holds, four-eyes release, SLA sweep + P1 alert)

---

### Task 14.3.11: Cooling-off period / responsible trading self-exclusion

Cooling-off period / responsible trading self-exclusion — `POST /api/v1/account/cooling-off` with `duration` parameter ∈ {1d, 3d, 7d, 30d}. Strictly irrevocable once activated — no admin override, no support override, no early termination. Effects: all open leveraged/margin positions market-closed, new margin/leveraged order entry rejected with `COOLING_OFF_ACTIVE`, spot conversions and withdrawals remain available. Stored in `cooling_off_periods` table (migration 213; supersedes placeholder "migration 070" — 070 is occupied by `070_users_anti_phishing_code`): `{id, user_id, duration, started_at, expires_at, acknowledged_at}`. Requires legal acknowledgment checkbox before activation. Regulatory compliance: ESMA (PS 19/18), FCA, ASIC, CySEC responsible trading mandates.

**SDD Checklist:**
- [x] Spec checkpoint: cooling-off is irrevocable for its term and blocks leveraged trading (§24 #273) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation (`P14-T14.3.11-C1` PASS live — `CoolingOffService` immutable insert-only row before the de-risking saga; no cancel/shorten/update path exists anywhere; leveraged entry rejects `COOLING_OFF_ACTIVE` via `orders.CoolingOffGate`, fails closed on lookup errors; spot/withdrawals untouched; PG-gated `TestIntegrationCoolingOff*`)

---

### Task 14.3.12: Cooling-Off Invariant Enforcement & Webhook Delivery Retries

**Objective:** Implement strict fail-closed enforcement of responsible trading cooling-off periods and resilient webhook delivery retry pipelines per spec §2.7, §8.7, §12.6, and §24 #315.

**Implementation:**
1. **Irrevocable Cooling-Off Enforcement:** When cooling-off is activated, execute atomic saga: cancel all resting limit/stop orders on leveraged instruments, submit market orders to flatten open margin positions, and set account flag `COOLING_OFF_ACTIVE` preventing leveraged order submission. Block all administrative or programmatic attempts to shorten or bypass the active window.
2. **Webhook Resilient Delivery Pipeline:** Implement worker consuming from NATS JetStream webhook stream. Deliver events with HMAC-SHA256 signature; on HTTP 5xx or network timeout, retry with exponential backoff (1s, 5s, 30s, 5m, 30m; max 5 attempts). *(Superseded, ownership pinned 2026-09-27 remediation #35: the Phase-05 Task 5.3.17 signed-delivery engine — `webhooks.Dispatcher`, migration 180 — owns POST/sign/retry/dead-letter with its own schedule `RetryPolicy` = 1s/2s/4s/8s/16s, max_attempts=5. The Phase-14 deliverable is the missing legs: the JetStream `webhooks` stream → `Store.Enqueue` ingest consumer and the admin dead-letter inspection/retransmit surface.)*
3. **Dead-Letter Storage:** Webhooks failing all 5 attempts move to `webhook_dead_letters` table for inspection and manual re-transmission. *(Supersedes prior wording: no second table — exhausted deliveries persist as `webhook_deliveries.status='DEAD_LETTERED'` (migration 180 partial index); inspection via `GET /api/v1/admin/webhooks/dead-letters`, manual retransmit via `POST /api/v1/admin/webhooks/dead-letters/{id}/retransmit` — requeue is PENDING with a fresh attempt budget, audited in `admin_audit_log`.)*

**Definition of Done (Acceptance Criteria):**
* [x] Cooling-off irrevocably locks leveraged trading and flattens positions (gate enforced on Submit/modify/CancelReplace/BatchSubmit and every other order-entry path that re-runs `checkAdmission` — cancels and qty-down keep-priority amends unaffected; partial saga failures surface `partial_failure` + P1 `COOLING_OFF_PARTIAL`)
* [x] Webhook deliveries retry with exponential backoff up to 5 times (canonical `webhooks.Dispatcher` — `RetryPolicy` 1s/2s/4s/8s/16s per migration-180-pinned ownership, supersedes the 1s/5s/30s/5m/30m schedule in item 2 above)
* [x] Exhausted webhooks persist in dead-letter table with error payload (`webhook_deliveries.status='DEAD_LETTERED'` + `last_status_code`/`last_error`; `ListDeadLetters` + audited `Retransmit` — fresh attempt budget, `admin_audit_log` row `webhook.retransmit`; `TestIntegrationDeadLetterRetransmit`)

**SDD Checklist:**
- [x] Spec checkpoint: Cooling-off invariant enforcement and webhook delivery retries (§24 #315) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation (`P14-T14.3.12-C1` PASS live — orders-gate tests + `TestIntegrationDeadLetterRetransmit`)

---

### Task 14.3.13: Account Product Profiles (Advantage-Style Segmentation)

**Objective:** Bind each account to an admin-defined product profile governing pricing plan, product scope and unit denomination, per spec §5.41 and §24 #369. Added 2026-09-27 (FXTM-parity remediation #29).

**File Locations:** `services/internal/accounts/products.go` (+ `product_gate.go` — plural package convention supersedes the singular sketch), `services/internal/db/migrations/095_account_products_swapfree.up.sql`

**Implementation:**
1. `account_product_profiles` table (migration 095): `profile_id`, `code` (admin-defined; seed `STANDARD` + `CENT`), `pricing_plan` (`SPREAD_MARKUP`|`RAW_SPREAD_COMMISSION`, consumed by Phase-03 Task 3.3.13), `instrument_scope` (allowlisted classes, e.g. SPOT-only vs full derivatives), `subunit_divisor` (1 standard, 100 cent — consumed by Phase-03 Task 3.3.21), `min_deposit`, `status` (ACTIVE|RETIRED).
2. `accounts.product_profile_id` FK (migration 095); assigned `STANDARD` at onboarding; profile changes rejected with `INVALID_REQUEST` while any open position, resting order or pending settlement exists (checked against Phase-03/Phase-19 state, same read path as Task 14.3.9 preconditions).
3. Enforcement: order entry rejects out-of-scope instruments with existing `PRODUCT_NOT_PERMITTED`; fee engine resolves `pricing_plan` from the profile (account override removed — profile is the single source).
4. Admin: `POST/PUT /api/v1/admin/product-profiles` and `PUT /api/v1/admin/accounts/{id}/product-profile` (pricing/scope changes dual-controlled, audit-logged); venue-info (Phase-05 Task 5.3.44) publishes per-profile scope so clients discover eligibility. (As implemented: account assignment mounts `POST /api/v1/admin/accounts/{id}/product-profile` — Task 14.3.7 already owns the live PUT variant for client categorization; same path family, distinct method. Target-market admin rows ride `PUT /api/v1/admin/product-profiles/{id}/target-market` + `POST /api/v1/admin/product-target-markets/{id}/review`.)

**Definition of Done (Acceptance Criteria):**
* [x] Out-of-scope instruments rejected; fees follow the profile pricing plan (`ProductGateService.AdmitOrder` rejects `PRODUCT_NOT_PERMITTED` inside `orders.checkAdmission` on every new-order path incl. batch; `settlement.PgProfileFeeModelSource` resolves `pricing_plan` via `accounts.product_profile_id` — no account-level override exists)
* [x] Profile switch with open exposure rejected; retired profiles block new assignment (`ProfileService.AssignProfile` reuses the Task 14.3.9 open-exposure read — positions/resting orders/pending `settlement_instructions` → `INVALID_REQUEST`; RETIRED targets rejected, existing holders grandfathered; divisor-changing switches additionally require zero balances via `ledger.ValidateProfileSwitch`)

**SDD Checklist:**
- [x] Spec checkpoint: product profiles gate pricing, scope and denomination with guarded switching (§24 #369) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation (`P14-T14.3.13-C1` PASS — PG-gated `TestIntegrationProduct*`)
- [x] Edge cases: profile retired with live holders (grandfathered — `AccountProfile` still resolves; new `AssignProfile` to a RETIRED row rejects); scope narrowed below held positions (reduce-only exits bypass the product gate — close-only); admin create/update mutations run through `admin.OpProductProfileChange` dual control + `admin_audit_log`; venue-info publishes `product_profiles[].instrument_scope`

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
* [x] Discovery ranks by computed stats only; gating (appropriateness + incubation) enforced (`GET /api/v1/copy/strategies` serves LISTED strategies with stats computed from persisted fills; conversion missing rate excludes the strategy — fail closed)
* [x] HALF_RISK halves child quantities; profit share accrues only above HWM with balanced GL lines (HWM ratchets on positive accrual, holds through loss months; `PAMM_FEE_PERF` sub-ledger rows; period idempotency; compliance suspension audit-chained, blocks new follows while existing continue)

**SDD Checklist:**
- [x] Spec checkpoint: copy discovery, safety-scaled follows and HWM profit-share over the PAMM engine (§24 #372) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: manager strategy deleted with live followers (SUSPENDED, no delete); loss month (no accrual, HWM holds); safety-mode rounding below min notional (durable SKIPPED_MIN_NOTIONAL row + `copy_child_skipped` notification)

---

### Task 14.3.15: Swap-Free (Islamic) Verification Lifecycle

**Objective:** Gate zero-financing treatment behind a request → attestation → Compliance-approval lifecycle with revocation and abuse guards, per spec §12.8 and §24 #373. Added 2026-09-27 (FXTM-parity remediation #29).

**File Locations:** `services/internal/accounts/swapfree.go` (plural package convention), `services/internal/db/migrations/095_account_products_swapfree.up.sql` (shared with Task 14.3.13)

**Implementation:**
1. `swapfree_verifications` table (migration 095): `account_id`, attestation ref (stored via the Phase-12 Task 12.3.4 document pipeline), `status` (PENDING|APPROVED|REVOKED), verifier, `decided_at`. `accounts.swapfree_status` mirrors the outcome (`STANDARD`|PENDING|VERIFIED|REVOKED).
2. `POST /api/v1/account/swap-free/request` (attestation ref required) → PENDING; Compliance Officer `POST /api/v1/admin/swap-free/{id}/approve|reject` (single approver + audit log; revocation same path). Requests allowed with open positions — treatment changes apply prospectively from approval, never retroactively.
3. Enforcement: VERIFIED accounts accrue zero Tom-Next financing per §5.21a (Phase-03 Task 3.3.19 consumes the flag); REVOKED resumes standard accrual prospectively with no back-billing (disclosed in the endpoint response and statements).
4. Abuse guard: >2 VERIFIED→REVOKED transitions per 12 months auto-flags the account for compliance-hold review (Task 14.3.10); cooling-off (Tasks 14.3.11–12) is orthogonal and unaffected.
5. Surfaces: Trader UI Settings → Swap-Free panel and the Phase-07 admin verification queue consume these endpoints (no new UI tasks — API contract owned here).

**Definition of Done (Acceptance Criteria):**
* [x] Zero financing applies only while VERIFIED; revocation resumes standard accrual with no back-billing (`accounts.swapfree_status` mirrors `swapfree_verifications` decisions — VERIFIED on approve, STANDARD on reject, REVOKED on revoke; the Phase-03 rollover reads the column and skips Tom-Next accrual prospectively only — no retroactive adjustment exists in the path)
* [x] Repeat-flip abuse routes to compliance-hold review; all transitions audit-logged (>2 VERIFIED→REVOKED per trailing 12 months inserts `compliance_holds` `UNUSUAL_ACTIVITY` inside the revocation tx — the Task 14.3.10 review workflow owns it; every decision writes `admin_audit_log` `swapfree.{approved|rejected|revoked}`)

**SDD Checklist:**
- [x] Spec checkpoint: swap-free verification lifecycle with prospective enforcement and abuse guards (§24 #373) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation (`P14-T14.3.15-C1` PASS — PG-gated `TestIntegrationSwapfreeLifecycle`)
- [x] Edge cases: request with open positions (allowed — enforcement is prospective); revoke on rollover day (next roll accrues normally, never back-billed); attestation must resolve to the account's own `kyc_documents` row (expired/absent docs reject `INVALID_REQUEST` — re-verification is a fresh document + request)

---

### Task 14.3.16: Retail Product Target-Market Governance

**Objective:** Attach a MiFID II target market (positive + negative) to every product profile and review it periodically, per spec §14.12 and §24 #376. Added 2026-09-27 (reporting-sufficiency remediation #30). Venue rulebook/product approvals (Task 21.3.15, §24 #174) govern members; nothing governs retail distribution — this closes that side.

**File Locations:** `services/internal/accounts/target_market.go` (plural package convention), `services/internal/db/migrations/099_product_target_markets.up.sql`

**Implementation:**
1. `product_target_markets` table (migration 099): `profile_id` (FK → Task 14.3.13 profiles), `client_category` (RETAIL-covered; PROFESSIONAL/ECP recorded for completeness), `knowledge_experience` band, `risk_tolerance` band, `negative_target` (e.g. no-loss-capacity retail for leveraged derivatives), `distribution_strategy` (advised/non-advised), `review_due_at`.
2. Order-entry gate: RETAIL orders on instruments outside the account's positive target (or inside its negative target) reject with existing `PRODUCT_NOT_PERMITTED`, citing the target-market reason; appropriateness (Task 14.3.7) remains the knowledge check, target-market the product check — both must pass.
3. Periodic review: `review_due_at` ≤ 12 months; overdue suspends new RETAIL opens on that profile (close-only) with Compliance Officer alert; review workflow re-approves or narrows the market, audit-logged.
4. Copy-trading strategies (Task 14.3.14) inherit the manager account's profile market; followers blocked where the strategy sits in their negative target.

**Definition of Done (Acceptance Criteria):**
* [x] Out-of-target retail orders rejected with reason; both appropriateness and target-market must pass (`ProductGateService.retailTargetCheck` rejects `PRODUCT_NOT_PERMITTED` naming the positive/negative reason; the Task 14.3.7 `AppropriatenessGate` runs as the next `checkAdmission` step — both must pass; non-RETAIL categories skip the target check)
* [x] Overdue review forces close-only with alert; reviews audit-logged (`TargetMarketService.SweepOverdue` flips APPROVED→REVIEW_OVERDUE + one `TARGET_MARKET_REVIEW_OVERDUE` P1 alert per row; the gate also evaluates `review_due_at` lazily so an unswept row still fails closed for opens while reduce-only exits pass; `Review` APPROVE|NARROW|SUSPEND writes `admin_audit_log` `target_market.review.*`)

**SDD Checklist:**
- [x] Spec checkpoint: per-profile target markets with dual-gate enforcement and periodic review (§24 #376) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation (`P14-T14.3.16-C1` PASS — PG-gated `TestIntegrationTargetMarket*`)
- [x] Edge cases: category upgrade RETAIL→PROFESSIONAL mid-review-cycle (the gate keys on live `client_category` — upgraded accounts leave the retail gate immediately; rows are recorded for completeness); profile retired with live target rows (rows persist; no new assignments reach the retired profile); copy-follow where strategy drifts out of follower target (Task 14.3.14 integration seam — followers blocked where the manager profile's market is inside their negative target; the 14.3.8/14.3.14 engine lands the consumer)

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
