/**
 * Real-time preview engine (Task 10.3.29 item 3).
 *
 * Unified abstraction for any operation that moves money:
 *   - Order preview    → POST /api/v1/orders/test          (live — Task 5.3.39)
 *   - Funding preview  → POST /api/v1/funding/fee-estimate (stub — Phase-11)
 *   - Grid-bot preview → client-derived grid math (deterministic — no
 *     backend needed); the live 7-day backtest lands with the Phase-16
 *     grid engine and is surfaced honestly when unavailable.
 *
 * Every preview result carries `risk_level` (LOW|MEDIUM|HIGH) which
 * drives the ConfirmModal severity colour (item 8).
 */
import type { ApiClient } from '@/lib/api';

import { addDecimal, divDecimal, mulDecimal, subDecimal } from './decimal';

/** Orders preview wire shape — mirrors orders.Preview (service.go). */
export interface OrderPreviewResult {
  estimated_base_qty: string;
  estimated_quote_qty: string;
  margin: string;
  commission_estimate: string;
  spread_estimate: string;
  risk_level: 'LOW' | 'MEDIUM' | 'HIGH' | (string & {});
  warnings: string[];
  active_filters: string[];
  /** Always false server-side — a preview never binds. */
  binding: boolean;
}

export interface OrderPreviewRequest {
  symbol: string;
  side: 'BUY' | 'SELL' | (string & {});
  type: string;
  time_in_force?: string;
  quantity?: string;
  quote_quantity?: string;
  price?: string;
  stop_price?: string;
  iceberg_visible_qty?: string;
  post_only?: boolean;
  reduce_only?: boolean;
  stp_mode?: string;
  gtd_expiry?: string;
  client_order_id?: string;
}

/** POST /api/v1/orders/test — side-effect-free validation/preview. */
export function previewOrder(
  api: ApiClient,
  req: OrderPreviewRequest,
): Promise<OrderPreviewResult> {
  return api.post<OrderPreviewResult>('/orders/test', req);
}

/** Funding/withdrawal fee estimate (stub — Phase-11 Task 11.3.9). */
export interface FundingEstimate {
  rail?: string;
  fee?: string;
  currency?: string;
  arrival_estimate?: string;
  cutoff_utc?: string;
  resulting_balance?: string;
}

export function previewFunding(
  api: ApiClient,
  req: Record<string, unknown>,
): Promise<FundingEstimate> {
  return api.post<FundingEstimate>('/funding/fee-estimate', req);
}

/** Client-side grid-bot derivation (deterministic — no backend needed):
 * per-grid spacing, per-grid quantity and a notional fee estimate.
 * The 7-day backtest stays a server concern; when absent the wizard
 * shows this derivation plus an honest 'backtest pending' note. */
export interface GridDerivation {
  /** Price distance between adjacent grid levels. */
  gridStep: string | null;
  /** Per-grid notional (investment / gridCount). */
  perGridQuote: string | null;
  /** Estimated per-grid base quantity at mid price (pre-lot-rounding). */
  perGridBase: string | null;
  /** Estimated one-side fee on the full investment at `feeBps`. */
  feeEstimate: string | null;
  gridCount: number;
  lower: string;
  upper: string;
}

export function deriveGrid(
  lowerPrice: string,
  upperPrice: string,
  gridCount: number,
  totalInvestment: string,
  feeBps: string | null,
): GridDerivation {
  const span = subDecimal(upperPrice, lowerPrice);
  const denom = Math.max(gridCount - 1, 1);
  const gridStep = span === null ? null : divDecimal(span, String(denom), 8, 'down');
  const perGridQuote = divDecimal(totalInvestment, String(gridCount), 8, 'down');
  const spanHalf = span === null ? null : divDecimal(span, '2', 12);
  const midPrice = spanHalf === null ? null : addDecimal(lowerPrice, spanHalf);
  const perGridBase =
    perGridQuote !== null && midPrice !== null
      ? divDecimal(perGridQuote, midPrice, 12, 'down')
      : null;
  // bps → fraction: fee = investment × bps × 1e-4.
  const feeEstimate =
    feeBps !== null ? mulDecimal(mulDecimal(totalInvestment, feeBps) ?? '0', '0.0001') : null;
  return {
    gridStep,
    perGridQuote,
    perGridBase,
    feeEstimate,
    gridCount,
    lower: lowerPrice,
    upper: upperPrice,
  };
}

/** Debounce helper shared by preview hooks (150ms per the task text). */
export function debounce<A extends unknown[]>(
  fn: (...args: A) => void,
  ms = 150,
): { call: (...args: A) => void; cancel: () => void } {
  let t: ReturnType<typeof setTimeout> | undefined;
  return {
    call: (...args: A) => {
      if (t !== undefined) clearTimeout(t);
      t = setTimeout(() => {
        fn(...args);
      }, ms);
    },
    cancel: () => {
      if (t !== undefined) clearTimeout(t);
    },
  };
}
