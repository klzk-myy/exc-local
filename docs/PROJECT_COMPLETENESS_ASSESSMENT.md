# FOREX Exchange System Suite — Comprehensive Component Completeness Assessment

**Assessment Date:** 2026-09-27  
**Project State:** Planning-Stage Repository (100% Architecture & Specification / 0% Application Code Implementation)  
**Primary Standards & References:**
- Master Specification: `docs/Specification - Complete Exchange System Suite.md` (v7.0, 27 Sections, 414 §24 Criteria)
- Implementation Plans: 30 Phase Plans (`Phase-01` through `Phase-24`, including 6 buffer phases)
- Architectural Directives: `ARCHITECTURE.md` · `DESIGN.md` · `WORKFLOWS.md` · `MEMORY.md` · `AGENTS.md` · `CONTEXT.md`

---

## 1. Executive Summary & Assessment Methodology

### 1.1 Project Reality & Baseline
The FOREX Exchange System Suite is an institutional-grade foreign exchange CLOB exchange designed for continuous 24/5 fiat trading (spot, forwards, swaps, NDFs, and options). 

A critical reality established across all foundational project documents:
- **Specification & Architectural Design Completeness:** **100%** (All functional requirements, mathematical formulas, state machines, protocol mappings, error codes, and migrations are specified across 30 phases).
- **Production Code Implementation Completeness:** **0%** (The repository contains zero application source code commits; no C++ matching engine, no Go services, no React frontend, and no deployed database schemas exist on disk yet).
- **Execution Readiness:** The project is at **Phase 01 Day 0**, with zero predecessor dependencies blocking implementation.

### 1.2 Metric Framework for Completeness Analysis
Each domain and subcomponent is analyzed across six quantitative and qualitative dimensions:

1. **Spec & Algorithm Definition Depth (Spec %):** Mathematical formulation, data structure selection, edge-case coverage, and protocol framing in the Master Spec (v7.0).
2. **Phase Task & Implementation Plan Coverage (Plan %):** Concrete task breakdown in `Phase-NN` docs, step-by-step implementation instructions, test cases, and delivery deliverables.
3. **Acceptance Criteria Traceability (AC Count & %):** Explicit mapping to the 414 canonical §24 Acceptance Criteria and 1,079 phase-level AC rows.
4. **Data Model & Migration Mapping (DB Migrations):** Assigned PostgreSQL migrations (out of 108 contiguous migrations `001_`–`108_`) and ClickHouse table definitions.
5. **Resilience & Fault Invariants (Error Codes & Tiers):** Coverage by the 149 registered §23 error codes and the 4-tier error severity hierarchy (L0–L3).
6. **Codebase Implementation (Code %):** Physical presence of compilable source code, unit/integration test suites, and operational configurations in the workspace.

---

## 2. High-Level Domain Summary Matrix

| # | Domain | Core Components | Spec % | Plan % | §24 ACs | Migrations | Code % | Implementation Readiness |
|---|---|---|---|---|---|---|---|---|
| **1** | **Matching & Execution Core** | LOB, deterministic matching, queue, priority, cross-shard, atomic amend, mass cancel, STP, slippage, auction, OTR | 100% | 100% | 46 | 001–003, 041, 049 | 0% | Ready for Phase 01/02 |
| **2** | **Instrument & Market Admin** | Lifecycle (7 states), maker-checker, 24/5 hours, holiday calendar, collars, min notional | 100% | 100% | 28 | 001, 005, 050 | 0% | Ready for Phase 03/15 |
| **3** | **Order Types** | Limit/Market/Stop/TP/Trailing, bracket/OCO, iceberg, GTD/GTC, pegged, TWAP/VWAP/VP, grid, conditional, GSLO | 100% | 100% | 36 | 002, 066, 071, 076 | 0% | Ready for Phase 02/16 |
| **4** | **Pricing & Liquidity Infra** | Mark price, index oracle, staleness gates, yield curves, MTF/fair value, LP scorecard, ADL, MM program | 100% | 100% | 29 | 024, 058, 073, 080 | 0% | Ready for Phase 18/19/19.5 |
| **5** | **APIs & Connectivity** | REST, WS (interactive), SBE multicast, FIX 4.4/5.0 + mTLS, mass quoting, batch orders, deprecation, dead-man, SOR | 100% | 100% | 48 | 004, 018, 067, 072 | 0% | Ready for Phase 05/06/18 |
| **6** | **Market Data Products** | L2/L3, klines (13 TF), tick history, book ticker, liquidation feed, subscriptions, SLA, block tape | 100% | 100% | 24 | ClickHouse MergeTree | 0% | Ready for Phase 06/17/20/23 |
| **7** | **Risk & Credit** | Margin (cross/isolated/portfolio), leverage tiers, liquidation ladder, NBP, insurance fund, PB credit (NOP/DSL), haircuts | 100% | 100% | 42 | 003, 052, 053, 074, 078 | 0% | Ready for Phase 03/19 |
| **8** | **Compliance & AML** | KYC tiers, PEP/sanctions, Travel Rule, SAR/CTR, MiFID II RTS 6/22/27/28, EMIR REFIT, CFTC, surveillance, taping, CRS | 100% | 100% | 55 | 006, 026, 038, 060–064, 081–086 | 0% | Ready for Phase 14/21 |
| **9** | **Trade Lifecycle Ops** | Settlement T+1/T+2, rails (SWIFT/SEPA), CLS PvP, nostro/vostro, double-entry GL, Tom-Next, TCA, allocation, CSDR | 100% | 100% | 44 | 008–012, 020, 070, 087–094 | 0% | Ready for Phase 03/11/24 |
| **10**| **Recovery & Resilience** | Custom WAL, recovery ladder, PG reconciliation, multi-region DR, drills, chaos, circuit breakers, DORA, WAF, KMS | 100% | 100% | 35 | 065, 095–098 | 0% | Ready for Phase 01/04/09/13 |
| **11**| **Engineering & Delivery** | CI/CD, validation harness (543 checkpoints), soak (72h), load test (75k/s), blue-green, capacity models, tiering | 100% | 100% | 22 | Infra / GitHub Actions / K8s | 0% | Ready for Phase 01.5/02.5/08.5 |
| **12**| **Client Experience** | Trader UI (Pro/Lite, charts, calculators), auth (TOTP/WebAuthn), anti-phishing, KYC portal, webhooks, PAMM/MAM | 100% | 100% | 32 | 007, 027, 068, 069, 099–104 | 0% | Ready for Phase 10/12/14 |
| **13**| **Governance & Business** | Treasury/own funds, client safeguarding (CASS 7), insurance replenishment, employee dealing, vendor risk, complaints | 100% | 100% | 21 | 032, 056, 105–108 | 0% | Ready for Phase 07/09/14/20/24 |

---


---

## 3. Slice 1 Completeness Assessment: Domains 1, 2, and 3

**Assessed Domains:**
- **Domain 1:** Matching & Execution Core (12 components)
- **Domain 2:** Instrument & Market Admin (6 components)
- **Domain 3:** Order Types (14 components)

---

### 3.1 Domain 1: Matching & Execution Core

#### 3.1.1 Domain Overview & Architectural Baseline
The Matching & Execution Core forms the central latency-critical computational engine of the exchange suite. It is designed in C++17/20 as a single-threaded event loop per currency pair shard, completely decoupled from network I/O via Aeron shared-memory ring buffers (`aeron:ipc`) and backed by a synchronous binary Write-Ahead Log (WAL) with `O_DIRECT` block-aligned flushing.

- **Spec Coverage:** **100%** (Formally defined in Spec v7.0 §2.1, §2.2, §2.7, §3.1–§3.7, §6.5, §6.6, §6.9, §6.11, §7.1, §13.4, §13.6a).
- **Planning Coverage:** **100%** (30 concrete tasks across Phase-01 Tasks 1.3.1/1.3.5–1.3.12, Phase-02 Tasks 2.3.1–2.3.26, Phase-02.5, Phase-05 Tasks 5.3.24/5.3.25/5.3.33/5.3.37, Phase-13 Task 13.3.6, Phase-15 Tasks 15.3.6/15.3.10/15.3.13, Phase-19 Task 19.3.4/19.3.11).
- **Acceptance Criteria Traceability:** **46 §24 Criteria** (#1, #2, #3, #4, #5, #6, #7, #8, #10, #11, #12, #14, #33, #34, #35, #36, #39, #40, #41, #118, #129, #130, #140, #142, #153, #154, #176, #188, #214, #220, #260, #274, #277, #279, #280, #282, #288, #299, #300, #316, #320, #336, #368, #394, #400, #402, #403, #404, #405).
- **Database Migrations Mapped:** 13 Migrations (`001_`, `005_`, `006_`, `020_`, `024_`, `038_`, `046_`, `047_`, `050_`, `058_`, `072_`, `094_`, `103_`).
- **Error Severity Mapping:** L0 (Fatal/Panic Core Halt), L1 (Degradation Mode / Circuit Breaker), L2 (Atomic Rejection & Rollback), L3 (Gateway Edge Fast-Rejection).
- **Code Implementation:** **0%** (Zero C++ or Go source code commits).

---

#### 3.1.2 Detailed Component Evaluation

##### 1. Limit Order Book (LOB) Data Structure
- **Spec Coverage:** 100% (§3.1, §3.6, §3.7).
- **Planning Coverage:** 100% (Phase-01 Task 1.3.1, 1.3.12; Phase-02 Task 2.3.1, 2.3.13; Phase-02.5 Tasks 2.5.3.1–2.5.3.4).
- **Acceptance Criteria:** §24 #1, #2, #5, #6, #11, #12; Phase-02 AC rows 1, 2, 8, 30, 52; Phase-02.5 AC rows 1, 2.
- **Database Migrations:** `001_create_instruments.up.sql`, `005_create_orders.up.sql`, `020_create_indexes.up.sql`.
- **Error Codes & Tiers:** `CAPACITY_EXCEEDED` (L1/503 / FIX Tag 35=8 OrdRejReason=99), `CRITICAL_BACKPRESSURE` (L1/503), `BOOK_CROSS_ERROR` (L0/503), `ORDER_NOT_FOUND` (L2/404).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Designed flat-array price level lookup with intrusive doubly-linked list (`Order* next`, `Order* prev`) eliminating runtime heap allocators.
  - Pre-allocated fixed-capacity memory arenas via `posix_memalign` (64-byte cache-line aligned).
  - Specified O(1) order cancellation/insertion and O(log N) price-level iteration.
  - Specified safe serialization of sparse books and empty price levels without memory dereference panics (Task 2.3.13).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ `OrderBook.hpp` / `OrderBook.cpp`.
  - Unwritten memory pool slab allocators.
  - Uncompiled LOB unit and benchmark test harness.

##### 2. Deterministic Matching Engine
- **Spec Coverage:** 100% (§2.1, §2.2, §3.2, §3.4, §3.7).
- **Planning Coverage:** 100% (Phase-01 Task 1.3.6, 1.3.10; Phase-02 Tasks 2.3.2, 2.3.4, 2.3.10, 2.3.23).
- **Acceptance Criteria:** §24 #1, #6, #7, #8, #394, #402; Phase-02 AC rows 2, 14, 17, 32, 53, 65; Phase-04 AC rows 17, 20.
- **Database Migrations:** `005_create_orders.up.sql`, `006_create_trades.up.sql`.
- **Error Codes & Tiers:** `WAL_CRC_CORRUPTED` (L0/503), `LEDGER_IMBALANCE` (L0/503), `NON_DETERMINISTIC_REPLAY` (L0/503).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Strict single-threaded matching loop pinned to dedicated CPU cores (`isolcpus` + `pthread_setaffinity_np`).
  - Total determinism invariant: matching loop forbidden from calling `clock_gettime` or system wall clock; time progression driven exclusively by discrete `TIME_TICK` IPC events appended to WAL (§24 #394).
  - Fixed-point integer math ($10^8$ pipette scaling) eliminating floating-point non-determinism across compilers/CPUs (Task 2.3.23, §24 #402).
  - Exact state reconstruction via snapshot + WAL replay ladder (§3.5).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ `MatchingEngine.hpp` / `MatchingEngine.cpp`.
  - Unimplemented deterministic event dispatcher.
  - Unwritten WAL replay verification harness.

##### 3. Order Ingress Queuing & IPC Ring Buffer
- **Spec Coverage:** 100% (§2.2, §2.7.3, §3.6, §3.7).
- **Planning Coverage:** 100% (Phase-01 Task 1.3.5, 1.3.10; Phase-02 Task 2.3.7, 2.3.19).
- **Acceptance Criteria:** §24 #11, #12, #14, #299; Phase-02 AC rows 19, 20, 30, 41, 61.
- **Database Migrations:** None (in-memory lock-free ring buffers).
- **Error Codes & Tiers:** `CAPACITY_EXCEEDED` (L1/503), `CRITICAL_BACKPRESSURE` (L1/503), `SYSTEM_OVERLOAD` (L1/503).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Aeron shared-memory IPC architecture (`aeron:ipc`) connecting Go API gateways to C++ matching cores.
  - High-watermark load-shedding protocol: at 80% ring buffer utilization, gateway fast-rejects aggressive market and batch orders with `CAPACITY_EXCEEDED` (HTTP 503 / FIX 99); at 95%, matching engine signals `CRITICAL_BACKPRESSURE` and pauses ingress to flush WAL without frame drops.
  - Poison-pill defense: invalid IPC framing quarantined with ring buffer offset logging (Task 2.3.19).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Aeron C++ publisher/subscriber endpoints.
  - Unwritten Go Aeron wrapper bindings.
  - Kernel shm/hugepages automated deployment scripts.

##### 4. Price-Time Priority (FIFO) & Tie-Breaking
- **Spec Coverage:** 100% (§3.1, §3.2, §6.5, §6.9).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.1, 2.3.2, 2.3.20).
- **Acceptance Criteria:** §24 #1, #336; Phase-02 AC rows 1, 2, 62.
- **Database Migrations:** `005_create_orders.up.sql`.
- **Error Codes & Tiers:** `BOOK_CROSS_ERROR` (L0/503), `ORDER_NOT_FOUND` (L2/404).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Formal priority tuple specified: orders ranked first by price (descending for bids, ascending for asks), then by nanosecond queue entry timestamp.
  - Unambiguous deterministic tie-break tuple defined: `(price, timestamp_ns, ingress_seq)` to guarantee deterministic ordering even with identical hardware timestamps (§24 #336).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ order comparison functors.
  - Uncompiled FIFO queue unit tests.

##### 5. Cross-Shard Coordination & 2PC Basket Matching
- **Spec Coverage:** 100% (§2.1, §2.7.2, §3.2, §13.1).
- **Planning Coverage:** 100% (Phase-01 Task 1.3.7; Phase-02 Tasks 2.3.8, 2.3.12, 2.3.14, 2.3.25; Phase-19 Task 19.3.11).
- **Acceptance Criteria:** §24 #41, #118, #176, #214, #320, #404; Phase-02 AC rows 25, 39, 40, 53, 54, 67; Phase-19 AC rows 42, 58.
- **Database Migrations:** `058_shard_margin_reservations.up.sql`.
- **Error Codes & Tiers:** `CROSS_SHARD_TIMEOUT` (L2/409), `CROSS_SHARD_RESERVATION_FAILED` (L2/409), `CROSS_SHARD_LIMIT_EXCEEDED` (L2/429), `INSUFFICIENT_MARGIN` (L2/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Two-Phase Commit (2PC) protocol for multi-leg cross-currency orders across independent engine shards (`RESERVE` $ightarrow$ `COMMIT` / `COMPENSATE`).
  - Strict 500µs RPC margin reservation budget with immediate pessimistic floor fallbacks.
  - Hard 5-second reservation expiration with automated compensating unwinds and compensation reaper (§24 #214, #404).
  - Maximum 10 concurrent active 2PC reservations per account to prevent resource exhaustion attacks.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ Cross-Shard Coordinator service.
  - Unwritten compensating transaction queue.
  - Unapplied migration `058_shard_margin_reservations.up.sql`.

##### 6. Atomic Order Amendment & Keep-Priority Logic
- **Spec Coverage:** 100% (§6.9, §8.4).
- **Planning Coverage:** 100% (Phase-02 Task 2.3.20; Phase-05 Tasks 5.3.22, 5.3.37; Phase-18 Task 18.3.12).
- **Acceptance Criteria:** §24 #282, #288, #336; Phase-02 AC row 62; Phase-05 AC rows 45, 62; Phase-18 AC row 8.
- **Database Migrations:** `024_order_audit.up.sql`, `038_orders_execution_params.up.sql`.
- **Error Codes & Tiers:** `STALE_MODIFY` (L2/409), `ORDER_AMEND_REJECTED` (L2/409), `CANCEL_REPLACE_PARTIAL_FAILURE` (L2/409), `ORDER_NOT_FOUND` (L2/404).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Asymmetric priority rules specified: Quantity-down amendment preserves FIFO queue priority and original timestamp; Price-change or quantity-up amendment strips priority and appends order to tail of price level.
  - Concurrency safety: optimistic concurrency check via `order_seq`; concurrent match or cancellation during amend yields deterministic `STALE_MODIFY` (HTTP 409).
  - Amendments during auction call phases explicitly rejected with `ORDER_AMEND_REJECTED`.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ in-place order modification routines.
  - Unwritten Go cancel-replace orchestration handlers.
  - Unapplied migration `024_order_audit.up.sql`.

##### 7. Scoped Mass Cancellation & Dead-Man Switch
- **Spec Coverage:** 100% (§6.5, §8.4, §9.3).
- **Planning Coverage:** 100% (Phase-05 Tasks 5.3.24, 5.3.25, 5.3.33, 5.3.36; Phase-06 Task 6.3.7; Phase-18 Tasks 18.3.9, 18.3.16).
- **Acceptance Criteria:** §24 #153, #260, #272, #365; Phase-05 AC rows 48, 61; Phase-06 AC row 23; Phase-18 AC row 15.
- **Database Migrations:** `005_create_orders.up.sql`, `024_order_audit.up.sql`, `046_fix_sessions_entitlement.up.sql`.
- **Error Codes & Tiers:** `CANCEL_REJECTED` (L2/409), `ORDER_NOT_FOUND` (L2/404), `INSTRUMENT_SUSPENDED` (L2/409).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Multi-dimensional scoped mass cancel across: `account_id`, `instrument_id`, `side` (BUY/SELL), `order_type`, and `order_list_id`.
  - Dead-man switch / countdown cancel-all (`POST /api/v1/orders/cancel-all-after`): client specifies timeout (1,000–60,000ms); failure to heartbeat triggers automatic batch cancellation.
  - FIX Protocol Mass Cancel (Tag 35=q) and Cancel-on-Disconnect (CoD) with heartbeat loss detection within 3,000ms.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go mass-cancellation engine.
  - Unwritten dead-man countdown timer daemon in Redis/Go.
  - Unapplied migration `046_fix_sessions_entitlement.up.sql`.

##### 8. Self-Trade Prevention (STP) Engine
- **Spec Coverage:** 100% (§6.5).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.11, 2.3.16, 2.3.18, 2.3.21).
- **Acceptance Criteria:** §24 #4, #154, #274, #279, #280, #368; Phase-02 AC rows 7, 54, 58, 60, 63.
- **Database Migrations:** `038_orders_execution_params.up.sql`, `072_execution_rules_stp_groups.up.sql`, `094_accounts_default_stp.up.sql`.
- **Error Codes & Tiers:** `SELF_TRADE_PREVENTED` (L2/400), `WASH_TRADE_DETECTED` (L3/403), `PRODUCT_NOT_PERMITTED` (L3/403).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Full suite of 6 STP modes specified: `CANCEL_NEW` (CN), `CANCEL_RESTING` (CR), `CANCEL_BOTH` (CB), `DECREMENT_AND_CANCEL` (DC), `TRANSFER`, and `NONE`.
  - Institutional trade groups: STP evaluated across sub-accounts matching on `trade_group_id`.
  - `TRANSFER` mode enables mutual internal inventory reallocation without CLOB prints or spread leakage (§24 #279).
  - Mode `NONE` restricted exclusively to certified Professional / ECP accounts; all trades under `NONE` stream directly to compliance surveillance (§24 #274).
  - Immutable audit trail: `prevented_matches` records prevented quantities, prices, and timestamps.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ STP evaluation branch in matching loop.
  - Unapplied migrations `072_` and `094_`.
  - Unwritten prevented match database sink service.

##### 9. Execution Rules & Trade-Through Protection
- **Spec Coverage:** 100% (§6.5, §6.6, §6.6b, §6.11).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.17, 2.3.22, 2.3.26).
- **Acceptance Criteria:** §24 #129, #130, #277, #400, #405; Phase-02 AC rows 47, 48, 59, 64, 68.
- **Database Migrations:** `038_orders_execution_params.up.sql`, `072_execution_rules_stp_groups.up.sql`, `103_orders_discretionary_offset.up.sql`.
- **Error Codes & Tiers:** `POST_ONLY_VIOLATION` (L3/400), `REDUCE_ONLY_VIOLATION` (L3/400), `EXECUTION_RULE_PRICE_RANGE_EXCEEDED` (L2/409), `TRADE_THROUGH_REJECTED` (L2/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - `POST_ONLY` enforces maker liquidity provision; rejected immediately if marketable.
  - `REDUCE_ONLY` strictly prevents position expansion or flip.
  - Trade-Through Protection (§6.6b): Prevents aggressive takers from trading through protected quotes; records price improvement delta in trade records for MiFID II TCA.
  - Reference-Price Execution Collars: snapshotted at taker match initiation; unfilled remainder expires with `EXECUTION_RULE_PRICE_RANGE_EXCEEDED` (Task 2.3.17).
  - Discretionary Offset / Fill-and-Store (FAS) (Task 2.3.26, §24 #405): Allows passive quotes to execute aggressively within offset range while displaying only passive limit.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ execution rule evaluation logic.
  - Unwritten trade-through price improvement calculator.
  - Unapplied migration `103_orders_discretionary_offset.up.sql`.

##### 10. Slippage Protection & Dynamic Price Banding
- **Spec Coverage:** 100% (§6.6, §6.6a).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.13, 2.3.15).
- **Acceptance Criteria:** §24 #188, #220; Phase-02 AC rows 52, 56.
- **Database Migrations:** `050_instruments_min_notional.up.sql`.
- **Error Codes & Tiers:** `SLIPPAGE_EXCEEDED` (L2/400), `SPREAD_TOO_WIDE` (L2/400), `PRICE_OUT_OF_BAND` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Aggressive market orders automatically converted into synthetic limit orders priced at `best_price ± max_slippage_bps` (default 50 bps, user configurable).
  - Unfilled market order remainder cancelled deterministically upon reaching slippage threshold (`SLIPPAGE_EXCEEDED`).
  - Wide-spread sparse book protection: Market orders rejected if current book spread exceeds $10	imes$ median spread, preventing catastrophic execution into air pockets.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ synthetic limit converter.
  - Unwritten rolling median spread tracker in C++ core.
  - Unapplied migration `050_instruments_min_notional.up.sql`.

##### 11. Auction Mechanisms (Reopening Call, Daily Close, Liquidation)
- **Spec Coverage:** 100% (§7.1, §13.4).
- **Planning Coverage:** 100% (Phase-15 Tasks 15.3.6, 15.3.10, 15.3.13; Phase-19 Task 19.3.4).
- **Acceptance Criteria:** §24 #33, #34, #35, #36, #142, #316, #401; Phase-15 AC rows 20, 24, 27; Phase-19 AC rows 14, 15, 16, 17, 29.
- **Database Migrations:** `015_create_liquidation_auctions.up.sql`, `016_create_insurance_fund.up.sql`.
- **Error Codes & Tiers:** `AUCTION_ACTIVE` (L2/409), `AUCTION_CLEANUP_FAILED` (L1/503), `AMEND_IN_AUCTION_REJECTED` (L2/409), `LIQUIDATION_AUCTION_FAILED` (L1/500).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Reopening Call Auction: Maximum executable volume uncrossing algorithm with minimum surplus and market price tie-breakers; 5-minute call phase with streaming indicative price/volume; 30s extension on clearing failure (§24 #142, #316).
  - Daily Closing Auction: Configurable calendar uncross matching Market-on-Close (MOC) orders at daily benchmark fixing times (§24 #401).
  - Liquidation Auction: 4-phase lifecycle (`CALL` 5s $ightarrow$ `FILL` $ightarrow$ `EXTEND` $\le$60s $ightarrow$ `FORCE_CASH`). Price floor decays 0.5% per 5s extension from 0.98/1.02 floor. LP rebate of 0.05% paid from Insurance Fund.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ auction uncrossing engine.
  - Unwritten indicative price calculation thread.
  - Unapplied migrations `015_` and `016_`.

##### 12. Order-to-Trade Ratio (OTR - MiFID II RTS 9)
- **Spec Coverage:** 100% (§13.6a, §14.1).
- **Planning Coverage:** 100% (Phase-13 Task 13.3.6).
- **Acceptance Criteria:** §24 #140; Phase-13 AC row 35.
- **Database Migrations:** `047_risk_limits_otr.up.sql`.
- **Error Codes & Tiers:** `OTR_LIMIT_EXCEEDED` (L2/429).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - MiFID II RTS 9 dual formulation specified: volume ratio $OTR_v = rac{\sum 	ext{Order Volume}}{\sum 	ext{Trade Volume}}$ and count ratio $OTR_c = rac{\sum 	ext{Orders}}{\sum 	ext{Trades}}$.
  - Evaluated per account and instrument over rolling 60-second and daily windows.
  - Invariant: Exceeding regulatory threshold throttles account and rejects subsequent orders with `OTR_LIMIT_EXCEEDED` (HTTP 429).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go OTR monitoring service and Redis sliding window counter.
  - Unapplied migration `047_risk_limits_otr.up.sql`.

---

#### 3.1.3 Domain 1 Summary Metrics & Component Matrix

| # | Component | Spec Coverage | Plan Coverage | §24 Criteria | DB Migrations | Error Codes | Code % |
|---|---|---|---|---|---|---|---|
| 1 | Limit Order Book (LOB) | 100% (§3.1, §3.6) | 100% (Task 2.3.1, 2.3.13) | #1, #2, #5, #6, #11, #12 | 001, 005, 020 | `CAPACITY_EXCEEDED`, `BOOK_CROSS_ERROR` | 0% |
| 2 | Deterministic Matching | 100% (§2.1, §3.2, §3.4) | 100% (Task 2.3.2, 2.3.10, 2.3.23) | #1, #6, #7, #8, #394, #402 | 005, 006 | `WAL_CRC_CORRUPTED`, `LEDGER_IMBALANCE` | 0% |
| 3 | Ingress Queue & IPC | 100% (§2.2, §2.7.3) | 100% (Task 1.3.5, 2.3.7, 2.3.19) | #11, #12, #14, #299 | In-memory shm | `CRITICAL_BACKPRESSURE`, `SYSTEM_OVERLOAD` | 0% |
| 4 | Price-Time Priority | 100% (§3.1, §6.5, §6.9) | 100% (Task 2.3.1, 2.3.20) | #1, #336 | 005 | `ORDER_NOT_FOUND`, `BOOK_CROSS_ERROR` | 0% |
| 5 | Cross-Shard 2PC | 100% (§2.1, §2.7.2, §13.1) | 100% (Task 2.3.8, 2.3.12, 2.3.25) | #41, #118, #176, #214, #320, #404 | 058 | `CROSS_SHARD_TIMEOUT`, `CROSS_SHARD_LIMIT_EXCEEDED` | 0% |
| 6 | Atomic Amend | 100% (§6.9, §8.4) | 100% (Task 2.3.20, 5.3.22, 5.3.37) | #282, #288, #336 | 024, 038 | `STALE_MODIFY`, `ORDER_AMEND_REJECTED` | 0% |
| 7 | Scoped Mass Cancel | 100% (§6.5, §8.4, §9.3) | 100% (Task 5.3.25, 5.3.33, 18.3.16) | #153, #260, #272, #365 | 005, 024, 046 | `CANCEL_REJECTED`, `ORDER_NOT_FOUND` | 0% |
| 8 | STP Engine | 100% (§6.5) | 100% (Task 2.3.11, 2.3.16, 2.3.18, 2.3.21) | #4, #154, #274, #279, #280, #368 | 038, 072, 094 | `SELF_TRADE_PREVENTED`, `WASH_TRADE_DETECTED` | 0% |
| 9 | Execution Rules | 100% (§6.5, §6.6b, §6.11) | 100% (Task 2.3.17, 2.3.22, 2.3.26) | #129, #130, #277, #400, #405 | 038, 072, 103 | `POST_ONLY_VIOLATION`, `REDUCE_ONLY_VIOLATION` | 0% |
| 10 | Slippage Protection | 100% (§6.6, §6.6a) | 100% (Task 2.3.13, 2.3.15) | #188, #220 | 050 | `SLIPPAGE_EXCEEDED`, `SPREAD_TOO_WIDE` | 0% |
| 11 | Auction Mechanisms | 100% (§7.1, §13.4) | 100% (Task 15.3.6, 15.3.10, 19.3.4) | #33–#36, #142, #316, #401 | 015, 016 | `AUCTION_ACTIVE`, `AUCTION_CLEANUP_FAILED` | 0% |
| 12 | OTR Limits (RTS 9) | 100% (§13.6a, §14.1) | 100% (Task 13.3.6) | #140 | 047 | `OTR_LIMIT_EXCEEDED` | 0% |

---

### 3.2 Domain 2: Instrument & Market Admin

#### 3.2.1 Domain Overview & Architectural Baseline
The Instrument & Market Admin domain governs instrument definitions, state machine lifecycles, administrative operations, trading schedules, reference price bands, and regulatory market constraints. It enforces institutional safeguards (Maker-Checker dual control, 24/5 trading week alignment, ISDA Modified Following split-currency holiday calendars) across both Go gateway services and the C++ engine core.

- **Spec Coverage:** **100%** (Formally defined in Spec v7.0 §3.3, §5.1, §5.44, §6.3, §6.6, §6.7, §7.1–§7.6, §8.2, §8.4, §13.6).
- **Planning Coverage:** **100%** (20 tasks across Phase-02 Tasks 2.3.3/2.3.9/2.3.17, Phase-03 Task 3.3.8, Phase-05 Tasks 5.3.35/5.3.44, Phase-07 Task 7.3.1, Phase-13 Task 13.3.1, Phase-15 Tasks 15.3.1–15.3.13).
- **Acceptance Criteria Traceability:** **28 §24 Criteria** (#26, #38, #46, #78, #108, #119, #123, #138, #156, #157, #173, #188, #203, #217, #234, #239, #259, #277, #290, #316, #343, #352, #401).
- **Database Migrations Mapped:** 9 Migrations (`001_`, `010_`, `011_`, `050_`, `051_`, `072_`, `087_`, `090_`, `091_`).
- **Error Severity Mapping:** L1 (Global Halt / Session Failure), L2 (Instrument State Rejection), L3 (Pre-Trade Validation Failure / Dual Control Rejection).
- **Code Implementation:** **0%** (Zero C++ or Go source code commits).

---

#### 3.2.2 Detailed Component Evaluation

##### 1. Instrument Lifecycle State Machine (7 States)
- **Spec Coverage:** 100% (§7.1, §7.4).
- **Planning Coverage:** 100% (Phase-15 Tasks 15.3.1, 15.3.3, 15.3.9).
- **Acceptance Criteria:** §24 #119, #290, #343, #352; Phase-15 AC rows 1–7, 23, 25, 26.
- **Database Migrations:** `001_create_instruments.up.sql`, `087_instrument_reference.up.sql`.
- **Error Codes & Tiers:** `INSTRUMENT_SUSPENDED` (L2/409), `INSTRUMENT_HALTED` (L2/409), `INSTRUMENT_DELISTED` (L2/409), `INSTRUMENT_CANCEL_ONLY` (L2/409), `INVALID_INSTRUMENT_STATE_TRANSITION` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Defined exact 7-state operational lifecycle: `DRAFT`, `ACTIVE`, `POST_ONLY`, `CANCEL_ONLY`, `RESTRICTED`, `SUSPENDED`, `HALTED`, `DELISTED`.
  - Defined explicit transition matrices, permissions, and automated grace periods: `SUSPENDED` provides a 5-minute cancel-only window before auto-cancelling resting orders; `RESTRICTED` allows only limit order reductions; `DELISTED` provides a 30-day position wind-down period (§24 #119).
  - State broadcast to C++ cores via IPC and external clients via WebSocket / FIX TradingSessionStatus (Tag 35=h).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go state machine transition engine.
  - Unwritten C++ core status listener.
  - Unapplied migrations `001_` and `087_`.

##### 2. Maker-Checker (Dual-Control) Administration
- **Spec Coverage:** 100% (§7.2, §8.2, §8.4).
- **Planning Coverage:** 100% (Phase-07 Task 7.3.1; Phase-15 Tasks 15.3.2, 15.3.5, 15.3.8, 15.3.12).
- **Acceptance Criteria:** §24 #26, #138, #234, #239, #352; Phase-07 AC row 1; Phase-15 AC rows 8, 18, 22, 26; Phase-05 AC row 54.
- **Database Migrations:** `010_create_admin_audit_log.up.sql`, `051_trade_busts.up.sql`, `090_admin_role_bindings.up.sql`, `091_fleet_and_ops_console.up.sql`.
- **Error Codes & Tiers:** `DUAL_CONTROL_REQUIRED` (L3/403), `CHECKER_CANNOT_BE_MAKER` (L3/403), `PROPOSAL_ALREADY_APPROVED` (L2/409), `UNAUTHORIZED_ROLE` (L3/403).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Strict two-person rule specified for sensitive actions: instrument creation/listing, tick/lot parameter adjustments, delisting, emergency resume, trade busts/obvious error adjustments, manual liquidations, and global kill-switch triggers.
  - Separation of duties: Maker user ID cannot approve own proposal as Checker.
  - Immutable audit trail: All proposals and approvals stored with SHA-256 hash chains (`admin_audit_log`, migration 010).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go Maker-Checker API handlers and approval workflows.
  - Unwritten operations console frontend screens.
  - Unapplied migrations `010_`, `051_`, `090_`, `091_`.

##### 3. Trading-Hours 24/5 Enforcement & Session Clock
- **Spec Coverage:** 100% (§6.7, §7.1, §7.4).
- **Planning Coverage:** 100% (Phase-15 Tasks 15.3.4, 15.3.7, 15.3.11).
- **Acceptance Criteria:** §24 #217, #343; Phase-15 AC rows 11–13, 21, 25.
- **Database Migrations:** `001_create_instruments.up.sql`, `087_instrument_reference.up.sql`.
- **Error Codes & Tiers:** `MARKET_CLOSED` (L2/409), `WEEKEND_HALT_ACTIVE` (L2/409), `SESSION_NOT_ACTIVE` (L2/409).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Continuous 24/5 institutional FX schedule: Sydney open Sunday 21:00 UTC through New York close Friday 22:00 UTC.
  - Session boundaries specified: Friday close halts matching, cancels DAY/GTD orders, and preserves GTC orders; Sunday pre-open window accepts orders into call auction; 21:00 UTC uncrosses the book into active continuous matching (§24 #217).
  - Out-of-hours order rejection with `MARKET_CLOSED` (HTTP 409).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go session scheduler daemon.
  - Unwritten C++ session transition events.
  - Unapplied migration `087_instrument_reference.up.sql`.

##### 4. Multi-Currency Holiday Calendar (ISDA Modified Following)
- **Spec Coverage:** 100% (§5.44, §6.3, §7.4).
- **Planning Coverage:** 100% (Phase-03 Task 3.3.8; Phase-15 Task 15.3.11).
- **Acceptance Criteria:** §24 #123, #343; Phase-03 AC row 29; Phase-15 AC row 25.
- **Database Migrations:** `087_instrument_reference.up.sql`.
- **Error Codes & Tiers:** `HOLIDAY_SETTLEMENT_SUSPENDED` (L2/409), `VALUE_DATE_INVALID` (L3/400), `NON_BUSINESS_DAY` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Comprehensive multi-jurisdiction bank holiday matrix (US Federal Reserve, UK Bank of England, ECB, Bank of Japan, Sydney, Canada, Mexico).
  - Pair-aware split holiday logic: if either base or quote currency center is closed, settlement rolls per ISDA Modified Following Business Day convention (rolls forward unless crossing month boundary, then rolls backward).
  - T+1 / T+2 spot FX value-date determination accounting for regional bank holidays and DST shifts.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go holiday calendar engine.
  - Unpopulated holiday tables for major global central bank financial centers.
  - Unapplied migration `087_instrument_reference.up.sql`.

##### 5. Reference-Price Collars & Volatility Bands
- **Spec Coverage:** 100% (§3.3, §6.6, §7.1, §13.6).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.3, 2.3.9, 2.3.17; Phase-13 Task 13.3.1).
- **Acceptance Criteria:** §24 #38, #188, #277; Phase-02 AC rows 10, 46, 52, 59; Phase-13 AC row 1.
- **Database Migrations:** `011_create_risk_limits.up.sql`, `050_instruments_min_notional.up.sql`.
- **Error Codes & Tiers:** `PRICE_OUT_OF_BAND` (L3/400), `COLLAR_EXCEEDED` (L3/400), `EXECUTION_RULE_PRICE_RANGE_EXCEEDED` (L2/409).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Static and dynamic price collars around Mark Price / Index Price.
  - Fast C++ pre-trade check: orders with limit prices outside the allowed band rejected in $< 10\mu s$ with `PRICE_OUT_OF_BAND`.
  - Circuit Breaker Tier 1 (Instrument): 2% price deviation over 5s triggers 60s trading pause; Tier 2: 5% deviation triggers 5-minute halt with reopening auction.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ pre-trade price collar check.
  - Unwritten dynamic collar adjustment service in Go.
  - Unapplied migrations `011_` and `050_`.

##### 6. Minimum Notional & Quantization Constraints
- **Spec Coverage:** 100% (§3.3, §5.1, §8.4).
- **Planning Coverage:** 100% (Phase-02 Task 2.3.3; Phase-05 Tasks 5.3.35, 5.3.44; Phase-15 Task 15.3.11).
- **Acceptance Criteria:** §24 #156, #157, #259, #343; Phase-02 AC rows 10, 46; Phase-05 AC rows 41, 60; Phase-15 AC row 25.
- **Database Migrations:** `001_create_instruments.up.sql`, `050_instruments_min_notional.up.sql`, `087_instrument_reference.up.sql`.
- **Error Codes & Tiers:** `MIN_NOTIONAL_VIOLATION` (L3/400), `MAX_NOTIONAL_EXCEEDED` (L3/400), `INVALID_PRICE_TICK` (L3/400), `INVALID_LOT_SIZE` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Quantization invariants specified: Order price must be an exact integer multiple of `tick_size` (pipettes); order quantity must be an exact integer multiple of `lot_size` (step size).
  - Notional value ($qty 	imes price$) validated against `min_notional` and `max_notional` at gateway pre-check and C++ matching loop.
  - Public `GET /api/v1/instruments` and `GET /api/v1/venue/info` endpoints expose structured filter objects (§24 #259).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go input sanitizer & filter validator.
  - Unwritten C++ tick/lot quantization assertions.
  - Unapplied migrations `001_`, `050_`, `087_`.

---

#### 3.2.3 Domain 2 Summary Metrics & Component Matrix

| # | Component | Spec Coverage | Plan Coverage | §24 Criteria | DB Migrations | Error Codes | Code % |
|---|---|---|---|---|---|---|---|
| 1 | Lifecycle (7 States) | 100% (§7.1, §7.4) | 100% (Task 15.3.1, 15.3.9) | #119, #290, #343, #352 | 001, 087 | `INSTRUMENT_SUSPENDED`, `INSTRUMENT_HALTED` | 0% |
| 2 | Maker-Checker Admin | 100% (§7.2, §8.2) | 100% (Task 7.3.1, 15.3.2, 15.3.12) | #26, #138, #234, #239, #352 | 010, 051, 090, 091 | `DUAL_CONTROL_REQUIRED`, `UNAUTHORIZED_ROLE` | 0% |
| 3 | 24/5 Trading Hours | 100% (§6.7, §7.1) | 100% (Task 15.3.4, 15.3.7) | #217, #343 | 001, 087 | `MARKET_CLOSED`, `WEEKEND_HALT_ACTIVE` | 0% |
| 4 | Holiday Calendar (ISDA) | 100% (§5.44, §6.3) | 100% (Task 3.3.8, 15.3.11) | #123, #343 | 087 | `HOLIDAY_SETTLEMENT_SUSPENDED`, `VALUE_DATE_INVALID` | 0% |
| 5 | Price Collars & Bands | 100% (§3.3, §6.6) | 100% (Task 2.3.3, 2.3.9, 13.3.1) | #38, #188, #277 | 011, 050 | `PRICE_OUT_OF_BAND`, `COLLAR_EXCEEDED` | 0% |
| 6 | Min Notional & Quant | 100% (§3.3, §5.1) | 100% (Task 2.3.3, 5.3.35, 15.3.11) | #156, #157, #259, #343 | 001, 050, 087 | `MIN_NOTIONAL_VIOLATION`, `INVALID_PRICE_TICK` | 0% |

---

### 3.3 Domain 3: Order Types & Execution Strategies

#### 3.3.1 Domain Overview & Architectural Baseline
Domain 3 defines the trading interface and execution semantics for all 14 order types supported by the exchange suite, ranging from standard CLOB limit/market orders to complex algorithmic slicing strategies (TWAP, VWAP, VP), multi-leg composites (OCO, OTO, OPO, OPOCO, Brackets), bot automation (Grid Trading), and institutional credit/risk protections (Pegged, GSLO).

- **Spec Coverage:** **100%** (Formally defined in Spec v7.0 §3.1, §3.2, §4.1–§4.3, §6.1, §6.2, §6.2a, §6.2b, §6.5, §6.6a, §6.10, §6.11, §8.4, §16.1).
- **Planning Coverage:** **100%** (32 tasks across Phase-02 Tasks 2.3.1/2.3.2/2.3.10/2.3.15/2.3.25/2.3.26, Phase-05 Tasks 5.3.3, 5.3.32, 5.3.37, 5.3.39, Phase-16 Tasks 16.3.1–16.3.25).
- **Acceptance Criteria Traceability:** **36 §24 Criteria** (#1, #2, #3, #5, #47, #48, #49, #50, #51, #52, #53, #124, #129, #130, #197, #198, #220, #252, #255, #275, #287, #296, #317, #336, #365, #367, #394, #395, #399, #400, #405).
- **Database Migrations Mapped:** 7 Migrations (`005_`, `016_`, `038_`, `066_`, `071_`, `075_`, `103_`).
- **Error Severity Mapping:** L1 (Oracle Staleness Disabling Triggers), L2 (Conditional Trigger Shortfall / Rejection), L3 (Input Parameter / Tick Quantization Rejection).
- **Code Implementation:** **0%** (Zero C++ or Go source code commits).

---

#### 3.3.2 Detailed Component Evaluation

##### 1. Limit Order (Standard, Post-Only, Reduce-Only)
- **Spec Coverage:** 100% (§3.1, §3.2, §6.1, §6.5).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.1, 2.3.2, 2.3.3; Phase-16 Task 16.3.10).
- **Acceptance Criteria:** §24 #1, #2, #129, #130; Phase-02 AC rows 2, 3, 47, 48.
- **Database Migrations:** `005_create_orders.up.sql`, `038_orders_execution_params.up.sql`.
- **Error Codes & Tiers:** `POST_ONLY_VIOLATION` (L3/400), `REDUCE_ONLY_VIOLATION` (L3/400), `INSUFFICIENT_MARGIN` (L2/400), `PRICE_OUT_OF_BAND` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Standard passive/aggressive limit order semantics specified.
  - Supports Time-in-Force (TIF): GTC, GTD, DAY, IOC, FOK.
  - Enforces `post_only` (guarantees maker execution; rejected with `POST_ONLY_VIOLATION` if crosses book) and `reduce_only` (guarantees position reduction; clipped or rejected with `REDUCE_ONLY_VIOLATION`).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ limit order processing logic.
  - Unapplied migrations `005_` and `038_`.

##### 2. Market Order (Standard, Slippage-Protected, Quote-Denominated)
- **Spec Coverage:** 100% (§3.2, §6.1, §6.6a, §8.4).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.2, 2.3.15; Phase-05 Task 5.3.39).
- **Acceptance Criteria:** §24 #2, #220; Phase-02 AC rows 4, 56; Phase-05 AC row 64.
- **Database Migrations:** `005_create_orders.up.sql`, `072_execution_rules_stp_groups.up.sql`.
- **Error Codes & Tiers:** `SLIPPAGE_EXCEEDED` (L2/400), `BOOK_EMPTY` (L2/400), `QUOTE_QUANTITY_INVALID` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Immediate execution against resting book depth.
  - Dynamic synthetic limit conversion: automatically converted to limit at `best_price ± max_slippage_bps` to prevent execution into thin liquidity.
  - Quote-quantity denomination supported (`quote_quantity`, Task 5.3.39) allowing "spend exactly X quote currency" market orders.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ market matching and remainder cancellation routine.
  - Unwritten quote-to-base quantity calculator in Go gateway.

##### 3. Stop Order (Stop-Loss, Stop-Limit)
- **Spec Coverage:** 100% (§6.1, §6.2a).
- **Planning Coverage:** 100% (Phase-02 Task 2.3.2; Phase-16 Tasks 16.3.3, 16.3.17).
- **Acceptance Criteria:** §24 #255; Phase-02 AC row 9; Phase-16 AC row 39.
- **Database Migrations:** `005_create_orders.up.sql`, `066_orders_trigger_source.up.sql`.
- **Error Codes & Tiers:** `TRIGGER_PRICE_INVALID` (L3/400), `ORACLE_STALE` (L1/503), `INSUFFICIENT_MARGIN` (L2/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Rests off-book in conditional order memory until trigger price is crossed.
  - Triggers on `LAST_PRICE`, `MARK_PRICE`, or `INDEX_PRICE`.
  - Converts to Market order (Stop-Loss) or Limit order (Stop-Limit) and submits into matching engine.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten off-book conditional trigger monitor daemon.
  - Unapplied migration `066_orders_trigger_source.up.sql`.

##### 4. Take-Profit Order (TP-Market, TP-Limit)
- **Spec Coverage:** 100% (§6.1, §6.2a).
- **Planning Coverage:** 100% (Phase-16 Tasks 16.3.5, 16.3.14, 16.3.17).
- **Acceptance Criteria:** §24 #51, #255; Phase-16 AC rows 16, 39.
- **Database Migrations:** `005_create_orders.up.sql`, `066_orders_trigger_source.up.sql`.
- **Error Codes & Tiers:** `TRIGGER_PRICE_INVALID` (L3/400), `ORACLE_STALE` (L1/503).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Favorable price movement lock-in order.
  - Evaluated against user-selected trigger source (`LAST`, `MARK`, `INDEX`).
  - Activates when market price $\ge$ trigger price (for SELL) or $\le$ trigger price (for BUY).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten TP trigger evaluation loop.

##### 5. Trailing Stop
- **Spec Coverage:** 100% (§6.2, §6.2a).
- **Planning Coverage:** 100% (Phase-16 Tasks 16.3.3, 16.3.15, 16.3.17).
- **Acceptance Criteria:** §24 #50, #255; Phase-16 AC rows 6, 7, 8, 36, 39.
- **Database Migrations:** `005_create_orders.up.sql`, `038_orders_execution_params.up.sql`, `066_orders_trigger_source.up.sql`.
- **Error Codes & Tiers:** `TRAILING_OFFSET_INVALID` (L3/400), `ORACLE_STALE` (L1/503).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Monotonic trailing stop engine: stop price adjusts dynamically in favorable direction; never regresses during adverse market movement.
  - Distance unit options: `PIPS` (pipettes), `PERCENTAGE`, or `ABSOLUTE` fiat value (Task 16.3.15).
  - Triggers as Market order upon breach.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten dynamic trailing stop manager.

##### 6. Bracket / OCO / OTO Composite Orders
- **Spec Coverage:** 100% (§6.2, §6.2a).
- **Planning Coverage:** 100% (Phase-16 Tasks 16.3.5, 16.3.14, 16.3.20, 16.3.22).
- **Acceptance Criteria:** §24 #47, #51, #255, #317, #367; Phase-16 AC rows 16, 17, 18, 37, 39, 43, 45.
- **Database Migrations:** `038_orders_execution_params.up.sql`, `075_order_lists_opo.up.sql`.
- **Error Codes & Tiers:** `SPREAD_ORDER_REJECTED` (L3/400), `OCO_SIBLING_CANCEL_FAILED` (L2/409), `CHILD_TRIGGER_FAILED` (L2/409).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Bracket Order: Entry order bundled with Take-Profit and Stop-Loss child orders.
  - OCO (One-Cancels-the-Other): Fill of one leg triggers atomic cancellation of sibling leg within 100ms (`oco_group_id`).
  - OTO (One-Triggers-Other): Secondary orders remain inactive until primary order fills.
  - Deterministic race resolution specified for near-simultaneous triggers (Task 16.3.22, §24 #317).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go composite order orchestrator.
  - Unapplied migration `075_order_lists_opo.up.sql`.

##### 7. Iceberg Order
- **Spec Coverage:** 100% (§3.1, §3.2, §6.1).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.1, 2.3.2; Phase-16 Task 16.3.10).
- **Acceptance Criteria:** §24 #5; Phase-02 AC row 8; Phase-16 AC row 34.
- **Database Migrations:** `005_create_orders.up.sql`, `038_orders_execution_params.up.sql`.
- **Error Codes & Tiers:** `INVALID_ICEBERG_SLICE` (L3/400), `DISPLAY_QTY_EXCEEDS_TOTAL` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Large order split into visible display slice (`display_qty`) and hidden reserve.
  - L2/L3 market data feeds publish only visible quantity.
  - Upon full fill of visible slice, next slice replenishes from reserve and takes new time priority at back of price level queue (FIFO penalty).
  - Memory-efficient intrusive order node re-use (zero runtime allocator call).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ iceberg replenishment logic in matching core.

##### 8. Time-in-Force Lifecycle (GTD / GTC / DAY / IOC / FOK) & Expiry Scheduler
- **Spec Coverage:** 100% (§3.2, §6.1, §6.7).
- **Planning Coverage:** 100% (Phase-02 Tasks 2.3.2, 2.3.10).
- **Acceptance Criteria:** §24 #3, #394; Phase-02 AC rows 5, 6, 27, 28, 53.
- **Database Migrations:** `005_create_orders.up.sql`.
- **Error Codes & Tiers:** `ORDER_EXPIRED` (L2/400), `INVALID_EXPIRY_TIMESTAMP` (L3/400), `TIF_NOT_SUPPORTED` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Full TIF suite specified: `GTC` (capped at 90 days per Business Ruling R8), `GTD` (explicit UTC timestamp), `DAY` (session close), `IOC` (partial fill and cancel remainder), `FOK` (all-or-nothing fill).
  - C++ Expiry Scheduler (Task 2.3.10): High-resolution priority queue / min-heap of expiring orders.
  - Deterministic `TIME_TICK` events in binary WAL ensure exact recovery replay without clock skew (§24 #394).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ expiry min-heap scheduler.
  - Uncompiled TIF expiry integration test suite.

##### 9. Pegged Orders (Peg-to-Mid, Peg-to-Primary, Peg-to-Market)
- **Spec Coverage:** 100% (§6.2).
- **Planning Coverage:** 100% (Phase-16 Task 16.3.11 — single owner).
- **Acceptance Criteria:** §24 #395; Phase-16 AC rows 9, 10, 31.
- **Database Migrations:** `038_orders_execution_params.up.sql`.
- **Error Codes & Tiers:** `PEG_OFFSET_INVALID` (L3/400), `PEG_PRICE_CROSS_REJECTED` (L2/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Re-pegging limit order tracking reference book prices with optional offset.
  - Re-peg event joins the back of the queue at the new price level (preserves FIFO fairness).
  - Bounded re-pegging frequency prevents order book churning and CPU overload.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten C++ / Go pegged order engine.

##### 10. Algorithmic Orders: TWAP, VWAP & Volume Participation (VP)
- **Spec Coverage:** 100% (§4.1–§4.3, §6.2).
- **Planning Coverage:** 100% (Phase-16 Tasks 16.3.1, 16.3.2, 16.3.8, 16.3.12, 16.3.18, 16.3.22, 16.3.23).
- **Acceptance Criteria:** §24 #48, #49, #275, #317, #365; Phase-16 AC rows 1–5, 12, 13, 32, 40, 43, 44.
- **Database Migrations:** `038_orders_execution_params.up.sql`.
- **Error Codes & Tiers:** `ALGO_NOT_CERTIFIED` (L3/403), `ALGO_SCHEDULE_INVALID` (L3/400), `CHILD_SLICE_REJECTED` (L2/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - TWAP: Uniform time-sliced child order distribution over horizon $T$.
  - VWAP: Volume-weighted slicing based on historical intraday volume curves.
  - Volume Participation (VP): Dynamically sizes child orders to maintain a target percentage of real-time market volume (§24 #275).
  - Anti-gaming randomization: $\pm 15\%$ jitter on child slice sizes and time delays (Task 16.3.12).
  - Clean lifecycle: parent order cancellation guarantees zero orphan child slices (§24 #365).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go Algo Order Manager service.
  - Unwritten trade tape listener for VP execution.

##### 11. Net-Proceeds Composite Orders (OPO & OPOCO)
- **Spec Coverage:** 100% (§6.2, §6.10).
- **Planning Coverage:** 100% (Phase-16 Tasks 16.3.20, 16.3.24).
- **Acceptance Criteria:** §24 #287, #367; Phase-16 AC rows 41, 45.
- **Database Migrations:** `075_order_lists_opo.up.sql`.
- **Error Codes & Tiers:** `NET_PROCEEDS_INSUFFICIENT` (L2/400), `OPO_PARENT_FAILED` (L2/409), `ORDER_LIST_NOT_FOUND` (L2/404).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - OPO: Primary BUY executes; secondary SELL order dynamically sized from actual delivered net proceeds after deducting commissions, fees, and lot quantization.
  - OPOCO: Primary BUY fill dynamically spawns an OCO Take-Profit / Stop-Loss pair sized to exact delivered balance.
  - Atomic persistence in `order_lists_opo` table.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Go OPO/OPOCO calculation handler.
  - Unapplied migration `075_order_lists_opo.up.sql`.

##### 12. Grid Trading Bot Engine
- **Spec Coverage:** 100% (§6.2).
- **Planning Coverage:** 100% (Phase-16 Task 16.3.19).
- **Acceptance Criteria:** §24 #275; Phase-16 AC row 40.
- **Database Migrations:** `071_grid_bots.up.sql`.
- **Error Codes & Tiers:** `MAX_GRID_BOTS_EXCEEDED` (L3/429), `GRID_PARAMETERS_INVALID` (L3/400), `GRID_MARGIN_INSUFFICIENT` (L2/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Automated grid bot strategy: deploys arithmetic or geometric ladder of BUY and SELL orders within defined price envelope.
  - Automatic replenishment: filling a buy submits paired sell one grid higher; filling a sell submits paired buy one grid lower.
  - Enforces cap of maximum 5 concurrent bots per account (Business Ruling R13).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten Grid Trading bot runner service.
  - Unapplied migration `071_grid_bots.up.sql`.

##### 13. Conditional Dual-Price Trigger Engine (LAST, MARK, INDEX)
- **Spec Coverage:** 100% (§6.2a).
- **Planning Coverage:** 100% (Phase-16 Tasks 16.3.17, 16.3.22).
- **Acceptance Criteria:** §24 #255, #317; Phase-16 AC rows 39, 43.
- **Database Migrations:** `066_orders_trigger_source.up.sql`.
- **Error Codes & Tiers:** `INVALID_TRIGGER_SOURCE` (L3/400), `ORACLE_STALE` (L1/503).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Client selectable trigger benchmark: `LAST_PRICE`, `MARK_PRICE`, or `INDEX_PRICE`.
  - Protects traders from manipulation / flash crashes by decoupling stop triggers from temporary CLOB price anomalies.
  - Fail-closed invariant: Oracle feed staleness $> 5s$ immediately suspends trigger evaluation with `ORACLE_STALE` (§24 #255).
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten trigger evaluation engine in Go / C++.
  - Unapplied migration `066_orders_trigger_source.up.sql`.

##### 14. Guaranteed Stop Loss Order (GSLO with Risk Premium)
- **Spec Coverage:** 100% (§6.2).
- **Planning Coverage:** 100% (Phase-16 Task 16.3.16).
- **Acceptance Criteria:** §24 #252; Phase-16 AC row 38.
- **Database Migrations:** `005_create_orders.up.sql`, `016_create_insurance_fund.up.sql`, `038_orders_execution_params.up.sql`.
- **Error Codes & Tiers:** `GSLO_EXPOSURE_EXCEEDED` (L2/400), `PREMIUM_INSUFFICIENT` (L2/400), `GSLO_NOT_SUPPORTED_FOR_INSTRUMENT` (L3/400).
- **Code Implementation:** **0%**.
- **WHAT HAS BEEN DONE:**
  - Guaranteed fill exactly at stop price regardless of gap size or market illiquidity.
  - Exchange assumes gap risk, financed by non-refundable risk premium charged at placement and deposited into Insurance Fund.
  - Premium refunded if order is cancelled prior to triggering.
  - Aggregate venue exposure limits enforced on GSLO open interest to safeguard exchange solvency.
- **WHAT HAS NOT BEEN DONE:**
  - Unwritten GSLO premium billing and insurance routing service.
  - Unwritten guaranteed fill simulator / execution hook.
  - Unapplied migrations `016_` and `038_`.

---

#### 3.3.3 Domain 3 Summary Metrics & Component Matrix

| # | Component | Spec Coverage | Plan Coverage | §24 Criteria | DB Migrations | Error Codes | Code % |
|---|---|---|---|---|---|---|---|
| 1 | Limit Order | 100% (§3.1, §6.1) | 100% (Task 2.3.1, 16.3.10) | #1, #2, #129, #130 | 005, 038 | `POST_ONLY_VIOLATION`, `REDUCE_ONLY_VIOLATION` | 0% |
| 2 | Market Order | 100% (§3.2, §6.6a) | 100% (Task 2.3.2, 5.3.39) | #2, #220 | 005, 072 | `SLIPPAGE_EXCEEDED`, `BOOK_EMPTY` | 0% |
| 3 | Stop Order | 100% (§6.1, §6.2a) | 100% (Task 2.3.2, 16.3.17) | #255 | 005, 066 | `TRIGGER_PRICE_INVALID`, `ORACLE_STALE` | 0% |
| 4 | Take-Profit | 100% (§6.1, §6.2a) | 100% (Task 16.3.5, 16.3.17) | #51, #255 | 005, 066 | `TRIGGER_PRICE_INVALID`, `ORACLE_STALE` | 0% |
| 5 | Trailing Stop | 100% (§6.2, §6.2a) | 100% (Task 16.3.3, 16.3.15) | #50, #255 | 005, 038, 066 | `TRAILING_OFFSET_INVALID`, `ORACLE_STALE` | 0% |
| 6 | Bracket / OCO / OTO | 100% (§6.2) | 100% (Task 16.3.14, 16.3.22) | #47, #51, #255, #317 | 038, 075 | `SPREAD_ORDER_REJECTED`, `OCO_SIBLING_CANCEL_FAILED` | 0% |
| 7 | Iceberg Order | 100% (§3.1, §6.1) | 100% (Task 2.3.1, 16.3.10) | #5 | 005, 038 | `INVALID_ICEBERG_SLICE`, `DISPLAY_QTY_EXCEEDS_TOTAL` | 0% |
| 8 | TIF & Expiry Scheduler | 100% (§3.2, §6.1) | 100% (Task 2.3.2, 2.3.10) | #3, #394 | 005 | `ORDER_EXPIRED`, `INVALID_EXPIRY_TIMESTAMP` | 0% |
| 9 | Pegged Orders | 100% (§6.2) | 100% (Task 16.3.11) | #395 | 038 | `PEG_OFFSET_INVALID`, `PEG_PRICE_CROSS_REJECTED` | 0% |
| 10 | TWAP / VWAP / VP Algos | 100% (§4.1–§4.3) | 100% (Task 16.3.1, 16.3.12, 16.3.18) | #48, #49, #275, #365 | 038 | `ALGO_NOT_CERTIFIED`, `CHILD_SLICE_REJECTED` | 0% |
| 11 | Net-Proceeds (OPO/OPOCO) | 100% (§6.2, §6.10) | 100% (Task 16.3.20, 16.3.24) | #287, #367 | 075 | `NET_PROCEEDS_INSUFFICIENT`, `OPO_PARENT_FAILED` | 0% |
| 12 | Grid Trading Bot | 100% (§6.2) | 100% (Task 16.3.19) | #275 | 071 | `MAX_GRID_BOTS_EXCEEDED`, `GRID_MARGIN_INSUFFICIENT` | 0% |
| 13 | Dual-Price Trigger | 100% (§6.2a) | 100% (Task 16.3.17) | #255, #317 | 066 | `INVALID_TRIGGER_SOURCE`, `ORACLE_STALE` | 0% |
| 14 | GSLO Guaranteed Stop | 100% (§6.2) | 100% (Task 16.3.16) | #252 | 005, 016, 038 | `GSLO_EXPOSURE_EXCEEDED`, `PREMIUM_INSUFFICIENT` | 0% |

---

### 3.4 Cross-Cutting Invariants, Architectural Gaps & Implementation Day-0 Action Items

#### 3.4.1 Failure Modes & Error Hierarchy Distribution
Across Domains 1, 2, and 3, error handling strictly follows the **Fail-Closed Zero-Loss Pessimism** doctrine:
1. **L0 Critical Core Halt:** Injected upon WAL CRC failure, zero-sum ledger imbalance, PTP clock skew $> 100\mu s$, or matching loop memory violation (`SIGTERM` + dump crash state).
2. **L1 Systemic Degradation:** Triggered when IPC ring buffers hit 80%/95% watermarks, Price Oracle is stale $> 5s$, or clearing auction fails after 3 extensions (`Normal` $ightarrow$ `ReadOnly` / `Throttled`).
3. **L2 Transaction Boundary Error:** Atomic synchronous rejections preserving zero side-effects (`INSUFFICIENT_MARGIN`, `CROSS_SHARD_TIMEOUT`, `SLIPPAGE_EXCEEDED`, `STALE_MODIFY`, `OTR_LIMIT_EXCEEDED`, `MARKET_CLOSED`).
4. **L3 Edge Protocol Rejection:** Gateways filter malformed requests, tick/lot quantization violations (`MIN_NOTIONAL_VIOLATION`, `INVALID_PRICE_TICK`), and unauthorized actions before IPC entry.

#### 3.4.2 Database Migration Audit for Slice 1
Slice 1 features map to **24 distinct database migrations** out of the 108 contiguous migration series:
- **Core Entities & Orders:** `001_create_instruments`, `005_create_orders`, `006_create_trades`, `020_create_indexes`.
- **Audit & Governance:** `010_create_admin_audit_log`, `024_order_audit`, `090_admin_role_bindings`, `091_fleet_and_ops_console`.
- **Risk & Collars:** `011_create_risk_limits`, `047_risk_limits_otr`, `050_instruments_min_notional`.
- **Auctions & Insurance:** `015_create_liquidation_auctions`, `016_create_insurance_fund`, `051_trade_busts`.
- **Execution & Parameters:** `038_orders_execution_params`, `046_fix_sessions_entitlement`, `072_execution_rules_stp_groups`, `094_accounts_default_stp`, `103_orders_discretionary_offset`.
- **Sharding & Reference:** `058_shard_margin_reservations`, `087_instrument_reference`.
- **Order Triggers & Bot Engines:** `066_orders_trigger_source`, `071_grid_bots`, `075_order_lists_opo`.

#### 3.4.3 Day-0 Implementation Action Items (Transition from Planning to Code)
To transition Slice 1 from 0% code implementation to active execution, the following immediate Day-0 engineering tasks must be initiated:
1. **Repository & Scaffold Initialization (Phase 01 Task 1.3.1):**
   - Create root `CMakeLists.txt` for C++20 with `-Wall -Wextra -Werror -O3 -march=native`.
   - Scaffold C++ engine directory structure: `engine/src/core/`, `engine/src/book/`, `engine/include/`.
   - Scaffold Go project: initialize `go.mod` with Go 1.23+, directory structure `services/gateway/`, `services/bridge/`, `services/common/`.
2. **PostgreSQL Migration Scaffold (Phase 01 Task 1.3.3):**
   - Implement `services/internal/db/migrations/001_create_instruments.up.sql` through `021_`.
   - Configure `golang-migrate` / `goose` and test migration application on fresh PostgreSQL 16.
3. **Aeron IPC Shared-Memory Setup (Phase 01 Task 1.3.5 / 1.3.10):**
   - Configure Aeron Media Driver low-latency parameters (`/dev/shm` IPC buffers, 16MB ring buffer).
   - Implement basic C++ Aeron subscriber and Go Aeron publisher test harnesses.
4. **Matching Engine Core Data Structures (Phase 02 Task 2.3.1):**
   - Implement `PriceLevel` intrusive doubly-linked list in C++ without runtime allocations.
   - Implement `OrderBook` flat array of price levels with integer pipette scaling ($10^8$ ticks).

---

## 4. Vertical Slice 2: Connectivity, Market Data & Liquidity Infrastructure (Domains 4, 5, 6)

### 4.1 Domain 4: Pricing & Liquidity Infrastructure
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Real-Time Multilateral Benchmark Integrity & Fail-Closed Staleness Guards (Spec §2.7, §19.5).

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **Mark Price** | §13.1, §13.6d, §15.2, §19.5 | Ph-19.5: 19.5.3.2<br>Ph-19: 19.3.12, 19.3.26<br>Ph-16: 16.3.17<br>Ph-02: 2.3.15<br>Ph-03: 3.3.9 | #45, #90, #91, #92, #120, #180, #190, #255, #397, #410 | `066_orders_trigger_source`<br>Redis `mark_price:{symbol}` | `MARK_PRICE_OUT_OF_BOUNDS` (400, L2)<br>`MARK_PRICE_STALE` (503, L1)<br>`PRICE_ORACLE_UNAVAILABLE` (503, L1) | 0% | Median of $\ge 2$ independent fresh feeds with outlier rejection; cross-currency USD numeraire conversion in $O(N)$ linear batch time without N+1 queries; Aeron publisher at `224.0.1.1:40456`. | Go service in `services/internal/oracle/mark_price.go` unwritten; Aeron C publisher/subscriber uncompiled; tick-driven evaluation loop unwritten. |
| **Index Oracle** | §6.8, §13.1, §19.5, §2.7 | Ph-19.5: 19.5.3.1, 19.5.3.4, 19.5.3.7 | #45, #91, #92, #134, #321 | Redis `index_price:{symbol}`<br>ClickHouse `oracle_ticks_history` | `ORACLE_FEED_STALE` (503, L1)<br>`ORACLE_DIVERGENCE_EXCEEDED` (503, L1) | 0% | Aggregates Refinitiv (TREP), Bloomberg BFIX, and ECB reference rates; minimum 2 fresh sources; volume-weighted average; single PriceOracle consumer pattern; divergence threshold $>25\text{ bps}$ drops outliers. | Ingest connectors for Refinitiv/Bloomberg unwritten; XML parsers for ECB unwritten; running feed ingestion daemon unbuilt. |
| **Staleness Gates** | §2.4, §2.7, §13.1, §13.4, §19.5 | Ph-19.5: 19.5.3.3, 19.5.3.6, 19.5.3.7<br>Ph-16: 16.3.22 | #45, #120, #203, #255, #317, #321, #397 | Redis `oracle:staleness:{symbol}`<br>Prometheus `oracle_feed_staleness_seconds` | `ORACLE_FEED_STALE` (503, L1)<br>`CIRCUIT_BREAKER_TRIGGERED` (503, L1) | 0% | 5-second staleness gate; fail-closed halt of margin orders; flash-crash circuit breaker ($>5\%$ in $<1\text{s}$ triggers 5s cooling freeze before fallback); graduated fallback ladder (5–15s $\rightarrow$ 2% haircut; 15–60s $\rightarrow$ 5% haircut; $>60\text{s}$ $\rightarrow$ FORCE_CASH). | Watchdog goroutines unwritten; fallback state machine and circuit breaker coordination with matching engine unbuilt. |
| **Yield Curves** | §15.2, §15.3, §19.5 | Ph-19.5: 19.5.3.5<br>Ph-03: 3.3.7, 3.3.11<br>Ph-22: 22.3.1, 22.3.3 | #90, #134, #181, #182 | Redis `curve:{currency}`<br>ClickHouse `yield_curve_snapshots` | `YIELD_CURVE_UNAVAILABLE` (503, L1)<br>`STALE_FORWARD_POINTS` (503, L1) | 0% | Fiat curves (SOFR, EURSTR, SONIA, TONAR) across 7 tenors (ON, T/N, 1W, 1M, 3M, 6M, 12M); log-linear interpolation; per-currency day-count conventions (ACT/360 for USD, EUR, CHF, JPY; ACT/365 for GBP, AUD, NZD, CAD). | Central bank rate scraping/ingestion pipelines unwritten; discount curve bootstrap engine in Go or C++ unwritten. |
| **MTF / Fair Value** | §6.3, §6.8, §7.1, §15.1, §15.2 | Ph-19.5: 19.5.3.2<br>Ph-16: 16.3.9<br>Ph-15: 15.3.7<br>Ph-21: 21.3.15<br>Ph-22: 22.3.1 | #90, #157, #181, #182, #401 | `038_orders_execution_params`<br>`054_regulatory_reporting` | `BENCHMARK_UNAVAILABLE` (503, L1)<br>`FIXING_WINDOW_CLOSED` (400, L2)<br>`FAIR_VALUE_DIVERGENCE` (503, L1) | 0% | Covered Interest Parity (CIP) fair value: $F = S \times \frac{1 + r_q \times (d / \text{basis}_q)}{1 + r_b \times (d / \text{basis}_b)}$; benchmark fixing orders (ECB 14:15 CET, WM/Reuters 4:00 PM London) executed during 5-minute uncross windows with TWAP/VWAP reference. | Fixing execution scheduler, TWAP/VWAP calculation routines, and MTF compliance checks unwritten. |
| **LP Scorecard** | §7.5, §19.4 | Ph-07: 7.3.9, 7.3.10 | #227 | `045_market_maker_program`<br>ClickHouse `lp_performance_hourly` | `LP_OBLIGATION_BREACH` (Notice/Alert, L1/L2)<br>`LP_SUSPENDED` (403, L2) | 0% | Quantitative evaluation across 6 dimensions: Quoting Uptime ($\ge 98\%$), Two-Sided Presence ($\ge 95\%$), Max Spread Compliance, Minimum Depth Commitment (5M base currency within 3 pips), Fill Ratio ($\ge 85\%$), Quoting Latency ($p99 < 5\text{ms}$). | Ingestion pipeline from engine execution logs into ClickHouse unwritten; Admin API endpoints unwritten. |
| **ADL (Auto-Deleveraging)** | §13.4, §13.5 | Ph-19: 19.3.4, 19.3.19<br>Ph-10: 10.3.13 | #94, #97, #269, #272, #273 | `015_create_liquidation_auctions`<br>`016_create_insurance_fund`<br>Redis `adl:priority:{symbol}:{side}` | `ADL_TRIGGERED` (L1 event)<br>`LIQUIDATION_FAILED` (500, L1)<br>`INSURANCE_FUND_DEPLETED` (Critical, L1/L0) | 0% | 4-tier liquidation waterfall: Scanner $\rightarrow$ Call Auction $\rightarrow$ Insurance Fund $\rightarrow$ ADL; ranking metric: $\text{ADL Score} = \text{Profit Percentile} \times \text{Effective Leverage}$; deleveraged at bankruptcy price of liquidated account, eliminating bad debt without socialized loss haircuts. | Go liquidation worker and ADL queue processor unwritten; C++ engine forced offsetting trade bridge unbuilt. |
| **MM Program (MMP)** | §9.5, §9.6, §13.8 | Ph-18: 18.3.7, 18.3.10 | #128, #139, #175, #176, #177, #178 | `045_market_maker_program`<br>`046_fix_sessions_entitlement` | `MMP_TRIGGERED` (FIX 35=8 OrdRejReason=99, L2)<br>`MMP_LOCKED_OUT` (403 / FIX 35=b, L2)<br>`QUOTE_REQUEST_REJECTED` (400, L2) | 0% | Sliding window protection ($\Delta t \in [100\text{ms}, 5000\text{ms}]$) on trade count ($N_{\max}$), traded volume ($V_{\max}$), and net delta exposure ($\Delta_{\max}$); on trigger breach, all quotes across instrument/market are atomically purged; requires explicit reset (FIX 35=c or REST). | C++ in-memory sliding window counters unwritten; QuickFIX-Go MMP interceptor unwritten. |

---

### 4.2 Domain 5: APIs & Connectivity
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Full Institutional Connectivity & Zero-GL-Bypass Protocol Translation (Spec §8, §9, §10).

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **REST API** | §8.1–§8.10, §22 | Ph-05: 5.3.1–5.3.46 | #13, #35–42, #70, #156, #192, #208, #225, #239, #254, #256, #258–260, #281–283, #286, #288, #304, #338, #355, #356, #363 | `002_`–`005_`, `024_`, `025_`, `067_`, `073_`, `074_` | `INVALID_REQUEST` (400, L3)<br>`UNAUTHORIZED` (401, L3)<br>`RATE_LIMIT_TIER_EXCEEDED` (429, L3)<br>`INSUFFICIENT_BALANCE` (400, L2) | 0% | Full OpenAPI 3.0 specification; HMAC-SHA256, Ed25519, RSA auth; account-scoped idempotency keys (`idem:{account_id}:{key}`) with PG unique constraint handling; REST Batch Orders (10 submit / 20 cancel); RFC 7807 problem details. | Zero Go code in `services/order-gateway/`; no HTTP router, middleware, DB connection pools, or Aeron publishers written. |
| **WS (Public & Interactive)** | §8.6, §10.5 | Ph-06: 6.3.1–6.3.4, 6.3.7, 6.3.9, 6.3.10<br>Ph-05: 5.3.26, 5.3.31 | #15, #43–46, #99, #187, #215, #245, #253, #265, #283, #284, #305, #339, #408 | Redis sessions, memory ring buffers | `AUTH_EXPIRED` (4019, L3)<br>`WS_RATE_EXCEEDED` (4029, L3)<br>`WS_SLOW_CONSUMER_DROP` (4008, L1) | 0% | Frame discriminator standardized on `action`; error envelope `{"type":"error","error":"<CODE>",...}`; `/ws/market` (public) and `/ws/v1` (interactive trading); sequence-based session resume with 10,000-message ring buffer; in-flight token rotation. | WebSocket server in Go unwritten; client connection hubs and goroutine reader/writers unbuilt. |
| **SBE Multicast (A/B)** | §10.6, §2.3 | Ph-06: 6.3.6, 6.3.18 | #166, #188, #189, #261, #284, #305 | Circular packet buffers | `SBE_FEED_A_DESYNC` (L1)<br>`SBE_REPLAY_GAP_EXCEEDED` (L3) | 0% | Dual independent UDP multicast streams (Feed A: `239.255.0.1:10001`, Feed B: `239.255.0.2:10002`) over separate network routes for zero-loss failover; MoldUDP64 framing with 64-bit monotonic sequences; out-of-band TCP snapshot and historical replay server. | SBE compiler (`sbe-tool`) unexecuted; no C++ UDP multicast publisher or socket tuning code; TCP replay daemon unwritten. |
| **FIX 4.4/5.0 + mTLS** | §9.1–§9.8 | Ph-18: 18.3.1–18.3.18 | #81–88, #128, #139, #167, #175–178, #186, #187, #189, #200, #201, #205, #242, #243, #257, #282–284, #289, #319 | `030_create_fix_sessions`<br>`046_fix_sessions_entitlement`<br>`052_fix_certification` | Tag 35=3 (L3)<br>Tag 35=8 Reject (L2)<br>Tag 35=h TradSesStatus (L1) | 0% | FIX 4.4 (spot) and 5.0 SP2 (derivatives); FIXS mandatory profile (TLS 1.3 with client mTLS bound to SenderCompID); QuickFIX-Go architecture; bidirectional sequence synchronization with ResendRequest (35=2) and Gap Fill (35=4). | QuickFIX-Go application callbacks unwritten; TLS listener and certificate verification logic uncompiled; Aeron bridge unwritten. |
| **Mass Quoting (Tag 35=i)** | §9.5, §3.2 | Ph-18: 18.3.7, 18.3.10 | #128, #175, #176 | `045_market_maker_program` | Tag 35=b QuoteRejectReason=99 (L2)<br>Tag 35=3 Reject (L3) | 0% | FIX Tag 35=i with repeating groups NoQuoteSets (296) and NoQuoteEntries (295); bid and ask levels updated atomically within C++ matching engine in a single book cycle; Tag 35=Z MassQuoteCancel; firm liquidity only (zero last look). | C++ matching engine mass quote handler unwritten; QuickFIX-Go Tag 296/295 parsers unwritten. |
| **Allocation (Tag 35=J/AK)** | §9.7, §17.4 | Ph-18: 18.3.13<br>Ph-24: 24.3.10, 24.3.15 | #200, #237, #242 | `055_trade_allocations` | `ALLOCATION_SUM_MISMATCH` (400, L2)<br>Tag 35=P AllocRejCode=4 (L2) | 0% | Institutional block trade allocation; fund managers execute block orders on omnibus accounts, then dispatch Tag 35=J AllocationInstruction; calculates $P_{\text{avg}}$, asserts $\sum\text{AllocQty} == \text{BlockQty}$, executes double-entry GL transfer, emits Tag 35=AK. | Allocation service in `services/internal/backoffice/` unwritten; QuickFIX-Go Tag 35=J handler unwritten. |
| **API Versioning / Deprecation** | §8.5, §19.2 | Ph-05: 5.3.20, 5.3.28<br>Ph-09: 9.3.6 | #70, #208, #222, #284, #289 | `026_create_api_deprecations` | `ENDPOINT_DEPRECATED` (Header)<br>`ENDPOINT_GONE` (410, L3) | 0% | Implements RFC 8594 headers (`Deprecation`, `Sunset`, `Link`); URL path prefixing `/api/v1` and `/api/v2`; mandatory 180-day grace period; post-sunset deterministic HTTP 410 Gone (`ENDPOINT_GONE`). | Go HTTP middleware unwritten; migration 026 unapplied. |
| **Rate Tiers** | §8.4, §8.7 | Ph-05: 5.3.8, 5.3.15, 5.3.27, 5.3.34<br>Ph-13: 13.3.6 | #39, #192, #221, #254, #258, #288 | `012_create_fee_tiers`<br>`047_risk_limits_otr`<br>`086_vip_tiers`<br>Redis `rate:*`, `ban:ip:*` | `RATE_LIMIT_TIER_EXCEEDED` (429, L3)<br>`IP_BANNED` (418, L3)<br>`OTR_EXCEEDED` (429, L3) | 0% | Multi-tier quota: Anonymous (60 req/min), Authenticated VIP 0 (20 req/s), VIP 5 (200 req/s), Institutional (1,000 req/s); RFC 6585 headers; progressive IP ban (100 req/s $\rightarrow$ 429, abuse $\rightarrow$ 418 IP ban); MiFID II RTS 9 OTR collars ($>500:1$ throttled). | Redis Lua scripts unwritten; Go rate-limiting middleware unwritten. |
| **Dead-Man / CoD** | §8.9, §9.4, §10.5 | Ph-05: 5.3.33<br>Ph-06: 6.3.7<br>Ph-18: 18.3.9, 18.3.16 | #245, #257, #260, #319 | `046_fix_sessions_entitlement`<br>Redis `deadman:{account_id}` | `DEAD_MAN_TRIGGERED` (L2)<br>`INVALID_TIMEOUT` (400, L3) | 0% | Unified multi-protocol countdown: single account-level timer shared across REST, WS, FIX; endpoint `POST /api/v1/orders/cancel-all-after` refreshes Redis TTL; on expiry or socket disconnect, resting orders are purged via Aeron mass-cancel; max 1 CoD per 5s rate limit. | Redis expiration listener daemon unwritten; gateway disconnect handlers unwritten. |
| **SOR (Smart Order Routing)** | §9.8 | Ph-18: 18.3.14<br>Ph-16: 16.3.20 | #193, #194, #205, #242, #247, #288 | `037_create_prime_brokerage`<br>`038_orders_execution_params` | `ROUTING_REJECTED` (400, L2)<br>`EXTERNAL_VENUE_TIMEOUT` (504, L2)<br>`FILL_BRIDGE_MISMATCH` (409, L2) | 0% | 5-state lifecycle (`PENDING_ROUTE` $\rightarrow$ `ROUTED` $\rightarrow$ `PARTIALLY_FILLED_EXTERNAL` $\rightarrow$ `FILLED_EXTERNAL` $\rightarrow$ `CANCELLED_EXTERNAL`); aggregates external liquidity; synthetic "shadow orders" with locked local collateral; `FILL_BRIDGE` translation within 500ms timeout budget. | Go SOR router and external FIX gateway connectors unwritten. |

---

### 4.3 Domain 6: Market Data Products
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Zero-Conflation Transparency, High-Throughput Analytics & Monotonic Sequencing (Spec §10, §11, §16, §20, §23).

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **L2 Book** | §10.1, §10.2, §10.5 | Ph-06: 6.3.2, 6.3.15<br>Ph-05: 5.3.5 | #17, #43, #44, #83, #84, #265, #408 | Redis cache, memory | `BOOK_CROSSED_DETECTED` (409, L2/L1)<br>`INVALID_DEPTH_LIMIT` (400, L3) | 0% | Full snapshot + incremental delta protocol; contiguous sequence numbers with `prev_last_seq` and CRC32 level checksums in every packet to prevent crossed order books; configurable depth levels (5, 10, 20) and conflation windows (100ms standard, 10ms institutional, 1000ms slow). | Go conflation engine unwritten; no CRC32 checksum calculator or depth serializers compiled. |
| **L3 Order-Level Data** | §11.1–§11.5 | Ph-17: 17.3.1–17.3.5<br>Ph-06: 6.3.21 | #18, #78, #79, #80, #84, #197, #244, #318 | `029_create_surveillance_signals` | `L3_UNAUTHORIZED` (403, L3)<br>`L3_SNAPSHOT_TOO_LARGE` (413, L3)<br>`L3_CONSUMER_OVERRUN` (L1) | 0% | Pure order-by-order lifecycle stream (ADD, MODIFY, CANCEL, EXECUTE) with nanosecond timestamps; client anonymization; hidden orders (`ORDER_TYPE_HIDDEN`) and iceberg slices suppressed until execution (§24 #197); async snapshot generation off secondary WAL reader. | C++ `L3Publisher.cpp` unwritten; no ITCH framing or Go WebSocket broadcaster. |
| **Klines (13 TF)** | §8.3, §10.3, §16.2 | Ph-06: 6.3.8, 6.3.14<br>Ph-20: 20.3.1, 20.3.2 | #45, #226, #231, #257, #264 | ClickHouse `fx_klines_{1m..1d}`, memory 1s | `INVALID_INTERVAL` (400, L3)<br>`QUERY_LIMIT_EXCEEDED` (400, L3) | 0% | 13 canonical timeframes: `1s`, `1m`, `3m`, `5m`, `15m`, `30m`, `1h`, `2h`, `4h`, `6h`, `8h`, `12h`, `1d`; 12 persisted to ClickHouse `SummingMergeTree` / `AggregatingMergeTree` tables; 1s maintained in memory; WS push capped at $\le 2\text{ updates/sec}$; TradingView UDF `/history`. | Go kline aggregation goroutine unwritten; ClickHouse tables uncreated; TradingView UDF handler unwritten. |
| **Tick History** | §16.1, §22, §23 | Ph-20: 20.3.2<br>Ph-23: 23.3.1, 23.3.4 | #65, #100, #236 | ClickHouse `fx_ticks` (LZ4, 90d TTL) | `INVALID_TICK_CURSOR` (400, L3)<br>`HISTORICAL_QUERY_TIMEOUT` (504, L1) | 0% | ClickHouse `fx_ticks` sustaining $\ge 50,000$ inserts/sec with LZ4 compression and 90-day raw TTL; REST endpoint `GET /api/v1/history/ticks/{symbol}` with opaque cursor-based pagination; JSON and CSV streaming; trade bust lineage preserved via `is_bust=1`. | ClickHouse batch ingestion daemon from NATS unwritten; HTTP CSV/JSON streaming handlers unwritten. |
| **Book Ticker / BBO** | §10.4 | Ph-06: 6.3.11 | #261, #288, #388 | Streaming IPC bypass | `BBO_DESYNC` (L1)<br>`STREAM_DISCONNECTED` (L3) | 0% | Zero-conflation top-of-book stream emitting every best-bid and best-ask modification without waiting for the 100ms depth conflation timer; hot-path Aeron IPC bypass direct to distributor. | C++ hot-path BBO filter unwritten; Go streaming hub unwritten. |
| **Liquidation Feed** | §10.5, §13.4 | Ph-06: 6.3.13<br>Ph-19: 19.3.26 | #256, #263, #388 | Derived from `015_create_liquidation_auctions` | `LIQUIDATION_FEED_DELAYED` (L1) | 0% | Public WebSocket stream `liquidation` pushing forced liquidations and auction outcomes; mandatory Anti-Front-Running Delay Gate: public feed is deliberately delayed by **2 seconds** so algorithmic traders cannot front-run active call auctions (§24 #263); full anonymization. | 2-second delay ring buffer and WebSocket push worker unwritten. |
| **WS Subscriptions** | §10.5 | Ph-06: 6.3.4, 6.3.16 | #46, #84, #215, #259, #265 | Connection subscriber trie | `SUBSCRIPTION_LIMIT_EXCEEDED` (close 4008/4029, L3) | 0% | Dynamic JSON subscription framing within established connection (`{"action":"subscribe","params":["eurusd@depth20","gbpusd@trade"]}`); subscription cap enforced at max 100 active streams per connection; combined URL stream paths (`/ws/stream?streams=...`); deduplication. | Go multiplexer and subscriber registration trees unwritten. |
| **SLA & Monitoring** | §2.1, §10.7, §19.3 | Ph-06: 6.3.5, 6.3.17<br>Ph-09: 9.3.14<br>Ph-23: 23.3.3 | #12, #13, #15, #99, #162, #183, #238, #380 | ClickHouse `sla_latency_metrics_1m`<br>Prometheus | `SLA_BREACH_WARNING` (L1)<br>`ERROR_BUDGET_EXHAUSTED` (L1) | 0% | Hard SLIs/SLOs: C++ matching latency (p99 $\le 50\mu\text{s}$), REST latency (p99 $\le 5\text{ms}$), WebSocket push (p99 $\le 10\text{ms}$ internal, $\le 100\text{ms}$ edge), Availability ($99.95\%$ 24/5); Multi-Window Multi-Burn-Rate alerting (1h burn 14.4x $\rightarrow$ P1 page; 6h burn 6x $\rightarrow$ P2 ticket). | PTP hardware timestamping integration unwritten; Prometheus exporters and Grafana dashboards unconfigured. |
| **Block Tape** | §10.8, §16.1 | Ph-06: 6.3.20<br>Ph-23: 23.3.2, 23.3.7 | #235, #237, #291, #358 | `055_trade_allocations`<br>ClickHouse `block_trades_tape` | `BLOCK_TRADE_NOT_FOUND` (404, L3)<br>`UNAUTHORIZED_TAPE_ACCESS` (403, L3) | 0% | Captures transactions $\ge \$1,000,000$ USD notional; MiFID II post-trade transparency deferred publication rules (up to 15-min delay); immediate dissemination to regulatory ARMs; public stream `blockTape` and REST endpoint `GET /api/v1/history/block-trades/{symbol}`. | Block trade deferral queue in Go unwritten; ClickHouse tape tables uncreated. |

---

## 5. Vertical Slice 3: Risk, Compliance & Post-Trade Operations (Domains 7, 8, 9)

### 5.1 Domain 7: Risk & Credit
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Strict Pre-Trade Capital Preservation & Solvency Invariants (Spec §2.7, §13).

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **Margin Modes (Cross, Isolated, Portfolio)** | §13.1, §13.6g, §13.15 | Ph-19: 19.3.1, 19.3.11, 19.3.18, 19.3.26, 19.3.27 | #32, #96, #176, #229, #230, #320, #398, #410, #411 | `013_`, `058_`, `085_`, `106_` | `MARGIN_INSUFFICIENT` (400, L2)<br>`CROSS_SHARD_TIMEOUT` (504, L2)<br>`ISOLATED_MARGIN_DEFICIT` (400, L2) | 0% | Cross, isolated, and portfolio margin; 90-day correlation matrix computation with $\|\rho\| > 0.7$ offset formula: $\text{offset} = \min(m_1, m_2) \times \rho \times f_{\text{offset}}$ (default factor 0.5, cap 0.8, 20% gross regulatory floor); 2PC cross-shard headroom reservation (500µs budget, 5s rollback). | Go microservices (`services/internal/risk/*`, `services/internal/margin/*`) unwritten; C++ headroom allocator unwritten; live correlation matrix feed ingestor unwritten. |
| **Leverage Tiers (ESMA / CFTC / Inst)** | §13.2, §13.6f, §13.14 | Ph-19: 19.3.2, 19.3.17, 19.3.23, 19.3.24 | #97, #231, #366, #371 | `001_`, `011_`, `098_` | `LEVERAGE_EXCEEDS_TIER_MAX` (400, L2)<br>`INVALID_LEVERAGE` (400, L3) | 0% | ESMA retail schedules (30:1 major / 20:1 minor / 10:1 exotic); CFTC retail (50:1 major / 20:1 minor); dynamic institutional notional bands (Tier 1 $\le \$5\text{M}$ at 100:1, Tier 2 $\$5\text{M}$–$\$20\text{M}$ at 50:1, Tier 3 $>\$20\text{M}$ at 20:1). | Leverage tier evaluation middleware and dynamic margin scaling algorithms unwritten. |
| **Liquidation Ladder (2s Scanner, Auction, Decay)** | §13.3, §13.4, §13.5 | Ph-19: 19.3.3, 19.3.16, 19.3.19, 19.3.22, 19.3.26<br>Ph-19.5: 19.5.3.6 | #32–37, #120, #230, #241, #269, #360, #397 | `014_create_liquidations`<br>`015_create_liquidation_auctions` | `LIQUIDATION_IN_PROGRESS` (409, L2)<br>`FORCE_CASH_FAILED` (500, L1) | 0% | 2s scanner cadence; 4-phase liquidation auction (trigger OI > 1%; CALL 5s, FILL continuous, EXTEND $\le 60\text{s}$ total in 5s increments with 0.5% decay); floor $\times 0.98/\times 1.02$; FORCE_CASH at mark $\times 0.95/\times 1.05$; 0.05% LP rebate from insurance fund. | Go liquidation daemon and auction clearing manager unwritten. |
| **Retail Negative Balance Protection (NBP)** | §13.6c, §2.7.1 | Ph-19: 19.3.9, 19.3.20<br>Ph-03: 3.3.6 | #133, #228, #320 | `016_create_insurance_fund`<br>`036_create_general_ledger`<br>`102_` | `NBP_DEFICIT_TRIGGERED` (L1)<br>`INSURANCE_FUND_EXHAUSTED` (500, L0) | 0% | Regulatory mandated zero-floor balance for retail clients; automated balance reset via Insurance Fund debit; balanced DEBIT Insurance Fund / CREDIT User Balance double-entry GL journal posting. | Automated NBP trigger daemon and GL balance restorer unwritten. |
| **Insurance Fund & Governance** | §13.4, §13.6c, §13.12, §17.13 | Ph-19: 19.3.4, 19.3.14, 19.3.21<br>Ph-24: 24.3.17 | #35, #36, #97, #218, #344 | `016_create_insurance_fund`<br>`082_treasury` | `INSURANCE_FUND_DEPLETED` (500, L1/L0)<br>`ADL_TRIGGERED` (L1) | 0% | Capital buffer sizing (0.5% OI calibration); automatic waterfall: Liquidation penalty fees $\rightarrow$ Retained earnings $\rightarrow$ Contingent capital facility $\rightarrow$ ADL on depletion. | Fund balance monitor and automated capital injection worker unwritten. |
| **Stress Testing & Backtesting** | §13.10 | Ph-19: 19.3.13, 19.3.21 | #206, #344 | `064_stress_test_scenarios` | `MODEL_VALIDATION_FAILED` (500, L1) | 0% | Historical replay of extreme FX shocks (SNB 2015 unpeg, Brexit 2016, Flash Crash 2019); automated daily Monte Carlo simulation ($N=10,000$) on multi-asset portfolio exposures. | Risk model simulation engine and automated stress testing cron unwritten. |
| **PB Credit Limits (NOP / DSL Pre-Trade Check)** | §13.7 | Ph-19: 19.3.7<br>Ph-18: 18.3.6 | #125 | `037_create_prime_brokerage` | `PB_CREDIT_EXCEEDED` (403, L2)<br>`NOP_LIMIT_EXCEEDED` (403, L2) | 0% | Pre-trade Net Open Position (NOP) and Daily Settlement Limit (DSL) credit checks in Go gateway before order dispatch to matching core; Traiana affirmation reconciliation. | PB credit cache in Redis and pre-trade limit middleware unwritten. |
| **Collateral Haircuts & Concentration** | §13.6b, §13.15 | Ph-19: 19.3.8, 19.3.28 | #145, #412 | `041_collateral_haircuts` | `COLLATERAL_NOT_ELIGIBLE` (400, L2)<br>`CONCENTRATION_LIMIT_EXCEEDED` (400, L2) | 0% | Multi-tier fiat haircuts: G10 cash (0%), minor currencies (5–15%), exotic currencies (20–40%); dynamic volatility-scaled haircuts based on 10-day ATR; maximum 30% single-currency concentration cap. | Dynamic haircut calculator and balance valuation service unwritten. |
| **Position / OTR Limits (MiFID II RTS 9)** | §13.6, §13.6a | Ph-13: 13.3.6<br>Ph-19: 19.3.5 | #140 | `011_create_risk_limits`<br>`047_risk_limits_otr` | `MAX_EXPOSURE_EXCEEDED` (400, L2)<br>`OTR_LIMIT_EXCEEDED` (429, L2) | 0% | Net and gross position caps per currency pair; RTS 9 Order-to-Trade Ratio (OTR) collars ($>500:1$ throttled, $>1000:1$ suspended); sliding 1-minute and 1-hour window tracking. | Redis sliding-window OTR evaluators and limit enforcement hooks unwritten. |
| **Credit Groups (Mutual Bilateral Credit)** | §13.8 | Ph-19: 19.3.10 | #165 | `053_bilateral_credit` | `BILATERAL_CREDIT_EXHAUSTED` (403, L2)<br>`NO_MUTUAL_CREDIT` (403, L2) | 0% | Matrix of bilateral credit limits between institutional counterparties; credit-screened order book matching; prevents execution if bilateral NOP limit is exhausted. | Bilateral credit matrix evaluator and C++ credit filtering hook unwritten. |

---

### 5.2 Domain 8: Compliance & AML
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Full Regulatory Transparency, Immutable Auditability & Fail-Closed Screening (Spec §11, §14, §16, §21).

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **KYC Tiers & Onboarding** | §14.2, §12.7 | Ph-14: 14.3.4, 14.3.7, 14.3.10<br>Ph-12: 12.3.13 | #27, #101, #102, #141, #385 | `017_`, `042_`, `095_`, `099_` | `KYC_TIER_EXCEEDED` (403, L2)<br>`KYC_VERIFICATION_REQUIRED` (403, L2) | 0% | 4-tier KYC framework: Tier 0 (Email verified, read-only/demo), Tier 1 (ID verified, $10k/day), Tier 2 (Proof of address, $100k/day), Institutional (Full corporate KYB, negotiable limits); annual re-verification. | KYC document processing pipeline, Sumsub/Onfido webhooks, and tier escalation daemons unwritten. |
| **PEP & Adverse Media Screening** | §14.3 | Ph-21: 21.3.11 | #149 | `017_`, `033_`, `060_` | `PEP_MATCH_FLAGGED` (403, L2)<br>`ADVERSE_MEDIA_ALERT` (L2) | 0% | Daily batch screening against Politically Exposed Persons (PEP) and adverse media watchlists (Dow Jones, World-Check); fuzzy name matching with Levenshtein distance $\le 2$; compliance escalation workflow. | Watchlist ingestion scripts and fuzzy matching evaluation workers unwritten. |
| **Sanctions Screening & PreTrade Hook** | §14.3, §7.5, §2.7 | Ph-21: 21.3.1, 21.3.10, 21.3.23 | #28, #29, #219, #323 | `009_create_audit_hash_chain`<br>`060_compliance_rules` | `SANCTIONS_BLOCKED` (403, L2)<br>`SANCTIONS_QUARANTINE` (500, L1) | 0% | Real-time OFAC, EU, UN, UK HMT sanctions screening; pre-trade `SanctionsHook` in C++ gateway ($<10\mu\text{s}$ check via in-memory bitset); fail-closed scoped degradation on provider disconnect. | C++ `SanctionsHook.cpp` and automated list synchronizers unwritten. |
| **Travel Rule (FATF / IVMS 101)** | §14.3, §5.19 | Ph-21: 21.3.2 | #107 | `032_travel_rule_records` | `TRAVEL_RULE_MISSING_INFO` (400, L2)<br>`TRAVEL_RULE_REJECTED` (403, L2) | 0% | FATF Recommendation 16 compliance for cross-institution fiat banking wires $\ge \$1,000$ USD; standardized IVMS 101 payload exchange; originator and beneficiary verification. | Travel Rule messaging gateway and IVMS 101 protocol serializing engine unwritten. |
| **SAR / CTR Filing (FinCEN Form 107)** | §14.1, §14.3 | Ph-21: 21.3.3, 21.3.6 | #105, #108 | `033_sar_filings`<br>`060_compliance_rules` | `CTR_TRIGGERED` (Audit Event)<br>`SAR_DUAL_CONTROL_REQUIRED` (400, L2) | 0% | Automated Currency Transaction Report (CTR) for cash/rail movements $>\$10,000$ within 24h; Suspicious Activity Report (SAR) filing workflow with dual-control compliance sign-off; XML export for FinCEN BSA E-Filing. | BSA E-Filing XML generator and compliance officer review dashboard unwritten. |
| **MiFID II Compliance (RTS 6/22/25/27/28)** | §14.1, §14.5, §16 | Ph-21: 21.3.4, 21.3.12, 21.3.16, 21.3.19<br>Ph-09: 9.3.12 | #30, #106, #150, #178, #202, #246, #345 | `054_regulatory_reporting`<br>`059_mifid_classifications` | `MIFID_REPORTING_FAILED` (500, L1/L2)<br>`CLOCK_SKEW_EXCEEDED` (500, L0/L1) | 0% | Comprehensive MiFID II package: RTS 6 algo trading controls, RTS 22 transaction reporting (UnaVista/TRAX ARM integration, T+1 23:59 CET deadline), RTS 25 PTP clock synchronization ($\le 100\mu\text{s}$ UTC), RTS 27/28 best execution. | ARM connectivity adapter and ISO 20022 XML reporting builders unwritten. |
| **EMIR REFIT & CFTC Parts 43/45** | §14.1a, §5.32 | Ph-21: 21.3.5, 21.3.14 | #31, #169, #170, #345 | `054_regulatory_reporting`<br>`059_` | `INVALID_UTI_FORMAT` (400, L2)<br>`TR_SUBMISSION_REJECTED` (500, L1/L2) | 0% | Derivative trade lifecycle reporting to DTCC / Regis-TR Trade Repositories; Unique Trade Identifier (UTI) generation (ISO 23897); Unique Product Identifier (UPI); Legal Entity Identifier (LEI); T+1 lifecycle reporting. | Trade Repository API connectors and ISO 20022 derivative reporting messages unwritten. |
| **Dodd-Frank Swap Reporting** | §14.1a | Ph-21: 21.3.9, 21.3.14 | #170, #345 | `054_regulatory_reporting`<br>`059_` | `DODD_FRANK_REPORT_FAILED` (500, L1/L2)<br>`SDR_UNAVAILABLE` (503, L1) | 0% | CFTC real-time public reporting (Part 43) within regulatory dissemination windows; regulatory swap data reporting (Part 45) to registered SDR. | SDR transmission engine and real-time public swap disseminator unwritten. |
| **FinCEN MSB Program** | §14.1 | Ph-21: 21.3.6 | #105, #108 | `033_sar_filings`<br>`060_` | `MSB_COMPLIANCE_BREACH` (500, L1/L2) | 0% | Money Services Business (MSB) registration tracking (Form 107); Customer Due Diligence (CDD) and Enhanced Due Diligence (EDD) rule engines; annual AML training audit logs. | FinCEN registration portal integration and compliance rule evaluators unwritten. |
| **GDPR & Privacy Controls** | §14.1, §14.7 | Ph-21: 21.3.7 | #103, #104, #342, #383 | `002_create_users`<br>`009_`<br>`060_` | `GEO_BLOCKED` (403, L2)<br>`GDPR_ERASURE_RESTRICTED` (409, L2) | 0% | Articles 15 (Access), 17 (Erasure), 20 (Portability); automated data export; legal retention carve-out: financial ledger and AML/KYC data retained for 7 years per regulatory mandates while PII is pseudonymized. | Automated GDPR request processor and PII masking workers unwritten. |
| **Surveillance & Case Management** | §11, §14.1c, §14.4 | Ph-17: 17.3.3<br>Ph-21: 21.3.8, 21.3.21, 21.3.27 | #18, #207, #274, #392 | `029_create_surveillance_signals`<br>`054_` | `MARKET_ABUSE_DETECTED` (400, L2)<br>`SURVEILLANCE_LAG_WARNING` (L1) | 0% | 7 market abuse detection patterns: Spoofing, Layering, Wash Trading, Marking the Close, Momentum Ignition, Front-Running, Insider Dealing; SLA case management: URGENT $\le 4\text{h}$, REVIEW $\le 24\text{h}$. | Surveillance signal consumer and case management backend API unwritten. |
| **Communications Recording (Taping)** | §14.8 | Ph-21: 21.3.20 | #203 | `062_comms_recordings` | `TAPING_STORAGE_ERROR` (500, L1/L0)<br>`WORM_BREACH` (500, L0) | 0% | MiFID II Art. 16(7) mandate: recording of all electronic communications (orders, chat, support tickets, internal execution logs); WORM immutable S3 storage (Object Lock); 5-year regulatory retention (7-year if requested). | S3 Object Lock storage integration and chat/voice recording ingestion daemons unwritten. |
| **CRS / FATCA Tax Reporting** | §14.1 | Ph-21: 21.3.22<br>Ph-20: 20.3.10 | #249 | `054_regulatory_reporting`<br>`059_` | `TAX_REPORT_GENERATION_FAILED` (500, L2)<br>`INVALID_TIN_FORMAT` (400, L3) | 0% | Common Reporting Standard (CRS) and Foreign Account Tax Compliance Act (FATCA) annual reporting; OECD XML schemas; automatic TIN validation; account balance and gross proceeds aggregation. | Tax data extractors and OECD XML generator unwritten. |
| **Basel III Capital Adequacy** | §14.1 | Phase-21: 21.3.13 | #151 | `054_regulatory_reporting`<br>`082_treasury` | `CAPITAL_ADEQUACY_BREACH` (500, L1/L0)<br>`LEVERAGE_RATIO_BREACH` (500, L1) | 0% | Automated Risk-Weighted Assets (RWA) calculation for FX spot, forward, and derivative credit risk; Common Equity Tier 1 (CET1) ratio monitoring; daily capital adequacy alerting. | Basel III calculator service and regulatory capital dashboard unwritten. |
| **FX Global Code Assessment** | §14.6 | Phase-21: 21.3.17 | #182 | `060_compliance_rules` | `FX_GLOBAL_CODE_NON_COMPLIANT` (Audit Warning) | 0% | Adherence tracking to all 55 principles of the FX Global Code; firm quote guarantees (Principle 17); no last-look; transparent order execution disclosures. | Automated compliance checklist and audit affirmation generator unwritten. |
| **Data Residency Enforcer** | §14.7 | Phase-21: 21.3.18 | #184 | `060_compliance_rules` | `CROSS_BORDER_ROUTING_PROHIBITED` (403, L2) | 0% | Jurisdictional data sovereignty rules; ensures PII, trade records, and encryption keys for regulated entities remain in designated geographical regions. | Network routing filters and multi-region database tenancy enforcer unwritten. |

---

### 5.3 Domain 9: Trade Lifecycle Ops
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Zero General Ledger Bypass, Multi-Currency Balance Conservation & PvP Settlement (Spec §5.21, §6.3, §17).

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **FX Settlement Cycles (T+1/T+2/Same-Day)** | §6.3, §17.1 | Ph-24: 24.3.1–24.3.3, 24.3.6<br>Ph-03: 3.3.3 | #19, #20, #21 | `019_`, `022_`, `044_`, `104_` | `SETTLEMENT_FAILED` (500, L2)<br>`FUTURE_SETTLEMENT_PENDING` (400, L2) | 0% | Standard T+1 spot settlement for major FX; T+2 for exotics; same-day settlement for USD/CAD and USD/MXN; value date calculation with split currency holiday calendar. | Go settlement engine (`services/internal/settlement/`) unwritten; value-date scheduler unbuilt. |
| **Banking Rails (SWIFT/SEPA/FedNow)** | §17.2 | Ph-11: 11.3.1, 11.3.2, 11.3.7, 11.3.9<br>Ph-24: 24.3.4, 24.3.12, 24.3.20, 24.3.21 | #22–25, #79, #80, #175, #413, #414 | `007_`, `008_`, `035_`, `040_`, `057_`, `078_`, `107_`, `108_` | `BANKING_RAIL_UNAVAILABLE` (503, L1)<br>`BANKING_RAIL_CUTOFF_PASSED` (400, L2) | 0% | Direct banking rails: SWIFT (MT103, MT202, MT900, MT910), SEPA (SCT / Inst), FedNow, ACH, CHAPS, TARGET2; cut-off time enforcement; deposit fraud tiers ($<\$10\text{K}$ auto, $\$10\text{K}$–$\$50\text{K}$ standard, $>\$50\text{K}$ manual 4h review). | Bank gateway adapters, SWIFT Alliance connectors, and ISO 20022 message parsers unwritten. |
| **CLS PvP Settlement (ISO 20022)** | §17.3a, §17.6 | Ph-24: 24.3.8, 24.3.16 | #126, #168, #326 | `019_`, `044_`, `053_` | `CLS_MATCH_FAILED` (409, L2)<br>`CLS_WINDOW_CLOSED` (400, L2) | 0% | Continuous Linked Settlement (CLS) third-party ISO 20022 XML adapter (`pacs.008`, `pacs.009`, `camt.054`); Payment-versus-Payment (PvP) atomic settlement eliminating Herstatt risk. | CLS message dispatcher and XML validation pipeline unwritten. |
| **Nostro / Vostro & Bank Reconcile** | §17.3, §17.10 | Ph-24: 24.3.1, 24.3.2, 24.3.12, 24.3.19 | #20, #21, #78, #175, #347 | `018_`, `057_`, `082_` | `NOSTRO_INSUFFICIENT_FUNDS` (500, L1/L2)<br>`NOSTRO_BREAK` (L1) | 0% | Multi-currency commercial bank account reconciliation; automated ingestion of MT940 (end-of-day statement), MT942 (interim report), and camt.053 XML; 3-way match: Internal Ledger vs Gateway vs Bank. | Bank statement ingestion daemon and automated break matchers unwritten. |
| **GL Double-Entry Posting** | §5.21a, §17, §2.7.1 | Ph-03: 3.3.6, 3.3.19, 3.3.21<br>Ph-24: 24.3.17, 24.3.21 | #9, #121, #301, #339, #358, #414 | `036_`, `088_`, `096_`, `102_`, `108_` | `LEDGER_UNBALANCED` (500, L0)<br>`ZERO_BALANCE_VIOLATION` (400, L2) | 0% | Strict zero-GL-bypass invariant: every transaction emits balanced DEBIT and CREDIT lines ($\sum\text{Debit} == \sum\text{Credit}$); multi-currency balance-sheet chart of accounts; immutable append-only journal entries. | Go ledger service `DoubleEntryLedgerService::postJournal()` unwritten; PG triggers unapplied. |
| **Tom-Next Rollover & Swap Engine** | §6.3, §17.4, §17.5, §17.15 | Ph-03: 3.3.7, 3.3.8, 3.3.11, 3.3.12, 3.3.23<br>Ph-23: 23.3.9 | #122, #123, #134, #221, #406, #407 | `001_`, `088_`, `105_` | `ROLLOVER_CALCULATION_ERROR` (500, L1)<br>`SWAP_RATE_STALE` (503, L1) | 0% | Automated 17:00 ET spot rollover; Tom-Next swap points calculation based on central bank benchmark rates; Triple-Swap Wednesday accounting; holiday calendar roll forward. | Daily rollover daemon and swap points calculator unwritten. |
| **Trade Confirmations (MT515)** | §16, §17 | Ph-20: 20.3.6, 20.3.8<br>Ph-24: 24.3.3 | #235, #237, #374 | `049_trade_confirmations` | `CONFIRMATION_DELIVERY_FAILED` (500, L2) | 0% | Automated generation of post-trade confirmations per MiFID II Art. 25; SWIFT MT515 generation for institutional counterparties; encrypted PDF generation and portal distribution. | Confirmation generator daemon and email/SFTP delivery workers unwritten. |
| **Trade Cost Analysis (TCA RTS 28)** | §14.5, §16 | Ph-20: 20.3.9<br>Ph-21: 21.3.19 | #202, #246 | ClickHouse MergeTree | `TCA_DATA_INSUFFICIENT` (400, L2) | 0% | Execution quality metrics: Arrival Price Slippage, Implementation Shortfall, Spread Capture, Reversion; automated quarterly RTS 27/28 reports. | ClickHouse TCA calculation queries and reporting exporter unwritten. |
| **Post-Trade Allocation & Bunched Orders** | §17.8 | Ph-24: 24.3.10, 24.3.15<br>Ph-18: 18.3.13 | #172, #237 | `055_trade_allocations` | `ALLOCATION_SUM_MISMATCH` (400, L2)<br>`ALLOCATION_INVALID` (400, L2) | 0% | Post-trade allocation of block trades to sub-accounts (FIX 35=J / 35=AK); volume-weighted average price ($P_{\text{avg}}$) calculation; atomic balance rebalancing across funds. | Allocation engine in `services/internal/backoffice/` unwritten. |
| **CSDR Settlement Discipline** | §17.14 | Ph-24: 24.3.13, 24.3.14, 24.3.19 | #347, #413 | `084_csdr_settlement_fails` | `CSDR_PENALTY_INCURRED` (Audit Event)<br>`BUY_IN_TRIGGERED` (500, L1/L2) | 0% | CSDR Article 7 settlement discipline: automatic flagging of settlement fails at ISD+1; daily cash penalties calculation; mandatory buy-in process initiation at ISD+4 (liquid) / ISD+7 (illiquid). | CSDR monitoring daemon and penalty accounting engine unwritten. |
| **Internal Position Transfers** | §13.9 | Ph-19: 19.3.12 | #190 | `061_position_transfers` | `POSITION_TRANSFER_FAILED` (400, L2)<br>`TRANSFER_INSUFFICIENT` (400, L2) | 0% | Atomic off-book position transfers between accounts (allocations, give-ups, admin adjustments); mark-to-market valuation at transfer time; atomic collateral unlock on source and initial margin lock on destination. | Position transfer service and balance verification routine unwritten. |
| **Trade Busts & Price Adjustment** | §7.4 | Ph-15: 15.3.5, 15.3.6, 15.3.10 | #138 | `051_trade_busts` | `TRADE_BUST_REJECTED` (400, L2)<br>`BUST_WINDOW_EXPIRED` (400, L2) | 0% | Obvious error policy: trade bust or price adjustment requested within 15 minutes of execution for prices deviating $>2\times$ prevailing price band; dual-control admin approval; atomic GL reversal journal posting. | Trade bust admin workflow and reversing entry generator unwritten. |

---

## 6. Vertical Slice 4: Resilience, Delivery, Client Experience & Governance (Domains 10, 11, 12, 13)

### 6.1 Domain 10: Recovery & Resilience
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Strict Fail-Closed Zero-Loss Pessimism (Spec §2.7). System halts deterministically on state corruption or consensus divergence.

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **Custom Binary WAL & Recovery Ladder** | §3.5, §18.1, §18.5, §2.7 | Ph-01: 1.3.6<br>Ph-04: 4.3.1, 4.3.2, 4.3.5, 4.3.9, 4.3.11<br>Ph-04.5: 4.5.3.1 | #302, #335, #353 (Phase-04 AC #1–8, #18, #19, #27–29) | `065_recovery_reports`<br>`092_recovery_digests` | `WAL_RECOVERY_HALT` (500, L0/L1)<br>`WAL_CORRUPT` (500, L0)<br>`SNAPSHOT_INTEGRITY_FAILED` (500, L0/L1) | 0% | 3-level graduated ladder (CRC repair → Snapshot rebase → Fail-closed halt with `recovery_reports` row); aligned `O_DIRECT` 4KB block flushing; snapshot CRC32C trailers; deterministic `TIME_TICK` events. | Zero C++ code written (`WalWriter.cpp`, `WalReader.cpp`, `SnapshotManager.cpp`); `exchange:replay-from-archive` S3 sync daemon unwritten. |
| **PostgreSQL Reconciliation (9 Categories)** | §3.5, §5.21, §17.1, §18.2, §18.3, §18.5 | Ph-13: 13.3.2<br>Ph-03: 3.3.6<br>Ph-04: 4.3.5, 4.3.8 | #67, #121, #125 (Phase-13 AC #12–14, #25–32, #34; Phase-03 AC #20–22) | `004_create_balances`<br>`009_create_audit_hash_chain`<br>`036_create_general_ledger`<br>`088_gl_chart_of_accounts`<br>`093_balance_snapshots` | `RECONCILIATION_MISMATCH` (500, L1 - auto-halt)<br>`LEDGER_IMBALANCE` (500, L1)<br>`ZERO_SUM_VIOLATION` (500, L1) | 0% | Hourly automated reconciliation across 9 financial correctness categories (Balances, Positions, Orders, Trades, Funding, Settlement, Fees, P&L, GL Zero-Sum); continuous balance sheet invariant $\sum\text{debits} == \sum\text{credits}$. | Go service package `services/internal/reconciliation/` unwritten; automated reconciliation report generator and PagerDuty dispatcher unwritten. |
| **Multi-Region DR (RPO/RTO Targets)** | §18.3, §19.3 | Ph-04: 4.3.3<br>Ph-09: 9.3.4, 9.3.20 | #48, #181, #335 (Phase-09 AC #13–16, #39–40; Phase-04 AC #13–16) | PG physical replication slots, Redis Sentinel HA topology | `DISASTER_RECOVERY_TRIGGERED` (503, L1)<br>`REPLICATION_LAG_EXCEEDED` (500, L1)<br>`SENTINEL_QUORUM_LOST` (500, L1) | 0% | Explicit institutional DR targets: Redis RPO ≤ 5s / RTO ≤ 30s; PostgreSQL RPO ≤ 15s / RTO ≤ 5min; C++ core RPO = 0 / RTO < 10s; ClickHouse RPO ≤ 60s / RTO ≤ 30min; fencing tokens. | Terraform/Ansible multi-region definitions (`deploy/terraform/regions/`) unwritten; failover trigger daemons and Route 53 health checkers unwritten. |
| **Quarterly DR Drill Program** | §18.3, §19.5, §19.11.1 | Ph-09: 9.3.21, 9.3.27 | #211, #331 (Phase-09 AC #41, #47) | `010_create_admin_audit_log`<br>`docs/runbooks/dr-drill.md` | `DR_DRILL_RTO_EXCEEDED` (L1/P1)<br>`DR_DRILL_RPO_EXCEEDED` (L1/P1) | 0% | Quarterly full primary → secondary failover drill procedure; structured post-drill template for DORA supervisory evidence; mandatory blameless PIR within 10 days. | ~~Automated drill runner CLI (`scripts/ops/dr_drill_runner.sh`) and evidence collector unwritten.~~ **Resolved 2026-10-01:** runner implemented + evidence collector (`results.jsonl` per-component RPO/RTO rows, §4 report generator). First full-catalog run committed at `docs/incidents/drills/2026-Q4-drill.md`: D2/D5/D6/D7 PASS (PG promote 454ms, Sentinel detect 1078ms/RPO 0, chaos 18/18 max recovery 1775ms, secrets rotation lifecycle clean); D1 region failover, D3 WAL archive replay, D4 ClickHouse restore reported SKIP-pending-infra (no second region / S3 WAL bucket / clickhouse-backup). |
| **Chaos Validation Suite (6 Scenarios)** | §18.1, §18.4, §18.5 | Ph-04.5: 4.5.3.1, 4.5.3.2 | #303 (Phase-04.5 AC #1–10) | Scaffolding in `tests/chaos/` | `SPLIT_BRAIN_DETECTED` (500, L0 - self-fence `SIGTERM`)<br>`SERIALIZATION_FAILURE` (40001 / 409, L2) | 0% | 6 chaos scenarios (Crash mid-batch, Timeout-safe requeue, Stale-snapshot guard, WAL trimmed fallback, PID conflict fail-closed, Warm recovery); 18 runs total; zero dup/missing trades; recovery < 10s. | Chaos test runner in `tests/chaos/scenarios/` and ChaosMesh fault injection manifests unwritten. |
| **Five-Tier Circuit Breakers** | §2.4, §13.2, §13.11 | Ph-13: 13.3.1, 13.3.9 | #63, #313 (Phase-13 AC #1–11, #39) | Redis keys `circuit_breaker:{scope}:{id}`<br>`010_create_admin_audit_log` | `CIRCUIT_BREAKER_TRIGGERED` (429/403/503, L1/L2)<br>`MARKET_WIDE_HALT` (503, L1)<br>`INSTRUMENT_HALTED` (422, L2) | 0% | 5 scopes (INSTRUMENT 5%/60s, ACCOUNT 3+ liquidations/5%/5min, VOLUME_SPIKE z≥4.0σ, OPTIONS_VOLATILITY IV>200%, MARKET_WIDE >20%); state machine CLOSED → OPEN → HALF_OPEN → CLOSED; dual-control reset. | Go service in `services/internal/circuitbreaker/`, Redis state handlers, and Prometheus metric exporters unwritten. |
| **Graceful Shutdown Protocols** | §2.7, §8.6, §19.7 | Ph-09: 9.3.23<br>Ph-05: 5.3.39<br>Ph-06: 6.3.7 | #182, #289 (Phase-09 AC #43; Phase-05 AC #57) | Systemd unit templates, Kubernetes lifecycle hooks | `SERVICE_SHUTTING_DOWN` (503, L1/L3)<br>`CONNECTION_DRAINING` (WS 1001, L3) | 0% | POSIX signal handling (`SIGTERM`/`SIGINT`), immediate readiness probe drop, 15s in-flight transaction drain deadline, WebSocket `server.shutdown` advisory frame. | Common Go package `pkg/lifecycle/shutdown.go` and Kubernetes preStop hook scripts unwritten. |
| **DORA ICT Resilience Framework** | §19.5, §19.5.1–§19.5.5 | Ph-09: 9.3.15 | #176 (Phase-09 AC #34) | `091_fleet_and_ops_console` | `DORA_INCIDENT_REPORT_REQUIRED` (L1/P0 SLA notification)<br>`ICT_PROVIDER_UNAVAILABLE` (503, L1) | 0% | Complete mapping to DORA pillars: ICT risk management, major incident reporting (<4h, <72h, <1m), annual TLPT testing, Register of Information on third-party ICT providers. | Incident reporting automation and third-party provider management console unwritten. |
| **Edge WAF / Anti-DDoS** | §8.4, §19.4 | Ph-09: 9.3.13<br>Ph-05: 5.3.29, 5.3.34 | #170, #270 (Phase-09 AC #32; Phase-05 AC #52) | Redis keys `ip_ban:{ip}` | `IP_BANNED` (418, L3)<br>`RATE_LIMIT_TIER_EXCEEDED` (429, L3)<br>`WAF_BLOCKED` (403, L3)<br>`DDOS_MITIGATION_ACTIVE` (503, L1) | 0% | BGP Anycast scrubbing (Magic Transit / Shield) + HAProxy L7 rate limiting; progressive IP ban ladder: 100 req/s → 429, sustained abuse → 1h IP ban (418), exploit injection → 24h ban. | `haproxy.cfg` stick-tables/Lua scripts and Redis ban synchronization worker unwritten. |
| **Secrets Management (Vault/KMS)** | §19.6, §20.2 | Ph-13.5: 13.5.3.5, 13.5.3.6<br>Ph-09: 9.3.29 | #177 (Phase-13.5 AC #26–27; Phase-09 AC #49) | `089_secrets_inventory` | `SECRET_EXPIRED` (500, L1)<br>`VAULT_UNREACHABLE` (500, L0/L1)<br>`INVALID_CREDENTIALS` (401, L3) | 0% | Vault agent on bare metal writing to `tmpfs` RAM-disks with `inotify` reload; Aeron shared memory Unix permissions (0660); zero-downtime 90-day rotation; 24h JWT `kid` overlap; dynamic DB credentials. | Ansible Vault roles (`deploy/ansible/roles/vault-agent/`) and inotify C++ reload watcher unwritten. |

---

### 6.2 Domain 11: Engineering & Delivery
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Automated Mechanical Verification Before Merge. Zero untraced §24 criteria or missing checkpoints.

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **CI/CD Pipeline Automation** | §19.1, §19.2 | Ph-01.5: 1.5.3.1, 1.5.3.3 | #31 (Phase-01.5 AC #1–5, #11–15) | Dynamic migrations 001–108 | `CI_BUILD_FAILED`<br>`LINT_FAILED`<br>`SHARD_TIMEOUT` (Pipeline Gates) | 0% | 4-shard parallel test pipeline over ephemeral Docker Compose (PostgreSQL 16, Redis 7, ClickHouse); hard timeouts: <5m C++ build/lint, <3m Go unit tests, <20m total. | GitHub Actions YAML files (`.github/workflows/ci.yml`) and shard runner scripts (`scripts/ci/shard_runner.sh`) unwritten. |
| **Spec Validation Harness** | §24 | Ph-01.5: 1.5.3.2, 1.5.3.5<br>Ph-08: 8.3.1 | #32, #71, #298 (Phase-01.5 AC #6–10, #21–25, #29; Phase-08 AC #1–5) | Test harness code & fixtures | `SPEC_CHECKPOINT_FAILED`<br>`CRITERIA_UNMAPPED`<br>`TRACEABILITY_DEFICIT` | 0% | Automated regex parser design extracting 543 checkpoints across 479 tasks; full traceability matrix mapping all 414 §24 criteria to executable test suites. | Go validator engine `tests/spec/validator.go` and checkpoint assertion scripts unwritten. |
| **Engine Soak & Benchmark (72h / 50k TPS)** | §3.1, §3.3, §24 | Ph-02.5: 2.5.3.1, 2.5.3.2, 2.5.3.3 | #33, #299 (Phase-02.5 AC #1–12) | Telemetry schemas | `SOAK_LATENCY_BREACH` (L1)<br>`MEMORY_LEAK_DETECTED` (L1)<br>`WAL_LAG_EXCEEDED` (L1) | 0% | 72h continuous load test at 50,000 orders/sec sustained per shard; p99 ≤ 50µs tick-to-trade, zero RSS memory growth, zero WAL lag, crash recovery every 4h in < 10s. | Load generator `tests/soak/loadgen.go` and continuous monitor `tests/soak/monitor.sh` unwritten. |
| **Pre-Prod Load Test (75k TPS / 10k WS)** | §3.1, §10.1, §24 | Ph-08: 8.3.1–8.3.5<br>Ph-08.5: 8.5.3.1, 8.5.3.3 | #71–76, #307 (Phase-08 AC #1–20; Phase-08.5 AC #1–10) | Staging test accounts & instruments | `LOAD_SHEDDING_ACTIVE` (503, L1)<br>`WS_FANOUT_OVERFLOW` (500, L1) | 0% | 75,000 orders/sec sustained for 4h on staging; 10,000 concurrent active WebSocket connections with zero dropped frames; PG replica lag < 2s; mid-run recovery < 10s. | Distributed load injection scripts (k6 / Aeron injectors) unwritten. |
| **Blue-Green Zero-Downtime Deploy** | §19.2 | Ph-09: 9.3.3, 9.3.26 | #47, #317 (Phase-09 AC #9–12, #46) | Backward-compatible migrations | `CANARY_ROLLBACK_TRIGGERED` (L1)<br>`TRAFFIC_SHIFT_FAILED` (L1) | 0% | Kubernetes + HAProxy staged traffic shifting: 0% → 10% → 50% → 100% over 30m; automated rollback on error rate > 0.1% or latency p99 > 100ms. | Argo Rollouts / Flagger manifests and HAProxy weight shifting automation unwritten. |
| **Capacity Planning Models** | §19.10 | Ph-09: 9.3.19 | #180 (Phase-09 AC #38) | Mathematical models in docs | `CAPACITY_LIMIT_WARNING` (P2)<br>`RESOURCE_EXHAUSTION_ALERT` (P1) | 0% | Mathematical M/M/1 queuing models, IOPS sizing formulas, Aeron buffer sizing, and 3x peak headroom reserves specified in `docs/operations/capacity-model.md`. | Automated capacity projection Prometheus exporter / calculation scripts unwritten. |
| **Hot/Warm/Cold Data Tiering** | §16.1, §19.12 | Ph-09: 9.3.17, 9.3.22, 9.3.24 | #169, #214, #241 (Phase-09 AC #36, #42, #44; Phase-04 AC #24) | PG table partitions, ClickHouse MergeTree TTLs | `ARCHIVE_JOB_FAILED` (P2)<br>`RETENTION_POLICY_VIOLATION` (P1) | 0% | 3-tier lifecycle: Hot PG 16 (30–90 days), Warm ClickHouse (5 years), Cold S3 Parquet (7 years) with Presto/Trino query layer. | Partition management bash scripts (`scripts/db/archive_partitions.sh`) and Presto catalog configs unwritten. |
| **Supply-Chain Security Scanning** | §20.3 | Ph-01.5: 1.5.3.4 | #161 (Phase-01.5 AC #26–28) | CI/CD security policies | `VULNERABILITY_CRITICAL_DETECTED`<br>`CREDENTIAL_LEAK_DETECTED` | 0% | CodeQL SAST, `govulncheck`, `npm audit`, pinned C++ FetchContent hashes, Trivy container scanning (HIGH/CRITICAL fails), gitleaks, 7-day dep cooldown. | Workflow `.github/workflows/security.yml` and `.trivyignore` unwritten. |

---

### 6.3 Domain 12: Client Experience
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Institutional Low-Latency Interactivity & Non-Destructive Credentials.

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **Trader UI & Widgets** | §10.1–§10.6 | Ph-10: 10.3.1–10.3.29 | #51–58, #261–269, #310 (Phase-10 AC #1–31) | `076_trader_workspace_preferences` | `WS_CONNECTION_LOST`<br>`FEED_STALE`<br>`ORDER_REJECTED` (RFC 7807 toast, L3) | 0% | React 18 + TS strict mode, Vite, Tailwind CSS; bundle budget ≤ 300 kB; WS reconnect backoff; L2 book, depth chart, calculator, percentage sliders, 5-light ADL indicator, TradingView charts. | Directory `frontend/` contains zero code; no Vite setup, Zustand stores, or TradingView components implemented. |
| **Admin Dashboard & Operations** | §8.2, §8.5, §15.1–§15.6 | Ph-07: 7.3.1–7.3.10<br>Ph-10: 10.3.6, 10.3.20 | #43–46, #144–147, #242 (Phase-07 AC #1–28; Phase-10 AC #6) | `010_create_admin_audit_log`<br>`048_support_tickets`<br>`090_admin_rbac`<br>`091_fleet_and_ops_console` | `UNAUTHORIZED_ADMIN_ACTION` (403, L3)<br>`DUAL_CONTROL_REQUIRED` (400, L2) | 0% | 6 RBAC roles formalized; UI layouts for LP scorecard, circuit breaker trip/reset with dual-control (4-eyes), trade bust dialogs, and shard health grids. | Admin frontend pages and Go admin API handlers unwritten. |
| **User Authentication (WebAuthn/Passkeys)** | §8.1, §12.1–§12.3, §12.6 | Ph-12: 12.3.1, 12.3.2, 12.3.7, 12.3.12 | #59, #60, #270, #312 (Phase-12 AC #1–7, #27, #30) | `002_create_users`<br>`027_alter_users_add_password_hash`<br>`068_webauthn_credentials` | `INVALID_CREDENTIALS` (401, L3)<br>`TWO_FACTOR_REQUIRED` (403, L3)<br>`ACCOUNT_LOCKED` (423, L3)<br>`PASSKEY_CLONE_DETECTED` (403, L3) | 0% | Non-destructive 2FA re-enrollment with 10m candidate secret staging; WebAuthn assertion ceremony elevating session with `amr: ["fido2"]`; 5-attempt brute-force lockout; passkey counter regression clone detection. | Auth service in `services/internal/auth/` and WebAuthn browser API integration unwritten. |
| **Anti-Phishing Security Code** | §8.1, §12.2 | Ph-12: 12.3.8<br>Ph-10: 10.3.22 | #271 (Phase-12 AC #28; Phase-10 AC #23) | `002_create_users` (field `anti_phishing_code`) | `INVALID_ANTI_PHISHING_CODE` (400, L3) | 0% | User-configured anti-phishing phrase stamped into headers of all transactional emails, SMS, and critical system alerts to prevent spoofing. | Mailer templating engine and user security settings endpoints unwritten. |
| **KYC Submission Portal** | §8.1, §12.4, §14.1 | Ph-12: 12.3.4<br>Ph-14: 14.3.4<br>Ph-10: 10.3.24 | #61, #131 (Phase-12 AC #10–13; Phase-14 AC #10–15; Phase-10 AC #25) | `017_create_kyc_documents`<br>`042_client_categorization` | `KYC_TIER_REQUIRED` (403, L3)<br>`DOCUMENT_UPLOAD_FAILED` (500, L3)<br>`KYC_EXPIRED` (403, L3) | 0% | 3-tier KYC workflow (T0 read-only, T1 $10k/day, T2 $100k/day, Institutional); encrypted S3 document storage; Sumsub/Onfido webhooks; periodic re-verification. | S3 pre-signed upload URL generator and vendor webhook receivers unwritten. |
| **Webhooks & Notifications** | §8.5, §12.5 | Ph-12: 12.3.5, 12.3.6<br>Ph-14: 14.3.12 | #62, #315 (Phase-12 AC #14–22; Phase-14 AC #38) | `028_create_notification_dead_letters` | `WEBHOOK_DELIVERY_FAILED` (500, L2/L3)<br>`NOTIFICATION_RATE_EXCEEDED` (429, L3) | 0% | Multi-channel dispatch (SendGrid/SES email, Twilio SMS, signed webhooks); 5 exponential backoff retries (1s to 30m); failed dispatches routed to dead-letter table. | Notification worker daemon `services/internal/notification/` unwritten. |
| **Copy Trading Layer** | §11.3 | Ph-14: 14.3.14<br>Ph-10: 10.3.26 | #191, #374 (Phase-14 AC #34, #40; Phase-10 AC #27) | `097_copy_trading_product` | `COPY_TRADING_SLIPPAGE_EXCEEDED` (422, L2)<br>`LEAD_TRADER_MAX_FOLLOWERS` (409, L3)<br>`DRAWDOWN_SAFETY_STOP_TRIGGERED` (422, L2) | 0% | Lead trader trade replication scaled to follower equity; 1.5-pip max slippage collar; maximum drawdown safety stop; high-water mark profit-share calculation. | Copy trading execution engine in `services/internal/copytrading/` unwritten. |
| **PAMM / MAM Investment Modules** | §11.2, Remediations #38 F6 | Ph-14: 14.3.8 | #190 (Phase-14 AC #33) | `003_create_accounts`<br>`004_create_balances`<br>`096_cent_subunit_ledger` | `PAMM_ALLOCATION_MISMATCH` (500, L1/L2)<br>`PAMM_INVESTOR_LOCKED` (403, L3)<br>`PAMM_MIN_INVESTMENT_NOT_MET` (400, L3) | 0% | Investment sub-ledger segregation from fiat banking allowances (F6); pro-rata allocation to investor pools; high-water mark performance fee calculation. | PAMM allocation engine and pool rebalancing workers unwritten. |

---

### 6.4 Domain 13: Governance & Business
**Domain Spec Score:** 100% | **Plan Coverage:** 100% | **Code Implementation:** 0.0%  
**Primary Invariant:** Institutional Trust, Segregated Client Assets, Regulatory Auditability.

| Component | Spec Ref | Phase Tasks | §24 Criteria | DB Migrations | Error Codes & Tiers | Code % | Done (Architecture / Spec / Design) | Not Done (Implementation / Execution) |
|---|---|---|---|---|---|---|---|---|
| **Treasury & House Funds** | §5.21, §16.5, §17.1, §17.13.1 | Ph-20: 20.3.7<br>Ph-24: 24.3.17 | #205, #329 (Phase-20 AC #23–24; Phase-24 AC #44) | `082_treasury` (`own_funds_balances`, `contingent_capital_commitments`) | `TRIAL_BALANCE_UNBALANCED` (500, L1)<br>`TREASURY_LIQUIDITY_BREACH` (L1 alert) | 0% | Daily trial balance per currency asserting debits==credits and Assets==Liabilities+Equity; nightly ERP batch export with SHA-256 manifest; 5-day house liquidity buffer. | Finance service `services/internal/analytics/trial_balance.go` and ERP SFTP exporter unwritten. |
| **Client-Money Safeguarding (CASS 7)** | §5.33, §17.9, §17.13.2 | Ph-24: 24.3.11, 24.3.16, 24.3.18 | #173, #326, #330 (Phase-24 AC #34, #43, #45) | `056_client_money`<br>`083_client_money_assurance` | `CLIENT_MONEY_SHORTFALL` (503, L1)<br>`SEGREGATION_BREACH` (500, L0/L1)<br>`TRUST_LETTER_MISSING` (400, L1) | 0% | Legal/operational segregation in trust bank accounts with acknowledgement letters; daily internal/external reconciliation; 4-tier shortfall waterfall (1h SLA); auditor pack generator. | Client money calculation engine in `services/internal/backoffice/client_money.go` unwritten. |
| **Insurance Fund Governance** | §13.5, §17.13.1 | Ph-19: 19.3.9, 19.3.14<br>Ph-24: 24.3.17 | #151, #178, #329 (Phase-19 AC #40, #58; Phase-24 AC #44) | `016_create_insurance_fund`<br>`082_treasury` | `INSURANCE_FUND_DEPLETED` (500, L1)<br>`NBP_RESERVE_DEFICIT` (500, L1) | 0% | 0.5% OI calibration against historical stress loss; replenishment waterfall via penalty fees, 0.05% LP rebates paid from fund, retained earnings, and contingent capital facility. | Automated insurance fund ledger worker in `services/internal/risk/insurance.go` unwritten. |
| **Internal Cryptographic Audit Trails** | §14.11, §17.11 | Ph-01: 1.3.8<br>Ph-13: 13.3.7<br>Ph-21: 21.3.27 | #18, #186, #345 (Phase-01 AC #8; Phase-13 AC #37; Phase-21 AC #63) | `009_create_audit_hash_chain`<br>`010_create_admin_audit_log`<br>`021_create_audit_merkle_roots` | `AUDIT_CHAIN_BROKEN` (500, L0 - immediate security trip)<br>`MERKLE_ROOT_MISMATCH` (500, L1) | 0% | Append-only WORM SHA-256 hash chaining ($H_n = \text{SHA256}(H_{n-1} \parallel \text{payload}_n)$); daily Proof of Reserves Merkle tree at 22:00 UTC with GPG-signed root and inclusion proofs. | Audit chain middleware and Merkle generation scheduler in `services/internal/audit/` unwritten. |
| **Employee Dealing & Insider Controls** | §14.8, §14.10.1 | Ph-21: 21.3.20, 21.3.24 | #203, #327 (Phase-21 AC #59, #67) | `062_comms_recordings`<br>`079_employee_dealing` | `EMPLOYEE_TRADE_UNAUTHORIZED` (403, L3)<br>`INSTRUMENT_RESTRICTED` (403, L3)<br>`MINIMUM_HOLDING_VIOLATION` (403, L3) | 0% | Employee trade pre-clearance with 24h window; restricted lists; 30-day min holding period; broker confirmation matching; MiFID II Art. 16(7) taping of communications with 5–7 yr retention. | Compliance approval portal and communications capture pipelines unwritten. |
| **Regulatory Change Horizon Scanning** | §14.10.2 | Ph-21: 21.3.25 | #328 (Phase-21 AC #68) | `080_regulatory_change` | `REGULATORY_DEADLINE_APPROACHING` (P2)<br>`RULEBOOK_VERSION_STALE` (P1) | 0% | Compliance horizon-scanning pipeline covering ESMA, FCA, CFTC, and ASIC updates; impact assessment, rulebook versioning, and compliance sign-off workflow. | Regulatory change portal and rulebook version manager unwritten. |
| **Regulated Venue License Governance** | §14.10, §14.14 | Ph-21: 21.3.15, 21.3.24, 21.3.29 | #174, #378 (Phase-21 AC #54, #70) | `101_governance_packs` | `JURISDICTION_UNLICENSED` (403, L3)<br>`ANNUAL_ATTESTATION_OVERDUE` (P1) | 0% | Boundary matrix: MiFID II MTF, FCA MTF, CFTC SEF, ASIC; annual algorithmic trading self-assessment per RTS 6; automated governance pack publisher. | Governance pack compiler script and jurisdictional blocking filters unwritten. |
| **Incident P0–P3 Escalation Matrix** | §19.8, §19.11 | Ph-09: 9.3.18, 9.3.25, 9.3.27 | #183 (Phase-09 AC #37, #45, #47) | `010_create_admin_audit_log` | Severity levels P0/P1/P2/P3 mapped to L0/L1/L2/L3 system failure hierarchy | 0% | SLA matrix: P0 <15m, P1 <30m, P2 <2h, P3 <8h; on-call paging; 48-hour blameless RCA requirement; public status page automation; BCP regulator notification tree. | PagerDuty routing automation and status page API integrations unwritten. |
| **Third-Party ICT Risk (DORA Art. 28)** | §19.5.3 | Ph-09: 9.3.15 | #176 (Phase-09 AC #34) | `091_fleet_and_ops_console` | `VENDOR_CONCENTRATION_BREACH` (P2)<br>`ICT_SUPPLIER_AUDIT_EXPIRED` (P2) | 0% | Register of Information per ESA technical standards; concentration risk assessment for cloud, market data, and banking rails; mandatory exit strategies. | Vendor audit database and contract renewal review scheduler unwritten. |
| **Client Appropriateness (MiFID II)** | §12.4 | Ph-14: 14.3.7, 14.3.16 | #149, #376 (Phase-14 AC #24–25, #42) | `042_client_categorization`<br>`099_product_target_markets` | `APPROPRIATENESS_ASSESSMENT_FAILED` (403, L3)<br>`NEGATIVE_TARGET_MARKET_RESTRICTION` (403, L3) | 0% | Knowledge and experience questionnaire scoring; risk warning delivery; retail leverage capping (30:1 major / 20:1 minor / 10:1 exotic); negative target market distribution blocking. | Frontend questionnaire form and automated grading evaluation service unwritten. |
| **Complaints & Dispute Resolution (ADR)** | §8.5, §14.15 | Ph-07: 7.3.7<br>Ph-21: 21.3.26, 21.3.28 | #147, #333 (Phase-07 AC #23–25; Phase-21 AC #69) | `048_support_tickets`<br>`081_vdp_and_promotions` | `COMPLAINT_SLA_BREACH` (P2)<br>`TICKET_NOT_FOUND` (404, L3) | 0% | Statutory complaints workflow: 48h acknowledgment, 8-week final response deadline; escalation to Compliance Officer; automated evidence compilation for external ADR (FOS / AFCA). | Support ticket backend endpoints and ADR evidence export tools unwritten. |


---

## 7. Global Synthesis, Component Coverage Metrics & Execution Roadmap

### 7.1 Cross-Domain Aggregate Completeness Metrics

| Metric Dimension | Total Tracked | Fully Specified & Planned | Production Implemented | Spec/Plan Coverage % | Code Coverage % |
|---|---|---|---|---|---|
| **Core Operational Domains** | 13 Domains | 13 Domains | 0 Domains | **100.0%** | **0.0%** |
| **Evaluated Subcomponents** | 133 Components | 133 Components | 0 Components | **100.0%** | **0.0%** |
| **§24 Acceptance Criteria** | 414 Criteria | 414 Criteria | 0 Criteria | **100.0%** | **0.0%** |
| **Checklist Spec Checkpoints** | 543 Checkpoints | 543 Checkpoints | 0 Checkpoints | **100.0%** | **0.0%** |
| **Phase Implementation Tasks** | 479 Tasks | 479 Tasks | 0 Tasks | **100.0%** | **0.0%** |
| **Phase AC Rows** | 1,079 Rows | 1,079 Rows | 0 Rows | **100.0%** | **0.0%** |
| **Spec §23 Error Codes** | 149 Codes | 149 Codes | 0 Codes | **100.0%** | **0.0%** |
| **Database Migrations** | 108 Migrations | 108 Migrations | 0 Migrations | **100.0%** | **0.0%** |
| **Degradation Modes** | 6 Modes | 6 Modes | 0 Modes | **100.0%** | **0.0%** |
| **Reconciliation Categories** | 9 Categories | 9 Categories | 0 Categories | **100.0%** | **0.0%** |

---

### 7.2 Component Completeness & Coverage Breakdown by Domain

```
DOMAIN COMPLETENESS BREAKDOWN (SPECIFICATION & PLANNING vs PRODUCTION CODE):

1. Matching & Execution Core   [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
2. Instrument & Market Admin   [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
3. Order Types                 [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
4. Pricing & Liquidity Infra   [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
5. APIs & Connectivity         [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
6. Market Data Products        [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
7. Risk & Credit               [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
8. Compliance & AML            [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
9. Trade Lifecycle Ops         [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
10. Recovery & Resilience      [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
11. Engineering & Delivery     [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
12. Client Experience          [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
13. Governance & Business      [SPEC: 100%] [CODE: 0%]  ████████████████████░░░░░░░░░░
```

#### Detailed Domain Metrics:

1. **Matching & Execution Core (Domain 1):**
   - **Components:** 12 components (LOB, deterministic matching, queue, priority, cross-shard 2PC, atomic amend, mass cancel, STP, execution rules, slippage protection, auction, OTR limits).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 46 §24 criteria; 30 phase tasks; 13 database migrations; 4 severity tiers represented.

2. **Instrument & Market Admin (Domain 2):**
   - **Components:** 6 components (7-state lifecycle, maker-checker admin, 24/5 trading hours, ISDA holiday calendar, price collars, min notional & quantization).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 28 §24 criteria; 12 phase tasks; 7 database migrations; 15-minute obvious error trade bust window.

3. **Order Types (Domain 3):**
   - **Components:** 14 components (Limit, Market, Stop, TP, Trailing Stop, Bracket/OCO, Iceberg, TIF/GTD/GTC, Pegged, TWAP/VWAP/VP, Net-proceeds OPO/OPOCO, Grid bot, Dual-price trigger, GSLO).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 36 §24 criteria; 22 phase tasks; 9 database migrations; 5 max concurrent grid bots.

4. **Pricing & Liquidity Infrastructure (Domain 4):**
   - **Components:** 8 components (Mark price, index oracle, 5s staleness gates, yield curves, MTF/fair value CIP, LP scorecard, ADL waterfall, MMP market maker protection).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 29 §24 criteria; 18 phase tasks; 7 database migrations; 6 LP scorecard evaluation dimensions.

5. **APIs & Connectivity (Domain 5):**
   - **Components:** 10 components (REST OpenAPI 3.0, WS public + interactive trading, SBE A/B multicast, FIX 4.4/5.0 SP2 + FIXS mTLS, Mass Quoting 35=i, Allocation 35=J/AK, RFC 8594 versioning/deprecation, RFC 6585 rate tiers + progressive IP ban, Dead-Man switch / CoD, Smart Order Routing SOR).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 48 §24 criteria; 46 gateway tasks; 18 database migrations; 100 active WS streams per connection cap.

6. **Market Data Products (Domain 6):**
   - **Components:** 9 components (L2 book snapshot+delta, L3 order-level real-time stream, klines 13 timeframes, ClickHouse tick history, Book ticker BBO zero-latency stream, Liquidation feed with 2s anti-front-running delay, WS subscriptions trie, Multi-window multi-burn-rate SLA, Block tape).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 24 §24 criteria; 21 phase tasks; ClickHouse MergeTree tables; p99 $\le 50\mu\text{s}$ C++ matching latency.

7. **Risk & Credit (Domain 7):**
   - **Components:** 10 components (Cross/Isolated/Portfolio margin with 90-day correlation matrix, ESMA/CFTC leverage tiers, 2s liquidation scanner & auction ladder, Retail Negative Balance Protection, Insurance Fund & waterfall governance, Monte Carlo stress testing, PB credit NOP/DSL limits, Collateral haircuts & concentration, RTS 9 OTR collars, Bilateral mutual credit screening).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 42 §24 criteria; 28 phase tasks; 14 database migrations; 20% regulatory portfolio margin floor.

8. **Compliance & AML (Domain 8):**
   - **Components:** 16 components (KYC T0/T1/T2/Inst, PEP & adverse media screening, C++ pre-trade SanctionsHook, Travel Rule IVMS 101, SAR/CTR FinCEN Form 107 XML, MiFID II RTS 6/22/25/27/28, EMIR REFIT & CFTC Parts 43/45 trade repository reporting, Dodd-Frank swap reporting, FinCEN MSB, GDPR data sovereignty & 7y carve-out, 7 market-abuse surveillance patterns + case management, WORM communications taping, CRS/FATCA tax reporting, Basel III capital adequacy, FX Global Code 55 principles, Data residency enforcer).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 55 §24 criteria; 34 phase tasks; 18 database migrations; 100µs UTC PTP clock sync requirement.

9. **Trade Lifecycle Ops (Domain 9):**
   - **Components:** 12 components (T+1/T+2/same-day settlement, Banking rails SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2, Third-party CLS ISO 20022 PvP, Nostro/vostro 3-way reconciliation with MT940/camt.053, Zero-GL-bypass double-entry General Ledger, Tom-Next spot rollover & swap points engine, Trade confirmations MT515, TCA RTS 28 engine, Bunched order allocation FIX 35=J/AK, CSDR Article 7 settlement discipline, Internal position transfers, Obvious error trade busts).
   - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
   - **Metrics:** 44 §24 criteria; 31 phase tasks; 22 database migrations; $\sum\text{Debit} == \sum\text{Credit}$ mathematical invariant.

10. **Recovery & Resilience (Domain 10):**
    - **Components:** 10 components (Custom binary WAL & 3-stage graduated recovery ladder, PostgreSQL reconciliation across 9 categories, Multi-region DR with strict RPO/RTO targets, Quarterly DR drill program, Chaos validation suite with 6 scenarios, Five-tier circuit breakers, Graceful shutdown protocols, DORA ICT resilience framework, Edge WAF / anti-DDoS with progressive IP ban, Secrets management via Vault/KMS).
    - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
    - **Metrics:** 35 §24 criteria; 25 phase tasks; 10 database migrations; Redis RPO $\le 5\text{s}$ / RTO $\le 30\text{s}$, PG RPO $\le 15\text{s}$ / RTO $\le 5\text{min}$.

11. **Engineering & Delivery (Domain 11):**
    - **Components:** 8 components (CI/CD pipeline with 4-shard parallel test execution, Spec validation regex harness, Engine soak & benchmark 72h at 50k TPS, Pre-production load test 75k TPS & 10,000 WS connections, Blue-green zero-downtime deployment, Capacity planning queuing models, Hot/warm/cold data tiering, Supply-chain security scanning).
    - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
    - **Metrics:** 22 §24 criteria; 18 phase tasks; 543 checklist checkpoints across 479 tasks.

12. **Client Experience (Domain 12):**
    - **Components:** 8 components (React 18 + TS Trader UI with Pro/Lite modes and TradingView charts, Admin operations dashboard, User authentication with WebAuthn/Passkeys and non-destructive TOTP, Anti-phishing security phrase, KYC submission portal, Multi-channel webhooks & notifications with dead-letter queue, Copy trading engine with 1.5-pip slippage collar, PAMM/MAM investment modules with segregated sub-ledgers).
    - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
    - **Metrics:** 32 §24 criteria; 35 phase tasks; 12 database migrations; bundle budget $\le 300\text{ kB}$.

13. **Governance & Business (Domain 13):**
    - **Components:** 11 components (Treasury & own-funds segregation, Client-money safeguarding CASS 7 trust accounts with 4-tier waterfall, Insurance fund replenishment, Append-only SHA-256 hash chaining & daily Merkle roots, Employee dealing controls & restricted lists, Regulatory change horizon scanning, Regulated venue license governance, Incident P0–P3 escalation matrix, DORA third-party ICT risk, Client appropriateness assessment, Customer complaints & ADR dispute resolution).
    - **Spec Coverage:** **100%** | **Plan Coverage:** **100%** | **Code Implementation:** **0.0%**.
    - **Metrics:** 21 §24 criteria; 22 phase tasks; 11 database migrations; 48-hour post-incident RCA requirement.

---

### 7.3 Systemic Gap Analysis: What Has Been Done vs What Has Not Been Done

#### What Has Been Done (100% Architecture & Design Completed)
1. **Mathematical & Algorithmic Rigor:**
   - Complete fixed-point arithmetic model with $10^8$ pipette scaling eliminating compiler non-determinism.
   - Deterministic matching core decoupled from wall-clock time via discrete `TIME_TICK` IPC events.
   - Closed-form formulas for portfolio margin offsets ($|\rho| > 0.7$), Covered Interest Parity (CIP) fair value, Tom-Next swap points, Black-76 Greeks, and ADL counterparty ranking.
2. **Regulatory & Institutional Compliance:**
   - MiFID II (RTS 6, RTS 22, RTS 25, RTS 27/28), EMIR REFIT, CFTC Parts 43/45, Dodd-Frank, FinCEN Form 107, Travel Rule IVMS 101, and DORA ICT resilience.
   - Complete 55-principle assessment against the FX Global Code with zero-last-look firm quote guarantees.
3. **Data Model & Schema Engineering:**
   - 108 contiguous, collision-free PostgreSQL migrations (`001_` to `108_`) fully specified with primary keys, indexes, foreign keys, and constraints.
   - Columnar ClickHouse MergeTree schemas designed for 50,000 inserts/sec, 90-day raw TTL, and 12 pre-materialized kline intervals.
4. **Resilience & Invariant Architecture:**
   - 149 registered §23 error codes, 100% mapped to owning phase tasks.
   - 4-tier error severity hierarchy (L0 Critical Core Halt, L1 Systemic ModeManager, L2 Transaction Rejection, L3 Edge Ban) governed by **Strict Fail-Closed Zero-Loss Pessimism**.
   - 3-stage graduated WAL recovery ladder and hourly reconciliation across 9 financial correctness categories.

#### What Has Not Been Done (0% Production Code Implemented)
1. **Software Source Code Implementation:**
   - **C++ Matching Engine (`engine/`):** CMake configuration, flat-array `OrderBook.cpp`, NUMA pinning, `MatchingEngine.cpp`, Aeron C media driver subscriber, binary WAL writer with `O_DIRECT` block flushing, and in-process `SanctionsHook.cpp` are completely unwritten.
   - **Go Microservices (`services/`):** No Go code written. `order-gateway`, `market-data`, `settlement`, `risk-engine`, `compliance`, `fix-gateway` (QuickFIX-Go), and `backoffice` modules need initial `go.mod` scaffolding and implementation.
   - **Frontend UI (`frontend/`):** No React 18, TypeScript, Tailwind, or TradingView Lightweight Charts code written.
2. **Infrastructure & Platform Deployment:**
   - No Dockerfiles, Docker Compose test rigs, Kubernetes manifests, Helm charts, Terraform regional definitions, or Ansible playbooks created on disk.
   - PostgreSQL 16 and ClickHouse databases not initialized; migrations `001_`–`108_` unapplied.
   - Redis Sentinel clusters and Aeron shared-memory ring buffers unconfigured.
3. **External Connectivity & Third-Party Adapters:**
   - Bank rails (SWIFT Alliance, SEPA SCT, FedNow), CLS ISO 20022 XML adapters, ARM/TR reporting endpoints (UnaVista, DTCC), KYC vendors (Sumsub, Onfido), and price oracle feeds (Refinitiv, Bloomberg BFIX, ECB) exist only as interface specifications.
4. **Deliberately Deferred / Out-of-Scope Items (Documented in Spec §27):**
   - Native mobile applications (iOS/Android) deferred to v2.
   - Referral and affiliate commission programs deferred.
   - Non-cash collateral (Treasury bills, gold) deferred; fiat cash only.
   - Retail Introducing Broker (IB) multi-tier commissions deferred.
   - RFQ, RFS, indicative quotes, and last-look liquidity models strictly excluded per FX Global Code Principle 17.

---

### 7.4 Immediate Actionable Execution Roadmap (Phase 01 Kickoff)

With 100% of specifications, architecture, and phase implementation plans verified and zero-drift confirmed, the repository is ready for Day 0 implementation:

```
┌────────────────────────────────────────────────────────────────────────┐
│                        PHASE 01 IMPLEMENTATION                         │
│                           (Days 1 to 14)                               │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │
       ┌────────────────────────────┼────────────────────────────┐
       ▼                            ▼                            ▼
┌──────────────┐             ┌──────────────┐             ┌──────────────┐
│  Task 1.3.1  │             │  Task 1.3.5  │             │  Task 1.3.3  │
│  C++ CMake   │             │  Aeron IPC   │             │  PostgreSQL  │
│  Scaffold    │             │  Ring Buffer │             │  Migrations  │
│  (C++20,     │             │  (/dev/shm   │             │  (001–021    │
│  sanitizers) │             │  16MB pool)  │             │  scaffold)   │
└──────┬───────┘             └──────┬───────┘             └──────┬───────┘
       │                            │                            │
       └────────────────────────────┼────────────────────────────┘
                                    │
                                    ▼
                             ┌──────────────┐
                             │  Task 1.3.6  │
                             │  Custom WAL  │
                             │  (O_DIRECT   │
                             │  4KB blocks) │
                             └──────┬───────┘
                                    │
                                    ▼
                             ┌──────────────┐
                             │  Phase 01.5  │
                             │  CI/CD Spec  │
                             │  Validation  │
                             │  Harness     │
                             └──────────────┘
```

1. **Step 1 — C++20 Core Foundation Scaffolding (Phase 01 Task 1.3.1):**
   - Create root `CMakeLists.txt` with C++20 standard, strict warnings (`-Wall -Wextra -Wpedantic -Werror`), and Address/UndefinedBehavior sanitizers.
   - Establish directory tree: `engine/src/core/`, `engine/src/book/`, `engine/src/wal/`, `engine/include/`.
   - Implement fixed-point pipette integer arithmetic primitives ($10^8$ scaling).
2. **Step 2 — Aeron Shared-Memory Driver Integration (Phase 01 Task 1.3.5, 1.3.10):**
   - Deploy embedded Aeron C media driver with `/dev/shm` IPC channel configuration.
   - Establish SPSC lock-free ring buffer ingress/egress queues.
3. **Step 3 — Custom Binary Write-Ahead Log (WAL) Engine (Phase 01 Task 1.3.6):**
   - Implement `WalWriter.cpp` utilizing `posix_memalign` 4KB block-aligned direct I/O (`O_DIRECT`).
   - Implement CRC32C packet checksumming and deterministic `TIME_TICK` event serialization.
4. **Step 4 — Database Migrations 001–021 & Go Module Initialization (Phase 01 Task 1.3.3, 1.3.7):**
   - Initialize root `go.mod` with Go 1.23+.
   - Implement migrations `001_create_instruments.up.sql` through `021_create_audit_merkle_roots.up.sql`.
   - Setup Docker Compose environment containing PostgreSQL 16, Redis 7, and ClickHouse for local testing.
5. **Step 5 — Phase 01.5 Automated CI/CD & Spec Validation Harness (Phase 01.5 Task 1.5.3.1, 1.5.3.2):**
   - Construct GitHub Actions pipeline validating all 543 checkpoints across 479 tasks against executable tests.
   - Enforce zero-tolerance gate: zero commits merged without 100% passing golden tests.


---

## 8. Audit Synchronization & Incremental Update Remarks

### 8.1 Synchronization Log with Master Spec & Plans
All findings, gap analyses, component metrics, filesystem layouts, and business boundary rulings documented in this assessment have been formally reflected and codified into the Master Specification and Phase Implementation Plans per Option A (full ingestion):

1. **Master Specification v7.0 §27 (Decision Log) Updated:**
   - **Remediation #39 Added:** Codifies the 13-Domain Architectural Matrix, the 133-component completeness mapping, the canonical filesystem directory conventions, and ratifies the 5 business boundary rulings (R14–R18).
   - **§27.1 Operational Domains & High-Level Completeness Matrix:** Fully embeds the 13-domain summary matrix.
   - **§27.2 Complete 133-Component Audit & Architectural Catalog:** Fully embeds the complete component tables for all 13 domains (Domains 1 through 13) with exact mappings to Spec refs, Phase tasks, §24 criteria, DB migrations, error codes, and implementation status.
   - **§27.3 Cross-Domain Completeness Scorecard & Invariant Verification:** Embeds the cross-domain aggregate completeness metrics table.
   - **§27.4 Systemic Implementation Gap Analysis:** Permanently codifies the comprehensive *What Has Been Done (100% Architecture & Design)* vs *What Has Not Been Done (0% Production Code)* synthesis.
2. **Business Boundary Rulings Formally Codified in Spec §27 & DESIGN.md §10:**
   - **R14:** Native mobile apps (iOS/Android) deferred to v2 (web-only responsive UI via React 18 in Phase 10).
   - **R15:** Multi-tier affiliate/referral programs excluded (institutional venue compliance).
   - **R16:** 100% fiat currency cash collateral only; non-cash collateral deferred to v2.
   - **R17:** Retail Introducing Broker (IB) volume rebate structures deferred; institutional PB give-ups and clearing prioritized.
   - **R18:** Firm liquidity CLOB model strictly defended per FX Global Code Principle 17; RFQ, RFS, indicative quotes, and last-look models prohibited.
3. **Phase-01 (Project Foundation) Plan Harmonization:**
   - **Section 1.1 Updated:** Embeds the Day-0 Codebase Layer Implementation Gap Inventory (unwritten C++, Go, React, SQL, and Infra) and the Tactical 5-Step Phase 01 Kickoff Implementation Sequence.
   - **Task 1.3.1 Updated:** Explicitly notes canonical directory conventions (`core/` in CMake tree is referenced symmetrically as `engine/` in architectural plans) and fixed-point pipette integer scaling ($10^8$ units) arithmetic primitives.
   - **Task 1.3.2 Updated:** Explicitly maps Go service binary entrypoints (`services/cmd/<service>`) to container deployment directories (`services/<service-name>/`).
4. **All Root Meta-Docs Synchronized:**
   - `MEMORY.md`, `CLAUDE.md`, `CONTEXT.md`, `ARCHITECTURE.md`, `DESIGN.md`, and `WORKFLOWS.md` synchronized with zero documentation drift.
5. **Zero Information Loss Attestation (Retirement Readiness):**
   - Because 100% of all 15 tables, 133 evaluated components, their owning phase tasks across the 30 phase plans, 414 §24 acceptance criteria, 108 database migrations, 149 registered error codes, and the 5 business boundary rulings have been permanently ingested into the Master Specification v7.0 (§27.1–§27.4) and Phase Implementation Plans, this document (`docs/PROJECT_COMPLETENESS_ASSESSMENT.md`) may be safely retired/deleted by the user at any time without loss of architectural memory.

*Full Option A synchronization verified clean on 2026-09-27.*
