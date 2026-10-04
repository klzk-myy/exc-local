/**
 * Chart overlay layer (Task 10.3.15) — ported onto the lightweight-charts
 * base in the 2026-10 chart unification (supersedes the bespoke SVG
 * candlestick renderer in features/advanced-orders):
 *
 *   - open orders → draggable dashed price lines (click/drag or
 *     ↑/↓ keyboard nudge by tick size) — release opens the inspect
 *     modal carrying the proposed price; quantity-down keeps queue
 *     priority, price drags are cancel-replace with a priority-loss
 *     warning (§6.9);
 *   - fills → ▲/▼ markers at avg fill price on the nearest candle;
 *   - open position → solid entry line + dashed liquidation line;
 *   - current-candle close countdown (fixed-width intervals only).
 *
 * Geometry is pure SVG attributes (strict CSP `style-src 'self'` bans
 * inline styles); coordinates come from the chart's own price/time
 * scales so pan and zoom keep overlays glued to their price level.
 * The wrapper svg is pointer-events-none; only interactive <g> nodes
 * re-enable hit testing via the pointer-events presentation attribute.
 */
import { useRef, useState, type PointerEvent as ReactPointerEvent } from 'react';
import type {
  IChartApi,
  ISeriesApi,
  UTCTimestamp,
} from 'lightweight-charts';

import type { KlineInterval } from '@/lib/market/channels';
import { formatCountdown, useNow } from '@/lib/ui';
import { Dec } from '@/lib/decimal/decimal';
import { priceDecimals } from '@/lib/trading/fx';
import type { Order, Position } from '@/lib/trading/types';

import { OrderInspectModal } from '@/features/advanced-orders/OrderInspectModal';
import { intervalBucketMs } from './udf';

/** Bar reference for fill→candle mapping (history + live merged). */
export interface OverlayBar {
  openTimeMs: number;
  time: UTCTimestamp;
}

interface Geom {
  priceToY: (p: Dec) => number | null;
  yToPrice: (y: number) => Dec | null;
  timeToX: (t: UTCTimestamp) => number | null;
  paneW: number;
  paneH: number;
}

function useGeom(chart: IChartApi, series: ISeriesApi<'Candlestick'>): Geom {
  return {
    priceToY: (p) => series.priceToCoordinate(p.toNumber()),
    yToPrice: (y) => {
      const v = series.coordinateToPrice(y);
      return v === null ? null : Dec.of(v);
    },
    timeToX: (t) => chart.timeScale().timeToCoordinate(t),
    paneW: chart.timeScale().width(),
    paneH: chart.timeScale().height(),
  };
}

function PriceLine({
  y,
  color,
  dashed,
  paneW,
  label,
  ariaLabel,
}: {
  y: number;
  color: string;
  dashed: string;
  paneW: number;
  label?: string;
  ariaLabel: string;
}) {
  return (
    <g aria-label={ariaLabel}>
      <line
        x1={0}
        x2={paneW}
        y1={y}
        y2={y}
        stroke={color}
        strokeWidth={1.5}
        strokeDasharray={dashed}
      />
      {label !== undefined && (
        <text x={paneW - 8} y={y - 4} fontSize={9} fill={color} textAnchor="end">
          {label}
        </text>
      )}
    </g>
  );
}

/** One order overlay line — draggable vertically (pointer) and nudgeable
 * by tick (↑/↓). Click without a move opens the inspect modal; a drag
 * opens it with the proposed price. */
function OrderLine({
  order,
  price,
  geom,
  tickSize,
  onInspect,
  onPropose,
}: {
  order: Order;
  price: Dec;
  geom: Geom;
  tickSize: Dec;
  onInspect: (o: Order) => void;
  onPropose: (o: Order, price: string) => void;
}) {
  const [dragY, setDragY] = useState<number | null>(null);
  const startY = useRef<number | null>(null);
  const moved = useRef(false);
  const gRef = useRef<SVGGElement>(null);

  const baseY = geom.priceToY(price);
  const y = dragY ?? baseY;
  const isBuy = order.side === 'BUY';
  const stroke = isBuy ? '#34d399' : '#f87171';
  const isTrigger =
    order.type === 'STOP' || order.type === 'STOP_LIMIT' || order.type === 'TRAILING_STOP';

  const priceAtY = (py: number): Dec | null => {
    const p = geom.yToPrice(py);
    if (p === null) return null;
    return tickSize.isPositive() ? p.quantizeTo(tickSize, 'nearest') : p;
  };

  const onPointerDown = (e: ReactPointerEvent<SVGGElement>) => {
    startY.current = e.clientY;
    moved.current = false;
    (e.target as Element).setPointerCapture(e.pointerId);
  };
  const onPointerMove = (e: ReactPointerEvent<SVGGElement>) => {
    if (startY.current === null || baseY === null) return;
    const dy = e.clientY - startY.current;
    if (Math.abs(dy) > 2) moved.current = true;
    if (moved.current) setDragY(Math.max(0, Math.min(geom.paneH, baseY + dy)));
  };
  const onPointerUp = () => {
    if (dragY !== null) {
      const p = priceAtY(dragY);
      if (p !== null) onPropose(order, p.toString());
    } else if (!moved.current) onInspect(order);
    setDragY(null);
    startY.current = null;
  };
  const onKeyDown = (e: React.KeyboardEvent<SVGGElement>) => {
    if (e.key === 'ArrowUp' || e.key === 'ArrowDown') {
      e.preventDefault();
      const delta = e.key === 'ArrowUp' ? tickSize : tickSize.neg();
      onPropose(order, price.add(delta).toString());
    } else if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      onInspect(order);
    }
  };

  if (y === null) return null;

  return (
    <g
      ref={gRef}
      role="button"
      tabIndex={0}
      pointerEvents="auto"
      aria-label={`${order.side} ${order.type} ${order.id} at ${price.toFixed(priceDecimals(order.symbol, tickSize.isPositive() ? tickSize : undefined))} — drag or arrow keys to reprice, Enter to inspect`}
      className="cursor-ns-resize outline-none focus-visible:opacity-80"
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onKeyDown={onKeyDown}
    >
      <line
        x1={0}
        x2={geom.paneW}
        y1={y}
        y2={y}
        stroke={stroke}
        strokeWidth={1.5}
        strokeDasharray={isTrigger ? '6 3' : '2 2'}
      />
      {/* fat hit area — ≥24px tall for pointer targets */}
      <line x1={0} x2={geom.paneW} y1={y} y2={y} stroke="transparent" strokeWidth={24} />
      <rect x={4} y={y - 9} width={76} height={16} rx={3} fill="#171717" stroke={stroke} strokeWidth={0.75} />
      <text x={8} y={y + 3} fontSize={9} fill={stroke} className="select-none">
        {order.side === 'BUY' ? 'B' : 'S'} {order.type.slice(0, 3)} {price.toString()}
      </text>
    </g>
  );
}

export interface ChartOverlaysProps {
  chart: IChartApi;
  series: ISeriesApi<'Candlestick'>;
  symbol: string;
  interval: KlineInterval;
  bars: readonly OverlayBar[];
  workingOrders: readonly { order: Order; price: Dec }[];
  fills: readonly Order[];
  position: Position | undefined;
  tickSize: Dec;
}

export function ChartOverlays({
  chart,
  series,
  symbol,
  interval,
  bars,
  workingOrders,
  fills,
  position,
  tickSize,
}: ChartOverlaysProps) {
  const geom = useGeom(chart, series);
  const now = useNow(1000);
  const [inspect, setInspect] = useState<Order | null>(null);
  const [proposed, setProposed] = useState<string | undefined>(undefined);

  // Countdown to current-candle close (fixed-width intervals only —
  // intervalBucketMs is null for 1W/1M calendar frames).
  const intervalMs = intervalBucketMs(interval);
  const lastOpen = bars.at(-1)?.openTimeMs;
  const countdown =
    intervalMs !== null && lastOpen !== undefined ? intervalMs - (now - lastOpen) : undefined;

  const paneW = geom.paneW;

  return (
    <>
      <svg className="pointer-events-none absolute inset-0 h-full w-full" role="group" aria-label={`${symbol} order and position overlays`}>
        {position !== undefined &&
          (() => {
            const y = geom.priceToY(position.entryPrice);
            return y === null ? null : (
              <PriceLine
                y={y}
                color="#38bdf8"
                dashed=""
                paneW={paneW}
                label={`${position.side === 'LONG' ? 'LONG' : 'SHORT'} ${position.entryPrice.toString()}`}
                ariaLabel={`open ${position.side} position entry`}
              />
            );
          })()}
        {position?.liquidationPrice !== undefined &&
          (() => {
            const y = geom.priceToY(position.liquidationPrice);
            return y === null ? null : (
              <PriceLine
                y={y}
                color="#f87171"
                dashed="4 4"
                paneW={paneW}
                ariaLabel="estimated liquidation price"
              />
            );
          })()}

        {/* fill markers on the nearest candle */}
        {fills.map((f) => {
          const createdMs = Date.parse(f.createdAt);
          if (!Number.isFinite(createdMs) || bars.length === 0) return null;
          const bar = bars.reduce((best, b) =>
            Math.abs(b.openTimeMs - createdMs) < Math.abs(best.openTimeMs - createdMs) ? b : best,
          );
          const x = geom.timeToX(bar.time);
          const px = f.avgFillPrice;
          const y = px === undefined ? null : geom.priceToY(px);
          if (x === null || y === null) return null;
          return (
            <text
              key={f.id}
              x={x}
              y={y}
              fontSize={10}
              fill={f.side === 'BUY' ? '#34d399' : '#f87171'}
              aria-label={`fill ${f.side} ${f.id}`}
            >
              {f.side === 'BUY' ? '▲' : '▼'}
            </text>
          );
        })}

        {/* open-order overlays */}
        {workingOrders.map(({ order, price }) => (
          <OrderLine
            key={order.id}
            order={order}
            price={price}
            geom={geom}
            tickSize={tickSize}
            onInspect={(ord) => {
              setProposed(undefined);
              setInspect(ord);
            }}
            onPropose={(ord, p) => {
              setProposed(p);
              setInspect(ord);
            }}
          />
        ))}

        {/* candle-close countdown, top-right of the pane */}
        {countdown !== undefined && (
          <text
            x={paneW - 8}
            y={14}
            fontSize={10}
            fill="#737373"
            textAnchor="end"
            aria-label="candle close countdown"
          >
            closes in {formatCountdown(Math.max(0, countdown))}
          </text>
        )}
        <text x={8} y={14} fontSize={9} fill="#525252" aria-hidden="true">
          — pos · -- liq · -- orders (drag) · ▲▼ fills
        </text>
      </svg>

      <OrderInspectModal
        order={inspect}
        proposedPrice={proposed}
        open={inspect !== null}
        onClose={() => {
          setInspect(null);
          setProposed(undefined);
        }}
      />
    </>
  );
}
