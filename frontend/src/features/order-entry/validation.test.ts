import { describe, expect, it } from 'vitest';

import type { Instrument } from '../../lib/market/wire';
import { buildRequest, newClientOrderId, validateDraft, type OrderDraft } from './validation';

const INST: Instrument = {
  symbol: 'EUR/USD',
  baseCurrency: 'EUR',
  quoteCurrency: 'USD',
  instrumentType: 'SPOT',
  status: 'ACTIVE',
  tickSize: '0.00001',
  lotSize: '100',
  minOrderQty: '100',
  maxOrderQty: '1000000',
  minNotional: '10',
  minPrice: '0.5',
  maxPrice: '2.0',
  maxLeverage: 30,
  settlementCycle: 1,
  settlement: 'T+1',
};

const draft = (over: Partial<OrderDraft> = {}): OrderDraft => ({
  symbol: 'EUR/USD',
  side: 'BUY',
  type: 'LIMIT',
  quantity: '1000',
  price: '1.08500',
  timeInForce: 'GTC',
  gtdExpiry: '',
  ...over,
});

describe('validateDraft', () => {
  it('accepts a well-formed limit order', () => {
    expect(validateDraft(draft(), INST)).toEqual({});
  });
  it('rejects empty/non-positive quantity', () => {
    // quantity is conditionally required (MARKET accepts quote_quantity
    // instead), so empty lands on the relational rule, not "required".
    expect(validateDraft(draft({ quantity: '' }), INST).quantity).toMatch(/positive/);
    expect(validateDraft(draft({ quantity: '0' }), INST).quantity).toMatch(/positive/);
    expect(validateDraft(draft({ quantity: 'abc' }), INST).quantity).toMatch(/decimal/);
  });
  it('enforces min/max/lot from the instrument', () => {
    expect(validateDraft(draft({ quantity: '50' }), INST).quantity).toMatch(/min_order_qty/);
    expect(validateDraft(draft({ quantity: '2000000' }), INST).quantity).toMatch(/max_order_qty/);
    expect(validateDraft(draft({ quantity: '150' }), INST).quantity).toMatch(/lot_size/);
  });
  it('limit price: required, positive, tick-aligned, in band', () => {
    expect(validateDraft(draft({ price: '' }), INST).price).toMatch(/required/);
    expect(validateDraft(draft({ price: '-1' }), INST).price).toMatch(/positive/);
    expect(validateDraft(draft({ price: '1.085001' }), INST).price).toMatch(/tick_size/);
    expect(validateDraft(draft({ price: '0.4' }), INST).price).toMatch(/min_price/);
    expect(validateDraft(draft({ price: '3.0' }), INST).price).toMatch(/max_price/);
  });
  it('enforces minimum notional on limit orders', () => {
    // 100 × 0.5 = 50 ≥ 10 ok; 100 × ... need <10 → qty 100 @ 0.05 (min price 0.5 blocks); use minNotional-aware pair:
    const inst = { ...INST, minPrice: '0.01', minNotional: '100' };
    const e = validateDraft(draft({ quantity: '100', price: '0.50' }), inst);
    expect(e.notional).toMatch(/minimum notional/);
  });
  it('market orders skip price validation entirely', () => {
    const e = validateDraft(draft({ type: 'MARKET', price: '' }), INST);
    expect(e.price).toBeUndefined();
    expect(e).toEqual({});
  });
  it('GTD requires a future expiry; other TIFs do not', () => {
    expect(validateDraft(draft({ timeInForce: 'GTD' }), INST).gtdExpiry).toMatch(/require/);
    expect(
      validateDraft(draft({ timeInForce: 'GTD', gtdExpiry: '2000-01-01T00:00' }), INST).gtdExpiry,
    ).toMatch(/future/);
    const future = new Date(Date.now() + 86_400_000).toISOString().slice(0, 16);
    expect(
      validateDraft(draft({ timeInForce: 'GTD', gtdExpiry: future }), INST).gtdExpiry,
    ).toBeUndefined();
    expect(validateDraft(draft({ timeInForce: 'IOC' }), INST).gtdExpiry).toBeUndefined();
  });
  it('works without an instrument (server is authoritative)', () => {
    expect(validateDraft(draft(), undefined)).toEqual({});
  });
});

describe('buildRequest', () => {
  it('canonicalizes decimals and wires the dedup key', () => {
    const r = buildRequest(draft({ quantity: ' 1,000.00 ', price: '1.085000' }), 'web-x');
    expect(r).toMatchObject({
      symbol: 'EUR/USD',
      side: 'BUY',
      type: 'LIMIT',
      time_in_force: 'GTC',
      client_order_id: 'web-x',
      quantity: '1000',
      price: '1.085',
    });
  });
  it('omits price for market orders', () => {
    const r = buildRequest(draft({ type: 'MARKET', price: '9.99' }), 'web-x');
    expect(r.price).toBeUndefined();
  });
  it('serializes GTD expiry to RFC3339', () => {
    const r = buildRequest(draft({ timeInForce: 'GTD', gtdExpiry: '2030-06-01T12:30' }), 'web-x');
    expect(r.gtd_expiry).toMatch(/^2030-06-01T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
  });
});

describe('newClientOrderId', () => {
  it('generates web-prefixed unique ids', () => {
    const a = newClientOrderId();
    const b = newClientOrderId();
    expect(a).toMatch(/^web-/);
    expect(a).not.toBe(b);
  });
});
