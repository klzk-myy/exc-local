/**
 * Performance feature wire seams — parses the live order-history rows
 * (orders.Order.View()) and tolerantly probes the registered-but-stub
 * reporting endpoints. Stub endpoints return RFC 7807 errors (501
 * NOT_IMPLEMENTED / SERVICE_DEGRADED); callers catch + mark derived.
 */
import type { ApiClient } from '@/lib/api';

import type { OrderRow } from './derive';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const decStr = (v: unknown): string | undefined => {
  if (typeof v === 'string') return v;
  if (typeof v === 'number' && Number.isFinite(v)) return String(v);
  return undefined;
};

function parseOrderRow(v: unknown): OrderRow | null {
  if (!isRecord(v)) return null;
  const status = str(v['status']);
  const side = str(v['side']);
  if (status === undefined || side === undefined) return null;
  return {
    orderId: str(v['order_id']) ?? String(num(v['order_id']) ?? ''),
    side,
    status,
    filledQty: decStr(v['filled_qty']) ?? '0',
    avgFillPrice: decStr(v['avg_fill_price']),
    symbol: str(v['symbol']),
    instrumentId: num(v['instrument_id']) ?? 0,
    createdAt: str(v['created_at']) ?? '',
  };
}

export interface OrdersPage {
  data: OrderRow[];
  nextCursor: string;
  total: number;
}

/** GET /api/v1/orders — the live cursor-paged order history. Pulls up
 * to `pages` pages for the derivation window. */
export async function fetchOrderHistory(
  api: ApiClient,
  opts: { limit?: number; pages?: number } = {},
): Promise<OrdersPage> {
  const limit = opts.limit ?? 200;
  const maxPages = opts.pages ?? 3;
  const all: OrderRow[] = [];
  let cursor: string | undefined;
  let total = 0;
  for (let i = 0; i < maxPages; i++) {
    const raw: unknown = await api.get('/orders', {
      query: { limit, cursor, status: 'FILLED' },
    });
    if (!isRecord(raw) || !Array.isArray(raw['data'])) break;
    total = num(raw['total']) ?? 0;
    for (const it of raw['data']) {
      const row = parseOrderRow(it);
      if (row !== null) all.push(row);
    }
    const next = str(raw['next_cursor']);
    if (next === undefined || next === '') break;
    cursor = next;
  }
  return { data: all, nextCursor: cursor ?? '', total };
}
