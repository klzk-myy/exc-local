/**
 * TradingChart — the Lightweight Charts boundary (Task 10.3.4).
 *
 * THIS MODULE IS THE LAZY CHUNK: `lightweight-charts` (~45 kB gzip) is
 * imported here and nowhere else, and ChartPage mounts this component
 * through React.lazy + Suspense — so the chart library never enters the
 * initial bundle (≤300 kB gate, Task 10.3.1).
 *
 *   - Candlestick series + volume histogram (overlay price scale)
 *   - History via the UDF adapter (GET /klines/{symbol}?interval=…)
 *   - Live bars via `kline@{symbol}_{interval}` — `series.update` merges
 *     each frame into the currently-open bar, then rolls on open_time
 *   - Strictly typed around the v5 API (`addSeries(CandlestickSeries, …)`)
 */
import { useEffect, useRef } from 'react';
import {
  CandlestickSeries,
  ColorType,
  createChart,
  CrosshairMode,
  HistogramSeries,
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
import type { WsClient } from '@/lib/ws';

import { getBars } from './udf';

export interface TradingChartProps {
  symbol: string;
  interval: KlineInterval;
  api?: ApiClient;
  ws?: WsClient;
  /** Fixed height — the chart owns its box (parent controls width). */
  height?: number;
}

const toTime = (ms: number): UTCTimestamp => Math.floor(ms / 1000) as UTCTimestamp;

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

  // Chart lifecycle — recreated per symbol/interval (interval switch
  // re-keys the canvas AND the kline channel).
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
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

    let live = true;
    getBars(api, symbol, interval, { limit: 500 })
      .then((bars) => {
        if (!live) return;
        candles.setData(
          bars.map((b): CandlestickData<UTCTimestamp> => ({
            time: b.time as UTCTimestamp,
            open: b.open,
            high: b.high,
            low: b.low,
            close: b.close,
          })),
        );
        volume.setData(
          bars.map((b): HistogramData<UTCTimestamp> => ({
            time: b.time as UTCTimestamp,
            value: b.volume,
            color: b.close >= b.open ? '#10b98155' : '#ef444455',
          })),
        );
        chart.timeScale().fitContent();
      })
      .catch(() => {
        /* history unavailable — the live channel still feeds the chart */
      });

    const ro =
      typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(() => {
            chart.applyOptions({ width: el.clientWidth });
          })
        : null;
    ro?.observe(el);

    const unsub = ws.subscribe(klineChannel(symbol, interval), (frame) => {
      const k = parseKline(frame.data);
      // Channel is authoritative for routing; the symbol check only
      // rejects frames that positively identify a different symbol.
      if (!k || (k.symbol !== undefined && k.symbol !== symbol)) return;
      candleRef.current?.update({
        time: toTime(k.openTimeMs),
        open: Number(k.open),
        high: Number(k.high),
        low: Number(k.low),
        close: Number(k.close),
      });
      volumeRef.current?.update({
        time: toTime(k.openTimeMs),
        value: Number(k.volume),
        color: Number(k.close) >= Number(k.open) ? '#10b98155' : '#ef444455',
      });
    });

    return () => {
      live = false;
      unsub();
      ro?.disconnect();
      candleRef.current = null;
      volumeRef.current = null;
      chart.remove();
    };
  }, [api, ws, symbol, interval, height]);

  return (
    <div
      ref={containerRef}
      className="w-full overflow-hidden rounded-lg border border-neutral-800"
      data-testid="trading-chart"
      data-symbol={symbol}
      data-interval={interval}
      role="img"
      aria-label={`${symbol} ${interval} candlestick chart`}
    />
  );
}
