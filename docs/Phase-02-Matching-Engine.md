# Phase 2 — Matching Engine Core (C++)

**Duration:** 14–22 days (supersedes 12–18 — duration itemization completed 2026-09-27, feature completeness audit #36: Task 2.3.22 trade-through protection added; prior supersedes 12–18 — duration itemization completed 2026-09-27, remediation #35: Tasks 2.3.14–2.3.18 added to §2.6; prior supersedes 12–17 — Task 2.3.20 added 2026-09-27, remediation #24)
**Dependencies:** Phase 1, Phase 1.5
**Spec Reference:** §3 (Matching Engine), §6 (Order Types), §2.4 (Degradation), §2.5 (Leader Election), §2.6 (Circuit Breaker)

---

## 2.1 Objectives

Implement the C++ matching engine: order book data structure, price-time priority matching, pre-trade risk checks, ICEBERG orders, binary WAL integration, leader election, degradation mode manager, health checker, cross-shard basket order 2PC, and the IPC layer for Go gateway communication. Target: 50k orders/sec sustained, p99 ≤ 50µs (supersedes prior ≤ 1ms) tick-to-trade.

---

## 2.2 Prerequisites

- Phase 1 complete (C++ scaffold, WAL, IPC, PostgreSQL, Redis, shard map)
- Phase 1.5 complete (CI pipeline, spec validation harness)

---

## 2.3 Tasks

### Task 2.3.1: Order Book Data Structure

**Objective:** Implement the flat-array + intrusive-linked-list order book.

**File Locations:** `core/src/book/OrderBook.cpp`, `core/src/book/PriceLevel.cpp`, `core/src/book/Order.cpp`

**Implementation:**
1. `Order` struct: id, account_id, side, type, quantity, price, filled_qty, tif, timestamp_ns, next/prev pointers (intrusive).
2. `PriceLevel`: price, head/tail order pointers, total_qty, order_count. 64-byte cache-line aligned.
3. `OrderBook`: bids_ (descending) and asks_ (ascending) flat arrays. Binary search for price level insertion. `book_seq_` monotonic counter.
4. Operations: `addOrder()`, `cancelOrder()`, `modifyOrder()`, `getBestBid()`, `getBestAsk()`, `getLevel(depth)`, `getSnapshot()`.
5. Memory: orders allocated from MemoryPool (Task 1.3.1). Zero `new`/`delete`.

**Definition of Done (Acceptance Criteria):**
* [x] Add/cancel/modify order operations are O(log N) for level lookup, O(1) for order insertion
* [x] Best bid > best ask never occurs (invariant maintained)
* [x] Price-time priority: orders at same price level execute in timestamp order (FIFO)
* [x] Book snapshot returns consistent bids + asks + seq
* [x] Zero heap allocation during add/cancel/modify (verified with allocator hook)

**SDD Checklist:**
- [x] Spec checkpoint: flat array price levels + intrusive linked-list orders — defined first, validated against spec
- [x] Spec checkpoint: zero allocations in hot path — defined first, validated against spec
- [x] Spec checkpoint: price-time priority (FIFO at each level) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: empty book, single order, full level cancel, max levels

---

### Task 2.3.2: Matching Engine

**Objective:** Implement price-time priority matching for limit, market, stop, and ICEBERG orders.

**File Locations:** `core/src/matching/MatchingEngine.cpp`, `core/src/matching/WalWriter.cpp`, `core/src/matching/IpcPublisher.cpp`, `core/src/matching/SelfTradeGuard.cpp`, `core/src/matching/IcebergManager.cpp`, `core/src/matching/StopOrderTrigger.cpp` *(decomposed 2026-09-27, remediation #38, F10: decomposes monolithic matching core into modular zero-heap components SelfTradeGuard, IcebergManager, StopOrderTrigger, WalWriter, IpcPublisher per spec §3.7 — supersedes prior single-file `MatchingEngine.cpp`)*

**Implementation:**
1. `onOrderReceived(order)`: pre-trade risk check → match → WAL write → IPC publish.
2. `matchLimitOrder(order)`: walk opposite book, fill at best prices, insert remainder if GTC.
3. `matchMarketOrder(order)`: walk opposite book, fill until exhausted or book empty.
4. FOK: fill fully or cancel (rollback). IOC: fill partially, cancel remainder.
5. Self-trade prevention: if maker and taker have same account_id, apply per-order `stp_mode` (spec §6.5 — supersedes fixed "reject taker" 2026-09-15; `CANCEL_NEWEST` preserves that behavior as the default).
6. ICEBERG: split into visible slice (configurable, default 10% of total) and hidden remainder. Replenish visible slice after fill. *(Migration note: `display_qty` column is stubbed in Phase-02 migration 038; Phase-16 Task 16.3.4 extends it for PEG. — added 2026-09-27, functional cluster review F2)*
7. Stop orders: enqueue in stop queue; trigger when market price crosses stop price.

**Definition of Done (Acceptance Criteria):**
* [x] Limit order matches correctly against opposite book (price-time priority)
* [x] Market order fills until exhausted or book empty
* [x] FOK fills fully or cancels with zero partial fill
* [x] IOC fills partially and cancels remainder
* [x] Self-trade prevention: no account trades against itself
* [x] ICEBERG: visible slice fills, hidden portion replenishes; total fill = order quantity
* [x] Stop order triggers when market price crosses stop price
* [x] Concurrent cancel requests for the same order resolve to a single cancel — locked balance released exactly once, no double-credit (spec §24 #10)

**SDD Checklist:**
- [x] Spec checkpoint: price-time priority matching — defined first, validated against spec
- [x] Spec checkpoint: self-trade prevention — defined first, validated against spec
- [x] Spec checkpoint: FOK/IOC semantics — defined first, validated against spec
- [x] Spec checkpoint: ICEBERG visible/hidden slices — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: empty book, cross-trade, partial fill, self-trade, ICEBERG replenish race

---

### Task 2.3.3: Pre-Trade Risk (In-Process)

**Objective:** Implement all 14 pre-trade risk checks in the C++ core (no IPC) — supersedes prior "10 checks"; checks 11–14 added 2026-09-15 per spec §3.3.

**File Locations:** `core/src/risk/PreTradeChecker.cpp`

**Implementation:**
1. Account status (ACTIVE only)
2. Instrument status (ACTIVE only; SUSPENDED/HALTED → reject with specific code)
3. Balance check (sufficient available)
4. Position limit (max open positions)
5. Order rate limit (per-account orders/sec via token bucket)
6. Price band (within price_band_pct_up/down of last_price)
7. Max order qty (within max_order_qty)
8. Margin check (post-fill utilization < liquidation_threshold for CROSS/PORTFOLIO)
9. Circuit breaker (not OPEN for instrument/account/scope)
10. KYC tier (tier allows trading on this instrument)
11. Tick/lot validation — price multiple of `tick_size`, qty multiple of `lot_size` and ≥ `min_order_qty` (spec §3.3 #11)
12. Min notional — order notional ≥ `instruments.min_notional` → `MIN_NOTIONAL_VIOLATION` (spec §3.3 #12)
13. Execution flags — `post_only` rejected if marketable (`POST_ONLY_VIOLATION`); `reduce_only` requires open position and may only reduce it (`REDUCE_ONLY_VIOLATION`) (spec §3.3 #13, §6.5)
14. Self-trade prevention — apply per-order `stp_mode` when maker/taker share account_id (spec §3.3 #14, §6.5; Task 2.3.11)

**Forward-reference placeholder notes (checks 8, 9, 10):**
- **Check 8 (Margin):** Full margin calculation (CROSS/ISOLATED/PORTFOLIO) is implemented in Phase 19 (Task 19.3.1). During Phase 2, use a **stub margin check** (simple balance ≥ notional × minimum margin ratio placeholder). Phase 19 replaces this stub with the real margin engine.
- **Check 9 (Circuit breaker):** The five-tier circuit breaker is implemented in Phase 13 (Task 13.3.1). During Phase 2, implement a **basic price-band circuit breaker** (reject if price moves > 5% in 60s) as a placeholder. Phase 13 replaces this with the full five-tier system (INSTRUMENT, ACCOUNT, VOLUME_SPIKE, OPTIONS_VOLATILITY, MARKET_WIDE).
- **Check 10 (KYC tier):** KYC tier enforcement is implemented in Phase 14 (Task 14.3.4). During Phase 2, use a **stub KYC check** (all accounts treated as T2 — full trading access placeholder). Phase 14 replaces this stub with the real KYC tier enforcement.
- **Check 13 (reduce_only):** Reduce-only enforcement needs open-position state; during Phase 2 use a **stub reduce-only check** (gateway-side position lookup — positions exist in PostgreSQL from Phase 3 Task 3.3.2). Phase 19 replaces the stub with in-process position-aware enforcement when the margin engine wires core position state.
- **Migration note:** `migrations/050_instruments_min_notional.up.sql` — adds `instruments.min_notional DECIMAL(28,8)` (spec §5.1; check 12).

**Additional forward-reference notes (degradation mode, monitoring):**
- **X-Degradation-Mode HTTP header** (Task 2.3.6): The HTTP middleware that sets this header on API responses is implemented in Phase 5 (Gateway API). During Phase 2, the C++ core publishes degradation mode transitions via Aeron; Phase 5 wires the HTTP header.
- **WS `system.status` broadcast** (Task 2.3.6): WebSocket distribution is Phase 6. During Phase 2, the C++ core publishes transitions via Aeron; Phase 6 wires the WS broadcast.
- **Prometheus + PagerDuty alerting** (AC #38): Monitoring infrastructure is Phase 7. During Phase 2, the C++ core exposes metrics via Aeron forwarder; Phase 7 wires Prometheus scraping and PagerDuty routing.
- **Throttled tiered priority** (AC #41): Rate limit tiers are Phase 5 (Task 5.3.2). During Phase 2, Throttled mode uses a simple rate cap; Phase 5 wires the tiered priority queue.

**Definition of Done (Acceptance Criteria):**
* [x] All 14 checks execute in < 10µs total (supersedes prior "10 checks")
* [x] Rejected orders emit specific error code (INSUFFICIENT_BALANCE, PRICE_OUT_OF_BAND, MIN_NOTIONAL_VIOLATION, POST_ONLY_VIOLATION, REDUCE_ONLY_VIOLATION, etc.)
* [x] Checks run in order: cheapest first (account status) → most expensive last (margin)
* [x] No IPC for risk checks (all in-process)
* [x] Tick/lot violations rejected; min-notional violations rejected with `MIN_NOTIONAL_VIOLATION`
* [x] `post_only` marketable order rejected; `reduce_only` without position rejected (stub during Phase 2 per note above)

**SDD Checklist:**
- [x] Spec checkpoint: 14 pre-trade risk checks in-process — defined first, validated against spec
- [x] Spec checkpoint: sub-10µs risk check latency — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: insufficient balance, suspended account, halted instrument, circuit breaker open

---

### Task 2.3.4: Binary WAL Integration

**Objective:** Integrate the WAL (from Phase 1) into the matching engine for crash recovery.

**File Locations:** `core/src/wal/Wal.cpp` (extend), `core/src/recovery/RecoveryManager.cpp`

**Implementation:**
1. On every state change (order add, cancel, modify, trade): append WAL entry with seq, timestamp, event_type, payload (FlatBuffers), CRC32.
2. `fsync` per batch (1ms or 100 events).
3. On startup: load snapshot from PostgreSQL → replay WAL from snapshot_seq+1 → verify boot-time invariant (book_seq == WAL tail).
4. Snapshot cadence: every 100k trades or 5 min. Snapshot stored in PostgreSQL.
5. WAL trim: only after PostgreSQL persistence confirmed + S3 archive confirmed.

**Definition of Done (Acceptance Criteria):**
* [x] Every state change produces a WAL entry with correct seq and CRC32
* [x] fsync per batch (1ms or 100 events) verified via strace
* [x] Recovery: snapshot + WAL replay restores exact book state
* [x] Boot-time invariant: book_seq == WAL tail; fail-closed on mismatch
* [x] Zero duplicate trades on recovery (idempotent replay via seq)
* [x] Zero missing trades on recovery

**SDD Checklist:**
- [x] Spec checkpoint: WAL entry per state change with CRC32 — defined first, validated against spec
- [x] Spec checkpoint: snapshot + WAL replay exact recovery — defined first, validated against spec
- [x] Spec checkpoint: zero dup/miss on recovery — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: partial write (crash mid-entry), WAL corruption, snapshot stale

---

### Task 2.3.5: Leader Election

**Objective:** Implement per-shard leader election using Redis SETNX.

**File Locations:** `core/src/election/LeaderElection.cpp`

**Implementation:**
1. `acquire()`: SETNX `engine:leader:{shardId}` with 10s TTL; value = `{pid}:{instanceId}`.
2. `heartbeat()`: every 3s, EXPIRE `engine:leader:{shardId}` to 10s; also SET `leader:heartbeat:{shardId}` with 15s TTL.
3. `check()`: if `leader:heartbeat:{shardId}` is missing or stale (>12s), attempt SETNX.
4. Split-brain detection: if two workers claim leadership (detected via heartbeat mismatch), both stop matching, P1 alert, degradation → ReadOnly.
5. Follower: subscribes to leader's IPC output; skips recovery; 5s startup delay for election stabilization.

**Definition of Done (Acceptance Criteria):**
* [x] SETNX acquire works: first caller wins, second fails
* [x] Heartbeat renewal every 3s; TTL extended to 10s
* [x] Follower detects leader death when heartbeat > 12s stale; attempts SETNX
* [x] Split-brain: both workers stop matching; P1 alert fires; degradation → ReadOnly
* [x] On restart, election runs before any order processing; 5s startup delay

**SDD Checklist:**
- [x] Spec checkpoint: Redis SETNX leader election with 10s TTL — defined first, validated against spec
- [x] Spec checkpoint: split-brain detection fail-closed — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: Redis down, network partition, simultaneous SETNX, stale heartbeat

---

### Task 2.3.6: Degradation Mode Manager

**Objective:** Implement the ModeManager for graceful degradation.

**File Locations:** `core/src/degradation/ModeManager.cpp`, `core/src/health/HealthChecker.cpp`

**Implementation:**
1. `ModeManager`: Redis keys `system:degradation:mode`, `:entered_at`, `:reason`. 6 mode constants: Normal, ReadOnly, MarketDataOnly, SpotOnly, Throttled, Maintenance.
2. `getMode()`: read from Redis (cached locally, refreshed per job). Default: Normal.
3. `setMode(mode, reason)`: atomic Redis SET + WS `system.status` broadcast.
4. Priority ordering: Maintenance > MarketDataOnly > SpotOnly > ReadOnly > Throttled > Normal.
5. `HealthChecker`: runs every 5s. Checks: **matching-loop p50 self-probe (the engine's own rolling matching-thread p50 — the §2.4 "core slow" ReadOnly trigger; supersedes Redis-p50-only, remediation #8)**, Redis p50 latency, PostgreSQL connectivity, WAL integrity, shard worker heartbeats, capacity (queue depth). Auto-transitions to correct mode. Auto-recovers when metrics normalize (cooldown prevents flapping).
6. HTTP middleware: `X-Degradation-Mode` header on every API response.

**Definition of Done (Acceptance Criteria):**
* [x] `setMode('ReadOnly', 'redis_slow')` atomically sets Redis keys; `getMode()` returns 'ReadOnly'
* [x] All 6 mode constants defined (Normal + 5 degraded)
* [x] HealthChecker runs every 5s, evaluates all metrics, auto-transitions
* [x] Matching-loop p50 self-probe drives the ReadOnly "core slow" trigger (spec §2.4, remediation #8); Redis/PG signals feed the same FSM
* [x] Auto-recovery: when triggers clear, transitions down (e.g., ReadOnly → Normal)
* [x] Cooldown prevents flapping (min 60s between transitions)
* [x] X-Degradation-Mode header present on all API responses

**SDD Checklist:**
- [x] Spec checkpoint: 6 degradation modes with correct triggers/recovery — defined first, validated against spec
- [x] Spec checkpoint: HealthChecker auto-transition with cooldown — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: simultaneous triggers, flapping, Redis down during mode read

---

### Task 2.3.7: IPC Layer (Aeron/Shared-Memory)

**Objective:** Wire the IPC layer for Go gateway ↔ C++ core communication.

**File Locations:** `core/src/ipc/AeronChannel.cpp`, `core/src/ipc/IpcChannel.cpp`

**Implementation:**
1. Inbound (Go → C++): order submissions, cancellations, modifies. SPSC queue.
2. Outbound (C++ → Go): trade fills, book updates, order status changes. SPSC queue.
3. FlatBuffers message encoding/decoding. Zero-copy where possible.
4. Backpressure: if inbound queue full, C++ drops with `ENGINE_OVERLOAD` alert; Go retries with backoff.
5. Metrics: queue depth, messages/sec, latency histogram.

**Definition of Done (Acceptance Criteria):**
* [x] Inbound: Go writes 50k orders/sec, C++ reads all with zero loss
* [x] Outbound: C++ writes 50k fills/sec, Go reads all with zero loss
* [x] Round-trip latency (Go → C++ → Go) < 10µs p99
* [x] Backpressure: queue full → ENGINE_OVERLOAD alert; Go retries

**SDD Checklist:**
- [x] Spec checkpoint: Aeron/shared-memory IPC zero-loss — defined first, validated against spec
- [x] Spec checkpoint: sub-10µs IPC round-trip — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: queue full, consumer slow, producer crash

---

### Task 2.3.8: Cross-Shard Basket Order 2PC

**Objective:** Implement 2-phase commit for cross-shard basket orders.

**File Locations:** `core/src/matching/CrossShardCoordinator.cpp`

**Implementation:**
1. `operation_id` (UUID v4) deduplicates retries.
2. Coordinator = lowest shard_id among participants.
3. Phase 1 (Reserve): lock balances via `AccountMutex` + `BalanceLock` on all participant shards; mark orders RESERVED with 5s TTL (supersedes prior 30s TTL — remediation #35; §24 #214 sets the 5s reservation timeout).
4. Phase 2 (Commit): atomically activate all orders within 5s window.
5. Compensate: on any failure (timeout or rejection), roll back all reservations.
6. `CompensationReaper`: scheduled job handles orphaned operations via reservation TTL expiry.
7. 8s total deadline (5s reserve + 3s commit/compensate) — arithmetic corrected 2026-09-27, remediation #35 (supersedes prior "10s total deadline (5s reserve + 3s commit/compensate)", which was self-contradictory).

**Definition of Done (Acceptance Criteria):**
* [x] Reserve phase holds balances on all participant shards; 5s TTL (supersedes prior 30s, remediation #35)
* [x] Commit atomically activates all within 5s window
* [x] Compensate rolls back all on any failure
* [x] `operation_id` deduplicates retries — same ID returns cached result
* [x] Cross-shard basket (3 shards, 3 orders) achieves all-or-nothing commit/compensate
* [x] Simulated reserve timeout → full compensation with balance unlock
* [x] Prometheus metrics: cross_shard_transactions_total, successes, failures, duration

**SDD Checklist:**
- [x] Spec checkpoint: 2-phase commit with compensation — defined first, validated against spec
- [x] Spec checkpoint: operation_id dedup — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: partial reserve failure, commit timeout, reaper cleanup, duplicate operation_id

---

### Task 2.3.9: Per-Account Collar and Price Band

**Objective:** Implement per-account collar (rate limit) and price band checks.

**File Locations:** `core/src/risk/PreTradeChecker.cpp` (extend)

**Implementation:**
1. Per-account collar: max orders/sec per account (configurable, default 50/sec). Token bucket maintained **in-process** in the engine (remediation #35 — supersedes the prior Redis `rl:account:{id}` implementation: a Redis round-trip per order violates the §3.3 in-process/no-IPC mandate and the <10µs risk budget; Redis-backed tiered quotas remain a Phase-05 gateway concern only).
2. Price band: reject if price deviates beyond `price_band_pct_up` (default 2%) / `price_band_pct_down` (default 5%) from `last_price`.
3. Check runs immediately AFTER the per-account collar.

**Definition of Done (Acceptance Criteria):**
* [x] Per-account collar: 51st order/sec rejected with `RATE_LIMIT_EXCEEDED`
* [x] Price band: price beyond band rejected with `PRICE_OUT_OF_BAND`
* [x] Band thresholds configurable per instrument

**SDD Checklist:**
- [x] Spec checkpoint: per-account collar + price band — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: first order (no last_price), collar burst, band edge

---

### Task 2.3.10: Time-in-Force Expiry Scheduler (GTD/DAY)

**Objective:** Expire orders whose time-in-force has elapsed, per spec §5.4 `time_in_force` enum (`GTC|IOC|FOK|GTD|DAY`).

**File Locations:** `core/src/matching/ExpiryScheduler.cpp`

**Implementation:**
1. Min-heap of pending expiries keyed by `expiry_timestamp` (GTD carries explicit expiry; DAY expires at instrument trading-day end per spec §1 24/5 calendar — 22:00 UTC Friday close, or session end).
2. `onOrderReceived`: GTD/DAY orders register in the heap; IOC/FOK handled inline in matching (Task 2.3.2).
3. On expiry: engine cancels order → emits `ORDER_CANCEL` WAL entry with `reason=EXPIRED` → status `EXPIRED` → Phase-6 `order_expired` WS event (§10.2 event already defined; this task is the producer).
4. Expiry checks run on the matching thread (deterministic ordering vs cancels/fills — an expired order can never fill).
5. **Deterministic time injection (WAL TIME_TICK):** The matching thread MUST NOT call `clock_gettime()` or any system clock function for expiry evaluation. Instead, the gateway sends periodic `TIME_TICK` IPC commands (cadence: every 100ms) carrying a monotonic wall-clock timestamp; **the matching thread itself stamps and appends the WAL entry** — the gateway never writes the WAL (single-writer invariant, remediation #35 — supersedes the prior "the gateway injects periodic TIME_TICK WAL events" wording, which gave the gateway a write path into the engine-owned WAL). TIME_TICK is registered in the §3.4 event-type enum. The ExpiryScheduler evaluates the min-heap against the latest TIME_TICK timestamp only — ensuring 100% deterministic replay. On WAL recovery, time advances exactly as recorded. (Added 2026-09-15 — production-completeness audit remediation #4: fixes non-deterministic replay when system clock jitter causes expiry ordering divergence.)

**Definition of Done (Acceptance Criteria):**
* [x] GTD order expires at its timestamp → status EXPIRED, balance unlocked exactly once
* [x] DAY order expires at end of trading day; orders spanning weekend close expire Friday 22:00 UTC
* [x] Expired order emits WAL `ORDER_CANCEL` + `EXPIRED` status → `order_expired` event
* [x] TIME_TICK WAL events injected every 100ms; expiry scheduler uses only WAL timestamps, never system clock
* [x] WAL replay of GTD expiry produces identical cancellation sequence regardless of replay speed

**SDD Checklist:**
- [x] Spec checkpoint: GTD/DAY time-in-force (spec §5.4/§6.1) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: expiry vs in-flight fill race, expiry during halted book, DAY across weekend boundary
- [x] Edge cases: TIME_TICK gap during leader failover (catchup from new leader's clock), replay at 100x speed produces identical sequence

---

### Task 2.3.11: Configurable Self-Trade Prevention Modes

**Objective:** Implement the four `stp_mode` values per spec §6.5 (supersedes the fixed "reject taker" behavior implied by §24 #4 — CANCEL_NEWEST preserves it as default).

**File Locations:** `core/src/matching/SelfMatchGuard.cpp`, `core/src/risk/PreTradeChecker.cpp` (extend)

**Implementation:**
1. `stp_mode` is carried on the incoming order (column `orders.stp_mode`, migration 038 — Phase-16 Task 16.3.10 owns the migration; this task owns the engine behavior and reads the field once persisted; during Phase 2 the field may arrive via IPC payload ahead of the column).
2. On same-account match: CANCEL_NEWEST rejects/cancels the incoming order; CANCEL_OLDEST cancels the resting order and proceeds; CANCEL_BOTH cancels both; DECREMENT reduces resting by incoming qty and cancels the incoming remainder.
3. STP handling runs inside the matching loop before any fill is emitted; all outcomes emit WAL events (`ORDER_CANCEL` with `reason=STP`) for recovery.

**Definition of Done (Acceptance Criteria):**
* [ ] All 4 prevention modes enforced in matching (CANCEL_NEWEST/CANCEL_OLDEST/CANCEL_BOTH/DECREMENT) plus NONE (Task 2.3.16) — wording clarified 2026-09-27, remediation #35
* [ ] CANCEL_NEWEST preserves spec §24 #4 (no account trades against itself)
* [ ] STP outcomes emit WAL cancel events with reason=STP; recovery reproduces them exactly

**SDD Checklist:**
- [x] Spec checkpoint: configurable STP modes per spec §6.5 / §24 #154 — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: DECREMENT partial resting, STP on ICEBERG hidden portion, STP during auction uncross

---

### Task 2.3.12: Cross-Shard Margin Coordination Interface

**Objective:** Implement the C++ matching engine interface for cross-shard margin headroom reservation and allocation coordination per spec §5.35 / §13.1 / §24 #176.

**File Locations:** `core/src/risk/CrossShardMarginCoordinator.cpp`, `core/include/risk/CrossShardMarginCoordinator.h`

**Implementation:**
1. **Reservation Protocol:** Engine accepts margin reservation instructions (`MARGIN_RESERVE_REQ` / `MARGIN_RESERVE_ACK` / `MARGIN_RESERVE_NACK` / `MARGIN_RELEASE` — message names unified 2026-09-27, remediation #35; supersedes the prior `RESERVE_MARGIN`/`COMMIT_MARGIN`/`RELEASE_MARGIN` names, which diverged from Phase-19 Task 19.3.11 on the same Aeron channel) from the Go Risk Coordinator via low-latency Aeron IPC (`aeron:ipc`).
2. **Atomic Slice Reservation:** When an order for a cross-shard portfolio account arrives, the engine verifies that local margin plus allocated reservation slice covers the required initial margin before placing the order on the book.
3. **Pessimistic Fallback (layered timeout budget, amended 2026-09-19):** the coordinator reservation RPC budget is **500µs**; at expiry the engine applies the pessimistic fallback immediately — standalone isolated/cross margin evaluation, rejecting orders that rely on external shard correlation offsets (`CROSS_SHARD_MARGIN_UNAVAILABLE`). The request continues in the background to the **>10ms** hard deadline, at which point the in-flight reservation is cancelled and compensated (released) before local-floor admission continues. Matches Phase-19 Task 19.3.11 and spec §13.1.
4. **WAL State:** Margin reservation IDs and slice commitments are recorded in the binary WAL to ensure deterministic recovery across restarts.

**Definition of Done (Acceptance Criteria):**
* [x] Margin slice reservations processed with <10µs latency in matching loop
* [x] Orders rejected if allocated reservation slice is insufficient
* [x] Timeout on coordinator IPC triggers fail-closed pessimistic margin fallback

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: cross-shard margin coordination interface (§13.1, §24 #176) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: reservation timeout during match, out-of-order slice release, shard engine restart mid-reservation

---

### Task 2.3.13: Sparse Order Book Liquidity Protection & Safe Level Serialization

**Objective:** Implement wide-spread protection bands, market order rejection in thin/empty books, and unpadded L2/L3 serialization per spec §6.6 / §10.1 / §24 #188.

**File Locations:** `core/src/book/OrderBook.cpp`, `core/src/matching/MatchingEngine.cpp`, `core/src/marketdata/BookSerializer.cpp`

**Implementation:**
1. **Wide-Spread Protection:** In `MatchingEngine`, inspect current spread `best_ask - best_bid`. If `spread > instrument.max_spread_pips`, immediately reject aggressive MARKET orders with `MARKET_ORDER_REJECTED_WIDE_SPREAD`.
2. **Empty Book Fail-Closed:** If the order's *opposite* book side is empty (a BUY rejects only when `ask_count == 0`; a SELL only when `bid_count == 0` — side-aware check, remediation #35, supersedes the prior side-unaware `bid_count == 0` or `ask_count == 0` condition that rejected legitimate liquidity consumption), MARKET, IOC, and FOK orders reject immediately with `ORDER_REJECTED_NO_LIQUIDITY` (no partial fills against nonexistent liquidity).
3. **Limit Order Liquidity Injection:** Limit orders inside or outside the wide spread are accepted normally and update book depth.
4. **Safe Level Serialization:** In `BookSerializer`, serialize only actual populated price levels. When book depth < 20 (or 0), emit exact level count without synthesizing zero-price padding records.

**Definition of Done (Acceptance Criteria):**
* [x] Market order rejected when spread exceeds `max_spread_pips`
* [x] Market/IOC/FOK order rejected when target book side is empty
* [x] L2 snapshot serializes exact available level count without artificial zero padding

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: sparse order book protection and level serialization (§6.6, §24 #188) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: crossed book rejection, 1-level book, rapid spread widening during matching

---

### Task 2.3.14: 2PC Reservation Timeout & Concurrent Limit
Added 2026-09-17 (gap analysis remediation #6).

Cross-shard basket order 2PC reservations (Task 2.3.5) must enforce:
1. Reservation timeout: 5 seconds. If any shard does not respond within 5s, the coordinator cancels the reservation and compensates all participating shards.
2. Maximum concurrent cross-shard operations per account: 10. Additional requests rejected with `CROSS_SHARD_LIMIT_EXCEEDED`.
3. Compensation reaper: background goroutine scans `pending_reservations` every 2s, compensating any reservation older than timeout.
4. Metrics: `cross_shard_reservation_timeout_total`, `cross_shard_reservation_active` Prometheus counters.

---

### Task 2.3.15: Market Order Slippage Protection & Price Banding

**Objective:** Prevent catastrophic fills on market orders in thin or gapped order books by converting market orders into synthetic aggressive limit orders with configurable slippage bands.

**File Locations:** `core/matching/market_order_protection.h`, `core/matching/market_order_protection.cpp`

**Implementation:**
1. Add per-instrument `max_slippage_bps` configuration (default: 100bps for major pairs, 200bps for exotic pairs).
2. On market order arrival: compute `protection_price = best_price ± (best_price × max_slippage_bps / 10000)`. Buy orders use `+`, sell orders use `-`.
3. Convert market order into synthetic limit order at `protection_price`. Mark as `MARKET_WITH_PROTECTION` in WAL.
4. If unfilled remainder exists after sweeping book to `protection_price`, reject remainder with `SLIPPAGE_EXCEEDED` status.
5. Admin API endpoint to update `max_slippage_bps` per instrument (Phase-07 registers, Phase-15 lifecycle manages).
6. Emit `MarketOrderProtectionTriggered` event to NATS for surveillance (Phase-17 consumes).
7. During auction/halt modes, market orders are already rejected — protection only applies in continuous trading.

**Definition of Done (Acceptance Criteria):**
* [x] Market order in thin book fills only up to protection_price, remainder rejected with SLIPPAGE_EXCEEDED
* [x] Protection price computed correctly for both buy and sell sides
* [x] max_slippage_bps configurable per instrument via admin API
* [x] WAL records MARKET_WITH_PROTECTION type for deterministic replay
* [x] Surveillance event emitted when protection triggers

**SDD Checklist:**
- [x] Spec checkpoint: market order protection — defined first, validated against spec
- [x] Spec checkpoint: per-instrument slippage bands — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: empty book (immediate reject), slippage_bps = 0 (full protection off), crossed book

---

### Task 2.3.16: STP mode `NONE` (disabled)

STP mode `NONE` (disabled) — gated to `PROFESSIONAL` and `ELIGIBLE_COUNTERPARTY` client categories only. When `stp_mode=NONE`, self-matching proceeds without prevention. All self-trades logged with `SELF_TRADE` flag and routed to Phase-17 surveillance for wash-trading monitoring. Rejects with `STP_NONE_NOT_PERMITTED` if client category is retail.

**SDD Checklist:**
- [x] Spec checkpoint: STP NONE is category-gated and surveillance-visible (§24 #274) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.17: Reference-Price Execution Rule

**Objective:** Enforce per-side execution collars against a PriceOracle reference price during the complete taker phase, independently of submission-time price-band validation. Migration 072 adds execution-rule configuration and `orders.expiry_reason`.

**Implementation:**
1. On entry to the taker phase, snapshot the current non-stale reference price and configured bid/ask up/down multipliers; hold those limits constant for that taker phase.
2. Stop matching before the first maker price outside the allowed range; expire the remaining quantity with `EXECUTION_RULE_PRICE_RANGE_EXCEEDED` and persist `expiry_reason` in WAL/order state.
3. If the rule, reference price, or required multiplier is absent, the execution rule is not enforced; stale references fail closed according to Phase-19.5.
4. Apply identically to direct, SOR-returned, batch, WS, and FIX orders.

**SDD Checklist:**
- [x] Spec checkpoint: reference-price execution limits are fixed for the taker phase and emit an expiry reason (§24 #277) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.18: STP Trade Groups, `TRANSFER`, and Prevented-Match Records

**Objective:** Extend STP from same-account checks to institution-defined trade groups and add auditable `TRANSFER` behavior. Migration 072 adds `accounts.trade_group_id`, prevented quantities, and `prevented_matches`.

**Implementation:**
1. Treat maker/taker accounts sharing `trade_group_id` as self-trade candidates; the taker mode remains authoritative except that `TRANSFER` requires both sides to request it.
2. `TRANSFER` behaves as DECREMENT for same-account orders; across accounts in one trade group it also transfers prevented quantity/notional between the member accounts — **the engine emits a `PREVENTED_MATCH` WAL event only; the balanced-ledger posting is performed by the Phase-03 GL service** (boundary pinned 2026-09-27, remediation #35 — supersedes the prior "through balanced ledger events" wording, which put GL posting inside the engine; the engine never writes PostgreSQL).
3. Emit immutable `PREVENTED_MATCH` records with maker/taker order IDs, group ID, mode, price, prevented quantities, and timestamp; no trade is emitted.
4. Persist cumulative prevented quantity on each order and expose records through Phase-05 APIs/private streams.

**SDD Checklist:**
- [x] Spec checkpoint: trade-group STP, TRANSFER, and prevented-match accounting replay deterministically (§24 #279–280) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.19: Engine Error Handling, Backpressure & Poison-Pill Defense

**Objective:** Implement matching loop crash boundaries, dynamic ring buffer backpressure thresholds, and poison-pill message quarantine per spec §2.7, §3.6, and §24 #299.

**Implementation:**
1. **Ring Buffer Watermark Backpressure:** Monitor inbound Aeron ring buffer utilization. At $>80\%$, shed non-critical low-priority traffic with `CAPACITY_EXCEEDED` (HTTP 503). At $>95\%$, trigger `CRITICAL_BACKPRESSURE` and halt ingress to prevent unsequenced drops.
2. **Poison-Pill Quarantine Boundary:** Wrap command decoding and order dispatch in `noexcept` boundary with structured error interceptor. Malformed payloads or internal assertion failures quarantine the offending event into `/var/log/exchange/poison_pill.log` without crashing the core matching thread.
3. **Emergency Halt Ingress Clamp:** When an instrument or engine enters `HALTED` or `Maintenance`, reject in-flight orders with `INSTRUMENT_HALTED` within $\le 10\mu\text{s}$ while preserving resting book order integrity.
4. **In-Process Watchdog Thread & Systemd Heartbeat:** Implement dedicated `std::jthread WatchdogThread` sampled at 100µs intervals monitoring `matching_loop_last_tick_tsc`. If cycle duration exceeds 500µs, logs non-blocking telemetry warning; if cycle stalls beyond 2ms (L0 catastrophic threshold), triggers immediate dirty WAL flush, releases Redis shard leader lock, and halts matching core to enable instant hot-standby promotion. Pings host supervisor via `sd_notify(0, "WATCHDOG=1")` per spec §3.6 and §19.13.

**Definition of Done (Acceptance Criteria):**
* [x] Ring buffer backpressure triggers shedding at 80% and ingress halt at 95%
* [x] Poison-pill quarantine catches decoding errors without crashing matching thread
* [x] In-process watchdog thread monitors loop cycle with 100µs sampling, 500µs warning, 2ms L0 halt, and systemd watchdog heartbeat

**SDD Checklist:**
- [x] Spec checkpoint: Matching engine backpressure, ring buffer watermarks, and poison-pill containment fail closed (§24 #299) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.20: Atomic Amend/Replace & Fairness Timestamping

**Objective:** Close the amend/replace race window and pin down fairness inputs the FIFO claim depends on, per spec §6.9 and §24 #336. Added 2026-09-27 (production-maturity remediation #24).

**File Locations:** `core/src/matching/MatchingEngine.cpp`, `core/src/book/OrderBook.cpp`

**Implementation:**
1. **Atomic cancel-replace:** route every amend through a single matching-thread `replaceOrder()` that validates, re-prices and re-queues under the thread's total order — never cancel+new from the gateway. Concurrent amends against the same `order_seq` resolve to exactly one winner; losers receive `STALE_MODIFY` (HTTP 409, Phase-05 Task 5.3.22).
2. **Amend priority restated in-core:** price change, quantity-up, `display_qty` change on ICEBERG slices, and any stop/peg/algo-child trigger-price change mint a fresh timestamp (lose priority); quantity-down-only preserves it. GTD timers reset on any accepted amend. `IOC`/`FOK` amends are rejected outright.
3. **Auction-state gate:** amends arriving while the instrument is in `CALL`, `CANCEL_ONLY`, `SUSPENDED` or `HALTED` are rejected with `AMEND_IN_AUCTION_REJECTED` (HTTP 409); cancels always remain available.
4. **Fairness timestamping:** `timestamp_ns` is stamped at Aeron ingress (`ingress_seq` assigned per shard, gap-checked); FIFO ties break on `(price, timestamp_ns, ingress_seq)`. PTP discipline (Phase-09 Task 9.3.12) feeds the stamp clock; minimum quote life interacts with the OTR counter (Phase-13 Task 13.3.6) rather than bypassing it.

**Definition of Done (Acceptance Criteria):**
* [x] Concurrent amends resolve to one winner; losers get STALE_MODIFY with zero double-apply
* [x] Priority rules enforced per field class; GTD reset and IOC/FOK amend rejection verified
* [x] Amend-in-auction rejected with AMEND_IN_AUCTION_REJECTED; cancel path unaffected
* [x] FIFO tie-break deterministic on (price, timestamp_ns, ingress_seq) under replay

**SDD Checklist:**
- [x] Spec checkpoint: atomic cancel-replace, amend priority per field class, auction-state amend gate and deterministic fairness timestamping (§24 #336) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.21: Account-Default Self-Trade Prevention Mode

**Objective:** Let accounts declare a default `stp_mode` applied when an order omits it, per spec §6.5 and §24 #368. Added 2026-09-27 (Binance-parity remediation #28).

**File Locations:** `core/src/risk/PreTradeChecker.cpp`, `migrations/094_accounts_default_stp.up.sql`

**Implementation:**
1. `accounts.default_stp_mode` (migration 094, default `CANCEL_NEWEST`); settable via account API within the category-gated value set (`NONE` stays Professional/ECP-only per Task 2.3.16).
2. Pre-trade resolution order: per-order `stp_mode` → account default → `CANCEL_NEWEST`. The resolved mode is stamped on the fill record for surveillance (Task 17.3.x wash-trade detection).
3. Changing the default never touches resting orders; it applies to orders accepted after the change timestamp.

**Definition of Done (Acceptance Criteria):**
* [ ] Omitted stp_mode resolves through account default to CANCEL_NEWEST
* [ ] Category gating enforced on NONE; resolved mode persisted per fill

**SDD Checklist:**
- [x] Spec checkpoint: account-default STP with category gating and per-fill persistence (§24 #368) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

### Task 2.3.22: Trade-Through Protection & Price Improvement

**Objective:** Prevent aggressive orders from matching at prices worse than the protected quote, and record price-improvement deltas for TCA reporting, per spec §6.6b and §24 #400. Added 2026-09-27 (feature completeness audit #36 — institutional CLOB best-execution requirement for MiFID II RTS 27/28).

**File Locations:** `core/src/matching/TradeThroughGuard.cpp`, `core/src/matching/PriceImprovementRecorder.cpp`

**Implementation:**
1. Maintain a protected-quote cache (best bid/ask from the public L2 feed) updated atomically on every book mutation.
2. Before executing an aggressive order, check its price against the protected quote:
   - Limit orders that would trade through → reject `TRADE_THROUGH_DETECTED` (HTTP 409).
   - Market orders → clip synthetic limit protection price to the protected quote; excess remainder `SLIPPAGE_EXCEEDED`.
   - IOC/FOK → reject `TRADE_THROUGH_DETECTED` if no liquidity at or better than the protected quote.
3. Price improvement: when a limit order fills at a better price than its limit, record `price_improvement_delta = limit_price − execution_price` on the fill record.
4. Auction suspension: trade-through checks are bypassed during call auctions (§7.1) since the uncross price is the single market-clearing price.
5. Emit trade-through prevention events to NATS for Phase-20 TCA reporting (RTS 27/28).

**Definition of Done (Acceptance Criteria):**
* [ ] Aggressive limit orders rejected with `TRADE_THROUGH_DETECTED` when crossing the protected quote
* [ ] Market orders clipped to protected quote; excess remainder cancelled `SLIPPAGE_EXCEEDED`
* [ ] Price-improvement delta recorded on fill records for TCA
* [ ] Trade-through checks suspended during call auctions
* [ ] Events emitted to NATS for best-execution reporting

**SDD Checklist:**
- [x] Spec checkpoint: trade-through prevention and price-improvement recording (§24 #400) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.23: Pipette Fixed-Point Scaling & Integer Matching Precision

**Objective:** Standardize matching engine internal arithmetic on $10^8$ integer fixed-point price ticks and units with instrument-aware pipette conversion factors, eliminating floating-point roundoff per spec §3.3a and §24 #402. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `core/include/book/Order.h`, `core/include/book/PriceLevel.h`, `core/src/book/OrderBook.cpp`, `core/src/matching/MatchingEngine.cpp`

**Implementation:**
1. Internal `Order` struct stores `int64_t price_ticks` ($10^8$ fixed-point) and `int64_t qty_units` ($10^8$ fixed-point). Native `float` and `double` are forbidden in the matching hot path.
2. In-memory `Instrument` definition loads `pip_factor` ($10^3$ for 3-decimal JPY pairs, $10^1$ for 5-decimal pairs) from reference data.
3. Pre-trade price-band, tick-size multiple, and slippage checks compute in integer space:  
   `spread_pips = (best_ask_ticks - best_bid_ticks) / (instrument.pip_factor * 1000)`.
4. SBE serialization and FlatBuffers IPC encode raw integer ticks without float string conversion.

**Definition of Done (Acceptance Criteria):**
* [x] All matching engine order book operations execute strictly with `int64_t` ticks
* [x] Pipette calculations (0.1 pip) for major (5-decimal) and JPY (3-decimal) pairs verified exact without rounding error
* [x] SBE serialization encodes integer ticks; zero floating-point math in hot path

**SDD Checklist:**
- [x] Spec checkpoint: pipette fixed-point integer scaling ($10^8$ ticks) and zero float math in hot path (§24 #402) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.24: In-Memory Bilateral Credit Matrix & Counterparty Matching Filter

**Objective:** Implement a high-performance in-memory shared-memory bilateral credit matrix in the C++ matching loop to enforce institutional credit screening without latency degradation per spec §3.3b and §24 #403. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `core/include/risk/BilateralCreditMatrix.h`, `core/src/risk/BilateralCreditMatrix.cpp`, `core/src/matching/MatchingEngine.cpp`

**Implementation:**
1. Pre-allocate 2D shared-memory array `std::atomic<uint64_t> credit_limit[MAX_PARTIES][MAX_PARTIES]` sized for up to 1,024 institutional clearing parties.
2. In `MatchingEngine::matchLimitOrder()`, walk the opposite book: before executing each potential match, check `credit_limit[maker_party][taker_party] >= fill_notional` and reciprocal counterparty credit.
3. If credit is insufficient: skip the resting order without removing it or altering its priority, and attempt match against the next eligible order in price-time queue.
4. On fill: atomically decrement credit in both directions; the Go Risk service updates limits asynchronously via Aeron control messages (`CREDIT_UPDATE`).

**Definition of Done (Acceptance Criteria):**
* [x] In-memory bilateral credit checks execute in < 2µs in the matching loop
* [x] Orders from counterparties with exhausted bilateral credit are skipped; price-time priority preserved among credit-eligible counterparties
* [x] Credit limits decremented atomically on fill; credit replenishments applied via Aeron control plane

**SDD Checklist:**
- [x] Spec checkpoint: in-memory bilateral credit matrix counterparty screening in matching loop (§24 #403) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.25: Optimistic Cross-Shard Reservation & Asynchronous Compensating Unwind

**Objective:** Replace blocking 5-second 2PC cross-shard balance locks with an optimistic reservation model and immediate compensating unwinds per spec §2.2a and §24 #404 (supersedes the blocking 5-second 2PC locking behavior of Task 2.3.8). Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `core/src/matching/OptimisticShardCoordinator.cpp`, `core/src/matching/MatchingEngine.cpp`

**Implementation:**
1. The coordinating gateway issues parallel non-blocking `TRY_MATCH` commands to participating shards with an aggressive **500µs** timeout.
2. Participant shards execute matching immediately without holding resting cross-shard mutex locks; if all legs fill, fills are committed.
3. If any participating shard rejects or times out (>500µs), the coordinator issues immediate synthetic market orders (`COMPENSATE_UNWIND`) to liquidate any partial legs filled on other shards at prevailing market prices.
4. Unwind slippage/loss is posted to `5010_CROSS_SHARD_EXECUTION_DIFF` in the general ledger.

**Definition of Done (Acceptance Criteria):**
* [x] Cross-shard basket orders execute optimistically with 500µs timeout
* [x] Zero resting multi-second balance locks on participating matching shards
* [x] Failed legs trigger immediate compensating market unwinds with zero orphaned positions

**SDD Checklist:**
- [x] Spec checkpoint: optimistic cross-shard routing with 500µs timeout and compensating unwinds (§24 #404) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 2.3.26: Discretionary Offset & Fill-and-Store (FAS) Order Execution

**Objective:** Implement Discretionary Offset limit orders that rest passively at limit price but execute aggressively up to a hidden price improvement band per spec §6.11 and §24 #405. Migration 103 adds `orders.discretionary_offset_pips`. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `core/include/book/Order.h`, `core/src/matching/MatchingEngine.cpp`, `migrations/103_orders_discretionary_offset.up.sql`

**Implementation:**
1. Migration 103 adds `orders.discretionary_offset_pips DECIMAL(10,4) DEFAULT 0.0`.
2. When a discretionary limit order arrives:
   - For BUY: aggressive evaluation price = `limit_price + discretionary_offset_pips * pip_size`.
   - For SELL: aggressive evaluation price = `limit_price - discretionary_offset_pips * pip_size`.
3. If opposite book has liquidity within the discretionary band, match immediately as taker.
4. Any unfilled remainder is inserted into the passive order book strictly at `limit_price`; discretionary offset is hidden and not broadcast on public L2/L3 market data feeds.

**Definition of Done (Acceptance Criteria):**
* [ ] Discretionary orders match against resting liquidity within the discretionary band
* [ ] Resting remainder displays only the limit price on public market data feeds
* [ ] Negative discretionary offsets rejected with `DISCRETIONARY_OFFSET_INVALID` (HTTP 400)

**SDD Checklist:**
- [x] Spec checkpoint: discretionary offset order execution with hidden price band and passive public display (§24 #405) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 2.4 Deliverables

- C++ matching engine: order book, matching, pre-trade risk (14 checks), ICEBERG, TIF expiry (GTD/DAY) with deterministic TIME_TICK WAL events, WAL, leader election, degradation, health, IPC, cross-shard 2PC, collar/price band, configurable self-trade prevention (4 prevention modes + NONE)
- Cross-shard margin coordination interface with pessimistic fail-closed fallback (Task 2.3.12)
- Sparse order book liquidity protection & safe unpadded level serialization (Task 2.3.13)
- Reference-price taker execution collars with persisted expiry reasons (Task 2.3.17)
- Trade-group STP, TRANSFER, and prevented-match accounting (Task 2.3.18)
- Engine backpressure watermarks, crash boundary, poison-pill quarantine, and in-process watchdog thread (Task 2.3.19)
- Atomic cancel-replace, amend-priority per field class, auction-state amend gate and fairness timestamping (Task 2.3.20)
- Account-default STP mode with category gating (Task 2.3.21)
- Trade-through protection and price-improvement delta recording (Task 2.3.22)
- Pipette fixed-point integer scaling and pip factor zero-float arithmetic (Task 2.3.23)
- In-memory bilateral credit matrix counterparty screening (Task 2.3.24)
- Optimistic cross-shard execution and automated compensating unwinds (Task 2.3.25)
- Discretionary offset and Fill-and-Store (FAS) order execution (Task 2.3.26)

---

## 2.5 Dependencies

- Phase 1, Phase 1.5

---

## 2.6 Duration Estimate

14–22 days (supersedes prior 12–18 — Tasks 2.3.23–2.3.26 absorbed in range; prior supersedes 12–18 — duration itemization completed 2026-09-27, feature completeness audit #36: Task 2.3.22 added; prior supersedes 12–18 — remediation #35: Tasks 2.3.14–2.3.18 added; prior supersedes 12–17):
- Task 2.3.1 (Order book): 2 days
- Task 2.3.2 (Matching): 2 days
- Task 2.3.3 (Pre-trade risk): 1 day
- Task 2.3.4 (WAL integration): 1.5 days
- Task 2.3.5 (Leader election): 1 day
- Task 2.3.6 (Degradation): 1 day
- Task 2.3.7 (IPC): 1.5 days
- Task 2.3.8 (Cross-shard 2PC): 1.5 days
- Task 2.3.9 (Collar/price band): 0.5 day
- Task 2.3.10 (TIF expiry): 0.5 day
- Task 2.3.11 (STP modes): 0.5 day
- Task 2.3.12 (Cross-shard margin): 0.5 day
- Task 2.3.13 (Sparse book protection): 0.5 day
- Task 2.3.14 (Cross-shard basket 2PC): 1 day (added to itemization, remediation #35)
- Task 2.3.15 (Market-order slippage protection): 1 day (added to itemization, remediation #35)
- Task 2.3.16 (STP mode NONE): 0.5 day (added to itemization, remediation #35)
- Task 2.3.17 (Reference-price execution collars): 0.5 day (added to itemization, remediation #35)
- Task 2.3.18 (STP trade groups / TRANSFER): 1 day (added to itemization, remediation #35)
- Task 2.3.19 (Engine error handling & backpressure): 0.5 day
- Task 2.3.20 (Atomic amend/replace & fairness timestamping): 1 day
- Task 2.3.21 (Account-default STP mode): 0.5 day (absorbed in range)
- Task 2.3.22 (Trade-through protection & price improvement): 0.5 day (added to itemization, feature completeness audit #36)
- Task 2.3.23 (Pipette fixed-point integer scaling): 0.5 day (absorbed in range, remediation #37)
- Task 2.3.24 (In-memory bilateral credit matrix): 0.5 day (absorbed in range, remediation #37)
- Task 2.3.25 (Optimistic cross-shard reservation): 0.5 day (absorbed in range, remediation #37)
- Task 2.3.26 (Discretionary offset execution): 0.5 day (absorbed in range, remediation #37)
- Testing + benchmarking: 2 days

---

## 2.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Order book: add/cancel/modify O(log N) level lookup, O(1) order insertion |
| 2 | Price-time priority: orders at same price execute in timestamp order (FIFO) |
| 3 | Limit order matches correctly against opposite book |
| 4 | Market order fills until exhausted or book empty |
| 5 | FOK fills fully or cancels with zero partial fill |
| 6 | IOC fills partially and cancels remainder |
| 7 | Self-trade prevention: no account trades against itself |
| 8 | ICEBERG: visible slice fills, hidden replenishes; total = order quantity |
| 9 | Stop order triggers when market price crosses stop price |
| 10 | All 14 pre-trade risk checks execute in < 10µs total (supersedes prior "10") |
| 11 | Rejected orders emit specific error codes |
| 12 | WAL entry per state change with correct seq and CRC32 |
| 13 | fsync per batch (1ms or 100 events) verified |
| 14 | Recovery: snapshot + WAL replay restores exact book state |
| 15 | Boot-time invariant: book_seq == WAL tail; fail-closed on mismatch |
| 16 | Zero duplicate trades on recovery (idempotent replay) |
| 17 | Zero missing trades on recovery |
| 18 | SETNX leader election: first caller wins, second fails |
| 19 | Heartbeat renewal every 3s; follower detects death at >12s stale |
| 20 | Split-brain: both stop matching; P1 alert; degradation → ReadOnly |
| 21 | 6 mode constants (Normal + 5 degraded) with correct triggers/recovery |
| 22 | HealthChecker runs every 5s; auto-transitions with cooldown |
| 23 | X-Degradation-Mode header on all API responses |
| 24 | IPC: 50k orders/sec zero-loss; round-trip < 10µs p99 |
| 25 | Cross-shard 2PC: all-or-nothing commit/compensate; operation_id dedup |
| 26 | Per-account collar: max orders/sec enforced |
| 27 | Price band: rejects beyond price_band_pct_up/down |
| 28 | Load test: 50k orders/sec sustained per shard on bare metal (4-core/16GB) |
| 29 | p99 tick-to-trade latency ≤ 50µs (supersedes prior ≤ 1ms — competitive with LMAX/EBS tier; individual component budget: IPC < 10µs, pre-trade risk < 10µs, matching < 5µs, WAL < 25µs) |
| 30 | No stream/queue backlog growth under sustained load |
| 31 | Zero heap allocation in hot path (verified with allocator hook) |
| 32 | Book snapshot + WAL replay exact recovery after crash |
| 33 | Boot-time invariant holds: book_seq == WAL tail (supersedes prior "≤"; equal to row 15 — snapshot advances book_seq to tail) |
| 34 | No PostgreSQL UPSERT while account mutex held; balance writes buffered outside lock |
| 35 | ModeManager::setMode atomically sets via Redis; all 6 modes defined |
| 36 | HealthChecker auto-transitions to correct mode and auto-recovers |
| 37 | X-Degradation-Mode header + WS system.status on transitions |
| 38 | Prometheus degradation metrics + PagerDuty P2/P1 alerting |
| 39 | Cross-shard operation_id dedup; 10s deadline; CompensationReaper for orphans |
| 40 | Cross-shard basket (3 shards) all-or-nothing; reserve timeout → compensation |
| 41 | Throttled mode enforces tiered priority (institutional > standard > basic) |
| 42 | Price band rejects beyond price_band_pct_up/down; configurable per instrument |
| 43 | Concurrent cancels of same order resolve once: locked balance released exactly once, no double-credit (§24 #10) |
| 44 | GTD/DAY expiry: order auto-cancels at expiry, emits EXPIRED status + `order_expired` event; no expiry-vs-fill race |
| 45 | Tick/lot validation rejects non-multiple price/qty (§3.3 #11) |
| 46 | Min-notional violations rejected with MIN_NOTIONAL_VIOLATION (§3.3 #12, §24 #157) |
| 47 | post_only marketable order rejected POST_ONLY_VIOLATION (§3.3 #13, §24 #129) |
| 48 | reduce_only without position / increasing exposure rejected REDUCE_ONLY_VIOLATION (§24 #130) |
| 49 | All 4 prevention modes + NONE enforced: CANCEL_NEWEST/OLDEST/BOTH/DECREMENT/NONE (§6.5, §24 #154) |
| 50 | STP outcomes emit WAL cancel (reason=STP); recovery reproduces exactly |
| 51 | Cross-shard margin coordination: matching engine reserves and releases margin slices via low-latency IPC interface; rejects orders when reservation fails (§24 #176) |
| 52 | Sparse book protection: aggressive market orders rejected when spread > max_spread_pips; empty book levels serialize without synthetic price padding (§24 #188) |
| 53 | TIME_TICK IPC commands ingested every 100ms; matching thread stamps and appends TIME_TICK WAL entries itself — zero gateway WAL writes, zero `clock_gettime` calls on matching thread (§24 #394 — citation corrected 2026-09-27, remediation #35; supersedes prior mis-citation of §24 #193, the SOR criterion) |
| 54 | WAL deterministic replay: GTD/DAY expiry sequence identical regardless of replay speed or system clock drift |
| 55 | 2PC reservation timeout fires within 5s and compensates all participating shards; max 10 concurrent per account enforced (spec §2.2, §24 #214) |
| 56 | Market orders converted to synthetic limits at best_price ± max_slippage_bps; unfilled remainder rejected SLIPPAGE_EXCEEDED (§24 #220) |
| 57 | Order amendment: price change or quantity-up generates new timestamp (loses priority); quantity-down preserves original timestamp |
| 58 | STP `NONE` is restricted to Professional/ECP accounts and self-trades emit surveillance signals (§24 #274) |
| 59 | Reference-price execution limits are snapshotted for the taker phase; out-of-range remainder expires with `EXECUTION_RULE_PRICE_RANGE_EXCEEDED` (§24 #277) |
| 60 | STP applies across `trade_group_id`; `TRANSFER` and immutable prevented-match records replay exactly (§24 #279–280) |
| 61 | Ring buffer >80% sheds non-critical orders, >95% halts ingress; malformed payloads quarantine into poison-pill DLQ without crashing engine thread (§24 #299) |
| 62 | Atomic cancel-replace resolves one winner per order_seq (STALE_MODIFY for losers); amend priority per field class; amend-in-auction rejected AMEND_IN_AUCTION_REJECTED; FIFO tie-break on (price, timestamp_ns, ingress_seq) (§24 #336) |
| 63 | Account-default STP resolves omitted modes with NONE gated to Professional/ECP; resolved mode persisted per fill for surveillance (§24 #368) |
| 64 | Trade-through prevention: aggressive orders rejected/clipped at protected quote; price-improvement delta recorded for TCA (§24 #400) |
| 65 | Pipette fixed-point integer scaling ($10^8$ ticks) and pip factor eliminate float roundoff in hot path (§24 #402) |
| 66 | In-memory bilateral credit matrix screens counterparties in matching loop, skipping credit-depleted orders without FIFO distortion (§24 #403) |
| 67 | Optimistic cross-shard routing executes with 500µs timeout and automated compensating unwinds without resting 2PC locks (§24 #404) |
| 68 | Discretionary offset orders execute aggressively within discretionary band while displaying only passive limit price (§24 #405) |
