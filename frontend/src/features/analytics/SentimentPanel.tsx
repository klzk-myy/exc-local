/**
 * SentimentPanel — per-symbol open interest, long/short ratio and
 * taker-flow series. Publication-delay metadata is rendered verbatim
 * (Phase-23 §23.3.10): the `delayed` flag and `as_of_ms` horizon are
 * shown as a badge; suppressed cohort buckets (<100 accounts) render
 * as explicit "suppressed" rows — never silently dropped.
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { ApiError } from '@/lib/api';

import { apiClient } from '@/app/runtime';
import { ErrorBox, Field, cardCls, selectCls, tableCls, tdCls, thCls } from '@/lib/ui';

import {
  fetchLongShort,
  fetchMarketTakerVolume,
  fetchOpenInterest,
  type LongShortPoint,
  type TakerFlowPoint,
} from './api';

const OI_INTERVALS = ['1h', '4h', '1d'] as const;
const SENTIMENT_PERIODS = ['5m', '15m', '1h', '4h', '1d'] as const;

function DelayBadge({ delayed, asOfMs }: { delayed?: boolean; asOfMs?: number }) {
  if (delayed !== true) return null;
  return (
    <span
      role="status"
      className="rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] font-medium text-amber-400"
    >
      5-min delayed
      {asOfMs !== undefined ? ` · as of ${new Date(asOfMs).toISOString().slice(11, 19)}` : ''}
    </span>
  );
}

export function SentimentPanel({ symbol }: { symbol: string }) {
  const [interval, setInterval] = useState<string>('1h');
  const [period, setPeriod] = useState<string>('1h');

  const oi = useQuery({
    queryKey: ['analytics', 'oi', symbol, interval],
    queryFn: () => fetchOpenInterest(symbol, interval, apiClient),
    enabled: symbol !== '',
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  const ls = useQuery({
    queryKey: ['analytics', 'long-short', symbol, period],
    queryFn: () => fetchLongShort(symbol, period, apiClient),
    enabled: symbol !== '',
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  const tf = useQuery({
    queryKey: ['analytics', 'taker-flow', symbol, period],
    queryFn: () => fetchMarketTakerVolume(symbol, apiClient),
    enabled: symbol !== '',
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });

  const lsPoints = (ls.data?.points ?? []).slice(-12);
  const tfPoints = (tf.data?.points ?? []).slice(-12);

  return (
    <section aria-label="Sentiment & positioning" className={`${cardCls} space-y-3`}>
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-sm font-semibold">Sentiment — {symbol}</h2>
        <Field label="OI interval">
          {(id) => (
            <select
              id={id}
              className={`${selectCls} !w-auto py-0.5 text-xs`}
              value={interval}
              onChange={(e) => {
                setInterval(e.target.value);
              }}
            >
              {OI_INTERVALS.map((i) => (
                <option key={i} value={i}>
                  {i}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Sentiment period">
          {(id) => (
            <select
              id={id}
              className={`${selectCls} !w-auto py-0.5 text-xs`}
              value={period}
              onChange={(e) => {
                setPeriod(e.target.value);
              }}
            >
              {SENTIMENT_PERIODS.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          )}
        </Field>
      </div>

      {/* Open interest */}
      <div>
        <h3 className="mb-1 text-xs font-medium text-neutral-400">Open interest</h3>
        {oi.isPending ? (
          <p className="text-xs text-neutral-500">Loading…</p>
        ) : oi.isError ? (
          <ErrorBox error={oi.error} />
        ) : oi.data?.insufficient_data === true ? (
          <p className="text-xs text-neutral-500">Insufficient data for this interval.</p>
        ) : oi.data?.current != null ? (
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
            <span>
              OI <b className="font-mono">{oi.data.current.open_interest ?? '—'}</b>
            </span>
            <span>
              Notional <b className="font-mono">{oi.data.current.open_interest_notional ?? '—'}</b>
            </span>
            <span>
              Positions <b className="font-mono">{oi.data.current.positions ?? '—'}</b>
            </span>
            {oi.data.current.stale === true ? (
              <span role="alert" className="text-amber-400">
                stale
              </span>
            ) : null}
          </div>
        ) : (
          <p className="text-xs text-neutral-500">No open-interest data.</p>
        )}
      </div>

      {/* Long/short ratio */}
      <div>
        <h3 className="mb-1 flex items-center gap-2 text-xs font-medium text-neutral-400">
          Long/short ratio
          <DelayBadge delayed={ls.data?.delayed} asOfMs={ls.data?.as_of_ms} />
        </h3>
        {ls.isPending ? (
          <p className="text-xs text-neutral-500">Loading…</p>
        ) : ls.isError ? (
          <ErrorBox error={ls.error} />
        ) : ls.data?.insufficient_data === true ? (
          <p className="text-xs text-neutral-500">
            Insufficient account cohort — publication withheld.
          </p>
        ) : lsPoints.length === 0 ? (
          <p className="text-xs text-neutral-500">No ratio points.</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Bucket</th>
                <th className={thCls}>Accounts</th>
                <th className={thCls}>Long</th>
                <th className={thCls}>Short</th>
                <th className={thCls}>L/S</th>
              </tr>
            </thead>
            <tbody>
              {lsPoints.map((p: LongShortPoint, i) => (
                <tr key={i}>
                  <td className={`${tdCls} font-mono`}>
                    {p.bucket_start_ms !== undefined
                      ? new Date(p.bucket_start_ms).toISOString().slice(11, 16)
                      : '—'}
                    {p.open === true ? ' (open)' : ''}
                  </td>
                  {p.suppressed === true ? (
                    <td className={tdCls} colSpan={4}>
                      <span className="text-neutral-500">suppressed — cohort below floor</span>
                    </td>
                  ) : (
                    <>
                      <td className={tdCls}>{p.accounts ?? '—'}</td>
                      <td className={`${tdCls} font-mono`}>{p.long_ratio ?? '—'}</td>
                      <td className={`${tdCls} font-mono`}>{p.short_ratio ?? '—'}</td>
                      <td className={`${tdCls} font-mono`}>{p.long_short_ratio ?? '—'}</td>
                    </>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Taker flow */}
      <div>
        <h3 className="mb-1 flex items-center gap-2 text-xs font-medium text-neutral-400">
          Taker flow
          <DelayBadge delayed={tf.data?.delayed} asOfMs={tf.data?.as_of_ms} />
        </h3>
        {tf.isPending ? (
          <p className="text-xs text-neutral-500">Loading…</p>
        ) : tf.isError ? (
          <ErrorBox error={tf.error} />
        ) : tfPoints.length === 0 ? (
          <p className="text-xs text-neutral-500">No flow points.</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Bucket</th>
                <th className={thCls}>Trades</th>
                <th className={thCls}>Buy vol</th>
                <th className={thCls}>Sell vol</th>
                <th className={thCls}>B/S ratio</th>
              </tr>
            </thead>
            <tbody>
              {tfPoints.map((p: TakerFlowPoint, i) => (
                <tr key={i}>
                  <td className={`${tdCls} font-mono`}>
                    {p.bucket_start_ms !== undefined
                      ? new Date(p.bucket_start_ms).toISOString().slice(11, 16)
                      : '—'}
                  </td>
                  {p.suppressed === true ? (
                    <td className={tdCls} colSpan={4}>
                      <span className="text-neutral-500">suppressed — cohort below floor</span>
                    </td>
                  ) : (
                    <>
                      <td className={tdCls}>{p.trades ?? '—'}</td>
                      <td className={`${tdCls} font-mono`}>{p.buy_volume ?? '—'}</td>
                      <td className={`${tdCls} font-mono`}>{p.sell_volume ?? '—'}</td>
                      <td className={`${tdCls} font-mono`}>{p.buy_sell_ratio ?? '—'}</td>
                    </>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </section>
  );
}
