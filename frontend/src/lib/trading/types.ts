/**
 * Trading domain wire types + parsers (Phase-10 Wave-2 trading-ux).
 *
 * Every inbound payload arrives as `unknown` and is narrowed here via
 * structural guards — mirrors the lib/ws adapter-boundary convention.
 * Money/price/qty fields parse to `Dec`; malformed payloads narrow to
 * `null` rather than partially-fabricated objects (fail-closed §2.7).
 *
 * Wire sources:
 *   - REST: services/internal/api (Order.View, funding.PositionRow /
 *     BalanceRow, marketapi.BookSnapshot / InstrumentDoc / Kline,
 *     accounts.SubAccount)
 *   - WS:   services/internal/marketdata (bbo@, depth@/book@ depthUpdate,
 *     private:positions adl_indicator, private:orders OrderEvent)
 */
import { Dec, tryDec } from '@/lib/decimal/decimal';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}
function str(v: unknown): string | undefined {
  return typeof v === 'string' && v.length > 0 ? v : undefined;
}
function num(v: unknown): number | undefined {
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (typeof v === 'string' && v.trim() !== '') {
    const n = Number(v);
    if (Number.isFinite(n)) return n;
  }
  return undefined;
}

// ---------------------------------------------------------------------------
// Reference data — GET /api/v1/instruments (marketapi.InstrumentDoc)
// ---------------------------------------------------------------------------

export interface Instrument {
  symbol: string;
  base: string;
  quote: string;
  instrumentType: string; // SPOT|FORWARD|SWAP|NDF|OPTION
  status: string; // instrument_status_enum (§7.1)
  tickSize: Dec;
  lotSize: Dec;
  minQty: Dec;
  maxQty: Dec;
  minNotional: Dec;
  maxLeverage: number;
}

export function parseInstrument(v: unknown): Instrument | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  const tick = tryDec(v['tick_size']);
  if (!symbol || !tick) return null;
  return {
    symbol,
    base: str(v['base_currency']) ?? symbol.split('/')[0] ?? '',
    quote: str(v['quote_currency']) ?? symbol.split('/')[1] ?? '',
    instrumentType: str(v['instrument_type']) ?? 'SPOT',
    status: str(v['status']) ?? 'UNKNOWN',
    tickSize: tick,
    lotSize: tryDec(v['lot_size']) ?? Dec.ZERO,
    minQty: tryDec(v['min_order_qty']) ?? Dec.ZERO,
    maxQty: tryDec(v['max_order_qty']) ?? Dec.ZERO,
    minNotional: tryDec(v['min_notional']) ?? Dec.ZERO,
    maxLeverage: num(v['max_leverage']) ?? 0,
  };
}

/** `{data: [...]}` list envelope (handlers_market.go). */
export function parseInstrumentList(v: unknown): Instrument[] {
  const rows = isRecord(v) ? v['data'] : undefined;
  if (!Array.isArray(rows)) return [];
  return rows.map(parseInstrument).filter((i): i is Instrument => i !== null);
}

// ---------------------------------------------------------------------------
// Balances — GET /api/v1/account/balances (funding.BalanceRow)
// ---------------------------------------------------------------------------

export interface Balance {
  currency: string;
  available: Dec;
  locked: Dec;
  total: Dec;
}

export function parseBalance(v: unknown): Balance | null {
  if (!isRecord(v)) return null;
  const currency = str(v['currency']);
  if (!currency) return null;
  return {
    currency,
    available: tryDec(v['available']) ?? Dec.ZERO,
    locked: tryDec(v['locked']) ?? Dec.ZERO,
    total: tryDec(v['total']) ?? Dec.ZERO,
  };
}

export function parseBalanceList(v: unknown): Balance[] {
  const rows = isRecord(v) ? v['balances'] : undefined;
  if (!Array.isArray(rows)) return [];
  return rows.map(parseBalance).filter((b): b is Balance => b !== null);
}

// ---------------------------------------------------------------------------
// Positions — GET /api/v1/positions (funding.PositionRow) and the
// private:positions WS payload (adds `adl_indicator`, Phase-19 Task 19.3.19)
// ---------------------------------------------------------------------------

export type PositionSide = 'LONG' | 'SHORT';

export interface Position {
  id: string;
  symbol: string;
  side: PositionSide;
  quantity: Dec;
  entryPrice: Dec;
  markPrice: Dec | undefined;
  unrealizedPnl: Dec;
  realizedPnl: Dec;
  liquidationPrice: Dec | undefined;
  marginUsed: Dec;
  /** 1..5 ADL rank (1 = lowest deleveraging risk), or undefined when the
   * Phase-19 ADL publisher hasn't emitted — never fabricated. */
  adlIndicator: number | undefined;
}

export function parsePosition(v: unknown): Position | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  const sideRaw = str(v['side'])?.toUpperCase();
  const qty = tryDec(v['quantity']);
  if (!symbol || (sideRaw !== 'LONG' && sideRaw !== 'SHORT') || !qty) return null;
  const adl = num(v['adl_indicator']);
  const idNum = num(v['position_id'] ?? v['id']);
  return {
    id: str(v['position_id']) ?? str(v['id']) ?? (idNum !== undefined ? String(idNum) : symbol),
    symbol,
    side: sideRaw,
    quantity: qty.abs(),
    entryPrice: tryDec(v['entry_price']) ?? Dec.ZERO,
    markPrice: tryDec(v['mark_price']),
    unrealizedPnl: tryDec(v['unrealized_pnl']) ?? Dec.ZERO,
    realizedPnl: tryDec(v['realized_pnl']) ?? Dec.ZERO,
    liquidationPrice: tryDec(v['liquidation_price']),
    marginUsed: tryDec(v['margin_used']) ?? Dec.ZERO,
    adlIndicator: adl !== undefined && adl >= 1 && adl <= 5 ? Math.round(adl) : undefined,
  };
}

export function parsePositionList(v: unknown): Position[] {
  const rows = isRecord(v) ? v['positions'] : undefined;
  if (!Array.isArray(rows)) return [];
  return rows.map(parsePosition).filter((p): p is Position => p !== null);
}

// ---------------------------------------------------------------------------
// Orders — GET /api/v1/orders (orders.Order.View) / private:orders events
// ---------------------------------------------------------------------------

export interface Order {
  id: string;
  clientOrderId: string;
  symbol: string;
  side: 'BUY' | 'SELL';
  type: string;
  timeInForce: string;
  quantity: Dec;
  filledQty: Dec;
  price: Dec | undefined;
  stopPrice: Dec | undefined;
  status: string;
  orderSeq: number;
  postOnly: boolean;
  reduceOnly: boolean;
  createdAt: string;
  /** Average fill price — set on FILLED/PARTIALLY_FILLED rows. */
  avgFillPrice: Dec | undefined;
}

export function parseOrder(v: unknown): Order | null {
  if (!isRecord(v)) return null;
  const id =
    str(v['order_id']) ?? (num(v['order_id']) !== undefined ? String(v['order_id']) : null);
  const symbol = str(v['symbol']);
  const sideRaw = str(v['side'])?.toUpperCase();
  if (!id || !symbol || (sideRaw !== 'BUY' && sideRaw !== 'SELL')) return null;
  return {
    id,
    clientOrderId: str(v['client_order_id']) ?? '',
    symbol,
    side: sideRaw,
    type: str(v['type']) ?? 'LIMIT',
    timeInForce: str(v['time_in_force']) ?? 'GTC',
    quantity: tryDec(v['quantity']) ?? Dec.ZERO,
    filledQty: tryDec(v['filled_qty']) ?? Dec.ZERO,
    price: tryDec(v['price']),
    stopPrice: tryDec(v['stop_price']),
    status: str(v['status']) ?? 'UNKNOWN',
    orderSeq: num(v['order_seq']) ?? 0,
    postOnly: v['post_only'] === true,
    reduceOnly: v['reduce_only'] === true,
    createdAt: str(v['created_at']) ?? '',
    avgFillPrice: tryDec(v['avg_fill_price']),
  };
}

/** `{data:[…], next_cursor, limit, total}` list envelope (pagination.go). */
export function parseOrderList(v: unknown): { orders: Order[]; nextCursor: string | null } {
  const rows = isRecord(v) ? v['data'] : undefined;
  const orders = Array.isArray(rows)
    ? rows.map(parseOrder).filter((o): o is Order => o !== null)
    : [];
  const nextCursor = isRecord(v) ? (str(v['next_cursor']) ?? null) : null;
  return { orders, nextCursor };
}

export const OPEN_ORDER_STATUSES = new Set(['PENDING', 'RESERVED', 'ACTIVE', 'PARTIALLY_FILLED']);

export function isOpenOrder(o: Order): boolean {
  return OPEN_ORDER_STATUSES.has(o.status);
}

// ---------------------------------------------------------------------------
// Sub-accounts — GET /api/v1/account/sub-accounts (accounts.SubAccount)
// ---------------------------------------------------------------------------

export interface SubAccount {
  id: number;
  masterId: number;
  accountType: string;
  kycTier: string;
  status: string;
  tradingEnabled: boolean;
  balances: Balance[];
}

export function parseSubAccount(v: unknown): SubAccount | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  const rows = Array.isArray(v['balances']) ? v['balances'] : [];
  return {
    id,
    masterId: num(v['master_account_id']) ?? 0,
    accountType: str(v['account_type']) ?? 'SUB',
    kycTier: str(v['kyc_tier']) ?? '',
    status: str(v['status']) ?? 'UNKNOWN',
    tradingEnabled: v['trading_enabled'] === true,
    balances: rows.map(parseBalance).filter((b): b is Balance => b !== null),
  };
}

export function parseSubAccountList(v: unknown): SubAccount[] {
  const rows = isRecord(v) ? v['data'] : undefined;
  if (!Array.isArray(rows)) return [];
  return rows.map(parseSubAccount).filter((s): s is SubAccount => s !== null);
}

// ---------------------------------------------------------------------------
// Book snapshot — GET /api/v1/book/{symbol} (marketapi.BookSnapshot):
// {symbol, seq, depth, bids/asks: [{price, quantity}]}
// ---------------------------------------------------------------------------

export interface BookLevel {
  price: Dec;
  qty: Dec;
}

export interface BookSnapshot {
  symbol: string;
  seq: number;
  bids: BookLevel[];
  asks: BookLevel[];
  updatedAtMs: number;
}

function parseLevels(v: unknown): BookLevel[] {
  if (!Array.isArray(v)) return [];
  const out: BookLevel[] = [];
  for (const lvl of v) {
    // REST snapshot rows are objects; WS depthUpdate rows are triples.
    if (isRecord(lvl)) {
      const price = tryDec(lvl['price']);
      const qty = tryDec(lvl['quantity']);
      if (price && qty) out.push({ price, qty });
    } else if (Array.isArray(lvl)) {
      const price = tryDec(lvl[0]);
      const qty = tryDec(lvl[1]);
      if (price && qty) out.push({ price, qty });
    }
  }
  return out;
}

export function parseBookSnapshot(v: unknown): BookSnapshot | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  if (!symbol) return null;
  return {
    symbol,
    seq: num(v['seq']) ?? 0,
    bids: parseLevels(v['bids']),
    asks: parseLevels(v['asks']),
    updatedAtMs: num(v['updated_at_ms']) ?? 0,
  };
}

/** depthUpdate frame payload (marketdata/l2.go) → BookSnapshot. */
export function parseDepthUpdate(v: unknown): BookSnapshot | null {
  return parseBookSnapshot(v); // same field names on the wire
}

// ---------------------------------------------------------------------------
// BBO — bbo@{symbol} (marketdata/bbo.go): nullable sides on one-sided books
// ---------------------------------------------------------------------------

export interface Bbo {
  symbol: string;
  bid: Dec | undefined;
  bidQty: Dec | undefined;
  ask: Dec | undefined;
  askQty: Dec | undefined;
  tsMs: number;
}

export function parseBbo(v: unknown): Bbo | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  if (!symbol) return null;
  return {
    symbol,
    bid: tryDec(v['bid']),
    bidQty: tryDec(v['bid_qty']),
    ask: tryDec(v['ask']),
    askQty: tryDec(v['ask_qty']),
    tsMs: num(v['ts_ms']) ?? 0,
  };
}

// ---------------------------------------------------------------------------
// private:positions WS event — Phase-19 publishes position upserts carrying
// `adl_indicator`; the envelope `data` may be one position or an array.
// ---------------------------------------------------------------------------

export function parsePrivatePositions(v: unknown): Position[] {
  const rows = Array.isArray(v)
    ? v
    : isRecord(v) && Array.isArray(v['positions'])
      ? v['positions']
      : [v];
  return rows.map(parsePosition).filter((p): p is Position => p !== null);
}
