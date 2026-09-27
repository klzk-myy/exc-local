# Phase 13 — Production Hardening & Reconciliation

**Duration:** 6–8 days
**Dependencies:** Phases 3–12
**Spec Reference:** §13 (Risk Management), §2.6 (Circuit Breaker), §18 (Recovery)

---

## 13.1 Objectives

Production hardening: five-tier circuit breaker, reconciliation engine (9 financial correctness categories, supersedes prior 8 — Category 9 General Ledger Zero-Sum added 2026-09-16), 47+ alert rules, and the real-time P&L service.

---

## 13.2 Prerequisites

- Phases 3–12 complete

---

## 13.3 Tasks

### Task 13.3.1: Five-Tier Circuit Breaker

**Objective:** Implement the five-tier circuit breaker per spec §2.6 with exact scopes, triggers, hold times, state machine, and admin endpoints. (Supersedes prior generic tier naming — now uses spec canonical scope names: INSTRUMENT, ACCOUNT, VOLUME_SPIKE, OPTIONS_VOLATILITY, MARKET_WIDE.)

**File Locations:** `services/internal/risk/circuit_breaker.go`

**Implementation:**
1. **State machine:** `CLOSED → OPEN(hold) → HALF_OPEN(probe) → CLOSED`. Persisted in Redis `circuit_breaker:{scope}:{id}` HASH.
2. **INSTRUMENT scope:** price move > `instrument_price_limit` (default 5%) within 60s → OPEN, hold 5min. Recovery: 10/10 probes in 30s → CLOSED.
3. **ACCOUNT scope:** 3+ rapid losses > 5% of equity within 5min → OPEN, hold 30min. Recovery: manual admin reset only.
4. **VOLUME_SPIKE scope:** 1-min volume z-score ≥ 4.0σ vs trailing 1h → OPEN, hold 10min. Recovery: 10/10 probes in 30s → CLOSED.
5. **OPTIONS_VOLATILITY scope:** IV spike > 200% vs 30-day average → OPEN, hold 15min. Recovery: 10/10 probes in 30s → CLOSED.
6. **MARKET_WIDE scope:** aggregate volatility > 20% on > 2 instruments → OPEN, no auto-hold. Recovery: manual admin resume only.
7. Auto-recovery cooldown: 60s minimum between transitions (composes with Task 13.3.9's doubled hold periods — the cooldown applies between successive transitions including doubled holds; interaction pinned 2026-09-27, remediation #35) (prevents flapping).
8. Manual override: Risk Manager can force state via admin endpoints.
9. **Admin endpoints:**
   - `POST /api/v1/admin/circuit-breaker/{symbol}` — manual trip (Risk Manager+).
   - `POST /api/v1/admin/circuit-breaker/{symbol}/reset` — manual close (dual control: 2 distinct approvers within 15min).
10. Prometheus metrics: `circuit_breaker_state{scope,id}`, `circuit_breaker_transitions_total{scope,id}`.

**Definition of Done (Acceptance Criteria):**
* [ ] 5 scopes with exact spec triggers: INSTRUMENT (5%/60s), ACCOUNT (3+ losses/5%/5min), VOLUME_SPIKE (z≥4.0σ), OPTIONS_VOLATILITY (IV>200%), MARKET_WIDE (>20% on >2 instruments)
* [ ] Hold times: INSTRUMENT 5min, ACCOUNT 30min, VOLUME_SPIKE 10min, OPTIONS_VOLATILITY 15min, MARKET_WIDE manual
* [ ] State machine CLOSED → OPEN → HALF_OPEN → CLOSED
* [ ] Recovery: INSTRUMENT/VOLUME_SPIKE/OPTIONS_VOLATILITY auto (10/10 probes in 30s); ACCOUNT manual reset; MARKET_WIDE manual resume
* [ ] Auto-recovery cooldown (60s min between transitions)
* [ ] Manual override by Risk Manager
* [ ] Manual reset requires dual control (2 distinct approvers within 15min)
* [ ] `POST /api/v1/admin/circuit-breaker/{symbol}` manual trip works
* [ ] `POST /api/v1/admin/circuit-breaker/{symbol}/reset` manual close with dual control
* [ ] Circuit breaker state in Prometheus metrics
* [ ] State persisted in Redis `circuit_breaker:{scope}:{id}` HASH

**SDD Checklist:**
- [ ] Spec checkpoint: five-tier circuit breaker with exact scopes (INSTRUMENT, ACCOUNT, VOLUME_SPIKE, OPTIONS_VOLATILITY, MARKET_WIDE) — defined first, validated against spec
- [ ] Spec checkpoint: exact triggers (5%/60s, 3+ losses/5%/5min, z≥4.0σ, IV>200%, >20%/>2 instruments) — defined first, validated against spec
- [ ] Spec checkpoint: hold times (5min, 30min, 10min, 15min, manual) — defined first, validated against spec
- [ ] Spec checkpoint: state machine CLOSED → OPEN → HALF_OPEN → CLOSED — defined first, validated against spec
- [ ] Spec checkpoint: admin endpoints with dual control on reset — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 13.3.2: Reconciliation Engine

**Objective:** Implement reconciliation across 9 financial correctness categories (supersedes prior 8 — Category 9 General Ledger Zero-Sum added 2026-09-16).

**File Locations:** `services/internal/reconciliation/`

**Implementation:**
1. **Balances:** PostgreSQL balances vs WAL-derived balances.
2. **Positions:** PostgreSQL positions vs C++ core positions.
3. **Orders:** PostgreSQL orders vs C++ core orders.
4. **Trades:** PostgreSQL trades vs WAL trades.
5. **Funding:** PostgreSQL funding_transactions vs bank statements.
6. **Settlement:** settlement_instructions vs nostro account movements.
7. **Fees:** collected fees vs expected fees.
8. **P&L:** realized + unrealized P&L vs balance changes.
9. **General Ledger:** `SUM(debit_amount) == SUM(credit_amount)` per currency across all journal entries in `ledger_lines` (zero-sum balance sheet invariant).
10. Scheduled: every 1h. On mismatch: P1 alert + auto-halt.

**Definition of Done (Acceptance Criteria):**
* [ ] All 9 categories reconciled every 1h
* [ ] Mismatch triggers P1 alert + auto-halt
* [ ] Reconciliation report generated

**SDD Checklist:**
- [ ] Spec checkpoint: 9 financial correctness categories (incl. GL zero-sum) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 13.3.3: 47+ Alert Rules

**Objective:** Implement 47+ Prometheus alert rules with PagerDuty routing.

**File Locations:** `deploy/prometheus/alerts.yml`

**Implementation:**
1. 47+ alert rules covering: latency, throughput, queue depth, WAL lag, memory, CPU, degradation, circuit breaker, reconciliation, DR, settlement, funding.
2. Severity: P1 (immediate page), P2 (business hours), P3 (ticket).
3. PagerDuty routing per severity.
4. Runbook link in alert annotation.

**Definition of Done (Acceptance Criteria):**
* [ ] 47+ alert rules defined
* [ ] P1/P2/P3 severity routing to PagerDuty
* [ ] Runbook link in each alert

**SDD Checklist:**
- [ ] Spec checkpoint: 47+ alert rules — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 13.3.4: Real-Time P&L Service

**Objective:** Implement real-time P&L computation per account.

**File Locations:** `services/internal/risk/pnl.go`

**Implementation:**
1. Realized P&L: from closed positions.
2. Unrealized P&L: open positions × (mark - entry).
3. Mark price from Phase 19.5 PriceOracle (placeholder: last trade).
4. Updated on every trade and every mark price update.
5. `GET /api/v1/account/pnl` — current realized + unrealized.
6. P&L updates pushed to UI via the Phase-6 WS private stream (`pnl` event on `account:{id}` channel) — spec §24 #67 requires streaming, not polling only.

**Definition of Done (Acceptance Criteria):**
* [ ] Realized P&L computed from closed positions
* [ ] Unrealized P&L computed from mark price
* [ ] Updated on every trade and mark update
* [ ] P&L endpoint returns correct values
* [ ] P&L updates streamed to UI via WS (spec §24 #67)

**SDD Checklist:**
- [ ] Spec checkpoint: real-time P&L — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 13.3.5: Penetration Testing Prep

**Objective:** Prepare for penetration testing (Phase 13.5).

**Implementation:**
1. Attack surface mapping: all endpoints, WS, FIX.
2. Test accounts with known balances.
3. Test data: orders, positions, trades.
4. Scope document for pen testers.

**Definition of Done (Acceptance Criteria):**
* [ ] Attack surface mapped
* [ ] Test accounts and data prepared
* [ ] Scope document ready

**SDD Checklist:**
- [ ] Spec checkpoint: pen test prep — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 13.3.6: Order-to-Trade Ratio (OTR) Limits

**Objective:** Enforce order-to-trade ratio limits per MiFID II RTS 9 (spec §13.6a, §24 #140) — counts of submitted/modified/cancelled order events vs executed trades per account per instrument over a rolling window. Added 2026-09-15.

**File Locations:** `services/internal/risk/otr_monitor.go`, `core/src/risk/PreTradeChecker.cpp` (breach flag)

**Implementation:**
1. `risk_limits` gains `max_order_to_trade_ratio` (default 500) and `otr_window` (default 60s) columns (migration 047) — canonical defaults per spec §13.6a, remediation #35; supersedes the prior 100:1/24h values, which contradicted the spec contract.
2. Event counting: every order event (new/modify/cancel) increments `otr:events:{account}:{symbol}` sliding window in Redis; fills increment `otr:trades:{account}:{symbol}`. C++ core publishes counters via Aeron; Go monitor aggregates.
3. Breach: when `events > ratio × max(trades, 1)` over the window → flag account (Redis `otr:breach:{account}`); PreTradeChecker rejects new orders (`OTR_LIMIT_EXCEEDED`, spec §23) until the account returns under the limit; cancel-only during breach.
4. Market-maker program accounts get a higher OTR allowance (spec §9.6 — e.g., 500:1 for registered MMs on their program instruments).
5. Metrics: per-account OTR gauge in Prometheus; P2 alert on breach.

**Migration note:** `migrations/047_risk_limits_otr.up.sql` — `risk_limits.max_order_to_trade_ratio`, `risk_limits.otr_window` (spec §13.6a).

**Definition of Done (Acceptance Criteria):**
* [ ] OTR computed per account+instrument over rolling window
* [ ] Breach → new orders rejected `OTR_LIMIT_EXCEEDED`, cancels allowed
* [ ] MM program accounts use higher allowance
* [ ] OTR metrics + breach alerts in Prometheus/PagerDuty

**SDD Checklist:**
- [ ] Spec checkpoint: OTR limits per RTS 9 (§13.6a, §24 #140) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: zero-trade window (ratio vs max(trades,1)), breach recovery boundary, MM vs standard accounts

---

---

### Task 13.3.7: Proof of Reserves & Solvency Merkle Tree Daily Scheduler & Client Verification API

**Objective:** Implement automated daily Proof of Reserves Merkle tree generation at 22:00 UTC EOD and the client leaf inclusion verification endpoint per spec §17.11 and §24 #186. Added 2026-09-15.

**File Locations:** `services/internal/reconciliation/merkle_tree.go`, `services/internal/api/solvency.go`, `deploy/crons/solvency-tree.sh`

**Implementation:**
1. Automated daily scheduler: cron task triggers daily at 22:00 UTC (New York close / FX EOD) to initiate solvency snapshot.
2. Tree construction: extract all active account balances per currency; compute leaf hashes `SHA256(account_id || currency || balance || salt)`; build binary Merkle tree aggregating total liabilities.
3. Nostro asset attestation: ingest bank/custodian nostro account balances, compute global reserve backing ratio (Assets / Liabilities ≥ 100%).
4. Public root publishing: sign the Merkle root hash with exchange cold-storage GPG key; publish root hash, timestamp, total balance, and GPG signature to `GET /api/v1/solvency/latest`.
5. Client verification endpoint: implement `GET /api/v1/solvency/proof?account_id={id}&currency={curr}` returning the client's salted leaf, balance, and cryptographic sibling path (Merkle audit trail) enabling independent client-side verification of root inclusion without leaking peer account identities.

**Definition of Done (Acceptance Criteria):**
* [ ] Merkle tree generates automatically daily at 22:00 UTC with all account balances included (§24 #186)
* [ ] Merkle root, reserve ratio, and GPG signature published to public endpoint
* [ ] Client verification API returns cryptographic proof path validating inclusion against the published root
* [ ] Zero leak of individual peer balance, identity, or account numbers in proof paths

**SDD Checklist:**
- [ ] Spec checkpoint: Proof of Reserves Merkle tree and verification API (§17.11, §24 #186) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: zero-balance accounts, negative balance rejection, huge account count tree scalability (>1M leaves)

---

### Task 13.3.8: API key auto-expiration policy

API key auto-expiration policy — automated daily cron job that revokes `TRADING` and `TRANSFER` permissions from API keys that lack IP allowlist configuration and are older than 90 days. Notification email sent 7 days before revocation warning. Revocation recorded as `permissions_revoked_at` timestamp on `api_keys` table; user can restore permissions after configuring an IP allowlist. Admin override via `PUT /api/v1/admin/api-keys/{id}/extend-expiry` (dual-control). Implements defense-in-depth against orphaned high-privilege keys.

**SDD Checklist:**
- [ ] Spec checkpoint: non-allowlisted privileged API keys auto-expire with warning and audit (§24 #272) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 13.3.9: Five-Tier Circuit Breaker Automated Reset & Flapping Prevention

**Objective:** Implement robust automated recovery probing, state-transition flapping mitigation, and trip telemetry across all 5 circuit breaker tiers per spec §2.7, §7.3, and §24 #313.

**Implementation:**
1. **HALF_OPEN Probing:** When hold period expires (INSTRUMENT 5min, VOLUME_SPIKE 10min, OPTIONS_VOLATILITY 15min), enter `HALF_OPEN` state permitting exactly 10 test probe orders over 30s. If all 10 probes succeed without tripping thresholds, transition to `CLOSED`.
2. **Flapping Prevention Penalty:** If a breaker trips again within 15 minutes of recovery, double the hold period (up to max 120 minutes) and alert Risk Management.
3. **Trip Telemetry & Audit:** Persist every state change with trigger parameters in `circuit_breaker_events` and stream to WebSocket admin monitor.

**Definition of Done (Acceptance Criteria):**
* [ ] HALF_OPEN state permits bounded probes before full reset
* [ ] Repeated trips apply exponential hold-time multipliers
* [ ] Circuit breaker state transitions fully audited and alerted

**SDD Checklist:**
- [ ] Spec checkpoint: Circuit breaker automated reset, flapping penalty, and probe verification (§24 #313) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 13.4 Deliverables

- Five-tier circuit breaker
- Reconciliation engine (9 categories incl. GL zero-sum) (supersedes prior 8-category scope)
- 47+ alert rules with PagerDuty
- Real-time P&L service
- Pen test preparation
- Order-to-trade ratio (OTR) limits per MiFID II RTS 9
- Proof of Reserves Merkle tree daily scheduler & client verification API (Task 13.3.7)
- Circuit breaker auto-reset, flapping prevention & half-open probe testing (Task 13.3.9)

---

## 13.5 Dependencies

- Phases 3–12
- Phase 2 (transitive via Phase 3; C++ core reconciliation references positions/orders)

---

## 13.6 Duration Estimate

6–8 days (supersedes prior 6–8 breakdown — Tasks 13.3.6–13.3.9 added, absorbed in range):
- Task 13.3.1 (Circuit breaker): 2 days
- Task 13.3.2 (Reconciliation): 2 days
- Task 13.3.3 (Alerts): 1 day
- Task 13.3.4 (P&L): 1 day
- Task 13.3.5 (Pen test prep): 0.5 day
- Task 13.3.6 (OTR limits): 0.5 day
- Task 13.3.7 (Merkle tree solvency proof): 0.5 day
- Task 13.3.9 (Circuit breaker auto-reset & flapping prevention): 0.5 day
- Testing: 0.5 day

---

## 13.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | 5 scopes with exact spec triggers: INSTRUMENT (5%/60s), ACCOUNT (3+ losses/5%/5min), VOLUME_SPIKE (z≥4.0σ), OPTIONS_VOLATILITY (IV>200%), MARKET_WIDE (>20% on >2 instruments) |
| 2 | Hold times: INSTRUMENT 5min, ACCOUNT 30min, VOLUME_SPIKE 10min, OPTIONS_VOLATILITY 15min, MARKET_WIDE manual |
| 3 | State machine CLOSED → OPEN(hold) → HALF_OPEN(probe) → CLOSED |
| 4 | Recovery: INSTRUMENT/VOLUME_SPIKE/OPTIONS_VOLATILITY auto (10/10 probes in 30s); ACCOUNT manual reset; MARKET_WIDE manual resume |
| 5 | Auto-recovery cooldown (60s min between transitions) |
| 6 | Manual override by Risk Manager |
| 7 | Manual reset requires dual control (2 distinct approvers within 15min) |
| 8 | `POST /api/v1/admin/circuit-breaker/{symbol}` manual trip works |
| 9 | `POST /api/v1/admin/circuit-breaker/{symbol}/reset` manual close with dual control |
| 10 | Circuit breaker state in Prometheus metrics |
| 11 | State persisted in Redis `circuit_breaker:{scope}:{id}` HASH |
| 12 | All 9 reconciliation categories checked every 1h (supersedes prior 8 categories) |
| 13 | Reconciliation mismatch triggers P1 alert + auto-halt |
| 14 | Reconciliation report generated |
| 15 | 47+ alert rules defined |
| 16 | P1/P2/P3 severity routing to PagerDuty |
| 17 | Runbook link in each alert annotation |
| 18 | Realized P&L computed from closed positions |
| 19 | Unrealized P&L computed from mark price |
| 20 | P&L updated on every trade and mark update |
| 21 | P&L endpoint returns correct values |
| 22 | Attack surface mapped for pen test |
| 23 | Test accounts and data prepared |
| 24 | Scope document ready for pen testers |
| 25 | Reconciliation: balances match WAL-derived |
| 26 | Reconciliation: positions match C++ core |
| 27 | Reconciliation: orders match C++ core |
| 28 | Reconciliation: trades match WAL |
| 29 | Reconciliation: funding matches bank statements |
| 30 | Reconciliation: settlement matches nostro movements |
| 31 | Reconciliation: fees match expected |
| 32 | Reconciliation: P&L matches balance changes |
| 33 | Real-time P&L streamed to UI via WS private stream on every trade/mark update (§24 #67) |
| 34 | Reconciliation: General Ledger zero-sum balance sheet invariant verified (SUM(debits) == SUM(credits)) (§24 #121) |
| 35 | OTR computed per account+instrument over rolling window; breach rejects new orders `OTR_LIMIT_EXCEEDED` (cancel-only) (§24 #140) |
| 36 | MM program accounts use higher OTR allowance; breach alerts + metrics live |
| 37 | Proof of Reserves Merkle tree generates daily at 22:00 UTC, publishes root hash with GPG signature, and serves client leaf inclusion verification proofs (§24 #186) |
| 38 | Non-IP-allowlisted privileged API keys auto-expire after 90 days with 7-day warning and audit (§24 #272) |
| 39 | Circuit breaker executes HALF_OPEN recovery probing, suppresses rapid flapping with exponential hold-time penalty, and logs trip history (§24 #313) |

