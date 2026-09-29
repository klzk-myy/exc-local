/**
 * Order-book data hook — REST snapshot seed + depth@ channel frames.
 *
 *   - Seed: GET /api/v1/book/{symbol}?depth=N (§10.3, 100ms cache)
 *   - Live: `depth@{symbol}:{levels}:{cadence_ms}` frames replace the
 *     level set (server-conflated at the requested cadence, §10.1/10.2)
 *   - Snapshot frames (resume fallback / resync repair) replace the book
 *   - Stale surface: `status.health[channel]` — a silent channel flips
 *     `stale` per the ws client monitor (Task 10.3.19)
 *   - Per-level flash: signed qty deltas between consecutive frames
 *     (book.diffLevels) rendered for FLASH_MS
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import type { ApiClient } from '@/lib/api';
import { depthChannel, type DepthCadenceMs, type DepthLevels } from '@/lib/market/channels';
import { fetchBook } from '@/lib/market/api';
import {
  parseBookSnapshot,
  parseDepthUpdate,
  type BookSnapshotWire,
  type DepthUpdate,
} from '@/lib/market/wire';
import { useChannel, useWsStatus, type WsClient } from '@/lib/ws';

import {
  applyDepthUpdate,
  applySnapshot,
  deriveBookView,
  diffLevels,
  EMPTY_BOOK,
  type BookState,
  type BookView,
} from './book';

/** How long a level flashes after a qty change. */
export const FLASH_MS = 350;

export interface OrderBookData {
  view: BookView | null;
  flashes: ReadonlyMap<string, 'up' | 'down'>;
  channel: string;
  /** Channel-level staleness + resync flags (ws health surface). */
  stale: boolean;
  resyncing: boolean;
  error: string | null;
  reload: () => void;
}

export function useOrderBook(opts: {
  api: ApiClient;
  ws: WsClient;
  symbol: string;
  levels: DepthLevels;
  cadenceMs?: DepthCadenceMs;
}): OrderBookData {
  const { api, ws, symbol, levels } = opts;
  const cadenceMs = opts.cadenceMs ?? 100;
  const channel = depthChannel(symbol, levels, cadenceMs);
  const [state, setState] = useState<BookState>(EMPTY_BOOK);
  const [flashes, setFlashes] = useState<ReadonlyMap<string, 'up' | 'down'>>(new Map());
  const [error, setError] = useState<string | null>(null);
  const [reloadTick, setReloadTick] = useState(0);
  const prevState = useRef<BookState>(EMPTY_BOOK);
  const flashTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const status = useWsStatus(ws);
  const health = status.health[channel];

  const apply = useCallback((next: BookState) => {
    const diffs = diffLevels(prevState.current, next);
    prevState.current = next;
    setState(next);
    if (diffs.size > 0) {
      setFlashes(diffs);
      if (flashTimer.current) clearTimeout(flashTimer.current);
      flashTimer.current = setTimeout(() => setFlashes(new Map()), FLASH_MS);
    }
  }, []);

  // REST seed (also the manual resync path — §10.9 snapshot fallback).
  useEffect(() => {
    let live = true;
    prevState.current = EMPTY_BOOK;
    setState(EMPTY_BOOK);
    setError(null);
    fetchBook(api, symbol, levels)
      .then((snap) => {
        if (live) apply(applySnapshot(snap, levels));
      })
      .catch((e: unknown) => {
        if (live) setError(e instanceof Error ? e.message : 'book snapshot failed');
      });
    return () => {
      live = false;
    };
  }, [api, symbol, levels, reloadTick, apply]);

  useEffect(() => {
    return () => {
      if (flashTimer.current) clearTimeout(flashTimer.current);
    };
  }, []);

  useChannel(ws, channel, (frame) => {
    if (frame.type === 'snapshot') {
      const snap = parseBookSnapshot(frame.data);
      if (snap) {
        apply(applySnapshot(snap, levels));
        return;
      }
      const upd = parseDepthUpdate(frame.data);
      if (upd) apply(applyDepthUpdate(prevState.current, upd, levels));
      return;
    }
    const upd = parseDepthUpdate(frame.data);
    if (upd) apply(applyDepthUpdate(prevState.current, upd, levels));
  });

  const view = useMemo(() => (state.symbol === '' ? null : deriveBookView(state)), [state]);

  return {
    view,
    flashes,
    channel,
    stale: health?.stale ?? false,
    resyncing: health?.resyncing ?? false,
    error,
    reload: useCallback(() => setReloadTick((t) => t + 1), []),
  };
}

/** Re-export wire unions for test ergonomics. */
export type { BookSnapshotWire, DepthUpdate };
