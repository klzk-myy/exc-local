import { describe, expect, it } from 'vitest';

import { routeContract } from './generated/route-contracts';
import {
  ROUTE_FIELD_BINDINGS,
  RULE_COUNTDOWN_MS,
  RULE_SYMBOL,
  validateAgainstInstrument,
  validateField,
  validateOrderSubmit,
  validateRecord,
} from './validation';

const eurUsd = {
  tickSize: '0.00005',
  lotSize: '1000',
  minOrderQty: '1000',
  maxOrderQty: '50000000',
  minNotional: '100',
  minPrice: null,
  maxPrice: null,
};

describe('route contracts (generated from docs/openapi/openapi.json)', () => {
  it('resolves known routes', () => {
    const c = routeContract('POST /api/v1/orders');
    expect(c).toBeDefined();
    expect(c?.method).toBe('POST');
    expect(c?.authRequired).toBe(true);
    expect(c?.scopes).toContain('trade');
  });
  it('flags stub routes', () => {
    expect(routeContract('GET /api/v1/copy/strategies')?.status).toBe('stub');
    expect(routeContract('GET /api/v1/exchange-info')?.status).toBe('live');
  });
  it('returns undefined for unknown routes', () => {
    expect(routeContract('GET /api/v1/nope')).toBeUndefined();
  });
});

describe('validateField', () => {
  it('enforces required', () => {
    expect(validateField(RULE_SYMBOL, '')?.message).toMatch(/required/);
    expect(validateField(RULE_SYMBOL, undefined)?.message).toMatch(/required/);
  });
  it('enforces symbol pattern', () => {
    expect(validateField(RULE_SYMBOL, 'EURUSD')).not.toBeNull();
    expect(validateField(RULE_SYMBOL, 'eur/usd')).not.toBeNull(); // canonical form is uppercase
    expect(validateField(RULE_SYMBOL, 'EUR/USD')).toBeNull();
  });
  it('enforces integer bounds (dead-man countdown)', () => {
    expect(validateField(RULE_COUNTDOWN_MS, '999')?.message).toMatch(/below minimum 1000/);
    expect(validateField(RULE_COUNTDOWN_MS, '300001')?.message).toMatch(/exceeds maximum 300000/);
    expect(validateField(RULE_COUNTDOWN_MS, '60000')).toBeNull();
    expect(validateField(RULE_COUNTDOWN_MS, '60.5')?.message).toMatch(/integer/);
  });
  it('enforces enum + decimal positivity', () => {
    const sideRule = {
      name: 'side',
      required: true,
      kind: 'string' as const,
      enum: ['BUY', 'SELL'],
    };
    expect(validateField(sideRule, 'HOLD')).not.toBeNull();
    const qty = { name: 'quantity', kind: 'decimal' as const, positive: true };
    expect(validateField(qty, '-5')).not.toBeNull();
    expect(validateField(qty, '0')).not.toBeNull();
    expect(validateField(qty, '0.5')).toBeNull();
  });
});

describe('validateRecord + route bindings', () => {
  it('POST /api/v1/orders binding validates a complete submit', () => {
    const errs = validateRecord(ROUTE_FIELD_BINDINGS['POST /api/v1/orders'] ?? [], {
      symbol: 'EUR/USD',
      side: 'BUY',
      type: 'LIMIT',
      quantity: '1000',
      price: '1.08500',
    });
    expect(errs).toEqual([]);
  });
  it('reports missing required fields', () => {
    const errs = validateRecord(ROUTE_FIELD_BINDINGS['POST /api/v1/orders'] ?? [], {});
    expect(errs.map((e) => e.field)).toContain('symbol');
    expect(errs.map((e) => e.field)).toContain('side');
  });
});

describe('validateOrderSubmit (§22.1 relational rules)', () => {
  it('MARKET requires exactly one of quantity|quote_quantity', () => {
    expect(validateOrderSubmit({ type: 'MARKET' }).map((e) => e.field)).toContain('quantity');
    expect(validateOrderSubmit({ type: 'MARKET', quantity: '100' })).toEqual([]);
    expect(
      validateOrderSubmit({ type: 'MARKET', quantity: '100', quote_quantity: '90' }).map(
        (e) => e.field,
      ),
    ).toContain('quantity');
  });
  it('quote_quantity only on MARKET', () => {
    expect(
      validateOrderSubmit({ type: 'LIMIT', quantity: '1', price: '1', quote_quantity: '5' })[0]
        ?.field,
    ).toBe('quote_quantity');
  });
  it('price required for LIMIT/ICEBERG, stop for STOP', () => {
    expect(validateOrderSubmit({ type: 'LIMIT', quantity: '1' }).map((e) => e.field)).toContain(
      'price',
    );
    expect(validateOrderSubmit({ type: 'STOP', quantity: '1' }).map((e) => e.field)).toContain(
      'stop_price',
    );
    expect(
      validateOrderSubmit({ type: 'STOP_LIMIT', quantity: '1', price: '1' }).map((e) => e.field),
    ).toContain('stop_price');
  });
  it('GTD requires gtd_expiry; iceberg qty only on ICEBERG', () => {
    expect(
      validateOrderSubmit({ type: 'LIMIT', quantity: '1', price: '1', time_in_force: 'GTD' })[0]
        ?.field,
    ).toBe('gtd_expiry');
    expect(
      validateOrderSubmit({
        type: 'LIMIT',
        quantity: '1',
        price: '1',
        iceberg_visible_qty: '0.5',
      })[0]?.field,
    ).toBe('iceberg_visible_qty');
  });
});

describe('validateAgainstInstrument (filter mirror)', () => {
  it('catches tick/lot/min violations', () => {
    const errs = validateAgainstInstrument(eurUsd, {
      quantity: '1500', // not a 1000 multiple
      price: '1.08497', // not a 0.00005 multiple
    });
    expect(errs.map((e) => e.field).sort()).toEqual(['price', 'quantity']);
  });
  it('catches min/max qty bounds', () => {
    expect(validateAgainstInstrument(eurUsd, { quantity: '500' })[0]?.message).toMatch(
      /below min_order_qty/,
    );
    expect(validateAgainstInstrument(eurUsd, { quantity: '60000000' })[0]?.message).toMatch(
      /exceeds max_order_qty/,
    );
  });
  it('passes clean values', () => {
    expect(validateAgainstInstrument(eurUsd, { quantity: '25000', price: '1.0850' })).toEqual([]);
  });
});
