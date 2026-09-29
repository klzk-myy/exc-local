import { describe, expect, it } from 'vitest';

import { Dec, decOrZero, stepDecimals } from './decimal';

describe('Dec.parse', () => {
  it('parses integers, decimals, signs', () => {
    expect(Dec.parse('123')?.toString()).toBe('123');
    expect(Dec.parse('1.2300')?.toString()).toBe('1.23');
    expect(Dec.parse('-0.5')?.toString()).toBe('-0.5');
    expect(Dec.parse('+7')?.toString()).toBe('7');
    expect(Dec.parse('0.000001')?.toFixed(6)).toBe('0.000001');
  });

  it('rejects malformed input (fail-closed)', () => {
    for (const s of ['', 'abc', '1.2.3', 'NaN', '1e5', '.5', '5.', '  ']) {
      expect(Dec.parse(s), s).toBeNull();
    }
  });
});

describe('arithmetic', () => {
  it('add/sub align scales exactly', () => {
    expect(Dec.of('0.1').add(Dec.of('0.2')).toString()).toBe('0.3');
    expect(Dec.of('1.005').sub(Dec.of('1')).toString()).toBe('0.005');
    expect(Dec.of('2').add(Dec.of('-5')).toString()).toBe('-3');
  });

  it('mul accumulates scale', () => {
    expect(Dec.of('1.5').mul(Dec.of('2')).toString()).toBe('3');
    expect(Dec.of('1.08500').mul(Dec.of('100000')).toString()).toBe('108500');
    expect(Dec.of('0.001').mul(Dec.of('0.001')).toString()).toBe('0.000001');
  });

  it('divInt truncates toward zero', () => {
    expect(Dec.of('10').divInt(3).toString()).toBe('3');
    expect(Dec.of('-10').divInt(3).toString()).toBe('-3');
  });

  it('divInt extraScale keeps sub-precision digits (exact mid)', () => {
    // mid of 1.08501 + 1.08502 = 1.085015 — needs one extra scale digit
    const mid = Dec.of('1.08501').add(Dec.of('1.08502')).divInt(2, 1);
    expect(mid.toString()).toBe('1.085015');
    expect(mid.toFixed(5)).toBe('1.08502'); // rounds half-up at tick scale
    // without extraScale the half-tick truncates away
    expect(Dec.of('1.08501').add(Dec.of('1.08502')).divInt(2).toString()).toBe('1.08501');
  });

  it('cmp/gt/lt/eq across scales', () => {
    expect(Dec.of('1.0').eq(Dec.of('1'))).toBe(true);
    expect(Dec.of('1.0001').gt(Dec.of('1'))).toBe(true);
    expect(Dec.of('-2').lt(Dec.of('0.00000001'))).toBe(true);
  });
});

describe('isMultipleOf (tick/lot alignment)', () => {
  it('accepts exact steps', () => {
    expect(Dec.of('1.0850').isMultipleOf(Dec.of('0.0001'))).toBe(true);
    expect(Dec.of('1500').isMultipleOf(Dec.of('100'))).toBe(true);
    expect(Dec.of('0.5').isMultipleOf(Dec.of('0.25'))).toBe(true);
  });
  it('rejects off-step values', () => {
    expect(Dec.of('1.08501').isMultipleOf(Dec.of('0.0001'))).toBe(false);
    expect(Dec.of('150').isMultipleOf(Dec.of('100'))).toBe(false);
    expect(Dec.of('1').isMultipleOf(Dec.of('0'))).toBe(false);
  });
});

describe('toFixed / toString / toNumber', () => {
  it('toFixed rounds half-up', () => {
    expect(Dec.of('1.2345').toFixed(3)).toBe('1.235');
    expect(Dec.of('1.2344').toFixed(3)).toBe('1.234');
    expect(Dec.of('-1.235').toFixed(2)).toBe('-1.24');
    expect(Dec.of('5').toFixed(4)).toBe('5.0000');
    expect(Dec.of('0.0004').toFixed(3)).toBe('0.000');
    expect(Dec.of('-0.0004').toFixed(3)).toBe('0.000'); // no "-0.000"
  });
  it('toNumber is display-only', () => {
    expect(Dec.of('1.5').toNumber()).toBe(1.5);
  });
});

describe('helpers', () => {
  it('decOrZero tolerates null/garbage', () => {
    expect(decOrZero(null).isZero()).toBe(true);
    expect(decOrZero('nonsense').isZero()).toBe(true);
    expect(decOrZero('2.5').toString()).toBe('2.5');
  });
  it('stepDecimals derives fraction digits', () => {
    expect(stepDecimals('0.0001')).toBe(4);
    expect(stepDecimals('0.5')).toBe(1);
    expect(stepDecimals('1000')).toBe(0);
    expect(stepDecimals('junk')).toBe(0);
  });
});
