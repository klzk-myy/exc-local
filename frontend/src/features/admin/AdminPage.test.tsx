import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import AdminPage from './AdminPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const OK_TICKETS = {
  data: [
    {
      ticket_id: 9,
      account_id: 1001,
      type: 'QUERY',
      category: 'funding',
      priority: 'HIGH',
      subject: 'Wire missing',
      status: 'OPEN',
      queue: 'funding',
      sla_breached: true,
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    },
  ],
  next_cursor: '',
  limit: 50,
  total: 1,
};

const OK_AUDIT = {
  data: [
    {
      id: 5,
      admin_user_id: 7,
      action: 'user.freeze',
      target_type: 'account',
      target_id: 1001,
      ip_address: '10.0.0.1',
      created_at: '2026-01-02T00:00:00Z',
    },
  ],
  next_cursor: '',
  limit: 50,
  total: 1,
};

const OK_STATUS = {
  status: 'operational',
  mode: 'Normal',
  components: [{ name: 'gateway', state: 'operational', critical: true, latency_ms: 1.2 }],
  source: 'aggregator',
  updated_at: '2026-01-02T00:00:00Z',
};

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('RequireAdmin gate', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<AdminPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('redirects signed-out users to /login', async () => {
    renderApp(
      <Routes>
        <Route path="/admin" element={<AdminPage />} />
        <Route path="/login" element={<div>login destination</div>} />
      </Routes>,
      '/admin',
    );
    await waitFor(() => expect(screen.getByText('login destination')).toBeInTheDocument());
  });
});

describe('AdminPage (Super Admin)', () => {
  it('renders all dashboard panels and stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      'GET /api/v1/system/status': { body: OK_STATUS },
      'GET /api/v1/admin/ops/health': {
        body: { mode: 'Normal', components: { gateway: { state: 'up' } }, recent_events: [] },
      },
      'GET /api/v1/admin/support/tickets': { body: OK_TICKETS },
      'GET /api/v1/admin/dual-control': { body: { requests: [] } },
      'GET /api/v1/admin/audit-log': { body: OK_AUDIT },
    });
    renderApp(<AdminPage />);

    await waitFor(() => expect(screen.getByText('Wire missing')).toBeInTheDocument());
    expect(screen.getByText('user.freeze')).toBeInTheDocument();
    expect(screen.getByTestId('system-status')).toHaveTextContent('operational');
    expect(screen.getByTestId('admin-role-badge')).toHaveTextContent('Super Admin');
    for (const pill of screen.getAllByTestId('env-pill')) {
      expect(pill).toHaveTextContent('DEV');
    }

    // Every admin call is env-bound.
    const adminCalls = calls.filter((c) => c.url.includes('/admin/'));
    expect(adminCalls.length).toBeGreaterThan(0);
    for (const c of adminCalls) {
      const headers = new Headers(c.init?.headers);
      expect(headers.get('X-Admin-Env')).toBe('dev');
    }
  });

  it('renders the 403 surface when the server denies a panel', async () => {
    signInForTests({ roles: ['Super Admin'] });
    installFetchMock({
      'GET /api/v1/system/status': { body: OK_STATUS },
      'GET /api/v1/admin/support/tickets': {
        status: 403,
        body: {
          type: 'error',
          error: 'UNAUTHORIZED_ROLE',
          message: 'support queue requires Support Agent or auditor access',
          status: 403,
        },
      },
      'GET /api/v1/admin/dual-control': { body: { requests: [] } },
      'GET /api/v1/admin/audit-log': { body: OK_AUDIT },
      'GET /api/v1/admin/ops/health': { body: { mode: 'Normal' } },
    });
    renderApp(<AdminPage />);
    await waitFor(() => expect(screen.getAllByTestId('access-denied').length).toBeGreaterThan(0));
  });

  it('user lookup surfaces the support dossier', async () => {
    signInForTests({ roles: ['Support Agent'] });
    installFetchMock({
      'GET /api/v1/system/status': { body: OK_STATUS },
      'GET /api/v1/admin/support/tickets': {
        body: { data: [], next_cursor: '', limit: 50, total: 0 },
      },
      'GET /api/v1/admin/dual-control': { body: { requests: [] } },
      'GET /api/v1/admin/audit-log': { body: { data: [], next_cursor: '', limit: 50, total: 0 } },
      'GET /api/v1/admin/ops/health': { body: { mode: 'Normal' } },
      'GET /api/v1/admin/support/accounts/1001': {
        body: {
          account_id: 1001,
          user_id: 42,
          status: 'active',
          kyc_tier: 'T1',
          account_type: 'RETAIL',
          created_at: '2025-01-01T00:00:00Z',
          balances: [{ currency: 'USD', available: '1000.00', locked: '0' }],
          recent_orders: [],
          recent_tickets: [],
          kyc_documents: [],
        },
      },
    });
    renderApp(<AdminPage />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Account id'), '1001');
    await user.click(screen.getByRole('button', { name: 'Look up' }));
    await waitFor(() => expect(screen.getByTestId('support-view')).toBeInTheDocument());
    expect(screen.getByText(/USD: 1000\.00 available/)).toBeInTheDocument();
  });
});
