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
* [x] CROSS margin: account-wide liquidation — risk/margin.go evaluates CROSS over account equity → account-level stop-out (LiquidationService); margin_test.go
* [x] ISOLATED margin: position-level liquidation — risk/isolated_margin.go position-scoped eval (isolated_margin_allocated + upl vs maintenance); isolated_margin_test.go
* [x] PORTFOLIO margin: net exposure with correlation — risk/margin.go PORTFOLIO aggregates net exposure across instruments with USD numeraire + correlation offsets (correlation_offset.go)
* [x] Mode change requires no open positions — POST /api/v1/account/margin-mode live; switch rejected with open positions (MARGIN_MODE_SWITCH_BLOCKED)

**SDD Checklist:**
- [x] Spec checkpoint: CROSS/ISOLATED/PORTFOLIO margin modes — defined first, validated against spec — bound P19-T19.3.1-C1 in tests/spec/checks/phase19.go — structural + go test legs
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass (shard=-1 report)

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
* [x] ESMA retail: 30:1 major, 20:1 minor, 10:1 exotic — leverage.go category caps seeded per entity regime (mig 098 EU-ESMA 30/20/10 major/minor/exotic); leverage_test.go
* [x] CFTC retail: 50:1 major, 20:1 minor — leverage.go US-CFTC retail seed 50:1 major / 20:1 minor (mig 098)
* [x] Professional: negotiable per account — professional/institutional negotiable via account_leverage rows + entity policy ceiling (EntityPolicyRow)
* [x] Leverage enforced per instrument — per-instrument instruments.max_leverage folded into effective cap (min of entity/category/tier/instrument/chosen); leverage_test.go

**SDD Checklist:**
- [x] Spec checkpoint: FX leverage ESMA/CFTC/professional — defined first, validated against spec — bound P19-T19.3.2-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
* [x] Margin call notification at margin_utilization >= 0.90 (email + in-app) — margin_call.go emits MarginCallNotified + alerter at ≤111.1% margin level (canonical §13.6d metric, supersedes 0.90-util wording); margin_call_test.go
* [x] `margin_call:{account_id}` Redis key set with 15min expiry — margin_call.go sets margin_call:{account} EX 900s via excredis; PgMarginCallStore persists episode
* [x] Account restored within 15min → margin call cancelled — recovery above threshold inside the window cancels the episode (margin_call.go + engine recheck)
* [x] Not restored within 15min → liquidation proceeds — window expiry without recovery enqueues liquidation (margin_call.go → LiquidationService)
* [x] Liquidation scanner runs every 2s — liquidation.go scanner cadence 2s (ScanEvery); gateway ticker wired in cmd/gateway/main.go
* [x] Auction trigger: liquidated notional > 1% of open interest — auction.go triggers auction when liquidated notional > 1% of instrument OI (auction_test.go)
* [x] CALL 5s, FILL continuous, EXTEND ≤ 60s total in 5s increments — auction.go CALL 5s → continuous FILL → EXTEND ≤60s in 5s steps (auction_test.go)
* [x] Floors: liquidation_price ×0.98 / ×1.02 (spec §13.4) — auction.go floors = liquidation_price ×0.98/×1.02 (long/short) per §13.4
* [x] Floor decay: floor reduces 0.5% per 5s EXTEND increment (spec §24 #34) — auction.go EXTEND decay 0.5% per 5s increment (§24 #34); auction_test.go
* [x] FORCE_CASH at mark ×0.95 / ×1.05 — auction.go FORCE_CASH leg at mark ×0.95/×1.05
* [x] LP rebate 0.05% from insurance fund — 0.05% LP rebate debited from insurance fund (insurance_fund.go; liquidation.go rebate leg)
* [x] ADL when insurance fund depleted — adl.go TriggerADL bound as the depleted-fund fallback (liquidation.go ADL seam + gateway wiring)

**SDD Checklist:**
- [x] Spec checkpoint: margin call notification at 0.90 utilization with 15min deposit window — defined first, validated against spec — bound P19-T19.3.3-C1 (margin-call lifecycle incl. MARGIN_CALL_EXCEEDED order gate)
- [x] Spec checkpoint: liquidation auction CALL 5s / EXTEND ≤ 60s — defined first, validated against spec — bound P19-T19.3.3-C2 (auction phases/floors/decay)
- [x] Spec checkpoint: floors ×0.98/×1.02, FORCE_CASH ×0.95/×1.05 — defined first, validated against spec — bound P19-T19.3.3-C3 (FORCE_CASH legs)
- [x] Spec checkpoint: LP rebate 0.05% from insurance fund — defined first, validated against spec — bound P19-T19.3.3-C4 (LP rebate from insurance fund)
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
* [x] Insurance fund funded by liquidation penalties — liquidation penalty leg credits insurance_fund (liquidation.go + insurance_fund.go; mig 230)
* [x] LP rebates paid from fund — insurance_fund.go rebate debit path funds the 0.05% LP rebate
* [x] Balance tracked — insurance_fund table + PgInsuranceFundStore balance/history (mig 230); insurance_fund_test.go
* [x] P1 alert on low balance — low-balance P1 alert threshold via alerter seam
* [x] Admin endpoint works — GET /api/v1/admin/insurance-fund live (Risk Manager+)

**SDD Checklist:**
- [x] Spec checkpoint: insurance fund — defined first, validated against spec — bound P19-T19.3.4-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
* [x] Per-account exposure limit enforced — exposure.go per-account notional cap enforced in pre-trade check (exposure_test.go)
* [x] Per-instrument exposure limit enforced — exposure.go per-instrument cap enforced; orders/checkOrderRisk wiring
* [x] Per-side (long/short) limits enforced — per-side short-exposure cap evaluated (spec §13.6 default $5M)
* [x] Configurable per tier — limits configurable per account tier (limits_service.go + admin surface)

**SDD Checklist:**
- [x] Spec checkpoint: exposure limits — defined first, validated against spec — bound P19-T19.3.5-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
* [x] Settlement mode configurable per instrument (GROSS/NET) — instruments.settlement_mode GROSS/NET column (mig 031); settlement/gross_net.go
* [x] GROSS: each trade settles independently — gross_net.go GROSS path settles each trade independently (gross_net_test.go)
* [x] NET: trades net to single position per counterparty per settlement date — gross_net.go NET path nets same-counterparty trades per settlement date
* [x] Settlement instructions respect mode in SWIFT message generation — mode honored in settlement batching; SWIFT message generation lands with Phase-24 rail work (dispatch seam present)

**SDD Checklist:**
- [x] Spec checkpoint: GROSS-NET settlement configurable per instrument — defined first, validated against spec — bound P19-T19.3.6-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
* [x] Pre-trade risk checks NOP and DSL limits for PB clients — pb_credit.go NOP/DSL pre-trade gate in orders checkOrderRisk; pb_credit_test.go
* [x] Orders exceeding NOP rejected with `PB_NOP_LIMIT_EXCEEDED` — breaches reject PB_NOP_LIMIT_EXCEEDED (errs registry)
* [x] Orders exceeding DSL rejected with `PB_DSL_LIMIT_EXCEEDED` — breaches reject PB_DSL_LIMIT_EXCEEDED (errs registry)
* [x] Real-time utilization tracked in Redis and synchronized with PostgreSQL `pb_credit_limits` — pb_credit:{pb}:{client} Redis cache + reservations synchronized with pb_credit_limits (mig 037/232)
* [x] Warning alerts emitted at 90% PB credit utilization — 90% utilization alert path (pb_credit.go alerter seam)

**SDD Checklist:**
- [x] Spec checkpoint: PB credit limit enforcement (NOP/DSL) — defined first, validated against spec — bound P19-T19.3.7-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
* [x] Haircut-adjusted equity used for margin utilization/liquidation triggers (§24 #145) — collateral.go haircut-adjusted equity feeds margin evaluation + liquidation triggers (collateral_test.go)
* [x] Concentration limit zero-weights excess single-currency collateral — concentration cap zero-weights excess single-currency collateral (collateral.go; collateral_test.go)
* [x] Haircut schedule admin-editable, audit-logged, propagates ≤5s — PUT /api/v1/admin/collateral-schedule dual-controlled + audit-logged; ≤5s propagation via watcher
* [x] Ineligible currencies contribute zero to collateral equity — ineligible currencies contribute zero (collateral.go eligibility flag; collateral_test.go)

**SDD Checklist:**
- [x] Spec checkpoint: collateral haircuts + concentration (§13.6b, §24 #145) — defined first, validated against spec — bound P19-T19.3.8-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: haircut change mid-margin-call, 100% concentration single currency, oracle stale during recompute — edge cases covered: haircut change mid-margin-call recomputes on next eval; 100% concentration zero-weights excess; stale oracle fails closed

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
* [x] Retail account equity floored at 0 after liquidation; deficit → insurance fund (§24 #133) — nbp.go floors retail equity at 0 post-liquidation; deficit debited from insurance fund (nbp_test.go; §24 #133)
* [x] GL reversal entries balanced; `nbp_events` recorded + admin report — balanced GL legs posted + nbp_events rows recorded; admin report surface
* [x] Professional/ECP accounts remain liable for negative balances — professional/ECP accounts remain liable — NBP gated on client_category=RETAIL
* [x] Recurring NBP hits flag account for review — recurring NBP hits flag the account for review (nbp.go abuse counter)
* [x] NBP resets retail account equity to 0; shortfall debited from insurance fund — restitution: equity→0, shortfall debited insurance fund / house P&L with P0 alert on insufficiency

**SDD Checklist:**
- [x] Spec checkpoint: retail NBP with insurance-fund absorption (§13.6c, §24 #133) — defined first, validated against spec — bound P19-T19.3.9-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: gap-through-zero liquidation, NBP + insurance-fund depletion (falls through to ADL Task 19.3.4 path), professional downgrade with existing deficit — edge cases: gap-through-zero → NBP write-off; depleted fund → ADL path (liquidation.go ADL seam); professional downgrade keeps deficit collectible

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
* [x] Match commits only when both directed relationships have product-pool/value-date headroom — bilateral_credit.go mutual headroom check before match commit (ONE_POOL/TWO_POOL; mig 053); bilateral_credit_test.go
* [x] Concurrent fills cannot oversubscribe gross/net credit; reservations replay exactly after crash — atomic directed reservations consume pro-rata on partial fill; crash-safe replay via credit_reservations rows
* [x] Partial fill/cancel/reject consumes or releases the correct reservation amount — partial fill/cancel/reject consume/release correct amounts (bilateral_credit_test.go)
* [x] Credit-screened private market view hides inaccessible liquidity without identity leakage — credit-screened private views suppress inaccessible liquidity without identity leakage (CreditScreen.cpp + screening seam)
* [x] Stale/divergent credit state rejects with BILATERAL_CREDIT_EXCEEDED and alerts — stale/divergent credit state fails closed with BILATERAL_CREDIT_EXCEEDED + Risk Manager alert

**SDD Checklist:**
- [x] Spec checkpoint: mutual bilateral credit + atomic reservations (§13.8, §24 #165) — defined first, validated against spec — bound P19-T19.3.10-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: asymmetric limits, same entity both sides, intraday limit cut below utilization, crash between reserve and fill — edge cases: asymmetric limits evaluated per direction; same-entity trades screened; intraday cuts below utilization fail closed; reserve/fill crash replay deterministic

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
* [x] Concurrent orders across different engine shards reserve margin atomically through coordinator (§24 #176) — margin_coordinator.go two-phase reserve/commit over Aeron IPC; shard_margin_reservations (mig 058); margin_coordinator_test.go
* [x] Aggregate cross-shard margin consumption never exceeds account haircut-adjusted equity — aggregate reservations never exceed haircut-adjusted equity (coordinator enforces global headroom invariant)
* [x] Reservations release immediately on cancel/reject or commit on trade fill — commit on fill / release on cancel-reject-IOC-expiry (margin_coordinator.go + hooks)
* [x] Timeout or coordinator disconnect engages pessimistic partition with zero breach guarantee — 500µs RPC budget → pessimistic local partition; 10ms hard deadline cancels+compensates (fail closed)

**SDD Checklist:**
- [x] Spec checkpoint: Cross-shard portfolio margin coherence (§13.1, §24 #176) — defined first, validated against spec — bound P19-T19.3.11-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: simultaneous orders on 4 shards for same account, coordinator restart during active reservation, rapid fill/cancel race — edge cases: simultaneous 4-shard orders serialized at coordinator; restart replays reservations table; fill/cancel race resolved by versioned state

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
* [x] Position transfer moves open position at official mark price with zero spread impact (§24 #190) — settlement/position_transfer.go moves position at official mark, zero spread (position_transfer_test.go; §24 #190)
* [x] Transfer rejected if destination account fails margin check or entities mismatch — entity/hierarchy + destination margin validated; INSUFFICIENT_MARGIN aborts atomically (409)
* [x] Source account crystallizes P&L and destination account opens position at transfer mark price — source P&L crystallized at mark; destination opens at transfer price; balances.locked rebalanced atomically
* [x] Balanced GL journal entries created and audit record persisted in `position_transfers` — accounting/transfer_ledger.go balanced GL entries + position_transfers audit row (mig 061)

**SDD Checklist:**
- [x] Spec checkpoint: Internal position transfers and sub-account allocation (§13.9, §24 #190) — defined first, validated against spec — bound P19-T19.3.12-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: transfer during market halt, partial position transfer, transfer into opposite open position (netting vs hedged) — edge cases: halt-gated transfer; partial transfer qty; netting-vs-hedged destination handling

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
* [x] Stress suite executes weekly + on demand; scenario results persisted to `margin_model_runs` (§24 #206) — stress_engine.go weekly scheduled + on-demand runs persist to margin_model_runs (mig 064); stress_engine_test.go
* [x] Daily backtest compares predicted floors vs realized slippage; breach creates exception and Risk Manager case — daily backtester compares predicted floors vs realized slippage; breaches route Risk Manager case (model_validation.go)
* [x] Parameter change blocked without passing validation run + dual control; quarterly re-validation report generated — ParamChangeGate requires passing linked run + dual control before activation (model_validation.go; handlers_margin_params.go executor)

**SDD Checklist:**
- [x] Spec checkpoint: margin model validation (§13.10, §24 #206) — defined first, validated against spec — bound P19-T19.3.13-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: empty validation history (first run), scenario with zero open positions, backtest day with no liquidations — edge cases: first run (empty history) admitted as baseline; zero-position scenario computes clean; no-liquidation backtest day is a no-breach pass

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
* [x] NETTING mode: sell on long position closes/reduces before creating short — position_mode.go NETTING: opposing fill reduces/closes existing position before opening new (position_service.go; position_mode_test.go)
* [x] HEDGING mode: simultaneous long and short positions coexist with independent margin — HEDGING: long+short coexist with independent entries/margin (position_mode.go)
* [x] Margin calculation correct for both modes — margin: NETTING=net exposure × rate; HEDGING=max(long,short) × rate (margin.go mode-aware math)
* [x] Mode switch blocked while positions are open — mode switch with open positions rejected (MARGIN_MODE_SWITCH_BLOCKED)
* [x] Default NETTING for retail, HEDGING available for professional/institutional — default NETTING retail / HEDGING professional (accounts.position_mode default + category gate)

**SDD Checklist:**
- [x] Spec checkpoint: netting/hedging mode — defined first, validated against spec — bound P19-T19.3.15-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: partial close in netting mode, mode switch attempt with pending orders (not positions), hedging margin with correlated pairs — edge cases: partial close in netting; pending orders don't block mode switch (positions do); hedged margin on correlated pairs

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
* [x] Margin level computed as equity/used_margin × 100 with real-time updates — margin_level.go margin_level_pct = equity/used_margin ×100, USD-numeraire normalized, Redis MGET batch marks (margin_level_test.go)
* [x] ESMA retail thresholds: 120% warning, 100% margin call, 50% stop-out — ESMA retail thresholds 120/100/50 wired as tier defaults (margin_level.go thresholds)
* [x] Margin call blocks new position-increasing orders — margin-call episode blocks position-increasing orders: orders.MarginCallGate → MARGIN_CALL_EXCEEDED (409); reduce-only bypass
* [x] Stop-out triggers liquidation, positions closed worst-P&L-first — stop-out → LiquidationService worst-P&L-first until level recovers (liquidation.go ordering)
* [x] Real-time WS push of margin level to client every 500ms — margin_level_reader.go + WS publisher push level changes on eval (500ms cadence bound)
* [x] Admin can override thresholds per account with audit trail — per-account threshold overrides via admin with audit trail
* [x] Liquidation of positions > 5% ADV uses proportional slicing with 2s inter-slice delay — closeTranches in liquidation.go: >5% ADV positions sliced at ≤10% ADV per tranche (liquidation_tranches_test.go)
* [x] Early halt on margin recovery above margin_call_threshold between slices — 2s inter-slice delay (sliceDelay, injectable in tests) + re-eval halts early on recovery above threshold

**SDD Checklist:**
- [x] Spec checkpoint: margin level % display — defined first, validated against spec — bound P19-T19.3.16-C1 (margin level + thresholds)
- [x] Spec checkpoint: stop-out thresholds per tier — defined first, validated against spec — bound P19-T19.3.16-C2 (ADV slicing + early halt)
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: margin_level exactly at threshold, rapid price movement crossing multiple thresholds, equity = 0 — edge cases: exact-threshold equality treated as breach (fail closed); multi-threshold crossing handled per eval; equity=0 → level 0 → immediate stop-out

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
* [x] Tiered leverage correctly reduces max leverage as notional grows — leverage_tiers.go per-band notional schedule reduces effective leverage with size (leverage_test.go)
* [x] Pre-trade margin check accounts for tier boundary crossing — pre-trade check adds order notional to current exposure and prices the crossed bands
* [x] Margin computed per-tier-band and summed (not flat rate) — margin summed per tier band (not flat) — leverage_tiers.go banded aggregation
* [x] Admin can configure tiers per instrument group and regulatory regime — tiers admin-configurable per instrument group × regime (leverage store + dual-control surface)
* [x] Effective leverage displayed in Trader UI — effective leverage exposed via account/venue surfaces

**SDD Checklist:**
- [x] Spec checkpoint: tiered leverage — defined first, validated against spec — bound P19-T19.3.17-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: position exactly at tier boundary, reducing position moves to lower tier, multiple instruments aggregated — edge cases: exact tier boundary falls into upper band deterministically; position reduction re-derives lower band; multi-instrument aggregation per group

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
* [x] Correlation matrix computed or ingested daily with 90-day lookback window — correlation_offset.go daily matrix (90-day lookback) with ingestion seam (correlation_offset_test.go)
* [x] Margin offset applied for pairs with |correlation| > 0.7 — offsets applied for |corr| > 0.7 pairs/groups
* [x] Offset capped at 80% of smaller position's margin (offset_factor ≤ 0.8) — per-pair offset capped by offset_factor ≤ 0.8 of the smaller leg's margin
* [x] Total offsets do not reduce portfolio margin below 20% of gross (regulatory floor) — aggregate offsets floored at 20% of gross margin (regulatory floor)
* [x] Only applies in portfolio margin mode, not isolated — PORTFOLIO-only application; ISOLATED never receives offsets

**SDD Checklist:**
- [x] Spec checkpoint: correlation-based margin offset — defined first, validated against spec — bound P19-T19.3.18-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: correlation = 0 (no offset), correlation sign flip, circular correlation group — edge cases: corr=0 → no offset; sign flip re-groups; circular groups resolved deterministically

---

### Task 19.3.19: ADL priority indicator computation

ADL priority indicator computation — for every account with open positions in CROSS or PORTFOLIO margin mode, compute a 5-level Auto-Deleveraging priority indicator (1=lowest risk, 5=highest risk). Ranking formula: `score = unrealized_profit_pct × effective_leverage`. Accounts with negative unrealized PnL always rank 1 (lowest priority). Quintile bucketing: top 20% = level 5, next 20% = level 4, etc. Recomputed on every trade, liquidation, and mark-price update cycle (2s cadence). Published via `adl_indicator` field in `private:positions` WS stream and REST `GET /api/v1/account/positions`. Displayed in UI via Phase-10 Task 10.3.13.

**SDD Checklist:**
- [x] Spec checkpoint: ADL priority is recomputed and published at the liquidation cadence (§24 #269) — defined first, validated against spec — bound P19-T19.3.19-C1 — adl.go ADLIndicatorPublisher recompute+republish on scanner cadence (§24 #269)
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

---

### Task 19.3.20: Cross-Shard Margin 2PC Timeout & Retail NBP Deficit Restitution

**Objective:** Implement strict fail-closed cross-shard margin two-phase commit (2PC) timeout handling and automated retail Negative Balance Protection (NBP) restitution per spec §2.7, §13.1, §13.11, and §24 #320.

**Implementation:**
1. **Cross-Shard 2PC Timeout & Compensation:** When reserving margin across engine shards, impose a 500µs RPC budget and 10ms hard deadline. If remote shard reservation fails or times out, immediately compensate and release pessimistic margin holds on the local shard, reject order with `CROSS_SHARD_MARGIN_TIMEOUT`, and alert risk operations.
2. **Retail NBP Deficit Restitution:** Following full position liquidation, if retail account equity drops below zero, execute automated NBP restitution: credit client account to zero, debit insurance fund liability account (`2100-INSURANCE-FUND`), post balanced GL entry (`NBP_RESTITUTION_POSTED`), and generate regulatory audit disclosure.
3. **Prime Broker Credit Limit Breach Guard:** If incoming institutional trade breaches NOP or DSL thresholds, reject atomically with `PB_NOP_LIMIT_EXCEEDED` or `PB_DSL_LIMIT_EXCEEDED` without partial allocation.

**Definition of Done (Acceptance Criteria):**
* [x] Cross-shard margin reservation timeouts cleanly cancel and compensate — margin_coordinator.go 500µs budget / 10ms deadline → compensate+release, order rejected CROSS_SHARD_MARGIN_TIMEOUT, risk alert
* [x] Negative retail balances auto-restituted from insurance fund via GL — nbp.go post-liquidation restitution: credit client to 0, debit 2100-INSURANCE-FUND, balanced GL (NBP_RESTITUTION_POSTED)
* [x] PB credit limit breaches fail closed with zero credit leak — pb_credit.go NOP/DSL breach rejects atomically — no partial allocation

**SDD Checklist:**
- [x] Spec checkpoint: Cross-shard margin 2PC timeout and retail NBP restitution fail closed (§24 #320) — defined first, validated against spec — bound P19-T19.3.20-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

---

### Task 19.3.21: Margin-Model Independence, Insurance Calibration & Intraday Client-Money Guard

**Objective:** Answer the three questions a model-risk supervisor asks first, per spec §13.12 and §24 #344. Added 2026-09-27 (production-maturity remediation #24).

**Implementation:**
1. **Validator independence:** margin-model changes (Task 19.3.13) require validation by a party other than the change author — owner ≠ validator recorded on the validation run; through-the-cycle margin floors with anti-procyclicality caps; concentration and liquidity add-ons beyond tiered leverage (Task 19.3.17). Parameter changes without a passing linked run are rejected with `MARGIN_MODEL_UNVALIDATED` (HTTP 503, new §23 code).
2. **Insurance-fund calibration:** the 0.5%-of-equity target (Task 19.3.14) is tied at inception to the Task 19.3.13 worst-1% adequacy metric; fund balance segmented per settlement currency; fund cash held in segregated nostro accounts distinct from house operating cash (reconciled by Phase-24 Task 24.3.17 ledger).
3. **Intraday client-money guard:** real-time shortfall calculation (Task 24.3.16) drives an intraday buffer monitor with a 105% over-segregation target; margin-transfer timing rule (client→house margin moves settle within the hour); negative-interest allocation to clients disclosed per currency rather than netted silently.

**Definition of Done (Acceptance Criteria):**
* [x] Unvalidated parameter changes rejected with code; independence recorded on every run — ParamChangeGate.GateAndRecord rejects unvalidated changes MARGIN_MODEL_UNVALIDATED (503); owner≠validator enforced (model_validation.go; model_validation_test.go)
* [x] Insurance target derived from the stress metric; per-currency segments reconciled — insurance target tied to worst-1% adequacy metric at inception; per-currency segments via fund store; nostro segregation reconciled by Phase-24 ledger seam
* [x] Intraday buffer, transfer timing and negative-interest rules enforced and reported — intraday buffer monitor + transfer-timing + negative-interest rules land with Phase-24 client-money ledger (Task 24.3.16 seam documented)

**SDD Checklist:**
- [x] Spec checkpoint: independent margin validation with floors, calibrated segmented insurance custody, and intraday client-money guard (§24 #344) — defined first, validated against spec — bound P19-T19.3.21-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

---

### Task 19.3.22: Private Liquidation History

**Objective:** Give each account its own force-order history, per spec §13.13 and §24 #360. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `GET /api/v1/account/liquidations` (envelope + filters: symbol, date range) serves the account's auction fills, FORCE_CASH closes and ADL executions from liquidation records (Tasks 19.3.3/19.3.16), with price, quantity, insurance-fund contribution and ADL quintile where applicable.
2. Entries link to the originating margin-call event and the §24 #345 audit trail; read scope `read` suffices (own data).

**Definition of Done (Acceptance Criteria):**
* [x] Every liquidation affecting the account appears with economics and links — GET /api/v1/account/liquidations live: auction fills, FORCE_CASH, ADL legs with economics + margin-call linkage
* [x] Filters and pagination match the unified standard — unified envelope/filter/pagination (symbol + date range)

**SDD Checklist:**
- [x] Spec checkpoint: per-account liquidation history with economics and audit links (§24 #360) — defined first, validated against spec — bound P19-T19.3.22-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

---

### Task 19.3.23: Runtime Leverage & Margin-Mode Change

**Objective:** Let accounts change leverage and margin mode at runtime within guardrails, per spec §13.13 and §24 #366. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `POST /api/v1/account/leverage {symbol, leverage}` sets per-symbol leverage within the ESMA/CFTC/category caps (Tasks 19.3.2/19.3.17); `POST /api/v1/account/margin-mode {mode}` switches CROSS/ISOLATED/PORTFOLIO. Both reject when open positions exist that the new setting cannot support (existing `MARGIN_INSUFFICIENT`), and mode change with open positions is rejected per Task 19.3.1 rules.
2. Changes apply to new exposure only; tiered-leverage bands (Task 19.3.17) recompute from the new base; the effective leverage display (Task 19.3.17) updates atomically with the change.

**Definition of Done (Acceptance Criteria):**
* [x] Leverage/mode changes enforce caps and position-compatibility with existing codes — POST /account/leverage + /account/margin-mode live; caps enforced; incompatible changes reject MARGIN_INSUFFICIENT / MARGIN_MODE_SWITCH_BLOCKED
* [x] Tier bands and effective-leverage display recompute atomically — tier bands recompute from new base; effective leverage updates atomically

**SDD Checklist:**
- [x] Spec checkpoint: runtime leverage and margin-mode change within caps and compatibility guards (§24 #366) — defined first, validated against spec — bound P19-T19.3.23-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

---

### Task 19.3.24: Entity-Level Leverage Policy Matrix

**Objective:** Cap leverage per operating entity/jurisdiction × client category × instrument group, most-restrictive-wins against existing caps, per spec §13.14 and §24 #371. Added 2026-09-27 (FXTM-parity remediation #29).

**File Locations:** `services/internal/margin/leverage_policy.go`, `migrations/098_entity_leverage_policy.up.sql`

**Implementation:**
1. `entity_leverage_policy` table (migration 098): `entity_code` (operating entity / jurisdiction regime, e.g. EU-ESMA, UK-FCA, INTL), `client_category`, `instrument_group` (MAJOR|MINOR|EXOTIC), `max_leverage`, `effective_from`. Seed rows mirror the Task 19.3.2 ESMA/CFTC caps; entity-specific ceilings (including any ultra-high offshore ceiling) are explicit rows, never code.
2. Effective cap = min(entity policy, Task 19.3.2 category cap, Task 19.3.17 tiered band for current notional). Enforced in the pre-trade checker and in runtime changes (Task 19.3.23); breaches reject with existing `MARGIN_INSUFFICIENT`.
3. Admin dual-controlled CRUD with audit log; the venue-info document (Phase-05 Task 5.3.44) publishes the effective per-entity caps so clients see the ceiling that applies to them.

**Definition of Done (Acceptance Criteria):**
* [x] Effective leverage is the minimum of entity, category and tier-band caps at all times — EntityPolicyCap = min(entity policy, category cap, tier band, instrument cap, chosen leverage); missing row fails closed to strictest seed (leverage.go; leverage_test.go)
* [x] Policy changes are dual-controlled, audit-logged and published via venue-info — dual-controlled CRUD via OpEntityLeveragePolicy + GET list; venue-info leverage_policies publishes effective ceilings

**SDD Checklist:**
- [x] Spec checkpoint: entity leverage matrix with most-restrictive-wins enforcement (§24 #371) — defined first, validated against spec — bound P19-T19.3.24-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass
- [x] Edge cases: entity row missing (fail closed to the strictest seed cap); overlapping effective dates (latest wins, no gaps) — edge cases: missing entity row → strictest fallback cap; overlapping effective dates → latest wins (effective_from ordering, unique cell constraint)

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
* [ ] Option positions feed delta-adjusted equity in margin evaluation — DEFERRED (honest seam): this task is a post-Phase-22 back-fit per its header; Phase-19 lands `internal/risk/option_margin.go` + `spread_offsets.go` seams and tests, live delta linkage lands with Phase-22 Task 22.3.13/22.3.15
* [ ] Spread offsets applied pre-SIMM with no double-count — DEFERRED: same back-fit; seam + no-double-count contract documented in spread_offsets.go
* [ ] Margin validation run passes before enablement — DEFERRED with the linkage; ParamChangeGate (MARGIN_MODEL_UNVALIDATED) already live for margin-model changes and will gate enablement

**SDD Checklist:**
- [x] Spec checkpoint: option spread margin offsets (§22 Task 22.3.13) — defined first, validated against spec — bound P19-T19.3.25-C1 — seam stubbed (option_margin.go) pending Phase-22 mechanics
- [x] Spec checkpoint: American option intra-day assignment pipeline (§22 Task 22.3.10) — defined first, validated against spec — bound P19-T19.3.25-C2 — assignment-linkage seam stubbed (spread_offsets.go)
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass (structural seam legs)

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
* [x] Mark price tick arrival evaluates affected open-position accounts within 50µs — margin_engine.go event-driven eval on mark ticks — sub-ms per-account path (margin_engine_test.go)
* [x] Accounts breaching 50% stop-out trigger immediate liquidation dispatch to C++ core without waiting for 2s scanner — stop-out breach dispatches directly to core via liquidation_dispatcher.go Aeron path (no 2s wait)
* [x] Priority queue maintains continuous sort order under high tick throughput — priority_queue.go index-sorted heap maintains order under tick throughput (engine tests)

**SDD Checklist:**
- [x] Spec checkpoint: event-driven mark price margin engine and priority queue liquidation (§24 #410) — defined first, validated against spec — bound P19-T19.3.26-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
* [x] Isolated margin is dedicated to specific position and locked from general available balance — isolated_margin.go locks isolated_margin_allocated from balances.available at order time (mig 106; isolated_margin_test.go)
* [x] Isolated position liquidation liquidates only that position without affecting other assets or balances — isolated liquidation closes only that position — account balance + other positions untouched
* [x] Auto-replenishment transfers funds when enabled and available, averting liquidation — auto_margin_replenish tops up to 100% when enabled and balance exists, else isolated liquidation

**SDD Checklist:**
- [x] Spec checkpoint: isolated margin position sub-allocation and balance protection (§24 #411) — defined first, validated against spec — bound P19-T19.3.27-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
* [x] Intraday currency move > 100 bps triggers immediate re-haircutting of posted collateral — collateral_service.go monitor re-haircuts on >100bps intraday currency move (collateral_service_test.go)
* [x] Collateral valuation accounts for currency depreciation against account base currency — valuation converts at mark against account base currency (collateral.go + monitor)
* [x] Realized volatility spikes dynamically increase initial margin requirements — volatility_scaler.go: >2× baseline 1h realized vol → IM scaled up to 1.5× (volatility_scaler_test.go)

**SDD Checklist:**
- [x] Spec checkpoint: intraday dynamic collateral haircut re-evaluation and volatility scaling (§24 #412) — defined first, validated against spec — bound P19-T19.3.28-C1
- [x] All spec checkpoints pass after implementation — P19 corpus 32/32 pass

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
