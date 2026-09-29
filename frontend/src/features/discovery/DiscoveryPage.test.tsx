import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useRateAlertStore, useWatchlistStore } from '@/lib/alerts';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import DiscoveryPage from './DiscoveryPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const INSTRUMENTS = {
  data: [
    {
      symbol: 'EUR/USD',
      base_currency: 'EUR',
      quote_currency: 'USD',
      instrument_type: 'SPOT',
      status: 'ACTIVE',
      tick_size: '0.00001',
      lot_size: '1000',
      min_order_qty: '1000',
      max_order_qty: '10000000',
      min_notional: '100',
      max_spread_pips: '2.5',
      max_leverage: 30,
      settlement_cycle: 1,
      settlement: 'T+1',
    },
    {
      symbol: 'USD/JPY',
      base_currency: 'USD',
      quote_currency: 'JPY',
      instrument_type: 'SPOT',
      status: 'ACTIVE',
      tick_size: '0.001',
      lot_size: '1000',
      min_order_qty: '1000',
      max_order_qty: '10000000',
      min_notional: '100',
      max_spread_pips: '3.0',
      max_leverage: 30,
      settlement_cycle: 1,
      settlement: 'T+1',
    },
  ],
};

const browser = () => within(screen.getByLabelText('Instrument browser'));

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useWatchlistStore.setState({ lists: {} });
  useRateAlertStore.setState({ alerts: {} });
});

describe('DiscoveryPage', () => {
  it('lists instruments from the live reference endpoint', async () => {
    signInForTests();
    installFetchMock({ 'GET /api/v1/instruments': { body: INSTRUMENTS } });
    renderApp(<DiscoveryPage />);
    await waitFor(() => expect(browser().getByText('EUR/USD')).toBeInTheDocument());
    expect(browser().getByText('USD/JPY')).toBeInTheDocument();
    // Local-alerting honesty note is rendered (panel note + footer copy).
    expect(screen.getAllByText(/no server-side alerting/i).length).toBeGreaterThan(0);
  });

  it('filters by symbol substring', async () => {
    signInForTests();
    installFetchMock({ 'GET /api/v1/instruments': { body: INSTRUMENTS } });
    renderApp(<DiscoveryPage />);
    await waitFor(() => expect(browser().getByText('EUR/USD')).toBeInTheDocument());
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Search instruments'), 'JPY');
    expect(browser().queryByText('EUR/USD')).not.toBeInTheDocument();
    expect(browser().getByText('USD/JPY')).toBeInTheDocument();
  });

  it('stars an instrument into the account watchlist (persisted)', async () => {
    signInForTests();
    installFetchMock({ 'GET /api/v1/instruments': { body: INSTRUMENTS } });
    renderApp(<DiscoveryPage />);
    await waitFor(() => expect(browser().getByText('EUR/USD')).toBeInTheDocument());
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Add EUR/USD to watchlist' }));
    await waitFor(() => expect(useWatchlistStore.getState().lists['1001']).toContain('EUR/USD'));
    expect(window.localStorage.getItem('exc.watchlist.v1.1001')).toContain('EUR/USD');
    // Disconnected socket → honest "unavailable" note, never a fake price.
    expect(screen.getByText(/Prices unavailable — market data disconnected/)).toBeInTheDocument();
  });

  it('creates a rate alert from the form', async () => {
    signInForTests();
    installFetchMock({ 'GET /api/v1/instruments': { body: INSTRUMENTS } });
    renderApp(<DiscoveryPage />);
    await waitFor(() => expect(browser().getByText('EUR/USD')).toBeInTheDocument());
    const user = userEvent.setup();
    await user.selectOptions(screen.getByLabelText('Symbol'), 'EUR/USD');
    await user.type(screen.getByLabelText('Target price'), '1.09000');
    await user.click(screen.getByRole('button', { name: 'Add' }));
    await waitFor(() => expect(useRateAlertStore.getState().alerts['1001']?.length).toBe(1));
    expect(screen.getByText(/EUR\/USD ≥ 1\.09000/)).toBeInTheDocument();
  });

  it('renders the API error and no fabricated table when instruments fail', async () => {
    signInForTests();
    installFetchMock({
      'GET /api/v1/instruments': {
        status: 503,
        body: {
          type: 'error',
          error: 'SERVICE_DEGRADED',
          message: 'instrument cache unavailable',
          status: 503,
        },
      },
    });
    renderApp(<DiscoveryPage />);
    await waitFor(() =>
      expect(screen.getByText(/instrument cache unavailable/)).toBeInTheDocument(),
    );
    expect(browser().queryByText('EUR/USD')).not.toBeInTheDocument();
  });
});
