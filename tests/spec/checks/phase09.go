package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-09 deployment & operations checkpoints.
//
// Code tasks bind to real Go tests (flags, cache warming, shed,
// shutdown, tracing, timesync/PTP, fleet, retention, archiver,
// deprecation ops, ops status, drains). Deploy/doc tasks bind to
// artifact existence + content — live drills (multi-region DR, Sentinel
// failover timing, K8s scheduling, PTP hardware) remain honest open AC
// rows, not checkpoint failures.
const (
	p09Flags   = "./internal/flags"
	p09Cache   = "./internal/cache"
	p09MW      = "./internal/middleware"
	p09Ops     = "./internal/ops"
	p09Fleet   = "./internal/fleet"
	p09Trace   = "./internal/tracing"
	p09TS      = "./internal/timesync"
	p09Ret     = "./internal/operations/retention"
	p09Arch    = "./internal/archiver"
	p09Dep     = "./internal/deprecation"
	p09API     = "./internal/api"
	p09MD      = "./internal/marketdata"
	p09Bridge  = "./internal/bridge"
)

func registerPhase09(r *spec.Registry) {
	r.Register("P09-T9.3.1-C1", ckP09Baremetal, "bare metal C++ core with NUMA pinning")
	r.Register("P09-T9.3.2-C1", ckP09K8s, "K8s Go services with HPA")
	r.Register("P09-T9.3.3-C1", ckP09BlueGreen, "blue-green with rollback")
	r.Register("P09-T9.3.4-C1", ckP09DR, "multi-region DR with RTO targets")
	r.Register("P09-T9.3.4-C2", ckP09DRPG, "PostgreSQL RPO <= 15s / RTO <= 5min")
	r.Register("P09-T9.3.4-C3", ckP09DRRedis, "Redis RPO <= 5s / RTO <= 30s")
	r.Register("P09-T9.3.5-C1", ckP09Runbooks, "47+ alert runbooks")
	r.Register("P09-T9.3.5-C2", ckP09Tabletops, "4 tabletops < SLA")
	r.Register("P09-T9.3.6-C1", ckP09Deprecation, "6-month API deprecation notice")
	r.Register("P09-T9.3.7-C1", ckP09Flags, "feature flags for canary deploys")
	r.Register("P09-T9.3.8-C1", ckP09Warm, "cache warming P0 30s / P1 5s")
	r.Register("P09-T9.3.9-C1", ckP09Postmortem, "post-mortem within 48h")
	r.Register("P09-T9.3.10-C1", ckP09Shed, "graceful load shedding")
	r.Register("P09-T9.3.11-C1", ckP09Tracing, "trace_id continuity HTTP -> Aeron -> C++")
	r.Register("P09-T9.3.12-C1", ckP09PTP, "PTP clock sync within 100us (MiFID II RTS 25)")
	r.Register("P09-T9.3.13-C1", ckP09Edge, "WAF/DDoS edge protection (§19.1, §24 #160)")
	r.Register("P09-T9.3.14-C1", ckP09SLO, "SLOs + error budgets defined and wired (§19.3, §24 #162)")
	r.Register("P09-T9.3.15-C1", ckP09DORA, "DORA ICT governance, reporting, testing and third-party registers")
	r.Register("P09-T9.3.16-C1", ckP09ShardSwap, "C++ bare-metal deployment and shard drain procedure")
	r.Register("P09-T9.3.17-C1", ckP09Archival, "PostgreSQL 5-year partition archival pipeline")
	r.Register("P09-T9.3.18-C1", ckP09Incident, "Incident classification P0-P3 and escalation matrix")
	r.Register("P09-T9.3.19-C1", ckP09Capacity, "Capacity planning and sizing models")
	r.Register("P09-T9.3.20-C1", ckP09Sentinel, "Redis Sentinel 3-node HA deployment and failover")
	r.Register("P09-T9.3.21-C1", ckP09DRDrills, "Quarterly DR Drill Program")
	r.Register("P09-T9.3.22-C1", ckP09Retention, "Unified Data Retention Policy")
	r.Register("P09-T9.3.23-C1", ckP09Shutdown, "Go Service Graceful Shutdown")
	r.Register("P09-T9.3.24-C1", ckP09Tiering, "data tiering policy")
	r.Register("P09-T9.3.25-C1", ckP09Status, "public status page infrastructure and operational health model")
	r.Register("P09-T9.3.26-C1", ckP09Canary, "Automated canary rollback and blue-green health verification")
	r.Register("P09-T9.3.27-C1", ckP09BCP, "documented, annually exercised BCP with stand-down/go-forward")
	r.Register("P09-T9.3.28-C1", ckP09Daemons, "Daemon execution inventory, systemd templates, and multi-tier watchdog")
	r.Register("P09-T9.3.29-C1", ckP09ObsContract, "observability budgets, capacity proof, secrets inventory")
	r.Register("P09-T9.3.30-C1", ckP09Fleet, "environment context model, fleet inventory, promotion gates")
}

func ckP09Baremetal(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/baremetal/provision-matching-host.sh",
			"deploy/baremetal/sysctl.d/90-exchange-latency.conf",
			"docs/ops/baremetal-provisioning.md",
		),
		structural(env, "deploy/baremetal/provision-matching-host.sh", "nr_hugepages", "isolcpus"),
	)
}

func ckP09K8s(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "deploy/k8s/services/admin.yaml", "readinessProbe", "livenessProbe", "/health/ready"),
		structural(env, "deploy/k8s/services/aeron-nats-bridge.yaml", "readinessProbe", "livenessProbe"),
		structural(env, "deploy/k8s/services/aeron-nats-bridge.yaml", "HorizontalPodAutoscaler", "resources"),
	)
}

func ckP09BlueGreen(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/scripts/bluegreen.sh",
			"deploy/scripts/rollback.sh",
			"docs/ops/blue-green-deploy.md",
		),
		structural(env, "deploy/scripts/bluegreen.sh", "active_color"),
	)
}

func ckP09DR(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/ops/dr.md", "deploy/dr/failover-runbook.sh"),
		structural(env, "docs/ops/dr.md", "RPO", "RTO"),
	)
}

func ckP09DRPG(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "deploy/dr/postgres-standby.conf.sample", "deploy/dr/s3-crr-wal-archive.json"),
		structural(env, "deploy/dr/postgres-standby.conf.sample", "remote_flush"),
	)
}

func ckP09DRRedis(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "deploy/dr/redis-secondary.conf.sample"),
		structural(env, "docs/ops/dr.md", "5s", "30s"),
	)
}

func ckP09Runbooks(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/runbooks/README.md", "docs/runbooks/wal-recovery-halt.md"),
		structural(env, "docs/runbooks/wal-recovery-halt.md", "Mitigation"),
	)
}

func ckP09Tabletops(ctx context.Context, env *spec.Env) spec.Result {
	return structural(env, "docs/runbooks/README.md", "T1", "T4")(ctx, env)
}

func ckP09Deprecation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09Dep, "TestMiddlewareTelemetry|TestMiddlewareTelemetryNilSink|TestMiddlewareGone|TestIntegrationDeprecations|TestIntegrationMiddlewareGone"),
		files(env, "services/internal/db/migrations/196_api_deprecation_ops.up.sql"),
	)
}

func ckP09Flags(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09Flags, "TestEvalDeterministic|TestStageLadder|TestStoreRoundTripIntegration|TestBucketDistribution|TestEvalMonotonicRollout"),
		gotest(p09API, "TestFlagHandlersIntegration|TestFlagHandlersRequireAdmin|TestFlagCreateMalformedBody"),
		files(env, "services/internal/db/migrations/193_feature_flags.up.sql"),
	)
}

func ckP09Warm(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09Cache, "TestPriorityOrder|TestMinKeysGate|TestBudgetOverrun|TestFailingUnitNotFresh|TestEnvelopeFreshness|TestConcurrentRuns|TestTriggerDefault"),
		gotest(p09API, "TestAdminCacheWarm"),
	)
}

func ckP09Postmortem(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/ops/postmortem-template.md"),
		structural(env, "docs/ops/postmortem-template.md", "Timeline", "Action"),
	)
}

func ckP09Shed(ctx context.Context, env *spec.Env) spec.Result {
	return gotest(p09MW,
		"TestShedActivatesAbove500|TestShedFractionDeterministic|TestShedHysteresis|TestShedTierOrder|TestShedMissingDepth|TestSheddingMiddlewareCodes|TestCancelExempt|TestCapacityWatermarks")(ctx, env)
}

func ckP09Tracing(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09Trace, "TestParseTraceParent|TestParentChildContinuity|TestHeadSamplingDeterministic|TestTailSampling|TestAeronTraceRoundTrip|TestOTLPEncodingShape|TestHTTPMiddlewareContinuity|TestBatchExporterFlushAndDrop"),
		files(env, "deploy/otel/otel-collector.yaml"),
	)
}

func ckP09PTP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09TS, "TestPMCReaderParsesLockedSlave|TestPMCReaderGMMissingNotSynced|TestMonitorBoundViolation|TestMonitorStaleness|TestMonitorUnavailableOnExpectedHost|TestDailyDivergenceReport|TestStatsFileReader|TestCheckDriftTripsHaltCode"),
		files(env, "deploy/ansible/roles/ptp/tasks/main.yml", "deploy/prometheus/rules/exchange-alerts.yml"),
	)
}

func ckP09Edge(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "deploy/edge/README.md", "deploy/edge/ddos-playbook.md"),
		structural(env, "deploy/haproxy/haproxy.cfg", "st_conn_per_ip", "st_req_per_ip"),
		structural(env, "deploy/prometheus/rules/exchange-alerts.yml", "edge"),
	)
}

func ckP09SLO(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/ops/slo-policy.md"),
		structural(env, "docs/ops/slo-policy.md", "burn", "Error-budget"),
		structural(env, "deploy/prometheus/rules/exchange-alerts.yml", "AvailabilityBurn"),
	)
}

func ckP09DORA(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"docs/ops/dora-ict-risk-register.md",
			"docs/ops/dora-incident-reporting.md",
			"docs/ops/dora-third-party-register.md",
		),
		structural(env, "docs/ops/dora-incident-reporting.md", "4h|72h|1mo|72-hour"),
	)
}

func ckP09ShardSwap(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "scripts/deploy/shard-swap.sh", "docs/ops/shard-binary-swap.md"),
		structural(env, "scripts/deploy/shard-swap.sh", "system:degradation:mode", "force_snapshot|snapshot"),
	)
}

func ckP09Archival(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09Arch, "TestLifecycleHotToWarmToCold|TestComplianceHoldBlocksLifecycle|TestVerifyDrillDetectsCorruption|TestArchiveAndRestorePartition"),
		files(env, "services/internal/db/migrations/194_data_retention_holds.up.sql", "services/internal/db/migrations/195_partition_tier_state.up.sql"),
	)
}

func ckP09Incident(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/runbooks/incident-escalation.md"),
		structural(env, "docs/runbooks/incident-escalation.md", "P0", "P3"),
	)
}

func ckP09Capacity(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/ops/capacity-planning.md"),
		structural(env, "deploy/monitoring/capacity-alerts.yml", "expr"),
	)
}

func ckP09Sentinel(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/sentinel/sentinel-1.conf",
			"deploy/sentinel/sentinel-2.conf",
			"deploy/sentinel/sentinel-3.conf",
			"deploy/sentinel/health-probe.sh",
		),
		structural(env, "deploy/sentinel/sentinel-1.conf", "sentinel monitor", "down-after-milliseconds"),
		files(env, "deploy/crons/redis-failover-drill.sh", "docs/ops/redis-sentinel-failover.md"),
	)
}

func ckP09DRDrills(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/runbooks/dr-drill.md"),
		structural(env, "docs/runbooks/dr-drill.md", "D1", "RPO"),
	)
}

func ckP09Retention(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09Ret, "TestBuiltinPolicyValidates|TestEnforcerDryRunAndApply|TestEnforcerHoldBlocksPurge|TestEnforcerClickHouseTTL|TestLoadYAML|TestShippedPolicyCoversBuiltin"),
		files(env, "docs/compliance/data-retention.md", "infrastructure/data-tiering/tiering_policy.yaml"),
	)
}

func ckP09Shutdown(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09MW, "TestDrainFlag|TestReadyGate|TestRunStepsOrderAndBounds|TestRunStepTimeout"),
		gotest(p09MD, "TestDrain|TestDrainLatch|Drain"),
		gotest(p09Bridge, "TestFlushDrainsBuffer|TestFlushBoundedByContext"),
	)
}

func ckP09Tiering(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/ops/data-tiering-policy.md", "infrastructure/data-tiering/tiering_policy.yaml", "scripts/data_migration.sh"),
		structural(env, "infrastructure/data-tiering/tiering_policy.yaml", "hot|HOT"),
	)
}

func ckP09Status(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09Ops, "TestCollectAggregates|TestProbeTimeoutIsDown|TestUptimeRequiresPG|TestModeReadFailureIsMaintenance"),
		gotest(p09API, "TestSystemStatusFallback|TestAdminOpsHealthRequiresAuditor"),
		files(env, "services/internal/db/migrations/197_ops_status.up.sql"),
	)
}

func ckP09Canary(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "deploy/scripts/canary-check.sh"),
		structural(env, "deploy/scripts/canary-check.sh", "rollback|ROLLBACK", "health"),
	)
}

func ckP09BCP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/policies/business-continuity-plan.md", "docs/runbooks/bcp-standdown.md", "docs/runbooks/bcp-goforward.md"),
		structural(env, "docs/policies/business-continuity-plan.md", "go-forward|GF-"),
	)
}

func ckP09Daemons(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"docs/ops/daemon-inventory.md",
			"deploy/systemd/matching-engine@.service",
			"deploy/systemd/exchange-watchdogd.service",
		),
		structural(env, "deploy/systemd/matching-engine@.service", "WatchdogSec", "Restart"),
	)
}

func ckP09ObsContract(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"docs/ops/observability-contract.md",
			"docs/ops/capacity-proof.md",
			"docs/ops/secrets-inventory.md",
		),
		structural(env, "docs/ops/observability-contract.md", "metric|metric"),
	)
}

func ckP09Fleet(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p09Fleet, "TestPromotionDirectionLattice|TestEvaluateGatesFailClosed|TestEvaluateGatesProductionInterlocks|TestGateEvidenceRules|TestActionTransitions|TestSecretPath|TestRequireApprover"),
		gotest(p09Fleet, "TestFleetHostActionsIntegration|TestPromotionIntegration|TestTopologyIntegration"),
		files(env, "services/internal/db/migrations/091_fleet_and_ops_console.up.sql"),
	)
}
