/**
 * Silent token refresh — Task 10.3.21 items 5 + 6 (spec §21.4).
 *
 * `SessionRefresher` owns the single source of truth for "is the access
 * token fresh enough" and performs single-flight POST /api/v1/auth/refresh
 * behind three call sites wired in app/runtime.ts:
 *
 *   - `apiClient.getAuthToken`  → `refresher.accessToken()` — preemptive
 *     refresh when the stored token is within `skewMs` of expiry
 *   - `apiClient.onUnauthorized` → `refresher.handleUnauthorized()` —
 *     reactive refresh on a 401 envelope
 *   - `wsClient.tokenRefresher`  → `refresher.refreshForWs()` — the 4019
 *     AUTH_EXPIRED close-code path (§10.5, Task 10.3.19)
 *
 * Refresh posts `{refresh_token}` (the Phase-12 contract) and accepts the
 * tolerant response shape `{access_token|token, refresh_token?,
 * expires_in?|expires_at?}`. Failure semantics are fail-closed (§2.7):
 * the session is cleared, `authEpoch` bumps (flushing optimistic order
 * state), and — in the browser — the app hard-redirects to /login with
 * the current path as the redirect target.
 *
 * The refresher intentionally does NOT go through ApiClient: a 401 on the
 * refresh call itself would re-enter `onUnauthorized` and recurse.
 */
import { useSessionStore } from './session';

export interface RefreshRequestBody {
  refresh_token: string;
}

/** Tolerant response view — Phase-12's handler is mid-flight, so both the
 * `{token}` (runtime placeholder) and `{access_token}` spellings parse. */
export interface RefreshResponseBody {
  access_token?: string;
  token?: string;
  refresh_token?: string;
  expires_in?: number; // seconds
  expires_at?: number | string; // ms epoch or RFC3339
}

export interface SessionRefresherOptions {
  /** Absolute URL, e.g. `${apiBase}/auth/refresh`. */
  refreshUrl: string;
  fetchImpl?: typeof fetch;
  /** Refresh proactively when token expiry is inside this window. */
  skewMs?: number;
  /** Back-to-back failure cooldown so a dead refresh token doesn't
   * hammer the endpoint on every 401 in a burst. */
  failureCooldownMs?: number;
  now?: () => number;
}

export class SessionRefresher {
  private readonly refreshUrl: string;
  private readonly fetchImpl: typeof fetch;
  private readonly skewMs: number;
  private readonly failureCooldownMs: number;
  private readonly now: () => number;
  private inflight: Promise<string | null> | null = null;
  private failedUntil = 0;

  constructor(opts: SessionRefresherOptions) {
    this.refreshUrl = opts.refreshUrl;
    this.fetchImpl = opts.fetchImpl ?? fetch.bind(globalThis);
    this.skewMs = opts.skewMs ?? 30_000;
    this.failureCooldownMs = opts.failureCooldownMs ?? 5_000;
    this.now = opts.now ?? (() => Date.now());
  }

  /** `getAuthToken` seam — returns a usable access token, preemptively
   * refreshing when the stored token is expiring. */
  async accessToken(): Promise<string | null> {
    const s = useSessionStore.getState();
    if (!s.accessToken) return null;
    const exp = s.accessTokenExpiresAt;
    if (exp !== null && exp - this.now() < this.skewMs) {
      return this.refresh();
    }
    return s.accessToken;
  }

  /** `onUnauthorized` seam — a 401 means the gateway rejected the token;
   * try one refresh, else fail closed (clear + redirect). */
  handleUnauthorized(): void {
    void this.refresh().then((token) => {
      if (token === null) this.failClosed();
    });
  }

  /** `tokenRefresher` seam for the WS client — throws on failure so the
   * machine's onAuthFailure path runs. */
  async refreshForWs(): Promise<string> {
    const token = await this.refresh();
    if (token === null) {
      this.failClosed();
      throw new Error('session refresh failed');
    }
    return token;
  }

  /** Single-flight refresh: concurrent callers share one request.
   * Returns the new access token, or null when no refresh token exists
   * or the refresh failed. */
  refresh(): Promise<string | null> {
    if (this.inflight) return this.inflight;
    if (this.now() < this.failedUntil) {
      return Promise.resolve(null);
    }
    const refreshToken = useSessionStore.getState().refreshToken;
    if (!refreshToken) return Promise.resolve(null);
    this.inflight = this.doRefresh(refreshToken).finally(() => {
      this.inflight = null;
    });
    return this.inflight;
  }

  private async doRefresh(refreshToken: string): Promise<string | null> {
    const body: RefreshRequestBody = { refresh_token: refreshToken };
    let res: Response;
    try {
      res = await this.fetchImpl(this.refreshUrl, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        credentials: 'same-origin',
      });
    } catch {
      return null; // transport failure — caller decides (retry later)
    }
    if (!res.ok) {
      if (res.status === 401 || res.status === 403 || res.status === 400) {
        // Refresh token rejected — definitive session end.
        useSessionStore.getState().clearSession();
        this.failedUntil = this.now() + this.failureCooldownMs;
        return null;
      }
      // 5xx/429 — transient; keep the session, back off briefly.
      this.failedUntil = this.now() + this.failureCooldownMs;
      return null;
    }
    let payload: RefreshResponseBody;
    try {
      payload = (await res.json()) as RefreshResponseBody;
    } catch {
      return null;
    }
    const accessToken = payload.access_token ?? payload.token;
    if (typeof accessToken !== 'string' || accessToken.length === 0) return null;
    const expiresAt = normalizeExpiresAt(payload, this.now());
    useSessionStore.getState().applyRefresh(accessToken, payload.refresh_token, expiresAt);
    return accessToken;
  }

  /** Fail-closed boundary: clear the session, bump the auth epoch so
   * optimistic state flushes, and bounce to /login (browser only). */
  private failClosed(): void {
    const s = useSessionStore.getState();
    if (s.accessToken !== null || s.refreshToken !== null) {
      s.clearSession();
    }
    s.bumpAuthEpoch();
    if (typeof window !== 'undefined' && !window.location.pathname.startsWith('/login')) {
      const redirect = encodeURIComponent(window.location.pathname + window.location.search);
      window.location.assign(`/login?redirect=${redirect}`);
    }
  }
}

function normalizeExpiresAt(payload: RefreshResponseBody, now: number): number | null {
  if (typeof payload.expires_in === 'number' && payload.expires_in > 0) {
    return now + payload.expires_in * 1000;
  }
  if (typeof payload.expires_at === 'number') return payload.expires_at;
  if (typeof payload.expires_at === 'string') {
    const t = Date.parse(payload.expires_at);
    return Number.isNaN(t) ? null : t;
  }
  return null; // falls back to the JWT exp claim in applyRefresh
}
