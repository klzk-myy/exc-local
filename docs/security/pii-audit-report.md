# PII Audit Report — Task 13.5.3.2

**Date:** 2026-09-29 · **Auditor:** Devin subagent (Phase-13.5) ·
**Method:** mechanical — schema catalog generated from 102 `*.up.sql`
migrations by `scripts/security/gen-pii-inventory.py` (fail-closed:
unclassified PII-name-pattern columns fail the run), code-path
verification per claim, log/error sink scans.

Artifacts: [`pii-inventory.md`](./pii-inventory.md) +
[`pii-catalog.csv`](./pii-catalog.csv) (131 PII-bearing columns across
46 of 134 catalogued tables; +2 sealed mirrors added by migration 210 —
PII-F1 remediation) · [`gdpr-erasure-runbook.md`](./gdpr-erasure-runbook.md).

---

## 1. Inventory summary

| Class | Columns | Meaning |
|---|---|---|
| DIRECT_ID | 9 | legal/display names, address, residency country, jurisdiction |
| CONTACT | 4 | `users.email`, `users.phone`, VDP reporter handle/email |
| GOV_ID | 6 | `tax_self_certifications.tin`/`.fields`/`tin_country`, `kyc_documents.file_url`, `unfreeze_requests` doc refs |
| FINANCIAL | 11 | IBAN/account numbers, BIC/routing, sender/originator accounts, SWIFT refs |
| AUTH_SECRET | 10 | password hash, TOTP secret, backup codes, API/webhook/OAuth secrets |
| PSEUDONYMOUS | 20 | IPs, UA, device fingerprints, geo, actor-id columns |
| LINKAGE | 28 | `user_id`/`account_id`/`proposer_id` re-identification joins |
| FREE_TEXT | 39 | bodies/notes/payloads that may incidentally embed PII |
| ORG_CONTACT | 2 | `liquidity_providers.name`/`.contact` |

Not in schema yet (deferred): `kyc_profiles` (verified legal
name/DOB/address — Phase-14/21, §14.8), `comms_recordings` (Phase-21
Task 21.3.20), `account_closures` (Phase-14 Task 14.3.9). DOB is **not
collected anywhere today** — `users` carries self-declared
`full_name`/`address`/`country` only (migration 027 comment: verified
PII belongs to `kyc_profiles`).

## 2. Encryption at rest — per-claim verification

| Claim | Evidence | Verdict |
|---|---|---|
| Passwords bcrypt cost-12 | `internal/auth/registration.go:618` `bcrypt.GenerateFromPassword(..., bcryptCost)`; policy comment `:14`; OAuth client secrets same cost (`oauth.go`, migration 151 `client_secret_hash`) | ✅ verified |
| TOTP sealed | `auth.SecretBox` = AES-256-GCM `nonce‖ct` (`secrets.go:37-78`); stored `base64(Seal(seed))` (`registration.go:245-270`); column widened to VARCHAR(160) for sealed form (migration 027) | ✅ verified |
| API key secrets | `apikey.go:214-218`: `secret_enc = box.Seal(secret)`, `key_hash = sha256(secret)`; asymmetric keys store public key only (migration 025/073) | ✅ verified |
| Webhook secrets | `webhook_endpoints.secret_enc` BYTEA sealed (migration 180; `internal/webhooks` uses SecretBox) | ✅ verified |
| Withdrawal confirm tokens | `withdrawal_confirmations.token_hash` SHA-256 (migration 160) | ✅ verified |
| TOTP backup codes | SHA-256 digests, `users.totp_backup_codes TEXT[]` (migration 027) | ✅ verified |
| KYC document bytes | `compliance/upload.go:281-283` sets `ServerSideEncryption:"aws:kms"` + `SSEKMSKeyID` on every object Put; `kyc_documents.sse_algorithm` records `aws:kms`; devs3 records/echoes SSE headers (`devs3/server.go:196,291`); asserted by `objectstore/sse_test.go:27-51` and `compliance/kyc_test.go:260-272` | ✅ verified |
| `secrets.data_key` (envelope key) | required in production (`config.go:254-261` — boot fails closed without it); production source of truth Vault/KMS (config.go:49 comment; Task 13.5.3.5) | ✅ verified |
| **`tax_self_certifications.tin` + `fields`** | PII-F1 remediated (migration 210): writes seal TIN + the whole fields document via `auth.SecretBox` into `tin_sealed`/`fields_sealed` BYTEA (`compliance/store.go` `sealSelfCert`/`InsertSelfCert`); plaintext columns are deprecated read-fallback only, cleared by `SealTaxPIIBackfill` (`exchange seal-tax-pii`, also run at gateway boot); `TestITSelfCertSealedAtRest` asserts raw rows carry no SSN-shaped plaintext | ✅ **FIXED** (was FINDING PII-F1) |
| `bank_accounts.*` (IBAN, account_number, beneficiary_name, BIC) | plaintext (migration 040) | ⚠️ FINDING PII-F2 |
| `users.email/phone/full_name/address/country` | plaintext | ⚠️ justified plaintext — see findings PII-F6 |

**Data-flow boundary note:** PostgreSQL is not configured with
`sslmode` in the dev DSN (`services/config.example.yaml:42` —
`sslmode=disable`) and table-level encryption is not used; PG at-rest
protection is volume/filesystem-level in the reference deployment
(`deploy/postgres`). That's consistent with the spec's encryption-at-
rest claim only for *secrets* (all secrets verified sealed/hashed) and
KYC object bytes (SSE-KMS). PII columns relying on it — see findings.

## 3. Encryption in transit

| Hop | Evidence | Verdict |
|---|---|---|
| Client → edge | `deploy/haproxy/haproxy.cfg:39` `ssl-min-ver TLSv1.3 no-tls-tickets`, `:40` restricted TLS1.3 suites, `:67` :80→443 redirect, `:76` `bind :443 ssl` | ✅ TLS 1.3 only |
| Edge → gateway | gateway serves plain `http.Server` (`cmd/gateway/main.go:1775,1821`) — the listener is internal-only behind HAProxy (`main.go:793` comment: HAProxy front is the only supported client path); HSTS armed only in prod/TLS mode (`main.go:1974`) | ✅ boundary documented — dev/loopback plaintext, edge-terminated TLS |
| FIX sessions | `deploy/k8s/services/fix-gateway.yaml` FIXS mTLS keystore wiring (Phase-18 scaffold) | ✅ mTLS planned |
| Internal (PG/Redis/NATS) | dev `sslmode=disable`; Sentinel config exists (`config/redis_sentinel.go`) | ⚠️ internal-hop TLS is a prod-deploy concern — documented boundary, not a code defect |

## 4. Access logging — who accessed PII

| Path | Audited? | Evidence |
|---|---|---|
| Support dossier view (balances/orders/tickets/KYC metadata) | ✅ | `internal/admin/support_view.go:203-213` — `LogAuto` writes `admin_audit_log` + `audit_hash_chain` link; **audit failure fails the view closed** (`:213`). Dossier deliberately excludes email/name |
| Admin beneficiary verify/reject, withdrawal/deposit review, chargeback ops, freeze, RBAC changes | ✅ (mutations) | `funding/bank_accounts.go:134-136,711-725` in-tx `AdminAuditTx`; `chargebacks.go:189,335,380`; `admin/audit.go:105-117` |
| `GET /api/v1/admin/funding/bank-accounts` (full IBAN/name registry) | ❌ | `api/handlers_bank_accounts.go:109-122` — no audit call on the read |
| `GET /api/v1/admin/funding/quarantine` (originator name/account rows) | ❌ | `api/handlers_funding_p11.go:183-230` — no audit call |
| Admin nostro/ops-alert/replenishment list reads (may embed sender refs) | ❌ | `handlers_flows_p11.go:473-560` — no audit call |
| Client self-reads (own profile, own beneficiaries, own KYC status) | n/a — subject access to own data needs no audit row |

**→ FINDING PII-F3:** privileged *reads* of PII-bearing tables are
unaudited (only the mutation + the composite dossier are). The Phase-07
support-view audit hook exists as the template (`LogAuto` on read).

## 5. Retention mapping

| PII surface | Policy class | Covered? |
|---|---|---|
| orders / trades / order_audit | `tiering_policy.yaml` — 5y partitioned lifecycle | ✅ enforcer-checked nightly |
| ledger_lines, audit_hash_chain | 7y | ✅ |
| support_tickets | 5y | ✅ |
| kyc_documents (+S3 objects) | `external` — account close +5y | ✅ lifecycle-bound (Phase-14 14.3.9 offboarding owns the delete) |
| webhook_deliveries, idempotency, dedup | 7–90d purgeable | ✅ |
| comms_recordings | 5y WORM (class pre-registered) | ⏸ deferred — table not yet implemented |
| **`users`, `login_history`, `bank_accounts`, `tax_self_certifications`, `notification_*`, `deposit_confirmations`, `suspense_*`, `client_*`, `unfreeze_requests`, `vulnerability_disclosures`** | — | ⚠️ **FINDING PII-F7**: no enforcer class; lifecycle handled by GDPR runbook §4 (manual) until Phase-14/21 tasks land — recommend adding non-purgeable informational classes |

## 6. Zero-leak verification

Method: (a) `slog`/`logf`/`Errorf` sink sweep over all `services/`
non-test Go for PII field names and values; (b) response-envelope
review (`api/respond.go:40` `WriteError` — registered codes only,
internal errors degrade to `INTERNAL_ERROR`); (c) auth enumeration
check (registration.go:33-34,58-61 — identical "verification email
sent" shape for duplicates; login failures emit `INVALID_CREDENTIALS`
without echoing the email, `:736`); (d) dev-sink boundary review.

| Item | Result |
|---|---|
| `slog` attribute keys across services | `err/shard/stage/run_id/roll_date/next_fire` only — no PII keys |
| `notifications.LogSender` (the **only** wired sender — `cmd/gateway/main.go:667-669`, all envs) logged `to=<raw email/phone>` | ❌ leaked → **FIXED** (PII-F4): `senders.go:94-114` masks to `***@domain`; test `TestLogSenderMasksRecipient` |
| `auth.LogSender` (dev mailer, `main.go:984`) logged `to=%s` raw email | ❌ leaked → **FIXED** (PII-F5): `mailer.go:62-81` masked; `TestLogSenderDelivers` updated to assert the mask |
| `auth.LogSender` also logs `body=%q` (verification/reset links with tokens) | ⚠️ retained deliberately — the log line is the dev "mailbox" (`mailer.go:50-52` contract); INFO-severity note PII-F9, prod sender is a Phase-12 seam |
| `notifications.FileSender` (dev mailbox, unwired) | writes full envelope JSONL to local dir — dev artifact boundary, documented (PII-F9) |
| `middleware.Logging` logs `remote=RemoteAddr` | ✅ IP in access log is legitimate-interest security telemetry (retain w/ log policy) |
| KYC object keys | `upload.go:309` strips `ObjectKey` before the API response — keys never leave the server path |
| Error responses | ✅ no raw PII echo found; service errors map to registered codes |

**Verdict: 0 unresolved PII log/response leaks after fixes.**

## 7. Findings register

| ID | Severity | Finding | Status |
|---|---|---|---|
| PII-F1 | **HIGH** | `tax_self_certifications.tin` (SSN/EIN) + `fields` JSONB (legal name/address/treaty claims) stored **plaintext** — the only GOV_ID column with no protection | **FIXED** — migration 210 `tin_sealed`/`fields_sealed` BYTEA (AES-256-GCM SecretBox); `compliance/store.go` seals on write, unseals on read, plaintext fallback retained only for pre-backfill rows; `exchange seal-tax-pii` (`cmd/exchange/seal_tax_pii.go`) + gateway boot backfill clear legacy plaintext; `--restore` is the 210-down pre-step (documented privacy regression); tests: `TestITSelfCertSealedAtRest`, `TestITSelfCertBackfill`, `TestITSelfCertUnseal`, `TestInsertSelfCertSealFailureFailsClosed` |
| PII-F2 | MEDIUM | `bank_accounts` IBAN/account_number/beneficiary_name and `funding_transactions`/`deposit_confirmations`/`suspense_account_mappings` originator fields plaintext | OPEN — functional justification (rails dispatch raw IBAN; name-match screening reads plaintext) documented; recommend SecretBox sealing + hash index as defense-in-depth in a later phase |
| PII-F3 | MEDIUM | Admin PII **reads** unaudited: `GET /admin/funding/bank-accounts`, `GET /admin/funding/quarantine`, nostro/alert list endpoints return PII rows with no `admin_audit_log` entry | OPEN — remediation: wrap list handlers with `admin.LogAuto` read entries (same pattern as support_view). Code change beyond minimal-leak-fix boundary → reported |
| PII-F4 | MEDIUM | `notifications.LogSender` logged raw `to` (email/phone) — the sole wired sender in all environments | **FIXED** — `senders.go:94-114` + `TestLogSenderMasksRecipient`, `TestMaskRecipient` |
| PII-F5 | MEDIUM | `auth.LogSender` logged raw `to=` email on every verification/reset mail | **FIXED** — `mailer.go:62-81` + updated `TestLogSenderDelivers` |
| PII-F6 | LOW | `users.email/phone/full_name/address` plaintext | DOCUMENTED — justified: login lookup requires exact-match email; notification addressing needs cleartext endpoints; compensating controls = audited admin views + masked logs + TLS1.3 edge. HMAC-index alternative noted for future hardening |
| PII-F7 | LOW | `login_history`, `notification_deliveries`, `users`, `bank_accounts` have no `tiering_policy.yaml` class — retention enforced only by runbook | OPEN — recommend informational (non-purgeable) classes so the nightly enforcer emits coverage evidence |
| PII-F8 | INFO | KYC S3 object metadata carries `filename` (uploader's original filename may embed a personal name) | DOCUMENTED — metadata rides inside SSE-KMS; recommend filename normalization on write in a later phase |
| PII-F9 | INFO | `auth.LogSender` body=%q includes token-bearing links; `FileSender` dev mailbox persists full envelope | DOCUMENTED — dev-boundary seams; prod senders (SES/Twilio) are the Phase-12 wiring seam; flag for removal once real providers land |

## 8. DoD verdict

| DoD item | Result |
|---|---|
| PII inventory complete | ✅ 134 tables catalogued; PII columns annotated; generator fails on unclassified drift (`--check` mode for CI) |
| Encryption at rest + transit verified | ✅ per §2/§3 — every claim has a code/test reference; PII-F1 remediated (migration 210), PII-F2 remains the registered exception |
| Access logging verified | ⚠️ partial — mutations + dossier audited; read-path gap = PII-F3 |
| 0 PII leaks | ✅ after PII-F4/F5 fixes — verified by sink sweep + tests |
