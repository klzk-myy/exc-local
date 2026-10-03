/**
 * OrderBook — virtualized L2 depth (Task 10.3.2).
 *
 *   - depth{5,10,20} selector → `depth@{symbol}:{levels}:100` channel
 *   - price / qty / cumulative-total columns, cumulative depth bars
 *   - spread + mid between the two books
 *   - per-level flash on qty change (CSS color transition — no inline
 *     styles: production CSP is `style-src 'self'`, so bar widths are
 *     quantized to the WIDTH_STEPS class palette)
 *   - stale/resync surfaces from the ws health registry (Task 10.3.19)
 *   - clicking a level emits onPriceClick (quick-order prefill, 10.3.3)
 */
import { useMemo, useState, type UIEvent } from 'react';

import { apiClient, wsClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import { DEPTH_LEVELS, type DepthLevels } from '@/lib/market/channels';
import { stepDecimals } from '@/lib/market/decimal';
import { formatDecimal } from '@/lib/market/format';
import { useInstruments } from '@/lib/market/hooks';
import type { WsClient } from '@/lib/ws';

import type { BookRow } from './book';
import { useOrderBook } from './useOrderBook';
import { visibleRange } from './virtual';

const ROW_H = 24; // h-6

/** Height palette (24px steps → spacing units ×6). Production CSP is
 * `style-src 'self'` — inline `style={{height}}` is refused — so spacer
 * heights quantize to literal Tailwind classes. Covers 0–33 rows; depth
 * selector caps levels at 20. */
const H_STEPS = [
  'h-0',
  'h-6',
  'h-12',
  'h-18',
  'h-24',
  'h-30',
  'h-36',
  'h-42',
  'h-48',
  'h-54',
  'h-60',
  'h-66',
  'h-72',
  'h-78',
  'h-84',
  'h-90',
  'h-96',
  'h-102',
  'h-108',
  'h-114',
  'h-120',
  'h-126',
  'h-132',
  'h-138',
  'h-144',
  'h-150',
  'h-156',
  'h-162',
  'h-168',
  'h-174',
  'h-180',
  'h-186',
  'h-192',
  'h-198',
] as const;

function hClass(px: number): string {
  const i = Math.max(0, Math.min(H_STEPS.length - 1, Math.round(px / ROW_H)));
  return H_STEPS[i] ?? 'h-0';
}

/** Cumulative-depth bar width steps — literal Tailwind classes (5%
 * quantization). Dynamic `style={{width}}` is CSP-blocked in production,
 * and runtime-computed `w-[x%]` never reaches the compiler, so the
 * palette is enumerated. */
const WIDTH_STEPS = [
  'w-0',
  'w-[5%]',
  'w-[10%]',
  'w-[15%]',
  'w-[20%]',
  'w-[25%]',
  'w-[30%]',
  'w-[35%]',
  'w-[40%]',
  'w-[45%]',
  'w-1/2',
  'w-[55%]',
  'w-[60%]',
  'w-[65%]',
  'w-[70%]',
  'w-[75%]',
  'w-[80%]',
  'w-[85%]',
  'w-[90%]',
  'w-[95%]',
  'w-full',
] as const;

function barClass(frac: number): string {
  const i = Math.max(0, Math.min(WIDTH_STEPS.length - 1, Math.round(frac * 20)));
  return WIDTH_STEPS[i] ?? 'w-0';
}

function DepthRow(props: {
  row: BookRow;
  flash: 'up' | 'down' | undefined;
  tickDecimals: number;
  lotDecimals: number;
  onClick?: (row: BookRow) => void;
}) {
  const { row, flash } = props;
  const isBid = row.side === 'bid';
  const flashCls =
    flash === 'up'
      ? isBid
        ? 'bg-emerald-500/25'
        : 'bg-red-500/25'
      : flash === 'down'
        ? 'bg-neutral-500/25'
        : 'bg-transparent';
  const body = (
    <>
      <span
        aria-hidden
        className={`absolute inset-y-0 right-0 ${barClass(row.depthFrac)} ${
          isBid ? 'bg-emerald-500/10' : 'bg-red-500/10'
        }`}
      />
      <span className={`relative z-10 font-mono ${isBid ? 'text-emerald-400' : 'text-red-400'}`}>
        {formatDecimal(row.price, props.tickDecimals)}
      </span>
      <span className="relative z-10 font-mono text-neutral-200">
        {formatDecimal(row.qty, props.lotDecimals)}
      </span>
      <span className="relative z-10 font-mono text-neutral-400">
        {formatDecimal(row.total, props.lotDecimals)}
      </span>
    </>
  );
  const cls = `relative grid h-6 w-full grid-cols-3 items-center px-2 text-right text-xs transition-colors duration-300 ${flashCls}`;
  return props.onClick ? (
    <button
      type="button"
      className={`${cls} hover:bg-neutral-800/60`}
      onClick={() => props.onClick?.(row)}
      aria-label={`${isBid ? 'bid' : 'ask'} ${row.price}`}
    >
      {body}
    </button>
  ) : (
    <div className={cls} role="row">
      {body}
    </div>
  );
}

function BookSide(props: {
  label: string;
  rows: BookRow[];
  flashes: ReadonlyMap<string, 'up' | 'down'>;
  viewportHeight: number;
  tickDecimals: number;
  lotDecimals: number;
  onRowClick?: (row: BookRow) => void;
}) {
  const { rows, viewportHeight } = props;
  const [scrollTop, setScrollTop] = useState(0);
  const range = useMemo(
    () =>
      visibleRange({
        count: rows.length,
        rowHeight: ROW_H,
        scrollTop,
        viewportHeight,
        overscan: 2,
      }),
    [rows.length, scrollTop, viewportHeight],
  );
  function onScroll(e: UIEvent<HTMLDivElement>): void {
    setScrollTop(e.currentTarget.scrollTop);
  }
  return (
    <div
      className={`overflow-y-auto ${hClass(viewportHeight)}`}
      onScroll={onScroll}
      data-testid={`book-${props.label}`}
      role="rowgroup"
      aria-label={`${props.label} depth`}
    >
      <div className={hClass(range.topPad)} aria-hidden />
      {rows.slice(range.start, range.end).map((r) => (
        <DepthRow
          key={r.key}
          row={r}
          flash={props.flashes.get(r.key)}
          tickDecimals={props.tickDecimals}
          lotDecimals={props.lotDecimals}
          onClick={props.onRowClick}
        />
      ))}
      <div className={hClass(range.bottomPad)} aria-hidden />
      {rows.length === 0 && (
        <p className="px-2 py-3 text-center text-xs text-neutral-500">No {props.label}</p>
      )}
    </div>
  );
}

export interface OrderBookProps {
  symbol: string;
  api?: ApiClient;
  ws?: WsClient;
  /** Quick-order prefill (Task 10.3.3 item 4). */
  onPriceClick?: (price: string, side: 'bid' | 'ask') => void;
  /** Per-side viewport height in px (scrollable window). */
  viewportHeight?: number;
  /** Strip tile chrome + self-title when embedded in a framed panel. */
  bare?: boolean;
}

export function OrderBook(props: OrderBookProps) {
  const api = props.api ?? apiClient;
  const ws = props.ws ?? wsClient;
  const viewportHeight = props.viewportHeight ?? 240;
  const [levels, setLevels] = useState<DepthLevels>(20);
  const { view, flashes, stale, resyncing, error, reload } = useOrderBook({
    api,
    ws,
    symbol: props.symbol,
    levels,
  });
  const { bySymbol } = useInstruments(api);
  const inst = bySymbol.get(props.symbol);
  const tickDecimals = inst ? stepDecimals(inst.tickSize) : 5;
  const lotDecimals = inst ? stepDecimals(inst.lotSize) : 4;

  // Asks render worst→best top-down so the best ask sits adjacent to the
  // spread row; virtualization windows the REVERSED array.
  const asksReversed = useMemo(() => (view ? [...view.asks].reverse() : []), [view]);

  return (
    <section
      aria-label={`Order book ${props.symbol}`}
      className={
        props.bare
          ? 'w-full'
          : 'w-full rounded-lg border border-neutral-800 bg-neutral-900'
      }
    >
      <header className="flex items-center justify-between border-b border-neutral-800 px-3 py-2">
        <div className="flex items-center gap-2">
          {props.bare ? null : <h2 className="text-sm font-semibold">Order Book</h2>}
          <span className="text-xs text-neutral-400">{props.symbol}</span>
          {stale && (
            <span
              role="status"
              className="rounded bg-amber-500/20 px-1.5 py-0.5 text-[10px] font-semibold text-amber-400"
            >
              STALE
            </span>
          )}
          {resyncing && (
            <span
              role="status"
              className="rounded bg-sky-500/20 px-1.5 py-0.5 text-[10px] font-semibold text-sky-400"
            >
              RESYNCING
            </span>
          )}
        </div>
        <div className="flex items-center gap-1" role="group" aria-label="Depth levels">
          {DEPTH_LEVELS.map((n) => (
            <button
              key={n}
              type="button"
              aria-pressed={levels === n}
              onClick={() => setLevels(n)}
              className={`rounded px-2 py-0.5 text-xs font-medium ${
                levels === n
                  ? 'bg-neutral-700 text-white'
                  : 'text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200'
              }`}
            >
              {n}
            </button>
          ))}
        </div>
      </header>

      <div className="grid grid-cols-3 px-2 pt-2 text-right text-[10px] font-medium uppercase tracking-wide text-neutral-500">
        <span>Price</span>
        <span>Qty</span>
        <span>Total</span>
      </div>

      {error !== null ? (
        <div className="p-4 text-center">
          <p className="text-sm text-red-400" role="alert">
            {error}
          </p>
          <button
            type="button"
            onClick={reload}
            className="mt-2 rounded border border-neutral-700 px-3 py-1 text-xs text-neutral-300 hover:bg-neutral-800"
          >
            Retry
          </button>
        </div>
      ) : view === null ? (
        <p className="p-4 text-center text-sm text-neutral-500" role="status">
          Loading book…
        </p>
      ) : (
        <>
          <BookSide
            label="asks"
            rows={asksReversed}
            flashes={flashes}
            viewportHeight={viewportHeight}
            tickDecimals={tickDecimals}
            lotDecimals={lotDecimals}
            onRowClick={
              props.onPriceClick ? (r) => props.onPriceClick?.(r.price, 'ask') : undefined
            }
          />
          <div
            className="flex items-center justify-between border-y border-neutral-800 px-3 py-1.5"
            data-testid="book-spread"
          >
            <span className="text-xs text-neutral-400">
              Spread{' '}
              <span className="font-mono text-neutral-200">
                {view.spread ? view.spread.toFixed(tickDecimals) : '—'}
              </span>
            </span>
            <span className="text-xs text-neutral-400">
              Mid{' '}
              <span className="font-mono text-neutral-200">
                {view.mid ? view.mid.toFixed(tickDecimals) : '—'}
              </span>
            </span>
          </div>
          <BookSide
            label="bids"
            rows={view.bids}
            flashes={flashes}
            viewportHeight={viewportHeight}
            tickDecimals={tickDecimals}
            lotDecimals={lotDecimals}
            onRowClick={
              props.onPriceClick ? (r) => props.onPriceClick?.(r.price, 'bid') : undefined
            }
          />
        </>
      )}
    </section>
  );
}
