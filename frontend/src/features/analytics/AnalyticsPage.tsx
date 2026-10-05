/**
 * Market analytics & intelligence page (Task 10.5.3.21).
 *
 *   SentimentPanel — OI / long-short / taker-flow series with the
 *                    Phase-23 5-min publication-delay badges and
 *                    suppressed-cohort rows rendered explicitly.
 *   PositioningCard — /market/positioning?symbol= delayed cohort.
 *   VenuePanel — /stats/24h + /market/performance + /analytics/volume
 *                    + /analytics/stats fill rates.
 *   DepthPanel — /market/depth L2 snapshot + on-demand L3
 *                    (TierProfessional; 402/403 honest).
 *   MyPnlCard — /analytics/pnl, rendered only when signed in.
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { ApiError } from '@/lib/api';

import { apiClient } from '@/app/runtime';
import { useSessionStore } from '@/lib/auth/session';
import { ErrorBox, Field, cardCls, inputCls, tableCls, tdCls, thCls } from '@/lib/ui';

import { fetchAccountPnl, fetchPositioning } from './api';
import { DepthPanel } from './DepthPanel';
import { SentimentPanel } from './SentimentPanel';
import { VenuePanel } from './VenuePanel';

function PositioningCard({ symbol }: { symbol: string }) {
  const q = useQuery({
    queryKey: ['analytics', 'positioning', symbol],
    queryFn: () => fetchPositioning(symbol, apiClient),
    enabled: symbol !== '',
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  return (
    <section aria-label="Positioning" className={cardCls}>
      <h2 className="mb-1 flex items-center gap-2 text-sm font-semibold">
        Positioning
        {q.data?.delayed === true ? (
          <span
            role="status"
            className="rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] text-amber-400"
          >
            delayed {q.data.delay_ms ?? 0}ms
          </span>
        ) : null}
      </h2>
      {q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : q.data?.insufficient_data === true ? (
        <p className="text-xs text-neutral-500">
          Insufficient account cohort — publication withheld.
        </p>
      ) : (
        <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs">
          <dt className="text-neutral-500">Accounts</dt>
          <dd className="font-mono">{q.data?.accounts ?? '—'}</dd>
          <dt className="text-neutral-500">Long / short</dt>
          <dd className="font-mono">
            {q.data?.long_accounts ?? '—'} / {q.data?.short_accounts ?? '—'}
          </dd>
          <dt className="text-neutral-500">Long notional</dt>
          <dd className="font-mono">{q.data?.long_notional ?? '—'}</dd>
          <dt className="text-neutral-500">Short notional</dt>
          <dd className="font-mono">{q.data?.short_notional ?? '—'}</dd>
          <dt className="text-neutral-500">Gross</dt>
          <dd className="font-mono">{q.data?.gross_notional ?? '—'}</dd>
        </dl>
      )}
    </section>
  );
}

function MyPnlCard() {
  const q = useQuery({
    queryKey: ['analytics', 'pnl'],
    queryFn: () => fetchAccountPnl(),
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  return (
    <section aria-label="Account P&L analytics" className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">My P&L analytics</h2>
      {q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : q.data.length === 0 ? (
        <p className="text-xs text-neutral-500">No P&L rows in the window.</p>
      ) : (
        <div className="max-h-40 overflow-y-auto" tabIndex={0}>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Day</th>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Realized</th>
                <th className={thCls}>Unrealized</th>
                <th className={thCls}>Fees</th>
                <th className={thCls}>Net</th>
              </tr>
            </thead>
            <tbody>
              {q.data.map((r, i) => (
                <tr key={i}>
                  <td className={`${tdCls} font-mono`}>{r.day ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{r.symbol ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{r.realized ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{r.unrealized ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{r.fees ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{r.net ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export default function AnalyticsPage() {
  const signedIn = useSessionStore((s) => s.user !== null);
  const [symbol, setSymbol] = useState('EUR/USD');

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold text-neutral-100">Market analytics</h1>
          <p className="text-sm text-neutral-500">
            Positioning, open interest, taker flow, venue stats and book snapshots.
          </p>
        </div>
        <Field label="Symbol">
          {(id) => (
            <input
              id={id}
              className={inputCls}
              value={symbol}
              onChange={(e) => {
                setSymbol(e.target.value.toUpperCase());
              }}
            />
          )}
        </Field>
      </header>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <SentimentPanel symbol={symbol} />
        <div className="space-y-4">
          <PositioningCard symbol={symbol} />
          <DepthPanel symbol={symbol} />
          {signedIn ? <MyPnlCard /> : null}
        </div>
      </div>
      <VenuePanel />
    </div>
  );
}
