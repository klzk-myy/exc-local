/**
 * Percentage quantity slider (Task 10.3.11) — 10/25/50/75/100% preset
 * buttons + a continuous slider that sizes `quantity` off free margin,
 * the account leverage tier, and the live mark price.
 *
 * Every sizing respects the instrument filters (lot step, min/max qty,
 * min notional) via `sizeByPercent`; a clamped result carries a visible
 * reason ("clamped — below min notional") rather than silently resizing.
 * Recomputes as the mark moves (the `price` prop is live via bbo@).
 */
import { useMemo, useState } from 'react';

import { Dec } from '@/lib/decimal/decimal';
import { qtyConstraints, qtyDecimals } from '@/lib/trading/fx';
import type { Instrument } from '@/lib/trading/types';

import { SIZE_PRESETS, sizeByPercent } from './sizing';

export interface PercentSliderProps {
  instrument: Instrument | undefined;
  /** Live mark (or order-side price) used to convert margin → qty. */
  price: Dec | undefined;
  /** Free margin available to this ticket, in account currency. */
  freeMargin: Dec | undefined;
  /** Effective account leverage tier for the instrument. */
  leverage: Dec;
  /** Fired with the quantized quantity string ('' when unsizable). */
  onSize: (qty: string) => void;
  disabled?: boolean;
}

export function PercentSlider({
  instrument,
  price,
  freeMargin,
  leverage,
  onSize,
  disabled = false,
}: PercentSliderProps) {
  const [pct, setPct] = useState<number>(0);

  const constraints = useMemo(() => qtyConstraints(instrument), [instrument]);
  const result = useMemo(
    () => sizeByPercent(pct, freeMargin ?? Dec.ZERO, leverage, price, constraints),
    [pct, freeMargin, leverage, price, constraints],
  );

  const apply = (next: number) => {
    setPct(next);
    const r = sizeByPercent(next, freeMargin ?? Dec.ZERO, leverage, price, constraints);
    onSize(r.qty.isZero() ? '' : r.qty.toString());
  };

  const decimals = qtyDecimals(instrument);

  return (
    <div aria-label="Size by percentage of free margin">
      <div className="flex gap-1" role="group" aria-label="Size presets">
        {SIZE_PRESETS.map((p) => (
          <button
            key={p}
            type="button"
            disabled={disabled}
            aria-pressed={pct === p}
            onClick={() => apply(p)}
            className={`flex-1 rounded border px-1 py-1 text-xs font-medium transition-colors focus-visible:ring-2 focus-visible:ring-sky-500 disabled:opacity-50 ${
              pct === p
                ? 'border-sky-600 bg-sky-600/20 text-sky-300'
                : 'border-neutral-700 text-neutral-400 hover:bg-neutral-800'
            }`}
          >
            {p}%
          </button>
        ))}
      </div>
      <input
        type="range"
        min={0}
        max={100}
        step={1}
        value={pct}
        disabled={disabled}
        aria-label="Position size as a percentage of free margin"
        aria-valuetext={`${pct}% of free margin`}
        onChange={(e) => apply(Number(e.target.value))}
        className="mt-2 w-full accent-sky-500"
      />
      <div className="mt-1 flex items-center justify-between text-xs text-neutral-500">
        <span aria-live="polite">
          {pct > 0 && !result.unavailable && result.qty.isPositive()
            ? `${pct}% → ${result.qty.toDisplay(decimals)} units (margin ${result.marginUsed.toDisplay(2)})`
            : pct > 0
              ? 'cannot size — check margin, price, and filters'
              : 'of free margin'}
        </span>
        {result.clampReason !== null && (
          <span className="text-amber-400" role="status">
            clamped — {result.clampReason}
          </span>
        )}
      </div>
    </div>
  );
}
