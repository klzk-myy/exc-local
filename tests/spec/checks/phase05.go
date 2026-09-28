package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-05 order-gateway/API checkpoints.
//
// Wave 1 landed the foundations (auth, rate limiting, accounts, error
// registry, route registry); Wave 2 landed the endpoint clusters (orders,
// funding, market surface, platform, WS trading/admin). Bindings point at
// the real packages and migrations — gateway tests run against live
// PG/Redis when the harness is pointed at endpoints (EXC_TEST_DSN et al).
// Phase-05 package paths under services/ for gotest bindings.
const (
	p05Auth       = "./internal/auth"
	p05Ratelimit  = "./internal/ratelimit"
	p05Accounts   = "./internal/accounts"
	p05Errs       = "./internal/errs"
	p05Gateway    = "./internal/gateway"
	p05API        = "./internal/api"
	p05Orders     = "./internal/orders"
	p05Funding    = "./internal/funding"
	p05Marketapi  = "./internal/marketapi"
	p05WS         = "./internal/ws"
	p05Webhooks   = "./internal/webhooks"
	p05Promos     = "./internal/promos"
	p05Tax        = "./internal/tax"
	p05Deprec     = "./internal/deprecation"
	p05Middleware = "./internal/middleware"
	p05Testenv    = "./internal/testenv"
)

func registerPhase05(r *spec.Registry) {

	// ---- Wave 1: foundations ----------------------------------------------
	r.Register("P05-T5.3.1-C1", ckP05JWT,
		"JWT 15min access / 7d refresh")
	r.Register("P05-T5.3.1-C2", ckP05OAuth2,
		"OAuth2 client credentials")
	r.Register("P05-T5.3.2-C1", ckP05RateTiers,
		"5 rate limit tiers")
	r.Register("P05-T5.3.7-C1", ckP05RouteRegistry,
		"central route registry for all endpoints")
	r.Register("P05-T5.3.9-C1", ckP05IPAllowlist,
		"API token IP allowlist")
	r.Register("P05-T5.3.10-C1", ckP05SessionLimits,
		"concurrent session limits")
	r.Register("P05-T5.3.11-C1", ckP05SubAccounts,
		"sub-account max 20 per master (tiered up to 1,000 for institutional)")
	r.Register("P05-T5.3.12-C1", ckP05Frozen,
		"FROZEN legal-hold state")
	r.Register("P05-T5.3.21-C1", ckP05ErrorRegistry,
		"central error code registry")
	r.Register("P05-T5.3.24-C1", ckP05HMAC,
		"HMAC request signing + 30s replay window (§8.1, §24 #147)")
	r.Register("P05-T5.3.27-C1", ckP05RateHeaders,
		"Standard HTTP rate-limit headers (§8.3, §24 #192)")
	r.Register("P05-T5.3.33-C1", ckP05DeadMan,
		"dead-man switch is shared across REST/WS/FIX and cancels atomically")
	r.Register("P05-T5.3.34-C1", ckP05ProgressiveBans,
		"repeated post-429 abuse escalates to auditable timed HTTP 418 bans")
	r.Register("P05-T5.3.36-C1", ckP05CloseAll,
		"close-all uses reduce-only protected closes and reports partial failures")
	r.Register("P05-T5.3.38-C1", ckP05AsymmetricKeys,
		"Ed25519/RSA authentication across REST/WS; private keys never leave client")

	// ---- Wave 2: endpoint clusters -----------------------------------------
	r.Register("P05-T5.3.3-C1", ckP05OrderPipeline,
		"order submission via Aeron/SHM to C++ core")
	r.Register("P05-T5.3.4-C1", ckP05AccountEndpoints,
		"account/balance/position endpoints")
	r.Register("P05-T5.3.5-C1", ckP05MarketData,
		"market data REST with caching")
	r.Register("P05-T5.3.6-C1", ckP05DepositInstructions,
		"deposit instructions per currency")
	r.Register("P05-T5.3.6-C2", ckP05WithdrawalWindow,
		"15min withdrawal confirmation window")
	r.Register("P05-T5.3.8-C1", ckP05OpenAPIPortal,
		"OpenAPI docs at /developer")
	r.Register("P05-T5.3.13-C1", ckP05TestEnv,
		"test environment with reset")
	r.Register("P05-T5.3.14-C1", ckP05Announcements,
		"announcements + maintenance calendar")
	r.Register("P05-T5.3.15-C1", ckP05FeePromos,
		"fee promo windows")
	r.Register("P05-T5.3.16-C1", ckP05DevPortal,
		"developer portal with API keys")
	r.Register("P05-T5.3.17-C1", ckP05Webhooks,
		"signed webhooks with retry")
	r.Register("P05-T5.3.18-C1", ckP05Chargebacks,
		"chargeback dispute workflow")
	r.Register("P05-T5.3.19-C1", ckP05Tax,
		"tax reporting with FIFO lot tracking")
	r.Register("P05-T5.3.20-C1", ckP05Deprecation,
		"6-month deprecation notice")
	r.Register("P05-T5.3.22-C1", ckP05OrderAudit,
		"order-modify audit trail with old/new fields in order_audit table")
	r.Register("P05-T5.3.22-C2", ckP05StaleModify,
		"STALE_MODIFY rejects stale order_seq on modify")
	r.Register("P05-T5.3.23-C1", ckP05Transfers,
		"internal transfer endpoint with GL posting (§8.4, §24 #144)")
	r.Register("P05-T5.3.24-C2", ckP05IdempotentSubmit,
		"idempotent order submission on client_order_id (§8.4, §24 #148)")
	r.Register("P05-T5.3.25-C1", ckP05MassCancel,
		"per-instrument mass cancellation (§8.4, §24 #153)")
	r.Register("P05-T5.3.26-C1", ckP05WSAuthRenewal,
		"WebSocket authentication upgrade and renewal (§10.5, §24 #187)")
	r.Register("P05-T5.3.29-C1", ckP05GatewayArch,
		"API gateway architecture")
	r.Register("P05-T5.3.30-C1", ckP05ManualLiquidation,
		"manual liquidation admin endpoint")
	r.Register("P05-T5.3.31-C1", ckP05WSTrading,
		"Interactive WebSocket Trading API with correlated request-response frames")
	r.Register("P05-T5.3.32-C1", ckP05BatchOrders,
		"REST batch orders submit and cancel endpoints")
	r.Register("P05-T5.3.35-C1", ckP05StructuredFilters,
		"instrument responses expose all effective structured validation filters")
	r.Register("P05-T5.3.37-C1", ckP05AmendAtomic,
		"atomic cancel-replace and quantity-down keep-priority operations")
	r.Register("P05-T5.3.39-C1", ckP05QuoteMarketPreview,
		"quote-denominated market orders and side-effect-free order preview")
	r.Register("P05-T5.3.40-C1", ckP05RateIntrospection,
		"clients can query weighted request/order usage and account limits")
	r.Register("P05-T5.3.41-C1", ckP05ErrorEnvelopeBreaker,
		"RFC 7807 error envelope, circuit breaker, and engine timeout fallback")
	r.Register("P05-T5.3.42-C1", ckP05ListEnvelopeIdem,
		"unified list envelope, cross-endpoint idempotency, auth lifecycle, rate weights")
	r.Register("P05-T5.3.43-C1", ckP05ServerTime,
		"public PTP-sourced server-time endpoint for HMAC clock sync (§24 #355)")
	r.Register("P05-T5.3.44-C1", ckP05VenueInfo,
		"unified venue-info document with ETag and change signaling (§24 #356)")
	r.Register("P05-T5.3.45-C1", ckP05TransferHistory,
		"paginated transfer history with GL linkage (§24 #363)")
	r.Register("P05-T5.3.46-C1", ckP05SchemaValidation,
		"centralized OpenAPI-schema request validation + route-registry completeness")
	r.Register("P05-T5.3.46-C2", ckP05AllRoutesRegistered,
		"route registration for ALL endpoints (spec §8.4 conventions)")
}

// ---------------------------------------------------------------------------
// Wave 1 — foundations
// ---------------------------------------------------------------------------

var ckP05JWT = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Auth, "TestJWTRoundTrip|TestJWTExpiredRejected|TestJWTAlgConfusionRejected|TestJWTRS256AndEdDSA"))
}

var ckP05OAuth2 = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/151_oauth_clients.up.sql"),
		gotest(p05Auth, "TestOAuthClientCredentialsGrant"))
}

var ckP05RateTiers = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Ratelimit, "TestRedisHitAndKeys|TestRedisBurstAndDeny"),
		structural(env, "services/internal/ratelimit/tier.go", "Tier"))
}

var ckP05RouteRegistry = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Gateway, "TestSeedMountsAndDumps|TestRoutesEndpoint"))
}

var ckP05IPAllowlist = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Auth, "TestIPAllowlistIPv4AndCIDR|TestIPAllowlistIPv6|TestIPAllowlistMalformedFailsClosed"))
}

var ckP05SessionLimits = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Auth, "TestSessionAccountCapEvictsOldest|TestSessionIPCapEvictsOldest|TestSessionRefreshRotationAndReuseDetection"))
}

var ckP05SubAccounts = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/067_accounts_subaccount_limit.up.sql"),
		gotest(p05Accounts, "TestIntegrationSubAccountLifecycle|TestIntegrationSubAccountCeiling|TestIntegrationSubAccountNestingDenied"))
}

var ckP05Frozen = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/152_account_freeze_events.up.sql"),
		gotest(p05Accounts, "TestIntegrationFreezeLifecycle|TestIntegrationFreezeRoleGate|TestFreezeHandlerRequiresRoleAndApprover"))
}

var ckP05ErrorRegistry = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Errs, "TestSpecTableSizeAndUniqueness|TestEmissionGate|TestRegistryLoad"))
}

var ckP05HMAC = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Auth, "TestSignedRequestReplayAndWindow|TestHMACKeyLifecycleAndVerify"))
}

var ckP05RateHeaders = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Middleware, "TestRateHeadersOnSuccess|Test429EnvelopeAndRetryAfter|Test418BanWithExpiresHeader"))
}

var ckP05DeadMan = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Accounts, "TestRedisCountdownArmDisarmPop|TestCountdownExpiryTriggersMassCancel|TestCountdownStrictStartRejectsActive"))
}

var ckP05ProgressiveBans = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Ratelimit, "TestRedisBanEscalationAndTTL|TestRedisAllowlistSkipsBanMachinery"),
		gotest(p05API, "TestAdminBanListAndAudit"))
}

var ckP05CloseAll = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Accounts, "TestCloseAllCancelsThenCloses|TestCloseAllPartialFailure|TestCloseAllFrozenAccountRejected"))
}

var ckP05AsymmetricKeys = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/073_api_key_asymmetric_types.up.sql"),
		gotest(p05Auth, "TestEd25519KeyEndToEnd|TestRSAKeyEndToEnd|TestKeyRotationOverlap"))
}

// ---------------------------------------------------------------------------
// Wave 2 — endpoint clusters
// ---------------------------------------------------------------------------

var ckP05OrderPipeline = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/orders/ipc.go",
			"services/internal/db/migrations/155_order_pipeline_columns.up.sql"),
		gotest(p05Orders, "TestSubmitHappyPathAndDedup|TestSubmitMarksRejectedOnDispatchFailure"))
}

var ckP05AccountEndpoints = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05API, "TestAccountBalances_Happy|TestAccountBalances_ForeignAccountRejected"))
}

var ckP05MarketData = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Marketapi, "TestCacheTTLAndErrorPassthrough"),
		gotest(p05API, "TestBookSnapshotEndpoint|TestTradesEndpoint|TestKlinesEndpoint|TestInstrumentsEndpoint"))
}

var ckP05DepositInstructions = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05API, "TestDepositInstructions"))
}

var ckP05WithdrawalWindow = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/funding/funding.go", "WithdrawalConfirmWindow"),
		gotest(p05Funding, "TestWithdrawalValidation|TestITWithdrawalLifecycle|TestITWithdrawalExpirySweep"))
}

var ckP05OpenAPIPortal = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/openapi/openapi.json"),
		gotest(p05API, "TestOpenAPIDocument|TestDeveloperPortal"))
}

var ckP05TestEnv = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05API, "TestTestResetHandler"),
		gotest(p05Testenv, "TestEnabledFailClosed|Test"))
}

var ckP05Announcements = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/171_announcements.up.sql",
			"services/internal/db/migrations/172_maintenance_windows.up.sql"),
		gotest(p05API, "TestAnnouncementCRUD|TestMaintenanceScheduleCRUD"))
}

var ckP05FeePromos = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/181_fee_promo_windows.up.sql"),
		gotest(p05Promos, "TestIntegrationPromoLifecycle|TestCreateValidation|TestValidRateBounds"))
}

var ckP05DevPortal = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05API, "TestDeveloperPortal|TestDecodePublicKey"))
}

var ckP05Webhooks = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/180_webhooks.up.sql"),
		gotest(p05Webhooks, "TestSignKAT|TestIntegrationEnqueueClaimComplete|TestIntegrationDispatcherRetry|TestIntegrationDeadLetter"))
}

var ckP05Chargebacks = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/162_chargebacks.up.sql"),
		gotest(p05API, "TestAdminChargeback_CreateHappy|TestAdminChargeback_ScopeGate"))
}

var ckP05Tax = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Tax, "TestComputeFIFO|TestComputeLIFO|TestComputeHIFO|TestServiceReport|TestRenderCSV"))
}

var ckP05Deprecation = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/182_api_deprecations.up.sql"),
		gotest(p05Deprec, "TestMiddlewareAnnounced|TestMiddlewareGone|TestIntegrationDeprecations"))
}

var ckP05OrderAudit = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/153_order_audit.up.sql"),
		gotest(p05Orders, "TestModifyStaleAndCAS|TestModifyRollbackOnDispatchFailure"))
}

var ckP05StaleModify = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Orders, "TestStaleModify"))
}

var ckP05Transfers = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/161_internal_transfers.up.sql"),
		gotest(p05Funding, "TestTransferHappyPath|TestTransferIdempotentReplay|TestTransferCrossUserRejected"))
}

var ckP05IdempotentSubmit = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/154_order_idempotency.up.sql"),
		gotest(p05Orders, "TestSubmitHappyPathAndDedup"),
		gotest(p05Middleware, "TestIdemReplayAndMismatch|TestIdemInFlightCollision"))
}

var ckP05MassCancel = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Orders, "TestMassCancelScoped|TestNormalizeMassCancelScope"))
}

var ckP05WSAuthRenewal = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05WS, "TestAuthExpiryDemotesWithPublicSubs|TestAuthExpiryCloses4019WithoutPublicSubs|TestRefreshRequiresAuth"))
}

var ckP05GatewayArch = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "deploy/haproxy/haproxy.cfg", "deploy/haproxy/README.md"),
		structural(env, "deploy/haproxy/haproxy.cfg", "backend", "frontend"))
}

var ckP05ManualLiquidation = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/190_manual_liquidations.up.sql"),
		gotest(p05API, "TestLiquidationHappyPath|TestLiquidationDualControl|TestLiquidationNilSinkFailsClosed"))
}

var ckP05WSTrading = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05WS, "TestAdapterCancelRoutesAndStampsIdentity|TestAdapterRequiresAccountBoundSession|TestPrivateChannelRequiresAuth|TestUnauthenticatedOrderFrameCloses4019"))
}

var ckP05BatchOrders = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Orders, "TestBatchSubmitAtomicity|TestBatchCancelResolvesClientIDs|TestBatchRateLimitGate"))
}

var ckP05StructuredFilters = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/170_instrument_filter_columns.up.sql"),
		gotest(p05Marketapi, "TestInstrumentFiltersEmitAllSix"),
		gotest(p05API, "TestAccountFilters"))
}

var ckP05AmendAtomic = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Orders, "TestCancelReplaceAtomicAmend|TestAmendKeepPriorityService|TestValidateKeepPriority"))
}

var ckP05QuoteMarketPreview = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Orders, "TestSubmitQuoteMarketConvertsToBase|TestDryRunNoSideEffects|TestValidateSubmitQuoteQuantityRules"))
}

var ckP05RateIntrospection = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05API, "TestAccountRateLimits|TestAccountRateLimitsForeignAccountForbidden|TestTierResolverFromLookup"))
}

var ckP05ErrorEnvelopeBreaker = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Gateway, "TestBreakerTripsAbove15Percent|TestBreakerHalfOpenProbe|TestEngineTimeoutBudgetIsSpecValue|TestEnvelopeShape"))
}

var ckP05ListEnvelopeIdem = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05API, "TestListEnvelopeNextCursor|TestParseListParamsDefaultsAndBounds|TestDecodeCursorRejectsGarbage"))
}

var ckP05ServerTime = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05API, "TestServerTime"))
}

var ckP05VenueInfo = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Marketapi, "TestVenueETagStableAndSensitive|TestVenueDocShape"),
		gotest(p05API, "TestExchangeInfoETagAnd304"))
}

var ckP05TransferHistory = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/funding/history.go", "journal_entry_id"),
		gotest(p05API, "TestTransferHistory_FilterMapping"),
		gotest(p05Funding, "TestITTransferHistory"))
}

var ckP05SchemaValidation = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Gateway, "TestBodySchemaValidation|TestRouteRegistryCoversDeclaredPaths"),
		gotest(p05API, "TestOpenAPIDocument"))
}

var ckP05AllRoutesRegistered = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p05Gateway, "TestRouteRegistryCoversDeclaredPaths|TestSeedMountsAndDumps"))
}
