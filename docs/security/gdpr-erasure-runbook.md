# GDPR Erasure / DSR Runbook — Art. 17 Per-Table Procedure

**Task 13.5.3.9 (GDPR half) · Phase-13.5.** Covers the erasure right
(`POST /api/v1/account/gdpr/erase`, route registered in Phase-05 Task
5.3.7, `services/internal/gateway/routes_v1.go:619`) and the export right
(`/api/v1/account/gdpr/export`, `routes_v1.go:617`). **Both routes are
registered stubs — the executing code path is Phase-21 Task 21.3.7
(geo-block/DSR service), which is not yet implemented.** Until it lands,
this runbook IS the DSR procedure: it is executed manually by a
**Compliance Officer** with a second **Finance Ops / Risk Manager**
reviewer, and every step writes `admin_audit_log` evidence (fail-closed
— a DSR step whose audit write fails does not proceed).

Scope of "the data subject": one `users` row and every `accounts` row
where `accounts.user_id = users.id`, plus all tables reachable via
`account_id` / `user_id` linkage (see `pii-inventory.md`).

---

## 1. SLA and intake

| Item | Rule |
|---|---|
| SLA | **30 calendar days** from verified request (Art. 12(3)); one 60-day extension permitted for complexity — notify the subject in writing before day 30 |
| Verification | Authenticated request (JWT + TOTP step-up where enrolled). Unauthenticated/email requests must verify identity before disclosure |
| Triage owner | Compliance Officer queue (support ticket `category=COMPLAINT`/`KYC` or DSR inbox) |
| Audit | One `admin_audit_log` row per DSR event: `dsr.request`, `dsr.export`, `dsr.erase`, `dsr.blocked_hold`, `dsr.complete` — via `admin.LogAuto` (`services/internal/admin/audit.go:149`) so each row links into `audit_hash_chain` |
| Response channel | JSON export (§5) delivered over the authenticated session, or encrypted email attachment (PGP/portal link, 7-day expiry) |

## 2. Legal-hold gate — run FIRST, always

An **ACTIVE** `data_retention_holds` row (`released_at IS NULL` and
`expires_at` NULL or future) blocks every destructive lifecycle
transition for the covered scope — hot→warm detach, warm→cold drop,
retention purge **and DSR erasure/mutation on covered tables**
(migration 194; enforced for lifecycle ops by `internal/archiver` —
`holds.go` — and the nightly enforcer; this runbook applies the same
gate manually).

```sql
-- Run for every table the DSR touches; non-empty result = BLOCK that table.
SELECT hold_id, parent_table, partition_name, case_ref, reason,
       created_by, expires_at
  FROM data_retention_holds
 WHERE released_at IS NULL
   AND (expires_at IS NULL OR expires_at > now())
   AND parent_table = :table;          -- repeat per target table
```

- Any match → the covered table is **BLOCKED**: no DELETE/UPDATE; record
  `admin_audit_log` action `dsr.blocked_hold` with `case_ref`; the DSR
  response states "erasure restricted: legal obligation (Art. 17(3))".
- A hold covers the whole parent when `partition_name IS NULL`.
- Partitioned parents (`orders`, `trades`, `order_audit`, `ledger_lines`,
  `audit_hash_chain`): also check the rows' own partition names.
- Fail-closed (§2.7): if `data_retention_holds` cannot be read, the DSR
  aborts — an unreadable hold registry is a block, not a pass.
- Complement: per-account compliance hold (Phase-14 Task 14.3.10, not
  yet implemented) and litigation flags in `support_tickets`
  (`type='DISPUTE'` open) also block — check before executing §4.

## 3. Identity resolution

```sql
SELECT id AS user_id, email FROM users WHERE lower(email)=lower(:email);
SELECT id AS account_id, status FROM accounts WHERE user_id = :user_id;
```

Produce the target set: `{user_id, account_id[]}` — every step below is
parameterized on it. Requests from closed accounts resolve identically
(`accounts.status='CLOSED'`).

## 4. Per-table erasure procedure

Legend: **DELETE** row removal · **PSEUDO** pseudonymize listed columns
in place · **RETAIN** record kept under Art. 17(3)(b)/(e) legal-obligation
or public-interest carve-out — identity linkage severed at `users` instead ·
**DEFERRED** table/object type not yet implemented (owner noted) ·
**N/A** no personal data.

Pseudonymization constants: `email` → `dsr-<user_id>@erased.invalid`;
`full_name`/`beneficiary_name`/`display_name` → `[erased <user_id>]`;
`phone`/`address`/`country`/IPs/UA/fingerprints → `NULL`;
`totp_secret` → `NULL`; `password_hash` → `!`; `totp_backup_codes` → `'{}'`.
Keep `users.id`/`accounts.id` keys — referential integrity for the
retained financial record requires the join surrogate to survive.

### 4.1 Identity core (the subject)

| Table | Action | Columns / detail |
|---|---|---|
| `users` | PSEUDO | `email`, `phone`, `full_name`, `address`, `country`, `anti_phishing_code`, `password_hash`, `totp_secret`, `totp_backup_codes`; set `status='CLOSED'` |
| `login_history` | PSEUDO | `ip`, `user_agent`, `device_fingerprint`, `geo_city`, `geo_country`, `session_id` → NULL (row + result retained — security telemetry) |
| `notification_preferences` | DELETE | one row per user |
| `notification_deliveries` / `notification_dead_letters` | DELETE | `payload` may embed event data; recipient address is never persisted (resolved at send time) |
| `webauthn_credentials` | DELETE | row + `credential_id`/`public_key` (not secret, still identifying) |
| `api_keys` | DELETE | `key_hash`/`secret_enc` are hash/sealed but delete the row — erasure covers lookup digests too |
| `oauth_clients` | DELETE or PSEUDO `name` | delete row if client is personal; `client_secret_hash` goes with it |
| `client_delegated_users` | PSEUDO `display_name`; DELETE binding if the delegate THEMSELF is the subject | `suspend_reason`/`revoke_reason` reviewed for embedded names |
| `client_role_bindings` / `client_approval_*` / `client_delegation_events` | RETAIN | audit trail of M-of-N approvals; actor ids are linkage only — severed at `users` |
| `unfreeze_requests` | PSEUDO | `id_document_ref`, `liveness_ref` → NULL (also delete the referenced objects under §4.3); `note` scrub |
| Redis sessions | DELETE | `session:{sid}` hashes (store `ip`/`user_agent`) + `sess:ip:{ip}` index members — `SessionManager.RevokeAll` (`services/internal/auth/session.go:371`) or `DEL session:*` for the user; TTL-bound regardless |

### 4.2 KYC / tax (statutory — blocks until floor)

| Table | Action | Basis / floor |
|---|---|---|
| `kyc_submissions` | RETAIN until account-close +5y, then PSEUDO `jurisdiction`,`reject_reason` | AML record-keeping; lifecycle-bound class (`tiering_policy.yaml` `kyc_documents` entry covers the bundle) |
| `kyc_documents` (rows) | RETAIN rows until floor; DELETE the S3 objects at floor | `file_url`/`sha256`/`sse_algorithm` are metadata; see §4.3 for the object delete |
| `tax_self_certifications` | RETAIN until statutory tax-reporting floor (CRS/FATCA), then DELETE | SSN/EIN + legal name/address live in `tin_sealed`/`fields_sealed` (AES-256-GCM SecretBox — **PII-F1 remediated, migration 210**); DELETE removes the only recoverable copy (sealed blobs + deprecated plaintext columns together); erasure claims are refused under Art. 17(3)(b) while the reporting obligation lives |

### 4.3 Object store

| Object | Action |
|---|---|
| `kyc/<account>/<submission>/*` (SSE-KMS objects referenced by `kyc_documents.file_url`) | DELETE objects at erasure **only when no §2 hold and the AML floor (account close +5y) has lapsed**; document each deleted key in the `dsr.erase` audit row `after_state` |
| `comms_recordings` (MiFID taping) | **DEFERRED** — table/store not yet implemented (Phase-21 Task 21.3.20); policy class already reserves WORM 5y. When it lands: RETAIN (Art. 16(7) legal obligation), no erasure before floor |
| WAL/snapshot archives | N/A — order-event journal, no PII fields; erasure claim answered as out-of-scope with basis |

### 4.4 Financial/immutable records (Art. 17(3)(b))

RETAIN — never delete, never pseudonymize the record body. These are
legal-obligation records (MiFID II RTS 6 / finance 7y per
`tiering_policy.yaml`). The only permitted mutation is on the listed
*identity-leak* columns:

| Table | Action | Permitted pseudonymization |
|---|---|---|
| `orders`, `order_audit`, `trades` | RETAIN (5y) | `order_audit.ip_address` → NULL; `order_audit.modified_by` → `system:dsr` when it carries a user sub; `orders.client_order_id` is client-supplied free text — left intact (record fidelity), documented in the DSR response |
| `settlement_instructions` | RETAIN | `swift_message_id` is a bank reference — retained |
| `transfers`, `funding_transactions`, `withdrawal_dispatch_queue`, `withdrawal_confirmations` | RETAIN | none — `reference`/`reference_account`/`originator_name` are the transaction record (AML + dispute defense) |
| `deposit_confirmations`, `suspense_account_mappings` | RETAIN | third-party sender data kept under AML source-of-funds obligation; a THIRD-PARTY sender's own erasure request is handled case-by-case by Compliance (they are not the account subject) |
| `bank_accounts` | RETAIN until floor | beneficiary `iban`/`account_number`/`beneficiary_name` are the disbursement record; **after** the floor lapses: PSEUDO `iban`/`account_number` to masked form, `beneficiary_name` → `[erased <user_id>]` |
| `withdrawal_destination_holds`, `withdrawal_whitelist_settings` | DELETE | destination cache is a control copy, not the disbursement record |
| `chargebacks`, `chargeback_evidence` | RETAIN (dispute floor) | review `chargeback_evidence.payload` JSONB — redact embedded personal strings on best-effort where the evidence kind allows |
| `journal_entries`, `ledger_lines`, `balances` history | RETAIN (7y) | account linkage only — severed at `users` |
| `idempotency_keys`, `client_order_id_dedup` | DELETE or natural purge | 7-day retention class already purgeable by the enforcer |

### 4.5 Support / admin / audit plane

| Table | Action |
|---|---|
| `support_tickets`, `ticket_notes` | RETAIN (MiFID complaint register, 5y — in policy `support_tickets` class); PSEUDO scrub of `subject`/`body`/`adr_reference` best-effort; internal notes reviewed for embedded PII |
| `admin_audit_log`, `audit_hash_chain`, `retention_audit_log`, `client_delegation_events`, `account_freeze_events`, `trading_suspensions` | RETAIN (7y audit floor; `audit_hash_chain` never purgeable). Staff/admin actor ids are employee data under employment basis, not subject to client erasure. `trading_suspensions.client_ip`, `account_freeze_events.metadata` reviewed + PSEUDO where they capture *client* data |
| `admin_role_bindings`, `admin_recert_*`, `admin_dual_control_requests`, `admin_break_glass_grants` | RETAIN (staff records, employment basis) |
| `listing_proposals` | RETAIN `proposer_id` linkage (severed at `users`); `reason` free text scrubbed |
| `vulnerability_disclosures` | PSEUDO `contact_email`→NULL/`reporter_handle`→`[erased]` on request — no retention obligation; `reproduction` PoC reviewed for embedded user data |
| `liquidity_providers.contact` | ORG basis — business contact; erasure honored on request unless the contact is also a client user |

### 4.6 Deferred surfaces (not yet implemented — owner on record)

| Surface | Owner |
|---|---|
| `account_closures` table (closure record, `sweep_refs`, `preconditions_snapshot`) | Phase-14 Task 14.3.9 — spec §12.5 |
| `kyc_profiles` verified PII (legal name/DOB/address — the authoritative identity record) | Phase-14/21 (§14.8; migration not yet landed) |
| `comms_recordings` + taping pipeline | Phase-21 Task 21.3.20 (policy class pre-registered) |
| `POST /account/close` + offboarding state machine | Phase-14 Task 14.3.9 (`routes_v1.go:523` stub) |
| `POST /account/gdpr/export`, `POST /account/gdpr/erase` executors | Phase-21 Task 21.3.7 (`routes_v1.go:617-620` stubs) |
| Automated DSR service (this runbook → code) | Phase-21 Task 21.3.7 |

## 5. DSR export format (30-day SLA payload)

One JSON document per subject, delivered via the authenticated session:

```json
{
  "meta": {
    "dsr_id": "DSR-2026-000123",
    "generated_at": "2026-09-29T00:00:00Z",
    "subject_user_id": 1001,
    "account_ids": [20001, 20002],
    "retention_blocks": [
      {"table": "tax_self_certifications", "basis": "Art.17(3)(b) CRS/FATCA reporting obligation"},
      {"table": "orders", "basis": "Art.17(3)(b) MiFID II RTS 6 — 5y"}
    ]
  },
  "identity":   {"users": {}, "profile": {}, "login_history": []},
  "security":   {"api_keys": [], "webauthn_credentials": [], "oauth_clients": [], "sessions": []},
  "kyc":        {"submissions": [], "documents_meta": [], "tax_self_certifications": []},
  "funding":    {"bank_accounts": [], "transactions": [], "deposit_confirmations": [], "chargebacks": []},
  "trading":    {"orders": [], "order_audit": [], "trades": [], "positions": [], "transfers": []},
  "support":    {"tickets": [], "notes_external": []},
  "preferences":{"notification_preferences": {}},
  "notes":      "Internal admin notes, admin_audit_log entries and surveillance data are exempt from subject access (Art. 15 limitations / national law); supplied on competent-authority request only."
}
```

Every field name mirrors the source column names in `pii-catalog.csv` so
the export ↔ inventory correspondence is mechanically auditable.

## 6. Account-closure re-registration identity-linking rule

Spec §12.5: **re-opening a closed account is prohibited — a new
registration is required.** To keep the AML/fraud boundary honest, the
new registration must remain *linkable* to the prior identity:

- At closure/erasure, Compliance records a **keyed digest** per identity
  token — `HMAC-SHA256(data_key, lower(email))` and
  `HMAC-SHA256(data_key, normalizeTIN(tin))` — into the identity-linkage
  record owned by `account_closures` (Phase-14 Task 14.3.9; proposed
  column set `closed_identity_id, digest_kind, digest_hex, linked_user_id`).
- `POST /auth/register` computes the same digests over the submitted
  email/TIN; a match flags the registration for manual review and links
  `new users.id → prior (erased) identity` in the linkage record — the
  linkage carries ids/digests only, never resurrected plaintext PII.
- This is also how a DSR-erased user who re-registers is detected for
  PEP/sanctions re-screening without retaining the erased data itself.

**Status: runbook-specified, implementation deferred** — `account_closures`
(migration 063 numbering reserved in spec §12.5) does not exist yet;
Phase-14 Task 14.3.9 owns the table and Phase-21 Task 21.3.7 owns the
registration-side digest check. Until then, re-registration linking is a
manual Compliance procedure keyed off the pseudonymized `users` row
(`dsr-<id>@erased.invalid` pattern is grep-able evidence of a prior
erasure but NOT a positive identity link).

## 7. Evidence pack (DPIA / minimization)

- `docs/security/pii-inventory.md` + `pii-catalog.csv` — Art. 30 record of processing, generated from migrations (regenerate + `--check` in CI).
- `infrastructure/data-tiering/tiering_policy.yaml` + `docs/compliance/data-retention.md` — retention schedule + regulatory basis per class; `internal/operations/retention` enforces nightly with `retention_audit_log` evidence.
- `docs/security/attack-surface.md` — route/auth surface the data flows through.
- Minimization evidence: support view exposes no email/name (`internal/admin/support_view.go` — dossier is balances/orders/tickets/KYC-*metadata*); KYC object keys never leave the API path (`upload.go:309`); notification recipient resolved at send time, never persisted; `admin_audit_log.before/after_state` policy: mutation diffs only.
- DPIA narrative: lawful basis = contract (trading) + legal obligation (AML/MiFID/tax) + legitimate interest (security telemetry); high-risk items = GOV_ID plaintext (PII-F1) + unaudited admin reads (PII-F3) — see `pii-audit-report.md` findings register.

## 8. Runbook execution checklist (manual DSR today)

1. Verify subject identity; open `dsr.request` audit row; start 30-day clock.
2. Run §2 hold check across every §4 target table; record `dsr.blocked_hold` rows.
3. Run §3 identity resolution; build target set.
4. Execute §4 top-down (identity core last — sever linkage after evidence captured); each table's mutation is its own tx + `admin_audit_log` row.
5. Delete Redis session keys; delete eligible S3 objects (§4.3).
6. Produce §5 export (or confirm erasure) to the subject; write `dsr.complete`.
7. File the audit rows + export hash under the DSR case ref.
