import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import { parseAlgoOrder, parseOrderListSummary, fetchOrdersPage } from './api';
import { DeadmanSwitch } from './DeadmanSwitch';
import { OrderListsPanel } from './OrderListsPanel';
import { AlgoPanel } from './AlgoPanel';
import { BatchOpsPanel, CompositeSubmitPanel } from './CompositeSubmitPanel';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const STUB_501 = {
  status: 501,
  body: { type: 'error', error: 'NOT_IMPLEMENTED', message: 'stub', status: 501 },
};

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
  signInForTests();
});

describe('history api narrowing', () => {
  it('parseAlgoOrder / parseOrderListSummary reject junk', () => {
    expect(
      parseAlgoOrder({
        algo_order_id: 'a1',
        symbol: 'EUR/USD',
        algo_type: 'TWAP',
        status: 'RUNNING',
      }),
    ).toMatchObject({ id: 'a1', algoType: 'TWAP' });
    expect(parseAlgoOrder({})).toBeNull();
    expect(
      parseOrderListSummary({
        list_id: 'l1',
        type: 'OCO',
        symbol: 'EUR/USD',
        status: 'OPEN',
        orders: [{}, {}],
      }),
    ).toMatchObject({ id: 'l1', type: 'OCO', legs: 2 });
    expect(parseOrderListSummary('x')).toBeNull();
  });

  it('fetchOrdersPage sends the registered filter params + cursor', async () => {
    const calls = installFetchMock({
      'GET /api/v1/orders': {
        status: 200,
        body: { data: [], next_cursor: 'abc', limit: 100, total: 0 },
      },
    });
    const { apiClient: api } = await import('@/app/runtime');
    const page = await fetchOrdersPage(
      {
        symbol: 'EUR/USD',
        side: 'BUY',
        status: 'FILLED',
        type: 'LIMIT',
        cursor: 'c9',
        from: '2026-01-01T00:00:00Z',
        to: '2026-02-01T00:00:00Z',
      },
      'master',
      api,
    );
    expect(page.nextCursor).toBe('abc');
    const url = calls.find((c) => c.url.includes('/orders'))?.url ?? '';
    expect(url).toContain('symbol=EUR%2FUSD');
    expect(url).toContain('side=BUY');
    expect(url).toContain('status=FILLED');
    expect(url).toContain('cursor=c9');
    expect(url).toContain('from=');
  });
});

describe('DeadmanSwitch', () => {
  function renderSwitch() {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <DeadmanSwitch />
      </QueryClientProvider>,
    );
  }

  it('blocks out-of-range countdowns before any network call', async () => {
    const calls = installFetchMock({});
    renderSwitch();
    const input = screen.getByLabelText('Countdown (ms)');
    fireEvent.change(input, { target: { value: '500' } });
    fireEvent.blur(input);
    expect(await screen.findByRole('alert')).toHaveTextContent(/below minimum/);
    expect(screen.getByRole('button', { name: 'Arm' })).toBeDisabled();
    expect(calls.filter((c) => c.url.includes('countdown-cancel-all'))).toHaveLength(0);
  });

  it('surfaces the honest 501 regression note when the route reports NOT_IMPLEMENTED', async () => {
    installFetchMock({ 'POST /api/v1/orders/countdown-cancel-all': STUB_501 });
    renderSwitch();
    const input = screen.getByLabelText('Countdown (ms)');
    fireEvent.change(input, { target: { value: '60000' } });
    fireEvent.click(screen.getByRole('button', { name: 'Arm' }));
    await waitFor(() => {
      expect(screen.getByText(/NOT_IMPLEMENTED \(501\).*regression/i)).toBeInTheDocument();
    });
  });

  it('shows the armed countdown only after a successful arm', async () => {
    installFetchMock({ 'POST /api/v1/orders/countdown-cancel-all': { status: 200, body: {} } });
    renderSwitch();
    const input = screen.getByLabelText('Countdown (ms)');
    fireEvent.change(input, { target: { value: '30000' } });
    fireEvent.click(screen.getByRole('button', { name: 'Arm' }));
    await waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent(/armed/);
    });
  });
});

describe('OrderListsPanel + AlgoPanel', () => {
  it('render UnavailablePanel on 501 (no fabricated lists/algos)', async () => {
    installFetchMock({
      'GET /api/v1/order-lists': STUB_501,
      'GET /api/v1/order-lists/history': STUB_501,
      'GET /api/v1/algo-orders': STUB_501,
    });
    renderApp(<OrderListsPanel />);
    await waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent('OPO / OCO order lists');
    });
  });

  it('renders algo rows when the endpoint serves data', async () => {
    installFetchMock({
      'GET /api/v1/algo-orders': {
        status: 200,
        body: {
          data: [
            {
              algo_order_id: 'a1',
              symbol: 'EUR/USD',
              algo_type: 'TWAP',
              status: 'RUNNING',
              side: 'BUY',
            },
          ],
        },
      },
    });
    renderApp(<AlgoPanel />);
    await waitFor(() => {
      expect(screen.getByText('TWAP')).toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: 'Pause' })).toBeInTheDocument();
  });

  it('per-row cancel hits DELETE /orders/algo/{id}; Stop-all hits DELETE /algo-orders', async () => {
    const calls = installFetchMock({
      'GET /api/v1/algo-orders': {
        status: 200,
        body: {
          data: [{ algo_order_id: 'a1', symbol: 'EUR/USD', algo_type: 'TWAP', status: 'RUNNING' }],
        },
      },
      'DELETE /api/v1/orders/algo/a1': { status: 200, body: {} },
      'DELETE /api/v1/algo-orders': { status: 200, body: { cancelled: 1 } },
    });
    renderApp(<AlgoPanel />);
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE' && c.url.includes('/orders/algo/a1'))).toBe(
        true,
      );
    });
    // Bulk route is a distinct, double-confirmed action — never the row path.
    fireEvent.click(screen.getByRole('button', { name: 'Stop all algos' }));
    fireEvent.click(screen.getByRole('button', { name: /Confirm: stop ALL/ }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/algo-orders'))).toBe(true);
    });
  });

  it('OPOCO intake posts working BUY + two pending SELL legs (no pending qty)', async () => {
    const calls = installFetchMock({
      'GET /api/v1/order-lists': { status: 200, body: { data: [] } },
      'POST /api/v1/order-lists': { status: 202, body: { list_id: 5 } },
    });
    renderApp(<OrderListsPanel />);
    await userEvent.selectOptions(await screen.findByLabelText('Contingency'), 'OPOCO');
    fireEvent.change(screen.getByLabelText(/Working quantity/), { target: { value: '100000' } });
    fireEvent.change(screen.getByLabelText(/Working price/), { target: { value: '1.1000' } });
    fireEvent.change(screen.getByLabelText(/Pending 1 price/), { target: { value: '1.1200' } });
    fireEvent.change(screen.getByLabelText(/Pending 2 stop price/), {
      target: { value: '1.0800' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Submit OPOCO' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/order-lists'));
      const body = JSON.parse(post?.init?.body as string) as Record<string, unknown>;
      expect(body).toMatchObject({ contingency_type: 'OPOCO', symbol: 'EUR/USD' });
      expect(body['working']).toMatchObject({
        side: 'BUY',
        type: 'LIMIT',
        quantity: '100000',
        price: '1.1000',
      });
      const pending = body['pending'] as Record<string, unknown>[];
      expect(pending).toHaveLength(2);
      expect(pending[0]).toMatchObject({ side: 'SELL', type: 'LIMIT', price: '1.1200' });
      expect(pending[1]).toMatchObject({ side: 'SELL', type: 'STOP', stop_price: '1.0800' });
      expect(pending[0]?.['quantity']).toBeUndefined(); // server recomputes net proceeds
    });
  });

  it('expands a list row into legs detail and cancels it (GET/DELETE /order-lists/{id})', async () => {
    const calls = installFetchMock({
      'GET /api/v1/order-lists': {
        status: 200,
        body: { data: [{ list_id: 'l9', type: 'OCO', symbol: 'EUR/USD', status: 'ACTIVE' }] },
      },
      'GET /api/v1/order-lists/l9': {
        status: 200,
        body: {
          list_id: 9,
          contingency_type: 'OCO',
          state: 'ACTIVE',
          working_order_id: 88,
          legs: [
            { leg_index: 0, role: 'WORKING', order_id: 88, state: 'ACTIVE' },
            { leg_index: 1, role: 'SIBLING', state: 'DORMANT' },
          ],
        },
      },
      'DELETE /api/v1/order-lists/l9': { status: 200, body: { list_id: 9, state: 'CANCELLED' } },
    });
    renderApp(<OrderListsPanel />);
    fireEvent.click(await screen.findByRole('button', { name: 'Detail' }));
    expect(await screen.findByText('DORMANT')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    fireEvent.click(screen.getByRole('button', { name: 'Confirm cancel' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE' && c.url.includes('/order-lists/l9'))).toBe(
        true,
      );
    });
  });
});

describe('CompositeSubmitPanel', () => {
  it('submits TWAP with the flat ParseTypedSubmit body', async () => {
    const calls = installFetchMock({
      'POST /api/v1/orders/twap': { status: 202, body: { algo_order_id: 'a7' } },
    });
    renderApp(<CompositeSubmitPanel />);
    fireEvent.change(screen.getByLabelText(/Total quantity/), { target: { value: '100000' } });
    fireEvent.change(screen.getByLabelText(/Duration seconds/), { target: { value: '600' } });
    fireEvent.click(screen.getByRole('button', { name: /Submit TWAP/ }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/orders/twap'));
      expect(JSON.parse(post?.init?.body as string) as Record<string, unknown>).toMatchObject({
        symbol: 'EUR/USD',
        side: 'BUY',
        total_qty: '100000',
        interval_secs: 60,
        duration_secs: 600,
      });
    });
  });

  it('submits a basket with typed legs to POST /orders/basket', async () => {
    const calls = installFetchMock({
      'POST /api/v1/orders/basket': { status: 200, body: { op_id: 'op-1', status: 'MATCHING' } },
    });
    renderApp(<CompositeSubmitPanel />);
    await userEvent.selectOptions(screen.getByLabelText('Strategy'), 'basket');
    fireEvent.change(screen.getByLabelText('Leg symbol'), { target: { value: 'EUR/USD' } });
    fireEvent.change(screen.getByLabelText('Quantity'), { target: { value: '50000' } });
    fireEvent.change(screen.getByLabelText('Leg 2 quantity'), { target: { value: '30000' } });
    fireEvent.click(screen.getByRole('button', { name: /Submit Basket/ }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/orders/basket'));
      expect(JSON.parse(post?.init?.body as string) as Record<string, unknown>).toMatchObject({
        legs: expect.arrayContaining([
          expect.objectContaining({ symbol: 'EUR/USD', quantity: '50000' }),
        ]) as unknown[],
      });
    });
  });

  it('submits a roll to POST /orders/roll with contract_id + tenor', async () => {
    const calls = installFetchMock({
      'POST /api/v1/orders/roll': { status: 200, body: { roll_id: 5, status: 'EXECUTED' } },
    });
    renderApp(<CompositeSubmitPanel />);
    await userEvent.selectOptions(screen.getByLabelText('Strategy'), 'roll');
    fireEvent.change(screen.getByLabelText(/Contract id/), { target: { value: '123' } });
    fireEvent.change(screen.getByLabelText(/Tenor/), { target: { value: '1M' } });
    fireEvent.click(screen.getByRole('button', { name: /Submit Forward roll/ }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/orders/roll'));
      expect(JSON.parse(post?.init?.body as string) as Record<string, unknown>).toMatchObject({
        contract_id: 123,
        tenor: '1M',
      });
    });
  });
});

describe('BatchOpsPanel', () => {
  it('posts the parsed orders array to /orders/batch and DELETEs batch by ids', async () => {
    const calls = installFetchMock({
      'POST /api/v1/orders/batch': { status: 200, body: { acks: [] } },
      'DELETE /api/v1/orders/batch': { status: 200, body: { cancelled: 2 } },
    });
    renderApp(<BatchOpsPanel />);
    fireEvent.change(screen.getByLabelText(/Batch submit/), {
      target: {
        value:
          '{"orders":[{"symbol":"EUR/USD","side":"BUY","type":"LIMIT","quantity":"1000","price":"1.1"}]}',
      },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Submit batch' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/orders/batch'));
      expect(JSON.parse(post?.init?.body as string) as Record<string, unknown>).toMatchObject({
        orders: [{ symbol: 'EUR/USD' }],
      });
    });
    fireEvent.change(screen.getByLabelText('Batch cancel — order ids'), {
      target: { value: '11, 12' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Cancel batch' }));
    await waitFor(() => {
      const del = calls.find((c) => c.method === 'DELETE' && c.url.includes('/orders/batch'));
      expect(JSON.parse(del?.init?.body as string) as Record<string, unknown>).toMatchObject({
        order_ids: [11, 12],
      });
    });
  });

  it('requires double-click confirm before DELETE /orders/all', async () => {
    const calls = installFetchMock({
      'DELETE /api/v1/orders/all': { status: 200, body: { cancelled: 9 } },
    });
    renderApp(<BatchOpsPanel />);
    fireEvent.click(screen.getByRole('button', { name: 'Cancel all open orders' }));
    expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: /Confirm: cancel ALL/ }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/orders/all'))).toBe(true);
    });
  });

  it('scoped mass cancel sends DELETE /orders?symbol=&side=', async () => {
    const calls = installFetchMock({
      'DELETE /api/v1/orders*': { status: 200, body: { cancelled: 3 } },
    });
    renderApp(<BatchOpsPanel />);
    fireEvent.change(screen.getByLabelText(/Symbol scope/), { target: { value: 'gbp/usd' } });
    await userEvent.selectOptions(screen.getByLabelText('Side filter'), 'BUY');
    fireEvent.click(screen.getByRole('button', { name: 'Cancel GBP/USD orders' }));
    await waitFor(() => {
      const del = calls.find((c) => c.method === 'DELETE' && c.url.includes('/orders?'));
      expect(del?.url).toContain('symbol=GBP%2FUSD');
      expect(del?.url).toContain('side=BUY');
    });
  });
});
