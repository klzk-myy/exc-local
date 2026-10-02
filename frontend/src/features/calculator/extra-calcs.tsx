/**
 * Supplemental calculators (Task 8f):
 *   - Standalone P&L — pure math, no feed dependency.
 *   - Overnight financing — GET /instruments/{symbol}/swap-rates (live
 *     Task-7 mount) supplies the published points sheet; the projection
 *     follows InterbankSwapCharge = qty × points × days verbatim, with
 *     the admin markup leg shown separately.
 */
import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { tryDec } from '@/lib/decimal/decimal';
import { cardCls, inputCls, labelCls } from '@/lib/ui';
import type { Instrument } from '@/lib/trading/types';
import { UnavailablePanel } from '@/lib/input-helpers';

import { computePnL, computeSwap, parseCalcField, type CalcSide } from './calc';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

export function SideToggle({
  side,
  onChange,
}: {
  side: CalcSide;
  onChange: (s: CalcSide) => void;
}) {
  return (
    <div className="mb-4 grid grid-cols-2 gap-2" role="group" aria-label="Position side">
      {(['LONG', 'SHORT'] as const).map((s) => (
        <button
          key={s}
          type="button"
          aria-pressed={side === s}
          onClick={() => onChange(s)}
          className={`rounded py-1.5 text-sm font-semibold ${
            side === s
              ? s === 'LONG'
                ? 'bg-emerald-600 text-white'
                : 'bg-red-600 text-white'
              : 'bg-neutral-800 text-neutral-400 hover:bg-neutral-700'
          }`}
        >
          {s}
        </button>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Standalone P&L
// ---------------------------------------------------------------------------

export function PnlSection({ symbol }: { symbol: string }) {
  const [side, setSide] = useState<CalcSide>('LONG');
  const [qtyText, setQtyText] = useState('1000');
  const [entryText, setEntryText] = useState('');
  const [exitText, setExitText] = useState('');

  const result = useMemo(
    () =>
      computePnL(
        symbol,
        side,
        parseCalcField(qtyText),
        parseCalcField(entryText),
        parseCalcField(exitText),
      ),
    [symbol, side, qtyText, entryText, exitText],
  );

  return (
    <div className={cardCls}>
      <h2 className="mb-3 text-sm font-semibold text-neutral-200">P&amp;L calculator</h2>
      <SideToggle side={side} onChange={setSide} />
      <div className="mb-4 grid grid-cols-3 gap-2">
        <div>
          <label htmlFor="pnl-qty" className={labelCls}>
            Quantity
          </label>
          <input
            id="pnl-qty"
            inputMode="decimal"
            value={qtyText}
            onChange={(e) => setQtyText(e.target.value)}
            className={inputCls}
          />
        </div>
        <div>
          <label htmlFor="pnl-entry" className={labelCls}>
            Entry
          </label>
          <input
            id="pnl-entry"
            inputMode="decimal"
            value={entryText}
            onChange={(e) => setEntryText(e.target.value)}
            className={inputCls}
          />
        </div>
        <div>
          <label htmlFor="pnl-exit" className={labelCls}>
            Exit
          </label>
          <input
            id="pnl-exit"
            inputMode="decimal"
            value={exitText}
            onChange={(e) => setExitText(e.target.value)}
            className={inputCls}
          />
        </div>
      </div>
      {result === undefined ? (
        <p className="py-3 text-center text-xs text-neutral-500" role="status">
          Enter quantity, entry, and exit to compute P&amp;L.
        </p>
      ) : (
        <div aria-live="polite">
          <p
            className={`text-lg font-semibold ${
              result.pnlQuote.isNegative()
                ? 'text-red-400'
                : result.pnlQuote.isZero()
                  ? 'text-neutral-300'
                  : 'text-emerald-400'
            }`}
            data-testid="pnl-result"
          >
            {result.pnlQuote.toDisplay(2)} {result.quoteCcy}
          </p>
          <p className="text-xs text-neutral-500">
            {result.priceMove.isNegative() ? '' : '+'}
            {result.pips.toDisplay(1)} pips · quote-currency, before costs
          </p>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Overnight financing (swap/rollover)
// ---------------------------------------------------------------------------

interface SwapDay {
  effectiveDate: string;
  longPoints: string;
  shortPoints: string;
  longMarkupBps: string;
  shortMarkupBps: string;
  triple: boolean;
}

function parseSwapDay(v: unknown): SwapDay | null {
  if (!isRecord(v)) return null;
  const d = v;
  const eff = d['effective_date'];
  if (typeof eff !== 'string') return null;
  return {
    effectiveDate: eff,
    longPoints: typeof d['long_points'] === 'string' ? d['long_points'] : '0',
    shortPoints: typeof d['short_points'] === 'string' ? d['short_points'] : '0',
    longMarkupBps: typeof d['long_markup_bps'] === 'string' ? d['long_markup_bps'] : '0',
    shortMarkupBps: typeof d['short_markup_bps'] === 'string' ? d['short_markup_bps'] : '0',
    triple: d['triple'] === true,
  };
}

async function latestSwapDay(symbol: string): Promise<SwapDay | null> {
  const res = await apiClient.get<unknown>(
    `/instruments/${encodeURIComponent(symbol)}/swap-rates?limit=1`,
  );
  const data = isRecord(res) && Array.isArray(res['data']) ? res['data'] : [];
  return data.length > 0 ? parseSwapDay(data[0]) : null;
}

export function SwapSection({
  symbol,
  instrument,
}: {
  symbol: string;
  instrument: Instrument | undefined;
}) {
  const [side, setSide] = useState<CalcSide>('LONG');
  const [qtyText, setQtyText] = useState('1000');
  const [daysText, setDaysText] = useState('1');

  const q = useQuery({
    queryKey: ['swap-rates', symbol],
    queryFn: () => latestSwapDay(symbol),
    retry: false,
    staleTime: 300_000,
  });

  const day = q.data ?? null;
  const days = (() => {
    const n = Number(daysText);
    if (!Number.isFinite(n) || n < 1) return 1;
    return Math.min(Math.floor(n), 30);
  })();

  const points =
    day === null ? undefined : tryDec(side === 'LONG' ? day.longPoints : day.shortPoints);
  const markup =
    day === null ? undefined : tryDec(side === 'LONG' ? day.longMarkupBps : day.shortMarkupBps);
  const result = computeSwap(symbol, side, parseCalcField(qtyText), points, markup, days);

  if (q.error instanceof ApiError && q.error.status >= 500) {
    return (
      <div className={cardCls}>
        <UnavailablePanel
          feature="Swap rate sheet"
          owner="Phase-03 Task 3.3.11"
          note="The published interbank sheet is unavailable — financing cannot be projected offline."
        />
      </div>
    );
  }

  return (
    <div className={cardCls}>
      <h2 className="mb-3 text-sm font-semibold text-neutral-200">
        Overnight financing <span className="font-normal text-neutral-500">(Tom-Next)</span>
      </h2>
      {day !== null && (
        <p className="mb-2 text-xs text-neutral-500">
          Sheet {day.effectiveDate}
          {day.triple ? ' · triple-day roll' : ''} · {side} points{' '}
          {side === 'LONG' ? day.longPoints : day.shortPoints}
        </p>
      )}
      {q.isLoading && <p className="mb-2 text-xs text-neutral-500">Loading sheet…</p>}
      {day === null && !q.isLoading && !q.error && (
        <p className="mb-2 text-xs text-amber-400">
          No published sheet for {symbol} — financing cannot be projected.
        </p>
      )}
      <SideToggle side={side} onChange={setSide} />
      <div className="mb-4 grid grid-cols-2 gap-2">
        <div>
          <label htmlFor="swap-qty" className={labelCls}>
            Quantity
          </label>
          <input
            id="swap-qty"
            inputMode="decimal"
            value={qtyText}
            onChange={(e) => setQtyText(e.target.value)}
            className={inputCls}
          />
        </div>
        <div>
          <label htmlFor="swap-days" className={labelCls}>
            Days held
          </label>
          <input
            id="swap-days"
            inputMode="numeric"
            value={daysText}
            onChange={(e) => setDaysText(e.target.value)}
            className={inputCls}
          />
        </div>
      </div>
      {result === undefined ? (
        <p className="py-3 text-center text-xs text-neutral-500" role="status">
          {instrument === undefined
            ? 'Select a listed instrument.'
            : 'Awaiting a published rate sheet.'}
        </p>
      ) : (
        <div aria-live="polite">
          <p className="text-lg font-semibold text-neutral-100" data-testid="swap-result">
            {result.totalQuote.toDisplay(2)} {result.quoteCcy}
          </p>
          <p className="text-xs text-neutral-500">
            interbank {result.interbankQuote.toDisplay(4)} · markup{' '}
            {result.markupQuote.toDisplay(4)} · {result.daysApplied}d · quote currency. Negative =
            charge.
          </p>
        </div>
      )}
    </div>
  );
}
