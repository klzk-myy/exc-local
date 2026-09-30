package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-21 Compliance & AML checkpoints (Tasks 21.3.1–21.3.28; task .9
// carries two checkpoints). Evidence: services/internal/compliance/*
// (sanctions, screening, monitoring, travel_rule, sar*, aml, gdpr,
// enforcement, dodd_frank, mifid, emir, rts6, basel, venue/, reporting/,
// fx_global_code, assessment_reporter, data_residency, rts27/28_report,
// comms_recording, cases, tax_reporting, crs_xml, fatca_xml,
// quarantine*, employee_dealing, regulatory_change, promotions,
// reporting_values, surveillance_tuning, audit_query, execution_policy),
// services/internal/compliance/reporting/* (submission lifecycle, APA/ARM
// adapters), services/internal/admin/{restricted_list,regulatory_change},
// services/internal/content/promotion_gate.go, order-admission gates in
// internal/orders/service.go, api handler packs handlers_{aml,regreporting,
// p21gov,p21surv,p21privacy,p21venue}.go, migrations 032/033/054/059/060/
// 062/079/080/100/239-251, C++ SanctionsHook (core). Gated legs self-skip
// (EXC_PG_TEST / EXC_REDIS_TEST / EXC_CH_TEST).
const (
	p21Comp    = "./internal/compliance"
	p21CompVen = "./internal/compliance/venue"
	p21CompRep = "./internal/compliance/reporting"
	p21API     = "./internal/api"
	p21Adm     = "./internal/admin"
	p21Content = "./internal/content"
	p21Orders  = "./internal/orders"
	p21Funding = "./internal/funding"
	p21Mig     = "services/internal/db/migrations"
)

func registerPhase21(r *spec.Registry) {
	r.Register("P21-T21.3.1-C1", ckP21Sanctions,
		"sanctions screening OFAC/EU/UN — defined first, validated against spec")
	r.Register("P21-T21.3.2-C1", ckP21TravelRule,
		"FATF travel rule ≥$1,000 — defined first, validated against spec")
	r.Register("P21-T21.3.3-C1", ckP21SAR,
		"SAR generation — defined first, validated against spec")
	r.Register("P21-T21.3.4-C1", ckP21MiFIDTx,
		"MiFID II transaction reporting — defined first, validated against spec")
	r.Register("P21-T21.3.5-C1", ckP21EMIR,
		"EMIR trade reporting — defined first, validated against spec")
	r.Register("P21-T21.3.6-C1", ckP21FinCEN,
		"FinCEN MSB + AML program — defined first, validated against spec")
	r.Register("P21-T21.3.7-C1", ckP21GDPRGeo,
		"GDPR + geo-block — defined first, validated against spec")
	r.Register("P21-T21.3.8-C1", ckP21Enforcement,
		"market-abuse enforcement — defined first, validated against spec")
	r.Register("P21-T21.3.9-C1", ckP21DoddFrank,
		"Dodd-Frank swap reporting to US SDR — defined first, validated against spec")
	r.Register("P21-T21.3.9-C2", ckP21CFTCLimits,
		"CFTC position limits + large trader reporting — defined first, validated against spec")
	r.Register("P21-T21.3.10-C1", ckP21SanctionsHook,
		"SanctionsHook in PreTradeChecker (spec §14.3) — defined first, validated against spec")
	r.Register("P21-T21.3.11-C1", ckP21PEPMonitoring,
		"PEP/adverse-media + ongoing monitoring (§14.3, §24 #149) — defined first, validated against spec")
	r.Register("P21-T21.3.12-C1", ckP21RTS6,
		"RTS 6 cert + DEA + retention (§14.1, §24 #150) — defined first, validated against spec")
	r.Register("P21-T21.3.13-C1", ckP21Basel,
		"Basel III capital/leverage reporting (§14.1, §24 #151) — defined first, validated against spec")
	r.Register("P21-T21.3.14-C1", ckP21EMIRREFIT,
		"EMIR REFIT + CFTC lifecycle/data-quality reporting (§14.1a) — defined first, validated against spec")
	r.Register("P21-T21.3.15-C1", ckP21Venue,
		"regulated-venue governance and rule enforcement (§14.1b) — defined first, validated against spec")
	r.Register("P21-T21.3.16-C1", ckP21APAARM,
		"MiFID II APA/ARM submission adapters (§14.5, §24 #178) — defined first, validated against spec")
	r.Register("P21-T21.3.17-C1", ckP21FXGC,
		"FX Global Code 55-principle self-assessment (§14.6) — defined first, validated against spec")
	r.Register("P21-T21.3.18-C1", ckP21Residency,
		"Jurisdictional data residency enforcement (§14.7, §24 #184) — defined first, validated against spec")
	r.Register("P21-T21.3.19-C1", ckP21RTS2728,
		"MiFID II RTS 27/28 best execution reporting — defined first, validated against spec")
	r.Register("P21-T21.3.20-C1", ckP21Comms,
		"communications recording (§14.8, §24 #203) — defined first, validated against spec")
	r.Register("P21-T21.3.22-C1", ckP21TaxXML,
		"CRS/FATCA tax reporting — defined first, validated against spec")
	r.Register("P21-T21.3.23-C1", ckP21Quarantine,
		"Sanctions service downtime quarantine and ARM/APA resubmission — defined first, validated against spec")
	r.Register("P21-T21.3.24-C1", ckP21EmployeeDealing,
		"employee dealing controls with restricted lists, pre-clearance — defined first, validated against spec")
	r.Register("P21-T21.3.25-C1", ckP21RegChange,
		"regulatory change monitoring with triage SLA and impact assessment — defined first, validated against spec")
	r.Register("P21-T21.3.26-C1", ckP21Promotions,
		"financial promotions pre-approved with bounded expiry — defined first, validated against spec")
	r.Register("P21-T21.3.27-C1", ckP21ReportingValues,
		"tabulated reporting values with outage buffering, per-signal tuning, audit-trail query — defined first, validated against spec")
	r.Register("P21-T21.3.28-C1", ckP21ExecPolicy,
		"published execution policy with consent gate and annual evidence review — defined first, validated against spec")
}

func ckP21Sanctions(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/sanctions.go",
			"services/internal/compliance/sanctions_flags.go",
			"services/internal/compliance/sanctions_refresh.go"),
		structural(env, "services/internal/compliance/sanctions.go",
			"SrcOFACSDN", "AliasOf", "sameListFamily"),
		gotest(p21Comp,
			"TestClassifyFile|TestITSanctionsWithdrawalBlocks|"+
				"TestITSanctionsDepositBlocks|TestITSanctionsCleanCounterpartyPasses|"+
				"TestDevFixtureLoads"),
	)
}

func ckP21TravelRule(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/travel_rule.go",
			p21Mig+"/032_create_travel_rule_records.up.sql"),
		structural(env, "services/internal/compliance/travel_rule.go",
			"EnforceOutbound", "CheckInbound", "SupplyInfo"),
		structural(env, "services/internal/funding/nostro.go",
			"WithTravelRule", "TravelRuleGate"),
		gotest(p21Comp,
			"TestTravelRuleInScope|TestTravelRuleMissingFields|"+
				"TestTravelRuleMergePartyStoredWins|"+
				"TestIntegrationTravelRuleOutbound|TestIntegrationTravelRuleInbound|"+
				"TestIntegrationTravelRuleBelowThreshold"),
	)
}

func ckP21SAR(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/sar.go",
			"services/internal/compliance/sar_signals.go",
			p21Mig+"/033_create_sar_reports.up.sql"),
		structural(env, "services/internal/compliance/sar.go",
			"SAR_DUAL_CONTROL_REQUIRED", "filing_deadline", "DraftFromSignal"),
		gotest(p21Comp,
			"TestSARDraftValidation|TestIntegrationSARLifecycle|"+
				"TestIntegrationSARFromOpenSignals|TestIntegrationHoldEscalateDraftsSAR"),
	)
}

func ckP21MiFIDTx(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/mifid.go",
			"services/internal/compliance/reporting/service.go",
			p21Mig+"/054_regulatory_reporting.up.sql"),
		structural(env, "services/internal/compliance/mifid.go",
			"RTS22|MIFID2|mifid"),
		gotest(p21CompRep,
			"TestPgStore_Lifecycle_EndToEnd|TestRTS22Deadline_T1_2359CET"),
	)
}

func ckP21EMIR(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/compliance/emir.go"),
		structural(env, "services/internal/compliance/emir.go",
			"EMIR"),
		gotest(p21Comp, "TestEMIRReporter|TestEMIRXML_WellFormed"),
	)
}

func ckP21FinCEN(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/compliance/aml.go"),
		structural(env, "services/internal/compliance/aml.go",
			"ScanBusinessDay", "MSB_COMPLIANCE_BREACH", "STRUCTURING"),
		structural(env, p21Mig+"/033_create_sar_reports.up.sql",
			"ctr_reports", "aml_monitoring_events", "aml_program_artifacts"),
		gotest(p21Comp,
			"TestStructuringRule|TestIntegrationAMLScanCTRAndStructuring|"+
				"TestIntegrationAMLScanBelowThreshold|TestIntegrationAMLProgramStatus"),
	)
}

func ckP21GDPRGeo(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/gdpr.go",
			p21Mig+"/243_gdpr_geo.up.sql"),
		structural(env, "services/internal/compliance/gdpr.go",
			"GeoGate|geoGate|EXC_GEO|Geo"),
		gotest(p21Comp,
			"TestGDPRConsentLifecycle|TestGDPRExportAndErasure|"+
				"TestGeoGateBlockAndAllow|TestGeoGateRetailBlockMatrix|"+
				"TestGeoGateResolverErrorFailsClosed|TestCIDRResolverLongestPrefix|"+
				"TestConsentPurposeValidation"),
	)
}

func ckP21Enforcement(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/enforcement.go",
			p21Mig+"/247_market_abuse_enforcement.up.sql"),
		structural(env, "services/internal/compliance/enforcement.go",
			"EnforcementService", "WARN|THROTTLE|RESTRICT|SUSPEND"),
		gotest(p21Comp,
			"TestEnforcementActionVocabulary|TestPGEnforcementWarnDismiss|"+
				"TestPGEnforcementGuards"),
	)
}

func ckP21DoddFrank(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/compliance/dodd_frank.go"),
		structural(env, "services/internal/compliance/dodd_frank.go",
			"DoddFrank|SDR|dodd"),
		gotest(p21Comp, "TestDoddFrankReporter"),
	)
}

func ckP21CFTCLimits(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/compliance/dodd_frank.go",
			"PositionLimit|position_limit|LargeTrader|large_trader|CFTC"),
	)
}

func ckP21SanctionsHook(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/include/risk/SanctionsCache.hpp",
			"core/src/risk/SanctionsCache.cpp"),
		structural(env, "core/src/risk/PreTradeChecker.cpp",
			"SanctionsCache::Verdict"),
		structural(env, "core/include/risk/PreTradeChecker.hpp",
			"bind_sanctions"),
		gtest("test_sanctions",
			"SanctionsVerdict.*:SanctionsPreTrade.*:SanctionsKey.*"),
	)
}

func ckP21PEPMonitoring(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/screening.go",
			"services/internal/compliance/monitoring.go"),
		structural(env, "services/internal/compliance/monitoring.go",
			"ObserveFundingNotification|MonitoringService"),
		gotest(p21Comp,
			"TestPEPKindSeparation|TestScreenOnboardingPEPHitRoutesToReview|"+
				"TestScreenOnboardingSanctionsHit|TestAdverseMediaSeverityRouting|"+
				"TestDeltaRescreenOnlyNewlyAdded|TestStructuringRule"),
	)
}

func ckP21RTS6(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/rts6.go",
			p21Mig+"/241_rts6_algo_dea.up.sql"),
		structural(env, "services/internal/compliance/rts6.go",
			"AssertCertified", "DEALimitsFor"),
		gotest(p21Comp, "TestPGRTS6CertGate|TestPGRTS6DEA"),
	)
}

func ckP21Basel(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/basel.go",
			p21Mig+"/249_basel_reports.up.sql"),
		structural(env, "services/internal/compliance/basel.go",
			"CAPITAL_ADEQUACY_BREACH", "LEVERAGE_RATIO_BREACH", "RunEOD"),
		gotest(p21Comp,
			"TestBaselConvert|TestBaselFloors|TestBaselRunEODKeyShape|"+
				"TestPGBasel"),
	)
}

func ckP21EMIRREFIT(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/submissions_ledger.go",
			"services/internal/compliance/submission_dispatcher.go"),
		structural(env, "services/internal/compliance/reporting/service.go",
			"CORR|supersedes|Lifecycle"),
		gotest(p21CompRep, "TestPgStore_Lifecycle_EndToEnd"),
	)
}

func ckP21Venue(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/venue/venue.go",
			"services/internal/compliance/venue/rulebook.go",
			p21Mig+"/250_venue_governance.up.sql"),
		structural(env, "services/internal/compliance/venue/venue.go",
			"AdmissionGate", "JURISDICTION_UNLICENSED|SweepOverdue"),
		gotest(p21CompVen,
			"TestAdmissionGate_NilServiceFailsClosed|TestCaseTransitions_LifecycleShape"),
	)
}

func ckP21APAARM(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/apa_client.go",
			"services/internal/compliance/arm_client.go",
			p21Mig+"/059_regulatory_submissions.up.sql"),
		gotest(p21Comp,
			"TestAPAClient_Submit|TestARMClient_SubmitXML|"+
				"TestDispatcher_ACK|TestDispatcher_NACKOpensRepair|"+
				"TestDispatcher_UnconfiguredEndpoint_FailClosed|"+
				"TestIngestAck_NACKOpensRepairBreak"),
	)
}

func ckP21FXGC(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/fx_global_code.go",
			"services/internal/compliance/assessment_reporter.go",
			p21Mig+"/060_compliance_assessments.up.sql"),
		structural(env, "services/internal/compliance/fx_global_code.go",
			"Assess", "PublishStatement", "55|principle"),
		gotest(p21Comp,
			"TestFXGCAutomatedProbeSet|TestFXGCThemeBoundaries|"+
				"TestPGFXGC|TestPGExecPol|TestPGRegChange"),
	)
}

func ckP21Residency(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/data_residency.go",
			p21Mig+"/244_data_residency.up.sql"),
		gotest(p21Comp,
			"TestResidencyResolveCountry|TestResidencyAuthorizeAccess|"+
				"TestResidencyReplicationLegality|TestResidencyVerifyPlacement|"+
				"TestResidencyResolveFailClosedWithoutROW"),
	)
}

func ckP21RTS2728(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/rts27_report.go",
			"services/internal/compliance/rts28_report.go",
			p21Mig+"/251_best_execution_reports.up.sql"),
		gotest(p21Comp,
			"TestBuildRTS27Artifact_FullInputs|TestBuildRTS27Artifact_GapsPropagate|"+
				"TestITRTS27Lifecycle|TestITRTS28Lifecycle|TestCHRTS27MaterializeDay"),
	)
}

func ckP21Comms(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/comms_recording.go",
			p21Mig+"/062_comms_recordings.up.sql"),
		gotest(p21Comp,
			"TestCommsChainHashDeterministicAndLinked|TestCommsRecordingLifecycle|"+
				"TestCommsRetentionFloor"),
	)
}

func ckP21TaxXML(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/tax_reporting.go",
			"services/internal/compliance/crs_xml.go",
			"services/internal/compliance/fatca_xml.go",
			p21Mig+"/245_tax_report_runs.up.sql",
			"services/internal/compliance/testdata/crs_golden.xml",
			"services/internal/compliance/testdata/fatca_golden.xml"),
		gotest(p21Comp, "TestTaxXMLCRSGolden|TestTaxXMLFATCAGolden"),
	)
}

func ckP21Quarantine(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/quarantine.go",
			"services/internal/compliance/quarantine_resubmit.go"),
		gotest(p21Comp,
			"TestQuarantinedScreenerEnqueuesAndDelegates|"+
				"TestResubmissionBackoffAndDeadline|TestHandleReplayHitRoutesHold|"+
				"TestScreenOnboardingQuarantinedDefers|"+
				"TestRecordExecution_Quarantine|"+
				"TestRecordExecution_NoEnricherQuarantinesDerivative"),
	)
}

func ckP21EmployeeDealing(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/employee_dealing.go",
			"services/internal/admin/restricted_list.go",
			p21Mig+"/079_employee_dealing.up.sql"),
		structural(env, "services/internal/compliance/employee_dealing.go",
			"EMPLOYEE_DEALING_PRECLEARANCE_REQUIRED|AssertOrderEntry"),
		gotest(p21Comp,
			"TestPGEmployeeDealingGate|TestPGRestrictedWindowGate"),
	)
}

func ckP21RegChange(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/regulatory_change.go",
			"services/internal/admin/regulatory_change.go",
			p21Mig+"/080_regulatory_change.up.sql"),
		structural(env, "services/internal/compliance/regulatory_change.go",
			"TRACKED|TRIAGED|SCOPED|IMPLEMENTED", "MarkImplemented"),
		gotest(p21Comp, "TestPGRegChangeLifecycle|TestRegChangeConstants"),
		gotest(p21Adm,
			"TestBusinessDaysAfter|TestGateRegChangeMutation|TestGateRegChangeRead"),
	)
}

func ckP21Promotions(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/promotions.go",
			"services/internal/content/promotion_gate.go",
			p21Mig+"/246_financial_promotions.up.sql"),
		gotest(p21Comp,
			"TestPromoChecklistAllMandatory|TestPromotionLifecycle"),
		gotest(p21Content,
			"TestGateApprovedUnexpiredRenders|TestGateRefusesExpiredApproval|"+
				"TestGateRefusesNonApproved"),
	)
}

func ckP21ReportingValues(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/reporting_values.go",
			"services/internal/compliance/surveillance_tuning.go",
			"services/internal/compliance/audit_query.go",
			p21Mig+"/242_surveillance_tuning.up.sql"),
		gotest(p21Comp,
			"TestPGTuningLifecycle|TestPGReportingValues|TestPGAuditTrailJoin|"+
				"TestAuditTrailFilterZeroValue"),
	)
}

func ckP21ExecPolicy(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/execution_policy.go",
			p21Mig+"/100_execution_policies.up.sql",
			p21Mig+"/239_account_consents.up.sql"),
		structural(env, "services/internal/compliance/execution_policy.go",
			"CheckConsent", "PolicyConsentGate", "SweepOverdueReview"),
		gotest(p21Comp, "TestPGExecutionPolicyLifecycle"),
	)
}
