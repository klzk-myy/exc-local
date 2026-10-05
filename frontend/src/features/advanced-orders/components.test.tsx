/**
 * Component tests — AdlIndicator (10.3.13), PercentSlider (10.3.11),
 * ConfirmExecutionModal (10.3.10), OrderInspectModal (10.3.15),
 * SubAccountSwitcher (10.3.7).
 */
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiClient } from '@/lib/api';
import { dec } from '@/lib/decimal/decimal';
import { useAccountScope, MASTER_ACCOUNT_KEY } from '@/lib/trading/accountScope';
import type { Instrument, Order } from '@/lib/trading/types';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

// Late-bound fetch singletons so installFetchMock intercepts queries.
vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

import { AdlIndicator } from './AdlIndicator';
import { ConfirmExecutionModal } from './ConfirmExecutionModal';
import { OrderInspectModal } from './OrderInspectModal';
import { PercentSlider } from './PercentSlider';
import { SubAccountSwitcher } from './SubAccountSwitcher';

const INST: Instrument = {
  symbol: 'EUR/USD',
  base: 'EUR',
  quote: 'USD',
  instrumentType: 'SPOT',
  status: 'ACTIVE',
  tickSize: dec('0.00001'),
  lotSize: dec('1000'),
  minQty: dec('1000'),
  maxQty: dec('10000000'),
  minNotional: dec('10'),
  maxLeverage: 30,
};

// ---------------------------------------------------------------------------

describe('AdlIndicator', () => {
  it.each([1, 3, 5])('renders rank %i/5 with an accessible label', (rank) => {
    render(<AdlIndicator rank={rank} />);
    expect(screen.getByText(`ADL ${rank}/5`)).toBeInTheDocument();
    expect(screen.getByRole('img')).toHaveAccessibleName(new RegExp(`rank ${rank} of 5`));
  });

  it('renders "unavailable" when the feed has no rank — never fabricated', () => {
    render(<AdlIndicator rank={undefined} symbol="EUR/USD" />);
    expect(screen.getByText('ADL n/a')).toBeInTheDocument();
    expect(screen.getByRole('img')).toHaveAccessibleName(/unavailable.*EUR\/USD/i);
  });

  it('rejects out-of-range ranks as unavailable', () => {
    render(<AdlIndicator rank={9} />);
    expect(screen.getByText('ADL n/a')).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------

describe('PercentSlider', () => {
  it('emits the 25/50/75/100 presets and sizes via onSize', async () => {
    const onSize = vi.fn();
    render(
      <PercentSlider
        instrument={INST}
        price={dec('1.10')}
        freeMargin={dec('1000')}
        leverage={dec('30')}
        onSize={onSize}
      />,
    );
    for (const p of [10, 25, 50, 75, 100]) {
      expect(screen.getByRole('button', { name: `${p}%` })).toBeInTheDocument();
    }
    await userEvent.click(screen.getByRole('button', { name: '100%' }));
    // 1000×30/1.10 = 27272.7 → lot-quantized 27000
    expect(onSize).toHaveBeenCalledWith('27000');
    expect(screen.getByText(/27,000 units/)).toBeInTheDocument();
  });

  it('shows a clamp reason when the size lands below min notional', async () => {
    const onSize = vi.fn();
    render(
      <PercentSlider
        instrument={{
          ...INST,
          lotSize: dec('0.01'),
          minQty: dec('0.01'),
          minNotional: dec('50000'),
        }}
        price={dec('1.10')}
        freeMargin={dec('100')}
        leverage={dec('5')}
        onSize={onSize}
      />,
    );
    await userEvent.click(screen.getByRole('button', { name: '10%' }));
    expect(screen.getByText(/clamped — below minimum notional/)).toBeInTheDocument();
    expect(onSize).toHaveBeenCalledWith('');
  });

  it('is keyboard-reachable and labelled', () => {
    render(
      <PercentSlider
        instrument={INST}
        price={dec('1.1')}
        freeMargin={dec('100')}
        leverage={dec('30')}
        onSize={() => undefined}
      />,
    );
    const slider = screen.getByRole('slider', { name: /percentage of free margin/i });
    expect(slider).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------

describe('ConfirmExecutionModal', () => {
  const projection = {
    avgPrice: dec('1.10016'),
    filledQty: dec('100000'),
    fullyFillable: true,
    slippageBps: dec('1.45'),
  };

  it('shows est. fill / slippage / resulting position and calls onConfirm', async () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmExecutionModal
        open
        title="Flatten position"
        action="Close LONG 100,000 EUR/USD"
        projection={projection}
        resultingPosition="flat"
        onConfirm={onConfirm}
        onClose={() => undefined}
      />,
    );
    expect(screen.getByText('1.100160')).toBeInTheDocument();
    expect(screen.getByText('1.45 bps')).toBeInTheDocument();
    expect(screen.getByText('flat')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Confirm' }));
    expect(onConfirm).toHaveBeenCalled();
  });

  it('states honestly when no projection is available', () => {
    render(
      <ConfirmExecutionModal
        open
        title="t"
        action="a"
        projection={undefined}
        onConfirm={() => undefined}
        onClose={() => undefined}
      />,
    );
    expect(screen.getByText(/Projection unavailable/)).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------

describe('OrderInspectModal', () => {
  const ORDER: Order = {
    id: '777',
    clientOrderId: 'cid-1',
    symbol: 'EUR/USD',
    side: 'BUY',
    type: 'LIMIT',
    timeInForce: 'GTC',
    quantity: dec('100000'),
    filledQty: dec('0'),
    price: dec('1.1000'),
    stopPrice: undefined,
    status: 'ACTIVE',
    orderSeq: 42,
    postOnly: false,
    reduceOnly: false,
    createdAt: '2026-01-01T00:00:00Z',
    avgFillPrice: undefined,
  };

  function apiSpy() {
    const calls: { method: string; url: string; body?: unknown }[] = [];
    const api = new ApiClient({
      baseUrl: '/api/v1',
      getAuthToken: () => 'tok',
      fetchImpl: (input, init) => {
        calls.push({
          method: (init?.method ?? 'GET').toUpperCase(),
          url: typeof input === 'string' ? input : input instanceof URL ? input.href : input.url,
          body: init?.body !== undefined ? JSON.parse(init.body as string) : undefined,
        });
        return Promise.resolve(
          new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }),
        );
      },
    });
    return { api, calls };
  }

  beforeEach(() => {
    useAccountScope.setState({ scopeKey: MASTER_ACCOUNT_KEY, scopeLabel: 'Master account' });
  });

  it('quantity-DOWN uses keep-priority amend (queue position kept)', async () => {
    const { api, calls } = apiSpy();
    renderApp(<OrderInspectModal order={ORDER} open onClose={() => undefined} api={api} />);
    const qty = screen.getByLabelText('New quantity');
    await userEvent.clear(qty);
    await userEvent.type(qty, '50000');
    expect(screen.getByText(/keeps your queue position/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Amend \(keep priority\)/ }));
    await waitFor(() =>
      expect(calls).toContainEqual(
        expect.objectContaining({
          method: 'PUT',
          url: '/api/v1/orders/777/amend/keep-priority',
          body: { order_seq: 42, quantity: '50000' },
        }),
      ),
    );
  });

  it('price change warns about losing queue priority and cancel-replaces', async () => {
    const { api, calls } = apiSpy();
    renderApp(<OrderInspectModal order={ORDER} open onClose={() => undefined} api={api} />);
    const price = screen.getByLabelText('New price');
    await userEvent.clear(price);
    await userEvent.type(price, '1.0900');
    expect(screen.getByText(/queue position is lost/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Amend \(lose priority\)/ }));
    await waitFor(() =>
      expect(calls).toContainEqual(
        expect.objectContaining({
          method: 'POST',
          url: '/api/v1/orders/777/cancel-replace',
          body: expect.objectContaining({ order_seq: 42, price: '1.09' }) as Record<
            string,
            unknown
          >,
        }),
      ),
    );
  });

  it('cancel issues DELETE /orders/{id}', async () => {
    const { api, calls } = apiSpy();
    renderApp(<OrderInspectModal order={ORDER} open onClose={() => undefined} api={api} />);
    await userEvent.click(screen.getByRole('button', { name: 'Cancel order' }));
    await waitFor(() =>
      expect(calls).toContainEqual(
        expect.objectContaining({ method: 'DELETE', url: '/api/v1/orders/777' }),
      ),
    );
  });

  it('renders the amendment audit trail (GET /orders/{id}/amendments, PascalCase rows)', async () => {
    const calls: string[] = [];
    const api = new ApiClient({
      baseUrl: '/api/v1',
      getAuthToken: () => 'tok',
      fetchImpl: (input) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        calls.push(url);
        const body = url.includes('/amendments')
          ? {
              order_id: 777,
              amendments: [
                {
                  OrderID: 777,
                  Operation: 'AMEND',
                  FieldName: 'Quantity',
                  OldValue: '100000',
                  NewValue: '50000',
                  ModifiedBy: 'trader@example.com',
                  RequestID: 'req-9',
                  ModifiedAt: '2026-01-02T00:00:00Z',
                },
              ],
            }
          : {};
        return Promise.resolve(
          new Response(JSON.stringify(body), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        );
      },
    });
    renderApp(<OrderInspectModal order={ORDER} open onClose={() => undefined} api={api} />);
    await userEvent.click(screen.getByRole('button', { name: 'Amendment history' }));
    await waitFor(() => {
      expect(screen.getByText('AMEND')).toBeInTheDocument();
    });
    expect(screen.getByText(/Quantity: 100000 → 50000/)).toBeInTheDocument();
    expect(calls.some((u) => u.includes('/orders/777/amendments'))).toBe(true);
  });
});

// ---------------------------------------------------------------------------

describe('SubAccountSwitcher', () => {
  beforeEach(() => {
    signInForTests();
    useAccountScope.setState({ scopeKey: MASTER_ACCOUNT_KEY, scopeLabel: 'Master account' });
  });

  const SUBS = {
    data: [
      {
        id: 42,
        master_account_id: 1001,
        account_type: 'SUB',
        kyc_tier: 'T2',
        status: 'ACTIVE',
        trading_enabled: true,
        balances: [{ currency: 'USD', available: '500', locked: '0', total: '500' }],
      },
      {
        id: 43,
        master_account_id: 1001,
        account_type: 'SUB',
        kyc_tier: 'T1',
        status: 'SUSPENDED',
        trading_enabled: false,
        balances: [],
      },
    ],
  };

  it('lists sub-accounts with balances and re-scopes the store on select', async () => {
    installFetchMock({ 'GET /api/v1/account/sub-accounts': { body: SUBS } });
    renderApp(<SubAccountSwitcher />);
    await userEvent.click(screen.getByRole('button', { name: /switch trading account/i }));
    const opt = await screen.findByRole('option', { name: /Sub-account #42/ });
    expect(screen.getByText(/500 USD/)).toBeInTheDocument();
    // suspended account is not selectable
    expect(screen.getByRole('option', { name: /Sub-account #43/ })).toBeDisabled();
    await userEvent.click(opt);
    expect(useAccountScope.getState().scopeKey).toBe('42');
    expect(useAccountScope.getState().scopeLabel).toBe('Sub-account #42');
  });

  it('surfaces endpoint failure instead of a fake list', async () => {
    installFetchMock({
      'GET /api/v1/account/sub-accounts': {
        status: 501,
        body: { type: 'error', error: 'NOT_IMPLEMENTED', message: 'not implemented' },
      },
    });
    renderApp(<SubAccountSwitcher />);
    await userEvent.click(screen.getByRole('button', { name: /switch trading account/i }));
    expect(await screen.findByText(/unavailable/i)).toBeInTheDocument();
    expect(useAccountScope.getState().scopeKey).toBe(MASTER_ACCOUNT_KEY);
  });
});
