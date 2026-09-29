import { describe, expect, it } from 'vitest';

import {
  formatCurrency,
  formatFixed,
  formatPct,
  formatPrice,
  formatQty,
  isPriceKeystrokeAllowed,
  pipsToPrice,
  pnlTone,
  priceToPips,
  snapPriceToTick,
  snapQtyToLot,
} from './format';
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

const usdJpy = toInstrumentMeta({
  symbol: 'USD/JPY',
  base_currency: 'USD',
  quote_currency: 'JPY',
  instrument_type: 'SPOT',
  status: 'ACTIVE',
  tick_size: '0.001',
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

describe('instrument derivation', () => {
  it('majors render 5dp, JPY pairs 3dp', () => {
    expect(eurUsd.pricePrecision).toBe(5);
    expect(usdJpy.pricePrecision).toBe(3);
  });
  it('pip size: 0.0001 standard, 0.01 for JPY-quoted', () => {
    expect(eurUsd.pipSize).toBe('0.0001');
    expect(usdJpy.pipSize).toBe('0.01');
  });
  it('contract size defaults to standard lot', () => {
    expect(eurUsd.contractSize).toBe('100000');
  });
});

describe('formatPrice / formatQty', () => {
  it('formats at instrument precision', () => {
    expect(formatPrice(eurUsd, '1.085')).toBe('1.08500');
    expect(formatPrice(usdJpy, '156.7')).toBe('156.700');
    expect(formatQty(eurUsd, '25000')).toBe('25000');
  });
  it('renders — on malformed input', () => {
    expect(formatPrice(eurUsd, 'n/a')).toBe('—');
  });
  it('snaps price to tick', () => {
    expect(snapPriceToTick(eurUsd, '1.08498')).toBe('1.085');
    expect(snapPriceToTick(eurUsd, 'bad')).toBeNull();
  });
  it('snaps qty to lot (down)', () => {
    expect(snapQtyToLot(eurUsd, '25999')).toBe('25000');
  });
  it('keystroke guard blocks sub-tick input', () => {
    expect(isPriceKeystrokeAllowed(usdJpy, '156.701')).toBe(true);
    expect(isPriceKeystrokeAllowed(usdJpy, '156.7012')).toBe(false);
    expect(isPriceKeystrokeAllowed(usdJpy, 'x')).toBe(false);
    expect(isPriceKeystrokeAllowed(undefined, '1.234567')).toBe(false);
    expect(isPriceKeystrokeAllowed(undefined, '1.23456')).toBe(true);
  });
});

describe('pips', () => {
  it('priceToPips / pipsToPrice', () => {
    expect(priceToPips(eurUsd, '0.0012')).toBe('12.00');
    expect(pipsToPrice(usdJpy, '25')).toBe('0.250');
  });
});

describe('currency / pct / pnl', () => {
  it('formatCurrency groups and respects zero-decimal currencies', () => {
    expect(formatCurrency('USD', '1234567.891')).toBe('USD 1,234,567.89');
    expect(formatCurrency('JPY', '1234567.891')).toBe('JPY 1,234,568');
  });
  it('formatPct', () => {
    expect(formatPct('12.345')).toBe('12.35%');
  });
  it('pnlTone', () => {
    expect(pnlTone('5')).toBe('pos');
    expect(pnlTone('-0.1')).toBe('neg');
    expect(pnlTone('0')).toBe('zero');
  });
  it('formatFixed rounds half-up', () => {
    expect(formatFixed('1.005', 2)).toBe('1.01');
    expect(formatFixed('-1.005', 2)).toBe('-1.01'); // half-up on magnitude
  });
});
