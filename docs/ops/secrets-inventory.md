# Secrets Inventory Procedure (no values)

**Phase-09 Task 9.3.29 (items 4–5)** · **Authority:** spec §19.14, §24 #340 · **Store of record:** Vault/KMS (Phase-13.5 Task 13.5.3.5) · **Registry table:** `secrets_inventory` — **migration 089 pending** (this document is the procedure the table + `services/internal/security/secrets_inventory.go` will enforce) · **Alert:** `SECRET_ROTATION_OVERDUE` (HTTP 503, §23) at 14-day overdue (P2 → [../runbooks/secret-rotation-overdue.md](../runbooks/secret-rotation-overdue.md)).

Rule zero: this inventory stores **metadata only** — never secret values. Values exist exclusively in Vault/KMS and env injection at deploy time (same convention as `deploy/prometheus/alertmanager.yml` `PAGERDUTY_*_SERVICE_KEY`/`SLACK_OPS_WEBHOOK_URL` envsubst).

## 1. Inventory record (one row per secret)

| Field | Content |
|---|---|
| `name` | canonical secret name (e.g. `fix/mtls/client-certs/session-*`) |
| `category` | banking API key / FIX mTLS cert / OAuth secret / KMS grant / webhook / PagerDuty key / Slack webhook / DB credential / S3 credential |
| `owner` | team + named individual |
| `consumers` | services/hosts that resolve it (`gateway`, `fix`, `bridge`, `settlement`, banking-rails, `deploy/clickhouse/backup.sh` env…) |
| `ttl` | rotation period |
| `rotation_procedure` | runbook link or steps (issue → store → distribute → reload → verify → mark) |
| `last_rotated_at` | timestamp of last completed rotation |
| `dr_critical` | yes/no — drives §4 secondary-copy gate |
| `break_glass` | emergency path reference |

## 2. Current inventory classes (populate table on migration 089 landing)

| Class | Examples | Owner | TTL | DR-critical |
|---|---|---|---|---|
| Banking/rail credentials | SWIFT/SEPA/FedNow API keys, nostro SFTP keys (Phase-11) | Finance Ops | per rail (≤90d) | yes |
| FIX mTLS | client certs, venue CA chain (Phase-18 Task 18.3.11) | Platform | ≤12mo | yes |
| OAuth/OIDC | client secrets, API-key asymmetric keys (`api_key_asymmetric_types`, mig. 073) | Platform | ≤12mo | yes |
| KMS/Vault grants | envelope keys, unseal shares | Security | per KMS policy | yes |
| Alerting | `PAGERDUTY_P0/P1/P2_SERVICE_KEY`, `SLACK_OPS_WEBHOOK_URL` | SRE | ≤12mo | no |
| Data-path creds | `CH_PASSWORD` + `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` (`deploy/clickhouse/clickhouse-backup/config.yml`), PG DSNs | DBA | ≤90d | yes |
| Webhooks/partner | outbound webhook signing secrets (mig. 180), LP credentials (mig. 191) | Platform | ≤12mo | partial |

## 3. Rotation procedures

**Scheduled rotation (per row's `rotation_procedure`):**

1. Issue new credential at the source (bank portal / CA / KMS / provider console).
2. Write to Vault/KMS (new version — never overwrite-in-place without version).
3. Distribute to consumers: K8s secret refresh + rolling restart, or config reload; FIX mTLS swaps during the §19.6 deploy window for engine-adjacent consumers.
4. Verify consumers: FIX session logon, rail auth call, OAuth mint, `clickhouse-backup` S3 access — a failed verify rolls back to the prior version.
5. Update `last_rotated_at`; alert evaluator clears `SECRET_ROTATION_OVERDUE` on next pass.

**Emergency rotation (leak-triggered):**

- Hours, not days: rotate → revoke at issuer → invalidate derivative sessions/tokens → security incident record.
- Break-glass path delegates to Task 7.3.12 break-glass (pending Phase-07 backend) with mandatory post-review — every break-glass use is audited in `admin_audit_log` (migration 010).

**Rotation failure:** beyond the 14-day P2 bound the enforcer raises `SECRET_ROTATION_OVERDUE`; chronically failing procedures are defects on the *procedure* (file against row owner), never silently extend TTL.

## 4. DR-region secret availability (Task 9.3.29 item 5)

Every `dr_critical` secret carries a tested **secondary-region copy** (KMS grant replicated, certs mirrored, keys escrowed to the DR Vault cluster):

- The quarterly DR drill verifies **decrypt-in-secondary before promotion** (drill gate D7, [../runbooks/dr-drill.md](../runbooks/dr-drill.md)).
- A missing/unusable DR copy **blocks the drill's pass verdict** — file as remediation item, not a waiver.
- Secrets are environment-scoped (Task 9.3.30): no copying prod secrets to dev/staging, ever; promotion gates resolve per-env from Vault/KMS (`FORBIDDEN` on cross-env resolution).

## 5. Access & audit

- Inventory reads/writes: Security + SRE leads; row changes logged in `admin_audit_log`.
- Vault/KMS audit log is the corroborating source for `last_rotated_at` — drift between table and store fails the monthly secrets review.
- No secret material in tickets, docs (including this one), Slack, or incident channels — reference by `name` only.
