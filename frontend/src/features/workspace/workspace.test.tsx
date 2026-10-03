/**
 * Workspace tests (Tasks 10.3.9 + 10.3.14) — liteMode store + KYC
 * defaults, layout persistence round-trip, the Pro grid's panel
 * hide/show + safety-critical warning, Lite dashboard render + order
 * form validation.
 */
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { installFetchMock, renderApp } from '@/test/accountMocks';
import { connectWs, makeWsHarness } from '@/lib/trading/testkit';
import { useOrderDraft } from '@/lib/trading/orderDraft';

import {
  defaultPlacements,
  listLayouts,
  loadLayout,
  resetLayouts,
  saveLayout,
  workspaceStorageKey,
  type Placements,
} from './layouts';
import { defaultModeForKyc, useUiModeStore } from './liteMode';
import { LiteOrderForm } from './LiteDashboard';
import WorkspacePage from './WorkspacePage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

// The unified TradingChart pulls in lightweight-charts (Canvas/matchMedia)
// — outside jsdom's reach and outside this suite's scope (panel plumbing,
// not chart internals).
vi.mock('@/features/charts/TradingChart', () => ({
  default: ({ symbol }: { symbol: string }) => (
    <div data-testid="trading-chart" data-symbol={symbol} />
  ),
}));
// DepthChart migrated onto lightweight-charts too — same jsdom blind
// spot, same out-of-scope stub.
vi.mock('@/features/depth-chart/DepthChart', () => ({
  DepthChart: ({ symbol }: { symbol: string }) => (
    <div data-testid="depth-chart" data-symbol={symbol} />
  ),
}));

const EMPTY_FETCH = {
  'GET /api/v1/instruments': { body: { data: [] } },
  'GET /api/v1/account/balances': {
    body: { balances: [{ currency: 'USD', available: '1000', locked: '0', total: '1000' }] },
  },
  'GET /api/v1/positions': { body: { positions: [] } },
  'GET /api/v1/orders*': { body: { data: [] } },
  'GET /api/v1/klines*': { body: { data: [] } },
  'GET /api/v1/book*': { body: { symbol: 'EUR/USD', seq: 0, bids: [], asks: [] } },
};

beforeEach(() => {
  useUiModeStore.setState({ mode: 'pro' });
  useOrderDraft.setState({ draft: { symbol: 'EUR/USD', side: 'BUY', price: '', quantity: '' } });
  localStorage.clear();
});

describe('liteMode store', () => {
  it('auto mode resolves from KYC tier (T0/T1→lite, T2/institutional→pro)', () => {
    expect(defaultModeForKyc('T0')).toBe('lite');
    expect(defaultModeForKyc('T1')).toBe('lite');
    expect(defaultModeForKyc('T2')).toBe('pro');
    expect(defaultModeForKyc('INSTITUTIONAL')).toBe('pro');
    expect(defaultModeForKyc(null)).toBe('lite');
  });

  it('persists an explicit choice and clears back to auto', () => {
    useUiModeStore.getState().setMode('pro');
    expect(useUiModeStore.getState().mode).toBe('pro');
    expect(localStorage.getItem('exc.ui-mode.v1')).toContain('pro');
    useUiModeStore.getState().clearMode();
    expect(useUiModeStore.getState().mode).toBeNull();
  });
});

describe('workspace layout persistence', () => {
  it('save → list → load → reset round-trips under the scope key', () => {
    const placements: Placements = defaultPlacements('pro');
    placements.order = { ...placements.order, x: 2, w: 5 };
    saveLayout('master', 'desk-a', placements);

    expect(listLayouts('master')).toEqual(['desk-a']);
    const loaded = loadLayout('master', 'pro', 'desk-a');
    expect(loaded.placements.order.x).toBe(2);
    expect(loaded.placements.order.w).toBe(5);

    // other scope is isolated
    expect(listLayouts('sub-42')).toEqual([]);

    resetLayouts('master');
    expect(listLayouts('master')).toEqual([]);
    expect(localStorage.getItem(workspaceStorageKey('master'))).toBeNull();
  });

  it('load falls back to the mode default for unknown names', () => {
    const l = loadLayout('master', 'lite', 'does-not-exist');
    expect(l.placements).toEqual(defaultPlacements('lite'));
    expect(l.name).toBe('Lite default');
  });

  it('corrupt JSON degrades to defaults rather than throwing', () => {
    localStorage.setItem(workspaceStorageKey('master'), '{not json');
    const l = loadLayout('master', 'pro');
    expect(l.placements.order.visible).toBe(true);
  });

  it('merges panels added after a layout was saved (book backfilled from default)', () => {
    const legacy = { ...defaultPlacements('pro') };
    // Simulate a pre-'book' stored layout: drop the panel entirely.
    Reflect.deleteProperty(legacy, 'book');
    saveLayout('master', 'legacy', legacy);

    const l = loadLayout('master', 'pro', 'legacy');
    expect(l.name).toBe('legacy');
    // Stored values preserved; the new panel comes from the mode default.
    expect(l.placements.book).toEqual(defaultPlacements('pro').book);
    expect(l.placements.order).toEqual(legacy.order);
  });
});

describe('WorkspacePage (pro mode)', () => {
  it('renders the six panels of the Pro default layout', async () => {
    installFetchMock(EMPTY_FETCH);
    renderApp(<WorkspacePage />);
    expect(await screen.findByLabelText('Order ticket panel')).toBeInTheDocument();
    expect(screen.getByLabelText('Order book panel')).toBeInTheDocument();
    expect(screen.getByLabelText('Positions & quick actions panel')).toBeInTheDocument();
    expect(screen.getByLabelText('Chart & overlays panel')).toBeInTheDocument();
    expect(screen.getByLabelText('Market depth panel')).toBeInTheDocument();
    // balances hidden by default in the Pro layout
    expect(screen.queryByLabelText('Balances panel')).not.toBeInTheDocument();
    expect(screen.getByLabelText(/Show a hidden panel/)).toBeInTheDocument();
  });

  it('safety-critical panels warn before hiding; others hide directly', async () => {
    installFetchMock(EMPTY_FETCH);
    renderApp(<WorkspacePage />);

    // non-critical: hide straight away
    const depthPanel = await screen.findByLabelText('Market depth panel');
    await userEvent.click(screen.getByRole('button', { name: 'Hide Market depth panel' }));
    expect(depthPanel).not.toBeInTheDocument();

    // safety-critical: confirmation modal required
    await userEvent.click(screen.getByRole('button', { name: 'Hide Order ticket panel' }));
    const dialog = await screen.findByRole('dialog', { name: 'Hide safety-critical panel?' });
    expect(dialog).toHaveTextContent(/safety-critical surface/);
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.getByLabelText('Order ticket panel')).toBeInTheDocument(); // still there

    await userEvent.click(screen.getByRole('button', { name: 'Hide Order ticket panel' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Hide anyway' }));
    expect(screen.queryByLabelText('Order ticket panel')).not.toBeInTheDocument();
  });

  it('save-as persists a named layout; reset restores defaults', async () => {
    installFetchMock(EMPTY_FETCH);
    renderApp(<WorkspacePage />);
    await screen.findByLabelText('Order ticket panel');

    await userEvent.click(screen.getByRole('button', { name: 'Save as…' }));
    await userEvent.type(screen.getByLabelText('Layout name'), 'scalp desk');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(localStorage.getItem(workspaceStorageKey('master'))).toContain('scalp desk');

    await userEvent.click(screen.getByRole('button', { name: 'Reset workspace layout' }));
    expect(localStorage.getItem(workspaceStorageKey('master'))).toBeNull();
  });
});

describe('WorkspacePage (lite mode)', () => {
  it('renders the Lite dashboard instead of the grid', async () => {
    useUiModeStore.setState({ mode: 'lite' });
    installFetchMock(EMPTY_FETCH);
    renderApp(<WorkspacePage />);
    expect(await screen.findByRole('form', { name: 'Simple order form' })).toBeInTheDocument();
    expect(screen.getByText('Balances')).toBeInTheDocument();
    expect(screen.queryByLabelText('Order ticket panel')).not.toBeInTheDocument();
  });

  it('lite → pro toggle reloads the Pro layout, not stale lite geometry', async () => {
    useUiModeStore.setState({ mode: 'lite' });
    installFetchMock(EMPTY_FETCH);
    renderApp(<WorkspacePage />);
    await screen.findByRole('form', { name: 'Simple order form' });

    await userEvent.click(screen.getByRole('button', { name: /^pro/ }));
    // The Pro default shows the five trading panels — a lite-initialized
    // placements state would instead render order/positions/balances only.
    expect(await screen.findByLabelText('Order ticket panel')).toBeInTheDocument();
    expect(screen.getByLabelText('Order book panel')).toBeInTheDocument();
    expect(screen.getByLabelText('Chart & overlays panel')).toBeInTheDocument();
    expect(screen.getByLabelText('Market depth panel')).toBeInTheDocument();
    expect(screen.getByLabelText('Positions & quick actions panel')).toBeInTheDocument();
    expect(screen.queryByLabelText('Balances panel')).not.toBeInTheDocument();
  });
});

describe('LiteOrderForm', () => {
  it('blocks submission until a positive quantity is entered', async () => {
    installFetchMock(EMPTY_FETCH);
    const h = makeWsHarness();
    renderApp(<LiteOrderForm client={h.client} />);
    await connectWs(h);

    const submit = await screen.findByRole('button', { name: /Buy EUR\/USD/ });
    await userEvent.click(submit);
    expect(await screen.findByRole('alert')).toHaveTextContent(/quantity greater than zero/);
  });
});
