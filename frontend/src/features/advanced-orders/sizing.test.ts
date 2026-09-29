/**
 * Percentage sizing tests (Task 10.3.11) — margin→qty conversion, lot
 * quantization, and every clamp must carry a visible reason.
 */
import { describe, expect, it } from 'vitest';

import { dec } from '@/lib/decimal/decimal';

import { sizeByPercent, SIZE_PRESETS } from './sizing';

const C = {
  step: dec('1000'),
  minQty: dec('1000'),
  maxQty: dec('10000000'),
  minNotional: dec('10'),
};

describe('sizeByPercent', () => {
  it('presets cover the Task 10.3.11 set (10/25/50/75/100)', () => {
    expect([...SIZE_PRESETS]).toEqual([10, 25, 50, 75, 100]);
  });

  it('sizes off free margin × leverage ÷ mark, quantized to lot step', () => {
    // 1000 USD margin × 30 lev = 30k notional → /1.10 = 27272.7 → 27000
    const r = sizeByPercent(100, dec('1000'), dec('30'), dec('1.10'), C);
    expect(r.qty.toString()).toBe('27000');
    expect(r.clampReason).toBeNull();
    expect(r.unavailable).toBe(false);
    expect(r.notional.toFixed(2)).toBe('29700.00');
    expect(r.marginUsed.toFixed(2)).toBe('990.00');
  });

  it('25% preset uses a quarter of free margin', () => {
    const r = sizeByPercent(25, dec('1000'), dec('30'), dec('1.10'), C);
    // 250 × 30 = 7500 / 1.1 = 6818.18 → 6000
    expect(r.qty.toString()).toBe('6000');
  });

  it('clamps at max order qty with a reason', () => {
    const c = { ...C, maxQty: dec('5000') };
    const r = sizeByPercent(100, dec('1000'), dec('30'), dec('1.10'), c);
    expect(r.qty.toString()).toBe('5000');
    expect(r.clampReason).toContain('max order qty');
  });

  it('zeroes and reports when below min qty', () => {
    const c = { ...C, minQty: dec('100000') };
    const r = sizeByPercent(10, dec('100'), dec('30'), dec('1.10'), c);
    expect(r.qty.isZero()).toBe(true);
    expect(r.clampReason).toContain('minimum order qty');
  });

  it('zeroes and reports when below min notional', () => {
    const c = { ...C, step: dec('0.01'), minQty: dec('0.01'), minNotional: dec('50000') };
    const r = sizeByPercent(10, dec('100'), dec('5'), dec('1.10'), c);
    expect(r.qty.isZero()).toBe(true);
    expect(r.clampReason).toContain('minimum notional');
  });

  it('is unavailable (not zero-fabricated) without a mark or margin', () => {
    const noPrice = sizeByPercent(50, dec('1000'), dec('30'), undefined, C);
    expect(noPrice.unavailable).toBe(true);
    const noMargin = sizeByPercent(50, dec('0'), dec('30'), dec('1.1'), C);
    expect(noMargin.unavailable).toBe(true);
  });

  it('reports lot-size rounding when the raw size vanishes', () => {
    const c = { ...C, minQty: Dec0(), minNotional: Dec0() };
    const r = sizeByPercent(10, dec('1'), dec('1'), dec('1.10'), c);
    // raw = 0.1 × 1 / 1.1 = 0.09 < step 1000 → rounds to 0
    expect(r.qty.isZero()).toBe(true);
    expect(r.clampReason).toContain('lot size');
  });
});

function Dec0() {
  return dec('0');
}
