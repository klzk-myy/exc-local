/**
 * Interactive cumulative depth chart (Task 10.3.12) — canonical L2
 * `depth@{symbol}` + REST snapshot fallback, rendered as two cumulative
 * area curves meeting at the mid-price line on the shared
 * lightweight-charts seam (useLwcChart — one chart library everywhere).
 *
 * The price axis rides lwc's time scale via the standard trick: each
 * level's price is encoded as an integer `time` (units × 1e8), and the
 * tick/crosshair formatters decode it back — the geometry stays linear
 * in price because lwc spaces points by their numeric time.
 *
 * Contract:
 *   - green bids / red asks — plus a DASHED ask line and text labels, so
 *     color is never the sole signal (WCAG 2.1 AA);
 *   - crosshair → readout with price, cumulative qty and cumulative
 *     notional on each side (DOM row — keyboard/SR accessible);
 *   - zoom: wheel / ± / reset buttons, centered on mid;
 *   - click → writes the order-draft store (chart-to-ticket seam shared
 *     with Task 10.3.15);
 *   - stale feed → visible badge; empty book → honest "no depth" state.
 */
import { useEffect, useMemo, useRef, useState, type WheelEvent as ReactWheelEvent } from 'react';
import {
  AreaSeries,
  LineStyle,
  type AreaData,
  type IChartApi,
  type ISeriesApi,
  type MouseEventParams,
  type UTCTimestamp,
} from 'lightweight-charts';

import { useLwcChart } from '@/features/charts/useLwcChart';
import { wsClient } from '@/app/runtime';
import { useWsStatus, type WsClient } from '@/lib/ws';
import { Dec, maxDec, minDec } from '@/lib/decimal/decimal';
import { formatPrice, priceDecimals } from '@/lib/trading/fx';
import { bookMid, cumulate, depthAtPrice } from '@/lib/trading/projections';
import { useChannelHealth, useDepthBook, useMarketFeed } from '@/lib/trading/marketStore';
import { useOrderDraft } from '@/lib/trading/orderDraft';
import { useBookSnapshot, useInstrument } from '@/lib/trading/queries';
import type { BookSnapshot } from '@/lib/trading/types';

const H = 300;
/** Price → lwc `time` codec: units × 1e8 (covers every venue tick size). */
const PRICE_SCALE = 1e8;
const priceToTime = (p: Dec): UTCTimestamp =>
  Math.round(p.toNumber() * PRICE_SCALE) as UTCTimestamp;
const timeToPrice = (t: number): Dec => Dec.of(t / PRICE_SCALE);

export interface DepthChartProps {
  symbol: string;
  client?: WsClient;
}

export function DepthChart({ symbol, client = wsClient }: DepthChartProps) {
  useMarketFeed(symbol, client, { depth: true });
  const wsBook = useDepthBook(symbol);
  const rest = useBookSnapshot(symbol, 50);
  const book: BookSnapshot | undefined = wsBook ?? rest.data ?? undefined;
  const instrument = useInstrument(symbol);
  const setDraft = useOrderDraft((s) => s.setDraft);

  const ws = useWsStatus(client);
  const health = useChannelHealth(ws, `depth@${symbol}`);
  const stale = health.stale || ws.state === 'STALE';

  const [zoom, setZoom] = useState(1); // fraction of book range shown around mid
  const [hover, setHover] = useState<Dec | null>(null);
  const [midX, setMidX] = useState<number | null>(null);
  const decimals = priceDecimals(symbol, instrument?.tickSize);
  const decimalsRef = useRef(decimals);
  decimalsRef.current = decimals;

  const geom = useMemo(() => {
    if (!book || (book.bids.length === 0 && book.asks.length === 0)) return null;
    const mid = bookMid(book) ?? book.bids[0]?.price ?? book.asks[0]?.price;
    if (!mid) return null;

    const worstBid = book.bids.at(-1)?.price ?? mid;
    const worstAsk = book.asks.at(-1)?.price ?? mid;
    // Default viewport scales off the inside market so one deep outlier
    // level can't compress the visible book into a sliver: the zoom-1
    // half-span is capped at 50× the mid→best distance (~25× the inside
    // spread), floored at ~5bp of mid for degenerate/one-sided books.
    // Outlier levels beyond the cap are clipped from the chart but stay
    // listed in the Levels table below.
    const halfSpread = maxDec(
      mid.sub(book.bids[0]?.price ?? mid),
      (book.asks[0]?.price ?? mid).sub(mid),
    );
    const sideCap = halfSpread.isPositive()
      ? halfSpread.mul(Dec.of('50'))
      : mid.mul(Dec.of('0.05'));
    const spanFloor = mid.mul(Dec.of('0.0005'));
    const dev = maxDec(spanFloor, minDec(maxDec(mid.sub(worstBid), worstAsk.sub(mid)), sideCap));
    const halfSpan = dev.mul(Dec.of(Math.max(0.05, Math.min(1, zoom)).toFixed(4)));
    const lo = mid.sub(halfSpan);
    const hi = mid.add(halfSpan);
    if (!hi.gt(lo)) return null;

    const bidPts = cumulate(book.bids).filter((p) => p.price.gte(lo));
    const askPts = cumulate(book.asks).filter((p) => p.price.lte(hi));
    const maxCum = maxDec(bidPts.at(-1)?.cumQty ?? Dec.ZERO, askPts.at(-1)?.cumQty ?? Dec.ZERO);
    if (!maxCum.isPositive()) return null;

    return { mid, lo, hi, bidPts, askPts, maxCum };
  }, [book, zoom]);

  const fmt = (t: number) => timeToPrice(t).toFixed(decimalsRef.current);
  const { containerRef, chart } = useLwcChart({
    height: H,
    options: {
      // The x axis is price, not time — both formatters decode the
      // encoded ticks back to price strings.
      timeScale: { tickMarkFormatter: (t: number) => fmt(t) },
      localization: { timeFormatter: (t: number) => fmt(t) },
      handleScroll: false,
      handleScale: false,
    },
    deps: [symbol],
  });

  const bidSeriesRef = useRef<ISeriesApi<'Area'> | null>(null);
  const askSeriesRef = useRef<ISeriesApi<'Area'> | null>(null);

  // Series wiring — once per chart instance.
  useEffect(() => {
    if (chart === null) return;
    const c: IChartApi = chart;
    bidSeriesRef.current = c.addSeries(AreaSeries, {
      lineColor: '#34d399',
      topColor: '#34d39955',
      bottomColor: '#34d39910',
      lineWidth: 2,
      priceLineVisible: false,
      crosshairMarkerVisible: true,
    });
    askSeriesRef.current = c.addSeries(AreaSeries, {
      lineColor: '#f87171',
      topColor: '#f8717155',
      bottomColor: '#f8717110',
      lineWidth: 2,
      lineStyle: LineStyle.Dashed, // non-color distinguisher
      priceLineVisible: false,
      crosshairMarkerVisible: true,
    });

    const onMove = (param: MouseEventParams) => {
      const t = param.time;
      setHover(typeof t === 'number' ? timeToPrice(t) : null);
    };
    const onClick = (param: MouseEventParams) => {
      const t = param.time;
      if (typeof t === 'number') {
        setDraft({ symbol, price: timeToPrice(t).toFixed(decimalsRef.current) });
      }
    };
    const onRange = () => {
      const g = geomRef.current;
      setMidX(g === null ? null : c.timeScale().timeToCoordinate(priceToTime(g.mid)));
    };
    c.subscribeCrosshairMove(onMove);
    c.subscribeClick(onClick);
    c.timeScale().subscribeVisibleLogicalRangeChange(onRange);
    return () => {
      c.unsubscribeCrosshairMove(onMove);
      c.unsubscribeClick(onClick);
      c.timeScale().unsubscribeVisibleLogicalRangeChange(onRange);
      bidSeriesRef.current = null;
      askSeriesRef.current = null;
    };
  }, [chart, symbol, setDraft]);

  // Latest geom for the range-change callback (ref — avoids stale closure).
  const geomRef = useRef(geom);
  geomRef.current = geom;

  // Data push — bids ascend to mid, asks ascend from mid; a flat
  // extension point at mid keeps the areas meeting like the old fill.
  useEffect(() => {
    const bid = bidSeriesRef.current;
    const ask = askSeriesRef.current;
    if (chart === null || bid === null || ask === null) return;
    const midT = priceToTime(geom?.mid ?? Dec.ZERO);
    // lwc requires strictly-ascending unique times — collapse levels that
    // encode to the same tick (>8dp books, or a locked book where
    // best-bid == mid) keeping the deepest cumulative value.
    const toPoints = (pts: { price: Dec; cumQty: Dec }[]): AreaData<UTCTimestamp>[] =>
      pts
        .map((p) => ({ time: priceToTime(p.price), value: p.cumQty.toNumber() }))
        .filter((p, i, a) => p.time !== a[i + 1]?.time);
    // cumulate() walks best→worst: bids descend in price → reverse for
    // lwc's strictly-ascending-time requirement.
    const bidData = toPoints(geom?.bidPts.slice().reverse() ?? []);
    const askData = toPoints(geom?.askPts ?? []);
    if (geom !== null) {
      // cumulate() walks best→worst, so [0] is the best level — the
      // shallowest cum — which is what the curve shows AT mid.
      const bidMid = { time: midT, value: geom.bidPts[0]?.cumQty.toNumber() ?? 0 };
      const askMid = { time: midT, value: geom.askPts[0]?.cumQty.toNumber() ?? 0 };
      if (bidData.at(-1)?.time === midT) bidData[bidData.length - 1] = bidMid;
      else bidData.push(bidMid);
      if (askData[0]?.time === midT) askData[0] = askMid;
      else askData.unshift(askMid);
    }
    bid.setData(bidData);
    ask.setData(askData);
    chart.timeScale().fitContent();
    setMidX(geom === null ? null : chart.timeScale().timeToCoordinate(midT));
  }, [chart, geom]);

  const onWheel = (e: ReactWheelEvent<HTMLDivElement>) => {
    setZoom((z) => Math.max(0.05, Math.min(1, z * (e.deltaY > 0 ? 1.2 : 1 / 1.2))));
  };

  const hoverDepth =
    hover !== null && book !== undefined
      ? {
          bid: depthAtPrice('BID', hover, book),
          ask: depthAtPrice('ASK', hover, book),
        }
      : undefined;

  return (
    <section
      aria-label={`Cumulative market depth for ${symbol}`}
      className="rounded-lg border border-neutral-800 bg-neutral-900 p-4"
    >
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-semibold text-neutral-200">
          Depth — {symbol}
          {stale && (
            <span
              className="ml-2 rounded bg-amber-500/15 px-1.5 py-0.5 text-xs text-amber-400"
              role="status"
            >
              STALE
            </span>
          )}
        </h2>
        <div className="flex items-center gap-1" role="group" aria-label="Zoom controls">
          <button
            type="button"
            aria-label="Zoom in"
            onClick={() => setZoom((z) => Math.max(0.05, z / 1.4))}
            className="rounded border border-neutral-700 px-2 py-0.5 text-sm hover:bg-neutral-800 focus-visible:ring-2 focus-visible:ring-sky-500"
          >
            +
          </button>
          <button
            type="button"
            aria-label="Zoom out"
            onClick={() => setZoom((z) => Math.min(1, z * 1.4))}
            className="rounded border border-neutral-700 px-2 py-0.5 text-sm hover:bg-neutral-800 focus-visible:ring-2 focus-visible:ring-sky-500"
          >
            −
          </button>
          <button
            type="button"
            onClick={() => setZoom(1)}
            className="rounded border border-neutral-700 px-2 py-0.5 text-xs hover:bg-neutral-800 focus-visible:ring-2 focus-visible:ring-sky-500"
          >
            Reset
          </button>
        </div>
      </div>

      {/* The lwc container stays mounted through empty/loading states —
          useLwcChart creates the chart once on mount and would never
          retry if the element appeared only after data arrived. */}
      <div className="relative" onWheel={onWheel}>
        <div
          ref={containerRef}
          className="w-full overflow-hidden rounded-lg cursor-crosshair"
          role="img"
          aria-label={
            geom === null
              ? `Depth chart — ${symbol}`
              : `Depth chart — mid ${geom.mid.toFixed(decimals)}`
          }
        />
        {geom === null && (
          <p
            className="absolute inset-0 flex items-center justify-center text-sm text-neutral-500"
            role="status"
          >
            {rest.isLoading ? 'Loading depth…' : `No depth data for ${symbol}.`}
          </p>
        )}
        {geom !== null && midX !== null && (
          <>
            {/* mid line + label (vertical marker — lwc has no vertical
                line primitive; the overlay rides timeToCoordinate) */}
            <div
              className="pointer-events-none absolute top-0 bottom-0 border-l border-dashed border-sky-400"
              style={{ left: midX }}
              aria-hidden="true"
            />
            <div
              className="pointer-events-none absolute bottom-0 -translate-x-1/2 text-[9px] text-sky-400"
              style={{ left: midX }}
            >
              mid {geom.mid.toFixed(decimals)}
            </div>
            {/* side labels — non-color distinguisher */}
            <span className="pointer-events-none absolute left-2 top-1 text-[10px] font-semibold text-emerald-400">
              ▲ BIDS
            </span>
            <span className="pointer-events-none absolute right-2 top-1 text-[10px] font-semibold text-red-400">
              ASKS ▼
            </span>
          </>
        )}
      </div>

      {/* crosshair readout (DOM tooltip — keyboard/SR accessible) */}
      <div
        className="mt-2 flex items-center justify-between text-xs text-neutral-400"
        aria-live="polite"
      >
        {hover !== null && hoverDepth !== undefined ? (
          <span>
            price {formatPrice(symbol, hover, instrument?.tickSize)} · bid depth{' '}
            {hoverDepth.bid.qty.toDisplay(2)} ({hoverDepth.bid.notional.toDisplay(0)} quote) · ask
            depth {hoverDepth.ask.qty.toDisplay(2)} ({hoverDepth.ask.notional.toDisplay(0)} quote)
          </span>
        ) : (
          <span>Hover for cumulative depth · click to prefill the order ticket · scroll to zoom</span>
        )}
        <span className="text-neutral-600">zoom {(zoom * 100).toFixed(0)}%</span>
      </div>
    </section>
  );
}
