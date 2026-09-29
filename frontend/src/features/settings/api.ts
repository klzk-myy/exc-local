/**
 * Settings & security-center API — Task 10.3.22.
 *
 * Route registry (services/internal/gateway/routes_v1.go):
 *   GET/PUT  /api/v1/account/profile                       (Phase-12, stub)
 *   POST     /api/v1/account/change-password               (Phase-12, stub)
 *   GET      /api/v1/account/login-history                 (Phase-12, stub)
 *   GET/PUT  /api/v1/account/notifications/preferences     (Phase-12, stub)
 *   PUT      /api/v1/account/settings/anti-phishing-code   (Phase-12, stub)
 *   POST     /api/v1/account/webauthn/{register,authenticate} (Phase-12, stub)
 *   POST     /api/v1/account/emergency-freeze              (Phase-12, stub)
 *   POST     /api/v1/account/unfreeze-request              (Phase-12, stub)
 *   POST     /api/v1/account/cooling-off                   (Phase-14 Task 14.3.11, stub)
 *   POST     /api/v1/account/close                         (Phase-14 Task 14.3.9, stub)
 *   PUT      /api/v1/account/consent                       (Phase-14 Task 14.3.7, stub)
 *   POST     /api/v1/account/gdpr/{export,erase}           (Phase-21 Task 21.3.7, stub)
 *   GET/POST/DELETE /api/v1/developer/api-keys[/{id}]      (Phase-05 Task 5.3.16, LIVE)
 *   GET/POST /api/v1/account/sub-accounts                  (Phase-05 Task 5.3.11, live)
 *   POST     /api/v1/account/sub-accounts/{id}/api-keys    (live)
 *   DELETE   /api/v1/account/sub-accounts/{id}/api-keys/{keyId} (live)
 *
 * Phase-12 routes are Status=Stub (501 NOT_IMPLEMENTED) until the owning
 * task lands — components render the envelope error and stay usable.
 */
import type { ApiClient } from '@/lib/api';

// ---------------------------------------------------------------------------
// Profile
// ---------------------------------------------------------------------------

/** Editable profile fields (Task 10.3.22 item 1). MiFID categorization is
 * server-owned → rendered read-only. */
export interface AccountProfile {
  user_id?: string;
  email?: string;
  display_name?: string;
  phone?: string;
  tax_residency?: string;
  /** MiFID II client categorization — read-only in the UI. */
  mifid_category?: string;
  kyc_tier?: string;
  anti_phishing_code?: string; // masked server-side
}

export async function getProfile(api: ApiClient): Promise<AccountProfile> {
  const res = await api.get<{ profile?: AccountProfile } | AccountProfile>('/account/profile');
  if ('profile' in res && res.profile !== undefined) return res.profile;
  return res as AccountProfile;
}

export async function putProfile(api: ApiClient, p: AccountProfile): Promise<AccountProfile> {
  const res = await api.put<{ profile?: AccountProfile } | AccountProfile>('/account/profile', {
    display_name: p.display_name,
    phone: p.phone,
    tax_residency: p.tax_residency,
  });
  if ('profile' in res && res.profile !== undefined) return res.profile;
  return res as AccountProfile;
}

// ---------------------------------------------------------------------------
// Password + anti-phishing + login history
// ---------------------------------------------------------------------------

export async function changePassword(
  api: ApiClient,
  currentPassword: string,
  newPassword: string,
): Promise<void> {
  await api.post('/account/change-password', {
    current_password: currentPassword,
    new_password: newPassword,
  });
}

/** Anti-phishing code — 4–32 chars (spec §5.16 / Phase-12 Task 12.3.8);
 * backend additionally requires 2FA enabled. */
export const ANTI_PHISHING_MIN = 4;
export const ANTI_PHISHING_MAX = 32;

export function antiPhishingValid(code: string): boolean {
  return code.length >= ANTI_PHISHING_MIN && code.length <= ANTI_PHISHING_MAX;
}

export async function setAntiPhishingCode(api: ApiClient, code: string): Promise<void> {
  await api.put('/account/settings/anti-phishing-code', { code });
}

export interface LoginHistoryEntry {
  id?: string;
  timestamp: string;
  device?: string;
  ip?: string;
  geo_city?: string;
  geo_country?: string;
  success: boolean;
  method?: string;
}

export interface ListEnvelope<T> {
  data: T[];
  next_cursor?: string;
  limit?: number;
  total?: number;
}

export async function loginHistory(
  api: ApiClient,
  cursor?: string,
): Promise<ListEnvelope<LoginHistoryEntry>> {
  const qs = cursor !== undefined && cursor !== '' ? `?cursor=${encodeURIComponent(cursor)}` : '';
  const res = await api.get<ListEnvelope<LoginHistoryEntry> | LoginHistoryEntry[]>(
    `/account/login-history${qs}`,
  );
  if (Array.isArray(res)) return { data: res };
  return { data: res.data, next_cursor: res.next_cursor, limit: res.limit, total: res.total };
}

// ---------------------------------------------------------------------------
// WebAuthn / passkeys (Phase-12 Task 12.3.7)
// ---------------------------------------------------------------------------

/** The registry pins one endpoint per ceremony. Contract assumption
 * (documented deviation): POST with an empty/options-request body returns
 * the PublicKeyCredential options; POST with the browser credential JSON
 * completes the ceremony. Assertion success sets two_factor_verified and
 * JWT amr=["fido2"] server-side. */
export async function webauthnRegisterBegin(
  api: ApiClient,
  friendlyName: string,
): Promise<unknown> {
  return api.post<unknown>('/account/webauthn/register', { stage: 'begin', name: friendlyName });
}

export async function webauthnRegisterFinish(api: ApiClient, credential: unknown, name: string) {
  return api.post<unknown>('/account/webauthn/register', { stage: 'finish', name, credential });
}

export async function webauthnAuthenticateBegin(api: ApiClient): Promise<unknown> {
  return api.post<unknown>('/account/webauthn/authenticate', { stage: 'begin' });
}

export async function webauthnAuthenticateFinish(api: ApiClient, credential: unknown) {
  return api.post<unknown>('/account/webauthn/authenticate', { stage: 'finish', credential });
}

// ---------------------------------------------------------------------------
// Safety: freeze / cooling-off / closure / GDPR / consent
// ---------------------------------------------------------------------------

export async function emergencyFreeze(api: ApiClient): Promise<void> {
  await api.post('/account/emergency-freeze', {});
}

export async function requestUnfreeze(api: ApiClient, reason: string): Promise<void> {
  await api.post('/account/unfreeze-request', { reason });
}

export const COOLING_OFF_DURATIONS = ['24h', '7d', '30d', 'permanent'] as const;
export type CoolingOffDuration = (typeof COOLING_OFF_DURATIONS)[number];

export async function coolingOff(api: ApiClient, duration: CoolingOffDuration): Promise<void> {
  await api.post('/account/cooling-off', { duration });
}

export async function closeAccount(api: ApiClient, confirmation: string): Promise<void> {
  await api.post('/account/close', { confirmation });
}

export interface GdprJob {
  job_id?: string;
  status?: string;
  download_url?: string;
  message?: string;
}

export async function gdprExport(api: ApiClient): Promise<GdprJob> {
  return api.post<GdprJob>('/account/gdpr/export', {});
}

export async function gdprErase(api: ApiClient, confirmation: string): Promise<GdprJob> {
  return api.post<GdprJob>('/account/gdpr/erase', { confirmation });
}

export interface ConsentState {
  marketing: boolean;
  research: boolean;
  third_party: boolean;
}

export async function putConsent(api: ApiClient, c: ConsentState): Promise<void> {
  await api.put('/account/consent', c);
}

// ---------------------------------------------------------------------------
// Notification preferences — channel × event matrix (Task 10.3.22 item 10)
// ---------------------------------------------------------------------------

export const NOTIFICATION_CHANNELS = ['email', 'push', 'sms', 'ws'] as const;
export const NOTIFICATION_EVENTS = [
  'order_fill',
  'funding',
  'security',
  'margin',
  'announcements',
] as const;
export type NotificationChannel = (typeof NOTIFICATION_CHANNELS)[number];
export type NotificationEvent = (typeof NOTIFICATION_EVENTS)[number];

/** event → channel → enabled */
export type NotificationPrefs = Partial<Record<string, Partial<Record<string, boolean>>>>;

export async function getNotificationPrefs(api: ApiClient): Promise<NotificationPrefs> {
  const res = await api.get<unknown>('/account/notifications/preferences');
  const wrapped = res as { preferences?: NotificationPrefs };
  if (wrapped.preferences !== undefined) return wrapped.preferences;
  return res as NotificationPrefs;
}

export async function putNotificationPrefs(
  api: ApiClient,
  prefs: NotificationPrefs,
): Promise<void> {
  await api.put('/account/notifications/preferences', { preferences: prefs });
}

// ---------------------------------------------------------------------------
// API keys — developer (main-account) + sub-account keys
// ---------------------------------------------------------------------------

/** §8.3 scope vocabulary. Sub-account keys may carry read|trade only —
 * transfer/admin are never issuable there (accounts/apikeys.go). */
export const KEY_SCOPES = ['read', 'trade', 'transfer', 'admin'] as const;
export const SUB_ACCOUNT_KEY_SCOPES = ['read', 'trade'] as const;

export interface ApiKeyView {
  key_id: string;
  label: string;
  key_type: string;
  algorithm?: string;
  scopes: string[];
  rate_limit_tier: string;
  ip_allowlist?: string[];
  status: string;
  expires_at?: string | null;
  last_used_at?: string | null;
  created_at: string;
  needs_rotation?: boolean;
}

export interface IssuedDeveloperKey {
  key: ApiKeyView;
  /** Plaintext secret — returned exactly once (HMAC keys only). */
  secret?: string;
  notice?: string;
}

export async function listApiKeys(api: ApiClient): Promise<ApiKeyView[]> {
  const res = await api.get<{ api_keys?: ApiKeyView[] } | ApiKeyView[]>('/developer/api-keys');
  if (Array.isArray(res)) return res;
  return res.api_keys ?? [];
}

export async function createApiKey(
  api: ApiClient,
  input: { label: string; scopes: string[]; rateLimitTier?: string; expiresAt?: string },
): Promise<IssuedDeveloperKey> {
  return api.post<IssuedDeveloperKey>('/developer/api-keys', {
    label: input.label,
    key_type: 'HMAC',
    scopes: input.scopes,
    rate_limit_tier: input.rateLimitTier,
    expires_at: input.expiresAt,
  });
}

export async function revokeApiKey(api: ApiClient, keyId: string): Promise<void> {
  await api.delete(`/developer/api-keys/${encodeURIComponent(keyId)}`);
}

export interface SubAccount {
  id: number;
  master_account_id: number;
  account_type?: string;
  kyc_tier?: string;
  status: string;
  trading_enabled?: boolean;
  created_at: string;
}

export async function listSubAccounts(api: ApiClient): Promise<SubAccount[]> {
  const res = await api.get<{ data?: SubAccount[] } | SubAccount[]>('/account/sub-accounts');
  if (Array.isArray(res)) return res;
  return res.data ?? [];
}

export async function createSubAccount(api: ApiClient): Promise<SubAccount> {
  return api.post<SubAccount>('/account/sub-accounts', {});
}

export interface IssuedSubAccountKey {
  id: number;
  key_id: string;
  secret: string; // one-time plaintext
  account_id: number;
  label: string;
  scopes: string[];
  created_at: string;
}

export async function createSubAccountApiKey(
  api: ApiClient,
  subAccountId: number,
  input: { label: string; scopes: string[] },
): Promise<IssuedSubAccountKey> {
  return api.post<IssuedSubAccountKey>(`/account/sub-accounts/${subAccountId}/api-keys`, {
    scopes: input.scopes,
    label: input.label,
  });
}

export async function revokeSubAccountApiKey(
  api: ApiClient,
  subAccountId: number,
  keyId: number,
): Promise<void> {
  await api.delete(`/account/sub-accounts/${subAccountId}/api-keys/${keyId}`);
}
