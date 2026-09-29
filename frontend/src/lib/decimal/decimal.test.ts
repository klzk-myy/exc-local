import { describe, expect, it } from 'vitest';

import { Dec, dec, tryDec } from './decimal';

describe('Dec parsing', () => {
  it('parses plain decimals', () => {
    expect(dec('1.23').toString()).toBe('1.23');
    expect(dec('-0.005').toString()).toBe('-0.005');
    expect(dec('1000').toString()).toBe('1000');
    expect(dec('0.00000001').toString()).toBe('0.00000001');
  });

  it('parses exponents and signs', () => {
    expect(dec('1.5e3').toString()).toBe('1500');
    expect(dec('12e-2').toString()).toBe('0.12');
    expect(dec('+4.50').toString()).toBe('4.5');
  });

  it('rejects malformed input without throwing via tryDec', () => {
    expect(tryDec('abc')).toBeUndefined();
    expect(tryDec('')).toBeUndefined();
    expect(tryDec('1.2.3')).toBeUndefined();
    expect(tryDec(null)).toBeUndefined();
    expect(tryDec(NaN)).toBeUndefined();
    expect(tryDec(Infinity)).toBeUndefined();
    expect(() => dec('abc')).toThrow(TypeError);
  });

  it('accepts numbers and bigints', () => {
    expect(dec(0.1).toString()).toBe('0.1');
    expect(dec(42n).toString()).toBe('42');
  });
});

describe('Dec arithmetic', () => {
  it('adds and subtracts exactly (0.1 + 0.2 = 0.3)', () => {
    expect(dec('0.1').add(dec('0.2')).toString()).toBe('0.3');
    expect(dec('1.10500').sub(dec('1.10000')).toString()).toBe('0.005');
  });

  it('multiplies exactly', () => {
    expect(dec('1.1055').mul(dec('100000')).toString()).toBe('110550');
    expect(dec('0.01').mul(dec('0.01')).toString()).toBe('0.0001');
  });

  it('divides at fixed precision (half-up)', () => {
    expect(dec('1').div(dec('3')).toFixed(6)).toBe('0.333333');
    expect(dec('2').div(dec('3')).toFixed(6)).toBe('0.666667');
    expect(dec('110550').div(dec('1.1055')).toFixed(0)).toBe('100000');
    // Half-up on negative magnitudes rounds away from zero.
    expect(dec('-2').div(dec('3')).toFixed(2)).toBe('-0.67');
  });

  it('throws on division by zero (fail-closed)', () => {
    expect(() => dec('1').div(Dec.ZERO)).toThrow(RangeError);
  });
});

describe('Dec comparison and predicates', () => {
  it('compares across different scales', () => {
    expect(dec('1.10').cmp(dec('1.1'))).toBe(0);
    expect(dec('-1').lt(dec('0'))).toBe(true);
    expect(dec('0.5').gte(dec('0.49999'))).toBe(true);
  });
  it('predicates', () => {
    expect(Dec.ZERO.isZero()).toBe(true);
    expect(dec('-0.01').isNegative()).toBe(true);
    expect(dec('0.01').isPositive()).toBe(true);
    expect(dec('-2').abs().toString()).toBe('2');
    expect(dec('2').neg().toString()).toBe('-2');
  });
});

describe('Dec quantizeTo (lot/tick rounding)', () => {
  it('floors to the step on down', () => {
    expect(dec('1.234').quantizeTo(dec('0.01'), 'down').toString()).toBe('1.23');
    expect(dec('0.999').quantizeTo(dec('0.25'), 'down').toString()).toBe('0.75');
  });
  it('ceils on up', () => {
    expect(dec('1.231').quantizeTo(dec('0.01'), 'up').toString()).toBe('1.24');
    expect(dec('1.20').quantizeTo(dec('0.01'), 'up').toString()).toBe('1.2');
  });
  it('rounds half-up on nearest', () => {
    expect(dec('1.235').quantizeTo(dec('0.01'), 'nearest').toString()).toBe('1.24');
    expect(dec('1.234').quantizeTo(dec('0.01'), 'nearest').toString()).toBe('1.23');
    expect(dec('1.125').quantizeTo(dec('0.25'), 'nearest').toString()).toBe('1.25');
  });
  it('returns self on a non-positive step', () => {
    expect(dec('5').quantizeTo(Dec.ZERO).toString()).toBe('5');
  });
});

describe('Dec rendering', () => {
  it('toFixed pads and rounds', () => {
    expect(dec('1.1').toFixed(5)).toBe('1.10000');
    expect(dec('1.23456').toFixed(3)).toBe('1.235');
    expect(dec('-1.005').toFixed(2)).toBe('-1.01'); // exact decimal half-up
    expect(dec('9').toFixed(0)).toBe('9');
  });
  it('toDisplay groups thousands', () => {
    expect(dec('1234567.891').toDisplay(2)).toBe('1,234,567.89');
    expect(dec('-42000').toDisplay()).toBe('-42,000');
    expect(dec('0.5').toDisplay(2)).toBe('0.50');
  });
  it('toNumber is display-only', () => {
    expect(dec('1.5').toNumber()).toBe(1.5);
  });
});
