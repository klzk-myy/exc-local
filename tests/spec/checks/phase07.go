package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-07 admin & monitoring checkpoints.
//
// Landed: internal/admin (RBAC roles/scopes/lifecycle, dual-control
// queue, audit log, LP module, governance packs), internal/support
// (tickets/complaints/SLA), internal/observability (Prometheus
// exposition, alert evaluator, JetStream DLQ, Aeron CnC monitor),
// per-dependency health endpoints, Grafana/Prometheus deploy assets.
const (
	p07Admin = "./internal/admin"
	p07Obs   = "./internal/observability"
	p07Sup   = "./internal/support"
	p07API   = "./internal/api"
)

func registerPhase07(r *spec.Registry) {

	r.Register("P07-T7.3.1-C1", ckP07RBAC,
		"6 RBAC roles with permission matrix")
	r.Register("P07-T7.3.2-C1", ckP07DualControl,
		"dual control for sensitive ops")
	r.Register("P07-T7.3.3-C1", ckP07Audit,
		"admin audit log with before/after state")
	r.Register("P07-T7.3.4-C1", ckP07Metrics,
		"Prometheus metrics from all services")
	r.Register("P07-T7.3.5-C1", ckP07Grafana,
		"Grafana dashboards + PagerDuty")
	r.Register("P07-T7.3.6-C1", ckP07Health,
		"health + readiness endpoints")
	r.Register("P07-T7.3.7-C1", ckP07Support,
		"support tickets + complaint routing + read-only support view")
	r.Register("P07-T7.3.9-C1", ckP07LP,
		"LP management")
	r.Register("P07-T7.3.10-C1", ckP07Alerting,
		"Error rate alerting, DLQ inspection, and anomaly alarms")
	r.Register("P07-T7.3.11-C1", ckP07Scopes,
		"scoped bindings with grant-time intersection and disjoint role systems")
	r.Register("P07-T7.3.12-C1", ckP07Lifecycle,
		"binding expiry with session kill, quarterly recertification, break-glass")
	r.Register("P07-T7.3.13-C1", ckP07CEOPack,
		"daily executive roll-up with hash-retained packs (§24 #378)")
	r.Register("P07-T7.3.14-C1", ckP07BoardPack,
		"dual-controlled immutable quarterly board packs (§24 #379)")
}

func ckP07RBAC(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Admin, "TestCanonicalRoles|TestPermits|TestPermissionMatrix"),
		gotest(p07Admin, "TestRBACLifecycleIntegration|TestRBACStoreFailsClosed"),
	)
}

func ckP07DualControl(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Admin, "TestRBACLifecycleIntegration"),
		gotest(p07API, "TestLiquidationDualControl"),
		gotest(p07Admin, "TestReleaseDualControl"),
		structural(env, "services/internal/admin/dualcontrol.go", "DUAL_CONTROL_VIOLATION", "DUAL_CONTROL_REQUIRED"),
	)
}

func ckP07Audit(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Admin, "TestAuditEntryValidate|TestVerifyHashTamper|TestMarshalState"),
		gotest(p07Admin, "TestAuditLogIntegration|TestVerifyDayIntegration"),
		gotest(p07API, "TestAdminAuditLog_RoleGate|TestAdminBanListAndAudit"),
	)
}

func ckP07Metrics(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Obs, "TestRegistryCounterGaugeRoundTrip|TestRegistryDeterministicOrder|TestRegistryHandler|TestRegistryHistogramExposition|TestRegistryLabelEscaping"),
		gotest(p07Obs, "TestHTTPMiddlewareCountsAndTiers|TestHTTPMiddlewareUnmatchedRoute|TestTierForStatusMapping|TestServiceGauges|TestGaugeFunc|TestCounterFuncAndVecFunc"),
		files(env,
			"deploy/prometheus/prometheus.yml",
			"deploy/prometheus/rules/exchange-alerts.yml",
		),
	)
}

func ckP07Grafana(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/grafana/dashboards/system-overview.json",
			"deploy/grafana/dashboards/shard-health.json",
			"deploy/grafana/dashboards/trading.json",
			"deploy/grafana/dashboards/ipc-backbone.json",
		),
		structural(env, "deploy/grafana/dashboards/system-overview.json", "panels", "datasource"),
		structural(env, "deploy/prometheus/alertmanager.yml", "pagerduty"),
	)
}

func ckP07Health(ctx context.Context, env *spec.Env) spec.Result {
	return gotest(p07API,
		"TestHealthFullSchema|TestHealthLive_Always200|TestHealthReady_AllOK|TestHealthReady_DegradedModeStays200|TestHealthReady_MaintenanceIs503|TestHealthReady_ModeReadFailureFailsClosed|TestHealthReady_OptionalDepDown|TestHealthReady_RequiredDepDown|TestHealthReady_ShardDownDegrades")(ctx, env)
}

func ckP07Support(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Sup, "TestTicketsIntegration|TestSupportSLA_IsPlain24h|TestTicketMarkBreach"),
		gotest(p07API, "TestSupportTicketCreate_Happy|TestSupportTicketGet_NotFound404|TestAdminSupportTicketList_Filters|TestAdminSupportView_RoleGate|TestAdminSupportTicketUpdate_MappingAndIP"),
		files(env, "services/internal/db/migrations/048_support_tickets.up.sql"),
	)
}

func ckP07LP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Admin, "TestLPTransitions|TestLPScorecardThresholdAlerts|TestLPScorecardPersistedFallback|TestLPAllDownVenueAlert|TestLPIntegration|TestLPScorecardLivePath|TestLPScorecardNoDataNoSource"),
		gotest(p07Admin, "TestLPCreateLifecycleEntry|TestLPCreateRoleGate|TestLPCreateValidation|TestLPUpdateValidation"),
		files(env, "services/internal/db/migrations/191_liquidity_providers.up.sql"),
	)
}

func ckP07Alerting(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Obs, "TestDeltaRuleL0FiresImmediately|TestRatioWindowRule|TestRatioWindowLowTrafficInert|TestErrorRateAnomaly|TestThresholdForDuration|TestStandardRulesTask738|TestPublisherSinkPayload"),
		gotest(p07Obs, "TestMemoryDLQLifecycle|TestDLQHandlers|TestDLQSubjectSanitize|TestDeadLetterMsgTerminates|TestDeadLetterMsgStoreFailureLeavesMessage"),
		gotest(p07Obs, "TestBridgeHeartbeatWatcher|TestCnCMonitorGaugesAndMissing|TestReadCnCCounters|TestNATSMonitorGauges"),
	)
}

func ckP07Scopes(ctx context.Context, env *spec.Env) spec.Result {
	return gotest(p07Admin,
		"TestIntersectScopeNeverWidens|TestUnionScope|TestScopeAllows|TestNormalizeEnv|TestAssertScope|TestMiddleware")(ctx, env)
}

func ckP07Lifecycle(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Admin, "TestRBACLifecycleIntegration|TestCapExpiry|TestAddBusinessDays"),
		files(env, "services/internal/db/migrations/090_admin_rbac.up.sql"),
		structural(env, "services/internal/admin/lifecycle.go", "GrantBreakGlass", "SuspendOverdueRecerts", "ExpireDue"),
	)
}

func ckP07CEOPack(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Admin, "TestGenerateSectionsHashAndVersions|TestGenerateHashChain|TestGenerateDeliverySink|TestPackPeriods|TestGenerateRoleGate|TestGovernancePackIntegration"),
		gotest(p07Admin, "TestPackReadGate"),
		files(env, "services/internal/db/migrations/101_governance_packs.up.sql"),
	)
}

func ckP07BoardPack(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p07Admin, "TestBoardAbsentMarking|TestReleaseDualControl|TestReleaseCEODailyRejected|TestGenerateBoardKindGuard|TestGovernancePackIntegration"),
		structural(env, "services/internal/db/migrations/101_governance_packs.up.sql", "TRIGGER", "released_by"),
	)
}
