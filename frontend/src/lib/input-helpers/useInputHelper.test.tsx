import { renderHook, act } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { useInputHelper } from './useInputHelper';

describe('useInputHelper', () => {
  it('binds the generated contract for a known route', () => {
    const { result } = renderHook(() => useInputHelper('POST /api/v1/orders'));
    expect(result.current.registered).toBe(true);
    expect(result.current.isStub).toBe(false);
    expect(result.current.fields.length).toBeGreaterThan(0);
  });

  it('flags stub routes (sub-accounts owe a later phase)', () => {
    const { result } = renderHook(() => useInputHelper('GET /api/v1/account/sub-accounts'));
    expect(result.current.isStub).toBe(true);
    // Phase-14 landed copy trading — the same hook reports live now.
    const live = renderHook(() => useInputHelper('GET /api/v1/copy/strategies'));
    expect(live.result.current.isStub).toBe(false);
    // Phase-20 landed trade confirmations — live now.
    const confirmations = renderHook(() =>
      useInputHelper('GET /api/v1/account/confirmations/{trade_id}'),
    );
    expect(confirmations.result.current.isStub).toBe(false);
  });

  it('validate() surfaces field + cross-field errors', () => {
    const { result } = renderHook(() => useInputHelper('POST /api/v1/orders'));
    let errs: readonly { field: string }[] = [];
    act(() => {
      errs = result.current.validate({ symbol: 'EUR/USD', type: 'LIMIT', quantity: '1000' });
    });
    // LIMIT without price → relational error; missing side → field error
    const names = errs.map((e) => e.field);
    expect(names).toContain('side');
    expect(names).toContain('price');
    expect(result.current.isValid).toBe(false);
    act(() => {
      errs = result.current.validate({
        symbol: 'EUR/USD',
        side: 'BUY',
        type: 'LIMIT',
        quantity: '1000',
        price: '1.08500',
      });
    });
    expect(errs).toEqual([]);
    expect(result.current.isValid).toBe(true);
  });

  it('pathErrors validates path params (countdown range is body, trade_id is a path param)', () => {
    const { result } = renderHook(() =>
      useInputHelper('GET /api/v1/account/confirmations/{trade_id}'),
    );
    expect(result.current.registered).toBe(true);
    expect(result.current.pathErrors({})).toHaveLength(1);
    expect(result.current.pathErrors({ trade_id: 'abc-123' })).toEqual([]);
  });

  it('unknown routes report registered=false and validate nothing', () => {
    const { result } = renderHook(() => useInputHelper('POST /api/v1/fictional'));
    expect(result.current.registered).toBe(false);
    act(() => {
      expect(result.current.validate({ anything: 'x' })).toEqual([]);
    });
  });
});
