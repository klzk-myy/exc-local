package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-22 FX Derivatives Foundation checkpoints (Tasks 22.3.1–22.3.15;
// tasks .5 and .6 carry two checkpoints each — 17 total). Evidence:
// services/internal/derivatives/* (curve/DCC forwards, swaps, NDFs,
// contract store, roll engine, option lifecycle, initial margin, UMR
// gate, implied feature gate), services/internal/options/* (GK vanilla,
// barrier monitor, binary, Monte Carlo, American lattice, IV surface,
// vol-arb rejection, determinism), services/internal/risk
// (variation_margin.go, exercise shortfall liquidator),
// services/internal/margin/spread_offsets.go,
// services/internal/compliance/legal_docs.go, migration-039 order
// params in internal/orders/{types,service}.go +
// internal/fix/fix50sp2.go, C++ core matching (ImpliedMatcher +
// MatchingEngine implied wiring + curve-shard host in main.cpp),
// migrations 034/039/043/085/252-255. Gated legs self-skip
// (EXC_PG_TEST / EXC_REDIS_TEST / EXC_CH_TEST).
const (
	p22Deriv = "./internal/derivatives"
	p22Opts  = "./internal/options"
	p22Risk  = "./internal/risk"
	p22Marg  = "./internal/margin"
	p22Comp  = "./internal/compliance"
	p22Ord   = "./internal/orders"
	p22Fix   = "./internal/fix"
	p22Mig   = "services/internal/db/migrations"
)

func registerPhase22(r *spec.Registry) {
	r.Register("P22-T22.3.1-C1", ckP22Forwards,
		"FX forwards with interest rate parity — defined first, validated against spec")
	r.Register("P22-T22.3.2-C1", ckP22Swaps,
		"FX swaps (near + far leg) — defined first, validated against spec")
	r.Register("P22-T22.3.3-C1", ckP22NDFs,
		"NDFs with cash settlement — defined first, validated against spec")
	r.Register("P22-T22.3.4-C1", ckP22Vanilla,
		"vanilla FX options with Greeks — defined first, validated against spec")
	r.Register("P22-T22.3.5-C1", ckP22BarrierMonitor,
		"barrier options with knock-in/knock-out monitoring — defined first, validated against spec")
	r.Register("P22-T22.3.5-C2", ckP22BarrierMC,
		"Monte Carlo pricing for barrier options — defined first, validated against spec")
	r.Register("P22-T22.3.6-C1", ckP22Binary,
		"binary options with fixed payout — defined first, validated against spec")
	r.Register("P22-T22.3.6-C2", ckP22BinaryMC,
		"Monte Carlo pricing for binary options — defined first, validated against spec")
	r.Register("P22-T22.3.7-C1", ckP22VariationMargin,
		"variation margin for derivatives — defined first, validated against spec")
	r.Register("P22-T22.3.8-C1", ckP22Roll,
		"position roll — defined first, validated against spec")
	r.Register("P22-T22.3.9-C1", ckP22DerivParams,
		"derivative order params persisted (§5.4, §24 #131) — defined first, validated against spec")
	r.Register("P22-T22.3.10-C1", ckP22OptionLifecycle,
		"option lifecycle — premium, cutoff, auto-exercise (§15.4, §24 #158) — defined first, validated against spec")
	r.Register("P22-T22.3.11-C1", ckP22IMUMR,
		"IM/UMR + legal agreements (§15.5, §24 #146) — defined first, validated against spec")
	r.Register("P22-T22.3.12-C1", ckP22Implied,
		"Multi-Leg Implied Matching Engine — defined first, validated against spec")
	r.Register("P22-T22.3.13-C1", ckP22SpreadOffsets,
		"option spread margin offsets — defined first, validated against spec")
	r.Register("P22-T22.3.14-C1", ckP22VolArbExerciseFail,
		"Option pricing solvers fail gracefully on numerical non-convergence and exercise shortfalls trigger liquidation (§24 #324) — defined first, validated against spec")
	r.Register("P22-T22.3.15-C1", ckP22AmericanSurface,
		"lattice American pricing, built IV surface with fallback hierarchy, deterministic assignment/barrier/expiry-roll linkage (§24 #346) — defined first, validated against spec")
}

// ---------------------------------------------------------------------------

func ckP22Forwards(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/derivatives/forwards.go",
			"services/internal/derivatives/curve.go",
			"services/internal/derivatives/settlement_dates.go",
			p22Mig+"/254_derivative_contracts.up.sql"),
		structural(env, "services/internal/derivatives/forwards.go",
			"BookForward", "deliveryLegs"),
		structural(env, "services/internal/derivatives/curve.go",
			"ForwardRate", "DayCount"),
		structural(env, "services/internal/derivatives/settlement_dates.go",
			"SpotDate", "CheckValueDate"),
		gotest(p22Deriv,
			"TestForwardRateParityIdentity|TestForwardRateDCCBoundaryGBPUSD|"+
				"TestForwardRateUSDJPYBoth360|TestForwardRateRoundTripConsistency|"+
				"TestForwardRateRejectsNonPositiveInputs|TestForwardCurvePillars|"+
				"TestForwardValidation|TestForwardBookAgreedRate|"+
				"TestForwardBookDeliveryLegs|TestForwardRollupSettlement|"+
				"TestPricerForwardEURUSD|TestPricerMissingCurveFailsClosed|"+
				"TestPricerStaleCurveFailsClosed|TestPricerIncompleteCurveFailsClosed|"+
				"TestPricerSpotGate|TestDayCountBoundary|"+
				"TestSpotDateT1Weekday|TestSpotDateHolidaySkip|"+
				"TestSpotDateSameDayUSDCAD|TestDatesFailClosedNoCalendar|"+
				"TestCheckValueDateHolidayRejected|TestPgxContractStoreRoundTrip|"+
				"TestDueSettlements"),
	)
}

func ckP22Swaps(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/derivatives/swaps.go"),
		structural(env, "services/internal/derivatives/swaps.go",
			"BookSwap", "PriceSwap", "assertZeroSpotExposure"),
		gotest(p22Deriv,
			"TestSwapBookingLegs|TestSwapPricingPoints|TestSwapValidationFailures|"+
				"TestSwapPartialRollup|TestCheckSwapDates|TestAssertZeroSpotExposure"),
	)
}

func ckP22NDFs(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/derivatives/ndfs.go"),
		structural(env, "services/internal/derivatives/ndfs.go",
			"NdfService", "ApplyFixing", "ValidateNdfSource"),
		gotest(p22Deriv,
			"TestNdfCashSettlementBuySide|TestNdfCashSettlementSellSidePays|"+
				"TestNdfSettlementAmountMath|TestNdfValidation|"+
				"TestNdfBenchmarkUnavailable|TestNdfFixingAfterSettleRejected|"+
				"TestNdfBookingNoPhysicalLegs|TestPgxFixingOutcomeAtomic"),
	)
}

func ckP22Vanilla(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/options/vanilla.go",
			"services/internal/options/greeks.go",
			"services/internal/options/types.go"),
		structural(env, "services/internal/options/vanilla.go",
			"VanillaOption", "PriceAndGreeks", "PutCallParityForward"),
		structural(env, "services/internal/options/greeks.go",
			"evalGK", "gkCore"),
		gotest(p22Opts,
			"TestVanillaKnownValue|TestVanillaATMBounds|TestVanillaMonotonicity|"+
				"TestVanillaExpired|TestVanillaRejectsAmerican|TestPutCallParity|"+
				"TestGreeksSanity|TestMCVanillaConverges|TestInvalidInputs"),
	)
}

func ckP22BarrierMonitor(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/options/barrier.go",
			p22Mig+"/252_option_barrier_events.up.sql"),
		structural(env, "services/internal/options/barrier.go",
			"BarrierMonitor", "EvaluateBarrierTick", "IsKnockIn"),
		gotest(p22Opts,
			"TestMonitorKnockDownIn|TestMonitorKnockUpOut|"+
				"TestBarrierInOutParity|TestBarrierKOBoundedByVanilla|"+
				"TestBarrierTypeParse|TestMonitorBadTicks|TestMonitorGapFlag|"+
				"TestMonitorOutOfOrder|TestMonitorStaleNeverFabricates|"+
				"TestBarrierAlreadyKnocked"),
	)
}

func ckP22BarrierMC(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/options/montecarlo.go"),
		structural(env, "services/internal/options/montecarlo.go",
			"MCPricer", "PriceBarrier"),
		gotest(p22Opts,
			"TestMCDeterministic|TestMCConfigValidation|TestBarrierFarAway|"+
				"TestLatticeConvergence|TestLatticeNonConvergence|"+
				"TestLatticeProbabilityBreach|TestNonConvergenceCode"),
	)
}

func ckP22Binary(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/options/binary.go"),
		structural(env, "services/internal/options/binary.go",
			"BinaryOption", "PriceClosedForm"),
		gotest(p22Opts,
			"TestBinaryClosedFormBounds|TestBinaryExpiryPayoff|TestBinaryInvalid"),
	)
}

func ckP22BinaryMC(ctx context.Context, env *spec.Env) spec.Result {
	return gotest(p22Opts, "TestBinaryMCConverges|TestMCVanillaConverges")(ctx, env)
}

func ckP22VariationMargin(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/variation_margin.go",
			p22Mig+"/034_create_variation_margin.up.sql"),
		structural(env, "services/internal/risk/variation_margin.go",
			"VariationMarginService", "Sweep", "vmJournal"),
		gotest(p22Risk,
			"TestVMSweep_PositiveVMPostsGL|TestVMSweep_NegativeVMCollects|"+
				"TestVMSweep_ZeroVMNoJournal|TestVMSweep_RerunSameDayNoDoublePost|"+
				"TestVMSweep_SecondDayUsesPrevWatermark|"+
				"TestVMSweep_AllSubjectKindsAndTypes|TestVMSweep_ShortfallFlagged|"+
				"TestVMSweep_ResumesUnsettledClaim|TestVMSweep_NilDepsFailClosed|"+
				"TestVMSweep_StoreErrorPropagates|"+
				"TestPgVariationMargin_SweepAndIdempotency|"+
				"TestPgVariationMargin_DownMigration"),
	)
}

func ckP22Roll(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/derivatives/roll.go",
			p22Mig+"/255_derivatives_roll_lifecycle.up.sql"),
		structural(env, "services/internal/derivatives/roll.go",
			"RollRequest", "RollResult", "PgxRollStore", "AutoRollDue"),
		gotest(p22Deriv,
			"TestRollForwardHappyPath|TestRollSwapReLegs|TestRollNdfFixing|"+
				"TestRollRejectsTerminalAndNDF|TestRollSpreadTolerance|"+
				"TestRollTargetBeforeSourceRejected|TestRollValidation|"+
				"TestRollStaleMarkFailsClosed|TestRollAtomicRollback|"+
				"TestRollIdempotentReplay|TestRollHandler|"+
				"TestAutoRollConfigAndSweep|TestPgxRollAtomicAndIdempotent"),
	)
}

func ckP22DerivParams(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, p22Mig+"/039_orders_derivative_params.up.sql"),
		structural(env, "services/internal/orders/types.go",
			"DerivativeParams", "validateDerivativeParams", "derivParamsFromRequest"),
		structural(env, "services/internal/orders/service.go",
			"checkDerivativeParams", "LoadDerivativeParams"),
		gotest(p22Fix,
			"TestForwardMapping|TestNDFRequiresFixingDate|TestSwapLegDates|"+
				"TestOptionContract|TestSettlementDateStandardTag"),
	)
}

func ckP22OptionLifecycle(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/derivatives/lifecycle.go"),
		structural(env, "services/internal/derivatives/lifecycle.go",
			"ManualExercise", "checkExerciseWindow", "option_assignments",
			"ExerciseAuctionGuard", "checkExerciseAuction"),
		gotest(p22Deriv,
			"TestExerciseCutoff|TestCheckExerciseWindow|TestITMBpsBoundaries|"+
				"TestAssignProRata|TestAssignmentSeedDeterministic|"+
				"TestNextExpiryTick|TestPgxManualExerciseCash|"+
				"TestPgxManualExerciseAfterCutoffRejected|TestPgxExpirySweep|"+
				"TestPgxExpirySweepStaleMarkFailsClosed|"+
				"TestPgxPremiumSettlesThroughGL|"+
				"TestPgxPremiumInsufficientQueuesMarginCall|"+
				"TestPgxManualExerciseAuctionGate"),
	)
}

func ckP22IMUMR(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/derivatives/initial_margin.go",
			"services/internal/compliance/legal_docs.go",
			p22Mig+"/043_legal_agreements.up.sql",
			p22Mig+"/253_umr_im_assessments.up.sql"),
		structural(env, "services/internal/derivatives/initial_margin.go",
			"UMRParams", "IMAggregate", "UMRAssessment"),
		structural(env, "services/internal/compliance/legal_docs.go",
			"LegalDocService", "LegalAgreement"),
		gotest(p22Deriv,
			"TestIMDeltaMargin_SinglePair|TestIMDeltaMargin_NettingSamePair|"+
				"TestIMDeltaMargin_OffsettingPairs|"+
				"TestIMDeltaMargin_TwoPairsCorrelation|"+
				"TestIMSpreadRelief_AppliesAndCaps|"+
				"TestIMSpreadRelief_DoubleConsumeFailsClosed|"+
				"TestIMVegaAndCurvature|TestIMVolGroupFor|"+
				"TestUMRAdmissionGate|TestUMRAdmissionGate_NilDepsFailClosed|"+
				"TestPgUMRIMAssess_PersistAndIdempotency|TestPgUMRIM_DownMigration"),
		gotest(p22Comp,
			"TestLegalDocs_RegisterExecuteLifecycle|"+
				"TestLegalDocs_UnknownTypeAndRoleGuards|TestLegalDocs_ExpirySweep|"+
				"TestLegalGate_NDFRequiresISDAAndCSA|"+
				"TestLegalGate_PendingAndExpiredReject|"+
				"TestLegalGate_StoreErrorFailsClosed|"+
				"TestPgLegalDocs_FullLifecycleAndGate|TestPgLegalDocs_DownMigration"),
	)
}

func ckP22Implied(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/include/matching/ImpliedMatcher.hpp",
			"core/src/matching/ImpliedMatcher.cpp",
			"services/internal/derivatives/implied_gate.go",
			"core/tests/test_implied.cpp",
			"core/tests/test_implied_engine.cpp"),
		structural(env, "core/include/matching/MatchingEngine.hpp",
			"bind_implied", "implied_sync", "on_implied_fill"),
		structural(env, "core/src/main.cpp",
			"curve_ingress", "implied_link_specs", "CurveSlot"),
		structural(env, "services/internal/derivatives/implied_gate.go",
			"ImpliedGate", "Admit", "ImpliedMatchingFlag"),
		gotest(p22Deriv, "TestImpliedGateAdmitMatrix"),
		gtest("test_implied", "*"),
		gtest("test_implied_engine", "*"),
	)
}

func ckP22SpreadOffsets(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/margin/spread_offsets.go",
			p22Mig+"/085_option_spread_offsets.up.sql"),
		structural(env, "services/internal/margin/spread_offsets.go",
			"DetectSpreads", "matchVerticals", "matchStraddles",
			"matchCalendars", "SpreadOffsetService"),
		gotest(p22Marg,
			"TestDetectVerticalSpread_MaxLossBound|"+
				"TestDetectVerticalSpread_PutAndPartialQty|"+
				"TestDetectStraddle_LongBothSides_ZeroBound|"+
				"TestDetectStraddle_ShortBothSides|"+
				"TestDetectStrangle_DifferentStrikes|"+
				"TestDetectCalendar_ShortNearLongFar|"+
				"TestDetectCalendar_ReverseNotRecognized|"+
				"TestDetect_DeterministicOrdering|TestDetect_MultiAccountIsolation|"+
				"TestDetect_NonQualifyingLegs|TestDetect_SingleLegCannotPairTwice|"+
				"TestSpreadParams_CustomOffset|"+
				"TestPgSpreadOffsets_PortfolioModeGateAndPersist|"+
				"TestPgSpreadOffsets_NonPortfolioGetsNoOffsets|"+
				"TestPgSpreadOffsets_MakerCheckerParams|"+
				"TestPgSpreadOffsets_DownMigration"),
	)
}

func ckP22VolArbExerciseFail(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/options/volarb.go"),
		structural(env, "services/internal/options/volarb.go",
			"VOLATILITY_SURFACE_ARBITRAGE"),
		structural(env, "services/internal/risk/variation_margin.go",
			"ExerciseShortfallLiquidator", "LiquidateForExerciseShortfall"),
		gotest(p22Opts,
			"TestCalendarArbRejection|TestButterflyArbRejection|"+
				"TestViolationDeterminism|TestDeclaredCodesRegistered"),
		gotest(p22Risk, "TestExerciseShortfallLiquidator.*"),
	)
}

func ckP22AmericanSurface(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/options/american.go",
			"services/internal/options/ivsurface.go",
			"services/internal/options/determinism.go"),
		structural(env, "services/internal/options/american.go",
			"PriceAmerican", "LatticeConfig", "ExerciseBoundaryPoint"),
		structural(env, "services/internal/options/ivsurface.go",
			"BuildIVSurface", "IVSurface"),
		gotest(p22Opts,
			"TestAmericanDeterminism|TestAmericanGeEuropean|"+
				"TestAmericanStrictPremium|TestAmericanInvalidInputs|"+
				"TestEuropeanLatticeVsGK|TestImpliedVolRoundTrip|"+
				"TestImpliedVolFailures|TestBuildFullSurface|TestSurfaceClean|"+
				"TestSurfaceDeterminism|TestSurfaceInvalidInputs|"+
				"TestFallbackHierarchy|TestResolveCurveSnapshot|"+
				"TestMarketFromCurves|TestMarketFromCurvesFailClosed|"+
				"TestMarkStale|TestDecideRoll|TestDecideExercise"),
	)
}
