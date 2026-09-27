# Phase 1.5 — CI/CD & Validation Harness Hardening

**Duration:** 3–4 days (supersedes prior 3 — Task 1.5.3.4 supply-chain scanning added 2026-09-15)
**Dependencies:** Phase 1
**Spec Reference:** §19 (Deployment), §24 (Acceptance Criteria)

---

## 1.5.1 Objectives

Harden the CI pipeline and test infrastructure to support 400+ per-task spec validation checks (actual 543 across 479 tasks (canonical per AGENTS.md, remediation #37 — supersedes the prior 530/466, 527/463, 523/462, 467/405, 468/406) — remediation #37 mechanical recount, 2026-09-27; supersedes 530/466, 527/463, 523/461, 466/404, 422/360, unverified #13 claim 407/346 and prior 377/316, 373/312, 365+, 358/297, 369+, 340+, 323, 315, 310+, 240+, and 182+) running against ephemeral PostgreSQL 16 + Redis 7 + ClickHouse in under 20 minutes — eliminating flaky spec checkpoints, container startup overhead, and resource contention that would otherwise block every PR.

---

## 1.5.2 Prerequisites

- Phase 1 complete (CMake build, C++ skeleton, Go skeleton, migrations 001–021)
- Access to GitHub Actions runners with 8+ cores (or self-hosted equivalent)

---

## 1.5.3 Tasks

### Task 1.5.3.1: CI Pipeline Optimization

**Objective:** Reduce CI feedback loop to < 5 minutes for unit tests and < 20 minutes for full spec validation across 4 parallel shards.

**File Locations:** `.github/workflows/ci.yml`, `scripts/ci/shard_runner.sh`

**Implementation:**
1. Split GitHub Actions workflow into 3 parallel stages:
   - `build-and-lint` — C++ (clang-tidy, clang-format) + Go (golangci-lint) + CMake build. Target: < 5 min.
   - `unit-tests` — C++ googletest + Go `go test -race ./...`. Target: < 3 min.
   - `spec-validation` — 4 parallel shards, each running ~136 spec checkpoints (actual 543/4, balanced 135–136; supersedes 530/4, 467/4, 466/4, 422/4, 377/4 and ~94/~93/~90/~92/~85/~78/~60/~45)
2. **Migrations:** Apply all 21 migrations to ephemeral PostgreSQL 16 (migration 021 `audit_merkle_roots` added by Phase-01 Task 1.3.8; later phases add 022–077 — remediation #14 adds 072–077 and supersedes prior 022–071/067/065/061 ranges), verify schema.
3. **Ephemeral services:** Docker Compose with PostgreSQL 16, Redis 7, ClickHouse — health-checked before tests run.
4. **Caching:** CMake build cache, Go module cache, Docker layer cache.
5. **Timeout:** Hard 20-minute timeout; any job exceeding it fails.

**Definition of Done (Acceptance Criteria):**
* [ ] CI runs on every PR
* [ ] C++ build + clang-tidy + unit tests complete in < 5 min
* [ ] Go build + golangci-lint + unit tests complete in < 3 min
* [ ] Migrations apply to ephemeral PostgreSQL in < 1 min
* [ ] Spec validation: 4 shards complete in < 20 min total

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: CI runs on every PR — defined first, validated against spec
- [ ] Spec checkpoint: ephemeral PostgreSQL 16 + Redis 7 + ClickHouse — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: flaky tests, container startup failure, cache miss

---

### Task 1.5.3.2: Spec Validation Harness

**Objective:** Create the per-task spec validation framework that extracts and runs spec checkpoints.

**File Locations:** `tests/spec/validator.go`, `tests/spec/checkpoints/`

**Implementation:**
1. **Checkpoint extraction:** Parse all `Spec checkpoint:` lines from phase docs; generate test stubs.
2. **Checkpoint runner:** Go test runner that executes each checkpoint against ephemeral services.
3. **Shard assignment:** Deterministic hash of checkpoint ID → shard (0-3); no single shard runs all.
4. **Golden corpus:** 20+ spec-derived test cases (FIFO matching, self-trade prevention, FOK/IOC, ICEBERG, balance atomicity, WAL recovery).
5. **Report:** JSON report with pass/fail per checkpoint; CI fails on any failure.

**Definition of Done (Acceptance Criteria):**
* [ ] 400+ spec checkpoints extracted from phase docs (actual 543 across 479 tasks; supersedes 530/466, 527/463, 523/461, 467/405, 466/404, 422/360, unverified 407/346 and prior 377/316, 373/312, 365+, 358/297, 369+, 340+, 323, 310+, 240+, 182+)
* [ ] 4 parallel shards with deterministic assignment
* [ ] Golden corpus: 20+ spec-derived test cases
* [ ] JSON report generated with per-checkpoint pass/fail
* [ ] CI fails on any checkpoint failure

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: 400+ per-task spec validation checks (actual 543 across 479 tasks; supersedes 530/466, 527/463, 523/461, 467/405, 466/404, 422/360 and prior 350+/377/316) — defined first, validated against spec
- [ ] Spec checkpoint: 4 parallel shards < 20 min — defined first, validated against spec
- [ ] Spec checkpoint: golden corpus 20+ spec-derived cases — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: checkpoint timeout, flaky checkpoint, missing dependency

---

### Task 1.5.3.3: Criteria-to-Test Traceability Matrix

**Objective:** Validate that the master specification §24 criteria traceability matrix maps every spec criterion to validating tests.

**File Locations:** `docs/Specification - Complete Exchange System Suite.md` (§24), `tests/spec/traceability.go`

**Implementation:**
1. Extract every §24 criterion ID (1–414 (canonical criterion count, remediation #37); supersedes prior 1–401, 1–398, 1–335, 1–334, 1–333, 1–296, 1–276, 1–256, 1–252, 1–237, 1–219, 1–206, 1–201, 1–192, 1–174) directly from the specification §24 matrix.
2. Map each criterion to ≥1 validating test and/or phase AC reference.
3. CI job validates: 0 unmapped criteria; fails on gaps.

**Definition of Done (Acceptance Criteria):**
* [ ] Specification §24 traceability matrix maps all 414 criteria (canonical, remediation #37) to owner phase, phase AC and stable test contract (supersedes prior 401, 398, 335, 334, 333, 296, 276, 256, 252, 237, 219, 206, 201, 192, 174; consolidated from prior standalone acceptance-matrix.md)
* [ ] CI job validates 0 unmapped criteria
* [ ] Matrix updated automatically when new criteria added

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: every §24 criterion mapped to ≥1 test — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation
- [ ] Edge cases: deleted tests, renamed criteria, manual waiver

---

### Task 1.5.3.4: Supply-Chain Security Scanning

**Objective:** Gate PRs on SAST and dependency vulnerability scanning (spec §19.2, §24 #161).

**File Locations:** `.github/workflows/security.yml`, `.trivyignore`

**Implementation:**
1. **SAST:** CodeQL (or equivalent) analysis for Go and TypeScript on every PR.
2. **Dependency audit:** `govulncheck` for Go modules; `npm audit` (or `better-npm-audit`) for the React frontend; audit of vendored C++ dependencies (CMake FetchContent pins + CVE feed check).
3. **Container scan:** Trivy on built service images; HIGH/CRITICAL findings fail the pipeline.
4. **Secret scan:** gitleaks (or CI-native secret scanning) on every push.
5. New dependencies require a version published ≥ 7 days ago (supply-chain cooldown).

**Definition of Done (Acceptance Criteria):**
* [ ] CodeQL/SAST runs on every PR and fails on HIGH findings
* [ ] govulncheck + npm audit + C++ dep audit gate the build
* [ ] Trivy image scan fails on HIGH/CRITICAL vulnerabilities
* [ ] Secret scanning blocks pushes containing credentials

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: SAST + dependency audit gates PRs (§24 #161) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 1.5.3.5: Negative Test Suite & Fault Injection Harness

**Objective:** Implement dedicated negative test scenarios and fault injection assertions in CI/CD pipeline verifying L0–L3 error handling invariants per spec §2.7, §22.7, and §24 #298.

**Implementation:**
1. **Fault Injection Corpus:** Create test harness in `ci/fault-injection/` simulating corrupt WAL headers, invalid Aeron buffer addresses, simulated clock jumps >100µs, PostgreSQL serialization conflicts (SQLSTATE 40001), and malformed FIX/WS frames.
2. **Negative Assertion Engine:** Implement automated check verifying every invalid input or simulated infrastructure failure returns expected RFC 7807 error code or deterministic process halt rather than unhandled panic or partial state mutation.
3. **Automated Recovery Check:** Verify secondary standby engine replays up to last valid transaction without data loss after simulated primary crash.

**Definition of Done (Acceptance Criteria):**
* [ ] Negative test suite runs on CI and asserts fail-closed invariants across all 4 error tiers
* [ ] Simulated WAL corruption halts matching core and invokes recovery report
* [ ] Replay attack and invalid signature inputs rejected with HTTP 401/403 RFC 7807 envelopes

**SDD Checklist (MANDATORY):**
- [ ] Spec checkpoint: CI negative test suite injects faults and validates deterministic fail-closed behavior (§24 #298) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 1.5.4 Deliverables

- GitHub Actions CI pipeline with 4 parallel spec validation shards
- Spec validation harness with 400+ checkpoint extraction (actual 543 across 479 tasks (canonical per AGENTS.md, remediation #37); supersedes prior 530/466, 527/463, 523/462, 467/405, 466/404, 422/360, 407/346, 377/316, 373/312, 365+, 358/297, 369+, 340+, 323, 315, 310+, 240+, 182+)
- Golden corpus of 20+ spec-derived test cases
- Criteria-to-test traceability matrix (414 criteria mapped; supersedes prior 401, 398, 335, 334, 333, 296, 276, 256, 252, 237, 219, 206, 201, 192, 174 and 164)
- Supply-chain security scanning (SAST, dependency audit, Trivy, secret scan)
- Negative test suite & fault injection CI harness (Task 1.5.3.5)

---

## 1.5.5 Dependencies

- Phase 1 (all tasks)

---

## 1.5.6 Duration Estimate

3–4 days (supersedes prior 3 — Tasks 1.5.3.4–1.5.3.5 added, absorbed in range):
- Task 1.5.3.1 (CI scaffold): 1 day
- Task 1.5.3.2 (Spec validation harness): 1 day
- Task 1.5.3.3 (Traceability matrix): 0.5 day
- Task 1.5.3.4 (Supply-chain security): 0.5 day
- Task 1.5.3.5 (Negative test harness): 0.5 day
- Testing + hardening: 0.5 day

---

## 1.5.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | CI pipeline runs on every PR (GitHub Actions) |
| 2 | C++ build + clang-tidy + unit tests complete in < 5 min |
| 3 | Go build + golangci-lint + unit tests complete in < 3 min |
| 4 | Migrations apply to ephemeral PostgreSQL 16 in < 1 min |
| 5 | 400+ spec checkpoints extracted from phase docs; actual 543 across 479 tasks (supersedes prior 530/466, 527/463, 523/461, 468/406, 467/405, 466/404, 422/360, 407/346, 377/316, 373/312, 365+, 358/297, 369+, 340+, 323, 315, 310+, 240+, 182+) |
| 6 | 4 parallel shards with deterministic checkpoint assignment |
| 7 | All 4 shards complete in < 20 min total |
| 8 | Golden corpus: 20+ spec-derived test cases (FIFO, self-trade, FOK/IOC, ICEBERG, balance, WAL) |
| 9 | JSON report with per-checkpoint pass/fail generated |
| 10 | CI fails on any checkpoint failure |
| 11 | Specification §24 traceability matrix maps all 414 criteria to owner phase, phase AC and stable test contract (supersedes prior 401, 398, 335, 334, 333, 296, 276, 256, 252, 237, 219, 206, 201, 192, 174; consolidated from prior standalone acceptance-matrix.md) |
| 12 | CI job validates 0 unmapped criteria |
| 13 | Ephemeral services (PostgreSQL, Redis, ClickHouse) health-checked before tests |
| 14 | CMake build cache + Go module cache + Docker layer cache configured |
| 15 | Hard 20-minute timeout; any job exceeding it fails |
| 16 | SAST (CodeQL or equivalent) runs on every PR; fails on HIGH findings |
| 17 | Dependency audit (govulncheck + npm audit + C++ dep audit) gates the build |
| 18 | Trivy image scan fails on HIGH/CRITICAL; secret scan blocks credential pushes |
| 19 | Negative test harness injects L0–L3 faults (corrupt WAL, network split, arithmetic overflow, invalid HMAC) and verifies fail-closed invariants (§24 #298) |
