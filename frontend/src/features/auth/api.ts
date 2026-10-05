/**
 * Auth API — Task 10.3.21. Typed seams over the Phase-05/Phase-12 auth
 * surface (route registry: services/internal/gateway/routes_v1.go):
 *
 *   POST /api/v1/auth/login          {email,password[,totp_code,challenge]}
 *   POST /api/v1/auth/register       {email,password,country,accept_terms}
 *   POST /api/v1/auth/logout         (revokes the current session)
 *   POST /api/v1/auth/refresh        (owned by src/lib/auth/refresh.ts)
 *   POST /api/v1/auth/2fa/setup      → TOTP secret + otpauth URI
 *   POST /api/v1/auth/2fa/verify     {code} (activates; first-code proof)
 *   POST /api/v1/auth/2fa/disable    {password, code}
 *   GET  /api/v1/account/sessions    active sessions
 *   DELETE /api/v1/account/sessions/{id} | /account/sessions (all)
 *   POST /api/v1/auth/forgot-password | /auth/reset-password  (Phase-12
 *        password-reset flow — registered path names pending; see note)
 *
 * Login is two-phase: the first response may carry `requires_totp` — the
 * UI then collects the TOTP code and resubmits it alongside the pending
 * challenge token (`challenge`/`mfa_token`/`pending_token` spellings are
 * all normalized here).
 */
import type { ApiClient } from '@/lib/api';
import { useSessionStore, type SessionUser } from '@/lib/auth/session';
import { decodeJwtClaims, tokenRoles } from '@/lib/auth/jwt';

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

export interface LoginInput {
  email: string;
  password: string;
  rememberMe: boolean;
  /** Second-phase TOTP submission. */
  totpCode?: string;
  challenge?: string;
}

export type LoginResult = { kind: 'authenticated' } | { kind: 'totp'; challenge: string };

interface RawLoginResponse {
  access_token?: string;
  token?: string;
  refresh_token?: string;
  expires_in?: number;
  expires_at?: number | string;
  requires_totp?: boolean;
  totp_required?: boolean;
  two_factor_required?: boolean;
  challenge?: string;
  mfa_token?: string;
  pending_token?: string;
  // Gateway flat shape (services/internal/api/authn.go sessionBundleJSON):
  // identity + admin-role hint ride top-level keys, not a nested `user`
  // object. Accepted here so the §8.2 role hint reaches the session.
  roles?: string[];
  role?: string;
  user_id?: number | string;
  email?: string;
  account_id?: number;
  kyc_tier?: string;
  user?: {
    id?: number | string;
    user_id?: number | string;
    email?: string;
    account_id?: number;
    roles?: string[];
    role?: string;
    kyc_tier?: string;
  };
}

function userFrom(res: RawLoginResponse, accessToken: string): SessionUser {
  const claims = decodeJwtClaims(accessToken);
  const u = res.user;
  // Nested `user` object wins; gateway flat top-level keys are the
  // fallback; JWT claims are last. Any one carrying a §8.2 name lets
  // useAdminRole() gate admin surfaces (server still re-authorizes).
  const roles =
    u?.roles ??
    (u?.role !== undefined ? [u.role] : undefined) ??
    res.roles ??
    (res.role !== undefined ? [res.role] : undefined) ??
    tokenRoles(accessToken);
  const userId =
    u?.id !== undefined
      ? String(u.id)
      : u?.user_id !== undefined
        ? String(u.user_id)
        : res.user_id !== undefined
          ? String(res.user_id)
          : (claims?.sub ?? null);
  return {
    userId,
    email: u?.email ?? res.email ?? claims?.email ?? null,
    accountId: u?.account_id ?? res.account_id ?? claims?.account_id ?? null,
    roles,
    kycTier: u?.kyc_tier ?? res.kyc_tier ?? claims?.kyc_tier ?? null,
  };
}

function expiresAtMs(res: RawLoginResponse, now: number): number | null {
  if (typeof res.expires_in === 'number' && res.expires_in > 0) {
    return now + res.expires_in * 1000;
  }
  if (typeof res.expires_at === 'number') return res.expires_at;
  if (typeof res.expires_at === 'string') {
    const t = Date.parse(res.expires_at);
    return Number.isNaN(t) ? null : t;
  }
  return null; // session store falls back to the JWT exp claim
}

export async function login(api: ApiClient, input: LoginInput): Promise<LoginResult> {
  const body: Record<string, unknown> = { email: input.email, password: input.password };
  if (input.totpCode !== undefined) body['totp_code'] = input.totpCode;
  if (input.challenge !== undefined) body['challenge'] = input.challenge;
  const res = await api.post<RawLoginResponse>('/auth/login', body);

  if (
    res.requires_totp === true ||
    res.totp_required === true ||
    res.two_factor_required === true
  ) {
    return {
      kind: 'totp',
      challenge: res.challenge ?? res.mfa_token ?? res.pending_token ?? '',
    };
  }
  const accessToken = res.access_token ?? res.token;
  if (typeof accessToken !== 'string' || accessToken.length === 0) {
    throw new Error('login succeeded without an access token');
  }
  useSessionStore.getState().setSession({
    accessToken,
    refreshToken: res.refresh_token ?? null,
    accessTokenExpiresAt: expiresAtMs(res, Date.now()),
    user: userFrom(res, accessToken),
    persistent: input.rememberMe,
  });
  return { kind: 'authenticated' };
}

// ---------------------------------------------------------------------------
// Registration + password reset
// ---------------------------------------------------------------------------

export interface RegisterResult {
  /** True when the account needs the emailed verification link confirmed. */
  emailVerificationRequired: boolean;
}

export async function register(
  api: ApiClient,
  input: { email: string; password: string; country: string; acceptTerms: boolean },
): Promise<RegisterResult> {
  const res = await api.post<{
    email_verification_required?: boolean;
    verification_required?: boolean;
    requires_email_verification?: boolean;
  }>('/auth/register', {
    email: input.email,
    password: input.password,
    country: input.country,
    accept_terms: input.acceptTerms,
  });
  return {
    emailVerificationRequired:
      res.email_verification_required ??
      res.verification_required ??
      res.requires_email_verification ??
      true,
  };
}

/** Password-reset request — 1h email-link expiry (Phase-12 Task 12.3.1).
 * NOTE: the route registry does not pin a forgot/reset path yet; the
 * conventional `/auth/{forgot,reset}-password` pair is used and flagged
 * as a deviation in the delivery report. */
export async function forgotPassword(api: ApiClient, email: string): Promise<void> {
  await api.post('/auth/forgot-password', { email });
}

export async function resetPassword(
  api: ApiClient,
  input: { token: string; password: string },
): Promise<void> {
  await api.post('/auth/reset-password', { token: input.token, password: input.password });
}

// ---------------------------------------------------------------------------
// Logout + session management
// ---------------------------------------------------------------------------

/** Server-side session revocation, then local wipe. The POST is
 * best-effort — a dead network must not trap the user in a session. */
export async function logout(api: ApiClient): Promise<void> {
  try {
    await api.post('/auth/logout', {});
  } catch {
    // best-effort revocation; local clear proceeds regardless
  } finally {
    useSessionStore.getState().clearSession();
  }
}

/** Active session row — mirrors auth.Session JSON (session.go). */
export interface SessionInfo {
  id: string;
  user_id?: string;
  account_id?: number;
  device?: string;
  ip?: string;
  user_agent?: string;
  geo_city?: string;
  geo_country?: string;
  amr?: string[];
  created_at: string;
  last_active_at: string;
  expires_at: string;
  current?: boolean;
}

export async function listSessions(api: ApiClient): Promise<SessionInfo[]> {
  const res = await api.get<{ data?: SessionInfo[]; sessions?: SessionInfo[] } | SessionInfo[]>(
    '/account/sessions',
  );
  if (Array.isArray(res)) return res;
  return res.data ?? res.sessions ?? [];
}

export async function revokeSession(api: ApiClient, sessionId: string): Promise<void> {
  await api.delete(`/account/sessions/${encodeURIComponent(sessionId)}`);
}

/** Revoke every OTHER session (Phase-12 Task 12.3.9 semantics). */
export async function revokeAllSessions(api: ApiClient): Promise<void> {
  await api.delete('/account/sessions');
}

// ---------------------------------------------------------------------------
// TOTP 2FA (Phase-12 Task 12.3.2)
// ---------------------------------------------------------------------------

export interface TotpSetup {
  secret: string;
  otpauthUri: string;
  /** Present when the backend re-enrollment returns them early. */
  backupCodes: string[];
}

function parseTotpSetup(res: {
  secret?: string;
  otpauth_uri?: string;
  otpauth_url?: string;
  qr_uri?: string;
  backup_codes?: string[];
}): TotpSetup {
  const uri = res.otpauth_uri ?? res.otpauth_url ?? res.qr_uri ?? '';
  return {
    secret: res.secret ?? '',
    otpauthUri: uri,
    backupCodes: res.backup_codes ?? [],
  };
}

/** Re-stage a 2FA candidate secret — POST /auth/2fa/setup. */
export async function totpSetup(api: ApiClient): Promise<TotpSetup> {
  return parseTotpSetup(
    await api.post<Parameters<typeof parseTotpSetup>[0]>('/auth/2fa/setup', {}),
  );
}

/** Start a fresh enrollment ceremony — POST /auth/2fa/enroll. The route
 * registry documents the same stage-a-candidate semantics as /setup
 * (non-destructive until /verify proves possession); the distinct verb is
 * used for the initial "enable" action while /setup re-stages a secret. */
export async function totpEnroll(api: ApiClient): Promise<TotpSetup> {
  return parseTotpSetup(
    await api.post<Parameters<typeof parseTotpSetup>[0]>('/auth/2fa/enroll', {}),
  );
}

export interface TotpVerifyResult {
  backupCodes: string[];
}

/** Activate 2FA — proves possession of the candidate secret (the
 * non-destructive re-enrollment invariant, remediation #38). */
export async function totpVerify(api: ApiClient, code: string): Promise<TotpVerifyResult> {
  const res = await api.post<{ backup_codes?: string[] }>('/auth/2fa/verify', { code });
  return { backupCodes: res.backup_codes ?? [] };
}

export async function totpDisable(api: ApiClient, password: string, code: string): Promise<void> {
  await api.post('/auth/2fa/disable', { password, code });
}

// ---------------------------------------------------------------------------
// Email verification + passkey sign-in (Phase-12 Tasks 12.3.5/12.3.7)
// ---------------------------------------------------------------------------

/** Consume an emailed verification token — POST /auth/verify-email.
 * The route registry exposes no resend endpoint: an expired or invalid
 * token surfaces the backend error code verbatim (fail-visible). */
export async function verifyEmail(
  api: ApiClient,
  token: string,
): Promise<{ userId: number | string | null }> {
  const res = await api.post<{ user_id?: number | string; email_verified?: boolean }>(
    '/auth/verify-email',
    { token },
  );
  return { userId: res.user_id ?? null };
}

export interface PasskeyAssertChallenge {
  challengeId: string;
  /** WebAuthn request options; base64 fields converted by
   * lib/auth/webauthn.toRequestOptions. */
  publicKey: Record<string, unknown>;
}

/** Passkey sign-in, step 1 — POST /auth/passkey/assert with no
 * challenge_id returns a fresh discoverable-credential challenge. */
export async function passkeyAssertBegin(api: ApiClient): Promise<PasskeyAssertChallenge> {
  const res = await api.post<{
    challenge_id?: string;
    publicKey?: Record<string, unknown>;
  }>('/auth/passkey/assert', {});
  if (typeof res.challenge_id !== 'string' || res.challenge_id === '') {
    throw new Error('passkey challenge missing challenge_id');
  }
  return { challengeId: res.challenge_id, publicKey: res.publicKey ?? {} };
}

/** Passkey sign-in, step 2 — POST /auth/passkey/assert with the
 * challenge_id + browser assertion. Success issues a full session bundle
 * (amr ["fido2"], two_factor_verified) — stored here like a login. */
export async function passkeyAssertFinish(
  api: ApiClient,
  challengeId: string,
  credential: Record<string, unknown>,
  persistent: boolean,
): Promise<void> {
  const res = await api.post<RawLoginResponse>('/auth/passkey/assert', {
    challenge_id: challengeId,
    credential,
  });
  const accessToken = res.access_token ?? res.token;
  if (typeof accessToken !== 'string' || accessToken.length === 0) {
    throw new Error('passkey assertion succeeded without an access token');
  }
  useSessionStore.getState().setSession({
    accessToken,
    refreshToken: res.refresh_token ?? null,
    accessTokenExpiresAt: expiresAtMs(res, Date.now()),
    user: userFrom(res, accessToken),
    persistent,
  });
}
