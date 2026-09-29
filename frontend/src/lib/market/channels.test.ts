import { describe, expect, it } from 'vitest';

import {
  bboChannel,
  depthChannel,
  KLINE_INTERVAL_MS,
  KLINE_INTERVAL_SET,
  KLINE_INTERVALS,
  klineChannel,
  PRIVATE_CHANNELS,
  tradesChannel,
} from './channels';

describe('channel grammar (services/internal/marketdata/channels.go)', () => {
  it('builds the canonical bare depth@ default (20:100)', () => {
    expect(depthChannel('EUR/USD')).toBe('depth@EUR/USD');
    expect(depthChannel('EUR/USD', 20, 100)).toBe('depth@EUR/USD');
  });
  it('parameterizes non-default depth variants', () => {
    expect(depthChannel('EUR/USD', 5)).toBe('depth@EUR/USD:5:100');
    expect(depthChannel('EUR/USD', 10, 250)).toBe('depth@EUR/USD:10:250');
    expect(depthChannel('USD/JPY', 5, 1000)).toBe('depth@USD/JPY:5:1000');
  });
  it('builds kline@{symbol}_{timeframe} channels', () => {
    expect(klineChannel('EUR/USD', '1h')).toBe('kline@EUR/USD_1h');
    expect(klineChannel('EUR/USD', '1D')).toBe('kline@EUR/USD_1D');
  });
  it('builds the other typed channels', () => {
    expect(tradesChannel('EUR/USD')).toBe('trades@EUR/USD');
    expect(bboChannel('EUR/USD')).toBe('bbo@EUR/USD');
  });
  it('pins the canonical 13-timeframe set (§24 #264)', () => {
    expect(KLINE_INTERVALS).toEqual([
      '1s',
      '1m',
      '5m',
      '15m',
      '30m',
      '1h',
      '2h',
      '4h',
      '6h',
      '8h',
      '1D',
      '1W',
      '1M',
    ]);
    expect(KLINE_INTERVAL_SET.has('3m')).toBe(false);
    expect(KLINE_INTERVAL_SET.has('12h')).toBe(false);
  });
  it('interval ms map is fixed-width except calendar frames', () => {
    expect(KLINE_INTERVAL_MS['5m']).toBe(300_000);
    expect(KLINE_INTERVAL_MS['1W']).toBeUndefined();
    expect(KLINE_INTERVAL_MS['1M']).toBeUndefined();
  });
  it('private channel names match ws.PrivateChannels', () => {
    expect(PRIVATE_CHANNELS).toEqual({
      orders: 'private:orders',
      executions: 'private:executions',
      positions: 'private:positions',
      balances: 'private:balances',
    });
  });
});
