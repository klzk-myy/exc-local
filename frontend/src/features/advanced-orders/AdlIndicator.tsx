/**
 * ADL priority indicator (Task 10.3.13) — 5-segment bar rendering the
 * account's Auto-Deleveraging rank from `adl_indicator` on
 * private:positions (Phase-19 Task 19.3.19; §24 #269).
 *
 *   1 = lowest risk (last to be deleveraged) … 5 = highest risk.
 *
 * Rank is never fabricated: no data → an explicit "unavailable" state.
 * Colour is never the sole signal — each segment count and rank is also
 * announced textually (`aria-label` + visible "ADL 3/5").
 */
export interface AdlIndicatorProps {
  /** 1..5 or undefined when the feed hasn't published a rank. */
  rank: number | undefined;
  symbol?: string;
}

const SEGMENTS = 5;

const RANK_LABEL: Record<number, string> = {
  1: 'lowest',
  2: 'low',
  3: 'moderate',
  4: 'high',
  5: 'highest',
};

const TOOLTIP =
  'Auto-Deleveraging priority: if the insurance fund cannot absorb a ' +
  'liquidated position, profitable high-leverage positions are ' +
  'deleveraged first. Rank 5 = deleveraged earliest. Reduce risk by ' +
  'lowering leverage, taking profit, or reducing position size.';

export function AdlIndicator({ rank, symbol }: AdlIndicatorProps) {
  const valid = rank !== undefined && rank >= 1 && rank <= SEGMENTS;
  const label = valid ? (RANK_LABEL[rank] ?? 'unknown') : 'unavailable';
  const aria = valid
    ? `ADL rank ${rank} of ${SEGMENTS} — ${label} deleveraging priority`
    : 'ADL rank unavailable — the venue has not published a rank for this position';

  return (
    <span
      className="inline-flex items-center gap-1.5"
      role="img"
      aria-label={symbol ? `${aria} (${symbol})` : aria}
      title={TOOLTIP}
      tabIndex={0}
    >
      <span className="flex gap-0.5" aria-hidden="true">
        {Array.from({ length: SEGMENTS }, (_, i) => {
          const lit = valid && i < rank;
          const hot = valid && rank >= 4;
          return (
            <span
              key={i}
              className={`h-2.5 w-1.5 rounded-sm ${
                lit ? (hot ? 'bg-red-400' : 'bg-emerald-400') : 'bg-neutral-700'
              }`}
            />
          );
        })}
      </span>
      <span
        className={`text-xs ${valid ? (rank === 5 ? 'text-red-300' : 'text-neutral-300') : 'text-neutral-500 italic'}`}
      >
        {valid ? `ADL ${rank}/${SEGMENTS}` : 'ADL n/a'}
      </span>
    </span>
  );
}
