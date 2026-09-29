/**
 * Typed REST calls for the trading-ux feature cluster — thin wrappers over
 * `ApiClient` returning already-parsed domain types. All functions take an
 * optional client so tests inject a mock-transport ApiClient.
 *
 * Money-moving POSTs carry `idempotent: true` (spec §8.8); an explicit
 * `idempotencyKey` is forwarded for true client retries of identical
 * payloads.
 */
import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';

import {
  parseBalanceList,
  parseBookSnapshot,
  parseInstrumentList,
  parseKlineList,
  parseOrder,
  parseOrderList,
  parsePositionList,
  parseSubAccountList,
  type Balance,
  type BookSnapshot,
  type Instrument,
  type Kline,
  type Order,
  type Position,
  type SubAccount,
} from './types';

export type Api = Pick<ApiClient, 'get' | 'post' | 'put' | 'delete'>;

/** Account scope query params — the gateway rejects foreign account_ids
 * with FORBIDDEN (fail-closed §2.7); sub-account trading surfaces that
 * honestly rather than fabricating a view. */
export interface AccountScope {
  /** Selected sub-account id; 0/undefined = master account. */
  accountId?: number;
}

function scopeQuery(scope?: AccountScope): Record<string, number> | undefined {
  return scope?.accountId !== undefined && scope.accountId > 0
    ? { account_id: scope.accountId }
    : undefined;
}

// -- reference data -----------------------------------------------------------

export async function fetchInstruments(api: Api = apiClient): Promise<Instrument[]> {
  return parseInstrumentList(await api.get('/instruments'));
}

export async function fetchBookSnapshot(
  symbol: string,
  depth = 20,
  api: Api = apiClient,
): Promise<BookSnapshot | null> {
  return parseBookSnapshot(
    await api.get(`/book/${encodeURIComponent(symbol)}`, { query: { depth } }),
  );
}

export async function fetchKlines(
  symbol: string,
  interval = '15m',
  limit = 120,
  api: Api = apiClient,
): Promise<Kline[]> {
  return parseKlineList(
    await api.get(`/klines/${encodeURIComponent(symbol)}`, {
      query: { interval, limit },
    }),
  );
}

// -- account reads --------------------------------------------------------------

export async function fetchBalances(
  scope?: AccountScope,
  api: Api = apiClient,
): Promise<Balance[]> {
  return parseBalanceList(await api.get('/account/balances', { query: scopeQuery(scope) }));
}

export async function fetchPositions(
  scope?: AccountScope,
  api: Api = apiClient,
): Promise<Position[]> {
  return parsePositionList(await api.get('/positions', { query: scopeQuery(scope) }));
}

export async function fetchSubAccounts(api: Api = apiClient): Promise<SubAccount[]> {
  return parseSubAccountList(await api.get('/account/sub-accounts'));
}

export async function fetchOrders(
  params: { symbol?: string; status?: string; limit?: number },
  scope?: AccountScope,
  api: Api = apiClient,
): Promise<{ orders: Order[]; nextCursor: string | null }> {
  return parseOrderList(
    await api.get('/orders', {
      query: {
        symbol: params.symbol,
        status: params.status,
        limit: params.limit ?? 100,
        ...scopeQuery(scope),
      },
    }),
  );
}

// -- order mutations --------------------------------------------------------------

/** POST /api/v1/orders body — keys per orders.ParseSubmit. */
export interface SubmitOrderBody {
  symbol: string;
  side: 'BUY' | 'SELL';
  type: string;
  time_in_force?: string;
  client_order_id?: string;
  quantity?: string;
  quote_quantity?: string;
  price?: string;
  stop_price?: string;
  iceberg_visible_qty?: string;
  gtd_expiry?: string; // RFC3339
  post_only?: boolean;
  reduce_only?: boolean;
  stp_mode?: string;
  /** Phase-16 TRAILING_STOP params ride algo_params (migration 038). */
  algo_params?: Record<string, unknown>;
}

export interface OrderAck {
  order_id?: number;
  client_order_id?: string;
  status?: string;
  transact_time?: string;
}

export async function submitOrder(body: SubmitOrderBody, api: Api = apiClient): Promise<OrderAck> {
  return api.post<OrderAck>('/orders', body, { idempotent: true });
}

/** Bracket/OTO — POST /api/v1/orders/bracket (Phase-16 Task 16.3.14):
 * parent entry + OCO children (stop-loss + take-profit). */
export interface BracketOrderBody {
  symbol: string;
  side: 'BUY' | 'SELL';
  quantity: string;
  entry_price?: string; // absent ⇒ parent is MARKET
  stop_price: string; // SL child trigger
  take_profit_price: string; // TP child limit
  time_in_force?: string;
  client_order_id?: string;
}

export async function submitBracketOrder(
  body: BracketOrderBody,
  api: Api = apiClient,
): Promise<OrderAck> {
  return api.post<OrderAck>('/orders/bracket', body, { idempotent: true });
}

/** OCO — POST /api/v1/orders/oco (Phase-16 Task 16.3.14): a limit leg +
 * a stop leg; first fill cancels the sibling. */
export interface OcoOrderBody {
  symbol: string;
  side: 'BUY' | 'SELL';
  quantity: string;
  price: string; // limit leg
  stop_price: string; // stop leg trigger
  stop_limit_price?: string; // absent ⇒ stop leg fires market
  time_in_force?: string;
  client_order_id?: string;
}

export async function submitOcoOrder(body: OcoOrderBody, api: Api = apiClient): Promise<OrderAck> {
  return api.post<OrderAck>('/orders/oco', body, { idempotent: true });
}

/** Algo orders — POST /api/v1/orders/algo (Phase-16): TRAILING_STOP and
 * strategy orders carry their params in `algo_params` (migration 038). */
export interface AlgoOrderBody {
  symbol: string;
  side: 'BUY' | 'SELL';
  algo_type: string; // TRAILING_STOP | TWAP | VWAP | …
  quantity: string;
  algo_params: Record<string, string>;
  client_order_id?: string;
}

export async function submitAlgoOrder(
  body: AlgoOrderBody,
  api: Api = apiClient,
): Promise<OrderAck> {
  return api.post<OrderAck>('/orders/algo', body, { idempotent: true });
}

/** POST /api/v1/orders/test — dry-run preview (Phase-05 Task 5.3.39):
 * margin/commission/spread estimate + active filters; `binding` is
 * always false — never treated as an execution guarantee. */
export interface OrderPreview {
  estimatedBaseQty: string;
  estimatedQuoteQty: string;
  margin: string;
  commissionEstimate: string;
  spreadEstimate: string;
  riskLevel: string;
  warnings: string[];
  binding: boolean;
}

export async function previewOrder(
  body: SubmitOrderBody,
  api: Api = apiClient,
): Promise<OrderPreview | null> {
  const raw: unknown = await api.post('/orders/test', body);
  if (typeof raw !== 'object' || raw === null) return null;
  const r = raw as Record<string, unknown>;
  const s = (k: string): string => {
    const v = r[k];
    return typeof v === 'string' ? v : typeof v === 'number' ? String(v) : '0';
  };
  return {
    estimatedBaseQty: s('estimated_base_qty'),
    estimatedQuoteQty: s('estimated_quote_qty'),
    margin: s('margin'),
    commissionEstimate: s('commission_estimate'),
    spreadEstimate: s('spread_estimate'),
    riskLevel: typeof r['risk_level'] === 'string' ? r['risk_level'] : 'LOW',
    warnings: Array.isArray(r['warnings'])
      ? r['warnings'].filter((w): w is string => typeof w === 'string')
      : [],
    binding: r['binding'] === true,
  };
}

export async function cancelOrder(orderId: string, api: Api = apiClient): Promise<void> {
  await api.delete(`/orders/${encodeURIComponent(orderId)}`);
}

/** PUT /orders/{id}/amend/keep-priority — quantity-DOWN only; preserves
 * queue position (§6.9, Phase-05 Task 5.3.37). order_seq is the mandatory
 * STALE_MODIFY fence. */
export async function amendKeepPriority(
  orderId: string,
  quantity: string,
  orderSeq: number,
  api: Api = apiClient,
): Promise<void> {
  await api.put(`/orders/${encodeURIComponent(orderId)}/amend/keep-priority`, {
    order_seq: orderSeq,
    quantity,
  });
}

/** POST /orders/{id}/cancel-replace — atomic amend; a PRICE change loses
 * queue position by definition (§6.9). */
export async function cancelReplace(
  orderId: string,
  fields: {
    order_seq: number;
    price?: string;
    quantity?: string;
    stop_price?: string;
    mode?: 'STOP_ON_FAILURE' | 'ALLOW_FAILURE';
  },
  api: Api = apiClient,
): Promise<void> {
  await api.post(`/orders/${encodeURIComponent(orderId)}/cancel-replace`, fields, {
    idempotent: true,
  });
}

/** POST /positions/close-all — optional symbol/side filters + X-2FA-Token
 * (Phase-05 Task 5.3.36). */
export async function closeAllPositions(
  opts: { symbol?: string; side?: string; twoFactorToken?: string } = {},
  api: Api = apiClient,
): Promise<void> {
  const headers: Record<string, string> = {};
  if (opts.twoFactorToken) headers['X-2FA-Token'] = opts.twoFactorToken;
  await api.post(
    '/positions/close-all',
    { symbol: opts.symbol, side: opts.side },
    { idempotent: true, headers },
  );
}

/** Admin fee-tier assignment — PUT /admin/accounts/{id}/product-profile
 * (Phase-14 Task 14.3.7, Compliance Officer role). Registered-but-stubbed
 * endpoints surface 501 NOT_IMPLEMENTED to the caller honestly. */
export async function assignProductProfile(
  accountId: string,
  body: { category?: string; fee_tier_id?: string; product_profile?: string },
  api: Api = apiClient,
): Promise<void> {
  await api.put(`/admin/accounts/${encodeURIComponent(accountId)}/product-profile`, body);
}

export async function fetchOrderById(orderId: string, api: Api = apiClient): Promise<Order | null> {
  return parseOrder(await api.get(`/orders/${encodeURIComponent(orderId)}`));
}
