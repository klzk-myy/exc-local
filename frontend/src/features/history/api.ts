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

export function cancelAlgoOrder(api: ApiClient, id: string): Promise<unknown> {
  return api.delete('/algo-orders', { body: { algo_order_id: id } });
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
