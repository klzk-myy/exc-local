# Phase 6 — Market Data Distribution

**Duration:** 13–17 days (supersedes 10–13 — duration itemization completed 2026-09-27, remediation #35: Tasks 6.3.7–6.3.20 added to §6.6)
**Dependencies:** Phase 2, Phase 2.5, Phase 5
**Spec Reference:** §10 (Market Data Distribution)

---

## 6.1 Objectives

Implement the Go market data service: WebSocket fan-out (gorilla/websocket), L2 book distribution with 100ms conflation, trades stream, ticker updates, reconnect with `last_seq` replay, the conflation engine, and an institutional SBE A/B multicast feed with deterministic replay/snapshot recovery.

---

## 6.2 Prerequisites

- Phase 2 complete (C++ core publishes book updates via Aeron)
- Phase 2.5 complete (engine soak passed)
- Phase 5 complete (gateway API)

---

## 6.3 Tasks

### Task 6.3.1: WebSocket Server

**Objective:** Implement the WebSocket server with gorilla/websocket.

**File Locations:** `services/cmd/marketdata/main.go`, `services/internal/marketdata/ws.go`

**Implementation:**
1. WS endpoint: `/ws/v1/marketdata` (public) and `/ws/v1/orders` (private, auth required).
2. Goroutine-per-connection model.
3. Subscription: client sends `{"action": "subscribe", "channels": ["book@EUR/USD", "bbo@GBP/USD"]}` — unified on the `action` discriminator per spec §10.5 (remediation #9; remediation #35 supersedes the prior bare `{"subscribe": [...]}` frame, which predated the unification) with the typed-channel namespace (`book@`, `bbo@`, `aggTrades@`, `depth@`, `kline@`, `openInterest@`, `referencePrice@`, `liquidations@`).
4. Heartbeat: server sends ping every 30s; client must respond pong within 60s (aligned with Task 6.3.7/AC #20, remediation #35 — supersedes the prior 10s window).
5. Max connections: 20 L2 + 5 L3 per session.

**Definition of Done (Acceptance Criteria):**
* [ ] WS server accepts connections and subscriptions
* [ ] Heartbeat ping/pong works; stale connections dropped
* [ ] Subscription limits enforced (20 L2, 5 L3)

**SDD Checklist:**
- [ ] Spec checkpoint: goroutine-per-connection WS — defined first, validated against spec
- [ ] Spec checkpoint: 20 L2 / 5 L3 subscription limits — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.2: L2 Book Distribution with Conflation

**Objective:** Distribute L2 book updates with 100ms conflation.

**File Locations:** `services/internal/marketdata/l2.go`

**Implementation:**
1. C++ core publishes book deltas via Aeron; Go service consumes.
2. Conflation: batch updates within 100ms window or 100 events, whichever first.
3. Per-symbol sequence counter (`md:seq:{symbol}` in Redis).
4. Send: top 20 levels per side with seq.
5. On reconnect: client sends `last_seq`; server replays from `last_seq+1` or sends full snapshot if gap > 1000.

**Definition of Done (Acceptance Criteria):**
* [ ] L2 book updates distributed with 100ms conflation
* [ ] Per-symbol sequence counter maintained
* [ ] Reconnect with last_seq: replays missed or sends snapshot
* [ ] No stale data, no gaps under load
* [ ] L2 depth checksums: CRC32 in depth updates; resync on mismatch (per spec §24 #83)
* [ ] Market data SLA: p99 WS push ≤ 100ms; uptime 99.95% (per spec §24 #99)

**SDD Checklist:**
- [ ] Spec checkpoint: 100ms conflation or 100 events — defined first, validated against spec
- [ ] Spec checkpoint: last_seq reconnect replay — defined first, validated against spec
- [ ] Spec checkpoint: L2 depth CRC32 checksums with resync on mismatch — defined first, validated against spec
- [ ] Spec checkpoint: market data SLA p99 WS push ≤ 100ms + uptime 99.95% — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.3: Trades Stream

**Objective:** Distribute real-time trades (no conflation).

**File Locations:** `services/internal/marketdata/trades.go`

**Implementation:**
1. C++ core publishes trades via Aeron; Go service consumes.
2. No conflation — trades sent immediately.
3. Fields: trade_id, price, quantity, side, timestamp, seq.

**Definition of Done (Acceptance Criteria):**
* [ ] Trades distributed in real-time (no conflation)
* [ ] All trade fields correct

**SDD Checklist:**
- [ ] Spec checkpoint: real-time trades no conflation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.4: Ticker Updates

**Objective:** Distribute 24h rolling ticker.

**File Locations:** `services/internal/marketdata/ticker.go`

**Implementation:**
1. Rolling 24h OHLCV computed from trades.
2. Updates every 1s.
3. Fields: open, high, low, close, volume, change_pct, seq.

**Definition of Done (Acceptance Criteria):**
* [ ] Ticker updates every 1s
* [ ] 24h OHLCV computed correctly
* [ ] Change percentage correct

**SDD Checklist:**
- [ ] Spec checkpoint: 1s ticker with 24h OHLCV — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.5: Private Order Stream

**Objective:** Distribute private order updates to authenticated users.

**File Locations:** `services/internal/marketdata/private.go`

**Implementation:**
1. WS endpoint: `/ws/v1/orders` (JWT auth required).
2. Events: order_accepted, order_rejected, order_filled, order_cancelled, order_expired.
3. Filtered by account_id from JWT.

**Definition of Done (Acceptance Criteria):**
* [ ] Private order stream requires JWT auth
* [ ] Events filtered by account_id
* [ ] All order lifecycle events delivered

**SDD Checklist:**
- [ ] Spec checkpoint: private order stream with JWT auth — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.6: Institutional A/B Multicast Feed & Recovery

**Objective:** Provide production-grade low-latency market data with independently routed redundant feeds and deterministic gap recovery per spec §10.4/§24 #166. Added 2026-09-15.

**File Locations:** `services/internal/marketdata/multicast.go`, `services/internal/marketdata/replay.go`, `core/schema/marketdata_sbe.xml`

**Implementation:**
1. Encode L2, L3, trades, security definitions, and trading-status events using versioned SBE schemas; no WebSocket conflation applies to this feed.
2. Publish every incremental event with the same channel sequence to UDP multicast Feed A and Feed B over independent NICs/switch paths.
3. TCP replay accepts channel+sequence ranges with bounded retention; snapshot/recovery channels provide full state when a gap is outside replay range.
4. Reference client arbitrates A/B by sequence, suppresses duplicates, queues incrementals during snapshot, detects channel reset, and publishes only after exact convergence.
5. Instrumentation covers per-feed lag/loss, replay demand, snapshot age, divergence, and sequence-reset events; either feed may fail with no client-visible gap.

**Definition of Done (Acceptance Criteria):**
* [ ] A/B feeds carry byte-equivalent business events and identical channel sequences
* [ ] Feed A or B loss causes zero client-visible gap when the peer feed remains healthy
* [ ] TCP replay repairs bounded gaps; snapshot plus queued incrementals repairs out-of-range gaps
* [ ] Duplicate suppression and channel-reset recovery converge to the engine book exactly
* [ ] SBE schema compatibility and security-definition/status recovery tests pass

**SDD Checklist:**
- [ ] Spec checkpoint: institutional A/B multicast + replay/snapshot recovery (§10.4, §24 #166) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: asymmetric feed delay, both feeds gap at different sequences, replay retention exceeded, snapshot races live incrementals

---

### Task 6.3.7: WebSocket Message Rate Limiting & Abuse Protection
Added 2026-09-17 (gap analysis remediation #6).

Protect WS infrastructure from subscription abuse (spec §2.3.1 note, §24 #215):
1. Per-session message rate limit: 100 messages/sec for control messages (subscribe, unsubscribe, ping). Separate from order submission rate.
2. Enforcement: token bucket per session in Redis (`ws:rate:{session_id}`). Excess → `{"type": "error", "error": "WS_RATE_EXCEEDED", "retry_after_ms": N}` warning (standard error envelope + registered WS control code per §23 — remediation #9). 3 warnings in 60s → forced disconnect.
3. Subscribe/unsubscribe churn protection: max 10 subscription changes per second. Rapid churn (>10/s sustained for 5s) → session terminated with `WS_ABUSE_DETECTED`.
4. WebSocket heartbeat: server sends `ping` every 30s; client must respond with `pong` within 60s or connection is closed.
5. WS rate feedback: `{"type": "rate_info", "remaining": N, "reset_ms": T}` message sent on throttle.
6. **(amended 2026-09-20 — feature-completeness audit remediation #11):** WS close frames include a `disconnect_reason` discriminator: `ABUSE_DISCONNECT` (churn protection), `CLIENT_DISCONNECT` (normal close), `NETWORK_TIMEOUT` (heartbeat failure). Cancel-on-disconnect (Phase-05 Task 5.3.25) triggers ONLY for `CLIENT_DISCONNECT` and `NETWORK_TIMEOUT`, NOT for `ABUSE_DISCONNECT`. On abuse disconnect, orders remain resting but new orders/modifications are blocked until reconnection and re-authentication. Rate limiting on the cancel-on-disconnect machinery: max 1 mass-cancel per account per 5 seconds to prevent mass-cancel storm attacks.

**Definition of Done (Acceptance Criteria):**
* [ ] WS close includes disconnect_reason: ABUSE_DISCONNECT, CLIENT_DISCONNECT, NETWORK_TIMEOUT
* [ ] Cancel-on-disconnect only triggers for CLIENT/NETWORK disconnects, not ABUSE
* [ ] Mass-cancel storm prevention: max 1 cancel-on-disconnect per account per 5s

### Task 6.3.8: OHLCV Candlestick Aggregation Engine (Go)

**Objective:** Implement a real-time candlestick/K-line computation service that aggregates trade events into OHLCV bars across multiple timeframes.

**File Locations:** `services/internal/marketdata/candle_aggregator.go`, `services/internal/marketdata/candle_store.go`

**Implementation:**
1. NATS consumer: subscribe to trade events from NATS JetStream (published by Bridge Service, Task 3.3.10).
2. Supported timeframes: 1m, 5m, 15m, 30m, 1h, 4h, 1D, 1W, 1M. Each instrument maintains independent candle state per timeframe.
3. In-progress candle: maintained in memory. On each trade: update High (max), Low (min), Close (last), Volume (cumulative). Open = first trade price in interval.
4. Candle close: on interval boundary, finalize candle and persist to ClickHouse `candles` table (MergeTree, partitioned by day, indexed by instrument_id + timeframe + timestamp).
5. Real-time push: publish in-progress candle updates via WS channel `kline@{symbol}_{timeframe}` (throttled to max 2 updates/sec per channel).
6. Historical candle REST API: `GET /api/v1/klines/{symbol}?interval=1h&from=&to=&limit=500`. Backed by ClickHouse query. Max 1500 candles per request. (Param unified to `interval` per spec §8.3/§8.4 conventions — remediation #10 supersedes prior `timeframe`; WS channel naming `kline@{symbol}_{timeframe}` is unaffected.)
   - **Pre-Materialized Candlestick & TradingView UDF Contract (added 2026-09-27, remediation #38):** All charting and candle endpoints (including `GET /api/v1/klines/{symbol}`, the TradingView Lightweight Charts feed, and TradingView UDF `/history`) MUST query directly from pre-materialized `fx_klines` table aggregates in ClickHouse/PostgreSQL. Scanning `fx_trades` or performing dynamic in-memory candle aggregation during HTTP request handling is strictly prohibited.
7. Gap handling: if no trades in an interval, emit a candle with Open=High=Low=Close=previous_close, Volume=0.
8. Alignment: 1D candles aligned to 00:00 UTC; 1W to Monday 00:00 UTC; 1M to first of month 00:00 UTC.

**Definition of Done (Acceptance Criteria):**
* [ ] Trade events aggregated across all 13 timeframes (supersedes prior 9; 1s memory-only)
* [ ] Completed candles persisted to ClickHouse within 1s of interval close
* [ ] In-progress candles pushed via WS at ≤2 updates/sec
* [ ] Historical candle REST API returns paginated results from ClickHouse
* [ ] Zero-volume candles emitted for no-trade intervals with previous close carried forward

**SDD Checklist:**
- [ ] Spec checkpoint: OHLCV aggregation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: first candle ever (no previous close), DST transition for daily candles, very high trade rate (10k+ trades/sec)

---

### Task 6.3.9: WebSocket Session Resume & Sequence Replay Protocol

**Objective:** Enable WebSocket clients to resume subscriptions after brief disconnects without missing messages by replaying from a sequence-based in-memory buffer.

**File Locations:** `services/internal/marketdata/ws_session_resume.go`

**Implementation:**
1. Every WS message on every channel includes a monotonically increasing `seq` number (uint64, per channel).
2. Server maintains a bounded in-memory ring buffer per channel: last 60 seconds or 10,000 messages, whichever is smaller.
3. On client reconnect: client sends `{"action": "resume", "channel": "book@EURUSD", "last_seq": 12345}` (control-frame discriminator `action` per §10.5 — remediation #9 supersedes prior `op`).
4. Server looks up channel buffer: if `last_seq` is within buffer range, replay all messages from `last_seq + 1` to current, then switch to live streaming.
5. If `last_seq` is too old (outside buffer) or invalid (`last_seq > current_seq`): send a full snapshot for the channel, then switch to live streaming. Include `{"type": "snapshot", "reason": "gap_too_large"}` (or `"invalid_sequence"`) metadata, enforcing fail-closed client state synchronization (spec §10.5, remediation #38).
6. Sequence numbers reset to 0 on server restart — clients must handle this by falling back to snapshot.
7. Private channels (orders, positions): same protocol but keyed by (user_id, channel).

**Definition of Done (Acceptance Criteria):**
* [ ] WS messages include monotonic seq numbers per channel
* [ ] Resume within 60s window replays missed messages without loss
* [ ] Resume beyond buffer window falls back to full snapshot
* [ ] Private channels support per-user sequence tracking
* [ ] Server restart resets sequences; clients detect and request snapshot

**SDD Checklist:**
- [ ] Spec checkpoint: WS session resume protocol — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: resume with seq=0 (full snapshot), concurrent resumes on same channel, buffer full eviction

---

### Task 6.3.10: WebSocket Request-Response Dispatcher & In-Band Routing

**Objective:** Implement bidirectional frame parsing and request-response routing in the WebSocket gateway to support low-latency order actions concurrently with market data streaming (§24 #253). Added 2026-09-22 (remediation #12).

**File Locations:** `services/internal/marketdata/ws_dispatcher.go`, `services/internal/marketdata/ws_router.go`

**Implementation:**
1. Incoming frame discriminator: inspect client frames for `action` field. If `action` starts with `order.` (`order.place`, `order.cancel`, `order.modify`, `order.batch`, `order.status`), route to Order Gateway trading dispatcher (Phase-05 Task 5.3.31).
2. Concurrency isolation: order actions are processed on dedicated goroutines per connection and do not block downstream market data write loops or subscription managers.
3. Outbound write synchronization: protect the underlying gorilla WebSocket connection with a thread-safe mutex ring buffer so market data broadcasts and interactive trading response ACKs serialize cleanly without frame interleaving.
4. Response framing: correlated responses include `request_id`, `type: "response"`, `status: "ACK"|"NACK"`, and execution metadata.
5. In-flight timeout: if Aeron core IPC does not respond within 500ms, emit correlated timeout NACK: `{"type": "error", "request_id": "<id>", "error": "CORE_TIMEOUT"}`.

**Definition of Done (Acceptance Criteria):**
* [ ] In-band frame router separates control frames, subscription frames, and interactive trading frames
* [ ] Thread-safe write coordination prevents corrupted or interleaved WebSocket frames
* [ ] Order ACKs delivered over same connection within 5ms round trip
* [ ] Zero frame drops on simultaneous heavy L2 market data stream and rapid order entry

**SDD Checklist:**
- [ ] Spec checkpoint: WS request-response dispatcher with thread-safe write synchronization — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: client sends malformed JSON during burst market data, socket backpressure during order ACK, concurrent order actions on same socket

---

### Task 6.3.11: Book Ticker / Best Bid-Offer (BBO) stream

Book Ticker / Best Bid-Offer (BBO) stream — `bbo@{symbol}` WS channel emitting `{bid, bid_qty, ask, ask_qty, ts}` with zero conflation on every top-of-book change. Also available as SBE multicast message type on A/B feeds (extends Task 6.3.6). Dedicated low-latency path bypassing L2 conflation timer. Max ~5000 updates/sec per symbol on liquid pairs.

**SDD Checklist:**
- [ ] Spec checkpoint: BBO emits every top-of-book change without conflation (§24 #261) — defined first, validated against spec

---

### Task 6.3.12: Aggregated trades stream

Aggregated trades stream — `aggTrades@{symbol}` WS channel consolidating all individual fills from the same taker order at the same price level into a single message: `{agg_trade_id, symbol, price, total_qty, first_trade_id, last_trade_id, ts, is_buyer_maker}`. Reduces client message volume by 10–50× during high-activity periods. Coexists with raw `trades@{symbol}` stream.

**SDD Checklist:**
- [ ] Spec checkpoint: aggregate-trade events preserve taker/price grouping and trade-ID lineage (§24 #262) — defined first, validated against spec

---

### Task 6.3.13: Liquidation feed (public)

Liquidation feed (public) — `liquidations@{symbol}` and `liquidations@all` WS channels broadcasting forced liquidation events: `{symbol, side, order_type, price, qty, ts, is_auction}`. Events delayed by 2 seconds to prevent front-running of active liquidation auctions. Liquidation data sourced from Phase-19 liquidation scanner via NATS `margin-events` stream. *(Forward dependency declared 2026-09-27, remediation #35: Phase-19 is not in this phase's prerequisite list — Task 6.3.13 is delivered as a transport shell at Phase-06 with the data feed wired when Phase-19 lands; the deliverables list updated accordingly.)*

**SDD Checklist:**
- [ ] Spec checkpoint: public liquidation feed is delayed and never front-runs active auctions (§24 #263) — defined first, validated against spec

---

### Task 6.3.14: Additional kline timeframes

Additional kline timeframes — expand OHLCV engine (Task 6.3.8) from 9 to 13 timeframes by adding `1s`, `2h`, `6h`, `8h` (supersedes prior 9-timeframe list). `1s` candles are memory-only (not persisted to ClickHouse) with 60-second ring buffer. `8h` candles align with major FX session boundaries (Sydney/London/New York). `2h` and `6h` fill analysis gaps. Update kline subscription format: `kline@{symbol}_{timeframe}` accepting all 13 intervals — channel format reconciled with §8.4 item 1 and Task 6.3.8 (remediation #35; supersedes the prior `kline@{symbol}:{interval}`, which mixed the REST param name into the WS channel namespace).

**SDD Checklist:**
- [ ] Spec checkpoint: canonical candle set contains 13 aligned intervals (§24 #264) — defined first, validated against spec

---

### Task 6.3.15: Configurable depth levels

Configurable depth levels — extend L2 subscription (Task 6.3.3 or equivalent) to accept depth parameter: `depth@{symbol}:{levels}:{update_ms}` where `levels` ∈ {5, 10, 20} and `update_ms` ∈ {100, 250, 1000}. Default remains `20:100`. 5-level subscriptions reduce bandwidth by 75% for mobile/lightweight clients. Server multiplexes from internal 20-level snapshots.

**SDD Checklist:**
- [ ] Spec checkpoint: depth subscriptions enforce supported level/cadence combinations (§24 #265) — defined first, validated against spec

---

### Task 6.3.16: WS stream multiplexing limits & documentation

WS stream multiplexing limits & documentation — codify maximum 200 subscriptions per WS connection (sufficient for FX with ~100 instruments). Document dynamic subscribe/unsubscribe protocol: `{"action": "subscribe", "channels": [...]}` / `{"action": "unsubscribe", "channels": [...]}`. Server responds with `{"type": "subscribed", "channels": [...], "total": N}`. Reject with `WS_MAX_SUBSCRIPTIONS_EXCEEDED` if limit breached. Register error code in §23.

**SDD Checklist:**
- [ ] Spec checkpoint: WS multiplexing enforces documented subscription limits (§24 #265) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.17: Public Reference-Price and Execution-Rule Distribution

**Objective:** Publish the reference prices and execution collars consumed by Task 2.3.17.

**Implementation:**
1. Add REST/WS methods for execution rules, current reference price, and calculation provenance; add real-time `referencePrice@{symbol}` stream.
2. Include source mode (`ORACLE_MEDIAN|TRADE_SMA|MANUAL_FIXED`), validity timestamp, staleness, and per-side multipliers without leaking proprietary feed details.
3. Private execution events include `expiry_reason` when a taker remainder expires under the rule.
4. Source all external references from Phase-19.5; no duplicate price calculation in MarketData.

**SDD Checklist:**
- [ ] Spec checkpoint: execution rules, reference prices, provenance, stream, and expiry reasons are client-visible (§24 #278) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.18: Negotiated SBE for REST, WebSocket, and Private Streams

**Objective:** Extend SBE beyond institutional multicast to version-negotiated API responses and user events.

**Implementation:**
1. REST negotiates `Accept: application/sbe` plus schema ID/version; WS negotiates response format and schema during handshake.
2. Public market data, interactive trading responses, and private account/order events support JSON and SBE with equivalent semantics.
3. Maintain machine-readable latest/deprecated/retired schema lifecycle; deprecated schemas receive warnings and remain compatible for at least six months; retired schemas fail explicitly.
4. CI verifies forward-compatible decoding, unknown enum/message sentinels, JSON fallback, and schema retirement.

**SDD Checklist:**
- [ ] Spec checkpoint: REST/WS/private SBE negotiation and six-month schema lifecycle are interoperable (§24 #284) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.19: Graceful Client Drain and Server-Shutdown Advisory

**Objective:** Give clients deterministic reconnect signals before planned endpoint shutdown.

**Implementation:**
1. Emit `server.shutdown` on JSON/SBE WS connections before closure with reason, reconnect target, and deadline when known.
2. Stop new subscriptions/order requests, allow cancels and in-flight responses through the drain deadline, then close with a registered code.
3. Integrate blue-green deployment and Maintenance mode so replacement endpoints are ready before advisory emission.
4. Phase-18 emits equivalent FIX `News` countdown/advisory; clients reconnect without duplicate subscriptions or orders.

**SDD Checklist:**
- [ ] Spec checkpoint: planned shutdown drains clients with explicit WS/FIX reconnect advisories and cancel availability (§24 #289) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.20: All-Market Statistics and Public Block-Trade Tape

**Objective:** Add market-wide discovery streams and delayed transparency for eligible block executions.

**Implementation:**
1. Publish all-symbol mini-ticker and rolling-window statistics for 1h/4h/1d/1w windows from canonical trade data.
2. Publish `blockTrades@{symbol}` only after execution and the configured regulatory delay; never expose resting hidden liquidity or participant identity.
3. Include block trade ID, symbol, price, quantity, execution time, publication time, and venue flags; corrections/busts emit linked correction events.
4. Persist to ClickHouse for Phase-23 historical queries and UI top-mover/heatmap consumption.

**SDD Checklist:**
- [ ] Spec checkpoint: all-market rolling statistics and delayed anonymous block-trade tape are complete and correction-aware (§24 #291) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.21: Slow Consumer Eviction, Disconnect Flood & Feed Failover

**Objective:** Implement strict slow-consumer eviction, disconnect flood rate-limiting, and A/B multicast feed failover recovery per spec §2.7, §10.6, and §24 #305.

**Implementation:**
1. **Slow Consumer Buffer Eviction:** Monitor per-client outbound WebSocket write buffers. If a client's buffer remains saturated for $>2.0\text{s}$, terminate connection with RFC 6455 policy violation code `4008` to protect server memory and neighboring consumers.
2. **Disconnect Flood Throttling:** Enforce rate limit of at most 10 reconnect attempts per minute per IP. Excess reconnections reject with HTTP 429 (`RATE_LIMIT_TIER_EXCEEDED`).
3. **Multicast A/B Dual Loss Recovery:** If both multicast feeds A and B suffer packet loss simultaneously, trigger automatic fallback to TCP snapshot and incremental replay (`SBE_MULTICAST_RECOVERY`).

**Definition of Done (Acceptance Criteria):**
* [ ] Saturated client buffers terminate after 2.0s with code 4008
* [ ] Disconnect and reconnect flood throttled per IP
* [ ] Dual multicast loss recovers via TCP snapshot without lost trades

**SDD Checklist:**
- [ ] Spec checkpoint: Slow consumer eviction, reconnect flood throttling, and multicast failover recovery (§24 #305) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.22: WS Sequence Durability & Market-Data Entitlements

**Objective:** Make the 60s resume window survive restarts and put symbol-level access control on data, per spec §10.7 and §24 #339. Added 2026-09-27 (production-maturity remediation #24).

**Implementation:**
1. **Durable sequence log:** persist the per-symbol `seq` cursor and a bounded gap log across market-data restarts — `seq` never resets to 0 on restart (supersedes reset-and-snapshot-storm behavior). Resume with `last_seq` replays from the ring buffer (60s/10k window) or falls back to snapshot with an explicit `resync` frame; missed-heartbeat count and per-IP reconnect caps are tuned against the Task 6.3.7 10/min throttle.
2. **`request_id` dedup window:** WS trading actions (`order.place/cancel` via Task 6.3.10) share the 60s server dedup window with Phase-05 Task 5.3.42 — retries after `DISCONNECTED` replay the stored ack, never double-submit.
3. **Entitlements:** symbol-level entitlement checks on WS/SBE/private streams (mirroring the FIX `SESSION_NOT_ENTITLED` model); non-display, redistribution and delayed-feed (15-min free tier per §24 #236) enforcement with `ENTITLEMENT_REQUIRED` (HTTP 403, new §23 code); per-tier quote-count/bandwidth fair-use counters.

**Definition of Done (Acceptance Criteria):**
* [ ] Restart preserves seq continuity; resume replays or resyncs with explicit frames
* [ ] WS order retries dedup within 60s; zero double-submits across disconnects
* [ ] Unentitled symbols rejected with ENTITLEMENT_REQUIRED on all surfaces

**SDD Checklist:**
- [ ] Spec checkpoint: durable WS sequence log with gap journal, order-action dedup window, and symbol-level market-data entitlements (§24 #339) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.23: Public Open-Interest Stream & REST

**Objective:** Publish aggregate open interest per pair with history, per spec §10.8 and §24 #357. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. OI aggregator consumes position snapshots (Phase-03 Task 3.3.2) per symbol; publishes `openInterest@{symbol}` WS stream (1s cadence, no conflation on change) and `GET /api/v1/market/open-interest?symbol=&interval=` for history from ClickHouse.
2. Values are LONG|SHORT-summed notionals (FX has no long/short float split to disclose); laggards beyond the 5s oracle gate are flagged stale, never interpolated.

**Definition of Done (Acceptance Criteria):**
* [ ] Live OI stream matches position-store aggregates within 1s
* [ ] History endpoint serves OHLC-style OI candles from ClickHouse

**SDD Checklist:**
- [ ] Spec checkpoint: public open-interest stream and history from position aggregates (§24 #357) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 6.3.24: WebSocket L2/L3 Resynchronization Protocol & Gap Detection Contract

**Objective:** Standardize the WebSocket L2/L3 incremental depth synchronization contract, sequence continuity tagging (`prev_last_seq`), and client resynchronization state machine per spec §10.9 and §24 #408. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/marketdata/ws_server.go`, `services/internal/marketdata/l2_stream.go`, `services/internal/marketdata/sync_contract.go`

**Implementation:**
1. Every L2 depth delta frame emitted over WebSocket carries:
   - `first_seq` (first update sequence in this buffer)
   - `last_seq` (last update sequence in this buffer)
   - `prev_last_seq` (must equal `last_seq` of the immediate prior frame).
2. Codify the institutional client synchronization state machine:
   - Client opens WebSocket and buffers inbound delta frames.
   - Client requests REST snapshot `GET /api/v1/market/depth?symbol={s}&limit=100`, receiving payload with `last_update_id`.
   - Client discards buffered deltas where `last_seq <= last_update_id`.
   - Client applies the first packet satisfying `first_seq <= last_update_id + 1` and `last_seq >= last_update_id + 1`.
   - Client applies subsequent contiguous packets where `current.prev_last_seq == prior.last_seq`. If a gap is detected, client drops local book and triggers resynchronization.
3. Bidirectional Heartbeat: enforce 15s ping/pong. If client fails to reply within 5s of ping, server terminates TCP connection to free resources and trigger dead-man cancel rules.

**Definition of Done (Acceptance Criteria):**
* [ ] Depth updates emit `first_seq`, `last_seq`, and `prev_last_seq` contiguous sequences
* [ ] Gap detection triggers client resync advisory; zero crossed books ($Bid > Ask$) after reconnect
* [ ] Ping/pong heartbeat terminates unresponsive connections after 5s timeout

**SDD Checklist:**
- [ ] Spec checkpoint: WebSocket L2/L3 snapshot/delta resync protocol and sequence continuity contract (§24 #408) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 6.4 Deliverables

- WebSocket server with goroutine-per-connection
- L2 book distribution with 100ms conflation
- Real-time trades stream
- 1s ticker updates
- Private order stream
- Institutional SBE A/B multicast feed with TCP replay and snapshot recovery
- WebSocket request-response dispatcher for interactive trading (Task 6.3.10)
- Reference-price/execution-rule streams and negotiated REST/WS/private SBE
- Graceful shutdown drain advisories; all-market statistics and delayed block tape
- Slow consumer eviction, multicast A/B feed failover & disconnect flood protection (Task 6.3.21)
- Durable WS sequence log, order-action dedup window & symbol-level entitlements (Task 6.3.22)
- Public open-interest stream & history (Task 6.3.23)
- WebSocket L2/L3 client resynchronization contract and gap continuity validation (Task 6.3.24)

---

## 6.5 Dependencies

- Phase 2, Phase 2.5, Phase 5

---

## 6.6 Duration Estimate

13–17 days (supersedes 10–13 — Task 6.3.24 absorbed in range, remediation #37; prior supersedes 10–13 — duration itemization completed 2026-09-27, remediation #35: 11 tasks were omitted from the itemization):
- Task 6.3.1 (WS server): 0.5 day
- Task 6.3.2 (L2 conflation): 1 day
- Task 6.3.3 (Trades): 0.5 day
- Task 6.3.4 (Ticker): 0.5 day
- Task 6.3.5 (Private stream): 0.5 day
- Task 6.3.6 (A/B multicast + recovery): 1.5 days
- Task 6.3.7 (WS rate limiting & abuse protection): 0.5 day (added to itemization, remediation #35)
- Task 6.3.8 (OHLCV candlestick engine): 1 day (added to itemization, remediation #35)
- Task 6.3.9 (WS session resume protocol): 1 day (added to itemization, remediation #35)
- Task 6.3.10 (WS request-response dispatcher): 1 day
- Task 6.3.11 (BBO / book ticker stream): 0.5 day (added to itemization, remediation #35)
- Task 6.3.12 (Aggregated trades stream): 0.5 day (added to itemization, remediation #35)
- Task 6.3.13 (Public liquidation feed): 0.5 day (added to itemization, remediation #35)
- Task 6.3.14 (Kline timeframe expansion): 0.5 day (added to itemization, remediation #35)
- Task 6.3.15 (Configurable depth levels): 0.5 day (added to itemization, remediation #35)
- Task 6.3.16 (Stream multiplexing limits): 0.5 day (added to itemization, remediation #35)
- Task 6.3.17 (Graceful shutdown drain): 0.5 day (added to itemization, remediation #35)
- Task 6.3.18 (Negotiated SBE lifecycle): 1 day (added to itemization, remediation #35)
- Task 6.3.19 (All-market stats & block tape): 0.5 day (added to itemization, remediation #35)
- Task 6.3.20 (Reference-price stream): 0.5 day (added to itemization, remediation #35)
- Task 6.3.21 (Slow consumer eviction & feed failover): 0.5 day
- Task 6.3.22 (WS seq durability & entitlements): 1 day
- Task 6.3.23 (Open-interest stream & history): 0.5 day (absorbed in range)
- Task 6.3.24 (WS L2/L3 resync contract & heartbeat): 0.5 day (absorbed in range, remediation #37)
- Testing: 1 day

---

## 6.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | WS server accepts connections and subscriptions |
| 2 | Heartbeat ping/pong works; stale connections dropped |
| 3 | Subscription limits enforced (20 L2, 5 L3) (§24 #84) |
| 4 | L2 book updates distributed with 100ms conflation |
| 5 | Per-symbol sequence counter maintained |
| 6 | Reconnect with last_seq: replays missed or sends snapshot (§24 #16) |
| 7 | No stale data, no gaps under load (§24 #15) |
| 8 | Trades distributed in real-time (no conflation) |
| 9 | Ticker updates every 1s with correct 24h OHLCV |
| 10 | Private order stream requires JWT auth |
| 11 | Private events filtered by account_id |
| 12 | All order lifecycle events delivered (accepted, rejected, filled, cancelled, expired) |
| 13 | N≥100 WS connections converge with zero drops under load — small-scale functional check only; the scale gate (N≥10,000) is the Phase-08.5 AC #4 test (remediation #35 — supersedes the prior standalone reading, which contradicted the ≥10,000 supersede recorded in AGENTS.md) |
| 14 | L2 depth checksums: CRC32 in depth updates; resync on mismatch (§24 #83) |
| 15 | Market data SLA: p99 WS push ≤ 100ms (§24 #99) |
| 16 | Market data SLA: uptime 99.95% (§24 #99) |
| 17 | Institutional A/B multicast feeds carry identical channel sequences; either single-feed loss creates zero client-visible gap (§24 #166) |
| 18 | TCP replay repairs bounded gaps; snapshot plus queued incrementals converges exactly when replay range is exceeded |
| 19 | SBE duplicate suppression, channel-reset recovery, security definitions and status recovery pass compatibility tests |
| 20 | WS message rate limit enforced (100 control msgs/sec); churn protection disconnects abusive clients; heartbeat 30s/60s timeout functional (spec §24 #215) |
| 21 | OHLCV candles aggregate all 13 intervals; 12 persisted in ClickHouse, 1s memory-only; in-progress pushed ≤2/sec (§24 #226; supersedes prior 9) |
| 22 | WS session resume: client reconnects with last_seq, server replays from ring buffer (60s window) or falls back to snapshot |
| 23 | WS disconnect_reason discriminator prevents abuse-triggered mass-cancel storms; cancel-on-disconnect exempt for ABUSE_DISCONNECT; max 1 CoD per account per 5s (§24 #245) |
| 24 | WS in-band request router dispatches interactive trading actions (`order.*`) to Order Gateway dispatcher and streams correlated ACKs without dropping concurrent market data frames (§24 #253) |
| 25 | BBO emits every top-of-book change without conflation (§24 #261) |
| 26 | Aggregated trades preserve taker/price grouping and first/last trade lineage (§24 #262) |
| 27 | Public liquidation feed is delayed 2s and never exposes an active auction early (§24 #263) |
| 28 | OHLCV supports the canonical 13 intervals including 1s/2h/6h/8h (§24 #264) |
| 29 | Depth subscriptions support 5/10/20 levels and documented cadence; multiplex limit is enforced (§24 #265) |
| 30 | Execution-rule/reference-price REST+WS methods, provenance stream, and expiry reasons are consistent (§24 #278) |
| 31 | REST, interactive WS, and private streams negotiate JSON/SBE with a six-month schema lifecycle (§24 #284) |
| 32 | Planned shutdown emits explicit WS/FIX drain advisories while cancels remain available (§24 #289) |
| 33 | All-market rolling statistics and anonymous delayed block-trade tape are correction-aware (§24 #291) |
| 34 | Slow WebSocket consumers evicted after 2.0s buffer saturation; multicast A/B feed failover operates with zero message loss (§24 #305) |
| 35 | WS seq survives restarts with gap journal (no reset-to-0 storms); order-action retries dedup 60s; symbol-level entitlements enforced with ENTITLEMENT_REQUIRED (§24 #339) |
| 36 | Public open-interest stream (1s) and history from position aggregates; stale-flagged beyond the oracle gate (§24 #357) |
| 37 | WebSocket L2 depth feed emits contiguous sequences with prev_last_seq; client snapshot/delta resync protocol prevents crossed order books (§24 #408) |
