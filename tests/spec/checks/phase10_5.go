package checks

// Phase-10.5 checkpoint bindings — Console Surface Completion.
//
// The phase closed the console-coverage gap: every mounted route needs a
// real UI surface (or an explicitly-reasoned exclusion), and every new
// surface is exercised by its feature's vitest suite. Bindings therefore
// pair the structural file presence of each shipped surface with the
// feature-local vitest run that proves its behaviour, plus `check:routes`
// for the standing route-coverage gate (Task 10.5.3.27) and Go tests for
// the two backend seams (10.5.3.25 consumer self-heal, 10.5.3.26 recon
// POSITIONS leg).

import (
	"context"

	spec "exchange-testspec/spec"
)

func registerPhase105(r *spec.Registry) {
	// Batch A — safety & platform admin.
	r.Register("P10.5-T10.5.3.1-C1", ckP105SafetyLevers,
		"all safety levers reachable, dual-controlled, audit-written")
	r.Register("P10.5-T10.5.3.2-C1", ckP105FleetMutations,
		"fleet mutations reach backend through context-bound client")
	r.Register("P10.5-T10.5.3.3-C1", ckP105IntegritySurfaces,
		"audit/recon/DLQ surfaces show authoritative state with honest-degraded fallbacks")

	// Batch B — client-facing ops.
	r.Register("P10.5-T10.5.3.4-C1", ckP105Customer360,
		"customer lifecycle is fully operable from one surface")
	r.Register("P10.5-T10.5.3.5-C1", ckP105ClientCompliance,
		"client compliance ops executable with evidence capture")
	r.Register("P10.5-T10.5.3.6-C1", ckP105SurveillanceDesk,
		"surveillance case desk + SAR lifecycle + comms WORM retrieval")

	// Batch C — money movement & post-trade.
	r.Register("P10.5-T10.5.3.7-C1", ckP105FundingOps,
		"funding ops queues enforce review tiers and dual control")
	r.Register("P10.5-T10.5.3.8-C1", ckP105SegregatedFunds,
		"segregated-funds state is inspectable and auditable")
	r.Register("P10.5-T10.5.3.9-C1", ckP105PostTrade,
		"post-trade ops surfaces expose the full state machines")

	// Batch D — market & finance admin.
	r.Register("P10.5-T10.5.3.10-C1", ckP105PricingFinance,
		"pricing/finance admin complete with approval flows")
	r.Register("P10.5-T10.5.3.11-C1", ckP105InstrumentLifecycle,
		"instrument lifecycle console covers halt→delist with dual control")
	r.Register("P10.5-T10.5.3.12-C1", ckP105AlgoGovernance,
		"algo-trading governance surfaces complete")
	r.Register("P10.5-T10.5.3.13-C1", ckP105RegReporting,
		"reg-reporting desk operates the submissions lifecycle")

	// Batch E — governance consoles.
	r.Register("P10.5-T10.5.3.14-C1", ckP105VenueGovernance,
		"venue governance console covers cases/rulebook/admission/CCO reporting")
	r.Register("P10.5-T10.5.3.15-C1", ckP105ConductDora,
		"conduct & DORA consoles complete")
	r.Register("P10.5-T10.5.3.16-C1", ckP105ContentEmergency,
		"content/engagement/emergency-access admin complete")

	// Batch F — user self-service & public surfaces.
	r.Register("P10.5-T10.5.3.17-C1", ckP105AccountSelfService,
		"full account self-service surface")
	r.Register("P10.5-T10.5.3.18-C1", ckP105FundingSelfService,
		"funding self-service covers all rails")
	r.Register("P10.5-T10.5.3.19-C1", ckP105StrategyMarketplace,
		"strategy marketplace user flow complete")
	r.Register("P10.5-T10.5.3.20-C1", ckP105OrderComposition,
		"order composition UI covers every mounted submit endpoint")
	r.Register("P10.5-T10.5.3.21-C1", ckP105MarketAnalytics,
		"market analytics surface complete")
	r.Register("P10.5-T10.5.3.22-C1", ckP105HistoryExport,
		"history + export center complete")
	r.Register("P10.5-T10.5.3.23-C1", ckP105TransparencyStatus,
		"public transparency + status surfaces live")
	r.Register("P10.5-T10.5.3.24-C1", ckP105AuthFlows,
		"auth flows complete")

	// Batch G — backend seams & audit closure.
	r.Register("P10.5-T10.5.3.25-C1", ckP105ConsumerSelfHeal,
		"consumers self-heal across stream provisioning order")
	r.Register("P10.5-T10.5.3.26-C1", ckP105PositionsReconLeg,
		"positions recon has a verifiable engine leg")
	r.Register("P10.5-T10.5.3.27-C1", ckP105RouteGate,
		"route coverage is mechanically enforced")
}

func ckP105SafetyLevers(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-ops/OpsSafetyPage.tsx",
			"frontend/src/features/admin-ops/KillSwitchPanel.tsx",
			"frontend/src/features/admin-ops/CircuitBreakerPanel.tsx",
			"frontend/src/features/admin-ops/MaintenancePanel.tsx",
			"frontend/src/features/admin-ops/DestructiveOpsPanel.tsx",
			"frontend/src/features/admin/DualControlPanel.tsx",
		),
		vitest("src/features/admin-ops"),
	)
}

func ckP105FleetMutations(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-ops/api.ts",
			"frontend/src/features/admin-ops/FlagsPanel.tsx",
			"frontend/src/features/admin-ops/IpSecurityPanel.tsx",
		),
		structural(env, "frontend/src/features/admin-ops/api.ts",
			"X-Admin-Env"),
		vitest("src/features/admin-ops"),
	)
}

func ckP105IntegritySurfaces(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-integrity/IntegrityPage.tsx",
			"frontend/src/features/admin-integrity/AuditIntegrityPanel.tsx",
			"frontend/src/features/admin-integrity/AuditTrailPanel.tsx",
			"frontend/src/features/admin-integrity/ReconPanel.tsx",
			"frontend/src/features/admin-integrity/ArchiveDlqPanel.tsx",
		),
		vitest("src/features/admin-integrity"),
	)
}

func ckP105Customer360(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-crm/Customer360Page.tsx",
			"frontend/src/features/admin-crm/DossierPanel.tsx",
			"frontend/src/features/admin-crm/LifecyclePanel.tsx",
			"frontend/src/features/admin-crm/SupportDeskPanel.tsx",
			"frontend/src/features/admin-crm/KycDeskPanel.tsx",
		),
		vitest("src/features/admin-crm"),
	)
}

func ckP105ClientCompliance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-compliance/CompliancePage.tsx",
			"frontend/src/features/admin-compliance/ScreeningPanel.tsx",
			"frontend/src/features/admin-compliance/TravelRulePanel.tsx",
			"frontend/src/features/admin-compliance/RestrictedListsPanel.tsx",
			"frontend/src/features/admin-compliance/EmployeeDealingPanel.tsx",
			"frontend/src/features/admin-compliance/EnforcementPanel.tsx",
		),
		vitest("src/features/admin-compliance"),
	)
}

func ckP105SurveillanceDesk(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-surveillance/CaseDeskPage.tsx",
			"frontend/src/features/admin-surveillance/SarCtrPanel.tsx",
			"frontend/src/features/admin-surveillance/TuningPanel.tsx",
		),
		vitest("src/features/admin-surveillance"),
	)
}

func ckP105FundingOps(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-funding/FundingOpsPage.tsx",
			"frontend/src/features/admin-funding/DepositOpsPanel.tsx",
			"frontend/src/features/admin-funding/WithdrawalPanel.tsx",
			"frontend/src/features/admin-funding/QuarantinePanel.tsx",
			"frontend/src/features/admin-funding/BankAccountsPanel.tsx",
		),
		vitest("src/features/admin-funding"),
	)
}

func ckP105SegregatedFunds(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-treasury/TreasuryPage.tsx",
			"frontend/src/features/admin-treasury/ClientMoneyPanel.tsx",
			"frontend/src/features/admin-treasury/NostroPanel.tsx",
			"frontend/src/features/admin-treasury/ReconPanel.tsx",
		),
		vitest("src/features/admin-treasury"),
	)
}

func ckP105PostTrade(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-settlement/SettlementPage.tsx",
			"frontend/src/features/admin-settlement/ClsPanel.tsx",
			"frontend/src/features/admin-settlement/ExceptionsPanel.tsx",
			"frontend/src/features/admin-settlement/AllocationsPanel.tsx",
			"frontend/src/features/admin-settlement/NettingPanel.tsx",
			"frontend/src/features/admin-settlement/BackofficePanel.tsx",
		),
		vitest("src/features/admin-settlement"),
	)
}

func ckP105PricingFinance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-finance/FinancePage.tsx",
			"frontend/src/features/admin-finance/FeeSchedulesPanel.tsx",
			"frontend/src/features/admin-finance/TaxReportingPanel.tsx",
			"frontend/src/features/admin-finance/PromosPanel.tsx",
		),
		vitest("src/features/admin-finance"),
	)
}

func ckP105InstrumentLifecycle(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-instruments/InstrumentGovernancePage.tsx",
			"frontend/src/features/admin-instruments/ListingPanel.tsx",
			"frontend/src/features/admin-instruments/SchedulePanel.tsx",
			"frontend/src/features/admin/InstrumentsPanel.tsx",
		),
		vitest("src/features/admin-instruments", "src/features/admin"),
	)
}

func ckP105AlgoGovernance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-marketmaking/MarketMakingPage.tsx",
			"frontend/src/features/admin-marketmaking/MMProgramsPanel.tsx",
			"frontend/src/features/admin-marketmaking/AlgoDeaPanel.tsx",
		),
		vitest("src/features/admin-marketmaking"),
	)
}

func ckP105RegReporting(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-regreport/RegReportingPage.tsx",
			"frontend/src/features/admin-regreport/SubmissionsPanel.tsx",
			"frontend/src/features/admin-regreport/BestExecPanel.tsx",
			"frontend/src/features/admin-regreport/RegimePanel.tsx",
		),
		vitest("src/features/admin-regreport"),
	)
}

func ckP105VenueGovernance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-venue/VenuePage.tsx",
		),
		vitest("src/features/admin-venue"),
	)
}

func ckP105ConductDora(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-conduct/ConductPage.tsx",
		),
		vitest("src/features/admin-conduct"),
	)
}

func ckP105ContentEmergency(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/admin-content/ContentPage.tsx",
		),
		vitest("src/features/admin-content"),
	)
}

func ckP105AccountSelfService(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/settings/SettingsPage.tsx",
			"frontend/src/features/settings/DelegationPanel.tsx",
			"frontend/src/features/settings/TradingPanel.tsx",
			"frontend/src/features/settings/SecurityPanel.tsx",
			"frontend/src/features/settings/ApiKeysPanel.tsx",
		),
		vitest("src/features/settings"),
	)
}

func ckP105FundingSelfService(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/funding/FundingPage.tsx",
			"frontend/src/features/funding/AccountsPanel.tsx",
			"frontend/src/features/funding/DepositPanel.tsx",
			"frontend/src/features/funding/WithdrawalPanel.tsx",
			"frontend/src/features/funding/TransferPanel.tsx",
		),
		vitest("src/features/funding"),
	)
}

func ckP105StrategyMarketplace(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/copy-grid/MarketplacePanel.tsx",
			"frontend/src/features/copy-grid/StrategyBrowser.tsx",
		),
		vitest("src/features/copy-grid"),
	)
}

func ckP105OrderComposition(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/history/CompositeSubmitPanel.tsx",
			"frontend/src/features/advanced-orders/AdvancedOrderPanel.tsx",
		),
		vitest("src/features/history", "src/features/advanced-orders"),
	)
}

func ckP105MarketAnalytics(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/analytics/AnalyticsPage.tsx",
		),
		vitest("src/features/analytics"),
	)
}

func ckP105HistoryExport(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/explorer/HistoryExplorerPage.tsx",
			"frontend/src/features/reports/DownloadCenter.tsx",
			"frontend/src/features/reports/ExportJobsPanel.tsx",
		),
		vitest("src/features/explorer", "src/features/reports"),
	)
}

func ckP105TransparencyStatus(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/transparency/StatusPage.tsx",
			"frontend/src/features/transparency/TransparencyPage.tsx",
			"frontend/src/features/transparency/SecurityPage.tsx",
		),
		vitest("src/features/transparency"),
	)
}

func ckP105AuthFlows(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/auth/VerifyEmailPage.tsx",
			"frontend/src/features/auth/LoginPage.tsx",
			"frontend/src/features/settings/TotpEnrollment.tsx",
			"frontend/src/lib/auth/webauthn.ts",
		),
		structural(env, "frontend/src/features/auth/LoginPage.tsx",
			"passkey"),
		vitest("src/features/auth", "src/features/settings"),
	)
}

func ckP105ConsumerSelfHeal(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/nats/consumer.go",
			"EnsureConsumerRetry"),
		gotest("./internal/nats", "EnsureConsumerRetry"),
	)
}

func ckP105PositionsReconLeg(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/reconciliation/recon_check_positions.go",
			"Wal"),
		gotest("./internal/reconciliation", "PositionsChecker"),
	)
}

func ckP105RouteGate(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/scripts/check-route-coverage.mjs",
			"frontend/scripts/route-coverage-exclusions.mjs",
			"frontend/scripts/render-audit.mjs",
			"frontend/scripts/func-audit.mjs",
		),
		structural(env, "frontend/package.json", "check:routes"),
		structural(env, ".github/workflows/ci.yml", "check:routes"),
		npmScript("check:routes"),
	)
}
