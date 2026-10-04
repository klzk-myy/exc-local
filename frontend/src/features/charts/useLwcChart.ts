/**
 * useLwcChart — the shared Lightweight-Charts lifecycle seam.
 *
 * THIS MODULE MUST STAY LAZY-CHUNK-ONLY: every component that renders a
 * chart imports `lightweight-charts` through this hook, and every such
 * component is mounted via React.lazy — the ~45 kB gzip library never
 * enters the initial bundle (≤300 kB gate, Task 10.3.1).
 *
 * One chart lifecycle for every chart surface: createChart with the
 * shared theme (dark base, light variant when a `[data-theme='light']`
 * ancestor — e.g. the workspace root — requests it), ResizeObserver →
 * width, remove() on cleanup. Consumers add their own
 * series/subscriptions in a follow-up effect keyed on the returned
 * `chart` handle.
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
    // The attribution logo injects a <style> element per chart — blocked
    // by style-src 'self' — and renders an external link inside the
    // cockpit. Disabled: Apache-2.0 permits it; CSP stays strict.
    attributionLogo: false,
  },
  grid: {
    vertLines: { color: '#1f1f1f' },
    horzLines: { color: '#1f1f1f' },
  },
  crosshair: { mode: CrosshairMode.Normal },
  rightPriceScale: { borderColor: '#262626' },
  timeScale: { borderColor: '#262626' },
};

/** Light variant — mirrors the workspace theme.css neutral remap so the
 * canvas doesn't stay a black slab inside a light panel. */
export const LWC_LIGHT_OPTIONS: DeepPartial<ChartOptions> = {
  layout: {
    background: { type: ColorType.Solid, color: '#ffffff' },
    textColor: '#57534e',
    fontSize: 11,
    attributionLogo: false,
  },
  grid: {
    vertLines: { color: '#e7e5e4' },
    horzLines: { color: '#e7e5e4' },
  },
  crosshair: { mode: CrosshairMode.Normal },
  rightPriceScale: { borderColor: '#d6d3d1' },
  timeScale: { borderColor: '#d6d3d1' },
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
  const [theme, setTheme] = useState<'dark' | 'light'>('dark');
  const resizeCb = useRef(onResize);
  resizeCb.current = onResize;

  // Theme follows the nearest [data-theme] ancestor (the workspace root
  // owns it) — a MutationObserver re-creates the chart on toggle rather
  // than waiting for a symbol/interval change. Scoped the same way as
  // theme.css: no ancestor → dark.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const host = el.closest('[data-theme]');
    const read = () => {
      setTheme(host?.getAttribute('data-theme') === 'light' ? 'light' : 'dark');
    };
    read();
    if (!host || typeof MutationObserver === 'undefined') return;
    const mo = new MutationObserver(read);
    mo.observe(host, { attributes: true, attributeFilter: ['data-theme'] });
    return () => mo.disconnect();
  }, []);

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const base = theme === 'light' ? LWC_LIGHT_OPTIONS : LWC_BASE_OPTIONS;
    const c: IChartApi = createChart(el, {
      height,
      ...base,
      ...options,
    });
    let alive = true;
    const ro =
      typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(() => {
            // RO callbacks queued before disconnect can still fire after
            // c.remove() — lwc throws "Object is disposed" on late calls.
            if (!alive) return;
            try {
              c.applyOptions({ width: el.clientWidth });
              resizeCb.current?.();
            } catch {
              /* chart disposed mid-teardown */
            }
          })
        : null;
    ro?.observe(el);
    setChart(c);
    return () => {
      alive = false;
      ro?.disconnect();
      setChart(null);
      // Dispose after this commit's synchronous cleanups: consumer
      // effects declared after the hook still call chart APIs in their
      // own cleanup (e.g. unsubscribeVisibleLogicalRangeChange) and lwc
      // throws "Object is disposed" if removal ran first.
      queueMicrotask(() => c.remove());
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [theme, ...deps]);

  // Height applies in place — recreating the canvas on every panel
  // resize would reset the trader's pan/zoom mid-gesture. Width is
  // handled by the ResizeObserver; height via applyOptions (callers may
  // leave `height` out of `deps`).
  useEffect(() => {
    try {
      chart?.applyOptions({ height });
    } catch {
      /* disposed */
    }
  }, [chart, height]);

  return { containerRef, chart };
}
