package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-12 user self-service & notifications checkpoints.
//
// Every binding is a real test or migration artifact. Live-provider legs
// (SES/Twilio/FCM sends, GeoIP resolution, ClamAV, liveness vendor calls)
// are honest interface seams and are not asserted here; the checkpoint
// texts cover challenge/registry/queue/preference/state-machine
// behavior, all of which execute against live PG/Redis.
const (
	p12Auth  = "./internal/auth"
	p12API   = "./internal/api"
	p12Notif = "./internal/notifications"
	p12Compl = "./internal/compliance"
	p12Deleg = "./internal/delegation"
	p12Acc   = "./internal/accounts"
)

func registerPhase12(r *spec.Registry) {
	r.Register("P12-T12.3.1-C1", ckP12Registration, "user registration + email verification")
	r.Register("P12-T12.3.2-C1", ckP12TOTP, "TOTP 2FA with backup codes")
	r.Register("P12-T12.3.3-C1", ckP12Profile, "profile + API key management")
	r.Register("P12-T12.3.4-C1", ckP12KYCTiers, "KYC tiers T0/T1/T2/institutional")
	r.Register("P12-T12.3.4-C2", ckP12Reverify, "re-verification 12mo/24mo")
	r.Register("P12-T12.3.5-C1", ckP12NotifyService, "notifications with retry + preferences")
	r.Register("P12-T12.3.6-C1", ckP12NotifyPrefs, "notification preferences with quiet hours")
	r.Register("P12-T12.3.7-C1", ckP12WebAuthn, "passkey ceremonies verify challenge/origin/signature/counter")
	r.Register("P12-T12.3.8-C1", ckP12AntiPhish, "anti-phishing code on every official user message")
	r.Register("P12-T12.3.9-C1", ckP12LoginHistory, "login history review + session revocation")
	r.Register("P12-T12.3.10-C1", ckP12SelfFreeze, "self-freeze cancels orders, revokes credentials, requires re-verification")
	r.Register("P12-T12.3.11-C1", ckP12Delegation, "institutional delegated logins + M-of-N validation")
	r.Register("P12-T12.3.12-C1", ckP12LockoutClone, "auth lockout + WebAuthn clone detection + freeze partial-failure")
	r.Register("P12-T12.3.13-C1", ckP12KYCMatrixTax, "KYC ops matrix with SLAs + owned tax inputs (FIFO record)")
}

func ckP12Registration(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/027_alter_users_add_password_hash.up.sql"),
		gotest(p12Auth, "TestRegisterLoginRefreshLogoutIntegration|TestPasswordResetIntegration|TestPendingKeyShape|TestTokenKeyIsDigestOnly"),
		gotest(p12API, "TestAuthnIntegration"),
	)
}

func ckP12TOTP(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p12Auth, "TestTwoFactorLifecycleIntegration|TestTwoFactorBackupCodesSingleUseIntegration|TestBackupCodesShapeAndDigest|TestTwoFactorServiceNilDepsFailClosed"),
		structural(env, "services/internal/auth/totp.go", "pendingKey", "2fa:pending:"),
	)
}

func ckP12Profile(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p12Auth, "TestChangePasswordRevokesOthersIntegration"),
		structural(env, "services/internal/api/profile.go", "AccountAPIKeys", "profile"),
		gotest(p12API, "TestAuthnIntegration"),
	)
}

func ckP12KYCTiers(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/203_kyc_submission.up.sql"),
		gotest(p12Compl, "TestTierRank|TestSubmitHappyPath|TestSubmitT2RequiresFundsDoc|TestSubmitScannerFailClosed|TestITSubmissionLifecycle|TestITMigrationRoundTrip"),
		gotest(p12API, "TestKYCSubmitHappyPath|TestKYCStatus|TestKYCSelfCertTierGate"),
	)
}

func ckP12Reverify(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p12Compl, "TestComputeReverifyDue"),
		structural(env, "services/internal/compliance/kyc.go", "reverify_due_at"),
		structural(env, "services/internal/db/migrations/203_kyc_submission.up.sql", "reverify_due_at"),
	)
}

func ckP12NotifyService(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/028_create_notification_dead_letters.up.sql"),
		gotest(p12Notif, "TestDispatchRetriesThenDeadLetters|TestDispatchTransientErrorRetriesBounded|TestDispatchDelivers|TestNotifyFansOutEnabledChannels|TestPgDeliveryLifecycleAndDeadLetter|TestRedisQueueRoundTrip"),
	)
}

func ckP12NotifyPrefs(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/202_notification_preferences.up.sql"),
		gotest(p12Notif, "TestQuietHours|TestDispatchQuietHoursDefersNonCritical|TestDispatchCriticalBypassesQuietHours|TestPgPreferencesRoundTrip|TestPreferencesMatrixOverride"),
		gotest(p12API, "TestNotificationPreferencesGetDefaults|TestNotificationPreferencesPutThenGet|TestNotificationPreferencesPutValidation"),
	)
}

func ckP12WebAuthn(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/068_webauthn_credentials.up.sql"),
		gotest(p12Auth, "TestWebAuthnRegistrationChallengeSingleUse|TestWebAuthnUnknownChallengeFailsClosed|TestWebAuthnChallengeUserBinding|TestSessionElevateAMRAndReissue|TestRedisWebAuthnChallengeSingleUse|TestPgWebAuthnCredentialStore|TestWebAuthnUserAdapter"),
	)
}

func ckP12AntiPhish(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/070_users_anti_phishing_code.up.sql"),
		gotest(p12Auth, "TestValidateAntiPhishingCode|TestPgAntiPhishingCode"),
		gotest(p12Notif, "TestAntiPhishBanner|TestAntiPhishUnsetBanner"),
	)
}

func ckP12LoginHistory(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/069_login_history.up.sql"),
		gotest(p12Auth, "TestPgLoginHistory|TestLoginCursorRoundTrip|TestSessionRevokeAllExceptKeepsCurrent|TestSessionLookupIsReadOnly"),
	)
}

func ckP12SelfFreeze(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/201_unfreeze_requests.up.sql"),
		gotest(p12Acc, "TestIntegrationEmergencyFreezeHappyPath|TestIntegrationEmergencyFreezeCancelRetriesRecover|TestIntegrationEmergencyFreezeGuards|TestIntegrationUnfreezeRequestFlow"),
	)
}

func ckP12Delegation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/db/migrations/074_client_delegation.up.sql"),
		gotest(p12Deleg, "TestRoleCapabilityMatrix|TestIntegrationDelegationLifecycle|TestIntegrationApprovals|TestIntegrationApprovalExpirySweep|TestIntegrationRevokeAll|TestIntegrationDelegationGuards|TestScopeContainment"),
	)
}

func ckP12LockoutClone(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gotest(p12Auth, "TestRedisLockoutThresholdAndLock|TestWebAuthnCloneResponseRevokesAndFreezes"),
		gotest(p12Acc, "TestIntegrationEmergencyFreezePartialFailure|TestIntegrationEmergencyFreezeCancelExhausted|TestSecurityFreezeUserAccounts"),
	)
}

func ckP12KYCMatrixTax(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/204_kyc_ops_matrix.up.sql",
			"services/internal/db/migrations/205_tax_self_certifications.up.sql",
		),
		gotest(p12Compl, "TestRequirementsMerged|TestOverdueReviews|TestValidateSelfCertW9|TestValidateSelfCertW8Passthrough|TestITSelfCerts"),
		gotest(p12API, "TestKYCRequirements|TestKYCSelfCertBadTIN"),
		structural(env, "services/internal/tax/tax.go", "BookOfRecordMethod", "FIFO"),
	)
}
