/**
 * Delegation & trading-params panel tests (Phase-10.5 Task 10.5.3.17) —
 * delegated-user CRUD + revoke-all, M-of-N policy upsert, approval
 * decide votes, leverage/margin-mode posts, swap-free + appropriateness
 * submissions, and the read-only account data surfaces.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import SettingsPage from './SettingsPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const DELEGATION_BASE = {
  'GET /api/v1/account/delegated-users': {
    body: {
      account_id: 9,
      delegated_users: [
        {
          id: 3,
          user_id: 42,
          display_name: 'Ops manager',
          status: 'ACTIVE',
          role: 'CLIENT_TRADER',
          scope: { instruments: ['EURUSD'] },
          binding_expires_at: '2027-01-01T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/account/approval-policies': {
    body: {
      policies: [
        {
          id: 7,
          operation: 'WITHDRAWAL',
          required_approvals: 2,
          threshold_amount: '10000',
          threshold_currency: 'USD',
          expires_in_seconds: 3600,
          status: 'ACTIVE',
        },
      ],
    },
  },
  'GET /api/v1/account/approval-requests': {
    body: {
      requests: [
        {
          id: 21,
          policy_id: 7,
          operation: 'WITHDRAWAL',
          status: 'PENDING',
          approvals_count: 1,
          required_approvals: 2,
          requested_by_user: 42,
          expires_at: '2026-10-08T00:00:00Z',
          fingerprint: 'fp-abc',
        },
      ],
    },
  },
};

const TRADING_BASE = {
  'GET /api/v1/account/swap-free': { body: { status: 'NONE', verification: null } },
  'GET /api/v1/account/appropriateness': {
    body: { client_category: 'RETAIL', nbp: true, assessments: [] },
  },
  'GET /api/v1/account/risk-limits': {
    body: {
      limits: { max_open_orders: 200, max_order_qty: '1000000' },
      usage: { open_orders: 3, open_orders_pct: '1.5' },
      symbols: [],
    },
  },
  'GET /api/v1/account/rate-limits': { body: { tier: 'basic', order_entry_per_sec: 10 } },
  'GET /api/v1/account/sub-accounts/aggregate': {
    body: {
      master_account_id: 9,
      balances: {
        master_account_id: 9,
        account_ids: [9, 10],
        by_currency: [{ currency: 'USD', available: '1500.00', locked: '0', total: '1500.00' }],
      },
      positions: [],
    },
  },
  'GET /api/v1/account/filters/EUR%2FUSD': {
    body: {
      filters: { symbol: 'EUR/USD', tick_size: '0.0001', lot_size: '1000', max_leverage: 30 },
    },
  },
  'GET /api/v1/account/commission/EUR%2FUSD': {
    body: { commission: { symbol: 'EUR/USD', per_lot: '3.50', model: 'RAW_SPREAD_COMMISSION' } },
  },
  'GET /api/v1/account/liquidations': {
    body: {
      liquidations: [
        {
          id: 5,
          symbol: 'GBPUSD',
          kind: 'DIRECT_CLOSE',
          side: 'SELL',
          quantity: '1000',
          price: '1.20',
          insurance_fund_contribution: '0.50',
          created_at: '2026-10-01T00:00:00Z',
        },
      ],
    },
  },
};

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  signInForTests();
});

function renderSettings(tab: string) {
  return renderApp(
    <Routes>
      <Route path="/settings" element={<SettingsPage />} />
    </Routes>,
    `/settings?tab=${tab}`,
  );
}

describe('DelegationPanel', () => {
  it('lists delegates, grants one, and casts a PENDING vote', async () => {
    const user = userEvent.setup();
    const calls = installFetchMock({
      ...DELEGATION_BASE,
      'POST /api/v1/account/delegated-users': { status: 201, body: { id: 4 } },
      'POST /api/v1/account/approval-requests/21/decide': { body: { id: 21, status: 'APPROVED' } },
    });
    renderSettings('delegation');
    const panel = await screen.findByRole('region', { name: 'Delegation' });
    await waitFor(() => expect(within(panel).getByText('Ops manager')).toBeInTheDocument());
    await waitFor(() => expect(within(panel).getByText('1/2')).toBeInTheDocument());

    await user.type(within(panel).getByLabelText('user_id'), '77');
    await user.type(within(panel).getByLabelText('display_name'), 'Back office');
    await user.selectOptions(within(panel).getByLabelText('Delegate role'), 'CLIENT_APPROVER');
    await user.type(within(panel).getByLabelText('instruments'), 'EURUSD');
    await user.click(within(panel).getByRole('button', { name: 'Grant delegate' }));
    await waitFor(() => {
      const call = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/account/delegated-users'),
      );
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['user_id']).toBe(77);
      expect(body['role']).toBe('CLIENT_APPROVER');
      expect(body['scope']).toMatchObject({ instruments: ['EURUSD'] });
    });

    await user.click(within(panel).getByRole('button', { name: 'Approve' }));
    await waitFor(() => {
      const call = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/approval-requests/21/decide'),
      );
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['approve']).toBe(true);
    });
  });

  it('upserts an M-of-N policy and revokes all delegates', async () => {
    const user = userEvent.setup();
    const calls = installFetchMock({
      ...DELEGATION_BASE,
      'PUT /api/v1/account/approval-policies': { body: { id: 8 } },
      'POST /api/v1/account/delegated-users/revoke-all': { body: { revoked: 1 } },
    });
    renderSettings('delegation');
    const panel = await screen.findByRole('region', { name: 'Delegation' });
    await waitFor(() => expect(within(panel).getByText('Ops manager')).toBeInTheDocument());

    await user.clear(within(panel).getByLabelText('required_approvals'));
    await user.type(within(panel).getByLabelText('required_approvals'), '3');
    await user.type(within(panel).getByLabelText('threshold_amount'), '25000');
    await user.type(within(panel).getByLabelText('threshold_currency'), 'USD');
    await user.click(within(panel).getByRole('button', { name: 'Upsert policy' }));
    await waitFor(() => {
      const call = calls.find(
        (c) => c.method === 'PUT' && c.url.endsWith('/account/approval-policies'),
      );
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['operation']).toBe('WITHDRAWAL');
      expect(body['required_approvals']).toBe(3);
      expect(body['threshold_amount']).toBe('25000');
    });

    await user.type(within(panel).getByLabelText('Revoke-all reason'), 'credential leak');
    await user.click(within(panel).getByRole('button', { name: 'Revoke ALL' }));
    await waitFor(() => {
      const call = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/delegated-users/revoke-all'),
      );
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['reason']).toBe('credential leak');
    });
  });
});

describe('TradingPanel', () => {
  it('posts per-symbol leverage and margin-mode', async () => {
    const user = userEvent.setup();
    const calls = installFetchMock({
      ...TRADING_BASE,
      'POST /api/v1/account/leverage': { body: { leverage: 20 } },
      'POST /api/v1/account/margin-mode': { body: { mode: 'ISOLATED' } },
    });
    renderSettings('trading');
    const panel = await screen.findByRole('region', { name: 'Trading parameters' });
    await waitFor(() => expect(within(panel).getByText(/max_open_orders/)).toBeInTheDocument());

    await user.type(within(panel).getByLabelText('Leverage symbol'), 'EURUSD');
    await user.clear(within(panel).getByLabelText('leverage'));
    await user.type(within(panel).getByLabelText('leverage'), '30');
    await user.click(within(panel).getByRole('button', { name: 'Set leverage' }));
    await waitFor(() => {
      const call = calls.find((c) => c.method === 'POST' && c.url.endsWith('/account/leverage'));
      expect(call).toBeDefined();
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['symbol']).toBe('EURUSD');
      expect(body['leverage']).toBe(30);
    });

    await user.selectOptions(within(panel).getByLabelText('Margin mode'), 'ISOLATED');
    await user.click(within(panel).getByRole('button', { name: 'Switch' }));
    await waitFor(() => {
      const call = calls.find((c) => c.method === 'POST' && c.url.endsWith('/account/margin-mode'));
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['mode']).toBe('ISOLATED');
    });
  });

  it('submits swap-free request and appropriateness assessment', async () => {
    const user = userEvent.setup();
    const calls = installFetchMock({
      ...TRADING_BASE,
      'POST /api/v1/account/swap-free/request': { status: 202, body: { status: 'PENDING' } },
      'POST /api/v1/account/appropriateness': { status: 201, body: { outcome: 'PASS' } },
    });
    renderSettings('trading');
    const panel = await screen.findByRole('region', { name: 'Trading parameters' });
    await waitFor(() => expect(within(panel).getByText('NONE')).toBeInTheDocument());

    await user.type(within(panel).getByLabelText('attestation_ref'), 'doc-88');
    await user.click(within(panel).getByRole('button', { name: 'Request' }));
    await waitFor(() => {
      const call = calls.find((c) => c.method === 'POST' && c.url.includes('/swap-free/request'));
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['attestation_ref']).toBe('doc-88');
    });

    await user.selectOptions(within(panel).getByLabelText('instrument_class'), 'NDF');
    await user.clear(within(panel).getByLabelText('score'));
    await user.type(within(panel).getByLabelText('score'), '9');
    await user.click(within(panel).getByRole('button', { name: 'Submit assessment' }));
    await waitFor(() => {
      const call = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/account/appropriateness'),
      );
      const body = JSON.parse(call?.init?.body as string) as Record<string, unknown>;
      expect(body['instrument_class']).toBe('NDF');
      expect(body['score']).toBe(9);
    });
  });

  it('renders the read-only data surfaces', async () => {
    installFetchMock(TRADING_BASE);
    renderSettings('trading');
    const panel = await screen.findByRole('region', { name: 'Trading parameters' });
    await waitFor(() => expect(within(panel).getByText(/tick_size/)).toBeInTheDocument());
    await waitFor(() => expect(within(panel).getByText('DIRECT CLOSE')).toBeInTheDocument());
    expect(within(panel).getByText(/per_lot/)).toBeInTheDocument();
    expect(within(panel).getByText(/order_entry_per_sec/)).toBeInTheDocument();
    expect(within(panel).getAllByText(/master_account_id/).length).toBeGreaterThan(0);
  });
});
