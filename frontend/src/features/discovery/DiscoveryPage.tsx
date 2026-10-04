/**
 * FX market discovery (Phase-10 Task 10.3.17) — instrument browser +
 * per-account watchlists + client-side rate alerts.
 *
 * Honesty notes (surfaced in the UI):
 *   - "Session" filter = the venue-wide 24/5 FX session gate
 *     (lib/market/tradingHours) — instruments carry no per-symbol
 *     session calendar.
 *   - The "spread" filter applies the configured `max_spread_pips`
 *     instrument cap (reference data), NOT the live spread — labeled.
 *   - Rate alerts are evaluated client-side against live ticks while
 *     this page (or its subscriptions) is open — there is no server-side
 *     alerting endpoint (Phase-05 route registry), and the UI says so.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiClient, wsClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import { useSessionStore } from '@/lib/auth/session';
import { fetchInstruments } from '@/lib/market/api';
import type { Instrument } from '@/lib/market/wire';
import { isFxMarketOpen } from '@/lib/market/tradingHours';
import { tickerChannel } from '@/lib/market/channels';
import { useChannel, useWsStatus } from '@/lib/ws';
import type { WsClient } from '@/lib/ws';
import {
  alertSatisfied,
  useRateAlerts,
  useRateAlertStore,
  useWatchlist,
  useWatchlistStore,
  type RateAlert,
} from '@/lib/alerts';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  inputCls,
  labelCls,
  tableCls,
  tdCls,
  thCls,
  ErrorBox,
  StatusBadge,
} from '@/lib/ui';
import { useNow } from '@/lib/ui';

import { observablePrice } from './wire';

type Ticks = Record<string, number | undefined>;

/** Subscribes one symbol's ticker channel for the page's lifetime and
 * reports the observable price up to the parent. */
function SymbolTicker({
  ws,
  symbol,
  onTick,
}: {
  ws: WsClient;
  symbol: string;
  onTick: (symbol: string, price: number) => void;
}) {
  useChannel(ws, tickerChannel(symbol), (frame) => {
    const px = observablePrice('data' in frame ? frame.data : frame);
    if (px !== null) onTick(symbol, px);
  });
  return null;
}

export default function DiscoveryPage({
  api = apiClient,
  ws = wsClient,
}: {
  api?: ApiClient;
  ws?: WsClient;
}) {
  const accountId = useSessionStore((s) => s.user?.accountId ?? null);
  const instruments = useQuery({
    queryKey: ['instruments'],
    queryFn: () => fetchInstruments(api),
    retry: false,
    staleTime: 60_000,
  });

  const [search, setSearch] = useState('');
  const [typeFilter, setTypeFilter] = useState('');
  const [sessionOnly, setSessionOnly] = useState(false);
  const [maxSpreadCap, setMaxSpreadCap] = useState('');
  const [watchlistOnly, setWatchlistOnly] = useState(false);
  const [ticks, setTicks] = useState<Ticks>({});

  const wsStatus = useWsStatus(ws);
  const now = useNow(30_000);
  const marketOpen = isFxMarketOpen(now);
  const watchlist = useWatchlist(accountId);
  const watchlistStore = useWatchlistStore;

  const onTick = useCallback((symbol: string, price: number) => {
    setTicks((prev) => (prev[symbol] === price ? prev : { ...prev, [symbol]: price }));
  }, []);

  const types = useMemo(
    () => [...new Set((instruments.data ?? []).map((i) => i.instrumentType))].sort(),
    [instruments.data],
  );

  const filtered = useMemo(() => {
    const needle = search.trim().toUpperCase();
    const spreadCap = Number(maxSpreadCap);
    const watch = new Set(watchlist);
    return (instruments.data ?? []).filter((i) => {
      if (watchlistOnly && !watch.has(i.symbol)) return false;
      if (typeFilter !== '' && i.instrumentType !== typeFilter) return false;
      if (needle !== '' && !i.symbol.toUpperCase().includes(needle)) return false;
      if (sessionOnly && !marketOpen) return false;
      if (maxSpreadCap !== '' && Number.isFinite(spreadCap)) {
        const cap = Number(i.maxSpreadPips ?? '');
        if (!Number.isFinite(cap) || cap > spreadCap) return false;
      }
      return true;
    });
  }, [
    instruments.data,
    search,
    typeFilter,
    sessionOnly,
    marketOpen,
    maxSpreadCap,
    watchlistOnly,
    watchlist,
  ]);

  // Symbols we subscribe to for live evaluation: watchlist + alert symbols.
  const alerts = useRateAlerts(accountId);
  const liveSymbols = useMemo(
    () => [...new Set([...watchlist, ...alerts.map((a) => a.symbol)])],
    [watchlist, alerts],
  );

  return (
    <div className="mx-auto max-w-6xl p-6">
      {/* live tick plumbing — one subscription per watched symbol */}
      {liveSymbols.map((sym) => (
        <SymbolTicker key={sym} ws={ws} symbol={sym} onTick={onTick} />
      ))}

      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <h1 className="text-2xl font-semibold">Market discovery</h1>
        <span
          className={`rounded px-2 py-0.5 text-xs font-medium ${marketOpen ? 'bg-emerald-500/20 text-emerald-400' : 'bg-neutral-700/40 text-neutral-400'}`}
        >
          FX session {marketOpen ? 'open' : 'closed'} (24/5)
        </span>
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <section className={`${cardCls} lg:col-span-2`} aria-label="Instrument browser">
          <div className="mb-3 grid grid-cols-2 gap-2 md:grid-cols-4">
            <input
              aria-label="Search instruments"
              placeholder="Search symbol…"
              className={inputCls}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
            <select
              aria-label="Instrument group (type)"
              className={inputCls}
              value={typeFilter}
              onChange={(e) => setTypeFilter(e.target.value)}
            >
              <option value="">All types</option>
              {types.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
            <input
              aria-label="Max spread cap (pips)"
              placeholder="Max spread cap (pips)"
              className={inputCls}
              value={maxSpreadCap}
              onChange={(e) => setMaxSpreadCap(e.target.value)}
              title="Filters by the instrument's configured max_spread_pips — not the live spread."
            />
            <label className="flex items-center gap-2 text-xs text-neutral-400">
              <input
                type="checkbox" className="h-6 w-6"
                checked={sessionOnly}
                onChange={(e) => setSessionOnly(e.target.checked)}
              />
              Session open now
            </label>
          </div>
          <label className="mb-2 flex items-center gap-2 text-xs text-neutral-400">
            <input
              type="checkbox" className="h-6 w-6"
              checked={watchlistOnly}
              onChange={(e) => setWatchlistOnly(e.target.checked)}
            />
            Watchlist only
          </label>
          <ErrorBox error={instruments.error} />
          <div className="relative overflow-x-auto" tabIndex={0}>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}><span className="sr-only">Watchlist</span></th>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Type</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Spread cap (pips)</th>
                <th className={thCls}>Max lev</th>
                <th className={thCls}>Settles</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((i) => (
                <tr key={i.symbol}>
                  <td className={tdCls}>
                    <button
                      type="button"
                      aria-label={`${watchlist.includes(i.symbol) ? 'Remove' : 'Add'} ${i.symbol} ${watchlist.includes(i.symbol) ? 'from' : 'to'} watchlist`}
                      className="inline-flex min-h-6 min-w-6 items-center justify-center text-amber-400"
                      onClick={() => watchlistStore.getState().toggle(accountId, i.symbol)}
                    >
                      {watchlist.includes(i.symbol) ? '★' : '☆'}
                    </button>
                  </td>
                  <td className={tdCls}>{i.symbol}</td>
                  <td className={tdCls}>{i.instrumentType}</td>
                  <td className={tdCls}>
                    <StatusBadge value={i.status} />
                  </td>
                  <td className={tdCls}>{i.maxSpreadPips ?? '—'}</td>
                  <td className={tdCls}>{i.maxLeverage > 0 ? `${i.maxLeverage}×` : '—'}</td>
                  <td className={tdCls}>{i.settlement}</td>
                </tr>
              ))}
              {filtered.length === 0 && !instruments.isLoading && (
                <tr>
                  <td className={tdCls} colSpan={7}>
                    No instruments match the current filters.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
          </div>
        </section>

        <div className="space-y-4">
          <WatchlistPanel
            accountId={accountId}
            watchlist={watchlist}
            ticks={ticks}
            connected={wsStatus.state === 'AUTHENTICATED'}
          />
          <AlertsPanel
            accountId={accountId}
            alerts={alerts}
            ticks={ticks}
            instruments={instruments.data ?? []}
          />
        </div>
      </div>
      <p className="mt-4 text-xs text-neutral-600">
        Live ticks update while the market-data socket is connected ({wsStatus.state}); alerts
        evaluate locally — there is no server-side alerting service (see panel note).
      </p>
    </div>
  );
}

function WatchlistPanel({
  accountId,
  watchlist,
  ticks,
  connected,
}: {
  accountId: number | null;
  watchlist: string[];
  ticks: Ticks;
  connected: boolean;
}) {
  const store = useWatchlistStore;
  return (
    <section className={cardCls} aria-label="Watchlist">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">
        Watchlist {accountId !== null ? `(account ${accountId})` : '(local — not signed in)'}
      </h2>
      {watchlist.length === 0 && (
        <p className="text-sm text-neutral-500">
          Star instruments in the browser to pin them here.
        </p>
      )}
      <ul className="space-y-1">
        {watchlist.map((sym) => (
          <li key={sym} className="flex items-center justify-between text-sm">
            <span>{sym}</span>
            <span className="flex items-center gap-2">
              <span className="tabular-nums text-neutral-300" data-testid={`px-${sym}`}>
                {ticks[sym] !== undefined ? ticks[sym].toFixed(5) : '—'}
              </span>
              <button
                type="button"
                aria-label={`Remove ${sym} from watchlist`}
                className={btnGhost}
                onClick={() => store.getState().remove(accountId, sym)}
              >
                ×
              </button>
            </span>
          </li>
        ))}
      </ul>
      {!connected && watchlist.length > 0 && (
        <p className="mt-2 text-xs text-amber-400">
          Prices unavailable — market data disconnected.
        </p>
      )}
    </section>
  );
}

function AlertsPanel({
  accountId,
  alerts,
  ticks,
  instruments,
}: {
  accountId: number | null;
  alerts: RateAlert[];
  ticks: Ticks;
  instruments: Instrument[];
}) {
  const store = useRateAlertStore;
  const [symbol, setSymbol] = useState('');
  const [direction, setDirection] = useState<'above' | 'below'>('above');
  const [target, setTarget] = useState('');
  const [fired, setFired] = useState<string[]>([]);

  // Evaluate the alert set whenever a tick lands — mark triggered so
  // alerts are one-shot.
  useEffect(() => {
    const newlyFired: string[] = [];
    for (const a of alerts) {
      if (a.triggeredAtMs !== undefined) continue;
      const px = ticks[a.symbol];
      if (px === undefined) continue;
      if (alertSatisfied(a, px)) {
        store.getState().markTriggered(accountId, a.id, px.toString(), Date.now());
        newlyFired.push(`${a.symbol} ${a.direction} ${a.targetPrice}`);
      }
    }
    if (newlyFired.length > 0) setFired((prev) => [...prev, ...newlyFired]);
  }, [ticks, alerts, accountId, store]);

  const add = () => {
    const px = Number(target);
    if (symbol === '' || !Number.isFinite(px) || px <= 0) return;
    store.getState().addAlert(accountId, {
      id: `ra-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
      symbol,
      direction,
      targetPrice: target,
      createdAtMs: Date.now(),
    });
    setTarget('');
  };

  return (
    <section className={cardCls} aria-label="Rate alerts">
      <h2 className="mb-1 text-sm font-medium text-neutral-400">Rate alerts</h2>
      <p className="mb-2 text-xs text-neutral-500">
        Evaluated locally against live ticks while this app is open — the venue has no server-side
        alerting endpoint. Fired alerts are one-shot.
      </p>
      {fired.length > 0 && (
        <div
          className="mb-2 rounded border border-emerald-700/60 bg-emerald-950/40 px-3 py-2 text-xs text-emerald-300"
          role="status"
          data-testid="alert-fired"
        >
          {fired.map((f, i) => (
            <p key={i}>Alert fired: {f}</p>
          ))}
        </div>
      )}
      <form
        className="mb-3 space-y-2"
        onSubmit={(e) => {
          e.preventDefault();
          add();
        }}
      >
        <div>
          <label className={labelCls} htmlFor="ra-symbol">
            Symbol
          </label>
          <select
            id="ra-symbol"
            className={inputCls}
            value={symbol}
            onChange={(e) => setSymbol(e.target.value)}
          >
            <option value="">Select…</option>
            {instruments.map((i) => (
              <option key={i.symbol} value={i.symbol}>
                {i.symbol}
              </option>
            ))}
          </select>
        </div>
        <div className="flex gap-2">
          <select
            aria-label="Direction"
            className={inputCls}
            value={direction}
            onChange={(e) => setDirection(e.target.value === 'below' ? 'below' : 'above')}
          >
            <option value="above">≥ above</option>
            <option value="below">≤ below</option>
          </select>
          <input
            aria-label="Target price"
            placeholder="1.08500"
            className={inputCls}
            value={target}
            onChange={(e) => setTarget(e.target.value)}
          />
          <button type="submit" className={btnPrimary}>
            Add
          </button>
        </div>
      </form>
      <ul className="space-y-1 text-sm">
        {alerts.map((a) => (
          <li key={a.id} className="flex items-center justify-between">
            <span className={a.triggeredAtMs !== undefined ? 'text-neutral-500 line-through' : ''}>
              {a.symbol} {a.direction === 'above' ? '≥' : '≤'} {a.targetPrice}
              {a.triggeredAtMs !== undefined && ` — fired @ ${a.triggeredPrice ?? ''}`}
            </span>
            <button
              type="button"
              aria-label={`Delete alert ${a.id}`}
              className={btnGhost}
              onClick={() => store.getState().removeAlert(accountId, a.id)}
            >
              ×
            </button>
          </li>
        ))}
        {alerts.length === 0 && <li className="text-neutral-500">No alerts yet.</li>}
      </ul>
    </section>
  );
}
