# Phase 3 — Risk & Settlement (Go, T+1/T+2)

**Duration:** 5–8 days (supersedes prior 5–7 — Task 3.3.19 added 2026-09-27, remediation #24)
**Dependencies:** Phase 2, Phase 2.5
**Spec Reference:** §13 (Risk Management), §17 (Backoffice & Settlement)

---

## 3.1 Objectives

Implement the Go-based risk and settlement services: post-trade balance updates, position management, T+1/T+2 settlement instruction generation, fee calculation, and the risk limits enforcement layer. This phase runs after the C++ matching engine (Phase 2) and consumes trade fills via Aeron IPC.

---

## 3.2 Prerequisites

- Phase 2 complete (matching engine, IPC, WAL)
- Phase 2.5 complete (soak test passed)

---

## 3.3 Tasks

### Task 3.3.1: Post-Trade Balance Service (Go)

**Objective:** Consume trade fills from C++ core via Aeron and update balances in PostgreSQL.

**File Locations:** `services/internal/settlement/balance_service.go`

**Implementation:**
1. Aeron subscriber listens to `trades_out` channel from C++ core.
2. On trade fill: execute the **four-legged** balance mutation set — debit buyer's locked quote-currency balance, credit buyer's base-currency available balance; debit seller's locked base-currency balance, credit seller's available quote-currency balance (net of fees) — with a balanced double-entry journal entry per mutation pair (Phase-03 Task 3.3.6 owns the GL posting; remediation #35 — supersedes the prior single-leg description, under which buyers never received base currency).
3. PostgreSQL `SERIALIZABLE` isolation for all balance mutations.
4. Account mutex via Redis `SETNX account:lock:{id} EX 10`.
5. Idempotent: trade_id dedup in `processed_trades` table.
6. Batch writes: buffer 100 trades or 10ms, whichever first.

**Migration note:** Create migration `022_create_processed_trades.up.sql` with columns: `trade_id` (PK), `processed_at`, `shard_id`. This table was missing from the original Phase 1 migration list (001–020).

**Definition of Done (Acceptance Criteria):**
* [ ] Trade fills consumed from Aeron with zero loss at 50k/sec
* [ ] Balance updates atomic (SERIALIZABLE + account mutex)
* [ ] Idempotent: duplicate trade_id skipped
* [ ] Batch writes reduce PostgreSQL round-trips by 10x vs single-row

**SDD Checklist:**
- [ ] Spec checkpoint: SERIALIZABLE for balance mutations — defined first, validated against spec
- [ ] Spec checkpoint: account mutex via Redis SETNX — defined first, validated against spec
- [ ] Spec checkpoint: idempotent trade processing — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: concurrent trades same account, duplicate fill, mutex timeout

---

### Task 3.3.2: Position Management (Go)

**Objective:** Track open positions per account per instrument.

**File Locations:** `services/internal/settlement/position_service.go`

**Implementation:**
1. On trade fill: update `positions` table (quantity, entry_price, unrealized_pnl).
2. Mark price from Phase 19.5 PriceOracle (placeholder until then: last trade price).
3. Position side: LONG (net buy), SHORT (net sell), FLAT (zero).
4. Realized P&L on position close.
5. Position limit enforcement: max open positions per account.

**Definition of Done (Acceptance Criteria):**
* [ ] Positions updated correctly on each trade fill
* [ ] Unrealized P&L computed from mark price
* [ ] Realized P&L computed on position close
* [ ] Position limit enforced (reject trade if exceeded)

**SDD Checklist:**
- [ ] Spec checkpoint: position tracking with unrealized/realized P&L — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: position reversal, partial close, mark price stale

---

### Task 3.3.3: T+1/T+2 Settlement Instructions (Go)

**Objective:** Generate settlement instructions for each trade per FX settlement cycle.

**File Locations:** `services/internal/settlement/settlement_service.go`

**Implementation:**
1. On trade fill: create `settlement_instructions` row with:
   - trade_id, account_id, currency, amount, direction (PAY/RECEIVE)
   - settlement_date = trade_date + settlement_cycle (T+1 or T+2)
   - nostro_account_id (assigned per currency)
   - status = PENDING
2. Settlement cycle per instrument (from `instruments.settlement_cycle`):
   - T+1: most spot FX (EUR/USD, GBP/USD, USD/JPY, etc.)
   - T+2: some exotic pairs
   - Same-day: USD/CAD, USD/MXN
3. Scheduled job at settlement_date: send SWIFT MT202 / pacs.009 to correspondent bank.
4. On confirmation: status → SETTLED; update nostro account balance.

**Forward-reference notes:** SWIFT message sending is owned by Phase 11 (Banking Rails Integration, Task 11.3.1). Nostro account management is owned by Phase 24 (Backoffice & Settlement, Task 24.3.1). During Phase 3, generate the settlement instruction record only; actual SWIFT message dispatch and nostro balance updates are wired in Phase 11 and Phase 24. The `nostro_accounts` table (Phase 1 migration 018) must be pre-seeded with test data for Phase 3 validation.

**Definition of Done (Acceptance Criteria):**
* [ ] Settlement instruction created for every trade with correct settlement_date
* [ ] T+1: EUR/USD trade on Monday → settlement Tuesday
* [ ] Same-day: USD/CAD trade settles same business day
* [ ] SWIFT message generated with correct fields (currency, amount, counterparty, nostro)
* [ ] Nostro account balance updated on settlement confirmation

**SDD Checklist:**
- [ ] Spec checkpoint: T+1/T+2 settlement per FX standard — defined first, validated against spec
- [ ] Spec checkpoint: same-day for USD/CAD/USD/MXN — defined first, validated against spec
- [ ] Spec checkpoint: SWIFT MT202/pacs.009 message generation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: weekend rollover, holiday calendar, failed settlement, partial settlement

---

### Task 3.3.4: Fee Calculation (Go)

**Objective:** Calculate and apply trading fees per fee tier.

**File Locations:** `services/internal/settlement/fee_service.go`

**Implementation:**
1. On trade fill: compute fee = fill_qty × fill_price × fee_bps / 10000.
2. Maker vs taker fee (maker typically lower).
3. Fee tier from `accounts.fee_tier_id` → `fee_tiers` table.
4. Promo rates applied if `promo_until > now()`.
5. Fee deducted from balance immediately.

**Definition of Done (Acceptance Criteria):**
* [ ] Fee computed correctly: qty × price × bps / 10000
* [ ] Maker/taker distinction correct
* [ ] Promo rates applied while promo active
* [ ] Fee deducted from balance atomically with trade settlement

**SDD Checklist:**
- [ ] Spec checkpoint: maker/taker fee tiers — defined first, validated against spec
- [ ] Spec checkpoint: promo rate windows — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: zero fee tier, expired promo, fee > balance

---

### Task 3.3.5: Risk Limits Enforcement (Go)

**Objective:** Enforce per-account and per-symbol risk limits.

**File Locations:** `services/internal/risk/limits_service.go`

**Implementation:**
1. Load `risk_limits` table at startup; cache in Redis.
2. Per-account limits: max_order_qty, max_daily_volume, max_open_orders, daily_withdraw_limit.
3. Per-symbol limits: max notional exposure, max short exposure.
4. Daily reset at 00:00 UTC.
5. API: `GET /api/v1/account/risk-limits` returns current limits + utilization.

**Boundary note (Phase 3 vs Phase 5):** Phase 3 implements the risk limits **service logic** (loading limits, enforcing, caching). Phase 5 (Task 5.3.4) exposes the REST endpoint `GET /api/v1/account/risk-limits` via the gateway. During Phase 3, the service logic is testable via direct Go function calls; Phase 5 wires the HTTP route.

**Definition of Done (Acceptance Criteria):**
* [ ] Per-account limits enforced (order rejected if exceeded)
* [ ] Per-symbol exposure limits enforced
* [ ] Daily volume counter resets at 00:00 UTC
* [ ] API returns current limits + utilization

**SDD Checklist:**
- [ ] Spec checkpoint: per-account + per-symbol risk limits — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: limit boundary, concurrent order race, daily reset

---

### Task 3.3.6: Double-Entry General Ledger Posting Service (Go)

**Objective:** Implement formal double-entry bookkeeping (`SUM(debits) == SUM(credits)`) for all financial mutations per spec §5.21 (added 2026-09-16). Enhanced 2026-09-27 (ledger/wallet/balance specification) with `ledger_entries` and `journal_sums` tables (§5.3), PostgreSQL trigger enforcement, and `SERIALIZABLE` + `FOR UPDATE` + Redis distributed lock protocol.

**File Locations:** `services/internal/settlement/ledger_service.go`

**Implementation:**
1. Every balance mutation (trade fill, fee deduction, deposit, withdrawal, EOD rollover, liquidation) generates a balanced `journal_entries` record with child `ledger_lines`.
2. `chart_of_accounts` pre-seeded with standard accounts:
   - `1010_NOSTRO_{CURRENCY}` (Asset)
   - `2010_CUSTOMER_LIABILITY_{CURRENCY}` (Liability)
   - `4010_TRADING_FEE_REVENUE_{CURRENCY}` (Revenue)
   - `5010_LIQUIDATION_PENALTY_{CURRENCY}` (Revenue/Expense)
3. Invariant: `SUM(debit_amount) == SUM(credit_amount)` per currency enforced within same PostgreSQL `SERIALIZABLE` transaction.
4. **Migration note:** Create migration `036_create_general_ledger.up.sql` per spec §5.21.
5. **Ledger entries (added 2026-09-27):** Every financial event also inserts immutable `ledger_entries` records (§5.3) with `direction` (DEBIT/CREDIT), `amount` DECIMAL(28,8), and `running_balance`. The `journal_sums` table is updated atomically within the same transaction.
6. **Locking protocol (added 2026-09-27):** All balance mutations use `BEGIN ISOLATION LEVEL SERIALIZABLE` → `SELECT ... FOR UPDATE` on wallet rows → validate sufficient funds → update `balances` cache → insert `ledger_entries` → update `journal_sums` → `COMMIT`. Redis `SETNX account:lock:{id} EX 10` acquired before transaction for cross-service mutations.
7. **Precision standard (added 2026-09-27):** All monetary values use `decimal.Decimal` (shopspring/decimal) in Go. Native float64 is forbidden in financial code paths.
8. **Sub-Account Transfers & BalanceChanged Event Dispatch (added 2026-09-27, remediation #38):**
   - All internal sub-account balance movements MUST execute through `DoubleEntryLedgerService::postJournal()`. Direct raw SQL mutations on `balances` or skipping `ledger_entries` are strictly prohibited.
   - Upon successful commit of a transfer journal, the ledger service MUST emit a `BalanceChanged` event to NATS JetStream topic `account.balance.changed.{account_id}` for both debtor and creditor accounts, triggering immediate WebSocket push notifications to connected frontend clients.

**Definition of Done (Acceptance Criteria):**
* [ ] Balanced journal entries created for every trade fill and fee
* [ ] Invariant `SUM(debits) == SUM(credits)` enforced
* [ ] `chart_of_accounts` pre-seeded for all supported currencies
* [ ] Atomic execution with balance update under SERIALIZABLE transaction
* [ ] `ledger_entries` inserted for every financial event (append-only)
* [ ] `journal_sums.net_balance == balances.total` after every transaction
* [ ] Redis distributed lock acquired before balance mutation
* [ ] No native float64 math in ledger service code

**SDD Checklist:**
- [ ] Spec checkpoint: double-entry ledger invariant SUM(debit)==SUM(credit) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.7: Automated EOD Spot Rollover Service (Go)

**Objective:** Implement automated 17:00 ET spot rollover (Tom-Next / T/N) applying financing swap points per spec §17.4 (added 2026-09-16). Timezone wording corrected 2026-09-27, remediation #35: the canonical trigger is 17:00 ET = 21:00 UTC EDT / 22:00 UTC EST (supersedes the prior "17:00 EST / 21:00 UTC" pairing, which matched neither offset).

**File Locations:** `services/internal/settlement/rollover_service.go`

**Implementation:**
1. Scheduled cron job fires daily at 17:00 ET (21:00 UTC EDT / 22:00 UTC EST New York close — remediation #35, supersedes the prior fixed "21:00 UTC" schedule, which was wrong in EST).
2. Queries all open spot margin positions in `positions`.
3. Consumes swap points from the Task 3.3.11 swap rate engine (single owner of swap-point math and `swap_rates` storage — boundary pinned 2026-09-27, remediation #35; the prior "computes Tom-Next swap points based on interest rate differentials" duplicated Task 3.3.11's mechanism and created double-charge risk if both ran).
4. Shifts position value date forward to the next business day.
5. Posts financing debit/credit adjustments to account balances and writes double-entry journal entries (via Task 3.3.6 GL service).
6. **Wednesday Triple Rollover:** Open positions rolled from Wednesday to Thursday cover 3 calendar days over the weekend, incurring 3× swap points (multiplier supplied by Task 3.3.11).

**Definition of Done (Acceptance Criteria):**
* [ ] Daily rollover cron fires at 17:00 ET (21:00 UTC EDT / 22:00 UTC EST)
* [ ] Tom-Next swap points computed correctly from rate differential
* [ ] Wednesday rollover applies 3× financing swap points
* [ ] Balance adjustment and GL journal entries posted atomically

**SDD Checklist:**
- [ ] Spec checkpoint: automated EOD spot rollover with Wednesday triple roll — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.8: Multi-Currency Holiday Calendar Engine (Go)

**Objective:** Implement ISDA Modified Following Business Day convention across multiple central bank calendars per spec §17.5 (added 2026-09-16).

**File Locations:** `services/internal/settlement/calendar_service.go`

**Implementation:**
1. Ingests official banking calendars for all supported currencies (TARGET2, Federal Reserve, Bank of England, Bank of Japan, etc.).
2. Evaluates settlement dates: trade date + cycle (T+1/T+2) adjusted forward if target date falls on a bank holiday in base currency, quote currency, or settlement currency.
3. Split holiday handling: if T+1 is a holiday for one currency but not the other, rolls forward to next mutual business day.

**Definition of Done (Acceptance Criteria):**
* [ ] Major central bank holiday calendars ingested
* [ ] ISDA Modified Following Business Day convention enforced
* [ ] Split currency holidays correctly shift settlement dates

**SDD Checklist:**
- [ ] Spec checkpoint: multi-currency holiday calendar engine — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.9: Multi-Currency P&L Conversion & Multi-Currency Account Accounting

**Objective:** Implement multi-currency position P&L conversion from quote currency to account base currency and realized P&L ledger settlement per spec §13.1 / §16.4 / §24 #180.

**File Locations:** `services/internal/position/pnl_converter.go`, `services/internal/balance/settlement_service.go`

**Implementation:**
1. **Quote Currency Computation:** Maintain raw position P&L in the quote currency of the instrument traded (e.g. EUR/GBP yields GBP P&L).
2. **Oracle Mid-Rate Conversion:** For equity and margin utilization, convert unrealized quote P&L to account `base_currency` using current PriceOracle mark rates.
3. **Realized Settlement & Sweeps:** Upon position close, book realized P&L into quote currency cash balance, or optionally execute an automated currency sweep to base currency based on account configuration.
4. **General Ledger Integration:** Multi-currency realized P&L debits/credits balance lines and posts to GL currency translation and realized trading gain/loss accounts with zero-sum balancing.

**Definition of Done (Acceptance Criteria):**
* [ ] Unrealized P&L in quote currency converts accurately to account base currency using mark oracle mid-rate
* [ ] Position close settles realized P&L in appropriate ledger currency line with balanced GL entries
* [ ] Reporting currency conversion matches EOD official reference rates

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: multi-currency P&L base conversion (§13.1, §24 #180) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: indirect FX rate cross-conversion (e.g. AUD/NZD to USD), inverted quote currency rates, zero mark price exception

---

### Task 3.3.10: Aeron-to-NATS Bridge Service
Added 2026-09-17 (gap analysis remediation #6).

Implement the Bridge Service that fans out C++ engine events to the NATS JetStream event backbone (spec §2.3.1, §24 #210):
1. One Bridge instance per shard, NUMA-colocated with the engine process.
2. Aeron `FragmentHandler` consumes engine events (trade fills, order acks, book updates) with zero-copy semantics.
3. Republishes each event to the appropriate NATS JetStream stream (`trades.{shard_id}.{symbol}`, `settlements.{shard_id}.{symbol}`, etc.).
4. Bounded in-memory buffer (100k events) absorbs temporary NATS unavailability; replays on reconnect.
5. Latency budget: Bridge adds < 100µs to cold-path delivery (measured via OpenTelemetry span).
6. Metrics: `bridge_events_published_total`, `bridge_buffer_depth`, `bridge_nats_reconnect_total` Prometheus counters.
7. Health check: Bridge publishes heartbeat to `bridge.health.{shard_id}` every 5s; absence triggers P1 alert.

---

### Task 3.3.11: Overnight Swap Rate Engine (Go)

**Objective:** Implement the swap/rollover rate engine that computes and posts overnight financing charges/credits on positions held past the daily rollover cutoff.

**File Locations:** `services/internal/settlement/swap_engine.go`, `services/internal/settlement/swap_rates.go`

**Implementation:**
1. Swap rate data feed: ingest daily swap point feeds from Refinitiv/Bloomberg via scheduled pull (17:00 ET daily). Store in `swap_rates` table: `instrument_id`, `long_swap_points`, `short_swap_points`, `effective_date`.
2. Rollover cutoff: 17:00 ET (21:00 UTC EDT summer / 22:00 UTC EST winter — DST mapping corrected 2026-09-27, remediation #35; the prior "22:00 UTC summer / 21:00 UTC winter" was inverted). Configurable per session.
3. Rollover calculation: for each open position at cutoff, compute `swap_charge = position_qty × swap_points × lot_size`. Long positions use `long_swap_points`, short use `short_swap_points`.
4. Triple-swap Wednesday: Wednesday rollover applies 3× swap to cover Saturday+Sunday (non-trading days). Certain pairs with split holidays may have different triple-swap days.
5. Post swap charge/credit as double-entry GL journal entry (Task 3.3.6): debit/credit client account, contra entry to swap revenue account.
6. Holiday-aware: if rollover date falls on a holiday (from Task 3.3.8 calendar), shift to next business day per Modified Following convention. Holiday weekends may trigger 4× or 5× swaps.
7. Expose swap rates via REST API: `GET /api/v1/instruments/{symbol}/swap-rates` (Phase-05 registers route).
8. Push swap charge notifications to client via NATS → WS private channel.

**Definition of Done (Acceptance Criteria):**
* [ ] Swap rates ingested from external feed and stored with effective dates
* [ ] Rollover fires at 17:00 ET with correct timezone handling (DST)
* [ ] Swap charge computed correctly for long and short positions
* [ ] Triple-swap Wednesday applies 3× charges/credits
* [ ] GL journal entry posted for every swap event
* [ ] Holiday calendar integration: multi-day swaps on holiday weekends

**SDD Checklist:**
- [ ] Spec checkpoint: overnight swap/rollover — defined first, validated against spec
- [ ] Spec checkpoint: triple-swap Wednesday — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: DST transition, holiday-on-Thursday (4× swap), zero swap rate (no charge), position opened after cutoff same day

---

### Task 3.3.12: Pip Value Calculator Service (Go)

**Objective:** Implement a standardized pip value calculator that converts pip values to the account's base currency for use by margin engine, P&L calculator, and Trader UI.

**File Locations:** `services/internal/settlement/pip_calculator.go`

**Implementation:**
1. Pip value formula: `pip_value = lot_size × pip_size`. For a standard lot (100,000 units) on EUR/USD: `100000 × 0.0001 = $10/pip`.
2. For pairs where account currency ≠ quote currency: convert using live mid-rate from PriceOracle (Phase-19.5). Example: USD account trading EUR/GBP → pip in GBP → convert via GBP/USD mid-rate.
3. For pairs where account currency = base currency (e.g., EUR account trading EUR/USD): `pip_value = lot_size × pip_size / current_price`.
4. JPY pairs: pip_size = 0.01 (not 0.0001). Derive from instrument config `decimal_places`.
5. Cache pip values with 1-second TTL in Redis. Invalidate on price change > 0.1%.
6. Expose via internal gRPC for margin engine and via REST for Trader UI: `GET /api/v1/instruments/{symbol}/pip-value?lots=1&account_currency=USD`.

**Definition of Done (Acceptance Criteria):**
* [ ] Pip value correct for direct pairs (e.g., EUR/USD from USD account = $10/standard lot)
* [ ] Pip value correct for indirect pairs (e.g., USD/JPY from USD account)
* [ ] Pip value correct for cross pairs (e.g., EUR/GBP from USD account) with live FX conversion
* [ ] JPY pair pip size (0.01) handled correctly
* [ ] Cached with 1s TTL, invalidated on significant price moves

**SDD Checklist:**
- [ ] Spec checkpoint: pip value calculation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: exotic pairs with non-standard pip size, stale price oracle, circular conversion (account currency = neither base nor quote)

---

### Task 3.3.13: Commission Engine & Dual Fee Model (Go)

**Objective:** Support both spread-markup and raw-spread-plus-commission fee models, configurable per account type.

**File Locations:** `services/internal/settlement/commission_engine.go`

**Implementation:**
1. Account fee model enum: `{SPREAD_MARKUP, RAW_SPREAD_COMMISSION}`.
2. SPREAD_MARKUP mode: LP pricing widened by configured markup before distribution. Fee implicit in spread — no separate commission charged. Use existing Task 3.3.4 fee logic.
3. RAW_SPREAD_COMMISSION mode: pass LP pricing through without markup. Charge explicit commission per lot or per million notional on each execution.
4. Commission tiers table: `commission_tiers` with columns `tier_id`, `min_monthly_volume`, `rate_per_lot`, `rate_per_million`. Higher volume → lower commission.
5. Monthly volume tracker: aggregate per-account monthly notional volume for tier computation. Reset on calendar month boundary.
6. Commission posted as separate GL journal entry (debit client, credit commission revenue account). Distinct from spread revenue.
7. Execution report includes both `commission` and `effective_spread` fields for transparency (MiFID II cost disclosure).

**Definition of Done (Acceptance Criteria):**
* [ ] SPREAD_MARKUP accounts charge fees implicitly via widened spread
* [ ] RAW_SPREAD_COMMISSION accounts charge explicit commission per lot
* [ ] Commission tiers applied based on monthly volume
* [ ] GL journal entries separate spread revenue from commission revenue
* [ ] Execution reports include commission and effective_spread fields

**SDD Checklist:**
- [ ] Spec checkpoint: dual fee model — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: account type switch mid-month, zero commission tier, volume tier boundary during batch of trades

---

### Task 3.3.14: Multi-Asset Collateral Auto-Exchange Deficit Settlement & GL Posting

**Objective:** Implement auto-exchange deficit settlement engine clearing negative single-currency balances from excess collateral currencies per spec §13.6 and §24 #284.

**File Locations:** `services/internal/settlement/auto_exchange.go`

**Implementation:**
1. Ingests auto-exchange trigger instructions from Phase-19 Task 19.3.8 (collateral concentration-limit monitor; §13.6b). Amended 2026-09-25: the reference previously pointed at `Task 19.3.22`, which does not exist — Phase-19 ends at 19.3.20, and concentration monitoring is owned by 19.3.8.
2. Identifies target collateral currency with highest excess free equity after haircuts.
3. Computes required conversion amount using live Index Price + 0.1% buffer.
4. Executes internal spot balance transfer debiting excess collateral and crediting deficit currency.
5. Posts double-entry GL journal entry balancing multi-currency clearing accounts (`2100-CLIENT-COLLATERAL` and `1200-MULTI-CURRENCY-CLEARING`).

**Definition of Done (Acceptance Criteria):**
* [ ] Converts single-currency deficit at Index Price + 0.1% buffer
* [ ] Multi-currency GL journal entries balanced
* [ ] Deficit cleared without affecting external banking rails

**SDD Checklist:**
- [ ] Spec checkpoint: multi-asset auto-exchange deficit settlement with GL journal posting (§13.6, §24 #284)
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.15: Automated FX Carry Trade Swap Yield Tracking & Bot Settlement

**Objective:** Implement daily swap point accrual tracking and yield distribution for automated carry trade strategy bots per spec §15.3 and §24 #288.

**File Locations:** `services/internal/settlement/carry_trade_settlement.go`

**Implementation:**
1. Tracks open carry trade bot allocations (Phase-16 Task 16.3.22).
2. During daily 17:00 NY rollover (triple-swap Wednesday), calculates net earned swap points across hedged legs.
3. Automatically posts daily swap yield distributions to the bot's sub-account balance.
4. Records performance metrics and cumulative yield for bot reporting.

**Definition of Done (Acceptance Criteria):**
* [ ] Net positive swap points credited during daily 17:00 NY rollover
* [ ] Triple-swap Wednesday properly calculated
* [ ] Carry yield distributions posted to GL ledger

**SDD Checklist:**
- [ ] Spec checkpoint: carry trade swap yield tracking and daily rollover settlement (§15.3, §24 #288)
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.16: Formalized VIP 0–9 Tier Schedule & Daily Recalculation Engine

**Objective:** Implement VIP 0–9 tier calculation engine running daily at 00:00 UTC per spec §8.5 and §24 #290.

**File Locations:** `services/internal/settlement/vip_engine.go`, `migrations/086_vip_tiers.up.sql`

**Implementation:**
1. Cron job triggers daily at 00:00 UTC.
2. Computes 30-day trailing notional trading volume (USD equivalent) and 30-day average equity balance for all active accounts.
3. Evaluates qualification matrix: accounts achieving either volume OR balance threshold advance to corresponding VIP tier (VIP 0 to VIP 9).
4. Persists tier assignment in `accounts.vip_tier` and appends audit row to `account_vip_history` (migration 086).
5. Synchronizes active tier and effective fee schedule to Redis cache `vip_tier:{account_id}` for zero-latency gateway access.

**Definition of Done (Acceptance Criteria):**
* [ ] Daily 00:00 UTC cron recalculates 30-day volume and balance
* [ ] VIP 0–9 tiers assigned according to transparent matrix
* [ ] Tier history recorded in database and cached in Redis

**SDD Checklist:**
- [ ] Spec checkpoint: VIP 0-9 tier calculation engine with daily 00:00 UTC volume/equity aggregation (migration 086, §8.5, §24 #290)
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.17: Negative Maker Fee (Rebate) General Ledger Accounting

**Objective:** Implement negative maker fee accounting crediting cash rebates directly to high-tier liquidity providers per spec §8.5 and §24 #291.

**File Locations:** `services/internal/settlement/commission_engine.go` (extend)

**Implementation:**
1. Supports negative commission rates (e.g. -0.005%) for VIP 4+ accounts.
2. On maker execution fill, calculates rebate amount as `abs(rate) × notional`.
3. Posts GL transaction: credits client cash balance (`2100-CLIENT-FUNDS`) and debits exchange liquidity incentive expense account (`5100-LIQUIDITY-REBATES`).
4. Surfaces negative fee clearly in execution reports and monthly client statements.

**Definition of Done (Acceptance Criteria):**
* [ ] Negative maker rates credit client balance upon trade fill
* [ ] Exchange GL debits liquidity rebate expense account
* [ ] Transparent fee disclosure on execution reports

**SDD Checklist:**
- [ ] Spec checkpoint: negative maker fee rebate GL accounting with liquidity expense debit (§8.5, §24 #291)
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.18: Balance Invariant Violations & Settlement Error Compensation

**Objective:** Implement strict fail-closed balance validation, database-level zero-sum ledger constraints, and automated settlement error compensation routines per spec §5.40, §13.11, §17.12, and §24 #301.

**Implementation:**
1. **Ledger Imbalance Detection:** In the double-entry balance updater, verify `SUM(debits) == SUM(credits)` per transaction before committing. If imbalance is detected, abort transaction immediately with `LEDGER_IMBALANCE_ABORT` and raise P0 incident.
2. **PostgreSQL Serialization Retry with Jitter:** Handle SQLSTATE `40001` (serialization_failure) and `40P01` (deadlock_detected) with exponential backoff + jitter (5ms, 15ms, 45ms; max 3 retries). Exhaustion returns `TRANSACTION_CONFLICT_RETRY_EXHAUSTED` (HTTP 503).
3. **Settlement Compensation Workflow:** When downstream banking or settlement rails reject a confirmed settlement instruction, execute compensating journal entries to re-credit client balance, record failed wire fee contra-entry, and alert operations.

**Definition of Done (Acceptance Criteria):**
* [ ] Zero-sum ledger invariant enforced on every balance mutation; imbalances trip fatal alert
* [ ] Serialization conflicts retry up to 3 times before returning HTTP 503
* [ ] Settlement rail rejections trigger automated compensating journal entries

**SDD Checklist:**
- [ ] Spec checkpoint: Balance invariant verification, serialization retries, and settlement compensation fail closed (§24 #301) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.19: Full Chart of Accounts, Swap Markup & Non-Trading Fee Schedule

**Objective:** Replace the 3-account GL seed and markup-free swap engine with a production finance surface, per spec §5.21a and §24 #337. Added 2026-09-27 (production-maturity remediation #24).

**File Locations:** `services/internal/ledger/chart.go`, `services/internal/ledger/swap_accrual.go`, `migrations/088_gl_chart_of_accounts.up.sql`

**Implementation:**
1. **Full chart of accounts (migration 088):** numbered CoA with per-currency sub-accounts — client liabilities, nostro clearing, suspense/clearing-transit, swap/rollover revenue, commission revenue, funding-fee revenue, liquidity-rebate expense, insurance-fund liability, house equity/retained earnings. Client-vs-house segregation is structural in the numbering, not a query filter. Seeds the 4-account Task 3.3.6 set as a subset (supersedes the "3 examples" seed).
2. **Swap economics:** Tom-Next accrual applies a configurable admin markup spread over interbank points (per instrument, basis points, dual-controlled); day-count follows the Phase-22 per-currency convention (ACT/360 vs ACT/365, Task 22.3.1 — no separate accrual convention); negative policy rates credit/debit symmetrically with the sign preserved in the journal narrative; swap-free (Islamic) accounts accrue zero with the foregone amount reported, never silently forgiven.
3. **Non-trading fees:** inactivity/dormancy fee table (days-dormant × tier), trading conversion-spread default disclosed per pair, financing-spread disclosure line, and VAT-ability flag per fee line consumed by Phase-20 invoicing (Task 20.3.6).
4. **Tax-tool scope fix:** the Phase-20 tax tool's "dividends/adjustments" line is rescoped to FX-only income (swap/rollover financing) — no dividend feed exists in fiat spot FX (spec §1; `CORPORATE_ACTION_SCHEDULED` stays reserved, never emitted).

**Definition of Done (Acceptance Criteria):**
* [ ] Every ledger posting resolves to a seeded CoA account; unknown account aborts fail-closed
* [ ] Swap journal carries interbank points + markup as separate lines; negative rates and swap-free handled
* [ ] Non-trading fees accrue per schedule with VAT flags; invoicing consumes them without new logic

**SDD Checklist:**
- [ ] Spec checkpoint: full chart of accounts, swap markup with per-currency day-count, negative-rate and swap-free rules, non-trading fee schedule with VAT flags (§24 #337) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.20: Dust-Balance Conversion to Base Currency

**Objective:** Give sub-min-notional stranded balances a one-way path into the account base currency, per spec §5.21b and §24 #364. Added 2026-09-27 (Binance-parity remediation #28).

**File Locations:** `services/internal/ledger/dust_convert.go`

**Implementation:**
1. Eligibility: balance < `min_notional` for its pair, no resting orders locking it, account in good standing. Conversion debits the dust currency and credits base currency at mark mid-rate minus the standard conversion spread (Task 3.3.13 disclosure).
2. Each conversion posts balanced GL lines (dust clearing → conversion revenue/spread) and is idempotent per (account, currency, day).
3. Rate-limited to one sweep per currency per day per account; swap-free accounts included (conversion is not financing).

**Definition of Done (Acceptance Criteria):**
* [ ] Eligible dust converts at disclosed spread with balanced GL lines
* [ ] Ineligible balances (locked, above threshold) rejected with existing codes

**SDD Checklist:**
- [ ] Spec checkpoint: dust eligibility, disclosed-spread conversion and balanced GL posting (§24 #364) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.21: Cent-Denominated Sub-Unit Ledger Accounting

**Objective:** Post and read balances in minor units for cent-denominated product profiles while preserving the zero-sum invariant, per spec §5.41 and §24 #370. Added 2026-09-27 (FXTM-parity remediation #29).

**File Locations:** `services/internal/ledger/subunit.go`, `migrations/096_cent_subunit_ledger.up.sql`

**Implementation:**
1. Sub-unit rule (migration 096 — documents the convention; no new tables): for accounts whose product profile (Phase-14 Task 14.3.13) carries `subunit_divisor = 100`, `balances` and `ledger_lines` amounts are stored in minor units (cents); the per-currency `SUM(debits) == SUM(credits)` invariant (Task 3.3.18) is enforced in minor units. Standard profiles continue in major units; the two never mix within an account.
2. Profile switching STANDARD↔CENT is permitted only at zero balances in every currency (else `INVALID_REQUEST`); dust-convert (Task 3.3.20) is the pre-switch path for stranded minors.
3. Single display helper divides by the account's profile divisor at every read boundary (statements Task 20.3.6, snapshots Task 20.3.13, tax tool Task 20.3.10, margin equity Task 19.3.8) — no per-consumer conversion logic; the CoA (Task 3.3.19) is unchanged, minor-unit postings resolve to the same accounts.

**Definition of Done (Acceptance Criteria):**
* [ ] Cent balances post/read ×100 consistently with the invariant holding in minor units
* [ ] Profile switch with non-zero balances rejected; readers share one divisor helper

**SDD Checklist:**
- [ ] Spec checkpoint: minor-unit posting, zero-balance switching and single-helper reads (§24 #370) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.22: Physical Delivery vs. Rolling Spot Ledger Partitioning

**Objective:** Partition physical delivery spot trades from rolling leveraged spot (CFD) trades to prevent incorrect swap-point rollover and ensure segregation of funds destined for banking settlement rails per spec §5.2a, §17.4a, and §24 #406. Migrations 103 and 104 add `settlement_intent` to `orders` and `accounts`. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/settlement/balance_service.go`, `services/internal/settlement/rollover_service.go`, `migrations/104_accounts_settlement_intent.up.sql`

**Implementation:**
1. Migration 104 adds `accounts.settlement_intent ENUM('PHYSICAL_DELIVERY','ROLLING_MARGIN') DEFAULT 'ROLLING_MARGIN'`.
2. In `services/internal/settlement/rollover_service.go`: the 17:00 ET Tom-Next rollover query explicitly filters for `settlement_intent = 'ROLLING_MARGIN'`. Positions marked `PHYSICAL_DELIVERY` are strictly excluded from financing swap points.
3. In `services/internal/settlement/balance_service.go`: upon execution of a `PHYSICAL_DELIVERY` spot trade, the buyer's quote currency and seller's base currency are transferred out of `balances.available` and locked into `2011_PENDING_SETTLEMENT_DELIVERY_{CURRENCY}` in the general ledger until SWIFT/CLS settlement confirms.

**Definition of Done (Acceptance Criteria):**
* [ ] Positions with `PHYSICAL_DELIVERY` intent are bypassed by the daily 17:00 ET Tom-Next rollover cron
* [ ] Zero financing swap points or overnight interest charged to corporate physical delivery orders
* [ ] Settled delivery amounts posted to `2011_PENDING_SETTLEMENT_DELIVERY_{CURRENCY}` with zero-sum balancing

**SDD Checklist:**
- [ ] Spec checkpoint: physical delivery vs rolling spot ledger partitioning (§24 #406) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 3.3.23: Islamic (Swap-Free) Administration Fee Engine

**Objective:** Implement a Shariah-compliant non-interest administrative fee schedule on overnight positions held beyond a grace period for swap-free accounts per spec §5.45, §12.8a, and §24 #407. Migration 105 creates `swap_free_admin_fees`. Added 2026-09-27 (institutional audit remediation #37).

**File Locations:** `services/internal/settlement/rollover_service.go`, `services/internal/settlement/swapfree_fee_service.go`, `migrations/105_swap_free_admin_fees.up.sql`

**Implementation:**
1. Migration 105 creates `swap_free_admin_fees` table (`instrument_id`, `holding_grace_days` default 5, `daily_admin_fee_usd_per_lot` DECIMAL(10,4), `created_at`, `updated_at`).
2. During the 17:00 ET rollover job: if an account has `swapfree_status = 'VERIFIED'`, swap points are set to exactly 0.0 (no interest debited/credited).
3. The engine inspects `position.opened_at`. If position holding duration exceeds `holding_grace_days`, compute:  
   $$\text{Fee} = \text{position\_lots} \times \text{daily\_admin\_fee\_usd\_per\_lot}$$
4. Post balanced GL entries: debit customer balance and credit `4020_SWAPFREE_ADMIN_REVENUE_USD`.

**Definition of Done (Acceptance Criteria):**
* [ ] Verified swap-free accounts accrue zero interest swap points
* [ ] Positions held longer than grace period (default 5 days) assess fixed flat administrative holding fee
* [ ] Balanced journal entries created with credit to swap-free fee revenue account

**SDD Checklist:**
- [ ] Spec checkpoint: swap-free administrative holding fee engine (§24 #407) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 3.4 Deliverables

- Post-trade balance service (Aeron consumer, PostgreSQL SERIALIZABLE)
- Position management with P&L
- T+1/T+2 settlement instruction generation
- Fee calculation with promo support
- Risk limits enforcement
- Double-entry General Ledger posting service with migration 036 (Task 3.3.6)
- Automated EOD spot rollover (Tom-Next) service (Task 3.3.7)
- Multi-currency holiday calendar engine (Task 3.3.8)
- Multi-currency P&L conversion and multi-currency accounting engine (Task 3.3.9)
- Ledger imbalance zero-sum enforcement & settlement error compensation (Task 3.3.18)
- Full chart of accounts, swap markup/accrual rules & non-trading fee schedule (Task 3.3.19)
- Dust-balance conversion to base currency (Task 3.3.20)
- Cent-denominated sub-unit ledger accounting for cent product profiles (Task 3.3.21, migration 096)
- Physical delivery vs rolling spot ledger segregation (Task 3.3.22, migration 104)
- Islamic swap-free administrative holding fee engine (Task 3.3.23, migration 105)

---

## 3.5 Dependencies

- Phase 2, Phase 2.5

---

## 3.6 Duration Estimate

5–8 days (supersedes prior 5–7 — Tasks 3.3.22–3.3.23 absorbed in range, remediation #37; prior supersedes 5–7 — Task 3.3.19 GL chart & fee completeness added 2026-09-27, remediation #24):
- Task 3.3.1 (Balance service): 1 day
- Task 3.3.2 (Positions): 0.5 day
- Task 3.3.3 (Settlement instructions): 0.5 day
- Task 3.3.4 (Fees): 0.5 day
- Task 3.3.5 (Risk limits): 0.5 day
- Task 3.3.6 (Double-entry GL): 0.5 day
- Task 3.3.7 (EOD rollover): 0.5 day
- Task 3.3.8 (Holiday calendar): 0.5 day
- Task 3.3.9 (Multi-currency P&L conversion): 0.5 day
- Task 3.3.18 (Ledger invariants & compensation): 0.5 day
- Task 3.3.19 (GL chart, swap markup & non-trading fees): 1 day
- Task 3.3.20 (Dust-balance conversion): 0.5 day (absorbed in range)
- Task 3.3.21 (Cent sub-unit ledger): 0.5 day (absorbed in range)
- Task 3.3.22 (Physical delivery ledger partition): 0.5 day (absorbed in range, remediation #37)
- Task 3.3.23 (Swap-free admin fees): 0.5 day (absorbed in range, remediation #37)
- Testing: 0.5 day

---

## 3.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Trade fills consumed from Aeron with zero loss at 50k/sec |
| 2 | Balance updates atomic (SERIALIZABLE + account mutex) (§24 #9) |
| 3 | Idempotent: duplicate trade_id skipped |
| 4 | Batch writes reduce PostgreSQL round-trips by 10x |
| 5 | Positions updated correctly on each trade fill |
| 6 | Unrealized P&L computed from mark price |
| 7 | Realized P&L computed on position close |
| 8 | Position limit enforced (reject if exceeded) |
| 9 | Settlement instruction created for every trade with correct settlement_date |
| 10 | T+1: EUR/USD trade Monday → settlement Tuesday (§24 #19) |
| 11 | T+2: exotic pair trade Monday → settlement Wednesday |
| 12 | Same-day: USD/CAD trade settles same business day |
| 13 | SWIFT MT202 message generated with correct fields |
| 14 | Nostro account balance updated on settlement confirmation (§24 #20) |
| 15 | Fee computed: qty × price × bps / 10000 |
| 16 | Maker/taker fee distinction correct |
| 17 | Promo rates applied while promo_until > now() |
| 18 | Fee deducted from balance atomically with trade |
| 19 | Per-account limits enforced (max_order_qty, max_daily_volume, max_open_orders) |
| 20 | Per-symbol exposure limits enforced |
| 21 | Daily volume counter resets at 00:00 UTC |
| 22 | API returns current risk limits + utilization |
| 23 | Balanced journal entries created for every trade fill, fee, deposit, withdrawal (§24 #121) |
| 24 | Invariant SUM(debits) == SUM(credits) enforced per currency in ledger_lines |
| 25 | `chart_of_accounts` pre-seeded with customer liability, fee revenue, and nostro accounts |
| 26 | Daily EOD spot rollover cron fires at 17:00 ET (21:00 UTC EDT / 22:00 UTC EST) (§24 #122 — timezone wording aligned, remediation #35) |
| 27 | Tom-Next swap points computed from interest rate differentials |
| 28 | Wednesday rollover applies triple (3x) financing swap points |
| 29 | Major central bank holiday calendars ingested into HolidayCalendarService (§24 #123) |
| 30 | ISDA Modified Following Business Day convention shifts dates forward past holidays |
| 31 | Split currency holidays across base and quote correctly shift settlement dates |
| 32 | Multi-currency P&L: raw quote P&L converted to account base currency using mark oracle mid-rate; realized P&L settled unambiguously in base ledger (§24 #180) |
| 33 | Bridge Service consumes Aeron stream and publishes to NATS JetStream within < 1ms; bounded buffer handles NATS outage; settlement/compliance consumers receive all trade events (spec §2.3.1, §24 #210) |
| 34 | Swap rates ingested daily; rollover fires at 17:00 ET; triple-swap Wednesday applies 3× charges (§24 #221) |
| 35 | Swap GL journal entries posted: debit/credit client, contra to swap revenue account |
| 36 | Pip value correct for direct, indirect, and cross pairs with live FX conversion to account currency (§24 #222) |
| 37 | JPY pair pip size (0.01) handled; cached with 1s TTL |
| 38 | Dual fee model: SPREAD_MARKUP (implicit) and RAW_SPREAD_COMMISSION (explicit per-lot) configurable per account (§24 #223) |
| 39 | Commission tiers applied by monthly volume; GL separates spread revenue from commission revenue |
| 40 | Multi-asset auto-exchange converts negative balances at index+0.1%; GL balanced (§24 #284) |
| 41 | Carry trade swap yield tracked and settled during 17:00 NY rollover (§24 #288) |
| 42 | VIP 0–9 tier calculation runs daily at 00:00 UTC; volume and equity thresholds enforced (migration 086, §24 #290) |
| 43 | Negative maker fees credited as cash rebates with debit to liquidity expense GL (§24 #291) |
| 44 | Double-entry journal enforces SUM(debits)==SUM(credits) aborting on imbalance; SQLSTATE 40001 retries with jitter; settlement failure triggers automated compensation (§24 #301) |
| 45 | Full CoA seeded per currency with client/house segregation; swap posts interbank-plus-markup lines with per-currency day-count; negative-rate and swap-free rules enforced; non-trading fees accrue with VAT flags (§24 #337) |
| 46 | Sub-min-notional dust converts to base currency at disclosed spread with balanced GL lines; locked/above-threshold balances rejected (§24 #364) |
| 47 | Cent-profile balances post and read in minor units with the zero-sum invariant holding; profile switching requires zero balances (§24 #370) |
| 48 | Physical delivery accounts segregated from rolling spot; excluded from Tom-Next swap financing; delivery balances locked to settlement sub-ledger (§24 #406) |
| 49 | Islamic swap-free positions accrue zero interest swap points but assess flat administrative holding fees after grace period with balanced GL entries (§24 #407) |
