import { beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { ApiClient } from '@/lib/api';
import { resetSessionForTests, useSessionStore } from '@/lib/auth/session';
import { boundAdminApi, useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import { InstrumentsPanel } from './InstrumentsPanel';

const api = new ApiClient({
  baseUrl: '/api/v1',
  fetchImpl: (input, init) => fetch(input, init),
  getAuthToken: () => useSessionStore.getState().accessToken,
});
const adminApi = boundAdminApi(api, 'dev');

const DRAFT_ROW = {
  id: 1,
  symbol: 'EURUSD',
  base_currency: 'EUR',
  quote_currency: 'USD',
  instrument_type: 'SPOT',
  status: 'DRAFT',
  tick_size: '0.0001',
  lot_size: '1000',
  min_order_qty: '1000',
  max_order_qty: '5000000',
  settlement_cycle: 1,
  max_leverage: 30,
  state_entered_at: '2026-01-02T00:00:00Z',
};

const ACTIVE_ROW = {
  ...DRAFT_ROW,
  id: 2,
  symbol: 'USDJPY',
  base_currency: 'USD',
  quote_currency: 'JPY',
  status: 'ACTIVE',
  tick_size: '0.01',
};

const LIST = { instruments: [DRAFT_ROW, ACTIVE_ROW] };

function renderPanel(routes: Parameters<typeof installFetchMock>[0]) {
  const calls = installFetchMock({
    'GET /api/v1/admin/instruments': { body: LIST },
    ...routes,
  });
  renderApp(<InstrumentsPanel adminApi={adminApi} api={api} />);
  return calls;
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('InstrumentsPanel', () => {
  it('renders the instrument table from the admin list', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = renderPanel({});
    await waitFor(() => expect(screen.getByText('EURUSD')).toBeInTheDocument());
    expect(screen.getByText('USDJPY')).toBeInTheDocument();
    expect(screen.getByTestId('instrument-row-EURUSD')).toHaveTextContent('DRAFT');
    expect(screen.getByTestId('instrument-row-USDJPY')).toHaveTextContent('ACTIVE');
    const listCall = calls.find((c) => c.url.includes('/admin/instruments'));
    expect(new Headers(listCall?.init?.headers).get('X-Admin-Env')).toBe('dev');
  });

  it('gates lifecycle buttons by admin role', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    renderPanel({});
    const row = await screen.findByTestId('instrument-row-USDJPY');
    // Risk Manager: restrict / cancel-only / halt yes — suspend (CO) and
    // delist (SA + dual) never render.
    for (const name of ['Restrict', 'Cancel-only', 'Halt']) {
      expect(within(row).getByRole('button', { name })).toBeInTheDocument();
    }
    for (const name of ['Suspend', 'Delist']) {
      expect(within(row).queryByRole('button', { name })).not.toBeInTheDocument();
    }
    expect(screen.queryByRole('button', { name: 'New instrument' })).not.toBeInTheDocument();
    // DRAFT row: activate + edit are the RM-permitted actions.
    const draft = screen.getByTestId('instrument-row-EURUSD');
    expect(within(draft).getByRole('button', { name: 'Activate' })).toBeInTheDocument();
    expect(within(draft).getByRole('button', { name: 'Edit' })).toBeInTheDocument();
  });

  it('shows suspend for Compliance Officer and delist/create for Super Admin', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    renderPanel({});
    const row = await screen.findByTestId('instrument-row-USDJPY');
    expect(within(row).getByRole('button', { name: 'Suspend' })).toBeInTheDocument();
    expect(within(row).getByRole('button', { name: 'Cancel-only' })).toBeInTheDocument();
    expect(within(row).queryByRole('button', { name: 'Halt' })).not.toBeInTheDocument();
    expect(within(row).queryByRole('button', { name: 'Delist' })).not.toBeInTheDocument();
  });

  it('Super Admin sees delist + create', async () => {
    signInForTests({ roles: ['Super Admin'] });
    renderPanel({});
    const row = await screen.findByTestId('instrument-row-USDJPY');
    expect(within(row).getByRole('button', { name: 'Delist' })).toBeInTheDocument();
    expect(within(row).getByRole('button', { name: 'Suspend' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'New instrument' })).toBeInTheDocument();
  });

  it('posts the lifecycle op to the instrument route with the reason', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = renderPanel({
      'POST /api/v1/admin/instruments/2/halt': { body: { ...ACTIVE_ROW, status: 'HALTED' } },
    });
    const row = await screen.findByTestId('instrument-row-USDJPY');
    const user = userEvent.setup();
    await user.click(within(row).getByRole('button', { name: 'Halt' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText(/Reason/), 'volatility spike');
    await user.click(within(dialog).getByRole('button', { name: 'Halt' }));
    await waitFor(() =>
      expect(calls.some((c) => c.url.endsWith('/admin/instruments/2/halt'))).toBe(true),
    );
    const call = calls.find((c) => c.url.endsWith('/admin/instruments/2/halt'));
    expect(call?.method).toBe('POST');
    const body = call?.init?.body;
    expect(
      typeof body === 'string' ? (JSON.parse(body) as Record<string, unknown>) : {},
    ).toMatchObject({ reason: 'volatility spike' });
  });

  it('renders the dual-control 202 as a pending-approval notice, not an error', async () => {
    signInForTests({ roles: ['Super Admin'] });
    renderPanel({
      'POST /api/v1/admin/instruments/2/delist': {
        status: 202,
        body: {
          status: 'PENDING',
          dual_control_id: 42,
          operation: 'instrument-delist',
          required_approver: 'Super Admin',
          expires_at: 1_893_456_000,
          instrument_id: 2,
        },
      },
    });
    const row = await screen.findByTestId('instrument-row-USDJPY');
    const user = userEvent.setup();
    await user.click(within(row).getByRole('button', { name: 'Delist' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText(/Reason/), 'delisting notice served');
    await user.click(within(dialog).getByRole('button', { name: 'Delist' }));
    await waitFor(() => expect(screen.getByText(/Submitted for approval/)).toBeInTheDocument());
    expect(screen.getByText(/#42/)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('renders the denial card when the list rejects the role', async () => {
    signInForTests({ roles: ['Support Agent'] });
    renderPanel({
      'GET /api/v1/admin/instruments': {
        status: 403,
        body: {
          type: 'error',
          error: 'UNAUTHORIZED_ROLE',
          message: 'requires Risk Manager',
          status: 403,
        },
      },
    });
    await waitFor(() => expect(screen.getByTestId('access-denied')).toBeInTheDocument());
  });
});
