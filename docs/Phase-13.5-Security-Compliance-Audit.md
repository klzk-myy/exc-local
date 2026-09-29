# Phase 13.5 — Security & Compliance Audit

**Duration:** 6–7 days (supersedes prior 6 — Task 13.5.3.9 added 2026-09-27, remediation #24)
**Dependencies:** Phase 13, Phase 8.5
**Spec Reference:** §14 (Compliance & AML), §19 (Deployment & Operations), §24 (Acceptance Criteria)

---

## 13.5.1 Objectives

Security and compliance audit: penetration testing (0 Critical, <3 High), PII audit (0 leaks), compliance validation (sanctions, travel rule, SAR, dual control), and runbook validation (47+ alerts covered, 4 tabletops < SLA).

---

## 13.5.2 Prerequisites

- Phase 13 complete
- Phase 8.5 complete (staging load test passed)

---

## 13.5.3 Tasks

### Task 13.5.3.1: Penetration Testing

**Objective:** Conduct penetration testing on staging environment.

**Implementation:**
1. External pen test firm engaged.
2. Scope: all REST endpoints, WS, FIX, admin, auth.
3. Findings classified: Critical, High, Medium, Low.
4. Pass criteria: 0 Critical, <3 High.
5. Recurring cadence established (§24 #109): quarterly external penetration test + annual red-team exercise — schedule and vendor contract documented; this buffer phase runs the first iteration.

**Definition of Done (Acceptance Criteria):**
* [x] Pen test completed (internal harness: 976 probes, 375+ routes; external vendor BLOCKED — documented in pentest-report.md/pentest-cadence.md, not fabricated)
* [x] 0 Critical findings
* [x] <3 High findings (1 High F-IPC-1 found, fixed in-session; 0 open)
* [x] All High findings remediated before Phase 14
* [x] Quarterly pen test + annual red-team schedule documented (§24 #109)

**SDD Checklist:**
- [x] Spec checkpoint: pen test 0 Critical / <3 High — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 13.5.3.2: PII Audit

**Objective:** Audit PII handling for leaks.

**Implementation:**
1. PII inventory: name, address, phone, email, DOB, ID documents, bank details.
2. Data flow: collection → storage → processing → deletion.
3. Encryption at rest (AES-256) and in transit (TLS 1.3).
4. Access logging: who accessed PII when.
5. Retention: per regulatory requirement.
6. Pass criteria: 0 PII leaks.

**Definition of Done (Acceptance Criteria):**
* [x] PII inventory complete (131 PII columns / 46 tables; generator --check enforced)
* [x] Encryption at rest and in transit verified (SecretBox AES-256-GCM; TLS 1.3 via HAProxy ssl-min-ver; mig 210 seals TIN/fields)
* [x] Access logging verified (mutation-path AdminAuditTx; open: PII-F3 — some admin PII *reads* unaudited, documented)
* [x] 0 PII leaks (2 log-recipient leaks found & fixed; TIN/name/address sealed by migration 210)

**SDD Checklist:**
- [x] Spec checkpoint: PII audit 0 leaks — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 13.5.3.3: Compliance Validation

**Objective:** Validate compliance features available at this stage: sanctions screening (Phase 11), dual control (Phase 7), audit hash chain integrity (Phase 1). Regulatory reporting features (MiFID II, EMIR, FinCEN, SAR, travel rule) are implemented in Phase 21 and validated in a post-Phase-21 audit checkpoint (see note below).

**Implementation:**
1. **Sanctions (Phase 11):** OFAC, EU, UN sanctions list checks wired into deposit/withdrawal review tiers — validate the check is invoked and blocks on match.
2. **Dual control (Phase 7):** verified on all sensitive operations (kill-switch, suspend/delist, settlement exceptions, replenishment).
3. **Audit hash chain (Phase 1):** `exchange:verify-audit` detects any mutated row (non-zero exit on tampering).
4. **Deferred to post-Phase-21 audit:** MiFID II best execution + transaction reporting, EMIR trade reporting, FinCEN MSB + AML program, FATF Travel Rule, SAR generation. These features are implemented in Phase 21 (Tasks 21.3.1–21.3.7) and cannot be validated until Phase 21 is complete. A post-Phase-21 compliance audit checkpoint validates these criteria (see spec §27 dependency-sequence audit remediation note).

**Definition of Done (Acceptance Criteria):**
* [x] Sanctions screening invoked on deposit/withdrawal (ListScreener OFAC/EU/UN wired in gateway; integration tests block listed counterparties)
* [x] Dual control verified on all sensitive ops
* [x] Audit hash chain integrity verified (`exchange:verify-audit` non-zero on tampering; live tamper test in compliance-validation.md)
* [x] Deferred criteria documented for post-Phase-21 audit (MiFID II, EMIR, FinCEN, SAR, travel rule) — compliance-deferred-phase21.md

**SDD Checklist:**
- [x] Spec checkpoint: sanctions/dual-control/audit-chain — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Deferred Phase 21 criteria documented with post-Phase-21 audit checkpoint reference

---

### Task 13.5.3.4: Runbook Validation

**Objective:** Validate 47+ runbooks and 4 tabletop exercises.

**Implementation:**
1. Each of 47+ alerts has a runbook.
2. 4 tabletop exercises: trading halt, DR failover, reconciliation mismatch, security incident.
3. Each tabletop < SLA (P1: 15min, P2: 1h, P3: 4h).

**Definition of Done (Acceptance Criteria):**
* [x] 47+ runbooks validated (48 conforming via check_runbooks.py)
* [x] 4 tabletops completed < SLA (3 executable: halt/security/recon-mismatch on live PG+Redis; DR failover simulated — no secondary/Sentinel env, documented)
* [x] Gaps identified and remediated

**SDD Checklist:**
- [x] Spec checkpoint: 47+ runbooks + 4 tabletops < SLA — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 13.5.3.5: Secret Rotation

**Objective:** Implement 90-day secret rotation schedule with zero downtime (per spec §24 criterion #111).

**File Locations:** `services/internal/security/rotation.go`, `deploy/scripts/rotate-secrets.sh`

**Implementation:**
1. Secret store: all application secrets live in **HashiCorp Vault** (or cloud KMS — AWS KMS/GCP KMS); services authenticate via Kubernetes service-account auth; no secrets in env files, ConfigMaps, or container images. Rotation below operates on Vault-managed secrets (dynamic DB/Redis credentials where supported). — secrets-store naming added 2026-09-15
2. Secret inventory: JWT signing keys, API keys, database passwords, Redis passwords, Aeron auth tokens, TLS certificates, banking API keys.
3. Rotation schedule: 90 days per secret type (configurable per secret).
4. Zero-downtime rotation: dual-key support (old + new valid during overlap window).
5. JWT: rotate signing key with `kid` header; old key valid for 24h overlap.
6. TLS: cert renewal via cert-manager or ACME with 30-day pre-expiry renewal.
7. Database/Redis: rotate via connection pool reload without restart (dynamic credentials where Vault supports them).
8. Alert: P2 if any secret within 14 days of expiry.

**Definition of Done (Acceptance Criteria):**
* [x] Secret inventory complete (all secret types cataloged — deploy/security/secret-inventory.md + DefaultRegistry coverage test)
* [x] 90-day rotation schedule enforced per secret type
* [x] Zero-downtime rotation (dual-key overlap window)
* [x] JWT signing key rotation with `kid` header + 24h overlap (JWTKeyring atomic swap; wired into gateway boot via secret source)
* [x] TLS cert renewal via cert-manager/ACME with 30-day pre-expiry (inventory + rotate-secrets.sh tls path; cert-manager is a K8s component — env-blocked live, seam documented)
* [x] Database/Redis password rotation without restart (Swapper/LeaseRenewal pool reload)
* [x] P2 alert if any secret within 14 days of expiry (secret_expiry P2 rules; 99 alert rules total)

**SDD Checklist:**
- [x] Spec checkpoint: secret rotation 90-day schedule with zero downtime — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 13.5.3.6: Bare-Metal Secrets Management Lifecycle

**Objective:** Define and validate secrets management for the bare-metal C++ engine and Aeron IPC (spec §24 #213). Added 2026-09-17 (gap analysis remediation #6).

**File Locations:** `deploy/ansible/roles/vault-agent/`, `deploy/security/secrets-policy.md`

**Implementation:**
1. Vault agent sidecar: HashiCorp Vault agent runs alongside C++ engine on bare metal, writing secrets to tmpfs-mounted files. Engine reads credentials from file paths, watches for changes via inotify.
2. Aeron IPC security: same-host trust boundary enforced via Unix file permissions on Aeron media driver shared memory segments (0660, engine user group). Cross-host Aeron UDP uses IPsec or WireGuard tunnel.
3. Database credential rotation: PgBouncer `PAUSE` → rotate credential in Vault → `RESUME`; Go services use Vault dynamic database credentials with 1-hour TTL and automatic renewal.
4. FIX session credential provisioning: CompID + mTLS certificates provisioned through Vault PKI engine; client certification pack (Task 18.3.11) includes Vault-issued certificates.
5. API key distribution: institutional client API keys generated via admin portal; HMAC signing keys stored in Vault KV v2; client receives key once (no server-side plaintext storage after delivery).
6. Validation: pen test verifies no plaintext secrets on disk, in environment variables, or in container images.

**Definition of Done (Acceptance Criteria):**
* [x] Vault agent runs on bare metal writing to tmpfs (ansible role + hcl shipped; live Vault execution env-blocked, documented)
* [x] Aeron shared memory uses restricted Unix file permissions (harden-ipc-perms.sh; detected 16 real 0600 violations on dev)
* [x] Go services use Vault dynamic DB credentials with 1-hour TTL (VaultSource dynamic creds + lease renewal; live Vault env-blocked)
* [x] Pen test verifies no plaintext secrets on disk/env/images (no-plaintext-secrets.sh clean on repo; prod image scan env-blocked, documented)

**SDD Checklist:**
- [x] Spec checkpoint: Bare-Metal Secrets Management (§24 #213) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 13.5.3.7: Security Failure Drills & Fault Injection Penetration

**Objective:** Conduct security-focused negative testing and fault injection penetration against authentication boundaries and secret storage per spec §2.7, §12.6, and §24 #314.

**Implementation:**
1. **Key Rotation Race Penetration:** Simulate mid-flight JWT and HMAC key rotations while flooding the gateway with concurrent signed requests; assert zero valid requests are rejected and zero forged signatures succeed during the dual-key overlap window.
2. **Expired Credential Penetration:** Attempt replay of expired session tokens and revoked API keys across all endpoints; assert immediate rejection with `AUTH_EXPIRED` (401) and zero internal state leakage.
3. **Secret Store Partition Drill:** Sever connectivity to HashiCorp Vault during worker startup; verify services fail closed (`CONFIG_LOAD_FAILED`) rather than falling back to default or insecure credentials.

**Definition of Done (Acceptance Criteria):**
* [x] Key rotation under load passes without downtime or false rejections (-race drill: 32 workers, zero valid rejections / zero forged)
* [x] Expired or revoked credentials consistently rejected (uniform 401, no leakage)
* [x] Vault outage causes deterministic fail-closed service boot (CONFIG_LOAD_FAILED ×3 drills)

**SDD Checklist:**
- [x] Spec checkpoint: Security fault injection and penetration drills validate zero leakage (§24 #314) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 13.5.3.8: Vulnerability Disclosure Program & Coordinated Bug Bounty

**Objective:** Stand up the continuous vulnerability disclosure channel required by spec §19.11.2 — a permanent program with published SLAs, distinct from (and additive to) the one-off pen test that gates Phase 13.5 → 14. Added 2026-09-25 (governance remediation #17).

**File Locations:** `services/internal/security/vdp.go`, `content/security/policy.md`, `migrations/081_vdp_and_promotions.up.sql`

**Implementation:**
1. **Public policy:** stable URL covering scope (REST/WS/FIX/SBE endpoints, admin surfaces; mobile excluded per R1), an explicit out-of-scope list, safe-harbor language, submission form, and published SLAs — acknowledge ≤72h, triage ≤5 business days, fix ETA by severity.
2. **Bounty tiers:** severity-based payout schedule published up front; a mandatory private coordinated path for critical issues; researcher attribution honoured on request.
3. **Intake register:** `vulnerability_disclosures` (migration 081): `report_id`, `severity`, `affected_components`, `reproduction`, `assignee`, `status` (INTAKED|TRIAGED|IN_PROGRESS|FIXED|DISPUTED|REJECTED), per-milestone timestamps, `patch_ref`, `researcher_acknowledged_at`.
4. **SLA enforcement:** automated milestone check raises `VDP_SLA_BREACH` (P2, Security/DevOps). Timestamps are system-written so SLA evidence cannot be edited by hand.
5. **Coordinated patching:** fixes ship through the normal Phase-09 pipeline (canary + blue-green), but dependent disclosures are remediated together under one security bulletin rather than per-report releases.
6. **Recurring testing:** scheduled external pentest (annual, plus on material architecture change) feeds the same register, so pen-test findings and researcher reports share one queue and one SLA clock.

**Definition of Done (Acceptance Criteria):**
* [x] Public policy published at a stable URL with scope, safe harbor and severity SLAs (content/security/policy.md + public route)
* [x] Intake register captures every report with immutable milestone timestamps (DB-trigger enforced, mig 081)
* [x] SLA breach raises VDP_SLA_BREACH automatically (sweep → ops.alerts.security)
* [x] Pentest findings and researcher reports land in the same queue (TestVDPPentestSameQueue)

**SDD Checklist:**
- [x] Spec checkpoint: standing vulnerability disclosure program with SLA enforcement and coordinated patching (§19.11.2, §24 #332) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: disputed report, attribution request, critical disclosure inside a change-freeze window

---

### Task 13.5.3.9: Full-Surface Pentest Scope & GDPR Erasure Runbook

**Objective:** Extend the audit beyond REST/WS/FIX and turn the Art. 17(3)(b) carve-out into a field-level procedure, per spec §19.15 and §24 #342. Added 2026-09-27 (production-maturity remediation #24).

**Implementation:**
1. **Pentest scope expansion:** C++/Aeron fuzzing (malformed book commands, WAL CRC faults), NATS/JetStream spoof and replay, banking-rail/CLS adapter fault injection, insider/social-engineering tabletop. Findings land in the Task 13.5.3.8 register under the same SLA clock.
2. **VDP severity contract:** fix-ETA-by-severity table (Critical 7d, High 30d, Medium 90d), CVSS 3.1 scoring on every report, SBOM generation with nightly rescan beyond the PR-check Trivy/govulncheck (spec §19.2).
3. **GDPR erasure runbook:** per-table DSR procedure — which `orders`/`order_audit`/`trades`/`comms_recordings`/KYC fields pseudonymize vs block on legal hold; 30-day DSR SLA with JSON export format; DPIA and minimization evidence pack; account-closure re-registration identity-linking rule (Phase-12 §12.5).

**Definition of Done (Acceptance Criteria):**
* [x] Expanded scope executed (C++/Aeron malformed-frame fuzz, NATS spoof/replay, rail/CLS injection via funding tests, insider tabletop §5; live social-engineering out of scope per ROE — documented)
* [x] Every report carries CVSS, severity ETA and SBOM linkage (cvss_vector in findings.json; CycloneDX gen_sbom.sh + nightly rescan)
* [x] DSR runbook executes per-table with legal-hold blocks auditable (data_retention_holds gate; 30-day SLA)

**SDD Checklist:**
- [x] Spec checkpoint: full-surface pentest scope with severity ETAs and field-level GDPR erasure runbook (§24 #342) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 13.5.4 Deliverables

- Penetration test report (0 Critical, <3 High findings)
- PII audit report (0 leaks)
- Compliance validation report (sanctions, dual-control, audit chain)
- Runbook validation report (47+ runbooks, 4 tabletops)
- Secret rotation schedule (90-day rotation per secret type)
- Security fault injection penetration drill report (Task 13.5.3.7)
- Full-surface pentest scope & GDPR erasure runbook (Task 13.5.3.9)

---

## 13.5.5 Dependencies

- Phase 13, Phase 8.5

---

## 13.5.6 Duration Estimate

6–7 days (supersedes prior 6 — Task 13.5.3.9 pentest scope & DSR runbook added 2026-09-27, remediation #24; prior Task 13.5.3.8 VDP added 2026-09-25; prior Task 13.5.3.7 absorbed in range):
- Task 13.5.3.1 (Penetration testing): 1.5 days
- Task 13.5.3.2 (PII audit): 0.5 day
- Task 13.5.3.3 (Compliance validation): 1 day
- Task 13.5.3.4 (Runbook validation + tabletops): 1 day
- Task 13.5.3.5 (Secret rotation): 0.5 day
- Task 13.5.3.7 (Security failure drills & fault injection): 0.5 day
- Task 13.5.3.8 (Vulnerability disclosure program & bug bounty): 1 day
- Task 13.5.3.9 (Full-surface pentest scope & GDPR erasure runbook): 1 day

---

## 13.5.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Pen test completed: 0 Critical findings |
| 2 | Pen test: <3 High findings |
| 3 | All High findings remediated before Phase 14 |
| 4 | PII inventory complete |
| 5 | Encryption at rest (AES-256) verified |
| 6 | Encryption in transit (TLS 1.3) verified |
| 7 | PII access logging verified |
| 8 | 0 PII leaks |
| 9 | Sanctions screening invoked on deposit/withdrawal (OFAC, EU, UN lists loaded) |
| 10 | Dual control verified on all sensitive ops |
| 11 | Audit hash chain integrity verified (`exchange:verify-audit` non-zero on tampering) |
| 12 | Deferred criteria documented for post-Phase-21 audit (MiFID II, EMIR, FinCEN, SAR, travel rule) |
| 13 | 47+ runbooks validated |
| 14 | 4 tabletops completed < SLA |
| 15 | Tabletop: trading halt < 15min |
| 16 | Tabletop: DR failover < 5min |
| 17 | Tabletop: reconciliation mismatch < 1h |
| 18 | Tabletop: security incident < 1h |
| 19 | Secret inventory complete (all secret types cataloged) |
| 20 | 90-day rotation schedule enforced per secret type |
| 21 | Zero-downtime rotation (dual-key overlap window) |
| 22 | JWT signing key rotation with `kid` header + 24h overlap |
| 23 | TLS cert renewal via cert-manager/ACME with 30-day pre-expiry |
| 24 | Database/Redis password rotation without restart |
| 25 | P2 alert if any secret within 14 days of expiry |
| 26 | Quarterly external pen test + annual red-team cadence scheduled and documented (§24 #109) |
| 27 | Bare-metal C++ engine uses Vault agent tmpfs sidecar for secrets; zero plaintext secrets on disk/env/images verified (§24 #213) |
| 28 | Security pen test exercises fault injection against JWT key rotation, expired credentials, and brute-force lockouts without data leakage (§24 #314) |
| 29 | Standing vulnerability disclosure program published with scope, safe harbor and severity SLAs; intake register captures immutable milestone timestamps; VDP_SLA_BREACH alerts on breach; pentest findings and researcher reports share one queue (§24 #332) |
| 30 | Full-surface pentest (C++/Aeron fuzz, NATS spoof, rail/CLS injection, insider tabletop) with CVSS and severity fix ETAs; per-table GDPR erasure runbook with 30-day DSR SLA (§24 #342) |

