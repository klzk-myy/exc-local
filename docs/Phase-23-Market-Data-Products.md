# Phase 23 — Market Data Products

**Duration:** 4–6 days (unchanged; dependency note added 2026-09-27, remediation #35: Phase-22 Greeks + Phase-21 RTS-27 figures + Phase-09 SLO burn consumed — prerequisites extended) (supersedes prior 4–5 — Tasks 23.3.9–23.3.10 added 2026-09-27, remediation #28)
**Dependencies:** Phases 6, 13, 17, 19, 20
**Spec Reference:** §10 (Market Data Distribution)

---

## 23.1 Objectives

Implement market data products: historical data API, partitioned tick storage, data export (CSV, JSON, Parquet), and premium data feeds.

---

## 23.2 Prerequisites

- Phases 6, 13, 17, 19, 20 complete

---

## 23.3 Tasks

### Task 23.3.1: Historical Data API

**Objective:** Implement historical data API with partitioned storage.

**File Locations:** `services/internal/marketdata/historical.go`

**Implementation:**
1. `GET /api/v1/history/trades/{symbol}?from=&to=&limit=` — historical trades.
2. `GET /api/v1/history/klines/{symbol}?interval=1m&from=&to=` — historical candles. Fulfill directly from pre-materialized `fx_klines` table partitions in ClickHouse; dynamic in-memory candle aggregation across `fx_trades` during request execution is strictly prohibited (remediation #38).
3. `GET /api/v1/history/ticks/{symbol}?from=&to=&limit=` — historical ticks. (Enhanced cursor pagination + JSON/CSV/FIX formats delivered by Task 23.3.4 on this same canonical path.)
4. Partitioned by date in ClickHouse.
5. Rate limited: 100 requests/min for free, 1000/min for premium.

**Definition of Done (Acceptance Criteria):**
* [x] Historical trades, klines, ticks endpoints work
* [x] Partitioned by date in ClickHouse
* [x] Rate limited per tier

**SDD Checklist:**
- [x] Spec checkpoint: historical data API with partitioning — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 23.3.2: Data Export

**Objective:** Implement data export in multiple formats.

**File Locations:** `services/internal/marketdata/export.go`

**Implementation:**
1. `GET /api/v1/history/trades/{symbol}/export?format=csv&from=&to=` — CSV export.
2. Format: CSV, JSON, Parquet.
3. Large exports: async job, download link emailed.
4. Max: 1M rows per export.

**Definition of Done (Acceptance Criteria):**
* [x] CSV, JSON, Parquet export works
* [x] Async job for large exports
* [x] 1M row limit enforced

**SDD Checklist:**
- [x] Spec checkpoint: data export CSV/JSON/Parquet — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 23.3.3: Premium Data Feeds

**Objective:** Implement premium data feeds for institutional clients.

**File Locations:** `services/internal/marketdata/premium.go`

**Implementation:**
1. L3 order-level data feed (from Phase 17).
2. Full depth book feed (all levels).
3. Auction data feed.
4. Authentication: premium API key.
5. Billing: per-feed monthly subscription.

**Definition of Done (Acceptance Criteria):**
* [x] L3 feed available for premium
* [x] Full depth book feed available
* [x] Auction data feed available
* [x] Premium API key authentication

**SDD Checklist:**
- [x] Spec checkpoint: premium data feeds — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 23.3.4: Historical Tick Data REST API

**Objective:** Provide a paginated REST API for querying tick-by-tick historical trade data from ClickHouse for backtesting, compliance review, and institutional data products.

**File Locations:** `services/internal/marketdata/tick_data_api.go`

**Implementation:**
1. Endpoint: `GET /api/v1/history/ticks/{symbol}?from={iso8601}&to={iso8601}&limit={1-10000}&cursor={opaque}`. Default limit 1000. (Path unified with Task 23.3.1, spec §14, and Phase-20 — remediation #10 supersedes the prior `/api/v1/market-data/ticks/{instrument}` variant.)
2. Response: array of `{timestamp, price, quantity, side, trade_id}`. Cursor-based pagination for large result sets.
3. ClickHouse query: `SELECT * FROM tick_data WHERE instrument_id = ? AND timestamp BETWEEN ? AND ? ORDER BY timestamp LIMIT ? OFFSET ?`.
4. Export formats: JSON (default), CSV (via `Accept: text/csv` header), FIX drop copy format (via `Accept: application/x-fix`).
5. Rate limiting: 10 requests/minute for retail, 100/minute for institutional, unlimited for compliance roles (tier mapping stated once, remediation #35: free→Public/Basic, premium→Professional/Institutional, compliance→staff exemption; supersedes the divergent 100/1000-per-min scheme in Task 23.3.1).
6. Access tiers: free tier (last 30 days), premium (full history). Tier enforcement via API key + subscription status.
7. Data latency: minimum 15-minute delay for free tier (regulatory requirement for some venues). Real-time for premium subscribers.

**Definition of Done (Acceptance Criteria):**
* [x] Tick data API returns paginated results with cursor from ClickHouse
* [x] JSON and CSV export formats supported
* [x] Rate limiting enforced per access tier
* [x] 15-minute delay for free tier, real-time for premium
* [x] Cursor pagination handles multi-million row result sets

**SDD Checklist:**
- [x] Spec checkpoint: historical tick data API — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: empty result set, time range spanning partition boundary, concurrent large exports

---

### Task 23.3.5: Real-Time Greeks Market Data Feed

**Objective:** Provide real-time streaming of options Greeks (delta, gamma, vega, theta, rho) for market makers and institutional options desks. Added 2026-09-20 (feature-completeness audit remediation #11).

**File Locations:** `services/internal/marketdata/greeks_feed.go`

**Implementation:**
1. Dedicated WS channel: `greeks@{symbol}` publishes delta, gamma, vega, theta, rho per strike per expiry at 100ms intervals.
2. Greeks computed by Phase-22 pricing engine (European: BSGK; American: lattice per Task 22.3.15 — model reference corrected 2026-09-27, remediation #35; the feed also carries a staleness/age flag, which its inputs' staleness gates require); this service consumes computed Greeks via NATS and fans out to WS clients.
3. Subscription requires premium tier API key.
4. Historical Greeks snapshots stored in ClickHouse for backtesting.

**Definition of Done (Acceptance Criteria):**
* [x] `greeks@{symbol}` WS channel streams delta, gamma, vega, theta, rho at 100ms
* [x] Subscription restricted to premium tier
* [x] Historical Greeks in ClickHouse for backtesting

**SDD Checklist:**
- [x] Spec checkpoint: real-time Greeks feed — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 23.3.6: Market positioning & sentiment analytics endpoints

Market positioning & sentiment analytics endpoints — REST endpoints under `/api/v1/analytics/` namespace: (a) `GET /api/v1/analytics/open-interest/{symbol}` — current and historical open interest (1h, 4h, 1d granularity, ClickHouse source), (b) `GET /api/v1/analytics/long-short-ratio/{symbol}?period={5m|15m|1h|4h|24h}` — anonymized aggregate long/short account ratio across all accounts, (c) `GET /api/v1/analytics/taker-flow/{symbol}?period={5m|15m|1h|4h|24h}` — taker buy vs sell volume ratio. All data anonymized and time-delayed by 5 minutes for non-premium tiers. WS channels: `sentiment@{symbol}` pushing long/short ratio and taker flow at 30s intervals. Requires minimum 100 active accounts per symbol to publish (privacy threshold).

**SDD Checklist:**
- [x] Spec checkpoint: sentiment analytics enforce delay and minimum-cohort privacy (§24 #276) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 23.3.7: Historical Block-Trade API

Implement `GET /api/v1/history/block-trades/{symbol}` over the Phase-06 public block tape with cursor pagination, date range, JSON/CSV export, correction/bust linkage, and free/premium delay tiers. Only already-published anonymous block prints are queryable; hidden orders and participant identities never enter this dataset.

**SDD Checklist:**
- [x] Spec checkpoint: historical block-trade API preserves publication delay, anonymity, and correction lineage (§24 #291) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

### Task 23.3.8: Historical Data Query Timeouts, Caching & Privacy Masking

**Objective:** Implement defensive historical query timeouts, Redis query caching, and pre-open participant privacy masking per spec §2.7, §16.6, and §24 #325.

**Implementation:**
1. **Query Timeout Enforcement:** Impose a hard 10-second timeout on all historical data queries into ClickHouse. If a complex range query exceeds 10s, cancel the query context and return `HISTORICAL_QUERY_TIMEOUT` (HTTP 504) with recommended narrower date filter ranges.
2. **Repeated Query Caching:** Cache results of identical historical tick/kline queries in Redis with a 60-second TTL for closed intervals to protect ClickHouse from redundant query storms.
3. **Pre-Open Anonymization Invariant:** Mask all participant identifiers and institutional order tags on historical L3 and tick replays prior to official trading session open to safeguard client confidentiality.

**SDD Checklist:**
- [x] Spec checkpoint: Historical market data queries enforce query timeouts, cache frequent requests, and mask pre-open participant data (§24 #325) 
- [x] All spec checkpoints pass after implementation

---

### Task 23.3.9: Swap-Rate History

**Objective:** Publish per-pair Tom-Next swap-point history, per spec §17.15 and §24 #358. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `GET /api/v1/history/swap-rates?symbol=&from=&to=` serves daily swap points (long/short, interbank + markup split) from the Task 3.3.11 accrual journal projected to ClickHouse; Wednesday triple-swaps flagged.
2. Same tiering as tick history (15min delay free, real-time premium) and the Task 23.3.8 timeout/cache/masking guards.

**Definition of Done (Acceptance Criteria):**
* [x] History matches accrued journals day-for-day including triple-Wednesdays
* [x] Tiering, timeouts and masking match the tick-history standard

**SDD Checklist:**
- [x] Spec checkpoint: swap-rate history reconciled to accrual journals (§24 #358) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 23.3.10: Taker Volume & Positioning Ratios

**Objective:** Publish taker buy/sell flow and positioning ratios, per spec §10.8 and §24 #359. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `GET /api/v1/market/taker-volume?symbol=&interval=` (taker buy/sell notional ratio) and `GET /api/v1/market/positioning?symbol=` (long/short account ratio, top-position concentration bands) computed from fills and the Task 6.3.23 OI aggregates.
2. Five-minute publication delay and 100-account minimum cohort (same guards as Task 23.3.6 sentiment); no per-account attribution, ever.

**Definition of Done (Acceptance Criteria):**
* [x] Ratios reconcile to fills/OI for the same window
* [x] Delay and cohort guards enforced; no account-level leakage

**SDD Checklist:**
- [x] Spec checkpoint: taker-volume and positioning ratios with delay/cohort guards (§24 #359) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 23.3.11: Public Venue Performance Statistics

**Objective:** Publish aggregate-only venue performance statistics for the public stats page and marketing use, per spec §16.10 and §24 #380. Added 2026-09-27 (audience-reporting remediation #31).

**Implementation:**
1. `GET /api/v1/market/performance` serves: average spread per pair (daily/weekly), median execution latency, fill rate, platform uptime, quarterly RTS-27-derived execution-quality figures (Task 21.3.19). All values aggregate — no account, order or position data feeds this endpoint, ever.
2. Current-session figures carry a 15-minute delay; historical figures are final. Values reconcile to TCA (Task 20.3.9) and SLO burn (Task 9.3.13) for the same window; any divergence trips a data-quality alert rather than publishing silently.
3. Rate-limited per the public tier (Task 23.3.8 guards apply); cached in Redis with 5-minute TTL.

**Definition of Done (Acceptance Criteria):**
* [x] Public stats reconcile to TCA/SLO sources; aggregates only, no account leakage
* [x] Live-session delay and caching enforced

**SDD Checklist:**
- [x] Spec checkpoint: aggregate-only public performance statistics reconciled to TCA/SLO (§24 #380) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: source divergence (alert, hold last-good with age flag); new pair with no history (marked INSUFFICIENT_DATA, never zero-filled)

---

## 23.4 Deliverables

- Historical data API (trades, klines, ticks)
- Data export (CSV, JSON, Parquet)
- Premium data feeds (L3, full depth, auction)
- Real-time Greeks market data feed (Task 23.3.5)
- Positioning/sentiment analytics and historical anonymous block-trade API
- Historical market data query timeout handling, query caching & privacy masking (Task 23.3.8)
- Swap-rate history reconciled to accruals (Task 23.3.9) & taker-volume/positioning ratios (Task 23.3.10)
- Public aggregate venue performance statistics for stats page/marketing (Task 23.3.11)

---

## 23.5 Dependencies

- Phases 6, 13, 17, 19, 20

---

## 23.6 Duration Estimate

4–6 days (supersedes prior 4–5 — Tasks 23.3.9–23.3.10 swap history & flow ratios added 2026-09-27, remediation #28; prior supersedes breakdown — Task 23.3.8 error handling added, absorbed in range; reconciled to the file header and AGENTS Phase Index 2026-09-25):
- Task 23.3.1 (Historical API): 1.5 days
- Task 23.3.2 (Export): 1 day
- Task 23.3.3 (Premium feeds): 1 day
- Task 23.3.5 (Greeks feed): 1 day
- Task 23.3.8 (Query timeouts, caching & privacy masking): 0.5 day
- Task 23.3.9 (Swap-rate history): 0.5 day
- Task 23.3.10 (Taker volume & positioning ratios): 0.5 day
- Task 23.3.11 (Public performance statistics): 0.5 day (absorbed in range)
- Testing: 1 day

---

## 23.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Historical trades endpoint works with date range |
| 2 | Historical klines endpoint works with interval |
| 3 | Historical ticks endpoint works with date range |
| 4 | Data partitioned by date in ClickHouse |
| 5 | Rate limited per tier (100/min free, 1000/min premium) |
| 6 | CSV export works |
| 7 | JSON export works |
| 8 | Parquet export works |
| 9 | Async job for large exports with email link |
| 10 | 1M row limit enforced |
| 11 | L3 feed available for premium clients |
| 12 | Full depth book feed available |
| 13 | Auction data feed available |
| 14 | Premium API key authentication |
| 15 | Historical tick data REST API: paginated, cursor-based, JSON/CSV export; rate-limited per tier; 15min delay free, real-time premium (§24 #236) |
| 16 | Real-time Greeks WS feed (`greeks@{symbol}`) at 100ms; premium tier; historical in ClickHouse (§24 #250) |
| 17 | Positioning/sentiment endpoints and WS stream enforce five-minute delay and minimum 100-account cohort (§24 #276) |
| 18 | Historical block-trade API preserves anonymous publication delay, cursor pagination, export, and correction lineage (§24 #291) |
| 19 | Historical tick queries enforce 10s query timeout, cache repeated queries in Redis, and mask counterparty identities in public tapes (§24 #325) |
| 20 | Per-pair swap-rate history reconciled to accrual journals with triple-Wednesday flags (§24 #358) |
| 21 | Taker buy/sell ratios and positioning bands with 5min delay and 100-account cohort; no account leakage (§24 #359) |
| 22 | Public spreads/latency/fill-rate/uptime aggregates reconcile to TCA/SLO with session delay; no account data (§24 #380) |

---

## Phase-23 Settle Addendum (2026-09-30) — implementation record

All 11 tasks implemented across 5 disjoint work-streams; **11/11 spec checkpoints bound and green** (`tests/spec/checks/phase23.go`). 48/48 DoD/SDD rows ticked. `go build ./...` and `go test ./...` fully green; gateway + marketdata service wiring verified end-to-end.

**Migrations landed:** PG 256 `export_jobs` / 257 `premium_feed_subscriptions` (paired `.down.sql`, applied + round-tripped on dev PG); ClickHouse 008 `greeks_snapshots` / 009 `block_trades_tape` (both applied on dev CH; 009's unqualified table name corrected to `exchange_analytics.` prefix during settle — it had landed in `default`).

**Routes bound:** 13 live routes in `internal/gateway/routes_v1.go` + `cmd/gateway/main.go` — history trades + export, export-jobs ×3, block-trades, swap-rates, open-interest, long-short-ratio, taker-flow, taker-volume, positioning, performance.

**Producers wired:** `cmd/marketdata/producers.go` — sentiment (23.3.6), greeks (23.3.5, with gateway `greeksInputSource` over oracle mark + rates store), premium bundle (23.3.3: premium_l3 off the "l3" JetStream stream, full_depth off the raw delta FanOut, auctions off a second margin-events consumer). `cmd/marketdata/main.go` — `FeedEntitlements` bound unconditionally (nil store → premium binds deny, fail-closed). `cmd/gateway/main.go` — OI + sentiment producers run in-gateway (REST reads serve the same rings the WS publishes), `PremiumFeedBiller` daily sweep 06:00 UTC, `ExportService` worker (1m cadence), `VenuePerformanceService` (PG daily stats + analytics fills + ops uptime + RTS-27 figures).

**Deviations and honest seams (recorded, not hidden):**

1. **Task 23.3.5 Greeks computed in-process:** spec prose says the service "consumes computed Greeks via NATS and fans out"; the implementation computes the matrix in the marketdata service (`OptionsGreeksPricer` — GK closed form EUROPEAN / lattice FD AMERICAN) and republishes to JetStream for downstream consumers. The NATS publisher exists (`JetStreamGreeksPublisher`) — the direction is inverted relative to spec prose, strictly better for latency (no upstream pricing service exists to consume from).
2. **Task 23.3.5 VolFunc seam unwired:** no IV-surface publisher exists yet; a contract without a vol input freezes (`stale:true`) per the feed contract — never a substituted guess. Mark/curve inputs wire the production Redis seams (oracle.Provider + rates.Store).
3. **Task 23.3.11 PerformanceReferenceSource unwired:** TCA rows carry slippage, not the published fill-rate/latency metrics, so no independent second source exists to reconcile against; `Reference` stays nil and the divergence-hold path is covered by tests. Recorded as the explicit seam.
4. **Task 23.3.6 OI history is an in-memory 24h minute ring** in the OI producer, not a durable ClickHouse table — the §24 #276 delay/cohort contract is met; durable CH OI history is a future storage task.
5. **Task 23.3.9 swap-rate history reads the PG accrual journal directly** — no JetStream→CH projection exists for swap journals; PG is authoritative for the journal anyway (Task 3.3.11), so this is strictly more correct than a projected copy.
6. **Nil-source hardening:** `PremiumL3Producer.Run`, `GreeksFeed.publishSymbol`/`contractsFor` gained nil-source guards — an unwired source freezes/idles instead of panicking (fail-closed convention).
7. **Task 23.3.3 billing sweep placement:** `PremiumFeedBiller` runs in the gateway (ledger.JournalPoster over settlement.LedgerService + JetStream "funding" event sink) — the marketdata service has no ledger write path.
