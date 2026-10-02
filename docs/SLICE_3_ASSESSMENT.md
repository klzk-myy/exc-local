# FOREX Exchange System Suite — Completeness Assessment: Slice 3 (Domains 7, 8, 9)

> **⚠️ SUPERSEDED — historical record (2026-09-27, planning-stage snapshot).**
> All "Code Impl. 0%" / "Ready for Day 0 implementation" figures below describe the
> pre-code Day-0 baseline and are **no longer accurate**. The current evidence-based
> completeness record lives in **`IMP-PLAN.md` — Phase 2 (component assessment) and
> Phase 3 (remediation tasks 1–11)**: risk, compliance, and settlement surfaces are
> implemented and 354/419 §24 criteria are `EXECUTABLE` (65 `PLANNED` pending
> environment-gated evidence). Body preserved unchanged below as the original
> planning-baseline record.

**Assessment Date:** 2026-09-27  
**Scope:** Slice 3 — Domain 7 (Risk & Credit), Domain 8 (Compliance & AML), Domain 9 (Trade Lifecycle Ops)  
**Project State:** Planning-Stage Repository (100% Architecture & Specification / 0% Application Code Implementation)  
**Spec Reference:** `docs/Specification - Complete Exchange System Suite.md` (v7.0)  
**Primary Phase Plans:** `Phase-03`, `Phase-11`, `Phase-13`, `Phase-14`, `Phase-15`, `Phase-17`, `Phase-19`, `Phase-19.5`, `Phase-20`, `Phase-21`, `Phase-24`

---

## 1. Executive Summary: Slice 3 Assessment

Slice 3 covers the core institutional safety, regulatory compliance, and operational lifecycle infrastructure of the exchange:
- **Domain 7: Risk & Credit** (10 components): Multi-mode margin, leverage schedules, liquidation ladder, retail NBP, insurance fund governance, stress testing, PB credit (NOP/DSL), collateral haircuts, OTR/position limits, and bilateral credit groups.
- **Domain 8: Compliance & AML** (16 components): KYC tiers, PEP/adverse media screening, C++ pre-trade SanctionsHook, Travel Rule (IVMS 101), SAR/CTR filing, MiFID II (RTS 6/22/25/27/28), EMIR REFIT, CFTC Parts 43/45, Dodd-Frank, FinCEN MSB, GDPR, market-abuse surveillance & case management, comms taping (WORM), CRS/FATCA, Basel III capital adequacy, FX Global Code, and data residency.
- **Domain 9: Trade Lifecycle Ops** (12 components): FX settlement cycles (T+1/T+2/same-day), banking rails (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2), CLS PvP (ISO 20022), nostro/vostro management, zero-bypass double-entry General Ledger, Tom-Next spot rollover & swap engine, trade confirmations/statements (MT515), TCA engine, post-trade block allocations (FIX 35=J/AK), CSDR discipline, internal position transfers, and obvious-error trade busts.

### Quantitative Completeness Overview

| Domain | Components | Spec Coverage | Plan Coverage | §24 Criteria | Associated Migrations | Error Codes | Code Impl. | Readiness State |
|---|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|
| **7. Risk & Credit** | 10 | 100% | 100% | 42 | 18 migrations | 18 codes | 0% | Architectural contract 100% complete; Ready for Phase 03/19 implementation |
| **8. Compliance & AML** | 16 | 100% | 100% | 55 | 21 migrations | 24 codes | 0% | Regulatory reporting schemas & logic 100% complete; Ready for Phase 14/21 implementation |
| **9. Trade Lifecycle Ops** | 12 | 100% | 100% | 44 | 28 migrations | 22 codes | 0% | Banking rails, CLS, GL, & rollover models 100% complete; Ready for Phase 03/11/24 implementation |
| **TOTAL SLICE 3** | **38** | **100%** | **100%** | **141** | **67 unique migrations** | **64 unique codes** | **0%** | **Zero application code executed; Ready for Day 0 implementation** |

---

## 2. Domain 7: Risk & Credit

### 2.1 Domain Summary Metrics & Component Matrix

| # | Component | Spec Ref | Phase Tasks | §24 Criteria | Migrations | Error Codes | Code % | Done vs. Not Done Summary |
|---|---|---|---|---|---|---|:---:|---|
| **7.1** | **Margin Modes** (Cross, Isolated, Portfolio w/ 90d Corr) | §13.1, §13.6g, §13.15 | Phase-19: 19.3.1, 19.3.11, 19.3.18, 19.3.26, 19.3.27 | #32, #96, #176, #229, #230, #320, #398, #410, #411 | 013, 058, 085, 106 | `MARGIN_INSUFFICIENT` (L2), `CROSS_SHARD_TIMEOUT` (L2) | 0% | **Done:** 90d corr matrix formula, 20% floor, 2PC 500µs headroom.<br>**Not Done:** Go margin engine, C++ allocator, migrations unapplied. |
| **7.2** | **Leverage Tiers** (ESMA / CFTC / Dynamic Institutional) | §13.2, §13.6f, §13.14 | Phase-19: 19.3.2, 19.3.17, 19.3.23, 19.3.24 | #97, #231, #366, #371 | 001, 011, 098 | `LEVERAGE_EXCEEDS_TIER_MAX` (L2), `INVALID_LEVERAGE` (L3) | 0% | **Done:** ESMA 30:1/20:1/10:1, CFTC 50:1/20:1, notional bands.<br>**Not Done:** Go leverage validator, C++ pre-trade limit hooks. |
| **7.3** | **Liquidation Ladder** (2s Scanner, Auction, Decay, Force Cash) | §13.3, §13.4, §13.5 | Phase-19: 19.3.3, 19.3.16, 19.3.19, 19.3.22, 19.3.26; Phase-19.5: 19.5.3.6 | #32, #33, #34, #35, #36, #37, #120, #230, #241, #269, #360, #397 | 014, 015 | `LIQUIDATION_IN_PROGRESS` (L2), `FORCE_CASH_FAILED` (L1) | 0% | **Done:** 2s cadence, 111.1% margin call (15m), 4-phase auction, 0.5% decay.<br>**Not Done:** Go scanner daemon, auction orderbook, LP rebate payouts. |
| **7.4** | **Retail Negative Balance Protection (NBP)** | §13.6c, §2.7.1 | Phase-19: 19.3.9, 19.3.20; Phase-03: 3.3.6 | #133, #228, #320 | 016, 036, 102 | `NBP_DEFICIT_TRIGGERED` (L1), `INSURANCE_FUND_EXHAUSTED` (L0) | 0% | **Done:** ESMA retail guarantee, Insurance Fund deficit absorption, GL journal.<br>**Not Done:** Go deficit resolution service, automated GL write-off posting. |
| **7.5** | **Insurance Fund & Governance** | §13.4, §13.6c, §13.12, §17.13 | Phase-19: 19.3.4, 19.3.14, 19.3.21; Phase-24: 24.3.17 | #35, #36, #97, #218, #344 | 016, 082 | `INSURANCE_FUND_DEPLETED` (L1/L0), `ADL_TRIGGERED` (L1) | 0% | **Done:** 4-tier waterfall (Fund→House→Capital→ADL), VaR sizing.<br>**Not Done:** Capital replenishment automation, live equity monitoring daemon. |
| **7.6** | **Stress Testing & Backtesting** | §13.10 | Phase-19: 19.3.13, 19.3.21 | #206, #344 | 064 | `MODEL_VALIDATION_FAILED` (L1) | 0% | **Done:** Historical shock scenarios (CHF unpeg, Brexit), Kupiec POF tests.<br>**Not Done:** Go simulation pipeline, daily risk calculation jobs. |
| **7.7** | **PB Credit Limits** (NOP / DSL Pre-Trade Check) | §13.7 | Phase-19: 19.3.7; Phase-18: 18.3.6 | #125 | 037 | `PB_CREDIT_EXCEEDED` (L2), `NOP_LIMIT_EXCEEDED` (L2) | 0% | **Done:** Pre-trade NOP/DSL calculation in USD, sub-ms Redis reservation.<br>**Not Done:** Go PB credit manager, Redis atomic reservation scripts. |
| **7.8** | **Collateral Haircuts & Concentration** | §13.6b, §13.15 | Phase-19: 19.3.8, 19.3.28 | #145, #412 | 041 | `COLLATERAL_NOT_ELIGIBLE` (L2), `CONCENTRATION_LIMIT_EXCEEDED` (L2) | 0% | **Done:** Haircut schedule by asset tier, 10d volatility scaler, concentration caps.<br>**Not Done:** Live collateral valuation service, dynamic haircut re-evaluator. |
| **7.9** | **Position / OTR Limits** (MiFID II RTS 9) | §13.6, §13.6a | Phase-13: 13.3.6; Phase-19: 19.3.5 | #140 | 011, 047 | `MAX_EXPOSURE_EXCEEDED` (L2), `OTR_LIMIT_EXCEEDED` (L2) | 0% | **Done:** RTS 9 count/volume OTR formula, rolling window, throttle tiers.<br>**Not Done:** Go OTR tracking middleware, rate-limiting circuit breakers. |
| **7.10**| **Credit Groups** (Mutual Bilateral Credit) | §13.8 | Phase-19: 19.3.10 | #165 | 053 | `BILATERAL_CREDIT_EXHAUSTED` (L2), `NO_MUTUAL_CREDIT` (L2) | 0% | **Done:** 2-way headroom check, ONE_POOL/TWO_POOL, credit-screened feeds.<br>**Not Done:** C++ `CreditScreen.cpp`, Go bilateral reservation engine. |

### 2.2 Detailed Component Breakdown (Domain 7)

#### 7.1 Margin Modes (Cross, Isolated, Portfolio with 90-Day Correlation Matrix)
- **Coverage:** Spec 100% (§13.1, §13.6g, §13.15), Phase Planning 100% (Tasks 19.3.1, 19.3.11, 19.3.18, 19.3.26, 19.3.27).
- **Traceability:** §24 Criteria #32, #96, #176, #229, #230, #320, #398, #410, #411; Phase 19 AC #1–4, #11, #16, #26, #27, #37, #45, #55, #63–65.
- **Migrations:** `013_create_margin_accounts`, `058_shard_margin_reservations`, `085_option_spread_offsets`, `106_positions_isolated_margin`.
- **Error Codes & Hierarchy:** `MARGIN_INSUFFICIENT` (L2, HTTP 400), `INVALID_MARGIN_MODE` (L3, HTTP 400), `CROSS_SHARD_TIMEOUT` (L2, HTTP 504), `MARGIN_CALL_ACTIVE` (L2, HTTP 409).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Mathematical specification of Cross Margin (shared collateral across all positions), Isolated Margin (dedicated collateral sub-allocation per position per migration 106), and Portfolio Margin with 90-day historical correlation matrix. Defined offset equation: $\text{offset} = \min(m_1, m_2) \times \rho \times f_{\text{offset}}$ for pairs with $|\rho| > 0.7$ (offset factor 0.5 default, 0.8 max), capped at a 20% gross margin regulatory floor. Layered cross-shard margin protocol specified: 500µs RPC budget, pessimistic local reservation floor, and hard 5s cancellation/compensation timeout.
- **What Has Not Been Done:** Go margin service in `services/internal/margin/`, C++ headroom allocator, PostgreSQL schemas unapplied, daily correlation calculation jobs unwritten.

#### 7.2 Leverage Tiers (ESMA Retail, CFTC, Institutional Dynamic Notional Tiers)
- **Coverage:** Spec 100% (§13.2, §13.6f, §13.14), Phase Planning 100% (Tasks 19.3.2, 19.3.17, 19.3.23, 19.3.24).
- **Traceability:** §24 Criteria #97, #231, #366, #371; Phase 19 AC #5–8, #36, #58–60.
- **Migrations:** `001_create_instruments`, `011_create_risk_limits`, `098_entity_leverage_policy`.
- **Error Codes & Hierarchy:** `LEVERAGE_EXCEEDS_TIER_MAX` (L2, HTTP 400), `LEVERAGE_UPDATE_FORBIDDEN` (L2, HTTP 403), `INVALID_LEVERAGE` (L3, HTTP 400).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Full rule table specified: ESMA retail caps (30:1 for major FX pairs, 20:1 for minor pairs, 10:1 for exotics); CFTC retail caps (50:1 major FX, 20:1 minor FX); institutional dynamic tiered leverage declining across aggregate notional exposure bands ($0–$5M @ 100:1, $5M–$20M @ 50:1, $20M–$50M @ 20:1, >$50M @ 10:1); entity-level leverage policy matrix with "most restrictive wins" resolution across account, instrument, and legal jurisdiction.
- **What Has Not Been Done:** Pre-trade check validation in C++ `PreTradeChecker`, runtime leverage modification handler in Go, unapplied migration 098.

#### 7.3 Liquidation Ladder (2s Scanner, Auction, Decay, Force Cash, LP Rebates)
- **Coverage:** Spec 100% (§13.3, §13.4, §13.5), Phase Planning 100% (Tasks 19.3.3, 19.3.16, 19.3.19, 19.3.22, 19.3.26; Phase-19.5 Task 19.5.3.6).
- **Traceability:** §24 Criteria #32–37, #120, #230, #241, #269, #360, #397; Phase 19 AC #9–15, #35, #43, #57, #62; Phase 19.5 AC #6, #16.
- **Migrations:** `014_create_positions`, `015_create_liquidation_auctions`.
- **Error Codes & Hierarchy:** `LIQUIDATION_IN_PROGRESS` (L2, HTTP 409), `AUCTION_ACTIVE` (L2, HTTP 409), `FORCE_CASH_FAILED` (L1, HTTP 500), `STALE_PRICE_LIQUIDATION_ACTIVE` (L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Rigorous liquidation state machine: 2s scanner cadence evaluating all Cross/Portfolio accounts; margin call warning triggered at margin level $\le 111.1\%$ with a 15-minute deposit window; stop-out at $\le 50\%$ voids window; auction triggered when liquidated notional $> 1\%$ open interest; 4-phase auction: CALL (5s), FILL (continuous), EXTEND (up to 60s total in 5s increments with 0.5% price decay per increment); auction floors at $\text{liquidation\_price} \times 0.98 / 1.02$; FORCE_CASH fallback at mark $\times 0.95 / 1.05$; 0.05% LP rebate paid from insurance fund; ADV-proportional slicing for positions $> 5\%$ ADV.
- **What Has Not Been Done:** Liquidation daemon in `services/internal/risk/liquidation.go`, auction matching logic in C++, timer routines, unapplied migrations 014/015.

#### 7.4 Retail Negative Balance Protection (NBP)
- **Coverage:** Spec 100% (§13.6c, §2.7.1), Phase Planning 100% (Tasks 19.3.9, 19.3.20; Phase-03 Task 3.3.6).
- **Traceability:** §24 Criteria #133, #228, #320; Phase 19 AC #22, #23, #46, #53.
- **Migrations:** `016_create_insurance_fund`, `036_create_general_ledger`, `102_wallets_ledger_entries`.
- **Error Codes & Hierarchy:** `NBP_DEFICIT_TRIGGERED` (L1, Internal Event), `INSURANCE_FUND_EXHAUSTED` (L0, Critical Halt).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Regulatory mandate modeled per ESMA rules: retail accounts (`client_category == RETAIL`) can never lose more than deposited funds; negative equity post-liquidation is automatically written off to the insurance fund; balanced GL double-entry journal entries specified (Debit Insurance Fund Expense, Credit Client Receivable); account equity floored at zero.
- **What Has Not Been Done:** Unwritten Go NBP service in `services/internal/margin/nbp.go`, no GL posting execution, unapplied migrations 016/036/102.

#### 7.5 Insurance Fund & Governance
- **Coverage:** Spec 100% (§13.4, §13.6c, §13.12, §17.13), Phase Planning 100% (Tasks 19.3.4, 19.3.14, 19.3.21; Phase-24 Task 24.3.17).
- **Traceability:** §24 Criteria #35, #36, #97, #218, #344; Phase 19 AC #12, #13, #31, #32, #54; Phase 24 AC #33, #34.
- **Migrations:** `016_create_insurance_fund`, `082_treasury`.
- **Error Codes & Hierarchy:** `INSURANCE_FUND_DEPLETED` (L1/L0), `ADL_TRIGGERED` (L1, P1 Alert).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Comprehensive capital backstop architecture: initial capital sizing tied to portfolio VaR ($3\times$ 99.9% 1-day VaR); continuous fee replenishment (portion of liquidation penalty and liquidation spread); 4-tier waterfall fallback: (1) Insurance Fund $\rightarrow$ (2) Venue House Funds $\rightarrow$ (3) Contingent Capital Backstop $\rightarrow$ (4) Auto-Deleveraging (ADL); 5-level ADL ranking algorithm.
- **What Has Not Been Done:** Go capital monitor, automated GL replenishment sweeps, ADL execution daemon, unapplied migrations 016/082.

#### 7.6 Stress Testing & Backtesting (Margin Model Validation)
- **Coverage:** Spec 100% (§13.10), Phase Planning 100% (Tasks 19.3.13, 19.3.21).
- **Traceability:** §24 Criteria #206, #344; Phase 19 AC #29, #30, #54.
- **Migrations:** `064_margin_model_runs`.
- **Error Codes & Hierarchy:** `MODEL_VALIDATION_FAILED` (L1, Admin Block).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Full validation methodology specified: daily stress-testing against historical extreme currency market scenarios (2015 Swiss Franc unpeg, 2016 Brexit flash crash, 2020 COVID FX volatility spikes) plus hypothetical multi-standard-deviation shocks; daily backtesting over 250-day rolling returns; Kupiec POF and Christoffersen independence statistical testing; model run auditing in `margin_model_runs`.
- **What Has Not Been Done:** Go stress testing engine, historical price scenario replay pipeline, statistical test suite, unapplied migration 064.

#### 7.7 Prime Broker Credit Limits (NOP / DSL Pre-Trade Check)
- **Coverage:** Spec 100% (§13.7, §5.22), Phase Planning 100% (Phase-19 Task 19.3.7; Phase-18 Task 18.3.6).
- **Traceability:** §24 Criteria #125; Phase 19 AC #17–19; Phase 18 AC #13, #14.
- **Migrations:** `037_create_prime_brokerage`.
- **Error Codes & Hierarchy:** `PB_CREDIT_EXCEEDED` (L2, HTTP 400 / FIX Tag 35=8 OrdRejReason=16), `NOP_LIMIT_EXCEEDED` (L2), `DSL_LIMIT_EXCEEDED` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Pre-trade institutional risk filter: computes Net Open Position (NOP, aggregate net currency exposure converted to USD via real-time oracle) and Daily Settled Limit (DSL, cumulative executed notional per value date) against remaining PB headroom; sub-millisecond Redis cache reservation with rollback on order rejection; breach alerting.
- **What Has Not Been Done:** Go PB credit daemon in `services/internal/risk/pb_credit.go`, Redis Lua reservation scripts, unapplied migration 037.

#### 7.8 Collateral Haircuts & Concentration Limits
- **Coverage:** Spec 100% (§13.6b, §13.15, §5.23), Phase Planning 100% (Phase-19 Tasks 19.3.8, 19.3.28).
- **Traceability:** §24 Criteria #145, #412; Phase 19 AC #20, #21, #66.
- **Migrations:** `041_collateral_schedule`.
- **Error Codes & Hierarchy:** `COLLATERAL_NOT_ELIGIBLE` (L2, HTTP 400), `CONCENTRATION_LIMIT_EXCEEDED` (L2, HTTP 400), `HAIRCUT_THRESHOLD_BREACH` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Multi-asset collateral schedule: Cash USD/EUR/GBP (0% haircut), minor currencies (2–5%), US Treasuries (1–3%), Gold (10%); concentration limit rules (e.g. max 40% non-base currency collateral, max 20% single corporate bond); intraday dynamic haircut re-evaluation using 10-day volatility scaler ($h_{\text{dyn}} = h_{\text{base}} \times \max(1.0, \sigma_{10d} / \sigma_{\text{baseline}})$).
- **What Has Not Been Done:** Go collateral service in `services/internal/risk/collateral.go`, live collateral valuation engine, unapplied migration 041.

#### 7.9 Position & Order-to-Trade Ratio (OTR) Limits (MiFID II RTS 9)
- **Coverage:** Spec 100% (§13.6, §13.6a, §5.25), Phase Planning 100% (Phase-13 Task 13.3.6; Phase-19 Task 19.3.5).
- **Traceability:** §24 Criteria #140; Phase 13 AC #19, #20; Phase 19 AC #14.
- **Migrations:** `011_create_risk_limits`, `047_risk_limits_otr`.
- **Error Codes & Hierarchy:** `MAX_EXPOSURE_EXCEEDED` (L2, HTTP 400), `OTR_LIMIT_EXCEEDED` (L2, HTTP 429), `POSITION_LIMIT_EXCEEDED` (L2, HTTP 400).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** MiFID II RTS 9 compliant OTR tracking: computes both volume-based and count-based order-to-trade ratios over rolling evaluation windows; tiered penalty structure and automated rate throttling for non-compliant algorithmic traders; account and instrument net notional exposure caps.
- **What Has Not Been Done:** Go OTR tracking middleware in API gateway, C++ exposure limit filters, unapplied migrations 011/047.

#### 7.10 Bilateral Credit Groups & Mutual Screening
- **Coverage:** Spec 100% (§13.8, §5.30), Phase Planning 100% (Phase-19 Task 19.3.10).
- **Traceability:** §24 Criteria #165; Phase 19 AC #24, #25.
- **Migrations:** `053_parser` / `053_bilateral_credit`.
- **Error Codes & Hierarchy:** `BILATERAL_CREDIT_EXHAUSTED` (L2, HTTP 400), `CREDIT_GROUP_MISMATCH` (L2, HTTP 403), `NO_MUTUAL_CREDIT` (L2, HTTP 403).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Principal-to-principal mutual bilateral credit architecture: candidate matches require active buyer $\rightarrow$ seller and seller $\rightarrow$ buyer headroom; ONE_POOL and TWO_POOL profiles; atomic two-way reservations before match commit; customized credit-screened FIX and SBE order book views suppressing inaccessible liquidity.
- **What Has Not Been Done:** C++ `core/src/risk/CreditScreen.cpp`, Go bilateral credit coordinator, unapplied migration 053.

---

## 3. Domain 8: Compliance & AML

### 3.1 Domain Summary Metrics & Component Matrix

| # | Component | Spec Ref | Phase Tasks | §24 Criteria | Migrations | Error Codes | Code % | Done vs. Not Done Summary |
|---|---|---|---|---|---|---|:---:|---|
| **8.1** | **KYC Tiers & Onboarding** (T0/T1/T2/Inst) | §14.2, §12.7 | Phase-14: 14.3.4, 14.3.7, 14.3.10; Phase-12: 12.3.13 | #27, #101, #102, #141, #385 | 017, 042, 095, 099 | `KYC_TIER_EXCEEDED` (L2), `KYC_VERIFICATION_REQUIRED` (L2) | 0% | **Done:** 4-tier model, document validation rules, SSE-KMS encryption.<br>**Not Done:** Go KYC service, Jumio/Onfido integration, migrations unapplied. |
| **8.2** | **PEP & Adverse Media Screening** | §14.3 | Phase-21: 21.3.11 | #149 | 017, 033, 060 | `PEP_MATCH_FLAGGED` (L2), `ADVERSE_MEDIA_ALERT` (L2) | 0% | **Done:** Daily batch & onboarding screening, EDD triggers, SAR linkage.<br>**Not Done:** World-Check / Dow Jones feed adapters, Go worker daemon. |
| **8.3** | **Sanctions Screening & PreTrade Hook** | §14.3, §7.5, §2.7 | Phase-21: 21.3.1, 21.3.10, 21.3.23 | #28, #29, #219, #323 | 009, 060 | `SANCTIONS_BLOCKED` (L2), `SANCTIONS_QUARANTINE` (L1) | 0% | **Done:** C++ `<10µs` PreTrade SanctionsHook, fail-closed scoped degradation.<br>**Not Done:** C++ `SanctionsHook.cpp`, Go list ingestor, live list feeds. |
| **8.4** | **Travel Rule (FATF / IVMS 101)** | §14.3, §5.19 | Phase-21: 21.3.2 | #107 | 032 | `TRAVEL_RULE_MISSING_INFO` (L2), `TRAVEL_RULE_REJECTED` (L2) | 0% | **Done:** IVMS 101 data schema for $\ge \$1,000$ transfers, TRISA/Notabene hooks.<br>**Not Done:** Go Travel Rule engine, IVMS 101 parser, unapplied migration 032. |
| **8.5** | **SAR / CTR Filing** (FinCEN Form 107) | §14.1, §14.3 | Phase-21: 21.3.3, 21.3.6 | #105, #108 | 033, 060 | `CTR_TRIGGERED` (Event), `SAR_DUAL_CONTROL_REQUIRED` (L2) | 0% | **Done:** Automated CTR $\ge \$10,000$, SAR dual-control workflow, BSA XML schema.<br>**Not Done:** Go BSA generator, FinCEN E-Filing gateway, unapplied migration 033. |
| **8.6** | **MiFID II Compliance** (RTS 6/22/25/27/28) | §14.1, §14.5, §16 | Phase-21: 21.3.4, 21.3.12, 21.3.16, 21.3.19; Phase-09: 9.3.12 | #30, #106, #150, #178, #202, #246, #345 | 054, 059 | `MIFID_REPORTING_FAILED` (L1/L2), `CLOCK_SKEW_EXCEEDED` (L0/L1) | 0% | **Done:** RTS 6 kill switches, RTS 22 ARM reporting, RTS 25 PTP clock sync, RTS 27/28.<br>**Not Done:** UnaVista / Trax ARM adapters, PTP daemon, unapplied migrations. |
| **8.7** | **EMIR REFIT & CFTC Parts 43/45** | §14.1a, §5.32 | Phase-21: 21.3.5, 21.3.14 | #31, #169, #170, #345 | 054, 059 | `INVALID_UTI_FORMAT` (L2), `TR_SUBMISSION_REJECTED` (L1/L2) | 0% | **Done:** ISO 20022 XML TR reports, UTI/UPI/LEI generation, lifecycle reporting.<br>**Not Done:** DTCC / Regis-TR Trade Repository adapters, Go reporting daemon. |
| **8.8** | **Dodd-Frank Swap Reporting** | §14.1a | Phase-21: 21.3.9, 21.3.14 | #170, #345 | 054, 059 | `DODD_FRANK_REPORT_FAILED` (L1/L2), `SDR_UNAVAILABLE` (L1) | 0% | **Done:** Real-time Part 43 public dissemination & Part 45 regulatory reporting.<br>**Not Done:** Go SDR reporter service, SDR message schema formatting. |
| **8.9** | **FinCEN MSB Program** | §14.1 | Phase-21: 21.3.6 | #105, #108 | 033, 060 | `MSB_COMPLIANCE_BREACH` (L1/L2) | 0% | **Done:** Complete MSB operational policy, CIP verification, audit log models.<br>**Not Done:** Admin compliance dashboard, automated AML monitoring cron. |
| **8.10**| **GDPR & Privacy Controls** | §14.1, §14.7 | Phase-21: 21.3.7 | #103, #104, #342, #383 | 002, 009, 060 | `GEO_BLOCKED` (L2), `GDPR_ERASURE_RESTRICTED` (L2) | 0% | **Done:** Consent tracking, export API, 7-year regulatory AML erasure carve-out.<br>**Not Done:** Go GDPR handlers, MaxMind GeoIP integration, unapplied migration 002. |
| **8.11**| **Surveillance & Case Management** | §11, §14.1c, §14.4 | Phase-17: 17.3.3; Phase-21: 21.3.8, 21.3.21, 21.3.27 | #18, #207, #274, #392 | 029, 054 | `MARKET_ABUSE_DETECTED` (L2), `SURVEILLANCE_LAG_WARNING` (L1) | 0% | **Done:** 7 L3 signals (spoofing, layering, etc.), case SLA (4h/24h), disposition paths.<br>**Not Done:** Go L3 event stream processor, compliance case management UI. |
| **8.12**| **Communications Recording (Taping)** | §14.8 | Phase-21: 21.3.20 | #203 | 062 | `TAPING_STORAGE_ERROR` (L1/L0), `WORM_BREACH` (L0) | 0% | **Done:** S3 Object Lock in WORM mode, 5-year retention, tamper-evident logs.<br>**Not Done:** WORM S3 infrastructure deployment, voice/chat capture daemon. |
| **8.13**| **CRS / FATCA Tax Reporting** | §14.1 | Phase-21: 21.3.22; Phase-20: 20.3.10 | #249 | 054, 059 | `TAX_REPORT_GENERATION_FAILED` (L2), `INVALID_TIN_FORMAT` (L3) | 0% | **Done:** IRS Form 8966 XML & OECD CRS XML v2.0 generation, FIFO/LIFO tax tools.<br>**Not Done:** Go XML tax generator, schema validation suite, unapplied migrations. |
| **8.14**| **Basel III Capital Adequacy** | §14.1 | Phase-21: 21.3.13 | #151 | 054, 082 | `CAPITAL_ADEQUACY_BREACH` (L1/L0), `LEVERAGE_RATIO_BREACH` (L1) | 0% | **Done:** RWA formulas (FRTB market risk, SA-CCR credit risk), $\ge 3\%$ leverage ratio.<br>**Not Done:** Capital calculation jobs, COREP reporting output generation. |
| **8.15**| **FX Global Code Assessment** | §14.6 | Phase-21: 21.3.17 | #182 | 060 | `FX_GLOBAL_CODE_NON_COMPLIANT` (Audit Warning) | 0% | **Done:** 55-principle evaluation matrix, Principle 17 no-last-look firm quote model.<br>**Not Done:** Annual compliance review UI, Statement of Commitment engine. |
| **8.16**| **Data Residency Enforcer** | §14.7 | Phase-21: 21.3.18 | #184 | 060 | `CROSS_BORDER_ROUTING_PROHIBITED` (L2) | 0% | **Done:** Multi-region storage partitioning isolating EU/UK/US data, DB RLS rules.<br>**Not Done:** Multi-region PostgreSQL setup, geographic request routing proxy. |

### 3.2 Detailed Component Breakdown (Domain 8)

#### 8.1 KYC Tiers & Onboarding Lifecycle
- **Coverage:** Spec 100% (§14.2, §12.7, §5.10), Phase Planning 100% (Tasks 14.3.4, 14.3.7, 14.3.10; Phase-12 Task 12.3.13).
- **Traceability:** §24 Criteria #27, #101, #102, #141, #385; Phase 14 AC #7–10, #14–16.
- **Migrations:** `017_create_kyc_documents`, `042_client_categorization`, `095_account_products_swapfree`, `099_product_target_markets`.
- **Error Codes & Hierarchy:** `KYC_TIER_EXCEEDED` (L2, HTTP 403), `KYC_VERIFICATION_REQUIRED` (L2, HTTP 403), `INVALID_KYC_DOCUMENT` (L3, HTTP 400).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** 4-tier model fully specified: Tier 0 (unverified: read-only, deposit only, 0 withdrawal, 0 trading), Tier 1 (identity verified: $10,000/day withdrawal, retail leverage), Tier 2 (full proof of address & income: $100,000/day withdrawal, full leverage), Institutional (manual review: LEI, beneficial ownership, audited financials); automated expiry tracking and re-verification triggers; SSE-KMS document encryption at rest.
- **What Has Not Been Done:** Go KYC service in `services/internal/kyc/`, Jumio/Onfido API client, file upload processing worker, unapplied migrations 017/042/095/099.

#### 8.2 PEP & Adverse Media Screening
- **Coverage:** Spec 100% (§14.3, §5.10), Phase Planning 100% (Phase-21 Task 21.3.11).
- **Traceability:** §24 Criteria #149; Phase 21 AC #23, #24.
- **Migrations:** `017_create_kyc_documents`, `033_create_sar_reports`, `060_compliance_assessments`.
- **Error Codes & Hierarchy:** `PEP_MATCH_FLAGGED` (L2, Internal Review), `ADVERSE_MEDIA_ALERT` (L2, Internal).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Pre-onboarding and daily batch screening specified against PEP databases and adverse media feeds; risk scoring algorithm with automated escalation to Enhanced Due Diligence (EDD) and SAR triggers; compliance audit logging in `compliance_assessments`.
- **What Has Not Been Done:** Adapters for World-Check / Dow Jones feeds, Go screening daemon, unapplied migrations.

#### 8.3 Sanctions Screening & C++ PreTrade SanctionsHook
- **Coverage:** Spec 100% (§14.3, §7.5, §2.7), Phase Planning 100% (Phase-21 Tasks 21.3.1, 21.3.10, 21.3.23).
- **Traceability:** §24 Criteria #28, #29, #219, #323; Phase 21 AC #1–3, #21, #22, #43, #44.
- **Migrations:** `009_create_audit_hash_chain`, `060_compliance_assessments`.
- **Error Codes & Hierarchy:** `SANCTIONS_BLOCKED` (L2, HTTP 403), `SANCTIONS_SERVICE_UNAVAILABLE` (L1, HTTP 503), `SANCTIONS_QUARANTINE` (L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Two-layer zero-latency architecture: (1) C++ `SanctionsHook` in `PreTradeChecker` using an in-process bitmap/bloom filter updated via Aeron/Redis with `<10\mu s` latency budget; orders from sanctioned accounts rejected immediately, while cancels remain permitted; (2) Go background screening for deposits/withdrawals against OFAC, EU, UN, and UK HMT lists; fail-closed behavior on provider downtime with scoped degradation.
- **What Has Not Been Done:** C++ implementation in `core/src/risk/PreTradeChecker.cpp` and `SanctionsCache.cpp`, list download ingestion worker, live sanctions API integration.

#### 8.4 Travel Rule (FATF Recommendation 16 / IVMS 101)
- **Coverage:** Spec 100% (§14.3, §5.19), Phase Planning 100% (Phase-21 Task 21.3.2).
- **Traceability:** §24 Criteria #107; Phase 21 AC #4, #5.
- **Migrations:** `032_create_travel_rule_records`.
- **Error Codes & Hierarchy:** `TRAVEL_RULE_MISSING_INFO` (L2, HTTP 400), `TRAVEL_RULE_COUNTERPARTY_REJECTED` (L2, HTTP 422).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Full IVMS 101 data standard specified for transfers $\ge \$1,000$ (originator name, account number, address/ID, beneficiary name and account); integration protocol defined for TRISA and Notabene networks; persistence in `travel_rule_records`.
- **What Has Not Been Done:** Go Travel Rule engine in `services/internal/compliance/travel_rule.go`, IVMS 101 serializer, unapplied migration 032.

#### 8.5 SAR & CTR Regulatory Reporting (FinCEN Form 107)
- **Coverage:** Spec 100% (§14.1, §14.3, §5.20), Phase Planning 100% (Phase-21 Tasks 21.3.3, 21.3.6).
- **Traceability:** §24 Criteria #105, #108; Phase 21 AC #6, #7, #13, #14.
- **Migrations:** `033_create_sar_reports`, `060_compliance_assessments`.
- **Error Codes & Hierarchy:** `CTR_THRESHOLD_TRIGGERED` (Internal Event), `SAR_DUAL_CONTROL_REQUIRED` (L2, HTTP 403).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Automated Currency Transaction Report (CTR) generation for cash-equivalent transactions $\ge \$10,000$; Suspicious Activity Report (SAR) filing workflow with dual-control maker-checker sign-off; XML output schema conforming to FinCEN BSA E-Filing specifications.
- **What Has Not Been Done:** Go AML reporter in `services/internal/compliance/sar.go`, FinCEN XML schema validator, unapplied migrations 033/060.

#### 8.6 MiFID II Compliance Suite (RTS 6, RTS 22, RTS 25, RTS 27/28)
- **Coverage:** Spec 100% (§14.1, §14.1b, §14.5, §16, §5.28, §5.32), Phase Planning 100% (Phase-21 Tasks 21.3.4, 21.3.12, 21.3.16, 21.3.19; Phase-09 Task 9.3.12; Phase-20 Task 20.3.9).
- **Traceability:** §24 Criteria #30, #106, #150, #178, #202, #246, #345; Phase 21 AC #8, #9, #25, #26, #33, #34, #39, #40.
- **Migrations:** `054_regulatory_reporting`, `059_regulatory_submissions`.
- **Error Codes & Hierarchy:** `MIFID_REPORTING_FAILED` (L1/L2), `RTS6_ALGO_UNAUTHORIZED` (L2, HTTP 403), `CLOCK_SKEW_EXCEEDED` (L0/L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Comprehensive MiFID II package: RTS 6 (algo certification, DEA controls, kill switches, 5-year order retention); RTS 22 (transaction reporting to ARM with LEI, buyer/seller ID, timestamps); RTS 25 (PTP IEEE 1588 microsecond synchronization with max 100µs drift); RTS 27/28 (quarterly execution venue quality and top 5 execution venue TCA reporting).
- **What Has Not Been Done:** ARM submission adapters (UnaVista / Trax), PTP timekeeping hardware daemon, Go report builders, unapplied migrations 054/059.

#### 8.7 EMIR REFIT & CFTC Parts 43/45 Lifecycle Reporting
- **Coverage:** Spec 100% (§14.1a, §5.32), Phase Planning 100% (Phase-21 Tasks 21.3.5, 21.3.14).
- **Traceability:** §24 Criteria #31, #169, #170, #345; Phase 21 AC #10–12, #29, #30.
- **Migrations:** `054_regulatory_reporting`, `059_regulatory_submissions`.
- **Error Codes & Hierarchy:** `INVALID_UTI_FORMAT` (L2, HTTP 400), `INVALID_UPI_CODE` (L2, HTTP 400), `TR_SUBMISSION_REJECTED` (L1/L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Full trade repository reporting pipeline for FX derivatives (forwards, swaps, NDFs, options); ISO 20022 XML generation; Unique Trade Identifier (UTI) and Unique Product Identifier (ANNA DSB UPI) generation; lifecycle event reporting (New, Modify, Cancel, Terminate, Valuation) to DTCC / Regis-TR.
- **What Has Not Been Done:** Go TR reporter daemon, DTCC / Regis-TR API client, unapplied migrations 054/059.

#### 8.8 Dodd-Frank Swap Data Reporting
- **Coverage:** Spec 100% (§14.1a), Phase Planning 100% (Phase-21 Tasks 21.3.9, 21.3.14).
- **Traceability:** §24 Criteria #170, #345; Phase 21 AC #19, #20.
- **Migrations:** `054_regulatory_reporting`, `059_regulatory_submissions`.
- **Error Codes & Hierarchy:** `DODD_FRANK_REPORT_FAILED` (L1/L2), `SDR_UNAVAILABLE` (L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Real-time Part 43 public dissemination reporting (as soon as technologically practicable) and Part 45 regulatory lifecycle reporting to CFTC-registered Swap Data Repositories (SDR).
- **What Has Not Been Done:** Go SDR reporter service, SDR message schema formatting, unapplied migrations.

#### 8.9 FinCEN MSB Registration & AML Program
- **Coverage:** Spec 100% (§14.1), Phase Planning 100% (Phase-21 Task 21.3.6).
- **Traceability:** §24 Criteria #105, #108; Phase 21 AC #13, #14.
- **Migrations:** `033_create_sar_reports`, `060_compliance_assessments`.
- **Error Codes & Hierarchy:** `MSB_COMPLIANCE_BREACH` (L1/L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Complete Money Services Business (MSB) compliance documentation: independent annual audit checklist, designated AML compliance officer controls, employee training tracking, customer identification program (CIP).
- **What Has Not Been Done:** Compliance admin portal in React, automated AML monitoring cron, unapplied migrations 033/060.

#### 8.10 GDPR Compliance & Data Privacy Controls
- **Coverage:** Spec 100% (§14.1, §14.7, §5.2), Phase Planning 100% (Phase-21 Task 21.3.7).
- **Traceability:** §24 Criteria #103, #104, #342, #383; Phase 21 AC #15, #16.
- **Migrations:** `002_create_users`, `009_create_audit_hash_chain`, `060_compliance_assessments`.
- **Error Codes & Hierarchy:** `GEO_BLOCKED` (L2, HTTP 403), `GDPR_ERASURE_RESTRICTED` (L2, HTTP 409), `CONSENT_WITHDRAWN` (L2, HTTP 403).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** User consent management, data export API in JSON, right to erasure ("forgotten") with explicit financial regulatory carve-out: 7-year retention override under MiFID II and AML regulations prevents deletion of ledger lines, trades, and audit trails; PII pseudonymization in non-production environments.
- **What Has Not Been Done:** Go GDPR handlers in `services/internal/compliance/gdpr.go`, MaxMind GeoIP database integration, unapplied migrations.

#### 8.11 Market-Abuse Surveillance & Case Management
- **Coverage:** Spec 100% (§11, §14.1c, §14.4, §5.17, §5.28), Phase Planning 100% (Phase-17 Task 17.3.3; Phase-21 Tasks 21.3.8, 21.3.21, 21.3.27).
- **Traceability:** §24 Criteria #18, #207, #274, #392; Phase 17 AC #5–9; Phase 21 AC #17, #18, #41, #42, #55, #56.
- **Migrations:** `029_create_surveillance_signals`, `054_regulatory_reporting`.
- **Error Codes & Hierarchy:** `MARKET_ABUSE_DETECTED` (L2, HTTP 403), `WASH_TRADE_DETECTED` (L2, HTTP 403), `SURVEILLANCE_LAG_WARNING` (L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** 7 market-abuse signal algorithms defined on L3 order data (spoofing, layering, wash trading, front running, insider dealing, momentum ignition, marking the close); automated case generation in `surveillance_cases`; SLA routing (URGENT $\le 4$h, REVIEW $\le 24$h); case disposition workflow (FALSE_POSITIVE, ESCALATE_SAR, ESCALATE_STR, ESCALATE_ACTION).
- **What Has Not Been Done:** Go surveillance engine in `services/internal/surveillance/`, real-time L3 stream processing daemon, compliance case management UI, unapplied migrations 029/054.

#### 8.12 Communications Recording (MiFID II Taping on WORM Storage)
- **Coverage:** Spec 100% (§14.8, §5.34), Phase Planning 100% (Phase-21 Task 21.3.20).
- **Traceability:** §24 Criteria #203; Phase 21 AC #37, #38.
- **Migrations:** `062_comms_recordings`.
- **Error Codes & Hierarchy:** `TAPING_STORAGE_ERROR` (L1/L0), `WORM_IMMUTABILITY_BREACH` (L0).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Mandatory recording policy for all client electronic communications (support chat, trader messages, FIX session logs); S3 Object Lock in WORM (Write Once Read Many) compliance mode with 5-year retention; metadata indexing in `comms_recordings`.
- **What Has Not Been Done:** S3 WORM infrastructure provisioning, communication capture daemon, unapplied migration 062.

#### 8.13 Automated Tax Reporting (CRS & FATCA)
- **Coverage:** Spec 100% (§14.1, §5.32), Phase Planning 100% (Phase-21 Task 21.3.22; Phase-20 Task 20.3.10).
- **Traceability:** §24 Criteria #249; Phase 21 AC #45, #46; Phase 20 AC #20.
- **Migrations:** `054_regulatory_reporting`, `059_regulatory_submissions`.
- **Error Codes & Hierarchy:** `TAX_REPORT_GENERATION_FAILED` (L2, HTTP 500), `INVALID_TIN_FORMAT` (L3, HTTP 400).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Annual tax reporting engine generating IRS Form 8966 (FATCA XML) and OECD Common Reporting Standard (CRS XML v2.0); client tax residency data collection and TIN validation; FIFO/LIFO P&L reporting.
- **What Has Not Been Done:** Go tax report generator in `services/internal/compliance/tax.go`, XML schema validation, unapplied migrations 054/059.

#### 8.14 Basel III Capital Adequacy & Leverage Reporting
- **Coverage:** Spec 100% (§14.1), Phase Planning 100% (Phase-21 Task 21.3.13).
- **Traceability:** §24 Criteria #151; Phase 21 AC #27, #28.
- **Migrations:** `054_regulatory_reporting`, `082_treasury`.
- **Error Codes & Hierarchy:** `CAPITAL_ADEQUACY_BREACH` (L1/L0), `LEVERAGE_RATIO_BREACH` (L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Pillar 1 risk-weighted asset (RWA) calculations for market risk (FRTB standard approach), counterparty credit risk (SA-CCR), and operational risk; leverage ratio computation ($ \text{Tier 1 Capital} / \text{Total Exposure} \ge 3\% $); quarterly COREP report generation.
- **What Has Not Been Done:** Go capital calculation jobs, COREP XML generator, unapplied migrations 054/082.

#### 8.15 FX Global Code 55-Principle Self-Assessment Engine
- **Coverage:** Spec 100% (§14.6), Phase Planning 100% (Phase-21 Task 21.3.17).
- **Traceability:** §24 Criteria #182; Phase 21 AC #35, #36.
- **Migrations:** `060_compliance_assessments`.
- **Error Codes & Hierarchy:** `FX_GLOBAL_CODE_NON_COMPLIANT` (Audit Warning).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** 55-principle evaluation matrix across 6 themes (Ethics, Governance, Execution, Information Sharing, Risk Management & Compliance, Confirmation & Settlement); firm-quote model alignment (Principle 17, no last-look); public Statement of Commitment generator.
- **What Has Not Been Done:** Annual compliance review UI in React, Statement of Commitment generator in Go, unapplied migration 060.

#### 8.16 Jurisdictional Data Residency Enforcement
- **Coverage:** Spec 100% (§14.7), Phase Planning 100% (Phase-21 Task 21.3.18).
- **Traceability:** §24 Criteria #184; Phase 21 AC #35, #36.
- **Migrations:** `060_compliance_assessments`.
- **Error Codes & Hierarchy:** `CROSS_BORDER_ROUTING_PROHIBITED` (L2, HTTP 403), `DATA_RESIDENCY_VIOLATION` (L2, HTTP 403).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Multi-jurisdiction storage partitioning isolating EU, UK, US, and APAC client data; database-level row-level security and localized storage routing ensuring PII and order logs remain within sovereign regulatory boundaries.
- **What Has Not Been Done:** Multi-region PostgreSQL cluster setup, geographic request routing proxy, unapplied migration 060.

---

## 4. Domain 9: Trade Lifecycle Ops

### 4.1 Domain Summary Metrics & Component Matrix

| # | Component | Spec Ref | Phase Tasks | §24 Criteria | Migrations | Error Codes | Code % | Done vs. Not Done Summary |
|---|---|---|---|---|---|---|:---:|---|
| **9.1** | **FX Settlement Cycles** (T+1 / T+2 / Same-Day) | §6.3, §17.1 | Phase-24: 24.3.1, 24.3.2, 24.3.3, 24.3.6; Phase-03: 3.3.3 | #19, #20, #21 | 019, 022, 044, 104 | `SETTLEMENT_FAILED` (L2), `FUTURE_SETTLEMENT_PENDING` (L2) | 0% | **Done:** T+1 spot, T+2 exotics, same-day USD/CAD & USD/MXN, instruction states.<br>**Not Done:** Go settlement scheduler, automated value-date settlement runner. |
| **9.2** | **Banking Rails Integration** (SWIFT/SEPA/FedNow/etc.) | §17.2 | Phase-11: 11.3.1, 11.3.2, 11.3.7, 11.3.9; Phase-24: 24.3.4, 24.3.12, 24.3.20, 24.3.21 | #22–25, #79, #80, #175, #413, #414 | 007, 008, 035, 040, 057, 078, 107, 108 | `BANKING_RAIL_UNAVAILABLE` (L1), `BANKING_RAIL_CUTOFF_PASSED` (L2) | 0% | **Done:** SWIFT MT103/202/900/910, SEPA SCT/Inst, FedNow, cut-off enforcer.<br>**Not Done:** Live bank rail gateways, SWIFT Alliance connection, unapplied migrations. |
| **9.3** | **CLS PvP Settlement** (ISO 20022) | §17.3a, §17.6 | Phase-24: 24.3.8, 24.3.16 | #126, #168, #326 | 019, 044, 053 | `CLS_MATCH_FAILED` (L2), `CLS_WINDOW_CLOSED` (L2) | 0% | **Done:** Third-party PvP service eliminating Herstatt risk, ISO 20022 XML pairs.<br>**Not Done:** CLS settlement member integration, Go CLS adapter daemon. |
| **9.4** | **Nostro / Vostro & Bank Reconciliation** | §17.3, §17.10 | Phase-24: 24.3.1, 24.3.2, 24.3.12, 24.3.19 | #20, #21, #78, #175, #347 | 018, 057, 082 | `NOSTRO_INSUFFICIENT_FUNDS` (L1/L2), `NOSTRO_BREAK` (L1) | 0% | **Done:** MT940/MT942/camt.053 ingestion, 3-way reconciliation, break aging.<br>**Not Done:** Go bank statement parser, automated break management engine. |
| **9.5** | **General Ledger Double-Entry Posting** | §5.21a, §17, §2.7.1 | Phase-03: 3.3.6, 3.3.19, 3.3.21; Phase-24: 24.3.17, 24.3.21 | #9, #121, #301, #339, #358, #414 | 036, 088, 096, 102, 108 | `LEDGER_UNBALANCED` (L0), `ZERO_BALANCE_VIOLATION` (L2) | 0% | **Done:** Strict zero-bypass double-entry ($\sum \text{Debit} = \sum \text{Credit}$), minor-unit integers.<br>**Not Done:** Go ledger posting service, database journal triggers, unapplied migrations. |
| **9.6** | **Tom-Next Rollover & Swap Engine** | §6.3, §17.4, §17.5, §17.15 | Phase-03: 3.3.7, 3.3.8, 3.3.11, 3.3.12, 3.3.23; Phase-23: 23.3.9 | #122, #123, #134, #221, #406, #407 | 001, 088, 105 | `ROLLOVER_CALCULATION_ERROR` (L1), `SWAP_RATE_STALE` (L1) | 0% | **Done:** 17:00 ET rollover, swap points math, triple-swap Wed, ISDA calendar.<br>**Not Done:** Go rollover daemon, interest rate feed ingestion, Islamic fee worker. |
| **9.7** | **Trade Confirmations & Statements (MT515)** | §16, §17 | Phase-20: 20.3.6, 20.3.8; Phase-24: 24.3.3 | #235, #237, #374 | 049 | `CONFIRMATION_DELIVERY_FAILED` (L2) | 0% | **Done:** MiFID II Art 25 trade confirms, SWIFT MT515 generation, PDF/CSV statements.<br>**Not Done:** Go document generation pipeline, SMTP/email dispatch worker. |
| **9.8** | **Trade Cost Analysis (TCA RTS 28)** | §14.5, §16 | Phase-20: 20.3.9; Phase-21: 21.3.19 | #202, #246 | ClickHouse MergeTree | `TCA_DATA_INSUFFICIENT` (L2) | 0% | **Done:** Effective/realized spread, arrival price slippage, market impact math.<br>**Not Done:** ClickHouse TCA analytics tables, Go TCA reporting engine. |
| **9.9** | **Post-Trade Allocation & Bunched Orders** | §17.8 | Phase-24: 24.3.10, 24.3.15; Phase-18: 18.3.13 | #172, #237 | 055 | `ALLOCATION_SUM_MISMATCH` (L2), `ALLOCATION_INVALID` (L2) | 0% | **Done:** Pre-declared allocation models, average price calculation, FIX 35=J/AK.<br>**Not Done:** Go allocation service, FIX allocation handlers, unapplied migration 055. |
| **9.10**| **CSDR Settlement Discipline** (Fails/Buy-Ins) | §17.14 | Phase-24: 24.3.13, 24.3.14, 24.3.19 | #347, #413 | 084 | `CSDR_PENALTY_INCURRED` (L2), `BUY_IN_TRIGGERED` (L1/L2) | 0% | **Done:** ISD+1 fail flag, Article 7 daily penalties, ISD+4 notice, ISD+7 buy-in.<br>**Not Done:** Go penalty computation engine, buy-in workflow, unapplied migration 084. |
| **9.11**| **Internal Position Transfers** | §13.9 | Phase-19: 19.3.12 | #190 | 061 | `POSITION_TRANSFER_FAILED` (L2), `TRANSFER_INSUFFICIENT` (L2) | 0% | **Done:** Off-book same-entity transfers at mark price, atomic margin rebalancing.<br>**Not Done:** Go position transfer service, collateral lock transfer routines. |
| **9.12**| **Trade Busts & Price Adjustment** | §7.4 | Phase-15: 15.3.5, 15.3.6, 15.3.10 | #138 | 051 | `TRADE_BUST_REJECTED` (L2), `BUST_WINDOW_EXPIRED` (L2) | 0% | **Done:** Obvious-error policy, 15m window, $>2\times$ band, dual-control approval, GL reversal.<br>**Not Done:** Admin trade bust handler in Go, reopening call auction daemon. |

### 4.2 Detailed Component Breakdown (Domain 9)

#### 9.1 FX Settlement Cycles (T+1, T+2, Same-Day)
- **Coverage:** Spec 100% (§6.3, §17.1, §5.12, §5.14), Phase Planning 100% (Phase-24 Tasks 24.3.1, 24.3.2, 24.3.3, 24.3.6; Phase-03 Task 3.3.3).
- **Traceability:** §24 Criteria #19, #20, #21; Phase 3 AC #5–7; Phase 24 AC #1–6, #11, #12.
- **Migrations:** `019_create_settlement_instructions`, `022_create_processed_trades`, `044_settlement_netting`, `104_accounts_settlement_intent`.
- **Error Codes & Hierarchy:** `SETTLEMENT_FAILED` (L2, HTTP 409), `FUTURE_SETTLEMENT_PENDING` (L2, HTTP 409), `SETTLEMENT_CUTOFF_EXCEEDED` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Comprehensive settlement schedule: standard T+1 for major spot FX (EUR/USD, GBP/USD, USD/JPY); T+2 for exotics; same-day settlement for USD/CAD and USD/MXN; value-date generation based on currency holiday calendars; settlement instruction state machine (`PENDING`, `DISPATCHED`, `SETTLED`, `FAILED`, `DISPUTED`).
- **What Has Not Been Done:** Go settlement scheduler in `services/internal/settlement/`, automated value-date settlement batch runner, unapplied migrations 019, 022, 044, 104.

#### 9.2 Banking Rails Integration (SWIFT, SEPA, FedNow, ACH, CHAPS, TARGET2)
- **Coverage:** Spec 100% (§17.2, §5.8, §5.9, §5.21, §5.35, §5.47, §5.48), Phase Planning 100% (Phase-11 Tasks 11.3.1, 11.3.2, 11.3.7, 11.3.9; Phase-24 Tasks 24.3.4, 24.3.12, 24.3.20, 24.3.21).
- **Traceability:** §24 Criteria #22–25, #79, #80, #175, #413, #414; Phase 11 AC #1–4, #13–16; Phase 24 AC #7, #8, #23, #24, #39–42.
- **Migrations:** `007_create_funding_transactions`, `008_create_withdrawal_confirmations`, `035_create_swift_messages`, `040_bank_accounts`, `057_bank_statements`, `078_withdrawal_whitelist_settings`, `107_banking_rail_schedules`, `108_suspense_accounts_routing`.
- **Error Codes & Hierarchy:** `BANKING_RAIL_UNAVAILABLE` (L1, HTTP 503), `SWIFT_NACK_RECEIVED` (L2), `SEPA_RETURN_CODE` (L2), `BANKING_RAIL_CUTOFF_PASSED` (L2, HTTP 409).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Message schemas and parsers fully designed for SWIFT (MT103 customer payment, MT202 bank transfer, MT900 debit confirmation, MT910 credit confirmation), SEPA (SCT and SCT Inst), FedNow instant payments, ACH, CHAPS, TARGET2; automated banking rail cut-off time enforcer and value-date roll; unmatched deposit suspense account routing (`suspense_accounts_routing`).
- **What Has Not Been Done:** Live banking rail gateways, SWIFT Alliance Lite2 connection, Go payment processing workers, unapplied migrations.

#### 9.3 CLS PvP Settlement (Continuous Linked Settlement)
- **Coverage:** Spec 100% (§17.3a, §17.6), Phase Planning 100% (Phase-24 Tasks 24.3.8, 24.3.16).
- **Traceability:** §24 Criteria #126, #168, #326; Phase 24 AC #15, #16, #31, #32.
- **Migrations:** `019_create_settlement_instructions`, `044_settlement_netting`, `053_parser`.
- **Error Codes & Hierarchy:** `CLS_MATCH_FAILED` (L2), `CLS_SUBMISSION_REJECTED` (L1/L2), `CLS_WINDOW_CLOSED` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Third-party CLS PvP settlement service eliminating Herstatt principal risk; dynamic eligibility checking; paired instruction submission using SWIFT ISO 20022 XML (`pacs.008`/`pacs.009`/`camt.054`); CLS operating window enforcement; discrepancy quarantine.
- **What Has Not Been Done:** CLS third-party settlement member integration, Go CLS adapter daemon in `services/internal/settlement/cls_pvp.go`, unapplied migrations.

#### 9.4 Nostro / Vostro Account Management & Bank Reconciliation
- **Coverage:** Spec 100% (§17.3, §17.10, §5.11, §5.36), Phase Planning 100% (Phase-24 Tasks 24.3.1, 24.3.2, 24.3.12, 24.3.19).
- **Traceability:** §24 Criteria #20, #21, #78, #175, #347; Phase 24 AC #1–4, #23, #24, #37, #38.
- **Migrations:** `018_create_nostro_accounts`, `057_bank_statements`, `082_treasury`.
- **Error Codes & Hierarchy:** `NOSTRO_INSUFFICIENT_FUNDS` (L1/L2, HTTP 409), `NOSTRO_BREAK_DETECTED` (L1), `VOSTRO_OVERDRAFT_PROHIBITED` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Multi-currency nostro account tracking across correspondent banks; automated bank statement ingestion (SWIFT MT940/MT942 and ISO 20022 camt.053); automated 3-way reconciliation (Internal GL vs Nostro Ledger vs Bank Statement); break aging with SLA and dual-control write-offs.
- **What Has Not Been Done:** Go nostro reconciliation daemon in `services/internal/backoffice/reconciliation.go`, MT940/camt.053 parsers, unapplied migrations 018/057.

#### 9.5 General Ledger Double-Entry Journal Posting
- **Coverage:** Spec 100% (§5.21a, §17, §2.7.1), Phase Planning 100% (Phase-03 Tasks 3.3.6, 3.3.19, 3.3.21; Phase-24 Tasks 24.3.17, 24.3.21).
- **Traceability:** §24 Criteria #9, #121, #301, #339, #358, #414; Phase 3 AC #11, #12, #37, #38, #41, #42, #47, #48; Phase 24 AC #41, #42.
- **Migrations:** `036_create_general_ledger`, `088_gl_chart_and_fees`, `096_cent_subunit_ledger`, `102_wallets_ledger_entries`, `108_suspense_accounts_routing`.
- **Error Codes & Hierarchy:** `LEDGER_UNBALANCED` (L0, Fatal Halt), `GL_ACCOUNT_NOT_FOUND` (L2), `ZERO_BALANCE_VIOLATION` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Strict fail-closed double-entry GL architecture: zero GL bypass for any balance mutation; every trade, funding transaction, fee, swap, or liquidation produces balanced debit and credit journal lines ($ \sum \text{Debit} = \sum \text{Credit} $); minor-unit cent integer arithmetic; immutable audit hash chain.
- **What Has Not Been Done:** Go ledger engine in `services/internal/ledger/posting.go`, database triggers enforcing balanced journal lines, unapplied migrations 036, 088, 096, 102, 108.

#### 9.6 Tom-Next Rollover & Swap Engine
- **Coverage:** Spec 100% (§6.3, §17.4, §17.5, §17.15, §5.45, §5.46), Phase Planning 100% (Phase-03 Tasks 3.3.7, 3.3.8, 3.3.11, 3.3.12, 3.3.23; Phase-23 Task 23.3.9).
- **Traceability:** §24 Criteria #122, #123, #134, #221, #406, #407; Phase 3 AC #13–16, #21–24, #51, #52.
- **Migrations:** `001_create_instruments`, `088_gl_chart_and_fees`, `105_swap_free_admin_fees`.
- **Error Codes & Hierarchy:** `ROLLOVER_CALCULATION_ERROR` (L1), `HOLIDAY_CALENDAR_UNAVAILABLE` (L1), `SWAP_RATE_STALE` (L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Automated EOD rollover at 17:00 ET (21:00 UTC / 22:00 UTC DST); daily swap charge/credit based on interest rate differentials; triple swap on Wednesday for weekend coverage; ISDA Modified Following Business Day holiday calendar; swap-free Islamic account administration fee engine.
- **What Has Not Been Done:** Go rollover scheduler in `services/internal/risk/rollover.go`, live interest rate differential feed ingestor, unapplied migrations 001/088/105.

#### 9.7 Trade Confirmations & Client Statements
- **Coverage:** Spec 100% (§16, §17, §5.26), Phase Planning 100% (Phase-20 Tasks 20.3.6, 20.3.8; Phase-24 Task 24.3.3).
- **Traceability:** §24 Criteria #235, #237, #374; Phase 20 AC #11, #12, #15, #16; Phase 24 AC #5, #6.
- **Migrations:** `049_client_statements`.
- **Error Codes & Hierarchy:** `CONFIRMATION_DELIVERY_FAILED` (L2), `STATEMENT_GENERATION_FAILED` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** MiFID II Article 25 compliant confirmation generation (immediate electronic trade confirmation); SWIFT MT515 confirmation message generator; PDF/CSV daily/monthly account statements; client reporting portal delivery pipeline.
- **What Has Not Been Done:** Go PDF/MT515 document generator, email delivery worker, unapplied migration 049.

#### 9.8 Trade Cost Analysis (TCA RTS 28)
- **Coverage:** Spec 100% (§14.5, §16), Phase Planning 100% (Phase-20 Task 20.3.9; Phase-21 Task 21.3.19).
- **Traceability:** §24 Criteria #202, #246; Phase 20 AC #17, #18; Phase 21 AC #39, #40.
- **Migrations:** ClickHouse `tca_benchmarks` MergeTree table.
- **Error Codes & Hierarchy:** `TCA_DATA_INSUFFICIENT` (L2), `BENCHMARK_UNAVAILABLE` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** MiFID II RTS 28 compliant TCA engine: computes effective spread, realized spread, arrival price slippage, market impact, and implementation shortfall; automated benchmark capture against arrival mid-quote.
- **What Has Not Been Done:** ClickHouse TCA analytics tables, Go TCA reporting engine in `services/internal/analytics/tca.go`.

#### 9.9 Post-Trade Allocation & Bunched Orders (FIX 35=J/AK)
- **Coverage:** Spec 100% (§17.8, §5.31), Phase Planning 100% (Phase-24 Tasks 24.3.10, 24.3.15; Phase-18 Task 18.3.13).
- **Traceability:** §24 Criteria #172, #237; Phase 24 AC #19, #20, #29, #30; Phase 18 AC #25, #26.
- **Migrations:** `055_trade_allocations`.
- **Error Codes & Hierarchy:** `ALLOCATION_SUM_MISMATCH` (L2, HTTP 400 / FIX Tag 35=AK AllocRejCode=2), `ALLOCATION_ACCOUNT_INVALID` (L2), `ALLOCATION_EXCEEDS_BLOCK` (L2).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Institutional block order allocation workflow; pre-declared allocation models; weighted average price calculation across multiple fills; support for FIX Allocation Instruction (35=J) and Allocation Report (35=AK); immutable before/after audit log.
- **What Has Not Been Done:** Go allocation engine in `services/internal/backoffice/allocations.go`, FIX allocation message handlers, unapplied migration 055.

#### 9.10 CSDR Settlement Discipline Regime (Fails, Penalties, Buy-Ins)
- **Coverage:** Spec 100% (§17.14, §5.38), Phase Planning 100% (Phase-24 Tasks 24.3.13, 24.3.14, 24.3.19).
- **Traceability:** §24 Criteria #347, #413; Phase 24 AC #25–28, #37, #38.
- **Migrations:** `084_settlement_penalties`.
- **Error Codes & Hierarchy:** `CSDR_PENALTY_INCURRED` (L2, HTTP 409), `BUY_IN_TRIGGERED` (L1/L2), `SETTLEMENT_FAIL_ESCALATED` (L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** CSDR Article 7 settlement fail tracking (ISD+1 fail flag); automated daily bilateral cash penalties (1bp/day liquid FX, 0.5bp/day illiquid); mandatory buy-in escalation at ISD+4 notification and ISD+7 market execution; PB credit restitution on settlement fail.
- **What Has Not Been Done:** Go penalty computation engine in `services/internal/backoffice/csdr_discipline.go`, buy-in execution workflow, unapplied migration 084.

#### 9.11 Internal Position Transfers & Sub-Account Collateral Rebalancing
- **Coverage:** Spec 100% (§13.9, §5.33), Phase Planning 100% (Phase-19 Task 19.3.12).
- **Traceability:** §24 Criteria #190; Phase 19 AC #27, #28.
- **Migrations:** `061_position_transfers`.
- **Error Codes & Hierarchy:** `POSITION_TRANSFER_FAILED` (L2, HTTP 409), `TRANSFER_INSUFFICIENT_MARGIN` (L2, HTTP 400), `TRANSFER_ACCOUNTS_INCOMPATIBLE` (L2, HTTP 403).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Off-book atomic internal position transfers between accounts of the same legal entity at official mark price with zero bid/ask spread and zero crossing fees; atomic collateral (`balances.locked`) release on source account and locking on destination account; GL journal entries.
- **What Has Not Been Done:** Go position transfer service in `services/internal/risk/position_transfer.go`, unapplied migration 061.

#### 9.12 Trade Busts & Price Adjustment (Obvious-Error Policy)
- **Coverage:** Spec 100% (§7.4, §5.29), Phase Planning 100% (Phase-15 Tasks 15.3.5, 15.3.6, 15.3.10).
- **Traceability:** §24 Criteria #138; Phase 15 AC #10–12, #20.
- **Migrations:** `051_trade_busts`.
- **Error Codes & Hierarchy:** `TRADE_BUST_REJECTED` (L2, HTTP 409), `BUST_WINDOW_EXPIRED` (L2, HTTP 409), `PRICE_ADJUST_OUT_OF_BOUNDS` (L2, HTTP 400), `AUCTION_CLEARING_FAILED` (L1).
- **Implementation Status:** 0% Code.
- **What Has Been Done:** Clearly erroneous trade / obvious-error policy; 15-minute eligibility window; deviation $> 2\times$ normal price band; dual-control Risk Manager authorization; full GL reversal (BUST) or price differential correction (PRICE_ADJUST); reopening call auction after halt or crossed book.
- **What Has Not Been Done:** Admin trade bust handler in Go (`services/internal/admin/trade_busts.go`), reopening call auction daemon, unapplied migration 051.

---

## 5. Architectural Invariants, Error Hierarchy (L0–L3) & Cross-Domain Dependencies

### 5.1 Error Hierarchy Mapping for Slice 3

All failure modes across Domains 7, 8, and 9 adhere strictly to the 4-tier taxonomy defined in Master Spec §2.7.2:

```
┌────────────────────────────────────────────────────────────────────────────┐
│ L0: CRITICAL / FATAL FAULT                                                │
│ Core halt (SIGTERM), P0 alert, dump diagnostics, crash-consistent state     │
│ • GL Zero-Sum Imbalance (Debit != Credit)                                  │
│ • Client-Money Segregation Shortfall without automated house top-up       │
│ • Cryptographic audit-hash tampering / WORM comms immutability breach      │
├────────────────────────────────────────────────────────────────────────────┤
│ L1: SYSTEMIC / INFRASTRUCTURE DEGRADATION                                  │
│ Degraded mode (ReadOnly / Throttled / Maintenance), circuit breaker trip  │
│ • Price Oracle staleness >5s (fail-closed liquidation freeze / fallback)   │
│ • Sanctions feed downtime >30s (quarantines outbound funding)              │
│ • Banking rail disconnection (SWIFT / SEPA gateway down)                   │
│ • Insurance Fund depletion (triggers Auto-Deleveraging ADL)                │
├────────────────────────────────────────────────────────────────────────────┤
│ L2: TRANSACTION / STATE BOUNDARY ERROR                                     │
│ Atomic rejection, rollback optimistic reservations, emit error envelope   │
│ • Margin shortfall (MARGIN_INSUFFICIENT)                                  │
│ • PB NOP / DSL limit breach (PB_CREDIT_EXCEEDED)                           │
│ • Sanctioned account detection (SANCTIONS_BLOCKED)                         │
│ • Trade bust window expired (>15 min)                                      │
├────────────────────────────────────────────────────────────────────────────┤
│ L3: EDGE / PROTOCOL VALIDATION REJECTION                                   │
│ Fast gateway drop, metrics counter increment, IP ban escalation           │
│ • Invalid KYC document payload / unsupported format                        │
│ • Malformed FIX allocation instruction (Tag 35=J)                          │
│ • Invalid LEI / UTI syntax                                                 │
└────────────────────────────────────────────────────────────────────────────┘
```

### 5.2 Cross-Domain Invariant Rules

1. **Balance Non-Negativity & Retail NBP Invariant:**
   - Account balances for retail accounts (`accounts.client_category = 'RETAIL'`) must satisfy $\text{equity} \ge 0$.
   - If market slippage causes equity $< 0$ during liquidation, the deficit is absorbed by the Insurance Fund via an atomic GL journal entry.
2. **Zero-Bypass Double-Entry Invariant:**
   - No balance may ever be mutated without balanced debit and credit journal entries ($\sum \text{Debit} = \sum \text{Credit}$).
   - Direct database updates to `balances` table without corresponding `ledger_lines` are strictly prohibited by database triggers and serializable transaction wrappers.
3. **Strict Fail-Closed Sanctions Invariant:**
   - If the pre-trade sanctions cache or outbound screening provider cannot verify a party's status due to network partition or staleness $>60$s, all order placements and funding transfers must fail closed.
4. **Deterministic Value-Date Rolling Invariant:**
   - Spot trades executed before the daily 17:00 ET cut-off roll to $T+1$ or $T+2$ value dates adjusted by the multi-currency ISDA holiday calendar; trades executed after 17:00 ET roll an additional business day forward.

---

## 6. Synthesis & Execution Readiness

### 6.1 Findings & Observations
- **Design Completeness:** **100%**. Every formula (90-day correlation margin offsets, CSDR cash penalty calculation, RTS 9 OTR ratios, swap points, VaR insurance sizing), database schema (migrations 001–108), protocol message (SWIFT MT103/202/515/940, ISO 20022 XML, FIX 35=J/AK), and regulatory lifecycle (EMIR REFIT, CFTC, MiFID II, FinCEN) is exhaustively specified.
- **Implementation Reality:** **0%**. Zero application code has been committed to the repository. No Go microservices, C++ hooks, database schemas, or frontend components physically exist.
- **Traceability Integrity:** All 38 components in Slice 3 map cleanly to the 414 canonical §24 Acceptance Criteria and the 30 Phase plans without dangling references or unassigned migration numbers.

### 6.2 Actionable Day 0 Implementation Path for Slice 3
1. **Prerequisite Foundation (Phase 01–02):**
   - Execute C++ Core Foundation and PostgreSQL migrations `001_` through `021_`.
2. **Phase 03 (Risk & Settlement Foundation):**
   - Implement Go double-entry General Ledger service (`migrations/036_create_general_ledger.up.sql`), minor-unit integer arithmetic, and Tom-Next swap rate engine.
3. **Phase 11 & 14 (Funding, KYC & Banking Rails):**
   - Implement banking rail handlers (`migrations/040_bank_accounts.up.sql`) and 4-tier KYC lifecycle engine (`migrations/017_create_kyc_documents.up.sql`, `042_client_categorization.up.sql`).
4. **Phase 19 & 19.5 (Multi-Asset Margin & Liquidations):**
   - Deploy multi-asset margin engine (`migrations/013_`, `058_`, `106_`), 2s liquidation scanner, 4-phase auction, and retail NBP write-off logic.
5. **Phase 21 (Compliance & AML):**
   - Wire C++ `SanctionsHook` into `PreTradeChecker`, deploy MiFID II / EMIR / CFTC reporting engines (`migrations/054_`, `059_`), and deploy L3 market-abuse surveillance case management.
6. **Phase 24 (Backoffice & Settlement):**
   - Implement CLS PvP adapter, nostro reconciliation engine (`migrations/018_`, `057_`), CSDR discipline engine (`migrations/084_`), and bunched order block trade allocation (`migrations/055_`).

---
*Report compiled autonomously by Antigravity AI Engine — Completeness Assessment Slice 3 (Domains 7, 8, 9).*
