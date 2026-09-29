/**
 * Live market-data store (Task 10.3.8/10.3.12 consumers).
 *
 * `useMarketFeed(symbol)` subscribes the canonical Phase-06 channels —
 * `bbo@{symbol}` for top-of-book marks and `depth@{symbol}` (default
 * 20:100 variant) for the L2 depth walk — and lands frames in this
 * Zustand store. Components read via `useBbo`/`useDepthBook` and get
 * per-channel staleness from `useChannelHealth` (Task 10.3.19 surface:
 * ws health carries lastTickAt/stale flags).
 */
import { useCallback } from 'react';
import { create } from 'zustand';

import type { EventFrame, SnapshotFrame, WsClient, WsClientStatus } from '@/lib/ws';
import { useChannel, useWsStatus } from '@/lib/ws';

import { Dec } from '@/lib/decimal/decimal';
import { parseBbo, parseDepthUpdate, type Bbo, type BookSnapshot } from './types';

interface MarketState {
  bbo: Record<string, Bbo>;
  books: Record<string, BookSnapshot>;
  applyBbo: (b: Bbo) => void;
  applyBook: (s: BookSnapshot) => void;
}

export const useMarketStore = create<MarketState>()((set) => ({
  bbo: {},
  books: {},
  applyBbo: (b) => set((s) => ({ bbo: { ...s.bbo, [b.symbol]: b } })),
  applyBook: (b) => set((s) => ({ books: { ...s.books, [b.symbol]: b } })),
}));

export function useBbo(symbol: string | undefined): Bbo | undefined {
  return useMarketStore((s) => (symbol !== undefined ? s.bbo[symbol] : undefined));
}

export function useDepthBook(symbol: string | undefined): BookSnapshot | undefined {
  return useMarketStore((s) => (symbol !== undefined ? s.books[symbol] : undefined));
}

/** Mid price from the last BBO — undefined when a side is absent (one-sided
 * book) so consumers never fabricate a mark. The WeakMap memoizes one Dec
 * per Bbo identity — zustand snapshots must be referentially stable or the
 * selector would re-render forever (each add+div allocates a fresh Dec). */
const midMemo = new WeakMap<Bbo, Dec>();

export function useMidPrice(symbol: string | undefined): Dec | undefined {
  return useMarketStore((s) => {
    if (symbol === undefined) return undefined;
    const b = s.bbo[symbol];
    if (!b?.bid || !b.ask) return undefined;
    let m = midMemo.get(b);
    if (m === undefined) {
      m = b.bid.add(b.ask).div(Dec.of(2));
      midMemo.set(b, m);
    }
    return m;
  });
}

/**
 * Subscribe `bbo@{symbol}` (+ `depth@{symbol}` when `depth` is set) for the
 * component lifetime. `client` is injectable for tests — default is the
 * app singleton.
 */
export function useMarketFeed(
  symbol: string | undefined,
  client: WsClient,
  opts: { depth?: boolean | string } = {},
): void {
  const applyBbo = useMarketStore((s) => s.applyBbo);
  const applyBook = useMarketStore((s) => s.applyBook);

  const bboChannel = symbol !== undefined ? `bbo@${symbol}` : null;
  const depthChannel =
    symbol !== undefined && opts.depth !== false && opts.depth !== undefined
      ? typeof opts.depth === 'string'
        ? `depth@${symbol}:${opts.depth}`
        : `depth@${symbol}`
      : null;

  const onBbo = useCallback(
    (f: EventFrame | SnapshotFrame) => {
      const b = parseBbo(f.data);
      if (b) applyBbo(b);
    },
    [applyBbo],
  );
  const onDepth = useCallback(
    (f: EventFrame | SnapshotFrame) => {
      const b = parseDepthUpdate(f.data);
      if (b) applyBook(b);
    },
    [applyBook],
  );

  useChannel(client, bboChannel, onBbo);
  useChannel(client, depthChannel, onDepth);
}

/** Per-channel health surface (Task 10.3.19): consumers show "stale"
 * badges from these flags — never guess from render time. */
export function useChannelHealth(
  status: WsClientStatus,
  channel: string | undefined,
): { stale: boolean; resyncing: boolean; lastTickAt: number; hasData: boolean } {
  if (channel === undefined)
    return { stale: false, resyncing: false, lastTickAt: 0, hasData: false };
  const h = status.health[channel];
  if (!h) return { stale: false, resyncing: false, lastTickAt: 0, hasData: false };
  return {
    stale: h.stale,
    resyncing: h.resyncing,
    lastTickAt: h.lastTickAt,
    hasData: h.lastSeq > 0,
  };
}

export { useWsStatus };
