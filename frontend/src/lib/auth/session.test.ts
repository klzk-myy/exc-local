import { beforeEach, describe, expect, it } from 'vitest';

import { resetSessionForTests, useSessionStore } from './session';

const KEY = 'exc.session.v1';

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
});

describe('session store', () => {
  it('setSession stores tokens, derives expiry from the JWT, persists by remember-me', () => {
    const exp = Math.floor(Date.now() / 1000) + 900;
    const jwt = `h.${btoa(JSON.stringify({ sub: '7', exp, roles: ['Support Agent'] }))}.s`;
    useSessionStore.getState().setSession({
      accessToken: jwt,
      refreshToken: 'rt',
      accessTokenExpiresAt: null,
      user: null,
      persistent: true,
    });
    const s = useSessionStore.getState();
    expect(s.accessToken).toBe(jwt);
    expect(s.accessTokenExpiresAt).toBe(exp * 1000);
    expect(s.user?.roles).toEqual(['Support Agent']);
    expect(s.authEpoch).toBeGreaterThan(0);
    expect(window.localStorage.getItem(KEY)).toContain(jwt);
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
  });

  it('non-persistent sessions land in sessionStorage', () => {
    useSessionStore.getState().setSession({
      accessToken: 'at',
      refreshToken: 'rt',
      accessTokenExpiresAt: null,
      user: null,
      persistent: false,
    });
    expect(window.sessionStorage.getItem(KEY)).toContain('at');
    expect(window.localStorage.getItem(KEY)).toBeNull();
  });

  it('applyRefresh merges rotated tokens', () => {
    useSessionStore.getState().setSession({
      accessToken: 'at1',
      refreshToken: 'rt1',
      accessTokenExpiresAt: 100,
      user: null,
      persistent: false,
    });
    useSessionStore.getState().applyRefresh('at2', 'rt2', 5000);
    const s = useSessionStore.getState();
    expect(s.accessToken).toBe('at2');
    expect(s.refreshToken).toBe('rt2');
    expect(s.accessTokenExpiresAt).toBe(5000);
  });

  it('clearSession wipes everything and bumps the epoch', () => {
    useSessionStore.getState().setSession({
      accessToken: 'at',
      refreshToken: 'rt',
      accessTokenExpiresAt: null,
      user: { userId: '1', email: null, accountId: null, roles: [], kycTier: null },
      persistent: true,
    });
    const epoch = useSessionStore.getState().authEpoch;
    useSessionStore.getState().clearSession();
    const s = useSessionStore.getState();
    expect(s.accessToken).toBeNull();
    expect(s.refreshToken).toBeNull();
    expect(s.user).toBeNull();
    expect(s.authEpoch).toBe(epoch + 1);
    expect(window.localStorage.getItem(KEY)).toBeNull();
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
  });

  it('setAccessToken(null) signs out (WS auth-failure path)', () => {
    useSessionStore.getState().setSession({
      accessToken: 'at',
      refreshToken: 'rt',
      accessTokenExpiresAt: null,
      user: null,
      persistent: false,
    });
    useSessionStore.getState().setAccessToken(null);
    expect(useSessionStore.getState().accessToken).toBeNull();
    expect(useSessionStore.getState().refreshToken).toBeNull();
  });
});
