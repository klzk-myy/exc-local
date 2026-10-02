// Criterion → executable-test bindings (Phases 1–7, 09, 11, 13, 15, 16,
// 18, 19, 23 + stragglers).
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
			Note: "vendored aeronmd (core/third_party/aeron/bin) — runs when present"},
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
		{Kind: BindCompose, Run: "dr-failover",
			Note: "dr_drill_runner.sh --drills D1 — parent region-failover row SKIP→BLOCKED until a live secondary exists"},
	},
	85: { // PG backup/PITR
		{Kind: BindCompose, Run: "pg-pitr", Note: "deploy/postgres/pitr_smoke.sh — host-native scratch cluster; exit 77 (pg binaries absent) → BLOCKED"},
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
	// ---------- Phase 09 (deployment/operations) ----------
	211: { // quarterly DR drill catalog
		{Kind: BindBin, Binary: "dr-drill-runner",
			Note: "scripts/ops/dr_drill_runner.sh runs the D1-D7 catalog; per-drill SKIP→BLOCKED via results.jsonl — never PASS-laundered"},
	},
	216: { // graceful shutdown: order drain, FIX Logout, WS reconnect hint
		{Kind: BindGoTest, Pkg: "./internal/middleware",
			Run: "TestRunStepsOrderAndBounds|TestRunStepTimeout|TestDrainFlag|TestReadyGate|TestRejectWhenDraining"},
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestDrain(SendsNewsThenLogoutAndWaits|RefusesNewLogons|ForceClosesStraggler|ContextAbort|NilRegistryIsNoop)"},
		{Kind: BindGoTest, Pkg: "./internal/ws",
			Run: "TestDrain(AdvisoryOrderingAndDeadline|ReturnsWhenClientsLeave|TwiceFails|AdvisoryMergesFailoverEndpoints)|TestFeedFailoverAdvisory"},
	},
	309: { // canary health-check failure → automated blue-green rollback
		{Kind: BindBin, Binary: "bluegreen-gate",
			Note: "deploy/scripts/tests/bluegreen_gate_test.sh — probe-fail abort/rollback legs vs mock gateway + stub kubectl"},
	},
	// ---------- Phase 11 (banking rails — stragglers) ----------
	46: { // global trading halt: new orders rejected; cancels/reads/WS survive
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestKillSwitch_(SubmitRejectedWhenSuspended|SubmitFailsClosed|CancelSurvivesHalt)"},
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "TestKillSwitchResolve_(Precedence|FailClosed|OrderHaltDetail)"},
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestKillSwitchServiceIntegration", Needs: "pg"},
	},
	// ---------- Phase 13 (risk controls — stragglers) ----------
	140: { // MiFID II RTS 9 OTR: per-account per-instrument ratio enforced
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestOtr_(SubmitRejectedWhenBreached|CancelSurvivesBreach|UnsetGateSkips)"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestOtr(KeyLayout|BreachRejectsNewOrders|FillClearsBreach|SweepClearsDecayedBreach|AdmissionFailsClosedOnRedisError|CountFailureIsBestEffortButCounted|MarketMakerScopedRatio)"},
		{Kind: BindGoTest, Pkg: "./internal/risk", Run: "TestOtrMonitorRedisIntegration", Needs: "redis",
			Note: "live-Redis sliding-window leg — self-skips without EXC_REDIS_TEST"},
	},
	152: { // scoped kill-switch: per-account / per-FIX-session / per-instrument
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "TestKillSwitch(Resolve_(Precedence|FailClosed|OrderHaltDetail|RailAndLP)|ScopeVocabulary|Roles_FailClosed)|TestNormalizeScope"},
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run:  "TestKillSwitch_SubmitRejectedWhenSuspended",
			Note: "carries an ACCOUNT-scope suspension leg"},
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestKillSwitchServiceIntegration", Needs: "pg"},
	},
	// ---------- Phase 15 (instrument lifecycle / sessions) ----------
	119: { // SUSPENDED/RESTRICTED grace periods enforced
		{Kind: BindGTest, Binary: "test_lifecycle_auction",
			Filter: "Lifecycle.StatusMatrixNewOrders:Lifecycle.RestrictedIsLimitOnly:Lifecycle.DelistedReduceOnlyWindow:Lifecycle.CancelsStayOpenAmendsGated"},
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "TestLifecycleTransitionMatrix|TestLifecycleRoleMatrix"},
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "TestInstrumentLifecycle(PGTransitions|SuspendSweepPG|UpdatePG)", Needs: "pg"},
	},
	138: { // trade bust/price-adjust: dual control + GL reversal + hold settlement
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "TestBustJournal(Balanced|ZeroFees|DeliveryIntent|FeeExceedsProceeds)|TestAdjustJournalDelta|TestBustRoleGate|TestBustApproverDistinct|TestBustRequestValidation|TestBustServiceFailsClosedOnNilDeps"},
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "Test(BustExecute|BustOfBusted|BustWindowExpired|BustSettledRejects|BustPendingApprove|PartialFillBust|BustAfterHedge|PriceAdjust|BustInsideBandRejects|BustExpirePending)PG", Needs: "pg"},
	},
	142: { // reopening auction: HALT→ACTIVE + weekly open uncross (5-min call)
		{Kind: BindGTest, Binary: "test_lifecycle_auction", Filter: "Auction.*"},
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestReopeningAuctionRule"},
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestInjectorReopeningCallMOOOnly|TestAuctionSubmitQueuesLocally|TestInjectorQueueFilteredDispatch"},
	},
	217: { // 24/5 session lifecycle: Friday close preserves GTC; Sunday pre-open
		{Kind: BindGTest, Binary: "test_lifecycle_auction",
			Filter: "Lifecycle.MarketEntryWeek:Lifecycle.MarketHoursGate:Lifecycle.ParseMarketHours"},
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "TestSessionStateAt|TestSessionEventName|TestNewSessionServiceFailClosed|TestNextSessionClose"},
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestSessionLifecycleWeeklyFlow", Needs: "redis"},
	},
	234: { // instrument maintenance: maker-checker for creation/parameters
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "TestMaintenanceRoleGate|TestParamValidation|TestDefaultNextSessionStart|TestSensitiveOpRegistered|TestPairSymbolValidation"},
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "Test(CreateMakerCheckerChain|ParamChangeScheduled|EmergencyParamChange)PG|TestInstrumentLifecycleDualControlPG", Needs: "pg"},
	},
	290: { // persistent CANCEL_ONLY: rejects new/replace/amend, keeps resting + cancels
		{Kind: BindGTest, Binary: "test_lifecycle_auction",
			Filter: "Lifecycle.CancelsStayOpenAmendsGated"},
		{Kind: BindGoTest, Pkg: "./internal/orders", Run: "TestValidateSubmitInstrumentStates"},
		{Kind: BindGoTest, Pkg: "./internal/admin", Run: "TestLifecycleTransitionMatrix"},
	},
	316: { // reopening clearing failures → automated 30s extensions
		{Kind: BindGTest, Binary: "test_lifecycle_auction",
			Filter: "Auction.StrikeFailAwaitsExtensionThenClears:Auction.ExtendMovesDeadline:Auction.WithdrawnKeyQuarantinesCrossedBook:Auction.ReArmClearsQuarantineByUncrossing"},
	},
	343: { // seeded per-symbol reference (tick/lot/notional) + DST/holiday sessions
		{Kind: BindGoTest, Pkg: "./internal/instruments",
			Run: "TestNextOccurrence(DST|Tokyo)|TestWeeklyOpenDST|TestSessionStates|TestTradeDayAndCutoff|TestCalendarEntryValidate"},
		{Kind: BindGoTest, Pkg: "./internal/instruments", Run: "TestListingLifecycleIntegration", Needs: "pg"},
	},
	352: { // listing proposals auto-checks + impact-previewed delisting ladder
		{Kind: BindGoTest, Pkg: "./internal/instruments",
			Run: "TestDelistLadderIntegration|TestListingLifecycleIntegration", Needs: "pg"},
		{Kind: BindGoTest, Pkg: "./internal/admin",
			Run: "Test(DelistWorkflow|CreateMakerCheckerChain)PG", Needs: "pg"},
	},
	401: { // per-instrument configurable closing-auction calendar w/ DST
		{Kind: BindGoTest, Pkg: "./internal/instruments",
			Run: "TestParseRecurrence|TestNextOccurrence(DST|Tokyo)|TestCalendarEntryValidate"},
		{Kind: BindGoTest, Pkg: "./internal/instruments", Run: "TestCalendarReplaceIntegration", Needs: "pg"},
	},
	// ---------- Phase 16 (advanced order types / algo) ----------
	47: { // OCO: fill of one leg cancels sibling within 100ms
		{Kind: BindGTest, Binary: "test_oco", Filter: "OcoEngine.*"},
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestSubmitOCO_(LinkBeforeLegs|ReplayIsIdempotent|RaceReplayReturns409|CollisionRollsBackPair|CrossInstrumentRejected|DispatchFailureRejectsBoth)|TestConsumer_OcoReasonAudit|TestParseSubmitOco"},
	},
	48: { // TWAP: equal time slices over duration
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestTWAP(Validation|PlanEqualSlicesAndConservation|EndToEndFills)|TestPerturbSizesNeverNegative"},
	},
	49: { // VWAP: weighted by historical volume profile
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestVWAPProfileWeighting|TestVP(Validation|NoVolumeSourceFailsClosed)"},
	},
	50: { // trailing stop: stop trails market by offset
		{Kind: BindGTest, Binary: "test_phase16", Filter: "Phase16Trailing.*"},
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestSubmitTrailingStopEndToEnd|TestTrailingStop(FirstClassFields|LegalityMatrix|AlgoParamsFold|AlgoParamsFoldRejects)|TestOrderNewMsgTrailing(WireFields|UnitsWire)"},
	},
	51: { // bracket order: TP + SL around a position
		{Kind: BindGoTest, Pkg: "./internal/orders", Run: "TestITBracketFillCascade", Needs: "pg"},
	},
	52: { // spread order: multi-leg with price relationship enforced
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestSpread(MarketMismatchRejects|BothLegsFill|LegAFailureStopsLegB|RollbackOnPartial)"},
	},
	53: { // scaled order: multiple levels with quantity distribution
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestScaled(WeightsAndLevels|Validation)"},
	},
	124: { // benchmark fixing orders (WM/Refinitiv 4pm / ECB windows)
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestFixingWindowClosedWhen(NoEnabledWindow|RowDisabled)|TestSchedulerOrderBenchmarkRoundTrip|TestFixingVocabRejectsSupersededStrings"},
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestITFixing(LPResidualCleared|ResidualWithoutLPHonest)", Needs: "pg"},
	},
	129: { // post_only: marketable post-only rejected (POST_ONLY_VIOLATION)
		{Kind: BindGTest, Binary: "test_pretrade", Filter: "PreTrade.PostOnlyMarketable"},
		{Kind: BindGoTest, Pkg: "./internal/orders", Run: "TestValidateSubmitTypeVocab",
			Note: "Go-side vocabulary leg (post_only on MARKET/auction types)"},
	},
	130: { // reduce_only: only reduces an open position; reject/clip on increase
		{Kind: BindGTest, Binary: "test_pretrade", Filter: "PreTrade.ReduceOnlyNeedsOppositePosition"},
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestProductGate_(ReduceOnlyBypasses|ModifyConsultsOrderFlag)|TestValidateSubmitInstrumentStates"},
	},
	197: { // HIDDEN orders absent from L2/L3 market data
		{Kind: BindGTest, Binary: "test_phase16", Filter: "Phase16Hidden.L2OmissionButRests"},
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestL3Mirror_HiddenExcluded|TestL3EventPayload_HiddenRedacted|TestL3SnapshotReader_HiddenFlagFromWal"},
	},
	198: { // dark orders match at midpoint pricing
		{Kind: BindGTest, Binary: "test_phase16",
			Filter: "Phase16Hidden.MidpointFill:Phase16Hidden.NoVisibleMidFailsClosed"},
	},
	252: { // GSLO: guaranteed fill at stop, premium at placement, refund on cancel
		{Kind: BindGTest, Binary: "test_phase16", Filter: "Phase16Gslo.*"},
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestGSLO(PremiumFormula|GapLiability)|TestReservationOfParsesAlgoParams"},
	},
	255: { // dual-price conditional triggers (SL/TP/trailing/bracket arms)
		{Kind: BindGTest, Binary: "test_phase16",
			Filter: "Phase16Triggers.*:Phase16Pump.StopMarketPlusTrailUnitDecodesTrailing"},
		{Kind: BindGoTest, Pkg: "./internal/algo", Run: "TestTriggerGuardStaleMarkFailsClosed"},
	},
	275: { // VP/grid algos: parent risk/lifecycle, child accounting, anti-gaming
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestVP(Validation|NoVolumeSourceFailsClosed)|TestAntiGamingNonDeterministic|TestPerturbSizesNeverNegative|TestPauseResumeCancel|TestIdempotentParentReplay|TestDelayedDispatchStaysPending"},
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestIT(ParentLifecyclePersisted|ChildRowsAndIdempotentCID)", Needs: "pg"},
	},
	287: { // OPO/OPOCO: pending SELL legs sized from locked net working-order proceeds
		{Kind: BindGoTest, Pkg: "./internal/orders", Run: "TestNetPendingQty"},
		{Kind: BindGoTest, Pkg: "./internal/orders", Run: "TestITOrderListActivation", Needs: "pg",
			Note: "OPOCO activation: locked EXECUTING list stamps pending legs via shared dedup/OCO path, flips ALL_DONE with proceeds bookkeeping"},
	},
	296: { // recurring conversion/rebalancing/strategy replication stays firm-CLOB
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestSubmitBasket(EndToEnd|LegCountBounds|UnknownSymbolFailsClosed|SendFailureCompensates)|TestBasketStatusRoundTrip|TestBasketResultConsumerProjection"},
	},
	317: { // complex-order cancel races resolve deterministically; triggers survive
		{Kind: BindGTest, Binary: "test_oco",
			Filter: "OcoEngine.DoomedLegRejectsWithOcoSiblingCancelRace:OcoEngine.LinkConflictAndIdempotentResend:OcoEngine.WalJournalsLinkAndSiblingCancel:OcoEngine.RecoveryReplaysLinkAndSiblingCancel"},
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestSubmitOCO_(RaceReplayReturns409|CollisionRollsBackPair|ReplayIsIdempotent)|TestModifyStaleAndCAS"},
		{Kind: BindGoTest, Pkg: "./internal/algo", Run: "TestDelayedDispatchStaysPending|TestTriggerGuardStaleMarkFailsClosed"},
	},
	365: { // algo open-orders query with child progress; cancel-all, zero orphans
		{Kind: BindGoTest, Pkg: "./internal/algo", Run: "TestListAndCancelAll"},
		{Kind: BindGoTest, Pkg: "./internal/algo",
			Run: "TestIT(ReadSeams|ChildRowsAndIdempotentCID)", Needs: "pg"},
	},
	367: { // composite-list open/history/detail queries, unified envelope
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestBasketStatusRoundTrip|TestBasketResultConsumerProjection"},
		{Kind: BindGoTest, Pkg: "./internal/orders", Run: "TestITOrderListActivation", Needs: "pg",
			Note: "asserts open-vs-history paging partitions on the state set"},
	},
	395: { // pegged orders (Mid/Primary/Market) re-price per book update, bounded
		{Kind: BindGTest, Binary: "test_phase16",
			Filter: "Phase16Peg.*:Phase16Pump.PegFieldsDecodeToAux"},
	},
	399: { // MOO/MOC: queue pre-session, execute at session-open/close uncross
		{Kind: BindGTest, Binary: "test_phase16", Filter: "Phase16Moo.*"},
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestInjectorReopeningCallMOOOnly|TestAuctionSubmitValidation|TestAuctionCancelPreFreeze|TestAuctionFreezeRejectsMutation|TestAuctionAmendPreFreeze|TestAuctionLifecycleNotifications"},
	},
	// ---------- Phase 19 (margin/risk — stragglers) ----------
	33: { // liquidation auction: CALL → FILL → EXTEND → FORCE_CASH lifecycle
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestDedupKeyScoping|TestNewLiquidationQueueNilRedis|TestIsParkable"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgLiquidationStore(AuctionLifecycle|ActiveAuctions)", Needs: "pg",
			Note: "live-stack CALL→FORCE_CASH phase e2e remains a gap — needs mark-price oracle manipulation on the full stack"},
	},
	34: { // auction floor ×0.98/×1.02; 0.5% decay per 5s EXTEND; ≤60s total
		{Kind: BindGoTest, Pkg: "./internal/risk", Run: "TestFloorFor|TestDecayFloor"},
		{Kind: BindGoTest, Pkg: "./internal/risk", Run: "TestPgLiquidationStoreAuctionLifecycle", Needs: "pg"},
	},
	35: { // FORCE_CASH at mark×0.95/×1.05; deficiency to insurance fund
		{Kind: BindGoTest, Pkg: "./internal/risk", Run: "TestForceCashCap|TestPenaltyAndDeficiency"},
	},
	36: { // LP rebate 0.05% on auction fills, paid from insurance fund
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestInsuranceFundCanonicalDefaults|TestInsuranceFundJournalDebits|TestFundMovementDirection"},
		{Kind: BindGoTest, Pkg: "./internal/risk", Run: "TestPgInsuranceFundMovements", Needs: "pg"},
	},
	// ---------- Phase 23 (historical data — stragglers) ----------
	236: { // historical tick data REST: CH-backed cursor-paginated, JSON/CSV/FIX
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestTickStore(Insert|InsertEmptyIsNoop|Query|QueryCursor)|TestTickRowValuesExactScaling"},
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestHistory(AccessForRateTier|DelayFor|FormatFor)|TestRateTierHistoryResolver|TestResolveHistoryWindow|TestWriteTicks(CSV|FIXDropCopy)"},
		{Kind: BindGoTest, Pkg: "./internal/analytics", Run: "TestCHTickStore",
			Note: "live-CH leg — self-skips without EXC_CH_TEST; its gate keeps the criterion at EXECUTABLE-partial until ClickHouse is present"},
	},
	// ---------- Phase 18 (FIX protocol gateway) ----------
	54: { // FIX 4.4 session lifecycle: logon/heartbeat/logout/resend/gap-fill
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestFromAdmin_ValidLogon|TestFromAdmin_UnknownSessionRejected|TestHeartbeatAbnormal|TestPlanResend|TestSessionStateMapping|TestRecordInbound|TestRecordOutbound"},
		{Kind: BindGoTest, Pkg: "./internal/fix", Run: "TestResumeOnLogon|TestClaimSessionFencing", Needs: "redis"},
		{Kind: BindGoTest, Pkg: "./internal/fix", Run: "TestPGStore_SessionLifecycleAndSeq|TestPGStore_MessageArchive", Needs: "pg"},
	},
	55: { // FIX 5.0 SP2 derivative extension
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestSP2|TestApplVerStamp|TestOptionContract|TestNDFRequiresFixingDate|TestSwapLegDates|TestSettlementDateStandardTag|TestForwardMapping"},
	},
	56: { // FX-specific tags: Currency/SettlementType/SettlementDate/NoPartyIDs
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestSettlementDateStandardTag|TestSetFXParties|TestSetParties|TestPartyGroupParse|TestForwardMapping"},
	},
	128: { // Mass Quoting Tag 35=i + mass quote cancel
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "MassQuote|TestQuoteCancel|TestQuoteService|TestQuoteSetLifecycle|TestAskLegFailureUnwindsBid|TestEncodeLPQuoteEvent"},
		{Kind: BindGoTest, Pkg: "./internal/fix", Run: "TestIntegrationLPQuoteFeed|TestJetStreamQuoteSinkSaturation", Needs: "nats"},
	},
	135: { // session entitlement bound to account
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestEntitlementDenied|TestFromApp_UntitledInstrumentRejected|TestMassQuoteRejectsUnentitled"},
		{Kind: BindGoTest, Pkg: "./internal/fix", Run: "TestPGStore_EntitlementUpdateTx", Needs: "pg"},
	},
	136: { // cancel-on-disconnect purges session's resting orders
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestCoD|TestOnLogout_|TestDropSessionPurgesSubscriptions"},
	},
	137: { // per-session max_msgs_per_sec throttle
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestThrottle_|TestFromApp_ThrottledNeverSilentlyDropped"},
	},
	139: { // MM program obligations + MMP mass-cancel
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "MMP|TestMassQuoteLP|TestFromApp_MassQuoteLPSuspended"},
	},
	167: { // FIXS mTLS + certificate→CompID binding
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestCertificateMatrix|TestServerTLSConfigProfile|TestFromAdmin_BadCredentialRejected|TestFromAdmin_CredentialAccountMismatch"},
	},
	189: { // failover + sequence resync <5s (Task 18.3.12)
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestFailoverE2E|TestFailoverResumeLatencyDistribution", Needs: "pg+redis",
			Note: "real Redis lease/claim + PG fix_sessions archive; resume + total wall bands"},
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestFailoverStoreFallsBack|TestRedisSeqStoreIntegration", Needs: "redis"},
	},
	193: { // SOR routes externally when internal liquidity thin
		{Kind: BindGoTest, Pkg: "./internal/sor",
			Run: "TestLocalWhenLiquid|TestTimeoutWalksToNextVenue|TestAllVenuesDeadSORTimeout|TestVenueRejectFailsClosed|TestLoadVenueConfig"},
	},
	194: { // external-venue exec reports map to internal orders
		{Kind: BindGoTest, Pkg: "./internal/sor",
			Run: "TestRouteFillLifecycle|TestPartialFillState|TestFillDedup|TestReleaseForLocal"},
	},
	242: { // SOR 5-state lifecycle, shadow orders, 500ms, FILL_BRIDGE, no concurrent
		{Kind: BindGoTest, Pkg: "./internal/sor",
			Run: "TestRouteFillLifecycle|TestPartialFillState|TestConcurrentRouteGuard|TestTimeoutWalksToNextVenue"},
	},
	243: { // TradingSessionStatus 35=h broadcast ≤50ms + 35=g subscribe
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestBroadcast|TestSubscribeThenBroadcast|TestRapidStateFlap|TestInstrumentStatusMapping|TestUnmappedStatesDoNotBroadcast|TestSymbolScopedSubscription|TestUnsubscribeStopsBroadcasts|TestSnapshotStateUnavailableHonestReject"},
	},
	319: { // seq-gap ResendRequest/SequenceReset + CoD purge on disconnect
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestPlanResend|TestCoDPurges|TestCoDGracefulLogoutPreserves|TestDropSessionPurgesSubscriptions"},
	},

	// ── Phase 10 — frontend surfaces (vitest) ─────────────────────────
	// Each leg runs `npx vitest run <dir-or-file>` inside frontend/.
	// EXECUTABLE = the spec files exist and execute; runtime result
	// recorded separately. Needs "frontend" → node_modules must exist.
	251: { // advanced order UI: TIF/iceberg/trailing/bracket + sub-account + admin fees
		{Kind: BindVitest, Pkg: "src/features/advanced-orders", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/features/order-entry", Needs: "frontend"},
	},
	267: { // calculators: P&L/pip/margin/liq/swap against live marks
		{Kind: BindVitest, Pkg: "src/features/calculator", Needs: "frontend"},
	},
	268: { // Lite/Pro modes, reverse/flatten confirms, % sizing, interactive depth
		{Kind: BindVitest, Pkg: "src/features/workspace", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/features/depth-chart", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/features/advanced-orders/sizing.test.ts", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/lib/input-helpers/confirm.test.tsx", Needs: "frontend"},
	},
	269: { // ADL rank recompute + REST/WS publish (backend legs; UI explain lives in portfolio panels)
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestADLScorerNilLeverage|TestADLScorerFormula|TestADLScorerQuintileBucketing|TestADLScorerExactBuckets|TestADLScorerMarkFallbackChain|TestADLScorerLeverageFailureFailsClosed"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestRedisADLPublisherTick", Needs: "redis"},
	},
	292: { // save/reset layouts, drag orders + overlays with explicit confirm
		{Kind: BindVitest, Pkg: "src/features/workspace", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/features/advanced-orders/TradingChart.test.tsx", Needs: "frontend"},
	},
	293: { // indicators + sandboxed cost-aware backtests, reproducible
		{Kind: BindVitest, Pkg: "src/lib/indicators", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/lib/backtest", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/features/backtesting", Needs: "frontend"},
	},
	294: { // top movers, heatmaps, watchlists, rate alerts
		{Kind: BindVitest, Pkg: "src/features/discovery", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/lib/alerts", Needs: "frontend"},
	},
	295: { // equity/P&L/drawdown dashboard reconciling to ledger statements
		{Kind: BindVitest, Pkg: "src/features/performance", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/features/reports", Needs: "frontend"},
	},
	310: { // WS disconnect backoff, optimistic rollback, stale-data flags
		{Kind: BindVitest, Pkg: "src/lib/ws", Needs: "frontend"},
	},
	351: { // environment switcher + ops/fleet board with dual-control surface
		{Kind: BindVitest, Pkg: "src/lib/env", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/features/ops", Needs: "frontend"},
	},
	382: { // login/registration/2FA/session-list screens + route guards
		{Kind: BindVitest, Pkg: "src/features/auth", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/app/manifest.test.tsx", Needs: "frontend"},
	},
	383: { // account-security center: profile, WebAuthn, anti-phishing, devices
		{Kind: BindVitest, Pkg: "src/features/settings", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/lib/auth", Needs: "frontend"},
	},
	384: { // deposit instructions, withdrawal beneficiary/2FA/confirm, transfers, fee est
		{Kind: BindVitest, Pkg: "src/features/funding", Needs: "frontend"},
	},
	385: { // KYC tracker + doc-upload wizard + re-verification
		{Kind: BindVitest, Pkg: "src/features/kyc", Needs: "frontend"},
	},
	386: { // support tickets + thread + SLA status
		{Kind: BindVitest, Pkg: "src/features/support", Needs: "frontend"},
	},
	387: { // copy browser/follow + grid-bot wizard limits
		{Kind: BindVitest, Pkg: "src/features/copy-grid", Needs: "frontend"},
	},
	388: { // order history, algo mgmt, OPO lists, dead-man, test/preview
		{Kind: BindVitest, Pkg: "src/features/history", Needs: "frontend"},
		{Kind: BindVitest, Pkg: "src/features/advanced-orders/components.test.tsx", Needs: "frontend"},
	},
	389: { // tax report, statements, confirmations, TCA, solvency viewer, fee schedule
		{Kind: BindVitest, Pkg: "src/features/reports", Needs: "frontend"},
	},
	390: { // input-helper framework: OpenAPI-synced validation, instrument-aware format
		{Kind: BindVitest, Pkg: "src/lib/input-helpers", Needs: "frontend"},
	},

	// ── Phase 11 — funding rails leftovers ────────────────────────────
	22: { // 15-min PENDING withdrawal confirm window + token semantics
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestFlowConfirmRequiresTOTP|TestFlowConfirmSessionElevationSkipsToken|TestFlowConfirmWithoutTOTPProviderFailsClosed|TestRequestWithdrawal_TwoPhase|TestWithdrawalIdempotency"},
	},
	23: { // 30-min same-bank-account cooldown
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestFlowCreateCooldownReject|TestGateWhitelistOnlyRejectsUnverified|TestGateWhitelistOnlyAdmitsVerifiedUnlocked"},
	},
	24: { // review tiers: <$10K auto / $10–50K standard / >$50K PENDING_REVIEW+4h
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestWithdrawalNilScreenerStandardTierReviews|TestWithdrawalNilScreenerAutoTierConfirms|TestFlowAdminReviewFourEyes|TestSweepReviewSLAEscalates"},
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestITWithdrawalCaps", Needs: "pg"},
	},
	25: { // deposit anti-fraud tiers + double-confirm + review hold
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestDepositPendingReviewTierAndAdmin|TestDepositGuardResolverUnwiredFailsClosed"},
	},
	79: { // withdrawal caps: per-account daily/hourly, exchange-wide daily
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestITWithdrawalCaps", Needs: "pg"},
	},
	80: { // 24h hold for unverified bank accounts (timelock)
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestGateBeneficiaryTimelock|TestITBankAccountsLifecycle", Needs: "pg"},
	},
	141: { // beneficiary registry: KYC name-match, dual-control verify, third-party rejected
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestBeneficiaryRegister_KYCGate|TestBeneficiaryVerify_DualControl|TestBeneficiaryAssertWithdrawable|TestBeneficiaryRejectAndDelete|TestBankAccountService_NilResolverFailsClosed"},
	},
	164: { // per-rail cut-offs, value-date-aware ETAs
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestAssertRailOperational|TestRailCutoffSameDay|TestRailCutoffWeekendRoll|TestRailMatrixComplete|TestRailSelectionByCurrency|TestRailSelectionPreferredAndUnavailable|TestRailSelectionEligibility"},
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestRailCutoffTimezoneAware|TestRailCutoffEnforceSameDay|TestRailCutoffUnscheduledFailsClosed"},
	},
	224: { // funding fee schedule per rail/currency/direction/tier, flat+pct, min/max caps
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestFeeSchedule|TestFeeQuote|TestConversionQuote"},
	},
	311: { // rail return codes (AC01/AM04/RR04) → structured errors; third-party deposits blocked
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestSwiftReturnMT199|TestSwiftMT103Envelope|TestSwiftMT202BankToBank|TestDepositGuardResolverUnwiredFailsClosed|TestDepositPendingReviewTierAndAdmin"},
	},
	391: { // withdrawal whitelist mode + 24h addition timelock + scoped deactivation
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestGateWhitelistOnlyRejectsUnverified|TestGateWhitelistOnlyAdmitsVerifiedUnlocked|TestGateBeneficiaryTimelock|TestWhitelistEnableDisableLatch|TestWhitelistDisableIdempotent"},
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestITWhitelistLifecycle|TestITWithdrawalFlowWhitelistGate", Needs: "pg"},
	},
	409: { // scoped kill-switches: rail-scoped halt falls through to alternates
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestRailSelectionGateSingleHaltFallsThrough|TestRailSelectionEligibility"},
	},

	// ── Phase 13/13.5 — risk ops + security ───────────────────────────
	38: { // five-scope circuit breaker: triggers + state machine
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestValidBreakerScope|TestMarketWideTrip"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestRedisBreakerIntegration", Needs: "redis"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgBreakerEventStoreIntegration", Needs: "pg"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestAutoHaltLatencySpikeTrips|TestAutoHaltErrorRateSpikeTrips|TestAutoHaltClientRejectionsDoNotTrip|TestAutoHaltResumeAfterClear|TestAutoHaltManualTripNotAttributed"},
	},
	42: { // audit hash chain: SHA-256 prev/payload hash + verify detects tamper
		{Kind: BindGoTest, Pkg: "./internal/audit",
			Run: "TestGenesisPrevHashIsSHA256Empty|TestPayloadHashKnownVector|TestChainHashVector|TestCanonicalRecordID|TestGenesisRowUsesGenesisPrevHash|TestVerifyCleanChain|TestVerifyDetects"},
	},
	186: { // Merkle solvency tree generated + public proof endpoint
		{Kind: BindGoTest, Pkg: "./internal/audit",
			Run: "TestMerkleEmptyDay|TestMerkleSingleLeaf"},
		{Kind: BindGoTest, Pkg: "./internal/reconciliation",
			Run: "TestMerkleKnownVector|TestMerkleCanonicalOrder|TestMerkleProofVerify|TestMerkleTamper|TestMerkleProofBound|TestMerkleZeroBalance|TestMerkleTreeMillionLeaves"},
		{Kind: BindGoTest, Pkg: "./internal/api",
			Run: "TestSolvencyLatestPublic|TestSolvencyLatestNonePublished|TestSolvencyProofAuth|TestAccountSolvencyProof"},
	},
	272: { // privileged API keys expiry control; session velocity/idle limits
		{Kind: BindGoTest, Pkg: "./internal/auth",
			Run: "TestSessionIdleAndAbsoluteTimeouts|TestSessionAccountCapEvictsOldest|TestSessionIPCapEvictsOldest|TestSessionRefreshRotationAndReuseDetection|TestSessionRevokeAllAndList"},
		{Kind: BindGoTest, Pkg: "./internal/api",
			Run: "TestAdminAPIKeyExtendExpiry"},
	},
	313: { // five-tier CB half-open probing + hysteresis (no flapping)
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestAutoHaltResumeAfterClear|TestAutoHaltPersistsReTripsDuringProbe|TestAutoHaltLatencyMeanUnderThresholdNoTrip|TestAutoHaltStoreErrorFailsClosed|TestAutoHaltNilCBFailsConstruction"},
	},
	// 332 (standing vulnerability-disclosure program) stays PLANNED —
	// org/published-scope control; an empty leg list would fabricate
	// EXECUTABLE. 109 (external pentest cadence) stays PLANNED likewise.
	110: { // PII scrub legs (automated scan is the pentest harness below)
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestMaskIP|TestScrubJSONPII"},
	},
	111: { // secret rotation 90d zero-downtime: registry ceiling + rotation race drills
		{Kind: BindGoTest, Pkg: "./internal/security",
			Run: "TestDefaultRegistryCoversInventory|TestRegistryMaxAgeCeiling"},
		{Kind: BindGoTest, Pkg: "./internal/security",
			Run: "TestDrillKeyRotationRaceUnderLoad|TestDrillKeyRotationRaceThroughMiddleware|TestDrillExpiredCredentialReplay"},
	},
	213: { // bare-metal secrets injection: Vault source + Aeron credential seam
		{Kind: BindGoTest, Pkg: "./internal/security",
			Run: "TestVaultSourceRead|TestVaultSourceReadMiss|TestVaultSourceRejectsCleartextAndBadConfig|TestVaultSourceTokenFile|TestVaultSourceDynamicCreds|TestLoadSecretsHappyPath|TestLoadSecretsRequiredMissFailsClosed|TestLoadSecretsOptionalMissToleratedInDev|TestLoadSecretsDevAdapterRefusedWhenRequired|TestSecretsRequiredSemantics"},
		{Kind: BindGoTest, Pkg: "./internal/security",
			Run: "TestDrillVaultPartitionFailClosed|TestDrillVaultPartitionMidLease|TestDrillNoDevFallbackWhenRequired"},
	},
	314: { // pentest harness validates security failure handling end-to-end
		{Kind: BindBin, Binary: "pentest", Needs: "pg+redis+nats"},
		{Kind: BindGoTest, Pkg: "./internal/security",
			Run: "TestDrillExpiredCredentialReplay|TestDrillNoDevFallbackWhenRequired|TestDrillVaultPartitionFailClosed"},
	},
	342: { // full-surface pentest + GDPR erasure runbook legs
		{Kind: BindBin, Binary: "pentest", Needs: "pg+redis+nats"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestGDPRExportAndErasure|TestGDPRConsentLifecycle"},
	},
	// 109 (quarterly external pentest + annual red team) stays PLANNED —
	// org-process evidence, no executable binding honest.

	// ── Phase 14 — client lifecycle / compliance ──────────────────────
	27: { // KYC T0/T1/T2 caps on withdrawal + trading
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestTierRank|TestSubmitSelfCertTierGate|TestApprove_TierAssignedAndNotified"},
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestITWithdrawalCaps", Needs: "pg"},
	},
	101: { // re-verification triggers: expiry, risk change, regulatory update
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestComputeReverifyDue|TestSweepReverify_DowngradesAlertsNotifies|TestSweepReverify_FeedErrorFailsClosed|TestSweepReverify_IdempotentSkip|TestSweepReverify_RowFailureContinues|TestLegalDocs_ExpirySweep"},
	},
	102: { // KYC doc encryption sealed at rest + fail-closed store
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestInsertSelfCertSealFailureFailsClosed"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPgStoreNilBoxFailsClosed|TestITSelfCertSealedAtRest", Needs: "pg"},
	},
	103: { // GDPR erasure/export/consent
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestConsentPurposeValidation"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestGDPRExportAndErasure|TestGDPRConsentLifecycle", Needs: "pg"},
	},
	132: { // MiFID categorization + appropriateness + binary gate
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestAppropriateness.*|TestSetCategory_.*|TestSubmitAssessment_OutcomeAndExpiry"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestITCategorization_RoundTrip", Needs: "pg"},
	},
	195: { // PAMM/MAM pro-rata allocation
		{Kind: BindGoTest, Pkg: "./internal/pamm",
			Run: "Test.*"},
	},
	196: { // copy-trading replication
		{Kind: BindGoTest, Pkg: "./internal/copy",
			Run: "Test.*"},
	},
	204: { // account closure blocked on open exposure; residual sweep
		{Kind: BindGoTest, Pkg: "./internal/accounts",
			Run: "TestIntegrationClosureClientPath|TestIntegrationClosureBlocked|TestIntegrationClosureSweep|TestIntegrationClosureForced|TestIntegrationClosureGuards", Needs: "pg"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestIntegrationHoldEscalateToClosure", Needs: "pg"},
	},
	219: { // compliance hold: sanctions→FROZEN→CO review→release
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestIntegrationHoldEscalateDraftsSAR|TestIntegrationHoldEscalateSAR|TestIntegrationHoldRoleGate", Needs: "pg"},
	},
	273: { // responsible-trading cooling-off irrevocable + de-risk saga
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestCoolingOffRejectsLeveragedEntry|TestCoolingOffGateErrorFailsClosed|TestCoolingOffGateScope|TestCoolingOffBatchEntry|TestCoolingOffModifyEntry|TestCoolingOffPlainErrorFailsClosed"},
		{Kind: BindGoTest, Pkg: "./internal/accounts",
			Run: "TestIntegrationCoolingOffActivateAndGate|TestIntegrationCoolingOffDeriskSaga|TestIntegrationCoolingOffPartialFailure", Needs: "pg"},
	},
	315: { // cooling-off rejects leveraged entry + retries with backoff
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestCoolingOffRejectsLeveragedEntry|TestCoolingOffGateErrorFailsClosed|TestCoolingOffBatchEntry|TestCoolingOffModifyEntry"},
	},
	369: { // product profiles gate scope/denomination + guarded switching
		{Kind: BindGoTest, Pkg: "./internal/ledger",
			Run: "TestProfileSwitchZeroBalanceGuard|Test.*Subunit.*|Test.*Denomination.*"},
	},
	372: { // copy discovery + safety mode + HWM profit-share via PAMM
		{Kind: BindGoTest, Pkg: "./internal/copy", Run: "Test.*"},
		{Kind: BindGoTest, Pkg: "./internal/pamm", Run: "Test.*"},
	},
	373: { // swap-free verification lifecycle
		{Kind: BindGoTest, Pkg: "./internal/ledger",
			Run: "TestSwapAccrualSwapFree"},
	},
	// 376 (per-profile retail target-market dual-gate) — no dedicated test
	// found for the dual-gate path; stays PLANNED rather than overclaim.
	// 369 bound above via the guarded profile-switch leg only.

	// ── Phase 17 — surveillance / L3 data ─────────────────────────────
	18: { // L3 order-level data matches engine state
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestL3SnapshotReader_RebuildsRestingBook|TestL3SnapshotReader_SnapshotPlusTailReplay|TestL3AccountHash_MatchesEngineConvention|TestL3Mirror_ApplyAndSuspect"},
	},
	244: { // L3 snapshot pagination + staleness gate + 413 over ceiling
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestL3SnapshotReader_CursorPagination|TestL3SnapshotReader_StalenessGate|TestL3SnapshotReader_TooLargeCeiling|TestL3SnapshotReader_FailClosed"},
	},
	318: { // L3 stream gap → snapshot recovery; slow consumers drop
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestL3Hub_ReplayGapTooLarge|TestL3Hub_FeedGapMarksMirrorSuspect|TestL3Hub_ReplayWithinHorizon|TestL3Hub_NoConflationEveryEventForwarded"},
	},

	// ── Phase 19/19.5 — margin, liquidation, oracle ───────────────────
	32: { // margin call 15-min window then liquidation
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestCheckAccountReplenishAvertsLiquidation|TestCheckAccountInsufficientReplenishStillLiquidates|TestMarginCallServiceNilDeps|TestMarginCallThresholdsResolution|TestMarginCallEvaluateNoLevel|TestMarginCallAdminReleaseRequiresActor"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgMarginCallLifecycleEndToEnd|TestPgMarginCallStoreEpisodeLifecycle|TestPgMarginCallStoreExpiredScan", Needs: "pg"},
	},
	37: { // liquidation scanner 2s cadence across CROSS/PORTFOLIO accounts
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestNewLiquidationQueueNilRedis"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgLiquidationStoreAccountsForScan|TestPgLiquidationStoreOpenPositions|TestPgLiquidationStoreIsolatedBreaches", Needs: "pg"},
	},
	96: { // portfolio margin cross-currency with FX conversion
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPortfolioMarginCorrelationOffset|TestPortfolioMarginNoOffsetInCross|TestPortfolioMarginRegulatoryFloor|TestPortfolioMarginRealMatrixBinding"},
	},
	97: { // ADL: insurance-fund depletion triggers position reduction
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestADLEngineValidatesRequest|TestADLEngineExecutesTopCounterparties|TestADLEngineNoCounterpartyFailsLoud|TestADLEngineShortfallAndDispatchFailure|TestADLEngineInsertFailureFailsClosed|TestADLReconcileValidation|TestADLReconcileStoreErrorAlerts"},
	},
	125: { // PB give-up: NOP/DSL pre-trade + drop-copy dispatch
		{Kind: BindGoTest, Pkg: "./internal/orders",
			Run: "TestPBGateReserveOnSubmit|TestPBGateBreachRejectsRow|TestPBGateReleaseOnCancel|TestPBGateAdjustOnQtyAmend"},
	},
	133: { // NBP: retail never below zero; shortfall → insurance fund
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestNBPConstruction|TestNBPRetail|TestNBPProfessional|TestNBPHouse|TestNBPNoFundRow|TestNBPLockedDeficit|TestNBPMultiCurrency|TestNBPPositiveEquity|TestNBPNoDeficit|TestNBPPositionalDeficit|TestNBPFundDebit|TestNBPStoreReadFailure|TestNBPRecurring|TestNBPSingleHit|TestNBPAncientHit|TestNBPSweep|TestNBPEvents"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgNBPStoreIntegration", Needs: "pg"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestRedisNBPReviewFlag", Needs: "redis"},
	},
	145: { // collateral haircuts + concentration caps + ineligible currency
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestCollateralValuateHaircut|TestCollateralConcentrationCapsExcess|TestCollateralFullConcentrationSingleCurrency|TestCollateralIneligibleAndAbsentContributeZero|TestCollateralStaleRateContributesZero|TestCollateralDebitBalanceFaceValue"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgCollateralScheduleStore", Needs: "pg"},
	},
	165: { // bilateral credit screening + atomic reservation
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestScreenLiquidity|TestCreditTicks"},
	},
	176: { // cross-shard portfolio margin coherence / atomic reservations
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPortfolioMarginCorrelationOffset|TestPortfolioMarginNoOffsetInCross|TestPortfolioMarginRealMatrixBinding"},
		{Kind: BindGTest, Binary: "test_margin_coordinator", Filter: "MarginCoordinator.*"},
		{Kind: BindGTest, Binary: "test_cross_shard", Filter: "CrossShard2PC.*"},
	},
	190: { // position transfer off-book at mark with GL balance
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestTransferHappyPathFullPosition|TestTransferPartialQuantityProRata|TestTransferRejectsInsufficientSource|TestTransferRejectsCrossEntity|TestTransferHierarchyAllowsDifferentUsers|TestTransferAbortsOnInsufficientDestMargin|TestTransferNettingDestAbsorbsOpposing|TestTransferMissingMarkFailsClosed|TestTransferIdempotentReplay|TestTransferValidationRejectsBadRequests|TestTransferDestLockedClampWhenFreeing"},
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestIntegrationPositionTransferPgxStore", Needs: "pg"},
	},
	206: { // margin model validation: stress suite + daily backtesting
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestBacktestCoverageStamped|TestBacktestCoverageRejectsPathologicalModel|TestRunStressSuitePersistsAndAlerts|TestStressSchedulerRunDue|TestStressEngineConstructionFailClosed"},
	},
	218: { // insurance fund governance: capitalization, target, replenishment
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestInsuranceFundNewFailClosed|TestInsuranceFundJournalLiquidationPenalty|TestInsuranceFundJournalHouseFunding|TestInsuranceFundJournalDebits|TestInsuranceFundMovementValidation|TestInsuranceFundCanonicalDefaults"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgInsuranceFundMovements|TestPgInsuranceFundDepleted|TestPgInsuranceFundIdempotentReplay|TestPgRedisInsuranceFundWalletDebit", Needs: "pg+redis"},
	},
	228: { // retail NBP reset + insurance debit + house fallback
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestNBPConstruction|TestNBPRetail|TestNBPProfessional|TestNBPHouse|TestNBPNoFundRow|TestNBPLockedDeficit|TestNBPMultiCurrency|TestNBPPositiveEquity|TestNBPNoDeficit|TestNBPPositionalDeficit|TestNBPFundDebit|TestNBPStoreReadFailure|TestNBPRecurring|TestNBPSingleHit|TestNBPAncientHit|TestNBPSweep|TestNBPEvents"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgNBPStoreIntegration", Needs: "pg"},
	},
	229: { // netting/hedging account-mode toggle
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPositionModeReads|TestPositionModeSetModeGuards|TestPositionModeStoreErrors"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgPositionModeStore|TestPgPositionModeServiceGuards", Needs: "pg"},
	},
	230: { // margin level % push + ESMA thresholds
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestMarginLevelHeapOrdering|TestMarginLevelHeapChurn|TestMarginLevelThresholdHelpers|TestMarginLevelViewService|TestMarginLevelWatcherPublishesOnlyOnChange|TestMarginLevelReaderNilClient|TestMarginCoordinatorSetAccountMarginPush"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestRedisMarginLevelReaderRoundTrip", Needs: "redis"},
	},
	231: { // tiered leverage by notional bands
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestLeverageTierAt|TestLeverageBandMargin|TestLeverageTierEnums|TestLeverageResolveTierBandStepsDown"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgTierStore", Needs: "pg"},
	},
	232: { // correlation-based margin offset, 90d matrix, floor
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestCorrelationMatrixRefreshAndOffset|TestCorrelationGroups|TestCorrelationFactorClampAndDefault|TestCorrelationAuditPerEpoch|TestCorrelationStalenessGate"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestRedisCorrelationMatrix", Needs: "redis"},
	},
	241: { // proportional liquidation slicing: ADV caps + inter-slice delay
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestCloseTranchesNilADVUnsliced|TestCloseTranchesBelowTriggerUnsliced|TestCloseTranchesMissingADVFallsBack|TestCloseTranchesSlicedAtTenPercentADV|TestCloseTranchesADVErrorFallsBack"},
	},
	320: { // cross-shard margin 2PC timeout releases + NBP deficit restore
		{Kind: BindGTest, Binary: "test_cross_shard",
			Filter: "CrossShard2PC.ReserveTimeoutFullCompensation:CrossShard2PC.NackCompensatesAll:CrossShard2PC.ThreeShardAllOrNothingCommit:CrossShard2PC.ReaperCadenceIsTwoSeconds:CrossShard2PC.CommitLosingTtlRaceCompensatesCommittedLeg"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestNBPConstruction|TestNBPRetail|TestNBPSweep|TestNBPEvents|TestNBPStoreReadFailure"},
	},
	344: { // margin changes independent validation + stress-tied insurance target
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestMarginEvaluate.*|TestExposureCheckValidation|TestRunStressSuitePersistsAndAlerts"},
	},
	360: { // per-account liquidation history w/ economics + audit refs
		{Kind: BindGoTest, Pkg: "./internal/api",
			Run: "TestAccountLiquidations_OK|TestAccountLiquidations_ForeignAccount403"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgLiquidationStore.*", Needs: "pg"},
	},
	366: { // runtime leverage/margin-mode change within caps + guards
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestLeverageSetLeverage.*|TestLeverageResolve.*|TestLeverageInvalidate"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgLeverageStoreResolve|TestPgLeverageSetLeverage", Needs: "pg"},
	},
	371: { // entity-level leverage caps, most-restrictive-wins
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestLeverageResolveMostRestrictive|TestLeverageResolveRegulatoryCaps|TestLeverageResolveEntityPolicyFallbackStrictest|TestLeverageResolveInstrumentCeilingAndNonMarginable|TestLeverageResolveProfessionalNoStatutoryCap|TestLeverageResolveFailClosed|TestLeverageClassifyInstrumentGroup|TestLeverageRegulatoryRegime"},
	},
	398: { // option-delta margin linkage + spread offsets
		{Kind: BindGoTest, Pkg: "./internal/margin",
			Run: "TestDetectVerticalSpread_MaxLossBound|TestDetectVerticalSpread_PutAndPartialQty|TestDetectCalendar_ShortNearLongFar|TestDetectCalendar_ReverseNotRecognized|TestSpreadParams_CustomOffset"},
		{Kind: BindGoTest, Pkg: "./internal/margin",
			Run: "TestPgSpreadOffsets_PortfolioModeGateAndPersist|TestPgSpreadOffsets_NonPortfolioGetsNoOffsets|TestPgSpreadOffsets_MakerCheckerParams|TestPgSpreadOffsets_DownMigration", Needs: "pg"},
	},
	410: { // event-driven mark-price margin engine, immediate dispatch
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestCollateralMonitorTriggerRecomputesAndCalls|TestCollateralMonitorBelowTriggerStaysQuiet|TestCollateralMonitorNormalStatusSkipsCallSeam|TestCollateralMonitorCooldownBoundsFanout|TestCollateralMonitorDayRolloverReanchors|TestCollateralMonitorUnwatchedSymbolIgnored|TestCollateralMonitorEvalErrorStreakPages"},
	},
	411: { // isolated margin mode confines risk to allocated margin
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestIsolatedLevelPct|TestIsolatedTopUpUSD|TestIsolatedBoundaryMark"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestPgIsolatedMarginStoreLifecycle", Needs: "pg"},
	},
	412: { // intraday dynamic haircut re-evaluation on >100bps moves
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestCollateralMonitor.*|TestCollateralUpdateScheduleAuditAndPropagate|TestCollateralUpdateScheduleValidation|TestCollateralScheduleColdFailureFailsClosed|TestCollateralScheduleStaleCacheFallback"},
	},
	// Phase 19.5 — oracle
	45: { // ≥2 independent sources, 5s staleness gate, fail-closed
		{Kind: BindGoTest, Pkg: "./internal/oracle",
			Run: "TestServiceRequiresTwoFeeds|TestMedianOfTwoFeeds|TestIndexVolumeWeighted|TestStalenessGate|TestAllStaleUnavailable|TestDivergenceExclusion|TestNonPositiveQuoteRejected|TestPerSymbolFreshness|TestFeedErrorCounted"},
		{Kind: BindGoTest, Pkg: "./internal/oracle",
			Run: "TestRedisProviderReadAndGate|TestRedisPublishMarkKeys|TestRedisFallbackPublication", Needs: "redis"},
	},
	120: { // stale mark → liquidation fails closed
		{Kind: BindGoTest, Pkg: "./internal/oracle",
			Run: "TestFixingMarkSource_StaleMarkRefused|TestFixingMarkSource_AbsentMarkRefused|TestFixingMarkSource_NonPositiveMarkRefused|TestFixingMarkSource_UnwiredIsFailClosed|TestFallbackTrackerStaleReference|TestAllStaleUnavailable"},
	},
	134: { // yield-curve/day-count feeds per currency for forwards + Tom-Next
		{Kind: BindGoTest, Pkg: "./internal/derivatives",
			Run: "TestForwardRateDCCBoundaryGBPUSD|TestForwardRateParityIdentity|TestForwardRateRoundTripConsistency|TestForwardRateUSDJPYBoth360|TestDayCountBoundary|TestForwardRateRejectsNonPositiveInputs|TestPricerMissingCurveFailsClosed"},
	},
	321: { // multi-provider oracle staleness + divergence flag
		{Kind: BindGoTest, Pkg: "./internal/oracle",
			Run: "TestStalenessGate|TestAllStaleUnavailable|TestDivergenceExclusion|TestServiceRequiresTwoFeeds|TestFeedErrorCounted"},
	},
	397: { // stale-price liquidation fallback with tiered haircuts
		{Kind: BindGoTest, Pkg: "./internal/oracle",
			Run: "TestFallbackTrackerStaleReference|TestFlashCrashFreeze|TestFlashCrashNotArmedOnSlowMove"},
		{Kind: BindGoTest, Pkg: "./internal/risk",
			Run: "TestStaleFallbackTiersMirror|TestCollateralStaleRateContributesZero|TestCollateralScheduleStaleCacheFallback"},
	},

	// ── Phase 20 — reporting/analytics ────────────────────────────────
	65: { // ClickHouse 50k inserts/s sustained (CH-gated bench; self-skips w/o EXC_CH_TEST)
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestCHIngestThroughput"},
	},
	67: { // real-time P&L per trade streamed
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestPnLUpsertBatchesRows|TestPnLUpsertVersionMonotonicAcrossCalls|TestPnLUpsertEmptyAndNilConn|TestPnLUpsertSendErrorPropagates|TestPnLReportAggregationQuery|TestPnLReportErrors"},
	},
	68: { // analytics dashboard: latency, throughput, fill rate, P&L
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestCanonicalPositionsJSON|TestSnapshotHashDeterministic|TestSyncOrdersCountWritesDayDelta|TestTradesPerTierFoldsAccountsIntoCategories|TestPnLReportAggregationQuery"},
	},
	143: { // client statements: per-trade confirm + daily/monthly delivery
		{Kind: BindGoTest, Pkg: "./internal/reporting",
			Run: "TestOnFill_GeneratesAndDispatchesInstitutional|TestOnFill_RetailWaitsForT1|TestOnFill_GenerationErrorPropagates|TestOnFill_SLAViolationCounted|TestOnTradeAmended_DispatchesImmediately|TestDelivery_DeliverAllChannels|TestDelivery_MT515SkippedForRetail|TestScheduler_T1SweepAndDeadLetter|TestRenderConfirmationHTML_Jurisdictions|TestMT515Doc_DeterministicAndComplete|TestDispatchDue|TestNextBusinessDay_SkipsWeekend"},
		{Kind: BindGoTest, Pkg: "./internal/reporting",
			Run: "TestPgConfirmationTracker_RoundTrip|TestPgLookups", Needs: "pg"},
	},
	205: { // house finance: trial balance, finance P&L, reconciliation
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestClassifyAccount_StructuralGLCrossCheck|TestDailyReconciliation_BalancedAndSignedOff|TestDailyReconciliation_ExternalLeg"},
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestPgTrialBalanceEmptyDay|TestPgReconcileToStatement", Needs: "pg"},
	},
	235: { // trade confirmation portal + email/MT515 delivery (MiFID Art.25)
		{Kind: BindGoTest, Pkg: "./internal/reporting",
			Run: "TestDelivery_DeliverAllChannels|TestDelivery_MT515SkippedForRetail|TestDelivery_EmailFailureLeavesGenerated|TestDelivery_NoRecipientSkipsEmail|TestMT515Doc_DeterministicAndComplete|TestRenderConfirmationHTML_Jurisdictions"},
	},
	246: { // TCA: slippage vs arrival/VWAP/ECB fix + RTS 28 auto-generation
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestTCA_ArrivalSlippageBuy|TestTCA_ArrivalSlippageSellSign|TestTCA_VWAPSlippage|TestTCA_ECBFixSlippage|TestTCA_MissingFixRecordedNotFabricated|TestTCA_PriceImprovementFromPreventionEvent|TestTCA_PriceImprovementFromLimitFallback|TestTCA_AbsentSourcesYieldNullRow|TestTCA_SinkFailurePropagates|TestMemReportStore_PeriodAggregation|TestClassifyPair|TestSessionWindow_Boundary|TestRTS28Job_PersistsRollupAndArchives|TestRenderRTS28PDF_VenueSection"},
	},
	276: { // tax calculator + anonymized sentiment with privacy floor
		{Kind: BindGoTest, Pkg: "./internal/tax",
			Run: "TestComputeFIFO|TestComputeLIFO|TestComputeHIFO|TestComputeAvgCost|TestComputePreYearLotBasis|TestComputeFees|TestComputeShortRoundTrip|TestParseMethod|TestReportValidation|TestServiceReport|TestRenderCSV|TestRenderPDF|TestReportRangeWindow|TestReportFlowsSurface|TestRenderKoinlyCSV|TestDailyLimiterInterface"},
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestMarketingReport_CohortFloor"},
		{Kind: BindGoTest, Pkg: "./internal/api",
			Run: "TestLongShortRatioCohortFloor422"},
	},
	322: { // ClickHouse backpressure RocksDB spool + dedup replay
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestInsertWithSpoolDiverts"},
	},
	361: { // income ledger by type/symbol/time with GL linkage
		{Kind: BindGoTest, Pkg: "./internal/api",
			Run: "TestAccountIncomeRows|TestAccountIncomeFailClosedCHDown|TestAccountIncomeEmptyProjection503|TestAccountIncomeValidation"},
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestMapIncomeType|TestPollIncomeOnceProjectsSignedRows|TestCHIncomeProjectionShape|TestClassifyIncomeTable|TestValidIncomeType|TestIncomeStoreInsertBatches|TestIncomeStoreQueryPredicateShape|TestIncomeStoreFailClosed|TestIncomeWatermarkEmpty|TestCHIncomeRoundTrip|TestPgIncomeSyncRowsClassification", Needs: "pg"},
	},
	362: { // daily hash-chained account snapshots + history API
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestSnapshotHashDeterministic|TestCanonicalPositionsJSON"},
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestPgSnapshotBuildVerifyChain|TestPgSnapshotHistory", Needs: "pg"},
	},
	374: { // ex-ante cost preview + ex-post reconcile, no-inducement
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestCostPreview_SpreadMarkupModel|TestCostPreview_RawSpreadCommission|TestCostPreview_FinancingLeg|TestCostAnnual_ReconcilesLedgerRows"},
	},
	375: { // retail −10% leverage notifications same-day + episode dedupe
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestDepreciation_CrossingNotifiesOnce|TestDepreciation_ShortSide|TestDepreciation_FlatWithoutCrossingClosesEpisode"},
	},
	381: { // promo inventory SLA flags + consent cohorts ≥100 floor
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestMarketingReport_CohortFloor"},
		{Kind: BindGoTest, Pkg: "./internal/api",
			Run: "TestAdminPromotionsReportOK|TestAdminPromotionsReportSourceDown"},
		{Kind: BindGoTest, Pkg: "./internal/promos",
			Run: "TestIntegrationPromoLifecycle|TestIntegrationClearExpired", Needs: "pg"},
	},

	// ── Phase 21 — compliance/reporting ───────────────────────────────
	28: { // sanctions real-time fail-closed on timeout
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestListScreenerFailClosedContract|TestSubmitScannerFailClosed|TestProviderGateFailClosed|TestQueueReplayerFailClosed|TestVendorClients_FailClosed"},
	},
	29: { // sanctions FLAG blocks withdrawal + audit
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestWithdrawalSanctionsScreensBeneficiaryName|TestWithdrawalCleanScreenConfirms"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestScreenOnboardingSanctionsHit"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestITSanctionsDepositBlocks|TestITSanctionsWithdrawalBlocks|TestITSanctionsCleanCounterpartyPasses|TestIntegrationHoldSanctionsSLAAndStack", Needs: "pg"},
	},
	30: { // MiFID II trade reporting fields + timeliness
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestReport_MiFID2"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestMiFIDReporter|TestRecordExecution_Spot_MiFIDOnly|TestRecordExecution_Idempotent"},
	},
	31: { // EMIR derivatives reporting to TR
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestReport_EMIR"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestEMIRReporter|TestEMIRReporter_EnrichNEWT|TestDoddFrankReporter|TestRecordExecution_Derivative_RegimeFanout|TestRecordExecution_NoEnricherQuarantinesDerivative"},
	},
	104: { // geo-block IP-based jurisdiction restriction
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestGeoGateNilResolverPasses|TestGeoGateResolverErrorFailsClosed|TestGeoGateBlockAndAllow|TestGeoGateRetailBlockMatrix|TestGeoGateRetailBlockAuthedWithoutCategoryFailsClosed|TestCIDRResolverLongestPrefix"},
	},
	105: { // CTR auto-generated ≥$10K
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestReport_FinCEN_CTR_DateFilter"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestIntegrationAMLScanCTRAndStructuring", Needs: "pg"},
	},
	106: { // MiFID II best-execution quality report
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestRTS28Job_PersistsRollupAndArchives|TestRenderRTS28PDF_VenueSection|TestMemReportStore_PeriodAggregation"},
	},
	107: { // travel rule ≥$1,000 originator/beneficiary info
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestTravelRuleInScope|TestTravelRuleMissingFields|TestTravelRuleMergePartyStoredWins"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestIntegrationTravelRuleOutbound|TestIntegrationTravelRuleBelowThreshold|TestIntegrationTravelRuleInbound", Needs: "pg"},
	},
	108: { // SAR filing workflow dual control
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestSARDraftValidation"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestIntegrationSARLifecycle|TestIntegrationSARFromOpenSignals|TestIntegrationHoldEscalateDraftsSAR|TestIntegrationHoldEscalateSAR", Needs: "pg"},
	},
	149: { // PEP + adverse media + monitoring → SAR triggers
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPEPKindSeparation|TestScreenOnboardingPEPHitRoutesToReview|TestAdverseMediaSeverityRouting|TestMonitoringServiceEvaluateAndDormantSweep"},
	},
	150: { // RTS 6: algo cert + self-assessment + DEA + 5y retention
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPGRTS6CertGate|TestPGRTS6DEA", Needs: "pg"},
	},
	151: { // Basel III capital adequacy + leverage ratio reporting
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestReport_Basel3"},
	},
	169: { // EMIR REFIT ISO 20022 TR reports + UTI/UPI + lifecycle + ack
		{Kind: BindGoTest, Pkg: "./internal/compliance/reporting",
			Run: "TestRecordExecution_Spot_MiFIDOnly|TestRecordExecution_Derivative_RegimeFanout|TestRecordExecution_EnrichErrorFailsClosed|TestRecordExecution_NoEnricherQuarantinesDerivative|TestRecordExecution_Idempotent|TestRecordExecution_UTICollision|TestUTIFor_DeterministicAndValid"},
		{Kind: BindGoTest, Pkg: "./internal/compliance/reporting",
			Run: "TestPgStore_Lifecycle_EndToEnd", Needs: "pg"},
	},
	170: { // CFTC Parts 43/45 lifecycle + correction workflow
		{Kind: BindGoTest, Pkg: "./internal/compliance/reporting",
			Run: "TestUSIFor_Format|TestRecordExecution_UTICollision"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestDoddFrankReporter"},
	},
	174: { // regulated-venue controls: member/DEA due diligence + emergency actions
		{Kind: BindGoTest, Pkg: "./internal/compliance/venue",
			Run: "TestMemberInputValidate|TestAdmissionGate_NilServiceFailsClosed|TestCaseTransitions_LifecycleShape"},
	},
	178: { // MiFID II APA/ARM RTS 1/2 transparency + RTS 22
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestAPAClient_Submit|TestARMClient_SubmitXML|TestDispatcher_ACK|TestDispatcher_NACKOpensRepair|TestDispatcher_TransportError_Backoff|TestDispatcher_UnconfiguredEndpoint_FailClosed|TestDispatcher_AsyncPending"},
		{Kind: BindGoTest, Pkg: "./internal/compliance/reporting",
			Run: "TestPayloadForDest_APAVariant"},
	},
	// 182 (FX Global Code 55-principle assessment) stays PLANNED —
	// FXGCService exists but no dedicated test asserts the 55-principle
	// lifecycle; do not fabricate.
	184: { // data residency EU/UK/US partitioning
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestResidencyResolveCountry|TestResidencyResolveFailClosedWithoutROW|TestResidencyReplicationLegality|TestResidencyVerifyPlacement"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestResidencyAuthorizeAccess", Needs: "pg"},
	},
	202: { // RTS 27/28 quarterly venue quality + top-5 report
		{Kind: BindGoTest, Pkg: "./internal/analytics",
			Run: "TestRTS28Job_PersistsRollupAndArchives|TestRenderRTS28PDF_VenueSection"},
		{Kind: BindGoTest, Pkg: "./internal/marketdata",
			Run: "TestVenuePerformanceReport|TestVenuePerformanceWeeklyRollup|TestVenuePerformanceDivergenceHoldsLastGood|TestVenuePerformanceDivergenceWithoutLastGood"},
	},
	203: { // comms recording taping, WORM hash-chained
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestCommsChainHashDeterministicAndLinked|TestCommsRetentionFloor"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestCommsRecordingLifecycle", Needs: "pg"},
	},
	207: { // surveillance case mgmt: signal→case→disposition
		{Kind: BindGoTest, Pkg: "./internal/compliance/venue",
			Run: "TestCaseTransitions_LifecycleShape"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPGCaseLifecycle|TestPGCaseMonitoringSink|TestPGEnforcementWarnDismiss|TestPGEnforcementGuards", Needs: "pg"},
	},
	249: { // CRS/FATCA annual reports FIFO P&L + schema validation + dual sign-off
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestTaxXMLCRSGolden|TestTaxXMLFATCAGolden|TestTaxReportLifecycle", Needs: "pg"},
		{Kind: BindGoTest, Pkg: "./internal/tax",
			Run: "TestComputeFIFO|TestComputeShortRoundTrip|TestReportValidation|TestServiceReport|TestRenderCSV|TestRenderPDF"},
	},
	323: { // sanctions downtime quarantines funding (scoped degradation)
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestListScreenerFailClosedContract|TestProviderGateFailClosed|TestQueueReplayerFailClosed|TestVendorClients_FailClosed"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestIntegrationHoldSanctionsSLAAndStack|TestITSanctionsDepositBlocks|TestITSanctionsWithdrawalBlocks", Needs: "pg"},
	},
	327: { // employee trading pre-clearance + restricted lists + blackout
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPGEmployeeDealingGate|TestPGRestrictedWindowGate", Needs: "pg"},
	},
	328: { // regulatory-change tracking + triage + impact assessment
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestRegChangeConstants"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPGRegChangeLifecycle", Needs: "pg"},
	},
	333: { // financial promotions pre-approved + external complaint routing
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPromotionLifecycle|TestPromoChecklistAllMandatory"},
		{Kind: BindGoTest, Pkg: "./internal/support",
			Run: "TestComplaintRouting|TestComplaintAckSLA_StatutoryBoundWinsOnWeekend|TestComplaintAckSLA_InternalClockWinsMidweek"},
	},
	345: { // CFTC limits + LEI/UTI validation + APA/ARM buffering + tuning
		{Kind: BindGoTest, Pkg: "./internal/compliance/reporting",
			Run: "TestUTIFor_DeterministicAndValid|TestUSIFor_Format|TestPayloadForDest_APAVariant"},
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPGTuningLifecycle|TestPGReportingValues", Needs: "pg"},
	},
	377: { // published execution policy + consent gate + annual review
		{Kind: BindGoTest, Pkg: "./internal/compliance",
			Run: "TestPGExecutionPolicyLifecycle", Needs: "pg"},
	},
	392: { // surveillance lag >10K → SURVEILLANCE_LAG_WARNING + latency accounting
		{Kind: BindGoTest, Pkg: "./internal/surveillance",
			Run: "TestWatchLagFiresThenResolves|TestWatchLagBelowThresholdSilent|TestDetectionLatencyAccounting"},
	},

	// ── Phase 24 — settlement/backoffice ──────────────────────────────
	19: { // T+1/T+2 instructions per trade
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestSettlementT1PlainWeekday|TestSettlementT1WeekendShift|TestSettlementT2InterimHoliday|TestGenerateInstructionsT1MondayToTuesday|TestGenerateInstructionsT2|TestGenerateInstructionsMissingNostroFailsClosed|TestGenerateInstructionsQueuedPastCutoff"},
	},
	20: { // nostro debited/credited on settlement
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestDispatchSufficientNostro|TestDispatchInsufficientNostroQueues|TestDispatchNonConfirmedSkips|TestNostroCoverageDeficit"},
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestITNostroDispatch", Needs: "pg"},
	},
	21: { // recon detects discrepancies >$1K or 0.01%
		{Kind: BindGoTest, Pkg: "./internal/reconciliation",
			Run: "TestBalancesChecker|TestBalancesCheckerNilSource|TestGeneralLedgerClean|TestGeneralLedgerImbalance|TestEngineMismatchAlertsAndHalts|TestReconciliationFullCycle|TestReconTestScopedExclusion|TestTabletopReconMismatch|TestSettlementChecker|TestFundingChecker|TestFundingStatementLeg|TestTradesCheckerDiff"},
	},
	78: { // nostro-aware withdrawals + dual-control replenishment
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestDispatchSufficientNostro|TestDispatchInsufficientNostroQueues|TestNostroCoverageDeficit"},
	},
	98: { // GROSS-NET configurable per instrument
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestGrossNet.*|Test.*GrossNet.*"},
	},
	126: { // CLS MT300/MT304 + PvP tracking
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestClsSubmitDispatchMatched|TestClsNilMemberFailsClosed|TestClsMissingRefDataFailsClosed|TestClsIneligibleWaterfall|TestClsControlledGrossBreachAlerts|TestClsFinalityRequiresAuthentication|TestClsMemberStatusUnmatchedOpensBreak|TestNettingClsExclusion"},
	},
	155: { // bilateral netting per counterparty+currency+date + SSI registry
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestNettingRunAggregates|TestNettingClsExclusion|TestNettingAgreementRequired|TestNettingDispatchCutoffGate|TestNettingBustReopens|TestSsi.*"},
	},
	168: { // CLS ISO 20022 paired instructions submit/amend/rescind
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestCls.*"},
	},
	172: { // bunched allocation: fair method, avg price, partial-fill, claim/reject
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestAllocationNew_ManualAccepted|TestAllocationNew_OverAllocationRejected|TestAllocationNew_UnderAllocationRejected|TestAllocationNew_DeclaredQtyMismatch|TestAllocationNew_UnknownAccount|TestAllocationNew_UnknownExecRef|TestAllocationNew_NoRefs|TestParseAllocationInstruction.*"},
	},
	173: { // client-money safeguarding + daily recon
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestEvaluateSegregation_RealtimeShortfall|TestDailyReconciliation_BalancedAndSignedOff|TestDailyReconciliation_ExternalLeg|TestClassifyAccount_StructuralGLCrossCheck"},
	},
	175: { // MT940/MT942/camt.053 ingest + auto-match nostro GL
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestMT940Parse|TestMT940MissingMandatoryTagFailsClosed|TestMT942Parse|TestCamt053Parse|TestCamt053RejectsWrongNamespace|TestCamt053MalformedFailsClosed"},
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestStatementUnmatchedCreditRoutesSuspense|TestStatementMissingPaymentBreak"},
	},
	237: { // FIX 35=J block allocation to sub-accounts + confirms
		{Kind: BindGoTest, Pkg: "./internal/fix",
			Run: "TestAllocationNew.*|TestParseAllocationInstruction.*"},
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestPgAllocations_Integration", Needs: "pg"},
	},
	240: { // client-money shortfall 4-tier waterfall + Tier3+ notification
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestShortfall_Tier1CoversFully|TestShortfall_Tier2PendingAndDualControl|TestShortfall_ExceedsTier2_RaisesTier3Notice|TestShortfall_BlocksMovements_503|TestSweepDeadlines_EscalationAndTier4"},
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestClsControlledGrossBreachAlerts"},
	},
	326: { // CLS match discrepancies quarantine + segregation shortfall trigger
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestClsMemberStatusUnmatchedOpensBreak|TestClsControlledGrossBreachAlerts"},
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestEvaluateSegregation_RealtimeShortfall|TestShortfall_ExceedsTier2_RaisesTier3Notice|TestShortfall_BlocksMovements_503"},
	},
	329: { // venue own funds + contingent capital + 5-day stressed buffer
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestOwnFunds_LedgerAndReconciliation|TestCommitments_WaterfallAndBackstop|TestInsurancePolicy_ExpiryAlert|TestLiquidityBreach_FreezesOutflowsAndLP|TestAdmissionGate_FundedOnly|TestTreasury_RoleEnforcement"},
		{Kind: BindGoTest, Pkg: "./internal/api",
			Run: "TestContingentCapitalCreate_DecimalWire"},
	},
	330: { // independent client-money audit + evidence pack
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestAudit_RegisterLifecycleAndLogs|TestEvidencePack_SystemAssembledAndHashed|TestCertification_DualControlAndReleaseGate|TestExternalAuditor_TimeBoundedDualControlledAudited|TestAssurance_NilDepsFailClosed"},
	},
	347: { // break aging + suspense SLA + dual-control write-offs + nostro backup routing
		{Kind: BindGoTest, Pkg: "./internal/backoffice",
			Run: "TestException_DetectFailureFlagsLegAndOpensException|TestException_DetectOnSettledLegConflicts|TestException_InvestigateAssigns|TestException_ResolveRetryReArmsLeg|TestException_ResolveReversalReturnsFunds|TestException_ResolveTerminalConflicts|TestException_RequestResolutionRequiresDualQueue|TestException_RequestResolutionSubmitsFinanceOpsDual|TestException_RequestResolutionRejectsTerminalAndWriteOff|TestException_NilStoreFailsClosed|TestSweepDeadlines_EscalationAndTier4"},
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestSuspenseNilGuardFailsClosed|TestSuspenseRouteUnmatchedCredit|TestSuspenseResolvePassthrough"},
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestITSuspenseAndRailPayments", Needs: "pg"},
	},
	413: { // rail cut-off rolls settlement value dates forward
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestRailCutoffWeekendRoll|TestRailCutoffEnforceSameDay|TestGrossNetDispatchCutoffRollsSameDayLeg|TestGenerateInstructionsQueuedPastCutoff|TestNextWeekdayCutoff_WeekendSkip|TestRollValueDateAndDaysBetween"},
	},
	414: { // unmatched deposits → GL suspense + compliance quarantine
		{Kind: BindGoTest, Pkg: "./internal/settlement",
			Run: "TestStatementUnmatchedCreditRoutesSuspense|TestSuspenseRouteUnmatchedCredit|TestSuspenseResolvePassthrough|TestSuspenseNilGuardFailsClosed"},
		{Kind: BindGoTest, Pkg: "./internal/funding",
			Run: "TestITSuspenseAndRailPayments", Needs: "pg"},
	},
}
