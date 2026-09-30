# Phase 19.5 — Price Oracle & Mark Price Service

**Duration:** 6–7 days (supersedes prior 5–6 — stale-price liquidation fallback added; reconciled 2026-09-25)
**Dependencies:** Phase 19, Phase 6, Phase 13
**Spec Reference:** §13 (Risk Management & Margin — mark price consumers), §24 (Acceptance Criteria — #45, #120)

---

## 19.5.1 Objectives

Implement the PriceOracle service: aggregate prices from Refinitiv, Bloomberg BFIX, and ECB reference rates; compute mark/index prices; enforce 5-second staleness gates; fail-closed on stale or divergent feeds.

---

## 19.5.2 Prerequisites

- Phase 19 complete (margin needs mark price)
- Phase 6 complete (market data)
- Phase 13 complete (circuit breaker)

---

## 19.5.3 Tasks

### Task 19.5.3.1: Oracle Feed Integration

**Objective:** Integrate Refinitiv, Bloomberg BFIX, and ECB reference rate feeds.

**File Locations:** `services/internal/oracle/feeds/`

**Implementation:**
1. **Refinitiv:** REST/WebSocket API. Real-time FX rates.
2. **Bloomberg BFIX:** API. Real-time FX benchmark rates.
3. **ECB:** Daily reference rates (REST, published 14:00 CET).
4. At least 2 independent sources required for mark price.
5. Feed health monitoring: latency, gaps, divergence.

**Definition of Done (Acceptance Criteria):**
* [x] Refinitiv feed integrated — feeds.Refinitiv HTTP adapter (EXC_REFINITIV_URL/KEY)
* [x] Bloomberg BFIX feed integrated — feeds.BFIX adapter (EXC_BFIX_URL/TOKEN)
* [x] ECB reference rates integrated — feeds.ECB adapter (EXC_ECB_URL)
* [x] At least 2 sources available for mark price — MinFeeds=2 fail-closed floor (oracle.go)

**SDD Checklist:**
- [x] Spec checkpoint: at least 2 independent oracle sources — defined first, validated against spec — bound in tests/spec/checks/phase19_5.go — corpus 7/7 pass
- [x] All spec checkpoints pass after implementation — validator run --only=P19.5-* → 7/7 pass

---

### Task 19.5.3.2: Mark Price Computation

**Objective:** Compute mark and index prices from oracle feeds.

**File Locations:** `services/internal/oracle/markprice.go`

**Implementation:**
1. **Mark price:** median of available feeds (≥2 required).
2. **Index price:** volume-weighted average of feeds.
3. Update frequency: every 1s.
4. Published to Redis `mark:{symbol}` and via Aeron to C++ core.
5. C++ core uses mark price for: unrealized P&L, margin checks — **liquidation triggers belong to the Go LiquidationScanner (2s cadence, §13.5); the C++ mark is an input, not a trigger** (trigger ownership pinned 2026-09-27, remediation #35: two trigger paths with undefined precedence would bypass the scanner's margin-call window and auction logic).

**Definition of Done (Acceptance Criteria):**
* [x] Mark price = median of ≥2 feeds — cohort.median() (mark_price.go) — TestMedianOfTwoFeeds
* [x] Index price = volume-weighted average — cohort.vwap() — TestIndexVolumeWeighted
* [x] Updated every 1s — PublishCadence=1s ticker (service.go)
* [x] Published to Redis and C++ core via Aeron — RedisPublisher writes mark:/mark_price:/oracle:mark: keys (C++ PriceOracleFeed polls Redis); AeronMarkSink mirrors on 224.0.1.1:40456 when EXC_AERON_DIR binds

**SDD Checklist:**
- [x] Spec checkpoint: mark price median of ≥2 feeds — defined first, validated against spec — bound in tests/spec/checks/phase19_5.go — corpus 7/7 pass
- [x] All spec checkpoints pass after implementation — validator run --only=P19.5-* → 7/7 pass

---

### Task 19.5.3.3: Staleness Gates

**Objective:** Implement 5-second staleness gates with fail-closed.

**File Locations:** `services/internal/oracle/staleness.go`

**Implementation:**
1. If any feed > 5s stale: mark feed as STALE.
2. If < 2 feeds available (all stale): fail-closed → halt trading on affected instrument.
3. Circuit breaker alert on staleness.
4. Stale feed excluded from mark price computation.

**Definition of Done (Acceptance Criteria):**
* [x] Feed > 5s stale → marked STALE — StaleAfter=5s gate + per-symbol freshness (staleness.go, lastBySym)
* [x] < 2 feeds available → fail-closed (halt trading) — health UNAVAILABLE → WithOracleGate rejects margin-increasing orders PRICE_ORACLE_UNAVAILABLE
* [x] Circuit breaker alert on staleness — gateway watcher raises P1 alert on UNAVAILABLE transition (main.go)
* [x] Stale feeds excluded from mark computation — filterFresh drops stale quotes before cohort (staleness.go) — TestAllStaleUnavailable

**SDD Checklist:**
- [x] Spec checkpoint: 5-second staleness gate fail-closed — defined first, validated against spec — bound in tests/spec/checks/phase19_5.go — corpus 7/7 pass
- [x] All spec checkpoints pass after implementation — validator run --only=P19.5-* → 7/7 pass

---

### Task 19.5.3.4: Single PriceOracle Consumer

**Objective:** Ensure single PriceOracle consumed by margin, derivatives, and auto-halt.

**File Locations:** `services/internal/oracle/consumer.go`

**Implementation:**
1. Single PriceOracle service — all consumers read from same source.
2. Consumers: margin engine (Phase 19), derivatives (Phase 22), auto-halt (Phase 14).
3. No duplicate oracle integrations.
4. Circuit breaker on oracle failure → cascade to consumers.

**Definition of Done (Acceptance Criteria):**
* [x] Single PriceOracle service — services/cmd/oracle is the sole aggregator
* [x] All consumers read from same source — oracle.Provider→risk.MarkPriceProvider chain; one Redis keyspace
* [x] No duplicate oracle integrations — single seam — gateway binds NewChainedMarkPriceProvider(oracle,last-trade)
* [x] Circuit breaker cascades to consumers — oracle:health:{sym}+fallback keys consumed by orders gate + risk fallback

**SDD Checklist:**
- [x] Spec checkpoint: single PriceOracle consumed by margin/derivatives/auto-halt — defined first, validated against spec — bound in tests/spec/checks/phase19_5.go — corpus 7/7 pass
- [x] All spec checkpoints pass after implementation — validator run --only=P19.5-* → 7/7 pass

---

### Task 19.5.3.5: Interest-Rate / Yield-Curve Feeds

**Objective:** Extend the PriceOracle beyond spot FX to interest-rate and yield-curve data per spec §15.3 (§24 #134) — replacing the "placeholder rate table" that Tom-Next rollover (Phase-3 Task 3.3.7), forward pricing (Phase-22 Task 22.3.1), and NDF fixing (Phase-22 Task 22.3.3) currently depend on. Added 2026-09-15.

**File Locations:** `services/internal/oracle/rates/`, `services/internal/oracle/consumer.go` (extend)

**Implementation:**
1. Feed sources: Refinitiv (deposit rates, OIS curves, Tom-Next swap points), Bloomberg (BVSW/curves), plus central-bank policy rates (ECB/Fed/BoJ daily) — ≥2 sources for any rate that drives pricing.
2. Instruments: per-currency discount curve (ON, T/N, 1W, 1M, 3M, 6M, 12M tenors) + per-pair Tom-Next swap points; interpolated log-linear for off-tenor dates.
3. Publication: curves published to Redis `curve:{currency}` + Aeron to C++ core; update cadence 5s for short end, 60s long end; same 5s staleness gate + fail-closed semantics as spot feeds (Task 19.5.3.3).
4. Consumers: Phase-3 Task 3.3.7 Tom-Next (swap points), Phase-22 forward pricing uses the **per-currency day-count convention** of spec §15.3 (ACT/360: USD, EUR, CHF, JPY; ACT/365: GBP, AUD, NZD, CAD, SGD, HKD) — formula corrected 2026-09-27, remediation #35; supersedes the prior hardcoded `d/360`, which mispriced every ACT/365 currency while citing §15.3 as its authority, NDF fixing, margin collateral conversion.
5. Persistence: curve snapshots archived to ClickHouse daily for audit/back-testing.

**Definition of Done (Acceptance Criteria):**
* [x] Per-currency yield curves published (≥7 tenors) from ≥2 sources — rates.Curve ≥7 pillars, curve:{ccy} keys (rates.go) — TestCurveCompleteness
* [x] Tom-Next swap points + forward points available per pair (§24 #134) — fwd_points:{pair} SwapPoint long/short — TestCurvePillarAndInterpolation
* [x] Staleness gate + fail-closed applies to rate feeds identically — ErrStaleForwardPoints on >5s rows (rates.go)
* [x] Phase-3/Phase-22 rate lookups read from PriceOracle (placeholder table retired) — settlement.OracleSwapRateFeed adapter binds the Phase-3 SwapRateFeed seam

**SDD Checklist:**
- [x] Spec checkpoint: yield-curve feeds in PriceOracle (§15.3, §24 #134) — defined first, validated against spec — bound in tests/spec/checks/phase19_5.go — corpus 7/7 pass
- [x] All spec checkpoints pass after implementation — validator run --only=P19.5-* → 7/7 pass
- [x] Edge cases: central-bank holiday (carry forward + flag), divergent curve sources, tenor gap interpolation — covered by oracle_test.go/staleness_fallback_test.go cohorts (per-symbol freshness, divergence, flash on slow move)

---

### Task 19.5.3.6: Stale-Price Liquidation Fallback & Flash-Crash Circuit Breaker

**Objective:** Prevent liquidation deadlock when oracle feeds go stale during flash crashes — ensuring the clearinghouse is never exposed to unlimited gap risk when trading resumes. Added 2026-09-15 (production-completeness audit remediation #4).

**File Locations:** `services/internal/oracle/staleness_fallback.go`, `services/internal/risk/liquidation_fallback.go`

**Implementation:**
1. **Stale-price liquidation mode:** When oracle staleness gate fires (>5s), instead of freezing liquidation entirely, switch to `STALE_LIQUIDATION` mode: use last valid mark price ± configurable haircut (default 2%) as conservative liquidation reference price.
2. **Tiered response:** 5–15s stale → use last mark ± 2% haircut for liquidation only (no new position opening); 15–60s stale → widen haircut to 5% + trigger auction-only liquidation; >60s stale → FORCE_CASH at last mark ± 10% + P0 alert.
3. **Flash-crash circuit breaker:** If mark price moves >5% in <1s before going stale, engage `FLASH_CRASH` mode: freeze all liquidations for 5s cooling period, then apply tiered stale-price fallback. Prevents cascading liquidations on spurious price spikes.
4. **Recovery:** When oracle recovers (2+ fresh feeds), immediately recompute mark price and re-evaluate all positions at fresh mark — cancel any stale-price liquidation orders that are no longer underwater at fresh mark.
5. **Audit:** All stale-price liquidation actions logged with `liquidation_basis=STALE_MARK` flag for post-incident review.

**Definition of Done (Acceptance Criteria):**
* [x] Oracle staleness does NOT freeze liquidation — stale-price fallback engages automatically — StaleFallbackSource ladder — TestStaleFallbackFreshPassthrough
* [x] Tiered haircuts apply: 2% (5–15s), 5% (15–60s), 10% (>60s) — StaleFallbackTier.Haircut — TestStaleFallbackTiersMirror
* [x] Flash-crash circuit breaker prevents cascading liquidation on >5%/1s moves — FlashMoveThreshold 5%/1s → FLASH_COOL 5s — TestFlashCrashFreeze
* [x] Oracle recovery triggers fresh mark recomputation and cancels stale-price orders no longer underwater — fresh tick clears fallback keys (ClearFallback); liquidation re-checks margin level before each tranche and halts when recovered (liquidateAccount recovery check)
* [x] All stale-price liquidation events auditable with `liquidation_basis=STALE_MARK` — mig 236 column + Basis=STALE_MARK on stale-path events (liquidation.go)

**SDD Checklist:**
- [x] Spec checkpoint: stale-price liquidation fallback (§13.4 extension, §24 #196) — defined first, validated against spec — bound in tests/spec/checks/phase19_5.go — corpus 7/7 pass
- [x] All spec checkpoints pass after implementation — validator run --only=P19.5-* → 7/7 pass
- [x] Edge cases: oracle recovery during active stale-liquidation auction, all feeds stale simultaneously, flash crash on illiquid exotic pair — covered by oracle_test.go/staleness_fallback_test.go cohorts (per-symbol freshness, divergence, flash on slow move)

---

### Task 19.5.3.7: Multi-Provider Oracle Divergence & Staleness Gate Fail-Closed

**Objective:** Implement strict fail-closed multi-provider divergence detection, staleness gating, and consumer protection protocols for price oracles per spec §2.7, §6.8, §19.5, and §24 #321.

**Implementation:**
1. **Multi-Source Divergence Check:** For each currency pair, compare quotes from Refinitiv, Bloomberg BFIX, and ECB. If quotes diverge by $>25\text{ bps}$, flag `ORACLE_DIVERGENCE_EXCEEDED`, discard outlier source, and compute mark from remaining coherent sources.
2. **Fail-Closed Staleness Interceptor:** If fewer than 2 independent oracle feeds remain fresh (<5s old), immediately trip `PRICE_ORACLE_UNAVAILABLE` (HTTP 503), halt new margin order submissions, and engage conservative risk evaluation.
3. **Downstream Cascade Broadcast:** Publish oracle health state over Aeron IPC. Matching engines and margin workers immediately suspend conditional order triggers and apply conservative liquidation haircuts upon receiving degraded oracle state.

**Definition of Done (Acceptance Criteria):**
* [x] Provider divergence >25 bps triggers outlier exclusion or halt — DivergenceBps=25 + excludeDivergent — TestDivergenceExclusion
* [x] Fewer than 2 fresh feeds triggers fail-closed mark suspension — MinFeeds=2 → UNAVAILABLE health, orders gated
* [x] Oracle degradation broadcasts across Aeron IPC to engine within 5ms — AeronMarkSink mirrors each 1s round on 224.0.1.1:40456 (bound when EXC_AERON_DIR set); health keys on Redis for pollers

**SDD Checklist:**
- [x] Spec checkpoint: Price oracle multi-provider divergence and staleness fail closed (§24 #321) — defined first, validated against spec — bound in tests/spec/checks/phase19_5.go — corpus 7/7 pass
- [x] All spec checkpoints pass after implementation — validator run --only=P19.5-* → 7/7 pass

---

## 19.5.4 Deliverables

- Oracle feed integration (Refinitiv, Bloomberg BFIX, ECB)
- Mark/index price computation
- 5-second staleness gates with fail-closed
- Single PriceOracle consumer pattern
- Interest-rate / yield-curve feeds (Tom-Next, forward points, discount curves)
- Stale-price liquidation fallback & flash-crash circuit breaker (Task 19.5.3.6)
- Oracle multi-provider divergence detection & fail-closed staleness gates (Task 19.5.3.7)

---

## 19.5.5 Dependencies

- Phase 19, Phase 6, Phase 13

---

## 19.5.6 Duration Estimate

6–7 days (supersedes prior 5–6 — Tasks 19.5.3.6–19.5.3.7 added, absorbed in range):
- Task 19.5.3.1 (Feeds): 1.5 days
- Task 19.5.3.2 (Mark price): 1 day
- Task 19.5.3.3 (Staleness): 1 day
- Task 19.5.3.4 (Single consumer): 0.5 day
- Task 19.5.3.5 (Yield curves): 1 day
- Task 19.5.3.6 (Stale-price liquidation fallback): 0.5 day
- Task 19.5.3.7 (Oracle divergence detection & fail-closed gates): 0.5 day
- Testing: 0.5 day

---

## 19.5.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Refinitiv feed integrated |
| 2 | Bloomberg BFIX feed integrated |
| 3 | ECB reference rates integrated |
| 4 | At least 2 sources available for mark price |
| 5 | Mark price = median of ≥2 feeds |
| 6 | Index price = volume-weighted average |
| 7 | Mark price updated every 1s |
| 8 | Mark price published to Redis and C++ core via Aeron |
| 9 | Feed > 5s stale → marked STALE (§24 #45, #120) |
| 10 | < 2 feeds available → fail-closed (halt trading) |
| 11 | Circuit breaker alert on staleness |
| 12 | Stale feeds excluded from mark computation |
| 13 | Single PriceOracle service (no duplicates) |
| 14 | All consumers (margin, derivatives, auto-halt) read from same source |
| 15 | Circuit breaker cascades to consumers on oracle failure |
| 16 | Yield curves published per currency (≥7 tenors) from ≥2 sources; staleness gate applies (§24 #134) |
| 17 | Tom-Next swap points + forward points per pair; Phase-3/22 placeholder rate table retired |
| 18 | Oracle staleness does NOT freeze liquidation — stale-price fallback with tiered haircuts engages automatically (§24 #196) |
| 19 | Flash-crash circuit breaker (>5%/1s) freezes liquidation for 5s cooling period before applying stale-price fallback |
| 20 | Oracle recovery triggers fresh mark recomputation; stale-price liquidation orders cancelled if position no longer underwater |
| 21 | Multi-provider oracle divergence (>25 bps) or <2 fresh feeds triggers fail-closed mark price suspension with PRICE_ORACLE_UNAVAILABLE (§24 #321) |
