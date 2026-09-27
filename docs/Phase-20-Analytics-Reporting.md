# Phase 20 — Analytics & Reporting (ClickHouse)

**Duration:** 8–11 days (unchanged; header reconciled to §20.6, remediation #35) (supersedes 6–9 — Tasks 20.3.14–20.3.15 costs disclosure & depreciation notices added 2026-09-27, remediation #30; prior supersedes 6–8 — Tasks 20.3.12–20.3.13 added 2026-09-27, remediation #28; prior supersedes 5–7 — Task 20.3.6 statements added 2026-09-15; header drift repair 2026-09-27 aligns header to §20.6)
**Dependencies:** Phases 3, 11, 13
**Spec Reference:** §16 (Analytics & Reporting), §24 (Acceptance Criteria)

---

## 20.1 Objectives

Implement ClickHouse analytics: tick history storage, trade analytics, P&L reporting, and the ETL pipeline from PostgreSQL to ClickHouse.

---

## 20.2 Prerequisites

- Phases 3, 11, 13 complete

---

## 20.3 Tasks

### Task 20.3.1: ClickHouse Schema & ETL

**Objective:** Create ClickHouse schema and ETL pipeline from PostgreSQL.

**File Locations:** `deploy/clickhouse/`, `services/internal/analytics/etl.go`

**Implementation:**
1. ClickHouse tables: `ticks`, `trades`, `ohlcv`, `account_pnl`, `volume_stats`.
2. MergeTree engine with partitioning by date.
3. ETL: NATS JetStream (Bridge-published trade ticks) → analytics consumer → ClickHouse; PostgreSQL GL feeds (income ledger, trial balance) via change-data or batch projection — source and bus corrected 2026-09-27, remediation #35; supersedes the prior "PostgreSQL → ClickHouse via Kafka", which named a bus not in the stack (§2.3.1 committed NATS JetStream) and an OLTP source for tick history.
4. Real-time: trade ticks inserted within 1s.
5. Batch: daily aggregation for OHLCV, volume stats.
6. Throughput: sustain 50,000 inserts/sec on the tick-ingest path (spec §24 #65) — batch inserts + async insert mode; verified under the Phase-8.5 load profile.
7. Analytics dashboard data feed: latency, throughput, fill rate, and P&L metrics exposed for the analytics dashboard (spec §24 #68; rendered by Phase-7 Grafana/Phase-10 admin views).

**Definition of Done (Acceptance Criteria):**
* [ ] ClickHouse tables created (ticks, trades, ohlcv, pnl, volume)
* [ ] ETL pipeline: PostgreSQL → ClickHouse
* [ ] Real-time ticks within 1s
* [ ] ClickHouse sustains 50,000 inserts/sec on tick ingest (spec §24 #65)
* [ ] Analytics dashboard metrics exposed: latency, throughput, fill rate, P&L (spec §24 #68)
* [ ] Batch aggregation for OHLCV

**SDD Checklist:**
- [ ] Spec checkpoint: ClickHouse for tick history + analytics — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.2: Tick History

**Objective:** Store and query tick history.

**File Locations:** `services/internal/analytics/ticks.go`

**Implementation:**
1. Every trade tick stored in ClickHouse `ticks` table.
2. `GET /api/v1/history/ticks/{symbol}?from=&to=&limit=` — query tick history.
3. Retention: 90-day TTL for raw `ticks` per spec §16.1 (supersedes prior "5 years" — the 5-year retention applies to aggregated OHLCV/volume_stats, not raw ticks).
4. Compression: ClickHouse LZ4.

**Definition of Done (Acceptance Criteria):**
* [ ] Ticks stored in ClickHouse
* [ ] Tick history query works with date range
* [ ] Raw tick TTL 90 days; aggregated OHLCV retained 5 years (spec §16.1/§16.2)
* [ ] LZ4 compression

**SDD Checklist:**
- [ ] Spec checkpoint: tick history retention (90d raw / 5yr aggregates) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.3: OHLCV Candles

**Objective:** Generate and store OHLCV candles at multiple timeframes.

**File Locations:** `services/internal/analytics/ohlcv.go`

**Implementation:**
1. Timeframes: project the 12 persisted intervals `1m, 5m, 15m, 30m, 1h, 2h, 4h, 6h, 8h, 1D, 1W, 1M`; Phase-06's 1s interval remains memory-only (remediation #14 supersedes prior 9/6 lists).
2. Materialized views in ClickHouse for real-time analytics aggregation — projections over the `candles` table computed by Phase-06 Task 6.3.8 (single-owner: Phase-06 computes candles; Phase-20 never recomputes them).
3. `GET /api/v1/history/klines/{symbol}?interval=1m&from=&to=` — query candles.

**Definition of Done (Acceptance Criteria):**
* [ ] OHLCV analytics projection covers all 12 persisted intervals (supersedes prior 9/6; 1s remains memory-only in Phase-06)
* [ ] Materialized views for real-time aggregation
* [ ] Kline query works

**SDD Checklist:**
- [ ] Spec checkpoint: OHLCV at multiple timeframes — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.4: P&L Reporting

**Objective:** Generate P&L reports per account.

**File Locations:** `services/internal/analytics/pnl.go`

**Implementation:**
1. Realized + unrealized P&L per account per day.
2. `GET /api/v1/analytics/pnl?from=&to=` — P&L report.
3. Export: CSV, PDF.
4. Aggregated: per account, per instrument, per day.

**Definition of Done (Acceptance Criteria):**
* [ ] P&L report per account per day
* [ ] CSV and PDF export
* [ ] Aggregation by account, instrument, day

**SDD Checklist:**
- [ ] Spec checkpoint: P&L reporting — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.5: Volume & Stats Reporting

**Objective:** Generate volume and trading statistics.

**File Locations:** `services/internal/analytics/stats.go`

**Implementation:**
1. Volume: per symbol, per day, per hour.
2. Trade count: per symbol, per account tier.
3. Fill rate: orders filled vs submitted.
4. `GET /api/v1/analytics/volume?from=&to=` — volume report.
5. `GET /api/v1/analytics/stats` — trading statistics.

**Definition of Done (Acceptance Criteria):**
* [ ] Volume report per symbol/day/hour
* [ ] Trade count per symbol/tier
* [ ] Fill rate computed
* [ ] Stats endpoints work

**SDD Checklist:**
- [ ] Spec checkpoint: volume + stats reporting — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.6: Client Statements, Trade Confirmations & Fee Invoicing

**Objective:** Implement client-facing statements and trade confirmations (contract notes) per spec §5.28/§8.4 (§24 #143), plus institutional fee invoicing. Added 2026-09-15.

**File Locations:** `services/internal/analytics/statements.go`, `services/internal/analytics/invoicing.go`

**Implementation:**
1. `client_statements` table (migration 049): `statement_id`, `account_id`, `period` (daily/monthly), `generated_at`, `file_ref` (S3 PDF/CSV).
2. `GET /api/v1/account/statements?period=` — statements list + download; daily statements generated EOD (after rollover), monthly on 1st.
3. Statement contents: opening/closing balances per currency, trades, fees, funding movements, unrealized P&L — sourced from GL (Phase-3 Task 3.3.6) so statement == ledger.
4. **Trade confirmations:** per-fill confirmation generated within 60s of execution (MiFID Art. 59 contract note); delivered via email + `GET /api/v1/account/confirmations/{trade_id}` (PDF).
5. **Fee invoicing:** monthly institutional invoice aggregating trading fees, MM rebates, connectivity fees; `GET /api/v1/admin/invoices?account=` (Finance Ops).
6. Retention: statements + confirmations retained 5 years (MiFID record-keeping).

**Migration note:** `migrations/049_client_statements.up.sql` — `client_statements` + `trade_confirmations` + `fee_invoices` tables (spec §5.28).

**Definition of Done (Acceptance Criteria):**
* [ ] Daily + monthly statements generated and downloadable (PDF/CSV) (§24 #143)
* [ ] Trade confirmation within 60s of execution; retrievable via API
* [ ] Statement totals reconcile to GL (zero-sum invariant)
* [ ] Monthly fee invoices for institutional accounts (Finance Ops export)
* [ ] 5-year retention enforced

**SDD Checklist:**
- [ ] Spec checkpoint: client statements + confirmations (§5.28, §24 #143) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: statement during pending settlement, confirmation for busted trade (flagged + adjusted confirmation), invoice for suspended MM program

---

### Task 20.3.7: House Finance Reporting — Trial Balance & ERP Export

**Objective:** Implement the finance layer above the double-entry GL (Task 3.3.6) per spec §16.5: daily trial balance, house P&L / balance-sheet exports, and ERP batch delivery (§24 #205). Added 2026-09-16 (gap audit remediation #5).

**File Locations:** `services/internal/analytics/trial_balance.go`, `services/internal/analytics/erp_export.go`

**Implementation:**
1. Daily trial balance per currency across all GL accounts (assets / liabilities / equity / income / expense), computed EOD after Tom-Next rollover (§17.4); invariants asserted: Σ debits = Σ credits, Assets = Liabilities + Equity (category-9 zero-sum invariant, Phase-13 Task 13.3.2).
2. Finance statements: house P&L statement + balance sheet export (CSV/Parquet) via `GET /api/v1/admin/finance/trial-balance|pnl|balance-sheet` (Finance Ops role); reconciled to sub-ledgers — client balances, nostro positions, insurance fund, fee income.
3. ERP batch: nightly adapter delivers journals/statements to external accounting (configurable SFTP/webhook, checksummed manifest, replay protection).
4. Retention: trial balances and finance exports retained ≥ 7 years (audit/tax).

**Definition of Done (Acceptance Criteria):**
* [ ] Daily trial balance per currency with zero variance to GL (zero-sum invariant verified) (§24 #205)
* [ ] Finance P&L / balance-sheet exports generated by EOD+1h and reconciled to sub-ledgers
* [ ] ERP nightly batch delivered with checksum manifest + replay protection; 7-year retention

**SDD Checklist:**
- [ ] Spec checkpoint: house finance reporting (§16.5, §24 #205) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: first-run with empty GL day, weekend/holiday EOD, currency with no activity

---

### Task 20.3.8: Client Reporting Portal & Trade Confirmation Delivery

**Objective:** Implement automated trade confirmation generation and multi-channel delivery (secure portal, email, optional SWIFT MT515) compliant with MiFID II Article 25.

**File Locations:** `services/internal/reporting/confirmation_service.go`, `services/internal/reporting/templates/`

**Implementation:**
1. Trade confirmation generation: on trade execution, generate confirmation document (PDF/HTML) containing: trade details, execution venue, timestamp, counterparty, total costs (commission + spread + swap estimate), best execution data.
2. Delivery channels: (a) secure portal download in Trader UI, (b) email with encrypted PDF attachment, (c) optional SWIFT MT515 for institutional clients.
3. Delivery timing: T+1 business day for retail (MiFID II Article 25(6)), real-time for institutional (configurable).
4. Template system: Jinja2-style Go templates for PDF generation. Configurable per jurisdiction (regulatory disclosure text varies by EU/UK/US).
5. Statement generation: daily/weekly/monthly account statements summarizing trades, P&L, fees, swaps, deposits, withdrawals.
6. Client reporting portal: secure authenticated page in Trader UI (Phase-10) with document listing, search by date range, and bulk download.
7. Archival: generated documents stored in S3 with 7-year retention (MiFID II requirement). Retrievable by compliance for regulatory inquiries.

**Definition of Done (Acceptance Criteria):**
* [ ] Trade confirmations generated on execution with all regulatory fields
* [ ] Delivery via secure portal and email with encrypted PDF
* [ ] T+1 delivery for retail, real-time for institutional
* [ ] Account statements generated daily/weekly/monthly
* [ ] Documents archived in S3 with 7-year retention

**SDD Checklist:**
- [ ] Spec checkpoint: trade confirmation delivery — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: trade amendment after confirmation sent, multiple fills same order, email delivery failure retry

---

### Task 20.3.9: Trade Cost Analysis (TCA) Engine

**Objective:** Compute execution quality metrics against benchmarks (arrival price, VWAP, ECB fixing rate) for MiFID II Article 27 best-execution obligation and institutional client reporting. Added 2026-09-20 (feature-completeness audit remediation #11). Amended 2026-09-27 (feature completeness audit #36) to include price-improvement and trade-through prevention fields from Phase-02 Task 2.3.22.

**File Locations:** `services/internal/analytics/tca_engine.go`, `services/internal/analytics/tca_report.go`

**Implementation:**
1. Per-fill metrics: slippage vs arrival price (mid at order receipt), slippage vs VWAP (session VWAP at fill time), slippage vs ECB fixing rate (for benchmark orders).
2. **Price-improvement recording (added 2026-09-27, feature completeness audit #36):** consume trade-through prevention events from NATS; record `price_improvement_delta` (limit price − execution price) on fill records; include price-improvement metrics in per-fill and aggregated TCA reports.
3. Aggregated TCA reports: daily, monthly, quarterly per account and per instrument class. Stored in ClickHouse `tca_results` table.
4. REST API: `GET /api/v1/reports/tca/{account_id}?period=monthly&instrument_class=FX_MAJOR`.
5. Auto-generate quarterly TCA summaries per MiFID II RTS 28 requirements, feeding Phase-21 Task 21.3.19.
6. Institutional white-label TCA PDF reports with venue comparison.

**Definition of Done (Acceptance Criteria):**
* [ ] Per-fill TCA: slippage computed vs arrival price, VWAP, and ECB fix
* [ ] Price-improvement delta recorded and included in TCA reports (§24 #400)
* [ ] Aggregated TCA stored in ClickHouse with daily/monthly/quarterly granularity
* [ ] REST endpoint functional with account and period filters
* [ ] Quarterly RTS 28 TCA summary auto-generated

**SDD Checklist:**
- [ ] Spec checkpoint: TCA engine — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: zero-fill orders, benchmark orders with no ECB fix available, multi-fill orders spanning sessions
- [ ] Edge cases: price-improvement delta zero or negative (adverse selection), trade-through events with no fill

---

### Task 20.3.10: User-facing tax calculator tool

User-facing tax calculator tool — `GET /api/v1/account/tax-report?year={YYYY}&method={FIFO|LIFO|HIFO|AVG_COST}` generates downloadable tax report (PDF + CSV). Covers: realized P&L per trade (with selected cost-basis method), swap/rollover income/expense, fees paid, dividends/adjustments. Leverages Phase-03 ledger and Phase-20 reporting infrastructure. Supports date-range filtering. Frontend: Settings → Tax Reports → year/method selector → Generate & Download buttons. Rate-limited to 5 generations per day per account. Third-party export format compatible with common tax tools (Koinly CSV schema).

**SDD Checklist:**
- [ ] Spec checkpoint: tax calculations expose method, costs, period, and reproducible export (§24 #276) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.11: ClickHouse Ingestion Buffering & Dead-Letter Re-ingestion

**Objective:** Implement fault-tolerant asynchronous batch ingestion, local disk spool buffering during ClickHouse downtime, and deterministic deduplication per spec §2.7, §16.6, and §24 #322.

**Implementation:**
1. **Local RocksDB Ingestion Buffer:** If ClickHouse bulk insert times out (>5s) or cluster is unreachable, buffer incoming trade ticks and OHLCV bars to a local embedded RocksDB instance at `/var/spool/exchange/clickhouse_buffer/`.
2. **Backpressure & Drain Automation:** Continuously monitor buffer volume. When ClickHouse connection recovers, drain buffered events in batches of 10,000 with exponential pacing to avoid cluster stampede.
3. **Deduplication Invariant:** Enforce `ReplacingMergeTree` engine with versioning on `(instrument_id, trade_id, event_seq)` ensuring duplicate insertions during replay are completely transparent.

**Definition of Done (Acceptance Criteria):**
* [ ] ClickHouse timeouts divert ticks to local disk spool
* [ ] Spool automatically drains upon ClickHouse recovery
* [ ] ReplacingMergeTree deduplication guarantees zero duplicate ticks

**SDD Checklist:**
- [ ] Spec checkpoint: ClickHouse ingestion buffering and dead-letter recovery fail closed (§24 #322) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.12: Income Ledger Query

**Objective:** Expose per-account income history by type, symbol and time from GL lines, per spec §16.7 and §24 #361. Added 2026-09-27 (Binance-parity remediation #28).

**Implementation:**
1. `GET /api/v1/account/income?type=&symbol=&from=&to=` (envelope + pagination) over journal lines: COMMISSION, SWAP_ROLLOVER, REBATE, NBP_ADJUSTMENT, DUST_CONVERT, FUNDING_FEE — amounts signed, with GL references.
2. Served from a ClickHouse projection synced by the Task 20.3.1 ETL; PG remains the book of record for disputes.

**Definition of Done (Acceptance Criteria):**
* [ ] Every income type queryable per symbol and window with GL linkage
* [ ] Totals reconcile to statements (Task 20.3.6) for the same period

**SDD Checklist:**
- [ ] Spec checkpoint: income ledger by type/symbol/time reconciled to statements (§24 #361) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.13: Daily Account Snapshots

**Objective:** Persist point-in-time balance/position snapshots for audit and time-travel queries, per spec §16.7 and §24 #362. Added 2026-09-27 (Binance-parity remediation #28).

**File Locations:** `services/cmd/snapshot_builder/`, `migrations/093_balance_snapshots.up.sql`

**Implementation:**
1. Daily 23:59 UTC cron snapshots every (account, currency) balance and open position into `balance_snapshots` (migration 093: `account_id`, `currency`, `available`, `locked`, `positions_json`, `snapshot_date`, hash-chained per account).
2. `GET /api/v1/account/snapshots?date=` serves history; snapshots feed the auditor evidence pack (Task 24.3.18) and the §24 #345 audit API.

**Definition of Done (Acceptance Criteria):**
* [ ] Daily snapshots complete for all funded accounts with hash chain intact
* [ ] History endpoint serves any retained date within the retention schedule

**SDD Checklist:**
- [ ] Spec checkpoint: daily hash-chained account snapshots with history API (§24 #362) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 20.3.14: MiFID II Costs & Charges Disclosure (Ex-Ante + Annual Ex-Post)

**Objective:** Disclose all-in trading costs before the client trades and report them annually after the fact, per spec §16.8 and §24 #374. Added 2026-09-27 (reporting-sufficiency remediation #30). Per-trade commission/spread fields (Phase-03 Task 3.3.13) and statements (Task 20.3.6) exist; the ex-ante estimate and the annual aggregated statement do not.

**Implementation:**
1. Ex-ante: `GET /api/v1/account/cost-preview?symbol=&quantity=&side=` returns spread cost, commission, financing estimate and total in account currency before order entry, using live marks + the account's product-profile pricing plan (Task 14.3.13) + Task 3.3.13 tiers; Trader UI renders it in the order ticket (endpoint contract owned here).
2. Ex-post: annual per-account statement aggregating spread, commission, swap/rollover, funding and conversion costs from the income ledger (Task 20.3.12) + fee invoices (Task 20.3.6), with cumulative-effect illustration; delivered via the Task 20.3.6 channels by end of Q1; retained 7 years.
3. Third-party inducements: none paid (venue has no IB/retrocession flow per R10) — stated on both documents so audits stop flagging it.

**Definition of Done (Acceptance Criteria):**
* [ ] Cost preview matches post-trade actuals within rounding; ex-post annual statement reconciles to income ledger + invoices
* [ ] No-inducement statement present on both documents

**SDD Checklist:**
- [ ] Spec checkpoint: ex-ante preview and annual ex-post costs statement reconciled to ledger (§24 #374) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: preview vs volatile-spread drift (timestamped quote); ex-post for closed accounts (retained, downloadable); cent profiles (minor-unit aware)

---

### Task 20.3.15: Leveraged-Position Depreciation Notifications (10% Rule)

**Objective:** Notify retail holders when a leveraged position depreciates 10% (and each further 10% multiple) no later than end of business day, per spec §16.9 and §24 #375. Added 2026-09-27 (reporting-sufficiency remediation #30).

**Implementation:**
1. Detection: hourly job + EOD snapshot build (Task 20.3.13) evaluates each RETAIL leveraged position: `(current_value − open_value) / open_value` from entry price and live marks; thresholds at −10%, −20%, … per position (not per account).
2. Delivery via the Phase-12 notification service (Task 12.3.5) as a new `POSITION_DEPRECIATION` event (email + push + in-app, preferences-respecting); dedupe by querying prior notifications on (event, position_id, threshold) — no new migration.
3. Each notice states the depreciated value, threshold crossed and current margin level (Task 19.3.16 link); repeated crossings after recovery re-notify (new episode, HWM-style reset on return above −5%).

**Definition of Done (Acceptance Criteria):**
* [ ] Every −10% multiple crossing notifies same business day; no duplicate notices per episode
* [ ] Recovery above −5% resets the episode; notices link margin level

**SDD Checklist:**
- [ ] Spec checkpoint: per-position 10%-multiple depreciation notices with episode dedupe (§24 #375) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: flash-spike crossing that recovers intra-hour (EOD catch-up still notifies); position closed before EOD (notice on close if crossed); professional/ECP excluded (retail-only rule)

---

### Task 20.3.16: Marketing-Operations Reporting (Promo Inventory & Consent Cohorts)

**Objective:** Give marketing ops inventory and audience reads without touching client PII or building campaign tooling, per spec §16.11 and §24 #381. Added 2026-09-27 (audience-reporting remediation #31). Referrals/affiliates stay out per R4/R10 — this task covers owned-surface reporting only.

**Implementation:**
1. Promo inventory: `GET /api/v1/admin/promotions/report` over `financial_promotions` (migration 081, Task 21.3.26) — counts by status/channel, approval SLA compliance (approved within review window), expiries in the next 30/90 days; Compliance Officer + marketing-role bindings (read-only).
2. Consent cohorts: aggregate counts of marketing-consent state by channel/jurisdiction from Task 21.3.7 consent records — counts only, no identifiers, minimum cohort 100 (same guard family as Task 23.3.6); below-threshold cohorts report `INSUFFICIENT_COHORT`.
3. Explicit non-scope: no campaign attribution, no per-user engagement tracking, no purchased lists — stated so audits stop requesting it while R4/R10 stand.

**Definition of Done (Acceptance Criteria):**
* [ ] Promo inventory reconciles to the promotions table; SLA breaches flagged
* [ ] Consent cohorts are counts-only with the 100-cohort floor; non-scope stated

**SDD Checklist:**
- [ ] Spec checkpoint: promo inventory and counts-only consent cohorts (§24 #381) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: consent withdrawn mid-campaign window (counts reflect current state with as-of timestamp); expired-promotion content still served from cache (flagged, links Task 21.3.26 render gate)

---

## 20.4 Deliverables

- ClickHouse schema and ETL pipeline
- Tick history (5-year retention)
- OHLCV analytics projections (12 persisted intervals; supersedes prior 9/6)
- P&L reporting
- Volume and stats reporting
- Client statements, trade confirmations, and fee invoicing
- House finance reporting: trial balance, finance P&L/balance-sheet exports, ERP batch (Task 20.3.7)
- Trade Cost Analysis (TCA) engine with MiFID II RTS 28 reporting (Task 20.3.9)
- ClickHouse ingestion buffering & dead-letter queue recovery (Task 20.3.11)
- Income ledger query by type/symbol/time (Task 20.3.12) & daily hash-chained account snapshots (Task 20.3.13)
- MiFID II costs & charges ex-ante preview + annual ex-post statement (Task 20.3.14) & leveraged-position 10% depreciation notifications (Task 20.3.15)
- Marketing-ops reporting: promo inventory + counts-only consent cohorts (Task 20.3.16)

---

## 20.5 Dependencies

- Phases 3, 11, 13

---

## 20.6 Duration Estimate

8–11 days (supersedes 6–9 — Tasks 20.3.14–20.3.15 costs disclosure & depreciation notices added 2026-09-27, remediation #30; prior supersedes 6–8 — Tasks 20.3.12–20.3.13 added 2026-09-27, remediation #28):
- Task 20.3.1 (Schema + ETL): 1.5 days
- Task 20.3.2 (Ticks): 1 day
- Task 20.3.3 (OHLCV): 1 day
- Task 20.3.4 (P&L): 1 day
- Task 20.3.5 (Volume/stats): 1 day
- Task 20.3.6 (Statements/confirmations/invoicing): 1 day
- Task 20.3.7 (House finance reporting): 0.5 day
- Task 20.3.9 (TCA engine): 1.5 days
- Task 20.3.11 (ClickHouse ingestion buffering & DLQ recovery): 0.5 day
- Task 20.3.12 (Income ledger query): 0.5 day
- Task 20.3.13 (Daily account snapshots): 0.5 day
- Task 20.3.14 (Costs & charges disclosure): 1 day
- Task 20.3.15 (Depreciation notifications): 1 day
- Task 20.3.16 (Marketing-ops reporting): 0.5 day (absorbed in range)
- Testing: 0.5 day

---

## 20.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | ClickHouse tables created (ticks, trades, ohlcv, pnl, volume) |
| 2 | ETL pipeline: PostgreSQL → ClickHouse |
| 3 | Real-time ticks inserted within 1s |
| 4 | Batch aggregation for OHLCV |
| 5 | Ticks stored in ClickHouse with 90-day raw TTL; aggregates retained 5 years (spec §16.1/§16.2; supersedes prior "5-year retention" for raw ticks) |
| 6 | Tick history query works with date range |
| 7 | LZ4 compression enabled |
| 8 | OHLCV analytics projections cover all 12 persisted intervals; Phase-06 1s remains memory-only (§24 #66; supersedes prior 9/6) |
| 9 | Materialized views for real-time aggregation |
| 10 | Kline query works |
| 11 | P&L report per account per day |
| 12 | CSV and PDF export for P&L |
| 13 | Aggregation by account, instrument, day |
| 14 | Volume report per symbol/day/hour |
| 15 | Trade count per symbol/tier |
| 16 | Fill rate computed |
| 17 | Stats endpoints work |
| 18 | ClickHouse sustains 50,000 inserts/sec on tick ingest (§24 #65) |
| 19 | Analytics dashboard metrics exposed: latency, throughput, fill rate, P&L (§24 #68) |
| 20 | Daily + monthly client statements generated, downloadable, reconciled to GL (§24 #143) |
| 21 | Trade confirmations within 60s of execution, retrievable via API |
| 22 | Monthly institutional fee invoices; 5-year retention enforced |
| 23 | Daily per-currency trial balance reconciles zero-variance to GL (zero-sum invariant); finance P&L/balance-sheet exports by EOD+1h (§24 #205) |
| 24 | ERP nightly batch delivered with checksum + replay protection; finance records retained ≥ 7 years |
| 25 | Trade confirmations generated with regulatory fields; delivered via portal + email T+1 retail / real-time institutional; 7-year archival (§24 #235) |
| 26 | TCA engine: per-fill slippage vs arrival/VWAP/ECB; ClickHouse storage; REST API; quarterly RTS 28 auto-generation (§24 #246) |
| 27 | User tax calculator exposes FIFO/LIFO/HIFO/average-cost methods, fees/swaps, period, and reproducible PDF/CSV export (§24 #276) |
| 28 | ClickHouse ingestion timeout (>5s) spools ticks to local RocksDB buffer; re-ingestion deduplicates via ReplacingMergeTree without tick loss (§24 #322) |
| 29 | Income ledger queryable by type/symbol/time with GL linkage, reconciled to statements (§24 #361) |
| 30 | Daily hash-chained account snapshots with history API feeding auditor evidence (§24 #362) |
| 31 | Ex-ante cost preview matches actuals; annual ex-post statement reconciles to ledger with no-inducement declaration (§24 #374) |
| 32 | Retail leveraged-position −10% multiples notify same business day with episode dedupe (§24 #375) |
| 33 | Promo inventory reconciles to promotions table with SLA flags; consent cohorts counts-only with 100 floor (§24 #381) |
