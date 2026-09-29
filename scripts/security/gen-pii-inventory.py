#!/usr/bin/env python3
"""gen-pii-inventory.py — Task 13.5.3.2 PII inventory generator.

Parses services/internal/db/migrations/*.up.sql into a table.column
catalog, classifies every column against a curated PII map plus name-
pattern heuristics, and emits:

  docs/security/pii-inventory.md   human inventory (the audit artifact)
  docs/security/pii-catalog.csv    machine-checkable catalog

Classification rule (fail-closed): a column is listed under a table's
PII section only when the curated ANNOTATIONS map names it. Any column
whose NAME matches PII_RE but is not curated is emitted as
class=REVIEW — the generator exits non-zero if REVIEW rows remain, so
the inventory cannot silently drift from the schema.

PII classes:
  DIRECT_ID    name / address / DOB-style direct identifiers
  CONTACT      email / phone / contact address
  GOV_ID       government identifiers (TIN/SSN/passport refs)
  FINANCIAL    bank account / IBAN / card-adjacent identifiers
  AUTH_SECRET  credentials & secrets (hashed/sealed — not PII content,
               tracked because erasure must still cover them)
  PSEUDONYMOUS IP / UA / device fingerprint / geo / actor strings
  LINKAGE      user_id/account_id joins that re-identify a person
  FREE_TEXT    free-text fields that may embed incidental PII
  ORG_CONTACT  institutional/desk contact details

Run: python3 scripts/security/gen-pii-inventory.py [--check]
  --check  regenerate to a temp file and diff against the committed doc
"""

from __future__ import annotations

import csv
import os
import re
import sys
import tempfile
from datetime import datetime, timezone

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MIG = os.path.join(REPO, "services", "internal", "db", "migrations")
DOC_OUT = os.path.join(REPO, "docs", "security", "pii-inventory.md")
CSV_OUT = os.path.join(REPO, "docs", "security", "pii-catalog.csv")

# --- curated annotations: (table, column) -> (class, encryption, access, retention)
# encryption: plaintext | bcrypt-12 | sha256 | AES-256-GCM (SecretBox) |
#             SSE-KMS (object bytes) | n/a (identifier only)
ANNOTATIONS = {
    # ---- users ---------------------------------------------------------
    ("users", "email"): ("CONTACT", "plaintext (normalized lowercase)",
        "POST /api/v1/auth/register · UserByEmail login lookup · notifications Recipient",
        "account lifetime → pseudonymized at closure (runbook §3)"),
    ("users", "phone"): ("CONTACT", "plaintext",
        "notifications Recipient (SMS channel) · PUT /account/profile",
        "account lifetime → pseudonymized at closure"),
    ("users", "password_hash"): ("AUTH_SECRET", "bcrypt cost-12",
        "auth login/change-password compare only", "rotation on change; erased at closure"),
    ("users", "totp_secret"): ("AUTH_SECRET", "AES-256-GCM SecretBox (base64 nonce‖ct)",
        "auth TOTP verify/disable (auth.UserStore, PgxTOTPSecrets)", "until 2FA disabled/closure"),
    ("users", "totp_backup_codes"): ("AUTH_SECRET", "SHA-256 digests (10)",
        "auth backup-code consume", "until 2FA disabled/closure"),
    ("users", "country"): ("DIRECT_ID", "plaintext",
        "POST /auth/register · residency/geo gates", "account lifetime"),
    ("users", "full_name"): ("DIRECT_ID", "plaintext",
        "PUT /account/profile (self-declared)", "account lifetime"),
    ("users", "address"): ("DIRECT_ID", "plaintext",
        "PUT /account/profile (self-declared)", "account lifetime"),
    ("users", "anti_phishing_code"): ("FREE_TEXT", "plaintext (by design — rendered into outbound mail)",
        "notifications anti-phish banner", "until changed"),
    ("users", "kyc_status"): ("LINKAGE", "plaintext",
        "auth gates, compliance review", "account lifetime"),
    ("users", "email_verified_at"): ("LINKAGE", "plaintext",
        "verification ceremony timestamp", "account lifetime"),
    # ---- accounts ------------------------------------------------------
    ("accounts", "user_id"): ("LINKAGE", "n/a (FK)",
        "every account-scoped read joins here", "account lifetime + financial-record floor"),
    # ---- credentials / sessions ----------------------------------------
    ("webauthn_credentials", "user_id"): ("LINKAGE", "n/a (FK)", "passkey ceremonies", "credential lifetime; revoked≠deleted (forensic)"),
    ("webauthn_credentials", "credential_id"): ("AUTH_SECRET", "public credential id", "assertion lookup", "credential lifetime"),
    ("webauthn_credentials", "public_key"): ("AUTH_SECRET", "public key (not secret)", "assertion verify", "credential lifetime"),
    ("webauthn_credentials", "name"): ("FREE_TEXT", "plaintext", "user-chosen device label", "credential lifetime"),
    ("api_keys", "user_id"): ("LINKAGE", "n/a (FK)", "auth forensics", "key lifetime + audit floor"),
    ("api_keys", "key_hash"): ("AUTH_SECRET", "SHA-256", "auth lookup", "key lifetime"),
    ("api_keys", "secret_enc"): ("AUTH_SECRET", "AES-256-GCM SecretBox", "HMAC verify (sealed)", "key lifetime"),
    ("api_keys", "label"): ("FREE_TEXT", "plaintext", "user-chosen key label", "key lifetime"),
    ("api_keys", "ip_allowlist"): ("PSEUDONYMOUS", "plaintext", "auth allowlist check", "key lifetime"),
    ("api_keys", "last_used_ip"): ("PSEUDONYMOUS", "plaintext", "usage telemetry", "key lifetime"),
    ("oauth_clients", "name"): ("FREE_TEXT", "plaintext", "client display label", "client lifetime"),
    ("oauth_clients", "client_secret_hash"): ("AUTH_SECRET", "bcrypt-12", "client-credentials grant", "client lifetime"),
    ("login_history", "user_id"): ("LINKAGE", "n/a (FK)", "device/security page", "security telemetry (see report — retention class gap)"),
    ("login_history", "ip"): ("PSEUDONYMOUS", "plaintext INET", "login audit, anomaly detection", "security telemetry"),
    ("login_history", "user_agent"): ("PSEUDONYMOUS", "plaintext", "device management view", "security telemetry"),
    ("login_history", "device_fingerprint"): ("PSEUDONYMOUS", "plaintext", "device management view", "security telemetry"),
    ("login_history", "geo_city"): ("PSEUDONYMOUS", "plaintext", "login audit", "security telemetry"),
    ("login_history", "geo_country"): ("PSEUDONYMOUS", "plaintext", "login audit", "security telemetry"),
    ("login_history", "session_id"): ("PSEUDONYMOUS", "plaintext", "session correlation", "security telemetry"),
    # ---- KYC -------------------------------------------------------------
    ("kyc_documents", "account_id"): ("LINKAGE", "n/a (FK)", "KYC review, support view", "account close +5y (AML)"),
    ("kyc_documents", "type"): ("FREE_TEXT", "plaintext", "document type label", "account close +5y"),
    ("kyc_documents", "file_url"): ("GOV_ID", "pointer — object bytes are SSE-KMS",
        "S3 key kyc/<acct>/<sub>/<type>-<sha16>.<ext> (upload.go:274)", "account close +5y; object deleted with doc"),
    ("kyc_documents", "submission_id"): ("LINKAGE", "n/a (FK)", "submission join", "account close +5y"),
    ("kyc_documents", "sha256"): ("LINKAGE", "integrity digest", "integrity verify", "account close +5y"),
    ("kyc_submissions", "account_id"): ("LINKAGE", "n/a (FK)", "review pipeline", "account close +5y"),
    ("kyc_submissions", "jurisdiction"): ("DIRECT_ID", "plaintext (ISO country)", "risk/tier policy", "account close +5y"),
    ("kyc_submissions", "risk_score"): ("LINKAGE", "plaintext", "intake scoring", "account close +5y"),
    ("kyc_submissions", "reject_reason"): ("FREE_TEXT", "plaintext", "reviewer note", "account close +5y"),
    ("kyc_submissions", "reviewer_id"): ("PSEUDONYMOUS", "n/a (admin id)", "review attribution", "account close +5y"),
    ("tax_self_certifications", "account_id"): ("LINKAGE", "n/a (FK)", "CRS/FATCA reporting (Phase-21)", "statutory tax retention"),
    ("tax_self_certifications", "tin"): ("GOV_ID",
        "deprecated plaintext — migration-210 read-fallback window only; NULL post-backfill (PII-F1 remediated)",
        "legacy read fallback only (compliance.ListSelfCerts); never written", "statutory tax retention"),
    ("tax_self_certifications", "tin_sealed"): ("GOV_ID",
        "AES-256-GCM SecretBox nonce‖ct (secrets.data_key; Vault/KMS prod)",
        "compliance.InsertSelfCert writes; ListSelfCerts unseals; CRS/FATCA seam reads via box", "statutory tax retention"),
    ("tax_self_certifications", "tin_country"): ("GOV_ID", "plaintext", "TIN validation", "statutory tax retention"),
    ("tax_self_certifications", "fields"): ("GOV_ID",
        "deprecated plaintext JSONB — migration-210 read-fallback window only; '{}' post-backfill (PII-F1 remediated)",
        "legacy read fallback only; never written", "statutory tax retention"),
    ("tax_self_certifications", "fields_sealed"): ("GOV_ID",
        "AES-256-GCM SecretBox nonce‖ct of the whole fields document (legal_name/address/entity/treaty)",
        "compliance.InsertSelfCert writes; ListSelfCerts unseals; Phase-21 reporting via box", "statutory tax retention"),
    # ---- funding / beneficiaries ----------------------------------------
    ("bank_accounts", "account_id"): ("LINKAGE", "n/a (FK)", "beneficiary registry", "financial record floor"),
    ("bank_accounts", "iban"): ("FINANCIAL", "plaintext — FINDING PII-F2 (justified for rails; audit gap PII-F3)",
        "withdrawal allowlist gate, SWIFT/SEPA dispatch", "financial record floor"),
    ("bank_accounts", "account_number"): ("FINANCIAL", "plaintext — FINDING PII-F2",
        "withdrawal allowlist gate, ACH/FedNow dispatch", "financial record floor"),
    ("bank_accounts", "swift_bic"): ("FINANCIAL", "plaintext", "rail routing", "financial record floor"),
    ("bank_accounts", "bic_routing"): ("FINANCIAL", "plaintext", "rail routing", "financial record floor"),
    ("bank_accounts", "bank_name"): ("FINANCIAL", "plaintext", "registry view", "financial record floor"),
    ("bank_accounts", "beneficiary_name"): ("DIRECT_ID", "plaintext — must match KYC legal name (finding PII-F2)",
        "name-match screen + admin verify", "financial record floor"),
    ("funding_transactions", "account_id"): ("LINKAGE", "n/a (FK)", "funding lifecycle", "financial record floor (7y)"),
    ("funding_transactions", "reference"): ("FINANCIAL", "plaintext", "bank reference / reconciliation", "financial record floor"),
    ("funding_transactions", "reference_account"): ("FINANCIAL", "plaintext", "deposit source account", "financial record floor"),
    ("funding_transactions", "originator_name"): ("DIRECT_ID", "plaintext", "third-party deposit screen", "financial record floor"),
    ("deposit_confirmations", "sender_name"): ("DIRECT_ID", "plaintext", "dual-source bank verify", "financial record floor"),
    ("deposit_confirmations", "sender_account"): ("FINANCIAL", "plaintext", "dual-source bank verify", "financial record floor"),
    ("deposit_confirmations", "payload_sha256"): ("LINKAGE", "sha256 of source payload", "dedup", "financial record floor"),
    ("suspense_account_mappings", "originator_name"): ("DIRECT_ID", "plaintext", "quarantine routing screen", "financial record floor"),
    ("suspense_account_mappings", "originator_account"): ("FINANCIAL", "plaintext", "quarantine routing screen", "financial record floor"),
    ("suspense_account_mappings", "resolution_notes"): ("FREE_TEXT", "plaintext", "ops resolution note", "financial record floor"),
    ("withdrawal_destination_holds", "destination"): ("FINANCIAL", "plaintext (normalized)", "first-seen destination hold", "financial record floor"),
    ("withdrawal_confirmations", "token_hash"): ("AUTH_SECRET", "SHA-256", "confirm-token compare", "15-min window + audit"),
    ("withdrawal_confirmations", "confirmed_by"): ("PSEUDONYMOUS", "n/a (user id)", "confirm attribution", "audit floor"),
    ("chargebacks", "reason"): ("FREE_TEXT", "plaintext", "dispute record", "financial record floor"),
    ("chargebacks", "resolution_note"): ("FREE_TEXT", "plaintext", "dispute record", "financial record floor"),
    ("chargeback_evidence", "payload"): ("FREE_TEXT", "plaintext JSONB — may embed account/trade linkage", "evidence bundle", "financial record floor"),
    # ---- Phase-14 lifecycle/product records ----------------------------
    ("account_closures", "reason"): ("FREE_TEXT", "plaintext", "closure record — client/officer-entered", "financial record floor"),
    ("swapfree_verifications", "decision_note"): ("FREE_TEXT", "plaintext", "verifier note", "account close +5y"),
    ("strategy_profiles", "display_name"): ("FREE_TEXT", "plaintext", "manager-chosen public label", "strategy lifetime"),
    ("strategy_profiles", "suspend_reason"): ("FREE_TEXT", "plaintext", "compliance note", "strategy lifetime"),
    ("compliance_holds", "reason"): ("FREE_TEXT", "plaintext", "officer-entered hold justification", "financial record floor"),
    ("pamm_pools", "name"): ("FREE_TEXT", "plaintext", "manager-chosen pool label", "pool lifetime"),
    # ---- Phase-15 instrument lifecycle records --------------------------
    ("instrument_change_requests", "reason"): ("FREE_TEXT", "plaintext", "maker justification — officer-entered", "audit floor"),
    ("instrument_change_log", "reason"): ("FREE_TEXT", "plaintext", "parameter-change rationale — officer-entered", "audit floor"),
    ("market_schedule_overrides", "reason"): ("FREE_TEXT", "plaintext", "ops note — holiday/early-close justification", "audit floor"),
    ("benchmark_fixings", "skip_reason"): ("FREE_TEXT", "plaintext", "machine/officer skip note", "audit floor"),
    ("trade_busts", "reason"): ("FREE_TEXT", "plaintext", "obvious-error rationale — officer-entered", "audit floor"),
    ("instrument_change_requests", "payload"): ("FREE_TEXT", "plaintext JSONB — may embed actor context", "change bundle", "audit floor"),
    # ---- trading records (linkage, immutable) ----------------------------
    ("orders", "account_id"): ("LINKAGE", "n/a (FK)", "all order paths", "MiFID 5y — Art.17(3)(b) immutable"),
    ("orders", "client_order_id"): ("FREE_TEXT", "plaintext", "client-supplied id; may embed identifiers", "MiFID 5y"),
    ("order_audit", "account_id"): ("LINKAGE", "n/a (FK)", "order lifecycle audit", "MiFID 5y — immutable"),
    ("order_audit", "modified_by"): ("PSEUDONYMOUS", "plaintext (user sub / key id / system)", "actor attribution", "MiFID 5y"),
    ("order_audit", "ip_address"): ("PSEUDONYMOUS", "plaintext", "actor attribution", "MiFID 5y"),
    ("trades", "buyer_account_id"): ("LINKAGE", "n/a (FK)", "execution record", "MiFID 5y — immutable"),
    ("trades", "seller_account_id"): ("LINKAGE", "n/a (FK)", "execution record", "MiFID 5y — immutable"),
    ("settlement_instructions", "account_id"): ("LINKAGE", "n/a (FK)", "settlement dispatch", "financial record floor"),
    ("settlement_instructions", "swift_message_id"): ("FINANCIAL", "plaintext", "MT202/pacs.009 ref", "financial record floor"),
    # ---- support / compliance surface ------------------------------------
    ("support_tickets", "account_id"): ("LINKAGE", "n/a (FK)", "support/compliance queues", "complaints record floor (5y, in policy)"),
    ("support_tickets", "subject"): ("FREE_TEXT", "plaintext", "client-authored", "complaints floor"),
    ("support_tickets", "body"): ("FREE_TEXT", "plaintext — may embed PII", "client-authored", "complaints floor"),
    ("support_tickets", "adr_reference"): ("LINKAGE", "plaintext", "ADR routing", "complaints floor"),
    ("ticket_notes", "body"): ("FREE_TEXT", "plaintext — may embed PII", "admin/client notes", "complaints floor"),
    ("unfreeze_requests", "user_id"): ("LINKAGE", "n/a (FK)", "unfreeze workflow", "audit floor"),
    ("unfreeze_requests", "id_document_ref"): ("GOV_ID", "plaintext ref (object pointer)", "re-verification evidence", "audit floor"),
    ("unfreeze_requests", "liveness_ref"): ("GOV_ID", "plaintext ref", "re-verification evidence", "audit floor"),
    ("unfreeze_requests", "note"): ("FREE_TEXT", "plaintext", "request note", "audit floor"),
    ("account_freeze_events", "initiated_by"): ("PSEUDONYMOUS", "n/a (admin id)", "dual-control attribution", "audit floor"),
    ("account_freeze_events", "reason"): ("FREE_TEXT", "plaintext", "freeze rationale", "audit floor"),
    ("account_freeze_events", "metadata"): ("FREE_TEXT", "plaintext JSONB", "freeze context", "audit floor"),
    ("trading_suspensions", "client_ip"): ("PSEUDONYMOUS", "plaintext", "suspension evidence", "audit floor"),
    ("trading_suspensions", "reason"): ("FREE_TEXT", "plaintext", "suspension rationale", "audit floor"),
    # ---- admin/audit plane ------------------------------------------------
    ("admin_audit_log", "admin_user_id"): ("PSEUDONYMOUS", "n/a (admin id)", "who-did-what", "7y audit floor (in policy)"),
    ("admin_audit_log", "ip_address"): ("PSEUDONYMOUS", "plaintext INET", "actor attribution", "7y audit floor"),
    ("admin_audit_log", "before_state"): ("FREE_TEXT", "plaintext JSONB — snapshots may embed PII", "mutation audit", "7y audit floor"),
    ("admin_audit_log", "after_state"): ("FREE_TEXT", "plaintext JSONB — snapshots may embed PII", "mutation audit", "7y audit floor"),
    ("admin_role_bindings", "user_id"): ("LINKAGE", "n/a (FK)", "RBAC", "binding lifetime + audit floor"),
    ("admin_recert_decisions", "decided_by"): ("PSEUDONYMOUS", "n/a (admin id)", "recert attribution", "audit floor"),
    ("client_delegated_users", "user_id"): ("LINKAGE", "n/a (FK)", "delegation binding", "delegation lifetime + audit floor"),
    ("client_delegated_users", "display_name"): ("DIRECT_ID", "plaintext", "client workforce label", "delegation lifetime + audit floor"),
    ("client_delegated_users", "suspend_reason"): ("FREE_TEXT", "plaintext", "ops note", "audit floor"),
    ("client_delegated_users", "revoke_reason"): ("FREE_TEXT", "plaintext", "ops note", "audit floor"),
    ("client_approval_requests", "payload"): ("FREE_TEXT", "plaintext JSONB — operation payload (e.g. beneficiary refs)", "M-of-N approval", "audit floor"),
    ("client_approval_requests", "requested_by_user"): ("PSEUDONYMOUS", "n/a (user id)", "approval attribution", "audit floor"),
    ("client_approval_decisions", "approver_user_id"): ("PSEUDONYMOUS", "n/a (user id)", "vote attribution", "audit floor"),
    ("client_approval_decisions", "note"): ("FREE_TEXT", "plaintext", "vote note", "audit floor"),
    ("client_delegation_events", "actor_user_id"): ("PSEUDONYMOUS", "n/a (user id)", "delegation audit", "audit floor"),
    ("client_delegation_events", "detail"): ("FREE_TEXT", "plaintext JSONB", "delegation audit", "audit floor"),
    # ---- notifications -----------------------------------------------------
    ("notification_deliveries", "user_id"): ("LINKAGE", "n/a (FK)", "delivery tracking", "operational"),
    ("notification_deliveries", "payload"): ("FREE_TEXT", "plaintext JSONB — event data (amounts/refs); recipient address NOT persisted", "dispatch record", "operational"),
    ("notification_dead_letters", "payload"): ("FREE_TEXT", "plaintext JSONB — as deliveries", "DLQ review", "operational"),
    ("notification_preferences", "user_id"): ("LINKAGE", "n/a (PK=FK)", "dispatch policy", "until changed"),
    # ---- webhooks ------------------------------------------------------------
    ("webhook_endpoints", "url"): ("FREE_TEXT", "plaintext — client-supplied endpoint", "delivery target", "endpoint lifetime"),
    ("webhook_endpoints", "secret_enc"): ("AUTH_SECRET", "AES-256-GCM sealed", "HMAC signing", "endpoint lifetime"),
    ("webhook_deliveries", "payload"): ("FREE_TEXT", "plaintext JSONB — event data", "delivery log", "90d purge (in policy)"),
    # ---- other PII-bearing tables --------------------------------------------
    ("liquidity_providers", "contact"): ("ORG_CONTACT", "plaintext JSONB (desk email/phone)", "LP management", "LP lifetime"),
    ("liquidity_providers", "name"): ("ORG_CONTACT", "plaintext (institution)", "LP registry", "LP lifetime"),
    ("listing_proposals", "proposer_id"): ("LINKAGE", "n/a (FK→users)", "instrument listing pipeline", "audit floor"),
    ("vulnerability_disclosures", "reporter_handle"): ("CONTACT", "plaintext (pseudonymous ok)", "VDP correspondence", "disclosure lifetime"),
    ("vulnerability_disclosures", "contact_email"): ("CONTACT", "plaintext", "VDP correspondence", "disclosure lifetime"),
    ("vulnerability_disclosures", "reproduction"): ("FREE_TEXT", "plaintext — PoC may embed user data", "triage", "disclosure lifetime"),
    ("vulnerability_disclosures", "resolution_summary"): ("FREE_TEXT", "plaintext", "triage", "disclosure lifetime"),
}

# Name patterns that force REVIEW when a column isn't curated — keeps the
# inventory honest as the schema grows.
PII_RE = re.compile(
    r"(email|phone|_name|^name_|^name$|addr|iban|account_number|account_no|"
    r"routing|bic|dob|birth|tin|ssn|passport|document|_ip$|^ip_|inet|"
    r"agent|fingerprint|geo_|recipient|sender|originator|beneficiary|"
    r"legal|contact|secret|password|token|note$|notes$|reason$|body$|"
    r"subject$|handle|payload|response_body)", re.I)

# Columns matching PII_RE that were REVIEWED and confirmed non-PII —
# (table, column) -> justification. Fail-closed: anything matching
# PII_RE that is neither ANNOTATIONS nor NONPII fails the run.
NONPII = {
    ("orders", "expiry_reason"): "order TIF expiry metadata",
    ("audit_hash_chain", "table_name"): "audit target table name",
    ("audit_hash_chain", "payload_hash"): "SHA-256 digest — irreversible",
    ("risk_limits", "max_account_notional"): "risk config scalar",
    ("fee_tiers", "tier_name"): "fee tier label",
    ("commission_tiers", "tier_name"): "commission tier label",
    ("nostro_accounts", "bank_name"): "venue-owned correspondent account — not client data",
    ("nostro_accounts", "account_number"): "venue-owned nostro account — not client data",
    ("nostro_accounts", "iban"): "venue-owned nostro account — not client data",
    ("nostro_movements", "confirmation_ref"): "venue treasury reference",
    ("settlement_instructions", "confirmation_ref"): "settlement confirmation reference",
    ("settlement_instructions", "message_payload"): "outbound SWIFT/ISO envelope — account ids + amounts only",
    ("api_keys", "revoke_reason"): "enum vocabulary, not free text",
    ("chart_of_accounts", "account_name"): "GL account label (venue books)",
    ("bank_accounts", "rejection_reason"): "admin-entered vocabulary",
    ("ticket_notes", "note_id"): "primary key",
    ("client_role_bindings", "revoke_reason"): "admin vocabulary",
    ("client_approval_requests", "fingerprint"): "SHA-256 dedup digest",
    ("vulnerability_disclosures", "bulletin_ref"): "bulletin id (EXC-SA-…)",
    ("vulnerability_disclosures", "patch_ref"): "commit/tag/PR reference",
    ("vulnerability_disclosures", "sbom_ref"): "SBOM artifact path/digest",
    ("admin_role_bindings", "suspend_reason"): "RBAC lifecycle vocabulary",
    ("admin_role_bindings", "revoke_reason"): "RBAC lifecycle vocabulary",
    ("admin_break_glass_grants", "incident_ref"): "incident id reference",
    ("environments", "name"): "deploy environment name (dev/staging/prod)",
    ("release_promotions", "reason"): "release-train vocabulary",
    ("deploy_windows", "reason"): "deploy window note",
    ("strategy_profiles", "incubating_since"): "incubation start timestamp — no person data",

    ("governance_packs", "release_reason"): "governance pack vocabulary",
    ("suspense_account_mappings", "name_match_score"): "derived Jaro-Winkler score",
    ("suspense_account_mappings", "unmatched_reason"): "match-diagnostics enum vocabulary",
    ("suspense_account_mappings", "quarantine_status"): "lifecycle enum",
    ("suspense_account_mappings", "quarantined_at"): "timestamp",
    ("rail_payments", "return_reason"): "bank R-code text",
    ("carry_trade_allocations", "bot_ref"): "Phase-16 bot registry handle",
    ("partition_archive_log", "partition_name"): "partition identifier",
    ("partition_tier_state", "partition_name"): "partition identifier",
    ("partition_tier_log", "partition_name"): "partition identifier",
    ("currency_holidays", "name"): "holiday name",
    ("oauth_clients", "last_token_at"): "timestamp",
    ("order_audit", "field_name"): "changed-field label",
    ("transfers", "failure_reason"): "enum-ish failure vocabulary",
    ("announcements", "body"): "admin-authored broadcast; venue voice, moderation-reviewed",
    ("webhook_endpoints", "prev_secret_enc"): "AES-256-GCM sealed rotation overlap",
    ("webhook_endpoints", "secret_overlap_until"): "timestamp",
    ("fee_promo_windows", "note"): "ops note on promo window",
    ("manual_liquidations", "reason"): "admin vocabulary",
    ("feature_flags", "name"): "flag identifier",
    ("data_retention_holds", "partition_name"): "partition identifier",
    ("data_retention_holds", "case_ref"): "regulator case/SAR id — reference, not person data",
    ("data_retention_holds", "reason"): "hold justification (compliance-entered)",
    ("retention_audit_log", "check_name"): "enforcer check label",
    ("withdrawal_dispatch_queue", "reason"): "dispatch queue vocabulary",
    ("nostro_replenishment_requests", "decision_note"): "approver note (venue treasury)",
    ("trading_suspensions", "cleared_reason"): "admin vocabulary",
    ("kyc_ops_matrix", "document_type"): "requirement vocabulary (PASSPORT/…)",
    ("kyc_ops_matrix", "notes"): "matrix note (policy data)",
    ("tax_self_certifications", "tin_kind"): "enum SSN|EIN|ITIN — format class, not the number",
    ("tax_self_certifications", "tin_validated_at"): "timestamp",
    ("circuit_breaker_events", "reason"): "breaker vocabulary",
    ("reconciliation_findings", "subject"): "recon break subject (ledger refs)",
    ("solvency_snapshots", "signed_payload"): "Merkle proof bytes",
    ("solvency_snapshots", "signer_fingerprint"): "key fingerprint of signer",
    ("server_actions", "reason"): "fleet action vocabulary",
    ("admin_break_glass_grants", "reason"): "grant justification vocabulary",
    ("deposit_confirmations", "source"): "confirmation source vocabulary",
    ("funding_transactions", "payload_sha256"): "SHA-256 dedup digest",
    ("journal_entries", "payload_sha256"): "SHA-256 posting digest",
    ("transfers", "payload_sha256"): "SHA-256 dedup digest",
    ("chargeback_evidence", "payload_sha256"): "SHA-256 integrity digest",
}

# Curated FREE_TEXT-class additions for ops/audit JSONB + note columns
# that can incidentally embed PII (kept in the PII table deliberately).
EXTRA_FREE_TEXT = {
    ("idempotency_keys", "response_body"): ("FREE_TEXT",
        "cached response JSON — may embed account-scoped fields",
        "safe-retry replay", "7d purge (in policy)"),
    ("admin_dual_control_requests", "payload"): ("FREE_TEXT",
        "plaintext JSONB — operation payload", "dual-control review", "audit floor"),
    ("admin_dual_control_requests", "reason"): ("FREE_TEXT",
        "plaintext", "dual-control request", "audit floor"),
    ("admin_break_glass_grants", "review_notes"): ("FREE_TEXT",
        "plaintext", "break-glass review", "audit floor"),
    ("server_actions", "payload"): ("FREE_TEXT",
        "plaintext JSONB — fleet command payload", "fleet ops", "ops log floor"),
    ("releases", "notes"): ("FREE_TEXT",
        "plaintext — release notes", "release mgmt", "ops log floor"),
    ("listing_proposals", "reason"): ("FREE_TEXT",
        "plaintext — reviewer-entered text", "listing review", "audit floor"),
    ("vulnerability_disclosures", "dispute_reason"): ("FREE_TEXT",
        "plaintext", "VDP dispute handling", "disclosure lifetime"),
}

RECLASS = {
    "orders": "MiFID RTS 6 order records — 5y, Art.17(3)(b)",
    "trades": "MiFID RTS 6 trade records — 5y, Art.17(3)(b)",
    "order_audit": "MiFID RTS 6 order audit — 5y, Art.17(3)(b)",
}

CREATE_RE = re.compile(r"CREATE TABLE\s+(?:IF NOT EXISTS\s+)?([a-z_][a-z0-9_]*)\s*\(")
ALTER_RE = re.compile(r"ALTER TABLE\s+([a-z_][a-z0-9_]*)")
ADDCOL_RE = re.compile(
    r"ADD COLUMN\s+(?:IF NOT EXISTS\s+)?([a-z_][a-z0-9_]*)\s+"
    r"([A-Za-z][A-Za-z0-9_]*(?:\([^)]*\))?(?:\[\])?)")
ALTERTYPE_RE = re.compile(
    r"ALTER COLUMN\s+([a-z_][a-z0-9_]*)\s+TYPE\s+"
    r"([A-Za-z][A-Za-z0-9_]*(?:\([^)]*\))?(?:\[\])?)")
COL_RE = re.compile(
    r"^\s*([a-z_][a-z0-9_]*)\s+([A-Za-z][A-Za-z0-9_]*(?:\([^)]*\))?(?:\[\])?)")
SKIP = {"constraint", "unique", "primary", "foreign", "check", "exclude",
        "like", "partition", "create", "alter", "select", "insert"}


def parse_migrations():
    tables: dict[str, dict[str, str]] = {}
    order: list[str] = []
    for fn in sorted(os.listdir(MIG)):
        if not fn.endswith(".up.sql"):
            continue
        raw = open(os.path.join(MIG, fn), encoding="utf-8").read()
        # Strip -- line comments first: comment text carries unbalanced
        # parens and ';' that corrupt depth/statement boundaries.
        src = "\n".join(l.split("--", 1)[0] for l in raw.splitlines())
        for m in CREATE_RE.finditer(src):
            t = m.group(1)
            if t not in tables:
                tables[t] = {}
                order.append(t)
            # find the column block: from m.end() to the closing ');' at
            # top level — approximation: scan until line ');' or ')' at col0
            block_start = m.end()
            depth = 1
            i = block_start
            while i < len(src) and depth > 0:
                if src[i] == "(":
                    depth += 1
                elif src[i] == ")":
                    depth -= 1
                i += 1
            body = src[block_start:i - 1]
            for line in body.splitlines():
                line = line.split("--")[0].rstrip()
                cm = COL_RE.match(line)
                if not cm:
                    continue
                col = cm.group(1)
                if col.lower() in SKIP or col.startswith(")"):
                    continue
                if col not in tables[t]:
                    tables[t][col] = cm.group(2)
        for am in ALTER_RE.finditer(src):
            t = am.group(1)
            if t not in tables:
                tables[t] = {}
                order.append(t)
            # Bound the window at the statement's terminating ';' so
            # following ALTER/CREATE statements don't bleed in.
            semi = src.find(";", am.end())
            tail = src[am.end():semi if semi != -1 else am.end() + 4000]
            for cm in ADDCOL_RE.finditer(tail):
                col = cm.group(1)
                if col not in tables[t]:
                    tables[t][col] = cm.group(2).strip()
            for tm in ALTERTYPE_RE.finditer(tail):
                if tm.group(1) in tables[t]:
                    tables[t][tm.group(1)] = tm.group(2)
    return tables, order


def build_rows(tables, order):
    """Returns (pii_rows, review_rows, all_tables)."""
    pii_rows = []
    review = []
    for t in order:
        for col in tables[t]:
            key = (t, col)
            if key in ANNOTATIONS:
                cls, enc, access, ret = ANNOTATIONS[key]
                pii_rows.append((t, col, tables[t][col], cls, enc, access, ret))
            elif key in EXTRA_FREE_TEXT:
                cls, enc, access, ret = EXTRA_FREE_TEXT[key]
                pii_rows.append((t, col, tables[t][col], cls, enc, access, ret))
            elif key in NONPII:
                continue
            elif PII_RE.search(col):
                review.append((t, col, tables[t][col]))
    # Staleness guard: a NONPII/EXTRA_FREE_TEXT/ANNOTATIONS key for a
    # column that no longer exists means the map drifted — flag it.
    seen = {(t, c) for t in order for c in tables[t]}
    for key in list(ANNOTATIONS) + list(EXTRA_FREE_TEXT) + list(NONPII):
        if key not in seen:
            review.append((key[0], key[1], "<stale annotation — column absent>"))
    return pii_rows, review


def render(tables, order, pii_rows, review):
    now = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
    by_table: dict[str, list] = {}
    for r in pii_rows:
        by_table.setdefault(r[0], []).append(r)

    out = []
    out.append("# PII Inventory — GDPR Art. 30 Record + Encryption/Access Map")
    out.append("")
    out.append(f"**Generated:** {now} by `scripts/security/gen-pii-inventory.py` "
               f"(Task 13.5.3.2) from {sum(1 for f in os.listdir(MIG) if f.endswith('.up.sql'))} "
               f"`*.up.sql` migrations ({len(tables)} tables, "
               f"{sum(len(c) for c in tables.values())} columns). Do not "
               "hand-edit; update the generator's ANNOTATIONS map and re-run. "
               "Companion artifacts: `pii-catalog.csv` (same rows, machine-"
               "checkable), `pii-audit-report.md` (verification evidence), "
               "`gdpr-erasure-runbook.md` (Art. 17 procedure).")
    out.append("")
    out.append("PII classes: **DIRECT_ID** (name/address/residency) · "
               "**CONTACT** (email/phone) · **GOV_ID** (TIN/ID documents) · "
               "**FINANCIAL** (bank identifiers) · **AUTH_SECRET** "
               "(credentials — hashed/sealed, tracked for erasure) · "
               "**PSEUDONYMOUS** (IP/UA/fingerprint/geo/actor ids) · "
               "**LINKAGE** (user_id/account_id re-identification joins) · "
               "**FREE_TEXT** (may embed incidental PII) · **ORG_CONTACT** "
               "(institutional contacts).")
    out.append("")
    out.append("Retention classes reference `infrastructure/data-tiering/"
               "tiering_policy.yaml` (enforced nightly by "
               "`internal/operations/retention`) or the GDPR runbook's "
               "lifecycle rules when a table is outside the enforcer's "
               "schedule.")
    out.append("")
    out.append("## 1. PII-bearing columns")
    out.append("")
    for t in order:
        if t not in by_table:
            continue
        out.append(f"### `{t}`")
        out.append("")
        out.append("| column | type | class | encryption at rest | access path | retention |")
        out.append("|---|---|---|---|---|---|")
        for (_, col, typ, cls, enc, access, ret) in by_table[t]:
            out.append(f"| `{col}` | {typ} | {cls} | {enc} | {access} | {ret} |")
        out.append("")
    out.append("## 2. Columns pending review")
    out.append("")
    if review:
        out.append("> **FAIL — the generator exits non-zero while REVIEW "
                   "rows exist.** These columns match PII name patterns "
                   "but have no ANNOTATIONS entry: classify them in the "
                   "generator.")
        out.append("")
        for (t, col, typ) in review:
            out.append(f"- `{t}.{col}` ({typ})")
    else:
        out.append("None — every PII-name-pattern column carries a curated "
                   "classification.")
    out.append("")
    out.append("## 3. Tables with no personal data")
    out.append("")
    out.append("Schema-verified by the generator (no PII-name-pattern "
               "column and no curated annotation). Venue-owned operational/"
               "financial state: balances, positions, instruments, GL, "
               "nostro, margin, market data, fleet/ops, pricing config.")
    out.append("")
    nopii = [t for t in order if t not in by_table]
    line = ""
    for t in nopii:
        line += f"`{t}` "
        if len(line) > 100:
            out.append(line.rstrip())
            line = ""
    if line:
        out.append(line.rstrip())
    out.append("")
    out.append("## 4. Data-flow summary")
    out.append("")
    out.append("| Stage | Path |")
    out.append("|---|---|")
    out.append("| Collection | `POST /api/v1/auth/register` (email/password/country) → `auth.UserStore` → `users`; "
               "`PUT /account/profile` (full_name/address/phone); `POST /kyc/submit` → S3 SSE-KMS objects + `kyc_documents`/`kyc_submissions`; "
               "`POST /kyc/self-certification` → `tax_self_certifications`; `POST /funding/bank-accounts` → `bank_accounts`; "
               "login events → `login_history`; admin/deposit ingestion → `deposit_confirmations`/`suspense_account_mappings` |")
    out.append("| Storage | PostgreSQL 16 OLTP (columns above); KYC document bytes in S3 SSE-KMS (`upload.go` sets `aws:kms` + KMS key id, "
               "`kyc_documents.sse_algorithm` records it); sessions in Redis (`session:{sid}` hash: IP+UA, TTL-bound); "
               "secrets sealed via `auth.SecretBox` AES-256-GCM (`secrets.data_key`, prod source Vault/KMS) |")
    out.append("| Processing | matching/risk never touches PII columns — they join on `account_id`/`user_id` linkage only; "
               "notification dispatch resolves `users.email`/`phone` at send time (never persisted to `notification_deliveries`); "
               "admin views read PII-bearing rows through audited (`support_view`) and unaudited (see findings) paths |")
    out.append("| Deletion | per-table procedure in `gdpr-erasure-runbook.md`; legal holds via `data_retention_holds` "
               "(enforced by `archiver` + retention enforcer); KYC lifecycle-bound = account close + 5y |")
    out.append("")
    return "\n".join(out)


def main():
    check = "--check" in sys.argv
    tables, order = parse_migrations()
    pii_rows, review = build_rows(tables, order)
    doc = render(tables, order, pii_rows, review)

    if check:
        existing = open(DOC_OUT, encoding="utf-8").read() if os.path.exists(DOC_OUT) else ""
        # compare ignoring the generated timestamp line
        norm = lambda s: re.sub(r"\*\*Generated:\*\* .* by", "**Generated:** <ts> by", s)
        if norm(existing) != norm(doc):
            print("FAIL: pii-inventory.md is stale — re-run gen-pii-inventory.py")
            return 1
        print("OK: pii-inventory.md matches generator output")
    else:
        os.makedirs(os.path.dirname(DOC_OUT), exist_ok=True)
        with open(DOC_OUT, "w", encoding="utf-8") as f:
            f.write(doc)
        with open(CSV_OUT, "w", newline="", encoding="utf-8") as f:
            w = csv.writer(f)
            w.writerow(["table", "column", "type", "pii_class",
                        "encryption", "access_path", "retention"])
            for r in pii_rows:
                w.writerow(r)
        print(f"wrote {DOC_OUT} ({len(pii_rows)} PII columns, "
              f"{len(tables)} tables) + {CSV_OUT}")
    if review:
        print(f"FAIL: {len(review)} unclassified PII-pattern columns", file=sys.stderr)
        for t, c, _ in review:
            print(f"  REVIEW {t}.{c}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
