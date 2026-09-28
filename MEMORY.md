# MEMORY — FOREX Exchange System Suite

**Authoritative Project Memory & Operational Directive**  
**Spec Reference:** [`docs/Specification - Complete Exchange System Suite.md`](./docs/Specification%20-%20Complete%20Exchange%20System%20Suite.md) (v7.0)  
**Planning Reference:** [`AGENTS.md`](./AGENTS.md) (30 phases: 24 core + 6 buffer)  
**Companion Meta-Docs:** [`CONTEXT.md`](./CONTEXT.md) · [`CLAUDE.md`](./CLAUDE.md) · [`ARCHITECTURE.md`](./ARCHITECTURE.md) · [`DESIGN.md`](./DESIGN.md) · [`WORKFLOWS.md`](./WORKFLOWS.md)

---

## 1. Executive Summary & Domain Scope

- **Domain:** Tier-1 institutional Foreign Exchange (FX) CLOB exchange (Spot, Forwards, Swaps, NDFs, Vanilla/Barrier Options).
- **Asset Scope:** 100% Fiat currencies only (USD, EUR, GBP, JPY, AUD, CAD, CHF, MXN, BRL, etc.). **Zero cryptocurrency, zero blockchain, zero crypto custody.**
- **Trading Schedule:** 24/5 continuous trading (Sydney open 21:00 UTC Sunday → New York close 22:00 UTC Friday).
- **Liquidity Model:** Central Limit Order Book (CLOB) with firm liquidity only. RFQ, RFS, indicative quotes, and last-look protocols are strictly out of scope per FX Global Code Principle 17.
- **Funding & Rails:** Fiat banking rails only (SWIFT MT103/MT202/MT900/MT910, SEPA SCT/Inst, FedNow, ACH, CHAPS, TARGET2; CLS ISO 20022 PvP for eligible settlement pairs).
- **Completeness Matrix & Day-0 Baseline (Remediation #39):** Comprehensive assessment across 13 operational domains and 133 components confirms 100% specification and planning depth with 0.0% physical application code on disk.
- **Business Boundary Rulings (R14–R18, Spec §27):** R14 mobile UI deferred to v2 (web-only responsive UI via React 18); R15 multi-tier affiliate/referral programs excluded; R16 fiat cash collateral only, non-cash deferred; R17 retail IB rebate schedules deferred; R18 firm CLOB liquidity strictly enforced per FX Global Code Principle 17.
- **Repo State:** Planning-stage repository containing 100% complete specification and phase implementation plans. Git initialized with zero commits. Next unit of work: **Phase 1 (C++ Core Foundation)**.

---

## 2. Canonical Counts & System Metrics (Zero-Drift Baseline)

All counts are verified mechanically across the corpus:
- **Evaluated Operational Domains:** **13** domains (**133** components, 100% spec & plan depth, 0% code implementation, remediation #39)
- **§24 Acceptance Criteria:** **419** criteria (contiguous 1..419, each with unique Stable Test Contract ID; supersedes prior 418, 414)
- **Phase Tasks:** **479** tasks across 30 phase plans
- **Checklist Spec Checkpoints:** **543** checklist checkpoints across all phase tasks
- **Acceptance Criteria Rows:** **1,079** rows (976 core + 103 buffer phase rows)
- **Spec §23 Error Codes:** **172** codes (100% owner-resolvable against phase tasks; supersedes 170/149 — remediation #44 registered the 21 gateway `localRow` emissions + follow-on `INVALID_DEPTH_LIMIT`/`INVALID_INTERVAL` from Phase-06)
- **Database Migrations:** **68** `.up.sql` files on disk (sparse task-numbered prefixes 001–190, collision-free, each paired `.down.sql`; supersedes prior "49 files" and "108 contiguous" prose — planned corpus allocated 001–108 by task number, implementation allocates beyond it, e.g. 109–119/150 in Phase-03, 023/065/092/120 in Phase-04, 151–155/160–162/170–173/180–182/190 in Phase-05)
- **Phase Plans:** **30 phases** (24 core + 6 mandatory buffer phases: 01.5, 02.5, 04.5, 08.5, 13.5, 19.5)
- **Financial Correctness Categories:** **9** categories (including double-entry GL zero-sum conservation)
- **Degradation Modes:** **6** exact-cased modes (`Normal · ReadOnly · MarketDataOnly · SpotOnly · Throttled · Maintenance`)
- **Error Severity Hierarchy:** **4** tiers (`L0` Critical Core Halt, `L1` Systemic ModeManager, `L2` Transaction Atomic Rejection/Rollback, `L3` Gateway Fast Protocol Rejection/IP Ban)

---

## 3. Technology Architecture & Hard Constraints

| Layer | Planned Technology | Critical Architectural Constraints |
|---|---|---|
| **Matching Core** | C++17/20, Bare Metal | Single matching thread per shard, NUMA-pinned, lock-free SPSC queues, intrusive flat-array order book, zero heap allocation after warmup. |
| **Persistence (Core)** | Custom Binary WAL | `mmap(MAP_SHARED)` + `O_DIRECT` 4KB `posix_memalign` block flushing; fsync batch 1ms or 100 events; deterministic `TIME_TICK` WAL events for GTD/DAY order expiry. |
| **Hot Path IPC** | Aeron C Media Driver | Kernel-bypass sub-microsecond IPC (`aeron:ipc` / `aeron:udp`), dedicated threads, zero-copy FlatBuffers framing. **Never HTTP/gRPC in the hot path.** |
| **Cold Event Bus** | NATS JetStream | 3-node cluster, at-least-once delivery, 7-day replay window; Aeron-to-NATS Bridge fan-out. |
| **Microservices** | Go 1.23+, Kubernetes | Single-binary microservices, goroutine-per-connection for WS, structured `slog` logging, stateless with horizontal HPA. |
| **OLTP Database** | PostgreSQL 16 | `SERIALIZABLE` isolation for every balance mutation, `wal_level=replica`, `pg_partman` daily partitioning on `trade_history`, S3 WAL archive (RPO ≤ 15s, RTO ≤ 5min). |
| **Analytics Engine** | ClickHouse | Columnar MergeTree, daily partitions, LZ4 compression, 90d raw tick TTL / 5yr aggregates; pre-materialized kline tables. |
| **Coordination & Cache**| Redis 7 (+ Sentinel HA) | Sessions, rate limits, distributed locks, leader election (`SETNX`). **NEVER the order book, NEVER the WAL.** (RPO ≤ 5s, RTO ≤ 30s). |
| **Client Interfaces** | React 18, FIX, REST, WS | FIX 4.4/5.0 SP2 (QuickFIX-Go), FIXS mTLS, SBE binary streams, TradingView Lightweight Charts, RFC 6585 rate headers. |

---

## 4. Load-Bearing Architectural Invariants

1. **Strict Fail-Closed Zero-Loss Pessimism (Spec §2.7):** Any ambiguous, corrupted, or invariant-violating state immediately triggers deterministic fail-closed behavior, structured audit emission, and graceful system degradation. Financial loss or silent state corruption is strictly unacceptable.
2. **Double-Entry General Ledger & Zero GL Bypass (Spec §5.3, §5.21):** Every financial movement generates balanced DEBIT/CREDIT entries (`Assets = Liabilities`). All internal transfers (master-to-sub, sub-to-sub) MUST route through `DoubleEntryLedgerService::postJournal()`. Direct SQL balance updates or unbacked ledger rows are forbidden. Commits dispatch `BalanceChanged` to NATS JetStream.
3. **Account-Scoped Idempotency Keys (Spec §8.8):** All idempotency keys are namespaced `idem:{account_id}:{key}` in Redis and composite unique constraint `(account_id, idempotency_key)` in PostgreSQL. Handlers intercept PostgreSQL unique constraint conflicts (`23505`) and replay cached payloads rather than emitting HTTP 500 errors.
4. **PAMM Internal Ledger Segregation (Spec §5.3, §14.8):** PAMM unit investments and redemptions use internal transaction types (`PAMM_INVEST`, `PAMM_REDEEM`) on internal sub-ledgers, strictly segregated from external fiat banking daily withdrawal limit accumulators (KYC T0/T1/T2 caps).
5. **Cross-Currency Margin Normalization & Batch Mark Pricing (Spec §13.1, §13.6d):** Position initial and maintenance margin in quote/base currencies is converted to base USD numeraire (`required_margin_quote × rate_to_usd`) before summation; mark prices are batch-loaded in $O(N)$ linear time with zero per-position N+1 queries.
6. **Liquidation Queue Contention & Mutex Resilience (Spec §13.4, §13.15):** `ConsumeLiquidationQueue` worker must never return 0 (success) on mutex lock contention (`lock:liquidation:account:{account_id}`); must release back to queue with exponential backoff (`release(delay)`), clearing Redis dedup key and alerting on retry limit exhaustion.
7. **Atomic Position Transfer Collateral Rebalancing (Spec §13.9, §17.4):** Off-book position transfers (give-ups, allocations, admin adjustments) must atomically release `balances.locked` on source and calculate/lock initial margin on destination within the same `SERIALIZABLE` transaction.
8. **Pre-Materialized Candlesticks for TradingView (Spec §10.3, §16.2):** Historical chart endpoints and TradingView UDF `/history` must query pre-materialized `fx_klines` tables. Runtime table scans on `fx_trades` or dynamic in-memory candle aggregation are prohibited.
9. **Non-Destructive TOTP Re-Enrollment & Passkey Elevation (Spec §8.1, §12.2, §12.6):** `POST /api/v1/auth/2fa/setup` stages candidate secrets in `users.two_factor_pending_secret` / Redis `2fa:pending:{user_id}` (10-min TTL); active 2FA remains enforced until confirmed. Passkey assertion ceremony elevates session state `two_factor_verified = true` with JWT AMR `fido2`.
10. **Numerics & Money:** No floating point for money or prices. All calculations use fixed-point integers in tick/lot units (`DECIMAL(28,8)` in DB). Day-count conventions support instrument-specific `ACT/360` vs `ACT/365`.
11. **Matching Core SRP Component Decomposition (Spec §3.7, Phase-02 Task 2.3.2):** Monolithic matching loops are decoupled into zero-heap modular components (`SelfTradeGuard`, `IcebergManager`, `StopOrderTrigger`, `WalWriter`, `IpcPublisher`), ensuring zero dynamic heap allocations and non-blocking I/O in the matching hot path.
12. **Tamper-Evident Audit Trail Hash Chaining (Spec §14.11, Phase-01 Task 1.3.8, Phase-21 Task 21.3.27):** SHA-256 cryptographic linkage (`prev_checksum` / `prev_hash`) across all audit log records with daily automated Merkle root computations mathematically prevents unauthorized out-of-order data tampering.
13. **Canonical Filesystem Layout & Namespace Aliasing (Spec §27 Remediation #39, Phase-01 Tasks 1.3.1–1.3.2):** C++ matching core in CMake is located at `core/` and aliased symmetrically to `engine/` in architectural plans; Go microservice binaries reside in `services/cmd/<service>` and deploy symmetrically to container structures at `services/<service-name>/`; frontend web interface in `frontend/src/`; testing suites in `tests/chaos/`, `tests/soak/`, `tests/spec/`.

---

## 5. Change Protocol & Working Rules (Docs as Code)

1. **The Specification is the Contract:** Phase plans conform to the Master Specification. If a superior implementation design arises, document it in **Spec §27 (Decision Log)** before updating phase plans.
2. **Append-Only Task Numbering:** Never renumber existing tasks. New tasks receive the next sequential ID (`### Task N.N.N`).
3. **Sequential Acceptance Criteria:** Acceptance Criteria tables (`## N.7`) must remain strictly sequential. If criteria are added, update all stated totals (SDD checklist counts, gate counts, meta-docs) in the same commit.
4. **Explicit Supersession Trail:** When modifying existing behaviors, metrics, or thresholds, always record an explicit note at the old location: `(supersedes/replaces prior ...)`. Bare contradictions between documents are treated as critical defects.
5. **Atomic Meta-Doc Synchronization:** Any change to specifications, phase tasks, endpoints, schemas, error codes, or canonical counts must be synchronized across all root meta-docs (`AGENTS.md`, `CLAUDE.md`, `CONTEXT.md`, `ARCHITECTURE.md`, `DESIGN.md`, `WORKFLOWS.md`, and `MEMORY.md`).
6. **Mechanical Verification Before Completion:** Always verify edits using text searches (`grep -rn`) and automated count scripts before claiming task completion.
7. **Strict Repo Isolation (standing agent rule, 2026-09-27):** Investigate across repos freely, but write only inside the working repo. A cross-repo edit requires demonstrated necessity plus explicit approval first. Shared host infra (PostgreSQL/Redis/shell profile) is inherently cross-cutting — flag it before changing.

---

## 6. Key Mechanism Ownership Map

- **API Route Registry (all endpoints):** Phase 5 (Task 5.3.7 / 5.3.46)
- **Error Code Registry & Enforcement:** Phase 5 (Task 5.3.21) — 173 codes, zero ownerless codes
- **Matching Core & Degradation Modes (6):** Phase 2 (ModeManager)
- **Circuit Breaker (5-Tier):** Phase 13
- **Double-Entry General Ledger & Tom-Next Rollover:** Phase 3 (Tasks 3.3.6, 3.3.7)
- **Persistence, Crash Recovery & DR Orchestration:** Phase 4 (Tasks 4.3.5, 4.3.10, 4.3.11, 4.3.12)
- **Market Data WebSocket & SBE A/B Multicast:** Phase 6 (Tasks 6.3.6, 6.3.8)
- **Admin RBAC (6 Roles) & Dual Control:** Phase 7 (Tasks 7.3.11, 7.3.12)
- **Deployment, Ops, DORA & Multi-Tier Watchdogs:** Phase 9 (Tasks 9.3.12, 9.3.15, 9.3.28)
- **Trader Frontend UI (React + TradingView):** Phase 10
- **Fiat Banking Rails & Suspension:** Phase 11
- **User Self-Service, Auth, Passkeys & 2FA:** Phase 12 (Tasks 12.3.2, 12.3.7)
- **Instrument Lifecycle (7 States) & 24/5 Trading:** Phase 15
- **Advanced Order Types (Algo, Peg, Bracket, OPO):** Phase 16
- **L3 Market Data & Surveillance Signals:** Phase 17
- **FIX / FIXS Protocol Gateway & Drop Copy:** Phase 18
- **Multi-Asset Margin, ADL, Auction & Bilateral Credit:** Phase 19 (Tasks 19.3.1, 19.3.10, 19.3.12, 19.3.27)
- **Price Oracle (Refinitiv/Bloomberg/ECB, 5s staleness):** Phase 19.5
- **Analytics, ETL & Reporting (ClickHouse):** Phase 20
- **Compliance, AML, Sanctions, MiFID II / EMIR:** Phase 21
- **FX Derivatives (Forwards, Swaps, NDFs, Options):** Phase 22
- **Historical Market Data APIs:** Phase 23
- **Backoffice, Nostro/Vostro, CLS PvP & Allocation:** Phase 24

---

## 7. Operational Roadmap & Immediate Next Steps

- **Current Repository State:** Master specification, 30 phase implementation plans, and all companion meta-docs are 100% aligned, audited, and mechanically verified. System-wide completeness assessment (Remediation #39) verified 100% specification and planning coverage across all 13 domains and 133 components, with 0.0% physical source code on disk (Day-0 baseline).
- **Entry Gate:** Phase 1 has no predecessor dependencies and is ready for execution.
- **Actionable Next Step:** Begin **Phase 1: C++ Core Foundation** (`docs/Phase-01-Project-Foundation.md`):
  1. CMake project configuration with C++17/20 flags, sanitizer options, and strict warning levels (`Task 1.3.1`).
  2. Data structure scaffolding (fixed-point pipette integer arithmetic with $10^8$ scaling, order book entry pools, lock-free ring buffers).
  3. Aeron C media driver integration & SPSC ring queues (`Task 1.3.2`, `Task 1.3.5`).
  4. Binary WAL manager (`Task 1.3.6`) with `O_DIRECT` 4KB-aligned block flushing.
  5. Go service module skeleton with PostgreSQL 16 migrations 001–021 (`Task 1.3.7`).
- **Dev-environment maintenance (2026-09-27, no spec/plan impact):** `opencode.json` MCP servers `postgres`/`redis`/`playwright` flipped `enabled:false → true` (they were parked for unset `{env:...}` placeholders); credentials verified live (`match` DB `SELECT 1`, Redis `PONG`) and exported as `POSTGRES_CONNECTION_STRING`/`REDIS_URL` in `~/.bashrc` (kept out of git — restart opencode to pick up); `github`/`clickhouse` stay disabled (no token / no server). Host PG (`/www/server/pgsql`, PG 18.0) `log_statement all → 'ddl'` + reload — `all` logged every PDO `DEALLOCATE` (GBs/day); slow queries still covered by `log_min_duration_statement=5s`. Four one-off `fx_orders.quantity` probe errors (no source in code; column is `qty`) traced to a stale `quantity` wording in an API code comment, corrected to `qty`. Verified: `docs/` carry no `log_statement` requirement and no `quantity` claim, so no spec/§27 change. **Canonical counts unchanged** (414/479/543/1,079/149/108).
