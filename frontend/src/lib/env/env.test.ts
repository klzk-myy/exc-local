import { beforeEach, describe, expect, it } from 'vitest';

import { ApiClient } from '@/lib/api/client';
import { type ApiError } from '@/lib/api/errors';

import { boundAdminApi } from './client';
import { normalizeAdminEnv } from './env';
import { useAdminEnvStore } from './store';

describe('normalizeAdminEnv', () => {
  it('keeps the canonical vocabulary', () => {
    expect(normalizeAdminEnv('dev')).toBe('dev');
    expect(normalizeAdminEnv('staging')).toBe('staging');
    expect(normalizeAdminEnv('production')).toBe('production');
  });

  it('fails closed to production on unknown input', () => {
    expect(normalizeAdminEnv('prod')).toBe('production');
    expect(normalizeAdminEnv('')).toBe('production');
    expect(normalizeAdminEnv(undefined)).toBe('production');
    expect(normalizeAdminEnv(42)).toBe('production');
  });
});

describe('useAdminEnvStore', () => {
  beforeEach(() => {
    sessionStorage.clear();
    useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
  });

  it('switches non-production contexts immediately', () => {
    useAdminEnvStore.getState().requestEnvChange('staging');
    expect(useAdminEnvStore.getState().env).toBe('staging');
    expect(useAdminEnvStore.getState().pendingEnv).toBeNull();
  });

  it('stages a production switch until confirmed', () => {
    useAdminEnvStore.getState().requestEnvChange('production');
    expect(useAdminEnvStore.getState().env).toBe('dev');
    expect(useAdminEnvStore.getState().pendingEnv).toBe('production');
    useAdminEnvStore.getState().confirmEnvChange();
    expect(useAdminEnvStore.getState().env).toBe('production');
    expect(useAdminEnvStore.getState().pendingEnv).toBeNull();
  });

  it('cancel abandons a staged production switch', () => {
    useAdminEnvStore.getState().requestEnvChange('production');
    useAdminEnvStore.getState().cancelEnvChange();
    expect(useAdminEnvStore.getState().env).toBe('dev');
    expect(useAdminEnvStore.getState().pendingEnv).toBeNull();
  });
});

describe('boundAdminApi', () => {
  function makeApi() {
    const calls: { url: string; init: RequestInit }[] = [];
    const fetchImpl = (url: string, init?: RequestInit): Promise<Response> => {
      calls.push({ url, init: init ?? {} });
      return Promise.resolve(new Response('{"ok":true}', { status: 200 }));
    };
    const api = new ApiClient({ baseUrl: '/api/v1', fetchImpl: fetchImpl as typeof fetch });
    return { api, calls };
  }

  it('stamps X-Admin-Env on GET and POST', async () => {
    const { api, calls } = makeApi();
    const bound = boundAdminApi(api, 'staging');
    await bound.get('/admin/fleet/hosts', { state: 'ACTIVE' });
    await bound.post('/admin/releases/1/promote', { to_env: 'production' });
    const h1 = new Headers(calls[0]!.init.headers);
    const h2 = new Headers(calls[1]!.init.headers);
    expect(h1.get('X-Admin-Env')).toBe('staging');
    expect(h2.get('X-Admin-Env')).toBe('staging');
    expect(calls[0]!.url).toBe('/api/v1/admin/fleet/hosts?state=ACTIVE');
  });

  it('callers cannot override the bound env via headers', async () => {
    const { api, calls } = makeApi();
    const bound = boundAdminApi(api, 'dev');
    await bound.post(
      '/admin/fleet/hosts/1/drain',
      { reason: 'x' },
      {
        headers: { 'X-Admin-Env': 'production' },
      },
    );
    expect(new Headers(calls[0]!.init.headers).get('X-Admin-Env')).toBe('dev');
  });

  it('propagates RFC 7807 error envelopes', async () => {
    const api = new ApiClient({
      baseUrl: '/api/v1',
      fetchImpl: () =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              type: 'error',
              error: 'FORBIDDEN',
              message: 'no binding valid in environment production',
              status: 403,
            }),
            { status: 403 },
          ),
        ),
    });
    const bound = boundAdminApi(api, 'production');
    await expect(bound.get('/admin/fleet/hosts')).rejects.toMatchObject({
      code: 'FORBIDDEN',
      status: 403,
    } satisfies Partial<ApiError>);
  });
});
