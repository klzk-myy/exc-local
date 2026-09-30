package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-20 Analytics & Reporting checkpoints (Tasks 20.3.1–20.3.16).
// Evidence: ClickHouse DDL under deploy/clickhouse/schema/; CH client
// seam + ETL ingester + Pebble spool under services/internal/analytics
// (ch.go, etl.go, spool.go); tick/OHLCV/P&L/volume stores +
// statements/invoicing/trial-balance/ERP + income + snapshots + costs
// + depreciation + marketing under services/internal/analytics;
// confirmation generation/delivery/MT515 under
// services/internal/reporting; OHLCV candle engine under
// services/internal/marketdata/ohlcv; tax deltas under
// services/internal/tax; REST handlers under services/internal/api;
// daemons under services/cmd/analytics + services/cmd/snapshot_builder;
// gateway wiring under services/cmd/gateway; migrations 049/093.
// Gated legs (EXC_CH_TEST / EXC_PG_TEST / EXC_REDIS_TEST /
// EXC_NATS_TEST) self-skip when the dependency is absent.
const (
	p20Analytics = "./internal/analytics"
	p20Reporting = "./internal/reporting"
	p20Ohlcv     = "./internal/marketdata/ohlcv"
	p20Tax       = "./internal/tax"
	p20API       = "./internal/api"
	p20Snapshot  = "./cmd/snapshot_builder"
	p20CHSchema  = "deploy/clickhouse/schema"
	p20MigDir    = "services/internal/db/migrations"
)

func registerPhase20(r *spec.Registry) {
	r.Register("P20-T20.3.1-C1", ckP20CHFoundation,
		"ClickHouse for tick history + analytics — defined first, validated against spec")
	r.Register("P20-T20.3.2-C1", ckP20TickRetention,
		"tick history retention (90d raw / 5yr aggregates) — defined first, validated against spec")
	r.Register("P20-T20.3.3-C1", ckP20OHLCV,
		"OHLCV at multiple timeframes — defined first, validated against spec")
	r.Register("P20-T20.3.4-C1", ckP20PnL,
		"P&L reporting — defined first, validated against spec")
	r.Register("P20-T20.3.5-C1", ckP20VolumeStats,
		"volume + stats reporting — defined first, validated against spec")
	r.Register("P20-T20.3.6-C1", ckP20Statements,
		"client statements + confirmations (§5.28, §24 #143) — defined first, validated against spec")
	r.Register("P20-T20.3.7-C1", ckP20FinanceReporting,
		"house finance reporting (§16.5, §24 #205) — defined first, validated against spec")
	r.Register("P20-T20.3.8-C1", ckP20ConfirmationDelivery,
		"trade confirmation delivery — defined first, validated against spec")
	r.Register("P20-T20.3.9-C1", ckP20TCA,
		"TCA engine — defined first, validated against spec")
	r.Register("P20-T20.3.10-C1", ckP20TaxReporting,
		"tax calculations expose method, costs, period, and reproducible export (§24 #276) — defined first, validated against spec")
	r.Register("P20-T20.3.11-C1", ckP20IngestionBuffering,
		"ClickHouse ingestion buffering and dead-letter recovery fail closed (§24 #322) — defined first, validated against spec")
	r.Register("P20-T20.3.12-C1", ckP20IncomeLedger,
		"income ledger by type/symbol/time reconciled to statements (§24 #361) — defined first, validated against spec")
	r.Register("P20-T20.3.13-C1", ckP20Snapshots,
		"daily hash-chained account snapshots with history API (§24 #362) — defined first, validated against spec")
	r.Register("P20-T20.3.14-C1", ckP20CostsDisclosure,
		"ex-ante preview and annual ex-post costs statement reconciled to ledger (§24 #374) — defined first, validated against spec")
	r.Register("P20-T20.3.15-C1", ckP20Depreciation,
		"per-position 10%-multiple depreciation notices with episode dedupe (§24 #375) — defined first, validated against spec")
	r.Register("P20-T20.3.16-C1", ckP20MarketingOps,
		"promo inventory and counts-only consent cohorts (§24 #381) — defined first, validated against spec")
}

// --- Task 20.3.1: ClickHouse Schema + NATS→CH ETL ------------------------------

func ckP20CHFoundation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			p20CHSchema+"/000_database.sql",
			p20CHSchema+"/001_ticks.sql",
			p20CHSchema+"/002_trades.sql",
			p20CHSchema+"/003_ohlcv.sql",
			p20CHSchema+"/004_volume_stats.sql",
			p20CHSchema+"/005_account_pnl.sql",
			p20CHSchema+"/006_tca_results.sql",
			p20CHSchema+"/007_income_ledger.sql",
			"services/internal/analytics/ch.go",
			"services/internal/analytics/etl.go",
			"services/internal/analytics/spool.go",
			"services/cmd/analytics/main.go",
			"deploy/clickhouse/RUNBOOK.md"),
		structural(env, "services/internal/analytics/ch.go",
			"func Dial", "type Conn interface"),
		structural(env, "services/internal/analytics/etl.go",
			"func NewIngester", "tableColumns"),
		gotest(p20Analytics,
			"TestShowCreateTable|TestInsertUnknownTableRejected|"+
				"TestScanCoercions|TestCHDedupReplacingMergeTree"),
	)
}

// --- Task 20.3.2: Tick History Retention --------------------------------------

func ckP20TickRetention(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/analytics/ticks.go"),
		structural(env, p20CHSchema+"/001_ticks.sql",
			"TTL ts \\+ INTERVAL 90 DAY"),
		structural(env, p20CHSchema+"/003_ohlcv.sql",
			"TTL bucket \\+ INTERVAL 5 YEAR"),
		structural(env, "services/internal/analytics/ticks.go",
			"type TickStore", "type TickCursor"),
		gotest(p20Analytics,
			"TestTickStoreInsert|TestTickStoreQuery|TestTickRowValuesExactScaling|"+
				"TestCHTickStore"),
		gotest(p20API,
			"TestHistoryTicks|TestHistoryKlines"),
	)
}

// --- Task 20.3.3: OHLCV Projections -------------------------------------------

func ckP20OHLCV(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/ohlcv.go",
			"services/internal/marketdata/ohlcv/engine.go"),
		structural(env, "services/internal/analytics/ohlcv.go",
			"type OHLCVStore", "func NewOHLCVStore", "func OHLCVTableFor"),
		structural(env, "services/cmd/analytics/main.go",
			"ohlcv\\.NewEngine", "CHArchiveSink", "PGStore"),
		structural(env, "services/internal/analytics/consumer.go",
			"OnTradeFill", "strings.Replace"),
		gotest(p20Analytics,
			"TestOHLCVStoreInsert|TestOHLCVStoreQuery|TestOHLCVTableForAllIntervals|"+
				"TestCHArchiveSink|TestCanonicalIntervalSet|TestCHOhlcvStore"),
		gotest(p20Ohlcv,
			"TestBoundaryTradeOpensNewBucket|TestLateTradeGraceThenClosedImmutability|"+
				"TestRunConsumesSourceAndStops|TestEmissionPayloadShapeAndSeq"),
	)
}

// --- Task 20.3.4: P&L Reporting ------------------------------------------------

func ckP20PnL(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/pnl.go",
			"services/internal/api/handlers_analytics.go"),
		structural(env, "services/internal/analytics/pnl.go",
			"type PnLStore", "func NewPnLStore"),
		gotest(p20Analytics,
			"TestPnLUpsert|TestPnLReportAggregationQuery|TestPnLReportErrors|"+
				"TestCHPnLUpsertReport|TestRenderPnLCSV"),
		gotest(p20API,
			"TestAnalyticsPnLJSONReport|TestAnalyticsPnLCSVExport|"+
				"TestAnalyticsPnLPDFExport|TestAnalyticsPnLRejectsForeignAccount|"+
				"TestAnalyticsPnLNilStoreDegraded"),
	)
}

// --- Task 20.3.5: Volume + Stats Reporting -------------------------------------

func ckP20VolumeStats(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/analytics/stats.go"),
		structural(env, "services/internal/analytics/stats.go",
			"type VolumeRow", "type TierTradeRow", "type AccountTierLookup"),
		structural(env, "services/cmd/gateway/adapters.go",
			"pgAccountTierLookup"),
		gotest(p20Analytics,
			"TestVolumeQueryFiltersAndMaps|TestVolumeDayRollupUsesToStartOfDay|"+
				"TestTradesPerTier|TestFillRatesReadCounterRows|TestCHVolumeMV|"+
				"TestCHVolumeStatsRoundTrip"),
		gotest(p20API,
			"TestAnalyticsVolumeHappyPath|TestAnalyticsStatsHappyPath|"+
				"TestAnalyticsStatsDegradedPaths"),
	)
}

// --- Task 20.3.6: Statements + Invoicing ---------------------------------------

func ckP20Statements(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/statements.go",
			"services/internal/analytics/invoicing.go",
			p20MigDir+"/049_client_statements.up.sql",
			p20MigDir+"/049_client_statements.down.sql",
			"services/internal/api/handlers_statements.go"),
		structural(env, p20MigDir+"/049_client_statements.up.sql",
			"client_statements", "trade_confirmations", "fee_invoices"),
		gotest(p20Analytics,
			"TestPgStatementGenerateFetch|TestPgConfirmationGenerateAndAdjust|"+
				"TestPgConfirmationTracker_RoundTrip|TestPgInvoiceSuspendedMM|"+
				"TestParseStatementPeriod|TestRenderStatementCSVFlagsPending|"+
				"TestRenderStatementPDFValid|TestRenderInvoiceDocuments"),
		gotest(p20API,
			"TestAccountStatements|TestAdminInvoicesListAndCSV"),
	)
}

// --- Task 20.3.7: House Finance Reporting ---------------------------------------

func ckP20FinanceReporting(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/trial_balance.go",
			"services/internal/analytics/erp_export.go",
			"services/internal/api/handlers_finance.go"),
		gotest(p20Analytics,
			"TestPgDailyTBJob|TestPgTrialBalanceEmptyDay|TestPgVerifyGLConservation|"+
				"TestPgERPReplayProtection|TestSFTPDropAdapter|TestWebhookAdapter"),
		gotest(p20API,
			"TestAdminTrialBalanceJSONAndCSV|TestAdminFinancePnLValidation|"+
				"TestAdminFinanceBalanceSheet"),
	)
}

// --- Task 20.3.8: Confirmation Delivery -----------------------------------------

func ckP20ConfirmationDelivery(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/reporting/confirmation_service.go",
			"services/internal/reporting/delivery.go",
			"services/internal/reporting/mt515.go",
			"services/internal/reporting/consumer.go",
			"services/internal/reporting/store_pg.go",
			"services/internal/pdfsec/pdfsec.go",
			"services/internal/analytics/doccrypt.go"),
		structural(env, "services/internal/pdfsec/pdfsec.go",
			"/AESV2", "/Filter /Standard", "func EncryptPDF"),
		structural(env, "services/cmd/gateway/main.go",
			"EXC_DOCS_SECRET", "SetDocCipher"),
		gotest("./internal/pdfsec",
			"TestEncryptDecryptRoundTrip|TestEncryptRequiresPassword"),
		gotest(p20Reporting,
			"TestDelivery_|TestScheduler_T1SweepAndDeadLetter|TestMT515Doc_|"+
				"TestOnFill_|TestOnTradeAmended_|TestDispatchDue|"+
				"TestRenderConfirmationHTML_Jurisdictions|"+
				"TestNewConfirmationService_RequiresDeps|TestNextBusinessDay_SkipsWeekend"),
		gotest(p20API, "TestAccountStatementDownload"),
	)
}

// --- Task 20.3.9: TCA Engine -----------------------------------------------------

func ckP20TCA(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/tca_engine.go",
			"services/internal/analytics/tca_report.go",
			"services/internal/api/handlers_tca.go"),
		gotest(p20Analytics,
			"TestTCA_|TestCHTCAReplayDedup|TestCHTCARoundTrip|"+
				"TestRenderRTS28PDF_VenueSection|TestRTS28Job_PersistsRollupAndArchives"),
	)
}

// --- Task 20.3.10: Tax Reporting Deltas ------------------------------------------

func ckP20TaxReporting(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/tax/tax.go",
			"services/internal/tax/koinly.go",
			"services/internal/tax/limiter.go",
			"services/internal/api/tax.go"),
		structural(env, "services/cmd/gateway/main.go",
			"NewRedisDailyLimiter", "TaxReportsPerDay"),
		gotest(p20Tax,
			"TestComputeFIFO|TestComputeLIFO|TestComputeHIFO|TestComputeAvgCost|"+
				"TestComputePreYearLotBasis|TestComputeFees|TestComputeShortRoundTrip|"+
				"TestReportRangeWindow|TestReportFlowsSurface|TestRenderKoinlyCSV|"+
				"TestDailyLimiterInterface|TestRedisDailyLimiter_CapAndIsolation"),
		gotest(p20API,
			"TestTaxReportFromToWindow|TestTaxReportYearPathStillWorks"),
	)
}

// --- Task 20.3.11: Ingestion Buffering + Dead-Letter -------------------------------

func ckP20IngestionBuffering(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/spool.go",
			"services/internal/analytics/etl.go",
			"services/internal/analytics/metrics.go"),
		structural(env, "services/internal/analytics/spool.go",
			"func OpenSpool", "type Spool"),
		structural(env, "deploy/k8s/services/analytics-etl.yaml",
			"/var/spool/exchange", "100Gi", "analytics-etl-spool"),
		gotest(p20Analytics,
			"TestSpoolAppendPeekDelete|TestSpoolEvictsOldest|"+
				"TestSpoolPersistsAcrossReopen|TestDrainOnceGroupsAndDeletes|"+
				"TestDrainStopsOnPartialFailure|TestInsertWithSpool|"+
				"TestCHSpoolDrainLive|TestCHIngestThroughput|TestMetricsExposition"),
	)
}

// --- Task 20.3.12: Income Ledger ---------------------------------------------------

func ckP20IncomeLedger(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/income.go",
			"services/internal/api/handlers_income.go",
			p20CHSchema+"/007_income_ledger.sql"),
		gotest(p20Analytics,
			"TestIncomeStore|TestIncomeWatermark|TestMapIncomeType|"+
				"TestValidIncomeType|TestPgIncomeSyncRowsClassification|"+
				"TestPgReconcileToStatement|TestReconcileToStatementLive|"+
				"TestPollIncomeOnceProjectsSignedRows|TestCHIncomeProjectionShape|"+
				"TestCHIncomeRoundTrip|TestLedgerMovementSignedAmount"),
		gotest(p20API,
			"TestAccountIncome"),
	)
}

// --- Task 20.3.13: Daily Hash-Chained Snapshots -------------------------------------

func ckP20Snapshots(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/snapshots.go",
			"services/cmd/snapshot_builder/main.go",
			"services/internal/api/handlers_snapshots.go",
			p20MigDir+"/093_balance_snapshots.up.sql",
			p20MigDir+"/093_balance_snapshots.down.sql",
			"deploy/k8s/cronjobs/balance-snapshot-builder.yaml"),
		structural(env, p20MigDir+"/093_balance_snapshots.up.sql",
			"balance_snapshots", "prev_hash|hash_chain|snapshot_hash"),
		gotest(p20Analytics,
			"TestPgSnapshotBuildVerifyChain|TestPgSnapshotHistory|"+
				"TestSnapshotHashDeterministic|TestSnapshotNextRun|"+
				"TestSnapshotTargetDay|TestCanonicalPositionsJSON"),
		gotest(p20API, "TestAccountSnapshots"),
	)
}

// --- Task 20.3.14: Costs & Charges Disclosure ---------------------------------------

func ckP20CostsDisclosure(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/costs.go",
			"services/internal/api/handlers_costs.go",
			"services/internal/api/pdfdoc.go"),
		structural(env, "services/cmd/gateway/main.go",
			"CostsDisclosureService"),
		gotest(p20Analytics,
			"TestCostPreview_|TestCostAnnual_"),
		gotest(p20API,
			"TestCostPreview|TestRenderPDFDocStructure"),
	)
}

// --- Task 20.3.15: Leveraged-Position Depreciation Notices --------------------------

func ckP20Depreciation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/depreciation.go",
			"services/cmd/gateway/main.go"),
		structural(env, "services/cmd/gateway/main.go",
			"NewDepreciationService"),
		structural(env, "services/internal/notifications/notifications.go",
			"position_depreciation"),
		gotest(p20Analytics,
			"TestDepreciation_"),
	)
}

// --- Task 20.3.16: Marketing-Ops Reporting -------------------------------------------

func ckP20MarketingOps(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/analytics/marketing.go",
			"services/internal/api/handlers_marketing.go"),
		structural(env, "services/cmd/gateway/main.go",
			"MarketingReportService", "PgConsentCohortStore", "PgPromoInventoryStore"),
		gotest(p20Analytics,
			"TestMarketingReport_"),
		gotest(p20API,
			"TestAdminPromotionsReport"),
	)
}
