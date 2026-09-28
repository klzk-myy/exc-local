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
		v1(http.MethodPost, "/api/v1/orders", TierBasic, "Phase-05 Task 5.3.3",
			"Submit order (idempotent on client_order_id; → Aeron → C++ core)", authTrade),
		v1(http.MethodPut, "/api/v1/orders/{id}", TierBasic, "Phase-05 Task 5.3.3 / Task 5.3.22",
			"Modify order (price/qty/tif; STALE_MODIFY on stale seq)", authTrade),
		v1(http.MethodDelete, "/api/v1/orders/{id}", TierBasic, "Phase-05 Task 5.3.3",
			"Cancel order", authTrade),
		v1(http.MethodDelete, "/api/v1/orders/all", TierBasic, "Phase-05 Task 5.3.3 / Task 5.3.25",
			"Cancel all orders (per account, per-symbol breakdown)", authTrade),
		v1(http.MethodDelete, "/api/v1/orders", TierBasic, "Phase-05 Task 5.3.25",
			"Mass cancel per instrument: ?symbol={symbol}", authTrade),
		v1(http.MethodGet, "/api/v1/orders", TierBasic, "Phase-05 Task 5.3.3",
			"Order history (cursor paginated)", authRead),
		v1(http.MethodGet, "/api/v1/orders/{id}", TierBasic, "Phase-05 Task 5.3.3",
			"Order detail", authRead),

		// ---- Task 5.3.32 batch ops ----
		v1(http.MethodPost, "/api/v1/orders/batch", TierBasic, "Phase-05 Task 5.3.32",
			"Batch submit up to 10 orders (index-mapped results)", authTrade),
		v1(http.MethodDelete, "/api/v1/orders/batch", TierBasic, "Phase-05 Task 5.3.32",
			"Batch cancel up to 20 orders", authTrade),

		// ---- Task 5.3.33 dead-man switch ----
		v1(http.MethodPost, "/api/v1/orders/countdown-cancel-all", TierBasic, "Phase-05 Task 5.3.33",
			"Dead-man countdown cancel-all (countdown_ms 1000–300000, 0 disables)", authTrade),

		// ---- Task 5.3.36 close-all ----
		v1(http.MethodPost, "/api/v1/positions/close-all", TierBasic, "Phase-05 Task 5.3.36",
			"Close all positions (optional symbol/side filters; X-2FA-Token header)", authTrade),

		// ---- Task 5.3.37 atomic cancel-replace / keep-priority ----
		v1(http.MethodPost, "/api/v1/orders/{id}/cancel-replace", TierBasic, "Phase-05 Task 5.3.37",
			"Atomic cancel-replace (mode=STOP_ON_FAILURE|ALLOW_FAILURE)", authTrade),
		v1(http.MethodPut, "/api/v1/orders/{id}/amend/keep-priority", TierBasic, "Phase-05 Task 5.3.37",
			"Quantity-down keep-priority amendment", authTrade),
		v1(http.MethodGet, "/api/v1/orders/{id}/amendments", TierBasic, "Phase-05 Task 5.3.37",
			"Client-visible amendment history", authRead),

		// ---- Task 5.3.39 dry-run preview ----
		v1(http.MethodPost, "/api/v1/orders/test", TierBasic, "Phase-05 Task 5.3.39",
			"Side-effect-free order validation/preview (margin, filters, commission)", authTrade),

		// ---- Task 5.3.4 account & balance ----
		v1(http.MethodGet, "/api/v1/account/balances", TierBasic, "Phase-05 Task 5.3.4",
			"All currency balances (available/locked/total)", authRead),
		v1(http.MethodGet, "/api/v1/positions", TierBasic, "Phase-05 Task 5.3.4",
			"Open positions with unrealized P&L", authRead),
		v1(http.MethodGet, "/api/v1/account/risk-limits", TierBasic, "Phase-05 Task 5.3.4",
			"Current risk limits + utilization", authRead),

		// ---- Task 5.3.5 market data REST (public) ----
		v1(http.MethodGet, "/api/v1/book/{symbol}", TierPublic, "Phase-05 Task 5.3.5",
			"L2 order book snapshot (?depth=20, 100ms cache)", authPublic),
		v1(http.MethodGet, "/api/v1/trades/{symbol}", TierPublic, "Phase-05 Task 5.3.5",
			"Recent trades (?limit=100, 1s cache)", authPublic),
		v1(http.MethodGet, "/api/v1/ticker/{symbol}", TierPublic, "Phase-05 Task 5.3.5",
			"24h ticker (1s cache)", authPublic),
		v1(http.MethodGet, "/api/v1/klines/{symbol}", TierPublic, "Phase-05 Task 5.3.5",
			"OHLCV candles (?interval=1m&limit=500, 1s cache)", authPublic),
		v1(http.MethodGet, "/api/v1/instruments", TierPublic, "Phase-05 Task 5.3.5",
			"Instrument reference data: tick/lot/min-notional/status/hours/filters", authPublic),

		// ---- Task 5.3.6 funding (route stubs; Phase-11 implements) ----
		v1(http.MethodGet, "/api/v1/deposits/{currency}", TierBasic, "Phase-05 Task 5.3.6 / Phase-11 Task 11.3.1",
			"Deposit instructions (bank details per currency)", authRead),
		v1(http.MethodPost, "/api/v1/withdrawals", TierBasic, "Phase-05 Task 5.3.6 / Phase-11 Task 11.3.2",
			"Create withdrawal (15min confirmation window)", authTransfer),
		v1(http.MethodPost, "/api/v1/withdrawals/{id}/confirm", TierBasic, "Phase-05 Task 5.3.6 / Phase-11 Task 11.3.2",
			"Confirm withdrawal within the 15min window", authTransfer),
		v1(http.MethodGet, "/api/v1/funding", TierBasic, "Phase-05 Task 5.3.6 / Phase-11 Task 11.3.3",
			"Funding history (paginated)", authRead),

		// ---- Task 5.3.10 sessions ----
		v1(http.MethodGet, "/api/v1/account/sessions", TierBasic, "Phase-05 Task 5.3.10",
			"List active sessions", authRead),
		v1(http.MethodDelete, "/api/v1/account/sessions/{id}", TierBasic, "Phase-05 Task 5.3.10",
			"Revoke session", authUser),
		v1(http.MethodDelete, "/api/v1/account/sessions", TierBasic, "Phase-12",
			"Revoke all sessions", authUser),

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

		// ---- Task 5.3.13 test environment (non-prod only) ----
		{Method: http.MethodPost, Path: "/api/v1/test/reset", Version: "v1",
			Auth: authUser, RateTier: TierBasic, Weight: 1, Owner: "Phase-05 Task 5.3.13",
			Status: StatusStub, Env: "nonprod",
			Description: "Reset test account balances/orders/positions (1 per 5min)"},

		// ---- Task 5.3.14 announcements ----
		v1(http.MethodGet, "/api/v1/announcements", TierPublic, "Phase-05 Task 5.3.14",
			"List announcements", authPublic),
		v1(http.MethodPost, "/api/v1/admin/announcements", TierBasic, "Phase-05 Task 5.3.14",
			"Create announcement", adminAuth(RoleSupportAgent)),
		v1(http.MethodGet, "/api/v1/maintenance/schedule", TierPublic, "Phase-05 Task 5.3.14",
			"Upcoming maintenance windows", authPublic),

		// ---- Task 5.3.15 fees ----
		v1(http.MethodGet, "/api/v1/fees", TierBasic, "Phase-05 Task 5.3.15",
			"Current fee rates incl. active promos", authRead),
		v1(http.MethodPost, "/api/v1/admin/fees/promo", TierBasic, "Phase-05 Task 5.3.15",
			"Create fee promo window", adminAuth(RoleFinanceOps)),

		// ---- Task 5.3.16 developer portal ----
		{Method: http.MethodGet, Path: "/developer", Version: "",
			Auth: authPublic, RateTier: TierPublic, Weight: 1,
			Owner: "Phase-05 Task 5.3.16 / Task 5.3.8", Status: StatusStub,
			Description: "Swagger UI developer portal"},
		{Method: http.MethodGet, Path: "/developer/migration", Version: "",
			Auth: authPublic, RateTier: TierPublic, Weight: 1,
			Owner: "Phase-05 Task 5.3.20", Status: StatusStub,
			Description: "API deprecation migration guide"},
		v1(http.MethodPost, "/api/v1/developer/api-keys", TierBasic, "Phase-05 Task 5.3.16",
			"Create API key (migration 025)", authUser),
		v1(http.MethodGet, "/api/v1/developer/api-keys", TierBasic, "Phase-05 Task 5.3.16",
			"List API keys", authRead),
		v1(http.MethodDelete, "/api/v1/developer/api-keys/{id}", TierBasic, "Phase-05 Task 5.3.16",
			"Revoke API key", authUser),

		// ---- Tasks 5.3.17–5.3.19 ----
		v1(http.MethodPost, "/api/v1/webhooks", TierBasic, "Phase-05 Task 5.3.17",
			"Register webhook URL + events (HMAC-SHA256 signed delivery)", authUser),
		v1(http.MethodPost, "/api/v1/admin/chargebacks", TierBasic, "Phase-05 Task 5.3.18",
			"Create chargeback record (dispute workflow)", adminAuth(RoleFinanceOps)),
		v1(http.MethodGet, "/api/v1/tax/report", TierBasic, "Phase-05 Task 5.3.19",
			"Tax report with FIFO lot tracking (?year=)", authRead),

		// ---- Task 5.3.22 order-modify audit ----
		v1(http.MethodGet, "/api/v1/admin/orders/{id}/audit", TierBasic, "Phase-05 Task 5.3.22",
			"Order-modify audit trail (old/new field values)", adminAuth(RoleComplianceOfficer)),

		// ---- Task 5.3.23 internal transfers ----
		v1(http.MethodPost, "/api/v1/transfers", TierBasic, "Phase-05 Task 5.3.23",
			"Internal transfer (master↔sub / same-user) with GL posting", authTransfer),

		// ---- Tasks 5.3.26/5.3.31/§8.4 WebSocket surface ----
		{Method: "WS", Path: "/ws/v1", Version: "v1", Auth: authPublic,
			RateTier: TierPublic, Weight: 1, Owner: "Phase-05 Task 5.3.26 / Task 5.3.31",
			Status:      StatusStub,
			Description: "Unified interactive WebSocket: market data, private feeds, order.* request-response"},
		{Method: "WS", Path: "/ws/v1/marketdata", Version: "v1", Auth: authPublic,
			RateTier: TierPublic, Weight: 1, Owner: "Phase-05 Task 5.3.26 (legacy alias)",
			Status: StatusStub, Description: "Market data stream (legacy alias of /ws/v1)"},
		{Method: "WS", Path: "/ws/v1/orders", Version: "v1", Auth: authUser,
			RateTier: TierBasic, Weight: 1, Owner: "Phase-05 Task 5.3.26 (legacy alias)",
			Status: StatusStub, Description: "Private order stream (legacy alias of /ws/v1)"},

		// ---- Task 5.3.29 gateway health probes (R9 schema live via
		//      internal/api/health.go) ----
		{Method: http.MethodGet, Path: "/health/live", Version: "", Auth: authPublic,
			RateTier: TierExempt, Weight: 1, Owner: "Phase-05 Task 5.3.29", Status: StatusLive,
			Description: "Liveness probe (always 200 when process is up)"},
		{Method: http.MethodGet, Path: "/health/ready", Version: "", Auth: authPublic,
			RateTier: TierExempt, Weight: 1, Owner: "Phase-05 Task 5.3.29", Status: StatusLive,
			Description: "Readiness probe (PostgreSQL + Redis + NATS checks)"},

		// ---- Task 5.3.30 manual liquidation (dual control) ----
		{Method: http.MethodPost, Path: "/api/v1/admin/liquidation/manual", Version: "v1",
			Auth: adminAuth(RoleRiskManager), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-05 Task 5.3.30", Status: StatusStub, DualControl: true,
			Description: "Manual position liquidation (override_auction bypasses CALL)"},

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
		v1(http.MethodGet, "/api/v1/time", TierPublic, "Phase-05 Task 5.3.43",
			"PTP-sourced server time (UTC millis) for HMAC clock sync", authPublic),
		v1(http.MethodGet, "/api/v1/exchange-info", TierPublic, "Phase-05 Task 5.3.44",
			"Unified venue info: symbols, filters, permissions, rate_limits", authPublic),
		v1(http.MethodGet, "/api/v1/transfers", TierBasic, "Phase-05 Task 5.3.45",
			"Transfer history (cursor envelope, GL-linked)", authRead),

		// ---- Task 5.3.7 step 7: fleet / ops console / auditor ----
		v1(http.MethodGet, "/api/v1/admin/fleet/environments", TierBasic, "Phase-09 Task 9.3.30",
			"Fleet environments", adminAuth(RoleSuperAdmin)),
		v1(http.MethodGet, "/api/v1/admin/fleet/hosts", TierBasic, "Phase-09 Task 9.3.30",
			"Fleet hosts", adminAuth(RoleSuperAdmin)),
		{Method: http.MethodPost, Path: "/api/v1/admin/fleet/hosts/{id}/drain", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusStub, DualControl: true, Env: "env-scoped",
			Description: "Drain fleet host"},
		{Method: http.MethodPost, Path: "/api/v1/admin/fleet/hosts/{id}/cordon", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusStub, DualControl: true, Env: "env-scoped",
			Description: "Cordon fleet host"},
		{Method: http.MethodPost, Path: "/api/v1/admin/fleet/hosts/{id}/decommission", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusStub, DualControl: true, Env: "env-scoped",
			Description: "Decommission fleet host"},
		v1(http.MethodGet, "/api/v1/admin/fleet/topology", TierBasic, "Phase-09 Task 9.3.30",
			"Fleet topology (?env=)", adminAuth(RoleSuperAdmin)),
		v1(http.MethodGet, "/api/v1/admin/releases", TierBasic, "Phase-09 Task 9.3.30",
			"List releases", adminAuth(RoleSuperAdmin)),
		v1(http.MethodPost, "/api/v1/admin/releases", TierBasic, "Phase-09 Task 9.3.30",
			"Register release", adminAuth(RoleSuperAdmin)),
		{Method: http.MethodPost, Path: "/api/v1/admin/releases/{id}/promote", Version: "v1",
			Auth: adminAuth(RoleSuperAdmin), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-09 Task 9.3.30", Status: StatusStub, DualControl: true, Env: "env-scoped",
			Description: "Promote release to an environment (deploy-to-production four-eyes)"},
		v1(http.MethodGet, "/api/v1/admin/listing-proposals", TierBasic, "Phase-15 Task 15.3.12",
			"List instrument listing proposals", adminAuth(RoleRiskManager)),
		v1(http.MethodPost, "/api/v1/admin/listing-proposals", TierBasic, "Phase-15 Task 15.3.12",
			"Create listing proposal", adminAuth(RoleRiskManager)),
		v1(http.MethodPost, "/api/v1/admin/listing-proposals/{id}/review", TierBasic, "Phase-15 Task 15.3.12",
			"Review listing proposal", adminAuth(RoleRiskManager)),
		v1(http.MethodGet, "/api/v1/admin/ops-board", TierBasic, "Phase-15 Task 15.3.12",
			"Market-ops console board", adminAuth(RoleRiskManager)),
		v1(http.MethodGet, "/api/v1/admin/archive/status", TierBasic, "Phase-04 Task 4.3.2",
			"WAL archive status (?shard=)", adminAuth(RoleReadOnlyAuditor)),
		v1(http.MethodGet, "/api/v1/admin/audit/verify", TierBasic, "Phase-07 Task 7.3.3",
			"Audit hash-chain verification (?date=)", adminAuth(RoleReadOnlyAuditor)),

		// ---- §8.4 / client-surface rows with pinned owners (5.3.46 extends) ----
		v1(http.MethodPost, "/api/v1/funding/bank-accounts", TierBasic, "Phase-11 Task 11.3.7",
			"Register beneficiary bank account (withdrawal allowlist)", authUser),
		v1(http.MethodGet, "/api/v1/funding/bank-accounts", TierBasic, "Phase-11 Task 11.3.7",
			"List beneficiary bank accounts", authRead),
		v1(http.MethodDelete, "/api/v1/funding/bank-accounts", TierBasic, "Phase-11 Task 11.3.7",
			"Remove beneficiary bank account", authUser),
		v1(http.MethodPost, "/api/v1/funding/fee-estimate", TierBasic, "Phase-11 Task 11.3.9",
			"Funding fee estimate {amount, currency, rail, direction}", authRead),
		v1(http.MethodGet, "/api/v1/account/statements", TierBasic, "Phase-20 Task 20.3.6",
			"Client statements & trade confirmations (?period=)", authRead),
		{Method: http.MethodPost, Path: "/api/v1/admin/withdrawals/{id}/approve", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.2", Status: StatusStub, DualControl: true,
			Description: "Approve PENDING_REVIEW withdrawal (four-eyes)"},
		{Method: http.MethodPost, Path: "/api/v1/admin/withdrawals/{id}/reject", Version: "v1",
			Auth: adminAuth(RoleFinanceOps), RateTier: TierBasic, Weight: 1,
			Owner: "Phase-11 Task 11.3.2", Status: StatusStub, DualControl: true,
			Description: "Reject PENDING_REVIEW withdrawal (four-eyes)"},
		v1(http.MethodGet, "/api/v1/support/tickets", TierBasic, "Phase-07 Task 7.3.7",
			"List support tickets", authUser),
		v1(http.MethodPost, "/api/v1/support/tickets", TierBasic, "Phase-07 Task 7.3.7",
			"Open support ticket", authUser),
		v1(http.MethodGet, "/api/v1/session/status", TierPublic, "Phase-15 Task 15.3.7",
			"24/5 session lifecycle status (open/close windows)", authPublic),
		v1(http.MethodGet, "/api/v1/stats/24h", TierPublic, "Phase-11",
			"Venue-wide 24h stats", authPublic),
		v1(http.MethodGet, "/api/v1/stats/24h/{symbol}", TierPublic, "Phase-11",
			"Per-symbol 24h stats", authPublic),
		v1(http.MethodGet, "/api/v1/history/ticks/{symbol}", TierBasic, "Phase-23 Task 23.3.4",
			"Historical tick data (?from&to&limit&cursor; canonical §8.4 path)", authRead),
		v1(http.MethodGet, "/api/v1/account/api-keys", TierBasic, "Phase-12",
			"List programmatic API keys", authRead),
		v1(http.MethodPost, "/api/v1/account/api-keys", TierBasic, "Phase-12",
			"Create programmatic API key", authUser),
		v1(http.MethodDelete, "/api/v1/account/api-keys/{id}", TierBasic, "Phase-12",
			"Revoke programmatic API key", authUser),
		v1(http.MethodGet, "/api/v1/account/profile", TierBasic, "Phase-12",
			"Account profile", authRead),
		v1(http.MethodPut, "/api/v1/account/profile", TierBasic, "Phase-12",
			"Update account profile", authUser),
		v1(http.MethodPost, "/api/v1/account/emergency-freeze", TierBasic, "Phase-12",
			"Client-initiated emergency account freeze", authUser),
		v1(http.MethodPost, "/api/v1/account/close", TierBasic, "Phase-14 Task 14.3.9",
			"Account closure/offboarding (blocked while positions/orders pending)", authUser),
		v1(http.MethodPost, "/api/v1/auth/login", TierPublic, "Phase-12",
			"Password login → JWT pair", authPublic),
		v1(http.MethodPost, "/api/v1/auth/logout", TierBasic, "Phase-12",
			"Logout (revoke session)", authUser),
		v1(http.MethodPost, "/api/v1/auth/refresh", TierPublic, "Phase-12",
			"Refresh-token → new access token", authPublic),
		v1(http.MethodPost, "/api/v1/auth/register", TierPublic, "Phase-12",
			"Account registration", authPublic),
		v1(http.MethodPost, "/api/v1/kyc/submit", TierBasic, "Phase-14",
			"Submit KYC documents", authUser),
		v1(http.MethodGet, "/api/v1/kyc/status", TierBasic, "Phase-14",
			"KYC verification status", authRead),
	}
	return r
}

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
			h = http.HandlerFunc(r.OpenAPIHandler)
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
