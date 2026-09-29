import { describe, expect, it } from 'vitest';

import type { BookLevel, BookSnapshotWire, DepthUpdate } from '../../lib/market/wire';
import {
  aggregateLevels,
  applyDepthUpdate,
  applySnapshot,
  deriveBookView,
  diffLevels,
} from './book';

const lvl = (price: string, qty = '1', count?: number): BookLevel => ({ price, qty, count });
const snap = (over: Partial<BookSnapshotWire> = {}): BookSnapshotWire => ({
  symbol: 'EUR/USD',
  bids: [lvl('1.0850', '1000'), lvl('1.0849', '2500')],
  asks: [lvl('1.0852', '800'), lvl('1.0853', '1000')],
  seq: 10,
  depth: 20,
  updatedAtMs: 1_700_000_000_000,
  ...over,
});
const upd = (over: Partial<DepthUpdate> = {}): DepthUpdate => ({
  symbol: 'EUR/USD',
  bids: [],
  asks: [],
  seq: 11,
  lastSeq: 11,
  firstSeq: 11,
  prevLastSeq: 10,
  engineSeq: 0,
  coalesced: 0,
  crc32: undefined,
  isSnapshot: false,
  tsMs: 1_700_000_000_100,
  ...over,
});

describe('aggregateLevels', () => {
  it('merges same-price levels (qty + count summed)', () => {
    const out = aggregateLevels([lvl('1.0850', '100', 2), lvl('1.0850', '50', 3)], 'bids');
    expect(out).toEqual([{ price: '1.0850', qty: '150', count: 5 }]);
  });
  it('drops qty-zero levels', () => {
    expect(aggregateLevels([lvl('1.0850', '0'), lvl('1.0849', '5')], 'bids')).toHaveLength(1);
  });
  it('sorts bids descending, asks ascending', () => {
    const bids = aggregateLevels([lvl('1.07'), lvl('1.09'), lvl('1.08')], 'bids');
    expect(bids.map((l) => l.price)).toEqual(['1.09', '1.08', '1.07']);
    const asks = aggregateLevels([lvl('1.09'), lvl('1.07'), lvl('1.08')], 'asks');
    expect(asks.map((l) => l.price)).toEqual(['1.07', '1.08', '1.09']);
  });
});

describe('applySnapshot / applyDepthUpdate', () => {
  it('snapshot seeds symbol + levels capped at depth', () => {
    const s = applySnapshot(snap(), 1);
    expect(s.symbol).toBe('EUR/USD');
    expect(s.bids).toHaveLength(1);
    expect(s.seq).toBe(10);
  });
  it('replace semantics — frame carries the full level set', () => {
    const s = applySnapshot(snap(), 20);
    const next = applyDepthUpdate(
      s,
      upd({ bids: [lvl('1.0851', '500')], asks: [lvl('1.0852', '900')], lastSeq: 11 }),
      20,
    );
    expect(next.bids).toEqual([{ price: '1.0851', qty: '500', count: 0 }]);
    expect(next.seq).toBe(11);
  });
  it('seq guard drops stale/replayed frames', () => {
    const s = applySnapshot(snap(), 20);
    const stale = applyDepthUpdate(
      s,
      upd({ bids: [lvl('9.99')], lastSeq: 10 }), // <= current seq
      20,
    );
    expect(stale).toBe(s);
  });
  it('rejects frames for a different symbol', () => {
    const s = applySnapshot(snap(), 20);
    const other = applyDepthUpdate(s, upd({ symbol: 'GBP/USD', lastSeq: 99 }), 20);
    expect(other).toBe(s);
  });
  it('thin books do not pad to the depth level', () => {
    const s = applySnapshot(snap({ bids: [lvl('1.0850')] }), 20);
    expect(s.bids).toHaveLength(1);
  });
});

describe('deriveBookView', () => {
  const state = applySnapshot(snap(), 20);
  it('cumulative totals per side', () => {
    const v = deriveBookView(state);
    expect(v.bids.map((r) => r.total)).toEqual(['1000', '3500']);
    expect(v.asks.map((r) => r.total)).toEqual(['800', '1800']);
  });
  it('depthFrac is normalized to the cross-side max', () => {
    const v = deriveBookView(state);
    expect(v.bids[1]?.depthFrac).toBeCloseTo(1, 6); // 3500/3500
    expect(v.asks[0]?.depthFrac).toBeCloseTo(800 / 3500, 6);
  });
  it('spread + mid are exact decimals', () => {
    const v = deriveBookView(state);
    expect(v.spread?.toString()).toBe('0.0002');
    expect(v.mid?.toString()).toBe('1.0851');
  });
  it('empty side → no spread/mid (§6.6 sparse book)', () => {
    const v = deriveBookView(applySnapshot(snap({ asks: [] }), 20));
    expect(v.spread).toBeNull();
    expect(v.mid).toBeNull();
  });
});

describe('diffLevels (flash)', () => {
  const before = applySnapshot(snap(), 20);
  it('marks grown/new levels up, shrunk/removed down', () => {
    const next = applyDepthUpdate(
      before,
      upd({
        bids: [lvl('1.0850', '1500'), lvl('1.0848', '10')], // grew + new
        asks: [lvl('1.0852', '400'), lvl('1.0853', '1000')], // shrank, kept
        lastSeq: 12,
      }),
      20,
    );
    const d = diffLevels(before, next);
    expect(d.get('bid:1.0850')).toBe('up');
    expect(d.get('bid:1.0848')).toBe('up');
    expect(d.get('bid:1.0849')).toBe('down'); // vanished
    expect(d.get('ask:1.0852')).toBe('down');
    expect(d.has('ask:1.0853')).toBe(false); // unchanged
  });
});
