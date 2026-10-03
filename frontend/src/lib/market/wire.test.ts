import { describe, expect, it } from 'vitest';

import {
  parseBalanceEvent,
  parseBalancesResponse,
  parseBookSnapshot,
  parseDepthUpdate,
  parseInstrument,
  parseInstrumentsResponse,
  parseKline,
  parseKlinesResponse,
  parseOrderAck,
  parseOrderPreview,
  parsePositionEvent,
  parsePositionRow,
  parsePositionsResponse,
  parsePrivateOrderEvent,
  parseTicker24h,
  parseTradeEvent,
} from './wire';

describe('parseBookSnapshot (REST shape)', () => {
  it('parses {price,quantity} level objects', () => {
    const s = parseBookSnapshot({
      symbol: 'EUR/USD',
      seq: 42,
      depth: 2,
      bids: [{ price: '1.0850', quantity: '100' }],
      asks: [{ price: '1.0851', quantity: '200' }],
      updated_at_ms: 1234,
    });
    expect(s?.bids[0]).toEqual({ price: '1.0850', qty: '100', count: undefined });
    expect(s?.seq).toBe(42);
  });
  it('drops malformed levels and rejects missing symbol', () => {
    expect(
      parseBookSnapshot({ symbol: 'X', bids: [['1', '2'], { nope: 1 }, 'junk'], asks: [] })?.bids,
    ).toEqual([{ price: '1', qty: '2', count: undefined }]);
    expect(parseBookSnapshot({ bids: [] })).toBeNull();
    expect(parseBookSnapshot('nope')).toBeNull();
  });
});

describe('parseDepthUpdate (WS depth@/book@ shape)', () => {
  const frame = {
    event: 'depthUpdate',
    symbol: 'EUR/USD',
    bids: [
      ['1.0850', '100', '3'],
      ['1.0849', '50', '1'],
    ],
    asks: [['1.0852', '80', '2']],
    first_seq: 10,
    last_seq: 12,
    prev_last_seq: 9,
    seq: 12,
    engine_seq: 999,
    coalesced: 3,
    crc32: 12345,
    ts_ms: 555,
    is_snapshot: false,
  };
  it('parses [price,qty,count] triples + §10.9 envelope', () => {
    const d = parseDepthUpdate(frame);
    expect(d?.bids[0]).toEqual({ price: '1.0850', qty: '100', count: 3 });
    expect(d?.lastSeq).toBe(12);
    expect(d?.prevLastSeq).toBe(9);
    expect(d?.crc32).toBe(12345);
  });
  it('rejects symbol-less frames', () => {
    expect(parseDepthUpdate({ bids: [] })).toBeNull();
    expect(parseDepthUpdate(null)).toBeNull();
  });
});

describe('klines', () => {
  const k = {
    open_time_ms: 1000,
    open: '1.0',
    high: '1.1',
    low: '0.9',
    close: '1.05',
    volume: '500',
    quote_volume: '525',
    trade_count: 7,
    closed: true,
  };
  it('parseKline narrows OHLCV rows', () => {
    expect(parseKline(k)?.close).toBe('1.05');
    expect(parseKline({ ...k, open_time_ms: 'x' })).toBeNull();
    expect(parseKline({ ...k, open: null })).toBeNull();
  });
  it('parseKlinesResponse reads the envelope + next_cursor', () => {
    const r = parseKlinesResponse({
      symbol: 'EUR/USD',
      interval: '1m',
      data: [k],
      count: 1,
      limit: 500,
      next_cursor: '900',
    });
    expect(r?.data).toHaveLength(1);
    expect(r?.nextCursor).toBe('900');
    expect(parseKlinesResponse({ data: 'x' })).toBeNull();
  });
});

describe('instruments', () => {
  it('parses reference-data rows with defaults', () => {
    const i = parseInstrument({
      symbol: 'USD/JPY',
      base_currency: 'USD',
      quote_currency: 'JPY',
      instrument_type: 'SPOT',
      status: 'ACTIVE',
      tick_size: '0.001',
      lot_size: '1000',
      min_order_qty: '1000',
      max_order_qty: '10000000',
      min_notional: '1000',
      max_leverage: 30,
      settlement_cycle: 2,
      settlement: 'T+2',
    });
    expect(i?.tickSize).toBe('0.001');
    expect(i?.settlement).toBe('T+2');
    expect(parseInstrumentsResponse({ data: [i, { bad: 1 }] })).toHaveLength(1);
    expect(parseInstrumentsResponse({})).toBeNull();
  });
});

describe('private channels', () => {
  it('parses private:orders lifecycle events', () => {
    const ev = parsePrivateOrderEvent({
      event: 'orderReject',
      order_id: '77',
      client_order_id: 'web-1',
      symbol: 'EUR/USD',
      status: 'REJECTED',
      reason: 'INSUFFICIENT_BALANCE',
      ts_ms: 1,
    });
    expect(ev?.clientOrderId).toBe('web-1');
    expect(ev?.reason).toBe('INSUFFICIENT_BALANCE');
    expect(parsePrivateOrderEvent({})).toBeNull();
  });

  it('parses BALANCE_CHANGED events', () => {
    const b = parseBalanceEvent({
      account_id: 5,
      currency: 'USD',
      journal_id: 9,
      available: '1000.00',
      locked: '50.00',
      total: '1050.00',
      event_type: 'BALANCE_CHANGED',
    });
    expect(b?.available).toBe('1000.00');
    expect(parseBalanceEvent({ currency: 'USD' })).toBeNull();
  });

  it('parses tolerant position events (adl_indicator included)', () => {
    const p = parsePositionEvent({
      event: 'positionUpdate',
      symbol: 'GBP/USD',
      side: 'LONG',
      quantity: '1000',
      mark_price: '1.27',
      unrealized_pnl: '12.5',
      adl_indicator: 3,
      ts_ms: 2,
    });
    expect(p?.adlIndicator).toBe(3);
    expect(parsePositionEvent({ event: 'x' })).toBeNull();
  });
});

describe('trades + account rows', () => {
  it('parses trade events', () => {
    const t = parseTradeEvent({
      symbol: 'EUR/USD',
      trade_id: 9,
      price: '1.085',
      quantity: '10',
      side: 'SELL',
      ts_ms: 1,
    });
    expect(t?.side).toBe('SELL');
    expect(parseTradeEvent({ symbol: 'X', side: 'BUY' })).toBeNull();
  });

  it('parses 24h ticker frames (snake_case → camel, decimals stay strings)', () => {
    const t = parseTicker24h({
      symbol: 'EUR/USD',
      open: '1.08',
      high: '1.11',
      low: '1.07',
      close: '1.1001',
      volume: '250000',
      quote_volume: '275000',
      price_change: '0.0201',
      price_change_pct: '1.86',
      weighted_avg_price: '1.09',
      trade_count: 42,
      open_time_ms: 10,
      close_time_ms: 20,
    });
    expect(t).toMatchObject({
      symbol: 'EUR/USD',
      close: '1.1001',
      quoteVolume: '275000',
      priceChange: '0.0201',
      priceChangePct: '1.86',
      tradeCount: 42,
    });
    expect(parseTicker24h({ symbol: 'X' })).toBeNull(); // no close
    expect(parseTicker24h('nope')).toBeNull();
  });

  it('parses positions + balances responses', () => {
    const pos = {
      position_id: 1,
      instrument_id: 2,
      symbol: 'EUR/USD',
      side: 'LONG',
      quantity: '1000',
      entry_price: '1.08',
      mark_price: '1.09',
      unrealized_pnl: '10',
      realized_pnl: '0',
      liquidation_price: '1.01',
      margin_used: '36',
      opened_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    };
    expect(parsePositionRow(pos)?.markPrice).toBe('1.09');
    expect(parsePositionsResponse({ account_id: 1, positions: [pos] })).toHaveLength(1);
    expect(parsePositionsResponse({ positions: 'x' })).toBeNull();
    const b = parseBalancesResponse({
      account_id: 1,
      balances: [{ currency: 'USD', available: '100', locked: '5', total: '105' }],
    });
    expect(b).toEqual([{ currency: 'USD', available: '100', locked: '5', total: '105' }]);
    expect(parseBalancesResponse({ balances: 'x' })).toBeNull();
  });
});

describe('order ack + preview', () => {
  it('parses the 202 ack', () => {
    const a = parseOrderAck({
      order_id: 123,
      client_order_id: 'web-9',
      status: 'ACTIVE',
      order_seq: 55,
      replay: true,
      transact_time: '2026-01-01T00:00:00Z',
    });
    expect(a?.orderId).toBe(123);
    expect(a?.replay).toBe(true);
    expect(parseOrderAck({ status: 'X' })).toBeNull();
  });

  it('parses the orders/test preview envelope', () => {
    const p = parseOrderPreview({
      estimated_base_qty: '1000',
      estimated_quote_qty: '1085',
      margin: '36.17',
      commission_estimate: '0.05',
      spread_estimate: '0.0001',
      risk_level: 'LOW',
      warnings: ['thin book'],
      active_filters: ['PRICE_FILTER'],
      binding: false,
    });
    expect(p?.riskLevel).toBe('LOW');
    expect(p?.warnings).toEqual(['thin book']);
    expect(parseOrderPreview(null)).toBeNull();
  });
});
