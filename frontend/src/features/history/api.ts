/**
 * History-surface adapters (Task 10.3.27).
 *
 *   Live   — GET /api/v1/orders (keyset cursor + full filter set),
 *            DELETE /api/v1/orders (bulk cancel by filter is NOT used
 *            here — per-row cancel goes through lib/trading cancelOrder).
 *   Live   — algo-orders, order-lists (OPO/OCO), countdown-cancel-all are
 *            all `v1live` too (supersedes the earlier "stub → 501" note —
 *            the registry carries zero StatusStub rows).
 */
import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import { parseOrderList, type Order } from '@/lib/trading/types';
import { scopeAccountId, MASTER_ACCOUNT_KEY } from '@/lib/trading/accountScope';

export interface OrderPageFilter {
  symbol?: string;
  status?: string;
  side?: string;
  type?: string;
  clientOrderId?: string;
  from?: string; // RFC3339
  to?: string; // RFC3339
  limit?: number;
  cursor?: string;
}

/** GET /api/v1/orders — keyset pagination envelope
 * `{data, next_cursor, limit, total}` (pagination.go). Offset paging is
 * not supported — callers collect `nextCursor` and pass it back in. */
export async function fetchOrdersPage(
  filter: OrderPageFilter,
  scopeKey: string = MASTER_ACCOUNT_KEY,
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<{ orders: Order[]; nextCursor: string | null }> {
  const accountId = scopeAccountId(scopeKey);
  return parseOrderList(
    await api.get('/orders', {
      query: {
        symbol: filter.symbol ?? undefined,
        status: filter.status ?? undefined,
        side: filter.side ?? undefined,
        type: filter.type ?? undefined,
        client_order_id: filter.clientOrderId ?? undefined,
        from: filter.from ?? undefined,
        to: filter.to ?? undefined,
        limit: filter.limit ?? 100,
        cursor: filter.cursor ?? undefined,
        account_id: accountId > 0 ? accountId : undefined,
      },
    }),
  );
}

// ---------------------------------------------------------------------------
// Algo orders — GET/DELETE /api/v1/algo-orders (+ pause/resume on /orders/algo)
// ---------------------------------------------------------------------------

export interface AlgoOrder {
  id: string;
  symbol: string;
  algoType: string;
  status: string;
  side: string | null;
  quantity: string | null;
}

export function parseAlgoOrder(v: unknown): AlgoOrder | null {
  if (typeof v !== 'object' || v === null) return null;
  const r = v as Record<string, unknown>;
  const idRaw = r['algo_order_id'] ?? r['id'];
  const id = typeof idRaw === 'string' ? idRaw : typeof idRaw === 'number' ? String(idRaw) : null;
  const symbol = typeof r['symbol'] === 'string' ? r['symbol'] : null;
  if (!id || !symbol) return null;
  const s = (k: string) => (typeof r[k] === 'string' ? r[k] : null);
  return {
    id,
    symbol,
    algoType: s('algo_type') ?? s('type') ?? 'ALGO',
    status: s('status') ?? 'UNKNOWN',
    side: s('side'),
    quantity: s('quantity'),
  };
}

export async function listAlgoOrders(
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<AlgoOrder[]> {
  const res: unknown = await api.get('/algo-orders');
  const rows =
    typeof res === 'object' &&
    res !== null &&
    Array.isArray((res as Record<string, unknown>)['data'])
      ? ((res as Record<string, unknown>)['data'] as unknown[])
      : Array.isArray(res)
        ? res
        : [];
  return rows.map(parseAlgoOrder).filter((o): o is AlgoOrder => o !== null);
}

/** DELETE /orders/algo/{id} — per-algo cancel (AlgoCancel). The bulk
 * route below ignores a per-id body — wiring the row action to it
 * would silently cancel EVERY running algo. */
export function cancelAlgoOrder(api: ApiClient, id: string): Promise<unknown> {
  return api.delete(`/orders/algo/${encodeURIComponent(id)}`);
}

/** DELETE /algo-orders[?symbol=] — cancel ALL running algos (+
 * registered bots) for the account; optional symbol filter. */
export function cancelAllAlgoOrders(api: ApiClient, symbol?: string): Promise<unknown> {
  const q = symbol !== undefined && symbol !== '' ? `?symbol=${encodeURIComponent(symbol)}` : '';
  return api.delete(`/algo-orders${q}`);
}

export function pauseAlgoOrder(api: ApiClient, id: string): Promise<unknown> {
  return api.post(`/orders/algo/${encodeURIComponent(id)}/pause`);
}
export function resumeAlgoOrder(api: ApiClient, id: string): Promise<unknown> {
  return api.post(`/orders/algo/${encodeURIComponent(id)}/resume`);
}

// ---------------------------------------------------------------------------
// OPO/OCO order lists — GET /order-lists (+ /history, /{id})
// ---------------------------------------------------------------------------

export interface OrderListSummary {
  id: string;
  type: string; // OCO | OPO | BRACKET …
  status: string;
  symbol: string | null;
  legs: number | null;
}

export function parseOrderListSummary(v: unknown): OrderListSummary | null {
  if (typeof v !== 'object' || v === null) return null;
  const r = v as Record<string, unknown>;
  const idRaw = r['list_id'] ?? r['id'];
  const id = typeof idRaw === 'string' ? idRaw : typeof idRaw === 'number' ? String(idRaw) : null;
  if (!id) return null;
  const s = (k: string) => (typeof r[k] === 'string' ? r[k] : null);
  const legs = Array.isArray(r['orders'])
    ? (r['orders'] as unknown[]).length
    : typeof r['leg_count'] === 'number'
      ? r['leg_count']
      : null;
  return {
    id,
    type: s('list_type') ?? s('type') ?? 'LIST',
    status: s('status') ?? 'UNKNOWN',
    symbol: s('symbol'),
    legs,
  };
}

function unwrapList(res: unknown): unknown[] {
  if (Array.isArray(res)) return res;
  if (typeof res === 'object' && res !== null) {
    const d = (res as Record<string, unknown>)['data'] ?? (res as Record<string, unknown>)['lists'];
    if (Array.isArray(d)) return d;
  }
  return [];
}

export async function listOrderLists(
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<OrderListSummary[]> {
  return unwrapList(await api.get('/order-lists'))
    .map(parseOrderListSummary)
    .filter((o): o is OrderListSummary => o !== null);
}

export async function listOrderListHistory(
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<OrderListSummary[]> {
  return unwrapList(await api.get('/order-lists/history'))
    .map(parseOrderListSummary)
    .filter((o): o is OrderListSummary => o !== null);
}

// ---------------------------------------------------------------------------
// Dead-man switch — POST /orders/countdown-cancel-all
// countdown_ms 1000–300000 to arm/renew; 0 disables (routes_v1.go:85-86).
// ---------------------------------------------------------------------------

export const COUNTDOWN_MIN_MS = 1_000;
export const COUNTDOWN_MAX_MS = 300_000;

export function armDeadman(countdownMs: number, api: ApiClient = apiClient): Promise<unknown> {
  return api.post('/orders/countdown-cancel-all', { countdown_ms: countdownMs });
}
export function disarmDeadman(api: ApiClient = apiClient): Promise<unknown> {
  return api.post('/orders/countdown-cancel-all', { countdown_ms: 0 });
}

// ---------------------------------------------------------------------------
// Composite order submitters (Phase-16/22) — flat body = symbol/side/
// total_qty plus the per-type params inline (algo.ParseTypedSubmit).
// ---------------------------------------------------------------------------

export type TypedAlgo = 'twap' | 'vwap' | 'scaled' | 'spread';

/** POST /orders/{twap,vwap,scaled,spread} — 202 ack is the algo parent
 * view; the whole body is forwarded as strategy params. */
export function submitTypedAlgo(
  api: ApiClient,
  type: TypedAlgo,
  body: Record<string, unknown>,
): Promise<unknown> {
  return api.post(`/orders/${type}`, body, { idempotent: true });
}

/** POST /orders/basket — cross-shard basket; acks MATCHING with a
 * 128-bit op id (poll /baskets/{op_id} for the terminal result). */
export function submitBasket(
  api: ApiClient,
  legs: { symbol: string; side: string; quantity: string; limit_price?: string }[],
): Promise<unknown> {
  return api.post('/orders/basket', { legs }, { idempotent: true });
}

/** POST /orders/roll — roll a forward/swap contract to a new value
 * date or tenor (Phase-22). */
export function submitRoll(
  api: ApiClient,
  body: {
    contract_id: number;
    new_value_date?: string;
    tenor?: string;
    max_roll_price_bps?: string;
  },
): Promise<unknown> {
  return api.post('/orders/roll', body, { idempotent: true });
}

// ---------------------------------------------------------------------------
// Batch + mass ops (Task 5.3.32 / 5.3.24)
// ---------------------------------------------------------------------------

/** POST /orders/batch — {"orders":[…]} atomic batch submit (≤20). */
export function submitOrderBatch(api: ApiClient, orders: unknown[]): Promise<unknown> {
  return api.post('/orders/batch', { orders }, { idempotent: true });
}

/** DELETE /orders/batch — {"order_ids":[],"client_order_ids":[]} — one
 * unknown/foreign id aborts the whole batch (atomicity). */
export function cancelOrderBatch(
  api: ApiClient,
  body: { order_ids?: (number | string)[]; client_order_ids?: string[] },
): Promise<unknown> {
  return api.delete('/orders/batch', { body });
}

/** DELETE /orders/all — every open order of the account. */
export function massCancelAll(api: ApiClient): Promise<unknown> {
  return api.delete('/orders/all');
}

/** DELETE /orders?symbol=…&side=&type= — scoped mass cancel
 * (symbol required; side/type optional filters). */
export function massCancelScoped(
  api: ApiClient,
  f: { symbol: string; side?: string; type?: string },
): Promise<unknown> {
  const q = new URLSearchParams({ symbol: f.symbol });
  if (f.side !== undefined && f.side !== '') q.set('side', f.side);
  if (f.type !== undefined && f.type !== '') q.set('type', f.type);
  return api.delete(`/orders?${q.toString()}`);
}

// ---------------------------------------------------------------------------
// Order-list detail + cancel (Task 16.3.20/.24)
// ---------------------------------------------------------------------------

export interface OrderListLeg {
  leg_index?: number;
  role?: string;
  state?: string;
  order_id?: number;
  params?: Record<string, unknown>;
}

export interface OrderListDetail {
  list_id: number;
  contingency_type?: string;
  state?: string;
  working_order_id?: number;
  fail_reason?: string;
  closed_at?: string;
  legs?: OrderListLeg[];
}

/** POST /order-lists — OPO (working BUY + 1 pending SELL) or OPOCO
 * (working BUY + 2 pending SELLs as an OCO pair). Pending quantity is
 * recomputed server-side from the working fill's net proceeds — the
 * client intentionally sends none (§24 #287). */
export function submitOrderList(
  api: ApiClient,
  body: {
    contingency_type: 'OPO' | 'OPOCO';
    symbol: string;
    client_order_id?: string;
    working: Record<string, unknown>;
    pending: Record<string, unknown>[];
  },
): Promise<unknown> {
  return api.post('/order-lists', body, { idempotent: true });
}

export function orderListDetail(api: Pick<ApiClient, 'get'>, id: string): Promise<OrderListDetail> {
  return api.get<OrderListDetail>(`/order-lists/${encodeURIComponent(id)}`);
}

/** DELETE /order-lists/{id} — cancels the list and its working leg. */
export function cancelOrderList(api: ApiClient, id: string): Promise<unknown> {
  return api.delete(`/order-lists/${encodeURIComponent(id)}`);
}

// ---------------------------------------------------------------------------
// Amendment ledger — GET /orders/{id}/amendments
// (wire rows are PascalCase — orders.AuditEntry carries no json tags)
// ---------------------------------------------------------------------------

export interface Amendment {
  orderId?: number;
  operation?: string;
  fieldName?: string;
  oldValue?: string;
  newValue?: string;
  modifiedBy?: string;
  requestId?: string;
  modifiedAt?: string;
}

export async function orderAmendments(
  api: Pick<ApiClient, 'get'>,
  id: string,
): Promise<Amendment[]> {
  const res = await api.get<{ order_id?: number; amendments?: Record<string, unknown>[] }>(
    `/orders/${encodeURIComponent(id)}/amendments`,
  );
  return (res.amendments ?? []).map((r) => ({
    orderId: typeof r['OrderID'] === 'number' ? r['OrderID'] : undefined,
    operation: typeof r['Operation'] === 'string' ? r['Operation'] : undefined,
    fieldName: typeof r['FieldName'] === 'string' ? r['FieldName'] : undefined,
    oldValue: typeof r['OldValue'] === 'string' ? r['OldValue'] : undefined,
    newValue: typeof r['NewValue'] === 'string' ? r['NewValue'] : undefined,
    modifiedBy: typeof r['ModifiedBy'] === 'string' ? r['ModifiedBy'] : undefined,
    requestId: typeof r['RequestID'] === 'string' ? r['RequestID'] : undefined,
    modifiedAt: typeof r['ModifiedAt'] === 'string' ? r['ModifiedAt'] : undefined,
  }));
}
