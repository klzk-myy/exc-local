/**
 * PAMM page — pool browse renders wire Pool rows (Go field names),
 * detail fetches the summary + statement, invest POSTs {amount}.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';
import { resetSessionForTests } from '@/lib/auth/session';

import PammPage from './PammPage';
import { parsePool, parsePoolDetail, parseEntry } from './api';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
  signInForTests({ roles: [] });
});

const POOL = {
  PoolID: 5,
  ManagerAccountID: 42,
  PoolAccountID: 500,
  Name: 'Alpha FX',
  Currency: 'USD',
  MinInvestment: '100',
  Status: 'ACTIVE',
  CreatedAt: '2026-01-01T00:00:00Z',
};

describe('parsers', () => {
  it('parses Go-field-name pool + snake_case detail/entry', () => {
    expect(parsePool(POOL)).toMatchObject({ poolId: 5, name: 'Alpha FX' });
    expect(
      parsePoolDetail({ pool: POOL, investor_count: 3, total_invested: '1200.5' }),
    ).toMatchObject({ investorCount: 3, totalInvested: '1200.5' });
    expect(
      parseEntry({
        entry_id: 9,
        txn_type: 'PAMM_INVEST',
        direction: 'IN',
        amount: '100',
        currency: 'USD',
        posted_at: '2026-01-01T00:00:00Z',
      }),
    ).toMatchObject({ entryId: 9, txnType: 'PAMM_INVEST' });
    expect(parsePool({})).toBeNull();
  });
});

describe('PammPage', () => {
  it('lists pools and opens detail with statement', async () => {
    installFetchMock({
      'GET /api/v1/pamm/pools': { body: { pools: [POOL] } },
      'GET /api/v1/pamm/pools/5': {
        body: { pool: POOL, investor_count: 2, total_invested: '250' },
      },
      'GET /api/v1/pamm/pools/5/statement': {
        body: {
          entries: [
            {
              entry_id: 9,
              txn_type: 'PAMM_INVEST',
              direction: 'IN',
              amount: '100',
              currency: 'USD',
              posted_at: '2026-01-01T00:00:00Z',
            },
          ],
        },
      },
      'POST /api/v1/pamm/pools/5/invest': { body: { pool_id: 5, amount: '50' } },
    });
    renderApp(<PammPage />);

    expect(await screen.findByText('Alpha FX')).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Open' }));
    expect(await screen.findByText('2 investors · 250 USD pooled')).toBeInTheDocument();
    expect(screen.getByText('PAMM_INVEST')).toBeInTheDocument();
  });

  it('invests a typed amount', async () => {
    const calls = installFetchMock({
      'GET /api/v1/pamm/pools': { body: { pools: [POOL] } },
      'GET /api/v1/pamm/pools/5': {
        body: { pool: POOL, investor_count: 0, total_invested: '0' },
      },
      'GET /api/v1/pamm/pools/5/statement': { body: { entries: [] } },
      'POST /api/v1/pamm/pools/5/invest': { body: { pool_id: 5, amount: '50' } },
    });
    renderApp(<PammPage />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Open' }));
    await user.type(await screen.findByTestId('pamm-amount'), '50');
    await user.click(screen.getByRole('button', { name: 'Invest' }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.method === 'POST' &&
            c.url.includes('/pamm/pools/5/invest') &&
            typeof c.init?.body === 'string' &&
            c.init.body.includes('"amount":"50"'),
        ),
      ).toBe(true),
    );
  });
});
