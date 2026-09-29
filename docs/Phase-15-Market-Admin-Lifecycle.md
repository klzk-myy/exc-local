# Phase 15 — Market Admin Lifecycle

**Duration:** 6–10 days (supersedes prior 6–9 — Task 15.3.13 daily closing-auction calendar added 2026-09-27, feature completeness audit #36; prior supersedes 6–9 — Task 15.3.12 added 2026-09-27, remediation #26; prior supersedes 6–8 — Task 15.3.11 reference/session/tenor added 2026-09-27, remediation #24)
**Dependencies:** Phases 7, 11, 14, 19 (Task 19.3.3 CALL/EXTEND auction machinery — added 2026-09-15 for Task 15.3.6)
**Spec Reference:** §7 (Market Administration Lifecycle), §24 (Acceptance Criteria)

---

## 15.1 Objectives

Implement the seven-state instrument lifecycle: DRAFT → ACTIVE with CANCEL_ONLY / RESTRICTED / SUSPENDED / HALTED controls → DELISTED, with grace periods, admin actions, and audit trail. (Remediation #14 adds persistent CANCEL_ONLY; supersedes the prior six-state model.) (Supersedes prior "PENDING" state name — now uses spec §7.1 canonical "DRAFT". SUSPENDED grace corrected to 5min per spec §7.1; supersedes prior 7d.)

---

## 15.2 Prerequisites

- Phases 7, 11, 14 complete; Phase-19 Task 19.3.3 auction machinery available (added 2026-09-15)

---

## 15.3 Tasks

### Task 15.3.1: Instrument Lifecycle State Machine

**Objective:** Implement the instrument lifecycle state machine.

**File Locations:** `services/internal/admin/instrument_lifecycle.go`

**Implementation:**
1. States: DRAFT → ACTIVE with CANCEL_ONLY / RESTRICTED / SUSPENDED / HALTED controls → DELISTED (seven canonical states; supersedes prior six-state list).
2. DRAFT: instrument created, not tradable.
3. ACTIVE: fully tradable.
4. RESTRICTED: limit orders only — market/stop orders rejected (per spec §7.1; supersedes prior "reduced leverage, no new shorts").
5. SUSPENDED: no new orders (`INSTRUMENT_SUSPENDED`), existing orders cancelled after 5min cancel-only grace window, positions remain.
6. HALTED: no new orders (`INSTRUMENT_HALTED`), existing orders remain (brief halt).
7. DELISTED: no trading (`INSTRUMENT_DELISTED`), positions must close within grace period.
8. Transitions per spec §7.2 matrix: create = Super Admin + dual control; suspend = Compliance Officer+ (no dual control); halt = Risk Manager+ (no dual); resume = Risk Manager+ + dual control; delist = Super Admin + dual control; global halt = Risk Manager+ + dual control. (Supersedes prior "dual control for SUSPEND/DELIST" — spec §7.2 assigns dual control to create/resume/delist/global-halt, not suspend.)
9. Grace periods: SUSPENDED 5min cancel-only window (per spec §7.1); RESTRICTED 24h; DELISTED 30d.
10. Audit: every transition logged with reason, admin ID, timestamp.

**Definition of Done (Acceptance Criteria):**
* [x] All 7 states implemented with correct transitions (CANCEL_ONLY added; supersedes prior 6) — `services/internal/admin/instrument_lifecycle.go` `lifecycleTransitions` + `transitionTx`; TestLifecycleTransitionMatrix, TestInstrumentLifecyclePGTransitions
* [x] DRAFT: instrument created, not tradable — `CreateInTx` inserts DRAFT (no `instrument:status` key by contract); the Go order gate rejects DRAFT with `INSTRUMENT_SUSPENDED`; TestInstrumentLifecycleCreateTxPG
* [x] RESTRICTED: limit orders only, market/stop rejected (spec §7.1) — service owns the state + feed publication; entry gate in `orders/validate.go` (LIMIT_ORDER_REQUIRED) + `core/src/risk/PreTradeChecker.cpp`
* [x] SUSPENDED: no new orders, existing cancelled after 5min grace — `InstrumentService.Sweep` mass-cancels via `InstrumentOrderCanceller` (orders dispatcher — Task 5.3.24 scoped mass cancel); TestInstrumentLifecycleSuspendSweepPG
* [x] HALTED: no new orders, existing remain — state gate `INSTRUMENT_HALTED`; no sweep arm for HALTED
* [x] DELISTED: no trading, grace period for position close — 30d close-only window admits `reduce_only` only (spec §7.1 remediation #35; Go gate `newOrderStateGate(inst, reduceOnly)` mirrors `PreTradeChecker.cpp` flag check)
* [x] Grace periods: SUSPENDED 5min cancel-only (spec §7.1); RESTRICTED 24h; DELISTED 30d — `SuspendedGrace`/`RestrictedGrace`/`DelistedGrace` constants; deadlines surfaced via `grace_deadline` on admin reads + `instrument:lifecycle:sweep_deadline:{id}`; the 5-min SUSPENDED window is sweep-enforced, the 24h/30d windows are displayed/ops-driven (delist stays an operator action)
* [x] Dual control per spec §7.2: create/resume/delist/global-halt require it; suspend does not — `OpInstrumentCreate/Resume/Delist` on the `DualControlService` executor path; TestInstrumentLifecycleDualControlPG
* [x] Every transition audit logged — `admin_audit_log` + `audit_hash_chain` written inside the transition's own tx (`Log` in `transitionTx`); TestInstrumentLifecyclePGTransitions asserts row count + chain links

**SDD Checklist:**
- [x] Spec checkpoint: instrument lifecycle 7 states including persistent CANCEL_ONLY (§24 #290) — defined first, validated against spec
- [x] Spec checkpoint: SUSPENDED 5min cancel-only grace (spec §7.1); RESTRICTED 24h; DELISTED 30d — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go build/vet/test ./internal/admin` green; PG/Redis-gated tests green (EXC_PG_TEST=1 EXC_REDIS_TEST=1)

**Engine-feed contract (Go → C++, documented 2026-09-29 — consumed by Task 15.3.3's refresher and Task 15.3.6's auction manager):**
- `instrument:status:{symbol}` = plain enum word; written for every non-DRAFT instrument on every transition and re-published by the 1s `InstrumentService.Run` reconciler (boot reconciliation included); deleted only when the PG row is purged (delisting purge) or for DRAFT rows.
- `instrument:auction:{symbol}` = `CALL:{deadline_unix_ns}` — written on resume when the reopening CALL applies (HALTED/SUSPENDED→ACTIVE default, RESTRICTED/CANCEL_ONLY→ACTIVE opt-in via `auction:true`, `skip_auction:true` bypasses); deleted on direct resumes and on any transition to a non-ACTIVE state. Dual-approved resumes arm it via `DualControlService.SetOnExecuted` → `InstrumentService.PublishCommitted` (post-commit hook re-deriving side effects from the audit row).
- `instrument:lifecycle:sweep_deadline:{instrument_id}` = unix ms (grace deadline); `instrument:lifecycle:swept:{instrument_id}` = "1" (mass-cancel marker).
- WS: `InstrumentStatusEvent` on public channel `venue.instrument_status` per transition (+ drift events from the reconciler).

---

### Task 15.3.2: Admin Instrument Management API

**Objective:** Implement admin API for instrument management.

**File Locations:** `services/internal/api/admin_instruments.go`

**Implementation:**
1. `POST /api/v1/admin/instruments` — create instrument (Super Admin).
2. `PUT /api/v1/admin/instruments/{id}` — update instrument.
3. `POST /api/v1/admin/instruments/{id}/activate` — DRAFT → ACTIVE.
4. `POST /api/v1/admin/instruments/{id}/restrict` — ACTIVE → RESTRICTED.
5. `POST /api/v1/admin/instruments/{id}/cancel-only` — → CANCEL_ONLY; cancels remain available, resting orders preserved (Risk Manager/Compliance Officer).
6. `POST /api/v1/admin/instruments/{id}/suspend` — → SUSPENDED (Compliance Officer+, no dual control per spec §7.2).
7. `POST /api/v1/admin/instruments/{id}/halt` — → HALTED (Risk Manager+).
8. `POST /api/v1/admin/instruments/{id}/resume` — CANCEL_ONLY/SUSPENDED/RESTRICTED/HALTED → ACTIVE (Risk Manager+, dual control per spec §7.2).
9. `POST /api/v1/admin/instruments/{id}/delist` — → DELISTED (Super Admin, dual control).

**Definition of Done (Acceptance Criteria):**
* [x] All instrument management endpoints work — `services/internal/api/admin_instruments.go`; routes flipped live with §7.2 role gates in `internal/gateway/routes_v1.go`; handlers wired in `cmd/gateway/main.go` (list/create-dual/update/activate/restrict/cancel-only/suspend/halt/resume-dual/delist-dual)
* [x] Dual control for create/resume/delist/global-halt; suspend is Compliance Officer+ without dual control (spec §7.2) — `OpInstrumentCreate`/`OpInstrumentResume`/`OpInstrumentDelist` executors on `DualControlService` (`RegisterInstrumentExecutors`); dual ops return 202 PENDING, approval executes in-tx
* [x] Resume covers SUSPENDED→ACTIVE, RESTRICTED→ACTIVE, HALTED→ACTIVE (spec §7.1 transition graph) — plus CANCEL_ONLY→ACTIVE; `skip_auction`/`auction` params control the reopening CALL key (`instrument:auction:{symbol}` = `CALL:{unix_ns}`)
* [x] State transitions validated — matrix + per-op edges reject with `INVALID_LIFECYCLE_TRANSITION`; TestInstrumentLifecyclePGTransitions, TestInstrumentLifecycleDualControlPG

**SDD Checklist:**
- [x] Spec checkpoint: admin instrument management API — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 15.3.3: Instrument Status in C++ Core

**Objective:** C++ core respects instrument status for order acceptance.

**File Locations:** `core/src/risk/PreTradeChecker.cpp` (extend)

**Implementation:**
1. C++ core reads instrument status from Redis `instrument:status:{symbol}`.
2. ACTIVE: accept orders.
3. RESTRICTED: accept limit orders only — market/stop orders rejected (spec §7.1).
4. CANCEL_ONLY rejects new/replace/amend with `INSTRUMENT_CANCEL_ONLY` but permits cancel/mass-cancel; SUSPENDED/HALTED/DELISTED reject new orders with their specific codes. DELISTED admits `reduce_only` closing orders during the 30-day close-only window (spec §7.1 remediation #35 — already implemented in `PreTradeChecker.cpp`; the Go order gate mirrors it).
5. Status refresh: every 1s from Redis.
6. **Coordination note (added 2026-09-29, Go cluster Task 15.3.1):** the publisher contract is documented under Task 15.3.1 "Engine-feed contract" — `instrument:status:{symbol}` holds the plain enum word for every non-DRAFT instrument, re-published by a 1s reconciler, deleted only on row purge. A missing key must keep failing closed (never default to ACTIVE). The reopening-CALL arm rides `instrument:auction:{symbol}` = `CALL:{deadline_unix_ns}` (Task 15.3.6 note).

**Definition of Done (Acceptance Criteria):**
* [x] C++ core respects instrument status — `MatchingEngine::admission_gate`/`effective_status` (`core/src/matching/MatchingEngine.cpp`) evaluate the immutable `InstrumentFeed::Snapshot` (`core/include/risk/InstrumentFeed.hpp`); missing/unverifiable status fails closed to `INSTRUMENT_SUSPENDED` (never reads as ACTIVE); the feed-unbound fallback consults the book's static `Instrument::status`; `Lifecycle.StatusMatrixNewOrders`/`UnverifiableFeedFailsClosed`/`CancelsStayOpenAmendsGated` in `core/tests/test_lifecycle_auction.cpp` pin the matrix including amend-vs-cancel asymmetry
* [x] RESTRICTED: limit orders only; market/stop rejected (spec §7.1) — `instrument_entry_gate` admits only `OrderType::LIMIT` under RESTRICTED (`kRejectInstrumentRestricted`); `Lifecycle.RestrictedIsLimitOnly`
* [x] SUSPENDED/HALTED/DELISTED: reject with specific code — `INSTRUMENT_SUSPENDED`/`INSTRUMENT_HALTED`/`INSTRUMENT_DELISTED` (DRAFT/unknown fail closed as SUSPENDED); DELISTED admits `kOrderFlagReduceOnly` closers per the 30-day window; `Lifecycle.StatusMatrixNewOrders`/`DelistedReduceOnlyWindow`
* [x] Status refresh every 1s — `InstrumentFeedRefresher` (`core/src/risk/InstrumentFeedRefresher.cpp`) polls `instrument:status:{symbol}` + `instrument:auction:{symbol}` + `market:hours` off the matching thread (`-feed-poll-ms`, default 1000) and republishes the snapshot; the matching thread reads `InstrumentFeed::snapshot()` only — never Redis

**SDD Checklist:**
- [x] Spec checkpoint: C++ core respects instrument status — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `ctest` green (31/31 incl. `test_lifecycle_auction`, 20 cases)

---

### Task 15.3.4: Trading-Hours Enforcement (24/5)

**Objective:** Enforce the canonical FX trading window — Sydney open (21:00 UTC Sunday) → New York close (22:00 UTC Friday) — per spec §1.

**File Locations:** `core/src/risk/PreTradeChecker.cpp` (extend), `services/internal/admin/market_schedule.go`

**Implementation:**
1. Market schedule published to Redis `market:hours` (open 21:00 UTC Sun, close 22:00 UTC Fri; admin-editable holiday overrides).
2. C++ PreTradeChecker adds a trading-hours check: new orders outside the window rejected with `MARKET_CLOSED`; cancels always allowed.
3. Weekend-close sequence: at 22:00 UTC Friday, engine transitions market to closed; GTD/DAY orders expiring over the weekend are handled by Task 2.3.10 expiry.
4. Re-open at 21:00 UTC Sunday: instruments return to their pre-close state (ACTIVE instruments resume accepting orders).
5. Per-instrument schedules may override global window (e.g., instrument-specific sessions) via `instrument:status:{symbol}` + schedule key.
6. **Go side (implemented 2026-09-29, Task 15.3.4 Go cluster):** `services/internal/admin/market_schedule.go` owns the `market:hours` Redis projection — a JSON STRING `{open_utc:"SUN 21:00", close_utc:"FRI 22:00", pre_open_utc:"SUN 20:45", overrides:[{date,closed,open,close,reason}], published_at, version}` rewritten (a) unconditionally at gateway boot (`Reconcile` — a restarted gateway never leaves a stale schedule in front of the C++ poller) and (b) after every committed override mutation. `pre_open_utc` is required so the §6.7 PRE_OPEN order-entry window (Sunday 20:45–21:00) is not rejected by the hours gate while matching is suppressed by the auction CALL key. Overrides are durable in `market_schedule_overrides` (migration 221; `override_date` UNIQUE, `closed ↔ open/close` CHECK pair, `open_utc < close_utc` CHECK), CRUD'd via `GET /api/v1/admin/market-schedule`, `GET|POST /api/v1/admin/market-schedule/overrides`, `PUT|DELETE /api/v1/admin/market-schedule/overrides/{id}` — reads for any venue-admin role, writes gated to Risk Manager/Super Admin (`scheduleWriteRoles`), every mutation writing `admin_audit_log` + `audit_hash_chain` atomically in the same tx. Expired rows stay queryable via the admin list but are excluded from the projection. The C++ PreTradeChecker remains the admission consumer (same-seam sibling scope): it reads `market:hours` and rejects `MARKET_CLOSED` outside the window — the Go side publishes the contract it consumes.
7. **Admin API surface (registered Task 5.3.7 registry, `routes_v1.go`, wired `cmd/gateway/main.go`):** `GET /api/v1/admin/market-schedule` (merged document as served to admins — read from PG so projection drift is visible), `GET /api/v1/admin/market-schedule/overrides`, `POST /api/v1/admin/market-schedule/overrides`, `PUT /api/v1/admin/market-schedule/overrides/{id}`, `DELETE /api/v1/admin/market-schedule/overrides/{id}`. Validation rejects malformed dates/times/past dates with `INVALID_REQUEST`; unique-date conflicts map 23505 → `INVALID_REQUEST`; post-commit republish failure surfaces `SERVICE_DEGRADED` (never silent — the next mutation or boot reconcile rewrites the key).

**Definition of Done (Acceptance Criteria):**
* [x] Orders outside 24/5 window rejected with `MARKET_CLOSED`; cancels allowed — Go side publishes the `market:hours` contract (`services/internal/admin/market_schedule.go` `Publish`/`Reconcile`); the engine-side consumer lands in `MatchingEngine::admission_gate` (feed-bound branch): `market_entry_allowed(snap.market, now_ns_)` over the logical clock, with `!verifiable || !market_known` failing closed to `MARKET_CLOSED`; `on_cancel_received` is ungated — `Lifecycle.MarketHoursGate` + `Lifecycle.MarketEntryWeek` in `core/tests/test_lifecycle_auction.cpp` (Saturday reject, cancel admitted, `market_known=false` reject, Sunday 20:44:59/20:45 pre-open boundary, Friday 21:59/22:00 close boundary)
* [x] Weekend close Friday 22:00 UTC and re-open Sunday 21:00 UTC enforced automatically — Go side: `SessionService.Evaluate` flips every shard on the canonical grid (see Task 15.3.7); `market:hours` always carries the canonical window so the engine poller enforces it — the pure `MarketHours`/`seconds_of_week`/`market_entry_allowed` helpers (`core/include/risk/InstrumentFeed.hpp`) pin `[pre_open_sow, close_sow)` including exact-instant edges; `parse_market_hours_json` honors per-date `closed`/window overrides (fail-closed on malformed values)
* [x] Holiday/schedule overrides configurable by admin — `market_schedule_overrides` (migration 221) + full CRUD (`CreateOverride`/`UpdateOverride`/`DeleteOverride`/`ListOverrides`) with atomic `admin_audit_log`/`audit_hash_chain` rows and mandatory republish; TestMarketScheduleIntegration (EXC_PG_TEST=1 EXC_REDIS_TEST=1) drives create→update→delete→republish and the boot-reconcile overwrite of a deliberately stale `market:hours`

**SDD Checklist:**
- [x] Spec checkpoint: 24/5 trading hours (spec §1) — defined first, validated against spec — canonical `SUN 21:00`/`FRI 22:00` UTC window with `SUN 20:45` pre-open entry boundary; `TestSessionStateAt`/`TestNextBoundary` pin the boundary table (exact-instant edges included)
- [x] All spec checkpoints pass after implementation — Go side: `go build/vet/test ./internal/admin` green incl. gated `TestMarketScheduleIntegration`
- [x] Edge cases: DST transitions (all boundaries are fixed UTC instants — the Go service computes in UTC so DST cannot shift them; the settlement rollover clock's own 17:00-ET/DST handling is untouched and authoritative for the roll), mid-week holiday override (`closed` + partial-window rows validated and projected), orders submitted at exact open/close boundary (`sessionStateAt` treats 22:00 Friday as CLOSED and 21:00 Sunday as OPEN — closed-boundary/exact-instant cases pinned in `TestSessionStateAt`)

---

### Task 15.3.5: Trade Bust & Price-Adjust Workflow (Obvious-Error Policy)

**Objective:** Implement the obvious-error trade bust/price-adjust mechanism per spec §5.29/§7.2 (§24 #138) — dual-control admin correction of erroneous executions with full ledger reversal. Added 2026-09-15.

**File Locations:** `services/internal/admin/trade_busts.go`, `services/internal/ledger/posting.go` (reversal entries)

**Implementation:**
1. `trade_busts` table (migration 051): `id`, `trade_id`, `action` (BUST|PRICE_ADJUST), `adjusted_price`, `reason`, `initiated_by`, `approved_by`, `status`, `executed_at`, `created_at` (columns aligned with spec §5.29, remediation #35 — supersedes the prior renamed/dropped columns, which would not match migration 051).
2. Endpoint: `POST /api/v1/admin/trades/{id}/bust` — Risk Manager + dual control (2 distinct approvers). Eligibility window: trades < 15 min old (canonical per spec §7.3.4, remediation #35 — supersedes the prior 1h window, which contradicted the spec contract) and price deviating > obvious-error band (configurable, default >2× price-band) from market at fill time.
3. BUST: full reversal — balances/positions restored via GL reversal journal entries; fees refunded; settlement record voided (if not yet dispatched to rails — settled trades cannot be busted, only bilaterally compensated).
4. PRICE_ADJUST: GL adjustment entries for the price delta; settlement record amended.
5. Notifications to both counterparties; immutable audit trail; busted trades remain in trade history flagged `BUSTED` (never deleted — MiFID record-keeping).

**Migration note:** `migrations/051_trade_busts.up.sql` — `trade_busts` table + `trades.status` gains `BUSTED`/`PRICE_ADJUSTED` (spec §5.29).

**Definition of Done (Acceptance Criteria):**
* [x] Dual-control bust reverses balances/positions/fees with balanced GL reversal — `services/internal/admin/trade_busts.go` `execute` posts the reversal via `settlement.LedgerService.PostJournal` inside the bust tx (balanced-entry invariant enforced by `ledger.Journal`); fees refunded as journal legs; positions rebuilt from `position_fills` excluding the busted trade's fills (mirror of `settlement.applyFill`); `TestBustJournalBalanced`/`TestBustJournalZeroFees`/`TestBustJournalFeeExceedsProceeds` pin the journal shapes; two-principal execution (`initiated_by`/`approved_by` distinct, `TestBustApproverDistinct` + `TestBustPendingApprovePG`)
* [x] Price-adjust posts GL delta entries and amends settlement — `PRICE_ADJUST` builds quote-delta journals against `adjusted_price` and `applySettlement` rewrites undispatched PENDING legs (`AMENDED` outcome); `TestAdjustJournalDelta` + `TestPriceAdjustPG`
* [x] Settled/dispatched trades cannot be busted (rejected with TRADE_ALREADY_SETTLED) — `checkEligibility` refuses when any `settlement_instructions` leg has `dispatched_at` set or a terminal status → `TRADE_ALREADY_SETTLED` (HTTP 409); `TestBustSettledRejectsPG`
* [x] Busted trades retained, flagged, never deleted; both parties notified — `trades.status` UPDATE only (`BUSTED`/`PRICE_ADJUSTED`, migration 051; row + `trade_busts` record persist); `notifications.EventTradeBusted`/`EventTradePriceAdjusted` dispatched to both counterparties as critical events; `TestBustExecutePG` asserts retention + notification calls

**SDD Checklist:**
- [x] Spec checkpoint: trade bust/price-adjust with dual control + GL reversal (§7.2, §24 #138) — defined first, validated against spec: 15-min §7.3.4 window + >2× price-band eligibility gate in `checkEligibility` (`TestBustWindowExpiredPG`, `TestBustInsideBandRejectsPG`), `OpTradeBust` registered as sensitive op, dual-control `PENDING_APPROVAL` queue with settlement hold (`HasPendingBust` seam for the dispatcher)
- [x] All spec checkpoints pass after implementation — `go build/vet/test` green; `EXC_PG_TEST=1 go test ./internal/admin` green incl. all `*PG` flows (`TestBustExecutePG`, `TestBustPendingApprovePG`, `TestPriceAdjustPG`, `TestBustExpirePendingPG`)
- [x] Edge cases: bust of a busted trade (`TestBustOfBustedPG` — second review rejected; status≠COMPLETED re-check under row lock), bust after downstream hedging (`TestBustAfterHedgePG` — hedge fills are separate trades; bust reverses only the flagged trade's legs and the deviation is documented in-file), partial-fill bust (`TestPartialFillBustPG` — reversal scoped to the trade's fills, sibling fills untouched)

---

### Task 15.3.6: Reopening Call Auction (HALT/SUSPEND→ACTIVE + Weekly Open)

**Objective:** Add a call-auction reopening phase so resumption after HALT/SUSPEND and the weekly 24/5 open run through price discovery instead of flipping straight to continuous trading (spec §7.1, §24 #142). Added 2026-09-15.

**File Locations:** `core/src/matching/AuctionManager.cpp`, `services/internal/admin/market_schedule.go` (extend)

**Implementation:**
1. Reopening auction state `CALL` on transition to ACTIVE from HALTED/SUSPENDED and at Sunday 21:00 UTC weekly open (when resting orders + weekend gap exist); admin may bypass via `resume` parameter `skip_auction=true` for trivial resumes.
2. CALL phase 5 min default (per spec §24 #142 — orders accumulate, no matching, indicative uncross price streamed); EXTEND up to a configurable cap if imbalance persists (machinery shared with the liquidation auction's CALL/EXTEND pattern, but reopening uses the longer market-open cadence, not the 5s liquidation cadence).
3. Uncross: single clearing price maximizing executable volume; orders execute at uncross price (price-time priority for allocation); unfilled orders remain in book → continuous ACTIVE.
4. Reuses Task 19.3.3/Phase-2 auction machinery; auction events emitted to WAL + WS `auction.indicative` for transparency (channel name aligned with spec §7.1, remediation #35 — supersedes the prior `auction.status`).
5. **Coordination note (added 2026-09-29, Go cluster Task 15.3.2):** the Go lifecycle service arms the reopening CALL through `instrument:auction:{symbol}` = `CALL:{deadline_unix_ns}` (written on resume when the auction applies — HALTED/SUSPENDED→ACTIVE default, RESTRICTED/CANCEL_ONLY→ACTIVE opt-in via `auction:true`; `skip_auction:true` bypasses and the key is deleted). The C++ AuctionManager consumes this key to hold the book in CALL until the deadline; instrument status reports `ACTIVE` while the auction runs. Deadline is written as now+5min (the §24 #142 default); weekly-open auctions are the Task 15.3.7 scheduler's seam (same key contract).

**Definition of Done (Acceptance Criteria):**
* [x] HALT/SUSPEND→ACTIVE transitions route through CALL auction (5-min default + EXTEND cap, per spec §24 #142) — Go arms `instrument:auction:{symbol}` on resume (Task 15.3.2 contract); the engine observes it in `MatchingEngine::auction_control_sync` at every ingress event/tick and enters CALL via `auction_enter_call` (`core/src/matching/MatchingEngine.cpp`); EXTEND rewrites move the deadline (max 3 observed extensions, `kAuctionMaxExtensions`, then fail-closed quarantine); `Auction.CallAccumulatesCrossedBook`/`ExtendMovesDeadline` pin the flow
* [x] Weekly reopen (Sun 21:00 UTC) uses call auction when weekend gap exists — Task 15.3.7 `SessionService` writes the same `CALL:{deadline}` key at Sunday 20:45; the engine consumes the identical contract (deadline strikes only on journaled `TIME_TICK` logical time — deterministic on replay)
* [x] Uncross executes at single clearing price; residual orders rest → continuous trading — `auction_indicative` picks max-executable-volume price (iceberg hidden remainder counts); `auction_uncross` allocates in strict price-time order with parked MARKET orders ranked ahead of all limits; residual resting orders resume continuous trading; journaled `AUCTION_PHASE`/`TRADE` rows replay deterministically (`Auction.UncrossSinglePriceAndResume`, `WalReplayConverges` — live-vs-recovered book fingerprints identical)
* [x] Admin `skip_auction` bypass works for trivial resumes — the key-absent path never enters CALL (continuous resumes directly); a key deleted mid-CALL aborts the auction via `AUCTION_PHASE:CANCEL` (crossed residue quarantines fail-closed — `Auction.WithdrawnKeyQuarantinesCrossedBook`)

**SDD Checklist:**
- [x] Spec checkpoint: reopening auction on resume + weekly open (§7.1, §24 #142) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `ctest` green (31/31; `test_lifecycle_auction` adds 20 cases: accumulation, parked MARKET/IOC/FOK, untriggered stops, indicative dedupe, EXTEND, uncross, withdraw/quarantine, WAL replay convergence)
- [x] Edge cases: empty book at reopen (no clearing candidate → `STRIKE_FAIL` + `FAILED` result → Go ladder decides EXTEND/SUSPEND — `Auction.StrikeFailAwaitsExtensionThenClears`), uncross with only market orders (same no-anchor path — a lone parked market cannot form a price), auction during degradation mode (admission remains feed-gated; `ModeManager` degradation is the upstream gate — the engine itself holds CALL state independent of mode), consumed-deadline replay (`Auction.CompletedDeadlineDoesNotReenter` — a stale republished key never re-enters CALL)

---

### Task 15.3.7: 24/5 Trading Session Lifecycle Implementation

**Objective:** Implement the weekly session state machine (spec §6.7, §24 #217). Added 2026-09-17 (gap analysis remediation #6).

**File Locations:** `services/internal/admin/session_lifecycle.go`

**Implementation:**
1. Session states: `OPEN`, `PRE_CLOSE`, `CLOSED`, `PRE_OPEN`. State machine enforced in Go gateway.
2. Friday 22:00 UTC: transition `OPEN` → `CLOSED`. Stop routing new orders to engine. GTC orders preserved. DAY orders expired. Tom-Next rollover triggered (Task 3.3.7).
3. Sunday 20:45 UTC: transition `CLOSED` → `PRE_OPEN`. Accept orders into book without matching.
4. Sunday 21:00 UTC: trigger reopening auction (Task 15.3.6). On auction completion, transition to `OPEN`.
5. WS events: `session.closed`, `session.pre_open`, `session.open` broadcast to all subscribers.
6. GTD expiry: clock-driven, fires regardless of session state (weekend GTD expiry is valid).
7. Session state stored in Redis: `session:state:{shard_id}`. Persisted across restarts.
8. API: `GET /api/v1/session/status` returns current state + next transition time.
9. **Implementation note (added 2026-09-29, Go cluster):** `services/internal/admin/session_lifecycle.go` owns the machine. `PRE_CLOSE` is a 5-minute advisory window (Friday 21:55 UTC — spec §6.7 lists the state but schedules no duration; the close event gains a `session.pre_close` advisory on the same channel for free). Each shard's `session:state:{shard_id}` HASH carries `{state, entered_at, next_transition_at, next_state, last_event}`; transitions CAS via a Lua script so concurrent gateway replicas cannot double-fire a boundary, and the per-boundary effects set (rollover / auction keys / WS broadcast) is recorded in the `session:ctl` pending ledger BEFORE execution under a `session:ctl:effects:{boundary_unix}` single-fire lock — a mid-transition crash is resumed by the next `Evaluate` on any replica (`drainPending`). Friday close invokes the Tom-Next roll through the Task 3.3.7 `settlement.RolloverService.RunOnce` seam (the `RolloverRunner` adapter — internally idempotent via `rollover_runs` + execution lock; the 17:00-ET rollover clock is settlement's authority and is untouched). Sunday 20:45 writes `instrument:auction:{symbol}` = `CALL:{deadline_unix_ns}` per ACTIVE instrument with deadline = the Sunday 21:00 open instant — the same key contract the Task 15.3.2 resume path and the C++ AuctionManager consumer share (the consumer holds the book in CALL until the deadline, then performs the §24 #142 single-price uncross and deletes the key; Go deliberately writes nothing at the open boundary — the deadline IS the release). Friday close also clears leftover auction keys (`auction_clear`) so a stale CALL can never leak into the next weekly cycle. Order-entry rejection is the `market:hours` contract (Task 15.3.4 Go side); DAY/GTD expiry stays clock-driven in `core/src/matching/ExpiryScheduler.cpp` — verified: `on_time_tick` pops by `expiry_ns` with no session gate, and `day_expiry_ns` maps weekend accepts to the upcoming Friday 22:00 close; not rebuilt or re-gated here.
10. **API surface:** `GET /api/v1/session/status` (public, `routes_v1.go` v1live, handler `api.SessionStatus` wired in `cmd/gateway/main.go`) returns `{state, consistent, shards:[{shard_id,state,entered_at,reachable}], shard_coverage, next_state, next_transition_at, market_open, pending_effects?}` — `state` is schedule-implied (the machine's clock is authoritative); `consistent=false` and per-shard `reachable=false` expose non-converged/unreachable shards rather than fabricating all-green. Scheduler runs as a 5s `Run` loop on the gateway sweep context; `Reconcile` evaluates once at boot (missing keys written, stale states corrected). Both seams wired in `cmd/gateway/main.go` (real `settlement.RolloverService` when the holiday calendar + swap stores resolve — unwired → logged skip, daemon backstop documented).

**Definition of Done (Acceptance Criteria):**
* [x] 4 session states implemented — `SessionOpen`/`SessionPreClose`/`SessionClosed`/`SessionPreOpen` in `services/internal/admin/session_lifecycle.go`; `sessionStateAt`/`nextBoundary`/`lastBoundary` are pure UTC functions pinned by TestSessionStateAt/TestNextBoundary/TestLastBoundary
* [x] Friday 22:00 UTC closure and Sunday 21:00 UTC opening sequences enforced — `SessionService.Evaluate` (5s `Run` loop, injected clock) drives the grid; `TestSessionLifecycleWeeklyFlow` (EXC_REDIS_TEST=1, real Redis, fake clock) sweeps OPEN→PRE_CLOSE→CLOSED (rollover seam fires exactly once)→PRE_OPEN (CALL keys land, `uncross_at`=21:00)→OPEN (UNCROSS keys) and asserts idempotent re-evaluation inside a boundary
* [x] Session WS events broadcasted — `effectPublish` emits `session.closed`/`session.pre_open`/`session.open` (+ `session.pre_close` advisory) on the `session.status` public channel via `ws.Server.Publish`; asserted by `TestSessionLifecycleWeeklyFlow` (exactly once per boundary, pending-ledger drain re-fires missed effects)
* [x] Session state persisted in Redis and accessible via API — `session:state:{shard_id}` HASH per shard survives restarts (`Reconcile` rewrites on boot; the post-restart `Status` assertion in the integration test reads OPEN without re-evaluation); `GET /api/v1/session/status` live in the route registry + `MountSeedLive` handler map

**SDD Checklist:**
- [x] Spec checkpoint: 24/5 Trading Session Lifecycle (§6.7, §24 #217) — defined first, validated against spec: canonical boundaries Friday 22:00 close / Sunday 20:45 pre-open / 21:00 open; `session.pre_close` advisory added as a documented extension (spec §6.7 lists PRE_CLOSE but schedules no window — the 21:55 boundary is the operative interpretation)
- [x] All spec checkpoints pass after implementation — `go build/vet/test` green; `TestSessionLifecycleWeeklyFlow` green against Redis 127.0.0.1:6379 with dedicated shard ids; fail-closed constructor (`nil` RDB/empty shards rejected) verified
- [x] GTD expiry verified, not rebuilt — `core/src/matching/ExpiryScheduler.cpp` `on_time_tick` is clock-driven (`expiry_ns <= tick_ns` pops regardless of session state); `day_expiry_ns` already handles the weekend gap (expiry at the upcoming Friday 22:00 UTC close); no session-state gate added — weekend GTD expiries fire during CLOSED/PRE_OPEN per spec §6.7

---

### Task 15.3.8: Instrument Maintenance Workflow & Parameter Change Lifecycle

**Objective:** Implement a maker-checker approval workflow for instrument creation, parameter modification, and delisting with full audit trail.

**File Locations:** `services/internal/admin/instrument_maintenance.go`

**Implementation:**
1. Instrument creation workflow: new instrument request → Risk Manager proposes → Compliance Officer reviews → Super Admin approves → instrument enters the DRAFT state with a pending listing proposal (workflow status, not a lifecycle state — the canonical 7-state enum per §5.1/§7.1 is unchanged; remediation #35).
2. Parameter change workflow: any change to tick_size, lot_size, margin_rate, trading_hours, max_leverage → maker submits change request → checker approves → effective_date scheduled.
3. Effective-date scheduling: parameter changes activate at start of next trading session (not mid-session). Exception: emergency changes by Super Admin can take effect immediately with P1 alert.
4. Symbol conventions: enforce standard FX naming (ISO 4217 base/quote, e.g., EUR/USD not EURUSD). Migration mapping for legacy symbols.
5. Delisting workflow: instrument enters RESTRICTED (limit orders only — no new market/stop orders; existing positions can close via limit orders) → after grace period → DELISTED (all new order entry rejected `INSTRUMENT_DELISTED`; positions close during the 30d close-only window via `reduce_only` orders — the §7.1 ladder is canonical; the "force-close at mark price" variant is superseded except for the §7.4 redenomination/peg-break path, remediation #35).
6. Notification to connected clients: FIX SecurityStatus (35=f) sent on any instrument state change (Phase-18). WS instrument update channel.
7. Audit trail: every parameter change logged with who/what/when/why in `instrument_change_log` table. Immutable log for regulatory review.
8. **Implementation note (Go cluster):** `services/internal/admin/instrument_maintenance.go` + migration `219_instrument_change_log` (`instrument_change_requests` + immutable `instrument_change_log` + `instruments.param_overrides JSONB`). Parameters without dedicated columns in the base `instruments` schema (`margin_rate`, `trading_hours`) live in `param_overrides` — additive deviation from the column-per-param reading; dedicated columns (`tick_size`, `lot_size`, `min/max_order_qty`, `price_band_pct_*`, `settlement_cycle`, `max_leverage`, `min_notional`, filter fields) are updated in place. Create files a DRAFT-state instrument row; param/delist requests ride the shared `DualControlService` (delist via the existing `OpInstrumentDelist` executor → `InstrumentLifecycle.TransitionTx`, so RESTRICTED/DELISTED land on the canonical §7.1 ladder and Redis `instrument:status:{symbol}`/`venue.instrument_status` publication stays with the lifecycle service). Maintenance additionally emits `SecurityStatusEvent` on NATS `marketdata.security_status` — the Phase-18 FIX SecurityStatus (35=f) consumer binds that payload shape; FIX encoding is out of scope here. `ApplyDue` runs as a gateway sweep to apply due effective-dated changes; emergency changes alert via the ops alerter. Wired in `cmd/gateway/main.go` (`InstrumentMaintenanceService` + sweep loop).

**Definition of Done (Acceptance Criteria):**
* [x] Maker-checker workflow enforced for instrument creation and parameter changes — `services/internal/admin/instrument_maintenance.go` `RequestCreate`/`RequestParamChange` file `instrument_change_requests` rows gated by the §8.2 role matrix and execute only through the dual-control queue (`TestCreateMakerCheckerChainPG`, `TestMaintenanceRoleGate`); one open request per instrument+field enforced by `icr_open_field_ux`/`icr_open_create_ux` (migration 219)
* [x] Effective-date scheduling activates changes at session boundary — `effective_at` defaults to next-session start (`NextSession` seam, `TestDefaultNextSessionStart`); the `ApplyDue` sweep applies rows whose effective time has passed so nothing lands mid-session (`TestParamChangeScheduledPG`)
* [x] Emergency changes require Super Admin with P1 alert — `emergency:true` bypasses the session wait under the Super-Admin role gate and pages through the ops alerter seam (`maintenanceAlerter` wiring); `TestEmergencyParamChangePG`
* [x] Delisting workflow: RESTRICTED → grace period → DELISTED with force-close — `RequestDelist` submits `OpInstrumentDelist` through the existing dual-control executor into `InstrumentLifecycle.TransitionTx`, preserving the §7.1 ladder (RESTRICTED 24h notice → DELISTED 30d close-only; force-close/purge stays with the lifecycle/engine path per the remediation-#35 reading); `TestDelistWorkflowPG` (delist proceeds with open margin positions — wind-down is engine-side)
* [x] FIX SecurityStatus and WS notifications sent on state changes — every transition/`ApplyDue` posts `SecurityStatusEvent` on NATS `marketdata.security_status` (the Phase-18 FIX 35=f binding seam) and broadcasts on the public `venue.instrument_status` WS channel; Redis `instrument:status:{symbol}` remains the lifecycle feed's authority
* [x] Full audit trail in instrument_change_log — migration 219 creates the immutable log (UPDATE/DELETE blocked by trigger) with before/after param snapshots; `admin.Log` records each request/approve/apply; `param_overrides JSONB` carries parameters without dedicated columns (margin_rate, trading_hours — additive deviation, see implementation note below)

**SDD Checklist:**
- [x] Spec checkpoint: instrument maintenance workflow — defined first, validated against spec: maker-checker over create/param/delist, session-boundary application, emergency path, immutable `instrument_change_log` per §7.4/§7.5 and §24 #234
- [x] All spec checkpoints pass after implementation — `go build/vet/test` green; `EXC_PG_TEST=1 go test ./internal/admin` green (`TestCreateMakerCheckerChainPG`, `TestParamChangeScheduledPG`, `TestEmergencyParamChangePG`, `TestDelistWorkflowPG`)
- [x] Edge cases: parameter change during scheduled maintenance (emergency flag is the only same-session path — asserted by `TestEmergencyParamChangePG`), concurrent change requests for same instrument (unique-open-request indexes → 409 `CHANGE_REQUEST_PENDING`), delisting with open margin positions (`TestDelistWorkflowPG` — delist is allowed, positions wind down through the §7.1 close-only ladder rather than blocking governance)

---

### Task 15.3.9: Explicit `CANCEL_ONLY` Instrument State

**Objective:** Add a persistent operator-controlled state in which cancellations remain available while all new orders and amendments are rejected.

**Implementation:**
1. Extend the lifecycle enum with `CANCEL_ONLY`; transitions require Risk Manager or Compliance Officer authorization and an auditable reason.
2. Preserve resting orders indefinitely until clients cancel or an administrator transitions to SUSPENDED/HALTED; never force the five-minute SUSPENDED timer while in CANCEL_ONLY.
3. Publish the state through instruments REST, WS status, SBE security definitions, and FIX TradingSessionStatus/SecurityStatus.
4. Core pre-trade rejects new/replace/amend requests with `INSTRUMENT_CANCEL_ONLY`; cancel and mass-cancel always remain available.

**SDD Checklist:**
- [x] Spec checkpoint: persistent CANCEL_ONLY rejects entry/amend while preserving all cancellation paths (§24 #290) — defined first, validated against spec; `CANCEL_ONLY` is one of the seven enum states, the orders gate rejects new/replace/amend with `INSTRUMENT_CANCEL_ONLY` while cancels/mass-cancels stay open, and the sweeper arms the 5-min mass-cancel only for `SUSPENDED` (TestInstrumentLifecycleSuspendSweepPG asserts CANCEL_ONLY is never swept). REST/Redis/WS publication via `instrument:status:{symbol}` + `venue.instrument_status` + `GET /api/v1/admin/instruments`; SBE security-definition and FIX SecurityStatus republication consume the same feed (Task 15.3.3 core refresher + Phase-18 FIXS cluster).
- [x] All spec checkpoints pass after implementation

---

### Task 15.3.10: Reopening Auction Clearing Failure & Crossed Book Quarantine

**Objective:** Implement error boundaries and fail-closed state transitions for reopening auction uncross failures and crossed-book anomalies per spec §2.7, §7.3, and §24 #316.

**Implementation:**
1. **Auction Clearing Timeout Fallback:** In `AuctionManager.cpp`, if the indicative uncross price cannot be formed after $3\times 30\text{s}$ extension phases, transition instrument state to `SUSPENDED` with error code `AUCTION_CLEARING_FAILED` (HTTP 409 — supersedes prior HTTP 503; auction clearing failure is a state conflict, not a service unavailable; aligned to spec §23) rather than forcing an arbitrary fill.
2. **Crossed-Book Quarantine:** If bids cross asks during continuous trading without an immediate match execution, immediately quarantine the instrument into `HALTED` (`CROSSED_BOOK_DETECTED`), preserve resting orders, and page the Risk Manager.
3. **Admin Resolution Workflow:** Provide `POST /api/v1/admin/instruments/{id}/uncross-override` requiring dual control to manually review book imbalances, cancel offending orders, and restart continuous trading.

**Definition of Done (Acceptance Criteria):**
* [x] Unclearable auctions fail closed to SUSPENDED state — a deadline strike with no candidate clearing price arms `auction_awaiting_` + journals `AUCTION_PHASE:STRIKE_FAIL` and requests a `FAILED` result on `{key}:result` (the armed key survives so the Go EXTEND ladder decides); past `kAuctionMaxExtensions` the engine refuses further extensions, journals `AUCTION_PHASE:QUARANTINE` with `kAuctionReasonClearingFailed`, drains parked orders, and quarantines with `AUCTION_CLEARING_FAILED` (`quarantine_enter`/`auction_control_sync` cap branch) — `Auction.StrikeFailAwaitsExtensionThenClears`
* [x] Crossed book condition triggers immediate emergency halt — `quarantine_check` runs after every mutation in continuous mode: a crossed book with no armed CALL quarantines the instrument (`CROSSED_BOOK_DETECTED`), preserves all resting orders, legitimates the forensic crossed state via `OrderBook::allow_crossed` so audits halt on real anomalies only, and emits an `AuctionEvent` (phase QUARANTINE) on the IPC stream for the Risk-Manager alert path — `Auction.WithdrawnKeyQuarantinesCrossedBook` (orders + amends reject, cancels stay open, crossed residue preserved for forensics)
* [x] Admin recovery workflow restores orderly matching under dual control — a quarantined instrument resolves through the dual-controlled resume path (Task 15.3.2 `OpInstrumentResume`, `auction:true`): the re-armed CALL uncrosses the forensic residue at a single clearing price and clears quarantine only when the book uncrosses clean — `Auction.ReArmClearsQuarantineByUncrossing`; a residual crossing re-quarantines immediately

**SDD Checklist:**
- [x] Spec checkpoint: Reopening auction clearing failure and crossed-book quarantine fail closed (§24 #316) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `ctest` green (31/31 incl. `test_lifecycle_auction`); WAL replay restores quarantine + crossed-book allowance (`AUCTION_PHASE:QUARANTINE` replay path, `core/src/recovery/RecoveryManager.cpp`)

---

### Task 15.3.11: Production Instrument Reference, Session Calendar & Tenor Grid

**Objective:** Replace column-only instrument parameters with a seeded production reference and a calendar-correct session/tenor engine, per spec §7.4 and §24 #343. Added 2026-09-27 (production-maturity remediation #24).

**File Locations:** `services/internal/instruments/reference.go`, `services/internal/instruments/sessions.go`, `migrations/087_instrument_reference.up.sql`

**Implementation:**
1. **Reference seed (migration 087):** per-symbol `tick_size/lot_size/min_order_qty/max_order_qty/min_notional/contract_size/decimal_places/pip_size` — majors tick 0.00001 (5dp), JPY pairs 0.001 (3dp), with `GET /api/v1/instruments` exposing the full row (extends Phase-05 Task 5.3.5). `order_type` enum note: the §5.4 14-value base enum is unchanged; VP/GRID execute as named algo strategies over base types, HIDDEN/GSLO are execution flags, OPO/OPOCO are composite list modes, and `stp_mode` gains fifth value `NONE` (Professional/ECP only, Task 2.3.16) — supersedes the 4-value enum.
2. **Session enforcement:** daily 17:00 ET rollover cutoff vs 22:00 UTC Friday close interaction; DST shift rules (21:00 vs 22:00 UTC Sunday open); Christmas/New Year early-close schedule overrides; `value_date`-on-holiday rejected with `VALUE_DATE_ON_HOLIDAY` (HTTP 422, new §23 code) wired into `PreTradeChecker` via the Phase-03 Task 3.3.8 calendar; NDF fixing-holiday shift to the next good business day.
3. **Tenor grid:** standard tenors (ON/TN/SN/1W/2W/1M/2M/3M/6M/9M/1Y/2Y) with broken-date interpolation rule and `spot-date+T+1/T+2+tenor` value-date math; IMM-date stub calendar; option-expiry calendar (10:00 NY cut for NDF fixings, 15:00 UTC venue cut per Task 22.3.10, holiday roll-forward).
4. **No corporate actions (stated):** fiat spot FX has no splits/dividends — recorded here so audits stop flagging it; currency redenomination or peg-break follows RESTRICTED→DELISTED with force-close under Task 15.3.8 maker-checker (`CORPORATE_ACTION_SCHEDULED` stays reserved, never emitted).

**Definition of Done (Acceptance Criteria):**
* [x] Every listed symbol carries a complete reference row; gateway validates tick/lot/notional from it — migration 087 adds `contract_size/decimal_places/pip_size` with NOT NULL + CHECK constraints, corrects USD/JPY to the JPY 3dp convention (tick 0.001, pip 0.01), seeds the four missing §2.2 pairs, backfills `instruments_reference` per instrument, and installs the `instruments_reference_defaults` trigger so reference-less inserts get convention-derived defaults; `marketapi` `Instrument`/`PgStore` project the full row through `GET /api/v1/instruments`
* [x] Session, DST, holiday and value-date rules enforced in-core and at the gateway with code — `instruments/sessions.go` `SessionCalendar` anchors every mark at 17:00 ET (DST-resolved: 21:00 UTC EDT / 22:00 UTC EST), `State()` resolves OPEN/PRE_OPEN(15m)/WEEKEND/CLOSED/EARLY_CLOSE incl. Christmas/New-Year overrides; `ValidateValueDate` emits `VALUE_DATE_ON_HOLIDAY` via the Phase-03 `settlement.HolidayCalendar`, `RollNdfFixing` rolls NDF fixings to the next mutual business day. Fail-closed: missing center calendar → SERVICE_DEGRADED. The engine-admission gate consumes `SessionService.CheckValueDate` (wiring rides the order-admission path, Phase-16 sibling scope)
* [x] Tenor/value-date math calendar-correct; redenomination path documented — `TenorGrid` (ON/TN/SN/1W–2Y), `TenorValueDate` (Modified-Following + end-of-month rule), `TenorBracket` broken-date interpolation, `IMMDate`/`NextIMMDate` stubs, 10:00-NY NDF fixing cut and 15:00-UTC venue cut via `OptionExpiryInstant`/`RollOptionExpiry`; no-corporate-actions rule stated in code + migration header (redenomination/peg-break → RESTRICTED→DELISTED force-close). `sessions_test.go` pins DST transitions, session states, tenor math, holiday rolls

**SDD Checklist:**
- [x] Spec checkpoint: seeded instrument reference, calendar-correct sessions with value-date blocking, tenor grid, and stated no-corporate-actions rule (§24 #343) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go build/vet/test ./internal/instruments` green

---

### Task 15.3.12: Operations Console Backend — Listing, Delisting & Venue Ops

**Objective:** Implement the listing/delisting workflows and ops-board queries behind the Phase-10 console, per spec §7.5 and §24 #352. Added 2026-09-27 (environment & operations remediation #26).

**File Locations:** `services/internal/instruments/listing.go`, `services/internal/instruments/opsboard.go`, `migrations/091_fleet_and_ops_console.up.sql`

**Implementation:**
1. **Listing proposals (migration 091):** `listing_proposals` (proposer, symbol, reference row, oracle coverage proof, risk defaults, status PROPOSED/IN_REVIEW/APPROVED/REJECTED/SCHEDULED, reason). Auto-checks validate the §7.4 reference row, ≥2 oracle feeds and defaulted risk limits; approval executes DRAFT creation (Task 15.3.2) and schedules ACTIVE with FIX/WS status broadcast.
2. **Delisting with impact preview:** pre-transition query aggregates open positions, resting orders and sub-account exposure per symbol; executes RESTRICTED (24h notice) → DELISTED → 30d close-only → purge, reusing the Task 15.3.8 maker-checker and the §7.4 force-close path. Dual control per the §7.2 matrix.
3. **Ops board queries:** single endpoint serving all non-ACTIVE instruments with state, grace timers and pending approvals; resume/halt actions route through the Task 15.3.1 state machine and Task 15.3.6 reopening auction. Every transition writes the approval pair to `admin_audit_log` (Task 7.3.3) and exports through the §24 #345 audit API.
4. **Route paths (amended 2026-09-27, remediation #26 route-path amendment):** `GET/POST /api/v1/admin/listing-proposals`, `POST /api/v1/admin/listing-proposals/{id}/review` (dual control per §7.2 matrix), `GET /api/v1/admin/ops-board` (non-ACTIVE instruments, timers, approvals). Pre-existing instrument routes (`POST /api/v1/admin/instruments`, `.../activate|restrict|cancel-only|suspend|halt|resume|delist`, `POST /api/v1/admin/trades/{id}/bust`) unchanged. All registered in Task 5.3.7.

**Definition of Done (Acceptance Criteria):**
* [x] Proposals auto-check, route for review, and schedule activation with broadcast — `instruments/listing.go` `Propose` (reference/symbol/oracle≥2-feeds/risk-defaults auto-checks persisted verbatim), `Review` (REVIEW/APPROVE/REJECT; APPROVE requires Super Admin and files `OpInstrumentListing` — approval runs the DRAFT create + `instruments_reference` envelope + SCHEDULED in the four-eyes tx via `RegisterListingExecutor`), `ActivateDue` sweep flips DRAFT→ACTIVE at `activate_at` (default = next weekly open) through the lifecycle audit shape + `PublishCommitted` (engine status key + WS broadcast; the FIX broadcast leg rides the §18 FIX gateway surface). Resubmission after REJECTED/EXPIRED dual requests is handled. Migration 223 adds `reviewed_by/reviewed_at/dual_control_id/instrument_id/activate_at` to the 091 table. `TestListingLifecycleIntegration` drives the full chain on the dev DB
* [x] Delisting previews impact and executes the grace ladder with dual control — `PreviewDelist` aggregates open positions/resting orders/sub-account exposure; `RequestDelist` (Super Admin) runs RESTRICTED via the lifecycle engine + records the ladder in `instruments_reference.delist_schedule`; `AdvanceDelisting` files `OpInstrumentDelist` after the 24h notice, adopts direct-admin DELISTED rows, tracks the 30d close-only window to `PURGE_READY` (force-close = Task 15.3.8 maker-checker). `TestDelistLadderIntegration` covers notice-gating and the approval submission
* [x] Ops board serves state/timers/approvals; transitions audit-logged with approval pairs — `instruments/opsboard.go` `Board` (non-ACTIVE instruments + engine-gate drift, pending proposals + overdue activations, pending instrument-surface dual-control rows, upcoming auctions, today's fixings, warnings); every mutation writes `admin_audit_log` + `audit_hash_chain` in-tx (propose/review/delist-notice/ladder steps; dual-control submit/decide rows carry the approval pair)

**SDD Checklist:**
- [x] Spec checkpoint: listing proposals with auto-checks, impact-previewed delisting ladder, and approval-paired ops-board transitions (§24 #352) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `TestListingLifecycleIntegration`/`TestCalendarReplaceIntegration`/`TestDelistLadderIntegration`/`TestOpsBoardIntegration` green (EXC_PG_TEST=1)

### Task 15.3.13: Daily Closing-Auction Calendar

**Objective:** Implement the per-instrument daily auction calendar with DST-aware recurrence, MOC order execution at the daily close uncross, and FIXING order execution at benchmark times, per spec §7.1 and §24 #401. Added 2026-09-27 (feature completeness audit #36 — institutional FX venues require scheduled daily close auctions for EOD benchmark fixing participation and MOC order execution).

**File Locations:** `services/internal/instruments/auction_calendar.go`, `services/internal/instruments/fixing_scheduler.go`

**Implementation:**
1. **Auction calendar table:** `auction_calendar` (added to migration 087 reference schema) — per-instrument rows with `auction_type` (DAILY_CLOSE, BENCHMARK_FIXING, INTRADAY), `trigger_time` (UTC), `timezone` (for DST-aware recurrence), `recurrence` (cron-like expression), and `enabled` flag.
2. **DST-aware recurrence:** use the instrument's timezone to compute the next trigger time; handle DST transitions (21:00 vs 22:00 UTC Sunday open) via the Phase-15 Task 15.3.11 session calendar.
3. **Daily close auction:** at 22:00 UTC Friday (or admin-configured daily close), execute a 5-minute call auction (same machinery as Task 15.3.6 reopening auction); MOC orders queued pre-close are injected into the uncross; unfilled remainder cancelled.
4. **Benchmark fixing scheduler:** at 16:00 UTC (London 4PM), 14:15 CET (ECB), and 09:55 JST (Tokyo), trigger FIXING order execution at the published rate ± spread (Phase 19.5 Price Oracle). Record the fixing rate and execution timestamp.
5. **Auction failure handling:** if a daily close auction cannot clear (zero liquidity, crossed book), extend by 30s increments (max 3 extensions) then transition to SUSPENDED with `AUCTION_CLEARING_FAILED` (§7.3).
6. **Admin API:** `GET /api/v1/admin/instruments/{symbol}/auction-calendar` and `PUT /api/v1/admin/instruments/{symbol}/auction-calendar` (dual control per §7.2 matrix).

**Definition of Done (Acceptance Criteria):**
* [x] Per-instrument auction calendar with DST-aware recurrence stored and queryable — `auction_calendar` (migration 087) per-instrument rows (`auction_type/trigger_time/timezone/recurrence` DAILY|WEEKDAYS|MON–FRI selection/`enabled`); `NextOccurrence` resolves local marks through the row `time.Location` so DST shifts are correct; `ReplaceCalendar` runs inside the four-eyes tx (`OpInstrumentCalendar`); `GET /api/v1/admin/instruments/{symbol}/auction-calendar` reads it. `TestCalendarReplaceIntegration` green
* [x] Daily close auction executes with MOC order injection and uncross — `AuctionScheduler` (5s sweep) arms the `instrument:auction:{symbol}` `CALL:{deadline}` key (5-min window) at each due DAILY_CLOSE row and seeds `instrument:auction:{symbol}:queue` with live MOC+MOO order IDs; the engine AuctionManager (Task 15.3.6 machinery) performs the uncross and writes `:result` (CLEARED|FAILED); on CLEARED the remainder is mass-cancelled via `InstrumentOrderCanceller` (LIMIT/MOC/MOO/STOP_LIMIT scope). Auction occurrence is surfaced on the ops board + WS `instrument_status` `AUCTION_CALL`/`AUCTION_COMPLETE`/`AUCTION_FAILED` events
* [x] Benchmark fixing scheduler triggers FIXING orders at London/ECB/Tokyo fix times — `FixingScheduler` (30s sweep) materializes `benchmark_fixings` rows (migration 222) for due BENCHMARK_FIXING marks, queues FIXING orders via `instrument:auction:{symbol}:queue` and publishes `instrument:fixing:{symbol}` = `FIXING:{benchmark}:{rate}:{unix_ns}` for the engine; `PriceSource` seam (Phase-19.5 oracle) — without a wired source the row lands `SKIPPED` (fail-closed, no fabricated rate). `TestFixingSchedulerIntegration` covers SKIPPED + after-holiday + dup-gap paths
* [x] Auction failure handling matches §7.3 (extend → SUSPENDED) — missing/FAILED `:result` → `EXTEND` +30s (max 3 extensions) → terminal failure writes `SUSPENDED` + `AUCTION_CLEARING_FAILED` through the admin.Log+audit-chain shape with explicit `auction` reason and clears the auction key family
* [x] Admin API functional with dual control — `PUT /api/v1/admin/instruments/{symbol}/auction-calendar` files `OpInstrumentCalendar` (full replacement executes in the approval tx, one scheduled occurrence guaranteed); GET is privileged single-eyes. Handlers `api/handlers_listing_ops.go`; routes live in `routes_v1.go`

**SDD Checklist:**
- [x] Spec checkpoint: daily closing-auction calendar with MOC/FIXING execution (§24 #401) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `TestNextOccurrence`/`TestDailyCloseAuctionEntry`/`TestCalendarReplaceIntegration`/`TestFixingSchedulerIntegration` green; `go build/vet/test ./internal/instruments` green

---

## 15.4 Deliverables

- Instrument lifecycle state machine (7 states; CANCEL_ONLY added, supersedes 6)
- Admin instrument management API
- C++ core instrument status enforcement
- Trading-hours enforcement (24/5 window + schedule overrides)
- Trade bust & price-adjust workflow (dual control + GL reversal)
- Reopening call auction (HALT/SUSPEND→ACTIVE + weekly open)
- Persistent CANCEL_ONLY state with cancel-path availability
- Reopening auction failure handler & crossed-book quarantine protocol (Task 15.3.10)
- Production instrument reference seed, session calendar & tenor grid (Task 15.3.11)
- Operations console backend: listing proposals, delisting ladder & ops board (Task 15.3.12)
- Daily closing-auction calendar + benchmark-fixing scheduler with MOC/FIXING execution (Task 15.3.13)

---

## 15.5 Dependencies

- Phases 7, 11, 14; Phase-19 Task 19.3.3 (auction machinery reused by Task 15.3.6)

---

## 15.6 Duration Estimate

6–10 days (supersedes prior 6–9 — Task 15.3.13 daily closing-auction calendar added 2026-09-27, feature completeness audit #36; prior supersedes 6–9 — Task 15.3.12 operations console added 2026-09-27, remediation #26; prior supersedes 6–8 — Task 15.3.11 reference/session/tenor added 2026-09-27, remediation #24):
- Task 15.3.1 (State machine): 2 days
- Task 15.3.2 (Admin API): 1.5 days
- Task 15.3.3 (C++ core): 1 day
- Task 15.3.4 (Trading hours): 1 day
- Task 15.3.5 (Trade bust): 0.5 day
- Task 15.3.6 (Reopening auction): 1 day
- Task 15.3.10 (Auction clearing failure & crossed-book quarantine): 0.5 day
- Task 15.3.11 (Instrument reference, sessions & tenor grid): 1 day
- Task 15.3.12 (Operations console backend): 1 day
- Task 15.3.13 (Daily closing-auction calendar): 1 day (added to itemization, feature completeness audit #36)
- Testing: 1 day

---

## 15.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | All 7 instrument states implemented (DRAFT, ACTIVE, CANCEL_ONLY, RESTRICTED, SUSPENDED, HALTED, DELISTED; supersedes prior 6-state criterion) |
| 2 | DRAFT: instrument created, not tradable |
| 3 | RESTRICTED: limit orders only; market/stop orders rejected (per spec §7.1; supersedes prior "reduced leverage, no new shorts") |
| 4 | SUSPENDED: no new orders, existing orders cancelled after 5min cancel-only grace |
| 5 | HALTED: no new orders, existing orders remain |
| 6 | DELISTED: no trading, grace period for position close |
| 7 | Grace periods: SUSPENDED 5min cancel-only (spec §7.1); RESTRICTED 24h; DELISTED 30d (§24 #119) |
| 8 | Dual control per spec §7.2: create/resume/delist/global-halt require it; suspend does not (supersedes prior "SUSPEND/DELIST") |
| 9 | Every transition audit logged with reason, admin ID, timestamp |
| 10 | All instrument management endpoints work |
| 11 | State transitions validated (invalid transitions rejected) |
| 12 | C++ core respects instrument status |
| 13 | RESTRICTED in C++ core: limit orders only; market/stop rejected |
| 14 | SUSPENDED/HALTED/DELISTED in C++ core: reject with specific error code |
| 15 | Instrument status refresh every 1s from Redis |
| 16 | Resume endpoint covers SUSPENDED→ACTIVE, RESTRICTED→ACTIVE, HALTED→ACTIVE (spec §7.1) |
| 17 | Trading hours enforced: orders outside 24/5 window rejected `MARKET_CLOSED`; weekend close Fri 22:00 UTC / open Sun 21:00 UTC |
| 18 | Dual-control trade bust: balances/positions/fees reversed with balanced GL entries; settled trades rejected (§24 #138) |
| 19 | Price-adjust posts GL delta + amends settlement; busted trades retained flagged, never deleted |
| 20 | Reopening call auction on HALT/SUSPEND→ACTIVE + weekly open: 5-min CALL default + EXTEND cap, indicative price streamed, single-price uncross (§24 #142) |
| 21 | 24/5 weekly session state machine (OPEN/PRE_CLOSE/CLOSED/PRE_OPEN) enforces weekend closure, Tom-Next rollover, and Sunday reopening auction (§24 #217) |
| 22 | Instrument maintenance: maker-checker approval for creation/parameter changes; effective-date scheduling; delisting RESTRICTED→DELISTED with FIX/WS notification (§24 #234) |
| 23 | Persistent CANCEL_ONLY rejects new/replace/amend requests, preserves resting orders, and keeps every cancel path available (§24 #290) |
| 24 | Reopening auction clearing failures transition instrument to SUSPENDED with AUCTION_CLEARING_FAILED; crossed-book state triggers fail-closed quarantine (§24 #316) |
| 25 | Seeded per-symbol tick/lot/notional/decimals reference; DST/holiday-aware 24/5 sessions with value-date blocking (VALUE_DATE_ON_HOLIDAY); tenor grid with calendar-correct value dates; no corporate actions in spot FX (§24 #343) |
| 26 | Listing proposals auto-check and schedule activation; delisting previews impact and executes the grace ladder; ops-board transitions carry approval pairs (§24 #352) |
| 27 | Daily closing-auction calendar: per-instrument DST-aware recurrence; MOC orders execute at daily close uncross; FIXING orders at benchmark times (§24 #401) |
