# Phase 18 — FIX Protocol Gateway

**Duration:** 16–19 days (unchanged; §18.6 itemization reconciled 2026-09-27, remediation #35: Task 18.3.16 added) (supersedes prior 14–17 — Task 18.3.14 Smart Order Routing (SOR) & External Liquidity Aggregation added 2026-09-15)
**Dependencies:** Phases 2, 5, 6
**Spec Reference:** §9 (FIX Protocol)

---

## 18.1 Objectives

Implement the FIX 4.4 gateway using quickfix-go: session management, order submission (NewOrderSingle), cancellation (OrderCancelRequest), modification (OrderCancelReplaceRequest), execution reports, market data (MarketDataSnapshotFullRefresh, MarketDataIncrementalRefresh), Prime Brokerage Drop Copy & trade affirmation (Traiana), FIX Mass Quoting (Tag 35=i) with 100% firm liquidity, and SBE binary gateway specification.

---

## 18.2 Prerequisites

- Phases 2, 5, 6 complete

---

## 18.3 Tasks

### Task 18.3.1: FIX Session Management

**Objective:** Implement FIX session management with quickfix-go.

**File Locations:** `services/cmd/fixgateway/main.go`, `services/internal/fix/session.go`

**Implementation:**
1. FIX 4.4 protocol.
2. Session types: initiator (we connect to client) and acceptor (client connects to us).
3. Session persistence: PostgreSQL `fix_sessions` table (spec §5.20).
4. Heartbeat: 30s default, configurable.
5. Sequence numbers: persisted, reset on daily logon.
6. Resend/gap-fill (spec §9.3, §24 #54): on `ResendRequest(7<16)=begin,end`, replay persisted messages with `PossDupFlag(43)=Y`; for non-order messages respond with `SequenceReset(4)` gap-fill; incoming gaps detected from `MsgSeqNum(34)` jumps trigger our own `ResendRequest`.
7. Logon timeout: 10s; logout timeout: 5s (spec §9.3).
8. Encryption: TLS 1.3.
9. Authentication: SenderCompID + API key.
10. **Migration note:** `migrations/030_create_fix_sessions.up.sql` — `fix_sessions` table per spec §5.20 (id, session_id, protocol_version, sender_seq_num, target_seq_num, status, last_heartbeat_at, created_at, updated_at).

**Definition of Done (Acceptance Criteria):**
* [ ] FIX 4.4 sessions established (initiator + acceptor)
* [ ] Heartbeat works (30s default)
* [ ] Sequence numbers persisted and reset on daily logon
* [ ] Resend request handled: gap-fill with possible-duplicate flag; SequenceReset for non-order messages (spec §9.3)
* [ ] Incoming sequence gap triggers ResendRequest; recovered messages applied exactly once
* [ ] Logon timeout 10s / logout timeout 5s enforced
* [ ] TLS 1.3 encryption
* [ ] SenderCompID + API key authentication

**SDD Checklist:**
- [ ] Spec checkpoint: FIX 4.4 session management — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.2: Order Submission via FIX

**Objective:** Implement order submission, cancellation, modification via FIX.

**File Locations:** `services/internal/fix/orders.go`

**Implementation:**
1. NewOrderSingle (D): submit order → Aeron → C++ core.
2. OrderCancelRequest (F): cancel order.
3. OrderCancelReplaceRequest (G): modify order.
4. ExecutionReport (8): sent on order acceptance, partial fill, full fill, cancellation, rejection.
5. Order status mapping: FIX OrdStatus ↔ internal status.

**Definition of Done (Acceptance Criteria):**
* [ ] NewOrderSingle submits order to C++ core via Aeron
* [ ] OrderCancelRequest cancels order
* [ ] OrderCancelReplaceRequest modifies order
* [ ] ExecutionReport sent on all order events
* [ ] Order status correctly mapped

**SDD Checklist:**
- [ ] Spec checkpoint: FIX order submission — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.3: FIX Market Data

**Objective:** Implement FIX market data distribution.

**File Locations:** `services/internal/fix/marketdata.go`

**Implementation:**
1. MarketDataRequest (V): client requests market data.
2. MarketDataSnapshotFullRefresh (W): full book snapshot.
3. MarketDataIncrementalRefresh (X): incremental updates.
4. Subscription: per-session, per-symbol.

**Definition of Done (Acceptance Criteria):**
* [ ] MarketDataRequest processed
* [ ] Full snapshot sent on subscription
* [ ] Incremental updates sent on book changes
* [ ] Per-session subscription management

**SDD Checklist:**
- [ ] Spec checkpoint: FIX market data — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.4: FIX Drop Copy

**Objective:** Implement FIX drop copy for compliance.

**File Locations:** `services/internal/fix/dropcopy.go`

**Implementation:**
1. All execution reports copied to a separate FIX session (compliance).
2. Read-only: no order submission on drop copy session.
3. Used for regulatory reporting and audit.

**Definition of Done (Acceptance Criteria):**
* [ ] All execution reports copied to drop copy session
* [ ] Drop copy session is read-only
* [ ] Used for regulatory reporting

**SDD Checklist:**
- [ ] Spec checkpoint: FIX drop copy — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.5: FIX 5.0 SP2 for Derivatives

**Objective:** Implement FIX 5.0 SP2 protocol support for derivatives and institutional flow (per spec §9.1).

**File Locations:** `services/internal/fix/fix50sp2.go`

**Implementation:**
1. FIX 5.0 SP2 protocol support alongside FIX 4.4.
2. Extended for derivatives: forwards, swaps, NDFs, options.
3. FX-specific tags (per spec §9.2): Currency (15), SecurityType (167 = FORWARD/SWAP/NDF/OPT), StrikePrice (202), SettlementType (9501), SettlementDate (9502), NoPartyIDs (9018).
4. Session management: same heartbeat, sequence persistence, TLS 1.3 as FIX 4.4.
5. Derivative-specific order fields: value date, fixing date, barrier level, option type.

**Definition of Done (Acceptance Criteria):**
* [ ] FIX 5.0 SP2 sessions established (initiator + acceptor)
* [ ] Derivative orders (forwards, swaps, NDFs, options) submitted via FIX 5.0 SP2
* [ ] FX-specific tags supported (Currency, SecurityType, StrikePrice, SettlementType, SettlementDate, NoPartyIDs)
* [ ] Derivative-specific fields (value date, fixing date, barrier level) supported
* [ ] TLS 1.3 encryption on FIX 5.0 SP2 sessions
* [ ] Execution reports for derivative orders sent via FIX 5.0 SP2

**SDD Checklist:**
- [ ] Spec checkpoint: FIX 5.0 SP2 for derivatives and institutional flow — defined first, validated against spec
- [ ] Spec checkpoint: FX-specific FIX tags (Currency, SettlementType, NoPartyIDs) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

**Derivative dependency note:** The FIX 5.0 SP2 protocol layer (session management, tags, TLS) is built in Phase 18. Derivative instruments and their trading logic are owned by Phase 22 (FX Derivatives Foundation), which runs after Phase 18. Acceptance criteria requiring "Derivative orders (forwards, swaps, NDFs, options) submitted via FIX 5.0 SP2" and "Execution reports for derivative orders sent via FIX 5.0 SP2" can only be fully validated after Phase 22 derivative instruments exist. During Phase 18, validate the protocol layer using **stub derivative instruments** (test-only instrument definitions with derivative SecurityType tags). Phase 22 integration testing validates the full derivative FIX 5.0 SP2 flow with real instruments.

---

### Task 18.3.6: PB Drop Copy & Electronic Trade Affirmation

**Objective:** Implement real-time Prime Brokerage Drop Copy sessions and trade affirmation feeds for Prime Brokers (Traiana Harmony, MarkitSERV, standard FIX Drop Copy) using `prime_brokers` and `pb_giveup_trades` schemas (`migrations/037_create_prime_brokerage.up.sql`) per spec §5.22, §9.2, §24 #125.

**File Locations:** `services/internal/fix/pb_dropcopy.go`, `services/internal/fix/affirmation.go`

**Implementation:**
1. Dedicated PB Drop Copy sessions: route execution reports to the client's prime broker FIX session immediately upon fill.
2. FX Party Identification tags: populate PartyID (448), PartyIDSource (447 = D / BIC), and PartyRole (452 = 1 Executing Firm, 3 Client ID, 13 Order Origination Trader, 36 Prime Broker).
3. Trade affirmation integration: export real-time trade feed to Traiana Harmony messaging gateway and MarkitSERV trade affirmation endpoints.
4. Two-way status synchronization: update `pb_giveup_trades` table status (`PENDING` -> `AFFIRMED` / `REJECTED`) based on affirmation responses.
5. Affirmation timeout monitoring: flag trades un-affirmed after configurable window (default 60s) for middle-office break alerts.

**Definition of Done (Acceptance Criteria):**
* [ ] Dedicated PB Drop Copy sessions route executions in real time
* [ ] FIX PartyID (448), PartyIDSource (447), PartyRole (452) populated per spec §9.2
* [ ] Traiana Harmony / MarkitSERV affirmation feeds integrated
* [ ] Status updates tracked in `pb_giveup_trades` table
* [ ] Affirmation timeouts alert middle office

**SDD Checklist:**
- [ ] Spec checkpoint: PB drop copy & trade affirmation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.7: FIX Mass Quoting (Tag 35=i) & Quote Cancel (Tag 35=Z)

**Objective:** Implement FIX Mass Quoting (Tag 35=i) and Mass Quote Cancel (Tag 35=Z) for institutional Liquidity Providers (LPs) per spec §9.4, with strict 100% Firm Liquidity (No Last Look) rulebook policy per FX Global Code Principle 17 (spec §6.4, §24 #128).

**File Locations:** `services/internal/fix/massquote.go`, `core/src/matching/MassQuote.cpp`

**Implementation:**
1. Handle `MassQuote` (35=i): multi-instrument quote updates in a single network packet containing `QuoteSetID` and repeating group of `QuoteEntryID`, `Symbol`, `BidPx`, `BidSize`, `OfferPx`, `OfferSize`.
2. Core integration: stream quotes directly to matching engine via Aeron SPSC IPC with atomic replacement of preceding LP quote level.
3. Emit `MassQuoteAcknowledgement` (35=b) with `QuoteStatus` per entry (`0 = Accepted`, `5 = Rejected`).
4. Handle `QuoteCancel` (35=Z): support QuoteCancelType (`1 = Cancel per symbol`, `4 = Cancel all quotes for LP`).
5. Enforce 100% Firm Liquidity rule: quotes are binding; reject any quote with non-zero hold times; incoming marketable client orders execute against mass quotes with zero last look or subjective confirmation delays.

**Definition of Done (Acceptance Criteria):**
* [ ] MassQuote (35=i) parsed and applied across multiple currency pairs
* [ ] MassQuoteAcknowledgement (35=b) returned with status
* [ ] QuoteCancel (35=Z) cancels all or symbol quotes atomically
* [ ] Strict 100% Firm Liquidity enforced: no last look, zero hold time, immediate execution

**SDD Checklist:**
- [ ] Spec checkpoint: FIX mass quoting & firm liquidity — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.8: Simple Binary Encoding (SBE) / Aeron Binary Gateway Specification

**Objective:** Define the high-performance ultra-low-latency binary gateway specification using Simple Binary Encoding (SBE) over Aeron UDP kernel bypass for institutional market makers (spec §9.5, §24 #128).

**File Locations:** `core/schema/sbe_messages.xml`, `core/include/sbe/`

**Implementation:**
1. Author canonical SBE XML schema defining messages: `NewOrderSingle`, `OrderCancel`, `OrderExecutionReport`, `MassQuote`, `QuoteCancel`.
2. Binary struct alignment: align byte fields directly with C++ matching engine POD types to achieve zero-copy deserialization.
3. Ingress latency target: design pipeline for sub-5µs gateway ingress (measured from NIC packet arrival to matching engine ring buffer).
4. SBE message specification document detailing header format, schema ID, versioning rules, and Aeron channel binding.

**Definition of Done (Acceptance Criteria):**
* [ ] SBE XML message schema defined and validated
* [ ] Binary layouts match C++ engine structs with zero padding gaps
* [ ] Aeron UDP transport channel configuration specified
* [ ] Sub-5µs gateway ingress architecture documented

**SDD Checklist:**
- [ ] Spec checkpoint: SBE / Aeron binary gateway spec — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.9: FIX Session Entitlement, Cancel-on-Disconnect & Throttling

**Objective:** Bind every FIX session to an account with instrument entitlements, add cancel-on-disconnect, and per-session inbound throttling per spec §5.20/§9.3 (§24 #135–137, #153). Added 2026-09-15.

**File Locations:** `services/internal/fix/session.go` (extend), `migrations/046_fix_sessions_entitlement.up.sql`

**Implementation:**
1. `fix_sessions` gains `account_id` (required), `allowed_instruments` (array or NULL=all), `cancel_on_disconnect BOOL` (default true), `max_msgs_per_sec INT` (default 100 per spec §9.3) — migration 046.
2. **Entitlement:** every order message validated: session's `account_id` is stamped on the order (Tag 1 Account must match or be a sub-account of the bound account); instrument must be in `allowed_instruments`; violations rejected `SESSION_NOT_ENTITLED` (BusinessMessageReject 35=j — terminology corrected, remediation #35: Reject is 35=3 per spec §9.9).
3. **Cancel-on-disconnect:** on abnormal disconnect (TCP drop, heartbeat timeout, no orderly Logout), all open orders owned by that session are mass-cancelled (reuses Task 5.3.25 mass-cancel path scoped by session); orderly Logout leaves orders unless `cancel_on_logout` configured.
4. **Per-session throttle:** token bucket per session on inbound messages; excess messages rejected with `SESSION_THROTTLED` reject — never silently dropped.
5. Entitlement/throttle config admin-managed via `PUT /api/v1/admin/fix-sessions/{id}` (Risk Manager+).
6. **Venue-wide reconnect pacing (amended 2026-09-27, remediation #27):** after a mass disconnect (gateway restart, DR failover), logons are admitted in staged windows per session class (MM → institutional → retail, 500ms stagger) so the reconnect storm cannot re-trip the outage; overflow logons queue with `SESSION_THROTTLED` rather than dropping.

**Migration note:** `migrations/046_fix_sessions_entitlement.up.sql` — `fix_sessions.account_id`, `allowed_instruments`, `cancel_on_disconnect`, `max_msgs_per_sec` (spec §5.20).

**Definition of Done (Acceptance Criteria):**
* [ ] Order on non-entitled account/instrument rejected SESSION_NOT_ENTITLED (§24 #135)
* [ ] Abnormal disconnect mass-cancels that session's open orders (§24 #136)
* [ ] Inbound rate beyond `max_msgs_per_sec` rejected, not dropped (§24 #137)
* [ ] Orderly Logout does not cancel unless configured

**SDD Checklist:**
- [ ] Spec checkpoint: FIX entitlement + CoD + throttle (§9.3, §24 #135–137) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: reconnect during CoD mass-cancel, session bound to sub-account hierarchy, throttle burst at exact boundary

---

### Task 18.3.10: Market-Maker Program & MMP Protection

**Objective:** Implement the market-maker program per spec §5.27/§9.6 (§24 #139): quoting obligations, compliance monitoring, MMP mass-cancel protection, and rebate reconciliation. Added 2026-09-15.

**File Locations:** `services/internal/marketmaking/program.go`, `migrations/045_market_maker_program.up.sql`

**Implementation:**
1. `mm_programs` table (migration 045) columns per spec §5.27/§9.6: `id`, `account_id`, `instrument_id` (NULL = program-wide), `min_quote_size`, `max_spread_bps`, `presence_pct`, `mmp_max_fills`, `mmp_window_ms`, `rebate_bps`, `status` (+ `otr_allowance` column for the MM OTR allowance used by Task 13.3.6).
2. Obligations: registered MM must maintain two-sided quotes ≥ `min_quote_size` within `max_spread_bps` for ≥ `presence_pct` of the trading day (sampled per minute); compliance tracked in `mm_compliance` daily rollup.
3. **MMP protection:** account configures `mmp_max_fills` per instrument per window (e.g., 10 fills/5s); on breach, engine mass-cancels all that MM's resting orders on the instrument (Task 5.3.25 path) and flags `MM_OBLIGATION_BREACH` (spec §23) on the session.
4. Breach handling: 3 daily compliance failures in a rolling week → program suspension (Risk Manager review); rebate accrual pauses during suspension.
5. Rebates: maker rebates accrued per fill (`rebate_bps`), posted through the GL (Phase-3 Task 3.3.6) monthly; reconciled in Phase-24 settlement reporting.

**Migration note:** `migrations/045_market_maker_program.up.sql` — `mm_programs` + `mm_compliance` tables (spec §5.27).

**Definition of Done (Acceptance Criteria):**
* [ ] Quoting obligations tracked (size/spread/presence) with daily compliance rollup
* [ ] MMP triggers mass-cancel after configured fill count per window (§24 #139)
* [ ] Repeated compliance breach → program suspension; rebate accrual pauses
* [ ] Maker rebates accrued per fill and posted to GL

**SDD Checklist:**
- [ ] Spec checkpoint: MM program + MMP protection (§9.6, §24 #139) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: MMP during auction uncross, presence sampling across 24/5 open/close, suspension with resting quotes

---

### Task 18.3.11: FIXS Mutual TLS & Client Certification

**Objective:** Enforce FIX-over-TLS transport identity and production conformance certification per spec §9.7/§24 #167. Added 2026-09-15.

**File Locations:** `services/internal/fix/tls.go`, `services/internal/fix/certification/`, `migrations/052_fix_certification.up.sql`

**Implementation:**
1. Implement FIX Trading Community FIXS on TLS 1.3; require mutual TLS for order-entry/admin sessions, with certificate SAN/fingerprint bound to approved SenderCompID, account, environment, and source network.
2. Validate certificate chain, expiry, revocation/OCSP policy, and allowed cipher/profile; support overlap-based dual-certificate rotation without session downtime.
3. Add versioned certification harness cases for logon/logout, heartbeat/TestRequest, resend/gap-fill, PossDup duplicate suppression, cancel/replace, session/business rejects, malformed messages, entitlement, throttle, cancel-on-disconnect, and recovery.
4. Persist certification by participant, client build, protocol dictionary, venue schema, environment, result, evidence hash, expiry, and revocation status; production session enablement fails closed without a current passing certification.

**Migration note:** `migrations/052_fix_certification.up.sql` creates `fix_certifications` and adds certificate identity/rotation fields to `fix_sessions`.

**Definition of Done (Acceptance Criteria):**
* [ ] FIXS TLS 1.3 mutual authentication binds client certificate to CompID/account/environment
* [ ] Invalid, expired, revoked, or mismatched certificates fail before FIX Logon is accepted
* [ ] Dual-certificate rollover preserves availability and audit history
* [ ] Full certification pack passes and evidence is versioned by client/schema build
* [ ] Uncertified or stale-certified client builds cannot open production order-entry sessions

**SDD Checklist:**
- [ ] Spec checkpoint: FIXS mTLS + production client certification (§9.7, §24 #167) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: cert rollover during reconnect, OCSP unavailable, CompID collision, schema version revoked

---

---

### Task 18.3.12: FIX Gateway Session Failover, Bidirectional Gap Fill & Secondary Gateway State Synchronization

**Objective:** Implement seamless FIX session failover across active-passive gateway instances, shared sequence state persistence, bidirectional ResendRequest gap fill, and duplicate execution suppression per spec §9.8 and §24 #189. Added 2026-09-15.

**File Locations:** `services/internal/fix/failover.go`, `services/internal/fix/session_store.go`, `services/internal/fix/resend.go`

**Implementation:**
1. Atomic sequence state synchronization: maintain per-session sequence state (`in_seq_num`, `out_seq_num`, `last_heard`) in Redis Sentinel cluster with fallback to PostgreSQL `fix_sessions` table; update offsets atomically with message receipt and transmission.
2. Secondary gateway standby: secondary FIX gateway instances operate in hot standby; DNS round-robin or load balancer health checks re-route client TCP connections to secondary upon primary termination.
3. Logon re-synchronization: upon client Logon (Tag 35=A) to secondary instance, validate expected sequence numbers:
   - If client sequence is greater than expected, immediately emit `ResendRequest (Tag 35=2)` from expected sequence to 0.
   - If client sequence is less than expected, reject or request gap fill according to session configuration.
4. Bidirectional gap fill: handle incoming `ResendRequest` from clients by querying outbound execution log archive; replay missing `ExecutionReport (Tag 35=8)` with `PossDupFlag (Tag 43)=Y` and `OrigSendingTime (Tag 122)`; replace administrative messages (Heartbeat, TestRequest) with `SequenceReset (Tag 35=4)` with `GapFillFlag (Tag 123)=Y`.
5. Duplicate order suppression: detect duplicate in-flight `NewOrderSingle (Tag 35=D)` messages replayed with `PossDupFlag=Y` using `ClOrdID` cache; emit matching existing ExecutionReport without re-submitting to the matching core.

**Definition of Done (Acceptance Criteria):**
* [ ] FIX gateway failover preserves sequence continuity without manual counter reset (§24 #189)
* [ ] ResendRequest triggers gap-fill replay with PossDupFlag=Y and SeqReset for admin messages
* [ ] Replayed NewOrderSingle messages are suppressed without duplicate executions in matching engine
* [ ] Full failover and sequence re-synchronization completes in <5s

**SDD Checklist:**
- [ ] Spec checkpoint: FIX gateway session failover and gap fill (§9.8, §24 #189) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: simultaneous bidirectional ResendRequest race, missing message gap across archive storage, client resets sequence mid-session

---

### Task 18.3.13: FIX Allocation Instructions & Allocation Reports (Tag 35=J / 35=AK)

**Objective:** Implement post-trade block allocation workflow per FIX 4.4/5.0 SP2 to enable institutional buy-side clients (asset managers, pension funds) to split block fills across sub-accounts and funds. Added 2026-09-15 (production-completeness audit remediation #4).

**File Locations:** `services/internal/fix/allocation.go`, `services/internal/fix/messages/allocation_instruction.go`, `services/internal/fix/messages/allocation_report.go`

**Implementation:**
1. **Allocation Instruction (Tag 35=J):** Parse incoming AllocationInstruction messages containing `AllocID (Tag 70)`, `AllocTransType (Tag 71)` (NEW/REPLACE/CANCEL), referenced `ExecID` or `OrderID` group, and allocation legs: `NoAllocs (Tag 78)` repeating group with `AllocAccount (Tag 79)`, `AllocQty (Tag 80)`, `AllocPrice` (average or specified).
2. **Allocation methods:** Support three methods: PRO_RATA (proportional to target), MANUAL (explicit qty per sub-account), STEP_OUT (allocate to external executing broker).
3. **Validation:** Total allocated quantity MUST equal executed quantity exactly — reject with `AllocationInstructionAck (Tag 35=P)` — message name corrected, remediation #35 (AllocationReportAck is 35=AL) status REJECTED if over/under-allocated. Validate all `AllocAccount` values exist and are linked to the same master institutional account.
4. **Allocation Report (Tag 35=AK):** Emit AllocationReport with `AllocStatus (Tag 87)` (ACCEPTED/PARTIAL_ACCEPT/REJECTED) and per-leg booking confirmations. Booked allocations generate individual trade records per sub-account in PostgreSQL with `parent_exec_id` linkage.
5. **Settlement impact:** Each allocation leg generates its own settlement obligation (Phase-24 netting applies per sub-account). PB Drop Copy (Task 18.3.6) receives allocation reports.
6. **Claim/correction:** Support `AllocTransType=REPLACE` to amend allocations pre-settlement and `CANCEL` to reverse. Corrections propagate to settlement (Phase-24 Task 24.3.10 allocation correction lifecycle).

**Definition of Done (Acceptance Criteria):**
* [ ] AllocationInstruction (35=J) parsed with NoAllocs legs; over/under-allocation rejected
* [ ] PRO_RATA, MANUAL, and STEP_OUT allocation methods work correctly
* [ ] AllocationReport (35=AK) emitted with per-leg booking status
* [ ] Each allocation leg creates individual trade record with parent_exec_id linkage
* [ ] Allocation REPLACE and CANCEL propagate corrections; immutable audit trail maintained
* [ ] PB Drop Copy receives allocation reports for all allocated sub-accounts

**SDD Checklist:**
- [ ] Spec checkpoint: FIX Allocation Instructions & Reports (Tag 35=J/35=AK) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: partial fill allocation, allocation across different settlement dates, step-out to external PB, concurrent allocation + cancel race

---


### Task 18.3.14: Smart Order Routing (SOR) & External Liquidity Aggregation

**Objective:** Implement outward FIX sessions for Smart Order Routing (SOR) to aggregate liquidity from external ECNs.

**File Locations:** `services/internal/features/18_3_14.go`

**Implementation:**
1. Maintain outward FIX sessions to external venues (ECNs, bank LPs).
2. Route orders when internal CLOB lacks liquidity: if internal L2 depth < min_depth_threshold or spread > max_spread_threshold, route to external venue(s).
3. **(amended 2026-09-20 — feature-completeness audit remediation #11):** SOR 5-state lifecycle: `PENDING → ROUTED → PARTIAL_FILL → FILLED → CANCELLED/EXPIRED`. Each external route generates a shadow order in the local OMS with `venue_id` + `external_order_id` for reconciliation.
4. Partial fill reconciliation: fills from external venues generate `FILL_BRIDGE` events via NATS JetStream back to the Phase-3 settlement service; shadow order tracks cumulative fill qty.
5. External venue non-response timeout: 500ms. On timeout, auto-cancel external order + re-route to next venue or reject back to client with `SOR_TIMEOUT`.
6. Race condition prevention: local CLOB and external venue orders never coexist for the same parent order. SOR routes exclusively — if local liquidity appears while routed, external cancel must complete before local re-submission.
7. Deduplication: `(parent_order_id, venue_id, external_order_id)` unique constraint prevents duplicate fills.

**Definition of Done (Acceptance Criteria):**
* [ ] SOR routes to external venues when internal CLOB depth/spread thresholds breached
* [ ] 5-state SOR lifecycle (PENDING→ROUTED→PARTIAL_FILL→FILLED→CANCELLED) with shadow orders
* [ ] External venue fills reconciled via FILL_BRIDGE events to Phase-3 settlement
* [ ] 500ms external timeout triggers auto-cancel + re-route or SOR_TIMEOUT rejection
* [ ] Race condition prevention: no concurrent local+external orders for same parent
* [ ] Tests passing for Smart Order Routing (SOR) & External Liquidity Aggregation

**SDD Checklist:**
- [ ] Spec checkpoint: Smart Order Routing (SOR) & External Liquidity Aggregation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.15: FIX Trading Session Status Broadcast (Tag 35=h)

**Objective:** Broadcast FIX `TradingSessionStatus` messages to all subscribed sessions when instrument or market state changes. Added 2026-09-20 (feature-completeness audit remediation #11).

**File Locations:** `services/internal/fix/session_status.go`

**Implementation:**
1. Subscribe to instrument state-change events from Phase-15 (NATS topic `instrument.state.{symbol}`).
2. Map instrument states to FIX TradSesStatus (Tag 340): ACTIVE → `2` (Open), HALTED → `3` (Closed), SUSPENDED → `5` (Pre-Close), RESTRICTED → `4` (Pre-Open) — TradSesStatus mapping corrected per the FIX 4.4 dictionary (remediation #35: the prior labels were swapped and 18 is not a standard TradSesStatus value; the internal CANCEL_ONLY state maps to no TradSesStatus value and is signalled via the auction channel instead), DELISTED → `6` (Closed).
3. Unsolicited push of `TradingSessionStatus` (Tag 35=h) to all FIX sessions subscribed to the affected instrument within 50ms of state change.
4. Include TradingSessionID (Tag 336), TradSesStatus (Tag 340), TradSesStatusRejReason (Tag 567) if applicable.
5. FIX clients can subscribe/unsubscribe to session status via `TradingSessionStatusRequest` (Tag 35=g).

**Definition of Done (Acceptance Criteria):**
* [ ] Instrument state changes trigger FIX TradingSessionStatus broadcast within 50ms
* [ ] All TradSesStatus codes correctly mapped from internal states
* [ ] FIX clients can subscribe/unsubscribe to session status updates
* [ ] HALT broadcast prevents algo rejection storms by informing clients before orders arrive

**SDD Checklist:**
- [ ] Spec checkpoint: FIX TradingSessionStatus — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: rapid state flapping (ACTIVE→HALTED→ACTIVE within 1s), client subscribed mid-transition

---

### Task 18.3.16: Dead-man switch via FIX

Dead-man switch via FIX — implement heartbeat-based countdown cancel-all for FIX sessions. Client sets countdown via `UserRequest (35=BE)` with `UserRequestType=4` (custom) and `CountdownMs` tag (custom tag 20001, min 1000, max 60000). Server maintains per-session timer; if not renewed before expiry, atomically cancels all resting orders for the session's bound account(s). Setting `CountdownMs=0` disables. Confirmation via `UserResponse (35=BF)` with countdown state. Timer state synchronized with REST/WS dead-man switch (Task 5.3.33) — a single account-level timer shared across all interfaces.

**SDD Checklist:**
- [ ] Spec checkpoint: FIX dead-man switch shares the canonical account timer with REST/WS (§24 #257) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.17: FIX SBE Production Transport and Graceful Maintenance Drain

**Objective:** Productionize standards-based FIX SBE alongside tag-value FIX and make planned drains explicit.

**Implementation:**
1. Provide TLS endpoints for tag-value requests/SBE responses and SBE requests/responses using the same business semantics, session entitlements, sequence rules, and drop-copy coverage.
2. Require Ed25519 session keys, TLS SNI, hostname validation, and schema ID/version negotiation; apply the Phase-06 schema lifecycle and retirement policy.
3. During maintenance, send FIX `News` advisories repeatedly until disconnect, preserve cancels, and direct clients to a ready replacement endpoint.
4. Certification covers tag-value↔SBE equivalence, schema upgrade/retirement, maintenance reconnect, and duplicate suppression.

**SDD Checklist:**
- [ ] Spec checkpoint: FIX SBE transport, Ed25519/SNI, schema lifecycle, and graceful News drain pass certification (§24 #284, #289) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 18.3.18: FIX Sequence Gap Resolution, Session Reject & CoD Recovery

**Objective:** Implement strict FIX message sequence gap recovery, standardized reject messages, and low-latency Cancel-on-Disconnect (CoD) execution per spec §2.7, §9.9, and §24 #319.

**Implementation:**
1. **Sequence Gap Recovery Protocol:** On inbound sequence gap, dispatch `ResendRequest (35=2)`. Handle unrecoverable gaps with `SequenceReset (35=4) GapFillFlag=Y`. If inbound gap exceeds 2,500 messages, terminate session immediately with `Logout (35=5) Text=EXCESSIVE_SEQUENCE_GAP` to defend memory against replay exhaustion attacks.
2. **Structured Reject Responses:** Standardize application-level rejections via `BusinessMessageReject (35=j)` with `BusinessRejectReason (380)` and execution rejections via `ExecutionReport (35=8)` with `OrdRejReason (103)`.
3. **Cancel-on-Disconnect (CoD) Execution:** When socket heartbeat fails (2 consecutive missed heartbeats) or TCP drops, cancel all resting non-exempt orders within 50ms and emit execution reports to drop-copy.

**Definition of Done (Acceptance Criteria):**
* [ ] Sequence gaps resolve cleanly via ResendRequest and GapFill
* [ ] Massive sequence gaps (>2,500) trigger fail-closed session Logout
* [ ] Cancel-on-disconnect purges open orders within 50ms of socket loss

**SDD Checklist:**
- [ ] Spec checkpoint: FIX sequence gap resolution, session reject, and CoD recovery (§24 #319) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 18.4 Deliverables

- FIX 4.4 gateway with session management
- Order submission/cancel/modify via FIX
- FIX market data (snapshot + incremental)
- FIX drop copy for compliance
- FIX 5.0 SP2 for derivatives and institutional flow
- Prime Brokerage Drop Copy & Electronic Trade Affirmation (Traiana/MarkitSERV)
- FIX Mass Quoting (Tag 35=i) & Quote Cancel (Tag 35=Z) with No Last Look enforcement
- Simple Binary Encoding (SBE) / Aeron binary gateway specification
- FIX session entitlement (account binding, allowed instruments) + cancel-on-disconnect + per-session throttle
- Market-maker program: quoting obligations, MMP mass-cancel protection, GL-reconciled rebates
- FIXS TLS 1.3 mutual authentication + versioned production client certification
- FIX gateway session failover, bidirectional gap fill & secondary gateway state synchronization (Task 18.3.12)
- FIX Allocation Instructions (35=J) & Allocation Reports (35=AK) with PRO_RATA/MANUAL/STEP_OUT methods (Task 18.3.13)
- Smart Order Routing (SOR) engine (Task 18.3.14)
- FIX Trading Session Status broadcast (Tag 35=h) for instrument state changes (Task 18.3.15)
- Shared dead-man switch; production FIX SBE, Ed25519/SNI, schema lifecycle, and graceful News drain
- FIX sequence gap resolution engine, reject formatting & CoD recovery (Task 18.3.18)

---

## 18.5 Dependencies

- Phases 2, 5, 6

---

## 18.6 Duration Estimate

16–19 days (supersedes prior 14–17 — Tasks 18.3.13–18.3.18 added, absorbed in range; reconciled to the file header and AGENTS Phase Index 2026-09-25):
- Task 18.3.1 (Sessions): 2 days
- Task 18.3.2 (Orders): 2 days
- Task 18.3.3 (Market data): 2 days
- Task 18.3.4 (Drop copy): 1 day
- Task 18.3.5 (FIX 5.0 SP2): 1.5 days
- Task 18.3.6 (PB Drop Copy & Affirmation): 1 day
- Task 18.3.7 (Mass Quoting & Firm Liquidity): 1.5 days
- Task 18.3.8 (SBE Gateway Spec): 1 day
- Task 18.3.9 (Entitlement/CoD/throttle): 1 day
- Task 18.3.10 (MM program + MMP): 1 day
- Task 18.3.11 (FIXS/mTLS + certification): 1.5 days
- Task 18.3.12 (FIX failover & gap fill): 0.5 day
- Task 18.3.13 (FIX allocations): 1 day
- Task 18.3.14 (SOR Engine): 1.5 days
- Task 18.3.15 (FIX TradingSessionStatus): 0.5 day
- Task 18.3.18 (FIX gap resolution & session reject formatting): 0.5 day
- Testing: 1 day

## 18.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | FIX 4.4 sessions established (initiator + acceptor) |
| 2 | Heartbeat works (30s default) |
| 3 | Sequence numbers persisted and reset on daily logon |
| 4 | TLS 1.3 encryption on FIX sessions |
| 5 | SenderCompID + API key authentication |
| 6 | NewOrderSingle submits order to C++ core via Aeron |
| 7 | OrderCancelRequest cancels order |
| 8 | OrderCancelReplaceRequest modifies order |
| 9 | ExecutionReport sent on all order events |
| 10 | Order status correctly mapped (FIX ↔ internal) |
| 11 | MarketDataRequest processed |
| 12 | Full snapshot sent on subscription |
| 13 | Incremental updates sent on book changes |
| 14 | Per-session subscription management (§24 #71) |
| 15 | All execution reports copied to drop copy session |
| 16 | Drop copy session is read-only |
| 17 | FIX 5.0 SP2 sessions established (initiator + acceptor) |
| 18 | Derivative orders submitted via FIX 5.0 SP2 |
| 19 | FX-specific tags supported (Currency, SecurityType, StrikePrice, SettlementType, SettlementDate, NoPartyIDs) (§24 #56) |
| 20 | Derivative-specific fields (value date, fixing date, barrier level) supported |
| 21 | TLS 1.3 encryption on FIX 5.0 SP2 sessions |
| 22 | Execution reports for derivative orders sent via FIX 5.0 SP2 (§24 #55) |
| 23 | Resend request: gap-fill with PossDupFlag; SequenceReset for non-order messages (§24 #54) |
| 24 | Incoming sequence gap triggers ResendRequest; messages applied exactly once |
| 25 | Logon timeout 10s / logout timeout 5s enforced (spec §9.3) |
| 26 | Prime Broker Drop Copy & Traiana trade affirmation feed emits real-time execution reports with PartyIDs (§9.2, §24 #125) |
| 27 | FIX Mass Quoting (Tag 35=i) & Quote Cancel (Tag 35=Z) processed with 100% firm liquidity (no last look) (§9.4, §6.4, §24 #128) |
| 28 | SBE binary gateway schema defined with zero-deserialization layout over Aeron (<5µs ingress) (§9.5, §24 #128) |
| 29 | Session bound to account_id; order on non-entitled account/instrument rejected SESSION_NOT_ENTITLED (§24 #135) |
| 30 | Abnormal disconnect mass-cancels that session's open orders; orderly Logout preserves them (§24 #136, #153) |
| 31 | Per-session throttle rejects excess messages (never silently dropped) (§24 #137) |
| 32 | MM obligations tracked (size/spread/presence) with daily compliance rollup; breach → suspension (§24 #139) |
| 33 | MMP mass-cancel triggers after configured fills per window; rebates accrued per fill via GL |
| 34 | FIXS TLS 1.3 mutual authentication binds certificate identity to CompID/account/environment (§24 #167) |
| 35 | Invalid/expired/revoked/mismatched certificate is rejected before FIX Logon; dual-certificate rollover is auditable |
| 36 | Versioned client certification pack passes; uncertified or stale-certified builds cannot open production order-entry sessions |
| 37 | FIX gateway session failover preserves bidirectional sequence numbers via shared state, re-synchronizes via ResendRequest (Tag 35=2) gap fill with PossDupFlag (Tag 43=Y), and prevents duplicate execution reports (§24 #189) |
| 38 | FIX AllocationInstruction (35=J) parsed with NoAllocs legs; total qty must equal exec qty exactly; over/under rejected (§24 #195) |
| 39 | AllocationReport (35=AK) emitted per allocation with ACCEPTED/REJECTED status and per-leg booking records |
| 40 | PRO_RATA, MANUAL, STEP_OUT allocation methods; corrections (REPLACE/CANCEL) propagate to settlement and PB Drop Copy |
| 41 | Smart Order Routing routes to external FIX venues when internal liquidity is thin |
| 42 | Execution reports from external venues map correctly to internal orders |
| 43 | SOR 5-state lifecycle with shadow orders; 500ms external timeout; FILL_BRIDGE reconciliation; no concurrent local+external orders (§24 #242) |
| 44 | FIX TradingSessionStatus (35=h) broadcast within 50ms of instrument state change; TradSesStatus codes mapped; subscribe/unsubscribe functional (§24 #243) |
| 45 | FIX dead-man switch shares the canonical account timer with REST/WS and atomically mass-cancels on expiry (§24 #257) |
| 46 | FIX SBE supports tag-value/SBE transport combinations, Ed25519+SNI, schema lifecycle, and graceful `News` maintenance drain (§24 #284, #289) |
| 47 | FIX gateway resolves sequence gaps via ResendRequest/GapFill; excessive gaps (>2,500) force Logout; CoD cancels resting orders within 50ms (§24 #319) |

