import { describe, expect, it, vi } from 'vitest';

import { ApiClient } from '@/lib/api';
import { parseKline } from '@/lib/market/wire';

import {
  getBars,
  intervalBucketMs,
  intervalToResolution,
  isKlineInterval,
  klineToBar,
  liveKlineToBar,
  resolutionToInterval,
  resolveSymbol,
} from './udf';

const KLINE_ROW = {
  open_time_ms: 1_700_000_000_000,
  close_time_ms: 1_700_000_059_999,
  open: '1.0850',
  high: '1.0862',
  low: '1.0848',
  close: '1.0860',
  volume: '12500',
  quote_volume: '13573.5',
  trade_count: 47,
  closed: true,
};

describe('UDF resolution ↔ canonical interval (13 timeframes)', () => {
  it('maps every canonical interval to a UDF token and back', () => {
    const expected: [string, string][] = [
      ['1s', '1S'],
      ['1m', '1'],
      ['5m', '5'],
      ['15m', '15'],
      ['30m', '30'],
      ['1h', '60'],
      ['2h', '120'],
      ['4h', '240'],
      ['6h', '360'],
      ['8h', '480'],
      ['1D', 'D'],
      ['1W', 'W'],
      ['1M', 'M'],
    ];
    for (const [interval, res] of expected) {
      expect(intervalToResolution(interval as never)).toBe(res);
      expect(resolutionToInterval(res)).toBe(interval);
    }
  });
  it('rejects unsupported resolutions', () => {
    expect(resolutionToInterval('3')).toBeNull();
    expect(resolutionToInterval('720')).toBeNull();
    expect(resolutionToInterval('')).toBeNull();
  });
  it('interval guards + buckets', () => {
    expect(isKlineInterval('4h')).toBe(true);
    expect(isKlineInterval('3m')).toBe(false);
    expect(intervalBucketMs('1h')).toBe(3_600_000);
    expect(intervalBucketMs('1M')).toBeNull();
  });
});

describe('resolveSymbol', () => {
  it('describes a 24x5 forex symbol with the full resolution set', () => {
    const info = resolveSymbol('EUR/USD');
    expect(info).toMatchObject({ name: 'EUR/USD', type: 'forex', session: '24x5' });
    expect(info.supported_resolutions).toHaveLength(13);
  });
});

describe('bar mapping', () => {
  it('klineToBar: ms→seconds, decimal strings→numbers', () => {
    const k = parseKline(KLINE_ROW);
    expect(k).not.toBeNull();
    expect(klineToBar(k!)).toEqual({
      time: 1_700_000_000,
      open: 1.085,
      high: 1.0862,
      low: 1.0848,
      close: 1.086,
      volume: 12500,
    });
  });
  it('liveKlineToBar carries open/closed frames identically', () => {
    const k = parseKline({ ...KLINE_ROW, closed: false });
    expect(liveKlineToBar(k!).time).toBe(1_700_000_000);
  });
});

describe('getBars (UDF history → REST klines)', () => {
  it('fetches /klines, sorts ascending, dedupes overlapping open_times', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(
      new Response(
        JSON.stringify({
          symbol: 'EUR/USD',
          interval: '1h',
          count: 3,
          data: [
            { ...KLINE_ROW, open_time_ms: 1_700_003_600_000, close: '1.09' },
            KLINE_ROW,
            { ...KLINE_ROW }, // duplicate row — dedup
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = new ApiClient({ baseUrl: '/api/v1', fetchImpl: fetchMock });
    const bars = await getBars(api, 'EUR/USD', '1h', { limit: 500 });
    const [url] = fetchMock.mock.calls[0]!;
    expect(url).toBe('/api/v1/klines/EUR%2FUSD?interval=1h&limit=500');
    expect(bars).toHaveLength(2);
    expect(bars[0]?.time).toBeLessThan(bars[1]!.time);
  });

  it('returns [] for an empty history window (UDF no-data)', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(
      new Response(JSON.stringify({ symbol: 'EUR/USD', interval: '1h', count: 0, data: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = new ApiClient({ baseUrl: '/api/v1', fetchImpl: fetchMock });
    expect(await getBars(api, 'EUR/USD', '1h')).toEqual([]);
  });
});
