/**
 * TCA — transaction-cost analysis (Task 10.5.3.22 item 3).
 *
 *   GET /api/v1/reports/tca/{account_id}?period=daily|monthly|quarterly
 *       &instrument_class=…&from=&to=
 *
 * The endpoint is account-scoped (a foreign account id is FORBIDDEN);
 * buckets carry `symbol`, so the order-inspect card filters the account
 * report down to the inspected order's instrument — an honest
 * per-symbol view, not a fabricated per-order one.
 */
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';

import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import { useSessionStore } from '@/lib/auth/session';
import { ErrorBox, Field, cardCls, selectCls, tableCls, tdCls, thCls } from '@/lib/ui';
import { useState } from 'react';

type Api = Pick<ApiClient, 'get'>;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined =>
  typeof v === 'string' ? v : typeof v === 'number' ? String(v) : undefined;

export interface TcaBucket {
  bucket_start?: string;
  symbol?: string;
  fills?: number;
  avg_slip_arrival_bps?: string;
  avg_slip_vwap_bps?: string;
  avg_slip_fix_bps?: string;
  avg_price_improvement_delta?: string;
}

function parseBucket(v: unknown): TcaBucket | null {
  if (!isRecord(v)) return null;
  return {
    bucket_start: str(v['bucket_start']),
    symbol: str(v['symbol']),
    fills:
      typeof v['fills'] === 'number' && Number.isFinite(v['fills'])
        ? (v['fills'])
        : undefined,
    avg_slip_arrival_bps: str(v['avg_slip_arrival_bps']),
    avg_slip_vwap_bps: str(v['avg_slip_vwap_bps']),
    avg_slip_fix_bps: str(v['avg_slip_fix_bps']),
    avg_price_improvement_delta: str(v['avg_price_improvement_delta']),
  };
}

export interface TcaQuery {
  period?: string;
  instrumentClass?: string;
  from?: string;
  to?: string;
}

export async function fetchTca(
  accountId: number,
  q: TcaQuery = {},
  api: Api = apiClient,
): Promise<TcaBucket[]> {
  const p = new URLSearchParams();
  if (q.period !== undefined && q.period !== '') p.set('period', q.period);
  if (q.instrumentClass !== undefined && q.instrumentClass !== '')
    p.set('instrument_class', q.instrumentClass);
  if (q.from !== undefined && q.from !== '') p.set('from', q.from);
  if (q.to !== undefined && q.to !== '') p.set('to', q.to);
  const res = await api.get<unknown>(
    `/reports/tca/${accountId}${p.size > 0 ? `?${p.toString()}` : ''}`,
  );
  const rows = isRecord(res) && Array.isArray(res['buckets']) ? res['buckets'] : [];
  return rows.map(parseBucket).filter((b): b is TcaBucket => b !== null);
}

const PERIODS = ['daily', 'monthly', 'quarterly'];
const CLASSES = [
  '',
  'SPOT',
  'FORWARD',
  'SWAP',
  'NDF',
  'OPTION',
  'FX_MAJOR',
  'FX_MINOR',
  'FX_EXOTIC',
];

function BucketTable({ buckets }: { buckets: TcaBucket[] }) {
  if (buckets.length === 0) {
    return <p className="py-3 text-center text-xs text-neutral-500">No fills in this window.</p>;
  }
  return (
    <div className="overflow-x-auto">
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>Bucket</th>
            <th className={thCls}>Symbol</th>
            <th className={thCls}>Fills</th>
            <th className={thCls}>Slip vs arrival</th>
            <th className={thCls}>Slip vs VWAP</th>
            <th className={thCls}>Slip vs fix</th>
            <th className={thCls}>Improvement</th>
          </tr>
        </thead>
        <tbody>
          {buckets.map((b, i) => (
            <tr key={`${b.bucket_start ?? ''}-${b.symbol ?? i}`}>
              <td className={`${tdCls} font-mono`}>
                {b.bucket_start !== undefined && b.bucket_start !== ''
                  ? new Date(b.bucket_start).toLocaleDateString('en-US')
                  : '—'}
              </td>
              <td className={`${tdCls} font-mono`}>{b.symbol ?? '—'}</td>
              <td className={`${tdCls} font-mono`}>{b.fills ?? '—'}</td>
              <td className={`${tdCls} font-mono`}>{b.avg_slip_arrival_bps ?? '—'}</td>
              <td className={`${tdCls} font-mono`}>{b.avg_slip_vwap_bps ?? '—'}</td>
              <td className={`${tdCls} font-mono`}>{b.avg_slip_fix_bps ?? '—'}</td>
              <td className={`${tdCls} font-mono`}>{b.avg_price_improvement_delta ?? '—'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** Compact card for order inspect — the account report filtered to the
 * order's symbol. Account-scoped per the backend contract; labeled so. */
export function TcaCard({ symbol, api = apiClient }: { symbol: string; api?: Api }) {
  const accountId = useSessionStore((s) => s.user?.accountId ?? null);
  const q = useQuery({
    queryKey: ['tca', accountId, 'daily'],
    queryFn: () => fetchTca(accountId ?? 0, { period: 'daily' }, api),
    enabled: accountId !== null,
    retry: false,
  });
  const buckets = (q.data ?? []).filter((b) => b.symbol === symbol);
  return (
    <section aria-label="Transaction cost analysis" className="space-y-1">
      <div className="flex items-baseline justify-between">
        <h4 className="text-xs font-semibold text-neutral-300">TCA (account, daily)</h4>
        <Link to="/reports" className="text-xs text-sky-400 hover:underline">
          Full report
        </Link>
      </div>
      {accountId === null ? (
        <p className="text-xs text-neutral-500">Sign in to view execution-quality stats.</p>
      ) : q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : buckets.length === 0 ? (
        <p className="text-xs text-neutral-500">No {symbol} fills in the current daily buckets.</p>
      ) : (
        <BucketTable buckets={buckets} />
      )}
    </section>
  );
}

export function TcaPanel() {
  const accountId = useSessionStore((s) => s.user?.accountId ?? null);
  const [period, setPeriod] = useState('daily');
  const [klass, setKlass] = useState('');
  const q = useQuery({
    queryKey: ['tca', accountId, period, klass],
    queryFn: () =>
      fetchTca(accountId ?? 0, {
        period,
        instrumentClass: klass === '' ? undefined : klass,
      }),
    enabled: accountId !== null,
    retry: false,
  });
  return (
    <section aria-label="TCA report" className={cardCls}>
      <div className="mb-2 flex flex-wrap items-end gap-2">
        <h2 className="mr-auto text-sm font-semibold">Transaction-cost analysis</h2>
        <Field label="Period">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={period}
              onChange={(e) => {
                setPeriod(e.target.value);
              }}
            >
              {PERIODS.map((p) => (
                <option key={p}>{p}</option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Instrument class">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={klass}
              onChange={(e) => {
                setKlass(e.target.value);
              }}
            >
              {CLASSES.map((c) => (
                <option key={c} value={c}>
                  {c === '' ? 'All classes' : c}
                </option>
              ))}
            </select>
          )}
        </Field>
      </div>
      {accountId === null ? (
        <p className="text-xs text-neutral-500">Sign in to view execution-quality stats.</p>
      ) : q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : (
        <BucketTable buckets={q.data} />
      )}
    </section>
  );
}
