/**
 * Portfolio data hook (Task 10.3.5) — REST seed + private WS deltas.
 *
 *   - Seed: GET /api/v1/positions + GET /api/v1/account/balances
 *   - Live: `private:positions` (position upsert/close events) and
 *     `private:balances` (ledger BALANCE_CHANGED) — the registered
 *     auth-gated channels per services/internal/ws/server.go
 *   - Staleness: `private:positions` channel health flags drive the
 *     STALE MARKS badge (mark prices ride this channel; >3s silence →
 *     badge via the ws monitor, Task 10.3.19)
 */
import { useCallback, useEffect, useState } from 'react';

import type { ApiClient } from '@/lib/api';
import { fetchBalances, fetchPositions } from '@/lib/market/api';
import { PRIVATE_CHANNELS } from '@/lib/market/channels';
import {
  parseBalanceEvent,
  parsePositionEvent,
  type BalanceRow,
  type PositionRow,
} from '@/lib/market/wire';
import { useChannel, useWsStatus, type WsClient } from '@/lib/ws';

export interface PortfolioData {
  positions: PositionRow[];
  balances: BalanceRow[];
  loading: boolean;
  error: string | null;
  /** True when the private:positions channel has been silent >3s. */
  marksStale: boolean;
  reload: () => void;
}

/** Exported for tests — position event merge semantics (upsert/close). */
export function upsertPosition(
  rows: PositionRow[],
  ev: {
    positionId?: number;
    symbol: string;
    side?: string;
    quantity?: string;
    entryPrice?: string;
    markPrice?: string;
    unrealizedPnl?: string;
    marginUsed?: string;
    liquidationPrice?: string;
    adlIndicator?: number;
    closed?: boolean;
  },
): PositionRow[] {
  const key = (r: PositionRow) =>
    ev.positionId !== undefined
      ? r.positionId === ev.positionId
      : r.symbol === ev.symbol && (ev.side === undefined || r.side === ev.side);
  const idx = rows.findIndex(key);
  if (ev.closed === true) {
    return idx === -1 ? rows : rows.filter((_, i) => i !== idx);
  }
  if (idx === -1) {
    // New position — only materialize when the event carries a full row.
    if (!ev.side || !ev.quantity || !ev.entryPrice) return rows;
    return [
      ...rows,
      {
        positionId: ev.positionId ?? 0,
        instrumentId: 0,
        symbol: ev.symbol,
        side: ev.side,
        quantity: ev.quantity,
        entryPrice: ev.entryPrice,
        markPrice: ev.markPrice,
        unrealizedPnl: ev.unrealizedPnl ?? '0',
        realizedPnl: '0',
        liquidationPrice: ev.liquidationPrice,
        marginUsed: ev.marginUsed ?? '0',
        openedAt: '',
        updatedAt: '',
      },
    ];
  }
  const cur = rows[idx];
  if (cur === undefined) return rows;
  const next: PositionRow = {
    ...cur,
    quantity: ev.quantity ?? cur.quantity,
    entryPrice: ev.entryPrice ?? cur.entryPrice,
    markPrice: ev.markPrice ?? cur.markPrice,
    unrealizedPnl: ev.unrealizedPnl ?? cur.unrealizedPnl,
    marginUsed: ev.marginUsed ?? cur.marginUsed,
    liquidationPrice: ev.liquidationPrice ?? cur.liquidationPrice,
  };
  return rows.map((r, i) => (i === idx ? next : r));
}

/** Exported for tests — balance event merge semantics (upsert by ccy). */
export function upsertBalance(
  rows: BalanceRow[],
  ev: {
    currency: string;
    available: string;
    locked: string;
    total: string;
  },
): BalanceRow[] {
  const idx = rows.findIndex((r) => r.currency === ev.currency);
  const row: BalanceRow = {
    currency: ev.currency,
    available: ev.available,
    locked: ev.locked,
    total: ev.total,
  };
  if (idx === -1) return [...rows, row].sort((a, b) => a.currency.localeCompare(b.currency));
  return rows.map((r, i) => (i === idx ? row : r));
}

export function usePortfolio(opts: { api: ApiClient; ws: WsClient }): PortfolioData {
  const { api, ws } = opts;
  const [positions, setPositions] = useState<PositionRow[]>([]);
  const [balances, setBalances] = useState<BalanceRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [reloadTick, setReloadTick] = useState(0);

  const status = useWsStatus(ws);
  const marksHealth = status.health[PRIVATE_CHANNELS.positions];

  useEffect(() => {
    let live = true;
    setLoading(true);
    setError(null);
    Promise.all([fetchPositions(api), fetchBalances(api)])
      .then(([pos, bal]) => {
        if (!live) return;
        setPositions(pos);
        setBalances(bal);
        setLoading(false);
      })
      .catch((e: unknown) => {
        if (!live) return;
        setError(e instanceof Error ? e.message : 'portfolio load failed');
        setLoading(false);
      });
    return () => {
      live = false;
    };
  }, [api, reloadTick]);

  useChannel(ws, PRIVATE_CHANNELS.positions, (frame) => {
    const ev = parsePositionEvent(frame.data);
    if (!ev) return;
    setPositions((rows) => upsertPosition(rows, ev));
  });

  useChannel(ws, PRIVATE_CHANNELS.balances, (frame) => {
    const ev = parseBalanceEvent(frame.data);
    if (!ev) return;
    setBalances((rows) => upsertBalance(rows, ev));
  });

  return {
    positions,
    balances,
    loading,
    error,
    marksStale: marksHealth?.stale ?? false,
    reload: useCallback(() => setReloadTick((t) => t + 1), []),
  };
}
