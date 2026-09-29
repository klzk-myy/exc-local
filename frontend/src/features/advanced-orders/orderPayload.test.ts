/**
 * Order payload builder tests (Task 10.3.7) — wire-shape coverage for
 * every order kind + TIF legality + field-level validation errors.
 */
import { describe, expect, it } from 'vitest';

import { buildOrderPayload, gtdToRfc3339, EMPTY_FORM, type OrderFormState } from './orderPayload';
import type {
  AlgoOrderBody,
  BracketOrderBody,
  OcoOrderBody,
  SubmitOrderBody,
} from '@/lib/trading/api';

function form(p: Partial<OrderFormState>): OrderFormState {
  return { ...EMPTY_FORM, symbol: 'EUR/USD', quantity: '1000', ...p };
}

describe('buildOrderPayload — base types', () => {
  it('LIMIT GTC emits price + TIF on /orders', () => {
    const b = buildOrderPayload(form({ kind: 'LIMIT', tif: 'GTC', price: '1.10' }));
    expect(b.endpoint).toBe('orders');
    expect(b.errors).toEqual({});
    const body = b.body as SubmitOrderBody;
    expect(body).toMatchObject({
      symbol: 'EUR/USD',
      side: 'BUY',
      type: 'LIMIT',
      time_in_force: 'GTC',
      price: '1.1', // canonical Dec form — trailing zeros trimmed
      quantity: '1000',
    });
  });

  it('MARKET omits price and TIF (implicit IOC)', () => {
    const b = buildOrderPayload(form({ kind: 'MARKET' }));
    const body = b.body as SubmitOrderBody;
    expect(body.type).toBe('MARKET');
    expect(body.price).toBeUndefined();
    expect(body.time_in_force).toBeUndefined();
  });

  it('STOP requires stop_price; STOP_LIMIT requires both legs', () => {
    const stop = buildOrderPayload(form({ kind: 'STOP', stopPrice: '1.09' }));
    expect((stop.body as SubmitOrderBody).stop_price).toBe('1.09');
    expect(stop.errors).toEqual({});

    const missing = buildOrderPayload(form({ kind: 'STOP' }));
    expect(missing.errors['stopPrice']).toBeDefined();

    const sl = buildOrderPayload(form({ kind: 'STOP_LIMIT', stopPrice: '1.09', price: '1.091' }));
    const slBody = sl.body as SubmitOrderBody;
    expect(slBody.stop_price).toBe('1.09');
    expect(slBody.price).toBe('1.091');
    expect(sl.errors).toEqual({});
  });

  it('ICEBERG emits iceberg_visible_qty and rejects visible ≥ total', () => {
    const ok = buildOrderPayload(form({ kind: 'ICEBERG', price: '1.1', visibleQty: '100' }));
    expect((ok.body as SubmitOrderBody).iceberg_visible_qty).toBe('100');
    expect(ok.errors).toEqual({});

    const bad = buildOrderPayload(form({ kind: 'ICEBERG', price: '1.1', visibleQty: '1000' }));
    expect(bad.errors['visibleQty']).toContain('below total');
  });

  it('GTD requires a future RFC3339 expiry', () => {
    const past = buildOrderPayload(
      form({ kind: 'LIMIT', price: '1.1', tif: 'GTD', gtdExpiry: '2000-01-01T00:00' }),
    );
    expect(past.errors['gtdExpiry']).toContain('future');

    const future = buildOrderPayload(
      form({ kind: 'LIMIT', price: '1.1', tif: 'GTD', gtdExpiry: '2999-01-01T00:00' }),
    );
    expect(future.errors).toEqual({});
    expect((future.body as SubmitOrderBody).gtd_expiry).toBe('2999-01-01T00:00:00.000Z');

    const missing = buildOrderPayload(form({ kind: 'LIMIT', price: '1.1', tif: 'GTD' }));
    expect(missing.errors['gtdExpiry']).toBeDefined();
  });

  it('IOC/FOK ride on LIMIT; post_only/reduce_only forward', () => {
    const b = buildOrderPayload(
      form({ kind: 'LIMIT', price: '1.1', tif: 'FOK', postOnly: true, reduceOnly: true }),
    );
    const body = b.body as SubmitOrderBody;
    expect(body.time_in_force).toBe('FOK');
    expect(body.post_only).toBe(true);
    expect(body.reduce_only).toBe(true);
  });

  it('non-default trigger source rides algo_params', () => {
    const b = buildOrderPayload(
      form({ kind: 'STOP', stopPrice: '1.09', triggerSource: 'MARK_PRICE' }),
    );
    expect((b.body as SubmitOrderBody).algo_params).toEqual({ trigger_source: 'MARK_PRICE' });
  });
});

describe('buildOrderPayload — composite/algo kinds', () => {
  it('TRAILING_STOP → /orders/algo with distance + unit params', () => {
    const b = buildOrderPayload(
      form({ kind: 'TRAILING_STOP', trailingDistance: '20', trailingUnit: 'PIPS' }),
    );
    expect(b.endpoint).toBe('algo');
    expect(b.errors).toEqual({});
    const body = b.body as AlgoOrderBody;
    expect(body.algo_type).toBe('TRAILING_STOP');
    expect(body.algo_params).toMatchObject({
      trailing_offset: '20',
      trailing_unit: 'PIPS',
      trigger_source: 'LAST_PRICE',
    });
  });

  it('BRACKET → /orders/bracket with SL + TP children; market parent when entry empty', () => {
    const b = buildOrderPayload(
      form({ kind: 'BRACKET', bracketStop: '1.05', bracketTarget: '1.20' }),
    );
    expect(b.endpoint).toBe('bracket');
    const body = b.body as BracketOrderBody;
    expect(body.stop_price).toBe('1.05');
    expect(body.take_profit_price).toBe('1.2');
    expect(body.entry_price).toBeUndefined(); // market parent

    const limited = buildOrderPayload(
      form({ kind: 'BRACKET', price: '1.10', bracketStop: '1.05', bracketTarget: '1.20' }),
    );
    expect((limited.body as BracketOrderBody).entry_price).toBe('1.1');
  });

  it('BRACKET sanity: BUY stop must sit below take-profit', () => {
    const b = buildOrderPayload(
      form({ kind: 'BRACKET', bracketStop: '1.20', bracketTarget: '1.05' }),
    );
    expect(b.errors['bracketStop']).toBeDefined();
    const sell = buildOrderPayload(
      form({ kind: 'BRACKET', side: 'SELL', bracketStop: '1.05', bracketTarget: '1.20' }),
    );
    expect(sell.errors['bracketStop']).toBeDefined();
  });

  it('OCO → /orders/oco with limit leg + stop trigger', () => {
    const b = buildOrderPayload(form({ kind: 'OCO', ocoLimit: '1.20', stopPrice: '1.05' }));
    expect(b.endpoint).toBe('oco');
    const body = b.body as OcoOrderBody;
    expect(body.price).toBe('1.2');
    expect(body.stop_price).toBe('1.05');
    expect(b.errors).toEqual({});
  });

  it('missing fields surface per-field errors and never produce a half-body', () => {
    const b = buildOrderPayload({ ...EMPTY_FORM });
    expect(Object.keys(b.errors).length).toBeGreaterThan(0);
    expect(b.errors['symbol']).toBeDefined();
    expect(b.errors['quantity']).toBeDefined();
  });
});

describe('gtdToRfc3339', () => {
  it('parses datetime-local as UTC', () => {
    expect(gtdToRfc3339('2030-06-15T14:30')).toBe('2030-06-15T14:30:00.000Z');
  });
  it('returns undefined for empty/garbage', () => {
    expect(gtdToRfc3339('')).toBeUndefined();
    expect(gtdToRfc3339('not-a-date')).toBeUndefined();
  });
});
