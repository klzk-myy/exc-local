// Seed route table — every endpoint declared by Phase-05 tasks and the
// cross-phase routes Task 5.3.7 step 7 pins (fleet, ops-console, auditor),
// plus the client-surface and §8.4 rows whose owning tasks are already
// pinned in the phase docs.
//
// Status=Stub until the owning task lands the real handler — the
// registration-completeness invariant (spec §8.4 item 4, Task 5.3.7 step 6)
// requires the route present with correct metadata, not a live handler.
// Owning agents register their live handler by re-declaring the Route with
// Status=Live (or editing their row here); the CI cross-check compares this
// table against every path declared in the phase plans.
package gateway

import (
	"net/http"

	"exchange/internal/errs"
)

// Common auth declarations.
var (
	authPublic   = AuthSpec{Required: false}
	authUser     = AuthSpec{Required: true, Methods: []string{AuthJWT, AuthHMAC, AuthOAuth2}}
	authRead     = AuthSpec{Required: true, Scopes: []string{ScopeRead}}
	authTrade    = AuthSpec{Required: true, Scopes: []string{ScopeTrade}}
	authTransfer = AuthSpec{Required: true, Scopes: []string{ScopeTransfer}}
)

func adminAuth(role string) AuthSpec {
	return AuthSpec{Required: true, Role: role}
}

// v1 stamps a versioned /api/v1 route.
func v1(method, path, tier, owner, desc string, auth AuthSpec) Route {
	return Route{Method: method, Path: path, Version: "v1", Auth: auth,
		RateTier: tier, Weight: 1, Owner: owner, Status: StatusStub, Description: desc}
}

// v1live stamps a versioned /api/v1 route whose owning task has landed
// the handler (Status=Live).
func v1live(method, path, tier, owner, desc string, auth AuthSpec) Route {
	rt := v1(method, path, tier, owner, desc, auth)
	rt.Status = StatusLive
	return rt
}

// v1liveDC stamps a live /api/v1 route that executes through the §8.2
// four-eyes queue (DualControl metadata flag — surfaced by the route
// dump/OpenAPI generator).
func v1liveDC(method, path, tier, owner, desc string, auth AuthSpec) Route {
	rt := v1live(method, path, tier, owner, desc, auth)
	rt.DualControl = true
	return rt
}

// SeedRoutes returns the full Phase-05-era route table. Meta endpoints
// owned by this cluster are Status=Live and mounted by MountSeed.
func SeedRoutes() []Route {
	r := []Route{
		// ---- Meta (live, owned by this cluster) ----
		{Method: "GET", Path: "/api/v1/routes", Version: "v1", Auth: adminAuth(RoleSuperAdmin),
			RateTier: TierExempt, Weight: 1, Owner: "Phase-05 Task 5.3.7", Status: StatusLive,
			Description: "Dump the registered route table (admin only)"},
		{Method: "GET", Path: "/api/v1/errors", Version: "v1", Auth: adminAuth(RoleSuperAdmin),
			RateTier: TierExempt, Weight: 1, Owner: "Phase-05 Task 5.3.21", Status: StatusLive,
			Description: "Dump the error-code registry (admin only)"},
		{Method: "GET", Path: "/api/v1/openapi.json", Version: "v1", Auth: authPublic,
			RateTier: TierExempt, Weight: 1, Owner: "Phase-05 Task 5.3.8", Status: StatusLive,
			Description: "OpenAPI 3.1 document generated from the route registry"},

		// ---- Task 5.3.3 Order endpoints ----
		v1live(http.MethodPost, "/api/v1/orders", TierBasic, "Phase-05 Task 5.3.3",
			"Submit order (idempotent on client_order_id; → Aeron → C++ core)", authTrade),
		v1live(http.MethodPut, "/api/v1/orders/{id}", TierBasic, "Phase-05 Task 5.3.3 / Task 5.3.22",
			"Modify order (price/qty/tif; STALE_MODIFY on stale seq)", authTrade),
		v1live(http.MethodDelete, "/api/v1/orders/{id}", TierBasic, "Phase-05 Task 5.3.3",
			"Cancel order", authTrade),
		v1live(http.MethodDelete, "/api/v1/orders/all", TierBasic, "Phase-05 Task 5.3.3 / Task 5.3.25",
			"Cancel all orders (per account, per-symbol breakdown)", authTrade),
		v1live(http.MethodDelete, "/api/v1/orders", TierBasic, "Phase-05 Task 5.3.25",
			"Mass cancel per instrument: ?symbol={symbol}", authTrade),
		v1live(http.MethodGet, "/api/v1/orders", TierBasic, "Phase-05 Task 5.3.3",
			"Order history (cursor paginated)", authRead),
		v1live(http.MethodGet, "/api/v1/orders/{id}", TierBasic, "Phase-05 Task 5.3.3",
			"Order detail", authRead),

		// ---- Task 5.3.32 batch ops ----
		v1live(http.MethodPost, "/api/v1/orders/batch", TierBasic, "Phase-05 Task 5.3.32",
			"Batch submit up to 10 orders (index-mapped results)", authTrade),
		v1live(http.MethodDelete, "/api/v1/orders/batch", TierBasic, "Phase-05 Task 5.3.32",
			"Batch cancel up to 20 orders", authTrade),

		// ---- Task 5.3.33 dead-man switch ----
		v1(http.MethodPost, "/api/v1/orders/countdown-cancel-all", TierBasic, "Phase-05 Task 5.3.33",
			"Dead-man countdown cancel-all (countdown_ms 1000–300000, 0 disables)", authTrade),

		// ---- Task 5.3.36 close-all ----
		v1(http.MethodPost, "/api/v1/positions/close-all", TierBasic, "Phase-05 Task 5.3.36",
			"Close all positions (optional symbol/side filters; X-2FA-Token header)", authTrade),

		// ---- Task 5.3.37 atomic cancel-replace / keep-priority ----
		v1live(http.MethodPost, "/api/v1/orders/{id}/cancel-replace", TierBasic, "Phase-05 Task 5.3.37",
			"Atomic cancel-replace (mode=STOP_ON_FAILURE|ALLOW_FAILURE)", authTrade),
		v1live(http.MethodPut, "/api/v1/orders/{id}/amend/keep-priority", TierBasic, "Phase-05 Task 5.3.37",
			"Quantity-down keep-priority amendment", authTrade),
		v1live(http.MethodGet, "/api/v1/orders/{id}/amendments", TierBasic, "Phase-05 Task 5.3.37",
			"Client-visible amendment history", authRead),

		// ---- Task 5.3.39 dry-run preview ----
		v1live(http.MethodPost, "/api/v1/orders/test", TierBasic, "Phase-05 Task 5.3.39",
			"Side-effect-free order validation/preview (margin, filters, commission)", authTrade),

		// ---- Task 5.3.4 account & balance (handlers: internal/api/account.go) ----
		v1live(http.MethodGet, "/api/v1/account/balances", TierBasic, "Phase-05 Task 5.3.4",
			"All currency balances (available/locked/total)", authRead),
		v1live(http.MethodGet, "/api/v1/positions", TierBasic, "Phase-05 Task 5.3.4",
			"Open positions with unrealized P&L", authRead),
		v1live(http.MethodGet, "/api/v1/account/risk-limits", TierBasic, "Phase-05 Task 5.3.4",
			"Current risk limits + utilization", authRead),

		// ---- Task 5.3.5 market data REST (public) — wave-2 cluster C ----
		v1live(http.MethodGet, "/api/v1/book/{symbol}", TierPublic, "Phase-05 Task 5.3.5",
			"L2 order book snapshot (?depth=20, 100ms cache)", authPublic),
		v1live(http.MethodGet, "/api/v1/trades/{symbol}", TierPublic, "Phase-05 Task 5.3.5",
			"Recent trades (?limit=100, 1s cache)", authPublic),
		v1live(http.MethodGet, "/api/v1/ticker/{symbol}", TierPublic, "Phase-05 Task 5.3.5",
			"24h ticker (1s cache)", authPublic),
		v1live(http.MethodGet, "/api/v1/klines/{symbol}", TierPublic, "Phase-05 Task 5.3.5",
			"OHLCV candles (?interval=1m&limit=500, 1s cache)", authPublic),
		v1live(http.MethodGet, "/api/v1/instruments", TierPublic, "Phase-05 Task 5.3.5",
			"Instrument reference data: tick/lot/min-notional/status/hours/filters", authPublic),

		// ---- Task 5.3.6 funding (handlers: internal/api/funding.go →
		//      internal/funding; Phase-11 owns rail dispatch of CONFIRMED
		//      withdrawals + beneficiary registry) ----
		v1live(http.MethodGet, "/api/v1/deposits/{currency}", TierBasic, "Phase-05 Task 5.3.6 / Phase-11 Task 11.3.1",
			"Deposit instructions (bank details per currency)", authRead),
		v1live(http.MethodPost, "/api/v1/withdrawals", TierBasic, "Phase-05 Task 5.3.6 / Phase-11 Task 11.3.2",
			"Create withdrawal (15min confirmation window)", authTransfer),
		v1live(http.MethodPost, "/api/v1/withdrawals/{id}/confirm", TierBasic, "Phase-05 Task 5.3.6 / Phase-11 Task 11.3.2",
			"Confirm withdrawal within the 15min window", authTransfer),
		v1live(http.MethodGet, "/api/v1/funding", TierBasic, "Phase-05 Task 5.3.6 / Phase-11 Task 11.3.3",
			"Funding history (paginated)", authRead),

		// ---- Task 5.3.10 sessions (live surface: Phase-12 Task 12.3.9) ----
		v1live(http.MethodGet, "/api/v1/account/sessions", TierBasic, "Phase-05 Task 5.3.10 / Phase-12 Task 12.3.9",
			"List active sessions", authRead),
		v1live(http.MethodDelete, "/api/v1/account/sessions/{id}", TierBasic, "Phase-05 Task 5.3.10 / Phase-12 Task 12.3.9",
			"Revoke session", authUser),
		v1live(http.MethodDelete, "/api/v1/account/sessions", TierBasic, "Phase-12 Task 12.3.9",
			"Revoke all sessions except current", authUser),

		// ---- Task 5.3.11 sub-accounts ----
		v1(http.MethodGet, "/api/v1/account/sub-accounts", TierBasic, "Phase-05 Task 5.3.11",
			"List sub-accounts with balances/permissions", authRead),
		v1(http.MethodPost, "/api/v1/account/sub-accounts", TierBasic, "Phase-05 Task 5.3.11",
			"Create sub-account (enforces tiered max_sub_accounts)", authUser),
		v1(http.MethodPut, "/api/v1/admin/accounts/{id}/sub-account-limit", TierBasic, "Phase-05 Task 5.3.11",
			"Adjust sub-account ceiling (up to 1,000 institutional)", adminAuth(RoleRiskManager)),
		v1(http.MethodPost, "/api/v1/account/sub-accounts/{id}/api-keys", TierBasic, "Phase-05 Task 5.3.11",
			"Provision scoped sub-account API key (read/trade)", authUser),

		// ---- Task 5.3.12 FROZEN legal hold (dual control) ----
		{Method: http.MethodPost, Path: "/api/v1/admin/accounts/{id}/freeze", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-05 Task 5.3.12", Status: StatusStub, DualControl: true,
			Description: "Freeze account — no trading/withdrawals (dual control)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/accounts/{id}/unfreeze", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-05 Task 5.3.12", Status: StatusStub, DualControl: true,
			Description: "Release frozen account (dual control)"},

		// ---- Task 14.3.9 forced closure (dual-control queue) ----
		{Method: http.MethodPost, Path: "/api/v1/admin/accounts/{id}/close", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.9", Status: StatusLive, DualControl: true,
			Description: "Forced account closure maker — mandatory reason; approval executes the offboarding pipeline"},

		// ---- Task 14.3.10 compliance holds ----
		{Method: http.MethodPost, Path: "/api/v1/admin/compliance/holds", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.10", Status: StatusLive,
			Description: "Place compliance hold — freezes account, cancels resting orders, positions preserved"},
		{Method: http.MethodGet, Path: "/api/v1/admin/compliance/holds", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.10", Status: StatusLive,
			Description: "Officer review — holds with reason/evidence/SLA timeline (?status=&limit=)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/compliance/holds/{id}/release", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.10", Status: StatusLive, DualControl: true,
			Description: "Release hold — restores ACTIVE when no other OPEN hold stands ({approver_id, reason})"},
		{Method: http.MethodPost, Path: "/api/v1/admin/compliance/holds/{id}/escalate", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.10", Status: StatusLive,
			Description: "Escalate hold — disposition 'sar' records escalation (Phase-21 files SAR); 'closure' submits forced-closure dual control"},

		// ---- Task 14.3.12 webhook dead-letter admin ----
		{Method: http.MethodGet, Path: "/api/v1/admin/webhooks/dead-letters", Version: "v1",
			Auth: adminAuth(RoleSupportAgent), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.12", Status: StatusLive,
			Description: "Dead-lettered webhook deliveries review (?limit=)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/webhooks/dead-letters/{id}/retransmit", Version: "v1",
			Auth: adminAuth(RoleSupportAgent), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.12", Status: StatusLive,
			Description: "Manual retransmit — requeues delivery PENDING with fresh attempt budget (audited)"},

		// ---- Task 5.3.13 test environment (non-prod only) ----
		{Method: http.MethodPost, Path: "/api/v1/test/reset", Version: "v1",
			Auth: authUser, RateTier: TierBasic, Weight: 1, Owner: "Phase-05 Task 5.3.13",
			Status: StatusLive, Env: "nonprod",
			Description: "Reset test account balances/orders/positions (1 per 5min)"},
		{Method: http.MethodPost, Path: "/api/v1/test/seed", Version: "v1",
			Auth: authUser, RateTier: TierBasic, Weight: 1, Owner: "Phase-14 Task 14.3.3",
			Status: StatusLive, Env: "nonprod",
			Description: "Apply named testnet balance preset (default standard)"},
		{Method: http.MethodPost, Path: "/api/v1/test/reset-seed", Version: "v1",
			Auth: authUser, RateTier: TierBasic, Weight: 1, Owner: "Phase-14 Task 14.3.3",
			Status: StatusLive, Env: "nonprod",
			Description: "Reset + seed preset in one 5min cooldown slot"},
		{Method: http.MethodPost, Path: "/api/v1/test/funding/deposit", Version: "v1",
			Auth: authUser, RateTier: TierBasic, Weight: 1, Owner: "Phase-14 Task 14.3.3",
			Status: StatusLive, Env: "nonprod",
			Description: "Simulated faucet credit — no banking rails"},
		{Method: http.MethodPost, Path: "/api/v1/test/funding/withdrawal", Version: "v1",
			Auth: authUser, RateTier: TierBasic, Weight: 1, Owner: "Phase-14 Task 14.3.3",
			Status: StatusLive, Env: "nonprod",
			Description: "Simulated debit — INSUFFICIENT_BALANCE shape, no rails"},

		// ---- Task 5.3.14 announcements & maintenance calendar ----
		v1live(http.MethodGet, "/api/v1/announcements", TierPublic, "Phase-05 Task 5.3.14",
			"List live announcements (?category=&limit=)", authPublic),
		v1live(http.MethodGet, "/api/v1/announcements/{id}", TierPublic, "Phase-05 Task 5.3.14",
			"Single announcement (live only; drafts 404)", authPublic),
		v1live(http.MethodPost, "/api/v1/admin/announcements", TierBasic, "Phase-05 Task 5.3.14",
			"Create announcement", adminAuth(RoleSupportAgent)),
		v1live(http.MethodGet, "/api/v1/admin/announcements", TierBasic, "Phase-05 Task 5.3.14",
			"List all announcements incl. drafts/retracted", adminAuth(RoleSupportAgent)),
		v1live(http.MethodPatch, "/api/v1/admin/announcements/{id}", TierBasic, "Phase-05 Task 5.3.14",
			"Update announcement (partial)", adminAuth(RoleSupportAgent)),
		v1live(http.MethodDelete, "/api/v1/admin/announcements/{id}", TierBasic, "Phase-05 Task 5.3.14",
			"Retract announcement (status transition, never delete)", adminAuth(RoleSupportAgent)),
		v1live(http.MethodGet, "/api/v1/maintenance/schedule", TierPublic, "Phase-05 Task 5.3.14",
			"Upcoming maintenance windows", authPublic),
		v1live(http.MethodGet, "/api/v1/admin/maintenance-windows", TierBasic, "Phase-05 Task 5.3.14",
			"All maintenance windows (admin)", adminAuth(RoleSupportAgent)),
		v1live(http.MethodPost, "/api/v1/admin/maintenance-windows", TierBasic, "Phase-05 Task 5.3.14",
			"Schedule maintenance window", adminAuth(RoleSupportAgent)),
		v1live(http.MethodPatch, "/api/v1/admin/maintenance-windows/{id}", TierBasic, "Phase-05 Task 5.3.14",
			"Update maintenance window (partial)", adminAuth(RoleSupportAgent)),
		v1live(http.MethodDelete, "/api/v1/admin/maintenance-windows/{id}", TierBasic, "Phase-05 Task 5.3.14",
			"Cancel maintenance window", adminAuth(RoleSupportAgent)),

		// ---- Task 5.3.15 fees ----
		v1live(http.MethodGet, "/api/v1/fees", TierBasic, "Phase-05 Task 5.3.15",
			"Current fee rates incl. active promos", authRead),
		v1live(http.MethodPost, "/api/v1/admin/fees/promo", TierBasic, "Phase-05 Task 5.3.15",
			"Create fee promo window (PENDING_APPROVAL; four-eyes apply)", adminAuth(RoleFinanceOps)),
		v1live(http.MethodGet, "/api/v1/admin/fees/promos", TierBasic, "Phase-05 Task 5.3.15",
			"List fee promo windows (?status=)", adminAuth(RoleFinanceOps)),
		{Method: http.MethodPost, Path: "/api/v1/admin/fees/promo/{id}/approve", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-05 Task 5.3.15", Status: StatusLive, DualControl: true,
			Description: "Approve promo window → applies promo_* to fee_tiers (four-eyes, 15m window)"},
		v1live(http.MethodPost, "/api/v1/admin/fees/promo/{id}/reject", TierBasic, "Phase-05 Task 5.3.15",
			"Reject a pending promo window", adminAuth(RoleFinanceOps)),

		// ---- Task 5.3.16 developer portal ----
		{Method: http.MethodGet, Path: "/developer", Version: "",
			Auth: authPublic, RateTier: TierPublic, Weight: 1,
			Owner: "Phase-05 Task 5.3.16 / Task 5.3.8", Status: StatusLive,
			Description: "Swagger UI developer portal"},
		{Method: http.MethodGet, Path: "/developer/migration", Version: "",
			Auth: authPublic, RateTier: TierPublic, Weight: 1,
			Owner: "Phase-05 Task 5.3.20", Status: StatusLive,
			Description: "API deprecation migration guide"},
		v1live(http.MethodPost, "/api/v1/developer/api-keys", TierBasic, "Phase-05 Task 5.3.16",
			"Create API key — HMAC (secret shown once) | ED25519/RSA (public key only); migration 025+073", authUser),
		v1live(http.MethodGet, "/api/v1/developer/api-keys", TierBasic, "Phase-05 Task 5.3.16",
			"List API keys", authRead),
		v1live(http.MethodDelete, "/api/v1/developer/api-keys/{id}", TierBasic, "Phase-05 Task 5.3.16",
			"Revoke API key", authUser),

		// ---- Tasks 5.3.17–5.3.19 ----
		v1live(http.MethodPost, "/api/v1/webhooks", TierBasic, "Phase-05 Task 5.3.17",
			"Register webhook URL + events (HMAC-SHA256 signed delivery)", authUser),
		v1live(http.MethodGet, "/api/v1/webhooks", TierBasic, "Phase-05 Task 5.3.17",
			"List registered webhook endpoints", authRead),
		v1live(http.MethodDelete, "/api/v1/webhooks/{id}", TierBasic, "Phase-05 Task 5.3.17",
			"Disable a webhook endpoint", authUser),
		v1live(http.MethodPost, "/api/v1/webhooks/{id}/rotate-secret", TierBasic, "Phase-05 Task 5.3.17",
			"Rotate signing secret (overlap ≤72h; prev signs via X-Webhook-Signature-Prev)", authUser),
		v1live(http.MethodGet, "/api/v1/webhooks/{id}/deliveries", TierBasic, "Phase-05 Task 5.3.17",
			"Delivery log incl. DEAD_LETTERED queue view", authRead),
		// Task 5.3.18 chargeback lifecycle (handlers:
		// internal/api/chargebacks.go → internal/funding). The four workflow
		// rows are appended — the lone create route cannot satisfy the
		// dispute-lifecycle DoD (opened → evidence → submitted → resolved).
		v1live(http.MethodPost, "/api/v1/admin/chargebacks", TierBasic, "Phase-05 Task 5.3.18",
			"Create chargeback record (dispute workflow)", adminAuth(RoleFinanceOps)),
		v1live(http.MethodGet, "/api/v1/admin/chargebacks", TierBasic, "Phase-05 Task 5.3.18",
			"Chargeback journal — cursor-paged dispute list", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodGet, "/api/v1/admin/chargebacks/{id}", TierBasic, "Phase-05 Task 5.3.18",
			"Chargeback detail — dispute + hashed evidence bundle", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodPost, "/api/v1/admin/chargebacks/{id}/submit", TierBasic, "Phase-05 Task 5.3.18",
			"Chargeback submit — EVIDENCE_COLLECTED → SUBMITTED", adminAuth(RoleFinanceOps)),
		v1live(http.MethodPost, "/api/v1/admin/chargebacks/{id}/resolve", TierBasic, "Phase-05 Task 5.3.18",
			"Chargeback resolve — SUBMITTED → RESOLVED_WON|LOST", adminAuth(RoleFinanceOps)),
		v1live(http.MethodGet, "/api/v1/tax/report", TierBasic, "Phase-05 Task 5.3.19",
			"Tax report with FIFO lot tracking (?year=&format=json|csv|pdf)", authRead),
		v1live(http.MethodGet, "/api/v1/account/tax-report", TierBasic,
			"Phase-05 Task 5.3.19 / Phase-20 Task 20.3.10",
			"Canonical tax-report path (§21: supersedes /tax/report; same producer; ?year=&method=&format=)", authRead),

		// ---- Task 5.3.20 deprecation policy administration ----
		v1live(http.MethodPost, "/api/v1/admin/api-deprecations", TierBasic, "Phase-05 Task 5.3.20",
			"Announce endpoint deprecation (sunset ≥ 6 months out; enforced)", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/api-deprecations", TierBasic, "Phase-05 Task 5.3.20",
			"List deprecation rules", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodGet, "/api/v1/admin/api-deprecations/usage", TierBasic, "Phase-09 Task 9.3.6",
			"Deprecated-route usage telemetry (per-rule daily counters)", adminAuth(RoleReadOnlyAuditor)),

		// ---- Task 5.3.22 order-modify audit ----
		v1live(http.MethodGet, "/api/v1/admin/orders/{id}/audit", TierBasic, "Phase-05 Task 5.3.22",
			"Order-modify audit trail (old/new field values)", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodPost, "/api/v1/admin/orders/mass-cancel", TierBasic, "Phase-05 Task 5.3.25",
			"Cross-account mass cancel (account_id optional; symbol/side/type scopes)", adminAuth(RoleRiskManager)),

		// ---- Task 5.3.23 internal transfers (handler: internal/api/transfers.go) ----
		v1live(http.MethodPost, "/api/v1/transfers", TierBasic, "Phase-05 Task 5.3.23",
			"Internal transfer (master↔sub / same-user) with GL posting", authTransfer),

		// ---- Tasks 5.3.26/5.3.31/§8.4 WebSocket surface ----
		{Method: "WS", Path: "/ws/v1", Version: "v1", Auth: authPublic,
			RateTier: TierPublic, Weight: 1, Owner: "Phase-05 Task 5.3.26 / Task 5.3.31",
			Status:      StatusLive,
			Description: "Unified interactive WebSocket: market data, private feeds, order.* request-response"},
		{Method: "WS", Path: "/ws/v1/marketdata", Version: "v1", Auth: authPublic,
			RateTier: TierPublic, Weight: 1, Owner: "Phase-05 Task 5.3.26 (legacy alias)",
			Status: StatusStub, Description: "Market data stream (legacy alias of /ws/v1)"},
		{Method: "WS", Path: "/ws/v1/orders", Version: "v1", Auth: authUser,
			RateTier: TierBasic, Weight: 1, Owner: "Phase-05 Task 5.3.26 (legacy alias)",
			Status: StatusStub, Description: "Private order stream (legacy alias of /ws/v1)"},
		{Method: "WS", Path: "/ws/market", Version: "", Auth: authPublic,
			RateTier: TierPublic, Weight: 1, Owner: "Phase-06 Task 6.3.1",
			Status: StatusStub, Description: "Public market-data WebSocket (spec §8.6/§10.5; alias surface of /ws/v1)"},
		{Method: "WS", Path: "/ws/trade", Version: "", Auth: authUser,
			RateTier: TierBasic, Weight: 1, Owner: "Phase-05 Task 5.3.31",
			Status: StatusStub, Description: "Interactive trading WebSocket (spec §8.6/§10.5; legacy alias surface of /ws/v1)"},
		{Method: "WS", Path: "/ws/stream", Version: "", Auth: authPublic,
			RateTier: TierPublic, Weight: 1, Owner: "Phase-06 Task 6.3.16",
			Status:      StatusStub,
			Description: "Combined stream path (?streams=a@x,b@y; subscription multiplexing)"},
		{Method: "WS", Path: "/ws/v1/l3/{symbol}", Version: "v1", Auth: authUser,
			RateTier: TierProfessional, Weight: 1, Owner: "Phase-17",
			Status: StatusStub, Description: "L3 order-level data stream (authenticated, premium tier)"},

		// ---- Task 5.3.29 gateway health probes (R9 schema live via
		//      internal/api/health.go) ----
		{Method: http.MethodGet, Path: "/health/live", Version: "", Auth: authPublic,
			RateTier: TierExempt, Weight: 1, Owner: "Phase-05 Task 5.3.29", Status: StatusLive,
			Description: "Liveness probe (always 200 when process is up)"},
		{Method: http.MethodGet, Path: "/health/ready", Version: "", Auth: authPublic,
			RateTier: TierExempt, Weight: 1, Owner: "Phase-05 Task 5.3.29", Status: StatusLive,
			Description: "Readiness probe (PostgreSQL + Redis + NATS checks)"},
		{Method: http.MethodGet, Path: "/health", Version: "", Auth: authPublic,
			RateTier: TierExempt, Weight: 1, Owner: "Phase-07 Task 7.3.6", Status: StatusLive,
			Description: "Liveness alias of /health/live (admin-monitoring convention)"},
		{Method: http.MethodGet, Path: "/ready", Version: "", Auth: authPublic,
			RateTier: TierExempt, Weight: 1, Owner: "Phase-07 Task 7.3.6", Status: StatusLive,
			Description: "Readiness alias of /health/ready (admin-monitoring convention)"},

		// ---- Task 5.3.30 manual liquidation (dual control) ----
		{Method: http.MethodPost, Path: "/api/v1/admin/liquidation/manual", Version: "v1",
			Auth: adminAuth(RoleRiskManager), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-05 Task 5.3.30", Status: StatusLive, DualControl: true,
			Description: "Manual position liquidation (override_auction bypasses CALL)"},

		// ---- Task 5.3.42 hardening introspection (published contracts) ----
		v1live(http.MethodGet, "/api/v1/meta/rate-limits", TierPublic, "Phase-05 Task 5.3.42",
			"Published rate-limit contract: §8.3 tiers, route weights, ban schedule, WS caps", authPublic),
		v1live(http.MethodGet, "/api/v1/meta/pagination", TierPublic, "Phase-05 Task 5.3.42",
			"Published pagination contract: list envelope, per-endpoint limits/sort/filter matrix", authPublic),

		// ---- Task 5.3.40 introspection (handlers: internal/api/introspection.go) ----
		v1live(http.MethodGet, "/api/v1/account/rate-limits", TierBasic, "Phase-05 Task 5.3.40",
			"Effective rate limits + weighted usage", authRead),
		v1live(http.MethodGet, "/api/v1/account/filters/{symbol}", TierBasic, "Phase-05 Task 5.3.40",
			"Effective instrument filters for the account", authRead),
		v1live(http.MethodGet, "/api/v1/account/commission/{symbol}", TierBasic, "Phase-05 Task 5.3.40",
			"Effective commission for the account/symbol", authRead),

		// ---- Task 5.3.34 progressive IP-ban admin surface
		//      (handlers: internal/api/ipbans.go) ----
		v1live(http.MethodGet, "/api/v1/admin/ip-bans", TierBasic, "Phase-05 Task 5.3.34",
			"List active IP bans", adminAuth(RoleSupportAgent)),
		v1live(http.MethodGet, "/api/v1/admin/ip-bans/audit", TierBasic, "Phase-05 Task 5.3.34",
			"Ban/override audit trail", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodGet, "/api/v1/admin/ip-bans/{ip}", TierBasic, "Phase-05 Task 5.3.34",
			"Single IP-ban record", adminAuth(RoleSupportAgent)),
		v1live(http.MethodPut, "/api/v1/admin/ip-bans/{ip}", TierBasic, "Phase-05 Task 5.3.34",
			"Manual IP ban {duration_s, reason}", adminAuth(RoleRiskManager)),
		v1live(http.MethodDelete, "/api/v1/admin/ip-bans/{ip}", TierBasic, "Phase-05 Task 5.3.34",
			"Unban IP; ?pardon=1 also resets strikes", adminAuth(RoleRiskManager)),
		v1live(http.MethodPut, "/api/v1/admin/ip-allowlist/{ip}", TierBasic, "Phase-05 Task 5.3.34",
			"Exempt IP from the ban machinery", adminAuth(RoleRiskManager)),
		v1live(http.MethodDelete, "/api/v1/admin/ip-allowlist/{ip}", TierBasic, "Phase-05 Task 5.3.34",
			"Remove ban-machinery exemption", adminAuth(RoleRiskManager)),

		// ---- Tasks 5.3.43–5.3.45 ----
		v1live(http.MethodGet, "/api/v1/time", TierExempt, "Phase-05 Task 5.3.43",
			"PTP-sourced server time (UTC millis) for HMAC clock sync", authPublic),
		v1live(http.MethodGet, "/api/v1/exchange-info", TierPublic, "Phase-05 Task 5.3.44",
			"Unified venue info: symbols, filters, permissions, rate_limits", authPublic),
		v1live(http.MethodGet, "/api/v1/transfers", TierBasic, "Phase-05 Task 5.3.45",
			"Transfer history (cursor envelope, GL-linked)", authRead),

		// ---- Task 5.3.7 step 7: fleet / ops console / auditor ----
		//      (handlers landed: Phase-09 Task 9.3.30 → internal/api/
		//      handlers_fleet.go → internal/fleet, migration 091)
		v1live(http.MethodGet, "/api/v1/admin/fleet/environments", TierBasic, "Phase-09 Task 9.3.30",
			"Fleet environments (+ session env context)", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/fleet/hosts", TierBasic, "Phase-09 Task 9.3.30",
			"Fleet hosts (env-scoped; ?state=&role=)", adminAuth(RoleSuperAdmin)),
		{Method: http.MethodPost, Path: "/api/v1/admin/fleet/hosts/{id}/drain", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusLive, DualControl: true, Env: "env-scoped",
			Schema:      &BodySchema{Required: []string{"reason"}, Fields: map[string]string{"reason": "string", "approver_id": "any"}},
			Description: "Drain fleet host (prod: four-eyes approver_id)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/fleet/hosts/{id}/cordon", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusLive, DualControl: true, Env: "env-scoped",
			Schema:      &BodySchema{Required: []string{"reason"}, Fields: map[string]string{"reason": "string", "approver_id": "any"}},
			Description: "Cordon fleet host (prod: four-eyes approver_id)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/fleet/hosts/{id}/decommission", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusLive, DualControl: true, Env: "env-scoped",
			Schema:      &BodySchema{Required: []string{"reason"}, Fields: map[string]string{"reason": "string", "approver_id": "any"}},
			Description: "Decommission fleet host (prod: four-eyes approver_id)"},
		v1live(http.MethodGet, "/api/v1/admin/fleet/topology", TierBasic, "Phase-09 Task 9.3.30",
			"Fleet topology (?env=, scoped to session env)", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/releases", TierBasic, "Phase-09 Task 9.3.30",
			"List releases (env-scoped; ?status=)", adminAuth(RoleSuperAdmin)),
		{Method: http.MethodPost, Path: "/api/v1/admin/releases", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusLive,
			Schema: &BodySchema{Required: []string{"component", "version", "artifact_hash"},
				Fields: map[string]string{"component": "string", "version": "string",
					"artifact_hash": "string", "gate_evidence": "object", "notes": "string"}},
			Description: "Register release (lands DEPLOYED in dev — dev auto-deploys)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/releases/{id}/promote", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusLive, DualControl: true, Env: "env-scoped",
			Schema: &BodySchema{Required: []string{"to_env"},
				Fields: map[string]string{"to_env": "string", "reason": "string", "approver_id": "any"}},
			Description: "Promote release to an environment (direction-enforced gates; prod = four-eyes + §19.16.3 interlocks)"},
		v1live(http.MethodGet, "/api/v1/admin/listing-proposals", TierBasic, "Phase-15 Task 15.3.12",
			"List instrument listing proposals (?status=)", adminAuth(RoleRiskManager)),
		v1live(http.MethodPost, "/api/v1/admin/listing-proposals", TierBasic, "Phase-15 Task 15.3.12",
			"Create listing proposal {symbol, reference, oracle_feeds, risk_defaults, reason}", adminAuth(RoleRiskManager)),
		v1liveDC(http.MethodPost, "/api/v1/admin/listing-proposals/{id}/review", TierBasic, "Phase-15 Task 15.3.12",
			"Review listing proposal {action: REVIEW|APPROVE|REJECT} — APPROVE files OpInstrumentListing (202)", adminAuth(RoleRiskManager)),
		v1live(http.MethodGet, "/api/v1/admin/ops-board", TierBasic, "Phase-15 Task 15.3.12",
			"Market-ops console board (non-ACTIVE, proposals, approvals, auctions, fixings, warnings)", adminAuth(RoleRiskManager)),
		v1(http.MethodGet, "/api/v1/admin/archive/status", TierBasic, "Phase-04 Task 4.3.2",
			"WAL archive status (?shard=)", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodGet, "/api/v1/admin/audit/verify", TierBasic, "Phase-07 Task 7.3.3",
			"Audit hash-chain verification (?date=)", adminAuth(RoleReadOnlyAuditor)),

		// ---- §8.4 / client-surface rows with pinned owners (5.3.46 extends) ----
		v1live(http.MethodPost, "/api/v1/funding/bank-accounts", TierBasic, "Phase-11 Task 11.3.7",
			"Register beneficiary bank account (withdrawal allowlist)", authUser),
		v1live(http.MethodGet, "/api/v1/funding/bank-accounts", TierBasic, "Phase-11 Task 11.3.7",
			"List beneficiary bank accounts", authRead),
		v1live(http.MethodDelete, "/api/v1/funding/bank-accounts", TierBasic, "Phase-11 Task 11.3.7",
			"Remove beneficiary bank account (?id=)", authUser),
		v1live(http.MethodGet, "/api/v1/admin/funding/bank-accounts", TierBasic, "Phase-11 Task 11.3.7",
			"Beneficiary registry admin list (?status=&limit=)", adminAuth(RoleReadOnlyAuditor)),
		{Method: http.MethodPost, Path: "/api/v1/admin/funding/bank-accounts/{id}/verify", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.7", Status: StatusLive, DualControl: true,
			Description: "Verify beneficiary {approver_id, method} — bank-statement review / micro-deposit (four-eyes)"},
		v1live(http.MethodPost, "/api/v1/admin/funding/bank-accounts/{id}/reject", TierBasic, "Phase-11 Task 11.3.7",
			"Reject beneficiary {reason} — single approver (releases no funds)", adminAuth(RoleFinanceOps)),
		v1live(http.MethodPost, "/api/v1/funding/fee-estimate", TierBasic, "Phase-11 Task 11.3.9",
			"Funding fee estimate {amount, currency, rail, direction}", authRead),
		v1live(http.MethodPost, "/api/v1/funding/convert", TierBasic, "Phase-11 Task 11.3.9",
			"Indicative currency conversion {from_currency, to_currency?, amount, direction?} — mid±spread, persisted", authTransfer),
		v1live(http.MethodGet, "/api/v1/funding/conversions", TierBasic, "Phase-11 Task 11.3.9",
			"Account conversion history (?limit=100)", authRead),
		v1live(http.MethodGet, "/api/v1/admin/funding/fees", TierBasic, "Phase-11 Task 11.3.9",
			"Funding fee schedule list (?rail=&currency=&direction=&tier=&all=&limit=)", adminAuth(RoleFinanceOps)),
		v1live(http.MethodPost, "/api/v1/admin/funding/fees", TierBasic, "Phase-11 Task 11.3.9",
			"Create funding fee schedule version 1 {rail, currency, direction, account_tier, flat_fee, percentage_bps, min_fee, max_fee?, free_tier_monthly_count?, effective_date?}", adminAuth(RoleFinanceOps)),
		v1live(http.MethodGet, "/api/v1/admin/funding/fees/{id}", TierBasic, "Phase-11 Task 11.3.9",
			"Funding fee schedule row detail", adminAuth(RoleFinanceOps)),
		v1live(http.MethodPut, "/api/v1/admin/funding/fees/{id}", TierBasic, "Phase-11 Task 11.3.9",
			"Insert successor schedule version (supersedes {id}; fee parameters in body)", adminAuth(RoleFinanceOps)),
		v1live(http.MethodDelete, "/api/v1/admin/funding/fees/{id}", TierBasic, "Phase-11 Task 11.3.9",
			"Retire schedule row (leaves resolution, stays on audit record)", adminAuth(RoleFinanceOps)),
		v1live(http.MethodGet, "/api/v1/admin/funding/fees/{id}/versions", TierBasic, "Phase-11 Task 11.3.9",
			"Full version chain of the row's schedule group", adminAuth(RoleFinanceOps)),
		v1live(http.MethodGet, "/api/v1/funding/rails", TierBasic, "Phase-11 Task 11.3.1",
			"Banking rail capability matrix (currencies, cut-offs, settlement lag)", authRead),
		v1live(http.MethodPost, "/api/v1/funding/rail-selection", TierBasic, "Phase-11 Task 11.3.1",
			"Rail selection preview {currency, amount, preferred_rail, require_same_day}", authRead),
		v1live(http.MethodPost, "/api/v1/admin/funding/inbound-wires", TierBasic, "Phase-11 Task 11.3.11",
			"Ingest inbound bank credit — third-party deposit screen + quarantine", adminAuth(RoleFinanceOps)),
		v1live(http.MethodGet, "/api/v1/admin/funding/quarantine", TierBasic, "Phase-11 Task 11.3.11",
			"Suspense/quarantine journal (?status=&account_id=&cursor=&limit=)", adminAuth(RoleAnyAdmin)),
		{Method: http.MethodPost, Path: "/api/v1/admin/funding/quarantine/{id}/resolve", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.11", Status: StatusLive, DualControl: true,
			Description: "Resolve quarantined deposit {action: RELEASE_TO_CLIENT|RETURN_TO_SOURCE} (four-eyes)"},
		v1live(http.MethodPost, "/api/v1/admin/funding/returns", TierBasic, "Phase-11 Task 11.3.11",
			"Apply bank return/reject code {end_to_end_id, return_code} — pacs.004/MT199/R-code mapping", adminAuth(RoleFinanceOps)),
		// --- Phase-11 flows cluster (Tasks 11.3.2/11.3.3/11.3.6/11.3.10) ---
		v1live(http.MethodPost, "/api/v1/deposits", TierBasic, "Phase-11 Task 11.3.3",
			"Client deposit intent {currency, amount, reference?} — Idempotency-Key required", authUser),
		v1live(http.MethodPost, "/api/v1/admin/funding/deposits", TierBasic, "Phase-11 Task 11.3.3",
			"Ingest detected deposit (statement poll/webhook) {account_id, currency, amount, reference, source}", adminAuth(RoleFinanceOps)),
		v1live(http.MethodPost, "/api/v1/admin/funding/deposits/{id}/confirm", TierBasic, "Phase-11 Task 11.3.3",
			"Record independent bank-source confirmation {source} — 2 distinct sources resolve the tier", adminAuth(RoleFinanceOps)),
		{Method: http.MethodPost, Path: "/api/v1/admin/funding/deposits/{id}/review", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.3", Status: StatusLive, DualControl: true,
			Description: "Resolve PENDING_REVIEW deposit {action: APPROVE|REJECT, approver_id} (four-eyes)"},
		v1live(http.MethodGet, "/api/v1/funding/withdrawal-whitelist", TierBasic, "Phase-11 Task 11.3.10",
			"Whitelist mode + lock state + verified beneficiaries", authRead),
		v1live(http.MethodPost, "/api/v1/funding/withdrawal-whitelist/enable", TierBasic, "Phase-11 Task 11.3.10",
			"Enable WHITELIST_ONLY mode (re-enable rate-limited by timelock_until)", authUser),
		v1live(http.MethodPost, "/api/v1/funding/withdrawal-whitelist/disable", TierBasic, "Phase-11 Task 11.3.10",
			"Disable to ALLOW_ALL — latches account-scoped 24h egress lock", authUser),
		v1live(http.MethodGet, "/api/v1/admin/funding/nostro", TierBasic, "Phase-11 Task 11.3.6",
			"Nostro balances per currency + confirmed-withdrawal coverage", adminAuth(RoleFinanceOps)),
		v1live(http.MethodGet, "/api/v1/admin/funding/nostro/replenishments", TierBasic, "Phase-11 Task 11.3.6",
			"Replenishment requests (?limit=)", adminAuth(RoleFinanceOps)),
		v1live(http.MethodPost, "/api/v1/admin/funding/nostro/replenishments", TierBasic, "Phase-11 Task 11.3.6",
			"Reserve→operating nostro replenishment request {currency, amount, source_nostro_id, target_nostro_id}", adminAuth(RoleFinanceOps)),
		{Method: http.MethodPost, Path: "/api/v1/admin/funding/nostro/replenishments/{id}/decide", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.6", Status: StatusLive, DualControl: true,
			Description: "Approve→execute / reject replenishment (four-eyes)"},
		v1live(http.MethodGet, "/api/v1/admin/funding/ops-alerts", TierBasic, "Phase-11 Task 11.3.6",
			"Durable funding ops-alert trail (?limit=)", adminAuth(RoleFinanceOps)),
		// --- end Phase-11 flows cluster ---
		v1(http.MethodGet, "/api/v1/account/statements", TierBasic, "Phase-20 Task 20.3.6",
			"Client statements & trade confirmations (?period=)", authRead),
		{Method: http.MethodPost, Path: "/api/v1/admin/withdrawals/{id}/approve", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.2", Status: StatusLive, DualControl: true,
			Description: "Approve PENDING_REVIEW withdrawal (four-eyes)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/withdrawals/{id}/reject", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.2", Status: StatusLive, DualControl: true,
			Description: "Reject PENDING_REVIEW withdrawal (four-eyes)"},
		v1live(http.MethodGet, "/api/v1/support/tickets", TierBasic, "Phase-07 Task 7.3.7",
			"List support tickets", authUser),
		v1live(http.MethodPost, "/api/v1/support/tickets", TierBasic, "Phase-07 Task 7.3.7",
			"Open support ticket", authUser),
		v1live(http.MethodGet, "/api/v1/support/tickets/{id}", TierBasic, "Phase-07 Task 7.3.7",
			"Own ticket detail + public notes", authUser),
		v1live(http.MethodGet, "/api/v1/session/status", TierPublic, "Phase-15 Task 15.3.7",
			"24/5 session lifecycle status {state, next_transition_at, shard coverage}", authPublic),
		v1live(http.MethodGet, "/api/v1/stats/24h", TierPublic, "Phase-11 Task 11.3.5",
			"Venue-wide rolling 24h market statistics (1s cache)", authPublic),
		v1live(http.MethodGet, "/api/v1/stats/24h/{symbol}", TierPublic, "Phase-11 Task 11.3.5",
			"Per-symbol rolling 24h market statistics (1s cache)", authPublic),
		v1(http.MethodGet, "/api/v1/history/ticks/{symbol}", TierBasic, "Phase-23 Task 23.3.4",
			"Historical tick data (?from&to&limit&cursor; canonical §8.4 path)", authRead),
		v1live(http.MethodGet, "/api/v1/account/api-keys", TierBasic, "Phase-12 Task 12.3.3",
			"List programmatic API keys", authRead),
		v1live(http.MethodPost, "/api/v1/account/api-keys", TierBasic, "Phase-12 Task 12.3.3",
			"Create programmatic API key (TOTP step-up enforced)", authUser),
		v1live(http.MethodDelete, "/api/v1/account/api-keys/{id}", TierBasic, "Phase-12 Task 12.3.3",
			"Revoke programmatic API key", authUser),
		v1live(http.MethodGet, "/api/v1/account/profile", TierBasic, "Phase-12 Task 12.3.3",
			"Account profile", authRead),
		v1live(http.MethodPut, "/api/v1/account/profile", TierBasic, "Phase-12 Task 12.3.3",
			"Update account profile", authUser),
		v1live(http.MethodPost, "/api/v1/account/emergency-freeze", TierBasic, "Phase-12 Task 12.3.10",
			"Client-initiated emergency account freeze (session-only; mass-cancel ≤3 retries → FROZEN+SELF_FREEZE, other sessions + API keys revoked; P1 alert on partial failure)", authUser),
		v1live(http.MethodPost, "/api/v1/account/close", TierBasic, "Phase-14 Task 14.3.9",
			"Account closure/offboarding (RequireTwoFactor; blocked while positions/orders/settlements/funding pending — ACCOUNT_CLOSE_BLOCKED; residual sweep via Phase-11 withdrawals)", authUser),
		v1live(http.MethodPost, "/api/v1/account/appropriateness", TierBasic, "Phase-14 Task 14.3.7",
			"Submit MiFID II appropriateness assessment {instrument_class, score, answers} (12-month expiry)", authUser),
		v1live(http.MethodGet, "/api/v1/account/appropriateness", TierBasic, "Phase-14 Task 14.3.7",
			"Own client_category, nbp entitlement + assessment history", authRead),
		v1live(http.MethodPost, "/api/v1/auth/login", TierPublic, "Phase-12 Task 12.3.1",
			"Password login → JWT pair (two-phase TOTP challenge)", authPublic),
		v1live(http.MethodPost, "/api/v1/auth/logout", TierBasic, "Phase-12 Task 12.3.1",
			"Logout (revoke session)", authUser),
		v1live(http.MethodPost, "/api/v1/auth/refresh", TierPublic, "Phase-12 Task 12.3.1",
			"Refresh-token → new access token", authPublic),
		v1live(http.MethodPost, "/api/v1/auth/register", TierPublic, "Phase-12 Task 12.3.1",
			"Account registration (bcrypt, verification email)", authPublic),
		v1live(http.MethodPost, "/api/v1/auth/verify-email", TierPublic, "Phase-12 Task 12.3.1",
			"Consume the emailed verification token (24h, single-use)", authPublic),
		v1live(http.MethodPost, "/api/v1/auth/forgot-password", TierPublic, "Phase-12 Task 12.3.1",
			"Dispatch password-reset link (1h token, uniform response)", authPublic),
		v1live(http.MethodPost, "/api/v1/auth/reset-password", TierPublic, "Phase-12 Task 12.3.1",
			"Consume reset token, set new password, revoke sessions", authPublic),
		v1live(http.MethodPost, "/api/v1/kyc/submit", TierBasic, "Phase-12 Task 12.3.4",
			"Submit KYC documents (multipart doc_<TYPE> parts; → S3 SSE-KMS)", authUser),
		v1live(http.MethodGet, "/api/v1/kyc/status", TierBasic, "Phase-12 Task 12.3.4",
			"KYC verification status (tier, limits, latest submission)", authRead),
		v1live(http.MethodGet, "/api/v1/kyc/requirements", TierBasic, "Phase-12 Task 12.3.13",
			"Ops-matrix query: tier+jurisdiction → policy, doc grid, cadence, SLA", authRead),
		v1live(http.MethodPost, "/api/v1/kyc/self-certification", TierBasic, "Phase-12 Task 12.3.13",
			"Tax self-certification intake (W-8BEN/W-8BEN-E/W-9, T1+, TIN validated)", authUser),
		v1live(http.MethodGet, "/api/v1/kyc/self-certification", TierBasic, "Phase-12 Task 12.3.13",
			"Own tax self-certification history", authRead),

		// ==================================================================
		// Task 5.3.46 — client-surface route registration
		// ==================================================================
		// Every remaining client endpoint declared by spec §12, §21.4–21.11
		// and §8.4 registers here — Stub until its owning phase lands the
		// handler (spec §8.4 item 4 completeness invariant). Normalization:
		// doc artifacts ("/api/v1/orders.", brace-expanded
		// "{register,authenticate}", bare "/api/v1/admin/" namespaces) were
		// flattened to concrete paths; the superseded
		// /api/v1/market-data/ticks/{instrument} variant is NOT registered
		// (canonical: /api/v1/history/ticks/{symbol}).

		// ---- Auth & 2FA (Phase-12) ----
		v1live(http.MethodPost, "/api/v1/auth/2fa/setup", TierBasic, "Phase-12 Task 12.3.2",
			"TOTP enrollment — returns secret/QR (10m pending candidate, non-destructive)", authUser),
		v1live(http.MethodPost, "/api/v1/auth/2fa/verify", TierBasic, "Phase-12 Task 12.3.2",
			"Activate TOTP (verify candidate → backup codes) or session step-up", authUser),
		v1live(http.MethodPost, "/api/v1/auth/2fa/enroll", TierBasic, "Phase-12 Task 12.3.2",
			"2FA enrollment alias (setup + verify ceremony)", authUser),
		v1live(http.MethodPost, "/api/v1/auth/2fa/disable", TierBasic, "Phase-12 Task 12.3.2",
			"Disable 2FA (password + live TOTP/backup code required)", authUser),
		v1live(http.MethodPost, "/api/v1/auth/passkey/assert", TierPublic, "Phase-12 Task 12.3.7",
			"WebAuthn/FIDO2 assertion challenge → session", authPublic),

		// ---- Account self-service (Phase-12/14) ----
		v1live(http.MethodPost, "/api/v1/account/change-password", TierBasic, "Phase-12 Task 12.3.3",
			"Change password (revokes other sessions)", authUser),
		v1live(http.MethodGet, "/api/v1/account/login-history", TierBasic, "Phase-12 Task 12.3.9",
			"Login history (IP/device/timestamp audit, 90d)", authRead),
		v1live(http.MethodGet, "/api/v1/account/notifications/preferences", TierBasic, "Phase-12 Task 12.3.6",
			"Notification preferences", authRead),
		v1live(http.MethodPut, "/api/v1/account/notifications/preferences", TierBasic, "Phase-12 Task 12.3.6",
			"Update notification preferences", authUser),
		v1live(http.MethodPut, "/api/v1/account/settings/anti-phishing-code", TierBasic, "Phase-12 Task 12.3.8",
			"Set anti-phishing code shown in outbound mail (2FA)", authUser),
		v1live(http.MethodPost, "/api/v1/account/webauthn/register", TierBasic, "Phase-12 Task 12.3.7",
			"Register WebAuthn/FIDO2 credential", authUser),
		v1live(http.MethodPost, "/api/v1/account/webauthn/authenticate", TierBasic, "Phase-12 Task 12.3.7",
			"WebAuthn assertion (step-up auth)", authUser),
		v1live(http.MethodPost, "/api/v1/account/unfreeze-request", TierBasic, "Phase-12 Task 12.3.10",
			"Request unfreeze of a self-frozen account (durable re-verification record; Phase-14 workflow decides)", authUser),

		// ---- Task 12.3.11 institutional delegated logins + M-of-N ----
		v1live(http.MethodGet, "/api/v1/account/delegated-users", TierBasic, "Phase-12 Task 12.3.11",
			"List client delegated users with active role/scope bindings", authRead),
		v1live(http.MethodPost, "/api/v1/account/delegated-users", TierBasic, "Phase-12 Task 12.3.11",
			"Create a named delegated login (CLIENT_* role + explicit scope)", authUser),
		v1live(http.MethodPut, "/api/v1/account/delegated-users/{id}", TierBasic, "Phase-12 Task 12.3.11",
			"Update a delegated user's role/scope binding (SCOPE_CHANGED audit)", authUser),
		v1live(http.MethodDelete, "/api/v1/account/delegated-users/{id}", TierBasic, "Phase-12 Task 12.3.11",
			"Revoke a delegated login (sessions terminated immediately)", authUser),
		v1live(http.MethodPost, "/api/v1/account/delegated-users/revoke-all", TierBasic, "Phase-12 Task 12.3.11",
			"Emergency master revocation — every delegation + session dies", authUser),
		v1live(http.MethodGet, "/api/v1/account/approval-policies", TierBasic, "Phase-12 Task 12.3.11",
			"List client M-of-N approval policies", authRead),
		v1live(http.MethodPut, "/api/v1/account/approval-policies", TierBasic, "Phase-12 Task 12.3.11",
			"Upsert M-of-N policy (operation, required_approvals, threshold, expiry)", authUser),
		v1live(http.MethodDelete, "/api/v1/account/approval-policies/{id}", TierBasic, "Phase-12 Task 12.3.11",
			"Disable an approval policy (open requests still decide)", authUser),
		v1live(http.MethodGet, "/api/v1/account/approval-requests", TierBasic, "Phase-12 Task 12.3.11",
			"List M-of-N approval requests (?status=&limit=)", authRead),
		v1live(http.MethodPost, "/api/v1/account/approval-requests/{id}/decide", TierBasic, "Phase-12 Task 12.3.11",
			"Cast one CLIENT_APPROVER vote ({approve, note}; anti-self-approval)", authUser),
		v1(http.MethodPut, "/api/v1/account/consent", TierBasic, "Phase-14 Task 14.3.7",
			"Record execution-policy/document consent (product gating)", authUser),
		v1live(http.MethodPost, "/api/v1/account/cooling-off", TierBasic, "Phase-14 Task 14.3.11",
			"Activate cooling-off / self-exclusion period (irrevocable; {duration, acknowledged:true})", authUser),
		v1(http.MethodPost, "/api/v1/account/gdpr/export", TierBasic, "Phase-21 Task 21.3.7",
			"GDPR data export request", authUser),
		v1(http.MethodPost, "/api/v1/account/gdpr/erase", TierBasic, "Phase-21 Task 21.3.7",
			"GDPR erasure request", authUser),

		// ---- Account trading/margin/reporting views ----
		v1(http.MethodGet, "/api/v1/account/positions", TierBasic, "Phase-19 Task 19.3.15",
			"Account positions view (netting/hedging-mode aware)", authRead),
		v1(http.MethodPost, "/api/v1/account/margin-mode", TierBasic, "Phase-19 Task 19.3.1",
			"Set margin mode (cross/isolated per spec §13)", authUser),
		v1(http.MethodPost, "/api/v1/account/leverage", TierBasic, "Phase-19 Task 19.3.16",
			"Set per-account leverage within tier ceiling", authUser),
		v1(http.MethodGet, "/api/v1/account/liquidations", TierBasic, "Phase-19",
			"Liquidation history (insurance-fund linked)", authRead),
		v1live(http.MethodGet, "/api/v1/account/pnl", TierBasic, "Phase-13 Task 13.3.4",
			"Realized/unrealized P&L rollup", authRead),
		v1(http.MethodGet, "/api/v1/account/income", TierBasic, "Phase-20 Task 20.3.6",
			"Income history (fees, rebates, funding)", authRead),
		v1(http.MethodGet, "/api/v1/account/snapshots", TierBasic, "Phase-20 Task 20.3.6",
			"Account balance/equity snapshots (?date=)", authRead),
		v1live(http.MethodGet, "/api/v1/account/solvency-proof", TierBasic, "Phase-13 Task 13.3.7",
			"Merkle inclusion proof for the account's balances", authRead),
		v1(http.MethodGet, "/api/v1/account/confirmations/{trade_id}", TierBasic, "Phase-20 Task 20.3.8",
			"Trade confirmation document (MT515)", authRead),
		v1(http.MethodGet, "/api/v1/account/cost-preview", TierBasic, "Phase-20",
			"Pre-trade cost preview (fees + spread)", authRead),
		v1(http.MethodGet, "/api/v1/reports/tca/{account_id}", TierBasic, "Phase-20 Task 20.3.9",
			"TCA execution-quality report", authRead),
		v1live(http.MethodPost, "/api/v1/account/swap-free/request", TierBasic, "Phase-14 Task 14.3.15",
			"Request swap-free account status (attestation ref required)", authUser),
		v1live(http.MethodGet, "/api/v1/account/swap-free", TierBasic, "Phase-14 Task 14.3.15",
			"Swap-free status + latest verification", authRead),

		// ---- Public venue/market surface ----
		v1live(http.MethodGet, "/api/v1/system/status", TierPublic, "Phase-09 Task 9.3.18/9.3.25",
			"Public status feed (degradation mode, shard health)", authPublic),
		v1live(http.MethodGet, "/api/v1/system/incidents", TierPublic, "Phase-09 Task 9.3.25",
			"Public incident notices incl. postmortem links", authPublic),
		v1(http.MethodGet, "/api/v1/execution-policy", TierPublic, "Phase-21 Task 21.3.15",
			"ACTIVE execution policy document", authPublic),
		v1(http.MethodGet, "/api/v1/venue/info", TierPublic, "Phase-05 Task 5.3.44",
			"Venue info alias of /api/v1/exchange-info", authPublic),
		v1(http.MethodGet, "/api/v1/market/depth", TierPublic, "Phase-06 Task 6.3.5",
			"REST depth snapshot for resync (?symbol=&limit=, last_update_id)", authPublic),
		v1(http.MethodGet, "/api/v1/market/open-interest", TierPublic, "Phase-06",
			"Open interest snapshot", authPublic),
		v1(http.MethodGet, "/api/v1/market/performance", TierPublic, "Phase-23",
			"Market performance summary", authPublic),
		v1(http.MethodGet, "/api/v1/market/positioning", TierPublic, "Phase-23",
			"Market positioning aggregates", authPublic),
		v1(http.MethodGet, "/api/v1/market/taker-volume", TierPublic, "Phase-23",
			"Taker buy/sell volume split", authPublic),
		v1(http.MethodGet, "/api/v1/market-data/snapshot", TierPublic, "Phase-06",
			"Full L2/L3 snapshot fallback for WS gap recovery", authPublic),
		v1(http.MethodGet, "/api/v1/market-data/l3-snapshot/{symbol}", TierPublic, "Phase-17 Task 17.3.2",
			"L3 order-level snapshot (≤100k-order ceiling)", authRead),
		v1(http.MethodGet, "/api/v1/instruments/{symbol}/pip-value", TierPublic, "Phase-03 Task 3.3.12",
			"Pip value calculator (cross-currency, JPY-aware)", authPublic),
		v1(http.MethodGet, "/api/v1/instruments/{symbol}/swap-rates", TierPublic, "Phase-03 Task 3.3.11",
			"Overnight swap rates for the instrument", authPublic),
		v1(http.MethodGet, "/api/v1/history/klines/{symbol}", TierPublic, "Phase-23 Task 23.3.1",
			"Historical OHLCV (long range, ClickHouse)", authPublic),
		v1(http.MethodGet, "/api/v1/history/trades/{symbol}", TierPublic, "Phase-23 Task 23.3.4",
			"Historical trades feed", authPublic),
		v1(http.MethodGet, "/api/v1/history/trades/{symbol}/export", TierBasic, "Phase-23 Task 23.3.4",
			"Bulk trades export (JSON/CSV, cursor)", authRead),
		v1(http.MethodGet, "/api/v1/history/block-trades/{symbol}", TierPublic, "Phase-24 Task 24.3.15",
			"Block trade prints", authPublic),
		v1(http.MethodGet, "/api/v1/history/swap-rates", TierPublic, "Phase-03 Task 3.3.11",
			"Historical swap-rate series", authPublic),
		v1(http.MethodGet, "/api/v1/analytics/open-interest/{symbol}", TierPublic, "Phase-23",
			"OI history (1h/4h/1d granularity)", authPublic),
		v1(http.MethodGet, "/api/v1/analytics/long-short-ratio/{symbol}", TierPublic, "Phase-23",
			"Aggregate long/short ratio (5-min delayed)", authPublic),
		v1(http.MethodGet, "/api/v1/analytics/taker-flow/{symbol}", TierPublic, "Phase-23",
			"Taker buy/sell flow ratio", authPublic),
		v1(http.MethodGet, "/api/v1/analytics/volume", TierPublic, "Phase-23",
			"Volume analytics", authPublic),
		v1(http.MethodGet, "/api/v1/analytics/pnl", TierBasic, "Phase-23",
			"Account P&L analytics", authRead),
		v1(http.MethodGet, "/api/v1/analytics/stats", TierPublic, "Phase-23",
			"Aggregate venue stats", authPublic),
		v1live(http.MethodGet, "/api/v1/solvency/latest", TierPublic, "Phase-13 Task 13.3.7",
			"Latest proof-of-reserves Merkle root (GPG-signed)", authPublic),
		v1live(http.MethodGet, "/api/v1/solvency/proof", TierBasic, "Phase-13 Task 13.3.7",
			"Merkle inclusion proof (?account_id&currency)", authRead),
		v1live(http.MethodGet, "/api/v1/public/proof-of-reserves/daily-root", TierPublic, "Phase-13 Task 13.3.7",
			"Daily published Merkle root", authPublic),

		// ---- Advanced order types (Phase-16) ----
		v1(http.MethodPost, "/api/v1/orders/twap", TierBasic, "Phase-16 Task 16.3.1",
			"Submit TWAP algo order", authTrade),
		v1(http.MethodPost, "/api/v1/orders/vwap", TierBasic, "Phase-16 Task 16.3.2",
			"Submit VWAP algo order", authTrade),
		v1(http.MethodPost, "/api/v1/orders/scaled", TierBasic, "Phase-16",
			"Submit scaled (iceberg-ladder) order", authTrade),
		v1(http.MethodPost, "/api/v1/orders/bracket", TierBasic, "Phase-16 Task 16.3.14",
			"Submit bracket order (parent→SL+TP OCO children)", authTrade),
		v1live(http.MethodPost, "/api/v1/orders/oco", TierBasic, "Phase-14 Task 14.3.1",
			"Submit OCO pair (linked legs; first fill cancels sibling)", authTrade),
		v1(http.MethodPost, "/api/v1/orders/spread", TierBasic, "Phase-16 Task 16.3.14",
			"Submit spread/composite order", authTrade),
		v1(http.MethodPost, "/api/v1/orders/roll", TierBasic, "Phase-22",
			"Roll a forward/swap position to a new value date", authTrade),
		v1(http.MethodPost, "/api/v1/orders/algo", TierBasic, "Phase-16",
			"Submit algo order (type + params)", authTrade),
		v1(http.MethodPost, "/api/v1/orders/algo/{id}/pause", TierBasic, "Phase-16",
			"Pause running algo order", authTrade),
		v1(http.MethodPost, "/api/v1/orders/algo/{id}/resume", TierBasic, "Phase-16",
			"Resume paused algo order", authTrade),
		v1(http.MethodDelete, "/api/v1/orders/algo/{id}", TierBasic, "Phase-16",
			"Cancel algo order (children terminated via owning engine)", authTrade),
		v1(http.MethodGet, "/api/v1/algo-orders", TierBasic, "Phase-16 Task 16.3.21",
			"List running algo orders + child progress", authRead),
		v1(http.MethodDelete, "/api/v1/algo-orders", TierBasic, "Phase-16 Task 16.3.21",
			"Cancel all running algos (children via owning engine)", authTrade),
		v1(http.MethodPost, "/api/v1/orders/cancel-all-after", TierBasic, "Phase-05 Task 5.3.33 / Phase-18 Task 18.3.9",
			"Dead-man/CoD countdown: refresh TTL; on expiry resting orders are purged (spec §8.9)", authTrade),
		v1(http.MethodGet, "/api/v1/order-lists", TierBasic, "Phase-16 Task 16.3.20",
			"List open OPO/OCO order lists", authRead),
		v1(http.MethodGet, "/api/v1/order-lists/history", TierBasic, "Phase-16 Task 16.3.20",
			"List closed OPO/OCO order lists", authRead),
		v1(http.MethodGet, "/api/v1/order-lists/{id}", TierBasic, "Phase-16 Task 16.3.20",
			"Order-list detail (parent-child state)", authRead),
		v1(http.MethodPost, "/api/v1/allocations", TierBasic, "Phase-24 Task 24.3.15",
			"Post-trade block-trade allocation instruction", authTransfer),

		// ---- Product surfaces (Phase-14/16) ----
		v1live(http.MethodGet, "/api/v1/copy/strategies", TierPublic, "Phase-14 Task 14.3.14",
			"Copy-trading strategy leaderboard (computed stats only)", authPublic),
		v1live(http.MethodPost, "/api/v1/copy/strategies", TierBasic, "Phase-14 Task 14.3.14",
			"Create an INCUBATING strategy profile (manager)", authUser),
		v1live(http.MethodPost, "/api/v1/copy/strategies/{id}/list", TierBasic,
			"Phase-14 Task 14.3.14",
			"Request LISTED (≥30d incubation + appropriateness PASS)", authUser),
		v1live(http.MethodPost, "/api/v1/admin/copy/strategies/{id}/suspend", TierBasic,
			"Phase-14 Task 14.3.14",
			"Compliance Officer misconduct suspension (audit-logged)", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodPost, "/api/v1/copy/follows", TierBasic, "Phase-14 Task 14.3.14",
			"Follow a strategy (allocation, safety mode, stop-loss cap)", authUser),
		v1live(http.MethodDelete, "/api/v1/copy/follows/{id}", TierBasic, "Phase-14 Task 14.3.14",
			"Unfollow (cancels pending child orders; open positions stay)", authUser),
		v1live(http.MethodPost, "/api/v1/pamm/pools", TierBasic, "Phase-14 Task 14.3.8",
			"Create a PAMM pool (manager)", authUser),
		v1live(http.MethodPost, "/api/v1/pamm/pools/{id}/invest", TierBasic,
			"Phase-14 Task 14.3.8",
			"PAMM_INVEST — internal investment (not a deposit; no fiat caps)", authTransfer),
		v1live(http.MethodPost, "/api/v1/pamm/pools/{id}/redeem", TierBasic,
			"Phase-14 Task 14.3.8",
			"PAMM_REDEEM — internal redemption (not a withdrawal; no fiat caps)", authTransfer),
		v1(http.MethodGet, "/api/v1/bots/grid", TierBasic, "Phase-16 Task 16.3.19",
			"List grid bots (live P&L, filled levels)", authRead),
		v1(http.MethodPost, "/api/v1/bots/grid", TierBasic, "Phase-16 Task 16.3.19",
			"Create grid bot (≤5 concurrent per account)", authUser),
		v1(http.MethodGet, "/api/v1/bots/grid/{id}", TierBasic, "Phase-16 Task 16.3.19",
			"Grid-bot detail + fills", authRead),
		v1(http.MethodDelete, "/api/v1/bots/grid/{id}", TierBasic, "Phase-16 Task 16.3.19",
			"Stop grid bot + cancel child orders", authUser),

		// ---- Admin: instruments/lifecycle/market-ops ----
		// (Phase-15 Tasks 15.3.1/15.3.2/15.3.9 — live; role gates follow the
		// spec §7.2 matrix: suspend is Compliance Officer+, create/delist are
		// Super Admin + dual control, resume is Risk Manager+ + dual control,
		// and cancel-only's Risk Manager|Compliance Officer two-role gate is
		// enforced at the service layer over the any-admin route gate.)
		v1live(http.MethodGet, "/api/v1/admin/instruments", TierBasic, "Phase-15 Task 15.3.2/15.3.8",
			"List instruments incl. non-ACTIVE states", adminAuth(RoleRiskManager)),
		v1liveDC(http.MethodPost, "/api/v1/admin/instruments", TierBasic, "Phase-15 Task 15.3.2",
			"Create instrument →DRAFT (Super Admin + dual control)", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodPut, "/api/v1/admin/instruments/{id}", TierBasic, "Phase-15 Task 15.3.2/15.3.8",
			"Update instrument parameters (DRAFT/ACTIVE only)", adminAuth(RoleRiskManager)),
		v1live(http.MethodPost, "/api/v1/admin/instruments/{id}/activate", TierBasic, "Phase-15 Task 15.3.2",
			"Lifecycle: DRAFT→ACTIVE", adminAuth(RoleRiskManager)),
		v1live(http.MethodPost, "/api/v1/admin/instruments/{id}/suspend", TierBasic, "Phase-15 Task 15.3.2",
			"Lifecycle: → SUSPENDED (5-min cancel grace then mass-cancel)", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodPost, "/api/v1/admin/instruments/{id}/restrict", TierBasic, "Phase-15 Task 15.3.2",
			"Lifecycle: → RESTRICTED (limit-only; 24h notice window)", adminAuth(RoleRiskManager)),
		v1live(http.MethodPost, "/api/v1/admin/instruments/{id}/cancel-only", TierBasic, "Phase-15 Task 15.3.9",
			"Lifecycle: → CANCEL_ONLY (persistent; resting orders preserved; RM|CO)", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodPost, "/api/v1/admin/instruments/{id}/halt", TierBasic, "Phase-15 Task 15.3.2",
			"Lifecycle: → HALTED (orders rest, no matching)", adminAuth(RoleRiskManager)),
		v1liveDC(http.MethodPost, "/api/v1/admin/instruments/{id}/resume", TierBasic, "Phase-15 Task 15.3.2",
			"Lifecycle: resume →ACTIVE (reopening CALL; skip_auction opts out)", adminAuth(RoleRiskManager)),
		v1liveDC(http.MethodPost, "/api/v1/admin/instruments/{id}/delist", TierBasic, "Phase-15 Task 15.3.2",
			"Lifecycle: → DELISTED (30d reduce-only close window)", adminAuth(RoleSuperAdmin)),
		v1(http.MethodPost, "/api/v1/admin/instruments/{id}/uncross-override", TierBasic, "Phase-15 Task 15.3.10",
			"Uncrossed-book override (quarantine release)", adminAuth(RoleRiskManager)),
		// Phase-15 Task 15.3.4 — market schedule (24/5 window) admin CRUD.
		// Reads are open to any venue admin (the schedule is operational
		// reference data); mutations pin Risk Manager (Super Admin holds
		// all permissions per spec §8.2).
		v1live(http.MethodGet, "/api/v1/admin/market-schedule", TierBasic, "Phase-15 Task 15.3.4",
			"Merged market:hours schedule document + overrides", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/market-schedule/overrides", TierBasic, "Phase-15 Task 15.3.4",
			"List market_schedule_overrides (incl. expired)", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodPost, "/api/v1/admin/market-schedule/overrides", TierBasic, "Phase-15 Task 15.3.4",
			"Create holiday/partial-session override {date, closed, open?, close?, reason} — republishes market:hours", adminAuth(RoleRiskManager)),
		v1live(http.MethodPut, "/api/v1/admin/market-schedule/overrides/{id}", TierBasic, "Phase-15 Task 15.3.4",
			"Update override window {closed, open?, close?, reason} (date immutable)", adminAuth(RoleRiskManager)),
		v1live(http.MethodDelete, "/api/v1/admin/market-schedule/overrides/{id}", TierBasic, "Phase-15 Task 15.3.4",
			"Delete override (audit row preserves it) — republishes market:hours", adminAuth(RoleRiskManager)),
		v1live(http.MethodGet, "/api/v1/admin/instruments/{symbol}/auction-calendar", TierBasic, "Phase-15 Task 15.3.13",
			"Auction/fixing calendar rows for the instrument", adminAuth(RoleRiskManager)),
		v1liveDC(http.MethodPut, "/api/v1/admin/instruments/{symbol}/auction-calendar", TierBasic, "Phase-15 Task 15.3.13",
			"Replace auction/fixing calendar {entries[], reason} — OpInstrumentCalendar four-eyes", adminAuth(RoleRiskManager)),
		v1liveDC(http.MethodPost, "/api/v1/admin/trades/{id}/bust", TierBasic, "Phase-15 Task 15.3.5",
			"Trade bust / price-adjust — obvious-error review (§7.3.4 15min window; BUSTED/PRICE_ADJUSTED flags)",
			adminAuth(RoleRiskManager)),
		{Method: http.MethodPost, Path: "/api/v1/admin/circuit-breaker/{symbol}", Version: "v1",
			Auth: adminAuth(RoleRiskManager), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-13 Task 13.3.1", Status: StatusLive,
			Description: "Trip circuit breaker for symbol (optional body {scope,target_id} for ACCOUNT/MARKET_WIDE tiers)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/circuit-breaker/{symbol}/reset", Version: "v1",
			Auth: adminAuth(RoleRiskManager), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-13 Task 13.3.9", Status: StatusLive, DualControl: true,
			Description: "Reset circuit breaker for symbol (four-eyes queue: 2 approvers ≤15min)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/kill-switch", Version: "v1",
			Auth: adminAuth(RoleRiskManager), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.8", Status: StatusLive, DualControl: true,
			Description: "Scoped kill switch {scope: GLOBAL|ACCOUNT|COUNTERPARTY|INSTRUMENT|INSTRUMENT_CLASS|FIX_SESSION|LP|RAIL|REGION|ENV|DESK, target_id, reason, approver_id}"},
		{Method: http.MethodPost, Path: "/api/v1/admin/kill-switch/reset", Version: "v1",
			Auth: adminAuth(RoleRiskManager), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.8", Status: StatusLive, DualControl: true,
			Description: "Resume after kill-switch trip {scope, target_id, approver_id}"},
		v1live(http.MethodGet, "/api/v1/admin/kill-switch", TierBasic, "Phase-11 Task 11.3.4",
			"Active suspension set (scope/target/reason/actors)", adminAuth(RoleReadOnlyAuditor)),

		// ---- Admin: compliance/finance/risk ----
		v1live(http.MethodPost, "/api/v1/admin/kyc/{id}/approve", TierBasic, "Phase-14 Task 14.3.4",
			"KYC approve — tier assign + re-verification horizon", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodPost, "/api/v1/admin/kyc/{id}/reject", TierBasic, "Phase-14 Task 14.3.4",
			"KYC reject — reason mandatory {reason}", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodPost, "/api/v1/admin/sar", TierBasic, "Phase-21",
			"File SAR report", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodGet, "/api/v1/admin/compliance-report", TierBasic, "Phase-21",
			"Compliance reporting export", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodGet, "/api/v1/admin/emir-report", TierBasic, "Phase-21 Task 21.3.14",
			"EMIR REFIT lifecycle report", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodGet, "/api/v1/admin/mifid-report", TierBasic, "Phase-21 Task 21.3.19",
			"MiFID II RTS 27/28 best-execution report", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodGet, "/api/v1/admin/basel-report", TierBasic, "Phase-21 Task 21.3.13",
			"Basel III reporting pack", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodPost, "/api/v1/admin/enforcement/{signal_id}", TierBasic, "Phase-21 Task 21.3.8",
			"Enforce on a confirmed market-abuse signal", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodGet, "/api/v1/admin/employee-dealing/audit", TierBasic, "Phase-21 Task 21.3.24",
			"Employee-dealing pre-clearance audit", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodPost, "/api/v1/admin/pre-clearance", TierBasic, "Phase-21 Task 21.3.24",
			"Grant employee pre-clearance", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodGet, "/api/v1/admin/restricted-lists", TierBasic, "Phase-21 Task 21.3.24",
			"List employee-dealing restricted lists", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodDelete, "/api/v1/admin/restricted-lists", TierBasic, "Phase-21 Task 21.3.24",
			"Remove restricted-list entry", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodPost, "/api/v1/admin/regulatory-changes", TierBasic, "Phase-21",
			"Register a regulatory change record", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodPut, "/api/v1/admin/regulatory-changes/{id}/impact", TierBasic, "Phase-21",
			"Record regulatory-change impact assessment", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodGet, "/api/v1/admin/promotions", TierBasic, "Phase-21 Task 21.3.26",
			"List financial promotions", adminAuth(RoleComplianceOfficer)),
		v1(http.MethodPut, "/api/v1/admin/promotions", TierBasic, "Phase-21 Task 21.3.26",
			"Update financial promotion", adminAuth(RoleComplianceOfficer)),
		{Method: http.MethodPost, Path: "/api/v1/admin/promotions/{id}/approve", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-21 Task 21.3.26", Status: StatusStub, DualControl: true,
			Description: "Approve financial promotion (≤12-month approval expiry)"},
		v1(http.MethodGet, "/api/v1/admin/promotions/report", TierBasic, "Phase-21 Task 21.3.26",
			"Promotion compliance report", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodPost, "/api/v1/admin/swap-free/{id}/approve", TierBasic, "Phase-14 Task 14.3.15",
			"Approve swap-free account request", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodPost, "/api/v1/admin/swap-free/{id}/reject", TierBasic, "Phase-14 Task 14.3.15",
			"Reject swap-free account request", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodPost, "/api/v1/admin/swap-free/{id}/revoke", TierBasic, "Phase-14 Task 14.3.15",
			"Revoke swap-free status (abuse guard → compliance hold)", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodPut, "/api/v1/admin/accounts/{id}/product-profile", TierBasic, "Phase-14 Task 14.3.7",
			"Set client categorization {client_category, evidence}", adminAuth(RoleComplianceOfficer)),
		{Method: http.MethodPost, Path: "/api/v1/admin/accounts/{id}/product-profile", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.13", Status: StatusLive,
			Description: "Assign account product profile {profile_code} — Task 14.3.7 owns the PUT variant (categorization)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/product-profiles", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.13", Status: StatusLive, DualControl: true,
			Description: "Create product profile (dual-control maker)"},
		{Method: http.MethodPut, Path: "/api/v1/admin/product-profiles", Version: "v1",
			Auth: adminAuth(RoleComplianceOfficer), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-14 Task 14.3.13", Status: StatusLive, DualControl: true,
			Description: "Update product profile pricing_plan/scope/divisor (dual-control maker)"},
		v1live(http.MethodGet, "/api/v1/admin/product-profiles", TierBasic, "Phase-14 Task 14.3.13",
			"List product profiles (?include_retired=)", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodPut, "/api/v1/admin/product-profiles/{id}/target-market", TierBasic, "Phase-14 Task 14.3.16",
			"Upsert (profile,category) target market", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodPost, "/api/v1/admin/product-target-markets/{id}/review", TierBasic, "Phase-14 Task 14.3.16",
			"Periodic review: APPROVE|NARROW|SUSPEND", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodGet, "/api/v1/admin/product-target-markets", TierBasic, "Phase-14 Task 14.3.16",
			"Target-market review queue (?overdue=true)", adminAuth(RoleComplianceOfficer)),

		// ---- Admin: finance/ops ----
		v1(http.MethodGet, "/api/v1/admin/finance/trial-balance", TierBasic, "Phase-20 Task 20.3.7",
			"Trial balance per currency", adminAuth(RoleFinanceOps)),
		v1(http.MethodGet, "/api/v1/admin/finance/pnl", TierBasic, "Phase-20 Task 20.3.7",
			"House P&L statement (CSV/Parquet)", adminAuth(RoleFinanceOps)),
		v1(http.MethodGet, "/api/v1/admin/finance/balance-sheet", TierBasic, "Phase-20 Task 20.3.7",
			"House balance-sheet export (CSV/Parquet)", adminAuth(RoleFinanceOps)),
		v1(http.MethodGet, "/api/v1/admin/invoices", TierBasic, "Phase-20 Task 20.3.6",
			"Client invoicing surface", adminAuth(RoleFinanceOps)),
		v1(http.MethodGet, "/api/v1/admin/insurance-fund", TierBasic, "Phase-19 Task 19.3.14",
			"Insurance fund balance + governance view", adminAuth(RoleRiskManager)),
		v1(http.MethodPut, "/api/v1/admin/collateral-schedule", TierBasic, "Phase-19 Task 19.3.8",
			"Update collateral haircut schedule", adminAuth(RoleRiskManager)),
		v1(http.MethodGet, "/api/v1/admin/nostro-accounts", TierBasic, "Phase-24 Task 24.3.8",
			"Nostro/vostro account registry", adminAuth(RoleFinanceOps)),
		v1(http.MethodPost, "/api/v1/admin/nostro-accounts", TierBasic, "Phase-24 Task 24.3.8",
			"Register nostro account", adminAuth(RoleFinanceOps)),
		v1(http.MethodGet, "/api/v1/admin/nostro-reconciliation", TierBasic, "Phase-24",
			"Correspondent-bank reconciliation view", adminAuth(RoleReadOnlyAuditor)),
		v1(http.MethodGet, "/api/v1/admin/pb-reconciliation", TierBasic, "Phase-24 Task 24.3.7",
			"Prime-broker give-up reconciliation", adminAuth(RoleReadOnlyAuditor)),
		// Phase-13 Task 13.3.2 — nine-category hourly reconciliation engine.
		v1live(http.MethodGet, "/api/v1/admin/reconciliation/latest", TierBasic,
			"Phase-13 Task 13.3.2",
			"Latest reconciliation run + findings", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodGet, "/api/v1/admin/reconciliation/runs", TierBasic,
			"Phase-13 Task 13.3.2",
			"Recent reconciliation runs (?limit=, ?run_id= drill-down)",
			adminAuth(RoleReadOnlyAuditor)),
		v1(http.MethodGet, "/api/v1/admin/swift-messages", TierBasic, "Phase-24 Task 24.3.12",
			"Bank statement ingestion log (MT940/942/camt.053)", adminAuth(RoleFinanceOps)),
		v1(http.MethodGet, "/api/v1/admin/treasury/own-funds", TierBasic, "Phase-24 Task 24.3.17",
			"House own-funds balances", adminAuth(RoleFinanceOps)),
		v1(http.MethodPost, "/api/v1/admin/treasury/contingent-capital", TierBasic, "Phase-24 Task 24.3.17",
			"Manage contingent-capital commitments", adminAuth(RoleFinanceOps)),
		{Method: http.MethodPost, Path: "/api/v1/admin/settlement-exceptions/{id}/resolve", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-24", Status: StatusStub, DualControl: true,
			Description: "Resolve settlement exception (money-moving — four-eyes on Phase-24 implementation)"},
		v1(http.MethodPost, "/api/v1/admin/client-money/audits", TierBasic, "Phase-24 Task 24.3.11",
			"Run client-money safeguarding audit", adminAuth(RoleFinanceOps)),
		v1(http.MethodPost, "/api/v1/admin/client-money/audits/{id}/evidence-pack", TierBasic, "Phase-24 Task 24.3.11",
			"Generate client-money evidence pack", adminAuth(RoleFinanceOps)),
		v1(http.MethodPost, "/api/v1/admin/client-money/certifications", TierBasic, "Phase-24 Task 24.3.11",
			"Record client-money certification", adminAuth(RoleFinanceOps)),

		// ---- Admin: platform/ops ----
		v1live(http.MethodGet, "/api/v1/admin/audit", TierBasic, "Phase-07 Task 7.3.3",
			"Audit log query surface", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodGet, "/api/v1/admin/audit-log", TierBasic, "Phase-07 Task 7.3.3",
			"Audit log (filters)", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodGet, "/api/v1/admin/dlq", TierBasic, "Phase-07 Task 7.3.10",
			"Dead-letter queue review surface", adminAuth(RoleSupportAgent)),
		v1live(http.MethodGet, "/api/v1/admin/roles", TierBasic, "Phase-07 Task 7.3.1",
			"RBAC role catalog", adminAuth(RoleSuperAdmin)),
		// ---- Phase-07 Tasks 7.3.2/7.3.11/7.3.12 — RBAC lifecycle surface ----
		v1live(http.MethodGet, "/api/v1/admin/bindings", TierBasic, "Phase-07 Task 7.3.11",
			"Admin role bindings (?user_id=&status=)", adminAuth(RoleReadOnlyAuditor)),
		{Method: http.MethodPost, Path: "/api/v1/admin/bindings", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-07 Task 7.3.11", Status: StatusLive, DualControl: true,
			Description: "Grant scoped admin role binding (four-eyes queue)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/bindings/{id}/revoke", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-07 Task 7.3.11", Status: StatusLive, DualControl: true,
			Description: "Revoke admin role binding (four-eyes queue)"},
		v1live(http.MethodGet, "/api/v1/admin/dual-control", TierBasic, "Phase-07 Task 7.3.2",
			"Four-eyes pending request list (?status=)", adminAuth(RoleReadOnlyAuditor)),
		// Approve/reject gate at the any-binding sentinel: the
		// per-request required_role is enforced by the dual-control
		// service against the request row, not the route.
		v1live(http.MethodPost, "/api/v1/admin/dual-control/{id}/approve", TierBasic, "Phase-07 Task 7.3.2",
			"Confirm a pending four-eyes request (per-request role applies)", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodPost, "/api/v1/admin/dual-control/{id}/reject", TierBasic, "Phase-07 Task 7.3.2",
			"Reject a pending four-eyes request (per-request role applies)", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodPost, "/api/v1/admin/recert", TierBasic, "Phase-07 Task 7.3.12",
			"Open a quarterly recertification campaign", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/recert/{id}", TierBasic, "Phase-07 Task 7.3.12",
			"Recertification campaign report (auditor export)", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodPost, "/api/v1/admin/recert/{id}/decisions", TierBasic, "Phase-07 Task 7.3.12",
			"Record a recertification decision {binding_id, approve}", adminAuth(RoleSuperAdmin)),
		{Method: http.MethodPost, Path: "/api/v1/admin/break-glass", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-07 Task 7.3.12", Status: StatusLive, DualControl: true,
			Description: "Break-glass grant — incident-confined ≤4h Super Admin"},
		// Reviewer is the incident-ops role (spec §8.2b does not pin the
		// reviewer role — Risk Manager owns incidents; grantee self-review
		// is rejected by the service).
		v1live(http.MethodPost, "/api/v1/admin/break-glass/{id}/review", TierBasic, "Phase-07 Task 7.3.12",
			"Mandatory post-incident break-glass review {notes}", adminAuth(RoleRiskManager)),
		// ---- Task 9.3.7 feature flags (admin CRUD + canary ladder) ----
		v1live(http.MethodGet, "/api/v1/admin/flags", TierBasic, "Phase-09 Task 9.3.7",
			"Feature-flag list", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodPost, "/api/v1/admin/flags", TierBasic, "Phase-09 Task 9.3.7",
			"Feature-flag create", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/flags/{name}", TierBasic, "Phase-09 Task 9.3.7",
			"Feature-flag read", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodPut, "/api/v1/admin/flags/{name}", TierBasic, "Phase-09 Task 9.3.7",
			"Feature-flag update", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodPost, "/api/v1/admin/flags/{name}", TierBasic, "Phase-09 Task 9.3.7",
			"Feature-flag toggle {enabled}", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodDelete, "/api/v1/admin/flags/{name}", TierBasic, "Phase-09 Task 9.3.7",
			"Feature-flag delete", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodPost, "/api/v1/admin/flags/{name}/advance", TierBasic, "Phase-09 Task 9.3.7",
			"Feature-flag canary ladder advance", adminAuth(RoleSuperAdmin)),
		// ---- Task 9.3.8 cache warming + Task 9.3.25 health export ----
		v1live(http.MethodPost, "/api/v1/admin/cache/warm", TierBasic, "Phase-09 Task 9.3.8",
			"Manual cache-warm trigger", adminAuth(RoleSuperAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/ops/health", TierBasic, "Phase-09 Task 9.3.25",
			"Operational health export (components, uptime, events)", adminAuth(RoleReadOnlyAuditor)),
		v1(http.MethodPut, "/api/v1/admin/fix-sessions/{id}", TierBasic, "Phase-18 Task 18.3.9",
			"FIX session entitlement/throttle update", adminAuth(RoleSuperAdmin)),
		{Method: http.MethodPut, Path: "/api/v1/admin/api-keys/{id}/extend-expiry", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-13 Task 13.3.8", Status: StatusLive, DualControl: true,
			Description: "Extend API-key privilege-expiry deadline (dual-control)"},
		v1live(http.MethodGet, "/api/v1/admin/liquidity-providers", TierBasic, "Phase-07 Task 7.3.9",
			"LP entity list (?status=)", adminAuth(RoleRiskManager)),
		v1live(http.MethodPost, "/api/v1/admin/liquidity-providers", TierBasic, "Phase-07 Task 7.3.9",
			"LP entity create (ONBOARDING; instruments attachable)", adminAuth(RoleRiskManager)),
		v1live(http.MethodGet, "/api/v1/admin/liquidity-providers/{id}", TierBasic, "Phase-07 Task 7.3.9",
			"LP entity detail incl. instrument configs", adminAuth(RoleRiskManager)),
		v1live(http.MethodPut, "/api/v1/admin/liquidity-providers", TierBasic, "Phase-07 Task 7.3.9",
			"LP entity update (lp_id in body; guarded lifecycle)", adminAuth(RoleRiskManager)),
		v1live(http.MethodPut, "/api/v1/admin/liquidity-providers/{id}", TierBasic, "Phase-07 Task 7.3.9",
			"LP entity update (path id; guarded lifecycle)", adminAuth(RoleRiskManager)),
		v1live(http.MethodGet, "/api/v1/admin/liquidity-providers/{id}/scorecard", TierBasic, "Phase-07 Task 7.3.9",
			"LP scorecard (?window=1h; persisted snapshot marked stale when no live feed)", adminAuth(RoleRiskManager)),
		v1live(http.MethodGet, "/api/v1/admin/liquidity-providers/{id}/alerts", TierBasic, "Phase-07 Task 7.3.9",
			"LP performance-alert trail (?open=1)", adminAuth(RoleRiskManager)),
		v1live(http.MethodGet, "/api/v1/admin/governance-packs", TierBasic, "Phase-07 Tasks 7.3.13/7.3.14",
			"Governance pack list (?kind=&limit=; auditors see released + CEO_DAILY)", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodGet, "/api/v1/admin/governance-packs/{id}", TierBasic, "Phase-07 Tasks 7.3.13/7.3.14",
			"Governance pack detail + hash_ok re-verification", adminAuth(RoleReadOnlyAuditor)),
		v1live(http.MethodPost, "/api/v1/admin/governance-packs/generate", TierBasic, "Phase-07 Tasks 7.3.13/7.3.14",
			"On-demand pack rebuild (CEO_DAILY|BOARD_QUARTERLY|BOARD_ADHOC)", adminAuth(RoleSuperAdmin)),
		{Method: http.MethodPost, Path: "/api/v1/admin/governance-packs/{id}/release", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-07 Task 7.3.14", Status: StatusLive, DualControl: true,
			Description: "Board-pack release (four-eyes; immutable after release)"},
		v1live(http.MethodPut, "/api/v1/admin/support/tickets", TierBasic, "Phase-07 Task 7.3.7",
			"Update support ticket (assign/transition/ADR)", adminAuth(RoleSupportAgent)),
		v1live(http.MethodGet, "/api/v1/admin/support/tickets", TierBasic, "Phase-07 Task 7.3.7",
			"Queue list (?status=&category=&queue=&assignee=&account_id=&breached=)", adminAuth(RoleSupportAgent)),
		v1live(http.MethodGet, "/api/v1/admin/support/tickets/{id}", TierBasic, "Phase-07 Task 7.3.7",
			"Ticket detail incl. internal notes", adminAuth(RoleSupportAgent)),
		v1live(http.MethodPost, "/api/v1/admin/support/tickets/{id}/notes", TierBasic, "Phase-07 Task 7.3.7",
			"Append internal note", adminAuth(RoleSupportAgent)),
		v1live(http.MethodGet, "/api/v1/admin/support/complaints/register", TierBasic, "Phase-07 Task 7.3.7",
			"MiFID complaint-handling register (COMPLAINT+DISPUTE)", adminAuth(RoleComplianceOfficer)),
		v1live(http.MethodGet, "/api/v1/admin/support/accounts/{id}", TierBasic, "Phase-07 Task 7.3.7",
			"Read-only support-view (audit-logged; no impersonation)", adminAuth(RoleSupportAgent)),

		// ---- Phase-13.5 Task 13.5.3.8/13.5.3.9 — Vulnerability Disclosure
		// Program (spec §19.11.2, §24 #332). Public submission +
		// policy page are anonymous (TierPublic per-IP bucket; honeypot +
		// 64KB cap in the handler). Admin rows declare RoleAnyAdmin: the
		// §8.2 canon has no dedicated security-engineer role, so the
		// service gates writes to Compliance Officer|Super Admin and
		// reads to +Read-Only Auditor. DualControl=false by decision —
		// VDP transitions are incident-response-time-critical; integrity
		// comes from write-once milestone timestamps + hash-chained
		// admin_audit_log, not four-eyes.
		v1live(http.MethodPost, "/api/v1/security/disclosures", TierPublic, "Phase-13.5 Task 13.5.3.8",
			"Researcher vulnerability report (idempotent report_id; honeypot `website`)", authPublic),
		v1live(http.MethodGet, "/api/v1/security/policy", TierPublic, "Phase-13.5 Task 13.5.3.8",
			"Published VDP policy: scope/safe-harbor/SLAs/bounty tiers (markdown)", authPublic),
		v1live(http.MethodPost, "/api/v1/admin/security/disclosures/intake", TierBasic, "Phase-13.5 Task 13.5.3.8",
			"Pentest/internal finding intake — same register, same SLA clock", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/security/disclosures", TierBasic, "Phase-13.5 Task 13.5.3.8",
			"VDP register list (?status=&severity=&source=&bulletin=&assignee=&breached=)", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodGet, "/api/v1/admin/security/disclosures/{id}", TierBasic, "Phase-13.5 Task 13.5.3.8",
			"Disclosure detail incl. milestone SLA evidence", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodPost, "/api/v1/admin/security/disclosures/{id}/triage", TierBasic, "Phase-13.5 Task 13.5.3.8/13.5.3.9",
			"Triage: severity + CVSS 3.1 + fix-ETA assignment (Critical 7d/High 30d/Medium 90d/Low 180d)", adminAuth(RoleAnyAdmin)),
		v1live(http.MethodPut, "/api/v1/admin/security/disclosures/{id}", TierBasic, "Phase-13.5 Task 13.5.3.8",
			"Transition/metadata update (status, assignee, bulletin, patch_ref, attribution, re-grade)", adminAuth(RoleAnyAdmin)),
	}
	return r
}

// OpenAPIDocFn, when set, replaces the Task 5.3.7 skeleton document for
// GET /api/v1/openapi.json with the Task 5.3.8 full document. Assigned by
// the wiring layer (internal/api hosts the generator — the import edge
// runs api → gateway, so injection is the only non-cyclic seam).
var OpenAPIDocFn func(r *Router) map[string]any

// MountSeed registers the seed table on the router: meta endpoints get
// their live handlers, everything else mounts the 501 stub. Emitted codes
// used by this cluster are declared to the registry so an unregistered
// emission fails startup (Task 5.3.21 gate).
func (r *Router) MountSeed() error {
	return r.MountSeedLive(nil)
}

// MountSeedLive is MountSeed with owner-task live handlers injected as a
// map keyed "METHOD path" (e.g. "GET /api/v1/account/rate-limits"). A
// Status=Live row missing a handler mounts a fail-closed 503
// SERVICE_DEGRADED shim rather than aborting the mount — the registry
// row is the normative declaration; the binary may not host every live
// owner yet.
func (r *Router) MountSeedLive(live map[string]http.Handler) error {
	// Codes this cluster emits directly — declared and checked at startup.
	if err := r.reg.CheckRegistered(
		errs.CodeNotImplemented, errs.CodeInternalError,
		"INVALID_REQUEST", "SERVICE_DEGRADED", "GATEWAY_TIMEOUT_MATCHING_ENGINE",
	); err != nil {
		return err
	}
	for _, rt := range SeedRoutes() {
		var h http.Handler
		switch rt.key() {
		case "GET /api/v1/routes":
			h = http.HandlerFunc(r.RoutesHandler)
		case "GET /api/v1/errors":
			h = http.HandlerFunc(r.ErrorsHandler)
		case "GET /api/v1/openapi.json":
			// Task 5.3.8 replaces the Task 5.3.7 skeleton via injection —
			// internal/api hosts the full generator (import direction is
			// api → gateway, so the hook is assigned by the wiring layer).
			if OpenAPIDocFn != nil {
				h = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					writeJSON(w, http.StatusOK, OpenAPIDocFn(r))
				})
			} else {
				h = http.HandlerFunc(r.OpenAPIHandler)
			}
		}
		if h == nil && live != nil {
			h = live[rt.key()]
		}
		if rt.Status == StatusLive && h == nil {
			h = r.unwiredHandler(rt)
		}
		if err := r.Register(rt, h); err != nil {
			return err
		}
	}
	return nil
}

// unwiredHandler is the fail-closed shim for a Live route whose owner
// handler is not hosted by this binary — 503 SERVICE_DEGRADED, never a
// silent pass-through or a misleading 501.
func (r *Router) unwiredHandler(rt Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.WriteError(w, req, "SERVICE_DEGRADED",
			"route handler not wired in this binary",
			map[string]any{"owner": rt.Owner, "route": rt.Method + " " + rt.Path})
	})
}
