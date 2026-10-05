/**
 * Integrity console tests (Phase-10.5 Task 10.5.3.3) — live-data
 * rendering for audit verify/recon/export/DLQ/deprecation, plus the
 * honest-degraded DLQ path (503 ≠ empty list).
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import IntegrityPage from './IntegrityPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const RECON_LATEST = {
  run: {
    id: 12,
    started_at: '2026-01-02T03:00:00Z',
    finished_at: '2026-01-02T03:00:42Z',
    status: 'INCONCLUSIVE',
    categories_checked: 9,
    findings_count: 2,
    mismatch_count: 0,
    inconclusive_count: 2,
  },
  findings: [
    {
      id: 1,
      run_id: 12,
      category: 'POSITIONS',
      subject: 'engine↔pg',
      leg: 'engine',
      unit: 'state',
      severity: 'INCONCLUSIVE',
      detail: { reason: 'no AdminGetPositions seam' },
    },
    {
      id: 2,
      run_id: 12,
      category: 'BALANCES',
      subject: 'acct:1001:USD',
      leg: 'pg↔projection',
      expected: '1000.00',
      actual: '999.98',
      delta: '-0.02',
      unit: 'amount',
      severity: 'MISMATCH',
      halt_scope: 'ACCOUNT',
      halt_target: '1001',
    },
  ],
};

const BASE = {
  'GET /api/v1/admin/audit-log': { body: { data: [], next_cursor: '', limit: 50, total: 0 } },
  'GET /api/v1/admin/reconciliation/latest': { body: RECON_LATEST },
  'GET /api/v1/admin/reconciliation/runs': { body: { runs: [RECON_LATEST.run] } },
  'GET /api/v1/admin/dlq': { body: { entries: [], count: 0 } },
  'GET /api/v1/admin/api-deprecations/usage': { body: { usage: [] } },
};

const region = (name: string) => screen.getByRole('region', { name });

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('IntegrityPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<IntegrityPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all five panels and stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Read-Only Auditor'] });
    const calls = installFetchMock(BASE);
    renderApp(<IntegrityPage />);
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Audit chain' })).toBeInTheDocument(),
    );
    expect(screen.getByRole('heading', { name: 'Reconciliation' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Records & archive' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Dead-letter queue' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'API deprecations' })).toBeInTheDocument();
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('renders the latest recon run with the mismatch/inconclusive breakdown', async () => {
    signInForTests({ roles: ['Read-Only Auditor'] });
    installFetchMock(BASE);
    renderApp(<IntegrityPage />);
    await waitFor(() => expect(screen.getByText(/run #12/)).toBeInTheDocument());
    const rec = region('Reconciliation');
    expect(within(rec).getByText('POSITIONS')).toBeInTheDocument();
    expect(within(rec).getByText('acct:1001:USD')).toBeInTheDocument();
    expect(within(rec).getByText('ACCOUNT:1001')).toBeInTheDocument();
  });

  it('verifies the audit chain for a date and renders the report badge', async () => {
    signInForTests({ roles: ['Read-Only Auditor'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/audit/verify': {
        body: {
          date: '2026-01-02',
          rows_checked: 341,
          ok: true,
          violations: [],
          merkle: { stored_root: true, verified: true },
          checked_at: '2026-01-02T04:00:00Z',
        },
      },
    });
    renderApp(<IntegrityPage />);
    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByLabelText('Date (UTC)')).toBeInTheDocument());
    await user.type(screen.getByLabelText('Date (UTC)'), '2026-01-02');
    await user.click(screen.getByRole('button', { name: 'Verify chain' }));
    await waitFor(() => expect(screen.getByTestId('verify-report')).toBeInTheDocument());
    expect(screen.getByTestId('verify-report')).toHaveTextContent('341 rows checked');
    const call = calls.find((c) => c.url.includes('/admin/audit/verify'));
    expect(call?.url).toContain('date=2026-01-02');
  });

  it('renders DLQ entries and the honest-degraded state on 503', async () => {
    signInForTests({ roles: ['Read-Only Auditor'] });
    installFetchMock({
      ...BASE,
      'GET /api/v1/admin/dlq': {
        body: {
          entries: [
            {
              seq: 44,
              stream: 'trades',
              consumer: 'settlement-durable',
              subject: 'trades.9.fill',
              reason: 'max_deliver_exceeded',
              failed_at: '2026-01-02T05:00:00Z',
              deliveries: 8,
            },
          ],
          count: 1,
        },
      },
    });
    renderApp(<IntegrityPage />);
    await waitFor(() => expect(screen.getByText('trades.9.fill')).toBeInTheDocument());
    expect(screen.getByText('max_deliver_exceeded')).toBeInTheDocument();
  });

  it('shows SERVICE_DEGRADED on a 503 DLQ instead of an empty list', async () => {
    signInForTests({ roles: ['Read-Only Auditor'] });
    installFetchMock({
      ...BASE,
      'GET /api/v1/admin/dlq': {
        status: 503,
        body: {
          type: 'error',
          error: 'SERVICE_DEGRADED',
          message: 'DLQ store unavailable',
          status: 503,
        },
      },
    });
    renderApp(<IntegrityPage />);
    const dlq = await waitFor(() => region('Dead-letter queue'));
    await waitFor(() => expect(within(dlq).getByRole('alert')).toBeInTheDocument());
    expect(within(dlq).getByText(/DLQ store unavailable/)).toBeInTheDocument();
    expect(within(dlq).queryByText(/DLQ empty/)).not.toBeInTheDocument();
  });

  it('exports an order lifecycle and announces a deprecation', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/order-records/555/export': {
        body: {
          order_id: 555,
          lifecycle: [{ event: 'NEW', at: '2026-01-01T10:00:00Z' }],
          retention: 'order_audit — 1825d',
        },
      },
      'POST /api/v1/admin/api-deprecations': { status: 201, body: { id: 2 } },
    });
    renderApp(<IntegrityPage />);
    const user = userEvent.setup();

    await waitFor(() => expect(screen.getByLabelText('Order id')).toBeInTheDocument());
    await user.type(screen.getByLabelText('Order id'), '555');
    await user.click(screen.getByRole('button', { name: 'Export lifecycle' }));
    await waitFor(() => expect(screen.getByTestId('order-lifecycle')).toBeInTheDocument());
    expect(screen.getByTestId('order-lifecycle')).toHaveTextContent('1825d');

    const dep = region('API deprecations');
    await user.type(within(dep).getByLabelText('Path'), '/api/v1/orders/legacy');
    await user.type(within(dep).getByLabelText(/Sunset/), '2026-08-01T00:00');
    await user.click(within(dep).getByRole('button', { name: 'Announce' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/admin/api-deprecations'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        path: '/api/v1/orders/legacy',
        match_prefix: false,
      });
    });
  });
});
