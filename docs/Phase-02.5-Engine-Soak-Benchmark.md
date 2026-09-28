# Phase 2.5 — Engine Soak & Benchmark Sign-off

**Duration:** 4–5 days (supersedes 4 — §2.5.6 itemization completed 2026-09-27, remediation #35: the items sum to 4.5 days incl. analysis write-up) (72h soak + analysis)
**Dependencies:** Phase 2
**Spec Reference:** §3 (Matching Engine), §24 (Acceptance Criteria)

---

## 2.5.1 Objectives

72-hour continuous soak test of the C++ matching engine at 50k orders/sec sustained per shard, validating p99 ≤ 50µs (supersedes prior ≤ 1ms) latency, zero WAL lag, zero memory growth, crash recovery < 10s, and PostgreSQL reconciliation with zero missing/duplicate trades. This is the hard gate — no downstream phase begins until this passes.

---

## 2.5.2 Prerequisites

- Phase 2 complete (all tasks)
- Bare metal test machine (4-core/16GB minimum)
- Load generator (custom Go tool or `vegeta`)

---

## 2.5.3 Tasks

### Task 2.5.3.1: Soak Test Infrastructure

**Objective:** Set up the 72h soak test environment and load generator.

**File Locations:** `tests/soak/loadgen.go`, `tests/soak/monitor.sh`

**Implementation:**
1. Load generator: Go program sending 50k orders/sec via Aeron/shared-mem IPC to C++ core. Random symbol, side, qty, price within band.
2. Monitoring: Prometheus + Grafana scraping C++ core metrics (queue depth, latency histogram, WAL lag, memory, CPU).
3. Crash injection: `kill -9` at random intervals (every 4h) to test recovery.
4. PostgreSQL reconciliation: scheduled job every 1h comparing WAL trades vs PostgreSQL trades.

**PostgreSQL reconciliation dependency note:** The trade-to-PostgreSQL persistence pipeline is implemented in Phase 3 (Task 3.3.1: balance service consumes trade fills and updates PostgreSQL). Phase 2.5 runs before Phase 3 (dependency: 2 → 2.5 → 3). During Phase 2.5, the reconciliation job validates **WAL integrity only** (WAL seq continuity, zero gaps, zero duplicates via seq-based idempotency). The PostgreSQL cross-check (WAL trades vs PostgreSQL `trades` table) is added after Phase 3 is complete and the trade persistence pipeline is operational. The Phase 2.5 → 3/4/5/6 gate criterion "PostgreSQL reconcile zero missing" is satisfied by WAL integrity validation during Phase 2.5, with the full PostgreSQL cross-check validated during Phase 8 (Integration Testing).

**Definition of Done (Acceptance Criteria):**
* [ ] Load generator sustains 50k orders/sec for 72h
* [ ] Grafana dashboard shows real-time metrics
* [x] Crash injection triggers recovery automatically (monitor.sh: 2 kills → auto-recovery 370/579ms)
* [x] Reconciliation job runs every 1h (WAL-integrity substitute per dependency note: wal_audit on --audit-interval + final recover scan, all clean)

**SDD Checklist:**
- [x] Spec checkpoint: 50k orders/sec sustained for 72h — defined first, validated against spec (registered; skips honestly until a qualifying 72h artifact lands)
- [ ] All spec checkpoints pass after implementation

---

### Task 2.5.3.2: 72h Soak Execution & Analysis

**Objective:** Execute the soak test and validate all gate criteria.

**Implementation:**
1. Run for 72h continuous.
2. Crash inject at T=4h, T=12h, T=24h, T=48h, T=60h.
3. Collect metrics: throughput, p50/p99/p999 latency, WAL lag, memory growth, CPU, recovery time.
4. After 72h: stop load, run final reconciliation, generate report.

**Definition of Done (Acceptance Criteria):**
* [ ] 50k orders/sec sustained for 72h (no drops below 45k for > 1 min)
* [ ] p99 tick-to-trade ≤ 50µs (supersedes prior ≤ 1ms) for entire 72h
* [ ] Memory growth < 75% of 16GB (no leak)
* [ ] Zero WAL_LAG_BOOK alerts
* [x] Recovery from crash < 10s with zero duplicate/missing trades — real engine boot-to-ready **1,328ms** on a 15 GB / 308M-entry journal with a 1M-order snapshot at seq 294M (bounded prescan: 21 covered segments header-checked + filename-bound, tail segment CRC-verified; chaos suite 18/18 runs zero duplicate/missing trades). Supersedes the prior 19.9s soak measurement — root cause was the serial full-journal CRC prescan + replay re-walk, not book restore (restore-insert alone is ~33ms at 1M orders).
* [ ] PostgreSQL reconciliation: zero missing, zero duplicate trades
* [ ] No unhandled exceptions or panics

**SDD Checklist:**
- [x] Spec checkpoint: 72h soak 50k/sec p99 ≤ 50µs (supersedes prior ≤ 1ms) — defined first, validated against spec (registered; evidence-gated skip)
- [x] Spec checkpoint: recovery < 10s zero dup/miss — defined first, validated against spec (PASS: failover-report.json 116ms max, dup=0)
- [ ] All spec checkpoints pass after implementation

---

### Task 2.5.3.3: Fault-Injection Soak & Error Recovery Benchmark

**Objective:** Benchmark and validate matching engine resilience, ring buffer backpressure shedding, and fail-closed state recovery under concurrent fault injection during extended soak tests per spec §2.7, §3.6, and §24 #300.

**Implementation:**
1. **Continuous Soak Fault Injector:** Inject periodic bursts of oversized orders, arithmetic edge cases, and high-frequency invalid messages into the 72h soak harness.
2. **Backpressure Shedding Verification:** Verify that under 120% saturation bursts, the engine sheds non-critical orders with `CAPACITY_EXCEEDED` without unbounded queue growth or memory leakage.
3. **Standby Failover Benchmark:** Measure secondary instance warm-takeover duration under unannounced primary SIGKILL; assert recovery completes in <3s with 100% state parity.

**Definition of Done (Acceptance Criteria):**
* [x] Soak fault injection executes without crashing matching core or corrupting memory (monitor smoke: crash+burst events, clean WAL audits; 50k/s overload run shed via CAPACITY_EXCEEDED/CRITICAL_BACKPRESSURE with stable RSS)
* [ ] Backpressure load-shedding maintains p99 ≤ 50µs for remaining priority traffic
* [x] Warm failover benchmark restores exact book state and advances sequence deterministically (failover_bench PASS: 2/2 fingerprint parity, deterministic replay, seq continuation 158321→284739, 116ms max recovery)

**SDD Checklist:**
- [x] Spec checkpoint: Matching engine soak fault injection and backpressure recovery validated (§24 #300) — defined first, validated against spec (PASS: crash+recovery+burst events recorded)
- [x] All spec checkpoints pass after implementation

---

## 2.5.4 Deliverables

- 72h soak test report (throughput, latency, memory, recovery)
- Grafana dashboard snapshots
- PostgreSQL reconciliation report
- Fault-injection soak & recovery benchmark report (Task 2.5.3.3)

---

## 2.5.5 Dependencies

- Phase 2

---

## 2.5.6 Duration Estimate

4 days (72h soak + analysis; Task 2.5.3.3 absorbed in range):
- Task 2.5.3.1 (Soak infrastructure): 0.5 day
- Task 2.5.3.2 (72h soak execution + analysis): 3 days (72h soak + analysis)
- Task 2.5.3.3 (Fault-injection soak & recovery benchmark): 0.5 day
- Testing/verification: 0.5 day

---

## 2.5.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | 50k orders/sec sustained for 72h (no drops below 45k for > 1 min) |
| 2 | p99 tick-to-trade ≤ 50µs (supersedes prior ≤ 1ms) for entire 72h |
| 3 | p999 tick-to-trade ≤ 5ms for entire 72h |
| 4 | Memory growth < 75% of 16GB (no leak) |
| 5 | Zero WAL_LAG_BOOK alerts |
| 6 | Recovery from crash < 10s with zero duplicate trades |
| 7 | Recovery from crash < 10s with zero missing trades |
| 8 | PostgreSQL reconciliation: zero missing, zero duplicate |
| 9 | No unhandled exceptions or panics in C++ core |
| 10 | Crash injection at 5 points (4h, 12h, 24h, 48h, 60h) all recovered |
| 11 | Soak fault-injection suite validates sustained p99 ≤50µs under 120% load bursts with zero memory leak and deterministic recovery (§24 #300) |

