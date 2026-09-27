# Phase 12 — User Self-Service & Notifications

**Duration:** 10–13 days (unchanged; §12.6 itemization completed 2026-09-27, remediation #35: Tasks 12.3.7–12.3.11 added) (supersedes 7–10 — duration itemization completed 2026-09-27, remediation #35: Tasks 12.3.7–12.3.11 added to §12.6) (supersedes prior 7–9 — Task 12.3.13 added 2026-09-27, remediation #24)
**Dependencies:** Phases 3, 5, 10, 11
**Spec Reference:** §12 (User Self-Service & Notifications), §8.1 (Authentication), §14.2 (KYC Tiers)

---

## 12.1 Objectives

Implement user self-service: registration, KYC submission, 2FA setup, profile management, notification preferences, and the notification service (email, SMS, push) with retry/backoff.

---

## 12.2 Prerequisites

- Phases 3, 5, 10, 11 complete

---

## 12.3 Tasks

### Task 12.3.1: User Registration & Authentication

**Objective:** Implement user registration and login.

**File Locations:** `services/internal/auth/registration.go`

**Implementation:**
1. `POST /api/v1/auth/register` — email, password (bcrypt), country.
2. Email verification (token link).
3. `POST /api/v1/auth/login` — email + password → JWT.
4. `POST /api/v1/auth/refresh` — refresh token → new access token.
5. `POST /api/v1/auth/logout` — invalidate session.
6. Password reset: email link, 1h expiry.
7. **Migration note:** `migrations/027_alter_users_add_password_hash.up.sql` — `ALTER TABLE users ADD COLUMN password_hash VARCHAR(128)` (spec §5.16).

**Definition of Done (Acceptance Criteria):**
* [ ] Registration creates account with bcrypt `password_hash` persisted to `users` (migration 027)
* [ ] Login returns JWT (access + refresh)
* [ ] Refresh produces new access token
* [ ] Logout invalidates session
* [ ] Password reset works with 1h expiry

**SDD Checklist:**
- [ ] Spec checkpoint: user registration + email verification — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.2: 2FA Setup (TOTP)

**Objective:** Implement TOTP 2FA setup and verification.

**File Locations:** `services/internal/auth/totp.go`

**Implementation:**
1. `POST /api/v1/auth/2fa/setup` — generate secret, return QR code.
2. `POST /api/v1/auth/2fa/verify` — verify first TOTP code, enable 2FA.
3. `POST /api/v1/auth/2fa/disable` — disable (requires password + TOTP).
4. Backup codes: 10 single-use codes.
5. Required for: withdrawals, balance adjustments, API key creation.
6. **Non-Destructive Candidate Re-Enrollment (added 2026-09-27, remediation #38):** When an account with active 2FA initiates re-enrollment or secret rotation via `POST /api/v1/auth/2fa/setup`, the system MUST stage the candidate secret in temporary cache (`2fa:pending:{user_id}`) or `users.two_factor_pending_secret` with a 10-minute TTL. The system MUST NOT clear `two_factor_enabled` or overwrite the active `two_factor_secret` during setup. Existing 2FA remains enforced on all sensitive actions until the user successfully verifies a token against the candidate secret via `POST /api/v1/auth/2fa/verify`. If re-enrollment is abandoned or unverified, active 2FA security remains intact without downtime or unprotected exposure.

**Definition of Done (Acceptance Criteria):**
* [ ] TOTP setup generates QR code
* [ ] First TOTP verification enables 2FA
* [ ] Disable requires password + TOTP
* [ ] 10 backup codes generated
* [ ] 2FA required for sensitive operations

**SDD Checklist:**
- [ ] Spec checkpoint: TOTP 2FA with backup codes — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.3: Profile Management

**Objective:** Implement profile management.

**File Locations:** `services/internal/api/profile.go`

**Implementation:**
1. `GET /api/v1/account/profile` — get profile.
2. `PUT /api/v1/account/profile` — update (name, address, phone).
3. `POST /api/v1/account/change-password` — change password.
4. `GET /api/v1/account/api-keys` — list API keys.
5. `POST /api/v1/account/api-keys` — create API key.
6. `DELETE /api/v1/account/api-keys/{id}` — revoke API key.

**Definition of Done (Acceptance Criteria):**
* [ ] Profile get and update work
* [ ] Password change works
* [ ] API key CRUD works

**SDD Checklist:**
- [ ] Spec checkpoint: profile + API key management — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.4: KYC Submission

**Objective:** Implement KYC document submission.

**File Locations:** `services/internal/compliance/kyc.go`

**Implementation:**
1. `POST /api/v1/kyc/submit` — upload documents (ID, proof of address).
2. Document storage: S3 with SSE-KMS encryption (§24 #102).
3. KYC tiers: T0 (no verification), T1 (basic), T2 (full), institutional (manual review).
4. Tier limits (spec §14.2): T0 → no trading/no withdrawal; T1 → $10K/day withdrawal; T2 → $100K/day withdrawal; institutional → negotiated.
5. Re-verification: 12 months for T2, 24 months for institutional.

**Boundary note (Phase 12 vs Phase 14):** Phase 12 owns the **KYC submission flow**: user uploads documents, system stores them in S3, assigns initial tier (T0 by default). Phase 14 (Task 14.3.4) owns the **KYC lifecycle management**: admin approve/reject workflow, auto-downgrade on overdue re-verification, re-verification triggers (document expiry, risk score change, regulatory update). Phase 12 creates the submission endpoint and document storage; Phase 14 extends it with the admin workflow and lifecycle automation.

**Definition of Done (Acceptance Criteria):**
* [ ] KYC documents uploaded to S3 encrypted
* [ ] 4 KYC tiers (T0, T1, T2, institutional) with correct limits
* [ ] Re-verification: 12 months T2, 24 months institutional

**SDD Checklist:**
- [ ] Spec checkpoint: KYC tiers T0/T1/T2/institutional — defined first, validated against spec
- [ ] Spec checkpoint: re-verification 12mo/24mo — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.5: Notification Service

**Objective:** Implement email, SMS, push notifications with retry.

**File Locations:** `services/internal/notifications/`

**Implementation:**
1. Channels: email (SES/SendGrid), SMS (Twilio), push (FCM/APNS), WS (WS channel added 2026-09-27, remediation #35 — Phase-10 Task 10.3.22's UI toggle requires the backend WS delivery this task produces; supersedes the prior 3-channel list).
2. Events: deposit confirmed, withdrawal completed, order filled, KYC approved/rejected, liquidation warning, security alert.
3. Queue: Redis list `notifications:pending`.
4. Retry: exponential backoff (1s, 2s, 4s, 8s, 16s; max 5).
5. Dead letter: failed notifications stored in `notification_dead_letters` PostgreSQL table (§24 #100 delivery tracking — every notification records channel/status/attempts/delivered_at).
6. User preferences: per-event, per-channel opt-in/opt-out.
7. **Migration note:** `migrations/028_create_notification_dead_letters.up.sql` — `notification_dead_letters` table (id, user_id, channel, event, payload JSONB, attempts, last_error, created_at).

**Definition of Done (Acceptance Criteria):**
* [ ] Email, SMS, push channels work
* [ ] All notification events delivered
* [ ] Retry with exponential backoff
* [ ] Dead letter queue for failures (`notification_dead_letters` table)
* [ ] Delivery tracking per notification: channel, status, attempts, delivered_at (§24 #100)
* [ ] User preferences respected

**SDD Checklist:**
- [ ] Spec checkpoint: notifications with retry + preferences — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.6: Notification Preferences

**Objective:** Implement user notification preferences.

**File Locations:** `services/internal/api/preferences.go`

**Implementation:**
1. `GET /api/v1/account/notifications/preferences` — get preferences.
2. `PUT /api/v1/account/notifications/preferences` — update.
3. Per-event, per-channel: email/SMS/push on/off.
4. Quiet hours: configurable do-not-disturb window.

**Definition of Done (Acceptance Criteria):**
* [ ] Preferences get and update work
* [ ] Per-event, per-channel opt-in/opt-out
* [ ] Quiet hours respected

**SDD Checklist:**
- [ ] Spec checkpoint: notification preferences with quiet hours — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.7: WebAuthn / Passkeys / FIDO2 authentication

WebAuthn / Passkeys / FIDO2 authentication — implement WebAuthn Level 2 registration and authentication flows. Store credentials in `webauthn_credentials` table (migration 068): `{id, user_id, credential_id, public_key, sign_count, transports, created_at, last_used_at, name}`. Support as 2FA factor alongside TOTP, or as sole passwordless login. Registration: `POST /api/v1/account/webauthn/register` → challenge → attestation verification. Authentication: `POST /api/v1/account/webauthn/authenticate` → challenge → assertion verification. Compatible with YubiKey, Apple TouchID/FaceID, Windows Hello, Android biometrics.
- **Session State Elevation Invariant (added 2026-09-27, remediation #38):** Successful passkey assertion ceremony satisfies strong multi-factor authentication (FIDO2 possession + biometric/PIN). The handler MUST atomically increment `sign_count` AND elevate session context by setting `session.two_factor_verified = true` and issuing JWT AMR claim `amr: ["fido2"]`, ensuring passkey-authenticated sessions satisfy downstream 2FA-gated middleware without redundant TOTP challenges.

**SDD Checklist:**
- [ ] Spec checkpoint: passkey registration/authentication verifies challenge, origin, signature, and counter (§24 #270) — defined first, validated against spec

---

### Task 12.3.8: Anti-phishing code

Anti-phishing code — user-configurable secret phrase (`anti_phishing_code` column on `users` table, 4–32 chars (range aligned with the Phase-10 UI and spec §5.16, remediation #35 — supersedes the prior 20-char maximum, which rejected valid UI-set codes)) displayed in the header of every official email and SMS notification from the platform. Set via `PUT /api/v1/account/settings/anti-phishing-code` (requires 2FA). Injected into all email templates by notification service. If code is not set, emails display a banner prompting the user to configure one. Frontend: Settings → Security → Anti-Phishing Code panel.

**SDD Checklist:**
- [ ] Spec checkpoint: anti-phishing code appears on every official user message (§24 #271) — defined first, validated against spec

---

### Task 12.3.9: Device management & login history

Device management & login history — `login_history` table (migration 069): `{id, user_id, timestamp, ip, user_agent, device_fingerprint, geo_city, geo_country, result (SUCCESS/FAILED/2FA_FAILED/LOCKED), session_id}`. REST endpoints: `GET /api/v1/account/sessions` (active sessions), `GET /api/v1/account/login-history` (last 90 days, paginated), `DELETE /api/v1/account/sessions` (terminate all except current), `DELETE /api/v1/account/sessions/{id}` (terminate specific session). Frontend: Settings → Security → Login Activity & Active Sessions panel with device type icons, location, and 'Revoke' buttons.

**SDD Checklist:**
- [ ] Spec checkpoint: users can review login history and revoke active sessions (§24 #271) — defined first, validated against spec

---

### Task 12.3.10: User self-service emergency account freeze

User self-service emergency account freeze — `POST /api/v1/account/emergency-freeze` requiring only current session authentication (no additional 2FA to avoid lockout if 2FA device is compromised). Immediately: cancels all open orders via mass-cancel, terminates all other sessions, revokes all API keys, sets account status to `FROZEN` with `freeze_reason=SELF_FREEZE`. Unfreezing requires full identity re-verification (government ID + liveness check + new 2FA setup) via `POST /api/v1/account/unfreeze-request`. Audit-logged as `SELF_FREEZE` event. Frontend: prominent red 'Freeze My Account' button in Security settings with legal acknowledgment modal.

**SDD Checklist:**
- [ ] Spec checkpoint: self-freeze cancels orders, revokes credentials, and requires identity re-verification (§24 #271) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.11: Institutional Delegated Logins and Client Multi-Validator Controls

**Objective:** Give institutional master accounts client-side workforce RBAC distinct from venue-admin RBAC. Migration 074 creates delegated users, role/scope bindings, approval policies, and approval requests.

**Implementation:**
1. Master accounts create named human logins with `CLIENT_READ_ONLY`, `CLIENT_FINANCE_MANAGER`, `CLIENT_TRADER`, or `CLIENT_APPROVER` roles and explicit account/sub-account/instrument scopes.
2. Finance managers may transfer only within entitled hierarchies; traders cannot withdraw or change security; approvers can approve but not initiate configured operations.
3. Multi-validator policies support M-of-N approval for withdrawals, beneficiary changes, API-key privilege changes, and high-value internal transfers with expiry and anti-self-approval.
4. Every delegation, login, action, approval, revocation, and scope change is audit-logged; emergency master revocation terminates sessions immediately.

**SDD Checklist:**
- [ ] Spec checkpoint: institutional delegated logins enforce scoped client RBAC and M-of-N validation (§24 #285) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.12: Auth Lockout, Credential Cloning Detection & Freeze Partial-Failure

**Objective:** Implement brute-force authentication protection, passkey cloning detection, and emergency freeze fault tolerance per spec §2.7, §12.6, and §24 #312.

**Implementation:**
1. **Brute-Force Lockout:** Track failed password/TOTP attempts in Redis `auth_failures:{user_id}`. On 5 consecutive failures within 5 minutes, lock account for 15 minutes (`ACCOUNT_LOCKED_AUTH_FAILURES`), reject subsequent requests with HTTP 423, and send security email.
2. **WebAuthn Credential Cloning Guard:** Compare sign count on incoming assertion with stored `webauthn_credentials.sign_count` (table name aligned with Task 12.3.7 and migration 068, remediation #35 — supersedes the prior `users_passkeys`, which exists nowhere in the registry). If signature counter decreases or stays static (when non-zero), detect credential cloning, revoke passkey immediately, freeze account, and log `WEBAUTHN_VERIFICATION_FAILED`.
3. **Emergency Freeze Partial-Failure Recovery:** In the emergency freeze saga, retry open order cancellations up to 3 times on matching engine timeout. If cancellations fail, freeze user login and balance withdrawals immediately while generating a critical P1 alert with order IDs for manual desk cancellation.

**Definition of Done (Acceptance Criteria):**
* [ ] 5 failed logins triggers 15-minute lock with security notification
* [ ] Decreasing WebAuthn sign counters immediately revoke credential
* [ ] Emergency freeze gracefully handles partial cancellation failures

**SDD Checklist:**
- [ ] Spec checkpoint: Auth lockout, WebAuthn clone detection, and freeze partial-failure recovery (§24 #312) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

### Task 12.3.13: KYC Operations Matrix & Tax-Input Ownership

**Objective:** Turn tier names into an operable onboarding pipeline and assign the unowned tax inputs, per spec §12.7 and §24 #341. Added 2026-09-27 (production-maturity remediation #24).

**Implementation:**
1. **KYC operations matrix:** vendor + document-type × jurisdiction table; liveness/biometric requirement per tier; document-expiry trigger values; PEP/adverse-media rescreen cadence (daily for T2/institutional, weekly T1 — supersedes the "on schedule" vagueness in Phase-21 Task 21.3.11); applicant risk score with step-up thresholds; manual-review SLA (24h) with reject-and-appeal flow; desktop upload retry, virus-scan and encrypted-chunk resume (mobile stays out per R1).
2. **Tax-input ownership:** onboarding collects self-certifications (W-8BEN/W-8BEN-E/W-9) with TIN validation at T1+; CRS/FATCA reporting (Phase-21 Task 21.3.22) consumes exactly these records. Lot-method book of record: FIFO for 1099-B (Phase-05 Task 5.3.19 governs); the Phase-20 calculator's LIFO/HIFO/AVG are planning-only projections, never the filed record. Spot FX 871(m) withholding confirmed N/A in the endpoint help text.

**Definition of Done (Acceptance Criteria):**
* [ ] Every tier×jurisdiction maps to vendor, documents, liveness and SLA; rescreen cadences enforced
* [ ] Self-certs/TINs collected at onboarding; CRS/FATCA consumes them with zero manual joins
* [ ] Filed lots are FIFO; alternate methods labelled projections in the UI

**SDD Checklist:**
- [ ] Spec checkpoint: KYC operations matrix with SLAs and owned tax inputs with FIFO book of record (§24 #341) — defined first, validated against spec
- [ ] All spec checkpoints pass after implementation

---

## 12.4 Deliverables

- User registration and authentication
- TOTP 2FA with backup codes
- Profile and API key management
- KYC submission with 4 tiers
- Notification service (email, SMS, push)
- Notification preferences
- Passkeys, anti-phishing, device/login management, emergency self-freeze
- Institutional delegated client roles and M-of-N multi-validator controls
- Auth brute-force protection, WebAuthn clone detection & freeze fault recovery (Task 12.3.12)
- KYC operations matrix with review SLAs & owned tax inputs (Task 12.3.13)

---

## 12.5 Dependencies

- Phases 3, 5, 10, 11

---

## 12.6 Duration Estimate

10–13 days (supersedes 7–10 — duration itemization completed 2026-09-27, remediation #35: Tasks 12.3.7–12.3.11 were omitted, which summed to 7 days against the 10-day header; prior supersedes 7–9 — Task 12.3.13 KYC matrix & tax inputs added 2026-09-27, remediation #24):
- Task 12.3.1 (Registration): 1 day
- Task 12.3.2 (2FA): 0.5 day
- Task 12.3.3 (Profile): 0.5 day
- Task 12.3.4 (KYC): 1 day
- Task 12.3.5 (Notifications): 1.5 days
- Task 12.3.6 (Preferences): 0.5 day
- Task 12.3.7 (WebAuthn/Passkeys/FIDO2): 1 day (added to itemization, remediation #35)
- Task 12.3.8 (Anti-phishing code): 0.5 day (added to itemization, remediation #35)
- Task 12.3.9 (Device management & login history): 0.5 day (added to itemization, remediation #35)
- Task 12.3.10 (Emergency account freeze): 0.5 day (added to itemization, remediation #35)
- Task 12.3.11 (Delegated client RBAC): 1 day (added to itemization, remediation #35)
- Task 12.3.12 (Auth lockout & credential clone detection): 0.5 day
- Task 12.3.13 (KYC operations matrix & tax-input ownership): 1 day
- Testing: 0.5 day

---

## 12.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Registration creates account with email verification |
| 2 | Login returns JWT (access + refresh) |
| 3 | Refresh produces new access token |
| 4 | Logout invalidates session |
| 5 | Password reset works with 1h expiry |
| 6 | TOTP setup generates QR code |
| 7 | First TOTP verification enables 2FA |
| 8 | Disable 2FA requires password + TOTP |
| 9 | 10 backup codes generated |
| 10 | 2FA required for withdrawals, balance adjustments, API key creation |
| 11 | Profile get and update work |
| 12 | Password change works |
| 13 | API key CRUD works |
| 14 | KYC documents uploaded to S3 encrypted |
| 15 | 4 KYC tiers (T0, T1, T2, institutional) with correct limits |
| 16 | Re-verification: 12 months T2, 24 months institutional |
| 17 | Email, SMS, push notification channels work |
| 18 | All notification events delivered |
| 19 | Retry with exponential backoff (1s, 2s, 4s, 8s, 16s; max 5) |
| 20 | Dead letter queue for failed notifications |
| 21 | User preferences respected (per-event, per-channel) |
| 22 | Quiet hours respected |
| 23 | bcrypt password persisted to `users.password_hash` (migration 027) |
| 24 | KYC documents stored in S3 with SSE-KMS encryption (§24 #102) |
| 25 | Notification delivery tracking: channel/status/attempts/delivered_at per notification (§24 #100); dead letters in `notification_dead_letters` (migration 028) |
| 26 | KYC caps per spec §14.2: T1 $10K/day, T2 $100K/day withdrawal |
| 27 | WebAuthn/passkeys verify challenge/origin/signature/counter and support secure passwordless or second-factor login (§24 #270) |
| 28 | Anti-phishing codes, device/login history, session revocation, and emergency self-freeze are complete (§24 #271) |
| 29 | Institutional delegated logins enforce scoped client roles and M-of-N multi-validator policies (§24 #285) |
| 30 | 5 consecutive auth failures locks account 15m; WebAuthn signature counter regression revokes key; freeze partial-failure retries and alerts (§24 #312) |
| 31 | KYC vendor×jurisdiction matrix with liveness, rescreen cadence, risk score and 24h review SLA; self-certs/TINs owned at onboarding feeding CRS/FATCA; FIFO book of record for filed lots (§24 #341) |
