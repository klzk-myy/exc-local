/**
 * DepthChart tests (Task 10.3.12) — the lwc module is stubbed (jsdom has
 * no Canvas): the suite asserts OUR wiring — cumulative bid/ask series
 * data, the mid extension point, zoom controls, the crosshair→price
 * readout, and click→order-draft. Rendered pixels are lwc's problem.
 */
import { act, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { installFetchMock, renderApp } from '@/test/accountMocks';
import { makeWsHarness } from '@/lib/trading/testkit';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

interface FakeSeries {
  kind: string;
  data: { time: number; value: number }[];
  setData(d: { time: number; value: number }[]): void;
  applyOptions(o: unknown): void;
}

type Handler = (param: { time?: number }) => void;

const fake = vi.hoisted(() => ({
  charts: [] as {
    series: FakeSeries[];
    crosshair: Handler[];
    clicks: Handler[];
    removed: boolean;
  }[],
}));

vi.mock('lightweight-charts', () => ({
  AreaSeries: 'Area',
  LineSeries: 'Line',
  LineStyle: { Solid: 0, Dashed: 2 },
  ColorType: { Solid: 'solid' },
  CrosshairMode: { Normal: 0 },
  createChart: () => {
    const chart = {
      series: [] as FakeSeries[],
      crosshair: [] as Handler[],
      clicks: [] as Handler[],
      removed: false,
      addSeries(def: unknown) {
        const s: FakeSeries = {
          kind: String(def),
          data: [],
          setData(d) {
            this.data = d;
          },
          applyOptions: () => undefined,
        };
        chart.series.push(s);
        return s;
      },
      subscribeCrosshairMove(h: Handler) {
        chart.crosshair.push(h);
      },
      unsubscribeCrosshairMove: () => undefined,
      subscribeClick(h: Handler) {
        chart.clicks.push(h);
      },
      unsubscribeClick: () => undefined,
      priceScale: () => ({ applyOptions: () => undefined }),
      timeScale: () => ({
        fitContent: () => undefined,
        subscribeVisibleLogicalRangeChange: () => undefined,
        unsubscribeVisibleLogicalRangeChange: () => undefined,
        timeToCoordinate: () => 300,
      }),
      applyOptions: () => undefined,
      remove() {
        this.removed = true;
      },
    };
    fake.charts.push(chart);
    return chart;
  },
}));

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
  it('pushes cumulative bid/ask areas meeting at mid', async () => {
    const h = makeWsHarness();
    setup();
    fake.charts.length = 0;
    renderApp(<DepthChart symbol="EUR/USD" client={h.client} />);
    await screen.findByRole('img', { name: /mid 1\.10000/ });
    expect(screen.getByText('▲ BIDS')).toBeInTheDocument();
    expect(screen.getByText('ASKS ▼')).toBeInTheDocument();
    expect(screen.getByText('zoom 100%')).toBeInTheDocument();

    const chart = fake.charts.at(-1);
    expect(chart).toBeDefined();
    expect(chart!.series.length).toBe(2);
    const [bid, ask] = chart!.series as [FakeSeries, FakeSeries];
    expect(bid.kind).toBe('Area');
    expect(ask.kind).toBe('Area');
    // Bid curve descends to mid: far-left (worst bid) carries the full
    // cumulative 350k, the mid extension carries the best level's 50k.
    expect(bid.data[0]!.value).toBe(350000);
    expect(bid.data.at(-1)!.value).toBe(50000);
    // Ask curve starts at mid with its cumulative at best.
    expect(ask.data[0]!.value).toBe(40000);
    expect(ask.data.at(-1)!.value).toBe(240000);
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

  it('crosshair reports price + cumulative depth on both sides', async () => {
    const h = makeWsHarness();
    setup();
    fake.charts.length = 0;
    renderApp(<DepthChart symbol="EUR/USD" client={h.client} />);
    await screen.findByRole('img', { name: /mid 1\.10000/ });
    const chart = fake.charts.at(-1)!;
    // Hover at price 1.0998 (encoded: 1.0998 × 1e8).
    act(() => chart.crosshair.forEach((fn) => fn({ time: 109980000 })));
    expect(await screen.findByText(/price 1\.0998/)).toBeInTheDocument();
    expect(screen.getByText(/bid depth/)).toBeInTheDocument();
    expect(screen.getByText(/ask depth/)).toBeInTheDocument();
  });

  it('click writes the order ticket draft', async () => {
    const h = makeWsHarness();
    setup();
    fake.charts.length = 0;
    const { useOrderDraft } = await import('@/lib/trading/orderDraft');
    renderApp(<DepthChart symbol="EUR/USD" client={h.client} />);
    await screen.findByRole('img', { name: /mid 1\.10000/ });
    const chart = fake.charts.at(-1)!;
    act(() => chart.clicks.forEach((fn) => fn({ time: 110010000 })));
    expect(useOrderDraft.getState().draft).toMatchObject({
      symbol: 'EUR/USD',
      price: '1.10010',
    });
  });
});
