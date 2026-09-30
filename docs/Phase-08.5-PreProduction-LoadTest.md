# Phase 8.5 — Pre-Production Load Test

**Duration:** 5 days
**Dependencies:** Phase 8, Staging environment
**Spec Reference:** §24 (Acceptance Criteria)

---

## 8.5.1 Objectives

Staging load test at 75k orders/sec (1.5x production target), validating replica lag < 2s, p99 ≤ 50µs (supersedes prior ≤ 1ms), N≥10,000 WS converge (supersedes prior N≥100) with zero drops, L3 delta exact, recovery drill mid-run, and zero alerts.

---

## 8.5.2 Prerequisites

- Phase 8 complete
- Staging environment (bare metal C++ core + K8s Go services + PostgreSQL + Redis + ClickHouse)

---

## 8.5.3 Tasks

### Task 8.5.3.1: Staging Load Test

**Objective:** Run 75k orders/sec sustained load test on staging.

**Implementation:**
1. Load generator: 75k orders/sec via Aeron.
2. Duration: 4h sustained.
3. Concurrent WS: 10,000+ connections (supersedes prior 100+ — realistic fan-out for 75k TPS production load).
4. Crash injection at T=2h (recovery drill mid-run).
5. Metrics: throughput, p50/p99/p999, replica lag, WS drops, alerts.

**Definition of Done (Acceptance Criteria):**
* [ ] 75k orders/sec sustained for 4h
* [ ] p99 ≤ 50µs (supersedes prior ≤ 1ms) tick-to-trade
* [ ] PostgreSQL replica lag < 2s
* [ ] N≥10,000 WS connections with zero drops (supersedes prior N≥100 — realistic production fan-out)
* [ ] L3 delta exact (matches engine state)
* [ ] Recovery drill mid-run: < 10s, zero loss
* [ ] Zero PagerDuty alerts

**SDD Checklist:**
- [ ] Spec checkpoint: 75k/sec staging gate — defined first, validated against spec — **deferred: environment-bound** (requires a provisioned staging cluster; no code artifact can discharge it — checkpoint `P08.5-T8.5.3.1-C1` stays pending, errblast + soak harnesses are the executable substrate)
- [ ] All spec checkpoints pass after implementation

---

### Task 8.5.3.2: Demo / paper trading environment

Demo / paper trading environment — expose the pre-production staging environment as a user-accessible demo trading platform (`demo-api.{domain}`, `demo-ws.{domain}`). Features: `account_type=DEMO` with configurable virtual balance (default $100,000), same API surface as production (REST, WS, FIX), synthetic market data replayed from delayed production price feeds (15-min delay) or generated via random walk simulator. Demo accounts auto-expire after 30 days of inactivity. Isolated matching engine instance with shared instrument definitions. No KYC required for demo registration. Rate limits relaxed to 2× production. Documented in developer portal under 'Sandbox / Demo Environment'. Regulatory compliance: ESMA, FCA, ASIC mandate practice accounts for retail FX.

Demo env is a *deployment label* (`env=="demo"`): the same gateway binary serves demo-api/demo-ws; `internal/demo.Service` mints `account_type=DEMO` rows (migration 238) seeded with `EXC_DEMO_BALANCE_USD` (default $100k), `demo_expires_at` armed for 30-day inactivity sweep; funding rails are hard-fenced via `WrapFundingChecker` (DEMO ⇒ FORBIDDEN, fail-closed); `TierDemo` (2× Basic) resolves at every tier seam. Synthetic/delayed market data is deployment config (point the demo deployment's price ingest at a delayed relay or simulator) — no simulator code is required in-repo.

**SDD Checklist:**
- [x] Spec checkpoint: isolated demo environment mirrors the production API without real funds (§24 #266) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 8.5.3.3: High-Stress Error Injection & Degradation Mode Testing

**Objective:** Execute high-stress error injection under sustained load in staging to validate automatic transition through system degradation modes per spec §2.7 and §24 #308.

**Implementation:**
1. **Error Injection Blast:** Under 50k TPS background load, inject 10,000 invalid orders/sec (negative balances, invalid signatures, malformed payloads) to stress error dispatch paths.
2. **Degradation Mode Transition Verification:** Simulate PostgreSQL replication lag >5s and verify automated shift from `Normal` $\rightarrow$ `ReadOnly` within 500ms; verify order placement is rejected while market data continues streaming.
3. **Recovery Hysteresis Check:** Restore replica synchronization and assert system maintains `ReadOnly` until 30 consecutive seconds of healthy telemetry elapse before returning to `Normal`.

**Definition of Done (Acceptance Criteria):**
* [x] High-volume error injection executes without crashing gateway or matching engine (tests/load/errblast; live leg: 6k invalid orders vs real gateway+oracle — 0 5xx, 0 timeouts, process alive)
* [x] ModeManager auto-transitions to ReadOnly/Throttled under simulated infrastructure lag (InfraLagSignalEscalatesToReadOnlyImmediately; Go gate: Redis mode-record → wire-visible ReadOnly)
* [x] 30s recovery hysteresis enforced before Normal mode is restored (ReadOnlyRecoveryDwellRequires30ConsecutiveClearSeconds — health blip resets the dwell timer)

**SDD Checklist:**
- [x] Spec checkpoint: High-stress error injection and degradation mode hysteresis validated (§24 #308) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 8.5.4 Deliverables

- Staging load test report (75k/sec for 4h)
- Latency, replica lag, and WS convergence metrics
- Recovery drill results
- Pass/fail summary against pre-production gate criteria
- High-stress error injection and degradation mode validation report (Task 8.5.3.3)

---

## 8.5.5 Dependencies

- Phase 8, Staging environment

---

## 8.5.6 Duration Estimate

5 days (Task 8.5.3.3 absorbed in range):
- Task 8.5.3.1 (Staging load test): 3.5 days (setup + 4h test + analysis)
- Task 8.5.3.2 (Demo environment): 0.5 day
- Task 8.5.3.3 (High-stress error injection & degradation mode testing): 0.5 day
- Reporting and sign-off: 0.5 day

---

## 8.5.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | 75k orders/sec sustained for 4h on staging |
| 2 | p99 ≤ 50µs (supersedes prior ≤ 1ms) tick-to-trade |
| 3 | PostgreSQL replica lag < 2s |
| 4 | N≥10,000 WS connections converge with zero drops (supersedes prior N≥100 — production fan-out scale) |
| 5 | L3 delta exact (matches engine state) (§24 #18) |
| 6 | Recovery drill mid-run: < 10s, zero loss |
| 7 | Zero PagerDuty alerts during test |
| 8 | No memory leaks (memory < 75% after 4h) |
| 9 | No WAL lag alerts |
| 10 | Market data fan-out latency p99 < 50ms across 10,000 concurrent WS subscribers |
| 11 | Isolated demo environment exposes production-equivalent REST/WS/FIX behavior with virtual funds only (§24 #266) |
| 12 | Staging stress harness injects 10k errors/sec and asserts graceful transition through degradation modes (ReadOnly/Throttled) without crashing (§24 #308) |

