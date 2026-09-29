import { describe, expect, it } from 'vitest';

import {
  csvOrderNotional,
  parseBeneficiaryLines,
  parseCsvOrders,
  parseScaledLadder,
  splitCsvLine,
} from './bulk';

describe('splitCsvLine', () => {
  it('handles quotes and escaped quotes', () => {
    expect(splitCsvLine('a,b,c')).toEqual(['a', 'b', 'c']);
    expect(splitCsvLine('"a,b",c')).toEqual(['a,b', 'c']);
    expect(splitCsvLine('"a""b",c')).toEqual(['a"b', 'c']);
  });
});

describe('parseCsvOrders', () => {
  const good = 'EUR/USD,BUY,LIMIT,1000,1.08500,GTC';

  it('parses a clean CSV with header', () => {
    const r = parseCsvOrders(`symbol,side,type,qty,price,tif\n${good}`);
    expect(r.okCount).toBe(1);
    expect(r.rows[0]?.value).toEqual({
      symbol: 'EUR/USD',
      side: 'BUY',
      type: 'LIMIT',
      quantity: '1000',
      price: '1.08500',
      time_in_force: 'GTC',
    });
  });

  it('defaults TIF to GTC when empty', () => {
    const r = parseCsvOrders('EUR/USD,SELL,MARKET,500');
    expect(r.rows[0]?.value?.time_in_force).toBe('GTC');
  });

  it('rejects invalid rows individually (never silently submits)', () => {
    const r = parseCsvOrders(
      `${good}\nEURUSD,BUY,LIMIT,1000,1.0,GTC\nEUR/USD,HOLD,LIMIT,1,1.0\nEUR/USD,BUY,LIMIT,1000,,GTC`,
    );
    expect(r.okCount).toBe(1);
    expect(r.invalidCount).toBe(3);
    expect(r.rows[1]?.value).toBeNull();
    expect(r.rows[3]?.errors.join(' ')).toMatch(/price required/);
  });

  it('flags batches over the cap (10 submits)', () => {
    const r = parseCsvOrders(Array(11).fill(good).join('\n'));
    expect(r.overLimit).toBe(true);
    expect(r.okCount).toBe(11); // rows still validated; caller blocks submit
  });

  it('computes total notional over valid priced rows', () => {
    const r = parseCsvOrders(`${good}\nEUR/USD,SELL,LIMIT,2000,1.09000,GTC`);
    expect(csvOrderNotional(r.rows)).toBe('3265');
  });
});

describe('parseBeneficiaryLines', () => {
  it('accepts comma or tab lines', () => {
    const r = parseBeneficiaryLines(
      'Alice, DE89370400440532013000, COBADEFF, EUR\nBob\tGB29NWBK60161331926819\tNWBKGB2L',
    );
    expect(r.okCount).toBe(2);
    expect(r.rows[0]?.value?.currency).toBe('EUR');
    expect(r.rows[1]?.value?.currency).toBeNull();
  });
  it('rejects malformed account refs', () => {
    const r = parseBeneficiaryLines('X, abc!, BANK');
    expect(r.invalidCount).toBe(1);
    expect(r.rows[0]?.errors.join(' ')).toMatch(/account reference/);
  });
});

describe('parseScaledLadder', () => {
  it('parses price,qty lines', () => {
    const r = parseScaledLadder('1.08500,1000\n1.08600 2000');
    expect(r.okCount).toBe(2);
    expect(r.rows[1]?.value?.quantity).toBe('2000');
  });
  it('rejects bad columns', () => {
    const r = parseScaledLadder('1.08500');
    expect(r.invalidCount).toBe(1);
  });
});
