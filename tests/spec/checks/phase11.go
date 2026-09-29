package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-11 funding via banking rails, suspension, and statistics
// checkpoints.
//
// Code tasks bind to real Go tests (rail envelopes, withdrawal/deposit
// flows, kill-switch service + gateway gate, market statistics, fee
// schedule, whitelist, return-code mapping) plus migration files. The
// C++ pre-trade suspension leg binds structurally — SuspensionFlags and
// SuspensionRefresher have no dedicated gtest yet. Live bank dispatch
// stays fail-closed by design and is not asserted here.
const (
	p11Fund   = "./internal/funding"
	p11Admin  = "./internal/admin"
	p11Market = "./internal/marketapi"
	p11API    = "./internal/api"
	p11MW     = "./internal/middleware"
)

func registerPhase11(r *spec.Registry) {
	r.Register("P11-T11.3.1-C1", ckP11Rails, "SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2 banking rails")
	r.Register("P11-T11.3.2-C1", ckP11WithdrawalWindow, "15min withdrawal confirmation window")
	r.Register("P11-T11.3.2-C2", ckP11ReviewTiers, "review tiers <$10K/$10K-$50K/>$50K+4h")
	r.Register("P11-T11.3.2-C3", ckP11CooldownHold, "30min cooldown + 24h new account hold")
	r.Register("P11-T11.3.3-C1", ckP11DepositTiers, "deposit anti-fraud tiers")
	r.Register("P11-T11.3.3-C2", ckP11DualSource, "dual-source bank confirmation")
	r.Register("P11-T11.3.4-C1", ckP11GlobalKillSwitch, "global kill-switch with dual control")
	r.Register("P11-T11.3.5-C1", ckP11MarketStats, "24h market statistics")
	r.Register("P11-T11.3.6-C1", ckP11Nostro, "nostro-aware withdrawals (nostro tracking)")
	r.Register("P11-T11.3.7-C1", ckP11Beneficiary, "verified beneficiary registry + third-party rejection")
	r.Register("P11-T11.3.7-C2", ckP11RailCutoff, "per-rail cut-off enforcement")
	r.Register("P11-T11.3.8-C1", ckP11ScopedKillSwitch, "scoped kill-switch")
	r.Register("P11-T11.3.9-C1", ckP11FeeSchedule, "funding fee schedule")
	r.Register("P11-T11.3.10-C1", ckP11Whitelist, "withdrawal whitelist mode with 24h timelocks")
	r.Register("P11-T11.3.11-C1", ckP11ReturnCodes, "rail return-code mapping + third-party fraud quarantine")
	r.Register("P11-T11.3.12-C1", ckP11MultiDimKillSwitch, "multi-dimensional scoped emergency kill-switches")
}

func ckP11Rails(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestRailMatrixComplete|TestSwiftMT103Envelope|TestSwiftMT202BankToBank|TestSepaEnvelopes|TestFedNowEnvelopeUSDOnly|TestACHEnvelopeCents|TestChapsTarget2Envelopes|TestAssertRailOperational"),
		gotest(p11API, "TestP11RailsMatrix|TestP11RailSelection"),
	)
}

func ckP11WithdrawalWindow(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestITWithdrawalLifecycle|TestITWithdrawalExpirySweep|TestFlowConfirmRequiresTOTP|TestFlowConfirmWithoutTOTPProviderFailsClosed"),
		structural(env, "services/internal/funding/withdrawals.go", "WithdrawalConfirmWindow", "ExpiresAt"),
	)
}

func ckP11ReviewTiers(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestTierForUSD|TestFlowAdminReviewFourEyes|TestSweepReviewSLAEscalates"),
	)
}

func ckP11CooldownHold(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestFlowCreateCooldownReject|TestITUnverifiedDestinationHold|TestFlowCreateUnverifiedDestinationHold"),
		structural(env, "services/internal/funding/withdrawal_flow.go", "WithdrawalCooldown"),
	)
}

func ckP11DepositTiers(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestDepositStandardTierSanctionsGate|TestDepositStandardTierCreditsWithScreener|TestDepositPendingReviewTierAndAdmin|TestDepositIntentIdempotency"),
	)
}

func ckP11DualSource(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestDepositDualSourceAutoCredit|TestDepositSameSourceReplayAndMismatch|TestITDepositFlowLifecycle|TestDepositIntentAdoptedByDetection"),
	)
}

func ckP11GlobalKillSwitch(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Admin, "TestKillSwitchServiceIntegration|TestKillSwitchRoles_FailClosed"),
		gotest(p11MW, "TestKillSwitchGate_RejectsNewOrdersWhenHalted|TestKillSwitchGate_CancelsAndReadsSurvive|TestKillSwitchGate_FailClosed"),
		files(env, "services/internal/db/migrations/200_trading_suspensions.up.sql"),
		structural(env, "core/include/risk/PreTradeChecker.hpp", "SuspensionFlags"),
		structural(env, "core/src/risk/SuspensionRefresher.cpp", "Suspension"),
	)
}

func ckP11MarketStats(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Market, "TestPgStoreStats24hIntegration"),
		gotest(p11API, "TestStats24hAll|TestStats24hAll_StoreErrorDegraded|TestStats24hSymbol|TestStats24hSymbol_Unknown404"),
	)
}

func ckP11Nostro(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestITNostroDispatch|TestDispatchSufficientNostro|TestDispatchInsufficientNostroQueues|TestReplenishmentDualControl|TestReplenishmentOverdraftRefuses|TestNostroCoverageDeficit|TestDispatchHeldUntilLapses|TestDispatchSweepRetries"),
	)
}

func ckP11Beneficiary(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestITBankAccountsLifecycle|TestBeneficiaryRegister_KYCGate|TestBeneficiaryVerify_DualControl|TestBeneficiaryAssertWithdrawable|TestDepositGuardAccepts|TestDepositGuardNameMismatchQuarantines|TestDepositGuardDedup"),
		files(env, "services/internal/db/migrations/040_bank_accounts.up.sql"),
	)
}

func ckP11RailCutoff(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestRailCutoffSameDay|TestRailCutoffWeekendRoll|TestRailSelectionSepaOverInstantCap"),
	)
}

func ckP11ScopedKillSwitch(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Admin, "TestKillSwitchResolve_Precedence|TestKillSwitchResolve_FailClosed|TestKillSwitchScopeVocabulary|TestNormalizeScope"),
		gotest(p11MW, "TestKillSwitchGate_CancelExemptHeaderBypass|TestKillSwitchGate_OpenPasses"),
	)
}

func ckP11FeeSchedule(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestITMigration198RoundTrip|TestITFeeScheduleAdminLifecycle|TestITFeeEstimateCharge|TestITFreeTierUsage|TestITConversionPersistence|TestFeeTierFormula|TestFeeChargePostsJournal|TestFeeScheduleRoleGate|TestConversionDepositSpread|TestConversionFailClosedNoSource"),
		gotest(p11API, "TestFundingFeeEstimate|TestAdminFundingFeeCreate|TestAdminFundingFeeUpdateAndRetire|TestFundingConvert|TestFundingConversions"),
		files(env, "services/internal/db/migrations/198_funding_fee_schedule.up.sql"),
	)
}

func ckP11Whitelist(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestITWhitelistLifecycle|TestITWithdrawalFlowWhitelistGate|TestGateWhitelistOnlyRejectsUnverified|TestGateWhitelistOnlyAdmitsVerifiedUnlocked|TestGateBeneficiaryTimelock|TestGateDeactivationLockBlocks|TestWhitelistEnableDisableLatch|TestWhitelistDisableIdempotent"),
		files(env, "services/internal/db/migrations/078_withdrawal_whitelist_settings.up.sql"),
	)
}

func ckP11ReturnCodes(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Fund, "TestReturnCodeMappingSpecPins|TestReturnCodeUnknownQuarantines|TestApplyReturnCompensating|TestApplyReturnUnknownQuarantinesAndAlerts|TestResolveSuspenseReleaseAndReturn|TestResolveSuspenseUnattributableReleaseFails|TestITSuspenseAndRailPayments"),
		files(env, "services/internal/db/migrations/108_suspense_accounts_routing.up.sql"),
	)
}

func ckP11MultiDimKillSwitch(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p11Admin, "TestKillSwitchResolve_RailAndLP|TestKillSwitchResolve_OrderHaltDetail|TestKillSwitchResolve_Precedence"),
		structural(env, "services/internal/middleware/killswitch.go", "KillSwitchGate"),
	)
}
