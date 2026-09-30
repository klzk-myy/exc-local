package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-06 market-data-distribution checkpoints.
//
// Wave 1 landed the internal/marketdata hub (WS server, conflation,
// resume), the ohlcv engine, internal/sbe (A/B multicast + negotiated
// SBE), and internal/ws resilience (rate limits, drain, backpressure).
// Wave 2 landed the stream producers (trades/ticker/BBO/aggTrades/
// liquidations/stats/OI) and lifecycle tasks (private stream, depth
// params, reference price, seq durability, resync).
const (
	p06MD    = "./internal/marketdata"
	p06OHLCV = "./internal/marketdata/ohlcv"
	p06SBE   = "./internal/sbe"
	p06WS    = "./internal/ws"
)

func registerPhase06(r *spec.Registry) {

	// ---- 6.3.1 WS server --------------------------------------------------
	r.Register("P06-T6.3.1-C1", ckP06WSServer,
		"goroutine-per-connection WS")
	r.Register("P06-T6.3.1-C2", ckP06SubLimits,
		"20 L2 / 5 L3 subscription limits")

	// ---- 6.3.2 L2 conflation ----------------------------------------------
	r.Register("P06-T6.3.2-C1", ckP06Conflation,
		"100ms conflation or 100 events")
	r.Register("P06-T6.3.2-C2", ckP06ReconnectReplay,
		"last_seq reconnect replay")
	r.Register("P06-T6.3.2-C3", ckP06DepthCRC,
		"L2 depth CRC32 checksums with resync on mismatch")
	r.Register("P06-T6.3.2-C4", ckP06SLA,
		"market data SLA p99 WS push <= 100ms + uptime 99.95%")

	// ---- 6.3.3/6.3.4/6.3.5 streams + private -------------------------------
	r.Register("P06-T6.3.3-C1", ckP06Trades,
		"real-time trades no conflation")
	r.Register("P06-T6.3.4-C1", ckP06Ticker,
		"1s ticker with 24h OHLCV")
	r.Register("P06-T6.3.5-C1", ckP06PrivateOrders,
		"private order stream with JWT auth")

	// ---- 6.3.6 SBE A/B multicast -------------------------------------------
	r.Register("P06-T6.3.6-C1", ckP06SBE,
		"institutional A/B multicast + replay/snapshot recovery (§10.4)")

	// ---- 6.3.8 OHLCV engine -------------------------------------------------
	r.Register("P06-T6.3.8-C1", ckP06OHLCV,
		"OHLCV aggregation")

	// ---- 6.3.9/6.3.10 session + dispatch ------------------------------------
	r.Register("P06-T6.3.9-C1", ckP06Resume,
		"WS session resume protocol")
	r.Register("P06-T6.3.10-C1", ckP06Dispatcher,
		"WS request-response dispatcher with thread-safe write synchronization")

	// ---- 6.3.11–6.3.16 stream + limit tasks ---------------------------------
	r.Register("P06-T6.3.11-C1", ckP06BBO,
		"BBO emits every top-of-book change without conflation (§24 #261)")
	r.Register("P06-T6.3.12-C1", ckP06AggTrades,
		"aggregate-trade events preserve taker/price grouping and trade-ID lineage")
	r.Register("P06-T6.3.13-C1", ckP06Liquidations,
		"public liquidation feed is delayed and never front-runs active auctions")
	r.Register("P06-T6.3.14-C1", ckP06KlineIntervals,
		"canonical candle set contains 13 aligned intervals (§24 #264)")
	r.Register("P06-T6.3.15-C1", ckP06DepthVariants,
		"depth subscriptions enforce supported level/cadence combinations (§24 #265)")
	r.Register("P06-T6.3.16-C1", ckP06Multiplexing,
		"WS multiplexing enforces documented subscription limits (§24 #265)")

	// ---- 6.3.17–6.3.24 protocol/ops tasks -----------------------------------
	r.Register("P06-T6.3.17-C1", ckP06RefPrice,
		"execution rules, reference prices, provenance, stream, and expiry")
	r.Register("P06-T6.3.18-C1", ckP06SBENegotiated,
		"REST/WS/private SBE negotiation and six-month schema lifecycle")
	r.Register("P06-T6.3.19-C1", ckP06Drain,
		"planned shutdown drains clients with explicit WS/FIX reconnect advisory")
	r.Register("P06-T6.3.20-C1", ckP06StatsTape,
		"all-market rolling statistics and delayed anonymous block-trade tape")
	r.Register("P06-T6.3.21-C1", ckP06SlowConsumer,
		"Slow consumer eviction, reconnect flood throttling, and multicast failover")
	r.Register("P06-T6.3.22-C1", ckP06SeqDurability,
		"durable WS sequence log with gap journal, order-action dedup window")
	r.Register("P06-T6.3.23-C1", ckP06OI,
		"public open-interest stream and history from position aggregates (§24 #266)")
	r.Register("P06-T6.3.24-C1", ckP06Resync,
		"WebSocket L2/L3 snapshot/delta resync protocol and sequence continuity")
}

// ---------------------------------------------------------------------------

var ckP06WSServer = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestConcurrentWritesRace|TestHeartbeatStaleClientDrop|TestHeartbeatHealthyClientSurvives|TestSubscribeUnknownChannel"))
}

var ckP06SubLimits = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestSubscribeLimitsL2|TestSubscribeLimitsL3"))
}

var ckP06Conflation = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestConflationWindowFlush|TestConflationCountFlush|TestSeqOrderingPerSymbol|TestSeqPrevChain"))
}

var ckP06ReconnectReplay = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestEventFanoutAndResume|TestRingReplayInRange|TestRingReplayUpToDate|TestRingReplayGapTooLarge|TestRingAgeEviction"))
}

var ckP06DepthCRC = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestDepthCRC32Stable|TestDepthVariantPrevChain"))
}

var ckP06SLA = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/marketdata/metrics.go", "LatencySnapshot"),
		structural(env, "services/internal/marketdata/metrics.go", "p99"))
}

var ckP06Trades = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestTrades_NoConflation|TestTrades_"))
}

var ckP06Ticker = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestTicker_24hRollingStats|TestTicker_ExpiryEvictsOldTrades|TestTicker_CadenceEmitsEachTick"))
}

var ckP06PrivateOrders = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestPrivateOrdersRequireAuth|TestPrivateOrdersIsolation|TestPrivateOrdersScopeGate|TestPrivateResumeIsolation|TestPrivateSeqRestartContinuity|TestPrivateEndpointRejectsPublic"))
}

var ckP06SBE = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06SBE, "TestEndToEnd_ABMulticast_ReplayGapFill|TestEndToEnd_BothFeedsLost_TCPReplayAndSnapshot|TestDedupBySeq|TestGapReplayResumeContinuity|TestGapBeyondRetention_SnapshotRecovery|TestFeedDesyncByteDivergence|TestDeterministicReplayOrdering"))
}

var ckP06OHLCV = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06OHLCV, "TestAggregationMathAcrossIntervals|TestBoundaryTradeOpensNewBucket|TestLateTradeGraceThenClosedImmutability|TestGapCandlesCarryForward"))
}

var ckP06Resume = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestResumeGapTooLarge|TestResumeInvalidSeq|TestRingReplayInvalidSeq|TestRingReplayEmpty"))
}

var ckP06Dispatcher = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestRequestReplyCorrelation|TestRegisterMethodDispatch|TestOrderDispatchAckAndDedup|TestOrderDispatchTimeout|TestUnauthenticatedOrderCloses"))
}

var ckP06BBO = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestBBO_"))
}

var ckP06AggTrades = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestAggTrades_"))
}

var ckP06Liquidations = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestLiquidations_|TestJetStreamLiquidationSource_"))
}

var ckP06KlineIntervals = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06OHLCV, "TestCanonicalIntervalSet|TestCanonicalSetCompletenessEndToEnd|TestIntervalFloorAlignment"))
}

var ckP06DepthVariants = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestParseChannelDepthVariants|TestDepthVariantSlicedEmit|TestDepthVariantCadenceGate|TestSnapshotDepthVariant|TestDepthVariantWireEndToEnd"))
}

var ckP06Multiplexing = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06WS, "TestSubAdmitHookEnforcesPerChannelCap"),
		gotest(p06MD, "TestSubscribeLimits"))
}

var ckP06RefPrice = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestRefPriceFreshEmit|TestRefPriceStaleFailClosed|TestRefPriceOracleUnavailable|TestRefPriceNoSubscribersNoPoll|TestRefPriceSnapshot|TestRefPriceSeqRestartContinuity"))
}

var ckP06SBENegotiated = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06SBE, "TestNegotiateActive|TestNegotiateUnknownSchemaAndVersion|TestNegotiateDeprecatedWarnsWithinWindow|TestNegotiateRetiredFailsExplicitly|TestRegistryEnforcesSixMonthWindow|TestNegotiateREST_HeadersAndErrors|TestNegotiateWS"))
}

var ckP06Drain = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06WS, "TestDrainAdvisoryOrderingAndDeadline|TestDrainReturnsWhenClientsLeave|TestDrainTwiceFails|TestDrainAdvisoryMergesFailoverEndpoints"))
}

var ckP06StatsTape = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestStats_|TestBlockTape_"))
}

var ckP06SlowConsumer = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06WS, "TestBackpressureEvictionSynthetic|TestBackpressureEvictionBytes|TestSlowConsumerEvictionLive|TestReconnectThrottle429|TestFeedFailoverAdvisory"))
}

var ckP06SeqDurability = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestGapJournalBounded|TestSeqRestartBoundaryJournaled|TestMemSeqStore"),
		gotest(p06MD, "TestRedisSeqStore"))
}

var ckP06OI = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestOI_"))
}

var ckP06Resync = func(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p06MD, "TestResyncBySymbol|TestResyncOrderingContinuity"))
}
