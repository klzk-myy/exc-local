# Phase 4.5 — Recovery Chaos Validation

**Duration:** 3 days
**Dependencies:** Phase 4, Phase 2.5
**Spec Reference:** §18 (Recovery & Replay)

---

## 4.5.1 Objectives

Validate crash recovery under 6 chaos scenarios, each run 3 times: crash mid-batch, timeout-safe requeue, stale-snapshot guard, WAL trimmed → PostgreSQL fallback, PID conflict fail-closed, and warm recovery. Zero duplicate/missing trades required on every run.

---

## 4.5.2 Prerequisites

- Phase 4 complete (snapshot, archive, PITR, recovery manager)
- Phase 2.5 complete (soak test infrastructure)

---

## 4.5.3 Tasks

### Task 4.5.3.1: Chaos Scenario Suite

**Objective:** Implement and run 6 chaos scenarios, each 3 times.

**File Locations:** `tests/chaos/scenarios/`

**Scenarios:**
1. **Crash mid-batch:** Kill C++ core during WAL fsync; verify zero dup/miss on recovery. Recovered boots must exercise the §3.5 graduated ladder: CRC-repair path restores service via `Maintenance` + synthetic probes; unrecoverable tails halt fail-closed with a `recovery_reports` row + P1 alert (never silent, never a partial book).
2. **Timeout-safe requeue:** IPC timeout during order submission; verify order requeued or rejected (no loss).
3. **Stale-snapshot guard:** Snapshot older than WAL tail; verify guard fires and forces full WAL replay.
4. **WAL trimmed → PostgreSQL fallback:** WAL trimmed before S3 archive; verify PostgreSQL PITR used for recovery.
5. **PID conflict fail-closed:** Two C++ processes with same PID file; verify both fail-closed.
6. **Warm recovery:** Follower takes over after leader crash; verify < 10s recovery with zero loss.

**Definition of Done (Acceptance Criteria):**
* [ ] All 6 scenarios pass 3 times each (18 total runs)
* [ ] Zero duplicate trades on every run
* [ ] Zero missing trades on every run
* [ ] Recovery time < 10s on every run

**SDD Checklist:**
- [ ] Spec checkpoint: 6 chaos scenarios pass 3x each — defined first, validated against spec
- [ ] Spec checkpoint: zero dup/miss on every recovery — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 4.5.3.2: Database Serialization Conflict & Split-Brain Chaos Scenarios

**Objective:** Validate system resilience and zero-loss guarantees under simulated database deadlocks, network partitioning, and split-brain leader conditions per spec §2.7, §5.40, and §24 #303.

**Implementation:**
1. **Serialization Contention Chaos:** Inject 1,000 concurrent balance mutations targeting identical accounts to induce intense PostgreSQL `40001` serialization failures. Verify all conflicts either cleanly retry or abort without phantom balances.
2. **Network Split & Fencing Verification:** Sever network connectivity between matching engine primary and standby nodes; verify monotonic fencing token causes partitioned node to self-terminate with `SIGTERM` rather than process dual-writes.
3. **Standby Promotion Parity Check:** Measure failover latency and verify promoted node validates book sequence against WAL tail before opening order ingress.

**Definition of Done (Acceptance Criteria):**
* [ ] Database serialization contention resolves with zero balance anomalies
* [ ] Network partition trips leader fencing and avoids split-brain dual execution
* [ ] Standby promotion maintains monotonic sequence ordering without missing transactions

**SDD Checklist:**
- [ ] Spec checkpoint: Chaos validation verifies database serialization retries and split-brain fencing (§24 #303) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 4.5.4 Deliverables

- Chaos validation report (6 scenarios × 3 runs each = 18 runs)
- Pass/fail results per scenario with recovery time metrics
- Zero duplicate/missing trade verification logs
- Database contention & split-brain chaos verification suite (Task 4.5.3.2)

---

## 4.5.5 Dependencies

- Phase 4, Phase 2.5

---

## 4.5.6 Duration Estimate

3 days (Task 4.5.3.2 absorbed in range):
- Task 4.5.3.1 (Chaos scenario suite): 2 days (6 scenarios × 3 runs)
- Task 4.5.3.2 (Serialization & split-brain chaos): 0.5 day
- Analysis and reporting: 0.5 day

---

## 4.5.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Crash mid-batch: zero dup/miss on recovery (3 runs); ambiguous tails resolve via §3.5 ladder (repair/rebase resume, or fail-closed with recovery_report) |
| 2 | Timeout-safe requeue: no order lost (3 runs) |
| 3 | Stale-snapshot guard fires and forces full replay (3 runs) |
| 4 | WAL trimmed → PostgreSQL PITR fallback works (3 runs) |
| 5 | PID conflict: both processes fail-closed (3 runs) |
| 6 | Warm recovery: < 10s with zero loss (3 runs) |
| 7 | All 18 runs pass with zero duplicate trades |
| 8 | All 18 runs pass with zero missing trades |
| 9 | Recovery time < 10s on every run |
| 10 | Chaos harness verifies database serialization retries without balance corruption and confirms leader fencing terminates partitioned nodes (§24 #303) |

