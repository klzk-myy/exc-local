package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-23 Market-Data Products checkpoints (Tasks 23.3.1–23.3.11; 11
// total). Evidence: services/internal/marketdata/{historical,
// tick_data_api, export, export_csv, export_store, premium, greeks_feed,
// sentiment, oi, block_trades, history_guards, swap_rates, positioning,
// performance}.go, services/internal/api/handlers_{history,export,
// block_trades,market_stats,swap_rates}.go, gateway wiring in
// cmd/gateway/main.go + cmd/marketdata/{main,producers}.go, PG
// migrations 256/257, ClickHouse schema 008/009, route registry in
// internal/gateway/routes_v1.go. Gated legs self-skip.
const (
	p23MD  = "./internal/marketdata"
	p23API = "./internal/api"
	p23Mig = "services/internal/db/migrations"
	p23CH  = "deploy/clickhouse/schema"
)

func registerPhase23(r *spec.Registry) {
	r.Register("P23-T23.3.1-C1", ckP23HistoricalAPI,
		"historical data API with partitioning — defined first, validated against spec")
	r.Register("P23-T23.3.2-C1", ckP23DataExport,
		"data export CSV/JSON/Parquet — defined first, validated against spec")
	r.Register("P23-T23.3.3-C1", ckP23PremiumFeeds,
		"premium data feeds — defined first, validated against spec")
	r.Register("P23-T23.3.4-C1", ckP23TickHistory,
		"historical tick data API — defined first, validated against spec")
	r.Register("P23-T23.3.5-C1", ckP23GreeksFeed,
		"real-time Greeks feed — defined first, validated against spec")
	r.Register("P23-T23.3.6-C1", ckP23Sentiment,
		"sentiment analytics enforce delay and minimum-cohort privacy (§24 #276) — defined first, validated against spec")
	r.Register("P23-T23.3.7-C1", ckP23BlockTrades,
		"historical block-trade API preserves publication delay, anonymity, and correction lineage (§24 #291) — defined first, validated against spec")
	r.Register("P23-T23.3.8-C1", ckP23HistoryGuards,
		"Historical market data queries enforce query timeouts, cache frequent requests, and mask pre-open participant data (§24 #325) — defined first, validated against spec")
	r.Register("P23-T23.3.9-C1", ckP23SwapRates,
		"swap-rate history reconciled to accrual journals (§24 #358) — defined first, validated against spec")
	r.Register("P23-T23.3.10-C1", ckP23TakerPositioning,
		"taker-volume and positioning ratios with delay/cohort guards (§24 #359) — defined first, validated against spec")
	r.Register("P23-T23.3.11-C1", ckP23VenuePerformance,
		"aggregate-only public performance statistics reconciled to TCA/SLO (§24 #380) — defined first, validated against spec")
}

// ---------------------------------------------------------------------------

func ckP23HistoricalAPI(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/historical.go",
			"services/internal/api/handlers_history.go"),
		structural(env, "services/internal/marketdata/historical.go",
			"TradeHistoryStore", "TradeHistoryQuery"),
		structural(env, "services/internal/api/handlers_history.go",
			"HistoryTrades", "historyKlinesSpec"),
		gotest(p23MD,
			"TestTradeHistoryStoreQuerySQL|TestTradeHistoryStoreDefaultsAndScan|"+
				"TestTradeHistoryStoreFailClosed|TestHistoryAccessForRateTier|"+
				"TestRateTierHistoryResolver|TestResolveHistoryWindow|"+
				"TestHistoryDelayFor|TestHistoryFormatFor"),
	)
}

func ckP23DataExport(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/export.go",
			"services/internal/marketdata/export_csv.go",
			"services/internal/marketdata/export_store.go",
			"services/internal/api/handlers_export.go",
			p23Mig+"/256_data_export_jobs.up.sql"),
		structural(env, "services/internal/marketdata/export.go",
			"ExportService", "Admit", "RunDue"),
		structural(env, "services/internal/api/handlers_export.go",
			"HistoryTradesExport", "ExportJobDownload"),
		gotest(p23MD,
			"TestExportValidate|TestExportAdmit|TestExportNeedsAsync|"+
				"TestExportStreamCSV|TestExportStreamJSONEnvelope|"+
				"TestExportStreamParquet|TestExportKeysetBatches|"+
				"TestExportTruncated|TestExportAsyncLifecycle|"+
				"TestExportNotifyRetry|TestExportDownloadForAccount"),
		gotest(p23API,
			"TestExportSyncCSV|TestExportSyncJSON|TestExportAsyncFlag|"+
				"TestExportAsyncByBound|TestExportLimitBoundary|"+
				"TestExportJobStatusOwnerScoped|TestExportJobListOwnOnly"),
	)
}

func ckP23PremiumFeeds(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/premium.go",
			p23Mig+"/257_premium_feed_subscriptions.up.sql"),
		structural(env, "services/internal/marketdata/premium.go",
			"FeedEntitlements", "PremiumL3Producer", "FullDepthProducer",
			"AuctionsProducer", "PremiumFeedBiller"),
		structural(env, "services/internal/marketdata/channels.go",
			"premium_l3", "depth_full", "auction"),
		structural(env, "services/cmd/marketdata/producers.go",
			"PremiumL3", "FullDepth", "Auctions"),
		gotest(p23MD,
			"TestPremiumChannelsRegistered|TestFeedEntitlement_Gates|"+
				"TestFeedEntitlement_FailsClosed|TestFeedEntitlement_TierResolver|"+
				"TestChainEntitlements_FirstDenyWins|"+
				"TestPremiumL3Producer_PublishesWithL3Seq|"+
				"TestFullDepthProducer_AllLevels|"+
				"TestAuctionsProducer_FiltersAndGates|"+
				"TestPremiumBiller_PostsBalancedFeeJournal|"+
				"TestPremiumBiller_FailsClosedWithoutPoster|"+
				"TestPremiumBiller_PostFailureKeepsRowUnbilled|TestCcyExponent"),
	)
}

func ckP23TickHistory(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/tick_data_api.go",
			"services/internal/api/handlers_history.go"),
		structural(env, "services/internal/marketdata/tick_data_api.go",
			"HistoryAccessForRateTier", "WriteTicksCSV", "WriteTicksFIXDropCopy"),
		gotest(p23MD,
			"TestWriteTicksCSV|TestWriteTicksFIXDropCopy|"+
				"TestHistoryDelayFor|TestHistoryFormatFor"),
	)
}

func ckP23GreeksFeed(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/greeks_feed.go",
			p23CH+"/008_greeks_snapshots.sql"),
		structural(env, "services/internal/marketdata/greeks_feed.go",
			"GreeksFeed", "OptionsGreeksPricer", "CHGreeksStore",
			"JetStreamGreeksPublisher", "FeedGreeks"),
		structural(env, "services/cmd/marketdata/producers.go",
			"greeksInputSource", "NewGreeksFeed"),
		gotest(p23MD,
			"TestGreeksFeed_PublishesGKMatrix|"+
				"TestGreeksFeed_StaleFreezesNeverFabricates|"+
				"TestGreeksFeed_LatticeModelForAmerican|"+
				"TestGreeksFeed_SnapshotStoreRowShape|"+
				"TestGreeksFeed_CadenceAndGateDefaults|"+
				"TestGreeksFeed_NoContractsPublishesEmptyMatrix"),
	)
}

func ckP23Sentiment(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/sentiment.go",
			"services/internal/marketdata/oi.go",
			"services/internal/api/handlers_market_stats.go"),
		structural(env, "services/internal/marketdata/sentiment.go",
			"SentimentProducer", "SentimentPublicationDelay",
			"SentimentMinCohortAccounts", "PgxPositionCohortSource"),
		structural(env, "services/internal/api/handlers_market_stats.go",
			"AnalyticsOpenInterest", "AnalyticsLongShortRatio",
			"INSUFFICIENT_COHORT"),
		gotest(p23MD,
			"TestSentimentChannelParses|TestSentimentDelayEnforced|"+
				"TestSentimentCohortSuppressed|TestSentimentFramePrivacy|"+
				"TestLongShortSeries|TestLatestCohortHorizon|"+
				"TestAggregateCohortConcentration|TestOIProducerLatest|"+
				"TestOI_EmitsPositionAggregate|TestOI_ConfirmedZeroIsNotStale"),
		gotest(p23API,
			"TestOpenInterestEndpoint|TestOpenInterestNoDataMarksInsufficient|"+
				"TestLongShortRatioDelayEnforced|TestLongShortRatioPremiumRealtime|"+
				"TestLongShortRatioCohortFloor422|TestMarketPositioning|"+
				"TestMarketPositioningSuppressed"),
	)
}

func ckP23BlockTrades(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/block_trades.go",
			"services/internal/api/handlers_block_trades.go",
			p23CH+"/009_block_trades_tape.sql"),
		structural(env, "services/internal/marketdata/block_trades.go",
			"BlockTapeStore", "blockTapeFrom"),
		structural(env, "services/internal/api/handlers_block_trades.go",
			"HistoryBlockTrades", "historyBlockTradesSpec"),
		gotest(p23MD,
			"TestBlockTapeStoreQuerySQL|TestBlockTapeStoreLineageAnnotation|"+
				"TestBlockTapeStoreRecord|TestBlockTapeStoreRecordCorrection|"+
				"TestBlockTapeStoreFailClosed|TestBlockTapeProjectionAnonymous|"+
				"TestWriteBlockTapeCSV"),
		gotest(p23API,
			"TestHistoryBlockTrades|TestHistoryBlockTradesFreeTierDelay|"+
				"TestHistoryBlockTradesPremiumRealtime|"+
				"TestHistoryBlockTradesCursorRoundTrip|"+
				"TestHistoryBlockTradesCSV|TestHistoryBlockTradesTimeoutAndOutage|"+
				"TestHistoryBlockTradesUnknownSymbolAndNotConfigured|"+
				"TestHistoryBlockTradesClosedIntervalCache"),
	)
}

func ckP23HistoryGuards(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/marketdata/history_guards.go"),
		structural(env, "services/internal/marketdata/history_guards.go",
			"HistoryQueryGuard", "CacheGet", "MaskParticipantFields",
			"VenueWeekOpenUTC"),
		gotest(p23MD,
			"TestHistoryQueryGuardTimeout|TestHistoryCacheKeyDeterministic|"+
				"TestClosedInterval|TestCacheGetPutBestEffort|"+
				"TestMaskParticipantFields|TestMaskPreOpenWindowed|"+
				"TestVenueWeekOpenUTC"),
	)
}

func ckP23SwapRates(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/swap_rates.go",
			"services/internal/api/handlers_swap_rates.go"),
		structural(env, "services/internal/marketdata/swap_rates.go",
			"PgSwapRateHistory", "ProjectSwapRateDay"),
		gotest(p23MD,
			"TestProjectSwapRateDayTripleWednesday|"+
				"TestProjectSwapRateDayHolidaySpan|"+
				"TestProjectSwapRateDayNoAccruals|"+
				"TestProjectSwapRateDayPerSideSplit|TestWriteSwapRateDaysCSV"),
		gotest(p23API,
			"TestHistorySwapRatesTripleWednesday|"+
				"TestHistorySwapRatesFreeTierDelay|"+
				"TestHistorySwapRatesPremiumRealtime|"+
				"TestHistorySwapRatesTimeoutOutageUnknown|"+
				"TestHistorySwapRatesCSVAndCache"),
	)
}

func ckP23TakerPositioning(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/positioning.go",
			"services/internal/api/handlers_market_stats.go"),
		structural(env, "services/internal/marketdata/positioning.go",
			"TakerFlowStore", "TakerFlowBucket"),
		structural(env, "services/internal/api/handlers_market_stats.go",
			"MarketTakerVolume", "MarketPositioning", "AnalyticsTakerFlow"),
		gotest(p23MD,
			"TestTakerFlowWindow|TestTakerFlowWindowZeroSell|"+
				"TestTakerFlowBuckets|TestTakerFlowStoreNotWired|"+
				"TestTakerFlowQueryError"),
		gotest(p23API,
			"TestMarketTakerVolume|TestTakerVolumeAllSuppressed422|"+
				"TestTakerFlowDeadlineMaps504|TestMarketPositioning|"+
				"TestMarketPositioningSuppressed|TestMarketPositioningMissingSymbol"),
	)
}

func ckP23VenuePerformance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/performance.go",
			"services/internal/api/handlers_market_stats.go"),
		structural(env, "services/internal/marketdata/performance.go",
			"VenuePerformanceService", "PgVenueDayStatsSource",
			"PerformanceReferenceSource"),
		structural(env, "services/internal/api/handlers_market_stats.go",
			"MarketPerformance", "NewVenueFillRateSource", "RTS27FiguresSource"),
		gotest(p23MD,
			"TestVenuePerformanceReport|TestVenuePerformanceWeeklyRollup|"+
				"TestVenuePerformanceDivergenceHoldsLastGood|"+
				"TestVenuePerformanceDivergenceWithoutLastGood|"+
				"TestVenuePerformancePrivacy|TestVenuePerformanceUptimeUnknown|"+
				"TestVenuePerformanceNoDailySource"),
		gotest(p23API,
			"TestMarketPerformance|TestMarketPerformanceCache|"+
				"TestMarketPerformanceRedisOutage|TestMarketPerformanceNotConfigured"),
	)
}
