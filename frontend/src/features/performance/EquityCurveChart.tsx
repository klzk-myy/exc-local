/**
 * P&L history chart — lazy chunk (kept out of the page's eager path).
 *
 * There is no snapshots endpoint yet (Phase-20), so the "history" is
 * reconstructed per-pair cumulative realized P&L over the fetched order
 * window plus the current unrealized tail — the chart is captioned
 * accordingly rather than pretending to be an account equity series.
 */
import type { PositionRow } from '@/lib/market/wire';

import type { PerformanceSummary } from './derive';

export default function EquityCurveChart({
  summary,
  positions,
}: {
  summary: PerformanceSummary;
  positions: PositionRow[];
}) {
  // Single-point "curve": cumulative realized + unrealized today vs the
  // window start. When a snapshots endpoint ships this component swaps
  // to a real time series — the contract point is that nothing is
  // invented now.
  const now = summary.realizedPnl + summary.unrealizedPnl;
  const points = [
    { label: 'window start', v: 0 },
    { label: 'realized (derived)', v: summary.realizedPnl },
    { label: 'now (incl. unrealized)', v: now },
  ];
  const min = Math.min(0, ...points.map((p) => p.v));
  const max = Math.max(0, ...points.map((p) => p.v));
  const span = max - min || 1;
  const w = 640;
  const h = 120;
  const step = w / Math.max(points.length - 1, 1);
  const polyline = points
    .map((p, i) => `${(i * step).toFixed(1)},${(h - ((p.v - min) / span) * h).toFixed(1)}`)
    .join(' ');
  return (
    <div>
      <svg
        viewBox={`0 0 ${w} ${h}`}
        className="h-32 w-full"
        role="img"
        aria-label="P&L curve"
        data-testid="pnl-curve"
      >
        <polyline
          fill="none"
          stroke={now >= 0 ? '#34d399' : '#f87171'}
          strokeWidth="1.5"
          points={polyline}
        />
        {points.map((p, i) => (
          <circle
            key={p.label}
            cx={i * step}
            cy={h - ((p.v - min) / span) * h}
            r="3"
            fill="#a3a3a3"
          />
        ))}
      </svg>
      <p className="mt-1 text-xs text-neutral-500">
        Derived cumulative P&L over the analyzed order window ({positions.length} open position
        {positions.length === 1 ? '' : 's'} included). A true equity-history series lands with the
        Phase-20 snapshots endpoint.
      </p>
    </div>
  );
}
