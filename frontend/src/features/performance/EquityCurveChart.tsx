/**
 * P&L history chart — lazy chunk (kept out of the page's eager path),
 * rendered on the shared lightweight-charts seam (useLwcChart — one
 * chart library everywhere).
 *
 * There is no snapshots endpoint yet (Phase-20), so the "history" is
 * reconstructed per-pair cumulative realized P&L over the fetched order
 * window plus the current unrealized tail — the chart is captioned
 * accordingly rather than pretending to be an account equity series.
 */
import { useEffect, useRef } from 'react';
import { LineSeries, type ISeriesApi, type LineData, type UTCTimestamp } from 'lightweight-charts';

import { useLwcChart } from '@/features/charts/useLwcChart';
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

  // Index-encoded x axis: point order is ordinal, labels ride the tick
  // formatter (same time-slot trick as the depth chart's price axis).
  const { containerRef, chart } = useLwcChart({
    height: 128,
    options: {
      timeScale: {
        tickMarkFormatter: (t: number) => points[t]?.label ?? '',
        borderVisible: false,
      },
      localization: { timeFormatter: (t: number) => points[t]?.label ?? '' },
      rightPriceScale: { borderVisible: false },
      grid: { vertLines: { visible: false } },
    },
    deps: [],
  });

  const seriesRef = useRef<ISeriesApi<'Line'> | null>(null);
  useEffect(() => {
    if (chart === null) return;
    seriesRef.current = chart.addSeries(LineSeries, {
      lineWidth: 2,
      priceLineVisible: false,
      crosshairMarkerRadius: 3,
      pointMarkersVisible: true,
    });
    return () => {
      seriesRef.current = null;
    };
  }, [chart]);

  useEffect(() => {
    const series = seriesRef.current;
    if (chart === null || series === null) return;
    series.applyOptions({ color: now >= 0 ? '#34d399' : '#f87171' });
    series.setData(
      points.map((p, i): LineData<UTCTimestamp> => ({
        time: i as UTCTimestamp,
        value: p.v,
      })),
    );
    chart.timeScale().fitContent();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chart, now, summary.realizedPnl]);

  return (
    <div>
      <div
        ref={containerRef}
        className="w-full overflow-hidden rounded"
        role="img"
        aria-label="P&L curve"
        data-testid="pnl-curve"
      />
      <p className="mt-1 text-xs text-neutral-500">
        Derived cumulative P&L over the analyzed order window ({positions.length} open position
        {positions.length === 1 ? '' : 's'} included). A true equity-history series lands with the
        Phase-20 snapshots endpoint.
      </p>
    </div>
  );
}
