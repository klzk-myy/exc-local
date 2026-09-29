/**
 * Typed endpoint wrappers over `ApiClient` for the trading surfaces.
 * Every function narrows the response through `wire.ts` — a malformed
 * payload throws `INVALID_REQUEST`-shaped `ApiError` (fail-closed: never
 * render unparsed money).
 */
import type { ApiClient } from '@/lib/api';
import { ApiError } from '@/lib/api';

import type { KlineInterval } from './channels';
import {
  parseBalancesResponse,
  parseBookSnapshot,
  parseInstrumentsResponse,
  parseKlinesResponse,
  parseOrderAck,
  parseOrderPreview,
  parsePositionsResponse,
  type BalanceRow,
  type BookSnapshotWire,
  type Instrument,
  type KlinesResponse,
  type OrderAck,
  type OrderPreview,
  type PositionRow,
  type SubmitOrderRequest,
} from './wire';

function malformed(what: string): ApiError {
  return new ApiError({
    type: 'error',
    error: 'INVALID_REQUEST',
    message: `malformed ${what} payload`,
    status: 0,
  });
}

/** GET /api/v1/instruments — public reference data (1min cache upstream). */
export async function fetchInstruments(api: ApiClient): Promise<Instrument[]> {
  const raw: unknown = await api.get('/instruments');
  const list = parseInstrumentsResponse(raw);
  if (list === null) throw malformed('instruments');
  return list;
}

/** GET /api/v1/book/{symbol}?depth=N — persisted L2 snapshot (§10.3). */
export async function fetchBook(
  api: ApiClient,
  symbol: string,
  depth = 20,
): Promise<BookSnapshotWire> {
  const raw: unknown = await api.get(`/book/${encodeURIComponent(symbol)}`, {
    query: { depth },
  });
  const snap = parseBookSnapshot(raw);
  if (!snap) throw malformed('book snapshot');
  return snap;
}

export interface KlinesQuery {
  symbol: string;
  interval: KlineInterval;
  limit?: number;
  /** Epoch ms bounds on open_time. */
  fromMs?: number;
  toMs?: number;
}

/** GET /api/v1/klines/{symbol}?interval=…&limit=… — pre-materialized
 * fx_klines only (spec §10.3). `to` pages backwards via next_cursor. */
export async function fetchKlines(api: ApiClient, q: KlinesQuery): Promise<KlinesResponse> {
  const raw: unknown = await api.get(`/klines/${encodeURIComponent(q.symbol)}`, {
    query: { interval: q.interval, limit: q.limit ?? 500, from: q.fromMs, to: q.toMs },
  });
  const res = parseKlinesResponse(raw);
  if (!res) throw malformed('klines');
  return res;
}

/** GET /api/v1/positions — open positions + unrealized P&L. */
export async function fetchPositions(api: ApiClient): Promise<PositionRow[]> {
  const raw: unknown = await api.get('/positions');
  const rows = parsePositionsResponse(raw);
  if (!rows) throw malformed('positions');
  return rows;
}

/** GET /api/v1/account/balances — all currency balances. */
export async function fetchBalances(api: ApiClient): Promise<BalanceRow[]> {
  const raw: unknown = await api.get('/account/balances');
  const rows = parseBalancesResponse(raw);
  if (!rows) throw malformed('balances');
  return rows;
}

/** POST /api/v1/orders — idempotent submission (§8.1): client_order_id
 * dedup + Idempotency-Key header via `idempotent: true` (§8.8). */
export async function submitOrder(api: ApiClient, req: SubmitOrderRequest): Promise<OrderAck> {
  const raw: unknown = await api.post('/orders', req, { idempotent: true });
  const ack = parseOrderAck(raw);
  if (!ack) throw malformed('order ack');
  return ack;
}

/** POST /api/v1/orders/test — dry-run preview (§22.1: validates filters,
 * margin and estimated cost WITHOUT reserving funds; binding=false). Not
 * idempotent-keyed: it moves no money. */
export async function previewOrder(api: ApiClient, req: SubmitOrderRequest): Promise<OrderPreview> {
  const raw: unknown = await api.post('/orders/test', req);
  const preview = parseOrderPreview(raw);
  if (!preview) throw malformed('order preview');
  return preview;
}
