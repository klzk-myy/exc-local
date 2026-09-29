import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import FundingPage from './FundingPage';
import { newIdempotencyKey } from './api';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BALANCES = {
  account_id: 1001,
  balances: [{ currency: 'USD', available: '25000', locked: '0', total: '25000' }],
};

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  signInForTests();
});

function renderFunding(tab: string) {
  return renderApp(
    <Routes>
      <Route path="/funding" element={<FundingPage />} />
      <Route path="/login" element={<div>login</div>} />
    </Routes>,
    `/funding?tab=${tab}`,
  );
}

describe('DepositPanel', () => {
  it('shows the payment reference and nostro instructions', async () => {
    installFetchMock({
      'GET /api/v1/deposits/USD': {
        body: {
          currency: 'USD',
          account_id: 1001,
          reference: 'EXC-1001-USD',
          instructions: [
            {
              id: 1,
              currency: 'USD',
              bank_name: 'Bank of Test',
              bank_code: 'TESTUS33',
              account_number: '12345',
              status: 'ACTIVE',
            },
          ],
        },
      },
      'GET /api/v1/funding?type=DEPOSIT&status=PENDING&limit=20': { body: { data: [], total: 0 } },
      'GET /api/v1/funding*': { body: { data: [], total: 0 } },
    });
    renderFunding('deposit');
    expect(await screen.findByText('EXC-1001-USD')).toBeInTheDocument();
    expect((await screen.findAllByText('Bank of Test')).length).toBeGreaterThan(0);
    expect(screen.getByText('TESTUS33')).toBeInTheDocument();
  });
});

describe('WithdrawalPanel', () => {
  it('creates a withdrawal with Idempotency-Key and shows the one-time confirm token', async () => {
    const calls = installFetchMock({
      'GET /api/v1/account/balances': { body: BALANCES },
      'POST /api/v1/withdrawals': {
        status: 201,
        body: {
          withdrawal_id: 77,
          status: 'PENDING',
          currency: 'USD',
          amount: '500',
          review_tier: 'AUTO',
          confirm_token: 'ctok-xyz',
          expires_at: new Date(Date.now() + 15 * 60 * 1000).toISOString(),
        },
      },
      'GET /api/v1/funding*': { body: { data: [], total: 0 } },
    });
    renderFunding('withdraw');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/^Amount/), '500');
    await user.type(screen.getByLabelText(/Beneficiary account/), 'US-IBAN-1');
    await user.click(screen.getByRole('button', { name: 'Create withdrawal' }));
    expect(await screen.findByText(/ctok-xyz/)).toBeInTheDocument();
    const call = calls.find((c) => c.url.includes('/withdrawals') && c.method === 'POST');
    expect(call?.init?.headers).toMatchObject({ 'Idempotency-Key': expect.any(String) as unknown });
    expect(JSON.parse(call?.init?.body as string)).toMatchObject({
      currency: 'USD',
      amount: '500',
      reference_account: 'US-IBAN-1',
    });
  });

  it('blocks an amount exceeding the available balance', async () => {
    const calls = installFetchMock({
      'GET /api/v1/account/balances': { body: BALANCES },
      'GET /api/v1/funding*': { body: { data: [], total: 0 } },
    });
    renderFunding('withdraw');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/^Amount/), '99999999');
    expect(await screen.findByRole('alert')).toHaveTextContent('Exceeds available balance');
    expect(screen.getByRole('button', { name: 'Create withdrawal' })).toBeDisabled();
    expect(calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('confirms a pending withdrawal with the emailed token', async () => {
    const calls = installFetchMock({
      'GET /api/v1/account/balances': { body: BALANCES },
      'POST /api/v1/withdrawals/9/confirm': {
        body: { withdrawal_id: 9, status: 'CONFIRMED', currency: 'USD', amount: '100' },
      },
      'GET /api/v1/funding*': {
        body: {
          data: [
            {
              id: 9,
              account_id: 1001,
              currency: 'USD',
              type: 'WITHDRAWAL',
              amount: '100',
              status: 'PENDING',
              created_at: new Date(Date.now() - 60_000).toISOString(),
            },
          ],
          total: 1,
        },
      },
    });
    renderFunding('withdraw');
    const user = userEvent.setup();
    const input = await screen.findByLabelText(/Confirmation token for withdrawal 9/);
    await user.type(input, 'ctok-9');
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() => {
      const c = calls.find((x) => x.url.includes('/withdrawals/9/confirm'));
      expect(JSON.parse(c?.init?.body as string)).toMatchObject({ token: 'ctok-9' });
    });
  });
});

describe('TransferPanel', () => {
  it('sends an idempotent transfer between family accounts', async () => {
    const calls = installFetchMock({
      'GET /api/v1/account/balances': { body: BALANCES },
      'GET /api/v1/account/sub-accounts': {
        body: { data: [{ id: 2002, status: 'ACTIVE', created_at: '2026-01-01T00:00:00Z' }] },
      },
      'GET /api/v1/transfers': { body: { data: [] } },
      'POST /api/v1/transfers': {
        status: 201,
        body: {
          transfer: {
            id: 5,
            from_account_id: 1001,
            to_account_id: 2002,
            currency: 'USD',
            amount: '100',
            status: 'COMPLETED',
            created_at: '2026-01-02T00:00:00Z',
          },
        },
      },
    });
    renderFunding('transfer');
    const user = userEvent.setup();
    // account options populate after /account/balances + /sub-accounts resolve
    const fromSel = await screen.findByLabelText(/From account/);
    await waitFor(() => {
      expect(
        within(fromSel as HTMLSelectElement).getByRole('option', { name: /Main account/ }),
      ).toBeInTheDocument();
    });
    await user.selectOptions(await screen.findByLabelText(/From account/), '1001');
    await user.selectOptions(screen.getByLabelText(/To account/), '2002');
    await user.type(screen.getByLabelText(/^Amount/), '100');
    await user.click(screen.getByRole('button', { name: 'Transfer' }));
    await waitFor(() => {
      const c = calls.find((x) => x.method === 'POST' && x.url.includes('/transfers'));
      expect(c?.init?.headers).toMatchObject({ 'Idempotency-Key': expect.any(String) as unknown });
      expect(JSON.parse(c?.init?.body as string)).toEqual({
        from_account_id: 1001,
        to_account_id: 2002,
        currency: 'USD',
        amount: '100',
      });
    });
  });
});

describe('HistoryPanel', () => {
  it('renders rows and sends filter params server-side', async () => {
    const calls = installFetchMock({
      'GET /api/v1/funding*': {
        body: {
          data: [
            {
              id: 1,
              account_id: 1001,
              currency: 'USD',
              type: 'DEPOSIT',
              amount: '5000',
              status: 'COMPLETED',
              bank_method: 'SEPA',
              created_at: '2026-01-01T10:00:00Z',
            },
          ],
          next_cursor: '',
          limit: 50,
          total: 1,
        },
      },
    });
    renderFunding('history');
    await waitFor(() => {
      expect(screen.getAllByText('DEPOSIT').length).toBeGreaterThan(0);
    });
    const user = userEvent.setup();
    await user.selectOptions(screen.getByLabelText(/^Type/), 'WITHDRAWAL');
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('type=WITHDRAWAL'))).toBe(true);
    });
  });
});

describe('newIdempotencyKey', () => {
  it('produces unique keys', () => {
    expect(newIdempotencyKey()).not.toBe(newIdempotencyKey());
  });
});
