/**
 * Per-account watchlists (Phase-10 Task 10.3.17).
 *
 * The route registry (services/internal/gateway/routes_v1.go) has no
 * server-side watchlist endpoint — the spec's persistence requirement is
 * therefore satisfied with localStorage, namespaced per account so two
 * accounts on one browser never see each other's lists. If a
 * `/api/v1/account/watchlists` surface lands later, this module is the
 * seam to swap.
 */
import { create } from 'zustand';

const KEY_PREFIX = 'exc.watchlist.v1.';

function storageKey(accountId: number | null): string {
  return `${KEY_PREFIX}${accountId ?? 'anon'}`;
}

function load(accountId: number | null): string[] {
  if (typeof window === 'undefined') return [];
  try {
    const raw = window.localStorage.getItem(storageKey(accountId));
    if (raw === null) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((s): s is string => typeof s === 'string');
  } catch {
    return [];
  }
}

function save(accountId: number | null, symbols: string[]): void {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(storageKey(accountId), JSON.stringify(symbols));
  } catch {
    // storage full/blocked — the in-memory list still works this session
  }
}

interface WatchlistState {
  /** Symbol lists keyed by account id (number) or 'anon'. */
  lists: Record<string, string[]>;
  /** Add a symbol (deduped, order preserved). */
  add: (accountId: number | null, symbol: string) => void;
  remove: (accountId: number | null, symbol: string) => void;
  toggle: (accountId: number | null, symbol: string) => void;
  /** Move a symbol earlier/later in the list (ordering matters to users). */
  reorder: (accountId: number | null, fromIdx: number, toIdx: number) => void;
}

const keyOf = (accountId: number | null): string => String(accountId ?? 'anon');

export const useWatchlistStore = create<WatchlistState>()((set, get) => {
  const write = (accountId: number | null, symbols: string[]) => {
    save(accountId, symbols);
    set((s) => ({ lists: { ...s.lists, [keyOf(accountId)]: symbols } }));
  };
  return {
    lists: {},
    add: (accountId, symbol) => {
      const cur = get().lists[keyOf(accountId)] ?? load(accountId);
      if (cur.includes(symbol)) return;
      write(accountId, [...cur, symbol]);
    },
    remove: (accountId, symbol) => {
      const cur = get().lists[keyOf(accountId)] ?? load(accountId);
      write(
        accountId,
        cur.filter((s) => s !== symbol),
      );
    },
    toggle: (accountId, symbol) => {
      const cur = get().lists[keyOf(accountId)] ?? load(accountId);
      write(accountId, cur.includes(symbol) ? cur.filter((s) => s !== symbol) : [...cur, symbol]);
    },
    reorder: (accountId, fromIdx, toIdx) => {
      const cur = [...(get().lists[keyOf(accountId)] ?? load(accountId))];
      if (fromIdx < 0 || fromIdx >= cur.length || toIdx < 0 || toIdx >= cur.length) return;
      const [moved] = cur.splice(fromIdx, 1);
      if (moved === undefined) return;
      cur.splice(toIdx, 0, moved);
      write(accountId, cur);
    },
  };
});

/** Reactive selector — returns the account's list, hydrating from
 * localStorage into the store on first read. */
export function useWatchlist(accountId: number | null): string[] {
  const key = keyOf(accountId);
  const list = useWatchlistStore((s) => s.lists[key]);
  if (list === undefined) {
    const hydrated = load(accountId);
    // Populate lazily outside render — setState-in-render is a loop risk;
    // schedule on a microtask so StrictMode double-render is safe.
    queueMicrotask(() => {
      useWatchlistStore.setState((s) =>
        s.lists[key] === undefined ? { lists: { ...s.lists, [key]: hydrated } } : s,
      );
    });
    return hydrated;
  }
  return list;
}
