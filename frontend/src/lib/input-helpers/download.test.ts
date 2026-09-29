import { afterEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '@/lib/api';
import { useSessionStore } from '@/lib/auth/session';

import { downloadFile } from './download';

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  useSessionStore.setState({ accessToken: null });
});

describe('downloadFile', () => {
  it('returns blob + filename from Content-Disposition', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(new Blob(['csv-data']), {
          status: 200,
          headers: {
            'Content-Type': 'text/csv',
            'Content-Disposition': 'attachment; filename="statement-2026-09.csv"',
          },
        }),
      ),
    );
    const r = await downloadFile('/account/statements', 'statement.csv', {
      query: { from: '2026-09-01' },
    });
    expect(r.filename).toBe('statement-2026-09.csv');
    expect(r.contentType).toContain('text/csv');
    expect(vi.mocked(fetch).mock.calls[0]?.[0]).toContain('from=2026-09-01');
  });

  it('sends the session bearer token when present', async () => {
    useSessionStore.setState({ accessToken: 'tok-123' });
    const fetchMock = vi.fn().mockResolvedValue(new Response(new Blob(['x']), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    await downloadFile('/tax/report', 'tax.csv');
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect((init.headers as Record<string, string>)['Authorization']).toBe('Bearer tok-123');
  });

  it('maps an RFC 7807 error body to ApiError (501 stub surfaces)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(501, {
          type: 'error',
          error: 'NOT_IMPLEMENTED',
          message: 'route registered, handler pending',
          status: 501,
        }),
      ),
    );
    try {
      await downloadFile('/account/statements', 'x.csv');
      expect.unreachable('should throw');
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError);
      expect((e as ApiError).code).toBe('NOT_IMPLEMENTED');
      expect((e as ApiError).status).toBe(501);
    }
  });
});
