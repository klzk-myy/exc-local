package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-24 Backoffice & Settlement checkpoints (Tasks 24.3.1–24.3.21; 21
// total). Evidence: services/internal/backoffice/*.go,
// services/internal/settlement/{cls_pvp,netting,ssi,rail_cutoff_service,
// statement_parser,suspense_service}.go, services/internal/api/
// handlers_{backoffice,backoffice_settlement,backoffice_exceptions,
// client_money,allocations}.go, gateway composition in
// cmd/gateway/{main,adapters}.go, PG migrations 035/044/055/056/057/
// 082/083/084/107/259/260/261/262, route registry in
// internal/gateway/routes_v1.go. Gated legs self-skip.
const (
	p24BO  = "./internal/backoffice"
	p24ST  = "./internal/settlement"
	p24API = "./internal/api"
	p24Mig = "services/internal/db/migrations"
)

func registerPhase24(r *spec.Registry) {
	r.Register("P24-T24.3.1-C1", ckP24NostroAccounts,
		"nostro/vostro account management — defined first, validated against spec")
	r.Register("P24-T24.3.2-C1", ckP24NostroRecon,
		"nostro/vostro reconciliation — defined first, validated against spec")
	r.Register("P24-T24.3.3-C1", ckP24Confirmations,
		"settlement confirmation tracking — defined first, validated against spec")
	r.Register("P24-T24.3.4-C1", ckP24SwiftTracking,
		"SWIFT message tracking — defined first, validated against spec")
	r.Register("P24-T24.3.5-C1", ckP24ComplianceReports,
		"compliance reporting — defined first, validated against spec")
	r.Register("P24-T24.3.6-C1", ckP24SettlementExceptions,
		"failed settlement handling — defined first, validated against spec")
	r.Register("P24-T24.3.7-C1", ckP24PBRecon,
		"PB give-up reconciliation — defined first, validated against spec")
	r.Register("P24-T24.3.8-C1", ckP24ClsPvp,
		"CLS PvP settlement service — defined first, validated against spec")
	r.Register("P24-T24.3.9-C1", ckP24NettingSSI,
		"bilateral netting + SSI (§17.7, §24 #155) — defined first, validated against spec")
	r.Register("P24-T24.3.10-C1", ckP24BunchedAllocations,
		"bunched-order average-price allocation (§17.8, §24 #172) — defined first, validated against spec")
	r.Register("P24-T24.3.11-C1", ckP24ClientMoney,
		"client-money segregation + daily reconciliation (§17.9, §24 #173) — defined first, validated against spec")
	r.Register("P24-T24.3.12-C1", ckP24StatementIngestion,
		"Bank statement ingestion and reconciliation parser (§17.10, §24 #175) — defined first, validated against spec")
	r.Register("P24-T24.3.13-C1", ckP24CSDR,
		"CSDR settlement discipline regime (§17.6 extension, §24 #199) — defined first, validated against spec")
	r.Register("P24-T24.3.14-C1", ckP24Restitution,
		"PB credit restitution on settlement failure (§17.11, §24 #200) — defined first, validated against spec")
	r.Register("P24-T24.3.15-C1", ckP24PostTradeAllocations,
		"post-trade allocation workflow — defined first, validated against spec")
	r.Register("P24-T24.3.16-C1", ckP24CLSQuarantineShortfall,
		"CLS discrepancy quarantine and client-money shortfall top-up — defined first, validated against spec")
	r.Register("P24-T24.3.17-C1", ckP24Treasury,
		"venue treasury, own funds, and contingent capital — defined first, validated against spec")
	r.Register("P24-T24.3.18-C1", ckP24Assurance,
		"independent client-money assurance and segregation certification — defined first, validated against spec")
	r.Register("P24-T24.3.19-C1", ckP24OpsHardening,
		"settlement operations hardening — defined first, validated against spec")
	r.Register("P24-T24.3.20-C1", ckP24RailCutoffs,
		"banking rail cut-off enforcement and value-date roll — defined first, validated against spec")
	r.Register("P24-T24.3.21-C1", ckP24Suspense,
		"unmatched bank-deposit suspense routing and discrepancy quarantine — defined first, validated against spec")
}

// ---------------------------------------------------------------------------

func ckP24NostroAccounts(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/nostro.go",
			"services/internal/api/handlers_backoffice.go"),
		structural(env, "services/internal/backoffice/nostro.go",
			"NewNostroService", "PostMovement", "NewPgNostroStore"),
		structural(env, "services/internal/api/handlers_backoffice.go",
			"AdminNostroAccountCreate", "AdminNostroAccountList"),
		gotest(p24BO,
			"TestCreateAccount_NostroAndVostro|TestCreateAccount_Validation|"+
				"TestCreateAccount_DuplicateRejected|TestListAccounts_Filters|"+
				"TestPostMovement_CreditAndDebit|TestPostMovement_IdempotentReplay|"+
				"TestPostMovement_OverdrawnAlerts|TestPostMovement_MissingRows|"+
				"TestPostMovementForInstruction_NoMovementIsNoop|TestPostPendingMovements_Drains"),
	)
}

func ckP24NostroRecon(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/reconciliation.go",
			p24Mig+"/261_backoffice_nostro_recon.up.sql"),
		structural(env, "services/internal/backoffice/reconciliation.go",
			"NewNostroReconService", "NostroReconBreak", "NostroStatementSource"),
		structural(env, "services/internal/api/handlers_backoffice.go",
			"AdminNostroReconciliation", "AdminNostroReconRun", "AdminNostroBreakResolve"),
		gotest(p24BO,
			"TestNostroThreshold_Rules|TestReconDaily_CleanRun|"+
				"TestReconDaily_AutoResolvePostsPendingMovement|"+
				"TestReconDaily_BreakCategories|TestReconDaily_MissingConfirmationBreak|"+
				"TestReconDaily_ThresholdBreachFlag|TestReconDaily_StatementSourcePolls|"+
				"TestReconReport_LatestRunAndBreaks|TestReconBreak_Workflow|"+
				"TestReconService_FailClosedCtor|TestPgReconStore_Integration"),
	)
}

func ckP24Confirmations(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/confirmation.go"),
		structural(env, "services/internal/backoffice/confirmation.go",
			"NewConfirmationService", "SettlementConfirmer", "SwiftConfirmation"),
		structural(env, "services/internal/api/handlers_backoffice.go",
			"AdminSettlementConfirmation"),
		structural(env, "services/cmd/gateway/adapters.go",
			"boSettlementConfirmer", "ConfirmSettlement"),
		gotest(p24BO,
			"TestConfirmation_MT910_SettlesAndPosts|TestConfirmation_MT900_DebitPath|"+
				"TestConfirmation_ReplayIsIdempotent|TestConfirmation_Validation|"+
				"TestConfirmation_UnknownRef|TestConfirmation_CurrencyMismatch|"+
				"TestSweepOverdue_TwoBusinessDays|TestSweepOverdue_HolidayCalendarWired|"+
				"TestConfirmationService_FailClosedCtor|TestPgConfirmationStore_Integration"),
	)
}

func ckP24SwiftTracking(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/swift_tracking.go",
			p24Mig+"/035_create_swift_messages.up.sql"),
		structural(env, "services/internal/backoffice/swift_tracking.go",
			"NewSwiftTracker", "SwiftRecorder", "SwiftMessage"),
		structural(env, "services/internal/api/handlers_backoffice.go",
			"AdminSwiftMessages"),
		gotest(p24BO,
			"TestSwiftRecord_Validation|TestSwiftQuery_Filters|"+
				"TestSwiftStore_NoMutationSurface|TestPgSwiftTracker_Integration"),
	)
}

func ckP24ComplianceReports(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/compliance_reporting.go"),
		structural(env, "services/internal/backoffice/compliance_reporting.go",
			"NewComplianceReportService", "ComplianceExport", "MonthlySummary"),
		structural(env, "services/internal/api/handlers_backoffice.go",
			"AdminComplianceReport"),
		gotest(p24BO,
			"TestReport_MiFID2|TestReport_EMIR|TestReport_FinCEN_CTR_DateFilter|"+
				"TestReport_FinCEN_SAR|TestReport_Basel3|TestReport_MonthlySummary|"+
				"TestReport_NilSourcesDegrade|TestReport_UnknownTypeRejected"),
	)
}

func ckP24SettlementExceptions(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/exceptions.go",
			"services/internal/api/handlers_backoffice_exceptions.go",
			p24Mig+"/260_settlement_ops.up.sql"),
		structural(env, "services/internal/backoffice/exceptions.go",
			"NewExceptionService", "SettlementException", "NewPgxExceptionStore"),
		structural(env, "services/internal/api/handlers_backoffice_exceptions.go",
			"AdminSettlementExceptionResolve", "AdminSettlementExceptionGet"),
		gotest(p24BO,
			"TestException_DetectFailureFlagsLegAndOpensException|"+
				"TestException_DetectOnSettledLegConflicts|"+
				"TestException_InvestigateAssigns|TestException_ResolveRetryReArmsLeg|"+
				"TestException_ResolveReversalReturnsFunds|"+
				"TestException_ResolveTerminalConflicts|"+
				"TestException_RequestResolutionRequiresDualQueue|"+
				"TestException_RequestResolutionSubmitsFinanceOpsDual|"+
				"TestException_RequestResolutionRejectsTerminalAndWriteOff|"+
				"TestException_NilStoreFailsClosed"),
	)
}

func ckP24PBRecon(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/pb_reconciliation.go"),
		structural(env, "services/internal/backoffice/pb_reconciliation.go",
			"NewPBReconService", "AffirmationFeed", "NewPgxReconStore"),
		structural(env, "services/internal/api/handlers_backoffice_exceptions.go",
			"AdminPBReconciliation"),
		gotest(p24BO,
			"TestPBRecon_AutoMatchAffirms|"+
				"TestPBRecon_RateMismatchBeyondToleranceDisputes|"+
				"TestPBRecon_QuantityMismatchAndMissingTicket|TestPBRecon_PairMismatch|"+
				"TestPBRecon_UnaffirmedTimeoutAndMissingAtPB|"+
				"TestPBRecon_MissingLocallyAndKPIAlert|TestPBRecon_IdempotentBreakInsert|"+
				"TestPBRecon_AssignAndResolveBreak|TestPBRecon_AffirmMovesCollateralAtomically|"+
				"TestPBRecon_InsufficientMarginAbortsClean|TestPBRecon_ReportAggregates|"+
				"TestPBRecon_NilDepsFailClosed"),
	)
}

func ckP24ClsPvp(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/settlement/cls_pvp.go",
			p24Mig+"/259_cls_pvp.up.sql"),
		structural(env, "services/internal/settlement/cls_pvp.go",
			"NewClsPvpService", "NewPgxClsStore", "ClsPvpOptions"),
		structural(env, "services/internal/api/handlers_backoffice_settlement.go",
			"AdminClsSubmit", "AdminClsDispatch", "AdminClsAmend",
			"AdminClsRescind", "AdminClsPayIn", "AdminClsFinality", "AdminClsStatus"),
		gotest(p24ST,
			"TestClsSubmitDispatchMatched|TestClsNilMemberFailsClosed|"+
				"TestClsMissingRefDataFailsClosed|TestClsIneligibleWaterfall|"+
				"TestClsControlledGrossBreachAlerts|TestClsFinalityRequiresAuthentication|"+
				"TestClsMemberStatusUnmatchedOpensBreak"),
	)
}

func ckP24NettingSSI(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/settlement/netting.go",
			"services/internal/settlement/ssi.go",
			p24Mig+"/044_settlement_netting.up.sql"),
		structural(env, "services/internal/settlement/netting.go",
			"NewNettingService", "NewPgxNettingStore"),
		structural(env, "services/internal/settlement/ssi.go",
			"NewSsiService", "NewPgxSsiStore"),
		structural(env, "services/internal/api/handlers_backoffice_settlement.go",
			"AdminSsiRegister", "AdminSsiList", "AdminSsiRevoke",
			"AdminNettingRun", "AdminNettingBatches", "AdminNettingBatchLines",
			"AdminNettingDispatch", "AdminNettingSettle", "AdminNettingBust"),
		gotest(p24ST,
			"TestSsiRegisterVerified|TestSsiRejectsUnverifiedBeneficiary|"+
				"TestSsiRevokeIdempotent|TestNettingRunAggregates|"+
				"TestNettingClsExclusion|TestNettingAgreementRequired|"+
				"TestNettingDispatchCutoffGate|TestNettingBustReopens"),
	)
}

func ckP24BunchedAllocations(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/allocations.go",
			"services/internal/backoffice/allocation_engine.go",
			"services/internal/api/handlers_allocations.go",
			p24Mig+"/055_trade_allocations.up.sql"),
		structural(env, "services/internal/backoffice/allocation_engine.go",
			"NewEngine", "NewPgAllocStore", "EscalateResult"),
		structural(env, "services/internal/api/handlers_allocations.go",
			"AllocationsCreate", "AdminAllocationGroupCreate",
			"AdminAllocationAllocate", "AdminAllocationClaim",
			"AdminAllocationReject", "AdminAllocationCorrect",
			"AdminAllocationEscalate"),
		gotest(p24BO,
			"TestVWAP_WeightedAndResidual|TestDistribute_ManualPartialAndReject|"+
				"TestDistribute_ProRataEqualRuleBased_Conservation|"+
				"TestSpreadFills_ConservationPerFill|TestCreateGroup_CapacityMixBlocked|"+
				"TestAttachFills_Validation|"+
				"TestAllocate_FullLifecycle_ConservationAndRemainder|"+
				"TestAllocate_IneligibleBeneficiaryAfterExecution|"+
				"TestSettlementLock_AndDualControlCorrect|TestSubmitBlock_REST_HappyAndForbidden|"+
				"TestSubmitBlock_RuleBasedPercentages|TestAmendAllocation_PartialSettlementGate|"+
				"TestPgAllocations_Integration"),
	)
}

func ckP24ClientMoney(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/client_money.go",
			"services/internal/backoffice/shortfall.go",
			p24Mig+"/056_client_money.up.sql"),
		structural(env, "services/internal/backoffice/client_money.go",
			"NewClientMoneyService", "NewPgStore", "PgJournalPoster"),
		structural(env, "services/cmd/gateway/adapters.go",
			"cmFundSource", "cmNBPSource", "cmStatementSource", "cmSuspension"),
		gotest(p24BO,
			"TestClassifyAccount_StructuralGLCrossCheck|TestRecordBankReview_AcknowledgementPass|"+
				"TestReceipts_UnidentifiedThenAllocated|"+
				"TestDailyReconciliation_BalancedAndSignedOff|"+
				"TestDailyReconciliation_ExternalLeg|TestGuardJournal_ProhibitsHouseUse|"+
				"TestSweepDeadlines_EscalationAndTier4|TestExportPoolingPackage|"+
				"TestEvaluateSegregation_RealtimeShortfall|TestConstruction_NilDepsFailClosed"),
	)
}

func ckP24StatementIngestion(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/statement_parser.go",
			"services/internal/backoffice/camt053_parser.go",
			"services/internal/settlement/statement_parser.go",
			p24Mig+"/057_bank_statements.up.sql"),
		structural(env, "services/internal/backoffice/statement_parser.go",
			"StatementParsers"),
		structural(env, "services/internal/settlement/statement_parser.go",
			"NewStatementIngestionService", "NewPgxStatementStore"),
		structural(env, "services/internal/api/handlers_backoffice_settlement.go",
			"AdminIngestStatement", "AdminListStatements", "AdminStatementEntries"),
		gotest(p24BO,
			"TestMT940Parse|TestMT940MissingMandatoryTagFailsClosed|"+
				"TestMT942Parse|TestCamt053Parse|"+
				"TestCamt053RejectsWrongNamespace|TestCamt053MalformedFailsClosed"),
		gotest(p24ST,
			"TestStatementIngestChecksumDedup|TestStatementIngestChecksumMismatch|"+
				"TestStatementIngestAccountMismatch|TestStatementMatchPriorities|"+
				"TestStatementMissingPaymentBreak"),
	)
}

func ckP24CSDR(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/csdr_discipline.go",
			"services/internal/backoffice/buyin.go",
			p24Mig+"/084_settlement_penalties.up.sql"),
		structural(env, "services/internal/backoffice/csdr_discipline.go",
			"NewCSDRService", "NewPgxFailStore", "NewPgxLegClassifier"),
		structural(env, "services/internal/backoffice/buyin.go",
			"NewBuyInService", "MarketPricer"),
		gotest(p24BO,
			"TestCSDR_DetectFailsFlagsAndClassifies|TestCSDR_NilStoreFailsClosed|"+
				"TestCSDR_PenaltiesBilateralCSDRScopeOnly|TestCSDR_SettledLegAutoResolves|"+
				"TestCSDR_Reports|TestBuyIn_NotifyAtISD4ExecuteAtISD7|"+
				"TestBuyIn_NilDepsFailClosed"),
	)
}

func ckP24Restitution(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/credit_restitution.go",
			"services/internal/risk/pb_credit_adjuster.go"),
		structural(env, "services/internal/backoffice/credit_restitution.go",
			"NewRestitutionService", "NewPgxRestitutionStore", "CreditAdjuster"),
		structural(env, "services/internal/risk/pb_credit_adjuster.go",
			"RestituteInTx", "RestitutionInput"),
		structural(env, "services/cmd/gateway/main.go",
			"NewRestitutionService", "RestituteInTx"),
		gotest(p24BO,
			"TestRestitution_AppliesCountersJournalAndDeliveries|"+
				"TestRestitution_ReplayIsIdempotent|"+
				"TestRestitution_BlockedWhenReplacedOrAllocated|"+
				"TestRestitution_RequiresClientScope|"+
				"TestRestitution_ConstructionFailsClosed"),
	)
}

func ckP24PostTradeAllocations(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/allocation_engine.go",
			"services/internal/api/handlers_allocations.go"),
		structural(env, "services/internal/backoffice/allocation_engine.go",
			"ConfirmResult", "FundReportMeta", "EscalateResult", "TradeResolver"),
		structural(env, "services/internal/api/handlers_allocations.go",
			"AdminAllocationClaim", "AdminAllocationReject",
			"AdminAllocationSubmit", "AdminAllocationCancel"),
		gotest(p24BO,
			"TestConfirmFund_SettlementLegsGLAndReport|TestConfirmFund_SellSide|"+
				"TestEscalateUnallocated_T0EOD|"+
				"TestFIXIngest_CommittedReplacedCancelled|"+
				"TestFIXIngest_LockedGroupRefusesReplace"),
		gotest(p24API,
			"TestAdminAllocationLifecycle|TestAdminAllocationCorrect_DualControl|"+
				"TestRegisterAllocationCorrectExecutor|TestAllocationsCreate|"+
				"TestAdminAllocationHandlers_AuthAndIDs|TestAdminAllocationGroupCreate"),
	)
}

func ckP24CLSQuarantineShortfall(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/shortfall.go",
			"services/internal/backoffice/client_money.go"),
		structural(env, "services/internal/backoffice/shortfall.go",
			"Quarantine", "SegregationStatus"),
		gotest(p24BO,
			"TestShortfall_Tier1CoversFully|TestShortfall_Tier2PendingAndDualControl|"+
				"TestShortfall_ExceedsTier2_RaisesTier3Notice|"+
				"TestShortfall_BlocksMovements_503|TestRemoveExcess_DualControl|"+
				"TestStressTest_AdequateAndBreach|"+
				"TestCLSQuarantine_BlocksOutboundUntilAffirmed"),
	)
}

func ckP24Treasury(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/treasury.go",
			"services/internal/backoffice/contingent_capital.go",
			p24Mig+"/082_treasury.up.sql"),
		structural(env, "services/internal/backoffice/treasury.go",
			"NewTreasuryService", "OwnFunds", "StressedOutflowSource"),
		structural(env, "services/internal/backoffice/contingent_capital.go",
			"Commitment"),
		structural(env, "services/internal/api/handlers_client_money.go",
			"AdminTreasuryOwnFunds", "AdminContingentCapitalList",
			"AdminContingentCapitalCreate"),
		structural(env, "services/cmd/gateway/adapters.go",
			"pgStressedOutflows"),
		gotest(p24BO,
			"TestOwnFunds_LedgerAndReconciliation|TestCommitments_WaterfallAndBackstop|"+
				"TestInsurancePolicy_ExpiryAlert|TestLiquidityBreach_FreezesOutflowsAndLP|"+
				"TestAdmissionGate_FundedOnly|TestTreasury_RoleEnforcement"),
	)
}

func ckP24Assurance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/client_money_audit.go",
			"services/internal/backoffice/segregation_cert.go",
			p24Mig+"/083_client_money_assurance.up.sql"),
		structural(env, "services/internal/backoffice/client_money_audit.go",
			"NewAssuranceService", "AuditorGrant", "ClientMoneyAudit"),
		structural(env, "services/internal/backoffice/segregation_cert.go",
			"EvidencePack", "SegregationCertification"),
		structural(env, "services/internal/api/handlers_client_money.go",
			"AdminClientMoneyAudits", "AdminClientMoneyAuditCreate",
			"AdminClientMoneyEvidencePack", "AdminClientMoneyCertificationList",
			"AdminClientMoneyCertificationCreate"),
		gotest(p24BO,
			"TestAudit_RegisterLifecycleAndLogs|"+
				"TestEvidencePack_SystemAssembledAndHashed|"+
				"TestCertification_DualControlAndReleaseGate|"+
				"TestExternalAuditor_TimeBoundedDualControlledAudited|"+
				"TestAssurance_NilDepsFailClosed"),
	)
}

func ckP24OpsHardening(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/backoffice/ops_hardening.go",
			p24Mig+"/260_settlement_ops.up.sql"),
		structural(env, "services/internal/backoffice/ops_hardening.go",
			"NewOpsService", "NewPgxOpsStore", "SweepAging",
			"EvaluateNostroThresholds", "CLSPayInMonitor", "ProcessFailover",
			"MonitorHerstatt", "WithPricer", "WithCutoffs"),
		structural(env, "services/cmd/gateway/main.go",
			"settlementOpsSvc", "SweepAging", "MonitorHerstatt"),
		gotest(p24BO,
			"TestOps_SweepAgingBuckets|TestOps_WriteOffAuthorityMatrix|"+
				"TestOps_RequestWriteOffValidationAndDual|"+
				"TestOps_ApplyWriteOffJournalsAndClosesTarget|"+
				"TestOps_NostroThresholdBreach|TestOps_CheckCutoffLatePayment|"+
				"TestOps_CLSPrefundDeadlineIsTMinus1|TestOps_CLSPayInFundingAndLadder|"+
				"TestOps_CLSPayInLadderEscalatesToFailed|"+
				"TestOps_CLSMemberOutageArmsFallback|TestOps_FXCloseoutEconomics|"+
				"TestOps_FXCloseoutGuards|TestOps_QueuePaymentDuplicateGuard|"+
				"TestOps_RailFailoverLadderAndExhaustion|"+
				"TestOps_RailFailoverDispatchSuccess|TestOps_HerstattBreachAndSettle|"+
				"TestOps_LPDefaultLadder|TestOps_NilStoreFailsClosed|"+
				"TestPgSettlementOpsSchema_Replay|TestPgSettlementOps_FailedLegScan"),
	)
}

func ckP24RailCutoffs(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/settlement/rail_cutoff_service.go",
			p24Mig+"/107_banking_rail_schedules.up.sql"),
		structural(env, "services/internal/settlement/rail_cutoff_service.go",
			"NewRailCutoffService", "NewPgxRailScheduleStore", "Evaluate"),
		structural(env, "services/internal/api/handlers_backoffice_settlement.go",
			"AdminRailSchedules", "AdminRailEvaluate", "AdminRollInstruction"),
		gotest(p24ST,
			"TestRailCutoffTimezoneAware|TestRailCutoffWeekendRoll|"+
				"TestRailCutoffUnscheduledFailsClosed|"+
				"TestRailCutoffMalformedRowFailsReload|"+
				"TestRailCutoffEnforceSameDay|TestGenerateInstructionsQueuedPastCutoff"),
	)
}

func ckP24Suspense(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/settlement/suspense_service.go",
			p24Mig+"/262_statement_exception_links.up.sql"),
		structural(env, "services/internal/settlement/suspense_service.go",
			"NewSuspenseService", "DepositScreener"),
		structural(env, "services/internal/api/handlers_backoffice_settlement.go",
			"AdminSuspenseRoute", "AdminSuspenseResolve"),
		structural(env, "services/cmd/gateway/adapters.go",
			"suspenseGuard", "DepositGuard"),
		gotest(p24ST,
			"TestSuspenseNilGuardFailsClosed|TestSuspenseRouteUnmatchedCredit|"+
				"TestSuspenseResolvePassthrough|TestStatementUnmatchedCreditRoutesSuspense"),
	)
}
