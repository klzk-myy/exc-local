# PII Inventory — GDPR Art. 30 Record + Encryption/Access Map

**Generated:** 2026-09-29 21:01 UTC by `scripts/security/gen-pii-inventory.py` (Task 13.5.3.2) from 137 `*.up.sql` migrations (186 tables, 2152 columns). Do not hand-edit; update the generator's ANNOTATIONS map and re-run. Companion artifacts: `pii-catalog.csv` (same rows, machine-checkable), `pii-audit-report.md` (verification evidence), `gdpr-erasure-runbook.md` (Art. 17 procedure).

PII classes: **DIRECT_ID** (name/address/residency) · **CONTACT** (email/phone) · **GOV_ID** (TIN/ID documents) · **FINANCIAL** (bank identifiers) · **AUTH_SECRET** (credentials — hashed/sealed, tracked for erasure) · **PSEUDONYMOUS** (IP/UA/fingerprint/geo/actor ids) · **LINKAGE** (user_id/account_id re-identification joins) · **FREE_TEXT** (may embed incidental PII) · **ORG_CONTACT** (institutional contacts).

Retention classes reference `infrastructure/data-tiering/tiering_policy.yaml` (enforced nightly by `internal/operations/retention`) or the GDPR runbook's lifecycle rules when a table is outside the enforcer's schedule.

## 1. PII-bearing columns

### `users`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `email` | VARCHAR(255) | CONTACT | plaintext (normalized lowercase) | POST /api/v1/auth/register · UserByEmail login lookup · notifications Recipient | account lifetime → pseudonymized at closure (runbook §3) |
| `phone` | VARCHAR(32) | CONTACT | plaintext | notifications Recipient (SMS channel) · PUT /account/profile | account lifetime → pseudonymized at closure |
| `totp_secret` | VARCHAR(160) | AUTH_SECRET | AES-256-GCM SecretBox (base64 nonce‖ct) | auth TOTP verify/disable (auth.UserStore, PgxTOTPSecrets) | until 2FA disabled/closure |
| `kyc_status` | kyc_status_enum | LINKAGE | plaintext | auth gates, compliance review | account lifetime |
| `password_hash` | VARCHAR(128) | AUTH_SECRET | bcrypt cost-12 | auth login/change-password compare only | rotation on change; erased at closure |
| `country` | VARCHAR(2) | DIRECT_ID | plaintext | POST /auth/register · residency/geo gates | account lifetime |
| `full_name` | VARCHAR(128) | DIRECT_ID | plaintext | PUT /account/profile (self-declared) | account lifetime |
| `address` | VARCHAR(255) | DIRECT_ID | plaintext | PUT /account/profile (self-declared) | account lifetime |
| `email_verified_at` | TIMESTAMPTZ | LINKAGE | plaintext | verification ceremony timestamp | account lifetime |
| `totp_backup_codes` | TEXT[] | AUTH_SECRET | SHA-256 digests (10) | auth backup-code consume | until 2FA disabled/closure |
| `anti_phishing_code` | VARCHAR(32) | FREE_TEXT | plaintext (by design — rendered into outbound mail) | notifications anti-phish banner | until changed |

### `accounts`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (FK) | every account-scoped read joins here | account lifetime + financial-record floor |

### `orders`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | all order paths | MiFID 5y — Art.17(3)(b) immutable |
| `client_order_id` | VARCHAR(64) | FREE_TEXT | plaintext | client-supplied id; may embed identifiers | MiFID 5y |

### `trades`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `buyer_account_id` | BIGINT | LINKAGE | n/a (FK) | execution record | MiFID 5y — immutable |
| `seller_account_id` | BIGINT | LINKAGE | n/a (FK) | execution record | MiFID 5y — immutable |

### `funding_transactions`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | funding lifecycle | financial record floor (7y) |
| `reference` | VARCHAR(128) | FINANCIAL | plaintext | bank reference / reconciliation | financial record floor |
| `reference_account` | VARCHAR(64) | FINANCIAL | plaintext | deposit source account | financial record floor |
| `originator_name` | VARCHAR(255) | DIRECT_ID | plaintext | third-party deposit screen | financial record floor |

### `withdrawal_confirmations`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `confirmed_by` | BIGINT | PSEUDONYMOUS | n/a (user id) | confirm attribution | audit floor |
| `token_hash` | CHAR(64) | AUTH_SECRET | SHA-256 | confirm-token compare | 15-min window + audit |

### `admin_audit_log`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `admin_user_id` | BIGINT | PSEUDONYMOUS | n/a (admin id) | who-did-what | 7y audit floor (in policy) |
| `before_state` | JSONB | FREE_TEXT | plaintext JSONB — snapshots may embed PII | mutation audit | 7y audit floor |
| `after_state` | JSONB | FREE_TEXT | plaintext JSONB — snapshots may embed PII | mutation audit | 7y audit floor |
| `ip_address` | INET | PSEUDONYMOUS | plaintext INET | actor attribution | 7y audit floor |

### `kyc_documents`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | KYC review, support view | account close +5y (AML) |
| `type` | VARCHAR(32) | FREE_TEXT | plaintext | document type label | account close +5y |
| `file_url` | VARCHAR(512) | GOV_ID | pointer — object bytes are SSE-KMS | S3 key kyc/<acct>/<sub>/<type>-<sha16>.<ext> (upload.go:274) | account close +5y; object deleted with doc |
| `submission_id` | BIGINT | LINKAGE | n/a (FK) | submission join | account close +5y |
| `sha256` | VARCHAR(64) | LINKAGE | integrity digest | integrity verify | account close +5y |

### `settlement_instructions`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | settlement dispatch | financial record floor |
| `swift_message_id` | VARCHAR(64) | FINANCIAL | plaintext | MT202/pacs.009 ref | financial record floor |

### `api_keys`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (FK) | auth forensics | key lifetime + audit floor |
| `key_hash` | VARCHAR(64) | AUTH_SECRET | SHA-256 | auth lookup | key lifetime |
| `secret_enc` | BYTEA | AUTH_SECRET | AES-256-GCM SecretBox | HMAC verify (sealed) | key lifetime |
| `label` | VARCHAR(128) | FREE_TEXT | plaintext | user-chosen key label | key lifetime |
| `ip_allowlist` | TEXT[] | PSEUDONYMOUS | plaintext | auth allowlist check | key lifetime |
| `last_used_ip` | INET | PSEUDONYMOUS | plaintext | usage telemetry | key lifetime |

### `notification_deliveries`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (FK) | delivery tracking | operational |
| `payload` | JSONB | FREE_TEXT | plaintext JSONB — event data (amounts/refs); recipient address NOT persisted | dispatch record | operational |

### `notification_dead_letters`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `payload` | JSONB | FREE_TEXT | plaintext JSONB — as deliveries | DLQ review | operational |

### `bank_accounts`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | beneficiary registry | financial record floor |
| `iban` | VARCHAR(34) | FINANCIAL | plaintext — FINDING PII-F2 (justified for rails; audit gap PII-F3) | withdrawal allowlist gate, SWIFT/SEPA dispatch | financial record floor |
| `account_number` | VARCHAR(64) | FINANCIAL | plaintext — FINDING PII-F2 | withdrawal allowlist gate, ACH/FedNow dispatch | financial record floor |
| `swift_bic` | VARCHAR(16) | FINANCIAL | plaintext | rail routing | financial record floor |
| `bic_routing` | VARCHAR(32) | FINANCIAL | plaintext | rail routing | financial record floor |
| `bank_name` | VARCHAR(128) | FINANCIAL | plaintext | registry view | financial record floor |
| `beneficiary_name` | VARCHAR(255) | DIRECT_ID | plaintext — must match KYC legal name (finding PII-F2) | name-match screen + admin verify | financial record floor |

### `support_tickets`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | support/compliance queues | complaints record floor (5y, in policy) |
| `subject` | VARCHAR(255) | FREE_TEXT | plaintext | client-authored | complaints floor |
| `body` | TEXT | FREE_TEXT | plaintext — may embed PII | client-authored | complaints floor |
| `adr_reference` | VARCHAR(128) | LINKAGE | plaintext | ADR routing | complaints floor |

### `ticket_notes`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `body` | TEXT | FREE_TEXT | plaintext — may embed PII | admin/client notes | complaints floor |

### `trade_busts`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reason` | VARCHAR(255) | FREE_TEXT | plaintext | obvious-error rationale — officer-entered | audit floor |

### `account_closures`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reason` | TEXT | FREE_TEXT | plaintext | closure record — client/officer-entered | financial record floor |

### `webauthn_credentials`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (FK) | passkey ceremonies | credential lifetime; revoked≠deleted (forensic) |
| `credential_id` | BYTEA | AUTH_SECRET | public credential id | assertion lookup | credential lifetime |
| `public_key` | BYTEA | AUTH_SECRET | public key (not secret) | assertion verify | credential lifetime |
| `name` | VARCHAR(128) | FREE_TEXT | plaintext | user-chosen device label | credential lifetime |

### `login_history`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (FK) | device/security page | security telemetry (see report — retention class gap) |
| `ip` | INET | PSEUDONYMOUS | plaintext INET | login audit, anomaly detection | security telemetry |
| `user_agent` | TEXT | PSEUDONYMOUS | plaintext | device management view | security telemetry |
| `device_fingerprint` | TEXT | PSEUDONYMOUS | plaintext | device management view | security telemetry |
| `geo_city` | VARCHAR(128) | PSEUDONYMOUS | plaintext | login audit | security telemetry |
| `geo_country` | VARCHAR(2) | PSEUDONYMOUS | plaintext | login audit | security telemetry |
| `session_id` | VARCHAR(128) | PSEUDONYMOUS | plaintext | session correlation | security telemetry |

### `client_delegated_users`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (FK) | delegation binding | delegation lifetime + audit floor |
| `display_name` | VARCHAR(128) | DIRECT_ID | plaintext | client workforce label | delegation lifetime + audit floor |
| `suspend_reason` | VARCHAR(128) | FREE_TEXT | plaintext | ops note | audit floor |
| `revoke_reason` | TEXT | FREE_TEXT | plaintext | ops note | audit floor |

### `client_approval_requests`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `payload` | JSONB | FREE_TEXT | plaintext JSONB — operation payload (e.g. beneficiary refs) | M-of-N approval | audit floor |
| `requested_by_user` | BIGINT | PSEUDONYMOUS | n/a (user id) | approval attribution | audit floor |

### `client_approval_decisions`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `approver_user_id` | BIGINT | PSEUDONYMOUS | n/a (user id) | vote attribution | audit floor |
| `note` | TEXT | FREE_TEXT | plaintext | vote note | audit floor |

### `client_delegation_events`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `actor_user_id` | BIGINT | PSEUDONYMOUS | n/a (user id) | delegation audit | audit floor |
| `detail` | JSONB | FREE_TEXT | plaintext JSONB | delegation audit | audit floor |

### `order_lists`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `fail_reason` | VARCHAR(64) | FREE_TEXT | plaintext | list-leg validation text — may echo client params | order record floor |

### `strategy_templates`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `name` | VARCHAR(128) | FREE_TEXT | plaintext | manager-chosen public label | template lifetime |
| `reject_reason` | VARCHAR(255) | FREE_TEXT | plaintext | approver-entered review note | template lifetime |

### `strategy_runs`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `skip_reason` | VARCHAR(64) | FREE_TEXT | plaintext | machine/officer skip note | run record |

### `vulnerability_disclosures`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reproduction` | TEXT | FREE_TEXT | plaintext — PoC may embed user data | triage | disclosure lifetime |
| `reporter_handle` | VARCHAR(128) | CONTACT | plaintext (pseudonymous ok) | VDP correspondence | disclosure lifetime |
| `contact_email` | VARCHAR(320) | CONTACT | plaintext | VDP correspondence | disclosure lifetime |
| `resolution_summary` | TEXT | FREE_TEXT | plaintext | triage | disclosure lifetime |
| `dispute_reason` | TEXT | FREE_TEXT | plaintext | VDP dispute handling | disclosure lifetime |

### `admin_role_bindings`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (FK) | RBAC | binding lifetime + audit floor |

### `admin_dual_control_requests`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `payload` | JSONB | FREE_TEXT | plaintext JSONB — operation payload | dual-control review | audit floor |
| `reason` | TEXT | FREE_TEXT | plaintext | dual-control request | audit floor |

### `admin_recert_decisions`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `decided_by` | BIGINT | PSEUDONYMOUS | n/a (admin id) | recert attribution | audit floor |

### `admin_break_glass_grants`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `review_notes` | TEXT | FREE_TEXT | plaintext | break-glass review | audit floor |

### `server_actions`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `payload` | JSONB | FREE_TEXT | plaintext JSONB — fleet command payload | fleet ops | ops log floor |

### `releases`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `notes` | TEXT | FREE_TEXT | plaintext — release notes | release mgmt | ops log floor |

### `listing_proposals`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `proposer_id` | BIGINT | LINKAGE | n/a (FK→users) | instrument listing pipeline | audit floor |
| `reason` | TEXT | FREE_TEXT | plaintext — reviewer-entered text | listing review | audit floor |

### `swapfree_verifications`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `decision_note` | TEXT | FREE_TEXT | plaintext | verifier note | account close +5y |

### `strategy_profiles`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `display_name` | VARCHAR(128) | FREE_TEXT | plaintext | manager-chosen public label | strategy lifetime |
| `suspend_reason` | VARCHAR(255) | FREE_TEXT | plaintext | compliance note | strategy lifetime |

### `suspense_account_mappings`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `originator_name` | VARCHAR(255) | DIRECT_ID | plaintext | quarantine routing screen | financial record floor |
| `originator_account` | VARCHAR(64) | FINANCIAL | plaintext | quarantine routing screen | financial record floor |
| `resolution_notes` | TEXT | FREE_TEXT | plaintext | ops resolution note | financial record floor |

### `oauth_clients`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `client_secret_hash` | VARCHAR(100) | AUTH_SECRET | bcrypt-12 | client-credentials grant | client lifetime |
| `name` | VARCHAR(128) | FREE_TEXT | plaintext | client display label | client lifetime |

### `account_freeze_events`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reason` | TEXT | FREE_TEXT | plaintext | freeze rationale | audit floor |
| `initiated_by` | BIGINT | PSEUDONYMOUS | n/a (admin id) | dual-control attribution | audit floor |
| `metadata` | JSONB | FREE_TEXT | plaintext JSONB | freeze context | audit floor |

### `order_audit`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | order lifecycle audit | MiFID 5y — immutable |
| `modified_by` | VARCHAR(128) | PSEUDONYMOUS | plaintext (user sub / key id / system) | actor attribution | MiFID 5y |
| `ip_address` | VARCHAR(64) | PSEUDONYMOUS | plaintext | actor attribution | MiFID 5y |

### `idempotency_keys`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `response_body` | JSONB | FREE_TEXT | cached response JSON — may embed account-scoped fields | safe-retry replay | 7d purge (in policy) |

### `chargebacks`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reason` | TEXT | FREE_TEXT | plaintext | dispute record | financial record floor |
| `resolution_note` | TEXT | FREE_TEXT | plaintext | dispute record | financial record floor |

### `chargeback_evidence`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `payload` | JSONB | FREE_TEXT | plaintext JSONB — may embed account/trade linkage | evidence bundle | financial record floor |

### `webhook_endpoints`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `url` | TEXT | FREE_TEXT | plaintext — client-supplied endpoint | delivery target | endpoint lifetime |
| `secret_enc` | BYTEA | AUTH_SECRET | AES-256-GCM sealed | HMAC signing | endpoint lifetime |

### `webhook_deliveries`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `payload` | JSONB | FREE_TEXT | plaintext JSONB — event data | delivery log | 90d purge (in policy) |

### `liquidity_providers`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `name` | VARCHAR(128) | ORG_CONTACT | plaintext (institution) | LP registry | LP lifetime |
| `contact` | JSONB | ORG_CONTACT | plaintext JSONB (desk email/phone) | LP management | LP lifetime |

### `deposit_confirmations`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `sender_name` | VARCHAR(255) | DIRECT_ID | plaintext | dual-source bank verify | financial record floor |
| `sender_account` | VARCHAR(64) | FINANCIAL | plaintext | dual-source bank verify | financial record floor |
| `payload_sha256` | CHAR(64) | LINKAGE | sha256 of source payload | dedup | financial record floor |

### `withdrawal_destination_holds`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `destination` | VARCHAR(64) | FINANCIAL | plaintext (normalized) | first-seen destination hold | financial record floor |

### `trading_suspensions`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reason` | TEXT | FREE_TEXT | plaintext | suspension rationale | audit floor |
| `client_ip` | VARCHAR(64) | PSEUDONYMOUS | plaintext | suspension evidence | audit floor |

### `unfreeze_requests`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (FK) | unfreeze workflow | audit floor |
| `id_document_ref` | VARCHAR(255) | GOV_ID | plaintext ref (object pointer) | re-verification evidence | audit floor |
| `liveness_ref` | VARCHAR(255) | GOV_ID | plaintext ref | re-verification evidence | audit floor |
| `note` | TEXT | FREE_TEXT | plaintext | request note | audit floor |

### `notification_preferences`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `user_id` | BIGINT | LINKAGE | n/a (PK=FK) | dispatch policy | until changed |

### `kyc_submissions`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | review pipeline | account close +5y |
| `jurisdiction` | CHAR(2) | DIRECT_ID | plaintext (ISO country) | risk/tier policy | account close +5y |
| `risk_score` | SMALLINT | LINKAGE | plaintext | intake scoring | account close +5y |
| `reviewer_id` | BIGINT | PSEUDONYMOUS | n/a (admin id) | review attribution | account close +5y |
| `reject_reason` | TEXT | FREE_TEXT | plaintext | reviewer note | account close +5y |

### `tax_self_certifications`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `account_id` | BIGINT | LINKAGE | n/a (FK) | CRS/FATCA reporting (Phase-21) | statutory tax retention |
| `tin` | VARCHAR(32) | GOV_ID | deprecated plaintext — migration-210 read-fallback window only; NULL post-backfill (PII-F1 remediated) | legacy read fallback only (compliance.ListSelfCerts); never written | statutory tax retention |
| `tin_country` | CHAR(2) | GOV_ID | plaintext | TIN validation | statutory tax retention |
| `fields` | JSONB | GOV_ID | deprecated plaintext JSONB — migration-210 read-fallback window only; '{}' post-backfill (PII-F1 remediated) | legacy read fallback only; never written | statutory tax retention |
| `tin_sealed` | BYTEA | GOV_ID | AES-256-GCM SecretBox nonce‖ct (secrets.data_key; Vault/KMS prod) | compliance.InsertSelfCert writes; ListSelfCerts unseals; CRS/FATCA seam reads via box | statutory tax retention |
| `fields_sealed` | BYTEA | GOV_ID | AES-256-GCM SecretBox nonce‖ct of the whole fields document (legal_name/address/entity/treaty) | compliance.InsertSelfCert writes; ListSelfCerts unseals; Phase-21 reporting via box | statutory tax retention |

### `compliance_holds`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reason` | TEXT | FREE_TEXT | plaintext | officer-entered hold justification | financial record floor |

### `pamm_pools`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `name` | VARCHAR(128) | FREE_TEXT | plaintext | manager-chosen pool label | pool lifetime |

### `instrument_change_requests`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `payload` | JSONB | FREE_TEXT | plaintext JSONB — may embed actor context | change bundle | audit floor |
| `reason` | TEXT | FREE_TEXT | plaintext | maker justification — officer-entered | audit floor |

### `instrument_change_log`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reason` | TEXT | FREE_TEXT | plaintext | parameter-change rationale — officer-entered | audit floor |

### `market_schedule_overrides`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `reason` | TEXT | FREE_TEXT | plaintext | ops note — holiday/early-close justification | audit floor |

### `benchmark_fixings`

| column | type | class | encryption at rest | access path | retention |
|---|---|---|---|---|---|
| `skip_reason` | TEXT | FREE_TEXT | plaintext | machine/officer skip note | audit floor |

## 2. Columns pending review

None — every PII-name-pattern column carries a curated classification.

## 3. Tables with no personal data

Schema-verified by the generator (no PII-name-pattern column and no curated annotation). Venue-owned operational/financial state: balances, positions, instruments, GL, nostro, margin, market data, fleet/ops, pricing config.

`instruments` `balances` `audit_hash_chain` `risk_limits` `fee_tiers` `margin_accounts` `positions` `liquidation_auctions`
`insurance_fund` `nostro_accounts` `audit_merkle_roots` `processed_trades` `book_snapshots` `surveillance_signals`
`fix_sessions` `fix_messages` `chart_of_accounts` `journal_entries` `ledger_lines` `prime_brokers` `pb_credit_limits`
`pb_giveup_trades` `appropriateness_assessments` `mm_programs` `mm_compliance` `mm_rebate_accruals` `fix_certifications`
`recovery_reports` `grid_bots` `grid_bot_orders` `prevented_matches` `client_role_bindings` `client_approval_policies`
`order_list_legs` `strategies` `withdrawal_whitelist_settings` `vip_tier_schedule` `account_equity_snapshots`
`account_vip_history` `instruments_reference` `auction_calendar` `currency_day_counts` `swap_markup_policies`
`non_trading_fee_schedule` `swap_accrual_records` `principal_role_systems` `admin_recert_campaigns` `environments`
`fleet_hosts` `release_promotions` `deploy_windows` `recovery_digests` `account_product_profiles` `copy_follows`
`copy_child_orders` `high_water_marks` `profit_share_accruals` `product_target_markets` `governance_packs`
`ledger_entries` `journal_sums` `swap_free_admin_fees` `rail_payments` `risk_daily_usage` `currency_conversions`
`position_fills` `nostro_movements` `dust_sweeps` `swap_rates` `carry_trade_allocations` `carry_trade_legs`
`carry_yield_records` `carry_yield_totals` `swap_free_admin_fee_assessments` `rollover_runs` `commission_tiers`
`account_monthly_volume` `partition_archive_log` `currency_holidays` `client_order_id_dedup` `transfers`
`announcements` `maintenance_windows` `fx_klines` `fee_promo_windows` `api_deprecations` `manual_liquidations`
`lp_instrument_configs` `lp_performance_alerts` `feature_flags` `data_retention_holds` `retention_audit_log`
`partition_tier_state` `partition_tier_log` `api_deprecation_hits` `ops_status_events` `ops_component_state`
`ops_incidents` `funding_fee_tiers` `funding_fee_free_usage` `funding_currency_conversions` `withdrawal_dispatch_queue`
`funding_ops_alerts` `nostro_replenishment_requests` `kyc_tier_policies` `kyc_ops_matrix` `circuit_breaker_events`
`reconciliation_runs` `reconciliation_findings` `solvency_snapshots` `solvency_proofs` `cooling_off_periods`
`pamm_allocations` `pamm_subledger_entries` `pamm_fill_allocations` `auto_halt_events` `algo_orders`
`algo_order_children` `bracket_orders` `bracket_children` `fix_allocations` `fix_allocation_legs` `allocation_events`
`sor_shadow_orders` `sor_fill_dedup` `fixsbe_sessions` `fixsbe_schema_registry`

## 4. Data-flow summary

| Stage | Path |
|---|---|
| Collection | `POST /api/v1/auth/register` (email/password/country) → `auth.UserStore` → `users`; `PUT /account/profile` (full_name/address/phone); `POST /kyc/submit` → S3 SSE-KMS objects + `kyc_documents`/`kyc_submissions`; `POST /kyc/self-certification` → `tax_self_certifications`; `POST /funding/bank-accounts` → `bank_accounts`; login events → `login_history`; admin/deposit ingestion → `deposit_confirmations`/`suspense_account_mappings` |
| Storage | PostgreSQL 16 OLTP (columns above); KYC document bytes in S3 SSE-KMS (`upload.go` sets `aws:kms` + KMS key id, `kyc_documents.sse_algorithm` records it); sessions in Redis (`session:{sid}` hash: IP+UA, TTL-bound); secrets sealed via `auth.SecretBox` AES-256-GCM (`secrets.data_key`, prod source Vault/KMS) |
| Processing | matching/risk never touches PII columns — they join on `account_id`/`user_id` linkage only; notification dispatch resolves `users.email`/`phone` at send time (never persisted to `notification_deliveries`); admin views read PII-bearing rows through audited (`support_view`) and unaudited (see findings) paths |
| Deletion | per-table procedure in `gdpr-erasure-runbook.md`; legal holds via `data_retention_holds` (enforced by `archiver` + retention enforcer); KYC lifecycle-bound = account close + 5y |
