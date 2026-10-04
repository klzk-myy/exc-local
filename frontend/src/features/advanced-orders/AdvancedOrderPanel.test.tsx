/**
 * AdvancedOrderPanel tests — the Binance-style dual-column ticket:
 * independent Buy/Sell columns, per-side submission, disabled-state
 * locking, and the OCO single-column fallback.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { useOrderDraft } from '@/lib/trading/orderDraft';
import type { WsClient, WsClientStatus } from '@/lib/ws';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

// Late-bound fetch singletons so installFetchMock intercepts queries.
vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

import { AdvancedOrderPanel } from './AdvancedOrderPanel';

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
      min_notional: '10',
      max_leverage: 30,
    },
  ],
};

const BALANCES = {
  balances: [
    { currency: 'USD', available: '100000', locked: '0', total: '100000' },
    { currency: 'EUR', available: '50000', locked: '0', total: '50000' },
  ],
};

const ENABLED_STATUS: WsClientStatus = {
  state: 'AUTHENTICATED',
  attempt: 0,
  orderEntryEnabled: true,
  subscriptions: [],
  health: {},
  lastError: null,
};

/** Order-entry-enabled client — the runtime stub defaults to locked. */
const enabledClient: WsClient = {
  start: () => undefined,
  stop: () => undefined,
  subscribe: () => () => undefined,
  unsubscribe: () => undefined,
  onStatusChange: () => () => undefined,
  getStatus: () => ENABLED_STATUS,
} as unknown as WsClient;

function installRoutes(posts?: { body: Record<string, unknown> }) {
  return installFetchMock({
    'GET /api/v1/instruments': { body: INSTRUMENTS },
    'GET /api/v1/account/balances': { body: BALANCES },
    'POST /api/v1/orders': {
      handler: (_url, init) => {
        if (posts !== undefined)
          Object.assign(
            posts.body,
            typeof init?.body === 'string' ? JSON.parse(init.body) : {},
          );
        return { status: 200, body: { order_id: '1' } };
      },
    },
  });
}

const buyColumn = () => screen.getByLabelText('Buy ticket');
const sellColumn = () => screen.getByLabelText('Sell ticket');

async function waitForSymbol() {
  // Default-instrument pick lands once the instruments query resolves.
  await waitFor(() => expect(screen.getByLabelText(/Instrument/i)).toHaveValue('EUR/USD'));
}

beforeEach(() => {
  signInForTests();
  useOrderDraft.getState().setDraft({ symbol: '', side: 'BUY', price: '', quantity: '' });
});

describe('AdvancedOrderPanel — Binance-style dual columns', () => {
  it('renders Buy and Sell columns side by side', async () => {
    installRoutes();
    renderApp(<AdvancedOrderPanel />);
    await waitForSymbol();

    const group = screen.getByRole('group', { name: /order sides/i });
    expect(within(group).getByLabelText('Buy ticket')).toBeInTheDocument();
    expect(within(group).getByLabelText('Sell ticket')).toBeInTheDocument();
    expect(within(buyColumn()).getByRole('button', { name: 'Buy EUR' })).toBeInTheDocument();
    expect(within(sellColumn()).getByRole('button', { name: 'Sell EUR' })).toBeInTheDocument();
  });

  it('shows per-column availability: quote for Buy, base for Sell', async () => {
    installRoutes();
    renderApp(<AdvancedOrderPanel />);
    await waitForSymbol();

    await waitFor(() => {
      expect(within(buyColumn()).getByText(/[\d] USD$/)).toBeInTheDocument();
      expect(within(sellColumn()).getByText(/[\d] EUR$/)).toBeInTheDocument();
    });
  });

  it('keeps column fields independent — typing in Buy never touches Sell', async () => {
    installRoutes();
    renderApp(<AdvancedOrderPanel />);
    await waitForSymbol();

    await userEvent.type(within(buyColumn()).getByLabelText(/^Price/), '1.10000');
    await userEvent.type(within(buyColumn()).getByLabelText(/^Quantity/), '2000');

    expect(within(buyColumn()).getByLabelText(/^Price/)).toHaveValue('1.10000');
    expect(within(sellColumn()).getByLabelText(/^Price/)).toHaveValue('');
    expect(within(sellColumn()).getByLabelText(/^Quantity/)).toHaveValue('');
  });

  it('disables both submit buttons while order entry is locked', async () => {
    installRoutes();
    renderApp(<AdvancedOrderPanel />); // default stub: DISCONNECTED
    await waitForSymbol();

    expect(within(buyColumn()).getByRole('button', { name: 'Buy EUR' })).toBeDisabled();
    expect(within(sellColumn()).getByRole('button', { name: 'Sell EUR' })).toBeDisabled();
  });

  it('submits side BUY with the Buy column values and an idempotency key', async () => {
    const posts = { body: {} as Record<string, unknown> };
    installRoutes(posts);
    renderApp(<AdvancedOrderPanel client={enabledClient} />);
    await waitForSymbol();

    await userEvent.type(within(buyColumn()).getByLabelText(/^Price/), '1.10000');
    await userEvent.type(within(buyColumn()).getByLabelText(/^Quantity/), '2000');
    await userEvent.type(within(sellColumn()).getByLabelText(/^Price/), '9.99999');
    await userEvent.click(within(buyColumn()).getByRole('button', { name: 'Buy EUR' }));

    await waitFor(() => expect(posts.body['side']).toBe('BUY'));
    expect(posts.body['symbol']).toBe('EUR/USD');
    expect(posts.body['type']).toBe('LIMIT');
    expect(posts.body['price']).toBe('1.1');
    expect(posts.body['quantity']).toBe('2000');
    expect(posts.body['client_order_id']).toEqual(expect.any(String));
  });

  it('submits side SELL with the Sell column values', async () => {
    const posts = { body: {} as Record<string, unknown> };
    installRoutes(posts);
    renderApp(<AdvancedOrderPanel client={enabledClient} />);
    await waitForSymbol();

    await userEvent.type(within(sellColumn()).getByLabelText(/^Price/), '1.20000');
    await userEvent.type(within(sellColumn()).getByLabelText(/^Quantity/), '3000');
    await userEvent.click(within(sellColumn()).getByRole('button', { name: 'Sell EUR' }));

    await waitFor(() => expect(posts.body['side']).toBe('SELL'));
    expect(posts.body['price']).toBe('1.2');
    expect(posts.body['quantity']).toBe('3000');
  });

  it('blocks a side with invalid fields while the other stays clean', async () => {
    const calls = installRoutes();
    renderApp(<AdvancedOrderPanel client={enabledClient} />);
    await waitForSymbol();

    // Buy column empty → its submit must not reach the wire and must
    // flag errors inside the Buy column only.
    await userEvent.click(within(buyColumn()).getByRole('button', { name: 'Buy EUR' }));
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0);
    expect(
      within(buyColumn()).getAllByRole('alert').length,
    ).toBeGreaterThan(0);
    expect(within(sellColumn()).queryAllByRole('alert')).toHaveLength(0);
  });

  it('greys the price row to "Market" for MARKET orders', async () => {
    installRoutes();
    renderApp(<AdvancedOrderPanel />);
    await waitForSymbol();

    await userEvent.selectOptions(screen.getByLabelText('Order type'), 'MARKET');
    expect(within(buyColumn()).getByDisplayValue('Market')).toBeDisabled();
    expect(within(sellColumn()).getByDisplayValue('Market')).toBeDisabled();
  });

  it('keeps OCO on a single column with a side toggle', async () => {
    installRoutes();
    renderApp(<AdvancedOrderPanel />);
    await waitForSymbol();

    await userEvent.selectOptions(screen.getByLabelText('Order type'), 'OCO');
    expect(screen.queryByRole('group', { name: /order sides/i })).not.toBeInTheDocument();
    const toggle = screen.getByRole('group', { name: /order side/i });
    expect(within(toggle).getByRole('button', { name: 'BUY' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    expect(screen.getByRole('button', { name: /Submit OCO/i })).toBeInTheDocument();
  });
});
