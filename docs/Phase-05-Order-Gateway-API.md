# Phase 5 — Order Gateway API (Go)

**Duration:** 9–16 days (supersedes 9–14 — duration itemization completed 2026-09-27, remediation #35: Tasks 5.3.33–5.3.40 + 5.3.46 added to §5.6)
**Dependencies:** Phases 2–3, Phase 2.5
**Spec Reference:** §8 (Order Gateway API)

---

## 5.1 Objectives

Implement the Go REST API gateway: authentication (JWT/OAuth2), rate limiting (5 tiers), all order endpoints, error code registry, OpenAPI documentation, and the route registration system that all future phases extend.

---

## 5.2 Prerequisites

- Phase 2 complete (matching engine, IPC)
- Phase 2.5 complete (engine soak passed)
- Phase 3 complete (settlement service)

---

## 5.3 Tasks

### Task 5.3.1: Authentication (JWT + OAuth2)

**Objective:** Implement JWT and OAuth2 authentication.

**File Locations:** `services/internal/auth/`

**Implementation:**
1. JWT: access token 15min, refresh token 7d. HS256 signing.
2. OAuth2: client credentials grant for institutional clients.
3. TOTP (RFC 6238) for 2FA on sensitive operations.
4. Session stored in Redis `session:{token}` with 1h TTL.
5. Middleware: `AuthMiddleware` validates JWT on every request.

**Definition of Done (Acceptance Criteria):**
* [x] JWT access token valid for 15min; refresh produces new access token
* [x] OAuth2 client credentials grant works
* [x] TOTP 2FA required for withdrawals, balance adjustments
* [x] Session stored in Redis with correct TTL

**SDD Checklist:**
- [x] Spec checkpoint: JWT 15min access / 7d refresh — defined first, validated against spec
- [x] Spec checkpoint: OAuth2 client credentials — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.2: Rate Limiting (5 Tiers)

**Objective:** Implement 5-tier rate limiting.

**File Locations:** `services/internal/middleware/rate_limit.go`

**Implementation:**
1. Tiers: Public (5/s), Basic (20/s), Standard (100/s), Professional (500/s), Institutional (2000+/s).
2. Redis token bucket: `rl:{tier}:{accountId|ip}:{second}` INCR + EXPIRE — tier-aware key scheme (corrected 2026-09-27, remediation #35; supersedes the prior `rl:{ip}:{second}` for all tiers, which could not enforce the four per-account tiers of §8.3: Public/Anonymous is keyed by IP, Basic/Pro/Institutional/MM by `accountId`; the batch key `rl:batch:{accountId}:{second}` per Task 5.3.32 and the `ORDERS` multi-interval counter per Task 5.3.40 layer on top).
3. Burst: 2x for 500ms.
4. Under Throttled mode: lower tiers reduced first.
5. Headers: `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`.

**Definition of Done (Acceptance Criteria):**
* [x] 5 tiers with correct limits and burst
* [x] Redis token bucket enforces limits
* [x] Throttled mode reduces lower tiers first
* [x] Rate limit headers present on all responses

**SDD Checklist:**
- [x] Spec checkpoint: 5 rate limit tiers — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.3: Order Endpoints

**Objective:** Implement all order REST endpoints.

**File Locations:** `services/internal/api/orders.go`

**Endpoints:**
- `POST /api/v1/orders` — submit order (→ Aeron → C++ core); idempotent on `client_order_id` (Task 5.3.24)
- `PUT /api/v1/orders/{id}` — modify order (price/qty/tif; STALE_MODIFY on stale seq — Task 5.3.22) — added 2026-09-15 per spec §8.4
- `DELETE /api/v1/orders/{id}` — cancel order
- `DELETE /api/v1/orders/all` — cancel all (per account)
- `DELETE /api/v1/orders?symbol={symbol}` — mass cancel per instrument (per account) — added 2026-09-15 (Task 5.3.25)
- `GET /api/v1/orders` — order history (paginated)
- `GET /api/v1/orders/{id}` — order detail

**Implementation:**
1. Order submission: validate → serialize to FlatBuffers → Aeron publish → await fill (async via WS or poll).
2. Cancel: send cancel via Aeron; return success when C++ confirms.
3. Modify: `PUT` reuses the modify path (Task 5.3.22 audit trail + STALE_MODIFY); price/qty change loses queue priority per spec §6.1.
4. Pagination: cursor-based (created_at + id).
5. Response: order with status, filled_qty, avg_fill_price.

**Definition of Done (Acceptance Criteria):**
* [x] Order submission reaches C++ core via Aeron
* [x] Cancel reaches C++ core; confirmation returned
* [x] Modify via `PUT` reaches C++ core; queue-priority rules applied (price/qty up = new timestamp)
* [x] Order history paginated correctly
* [x] Order detail returns correct state

**SDD Checklist:**
- [x] Spec checkpoint: order submission via Aeron to C++ core — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.4: Account & Balance Endpoints

**Objective:** Implement account and balance endpoints.

**File Locations:** `services/internal/api/account.go`

**Endpoints:**
- `GET /api/v1/account/balances` — all currency balances
- `GET /api/v1/positions` — open positions
- `GET /api/v1/account/risk-limits` — current limits + utilization

**Definition of Done (Acceptance Criteria):**
* [x] Balances endpoint returns all currencies with available/locked/total
* [x] Positions endpoint returns open positions with unrealized P&L
* [x] Risk limits endpoint returns limits + utilization

**SDD Checklist:**
- [x] Spec checkpoint: account/balance/position endpoints — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.5: Market Data REST Endpoints

**Objective:** Implement market data REST endpoints.

**File Locations:** `services/internal/api/marketdata.go`

**Endpoints:**
- `GET /api/v1/book/{symbol}?depth=20` — L2 snapshot (100ms cache)
- `GET /api/v1/trades/{symbol}?limit=100` — recent trades (1s cache)
- `GET /api/v1/ticker/{symbol}` — 24h ticker (1s cache)
- `GET /api/v1/klines/{symbol}?interval=1m&limit=500` — OHLCV candles (1s cache)
- `GET /api/v1/instruments` — public instrument reference data: symbol, base/quote, tick_size, lot_size, min_order_qty, min_notional, lifecycle status, trading hours (1min cache) — added 2026-09-15 per spec §8.4

**Boundary note (Phase 5 vs Phase 6):** Phase 5 implements the **REST endpoints** with simple time-based caching (100ms/1s). Phase 6 (Task 6.3.2) implements the **WebSocket L2 distribution** with 100ms/100-event conflation and `last_seq` replay. The REST cache is independent of the WS conflation engine; both read from the C++ core's book state via Aeron.

**Definition of Done (Acceptance Criteria):**
* [x] L2 snapshot returns top N levels with seq
* [x] Recent trades paginated
* [x] Ticker returns 24h OHLCV
* [x] Klines returns OHLCV candles
* [x] Instruments endpoint returns full tradable universe with tick/lot/min-notional/status/hours

**SDD Checklist:**
- [x] Spec checkpoint: market data REST with caching — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.6: Funding Endpoints

**Objective:** Implement funding endpoint registration/stubs only — the banking-rail implementation owner is Phase-11 (Tasks 11.3.2/11.3.3 per the AGENTS.md ownership map). Boundary pinned 2026-09-27, remediation #35: supersedes the prior full-implementation scope (this task previously implemented the same four endpoints as Phase-11 Tasks 11.3.2/11.3.3 with overlapping DoD and no cross-reference). This task registers the routes and returns 501 stubs until Phase-11 lands.

**File Locations:** `services/internal/api/funding.go`

**Endpoints:**
- `GET /api/v1/deposits/{currency}` — deposit instructions (bank details per currency)
- `POST /api/v1/withdrawals` — create withdrawal
- `POST /api/v1/withdrawals/{id}/confirm` — confirm withdrawal (15min window)
- `GET /api/v1/funding` — funding history

**Definition of Done (Acceptance Criteria):**
* [x] Deposit instructions return correct bank details per currency
* [x] Withdrawal creates with 15min confirmation window
* [x] Withdrawal confirm validates token within window
* [x] Funding history paginated

**SDD Checklist:**
- [x] Spec checkpoint: deposit instructions per currency — defined first, validated against spec
- [x] Spec checkpoint: 15min withdrawal confirmation window — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.7: Route Registration System

**Objective:** Create the central route registry that all phases extend.

**File Locations:** `services/internal/api/routes.go`

**Implementation:**
1. Central `RouteRegistry` struct: method, path, handler, auth_required, rate_tier, description.
2. All routes registered here — even those implemented in later phases (stub returns 501).
3. `GET /api/v1/routes` returns all registered routes (admin only).
4. OpenAPI spec generated from registry.
5. **Centralized request validation (remediation #10):** every registered route declares its request schema; structural validation (required fields, types, ranges, enums) is enforced centrally by middleware driven from the OpenAPI schema — handlers stay thin (authz + orchestration only). Business-rule validation (risk, exposure, instrument state) remains in domain services and the C++ core.
6. **Registration completeness invariant (remediation #10):** every endpoint declared in any phase plan MUST appear in this registry (stubbed 501 until its phase implements it); Phase-8 §24 traceability cross-checks `GET /api/v1/routes` against the phase docs.
7. **Fleet/ops-console/auditor routes (amended 2026-09-27, remediation #26):** registry carries the Task 9.3.30 fleet endpoints (`GET /api/v1/admin/fleet/environments`, `GET /api/v1/admin/fleet/hosts`, `POST /api/v1/admin/fleet/hosts/{id}/drain|cordon|decommission`, `GET /api/v1/admin/fleet/topology?env=`, `GET/POST /api/v1/admin/releases`, `POST /api/v1/admin/releases/{id}/promote`, each with required role + `env` scope + dual-control flag), the Task 15.3.12 console endpoints (`GET/POST /api/v1/admin/listing-proposals`, `POST /api/v1/admin/listing-proposals/{id}/review`, `GET /api/v1/admin/ops-board`), and the read-only auditor endpoints (`GET /api/v1/admin/archive/status` per Task 4.3.2, `GET /api/v1/admin/audit/verify` per Task 7.3.3).

**Definition of Done (Acceptance Criteria):**
* [x] All Phase 5 routes registered
* [x] Future phase routes stubbed (501 Not Implemented)
* [x] All registered routes enforce centralized schema validation; no inline per-handler structural validation (remediation #10)
* [x] `GET /api/v1/routes` returns full route list
* [x] OpenAPI spec generated from registry

**SDD Checklist:**
- [x] Spec checkpoint: central route registry for all endpoints — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.8: OpenAPI Documentation

**Objective:** Generate OpenAPI 3.1 spec and Swagger UI.

**File Locations:** `services/internal/api/openapi.go`, `services/cmd/gateway/swagger.go`

**Implementation:**
1. OpenAPI 3.1 spec generated from route registry.
2. Swagger UI at `/developer`.
3. All schemas (request/response) defined.
4. Error codes documented.

**Definition of Done (Acceptance Criteria):**
* [x] OpenAPI 3.1 spec generated
* [x] Swagger UI accessible at `/developer`
* [x] All schemas documented
* [x] Error codes documented

**SDD Checklist:**
- [x] Spec checkpoint: OpenAPI docs at /developer — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.9: API Token IP Allowlist

**Objective:** Implement restrictive IP allowlist for API tokens.

**File Locations:** `services/internal/auth/token.go`

**Implementation:**
1. API tokens have optional IP allowlist.
2. Request IP must match allowlist; else `TOKEN_IP_FORBIDDEN`.
3. IPv4 and IPv6 supported.
4. CIDR notation supported (e.g., `192.168.0.0/24`).

**Definition of Done (Acceptance Criteria):**
* [x] API token with IP allowlist rejects non-matching IP
* [x] CIDR notation works
* [x] IPv4 and IPv6 supported

**SDD Checklist:**
- [x] Spec checkpoint: API token IP allowlist — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.10: Session Management

**Objective:** Implement session management with concurrent limits.

**File Locations:** `services/internal/auth/session.go`

**Implementation:**
1. Max concurrent sessions per account (default 5).
2. Max concurrent sessions per IP (default 20).
3. Session list: `GET /api/v1/account/sessions`.
4. Revoke session: `DELETE /api/v1/account/sessions/{id}`.

**Definition of Done (Acceptance Criteria):**
* [x] Concurrent session limits enforced
* [x] Session list returns active sessions
* [x] Session revocation works

**SDD Checklist:**
- [x] Spec checkpoint: concurrent session limits — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.11: Sub-Account Hierarchy & Tiered Ceilings

**Objective:** Implement sub-account support with tiered limits up to 1,000 for institutional accounts (amended 2026-09-22: supersedes the rigid 20-sub-account cap; migration 067 `accounts.max_sub_accounts`).

**File Locations:** `services/internal/api/subaccounts.go`, `migrations/067_accounts_subaccount_limit.up.sql`

**Implementation:**
1. Master account can create sub-accounts up to the account's configured limit (`max_sub_accounts` column in `accounts`, migration 067). Default is 20 for standard retail accounts (T0/T1), 100 for corporate accounts (T2), and up to 1,000 for institutional/ECP/Prime Brokerage clients (supersedes prior flat 20).
2. Sub-accounts trade independently with segregated balances and positions.
3. Master sees aggregated view across all owned sub-accounts.
4. `GET /api/v1/account/sub-accounts` lists sub-accounts with balances, active status, and trading permissions.
5. `POST /api/v1/account/sub-accounts` creates sub-account, checking active sub-account count against `max_sub_accounts`.
6. `PUT /api/v1/admin/accounts/{id}/sub-account-limit` allows Risk Managers / Super Admins to adjust the sub-account ceiling up to 1,000 for multi-strategy institutional funds.
7. `POST /api/v1/account/sub-accounts/{id}/api-keys` allows master accounts to programmatically generate API keys restricted to a specific sub-account with scoped permissions (`read`, `trade`, no `transfer` — the §8.8 money-moving scope; remediation #35 supersedes the prior "no `withdraw`" wording, which named a scope that does not exist).

**Definition of Done (Acceptance Criteria):**
* [x] Sub-account creation enforces tiered `max_sub_accounts` limit (default 20, corporate 100, institutional up to 1,000)
* [x] Sub-accounts trade independently
* [x] Master sees aggregated balances/positions
* [x] Sub-account list, creation, and admin limit adjustment endpoints work
* [x] Master account can programmatically provision and revoke API keys scoped to individual sub-accounts

**SDD Checklist:**
- [x] Spec checkpoint: sub-account max 20 per master (tiered up to 1,000 for institutional) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.12: FROZEN Legal-Hold State

**Objective:** Implement FROZEN account state for legal holds.

**File Locations:** `services/internal/auth/account_state.go`

**Implementation:**
1. FROZEN accounts: no trading, no withdrawals.
2. Only admin (Compliance Officer+) can freeze/unfreeze.
3. Freeze reason recorded in audit log.
4. `POST /api/v1/admin/accounts/{id}/freeze` (dual control).
5. `POST /api/v1/admin/accounts/{id}/unfreeze` (dual control).

**RBAC/dual-control dependency note:** RBAC role enforcement (Task 7.3.1) and dual-control middleware (Task 7.3.2) are implemented in Phase 7, which runs after Phase 5. During Phase 5, implement the FROZEN state logic and endpoints with **stub RBAC checks** (hardcoded role check placeholder) and **stub dual-control** (single-approver placeholder). Phase 7 replaces these stubs with the real RBAC middleware and four-eyes verification when it wires the admin endpoints to the full role/permission system.

**Definition of Done (Acceptance Criteria):**
* [x] FROZEN account cannot trade or withdraw
* [x] Freeze/unfreeze requires dual control
* [x] Freeze reason in audit log

**SDD Checklist:**
- [x] Spec checkpoint: FROZEN legal-hold state — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.13: Test Environment

**Objective:** Implement separate test environment with reset.

**File Locations:** `services/internal/api/testenv.go`

**Implementation:**
1. `POST /api/v1/test/reset` — reset test account balances, orders, positions.
2. Only available in staging/test deployment.
3. Rate limited (1 reset per 5 min per account).

**Definition of Done (Acceptance Criteria):**
* [x] Test reset clears balances, orders, positions
* [x] Only available in non-production
* [x] Rate limited

**SDD Checklist:**
- [x] Spec checkpoint: test environment with reset — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.14: Announcements & Maintenance Calendar

**Objective:** Implement status page and maintenance calendar.

**File Locations:** `services/internal/api/announcements.go`

**Implementation:**
1. `GET /api/v1/announcements` — list announcements.
2. `POST /api/v1/admin/announcements` — create (admin).
3. `GET /api/v1/maintenance/schedule` — upcoming maintenance windows.
4. User dashboard shows active announcements.

**Definition of Done (Acceptance Criteria):**
* [x] Announcements list and creation work
* [x] Maintenance schedule visible
* [x] User dashboard shows announcements

**SDD Checklist:**
- [x] Spec checkpoint: announcements + maintenance calendar — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.15: Fee Promo Windows

**Objective:** Implement fee promotion windows.

**File Locations:** `services/internal/api/fees.go`

**Implementation:**
1. `fee_tiers.promo_until` and `promo_maker_bps`/`promo_taker_bps`.
2. Promo rates applied while `promo_until > now()`.
3. `GET /api/v1/fees` returns current rates (including active promos).
4. Admin: `POST /api/v1/admin/fees/promo` creates promo window.

**Definition of Done (Acceptance Criteria):**
* [x] Promo rates applied while promo_until > now()
* [x] Fee endpoint returns current rates with promo
* [x] Admin can create promo windows

**SDD Checklist:**
- [x] Spec checkpoint: fee promo windows — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.16: Developer Portal

**Objective:** Implement developer portal with API keys and documentation.

**File Locations:** `services/internal/api/developer.go`

**Implementation:**
1. `GET /developer` — Swagger UI.
2. `POST /api/v1/developer/api-keys` — create API key.
3. `GET /api/v1/developer/api-keys` — list keys.
4. `DELETE /api/v1/developer/api-keys/{id}` — revoke key.
5. Rate limit tier selection per key.
6. **Migration note:** `migrations/025_create_api_keys.up.sql` — `api_keys` table (id, account_id, key_hash, label, rate_limit_tier, ip_allowlist CIDR[], status, created_at, revoked_at).

**Definition of Done (Acceptance Criteria):**
* [x] Developer portal with Swagger UI
* [x] API key CRUD (persisted in `api_keys`, migration 025)
* [x] Rate limit tier per key

**SDD Checklist:**
- [x] Spec checkpoint: developer portal with API keys — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.17: Webhooks

**Objective:** Implement signed outbound webhooks.

**File Locations:** `services/internal/api/webhooks.go`

**Implementation:**
1. `POST /api/v1/webhooks` — register webhook URL + events.
2. Events: order_filled, order_cancelled, deposit_confirmed, withdrawal_completed.
3. HMAC-SHA256 signature in `X-Webhook-Signature` header.
4. Retry with exponential backoff (1s, 2s, 4s, 8s, 16s; max 5) — **single owner of the webhook delivery pipeline** (ownership pinned 2026-09-27, remediation #35; Phase-14 Task 14.3.12's webhook items 2–3 previously specified a divergent retry schedule (1s/5s/30s/5m/30m) and a second `webhook_dead_letters` table — superseded; that task now consumes this pipeline).
5. Dead letter queue for failed webhooks.

**Definition of Done (Acceptance Criteria):**
* [x] Webhook registration works
* [x] Events delivered with HMAC-SHA256 signature
* [x] Retry with backoff on failure
* [x] Dead letter queue for permanent failures

**SDD Checklist:**
- [x] Spec checkpoint: signed webhooks with retry — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.18: Chargeback Handling

**Objective:** Implement chargeback dispute workflow.

**File Locations:** `services/internal/api/chargebacks.go`

**Implementation:**
1. `POST /api/v1/admin/chargebacks` — create chargeback record.
2. Workflow: dispute opened → evidence collected → submitted → resolved.
3. Account may be FROZEN during dispute.
4. Evidence: trade records, communications, timestamps.

**Definition of Done (Acceptance Criteria):**
* [x] Chargeback creation and workflow work
* [x] Account can be frozen during dispute
* [x] Evidence collection automated

**SDD Checklist:**
- [x] Spec checkpoint: chargeback dispute workflow — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.19: Tax Reporting

**Objective:** Implement tax reporting (1099-B equivalent).

**File Locations:** `services/internal/api/tax.go`

**Implementation:**
1. `GET /api/v1/tax/report?year=2026` — gain/loss report by lot.
2. FIFO lot tracking.
3. Summary: total proceeds, total cost basis, net gain/loss.
4. CSV and PDF export.

**Definition of Done (Acceptance Criteria):**
* [x] Tax report with gain/loss by lot (FIFO)
* [x] Summary with totals
* [x] CSV and PDF export

**SDD Checklist:**
- [x] Spec checkpoint: tax reporting with FIFO lot tracking — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.20: API Deprecation Policy

**Objective:** Implement API version deprecation policy.

**File Locations:** `services/internal/api/deprecation.go`

**Implementation:**
1. 6-month notice before deprecation.
2. `Sunset` header on deprecated endpoints.
3. `Deprecation` header with date.
4. Migration guide published at `/developer/migration`.

**Definition of Done (Acceptance Criteria):**
* [x] Sunset and Deprecation headers on deprecated endpoints
* [x] 6-month notice enforced
* [x] Migration guide published

**SDD Checklist:**
- [x] Spec checkpoint: 6-month deprecation notice — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.21: Error Code Registry

**Objective:** Create the central error code registry.

**File Locations:** `services/internal/errors/registry.go`

**Implementation:**
1. `ErrorRegistry` maps error code → HTTP status + description.
2. Every emitted error code registered here.
3. `GET /api/v1/errors` returns all error codes (admin only).
4. Aliases noted in registry and spec §23 intro.
5. **Owner-discoverability invariant (added 2026-09-25, remediation #19):** every code in spec §23 must name the task that emits it. CI asserts that each §23 row is *resolvable* — either the code token appears in at least one phase plan, **or** the §23 description cites an owning `Phase-NN Task N.N.N` (or carries an explicit `reserved, never emitted` marker). 24 of 124 codes had neither a phase reference nor an owner citation; remediation #19 added the citations. `CORPORATE_ACTION_SCHEDULED` is marked `reserved, never emitted` — no corporate actions exist in fiat spot FX (spec §1 fiat-only scope), so it must never be raised.

**Definition of Done (Acceptance Criteria):**
* [x] All error codes from spec §23 registered
* [x] Error response includes code, HTTP status, description
* [x] `GET /api/v1/errors` returns full registry
* [x] Every §23 code is owner-resolvable (referenced by a phase plan **or** cited with an owning `Phase-NN Task` / `reserved` marker in §23) — zero ownerless codes

**SDD Checklist:**
- [x] Spec checkpoint: central error code registry — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.22: Order-Modify Audit Trail + STALE_MODIFY Rejection

**Objective:** Implement order-modify audit trail (old/new fields in `order_audit` table) and STALE_MODIFY rejection (rejects stale `order_seq` on modify). Per spec §24 criteria #81 and #82.

**File Locations:** `services/internal/api/orders.go` (extend), `services/internal/audit/order_audit.go`

**Implementation:**
1. `order_audit` table: `audit_id`, `order_id`, `account_id`, `field_name`, `old_value`, `new_value`, `modified_by`, `modified_at`, `ip_address`.
2. On every order modify (PUT /api/v1/orders/{id}): write old and new field values to `order_audit`.
3. Fields audited: price, quantity, time_in_force, stop_price, iceberg_visible_qty.
4. STALE_MODIFY: client sends `order_seq` on modify; if `order_seq` < current order sequence → reject with `STALE_MODIFY` (HTTP 409).
5. Audit trail queryable: `GET /api/v1/admin/orders/{id}/audit` (Compliance Officer+).

**Migration note:** Create migration `024_create_order_audit.up.sql` with the `order_audit` table schema listed above. This table was missing from the original Phase 1 migration list (001–020).

**Definition of Done (Acceptance Criteria):**
* [x] `order_audit` table created with old/new field values
* [x] Every order modify writes audit entry with old and new values
* [x] Audited fields: price, quantity, time_in_force, stop_price, iceberg_visible_qty
* [x] STALE_MODIFY: stale `order_seq` on modify rejected with HTTP 409
* [x] Audit trail queryable via admin endpoint (Compliance Officer+)
* [x] Audit entries include modified_by, modified_at, ip_address

**SDD Checklist:**
- [x] Spec checkpoint: order-modify audit trail with old/new fields in order_audit table — defined first, validated against spec
- [x] Spec checkpoint: STALE_MODIFY rejects stale order_seq on modify — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.23: Internal Transfers

**Objective:** Implement `POST /api/v1/transfers` — internal balance transfers (master↔sub-account and account↔account for the same user) with double-entry GL posting per spec §5.21/§8.4. Added 2026-09-15.

**File Locations:** `services/internal/api/transfers.go` (thin endpoint: authz + validation only); posting lives in the Phase-3 settlement service ledger module (Task 3.3.6 — reuse, do not duplicate). (Remediation #10 supersedes the prior gateway-side `services/internal/ledger/posting.go` location.)

**Implementation:**
1. Endpoint: `POST /api/v1/transfers {from_account_id, to_account_id, currency, amount}` — caller must own both accounts (same `user_id`, or master↔sub within the same hierarchy).
2. Execution: delegated to the Phase-3 ledger service (Task 3.3.6) — one `SERIALIZABLE` transaction debits sender and credits recipient `balances` and posts the GL journal entry (transfer clearing account). The gateway endpoint enforces authz (same-`user_id` / master↔sub ownership) and schema validation only; the HTTP layer never writes ledger/balance tables directly (layer-boundary rule — remediation #10 supersedes direct gateway-side balance writes).
   - **Double-Entry GL & Real-Time Sync Invariant (amended 2026-09-27, remediation #38):** Direct SQL mutations on `balances` or bypassing `DoubleEntryLedgerService::postJournal()` is strictly forbidden. The transfer MUST atomically generate balanced DEBIT and CREDIT lines in `ledger_entries` with `entry_type = 'TRANSFER'`. Upon commit, the ledger service MUST emit a `BalanceChanged` event to NATS JetStream, triggering real-time WebSocket balance updates on both source and destination client connections.
3. No banking rail, no fee, no withdrawal-confirmation flow — internal transfers settle instantly inside the ledger; they still emit `transfer_completed` notifications + audit entries.
4. Rejects: insufficient balance, cross-user transfer, FROZEN/SUSPENDED account on either side.

**Definition of Done (Acceptance Criteria):**
* [x] Master↔sub and same-user account↔account transfers succeed atomically
* [x] GL journal entry posted per transfer (debit/credit balanced)
* [x] Cross-user and FROZEN/SUSPENDED transfers rejected with specific error codes
* [x] Transfers appear in funding history and audit log

**SDD Checklist:**
- [x] Spec checkpoint: internal transfer endpoint with GL posting (§8.4, §24 #144) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: concurrent transfers draining same balance, transfer into account under margin call

---

### Task 5.3.24: HMAC Request Signing + Idempotent Order Submission

**Objective:** Implement programmatic API-key authentication (HMAC-SHA256) and idempotent order submission per spec §8.1/§24 #147–148. Added 2026-09-15.

**File Locations:** `services/internal/api/middleware/hmac.go`, `services/internal/api/orders.go` (extend)

**Implementation:**
1. **HMAC auth:** clients sign `HMAC-SHA256(secret, timestamp + method + path + body)`; headers `X-API-KEY`, `X-SIGNATURE`, `X-TIMESTAMP`. Server recomputes; mismatch → `INVALID_SIGNATURE` (401). Timestamp outside 30s replay window → `TIMESTAMP_OUT_OF_WINDOW` (401). `(api_key, timestamp, signature)` replay cache in Redis rejects replays.
2. **Scope:** HMAC auth is the programmatic-alternative to JWT for order/account endpoints; API keys issued via developer portal (Task 5.3.16) carry permission scopes (`trade`, `read`, `transfer` none by default) and optional IP allowlist (Task 5.3.9).
3. **Idempotent submit & Account-Scoped Namespacing (amended 2026-09-27, remediation #38):**
   - `POST /api/v1/orders` with a `client_order_id` already present for the account returns the stored order ack (HTTP 200, same body as original), never a second order. Uniqueness enforced by `UNIQUE(account_id, client_order_id)` (spec §5.4) + lookup-before-insert.
   - **Account-Scoped Namespacing Invariant:** All idempotency keys across orders, deposits, and transfers MUST be namespaced by account ID: `idem:{account_id}:{idempotency_key}` in Redis and `(account_id, idempotency_key)` in database tables. Global un-namespaced keys (`idem:{key}`) are strictly prohibited to prevent cross-account key collisions.
   - **Unique Constraint Conflict Replay:** Handlers MUST catch unique constraint conflicts (`23505`) and inspect existing record payloads; if the payload matches, replay the original idempotent transaction acknowledgment (HTTP 200/201) rather than throwing an unhandled database exception (HTTP 500).
   - **Mismatch branch (added 2026-09-27, remediation #35):** an identical `client_order_id` resubmitted with a *different* payload rejects with `IDEMPOTENCY_KEY_COLLISION` (HTTP 409, spec §8.7) — the prior text implemented only the happy path while §23/§8.7 cited this task as the owner. Parallel mechanism note: `IDEMPOTENCY_KEY_MISMATCH` (422, Task 5.3.42) covers the `Idempotency-Key` header on money-moving POSTs — two distinct surfaces, two codes, not duplicates.
4. Error codes registered in Task 5.3.21 registry.
5. Mass cancel filter dimensions: `{account_id?, instrument_id?, side? (BUY/SELL/ALL), order_type? (LIMIT/STOP/ALL)}`
6. Corresponding FIX support: OrderMassCancelRequest (35=q) registered for Phase-18 implementation.
7. Admin mass cancel: Risk Manager role can mass-cancel across accounts (with audit log).

**Definition of Done (Acceptance Criteria):**
* [x] HMAC-signed request authenticates; bad signature → INVALID_SIGNATURE
* [x] Timestamp > 30s skew → TIMESTAMP_OUT_OF_WINDOW; replayed signature rejected
* [x] Duplicate `client_order_id` returns original ack (no duplicate order, no duplicate fills)
* [x] API key scopes enforced (read-only key cannot submit orders)

**SDD Checklist:**
- [x] Spec checkpoint: HMAC request signing + 30s replay window (§8.1, §24 #147) — defined first, validated against spec
- [x] Spec checkpoint: idempotent order submission on client_order_id (§8.4, §24 #148) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: clock skew boundary, concurrent duplicate submits, revoked key mid-session

---

### Task 5.3.25: Scoped Mass Cancellation

**Objective:** Extend mass-cancel beyond `DELETE /orders/all` — per-instrument cancel-all for a single account per spec §8.4/§24 #153. Added 2026-09-15.

**File Locations:** `services/internal/api/orders.go` (extend), `core/src/matching/MassCancel.cpp` (extend)

**Implementation:**
1. `DELETE /api/v1/orders?symbol={symbol}` — cancels all open orders for the caller's account on that instrument only; returns count.
2. `DELETE /api/v1/orders/all` (existing) — all instruments; response now includes per-symbol breakdown.
3. Engine: per-shard mass-cancel event via Aeron; each cancel emits WAL `ORDER_CANCEL` (reason=MASS_CANCEL); response confirms when all shards ack.
4. **REST/WS cancel-on-disconnect (spec §24 #153):** accounts may set `cancel_on_disconnect` (account-level flag, default false for REST/WS); on WS session drop or API-key session timeout, open orders submitted via that session are mass-cancelled through this same path. FIX-session CoD is separate (Phase-18 Task 18.3.9).
5. Market-maker MMP mass-cancel (spec §9.6) reuses this path scoped by `mm_programs.id` — wired in Phase-18 Task 18.3.10.

**Definition of Done (Acceptance Criteria):**
* [x] Per-instrument mass cancel cancels only that symbol's orders for the account
* [x] Cancel-all returns per-symbol breakdown; all shards confirm before 200
* [x] WAL events emitted per cancelled order; recovery reproduces final state
* [x] Mass cancel during HALT/SUSPEND still cancels resting orders
* [x] REST/WS `cancel_on_disconnect` flag mass-cancels session's orders on disconnect (§24 #153)

**SDD Checklist:**
- [x] Spec checkpoint: per-instrument mass cancellation (§8.4, §24 #153) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: mass cancel racing in-flight fills, cancel-all across shards, idempotent retry of the DELETE

---

---

### Task 5.3.26: WebSocket Authentication Upgrade & In-Flight Token Renewal

**Objective:** Implement seamless WebSocket connection authentication upgrade and in-flight JWT renewal per spec §10.5 and §24 #187, allowing clients to elevate unauthenticated connections and refresh tokens without disconnecting or re-subscribing. Added 2026-09-15.

**File Locations:** `services/internal/api/ws_auth.go`, `services/internal/api/ws_handler.go`

**Implementation:**
1. Support initial unauthenticated connections for public market data feeds.
2. In-flight elevation: accept incoming `{"action": "authenticate", "token": "<jwt_or_api_key>", "signature": "<optional_hmac>", "timestamp": <epoch_s>, "protocol_version": 1}` JSON frame on existing connection (canonical frame per §10.5 — remediation #9 supersedes prior `op` discriminator; frame schema aligned with §10.5 including `protocol_version` remediation #35; API-key tokens carry the `ak_` prefix and require `signature`).
3. Validate JWT signature, claims, account status, and IP allowlist. Upon success, elevate session context to authenticated state and permit subscription to private user channels (`private:orders`, `private:executions`, `private:positions`, `private:balances`) without reconnection — channel naming aligned with §10.5 (remediation #35: the `private:` prefix is canonical; the `executions` channel is added to §10.5's authorization list and is produced by Phase-06 Task 6.3.5).
4. Renewal protocol: allow client to submit `{"action": "refresh_token", "token": "<new_jwt>"}` before current token expiry (recommended 60s prior to expiry; verb per §10.5 — supersedes prior re-`authenticate` frame). Update connection session TTL without disrupting in-flight streaming or active channel subscriptions.
5. Expiry handling: if current token reaches expiration timestamp without renewal, immediately unsubscribe private channels and emit `{"type": "error", "error": "AUTH_EXPIRED", "code": 4019}` (code registered in §23; 4019 = registered RFC 6455 private close code, not a magic number), falling back to unauthenticated state or terminating if only private channels active.

**Definition of Done (Acceptance Criteria):**
* [x] Unauthenticated WS connection elevates to authenticated state via `{"action": "authenticate", "token": "..."}` frame (§24 #187)
* [x] Token renewal frame extends connection auth expiration seamlessly with zero message drops
* [x] Private subscriptions fail prior to authentication and succeed immediately after upgrade
* [x] Expired tokens without renewal trigger graceful private channel teardown with registered WS close code 4019 (§23)

**SDD Checklist:**
- [x] Spec checkpoint: WebSocket authentication upgrade and renewal (§10.5, §24 #187) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: invalid JWT token format, expired renewal token, renewal race with channel message delivery

---

### Task 5.3.27: Standard HTTP Rate-Limit Headers & Retry-After Middleware

**Objective:** Implement standard RFC 6585 / IETF draft rate-limiting headers and retry-after response semantics across all REST gateway endpoints per spec §8.3 and §24 #192. Added 2026-09-15.

**File Locations:** `services/internal/middleware/ratelimit.go`, `services/internal/api/router.go`

**Implementation:**
1. Rate limit header emission: inject standard headers on all REST responses:
   - `X-RateLimit-Limit`: maximum allowed requests in current window
   - `X-RateLimit-Remaining`: remaining requests allowed in current window
   - `X-RateLimit-Reset`: integer seconds remaining until current rate limit window resets

  (Header naming per spec §8.3 / §24 #192 `X-RateLimit-*` — remediation #9 supersedes prior prefix-less `RateLimit-*`.)
2. 429 Too Many Requests response handling: when threshold exceeded, return HTTP 429 status with `Retry-After: <seconds>` header per RFC 6585 and standardized JSON payload `{ "error": "RATE_LIMIT_TIER_EXCEEDED", "retry_after": <seconds> }` (tiered API quota per §23 registry — remediation #9 supersedes prior `RATE_LIMIT_EXCEEDED`, which is reserved for the Phase-02 per-account order-rate collar).
3. Support tier-aware limits: dynamically calculate headers based on caller identity (Public/Anonymous, Authenticated Basic, Authenticated Pro, Institutional/MM, Admin).
4. Redis token bucket integration: sync header counters from Redis cluster/Sentinel rate-limiting keys with minimal latency overhead (<50µs).

**Definition of Done (Acceptance Criteria):**
* [x] All REST endpoints return `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and `X-RateLimit-Reset` headers (§24 #192)
* [x] HTTP 429 responses include RFC 6585 `Retry-After` header with seconds until reset
* [x] Rate limit counters accurately reflect tier allocations and decrement per request
* [x] Degraded `Throttled` mode lowers limits and updates headers dynamically

**SDD Checklist:**
- [x] Spec checkpoint: Standard HTTP rate-limit headers (§8.3, §24 #192) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: clock skew between client and server, burst traffic exhaustion, multi-node rate sync

---

### Task 5.3.28: API Versioning Middleware
Added 2026-09-17 (gap analysis remediation #6).

Implement the API versioning strategy (spec §8.6, §24 #208):
1. REST URL routing: major version in path (`/api/v1/`, `/api/v2/`). Go router registers version-prefixed route groups.
2. Response headers: `X-API-Version: 1.2.3` (semantic version), `X-Deprecated-Field: fieldName` for deprecated fields.
3. WS protocol version: negotiated in auth frame (`{"action": "authenticate", "protocol_version": 1}` per §8.6) via `protocol_version` field. Server rejects unknown versions with `{"type": "error", "error": "UNSUPPORTED_PROTOCOL_VERSION"}` (standard error envelope — remediation #9).
4. Parallel version support: when v2 is introduced, v1 handler chain continues serving for 12 months. Version-aware middleware routes to correct handler.
5. Sunset header: `Sunset: <HTTP-date>` on deprecated endpoints per RFC 8594, matching the 6-month deprecation policy (Task 5.3.20).
6. Health endpoint schema: `GET /health` returns `{"status": "ok|degraded|down", "mode": "Normal|...", "shards": [{"id": N, "status": "ok|degraded", "leader": true}], "version": "1.2.3", "timestamp": "ISO8601"}`.

### Task 5.3.29: API Gateway & Load Balancer Specification

**Objective:** Define and implement the L7 API gateway layer for request routing, TLS termination, authentication, and observability.

**File Locations:** `infrastructure/gateway/haproxy.cfg`, `services/internal/gateway/reverse_proxy.go`

**Implementation:**
1. HAProxy (L4/L7) for TLS termination, connection pooling, and health-check-based routing to Go service instances.
2. Go reverse proxy middleware: authentication (JWT/HMAC validation), rate limiting (from Task 5.3.27), request tracing (OpenTelemetry trace-id injection).
3. Health check endpoints: `GET /health/live` (liveness, always 200 if process up), `GET /health/ready` (readiness, checks PostgreSQL + Redis + NATS connectivity). Schema per ruling R9.
4. Blue-green deployment routing: HAProxy backend switching via admin socket. Zero-downtime deploys.
5. WebSocket upgrade handling: HAProxy `option http-server-close` disabled for WS paths; sticky sessions by connection ID.
6. Circuit breaker: if backend error rate > 50% over 10s window, trip circuit and return 503 with `Retry-After` header. *(Layered with Task 5.3.41's Go-middleware breaker — interaction pinned 2026-09-27, remediation #35: the Go middleware (fast local trip at >15%/10s, `SERVICE_DEGRADED`) always trips first in practice; this HAProxy breaker is the coarse global layer. Both are deliberate layers, not duplicates.)*
7. Request size limits: 1MB max body for REST, 64KB max frame for WS.
8. Access logging: structured JSON logs with request_id, user_id, latency_ms, status_code, endpoint.
9. **(amended 2026-09-25 — governance remediation #17, claims the previously unassigned API-gateway CORS/CSP emission):** the gateway is the authoritative emitter of `Access-Control-Allow-Origin`/`-Methods`/`-Headers`/`-Max-Age` (allowlist from config, never `*` on authenticated paths), `Content-Security-Policy` for any gateway-served asset, and `Strict-Transport-Security`/`X-Content-Type-Options`/`X-Frame-Options`/`Referrer-Policy`/`Permissions-Policy` on every response. The SPA-served header set is Phase-10 Task 10.3.1's responsibility; this is the gateway layer. CI asserts every route registered in Task 5.3.7 returns a complete hardening-header set — a route that omits one fails the build.

**Definition of Done (Acceptance Criteria):**
* [ ] HAProxy routes traffic to Go services with health-check failover — *configured: `/health/ready` L7 checks gate be_gateway/be_ws backends with fall/inter retries; live failover unverified — no `haproxy` binary on this host (deploy-time check)*
* [x] TLS termination with certificate rotation support
* [x] Authentication middleware validates JWT/HMAC before routing
* [x] Health endpoints return correct status per dependency checks
* [ ] Blue-green deploy switches traffic with zero dropped requests — *configured: active_color.map + stats-socket `set map` flip; zero-drop live flip unverified — no `haproxy` binary on this host (deploy-time check)*
* [x] Circuit breaker trips on sustained backend errors
* [x] Gateway emits the full hardening-header set (CORS allowlist, CSP, HSTS, X-Content-Type-Options, X-Frame-Options, Referrer-Policy, Permissions-Policy) on every response — owned by this task; the SPA-served set is Phase-10 Task 10.3.1

**SDD Checklist:**
- [x] Spec checkpoint: API gateway architecture — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [ ] Edge cases: all backends down, TLS cert expiry, WebSocket upgrade through proxy — *config-covered: all-backends-down → L7 check fail → 503; TLS expiry is operational (crt dir + `-sf` reload); WS upgrade through proxy configured (timeout tunnel + Upgrade ACL); live paths unverified — env-blocked*

---

### Task 5.3.30: Manual Liquidation Admin API Endpoint

**Objective:** Provide Risk Managers with an explicit API endpoint to trigger manual position liquidation, routed through 4-eyes dual control (Phase-07 Task 7.3.2). Added 2026-09-20 (feature-completeness audit remediation #11).

**File Locations:** `services/internal/admin/manual_liquidation.go`

**Implementation:**
1. `POST /api/v1/admin/liquidation/manual` with request schema: `{account_id: string, instrument_id?: string, reason: string, override_auction: bool}`. Requires Risk Manager role + dual control approval.
2. Validates account exists, has open positions matching criteria. If `instrument_id` is null, liquidates all positions on the account.
3. Generates WAL event `MANUAL_LIQUIDATION` with `source: MANUAL` flag, routed to Phase-19 liquidation auction machinery.
4. If `override_auction` is true, bypasses the 5s CALL auction and executes immediate FORCE_CASH at mark price. Requires explicit justification in `reason` field.
5. Response includes `liquidation_id`, estimated fill quantities, and the dual-control approval record.
6. All manual liquidation events are logged in the audit hash chain with both approver IDs.

**Definition of Done (Acceptance Criteria):**
* [x] `POST /api/v1/admin/liquidation/manual` endpoint functional with Risk Manager+ RBAC
* [x] Dual control approval required before execution
* [x] WAL event MANUAL_LIQUIDATION generated with source: MANUAL
* [x] Override-auction mode bypasses CALL phase with explicit reason
* [x] Audit trail includes both approver IDs and full request payload

**SDD Checklist:**
- [x] Spec checkpoint: manual liquidation admin endpoint — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: account with no positions, concurrent automated + manual liquidation, override during active auction

---

### Task 5.3.31: Interactive WebSocket Trading API (Request-Response Protocol)

**Objective:** Enable high-performance, low-latency bidirectional trading over persistent WebSocket connections (`/ws/v1`) using correlated request-response framing per spec §8.1/§10.5 and §24 #253. Added 2026-09-22 (remediation #12).

**File Locations:** `services/internal/api/ws_trading.go`, `services/internal/marketdata/ws_dispatcher.go`

**Implementation:**
1. Clients establish an authenticated persistent session on `/ws/v1` via Task 5.3.26.
2. In-band trading action routing: gateway parses client frames containing `action`:
   - `order.place`: submit single order (`params`: symbol, side, type, price, qty, tif, client_order_id, etc.).
   - `order.cancel`: cancel order (`params`: order_id or client_order_id, symbol).
   - `order.modify`: modify order (`params`: order_id, price, qty).
   - `order.batch`: submit batch of up to 10 orders (`params`: array of order objects).
   - `order.status`: query order status (`params`: order_id or client_order_id).
3. Client provides a unique `request_id` (string) in each request frame.
4. Gateway performs session entitlement and risk pre-checks, forwards to C++ core via Aeron IPC, and immediately returns a correlated response frame:
   `{"type": "response", "request_id": "<client_request_id>", "action": "<action>", "status": "ACK"|"NACK", "data": {...}, "ts_ms": 1726400000000}`.
5. Errors return the canonical error envelope: `{"type": "error", "request_id": "<client_request_id>", "action": "<action>", "error": "<CODE>", "message": "...", "ts_ms": 1726400000000}`.
6. Execution reports continue to stream asynchronously over the client's subscribed private user channel (`private:orders`).

**Definition of Done (Acceptance Criteria):**
* [x] Persistent WebSocket session accepts `order.place`, `order.cancel`, `order.modify`, `order.batch`, `order.status` actions
* [x] Every response frame preserves client `request_id` with sub-millisecond gateway processing overhead
* [x] Rejected requests return canonical error envelope with mapped spec §23 error codes
* [x] Concurrent order actions do not interfere with or delay market data streaming on the same socket

**SDD Checklist:**
- [x] Spec checkpoint: Interactive WebSocket Trading API with correlated request-response framing — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: unauthenticated trade frame (rejected 4019), duplicate request_id, socket disconnect during pending order ACK

---

### Task 5.3.32: REST Batch Order Operations (`POST /api/v1/orders/batch` & `DELETE /api/v1/orders/batch`)

**Objective:** Implement batch order placement and batch cancellation REST endpoints to minimize HTTP round trips for algorithmic traders per spec §8.3/§8.4 and §24 #254. Added 2026-09-22 (remediation #12).

**File Locations:** `services/internal/api/batch_orders.go`

**Implementation:**
1. `POST /api/v1/orders/batch`:
   - Request schema: `{"orders": [<OrderParams>]}` with validation: min 1, max 10 orders per payload.
   - Enforces Redis batch rate limit: `rl:batch:{accountId}:{second}` (Phase 4).
   - Validates each order against pre-trade risk and balance reservation; routes valid orders to Aeron ring buffer.
   - Response returns array of results preserving input indices:
     `{"results": [{"index": 0, "status": "ACCEPTED", "order_id": "...", "client_order_id": "..."}, {"index": 1, "status": "REJECTED", "error": "INSUFFICIENT_BALANCE"}]}`.
   - HTTP status is 200 if at least one order is accepted or evaluated; 400 only if the overall batch payload schema is malformed.
2. `DELETE /api/v1/orders/batch`:
   - Request schema: `{"order_ids": ["..."], "client_order_ids": ["..."]}` (max 20 orders total).
   - Dispatches cancellations to matching engine shards; returns array of cancellation statuses per order.
3. Full audit logging for batch operations in `order_audit`.

**Definition of Done (Acceptance Criteria):**
* [x] `POST /api/v1/orders/batch` accepts up to 10 orders and returns index-mapped results
* [x] `DELETE /api/v1/orders/batch` cancels up to 20 orders in one call
* [x] Batch payload > 10 orders rejected with `BATCH_SIZE_EXCEEDED` (HTTP 400)
* [x] Redis rate limit `rl:batch:{accountId}:{second}` enforced

**SDD Checklist:**
- [x] Spec checkpoint: REST batch orders submit and cancel endpoints — defined first, validated against spec
- [x] All spec checkpoints pass after implementation
- [x] Edge cases: mixed valid and invalid orders in single batch, duplicate client_order_id within same batch, batch cancel on non-existent orders

---

### Task 5.3.33: Dead-man switch / countdown cancel-all

Dead-man switch / countdown cancel-all — `POST /api/v1/orders/countdown-cancel-all` with `countdown_ms` parameter (min 1000, max 300000 — range aligned with the Phase-10 UI's 30s–300s configurable timeout, remediation #35; supersedes the prior max 60000, which forced the server to reject valid UI values). Server maintains per-account timer; if not renewed before expiry, atomically cancels all resting orders for that account. `countdown_ms=0` disables. Response includes `server_time` and `countdown_expiry`. Timer state stored in Redis `countdown:{account_id}` with TTL. Also exposed via WS trading API action `order.countdown_cancel_all`. Register error codes: `COUNTDOWN_INVALID_DURATION`, `COUNTDOWN_ALREADY_ACTIVE`.

**SDD Checklist:**
- [x] Spec checkpoint: dead-man switch is shared across REST/WS/FIX and cancels atomically (§24 #257) — defined first, validated against spec

---

### Task 5.3.34: Progressive IP ban escalation

Progressive IP ban escalation — extend API gateway rate limiting (Task 5.3.29 HAProxy) with ban escalation for clients that continue sending requests after receiving HTTP 429. Tiers: 1st offense within 1min of 429 = 2min ban, 2nd offense = 30min ban, 3rd offense = 24h ban. Return HTTP 418 with `X-Ban-Expires` and `Retry-After` headers. Ban state tracked in Redis `ip_ban:{ip}` with TTL. Admin dashboard for ban review/override. Register error code: `IP_BANNED`.

**SDD Checklist:**
- [x] Spec checkpoint: repeated post-429 abuse escalates to auditable timed HTTP 418 bans (§24 #258) — defined first, validated against spec

---

### Task 5.3.35: Structured instrument filter objects

Structured instrument filter objects — extend `GET /api/v1/instruments` response (Task 5.3.25) to include a `filters` array per instrument surfacing all validation rules: `PRICE_FILTER` (min_price, max_price, tick_size), `LOT_SIZE` (min_qty, max_qty, step_size), `MIN_NOTIONAL` (min_notional), `PRICE_BAND` (price_band_pct_up, price_band_pct_down), `MAX_ORDERS` (max_open_orders, max_algo_orders), `SPREAD_PROTECTION` (max_spread_pips). Enables client-side pre-validation.

**SDD Checklist:**
- [x] Spec checkpoint: instrument responses expose all effective structured validation filters (§24 #259) — defined first, validated against spec

---

### Task 5.3.36: Close-all positions convenience endpoint

Close-all positions convenience endpoint — `POST /api/v1/positions/close-all` accepting optional filters `symbol`, `side`. Internally dispatches scoped mass cancel of working orders followed by batch market close orders for each open position with `reduce_only=true` and slippage protection. Returns array of order acknowledgments. Requires 2FA confirmation header `X-2FA-Token`. Register error code: `CLOSE_ALL_PARTIAL_FAILURE` (if some closes succeed but others fail).

**SDD Checklist:**
- [x] Spec checkpoint: close-all uses reduce-only protected closes and reports partial failure (§24 #260) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.37: Atomic Cancel-Replace and Keep-Priority Amendment APIs

**Objective:** Complete order-mutation parity across REST, interactive WS, and FIX.

**Implementation:**
1. Add `POST /api/v1/orders/{id}/cancel-replace` and WS `order.cancelReplace` with `mode=STOP_ON_FAILURE|ALLOW_FAILURE`; return HTTP/WS 409 with distinct cancel/new outcomes on partial success.
2. Add `PUT /api/v1/orders/{id}/amend/keep-priority` and WS `order.amend.keepPriority`; only quantity reduction is allowed and the order ID/timestamp remain unchanged.
3. Emit `REPLACED` private-stream events and add `GET /api/v1/orders/{id}/amendments` for client-visible amendment history.
4. Map both operations to FIX cancel/new and keep-priority messages in Phase-18; idempotency and stale-sequence checks remain mandatory.

**SDD Checklist:**
- [x] Spec checkpoint: atomic cancel-replace and quantity-down keep-priority operations expose complete outcome/history semantics (§24 #281–282) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.38: Ed25519 and RSA Programmatic API Keys

**Objective:** Add asymmetric request signing while retaining HMAC only for migration compatibility. Migration 073 adds key type, public key, algorithm metadata, and rotation overlap fields.

**Implementation:**
1. API keys declare `key_type=ED25519|RSA|HMAC`; store asymmetric public keys only and support RSA-2048/4096 plus Ed25519 verification.
2. Recommend Ed25519 for REST/WS; require Ed25519 for FIX API sessions. HMAC issuance is deprecated for new institutional accounts.
3. Canonicalize payloads identically across algorithms, preserve timestamp/replay protection, scopes, IP allowlists, auto-expiry, and revocation.
4. Developer portal publishes generation, rotation, overlap, and migration workflows without ever receiving private keys.

**SDD Checklist:**
- [x] Spec checkpoint: Ed25519/RSA authentication works across REST/WS and private keys never enter the platform (§24 #283) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.39: Quote-Denominated Market Orders and Dry-Run Preview

**Objective:** Support “spend/receive this quote-currency amount” and risk/fee preview without execution.

**Implementation:**
1. Market orders accept exactly one of `quantity` or `quote_quantity`; BUY spends at most the quote amount, SELL targets quote proceeds, and executed base quantity is lot-rounded without exceeding the requested quote amount.
2. `POST /api/v1/orders/test` runs schema, filters, entitlement, margin, price-range, and estimated commission checks without reserving funds, writing WAL, or creating an order.
3. Preview returns estimated base/quote quantity, margin, commission/spread/swap estimate, active filters, `risk_level` (LOW/MEDIUM/HIGH — drives the confirmation-modal severity per spec §21.12; added 2026-09-27, remediation #35), and warnings; it is explicitly non-binding.
4. Expose equivalent `order.test` WS action and Lite-UI preview.

**SDD Checklist:**
- [x] Spec checkpoint: quote-denominated market orders and side-effect-free order preview are deterministic and filter-compliant (§24 #286) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.40: Rate-Limit and Account Trading Introspection

**Objective:** Make effective limits and account-specific trading metadata queryable.

**Implementation:**
1. Add per-route request weights and multi-interval `RAW_REQUESTS`, `REQUEST_WEIGHT`, and `ORDERS` counters; successful cancel/order paths may carry zero request weight while failed requests remain charged.
2. Add `GET /api/v1/account/rate-limits`, `GET /api/v1/account/filters/{symbol}`, `GET /api/v1/account/commission/{symbol}`, and client-authorized prevented-match/amendment/SOR-allocation queries.
3. REST headers and WS responses expose limit, count, interval, and retry time; counters are account-wide across keys where specified and IP-wide for request weight.
4. Register every route and emitted error in Tasks 5.3.7/5.3.21.

**SDD Checklist:**
- [x] Spec checkpoint: clients can query weighted request/order usage and effective account filters/fees/history (§24 #288) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.41: Comprehensive Error Mapping, Circuit Breaker & RFC 7807 Envelope

**Objective:** Standardize API gateway error response serialization with RFC 7807 Problem Details, implement gateway-level circuit breakers, and enforce engine timeout fallbacks per spec §2.7, §8.7, §22.7, and §24 #304.

**Implementation:**
1. **RFC 7807 Middleware:** Enforce unified error schema `{"type": "error", "error": "<CODE>", "message": "...", "status": N, "request_id": "...", "details": {...}}` across all REST endpoints and WebSocket error frames.
2. **Gateway-to-Core Timeout Guard:** Impose strict 500ms timeout on unacknowledged Aeron IPC submissions; on expiry, return `GATEWAY_TIMEOUT_MATCHING_ENGINE` (HTTP 504) without leaving hanging client connections.
3. **Gateway Circuit Breaker:** Implement error-rate trip monitor in Go middleware. If upstream matching engine or persistence layer error rate exceeds 15% over 10 seconds, transition gateway to open circuit breaker state returning `SERVICE_DEGRADED` (HTTP 503) with 5-second backoff probe.

**Definition of Done (Acceptance Criteria):**
* [x] All gateway error responses conform to RFC 7807 schema with request_id
* [x] Engine unacknowledged timeouts return HTTP 504 within 500ms
* [x] Gateway circuit breaker opens at >15% error rate and probes recovery

**SDD Checklist:**
- [x] Spec checkpoint: RFC 7807 error envelope, circuit breaker, and engine timeout fallbacks (§24 #304) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.42: API Hardening — Pagination Envelope, Idempotency, Auth Lifecycle & Rate Weights

**Objective:** Close the five REST/WS contract gaps a production API review flags: envelope drift, orders-only idempotency, HS256/session vagueness, untabulated rate weights, and WS retry duplicates. Per spec §8.8 and §24 #338. Added 2026-09-27 (production-maturity remediation #24).

**File Locations:** `services/internal/api/envelope.go`, `services/internal/middleware/idempotency.go`, `services/internal/auth/lifecycle.go`

**Implementation:**
1. **Global list envelope:** every paginated `GET` (orders, trades, funding, positions, login-history, sessions, klines, history) returns `{data, next_cursor, limit, total}` with cursor `(created_at, id)`; default/max limits tabulated per endpoint in OpenAPI (reconciles the `limit=500/1500` vs `limit=100` drift — supersedes per-endpoint ad-hoc limits). Sort/filter matrix per endpoint published in `GET /api/v1/routes` metadata.
2. **Cross-endpoint idempotency:** `Idempotency-Key` header (UUIDv7, 24h window) required on `POST /withdrawals`, `/transfers`, `/funding/bank-accounts`, `/orders/batch`; WS `order.place` retries carry `request_id` with a 60s server dedup window — same key + same payload replays the stored ack, same key + different payload rejects `IDEMPOTENCY_KEY_MISMATCH` (HTTP 422, new §23 code).
3. **Auth/session/key lifecycle:** password policy (min 12 chars, breach-corpus screen), bcrypt cost 12; JWT adds `kid` rotation (RS256/KMS path alongside HS256) with refresh-token rotation + reuse detection; idle 30min vs absolute 8h session timeouts; eviction oldest-first when 5/account or 20/IP caps hit. API-key scope matrix (`read/trade/transfer/admin`) enforced on REST, WS `authenticate` and FIX logon; Ed25519 rotation cadence 90 days; IP-allowlist checked on all three surfaces.
4. **Rate-weight table:** per-route weights and `RAW_REQUESTS / REQUEST_WEIGHT / ORDERS` multi-interval counters tabulated in spec §8.3; burst refill math, Sentinel-failover counter correctness, 418 ban durations with allowlist bypass, and per-IP WS connection caps published; over-weight rejects `REQUEST_WEIGHT_EXCEEDED` (HTTP 429, new §23 code).
5. **Registry:** the 2 new codes above are registered through the Task 5.3.21 procedure (HTTP status + owner citation in §23).

**Definition of Done (Acceptance Criteria):**
* [x] All list endpoints share the envelope; OpenAPI tabulates limits/filters/sorts
* [x] Safe retry on all money-moving POSTs and WS order actions; mismatch rejected with code
* [x] Auth lifecycle enforced on REST/WS/FIX; rotation and eviction verified
* [x] Weight table published; over-weight rejected with REQUEST_WEIGHT_EXCEEDED

**SDD Checklist:**
- [x] Spec checkpoint: unified list envelope, cross-endpoint idempotency, auth lifecycle, and tabulated rate weights with two new §23 codes (§24 #338) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.43: Server-Time Endpoint for Clock Sync

**Objective:** Give HMAC clients a canonical clock to sync against the 30s signature window, per spec §8.9 and §24 #355. Added 2026-09-27 (Binance-parity remediation #28).

**File Locations:** `services/internal/api/time.go`

**Implementation:**
1. `GET /api/v1/time` (public, tier-exempt from weights) returns `{server_time_ms, timezone: "UTC"}` sourced from the PTP-disciplined clock (Phase-09 Task 9.3.12).
2. Documented skew guidance: clients syncing beyond ±5s SHOULD re-sync before signing; the 30s acceptance window itself is unchanged.

**Definition of Done (Acceptance Criteria):**
* [x] Endpoint returns PTP-sourced UTC millis with sub-second accuracy
* [x] Tier-exempt and present in OpenAPI + route registry

**SDD Checklist:**
- [x] Spec checkpoint: public PTP-sourced server-time endpoint for HMAC clock sync (§24 #355) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.44: Unified Venue-Info Document

**Objective:** Publish one machine-readable venue description (limits, filters, permissions per symbol), per spec §8.9 and §24 #356. Added 2026-09-27 (Binance-parity remediation #28).

**File Locations:** `services/internal/api/exchange_info.go`

**Implementation:**
1. `GET /api/v1/exchange-info` returns, per symbol: status, order-type permissions, the Task 5.3.35 filter set (PRICE/LOT/MIN_NOTIONAL/BAND/MAX_ORDERS/SPREAD), plus venue-level `rate_limits` (the Task 5.3.42 weight table) and `server_time` — one response replacing N discovery calls.
2. ETag + `If-None-Match` support; change events emitted on WS `system.status` so clients refresh on instrument updates (Task 15.3.8).

**Definition of Done (Acceptance Criteria):**
* [x] Single response carries symbols, filters, permissions and rate limits
* [x] ETag caching works; instrument changes emit refresh signals

**SDD Checklist:**
- [x] Spec checkpoint: unified venue-info document with ETag and change signaling (§24 #356) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 5.3.45: Transfer History Query

**Objective:** Make internal/sub-account money movements queryable, per spec §8.9 and §24 #363. Added 2026-09-27 (Binance-parity remediation #28).

**File Locations:** `services/internal/api/transfers.go`

**Implementation:**
1. `GET /api/v1/transfers` with the Task 5.3.42 envelope (cursor, filters: currency, direction, date range, sub-account), reading the Task 5.3.23 transfer journal.
2. Each entry carries source/destination, amount, GL reference, status and actor (self vs admin vs system sweep).

**Definition of Done (Acceptance Criteria):**
* [x] History paginates with filters; every transfer from Task 5.3.23 appears
* [x] Entries link to GL references and actor identity

**SDD Checklist:**
- [x] Spec checkpoint: paginated transfer history with GL linkage (§24 #363) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 5.4 Deliverables

- Go REST API gateway with JWT/OAuth2 auth
- 5-tier rate limiting
- All order, account, market data, funding endpoints
- Route registration system
- OpenAPI docs + Swagger UI
- API token IP allowlist
- Session management
- Sub-accounts with tiered ceilings (20/100/1,000) & programmatic API keys (Task 5.3.11 amended)
- FROZEN state, test environment
- Announcements, fee promos, developer portal
- Webhooks, chargebacks, tax reporting
- API deprecation policy
- Error code registry
- Order-modify audit trail + STALE_MODIFY rejection
- Public instrument reference endpoint (`GET /api/v1/instruments`)
- Internal transfers with GL posting (`POST /api/v1/transfers`)
- Atomic cancel-replace and keep-priority amendment/history APIs
- Ed25519/RSA API keys; quote-denominated orders and dry-run preview
- Weighted rate/account trading introspection endpoints
- HMAC request signing + idempotent order submission
- Scoped mass cancellation (per-instrument + per-account)
- WebSocket authentication upgrade & in-flight token renewal (Task 5.3.26)
- Standard HTTP rate-limit headers & Retry-After middleware (Task 5.3.27)
- Manual liquidation admin endpoint with dual control (Task 5.3.30)
- Interactive WebSocket Trading API with request-response framing (Task 5.3.31)
- REST batch order operations (submit/cancel, Task 5.3.32)
- Comprehensive RFC 7807 error envelopes, gateway circuit breaker & timeout fallbacks (Task 5.3.41)
- Unified list envelope, cross-endpoint idempotency, auth/session/key lifecycle & tabulated rate weights (Task 5.3.42)
- Server-time endpoint, unified venue-info document & transfer history query (Tasks 5.3.43–5.3.45)
- Client-surface route registration for §8.4/§12/§21 endpoints (Task 5.3.46)

---

### Task 5.3.46: Client-Surface Route Registration

**Objective:** Close the route-registry completeness gap for the entire client-facing HTTP surface (added 2026-09-27, remediation #35 — cluster architecture review found the §8.4/§12/§21 client routes had no registration owner; the registry-completeness invariant, spec §8.4 item 4, requires every endpoint declared in any phase plan to appear in the Task 5.3.7 registry).

**File Locations:** `services/internal/api/registry/client_routes.go`

**Implementation:**
1. Register every client endpoint declared in spec §12 (auth, 2FA, KYC, account, sessions, webauthn, notifications), §21.4–§21.11 (account profile/security/support/reports pages), and §8.4's table, each with role + scope + auth-method metadata.
2. Registration entries for endpoints implemented by Phase-12/14/20/24 carry `status: stub` until the owning phase lands; live behavior is delivered by the owning task, not this one.
3. Sub-account API-key scopes use the §8.8 vocabulary (`read`/`trade`/`transfer`; no `withdraw` scope — the money-moving scope is `transfer`; remediation #35 supersedes the prior "`no `withdraw`" wording in Task 5.3.11 step 7).
4. Dual-control metadata: `admin/withdrawals/{id}/approve|reject` (Finance Ops, per §8.1 four-eyes list and the Phase-11 Task 11.3.2 PENDING_REVIEW workflow) registered here even though the endpoint body lands in Phase-11.
5. Validation: a CI cross-check compares the registry against every `POST/GET/PUT/DELETE` path declared in any phase plan and fails on unregistered endpoints (extends Task 5.3.7 step 6).

**Definition of Done (Acceptance Criteria):**
* [x] All §12/§21 client endpoints registered with stub/live status
* [x] Registry-vs-plan path cross-check passes in CI
* [x] `admin/withdrawals/{id}/approve|reject` registered with dual-control metadata

**SDD Checklist:**
- [x] Spec checkpoint: centralized OpenAPI-schema request validation + route-registry completeness — defined first, validated against spec
- [x] Spec checkpoint: route registration for ALL endpoints (spec §8.4 conventions) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 5.5 Dependencies

- Phases 2–3, Phase 2.5

---

## 5.6 Duration Estimate

9–16 days (supersedes prior 9–14 — duration itemization completed 2026-09-27, remediation #35: Tasks 5.3.33–5.3.40 (8 tasks added by remediations #12–14) were omitted from the itemization, which already summed to 13.5 days against the 14-day header):
- Tasks 5.3.1–5.3.6 (core endpoints): 2 days
- Tasks 5.3.7–5.3.12 (route registry, docs, sessions, sub-accounts): 1.5 days
- Tasks 5.3.13–5.3.20 (test env, announcements, promos, portal, webhooks, chargebacks, tax, deprecation): 2 days
- Task 5.3.21 (error registry): 0.5 day
- Task 5.3.22 (order-modify audit + STALE_MODIFY): 0.5 day
- Task 5.3.23 (internal transfers): 0.5 day
- Task 5.3.24 (HMAC + idempotency): 0.5 day
- Task 5.3.25 (scoped mass cancel): 0.5 day
- Task 5.3.26 (WS auth upgrade & renewal): 0.5 day
- Task 5.3.27 (rate limit headers & retry-after): 0.5 day
- Task 5.3.30 (manual liquidation admin): 0.5 day
- Tasks 5.3.31–5.3.32 (WS trading API & batch orders): 1 day
- Tasks 5.3.33–5.3.40 (dead-man, IP bans, filters, close-all, cancel-replace, Ed25519, quote/preview, introspection): 2.5 days (added to itemization, remediation #35)
- Task 5.3.41 (Error mapping & envelopes): 0.5 day
- Task 5.3.42 (API hardening: envelope, idempotency, auth lifecycle, weights): 1 day
- Task 5.3.43 (Server-time endpoint): 0.5 day
- Task 5.3.44 (Unified venue-info document): 0.5 day
- Task 5.3.45 (Transfer history query): 0.5 day
- Task 5.3.46 (client-surface route registration): 0.5 day (added 2026-09-27, remediation #35)
- Testing: 1 day

---

## 5.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | JWT access token valid 15min; refresh produces new token |
| 2 | OAuth2 client credentials grant works |
| 3 | TOTP 2FA required for withdrawals and balance adjustments |
| 4 | 5 rate limit tiers with correct limits and burst |
| 5 | Throttled mode reduces lower tiers first |
| 6 | Rate limit headers present on all responses |
| 7 | Order submission reaches C++ core via Aeron |
| 8 | Cancel reaches C++ core; confirmation returned |
| 9 | Order history paginated correctly |
| 10 | Balances endpoint returns all currencies with available/locked/total |
| 11 | Positions endpoint returns open positions with unrealized P&L |
| 12 | Risk limits endpoint returns limits + utilization |
| 13 | L2 snapshot returns top N levels with seq |
| 14 | Ticker returns 24h OHLCV |
| 15 | Klines returns OHLCV candles |
| 16 | Deposit instructions return bank details per currency |
| 17 | Withdrawal creates with 15min confirmation window |
| 18 | Withdrawal confirm validates token within window |
| 19 | All Phase 5 routes registered; future stubs return 501 |
| 20 | GET /api/v1/routes returns full route list |
| 21 | OpenAPI 3.1 spec generated; Swagger UI at /developer |
| 22 | API token IP allowlist rejects non-matching IP |
| 23 | Concurrent session limits enforced |
| 24 | Sub-account tier limits enforced (default 20 retail, up to 100 corporate, up to 1,000 institutional; supersedes prior flat 20); independent trading; master aggregation |
| 25 | FROZEN account cannot trade or withdraw; dual control freeze/unfreeze |
| 26 | Test environment reset works in non-production |
| 27 | Announcements list and maintenance schedule visible |
| 28 | Fee promo rates applied while promo_until > now() |
| 29 | Developer portal with API key CRUD and rate tier selection |
| 30 | Webhooks delivered with HMAC-SHA256 signature; retry with backoff |
| 31 | Chargeback dispute workflow with account freeze |
| 32 | Tax report with FIFO lot tracking; CSV and PDF export |
| 33 | Sunset and Deprecation headers on deprecated endpoints |
| 34 | All error codes from spec §23 registered |
| 35 | Error response includes code, HTTP status, description |
| 36 | `order_audit` table created with old/new field values |
| 37 | Every order modify writes audit entry with old and new values |
| 38 | Audited fields: price, quantity, time_in_force, stop_price, iceberg_visible_qty |
| 39 | STALE_MODIFY: stale `order_seq` on modify rejected with HTTP 409 |
| 40 | Audit trail queryable via admin endpoint (Compliance Officer+) |
| 41 | `GET /api/v1/instruments` returns tradable universe with tick/lot/min-notional/status/hours (§24 #156) |
| 42 | `PUT /api/v1/orders/{id}` modifies order; price/qty-up loses queue priority |
| 43 | `POST /api/v1/transfers` moves balance atomically between caller-owned accounts with balanced GL entry (§24 #144) |
| 44 | Cross-user and FROZEN/SUSPENDED transfers rejected |
| 45 | HMAC-signed request authenticates; bad signature → INVALID_SIGNATURE (§24 #147) |
| 46 | Timestamp outside 30s window → TIMESTAMP_OUT_OF_WINDOW; replayed signature rejected |
| 47 | Duplicate `client_order_id` returns original ack; no duplicate order or fills (§24 #148) |
| 48 | `DELETE /api/v1/orders?symbol=` cancels only that instrument's orders; cancel-all returns per-symbol breakdown (§24 #153) |
| 49 | WebSocket connection supports in-flight authentication upgrade (`{"action":"authenticate"}`) without reconnecting and seamless token renewal before expiry (§24 #187) |
| 50 | All REST API responses emit standard rate limit headers (`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After` on 429) conforming to RFC 6585 (§24 #192) — header naming amended 2026-09-19 to `X-RateLimit-*` per §8.3/§24 #192 (remediation #9 supersedes prior prefix-less `RateLimit-*` wording) |
| 51 | API versioning middleware routes v1/v2, returns X-API-Version and Sunset headers; WS rejects unknown versions (§24 #208) |
| 52 | Mass cancel supports scoped filters: account_id, instrument_id, side, order_type; admin mass-cancel across accounts with audit log |
| 53 | API gateway: HAProxy L7 routing with health-check failover, TLS termination, blue-green deploy support (§24 #225) |
| 54 | `POST /api/v1/admin/liquidation/manual` endpoint: Risk Manager+ RBAC, dual control, WAL MANUAL_LIQUIDATION event, audit hash chain (§24 #239) |
| 55 | Sub-account hierarchy supports tiered ceilings (default 20 retail, up to 100 corporate, up to 1,000 institutional via admin endpoint); programmatic sub-account API key provisioning (§24 #256) |
| 56 | Interactive WebSocket Trading API handles `order.place`, `order.cancel`, `order.modify`, `order.batch`, `order.status` via persistent `/ws/v1` session with correlated request/response IDs (§24 #253) |
| 57 | `POST /api/v1/orders/batch` and `DELETE /api/v1/orders/batch` process up to 10 orders per submission and 20 per cancellation with atomic index-mapped result array (§24 #254) |
| 58 | REST/WS/FIX dead-man switch shares one account timer and atomically cancels on expiry (§24 #257) |
| 59 | Repeated post-429 abuse escalates through timed HTTP 418 bans with admin audit (§24 #258) |
| 60 | Instruments expose structured price/lot/notional/order-count/spread filters (§24 #259) |
| 61 | Close-all uses reduce-only slippage-protected orders and reports partial failure (§24 #260) |
| 62 | Cancel-replace exposes STOP_ON_FAILURE/ALLOW_FAILURE outcomes; keep-priority amendment is quantity-down only with client history (§24 #281–282) |
| 63 | REST/WS accept Ed25519 and RSA signatures; FIX requires Ed25519; asymmetric private keys never enter the platform (§24 #283) |
| 64 | Quote-denominated market orders and side-effect-free order previews respect all filters and disclose costs (§24 #286) |
| 65 | Clients can query weighted multi-interval rate usage, effective filters/commissions, prevented matches, amendments, and SOR allocations (§24 #288) |
| 66 | RFC 7807 error envelopes returned across all endpoints with request_id correlation, circuit breaker tripping, and 504 on engine timeout (§24 #304) |
| 67 | Unified `{data, next_cursor, limit, total}` envelope on all list endpoints; `Idempotency-Key` safe-retry on money-moving POSTs and WS order actions; auth/session/key lifecycle enforced on REST/WS/FIX; per-route weights tabulated with REQUEST_WEIGHT_EXCEEDED (§24 #338) |
| 68 | Public PTP-sourced `GET /api/v1/time` for HMAC clock sync (§24 #355) |
| 69 | Unified `GET /api/v1/exchange-info` with per-symbol filters/permissions, venue rate limits and ETag refresh signaling (§24 #356) |
| 70 | Paginated `GET /api/v1/transfers` with GL linkage and actor identity (§24 #363) |
| 71 | Client-surface route registration: every §8.4/§12/§21 client endpoint (`/auth/*`, `/kyc/*`, `/account/*`, `/support/tickets`, `/developer/*`) registered with role + scope + auth-method metadata in the Task 5.3.7 registry — registry-completeness invariant (remediation #35) |
