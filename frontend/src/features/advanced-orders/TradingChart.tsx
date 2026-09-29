/**
 * Chart trading & order overlays (Task 10.3.15).
 *
 * SVG candlestick surface (klines REST + live `kline@{symbol}_{tf}`
 * frames) with interactive overlays:
 *   - open orders → draggable dashed price lines (click/drag or
 *     ↑/↓ keyboard nudge by tick size) — release opens the inspect
 *     modal carrying the proposed price; quantity-down keeps queue
 *     priority, price drags are cancel-replace with a priority-loss
 *     warning (§6.9);
 *   - fills → ▲/▼ markers at avg fill price on the nearest candle;
 *   - open position → solid entry line + dashed liquidation line;
 *   - current-candle close countdown (fixed-width intervals only).
 *
 * All geometry uses SVG attributes (no inline styles) so the strict
 * production CSP (`style-src 'self'`) never blocks the chart.
 */
import { useQueryClient } from '@tanstack/react-query';
import { useMemo, useRef, useState, type PointerEvent as ReactPointerEvent } from 'react';

import { wsClient } from '@/app/runtime';
import { useChannel, type EventFrame, type SnapshotFrame, type WsClient } from '@/lib/ws';
import { KLINE_INTERVAL_MS, klineChannel, type KlineInterval } from '@/lib/market/channels';
import { useNow, formatCountdown } from '@/lib/ui';
import { Dec } from '@/lib/decimal/decimal';
import { priceDecimals } from '@/lib/trading/fx';
import { useInstrument, useKlines, useOrders, usePositions } from '@/lib/trading/queries';
import type { Kline, Order } from '@/lib/trading/types';
import { isOpenOrder, parseKline } from '@/lib/trading/types';

import { OrderInspectModal } from './OrderInspectModal';

const W = 720;
const H = 300;
const PRICE_AXIS_W = 72;
const TOP_PAD = 8;
const BOT_PAD = 18;

interface Scales {
  min: Dec;
  max: Dec;
  candleW: number;
}

function yOf(price: Dec, s: Scales): number {
  const span = s.max.sub(s.min);
  if (span.isZero()) return (TOP_PAD + H - BOT_PAD) / 2;
  const frac = price.sub(s.min).div(span).toNumber();
  return TOP_PAD + (1 - frac) * (H - BOT_PAD - TOP_PAD);
}

/** Candlestick row: wick + body rects. */
function Candle({ k, x, w, s }: { k: Kline; x: number; w: number; s: Scales }) {
  const up = !k.close.lt(k.open);
  const color = up ? '#34d399' : '#f87171';
  const yO = yOf(k.open, s);
  const yC = yOf(k.close, s);
  const bodyTop = Math.min(yO, yC);
  const bodyH = Math.max(1, Math.abs(yC - yO));
  const yH = yOf(k.high, s);
  const yL = yOf(k.low, s);
  return (
    <g aria-hidden="true">
      <line x1={x + w / 2} x2={x + w / 2} y1={yH} y2={yL} stroke={color} strokeWidth={1} />
      <rect x={x} y={bodyTop} width={w} height={bodyH} fill={color} />
    </g>
  );
}

/** One order overlay line — draggable vertically (pointer) and nudgeable
 * by tick (↑/↓). Click without a move opens the inspect modal; a drag
 * opens it with the proposed price. */
function OrderLine({
  order,
  price,
  s,
  tickSize,
  onInspect,
  onPropose,
}: {
  order: Order;
  price: Dec;
  s: Scales;
  tickSize: Dec;
  onInspect: (o: Order) => void;
  onPropose: (o: Order, price: string) => void;
}) {
  const [dragY, setDragY] = useState<number | null>(null);
  const startY = useRef(0);
  const moved = useRef(false);
  const gRef = useRef<SVGGElement>(null);

  const y = dragY ?? yOf(price, s);
  const isBuy = order.side === 'BUY';
  const stroke = isBuy ? '#34d399' : '#f87171';
  const isTrigger =
    order.type === 'STOP' || order.type === 'STOP_LIMIT' || order.type === 'TRAILING_STOP';

  const priceAtY = (py: number): Dec => {
    const frac = 1 - (py - TOP_PAD) / (H - BOT_PAD - TOP_PAD);
    const p = s.min.add(s.max.sub(s.min).mul(Dec.of(Math.max(0, Math.min(1, frac)).toFixed(6))));
    return tickSize.isPositive() ? p.quantizeTo(tickSize, 'nearest') : p;
  };

  const onPointerDown = (e: ReactPointerEvent<SVGGElement>) => {
    startY.current = e.clientY;
    moved.current = false;
    (e.target as Element).setPointerCapture(e.pointerId);
  };
  const onPointerMove = (e: ReactPointerEvent<SVGGElement>) => {
    if (startY.current === 0) return;
    const dy = e.clientY - startY.current;
    if (Math.abs(dy) > 2) moved.current = true;
    if (moved.current) {
      const svg = gRef.current?.ownerSVGElement;
      if (!svg) return;
      const rect = svg.getBoundingClientRect();
      const scale = H / rect.height;
      setDragY(yOf(price, s) + dy * scale);
    }
  };
  const onPointerUp = () => {
    if (dragY !== null) onPropose(order, priceAtY(dragY).toString());
    else if (!moved.current) onInspect(order);
    setDragY(null);
    startY.current = 0;
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

  return (
    <g
      ref={gRef}
      role="button"
      tabIndex={0}
      aria-label={`${order.side} ${order.type} ${order.id} at ${price.toFixed(priceDecimals(order.symbol, tickSize.isPositive() ? tickSize : undefined))} — drag or arrow keys to reprice, Enter to inspect`}
      className="cursor-ns-resize outline-none focus-visible:opacity-80"
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onKeyDown={onKeyDown}
    >
      <line
        x1={0}
        x2={W - PRICE_AXIS_W}
        y1={y}
        y2={y}
        stroke={stroke}
        strokeWidth={1.5}
        strokeDasharray={isTrigger ? '6 3' : '2 2'}
      />
      {/* fat hit area */}
      <line x1={0} x2={W - PRICE_AXIS_W} y1={y} y2={y} stroke="transparent" strokeWidth={10} />
      <rect
        x={4}
        y={y - 9}
        width={76}
        height={16}
        rx={3}
        fill="#171717"
        stroke={stroke}
        strokeWidth={0.75}
      />
      <text x={8} y={y + 3} fontSize={9} fill={stroke} className="select-none">
        {order.side === 'BUY' ? 'B' : 'S'} {order.type.slice(0, 3)} {price.toString()}
      </text>
    </g>
  );
}

export interface TradingChartProps {
  symbol: string;
  interval?: KlineInterval;
  client?: WsClient;
}

export function TradingChart({ symbol, interval = '15m', client = wsClient }: TradingChartProps) {
  const klines = useKlines(symbol, interval, 120);
  const allOrders = useOrders({ symbol, limit: 200 });
  const positions = usePositions();
  const instrument = useInstrument(symbol);
  const queryClient = useQueryClient();
  const now = useNow(1000);
  const decimals = priceDecimals(symbol, instrument?.tickSize);

  const [inspect, setInspect] = useState<Order | null>(null);
  const [proposed, setProposed] = useState<string | undefined>(undefined);

  // Live candle merge — kline@ frames upsert by open_time_ms.
  const [liveBars, setLiveBars] = useState<Kline[]>([]);
  useChannel(client, klineChannel(symbol, interval), (f: EventFrame | SnapshotFrame) => {
    const k = parseKline(f.data);
    if (!k) return;
    setLiveBars((prev) => {
      const idx = prev.findIndex((b) => b.openTimeMs === k.openTimeMs);
      if (idx >= 0) {
        const next = prev.slice();
        next[idx] = k;
        return next;
      }
      return [...prev, k].slice(-120);
    });
    // keep REST list fresh too (1s upstream cache)
    void queryClient.invalidateQueries({ queryKey: ['klines', symbol, interval] });
  });

  const bars = useMemo(() => {
    const base = klines.data ?? [];
    const merged = base.slice();
    for (const lb of liveBars) {
      const idx = merged.findIndex((b) => b.openTimeMs === lb.openTimeMs);
      if (idx >= 0) merged[idx] = lb;
      else merged.push(lb);
    }
    return merged.sort((a, b) => a.openTimeMs - b.openTimeMs).slice(-120);
  }, [klines.data, liveBars]);

  const position = (positions.data ?? []).find((p) => p.symbol === symbol);
  const all = allOrders.data?.orders ?? [];
  const workingOrders = all.filter(
    (o) => isOpenOrder(o) && (o.price !== undefined || o.stopPrice !== undefined),
  );
  const fills = all.filter((o) => o.status === 'FILLED' && o.avgFillPrice !== undefined);

  const scales = useMemo<Scales | null>(() => {
    const first = bars[0];
    if (first === undefined) return null;
    let min = first.low;
    let max = first.high;
    for (const b of bars) {
      if (b.low.lt(min)) min = b.low;
      if (b.high.gt(max)) max = b.high;
    }
    // Overlay prices must stay visible — widen the scale to include them.
    for (const o of workingOrders) {
      const p = o.price ?? o.stopPrice;
      if (p) {
        if (p.lt(min)) min = p;
        if (p.gt(max)) max = p;
      }
    }
    const pad = max.sub(min).mul(Dec.of('0.05'));
    return { min: min.sub(pad), max: max.add(pad), candleW: (W - PRICE_AXIS_W) / bars.length };
  }, [bars, workingOrders]);

  // Countdown to current-candle close (fixed-width intervals only).
  const intervalMs = KLINE_INTERVAL_MS[interval];
  const lastOpen = bars.at(-1)?.openTimeMs;
  const countdown =
    intervalMs !== undefined && lastOpen !== undefined ? intervalMs - (now - lastOpen) : undefined;

  const yTicks = useMemo(() => {
    if (!scales) return [];
    const out: { y: number; label: string }[] = [];
    const span = scales.max.sub(scales.min);
    for (let i = 0; i <= 4; i++) {
      const p = scales.min.add(span.mul(Dec.of(i)).div(Dec.of(4)));
      out.push({ y: yOf(p, scales), label: p.toFixed(decimals) });
    }
    return out;
  }, [scales, decimals]);

  return (
    <section
      aria-label={`${symbol} chart with order overlays`}
      className="rounded-lg border border-neutral-800 bg-neutral-900 p-4"
    >
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-semibold text-neutral-200">
          {symbol} · {interval}
        </h2>
        <span className="text-xs text-neutral-500" aria-live="polite">
          {countdown !== undefined
            ? `candle closes in ${formatCountdown(Math.max(0, countdown))}`
            : ''}
        </span>
      </div>

      {scales === null ? (
        <p className="py-10 text-center text-sm text-neutral-500" role="status">
          {klines.isLoading ? 'Loading candles…' : 'No candle data for this symbol/interval.'}
        </p>
      ) : (
        <svg
          viewBox={`0 0 ${String(W)} ${String(H)}`}
          className="h-auto w-full"
          role="img"
          aria-label={`${symbol} price chart — ${bars.length} candles`}
        >
          <rect x={0} y={0} width={W} height={H} fill="#0a0a0a" rx={6} />
          {yTicks.map((t) => (
            <g key={t.y}>
              <line
                x1={0}
                x2={W - PRICE_AXIS_W}
                y1={t.y}
                y2={t.y}
                stroke="#262626"
                strokeWidth={0.5}
              />
              <text x={W - PRICE_AXIS_W + 4} y={t.y + 3} fontSize={9} fill="#737373">
                {t.label}
              </text>
            </g>
          ))}

          {bars.map((b, i) => (
            <Candle
              key={b.openTimeMs}
              k={b}
              x={i * scales.candleW + 1}
              w={Math.max(1, scales.candleW - 2)}
              s={scales}
            />
          ))}

          {/* position entry + liquidation lines */}
          {position !== undefined && (
            <g aria-label={`open ${position.side} position entry`}>
              <line
                x1={0}
                x2={W - PRICE_AXIS_W}
                y1={yOf(position.entryPrice, scales)}
                y2={yOf(position.entryPrice, scales)}
                stroke="#38bdf8"
                strokeWidth={1.5}
              />
              <text
                x={W - PRICE_AXIS_W - 64}
                y={yOf(position.entryPrice, scales) - 3}
                fontSize={9}
                fill="#38bdf8"
              >
                {position.side === 'LONG' ? 'LONG' : 'SHORT'} {position.entryPrice.toString()}
              </text>
            </g>
          )}
          {position?.liquidationPrice !== undefined && (
            <line
              x1={0}
              x2={W - PRICE_AXIS_W}
              y1={yOf(position.liquidationPrice, scales)}
              y2={yOf(position.liquidationPrice, scales)}
              stroke="#f87171"
              strokeWidth={1}
              strokeDasharray="4 4"
              aria-label="estimated liquidation price"
            />
          )}

          {/* fill markers on the nearest candle */}
          {fills.map((f) => {
            const createdMs = Date.parse(f.createdAt);
            const idx = Number.isFinite(createdMs)
              ? bars.reduce(
                  (best, b, i) =>
                    Math.abs(b.openTimeMs - createdMs) <
                    Math.abs((bars[best]?.openTimeMs ?? 0) - createdMs)
                      ? i
                      : best,
                  0,
                )
              : bars.length - 1;
            const px = f.avgFillPrice;
            if (!px) return null;
            const x = idx * scales.candleW + scales.candleW / 2;
            const y = yOf(px, scales);
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
          {workingOrders.map((o) => {
            const p =
              o.type === 'STOP' || o.type === 'STOP_LIMIT' ? (o.stopPrice ?? o.price) : o.price;
            if (!p) return null;
            return (
              <OrderLine
                key={o.id}
                order={o}
                price={p}
                s={scales}
                tickSize={instrument?.tickSize ?? Dec.ZERO}
                onInspect={(ord) => {
                  setProposed(undefined);
                  setInspect(ord);
                }}
                onPropose={(ord, price) => {
                  setProposed(price);
                  setInspect(ord);
                }}
              />
            );
          })}
        </svg>
      )}

      <p className="mt-2 text-xs text-neutral-500">
        Solid line = open position entry · dashed red = liquidation est. · dashed green/red =
        working orders (drag to reprice — price changes lose queue priority) · ▲/▼ = fills.
      </p>

      <OrderInspectModal
        order={inspect}
        proposedPrice={proposed}
        open={inspect !== null}
        onClose={() => {
          setInspect(null);
          setProposed(undefined);
        }}
      />
    </section>
  );
}
