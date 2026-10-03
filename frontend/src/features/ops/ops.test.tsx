import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import OpsBoardPage from './OpsBoardPage';
import FleetPage from './FleetPage';
import ReleasesPage from './ReleasesPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const STATUS = {
  status: 'operational',
  mode: 'Normal',
  components: [
    { name: 'gateway', state: 'operational', critical: true, latency_ms: 1.4 },
    { name: 'matcher-0', state: 'degraded', critical: true, latency_ms: 9.9, detail: 'lagging' },
  ],
  source: 'aggregator',
  updated_at: '2026-01-02T00:00:00Z',
};

const HOSTS = {
  hosts: [
    {
      id: 7,
      env: 'dev',
      hostname: 'dev-matcher-0',
      role: 'matcher',
      shard_id: 0,
      az: 'az-a',
      health: 'healthy',
      state: 'ACTIVE',
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    },
  ],
  env: 'dev',
  count: 1,
};

const TOPOLOGY = {
  env: 'dev',
  shards: { '0': [{ id: 7, hostname: 'dev-matcher-0' }] },
  roles: { matcher: ['dev-matcher-0'] },
  health: { healthy: 1 },
};

const RELEASES = {
  releases: [
    {
      id: 3,
      component: 'matcher',
      version: '1.4.2',
      artifact_hash: 'abcdef0123456789',
      env: 'dev',
      status: 'DEPLOYED',
      gate_evidence: { soak: { status: 'ok' } },
      created_by: 5,
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    },
  ],
  env: 'dev',
  count: 1,
};

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

const BOARD = {
  generated_at: '2026-01-02T00:00:00Z',
  instruments: [
    {
      instrument_id: 9,
      symbol: 'USD/JPY',
      status: 'SUSPENDED',
      state_entered_at: '2026-01-02T00:00:00Z',
      engine_status: 'ACTIVE',
      status_drift: true,
      reference_seeded: true,
    },
  ],
  pending_proposals: [{ id: 4, symbol: 'CHF/JPY', status: 'SCHEDULED', overdue: true }],
  pending_approvals: [{ id: 2, operation: 'SUSPEND', target_id: 'USD/JPY' }],
  upcoming_auctions: [
    { entry_id: 1, symbol: 'EUR/USD', auction_type: 'FIXING', next_at: '2026-01-02T16:00:00Z' },
  ],
  today_fixings: [{ symbol: 'EUR/USD', benchmark: 'WMR4PM', status: 'PENDING' }],
  warnings: ['engine status drift on USD/JPY'],
};

describe('OpsBoardPage', () => {
  it('aggregates system status and renders the live market-ops board', async () => {
    signInForTests({ roles: ['Read-Only Auditor'] });
    installFetchMock({
      'GET /api/v1/system/status': { body: STATUS },
      'GET /api/v1/admin/ops/health': {
        body: { mode: 'Normal', components: { gateway: { state: 'up' } }, recent_events: [] },
      },
      'GET /api/v1/admin/ops-board': { body: BOARD },
    });
    renderApp(<OpsBoardPage />);
    await waitFor(() => expect(screen.getByText('matcher-0')).toBeInTheDocument());
    expect(screen.getByText('lagging')).toBeInTheDocument();
    // Live board rows — warnings, drift badge, pending queues.
    await waitFor(() =>
      expect(screen.getByText('engine status drift on USD/JPY')).toBeInTheDocument(),
    );
    expect(screen.getAllByText('USD/JPY').length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText('engine drift ACTIVE')).toBeInTheDocument();
    expect(screen.getByText(/#4 CHF\/JPY/)).toBeInTheDocument();
    // Lifecycle actions live on the admin console — linked, not duplicated.
    expect(screen.getByRole('link', { name: /Admin console instruments panel/ })).toHaveAttribute(
      'href',
      '/admin',
    );
  });
});

describe('FleetPage', () => {
  it('lists env-scoped hosts and topology', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      'GET /api/v1/admin/fleet/hosts': { body: HOSTS },
      'GET /api/v1/admin/fleet/topology': { body: TOPOLOGY },
    });
    renderApp(<FleetPage />);
    await waitFor(() => expect(screen.getByText('dev-matcher-0')).toBeInTheDocument());
    expect(screen.getByText(/1 host\(s\)/)).toBeInTheDocument();
    for (const c of calls.filter((x) => x.url.includes('/admin/fleet'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('runs a drain action through the modal', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      'GET /api/v1/admin/fleet/hosts': { body: HOSTS },
      'GET /api/v1/admin/fleet/topology': { body: TOPOLOGY },
      'POST /api/v1/admin/fleet/hosts/7/drain': {
        body: {
          server_action: {
            id: 1,
            host_id: 7,
            action: 'drain',
            status: 'EXECUTED',
            requested_by: 5,
          },
        },
      },
    });
    renderApp(<FleetPage />);
    await waitFor(() => expect(screen.getByText('dev-matcher-0')).toBeInTheDocument());
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'drain' }));
    await user.type(screen.getByLabelText(/Reason/), 'maintenance window');
    await user.click(screen.getByRole('button', { name: 'Confirm drain' }));
    await waitFor(() =>
      expect(screen.getByTestId('host-action-result')).toHaveTextContent('EXECUTED'),
    );
    const post = calls.find((c) => c.method === 'POST');
    expect(JSON.parse(typeof post?.init?.body === 'string' ? post.init.body : '')).toMatchObject({
      reason: 'maintenance window',
    });
  });
});

describe('ReleasesPage', () => {
  it('promotes dev→staging without an approver; direction enforced', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      'GET /api/v1/admin/releases': { body: RELEASES },
      'POST /api/v1/admin/releases/3/promote': {
        body: { promotion: { id: 9, status: 'PENDING' } },
      },
    });
    renderApp(<ReleasesPage />);
    await waitFor(() => expect(screen.getByText('matcher')).toBeInTheDocument());
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: '→ staging' }));
    await user.type(screen.getByLabelText('Reason'), 'qa soak passed');
    await user.click(screen.getByRole('button', { name: 'Promote to staging' }));
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Promote requested'));
    const post = calls.find((c) => c.method === 'POST');
    expect(JSON.parse(typeof post?.init?.body === 'string' ? post.init.body : '')).toMatchObject({
      to_env: 'staging',
    });
  });

  it('hides promote controls from non-Super-Admin roles', async () => {
    signInForTests({ roles: ['Read-Only Auditor'] });
    installFetchMock({ 'GET /api/v1/admin/releases': { body: RELEASES } });
    renderApp(<ReleasesPage />);
    await waitFor(() => expect(screen.getByText('matcher')).toBeInTheDocument());
    expect(screen.queryByRole('button', { name: '→ staging' })).not.toBeInTheDocument();
    expect(screen.getByText('Super Admin')).toBeInTheDocument();
  });
});

describe('environment switcher', () => {
  it('requires explicit confirmation before entering production', async () => {
    signInForTests({ roles: ['Super Admin'] });
    installFetchMock({
      'GET /api/v1/admin/fleet/hosts': { body: HOSTS },
      'GET /api/v1/admin/fleet/topology': { body: TOPOLOGY },
    });
    renderApp(<FleetPage />);
    await waitFor(() => expect(screen.getByText('dev-matcher-0')).toBeInTheDocument());
    const user = userEvent.setup();
    await user.selectOptions(screen.getByLabelText('Admin environment'), 'production');
    // Staged, not applied — still dev, confirmation dialog up.
    expect(useAdminEnvStore.getState().env).toBe('dev');
    expect(screen.getByRole('alertdialog')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Enter production' }));
    expect(useAdminEnvStore.getState().env).toBe('production');
    expect(screen.getByTestId('prod-watermark')).toBeInTheDocument();
    // Persisted only to sessionStorage, never localStorage.
    expect(window.sessionStorage.getItem('exchange.admin.env')).toContain('production');
    expect(window.localStorage.getItem('exchange.admin.env')).toBeNull();
  });
});
