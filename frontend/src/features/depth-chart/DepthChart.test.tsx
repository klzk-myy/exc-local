/**
 * DepthChart tests (Task 10.3.12) — cumulative bid/ask curves + mid
 * line render from canonical L2, zoom controls work, empty books render
 * an honest empty state, hover crosshair reports cumulative qty/notional.
 */
import { screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { installFetchMock, renderApp } from '@/test/accountMocks';
import { makeWsHarness } from '@/lib/trading/testkit';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

import { DepthChart } from './DepthChart';

const BOOK = {
  symbol: 'EUR/USD',
  seq: 5,
  bids: [
    { price: '1.0999', quantity: '50000' },
    { price: '1.0998', quantity: '100000' },
    { price: '1.0997', quantity: '200000' },
  ],
  asks: [
    { price: '1.1001', quantity: '40000' },
    { price: '1.1002', quantity: '80000' },
    { price: '1.1003', quantity: '120000' },
  ],
};

function setup(book: unknown = BOOK) {
  installFetchMock({
    'GET /api/v1/book*': { body: book },
    'GET /api/v1/instruments': { body: { data: [] } },
  });
}

describe('DepthChart', () => {
  it('renders the two cumulative areas, mid label, and axis endpoints', async () => {
    const h = makeWsHarness();
    setup();
    const { container } = renderApp(<DepthChart symbol="EUR/USD" client={h.client} />);
    const svg = await screen.findByRole('img', { name: /mid 1\.10000/ });
    expect(svg).toBeInTheDocument();
    expect(screen.getByText('▲ BIDS')).toBeInTheDocument();
    expect(screen.getByText('ASKS ▼')).toBeInTheDocument();
    const paths = container.querySelectorAll('path');
    expect(paths.length).toBe(2); // bid area + hatched ask area
    expect(screen.getByText('zoom 100%')).toBeInTheDocument();
  });

  it('zoom in/out buttons rescale around mid', async () => {
    const h = makeWsHarness();
    setup();
    renderApp(<DepthChart symbol="EUR/USD" client={h.client} />);
    await screen.findByRole('img', { name: /mid 1\.10000/ });
    await userEvent.click(screen.getByRole('button', { name: 'Zoom in' }));
    expect(screen.getByText('zoom 71%')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Reset' }));
    expect(screen.getByText('zoom 100%')).toBeInTheDocument();
  });

  it('honest empty state when the book has no depth', async () => {
    const h = makeWsHarness();
    setup({ symbol: 'EUR/USD', seq: 1, bids: [], asks: [] });
    renderApp(<DepthChart symbol="EUR/USD" client={h.client} />);
    expect(await screen.findByText(/No depth data for EUR\/USD/)).toBeInTheDocument();
  });

  it('pointer hover shows price + cumulative depth on both sides', async () => {
    const h = makeWsHarness();
    setup();
    renderApp(<DepthChart symbol="EUR/USD" client={h.client} />);
    const svg = await screen.findByRole('img', { name: /mid 1\.10000/ });
    // jsdom getBoundingClientRect is 0-sized → clientX maps to x=0 → price = lo.
    // jsdom lacks PointerEvent, so fireEvent.pointerMove can't reach React's
    // onPointerMove — dispatch a MouseEvent with the pointermove type instead.
    fireEvent(svg, new MouseEvent('pointermove', { bubbles: true, clientX: 0, clientY: 0 }));
    expect(await screen.findByText(/price 1\.0/)).toBeInTheDocument();
    expect(screen.getByText(/bid depth/)).toBeInTheDocument();
  });
});
