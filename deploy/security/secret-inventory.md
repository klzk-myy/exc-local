# Secret Inventory — Phase-13.5 Task 13.5.3.5 (spec §24 #111)

Metadata only — **no secret values in this file** (rule zero of
[../../docs/ops/secrets-inventory.md](../../docs/ops/secrets-inventory.md);
values exist exclusively in Vault/KMS). This table is the deploy-side
inventory; the DB-backed registry (`secrets_inventory`, migration 089 —
pending) and the Vault audit log are the stores of record it mirrors.

Rotation contracts are enforced in code by
`services/internal/security/rotation.go` (`DefaultRegistry`) and paged by
`SecretRotationDueSoon`/`SecretRotationOverdue` in
`deploy/prometheus/alerts.yml` (P2 within 14 days of the deadline).

Owner column follows the RBAC role set (spec §8.2) + platform teams.

| # | Secret (canonical name) | Class (`rotation.go`) | Owner | Storage of record | Consumers | Max age | Overlap | Rotation mechanism |
|---|---|---|---|---|---|---|---|---|
| 1 | `jwt-signing` (`jwt_active_kid` + `exchange/<env>/jwt/<kid>`) | `jwt_signing_key` | Platform | Vault KV v2 `secret/exchange/<env>/jwt/*`; K8s `exchange-secrets.jwt-hs256-key-b64` via ExternalSecret (dev bootstrap: `EXC_JWT_HS256_KEY_B64`) | gateway, admin, marketdata WS auth | **90d** | **24h** dual-key (`kid` header; `JWTKeyring` atomic Issuer swap) | `rotate-secrets.sh rotate jwt` → new kid written, active-kid marker patched; ring prunes retired kid at `NotAfter` |
| 2 | `data-key` (AES-256-GCM, `api_keys.secret_enc`, webhook secrets) | `api_key_material` | Platform | Vault `exchange/<env>/secrets.data-key-b64`; K8s `exchange-secrets.data-key`; dev: `EXC_SECRETS_DATA_KEY` (prod boot fails closed without it — `config.Validate`) | gateway, admin (auth.SecretBox) | 90d | 72h re-wrap window | `rotate-secrets.sh rotate datakey` — new KV version; old key retained until `secret_enc` re-wrap completes |
| 3 | `postgres-dsn` / dynamic roles | `db_credential` | DBA | Vault `database/creds/<role>` (dynamic, **1h TTL**) + static `exchange/<env>/postgres` (PgBouncer user) | all Go services via `internal/db`; engine never speaks PG | 90d static / 1h lease | none needed (pool reload) | PgBouncer `PAUSE`→`ALTER ROLE`→Vault write→`RESUME` (`rotate-secrets.sh rotate db`); Go services auto-renew via `Swapper.LeaseRenewal` |
| 4 | `redis-password` | `redis_password` | Platform | Vault `exchange/<env>/redis.password`; K8s `exchange-secrets.redis-password` | Go services, C++ `-redis` control path (RespClient — see secrets-policy §contract gap) | 90d | reconnect window | `ACL SETUSER` + Vault patch (`rotate-secrets.sh rotate redis`); clients reconnect via swapper |
| 5 | `aeron-auth-token` | `aeron_token` | Platform | Vault `exchange/<env>/aeron.auth_token` → vault-agent → tmpfs `/run/exchange-secrets/` | matching-engine, aeron-nats-bridge | 90d | 1h (file overlap) | Vault KV patch → agent template re-render → engine file-watch reload (`secrets-policy.md` contract) |
| 6 | `tls-ingress` | `tls_certificate` | SRE | cert-manager `Certificate`/ACME (K8s services) + Vault PKI `${ENV}-pki` (internal) | ingress/HAProxy edge, service listeners | cert lifetime ≤1y | chain validity | cert-manager renews **30d pre-expiry** automatically; manual: `rotate-secrets.sh rotate tls` (Vault PKI issue → KV) |
| 7 | `fix-mtls-client` (per-session CompID certs) | `tls_certificate` | Platform | Vault PKI `fix-pki`; client cert pack issued once | fix-gateway (Phase-18 Task 18.3.11 consumes) | ≤12mo | mTLS session re-establish | `deploy/security/provision-fix-mtls.sh` — Vault PKI `issue` + cert pack export |
| 8 | `banking/<rail>` (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2) | `banking_api_key` | Finance Ops | Vault `exchange/<env>/banking/<rail>` | banking-rails-worker (Phase-11) | ≤90d per rail | per-rail grace | re-issue at rail portal → `vault kv patch` → worker credential swap; `rotate-secrets.sh rotate banking` |
| 9 | `pagerduty-keys` (`PAGERDUTY_P0..P3_SERVICE_KEY`) | `api_key_material` | SRE | Vault → envsubst at alertmanager start (`alertmanager.yml` — never committed) | alertmanager | ≤12mo | dual-route window | rotate at PD console → Vault write → alertmanager reload |
| 10 | `slack-ops-webhook` | `api_key_material` | SRE | same envsubst path | alertmanager | ≤12mo | none | same as pagerduty-keys |
| 11 | `s3-credentials` (`access-key-id`/`secret-access-key`, `CH_PASSWORD`) | `api_key_material` | DBA | Vault `exchange/<env>/s3` → K8s `exchange-s3`; `clickhouse-backup/config.yml` envsubst refs | partition archiver, market-data exporter, clickhouse-backup | 90d | dual-key at S3 | rotate at object store → Vault patch → pod secret refresh (1h) |
| 12 | `nats-credentials` (`/etc/exchange/nats.env`) | `api_key_material` | Platform | Vault → env file (0600 exchange:exchange, provisioned by Ansible) | nats-server, bridge, consumers | 90d | reconnect | Vault patch → env file rewrite → `systemctl reload-or-restart nats-server` (graceful) |
| 13 | `webhook-signing` (outbound webhook secrets, mig. 180) | `api_key_material` | Platform | Vault + sealed at rest under `data-key` | notifications/webhooks service | ≤12mo | per-endpoint | same SecretBox re-wrap path as `data-key` |
| 14 | `lp-credentials` (liquidity-provider sessions, mig. 191) | `banking_api_key` | Platform | Vault `exchange/<env>/lp/<name>` | fix-gateway / LP connectors | ≤90d | session re-logon | same as banking rails |

## Rotation schedule

- **Ceiling:** 90 days for every credential class; `Registry.SetPolicy`
  rejects weaker values (`SECRET_CONFIG_INVALID`). TLS follows cert
  lifetime (≤1y) with a 30-day renewal lead; PagerDuty/Slack/webhook/
  OAuth-class material ≤12mo.
- **Overlap:** dual-key acceptance per class as listed; JWT pinned at
  24h, API-key predecessors ≤72h (`auth.MaxRotationOverlap`).
- **Alerting:** `secret_rotation_seconds_until_expiry` gauge (pull-based,
  `Scheduler.RegisterMetrics`) → `SecretRotationDueSoon` fires when any
  secret is <14d from its deadline (P2 → ticket+slack), escalates via
  `SecretRotationOverdue` past the deadline. Runbook:
  [../../docs/runbooks/secret-rotation-overdue.md](../../docs/runbooks/secret-rotation-overdue.md).
- **Zero-downtime:** JWT via kid ring; DB/Redis via `Swapper` pool reload
  (no restart); TLS via cert-manager overlap; Aeron via vault-agent tmpfs
  file + inotify-style reload contract.

## Prohibited storage (enforced by `scripts/ci/no-plaintext-secrets.sh`)

Secrets must never appear in: env files committed to the repo, container
images, ConfigMaps, systemd unit files (units reference
`EnvironmentFile=-/etc/exchange/*.env` — the env file itself is
provisioned by Ansible from Vault, mode 0600, never committed), or docs.
The dev adapters (`DevSource`, `EXC_DEV_SECRET_*`, `EXC_JWT_HS256_KEY_B64`,
`exchange_dev` DSN password) are explicitly labeled and refused when
`EXC_SECRETS_REQUIRED=production` or a production env label is in force.
