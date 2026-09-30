# Attack Surface Map — Penetration Test Preparation

**Generated:** 2026-09-30 16:49 UTC by `scripts/security/gen-attack-surface.py` (Task 13.3.5). Regenerate before each test window — the table is derived from `gateway.SeedRoutes()` (`services/internal/gateway/routes_v1.go`), the same table the gateway mounts and `GET /api/v1/routes` serves. Do not hand-edit; update the registry or this generator.

Scope/ROE live in [pentest-scope.md](./pentest-scope.md); fixtures in `tests/pentest/`.

## 1. Summary

- **641 registered routes**: 618 live, 23 stub (501-until-implemented — still routable surface: they consume auth/rate-limit middleware and are part of the attack surface)
- **7 WebSocket endpoints**, 19 public channel types, 6 private channels, 15 control/order actions
- **42 dual-control routes** (four-eyes operations), **9 env-gated routes** (nonprod/env-scoped only)
- FIX surface: scaffold only — see §4

## 2. HTTP/REST routes (full registry)

Columns: auth = `public` (no credentials) or required credential/scope/role; tier = §8.8 rate-limit bucket; status = live|stub; dual = dual-control required; env = env-gated (`nonprod` = never mounted in prod).

| Method | Path | Auth | Rate tier | Status | Dual | Env |
|---|---|---|---|---|---|---|
| `GET` | `/api/v1/account/api-keys` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/api-keys` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `DELETE` | `/api/v1/account/api-keys/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/appropriateness` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/appropriateness` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/approval-policies` | auth (scope:read) | basic | live |  |  |
| `PUT` | `/api/v1/account/approval-policies` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `DELETE` | `/api/v1/account/approval-policies/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/approval-requests` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/approval-requests/{id}/decide` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/balances` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/change-password` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/account/close` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/commission/{symbol}` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/confirmations/{trade_id}` | auth (scope:read) | basic | live |  |  |
| `PUT` | `/api/v1/account/consent` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/account/cooling-off` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/cost-preview` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/delegated-users` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/delegated-users` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/account/delegated-users/revoke-all` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `DELETE` | `/api/v1/account/delegated-users/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `PUT` | `/api/v1/account/delegated-users/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/account/emergency-freeze` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/filters/{symbol}` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/gdpr` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/gdpr/consent` | auth (scope:read) | basic | live |  |  |
| `PUT` | `/api/v1/account/gdpr/consent` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/account/gdpr/erase` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/account/gdpr/export` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/income` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/leverage` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/liquidations` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/login-history` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/margin-mode` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/notifications/preferences` | auth (scope:read) | basic | live |  |  |
| `PUT` | `/api/v1/account/notifications/preferences` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/pnl` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/positions` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/profile` | auth (scope:read) | basic | live |  |  |
| `PUT` | `/api/v1/account/profile` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/rate-limits` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/risk-limits` | auth (scope:read) | basic | live |  |  |
| `DELETE` | `/api/v1/account/sessions` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/sessions` | auth (scope:read) | basic | live |  |  |
| `DELETE` | `/api/v1/account/sessions/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `PUT` | `/api/v1/account/settings/anti-phishing-code` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/snapshots` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/solvency-proof` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/statements` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/statements/{id}/download` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/account/sub-accounts` | auth (scope:read) | basic | stub |  |  |
| `POST` | `/api/v1/account/sub-accounts` | auth (cred:jwt|hmac|oauth2) | basic | stub |  |  |
| `POST` | `/api/v1/account/sub-accounts/{id}/api-keys` | auth (cred:jwt|hmac|oauth2) | basic | stub |  |  |
| `GET` | `/api/v1/account/swap-free` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/swap-free/request` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/account/tax-report` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/account/unfreeze-request` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/account/webauthn/authenticate` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/account/webauthn/register` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/admin/accounts/{id}/close` | auth (role:Compliance Officer) | basic | live | yes |  |
| `POST` | `/api/v1/admin/accounts/{id}/freeze` | auth (role:Compliance Officer) | basic | stub | yes |  |
| `POST` | `/api/v1/admin/accounts/{id}/jurisdiction` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/accounts/{id}/product-profile` | auth (role:Compliance Officer) | basic | live |  |  |
| `PUT` | `/api/v1/admin/accounts/{id}/product-profile` | auth (role:Compliance Officer) | basic | live |  |  |
| `PUT` | `/api/v1/admin/accounts/{id}/sub-account-limit` | auth (role:Risk Manager) | basic | stub |  |  |
| `POST` | `/api/v1/admin/accounts/{id}/unfreeze` | auth (role:Compliance Officer) | basic | stub | yes |  |
| `GET` | `/api/v1/admin/algo-certifications` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/algo-certifications` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/algo-certifications/{id}/transition` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/escalate` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/groups` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/allocations/groups/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/groups/{id}/allocate` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/groups/{id}/eligibility` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/groups/{id}/fills` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/groups/{id}/submit` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/{id}/cancel` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/{id}/claim` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/{id}/correct` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/allocations/{id}/reject` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/aml/artifacts` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/aml/artifacts` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/aml/monitoring` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/aml/program` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/announcements` | auth (role:Support Agent) | basic | live |  |  |
| `POST` | `/api/v1/admin/announcements` | auth (role:Support Agent) | basic | live |  |  |
| `DELETE` | `/api/v1/admin/announcements/{id}` | auth (role:Support Agent) | basic | live |  |  |
| `PATCH` | `/api/v1/admin/announcements/{id}` | auth (role:Support Agent) | basic | live |  |  |
| `GET` | `/api/v1/admin/api-deprecations` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/api-deprecations` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/api-deprecations/usage` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `PUT` | `/api/v1/admin/api-keys/{id}/extend-expiry` | auth (role:Super Admin) | basic | live | yes |  |
| `GET` | `/api/v1/admin/archive/status` | auth (role:Read-Only Auditor) | basic | stub |  |  |
| `GET` | `/api/v1/admin/audit` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/audit-log` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/audit/chain` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/audit/trail` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/audit/verify` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/basel-report` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/bestexec/rts27` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/bestexec/rts27/generate` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/bestexec/rts27/materialize` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/bestexec/rts27/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/bestexec/rts27/{id}/publish` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/bestexec/rts28` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/bestexec/rts28/generate` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/bestexec/rts28/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/bestexec/rts28/{id}/publish` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/bindings` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/bindings` | auth (role:Super Admin) | basic | live | yes |  |
| `POST` | `/api/v1/admin/bindings/{id}/revoke` | auth (role:Super Admin) | basic | live | yes |  |
| `POST` | `/api/v1/admin/break-glass` | auth (role:Super Admin) | basic | live | yes |  |
| `POST` | `/api/v1/admin/break-glass/{id}/review` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/cache/warm` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/chargebacks` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/chargebacks` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/chargebacks/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/chargebacks/{id}/resolve` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/chargebacks/{id}/submit` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/circuit-breaker/{symbol}` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/circuit-breaker/{symbol}/reset` | auth (role:Risk Manager) | basic | live | yes |  |
| `GET` | `/api/v1/admin/client-money/audits` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/client-money/audits` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/client-money/audits/{id}/evidence-pack` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/client-money/certifications` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/client-money/certifications` | auth (role:Finance Ops) | basic | live | yes |  |
| `PUT` | `/api/v1/admin/collateral-schedule` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/comms-recordings` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/comms-recordings/verify-day` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/comms-recordings/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/comms-recordings/{id}/retrieve` | auth (role:Compliance Officer) | basic | live | yes |  |
| `GET` | `/api/v1/admin/compliance-report` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/compliance/holds` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/compliance/holds` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/compliance/holds/{id}/escalate` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/compliance/holds/{id}/release` | auth (role:Compliance Officer) | basic | live | yes |  |
| `POST` | `/api/v1/admin/compliance/screening/accounts/{id}` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/compliance/screening/adverse-media` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/copy/strategies/{id}/suspend` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/ctr` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/data-residency/access-log` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/data-residency/policies` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/dea/controls` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/dea/controls` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/dea/controls/{session_id}/suspend` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/dlq` | auth (role:Support Agent) | basic | live |  |  |
| `GET` | `/api/v1/admin/dual-control` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/dual-control/{id}/approve` | auth (role:*) | basic | live |  |  |
| `POST` | `/api/v1/admin/dual-control/{id}/reject` | auth (role:*) | basic | live |  |  |
| `GET` | `/api/v1/admin/emir-report` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/employee-dealing/audit` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/enforcement` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/enforcement/{signal_id}` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/entity-leverage-policy` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/entity-leverage-policy` | auth (role:Risk Manager) | basic | live | yes |  |
| `GET` | `/api/v1/admin/execution-policies` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/execution-policies` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/execution-policies/{id}/activate` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/execution-policies/{id}/review` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/fees/promo` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/fees/promo/{id}/approve` | auth (role:Finance Ops) | basic | live | yes |  |
| `POST` | `/api/v1/admin/fees/promo/{id}/reject` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/fees/promos` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/finance/balance-sheet` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/finance/pnl` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/finance/trial-balance` | auth (role:Finance Ops) | basic | live |  |  |
| `PUT` | `/api/v1/admin/fix-sessions/{id}` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/flags` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/flags` | auth (role:Super Admin) | basic | live |  |  |
| `DELETE` | `/api/v1/admin/flags/{name}` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/flags/{name}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/flags/{name}` | auth (role:Super Admin) | basic | live |  |  |
| `PUT` | `/api/v1/admin/flags/{name}` | auth (role:Super Admin) | basic | live |  |  |
| `POST` | `/api/v1/admin/flags/{name}/advance` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/fleet/environments` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/fleet/hosts` | auth (role:Super Admin) | basic | live |  |  |
| `POST` | `/api/v1/admin/fleet/hosts/{id}/cordon` | auth (role:Super Admin) | basic | live | yes | env-scoped |
| `POST` | `/api/v1/admin/fleet/hosts/{id}/decommission` | auth (role:Super Admin) | basic | live | yes | env-scoped |
| `POST` | `/api/v1/admin/fleet/hosts/{id}/drain` | auth (role:Super Admin) | basic | live | yes | env-scoped |
| `GET` | `/api/v1/admin/fleet/topology` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/funding/bank-accounts` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/bank-accounts/{id}/reject` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/bank-accounts/{id}/verify` | auth (role:Finance Ops) | basic | live | yes |  |
| `POST` | `/api/v1/admin/funding/deposits` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/deposits/{id}/confirm` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/deposits/{id}/review` | auth (role:Finance Ops) | basic | live | yes |  |
| `GET` | `/api/v1/admin/funding/fees` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/fees` | auth (role:Finance Ops) | basic | live |  |  |
| `DELETE` | `/api/v1/admin/funding/fees/{id}` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/funding/fees/{id}` | auth (role:Finance Ops) | basic | live |  |  |
| `PUT` | `/api/v1/admin/funding/fees/{id}` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/funding/fees/{id}/versions` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/inbound-wires` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/funding/nostro` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/funding/nostro/replenishments` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/nostro/replenishments` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/nostro/replenishments/{id}/decide` | auth (role:Finance Ops) | basic | live | yes |  |
| `GET` | `/api/v1/admin/funding/ops-alerts` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/funding/quarantine` | auth (role:*) | basic | live |  |  |
| `POST` | `/api/v1/admin/funding/quarantine/{id}/resolve` | auth (role:Finance Ops) | basic | live | yes |  |
| `POST` | `/api/v1/admin/funding/returns` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/fx-global-code/assessments` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/fx-global-code/assessments` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/fx-global-code/assessments/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/fx-global-code/assessments/{id}/complete` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/fx-global-code/assessments/{id}/publish` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/fx-global-code/assessments/{id}/sign` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/fx-global-code/assessments/{id}/verdicts` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/governance-packs` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/governance-packs/generate` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/governance-packs/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/governance-packs/{id}/release` | auth (role:Super Admin) | basic | live | yes |  |
| `GET` | `/api/v1/admin/instruments` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/instruments` | auth (role:Super Admin) | basic | live | yes |  |
| `PUT` | `/api/v1/admin/instruments/{id}` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/instruments/{id}/activate` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/instruments/{id}/cancel-only` | auth (role:*) | basic | live |  |  |
| `POST` | `/api/v1/admin/instruments/{id}/delist` | auth (role:Super Admin) | basic | live | yes |  |
| `POST` | `/api/v1/admin/instruments/{id}/halt` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/instruments/{id}/restrict` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/instruments/{id}/resume` | auth (role:Risk Manager) | basic | live | yes |  |
| `POST` | `/api/v1/admin/instruments/{id}/suspend` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/instruments/{id}/uncross-override` | auth (role:Risk Manager) | basic | stub |  |  |
| `GET` | `/api/v1/admin/instruments/{symbol}/auction-calendar` | auth (role:Risk Manager) | basic | live |  |  |
| `PUT` | `/api/v1/admin/instruments/{symbol}/auction-calendar` | auth (role:Risk Manager) | basic | live | yes |  |
| `GET` | `/api/v1/admin/insurance-fund` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/invoices` | auth (role:Finance Ops) | basic | live |  |  |
| `DELETE` | `/api/v1/admin/ip-allowlist/{ip}` | auth (role:Risk Manager) | basic | live |  |  |
| `PUT` | `/api/v1/admin/ip-allowlist/{ip}` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/ip-bans` | auth (role:Support Agent) | basic | live |  |  |
| `GET` | `/api/v1/admin/ip-bans/audit` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `DELETE` | `/api/v1/admin/ip-bans/{ip}` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/ip-bans/{ip}` | auth (role:Support Agent) | basic | live |  |  |
| `PUT` | `/api/v1/admin/ip-bans/{ip}` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/kill-switch` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/kill-switch` | auth (role:Risk Manager) | basic | live | yes |  |
| `POST` | `/api/v1/admin/kill-switch/reset` | auth (role:Risk Manager) | basic | live | yes |  |
| `POST` | `/api/v1/admin/kyc/{id}/approve` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/kyc/{id}/reject` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/liquidation/manual` | auth (role:Risk Manager) | basic | live | yes |  |
| `GET` | `/api/v1/admin/liquidity-providers` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/liquidity-providers` | auth (role:Risk Manager) | basic | live |  |  |
| `PUT` | `/api/v1/admin/liquidity-providers` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/liquidity-providers/{id}` | auth (role:Risk Manager) | basic | live |  |  |
| `PUT` | `/api/v1/admin/liquidity-providers/{id}` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/liquidity-providers/{id}/alerts` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/liquidity-providers/{id}/scorecard` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/listing-proposals` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/listing-proposals` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/listing-proposals/{id}/review` | auth (role:Risk Manager) | basic | live | yes |  |
| `GET` | `/api/v1/admin/maintenance-windows` | auth (role:Support Agent) | basic | live |  |  |
| `POST` | `/api/v1/admin/maintenance-windows` | auth (role:Support Agent) | basic | live |  |  |
| `DELETE` | `/api/v1/admin/maintenance-windows/{id}` | auth (role:Support Agent) | basic | live |  |  |
| `PATCH` | `/api/v1/admin/maintenance-windows/{id}` | auth (role:Support Agent) | basic | live |  |  |
| `POST` | `/api/v1/admin/margin-param-changes` | auth (role:Risk Manager) | basic | live | yes |  |
| `GET` | `/api/v1/admin/market-schedule` | auth (role:*) | basic | live |  |  |
| `GET` | `/api/v1/admin/market-schedule/overrides` | auth (role:*) | basic | live |  |  |
| `POST` | `/api/v1/admin/market-schedule/overrides` | auth (role:Risk Manager) | basic | live |  |  |
| `DELETE` | `/api/v1/admin/market-schedule/overrides/{id}` | auth (role:Risk Manager) | basic | live |  |  |
| `PUT` | `/api/v1/admin/market-schedule/overrides/{id}` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/mifid-report` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/mm-programs` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/mm-programs` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/mm-programs/rebates/post` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/mm-programs/{id}` | auth (role:Risk Manager) | basic | live |  |  |
| `PUT` | `/api/v1/admin/mm-programs/{id}` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/mm-programs/{id}/compliance` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/mm-programs/{id}/mmp-reset` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/mm-programs/{id}/rebates` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/mm-programs/{id}/resume` | auth (role:Risk Manager) | basic | live |  |  |
| `POST` | `/api/v1/admin/mm-programs/{id}/suspend` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/nostro-accounts` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/nostro-accounts` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/nostro-reconciliation` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/nostro-reconciliation/breaks/{id}/resolve` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/nostro-reconciliation/run` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/ops-board` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/ops/health` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/order-records/{order_id}/export` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/orders/mass-cancel` | auth (role:Risk Manager) | basic | live |  |  |
| `GET` | `/api/v1/admin/orders/{id}/audit` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/pb-reconciliation` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/pre-clearance` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/pre-clearance` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/product-profiles` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/product-profiles` | auth (role:Compliance Officer) | basic | live | yes |  |
| `PUT` | `/api/v1/admin/product-profiles` | auth (role:Compliance Officer) | basic | live | yes |  |
| `PUT` | `/api/v1/admin/product-profiles/{id}/target-market` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/product-target-markets` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/product-target-markets/{id}/review` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/promotions` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/promotions` | auth (role:Compliance Officer) | basic | live |  |  |
| `PUT` | `/api/v1/admin/promotions` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/promotions/report` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/promotions/{id}/approve` | auth (role:Compliance Officer) | basic | live | yes |  |
| `POST` | `/api/v1/admin/promotions/{id}/reject` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/promotions/{id}/submit` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/promotions/{id}/withdraw` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/recert` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/recert/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/recert/{id}/decisions` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/reconciliation/latest` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/reconciliation/runs` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/regreporting/acks` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/regreporting/breaks/{id}/resolve` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/regreporting/events` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/regreporting/events/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/regreporting/party-identifiers` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/regreporting/queue` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/regreporting/reconcile` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/regreporting/submissions` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/regreporting/submissions/{id}/resubmit` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/regulatory-changes` | auth (role:*) | basic | live |  |  |
| `POST` | `/api/v1/admin/regulatory-changes` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/regulatory-changes/impacts/{id}/done` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/regulatory-changes/{id}/correspondence` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/regulatory-changes/{id}/impact` | auth (role:*) | basic | live |  |  |
| `PUT` | `/api/v1/admin/regulatory-changes/{id}/impact` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/regulatory-changes/{id}/transition` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/releases` | auth (role:Super Admin) | basic | live |  |  |
| `POST` | `/api/v1/admin/releases` | auth (role:Super Admin) | basic | live |  |  |
| `POST` | `/api/v1/admin/releases/{id}/promote` | auth (role:Super Admin) | basic | live | yes | env-scoped |
| `GET` | `/api/v1/admin/reporting-values` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/reporting-values` | auth (role:Compliance Officer) | basic | live |  |  |
| `DELETE` | `/api/v1/admin/restricted-lists` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/restricted-lists` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/restricted-lists` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/roles` | auth (role:Super Admin) | basic | live |  |  |
| `GET` | `/api/v1/admin/rts6/self-assessments` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/rts6/self-assessments` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/sanctions/queue/replay` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/sanctions/refresh` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/sanctions/status` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/sar` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/sar` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/sar/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/sar/{id}/approve` | auth (role:Compliance Officer) | basic | live | yes |  |
| `POST` | `/api/v1/admin/sar/{id}/file` | auth (role:Compliance Officer) | basic | live | yes |  |
| `POST` | `/api/v1/admin/sar/{id}/reject` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/sar/{id}/review` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/security/disclosures` | auth (role:*) | basic | live |  |  |
| `POST` | `/api/v1/admin/security/disclosures/intake` | auth (role:*) | basic | live |  |  |
| `GET` | `/api/v1/admin/security/disclosures/{id}` | auth (role:*) | basic | live |  |  |
| `PUT` | `/api/v1/admin/security/disclosures/{id}` | auth (role:*) | basic | live |  |  |
| `POST` | `/api/v1/admin/security/disclosures/{id}/triage` | auth (role:*) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement-confirmations` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/settlement-exceptions/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement-exceptions/{id}/resolve` | auth (role:Finance Ops) | basic | live | yes |  |
| `POST` | `/api/v1/admin/settlement/cls/instructions` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/cls/instructions/{ref}/amend` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/cls/instructions/{ref}/dispatch` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/cls/instructions/{ref}/finality` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/cls/instructions/{ref}/pay-in` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/cls/instructions/{ref}/rescind` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/cls/instructions/{ref}/status` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/instructions/{id}/roll` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/settlement/netting/batches` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/netting/batches/{id}/bust` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/netting/batches/{id}/dispatch` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/settlement/netting/batches/{id}/lines` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/netting/batches/{id}/settle` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/netting/run` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/settlement/rail-schedules` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/rail-schedules/evaluate` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/settlement/ssi` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/ssi` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/ssi/{id}/revoke` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/settlement/statements` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/statements` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/settlement/statements/{id}/entries` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/suspense/route` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/settlement/suspense/{id}/resolve` | auth (role:Finance Ops) | basic | live | yes |  |
| `GET` | `/api/v1/admin/strategy-templates` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/strategy-templates/{id}/approve` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/strategy-templates/{id}/reject` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/support/accounts/{id}` | auth (role:Support Agent) | basic | live |  |  |
| `GET` | `/api/v1/admin/support/complaints/register` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/support/tickets` | auth (role:Support Agent) | basic | live |  |  |
| `PUT` | `/api/v1/admin/support/tickets` | auth (role:Support Agent) | basic | live |  |  |
| `GET` | `/api/v1/admin/support/tickets/{id}` | auth (role:Support Agent) | basic | live |  |  |
| `POST` | `/api/v1/admin/support/tickets/{id}/notes` | auth (role:Support Agent) | basic | live |  |  |
| `GET` | `/api/v1/admin/surveillance/cases` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/surveillance/cases/{id}` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/surveillance/cases/{id}/assign` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/surveillance/cases/{id}/disposition` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/surveillance/cases/{id}/evidence` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/surveillance/summary` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/surveillance/tuning` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/surveillance/tuning` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/surveillance/tuning/{signal}/activate` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/surveillance/tuning/{signal}/backtest` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/swap-free/{id}/approve` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/swap-free/{id}/reject` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/swap-free/{id}/revoke` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/swift-messages` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/tax-reporting/runs` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/tax-reporting/runs` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/tax-reporting/runs/{id}` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/tax-reporting/runs/{id}/approve` | auth (role:Compliance Officer) | basic | live | yes |  |
| `POST` | `/api/v1/admin/tax-reporting/runs/{id}/reject` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/tax-reporting/runs/{id}/review` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/tax-reporting/runs/{id}/submit` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/tax-reporting/runs/{id}/xml` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/trades/{id}/bust` | auth (role:Risk Manager) | basic | live | yes |  |
| `GET` | `/api/v1/admin/travel-rule` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `GET` | `/api/v1/admin/travel-rule/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/travel-rule/{id}/supply` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/treasury/contingent-capital` | auth (role:Finance Ops) | basic | live |  |  |
| `POST` | `/api/v1/admin/treasury/contingent-capital` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/treasury/own-funds` | auth (role:Finance Ops) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/cases` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/cases` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/cases/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/cases/{id}/evidence` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/cases/{id}/transition` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/cco-reports` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/cco-reports` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/cco-reports/{id}/file` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/cco-reports/{id}/sign` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/conflicts` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/conflicts` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/conflicts/{id}/resolve` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/interventions` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/interventions` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/interventions/{id}/lift` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/launch-gate` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/launch-prerequisites` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/launch-prerequisites` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/launch-prerequisites/{id}/expire` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/members` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/members/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/agreements` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/appeal-decision` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/appeals` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/decision` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/due-diligence` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/products` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/reinstate` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/reviews` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/suspend` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/members/{id}/terminate` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/rulebooks` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/rulebooks` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/rulebooks/{id}` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/rulebooks/{id}/acks` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/rulebooks/{id}/activate` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/rulebooks/{id}/approve` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/rulebooks/{id}/file` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/rulebooks/{id}/notices` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/rulebooks/{id}/regulator-decision` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/venue/self-assessments` | auth (role:Read-Only Auditor) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/self-assessments` | auth (role:Compliance Officer) | basic | live |  |  |
| `POST` | `/api/v1/admin/venue/self-assessments/{id}/complete` | auth (role:Compliance Officer) | basic | live |  |  |
| `GET` | `/api/v1/admin/webhooks/dead-letters` | auth (role:Support Agent) | basic | live |  |  |
| `POST` | `/api/v1/admin/webhooks/dead-letters/{id}/retransmit` | auth (role:Support Agent) | basic | live |  |  |
| `POST` | `/api/v1/admin/withdrawals/{id}/approve` | auth (role:Finance Ops) | basic | live | yes |  |
| `POST` | `/api/v1/admin/withdrawals/{id}/reject` | auth (role:Finance Ops) | basic | live | yes |  |
| `DELETE` | `/api/v1/algo-orders` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/algo-orders` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/allocations` | auth (scope:transfer) | basic | live |  |  |
| `GET` | `/api/v1/analytics/long-short-ratio/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/analytics/open-interest/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/analytics/pnl` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/analytics/stats` | public | public | live |  |  |
| `GET` | `/api/v1/analytics/taker-flow/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/analytics/volume` | public | public | live |  |  |
| `GET` | `/api/v1/announcements` | public | public | live |  |  |
| `GET` | `/api/v1/announcements/{id}` | public | public | live |  |  |
| `POST` | `/api/v1/auth/2fa/disable` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/auth/2fa/enroll` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/auth/2fa/setup` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/auth/2fa/verify` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/auth/forgot-password` | public | public | live |  |  |
| `POST` | `/api/v1/auth/login` | public | public | live |  |  |
| `POST` | `/api/v1/auth/logout` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/auth/passkey/assert` | public | public | live |  |  |
| `POST` | `/api/v1/auth/refresh` | public | public | live |  |  |
| `POST` | `/api/v1/auth/register` | public | public | live |  |  |
| `POST` | `/api/v1/auth/reset-password` | public | public | live |  |  |
| `POST` | `/api/v1/auth/verify-email` | public | public | live |  |  |
| `GET` | `/api/v1/book/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/bots/grid` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/bots/grid` | auth (scope:trade) | basic | live |  |  |
| `DELETE` | `/api/v1/bots/grid/{id}` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/bots/grid/{id}` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/copy/follows` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `DELETE` | `/api/v1/copy/follows/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/copy/strategies` | public | public | live |  |  |
| `POST` | `/api/v1/copy/strategies` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/copy/strategies/{id}/list` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/deposits` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/deposits/{currency}` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/developer/api-keys` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/developer/api-keys` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `DELETE` | `/api/v1/developer/api-keys/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/errors` | auth (role:Super Admin) | exempt | live |  |  |
| `GET` | `/api/v1/exchange-info` | public | public | live |  |  |
| `GET` | `/api/v1/execution-policy` | public | public | live |  |  |
| `GET` | `/api/v1/export-jobs` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/export-jobs/{id}` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/export-jobs/{id}/download` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/fees` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/funding` | auth (scope:read) | basic | live |  |  |
| `DELETE` | `/api/v1/funding/bank-accounts` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/funding/bank-accounts` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/funding/bank-accounts` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/funding/conversions` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/funding/convert` | auth (scope:transfer) | basic | live |  |  |
| `POST` | `/api/v1/funding/fee-estimate` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/funding/rail-selection` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/funding/rails` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/funding/withdrawal-whitelist` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/funding/withdrawal-whitelist/disable` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/funding/withdrawal-whitelist/enable` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/history/block-trades/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/history/klines/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/history/swap-rates` | public | public | live |  |  |
| `GET` | `/api/v1/history/ticks/{symbol}` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/history/trades/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/history/trades/{symbol}/export` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/instruments` | public | public | live |  |  |
| `GET` | `/api/v1/instruments/{symbol}/pip-value` | public | public | stub |  |  |
| `GET` | `/api/v1/instruments/{symbol}/swap-rates` | public | public | stub |  |  |
| `GET` | `/api/v1/klines/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/kyc/requirements` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/kyc/self-certification` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/kyc/self-certification` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/kyc/status` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/kyc/submit` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/maintenance/schedule` | public | public | live |  |  |
| `GET` | `/api/v1/market-data/l3-snapshot/{symbol}` | auth (scope:read) | professional | live |  |  |
| `GET` | `/api/v1/market-data/snapshot` | public | public | stub |  |  |
| `GET` | `/api/v1/market/depth` | public | public | stub |  |  |
| `GET` | `/api/v1/market/open-interest` | public | public | stub |  |  |
| `GET` | `/api/v1/market/performance` | public | public | live |  |  |
| `GET` | `/api/v1/market/positioning` | public | public | live |  |  |
| `GET` | `/api/v1/market/taker-volume` | public | public | live |  |  |
| `GET` | `/api/v1/meta/pagination` | public | public | live |  |  |
| `GET` | `/api/v1/meta/rate-limits` | public | public | live |  |  |
| `GET` | `/api/v1/openapi.json` | public | exempt | live |  |  |
| `GET` | `/api/v1/order-lists` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/order-lists` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/order-lists/history` | auth (scope:read) | basic | live |  |  |
| `DELETE` | `/api/v1/order-lists/{id}` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/order-lists/{id}` | auth (scope:read) | basic | live |  |  |
| `DELETE` | `/api/v1/orders` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/orders` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/orders` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/algo` | auth (scope:trade) | basic | live |  |  |
| `DELETE` | `/api/v1/orders/algo/{id}` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/algo/{id}/pause` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/algo/{id}/resume` | auth (scope:trade) | basic | live |  |  |
| `DELETE` | `/api/v1/orders/all` | auth (scope:trade) | basic | live |  |  |
| `DELETE` | `/api/v1/orders/batch` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/batch` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/bracket` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/cancel-all-after` | auth (scope:trade) | basic | stub |  |  |
| `POST` | `/api/v1/orders/countdown-cancel-all` | auth (scope:trade) | basic | stub |  |  |
| `POST` | `/api/v1/orders/oco` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/roll` | auth (scope:trade) | basic | stub |  |  |
| `POST` | `/api/v1/orders/scaled` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/spread` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/test` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/twap` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/orders/vwap` | auth (scope:trade) | basic | live |  |  |
| `DELETE` | `/api/v1/orders/{id}` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/orders/{id}` | auth (scope:read) | basic | live |  |  |
| `PUT` | `/api/v1/orders/{id}` | auth (scope:trade) | basic | live |  |  |
| `PUT` | `/api/v1/orders/{id}/amend/keep-priority` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/orders/{id}/amendments` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/orders/{id}/cancel-replace` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/pamm/pools` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/pamm/pools/{id}/invest` | auth (scope:transfer) | basic | live |  |  |
| `POST` | `/api/v1/pamm/pools/{id}/redeem` | auth (scope:transfer) | basic | live |  |  |
| `GET` | `/api/v1/positions` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/positions/close-all` | auth (scope:trade) | basic | stub |  |  |
| `GET` | `/api/v1/promotions/{id}` | public | basic | live |  |  |
| `GET` | `/api/v1/public/proof-of-reserves/daily-root` | public | public | live |  |  |
| `GET` | `/api/v1/reports/tca/{account_id}` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/routes` | auth (role:Super Admin) | exempt | live |  |  |
| `POST` | `/api/v1/security/disclosures` | public | public | live |  |  |
| `GET` | `/api/v1/security/policy` | public | public | live |  |  |
| `GET` | `/api/v1/session/status` | public | public | live |  |  |
| `GET` | `/api/v1/solvency/latest` | public | public | live |  |  |
| `GET` | `/api/v1/solvency/proof` | auth (scope:read) | basic | live |  |  |
| `GET` | `/api/v1/stats/24h` | public | public | live |  |  |
| `GET` | `/api/v1/stats/24h/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/strategies` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/strategies` | auth (scope:trade) | basic | live |  |  |
| `DELETE` | `/api/v1/strategies/{id}` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/strategies/{id}` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/strategies/{id}/pause` | auth (scope:trade) | basic | live |  |  |
| `POST` | `/api/v1/strategies/{id}/resume` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/strategy-templates` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/strategy-templates` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/strategy-templates/{id}/instantiate` | auth (scope:trade) | basic | live |  |  |
| `GET` | `/api/v1/support/tickets` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/support/tickets` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/support/tickets/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/system/incidents` | public | public | live |  |  |
| `GET` | `/api/v1/system/status` | public | public | live |  |  |
| `GET` | `/api/v1/tax/report` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/test/funding/deposit` | auth (cred:jwt|hmac|oauth2) | basic | live |  | nonprod |
| `POST` | `/api/v1/test/funding/withdrawal` | auth (cred:jwt|hmac|oauth2) | basic | live |  | nonprod |
| `POST` | `/api/v1/test/reset` | auth (cred:jwt|hmac|oauth2) | basic | live |  | nonprod |
| `POST` | `/api/v1/test/reset-seed` | auth (cred:jwt|hmac|oauth2) | basic | live |  | nonprod |
| `POST` | `/api/v1/test/seed` | auth (cred:jwt|hmac|oauth2) | basic | live |  | nonprod |
| `GET` | `/api/v1/ticker/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/time` | public | exempt | live |  |  |
| `GET` | `/api/v1/trades/{symbol}` | public | public | live |  |  |
| `GET` | `/api/v1/transfers` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/transfers` | auth (scope:transfer) | basic | live |  |  |
| `GET` | `/api/v1/venue/best-execution/rts27` | public | public | live |  |  |
| `GET` | `/api/v1/venue/best-execution/rts27/{id}` | public | public | live |  |  |
| `GET` | `/api/v1/venue/best-execution/rts27/{id}/csv` | public | public | live |  |  |
| `GET` | `/api/v1/venue/best-execution/rts28` | public | public | live |  |  |
| `GET` | `/api/v1/venue/best-execution/rts28/{id}` | public | public | live |  |  |
| `GET` | `/api/v1/venue/best-execution/rts28/{id}/csv` | public | public | live |  |  |
| `GET` | `/api/v1/venue/info` | public | public | stub |  |  |
| `GET` | `/api/v1/webhooks` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/webhooks` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `DELETE` | `/api/v1/webhooks/{id}` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `GET` | `/api/v1/webhooks/{id}/deliveries` | auth (scope:read) | basic | live |  |  |
| `POST` | `/api/v1/webhooks/{id}/rotate-secret` | auth (cred:jwt|hmac|oauth2) | basic | live |  |  |
| `POST` | `/api/v1/withdrawals` | auth (scope:transfer) | basic | live |  |  |
| `POST` | `/api/v1/withdrawals/{id}/confirm` | auth (scope:transfer) | basic | live |  |  |
| `GET` | `/developer` | public | public | live |  |  |
| `GET` | `/developer/migration` | public | public | live |  |  |
| `GET` | `/health` | public | exempt | live |  |  |
| `GET` | `/health/live` | public | exempt | live |  |  |
| `GET` | `/health/ready` | public | exempt | live |  |  |
| `GET` | `/ready` | public | exempt | live |  |  |
| `WS` | `/ws/market` | public | public | stub |  |  |
| `WS` | `/ws/stream` | public | public | stub |  |  |
| `WS` | `/ws/trade` | auth (cred:jwt|hmac|oauth2) | basic | stub |  |  |
| `WS` | `/ws/v1` | public | public | live |  |  |
| `WS` | `/ws/v1/l3/{symbol}` | auth (cred:jwt|hmac|oauth2) | professional | live |  |  |
| `WS` | `/ws/v1/marketdata` | public | public | stub |  |  |
| `WS` | `/ws/v1/orders` | auth (cred:jwt|hmac|oauth2) | basic | stub |  |  |

## 3. WebSocket surface

### 3.1 Upgrade endpoints (Method=WS in the registry)

| Path | Auth | Notes |
|---|---|---|
| `/ws/market` | public | Public market-data WebSocket (spec §8.6/§10.5; alias surface of /ws/v1) |
| `/ws/stream` | public | Combined stream path (?streams=a@x,b@y; subscription multiplexing) |
| `/ws/trade` | auth (cred:jwt|hmac|oauth2) | Interactive trading WebSocket (spec §8.6/§10.5; legacy alias surface of /ws/v1) |
| `/ws/v1` | public | Unified interactive WebSocket: market data, private feeds, order.* request-response |
| `/ws/v1/l3/{symbol}` | auth (cred:jwt|hmac|oauth2) | L3 order-level data stream (authenticated, premium tier; max 5 subs; 100k-event replay ring) |
| `/ws/v1/marketdata` | public | Market data stream (legacy alias of /ws/v1) |
| `/ws/v1/orders` | auth (cred:jwt|hmac|oauth2) | Private order stream (legacy alias of /ws/v1) |

The marketdata binary (`cmd/marketdata`, :8081) mounts `/ws/v1/marketdata` and `/ws/v1/orders` directly; gateway-side WS routes proxy the same machinery.

### 3.2 Control actions (client→server frames)

Session/control actions (`internal/ws/server.go` dispatch): `authenticate`, `ping`, `refresh_token`, `resume`, `subscribe`, `unsubscribe`.

Interactive order actions (`internal/marketdata/ws_dispatcher.go` `orderActionScope` — the §8.8 scope each action requires):

| Action | Required scope |
|---|---|
| `order.amend.keepPriority` | trade |
| `order.batch` | trade |
| `order.cancel` | trade |
| `order.cancelReplace` | trade |
| `order.countdown_cancel_all` | trade |
| `order.modify` | trade |
| `order.place` | trade |
| `order.status` | read |
| `order.test` | trade |

- `authenticate` accepts JWT or `ak_*` API key (+optional HMAC signature); `protocol_version` checked (UNSUPPORTED_PROTOCOL_VERSION).
- `order.*` actions are private (authenticated) order-entry over WS — `order.test` is the conformance probe.
- Abuse guards: subscription-churn guard (`WS_ABUSE_DETECTED` → close), per-conn subscription cap (`WS_MAX_SUBSCRIPTIONS_EXCEEDED`), channel-name charset allowlist, `resume` sequence-window replay.

### 3.3 Public channels (`internal/marketdata/channels.go` `channelTypes`)

`aggTrades`, `auction`, `bbo`, `blockTrades`, `book`, `depth`, `depth_full`, `greeks`, `kline`, `l3`, `l3Book`, `liquidations`, `miniTicker`, `openInterest`, `premium_l3`, `referencePrice`, `stats`, `ticker`, `trades`.

### 3.4 Private channels (`internal/ws/server.go` `PrivateChannels` — authenticated, account-scoped)

`private:balances`, `private:executions`, `private:notifications`, `private:orders`, `private:pnl`, `private:positions`.

## 4. FIX surface (Phase-18 — scaffold today)

`cmd/fix` currently binds only :8082 for `/metrics` + `/healthz` — **no FIX session acceptor is live**. The Phase-18 acceptor surface, when it lands, will be: FIX 4.4 + FIX 5.0 SP2 sessions (initiator + acceptor, `fix_sessions` table migration 030), order entry (NewOrderSingle 35=D, OrderCancelRequest 35=F, OrderCancelReplaceRequest 35=G), execution reports (35=8), market data (35=W/X), Mass Quoting (35=i), PB drop copy / Traiana affirmation, allocations (35=J/35=AK), mTLS client certification (Task 18.3.11), and an SBE binary gateway (Task 18.3.8). Retest this section when Phase-18 lands — none of it is reachable today.

## 5. Non-route surface

| Surface | Bind | Notes |
|---|---|---|
| `/metrics`, `/healthz` | per-service (8080-8085, bridge 9100+N) | Prometheus exposition + health; not client-facing but reachable on pod network |
| `GET /api/v1/routes`, `/api/v1/errors`, `/api/v1/openapi.json` | gateway :8080 | registry dumps — `openapi.json` is public, routes/errors are Super-Admin |
| Admin DLQ (`/api/v1/admin/dlq*`) | admin :8085 | dead-letter inspect/replay/discard |
| Aeron IPC media driver | host-local (`/dev/shm/aeron-*`, EXC_AERON_DIR) | CnC file + IPC rings — not network-exposed; on-host integrity is an OS problem |
| NATS JetStream | nats cluster (4222/8222) | event backbone + `ops.*` alert/DLQ subjects; mTLS per deploy/nats |
| Postgres / Redis / ClickHouse | 5433/16379+/8123 dev binds | infra-tier; pen-test in scope only via lateral movement |
