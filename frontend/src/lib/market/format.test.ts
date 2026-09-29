import { describe, expect, it } from 'vitest';

import { Dec } from './decimal';
import {
  formatCurrency,
  formatPrice,
  formatPnl,
  formatQty,
  groupThousands,
  parseInputDecimal,
} from './format';
import type { Instrument } from './wire';

const EURUSD: Instrument = {
  symbol: 'EUR/USD',
  baseCurrency: 'EUR',
  quoteCurrency: 'USD',
  instrumentType: 'SPOT',
  status: 'ACTIVE',
  tickSize: '0.00001',
  lotSize: '1000',
  minOrderQty: '1000',
  maxOrderQty: '10000000',
  minNotional: '1000',
  maxLeverage: 30,
  settlementCycle: 1,
  settlement: 'T+1',
};

const USDJPY: Instrument = { ...EURUSD, symbol: 'USD/JPY', tickSize: '0.001' };

describe('groupThousands', () => {
  it('groups integer part, preserves fraction + sign', () => {
    expect(groupThousands('1234567.89')).toBe('1,234,567.89');
    expect(groupThousands('-1234.5')).toBe('-1,234.5');
    expect(groupThousands('999')).toBe('999');
    expect(groupThousands('0.00001')).toBe('0.00001');
  });
});

describe('instrument-aware formatting', () => {
  it('formatPrice honors tick_size precision (5dp major, 3dp JPY)', () => {
    expect(formatPrice('1.085432', EURUSD)).toBe('1.08543');
    expect(formatPrice('148.3921', USDJPY)).toBe('148.392');
    expect(formatPrice(Dec.of('1.2'), EURUSD)).toBe('1.20000');
  });
  it('formatQty honors lot_size precision', () => {
    expect(formatQty('1500.5', EURUSD)).toBe('1,501'); // lot 1000 → 0dp, half-up
    expect(formatQty('2500', { ...EURUSD, lotSize: '0.01' })).toBe('2,500.00');
  });
  it('formatCurrency renders fiat 2dp', () => {
    expect(formatCurrency('1234.567')).toBe('1,234.57');
  });
});

describe('formatPnl', () => {
  it('prefixes positive, keeps negative sign, zero unsigned', () => {
    expect(formatPnl('12.5')).toBe('+12.50');
    expect(formatPnl('-3')).toBe('-3.00');
    expect(formatPnl('0.0001')).toBe('+0.00'); // sign survives rounding
    expect(formatPnl('0')).toBe('0.00');
  });
});

describe('parseInputDecimal', () => {
  it('accepts commas as group separators and trims', () => {
    expect(parseInputDecimal(' 1,500.25 ')?.toString()).toBe('1500.25');
    expect(parseInputDecimal('') ?? null).toBeNull();
    expect(parseInputDecimal('abc') ?? null).toBeNull();
  });
});
