/**
 * TradingChart — the Lightweight Charts boundary (Task 10.3.4) and the
 * single chart implementation (2026-10 unification — supersedes the
 * bespoke SVG renderer formerly in features/advanced-orders).
 *
 * THIS MODULE IS THE LAZY CHUNK: `lightweight-charts` (~45 kB gzip) is
 * imported here and nowhere else, and consumers mount this component
 * through React.lazy + Suspense — so the chart library never enters the
 * initial bundle (≤300 kB gate, Task 10.3.1).
 *
 *   - Candlestick series + volume histogram (overlay price scale)
 *   - History via the UDF adapter (GET /klines/{symbol}?interval=…)
 *   - Live bars via `kline@{symbol}_{interval}` — `series.update` merges
 *     each frame into the currently-open bar, then rolls on open_time
 *   - Strictly typed around the v5 API (`addSeries(CandlestickSeries, …)`)
 *   - ChartOverlays (Task 10.3.15): draggable order lines, fill markers,
 *     position/liquidation lines, candle-close countdown — coordinates
 *     derive from the chart's own scales; a custom autoscaleInfoProvider
 *     widens the price range so overlay levels stay visible.
 */
import { useEffect, useMemo, useRef, useState } from 'react';
import {
  CandlestickSeries,
  ColorType,
  createChart,
  CrosshairMode,
  HistogramSeries,
  type AutoscaleInfo,
  type CandlestickData,
  type HistogramData,
  type IChartApi,
  type ISeriesApi,
  type UTCTimestamp,
} from 'lightweight-charts';

import { apiClient, wsClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import { klineChannel, type KlineInterval } from '@/lib/market/channels';
import { parseKline } from '@/lib/market/wire';
import { Dec } from '@/lib/decimal/decimal';
import { useInstrument, useOrders, usePositions } from '@/lib/trading/queries';
import { isOpenOrder, type Order } from '@/lib/trading/types';
import { useChannel, type WsClient } from '@/lib/ws';

import { ChartOverlays, type OverlayBar } from './ChartOverlays';
import { getBars, liveKlineToBar } from './udf';

export interface TradingChartProps {
  symbol: string;
  interval: KlineInterval;
  api?: ApiClient;
  ws?: WsClient;
  /** Fixed height — the chart owns its box (parent controls width). */
  height?: number;
}

export default function TradingChart(props: TradingChartProps) {
  const { symbol, interval } = props;
  const api = props.api ?? apiClient;
  const ws = props.ws ?? wsClient;
  const height = props.height ?? 320;
  const containerRef = useRef<HTMLDivElement | null>(null);
  // Latest series handles for the WS handler (avoids stale closures over
  // the async history load).
  const candleRef = useRef<ISeriesApi<'Candlestick'> | null>(null);
  const volumeRef = useRef<ISeriesApi<'Histogram'> | null>(null);

  // Overlay plumbing — chart/series handles surface to React once the
  // effect creates them; viewTick re-renders on pan/zoom/resize; bars is
  // the fill→candle index and the countdown's last open time.
  const [apis, setApis] = useState<{
    chart: IChartApi;
    candles: ISeriesApi<'Candlestick'>;
  } | null>(null);
  const [, setViewTick] = useState(0);
  const [bars, setBars] = useState<OverlayBar[]>([]);

  // Chart lifecycle — recreated per symbol/interval (interval switch
  // re-keys the canvas AND the kline channel).
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const bump = () => setViewTick((t) => t + 1);
    const chart: IChartApi = createChart(el, {
      height,
      layout: {
        background: { type: ColorType.Solid, color: '#0a0a0a' },
        textColor: '#a3a3a3',
        fontSize: 11,
      },
      grid: {
        vertLines: { color: '#1f1f1f' },
        horzLines: { color: '#1f1f1f' },
      },
      crosshair: { mode: CrosshairMode.Normal },
      rightPriceScale: { borderColor: '#262626' },
      timeScale: { borderColor: '#262626', timeVisible: true, secondsVisible: interval === '1s' },
    });
    const candles = chart.addSeries(CandlestickSeries, {
      upColor: '#10b981',
      downColor: '#ef4444',
      borderVisible: false,
      wickUpColor: '#10b981',
      wickDownColor: '#ef4444',
    });
    const volume = chart.addSeries(HistogramSeries, {
      priceFormat: { type: 'volume' },
      priceScaleId: '',
    });
    chart.priceScale('').applyOptions({ scaleMargins: { top: 0.82, bottom: 0 } });
    candleRef.current = candles;
    volumeRef.current = volume;
    setApis({ chart, candles });
    setBars([]);

    // Re-derive overlay coordinates whenever the visible range moves.
    chart.timeScale().subscribeVisibleLogicalRangeChange(bump);

    let live = true;
    getBars(api, symbol, interval, { limit: 500 })
      .then((history) => {
        if (!live) return;
        candles.setData(
          history.map((b): CandlestickData<UTCTimestamp> => ({
            time: b.time as UTCTimestamp,
            open: b.open,
            high: b.high,
            low: b.low,
            close: b.close,
          })),
        );
        volume.setData(
          history.map((b): HistogramData<UTCTimestamp> => ({
            time: b.time as UTCTimestamp,
            value: b.volume,
            color: b.close >= b.open ? '#10b98155' : '#ef444455',
          })),
        );
        setBars(history.map((b) => ({ openTimeMs: b.time * 1000, time: b.time as UTCTimestamp })));
        chart.timeScale().fitContent();
      })
      .catch(() => {
        /* history unavailable — the live channel still feeds the chart */
      });

    const ro =
      typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(() => {
            chart.applyOptions({ width: el.clientWidth });
            bump();
          })
        : null;
    ro?.observe(el);

    return () => {
      live = false;
      ro?.disconnect();
      candleRef.current = null;
      volumeRef.current = null;
      setApis(null);
      chart.remove();
    };
  }, [api, ws, symbol, interval, height]);

  // Live kline frames → series.update + the overlay bar index — via the
  // shared useChannel binding (ref-stable handler, auto re-subscribe on
  // symbol/interval change).
  useChannel(ws, klineChannel(symbol, interval), (frame) => {
    const k = parseKline(frame.data);
    // Channel is authoritative for routing; the symbol check only
    // rejects frames that positively identify a different symbol.
    if (!k || (k.symbol !== undefined && k.symbol !== symbol)) return;
    const bar = liveKlineToBar(k);
    const time = bar.time as UTCTimestamp;
    candleRef.current?.update({
      time,
      open: bar.open,
      high: bar.high,
      low: bar.low,
      close: bar.close,
    });
    volumeRef.current?.update({
      time,
      value: bar.volume,
      color: bar.close >= bar.open ? '#10b98155' : '#ef444455',
    });
    setBars((prev) => {
      const idx = prev.findIndex((b) => b.openTimeMs === k.openTimeMs);
      if (idx >= 0) {
        const next = prev.slice();
        next[idx] = { openTimeMs: k.openTimeMs, time };
        return next;
      }
      return [...prev, { openTimeMs: k.openTimeMs, time }].slice(-600);
    });
  });

  // ---- overlay data (Task 10.3.15) -----------------------------------------
  const allOrders = useOrders({ symbol, limit: 200 });
  const positions = usePositions();
  const instrument = useInstrument(symbol);
  const tickSize = instrument?.tickSize ?? Dec.ZERO;

  const position = (positions.data ?? []).find((p) => p.symbol === symbol);
  const all = useMemo(() => allOrders.data?.orders ?? [], [allOrders.data]);
  const workingOrders = useMemo(
    () =>
      all
        .filter((o: Order) => isOpenOrder(o) && (o.price !== undefined || o.stopPrice !== undefined))
        .map((o: Order) => ({
          order: o,
          price:
            o.type === 'STOP' || o.type === 'STOP_LIMIT'
              ? (o.stopPrice ?? o.price)
              : o.price,
        }))
        .filter((w): w is { order: Order; price: Dec } => w.price !== undefined),
    [all],
  );
  const fills = useMemo(
    () => all.filter((o: Order) => o.status === 'FILLED' && o.avgFillPrice !== undefined),
    [all],
  );

  // Keep overlay price levels inside the autoscale window — a resting
  // order far from last price must still render its line.
  const overlayBounds = useMemo(() => {
    let min: Dec | undefined;
    let max: Dec | undefined;
    const acc = (p: Dec | undefined) => {
      if (p === undefined) return;
      if (min === undefined || p.lt(min)) min = p;
      if (max === undefined || p.gt(max)) max = p;
    };
    for (const w of workingOrders) acc(w.price);
    if (position !== undefined) {
      acc(position.entryPrice);
      acc(position.liquidationPrice);
    }
    return min !== undefined && max !== undefined
      ? { min: min.toNumber(), max: max.toNumber() }
      : null;
  }, [workingOrders, position]);

  useEffect(() => {
    if (apis === null) return;
    apis.candles.applyOptions({
      autoscaleInfoProvider: (original: AutoscaleInfo | null): AutoscaleInfo | null => {
        // Never hand lwc a priceRange-less object back — it decodes the
        // provider's return verbatim (undefined.minValue crash).
        if (original?.priceRange == null && overlayBounds === null) return null;
        if (overlayBounds === null) return original;
        // An empty series (no candles yet) yields no base range — seed one
        // from the overlay levels so resting orders still render.
        let { min, max } = overlayBounds;
        if (original?.priceRange != null) {
          min = Math.min(min, original.priceRange.minValue);
          max = Math.max(max, original.priceRange.maxValue);
        }
        if (min === max) {
          const pad = Math.max(Math.abs(min) * 1e-4, 1e-8);
          min -= pad;
          max += pad;
        }
        return { ...original, priceRange: { minValue: min, maxValue: max } };
      },
    });
  }, [apis, overlayBounds]);

  return (
    <div className="relative">
      <div
        ref={containerRef}
        className="w-full overflow-hidden rounded-lg border border-neutral-800"
        data-testid="trading-chart"
        data-symbol={symbol}
        data-interval={interval}
        role="img"
        aria-label={`${symbol} ${interval} candlestick chart`}
      />
      {apis !== null && (
        <ChartOverlays
          chart={apis.chart}
          series={apis.candles}
          symbol={symbol}
          interval={interval}
          bars={bars}
          workingOrders={workingOrders}
          fills={fills}
          position={position}
          tickSize={tickSize}
        />
      )}
    </div>
  );
}
