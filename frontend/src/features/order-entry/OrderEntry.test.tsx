/**
 * OrderEntry tests (Task 10.3.3 + 10.3.19 residual):
 *   - validation → inline errors, no POST
 *   - review → /orders/test preview → confirm → idempotent /orders POST
 *   - optimistic pending → REST ack confirm, RFC 7807 reject rollback,
 *     WS orderReject rollback, TTL timeout, auth-epoch flush
 *   - Task 10.3.19 lock: order entry disabled outside AUTHENTICATED/STALE
 */
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiClient } from '@/lib/api';
import { useSessionStore } from '@/lib/auth/session';
import { usePendingOrders } from '@/lib/market/pending';
import { installFetchMock, renderApp } from '@/test/accountMocks';
import { WsClient } from '@/lib/ws';
import { FakeClock, SocketFactory } from '@/lib/ws/testkit';

import { OrderEntry } from './OrderEntry';

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

const PREVIEW = {
  estimated_base_qty: '1000',
  estimated_quote_qty: '1085',
  margin: '36.17',
  commission_estimate: '0.05',
  spread_estimate: '0.0001',
  risk_level: 'LOW',
  warnings: [],
  active_filters: ['PRICE_FILTER'],
  binding: false,
};

const ACK = {
  order_id: 9001,
  client_order_id: 'web-test-1',
  status: 'NEW',
  order_seq: 42,
  replay: false,
  transact_time: '2026-01-01T00:00:00Z',
};

function harness(routes: Record<string, { status?: number; body?: unknown }> = {}) {
  const calls = installFetchMock({
    'GET /api/v1/instruments': { body: INSTRUMENTS },
    'POST /api/v1/orders/test': { body: PREVIEW },
    'POST /api/v1/orders': { status: 202, body: ACK },
    ...routes,
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
    sock.recv({ type: 'subscribed', channels: ['private:orders'], total: 1, ts_ms: h.clock.now() });
  });
  return sock;
}

async function fillAndReview(fields: { price?: string; qty?: string } = {}) {
  if (fields.price !== undefined) {
    const el = screen.getByLabelText('Price');
    await userEvent.clear(el);
    await userEvent.type(el, fields.price);
  }
  const qty = screen.getByLabelText('Quantity');
  await userEvent.clear(qty);
  await userEvent.type(qty, fields.qty ?? '1000');
  await userEvent.click(screen.getByRole('button', { name: /Review Buy/ }));
}

beforeEach(() => {
  vi.restoreAllMocks();
  usePendingOrders.getState().reset();
});

describe('OrderEntry', () => {
  it('validates inline and never reaches the wire on bad input', async () => {
    const h = harness();
    renderApp(<OrderEntry symbol="EUR/USD" api={h.api} ws={h.ws} />);
    await connect(h);
    await waitFor(() => expect(screen.getByRole('button', { name: /Review Buy/ })).toBeEnabled());
    await fillAndReview({ price: '1.085001', qty: '0' }); // off-tick price + zero qty
    expect(await screen.findAllByRole('alert')).not.toHaveLength(0);
    expect(h.calls.filter((c) => c.method === 'POST')).toHaveLength(0);
  });

  it('locks order entry while the socket is not authenticated (10.3.19)', async () => {
    const h = harness();
    renderApp(<OrderEntry symbol="EUR/USD" api={h.api} ws={h.ws} />);
    await waitFor(() => expect(screen.getByLabelText('Quantity')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: /Review Buy/ })).toBeDisabled();
    expect(screen.getByText(/Order entry locked/)).toBeInTheDocument();
  });

  it('enables entry once AUTHENTICATED, previews then submits idempotently', async () => {
    const h = harness();
    renderApp(<OrderEntry symbol="EUR/USD" api={h.api} ws={h.ws} />);
    await connect(h);
    await waitFor(() => expect(screen.getByRole('button', { name: /Review Buy/ })).toBeEnabled());
    await fillAndReview({ price: '1.08500', qty: '1000' });

    // confirmation modal with the /orders/test estimate
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId('order-preview')).toBeInTheDocument());
    expect(screen.getByText('36.17')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: /Confirm Buy/ }));

    // POST /orders carried Idempotency-Key + canonical fields
    await waitFor(() => {
      const post = h.calls.find(
        (c) => c.method === 'POST' && c.url.includes('/orders') && !c.url.includes('test'),
      );
      expect(post).toBeDefined();
      const headers = new Headers(post?.init?.headers);
      expect(headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
      const body = JSON.parse(post?.init?.body as string) as Record<string, unknown>;
      expect(body).toMatchObject({
        symbol: 'EUR/USD',
        side: 'BUY',
        type: 'LIMIT',
        time_in_force: 'GTC',
        quantity: '1000',
        price: '1.085',
      });
      expect(body['client_order_id']).toMatch(/^web-/);
    });
    // preview call carried NO idempotency key (it moves no money)
    const previewCall = h.calls.find((c) => c.url.includes('/orders/test'));
    expect(new Headers(previewCall?.init?.headers).get('Idempotency-Key')).toBeNull();

    // optimistic entry confirmed by the REST ack → notice
    await waitFor(() =>
      expect(screen.getByTestId('order-notices')).toHaveTextContent('Order accepted (id 9001)'),
    );
    expect(screen.queryByTestId('pending-orders')).not.toBeInTheDocument();
  });

  it('rolls back + shows the RFC 7807 code/request_id on INSUFFICIENT_BALANCE', async () => {
    const h = harness({
      'POST /api/v1/orders': {
        status: 400,
        body: {
          type: 'error',
          error: 'INSUFFICIENT_BALANCE',
          message: 'Not enough available balance',
          status: 400,
          request_id: 'req-77',
        },
      },
    });
    renderApp(<OrderEntry symbol="EUR/USD" api={h.api} ws={h.ws} />);
    await connect(h);
    await waitFor(() => expect(screen.getByRole('button', { name: /Review Buy/ })).toBeEnabled());
    await fillAndReview({ price: '1.08500', qty: '1000' });
    await userEvent.click(await screen.findByRole('button', { name: /Confirm Buy/ }));

    await waitFor(() => expect(screen.getByTestId('order-notices')).toBeInTheDocument());
    expect(screen.getByTestId('order-notices')).toHaveTextContent('INSUFFICIENT_BALANCE');
    expect(screen.getByTestId('order-notices')).toHaveTextContent('req-77');
    expect(screen.getByTestId('order-notices')).toHaveTextContent('Insufficient available balance');
    expect(screen.queryByTestId('pending-orders')).not.toBeInTheDocument();
  });

  it('rolls back a pending order on a WS private:orders orderReject', async () => {
    const h = harness();
    renderApp(<OrderEntry symbol="EUR/USD" api={h.api} ws={h.ws} />);
    const sock = await connect(h);
    // simulate an entry still awaiting confirmation
    act(() => {
      usePendingOrders.getState().add({
        clientOrderId: 'web-ws1',
        symbol: 'EUR/USD',
        side: 'SELL',
        type: 'LIMIT',
        quantity: '500',
        price: '1.09',
        epoch: useSessionStore.getState().authEpoch,
      });
    });
    await waitFor(() => expect(screen.getByTestId('pending-orders')).toHaveTextContent('web-ws1'));
    act(() => {
      sock.recv({
        type: 'event',
        channel: 'private:orders',
        seq: 1,
        ts_ms: h.clock.now(),
        data: {
          event: 'orderReject',
          order_id: '',
          client_order_id: 'web-ws1',
          symbol: 'EUR/USD',
          status: 'REJECTED',
          reason: 'ORDER_REJECTED_NO_LIQUIDITY',
          ts_ms: h.clock.now(),
        },
      });
    });
    await waitFor(() =>
      expect(screen.getByTestId('order-notices')).toHaveTextContent('ORDER_REJECTED_NO_LIQUIDITY'),
    );
    expect(screen.queryByTestId('pending-orders')).not.toBeInTheDocument();
  });

  it('times out unconfirmed orders with a rollback notice', async () => {
    // /orders hangs — no ack within the TTL
    installFetchMock({ 'GET /api/v1/instruments': { body: INSTRUMENTS } });
    const hanging = new ApiClient({
      baseUrl: '/api/v1',
      fetchImpl: (input) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (url.includes('/orders') && !url.includes('test')) {
          return new Promise<Response>(() => undefined); // never resolves
        }
        return fetch(input);
      },
    });
    const h = harness(); // installs fetch mock again; reuse ws/clock
    renderApp(<OrderEntry symbol="EUR/USD" api={hanging} ws={h.ws} pendingTtlMs={60} />);
    await connect(h);
    await waitFor(() => expect(screen.getByRole('button', { name: /Review Buy/ })).toBeEnabled());
    await fillAndReview({ price: '1.08500', qty: '1000' });
    await userEvent.click(await screen.findByRole('button', { name: /Confirm Buy/ }));
    await waitFor(() => expect(screen.getByTestId('pending-orders')).toBeInTheDocument());
    await waitFor(
      () => expect(screen.getByTestId('order-notices')).toHaveTextContent('unconfirmed'),
      { timeout: 2000 },
    );
    expect(screen.queryByTestId('pending-orders')).not.toBeInTheDocument();
  });

  it('flushes pending orders across a re-auth epoch bump (10.3.19 invariant)', async () => {
    const h = harness();
    renderApp(<OrderEntry symbol="EUR/USD" api={h.api} ws={h.ws} />);
    await connect(h);
    act(() => {
      usePendingOrders.getState().add({
        clientOrderId: 'web-old',
        symbol: 'EUR/USD',
        side: 'BUY',
        type: 'LIMIT',
        quantity: '100',
        epoch: useSessionStore.getState().authEpoch,
      });
    });
    await waitFor(() => expect(screen.getByTestId('pending-orders')).toBeInTheDocument());
    // flush-optimistic effect → session.authEpoch++
    act(() => {
      useSessionStore.getState().bumpAuthEpoch();
    });
    await waitFor(() =>
      expect(screen.getByTestId('order-notices')).toHaveTextContent('re-authenticated'),
    );
    expect(screen.queryByTestId('pending-orders')).not.toBeInTheDocument();
  });
});
