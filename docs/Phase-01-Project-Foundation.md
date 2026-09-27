# Phase 1 — C++ Core Foundation

**Duration:** 5–7 days
**Dependencies:** None (first phase)
**Spec Reference:** §2 (Architecture), §5 (Database Schema), §3 (Matching Engine), §4 (Redis Data Model)

---

## 1.1 Objectives

Establish the foundational infrastructure for the FOREX exchange: C++ project scaffold (CMake), Go services scaffold, PostgreSQL 16 schema migrations, Redis 7 configuration, Aeron IPC setup, binary WAL infrastructure, and the shard mapping system. No matching logic yet — this phase builds the skeleton that Phase 2 fills with the matching engine.

### Day-0 Codebase Layer Implementation Gap Inventory & Execution Roadmap (Remediation #39)

#### 1. Codebase Layer Implementation Gap Inventory (0.0% Production Code Baseline)
- **C++ Matching Engine (`core/` with `engine/` alias):** CMake configuration, flat-array `OrderBook.cpp`, NUMA pinning, `MatchingEngine.cpp`, Aeron C media driver subscriber, binary WAL writer with `O_DIRECT` block flushing, and in-process `SanctionsHook.cpp` are completely unwritten.
- **Go Microservices (`services/`):** No Go code written. `services/cmd/order-gateway/`, `services/cmd/market-data/`, `services/cmd/settlement/`, `services/cmd/risk-engine/`, `services/cmd/compliance/`, `services/cmd/fix-gateway/` (QuickFIX-Go), `services/cmd/backoffice/`, and `services/cmd/watchdogd/` need initial `go.mod` scaffolding and implementation.
- **Frontend UI (`frontend/src/`):** No React 18, TypeScript, Tailwind, or TradingView Lightweight Charts code written.
- **Databases & Cloud:** Migrations `001_`–`108_` unapplied; ClickHouse columnar schemas unapplied; Docker/Kubernetes/Terraform manifests unwritten.

#### 2. Tactical 5-Step Phase 01 Kickoff Implementation Sequence
1. **Step 1 — C++20 Core Foundation Scaffolding (Task 1.3.1):**
   - Create root `CMakeLists.txt` with C++20 standard, strict warnings (`-Wall -Wextra -Wpedantic -Werror`), and Address/UndefinedBehavior sanitizers.
   - Establish directory tree: `core/src/`, `core/include/`, `core/tests/` (symmetrically referenced as `engine/`).
   - Implement fixed-point pipette integer arithmetic primitives ($10^8$ scaling).
2. **Step 2 — Aeron Shared-Memory Driver Integration (Task 1.3.5, 1.3.10):**
   - Deploy embedded Aeron C media driver with `/dev/shm` IPC channel configuration.
   - Establish SPSC lock-free ring buffer ingress/egress queues.
3. **Step 3 — Custom Binary Write-Ahead Log (WAL) Engine (Task 1.3.6):**
   - Implement `WalWriter.cpp` utilizing `posix_memalign` 4KB block-aligned direct I/O (`O_DIRECT`).
   - Implement CRC32C packet checksumming and deterministic `TIME_TICK` event serialization.
4. **Step 4 — Database Migrations 001–021 & Go Module Initialization (Task 1.3.3, 1.3.7):**
   - Initialize root `go.mod` with Go 1.23+.
   - Implement migrations `001_create_instruments.up.sql` through `021_create_audit_merkle_roots.up.sql`.
   - Setup Docker Compose environment containing PostgreSQL 16, Redis 7, and ClickHouse for local testing.
5. **Step 5 — Phase 01.5 Automated CI/CD & Spec Validation Harness (Phase 01.5 Task 1.5.3.1, 1.5.3.2):**
   - Construct GitHub Actions pipeline validating all 543 checkpoints across 479 tasks against executable tests.
   - Enforce zero-tolerance gate: zero commits merged without 100% passing golden tests.

---

## 1.2 Prerequisites

- C++17/20 compiler (GCC 13+ or Clang 17+)
- Go 1.23+
- PostgreSQL 16
- Redis 7
- ClickHouse (latest stable)
- CMake 3.28+
- Aeron SDK (or shared-memory IPC fallback)
- Docker + Kubernetes (for Go services development)

---

## 1.3 Tasks

### Task 1.3.1: C++ Project Scaffold (CMake)

**Objective:** Create the C++ matching engine project structure with CMake build system.

**File Locations:** `core/CMakeLists.txt`, `core/src/`, `core/include/`, `core/tests/` *(Canonical convention: `core/` in the CMake tree is referenced symmetrically as `engine/` in architectural and multi-language documentation; both denote the single C++ matching core codebase, remediation #39)*

**Implementation:**
1. **CMake project structure:**
   ```
   core/ (engine/)
   ├── CMakeLists.txt
   ├── src/
   │   ├── main.cpp                 # Entry point
   │   ├── book/                     # Order book
   │   │   ├── OrderBook.cpp
   │   │   ├── PriceLevel.cpp
   │   │   └── Order.cpp
   │   ├── matching/                 # Matching engine
   │   │   └── MatchingEngine.cpp
   │   ├── risk/                     # Pre-trade risk
   │   │   └── PreTradeChecker.cpp
   │   ├── wal/                      # Write-ahead log
   │   │   ├── Wal.cpp
   │   │   └── WalEntry.cpp
   │   ├── ipc/                      # IPC (Aeron/shared-mem)
   │   │   ├── IpcChannel.cpp
   │   │   └── AeronChannel.cpp
   │   ├── recovery/                 # Crash recovery
   │   │   └── RecoveryManager.cpp
   │   ├── degradation/             # Mode manager
   │   │   └── ModeManager.cpp
   │   ├── health/                   # Health checker
   │   │   └── HealthChecker.cpp
   │   ├── election/                # Leader election
   │   │   └── LeaderElection.cpp
   │   └── utils/                    # Utilities
   │       ├── Decimal.cpp
   │       ├── MemoryPool.cpp
   │       └── TimeUtils.cpp
   ├── include/
   │   └── (mirror of src/ headers)
   ├── tests/
   │   ├── test_order_book.cpp
   │   ├── test_matching.cpp
   │   └── test_wal.cpp
   └── third_party/
       └── (Aeron, FlatBuffers, etc.)
   ```

2. **CMakeLists.txt:** C++20, `-O3 -march=native -DNDEBUG` for release, `-O0 -g -fsanitize=address` for debug. Link Aeron, pthread, rt.

3. **Decimal class:** Fixed-point arithmetic (no floating-point in financial calculations). 8 bytes integer + 8 bytes scale. All arithmetic via integer operations ($10^8$ pipette units scaling, remediation #37/#39).

4. **MemoryPool:** Pre-allocated pool of `Order` structs (1M entries per shard). `alloc()` returns from free list; `free()` returns to free list. Zero `new`/`delete` in hot path.

5. **TimeUtils:** PTP-disciplined nanosecond timestamps for price-time priority — `CLOCK_REALTIME` synchronized via PHC (`phc2sys`), with `rdtsc` retained only as a software-clock fallback until Phase-09 Task 9.3.12 wires hardware PTP (supersedes prior raw-`rdtsc` wording, remediation #35 — the determinism and FIFO claims depend on a calibrated, offset-synced stamp clock).


**Definition of Done (Acceptance Criteria):**
* [ ] CMake build succeeds with both debug and release configs
* [ ] `core/build/matching_engine` binary starts and logs "shard {N} initialized"
* [ ] Decimal class passes unit tests (add, sub, mul, div, compare with no floating-point)
* [ ] MemoryPool allocates 1M Order structs without heap allocation
* [ ] TimeUtils produces monotonic nanosecond timestamps

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: C++20 compiler with -O3 release build — defined first, validated against spec
- [ ] Spec checkpoint: Decimal fixed-point (no floating-point in financial math) — defined first, validated against spec
- [ ] Spec checkpoint: MemoryPool zero-allocation in hot path — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: pool exhaustion, decimal overflow, timestamp wraparound

---

### Task 1.3.2: Go Services Scaffold

**Objective:** Create the Go services monorepo with module structure.

**File Locations:** `services/go.mod`, `services/cmd/`, `services/internal/` *(Canonical convention: `services/cmd/` houses the Go service entrypoint binaries, referenced symmetrically as `services/<service-name>/` in Docker/Kubernetes container deployment topologies, remediation #39)*

**Implementation:**
1. **Go module structure:**
   ```
   services/
   ├── go.mod
   ├── cmd/
   │   ├── gateway/          # REST API gateway (order-gateway)
   │   ├── marketdata/       # Market data WS service (market-data)
   │   ├── fix/              # FIX gateway (fix-gateway)
   │   ├── settlement/       # Settlement service (settlement)
   │   ├── compliance/       # Compliance service (compliance)
   │   └── admin/            # Admin service (admin)
   ├── internal/
   │   ├── api/              # HTTP handlers
   │   ├── auth/             # JWT/OAuth2
   │   ├── config/           # Config loading (env + file)
   │   ├── db/               # PostgreSQL (pgx)
   │   ├── redis/            # Redis client
   │   ├── ipc/              # Aeron/shared-mem client (CGo or IPC)
   │   ├── middleware/       # Rate limit, auth, logging
   │   ├── models/           # Domain models
   │   ├── settlement/       # Settlement logic
   │   ├── compliance/       # Compliance logic
   │   └── utils/            # Utilities
   └── pkg/                  # Shared packages
       ├── decimal/          # Decimal wrapper
       ├── errors/          # Error codes
       └── logging/         # slog wrapper
   ```

2. **go.mod:** Go 1.23+, dependencies: `github.com/gorilla/websocket`, `github.com/jackc/pgx/v5`, `github.com/redis/go-redis/v9`, `github.com/quickfixgo/quickfix`, `github.com/spf13/cobra` (CLI), `github.com/spf13/viper` (config).

3. **Config:** Viper loads from `config.yaml` + env overrides. Per-service config sections.

4. **Structured logging:** `log/slog` with JSON handler. Log level configurable.

**Definition of Done (Acceptance Criteria):**
* [x] `go build ./cmd/gateway/` produces a binary
* [x] Gateway binary starts, binds port 8080, logs "gateway started"
* [x] Config loading from YAML + env overrides works
* [x] Structured logging (slog JSON) outputs to stdout

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: Go 1.23+ with structured logging (slog) — defined first, validated against spec
- [x] Spec checkpoint: single binary per service — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: missing config, invalid env, port conflict

---

### Task 1.3.3: PostgreSQL 16 Schema Migrations

**Objective:** Create all database migrations for the spec §5 schema.

**File Locations:** `services/internal/db/migrations/`

**Implementation:**
1. **Migration tool:** `golang-migrate` or `goose`; migration files numbered sequentially.
2. **Migrations** (per spec §5):
   - `001_create_instruments.up.sql` — instruments table with FX-specific columns (base_currency, quote_currency, settlement_cycle, max_leverage)
   - `002_create_users.up.sql` — users table
   - `003_create_accounts.up.sql` — accounts table with kyc_tier, parent_account_id
   - `004_create_balances.up.sql` — balances table with available/locked/total
   - `005_create_orders.up.sql` — orders table with all order types
   - `006_create_trades.up.sql` — trades table with settlement_date
   - `007_create_funding_transactions.up.sql` — funding with bank_method enum
   - `008_create_withdrawal_confirmations.up.sql`
   - `009_create_audit_hash_chain.up.sql`
   - `010_create_admin_audit_log.up.sql`
   - `011_create_risk_limits.up.sql`
   - `012_create_fee_tiers.up.sql`
   - `013_create_margin_accounts.up.sql`
   - `014_create_positions.up.sql`
   - `015_create_liquidation_auctions.up.sql`
   - `016_create_insurance_fund.up.sql`
   - `017_create_kyc_documents.up.sql`
   - `018_create_nostro_accounts.up.sql`
   - `019_create_settlement_instructions.up.sql`
   - `020_create_indexes.up.sql` — all performance indexes
   - `021_create_audit_merkle_roots.up.sql` — daily Merkle roots (Task 1.3.8)

**Forward migrations index (owned by downstream phases):**
022 `processed_trades` (Phase 3), 023 `book_snapshots` (Phase 4), 024 `order_audit` (Phase 5), 025 `api_keys` (Phase 5), 026 `api_deprecations` (Phase 5), 027 `users.password_hash` (Phase 12), 028 `notification_dead_letters` (Phase 12), 029 `surveillance_signals` (Phase 17), 030 `fix_sessions` (Phase 18), 031 `instruments.settlement_mode` (Phase 19), 032 `travel_rule_records` (Phase 21), 033 `sar_reports` (Phase 21), 034 `variation_margin` (Phase 22), 035 `swift_messages` (Phase 24), 036 `general_ledger` (`chart_of_accounts`, `journal_entries`, `ledger_lines`, Phase 3 Task 3.3.6), 037 `prime_brokerage` (`prime_brokers`, `pb_credit_limits`, `pb_giveup_trades`, Phase 18 Task 18.3.6 / Phase 19 Task 19.3.7), 038 `orders_execution_params` (`orders.post_only/reduce_only/display_qty/peg_offset/stp_mode/oco_group_id/algo_params`, Phase 16 Task 16.3.10), 039 `orders_derivative_params` (`orders.strike/option_type/exercise_style/expiry_at/barrier_type/barrier_level/value_date/near_leg_value_date/far_leg_value_date/premium/fixing_benchmark`, Phase 22 Task 22.3.9), 040 `bank_accounts` (Phase 11 Task 11.3.7), 041 `collateral_schedule` (Phase 19 Task 19.3.8), 042 `client_categorization` (`accounts.client_category` + `appropriateness_assessments`, Phase 14 Task 14.3.7), 043 `legal_agreements` (Phase 22 Task 22.3.11), 044 `settlement_netting` (`standing_settlement_instructions` + `payment_netting_batches`, Phase 24 Task 24.3.9), 045 `market_maker_program` (`mm_programs`, Phase 18 Task 18.3.10), 046 `fix_sessions_entitlement` (`account_id`, `allowed_instruments`, `cancel_on_disconnect`, `max_msgs_per_sec`, Phase 18 Task 18.3.9), 047 `risk_limits_otr` (`max_order_to_trade_ratio`, `otr_window`, Phase 13 Task 13.3.6), 048 `support_tickets` (Phase 7 Task 7.3.7), 049 `client_statements` (Phase 20 Task 20.3.6), 050 `instruments_min_notional` (Phase 2 Task 2.3.3), 051 `trade_busts` (Phase 15 Task 15.3.5), 052 `fix_certifications` (Phase 18 Task 18.3.11), 053 `credit_groups`, `credit_relationships`, `credit_reservations` (Phase 19 Task 19.3.10), 054 `regulatory_report_events`, `venue_members` (Phase 21 Tasks 21.3.14–21.3.15), 055 `average_price_groups`, `trade_allocations` (Phase 24 Task 24.3.10), 056 `client_money_accounts`, `client_money_reconciliations` (Phase 24 Task 24.3.11), 057 `bank_statements`, `statement_entries` (Phase 24 Task 24.3.12), 058 `shard_margin_reservations` (Phase 19 Task 19.3.11), 059 `regulatory_submissions` (Phase 21 Task 21.3.16), 060 `compliance_assessments` (Phase 21 Task 21.3.17), 061 `position_transfers` (Phase 19 Task 19.3.12), 062 `comms_recordings` (Phase 21 Task 21.3.20), 063 `account_closures` (Phase 14 Task 14.3.9), 064 `margin_model_runs` (Phase 19 Task 19.3.13), 065 `recovery_reports` (Phase 4 Task 4.3.5), 066 `orders_trigger_source` (Phase 16 Task 16.3.17), 067 `accounts_subaccount_limit` (Phase 5 Task 5.3.11), 068 `webauthn_credentials` (Phase 12), 069 `login_history` (Phase 12), 070 `cooling_off_periods` (Phase 14), 071 `grid_bots` (Phase 16), 072–077 (assigned per-number by remediation #14 — see the index note immediately below; supersedes the prior stale block here that lumped `072–075` as "Phase-05/Phase-10 workspace & order-engine work" and assigned 077 to Phase-05 VIP tiers), 078 `withdrawal_whitelist_settings` (Phase-11 Task 11.3.10 — attribution corrected 2026-09-27, remediation #35; supersedes the prior 11.3.7 attribution), 079 `restricted_lists` + `pre_clearance_requests` (Phase-21 Task 21.3.24), 080 `regulatory_changes` + `regulatory_change_impacts` (Phase-21 Task 21.3.25), 081 `vulnerability_disclosures` + `financial_promotions` (Phase-13.5 Task 13.5.3.8 / Phase-21 Task 21.3.26), 082 `own_funds_balances` + `contingent_capital_commitments` (Phase-24 Task 24.3.17), 083 `client_money_audits` + `segregation_certifications` (Phase-24 Task 24.3.18), 084 `settlement_penalties` (Phase-24 Task 24.3.13), 085 `option_spread_offsets` (Phase-22 Task 22.3.13), 086 `vip_tiers` (`accounts.vip_tier` + `account_vip_history`, Phase-03 Task 3.3.16). Remediation #35 completes the forward index with previously referenced-but-unregistered numbers: 087 `instrument_reference` (Phase-15 Task 15.3.11 — `instruments_reference` table per spec §5.44), 088 `gl_chart_and_fees` (Phase-03 Task 3.3.19 — chart of accounts, swap/fee columns), 089 `secrets_inventory` (Phase-09 Task 9.3.29), 090 `admin_role_bindings` (Phase-07 Tasks 7.3.11–7.3.12 — `scope` JSON gains the `env` axis per spec §5.44 item 12), 091 `fleet_and_ops_console` (Phase-09 Task 9.3.30 `environments`/`hosts`/`server_actions`/`releases` + Phase-15 Task 15.3.12 `listing_proposals` — dual-owned by design, remediation #35), 092 `recovery_audit_budgets` (Phase-04 Task 4.3.11), 093 `balance_snapshots` (Phase-20 Task 20.3.13), 094 `accounts_default_stp` (Phase-02 Task 2.3.21 — `accounts.default_stp_mode` incl. `NONE` per spec §5.4).

Remediation #14 continues the append-only migration index: 072 `execution_rules_stp_groups` (`instruments.execution_rule`, `accounts.trade_group_id`, `orders.expiry_reason/prevented_qty`, `prevented_matches`), 073 `api_key_asymmetric_types`, 074 `client_delegated_access` (`client_users`, roles/scopes, approval policies/requests), 075 `order_lists_opo`, 076 `trader_workspace_preferences` (layouts, watchlists, alerts), 077 `recurring_rebalancing_strategies`.

Remediation #29 continues the append-only migration index: 095 `account_products_swapfree` (`account_product_profiles`, `accounts.product_profile_id`, `swapfree_verifications`, `accounts.swapfree_status` — Phase-14 Tasks 14.3.13/14.3.15), 096 `cent_subunit_ledger` (minor-unit convention, no new tables — Phase-03 Task 3.3.21), 097 `copy_trading_product` (`strategy_profiles`, `copy_follows`, `high_water_marks`, `profit_share_accruals` — Phase-14 Task 14.3.14), 098 `entity_leverage_policy` (`entity_code`, category, group, caps — Phase-19 Task 19.3.24).

Remediation #30 continues the append-only migration index: 099 `product_target_markets` (profile + category target bands, negative target, distribution strategy, review due — Phase-14 Task 14.3.16), 100 `execution_policies` (policy versions, consents, annual reviews — Phase-21 Task 21.3.28).

Remediation #31 continues the append-only migration index: 101 `governance_packs` (CEO_DAILY/BOARD_QUARTERLY pack records with content hashes — Phase-07 Tasks 7.3.13–7.3.14).

Ledger/wallet/balance specification continues the append-only migration index: 102 `wallets_ledger_entries` (`wallets` with version column, `ledger_entries`, `journal_sums`, balance check trigger — Phase-03 Task 3.3.6 / Phase-04 Task 4.3.9).

Remediation #37 continues the append-only migration index: 103 `orders_discretionary_offset` (Phase-02 Task 2.3.25), 104 `accounts_settlement_intent` (Phase-03 Task 3.3.22), 105 `swap_free_admin_fees` (Phase-03 Task 3.3.23), 106 `positions_isolated_margin` (Phase-19 Task 19.3.28), 107 `banking_rail_schedules` (Phase-24 Task 24.3.20), 108 `suspense_accounts_routing` (Phase-24 Task 24.3.21).

3. **Partitioning:** `trades` table partitioned by `created_at` (daily) using `pg_partman` extension.

4. **Seed data:** Insert initial instruments (EUR/USD, GBP/USD, USD/JPY, AUD/USD, USD/CAD, USD/CHF, NZD/USD, USD/MXN) with correct settlement cycles and leverage.

**Definition of Done (Acceptance Criteria):**
* [x] All 21 migrations apply cleanly on fresh PostgreSQL 16
* [x] `trades` table has daily partition for today
* [x] Seed data: 8 currency pairs inserted with correct settlement_cycle and max_leverage
* [x] Indexes on: orders(account_id, status), trades(instrument_id, created_at), funding_transactions(account_id, status)
* [x] `SERIALIZABLE` isolation tested: concurrent balance mutations produce correct results

**SDD Checklist (MANDATORY):**
- [x] Spec checkpoint: PostgreSQL 16 with MVCC — defined first, validated against spec
- [x] Spec checkpoint: SERIALIZABLE for balance mutations — defined first, validated against spec
- [x] Spec checkpoint: pg_partman daily partitions on trades — defined first, validated against spec
- [x] Spec checkpoint: 8 seed currency pairs with correct settlement/leverage — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: concurrent migration, partial migration rollback, partition creation failure

---

### Task 1.3.4: Redis 7 Configuration

**Objective:** Configure Redis for sessions, rate limits, cache, and coordination.

**File Locations:** `services/internal/redis/client.go`, `deploy/redis/redis.conf`

**Implementation:**
1. **redis.conf:** AOF enabled (`appendonly yes`, `appendfsync everysec`), maxmemory 4GB with `noeviction` policy — coordination keys (`engine:leader:{shardId}`, `system:degradation:mode`, `circuit_breaker:{scope}:{id}`, `halt:global`) must never be evicted under memory pressure; cache keys needing eviction use a separate `volatile-lru` instance (remediation #35: `allkeys-lru` can evict the leader lease or degradation mode and break coordination), `notify-keyspace-events` for pub/sub.
2. **Go client:** `go-redis/v9` with connection pooling (20 connections), context-based timeouts.
3. **Key schema** (per spec §4):
   - Sessions: `session:{token}` HASH TTL 3600s
   - Rate limits: `rl:{ip}:{second}` STRING TTL 2s
   - Leader election: `engine:leader:{shardId}` STRING TTL 10s
   - Account locks: `account:lock:{accountId}` STRING TTL 10s
   - Degradation mode: `system:degradation:mode` STRING
   - Circuit breakers: `circuit_breaker:{scope}:{id}` HASH
4. **Health check:** `redis-cli ping` in Go service readiness probe.

**Definition of Done (Acceptance Criteria):**
* [ ] Redis 7 starts with AOF and maxmemory configured
* [ ] Go client connects, sets/gets a test key, disconnects cleanly
* [ ] Session key with TTL expires correctly
* [ ] Rate limit INCR + EXPIRE works atomically
* [ ] SETNX on `engine:leader:0` returns true for first caller, false for second

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: Redis 7 with AOF persistence — defined first, validated against spec
- [ ] Spec checkpoint: key schema matches spec §4 — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: Redis down (fail-closed), key expiry race, SETNX contention

---

### Task 1.3.5: Aeron IPC Setup

**Objective:** Set up Aeron (or shared-memory fallback) for C++ core ↔ Go services communication.

**File Locations:** `core/src/ipc/AeronChannel.cpp`, `core/src/ipc/SharedMemChannel.cpp`, `services/internal/ipc/`

**Implementation:**
1. **Aeron channel (preferred):** Aeron media driver running on the same host as C++ core. C++ publishes to `aeron:ipc?alias=orders_out`, subscribes to `aeron:ipc?alias=orders_in`. Go connects via CGo wrapper or Aeron Go bindings.
2. **Shared-memory fallback:** `shm_open("/exchange_ipc_{shard}")` + `mmap` with `MAP_SHARED`. SPSC ring buffer, 64-byte cache-line aligned slots. Producer = Go gateway, Consumer = C++ core (for inbound orders); reverse for outbound (trades, book updates).
3. **Message format:** FlatBuffers schema (`core/proto/exchange.fbs`) for order submissions, cancellations, trade fills, book snapshots.
4. **Zero-copy:** C++ reads directly from ring buffer; no memcpy into intermediate buffer.

**Definition of Done (Acceptance Criteria):**
* [ ] Aeron media driver starts; C++ and Go both connect to IPC channels
* [ ] (Fallback) Shared-memory ring buffer: Go writes 1000 messages, C++ reads all 1000 with zero loss
* [ ] FlatBuffers schema compiles for both C++ and Go
* [ ] Round-trip latency (Go → C++ → Go) measured at < 50µs end-to-end (the 10µs figure is the IPC-layer-only measurement; Task 1.3.10 and spec §2.3 use the 50µs end-to-end budget; remediation #35)

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: Aeron or shared-memory IPC (never HTTP/gRPC in hot path) — defined first, validated against spec
- [ ] Spec checkpoint: zero-copy message passing — defined first, validated against spec
- [ ] Spec checkpoint: sub-10µs IPC round-trip — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: IPC channel full (backpressure), consumer slow, producer crash

---

### Task 1.3.6: Binary WAL Infrastructure

**Objective:** Implement the custom binary WAL for crash recovery.

**File Locations:** `core/src/wal/Wal.cpp`, `core/src/wal/WalEntry.cpp`

**Implementation:**
1. **WAL file format** (per spec §3.4):
   - Header: magic (0x57414C00), version, shard_id
   - Entries: seq (u64), timestamp (u64), event_type (u8), payload_len (u32), payload (bytes), checksum (CRC32 u32)
2. **mmap + fsync:** `mmap(fd, MAP_SHARED)`. `fsync` per batch (1ms or 100 events, whichever first). `O_DIRECT` option for bypassing page cache with **aligned block-flushing**: pre-allocate 4KB-aligned write buffers via `posix_memalign`; pad each batch to 4KB block boundary (padding bytes zeroed with sentinel); track logical offset vs physical file offset independently. (Supersedes prior unqualified `O_DIRECT` — raw O_DIRECT requires block-aligned buffers and offsets per Linux ABI.)
3. **WAL writer:** Appends entries to the mmap'd region; advances tail pointer atomically.
4. **WAL reader:** Reads from head to tail for recovery; verifies CRC32 per entry.
5. **WAL rotation:** When file reaches 1GB, create new file; old file archived to S3.

**Definition of Done (Acceptance Criteria):**
* [ ] WAL file created with correct header (magic, version, shard_id)
* [ ] 1000 entries written, fsync'd, and read back with all CRC32s verified
* [ ] `O_DIRECT` mode works with 4KB-aligned buffers (no page cache, no EINVAL)
* [ ] WAL file rotates at 1GB; new file created; old file ready for S3 archive
* [ ] Crash simulation: kill process mid-write; on restart, reader detects incomplete entry and truncates

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: custom binary WAL (mmap + fsync) — defined first, validated against spec
- [ ] Spec checkpoint: CRC32 per entry for corruption detection — defined first, validated against spec
- [ ] Spec checkpoint: O_DIRECT option with aligned block-flushing (4KB posix_memalign) for page cache bypass — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: partial write (crash mid-entry), disk full, WAL corruption

---

### Task 1.3.7: Shard Mapping System

**Objective:** Create the shard mapping that assigns currency pairs to shards.

**File Locations:** `services/internal/config/sharding.go`, `core/src/utils/ShardMap.cpp`

**Implementation:**
1. **Shard config** (`config/sharding.yaml`): maps currency pair groups to shard IDs (per spec §2.2).
2. **Go:** `ShardMap` struct loads config at startup; `GetShard(symbol string) int` returns shard ID for any symbol.
3. **C++:** `ShardMap` class loads same config; `getShard(const char* symbol) -> uint16_t`.
4. **Redis cache:** Shard map cached in Redis `shard:map` HASH for all services to read.
5. **CLI command:** `exchange:cache-shard-map` writes the shard map to Redis.

**Definition of Done (Acceptance Criteria):**
* [ ] Shard map config loads correctly in both Go and C++
* [ ] `GetShard("EUR/USD")` returns 0; `GetShard("USD/JPY")` returns 1
* [ ] `exchange:cache-shard-map` writes shard map to Redis
* [ ] All services read shard map from Redis at startup

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: shard mapping by currency pair group — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: unknown symbol, config reload, Redis cache miss

---

### Task 1.3.8: Audit Hash Chain Infrastructure

**Objective:** Set up the tamper-evident audit log with SHA-256 hash chain.

**File Locations:** `services/internal/audit/HashChain.go`

**Implementation:**
1. **Hash chain:** Each audit row stores `payload_hash = SHA256(row_contents)` and `prev_hash = SHA256(prev_row's payload_hash + prev_row's prev_hash)`.
2. **Insert:** On any auditable operation, compute payload_hash, read prev_hash from previous sequence, compute new hash, insert row.
3. **Daily Merkle root:** Scheduled job at 00:10 UTC computes Merkle root over all rows for the previous day; stored in `audit_merkle_roots` table.
4. **Verification:** `exchange:verify-audit --date=2026-09-14` recomputes all hashes and compares; non-zero exit on any mismatch.

**Migration note:** Create migration `021_create_audit_merkle_roots.up.sql` with columns: `root_id`, `date`, `merkle_root`, `computed_at`. This table was missing from the original Phase 1 migration list (001–020).

**Definition of Done (Acceptance Criteria):**
* [ ] Audit row insertion computes correct payload_hash and prev_hash
* [ ] Daily Merkle root computed and stored
* [ ] `exchange:verify-audit` detects a manually modified row (non-zero exit with offending day + sequence)
* [ ] Tamper detection: modifying any field in `audit_hash_chain` causes verification failure

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: SHA-256 hash chain with prev_hash linkage — defined first, validated against spec
- [ ] Spec checkpoint: daily Merkle root computation — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: concurrent inserts, missing prev_hash, corrupted row

---

### Task 1.3.9: Redis Sentinel High-Availability Topology Configuration & Client Failover Wrapper

**Objective:** Implement Redis Sentinel high-availability configuration (1 Primary + 2 Replicas + 3 Sentinel daemons, quorum=2) and Sentinel-aware Go client failover wrapper per spec §4.5 / §24 #181.

**File Locations:** `services/internal/config/redis_sentinel.go`, `config/redis-sentinel.yaml`, `deploy/redis/sentinel.conf`

**Implementation:**
1. **Sentinel Configuration:** Configure 3 Sentinel instances monitoring primary `mymaster` with `down-after-milliseconds 2000`, `failover-timeout 10000`, `parallel-syncs 1`, and quorum=2.
2. **Persistence & Replication:** Primary/replica config with `appendonly yes`, `appendfsync everysec`, `min-replicas-to-write 1`, `min-replicas-max-lag 5`.
3. **Go Sentinel Client:** Initialize `go-redis/v9` using `NewFailoverClient` with Sentinel addresses, connection pool with active health probes, and automatic reconnect circuit breaker (<100ms reconnect upon master switch notification).
4. **C++ Integration:** C++ matching engine coordination module connects via Sentinel discovery for leader lease tracking and degradation state observation.

**Definition of Done (Acceptance Criteria):**
* [ ] 3-node Sentinel cluster monitors primary and handles automatic failover within 3s
* [ ] Go `FailoverClient` discovers new primary seamlessly on master switch without dropping active sessions
* [ ] AOF persistence and replication lag parameters enforced

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: Redis Sentinel 3-node HA topology (§4.5, §24 #181) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: network partition (split-brain guard), sentinel quorum loss, reconnect storm

---

### Task 1.3.10: Low-Latency Aeron Media Driver Configuration & Kernel Tuning

**Objective:** Configure dedicated low-latency Aeron C media driver (`aeronmd`), channel URIs, socket buffer sizes, and kernel tuning scripts per spec §2.3 / §24 #185.

**File Locations:** `core/src/ipc/AeronDriverConfig.cpp`, `scripts/tune-kernel-network.sh`, `config/aeron-low-latency.properties`

**Implementation:**
1. **Media Driver Parameters:** Configure dedicated C media driver with `aeron.term.buffer.length=134217728` (128MB), `aeron.mtu.length=1408`, `aeron.socket.so_rcvbuf=16777216` (16MB), and `aeron.socket.so_sndbuf=16777216` (16MB).
2. **Threading & Pinning:** Set `threadingMode=DEDICATED` pinning conductor, sender, and receiver threads to isolated CPU cores (`isolcpus`); configure `BusySpinIdleStrategy` for matching engine thread and `BackoffIdleStrategy` for Go consumers.
3. **Kernel Tuning Script:** `scripts/tune-kernel-network.sh` sets `net.core.rmem_max=16777216`, `net.core.wmem_max=16777216`, `net.core.busy_read=50`, `net.core.busy_poll=50`, and disables CPU frequency scaling (performance governor).
4. **Channel Setup:** Scaffolding for `aeron:ipc` (in-process/NUMA shared-memory) and `aeron:udp` multicast channels.

**Definition of Done (Acceptance Criteria):**
* [ ] Dedicated Aeron media driver boots with 128MB term buffers and 16MB socket buffers
* [ ] Benchmark validates round-trip Go ↔ C++ IPC latency < 50µs
* [ ] Kernel network tuning script sets socket buffers and CPU governor without errors

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: Aeron low-latency media driver configuration (§2.3, §24 #185) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: driver crash recovery, buffer wrap-around, unpinned thread jitter

---

### Task 1.3.11: NATS JetStream Cluster Deployment
Added 2026-09-17 (gap analysis remediation #6).

Deploy a 3-node NATS JetStream cluster in Kubernetes as the cold-path event backbone (spec §2.3.1):
1. NATS Server configuration: JetStream enabled, file-backed storage, 3-node cluster with R3 replication.
2. Stream definitions: `trades`, `settlements`, `compliance`, `analytics`, `funding`, `margin-events`, `surveillance` — work-queue retention, 7-day replay window, per-subject ordering by `{stream}.{shard_id}.{symbol}`.
3. Consumer group templates: durable, at-least-once delivery, with `AckWait=30s` and `MaxDeliver=5`.
4. Health monitoring: NATS advisory subjects exposed as Prometheus metrics; consumer lag alerts.
5. Go client library: `nats.go` with JetStream API; connection pooling and automatic reconnect.

---

### Task 1.3.12: Error Handling, Arithmetic Invariants & Safe Allocations

**Objective:** Implement baseline memory allocation bounds, checked arithmetic, and error handling primitives across C++ core foundation and Go service scaffolds per spec §2.7, §3.6, and §24 #297.

**Implementation:**
1. **Checked Arithmetic Primitives:** Implement `safe_math.hpp` using compiler intrinsics (`__builtin_add_overflow`, `__builtin_mul_overflow`) for all 64-bit and 128-bit fixed-point operations. On overflow/underflow, return `ARITHMETIC_OVERFLOW_DETECTED`.
2. **Memory Pool Bounds & Error Sentinel:** Pre-allocated `MemoryPool` tracks high-watermark usage; if pool capacity is reached, throw `OrderBookCapacityExceeded` mapped to error code `ORDER_BOOK_CAPACITY_EXCEEDED` (503) without allocating on heap.
3. **Structured Service Error Envelopes:** Standardize Go foundation error packaging in `pkg/errors` with RFC 7807 problem details helper.
4. **PTP Clock Sync Guard:** Implement startup and daemon health-check validating clock drift is within 100µs; trip `TIME_SYNC_LOSS_HALT` if synchronization fails.
5. **L0–L3 Error Severity Hierarchy Foundation:** Implement baseline error classification types and severity enums (`SeverityL0`, `SeverityL1`, `SeverityL2`, `SeverityL3`) in C++ (`error_severity.hpp`) and Go (`pkg/errors/severity.go`) establishing Strict Fail-Closed Zero-Loss Pessimism across all services (spec §2.7.2).

**SDD Checklist:**
- [ ] Spec checkpoint: C++ core checked arithmetic and bounded memory pool fail closed on violation (§24 #297) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 1.4 Deliverables

- C++ project scaffold with CMake build (debug + release)
- Go services monorepo with 6 service entry points
- 21 PostgreSQL migrations with seed data (supersedes prior "20 migrations" — migration 021 audit_merkle_roots added by Task 1.3.8)
- Redis 7 configuration with key schema and Redis Sentinel 3-node HA topology (Task 1.3.9)
- Aeron/shared-memory IPC with FlatBuffers schema and dedicated low-latency C media driver configuration (Task 1.3.10)
- Binary WAL with mmap + fsync + CRC32
- Shard mapping system (Go + C++ + Redis)
- Audit hash chain with Merkle root + verification
- Error handling primitives: checked arithmetic, MemoryPool bounds, RFC 7807 envelopes, L0–L3 severity hierarchy types (Task 1.3.12)

---

## 1.5 Dependencies

- C++17/20 compiler, Go 1.23+, PostgreSQL 16, Redis 7, ClickHouse, CMake 3.28+, Aeron SDK

---

## 1.6 Duration Estimate

5–7 days (supersedes prior breakdown — Tasks 1.3.9–1.3.12 added, absorbed in range):
- Task 1.3.1 (C++ scaffold): 1 day
- Task 1.3.2 (Go scaffold): 0.5 day
- Task 1.3.3 (PostgreSQL migrations): 1.5 days
- Task 1.3.4 (Redis config): 0.5 day
- Task 1.3.5 (Aeron IPC): 0.5 day
- Task 1.3.6 (Binary WAL): 1 day
- Task 1.3.7 (Shard mapping): 0.5 day
- Task 1.3.8 (Audit hash chain): 0.5 day
- Task 1.3.9 (Redis Sentinel HA): 0.5 day
- Task 1.3.10 (Aeron Driver Config): 0.5 day
- Task 1.3.11 (NATS JetStream Cluster): 0.5 day
- Task 1.3.12 (Error Handling & Safe Allocations): 0.5 day

---

## 1.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | C++ CMake build succeeds in debug (-O0 -g -fsanitize=address) and release (-O3 -march=native) |
| 2 | Go services build: all 6 service binaries compile (`go build ./cmd/...`) |
| 3 | All 21 PostgreSQL migrations apply cleanly on fresh PostgreSQL 16 |
| 4 | 8 seed currency pairs inserted with correct settlement_cycle and max_leverage |
| 5 | `trades` table has daily partition via pg_partman |
| 6 | Redis 7 starts with AOF + maxmemory; Go client connects and all key types work |
| 7 | Aeron or shared-memory IPC: Go → C++ round-trip < 50µs end-to-end measured (10µs = IPC-layer-only; remediation #35) |
| 8 | Binary WAL: 1000 entries written, fsync'd, read back with all CRC32s verified |
| 9 | WAL crash simulation: partial entry truncated on restart; no data corruption |
| 10 | Shard map: `GetShard("EUR/USD")=0`, `GetShard("USD/JPY")=1`, cached in Redis |
| 11 | Audit hash chain: insertion computes correct SHA-256 hashes |
| 12 | `exchange:verify-audit` detects modified row (non-zero exit) |
| 13 | Daily Merkle root computation scheduled at 00:10 UTC |
| 14 | Decimal class: no floating-point; all financial math via fixed-point integers |
| 15 | MemoryPool: 1M Order structs pre-allocated; zero heap allocation in alloc/free |
| 16 | Structured logging (slog JSON) in all Go services |
| 17 | Config loading from YAML + env overrides in all services |
| 18 | `exchange:cache-shard-map` CLI command writes shard map to Redis |
| 19 | Redis Sentinel HA: 3-node Sentinel topology executes automated failover <3s with zero session loss and instant client reconnect (§24 #181) |
| 20 | Aeron configuration: dedicated C media driver with 128MB term buffers, NUMA core pinning, and kernel socket tuning verified <50µs IPC latency (§24 #185) |
| 21 | NATS JetStream 3-node cluster healthy; all 7 streams created with R3 replication; Go consumer connects, publishes, and receives test event with at-least-once delivery (spec §2.3.1, §24 #209) |
| 22 | C++ safe math detects integer overflow, MemoryPool enforces hard capacity limits, and clock drift >100µs halts startup (§24 #297) |
