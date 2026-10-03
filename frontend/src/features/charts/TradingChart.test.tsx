/**
 * TradingChart tests — the chart library is module-mocked so the suite
 * exercises OUR wiring (REST history load → setData, kline@ WS frames →
 * series.update) without Canvas/WebGL. ChartPage smoke-tests the lazy
 * boundary + timeframe selector.
 */
import { useState } from 'react';
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiClient } from '@/lib/api';
import { installFetchMock, renderApp } from '@/test/accountMocks';
import { WsClient } from '@/lib/ws';
import { FakeClock, SocketFactory } from '@/lib/ws/testkit';

// -- lightweight-charts module stub ----------------------------------------

interface FakeSeries {
  kind: string;
  data: unknown[];
  updates: unknown[];
  setData(d: unknown[]): void;
  update(d: unknown): void;
  applyOptions(o: unknown): void;
  priceToCoordinate(p: number): number | null;
  coordinateToPrice(y: number): number | null;
}

const fake = {
  charts: [] as { removed: boolean; options: unknown; series: FakeSeries[] }[],
};

function makeSeries(kind: string): FakeSeries {
  return {
    kind,
    data: [],
    updates: [],
    setData(d: unknown[]) {
      this.data = d;
    },
    update(d: unknown) {
      this.updates.push(d);
    },
    applyOptions() {
      /* autoscaleInfoProvider etc. — no-op in the stub */
    },
    // Coordinate APIs return null → overlays render empty in jsdom.
    priceToCoordinate: () => null,
    coordinateToPrice: () => null,
  };
}

vi.mock('lightweight-charts', () => ({
  CandlestickSeries: 'Candlestick',
  HistogramSeries: 'Histogram',
  ColorType: { Solid: 'solid' },
  CrosshairMode: { Normal: 0, Magnet: 1 },
  createChart: (_el: HTMLElement, options: unknown) => {
    const chart = {
      options,
      removed: false,
      series: [] as FakeSeries[],
      addSeries(def: unknown) {
        const s = makeSeries(String(def));
        chart.series.push(s);
        return s;
      },
      priceScale: () => ({ applyOptions: () => undefined, width: () => 72 }),
      timeScale: () => ({
        fitContent: () => undefined,
        subscribeVisibleLogicalRangeChange: () => undefined,
        unsubscribeVisibleLogicalRangeChange: () => undefined,
        timeToCoordinate: () => null,
        width: () => 600,
        height: () => 280,
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

// Lazy boundary resolves through the mock.
import ChartPage from './ChartPage';
import TradingChart from './TradingChart';

const KLINES = {
  symbol: 'EUR/USD',
  interval: '1h',
  count: 2,
  data: [
    {
      open_time_ms: 1_699_996_400_000,
      open: '1.0800',
      high: '1.0810',
      low: '1.0798',
      close: '1.0805',
      volume: '400',
      trade_count: 10,
      closed: true,
    },
    {
      open_time_ms: 1_700_000_000_000,
      open: '1.0805',
      high: '1.0815',
      low: '1.0800',
      close: '1.0812',
      volume: '600',
      trade_count: 12,
      closed: true,
    },
  ],
};

function harness() {
  const calls = installFetchMock({
    'GET /api/v1/klines/EUR%2FUSD': { body: KLINES },
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
      channels: ['kline@EUR/USD_1h'],
      total: 1,
      ts_ms: h.clock.now(),
    });
  });
  return sock;
}

beforeEach(() => {
  vi.restoreAllMocks();
  fake.charts.length = 0;
});

describe('TradingChart', () => {
  it('creates the chart, loads REST history into candle+volume series', async () => {
    const h = harness();
    renderApp(<TradingChart symbol="EUR/USD" interval="1h" api={h.api} ws={h.ws} />);
    await waitFor(() => expect(fake.charts).toHaveLength(1));
    await waitFor(() => expect(fake.charts[0]?.series[0]?.data).toHaveLength(2));
    const candles = fake.charts[0]!.series[0]!;
    const volume = fake.charts[0]!.series[1]!;
    expect(candles.kind).toBe('Candlestick');
    expect(candles.data[0]).toMatchObject({ time: 1_699_996_400, open: 1.08 });
    expect(volume.kind).toBe('Histogram');
    expect(volume.data).toHaveLength(2);
    expect(h.calls.some((c) => c.url.includes('/klines/EUR%2FUSD'))).toBe(true);
  });

  it('subscribes kline@symbol_interval and merges live frames into the open bar', async () => {
    const h = harness();
    renderApp(<TradingChart symbol="EUR/USD" interval="1h" api={h.api} ws={h.ws} />);
    const sock = await connect(h);
    await waitFor(() => expect(fake.charts[0]?.series[0]?.data).toHaveLength(2));
    act(() => {
      sock.recv({
        type: 'event',
        channel: 'kline@EUR/USD_1h',
        seq: 1,
        ts_ms: h.clock.now(),
        data: {
          symbol: 'EUR/USD',
          timeframe: '1h',
          open_time_ms: 1_700_000_000_000, // same bucket → update open bar
          open: '1.0805',
          high: '1.0830',
          low: '1.0800',
          close: '1.0825',
          volume: '640',
          closed: false,
          last_seq: 1,
        },
      });
    });
    const candles = fake.charts[0]!.series[0]!;
    expect(candles.updates.at(-1)).toMatchObject({
      time: 1_700_000_000,
      high: 1.083,
      close: 1.0825,
    });
    const volume = fake.charts[0]!.series[1]!;
    expect(volume.updates.at(-1)).toMatchObject({ time: 1_700_000_000, value: 640 });
  });

  it('re-keys the chart when the interval changes (fresh channel+history)', async () => {
    const h = harness();
    // Providers must wrap the rerender — a state-driven harness keeps
    // renderApp's QueryClientProvider mounted across the interval change.
    function Harness() {
      const [tf, setTf] = useState<'1h' | '4h'>('1h');
      return (
        <>
          <button onClick={() => setTf('4h')}>switch-tf</button>
          <TradingChart symbol="EUR/USD" interval={tf} api={h.api} ws={h.ws} />
        </>
      );
    }
    renderApp(<Harness />);
    await waitFor(() => expect(fake.charts).toHaveLength(1));
    await userEvent.click(screen.getByRole('button', { name: 'switch-tf' }));
    await waitFor(() => expect(fake.charts).toHaveLength(2));
    expect(fake.charts[0]?.removed).toBe(true);
  });
});

describe('ChartPage', () => {
  it('renders the 13-timeframe selector and mounts the lazy chart', async () => {
    harness();
    renderApp(<ChartPage />, '/chart/EUR%2FUSD');
    await waitFor(() =>
      expect(screen.getByRole('group', { name: 'Timeframe' })).toBeInTheDocument(),
    );
    for (const tf of [
      '1s',
      '1m',
      '5m',
      '15m',
      '30m',
      '1h',
      '2h',
      '4h',
      '6h',
      '8h',
      '1D',
      '1W',
      '1M',
    ]) {
      expect(screen.getByRole('button', { name: tf })).toBeInTheDocument();
    }
    // lazy chart resolves through the mock
    await waitFor(() => expect(screen.getByTestId('trading-chart')).toBeInTheDocument());
    await userEvent.click(screen.getByRole('button', { name: '5m' }));
    await waitFor(() =>
      expect(screen.getByTestId('trading-chart')).toHaveAttribute('data-interval', '5m'),
    );
  });
});
