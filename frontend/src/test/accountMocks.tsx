/**
 * Account-cluster test helpers (Tasks 10.3.21–10.3.25):
 *   - `installFetchMock` — route-table fetch stub ("METHOD /api/v1/x" →
 *     canned response, `*` suffix = path prefix match, `*` method = any).
 *   - `renderApp` — RTL render inside QueryClientProvider + MemoryRouter.
 *   - `signInForTests` — put the session store into a signed-in state.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MemoryRouter } from 'react-router';
import { vi } from 'vitest';

import { ApiClient } from '@/lib/api';
import { useSessionStore, type SessionUser } from '@/lib/auth/session';

export interface MockRoute {
  status?: number;
  body?: unknown;
  /** Dynamic responder — inspected per request (url, init). */
  handler?: (url: string, init?: RequestInit) => { status?: number; body?: unknown };
}

export function installFetchMock(routes: Record<string, MockRoute>) {
  const calls: { method: string; url: string; init?: RequestInit }[] = [];
  const impl = (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const method = (init?.method ?? 'GET').toUpperCase();
    const path = new URL(url, 'http://test.local').pathname;
    calls.push({ method, url, init });
    const exact = routes[`${method} ${path}`] ?? routes[`* ${path}`];
    const prefixEntry = Object.entries(routes).find(([k]) => {
      if (!k.endsWith('*')) return false;
      const [m, p] = k.slice(0, -1).split(' ', 2);
      return (m === '*' || m === method) && p !== undefined && path.startsWith(p);
    });
    const route = exact ?? prefixEntry?.[1];
    const out = route?.handler ? route.handler(url, init) : route;
    const status = out?.status ?? 200;
    const body = out?.body ?? {};
    return Promise.resolve(
      new Response(typeof body === 'string' ? body : JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  };
  vi.stubGlobal('fetch', vi.fn(impl));
  return calls;
}

export function renderApp(ui: ReactNode, route = '/') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

/**
 * Runtime module replacement for component tests — use as:
 *
 *   vi.mock('@/app/runtime', () => import('@/test/accountMocks').then(m => m.runtimeModule()));
 *
 * apiClient defers to the CURRENT global fetch (so `installFetchMock`
 * works even though @/app/runtime was constructed before the stub) and
 * reads the bearer token straight from the session store. wsClient is a
 * no-op stub — the shell is not rendered in these tests.
 */
export function runtimeModule() {
  const apiClient = new ApiClient({
    baseUrl: '/api/v1',
    fetchImpl: (input, init) => fetch(input, init), // late-bound → honors stubs
    getAuthToken: () => useSessionStore.getState().accessToken,
  });
  const wsStatus = {
    state: 'DISCONNECTED',
    attempt: 0,
    orderEntryEnabled: false,
    subscriptions: [],
    health: {},
    lastError: null,
  };
  const wsClient = {
    start: () => undefined,
    stop: () => undefined,
    subscribe: () => () => undefined,
    unsubscribe: () => undefined,
    onStatusChange: () => () => undefined,
    // A stable snapshot — useSyncExternalStore consumers loop forever on a
    // fresh object per call.
    getStatus: () => wsStatus,
  };
  const sessionRefresher = { accessToken: () => useSessionStore.getState().accessToken };
  return { apiClient, wsClient, sessionRefresher };
}

/** Signed-in session for component tests (bypasses /auth/login). */
export function signInForTests(user: Partial<SessionUser> = {}) {
  useSessionStore.getState().setSession({
    accessToken: 'test-access-token',
    refreshToken: 'test-refresh-token',
    accessTokenExpiresAt: Date.now() + 15 * 60 * 1000,
    persistent: false,
    user: {
      userId: '42',
      email: 'trader@example.com',
      accountId: 1001,
      roles: [],
      kycTier: 'T1',
      ...user,
    },
  });
}
