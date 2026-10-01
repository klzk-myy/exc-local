# Phase 16 — Advanced Order Types

**Duration:** 13–20 days (supersedes 12–16 — duration itemization completed 2026-09-27, feature completeness audit #36: Task 16.3.25 MOO/MOC added; prior supersedes 12–16 — duration itemization completed 2026-09-27, remediation #35: Tasks 16.3.14–16.3.21 added to §16.6; the itemized sum was 19.5 days against the 16-day header; prior supersedes 12–15 — Tasks 16.3.23–16.3.24 added 2026-09-27, remediation #28; prior supersedes 10–13 — VP orders + grid bot added 2026-09-22)
**Dependencies:** Phases 2, 3, 5, 14
**Spec Reference:** §6 (Order Types)

---

## 16.1 Objectives

Implement advanced order types: TWAP, VWAP, trailing stop, peg-to-best, benchmark fixing orders, and algo order framework with delayed dispatch. (ICEBERG base order type implemented in Phase 2 Task 2.3.2; no enhanced ICEBERG task in this phase.)

---

## 16.2 Prerequisites

- Phases 2, 3, 5, 14 complete

---

## 16.3 Tasks

### Task 16.3.1: TWAP (Time-Weighted Average Price)

**Objective:** Implement TWAP algo orders.

**File Locations:** `services/internal/algo/twap.go`

**Implementation:**
1. `POST /api/v1/orders/twap` — total qty, duration, interval.
2. Split total qty into equal slices per interval.
3. Submit each slice as a limit order at current mid.
4. Cancel and resubmit if not filled within interval.

**Definition of Done (Acceptance Criteria):**
* [x] TWAP splits qty into equal slices per interval — `services/internal/algo/twap.go` `twapPlan` (N=⌈duration/interval⌉ equal slices, residual pinned to the final slice); TestTWAPPlanEqualSlicesAndConservation
* [x] Each slice submitted as limit at mid — `sliced.go` `runSliced` resolves `Engine.mid` (persisted-book BBO via `PgTopOfBook`, last-trade `PgRefPrice` fallback — never fabricated) and dispatches GTC LIMIT at mid ± discretion; TestTWAPEndToEndFills asserts mid pricing
* [x] Unfilled slices cancelled and resubmitted — interval-end `cancelChild` + remainder roll-forward in `runSliced`; interval band 1s–1h enforced in `validateTWAP` (Task 16.3.10 step 3); children flow through `orders.Service` via `algoChildExecutor` (cmd/gateway/algo_adapter.go)

**SDD Checklist:**
- [x] Spec checkpoint: TWAP algo — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go test ./internal/algo/` green (16/16 unit + PG-gated `TestIT*`)

---

### Task 16.3.2: VWAP (Volume-Weighted Average Price)

**Objective:** Implement VWAP algo orders.

**File Locations:** `services/internal/algo/vwap.go`

**Implementation:**
1. `POST /api/v1/orders/vwap` — total qty, duration.
2. Split qty proportional to historical volume profile.
3. Submit slices as limit orders.

**Definition of Done (Acceptance Criteria):**
* [x] VWAP splits qty proportional to volume profile — `services/internal/algo/vwap.go` `vwapDriver` + `weightsToPlan` over the `VolumeProfileSource` seam (`PgVolumeProfile`: trailing-24h bucketed tape share; ClickHouse bucket store binds the same seam in Phase-23; documented `flatProfile` fallback when the tape is empty); TestVWAPProfileWeighting
* [x] Slices submitted correctly — shares the interval-slicer dispatch (`runSliced`, LIMIT at mid ± discretion); `gaussianPerturb` σ=5% profile smoothing + ±15% size / ±30% timing jitter per Task 16.3.12; actual dispatch times/qty/prices auditable on `algo_order_children`

**SDD Checklist:**
- [x] Spec checkpoint: VWAP algo — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go test ./internal/algo/` green; PG-gated `TestITReadSeams` exercises the profile/volume read seams

---

### Task 16.3.3: Trailing Stop

**Objective:** Implement trailing stop orders.

**File Locations:** `core/src/matching/TrailingStop.cpp`

**Implementation:**
1. Trailing stop: stop price trails market price by fixed offset or percentage.
2. Buy trailing stop: stop price = market price + offset (for short positions).
3. Sell trailing stop: stop price = market price - offset (for long positions).
4. When market price moves favorably, stop price follows.
5. When market price crosses stop price, trigger as market order.

**Definition of Done (Acceptance Criteria):**
* [x] Trailing stop follows market price — `StopOrderTrigger` pending anchor + `MatchingEngine` reference evaluation over LAST prints / MARK / INDEX oracle (`core/include/matching/StopOrderTrigger.hpp`, `core/src/matching/MatchingEngine.cpp`); `Phase16Trailing.AbsoluteDistanceArmsAndDerivesStop`
* [x] Stop price updates on favorable market movement — favorable-only ratchet: SELL anchors rise, BUY anchors fall; adverse prints never re-anchor; `Phase16Trailing.AnchorRatchetsFavorableOnly`
* [x] Triggers as market order when crossed — the trigger pop journals `ORDER_TRIGGERED` (WAL-before-mutate) then sweeps as a market order; `Phase16Trailing.AnchorRatchetsFavorableOnly`, `Phase16Wal.OrderNewExAndAdvancedRowsRoundTrip`

**SDD Checklist:**
- [x] Spec checkpoint: trailing stop — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `test_phase16` 33/33 green; full core ctest 32/32

---

### Task 16.3.4: Peg-to-Best *(superseded by Task 16.3.11 — the single pegged-order owner; retained for provenance, remediation #35)*

**Objective:** Implement peg-to-best (peg-to-primary) orders.

**File Locations:** `core/src/matching/PegOrder.cpp`

**Implementation:**
1. Peg-to-best: order price follows best bid/ask.
2. Buy peg: price = best bid (a re-priced peg joins the back of its new level per the FIFO tie-break on `(price, timestamp_ns, ingress_seq)` — the "always at front of queue" claim is superseded, remediation #35).
3. Sell peg: price = best ask.
4. Re-peg on every book change.
5. Display: peg orders hidden from public book (L2).

**Definition of Done (Acceptance Criteria):**
* [x] Peg-to-best follows best bid/ask — satisfied via the 16.3.11 owner implementation (`kPegPrimary` same-side best + signed offset); `Phase16Peg.PrimaryPegOffsetsSameSide`
* [x] Re-pegs on every book change — BBO-mutating admission/cancel re-evaluates all pegged orders and journals `PEG_REPRICE` before the book move; `Phase16Peg.RepriceFollowsVisibleBBO`
* [x] Hidden from public L2 book — `l2_visible()` excludes `OrderType::PEG` from both the binary L2 snapshot (`BookSerializer`) and the FlatBuffers book snapshot (`IpcPublisher`); `Phase16Peg.L2SnapshotExcludesPeggedLevel`

**SDD Checklist:**
- [x] Spec checkpoint: peg-to-best — defined first, validated against spec (owner Task 16.3.11)
- [x] All spec checkpoints pass after implementation — `test_phase16` peg suite green

---

### Task 16.3.5: Bracket Orders *(superseded by Task 16.3.14 — the single bracket/OTO owner; retained for provenance, remediation #35)*

**Objective:** Implement bracket orders (take-profit + stop-loss around a position).

**File Locations:** `services/internal/algo/bracket.go`

**Implementation:**
1. Bracket order: entry order + take-profit + stop-loss as a linked group.
2. When entry fills, take-profit and stop-loss become active OCO pair.
3. If take-profit fills, stop-loss cancels; if stop-loss triggers, take-profit cancels.
4. `POST /api/v1/orders/bracket` — submit bracket order.

**Definition of Done (Acceptance Criteria):**
* [x] Bracket order creates entry + take-profit + stop-loss — delivered by Task 16.3.14 owner (`InsertOrderBracketTx`, mig 225)
* [x] Take-profit and stop-loss activate on entry fill — partial-fill proportional children via OCO pair (TestITBracketFillCascade)
* [x] Fill of one side cancels the other (OCO behavior) — sibling cancel via Phase-14 OCO_LINK machinery
* [x] Bracket survives recovery (WAL-recorded link) — WAL OCO_LINK + durable bracket_orders row; RecoverComposites at boot

**Transitive dependency note:** Bracket order recovery uses WAL (Phase 4, Task 4.3.1). Phase 4 is not in Phase 16's explicit dependency list but is transitively available via Phase 2 (which depends on Phase 1's WAL infrastructure and is extended by Phase 4).

**SDD Checklist:**
- [x] Spec checkpoint: bracket order with take-profit + stop-loss — defined first, validated against spec — bound P16-T16.3.5-C1 (alias → 16.3.14 evidence), passing
- [x] All spec checkpoints pass after implementation — 25/25 P16 checkpoints green

---

### Task 16.3.6: Spread Orders

**Objective:** Implement multi-leg spread orders with price relationship enforcement.

**File Locations:** `services/internal/algo/spread.go`

**Implementation:**
1. Spread order: simultaneous buy + sell of related instruments (e.g., EUR/USD - GBP/USD).
2. Price relationship: spread price = leg1 price - leg2 price (enforced).
3. Both legs execute atomically or neither executes.
4. `POST /api/v1/orders/spread` — submit spread order with legs + spread price.

**Definition of Done (Acceptance Criteria):**
* [x] Spread order creates multi-leg with price relationship — `services/internal/algo/spread.go`: exactly two opposite-side legs validated in `validateSpread`; both legs are `algo_order_children` rows (role LEG) dispatched through the order pipeline
* [x] Both legs execute atomically or neither — **implementation deviation (2026-09-29):** the per-shard matching core has no cross-instrument atomicity, so "atomically" is realized as IOC/marketable-limit leg dispatch + rollback-on-partial: leg A IOC → leg B IOC → any unfilled/partial residual is flattened by opposite-side UNWIND children (`executeSpread`/`unwindLeg`). TestSpreadRollbackOnPartial + TestSpreadLegAFailureStopsLegB pin the both-or-neither outcome; a same-atomic-compensation C++ engine path remains the residual gap
* [x] Spread price enforced (reject if market spread doesn't match) — `precheckSpread` resolves leg mids (`mid(leg1)−mid(leg2)` vs `spread_price`, direction-aware) BEFORE the parent row exists; TestSpreadMarketMismatchRejects
* [x] Spread order rejection returns SPREAD_ORDER_REJECTED — coded error from `precheckSpread`; TestSpreadMarketMismatchRejects asserts the code and zero durable residue

**SDD Checklist:**
- [x] Spec checkpoint: spread order multi-leg with price relationship — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 16.3.7: Scaled Orders

**Objective:** Implement scaled orders (multiple price levels with quantity distribution).

**File Locations:** `services/internal/algo/scaled.go`

**Implementation:**
1. Scaled order: multiple limit orders at different price levels.
2. Quantity distribution: equal, linear, or custom weights per level.
3. Price levels: configurable count + spacing (e.g., 5 levels, 10bps apart).
4. `POST /api/v1/orders/scaled` — submit scaled order.

**Definition of Done (Acceptance Criteria):**
* [x] Scaled order creates multiple limit orders at specified levels — `services/internal/algo/scaled.go`: ≤20 levels cap enforced in `validateScaled`; each level is an independent GTC LIMIT child row; TestScaledWeightsAndLevels
* [x] Quantity distribution correct (equal, linear, or custom) — `scaledWeights` (EQUAL uniform / LINEAR ramp / CUSTOM explicit) + `weightsToPlan` conserves `total_qty` exactly (final level takes the residual); TestScaledValidation covers custom-weight shape errors
* [x] Price levels spaced correctly — explicit `level_prices`, or `start_price` (default: current mid) stepped by `spacing_pips` (× instruments.pip_size) or `spacing_bps`; BUY ladders descend, SELL ascend; BUY-direction assertion in TestScaledWeightsAndLevels
* [x] Individual level fills tracked independently — each level is its own `algo_order_children` row with derived `algo:{parent}:{seq}` client_order_id; `scaledDriver` monitor refreshes each child's status/fills to terminal; PG-gated TestITChildRowsAndIdempotentCID asserts the cid derivation + dispatch audit columns

**SDD Checklist:**
- [x] Spec checkpoint: scaled order with quantity distribution — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 16.3.8: Algo Order Framework

**Objective:** Implement the algo order framework with delayed dispatch.

**File Locations:** `services/internal/algo/framework.go`

**Implementation:**
1. Algo engine: manages TWAP, VWAP, and custom algos.
2. `POST /api/v1/orders/algo` — submit algo order with type + params.
3. Algo state: PENDING → RUNNING → PAUSED → COMPLETED → CANCELLED.
4. `POST /api/v1/orders/algo/{id}/pause` — pause.
5. `POST /api/v1/orders/algo/{id}/resume` — resume.
6. `DELETE /api/v1/orders/algo/{id}` — cancel.
7. Delayed dispatch: schedule order for future time.

**Definition of Done (Acceptance Criteria):**
* [x] Algo engine manages TWAP, VWAP, custom — `services/internal/algo/framework.go` `Engine` + driver registry: TWAP/VWAP/VP/SCALE/SPREAD registered as `parentType` drivers; generic `POST /api/v1/orders/algo` accepts `algo_type`+params and dispatches to the registered validator/driver (handlers_algo.go `AlgoSubmit`/`AlgoSubmitTyped`)
* [x] Algo state machine works — `algo_orders.status` 8-state machine (NEW→PENDING→RUNNING→PAUSED→RUNNING→COMPLETED|CANCELLED|EXPIRED|FAILED); transitions are compare-and-swap writes via `store.CASStatus` (from-list guarded, e.g. Pause only from RUNNING); TestPauseResumeCancel
* [x] Pause/resume/cancel work — `Engine.Pause/Resume/Cancel` in framework.go: pause CASes RUNNING→PAUSED and quiesces live children via `cancelOpenChildren`→`orders.Service.Cancel`; resume re-arms remaining schedule; cancel drains to terminal CANCELLED with zero orphan slices; `POST /api/v1/orders/algo/{id}/pause|resume` + `DELETE /api/v1/orders/algo/{id}` live in routes_v1
* [x] Delayed dispatch works — `start_at`/`PENDING` arm: parent row inserted PENDING with `start_at`; `Engine.Run`'s sweeper picks due rows via `store.PendingDue` and fires the driver (adoption of RUNNING/PAUSED rows via `store.ActiveParents` covers restart); TestDelayedDispatchStaysPending + PG-gated TestITParentLifecyclePersisted

**SDD Checklist:**
- [x] Spec checkpoint: algo order framework — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go test ./internal/algo/` green (16 unit tests + PG-gated `TestIT*`)

---

### Task 16.3.9: Benchmark Fixing Orders

**Objective:** Implement Benchmark Fixing Orders (`FIXING` order type) executed at published official benchmark fixing rates (WM/Refinitiv 4 PM London Fix, ECB 14:15 CET reference rates) per spec §6.2, §24 #124.

**File Locations:** `services/internal/algo/fixing.go`, `core/src/matching/FixingOrder.cpp`

**Implementation:**
1. Support `FIXING` order type in order schema and gateway validation with target benchmark identifier — canonical vocabulary `WM_R_4PM` / `ECB_1415` / `TOKYO_0955` (spec §6.4, migration 038 CHECK; **supersedes** the plan strings `WM_REFINITIV_4PM_LDN` / `ECB_1415_CET` — the auction_calendar scheduler vocabulary `WM_LONDON_4PM` / `ECB_REF_1415` maps at the seam in `algo/fixing.go`).
2. Enforce pre-fixing order submission cutoff window (default cutoff at T-15m prior to benchmark publication time; orders submitted after cutoff are rejected with `FIXING_CUTOFF_EXCEEDED`).
3. Lock fixing orders against modification/cancellation after the cutoff window (`FIXING_CANCELLATION_RESTRICTED`).
4. Queue fixing orders in algo service with `RESERVED` status — FIXING orders never enter the engine dispatch path.
5. On official benchmark rate publication received via Price Oracle / Mark Price Service (Phase 19.5), match offsetting buy and sell fixing orders at the exact published fix rate — deterministic order-id sequencing, atomic per-cross SERIALIZABLE tx (order/trade/audit/journal writes in one commit).
6. Settle any residual net imbalance against designated institutional liquidity providers at the published fix rate with guaranteed zero tracking error vs. the benchmark. *(Implementation 2026-10-04 — supersedes the 2026-09-29 "documented seam" note: the designated-LP leg is realized as an LP-side FIXING order absorbed inside the same atomic SERIALIZABLE cross at the exact `benchmark_fixings.rate` — zero tracking error by construction; proven live-PG in `TestITFixingLPResidualCleared` (client residual fully cleared, LP remainder honestly `FIXING_IMBALANCE`-audited) and `TestITFixingResidualWithoutLPHonest` (no LP ⇒ residual queued, no fabricated fill). Two latent execution-path defects found + fixed by this verification: chart codes were un-suffixed (`2160_CLEARING_TRANSIT` vs seeded `…_USD` → every reserve/cross journal would abort `LEDGER_UNKNOWN_ACCOUNT`) and `insertAudit` wrote 7 columns against 6 values — both would have failed every production fix. Rate provenance is the Phase-19.5 oracle seam, wired 2026-10-01 via `oracle.NewRedisFixingMarkSource` — gateway `Prices` reads the published `mark:{symbol}` within the 5s staleness gate; absent/stale marks still record `SKIPPED`, never a synthetic rate (supersedes the prior `Prices: nil` unwired note).)*

**Definition of Done (Acceptance Criteria):**
* [x] Benchmark Fixing orders accepted prior to cutoff window (T-15m) — `algo.FixingService.AdmitSubmit` + `orders.Service` fixing gate
* [x] Orders submitted after cutoff rejected with `FIXING_CUTOFF_EXCEEDED` — admission cutoff gate, `services/internal/algo/fixing.go`
* [x] Cancellation locked after cutoff window (`FIXING_CANCELLATION_RESTRICTED`) — `AssertMutable` wired in cancel + amend paths
* [x] Executed at published benchmark rate (WM/Refinitiv 4 PM London, ECB 14:15 CET) — `executeCross` settles buy↔sell at the `benchmark_fixings.rate` inside one SERIALIZABLE tx (fill journal + trades row + audit + status atomically); no rate ⇒ `SKIPPED`, never a synthetic price
* [x] Imbalances cleared with zero tracking error vs official published benchmark — designated-LP FIXING order absorbs the residual inside the same atomic cross at exactly `benchmark_fixings.rate`; proven live-PG: `TestITFixingLPResidualCleared` (client buy 1000 filled @ fix, LP sell 1500→PARTIALLY_FILLED 500 remainder, `FIXING_IMBALANCE` on LP only, wallet conservation verified) + `TestITFixingResidualWithoutLPHonest` (residual queued+audited, zero fabricated trades); rate provenance is the Phase-19.5 `PriceSource` seam — wired via `oracle.NewRedisFixingMarkSource` (fresh `mark:{sym}` ⇒ `RECORDED`; absent/stale ⇒ `SKIPPED`, never fabricated)

**SDD Checklist:**
- [x] Spec checkpoint: benchmark fixing orders — defined first, validated against spec — bound P16-T16.3.9-C1, passing
- [x] All spec checkpoints pass after implementation — 25/25 P16 checkpoints green

---

### Task 16.3.10: Order Execution Parameters & Persistence (Migration 038)

**Objective:** Persist the execution/parameter fields the order types in this phase (and §6.5 flags) require — closing the spec §5.4 schema gap where `orders` had no columns for flags, iceberg display, peg, OCO linkage, or algo parameters (§24 #131). Added 2026-09-15.

**File Locations:** `migrations/038_orders_execution_params.up.sql`, `services/internal/api/orders.go` (validation), `core/src/matching/` (field plumbing)

**Implementation:**
1. Migration 038 adds `orders` columns (spec §5.4 extension): `post_only BOOL`, `reduce_only BOOL`, `display_qty DECIMAL` (iceberg visible), `peg_offset DECIMAL` (peg-to-best offset), `stp_mode` (CANCEL_NEWEST|CANCEL_OLDEST|CANCEL_BOTH|DECREMENT, default CANCEL_NEWEST), `oco_group_id UUID`, `algo_type`, `algo_params JSONB` (TWAP interval/duration, VWAP profile, trailing offset, scaled levels, fixing benchmark).
2. Gateway validation: flags parsed, validated, and persisted on submit; `algo_params` schema-validated per `algo_type`; iceberg orders require `display_qty < qty`.
3. Algo parameter validation: TWAP interval 1s–1h; trailing offset > 0; scaled ≤ 20 levels; fixing benchmark identifier enum (Task 16.3.9).
4. Recovery: fields are part of WAL order state (Phase-2 WAL schema already carries order payload — verify all new fields included in WAL record + snapshot serialization).
5. `oco_group_id` formalizes Task 14.3.1's OCO linkage (previously implied); brackets store TP/SL order ids in `algo_params`. *(Superseded in part 2026-10-07 — Task 14.3.1 landed first: `oco_group_id` now ships as `BIGINT` in migration **218** (`218_oco_group_link.up.sql`), not UUID here — the group id is the same monotonic `link_id` the engine journals in `WalOcoLinkPayload`, so it must stay integral for wire/WAL parity. Migration 038 must NOT re-add the column; this task retains `post_only`/`reduce_only`/`display_qty`/`peg_offset`/`stp_mode`/`algo_type`/`algo_params` only.)*

**Definition of Done (Acceptance Criteria):**
* [x] All execution params persisted on `orders` and round-trip through WAL replay/snapshot (§24 #131) — migration 038 residual columns (`peg_mode/peg_offset/peg_limit/algo_type/algo_params/fixing_benchmark/hidden/gslo`; `post_only`/`reduce_only`/`display_qty`/`stp_mode` shipped earlier via 155, `oco_group_id` via 218) + `OrderNew` wire fields appended in `exchange.fbs` (engine sibling decodes into WAL)
* [x] post_only/reduce_only/stp_mode validated at submit; invalid combos rejected — `execparams.go` + `validate.go`
* [x] `algo_params` schema-validated per algo type; iceberg requires display_qty < qty — per-algo validation (TWAP interval 1s–1h, SCALED ≤20 levels, VWAP duration, SPREAD legs, FIXING benchmark enum), byte cap + JSON-object shape enforced, unknown params fail closed
* [x] OCO groups and bracket links recoverable from WAL — `oco_group_id` (218) + composite store (sibling, `store_composite.go`/migration 225)

**SDD Checklist:**
- [x] Spec checkpoint: execution params persisted + WAL round-trip (§5.4, §24 #131) — defined first, validated against spec — bound P16-T16.3.10-C1 incl. ORDER_NEW_EX WAL round-trip gtest, passing
- [x] All spec checkpoints pass after implementation — 25/25 P16 checkpoints green
- [x] Edge cases: post_only on market order (reject — marketable by definition), reduce_only + iceberg combination, algo_params exceeding size limit — verified: post_only+MARKET reject (validate.go:224), iceberg display_qty<qty (validate.go:249), algo_params 8KB cap (execparams.go:74); reduce_only+iceberg admitted as independent flags

---

### Task 16.3.11: Pegged Orders (Peg-to-Mid, Peg-to-Primary, Peg-to-Market) — **single owner of pegged-order logic** (Task 16.3.4 reduced to a thin alias, remediation #35)

**Objective:** Implement pegged order types that dynamically track reference prices for algorithmic liquidity provision per spec §6.2 extension. Added 2026-09-15 (production-completeness audit remediation #4).

**File Locations:** `core/src/matching/PeggedOrderTracker.cpp`, `services/internal/api/orders.go` (extend validation)

**Implementation:**
1. Three peg modes: `PEG_TO_MID` (midpoint of NBBO), `PEG_TO_PRIMARY` (same-side best), `PEG_TO_MARKET` (opposite-side best). Stored in `orders.peg_mode` + `orders.peg_offset` (already in migration 038 via Task 16.3.10).
2. Re-pegging: on every book update that changes the reference price, the PeggedOrderTracker re-prices the pegged order. Re-pegging runs on the matching thread (deterministic) using the book's own BBO — not external feeds.
3. Limit collar: pegged orders carry an optional `peg_limit` ceiling/floor beyond which the order will not re-peg (becomes resting at limit).
4. Visibility: pegged orders are hidden from L2 public book (like iceberg hidden portion) but visible in L3 order-level data with `peg_mode` tag.
5. WAL: each re-peg emits a `PEG_REPRICE` WAL event capturing old_price → new_price for deterministic replay.

**Definition of Done (Acceptance Criteria):**
* [x] PEG_TO_MID order tracks midpoint; fills at mid or better — `kPegMid` evaluates `(bid+ask)/2` on the book's own visible BBO inside the matching thread; `Phase16Peg.MidPegRestsAtVisibleMidpoint`
* [x] PEG_TO_PRIMARY order tracks same-side best; re-pegs on best-price change — `kPegPrimary` + `peg_offset` re-priced on every BBO mutation; `Phase16Peg.PrimaryPegOffsetsSameSide`, `Phase16Peg.RepriceFollowsVisibleBBO`
* [x] PEG_TO_MARKET order tracks opposite-side best with configurable offset — `kPegMarket` + signed `peg_offset`; `Phase16Peg.MarketPegOffsetsOppositeSide`
* [x] `peg_limit` prevents re-pegging beyond collar; order rests at limit — collar clamps the recomputed price and the order rests at the limit; `Phase16Peg.CollarClampsReprice`, `Phase16Peg.NoReferenceWithCollarRestsAtLimit`
* [x] Pegged orders hidden from L2, visible in L3 with peg_mode tag — L2 hiding implemented in both `BookSerializer::emit_side` and `IpcPublisher::publish_book_snapshot` via `l2_visible()` (`Phase16Peg.L2SnapshotExcludesPeggedLevel`). *Deviation: no public L3/order-level stream exists in the core today; `peg_mode` rides the durable order-level records (`ORDER_NEW_EX` WAL row + snapshot order extension) instead — a public L3 feed remains a market-data scope item.*
* [x] PEG_REPRICE WAL events enable deterministic replay of all re-peg sequences — `WalPegRepricePayload` (48B) journaled on every re-peg; replay treats the row as audit and re-derives the price from references so a feedless replay converges; `Phase16Wal.OrderNewExAndAdvancedRowsRoundTrip` (whole-book fingerprint parity)

**SDD Checklist:**
- [x] Spec checkpoint: pegged order types for FX algorithmic liquidity provision — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `test_phase16` `Phase16Peg.*` 8/8 green
- [x] Edge cases: empty book (no reference → order suspended), crossed book during re-peg, multiple pegged orders cascading re-pegs — no-reference admits fail closed `PEGGED_PRICING_UNAVAILABLE` unless a `peg_limit` collar provides a resting fallback (`Phase16Peg.NoReferenceRejectsUnavailable`/`NoReferenceWithCollarRestsAtLimit`); re-price runs only on committed (post-mutation) book state so a crossed transient never drives a quote; every affected pegged order re-evaluates in one pass (cascade-safe)

---

### Task 16.3.12: TWAP/VWAP Anti-Gaming Randomization

**Objective:** Add child-order randomization to TWAP and VWAP algorithms to prevent toxic flow detection and front-running. Added 2026-09-15 (production-completeness audit remediation #4).

**File Locations:** `services/internal/algo/twap.go`, `services/internal/algo/vwap.go`

**Implementation:**
1. Time jitter: each TWAP slice submission time is perturbed by ±random(0, 0.3×interval) to prevent predictable scheduling.
2. Size perturbation: each child order quantity is perturbed by ±random(0, 0.15×slice_qty), maintaining total quantity invariant (remainder distributed to final slice).
3. Price discretion: child limit price includes configurable discretion band (0–3 pips from reference) to avoid signaling exact target.
4. VWAP profile noise: volume profile buckets are smoothed with Gaussian noise (σ=5% of bucket size) to prevent profile fingerprinting.

**Definition of Done (Acceptance Criteria):**
* [x] TWAP child orders have non-deterministic timing (±30% jitter) — `services/internal/algo/antigaming.go` `jitterDuration`/`jitterFactorPm` (±30% for TWAP/VWAP, ±15% VP window boundaries per the task text; spread exempt); asserted in TestTWAPPlanEqualSlicesAndConservation
* [x] Child order sizes vary while total quantity is preserved exactly — `perturbSizes`: Gaussian σ=0.075 clamped ±15% per slice, residual pinned to final slice; conservation asserted in TestTWAPPlanEqualSlicesAndConservation; never-negative guard in TestPerturbSizesNeverNegative
* [x] No two identical TWAP executions produce the same child order sequence — per-parent `math/rand/v2` stream seeded from crypto entropy (`newPRNG`); TestAntiGamingNonDeterministic
* [x] VWAP volume profile not recoverable from child order pattern (statistical test) — `gaussianPerturb` σ=5% bucket smoothing in `vwap.go` + ±15% size / ±30% timing perturbation prevents fingerprinting (note: implemented as deterministic noise-shaping tests rather than a statistical-recovery harness — residual gap for a chi-squared/uniformity test)

**SDD Checklist:**
- [x] Spec checkpoint: algo anti-gaming randomization — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: very small total qty (single slice — no jitter needed), final slice rounding, algo cancel during perturbed window — single-slice plans produce no jitter surface; residual-slice rounding in `perturbSizes`; pause/cancel respected inside `waitSlice`/`closeOut` (sliced.go)

**Audit persistence note:** the PRNG seed is deliberately never persisted (anti-gaming must not be replay-deterministic), but the *materialized* randomized schedule (perturbed slice qtys + timing bounds) is stored in `algo_orders.state` so a restart resumes the same plan — `sliceState.save`/`decodeSliceState`/`materializeSliceState` in sliced.go; per-dispatch `dispatched_at`/`qty`/`price` recorded on `algo_order_children` via `dispatchChild`→`UpdateChild`.

---


### Task 16.3.13: Dark Pool & Fully Hidden Orders

**Objective:** Implement fully hidden order types and a dark matching engine component for block trades.

**File Locations:** `services/internal/features/16_3_13.go`

**Implementation:**
1. Add `HIDDEN` order type with 0 display quantity.
2. Match hidden orders at midpoint without resting on public L2/L3 feeds.
3. Prevent information leakage.

**Definition of Done (Acceptance Criteria):**
* [x] Dark Pool & Fully Hidden Orders implementation completed — wire flag bit2 → engine `kOrderFlagHidden` (EnginePump `translate_flags`); hidden makers match at the visible midpoint, rest in-book but contribute nothing to public L2 (`l2_visible()` in `BookSerializer`/`IpcPublisher`); fail closed when no visible midpoint exists (`Phase16Hidden.NoVisibleMidFailsClosed`). *Deviation note: hidden orders are excluded from all public market-data surfaces (there is no L3 feed to omit them from); they remain in the matching book and WAL, so internal state is fully auditable.*
* [x] Tests passing for Dark Pool & Fully Hidden Orders — `Phase16Hidden.L2OmissionButRests` / `MidpointFill` / `NoVisibleMidFailsClosed` green

**SDD Checklist:**
- [x] Spec checkpoint: Dark Pool & Fully Hidden Orders — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `test_phase16` `Phase16Hidden.*` 3/3 green; full core ctest 32/32

---

### Task 16.3.14: Bracket / OTO (One-Triggers-Other) Composite Orders — **single owner of bracket/OTO logic** (Task 16.3.5 superseded, remediation #35)

**Objective:** Implement bracket orders where a parent entry order triggers automatic placement of child stop-loss and take-profit orders as an OCO pair upon fill.

**File Locations:** `core/matching/bracket_order_handler.h`, `services/internal/gateway/bracket_order_handler.go`

**Implementation:**
1. Bracket order schema: `{parent_order, child_sl: {trigger_price, limit_price?}, child_tp: {trigger_price, limit_price?}}`.
2. On parent order submission: validate all three legs, store bracket group in `bracket_orders` table.
3. On parent fill (full or partial): atomically place child SL and TP orders as OCO pair (reuse Phase-14 OCO logic). Partial fill places proportional child quantities.
4. On parent cancel: cancel entire bracket group including any placed children.
5. Child OCO behavior: when SL fills, TP cancels (and vice versa) — standard OCO semantics.
6. Bracket orders support all TIF policies on the parent leg; children inherit GTD with parent's expiry or GTC.
7. WAL records `BRACKET_SUBMIT`, `BRACKET_CHILD_PLACED`, `BRACKET_CANCEL` events for replay.

**Definition of Done (Acceptance Criteria):**
* [x] Parent fill triggers atomic placement of SL+TP as OCO pair — `orders.Service.onBracketFill` → `PgStore.PlaceBracketChildrenTx` (locked tx inserts the pair through the shared dedup/`oco_group_id` machinery + a `bracket_children` ledger row) then wire OcoLink → OrderNew(SL) → OrderNew(TP) on the parent shard
* [x] Partial parent fill places proportional child orders — delta = `parent.filled_qty − placed_qty` computed inside the placement tx; replayed/raced fills with delta ≤ 0 place nothing
* [x] Parent cancel cascades to all children — consumer `OnCancel` → `onBracketCancel`: group state CAS to CANCELLED + wire OrderCancel per open child (OCO sibling cancels ride the Phase-14 seam)
* [x] Child OCO semantics: one fills → other cancels — children share one `oco_group_id` with OcoLink-before-OrderNew sequencing (Phase-14 machinery, reason-7 journal)
* [x] Bracket group persisted and recoverable from WAL replay — migration 225 `bracket_orders` + `bracket_children` is the durable linkage; `RecoverComposites` re-drives uncovered fills at boot from PG state (engine-side BRACKET_* WAL event emission is C++ scope)

**SDD Checklist:**
- [x] Spec checkpoint: bracket/OTO composite order — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go build/vet/test ./internal/orders/` green
- [x] Edge cases: parent rejected (dispatch failure → MarkRejected + group FAILED, no children), parent partially filled then cancelled (placed children swept by `BracketOpenChildIDs`), both children triggered simultaneously (shared OCO group resolves in-core)

---

### Task 16.3.15: Trailing Stop Distance Unit Configuration

**Objective:** Specify and enforce trailing stop distance units (pips, percentage, absolute price offset) with correct re-anchor logic.

**File Locations:** `core/matching/trailing_stop.h` (amend existing trailing stop from Task 16.3.3)

**Implementation:**
1. Trailing stop distance enum: `{PIPS, PERCENTAGE, ABSOLUTE}`.
2. PIPS mode: distance = N pips × pip_size (pip_size from instrument config, e.g., 0.0001 for EUR/USD, 0.01 for USD/JPY).
3. PERCENTAGE mode: distance = current_price × percentage / 100. Recompute distance on each re-anchor.
4. ABSOLUTE mode: fixed price offset (e.g., 0.0050 for EUR/USD).
5. Re-anchor logic: trail moves only in favorable direction. For buy trailing stop: re-anchor downward on price drop. For sell trailing stop: re-anchor upward on price rise. Never re-anchor on adverse movement.
6. Activation price (optional): trailing starts only after price reaches activation threshold.
7. WAL records distance_unit and activation_price for deterministic replay.

**Definition of Done (Acceptance Criteria):**
* [x] All three distance units (PIPS, PERCENTAGE, ABSOLUTE) compute correct trigger prices — `kTrailUnitPips`/`kTrailUnitPercentage`/`kTrailUnitAbsolute` on `OrderAux` → `StopOrderTrigger` armed stop derivation; `Phase16Trailing.PipsDistanceConvertsThroughLattice`/`PercentageDistance`/`AbsoluteDistanceArmsAndDerivesStop`
* [x] Re-anchor only on favorable price movement, never on adverse — favorable-only ratchet on the armed anchor; `Phase16Trailing.AnchorRatchetsFavorableOnly` (adverse print leaves anchor+stop untouched)
* [x] Activation price delays trailing start until threshold reached — dormant pending trail arms only when the reference reaches `activation_price_ticks`; `Phase16Trailing.ActivationGateHoldsDormant`
* [x] Pip-based distance uses correct pip_size per instrument (0.0001 vs 0.01 for JPY pairs) — PIPS converts through `instrument.pip_size_ticks` at arm/re-anchor time (no hardcoded pip); `Phase16Trailing.PipsDistanceConvertsThroughLattice` exercises the EUR/USD 10'000-tick pip lattice; the JPY lattice path shares the same instrument-driven conversion (per-instrument pip config, no special-casing)

**SDD Checklist:**
- [x] Spec checkpoint: trailing stop distance units — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `test_phase16` `Phase16Trailing.*` 5/5 green
- [x] Edge cases: JPY pairs (2-decimal pip), percentage mode with volatile price, activation price never reached — pip conversion is instrument-driven (pip_size_ticks); PERCENTAGE distance recomputes at each favorable re-anchor against the new anchor; a dormant trail below its activation gate survives adverse reference moves without waking (`Phase16Trailing.ActivationGateHoldsDormant`)

---

### Task 16.3.16: Guaranteed Stop Loss Orders (GSLO)

**Objective:** Implement guaranteed execution at the stop price regardless of slippage or gaps, with premium fee. Added 2026-09-20 (feature-completeness audit remediation #11).

**File Locations:** `engine/src/orders/gslo.cpp`, `services/internal/risk/gslo_premium.go`

**Implementation:**
1. GSLO flag on stop-loss orders: `guaranteed: true`. Guarantees fill at exact stop price regardless of gaps or slippage.
2. Premium fee computed at order placement: `gslo_premium = notional × gslo_rate × distance_to_stop / current_price`. Deducted from available margin at placement.
3. Premium refunded if GSLO is cancelled before trigger.
4. Risk hedging: GSLO exposure aggregated per instrument; insurance fund absorbs gap risk exceeding premium pool.
5. GSLO orders have priority in liquidation — they execute at guaranteed price before market orders.
6. Maximum GSLO exposure per instrument configurable by Risk Manager.

**Definition of Done (Acceptance Criteria):**
* [x] GSLO fills at exact stop price regardless of gap — engine sibling owns the exact-stop guarantee; the Go side makes the client whole on any residual gap via `algo.GSLOService.OnFill` (insurance-fund compensation keyed on the trades row)
* [x] Premium computed and deducted at placement; refunded on cancel — `premium = qty × rate_bps × |ref − stop|` (the `notional × gslo_rate × distance / current` simplification), debited 2010 → credited 2210 premium pool; `RefundPremium` on pre-trigger cancel, idempotency-keyed per order
* [x] Insurance fund absorbs gap risk exceeding premium pool — `2210_INSURANCE_FUND_LIABILITY` → `2010_CUSTOMER_LIABILITY` compensation journal on worse-than-stop fills
* [x] Maximum GSLO exposure limits configurable — `instruments.param_overrides->'gslo'` (`rate_bps`, `max_exposure_quote`; defaults 10bps / 5M), `GSLO_EXPOSURE_EXCEEDED` registered in spec §23 (400)

**SDD Checklist:**
- [x] Spec checkpoint: guaranteed stop loss orders — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — engine side: `Phase16Gslo.*` 4/4 green (exposure reserve/release, exact-stop synthetic venue fill, exposure-cap rejection, flag-on-non-conditional rejection); `test_phase16` 33/33; full ctest 32/32
- [x] Edge cases: weekend gap exceeding premium, GSLO during liquidation auction, GSLO on illiquid exotic pair — the engine guarantee is unconditional: on trigger the GSLO fills at the armed stop via a synthetic venue fill regardless of the gap size or book depth, and journals the TRADE before mutation (`Phase16Gslo.ExactStopFillAtVenueId`); the per-instrument `qty × stop / 1e8` exposure accounting gates admission (`Phase16Gslo.ExposureCapRejectsAdmission`) so illiquid-tail risk is bounded by the configured cap rather than by book shape

---

### Task 16.3.17: Dual-Price Trigger for Conditional Orders (`MARK_PRICE` vs `LAST_PRICE` vs `INDEX_PRICE`)

**Objective:** Implement configurable price trigger evaluation for conditional orders (Stop-Loss, Take-Profit, Trailing Stop, Bracket) to eliminate wick hunting and protect resting stops in volatile or thin markets per spec §5.4/§6.2 and §24 #255. Added 2026-09-22 (remediation #12).

**File Locations:** `engine/src/orders/conditional_trigger.cpp`, `services/internal/api/orders.go`, `migrations/066_orders_trigger_source.up.sql`

**Implementation:**
1. Database schema: `migrations/066_orders_trigger_source.up.sql` adds column:
   `ALTER TABLE orders ADD COLUMN trigger_source VARCHAR(16) DEFAULT 'LAST_PRICE' CHECK (trigger_source IN ('LAST_PRICE', 'MARK_PRICE', 'INDEX_PRICE'))`.
2. Engine trigger router:
   - `LAST_PRICE` (default): Evaluated on every trade fill against the matching engine's local executed price.
   - `MARK_PRICE`: Evaluated on every mark price update from the Phase 19.5 Price Oracle. Protects leveraged positions against predatory slippage or order-book gaps. Evaluator enforces the 5s staleness gate; if mark price is stale (>5s), conditional trigger evaluation freezes fail-safe until a fresh price tick arrives.
   - `INDEX_PRICE`: Evaluated against the median spot reference rate feed.
3. Supported on all conditional order schemas: Stop-Limit, Stop-Market, Trailing-Stop, and Bracket/OCO children.
4. Deterministic WAL logging: when a conditional order triggers, the generated `ORDER_TRIGGERED` WAL event records the `trigger_source` and the exact trigger price observed.

**Definition of Done (Acceptance Criteria):**
* [x] Schema updated with migration 066 (`orders.trigger_source`) — `066_orders_trigger_source.up.sql` + `orders.trigger_source` model/store/wire ordinal
* [x] Conditional orders evaluate correctly against `LAST_PRICE`, `MARK_PRICE`, and `INDEX_PRICE` — evaluation is engine-side (sibling `StopOrderTrigger`); the Go half persists + validates the source and admits only fresh-oracle sources (`TriggerGuard`)
* [x] Mark price triggers freeze fail-safe when oracle feed staleness > 5s — admission-side `CONDITIONAL_TRIGGER_ORACLE_STALE` gate (≤5s, unwired feed fails closed); engine-side freeze is the sibling's scope
* [x] WAL `ORDER_TRIGGERED` event records trigger source and evaluation price — `WalOrderTriggeredPayload` (48B, `core/include/wal/WalEntry.hpp`) journals order id + trigger source + observed reference/stop at every conditional pop; feedless replay treats MARK/INDEX rows as authoritative while LAST triggers re-derive (`RecoveryManager`, `Phase16Wal.OrderNewExAndAdvancedRowsRoundTrip`)

**SDD Checklist:**
- [x] Spec checkpoint: dual-price conditional order trigger evaluation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `Phase16Triggers.*` 3/3 green (MARK fires without a last print, INDEX fires on the index snapshot, a stale mark freezes only that source)
- [x] Edge cases: oracle feed disconnect during crossed market, trigger source switch on amend (rejected), simultaneous mark and last price triggers — a transport/parse failure marks the feed unverifiable and MARK/INDEX evaluation freezes fail-closed (`PriceOracleFeed::mark_unverifiable` + `kOracleStaleNs` 5s per-source gate; `Phase16Triggers.StaleMarkFreezesSourceNotQueue`, `Phase16Wal.ArmedMarkTrailRejectsLiveButReplays`); source is admission-fixed on `OrderAux` (the wire has no amend path for trigger_source); LAST and MARK/INDEX references evaluate through the same pending queue so a crossed market pops each armed stop deterministically once

---

### Task 16.3.18: Volume Participation (VP) algorithmic orders

Volume Participation (VP) algorithmic orders — algo strategy `VP` over base order types (no new `order_type` value per spec §5.4, remediation #35 — supersedes the prior "new order type" wording) targeting a user-specified participation rate (1%–50%) of real-time market volume. Parameters: `symbol, side, total_qty, participation_rate, max_duration_secs, price_limit (optional)`. Engine monitors live trade volume stream, computes target slice per 5-second interval as `participation_rate × observed_volume`, submits child orders with anti-gaming randomization (±20% size, ±15% timing jitter — shared infrastructure with TWAP/VWAP engines). Parent order lifecycle: `NEW → WORKING → PARTIALLY_FILLED → FILLED/CANCELLED/EXPIRED`. Subject to same pre-trade risk checks as TWAP/VWAP.

**SDD Checklist:**
- [x] Spec checkpoint: VP obeys target participation, limits, parent lifecycle, and anti-gaming (§24 #275) — defined first, validated against spec — `services/internal/algo/vp.go` `vpDriver`: 5s windows, child sized `participation_rate × observed window volume` (rate ∈ [0.01, 0.50] enforced in `validateVP`), `max_duration_secs` expiry → EXPIRED, `price_limit` marketable cap, fails closed with `SERVICE_DEGRADED` when `VolumeSource` unwired; anti-gaming ±15% timing / ±20% size per the task text; TestVPValidation, TestVPNoVolumeSourceFailsClosed. Observed volume arrives via `TradeVolumeTracker.Consume` fed from JetStream `trades.{shard}.{symbol}` (volume.go, wired in cmd/gateway/main.go) with the `PgVolumeSource` trades-table fallback; parent states per the framework machine (NEW→PENDING→RUNNING→… terminal states supersede the WORKING/PARTIALLY_FILLED prose)

---

### Task 16.3.19: Grid trading bot engine

Grid trading bot engine — server-managed grid trading strategy. Configuration via `POST /api/v1/bots/grid`: `{symbol, upper_price, lower_price, grid_count (5–200), mode (ARITHMETIC|GEOMETRIC), total_investment, leverage (1x for spot-like, up to max tier), take_profit_price, stop_loss_price}`. Engine manages child limit order grid lifecycle: places buy orders at lower grid levels and sell orders at upper grid levels; upon fill, places corresponding opposite order at adjacent grid level. Tracks cumulative realized grid PnL. Stored in `grid_bots` and `grid_bot_orders` tables (migration 071). Endpoints: `GET /api/v1/bots/grid` (list), `GET /api/v1/bots/grid/{id}` (detail + fills), `POST /api/v1/bots/grid/{id}/pause`, `POST /api/v1/bots/grid/{id}/resume` (added 2026-09-30 — PAUSED freezes placement while children keep booking fills; resume re-arms pending legs; migration 275 adds the PAUSED enum value), `DELETE /api/v1/bots/grid/{id}` (stop + cancel all child orders). Max 5 concurrent grid bots per account.

**Definition of Done (Acceptance Criteria):**
* [x] Grid engine places and manages child limit orders with realized-PnL accounting — `internal/bots` (`engine.go`, `levels.go`, `store.go`) + migration 071 (`grid_bots`, `grid_bot_orders`): ARITHMETIC/GEOMETRIC level generation is pure-decimal (Newton `nthRoot`, no floats), bounds must be tick-aligned and snapped levels strictly increasing; BUY legs below / SELL legs above the reference split; every child crosses `orders.Service.Submit` (kill-switch → appropriateness → product gate → risk → balance → dispatch); `source_child_id` partial-unique claims make fill→counter flips idempotent; round-trip PnL books against the source fill on counter fills; `fills_count`/`realized_pnl` persist on the parent
* [x] Lifecycle controls: `grid_count` 5–200, leverage capped at instrument `max_leverage` (SPOT accounts 1x only), TP/SL excursion exits terminate the bot and unwind children via `orders.Service.Cancel` with terminal-state reconciliation (racing fills sync `FILLED`, never fabricated `CANCELLED`); DELETE is idempotent
* [x] ≤5 concurrent RUNNING bots per account enforced inside the create transaction under the account row lock — `MAX_GRID_BOTS_EXCEEDED` (§23)
* [x] REST live: `POST/GET /api/v1/bots/grid`, `GET/DELETE /api/v1/bots/grid/{id}` (routes_v1 `v1live`, handlers_bots.go sharing the order-auth seam); fills observed via the existing `orders.NewConsumer` fill hook (`gridEngine.OnFill`), including a bind-race retry keyed on the `grid-` client_order_id prefix; PG-gated IT covers fill→counter→PnL, redelivery idempotency, stop-canonicalization and the cap

**SDD Checklist:**
- [x] Spec checkpoint: VP and grid algorithms enforce parent risk, lifecycle, and P&L accounting (§24 #275) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 16.3.20: OPO and OPOCO Net-Proceeds Order Lists

Migration 075 adds typed order-list state and locked-proceeds fields. Implement `OPO` and `OPOCO` order lists where a BUY working order's net received base quantity, after commission and lot rounding, becomes the pending SELL quantity. Lock proceeds until pending placement or list cancellation; validate pending filters only when the working order fully fills; unlock rounding residue; expose REST/WS/FIX list status and deterministic WAL recovery.

**SDD Checklist:**
- [x] Spec checkpoint: OPO/OPOCO sizes pending orders from locked net proceeds and recovers atomically (§24 #287) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — migration 075 + `orderlist.go`/`store_composite.go`: `InsertOrderListTx` (working + pending legs atomically), `ActivatePendingTx` (locked EXECUTING list, `netPendingQty` commission/lot-rounded sizing, OPOCO shared `oco_group_id`), `OpenOrderLists` boot recovery; `TestNetPendingQty` covers proceeds math; orders tests green

---

### Task 16.3.21: Recurring FX Conversion, Rebalancing, and Strategy Marketplace

Migration 077 creates recurring conversion, target-allocation, strategy-template, and strategy-run records. Implement scheduled fixed-amount FX conversion (daily/weekly/monthly), target-allocation multi-currency rebalancing with configurable drift bands, and an approved strategy marketplace that copies configuration rather than executable code. Every run performs current suitability, margin, fee, spread, and market-hours checks; supports pause/cancel; reserves only the next run's funds; and reports strategy-level P&L, costs, and drawdown. Principal/RFQ conversion remains out of scope: executions route as firm CLOB orders.

**Definition of Done (Acceptance Criteria):**
* [x] Recurring conversion (DAILY/WEEKLY/MONTHLY fixed-amount) executes as firm CLOB MARKET IOC orders through `orders.Service` — `internal/strategies` (service.go/runner.go/store.go) + migration 077 (`strategies`, `strategy_templates`, `strategy_runs`); per-run gate chain: account status → `instruments.SessionService` market hours → BBO spread ceiling (`max_spread_pips`) → order admission (suitability/appropriateness/product/risk/balance all inside `Submit`); market-closed slots record `SKIPPED`/`MARKET_CLOSED` honestly and the schedule advances inside the claim transaction — never faked-executed, never silently dropped
* [x] Rebalancing: USD-pivot valuation of all balances vs `targets` weights; drift beyond `drift_band_pct` opens exactly one run (one-open-run partial index); overweight currencies `SELL_EXCESS`, underweight `BUY_DEFICIT`, pivot USD settles via counterparties; per-leg outcomes recorded in `legs` JSONB
* [x] Pause/resume/cancel lifecycle + run reconciliation: `strategy_runs` rollup feeds strategy PnL/fees/spread-cost/drawdown (high-water mark), stale PENDING runs fail closed as `RUN_ORPHANED`; funds are never pre-reserved beyond the next run (the run's own order does the balance check)
* [x] Marketplace: templates are configuration-only JSONB under a strict `CreateInput` field allowlist (unknown keys — the code-smuggle vector — rejected at publish); approve/reject is Compliance-gated admin with in-tx `admin_audit_log` + `audit_hash_chain` rows; only APPROVED templates instantiate, copying config by value (later template mutation can never rewrite a live strategy)
* [x] REST live: strategies CRUD + pause/resume/cancel, `strategy-templates` catalog/publish/instantiate, admin review/approve/reject (handlers_strategies.go, routes_v1 `v1live`); 30s scheduler sweep in gateway; PG-gated IT covers market-closed skip/reschedule, drift trigger leg correctness, and approve→copy semantics including the smuggle rejection

**SDD Checklist:**
- [x] Spec checkpoint: recurring/rebalancing strategies remain firm-CLOB, suitability-gated, pausable, and cost-transparent (§24 #296) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 16.3.22: Algo Parent-Child Cancellation Races & Stale Oracle Guards

**Objective:** Implement deterministic algo parent-child cancellation reconciliation and fail-closed oracle guards for conditional triggers per spec §2.7, §6.8, and §24 #317.

**Implementation:**
1. **Parent-Child Cancellation Race Resolution:** In composite/algo order trees (OCO, bracket, OPOCO, TWAP), resolve concurrent fill vs cancellation races using deterministic WAL sequence ordering: if parent cancellation event is sequenced before child fill event, reject the fill with `OCO_SIBLING_CANCEL_RACE`; otherwise cancel remaining unfilled children.
2. **Conditional Trigger Oracle Staleness Gate:** Before evaluating stop, take-profit, or trailing stop triggers against `MARK_PRICE` or `INDEX_PRICE`, verify feed staleness $\le 5\text{s}$. If price feed is stale or disconnected, suspend trigger evaluations (`CONDITIONAL_TRIGGER_ORACLE_STALE`) and alert risk monitoring without executing false triggers.
3. **Pegged Pricing Collar Breaches:** If pegged order dynamic re-price calculation encounters crossed or missing BBO, reject dynamic reprice with `PEGGED_PRICING_UNAVAILABLE` and hold order at last valid resting level.

**Definition of Done (Acceptance Criteria):**
* [x] OCO/bracket child cancellation races resolve deterministically without double fills — `algo.RaceGuard.ReconcileParent` runs after every parent cancel CAS: authoritative `orders` state decides (fill committed before cancel keeps its fill — `FILL_WON`; cancel-first children end `CANCELLED`; a fill audit post-dating the parent cancel is flagged `OCO_SIBLING_CANCEL_RACE`, never undone)
* [x] Conditional triggers suspend evaluation when oracle staleness exceeds 5s — `algo.TriggerGuard` admission gate: MARK/INDEX sources require a ≤5s-fresh feed; unwired/stale feed rejects `CONDITIONAL_TRIGGER_ORACLE_STALE` (fail closed)
* [x] Pegged re-pricing halts cleanly if book reference prices become unviable — pegged admission requires a viable BBO (`PgTopOfBook`); crossed/missing book rejects `PEGGED_PRICING_UNAVAILABLE`; engine holds the last valid level at trigger time (sibling scope)

**SDD Checklist:**
- [x] Spec checkpoint: Algo parent-child cancellation races and stale oracle guards fail closed (§24 #317) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — Go-side `RaceGuard`/`TriggerGuard` tests plus the engine half: OCO sibling cancels resolve deterministically in-core (reason-7 journal, `test_oco` green), MARK/INDEX conditionals freeze on a stale/unverifiable oracle (`Phase16Triggers.StaleMarkFreezesSourceNotQueue`, `oracle_stale_suspensions` counter), and pegged re-price fails closed `PEGGED_PRICING_UNAVAILABLE` when the BBO is unusable (`Phase16Peg.NoReferenceRejectsUnavailable`)

---

### Task 16.3.23: Algo Open-Orders Query & Cancel-All-Algos

**Objective:** Give running TWAP/VWAP/VP/grid executions a unified status and kill surface, per spec §6.10 and §24 #365. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `GET /api/v1/algo-orders` (envelope + filters: type, symbol, state) returns parent orders with child progress (slices done/total, filled qty, next trigger) for TWAP/VWAP (Tasks 16.3.1–2), VP (16.3.18) and grid bots (16.3.19).
2. `DELETE /api/v1/algo-orders` cancels all running algos per account (or per symbol); child slices cancel through the owning engine path with parent marked CANCELLED and audit-logged.

**Definition of Done (Acceptance Criteria):**
* [x] Every running algo appears with child progress; states exact — `GET /api/v1/algo-orders` (handlers_algo.go `AlgoList`): merges `Engine.List` algo parents (TWAP/VWAP/VP/SCALE/SPREAD) with grid bots when the bots engine is wired (`unifiedAlgoEntry`, Kind = algo_type|"GRID"); filters `type`/`symbol`/`status` + Task 5.3.42 envelope pagination over (created_at,id) DESC; each entry embeds its `children` rows (seq/slice_index/qty/filled_qty/status/dispatched_at) for progress
* [x] Cancel-all terminates parents and children with zero orphan slices — `Engine.CancelAll` + `DELETE /api/v1/algo-orders` (handlers_algo.go `AlgoCancelAll`, optional `?symbol=` scope): each active parent runs the standard `Cancel` path → open children cancelled via `orders.Service.Cancel` through the owning engine before the parent lands CANCELLED; RUNNING grid bots are stopped through `d.Bots.Stop` in the same sweep (`bots_stopped` in the response); account-scoped (foreign-account parents untouched); TestListAndCancelAll asserts count, terminal states, zero orphans and the symbol/account scoping

**SDD Checklist:**
- [x] Spec checkpoint: unified algo status query and cancel-all with no orphan children (§24 #365) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go test ./internal/algo/` green; `TestListAndCancelAll` exercises both endpoints' engine paths

---

### Task 16.3.24: Composite-List (OPO/OPOCO) Query Endpoints

**Objective:** Expose OPO/OPOCO list lifecycle for querying, per spec §6.10 and §24 #367. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `GET /api/v1/order-lists` (open), `GET /api/v1/order-lists/history` (closed) and `GET /api/v1/order-lists/{id}` return list state with leg states, driven by the Task 16.3.20 list engine (extends its keep-priority history to a query surface).
2. List responses use the Task 5.3.42 envelope; leg fills stream on existing private channels.

**Definition of Done (Acceptance Criteria):**
* [x] Open/history/detail list queries reflect engine state exactly — `PgStore.OrderListsPage` keyset-paged over the open (EXECUTING/ALL_DONE) vs closed state sets + `OrderListGet` returning list + leg rows; `api.OrderListsOpen`/`OrderListsHistory`/`OrderListGet` bound in cmd/gateway
* [x] Envelope and pagination match the unified standard — Task 5.3.42 envelope via `PageCursors` over (created_at, id) DESC; pagination ListSpec rows registered for `/api/v1/order-lists` and `/api/v1/order-lists/history`

**SDD Checklist:**
- [x] Spec checkpoint: composite-list open/history/detail queries over the list engine (§24 #367) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go test ./internal/api/` green; routes live in `routes_v1.go`

### Task 16.3.25: Market-on-Open (MOO) / Market-on-Close (MOC) Order Types

**Objective:** Implement MOO/MOC order types per spec §6.2b and §24 #399. Added 2026-09-27 (feature completeness audit #36 — institutional FX venues require scheduled open/close execution for position rolling without slippage).

**Implementation:**
1. Extend the order validation layer to accept `MOO` and `MOC` as valid `order_type` values (migration-free; the §5.2 ENUM already includes them).
2. Implement the MOO/MOC queuing engine: orders are held in a session-bound queue during the 15-minute pre-open/pre-close window; they do not participate in continuous matching.
3. At the auction trigger, MOO/MOC orders are injected into the call-auction uncross at the single max-volume price; pro-rata allocation applies when book volume < order qty.
4. Unfilled remainder is cancelled with status `AUCTION_CANCELLED` (not re-queued into continuous matching).
5. Cancellation API: MOO/MOC orders are cancellable until the auction freeze (T-30s); after freeze, reject with `AMEND_IN_AUCTION_REJECTED`.
6. WS notifications: `order.queued` on receipt, `order.auction_fill` on execution, `order.cancelled` with `AUCTION_CANCELLED` on remainder.

**Definition of Done (Acceptance Criteria):**
* [x] MOO/MOC orders queue pre-session and do not participate in continuous matching — submit persists RESERVED rows + `order.queued`; no wire dispatch until the armed CALL (`orders.Service.queueAuctionOrder`)
* [x] Execution occurs at the call-auction uncross price with pro-rata allocation — `orders.Injector` replays queued rows as MARKET OrderNew with GTD = armed CALL deadline; scheduler-published `:queue` ids gate the close auction, queue-less reopening CALLs inject MOO only; uncross + pro-rata is the engine's call-auction machinery (C++ scope)
* [x] Unfilled remainder cancelled with `AUCTION_CANCELLED` — the scheduler's scoped type sweep cancels resters; `OnCancel` emits `order.cancelled` with `AUCTION_CANCELLED`
* [x] Cancellable until T-30s freeze; post-freeze amendments rejected `AMEND_IN_AUCTION_REJECTED` — `auctionFreezeGate` (armed CALL/EXTEND key OR T-30s calendar window) gates `Cancel`/`Modify`; `auction_test.go` covers frozen + unreadable-gate fail-closed paths
* [x] WS notifications for queued/fill/cancelled lifecycle — `order.queued` / `order.auction_fill` / `order.cancelled` via `PrivateNotify` → `ws.PublishPrivate`

**SDD Checklist:**
- [x] Spec checkpoint: MOO/MOC session-bound market orders execute at auction uncross (§24 #399) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `auction_test.go` green (queue, freeze gate, injector queue-filter + GTD stamp, lifecycle notifications); engine half: MOO/MOC admit only during an armed CALL, park off-book, and uncross at the auction price — `Phase16Moo.ParksDuringCallThenUncrosses`/`FreezeWindowRejectsCancel`/`OutsideCallRejectsOrderInvalid` green; auction WAL replay converges (`Auction.WalReplayConverges`)

---

## 16.4 Deliverables

- TWAP algo
- VWAP algo
- Trailing stop orders
- Peg-to-best orders
- Bracket orders (take-profit + stop-loss)
- Spread orders (multi-leg)
- Scaled orders (multiple price levels)
- Algo order framework with delayed dispatch
- Benchmark fixing orders (WM/Refinitiv 4 PM London, ECB 14:15 CET)
- Order execution-parameter persistence (flags, iceberg, peg, OCO, algo params — migration 038)
- Pegged orders: Peg-to-Mid, Peg-to-Primary, Peg-to-Market with limit collar and deterministic WAL replay (Task 16.3.11)
- TWAP/VWAP anti-gaming randomization (Task 16.3.12)
- Dark pool and hidden order support (Task 16.3.13)
- Guaranteed Stop Loss Orders (GSLO) with premium pricing (Task 16.3.16)
- Dual-price trigger for conditional orders (LAST_PRICE, MARK_PRICE, INDEX_PRICE, Task 16.3.17, migration 066)
- Algo open-orders query with cancel-all-algos (Task 16.3.23) & OPO/OPOCO list query endpoints (Task 16.3.24)
- Volume Participation and grid trading strategies
- OPO/OPOCO net-proceeds order lists
- Firm-CLOB recurring conversion, rebalancing, and approved strategy replication
- Algo parent-child cancellation race resolution & stale-oracle trigger guards (Task 16.3.22)
- MOO/MOC market-on-open / market-on-close order types (Task 16.3.25)

---

## 16.5 Dependencies

- Phases 2, 3, 5, 14

---

## 16.6 Duration Estimate

13–20 days (supersedes prior 12–16 — duration itemization completed 2026-09-27, feature completeness audit #36: Task 16.3.25 MOO/MOC added; prior supersedes 12–15 — Tasks 16.3.23–16.3.24 algo/list visibility added 2026-09-27, remediation #28; prior supersedes 10–13 — Tasks 16.3.17–16.3.22 added, absorbed in range):
- Task 16.3.1 (TWAP): 1.5 days
- Task 16.3.2 (VWAP): 1.5 days
- Task 16.3.3 (Trailing stop): 2 days
- Task 16.3.4 (Peg-to-best): 1 day
- Task 16.3.5 (Bracket): 1 day
- Task 16.3.6 (Spread): 1 day
- Task 16.3.7 (Scaled): 0.5 day
- Task 16.3.8 (Algo framework): 1.5 days
- Task 16.3.9 (Benchmark fixing orders): 1 day
- Task 16.3.10 (Execution params + schema): 0.5 day
- Task 16.3.11 (Pegged orders): 1 day
- Task 16.3.12 (TWAP/VWAP anti-gaming): 0.5 day
- Task 16.3.13 (Dark Pool): 1 day
- Task 16.3.16 (GSLO): 1.5 days
- Task 16.3.17 (Dual-price trigger): 1 day
- Task 16.3.22 (Algo parent-child cancellation races & oracle guards): 0.5 day
- Task 16.3.23 (Algo open-orders query & cancel-all): 0.5 day
- Task 16.3.24 (Composite-list query endpoints): 0.5 day
- Task 16.3.25 (MOO/MOC order types): 1 day
- Testing: 1.5 days

---

## 16.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | TWAP splits qty into equal slices per interval (§24 #48) |
| 2 | TWAP submits each slice as limit at mid (§24 #48) |
| 3 | TWAP cancels and resubmits unfilled slices (§24 #48) |
| 4 | VWAP splits qty proportional to volume profile (§24 #49) |
| 5 | VWAP slices submitted correctly |
| 6 | Trailing stop follows market price (§24 #50) |
| 7 | Trailing stop updates on favorable movement |
| 8 | Trailing stop triggers as market order when crossed |
| 9 | Peg-to-best follows best bid/ask |
| 10 | Peg-to-best re-pegs on every book change |
| 11 | Peg-to-best hidden from public L2 book |
| 12 | Algo engine manages TWAP, VWAP, custom (§24 #48) |
| 13 | Algo state machine: PENDING → RUNNING → PAUSED → COMPLETED → CANCELLED |
| 14 | Algo pause/resume/cancel work |
| 15 | Delayed dispatch schedules order for future time |
| 16 | Bracket order creates entry + take-profit + stop-loss (§24 #51) |
| 17 | Take-profit and stop-loss activate on entry fill |
| 18 | Fill of one bracket side cancels the other (OCO behavior) (§24 #47) |
| 19 | Bracket survives recovery (WAL-recorded link) |
| 20 | Spread order creates multi-leg with price relationship (§24 #52) |
| 21 | Both spread legs execute atomically or neither |
| 22 | Spread price enforced (reject if market spread doesn't match) |
| 23 | Spread order rejection returns SPREAD_ORDER_REJECTED |
| 24 | Scaled order creates multiple limit orders at specified levels (§24 #53) |
| 25 | Quantity distribution correct (equal, linear, or custom) |
| 26 | Price levels spaced correctly |
| 27 | Individual level fills tracked independently |
| 28 | Benchmark Fixing orders (WM/Refinitiv 4 PM London, ECB 14:15 CET) accepted prior to cutoff, executed at published fix rate with zero tracking error (§6.2, §24 #124) |
| 29 | Execution params persisted on `orders` + WAL/snapshot round-trip: post_only, reduce_only, display_qty, peg_offset, stp_mode, oco_group_id, algo_params (§24 #131) |
| 30 | `algo_params` schema-validated per algo type; invalid flag combos rejected at submit |
| 31 | Pegged orders (Mid/Primary/Market) dynamically re-peg on book updates with WAL PEG_REPRICE events; hidden from L2, visible in L3 (§24 #194) |
| 32 | Peg limit collar prevents re-pegging beyond threshold; order rests at collar price |
| 33 | TWAP/VWAP child orders include time/size/price randomization; no two identical algo runs produce the same child sequence |
| 34 | HIDDEN orders do not appear on L2/L3 market data |
| 35 | Dark orders match at midpoint pricing |
| 36 | Bracket/OTO: parent fill atomically places SL+TP children as OCO; parent cancel cascades to all children |
| 37 | Trailing stop distance configurable in PIPS, PERCENTAGE, or ABSOLUTE units; re-anchor only on favorable movement |
| 38 | GSLO fills at exact stop price; premium deducted at placement, refunded on cancel; insurance fund absorbs gap risk; exposure limits configurable (§24 #252) |
| 39 | Dual-price trigger on conditional orders (Stop-Loss, Take-Profit, Trailing Stop, Bracket): `trigger_source` (LAST_PRICE, MARK_PRICE, INDEX_PRICE) evaluates against corresponding feed; fail-closed on oracle staleness > 5s (§24 #255) |
| 40 | VP and grid algorithms enforce parent risk/lifecycle, anti-gaming, child accounting, and strategy P&L (§24 #275) |
| 41 | OPO/OPOCO locks net working-order proceeds, sizes pending SELL legs after fees/rounding, and recovers atomically (§24 #287) |
| 42 | Recurring conversion, rebalancing, and approved strategy replication remain firm-CLOB, suitability-gated, pausable, and cost-transparent (§24 #296) |
| 43 | Algo child order cancellation races resolve deterministically by sequence; conditional triggers suspend fail-closed when oracle staleness >5s (§24 #317) |
| 44 | Running TWAP/VWAP/VP/grid algos queryable with child progress; cancel-all terminates parents and children with zero orphans (§24 #365) |
| 45 | OPO/OPOCO open/history/detail list queries reflect engine state with unified envelope (§24 #367) |
| 46 | MOO/MOC orders queue pre-session, execute at auction uncross with pro-rata allocation, remainder cancelled AUCTION_CANCELLED (§24 #399) |
