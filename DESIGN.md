# DESIGN — FOREX Exchange System Suite

**Companion to:** [`ARCHITECTURE.md`](./ARCHITECTURE.md) (where things live) — this file records *how* they are built and *why* (data structures, protocols, numerics, invariants, failure semantics).
**Contract:** [`docs/Specification - Complete Exchange System Suite.md, spec §5.3/§5.21 (ledger/wallet/balance)`](./docs/Specification%20-%20Complete%20Exchange%20System%20Suite.md) (v7.0). Where a design decision improves on spec prose, it is logged in spec §27, not silently diverged.

---

## 1. Design Goals & Ordering

1. **Financial correctness above all.** Bit-exact accounting, double-entry GL (`Assets = Liabilities`, zero-sum invariant), idempotent replay, fail-closed on any inconsistency.
2. **Deterministic low latency in the matching core only.** p99 ≤ 50µs tick-to-trade per shard (supersedes prior ≤ 1ms), 50k orders/sec sustained. Everything off the hot path may be slower, easier, and written in Go.
3. **Auditable by construction.** Every state change derivable from WAL events; tamper-evident admin audit chain; every alert has a runbook.
4. **Graceful degradation, never silent corruption.** Six explicitly modeled degradation modes; unknown state ⇒ halt, not guess.

Consequences: C++ on bare metal for matching; Go for I/O-bound services; PostgreSQL `SERIALIZABLE` for money; ClickHouse for analytics; Redis strictly excluded from book/WAL.

---

## 2. Matching Core Design (Phase 1–2)

### 2.1 Process Model

- One C++ process per shard on dedicated bare metal; **exactly one matching thread per shard** — total-order matching with zero locks on the book.
- NUMA-pinned to local cores/memory; `isolcpus` isolation; hugepages for WAL; optional SR-IOV/DPDK NIC path.
- All inbound work reaches the matching thread through a **lock-free SPSC ring**; everything the thread emits (fills, book deltas) goes out on SPSC rings toward IPC publishers. The matching thread never blocks on I/O.
- **Zero dynamic allocation after warmup.** Orders come from pre-sized pools; intrusive containers only. Deterministic memory ⇒ deterministic latency (no allocator/GC jitter).

### 2.2 Order Book Representation (spec §3.1)

- **Flat array of price levels** (`PriceLevel bids_[MAX_LEVELS]`, `asks_[MAX_LEVELS]`), 64-byte cache-line aligned; binary search for insertion level. FX tick grids are dense enough that a flat array beats tree-based books on cache behavior.
- **Intrusive doubly-linked order list per level** (`Order* head/tail`), oldest first — FIFO within a price = price-time priority with O(1) enqueue/cancel and no pointer-chasing allocator nodes.
- Orders carry `timestamp_ns`; `book_seq_` is the monotonic per-book sequence used as the replay cursor.
- **Sparse-book protection** (Task 2.3.13): reject market orders when spread > `max_spread_pips`; fail-closed reject on an empty book; L2/L3 serialization is never padded with phantom levels.

### 2.3 Matching Loop (spec §3.2)

- Limit/market matching walks the opposite side from best price inward, filling FIFO within each level; remainder rests unless IOC/FOK — FOK rolls back atomically (pre-validated fill-or-kill, never partial).
- Stop/stop-limit orders are parked off-book and promoted by trigger evaluation on trade/quote updates, supporting selectable `trigger_source` (`LAST_PRICE`, `MARK_PRICE`, `INDEX_PRICE` per Task 16.3.17).
- Self-trade prevention applies the taker's `stp_mode` when maker/taker share `account_id` or `trade_group_id`; mutually requested `TRANSFER` moves prevented notional through balanced ledger events and persists a non-trade prevented-match record (Tasks 2.3.11/18). NONE remains Professional/ECP-only.
- Reference-price execution collars are snapshotted when an order enters its taker phase and remain fixed for that phase; matching stops before an out-of-range maker and expires the remainder with a persisted reason (Task 2.3.17).
- Cross-shard basket orders never match atomically across shards — the Go gateway coordinates **2PC (Reserve → Commit/Compensate)**; per-shard matching stays single-threaded. Cross-shard portfolio margin coherence uses synchronized margin buffers broadcast to shards at <1ms cadence (Tasks 2.3.12 / 19.3.11).

### 2.4 Pre-Trade Risk — In-Process (spec §3.3)

All 14 checks execute inside the matching process before the book is touched — no IPC round-trip: account status, instrument status, balance, position limit, order rate, price band, max order qty, margin utilization, circuit breaker, KYC tier, tick/lot validation, min notional, exec flags (`post_only`/`reduce_only`, schema per Task 16.3.10), and STP. Rejection reasons use the Phase-05 error-code registry.

### 2.5 WAL & Deterministic Replay (Tasks 1.3.6, 2.3.10; Phase 4)

- **Custom binary WAL**, `mmap(MAP_SHARED)`; frame = `{magic 0x57414C00, version, shard_id}` then entries `{seq u64, ts_ns u64, event_type u8, payload_len u32, payload FlatBuffers, crc32 u32}`.
- **O_DIRECT with 4KB `posix_memalign`-aligned block flushing** (Task 1.3.6, amended) — no torn/page-cache-dependent writes; fsync per batch: 1ms or 100 events, whichever first.
- WAL is trimmed only **after** PostgreSQL persistence is confirmed, and S3-archived (ETag-verified) before trim — zero-loss guard, replayable via `exchange:replay-from-archive`.
- **Deterministic `TIME_TICK` WAL events** (Task 2.3.10, amended) drive GTD/DAY order expiry so replayed state evolves identically — replay is a pure function of the WAL stream.
- Snapshots every 100k trades or 5 min (FlatBuffers, stored with `snapshot_seq`). Boot: load snapshot → replay WAL from `snapshot_seq+1` → **verify `book_seq == WAL tail`; on mismatch apply the graduated recovery ladder (WAL repair → snapshot rebase → fail-closed halt with a `recovery_reports` row as last resort — spec §3.5, remediation #8)** → skip already-applied seqs (idempotent). Gate: < 10s recovery, zero duplicate/missing trades.
- Leader election via Redis `SETNX engine:leader:{shardId}` (10s TTL, 3s heartbeat); split-brain ⇒ both candidates stop matching, P1 alert, degradation to `ReadOnly`.

### 2.6 Numerics

- **No floating point for money or prices.** Prices and quantities are fixed-point integers in instrument tick/lot units (`tick_size`, `lot_size`, `min_notional` per instrument); P&L converted across currencies via oracle rates (Task 3.3.9).
- Time is UTC nanoseconds at the core; business-day math uses **ISDA Modified Following** across base/quote/settlement holiday calendars (Task 3.3.8); forward pricing respects instrument day-count **ACT/360 vs ACT/365** (Task 22.3.1, amended — supersedes hardcoded d/360).

---

## 3. IPC Design (spec §2.3)

| Path | Transport | Configuration |
|---|---|---|
| Core ↔ gateway (same host) | Aeron `aeron:ipc` | Dedicated C media driver (`aeronmd`) on isolated cores, `threadingMode=DEDICATED`, 128MB term buffers, MTU 1408, 16MB socket buffers; `BusySpinIdleStrategy` at core, `BackoffIdleStrategy` at Go consumers; verified RT < 50µs (Task 1.3.10) |
| Core ↔ remote nodes | Aeron `aeron:udp` multicast/unicast endpoints | For multi-node distribution and institutional feeds |
| Fallback/dev | Shared-memory SPSC ring (`shm_open` + `mmap`, cache-line aligned) | Same message framing |
| Go ↔ Go (cold path) | gRPC + **NATS JetStream** (committed backbone, spec §2.3.1 — supersedes 'NATS or Kafka') + PostgreSQL LISTEN/NOTIFY | Never on the hot path |

Envelope: zero-copy FlatBuffers/custom binary with explicit `seq` everywhere so consumers can detect gaps and demand replay. **HTTP/gRPC never carry order flow to the core.**

---

## 4. Service Design Conventions (Go, Phase 5+)

- One responsibility per service, single static binary, `slog` structured logging, context-scoped cancellation, goroutine-per-connection only at WebSocket edges.
- **All HTTP routes are registered in Phase-05 (Task 5.3.7)** even when implemented later; **all error codes in the Phase-05 registry (Task 5.3.21)**. Programmatic auth supports legacy HMAC plus Ed25519/RSA public-key verification (Task 5.3.38); weighted multi-interval limits are queryable (5.3.40). REST/WS expose batch, cancel-replace, keep-priority, quote-quantity, and dry-run preview. JSON is default; REST/WS/private/FIX negotiate versioned SBE with ≥6-month deprecation and explicit retirement (Tasks 6.3.18/18.3.17).
- Idempotency keys are strictly account-scoped (`idem:{account_id}:{key}` in Redis, composite `(account_id, idempotency_key)` in PostgreSQL); unique constraint violations (23505) replay cached responses rather than emitting 500 errors (Task 5.3.24, remediation #38). Sub-account internal transfers route via `DoubleEntryLedgerService::postJournal()`, emitting `BalanceChanged` to NATS JetStream for real-time WS sync (Task 5.3.23, remediation #38).
- Services are stateless; state lives in PostgreSQL (truth), Redis (cache/coordination, Sentinel HA), or the C++ core (book). Horizontal scale via HPA; blue-green deploys.

---

## 5. Data Design

| Store | Design rules |
|---|---|
| PostgreSQL 16 | Money moves only under `SERIALIZABLE`; double-entry GL posting (Task 3.3.6) with zero-sum journal invariant; `pg_partman`-partitioned `trade_history`; 5-year archival to S3 Parquet (Tasks 4.3.7 / 9.3.17); PITR RPO ≤ 15s / RTO ≤ 5min |
| ClickHouse | MergeTree tick tables, daily partitions, LZ4, TTL 90d raw / 5yr aggregate; ≥ 50k inserts/s; its own backup/DR (Task 4.3.6); pre-materialized kline tables serve TradingView/REST with zero runtime scans on raw trades (Tasks 6.3.8/23.3.1, remediation #38) |
| Redis 7 | Sessions, rate limits, locks, feature flags, non-critical pub/sub, leader election — **never the order book, never the WAL**; DR RPO ≤ 5s / RTO ≤ 30s |
| S3 | WAL archive (90d → Glacier, CRR to DR region, ETag-verified), KYC docs (SSE-KMS), exports |

---

## 6. Risk & Liquidation Design (Phase 19 / 19.5)

- Margin modes: CROSS / ISOLATED / PORTFOLIO; retail caps ESMA 30:1-20:1-10:1, CFTC 50:1/20:1 (Phase-19). Cross-currency portfolio required margin is normalized to base USD numeraire at Mark/Index rates (`required_margin_quote × rate_to_usd`) before summation; mark prices batch-loaded in O(N) linear time with zero per-position N+1 database queries (Tasks 19.3.1/19.3.16, remediation #38).
- Prime-broker pre-trade credit: NOP/DSL limits (Task 19.3.7); mutual bilateral credit screens liquidity so clients only see credit-eligible prices (Task 19.3.10); PB credit restitution on settlement failure (Task 24.3.14). Position transfers (give-ups / adjustments) atomically rebalance locked collateral (`balances.locked`) between accounts within the same `SERIALIZABLE` transaction (Tasks 19.3.12/24.3.7, remediation #38).
- Liquidation is an explicit state machine, not a best-effort job: **2s scanner** → auction when liquidated notional > 1% of OI → CALL **5s** → FILL → EXTEND ≤ 60s total with floor decay 0.5%/5s → FORCE_CASH at mark ×0.95/×1.05 → LP rebate **0.05% from insurance fund** → ADL only when the fund is depleted. Collateral haircuts/concentration limits and retail negative-balance protection: Tasks 19.3.8–9. Liquidation queue worker mutex lock contention releases back to queue with exponential backoff and clears dedup keys on retry timeout (fail-closed anti-stranding invariant, Task 19.3.27, remediation #38).
- Single **PriceOracle** (Refinitiv / Bloomberg BFIX / ECB) serves margin, derivatives, and auto-halt — one integration, no duplicates. Mark = median of ≥2 feeds, 1s cadence; **5s staleness gate, fail-closed**; stale-price liquidation fallback + flash-crash breaker per Task 19.5.3.6. Yield-curve feeds per Task 19.5.3.5.

## 7. Failure & Degradation Design

- **Strict Fail-Closed Zero-Loss Pessimism (spec §2.7, Remediation #15):** 4-tier error hierarchy (L0 Critical Core Halt, L1 Systemic ModeManager Degradation, L2 Transaction Rejection & Rollback, L3 Protocol Validation & IP Ban). In institutional FX trading, an error condition must never lead to lost client funds, corrupted ledger balances, unhedged positions, or non-deterministic execution. Any ambiguous, corrupt, or invariant-violating state immediately triggers deterministic fail-closed behavior, structured audit emission, and graceful system degradation. Zero non-deterministic state transitions, zero dropped ticks without backpressure buffering, and zero unhedged delivery.
- Six degradation modes (exact casing) `Normal · ReadOnly · MarketDataOnly · SpotOnly · Throttled · Maintenance`, owned by the Phase-02 ModeManager, surfaced via `X-Degradation-Mode` header and WS `system.status` events; P1/P2 alerting on transitions. Mode transitions trip within 500ms; restoring `Normal` requires 30s consecutive healthy telemetry hysteresis.
- Five-tier circuit breaker (Phase-13) `CLOSED → OPEN → HALF_OPEN → CLOSED`, persisted in Redis: INSTRUMENT, ACCOUNT, MARKET_WIDE, OPTIONS_VOLATILITY, VOLUME_SPIKE.
- Sanctions-feed loss degrades **scoped** (blocks affected flows) rather than forcing full `ReadOnly` (Phase-21 AC #43, amended).
- Recovery chaos suite (Phase-4.5): 6 scenarios × 3 runs, each must pass with zero duplicate/missing trades and < 10s recovery.

## 8. Security & Compliance By Design

- TLS 1.3 everywhere; FIX over FIXS with mTLS + client certification and Ed25519/SNI (Tasks 18.3.11/17); JWT (15min/7d) + OAuth2 + TOTP/passkeys (non-destructive candidate secret staging in `users.two_factor_pending_secret` / Redis `2fa:pending:{user_id}`, Task 12.3.2; Passkey assertion elevates `two_factor_verified = true` with JWT AMR `fido2`, Task 12.3.7, remediation #38); PAMM unit investments/redemptions use internal ledger taxonomy (`PAMM_INVEST`, `PAMM_REDEEM`) strictly excluded from daily fiat cash withdrawal limits (Task 14.3.8, remediation #38); API keys IP-allowlisted; venue-admin RBAC remains six roles while institutional clients have separate scoped delegated roles and M-of-N approvals (Task 12.3.11); tamper-evident audit chain; PII AES-256 at rest; secrets in Vault/KMS; supply-chain scanning from Phase 1.5.
- Compliance is designed-in, not bolted-on: SanctionsHook runs **inside** the C++ pre-trade checker (Task 21.3.10); surveillance signals emitted by the L3 pipeline (Phase-17) are enforced by Phase-21; clock discipline via PTP IEEE 1588 within 100µs of UTC for MiFID II RTS 25 (Task 9.3.12); data residency enforced by jurisdiction partitioner (Task 21.3.18).
- DORA ICT resilience (Task 9.3.15); incident classes P0–P3 with escalation matrix (Task 9.3.18); 47+ alert runbooks validated in Phase-13.5.

## 9. Validation Design

- **Traceability is executable:** CI maintains 400+ spec checkpoints (actual **543 across 479 tasks**, remediation #37 mechanical recount; supersedes 530/466, 527/463, 523/461, 467/405, 466/404, unverified #14 claim 422/360, #13 claim 407/346 and prior 377/316, 373/312, 358/297, 369/340/323/315/310+/240+/182+) plus the **419-criterion §24 matrix** — production release requires 419/419 passing executable tests and zero `PLANNED`/unmapped rows (supersedes prior 418/418, 414/414, 401/401, 398/398, 390/390; Phase-08 owns the matrix; Phase-01.5 owns the harness; golden corpus of 20+ spec-derived cases). **Every one of the 419 rows also carries a unique *Stable Test Contract* identifier** — remediation #18 backfilled this from 77 of 333 and renumbered 4 duplicate pairs (T16-010, T22-014, T22-015, T19-020), remediation #22 added `OPS-334`, remediation #23 added `DR-335`, remediation #32 added `MAT-382`–`MAT-389`, remediation #33 added `MAT-390`, remediation #35 added `MAT-391`–`MAT-398`, remediation #36 added `MAT-399`–`MAT-401`, remediation #37 added `MAT-402`–`MAT-414`; CI validates the ID set is unique with no gaps by prefix. The **owner column is uniform `Phase N — Title`** across all 419 rows (remediation #18 normalized 96 terse `Phase-NN` cells from the #238+ range), so CI can group rows by implementing phase mechanically — while the AC-ref column deliberately keeps its `Phase-NN §AC row N` form, since owner = *implements* and AC-ref = *verifies*, and those are different phases for 31 rows (integration, ops and FIX re-verification). Remediation #19 additionally canonicalized the owner **titles** to the AGENTS Phase Index verbatim (229 cells, 344 owner segments incl. 11 compound `A / B` owners; 2 phase H1s aligned), so the Phase Index is the single canonical name source and CI can key on the full label. Every §23 error code is **owner-resolvable**: a phase plan emits the token, or the §23 description cites the owning `Phase-NN Task N.N.N` — `CORPORATE_ACTION_SCHEDULED` is the lone `reserved, never emitted` entry, since no corporate actions exist in fiat spot FX. Phase-05 Task 5.3.21 asserts **zero ownerless codes** (149 codes, supersedes 145/144/131).
- Gates, not vibes: soak (72h), chaos (6×3), pre-prod load (75k/sec, 10,000 WS), pen test (0 Critical / <3 High) block downstream phases per `AGENTS.md`.

## 10. Explicit Non-Goals & Scope Boundaries

- **No RFQ / RFS / indicative quoting / last-look (spec §6.4):** 100% firm liquidity only. Traditional bank market makers accustomed to last-look optionality must provide firm quotes; defended per FX Global Code Principle 17 (spec §27 R11).
- **No retail third-party broker bridges (MT4/MT5 bridges):** The exchange suite provides direct, standard institutional interfaces (FIX 4.4/5.0 SP2, SBE multicast, interactive WebSocket, and REST). Third-party retail bridge plugins remain outside core exchange scope. (PAMM/MAM & Copy Trading are natively supported via Phase-14 Task 14.3.8).
- **No cryptocurrency rails, wallets, or custody:** Strictly fiat FOREX currencies and institutional settlement rails (SWIFT, SEPA, FedNow, ACH, CHAPS, TARGET2, CLS PvP).
- **No containers or dynamic allocation in the C++ hot path:** Zero heap allocations after warmup; dedicated bare metal with thread pinning.
- **No Redis or PostgreSQL involvement in the matching loop:** In-memory order book + binary WAL; Redis for sessions/coordination only; PostgreSQL for serializable GL settlement.
- **No floating-point money or prices:** Bit-exact fixed-point integers in tick/lot units throughout the matching and risk engines.
- **Native mobile applications deferred to v2 (spec §27 R14, remediation #39):** Phase 10 delivers a 100% responsive web UI via React 18 and TradingView charts; native iOS/Android apps are deferred.
- **No multi-tier affiliate or referral marketing schemes (spec §27 R15, remediation #39):** Excluded to preserve tier-1 institutional venue compliance and prevent predatory retail customer acquisition structures.
- **100% fiat cash collateral only; non-cash collateral deferred (spec §27 R16, remediation #39):** Phase 19/24 support fiat cash collateral only. Non-cash collateral (Treasury bills, physical gold) is deferred to v2.
- **Retail Introducing Broker (IB) volume rebate structures deferred (spec §27 R17, remediation #39):** Institutional clearing, prime-brokerage give-ups, and CLS settlement take priority over retail IB rebate schemes.
- **Firm CLOB liquidity strictly defended (spec §27 R11, R18, remediation #39):** Per FX Global Code Principle 17, all order book quotes are firm. RFQ, RFS, indicative quotes, and last-look liquidity models are strictly prohibited.
