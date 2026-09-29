package checks

import (
	"context"
	"os"
	"os/exec"
	"strings"

	spec "exchange-testspec/spec"
)

// Phase-13 production hardening & reconciliation checkpoints.
//
// Every binding is a real test, migration artifact, or validation script.
// Honest seams (NullIVSource for Phase-22 options, nostro custodian
// attestation metadata, C++ core state query) are documented in code and
// covered by inconclusive-result tests, not fabricated evidence.
const (
	p13Risk  = "./internal/risk"
	p13Recon = "./internal/reconciliation"
	p13API   = "./internal/api"
	p13Auth  = "./internal/auth"
	p13Ord   = "./internal/orders"
	p13Adm   = "./internal/admin"
)

func registerPhase13(r *spec.Registry) {
	r.Register("P13-T13.3.1-C1", ckP13BreakerScopes, "five-tier circuit breaker with exact scopes (INSTRUMENT, ACCOUNT, VOLUME_SPIKE, OPTIONS_VOLATILITY, MARKET_WIDE)")
	r.Register("P13-T13.3.1-C2", ckP13BreakerTriggers, "exact triggers (5%/60s, 3+ losses/5%/5min, z≥4.0σ, IV>200%, >20%/>2 instruments)")
	r.Register("P13-T13.3.1-C3", ckP13BreakerHolds, "hold times (5min, 30min, 10min, 15min, manual)")
	r.Register("P13-T13.3.1-C4", ckP13BreakerSM, "state machine CLOSED → OPEN → HALF_OPEN → CLOSED")
	r.Register("P13-T13.3.1-C5", ckP13BreakerAdmin, "admin endpoints with dual control on reset")
	r.Register("P13-T13.3.2-C1", ckP13Recon, "9 financial correctness categories (incl. GL zero-sum)")
	r.Register("P13-T13.3.3-C1", ckP13Alerts, "47+ alert rules")
	r.Register("P13-T13.3.4-C1", ckP13PnL, "real-time P&L")
	r.Register("P13-T13.3.5-C1", ckP13PentestPrep, "pen test prep")
	r.Register("P13-T13.3.6-C1", ckP13OTR, "OTR limits per RTS 9 (§13.6a, §24 #140)")
	r.Register("P13-T13.3.7-C1", ckP13Solvency, "Proof of Reserves Merkle tree and verification API (§17.11, §24 #186)")
	r.Register("P13-T13.3.8-C1", ckP13KeyExpiry, "non-allowlisted privileged API keys auto-expire with warning and audit (§24 #272)")
	r.Register("P13-T13.3.9-C1", ckP13BreakerReset, "Circuit breaker automated reset, flapping penalty, and probe verification (§24 #313)")
}

func ckP13BreakerScopes(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p13Risk, "TestScopeTable|TestValidBreakerScope|TestRedisKeyLayout|TestLoadHydrates"),
		structural(env, "services/internal/risk/circuit_breaker.go", "INSTRUMENT", "ACCOUNT", "VOLUME_SPIKE", "OPTIONS_VOLATILITY", "MARKET_WIDE"),
	)
}

func ckP13BreakerTriggers(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p13Risk, "TestInstrumentPriceMoveTrip|TestAccountLossRapidTrip|TestVolumeSpikeZScore|TestOptionsVolatilityTrip|TestMarketWideTrip|TestNullIVSourceFailsClosed|TestInstrumentLimitOverride"),
	)
}

func ckP13BreakerHolds(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p13Risk, "TestScopeTable|TestManualTripAndReset|TestAccountLossWindowPrunes"),
		structural(env, "services/internal/risk/circuit_breaker.go", `hold: 5 \* time\.Minute`, `hold: 30 \* time\.Minute`, `hold: 10 \* time\.Minute`, `hold: 15 \* time\.Minute`),
	)
}

func ckP13BreakerSM(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p13Risk, "TestProbeRecoveryCycle|TestProbeWindowExpiryReopens|TestTransitionCooldown|TestAdmissionFailsClosedOnBreakerGate"),
		gotest(p13Ord, "TestAdmissionFailsClosedOnBreakerGate"),
	)
}

func ckP13BreakerAdmin(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/api/handlers_circuit_breaker.go", "AdminCircuitBreakerTrip", "AdminCircuitBreakerReset"),
		structural(env, "services/internal/admin/dualcontrol.go", "OpCircuitBreakerReset"),
		structural(env, "services/internal/gateway/routes_v1.go", "circuit-breaker/{symbol}", "circuit-breaker/{symbol}/reset"),
	)
}

func ckP13Recon(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/207_reconciliation_engine.up.sql"),
		gotest(p13Recon, "TestBalancesChecker|TestPositionsChecker|TestOrdersChecker|TestTradesCheckerDiff|TestFundingChecker|TestSettlementChecker|TestFeesChecker|TestPnLChecker|TestGeneralLedgerImbalance|TestGeneralLedgerClean"),
		gotest(p13Recon, "TestEngineMismatchAlertsAndHalts|TestEngineInconclusiveNeverHalts|TestEngineHaltEscalation|TestReconciliationFullCycle"),
		gotest(p13Recon, "TestWalReplayRestingBook|TestWalReplayCorruptTailIsIncomplete|TestWalReplayGapIsIncomplete"),
	)
}

func ckP13Alerts(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "deploy/prometheus/alerts.yml", "deploy/prometheus/alertmanager.yml", "scripts/ci/check_alert_rules.py"),
		pyscript(env, "scripts/ci/check_alert_rules.py"),
		structural(env, "deploy/prometheus/alertmanager.yml", "pagerduty"),
	)
}

func ckP13PnL(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p13Risk, "TestPnlSnapshotLongAndShort|TestPnlMarkFallbackChain|TestPnlMarkLookupFailsClosed|TestPnlFailClosedOnStore|TestPnlBaseCurrencyConversion|TestPnlOnTradePublishesAccountAndHolders"),
		gotest(p13API, "TestAccountPnLHappyPath|TestAccountPnLRejectsForeignAccount|TestAccountPnLRequiresClaims|TestAccountPnLNilServiceDegraded"),
		structural(env, "services/internal/ws/server.go", "private:pnl"),
	)
}

func ckP13PentestPrep(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"docs/security/attack-surface.md",
			"docs/security/pentest-scope.md",
			"tests/pentest/seed.sql",
			"tests/pentest/seed.sh",
			"scripts/security/gen-attack-surface.py",
			"services/cmd/route-dump/main.go",
		),
		structural(env, "docs/security/attack-surface.md", "GET /api/v1/"),
		structural(env, "docs/security/pentest-scope.md", "scope"),
	)
}

func ckP13OTR(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/047_otr_limits.up.sql"),
		gotest(p13Risk, "TestOtrKeyLayout|TestOtrBreachRejectsNewOrders|TestOtrFillClearsBreach|TestOtrMarketMakerScopedRatio|TestOtrAdmissionFailsClosedOnRedisError|TestOtrSweepClearsDecayedBreach|TestResolveOtrDefaults|TestResolveOtrScopedOverride|TestOtrRowJSONRoundTrip"),
		gotest(p13Ord, "TestOtr_SubmitRejectedWhenBreached|TestOtr_CancelSurvivesBreach|TestOtr_UnsetGateSkips"),
		structural(env, "services/internal/risk/otr_monitor.go", "otr:events:", "otr:trades:", "otr:breach:", "OTR_LIMIT_EXCEEDED"),
		structural(env, "core/include/risk/RiskInterfaces.hpp", "OtrLimitExceeded"),
	)
}

func ckP13Solvency(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/209_solvency_snapshots.up.sql", "deploy/crons/solvency-tree.sh"),
		gotest(p13Recon, "TestMerkleKnownVector|TestMerkleProofVerify|TestMerkleTamper|TestMerkleZeroBalance|TestMerkleProofBound|TestGenerateHappyPath|TestGenerateNegativeBalanceAborts|TestGenerateInsolventFlag|TestGenerateMissingNostroFailsRatio|TestGenerateZeroLiabilitiesNullRatio|TestGenerateSignerFailurePublishesNothing|TestSignerFromEnv"),
		gotest(p13API, "TestSolvencyLatestPublic|TestSolvencyProofAuth|TestAccountSolvencyProof|TestAdminAPIKeyExtendExpiry"),
		structural(env, "deploy/crons/solvency-tree.sh", "0 22 * * *"),
		structural(env, "services/internal/reconciliation/signers.go", "GPGSigner", "EXC_SOLVENCY_GPG_FINGERPRINT"),
	)
}

func ckP13KeyExpiry(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/208_api_key_auto_expiry.up.sql", "deploy/crons/api-key-expiry.sh"),
		gotest(p13Auth, "TestKeyExpirySweepRevokesStaleUnallowlistedKey|TestKeyExpirySweepWarnsSevenDaysOut|TestKeyExpirySweepRestoresOnAllowlist|TestKeyExpiryOverrideBlocksRevoke"),
		structural(env, "services/internal/auth/apikey_expiry.go", "permissions_revoked_at", "RevokePrivilegedScopes", "RestoreAllowlistedScopes"),
		structural(env, "services/internal/admin/dualcontrol.go", "OpAPIKeyExpiryExtend"),
	)
}

func ckP13BreakerReset(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p13Risk, "TestProbeRecoveryCycle|TestFlapDoublesHoldAndAlerts|TestFlapCap|TestManualTripSkipsFlap|TestPersistErrorStillTrips|TestPgBreakerEventStoreIntegration|TestRedisBreakerIntegration"),
		files(env, "services/internal/db/migrations/206_circuit_breaker_events.up.sql"),
	)
}

// pyscript runs a repo-root python validator script as checkpoint evidence.
func pyscript(env *spec.Env, rel string, args ...string) step {
	return func(ctx context.Context, _ *spec.Env) spec.Result {
		c := exec.CommandContext(ctx, "python3", append([]string{env.Path(rel)}, args...)...)
		c.Dir = env.Path(".")
		c.Env = os.Environ()
		out, err := c.CombinedOutput()
		if err != nil {
			return spec.Failf("%s failed: %v\n%s", rel, err, tailstr(string(out), 30))
		}
		s := string(out)
		return spec.Pass(rel + ": " + tailstr(strings.TrimSpace(s), 5))
	}
}
