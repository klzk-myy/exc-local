import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp } from '@/test/accountMocks';

import BacktestPage from './BacktestPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

/** 60 hourly bars, server order (DESC), gently rising closes so the
 * SMA-cross preset produces one long trade. */
function risingKlines() {
  const base = Date.now() - 60 * 3_600_000;
  const rows = [];
  for (let i = 59; i >= 0; i--) {
    const c = 1.05 + i * 0.001;
    rows.push({
      open_time_ms: base + i * 3_600_000,
      open: (c - 0.0005).toFixed(5),
      high: (c + 0.0006).toFixed(5),
      low: (c - 0.0011).toFixed(5),
      close: c.toFixed(5),
      volume: '1000',
      closed: true,
    });
  }
  return {
    symbol: 'EUR/USD',
    interval: '1h',
    data: rows,
    count: rows.length,
    limit: 1500,
    next_cursor: '',
  };
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
});

describe('BacktestPage', () => {
  it('runs a preset over real klines and renders results + disclosures', async () => {
    installFetchMock({ 'GET /api/v1/klines/*': { body: risingKlines() } });
    renderApp(<BacktestPage />);
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Run backtest' }));

    await waitFor(() => expect(screen.getByTestId('backtest-result')).toBeInTheDocument());
    const result = within(screen.getByTestId('backtest-result'));
    expect(result.getAllByText('Net P&L').length).toBeGreaterThan(0);
    expect(result.getAllByText('Max drawdown').length).toBeGreaterThan(0);
    expect(screen.getByTestId('equity-curve')).toBeInTheDocument();
    // Required honesty copy
    expect(screen.getByText(/no lookahead/)).toBeInTheDocument();
    expect(screen.getByText(/Deterministic and reproducible/)).toBeInTheDocument();
  });

  it('surfaces a data error instead of fabricating results', async () => {
    installFetchMock({
      'GET /api/v1/klines/*': {
        status: 404,
        body: { type: 'error', error: 'NOT_FOUND', message: 'unknown symbol', status: 404 },
      },
    });
    renderApp(<BacktestPage />);
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Run backtest' }));
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument());
    expect(screen.queryByTestId('backtest-result')).not.toBeInTheDocument();
  });

  it('refuses to run when the window has too few candles', async () => {
    installFetchMock({
      'GET /api/v1/klines/*': {
        body: {
          symbol: 'EUR/USD',
          interval: '1h',
          data: [],
          count: 0,
          limit: 1500,
          next_cursor: '',
        },
      },
    });
    renderApp(<BacktestPage />);
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Run backtest' }));
    await waitFor(() => expect(screen.getByText(/need at least ~30/)).toBeInTheDocument());
  });
});
