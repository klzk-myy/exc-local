# CONTEXT — FOREX Exchange System Suite

**Read this first.** This file is the onboarding briefing for the repository: what the project is, what state the repo is in, which document to consult for what, and the rules that govern every change here.

---

## 1. What This Project Is

A **Tier-1 institutional Foreign Exchange (FX) exchange**: spot, forwards, swaps, NDFs, and options across fiat currency pairs (USD, EUR, GBP, JPY, AUD, CAD, CHF, MXN, BRL, …).

- **Fiat currencies only.** No cryptocurrency, no blockchain, no crypto custody. Funding and settlement move over banking rails (SWIFT, SEPA, FedNow, ACH, CHAPS, TARGET2; CLS PvP for eligible pairs).
- **Institutional scope:** REST/WebSocket/FIX with JSON and negotiated SBE, Ed25519/RSA keys, delegated client roles/M-of-N controls, prime-brokerage give-ups, credit-screened liquidity, MiFID II / EMIR / Dodd-Frank / FinCEN compliance, and 24/5 trading (Sydney open 21:00 UTC Sunday → New York close 22:00 UTC Friday).
- **Deliberately out of scope** (spec §6.4): RFQ, RFS, indicative quoting, last-look. All liquidity is firm.

## 2. Repository State

- **Planning-stage repo: documentation only.** There is no application code. The deliverable in this repo is the documentation set.
- **Git:** initialized, **zero commits** — everything is untracked.
- **Next actionable unit of work:** **Phase 1 (C++ Core Foundation)** per `docs/Phase-01-Project-Foundation.md`.
- **Completeness Assessment & Day-0 Baseline (Remediation #39):** Comprehensive assessment across 13 operational domains and 133 components confirms 100% specification and planning depth with 0.0% physical application code on disk. Business boundary rulings R14–R18 ratified in Spec §27.
- **All root meta-docs are canonical:** Historical domain analysis from the planning stage has been consolidated into `DESIGN.md` (Section 10 scope boundaries) and `ARCHITECTURE.md` (Section 18 feature map). No scratch or helper files exist in the repo.

## 3. Document Map — Which File Answers What

| File | Role | Use it for |
|---|---|---|
| `MEMORY.md` | **Authoritative project memory & operational directives** | Zero-drift canonical counts, load-bearing invariants, mechanism ownership, next steps |
| `docs/Specification - Complete Exchange System Suite.md, spec §5.3/§5.21 (ledger/wallet/balance)` | **The contract (v7.0)** — 27 sections, 419 acceptance criteria (§24; supersedes prior 418, 414, 401, 398, 390, 389, 381, 377, 373, 368, 354, 352, 349, 347, 335, 334, 333, 296, 276, 256, 252, 237, 206), decision log (§27) | What must be true. Phase plans must conform to it |
| `AGENTS.md` | **Master plan & working rules** — 30-phase index, dependency graph, hard gates, canonical values, mechanism ownership map | How work is sequenced; the rules for editing docs |
| `CLAUDE.md` | Agent quick reference | Compressed pointer to the above |
| `ARCHITECTURE.md` | System architecture — topology, components, flows, deployment, tech rationale | Where things live and how data moves |
| `DESIGN.md` | Engineering design decisions & invariants | How the system is built and why (data structures, protocols, numerics, failure semantics) |
| `WORKFLOWS.md` | End-to-end workflow catalog | Every operational flow — trading, money movement, risk, lifecycle, recovery, compliance, backoffice — with owning phase |
| `CONTEXT.md` | This file | Orientation; reading order; how to start |
| `docs/Phase-NN-*.md` | 30 phase plans (24 core + 6 buffer) | Executable task lists per phase |

**Reading order for a new contributor:** this file → `MEMORY.md` → `AGENTS.md` (rules + index) → spec §1–§2 (scope + architecture) → the phase doc for your assigned phase → `ARCHITECTURE.md` / `DESIGN.md` as needed.

## 4. Stack (Planned)

| Layer | Technology | Notes |
|---|---|---|
| Matching core | C++17/20, bare metal, NUMA-pinned | One process per shard; no containers in hot path |
| Services | Go 1.23+, Kubernetes | Single binary per service, REST/WS/FIX |
| IPC (hot path) | Aeron (C media driver) or shared-memory ring | Never HTTP/gRPC in hot path |
| OLTP | PostgreSQL 16 | `SERIALIZABLE` for balance mutations; pg_partman |
| Analytics | ClickHouse | MergeTree, 90d raw / 5yr aggregate TTL |
| Cache/coordination | Redis 7 (+Sentinel) | Sessions, rate limits, locks — **never** the order book, never the WAL |
| Frontend | React 18 + TypeScript | TradingView Lightweight Charts |
| Secrets | Vault/KMS | (Phase-13.5 Task 13.5.3.5) |

## 5. Plan Structure

- **30 phases = 24 core + 6 buffer.** Buffer phases (1.5 CI/CD harness · 2.5 soak · 4.5 recovery chaos · 8.5 pre-prod load · 13.5 security audit · 19.5 price oracle) are **non-negotiable** — they are the difference between "plan" and "delivered".
- **Every phase gate is a hard stop.** If gate criteria do not pass, the next phase does not begin. Gate table lives in `AGENTS.md`; the load-bearing ones:
  - **2.5 → 3/4/5/6:** 72h soak at 50k/sec sustained, p99 ≤ 50µs, memory < 75%, zero WAL lag, recovery < 10s with zero duplicate/missing trades.
  - **8.5 → 9:** staging 75k/sec, replica lag < 2s, N≥10,000 WS connections zero drops, mid-run recovery drill, zero alerts.
  - **13.5 → 14/15/24:** pen test 0 Critical / <3 High; PII audit 0 leaks; 47+ alert runbooks, 4 tabletops < SLA.
  - **24 → production:** all **419** §24 criteria have passing executable tests (supersedes prior 418, 414, 401, 398, 390, 389, 381, 377, 373, 368, 354, 352, 349, 347, 335, 334, 333, 296, 276, 256, 252, 237, 206); zero unmapped traceability rows; legal/venue/CLS/bank attestations complete.
- **Staffing assumption:** 7–10 developers (2–3 C++ engine, 2–3 Go services, 1 DevOps/SRE, 1–2 QA) + dedicated compliance officer by Phase 21.

## 6. Canonical Values (Do Not Drift)

If you ever find a conflicting value anywhere in the repo, **these win** and the conflict is a defect (fix the old text, keep a "(supersedes …)" note). Authoritative list lives in `AGENTS.md`; most load-bearing entries:

| Topic | Canonical value |
|---|---|
| p99 tick-to-trade latency | **≤ 50µs** (supersedes ≤ 1ms) |
| Throughput | 50,000 orders/sec per shard; 75k/sec staging gate |
| §24 acceptance criteria | **419** (supersedes 418, 414, 401/398/390/389/381/377/373/368/354/352/349/347/335/334/333/296/276/256/252/237/219/206/201/192/174); CI checkpoints 400+ (**actual 543 across 479 tasks**, remediation #37 mechanical recount; supersedes 530/466, 527/463, 523/461, 522/460, 514/452, 510/448, 506/444, 501/439, 487/425, 485/423, 482/420, 480/418, 468/406, 467/405, 466/404, 422/360, 407/346, 377/316, 373/312, 358/297, 369) |
| Withdrawal confirmation window | **15 min** |
| Data retention schedule | spec **§19.12** (enforcement owner Phase-09 Task 9.3.22, remediation #18): order/trade/surveillance records 5y (MiFID II RTS 6) · comms recordings 5y (Art. 16(7)) · ClickHouse raw ticks 90d then aggregate · OHLCV 5y · finance/house reports 7y · KYC docs account lifetime + 5y · audit logs 7y; hot→warm→cold tiers; Art. 17(3)(b) legal-hold carve-outs; nightly enforcer raises `RETENTION_POLICY_VIOLATION` (P2) on drift |
| Error-code registry | spec **§23**, **182 codes** (supersedes 181/180/176/173/172/170/149 — Phase-13.5 landing registered `VDP_*` codes (Task 13.5.3.8); Phase-12 landing registered `INVALID_CREDENTIALS` (Task 12.3.1); Phase-11 implementation landing registered `BANKING_RAIL_UNAVAILABLE` (Task 11.3.1), `FUNDING_FEE_EXCEEDS_AMOUNT` (Task 11.3.9), `BENEFICIARY_HOLD_ACTIVE` (Task 11.3.7) and `WITHDRAWAL_WHITELIST_ONLY`/`WITHDRAWAL_WHITELIST_LOCKED`/`WHITELIST_CHANGE_LOCKED`/`NOSTRO_INSUFFICIENT_FUNDS` (Tasks 11.3.2/11.3.3/11.3.6/11.3.10); `THIRD_PARTY_DEPOSIT_REJECTED` was amended in place, not added; earlier supersedes — remediation #44 registered the 21 gateway `localRow` emissions + follow-on `INVALID_DEPTH_LIMIT`/`INVALID_INTERVAL`, remediation #37 added `RAIL_CUTOFF_EXCEEDED`, `BILATERAL_CREDIT_EXHAUSTED`, `DISCRETIONARY_OFFSET_INVALID`, `ISOLATED_MARGIN_DEFICIT`); Phase-05 Task 5.3.21 is the enforcement owner · every code is **owner-resolvable** — either emitted by a phase plan or cited with its owning `Phase-NN Task N.N.N` in §23; `CORPORATE_ACTION_SCHEDULED` is `reserved, never emitted` (no corporate actions in fiat spot FX); CI asserts zero ownerless codes (remediation #19) |
| Withdrawal/deposit review tiers | `<$10K auto / $10K–$50K standard / >$50K PENDING_REVIEW + 4h` |
| Degradation modes (exact casing) | `Normal · ReadOnly · MarketDataOnly · SpotOnly · Throttled · Maintenance` |
| Liquidation scanner / auction | **2s** scanner; trigger OI > 1%; CALL **5s**, EXTEND ≤ 60s; floors ×0.98/×1.02 of liquidation_price with 0.5% decay per 5s EXTEND; FORCE_CASH ×0.95/×1.05; LP rebate **0.05% from insurance fund** |
| DR targets | Redis RPO ≤ 5s / RTO ≤ 30s; PostgreSQL RPO ≤ 15s / RTO ≤ 5min |
| RBAC | Super Admin · Risk Manager · Compliance Officer · Finance Ops · Support Agent · Read-Only Auditor |
| KYC | T0 / T1 / T2 (+ institutional manual review) |
| FX settlement | **T+1** most spot; **T+2** exotics; **same-day** USD/CAD, USD/MXN |
| Leverage (retail) | ESMA 30:1 major / 20:1 minor / 10:1 exotic; CFTC 50:1 major / 20:1 minor |
| Trading hours | **24/5** |
| Price oracle | Refinitiv / Bloomberg BFIX / ECB, ≥ 2 independent sources, **5s staleness gate**, fail-closed |
| Financial correctness categories | **9** acceptance categories (supersedes 8 — GL zero-sum balance-sheet invariant added) |
| Evaluated operational domains | **13 domains** (**133 components**, 100% spec & plan depth, 0% code, remediation #39) |
| Error severity hierarchy | **L0** Critical/P0 (Core halt/WAL replay) · **L1** Systemic/P1 (Degradation mode) · **L2** Transaction/P2 (Atomic rejection/rollback) · **L3** Edge/P3 (Gateway rejection/IP ban) — overarching invariant: **Strict Fail-Closed Zero-Loss Pessimism** (spec §2.7) |

## 7. Working Rules (Docs Are Code Here)

1. **The Specification is the contract.** Plans conform to it. A strictly-better plan design is recorded in spec §27 — never silently diverged.
2. **Append-only task numbering** in phase files: new work gets the next sequential task number; never renumber. AC tables (`## N.7`) stay strictly sequential; if you append rows, update stated counts in the same file.
3. **Superseded values keep an audit trail:** the old location gets an explicit "(supersedes/replaces prior …)" note. Bare contradictions are defects.
4. **One owner per mechanism.** Before adding a feature, check the mechanism ownership maps in `AGENTS.md` / `ARCHITECTURE.md §18` — e.g., routes → Phase-05, degradation modes → Phase-02, circuit breaker → Phase-13, liquidation/ADL/PB credit → Phase-19, oracle → Phase-19.5, KYC → Phase-14, surveillance signals Phase-17 → enforcement Phase-21, banking rails → Phase-11, nostro/CLS → Phase-24.
5. **Verify mechanically after bulk edits:** `rg -F "<term>" docs/*.md` sweeps for every new key term + stated-count vs actual-row audit per touched file.
6. **Always update meta-docs whenever the need arises:** Any modification to specifications, phase tasks, endpoints, schemas, or canonical counts must be immediately propagated across all root meta-docs (`AGENTS.md`, `CLAUDE.md`, `CONTEXT.md`, `ARCHITECTURE.md`, `DESIGN.md`, `WORKFLOWS.md`, `MEMORY.md`). Never leave meta-docs stale when underlying plans evolve.

## 8. Hard Technical Constraints (Carry Into All Code)

- C++ core on **bare metal**, exactly **one matching thread per shard**, zero allocations in the hot path, deterministic replay.
- Core↔services IPC via **Aeron / shared memory** — never HTTP/gRPC in the hot path.
- **Custom binary WAL** (mmap + O_DIRECT-aligned fsync batches) — not Redis Streams, not PostgreSQL WAL for the book.
- PostgreSQL **`SERIALIZABLE`** for every balance mutation and trade settlement.
- **ClickHouse** for tick history; **Redis** for sessions/rate-limits/locks only — never the order book.
- FIX is **FX-specific** (NoPartyIDs, SettlementType, FX tags) — not equities FIX.
- **Strict Fail-Closed Zero-Loss Pessimism**: Any ambiguous, corrupt, or invariant-violating state triggers immediate deterministic fail-closed behavior, structured audit emission, and graceful system degradation — never lost client funds, corrupted balances, or non-deterministic execution (spec §2.7).

## 9. How to Start Work

1. Read this file and `AGENTS.md`.
2. Open the current phase doc (today: `docs/Phase-01-Project-Foundation.md`) and its spec cross-references.
3. Confirm the entry gate for that phase has passed (see `AGENTS.md` gate table; Phase 1 has no predecessor).
4. Implement tasks in order; every task ends tied to its acceptance-criteria rows (`## N.7` in the phase doc).
5. Before hand-off, run the phase's gate checklist — the gate is a hard stop.
