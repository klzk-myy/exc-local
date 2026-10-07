/**
 * Content/promotions/emergency console tests (Phase-10.5 Task
 * 10.5.3.16) — wire-level assertions for announcement PATCH/DELETE,
 * promotion approval checklist + second approver, template decisions,
 * copy-strategy suspend, break-glass grants, API-key expiry extension
 * (202 dual-control), and dead-letter retransmission.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import ContentPage from './ContentPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BASE = {
  'GET /api/v1/admin/announcements': {
    body: {
      data: [
        {
          id: 5,
          title: 'Fee schedule change',
          category: 'GENERAL',
          status: 'PUBLISHED',
          publish_at: '2026-10-01T00:00:00Z',
          created_by: 'ops',
        },
      ],
      count: 1,
    },
  },
  'GET /api/v1/admin/maintenance-windows': {
    body: {
      data: [
        {
          id: 2,
          title: 'Gateway restart',
          description: '',
          scope: 'GATEWAY',
          symbols: [],
          status: 'SCHEDULED',
          starts_at: '2026-10-10T02:00:00Z',
          ends_at: '2026-10-10T02:30:00Z',
        },
      ],
      count: 1,
    },
  },
  'GET /api/v1/admin/promotions': {
    body: {
      promotions: [
        {
          promotion_id: 9,
          slug: 'autumn-spread',
          channel: 'LANDING',
          body_ref: 'doc://promo-9',
          title: 'Autumn spread promo',
          version: 1,
          approval_status: 'PENDING_REVIEW',
          contains_claim: true,
          submitted_by: 7,
        },
      ],
    },
  },
  'GET /api/v1/admin/promotions/report': {
    body: {
      promotions: {
        by_status: { PENDING_REVIEW: 1 },
        by_channel: { LANDING: 1 },
        sla_breached: 0,
        drafts_aging: 0,
        expired_flagged: 0,
        expiring_in_30d: 0,
      },
    },
  },
  'GET /api/v1/admin/strategy-templates': {
    body: {
      templates: [
        {
          template_id: 4,
          name: 'Grid bot',
          kind: 'GRID',
          status: 'PENDING_APPROVAL',
          publisher_account_id: 12,
        },
      ],
    },
  },
  'GET /api/v1/admin/webhooks/dead-letters': {
    body: {
      dead_letters: [
        {
          id: 1,
          delivery_id: 'dlv_abc',
          endpoint_id: 2,
          account_id: 44,
          event: 'trade.fill',
          status: 'DEAD',
          attempts: 8,
          max_attempts: 8,
          last_status_code: 503,
          last_error: 'connection refused',
          created_at: '2026-10-05T00:00:00Z',
        },
      ],
    },
  },
};

const region = (name: string) => screen.getByRole('region', { name });

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('ContentPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<ContentPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all five panels, stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock(BASE);
    renderApp(<ContentPage />);
    await waitFor(() => expect(region('Content')).toBeInTheDocument());
    expect(region('Promotions')).toBeInTheDocument();
    expect(region('Curation')).toBeInTheDocument();
    expect(region('Emergency and integrations')).toBeInTheDocument();
    expect(region('Liquidity providers')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('Fee schedule change')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('dlv_abc')).toBeInTheDocument());
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('patches an announcement and retracts via DELETE', async () => {
    const user = userEvent.setup();
    signInForTests({ roles: ['Support Agent'] });
    const calls = installFetchMock({
      ...BASE,
      'PATCH /api/v1/admin/announcements/5': { body: { id: 5, status: 'EXPIRED' } },
      'DELETE /api/v1/admin/announcements/5': { body: { id: 5, status: 'RETRACTED' } },
    });
    renderApp(<ContentPage />);
    const panel = region('Content');
    await waitFor(() => expect(within(panel).getByText('Fee schedule change')).toBeInTheDocument());
    await user.click(within(panel).getByRole('button', { name: 'Edit' }));
    await user.selectOptions(within(panel).getByLabelText('Edit status'), 'EXPIRED');
    await user.click(within(panel).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      const patch = calls.find((c) => c.method === 'PATCH' && c.url.includes('/announcements/5'));
      expect(patch).toBeDefined();
      const body = JSON.parse(patch?.init?.body as string) as Record<string, unknown>;
      expect(body['status']).toBe('EXPIRED');
      expect(new Headers(patch?.init?.headers).get('X-Admin-Env')).toBe('dev');
    });
    await user.click(within(panel).getByRole('button', { name: 'Retract' }));
    await waitFor(() => {
      expect(
        calls.find((c) => c.method === 'DELETE' && c.url.includes('/announcements/5')),
      ).toBeDefined();
    });
  });

  it('approves a claims promotion with checklist + second approver', async () => {
    const user = userEvent.setup();
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/promotions/9/approve': { body: { promotion: { promotion_id: 9 } } },
    });
    renderApp(<ContentPage />);
    const panel = region('Promotions');
    await waitFor(() => expect(within(panel).getByText('autumn-spread')).toBeInTheDocument());
    await user.click(within(panel).getByRole('button', { name: 'Approve' }));
    await user.type(within(panel).getByLabelText('Second approver id'), '88');
    await user.type(within(panel).getByLabelText('approved_until'), '2027-01-01T00:00:00Z');
    await user.click(within(panel).getByRole('button', { name: 'Confirm approval' }));
    await waitFor(() => {
      const call = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/promotions/9/approve'),
      );
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      const checklist = body['checklist'] as Record<string, boolean>;
      expect(checklist['risk_warning']).toBe(true);
      expect(checklist['fair_clear']).toBe(true);
      expect(body['second_approver_id']).toBe(88);
      expect(body['approved_until']).toBe('2027-01-01T00:00:00Z');
    });
  });

  it('mints a break-glass grant with a distinct approver', async () => {
    const user = userEvent.setup();
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/break-glass': { status: 201, body: { grant: { id: 31 } } },
    });
    renderApp(<ContentPage />);
    const panel = region('Emergency and integrations');
    await waitFor(() => expect(within(panel).getByText('dlv_abc')).toBeInTheDocument());
    await user.type(within(panel).getByLabelText('grantee_id'), '55');
    await user.type(within(panel).getByLabelText('incident_ref'), 'INC-99');
    await user.type(within(panel).getByLabelText('Break-glass reason'), 'prod outage');
    await user.type(within(panel).getByLabelText('Second approver id'), '66');
    await user.click(within(panel).getByRole('button', { name: 'Mint grant' }));
    await waitFor(() => {
      const call = calls.find((c) => c.method === 'POST' && c.url.endsWith('/admin/break-glass'));
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['grantee_id']).toBe(55);
      expect(body['incident_ref']).toBe('INC-99');
      expect(body['ttl_seconds']).toBe(3600);
      expect(body['second_approver_id']).toBe(66);
    });
    await waitFor(() => expect(screen.getByText(/grant #31 minted/i)).toBeInTheDocument());
  });

  it('queues an API-key expiry extension as a dual-control request', async () => {
    const user = userEvent.setup();
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      ...BASE,
      'PUT /api/v1/admin/api-keys/77/extend-expiry': {
        status: 202,
        body: { request: { id: 41 } },
      },
    });
    renderApp(<ContentPage />);
    const panel = region('Emergency and integrations');
    await waitFor(() => expect(within(panel).getByText('dlv_abc')).toBeInTheDocument());
    await user.type(within(panel).getByLabelText('API key id'), '77');
    await user.type(within(panel).getByLabelText('until'), '2027-02-01T00:00:00Z');
    await user.type(within(panel).getByLabelText('Extend reason'), 'cert renewal slip');
    await user.click(within(panel).getByRole('button', { name: 'Submit extension' }));
    await waitFor(() => {
      const call = calls.find(
        (c) => c.method === 'PUT' && c.url.includes('/api-keys/77/extend-expiry'),
      );
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['until']).toBe('2027-02-01T00:00:00Z');
      expect(body['reason']).toBe('cert renewal slip');
      expect(new Headers(call?.init?.headers).get('X-Admin-Env')).toBe('dev');
    });
    await waitFor(() => expect(screen.getByText(/request #41/)).toBeInTheDocument());
  });

  it('retransmits a dead-letter delivery and suspends a copy strategy', async () => {
    const user = userEvent.setup();
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/webhooks/dead-letters/dlv_abc/retransmit': {
        body: { delivery: { delivery_id: 'dlv_abc', status: 'PENDING' } },
      },
      'POST /api/v1/admin/copy/strategies/8/suspend': { body: { status: 'SUSPENDED' } },
    });
    renderApp(<ContentPage />);
    const panel = region('Emergency and integrations');
    await waitFor(() => expect(within(panel).getByText('dlv_abc')).toBeInTheDocument());
    await user.click(within(panel).getByRole('button', { name: 'Retransmit' }));
    await waitFor(() => {
      expect(
        calls.find(
          (c) => c.method === 'POST' && c.url.includes('/dead-letters/dlv_abc/retransmit'),
        ),
      ).toBeDefined();
    });

    const cur = region('Curation');
    await waitFor(() => expect(within(cur).getByText('Grid bot')).toBeInTheDocument());
    await user.type(within(cur).getByLabelText('Strategy id'), '8');
    await user.type(within(cur).getByLabelText('Suspend reason'), 'stat manipulation');
    await user.click(within(cur).getByRole('button', { name: 'Suspend strategy' }));
    await waitFor(() => {
      const call = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/copy/strategies/8/suspend'),
      );
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['reason']).toBe('stat manipulation');
    });
  });
});
