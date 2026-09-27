# Phase 19 — Multi-Asset & Portfolio Margin

**Duration:** 15–20 days (unchanged; new Task 19.3.25 option-delta/spread-offset integration appended 2026-09-27, remediation #35) (supersedes prior 15–19 — Tasks 19.3.22–19.3.23 added 2026-09-27, remediation #28)
**Dependencies:** Phases 2, 3, 8, 11
**Spec Reference:** §13 (Risk Management & Margin), §24 (Acceptance Criteria)

---

## 19.1 Objectives

Implement multi-asset margin: CROSS/ISOLATED/PORTFOLIO margin modes, FX leverage (ESMA/CFTC), liquidation engine with auction, insurance fund, ADL (auto-deleveraging), exposure limits, institutional Prime Broker credit limit checking (NOP/DSL), and mutual bilateral credit screening with credit-filtered liquidity.

---

## 19.2 Prerequisites

- Phases 2, 3, 8, 11 complete

---

## 19.3 Tasks

### Task 19.3.1: Margin Modes

**Objective:** Implement CROSS, ISOLATED, and PORTFOLIO margin modes.

**File Locations:** `services/internal/risk/margin.go`

**Implementation:**
1. **CROSS:** entire account balance as margin; liquidation when account equity < maintenance margin.
2. **ISOLATED:** only the position's assigned margin is at risk; liquidation when position margin < maintenance.
3. **PORTFOLIO:** netting across positions; margin = f(net exposure, correlation); all positions across all currencies share margin **with FX conversion** to the account's settlement currency (spec §13.1, §24 #96).
4. Default: CROSS for retail, PORTFOLIO for institutional.
5. `POST /api/v1/account/margin-mode` — change mode (requires no open positions).
6. **USD Numeraire Normalization & Batch Loading (added 2026-09-27, remediation #38):**
   - In PORTFOLIO and multi-currency margin accounts, each position's required margin is denominated in quote or base currency. Before summing into account `used_margin`, the engine MUST convert each position's required margin to base account numeraire (USD) at the latest Mark/Index price (`required_margin_usd = required_margin_quote × rate_to_usd`). Heterogeneous currency values must never be added directly.
   - Position valuation MUST batch-fetch all mark prices from Redis via single pipeline/MGET, evaluating portfolio margin in O(N) linear time without per-position N+1 database queries.

**Definition of Done (Acceptance Criteria):**
* [ ] CROSS margin: account-wide liquidation
* [ ] ISOLATED margin: position-level liquidation
* [ ] PORTFOLIO margin: net exposure with correlation
* [ ] Mode change requires no open positions

**SDD Checklist:**
- [ ] Spec checkpoint: CROSS/ISOLATED/PORTFOLIO margin modes — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.2: FX Leverage

**Objective:** Implement FX-specific leverage limits.

**File Locations:** `services/internal/risk/leverage.go`

**Implementation:**
1. **ESMA retail:** 30:1 major pairs, 20:1 minor, 10:1 exotic.
2. **CFTC retail:** 50:1 major, 20:1 minor.
3. **Professional/institutional:** negotiable (configured per account).
4. Major pairs: EUR/USD, GBP/USD, USD/JPY, USD/CHF, AUD/USD, USD/CAD, NZD/USD.
5. Minor pairs: EUR/GBP, EUR/JPY, GBP/JPY, etc.
6. Exotic: USD/MXN, USD/TRY, etc.
7. Leverage configurable per instrument in `instruments.max_leverage`.

**Definition of Done (Acceptance Criteria):**
* [ ] ESMA retail: 30:1 major, 20:1 minor, 10:1 exotic
* [ ] CFTC retail: 50:1 major, 20:1 minor
* [ ] Professional: negotiable per account
* [ ] Leverage enforced per instrument

**SDD Checklist:**
- [ ] Spec checkpoint: FX leverage ESMA/CFTC/professional — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.3: Liquidation Engine

**Objective:** Implement the liquidation engine with margin call notification, 15min deposit window, and auction.

**File Locations:** `services/internal/risk/liquidation.go`, `services/internal/risk/margin_call.go`

**Implementation:**
1. **Margin call notification (pre-liquidation):** when `margin_level_pct <= 111.1%` (canonical metric per spec §13.3/§13.6d — supersedes prior `margin_utilization >= 0.90`):
   - Emit `MarginCallNotified` event → email + in-app notification.
   - Set `margin_call:{account_id}` Redis key with 15min expiry (deposit window).
   - If account restored (margin_level_pct > 111.1%) within 15min → cancel margin call.
   - If not restored within 15min → proceed to liquidation.
2. **Liquidation scanner:** runs every 2s. Scans all `margin_mode IN ('CROSS','PORTFOLIO')` accounts below maintenance margin (post margin-call window); ISOLATED positions liquidate independently.
3. Trigger: liquidated notional > 1% of open interest → auction.
4. Auction:
   - CALL: 5 seconds (announce liquidation, solicit LP bids).
   - FILL: continuous (LPs fill at auction prices).
   - EXTEND: up to 60s total in 5s increments.
   - Floors: `liquidation_price` ×0.98 / ×1.02 (longs/shorts) per spec §13.4 (supersedes prior "of mark price" — floor base is the position's liquidation price; FORCE_CASH below remains mark-based per spec).
   - Floor decay (spec §24 #34): auction floor reduces 0.5% per 5s EXTEND increment.
   - FORCE_CASH: at mark ×0.95 (liquidated) / ×1.05 (counterparty) if auction fails.
5. LP rebate: 0.05% paid from insurance fund.
6. ADL (auto-deleveraging): if insurance fund depleted below threshold (spec §13.6: balance < -$100,000 or < 1% of fund equity), deleverage profitable counterparties.
7. **Liquidation Worker Mutex Contention & Anti-Stranding Invariant (added 2026-09-27, remediation #38):**
   - The liquidation queue worker (`ConsumeLiquidationQueue`) locks the target account via `lock:liquidation:account:{account_id}`.
   - On mutex lock contention (lock held by active matching or deposit flow), the worker MUST NEVER return with a successful (0) exit code or discard the job. A silent exit leaves the account's Redis dedup key (`liquidation:dedup:{account_id}`) active, stranding bankrupt positions in unliquidated state.
   - The worker MUST release the job back to the queue with exponential backoff (`release(delay)`). If maximum retries (default 5 attempts / 30s) are exhausted, clear the Redis dedup key and raise an L1 alert `LIQUIDATION_WORKER_LOCK_TIMEOUT`, ensuring the account is immediately re-scanned and re-enqueued.

**MarkPriceProvider interface (added 2026-09-27, functional cluster review F8):**
```go
type MarkPriceProvider interface {
    GetMarkPrice(symbol string) (decimal.Decimal, error)
    GetMarkPriceWithProvenance(symbol string) (MarkPrice, error)
}
```
Phase-19 uses `StubMarkPriceProvider` (last trade price from matching engine). Phase-19.5 replaces with `OracleMarkPriceProvider` (Refinitiv/Bloomberg/ECB feeds). The interface is the contract — Phase-19.5 MUST implement it without changing Phase-19 callers.

**Mark price dependency note:** Mark price is owned by Phase 19.5 (Price Oracle & Mark Price Service), which runs after Phase 19. During Phase 19 implementation, use a **stub mark price** (last trade price from the matching engine) as a placeholder. Phase 19.5 replaces this stub with the real PriceOracle service. This follows the same placeholder pattern as Phase 3 Task 3.3.2.

**Implementation order note (resolves circular dependency with Task 19.3.4):** Task 19.3.3 (Liquidation Engine) and Task 19.3.4 (Insurance Fund) have a mutual dependency: liquidation pays LP rebates from the insurance fund and checks depletion for ADL, while the insurance fund is funded by liquidation penalties. Implement in this order: (1) Task 19.3.4's `insurance_fund` table (spec §5.15) and balance tracking first, (2) Task 19.3.3's liquidation logic second, (3) wire the funding flow (liquidation penalties → insurance fund) last.

**Definition of Done (Acceptance Criteria):**
* [ ] Margin call notification at margin_utilization >= 0.90 (email + in-app)
* [ ] `margin_call:{account_id}` Redis key set with 15min expiry
* [ ] Account restored within 15min → margin call cancelled
* [ ] Not restored within 15min → liquidation proceeds
* [ ] Liquidation scanner runs every 2s
* [ ] Auction trigger: liquidated notional > 1% of open interest
* [ ] CALL 5s, FILL continuous, EXTEND ≤ 60s total in 5s increments
* [ ] Floors: liquidation_price ×0.98 / ×1.02 (spec §13.4)
* [ ] Floor decay: floor reduces 0.5% per 5s EXTEND increment (spec §24 #34)
* [ ] FORCE_CASH at mark ×0.95 / ×1.05
* [ ] LP rebate 0.05% from insurance fund
* [ ] ADL when insurance fund depleted

**SDD Checklist:**
- [ ] Spec checkpoint: margin call notification at 0.90 utilization with 15min deposit window — defined first, validated against spec
- [ ] Spec checkpoint: liquidation auction CALL 5s / EXTEND ≤ 60s — defined first, validated against spec
- [ ] Spec checkpoint: floors ×0.98/×1.02, FORCE_CASH ×0.95/×1.05 — defined first, validated against spec
- [ ] Spec checkpoint: LP rebate 0.05% from insurance fund — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.4: Insurance Fund

**Objective:** Implement the insurance fund.

**File Locations:** `services/internal/risk/insurance_fund.go`

**Implementation:**
1. Funded by: liquidation penalties (spread between liquidation price and auction price).
2. Used for: LP rebates (0.05%), covering losses when auction fails.
3. Balance tracked in `insurance_fund` table (spec §5.15 name; supersedes prior `insurance_fund_balance`).
4. Alert: P1 if balance < $100K (configurable threshold).
5. `GET /api/v1/admin/insurance-fund` — balance + history (Risk Manager+).

**Definition of Done (Acceptance Criteria):**
* [ ] Insurance fund funded by liquidation penalties
* [ ] LP rebates paid from fund
* [ ] Balance tracked
* [ ] P1 alert on low balance
* [ ] Admin endpoint works

**SDD Checklist:**
- [ ] Spec checkpoint: insurance fund — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.5: Exposure Limits

**Objective:** Implement per-account and per-instrument exposure limits.

**File Locations:** `services/internal/risk/exposure.go`

**Implementation:**
1. Per-account-per-symbol: max notional exposure per instrument (spec §13.6 default: $10M).
2. Per-account-all-symbols: max notional exposure across all positions (default $50M).
3. Per-side: max short exposure per symbol (default $5M — no max-long limit exists in spec §13.6; remediation #35 corrects the prior swapped per-account/per-instrument figures and the invented max-long limit, which also repeated into AC #32).
4. Configurable per account tier.
5. Enforced in pre-trade risk check.

**Definition of Done (Acceptance Criteria):**
* [ ] Per-account exposure limit enforced
* [ ] Per-instrument exposure limit enforced
* [ ] Per-side (long/short) limits enforced
* [ ] Configurable per tier

**SDD Checklist:**
- [ ] Spec checkpoint: exposure limits — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.6: GROSS-NET Settlement Mode

**Objective:** Implement configurable GROSS-NET settlement mode per instrument (per spec §24 criterion #98).

**File Locations:** `services/internal/settlement/gross_net.go`

**Implementation:**
1. Settlement mode configurable per instrument: `instruments.settlement_mode` ENUM('GROSS','NET').
2. GROSS: each trade settles independently (full notional per leg).
3. NET: trades within same counterparty net to single net position per settlement date.
4. Default: GROSS for spot FX, NET for forwards/swaps.
5. Settlement instructions respect the mode when generating SWIFT messages.
6. **Migration note:** `migrations/031_alter_instruments_add_settlement_mode.up.sql` — `ALTER TABLE instruments ADD COLUMN settlement_mode ENUM('GROSS','NET')` (spec §5.1).

**Definition of Done (Acceptance Criteria):**
* [ ] Settlement mode configurable per instrument (GROSS/NET)
* [ ] GROSS: each trade settles independently
* [ ] NET: trades net to single position per counterparty per settlement date
* [ ] Settlement instructions respect mode in SWIFT message generation

**SDD Checklist:**
- [ ] Spec checkpoint: GROSS-NET settlement configurable per instrument — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.7: Pre-Trade Prime Broker Credit Limit Enforcement (NOP & DSL)

**Objective:** Implement institutional pre-trade credit limit checking for Prime Brokerage accounts (Net Open Position - NOP, Daily Settled Limit - DSL) per spec §13.7, §24 #125.

**File Locations:** `services/internal/risk/pb_credit.go`

**Implementation:**
1. Credit parameters: load per-client PB credit limits (`net_open_position_limit`, `daily_settled_limit`, `current_net_open_position`, `current_daily_settled`) from `pb_credit_limits` table (`migrations/037_create_prime_brokerage.up.sql`, spec §5.22).
2. Real-time NOP calculation: aggregate net open positions across all currency pairs, converting each currency exposure to USD via real-time oracle rates, and compute absolute net position per PB client.
3. Real-time DSL calculation: aggregate cumulative executed notional for the current value date.
4. Pre-trade risk interceptor: evaluate order notional against remaining PB headroom (`net_open_position_limit - current_net_open_position` and `daily_settled_limit - current_daily_settled`). Reject non-compliant orders immediately before core matching dispatch:
   - Reject with `PB_NOP_LIMIT_EXCEEDED` if order breaches NOP.
   - Reject with `PB_DSL_LIMIT_EXCEEDED` if order breaches DSL.
5. In-memory caching & lock-free sync: cache active limits in Redis `pb_credit:{pb_id}:{client_id}` with atomic reservations to maintain sub-millisecond pre-trade check latencies.
6. Real-time breach alerts: trigger immediate risk notifications to Risk Manager and PB credit officer if utilization exceeds 90%.

**Definition of Done (Acceptance Criteria):**
* [ ] Pre-trade risk checks NOP and DSL limits for PB clients
* [ ] Orders exceeding NOP rejected with `PB_NOP_LIMIT_EXCEEDED`
* [ ] Orders exceeding DSL rejected with `PB_DSL_LIMIT_EXCEEDED`
* [ ] Real-time utilization tracked in Redis and synchronized with PostgreSQL `pb_credit_limits`
* [ ] Warning alerts emitted at 90% PB credit utilization

**SDD Checklist:**
- [ ] Spec checkpoint: PB credit limit enforcement (NOP/DSL) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.8: Collateral Eligibility, Haircuts & Concentration Limits

**Objective:** Implement the collateral model per spec §5.24/§13.6b (§24 #145) — eligible collateral currencies, haircuts on non-settlement-currency collateral, and concentration limits. Added 2026-09-15.

**File Locations:** `services/internal/margin/collateral.go`, `migrations/041_collateral_schedule.up.sql`

**Implementation:**
1. `collateral_schedule` table (migration 041): `currency`, `haircut_pct` (e.g., USD 0%, EUR 0.5%, JPY 1%, exotic 3–10%), `eligible BOOL`, `max_concentration_pct` (per account equity).
2. Collateral valuation: `margin_accounts.equity` computed as `Σ(balance × (1 − haircut) × oracle_rate)` — non-settlement-currency balances haircut; haircut-adjusted equity used for margin utilization, margin calls, and liquidation triggers (Tasks 19.3.3/19.3.5 consume this, replacing face-value equity).
3. Concentration: a single non-USD currency may not exceed `max_concentration_pct` of account collateral equity; excess is valued at zero for margin purposes (still withdrawable).
4. Admin: `PUT /api/v1/admin/collateral-schedule` (Risk Manager+) updates haircuts; changes audit-logged and applied on next equity recompute (≤5s propagation).
5. Equities recompute on every balance change + oracle tick batch (5s staleness gate per spec §15.3).

**Definition of Done (Acceptance Criteria):**
* [ ] Haircut-adjusted equity used for margin utilization/liquidation triggers (§24 #145)
* [ ] Concentration limit zero-weights excess single-currency collateral
* [ ] Haircut schedule admin-editable, audit-logged, propagates ≤5s
* [ ] Ineligible currencies contribute zero to collateral equity

**SDD Checklist:**
- [ ] Spec checkpoint: collateral haircuts + concentration (§13.6b, §24 #145) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: haircut change mid-margin-call, 100% concentration single currency, oracle stale during recompute

---

### Task 19.3.9: Retail Negative-Balance Protection

**Objective:** Enforce ESMA negative-balance protection for retail accounts per spec §13.6c (§24 #133) — a retail client can never lose more than deposited funds; residual debit is absorbed by the insurance fund, not collected. Added 2026-09-15.

**File Locations:** `services/internal/margin/liquidation.go` (extend), `services/internal/margin/nbp.go`

**Implementation:**
1. NBP applies to `accounts.client_category = RETAIL` (flag set in Phase-14 Task 14.3.7); professional/ECP accounts are liable for negative balances (existing collection flow).
2. On liquidation completion: if account equity < 0 and NBP applies → deficit written off to insurance fund (GL entry: insurance fund debit, client receivable credit cleared); account equity floored at 0. **Insurance fund pays the shortfall — the client is never debited below zero.**
3. Order-side guard: `reduce_only` orders + pre-trade margin check already prevent deficit creation; NBP is the backstop when gap-through-liquidation still leaves a deficit (weekend gap, flash move).
4. NBP write-offs tracked in `nbp_events` (admin report); recurring NBP hits on an account → auto-review flag (abuse detection).
5. Disclosures: retail onboarding text states NBP applies (MiFID investor-protection disclosure).
6. NBP trigger: after each liquidation event AND at daily session boundary (17:00 ET rollover), scan all retail accounts where equity < 0.
7. Reset workflow: for retail accounts (client_categorization = RETAIL per Phase-14 Task 14.3.7): set account equity to 0, compute shortfall = |negative_equity|.
8. Funding: debit shortfall from insurance fund (Phase-19 Task 19.3.14). If insurance fund < shortfall, debit from house P&L account with P0 alert.
9. GL posting: credit client liability account, debit insurance_fund/house_pnl account.
10. Professional/institutional accounts: NBP does NOT apply — standard margin call workflow continues.
11. Every NBP event logged to `nbp_events` table for regulatory reporting (Phase-21 consumes).

**Definition of Done (Acceptance Criteria):**
* [ ] Retail account equity floored at 0 after liquidation; deficit → insurance fund (§24 #133)
* [ ] GL reversal entries balanced; `nbp_events` recorded + admin report
* [ ] Professional/ECP accounts remain liable for negative balances
* [ ] Recurring NBP hits flag account for review
* [ ] NBP resets retail account equity to 0; shortfall debited from insurance fund

**SDD Checklist:**
- [ ] Spec checkpoint: retail NBP with insurance-fund absorption (§13.6c, §24 #133) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: gap-through-zero liquidation, NBP + insurance-fund depletion (falls through to ADL Task 19.3.4 path), professional downgrade with existing deficit

---

### Task 19.3.10: Bilateral Credit Groups, Mutual Screening & Reservations

**Objective:** Prevent principal-to-principal trades without sufficient mutual credit and produce credit-screened executable liquidity per spec §5.30/§13.8/§24 #165. Added 2026-09-15.

**File Locations:** `services/internal/risk/bilateral_credit.go`, `core/src/risk/CreditScreen.cpp`, `migrations/053_bilateral_credit.up.sql`

**Implementation:**
1. Add grantor/grantee credit groups with `ONE_POOL` or `TWO_POOL` (spot vs forward/NDF) profiles, directed gross/net limits, effective/expiry times, block state, version, and 90% utilization alerts.
2. A candidate match requires active buyer→seller and seller→buyer headroom. Margin, prefunding, and PB NOP/DSL checks remain separate controls and do not imply bilateral credit.
3. Reserve both directions atomically before match commit; consume pro-rata on partial fill, release on cancel/reject/expiry, and restore reservation state deterministically from WAL replay.
4. Generate account-specific credit-screened FIX/SBE market views by suppressing inaccessible liquidity without exposing counterparty identity; label public aggregate data non-credit-screened.
5. Intraday limit changes propagate versioned snapshots/deltas to the core via Aeron; stale or divergent credit state fails closed and alerts Risk Manager.

**Migration note:** `migrations/053_bilateral_credit.up.sql` creates `credit_groups`, `credit_relationships`, and `credit_reservations` per spec §5.30.

**Definition of Done (Acceptance Criteria):**
* [ ] Match commits only when both directed relationships have product-pool/value-date headroom
* [ ] Concurrent fills cannot oversubscribe gross/net credit; reservations replay exactly after crash
* [ ] Partial fill/cancel/reject consumes or releases the correct reservation amount
* [ ] Credit-screened private market view hides inaccessible liquidity without identity leakage
* [ ] Stale/divergent credit state rejects with BILATERAL_CREDIT_EXCEEDED and alerts

**SDD Checklist:**
- [ ] Spec checkpoint: mutual bilateral credit + atomic reservations (§13.8, §24 #165) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: asymmetric limits, same entity both sides, intraday limit cut below utilization, crash between reserve and fill

---

---

### Task 19.3.11: Cross-Shard Portfolio Margin Coherence & Headroom Reservation Engine

**Objective:** Implement the cross-shard portfolio margin coordination protocol and two-phase headroom reservation engine over Aeron IPC per spec §13.1 and §24 #176, guaranteeing zero cross-shard margin over-allocation. Added 2026-09-15.

**File Locations:** `services/internal/risk/margin_coordinator.go`, `core/src/risk/ShardMarginClient.cpp`, `migrations/058_shard_margin_reservations.up.sql`

**Implementation:**
1. Cross-shard margin coordinator: implement in-memory lock-free Margin Coordinator tracking global account equity, maintenance margin, and per-shard reserved headroom across all engine shards.
2. Two-phase reservation protocol:
   - When an order on Shard A requires portfolio margin, Shard A emits `MARGIN_RESERVE_REQ` over Aeron IPC ring buffer.
   - Coordinator calculates: `Available_Headroom = Total_Equity - Maintenance_Margin - Σ(Active_Shard_Reservations)`.
   - If `Available_Headroom >= Required_Margin`, coordinator logs reservation in `shard_margin_reservations` and emits `MARGIN_RESERVE_ACK`; otherwise emits `MARGIN_RESERVE_NACK`.
3. Commit and release cycle: on order fill, Shard A converts reservation to committed maintenance margin; on cancel/reject/IOC expiry, Shard A emits `MARGIN_RELEASE` restoring global headroom.
4. Pessimistic partition fallback: the coordinator reservation RPC budget is **500µs** (layered timeout, amended 2026-09-19 — supersedes the prior single 500µs threshold that contradicted spec §13.1's 10ms hard deadline); at expiry shards fall back immediately to the local pessimistic headroom partition (account headroom divided equally across shards), ensuring fail-closed margin enforcement with zero breach guarantee. The request continues in the background to the **>10ms hard deadline**, at which point the in-flight reservation is cancelled and compensated (released) before local-floor admission continues.

**Definition of Done (Acceptance Criteria):**
* [ ] Concurrent orders across different engine shards reserve margin atomically through coordinator (§24 #176)
* [ ] Aggregate cross-shard margin consumption never exceeds account haircut-adjusted equity
* [ ] Reservations release immediately on cancel/reject or commit on trade fill
* [ ] Timeout or coordinator disconnect engages pessimistic partition with zero breach guarantee

**SDD Checklist:**
- [ ] Spec checkpoint: Cross-shard portfolio margin coherence (§13.1, §24 #176) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: simultaneous orders on 4 shards for same account, coordinator restart during active reservation, rapid fill/cancel race

---

### Task 19.3.12: Internal Position Transfers & Sub-Account Allocation Engine

**Objective:** Implement off-book internal position transfers between sub-accounts or accounts of the same legal entity at official mark price with zero spread and GL journal entries per spec §13.9 and §24 #190. Added 2026-09-15.

**File Locations:** `services/internal/risk/position_transfer.go`, `services/internal/accounting/transfer_ledger.go`, `migrations/061_position_transfers.up.sql`

**Implementation:**
1. Pre-transfer validation: verify source and destination accounts share identical legal entity identity or belong to same master institutional hierarchy; check source account has sufficient open position; verify destination account has sufficient free margin to accept transferred position.
2. Mark price valuation: fetch official mid/mark price from Price Oracle; compute transfer notional without applying bid/ask spread or book crossing fees.
3. **Position reallocation & Collateral Rebalancing (amended 2026-09-27, remediation #38):**
   - Close position on source account at mark price, crystallizing realized P&L; open identical position on destination account at entry price equal to the transfer mark price.
   - **Collateral (`balances.locked`) Rebalancing Invariant:** Atomically release locked initial margin (`balances.locked`) on the source account, restoring its available balance. Simultaneously compute required initial margin on the destination account and lock it in `balances.locked`. If destination available balance is insufficient, abort the entire transfer atomically with `INSUFFICIENT_MARGIN` (HTTP 409). Transferring positions without atomically rebalancing `balances.locked` is strictly prohibited.
4. General Ledger posting: post balanced double-entry journal entries for transferred unrealized P&L and administrative transfer fees.
5. Audit persistence: record transfer record in `position_transfers` table (spec §5.38, migration 061) capturing `transfer_id`, source/dest `account_id`, `instrument_id`, `quantity`, `transfer_price`, and authorizing user ID.

**Definition of Done (Acceptance Criteria):**
* [ ] Position transfer moves open position at official mark price with zero spread impact (§24 #190)
* [ ] Transfer rejected if destination account fails margin check or entities mismatch
* [ ] Source account crystallizes P&L and destination account opens position at transfer mark price
* [ ] Balanced GL journal entries created and audit record persisted in `position_transfers`

**SDD Checklist:**
- [ ] Spec checkpoint: Internal position transfers and sub-account allocation (§13.9, §24 #190) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: transfer during market halt, partial position transfer, transfer into opposite open position (netting vs hedged)

---

### Task 19.3.13: Margin Model Validation — Stress Testing & Backtesting

**Objective:** Implement model validation for the margin and liquidation machinery (scenario stress tests + daily backtesting + parameter change control) per spec §13.10 (§24 #206). Added 2026-09-16 (gap audit remediation #5).

**File Locations:** `services/internal/risk/model_validation.go`, `services/internal/risk/stress_engine.go`, `migrations/064_margin_model_runs.up.sql`

**Implementation:**
1. Scenario library: rate shocks (±1σ/2σ/3σ), volatility spikes, weekend gap moves, flash-crash replay from historical tick data (Phase-8/Phase-20 datasets), insurance-fund depletion cascades.
2. Stress engine: offline batch replays scenarios against the portfolio margin model and liquidation auction machinery, measuring hypothetical margin shortfalls, auction failures, and insurance-fund drawdown. Runs weekly scheduled + on demand; never in the matching hot path.
3. Backtesting: daily comparison of predicted liquidation floors (§13.4) vs realized liquidation slippage; Basel-style breach counting over a rolling 250-day window.
4. Adequacy metric: insurance-fund adequacy = coverage of worst-1% scenario shortfall; breaches/exceptions persist to `margin_model_runs` (migration 064: `run_id`, `kind` (STRESS|BACKTEST), `scenario`, `result_metrics` (jsonb), `breach_count`, `created_at`, `reviewed_by`) and route a Risk Manager review case.
5. Change control: margin/liquidation parameter changes (floors, decay step, leverage tiers) require dual control and a linked validation run; quarterly full re-validation report exported as evidence for venue governance (Task 21.3.15).

**Definition of Done (Acceptance Criteria):**
* [ ] Stress suite executes weekly + on demand; scenario results persisted to `margin_model_runs` (§24 #206)
* [ ] Daily backtest compares predicted floors vs realized slippage; breach creates exception and Risk Manager case
* [ ] Parameter change blocked without passing validation run + dual control; quarterly re-validation report generated

**SDD Checklist:**
- [ ] Spec checkpoint: margin model validation (§13.10, §24 #206) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: empty validation history (first run), scenario with zero open positions, backtest day with no liquidations

---

### Task 19.3.14: Insurance Fund Governance & Capitalization
Added 2026-09-17 (gap analysis remediation #6).

Define and implement insurance fund lifecycle management (spec §13.4, §24 #218):
1. Initial capitalization: exchange seeds the insurance fund with a configurable initial balance (minimum $10M equivalent) from house funds, booked via GL (Task 3.3.6).
2. Target balance: `insurance_fund_target` = max(0.5% of total client equity, $10M). Computed daily after Tom-Next rollover.
3. Replenishment: when fund balance < 80% of target, a configurable percentage (default 10%) of daily exchange fee revenue is diverted to the fund via GL journal entries. When fund < 50% of target, P1 alert to Risk Manager.
4. Drawdown tracking: every insurance fund utilization (liquidation shortfall, retail NBP absorption, LP rebate) is logged in `insurance_fund_transactions` with reference to the triggering event.
5. Regulatory minimum: configurable per jurisdiction; breaching the regulatory floor triggers immediate P0 alert and `Throttled` degradation mode to reduce new risk exposure.
6. Reporting: daily fund balance report; monthly drawdown and replenishment summary; quarterly stress-test adequacy assessment (Task 19.3.13).
7. Dual control: any manual insurance fund adjustment (injection, withdrawal, parameter change) requires Risk Manager + Finance Ops dual approval.

---

### Task 19.3.15: Position Netting / Hedging Mode Toggle

**Objective:** Support per-account configurable position mode — netting (opposing trades close existing positions) and hedging (allow simultaneous long and short positions on same instrument).

**File Locations:** `services/internal/margin/netting_mode.go`

**Implementation:**
1. Account setting: `position_mode` enum `{NETTING, HEDGING}`. Default NETTING for retail, HEDGING available for professional/institutional.
2. NETTING mode: a sell order on an instrument where account holds a long position first reduces/closes the long before creating a new short. Order engine checks existing positions and adjusts order semantics.
3. HEDGING mode: long and short positions coexist independently. Each position has its own entry price, margin, and P&L.
4. Margin calculation differs:
   - NETTING: margin = net exposure × margin rate.
   - HEDGING: margin = MAX(long_exposure, short_exposure) × margin rate (hedged margin benefit).
5. Mode switch: only allowed when account has zero open positions on the instrument. Prevent mode switch with open positions.
6. Mode persisted in `accounts` table. Mode included in account info API response.
7. ESMA requirement: retail accounts in netting mode must be able to close positions with a single action.

**Definition of Done (Acceptance Criteria):**
* [ ] NETTING mode: sell on long position closes/reduces before creating short
* [ ] HEDGING mode: simultaneous long and short positions coexist with independent margin
* [ ] Margin calculation correct for both modes
* [ ] Mode switch blocked while positions are open
* [ ] Default NETTING for retail, HEDGING available for professional/institutional

**SDD Checklist:**
- [ ] Spec checkpoint: netting/hedging mode — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: partial close in netting mode, mode switch attempt with pending orders (not positions), hedging margin with correlated pairs

---

### Task 19.3.16: Margin Level Display & Warning Thresholds

**Objective:** Implement real-time margin level percentage computation with configurable warning and stop-out thresholds per account tier.

**File Locations:** `services/internal/margin/margin_level.go`

**Implementation:**
1. **Margin level formula & USD Numeraire Normalization (amended 2026-09-27, remediation #38):**
   - Formula: `margin_level_pct = (equity / used_margin) × 100`. If used_margin = 0, margin_level = ∞ (no positions).
   - In multi-currency portfolios, each position's required initial/maintenance margin is denominated in quote/base currency. The margin engine MUST convert each position's required margin to the account's base numeraire (USD) at current Mark/Index price (`required_margin_usd = required_margin_quote × rate_to_usd`) before summing into `used_margin`. Adding heterogeneous currency values without numeraire normalization is strictly prohibited.
   - Batch-fetch mark prices for all account positions from Redis via a single pipeline/MGET, evaluating portfolio margin in O(N) linear time without per-position N+1 database queries.
2. Equity = Task 19.3.8 collateral valuation (Σ balance × (1 − haircut) × oracle rate) + unrealized_pnl + swap_charges. Uses mark prices from PriceOracle (Phase-19.5) — Task 19.3.8's haircut-adjusted valuation is the single equity input (formula divergence fixed, remediation #35: the two tasks produced different margins exactly when haircuts/concentration bind).
3. Configurable thresholds per account tier:
   - ESMA Retail: 120% yellow warning, 100% margin call (restrict new positions), 50% stop-out (liquidation trigger).
   - Professional: 100% warning, 80% margin call, 30% stop-out.
   - Institutional: custom per agreement.
4. Real-time push: margin_level pushed to client via WS private channel every 500ms (or on significant change > 1%).
5. Margin call notification: at margin_call threshold, push notification + email. Block new position-increasing orders.
6. Stop-out trigger: at stop-out threshold, trigger liquidation engine (Task 19.3.3). Liquidate positions worst-P&L-first until margin_level > margin_call_threshold. **Precedence vs deposit window (spec §13.3, amended 2026-09-19):** stop-out supersedes any open 15-minute deposit window (`margin_call:{account_id}` is voided at stop-out entry); liquidation proceeds immediately worst-P&L-first until margin level > 100%. Conversely, the margin-call order block persists after the deposit window closes — it lifts only on automatic recovery above the margin-call threshold or an explicit, audited Risk Manager re-enable; the 15-minute window cures the shortfall, it does not restore trading.
6a. **(amended 2026-09-20 — feature-completeness audit remediation #11):** Liquidation of positions exceeding 5% of instrument ADV (Average Daily Volume) uses proportional slicing: `max_liquidation_slice = MIN(position_size, 0.10 × ADV)`. Slices execute as separate liquidation tranches with 2-second inter-slice delay. Margin is re-evaluated after each slice — liquidation halts early if margin_level recovers above margin_call_threshold. This prevents market-impact amplification during large-position forced closures.
7. Admin override: Risk Manager can adjust thresholds per account (with audit trail).
8. Dashboard widget: margin level bar with color coding (green > 200%, yellow > 120%, orange > 100%, red < 100%).

**Definition of Done (Acceptance Criteria):**
* [ ] Margin level computed as equity/used_margin × 100 with real-time updates
* [ ] ESMA retail thresholds: 120% warning, 100% margin call, 50% stop-out
* [ ] Margin call blocks new position-increasing orders
* [ ] Stop-out triggers liquidation, positions closed worst-P&L-first
* [ ] Real-time WS push of margin level to client every 500ms
* [ ] Admin can override thresholds per account with audit trail
* [ ] Liquidation of positions > 5% ADV uses proportional slicing with 2s inter-slice delay
* [ ] Early halt on margin recovery above margin_call_threshold between slices

**SDD Checklist:**
- [ ] Spec checkpoint: margin level % display — defined first, validated against spec
- [ ] Spec checkpoint: stop-out thresholds per tier — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: margin_level exactly at threshold, rapid price movement crossing multiple thresholds, equity = 0

---

### Task 19.3.17: Tiered Leverage by Notional Exposure

**Objective:** Implement dynamic leverage reduction as position notional exposure increases, applying tiered leverage schedules per instrument and regulatory regime.

**File Locations:** `services/internal/margin/tiered_leverage.go`

**Implementation:**
1. Tiered leverage table: `leverage_tiers` with columns `instrument_group` (MAJOR/MINOR/EXOTIC), `regulatory_regime` (ESMA/CFTC/PROFESSIONAL), `notional_from`, `notional_to`, `max_leverage`.
2. Example ESMA retail major tier: 0–$1M at 30:1, $1M–$5M at 20:1, $5M–$10M at 10:1, >$10M at 5:1.
3. Margin computation: sum notional per tier, compute margin per tier band, total margin = sum of tier margins.
4. Applied during pre-trade margin check: new order's additional notional is added to current exposure to determine which tier(s) the position spans.
5. Real-time: recalculate on each price tick that moves notional across a tier boundary.
6. Admin configurable: leverage tiers managed via Phase-07 admin panel with dual-control approval.
7. Display effective leverage in Trader UI account panel.

**Definition of Done (Acceptance Criteria):**
* [ ] Tiered leverage correctly reduces max leverage as notional grows
* [ ] Pre-trade margin check accounts for tier boundary crossing
* [ ] Margin computed per-tier-band and summed (not flat rate)
* [ ] Admin can configure tiers per instrument group and regulatory regime
* [ ] Effective leverage displayed in Trader UI

**SDD Checklist:**
- [ ] Spec checkpoint: tiered leverage — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: position exactly at tier boundary, reducing position moves to lower tier, multiple instruments aggregated

---

### Task 19.3.18: Correlation-Based Margin Offset for FX Portfolios

**Objective:** Implement margin offsets for correlated and inversely-correlated FX positions to reduce artificially inflated margin requirements on hedged portfolios.

**File Locations:** `services/internal/margin/correlation_offset.go`

**Implementation:**
1. Correlation matrix: daily-updated correlation coefficients between FX pairs. Source: computed from historical returns (90-day window) or ingested from risk data provider feed.
2. Correlation groups: pairs with |correlation| > 0.7 are grouped. Example: EUR/USD and GBP/USD (correlation ~0.85).
3. Offset formula: for two correlated positions, `margin_offset = min(pos1_margin, pos2_margin) × correlation × offset_factor`. Offset_factor configurable per group (default: 0.5, max: 0.8).
4. Inversely-correlated positions (e.g., long EUR/USD + long USD/CHF): apply natural hedge offset similarly.
5. Portfolio margin mode only (not applicable in isolated margin mode).
6. Maximum offset cap: total offsets cannot reduce portfolio margin below 20% of gross margin (regulatory floor).
7. Correlation matrix exposed via admin API for review; daily update at 00:00 UTC.
8. Audit trail: log every offset computation for regulatory review.

**Definition of Done (Acceptance Criteria):**
* [ ] Correlation matrix computed or ingested daily with 90-day lookback window
* [ ] Margin offset applied for pairs with |correlation| > 0.7
* [ ] Offset capped at 80% of smaller position's margin (offset_factor ≤ 0.8)
* [ ] Total offsets do not reduce portfolio margin below 20% of gross (regulatory floor)
* [ ] Only applies in portfolio margin mode, not isolated

**SDD Checklist:**
- [ ] Spec checkpoint: correlation-based margin offset — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: correlation = 0 (no offset), correlation sign flip, circular correlation group

---

### Task 19.3.19: ADL priority indicator computation

ADL priority indicator computation — for every account with open positions in CROSS or PORTFOLIO margin mode, compute a 5-level Auto-Deleveraging priority indicator (1=lowest risk, 5=highest risk). Ranking formula: `score = unrealized_profit_pct × effective_leverage`. Accounts with negative unrealized PnL always rank 1 (lowest priority). Quintile bucketing: top 20% = level 5, next 20% = level 4, etc. Recomputed on every trade, liquidation, and mark-price update cycle (2s cadence). Published via `adl_indicator` field in `private:positions` WS stream and REST `GET /api/v1/account/positions`. Displayed in UI via Phase-10 Task 10.3.13.

**SDD Checklist:**
- [ ] Spec checkpoint: ADL priority is recomputed and published at the liquidation cadence (§24 #269) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.20: Cross-Shard Margin 2PC Timeout & Retail NBP Deficit Restitution

**Objective:** Implement strict fail-closed cross-shard margin two-phase commit (2PC) timeout handling and automated retail Negative Balance Protection (NBP) restitution per spec §2.7, §13.1, §13.11, and §24 #320.

**Implementation:**
1. **Cross-Shard 2PC Timeout & Compensation:** When reserving margin across engine shards, impose a 500µs RPC budget and 10ms hard deadline. If remote shard reservation fails or times out, immediately compensate and release pessimistic margin holds on the local shard, reject order with `CROSS_SHARD_MARGIN_TIMEOUT`, and alert risk operations.
2. **Retail NBP Deficit Restitution:** Following full position liquidation, if retail account equity drops below zero, execute automated NBP restitution: credit client account to zero, debit insurance fund liability account (`2100-INSURANCE-FUND`), post balanced GL entry (`NBP_RESTITUTION_POSTED`), and generate regulatory audit disclosure.
3. **Prime Broker Credit Limit Breach Guard:** If incoming institutional trade breaches NOP or DSL thresholds, reject atomically with `PB_NOP_LIMIT_EXCEEDED` or `PB_DSL_LIMIT_EXCEEDED` without partial allocation.

**Definition of Done (Acceptance Criteria):**
* [ ] Cross-shard margin reservation timeouts cleanly cancel and compensate
* [ ] Negative retail balances auto-restituted from insurance fund via GL
* [ ] PB credit limit breaches fail closed with zero credit leak

**SDD Checklist:**
- [ ] Spec checkpoint: Cross-shard margin 2PC timeout and retail NBP restitution fail closed (§24 #320) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.21: Margin-Model Independence, Insurance Calibration & Intraday Client-Money Guard

**Objective:** Answer the three questions a model-risk supervisor asks first, per spec §13.12 and §24 #344. Added 2026-09-27 (production-maturity remediation #24).

**Implementation:**
1. **Validator independence:** margin-model changes (Task 19.3.13) require validation by a party other than the change author — owner ≠ validator recorded on the validation run; through-the-cycle margin floors with anti-procyclicality caps; concentration and liquidity add-ons beyond tiered leverage (Task 19.3.17). Parameter changes without a passing linked run are rejected with `MARGIN_MODEL_UNVALIDATED` (HTTP 503, new §23 code).
2. **Insurance-fund calibration:** the 0.5%-of-equity target (Task 19.3.14) is tied at inception to the Task 19.3.13 worst-1% adequacy metric; fund balance segmented per settlement currency; fund cash held in segregated nostro accounts distinct from house operating cash (reconciled by Phase-24 Task 24.3.17 ledger).
3. **Intraday client-money guard:** real-time shortfall calculation (Task 24.3.16) drives an intraday buffer monitor with a 105% over-segregation target; margin-transfer timing rule (client→house margin moves settle within the hour); negative-interest allocation to clients disclosed per currency rather than netted silently.

**Definition of Done (Acceptance Criteria):**
* [ ] Unvalidated parameter changes rejected with code; independence recorded on every run
* [ ] Insurance target derived from the stress metric; per-currency segments reconciled
* [ ] Intraday buffer, transfer timing and negative-interest rules enforced and reported

**SDD Checklist:**
- [ ] Spec checkpoint: independent margin validation with floors, calibrated segmented insurance custody, and intraday client-money guard (§24 #344) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.22: Private Liquidation History

**Objective:** Give each account its own force-order history, per spec §13.13 and §24 #360. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `GET /api/v1/account/liquidations` (envelope + filters: symbol, date range) serves the account's auction fills, FORCE_CASH closes and ADL executions from liquidation records (Tasks 19.3.3/19.3.16), with price, quantity, insurance-fund contribution and ADL quintile where applicable.
2. Entries link to the originating margin-call event and the §24 #345 audit trail; read scope `read` suffices (own data).

**Definition of Done (Acceptance Criteria):**
* [ ] Every liquidation affecting the account appears with economics and links
* [ ] Filters and pagination match the unified standard

**SDD Checklist:**
- [ ] Spec checkpoint: per-account liquidation history with economics and audit links (§24 #360) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.23: Runtime Leverage & Margin-Mode Change

**Objective:** Let accounts change leverage and margin mode at runtime within guardrails, per spec §13.13 and §24 #366. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `POST /api/v1/account/leverage {symbol, leverage}` sets per-symbol leverage within the ESMA/CFTC/category caps (Tasks 19.3.2/19.3.17); `POST /api/v1/account/margin-mode {mode}` switches CROSS/ISOLATED/PORTFOLIO. Both reject when open positions exist that the new setting cannot support (existing `MARGIN_INSUFFICIENT`), and mode change with open positions is rejected per Task 19.3.1 rules.
2. Changes apply to new exposure only; tiered-leverage bands (Task 19.3.17) recompute from the new base; the effective leverage display (Task 19.3.17) updates atomically with the change.

**Definition of Done (Acceptance Criteria):**
* [ ] Leverage/mode changes enforce caps and position-compatibility with existing codes
* [ ] Tier bands and effective-leverage display recompute atomically

**SDD Checklist:**
- [ ] Spec checkpoint: runtime leverage and margin-mode change within caps and compatibility guards (§24 #366) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.24: Entity-Level Leverage Policy Matrix

**Objective:** Cap leverage per operating entity/jurisdiction × client category × instrument group, most-restrictive-wins against existing caps, per spec §13.14 and §24 #371. Added 2026-09-27 (FXTM-parity remediation #29).

**File Locations:** `services/internal/margin/leverage_policy.go`, `migrations/098_entity_leverage_policy.up.sql`

**Implementation:**
1. `entity_leverage_policy` table (migration 098): `entity_code` (operating entity / jurisdiction regime, e.g. EU-ESMA, UK-FCA, INTL), `client_category`, `instrument_group` (MAJOR|MINOR|EXOTIC), `max_leverage`, `effective_from`. Seed rows mirror the Task 19.3.2 ESMA/CFTC caps; entity-specific ceilings (including any ultra-high offshore ceiling) are explicit rows, never code.
2. Effective cap = min(entity policy, Task 19.3.2 category cap, Task 19.3.17 tiered band for current notional). Enforced in the pre-trade checker and in runtime changes (Task 19.3.23); breaches reject with existing `MARGIN_INSUFFICIENT`.
3. Admin dual-controlled CRUD with audit log; the venue-info document (Phase-05 Task 5.3.44) publishes the effective per-entity caps so clients see the ceiling that applies to them.

**Definition of Done (Acceptance Criteria):**
* [ ] Effective leverage is the minimum of entity, category and tier-band caps at all times
* [ ] Policy changes are dual-controlled, audit-logged and published via venue-info

**SDD Checklist:**
- [ ] Spec checkpoint: entity leverage matrix with most-restrictive-wins enforcement (§24 #371) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: entity row missing (fail closed to the strictest seed cap); overlapping effective dates (latest wins, no gaps)

---

## 19.4 Deliverables

- CROSS/ISOLATED/PORTFOLIO margin modes
- FX leverage (ESMA/CFTC/professional)
- Liquidation engine with margin call notification + auction
- Insurance fund
- ADL
- Exposure limits
- GROSS-NET settlement mode
- Prime Broker credit limit engine (NOP & DSL enforcement)
- Collateral eligibility/haircuts/concentration model
- Retail negative-balance protection (insurance-fund absorption)
- Bilateral credit groups, mutual pre-match reservations, and credit-screened private liquidity
- Cross-shard portfolio margin coherence & headroom reservation engine (Task 19.3.11)
- Internal position transfers & sub-account allocation engine (Task 19.3.12)
- Margin model validation — stress testing, backtesting, parameter change control (Task 19.3.13)
- Position netting / hedging mode toggle (Task 19.3.15)
- Margin level display & warning thresholds (Task 19.3.16)
- Tiered leverage by notional exposure (Task 19.3.17)
- Correlation-based margin offset for FX portfolios (Task 19.3.18)
- Cross-shard margin 2PC timeout handling & NBP deficit restitution workflow (Task 19.3.20)
- Independent margin validation, calibrated insurance custody & intraday guard (Task 19.3.21)
- Private liquidation history (Task 19.3.22) & runtime leverage/margin-mode change (Task 19.3.23)
- Entity-level leverage policy matrix with most-restrictive-wins caps (Task 19.3.24, migration 098)
- Option-delta margin linkage & option spread offsets (Task 19.3.25 — post-Phase-22 back-fit, remediation #35)

---

### Task 19.3.25: Option-Delta Margin Linkage & Spread Offsets (post-Phase-22 back-fit)

**Objective:** Wire Phase-22 derivatives into the margin engine (added 2026-09-27, remediation #35 — Phase-22 Tasks 22.3.13/22.3.15 require this back-fit, but Phase-19 completes before Phase-22 with zero option awareness; without this task the linkage is unscheduled).

**File Locations:** `services/internal/risk/option_margin.go`, `services/internal/risk/spread_offsets.go`

**Implementation:**
1. Consume option positions (from the Phase-22 assignment pipeline) and compute writer delta per option position; adjust haircut-adjusted equity (Task 19.3.8 formula) by delta-adjusted delta exposure.
2. Apply option spread offsets (Task 22.3.13) before SIMM aggregation — never double-counted (§15.7 rule).
3. Margin-call/stop-out evaluation (Tasks 19.3.3/19.3.16) consumes the adjusted equity without formula changes.
4. Validation: margin-model change requires an independent validation run (`MARGIN_MODEL_UNVALIDATED` gate per Task 19.3.21) before the linkage goes live.

**Definition of Done (Acceptance Criteria):**
* [ ] Option positions feed delta-adjusted equity in margin evaluation
* [ ] Spread offsets applied pre-SIMM with no double-count
* [ ] Margin validation run passes before enablement

**SDD Checklist:**
- [ ] Spec checkpoint: option spread margin offsets (§22 Task 22.3.13) — defined first, validated against spec
- [ ] Spec checkpoint: American option intra-day assignment pipeline (§22 Task 22.3.10) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.26: Event-Driven Mark Price Margin Engine & Priority Queue Liquidation

**Objective:** Implement an event-driven mark-price margin evaluation engine with an in-memory priority queue to trigger liquidations sub-millisecond upon price dislocation per spec §13.4a and §24 #410 (supersedes the 2-second periodic polling interval of Task 19.3.3 for account stop-out evaluation). Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/risk/margin_engine.go`, `services/internal/risk/priority_queue.go`, `services/internal/risk/liquidation_dispatcher.go`

**Implementation:**
1. Maintain an in-memory min-heap / priority queue in Go Risk service index-sorted by `margin_level_pct` for all accounts with open positions.
2. Ingest PriceOracle ticks via Aeron IPC. On each tick arrival for instrument $S$:
   - Identify all active accounts holding open positions in $S$.
   - Recompute equity and updated margin level in-memory in < 50µs per account.
   - Update account position in priority queue.
3. If an account breaches the 50% stop-out threshold:
   - Immediately dispatch an emergency high-priority liquidation command directly into the C++ matching engine's ingress ring buffer via Aeron, without waiting for the 2-second background scanner.
4. The 2-second background scanner (`LiquidationScanner`, Task 19.3.3) remains as a secondary safety watchdog for slow-moving drift, uncrossed auctions, and ADL queue rebalancing.

**Definition of Done (Acceptance Criteria):**
* [ ] Mark price tick arrival evaluates affected open-position accounts within 50µs
* [ ] Accounts breaching 50% stop-out trigger immediate liquidation dispatch to C++ core without waiting for 2s scanner
* [ ] Priority queue maintains continuous sort order under high tick throughput

**SDD Checklist:**
- [ ] Spec checkpoint: event-driven mark price margin engine and priority queue liquidation (§24 #410) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.27: Isolated Margin Sub-Allocation & Position-Level Collateral Isolation

**Objective:** Implement isolated margin position allocation logic ensuring losses in an isolated position cannot draw from or trigger liquidation across an account's remaining balances per spec §5.6a, §13.1a, and §24 #411. Migration 106 adds `isolated_margin_allocated` and `auto_margin_replenish` to `positions`. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/risk/margin_service.go`, `services/internal/risk/isolated_margin.go`, `migrations/106_positions_isolated_margin.up.sql`

**Implementation:**
1. Migration 106 adds to `positions`:
   - `isolated_margin_allocated DECIMAL(28,8) DEFAULT 0.0`
   - `auto_margin_replenish BOOLEAN DEFAULT FALSE`
2. When placing an isolated margin order, the pre-trade checker locks the required initial margin from `balances.available` and assigns it to `positions.isolated_margin_allocated`.
3. Compute isolated margin level:  
   $$\text{IsolatedMarginLevel} = \frac{\text{isolated\_margin\_allocated} + \text{unrealized\_pnl}}{\text{maintenance\_margin\_required}} \times 100$$
4. When $\text{IsolatedMarginLevel} \le 50\%$:
   - If `auto_margin_replenish = TRUE` and available balance exists: transfer incremental margin to restore level to 100%.
   - Otherwise, liquidate only the isolated position. Account available balance and other positions remain untouched.

**Definition of Done (Acceptance Criteria):**
* [ ] Isolated margin is dedicated to specific position and locked from general available balance
* [ ] Isolated position liquidation liquidates only that position without affecting other assets or balances
* [ ] Auto-replenishment transfers funds when enabled and available, averting liquidation

**SDD Checklist:**
- [ ] Spec checkpoint: isolated margin position sub-allocation and balance protection (§24 #411) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 19.3.28: Intraday Dynamic Collateral Haircut Re-Evaluation & Volatility Scaler

**Objective:** Implement real-time mark-to-market re-haircutting of posted multi-currency collateral and dynamic volatility-scaled initial margin requirements per spec §13.6e and §24 #412. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/risk/collateral_service.go`, `services/internal/risk/volatility_scaler.go`

**Implementation:**
1. The collateral monitor subscribes to mark rates for all eligible collateral currencies.
2. If any non-base collateral currency experiences an intraday price deviation > 100 bps:
   - Trigger immediate re-calculation of effective collateral equity across all multi-currency accounts.
   - If effective equity breaches maintenance margin, emit margin call or trigger event-driven liquidation.
3. Volatility Scaler: compute rolling 1-hour realized volatility from ClickHouse tick feeds. If volatility exceeds baseline by $>2\times$, dynamically scale initial margin requirement by up to $1.5\times$ to protect against gap risk.

**Definition of Done (Acceptance Criteria):**
* [ ] Intraday currency move > 100 bps triggers immediate re-haircutting of posted collateral
* [ ] Collateral valuation accounts for currency depreciation against account base currency
* [ ] Realized volatility spikes dynamically increase initial margin requirements

**SDD Checklist:**
- [ ] Spec checkpoint: intraday dynamic collateral haircut re-evaluation and volatility scaling (§24 #412) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 19.5 Dependencies

- Phases 2, 3, 8, 11

---

## 19.6 Duration Estimate

15–20 days (supersedes prior 15–19 — Tasks 19.3.26–19.3.28 absorbed in range, remediation #37; prior supersedes 15–19 — Tasks 19.3.22–19.3.23 added 2026-09-27, remediation #28; prior supersedes 15–18):
- Task 19.3.1 (Margin modes): 2 days
- Task 19.3.2 (FX leverage): 1 day
- Task 19.3.3 (Liquidation): 3 days
- Task 19.3.4 (Insurance fund): 1 day
- Task 19.3.5 (Exposure): 1 day
- Task 19.3.6 (GROSS-NET settlement): 1 day
- Task 19.3.7 (PB credit limits): 1 day
- Task 19.3.8 (Collateral haircuts): 1 day
- Task 19.3.9 (NBP): 0.5 day
- Task 19.3.10 (Bilateral credit): 1.5 days
- Task 19.3.11 (Cross-shard portfolio margin): 1 day
- Task 19.3.12 (Position transfers): 0.5 day
- Task 19.3.13 (Margin model validation): 1 day
- Task 19.3.14 (Insurance fund governance): 0.5 day
- Task 19.3.15 (Position netting mode): 0.5 day
- Task 19.3.16 (Margin level thresholds): 1 day
- Task 19.3.17 (Tiered leverage): 1 day
- Task 19.3.18 (Correlation offset): 1 day
- Task 19.3.20 (Cross-shard margin timeout & NBP restitution): 0.5 day
- Task 19.3.21 (Validation independence, insurance calibration & intraday guard): 1 day
- Task 19.3.22 (Private liquidation history): 0.5 day
- Task 19.3.23 (Runtime leverage & margin-mode change): 0.5 day
- Task 19.3.24 (Entity leverage policy matrix): 1 day (absorbed in range)
- Task 19.3.26 (Event-driven margin engine): 0.5 day (absorbed in range, remediation #37)
- Task 19.3.27 (Isolated margin sub-allocation): 0.5 day (absorbed in range, remediation #37)
- Task 19.3.28 (Dynamic collateral re-haircutting): 0.5 day (absorbed in range, remediation #37)
- Testing: 2 days

---

## 19.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | CROSS margin: account-wide liquidation |
| 2 | ISOLATED margin: position-level liquidation |
| 3 | PORTFOLIO margin: net exposure with correlation |
| 4 | Mode change requires no open positions |
| 5 | ESMA retail: 30:1 major, 20:1 minor, 10:1 exotic |
| 6 | CFTC retail: 50:1 major, 20:1 minor |
| 7 | Professional/institutional: negotiable per account |
| 8 | Leverage enforced per instrument |
| 9 | Margin call notification at margin_utilization >= 0.90 (email + in-app) |
| 10 | `margin_call:{account_id}` Redis key set with 15min expiry (deposit window) |
| 11 | Account restored within 15min → margin call cancelled; not restored → liquidation proceeds (§24 #32) |
| 12 | Liquidation scanner runs every 2s (§24 #37) |
| 13 | Auction trigger: liquidated notional > 1% of open interest |
| 14 | Auction: CALL 5s, FILL continuous, EXTEND ≤ 60s total in 5s increments (§24 #33) |
| 15 | Auction floors: liquidation_price ×0.98 / ×1.02 (per spec §13.4; supersedes prior "of mark price") |
| 16 | FORCE_CASH at mark ×0.95 (liquidated) / ×1.05 (counterparty) (§24 #35) |
| 17 | LP rebate 0.05% paid from insurance fund (§24 #36) |
| 18 | ADL when insurance fund depleted (§24 #97) |
| 19 | Insurance fund funded by liquidation penalties |
| 20 | Insurance fund balance tracked |
| 21 | P1 alert on low insurance fund balance |
| 22 | Admin insurance fund endpoint works |
| 23 | Per-account exposure limit enforced |
| 24 | Per-instrument exposure limit enforced |
| 25 | Per-side (long/short) exposure limits enforced |
| 26 | Settlement mode configurable per instrument (GROSS/NET) (§24 #98) |
| 27 | GROSS: each trade settles independently |
| 28 | NET: trades net to single position per counterparty per settlement date |
| 29 | Auction floor reduces 0.5% per 5s EXTEND increment (§24 #34) |
| 30 | Liquidation scanner covers all CROSS and PORTFOLIO margin-mode accounts; ISOLATED positions liquidate independently |
| 31 | `instruments.settlement_mode` column exists (migration 031, spec §5.1) |
| 32 | Exposure defaults per spec §13.6: $10M retail account / $50M per instrument / $5M per side |
| 33 | PORTFOLIO margin shares margin across currencies with FX conversion (§24 #96) |
| 34 | Prime Broker credit limits (NOP and DSL) enforced in pre-trade risk pipeline with zero-breach guarantee (§13.7, §24 #125) |
| 35 | Haircut-adjusted collateral equity used for margin/liquidation; concentration limit zero-weights excess (§24 #145) |
| 36 | Collateral schedule admin-editable + audit-logged; ineligible currencies contribute zero |
| 37 | Retail negative-balance protection: equity floored at 0, deficit absorbed by insurance fund (§24 #133) |
| 38 | NBP events recorded + admin report; professional/ECP remain liable for deficits |
| 39 | Mutual bilateral credit requires both directed relationships and correct product-pool/value-date headroom before fill (§24 #165) |
| 40 | Atomic reservations prevent concurrent overfill and replay exactly; partial fill/cancel/reject adjusts headroom correctly |
| 41 | Private FIX/SBE liquidity is credit-screened without counterparty identity leakage; stale/divergent credit state fails closed |
| 42 | Cross-shard portfolio margin enforces two-phase headroom reservation over Aeron IPC, preventing cross-shard margin breach across concurrent orders (§24 #176) |
| 43 | Administrative and sub-account position transfers execute off-book at official mark price with zero bid/ask spread and balanced GL journal entries (§24 #190) |
| 44 | Stress-scenario suite + daily liquidation-floor backtesting persist results; exceptions route to Risk Manager (§24 #206) |
| 45 | Margin/liquidation parameter changes are dual-controlled and blocked without a passing linked validation run |
| 46 | Insurance fund lifecycle governance implemented: initial capitalization >$10M, dynamic target balance computed daily (spec §13.4, §24 #218) |
| 47 | Auto-replenishment from exchange fees at <80% target; regulatory floor breach triggers P0 + Throttled mode (spec §13.4, §24 #218) |
| 48 | NBP: retail account equity reset to 0 post-liquidation; shortfall debited from insurance fund; event logged for regulatory reporting (§24 #228) |
| 49 | Position netting mode: sell on long reduces/closes before creating short; hedging mode allows coexistent longs and shorts (§24 #229) |
| 50 | Margin calculation differs by mode: netting = net exposure × rate; hedging = max(long, short) × rate |
| 51 | Margin level = equity / used_margin × 100; ESMA retail: 120% warning, 100% margin call, 50% stop-out (§24 #230) |
| 52 | Stop-out triggers liquidation worst-P&L-first; margin call blocks new position-increasing orders |
| 53 | Tiered leverage: margin computed per notional band (e.g., 0-$1M at 30:1, $1M-$5M at 20:1); effective leverage shown in UI (§24 #231) |
| 54 | Correlation matrix computed daily (90-day window); margin offset for \|corr\| > 0.7 pairs; floor 20% of gross margin (§24 #232) |
| 55 | Correlation offset applies only in portfolio margin mode, not isolated margin |
| 56 | Liquidation of positions > 5% ADV uses proportional slicing (max slice = 10% ADV); 2s inter-slice delay; early halt on margin recovery (§24 #241) |
| 57 | ADL priority is recomputed per trade/liquidation/mark cycle and published through REST/private WS (§24 #269) |
| 58 | Cross-shard margin 2PC unacknowledged after 10ms hard deadline cancels order; negative retail balances auto-restituted via insurance fund GL posting (§24 #320) |
| 59 | Margin changes require independent validation (MARGIN_MODEL_UNVALIDATED otherwise) with through-the-cycle floors; insurance target tied to stress metric, per-currency custody; 105% intraday buffer with transfer-timing rule (§24 #344) |
| 60 | Per-account liquidation history with auction/ADL economics and margin-call links (§24 #360) |
| 61 | Runtime per-symbol leverage and margin-mode change within caps and position-compatibility guards; tier bands recompute atomically (§24 #366) |
| 62 | Entity × category × group leverage ceilings enforced most-restrictive-wins with tier bands; dual-controlled and published (§24 #371) |
| 63 | Option-delta margin linkage and option spread offsets integrated into the margin engine: writer delta adjusts haircut-adjusted equity per Phase-22 Task 22.3.15; spread offsets apply before SIMM aggregation per Task 22.3.13 (§24 #398; added 2026-09-27, remediation #35 — Phase-22 requires this back-fit but Phase-19 completes first with zero option awareness) |
| 64 | Event-driven mark price margin engine evaluates margin on tick arrival and dispatches liquidations immediately, superseding 2s polling for stop-outs (§24 #410) |
| 65 | Isolated margin mode confines position risk to allocated margin without draining general account balance (§24 #411) |
| 66 | Intraday collateral haircut re-evaluation recalculates effective equity on >100bps currency moves, preventing unauthorized collateral inflation (§24 #412) |
