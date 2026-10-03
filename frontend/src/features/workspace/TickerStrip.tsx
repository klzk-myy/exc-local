/**
 * Ticker strip — the workspace's symbol context header (MT5 Market
 * Watch / Binance symbol bar role):
 *
 *   - Symbol switcher groups the account watchlist first, then every
 *     instrument; selection writes the order-draft store — the same
 *     seam order-book/depth clicks use — so all panels rebind at once.
 *   - 24h rolling stats come from `ticker@{symbol}` (parseTicker24h);
 *     live spread from `bbo@{symbol}` via the shared market store the
 *     workspace already subscribes (useMarketFeed) — no second feed.
 *   - The star toggles the canonical per-account watchlist
 *     (lib/alerts/watchlist) shared with the Discovery page.
 */
import { useState } from 'react';

import { wsClient } from '@/app/runtime';
import { useWatchlist, useWatchlistStore } from '@/lib/alerts';
import { useSessionStore } from '@/lib/auth/session';
import { formatPrice, metaFor, useInstruments } from '@/lib/input-helpers';
import { tickerChannel } from '@/lib/market/channels';
import { parseTicker24h, type Ticker24h } from '@/lib/market/wire';
import { selectCls } from '@/lib/ui';
import { useBbo } from '@/lib/trading/marketStore';
import { useOrderDraft } from '@/lib/trading/orderDraft';
import { useChannel, type WsClient } from '@/lib/ws';

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <span className="flex items-baseline gap-1">
      <span className="text-[10px] uppercase tracking-wide text-neutral-500">{label}</span>
      <span className="font-mono text-xs text-neutral-200">{value}</span>
    </span>
  );
}

export function TickerStrip({
  symbol,
  client = wsClient,
}: {
  symbol: string;
  client?: WsClient;
}) {
  const accountId = useSessionStore((s) => s.user?.accountId ?? null);
  const setSymbol = useOrderDraft((s) => s.setSymbol);
  const { instruments } = useInstruments();
  const meta = metaFor(instruments, symbol);
  const watchlist = useWatchlist(accountId);
  const toggleWatch = useWatchlistStore((s) => s.toggle);

  const [ticker, setTicker] = useState<Ticker24h | null>(null);
  const bbo = useBbo(symbol);

  // The strip re-keys on symbol — stale 24h stats for the previous pair
  // must never render under the new label.
  const [tickerFor, setTickerFor] = useState(symbol);
  if (tickerFor !== symbol) {
    setTickerFor(symbol);
    setTicker(null);
  }
  useChannel(client, tickerChannel(symbol), (frame) => {
    const t = parseTicker24h(frame.data);
    if (t) setTicker(t);
  });

  const watched = watchlist.includes(symbol);
  const pct = ticker?.priceChangePct ?? '';
  const pctNum = pct === '' ? 0 : Number(pct);
  const pctCls =
    pct === '' ? 'text-neutral-400' : pctNum >= 0 ? 'text-emerald-400' : 'text-red-400';
  const bid = bbo?.bid;
  const ask = bbo?.ask;
  const twoSided = bid !== undefined && ask !== undefined;
  const spread = twoSided ? ask.sub(bid).toString() : null;
  const allSymbols = [...instruments.values()].map((m) => m.symbol).sort();

  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1" role="group" aria-label="Ticker">
      <span className="flex items-center gap-1">
        <button
          type="button"
          aria-label={watched ? `Remove ${symbol} from watchlist` : `Add ${symbol} to watchlist`}
          aria-pressed={watched}
          className={`p-1 text-sm leading-none ${watched ? 'text-amber-400' : 'text-neutral-600 hover:text-neutral-300'}`}
          onClick={() => toggleWatch(accountId, symbol)}
        >
          ★
        </button>
        <label className="sr-only" htmlFor="ws-symbol">
          Symbol
        </label>
        <select
          id="ws-symbol"
          aria-label="Symbol"
          value={symbol}
          onChange={(e) => setSymbol(e.target.value)}
          className={`${selectCls} w-auto py-0.5 font-mono text-xs font-semibold`}
        >
          {watchlist.length > 0 && (
            <optgroup label="Watchlist">
              {watchlist.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </optgroup>
          )}
          <optgroup label="All instruments">
            {allSymbols
              .filter((s) => !watchlist.includes(s))
              .map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
          </optgroup>
          {symbol !== '' && !allSymbols.includes(symbol) && (
            <option value={symbol}>{symbol}</option>
          )}
        </select>
      </span>
      <span className={`font-mono text-sm font-semibold ${pctCls}`}>
        {ticker !== null ? formatPrice(meta, ticker.close) : '—'}
      </span>
      <span className={`font-mono text-xs ${pctCls}`}>
        {pct !== '' ? `${pctNum >= 0 ? '+' : ''}${pctNum.toFixed(2)}%` : ''}
      </span>
      <Stat label="24h High" value={ticker?.high ? formatPrice(meta, ticker.high) : '—'} />
      <Stat label="24h Low" value={ticker?.low ? formatPrice(meta, ticker.low) : '—'} />
      <Stat
        label="24h Vol"
        value={ticker?.volume ? Number(ticker.volume).toLocaleString() : '—'}
      />
      <Stat
        label="Spread"
        value={
          spread !== null
            ? formatPrice(meta, spread)
            : '—'
        }
      />
      {twoSided && (
        <span className="font-mono text-xs text-neutral-400">
          {formatPrice(meta, bid.toString())} / {formatPrice(meta, ask.toString())}
        </span>
      )}
    </div>
  );
}
