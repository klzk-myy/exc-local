/**
 * TanStack Query hooks for trading surfaces. Every key carries the
 * account scope (`scope` segment) so the sub-account switcher (Task
 * 10.3.7) re-scopes cached data atomically on switch.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { scopeAccountId, useAccountScope } from './accountScope';
import {
  fetchBalances,
  fetchBookSnapshot,
  fetchInstruments,
  fetchKlines,
  fetchOrders,
  fetchPositions,
  fetchSubAccounts,
} from './api';
import {
  isOpenOrder,
  type Balance,
  type BookSnapshot,
  type Instrument,
  type Kline,
  type Order,
  type Position,
  type SubAccount,
} from './types';

/** Current account scope key for cache composition. */
export function useScopeKey(): string {
  return useAccountScope((s) => s.scopeKey);
}

export function useInstruments(): UseQueryResult<Instrument[]> {
  return useQuery({
    queryKey: ['instruments'],
    queryFn: () => fetchInstruments(),
    staleTime: 60_000,
  });
}

export function useInstrument(symbol: string | undefined): Instrument | undefined {
  const q = useInstruments();
  return q.data?.find((i) => i.symbol === symbol);
}

export function useBalances(): UseQueryResult<Balance[]> {
  const scope = useScopeKey();
  return useQuery({
    queryKey: ['balances', scope],
    queryFn: () => fetchBalances({ accountId: scopeAccountId(scope) }),
  });
}

export function usePositions(): UseQueryResult<Position[]> {
  const scope = useScopeKey();
  return useQuery({
    queryKey: ['positions', scope],
    queryFn: () => fetchPositions({ accountId: scopeAccountId(scope) }),
    refetchInterval: 15_000, // WS private:positions overlays; REST reconciles
  });
}

export function useSubAccounts(): UseQueryResult<SubAccount[]> {
  return useQuery({
    queryKey: ['sub-accounts'],
    queryFn: () => fetchSubAccounts(),
    retry: 1,
  });
}

export function useOrders(filters: {
  symbol?: string;
  status?: string;
  limit?: number;
}): UseQueryResult<{ orders: Order[]; nextCursor: string | null }> {
  const scope = useScopeKey();
  return useQuery({
    queryKey: ['orders', scope, filters.symbol ?? '', filters.status ?? '', filters.limit ?? 100],
    queryFn: () =>
      fetchOrders(
        { symbol: filters.symbol, status: filters.status, limit: filters.limit },
        { accountId: scopeAccountId(scope) },
      ),
    refetchInterval: 15_000,
  });
}

/** Open orders only — the REST list API takes a single status, so the
 * open-set filter (PENDING|RESERVED|ACTIVE|PARTIALLY_FILLED, orders.go
 * OpenStatuses) applies client-side over the full recent page. */
export function useOpenOrders(symbol?: string): {
  orders: Order[];
  query: UseQueryResult<{ orders: Order[]; nextCursor: string | null }>;
} {
  const query = useOrders({ symbol, limit: 200 });
  const orders = (query.data?.orders ?? []).filter(isOpenOrder);
  return { orders, query };
}

export function useBookSnapshot(
  symbol: string | undefined,
  depth = 20,
): UseQueryResult<BookSnapshot | null> {
  return useQuery({
    queryKey: ['book', symbol ?? '', depth],
    queryFn: () => (symbol ? fetchBookSnapshot(symbol, depth) : Promise.resolve(null)),
    enabled: symbol !== undefined,
    staleTime: 0,
  });
}

export function useKlines(
  symbol: string | undefined,
  interval: string,
  limit = 120,
): UseQueryResult<Kline[]> {
  return useQuery({
    queryKey: ['klines', symbol ?? '', interval, limit],
    queryFn: () => (symbol ? fetchKlines(symbol, interval, limit) : Promise.resolve([])),
    enabled: symbol !== undefined,
    staleTime: 10_000,
  });
}
