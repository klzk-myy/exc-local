/**
 * OrderBook component tests — real WsClient against MockSocket + FakeClock,
 * real ApiClient against the fetch route-table. Covers REST seed, live
 * depth@ frames, depth selector re-subscription, stale badge, price click.
 */
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiClient } from '@/lib/api';
import { installFetchMock, renderApp } from '@/test/accountMocks';
import { WsClient } from '@/lib/ws';
import { FakeClock, SocketFactory } from '@/lib/ws/testkit';

import { OrderBook } from './OrderBook';

const INSTRUMENTS = {
  data: [
    {
      symbol: 'EUR/USD',
      base_currency: 'EUR',
      quote_currency: 'USD',
      instrument_type: 'SPOT',
      status: 'ACTIVE',
      tick_size: '0.00001',
      lot_size: '1',
      min_order_qty: '1',
      max_order_qty: '10000000',
      min_notional: '1',
      max_leverage: 30,
      settlement_cycle: 1,
      settlement: 'T+1',
    },
  ],
};

const BOOK = {
  symbol: 'EUR/USD',
  seq: 1,
  depth: 20,
  bids: [
    { price: '1.08500', quantity: '1000' },
    { price: '1.08490', quantity: '2500' },
  ],
  asks: [
    { price: '1.08520', quantity: '800' },
    { price: '1.08530', quantity: '1000' },
  ],
  updated_at_ms: 1_700_000_000_000,
};

function harness(book: unknown = BOOK) {
  const calls = installFetchMock({
    'GET /api/v1/instruments': { body: INSTRUMENTS },
    'GET /api/v1/book/EUR%2FUSD': { body: book },
  });
  const api = new ApiClient({ baseUrl: '/api/v1' });
  const clock = new FakeClock();
  const factory = new SocketFactory();
  const ws = new WsClient({
    url: 'ws://test.local/ws',
    socketFactory: factory.make,
    clock,
    tokenProvider: () => null, // anonymous → AUTHENTICATED on open
    isTradingSession: () => true,
  });
  return { calls, api, clock, factory, ws };
}

async function connect(h: ReturnType<typeof harness>) {
  h.ws.start();
  const sock = h.factory.latest();
  await act(async () => {
    sock.open();
    await Promise.resolve();
  });
  return sock;
}

/** The subscribe made during CONNECTING converges the RESYNCING session
 * on its `subscribed` ack (Task 10.3.19 — RESYNCING until convergence). */
function converge(sock: ReturnType<SocketFactory['latest']>, channel: string, clock: FakeClock) {
  act(() => {
    sock.recv({ type: 'subscribed', channels: [channel], total: 1, ts_ms: clock.now() });
  });
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe('OrderBook', () => {
  it('seeds from the REST snapshot and renders both sides + spread/mid', async () => {
    const h = harness();
    renderApp(<OrderBook symbol="EUR/USD" api={h.api} ws={h.ws} />);
    expect(screen.getByRole('status')).toHaveTextContent('Loading book');
    await waitFor(() => expect(screen.getByText('1.08500')).toBeInTheDocument());
    // asks render worst→best: 1.08530 listed before 1.08520 in DOM order
    const asks = screen.getByTestId('book-asks');
    const askText = asks.textContent ?? '';
    expect(askText.indexOf('1.08530')).toBeLessThan(askText.indexOf('1.08520'));
    // spread = 0.00020, mid = 1.08510 (tick 5dp)
    const spread = screen.getByTestId('book-spread');
    expect(spread).toHaveTextContent('0.00020');
    expect(spread).toHaveTextContent('1.08510');
  });

  it('applies live depth@ frames (server-conflated replace)', async () => {
    const h = harness();
    renderApp(<OrderBook symbol="EUR/USD" api={h.api} ws={h.ws} />);
    const sock = await connect(h);
    await waitFor(() => expect(screen.getByText('1.08500')).toBeInTheDocument());
    // subscription went out on the canonical channel
    expect(sock.sentFrames().some((f) => (f['params'] as string[]).includes('depth@EUR/USD'))).toBe(
      true,
    );
    act(() => {
      sock.recv({
        type: 'event',
        channel: 'depth@EUR/USD',
        seq: 1,
        ts_ms: h.clock.now(),
        data: {
          symbol: 'EUR/USD',
          bids: [['1.08510', '2000', '1']],
          asks: [['1.08530', '900', '2']],
          last_seq: 2,
          seq: 2,
          ts_ms: h.clock.now(),
        },
      });
    });
    await waitFor(() => expect(screen.getByText('1.08510')).toBeInTheDocument());
    expect(screen.getByTestId('book-spread')).toHaveTextContent('1.08520'); // mid
  });

  it('depth selector re-fetches + re-subscribes at the new variant', async () => {
    const h = harness();
    renderApp(<OrderBook symbol="EUR/USD" api={h.api} ws={h.ws} />);
    const sock = await connect(h);
    converge(sock, 'depth@EUR/USD', h.clock);
    await waitFor(() => expect(screen.getByText('1.08500')).toBeInTheDocument());
    await userEvent.click(screen.getByRole('button', { name: '5' }));
    await waitFor(() =>
      expect(
        h.calls.some((c) => c.url.includes('/book/EUR%2FUSD') && c.url.includes('depth=5')),
      ).toBe(true),
    );
    await waitFor(() =>
      expect(
        sock
          .sentFrames()
          .some(
            (f) =>
              f['action'] === 'subscribe' &&
              (f['params'] as string[]).includes('depth@EUR/USD:5:100'),
          ),
      ).toBe(true),
    );
  });

  it('shows STALE when the channel goes silent >3s during trading hours', async () => {
    const h = harness();
    renderApp(<OrderBook symbol="EUR/USD" api={h.api} ws={h.ws} />);
    const sock = await connect(h);
    converge(sock, 'depth@EUR/USD', h.clock);
    await waitFor(() => expect(screen.getByText('1.08500')).toBeInTheDocument());
    act(() => {
      sock.recv({
        type: 'event',
        channel: 'depth@EUR/USD',
        seq: 1,
        ts_ms: h.clock.now(),
        data: {
          symbol: 'EUR/USD',
          bids: [['1.08500', '1000', '1']],
          asks: [['1.08520', '800', '1']],
          last_seq: 2,
          seq: 2,
          ts_ms: h.clock.now(),
        },
      });
    });
    // silence > STALE_TICK_THRESHOLD_MS (3s) → stale monitor tick
    act(() => {
      h.clock.advance(4_000);
    });
    await waitFor(() => expect(screen.getByText('STALE')).toBeInTheDocument());
  });

  it('emits onPriceClick for quick-order prefill', async () => {
    const h = harness();
    const onPriceClick = vi.fn();
    renderApp(<OrderBook symbol="EUR/USD" api={h.api} ws={h.ws} onPriceClick={onPriceClick} />);
    await waitFor(() => expect(screen.getByText('1.08500')).toBeInTheDocument());
    await userEvent.click(screen.getByRole('button', { name: 'bid 1.08500' }));
    expect(onPriceClick).toHaveBeenCalledWith('1.08500', 'bid');
  });

  it('surfaces REST failure with a retry affordance', async () => {
    const h = harness({ error: 'nope' }); // malformed → parse fails → error UI
    renderApp(<OrderBook symbol="EUR/USD" api={h.api} ws={h.ws} />);
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });
});
