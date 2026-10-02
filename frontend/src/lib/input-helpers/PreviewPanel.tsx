/**
 * Shared preview surface (Task 10.3.29 item 3) — renders an
 * OrderPreviewResult with the risk_level → ConfirmModal severity mapping.
 */
import { useEffect, useMemo, useRef, useState } from 'react';

import { ApiError, type ApiClient } from '@/lib/api';

import { isNotImplemented } from './availability';
import {
  debounce,
  previewOrder,
  type OrderPreviewRequest,
  type OrderPreviewResult,
} from './preview';
import type { ConfirmSeverity } from './confirm';
import { formatFixed } from './format';

export function severityForRisk(risk: string): ConfirmSeverity {
  if (risk === 'HIGH') return 'HIGH';
  if (risk === 'MEDIUM') return 'MEDIUM';
  return 'LOW';
}

export interface OrderPreviewState {
  result: OrderPreviewResult | null;
  /** ApiError/NetworkError when the dry-run itself failed (e.g. the
   * request would be rejected — the error IS the preview). */
  error: ApiError | Error | null;
  /** True while a debounced request is in flight. */
  pending: boolean;
  /** True when the route exists but the handler is a stub. */
  notImplemented: boolean;
}

/**
 * Debounced (150ms) order preview — calls POST /api/v1/orders/test
 * whenever `req` changes. `req === null` disables the probe.
 */
export function useOrderPreview(
  api: ApiClient,
  req: OrderPreviewRequest | null,
): OrderPreviewState {
  const [state, setState] = useState<OrderPreviewState>({
    result: null,
    error: null,
    pending: false,
    notImplemented: false,
  });
  const seq = useRef(0);
  const reqKey = useMemo(() => (req === null ? null : JSON.stringify(req)), [req]);

  useEffect(() => {
    if (reqKey === null || req === null) {
      setState({ result: null, error: null, pending: false, notImplemented: false });
      return;
    }
    const d = debounce(() => {
      const ticket = ++seq.current;
      setState((s) => ({ ...s, pending: true }));
      previewOrder(api, req)
        .then((result) => {
          if (ticket !== seq.current) return;
          setState({ result, error: null, pending: false, notImplemented: false });
        })
        .catch((err: unknown) => {
          if (ticket !== seq.current) return;
          setState({
            result: null,
            error: err instanceof Error ? err : new Error(String(err)),
            pending: false,
            notImplemented: isNotImplemented(err),
          });
        });
    }, 150);
    d.call();
    return () => {
      d.cancel();
    };
  }, [api, req, reqKey]);

  return state;
}

const RISK_STYLE: Record<string, string> = {
  LOW: 'bg-emerald-500/20 text-emerald-400',
  MEDIUM: 'bg-amber-500/20 text-amber-400',
  HIGH: 'bg-red-500/20 text-red-400',
};

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between py-0.5 text-xs">
      <span className="text-neutral-500">{label}</span>
      <span className="font-mono text-neutral-200">{value}</span>
    </div>
  );
}

export function PreviewPanel({ state }: { state: OrderPreviewState }) {
  if (state.notImplemented) {
    return (
      <div className="rounded border border-dashed border-neutral-700 p-3 text-xs text-neutral-500">
        Order preview is unavailable on this gateway (501 — route expected live; treat as
        regression).
      </div>
    );
  }
  if (state.error !== null) {
    const code = state.error instanceof ApiError ? ` [${state.error.code}]` : '';
    return (
      <div className="rounded border border-red-900/60 bg-red-950/30 p-3 text-xs text-red-300">
        Preview rejected{code}: {state.error.message}
      </div>
    );
  }
  const r = state.result;
  if (r === null) {
    return (
      <div className="rounded border border-neutral-800 p-3 text-xs text-neutral-500">
        {state.pending ? 'Computing preview…' : 'Enter an order to preview cost & margin.'}
      </div>
    );
  }
  return (
    <div className="rounded border border-neutral-800 bg-neutral-900 p-3" aria-live="polite">
      <div className="mb-2 flex items-center justify-between">
        <span className="text-xs font-medium text-neutral-400">Order preview (non-binding)</span>
        <span
          className={`rounded px-2 py-0.5 text-xs font-semibold ${RISK_STYLE[r.risk_level] ?? ''}`}
        >
          {r.risk_level} RISK
        </span>
      </div>
      <Row label="Estimated base qty" value={formatFixed(r.estimated_base_qty, 8)} />
      <Row label="Estimated quote qty" value={formatFixed(r.estimated_quote_qty, 8)} />
      <Row label="Margin impact" value={formatFixed(r.margin, 8)} />
      <Row label="Commission est." value={formatFixed(r.commission_estimate, 8)} />
      <Row label="Spread est." value={formatFixed(r.spread_estimate, 8)} />
      {r.active_filters.length > 0 && (
        <Row label="Active filters" value={r.active_filters.join(', ')} />
      )}
      {r.warnings.map((w) => (
        <p key={w} className="mt-1 text-xs text-amber-400">
          ⚠ {w}
        </p>
      ))}
    </div>
  );
}
