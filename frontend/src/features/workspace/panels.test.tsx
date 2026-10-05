/**
 * Workspace panel tests (cockpit consolidation):
 *   - TickerStrip: ticker@ stats render, symbol select writes the
 *     shared orderDraft (all panels rebind through the one seam).
 *   - MarketTrades: trades@ tape rows newest-first, bounded.
 *   - ChartPanel: interval buttons re-render TradingChart + persist.
 *   - BlotterPanel: tabs switch surfaces; cancel goes through the
 *     shared cancelOrder + ConfirmAction pair.
 */
import { Suspense } from 'react';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';
import { connectWs, makeWsHarness, pushEvent } from '@/lib/trading/testkit';
import { useOrderDraft } from '@/lib/trading/orderDraft';
import { useWatchlistStore } from '@/lib/alerts/watchlist';

import { TickerStrip } from './TickerStrip';
import { MarketTrades } from './MarketTrades';
import { ChartPanel } from './ChartPanel';
import { BlotterPanel } from './BlotterPanel';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

vi.mock('@/features/charts/TradingChart', () => ({
  default: ({ symbol, interval }: { symbol: string; interval: string }) => (
    <div data-testid="trading-chart" data-symbol={symbol} data-interval={interval} />
  ),
}));

vi.mock('@/features/advanced-orders/PositionsPanel', () => ({
  PositionsPanel: () => <div data-testid="positions-panel" />,
}));

const INSTRUMENTS = [
  {
    symbol: 'EUR/USD',
    base_currency: 'EUR',
    quote_currency: 'USD',
    instrument_type: 'SPOT',
    status: 'ACTIVE',
    tick_size: '0.00001',
    lot_size: '1000',
    min_order_qty: '1000',
    max_order_qty: '100000000',
    min_notional: '100',
    min_price: null,
    max_price: null,
    price_band_pct_up: '10',
    price_band_pct_down: '10',
    max_spread_pips: null,
    max_open_orders: null,
    max_algo_orders: null,
    max_leverage: 30,
    settlement_cycle: 1,
  },
  {
    symbol: 'GBP/USD',
    base_currency: 'GBP',
    quote_currency: 'USD',
    instrument_type: 'SPOT',
    status: 'ACTIVE',
    tick_size: '0.00001',
    lot_size: '1000',
    min_order_qty: '1000',
    max_order_qty: '100000000',
    min_notional: '100',
    min_price: null,
    max_price: null,
    price_band_pct_up: '10',
    price_band_pct_down: '10',
    max_spread_pips: null,
    max_open_orders: null,
    max_algo_orders: null,
    max_leverage: 30,
    settlement_cycle: 1,
  },
];

const OPEN_ORDER = {
  order_id: 41,
  symbol: 'EUR/USD',
  side: 'BUY',
  type: 'LIMIT',
  time_in_force: 'GTC',
  quantity: '2000',
  filled_qty: '0',
  price: '1.09',
  status: 'ACTIVE',
  order_seq: 7,
  created_at: '2026-01-01T00:00:00Z',
};

const FETCHES = {
  'GET /api/v1/instruments': { body: { data: INSTRUMENTS, count: 2, server_time_ms: 1 } },
  'GET /api/v1/orders*': { body: { data: [OPEN_ORDER], next_cursor: null } },
  'DELETE /api/v1/orders/41': { body: {} },
};

beforeEach(() => {
  localStorage.clear();
  useWatchlistStore.setState({ lists: {} });
  useOrderDraft.setState({ draft: { symbol: 'EUR/USD', side: 'BUY', price: '', quantity: '' } });
});

describe('TickerStrip', () => {
  it('renders ticker@ stats and the symbol switcher rebinds orderDraft', async () => {
    const h = makeWsHarness();
    installFetchMock(FETCHES);
    signInForTests();
    renderApp(<TickerStrip symbol="EUR/USD" client={h.client} />);
    const sock = await connectWs(h);

    pushEvent(sock, 'ticker@EUR/USD', 1, {
      symbol: 'EUR/USD',
      open: '1.08',
      high: '1.11',
      low: '1.07',
      close: '1.10010',
      volume: '250000',
      quote_volume: '275000',
      price_change: '0.0201',
      price_change_pct: '1.86',
      weighted_avg_price: '1.09',
      trade_count: 42,
      open_time_ms: 0,
      close_time_ms: 1,
    });

    expect(await screen.findByText('1.10010')).toBeInTheDocument();
    expect(screen.getByText('+1.86%')).toBeInTheDocument();
    expect(screen.getByText('1.11000')).toBeInTheDocument(); // 24h High

    // Symbol switch writes the canonical orderDraft — every panel rebinds.
    await userEvent.selectOptions(await screen.findByLabelText('Symbol'), 'GBP/USD');
    expect(useOrderDraft.getState().draft.symbol).toBe('GBP/USD');
  });

  it('star toggles the canonical per-account watchlist', async () => {
    const h = makeWsHarness();
    installFetchMock(FETCHES);
    signInForTests({ accountId: 7 });
    renderApp(<TickerStrip symbol="EUR/USD" client={h.client} />);

    await userEvent.click(await screen.findByRole('button', { name: 'Add EUR/USD to watchlist' }));
    expect(useWatchlistStore.getState().lists['7']).toContain('EUR/USD');
  });
});

describe('MarketTrades', () => {
  it('renders trades@ rows newest-first and drops malformed frames', async () => {
    const h = makeWsHarness();
    installFetchMock(FETCHES);
    signInForTests();
    renderApp(<MarketTrades symbol="EUR/USD" client={h.client} />);
    const sock = await connectWs(h);

    expect(await screen.findByText(/Waiting for prints/)).toBeInTheDocument();
    pushEvent(sock, 'trades@EUR/USD', 1, {
      symbol: 'EUR/USD',
      trade_id: 5,
      price: '1.10',
      quantity: '1000',
      side: 'BUY',
      ts_ms: 1,
    });
    pushEvent(sock, 'trades@EUR/USD', 2, {
      symbol: 'EUR/USD',
      trade_id: 6,
      price: '1.11',
      quantity: '500',
      side: 'SELL',
      ts_ms: 2,
    });
    pushEvent(sock, 'trades@EUR/USD', 3, { garbage: true });

    const newest = await screen.findByText('1.11000');
    const older = screen.getByText('1.10000');
    // newest first → 1.11 row precedes 1.10 in document order
    expect(newest.compareDocumentPosition(older) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText('SELL')).toBeInTheDocument();
    expect(screen.getByText('BUY')).toBeInTheDocument();
  });
});

describe('ChartPanel', () => {
  // In the app, PanelFrame owns the Suspense boundary around lazy panels;
  // the bare-component tests provide their own.
  const renderChart = () =>
    renderApp(
      <Suspense fallback={null}>
        <ChartPanel symbol="EUR/USD" />
      </Suspense>,
    );

  it('persists the interval choice and passes it to TradingChart', async () => {
    installFetchMock(FETCHES);
    signInForTests();
    renderChart();

    const chart = await screen.findByTestId('trading-chart');
    expect(chart).toHaveAttribute('data-interval', '15m');
    expect(chart).toHaveAttribute('data-symbol', 'EUR/USD');

    await userEvent.click(screen.getByRole('button', { name: '1h' }));
    expect(screen.getByTestId('trading-chart')).toHaveAttribute('data-interval', '1h');
    expect(localStorage.getItem('exc.chart.interval.v1')).toBe('1h');
  });

  it('restores a stored interval; ignores corrupt values', async () => {
    localStorage.setItem('exc.chart.interval.v1', '4h');
    installFetchMock(FETCHES);
    signInForTests();
    const { unmount } = renderChart();
    expect(await screen.findByTestId('trading-chart')).toHaveAttribute('data-interval', '4h');
    unmount();

    localStorage.setItem('exc.chart.interval.v1', 'bogus');
    renderChart();
    expect(await screen.findByTestId('trading-chart')).toHaveAttribute('data-interval', '15m');
  });
});

describe('BlotterPanel', () => {
  it('tabs switch between positions/open/history; cancel uses the shared seam', async () => {
    const calls = installFetchMock(FETCHES);
    signInForTests();
    renderApp(<BlotterPanel />);

    // default: positions tab (canonical PositionsPanel embedded)
    expect(await screen.findByTestId('positions-panel')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('tab', { name: 'Open orders' }));
    expect(await screen.findByText('ACTIVE')).toBeInTheDocument();

    // cancel → ConfirmAction modal → DELETE via cancelOrder
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel order' }));
    await waitFor(() =>
      expect(calls).toContainEqual(
        expect.objectContaining({ method: 'DELETE', url: '/api/v1/orders/41' }),
      ),
    );

    await userEvent.click(screen.getByRole('tab', { name: 'History' }));
    expect(await screen.findByText('ACTIVE')).toBeInTheDocument();
  });
});
