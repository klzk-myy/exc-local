import { describe, expect, it } from 'vitest';

import {
  addDecimal,
  clampDecimal,
  cmpDecimal,
  decimalPlaces,
  divDecimal,
  isDecimal,
  isMultipleOf,
  isPositive,
  isZero,
  mulDecimal,
  parseDecimal,
  roundToStep,
  subDecimal,
  toDecimalString,
} from './decimal';

describe('parseDecimal', () => {
  it('parses plain decimals', () => {
    expect(parseDecimal('1.2345')).toEqual({ mantissa: 12345n, scale: 4 });
    expect(parseDecimal('-0.001')).toEqual({ mantissa: -1n, scale: 3 });
    expect(parseDecimal('12')).toEqual({ mantissa: 12n, scale: 0 });
    expect(parseDecimal('+7')).toEqual({ mantissa: 7n, scale: 0 });
  });
  it('rejects malformed input (fail-closed)', () => {
    expect(parseDecimal('')).toBeNull();
    expect(parseDecimal('abc')).toBeNull();
    expect(parseDecimal('1.2.3')).toBeNull();
    expect(parseDecimal('NaN')).toBeNull();
    expect(parseDecimal('1,234.56')).toBeNull();
  });
  it('normalizes scientific notation', () => {
    expect(parseDecimal('1e-4')).toEqual({ mantissa: 1n, scale: 4 });
    expect(parseDecimal('1.5e3')).toEqual({ mantissa: 1500n, scale: 0 });
  });
});

describe('toDecimalString', () => {
  it('round-trips and trims trailing zeros', () => {
    expect(toDecimalString({ mantissa: 12345n, scale: 4 })).toBe('1.2345');
    expect(toDecimalString({ mantissa: 12000n, scale: 4 })).toBe('1.2');
    expect(toDecimalString({ mantissa: -5n, scale: 3 })).toBe('-0.005');
    expect(toDecimalString({ mantissa: 0n, scale: 4 })).toBe('0');
  });
});

describe('arithmetic', () => {
  it('add/sub', () => {
    expect(addDecimal('1.005', '0.01')).toBe('1.015');
    expect(subDecimal('1.10', '0.05')).toBe('1.05');
    expect(addDecimal('x', '1')).toBeNull();
  });
  it('mul keeps exact fixed-point precision', () => {
    expect(mulDecimal('1.0850', '100000')).toBe('108500');
    expect(mulDecimal('0.0001', '10')).toBe('0.001');
    expect(mulDecimal('2.5', '-4')).toBe('-10');
  });
  it('div rounds per mode', () => {
    expect(divDecimal('1', '3', 4, 'down')).toBe('0.3333');
    expect(divDecimal('1', '3', 4, 'up')).toBe('0.3334');
    expect(divDecimal('1', '2', 4, 'half-up')).toBe('0.5');
    expect(divDecimal('5', '0')).toBeNull();
  });
  it('div handles large quote conversions exactly', () => {
    // 10000 / 1.0852 = 9214.8912... — check monotonic floor.
    const v = divDecimal('10000', '1.0852', 8, 'down');
    expect(v).not.toBeNull();
    const back = mulDecimal(v ?? '0', '1.0852');
    expect(cmpDecimal(back ?? '0', '10000')).toBeLessThanOrEqual(0);
  });
});

describe('predicates', () => {
  it('cmp/isPositive/isZero/isDecimal', () => {
    expect(cmpDecimal('1.0', '1')).toBe(0);
    expect(cmpDecimal('0.999', '1')).toBe(-1);
    expect(isPositive('0.0001')).toBe(true);
    expect(isPositive('-1')).toBe(false);
    expect(isZero('0.000')).toBe(true);
    expect(isDecimal('12.50')).toBe(true);
    expect(isDecimal('')).toBe(false);
  });
  it('isMultipleOf enforces tick/lot grids', () => {
    expect(isMultipleOf('1.23450', '0.00005')).toBe(true);
    expect(isMultipleOf('1.23451', '0.00005')).toBe(false);
    expect(isMultipleOf('25000', '25000')).toBe(true);
    expect(isMultipleOf('25001', '25000')).toBe(false);
  });
  it('decimalPlaces', () => {
    expect(decimalPlaces('0.001')).toBe(3);
    expect(decimalPlaces('1.08500')).toBe(5);
    expect(decimalPlaces('7')).toBe(0);
  });
});

describe('roundToStep', () => {
  it('rounds to nearest tick', () => {
    expect(roundToStep('1.08498', '0.00005')).toBe('1.085');
    // 1.08497 is closer to 1.08495 than 1.08500 on a 0.00005 grid.
    expect(roundToStep('1.08497', '0.00005')).toBe('1.08495');
    expect(roundToStep('1.08492', '0.00005')).toBe('1.0849');
  });
  it('floors for down mode (lot-round-down semantics)', () => {
    expect(roundToStep('9999.9', '1000', 'down')).toBe('9000');
    expect(roundToStep('1.08499', '0.00005', 'down')).toBe('1.08495');
  });
  it('ceil for up mode', () => {
    expect(roundToStep('9999.1', '1000', 'up')).toBe('10000');
  });
  it('rejects zero/negative step', () => {
    expect(roundToStep('1', '0')).toBeNull();
    expect(roundToStep('1', '-0.01')).toBeNull();
  });
});

describe('clampDecimal', () => {
  it('clamps inside bounds', () => {
    expect(clampDecimal('5', '1', '10')).toBe('5');
    expect(clampDecimal('0', '1', '10')).toBe('1');
    expect(clampDecimal('11', '1', '10')).toBe('10');
    expect(clampDecimal('5', null, '10')).toBe('5');
  });
});
