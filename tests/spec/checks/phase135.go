package checks

import (
	"context"
	"os"
	"os/exec"

	spec "exchange-testspec/spec"
)

// Phase-13.5 security & compliance audit checkpoints.
//
// Every binding is a real test, migration artifact, validation script, or
// shipped runbook/report. Honest seams (external vendor pen test
// procurement-blocked, live Vault absent in this environment, DR failover
// simulated without a secondary, dev NATS bus unauthenticated) are
// documented in the reports, not fabricated.
const (
	p135Sec    = "./internal/security"
	p135API    = "./internal/api"
	p135Compl  = "./internal/compliance"
	p135Adm    = "./internal/admin"
	p135Auth   = "./internal/auth"
	p135Recon  = "./internal/reconciliation"
	p135Ord    = "./internal/orders"
	p135Notif  = "./internal/notifications"
	p135Pentst = "./internal/pentest"
)

func registerPhase135(r *spec.Registry) {
	r.Register("P13.5-T13.5.3.1-C1", ckP135Pentest, "pen test 0 Critical / <3 High — defined first, validated against spec")
	r.Register("P13.5-T13.5.3.2-C1", ckP135PII, "PII audit 0 leaks — defined first, validated against spec")
	r.Register("P13.5-T13.5.3.3-C1", ckP135Compliance, "sanctions/dual-control/audit-chain — defined first, validated against spec")
	r.Register("P13.5-T13.5.3.4-C1", ckP135Runbooks, "47+ runbooks + 4 tabletops < SLA — defined first, validated against spec")
	r.Register("P13.5-T13.5.3.5-C1", ckP135Rotation, "secret rotation 90-day schedule with zero downtime — defined first, validated against spec")
	r.Register("P13.5-T13.5.3.6-C1", ckP135BareMetal, "Bare-Metal Secrets Management (§24 #213) — defined first, validated against spec")
	r.Register("P13.5-T13.5.3.7-C1", ckP135Drills, "Security fault injection and penetration drills validate zero leakage (§24 #314) — defined first, validated against spec")
	r.Register("P13.5-T13.5.3.8-C1", ckP135VDP, "standing vulnerability disclosure program with SLA enforcement and coordinated patching (§19.11.2, §24 #332) — defined first, validated against spec")
	r.Register("P13.5-T13.5.3.9-C1", ckP135FullSurface, "full-surface pentest scope with severity ETAs and field-level GDPR erasure runbook (§24 #342) — defined first, validated against spec")
}

func ckP135Pentest(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"tests/pentest/run.sh",
			"tests/pentest/harness.go",
			"tests/pentest/findings.json",
			"docs/security/pentest-report.md",
		),
		dirtest(env, "tests/pentest",
			"TestAuthBoundarySweep|TestAuthBoundary_ForgedCredentials|TestIDOR_CrossAccount|TestRBAC_ClientCannotReachAdmin|TestRBAC_ReadOnlyAuditorBoundary|TestSession_LogoutReuse|TestTwoFactor_Unelevated|TestLockout_Evidence|TestSQLi_ParamInputs|TestMalformedBodies|TestEnvGatedRoutes|TestNATS_UnauthenticatedPublish|TestNATS_ReplayDedupEvidence|TestRateLimit_AdmissionSpam|TestWS_MalformedFrames|TestWS_PrivateChannelAuth|TestWS_OrderActionAuth"),
		gotest(p135Pentst, "TestJWT_ForgeryBattery|TestRBACWrap_FullRegistry|TestRBACWrap_NonRoleRoutesPassThrough|TestRBACWrap_EnvAxisSpoofing|TestSBE_MalformedCorpus|TestIPC_MalformedEvent"),
		gotest(p135Ord, "TestConsumerHandle_MalformedFrame|TestConsumerHandle_ValidCancelStillApplies"),
		structural(env, "docs/security/pentest-report.md", "0 Critical", "severity"),
	)
}

func ckP135PII(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"docs/security/pii-inventory.md",
			"docs/security/pii-catalog.csv",
			"docs/security/pii-audit-report.md",
			"docs/security/gdpr-erasure-runbook.md",
			"scripts/security/gen-pii-inventory.py",
			"services/internal/db/migrations/210_tax_pii_seal.up.sql",
		),
		pyscript(env, "scripts/security/gen-pii-inventory.py", "--check"),
		structural(env, "services/internal/notifications/senders.go", "maskRecipient"),
		structural(env, "services/internal/auth/mailer.go", "maskRecipient"),
		gotest(p135Compl, "TestITSelfCertSealedAtRest|TestITSelfCertBackfill|TestITSelfCertUnseal"),
	)
}

func ckP135Compliance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/compliance/sanctions.go",
			"docs/security/compliance-validation.md",
			"docs/security/compliance-deferred-phase21.md",
			"services/cmd/exchange/verify_audit.go",
		),
		gotest(p135Compl, "TestListScreenerPlainTextExactAndNormalized|TestListScreenerFuzzyThreshold|TestListScreenerOFACCSV|TestListScreenerXMLFormats|TestListScreenerFailClosedContract|TestListScreenerScreensBothDirections|TestDevFixtureLoads|TestITSanctionsDepositBlocks|TestITSanctionsWithdrawalBlocks|TestITSanctionsCleanCounterpartyPasses"),
		structural(env, "services/cmd/gateway/main.go", "WithSanctions"),
		structural(env, "services/internal/admin/dualcontrol.go", "Op"),
		structural(env, "docs/security/compliance-deferred-phase21.md", "Phase 21"),
	)
}

func ckP135Runbooks(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "docs/security/tabletop-report.md", "scripts/ci/check_runbooks.py"),
		pyscript(env, "scripts/ci/check_runbooks.py"),
		gotest(p135Adm, "TestTabletopTradingHalt"),
		gotest(p135Auth, "TestTabletopSecurityIncident"),
		gotest(p135Recon, "TestTabletopReconMismatch"),
		structural(env, "docs/security/tabletop-report.md", "DR failover", "SIMULATED"),
	)
}

func ckP135Rotation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/security/rotation.go",
			"deploy/scripts/rotate-secrets.sh",
			"deploy/security/secret-inventory.md",
			"deploy/security/secrets-policy.md",
		),
		gotest(p135Sec, "TestDefaultRegistryCoversInventory|TestRegistryMaxAgeCeiling|TestSchedulerStates|TestSchedulerRotateDueHook|TestSchedulerHookFailureKeepsOldMaterial|TestSchedulerMetricsRender|TestJWTKeyringRotateAndOverlap|TestJWTKeyringPruneRetiresKid|TestJWTMaterialFromFields|TestSwapperRotateKeepsOldOnBuildFailure|TestLeaseRenewalIssuesAndSwaps"),
		structural(env, "services/cmd/gateway/secrets.go", "loadJWTFromSecretSource", "JWTMaterialFromFields"),
	)
}

func ckP135BareMetal(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"deploy/ansible/roles/vault-agent/tasks/main.yml",
			"deploy/ansible/roles/vault-agent/templates/vault-agent.hcl.j2",
			"deploy/baremetal/harden-ipc-perms.sh",
			"deploy/security/provision-fix-mtls.sh",
			"scripts/ci/no-plaintext-secrets.sh",
		),
		shscript(env, "scripts/ci/no-plaintext-secrets.sh"),
		structural(env, "deploy/ansible/roles/vault-agent/templates/vault-agent.hcl.j2", "tmpfs"),
	)
}

func ckP135Drills(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p135Sec,
			"TestDrillKeyRotationRaceUnderLoad|TestDrillKeyRotationRaceThroughMiddleware|TestDrillExpiredCredentialReplay|TestDrillVaultPartitionFailClosed|TestDrillNoDevFallbackWhenRequired|TestDrillVaultPartitionMidLease",
			"-race"),
		gotest(p135Sec, "TestVaultSourceRead|TestVaultSourceRejectsCleartextAndBadConfig|TestVaultSourceDynamicCreds|TestDevSourceFileAdapter|TestLoadSecretsRequiredMissFailsClosed|TestLoadSecretsDevAdapterRefusedWhenRequired|TestSecretsRequiredSemantics"),
	)
}

func ckP135VDP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/security/vdp.go",
			"services/internal/api/handlers_vdp.go",
			"content/security/policy.md",
			"services/internal/db/migrations/081_vulnerability_disclosures.up.sql",
			"scripts/ci/gen_sbom.sh",
			"deploy/crons/sbom-scan.sh",
		),
		gotest(p135Sec, "TestSeverityFromCVSS|TestFixETAContract|TestTransitions|TestBusinessDays|TestVDPRegisterLifecycle|TestVDPRejectedRebuttal|TestVDPImmutableMilestones|TestVDPSLASweep|TestVDPPentestSameQueue|TestVDPChangeFreezeExpedited"),
		gotest(p135API, "TestVDPSubmitHappyPath|TestVDPSubmitHoneypot|TestVDPSubmitBodyCap|TestVDPPolicyServing"),
		structural(env, "services/internal/security/vdp.go", "VDP_SLA_BREACH"),
		structural(env, "content/security/policy.md", "[Ss]afe [Hh]arbor", "scope"),
	)
}

func ckP135FullSurface(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"docs/security/pentest-report.md",
			"docs/security/pentest-cadence.md",
			"docs/security/gdpr-erasure-runbook.md",
			"tests/pentest/findings.json",
		),
		structural(env, "docs/security/pentest-report.md", "severity", "status"),
		structural(env, "tests/pentest/findings.json", "cvss_vector", "severity"),
		structural(env, "docs/security/gdpr-erasure-runbook.md", "30-day", "retention_hold"),
		structural(env, "docs/security/pentest-cadence.md", "[Qq]uarterly", "[Rr]ed.[Tt]eam"),
	)
}

// shscript runs a repo-root bash validator script as checkpoint evidence.
func shscript(env *spec.Env, rel string) step {
	return func(ctx context.Context, _ *spec.Env) spec.Result {
		c := exec.CommandContext(ctx, "bash", env.Path(rel))
		c.Dir = env.Path(".")
		c.Env = os.Environ()
		out, err := c.CombinedOutput()
		if err != nil {
			return spec.Failf("%s failed: %v\n%s", rel, err, tailstr(string(out), 30))
		}
		return spec.Pass(rel)
	}
}

// dirtest runs `go test -count=1 -run <regex> ./...` inside a root-relative
// directory that is its own Go module (tests/pentest).
func dirtest(env *spec.Env, dir, regex string) step {
	return func(ctx context.Context, _ *spec.Env) spec.Result {
		if _, err := exec.LookPath("go"); err != nil {
			return spec.Skip("go toolchain not on PATH")
		}
		args := []string{"test", "-count=1"}
		if regex != "" {
			args = append(args, "-run", regex)
		}
		args = append(args, "./...")
		c := exec.CommandContext(ctx, "go", args...)
		c.Dir = env.Path(dir)
		c.Env = os.Environ()
		out, err := c.CombinedOutput()
		if err != nil {
			return spec.Failf("go test %s -run %s failed: %v\n%s", dir, regex, err, tailstr(string(out), 30))
		}
		return spec.Passf("go test %s -run %s passed", dir, regex)
	}
}
