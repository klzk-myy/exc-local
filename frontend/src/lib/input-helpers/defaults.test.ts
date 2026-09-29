import { describe, expect, it } from 'vitest';

import {
  defaultBeneficiary,
  defaultLimitPrice,
  defaultMarketQty,
  defaultStopPrice,
  defaultTransferAccounts,
} from './defaults';
import { toInstrumentMeta } from './instruments';

const meta = toInstrumentMeta({
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

describe('defaultLimitPrice', () => {
  const bbo = { bid: '1.08495', ask: '1.08505' };
  it('BUY defaults to ask, SELL to bid (tick-rounded)', () => {
    expect(defaultLimitPrice(meta, 'BUY', bbo)).toBe('1.08505');
    expect(defaultLimitPrice(meta, 'SELL', bbo)).toBe('1.08495');
  });
  it('returns null without a quote — never fabricates', () => {
    expect(defaultLimitPrice(meta, 'BUY', null)).toBeNull();
    expect(defaultLimitPrice(meta, 'BUY', { ask: null })).toBeNull();
  });
});

describe('defaultMarketQty', () => {
  it('max affordable = margin×leverage/price, lot-rounded down', () => {
    // 1000 × 30 / 1.085 = 27,649.7… → 27,000
    expect(defaultMarketQty(meta, '1000', '1.08500', '30')).toBe('27000');
  });
  it('null on bad input', () => {
    expect(defaultMarketQty(meta, 'x', '1', '1')).toBeNull();
  });
});

describe('defaultStopPrice', () => {
  it('BUY stop = entry − ATR; SELL = entry + ATR (tick-rounded)', () => {
    expect(defaultStopPrice(meta, 'BUY', '1.08500', '0.00500')).toBe('1.08');
    expect(defaultStopPrice(meta, 'SELL', '1.08500', '0.00500')).toBe('1.09');
  });
  it('no ATR → no default (honest absence)', () => {
    expect(defaultStopPrice(meta, 'BUY', '1.08500', null)).toBeNull();
  });
});

describe('defaultBeneficiary / defaultTransferAccounts', () => {
  const bens = [
    { id: 'b1', currency: 'EUR', name: 'A' },
    { id: 'b2', currency: 'EUR', name: 'B' },
    { id: 'b3', currency: 'USD', name: 'C' },
  ];
  it('prefers last-used within currency, falls back to first', () => {
    expect(defaultBeneficiary(bens, 'EUR', 'b2')?.id).toBe('b2');
    expect(defaultBeneficiary(bens, 'EUR', null)?.id).toBe('b1');
    expect(defaultBeneficiary(bens, 'GBP', null)).toBeUndefined();
  });
  it('transfer: sub→master default; master leaves `to` empty', () => {
    expect(defaultTransferAccounts('sub-1', 'master')).toEqual({ from: 'sub-1', to: 'master' });
    expect(defaultTransferAccounts('master', 'master')).toEqual({ from: 'master', to: '' });
  });
});
