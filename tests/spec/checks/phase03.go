package checks

import (
	"context"
	"strings"

	spec "exchange-testspec/spec"
)

// Phase-03 risk & settlement checkpoints.
//
// The landed implementation is Go under services/internal/{settlement,
// ledger, position, balance, risk, bridge} with migrations under
// services/internal/db/migrations. Each checker requires the owning files
// to exist and re-runs the unit tests that prove the criterion — corpus
// `[x]` checkboxes are only ever marked when these pass.
func registerPhase03(r *spec.Registry) {

	// ---- Task 3.3.1: Post-Trade Balance Service ---------------------------
	r.Register("P03-T3.3.1-C1", ckP03SerializableBalances,
		"SERIALIZABLE balance mutations + retry/backoff — BalanceService")
	r.Register("P03-T3.3.1-C2", ckP03AccountMutex,
		"per-account mutex via Redis SETNX, ordered lock release")
	r.Register("P03-T3.3.1-C3", ckP03IdempotentTradeProcessing,
		"idempotent trade processing via processed_trades dedup")

	// ---- Task 3.3.2: Position Management Service --------------------------
	r.Register("P03-T3.3.2-C1", ckP03PositionTracking,
		"position tracking with realized/unrealized P&L")

	// ---- Task 3.3.3: Settlement Date Calculator ---------------------------
	r.Register("P03-T3.3.3-C1", ckP03SettlementDates,
		"T+1/T+2 settlement per FX standard with weekend/holiday roll")
	r.Register("P03-T3.3.3-C2", ckP03SameDaySettlement,
		"same-day settlement for USD/CAD + USD/MXN")
	r.Register("P03-T3.3.3-C3", ckP03SwiftMessages,
		"SWIFT MT202 / pacs.009 generation for value-date instructions")

	// ---- Task 3.3.4: Fee Calculation Service ------------------------------
	r.Register("P03-T3.3.4-C1", ckP03MakerTakerFees,
		"maker/taker fee tiers with tier resolution")
	r.Register("P03-T3.3.4-C2", ckP03PromoWindows,
		"promo rate windows (partial window clamps, no negative clamp leak)")

	// ---- Task 3.3.5: Risk Limit Enforcement Service -----------------------
	r.Register("P03-T3.3.5-C1", ckP03RiskLimits,
		"per-account + per-symbol exposure limits, fail-closed")

	// ---- Task 3.3.6: Double-Entry Ledger Posting --------------------------
	r.Register("P03-T3.3.6-C1", ckP03DoubleEntry,
		"double-entry invariant SUM(debit)==SUM(credit) per journal+ccy")

	// ---- Task 3.3.7: Rollover / EOD Mark Service --------------------------
	r.Register("P03-T3.3.7-C1", ckP03EodRollover,
		"automated EOD spot rollover 17:00 NY with Wednesday triple roll")

	// ---- Task 3.3.8: Multi-Currency Holiday Calendar ----------------------
	r.Register("P03-T3.3.8-C1", ckP03HolidayCalendar,
		"multi-currency holiday calendar engine")

	// ---- Task 3.3.9: Multi-Currency P&L Base Conversion -------------------
	r.Register("P03-T3.3.9-C1", ckP03PnlConversion,
		"multi-currency P&L base conversion via oracle rates")

	// ---- Task 3.3.11: Overnight Swap Engine --------------------------------
	r.Register("P03-T3.3.11-C1", ckP03OvernightSwap,
		"overnight swap accrual qty×points×lot_size, long/short signed")
	r.Register("P03-T3.3.11-C2", ckP03TripleSwap,
		"triple-swap Wednesday + holiday 4x/5x day spans")

	// ---- Task 3.3.12: Pip Value Calculator ---------------------------------
	r.Register("P03-T3.3.12-C1", ckP03PipValue,
		"pip value calculation incl. JPY pip size and cross pairs")

	// ---- Task 3.3.13: Commission Engine ------------------------------------
	r.Register("P03-T3.3.13-C1", ckP03DualFeeModel,
		"dual fee model: SPREAD_MARKUP + RAW_SPREAD_COMMISSION")

	// ---- Task 3.3.14: Auto-Exchange Deficit Settlement ----------------------
	r.Register("P03-T3.3.14-C1", ckP03AutoExchange,
		"multi-asset auto-exchange deficit settlement with GL journal")

	// ---- Task 3.3.15: Carry-Trade Swap Yield --------------------------------
	r.Register("P03-T3.3.15-C1", ckP03CarryTrade,
		"carry-trade swap yield tracking + daily rollover settlement")

	// ---- Task 3.3.16: VIP Tier Engine ----------------------------------------
	r.Register("P03-T3.3.16-C1", ckP03VipTiers,
		"VIP 0–9 tier engine, daily 00:00 UTC volume recompute")

	// ---- Task 3.3.17: Negative Maker Fee Rebates ------------------------------
	r.Register("P03-T3.3.17-C1", ckP03MakerRebates,
		"negative maker fee rebates as liquidity expense GL accounting")

	// ---- Task 3.3.18: Invariant Compensation ----------------------------------
	r.Register("P03-T3.3.18-C1", ckP03InvariantCompensation,
		"balance invariant verification, serialization retry, compensation")

	// ---- Task 3.3.19: Chart of Accounts -----------------------------------------
	r.Register("P03-T3.3.19-C1", ckP03ChartOfAccounts,
		"full FX chart of accounts, seed superset of migration 036")

	// ---- Task 3.3.20: Dust Conversion ---------------------------------------------
	r.Register("P03-T3.3.20-C1", ckP03DustConversion,
		"dust eligibility + disclosed-spread conversion, balanced journal")

	// ---- Task 3.3.21: Cent-Denominated Accounting -----------------------------------
	r.Register("P03-T3.3.21-C1", ckP03MinorUnit,
		"minor-unit posting, zero-balance switching, single helper")

	// ---- Task 3.3.22: Physical Delivery Partitioning ---------------------------------
	r.Register("P03-T3.3.22-C1", ckP03DeliveryPartition,
		"physical delivery vs rolling spot ledger partitioning (§24 #284)")

	// ---- Task 3.3.23: Swap-Free Administrative Fee -------------------------------------
	r.Register("P03-T3.3.23-C1", ckP03SwapFreeFee,
		"swap-free admin holding fee after grace period (§24 #407)")
}

// svcFiles checks that files exist under services/.
func svcFiles(env *spec.Env, paths ...string) spec.Result {
	full := make([]string, len(paths))
	for i, p := range paths {
		full[i] = "services/" + p
	}
	return spec.RequireFiles(env, full...)
}

// goTestPattern runs `go test -count=1 -run <pat> <pkg>` inside services/.
func goTestPattern(ctx context.Context, env *spec.Env, pkg, pat string) spec.Result {
	out, err := spec.RunOutput(ctx, env.Path("services"),
		"go", "test", "-count=1", "-run", "^("+pat+")$", pkg)
	if err != nil {
		return spec.Failf("go test %s -run %s: %v — %s",
			pkg, pat, err, spec.Tail(out, 10))
	}
	return spec.Passf("%s: %s", pkg, pat)
}

// ck is a compact factory: require files, then run the proving tests.
func ck(files []string, pkg, testPat string) func(context.Context, *spec.Env) spec.Result {
	return func(ctx context.Context, env *spec.Env) spec.Result {
		if r := svcFiles(env, files...); r.Status != spec.StatusPass {
			return r
		}
		return goTestPattern(ctx, env, pkg, testPat)
	}
}

// ckContains additionally requires each file to contain its listed symbols.
func ckContains(fileSymbols map[string][]string, pkg, testPat string) func(context.Context, *spec.Env) spec.Result {
	return func(ctx context.Context, env *spec.Env) spec.Result {
		var files []string
		for f := range fileSymbols {
			files = append(files, f)
		}
		if r := svcFiles(env, files...); r.Status != spec.StatusPass {
			return r
		}
		for f, syms := range fileSymbols {
			if r := spec.FileContains(env, "services/"+f, syms...); r.Status != spec.StatusPass {
				return r
			}
		}
		if testPat == "" {
			return spec.Passf("files + symbols present: %s", strings.Join(files, ", "))
		}
		return goTestPattern(ctx, env, pkg, testPat)
	}
}

const (
	settle   = "./internal/settlement"
	ledger   = "./internal/ledger"
	position = "./internal/position"
	balance  = "./internal/balance"
	risk     = "./internal/risk"
)

var ckP03SerializableBalances = ckContains(map[string][]string{
	"internal/settlement/balance_service.go": {"pgx.Serializable"},
}, settle, "TestSerializationRetrySucceeds|TestSerializationRetryExhausted|TestProcessFillsSingleTxBatch")

var ckP03AccountMutex = ckContains(map[string][]string{
	"internal/settlement/balance_service.go": {"account:lock"},
}, settle, "TestMutexContentionAbortsBeforeTx|TestLocksAcquiredSortedAndReleased|TestMissingLockBackendFailsClosed")

var ckP03IdempotentTradeProcessing = ckContains(map[string][]string{
	"internal/settlement/balance_service.go": {"processed_trades"},
}, settle, "TestProcessFillsIdempotentReplay|TestProcessFillsDuplicateWithinBatch|TestPositionDuplicateFill")

var ckP03PositionTracking = ckContains(map[string][]string{
	"internal/settlement/position_service.go": {"realized", "unrealized"},
}, settle, "TestPositionOpenLong|TestPositionPartialCloseRealized|TestPositionReversal|TestPositionIncreaseVWAP|TestPositionLimitEnforced")

var ckP03SettlementDates = ckContains(map[string][]string{
	"internal/settlement/settlement_service.go": {},
}, settle, "TestSettlementT1PlainWeekday|TestSettlementT2InterimHoliday|TestSettlementHolidayShift|TestSettlementModifiedFollowingMonthEnd|TestSettlementNeverBeforeTradeDate")

var ckP03SameDaySettlement = ckContains(map[string][]string{
	"internal/settlement/settlement.go":      {"CycleSameDay"},
	"internal/settlement/settlement_service.go": {},
}, settle, "TestGenerateInstructionsSameDayUSDCAD|TestGenerateInstructionsSameDayUSDMXN|TestSettlementSameDayUSDCAD")

var ckP03SwiftMessages = ckContains(map[string][]string{
	"internal/settlement/settlement_service.go": {"MT202", "pacs"},
}, settle, "TestSwiftMT202MarshalFields|TestSwiftMT202RejectsBadInput|TestDispatchDueMT202|TestDispatchDuePacs009")

var ckP03MakerTakerFees = ckContains(map[string][]string{
	"internal/settlement/fee_service.go": {},
}, settle, "TestApplyFeeMakerRate|TestRateBpsMakerTaker|TestApplyFeeNoTierFailsClosed|TestApplyFeeZeroTierNoPosting")

var ckP03PromoWindows = ckContains(map[string][]string{
	"internal/settlement/fee_service.go": {"promo"},
}, settle, "TestRateBpsPromoWindow|TestApplyFeePromoRate|TestRateBpsPartialPromo|TestRateBpsNegativePromoClamps")

var ckP03RiskLimits = ck(
	[]string{"internal/risk/limits_service.go"},
	risk,
	"TestCheckOrderSymbolExposure|TestCheckOrderDailyVolume|TestCheckOrderOpenOrders|TestCheckOrderReduceOnly|TestCheckOrderMissingPriceFailsClosed|TestCheckOrderStoreErrorFailsClosed|TestLimitsViewUtilization")

var ckP03DoubleEntry = ckContains(map[string][]string{
	"internal/ledger/posting.go": {"debit", "credit"},
}, ledger, "TestValidateBalancedJournal|TestValidateImbalanceAborts|TestValidateImbalancePerCurrency|TestValidateRequiresTwoLines|TestLedgerIntegration")

var ckP03EodRollover = ckContains(map[string][]string{
	"internal/settlement/rollover_service.go": {"America/New_York"},
}, settle, "TestRolloverCutoff_EDT|TestRolloverCutoff_EST|TestRolloverClock_CutoffOnDST|TestIntegration_RolloverTuesday|TestIntegration_RolloverWednesdayTriple|TestIntegration_RolloverWeekend")

var ckP03HolidayCalendar = ckContains(map[string][]string{
	"internal/settlement/calendar_service.go": {},
}, settle, "TestIsHolidayAndBusinessDay|TestSettlementSplitHoliday|TestGenerateInstructionsHolidayShift")

var ckP03PnlConversion = ck(
	[]string{"internal/position/pnl_converter.go"},
	position,
	"TestConvertDirect|TestConvertCrossViaUSD|TestConvertInverse|TestConvertNoPathFails|TestConvertZeroRateFails|TestConvertOracleErrorPropagates")

var ckP03OvernightSwap = ckContains(map[string][]string{
	"internal/settlement/swap_engine.go": {"InterbankSwapCharge", "SWAP_RATE_STALE"},
}, settle, "TestProcessRollover_LongShortSigns|TestProcessRollover_StaleAndMissingRates|TestProcessRollover_ZeroRateNoCharge|TestProcessRollover_SwapFreeForegone|TestProcessRollover_OpenedAfterCutoff|TestInterbankSwapCharge|TestSwapAccrualDayCount")

var ckP03TripleSwap = ckContains(map[string][]string{
	"internal/settlement/swap_engine.go": {},
}, settle, "TestProcessRollover_TripleWednesday|TestRolloverDays_PlainAndTripleWednesday|TestRolloverDays_HolidayWeekend4x5x|TestSwapAccrualTripleDay")

var ckP03PipValue = ckContains(map[string][]string{
	"internal/settlement/pip_calculator.go": {},
}, settle, "TestPipValueDirectPair|TestPipValueJPYPipSize|TestPipValueCrossPair|TestPipValueCrossPairInverseLeg|TestPipValueIndirectPairAccountIsBase|TestPipValueStalePriceFailsClosed|TestPipValueNoConversionPath")

var ckP03DualFeeModel = ckContains(map[string][]string{
	"internal/settlement/commission_engine.go": {"SPREAD_MARKUP", "RAW_SPREAD_COMMISSION"},
}, settle, "TestAssessSpreadMarkup|TestAssessRawSpreadCommission|TestCommissionTierChargeAndEffectiveSpread|TestAssessUnknownModel|TestZeroCommissionTier")

var ckP03AutoExchange = ckContains(map[string][]string{
	"internal/settlement/auto_exchange.go": {},
}, balance, "TestSettleQuoteCurrencyLoss|TestSettleQuoteCurrencyProfit|TestSettleSweepLoss|TestSettleSweepToBase|TestSettleFailClosed|TestSettleInsufficientCollateral|TestSettleDuplicateRejected|TestSettleZeroPnLNoOp")

var ckP03CarryTrade = ckContains(map[string][]string{
	"internal/settlement/carry_trade_settlement.go": {"carry:"},
}, settle, "TestCarrySettleDay_HedgedNetNegative|TestCarrySettleDay_PositiveNetAndCumulative|TestCarrySettleDay_ReplayIdempotent|TestCarrySettleDay_FailedAllocationPreserved|TestCarrySettleDay_ZeroNetNoJournal")

var ckP03VipTiers = ck(
	[]string{
		"internal/settlement/vip_engine.go",
		"internal/db/migrations/086_vip_tiers.up.sql",
	},
	settle,
	"TestTierForBoundaries|TestTierForEmptySchedule|TestRunOnceFamilyTierAndHistory|TestRunOnceUnchangedTierStillAudits|TestRunOnceInactiveMasterSkipsFamily|TestNextUTCMidnight|TestDailyResetBoundary")

var ckP03MakerRebates = ckContains(map[string][]string{
	"internal/settlement/commission_engine.go": {"MakerRebate"},
}, settle, "TestMakerRebateJournal|TestMakerRebateAlongsideCommission|TestNegativeMakerBelowVip4Rejected|TestApplyFeeRebate")

var ckP03InvariantCompensation = ckContains(map[string][]string{
	"internal/settlement/invariant_compensation.go": {},
}, settle, "TestImbalanceAbortPropagatesAndPagesP0|TestCompensateRailRejectionFlow|TestCompensationJournalZeroSum|TestCompensateInvalidRejection|TestCompensatePostFailureStillAlertsP0|TestNonRetryableErrorNoRetry")

var ckP03ChartOfAccounts = ck(
	[]string{
		"internal/ledger/chart.go",
		"internal/db/migrations/088_gl_chart_of_accounts.up.sql",
	},
	ledger,
	"TestAllBuildersResolveInDefaultChart|Test036SeedSubsetOf088|TestValidateAccountsCurrencyMismatch|TestValidateAccountsNilChartFailsClosed|TestValidateAccountsUnknownAborts")

var ckP03DustConversion = ckContains(map[string][]string{
	"internal/ledger/dust_convert.go": {"spread"},
}, ledger, "TestDustSweepEligible|TestDustSweepIneligible|TestDustSweepInvertedPairThreshold|TestDustSweepIdempotentPerDay|TestDustSweepPendingClaim|TestConversionSpreadDisclosure")

var ckP03MinorUnit = ck(
	[]string{
		"internal/ledger/subunit.go",
		"internal/db/migrations/096_cent_subunit_ledger.up.sql",
	},
	ledger,
	"TestStorageAmount|TestMinorUnitZeroSumInvariant|TestProfileSwitchZeroBalanceGuard|TestDisplayAmount|TestDisplayAmountRejectsBadDivisor|TestAccountTypeSwitchMidMonth")

var ckP03DeliveryPartition = ckContains(map[string][]string{
	"internal/db/migrations/104_accounts_settlement_intent.up.sql": {"settlement_intent"},
}, settle, "TestPhysicalDeliveryJournalLocksDeliverables|TestIntegration_PhysicalDeliveryExcluded|TestMixedSettlementIntentAborts|TestRollingFillFeeExceedsProceedsAborts")

var ckP03SwapFreeFee = ck(
	[]string{
		"internal/settlement/swapfree_fee_service.go",
		"internal/db/migrations/105_swap_free_admin_fees.up.sql",
	},
	settle,
	"TestComputeAdminFee_GraceBoundary|TestIntegration_SwapFree|TestSelectDormancyFeeHonoursStatus|TestSelectDormancyFeeTiers|TestProcessRollover_SwapFreeForegone")
