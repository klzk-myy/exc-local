/**
 * private:positions overlay store (Tasks 10.3.13, §10.5).
 *
 * The Phase-19 producer publishes position upserts carrying
 * `adl_indicator` (1..5) plus mark/liquidation refreshes on the
 * auth-gated `private:positions` channel. REST /positions is the base
 * truth; this store holds the live overlay keyed by `symbol|side` and
 * merges are per-field — an absent adl_indicator never erases a known
 * rank, and a position absent from the overlay never fabricates one.
 */
import { useCallback } from 'react';
import { create } from 'zustand';

import type { EventFrame, SnapshotFrame, WsClient } from '@/lib/ws';
import { useChannel } from '@/lib/ws';
import { PRIVATE_CHANNELS } from '@/lib/market/channels';

import type { Dec } from '@/lib/decimal/decimal';
import { parsePrivatePositions, type Position } from './types';

export interface PositionOverlay {
  adlIndicator?: number;
  markPrice?: Dec;
  liquidationPrice?: Dec;
  unrealizedPnl?: Dec;
  tsMs: number;
}

interface PrivatePositionsState {
  overlays: Record<string, PositionOverlay>;
  apply: (symbol: string, side: string, patch: PositionOverlay) => void;
  clear: () => void;
}

export function overlayKey(symbol: string, side: string): string {
  return `${symbol}|${side}`;
}

export const usePositionOverlayStore = create<PrivatePositionsState>()((set) => ({
  overlays: {},
  apply: (symbol, side, patch) =>
    set((s) => {
      const prev = s.overlays[overlayKey(symbol, side)];
      // Per-field merge: an absent field must never erase a known value —
      // e.g. a mark refresh without adl_indicator keeps the last rank.
      const next: PositionOverlay = {
        adlIndicator: patch.adlIndicator ?? prev?.adlIndicator,
        markPrice: patch.markPrice ?? prev?.markPrice,
        liquidationPrice: patch.liquidationPrice ?? prev?.liquidationPrice,
        unrealizedPnl: patch.unrealizedPnl ?? prev?.unrealizedPnl,
        tsMs: patch.tsMs,
      };
      return { overlays: { ...s.overlays, [overlayKey(symbol, side)]: next } };
    }),
  clear: () => set({ overlays: {} }),
}));

/** Subscribe private:positions for the component lifetime. `client` is
 * injectable — pass a test WsClient; production uses the app singleton. */
export function usePrivatePositionsFeed(client: WsClient): void {
  const apply = usePositionOverlayStore((s) => s.apply);
  const onFrame = useCallback(
    (f: EventFrame | SnapshotFrame) => {
      for (const p of parsePrivatePositions(f.data)) {
        apply(p.symbol, p.side, {
          adlIndicator: p.adlIndicator,
          markPrice: p.markPrice,
          liquidationPrice: p.liquidationPrice,
          unrealizedPnl: p.unrealizedPnl,
          tsMs: Date.now(),
        });
      }
    },
    [apply],
  );
  useChannel(client, PRIVATE_CHANNELS.positions, onFrame);
}

/** Live overlay for one position (undefined = no private feed data yet). */
export function usePositionOverlay(position: Position): PositionOverlay | undefined {
  return usePositionOverlayStore((s) => s.overlays[overlayKey(position.symbol, position.side)]);
}
