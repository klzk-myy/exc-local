/**
 * ApiClient tests — mocked fetch: auth-header injection, RFC 7807 envelope
 * mapping (§8.7/§23), Idempotency-Key (§8.8), query serialization.
 */
import { describe, expect, it, vi } from 'vitest';

import { ApiClient } from './client';
import { ApiError, NetworkError } from './errors';
import { newIdempotencyKey } from './idempotency';

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(typeof body === 'string' ? body : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

type ApiOverrides = Omit<ConstructorParameters<typeof ApiClient>[0], 'baseUrl' | 'fetchImpl'>;

function harness(fetchImpl: typeof fetch, opts: ApiOverrides = {}) {
  return new ApiClient({ baseUrl: 'https://api.test/api/v1', fetchImpl, ...opts });
}

describe('requests', () => {
  it('injects Authorization: Bearer from the token provider', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(200, { ok: true }));
    const api = harness(fetchMock, { getAuthToken: () => 'tok123' });
    await api.get('/account');
    const [, init] = fetchMock.mock.calls[0]!;
    expect(new Headers(init?.headers).get('Authorization')).toBe('Bearer tok123');
  });

  it('serializes query params, dropping undefined', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(200, []));
    const api = harness(fetchMock);
    await api.get('/orders', { query: { symbol: 'EUR/USD', limit: 50, cursor: undefined } });
    const [url] = fetchMock.mock.calls[0]!;
    expect(url).toBe('https://api.test/api/v1/orders?symbol=EUR%2FUSD&limit=50');
  });

  it('attaches a fresh Idempotency-Key on idempotent POSTs', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(200, { id: 'w1' }));
    const api = harness(fetchMock);
    await api.post('/withdrawals', { amount: '100' }, { idempotent: true });
    const [, init] = fetchMock.mock.calls[0]!;
    const key = new Headers(init?.headers).get('Idempotency-Key');
    expect(key).toMatch(/^[0-9a-f-]{36}$/);
  });

  it('honors an explicit Idempotency-Key for true retries', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(200, {}));
    const api = harness(fetchMock);
    await api.post('/transfers', {}, { idempotencyKey: 'fixed-key' });
    const [, init] = fetchMock.mock.calls[0]!;
    expect(new Headers(init?.headers).get('Idempotency-Key')).toBe('fixed-key');
  });

  it('does NOT attach Idempotency-Key on plain POSTs', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(200, {}));
    const api = harness(fetchMock);
    await api.post('/orders/test', {});
    const [, init] = fetchMock.mock.calls[0]!;
    expect(new Headers(init?.headers).get('Idempotency-Key')).toBeNull();
  });
});

describe('error envelope (RFC 7807, spec §8.7)', () => {
  it('maps {type:error,error,message,status,request_id} → ApiError', async () => {
    const envelope = {
      type: 'error',
      error: 'INSUFFICIENT_BALANCE',
      message: 'Not enough available balance',
      status: 400,
      request_id: 'req-abc',
      timestamp: '2026-01-01T00:00:00Z',
    };
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(400, envelope));
    const api = harness(fetchMock);
    const err = await api.post('/orders', {}).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    const apiErr = err as ApiError;
    expect(apiErr.code).toBe('INSUFFICIENT_BALANCE');
    expect(apiErr.status).toBe(400);
    expect(apiErr.requestId).toBe('req-abc');
    expect(apiErr.message).toBe('Not enough available balance');
  });

  it('falls back to a status-derived code for non-envelope bodies', async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValue(new Response('<html>bad gateway</html>', { status: 503 }));
    const api = harness(fetchMock);
    const err = (await api.get('/x').catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err.code).toBe('DEGRADED_MODE');
    expect(err.status).toBe(503);
  });

  it('invokes onUnauthorized on 401', async () => {
    const seen: string[] = [];
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(
      jsonResponse(401, {
        type: 'error',
        error: 'UNAUTHORIZED',
        message: 'bad token',
        status: 401,
      }),
    );
    const api = harness(fetchMock, { onUnauthorized: (e) => seen.push(e.code) });
    await expect(api.get('/account')).rejects.toBeInstanceOf(ApiError);
    expect(seen).toEqual(['UNAUTHORIZED']);
  });

  it('throws NetworkError on transport failure', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockRejectedValue(new TypeError('fetch failed'));
    const api = harness(fetchMock);
    const err = (await api.get('/x').catch((e: unknown) => e)) as NetworkError;
    expect(err).toBeInstanceOf(NetworkError);
    expect(err.code).toBe('NETWORK_ERROR');
  });

  it('returns undefined for 204 No Content', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response(null, { status: 204 }));
    const api = harness(fetchMock);
    await expect(api.delete('/orders/o1')).resolves.toBeUndefined();
  });
});

describe('idempotency keys', () => {
  it('newIdempotencyKey mints RFC 4122 v4 UUIDs', () => {
    const k = newIdempotencyKey();
    expect(k).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    expect(newIdempotencyKey()).not.toBe(k);
  });
});
