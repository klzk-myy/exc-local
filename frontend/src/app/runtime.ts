/**
 * App-level singletons: the shared ApiClient and WsClient.
 *
 * Wiring decisions (documented in README):
 *   - REST base: `VITE_API_URL` or same-origin `/api/v1`
 *   - WS endpoint: `VITE_WS_URL` or same-origin `/ws/v1` (wss: on https:)
 *   - tokenProvider reads the session store; the WS tokenRefresher and the
 *     REST silent-refresh policy are BOTH owned by the auth feature's
 *     `SessionRefresher` (src/lib/auth/refresh.ts, Task 10.3.21): one
 *     single-flight POST /api/v1/auth/refresh serves preemptive expiry
 *     refresh (getAuthToken), reactive 401 refresh (onUnauthorized), and
 *     the WS 4019 AUTH_EXPIRED re-auth path
 *   - refresh failure → clearSession + authEpoch bump (flushes optimistic
 *     order state) + hard-redirect to /login (spec §21.4)
 */
import { ApiClient } from '@/lib/api';
import { SessionRefresher } from '@/lib/auth/refresh';
import { useSessionStore } from '@/lib/auth/session';
import { WsClient } from '@/lib/ws';

const apiBase = import.meta.env.VITE_API_URL ?? '/api/v1';

/** The auth feature owns token lifecycle — shared by REST + WS. */
export const sessionRefresher = new SessionRefresher({
  refreshUrl: `${apiBase}/auth/refresh`,
});

export const apiClient = new ApiClient({
  baseUrl: apiBase,
  // Preemptive refresh: an expiring access token is silently swapped for a
  // fresh one before the request leaves (15min access TTL, spec §8.1).
  getAuthToken: () => sessionRefresher.accessToken(),
  onUnauthorized: () => {
    // Reactive refresh on a 401 envelope — single-flight with the
    // preemptive path; definitive failure clears + redirects to /login.
    sessionRefresher.handleUnauthorized();
  },
});

function defaultWsUrl(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws/v1`;
}

export const wsClient = new WsClient({
  url: () => import.meta.env.VITE_WS_URL ?? defaultWsUrl(),
  tokenProvider: () => useSessionStore.getState().accessToken,
  tokenRefresher: () => sessionRefresher.refreshForWs(),
  onOptimisticFlush: () => {
    // Invalidate any optimistic order state keyed on the auth epoch —
    // Wave-2 order stores read `authEpoch` for exactly this.
    useSessionStore.getState().bumpAuthEpoch();
  },
  onAuthFailure: () => {
    // The refresher already failed closed; ensure the token is gone and
    // land on /login (Task 10.3.19, spec §21.4).
    useSessionStore.getState().clearSession();
    if (!window.location.pathname.startsWith('/login')) {
      const redirect = encodeURIComponent(window.location.pathname + window.location.search);
      window.location.assign(`/login?redirect=${redirect}`);
    }
  },
  onGap: (channel, expected, got) => {
    // §10.9 gap → resync is automatic; surface for observability.
    console.warn(`[ws] seq gap on ${channel}: expected ${expected}, got ${got} — resyncing`);
  },
  onResyncRequired: (channel, reason) => {
    // No inline snapshot: consumer must refetch via REST (§10.9/§10.3).
    console.warn(`[ws] ${channel} requires resync (${reason}) — REST refetch needed`);
  },
});
