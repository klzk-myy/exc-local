/**
 * VenuePanel — venue-wide intelligence: rolling 24h stats table,
 * /market/performance status card (held/divergent disclosed),
 * /analytics/volume buckets and /analytics/stats fill rates.
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { ApiError } from '@/lib/api';

import { apiClient } from '@/app/runtime';
import { ErrorBox, Field, cardCls, selectCls, tableCls, tdCls, thCls } from '@/lib/ui';

import { fetchPerformance, fetchStats24h, fetchVenueStats, fetchVolume } from './api';

export function VenuePanel() {
  const [granularity, setGranularity] = useState<'1h' | '1d'>('1h');

  const stats = useQuery({
    queryKey: ['analytics', '24h'],
    queryFn: () => fetchStats24h(),
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  const perf = useQuery({
    queryKey: ['analytics', 'performance'],
    queryFn: () => fetchPerformance(apiClient),
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  const vol = useQuery({
    queryKey: ['analytics', 'volume', granularity],
    queryFn: () => fetchVolume({ granularity, limit: 25 }, apiClient),
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  const venue = useQuery({
    queryKey: ['analytics', 'venue-stats'],
    queryFn: () => fetchVenueStats(apiClient),
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });

  return (
    <section aria-label="Venue statistics" className={`${cardCls} space-y-3`}>
      <h2 className="text-sm font-semibold">Venue</h2>

      {/* 24h rolling stats */}
      <div>
        <h3 className="mb-1 text-xs font-medium text-neutral-400">
          Rolling 24h
          {stats.data?.serverTimeMs !== undefined
            ? ` · server ${new Date(stats.data.serverTimeMs).toISOString().slice(11, 19)}`
            : ''}
        </h3>
        {stats.isPending ? (
          <p className="text-xs text-neutral-500">Loading…</p>
        ) : stats.isError ? (
          <ErrorBox error={stats.error} />
        ) : stats.data.rows.length === 0 ? (
          <p className="text-xs text-neutral-500">No trades in the window.</p>
        ) : (
          <div className="max-h-56 overflow-y-auto" tabIndex={0}>
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Symbol</th>
                  <th className={thCls}>Last</th>
                  <th className={thCls}>Chg %</th>
                  <th className={thCls}>Volume</th>
                  <th className={thCls}>Trades</th>
                </tr>
              </thead>
              <tbody>
                {stats.data.rows.map((r) => (
                  <tr key={r.symbol}>
                    <td className={`${tdCls} font-mono`}>{r.symbol}</td>
                    <td className={`${tdCls} font-mono`}>{r.last ?? '—'}</td>
                    <td className={`${tdCls} font-mono`}>{r.price_change_pct ?? '—'}</td>
                    <td className={`${tdCls} font-mono`}>{r.volume}</td>
                    <td className={tdCls}>{r.trade_count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* performance card — status/held/divergence disclosed verbatim */}
      <div>
        <h3 className="mb-1 text-xs font-medium text-neutral-400">Performance</h3>
        {perf.isPending ? (
          <p className="text-xs text-neutral-500">Loading…</p>
        ) : perf.isError ? (
          <ErrorBox error={perf.error} />
        ) : (
          <div className="space-y-1 text-xs">
            <p>
              status{' '}
              <b className={perf.data.status === 'ok' ? 'text-emerald-400' : 'text-amber-400'}>
                {perf.data.status ?? '—'}
              </b>
              {perf.data.held_age_ms !== undefined
                ? ` · held ${Math.round(perf.data.held_age_ms / 1000)}s`
                : ''}
              {perf.data.delay_ms !== undefined && perf.data.delay_ms > 0
                ? ` · delay ${perf.data.delay_ms}ms`
                : ''}
            </p>
            {(perf.data.divergent_metrics ?? []).length > 0 ? (
              <p role="alert" className="text-amber-400">
                divergent: {(perf.data.divergent_metrics ?? []).join(', ')}
              </p>
            ) : null}
            {perf.data.venue !== undefined ? (
              <p className="font-mono">
                fill24h {perf.data.venue.fill_rate_24h ?? '—'} · fill7d{' '}
                {perf.data.venue.fill_rate_7d ?? '—'} · uptime24h{' '}
                {perf.data.venue.uptime_24h ?? '—'} · uptime30d {perf.data.venue.uptime_30d ?? '—'}
              </p>
            ) : null}
          </div>
        )}
      </div>

      {/* volume buckets */}
      <div>
        <h3 className="mb-1 flex items-center gap-2 text-xs font-medium text-neutral-400">
          Volume
          <Field label="Granularity">
            {(id) => (
              <select
                id={id}
                className={`${selectCls} !w-auto py-0.5 text-xs`}
                value={granularity}
                onChange={(e) => {
                  setGranularity(e.target.value === '1d' ? '1d' : '1h');
                }}
              >
                <option value="1h">1h</option>
                <option value="1d">1d</option>
              </select>
            )}
          </Field>
          {vol.data?.truncated === true ? (
            <span className="text-amber-400">(truncated at limit)</span>
          ) : null}
        </h3>
        {vol.isPending ? (
          <p className="text-xs text-neutral-500">Loading…</p>
        ) : vol.isError ? (
          <ErrorBox error={vol.error} />
        ) : vol.data.rows.length === 0 ? (
          <p className="text-xs text-neutral-500">No volume in the window.</p>
        ) : (
          <div className="max-h-40 overflow-y-auto" tabIndex={0}>
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Bucket</th>
                  <th className={thCls}>Symbol</th>
                  <th className={thCls}>Volume</th>
                  <th className={thCls}>Trades</th>
                </tr>
              </thead>
              <tbody>
                {vol.data.rows.map((r, i) => (
                  <tr key={i}>
                    <td className={`${tdCls} font-mono`}>{r.bucket_start ?? '—'}</td>
                    <td className={`${tdCls} font-mono`}>{r.symbol ?? '—'}</td>
                    <td className={`${tdCls} font-mono`}>{r.volume ?? '—'}</td>
                    <td className={tdCls}>{r.trade_count ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* fill rates */}
      <div>
        <h3 className="mb-1 text-xs font-medium text-neutral-400">Fill rates by symbol</h3>
        {venue.isPending ? (
          <p className="text-xs text-neutral-500">Loading…</p>
        ) : venue.isError ? (
          <ErrorBox error={venue.error} />
        ) : (venue.data.fill_rates ?? []).length === 0 ? (
          <p className="text-xs text-neutral-500">No fills in the window.</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Submitted</th>
                <th className={thCls}>Filled</th>
                <th className={thCls}>Rate</th>
              </tr>
            </thead>
            <tbody>
              {(venue.data.fill_rates ?? []).map((f, i) => (
                <tr key={i}>
                  <td className={`${tdCls} font-mono`}>{f.symbol ?? '—'}</td>
                  <td className={tdCls}>{f.orders_submitted ?? '—'}</td>
                  <td className={tdCls}>{f.orders_filled ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{f.fill_rate ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </section>
  );
}
