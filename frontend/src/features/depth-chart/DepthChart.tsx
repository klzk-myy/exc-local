/**
 * Interactive cumulative depth chart (Task 10.3.12) — canonical L2
 * `depth@{symbol}` + REST snapshot fallback, rendered as two cumulative
 * curves meeting at the mid-price line.
 *
 * Contract:
 *   - green bids / red asks, PLUS a hatch pattern on the ask area and
 *     text labels — color is never the sole signal (WCAG 2.1 AA);
 *   - hover crosshair → tooltip with price, cumulative qty and
 *     cumulative notional on each side;
 *   - zoom: wheel / ± / reset buttons, centered on mid;
 *   - click a point → writes the order-draft store (chart-to-ticket
 *     seam shared with Task 10.3.15);
 *   - stale feed → visible badge; empty book → honest "no depth" state.
 */
import {
  useMemo,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type WheelEvent as ReactWheelEvent,
} from 'react';

import { wsClient } from '@/app/runtime';
import { useWsStatus, type WsClient } from '@/lib/ws';
import { Dec, maxDec } from '@/lib/decimal/decimal';
import { formatPrice, priceDecimals } from '@/lib/trading/fx';
import { bookMid, cumulate, depthAtPrice } from '@/lib/trading/projections';
import { useChannelHealth, useDepthBook, useMarketFeed } from '@/lib/trading/marketStore';
import { useOrderDraft } from '@/lib/trading/orderDraft';
import { useBookSnapshot, useInstrument } from '@/lib/trading/queries';
import type { BookSnapshot } from '@/lib/trading/types';

const W = 720;
const H = 300;
const PAD_L = 8;
const PAD_R = 76;
const PAD_T = 10;
const PAD_B = 22;

export interface DepthChartProps {
  symbol: string;
  client?: WsClient;
}

interface Hover {
  x: number;
  price: Dec;
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
  const [hover, setHover] = useState<Hover | null>(null);
  const svgRef = useRef<SVGSVGElement>(null);
  const decimals = priceDecimals(symbol, instrument?.tickSize);

  const geom = useMemo(() => {
    if (!book || (book.bids.length === 0 && book.asks.length === 0)) return null;
    const mid = bookMid(book) ?? book.bids[0]?.price ?? book.asks[0]?.price;
    if (!mid) return null;

    const worstBid = book.bids.at(-1)?.price ?? mid;
    const bestAsk = book.asks.at(-1)?.price ?? mid;
    const dev = maxDec(mid.sub(worstBid), bestAsk.sub(mid));
    const halfSpan = dev.mul(Dec.of(Math.max(0.05, Math.min(1, zoom)).toFixed(4)));
    const lo = mid.sub(halfSpan);
    const hi = mid.add(halfSpan);
    if (!hi.gt(lo)) return null;

    const bidPts = cumulate(book.bids).filter((p) => p.price.gte(lo));
    const askPts = cumulate(book.asks).filter((p) => p.price.lte(hi));
    const maxCum = maxDec(bidPts.at(-1)?.cumQty ?? Dec.ZERO, askPts.at(-1)?.cumQty ?? Dec.ZERO);
    if (!maxCum.isPositive()) return null;

    const plotW = W - PAD_L - PAD_R;
    const x = (p: Dec) => PAD_L + p.sub(lo).div(hi.sub(lo)).toNumber() * plotW;
    const y = (cum: Dec) => PAD_T + (1 - cum.div(maxCum).toNumber()) * (H - PAD_T - PAD_B);
    const midX = x(mid);

    // Bid area: fills left of mid. Walk levels worst→best so cum descends.
    const bidPath =
      bidPts.length > 0
        ? `M ${midX} ${y(Dec.ZERO)} ` +
          [...bidPts]
            .reverse()
            .map((p) => `L ${x(p.price)} ${y(p.cumQty)}`)
            .join(' ') +
          ` L ${x(bidPts[0]?.price ?? lo)} ${y(Dec.ZERO)} Z`
        : '';
    const askPath =
      askPts.length > 0
        ? `M ${midX} ${y(Dec.ZERO)} ` +
          askPts.map((p) => `L ${x(p.price)} ${y(p.cumQty)}`).join(' ') +
          ` L ${x(askPts.at(-1)?.price ?? hi)} ${y(Dec.ZERO)} Z`
        : '';

    return { mid, lo, hi, midX, bidPath, askPath, bidPts, askPts, x, y, maxCum };
  }, [book, zoom]);

  const priceAtX = (px: number): Dec | undefined => {
    if (!geom || !Number.isFinite(px)) return undefined;
    const frac = (px - PAD_L) / (W - PAD_L - PAD_R);
    return geom.lo.add(geom.hi.sub(geom.lo).mul(Dec.of(Math.max(0, Math.min(1, frac)).toFixed(6))));
  };

  const onMove = (e: ReactPointerEvent<SVGSVGElement>) => {
    const rect = svgRef.current?.getBoundingClientRect();
    if (!rect) return;
    // Degenerate layout (jsdom / zero-size) → treat clientX as viewBox coords.
    const px = rect.width > 0 ? ((e.clientX - rect.left) / rect.width) * W : e.clientX;
    const price = priceAtX(px);
    if (price) setHover({ x: px, price });
  };
  const onWheel = (e: ReactWheelEvent<SVGSVGElement>) => {
    setZoom((z) => Math.max(0.05, Math.min(1, z * (e.deltaY > 0 ? 1.2 : 1 / 1.2))));
  };
  const onClick = () => {
    if (hover) setDraft({ symbol, price: hover.price.toFixed(decimals) });
  };

  const hoverDepth =
    hover && book
      ? {
          bid: depthAtPrice('BID', hover.price, book),
          ask: depthAtPrice('ASK', hover.price, book),
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

      {geom === null ? (
        <p className="py-10 text-center text-sm text-neutral-500" role="status">
          {rest.isLoading ? 'Loading depth…' : `No depth data for ${symbol}.`}
        </p>
      ) : (
        <svg
          ref={svgRef}
          viewBox={`0 0 ${String(W)} ${String(H)}`}
          className="h-auto w-full cursor-crosshair"
          role="img"
          aria-label={`Depth chart — mid ${geom.mid.toFixed(decimals)}`}
          onPointerMove={onMove}
          onPointerLeave={() => setHover(null)}
          onWheel={onWheel}
          onClick={onClick}
        >
          <rect x={0} y={0} width={W} height={H} fill="#0a0a0a" rx={6} />
          <defs>
            <pattern
              id="askHatch"
              width="6"
              height="6"
              patternTransform="rotate(45)"
              patternUnits="userSpaceOnUse"
            >
              <rect width="6" height="6" fill="#f87171" fillOpacity="0.10" />
              <line
                x1="0"
                y1="0"
                x2="0"
                y2="6"
                stroke="#f87171"
                strokeOpacity="0.25"
                strokeWidth="1"
              />
            </pattern>
          </defs>

          {/* bid / ask cumulative areas */}
          {geom.bidPath !== '' && (
            <path
              d={geom.bidPath}
              fill="#34d399"
              fillOpacity="0.18"
              stroke="#34d399"
              strokeWidth={1.5}
            />
          )}
          {geom.askPath !== '' && (
            <path d={geom.askPath} fill="url(#askHatch)" stroke="#f87171" strokeWidth={1.5} />
          )}

          {/* mid line */}
          <line
            x1={geom.midX}
            x2={geom.midX}
            y1={PAD_T}
            y2={H - PAD_B}
            stroke="#38bdf8"
            strokeDasharray="4 3"
          />
          <text x={geom.midX} y={H - PAD_B + 13} fontSize={9} fill="#38bdf8" textAnchor="middle">
            mid {geom.mid.toFixed(decimals)}
          </text>

          {/* side labels — non-color distinguisher */}
          <text x={PAD_L + 6} y={PAD_T + 12} fontSize={10} fill="#34d399" fontWeight={600}>
            ▲ BIDS
          </text>
          <text x={W - PAD_R - 44} y={PAD_T + 12} fontSize={10} fill="#f87171" fontWeight={600}>
            ASKS ▼
          </text>

          {/* price axis ends */}
          <text x={PAD_L} y={H - PAD_B + 13} fontSize={9} fill="#737373">
            {geom.lo.toFixed(decimals)}
          </text>
          <text x={W - PAD_R} y={H - PAD_B + 13} fontSize={9} fill="#737373">
            {geom.hi.toFixed(decimals)}
          </text>
          {/* qty axis */}
          <text x={W - PAD_R + 4} y={PAD_T + 8} fontSize={9} fill="#737373">
            {geom.maxCum.toDisplay(0)}
          </text>
          <text x={W - PAD_R + 4} y={H - PAD_B} fontSize={9} fill="#737373">
            0
          </text>

          {/* crosshair */}
          {hover !== null && (
            <line
              x1={hover.x}
              x2={hover.x}
              y1={PAD_T}
              y2={H - PAD_B}
              stroke="#a3a3a3"
              strokeWidth={0.75}
              strokeDasharray="2 2"
            />
          )}
        </svg>
      )}

      {/* crosshair readout (DOM tooltip — keyboard/SR accessible) */}
      <div
        className="mt-2 flex items-center justify-between text-xs text-neutral-400"
        aria-live="polite"
      >
        {hover !== null && hoverDepth !== undefined ? (
          <span>
            price {formatPrice(symbol, hover.price, instrument?.tickSize)} · bid depth{' '}
            {hoverDepth.bid.qty.toDisplay(2)} ({hoverDepth.bid.notional.toDisplay(0)} quote) · ask
            depth {hoverDepth.ask.qty.toDisplay(2)} ({hoverDepth.ask.notional.toDisplay(0)} quote)
          </span>
        ) : (
          <span>
            Hover for cumulative depth · click to prefill the order ticket · scroll to zoom
          </span>
        )}
        <span className="text-neutral-600">zoom {(zoom * 100).toFixed(0)}%</span>
      </div>
    </section>
  );
}
