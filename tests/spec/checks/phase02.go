package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-02 completed tasks (all their checkboxes are `[x]` in
// docs/Phase-02-Matching-Engine.md): every `[x]` checkpoint MUST have a
// registered implementation — an unregistered `[x]` checkpoint reports
// status=missing and fails CI. Checkpoints for tasks still `[ ]` (2.3.8,
// 2.3.11, 2.3.13–2.3.18, 2.3.20–2.3.22, 2.3.25, 2.3.26) stay pending and
// are deliberately NOT registered here.

func registerPhase02(r *spec.Registry) {

	// ---- Task 2.3.1: Order Book ------------------------------------------
	r.Register("P02-T2.3.1-C1", ckOrderBookStructure,
		"flat array price levels + intrusive linked-list orders — spec §3.1")
	r.Register("P02-T2.3.1-C2", ckOrderBookZeroAlloc,
		"zero allocations in hot path — orders exclusively from MemoryPool")
	r.Register("P02-T2.3.1-C3", ckOrderBookFifo,
		"price-time priority (FIFO at each level) — spec §3.1, §24 #1")

	// ---- Task 2.3.2: Matching Engine --------------------------------------
	r.Register("P02-T2.3.2-C1", ckMatchPriceTime,
		"price-time priority matching — limit/market/stop in MatchingEngine")
	r.Register("P02-T2.3.2-C2", ckSelfTradePrevention,
		"self-trade prevention — SelfTradeGuard stp_mode dispatch (§6.5)")
	r.Register("P02-T2.3.2-C3", ckFokIocSemantics,
		"FOK/IOC semantics — fill-or-kill atomic, IOC remainder cancel")
	r.Register("P02-T2.3.2-C4", ckIcebergSlices,
		"ICEBERG visible/hidden slices with replenish — IcebergManager")

	// ---- Task 2.3.3: Pre-Trade Risk (In-Process) ---------------------------
	r.Register("P02-T2.3.3-C1", ckPreTrade14Checks,
		"14 pre-trade risk checks in-process, ordered cheapest-first — spec §3.3")
	r.Register("P02-T2.3.3-C2", ckPreTradeLatency,
		"sub-10µs risk check latency — p99 gate in test_pretrade")

	// ---- Task 2.3.4: Binary WAL Integration --------------------------------
	r.Register("P02-T2.3.4-C1", ckWalPerStateChange,
		"WAL entry per state change with CRC32 — spec §3.4/§3.5")
	r.Register("P02-T2.3.4-C2", ckSnapshotWalRecovery,
		"snapshot + WAL replay exact recovery — RecoveryManager")
	r.Register("P02-T2.3.4-C3", ckRecoveryNoDupMiss,
		"zero dup/miss on recovery — idempotent seq replay, fail-closed gaps")

	// ---- Task 2.3.5: Leader Election ----------------------------------------
	// Doc/stub text still says "Redis SETNX with 10s TTL" — that contract is
	// SUPERSEDED by spec §18.6.2 (remediation #35): epoch-lease, TTL 2,000ms,
	// refresh 500ms, 64-bit monotonic fencing epoch, token-checked Lua
	// compare-and-del (RedisLeaseStore). The checkers assert the canonical
	// epoch-lease implementation, not the legacy SETNX wording.
	r.Register("P02-T2.3.5-C1", ckEpochLeaseElection,
		"epoch-lease leader election (§18.6.2: 2s TTL/500ms refresh/64-bit fencing) — supersedes stub's SETNX 10s wording")
	r.Register("P02-T2.3.5-C2", ckSplitBrainFailClosed,
		"split-brain detection fail-closed — foreign holder → both stop matching")

	// ---- Task 2.3.6: Degradation Mode Manager -------------------------------
	r.Register("P02-T2.3.6-C1", ckDegradationModes,
		"6 degradation modes with correct triggers/recovery — canonical PascalCase set")
	r.Register("P02-T2.3.6-C2", ckHealthCheckerAuto,
		"HealthChecker auto-transition with 60s cooldown; matching-loop p50 self-probe")

	// ---- Task 2.3.7: IPC Layer (Aeron/Shared-Memory) -------------------------
	r.Register("P02-T2.3.7-C1", ckIPCZeroLossEngine,
		"Aeron/shared-memory IPC zero-loss — 1000-burst + SPSC rings")
	r.Register("P02-T2.3.7-C2", ckIPCRoundTripLatency,
		"IPC round-trip latency — ping-pong RTT probe (sub-10µs layer; <50µs Go↔C++ e2e budget)")

	// ---- Task 2.3.9: Per-Account Collar + Price Band -------------------------
	r.Register("P02-T2.3.9-C1", ckCollarPriceBand,
		"in-process per-account token-bucket collar + price band — supersedes Redis rl:account")

	// ---- Task 2.3.10: TIF Expiry Scheduler (GTD/DAY) -------------------------
	r.Register("P02-T2.3.10-C1", ckTifExpiry,
		"GTD/DAY expiry via TIME_TICK WAL events — deterministic replay (§5.4/§6.1)")

	// ---- Task 2.3.12: Cross-Shard Margin Coordination -------------------------
	r.Register("P02-T2.3.12-C1", ckCrossShardMarginCoord,
		"cross-shard margin reservation interface — MARGIN_RESERVE_*/500µs fallback (§13.1, §24 #176)")

	// ---- Task 2.3.19: Backpressure / Poison-Pill / Watchdog -------------------
	r.Register("P02-T2.3.19-C1", ckBackpressureWatchdog,
		"ring watermarks 80/95, poison-pill quarantine, watchdog thread — fail closed (§24 #299)")

	// ---- Task 2.3.23: Pipette Fixed-Point Scaling ------------------------------
	r.Register("P02-T2.3.23-C1", ckPipetteFixedPoint,
		"10^8 integer ticks, pip_factor pipette math, zero float in hot path (§3.3a, §24 #402)")

	// ---- Task 2.3.24: Bilateral Credit Matrix -----------------------------------
	r.Register("P02-T2.3.24-C1", ckBilateralCreditMatrix,
		"in-memory shm bilateral credit matrix, consume-or-skip in matching loop (§3.3b, §24 #403)")
}

// ============================ implementations ============================

func ckOrderBookStructure(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/include/book/OrderBook.hpp",
			"core/src/book/OrderBook.cpp",
			"core/include/book/Order.hpp",
			"core/include/book/PriceLevel.hpp"),
		// Intrusive linked-list orders: next/prev FIFO chain + hash_next
		// id-index chain — no allocator nodes.
		structural(env, "core/include/book/Order.hpp",
			`Order\* next`, `Order\* prev`, `Order\* hash_next`),
		// PriceLevel: head/tail intrusive queue, 64-byte cache-line aligned.
		structural(env, "core/include/book/PriceLevel.hpp",
			`alignas\(64\)`, `head`, `tail`, `order_count`),
		// Flat-array levels: side_levels() indexes bids_/asks_ arrays.
		structural(env, "core/include/book/OrderBook.hpp",
			`bids_`, `asks_`, `side_levels`),
		gtest("test_order_book",
			"OrderBookTypes.*:OrderBookBasic.*:OrderBookLevels.*"),
	)
}

func ckOrderBookZeroAlloc(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		// Orders come exclusively from the bound MemoryPool<Order>.
		structural(env, "core/include/book/OrderBook.hpp",
			`MemoryPool<Order>`),
		// Allocator-hook proof: zero heap allocs during add/cancel/modify/fill.
		gtest("test_order_book",
			"OrderBookAlloc.*:OrderBookEdges.*"),
		ctest("test_memory_pool"),
	)
}

func ckOrderBookFifo(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gtest("test_order_book",
			"OrderBookFifo.*:OrderBookCancel.*:OrderBookModify.QtyDownPreservesPriority:OrderBookFuzz.*"),
		// Engine-level proof: price-time order observed through matching.
		gtest("test_matching_engine",
			"MatchingEngine.LimitMatchPriceTimePriority:MatchingEngine.MakerPriceWinsOverTakerPrice"),
	)
}

func ckMatchPriceTime(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/matching/MatchingEngine.cpp",
			"core/include/matching/MatchingEngine.hpp",
			"core/src/matching/WalWriter.cpp",
			"core/src/matching/IpcPublisher.cpp",
			"core/src/matching/StopOrderTrigger.cpp"),
		gtest("test_matching_engine",
			"MatchingEngine.LimitRestsWhenNotMarketable:MatchingEngine.MarketWalksLevelsThenStops:MatchingEngine.StopTriggersWhenLastPriceCrosses:MatchingEngine.StopLimitRestsAfterTrigger:MatchingEngine.DeterministicSameInputSameOutput"),
		gtest("test_matching", "MatchingSmoke.*"),
	)
}

func ckSelfTradePrevention(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/matching/SelfTradeGuard.cpp",
			"core/include/matching/SelfTradeGuard.hpp"),
		structural(env, "core/include/matching/SelfTradeGuard.hpp",
			`StpAction`, `CANCEL_TAKER`, `CANCEL_MAKER`, `CANCEL_BOTH`, `DECREMENT`),
		// Same-account and same-trade-group matches are diverted before fill.
		structural(env, "core/src/matching/MatchingEngine.cpp",
			`SelfTradeGuard::action`, `apply_stp`),
		gtest("test_matching_engine", "MatchingEngine.Stp*"),
	)
}

func ckFokIocSemantics(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "core/include/book/Order.hpp",
			`GTC, IOC, FOK, GTD, DAY`),
		structural(env, "core/src/matching/MatchingEngine.cpp",
			`TimeInForce::FOK`, `TimeInForce::IOC`),
		gtest("test_matching_engine",
			"MatchingEngine.Fok*:MatchingEngine.Ioc*"),
	)
}

func ckIcebergSlices(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/matching/IcebergManager.cpp",
			"core/include/matching/IcebergManager.hpp"),
		structural(env, "core/include/matching/IcebergManager.hpp",
			`display_qty_units`, `kDefaultDisplayPct100`, `hidden`),
		structural(env, "core/src/matching/MatchingEngine.cpp",
			`replenish_iceberg`),
		gtest("test_matching_engine",
			"MatchingEngine.Iceberg*:MatchingComponents.IcebergSliceMath"),
	)
}

func ckPreTrade14Checks(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/risk/PreTradeChecker.cpp",
			"core/include/risk/PreTradeChecker.hpp",
			"core/include/risk/RiskInterfaces.hpp"),
		// All 14 ordered check sections present, cheapest-first ordering
		// (the run() body numbers them 1–14).
		structural(env, "core/src/risk/PreTradeChecker.cpp",
			`---- 1\. Account status`,
			`---- 8\. Margin`,
			`---- 12\. Min notional`,
			`---- 13\. Execution flags`,
			`---- 14\. STP`),
		// Specific rejection codes the task requires.
		structural(env, "core/src/risk/PreTradeChecker.cpp",
			`kCodeInsufficientBalance`,
			`kCodePriceOutOfBand`,
			`kCodeMinNotionalViolation`,
			`kCodePostOnlyViolation`,
			`kCodeReduceOnlyViolation`),
		// In-process mandate (§3.3): no Redis/IPC/client IO in the check TU.
		structuralLacks(env, "core/src/risk/PreTradeChecker.cpp",
			`RespClient|IpcChannel|redis\.call|::send\(|::recv\(`),
		// Migration 050 backs check 12 (instruments.min_notional).
		structural(env, "services/internal/db/migrations/050_instruments_min_notional.up.sql",
			`min_notional`, `DECIMAL\(28,8\)`),
		gtest("test_pretrade",
			"PreTrade.HappyPathPasses:PreTrade.AccountStatusRejects:PreTrade.InstrumentStatusRejects:PreTrade.BalanceCheck:PreTrade.PositionLimit:PreTrade.TickLot:PreTrade.MinNotional:PreTrade.PostOnlyMarketable:PreTrade.ReduceOnlyNeedsOppositePosition:PreTrade.StpModeResolution:PreTrade.Check*:PreTrade.MarginStub:PreTrade.CircuitBreakerStub:PreTrade.KycTierStub"),
	)
}

func ckPreTradeLatency(ctx context.Context, env *spec.Env) spec.Result {
	// P99 over the full 14-check pipeline must be < 10µs (spec §3.3).
	return spec.RunGTest(ctx, env, "test_pretrade", "PreTrade.LatencyP99Under10us")
}

func ckWalPerStateChange(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/matching/WalWriter.cpp",
			"core/include/matching/WalWriter.hpp",
			"core/src/wal/WalEntry.cpp",
			"core/include/wal/WalEntry.hpp"),
		structural(env, "core/src/wal/WalEntry.cpp",
			`crc32c|CRC32C`),
		// Engine journals every mutation: WAL-before-mutation ordering.
		structural(env, "core/src/matching/MatchingEngine.cpp",
			`wal_->write_|wal_->append`),
		gtest("test_matching_engine", "MatchingEngine.WalJournalsEveryMutation"),
		gtest("test_wal", "WalCrc32c.*:WalReader.*"),
	)
}

func ckSnapshotWalRecovery(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/recovery/RecoveryManager.cpp",
			"core/include/recovery/RecoveryManager.hpp",
			"core/src/recovery/SnapshotStore.cpp",
			"core/include/recovery/SnapshotStore.hpp"),
		// Cadence: every 100k trades or 5 minutes (spec §3.5).
		structural(env, "core/include/recovery/SnapshotStore.hpp",
			`trade_interval = 100'000`, `interval_ns`),
		// Boot-time invariant enforced fail-closed: book_seq == WAL tail.
		structural(env, "core/include/recovery/RecoveryManager.hpp",
			`snapshot_seq`, `wal_tail`),
		gtest("test_recovery",
			"RecoveryManager.Snapshot*:RecoveryManager.WalOnlyBootReplays:RecoveryManager.EmptyWalBootsClean:FileSnapshotSink.*:SnapshotStoreCadence.*"),
	)
}

func ckRecoveryNoDupMiss(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gtest("test_recovery",
			"RecoveryManager.Duplicate*:RecoveryManager.SequenceGapFailsClosed:RecoveryManager.ForgedBookSeqFailsInvariant:RecoveryManager.MidSegmentCorruptionFailsClosed:RecoveryManager.Corrupt*:RecoveryManager.TradeReplayDecrementsResting"),
	)
}

func ckEpochLeaseElection(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/election/LeaderElection.cpp",
			"core/include/election/LeaderElection.hpp",
			"core/src/election/RedisLeaseStore.cpp",
			"core/include/election/LeaderLeaseStore.hpp",
			"core/src/redis/RespClient.cpp"),
		// Canonical §18.6.2 constants: TTL 2,000ms / refresh 500ms / settle 5s.
		structural(env, "core/include/election/LeaderLeaseStore.hpp",
			`kLeaderLeaseTtlMs = 2'000`, `kLeaderRefreshMs = 500`),
		// Lua compare-act scripts: INCR epoch fencing counter + PX lease TTL;
		// atomic value+PTTL read; token-checked release (compare-and-del).
		structural(env, "core/src/election/RedisLeaseStore.cpp",
			`kAcquireScript`, `kHeartbeatScript`, `INCR`, `PX`, `PTTL`,
			`format_lease_value`),
		gtest("test_election",
			"LeaderElectionUnit.Acquire*:LeaderElectionUnit.HeartbeatRenews*:LeaderElectionUnit.Epoch*:LeaderElectionUnit.Follower*:LeaderElectionUnit.Elect*:LeaderElectionUnit.LeaseValueFormatRoundTrip:LeaderElectionUnit.StartupStabilization*"),
		// Live Redis path (self-skips when EXC_REDIS_TEST_ADDR is absent).
		gtest("test_election_live", "LiveElection.*"),
	)
}

func ckSplitBrainFailClosed(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "core/include/election/LeaderElection.hpp",
			`check_split_brain`, `frozen_`),
		structural(env, "core/src/election/LeaderElection.cpp",
			`check_split_brain`, `split-brain`),
		gtest("test_election",
			"LeaderElectionUnit.CheckSplitBrain*:LeaderElectionUnit.RedisDown*:LeaderElectionUnit.HeartbeatAbsent*:LeaderElectionUnit.HeartbeatTransportError*:LeaderElectionUnit.StoreLess*"),
	)
}

func ckDegradationModes(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/degradation/ModeManager.cpp",
			"core/include/degradation/ModeManager.hpp",
			"core/src/degradation/RedisModeStore.cpp",
			"core/src/degradation/ModeEventPublisher.cpp"),
		// Canonical mode set, exact PascalCase casing (spec §2.4).
		structural(env, "core/include/degradation/ModeManager.hpp",
			`Normal`, `ReadOnly`, `MarketDataOnly`, `SpotOnly`, `Throttled`, `Maintenance`),
		// Coordination keys per the plan: mode/entered_at/reason.
		structural(env, "core/src/degradation/RedisModeStore.cpp",
			`system:degradation:mode`, `system:degradation:entered_at`, `system:degradation:reason`),
		gtest("test_mode_manager",
			"ModeManagerUnit.SixModesDefinedAndSeverityOrdered:ModeManagerUnit.SetMode*:ModeManagerUnit.Refresh*:ModeManagerUnit.Absent*:ModeManagerUnit.AutoPath*:ModeManagerUnit.Cooldown*:ModeManagerUnit.RedisDown*:ModeManagerUnit.NoStore*:ModeEventPublisherTest.*"),
	)
}

func ckHealthCheckerAuto(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/health/HealthChecker.cpp",
			"core/include/health/HealthChecker.hpp"),
		// Matching-loop p50 self-probe (remediation #8 ReadOnly trigger),
		// Redis p50, cooldown gate.
		structural(env, "core/include/health/HealthChecker.hpp",
			`matching_p50_us`, `redis_p50_us`, `cooldown_ms`, `matching_slow_us`),
		structural(env, "core/src/health/HealthChecker.cpp",
			`cooldown`, `matching_loop_ok`),
		gtest("test_health", "HealthCheckerUnit.*"),
	)
}

func ckIPCZeroLossEngine(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/ipc/EnginePump.cpp",
			"core/include/ipc/EnginePump.hpp",
			"core/src/ipc/IpcChannel.cpp",
			"core/src/matching/IpcPublisher.cpp",
			"core/src/ipc/SharedMemChannel.cpp",
			"core/src/ipc/AeronChannel.cpp"),
		// Zero-loss SPSC delivery, in-process and cross-process.
		gtest("test_ipc",
			"ShmRing.ZeroLoss*:SharedMemChannel.*:ShmRing.BackpressureWhenFull"),
		// Engine ingress: decode → dispatch → outbound fill loopback.
		gtest("test_engine_pump",
			"EnginePump.OrderNewDecodesAndDispatches:EnginePump.OrderCancelDecodesAndDispatches:EnginePump.BatchDrainsAndStopsAtEmpty:EnginePump.ShmLoopbackOrderInFillOut"),
	)
}

func ckIPCRoundTripLatency(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		// Same probe as Phase-01 ckIPCLatency: 10k Go↔C++ ping-pong with a
		// latency report asserted under the <50µs end-to-end budget (the
		// sub-10µs figure is the IPC-layer-only measurement — see Task
		// 1.3.10 / remediation #35 note in phase01.go).
		gotest("./internal/ipc", `TestShmRoundTripCpp`, "-timeout", "120s"),
		// Aeron transport: loopback against a live driver, or fail-closed
		// proof when no driver is present (both branches are evidence).
		gtest("test_ipc", "AeronChannel.*"),
	)
}

func ckCollarPriceBand(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		// In-process token-bucket collar (remediation #35 — a Redis
		// round-trip per order would violate the §3.3 no-IPC mandate).
		structural(env, "core/src/risk/PreTradeChecker.cpp",
			`check_collar`, `kCodeRateLimitExceeded`, `kCodePriceOutOfBand`,
			`in-process`),
		// Collar tunable (default 50/sec) + per-instrument band thresholds.
		structural(env, "core/include/risk/RiskInterfaces.hpp",
			`order_rate_per_sec`),
		structural(env, "core/include/book/Instrument.hpp",
			`price_band_pct_up`, `price_band_pct_down`),
		// Collar must not consult Redis/IPC — in-process mandate.
		structuralLacks(env, "core/src/risk/PreTradeChecker.cpp",
			`rl:account|RespClient`),
		gtest("test_pretrade",
			"PreTrade.Collar*:PreTrade.PriceBand"),
	)
}

func ckTifExpiry(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/matching/ExpiryScheduler.cpp",
			"core/include/matching/ExpiryScheduler.hpp"),
		// Min-heap over (expiry_ns, order_id); DAY computes session end.
		structural(env, "core/include/matching/ExpiryScheduler.hpp",
			`min-heap`, `expiry_ns`),
		// Deterministic clock: TIME_TICK is a WAL event type and the pump
		// stamps it on the matching thread (single-writer invariant).
		structural(env, "core/include/wal/WalEntry.hpp", `TIME_TICK`),
		structural(env, "core/src/ipc/EnginePump.cpp", `TIME_TICK`),
		gtest("test_expiry",
			"ExpiryGtd.*:ExpiryDay.*:ExpiryTif.*:ExpiryDeterminism.*:ExpiryDrain.*:ExpiryLifecycle.*:ExpirySnapshot.*"),
		gtest("test_matching_engine",
			"MatchingEngine.GtdExpiresOnTimeTick:MatchingEngine.TimeTickIsMonotone"),
	)
}

func ckCrossShardMarginCoord(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/risk/CrossShardMarginCoordinator.cpp",
			"core/include/risk/CrossShardMarginCoordinator.h"),
		// Unified message names (remediation #35) + WAL-persisted reservation
		// state + pessimistic floor on coordinator timeout.
		structural(env, "core/src/risk/CrossShardMarginCoordinator.cpp",
			`MARGIN_RESERVE`, `MARGIN_RELEASE`, `pessimistic`),
		gtest("test_margin_coordinator",
			"MarginCoordinator.*:MarginCtlCodec.*"),
	)
}

func ckBackpressureWatchdog(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/matching/EngineLoop.cpp",
			"core/include/matching/EngineLoop.hpp",
			"core/src/ipc/EnginePump.cpp",
			"core/src/main.cpp"),
		// §2.7.3 watermarks: shed >80%, halt ingress >95%; cancels always pass.
		structural(env, "core/include/matching/EngineLoop.hpp",
			`shed_watermark_pct = 80`, `halt_watermark_pct = 95`),
		// Dedicated jthread watchdog + CRITICAL_BACKPRESSURE report.
		structural(env, "core/src/matching/EngineLoop.cpp",
			`std::jthread`, `CRITICAL_BACKPRESSURE`, `Watchdog`),
		// Poison-pill quarantine sink + poison_pill.log wiring in main.
		structural(env, "core/src/ipc/EnginePump.cpp",
			`quarantine`, `poison`),
		structural(env, "core/src/main.cpp",
			`poison_pill\.log`, `watchdog_report`),
		gtest("test_engine_pump",
			"EngineLoop.*:EnginePump.Poison*:Watchdog.*"),
	)
}

func ckPipetteFixedPoint(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/include/book/Order.hpp",
			"core/include/book/PriceLevel.hpp",
			"core/include/book/Instrument.hpp",
			"core/src/book/OrderBook.cpp"),
		// int64 10^8 fixed-point fields on the hot-path Order POD.
		structural(env, "core/include/book/Order.hpp",
			`int64_t price_ticks`, `int64_t qty_units`, `int64_t filled_qty_units`),
		// Instrument-aware pipette conversion: pip_factor, ticks_per_pip,
		// ticks_per_pipette, integer-exact spread_pips spec formula.
		structural(env, "core/include/book/Instrument.hpp",
			`pip_factor`, `ticks_per_pip`, `ticks_per_pipette`, `spread_pips`),
		// Zero float/double in the financial hot path (same intent as
		// Phase-01 ckDecimalFixedPoint, with a type-context pattern so
		// comment words like "double-apply" don't false-positive).
		func(context.Context, *spec.Env) spec.Result {
			const pat = `\b(float|double)\s+[A-Za-z_]|\(\s*(float|double)\s*\)|<\s*(float|double)\s*>`
			n, err := grepTree(env, "core/src/book", pat)
			if err != nil {
				return spec.Failf("scan core/src/book: %v", err)
			}
			m, err := grepTree(env, "core/src/matching", pat)
			if err != nil {
				return spec.Failf("scan core/src/matching: %v", err)
			}
			if n+m > 0 {
				return spec.Failf("float/double in matching hot path: book=%d matching=%d", n, m)
			}
			return spec.Pass("no float/double in book+matching")
		},
		// Exact pipette math for 5-decimal majors and 3-decimal JPY pairs,
		// plus integer-tick wire encoding (SBE/FlatBuffers, no float conv).
		gtest("test_book_pipette",
			"PipetteMath.*:PipetteChecks.*:PipetteWire.*:PipetteBook.*"),
		gtest("test_order_book", "OrderBookTypes.OrderIsPodPerSpec31"),
	)
}

func ckBilateralCreditMatrix(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/risk/BilateralCreditMatrix.cpp",
			"core/include/risk/BilateralCreditMatrix.h"),
		// Shared-memory 2D atomic matrix + consume-or-skip gate + the
		// CREDIT_UPDATE control-plane codec (single Go writer, §13.8).
		structural(env, "core/include/risk/BilateralCreditMatrix.h",
			`kCreditMaxParties`, `can_match`, `try_debit`, `consume_or_skip`, `CreditUpdate`),
		structural(env, "core/src/risk/BilateralCreditMatrix.cpp",
			`shm_open`, `MAP_SHARED`, `credit_ctl_decode`),
		// Admission-time pre-screen is bound through IPartyMap (§3.3b).
		structural(env, "core/include/risk/RiskInterfaces.hpp",
			`IPartyMap`, `credit_party_id`, `kCodeBilateralCreditExhausted`),
		gtest("test_credit_matrix", "CreditMatrix.*"),
	)
}
