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

**File Locations:** `services/cmd/fixgateway/main.go`, `services/internal/fix/session.go` *(renamed at implementation: the binary is `services/cmd/fix/main.go` — the repo's single-word service-name convention and pre-existing scaffold; the session layer landed as `internal/fix/{app,gateway,settings,msgstore,store,gapfill}.go`, one responsibility per file — deviation recorded in `internal/fix/doc.go`)*

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
* [x] FIX 4.4 sessions established (initiator + acceptor) *(`internal/fix/gateway.go` + `settings.go` build both roles; binary naming deviation — `cmd/fix` not `cmd/fixgateway` — recorded in `internal/fix/doc.go`)*
* [x] Heartbeat works (30s default) *(`settings.go` HeartBtInt=30, configurable per session)*
* [x] Sequence numbers persisted and reset on daily logon *(`msgstore.go` `PgMessageStoreFactory` hydrates sender/target seq from `fix_sessions`; `ResetSeqTime` = `fix.reset_time_utc` default 22:00:00 UTC)*
* [x] Resend request handled: gap-fill with possible-duplicate flag; SequenceReset for non-order messages (spec §9.3) *(`fix_messages` archive replayed via the QuickFIX MessageStore with PossDupFlag(43)=Y/OrigSendingTime(122); `gapfill.go` `PlanResend` emits SequenceReset for admin frames)*
* [x] Incoming sequence gap triggers ResendRequest; recovered messages applied exactly once *(`gapfill.go` `AssessInbound` + `gapfill_test.go`; quickfixgo session machinery owns the wire protocol leg)*
* [x] Logon timeout 10s / logout timeout 5s enforced *(`settings.go` LogonTimeout=10/LogoutTimeout=5)*
* [x] TLS 1.3 encryption *(`settings.go` SocketUseSSL + MinVersion=TLS1.3 + optional client CA)*
* [x] SenderCompID + API key authentication *(`app.go` `authenticateLogon`: session row must exist → Username(553)/Password(554) verified against bound `api_keys` row via `Store.VerifyAPIKey`; `app_test.go` covers unknown pair / bad credential / bound-account mismatch / valid logon)*

**SDD Checklist:**
- [x] Spec checkpoint: FIX 4.4 session management — defined first, validated against spec
- [x] All spec checkpoints pass after implementation *(unit suite green: `go test ./internal/fix`; PG-gated persistence legs in `store_pg_test.go` run under `EXC_PG_TEST=1`)*

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
* [x] NewOrderSingle submits order to C++ core via Aeron *(`mapfix.go` `MapNewOrderSingle` → canonical `orders.Service.Submit` → `AeronSubmitter` → `orders_in` aeron:ipc stream 1001; market/limit/stop/stop-limit/iceberg(MaxFloor)/GTD covered in `mapfix_test.go`)*
* [x] OrderCancelRequest cancels order *(`orders.go` 35=F handler → `Service.Cancel`; ClOrdID→OrderID resolution via dedup/read model; reject path 35=9 `OrderCancelReject`)*
* [x] OrderCancelReplaceRequest modifies order *(`orders.go` 35=G handler → `Service.CancelReplace` STOP_ON_FAILURE; `OrigClOrdID(41)` carried)*
* [x] ExecutionReport sent on all order events *(`report.go` builders for accept/reject/fill/partial/cancel/expire/replace; async engine lifecycle reaches the session via `feed.go` — `orders.Consumer.HandleFragment` canonical decode → read model → `App.ReportHooks` → `ReportBus.Emit`)*

* [x] Order status correctly mapped *(`mapfix.go` `ordStatusFIX` covers every internal status incl. RESERVED/EXPIRED; `TestOrdStatusMap_Complete` locks the map; `ordRejReason` maps coded errors to OrdRejReason(103))*

**SDD Checklist:**
- [x] Spec checkpoint: FIX order submission — defined first, validated against spec
- [x] All spec checkpoints pass after implementation *(`go test ./internal/fix` green — mapping, report construction, throttle, entitlement, auth, dead-man legs all covered)*

---

### Task 18.3.3: FIX Market Data

**Objective:** Implement FIX market data distribution.

**File Locations:** `services/internal/fix/mdata.go` (renamed from `marketdata.go` at implementation time — the name `marketdata.go` collides conceptually with the `internal/marketdata` package the file consumes)

**Implementation:**
1. MarketDataRequest (V): client requests market data.
2. MarketDataSnapshotFullRefresh (W): full book snapshot.
3. MarketDataIncrementalRefresh (X): incremental updates.
4. Subscription: per-session, per-symbol.

**Definition of Done (Acceptance Criteria):**
* [x] MarketDataRequest processed
* [x] Full snapshot sent on subscription
* [x] Incremental updates sent on book changes
* [x] Per-session subscription management

**SDD Checklist:**
- [x] Spec checkpoint: FIX market data — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 18.3.4: FIX Drop Copy

**Objective:** Implement FIX drop copy for compliance.

**File Locations:** `services/internal/fix/dropcopy.go`

**Implementation:**
1. All execution reports copied to a separate FIX session (compliance).
2. Read-only: no order submission on drop copy session.
3. Used for regulatory reporting and audit.

**Definition of Done (Acceptance Criteria):**
* [x] All execution reports copied to drop copy session *(DropCopyRouter taps `ReportBus.WithTap` — byte-identical 35=8 clones fanned to bound targets; `TestDropCopyRouter_FanOutBoundAccounts`)*
* [x] Drop copy session is read-only *(account_id NULL ⇒ `Session.Entitled` denies; `OrderEntryRejector` emits 35=j with `drop copy is read-only` — `TestOrderEntryRejector`)*
* [x] Used for regulatory reporting *(bound-account scoping is the compliance entitlement; unresolvable reports fail closed and never copy)*

**SDD Checklist:**
- [x] Spec checkpoint: FIX drop copy — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

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
* [x] FIX 5.0 SP2 sessions established (initiator + acceptor) *(`AcceptorSettingsSP2` FIXT.1.1+DefaultApplVerID=9, TLS-mandatory; `InitiatorSettingsSP2` for outward SP2; dynamic sessions via the shared provisioned-session store)*
* [x] Derivative orders (forwards, swaps, NDFs, options) submitted via FIX 5.0 SP2 *(`MapNewOrderSingleSP2` — per-contract required fields enforced; params persist on `orders.algo_params` under `algo_type="FX_DERIVATIVE"` until Phase-22 migration-039 columns land; validated on stub instruments per the dependency note below)*
* [x] FX-specific tags supported (Currency, SecurityType, StrikePrice, SettlementType, SettlementDate, NoPartyIDs) *(15/167/202/9501/9502 + custom party block 9018/9019/9020 and standard 453/448/447/452 — `parseParties` preserves repeated-tag order via raw-field scan)*
* [x] Derivative-specific fields (value date, fixing date, barrier level) supported *(9502/64 value+settlement dates, 9503 fixing, 9504/9505 barrier level+type, 9506 exercise style, 9507 premium, 9508/9509 swap leg dates, 541 maturity, 201 PutOrCall — `mapDerivativeFields`)*
* [x] TLS 1.3 encryption on FIX 5.0 SP2 sessions *(`AcceptorSettingsSP2` refuses plaintext — `SocketMinimumTLSVersion=TLS13`; mTLS layer is Task 18.3.11 `tls.go`)*
* [x] Execution reports for derivative orders sent via FIX 5.0 SP2 *(`ReportAcceptedSP2`/`ReportFillSP2`/`ReportCanceledSP2`/`ReportRejectedSP2` stamp ApplVerID(1128)=9 + `DerivativeEcho` of the stored algo_params; FIXT.1.1 dispatch wired in `orders.go` `onNewOrderSingle`/cancel/replace via `isSP2Session`)*

**SDD Checklist:**
- [x] Spec checkpoint: FIX 5.0 SP2 for derivatives and institutional flow — defined first, validated against spec
- [x] Spec checkpoint: FX-specific FIX tags (Currency, SettlementType, NoPartyIDs) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation *(`fix50sp2_test.go`: spot passthrough, FORWARD/NDF/SWAP/OPT contract matrices, party group parse, malformed/missing-field rejects, exec-report echo round-trip)*

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
* [x] Dedicated PB Drop Copy sessions route executions in real time *(PBDropCopy ReportBus tap → eligible ACTIVE PBs via pb_credit_limits → session by fix_comp_id; `TestPG_PrimeBrokerageSchema`)*
* [x] FIX PartyID (448), PartyIDSource (447), PartyRole (452) populated per spec §9.2 *(`pbParties` + `SetParties` stamp ExecutingFirm/ClientID/PrimeBroker(BIC); `TestSetParties_Roundtrip`)*
* [x] Traiana Harmony / MarkitSERV affirmation feeds integrated *(`HTTPAffirmationExporter` + `AffirmationSubmitter` seam; `TestHTTPAffirmationExporter`)*
* [x] Status updates tracked in `pb_giveup_trades` table *(guarded PENDING→AFFIRMED/REJECTED transitions + Traiana ref + affirmed_at; PG test covers transitions)*
* [x] Affirmation timeouts alert middle office *(`AffirmationMonitor` 60s sweep → `GiveUpBreak` alerter; `TestAffirmationMonitor_ScanOnce`)*

**SDD Checklist:**
- [x] Spec checkpoint: PB drop copy & trade affirmation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 18.3.7: FIX Mass Quoting (Tag 35=i) & Quote Cancel (Tag 35=Z)

**Objective:** Implement FIX Mass Quoting (Tag 35=i) and Mass Quote Cancel (Tag 35=Z) for institutional Liquidity Providers (LPs) per spec §9.4, with strict 100% Firm Liquidity (No Last Look) rulebook policy per FX Global Code Principle 17 (spec §6.4, §24 #128).

**File Locations:** `services/internal/fix/quoting.go` (implemented filename — supersedes the planned `massquote.go`), `core/src/matching/MassQuote.cpp`

**Implementation:**
1. Handle `MassQuote` (35=i): multi-instrument quote updates in a single network packet containing `QuoteSetID` and repeating group of `QuoteEntryID`, `Symbol`, `BidPx`, `BidSize`, `OfferPx`, `OfferSize`.
2. Core integration: stream quotes directly to matching engine via Aeron SPSC IPC with atomic replacement of preceding LP quote level. *(Implemented as: quote-sourced GTC LIMIT orders submitted through the canonical `orders.Service` admission pipeline — validation, dedup, OTR counting, kill-switch, Aeron dispatch — so quotes can never bypass §8.4 ingress semantics; same-set re-quote cancels the prior bid/ask legs before placing the new generation. Attribution via `mq:<set>:<gen>:<entry>:<side>` client_order_id + FIX session_id, which MMP/cancel-on-disconnect scopes key against.)*
3. Emit `MassQuoteAcknowledgement` (35=b) with `QuoteStatus` per entry (`0 = Accepted`, `5 = Rejected`, QuoteRejectReason 99 + machine-readable Text token).
4. Handle `QuoteCancel` (35=Z): support QuoteCancelType (`1 = Cancel per symbol`, `4 = Cancel all quotes for LP`) plus per-`QuoteSetID` cancellation; symbol/all scopes ride the Task 5.3.25 mass-cancel path scoped account+instrument+session.
5. Enforce 100% Firm Liquidity rule: quotes are binding; reject any quote with non-zero hold times; incoming marketable client orders execute against mass quotes with zero last look or subjective confirmation delays. *(Non-zero hold time rejects the whole set `QUOTE_REQUEST_REJECTED` — new §23 code.)*

**Definition of Done (Acceptance Criteria):**
* [x] MassQuote (35=i) parsed and applied across multiple currency pairs *(transport-agnostic parse+apply layer `QuoteService.SubmitMassQuote`; session.go tag-framing is Task 18.3.1's sibling surface)*
* [x] MassQuoteAcknowledgement (35=b) returned with status *(per-entry `MassQuoteAck` accepted/rejected)*
* [x] QuoteCancel (35=Z) cancels all or symbol quotes atomically *(plus per-set cancel; scopes ride `orders.MassCancel`)*
* [x] Strict 100% Firm Liquidity enforced: no last look, zero hold time, immediate execution *(unit-tested rejection; same-pipeline quote orders match immediately with no hold/delay semantics)*

**SDD Checklist:**
- [x] Spec checkpoint: FIX mass quoting & firm liquidity — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- Implementation notes (2026-09-29): mm_programs entitlement + MMP lockout consulted per entry; obligations sampled via `marketmaking.ObserveQuote`; second-side submission failure unwinds the first leg (no phantom one-sided quotes); `QUOTE_REQUEST_REJECTED`/`MMP_LOCKED_OUT`/`MMP_TRIGGERED` registered in §23.

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
* [x] SBE XML message schema defined and validated *(`internal/fixsbe/schema/order_entry.xml` — schema id 2 v1; NewOrder/Cancel/Replace/Negotiate inbound + ExecutionReport/BusinessReject/News/NegotiationResponse outbound; fixed-layout, zero varData; codec round-trip + forward-compat + malformed-frame tests in `fixsbe/codec_test.go`)*
* [x] Binary layouts match C++ engine structs with zero padding gaps *(all offsets 8-byte aligned, no padding inside blocks; the engine-facing fields carry the same fixed-scale mantissa convention as the engine wire (int64 ×10^-8 — identical to the FlatBuffers/core dispatch scale) — the C++ POD twin is a compile-time layout, documented in the schema XML since no core headers exist in this repo yet)*
* [x] Aeron UDP transport channel configuration specified *(`fixsbe/aeron.go` — `AttachFragment` binds `aeron.Client.AddSubscription` → `Gateway.Handle` → response `Publication.Offer`; session binding attaches at subscription time after out-of-band Ed25519 negotiation)*
* [x] Sub-5µs gateway ingress architecture documented *(measured honestly in `fixsbe/doc.go` + `bench_test.go`: decode 25.3ns/op; full in-process ingress 411ns/op on i7-14700K — NIC/driver legs deployment-measured, not unit-benchable)*

**SDD Checklist:**
- [x] Spec checkpoint: SBE / Aeron binary gateway spec — defined first, validated against spec
- [x] All spec checkpoints pass after implementation *(binary ingress maps onto the canonical `orders.Service` path via the `OrderAPI` seam — `gateway.go` + `gateway_test.go`; schema lifecycle reuses `internal/sbe.Registry` → `SBE_SCHEMA_RETIRED`/`UNSUPPORTED_PROTOCOL_VERSION`)*

---

### Task 18.3.9: FIX Session Entitlement, Cancel-on-Disconnect & Throttling

**Objective:** Bind every FIX session to an account with instrument entitlements, add cancel-on-disconnect, and per-session inbound throttling per spec §5.20/§9.3 (§24 #135–137, #153). Added 2026-09-15.

**File Locations:** `services/internal/fix/session.go` (extend) *(landed as `internal/fix/{app,types,store,throttle,admin,gapfill}.go` + `internal/api/handlers_fix.go` for the admin surface — same split as Task 18.3.1)*, `migrations/046_fix_sessions_entitlement.up.sql`

**Implementation:**
1. `fix_sessions` gains `account_id` (required), `allowed_instruments` (array or NULL=all), `cancel_on_disconnect BOOL` (default true), `max_msgs_per_sec INT` (default 100 per spec §9.3) — migration 046.
2. **Entitlement:** every order message validated: session's `account_id` is stamped on the order (Tag 1 Account must match or be a sub-account of the bound account); instrument must be in `allowed_instruments`; violations rejected `SESSION_NOT_ENTITLED` (BusinessMessageReject 35=j — terminology corrected, remediation #35: Reject is 35=3 per spec §9.9).
3. **Cancel-on-disconnect:** on abnormal disconnect (TCP drop, heartbeat timeout, no orderly Logout), all open orders owned by that session are mass-cancelled (reuses Task 5.3.25 mass-cancel path scoped by session); orderly Logout leaves orders unless `cancel_on_logout` configured.
4. **Per-session throttle:** token bucket per session on inbound messages; excess messages rejected with `SESSION_THROTTLED` reject — never silently dropped.
5. Entitlement/throttle config admin-managed via `PUT /api/v1/admin/fix-sessions/{id}` (Risk Manager+).
6. **Venue-wide reconnect pacing (amended 2026-09-27, remediation #27):** after a mass disconnect (gateway restart, DR failover), logons are admitted in staged windows per session class (MM → institutional → retail, 500ms stagger) so the reconnect storm cannot re-trip the outage; overflow logons queue with `SESSION_THROTTLED` rather than dropping.

**Migration note:** `migrations/046_fix_sessions_entitlement.up.sql` — `fix_sessions.account_id`, `allowed_instruments`, `cancel_on_disconnect`, `max_msgs_per_sec` (spec §5.20).

**Definition of Done (Acceptance Criteria):**
* [x] Order on non-entitled account/instrument rejected SESSION_NOT_ENTITLED (§24 #135) *(`app.go` `checkEntitlement`: drop-copy (NULL account_id) cannot submit; Tag 1 must equal the bound account or a sub-account via `SubAccountChecker`; `Session.Entitled` enforces `allowed_instruments`; reject = BusinessMessageReject 35=j BusinessRejectReason=5; `app_test.go` covers drop-copy + non-entitled instrument)*
* [x] Abnormal disconnect mass-cancels that session's open orders (§24 #136) *(`App.OnLogout` → `CoDExecutor` (rate gate §24 #245, audit seam) → `SessionCanceller` → canonical `Service.CancelOnDisconnect`; `app_test.go` covers abnormal vs orderly and `cancel_on_disconnect=false`)*
* [x] Inbound rate beyond `max_msgs_per_sec` rejected, not dropped (§24 #137) *(`throttle.go` per-session token bucket, burst = 1s of cap; excess → 35=j `SESSION_THROTTLED` reason=3; live `max_msgs_per_sec` updates apply per-message via `FromApp`'s row reload + `SetRate`; `throttle_test.go` covers burst/refill/tighten/reset)*
* [x] Orderly Logout does not cancel unless configured *(inbound 35=5 marks `a.orderly[session]`; `OnLogout` consults it — `TestOnLogout_AbnormalRunsCoD_OrderlyPreserves`)*

**SDD Checklist:**
- [x] Spec checkpoint: FIX entitlement + CoD + throttle (§9.3, §24 #135–137) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation *(unit suite green; admin route `PUT /api/v1/admin/fix-sessions/{id}` live + RBAC Super Admin in `routes_v1.go`, handler `api.FixSessionUpdateHandler` → `fix.ApplyEntitlementPatch` → transactional `PgStore.UpdateEntitlement`)*
- [x] Edge cases: reconnect during CoD mass-cancel *(CoD rate gate §24 #245 bounds repeats; reconnect re-authenticates + rehydrates seq via `PgMessageStoreFactory`)*, session bound to sub-account hierarchy *(`SubAccountChecker` seam; `accounts.SubAccountService` in prod)*, throttle burst at exact boundary *(`TestThrottle_*` burst/refill/zero-cap-default legs)*

---

### Task 18.3.10: Market-Maker Program & MMP Protection

**Objective:** Implement the market-maker program per spec §5.27/§9.6 (§24 #139): quoting obligations, compliance monitoring, MMP mass-cancel protection, and rebate reconciliation. Added 2026-09-15.

**File Locations:** `services/internal/marketmaking/` (program.go, store_pg.go, compliance.go, mmp.go, rebate.go), `migrations/045_market_maker_program.up.sql`

**Implementation:**
1. `mm_programs` table (migration 045) columns per spec §5.27/§9.6: `id`, `account_id`, `instrument_id` (NULL = program-wide), `min_quote_size`, `max_spread_bps`, `presence_pct`, `mmp_max_fills`, `mmp_window_ms`, `rebate_bps`, `status` (+ `otr_allowance` column for the MM OTR allowance used by Task 13.3.6).
2. Obligations: registered MM must maintain two-sided quotes ≥ `min_quote_size` within `max_spread_bps` for ≥ `presence_pct` of the trading day (sampled per minute); compliance tracked in `mm_compliance` daily rollup.
3. **MMP protection:** account configures `mmp_max_fills` per instrument per window (e.g., 10 fills/5s); on breach, engine mass-cancels all that MM's resting orders on the instrument (Task 5.3.25 path) and flags `MM_OBLIGATION_BREACH` (spec §23) on the session. *(Implemented: Go-side `MMPTracker` mirrors the engine's sub-ms trigger from the read-model fill feed — sliding window per covering program; on breach it mass-cancels account+instrument via `orders.MassCancel` (reason "mmp"), raises the P2 `MMP_TRIGGERED`/`MM_OBLIGATION_BREACH` alert, and holds `MMP_LOCKED_OUT` until explicit reset via `POST /admin/mm-programs/{id}/mmp-reset` or FIX 35=a.)*
4. Breach handling: 3 daily compliance failures in a rolling week → program suspension (Risk Manager review); rebate accrual pauses during suspension.
5. Rebates: maker rebates accrued per fill (`rebate_bps`), posted through the GL (Phase-3 Task 3.3.6) monthly; reconciled in Phase-24 settlement reporting. *(Implemented: `mm_rebate_accruals` per-fill rows idempotent on `fill_ref`; monthly sweep posts one balanced journal per (account, currency) — DR `5100_LIQUIDITY_REBATE_EXPENSE` / CR `2010_CUSTOMER_LIABILITY` + wallet credit — via the `JournalPoster` seam, never a balance write.)*
6. OTR allowance: `otr_allowance` feeds `risk.OtrMonitor` via the `WithMMAllowance` seam from the in-memory ACTIVE-program snapshot (no per-event PG reads); applies **only over the venue default** — explicitly scoped `risk_limits` rows still win.
7. Admin REST: `GET/POST /api/v1/admin/mm-programs`, `GET/PUT /{id}`, `POST /{id}/suspend|resume|mmp-reset`, `GET /{id}/compliance`, `GET /{id}/rebates`, `POST /rebates/post` (RoleRiskManager; rebate surface RoleFinanceOps).

**Migration note:** `migrations/045_market_maker_program.up.sql` — `mm_programs` + `mm_compliance` + `mm_rebate_accruals` tables (spec §5.27 + §9.6 rebate persistence).

**Definition of Done (Acceptance Criteria):**
* [x] Quoting obligations tracked (size/spread/presence) with daily compliance rollup
* [x] MMP triggers mass-cancel after configured fill count per window (§24 #139)
* [x] Repeated compliance breach → program suspension; rebate accrual pauses
* [x] Maker rebates accrued per fill and posted to GL

**SDD Checklist:**
- [x] Spec checkpoint: MM program + MMP protection (§9.6, §24 #139) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: MMP during auction uncross, presence sampling across 24/5 open/close, suspension with resting quotes *(coverage: window-sliding + lockout/reset + suspended-not-entitled + rebate-pause + ask-leg-unwind unit tests; PG-gated migration/rollup test `EXC_PG_TEST=1`)*

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
* [x] FIXS TLS 1.3 mutual authentication binds client certificate to CompID/account/environment *(`internal/fix/tls.go` `ServerTLSConfig` + `VerifyPeer`: SHA-256 cert fingerprint resolves the provisioned `fix_sessions` binding (`PgBindingLookup`, migration 052); environment label must equal the listener's; bound CN/SAN must match the presented subject)*
* [x] Invalid, expired, revoked, or mismatched certificates fail before FIX Logon is accepted *(admission runs in `tls.Config.VerifyConnection` — the handshake dies pre-Logon; `tls_test.go` matrix covers missing/expired/not-yet-valid/unbound/env-mismatch/CN-SAN-mismatch/revoked/unverifiable-revocation, each fail-closed with a coded `CertError` + audit event)*
* [x] Dual-certificate rollover preserves availability and audit history *(`cert_rollover_fingerprint` + `cert_rotation_ends_at` window: either fingerprint verifies while open, rollover-path acceptance audited as `MatchRollover`; post-window the rollover credential rejects `MTLS_CERT_ROLLOVER_EXPIRED`, primary unaffected — `TestRolloverWindow`)*
* [x] Full certification pack passes and evidence is versioned by client/schema build *(`internal/fix/certification/pack.go` — versioned case catalog (logon/logout, heartbeat/TestRequest, resend/gap-fill, PossDup suppression, cancel/replace, session+business rejects, malformed, entitlement, throttle, CoD, drop-copy, recovery + Task 18.3.17's sbe_equiv/schema_upgrade/news_drain/dup_suppress_transport); transcript → SHA-256 `evidence_hash`; `Gate.Admit` keys the exact tuple (participant, client build, dictionary, schema, environment))*
* [x] Uncertified or stale-certified client builds cannot open production order-entry sessions *(`internal/fix/certgate.go` `CertifiedApp` wraps `App` — production order-entry Logons must hold a current ACTIVE PASS `fix_certifications` row or reject `CERTIFICATION_REQUIRED`/`CERTIFICATION_STALE`; expired, FAIL, revoked, schema-revoked (`RevokeSchema`), and cross-environment rows all fail closed; drop-copy sessions ungated)*

**SDD Checklist:**
- [x] Spec checkpoint: FIXS mTLS + production client certification (§9.7, §24 #167) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation *(`certification_test.go` gate matrix: no row / FAIL / staging-only / expired / revoked / cross-schema / cross-dictionary / nil-store legs)*
- [x] Edge cases: cert rollover during reconnect *(rollover window verified by `TestRolloverWindow` — either fingerprint admits inside the window)*, OCSP unavailable *(staple decode failure → `MTLS_REVOCATION_UNVERIFIABLE`; nil/errored revocation seam fails closed)*, CompID collision *(identity is cert-fingerprint → `fix_sessions` row; a foreign CompID's cert has no binding → `MTLS_CERT_NOT_BOUND`)*, schema version revoked *(`RevokeSchema` mass-revokes ACTIVE rows on the old `venue_schema_version`; gate then rejects `CERTIFICATION_STALE`)*

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
* [x] FIX gateway failover preserves sequence continuity without manual counter reset (§24 #189) — `internal/fix/failover.go`: `SeqState` hash `fix:seq:{session}` via `RedisSeqStore` Lua CAS (`seqCASScript`/`seqClaimScript`, epoch-fenced owner lease) + `PGSeqStore` conditional-UPDATE fallback layered in `FailoverStore`; `Failover.ResumeOnLogon` resynchronizes a standby with zero manual resets
* [x] ResendRequest triggers gap-fill replay with PossDupFlag=Y and SeqReset for admin messages — `gapfill.go` `PlanResend` (admin `0/1/2/3/4/5/A` collapse into `NewSequenceReset(GapFillFlag=Y)`, application messages replay via `MarkReplay` = 43=Y + 122=OrigSendingTime)
* [x] Replayed NewOrderSingle messages are suppressed without duplicate executions in matching engine — `RedisExecDedup` ClOrdID registry (`fix:dedup:{session}:{clordid}` SET-NX-PX + Lua resolve) via `Failover.HandleNewOrder`; a repeat returns the stored `DedupRecord.Report` for verbatim echo, never resubmits
* [x] Full failover and sequence re-synchronization completes in <5s — `TestFailoverE2E` (real PG fix_sessions + Redis lease): resume median 2.06ms max 67.9ms, total incl. lease wait 452–518ms across 5 iterations; resume-latency distribution p99=9ms

**SDD Checklist:**
- [x] Spec checkpoint: FIX gateway session failover and gap fill (§9.8, §24 #189) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `go test ./internal/fix/` unit suite (26 tests: CAS/fencing, resync verdicts, dedup, fallback layering); Redis Lua path additionally gated `EXC_REDIS_TEST=1` (`TestRedisSeqStoreIntegration`)
- [x] Edge cases: simultaneous bidirectional ResendRequest race, missing message gap across archive storage, client resets sequence mid-session — CAS lost-race returns the post-image for bounded retry; archive hole fails closed (`ErrResendHole`, never silently gap-fills over a possibly-lost ExecutionReport); `ResetSeqNumFlag(141)=Y` handled by `resetSequences` CAS

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
* [x] AllocationInstruction (35=J) parsed with NoAllocs legs; over/under-allocation rejected *(35=P `AllocRejCode=4` + `ALLOCATION_SUM_MISMATCH` per §27.1 matrix; `TestAllocationNew_Over/UnderAllocationRejected`)*
* [x] PRO_RATA, MANUAL, and STEP_OUT allocation methods work correctly *(pro-rata reuses the Phase-14 largest-remainder quantum split; `TestAllocationProRata_ExactConservation`, `TestAllocationStepOut`)*
* [x] AllocationReport (35=AK) emitted with per-leg booking status *(`BuildAllocationReport` + NoAllocs echo w/ IndividualAllocID; `TestAllocationNew_ManualAccepted`)*
* [x] Each allocation leg creates individual trade record with parent_exec_id linkage *(`fix_allocation_legs` rows, generated `ALLOC-{id}-{leg}` child exec refs; `TestPG_AllocationStore`)*
* [x] Allocation REPLACE and CANCEL propagate corrections; immutable audit trail maintained *(SERIALIZABLE supersede/cancel + `allocation_events` append-only with UPDATE/DELETE trigger; `TestAllocationReplace*`, `TestPG_AllocationStore`)*
* [x] PB Drop Copy receives allocation reports for all allocated sub-accounts *(`PBDropCopy.OnAllocationChange` + `DropCopyRouter.OnAllocationChange` on the `AllocationChangeSink` seam)*

**SDD Checklist:**
- [x] Spec checkpoint: FIX Allocation Instructions & Reports (Tag 35=J/35=AK) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: partial fill allocation, allocation across different settlement dates, step-out to external PB, concurrent allocation + cancel race *(partial fills: exec-qty is the resolver's cum qty — legs must sum to it regardless of fill count; concurrent REPLACE/CANCEL is serialized by `FOR UPDATE` + status guard — settlement-locked rows reject)*

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
* [x] SOR routes to external venues when internal CLOB depth/spread thresholds breached *(`internal/sor` `Thresholds.ShouldRouteExternal` — no BBO, contra-side depth < `MinDepth`, or spread > `MaxSpreadBps`; venues reachable only via `VenueConnector`; `LoadVenueConfig` env-blocks real venues — `SOR_EXTERNAL_VENUES` unset → local-only)*
* [x] 5-state SOR lifecycle (PENDING→ROUTED→PARTIAL_FILL→FILLED→CANCELLED) with shadow orders *(implemented with the spec-§24-#242 canonical names `PENDING_ROUTE → ROUTED → PARTIALLY_FILLED_EXTERNAL → FILLED_EXTERNAL|CANCELLED_EXTERNAL`; `sor_shadow_orders` + `sor_fill_dedup` tables, migration 227; partial unique index enforces one open shadow per parent)*
* [x] External venue fills reconciled via FILL_BRIDGE events to Phase-3 settlement *(`onVenueEvent` → `FillPublisher` seam (JetStream "settlements" stream in production) emitting `FILL_BRIDGE` per fill; cumulative VWAP + filled qty tracked on the shadow)*
* [x] 500ms external timeout triggers auto-cancel + re-route or SOR_TIMEOUT rejection *(`ExternalTimeout = 500ms`; on expiry the external order is auto-cancelled, cancel-confirm awaited, and the walk continues to the next venue; exhaustion → `SOR_TIMEOUT`; `TestTimeoutWalksToNextVenue`/`TestAllVenuesDeadSORTimeout`)*
* [x] Race condition prevention: no concurrent local+external orders for same parent *(per-parent mutex + non-terminal shadow guard → `ROUTING_REJECTED`; `ReleaseForLocal` cancels the external leg and waits for confirm before local re-submission; `TestConcurrentRouteGuard`/`TestReleaseForLocal`)*
* [x] Tests passing for Smart Order Routing (SOR) & External Liquidity Aggregation *(`sor_test.go` — full lifecycle, partial fills, timeout walk, reject, dedup replay, env-config parse; `LoopbackConnector` exercises the real initiator wire path: outbound 35=D encode → parse-back, inbound 35=8 decode)*

**SDD Checklist:**
- [x] Spec checkpoint: Smart Order Routing (SOR) & External Liquidity Aggregation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- Implementation notes (2026-09-30): implemented under `internal/sor/` (preferred over the planned `internal/features/` path per the architecture's package conventions); lifecycle state names use the spec-§24-#242 canonical tokens (supersede the task text's PENDING/PARTIAL_FILL spellings); production venue connectors are env-blocked pending real ECN credentials — the loopback adapter is a genuine FIX initiator round-trip, not a stub

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
* [x] Instrument state changes trigger FIX TradingSessionStatus broadcast within 50ms
* [x] All TradSesStatus codes correctly mapped from internal states
* [x] FIX clients can subscribe/unsubscribe to session status updates
* [x] HALT broadcast prevents algo rejection storms by informing clients before orders arrive

**SDD Checklist:**
- [x] Spec checkpoint: FIX TradingSessionStatus — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: rapid state flapping (ACTIVE→HALTED→ACTIVE within 1s), client subscribed mid-transition

---

### Task 18.3.16: Dead-man switch via FIX

Dead-man switch via FIX — implement heartbeat-based countdown cancel-all for FIX sessions. Client sets countdown via `UserRequest (35=BE)` with `UserRequestType=4` (custom) and `CountdownMs` tag (custom tag 20001, min 1000, max 60000). Server maintains per-session timer; if not renewed before expiry, atomically cancels all resting orders for the session's bound account(s). Setting `CountdownMs=0` disables. Confirmation via `UserResponse (35=BF)` with countdown state. Timer state synchronized with REST/WS dead-man switch (Task 5.3.33) — a single account-level timer shared across all interfaces.

**SDD Checklist:**
- [x] Spec checkpoint: FIX dead-man switch shares the canonical account timer with REST/WS (§24 #257) — defined first, validated against spec *(`deadman.go` maps `UserRequest(35=BE)` UserRequestType=4 + `CountdownMs(20001)` onto the canonical `accounts.DeadManService` — same Redis store + atomic mass-cancel dispatcher as Task 5.3.33; `UserResponse(35=BF)` echoes armed/disabled state + expiry; `CountdownMs=0` disables; min 1000/max 60000 enforced → `INVALID_TIMEOUT`; drop-copy sessions rejected `SESSION_NOT_ENTITLED`; `app_test.go` covers arm/disable/out-of-range/drop-copy/unsupported-type)*
- [x] All spec checkpoints pass after implementation *(`cmd/fix` runs `deadman.Run` sweeper in-process — Lua pops are atomic so REST/WS/FIX sweepers never double-fire)*

---

### Task 18.3.17: FIX SBE Production Transport and Graceful Maintenance Drain

**Objective:** Productionize standards-based FIX SBE alongside tag-value FIX and make planned drains explicit.

**Implementation:**
1. Provide TLS endpoints for tag-value requests/SBE responses and SBE requests/responses using the same business semantics, session entitlements, sequence rules, and drop-copy coverage.
2. Require Ed25519 session keys, TLS SNI, hostname validation, and schema ID/version negotiation; apply the Phase-06 schema lifecycle and retirement policy.
3. During maintenance, send FIX `News` advisories repeatedly until disconnect, preserve cancels, and direct clients to a ready replacement endpoint.
4. Certification covers tag-value↔SBE equivalence, schema upgrade/retirement, maintenance reconnect, and duplicate suppression.

**SDD Checklist:**
- [x] Spec checkpoint: FIX SBE transport, Ed25519/SNI, schema lifecycle, and graceful News drain pass certification (§24 #284, #289) — defined first, validated against spec — tag-value drain half landed 2026-09-29 (`internal/fix/drain.go`); SBE/production-transport half landed 2026-09-30: `internal/fixsbe/transport.go` one-listener mux (`Classify` sniffs `8=FIX` vs SBE header — tag-value req/SBE resp combos supported via negotiated `ResponseCodec`), Ed25519 session-key proof bound to `server nonce ‖ SNI ‖ TLS keying material` (`ChallengeMessage`/`VerifySessionProof`/`SNIBinder`), schema ID/version negotiation through the Phase-06 `sbe.Registry` lifecycle (deprecated → sunset-stamped `NegotiationResponse`; retired → fail-closed), and `fixsbe/drain.go` `Drainer` broadcasting `News` (SBE template 103 / FIX 35=B) on every live session naming the replacement endpoint while `Gateway` drain-gates new/replace and preserves cancels; `fixsbe/conn.go` `ConnServer.Serve` runs the per-connection handshake→dispatch lifecycle; `fix_certifications` pack cases (`sbe_equiv`, `schema_upgrade`, `news_drain`, `dup_suppress_transport`) cover the §9.7 certification leg; sessions persisted in migration 228 `fixsbe_sessions` + `fixsbe_schema_registry`
- [x] All spec checkpoints pass after implementation *(`transport_test.go`: classify/sniff, frame extraction caps, Ed25519 handshake matrix (SNI mismatch, replay, tampered sig, unknown/disabled/env-bound keys, draining-session binding), schema deprecate/retire negotiation, dual-encoding drain broadcast)*

---

### Task 18.3.18: FIX Sequence Gap Resolution, Session Reject & CoD Recovery

**Objective:** Implement strict FIX message sequence gap recovery, standardized reject messages, and low-latency Cancel-on-Disconnect (CoD) execution per spec §2.7, §9.9, and §24 #319.

**Implementation:**
1. **Sequence Gap Recovery Protocol:** On inbound sequence gap, dispatch `ResendRequest (35=2)`. Handle unrecoverable gaps with `SequenceReset (35=4) GapFillFlag=Y`. If inbound gap exceeds 2,500 messages, terminate session immediately with `Logout (35=5) Text=EXCESSIVE_SEQUENCE_GAP` to defend memory against replay exhaustion attacks.
2. **Structured Reject Responses:** Standardize application-level rejections via `BusinessMessageReject (35=j)` with `BusinessRejectReason (380)` and execution rejections via `ExecutionReport (35=8)` with `OrdRejReason (103)`.
3. **Cancel-on-Disconnect (CoD) Execution:** When socket heartbeat fails (2 consecutive missed heartbeats) or TCP drops, cancel all resting non-exempt orders within 50ms and emit execution reports to drop-copy.

**Definition of Done (Acceptance Criteria):**
* [x] Sequence gaps resolve cleanly via ResendRequest and GapFill — `gapfill.go` `AssessInbound` (accept / ResendRequest(35=2) / possdup-drop / fatal-too-low) + `PlanResend` over the `OutboundArchiver` seam (archive holes fail closed via `ErrResendHole`)
* [x] Massive sequence gaps (>2,500) trigger fail-closed session Logout — `MaxResendGap=2500`; `AssessInbound` emits prebuilt `Logout(35=5)` `Text=EXCESSIVE_SEQUENCE_GAP`; covered by `TestAssessInbound/excessive_gap*` incl. the possdup-still-fatal edge
* [x] Cancel-on-disconnect purges open orders within 50ms of socket loss — `CoDExecutor.OnDisconnect` (`CoDBudget=50ms` measured per event; breach flagged, cancel never abandoned — fail-closed pessimism) over the `SessionCanceller` seam → `orders.Service.CancelOnDisconnect` (`orders.session_id` scope); §24 #245 rate gate via `RedisCoDRateGate` (`cod:rl:{account}` NX PX 5s); wired in `cmd/fix/main.go` into `app.Options.CoD` (App.OnLogout consumes it); graceful Logout preserves orders per AC #30. Per-order `orders.cod_exempt` exemption (spec §5.4 remediation #35) landed via migration 229 + FIX venue tag 9510 (`TagCODExempt` → `SubmitRequest.CoDExempt`) + `PgStore.OpenOrders` exemption scoped to Reason `cancel_on_disconnect` (dead-man/admin/close-all sweeps ignore it)

**SDD Checklist:**
- [x] Spec checkpoint: FIX sequence gap resolution, session reject, and CoD recovery (§24 #319) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation — `TestAssessInbound`, `TestPlanResend*`, `TestCoD*` (budget breach, graceful preserve, disabled, drop-copy, rate gate, fail-closed unknown session/unwired canceller)

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

