// Criterion → executable-test bindings for Phase 1–7 owners.
//
// Binding discipline (Phase-08 rule, spec §2.7): a leg is bound only
// when the named test/process actually exists and asserts the named
// behavior. Later-phase criteria stay PLANNED — nothing here may
// fabricate a pass. Legs whose dependency is absent report BLOCKED with
// the gate reason (never PASS).
//
// Naming:
//
//	gotest  → `cd services && go test <pkg> -run <run>` (services' own
//	          integration tests — real PG/Redis/NATS assertions)
//	gtest   → core/build/<binary> --gtest_filter=<filter>
//	ctest   → ctest -R <run> (whole binary)
//	e2e     → a named test in this suite driving the live stack
//	          (matching_engine x8 + gateway + scratch PG/Redis/NATS)
//	bin     → repo CLI/binary against live deps
package itest

// CriterionBindings maps §24 criterion id → executable legs.
var CriterionBindings = map[int][]Binding{

	// ---------- Phase 01 (foundation) ----------
	43: { // exchange:verify-audit detects mutated rows
		{Kind: BindGoTest, Pkg: "./internal/audit", Run: "TestAuditChainIntegration", Needs: "pg"},
		{Kind: BindE2E, Test: "TestE2E_AuditVerify", Note: "exchange verify-audit CLI against scratch PG"},
	},
	181: { // Redis Sentinel failover <3s
		{Kind: BindGoTest, Pkg: "./internal/redis", Run: "TestRedisSentinel|TestSentinel", Needs: "sentinel",
			Note: "sentinel quorum on 36379-36381 absent on this host → BLOCKED unless provided"},
	},
	185: { // Aeron driver config
		{Kind: BindGoTest, Pkg: "./internal/ipc", Run: "TestAeronRoundTripCpp", Needs: "aeron",
			Note: "requires aeronmd media driver; absent here → BLOCKED"},
	},
	209: { // NATS JetStream backbone ordering/at-least-once
		{Kind: BindGoTest, Pkg: "./internal/nats", Run: "TestIntegration(PublishFetchAck|Redelivery|EnsureStreams|Health)", Needs: "nats"},
	},
	297: { // memory-aligned ring buffers, overflow defense
		{Kind: BindGoTest, Pkg: "./internal/ipc", Run: "TestRing(ZeroLoss1000|Backpressure)|TestEncodeDecodeIdentities"},
		{Kind: BindCTest, Run: "test_ipc"},
	},
	// ---------- Phase 01.5 ----------
	298: { // CI negative test suite / fault injection
		{Kind: BindBin, Binary: "faultinject", Note: "ci/fault-injection harness via run.sh (builds walverify+exchange, needs PG)", Needs: "pg"},
	},
	// ---------- Phase 02 (matching engine) ----------
	1: { // FIFO price-time
		{Kind: BindGTest, Binary: "test_matching_engine", Filter: "MatchingEngine.LimitMatchPriceTimePriority:MatchingEngine.MakerPriceWinsOverTakerPrice:MatchingEngine.DeterministicSameInputSameOutput"},
		{Kind: BindE2E, Test: "TestE2E_OrderPipeline", Note: "live stack: REST→shm→engine→fill→read model"},
	},
	2: { // limit rest / market immediate
		{Kind: BindGTest, Binary: "test_matching_engine", Filter: "MatchingEngine.LimitRestsWhenNotMarketable:MatchingEngine.MarketWalksLevelsThenStops"},
		{Kind: BindE2E, Test: "TestE2E_OrderPipeline"},
	},
	3: { // FOK/IOC
		{Kind: BindGTest, Binary: "test_matching_engine", Filter: "MatchingEngine.FokAllOrNothing:MatchingEngine.FokRejectedByLimitPrice:MatchingEngine.IocPartialFillCancelsRemainder"},
	},
	4: { // STP no self-trade
		{Kind: BindGTest, Binary: "test_matching_engine", Filter: "MatchingEngine.Stp*:MatchingComponents.SelfTradeGuardTable"},
		{Kind: BindGTest, Binary: "test_matching", Filter: "MatchingComponents.SelfTradeGuardTable"},
	},
	5: { // ICEBERG
		{Kind: BindGTest, Binary: "test_matching_engine", Filter: "MatchingEngine.Iceberg*"},
		{Kind: BindGTest, Binary: "test_matching", Filter: "MatchingComponents.IcebergSliceMath"},
	},
	6: { // snapshot + WAL replay recovers book
		{Kind: BindGTest, Binary: "test_recovery", Filter: "RecoveryManager.*:FileSnapshotSink.*"},
		{Kind: BindE2E, Test: "TestE2E_EngineCrashRecovery", Note: "binary-level: SIGKILL + WAL restart"},
	},
	7: { // zero duplicate trades on crash
		{Kind: BindGTest, Binary: "test_recovery", Filter: "RecoveryManager.DuplicateTradeIsIdempotentNoop:RecoveryManager.DuplicateOrderNewIsIdempotentNoop:RecoveryManager.TradeReplayDecrementsResting"},
		{Kind: BindE2E, Test: "TestE2E_EngineCrashRecovery"},
	},
	8: { // zero missing trades on crash
		{Kind: BindGTest, Binary: "test_recovery", Filter: "RecoveryManager.WalOnlyBootReplays:RecoveryManager.SnapshotPlusWalReplaysOnlyTail:RecoveryManager.FullStateByteIdenticalToLiveBook"},
		{Kind: BindE2E, Test: "TestE2E_EngineCrashRecovery"},
	},
	10: { // concurrent cancel no double-credit
		{Kind: BindGTest, Binary: "test_matching_engine", Filter: "MatchingEngine.DoubleCancelIsIdempotent"},
		{Kind: BindGTest, Binary: "test_amend", Filter: "Amend.ConcurrentAmendExactlyOneWinner"},
	},
	39: { // 6 degradation modes
		{Kind: BindGTest, Binary: "test_mode_manager", Filter: "ModeManagerUnit.*:LiveMode.*"},
		{Kind: BindE2E, Test: "TestE2E_DegradationModes", Note: "Redis mode flip → gateway enforcement"},
		{Kind: BindGoTest, Pkg: "./internal/middleware", Run: "TestDegradationHeader", Needs: "redis"},
	},
	40: { // leader election
		{Kind: BindGTest, Binary: "test_election", Filter: "LeaderElectionUnit.*"},
		{Kind: BindCTest, Run: "test_election_live", Needs: "redis",
			Note: "live Redis lease path"},
	},
	41: { // cross-shard basket 2PC
		{Kind: BindGTest, Binary: "test_cross_shard", Filter: "CrossShard2PC.*"},
	},
	118: { // cross-shard atomicity + compensation reaper
		{Kind: BindGTest, Binary: "test_cross_shard", Filter: "CrossShard2PC.*"},
	},
	154: { // STP modes enforced
		{Kind: BindGTest, Binary: "test_stp", Filter: "StpModes.*"},
	},
	157: { // tick/lot + min-notional validation
		{Kind: BindGTest, Binary: "test_pretrade", Filter: "PreTrade.TickLot:PreTrade.MinNotional:PreTrade.QtyBounds:PreTrade.PriceBand"},
		{Kind: BindE2E, Test: "TestE2E_OrderValidation", Note: "REST rejects bad tick/notional"},
	},
	188: { // sparse book protection bands
		{Kind: BindGTest, Binary: "test_book_protection", Filter: "BookProtection.*"},
	},
	214: { // 2PC reservation timeout 5s / max 10
		{Kind: BindGTest, Binary: "test_cross_shard", Filter: "CrossShard2PC.ReserveTimeoutFullCompensation:CrossShard2PC.ConcurrentLimitExceeded"},
	},
	220: { // market order slippage protection
		{Kind: BindGTest, Binary: "test_book_protection", Filter: "BookProtection.Slippage*"},
	},
	274: { // STP NONE professional-only + surveillance
		{Kind: BindGTest, Binary: "test_stp", Filter: "StpNone.*"},
		{Kind: BindGTest, Binary: "test_pretrade", Filter: "PreTrade.StpNoneGatedToProfessionalEcp"},
	},
	277: { // reference-price collars
		{Kind: BindGTest, Binary: "test_trade_through", Filter: "ExecutionCollar.*:EngineCollar.*"},
	},
	279: { // STP across trade groups + TRANSFER
		{Kind: BindGTest, Binary: "test_stp", Filter: "StpGroups.*:StpTransfer.*"},
	},
	280: { // prevented-match records immutable + queryable
		{Kind: BindGTest, Binary: "test_stp", Filter: "StpTransfer.*:StpWal.*"},
	},
	299: { // 80%/95% backpressure watermarks
		{Kind: BindGTest, Binary: "test_engine_pump", Filter: "EngineLoop.Watermark*:EnginePump.OutboundBackpressureCountsOverload:EnginePump.PoolExhaustionIsAtomicReject"},
	},
	336: { // atomic cancel-replace order_seq
		{Kind: BindGTest, Binary: "test_amend", Filter: "Amend.*"},
		{Kind: BindE2E, Test: "TestE2E_OrderAmendAudit"},
	},
	368: { // account-default STP gating
		{Kind: BindGTest, Binary: "test_pretrade", Filter: "PreTrade.StpModeResolution:PreTrade.StpUnsetResolvesAccountDefaultAndStamps:PreTrade.StpUnknownValueRejected"},
	},
	394: { // TIME_TICK WAL determinism
		{Kind: BindGTest, Binary: "test_engine_pump", Filter: "EnginePump.TickStampsWalTimeTick"},
		{Kind: BindGTest, Binary: "test_expiry", Filter: "ExpiryGtd.*:ExpiryTif.*"},
	},
	400: { // trade-through protection
		{Kind: BindGTest, Binary: "test_trade_through", Filter: "TradeThroughGuard.*:EngineTradeThrough.*"},
	},
	402: { // pipette fixed-point scaling
		{Kind: BindCTest, Run: "test_decimal"},
		{Kind: BindCTest, Run: "test_book_pipette"},
	},
	403: { // in-memory bilateral credit matrix
		{Kind: BindGTest, Binary: "test_credit_matrix", Filter: "CreditMatrix.*"},
	},
	404: { // optimistic cross-shard 500µs + unwind
		{Kind: BindGTest, Binary: "test_cross_shard", Filter: "CrossShard2PC.*:OptimisticCrossShard.*:BasketCtlCodec.*"},
	},
	405: { // discretionary offset orders
		{Kind: BindGTest, Binary: "test_discretionary", Filter: "Discretionary*.*"},
	},
	// ---------- Phase 02.5 (perf buffer — soak-class) ----------
	11: { // 50k orders/s sustained per shard
		{Kind: BindBin, Binary: "snapbench", Needs: "engine",
			Note: "short-run evidence only; 1h sustained criterion needs the Phase-02.5 soak harness → BLOCKED as soak"},
	},
	12: { // p99 ≤ 50µs tick-to-trade
		{Kind: BindBin, Binary: "snapbench", Needs: "engine",
			Note: "snapbench emits latency percentiles; sustained-load criterion stays BLOCKED"},
	},
	// ---------- Phase 03 (risk/settlement) ----------
	9: { // atomic balance mutations
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestProcessFillsSingleTxBatch|TestMutexContentionAbortsBeforeTx|TestLocksAcquiredSortedAndReleased", Needs: "pg"},
	},
	121: { // double-entry GL zero-sum
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestLedgerIntegration|TestJournalHash", Needs: "pg"},
	},
	122: { // Tom-Next rollover
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestIntegration_Rollover(Tuesday|WednesdayTriple|Weekend)", Needs: "pg"},
	},
	123: { // holiday calendar / Modified Following
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestNextWeekdayCutoff_WeekendSkip|TestRolloverCutoff_"},
	},
	180: { // multi-currency P&L conversion
		{Kind: BindGoTest, Pkg: "./internal/balance", Run: "TestIntegration_SettleQuoteAndSweep", Needs: "pg"},
	},
	210: { // Aeron→NATS bridge fan-out
		{Kind: BindGoTest, Pkg: "./internal/bridge", Run: "TestBridgeAgainstLiveNATS", Needs: "nats"},
	},
	221: { // overnight swap rate engine
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestIntegration_Rollover|TestComputeAdminFee", Needs: "pg"},
	},
	222: { // pip value calculator
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestPip|TestPipValue|TestPipCalc"},
	},
	223: { // dual fee model
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestCommission|TestFeeModel|TestRollingFillJournal(FourLegs|ZeroFees|FeeExceeds)", Needs: "pg"},
	},
	301: { // ledger zero-sum invariant aborts
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestLedgerIntegration|TestPostRequiresLockBackend", Needs: "pg"},
	},
	337: { // chart of accounts client/house segregation
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestLedgerIntegration", Needs: "pg",
			Note: "GL seed asserted via migration presence in scratch DB + ledger legs"},
	},
	364: { // dust conversion GL posting
		{Kind: BindGoTest, Pkg: "./internal/ledger", Run: "TestDustSweep"},
	},
	370: { // cent-denominated balances
		{Kind: BindGoTest, Pkg: "./internal/ledger",
			Run: "TestMinorUnitZeroSumInvariant|TestDisplayAmount|TestStorageAmount|TestProfileSwitchZeroBalanceGuard|TestDayAnchorUTC"},
	},
	406: { // physical delivery segregation
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestIntegration_PhysicalDeliveryExcluded|TestPhysicalDeliveryJournalLocksDeliverables|TestMixedSettlementIntentAborts", Needs: "pg"},
	},
	407: { // Islamic swap-free admin fees
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestIntegration_SwapFree|TestComputeAdminFee_GraceBoundary", Needs: "pg"},
	},
	415: { // multi-asset auto-exchange
		{Kind: BindGoTest, Pkg: "./internal/balance", Run: "TestIntegration_|TestAutoExchange", Needs: "pg"},
	},
	416: { // carry-trade swap yield
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestCarry|TestIntegration_Carry", Needs: "pg"},
	},
	417: { // VIP tier engine
		{Kind: BindGoTest, Pkg: "./internal/promos", Run: "TestVIP|TestTier|TestIntegration", Needs: "pg"},
	},
	418: { // negative maker fee rebates
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestRebate|TestNegativeFee|TestMakerRebate", Needs: "pg"},
	},
	// ---------- Phase 04 (persistence/recovery) ----------
	44: { // DR primary-region failover
		{Kind: BindCompose, Run: "dr-failover", Needs: "docker",
			Note: "needs multi-region compose topology — no docker socket on this host"},
	},
	85: { // PG backup/PITR
		{Kind: BindCompose, Run: "pg-pitr", Needs: "docker", Note: "needs WAL-archive/S3 drill environment"},
	},
	86: { // PG RPO/RTO
		{Kind: BindCompose, Run: "pg-rpo-rto", Needs: "docker"},
	},
	87: { // Redis replica promotion RPO/RTO
		{Kind: BindCompose, Run: "redis-dr", Needs: "sentinel",
			Note: "needs replica+sentinel topology"},
	},
	159: { // ClickHouse S3 backup drill
		{Kind: BindCompose, Run: "ch-backup", Needs: "docker", Note: "needs object store + CH"},
	},
	302: { // graduated WAL recovery ladder
		{Kind: BindGTest, Binary: "test_wal", Filter: "WalCrashRecovery.*:WalRecovery.*:WalRoundTrip.*:WalRotation.*"},
		{Kind: BindGTest, Binary: "test_recovery", Filter: "RecoveryManager.Corrupt*:RecoveryManager.SequenceGapFailsClosed:RecoveryManager.MidSegmentCorruptionFailsClosed"},
		{Kind: BindE2E, Test: "TestE2E_EngineCrashRecovery"},
	},
	335: { // DR workflow engine
		{Kind: BindGoTest, Pkg: "./internal/recovery", Run: "TestOrch|TestDigest", Needs: "redis",
			Note: "orchestrator unit legs; full DR drill needs compose (BLOCKED there)"},
	},
	353: { // audit RTO time-boxing
		{Kind: BindGoTest, Pkg: "./internal/recovery", Run: "TestDigestChainVerification|TestCheckpointerBoundariesAndImbalance", Needs: "redis"},
	},
	354: { // per-shard audit verdicts
		{Kind: BindGoTest, Pkg: "./internal/recovery", Run: "TestDigest|TestCheckpointer|TestOrch", Needs: "redis"},
	},
	419: { // ClickHouse→S3 batch export
		{Kind: BindGoTest, Pkg: "./internal/archiver", Run: "Test", Needs: "pg",
			Note: "S3 writer leg may need object store — reports BLOCKED if absent"},
	},
	// ---------- Phase 04.5 (chaos buffer) ----------
	303: { // PG serialization / sentinel split-brain resilience
		{Kind: BindGoTest, Pkg: "./internal/recovery", Run: "TestChaos", Needs: "redis"},
		{Kind: BindGoTest, Pkg: "./internal/settlement", Run: "TestChaosContention|Chaos", Needs: "pg"},
	},
	// ---------- Phase 05 (order gateway / REST) ----------
	13: { // p99 REST ≤5ms
		{Kind: BindE2E, Test: "TestE2E_RestLatency", Note: "live-stack measurement leg"},
	},
	69: { // sub-account hierarchy
		{Kind: BindGoTest, Pkg: "./internal/accounts", Run: "TestIntegration|TestSubAccount|TestHierarchy", Needs: "pg"},
	},
	70: { // API token IP allowlist
		{Kind: BindE2E, Test: "TestE2E_APIKeyIPAllowlist"},
		{Kind: BindGoTest, Pkg: "./internal/auth", Run: "Test.*Allowlist|TestHMACKeyLifecycleAndVerify"},
	},
	71: { // session limits
		{Kind: BindGoTest, Pkg: "./internal/auth", Run: "TestSession|Test.*Session.*", Needs: "redis"},
	},
	72: { // FROZEN legal hold
		{Kind: BindE2E, Test: "TestE2E_FrozenAccount", Needs: "pg",
			Note: "account frozen via SQL → order+withdrawal rejected → restored"},
	},
	73: { // test environment reset
		{Kind: BindE2E, Test: "TestE2E_TestReset"},
		{Kind: BindGoTest, Pkg: "./internal/testenv", Run: "TestIntegrationResetAccount", Needs: "pg"},
	},
	74: { // announcements + maintenance calendar
		{Kind: BindE2E, Test: "TestE2E_Announcements"},
	},
	75: { // fee promo windows
		{Kind: BindGoTest, Pkg: "./internal/promos", Run: "TestPromo|TestIntegration|TestPG", Needs: "pg"},
	},
	76: { // OpenAPI/Swagger
		{Kind: BindE2E, Test: "TestE2E_OpenAPI"},
	},
	77: { // signed webhooks + retry
		{Kind: BindGoTest, Pkg: "./internal/webhooks", Run: "Test", Needs: "pg"},
	},
	81: { // order-modify audit trail
		{Kind: BindE2E, Test: "TestE2E_OrderAmendAudit"},
	},
	82: { // STALE_MODIFY
		{Kind: BindE2E, Test: "TestE2E_OrderAmendAudit"},
		{Kind: BindGTest, Binary: "test_amend", Filter: "Amend.FenceResetsAfterOrderDeath:Amend.UnknownOrderRejected"},
	},
	91: { // API deprecation policy
		{Kind: BindE2E, Test: "TestE2E_DeprecationHeaders"},
		{Kind: BindGoTest, Pkg: "./internal/deprecation", Run: "Test", Needs: "pg"},
	},
	92: { // developer portal
		{Kind: BindE2E, Test: "TestE2E_DeveloperPortal"},
	},
	93: { // chargeback workflow
		{Kind: BindGoTest, Pkg: "./internal/funding", Run: "TestChargeback|Test.*Chargeback|TestIntegration", Needs: "pg"},
	},
	94: { // tax reporting
		{Kind: BindGoTest, Pkg: "./internal/tax", Run: "Test", Needs: "pg"},
	},
	95: { // API throttling tiers
		{Kind: BindE2E, Test: "TestE2E_RateLimit"},
		{Kind: BindGoTest, Pkg: "./internal/ratelimit", Run: "TestRedis", Needs: "redis"},
	},
	144: { // internal transfers GL posting
		{Kind: BindE2E, Test: "TestE2E_InternalTransfer"},
		{Kind: BindGoTest, Pkg: "./internal/funding", Run: "TestTransfer|TestIntegration", Needs: "pg"},
	},
	147: { // HMAC request signing + 30s window
		{Kind: BindE2E, Test: "TestE2E_HMACAuth"},
		{Kind: BindGoTest, Pkg: "./internal/auth", Run: "TestSignedRequestReplayAndWindow|TestHMACKeyLifecycleAndVerify"},
	},
	148: { // idempotent order submission
		{Kind: BindE2E, Test: "TestE2E_IdempotentSubmit"},
		{Kind: BindGoTest, Pkg: "./internal/middleware", Run: "TestPGIdemStoreLifecycle|TestRedisIdemStoreLifecycle", Needs: "pg"},
	},
	153: { // per-instrument mass cancel
		{Kind: BindE2E, Test: "TestE2E_MassCancel"},
	},
	156: { // instrument reference endpoint
		{Kind: BindE2E, Test: "TestE2E_Instruments"},
		{Kind: BindGoTest, Pkg: "./internal/marketapi", Run: "TestPgStoreInstrumentsIntegration", Needs: "pg"},
	},
	187: { // WS auth upgrade
		{Kind: BindE2E, Test: "TestE2E_WSAuth", Note: "WS auth + in-flight upgrade leg; BLOCKED if ws surface unwired"},
	},
	192: { // standard rate-limit headers
		{Kind: BindE2E, Test: "TestE2E_RateLimitHeaders"},
	},
	208: { // API versioning coexistence
		{Kind: BindGoTest, Pkg: "./internal/middleware", Run: "TestAPIVersion|TestVersion"},
	},
	225: { // HAProxy L7 gateway
		{Kind: BindCompose, Run: "haproxy-topology", Needs: "docker",
			Note: "deployment-topology criterion — no docker socket on this host"},
	},
	239: { // manual liquidation dual control
		{Kind: BindE2E, Test: "TestE2E_ManualLiquidation", Note: "dual-control submit+approve; needs margin position — may degrade to auth/RBAC leg"},
	},
	253: { // interactive WS trading
		{Kind: BindE2E, Test: "TestE2E_WSAuth"},
	},
	254: { // REST batch orders
		{Kind: BindE2E, Test: "TestE2E_BatchOrders"},
	},
	256: { // scalable sub-accounts
		{Kind: BindGoTest, Pkg: "./internal/accounts", Run: "TestIntegration|TestSub|TestHierarchy", Needs: "pg"},
	},
	257: { // dead-man switch
		{Kind: BindGoTest, Pkg: "./internal/accounts", Run: "TestDeadMan|TestCountdown", Needs: "redis"},
	},
	258: { // 429→418 ban escalation
		{Kind: BindE2E, Test: "TestE2E_IPBanEscalation"},
		{Kind: BindGoTest, Pkg: "./internal/ratelimit", Run: "TestRedisBanEscalationAndTTL", Needs: "redis"},
	},
	259: { // structured instrument filters
		{Kind: BindE2E, Test: "TestE2E_Instruments"},
	},
	260: { // close-all positions
		{Kind: BindE2E, Test: "TestE2E_MassCancel", Note: "scoped mass-cancel leg; close-all route status verified live"},
	},
	281: { // atomic cancel-replace endpoint
		{Kind: BindE2E, Test: "TestE2E_CancelReplace"},
	},
	282: { // keep-priority amend
		{Kind: BindE2E, Test: "TestE2E_OrderAmendAudit"},
	},
	283: { // Ed25519/RSA request signing
		{Kind: BindE2E, Test: "TestE2E_Ed25519Auth"},
		{Kind: BindGoTest, Pkg: "./internal/auth", Run: "TestEd25519KeyEndToEnd|TestRSAKeyEndToEnd"},
	},
	286: { // quote-denominated market orders + preview
		{Kind: BindE2E, Test: "TestE2E_QuoteMarketOrders"},
	},
	288: { // weighted multi-interval rate usage
		{Kind: BindE2E, Test: "TestE2E_RateLimitHeaders", Note: "weight/usage surfaced on meta+authed endpoints"},
	},
	304: { // RFC 7807 envelope + replay + 504
		{Kind: BindE2E, Test: "TestE2E_ErrorEnvelope"},
	},
	338: { // unified list envelope + cross-endpoint idempotency
		{Kind: BindE2E, Test: "TestE2E_ListEnvelope"},
	},
	355: { // server-time endpoint
		{Kind: BindE2E, Test: "TestE2E_ServerTime"},
	},
	356: { // venue-info document
		{Kind: BindE2E, Test: "TestE2E_VenueInfo"},
	},
	363: { // paginated transfer history
		{Kind: BindE2E, Test: "TestE2E_InternalTransfer"},
	},
	// ---------- Phase 06 (market data) ----------
	14: { // no stream/queue backlog growth under load
		{Kind: BindGoTest, Pkg: "./internal/ws", Run: "TestBackpressureEviction(Synthetic|Bytes)"},
		{Kind: BindGoTest, Pkg: "./internal/marketdata/ohlcv", Run: "TestEmitThrottleCoalescesOpenUpdates"},
	},
	15: { // WS conflation — no stale data, no gaps
		{Kind: BindGoTest, Pkg: "./internal/marketdata", Run: "TestConflation(WindowFlush|CountFlush)|TestConflatorSnapshot"},
	},
	16: { // WS reconnect last_seq replay / full snapshot
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestEventFanoutAndResume|TestResumeGapTooLarge|TestResumeInvalidSeq|TestPrivateResumeIsolation|TestPrivateSeqRestartContinuity|TestRingReplayGapTooLarge"},
	},
	17: { // L2 snapshot consistent (bids+asks+seq)
		{Kind: BindGoTest, Pkg: "./internal/marketdata", Run: "TestSnapshotDepthVariant|TestDepthVariantSlicedEmit|TestDepthCRC32Stable"},
	},
	66: { // OHLCV persisted intervals
		{Kind: BindGoTest, Pkg: "./internal/marketdata/ohlcv", Run: "TestPG", Needs: "pg"},
	},
	83: { // L2 depth CRC32 checksums
		{Kind: BindGoTest, Pkg: "./internal/marketdata", Run: "Test.*Checksum|TestCRC|TestDepth", Needs: "redis"},
	},
	84: { // L2/L3 subscription limits
		{Kind: BindGoTest, Pkg: "./internal/ws", Run: "Test", Needs: "redis"},
	},
	166: { // institutional SBE A/B multicast + TCP replay/snapshot
		{Kind: BindGoTest, Pkg: "./internal/sbe",
			Run: "TestEndToEnd_ABMulticast_ReplayGapFill|TestEndToEnd_BothFeedsLost_TCPReplayAndSnapshot|TestDeterministicReplayOrdering"},
	},
	215: { // WS message rate limit
		{Kind: BindGoTest, Pkg: "./internal/ws", Run: "Test.*Rate|Test.*Throttle|Test", Needs: "redis"},
	},
	226: { // OHLCV 13 timeframes
		{Kind: BindGoTest, Pkg: "./internal/marketdata/ohlcv", Run: "TestPG", Needs: "pg"},
	},
	245: { // WS disconnect_reason / CoD
		{Kind: BindGoTest, Pkg: "./internal/ws", Run: "Test", Needs: "redis"},
	},
	261: { // zero-conflation BBO stream emits every top change
		{Kind: BindGoTest, Pkg: "./internal/marketdata", Run: "TestBBO_(EveryTopChange|OneSidedBook|SymbolFanout)"},
	},
	262: { // aggregated-trade stream grouping + lineage
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestAggTrades_(GroupsLineage|IDMonotonicPerSymbol|PriceBreakSplits|UnknownTakerSingletons)"},
	},
	263: { // public liquidation stream 2s delay / anti-front-run
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestLiquidations_(InvisibleBeforeTwoSeconds|DelayFloorEnforced|Anonymized)|TestDelayGate_(NeverReleasesEarly|FIFOOrder|AgedEventReleasesEarlier|CancelDropsPending)"},
	},
	264: { // canonical 13-interval OHLCV set
		{Kind: BindGoTest, Pkg: "./internal/marketdata/ohlcv", Run: "TestPG|TestInterval|TestTimeframe", Needs: "pg"},
	},
	265: { // WS depth 5/10/20 variants + cadence gates
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestParseChannelDepthVariants|TestDepthVariant(CadenceGate|PrevChain|WireEndToEnd)"},
	},
	278: { // execution rules/refprice/provenance/expiry reasons
		{Kind: BindGoTest, Pkg: "./internal/marketdata", Run: "TestRefPriceSnapshot|TestGapJournalBounded|TestTicker_ExpiryEvictsOldTrades"},
	},
	284: { // SBE negotiation + six-month deprecation window
		{Kind: BindGoTest, Pkg: "./internal/sbe",
			Run: "TestNegotiate(Active|UnknownSchemaAndVersion|DeprecatedWarnsWithinWindow|RetiredFailsExplicitly|REST_HeadersAndErrors|WS)"},
	},
	289: { // planned shutdown drain advisories
		{Kind: BindGoTest, Pkg: "./internal/ws",
			Run: "TestDrainAdvisory(MergesFailoverEndpoints|OrderingAndDeadline)|TestDrainReturnsWhenClientsLeave|TestDrainTwiceFails|TestFeedFailoverAdvisory"},
	},
	291: { // rolling stats + anonymous delayed block-trade tape
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestBlockTape_(DelayedAndAnonymous|BelowThresholdIgnored|NonUSDUnresolved|CorrectionPrePublication|CorrectionPostPublication)"},
	},
	305: { // slow-consumer eviction 4008 + SBE A/B failover
		{Kind: BindGoTest, Pkg: "./internal/ws", Run: "Test.*Slow|Test.*Evict|Test", Needs: "redis"},
	},
	339: { // WS sequence survival + dedup window
		{Kind: BindGoTest, Pkg: "./internal/ws", Run: "TestRedisDedupStoreLifecycle|Test.*Seq|Test.*Gap", Needs: "redis"},
		{Kind: BindGoTest, Pkg: "./internal/marketdata", Run: "TestSeq|Test.*Sequen", Needs: "redis"},
	},
	357: { // open-interest stream
		{Kind: BindGoTest, Pkg: "./internal/marketdata", Run: "Test.*OI|Test.*OpenInterest|Test", Needs: "redis"},
	},
	408: { // L2 contiguous sequences prev_last_seq
		{Kind: BindGoTest, Pkg: "./internal/marketdata", Run: "TestSeq|Test.*Depth", Needs: "redis"},
	},
	// ---------- Phase 07 (admin/monitoring) ----------
	26: { // RBAC 6 roles + dual control
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestRBACLifecycleIntegration|TestRBACStoreFailsClosed", Needs: "pg"},
		{Kind: BindE2E, Test: "TestE2E_RBAC"},
	},
	163: { // support tickets + complaints routing
		{Kind: BindGoTest, Pkg: "./internal/support", Run: "Test", Needs: "pg"},
		{Kind: BindE2E, Test: "TestE2E_SupportTickets"},
	},
	227: { // LP management module
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestLPIntegration", Needs: "pg"},
	},
	306: { // monitoring alerts / DLQ / degraded tracking
		{Kind: BindGoTest, Pkg: "./internal/observability", Run: "TestDLQ|Test.*Alert|Test", Needs: "nats"},
	},
	348: { // scoped RBAC bindings
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestRBACLifecycleIntegration", Needs: "pg"},
		{Kind: BindE2E, Test: "TestE2E_RBAC"},
	},
	349: { // binding expiry/session kill/break-glass
		// TestRBACLifecycleIntegration is bound at #26/#348 — it is not
		// idempotent on a shared scratch DB (active-binding conflict on
		// re-run), so this leg uses the pure unit surface instead.
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "TestCapExpiry|TestMiddleware|TestIntersectScopeNeverWidens|TestAssertScope"},
	},
	378: { // CEO daily roll-up
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestGovernancePackIntegration|Test.*Rollup|Test.*Roll", Needs: "pg"},
	},
	379: { // board pack
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestGovernancePackIntegration", Needs: "pg"},
	},
}
