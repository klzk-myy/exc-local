import { beforeEach, describe, expect, it, vi } from 'vitest';

import { SessionRefresher } from './refresh';
import { resetSessionForTests, useSessionStore } from './session';

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function signIn(opts: { expiresAt?: number | null } = {}) {
  useSessionStore.getState().setSession({
    accessToken: 'old-access',
    refreshToken: 'rt-1',
    accessTokenExpiresAt: opts.expiresAt === undefined ? Date.now() + 900_000 : opts.expiresAt,
    user: null,
    persistent: false,
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
});

describe('SessionRefresher', () => {
  it('accessToken() returns the stored token when fresh', async () => {
    signIn();
    const fetchMock = vi.fn<typeof fetch>();
    const r = new SessionRefresher({ refreshUrl: '/api/v1/auth/refresh', fetchImpl: fetchMock });
    expect(await r.accessToken()).toBe('old-access');
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('accessToken() preemptively refreshes an expiring token', async () => {
    signIn({ expiresAt: Date.now() + 5_000 }); // inside 30s skew
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValue(
        jsonResponse(200, { access_token: 'fresh', refresh_token: 'rt-2', expires_in: 900 }),
      );
    const r = new SessionRefresher({ refreshUrl: '/api/v1/auth/refresh', fetchImpl: fetchMock });
    expect(await r.accessToken()).toBe('fresh');
    const s = useSessionStore.getState();
    expect(s.accessToken).toBe('fresh');
    expect(s.refreshToken).toBe('rt-2');
    const [, init] = fetchMock.mock.calls[0]!;
    expect(JSON.parse(init?.body as string)).toEqual({ refresh_token: 'rt-1' });
  });

  it('refresh is single-flight — concurrent callers share one POST', async () => {
    signIn({ expiresAt: 0 });
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValue(jsonResponse(200, { token: 'fresh', refresh_token: 'rt-2' }));
    const r = new SessionRefresher({ refreshUrl: '/api/v1/auth/refresh', fetchImpl: fetchMock });
    const [a, b] = await Promise.all([r.refresh(), r.refresh()]);
    expect(a).toBe('fresh');
    expect(b).toBe('fresh');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('401 on refresh clears the session (fail closed)', async () => {
    signIn({ expiresAt: 0 });
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValue(
        jsonResponse(401, { type: 'error', error: 'AUTH_EXPIRED', message: 'x', status: 401 }),
      );
    const r = new SessionRefresher({ refreshUrl: '/api/v1/auth/refresh', fetchImpl: fetchMock });
    expect(await r.refresh()).toBeNull();
    expect(useSessionStore.getState().accessToken).toBeNull();
    expect(useSessionStore.getState().refreshToken).toBeNull();
  });

  it('refreshForWs throws when refresh fails', async () => {
    signIn({ expiresAt: 0 });
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(401, {}));
    const r = new SessionRefresher({ refreshUrl: '/api/v1/auth/refresh', fetchImpl: fetchMock });
    await expect(r.refreshForWs()).rejects.toThrow('session refresh failed');
  });

  it('no refresh token → null without calling fetch', async () => {
    resetSessionForTests();
    const fetchMock = vi.fn<typeof fetch>();
    const r = new SessionRefresher({ refreshUrl: '/api/v1/auth/refresh', fetchImpl: fetchMock });
    expect(await r.refresh()).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
