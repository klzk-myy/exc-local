# Architecture — FOREX Exchange System

**Stack:** C++17/20 (matching core) · Go 1.23+ (services) · PostgreSQL 16 · ClickHouse · Redis 7 · Aeron · React 18 + TypeScript
**Domain:** Foreign Exchange (spot, forwards, swaps, NDFs, options) — fiat currencies only, no cryptocurrency
**Spec Reference:** [`./docs/Specification - Complete Exchange System Suite.md, spec §5.3/§5.21 (ledger/wallet/balance)`](./docs/Specification%20-%20Complete%20Exchange%20System%20Suite.md) (v7.0)
**Plan Reference:** [`./AGENTS.md`](./AGENTS.md) (30 phases: 24 core + 6 buffer)
**Companion docs:** [`./CONTEXT.md`](./CONTEXT.md) (onboarding, canonical values, working rules) · [`./DESIGN.md`](./DESIGN.md) (engineering design decisions & invariants)

---

## 1. Design Principles

1. **Latency budget drives the split.** Microseconds matter only in the matching core; everything else is I/O-bound. C++ owns the hot path; Go owns everything else.
2. **The order book never leaves the process.** A flat-array, intrusive-linked-list book lives in C++ memory. Redis is NOT the book — it is cache, sessions, and coordination only.
3. **No dynamic allocation in the hot path.** Pre-allocated pools, intrusive containers, and mmap'd WAL. Deterministic memory = deterministic latency.
4. **Fail closed, not open.** On any inconsistency (WAL mismatch, oracle staleness, split-brain), the system halts rather than risking financial loss.
5. **One owner per mechanism.** Routes → Phase 5, degradation modes → Phase 2, circuit breaker → Phase 13, liquidation → Phase 19, oracle → Phase 19.5, KYC → Phase 14, surveillance → Phase 17/21, WAL archive → Phase 4, DR → Phase 9, instrument lifecycle + trading hours → Phase 15, TIF expiry → Phase 2, SanctionsHook (C++ pre-trade) → Phase 21. No duplicated logic across phases.
6. **Banking rails, not blockchain.** Funding and settlement via SWIFT, SEPA, FedNow, ACH, CHAPS, TARGET2. Nostro/vostro accounts replace crypto custody. T+1/T+2 settlement, not instant.

---

## 2. High-Level Topology

```
┌─────────────────────────────────────────────────────────────────┐
│                       CLIENTS                                    │
│  REST/WebSocket (retail) · FIX 4.4 (institutional) · Admin UI    │
└──────────────┬──────────────────────────┬────────────────────────┘
               │                          │
   ┌───────────▼───────────┐   ┌──────────▼───────────┐
   │  React 18 + TS UI     │   │  FIX Gateway (Go)    │
   │  (Phase 10)            │   │  quickfix-go (Ph 18) │
   └───────────┬───────────┘   └──────────┬───────────┘
               │                          │
┌──────────────▼──────────────────────────▼────────────────────────┐
│                    KUBERNETES (Go Services)                       │
│                                                                   │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────────────┐  │
│  │ Gateway     │  │ Market Data  │  │ Settlement               │  │
│  │ REST + JWT  │  │ WS fan-out   │  │ T+1/T+2 · SWIFT/SEPA    │  │
│  │ Rate limit  │  │ L2/L3 conf.  │  │ Nostro/vostro (Ph 3,11) │  │
│  │ (Phase 5)   │  │ (Phase 6)    │  └──────────────────────────┘  │
│  └──────┬──────┘  └──────┬───────┘  ┌──────────────────────────┐  │
│         │                │          │ Compliance               │  │
│         │  Aeron / shm   │          │ MiFID/EMIR/FinCEN (Ph21) │  │
│         │  (hot path)    │          └──────────────────────────┘  │
│         │                │          ┌──────────────────────────┐  │
│         │                │          │ Admin + RBAC (Phase 7)   │  │
│         │                │          │ Notifications (Phase 12) │  │
│         │                │          │ Analytics (Phase 20)    │  │
│         │                │          │ Risk/Liquidation (Ph 19) │  │
│         │                │          │ Oracle (Phase 19.5)     │  │
│         │                │          │ Backoffice (Phase 24)   │  │
│         │                │          └──────────────────────────┘  │
└─────────┼────────────────┼─────────────────────────────────────────┘
          │                │
┌─────────▼────────────────▼─────────────────────────────────────────┐
│                    BARE METAL (C++ Core)                           │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │  Matching Engine (one per shard)                             │  │
│  │   • Flat-array order book + intrusive linked list            │  │
│  │   • Price-time priority matching                             │  │
│  │   • In-process pre-trade risk checks                         │  │
│  │   • Binary WAL (mmap + batched fsync)                        │  │
│  │   • Lock-free SPSC queues                                     │  │
│  │   • NUMA-pinned, single matching thread                       │  │
│  │   • ModeManager (6 degradation modes)                        │  │
│  │   • Leader election via Redis SETNX                          │  │
│  │   (Phase 2)                                                   │  │
│  └──────────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────┘
          │
┌─────────▼─────────────────────────────────────────────────────────┐
│                    DATA LAYER                                      │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────────┐ │
│  │ PostgreSQL 16│  │ ClickHouse   │  │ Redis 7                  │ │
│  │ OLTP: accts, │  │ Ticks, OHLCV │  │ Sessions, rate limits,    │ │
│  │ orders,      │  │ Analytics    │  │ cache, leader election,  │ │
│  │ trades, audit│  │ 90d/5yr ret. │  │ locks, feature flags     │ │
│  │ SERIALIZABLE │  │              │  │ NEVER the order book     │ │
│  └──────────────┘  └──────────────┘  └──────────────────────────┘ │
│  ┌──────────────┐                                                  │
│  │ S3           │  WAL archive (90d → Glacier)                    │
│  └──────────────┘                                                  │
└────────────────────────────────────────────────────────────────────┘
```

---

## 3. Component Inventory

### 3.1 C++ Matching Core (bare metal)

| Component | Responsibility | Phase |
|---|---|---|
| OrderBook | Flat-array price levels + intrusive linked-list orders; price-time priority; 64-byte cache-line aligned | 2 |
| MatchingEngine | Zero-heap coordination of price-time matching, crossing evaluation, fill execution (spec §3.7) | 2 |
| SelfTradeGuard | Pre-trade evaluation of self-trade prevention policies (`CANCEL_NEWEST`, `CANCEL_OLDEST`, `DECREMENT_AND_CANCEL`) | 2 |
| IcebergManager | Visible slice replenishment & hidden remainder transitions without dynamic memory allocation | 2 |
| StopOrderTrigger | Manages trailing stop and conditional trigger queues outside resting book | 2 |
| PreTradeChecker | In-process risk: balance, exposure, leverage, instrument status incl. CANCEL_ONLY, price/execution bands, trading hours, sanctions flag | 2, 15, 21 |
| WalWriter | Binary WAL; mmap + POSIX `O_DIRECT` batched memory-aligned fsync; CRC32 | 2, 4 |
| SnapshotManager | FlatBuffers snapshot every 100k trades or 5min; stored in PostgreSQL | 4 |
| IpcPublisher | Aeron/shared-mem publication of fills, order events, L2/L3 deltas | 2, 6, 17 |
| IpcSubscriber | Aeron/shared-mem subscription of orders from Go gateway | 2, 5 |
| ModeManager | 6 degradation modes (Normal, ReadOnly, MarketDataOnly, SpotOnly, Throttled, Maintenance) | 2 |
| LeaderElection | Redis SETNX `engine:leader:{shardId}` EX 10; heartbeat 3s; split-brain fail-closed | 2 |
| HealthChecker | Auto-transitions between degradation modes based on health signals | 2, 7 |
| BasketCoordinator | Cross-shard 2PC for basket orders (Reserve → Commit/Compensate) | 2 |

**Deployment:** One process per shard, NUMA-pinned, `isolcpus`, hugepages for WAL, dedicated NIC (optional SR-IOV/DPDK). No containers in the hot path.

### 3.2 Go Services (Kubernetes)

| Service | Responsibility | Phase |
|---|---|---|
| Gateway | REST/interactive WS, JWT/OAuth2 plus HMAC/Ed25519/RSA, weighted rate introspection, account-scoped idempotency keys (`idem:{account_id}:{key}`), batch/cancel-replace/keep-priority/quote-quantity/preview → Aeron → core | 5 |
| MarketData | JSON/SBE WebSocket L2/L3, pre-materialized klines contract (zero runtime scans on trades), reference/execution rules, all-market stats/block tape, graceful drain, plus institutional SBE A/B multicast and recovery | 6, 17, 23 |
| FixGateway | FIX 4.4 + 5.0 SP2, FIX SBE, Ed25519/FIXS mTLS, client certification, graceful News drain, drop copy, Mass Quote, PB affirmation | 18 |
| Settlement | T+1/T+2 settlement, SWIFT MT103/MT202/MT900/MT910, nostro/vostro, double-entry GL posting (incl. sub-account transfers via `DoubleEntryLedgerService::postJournal()` & `BalanceChanged` event), automated EOD spot rollover (Tom-Next), holiday calendar engine | 3, 11, 24 |
| Compliance | Sanctions/AML, EMIR REFIT + CFTC lifecycle reporting, MiFID, DORA evidence, venue governance, cryptographic SHA-256 audit hash chaining (`prev_checksum`) | 21 |
| Admin | RBAC (6 roles), dual control, audit log, instrument lifecycle, kill-switch | 7, 15 |
| Notifications | Email/SMS/push/WS, retry/backoff, quiet hours, preferences, non-destructive 2FA candidate staging (`2fa:pending:{user_id}`), Passkey AMR session elevation (`two_factor_verified = true`) | 12 |
| Analytics | ClickHouse ETL, tick history, OHLCV, P&L, volume stats | 20 |
| Risk | Margin/liquidation (incl. cross-currency USD numeraire normalization & batch mark prices, worker mutex backoff retry, position transfer collateral rebalancing) plus PB NOP/DSL and mutual bilateral credit screening/reservations | 19 |
| Oracle | Refinitiv/Bloomberg BFIX/ECB feeds, mark/index price, 5s staleness gates | 19.5 |
| Backoffice | Nostro/vostro, CLS ISO 20022 adapter, PB reconciliation, allocation, client-money safeguarding | 24 |
| Funding | Banking rails (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2), deposits, withdrawals, nostro tracking, PAMM investment segregation from fiat withdrawal limits | 11 |
| Algo | TWAP/VWAP/VP/grid, trailing/peg/bracket/spread/scaled/fixing, OPO/OPOCO, delayed and recurring firm-CLOB conversion/rebalancing | 16 |

**Deployment:** Docker + Kubernetes, HPA (CPU >70%, min 2 max 10 replicas), Helm charts, blue-green deploys, `/health` + `/ready` probes.

### 3.3 Data Layer

| Store | Role | Key Constraints |
|---|---|---|
| PostgreSQL 16 | Accounts, orders, trades, balances, audit, settlement instructions | SERIALIZABLE for balance mutations; PgBouncer (100 max conns); PITR RPO ≤15s/RTO ≤5min; WAL archive to S3 |
| ClickHouse | Tick history, OHLCV, analytics, P&L reports | MergeTree, daily partitions, LZ4 compression, 90-day raw tick TTL / 5-year aggregate retention; 50k inserts/s sustained |
| Redis 7 | Sessions, rate limits, cache, leader election, locks, feature flags, pub/sub | NEVER the order book; maxmemory policy; DR RPO ≤ 5s/RTO ≤ 30s |
| S3 | WAL archive (90d → Glacier), KYC documents (encrypted), data exports | ETag verification before WAL trim |

---

## 4. Sharding

Sharded by currency pair group. Each shard = one C++ process on dedicated bare metal.

| Shard | Pairs | Rationale |
|---|---|---|
| 0 | EUR/USD, GBP/USD, USD/CHF | Major European |
| 1 | USD/JPY, AUD/USD, NZD/USD | Asia-Pacific |
| 2 | USD/CAD, USD/MXN, USD/BRL | Americas |
| 3 | EUR/GBP, EUR/JPY, EUR/CHF | Cross pairs |
| 4+ | Exotic, derivatives | Elastic |

Shards are independent — no cross-shard matching. Cross-shard basket orders use 2PC (Reserve → Commit/Compensate) coordinated by the Go gateway.

---

## 5. Request Flow (Order Lifecycle)

```
1. Client → Go Gateway (REST or FIX)
2. Gateway: auth (JWT/OAuth2), rate limit (Redis token bucket), request validation
3. Gateway → Aeron → C++ shard (hot path, sub-µs IPC)
4. C++ PreTradeChecker: balance, exposure, leverage, instrument status, price bands, trading hours, sanctions flag
5. C++ Matcher: price-time priority match
6. C++ WAL: binary append (mmap + batched fsync)
7. C++ IpcPublisher → Aeron → Go services:
   - Settlement: balance/position/fee updates (PostgreSQL SERIALIZABLE)
   - MarketData: L2/L3 deltas → WebSocket fan-out
   - Analytics: tick insert to ClickHouse
   - Notifications: fill alerts
   - Compliance: trade reporting (MiFID/EMIR)
8. Recovery: snapshot + WAL replay; S3 archive; PostgreSQL PITR fallback
```

**Latency budget:**
- C++ tick-to-trade: p99 ≤ 50µs (supersedes prior ≤ 1ms)
- REST API: p99 ≤ 5ms
- WebSocket fan-out: p99 ≤ 10ms
- Settlement (async): seconds

---

## 6. IPC Architecture

### Hot path (C++ ↔ Go)

| Transport | When | Latency |
|---|---|---|
| Aeron (preferred) | Production | Sub-µs, kernel-bypass |
| Shared-memory ring (fallback) | Dev/single-host | ~1µs, `shm_open` + `mmap`, SPSC, cache-line aligned |
| FlatBuffers / custom binary | Message format | Zero-copy, no serialization overhead |

**Never HTTP/gRPC in the hot path.**

### Cold path (Go ↔ Go)

| Transport | Use |
|---|---|
| gRPC | Inter-service RPC |
| NATS JetStream (committed backbone, spec §2.3.1) | Event streaming (settlement, compliance, analytics) — supersedes prior 'NATS or Kafka' ambiguity |
| PostgreSQL LISTEN/NOTIFY | Database events |

---

## 7. Graceful Degradation

Six modes, owned by Phase 2 ModeManager. Exact casing required.

| Mode | Trigger | Behavior | Recovery |
|---|---|---|---|
| `Normal` | Default | All systems operational | n/a |
| `ReadOnly` | Core slow (matching-loop p50 > 500µs — engine self-probe, spec §2.4) or shard unhealthy | Orders rejected (`DEGRADED_MODE`); reads/WS continue | Auto when p50 < 100µs for 60s |
| `MarketDataOnly` | PostgreSQL down or WAL corrupted | All trading blocked; market data from in-memory snapshot | PostgreSQL failover + WAL verified |
| `SpotOnly` | Derivatives engine unhealthy | Spot continues; forwards/swaps/options blocked | Derivatives shard recovered |
| `Throttled` | Capacity > 80% (queue > 250 for 30s) | Rate limits to 50%; priority queue enforces tier order | Auto when capacity < 60% for 5min |
| `Maintenance` | Planned deploy | New orders blocked; existing cancel on deploy | Admin re-enables |

**Visibility:** `X-Degradation-Mode` response header; WS `system.status` event on transitions; Prometheus + PagerDuty (P2 ReadOnly/Throttled, P1 MarketDataOnly/SpotOnly).

### 7.1 Error Handling & Fault Isolation Hierarchy (spec §2.7)

The system enforces **Strict Fail-Closed Zero-Loss Pessimism** across four severity tiers:

| Tier | Classification | Trigger Conditions | Recovery & Handling Strategy |
|---|---|---|---|
| **L0** | **Critical / Fatal Fault** | WAL CRC failure, zero-sum ledger imbalance, PTP clock skew >100µs, matching loop panic, memory corruption | Immediate core halt (`SIGTERM`), dump crash diagnostics with WAL offset, flush memory-aligned buffers to disk, failover to warm standby via WAL replay ladder, P0 pager alert. |
| **L1** | **Systemic / Infrastructure Degradation** | Redis Sentinel failover, IPC ring buffer >80% watermark, PostgreSQL replica lag >5s, Price Oracle staleness >5s, ClickHouse ingestion lag | Automated degradation mode transition (`Normal` $\rightarrow$ `ReadOnly` / `MarketDataOnly` / `SpotOnly` / `Throttled` / `Maintenance`), circuit breaker tripping, load shedding of market orders, hysteresis recovery (30s healthy telemetry required). |
| **L2** | **Transaction / State Error** | Insufficient margin, negative balance breach, counterparty credit exhaustion, STP self-match, cross-shard 2PC timeout (>10ms), CLS settlement mismatch, external sanctions outage (>30s) | Synchronous atomic rejection, zero side-effects, automatic rollback of optimistic reservations, compensation event emission, structured error envelope to client, spool failed external transactions to DLQ, route settlement mismatches to break queue, P2 alert. |
| **L3** | **Edge / Protocol Violation** | Malformed JSON/FIX/SBE, HMAC signature mismatch, replay outside 30s window, rate-limit tier breach, unauthorized IP, invalid idempotency key | Fast rejection at API/FIX gateway edge before reaching internal IPC or matching core with standardized error envelope `{"type":"error","error":"<CODE>",...}`, increment security metric, progressive IP ban escalation (HTTP 418) on repeated offenses. |

---

## 8. Circuit Breaker (Phase 13)

Five-tier, state machine `CLOSED → OPEN → HALF_OPEN → CLOSED`. Persisted in Redis `circuit_breaker:{scope}:{id}`.

| Tier | Scope | Trigger | Hold | Recovery |
|---|---|---|---|---|
| 1 | INSTRUMENT | Price move > 5% in 60s | 5min | 10/10 probes in 30s |
| 2 | ACCOUNT | 3+ rapid losses > 5% equity in 5min | 30min | Manual reset |
| 3 | VOLUME_SPIKE | 1-min volume z-score ≥ 4.0σ | 10min | 10/10 probes in 30s |
| 4 | OPTIONS_VOLATILITY | IV spike > 200% vs 30-day avg | 15min | 10/10 probes in 30s |
| 5 | MARKET_WIDE | Aggregate volatility > 20% on >2 instruments | n/a | Manual admin resume |

---

## 9. Recovery Architecture

### 9.1 C++ Core Recovery

1. Load latest snapshot (FlatBuffers, from PostgreSQL, includes `snapshot_seq`)
2. Replay WAL from `snapshot_seq + 1` (binary, mmap'd)
3. Verify `book_seq == WAL tail` — on mismatch apply the graduated recovery ladder (WAL repair → snapshot rebase → fail-closed halt with `recovery_reports` row as last resort; spec §3.5)
4. Skip already-applied sequence numbers (idempotent)
5. Support warm follower recovery (read snapshot + tail WAL)

**Gate:** Recovery < 10s, zero duplicate trades, zero missing trades.

### 9.2 WAL Archive (Phase 4)

- Archive WAL segments to S3 before trimming
- Verify S3 ETag before trim
- S3 archive index maintained
- Retention: 90 days → transition to Glacier
- S3 bucket cross-region replicated (CRR) to secondary region; archived segment replay in-region verified (spec §24 #44, Phase-9 Task 9.3.4)
- Commands: `exchange:archive-status`, `exchange:replay-from-archive`

### 9.3 PostgreSQL PITR (Phase 9, 14)

- `wal_level = replica`, `archive_mode = on`, `archive_command` to S3
- Nightly base backup
- **RPO ≤ 15s, RTO ≤ 5min** (supersedes prior ≤ 5min / ≤ 30min)
- Monthly PITR drill

### 9.4 Redis DR

- Replica promotion: RPO ≤ 5s, RTO ≤ 30s

### 9.5 Chaos Validation (Phase 4.5)

Six scenarios, each run 3× — all must pass with zero dup/miss and < 10s recovery:
1. Crash during WAL fsync
2. IPC timeout and timeout-safe requeue
3. Stale snapshot requiring full WAL replay
4. WAL trimmed before archive → PostgreSQL PITR fallback
5. PID conflict with fail-closed behavior
6. Warm recovery after leader crash

---

## 10. Risk & Liquidation (Phase 19)

### Margin Modes

| Mode | Behavior |
|---|---|
| CROSS | Entire account balance as margin; liquidation when equity < maintenance |
| ISOLATED | Only position's assigned margin at risk; liquidation when position margin < maintenance |
| PORTFOLIO | Netting across positions; margin = f(net exposure, correlation); shared margin across currencies with FX conversion (spec §13.1) |

### FX Leverage

| Regime | Major | Minor | Exotic |
|---|---|---|---|
| ESMA retail | 30:1 | 20:1 | 10:1 |
| CFTC retail | 50:1 | 20:1 | — |
| Professional | Negotiable per account | | |

- **Cross-Currency Margin USD Normalization:** Position required initial/maintenance margin in quote/base currency is converted to base USD numeraire (`required_margin_quote × rate_to_usd`) before summing into `used_margin`. Mark prices are batch-loaded from Redis in $O(N)$ linear time without per-position N+1 database queries (spec §13.1, Phase-19 Tasks 19.3.1/19.3.16, remediation #38).
- **Position Transfer Collateral Rebalancing:** Off-book position transfers (give-ups, allocations, admin adjustments) must atomically release `balances.locked` on source and calculate/lock initial margin on destination within the same `SERIALIZABLE` transaction (spec §13.9, Tasks 19.3.12/24.3.7, remediation #38).

### Liquidation Auction

- Scanner cadence: **2s**
- Auction trigger: liquidated notional > **1% of open interest**
- **CALL 5s** → FILL continuous → **EXTEND ≤ 60s total** (5s increments)
- Floors: **×0.98 / ×1.02** of liquidation price (spec §13.4); floor decays 0.5% per 5s EXTEND increment (spec §24 #34)
- **FORCE_CASH** at mark **×0.95** (liquidated) / **×1.05** (counterparty)
- LP rebate: **0.05%** paid from insurance fund
- ADL when insurance fund depleted
- **Worker Mutex Resilience:** Worker (`ConsumeLiquidationQueue`) must never return 0 (success) on mutex lock contention (`lock:liquidation:account:{account_id}`); must release back to queue with exponential backoff (`release(delay)`), clearing Redis dedup key and alerting `LIQUIDATION_WORKER_LOCK_TIMEOUT` on retry exhaustion (spec §13.15, Phase-19 Task 19.3.27, remediation #38).

---

## 11. Price Oracle (Phase 19.5)

Single PriceOracle service consumed by margin, derivatives, and auto-halt. No duplicate oracle integrations.

| Feed | Type |
|---|---|
| Refinitiv | Real-time REST/WS |
| Bloomberg BFIX | Real-time API |
| ECB | Daily reference (14:00 CET) |

- **Mark price:** median of ≥ 2 feeds
- **Index price:** volume-weighted average
- Update frequency: every 1s
- **Staleness gate:** feed > 5s stale → STALE; < 2 feeds available → fail-closed (halt trading)
- Circuit breaker cascades to consumers on oracle failure

---

## 12. Funding & Settlement (Banking Rails)

### Banking Rails

| Rail | Use | Messages |
|---|---|---|
| SWIFT | International wires | MT103 (customer), MT202 (bank), MT900 (debit conf), MT910 (credit conf), pacs.009 |
| SEPA | European EUR | SCT, SCT Inst, ISO 20022 pain.001 |
| FedNow | Instant USD | FedNow API |
| ACH | US batch | NACHA file or partner API (Plaid/Stripe) |
| CHAPS | UK GBP same-day | CHAPS payment |
| TARGET2 | EU EUR real-time | TARGET2 payment |

### Settlement Cycle

- **T+1/T+2** forex standard (instrument-specific in `instruments.settlement_cycle`)
- Settlement instructions generated on trade execution
- Nostro/vostro account per currency per correspondent bank
- **CLS (Continuous Linked Settlement):** Third-party ISO 20022 PvP adapter (supersedes prior MT300/MT304 instruction wording) for eligible currency pairs to eliminate Herstatt risk (Phase 24 Task 24.3.8)
- **Automated EOD Spot Rollover:** At 17:00 EST / 21:00 UTC daily, open spot margin positions roll value dates (Tom-Next / T/N) with interest rate differential financing swap points (triple rollover on Wednesday) (Phase 3 Task 3.3.7)
- **Multi-Currency Holiday Calendar:** ISDA Modified Following Business Day convention across base, quote, and settlement calendars (Phase 3 Task 3.3.8)
- SWIFT MT900/MT910 confirmations update settlement status → SETTLED
- Timeout: no confirmation within 2 business days → P2 alert

### Withdrawal Flow

- 15min confirmation window (token via email/SMS/push)
- Review tiers: `<$10K auto / $10K–$50K standard / >$50K PENDING_REVIEW + 4h`
- 30min cooldown same bank account; 24h hold for new bank accounts
- Withdrawal caps: per-account daily, hourly, exchange-wide

### Deposit Flow

- Anti-fraud tiers: `<$10K auto / $10K–$50K double-confirm / >$50K PENDING_REVIEW + 4h`
- Dual-source bank verification (2 independent confirmations)

### Internal Transfers & Idempotency Invariants

- **Sub-Account GL Posting & Event Dispatch:** Internal transfers between master and sub-accounts or between sub-accounts route through `DoubleEntryLedgerService::postJournal()`, creating balanced DEBIT/CREDIT lines in `ledger_entries` under `SERIALIZABLE` isolation; commit dispatches `BalanceChanged` event to NATS JetStream for real-time WebSocket sync (spec §5.3, Phase-05 Task 5.3.23, Phase-03 Task 3.3.6, remediation #38).
- **PAMM Sub-Ledger Segregation:** PAMM unit investments/redemptions use internal transaction types (`PAMM_INVEST`, `PAMM_REDEEM`), strictly excluded from external fiat banking daily withdrawal limits (spec §5.3, Phase-14 Task 14.3.8, Phase-11 Task 11.3.7, remediation #38).
- **Account-Scoped Idempotency:** Idempotency keys are namespaced `idem:{account_id}:{key}` in Redis and composite `(account_id, idempotency_key)` in PostgreSQL; DB unique constraint violations (23505) replay cached payload rather than throw HTTP 500 (spec §8.8, Phase-05 Task 5.3.24, Phase-11 Task 11.3.7, remediation #38).

---

## 13. Compliance (Phase 21)

| Requirement | Coverage |
|---|---|
| Sanctions screening | OFAC SDN, EU consolidated, UN, UK HMT — on deposit, withdrawal, trade, registration; fail-closed on timeout; SanctionsHook in C++ PreTradeChecker; daily list updates |
| Travel rule (FATF) | Transfers ≥ $1,000: originator + beneficiary info in SWIFT MT103 fields 50K/59 |
| SAR | Suspicious Activity Report generation; Compliance Officer review + dual-control filing (§24 #108) |
| MiFID II | Transaction reporting (RTS 22); best execution quality |
| EMIR | Derivative trade reporting (UTI/USI, collateral) to DTCC/REGIS-TR |
| Dodd-Frank | US swap reporting to SDR, position limits, large trader reporting |
| FinCEN | MSB registration (Form 107); CDD/EDD; CTR for cash > $10K; annual AML training |
| GDPR | Right to access, erasure, portability; per-purpose consent management (§24 #103); financial record retention exceptions |
| Geo-block | Restricted jurisdictions blocked via IP geo-detection |
| Market abuse | Phase 17 generates signals (spoofing, layering, wash, marking the close, momentum ignition, front-running, insider dealing); Phase 21 enforces (warn, throttle, restrict, suspend) |
| MiFID II RTS 25 | Clock synchronization: PTP IEEE 1588v2 hardware sync within 100µs of UTC (Phase 9 Task 9.3.12) |

---

## 14. Security

| Layer | Control |
|---|---|
| Transport | TLS 1.3 everywhere (REST, WS, FIX, PostgreSQL, Redis) |
| Auth | JWT (15min access, 7-day refresh), OAuth2 client credentials, TOTP 2FA (non-destructive candidate secret staging in `users.two_factor_pending_secret` / Redis `2fa:pending:{user_id}`), WebAuthn/Passkeys (assertion elevates `two_factor_verified = true` + JWT AMR `fido2`) |
| API keys | IP allowlists (IPv4/IPv6/CIDR), concurrent session limits |
| RBAC | 6 roles: Super Admin, Risk Manager, Compliance Officer, Finance Ops, Support Agent, Read-Only Auditor |
| Dual control | Four-eyes on sensitive ops (kill-switch, suspend/delist, settlement exceptions, replenishment) — 2 distinct approvers within 15min |
| Audit | Tamper-evident admin audit logs; immutable SWIFT message trail |
| PII | AES-256 at rest, TLS 1.3 in transit, access logging, retention per regulation |
| Pen test | Phase 13.5: 0 Critical, < 3 High findings required |

---

## 15. Observability

| Signal | Tool |
|---|---|
| Metrics | Prometheus (Go services + C++ via Aeron forwarder) |
| Dashboards | Grafana (per-shard health, queue depth, latency, degradation mode) |
| Alerting | PagerDuty P1/P2/P3; 47+ alert rules (Phase 13); runbook link in each alert |
| Tracing | OpenTelemetry distributed tracing (Jaeger/Tempo) |
| Logs | Structured JSON, centralized (ELK or Loki) |
| Probes | `/health` (liveness), `/ready` (readiness), `X-Degradation-Mode` header |

---

## 16. Deployment Topology

```
┌─────────────────────────────────────────────────────┐
│  Region A (Primary)                                  │
│  ├── Bare metal: C++ shards 0–3 (NUMA-pinned)       │
│  ├── K8s: Go services (Helm, HPA)                   │
│  ├── PostgreSQL 16 (primary + replicas)             │
│  ├── ClickHouse                                      │
│  └── Redis 7 (primary + replica)                    │
├─────────────────────────────────────────────────────┤
│  Region B (DR)                                       │
│  ├── Bare metal: C++ shards (standby)               │
│  ├── K8s: Go services (standby)                      │
│  ├── PostgreSQL 16 (async replica → promote on DR)  │
│  ├── ClickHouse (replica)                            │
│  └── Redis 7 (replica → promote on DR)              │
└─────────────────────────────────────────────────────┘
```

- C++ core: Ansible provisioning, per-shard rolling restarts, no containers
- Go services: Helm charts, blue-green deploy, rollback tested
- Feature flags: 1% → 10% → 100% canary
- Cache warming: P0 within 30s, P1 within 5s after recovery
- Load shedding: starts at queue depth > 500, lowest tiers first, `503 CAPACITY_EXCEEDED`, recovers < 250
- Post-mortems: P0/P1 within 48h
- Daemon supervision & multi-tier watchdog: 24-process inventory across 5 operational tiers supervised by systemd (`WatchdogSec=1s`), Tier 1 Hardware BMC (`/dev/watchdog`), Tier 2 matching loop watchdog thread (`WatchdogThread` <2ms L0 fail-closed trip), and Tier 3 `exchange-watchdogd` platform supervisor daemon (spec §19.13).

---

## 17. Performance Targets

| Metric | Target |
|---|---|
| C++ tick-to-trade p99 | ≤ 50µs (supersedes prior ≤ 1ms) |
| C++ tick-to-trade p999 | ≤ 5ms |
| Sustained throughput per shard | 50,000 orders/sec |
| REST API p99 | ≤ 5ms |
| WebSocket fan-out p99 | ≤ 10ms |
| Crash recovery | < 10s, zero dup/miss |
| PostgreSQL RPO / RTO | ≤ 15s / ≤ 5min |
| Redis RPO / RTO | ≤ 5s / ≤ 30s |
| Uptime | 99.99% (49min/year budget) |

**Soak gate (Phase 2.5):** 72h continuous, 50k/sec sustained, no drop below 45k/sec for > 1min, memory < 75% of 16GB, crash injections at 4h/12h/24h/48h/60h.

**Pre-production gate (Phase 8.5):** 75k/sec (1.5× target) for 4h, replica lag < 2s, N≥10,000 WS connections converge zero drops (supersedes prior N≥100), mid-run recovery < 10s, zero PagerDuty alerts.

---

## 18. Mechanism Ownership Map

Single owner per mechanism — no duplication across phases.

| Mechanism | Owner Phase |
|---|---|
| Route registry (all endpoints) | Phase 5 (Task 5.3.7) |
| Error code registry | Phase 5 (Task 5.3.21) |
| Degradation modes (6) | Phase 2 (ModeManager) |
| Five-tier circuit breaker | Phase 13 |
| Instrument lifecycle (SUSPENDED/RESTRICTED grace) | Phase 15 |
| Trading-hours enforcement (24/5) | Phase 15 (Task 15.3.4) |
| TIF expiry (GTD/DAY) | Phase 2 (Task 2.3.10) |
| Margin / ADL / insurance fund / auction / exposure | Phase 19 |
| Mark/index oracle & staleness gates | Phase 19.5 |
| KYC lifecycle + re-verification | Phase 14 |
| Market-abuse surveillance signals | Phase 17 (emits) → Phase 21 (enforces) |
| GDPR / geo-block / CTR / MiFID II / EMIR / Dodd-Frank / FinCEN / travel rule / SAR / consent | Phase 21 |
| SanctionsHook (in-process pre-trade check) | Phase 21 (Task 21.3.10) |
| Derivatives (VM, delivery, roll, IV, exercise, spreads, writer assignment) | Phase 22 |
| WAL archive / replay | Phase 4 |
| Multi-region DR / IaC / on-call / API deprecation / runbooks | Phase 9 |
| §24 acceptance traceability matrix (CI-gated) | Phase 8 |
| Banking rails (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2) | Phase 11 |
| Nostro/vostro account management & correspondent reconciliation | Phase 24 |
| Double-entry General Ledger (GL) posting & journalizing | Phase 3 (Task 3.3.6) |
| Automated EOD spot rollover (Tom-Next / T/N) & swap points | Phase 3 (Task 3.3.7) |
| Multi-currency holiday calendar engine (ISDA Modified Following) | Phase 3 (Task 3.3.8) |
| Benchmark fixing orders (London 4 PM / ECB 14:15 CET) | Phase 16 (Task 16.3.9) |
| Prime Brokerage (PB) Drop Copy & Traiana trade affirmation | Phase 18 (Task 18.3.6) |
| FIX Mass Quoting (Tag 35=i) & quote cancel (Tag 35=Z) | Phase 18 (Task 18.3.7) |
| Simple Binary Encoding (SBE) / Aeron binary gateway | Phase 18 (Task 18.3.8) |
| Pre-trade Prime Broker credit limit enforcement (NOP / DSL) | Phase 19 (Task 19.3.7) |
| Continuous Linked Settlement (CLS) third-party ISO 20022 PvP adapter | Phase 24 (Task 24.3.8) (supersedes prior MT300/MT304 instruction wording) |
| Prime Brokerage give-up reconciliation & breaks | Phase 24 (Task 24.3.7) |
| PTP IEEE 1588 hardware clock synchronization (RTS 25) | Phase 9 (Task 9.3.12) |
| Institutional A/B multicast market data + replay/snapshot recovery | Phase 6 (Task 6.3.6) |
| FIXS mTLS + participant client certification | Phase 18 (Task 18.3.11) |
| Mutual bilateral credit screening + credit-filtered liquidity | Phase 19 (Task 19.3.10) |
| EMIR REFIT/CFTC Parts 43/45 lifecycle reporting | Phase 21 (Task 21.3.14) |
| Regulated-venue membership/rule-enforcement/CCO controls | Phase 21 (Task 21.3.15) |
| DORA ICT operational resilience | Phase 9 (Task 9.3.15) |
| Bunched-order allocation + average price | Phase 24 (Task 24.3.10) |
| Client-money safeguarding + daily reconciliation | Phase 24 (Task 24.3.11) |
| Redis Sentinel HA topology & failover ops | Phase 1 (Task 1.3.9), Phase 9 (Task 9.3.20) |
| Aeron C media driver tuning & thread pinning | Phase 1 (Task 1.3.10) |
| Cross-shard portfolio margin coherence & headroom reservations | Phase 2 (Task 2.3.12), Phase 19 (Task 19.3.11) |
| Sparse order book liquidity protection (`max_spread_pips`) | Phase 2 (Task 2.3.13) |
| Multi-currency P&L conversion & account accounting | Phase 3 (Task 3.3.9) |
| PostgreSQL 5-year partition archival to S3 Parquet | Phase 4 (Task 4.3.7), Phase 9 (Task 9.3.17) |
| WebSocket in-flight auth upgrade & token renewal | Phase 5 (Task 5.3.26) |
| Standard RFC 6585 rate limit headers (`Retry-After`) | Phase 5 (Task 5.3.27) |
| C++ matching engine bare-metal deploy procedure | Phase 9 (Task 9.3.16) |
| Incident classification P0–P3 & escalation matrix | Phase 9 (Task 9.3.18) |
| Capacity planning & sizing models (50k TPS) | Phase 9 (Task 9.3.19) |
| Proof of Reserves Merkle tree daily scheduler & client API | Phase 13 (Task 13.3.7) |
| FIX session failover & bidirectional gap fill | Phase 18 (Task 18.3.12) |
| Internal off-book position transfers | Phase 19 (Task 19.3.12) |
| MiFID II APA/ARM reporting adapters | Phase 21 (Task 21.3.16) |
| FX Global Code 55-principle self-assessment engine | Phase 21 (Task 21.3.17) |
| Jurisdictional data residency enforcer | Phase 21 (Task 21.3.18) |
| Bank statement ingestion (MT940/MT942/camt.053) | Phase 24 (Task 24.3.12) |
| WAL O_DIRECT 4KB-aligned block flushing | Phase 1 (Task 1.3.6, amended) |
| Deterministic TIME_TICK WAL events for GTD/DAY replay | Phase 2 (Task 2.3.10, amended) |
| PAMM/MAM & copy trading | Phase 14 (Task 14.3.8) |
| Pegged orders (Peg-to-Mid/Primary/Market) | Phase 16 (Task 16.3.11) |
| TWAP/VWAP anti-gaming randomization | Phase 16 (Task 16.3.12) |
| Dark pool & hidden orders | Phase 16 (Task 16.3.13) |
| FIX Allocation Instructions & Reports (Tag 35=J / 35=AK) | Phase 18 (Task 18.3.13) |
| Smart Order Routing (SOR) | Phase 18 (Task 18.3.14) |
| Stale-price liquidation fallback & flash-crash circuit breaker | Phase 19.5 (Task 19.5.3.6) |
| MiFID II RTS 27/28 best-execution reporting | Phase 21 (Task 21.3.19) |
| Forward day-count convention ACT/360 vs ACT/365 | Phase 22 (Task 22.3.1, amended — supersedes hardcoded d/360) |
| Multi-leg implied matching | Phase 22 (Task 22.3.12) |
| CSDR settlement discipline (fails, penalties, buy-in) | Phase 24 (Task 24.3.13) |
| PB credit restitution on settlement failure | Phase 24 (Task 24.3.14) |
| Communications recording (MiFID II taping, WORM storage) | Phase 21 (Task 21.3.20) |
| Account closure & offboarding lifecycle | Phase 14 (Task 14.3.9) |
| House finance reporting (trial balance, finance statements, ERP export) | Phase 20 (Task 20.3.7) |
| Margin model stress testing & backtesting | Phase 19 (Task 19.3.13) |
| Interactive WebSocket Trading API (order submit/cancel) | Phase 5 (Task 5.3.31), Phase 6 (Task 6.3.10) |
| REST Batch Order operations (`/api/v1/orders/batch`) | Phase 5 (Task 5.3.32) |
| Dual-price trigger for conditional orders (`trigger_source`) | Phase 16 (Task 16.3.17) |
| Scalable institutional sub-account ceiling (20/100/1,000) | Phase 5 (Task 5.3.11, amended) |
| Platform daemon execution inventory & multi-tier watchdog architecture | Phase 9 (Task 9.3.28), Phase 2 (Task 2.3.19), spec §19.13 |
| Sub-account internal transfers double-entry GL journal integration | Phase 5 (Task 5.3.23), Phase 3 (Task 3.3.6) |
| Pre-materialized klines contract for TradingView UDF & REST | Phase 6 (Task 6.3.8), Phase 23 (Task 23.3.1) |
| Cross-currency margin USD numeraire normalization & batch mark pricing | Phase 19 (Tasks 19.3.1, 19.3.16) |
| Position transfer atomic collateral rebalancing | Phase 19 (Task 19.3.12), Phase 24 (Task 24.3.7) |
| Liquidation worker mutex contention fail-closed retry with exponential backoff | Phase 19 (Task 19.3.27) |
| Completeness assessment & Day-0 foundation harmonization | Master Spec §27, Phase 1 (Tasks 1.3.1–1.3.2, remediation #39) |
| Canonical filesystem layouts & aliases (`core/` ≡ `engine/`, `services/cmd/` ≡ `services/<svc>/`) | Phase 1 (Tasks 1.3.1–1.3.2, remediation #39) |

(Sanctions-feed-scoped degradation rather than full `ReadOnly` is Phase 21 AC #43, amended.)

---

## 19. Technology Choices — Rationale

| Choice | Why | Alternative Considered |
|---|---|---|
| C++17/20 for core | Sub-µs match latency, deterministic memory, no GC | Java (LMAX), Rust (emerging) |
| Go for services | 10× faster dev than C++ for I/O-bound work; goroutines; single-binary deploys | Java, Python |
| PostgreSQL 16 | MVCC, SERIALIZABLE isolation, logical replication — better for financial correctness | MySQL InnoDB |
| ClickHouse | Columnar, time-series optimized, 100× faster than PostgreSQL for analytics | kdb+ (cost), TimescaleDB |
| Redis 7 (cache only) | Sessions, rate limits, leader election — fast coordination | etcd (leader election) |
| Aeron | Kernel-bypass, sub-µs, used by CME | Chronicle Queue, raw shared memory |
| Custom binary WAL | 10× faster than Redis Streams; no Redis dependency in critical path | Redis Streams, Kafka |
| React 18 + TS | Industry standard for trading UIs; TradingView Lightweight Charts | Vue, Angular |
| Bare metal for core | Deterministic latency requires dedicated hardware; no container jitter | K8s with CPU pinning (insufficient) |
| K8s for services | Standard orchestration for stateless services; HPA, blue-green | Nomad, bare VMs |

---

## 20. File Map

| Path | Content |
|---|---|
| `MEMORY.md` | Authoritative project memory & operational directives |
| `AGENTS.md` | Master plan: phase index, gates, working rules, canonical values |
| `CLAUDE.md` | Agent quick reference |
| `CONTEXT.md` | Onboarding briefing: project scope, doc map, canonical values, how to start |
| `DESIGN.md` | Engineering design decisions & invariants |
| `WORKFLOWS.md` | End-to-end workflow analysis (trading, money, risk, ops, compliance, backoffice) |
| `ARCHITECTURE.md` | This file — system architecture overview |
| `docs/Specification - Complete Exchange System Suite.md` | Master contract (v7.0) |
| `docs/Phase-01-Project-Foundation.md` | C++ core scaffold, CMake, IPC, WAL, PostgreSQL migrations, canonical filesystem aliases (remediation #39) |
| `docs/Phase-01.5-CI-CD-Validation-Harness.md` | CI/CD, 400+ spec checkpoints (actual 543 across 479 tasks; supersedes 530/466, 527/463, 523/461, 522/460, 514/452, 510/448, 506/444, 501/439, 487/425, 485/423, 482/420, 480/418, unverified 467/405, 466/404, 422/360, 407/346), 419-criterion traceability (supersedes prior 418, 414, 401, 398, 390, 389, 381, 377, 373, 368, 354, 352, 349, 347, 335, 334, 333, 296, 276, 256/377/316, 252/373/312, 237/358/297, 219, 206, 201, 192), golden tests |
| `docs/Phase-02-Matching-Engine.md` | C++ matching engine, order book, TIF expiry (GTD/DAY), WAL, leader election, degradation |
| `docs/Phase-02.5-Engine-Soak-Benchmark.md` | 72h soak, 50k/sec, crash injection |
| `docs/Phase-03-Risk-Settlement.md` | Go settlement, T+1/T+2, SWIFT, nostro, fees, risk limits, double-entry GL posting, EOD Tom-Next rollover, holiday calendar |
| `docs/Phase-04-Persistence-Recovery.md` | Snapshots, WAL archive to S3, PostgreSQL PITR, DR & crash recovery orchestration engine (Task 4.3.10) |
| `docs/Phase-04.5-Recovery-Chaos-Validation.md` | 6 chaos scenarios × 3 runs |
| `docs/Phase-05-Order-Gateway-API.md` | Go REST API, JWT, rate limits, route registry |
| `docs/Phase-06-Market-Data-Distribution.md` | Go WS plus SBE A/B multicast, TCP replay/snapshot recovery |
| `docs/Phase-07-Admin-Monitoring.md` | RBAC, dual control, audit, Prometheus, PagerDuty |
| `docs/Phase-08-Integration-Testing.md` | End-to-end validation, acceptance matrix |
| `docs/Phase-08.5-PreProduction-LoadTest.md` | 75k/sec staging gate |
| `docs/Phase-09-Deployment-Operations.md` | Bare metal, K8s, DR, DORA, blue-green, runbooks, load shedding, PTP, multi-tier watchdog |
| `docs/Phase-10-Trader-UI.md` | React 18 + TS, order book, charts, admin dashboard |
| `docs/Phase-11-Funding-Suspension.md` | Banking rails, withdrawals, deposits, kill-switch, nostro |
| `docs/Phase-12-User-Self-Service.md` | Registration (bcrypt), 2FA, KYC submission (SSE-KMS), notifications + delivery tracking |
| `docs/Phase-13-Production-Hardening.md` | Circuit breaker, reconciliation (9 categories incl. GL zero-sum), 47+ alerts, P&L |
| `docs/Phase-13.5-Security-Compliance-Audit.md` | Pen test, PII audit, compliance validation, tabletops |
| `docs/Phase-14-Extended-Features.md` | OCO, auto-halt, testnet, KYC lifecycle, PITR hardening |
| `docs/Phase-15-Market-Admin-Lifecycle.md` | Instrument state machine (7 states — supersedes prior "6 states"; CANCEL_ONLY added), grace periods, trading-hours (24/5) enforcement |
| `docs/Phase-16-Advanced-Order-Types.md` | TWAP, VWAP, trailing stop, peg-to-best, bracket, spread, scaled, benchmark fixing, algo framework |
| `docs/Phase-17-L3-Order-Level-Data.md` | L3 data (real-time, no conflation), WS distribution, surveillance signals (incl. front-running, insider dealing) |
| `docs/Phase-18-FIX-Protocol-Gateway.md` | FIX/FIXS mTLS, certification, sessions, orders, market data, drop copy, Mass Quote, PB, SBE |
| `docs/Phase-19-Multi-Asset-Margin.md` | Margin/liquidation, PB NOP/DSL, mutual bilateral credit and credit-screened liquidity |
| `docs/Phase-19.5-Price-Oracle-Mark-Price.md` | Refinitiv/Bloomberg/ECB, mark/index, staleness gates |
| `docs/Phase-20-Analytics-Reporting.md` | ClickHouse ETL (50k inserts/s), ticks (90d TTL), OHLCV, P&L, volume stats |
| `docs/Phase-21-Compliance-AML.md` | AML, EMIR REFIT/CFTC lifecycle reporting, MiFID, venue governance, CCO controls |
| `docs/Phase-22-Derivatives-Foundation.md` | Forwards, swaps, NDFs, vanilla/barrier/binary options, Greeks, VM, roll |
| `docs/Phase-23-Market-Data-Products.md` | Historical API, data export (CSV/JSON/Parquet), premium feeds |
| `docs/Phase-24-Backoffice-Settlement.md` | Nostro/vostro, CLS ISO 20022, PB, allocations, client-money safeguarding |
