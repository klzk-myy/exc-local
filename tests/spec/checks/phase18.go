package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-18 FIX Protocol Gateway checkpoints (Tasks 18.3.1–18.3.18).
// Evidence split: the session core, order entry, entitlement/CoD/throttle,
// market data, session status, drop copy, PB affirmation, allocations,
// failover/gapfill/drain, mass quoting and dead-man live under
// services/internal/fix; SBE/Aeron under internal/fixsbe; SOR under
// internal/sor; the MM program under internal/marketmaking; FIXS
// certification under internal/fix/certification. PG-gated persistence
// legs run under EXC_PG_TEST=1; the Redis seq-store leg under
// EXC_REDIS_TEST=1.
const (
	p18Fix    = "./internal/fix"
	p18FixSBE = "./internal/fixsbe"
	p18SOR    = "./internal/sor"
	p18MM     = "./internal/marketmaking"
	p18Cert   = "./internal/fix/certification"
)

func registerPhase18(r *spec.Registry) {
	r.Register("P18-T18.3.1-C1", ckP18SessionMgmt,
		"FIX 4.4 session management — defined first, validated against spec")
	r.Register("P18-T18.3.2-C1", ckP18OrderEntry,
		"FIX order submission — defined first, validated against spec")
	r.Register("P18-T18.3.3-C1", ckP18MarketData,
		"FIX market data — defined first, validated against spec")
	r.Register("P18-T18.3.4-C1", ckP18DropCopy,
		"FIX drop copy — defined first, validated against spec")
	r.Register("P18-T18.3.5-C1", ckP18Fix50,
		"FIX 5.0 SP2 derivatives — defined first, validated against spec")
	r.Register("P18-T18.3.5-C2", ckP18FXTags,
		"FX-specific FIX tags (Currency, SettlementType, NoPartyIDs) — defined first, validated against spec")
	r.Register("P18-T18.3.6-C1", ckP18PBDropCopy,
		"PB drop copy & trade affirmation — defined first, validated against spec")
	r.Register("P18-T18.3.7-C1", ckP18MassQuote,
		"FIX mass quoting & firm liquidity — defined first, validated against spec")
	r.Register("P18-T18.3.8-C1", ckP18SBE,
		"SBE / Aeron binary gateway — defined first, validated against spec")
	r.Register("P18-T18.3.9-C1", ckP18Entitlement,
		"FIX entitlement + CoD + throttle (§9.3, §24 #135–137) — defined first, validated against spec")
	r.Register("P18-T18.3.10-C1", ckP18MMProgram,
		"MM program + MMP protection (§9.6, §24 #139) — defined first, validated against spec")
	r.Register("P18-T18.3.11-C1", ckP18Certification,
		"FIXS mTLS + client certification (§9.7, §24 #167) — defined first, validated against spec")
	r.Register("P18-T18.3.12-C1", ckP18Failover,
		"FIX gateway failover & sequence sync (§9.8, §24 #189) — defined first, validated against spec")
	r.Register("P18-T18.3.13-C1", ckP18Allocation,
		"FIX allocation instructions & reports — defined first, validated against spec")
	r.Register("P18-T18.3.14-C1", ckP18SOR,
		"smart order routing & external liquidity — defined first, validated against spec")
	r.Register("P18-T18.3.15-C1", ckP18SessionStatus,
		"FIX trading session status 35=h — defined first, validated against spec")
	r.Register("P18-T18.3.16-C1", ckP18DeadMan,
		"FIX dead-man switch (canonical account timer) — defined first, validated against spec")
	r.Register("P18-T18.3.17-C1", ckP18Drain,
		"SBE transport + graceful maintenance drain — defined first, validated against spec")
	r.Register("P18-T18.3.18-C1", ckP18GapRecovery,
		"sequence-gap resolution, rejects & CoD recovery — defined first, validated against spec")
}

// --- Task 18.3.1: session management -----------------------------------------

func ckP18SessionMgmt(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fix/app.go",
			"services/internal/fix/gateway.go",
			"services/internal/fix/store.go",
			"services/cmd/fix/main.go",
			"services/internal/db/migrations/030_create_fix_sessions.up.sql"),
		structural(env, "services/internal/db/migrations/030_create_fix_sessions.up.sql",
			"fix_sessions", "sender_seq_num", "target_seq_num"),
		gotest(p18Fix, "TestFromAdmin_UnknownSessionRejected|"+
			"TestFromAdmin_BadCredentialRejected|"+
			"TestFromAdmin_CredentialAccountMismatch|TestFromAdmin_ValidLogon|"+
			"TestPGStore_SessionLifecycleAndSeq|TestPGStore_MessageArchive"),
	)
}

// --- Task 18.3.2: order submission ---------------------------------------------

func ckP18OrderEntry(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fix/mapfix.go",
			"services/internal/fix/orders.go",
			"services/internal/fix/report.go"),
		gotest(p18Fix, "TestMapNewOrderSingle_Limit|"+
			"TestMapNewOrderSingle_MarketNeedsNoPrice|"+
			"TestMapNewOrderSingle_StopLimitRequiresBothPrices|"+
			"TestMapNewOrderSingle_IcebergViaMaxFloor|"+
			"TestMapNewOrderSingle_GTDRequiresExpireTime|"+
			"TestMapNewOrderSingle_Rejects|TestMapCancelReplace|"+
			"TestOrdStatusMap_Complete|TestOrdRejReasonMap|"+
			"TestReportAccepted_Fields|TestReportFill_PartialVsFull|"+
			"TestReportCanceledAndReplaced|TestCancelReject_Fields|"+
			"TestFromApp_NewOrderSingleAccepted"),
	)
}

// --- Task 18.3.3: market data --------------------------------------------------

func ckP18MarketData(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/fix/mdata.go"),
		gotest(p18Fix, "TestSubscribeSnapshotThenIncremental|"+
			"TestIncrementalBookShrinkEmitsDeletes|TestUnknownSymbolRejected|"+
			"TestDuplicateMDReqIDRejected|TestMaxSubscriptionsHonored|"+
			"TestUnsubscribeRemoves|TestSnapshotOnlyLeavesNoSubscription|"+
			"TestEntitlementDenied|TestFullRefreshUpdateTypeEmitsW|"+
			"TestMalformedRequestsSessionReject|"+
			"TestUnsupportedMDUpdateTypeRejected|TestEntryTypeFilter|"+
			"TestDropSessionPurgesSubscriptions|TestRunConsumesDeltaSource"),
	)
}

// --- Task 18.3.4: drop copy ------------------------------------------------------

func ckP18DropCopy(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/fix/dropcopy.go"),
		gotest(p18Fix, "TestDropCopySessionKind|TestOrderEntryRejector|"+
			"TestDropCopyRouter_FanOutBoundAccounts|"+
			"TestDropCopyRouter_UnresolvableAccountFailsClosed|"+
			"TestDropCopyRouter_Tag1Fallback|"+
			"TestDropCopyRouter_AllocationReportFanOut|"+
			"TestFromApp_DropCopyCannotSubmit|TestCloneMessage_Independence"),
	)
}

// --- Task 18.3.5: FIX 5.0 SP2 derivatives -----------------------------------------

func ckP18Fix50(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/fix/fix50sp2.go"),
		structural(env, "services/internal/fix/fix50sp2.go",
			"5.0", "Derivative"),
	)
}

func ckP18FXTags(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/fix/fix50sp2.go",
			"SecurityType", "SettlementType", "SettlementDate",
			"Currency", "NoPartyIDs", "StrikePrice"),
	)
}

// --- Task 18.3.6: PB drop copy + affirmation ----------------------------------------

func ckP18PBDropCopy(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fix/pb_dropcopy.go",
			"services/internal/fix/affirmation.go",
			"services/internal/db/migrations/037_create_prime_brokerage.up.sql"),
		structural(env, "services/internal/db/migrations/037_create_prime_brokerage.up.sql",
			"prime_brokers", "pb_giveup_trades"),
		gotest(p18Fix, "TestNormalizeAffirmStatus|"+
			"TestHTTPAffirmationExporter|TestAffirmationSync_Apply|"+
			"TestAffirmationMonitor_ScanOnce|TestPG_PrimeBrokerageSchema"),
	)
}

// --- Task 18.3.7: mass quoting -----------------------------------------------------

func ckP18MassQuote(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/fix/quoting.go"),
		structural(env, "services/internal/errs/codes.go",
			"QUOTE_REQUEST_REJECTED"),
		gotest(p18Fix, "TestMassQuoteRejectsNonZeroHoldTime|"+
			"TestMassQuoteRequiresSetID|TestMassQuoteRejectsUnentitled|"+
			"TestMassQuoteRejectsWhenMMPLocked|TestQuoteSetLifecycle|"+
			"TestQuoteCancelScopes|TestAskLegFailureUnwindsBid"),
	)
}

// --- Task 18.3.8: SBE/Aeron binary gateway -------------------------------------------

func ckP18SBE(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fixsbe/codec.go",
			"services/internal/fixsbe/gateway.go",
			"services/internal/fixsbe/transport.go",
			"services/internal/db/migrations/228_fixsbe_sessions.up.sql"),
		gotest(p18FixSBE, "TestNewOrderRoundTrip|TestCancelReplaceRoundTrip|"+
			"TestForwardCompatibility|TestMalformedInputs|"+
			"TestNegotiateRoundTrip|TestGatewayNewOrderMapsOntoSubmit|"+
			"TestGatewayRejectsMalformedOrder|"+
			"TestGatewayEntitlementAndAccountMismatch|"+
			"TestCancelByOrigClOrdID|TestReplaceMapping|"+
			"TestStreamMultipleMessages|TestFrameIntegrity"),
	)
}

// --- Task 18.3.9: entitlement / CoD / throttle -----------------------------------------

func ckP18Entitlement(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fix/throttle.go",
			"services/internal/db/migrations/046_fix_sessions_entitlement.up.sql"),
		structural(env, "services/internal/db/migrations/046_fix_sessions_entitlement.up.sql",
			"account_id", "cancel_on_disconnect", "max_msgs_per_sec"),
		structural(env, "services/internal/errs/codes.go",
			"SESSION_NOT_ENTITLED", "SESSION_THROTTLED"),
		gotest(p18Fix, "TestFromApp_UntitledInstrumentRejected|"+
			"TestFromApp_ThrottledNeverSilentlyDropped|"+
			"TestOnLogout_AbnormalRunsCoD_OrderlyPreserves|"+
			"TestOnLogout_CoDDisabledPreserves|TestThrottle_BurstThenRejects|"+
			"TestThrottle_Refill|TestThrottle_LiveRateChange|"+
			"TestThrottle_DropReleasesBudget|TestThrottle_ZeroCapDefaults|"+
			"TestPGStore_EntitlementUpdateTx"),
	)
}

// --- Task 18.3.10: MM program + MMP ------------------------------------------------------

func ckP18MMProgram(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketmaking/program.go",
			"services/internal/marketmaking/mmp.go",
			"services/internal/marketmaking/compliance.go",
			"services/internal/marketmaking/rebate.go",
			"services/internal/db/migrations/045_market_maker_program.up.sql"),
		structural(env, "services/internal/db/migrations/045_market_maker_program.up.sql",
			"mm_programs", "mm_compliance"),
		structural(env, "services/internal/errs/codes.go",
			"MMP_TRIGGERED", "MMP_LOCKED_OUT"),
		gotest(p18MM, "TestPgProgramRoundTripAndRollup|"+
			"TestEntitledPrefersSpecificOverWide|"+
			"TestSuspendedProgramNotEntitled|TestOtrAllowanceResolution|"+
			"TestEvaluateObligation|TestRollupBreachSuspendsAfterThreshold|"+
			"TestObserveQuoteSamplesBothPrograms|"+
			"TestMMPTriggerMassCancelsAndLocks|TestMMPWindowSlides|"+
			"TestMMPNoProgramNoTrigger|TestAccrueRebateAmountAndIdempotency|"+
			"TestPostAccruedRebatesGroupsAndMarks"),
	)
}

// --- Task 18.3.11: FIXS mTLS + certification ----------------------------------------------

func ckP18Certification(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fix/tls.go",
			"services/internal/fix/certgate.go",
			"services/internal/fix/certification/certification.go",
			"services/internal/fix/certification/pack.go",
			"services/internal/db/migrations/052_fix_certification.up.sql"),
		gotest(p18Cert, "TestGateMatrix|TestRevokeSchema|"+
			"TestRegisterValidation|TestPackRunner"),
	)
}

// --- Task 18.3.12: failover + sequence sync --------------------------------------------------

func ckP18Failover(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/fix/failover.go"),
		gotest(p18Fix, "TestClaimSessionFencing|TestResumeOnLogonFresh|"+
			"TestResumeOnLogonGapAhead|TestResumeOnLogonLowSeq|"+
			"TestResumeOnLogonResetFlag|"+
			"TestRecordInboundAdvancesAndMismatches|"+
			"TestRecordOutboundAdvances|TestFailoverStoreFallsBack|"+
			"TestHandleNewOrderDedup"),
	)
}

// --- Task 18.3.13: allocations --------------------------------------------------------------

func ckP18Allocation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fix/allocation.go",
			"services/internal/db/migrations/226_fix_allocations.up.sql"),
		structural(env, "services/internal/errs/codes.go",
			"ALLOCATION_SUM_MISMATCH", "ALLOCATION_INVALID"),
		gotest(p18Fix, "TestParseAllocationInstruction|"+
			"TestAllocationNew_ManualAccepted|"+
			"TestAllocationNew_OverAllocationRejected|"+
			"TestAllocationNew_UnderAllocationRejected|"+
			"TestAllocationNew_DeclaredQtyMismatch|"+
			"TestAllocationNew_UnknownAccount|TestAllocationNew_UnknownExecRef|"+
			"TestAllocationProRata_ExactConservation|TestAllocationStepOut|"+
			"TestAllocationReplace|TestAllocationCancel|"+
			"TestAllocationReplaceNotAccepted|"+
			"TestAllocationCancelLocked|"+
			"TestAllocationValidationErrorsAreCoded|TestPG_AllocationStore"),
	)
}

// --- Task 18.3.14: smart order routing ----------------------------------------------------------

func ckP18SOR(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/sor/sor.go",
			"services/internal/sor/connector.go",
			"services/internal/sor/tagval.go",
			"services/internal/db/migrations/227_sor_shadow_orders.up.sql"),
		structural(env, "services/internal/errs/codes.go",
			"ROUTING_REJECTED"),
		gotest(p18SOR, "TestLocalWhenLiquid|TestRouteFillLifecycle|"+
			"TestPartialFillState|TestTimeoutWalksToNextVenue|"+
			"TestAllVenuesDeadSORTimeout|TestVenueRejectFailsClosed|"+
			"TestConcurrentRouteGuard|TestReleaseForLocal|TestFillDedup|"+
			"TestLoadVenueConfig"),
	)
}

// --- Task 18.3.15: trading session status --------------------------------------------------------

func ckP18SessionStatus(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/fix/session_status.go"),
		gotest(p18Fix, "TestInstrumentStatusMapping|TestSessionStateMapping|"+
			"TestSubscribeThenBroadcast|TestSymbolScopedSubscription|"+
			"TestSnapshotRequestNoRegistration|TestUnsubscribeStopsBroadcasts|"+
			"TestRequestRejections|TestUnmappedStatesDoNotBroadcast|"+
			"TestSnapshotStateUnavailableHonestReject|"+
			"TestBroadcastVenueOrchestratorSeam|"+
			"TestBroadcastLatencyUnder50ms|"+
			"TestRapidStateFlapOrderedBroadcasts"),
	)
}

// --- Task 18.3.16: dead-man switch -----------------------------------------------------------------

func ckP18DeadMan(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/fix/deadman.go"),
		gotest(p18Fix, "TestDeadMan_ArmAndDisable|TestDeadMan_OutOfRange|"+
			"TestDeadMan_DropCopyRejected|TestDeadMan_UnsupportedRequestType"),
	)
}

// --- Task 18.3.17: SBE transport + drain -------------------------------------------------------------

func ckP18Drain(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fix/drain.go",
			"services/internal/fixsbe/drain.go"),
		gotest(p18Fix, "TestDrainRefusesNewLogons|"+
			"TestDrainSendsNewsThenLogoutAndWaits|"+
			"TestDrainForceClosesStraggler|TestDrainNilRegistryIsNoop|"+
			"TestDrainContextAbort"),
		gotest(p18FixSBE, "TestDrainBehavior|TestDrainBroadcast|"+
			"TestHandshakeHappyPath|TestHandshakeMatrix|"+
			"TestSchemaNegotiationLifecycle|TestClassify|"+
			"TestReadFrameSBE|TestReadFrameTagValue"),
	)
}

// --- Task 18.3.18: gap resolution + CoD recovery -------------------------------------------------------

func ckP18GapRecovery(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/fix/gapfill.go",
			"services/internal/db/migrations/229_orders_cod_exempt.up.sql"),
		structural(env, "services/internal/db/migrations/229_orders_cod_exempt.up.sql",
			"cod_exempt"),
		structural(env, "services/internal/fix/mapfix.go",
			"TagCODExempt"),
		structural(env, "services/internal/orders/store.go",
			"cod_exempt = false"),
		gotest(p18Fix, "TestAssessInbound|TestHeartbeatAbnormal|"+
			"TestPlanResendMixedAdminAndApp|"+
			"TestPlanResendArchiveHoleFailsClosed|"+
			"TestPlanResendAllAdminCollapses|TestPlanResendEmptyRange|"+
			"TestPlanResendRejectsBadBegin|TestMessageBuilders|"+
			"TestCoDPurgesWithinBudget|TestCoDGracefulLogoutPreservesOrders|"+
			"TestCoDDisabledAndDropCopy|TestCoDRateGate|"+
			"TestCoDBudgetBreachSurfaced|TestCoDUnknownSessionFailsClosed|"+
			"TestCoDUnwiredCancellerFailsClosed"),
	)
}
