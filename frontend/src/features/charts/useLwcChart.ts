/**
 * useLwcChart — the shared Lightweight-Charts lifecycle seam.
 *
 * THIS MODULE MUST STAY LAZY-CHUNK-ONLY: every component that renders a
 * chart imports `lightweight-charts` through this hook, and every such
 * component is mounted via React.lazy — the ~45 kB gzip library never
 * enters the initial bundle (≤300 kB gate, Task 10.3.1).
 *
 * One chart lifecycle for every chart surface: createChart with the
 * shared dark theme, ResizeObserver → width, remove() on cleanup.
 * Consumers add their own series/subscriptions in a follow-up effect
 * keyed on the returned `chart` handle.
 */
import { useEffect, useRef, useState, type MutableRefObject } from 'react';
import {
  ColorType,
  CrosshairMode,
  createChart,
  type ChartOptions,
  type DeepPartial,
  type IChartApi,
} from 'lightweight-charts';

/** Shared dark theme — every exchange chart starts from these options. */
export const LWC_BASE_OPTIONS: DeepPartial<ChartOptions> = {
  layout: {
    background: { type: ColorType.Solid, color: '#0a0a0a' },
    textColor: '#a3a3a3',
    fontSize: 11,
  },
  grid: {
    vertLines: { color: '#1f1f1f' },
    horzLines: { color: '#1f1f1f' },
  },
  crosshair: { mode: CrosshairMode.Normal },
  rightPriceScale: { borderColor: '#262626' },
  timeScale: { borderColor: '#262626' },
};

export interface UseLwcChartArgs {
  /** Fixed height — the chart owns its box (parent controls width). */
  height: number;
  /** Caller overrides merged AFTER the base theme. */
  options?: DeepPartial<ChartOptions>;
  /** Re-create the chart when these change (symbol/interval/etc). */
  deps: readonly unknown[];
  /** Fires after the chart's width is re-applied on container resize —
   *  overlay coordinate layers use it to re-derive positions. */
  onResize?: () => void;
}

export function useLwcChart({
  height,
  options,
  deps,
  onResize,
}: UseLwcChartArgs): {
  containerRef: MutableRefObject<HTMLDivElement | null>;
  chart: IChartApi | null;
} {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [chart, setChart] = useState<IChartApi | null>(null);
  const resizeCb = useRef(onResize);
  resizeCb.current = onResize;

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const c: IChartApi = createChart(el, {
      height,
      ...LWC_BASE_OPTIONS,
      ...options,
    });
    const ro =
      typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(() => {
            c.applyOptions({ width: el.clientWidth });
            resizeCb.current?.();
          })
        : null;
    ro?.observe(el);
    setChart(c);
    return () => {
      ro?.disconnect();
      setChart(null);
      c.remove();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return { containerRef, chart };
}
