package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-19 Multi-Asset & Portfolio Margin checkpoints (Tasks
// 19.3.1–19.3.28). Evidence split: margin/liquidation/insurance-fund/
// collateral/ADL/coordinator cores under services/internal/risk;
// settlement-mode + position transfers under internal/settlement +
// internal/accounting; API surface under internal/api; the cross-shard
// coordinator bridge under services/cmd/risk; gateway wiring under
// services/cmd/gateway/main.go. PG/Redis-gated legs run under
// EXC_PG_TEST=1 / EXC_REDIS_TEST=1.
const (
	p19Risk       = "./internal/risk"
	p19Settlement = "./internal/settlement"
	p19Accounting = "./internal/accounting"
	p19API        = "./internal/api"
)

func registerPhase19(r *spec.Registry) {
	r.Register("P19-T19.3.1-C1", ckP19MarginModes,
		"CROSS/ISOLATED/PORTFOLIO margin modes — defined first, validated against spec")
	r.Register("P19-T19.3.2-C1", ckP19FXLeverage,
		"FX leverage ESMA/CFTC/professional — defined first, validated against spec")
	r.Register("P19-T19.3.3-C1", ckP19MarginCall,
		"margin call notification at 0.90 utilization with 15min deposit window — defined first, validated against spec")
	r.Register("P19-T19.3.3-C2", ckP19AuctionLadder,
		"liquidation auction CALL 5s / EXTEND ≤ 60s — defined first, validated against spec")
	r.Register("P19-T19.3.3-C3", ckP19AuctionFloors,
		"floors ×0.98/×1.02, FORCE_CASH ×0.95/×1.05 — defined first, validated against spec")
	r.Register("P19-T19.3.3-C4", ckP19LPRebate,
		"LP rebate 0.05% from insurance fund — defined first, validated against spec")
	r.Register("P19-T19.3.4-C1", ckP19InsuranceFund,
		"insurance fund — defined first, validated against spec")
	r.Register("P19-T19.3.5-C1", ckP19ExposureLimits,
		"exposure limits — defined first, validated against spec")
	r.Register("P19-T19.3.6-C1", ckP19GrossNet,
		"GROSS-NET settlement configurable per instrument — defined first, validated against spec")
	r.Register("P19-T19.3.7-C1", ckP19PBCredit,
		"PB credit limit enforcement (NOP/DSL) — defined first, validated against spec")
	r.Register("P19-T19.3.8-C1", ckP19Collateral,
		"collateral haircuts + concentration (§13.6b, §24 #145) — defined first, validated against spec")
	r.Register("P19-T19.3.9-C1", ckP19NBP,
		"retail NBP with insurance-fund absorption (§13.6c, §24 #133) — defined first, validated against spec")
	r.Register("P19-T19.3.10-C1", ckP19BilateralCredit,
		"mutual bilateral credit + atomic reservations (§13.8, §24 #165) — defined first, validated against spec")
	r.Register("P19-T19.3.11-C1", ckP19MarginCoordinator,
		"Cross-shard portfolio margin coherence (§13.1, §24 #176) — defined first, validated against spec")
	r.Register("P19-T19.3.12-C1", ckP19PositionTransfers,
		"Internal position transfers and sub-account allocation (§13.9, §24 #190) — defined first, validated against spec")
	r.Register("P19-T19.3.13-C1", ckP19ModelValidation,
		"margin model validation (§13.10, §24 #206) — defined first, validated against spec")
	r.Register("P19-T19.3.15-C1", ckP19NettingHedging,
		"netting/hedging mode — defined first, validated against spec")
	r.Register("P19-T19.3.16-C1", ckP19MarginLevelDisplay,
		"margin level % display — defined first, validated against spec")
	r.Register("P19-T19.3.16-C2", ckP19StopOutTiers,
		"stop-out thresholds per tier — defined first, validated against spec")
	r.Register("P19-T19.3.17-C1", ckP19TieredLeverage,
		"tiered leverage — defined first, validated against spec")
	r.Register("P19-T19.3.18-C1", ckP19CorrelationOffset,
		"correlation-based margin offset — defined first, validated against spec")
	r.Register("P19-T19.3.19-C1", ckP19ADLPriority,
		"ADL priority recomputed and published at the liquidation cadence (§24 #269) — defined first, validated against spec")
	r.Register("P19-T19.3.20-C1", ckP19TimeoutNBP,
		"Cross-shard margin 2PC timeout and retail NBP restitution fail closed (§24 #320) — defined first, validated against spec")
	r.Register("P19-T19.3.21-C1", ckP19IndependentValidation,
		"independent margin validation with floors, calibrated segmented insurance custody, intraday buffers — defined first, validated against spec")
	r.Register("P19-T19.3.22-C1", ckP19LiquidationHistory,
		"per-account liquidation history with economics and audit links (§24 #360) — defined first, validated against spec")
	r.Register("P19-T19.3.23-C1", ckP19RuntimeChanges,
		"runtime leverage and margin-mode change within caps and compatibility guards (§24 #366) — defined first, validated against spec")
	r.Register("P19-T19.3.24-C1", ckP19EntityLeverageMatrix,
		"entity leverage matrix with most-restrictive-wins enforcement (§24 #371) — defined first, validated against spec")
	r.Register("P19-T19.3.25-C1", ckP19OptionSpreadMargin,
		"option spread margin offsets (§22 Task 22.3.13) — defined first, validated against spec")
	r.Register("P19-T19.3.25-C2", ckP19AmericanAssignment,
		"American option intra-day assignment pipeline (§22 Task 22.3.10) — defined first, validated against spec")
	r.Register("P19-T19.3.26-C1", ckP19EventEngine,
		"event-driven mark price margin engine and priority queue liquidation (§24 #410) — defined first, validated against spec")
	r.Register("P19-T19.3.27-C1", ckP19IsolatedMargin,
		"isolated margin position sub-allocation and balance protection (§24 #411) — defined first, validated against spec")
	r.Register("P19-T19.3.28-C1", ckP19IntradayCollateral,
		"intraday dynamic collateral haircut re-evaluation and volatility scaling (§24 #412) — defined first, validated against spec")
}

// --- Task 19.3.1: margin modes -------------------------------------------------

func ckP19MarginModes(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/margin.go",
			"services/internal/risk/margin_store.go",
			"services/internal/risk/margin_test.go",
			"services/internal/api/handlers_margin.go"),
		structural(env, "services/internal/risk/margin.go",
			"ModeCross", "ModeIsolated", "ModePortfolio"),
		structural(env, "services/cmd/gateway/main.go",
			"NewMarginService", "NewPgMarginStore"),
		gotest(p19Risk, "TestMarginMode|TestMarginService|TestMarginEvaluate|TestSetMarginMode|TestModeSwitch"),
	)
}

// --- Task 19.3.2: FX leverage --------------------------------------------------

func ckP19FXLeverage(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/leverage.go",
			"services/internal/risk/leverage_test.go",
			"services/internal/api/handlers_margin.go"),
		structural(env, "services/internal/risk/leverage.go",
			"ESMA", "CFTC"),
		gotest(p19Risk, "TestLeverageClassifyInstrumentGroup|"+
			"TestLeverageRegulatoryRegime|TestLeverageResolveRegulatoryCaps|"+
			"TestLeverageResolveProfessionalNoStatutoryCap"),
	)
}

// --- Task 19.3.3: margin calls + liquidation auction ---------------------------

func ckP19MarginCall(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/margin_call.go",
			"services/internal/risk/margin_call_store.go",
			"services/internal/risk/margin_call_test.go",
			"services/internal/risk/liquidation.go",
			"services/internal/risk/liquidation_store.go"),
		structural(env, "services/internal/risk/margin_call.go",
			"0.9", "margin_call"),
		structural(env, "services/internal/orders/service.go",
			"MarginCallGate", "MARGIN_CALL_EXCEEDED"),
		structural(env, "services/cmd/gateway/main.go",
			"WithMarginCall"),
		gotest(p19Risk, "TestPgLiquidationStore|TestMarginCall"),
	)
}

func ckP19AuctionLadder(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/auction.go",
			"services/internal/risk/auction_test.go"),
		structural(env, "services/internal/risk/auction.go",
			"CALL", "EXTEND", "FORCE_CASH", "AuctionCallDuration", "AuctionExtendBudget"),
		gotest(p19Risk, "TestForceCashCap|TestDedupKeyScoping|"+
			"TestNewLiquidationQueueNilRedis"),
	)
}

func ckP19AuctionFloors(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/risk/auction.go",
			"0.98", "1.02", "0.95", "1.05", "decay"),
		gotest(p19Risk, "TestFloorFor|TestDecayFloor"),
	)
}

func ckP19LPRebate(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/risk/liquidation.go",
			"LPRebate", "0.05", "closeTranches", "advSliceTrigger",
			"advSliceShare"),
		structural(env, "services/internal/risk/insurance_fund.go",
			"FundReasonLPRebate"),
		gotest(p19Risk, "TestInsuranceFundJournalLiquidationPenalty|"+
			"TestPenaltyAndDeficiency|TestCloseTranches"),
	)
}

// --- Task 19.3.4: insurance fund -------------------------------------------------

func ckP19InsuranceFund(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/insurance_fund.go",
			"services/internal/risk/insurance_fund_test.go",
			"services/internal/db/migrations/230_liquidation_risk.up.sql"),
		structural(env, "services/internal/db/migrations/230_liquidation_risk.up.sql",
			"insurance_fund_transactions", "insurance_fund_governance"),
		structural(env, "services/internal/risk/insurance_fund.go",
			"Depleted", "ADLNegativeFloor"),
		gotest(p19Risk, "TestInsuranceFund|TestPgInsuranceFund|TestFundMovementDirection"),
	)
}

// --- Task 19.3.5: exposure limits ------------------------------------------------

func ckP19ExposureLimits(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/exposure.go",
			"services/internal/risk/exposure_test.go"),
		gotest(p19Risk, "TestExposure"),
	)
}

// --- Task 19.3.6: GROSS-NET settlement mode ----------------------------------------

func ckP19GrossNet(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/settlement/gross_net.go",
			"services/internal/db/migrations/031_alter_instruments_add_settlement_mode.up.sql"),
		structural(env, "services/internal/db/migrations/031_alter_instruments_add_settlement_mode.up.sql",
			"settlement_mode", "GROSS", "NET"),
		gotest(p19Settlement, "TestGroupNetLegs|TestDispatchDue|"+
			"TestSettlementModeForPassesThrough|TestNetSettlementBatchesFiltersGross|"+
			"TestNewGrossNetServiceRejectsBadConfig"),
	)
}

// --- Task 19.3.7: prime-broker credit limits ---------------------------------------

func ckP19PBCredit(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/pb_credit.go",
			"services/internal/risk/pb_credit_test.go",
			"services/internal/db/migrations/232_pb_credit_reservations.up.sql"),
		structural(env, "services/internal/risk/pb_credit.go",
			"NOP", "DSL"),
		gotest(p19Risk, "TestPBReserveHeadroom|TestPBReleaseAndConsume|"+
			"TestPBSyncNetOpenPosition|TestPBDailyResetAndSweep|"+
			"TestPBUtilizationAlertBand|TestPgPBCreditStore"),
	)
}

// --- Task 19.3.8: collateral haircuts + concentration ------------------------------

func ckP19Collateral(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/collateral.go",
			"services/internal/risk/collateral_service.go",
			"services/internal/risk/collateral_test.go",
			"services/internal/db/migrations/041_collateral_schedule.up.sql"),
		gotest(p19Risk, "TestCollateralValuateHaircut|"+
			"TestCollateralConcentrationCapsExcess|"+
			"TestCollateralIneligibleAndAbsentContributeZero|"+
			"TestCollateralStaleRateContributesZero|"+
			"TestMarginEvaluateCollateralLeg|TestPgCollateralScheduleStore"),
	)
}

// --- Task 19.3.9: retail negative-balance protection -------------------------------

func ckP19NBP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/nbp.go",
			"services/internal/risk/nbp_test.go"),
		structural(env, "services/internal/risk/nbp.go",
			"EvaluateAccount", "SweepOnce", "nbp_events"),
		structural(env, "services/cmd/gateway/main.go",
			"NewNBPService", "SweepOnce", "America/New_York"),
		gotest(p19Risk, "TestNBPRetailDeficitRestitutedFromFund|"+
			"TestNBPProfessionalAndECPRemainLiable|"+
			"TestNBPHousePnLFallbackWhenFundCannotCover|"+
			"TestNBPSweepRestitutesOnlyRetail|TestPgNBPStoreIntegration"),
	)
}

// --- Task 19.3.10: bilateral credit ------------------------------------------------

func ckP19BilateralCredit(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/bilateral_credit.go",
			"services/internal/risk/bilateral_credit_test.go",
			"services/internal/db/migrations/053_bilateral_credit.up.sql"),
		structural(env, "services/internal/risk/bilateral_credit.go",
			"ONE_POOL", "TWO_POOL", "ReserveMatchTx", "ScreenLiquidity"),
		structural(env, "services/cmd/gateway/main.go",
			"NewBilateralCreditService", "ConsumeFill", "ReleaseOrder"),
		gotest(p19Risk, "TestBilateralService|TestPgBilateralCredit|TestScreenLiquidity"),
	)
}

// --- Task 19.3.11: cross-shard margin coordinator ------------------------------------

func ckP19MarginCoordinator(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/margin_coordinator.go",
			"services/internal/risk/margin_coordinator_test.go",
			"services/internal/db/migrations/058_shard_margin_reservations.up.sql",
			"services/cmd/risk/main.go"),
		structural(env, "services/cmd/risk/main.go",
			"MarginCtlInURI", "MarginCtlOutURI", "IngressFrame", "RunSweeper"),
		gotest(p19Risk, "TestMarginCoordinator|TestPgShardMargin"),
	)
}

// --- Task 19.3.12: internal position transfers ---------------------------------------

func ckP19PositionTransfers(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/settlement/position_transfer.go",
			"services/internal/accounting/transfer_ledger.go",
			"services/internal/db/migrations/061_position_transfers.up.sql"),
		gotest(p19Settlement, "TestTransfer"),
		gotest(p19Accounting, "TestTransferJournal"),
	)
}

// --- Task 19.3.13: margin-model validation + backtesting -----------------------------

func ckP19ModelValidation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/model_validation.go",
			"services/internal/risk/stress_engine.go",
			"services/internal/db/migrations/064_margin_model_runs.up.sql"),
		structural(env, "services/cmd/gateway/main.go",
			"NewStressEngine", "NewBacktester", "StressScheduler"),
		gotest(p19Risk, "TestRunStressSuitePersistsAndAlerts|"+
			"TestStressSchedulerRunDue|TestRunDailyBacktest|"+
			"TestBacktestRunDue|TestPredictedBoundAndBreached|"+
			"TestPgModelRunStoreRoundTrip"),
	)
}

// --- Task 19.3.15: netting/hedging mode ----------------------------------------------

func ckP19NettingHedging(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/position_mode.go",
			"services/internal/risk/position_mode_test.go",
			"services/internal/db/migrations/233_accounts_position_mode.up.sql"),
		structural(env, "services/internal/risk/position_mode.go",
			"NETTING", "HEDGING"),
		gotest(p19Risk, "TestPositionMode|TestPgPositionMode"),
		gotest(p19Settlement, "TestPositionOpenLong|TestPositionReversal"),
	)
}

// --- Task 19.3.16: margin-level display + thresholds ---------------------------------

func ckP19MarginLevelDisplay(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/margin_level.go",
			"services/internal/risk/margin_level_reader.go",
			"services/internal/risk/margin_level_test.go"),
		structural(env, "services/cmd/gateway/main.go",
			"MarginLevelWatcher", "PublishPrivate"),
		gotest(p19API, "TestAccountPositionsEnriched"),
	)
}

func ckP19StopOutTiers(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/risk/margin.go",
			"ThresholdsFor", "StopOut"),
		structural(env, "services/internal/risk/margin_level.go",
			"account_margin_thresholds"),
		files(env, "services/internal/db/migrations/235_account_margin_thresholds.up.sql"),
		gotest(p19Risk, "TestMarginThreshold|TestThresholds"),
	)
}

// --- Task 19.3.17: tiered leverage ------------------------------------------------------

func ckP19TieredLeverage(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/leverage_tiers.go",
			"services/internal/db/migrations/234_leverage_tiers.up.sql"),
		gotest(p19Risk, "TestLeverageResolveTierBandStepsDown|"+
			"TestLeverageTierAt|TestLeverageBandMargin|"+
			"TestLeverageTierEnums|TestPgTierStore"),
	)
}

// --- Task 19.3.18: correlation-based offsets ---------------------------------------------

func ckP19CorrelationOffset(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/correlation_offset.go",
			"services/internal/risk/correlation_offset_test.go"),
		structural(env, "services/internal/risk/correlation_offset.go",
			"90", "0.7", "0.8"),
		gotest(p19Risk, "TestCorrelationMatrixRefreshAndOffset|"+
			"TestCorrelationFactorClampAndDefault|"+
			"TestPortfolioMarginCorrelationOffset|"+
			"TestPortfolioMarginRegulatoryFloor|"+
			"TestRedisCorrelationMatrix"),
	)
}

// --- Task 19.3.19: ADL priority -----------------------------------------------------------

func ckP19ADLPriority(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/adl.go",
			"services/internal/risk/adl_test.go"),
		structural(env, "services/internal/risk/adl.go",
			"AdlScanCadence", "adl:indicator", "adl:priority"),
		structural(env, "services/cmd/gateway/main.go",
			"NewADLEngine", "NewADLIndicatorPublisher", "TickOnce"),
		gotest(p19Risk, "TestADLScorer|TestADLEngine|"+
			"TestPlanADLWritesShape|TestRedisADLPublisherTick|TestPgADLStore"),
	)
}

// --- Task 19.3.20: 2PC timeout + NBP restitution fail-closed ---------------------------------

func ckP19TimeoutNBP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/risk/margin_coordinator.go",
			"HardDeadline", "PessimisticFloor", "RECOVERY_ORPHAN"),
		structural(env, "services/internal/risk/nbp.go",
			"NBPSourceHousePnL", "HOUSE_PNL"),
		gotest(p19Risk, "TestMarginCoordinatorHardDeadlineCompensates|"+
			"TestMarginCoordinatorPessimisticFloor|"+
			"TestMarginCoordinatorLedgerFaultFailsClosed|"+
			"TestNBPHousePnLFallbackWhenFundCannotCover"),
	)
}

// --- Task 19.3.21: independent validation + client-money guard --------------------------------

func ckP19IndependentValidation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/risk/model_validation.go"),
		structural(env, "services/internal/risk/model_validation.go",
			"ParamChangeGate", "ClientMoneyGuard", "VerifySegregatedCustody",
			"CalibratedFundTarget", "GenerateRevalidationReport"),
		structural(env, "services/internal/api/handlers_margin_params.go",
			"AdminMarginParamChangeSubmit", "applyMarginParamChange",
			"RegisterMarginParamChangeExecutor"),
		structural(env, "services/cmd/gateway/main.go",
			"NewParamChangeGate", "RegisterMarginParamChangeExecutor"),
		structural(env, "services/internal/admin/dualcontrol.go",
			"OpMarginParamChange"),
		gotest(p19Risk, "TestParamChangeGate|TestCalibratedFundTarget|"+
			"TestVerifySegregatedCustody|TestClientMoneyGuard|"+
			"TestIntradayBufferMonitor|TestGenerateRevalidationReport|"+
			"TestPgParamChangeStoreRoundTrip|"+
			"TestMarginServiceEvaluateConcentration|"+
			"TestMarginServiceEvaluateLiquidity|"+
			"TestMarginServiceEvaluateAddons"),
		gotest(p19API, "TestAdminMarginParamChangeSubmit|TestApplyMarginParamChange"),
	)
}

// --- Task 19.3.22: private liquidation history ---------------------------------------------------

func ckP19LiquidationHistory(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/risk/liquidation_store.go",
			"liquidation_events"),
		structural(env, "services/internal/api/pagination.go",
			"/api/v1/account/liquidations"),
		gotest(p19API, "TestAccountLiquidations"),
		gotest(p19Risk, "TestPgLiquidationStoreRecordLiquidationEvent"),
	)
}

// --- Task 19.3.23: runtime leverage + margin-mode changes ---------------------------------------------

func ckP19RuntimeChanges(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/risk/leverage.go",
			"SetLeverage", "Invalidate"),
		structural(env, "services/internal/errs/codes.go",
			"MARGIN_MODE_SWITCH_BLOCKED"),
		gotest(p19API, "TestAccountMarginMode|TestAccountLeverageSet"),
		gotest(p19Risk, "TestLeverageSetLeverage|TestLeverageInvalidate"),
	)
}

// --- Task 19.3.24: entity leverage policy matrix -------------------------------------------------------

func ckP19EntityLeverageMatrix(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/098_entity_leverage_policy.up.sql"),
		structural(env, "services/internal/risk/leverage.go",
			"EntityPolicyCap", "strictest", "UpsertEntityPolicyTx",
			"ListEntityPolicies"),
		structural(env, "services/internal/api/handlers_margin_params.go",
			"AdminEntityLeveragePolicySubmit", "AdminEntityLeveragePolicyList",
			"RegisterEntityLeveragePolicyExecutor"),
		structural(env, "services/internal/marketapi/venue.go",
			"LeveragePolicies", "leverage_policies"),
		structural(env, "services/internal/admin/dualcontrol.go",
			"OpEntityLeveragePolicy"),
		gotest(p19Risk, "TestLeverageResolveEntityPolicyFallbackStrictest|"+
			"TestLeverageResolveMostRestrictive|TestPgLeverageStoreResolve|"+
			"TestPgEntityPolicyTxRoundTrip"),
		gotest(p19API, "TestAdminEntityLeveragePolicy"),
	)
}

// --- Task 19.3.25: option-delta margin + spread offsets -----------------------------------------------

func ckP19OptionSpreadMargin(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/option_margin.go",
			"services/internal/risk/spread_offsets.go",
			"services/internal/risk/option_margin_test.go"),
		gotest(p19Risk, "TestOptionDeltaAdjustment|"+
			"TestDeltaOptionMarginEvaluator|TestSpreadOffset"),
	)
}

func ckP19AmericanAssignment(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/risk/option_margin.go",
			"Assignment|American|assignment"),
	)
}

// --- Task 19.3.26: event-driven margin engine ------------------------------------------------------------

func ckP19EventEngine(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/margin_engine.go",
			"services/internal/risk/priority_queue.go",
			"services/internal/risk/liquidation_dispatcher.go",
			"services/internal/risk/mark_tick_ring.go",
			"services/internal/risk/margin_engine_test.go"),
		structural(env, "services/cmd/gateway/main.go",
			"NewMarginEngine", "NewRedisMarkSource", "PublishMarkDelta"),
		gotest(p19Risk, "TestMarginLevelHeapOrdering|TestMarginLevelHeapChurn|"+
			"TestMarginEngineTickEvaluatesAndDispatchesStopOut|"+
			"TestMarginEngineUnrelatedTickDoesNotEval|"+
			"TestMarginEngineEvalLatencyBudget|TestMarginEngineRealQueueDedup"),
	)
}

// --- Task 19.3.27: isolated margin sub-allocation -----------------------------------------------------------

func ckP19IsolatedMargin(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/isolated_margin.go",
			"services/internal/risk/isolated_margin_test.go",
			"services/internal/db/migrations/106_positions_isolated_margin.up.sql"),
		structural(env, "services/internal/risk/isolated_margin.go",
			"AllocateInitialMargin", "CheckAccount", "isolated_margin_allocated"),
		structural(env, "services/cmd/gateway/main.go",
			"NewIsolatedMarginService", "NewPgIsolatedMarginStore"),
		gotest(p19Risk, "TestIsolatedLevelPct|TestAllocateInitialMargin|"+
			"TestCheckAccountReplenishAvertsLiquidation|"+
			"TestCheckAccountDeficitDispatchesPositionScoped|"+
			"TestPgIsolatedMarginStoreLifecycle"),
	)
}

// --- Task 19.3.28: intraday collateral haircut re-evaluation ---------------------------------------------------

func ckP19IntradayCollateral(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/risk/collateral_service.go",
			"services/internal/risk/volatility_scaler.go",
			"services/internal/risk/collateral_service_test.go",
			"services/internal/risk/volatility_scaler_test.go"),
		structural(env, "services/cmd/gateway/main.go",
			"NewCollateralMonitor", "NewVolatilityScaler"),
		gotest(p19Risk, "TestCollateralMonitorTriggerRecomputesAndCalls|"+
			"TestCollateralMonitorBelowTriggerStaysQuiet|"+
			"TestVolatilityScalerScalesAboveGate|"+
			"TestVolatilityScalerCapAt1Point5|"+
			"TestMarginEvaluateVolatilityScalesUsedMargin"),
	)
}
