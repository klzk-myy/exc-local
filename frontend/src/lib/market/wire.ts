/**
 * Market/account wire types — ADAPTER BOUNDARY.
 *
 * Everything crossing the REST/WS edge arrives as `unknown` (or loosely
 * typed frame `data`) and is narrowed here by structural guards. Money
 * and prices are decimal strings end-to-end (spec §5.3, services render
 * shopspring decimals as JSON strings) — components convert via
 * `lib/market/decimal` for math and `lib/market/format` for display.
 *
 * Wire shapes mirrored from:
 *   services/internal/marketapi/types.go   (REST: book/klines/instruments)
 *   services/internal/marketdata/l2.go     (depthUpdate / book@ frames)
 *   services/internal/marketdata/ohlcv/emit.go (kline@ frames)
 *   services/internal/marketdata/trades.go     (trades@ frames)
 *   services/internal/marketdata/private.go    (private:orders)
 *   services/internal/ledger/posting.go        (BALANCE_CHANGED events)
 *   services/internal/orders/service.go        (order ack/preview)
 *   services/internal/funding/store.go         (positions/balances rows)
 */

// ---------------------------------------------------------------------------
// Shared scalar helpers
// ---------------------------------------------------------------------------

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}
function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}
function bool(v: unknown): boolean | undefined {
  return typeof v === 'boolean' ? v : undefined;
}
/** Decimal fields arrive as strings; accept finite numbers as a fallback
 * for producers that still emit them, but never fabricate one. */
function decStr(v: unknown): string | undefined {
  if (typeof v === 'string') return v;
  if (typeof v === 'number' && Number.isFinite(v)) return String(v);
  return undefined;
}

// ---------------------------------------------------------------------------
// Order book (L2)
// ---------------------------------------------------------------------------

/** One aggregated L2 price level. `count` (order count) is present on WS
 * depth frames; REST snapshot levels omit it. */
export interface BookLevel {
  price: string;
  qty: string;
  count?: number;
}

/** GET /api/v1/book/{symbol}?depth=N response (marketapi.BookSnapshot). */
export interface BookSnapshotWire {
  symbol: string;
  seq: number;
  depth: number;
  bids: BookLevel[];
  asks: BookLevel[];
  updatedAtMs: number;
}

function parseLevelPair(v: unknown): BookLevel | null {
  // WS depth frames render levels as [price, qty, count?] string triples.
  if (Array.isArray(v)) {
    const price = decStr(v[0]);
    const qty = decStr(v[1]);
    if (price === undefined || qty === undefined) return null;
    const rawCount: unknown = (v as unknown[])[2];
    const count =
      num(rawCount) ?? (typeof rawCount === 'string' ? num(Number(rawCount)) : undefined);
    return { price, qty, count };
  }
  // REST renders {price, quantity} objects.
  if (isRecord(v)) {
    const price = decStr(v['price']);
    const qty = decStr(v['quantity'] ?? v['qty']);
    if (price === undefined || qty === undefined) return null;
    return { price, qty, count: num(v['count']) };
  }
  return null;
}

function parseLevels(v: unknown): BookLevel[] {
  if (!Array.isArray(v)) return [];
  const out: BookLevel[] = [];
  for (const item of v) {
    const l = parseLevelPair(item);
    if (l) out.push(l); // malformed level dropped — never fabricate one
  }
  return out;
}

export function parseBookSnapshot(v: unknown): BookSnapshotWire | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  if (!symbol) return null;
  return {
    symbol,
    seq: num(v['seq']) ?? 0,
    depth: num(v['depth']) ?? 0,
    bids: parseLevels(v['bids']),
    asks: parseLevels(v['asks']),
    updatedAtMs: num(v['updated_at_ms']) ?? 0,
  };
}

/** WS event payload on `depth@{symbol}…` / `book@{symbol}` — the
 * conflated top-N book state (§10.9 envelope fields included). Replace
 * semantics: each frame carries the full aggregated level set. */
export interface DepthUpdate {
  symbol: string;
  bids: BookLevel[];
  asks: BookLevel[];
  firstSeq: number;
  lastSeq: number;
  prevLastSeq: number;
  seq: number;
  engineSeq: number;
  coalesced: number;
  crc32: number | undefined;
  tsMs: number;
  isSnapshot: boolean;
}

export function parseDepthUpdate(v: unknown): DepthUpdate | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  if (!symbol) return null;
  return {
    symbol,
    bids: parseLevels(v['bids']),
    asks: parseLevels(v['asks']),
    firstSeq: num(v['first_seq']) ?? 0,
    lastSeq: num(v['last_seq']) ?? num(v['seq']) ?? 0,
    prevLastSeq: num(v['prev_last_seq']) ?? 0,
    seq: num(v['seq']) ?? 0,
    engineSeq: num(v['engine_seq']) ?? 0,
    coalesced: num(v['coalesced']) ?? 0,
    crc32: num(v['crc32']),
    tsMs: num(v['ts_ms']) ?? 0,
    isSnapshot: bool(v['is_snapshot']) ?? false,
  };
}

// ---------------------------------------------------------------------------
// Klines / OHLCV
// ---------------------------------------------------------------------------

/** marketapi.Kline / ohlcv.KlineData — one materialized candle. */
export interface Kline {
  /** Present on WS kline@ frames; REST rows live inside the symbol-scoped
   * envelope and omit it. */
  symbol?: string;
  openTimeMs: number;
  /** Exclusive bucket end — present on WS frames, absent on REST rows. */
  closeTimeMs?: number;
  open: string;
  high: string;
  low: string;
  close: string;
  volume: string;
  quoteVolume?: string;
  tradeCount: number;
  closed: boolean;
}

export function parseKline(v: unknown): Kline | null {
  if (!isRecord(v)) return null;
  const openTimeMs = num(v['open_time_ms']);
  const open = decStr(v['open']);
  const high = decStr(v['high']);
  const low = decStr(v['low']);
  const close = decStr(v['close']);
  if (openTimeMs === undefined || !open || !high || !low || !close) return null;
  return {
    symbol: str(v['symbol']),
    openTimeMs,
    closeTimeMs: num(v['close_time_ms']),
    open,
    high,
    low,
    close,
    volume: decStr(v['volume']) ?? '0',
    quoteVolume: decStr(v['quote_volume']),
    tradeCount: num(v['trade_count']) ?? 0,
    closed: bool(v['closed']) ?? true,
  };
}

/** GET /api/v1/klines/{symbol}?interval=… response envelope. */
export interface KlinesResponse {
  symbol: string;
  interval: string;
  data: Kline[];
  count: number;
  /** Present when a full page was returned — page back with `to=cursor`. */
  nextCursor?: string;
}

export function parseKlinesResponse(v: unknown): KlinesResponse | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  const interval = str(v['interval']);
  if (!symbol || !interval || !Array.isArray(v['data'])) return null;
  const data: Kline[] = [];
  for (const item of v['data']) {
    const k = parseKline(item);
    if (k) data.push(k);
  }
  return {
    symbol,
    interval,
    data,
    count: num(v['count']) ?? data.length,
    nextCursor: str(v['next_cursor']),
  };
}

// ---------------------------------------------------------------------------
// Instruments (reference data)
// ---------------------------------------------------------------------------

/** GET /api/v1/instruments row (marketapi instrumentDoc — flat fields +
 * structured filters + trading hours). Decimal bounds stay strings. */
export interface Instrument {
  symbol: string;
  baseCurrency: string;
  quoteCurrency: string;
  instrumentType: string; // SPOT|FORWARD|SWAP|NDF|OPTION
  status: string; // §7.1 lifecycle (ACTIVE|RESTRICTED|SUSPENDED|…)
  tickSize: string;
  lotSize: string;
  minOrderQty: string;
  maxOrderQty: string;
  minNotional: string;
  minPrice?: string;
  maxPrice?: string;
  priceBandPctUp?: string;
  priceBandPctDown?: string;
  maxSpreadPips?: string;
  maxLeverage: number;
  settlementCycle: number; // 0 same-day, 1 T+1, 2 T+2
  settlement: string;
}

export function parseInstrument(v: unknown): Instrument | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  if (!symbol) return null;
  return {
    symbol,
    baseCurrency: str(v['base_currency']) ?? '',
    quoteCurrency: str(v['quote_currency']) ?? '',
    instrumentType: str(v['instrument_type']) ?? 'SPOT',
    status: str(v['status']) ?? 'DRAFT',
    tickSize: decStr(v['tick_size']) ?? '0.00001',
    lotSize: decStr(v['lot_size']) ?? '1',
    minOrderQty: decStr(v['min_order_qty']) ?? '0',
    maxOrderQty: decStr(v['max_order_qty']) ?? '0',
    minNotional: decStr(v['min_notional']) ?? '0',
    minPrice: decStr(v['min_price']),
    maxPrice: decStr(v['max_price']),
    priceBandPctUp: decStr(v['price_band_pct_up']),
    priceBandPctDown: decStr(v['price_band_pct_down']),
    maxSpreadPips: decStr(v['max_spread_pips']),
    maxLeverage: num(v['max_leverage']) ?? 0,
    settlementCycle: num(v['settlement_cycle']) ?? 1,
    settlement: str(v['settlement']) ?? 'T+1',
  };
}

export function parseInstrumentsResponse(v: unknown): Instrument[] | null {
  if (!isRecord(v) || !Array.isArray(v['data'])) return null;
  const out: Instrument[] = [];
  for (const item of v['data']) {
    const i = parseInstrument(item);
    if (i) out.push(i);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Public trades
// ---------------------------------------------------------------------------

/** trades@{symbol} event payload (marketdata tradeData). */
export interface TradeEvent {
  symbol: string;
  tradeId: number;
  price: string;
  quantity: string;
  side: 'BUY' | 'SELL' | 'UNKNOWN';
  isBuyerMaker?: boolean;
  tsMs: number;
}

export function parseTradeEvent(v: unknown): TradeEvent | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  const price = decStr(v['price']);
  const quantity = decStr(v['quantity']);
  if (!symbol || !price || !quantity) return null;
  const side = str(v['side']);
  return {
    symbol,
    tradeId: num(v['trade_id']) ?? 0,
    price,
    quantity,
    side: side === 'BUY' || side === 'SELL' ? side : 'UNKNOWN',
    isBuyerMaker: bool(v['is_buyer_maker']),
    tsMs: num(v['ts_ms']) ?? 0,
  };
}

// ---------------------------------------------------------------------------
// Private channels (§10.5)
// ---------------------------------------------------------------------------

/** private:orders lifecycle event (marketdata.OrderEvent). */
export interface PrivateOrderEvent {
  /** orderAck | orderFill | orderCancel | orderExpire | orderReject |
   * orderAmend | orderTrigger | orderLiquidate */
  event: string;
  orderId: string;
  clientOrderId?: string;
  symbol: string;
  side?: string;
  type?: string;
  status: string; // NEW|PARTIALLY_FILLED|FILLED|CANCELLED|EXPIRED|REJECTED
  price?: string;
  quantity?: string;
  filledQty?: string;
  leavesQty?: string;
  lastFillQty?: string;
  lastFillPrice?: string;
  expiryReason?: string;
  reason?: string;
  tsMs: number;
}

export function parsePrivateOrderEvent(v: unknown): PrivateOrderEvent | null {
  if (!isRecord(v)) return null;
  const event = str(v['event']);
  if (!event) return null;
  return {
    event,
    orderId: str(v['order_id']) ?? '',
    clientOrderId: str(v['client_order_id']),
    symbol: str(v['symbol']) ?? '',
    side: str(v['side']),
    type: str(v['type']),
    status: str(v['status']) ?? '',
    price: decStr(v['price']),
    quantity: decStr(v['quantity']),
    filledQty: decStr(v['filled_qty']),
    leavesQty: decStr(v['leaves_qty']),
    lastFillQty: decStr(v['last_fill_qty']),
    lastFillPrice: decStr(v['last_fill_price']),
    expiryReason: str(v['expiry_reason']),
    reason: str(v['reason']),
    tsMs: num(v['ts_ms']) ?? 0,
  };
}

/** private:balances payload (ledger.BalanceEvent, BALANCE_CHANGED). */
export interface BalanceEvent {
  accountId: number;
  currency: string;
  journalId: number;
  available: string;
  locked: string;
  total: string;
  eventType: string;
}

export function parseBalanceEvent(v: unknown): BalanceEvent | null {
  if (!isRecord(v)) return null;
  const currency = str(v['currency']);
  const available = decStr(v['available']);
  const locked = decStr(v['locked']);
  const total = decStr(v['total']);
  if (!currency || !available || !locked || !total) return null;
  return {
    accountId: num(v['account_id']) ?? 0,
    currency,
    journalId: num(v['journal_id']) ?? 0,
    available,
    locked,
    total,
    eventType: str(v['event_type']) ?? '',
  };
}

/** private:positions payload — the Phase-19 producer defines the event;
 * this parser is deliberately tolerant (carry the REST row field set +
 * `adl_indicator` per Task 10.3.13). */
export interface PositionEvent {
  event: string;
  positionId?: number;
  symbol: string;
  side?: string; // LONG | SHORT
  quantity?: string;
  entryPrice?: string;
  markPrice?: string;
  unrealizedPnl?: string;
  marginUsed?: string;
  liquidationPrice?: string;
  /** ADL priority rank 1–5 (Phase-19 Task 19.3.19). */
  adlIndicator?: number;
  closed?: boolean;
  tsMs: number;
}

export function parsePositionEvent(v: unknown): PositionEvent | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  if (!symbol) return null;
  return {
    event: str(v['event']) ?? 'positionUpdate',
    positionId: num(v['position_id']),
    symbol,
    side: str(v['side']),
    quantity: decStr(v['quantity']),
    entryPrice: decStr(v['entry_price']),
    markPrice: decStr(v['mark_price']),
    unrealizedPnl: decStr(v['unrealized_pnl']),
    marginUsed: decStr(v['margin_used']),
    liquidationPrice: decStr(v['liquidation_price']),
    adlIndicator: num(v['adl_indicator']),
    closed: bool(v['closed']),
    tsMs: num(v['ts_ms']) ?? 0,
  };
}

// ---------------------------------------------------------------------------
// Account REST rows (funding.BalanceRow / PositionRow)
// ---------------------------------------------------------------------------

export interface BalanceRow {
  currency: string;
  available: string;
  locked: string;
  total: string;
}

export function parseBalancesResponse(v: unknown): BalanceRow[] | null {
  if (!isRecord(v) || !Array.isArray(v['balances'])) return null;
  const out: BalanceRow[] = [];
  for (const item of v['balances']) {
    if (!isRecord(item)) continue;
    const currency = str(item['currency']);
    const available = decStr(item['available']);
    const locked = decStr(item['locked']);
    const total = decStr(item['total']);
    if (currency && available !== undefined && locked !== undefined && total !== undefined) {
      out.push({ currency, available, locked, total });
    }
  }
  return out;
}

export interface PositionRow {
  positionId: number;
  instrumentId: number;
  symbol: string;
  side: string; // LONG | SHORT
  quantity: string;
  entryPrice: string;
  markPrice?: string;
  unrealizedPnl: string;
  realizedPnl: string;
  liquidationPrice?: string;
  marginUsed: string;
  openedAt: string;
  updatedAt: string;
}

export function parsePositionRow(v: unknown): PositionRow | null {
  if (!isRecord(v)) return null;
  const symbol = str(v['symbol']);
  const side = str(v['side']);
  const quantity = decStr(v['quantity']);
  const entryPrice = decStr(v['entry_price']);
  if (!symbol || !side || !quantity || !entryPrice) return null;
  return {
    positionId: num(v['position_id']) ?? 0,
    instrumentId: num(v['instrument_id']) ?? 0,
    symbol,
    side,
    quantity,
    entryPrice,
    markPrice: decStr(v['mark_price']),
    unrealizedPnl: decStr(v['unrealized_pnl']) ?? '0',
    realizedPnl: decStr(v['realized_pnl']) ?? '0',
    liquidationPrice: decStr(v['liquidation_price']),
    marginUsed: decStr(v['margin_used']) ?? '0',
    openedAt: str(v['opened_at']) ?? '',
    updatedAt: str(v['updated_at']) ?? '',
  };
}

export function parsePositionsResponse(v: unknown): PositionRow[] | null {
  if (!isRecord(v) || !Array.isArray(v['positions'])) return null;
  const out: PositionRow[] = [];
  for (const item of v['positions']) {
    const p = parsePositionRow(item);
    if (p) out.push(p);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Order submission (orders.ParseSubmit body / orders.Ack / orders.Preview)
// ---------------------------------------------------------------------------

export type OrderSide = 'BUY' | 'SELL';
export type OrderType = 'LIMIT' | 'MARKET' | 'STOP' | 'STOP_LIMIT' | 'ICEBERG';
export type TimeInForce = 'GTC' | 'IOC' | 'FOK' | 'GTD' | 'DAY';

export interface SubmitOrderRequest {
  symbol: string;
  side: OrderSide;
  type: OrderType;
  time_in_force: TimeInForce;
  client_order_id: string;
  /** Exactly one of quantity|quote_quantity (§22.1). */
  quantity?: string;
  quote_quantity?: string;
  price?: string;
  stop_price?: string;
  iceberg_visible_qty?: string;
  /** RFC3339 expiry for GTD orders. */
  gtd_expiry?: string;
  post_only?: boolean;
  reduce_only?: boolean;
  stp_mode?: string;
}

/** POST /api/v1/orders ack (HTTP 202, orders.Ack). */
export interface OrderAck {
  orderId: number;
  clientOrderId?: string;
  status: string;
  orderSeq: number;
  replay: boolean;
  transactTime: string;
}

export function parseOrderAck(v: unknown): OrderAck | null {
  if (!isRecord(v)) return null;
  const orderId = num(v['order_id']);
  const status = str(v['status']);
  if (orderId === undefined || !status) return null;
  return {
    orderId,
    clientOrderId: str(v['client_order_id']),
    status,
    orderSeq: num(v['order_seq']) ?? 0,
    replay: bool(v['replay']) ?? false,
    transactTime: str(v['transact_time']) ?? '',
  };
}

/** POST /api/v1/orders/test preview (orders.Preview, §22.1 — dry-run,
 * never binding). */
export interface OrderPreview {
  estimatedBaseQty: string;
  estimatedQuoteQty: string;
  margin: string;
  commissionEstimate: string;
  spreadEstimate: string;
  riskLevel: 'LOW' | 'MEDIUM' | 'HIGH';
  warnings: string[];
  activeFilters: string[];
  binding: boolean;
}

export function parseOrderPreview(v: unknown): OrderPreview | null {
  if (!isRecord(v)) return null;
  const risk = str(v['risk_level']);
  return {
    estimatedBaseQty: decStr(v['estimated_base_qty']) ?? '0',
    estimatedQuoteQty: decStr(v['estimated_quote_qty']) ?? '0',
    margin: decStr(v['margin']) ?? '0',
    commissionEstimate: decStr(v['commission_estimate']) ?? '0',
    spreadEstimate: decStr(v['spread_estimate']) ?? '0',
    riskLevel: risk === 'MEDIUM' || risk === 'HIGH' ? risk : 'LOW',
    warnings: Array.isArray(v['warnings'])
      ? v['warnings'].filter((w): w is string => typeof w === 'string')
      : [],
    activeFilters: Array.isArray(v['active_filters'])
      ? v['active_filters'].filter((w): w is string => typeof w === 'string')
      : [],
    binding: bool(v['binding']) ?? false,
  };
}
