import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import { parseAlgoOrder, parseOrderListSummary, fetchOrdersPage } from './api';
import { DeadmanSwitch } from './DeadmanSwitch';
import { OrderListsPanel } from './OrderListsPanel';
import { AlgoPanel } from './AlgoPanel';

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
});
