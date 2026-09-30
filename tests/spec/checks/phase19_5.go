package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-19.5 Price Oracle & Mark Price checkpoints (Tasks
// 19.5.3.1–19.5.3.7). Evidence: feed adapters + median/VWAP
// aggregation + staleness/divergence gates + Redis publisher under
// services/internal/oracle; yield curves + forward points under
// services/internal/oracle/rates; vendor adapters under
// services/internal/oracle/feeds; risk-side stale-fallback +
// flash-crash cooling under services/internal/risk; settlement
// swap-point adapter under services/internal/settlement; gateway
// wiring (oracle-backed mark provider, order admission gate, health
// metric + alert) under services/cmd/gateway/main.go; oracle binary
// under services/cmd/oracle/main.go. Redis-gated legs run under
// EXC_REDIS_TEST=1.
const (
	p195Oracle      = "./internal/oracle"
	p195OracleRates = "./internal/oracle/rates"
	p195Risk        = "./internal/risk"
)

func registerPhase195(r *spec.Registry) {
	r.Register("P19.5-T19.5.3.1-C1", ckP195FeedIntegration,
		"Refinitiv + BFIX + ECB feed adapters, ≥2 independent sources — defined first, validated against spec")
	r.Register("P19.5-T19.5.3.2-C1", ckP195MarkPrice,
		"median mark + volume-weighted index, 1s cadence, Redis+Aeron publication — defined first, validated against spec")
	r.Register("P19.5-T19.5.3.3-C1", ckP195Staleness,
		"5s staleness gate, <2 fresh feeds fails closed, per-symbol health cascade — defined first, validated against spec")
	r.Register("P19.5-T19.5.3.4-C1", ckP195SingleConsumer,
		"single PriceOracle — margin/liquidation/halt consumers share one Redis seam — defined first, validated against spec")
	r.Register("P19.5-T19.5.3.5-C1", ckP195YieldCurves,
		"fiat yield curves ≥7 tenors + Tom-Next/forward points + STALE_FORWARD_POINTS — defined first, validated against spec")
	r.Register("P19.5-T19.5.3.6-C1", ckP195LiquidationFallback,
		"stale-price liquidation fallback (2/5/10% haircuts) + flash-crash 5s cooling — defined first, validated against spec")
	r.Register("P19.5-T19.5.3.7-C1", ckP195Divergence,
		"25bps divergence exclusion + <2 coherent feeds suspends marks with PRICE_ORACLE_UNAVAILABLE — defined first, validated against spec")
}

// --- Task 19.5.3.1: Oracle Feed Integration -----------------------------------

func ckP195FeedIntegration(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/oracle/feeds/feeds.go",
			"services/internal/oracle/feeds/http.go",
			"services/cmd/oracle/main.go"),
		structural(env, "services/internal/oracle/feeds/http.go",
			"refinitiv", "bfix", "ecb"),
		structural(env, "services/internal/oracle/feeds/feeds.go",
			"NewSimFeed", "Poll"),
		structural(env, "services/internal/oracle/oracle.go",
			"MinFeeds = 2"),
		gotest(p195Oracle, "TestServiceRequiresTwoFeeds|TestFeedErrorCounted|TestPerSymbolFreshness"),
	)
}

// --- Task 19.5.3.2: Mark Price Computation ------------------------------------

func ckP195MarkPrice(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/oracle/mark_price.go",
			"services/internal/oracle/publisher.go",
			"services/internal/oracle/service.go"),
		structural(env, "services/internal/oracle/mark_price.go",
			"median", "vwap"),
		structural(env, "services/internal/oracle/publisher.go",
			"mark_price:", "index_price:", "AeronSink", "TickSink"),
		structural(env, "services/internal/oracle/aeron_sink.go",
			"224\\.0\\.1\\.1:40456", "AeronMarkSink", "Offer"),
		structural(env, "services/cmd/oracle/main.go",
			"ipcaeron.Connect", "AddPublication"),
		structural(env, "services/internal/oracle/service.go",
			"PublishCadence"),
		gotest(p195Oracle, "TestMedianOfTwoFeeds|TestIndexVolumeWeighted|TestNonPositiveQuoteRejected"),
	)
}

// --- Task 19.5.3.3: Staleness Gates -------------------------------------------

func ckP195Staleness(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/oracle/staleness.go",
			"services/internal/oracle/consumer.go",
			"services/cmd/gateway/main.go"),
		structural(env, "services/internal/oracle/oracle.go",
			"StaleAfter", `5 \* time\.Second`),
		structural(env, "services/internal/oracle/oracle.go",
			"HealthUnavailable", "HealthDegraded"),
		structural(env, "services/internal/oracle/consumer.go",
			"oracle:health:", "SymbolHealth"),
		structural(env, "services/cmd/gateway/main.go",
			"WithOracleGate", "PRICE_ORACLE_UNAVAILABLE", "exchange_oracle_health"),
		gotest(p195Oracle, "TestStalenessGate|TestAllStaleUnavailable"),
	)
}

// --- Task 19.5.3.4: Single PriceOracle Consumer --------------------------------

func ckP195SingleConsumer(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/oracle/provider_risk.go",
			"services/internal/oracle/consumer.go"),
		structural(env, "services/internal/oracle/provider_risk.go",
			"MarkPriceProvider"),
		structural(env, "services/cmd/gateway/main.go",
			"NewChainedMarkPriceProvider", "oracle.NewProvider", "NewRedisStaleFallbackSource"),
		gotest(p195Oracle, "TestRedisProviderReadAndGate"),
	)
}

// --- Task 19.5.3.5: Interest-Rate / Yield-Curve Feeds --------------------------

func ckP195YieldCurves(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/oracle/rates/rates.go",
			"services/internal/oracle/rates/json.go",
			"services/internal/settlement/oracle_swap_feed.go"),
		structural(env, "services/internal/oracle/rates/rates.go",
			"curve:", "fwd_points:", "ErrStaleForwardPoints", "Basis360", "Basis365"),
		structural(env, "services/internal/settlement/oracle_swap_feed.go",
			"OracleSwapRateFeed"),
		gotest(p195OracleRates, "TestDayCountConventions|TestCurveCompleteness|"+
			"TestCurvePillarAndInterpolation|TestCurvePublishRefusesStaleAndIncomplete"),
	)
}

// --- Task 19.5.3.6: Stale-Price Liquidation Fallback & Flash-Crash -------------

func ckP195LiquidationFallback(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/oracle/staleness_fallback.go",
			"services/internal/risk/liquidation_fallback.go",
			"services/internal/risk/liquidation_fallback_test.go",
			"services/internal/db/migrations/236_liquidation_basis.up.sql"),
		structural(env, "services/internal/oracle/staleness_fallback.go",
			"FallbackTracker", "FlashMoveThreshold", "oracle:fallback:"),
		structural(env, "services/internal/risk/liquidation_fallback.go",
			"StaleFallbackSource", "TierStaleShort", "TierStaleMedium", "TierStaleLong",
			"STALE_MARK"),
		structural(env, "services/internal/risk/liquidation.go",
			"StaleFallback", "Basis"),
		structural(env, "services/internal/db/migrations/236_liquidation_basis.up.sql",
			"liquidation_basis", "STALE_MARK"),
		gotest(p195Oracle, "TestTierLadder|TestFallbackTrackerStaleReference|"+
			"TestFlashCrashFreeze|TestFlashCrashNotArmedOnSlowMove"),
		gotest(p195Risk, "TestStaleRefPricingDirection|TestStaleFallbackFrozenDefers|"+
			"TestStaleFallbackFreshPassthrough|TestStaleFallbackTiersMirror"),
	)
}

// --- Task 19.5.3.7: Divergence & Fail-Closed -----------------------------------

func ckP195Divergence(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/oracle/staleness.go",
			"services/internal/errs/codes.go"),
		structural(env, "services/internal/oracle/oracle.go",
			"DivergenceBps int64 = 25"),
		structural(env, "services/internal/oracle/mark_price.go",
			"excludeDivergent"),
		structural(env, "services/internal/errs/codes.go",
			"ORACLE_FEED_STALE", "ORACLE_DIVERGENCE_EXCEEDED", "MARK_PRICE_STALE",
			"MARK_PRICE_OUT_OF_BOUNDS", "STALE_FORWARD_POINTS", "PRICE_ORACLE_UNAVAILABLE"),
		gotest(p195Oracle, "TestDivergenceExclusion|TestAllStaleUnavailable"),
	)
}
