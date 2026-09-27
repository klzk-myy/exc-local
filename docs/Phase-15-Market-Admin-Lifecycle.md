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
* [ ] All 7 states implemented with correct transitions (CANCEL_ONLY added; supersedes prior 6)
* [ ] DRAFT: instrument created, not tradable
* [ ] RESTRICTED: limit orders only, market/stop rejected (spec §7.1)
* [ ] SUSPENDED: no new orders, existing cancelled after 5min grace
* [ ] HALTED: no new orders, existing remain
* [ ] DELISTED: no trading, grace period for position close
* [ ] Grace periods: SUSPENDED 5min cancel-only (spec §7.1); RESTRICTED 24h; DELISTED 30d
* [ ] Dual control per spec §7.2: create/resume/delist/global-halt require it; suspend does not
* [ ] Every transition audit logged

**SDD Checklist:**
- [ ] Spec checkpoint: instrument lifecycle 7 states including persistent CANCEL_ONLY (§24 #290) — defined first, validated against spec
- [ ] Spec checkpoint: SUSPENDED 5min cancel-only grace (spec §7.1); RESTRICTED 24h; DELISTED 30d — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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
* [ ] All instrument management endpoints work
* [ ] Dual control for create/resume/delist/global-halt; suspend is Compliance Officer+ without dual control (spec §7.2)
* [ ] Resume covers SUSPENDED→ACTIVE, RESTRICTED→ACTIVE, HALTED→ACTIVE (spec §7.1 transition graph)
* [ ] State transitions validated

**SDD Checklist:**
- [ ] Spec checkpoint: admin instrument management API — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 15.3.3: Instrument Status in C++ Core

**Objective:** C++ core respects instrument status for order acceptance.

**File Locations:** `core/src/risk/PreTradeChecker.cpp` (extend)

**Implementation:**
1. C++ core reads instrument status from Redis `instrument:status:{symbol}`.
2. ACTIVE: accept orders.
3. RESTRICTED: accept limit orders only — market/stop orders rejected (spec §7.1).
4. CANCEL_ONLY rejects new/replace/amend with `INSTRUMENT_CANCEL_ONLY` but permits cancel/mass-cancel; SUSPENDED/HALTED/DELISTED reject new orders with their specific codes.
5. Status refresh: every 1s from Redis.

**Definition of Done (Acceptance Criteria):**
* [ ] C++ core respects instrument status
* [ ] RESTRICTED: limit orders only; market/stop rejected (spec §7.1)
* [ ] SUSPENDED/HALTED/DELISTED: reject with specific code
* [ ] Status refresh every 1s

**SDD Checklist:**
- [ ] Spec checkpoint: C++ core respects instrument status — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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

**Definition of Done (Acceptance Criteria):**
* [ ] Orders outside 24/5 window rejected with `MARKET_CLOSED`; cancels allowed
* [ ] Weekend close Friday 22:00 UTC and re-open Sunday 21:00 UTC enforced automatically
* [ ] Holiday/schedule overrides configurable by admin

**SDD Checklist:**
- [ ] Spec checkpoint: 24/5 trading hours (spec §1) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: DST transitions, mid-week holiday override, orders submitted at exact open/close boundary

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
* [ ] Dual-control bust reverses balances/positions/fees with balanced GL reversal
* [ ] Price-adjust posts GL delta entries and amends settlement
* [ ] Settled/dispatched trades cannot be busted (rejected with TRADE_ALREADY_SETTLED)
* [ ] Busted trades retained, flagged, never deleted; both parties notified

**SDD Checklist:**
- [ ] Spec checkpoint: trade bust/price-adjust with dual control + GL reversal (§7.2, §24 #138) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: bust of a busted trade, bust after downstream hedging, partial-fill bust

---

### Task 15.3.6: Reopening Call Auction (HALT/SUSPEND→ACTIVE + Weekly Open)

**Objective:** Add a call-auction reopening phase so resumption after HALT/SUSPEND and the weekly 24/5 open run through price discovery instead of flipping straight to continuous trading (spec §7.1, §24 #142). Added 2026-09-15.

**File Locations:** `core/src/matching/AuctionManager.cpp`, `services/internal/admin/market_schedule.go` (extend)

**Implementation:**
1. Reopening auction state `CALL` on transition to ACTIVE from HALTED/SUSPENDED and at Sunday 21:00 UTC weekly open (when resting orders + weekend gap exist); admin may bypass via `resume` parameter `skip_auction=true` for trivial resumes.
2. CALL phase 5 min default (per spec §24 #142 — orders accumulate, no matching, indicative uncross price streamed); EXTEND up to a configurable cap if imbalance persists (machinery shared with the liquidation auction's CALL/EXTEND pattern, but reopening uses the longer market-open cadence, not the 5s liquidation cadence).
3. Uncross: single clearing price maximizing executable volume; orders execute at uncross price (price-time priority for allocation); unfilled orders remain in book → continuous ACTIVE.
4. Reuses Task 19.3.3/Phase-2 auction machinery; auction events emitted to WAL + WS `auction.indicative` for transparency (channel name aligned with spec §7.1, remediation #35 — supersedes the prior `auction.status`).

**Definition of Done (Acceptance Criteria):**
* [ ] HALT/SUSPEND→ACTIVE transitions route through CALL auction (5-min default + EXTEND cap, per spec §24 #142)
* [ ] Weekly reopen (Sun 21:00 UTC) uses call auction when weekend gap exists
* [ ] Uncross executes at single clearing price; residual orders rest → continuous trading
* [ ] Admin `skip_auction` bypass works for trivial resumes

**SDD Checklist:**
- [ ] Spec checkpoint: reopening auction on resume + weekly open (§7.1, §24 #142) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: empty book at reopen (skip auction → ACTIVE), uncross with only market orders, auction during degradation mode

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

**Definition of Done (Acceptance Criteria):**
* [ ] 4 session states implemented
* [ ] Friday 22:00 UTC closure and Sunday 21:00 UTC opening sequences enforced
* [ ] Session WS events broadcasted
* [ ] Session state persisted in Redis and accessible via API

**SDD Checklist:**
- [ ] Spec checkpoint: 24/5 Trading Session Lifecycle (§6.7, §24 #217) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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

**Definition of Done (Acceptance Criteria):**
* [ ] Maker-checker workflow enforced for instrument creation and parameter changes
* [ ] Effective-date scheduling activates changes at session boundary
* [ ] Emergency changes require Super Admin with P1 alert
* [ ] Delisting workflow: RESTRICTED → grace period → DELISTED with force-close
* [ ] FIX SecurityStatus and WS notifications sent on state changes
* [ ] Full audit trail in instrument_change_log

**SDD Checklist:**
- [ ] Spec checkpoint: instrument maintenance workflow — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: parameter change during scheduled maintenance, concurrent change requests for same instrument, delisting with open margin positions

---

### Task 15.3.9: Explicit `CANCEL_ONLY` Instrument State

**Objective:** Add a persistent operator-controlled state in which cancellations remain available while all new orders and amendments are rejected.

**Implementation:**
1. Extend the lifecycle enum with `CANCEL_ONLY`; transitions require Risk Manager or Compliance Officer authorization and an auditable reason.
2. Preserve resting orders indefinitely until clients cancel or an administrator transitions to SUSPENDED/HALTED; never force the five-minute SUSPENDED timer while in CANCEL_ONLY.
3. Publish the state through instruments REST, WS status, SBE security definitions, and FIX TradingSessionStatus/SecurityStatus.
4. Core pre-trade rejects new/replace/amend requests with `INSTRUMENT_CANCEL_ONLY`; cancel and mass-cancel always remain available.

**SDD Checklist:**
- [ ] Spec checkpoint: persistent CANCEL_ONLY rejects entry/amend while preserving all cancellation paths (§24 #290) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 15.3.10: Reopening Auction Clearing Failure & Crossed Book Quarantine

**Objective:** Implement error boundaries and fail-closed state transitions for reopening auction uncross failures and crossed-book anomalies per spec §2.7, §7.3, and §24 #316.

**Implementation:**
1. **Auction Clearing Timeout Fallback:** In `AuctionManager.cpp`, if the indicative uncross price cannot be formed after $3\times 30\text{s}$ extension phases, transition instrument state to `SUSPENDED` with error code `AUCTION_CLEARING_FAILED` (HTTP 409 — supersedes prior HTTP 503; auction clearing failure is a state conflict, not a service unavailable; aligned to spec §23) rather than forcing an arbitrary fill.
2. **Crossed-Book Quarantine:** If bids cross asks during continuous trading without an immediate match execution, immediately quarantine the instrument into `HALTED` (`CROSSED_BOOK_DETECTED`), preserve resting orders, and page the Risk Manager.
3. **Admin Resolution Workflow:** Provide `POST /api/v1/admin/instruments/{id}/uncross-override` requiring dual control to manually review book imbalances, cancel offending orders, and restart continuous trading.

**Definition of Done (Acceptance Criteria):**
* [ ] Unclearable auctions fail closed to SUSPENDED state
* [ ] Crossed book condition triggers immediate emergency halt
* [ ] Admin recovery workflow restores orderly matching under dual control

**SDD Checklist:**
- [ ] Spec checkpoint: Reopening auction clearing failure and crossed-book quarantine fail closed (§24 #316) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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
* [ ] Every listed symbol carries a complete reference row; gateway validates tick/lot/notional from it
* [ ] Session, DST, holiday and value-date rules enforced in-core and at the gateway with code
* [ ] Tenor/value-date math calendar-correct; redenomination path documented

**SDD Checklist:**
- [ ] Spec checkpoint: seeded instrument reference, calendar-correct sessions with value-date blocking, tenor grid, and stated no-corporate-actions rule (§24 #343) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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
* [ ] Proposals auto-check, route for review, and schedule activation with broadcast
* [ ] Delisting previews impact and executes the grace ladder with dual control
* [ ] Ops board serves state/timers/approvals; transitions audit-logged with approval pairs

**SDD Checklist:**
- [ ] Spec checkpoint: listing proposals with auto-checks, impact-previewed delisting ladder, and approval-paired ops-board transitions (§24 #352) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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
* [ ] Per-instrument auction calendar with DST-aware recurrence stored and queryable
* [ ] Daily close auction executes with MOC order injection and uncross
* [ ] Benchmark fixing scheduler triggers FIXING orders at London/ECB/Tokyo fix times
* [ ] Auction failure handling matches §7.3 (extend → SUSPENDED)
* [ ] Admin API functional with dual control

**SDD Checklist:**
- [ ] Spec checkpoint: daily closing-auction calendar with MOC/FIXING execution (§24 #401) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

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
