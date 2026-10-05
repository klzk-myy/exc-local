/**
 * Customer 360 tests (Phase-10.5 Task 10.5.3.4) — dossier rendering,
 * lifecycle wire contracts (four-eyes freeze, dual-control close,
 * jurisdiction pin, sub-account limit PUT), the account-scoped support
 * desk and the self-certification review surface.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import Customer360Page from './Customer360Page';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const DOSSIER = {
  account_id: 42,
  user_id: 7,
  status: 'active',
  kyc_tier: 'T1',
  account_type: 'INDIVIDUAL',
  created_at: '2026-01-01T00:00:00Z',
  balances: [{ currency: 'USD', available: '1000.00', locked: '50.00' }],
  recent_orders: [
    {
      id: 9,
      side: 'BUY',
      quantity: '1000',
      symbol: 'EUR/USD',
      status: 'ACTIVE',
      filled_quantity: '0',
    },
  ],
  recent_tickets: [],
  kyc_documents: [],
};

const BASE = {
  'GET /api/v1/admin/support/accounts/42': { body: DOSSIER },
  'GET /api/v1/admin/compliance/holds': {
    body: {
      holds: [
        {
          hold_id: 'H-1',
          account_id: 42,
          trigger_source: 'MANUAL',
          reason: 'sanctions review',
          status: 'OPEN',
          sla_deadline: '2026-01-03T00:00:00Z',
          placed_by: 5,
        },
      ],
    },
  },
  'GET /api/v1/admin/audit-log': {
    body: {
      data: [
        {
          id: 1,
          admin_user_id: 5,
          action: 'account.freeze',
          target_type: 'account',
          target_id: 42,
          created_at: '2026-01-02T00:00:00Z',
        },
      ],
      next_cursor: '',
      limit: 10,
      total: 1,
    },
  },
  'GET /api/v1/admin/support/tickets': {
    body: {
      data: [
        {
          ticket_id: 11,
          account_id: 42,
          type: 'SUPPORT',
          category: 'FUNDING',
          priority: 'P2',
          subject: 'Wire not credited',
          status: 'OPEN',
          queue: 'OPERATIONS',
          created_at: '2026-01-02T00:00:00Z',
          updated_at: '2026-01-02T00:00:00Z',
        },
      ],
      next_cursor: '',
      limit: 20,
      total: 1,
    },
  },
  'GET /api/v1/admin/support/tickets/11': {
    body: {
      ticket: { ticket_id: 11, subject: 'Wire not credited', status: 'OPEN' },
      notes: [
        {
          note_id: 3,
          body: 'Checked with ops — MT103 expected today.',
          internal: true,
          created_at: '2026-01-02T01:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/support/complaints/register': {
    body: { data: [], next_cursor: '', limit: 50, total: 0 },
  },
  'GET /api/v1/admin/kyc/pending': { body: { submissions: [] } },
  'GET /api/v1/admin/accounts/42/self-certifications': {
    body: {
      certifications: [
        {
          id: 4,
          account_id: 42,
          form_type: 'W-8BEN',
          tin_country: 'DE',
          status: 'VALIDATED',
          created_at: '2026-01-01T00:00:00Z',
        },
      ],
    },
  },
};

const openCustomer = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.type(screen.getByLabelText('Account id'), '42');
  await user.click(screen.getByRole('button', { name: 'Open customer' }));
};

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('Customer360Page', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<Customer360Page />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('loads the dossier, holds, audit excerpt, tickets and self-certs', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock(BASE);
    renderApp(<Customer360Page />);
    const user = userEvent.setup();
    await openCustomer(user);

    await waitFor(() => expect(screen.getByTestId('support-view')).toBeInTheDocument());
    expect(screen.getByText('H-1 [MANUAL] sanctions review')).toBeInTheDocument();
    expect(screen.getByText(/account\.freeze by admin #5/)).toBeInTheDocument();
    expect(screen.getByText(/#11 \[P2\] Wire not credited/)).toBeInTheDocument();
    expect(screen.getByText('W-8BEN')).toBeInTheDocument();
    expect(screen.getByTestId('preconditions')).toHaveTextContent('1 open order');
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('freezes with a distinct approver through the confirm dialog', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/accounts/42/freeze': { body: { account_id: 42, action: 'freeze' } },
    });
    renderApp(<Customer360Page />);
    const user = userEvent.setup();
    await openCustomer(user);
    await waitFor(() => expect(screen.getByTestId('support-view')).toBeInTheDocument());

    const lc = screen.getByRole('region', { name: 'Lifecycle actions' });
    await user.type(within(lc).getByLabelText(/Reason \(freeze/), 'court order 2026-114');
    await user.type(within(lc).getByLabelText(/Approver admin id/), '99');
    await user.click(within(lc).getByRole('button', { name: 'Freeze account' }));
    await user.click(screen.getByRole('button', { name: 'Confirm freeze' }));

    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/admin/accounts/42/freeze'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(typeof post?.init?.body === 'string' ? post.init.body : '{}')).toEqual({
        reason: 'court order 2026-114',
        approver_id: 99,
      });
    });
  });

  it('forced close submits the dual-control request and says PENDING', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/accounts/42/close': {
        status: 202,
        body: {
          request: { id: 77, status: 'PENDING' },
          message: 'dual-control request pending — approval executes the closure',
        },
      },
    });
    renderApp(<Customer360Page />);
    const user = userEvent.setup();
    await openCustomer(user);
    await waitFor(() => expect(screen.getByTestId('support-view')).toBeInTheDocument());

    const lc = screen.getByRole('region', { name: 'Lifecycle actions' });
    await user.type(within(lc).getByLabelText(/Reason \(freeze/), 'dormant + fraud markers');
    await user.click(within(lc).getByRole('button', { name: 'Forced close (dual-control)' }));
    await user.click(screen.getByRole('button', { name: 'Submit closure request' }));

    await waitFor(() =>
      expect(screen.getByText(/Closure request #77 pending approval/)).toBeInTheDocument(),
    );
    expect(
      calls.some((c) => c.method === 'POST' && c.url.endsWith('/admin/accounts/42/close')),
    ).toBe(true);
  });

  it('pins jurisdiction and applies the sub-account limit via PUT', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/accounts/42/jurisdiction': { body: { policy: {} } },
      'PUT /api/v1/admin/accounts/42/sub-account-limit': { body: { max_sub_accounts: 5 } },
    });
    renderApp(<Customer360Page />);
    const user = userEvent.setup();
    await openCustomer(user);
    await waitFor(() => expect(screen.getByTestId('support-view')).toBeInTheDocument());

    const lc = screen.getByRole('region', { name: 'Lifecycle actions' });
    await user.type(within(lc).getByLabelText('Jurisdiction code'), 'eu');
    await user.click(within(lc).getByRole('button', { name: 'Pin jurisdiction' }));
    await user.type(within(lc).getByLabelText('Max sub-accounts'), '5');
    await user.click(within(lc).getByRole('button', { name: 'Apply limit' }));

    await waitFor(() => {
      const pin = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/admin/accounts/42/jurisdiction'),
      );
      expect(pin).toBeDefined();
      expect(JSON.parse(typeof pin?.init?.body === 'string' ? pin.init.body : '{}')).toMatchObject({
        jurisdiction_code: 'eu',
      });
      const put = calls.find(
        (c) => c.method === 'PUT' && c.url.endsWith('/admin/accounts/42/sub-account-limit'),
      );
      expect(put).toBeDefined();
      expect(new Headers(put?.init?.headers).get('X-Admin-Env')).toBe('dev');
    });
  });

  it('expands a ticket and posts an internal note', async () => {
    signInForTests({ roles: ['Support Agent'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/support/tickets/11/notes': { status: 201, body: { note_id: 9 } },
    });
    renderApp(<Customer360Page />);
    const user = userEvent.setup();
    await openCustomer(user);
    await waitFor(() => expect(screen.getByText(/#11 \[P2\]/)).toBeInTheDocument());

    const desk = screen.getByRole('region', { name: 'Support desk' });
    await user.click(within(desk).getByRole('button', { name: /#11 \[P2\]/ }));
    await waitFor(() => expect(screen.getByText(/MT103 expected today/)).toBeInTheDocument());
    await user.type(within(desk).getByLabelText('Note body'), 'Customer called — ETA confirmed');
    await user.click(within(desk).getByRole('button', { name: 'Add note' }));

    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/admin/support/tickets/11/notes'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(typeof post?.init?.body === 'string' ? post.init.body : '{}')).toEqual({
        body: 'Customer called — ETA confirmed',
        internal: true,
      });
    });
  });
});
