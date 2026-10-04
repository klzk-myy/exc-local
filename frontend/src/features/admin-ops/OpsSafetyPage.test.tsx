/**
 * Ops-safety console tests (Phase-10.5 Task 10.5.3.1) — contract-level:
 * request path/method/body, X-Admin-Env stamping, confirm-modaled
 * destructive actions, dual-control pending surfaces, access denial.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import OpsSafetyPage from './OpsSafetyPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const SUSPENSION = {
  suspension_id: 7,
  scope: 'INSTRUMENT',
  target_id: 'EUR/USD',
  reason: 'stale oracle',
  state: 'ACTIVE',
  initiated_by: 42,
};

const BASE_GETS = {
  'GET /api/v1/admin/kill-switch': { body: { suspensions: [] } },
  'GET /api/v1/admin/flags': { body: { flags: [] } },
  'GET /api/v1/admin/ip-bans': { body: { bans: [] } },
  'GET /api/v1/admin/maintenance-windows': { body: { data: [] } },
};

const region = (name: string) => screen.getByRole('region', { name });

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'staging', pendingEnv: null });
});

const adminCalls = (calls: { method: string; url: string; init?: RequestInit }[]) =>
  calls.filter((c) => c.url.includes('/admin/'));

describe('OpsSafetyPage gating', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<OpsSafetyPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(adminCalls(calls)).toHaveLength(0);
  });
});

describe('OpsSafetyPage (Risk Manager)', () => {
  it('renders all six panels and stamps X-Admin-Env on every admin call', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock(BASE_GETS);
    renderApp(<OpsSafetyPage />);

    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Kill switch' })).toBeInTheDocument(),
    );
    expect(screen.getByRole('heading', { name: 'Circuit breaker' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Feature flags' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'IP bans & allowlist' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Maintenance windows' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Destructive operations' })).toBeInTheDocument();
    expect(screen.getByTestId('admin-role-badge')).toHaveTextContent('Risk Manager');

    const admin = adminCalls(calls);
    expect(admin.length).toBeGreaterThanOrEqual(4);
    for (const c of admin) {
      const headers = new Headers(c.init?.headers);
      expect(headers.get('X-Admin-Env')).toBe('staging');
    }
  });

  it('trips an instrument-scope kill switch with the wire contract', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE_GETS,
      'POST /api/v1/admin/kill-switch': { body: SUSPENSION },
    });
    renderApp(<OpsSafetyPage />);
    const user = userEvent.setup();

    const ks = await waitFor(() => region('Kill switch'));
    await user.type(within(ks).getByLabelText(/Target/), 'EUR/USD');
    await user.type(within(ks).getByLabelText('Reason'), 'oracle stale');
    await user.click(within(ks).getByRole('button', { name: 'Trip kill switch' }));

    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/api/v1/admin/kill-switch'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toEqual({
        scope: 'INSTRUMENT',
        target_id: 'EUR/USD',
        reason: 'oracle stale',
        approver_id: 0,
      });
    });
  });

  it('requires an approver id before a GLOBAL trip can be submitted', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock(BASE_GETS);
    renderApp(<OpsSafetyPage />);
    const user = userEvent.setup();

    const ks = await waitFor(() => region('Kill switch'));
    await user.selectOptions(within(ks).getByLabelText('Scope'), 'GLOBAL');
    await user.type(within(ks).getByLabelText('Reason'), 'incident');
    expect(within(ks).getByRole('button', { name: 'Trip kill switch' })).toBeDisabled();
    expect(calls.filter((c) => c.method === 'POST' && c.url.includes('kill-switch'))).toHaveLength(
      0,
    );
  });

  it('clears an active suspension through the reset modal', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE_GETS,
      'GET /api/v1/admin/kill-switch': { body: { suspensions: [SUSPENSION] } },
      'POST /api/v1/admin/kill-switch/reset': {
        body: { ...SUSPENSION, state: 'CLEARED' },
      },
    });
    renderApp(<OpsSafetyPage />);
    const user = userEvent.setup();

    await waitFor(() => expect(screen.getByText('stale oracle')).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Reset' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText('Reason'), 'oracle recovered');
    await user.click(within(dialog).getByRole('button', { name: 'Reset suspension' }));

    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/api/v1/admin/kill-switch/reset'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toEqual({
        scope: 'INSTRUMENT',
        target_id: 'EUR/USD',
        reason: 'oracle recovered',
        approver_id: 0,
      });
    });
  });

  it('surfaces circuit-breaker reset as a pending dual-control request', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    installFetchMock({
      ...BASE_GETS,
      'POST /api/v1/admin/circuit-breaker/EUR%2FUSD/reset': {
        status: 202,
        body: { dual_control: 'required', request: { id: 99 } },
      },
    });
    renderApp(<OpsSafetyPage />);
    const user = userEvent.setup();

    const cb = await waitFor(() => region('Circuit breaker'));
    await user.click(within(cb).getByRole('button', { name: 'Reset (4-eyes)' }));
    await waitFor(() =>
      expect(screen.getByText(/dual-control approval — request #99/)).toBeInTheDocument(),
    );
  });

  it('toggles a flag and deletes it only through the confirm modal', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      ...BASE_GETS,
      'GET /api/v1/admin/flags': {
        body: { flags: [{ name: 'new-ticket', enabled: true, rollout_pct: 50 }] },
      },
      'POST /api/v1/admin/flags/new-ticket': {
        body: { name: 'new-ticket', enabled: false },
      },
      'DELETE /api/v1/admin/flags/new-ticket': { body: {} },
    });
    renderApp(<OpsSafetyPage />);
    const user = userEvent.setup();

    const flags = await waitFor(() => region('Feature flags'));
    await waitFor(() => expect(within(flags).getByText('new-ticket')).toBeInTheDocument());
    await user.click(within(flags).getByRole('button', { name: 'Disable' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/admin/flags/new-ticket'),
      );
      expect(JSON.parse(post?.init?.body as string)).toEqual({ enabled: false });
    });

    await user.click(within(flags).getByRole('button', { name: 'Delete' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Delete flag' }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/admin/flags/new-ticket')),
      ).toBe(true),
    );
  });

  it('issues a manual IP ban via PUT with duration_s + env header', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE_GETS,
      'PUT /api/v1/admin/ip-bans/203.0.113.7': {
        body: {
          ip: '203.0.113.7',
          level: 1,
          strikes: 1,
          reason: 'credential stuffing',
          banned_at_ms: 1,
          expires_at_ms: 2,
          actor: 'manual',
        },
      },
    });
    renderApp(<OpsSafetyPage />);
    const user = userEvent.setup();

    const ip = await waitFor(() => region('IP security'));
    await user.type(within(ip).getByLabelText('IP to ban'), '203.0.113.7');
    await user.type(within(ip).getByLabelText('Reason'), 'credential stuffing');
    await user.click(within(ip).getByRole('button', { name: 'Ban IP' }));

    await waitFor(() => {
      const put = calls.find(
        (c) => c.method === 'PUT' && c.url.endsWith('/admin/ip-bans/203.0.113.7'),
      );
      expect(put).toBeDefined();
      expect(JSON.parse(put?.init?.body as string)).toMatchObject({
        duration_s: 7200,
        reason: 'credential stuffing',
      });
      expect(new Headers(put?.init?.headers).get('X-Admin-Env')).toBe('staging');
    });
  });

  it('gates mass cancel behind the typed confirm and posts the scope', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE_GETS,
      'POST /api/v1/admin/orders/mass-cancel': { body: { cancelled: 12 } },
    });
    renderApp(<OpsSafetyPage />);
    const user = userEvent.setup();

    const ops = await waitFor(() => region('Destructive operations'));
    await user.type(within(ops).getByLabelText('Account id (empty = ALL)'), '1001');
    await user.click(within(ops).getByRole('button', { name: 'Mass cancel…' }));
    expect(calls.some((c) => c.url.includes('mass-cancel'))).toBe(false);

    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Mass cancel' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/admin/orders/mass-cancel'),
      );
      expect(JSON.parse(post?.init?.body as string)).toEqual({ account_id: 1001 });
    });
  });

  it('renders the access-denied card for a panel the role cannot read', async () => {
    signInForTests({ roles: ['Support Agent'] });
    installFetchMock({
      ...BASE_GETS,
      'GET /api/v1/admin/flags': {
        status: 403,
        body: { type: 'error', error: 'UNAUTHORIZED_ROLE', message: 'denied', status: 403 },
      },
    });
    renderApp(<OpsSafetyPage />);
    await waitFor(() => expect(screen.getAllByTestId('access-denied').length).toBeGreaterThan(0));
    // Denial is per-panel — the rest of the console still renders.
    expect(screen.getByRole('heading', { name: 'Kill switch' })).toBeInTheDocument();
  });
});
