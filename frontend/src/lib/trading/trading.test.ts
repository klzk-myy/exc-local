import { describe, expect, it } from 'vitest';

import { dec } from '@/lib/decimal/decimal';

import { isJpyPair, liquidationEstimate, pipSize, pipValue, priceDecimals, splitPair } from './fx';
import { bookMid, cumulate, depthAtPrice, projectFill } from './projections';
import {
  isOpenOrder,
  parseBbo,
  parseBookSnapshot,
  parseDepthUpdate,
  parseInstrumentList,
  parseOrderList,
  parsePositionList,
  parseSubAccountList,
  type BookSnapshot,
} from './types';

describe('fx helpers', () => {
  it('splits pair symbols', () => {
    expect(splitPair('EUR/USD')).toEqual({ base: 'EUR', quote: 'USD' });
    expect(splitPair('USDJPY')).toEqual({ base: 'USD', quote: 'JPY' });
    expect(splitPair('X')).toBeNull();
  });
  it('pip size honors JPY convention', () => {
    expect(isJpyPair('USD/JPY')).toBe(true);
    expect(pipSize('USD/JPY').toString()).toBe('0.01');
    expect(pipSize('EUR/USD').toString()).toBe('0.0001');
    // 1 lot EUR/USD = $10/pip; 1 lot USD/JPY = ¥1000/pip
    expect(pipValue('EUR/USD', dec('100000')).toString()).toBe('10');
    expect(pipValue('USD/JPY', dec('100000')).toString()).toBe('1000');
  });
  it('liquidation estimate: long loses headroom below entry', () => {
    // entry 1.10, qty 100k, margin 2000, maint 50bps of notional
    const liq = liquidationEstimate('LONG', dec('1.10'), dec('100000'), dec('2000'), dec('50'));
    // headroom = 2000 - 550 = 1450; perUnit = 0.0145; liq = 1.10 - 0.0145 = 1.0855
    expect(liq?.toFixed(6)).toBe('1.085500');
  });
  it('liquidation estimate: undefined on zero inputs — never fabricated', () => {
    expect(
      liquidationEstimate('LONG', dec('1.1'), dec('0'), dec('2000'), dec('50')),
    ).toBeUndefined();
    expect(
      liquidationEstimate('SHORT', dec('1.1'), dec('100'), dec('0'), dec('50')),
    ).toBeUndefined();
  });
  it('price decimals follow the pair convention', () => {
    expect(priceDecimals('USD/JPY')).toBe(3);
    expect(priceDecimals('EUR/USD')).toBe(5);
    expect(priceDecimals('EUR/USD', dec('0.000001'))).toBe(6);
  });
});

const BOOK: BookSnapshot = {
  symbol: 'EUR/USD',
  seq: 10,
  updatedAtMs: 0,
  bids: [
    { price: dec('1.0999'), qty: dec('50000') },
    { price: dec('1.0998'), qty: dec('100000') },
  ],
  asks: [
    { price: dec('1.1001'), qty: dec('40000') },
    { price: dec('1.1002'), qty: dec('200000') },
  ],
};

describe('projected execution (book walk)', () => {
  it('walks asks for a BUY and reports VWAP + slippage', () => {
    const mid = bookMid(BOOK);
    const p = projectFill(BOOK, 'BUY', dec('100000'), mid);
    expect(p).toBeDefined();
    // 40k @1.1001 + 60k @1.1002 = 44004+66012=110016/100000=1.10016
    expect(p?.avgPrice.toFixed(6)).toBe('1.100160');
    expect(p?.fullyFillable).toBe(true);
    expect(p?.slippageBps?.gt(dec('0'))).toBe(true);
  });
  it('reports partial fillability on shallow books', () => {
    const p = projectFill(BOOK, 'SELL', dec('1000000'), bookMid(BOOK));
    expect(p?.fullyFillable).toBe(false);
    expect(p?.filledQty.toString()).toBe('150000');
  });
  it('never fabricates a fill on empty input', () => {
    expect(projectFill(undefined, 'BUY', dec('1'), undefined)).toBeUndefined();
    expect(projectFill(BOOK, 'BUY', dec('0'), undefined)).toBeUndefined();
  });
  it('mid is undefined on one-sided books', () => {
    expect(bookMid({ ...BOOK, asks: [] })).toBeUndefined();
  });
});

describe('depth curves', () => {
  it('cumulates qty and notional outward', () => {
    const pts = cumulate(BOOK.asks);
    expect(pts).toHaveLength(2);
    expect(pts[0]?.cumQty.toString()).toBe('40000');
    expect(pts[1]?.cumQty.toString()).toBe('240000');
    expect(pts[1]?.cumNotional.toFixed(2)).toBe('264044.00');
  });
  it('depthAtPrice answers "at price or better"', () => {
    const d = depthAtPrice('ASK', dec('1.1001'), BOOK);
    expect(d.qty.toString()).toBe('40000');
    const b = depthAtPrice('BID', dec('1.0998'), BOOK);
    expect(b.qty.toString()).toBe('150000');
  });
});

describe('wire parsers (fail-closed)', () => {
  it('parses the REST book snapshot shape', () => {
    const snap = parseBookSnapshot({
      symbol: 'EUR/USD',
      seq: 5,
      depth: 20,
      bids: [{ price: '1.0999', quantity: '50000' }],
      asks: [],
      updated_at_ms: 123,
    });
    expect(snap?.bids[0]?.price.toString()).toBe('1.0999');
  });
  it('parses the WS depthUpdate triples shape', () => {
    const snap = parseDepthUpdate({
      event: 'depthUpdate',
      symbol: 'EUR/USD',
      bids: [['1.0999', '50000', '3']],
      asks: [['1.1001', '40000', '2']],
      seq: 7,
    });
    expect(snap?.asks[0]?.qty.toString()).toBe('40000');
  });
  it('parses bbo with nullable sides', () => {
    const b = parseBbo({ event: 'bbo', symbol: 'EUR/USD', bid: '1.1', ask: null });
    expect(b?.bid?.toString()).toBe('1.1');
    expect(b?.ask).toBeUndefined();
    expect(parseBbo({ nope: 1 })).toBeNull();
  });
  it('parses instruments, positions, orders, sub-accounts', () => {
    const inst = parseInstrumentList({
      data: [{ symbol: 'EUR/USD', tick_size: '0.00001', lot_size: '1000', max_leverage: 30 }],
    });
    expect(inst[0]?.symbol).toBe('EUR/USD');

    const pos = parsePositionList({
      positions: [
        {
          position_id: 7,
          symbol: 'EUR/USD',
          side: 'LONG',
          quantity: '1000',
          entry_price: '1.1',
          adl_indicator: 4,
        },
      ],
    });
    expect(pos[0]?.adlIndicator).toBe(4);
    // out-of-range ADL narrows to undefined — never fabricated
    const pos2 = parsePositionList({
      positions: [
        { symbol: 'EUR/USD', side: 'SHORT', quantity: '1', entry_price: '1', adl_indicator: 9 },
      ],
    });
    expect(pos2[0]?.adlIndicator).toBeUndefined();

    const { orders } = parseOrderList({
      data: [
        {
          order_id: '9',
          symbol: 'EUR/USD',
          side: 'BUY',
          type: 'LIMIT',
          status: 'ACTIVE',
          quantity: '100',
          order_seq: 42,
        },
      ],
      next_cursor: 'abc',
    });
    expect(orders[0]?.orderSeq).toBe(42);
    expect(isOpenOrder(orders[0]!)).toBe(true);
    expect(orders[0]!.status).toBe('ACTIVE');

    const subs = parseSubAccountList({
      data: [
        {
          id: 42,
          master_account_id: 1,
          trading_enabled: true,
          balances: [{ currency: 'USD', available: '10', locked: '0', total: '10' }],
        },
      ],
    });
    expect(subs[0]?.balances[0]?.available.toString()).toBe('10');
  });
  it('returns empty lists on malformed envelopes', () => {
    expect(parseInstrumentList(null)).toEqual([]);
    expect(parsePositionList(42)).toEqual([]);
    expect(parseOrderList('x').orders).toEqual([]);
    expect(parseSubAccountList(undefined)).toEqual([]);
    expect(parseBookSnapshot({})).toBeNull();
  });
});
