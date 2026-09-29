/**
 * Portfolio tests (Task 10.3.5) — REST seed render, private:positions /
 * private:balances live updates, STALE MARKS badge via channel health,
 * close-position → reduce-only market order.
 */
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiClient } from '@/lib/api';
import { usePendingOrders } from '@/lib/market/pending';
import { installFetchMock, renderApp } from '@/test/accountMocks';
import { WsClient } from '@/lib/ws';
import { FakeClock, SocketFactory } from '@/lib/ws/testkit';

import { Portfolio } from './Portfolio';
import { upsertBalance, upsertPosition } from './usePortfolio';
import type { PositionRow } from '@/lib/market/wire';

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

const POSITIONS = {
  account_id: 1001,
  positions: [
    {
      position_id: 501,
      instrument_id: 7,
      symbol: 'EUR/USD',
      side: 'LONG',
      quantity: '100000',
      entry_price: '1.08000',
      mark_price: '1.08500',
      unrealized_pnl: '500',
      realized_pnl: '0',
      margin_used: '3616.67',
      opened_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    },
  ],
};

const BALANCES = {
  account_id: 1001,
  balances: [
    { currency: 'EUR', available: '50000', locked: '0', total: '50000' },
    { currency: 'USD', available: '95000', locked: '3616.67', total: '98616.67' },
  ],
};

const CLOSE_ACK = {
  order_id: 9100,
  client_order_id: 'web-close-x',
  status: 'NEW',
  order_seq: 1,
  replay: false,
  transact_time: '2026-01-01T00:00:00Z',
};

function harness() {
  const calls = installFetchMock({
    'GET /api/v1/instruments': { body: INSTRUMENTS },
    'GET /api/v1/positions': { body: POSITIONS },
    'GET /api/v1/account/balances': { body: BALANCES },
    'POST /api/v1/orders': { status: 202, body: CLOSE_ACK },
  });
  const api = new ApiClient({ baseUrl: '/api/v1' });
  const clock = new FakeClock();
  const factory = new SocketFactory();
  const ws = new WsClient({
    url: 'ws://test.local/ws',
    socketFactory: factory.make,
    clock,
    tokenProvider: () => null,
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
  act(() => {
    sock.recv({
      type: 'subscribed',
      channels: ['private:positions', 'private:balances'],
      total: 2,
      ts_ms: h.clock.now(),
    });
  });
  return sock;
}

beforeEach(() => {
  vi.restoreAllMocks();
  usePendingOrders.getState().reset();
});

describe('upsert reducers', () => {
  const row: PositionRow = {
    positionId: 501,
    instrumentId: 7,
    symbol: 'EUR/USD',
    side: 'LONG',
    quantity: '100000',
    entryPrice: '1.08',
    markPrice: '1.085',
    unrealizedPnl: '500',
    realizedPnl: '0',
    marginUsed: '3616.67',
    openedAt: '',
    updatedAt: '',
  };

  it('position event updates mark/pnl in place', () => {
    const next = upsertPosition([row], {
      positionId: 501,
      symbol: 'EUR/USD',
      markPrice: '1.09',
      unrealizedPnl: '1000',
    });
    expect(next).toHaveLength(1);
    expect(next[0]?.markPrice).toBe('1.09');
    expect(next[0]?.unrealizedPnl).toBe('1000');
    expect(next[0]?.entryPrice).toBe('1.08'); // untouched
  });

  it('closed event removes the row', () => {
    const next = upsertPosition([row], { positionId: 501, symbol: 'EUR/USD', closed: true });
    expect(next).toHaveLength(0);
  });

  it('new-position event materializes a full row only when complete', () => {
    const partial = upsertPosition([], { symbol: 'GBP/USD', markPrice: '1.2' });
    expect(partial).toHaveLength(0);
    const full = upsertPosition([], {
      positionId: 600,
      symbol: 'GBP/USD',
      side: 'SHORT',
      quantity: '50000',
      entryPrice: '1.25',
      markPrice: '1.25',
    });
    expect(full).toHaveLength(1);
    expect(full[0]?.side).toBe('SHORT');
  });

  it('balance event upserts by currency and sorts', () => {
    const next = upsertBalance([{ currency: 'USD', available: '1', locked: '0', total: '1' }], {
      currency: 'EUR',
      available: '2',
      locked: '0',
      total: '2',
    });
    expect(next.map((b) => b.currency)).toEqual(['EUR', 'USD']);
    const upd = upsertBalance(next, { currency: 'USD', available: '5', locked: '1', total: '6' });
    expect(upd.find((b) => b.currency === 'USD')?.available).toBe('5');
  });
});

describe('Portfolio', () => {
  it('renders REST-seeded positions and balances', async () => {
    const h = harness();
    renderApp(<Portfolio api={h.api} ws={h.ws} />);
    await waitFor(() => expect(screen.getByText('EUR/USD')).toBeInTheDocument());
    expect(screen.getByText('LONG')).toBeInTheDocument();
    expect(screen.getByText('1.08500')).toBeInTheDocument(); // mark at tick dp
    expect(screen.getByText('+500.00')).toBeInTheDocument(); // signed uPnL
    // margin column AND USD locked balance both render 3,616.67
    expect(screen.getAllByText('3,616.67').length).toBeGreaterThanOrEqual(2);
    // balances
    expect(screen.getByText('USD')).toBeInTheDocument();
    expect(screen.getByText('98,616.67')).toBeInTheDocument();
  });

  it('applies live private:positions + private:balances frames', async () => {
    const h = harness();
    renderApp(<Portfolio api={h.api} ws={h.ws} />);
    const sock = await connect(h);
    await waitFor(() => expect(screen.getByText('+500.00')).toBeInTheDocument());
    act(() => {
      sock.recv({
        type: 'event',
        channel: 'private:positions',
        seq: 1,
        ts_ms: h.clock.now(),
        data: {
          event: 'positionUpdate',
          position_id: 501,
          symbol: 'EUR/USD',
          mark_price: '1.09000',
          unrealized_pnl: '1000',
          ts_ms: h.clock.now(),
        },
      });
      sock.recv({
        type: 'event',
        channel: 'private:balances',
        seq: 2,
        ts_ms: h.clock.now(),
        data: {
          event_type: 'BALANCE_CHANGED',
          account_id: 1001,
          currency: 'USD',
          journal_id: 9,
          available: '95500',
          locked: '3616.67',
          total: '99116.67',
        },
      });
    });
    await waitFor(() => expect(screen.getByText('+1,000.00')).toBeInTheDocument());
    expect(screen.getByText('1.09000')).toBeInTheDocument();
    expect(screen.getByText('99,116.67')).toBeInTheDocument();
  });

  it('closes a position via reduce-only opposing market order', async () => {
    const h = harness();
    renderApp(<Portfolio api={h.api} ws={h.ws} />);
    await connect(h);
    await waitFor(() => expect(screen.getByText('EUR/USD')).toBeInTheDocument());
    await userEvent.click(screen.getByRole('button', { name: 'Close EUR/USD LONG' }));
    await waitFor(() => {
      const post = h.calls.find((c) => c.method === 'POST' && c.url.includes('/orders'));
      expect(post).toBeDefined();
      const body = JSON.parse(post?.init?.body as string) as Record<string, unknown>;
      expect(body).toMatchObject({
        symbol: 'EUR/USD',
        side: 'SELL', // opposing the LONG
        type: 'MARKET',
        time_in_force: 'IOC',
        quantity: '100000',
        reduce_only: true,
      });
      expect(new Headers(post?.init?.headers).get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
    });
  });

  it('flags STALE MARKS when the positions channel goes silent', async () => {
    const h = harness();
    renderApp(<Portfolio api={h.api} ws={h.ws} />);
    const sock = await connect(h);
    await waitFor(() => expect(screen.getByText('EUR/USD')).toBeInTheDocument());
    act(() => {
      sock.recv({
        type: 'event',
        channel: 'private:positions',
        seq: 1,
        ts_ms: h.clock.now(),
        data: {
          event: 'positionUpdate',
          position_id: 501,
          symbol: 'EUR/USD',
          mark_price: '1.09',
          ts_ms: h.clock.now(),
        },
      });
    });
    act(() => {
      h.clock.advance(4_000); // >3s silence → stale flag + monitor notify
    });
    await waitFor(() => expect(screen.getByText('STALE MARKS')).toBeInTheDocument());
  });
});
