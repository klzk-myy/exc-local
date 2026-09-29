package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-14 extended-features checkpoints — account lifecycle & compliance
// cluster (Tasks 14.3.9–14.3.12). Sibling Phase-14 clusters (KYC lifecycle,
// auto-halt, testnet, PITR, product profiles, …) append their registrations
// to registerPhase14 as they land.
//
// Bindings are PG-gated integration tests (enabled by EXC_PG_TEST=1 inside
// RunGoTest) plus structural evidence for the irreversibility/ownership
// invariants no test alone can prove:
//   - cooling_off_periods has no UPDATE/DELETE/shorten path (irrevocable);
//   - no CLOSED → any-status reopen path exists;
//   - the Phase-05 webhook Dispatcher retains POST/sign/retry/dead-letter
//     ownership (no second delivery worker).
const (
	p14Acct  = "./internal/accounts"
	p14API   = "./internal/api"
	p14Ord   = "./internal/orders"
	p14Comp  = "./internal/compliance"
	p14Hooks = "./internal/webhooks"
)

func registerPhase14(r *spec.Registry) {
	r.Register("P14-T14.3.1-C1", ckP14OCO, "OCO atomic cancel — engine link, WAL recovery, sibling cancel")
	r.Register("P14-T14.3.8-C1", ckP14PAMM, "PAMM/MAM & Copy Trading Framework — pro-rata engine, internal ledger taxonomy")
	r.Register("P14-T14.3.14-C1", ckP14CopyProduct, "copy discovery, safety-scaled follows and HWM profit-share over the PAMM engine (§24 #372)")
	// Sibling-cluster registrations (tasks 14.3.2–14.3.7) — bound to the
	// evidence those landings already shipped; kept in one file because a
	// ticked-but-unregistered checkpoint is a CI `missing` failure.
	r.Register("P14-T14.3.2-C1", ckP14AutoHalt, "auto-halt on anomaly — canonical breaker delegation + audit")
	r.Register("P14-T14.3.3-C1", ckP14Testnet, "testnet with reset — non-production label, simulated funding")
	r.Register("P14-T14.3.4-C1", ckP14KYCLifecycle, "KYC lifecycle T0/T1/T2/institutional — tiered limits, approve/reject")
	r.Register("P14-T14.3.4-C2", ckP14KYCReverify, "re-verification 12mo/24mo with auto-downgrade")
	r.Register("P14-T14.3.5-C1", ckP14PITR, "PostgreSQL PITR RPO ≤ 15s / RTO ≤ 5min")
	r.Register("P14-T14.3.6-C1", ckP14OpsHardening, "rate-limit/saturation instrumentation + connection pooling")
	r.Register("P14-T14.3.7-C1", ckP14Categorization, "MiFID II client categorization + appropriateness gate")

	// Account lifecycle & compliance cluster (tasks 14.3.9–14.3.12).
	r.Register("P14-T14.3.9-C1", ckP14Closure, "account closure & offboarding (§12.5, §24 #204)")
	r.Register("P14-T14.3.10-C1", ckP14Holds, "Compliance Hold Workflow (§24 #219)")
	r.Register("P14-T14.3.11-C1", ckP14CoolingOff, "cooling-off is irrevocable for its term and blocks leveraged trading (§24 #273)")
	r.Register("P14-T14.3.12-C1", ckP14CoolingOffEnforcement, "Cooling-off invariant enforcement and webhook delivery retries (§24 #315)")

	// Product governance cluster (tasks 14.3.13/14.3.15/14.3.16).
	r.Register("P14-T14.3.13-C1", ckP14ProductProfiles,
		"account product profiles — pricing/scope/denomination, guarded switching (§24 #369)")
	r.Register("P14-T14.3.15-C1", ckP14SwapFree,
		"swap-free verification lifecycle — prospective enforcement + abuse guard (§24 #373)")
	r.Register("P14-T14.3.16-C1", ckP14TargetMarket,
		"per-profile retail target markets — dual-gate admission + ≤12mo review (§24 #376)")
}

func ckP14AutoHalt(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/217_auto_halt_events.up.sql",
			"services/internal/db/migrations/217_auto_halt_events.down.sql"),
		structural(env, "services/internal/risk/auto_halt.go",
			"HALTED", "RESUMED", "EventTradingHalt"),
		gotest("./internal/risk", "TestAutoHalt"),
	)
}

func ckP14Testnet(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/k8s/testnet/00-namespace.yaml",
			"deploy/k8s/testnet/01-configmap.yaml"),
		structural(env, "services/internal/testenv/testenv.go",
			"testnet", "ErrDisabled"),
		gotest("./internal/testenv", "TestEnabledFailClosed|"+
			"TestResetGatesBeforeStore|TestResetCooldown|"+
			"TestIntegrationResetAccount|"+
			"TestIntegrationSeedAndSimulatedFunding|TestIntegrationResetTo"),
	)
}

func ckP14KYCLifecycle(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/compliance/kyc_lifecycle.go",
			"DecideSubmissionTx", "reverify_due_at"),
		gotest(p14Comp, "TestApprove_TierAssignedAndNotified|"+
			"TestApprove_FailClosed|"+
			"TestReject_ReasonRequiredAndNotified"),
	)
}

func ckP14KYCReverify(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/compliance/kyc_lifecycle.go",
			"SweepReverify", "kyc_tier_downgraded"),
		gotest(p14Comp, "TestSweepReverify_DowngradesAlertsNotifies|"+
			"TestSweepReverify_IdempotentSkip|"+
			"TestSweepReverify_RowFailureContinues|"+
			"TestApprove_InstitutionalResolves24Months"),
	)
}

func ckP14PITR(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/postgres/pitr_smoke.sh",
			"deploy/postgres/wal_archive.sh",
			"deploy/crons/pg-base-backup.sh",
			"docs/runbooks/pitr-monthly-drill.md"),
		structural(env, "deploy/postgres/pitr_smoke.sh",
			"archive_command", "restore_command"),
		structural(env, "deploy/postgres/postgresql.conf",
			"archive_timeout = 15"),
	)
}

func ckP14OpsHardening(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/pgbouncer/pgbouncer.ini",
			"deploy/prometheus/rules/exchange-alerts.yml"),
		structural(env, "deploy/prometheus/rules/exchange-alerts.yml",
			"RateLimitUtilizationHigh",
			"exchange_rate_limit_utilization_over80_total"),
		structural(env, "deploy/postgres/postgresql.conf",
			"log_min_duration_statement = 100ms"),
		pyscript(env, "scripts/ci/check_alert_rules.py"),
		pyscript(env, "scripts/ci/check_runbooks.py"),
	)
}

func ckP14Categorization(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/042_client_categorization.up.sql",
			"services/internal/db/migrations/042_client_categorization.down.sql"),
		structural(env, "services/internal/compliance/categorization.go",
			"client_category", "ELIGIBLE_COUNTERPARTY"),
		structural(env, "services/internal/orders/service.go",
			"AppropriatenessGate"),
		gotest(p14Comp, "TestAppropriateness_"),
		gotest(p14Ord, "TestProductGate_"),
	)
}

func ckP14Closure(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/063_account_closures.up.sql",
			"services/internal/db/migrations/063_account_closures.down.sql"),
		structural(env, "services/internal/accounts/closure.go",
			"ACCOUNT_CLOSE_BLOCKED", "'CLOSED'", "sweep_refs",
			"RevokeAllExcept", "RevokeAllKeys"),
		// No reopen path: nothing may transition an account out of CLOSED.
		structuralLacks(env, "services/internal/accounts/closure.go",
			"status != 'CLOSED'"),
		structural(env, "services/internal/api/handlers_offboard.go",
			"RequireTwoFactor", "ClosureService"),
		gotest(p14Acct, "TestIntegrationClosureClientPath|"+
			"TestIntegrationClosureBlocked|TestIntegrationClosureSweep|"+
			"TestIntegrationClosureForced|TestIntegrationClosureGuards"),
	)
}

func ckP14Holds(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/215_compliance_holds.up.sql",
			"services/internal/db/migrations/215_compliance_holds.down.sql"),
		structural(env, "services/internal/compliance/hold_workflow.go",
			"PlaceHold", "'FROZEN'", "sla_deadline",
			"HOLD_CANCEL_FAILED", "HOLD_SLA_BREACHED",
			"HoldStatusEscalatedSAR", "COMPLIANCE_HOLD"),
		gotest(p14Comp, "TestIntegrationHoldPlaceFreezesAccount|"+
			"TestIntegrationHoldSanctionsSLAAndStack|"+
			"TestIntegrationHoldRelease|TestIntegrationHoldRoleGate|"+
			"TestIntegrationHoldEscalateSAR|"+
			"TestIntegrationHoldEscalateToClosure|"+
			"TestIntegrationHoldSLASweep|"+
			"TestIntegrationHoldCancelPartialFailure"),
	)
}

func ckP14CoolingOff(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/213_cooling_off.up.sql",
			"services/internal/db/migrations/213_cooling_off.down.sql"),
		structural(env, "services/internal/accounts/coolingoff.go",
			"acknowledged", "cooling_off_periods", "COOLING_OFF_PARTIAL"),
		// Irrevocable: no mutation path on the cooling-off window exists.
		structuralLacks(env, "services/internal/accounts/coolingoff.go",
			"UPDATE cooling_off_periods", "DELETE FROM cooling_off_periods"),
		gotest(p14Acct, "TestIntegrationCoolingOffActivateAndGate|"+
			"TestIntegrationCoolingOffDeriskSaga|"+
			"TestIntegrationCoolingOffPartialFailure"),
		gotest(p14Ord, "TestCoolingOffRejectsLeveragedEntry|"+
			"TestCoolingOffGateErrorFailsClosed|TestCoolingOffGateScope|"+
			"TestCoolingOffBatchEntry|TestCoolingOffModifyEntry|"+
			"TestCoolingOffPlainErrorFailsClosed"),
	)
}

func ckP14CoolingOffEnforcement(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		// Order admission consults the gate for leveraged, exposure-adding
		// orders only — reduce-only exits bypass it.
		structural(env, "services/internal/orders/service.go",
			"CoolingOffGate", "checkAdmission", "COOLING_OFF_ACTIVE"),
		// Webhook ownership stays with the Phase-05 dispatcher (retry
		// schedule + DEAD_LETTERED status live there); Phase-14 adds only
		// the JetStream ingest + dead-letter admin surface.
		files(env, "services/internal/webhooks/jetstream.go"),
		structural(env, "services/internal/webhooks/dispatch.go",
			"RetryPolicy"),
		structural(env, "services/internal/webhooks/webhooks.go",
			"DEAD_LETTERED", "ListDeadLetters", "Retransmit",
			"webhook.retransmit"),
		gotest(p14Hooks, "TestIntegrationDeadLetterRetransmit"),
		gotest(p14Ord, "TestCoolingOffGateErrorFailsClosed"),
	)
}

func ckP14ProductProfiles(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/095_account_products_swapfree.up.sql",
			"services/internal/db/migrations/095_account_products_swapfree.down.sql"),
		structural(env, "services/internal/accounts/products.go",
			"account_product_profiles", "AssignProfile", "OpenExposure",
			"ValidateProfileSwitch"),
		// Admission seam: the orders checkAdmission path consults the
		// ProductGate for every new-order entry.
		structural(env, "services/internal/orders/service.go",
			"ProductGate", "products.AdmitOrder"),
		gotest(p14Acct, "TestIntegrationProductAssignPreconditions|"+
			"TestIntegrationProductDivisorSwitchZeroBalance|"+
			"TestIntegrationProductCreateTx|TestIntegrationTargetMarketGate"),
		gotest(p14Acct, "TestProfileInputValidation|TestProductProfileInScope"),
	)
}

func ckP14SwapFree(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/095_account_products_swapfree.up.sql",
			"services/internal/db/migrations/095_account_products_swapfree.down.sql"),
		structural(env, "services/internal/accounts/swapfree.go",
			"swapfree_verifications", "IsSwapFreeVerified",
			"AbuseWindowMax", "compliance_holds"),
		// Prospective enforcement: the Task 3.3.19 rollover consumes
		// accounts.swapfree_status directly for the zero-Tom-Next skip.
		structural(env, "services/internal/settlement/rollover_service.go",
			"swapfree_status"),
		gotest(p14Acct, "TestIntegrationSwapfreeLifecycle"),
	)
}

func ckP14TargetMarket(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/099_product_target_markets.up.sql",
			"services/internal/db/migrations/099_product_target_markets.down.sql"),
		structural(env, "services/internal/accounts/target_market.go",
			"product_target_markets", "SweepOverdue",
			"TARGET_MARKET_REVIEW_OVERDUE"),
		structural(env, "services/internal/accounts/product_gate.go",
			"retailTargetCheck", "REVIEW_OVERDUE"),
		gotest(p14Acct, "TestIntegrationTargetMarketGate|"+
			"TestIntegrationTargetMarketOverdue"),
		gotest(p14Acct, "TestTargetMarketInputValidation|TestTargetMarketOverdue"),
	)
}

// --- Task 14.3.1 OCO orders, 14.3.8 PAMM engine, 14.3.14 copy product ---

func ckP14OCO(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/218_oco_group_link.up.sql"),
		gtest("test_oco", "OcoEngine.*"),
		gotest(p14Ord, "TestSubmitOCO_LinkBeforeLegs|TestSubmitOCO_ReplayIsIdempotent|TestSubmitOCO_RaceReplayReturns409|TestSubmitOCO_CollisionRollsBackPair|TestSubmitOCO_CrossInstrumentRejected|TestSubmitOCO_DispatchFailureRejectsBoth|TestConsumer_OcoReasonAudit|TestParseSubmitOco"),
		gotest(p14API, "TestOrderSubmitOCO_RequiresAuth|TestWriteServiceErr_OcoRaceMaps409"),
	)
}

func ckP14PAMM(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/216_pamm_engine.up.sql"),
		gotest("./internal/pamm", "TestAllocateProRata_ConservesTotal|TestAllocateProRata_RemainderToLowestID|TestAllocateProRata_Deterministic|TestAllocateProRata_FailClosed|TestFanout_HandleMsg_DispatchesBothLegs|TestFanout_HandleMsg_PoisonAndNonFill|TestFanout_HandleMsg_ResolverErrorIsFatal|TestFanout_HandleMsg_HandlerErrorIsFatal|TestCreatePool_Hierarchy|TestInvest_TaxonomyAndCapIsolation|TestInvest_BelowMinimum|TestRedeem_OverInvested|TestInvest_Idempotency|TestITInvestRedeemAndFiatGuard|TestITFillFanout"),
		structural(env, "services/internal/ledger/chart.go", "PAMM_"),
	)
}

func ckP14CopyProduct(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/097_copy_trading_product.up.sql"),
		gotest("./internal/copy", "TestEngine_FullMode|TestEngine_HalfRiskScaling|TestEngine_MinNotionalSkipNotice|TestEngine_ReplayDedup|TestEngine_ProRataAcrossFollows|TestListGate_Incubation|TestListGate_Appropriateness|TestListGate_ManagerOnly|TestSuspendBlocksNewFollows|TestFollowRequiresListed|TestUnfollowCancelsPending|TestProfitShare_RatchetsHWM|TestProfitShare_LossMonthHoldsHWM|TestProfitShare_AccruesOnlyAboveHWM|TestProfitShare_BalancedGL|TestProfitShare_IdempotentPeriod|TestITCopyLifecycle|TestITCopyFanout"),
	)
}
