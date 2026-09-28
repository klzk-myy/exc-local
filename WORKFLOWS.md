# WORKFLOWS — FOREX Exchange System Suite

**Companion to:** [`CONTEXT.md`](./CONTEXT.md) (orientation) · [`ARCHITECTURE.md`](./ARCHITECTURE.md) (topology) · [`DESIGN.md`](./DESIGN.md) (engineering decisions).
**Contract:** [`docs/Specification - Complete Exchange System Suite.md, spec §5.3/§5.21 (ledger/wallet/balance)`](./docs/Specification%20-%20Complete%20Exchange%20System%20Suite.md) (v7.0). Every workflow cites its spec section and owning phase — each mechanism has exactly one owner (see `AGENTS.md` ownership map).

Groups: **A** Client trading · **B** Money movement · **C** Risk & market integrity · **D** Market lifecycle & admin · **E** Recovery & operations · **F** Compliance · **G** Backoffice & derivatives · **H** Development workflow.

---

## A. Client Trading

### A1. Order Lifecycle (entry → fill)

```
Client (REST / WS / FIX / UI)
  → Gateway: auth (JWT/OAuth2/FIXS mTLS) → rate limit (Redis, RFC 6585 headers)
  → entry via REST/interactive WS/FIX: single, batch, quote-quantity, test preview, cancel-replace, keep-priority
  → idempotency: account-scoped keys (`idem:{account_id}:{key}` in Redis, composite `(account_id, idempotency_key)` in DB; 23505 conflict replay)
  → auth via JWT/OAuth2 or HMAC/Ed25519/RSA; weighted multi-interval limits are queryable
  → validate (tick/lot, min_notional, exec flags, reference-price execution rule, preview has no side effects)
  → Aeron/IPC → C++ shard PreTradeChecker (14 in-process checks, spec §3.3)
      incl. instrument state (Phase-15), trading hours 24/5, PB NOP/DSL (19.3.7),
      sanctions hook (21.3.10), bilateral mutual credit (19.3.10)
  → Matcher (price-time priority; same-account/trade-group STP incl. TRANSFER; taker-phase reference collar)
  → WAL append (mmap + fsync batch, O_DIRECT 4KB-aligned)
  → Aeron fan-out → Settlement (SERIALIZABLE postings) · MarketData (L2/L3)
    · Analytics (ClickHouse tick) · Notifications (fill) · Compliance (APA < 1 min)
Rejects: typed error codes from Phase-05 registry (5.3.21), e.g.
POST_ONLY_VIOLATION, REDUCE_ONLY_VIOLATION, PB_NOP_LIMIT_EXCEEDED,
BILATERAL_CREDIT_EXCEEDED, DEGRADED_MODE, ORDER_REJECTED_NO_LIQUIDITY
```
Sparse-book rules (Task 2.3.13): market orders rejected when spread > `max_spread_pips`; MARKET/IOC/FOK fail-closed on an empty side; L2/L3 never padded.

### A2. Time-in-Force Expiry
GTD/DAY orders expire via **deterministic `TIME_TICK` WAL events** (Task 2.3.10, amended) so replayed books expire identical orders — expiry is a function of the WAL stream, not wall clocks.

### A3. Algo & Special Execution (Phase-16)
TWAP/VWAP/VP/grid with anti-gaming and parent accounting · TRAILING_STOP · BRACKET/OTO/OCO · OPO/OPOCO net-proceeds lists · SCALE/SPREAD/ICEBERG/PEG · dual-price triggers · FIXING · hidden/dark · delayed dispatch. Recurring conversion and target rebalancing execute only as suitability-gated firm CLOB orders; principal/RFQ Convert remains out. **SOR**: Phase-18 Task 18.3.14.

### A4. Institutional FIX Workflow (Phase-18)
Logon over FIXS with mTLS, Ed25519, SNI, certification, entitlement/CoD/throttle → tag-value FIX or negotiated FIX SBE → orders/cancel-replace/keep-priority/order lists/Mass Quote+MMP → drop copy + Traiana → allocations 35=J/AK → failover/gap fill. Planned maintenance sends repeated `News` drain advisories while cancels remain available (18.3.17).

---

## B. Money Movement

### B1. Deposit (Phase-11)
Bank credit on rail (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2, rail cut-offs per Task 11.3.7) → account-scoped idempotency check (`idem:{account_id}:{key}`) → statement/reference match (beneficiary registry + third-party-deposit rules, Task 11.3.7) → sanctions screen → anti-fraud tiering `<$10K auto / $10K–$50K double-confirm / >$50K PENDING_REVIEW + 4h` → dual-source verification → balance credit (GL double-entry) → notification.

### B2. Withdrawal (Phase-11)
Request → TOTP 2FA → **15-minute confirmation window** (email/SMS/push token) → anti-fraud tiering `<$10K auto / $10K–$50K standard / >$50K PENDING_REVIEW + 4h` → sanctions/velocity checks (PAMM investments/redemptions use internal transaction types `PAMM_INVEST`/`PAMM_REDEEM` and are strictly excluded from fiat cash limit accumulators, Task 14.3.8) → queued per rail cut-off → bank dispatch → MT900/910 confirmation → status SETTLED. Guards: 30-min same-bank cooldown, 24-h hold on new bank accounts, per-account/hourly/exchange-wide caps, scoped kill-switch (Task 11.3.8).

### B3. Funding Reconciliation (Phase-13/24)
Daily ingestion of **MT940 / MT942 / camt.053** statements (Task 24.3.12) → match by SWIFT reference/amount/currency/value date → tolerance $1,000 or 0.01% → unmatched to ops exception queue.

### B4. Sub-Account Internal Transfers (Phase-03/05)
Internal transfers between master and sub-accounts or between sub-accounts route via `DoubleEntryLedgerService::postJournal()`, executing under `SERIALIZABLE` isolation with balanced DEBIT/CREDIT ledger lines; commit atomically dispatches `BalanceChanged` event to NATS JetStream for real-time WebSocket sync (Task 5.3.23, Task 3.3.6). Account-scoped idempotency keys enforce deduplication with 23505 conflict payload replay.

---

## C. Risk & Market Integrity

### C1. Margin Call → Liquidation (Phase-19)
1. `margin_utilization ≥ 0.90` → `MarginCallNotified` + email/in-app; Redis `margin_call:{account_id}` 15-min deposit window.
2. Unrestored after 15 min → position queued for liquidation (not immediate close); the margin-call order block persists until cure (margin level back above the call threshold) or audited Risk Manager re-enable — spec §13.3 precedence (stop-out voids any open window).
3. **Scanner (2s cadence)** sweeps CROSS/PORTFOLIO accounts (state in `liquidation:scanner:state:{shard}`).
4. Position ≤ 1% of symbol OI → direct close; **> 1% → auction**.
5. Auction: CALL **5s** (LP broadcast) → FILL best bid/offer within floor (×0.98/×1.02 of `liquidation_price`) → EXTEND ≤ 60s total in 5s steps when unfilled > 50%, floor decays 0.5%/step → still unfilled: FORCE_CASH at mark ×0.95/×1.05, deficiency to **insurance fund**; LP filler earns **0.05% rebate from insurance fund**.
6. Insurance fund depleted → **ADL** by profits/leverage ranking.
7. Retail: NBP floor at zero equity — residual absorbed by fund, never debited (`NEGATIVE_BALANCE_PROTECTED`, Task 19.3.9).
8. Stale oracle during liquidation: fallback policy + flash-crash breaker (Task 19.5.3.6) — never liquidate off a stale mark.
9. Cross-currency portfolio required margin is converted to base USD numeraire (`required_margin_quote × rate_to_usd`) before summing into `used_margin`; mark prices are batch-loaded in $O(N)$ linear time without per-position N+1 queries (Tasks 19.3.1/19.3.16).
10. Liquidation worker mutex contention: `ConsumeLiquidationQueue` releases job with exponential backoff on mutex lock contention (`lock:liquidation:account:{account_id}`) instead of completing/dropping, clearing dedup key and alerting `LIQUIDATION_WORKER_LOCK_TIMEOUT` on retry limit exhaustion (Task 19.3.27).

### C2. Portfolio Margin Across Shards (Tasks 2.3.12 / 19.3.11)
Risk Coordinator computes global IM/MM (delta-normal correlation matrix), broadcasts synchronized margin buffers to shards at <1ms cadence; order on Shard A with positions on B/C → two-phase headroom reservation over Aeron (500µs RPC budget → immediate pessimistic floor; >10ms hard deadline cancels/compensates the in-flight reservation — spec §13.1) → shards reject leverage-increasing orders on the standalone floor.

### C3. Collateral & P&L Conventions
Non-settlement currencies haircut per `collateral_schedule` + concentration cap (Task 19.3.8). P&L kept in quote currency; account equity in `base_currency` via oracle mid (Task 3.3.9); reporting currency is display-only.

### C4. Circuit Breaker (Phase-13)
Five tiers — INSTRUMENT (±5%/60s), ACCOUNT (3 rapid losses), VOLUME_SPIKE (z≥4.0σ), OPTIONS_VOLATILITY (IV +200%), MARKET_WIDE (>20% on >2 instruments). States `CLOSED → OPEN → HALF_OPEN → CLOSED`; probes 10/10 in 30s recover; manual reset dual-control for ACCOUNT/MARKET_WIDE.

### C5. Degradation Modes (Phase-02 ModeManager owner)
`Normal · ReadOnly · MarketDataOnly · SpotOnly · Throttled · Maintenance` — triggers, behaviors, and recovery per spec §2.4; surfaced in `X-Degradation-Mode` header + WS `system.status`; P2 for ReadOnly/Throttled, P1 for MarketDataOnly/SpotOnly. OTR throttling (RTS 9, Task 13.3.6) runs orthogonally at 500:1 default per 60s window.

---

## D. Market Lifecycle & Admin

### D1. Instrument State Machine (Phase-15)
`DRAFT → ACTIVE → CANCEL_ONLY/SUSPENDED/HALTED/RESTRICTED → ACTIVE → DELISTED`. CANCEL_ONLY persistently permits cancels only and preserves resting orders; SUSPENDED gives a **5-min cancel-only grace** before forced cancellation; RESTRICTED = limit-only. **HALTED → ACTIVE and the weekly 21:00 UTC Sunday open always reopen via call auction** (5-min accumulation, indicative uncross on WS, single max-volume uncross) — Task 15.3.6. Trade bust / price-adjust for obvious errors with dual control — Task 15.3.5.

### D2. Admin Operations (Phase-07/11/15)
RBAC 6 roles; dual control (2 distinct approvers ≤ 15 min) for: instrument create/delist/resume, global halt, kill-switch (global and scoped account/FIX-session/instrument), settlement exceptions, replenishment, trade bust. Every admin action → tamper-evident audit log.

### D3. User Onboarding & KYC (Phase-12/14)
Register (bcrypt, email verify) → TOTP 2FA (candidate secret staged in `users.two_factor_pending_secret` / Redis `2fa:pending:{user_id}`, preserving active 2FA until confirmed, Task 12.3.2) / WebAuthn Passkeys (assertion elevates `two_factor_verified = true` + JWT AMR `fido2`, Task 12.3.7) → KYC tiers **T0/T1/T2** (+ institutional manual review) with per-tier limits → MiFID II `client_category` (RETAIL/PROFESSIONAL/ECP) with appropriateness tests for complex products and Annex II tests for upgrades (Task 14.3.7) → PEP/adverse-media screening at onboarding with EDD routing (Task 21.3.11) → periodic re-verification. PAMM/MAM & copy trading mandate additional manager onboarding (Task 14.3.8).

### D4. Notifications (Phase-12)
Event → per-user channel preference (email/SMS/push) + quiet hours → send with exponential backoff (max 5) → dead-letter to PostgreSQL → delivery tracking per §24 #100.

---

## E. Recovery & Operations

### E1. Core Recovery (Phase-04 target, verified Phase-04.5)
Load FlatBuffers snapshot → replay WAL from `snapshot_seq+1` → assert `book_seq == WAL tail` (on mismatch: graduated recovery ladder, spec §3.5 — WAL repair → snapshot rebase → halt last resort with `recovery_reports` row) → skip applied seqs (idempotent) → leader lock acquire → resume. Target **< 10s, zero dup/miss**; warm follower tail-reads for standby.

### E2. WAL Archive (Phase-04)
Segment close → S3 upload → **ETag verify** → local trim (never trim past last archived segment — zero-loss guard) → 90d → Glacier, CRR to DR region → `exchange:replay-from-archive`.

### E3. Go Service Deploy (Phase-09)
Helm blue-green → `/ready` gate → traffic switch → monitored soak → rollback script on SLO breach; feature flags 1% → 10% → 100%.

### E4. C++ Engine Binary Upgrade (Task 9.3.16 — 8 steps)
Pre-flight (SHA256, hugepages, affinity, config hash) → announce `Maintenance` + drain (≤5s) → WAL flush + `wal_tail_seq == book_seq` + synchronous snapshot → release leader lock, close Aeron → versioned symlink binary swap on pinned cores → snapshot load + WAL tail replay + book CRC verify → reacquire leadership → synthetic probe orders → resume traffic. Shards sequential, ≥60s apart; rollback = revert symlink + restart.

### E5. Chaos Gate (Phase-04.5) — 6 scenarios × 3 runs
Crash mid-WAL-fsync · IPC timeout + safe requeue · stale snapshot full replay · trimmed WAL → PITR fallback · PID conflict fail-closed · warm recovery after leader crash. All must pass before Phase-8 starts.

### E6. Incident Workflow (Task 9.3.18)
P0 (L0 Critical: matching panic/WAL corruption/data loss/breach): 15-min ack, 1-h mitigation, CTO + regulator (DORA 4h/72h/1-month reports). P1 (L1 Systemic: shard slow/Sentinel failover/oracle staleness): 30-min/4-h. P2 (L2 Transaction: margin timeout/CLS break/DLQ spill): 2-h/24-h. P3 (L3 Edge: rate violation/IP ban): next business day. P0/P1 → status page ≤ 30 min, client email ≤ 1h, **post-mortem ≤ 48h**. Material incidents cannot close until DORA evidence complete (Task 9.3.15). Every alert of the 47+ catalog links its runbook (Phase-13/13.5).

### E7. Clock Discipline (Task 9.3.12)
PTP IEEE 1588v2 hardware sync ≤ 100µs of UTC (MiFID II RTS 25); `phc2sys`/`ptp4l`; `clock_offset_nanoseconds` metric; drift > 100µs → P1.

### E8. End-to-End Crash & Disaster Recovery Orchestration (Task 4.3.10, Spec §18.6)
5-phase recovery state machine (`NORMAL` → `CRASH_DETECTED` → `FENCED_INGRESS_CLAMP` → `STANDBY_PROMOTION` → `WAL_REPLAY_LADDER` → `DATA_INTEGRITY_AUDIT` → `CANCEL_ONLY_GRACE` → `REOPENING_AUCTION` → `NORMAL`) · split-brain fencing via Redis 64-bit monotonic lease epoch (`epoch_local == epoch_current`) · single-shard local crash recovery with 4KB memory-aligned dirty WAL flush, standby promotion within RTO $\le 3\text{s}$, RPO = 0 · multi-region site disaster recovery with PostgreSQL semi-sync promotion, Redis Sentinel master promotion, S3 WORM binary WAL archive replay catch-up (`exchange:replay-from-archive`), and Anycast DNS edge failover within RTO $\le 5\text{min}$, RPO $\le 15\text{s}$ · mandatory pre-open 6-stage mathematical data integrity audit (total balance zero-sum conservation $\sum\text{Debits} == \sum\text{Credits}$, settled + house + insurance == nostro clearing cash; non-negative balances $\text{balance}_i \ge 0$; deterministic book-WAL equivalence $\text{book\_seq} == \text{wal\_tail\_seq}$; monotonic execution sequencing without orphan fills; MT942/camt.053 external bank statement reconciliation; CLS PvP gross settlement confirmation) · client resync and order book reopening ladder (60s `CANCEL_ONLY` grace period, FIX `TradingSessionStatus` 35=h broadcast + `ResendRequest`/`SequenceReset` gap fill, WebSocket `{"action":"resume"}` 60s ring buffer replay, 5s Call Auction uncrossing).

### E9. Error Handling & Fault Isolation Hierarchy (Spec §2.7)
The system enforces **Strict Fail-Closed Zero-Loss Pessimism** across four hierarchical severity tiers:
- **L0 (Critical / Fatal Fault):** Triggered by WAL CRC failure, zero-sum ledger imbalance, PTP clock skew >100µs, matching loop panic, memory corruption. Handling: immediate core halt (`SIGTERM`), dump crash diagnostics with WAL offset, flush memory-aligned buffers to disk, failover to warm standby via WAL replay ladder, P0 pager alert.
- **L1 (Systemic / Infrastructure Degradation):** Triggered by Redis Sentinel failover, IPC ring buffer >80% watermark, PostgreSQL replica lag >5s, Price Oracle staleness >5s. Handling: automated degradation mode transition (`Normal` $\rightarrow$ `ReadOnly` / `Throttled` / `Maintenance`), circuit breaker tripping, load shedding of market orders, hysteresis recovery (30s consecutive healthy telemetry required before returning to `Normal`).
- **L2 (Transaction / State Boundary Error):** Triggered by margin shortfall, negative balance breach, counterparty credit exhaustion, STP self-match, cross-shard 2PC timeout (>10ms). Handling: synchronous atomic rejection, zero side-effects, automatic rollback of optimistic reservations, compensation event emission, structured error envelope to client.
- **L3 (Edge / Protocol Validation Rejection):** Triggered by malformed JSON/SBE, HMAC signature mismatch, replay outside 30s window, rate-limit tier breach, unauthorized IP. Handling: fast rejection at API/FIX gateway edge before reaching internal IPC or matching core, metrics counter increment, progressive IP ban escalation on repeated offenses.

---

## F. Compliance

### F1. Sanctions & AML Pipeline (Phase-21)
Screening on **registration, deposit, withdrawal, trade** (in-core `SanctionsHook`, Task 21.3.10) against OFAC/EU/UN/UK-HMT with daily list updates; timeout ⇒ fail-closed (scoped degradation per AC #43, amended — not full ReadOnly). Ongoing monitoring rules (structuring, velocity, dormant reactivation) + PEP/adverse media → EDD → **SAR** drafting → Compliance Officer review → dual-control filing. CTR auto-filed ≥ $10,000. Travel rule: originator/beneficiary in MT103 50K/59 for ≥ $1,000.

### F2. Market-Abuse Surveillance (Phase-17 emits → Phase-21 enforces)
Wash / layering / spoofing / front-running / insider dealing / marking-the-close / momentum-ignition signals computed off L3 stream → enforcement ladder: warn → throttle → restrict → suspend, with case workflow.

### F3. Transaction & Transparency Reporting (Phase-21)
MiFID II **RTS 22** to ARM by T+1 23:59 UTC (ISO 20022 auth.016); **APA RTS 1/2** post-trade < 1 min; **RTS 27/28** best-execution (Task 21.3.19); EMIR REFIT + CFTC Parts 43/45 lifecycle events (UTI/USI/UPI, valuation/margin/termination, Task 21.3.14) — NACK → repair queue with immutable resubmission; submissions tracked in `regulatory_submissions`.

### F4. Venue Governance & Privacy (Phase-21)
Member/DEA admission + annual review, rulebook approvals, emergency actions, CCO annual report (Task 21.3.15); FX Global Code 55-principle self-assessment + public Statement of Commitment (Task 21.3.17); GDPR rights (access/erasure/portability) with financial-retention carve-outs; **data residency enforcement** by `jurisdiction_code` — EU/UK data stays in-region, DR replication respects SCCs, keys stay in regional HSMs (Task 21.3.18).

---

## G. Backoffice & Derivatives

### G1. Trade Settlement (Phase-03/24)
Execution → settlement instruction (T) → value date per calendar engine (T+1 default; T+2 exotics; same-day USD/CAD, USD/MXN; ISDA Modified Following across base/quote/USD calendars, Task 3.3.8) → dispatch (SWIFT MT202/pacs.009 or CLS) → nostro debit/credit → MT900/910 confirm → SETTLED → daily reconciliation → RECONCILED. No confirmation in 2 business days → P2 alert.

### G2. CLS PvP (Task 24.3.8)
Eligibility from current CLS reference data (currency/product/member/value-date/cut-off — never hard-coded) → paired instructions via member's **ISO 20022** interface → persist CLS statuses (match/eligibility/pay-in/settle/rescind/reject; exceptions to queue before cut-off) → internal settlement posted **only from authenticated CLS finality notifications**. Ineligible flow follows the FX Global Code waterfall: alternative PvP → enforceable netting → controlled gross with principal-risk limits.

### G3. EOD Rollover (Task 3.3.7)
21:00 UTC daily: open spot margin positions roll Tom-Next; swap points from PriceOracle yield curves (Task 19.5.3.5); financing posted via GL double-entry; **Wednesday = 3× rollover**. 

### G4. Netting, Allocation, Client Money (Phase-24)
SSI registry defaults settlement instructions (Task 24.3.9); bilateral netting per currency per value date into `payment_netting_batches`; bunched orders → average price → post-trade allocation with claim/reject/correct (24.3.10); client-money segregation with daily internal+external reconciliation and dual-control shortfall remediation (24.3.11); CSDR fails/penalties/buy-in discipline (24.3.13); PB give-up reconciliation + breaks (24.3.7) with **credit restitution on settlement failure** (24.3.14). Position transfers (give-ups, allocations, admin adjustments) atomically release `balances.locked` on source and calculate/lock initial margin on destination in the same `SERIALIZABLE` transaction (Task 19.3.12, Task 24.3.7).

### G5. Derivatives Lifecycle (Phase-22)
Forward/swap pricing off yield curves with ACT/360 vs ACT/365 per instrument (Task 22.3.1, amended); NDFs cash-settle at fixing; options: premium paid T+2 via GL, exercise cutoff 15:00 UTC expiry day, auto-exercise ≥ 0.5% ITM unless do-not-exercise, random writer assignment; barrier/binary via Monte Carlo; VM daily, UMR IM per ISDA-SIMM-consistent schedule; orders blocked `LEGAL_DOC_REQUIRED` without executed ISDA/CSA/give-up (Task 22.3.11); multi-leg implied matching (Task 22.3.12).

### G6. Analytics & Attestation (Phase-20/13)
Tick stream → ClickHouse (daily partitions, TTL 90d raw / 5yr aggregates) → OHLCV, P&L (quote + base-currency views), statements/confirmations/invoicing (Task 20.3.6). Pre-materialized klines contract: historical chart endpoints and TradingView UDF `/history` query pre-materialized `fx_klines` tables; runtime scans on `fx_trades` or dynamic in-memory candle aggregation are strictly prohibited (Task 6.3.8, Task 23.3.1). Daily 22:00 UTC Merkle proof-of-reserves: leaf `SHA256(account_id|nonce|balance|currency)`, root to `audit_merkle_roots` + public signed endpoint, per-account inclusion proofs (Task 13.3.7).

---

## H. Development & Change Workflow (docs-as-code)

1. **Spec is the contract.** Any plan-vs-spec divergence is resolved in spec §27 (decision log) — never by silent drift.
2. Phase files change by **append-only task numbering**; AC tables stay sequential; superseded values keep "(supersedes …)" notes; bulk edits verified by `rg -F` sweeps + stated-count audits.
3. CI (Phase-01.5): PR → unit + lint + **543 spec checkpoints across 479 tasks** (400+ stated; remediation #37 recount supersedes 530/466, 527/463, 523/461, 522/460, 514/452, 510/448, 506/444, 501/439, 487/425, 485/423, 482/420, 480/418, 468/406, 467/405, 466/404, 422/360, 407/346, 377/316, 373/312, 358/297, 369/323) in 4 shards < 20 min + supply-chain scans; integration on ephemeral services; staging load + chaos; production blue-green.
4. **Phase gates are hard stops** (`AGENTS.md` gate table): soak 72h/50k/sec · chaos 6×3 · staging 75k/sec + 10,000 WS · pen test 0 Critical/<3 High · final gate = **419/419 §24 criteria with executable passing tests** and zero unmapped traceability rows (supersedes 418/418, 414/414, 401/401, 398/398, 390/390, 389/389, 381/381, 377/377, 373/373, 368/368, 354/354, 352/352, 349/349, 347/347, 335/335, 334/334, 333/333, 296/296, 276/276, 256/256, 252/252, 237/237, 206/206).
5. Current position: Phase 1 (C++ Core Foundation) is next; nothing is built yet (Day-0 baseline: 13 operational domains, 133 components at 100% specification & plan depth, 0% application code on disk, remediation #39).
