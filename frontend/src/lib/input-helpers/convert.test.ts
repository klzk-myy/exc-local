import { describe, expect, it } from 'vitest';

import {
  baseToQuote,
  convertToAccountCurrency,
  lotsToUnits,
  pctToQty,
  pipToPrice,
  priceToPip,
  qtyToPct,
  quoteToBase,
  unitsToLots,
} from './convert';
import { toInstrumentMeta } from './instruments';

const eurUsd = toInstrumentMeta({
  symbol: 'EUR/USD',
  base_currency: 'EUR',
  quote_currency: 'USD',
  instrument_type: 'SPOT',
  status: 'ACTIVE',
  tick_size: '0.00005',
  lot_size: '1000',
  min_order_qty: '1000',
  max_order_qty: '50000000',
  min_notional: '100',
  min_price: null,
  max_price: null,
  price_band_pct_up: '2',
  price_band_pct_down: '2',
  max_spread_pips: null,
  max_open_orders: null,
  max_algo_orders: null,
  max_leverage: 30,
  settlement_cycle: 1,
});

describe('pip ↔ price', () => {
  it('uses instrument pipSize', () => {
    expect(pipToPrice(eurUsd, '25')).toBe('0.0025');
    expect(priceToPip(eurUsd, '0.0025')).toBe('25');
  });
});

describe('lots ↔ units', () => {
  it('standard lot math (100k contract)', () => {
    expect(lotsToUnits(eurUsd, '2')).toBe('200000');
    expect(unitsToLots(eurUsd, '150000')).toBe('1.5');
    // Falls back to standard lot without metadata
    expect(lotsToUnits(undefined, '1')).toBe('100000');
  });
});

describe('base ↔ quote', () => {
  it('buy base costs base*price quote', () => {
    expect(baseToQuote(eurUsd, '50000', '1.08500')).toBe('54250');
  });
  it('quote spend converts to base, lot-rounded down', () => {
    expect(quoteToBase(eurUsd, '54250', '1.08500')).toBe('50000');
    // 1000 USD at 1.085 = 921.658… EUR → floors to lot grid (1000) → 0
    expect(quoteToBase(eurUsd, '1000', '1.08500')).toBe('0');
  });
});

describe('percentage ↔ quantity', () => {
  it('pctToQty: (freeMargin × pct × leverage) / price, lot-rounded down', () => {
    // 10000 × 50% × 30 / 1.085 = 138,248.8… → 138,000
    expect(pctToQty(eurUsd, '50', '10000', '30', '1.08500')).toBe('138000');
  });
  it('qtyToPct inverts', () => {
    // 138000 × 1.085 / 30 / 10000 × 100 = 49.91%
    expect(qtyToPct(eurUsd, '138000', '10000', '30', '1.08500')).toBe('49.91');
  });
});

describe('account-currency seam', () => {
  it('converts at a supplied rate (never invents one)', () => {
    expect(convertToAccountCurrency('100', '1.2')).toBe('120');
    expect(convertToAccountCurrency('100', 'bad')).toBeNull();
  });
});
