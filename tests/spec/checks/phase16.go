package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-16 advanced order types checkpoints (Tasks 16.3.1–16.3.25). Evidence
// split: the Go control plane (algo framework, composites, grid, strategies,
// exec-param validation, GSLO premium, fixing settlement, race/oracle guards)
// is covered by unit + PG/Redis-gated tests; the C++ order-type machinery
// (trailing units, pegged, hidden, trigger sources, MOO/MOC, GSLO fill) is
// covered by gtest cases in the core test binaries.
const (
	p16Algo  = "./internal/algo"
	p16Order = "./internal/orders"
	p16Bots  = "./internal/bots"
	p16Strat = "./internal/strategies"
)

func registerPhase16(r *spec.Registry) {
	r.Register("P16-T16.3.1-C1", ckP16TWAP,
		"TWAP order slices over interval (§6.2)")
	r.Register("P16-T16.3.2-C1", ckP16VWAP,
		"VWAP order proportional to volume profile (§6.2)")
	r.Register("P16-T16.3.3-C1", ckP16TrailingStop,
		"trailing stop offset/percentage trail and trigger (§6.2)")
	r.Register("P16-T16.3.4-C1", ckP16PegToBest,
		"peg-to-best (superseded alias — pegged machinery per 16.3.11)")
	r.Register("P16-T16.3.5-C1", ckP16BracketAlias,
		"bracket order (superseded alias — bracket/OTO per 16.3.14)")
	r.Register("P16-T16.3.6-C1", ckP16Spread,
		"multi-leg spread order with price relationship (§6.2)")
	r.Register("P16-T16.3.7-C1", ckP16Scaled,
		"scaled order price-level quantity distribution (§6.2)")
	r.Register("P16-T16.3.8-C1", ckP16Framework,
		"algo order framework with delayed dispatch (§6.2)")
	r.Register("P16-T16.3.9-C1", ckP16Fixing,
		"benchmark fixing orders (WM/Refinitiv 4 PM London, ECB 14:15 CET) (§6.2, §24 #124)")
	r.Register("P16-T16.3.10-C1", ckP16ExecParams,
		"execution params persisted + WAL round-trip (§5.4, §24 #131)")
	r.Register("P16-T16.3.11-C1", ckP16Pegged,
		"pegged order types for FX algorithmic liquidity provision")
	r.Register("P16-T16.3.12-C1", ckP16AntiGaming,
		"algo anti-gaming randomization")
	r.Register("P16-T16.3.13-C1", ckP16Hidden,
		"Dark Pool & Fully Hidden Orders")
	r.Register("P16-T16.3.14-C1", ckP16Bracket,
		"bracket/OTO composite order")
	r.Register("P16-T16.3.15-C1", ckP16TrailingUnits,
		"trailing stop distance units (PIPS/PERCENTAGE/ABSOLUTE)")
	r.Register("P16-T16.3.16-C1", ckP16GSLO,
		"guaranteed stop loss orders")
	r.Register("P16-T16.3.17-C1", ckP16TriggerSource,
		"dual-price conditional order trigger evaluation")
	r.Register("P16-T16.3.18-C1", ckP16VP,
		"VP obeys target participation, limits, parent lifecycle, and anti-gaming (§24 #275)")
	r.Register("P16-T16.3.19-C1", ckP16Grid,
		"VP and grid algorithms enforce parent risk, lifecycle, and P&L accounting (§24 #275)")
	r.Register("P16-T16.3.20-C1", ckP16OrderLists,
		"OPO/OPOCO sizes pending orders from locked net proceeds and recovers atomically (§24 #287)")
	r.Register("P16-T16.3.21-C1", ckP16Strategies,
		"recurring/rebalancing strategies remain firm-CLOB, suitability-gated, pausable, and cost-transparent (§24 #296)")
	r.Register("P16-T16.3.22-C1", ckP16RaceGuards,
		"Algo parent-child cancellation races and stale oracle guards fail closed (§24 #317)")
	r.Register("P16-T16.3.23-C1", ckP16AlgoOrders,
		"unified algo status query and cancel-all with no orphan children (§24 #365)")
	r.Register("P16-T16.3.24-C1", ckP16ListQueries,
		"composite-list open/history/detail queries over the list engine (§24 #367)")
	r.Register("P16-T16.3.25-C1", ckP16Moomoc,
		"MOO/MOC session-bound market orders execute at auction uncross (§24 #399)")
}

// --- Task 16.3.1: TWAP --------------------------------------------------------

func ckP16TWAP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/algo/twap.go",
			"services/internal/algo/framework.go"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"orders/twap"),
		gotest(p16Algo, "TestTWAPValidation|TestTWAPPlanEqualSlicesAndConservation|"+
			"TestTWAPEndToEndFills"),
	)
}

// --- Task 16.3.2: VWAP --------------------------------------------------------

func ckP16VWAP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/algo/vwap.go",
			"services/internal/algo/volume.go"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"orders/vwap"),
		gotest(p16Algo, "TestVWAPProfileWeighting"),
	)
}

// --- Task 16.3.3: trailing stop (C++ engine) -----------------------------------

func ckP16TrailingStop(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/include/matching/StopOrderTrigger.hpp",
			"core/tests/test_phase16.cpp"),
		structural(env, "core/include/matching/StopOrderTrigger.hpp",
			"trail_unit", "anchor_ticks", "activation_price_ticks"),
		gtest("test_phase16",
			"Phase16Trailing.AbsoluteDistanceArmsAndDerivesStop:"+
				"Phase16Trailing.AnchorRatchetsFavorableOnly:"+
				"Phase16Trailing.ActivationGateHoldsDormant"),
	)
}

// --- Task 16.3.4: peg-to-best (superseded by 16.3.11) --------------------------

func ckP16PegToBest(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "docs/Phase-16-Advanced-Order-Types.md",
			"superseded by Task 16.3.11"),
		structural(env, "core/include/book/Order.hpp",
			"kPegMid", "kPegPrimary", "kPegMarket"),
	)
}

// --- Task 16.3.5: bracket (superseded by 16.3.14) ------------------------------

func ckP16BracketAlias(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "docs/Phase-16-Advanced-Order-Types.md",
			"superseded by Task 16.3.14"),
		files(env,
			"services/internal/orders/bracket.go",
			"services/internal/db/migrations/225_bracket_orders.up.sql"),
	)
}

// --- Task 16.3.6: spread orders ------------------------------------------------

func ckP16Spread(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/algo/spread.go"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"orders/spread"),
		structural(env, "services/internal/errs/codes.go",
			"SPREAD_ORDER_REJECTED"),
		gotest(p16Algo, "TestSpreadMarketMismatchRejects|TestSpreadBothLegsFill|"+
			"TestSpreadLegAFailureStopsLegB|TestSpreadRollbackOnPartial"),
	)
}

// --- Task 16.3.7: scaled orders -------------------------------------------------

func ckP16Scaled(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/algo/scaled.go"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"orders/scaled"),
		gotest(p16Algo, "TestScaledWeightsAndLevels|TestScaledValidation"),
	)
}

// --- Task 16.3.8: algo framework -------------------------------------------------

func ckP16Framework(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/algo/framework.go",
			"services/internal/algo/store.go",
			"services/internal/db/migrations/224_algo_orders.up.sql"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"orders/algo"),
		gotest(p16Algo, "TestPauseResumeCancel|TestDelayedDispatchStaysPending|"+
			"TestIdempotentParentReplay|TestITParentLifecyclePersisted|"+
			"TestITChildRowsAndIdempotentCID|TestITReadSeams"),
	)
}

// --- Task 16.3.9: benchmark fixing orders ----------------------------------------

func ckP16Fixing(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/algo/fixing.go",
			"services/internal/algo/fixing_exec.go"),
		structural(env, "services/internal/errs/codes.go",
			"FIXING_CUTOFF_EXCEEDED", "FIXING_CANCELLATION_RESTRICTED"),
		gotest(p16Algo, "TestSchedulerOrderBenchmarkRoundTrip|"+
			"TestReservationOfParsesAlgoParams|"+
			"TestFixingVocabRejectsSupersededStrings"),
	)
}

// --- Task 16.3.10: execution params persistence ----------------------------------

func ckP16ExecParams(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/038_orders_execution_params.up.sql"),
		structural(env, "services/internal/db/migrations/038_orders_execution_params.up.sql",
			"peg_offset", "peg_mode", "algo_type", "algo_params", "fixing_benchmark"),
		structural(env, "services/internal/db/migrations/038_orders_execution_params.up.sql",
			"peg_limit", "hidden", "gslo"),
		structuralLacks(env, "services/internal/db/migrations/038_orders_execution_params.up.sql",
			"COLUMN oco_group_id"),
		gotest(p16Algo, "TestReservationOfParsesAlgoParams"),
		gotest(p16Order, "TestSubmitHappyPathAndDedup"),
		gtest("test_phase16",
			"Phase16Wal.OrderNewExAndAdvancedRowsRoundTrip:"+
				"Phase16Pump.PegFieldsDecodeToAux:"+
				"Phase16Pump.StopMarketPlusTrailUnitDecodesTrailing:"+
				"Phase16Pump.HiddenAndGsloWireFlagsTranslate:"+
				"Phase16Pump.TrailUnitOnNonStopIsDecodeError:"+
				"Phase16Pump.OutOfRangeAdvancedEnumsAreDecodeErrors"),
	)
}

// --- Task 16.3.11: pegged orders (C++ engine) -------------------------------------

func ckP16Pegged(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "core/src/matching/MatchingEngine.cpp",
			"peg_erase", "pegs_"),
		structural(env, "core/include/wal/WalEntry.hpp",
			"PEG_REPRICE"),
		gtest("test_phase16",
			"Phase16Peg.MidPegRestsAtVisibleMidpoint:"+
				"Phase16Peg.PrimaryPegOffsetsSameSide:"+
				"Phase16Peg.MarketPegOffsetsOppositeSide:"+
				"Phase16Peg.RepriceFollowsVisibleBBO:"+
				"Phase16Peg.CollarClampsReprice:"+
				"Phase16Peg.NoReferenceRejectsUnavailable:"+
				"Phase16Peg.NoReferenceWithCollarRestsAtLimit:"+
				"Phase16Peg.L2SnapshotExcludesPeggedLevel"),
	)
}

// --- Task 16.3.12: anti-gaming randomization --------------------------------------

func ckP16AntiGaming(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/algo/antigaming.go"),
		gotest(p16Algo, "TestPerturbSizesNeverNegative|"+
			"TestAntiGamingNonDeterministic"),
	)
}

// --- Task 16.3.13: dark/hidden orders (C++ engine) ---------------------------------

func ckP16Hidden(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "core/src/marketdata/BookSerializer.cpp",
			"l2_visible"),
		structural(env, "services/internal/db/migrations/038_orders_execution_params.up.sql",
			"hidden"),
		gtest("test_phase16",
			"Phase16Hidden.L2OmissionButRests:Phase16Hidden.MidpointFill:"+
				"Phase16Hidden.NoVisibleMidFailsClosed"),
	)
}

// --- Task 16.3.14: bracket/OTO composites ------------------------------------------

func ckP16Bracket(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/orders/bracket.go",
			"services/internal/orders/store_composite.go",
			"services/internal/db/migrations/225_bracket_orders.up.sql"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"orders/bracket"),
		gotest(p16Order, "TestITBracketFillCascade"),
	)
}

// --- Task 16.3.15: trailing-stop distance units ------------------------------------

func ckP16TrailingUnits(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "core/include/book/Order.hpp",
			"kTrailUnitPips", "kTrailUnitPercentage", "kTrailUnitAbsolute"),
		gtest("test_phase16",
			"Phase16Trailing.AbsoluteDistanceArmsAndDerivesStop:"+
				"Phase16Trailing.PipsDistanceConvertsThroughLattice:"+
				"Phase16Trailing.PercentageDistance"),
	)
}

// --- Task 16.3.16: GSLO ------------------------------------------------------------

func ckP16GSLO(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/algo/gslo.go"),
		structural(env, "services/internal/errs/codes.go",
			"GSLO_EXPOSURE_EXCEEDED"),
		structural(env, "services/internal/algo/gslo.go",
			"INSURANCE", "premium", "RefundPremium"),
		gotest(p16Algo, "TestGSLOPremiumFormula|TestGSLOGapLiability"),
		gtest("test_phase16",
			"Phase16Gslo.ExposureReservedThenReleasedOnCancel:"+
				"Phase16Gslo.ExactStopFillAtVenueId:"+
				"Phase16Gslo.ExposureCapRejectsAdmission:"+
				"Phase16Gslo.FlagOnNonConditionalRejects"),
	)
}

// --- Task 16.3.17: trigger_source ---------------------------------------------------

func ckP16TriggerSource(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/066_orders_trigger_source.up.sql"),
		structural(env, "services/internal/db/migrations/066_orders_trigger_source.up.sql",
			"LAST_PRICE", "MARK_PRICE", "INDEX_PRICE"),
		structural(env, "services/internal/algo/raceguard.go",
			"CONDITIONAL_TRIGGER_ORACLE_STALE", "PEGGED_PRICING_UNAVAILABLE"),
		gotest(p16Algo, "TestTriggerGuardStaleMarkFailsClosed"),
		gtest("test_phase16",
			"Phase16Triggers.MarkSourceFiresWithoutLastPrint:"+
				"Phase16Triggers.IndexSourceFiresOnIndex:"+
				"Phase16Triggers.StaleMarkFreezesSourceNotQueue:"+
				"Phase16Wal.ArmedMarkTrailRejectsLiveButReplays"),
	)
}

// --- Task 16.3.18: volume participation ----------------------------------------------

func ckP16VP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/algo/vp.go"),
		gotest(p16Algo, "TestVPValidation|TestVPNoVolumeSourceFailsClosed"),
	)
}

// --- Task 16.3.19: grid bots -----------------------------------------------------------

func ckP16Grid(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/bots/engine.go",
			"services/internal/db/migrations/071_grid_bots.up.sql"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"bots/grid"),
		gotest(p16Bots, "TestITGridLifecycleFillCounterPnL|"+
			"TestITGridBotConcurrentCap|TestGridLevelsArithmetic|"+
			"TestGridLevelsGeometric|TestGridLevelsUnalignedBounds|"+
			"TestGridLevelsCountBounds"),
	)
}

// --- Task 16.3.20: OPO/OPOCO order lists -------------------------------------------------

func ckP16OrderLists(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/orders/orderlist.go",
			"services/internal/db/migrations/075_opo_order_lists.up.sql"),
		structural(env, "services/internal/db/migrations/075_opo_order_lists.up.sql",
			"order_lists", "order_list_legs", "locked_proceeds",
			"net_pending_qty", "contingency_type"),
		gotest(p16Order, "TestITOrderListActivation|TestNetPendingQty"),
	)
}

// --- Task 16.3.21: recurring conversion / rebalancing / marketplace ----------------------

func ckP16Strategies(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/strategies/service.go",
			"services/internal/strategies/runner.go",
			"services/internal/db/migrations/077_recurring_rebalancing_strategies.up.sql"),
		gotest(p16Strat, "TestITRecurringMarketClosedSkip|"+
			"TestITRebalanceDriftTrigger|"+
			"TestITTemplateApprovalCopySemantics"),
	)
}

// --- Task 16.3.22: cancellation races + stale-oracle guards -------------------------------

func ckP16RaceGuards(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/algo/raceguard.go"),
		structural(env, "services/internal/algo/raceguard.go",
			"FILL_WON", "CANCEL_WON", "RACE_VIOLATION",
			"ReconcileParent", "TriggerGuard"),
		structural(env, "services/internal/errs/codes.go",
			"OCO_SIBLING_CANCEL_RACE", "CONDITIONAL_TRIGGER_ORACLE_STALE",
			"PEGGED_PRICING_UNAVAILABLE"),
		gtest("test_phase16",
			"Phase16Peg.NoReferenceRejectsUnavailable:"+
				"Phase16Triggers.StaleMarkFreezesSourceNotQueue"),
	)
}

// --- Task 16.3.23: algo open-orders query + cancel-all ------------------------------------

func ckP16AlgoOrders(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/gateway/routes_v1.go",
			"algo-orders"),
		gotest(p16Algo, "TestListAndCancelAll"),
	)
}

// --- Task 16.3.24: composite-list query endpoints ------------------------------------------

func ckP16ListQueries(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/gateway/routes_v1.go",
			"order-lists"),
		gotest(p16Order, "TestITOrderListActivation"),
	)
}

// --- Task 16.3.25: MOO/MOC ------------------------------------------------------------------

func ckP16Moomoc(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/orders/auction.go"),
		structural(env, "services/internal/errs/codes.go",
			"AMEND_IN_AUCTION_REJECTED"),
		gotest(p16Order, "TestAuctionSubmitQueuesLocally|"+
			"TestAuctionSubmitValidation|"+
			"TestAuctionSubmitFailsClosedWithoutGate|"+
			"TestAuctionCancelPreFreeze|TestAuctionFreezeRejectsMutation|"+
			"TestAuctionAmendPreFreeze|TestAuctionMassCancelScope|"+
			"TestInjectorQueueFilteredDispatch|TestInjectorReopeningCallMOOOnly|"+
			"TestInjectorIdleWithoutArmedCall|TestAuctionLifecycleNotifications"),
		gtest("test_phase16",
			"Phase16Moo.ParksDuringCallThenUncrosses:"+
				"Phase16Moo.FreezeWindowRejectsCancel:"+
				"Phase16Moo.OutsideCallRejectsOrderInvalid"),
	)
}
