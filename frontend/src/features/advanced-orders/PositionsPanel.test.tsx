/**
 * PositionsPanel tests (Tasks 10.3.10 + 10.3.13) — quick actions behind
 * the projected-execution confirm modal; ADL ranks render per-position,
 * absent ADL renders "n/a" — never fabricated.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { installFetchMock, renderApp } from '@/test/accountMocks';
import { connectWs, makeWsHarness, pushEvent } from '@/lib/trading/testkit';

// Late-bound fetch singletons so installFetchMock intercepts queries.
vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

import { PositionsPanel } from './PositionsPanel';

const POSITIONS = {
  positions: [
    {
      position_id: 'p1',
      symbol: 'EUR/USD',
      side: 'LONG',
      quantity: '100000',
      entry_price: '1.1000',
      mark_price: '1.1050',
      unrealized_pnl: '500',
      liquidation_price: '1.0500',
      adl_indicator: 4,
    },
    {
      position_id: 'p2',
      symbol: 'USD/JPY',
      side: 'SHORT',
      quantity: '50000',
      entry_price: '150.00',
      mark_price: '149.80',
      unrealized_pnl: '66.67',
      // no adl_indicator → must render "ADL n/a"
    },
  ],
};

const BOOK_DEPTH = {
  symbol: 'EUR/USD',
  seq: 1,
  bids: [
    ['1.1049', '200000', 2],
    ['1.1048', '300000', 1],
  ],
  asks: [
    ['1.1051', '150000', 1],
    ['1.1052', '400000', 2],
  ],
  updated_at_ms: 1,
};

function setupFetch() {
  return installFetchMock({
    'GET /api/v1/positions': { body: POSITIONS },
    'POST /api/v1/orders': { body: { order_id: 9001, status: 'ACTIVE' } },
    'POST /api/v1/positions/close-all': { body: {} },
  });
}

describe('PositionsPanel', () => {
  it('renders positions with ADL rank and n/a fallbacks', async () => {
    const h = makeWsHarness();
    setupFetch();
    renderApp(<PositionsPanel client={h.client} />);
    expect(await screen.findByText('ADL 4/5')).toBeInTheDocument();
    expect(screen.getByText('ADL n/a')).toBeInTheDocument();
    expect(screen.getByText('Positions — Master account')).toBeInTheDocument();
  });

  it('flatten shows the projected-execution modal then posts reduce-only close', async () => {
    const h = makeWsHarness();
    const calls = setupFetch();
    renderApp(<PositionsPanel client={h.client} />);
    const sock = await connectWs(h);

    expect(await screen.findByRole('button', { name: 'Flatten LONG EUR/USD' })).toBeEnabled();
    await userEvent.click(screen.getByRole('button', { name: 'Flatten LONG EUR/USD' }));

    // depth event arrives → projection fills in
    pushEvent(sock, 'depth@EUR/USD', 1, BOOK_DEPTH);
    expect(await screen.findByRole('dialog', { name: 'Flatten position' })).toBeInTheDocument();
    expect(await screen.findByText(/Est\. fill price/)).toBeInTheDocument();
    expect(screen.getByText('flat')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Flatten' }));
    await waitFor(() =>
      expect(calls).toContainEqual(
        expect.objectContaining({ method: 'POST', url: '/api/v1/orders' }),
      ),
    );
    const post = calls.find((c) => c.url === '/api/v1/orders');
    const body = JSON.parse(post?.init?.body as string) as Record<string, unknown>;
    expect(body).toMatchObject({
      symbol: 'EUR/USD',
      side: 'SELL',
      type: 'MARKET',
      reduce_only: true,
      quantity: '100000',
    });
  });

  it('reverse closes then re-opens — two idempotent POSTs', async () => {
    const h = makeWsHarness();
    const calls = setupFetch();
    renderApp(<PositionsPanel client={h.client} />);
    await connectWs(h);

    expect(await screen.findByRole('button', { name: 'Reverse LONG EUR/USD' })).toBeEnabled();
    await userEvent.click(screen.getByRole('button', { name: 'Reverse LONG EUR/USD' }));
    await screen.findByRole('dialog', { name: 'Reverse position' });
    await userEvent.click(screen.getByRole('button', { name: 'Reverse' }));

    await waitFor(() => {
      const posts = calls.filter((c) => c.method === 'POST' && c.url === '/api/v1/orders');
      expect(posts).toHaveLength(2);
    });
    const bodies = calls
      .filter((c) => c.url === '/api/v1/orders')
      .map((c) => JSON.parse(c.init?.body as string) as Record<string, unknown>);
    expect(bodies[0]).toMatchObject({ reduce_only: true, side: 'SELL' });
    expect(bodies[1]).toMatchObject({ side: 'SELL', type: 'MARKET' });
    expect(bodies[1]?.['reduce_only']).toBeUndefined();
  });

  it('close-all hits POST /positions/close-all after confirmation', async () => {
    const h = makeWsHarness();
    const calls = setupFetch();
    renderApp(<PositionsPanel client={h.client} />);
    await connectWs(h);

    await screen.findByText('EUR/USD');
    await userEvent.click(screen.getByRole('button', { name: 'Close all positions' }));
    await screen.findByRole('dialog', { name: 'Close all positions' });
    await userEvent.click(screen.getByRole('button', { name: 'Close all' }));
    await waitFor(() =>
      expect(calls).toContainEqual(
        expect.objectContaining({ method: 'POST', url: '/api/v1/positions/close-all' }),
      ),
    );
  });

  it('private:positions overlay updates the ADL badge live', async () => {
    const h = makeWsHarness();
    setupFetch();
    renderApp(<PositionsPanel client={h.client} />);
    const sock = await connectWs(h);
    expect(await screen.findByText('ADL n/a')).toBeInTheDocument();

    pushEvent(sock, 'private:positions', 1, {
      positions: [
        {
          position_id: 'p2',
          symbol: 'USD/JPY',
          side: 'SHORT',
          quantity: '50000',
          adl_indicator: 2,
        },
      ],
    });
    expect(await screen.findByText('ADL 2/5')).toBeInTheDocument();
  });
});
