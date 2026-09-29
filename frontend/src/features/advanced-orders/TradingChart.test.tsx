/**
 * TradingChart tests (Task 10.3.15) — candles render, open-order
 * overlays are focusable + keyboard-repriceable (opening the inspect
 * modal with the proposed price and the priority-loss warning), fills
 * show as markers, candle countdown renders for fixed intervals.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { installFetchMock, renderApp } from '@/test/accountMocks';
import { makeWsHarness } from '@/lib/trading/testkit';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

import { TradingChart } from './TradingChart';

const NOW = Date.now();

const KLINES = {
  data: Array.from({ length: 30 }, (_, i) => {
    const o = 1.1 + i * 0.0001;
    return {
      open_time_ms: NOW - (30 - i) * 900_000,
      open: o.toFixed(5),
      high: (o + 0.0004).toFixed(5),
      low: (o - 0.0004).toFixed(5),
      close: (o + 0.0002).toFixed(5),
      volume: '1000',
      closed: i < 29,
    };
  }),
};

const ORDERS = {
  data: [
    {
      order_id: '501',
      symbol: 'EUR/USD',
      side: 'BUY',
      type: 'LIMIT',
      time_in_force: 'GTC',
      quantity: '100000',
      filled_qty: '0',
      price: '1.09950',
      status: 'ACTIVE',
      order_seq: 7,
      created_at: '2026-01-01T00:00:00Z',
    },
    {
      order_id: '502',
      symbol: 'EUR/USD',
      side: 'BUY',
      type: 'LIMIT',
      time_in_force: 'GTC',
      quantity: '100000',
      filled_qty: '100000',
      price: '1.10200',
      avg_fill_price: '1.10195',
      status: 'FILLED',
      order_seq: 6,
      created_at: new Date(NOW - 10 * 900_000).toISOString(),
    },
  ],
};

function fetchMock() {
  return installFetchMock({
    'GET /api/v1/klines*': { body: KLINES },
    'GET /api/v1/orders*': { body: ORDERS },
    'GET /api/v1/positions': { body: { positions: [] } },
    'GET /api/v1/instruments': {
      body: {
        data: [
          {
            symbol: 'EUR/USD',
            base_currency: 'EUR',
            quote_currency: 'USD',
            tick_size: '0.00001',
            lot_size: '1000',
            min_order_qty: '1000',
            max_leverage: 30,
            status: 'ACTIVE',
          },
        ],
      },
    },
  });
}

describe('TradingChart', () => {
  it('renders candles, order overlays, fill marker and countdown', async () => {
    const h = makeWsHarness();
    fetchMock();
    renderApp(<TradingChart symbol="EUR/USD" interval="15m" client={h.client} />);

    const svg = await screen.findByRole('img', { name: /price chart — 30 candles/ });
    expect(svg).toBeInTheDocument();
    // open-order overlay: role=button labelled with the order id
    expect(await screen.findByRole('button', { name: /501 at 1\.0995/ })).toBeInTheDocument();
    // fill marker for the FILLED order
    expect(screen.getByLabelText(/fill BUY 502/)).toBeInTheDocument();
    // candle countdown for a fixed-width interval
    expect(screen.getByText(/candle closes in/)).toBeInTheDocument();
  });

  it('keyboard ArrowUp on an order line opens the inspect modal with the proposed price', async () => {
    const h = makeWsHarness();
    fetchMock();
    renderApp(<TradingChart symbol="EUR/USD" interval="15m" client={h.client} />);

    const line = await screen.findByRole('button', { name: /501 at 1\.0995/ });
    line.focus();
    await userEvent.keyboard('{ArrowUp}');

    const dialog = await screen.findByRole('dialog', { name: /Order 501/ });
    expect(dialog).toBeInTheDocument();
    // tick size 0.00001 → proposed 1.09951
    const priceInput = screen.getByLabelText<HTMLInputElement>('New price');
    await waitFor(() => expect(priceInput.value).toBe('1.09951'));
    // priority-loss warning is visible because the price changed
    expect(screen.getByText(/queue position is lost/i)).toBeInTheDocument();
  });
});
