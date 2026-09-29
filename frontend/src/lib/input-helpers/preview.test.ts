import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ApiClient } from '@/lib/api';

import { debounce, deriveGrid, previewFunding, previewOrder } from './preview';

describe('previewOrder / previewFunding adapters', () => {
  it('posts to the live test endpoint', async () => {
    const post = vi.fn().mockResolvedValue({
      estimated_base_qty: '1000',
      estimated_quote_qty: '1085',
      margin: '36.17',
      commission_estimate: '0.10',
      spread_estimate: '0.00005',
      risk_level: 'LOW',
      warnings: [],
      active_filters: [],
      binding: false,
    });
    const api = { post } as unknown as ApiClient;
    const res = await previewOrder(api, {
      symbol: 'EUR/USD',
      side: 'BUY',
      type: 'LIMIT',
      quantity: '1000',
      price: '1.08500',
    });
    expect(post).toHaveBeenCalledWith(
      '/orders/test',
      expect.objectContaining({ symbol: 'EUR/USD' }),
    );
    expect(res.binding).toBe(false);
    expect(res.risk_level).toBe('LOW');
  });

  it('funding preview calls the (currently stubbed) estimator', async () => {
    const post = vi.fn().mockRejectedValue(new Error('NOT_IMPLEMENTED'));
    const api = { post } as unknown as ApiClient;
    await expect(previewFunding(api, { amount: '10' })).rejects.toThrow('NOT_IMPLEMENTED');
    expect(post).toHaveBeenCalledWith('/funding/fee-estimate', { amount: '10' });
  });
});

describe('deriveGrid (client-side grid math)', () => {
  it('spacing = (upper−lower)/(count−1)', () => {
    const g = deriveGrid('1.00000', '1.10000', 11, '11000', '2');
    expect(g.gridStep).toBe('0.01');
    expect(g.perGridQuote).toBe('1000');
    // mid = 1.05 → per-grid base ≈ 952.380952380952
    expect(g.perGridBase).toBe('952.380952380952');
    // fee = 11000 × 2bps × 1e-4 = 2.2
    expect(g.feeEstimate).toBe('2.2');
  });
  it('single-level grid divides by 1 (no div-by-zero)', () => {
    const g = deriveGrid('1.0', '1.0', 1, '100', null);
    expect(g.gridStep).toBe('0');
    expect(g.feeEstimate).toBeNull();
  });
  it('inverted range yields negative step — caller validates', () => {
    expect(deriveGrid('2.0', '1.0', 5, '100', null).gridStep).toBe('-0.25');
  });
});

describe('debounce', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });
  it('coalesces rapid calls into one', () => {
    const fn = vi.fn();
    const d = debounce(fn, 150);
    d.call('a');
    d.call('b');
    d.call('c');
    vi.advanceTimersByTime(200);
    expect(fn).toHaveBeenCalledTimes(1);
    expect(fn).toHaveBeenCalledWith('c');
  });
  it('cancel prevents the pending call', () => {
    const fn = vi.fn();
    const d = debounce(fn, 150);
    d.call('a');
    d.cancel();
    vi.advanceTimersByTime(200);
    expect(fn).not.toHaveBeenCalled();
  });
});
