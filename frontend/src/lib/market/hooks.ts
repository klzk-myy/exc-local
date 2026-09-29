/**
 * Shared query hooks for trading surfaces.
 */
import { useQuery } from '@tanstack/react-query';

import type { ApiClient } from '@/lib/api';

import { fetchInstruments } from './api';
import type { Instrument } from './wire';

/** Instrument reference data — 60s stale time mirrors the upstream
 * 1-minute instruments cache (Task 5.3.5). Returns a symbol→row map. */
export function useInstruments(api: ApiClient): {
  bySymbol: Map<string, Instrument>;
  isLoading: boolean;
  isError: boolean;
} {
  const q = useQuery({
    queryKey: ['market', 'instruments'],
    queryFn: () => fetchInstruments(api),
    staleTime: 60_000,
    refetchInterval: 60_000,
  });
  const bySymbol = new Map<string, Instrument>();
  for (const i of q.data ?? []) bySymbol.set(i.symbol, i);
  return { bySymbol, isLoading: q.isLoading, isError: q.isError };
}
