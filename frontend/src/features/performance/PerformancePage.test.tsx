import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import PerformancePage from './PerformancePage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const POSITIONS = {
  positions: [
    {
      position_id: 11,
      instrument_id: 1,
      symbol: 'EUR/USD',
      side: 'LONG',
      quantity: '10000',
      entry_price: '1.08000',
      mark_price: '1.08500',
      unrealized_pnl: '50.00',
      realized_pnl: '0',
      margin_used: '333.33',
      opened_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-02T00:00:00Z',
    },
  ],
};

const ORDERS = {
  data: [
    {
      order_id: 'o-1',
      symbol: 'EUR/USD',
      instrument_id: 1,
      side: 'BUY',
      status: 'FILLED',
      filled_qty: '10000',
      avg_fill_price: '1.08000',
      created_at: '2026-01-01T00:00:00Z',
    },
    {
      order_id: 'o-2',
      symbol: 'EUR/USD',
      instrument_id: 1,
      side: 'SELL',
      status: 'FILLED',
      filled_qty: '4000',
      avg_fill_price: '1.09000',
      created_at: '2026-01-01T12:00:00Z',
    },
  ],
  next_cursor: '',
  limit: 200,
  total: 2,
};

const STUB_501 = {
  status: 501,
  body: {
    type: 'error',
    error: 'NOT_IMPLEMENTED',
    message: 'endpoint returned NOT_IMPLEMENTED',
    status: 501,
  },
};

function installPerfMock() {
  return installFetchMock({
    'GET /api/v1/positions': { body: POSITIONS },
    'GET /api/v1/orders': { body: ORDERS },
    'GET /api/v1/account/pnl': STUB_501,
    'GET /api/v1/account/income': STUB_501,
    'GET /api/v1/account/snapshots': STUB_501,
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
});

describe('PerformancePage', () => {
  it('derives P&L stats from fills and marks them "derived"', async () => {
    signInForTests();
    installPerfMock();
    renderApp(<PerformancePage />);
    await waitFor(() => expect(screen.getByText('Filled orders analyzed')).toBeInTheDocument());
    // Sell 4000 @1.09 vs buy VWAP 1.08 → realized ≈ +40.00; position adds +50 unrealized.
    expect(screen.getByText('Realized P&L')).toBeInTheDocument();
    expect(screen.getByText('Unrealized P&L')).toBeInTheDocument();
    expect(screen.getAllByText('derived').length).toBeGreaterThan(0);
    expect(screen.getAllByText('EUR/USD').length).toBeGreaterThan(0);
  });

  it('marks stub reporting endpoints unavailable instead of fabricating', async () => {
    signInForTests();
    installPerfMock();
    renderApp(<PerformancePage />);
    await waitFor(() => expect(screen.getByTestId('derived-note')).toBeInTheDocument());
    expect(screen.getAllByText(/unavailable \(registered/).length).toBe(3);
  });

  it('shows the polling fallback when the socket is disconnected', async () => {
    signInForTests();
    installPerfMock();
    renderApp(<PerformancePage />);
    await waitFor(() => expect(screen.getByTestId('feed-status')).toBeInTheDocument());
    expect(screen.getByTestId('feed-status')).toHaveTextContent('polling — socket disconnected');
  });
});
