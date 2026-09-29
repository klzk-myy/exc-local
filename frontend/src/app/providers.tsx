/**
 * App providers — TanStack Query owns server state (spec §21.2); Zustand
 * owns local UI state (session store, feature stores).
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Trading data mutates constantly — retry sparingly and let the WS
      // layer be the liveness signal instead of polling storms.
      retry: 2,
      staleTime: 5_000,
      refetchOnWindowFocus: false,
    },
  },
});

export function AppProviders({ children }: { children: ReactNode }) {
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

export { queryClient };
