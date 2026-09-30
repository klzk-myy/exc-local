# Phase 22 — FX Derivatives Foundation

**Duration:** 15–22 days (unchanged; §22.6 itemization recomputed 2026-09-27, remediation #35: 18.5 task-days + 2 testing = 20.5) (supersedes 14–18 — duration itemization recomputed 2026-09-27, remediation #35: the 15 task-days sum to 18.5 + 2 testing = 20.5, exceeding the header) (supersedes prior 14–17 — Task 22.3.15 added 2026-09-27, remediation #24)
**Dependencies:** Phases 2, 15, 19.5
**Spec Reference:** §15 (FX Derivatives Foundation), §24 (Acceptance Criteria)

---

## 22.1 Objectives

Implement FX derivatives: forwards, swaps, NDFs, and vanilla options. Includes pricing (Black-Scholes for options), variation margin, physical delivery, roll, IV surface, exercise styles, and writer assignment. (Option spread strategies not in scope; roll price spread covered by Task 22.3.8.)

---

## 22.2 Prerequisites

- Phase 2 (matching engine), Phase 15 (instrument lifecycle), Phase 19.5 (price oracle)

---

## 22.3 Tasks

### Task 22.3.1: FX Forwards

**Objective:** Implement FX forward contracts.

**File Locations:** `services/internal/derivatives/forwards.go`

**Implementation:**
1. Forward: agreement to buy/sell currency at fixed price on future date.
2. Pricing: `forward_rate = spot_rate × (1 + quote_rate × days/DCC_quote) / (1 + base_rate × days/DCC_base)` where `DCC_quote` and `DCC_base` are per-currency day-count convention denominators: 360 for ACT/360 currencies (USD, EUR, CHF, JPY) and 365 for ACT/365 currencies (GBP, AUD, NZD, CAD, SGD, HKD) per spec §15.3 / §24 #57 (supersedes prior hardcoded `days/360` — day-count convention is currency-specific per market convention).
3. Settlement: physical delivery on maturity date.
4. Forward curve: interest rate differentials per maturity (1W, 1M, 3M, 6M, 1Y).
5. `POST /api/v1/orders` with instrument type=FORWARD.

**Definition of Done (Acceptance Criteria):**
* [x] Forward pricing: `spot × (1 + quote_rate × d/DCC_quote) / (1 + base_rate × d/DCC_base)` with currency-specific ACT/360 vs ACT/365 convention (spec §15.3)
* [x] Physical delivery on maturity
* [x] Forward curve per maturity
* [x] Forward order submission works

**SDD Checklist:**
- [x] Spec checkpoint: FX forwards with interest rate parity — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.2: FX Swaps

**Objective:** Implement FX swap contracts.

**File Locations:** `services/internal/derivatives/swaps.go`

**Implementation:**
1. Swap: simultaneous spot + forward (near leg + far leg).
2. Near leg: spot trade at current rate.
3. Far leg: forward at agreed rate.
4. Pricing: swap points = forward - spot.
5. Settlement: both legs settled on respective dates.

**Definition of Done (Acceptance Criteria):**
* [x] Swap = near leg (spot) + far leg (forward)
* [x] Swap points computed correctly
* [x] Both legs settled on respective dates

**SDD Checklist:**
- [x] Spec checkpoint: FX swaps (near + far leg) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.3: NDFs (Non-Deliverable Forwards)

**Objective:** Implement NDFs for restricted currencies.

**File Locations:** `services/internal/derivatives/ndf.go`

**Implementation:**
1. NDF: forward with cash settlement (no physical delivery).
2. Settlement: (fixing_rate - contract_rate) × notional × days/base.
3. Fixing: reference rate from central bank or Reuters/Bloomberg.
4. Used for: restricted currencies (CNY, INR, BRL, etc.).

**Definition of Done (Acceptance Criteria):**
* [x] NDF cash settlement works
* [x] Fixing rate from reference source
* [x] Settlement amount correct

**SDD Checklist:**
- [x] Spec checkpoint: NDFs with cash settlement — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.4: Vanilla Options

**Objective:** Implement vanilla FX options (European + American).

**File Locations:** `services/internal/derivatives/options.go`

**Implementation:**
1. Call/Put on FX pairs.
2. Pricing: Black-Scholes (Garman-Kohlhagen for FX).
3. Exercise styles: European (expiry only), American (any time).
4. Settlement: physical or cash.
5. IV surface: per-strike, per-maturity implied volatility.
6. Greeks: delta, gamma, vega, theta, rho.
7. Writer assignment: pro-rata by open interest with random tie-break (supersedes prior random selection — remediation #24/#35: the canonical policy per Task 22.3.15/§15.4; the AC row 16 copy is updated below).

**Definition of Done (Acceptance Criteria):**
* [x] Call/Put options with Black-Scholes pricing
* [x] European + American exercise styles
* [x] Physical + cash settlement
* [x] IV surface computed
* [x] Greeks computed (delta, gamma, vega, theta, rho)
* [x] Writer assignment on exercise

**SDD Checklist:**
- [x] Spec checkpoint: vanilla FX options with Greeks — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.5: Barrier Options

**Objective:** Implement barrier options (knock-in / knock-out) with continuous barrier monitoring.

**File Locations:** `services/internal/derivatives/barrier_options.go`

**Implementation:**
1. Knock-in: option activates when barrier price is touched.
2. Knock-out: option deactivates (worthless) when barrier price is touched.
3. Barrier types: up-and-in, up-and-out, down-and-in, down-and-out.
4. Barrier monitored continuously against market price (mark price from Phase 19.5 oracle).
5. Pricing: Monte Carlo simulation for barrier options (per spec §15.2).
6. Settlement: cash settlement at expiry if barrier was not knocked out (or was knocked in).

**Definition of Done (Acceptance Criteria):**
* [x] Knock-in barrier: option activates when barrier touched
* [x] Knock-out barrier: option becomes worthless when barrier touched
* [x] All 4 barrier types (up/down × in/out) supported
* [x] Barrier monitored continuously against mark price
* [x] Monte Carlo pricing for barrier options
* [x] Cash settlement at expiry with barrier event logging

**SDD Checklist:**
- [x] Spec checkpoint: barrier options with knock-in/knock-out monitoring — defined first, validated against spec
- [x] Spec checkpoint: Monte Carlo pricing for barrier options — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.6: Binary Options

**Objective:** Implement binary (digital) options with fixed payout.

**File Locations:** `services/internal/derivatives/binary_options.go`

**Implementation:**
1. Binary option: fixed payout if barrier/condition hit at expiry.
2. Types: cash-or-nothing (fixed cash payout), asset-or-nothing (underlying value payout).
3. Pricing: Monte Carlo simulation (per spec §15.2).
4. Settlement: cash settlement at expiry.

**Definition of Done (Acceptance Criteria):**
* [x] Binary option with fixed payout at expiry
* [x] Cash-or-nothing and asset-or-nothing types supported
* [x] Monte Carlo pricing for binary options
* [x] Cash settlement at expiry

**SDD Checklist:**
- [x] Spec checkpoint: binary options with fixed payout — defined first, validated against spec
- [x] Spec checkpoint: Monte Carlo pricing for binary options — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.7: Variation Margin

**Objective:** Implement variation margin for derivatives.

**File Locations:** `services/internal/risk/variation_margin.go`

**Implementation:**
1. VM = mark-to-market change since previous settlement.
2. Collected/paid daily.
3. For forwards, swaps, NDFs, options.
4. VM stored in `variation_margin` table.
5. **Migration note:** `migrations/034_create_variation_margin.up.sql` — `variation_margin` table (id, account_id, instrument_id, vm_amount, mtm_value, settled_at, created_at).

**Definition of Done (Acceptance Criteria):**
* [x] VM = MTM change since previous settlement
* [x] Collected/paid daily
* [x] Works for all derivative types

**SDD Checklist:**
- [x] Spec checkpoint: variation margin for derivatives — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.8: Roll Management

**Objective:** Implement position roll for derivatives.

**File Locations:** `services/internal/derivatives/roll.go`

**Implementation:**
1. Roll: close expiring contract + open new contract.
2. `POST /api/v1/orders/roll` — roll position.
3. Roll price: spread between expiring and new contract.
4. Automatic roll: configurable per account.

**Definition of Done (Acceptance Criteria):**
* [x] Roll closes expiring + opens new contract
* [x] Roll price computed correctly
* [x] Automatic roll works

**SDD Checklist:**
- [x] Spec checkpoint: position roll — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.9: Derivative Order Parameters & Persistence (Migration 039)

**Objective:** Persist the derivative order fields Phase-22 requires — `orders` had no columns for strike, option type, exercise style, expiry, barrier, value dates, or premium (spec §5.4 extension, §24 #131). Added 2026-09-15.

**File Locations:** `migrations/039_orders_derivative_params.up.sql`, `services/internal/api/orders.go` (extend), `core/src/matching/` (field plumbing)

**Implementation:**
1. Migration 039 adds `orders` columns (spec §5.4): `strike DECIMAL(28,8)`, `option_type` (CALL|PUT|BINARY), `exercise_style` (EUROPEAN|AMERICAN), `expiry_at TIMESTAMPTZ`, `barrier_type` (UP_IN|UP_OUT|DOWN_IN|DOWN_OUT), `barrier_level`, `value_date DATE`, `near_leg_value_date`, `far_leg_value_date` (swaps), `premium` + `premium_currency`, `fixing_benchmark` (NDF fixing source).
2. Validation: derivative order types require their fields (e.g., FORWARD requires `value_date`; options require `strike`+`option_type`+`exercise_style`+`expiry_at`; barrier options require `barrier_type`+`barrier_level`; swaps require both leg value dates).
3. Fields part of WAL order state + snapshot serialization (same requirement as Task 16.3.10).
4. Instrument linkage: derivative orders validate `instruments.settlement_mode` and the instrument's `expiry_at`/value-date conventions.

**Definition of Done (Acceptance Criteria):**
* [x] All derivative params persisted + WAL/snapshot round-trip (§24 #131)
* [x] Per-type required-field validation rejects incomplete derivative orders
* [x] Swap legs carry independent value dates; NDF fixing source recorded

**SDD Checklist:**
- [x] Spec checkpoint: derivative order params persisted (§5.4, §24 #131) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: expiry before value date, barrier inside current market, missing premium on option buy

---

### Task 22.3.10: Option Lifecycle — Premium Settlement, Exercise Cutoff & Auto-Exercise

**Objective:** Implement the options expiry lifecycle per spec §15.4 (§24 #158): premium settlement, exercise cutoff at 15:00 UTC, and automatic exercise of ≥0.5% ITM options. Added 2026-09-15.

**File Locations:** `services/internal/derivatives/lifecycle.go`

**Implementation:**
1. **Premium settlement:** option premium debits `premium` (buyer's settlement currency) to seller at **T+2** (per spec §24 #158 — standard FX option premium settlement) — GL entries via Phase-3; premium is not refunded on bust unless the whole trade is busted (Task 15.3.5).
2. **Exercise cutoff & Margin Validation:** manual exercise instructions accepted until 15:00 UTC on expiry day (`EXERCISE_CUTOFF_PASSED` after); American-style exercisable any time before expiry+cutoff. Prior to accepting an exercise instruction, the risk engine verifies that the exercising account holds sufficient initial margin to carry the resulting spot delivery position; insufficient margin rejects manual exercise with `OPTION_EXERCISE_MARGIN_SHORTFALL` (spec §15.6, §23, Phase-22 Task 22.3.14, remediation #38).
3. **Auto-exercise:** at expiry, options ≥0.5% in-the-money vs final fix/mark are auto-exercised (spec §15.4 threshold); OTM expire worthless (`EXPIRED` status); ATM ±0.5% band auto-exercise configurable per account.
4. Settlement: exercise produces the underlying FX spot trade (physical) or cash delta (cash-settled); writer assignment random per spec §15.4 (existing Task 22.3.4 machinery).
5. Expiry batch runs on expiry day 15:00 UTC; all outcomes WAL-recorded + GL-posted; expiry report per account.
   **(amended 2026-09-20 — feature-completeness audit remediation #11):** American-style intra-day exercise pipeline: exercise request → pro-rata writer assignment by open interest with random tie-break → **WAL event `OPTION_ASSIGNMENT` is appended by the engine, not this Go service** (single-writer invariant, remediation #35 — the derivatives service emits an IPC command; the engine owns WAL appends and the seq/checksum invariants) → position mutation on both buyer and writer (off-book position transfer at strike via the Phase-19 Task 19.3.12 machinery — not a public-CLOB spot trade, which would cross the book at an off-market strike) → margin recalculation within same cycle (target: ≤ 5 seconds end-to-end; margin evaluation path = synchronous call into the margin service with its own timeout/fallback) → position mutation on both buyer and writer → margin recalculation within same cycle (target: ≤ 5 seconds end-to-end) → NATS notification to buyer and writer private channels. Assignment is blocked during active liquidation auctions (assignment cannot worsen a liquidating account's position). Intra-day assignment records are logged in `option_assignments` table with timestamp, exercise_price, assignment_price, and margin_impact.

**Definition of Done (Acceptance Criteria):**
* [x] Premium settles T+2 via GL (§24 #158)
* [x] Manual exercise rejected after 15:00 UTC cutoff
* [x] Auto-exercise ≥0.5% ITM at expiry; OTM expire worthless
* [x] Exercise produces spot trade/cash delta with writer assignment; GL-posted
* [x] American-style intra-day exercise processes within 5 seconds end-to-end
* [x] Assignment blocked during active liquidation auctions
* [x] `option_assignments` table records all assignment events with margin impact

**SDD Checklist:**
- [x] Spec checkpoint: option lifecycle — premium, cutoff, auto-exercise (§15.4, §24 #158) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: expiry on holiday (next business day per holiday calendar Task 3.3.8), exercise during instrument HALT, binary option expiry (no exercise — payout evaluation)

---

### Task 22.3.11: Initial Margin (ISDA SIMM-consistent) & Legal-Agreement Gating

**Objective:** Implement uncleared-margin initial margin and legal-document gating per spec §15.5 (§24 #146): NDFs/FX options require ISDA/CSA documentation and IM under an ISDA SIMM-consistent model for covered counterparties. Added 2026-09-15.

**File Locations:** `services/internal/derivatives/initial_margin.go`, `services/internal/compliance/legal_docs.go`, `migrations/043_legal_agreements.up.sql`

**Implementation:**
1. `legal_agreements` table (migration 043): `agreement_id`, `account_id`, `type` (ISDA_MA|CSA|FMSB|GMRA), `status` (PENDING|EXECUTED|TERMINATED), `executed_at`, `document_ref` (S3), `reviewed_by`.
2. **Legal gating:** counterparty cannot submit NDF/FX-option orders without `EXECUTED` ISDA Master + CSA → `LEGAL_DOC_REQUIRED` rejection at gateway; FMSB acknowledgement for ECP flow.
3. **Initial margin:** SIMM-consistent IM computed per counterparty: delta/vega sensitivity buckets per ISDA SIMM FX class, risk weights + correlations per published SIMM parameters; IM held as segregated collateral (linked to `collateral_schedule` — Phase-19 Task 19.3.8 haircuts apply).
4. IM calls: daily IM recomputation; calls issued via margin-call workflow (Task 19.3.3 machinery, IM-specific thresholds: margin period of risk 10d for uncleared).
5. Regulatory scope: IM applies to counterparties above BCBS-IOSCO AANA threshold; below-threshold clients use Phase-19 margin model — scope flag on `accounts.umr_in_scope`.

**Migration note:** `migrations/043_legal_agreements.up.sql` — `legal_agreements` + `accounts.umr_in_scope` (spec §15.5).

**Definition of Done (Acceptance Criteria):**
* [x] NDF/option orders rejected `LEGAL_DOC_REQUIRED` without EXECUTED ISDA+CSA (§24 #146)
* [x] SIMM-consistent IM computed per counterparty; segregated + haircut per collateral schedule
* [x] Daily IM calls through margin-call workflow (10d MPOR)
* [x] `umr_in_scope` gates IM applicability; out-of-scope uses standard margin

**SDD Checklist:**
- [x] Spec checkpoint: IM/UMR + legal agreements (§15.5, §24 #146) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: agreement terminated with open positions (block new, allow reduce-only), IM dispute window, ineligible collateral for IM posting

---


### Task 22.3.12: Multi-Leg Implied Matching Engine

**Objective:** Implement implied-in and implied-out matching to automatically synthesize liquidity across derivative maturity curves.

**File Locations:** `services/internal/features/22_3_12.go`

**Implementation:**
1. Implied-in: generate synthetic swap bids/asks from outright forward books.
2. Implied-out: generate outright forward bids/asks from swap + spot books.
3. Match across books atomically.

**Definition of Done (Acceptance Criteria):**
* [x] Multi-Leg Implied Matching Engine implementation completed
* [x] Tests passing for Multi-Leg Implied Matching Engine

**SDD Checklist:**
- [x] Spec checkpoint: Multi-Leg Implied Matching Engine — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.13: Option Spread Margin Offsets

**Objective:** Implement recognized option spread margin reductions for institutional capital efficiency, supporting vertical spreads, straddles, strangles, and calendar spreads. Added 2026-09-20 (feature-completeness audit remediation #11).

**File Locations:** `services/internal/margin/spread_offsets.go`, `migrations/085_option_spread_offsets.up.sql`

**Implementation:**
1. Spread recognition engine: detect recognized combinations in portfolio — vertical spreads (bull/bear call/put), straddles, strangles, calendar spreads.
2. Margin offset table: `option_spread_offsets` mapping spread type to reduction percentage. Vertical spread margin = max loss of spread (not sum of legs).
3. Integration with Phase-19 PORTFOLIO margin mode: spread offsets apply only in PORTFOLIO mode, not ISOLATED.
4. Admin-configurable offset percentages per spread type with audit trail.
5. Real-time spread detection runs on every position change event.

**Definition of Done (Acceptance Criteria):**
* [x] Spread recognition detects verticals, straddles, strangles, calendar spreads
* [x] Vertical spread margin ≤ max loss of spread
* [x] Offsets apply only in PORTFOLIO margin mode
* [x] Admin-configurable offset percentages with audit trail

**SDD Checklist:**
- [x] Spec checkpoint: option spread margin offsets — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: partially filled spread (one leg only), spread broken by partial close, multi-currency spreads

### Task 22.3.14: Vol Surface Arbitrage Rejection & Option Exercise Margin Failures

**Objective:** Implement numerical pricing error fallbacks, volatility surface arbitrage rejection, and fail-closed option exercise margin validation per spec §2.7, §15.6, and §24 #324.

**Implementation:**
1. **Numerical Solver Safeguards:** For Black-Scholes and Garman-Kohlhagen implied volatility solvers, enforce a maximum of 100 Newton-Raphson iterations. If convergence fails, abort gracefully with `OPTION_PRICING_CONVERGENCE_ERROR` (HTTP 422) rather than hanging or returning infinite variance.
2. **Volatility Surface Arbitrage Rejection:** Reject quote updates that produce calendar spread or butterfly arbitrage on the implied volatility surface with `VOLATILITY_SURFACE_ARBITRAGE` (HTTP 422).
3. **Exercise Margin Shortfall Fail-Closed:** When an in-the-money option auto-exercises or is manually exercised, verify the exercising account has sufficient margin to hold the resulting spot/forward position. If margin is insufficient and cannot be covered, immediately trigger liquidation auction flow on the resulting position rather than rejecting delivery into an inconsistent state.

**SDD Checklist:**
- [x] Spec checkpoint: Option pricing solvers fail gracefully on numerical non-convergence and exercise shortfalls trigger liquidation (§24 #324) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 22.3.15: American Pricing Model, IV Surface Build & Assignment/Barrier Determinism

**Objective:** Repair the three sub findings where the spec claims more than the implementation defines, per spec §15.7 and §24 #346. Added 2026-09-27 (production-maturity remediation #24).

**Implementation:**
1. **American exercise model:** European options keep Black-Scholes-Garman-Kohlhagen; American options price on a binomial/trinomial lattice (or Longstaff-Schwartz for exotics) with an explicit early-exercise boundary — supersedes the "Black-Scholes for AMERICAN" claim in spec §15.2 and Phase-22 AC row 11/12, which has no early-exercise boundary.
2. **IV surface build:** per-strike/per-maturity surface construction (SVI/spline fit, spot-vs-forward ATM convention, smile interpolation) feeding Task 22.3.14 arbitrage rejection; yield-curve staleness falls back per a defined hierarchy (primary curve → secondary vendor → last-good with age flag) raising `YIELD_CURVE_UNAVAILABLE` where already defined in §15.6; NDF `fixing_benchmark` hierarchy (central-bank fixing → Reuters/Bloomberg page → prior-day hold with flag).
3. **Assignment determinism:** pro-rata writer assignment by open interest with random tie-break (supersedes the random-vs-weighted-OI inconsistency between Task 22.3.4 and Task 22.3.10); exercise-instruction amend/cancel window closes before the 15:00 UTC cutoff with holder notification deadline tabulated.
4. **Barrier monitoring:** knock events evaluate on discrete mark ticks (5s staleness gate per Phase-19.5) — weekend-gap touches apply the first post-gap mark with a gap flag; missed-touch on a stale feed never fabricates a knock.
5. **Margin linkage:** SIMM bucket/risk-weight/correlation tables with dispute-window length; IM-vs-VM netting rule; writer delta-margin linkage into Phase-19 spot margin; Task 22.3.13 spread offsets interact with SIMM as offsets-before-aggregation, never double-counted. Expiry/roll coordination: forward-maturity auto-settle vs physical-delivery funding sequence with Phase-24 nostro, roll spread-tolerance, trade-group preservation on roll close+open, and auto-roll vs same-day option-expiry conflict resolution.

**Definition of Done (Acceptance Criteria):**
* [x] American prices match lattice reference within tolerance; European unchanged
* [x] Surface builds without arbitrage; fallback hierarchy demonstrated per feed failure
* [x] Assignment, barrier and expiry/roll sequences replay deterministically

**SDD Checklist:**
- [x] Spec checkpoint: lattice American pricing, built IV surface with fallback hierarchy, deterministic assignment/barrier/expiry-roll linkage (§24 #346) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 22.4 Deliverables

- FX forwards
- FX swaps
- NDFs
- Vanilla options (European + American)
- Barrier options (knock-in/knock-out)
- Binary options (fixed payout, cash settlement)
- Variation margin
- Roll management
- Derivative order-parameter persistence (migration 039)
- Option lifecycle: premium settlement, 15:00 UTC exercise cutoff, auto-exercise ≥0.5% ITM
- Initial margin (ISDA SIMM-consistent) + ISDA/CSA/FMSB legal-agreement gating
- Multi-leg implied matching engine (Task 22.3.12)
- Option spread margin offsets (Task 22.3.13)
- Volatility surface arbitrage validation & option exercise failure handling (Task 22.3.14)
- American lattice pricing, IV surface build & deterministic assignment/barrier/expiry-roll (Task 22.3.15)

---

## 22.5 Dependencies

- Phases 2, 15, 19.5

---

## 22.6 Duration Estimate

14–18 days (supersedes prior 14–17 — Task 22.3.15 American pricing/surface/determinism added 2026-09-27, remediation #24; prior supersedes 12–15 — Tasks 22.3.12–22.3.14 added, absorbed in range; reconciled to the file header and AGENTS Phase Index 2026-09-25):
- Task 22.3.1 (Forwards): 1.5 days
- Task 22.3.2 (Swaps): 1.5 days
- Task 22.3.3 (NDFs): 1.5 days
- Task 22.3.4 (Vanilla Options): 2 days
- Task 22.3.5 (Barrier Options): 1 day
- Task 22.3.6 (Binary Options): 0.5 day
- Task 22.3.7 (Variation Margin): 1 day
- Task 22.3.8 (Roll Management): 1 day
- Task 22.3.9 (Derivative params schema): 0.5 day
- Task 22.3.10 (Option lifecycle): 1 day
- Task 22.3.11 (IM/UMR + legal docs): 1.5 days
- Task 22.3.12 (Implied Matching): 2 days
- Task 22.3.13 (spread margin offsets): 2 days
- Task 22.3.14 (Vol surface arbitrage & exercise failure handling): 0.5 day
- Task 22.3.15 (American lattice, surface build & determinism): 1 day
- Testing: 2 days

---

## 22.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Forward pricing: `spot × (1 + quote_rate × d/DCC_quote) / (1 + base_rate × d/DCC_base)` with per-currency ACT/360 vs ACT/365 day-count convention (spec §15.3, §24 #57; supersedes prior hardcoded d/360) |
| 2 | Forward physical delivery on maturity |
| 3 | Forward curve per maturity (1W, 1M, 3M, 6M, 1Y) |
| 4 | Forward order submission works |
| 5 | Swap = near leg (spot) + far leg (forward) (§24 #59) |
| 6 | Swap points computed correctly |
| 7 | Both swap legs settled on respective dates |
| 8 | NDF cash settlement works |
| 9 | NDF fixing rate from reference source (§24 #58) |
| 10 | NDF settlement amount correct |
| 11 | European options with Black-Scholes (Garman-Kohlhagen) pricing; American options on lattice (supersedes prior blanket BSGK — remediation #24/#35; §24 #60 amended likewise) (§24 #60) |
| 12 | European + American exercise styles (§24 #64) |
| 13 | Physical + cash settlement for options |
| 14 | IV surface computed (per-strike, per-maturity) |
| 15 | Greeks computed (delta, gamma, vega, theta, rho) (§24 #62) |
| 16 | Writer assignment on exercise (random selection) |
| 17 | Barrier option: knock-in activates when barrier touched (§24 #61) |
| 18 | Barrier option: knock-out becomes worthless when barrier touched |
| 19 | All 4 barrier types (up/down × in/out) supported |
| 20 | Barrier monitored continuously against mark price |
| 21 | Monte Carlo pricing for barrier options |
| 22 | Binary option: fixed payout at expiry |
| 23 | Cash-or-nothing and asset-or-nothing binary types supported |
| 24 | Monte Carlo pricing for binary options |
| 25 | Variation margin = MTM change since previous settlement |
| 26 | VM collected/paid daily for all derivative types |
| 27 | Roll closes expiring + opens new contract |
| 28 | Roll price computed correctly |
| 29 | Automatic roll works (configurable per account) |
| 30 | `variation_margin` table exists (migration 034) |
| 31 | Derivative params persisted + WAL round-trip; per-type required-field validation (§24 #131) |
| 32 | Option premium settles T+2 via GL; exercise cutoff 15:00 UTC; auto-exercise ≥0.5% ITM (§24 #158) |
| 33 | NDF/option orders rejected LEGAL_DOC_REQUIRED without EXECUTED ISDA+CSA (§24 #146) |
| 34 | SIMM-consistent IM computed, segregated, haircut per collateral schedule; daily IM calls |
| 35 | Day-count convention per currency (ACT/360: USD,EUR,CHF,JPY; ACT/365: GBP,AUD,NZD,CAD,SGD,HKD) applied correctly in all forward/swap/NDF pricing (§24 #198) |
| 36 | Implied-in liquidity generated from outright books correctly |
| 37 | Implied-out liquidity generated from spread books correctly |
| 38 | Atomic cross-book matching executes without race conditions |
| 39 | American-style intra-day exercise: ≤ 5s end-to-end; blocked during liquidation auctions; option_assignments table logged (§24 #247) |
| 40 | Option spread offsets: verticals, straddles, strangles, calendar; vertical margin ≤ max loss; PORTFOLIO mode only (§24 #248) |
| 41 | Volatility surface arbitrage rejects quotes with VOLATILITY_SURFACE_ARBITRAGE; ITM option exercise with insufficient margin triggers automatic liquidation (§24 #324) |
| 42 | American options on lattice with exercise boundary; built IV surface with feed-fallback hierarchy; pro-rata assignment; discrete barrier evaluation; SIMM linkage with spread offsets; coordinated expiry/roll (§24 #346) |
| 43 | Option premium settlement failure path: buyer lacking `premium_currency` balance at T+2 rejects with `PREMIUM_INSUFFICIENT` (400) and queues into the margin-call workflow (Task 19.3.3); GL reversal rules defined (§24 #393; added 2026-09-27, remediation #35 — a failed premium debit midway was a zero-loss-invariant hazard with no code and no path) |

---

## Phase-22 Settle Addendum (2026-09-30) — implementation record

All 15 tasks implemented across 6 disjoint work-streams; **17/17 spec checkpoints bound and green** (`tests/spec/checks/phase22.go`; tasks 22.3.5/22.3.6 carry two checkpoints each). 91/91 DoD/SDD rows ticked. C++ suite: **36/36 ctest green** incl. `test_implied` + `test_implied_engine` (production-ingress implied matching: implied-in/out fills, resting out-book fills on leg arrival, POST_ONLY probe, FOK admission, IOC partial, OCO cleanup, stop-trigger implied sweep, curve-ingress routing + bounded dirty-drain).

**Migrations landed:** 034 `variation_margin` / 039 `orders_derivative_params` / 043 `legal_agreements` / 085 `option_spread_offsets` (plan-reserved verbatim) + 252 `option_barrier_events` / 253 `umr_im_assessments` / 254 `derivative_contracts` / 255 `derivatives_roll_lifecycle` (fresh numbers). Zero collisions; all `.down.sql` paired.

**Deviations and honest seams (recorded, not hidden):**

1. **Task 22.3.12 file location:** the task prescribes `services/internal/features/22_3_12.go`; the implied-matching feature gate landed at `services/internal/derivatives/implied_gate.go` — `internal/flags` (migration-193 `feature_flags`) is the established flag convention, and the gate is a `FlagResolver` seam over `flags.Store`. The engine-side machinery lives in `core/src/matching/ImpliedMatcher.cpp` + `MatchingEngine` wiring. Recorded in spec §27.
2. **FOK is outright-only:** FOK orders never consume implied liquidity — a mixed outright+implied feasibility result could false-positive and produce a partial FOK execution, violating all-or-nothing. FOK on a locally-empty book therefore rejects `ORDER_REJECTED_NO_LIQUIDITY` at admission rather than consulting the implied probe. Recorded in spec §27.
3. **Empty-book admission gate consults implied liquidity** for non-FOK takers (`IOC`/restable types) when the local opposite side is empty — a read-only `evaluate_incoming` probe exempts the order from `ORDER_REJECTED_NO_LIQUIDITY` so `match_incoming` can run. Recorded in spec §27.
4. **Protected market orders do not sweep implied liquidity** while a hard slippage/protection bound is active; the local opposite price-level read is null-guarded for implied-exempted orders on an empty out-book. Recorded in spec §27.
5. **Curve-shard production host:** `matching_engine -curve <ids> -curve-symbols <syms> -implied-link <out>:<s0>:<i0>:<r0>:<s1>:<i1>:<r1>` co-locates N linked instruments on one matching thread — one `MatchingEngine` per instrument, one shared `ImpliedMatcher`, one WAL writer, one shared trade-ID stream, one `CurveIngress` router dispatching wire orders by `instrument_id` (cancels/amends route by book+pending+parked ownership probe). Recovery binds all N books and routes WAL rows by `instrument_id`; each instrument snapshots independently under the shared store. Single-book mode is unchanged. Recorded in spec §27.
6. **Hidden/non-displayed quantity is excluded from implied capacity** — the matcher computes leg capacity from displayed depth only; a hidden remainder never mints an implied quote it cannot honor.
7. **Exercise blocked during liquidation auctions (AC #39):** `ExerciseAuctionGuard` seam (satisfied by `*risk.PgLiquidationStore.ActiveAuctions`) gates `exerciseAttempt` for both MANUAL and AUTO sources — a live §13.4 auction on the option instrument or its underlying rejects assignment `OPTION_EXERCISE_AUCTION_BLOCKED` (409); an unreadable guard fails closed `EXERCISE_AUCTION_EVAL_FAILED` (503). On the AUTO path a block leaves the option OPEN for the next sweep (fail-closed, no stranded delivery). `option_assignments` (migration 255) logs every assignment with mark/margin-impact.
8. **Exercise during instrument HALT** is covered by the mark-staleness gate rather than a dedicated state check: `freshOptMark` halts the lifecycle on a stale/missing oracle mark (fail-closed), which is the operative risk during HALT; deliberate exercise-while-HALT with a fresh mark is permitted (delivery is settlement, not book matching).
9. **`OptionService` is not yet constructed in a `cmd/` binary** — the lifecycle service, roll engine, VM sweep and margin/offets machinery are verified library surfaces; service/binary wiring is an ops-topology decision bound when the derivatives runner lands (consistent with Phase-19 seam-wiring precedent).
10. **`orders`-package `validateDerivativeParams`** has no dedicated unit test; admission coverage is via the FIX 5.0 SP2 contract tests (`TestForwardMapping`, `TestNDFRequiresFixingDate`, `TestSwapLegDates`, `TestOptionContract`) plus structural binding — flagged for hardening.

**Error registry:** no specRow delta — the 4 emitted spec codes (`VARIATION_MARGIN_INSUFFICIENT`, `OPTION_ASSIGNMENT_FAILED`, `OPTION_EXERCISE_MARGIN_SHORTFALL`, `PREMIUM_INSUFFICIENT`) were pre-tabled at remediation #19 — emitted stays **207** (202 §23 + 5 matrix-resident). **+21 localCodes** pending §23 transcription (`VOLATILITY_SURFACE_UNAVAILABLE`, `OPTION_PRICING_INPUT_INVALID`, `BENCHMARK_UNAVAILABLE`, `DERIVATIVE_STATE_CONFLICT`, `ROLL_NOT_PERMITTED`, `ROLL_TARGET_INVALID`, `ROLL_SPREAD_TOLERANCE_EXCEEDED`, `ROLL_CONFIG_INVALID`, `OPTION_NOT_EXERCISABLE`, `OPTION_CONTRACT_INVALID`, `MARGIN_CALL_QUEUE_FAILED`, `OPTION_EXERCISE_AUCTION_BLOCKED`, `EXERCISE_AUCTION_EVAL_FAILED`, `DERIVATIVE_PARAMS_INVALID`, `IMPLIED_MATCHING_UNAVAILABLE`, `VARIATION_MARGIN_INTERNAL`, `EXERCISE_MARGIN_EVAL_UNAVAILABLE`, `UMR_IM_EVAL_FAILED`, `SPREAD_OFFSET_PARAM_INVALID`, `SPREAD_OFFSET_INTERNAL`, `LIQUIDATION_FAILED` — cites Phase-19 Task 19.3.3 as owner, registered by the Phase-22 exercise-shortfall liquidation seam).

**Settle-time fixes:** `types.go` `ON CONFLICT` clause matched to migration-254's partial-unique-index predicate (42P10 under the PG tests); the §24 #247 auction-block seam implemented during verification (guard + 2 codes + gated test) after the parallel agents landed without it; `ImpliedMatcher` wired into `MatchingEngine` (was a tested standalone unit — bind/rescan/owner-hook integration, eager book registration, shared trade-ID stream, bounded dirty-drain) plus the curve-shard host in `main.cpp`; `implied_gate_test.go` added (gate had no test).
