/**
 * History explorer (Task 10.5.3.22) — cursor-paginated browsing over
 * the Phase-23 historical tape: trades / ticks / klines / block-trades
 * per symbol, swap-rate sheets, and async export enqueue.
 *
 * Every page renders the envelope's access metadata ({access_tier,
 * delayed, degraded}) — a free-tier 15-minute-delayed tape is labeled,
 * never passed off as realtime.
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { useSessionStore } from '@/lib/auth/session';
import {
  ErrorBox,
  Field,
  btnGhost,
  btnPrimary,
  cardCls,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import {
  enqueueExport,
  fetchBlockTrades,
  fetchKlines,
  fetchSwapRates,
  fetchTicks,
  fetchTrades,
  type AccessMeta,
  type BlockTapeRow,
  type KlineRow,
  type Page,
  type SwapRateRow,
  type TapeRow,
} from './api';

const DATASETS = [
  { id: 'trades', label: 'Trades' },
  { id: 'ticks', label: 'Ticks' },
  { id: 'klines', label: 'Klines' },
  { id: 'block-trades', label: 'Block trades' },
  { id: 'swap-rates', label: 'Swap rates' },
] as const;
type Dataset = (typeof DATASETS)[number]['id'];

/** The 12 persisted kline intervals (analytics.PersistedIntervalLabels). */
const INTERVALS = ['1m', '5m', '15m', '30m', '1h', '2h', '4h', '6h', '8h', '1D', '1W', '1M'];

function toRfc3339(local: string): string | undefined {
  if (local === '') return undefined;
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

function MetaBadges({ meta }: { meta: AccessMeta | undefined }) {
  if (!meta) return null;
  return (
    <span className="inline-flex gap-1 align-middle" role="status">
      {meta.access_tier !== undefined ? (
        <span className="rounded bg-neutral-700/60 px-1.5 py-0.5 text-[10px] text-neutral-300">
          tier {meta.access_tier}
        </span>
      ) : null}
      {meta.delayed === true ? (
        <span className="rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] text-amber-400">
          delayed tape
        </span>
      ) : null}
      {meta.degraded === true ? (
        <span className="rounded bg-red-500/15 px-1.5 py-0.5 text-[10px] text-red-400">
          tier resolution degraded
        </span>
      ) : null}
    </span>
  );
}

const fmtTs = (iso?: string, ms?: number) =>
  iso !== undefined && iso !== ''
    ? new Date(iso).toLocaleString('en-US', { hour12: false })
    : ms !== undefined
      ? new Date(ms).toLocaleString('en-US', { hour12: false })
      : '—';

function TapeTable({ rows }: { rows: TapeRow[] }) {
  return (
    <table className={tableCls}>
      <thead>
        <tr>
          <th className={thCls}>Time</th>
          <th className={thCls}>Trade</th>
          <th className={thCls}>Side</th>
          <th className={thCls}>Price</th>
          <th className={thCls}>Qty</th>
          <th className={thCls}>Seq</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r, i) => (
          <tr key={r.trade_id ?? r.event_seq ?? i}>
            <td className={`${tdCls} font-mono`}>{fmtTs(r.time, r.time_ms)}</td>
            <td className={`${tdCls} font-mono`}>{r.trade_id ?? '—'}</td>
            <td className={tdCls}>{r.side ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.price ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.quantity ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.event_seq ?? '—'}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function KlineTable({ rows }: { rows: KlineRow[] }) {
  return (
    <table className={tableCls}>
      <thead>
        <tr>
          <th className={thCls}>Open time</th>
          <th className={thCls}>O</th>
          <th className={thCls}>H</th>
          <th className={thCls}>L</th>
          <th className={thCls}>C</th>
          <th className={thCls}>Volume</th>
          <th className={thCls}>Trades</th>
          <th className={thCls}>Closed</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r, i) => (
          <tr key={r.open_time_ms ?? i}>
            <td className={`${tdCls} font-mono`}>{fmtTs(undefined, r.open_time_ms)}</td>
            <td className={`${tdCls} font-mono`}>{r.open ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.high ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.low ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.close ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.volume ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.trade_count ?? '—'}</td>
            <td className={tdCls}>{r.closed === false ? 'forming' : 'yes'}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function BlockTable({ rows }: { rows: BlockTapeRow[] }) {
  return (
    <table className={tableCls}>
      <thead>
        <tr>
          <th className={thCls}>Pub time</th>
          <th className={thCls}>Kind</th>
          <th className={thCls}>Print</th>
          <th className={thCls}>Price</th>
          <th className={thCls}>Qty</th>
          <th className={thCls}>Notional USD</th>
          <th className={thCls}>Delay</th>
          <th className={thCls}>Lineage</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r, i) => (
          <tr key={r.entry_id ?? i} className={r.bust === true ? 'opacity-60' : ''}>
            <td className={`${tdCls} font-mono`}>{fmtTs(r.pub_ts)}</td>
            <td className={`${tdCls} font-mono`}>{r.kind ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.block_trade_id ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.price ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.quantity ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.notional_usd ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>
              {r.delay_ms !== undefined ? `${r.delay_ms}ms` : '—'}
            </td>
            <td className={`${tdCls} text-xs`}>
              {r.bust === true ? 'BUST ' : ''}
              {r.corrected_by !== undefined ? `corrected by #${r.corrected_by} ` : ''}
              {r.supersedes !== undefined ? `supersedes #${r.supersedes}` : ''}
              {r.bust !== true && r.corrected_by === undefined && r.supersedes === undefined
                ? '—'
                : ''}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function SwapTable({ rows }: { rows: SwapRateRow[] }) {
  return (
    <table className={tableCls}>
      <thead>
        <tr>
          <th className={thCls}>Effective</th>
          <th className={thCls}>Symbol</th>
          <th className={thCls}>Long pts</th>
          <th className={thCls}>Short pts</th>
          <th className={thCls}>Markup (L/S)</th>
          <th className={thCls}>Days</th>
          <th className={thCls}>Accruals</th>
          <th className={thCls}>Source</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r, i) => (
          <tr key={`${r.symbol ?? ''}-${r.effective_date ?? i}`}>
            <td className={`${tdCls} font-mono`}>
              {r.effective_date ?? '—'}
              {r.triple === true ? (
                <span className="ml-1 rounded bg-sky-500/15 px-1 text-[10px] text-sky-300">×3</span>
              ) : null}
            </td>
            <td className={`${tdCls} font-mono`}>{r.symbol ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.long_points ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.short_points ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>
              {r.long_markup_bps ?? '—'} / {r.short_markup_bps ?? '—'}
            </td>
            <td className={`${tdCls} font-mono`}>{r.days_applied ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{r.accrual_count ?? '—'}</td>
            <td className={tdCls}>{r.source ?? '—'}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** Accumulating cursor pager — keeps fetched pages and appends via the
 * envelope's next_cursor (keyset, no offset drift). */
function useCursorPages<T>(
  run: (cursor?: string) => Promise<Page<T>>,
  depsKey: string,
  enabled: boolean,
) {
  const [extra, setExtra] = useState<{ key: string; pages: Page<T>[]; err?: unknown }>({
    key: '',
    pages: [],
  });
  const first = useQuery({
    queryKey: ['explorer', depsKey],
    queryFn: () => run(undefined),
    enabled,
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  const pages = extra.key === depsKey ? extra.pages : [];
  const last = pages.length > 0 ? pages[pages.length - 1] : first.data;
  const rows = [...(first.data?.rows ?? []), ...pages.flatMap((p) => p.rows)];
  const loadMore = () => {
    const cursor = last?.nextCursor;
    if (cursor === null || cursor === undefined) return;
    void run(cursor)
      .then((p) => {
        setExtra((s) => ({
          key: depsKey,
          pages: [...(s.key === depsKey ? s.pages : []), p],
        }));
      })
      .catch((err: unknown) => {
        setExtra((s) => (s.key === depsKey ? { ...s, err } : { key: depsKey, pages: [], err }));
      });
  };
  return {
    first,
    rows,
    meta: first.data?.meta,
    nextCursor: last?.nextCursor ?? null,
    loadMore,
    moreError: extra.key === depsKey ? extra.err : undefined,
  };
}

export default function HistoryExplorerPage() {
  const signedIn = useSessionStore((s) => s.user !== null);
  const [dataset, setDataset] = useState<Dataset>('trades');
  const [symbol, setSymbol] = useState('EUR/USD');
  const [interval, setInterval] = useState('1h');
  const [from, setFrom] = useState('');
  const [to, setTo] = useState('');
  const [applied, setApplied] = useState({ symbol: 'EUR/USD', from: '', to: '' });
  const [exportMsg, setExportMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const symbolNeeded = dataset !== 'swap-rates';
  const depsKey = `${dataset}|${applied.symbol}|${applied.from}|${applied.to}|${interval}`;

  const query = {
    symbol: applied.symbol,
    from: toRfc3339(applied.from),
    to: toRfc3339(applied.to),
    limit: 200,
  };
  const pager = useCursorPages<unknown>(
    (cursor) => {
      const q = { ...query, cursor };
      switch (dataset) {
        case 'trades':
          return fetchTrades(q);
        case 'ticks':
          return fetchTicks(q);
        case 'klines':
          return fetchKlines({ ...q, interval });
        case 'block-trades':
          return fetchBlockTrades(q);
        case 'swap-rates':
          return fetchSwapRates({
            from: q.from,
            to: q.to,
            limit: q.limit,
            cursor,
            symbol: applied.symbol === '' ? undefined : applied.symbol,
          });
      }
    },
    depsKey,
    symbolNeeded ? applied.symbol !== '' : true,
  );

  const exportable = dataset === 'trades' || dataset === 'ticks' || dataset === 'klines';
  const runExport = (format: 'csv' | 'json' | 'parquet') => {
    if (!signedIn) {
      setExportMsg({ ok: false, text: 'Sign in to queue exports.' });
      return;
    }
    setExportMsg(null);
    void enqueueExport(
      {
        symbol: applied.symbol,
        kind: dataset as 'trades' | 'ticks' | 'klines',
        format,
        interval: dataset === 'klines' ? interval : undefined,
        from: query.from,
        to: query.to,
      },
      apiClient,
    )
      .then((job) => {
        setExportMsg(
          job !== null
            ? {
                ok: true,
                text: `Export job #${job.id} queued — track it under Reports → Downloads.`,
              }
            : { ok: true, text: 'Export queued.' },
        );
      })
      .catch((e: unknown) => {
        setExportMsg({
          ok: false,
          text: e instanceof ApiError ? `${e.code}: ${e.message}` : 'Export request failed.',
        });
      });
  };

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">History explorer</h1>
        <p className="text-sm text-neutral-500">
          Archived tape, candles, block prints and swap sheets — tier-delayed views are labeled by
          the envelope, never silently realtime.
        </p>
      </header>

      <form
        className={`${cardCls} flex flex-wrap items-end gap-2`}
        onSubmit={(e) => {
          e.preventDefault();
          setApplied({ symbol, from, to });
        }}
      >
        <Field label="Dataset">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={dataset}
              onChange={(e) => {
                setDataset(e.target.value as Dataset);
                setApplied({ symbol, from, to });
              }}
            >
              {DATASETS.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.label}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Symbol">
          {(id) => (
            <input
              id={id}
              className={inputCls}
              value={symbol}
              onChange={(e) => {
                setSymbol(e.target.value.toUpperCase());
              }}
              placeholder="EUR/USD"
              required={symbolNeeded}
            />
          )}
        </Field>
        {dataset === 'klines' ? (
          <Field label="Interval">
            {(id) => (
              <select
                id={id}
                className={selectCls}
                value={interval}
                onChange={(e) => {
                  setInterval(e.target.value);
                }}
              >
                {INTERVALS.map((iv) => (
                  <option key={iv}>{iv}</option>
                ))}
              </select>
            )}
          </Field>
        ) : null}
        <Field label="From">
          {(id) => (
            <input
              id={id}
              type="datetime-local"
              className={inputCls}
              value={from}
              onChange={(e) => {
                setFrom(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="To">
          {(id) => (
            <input
              id={id}
              type="datetime-local"
              className={inputCls}
              value={to}
              onChange={(e) => {
                setTo(e.target.value);
              }}
            />
          )}
        </Field>
        <button type="submit" className={btnPrimary}>
          Load
        </button>
        {exportable ? (
          <>
            <button type="button" className={btnGhost} onClick={() => runExport('csv')}>
              Export CSV
            </button>
            <button type="button" className={btnGhost} onClick={() => runExport('json')}>
              Export JSON
            </button>
          </>
        ) : null}
      </form>
      {exportMsg !== null ? (
        <p
          role={exportMsg.ok ? 'status' : 'alert'}
          className={`text-xs ${exportMsg.ok ? 'text-emerald-400' : 'text-red-400'}`}
        >
          {exportMsg.text}
        </p>
      ) : null}

      <section aria-label="History rows" className={cardCls}>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-sm font-semibold">
            {DATASETS.find((d) => d.id === dataset)?.label} <MetaBadges meta={pager.meta} />
          </h2>
          {pager.first.data?.total !== undefined ? (
            <span className="text-xs text-neutral-500">total {pager.first.data.total}</span>
          ) : null}
        </div>
        {pager.first.isPending ? (
          <p className="text-xs text-neutral-500">Loading…</p>
        ) : pager.first.isError ? (
          <ErrorBox error={pager.first.error} />
        ) : pager.rows.length === 0 ? (
          <p className="py-4 text-center text-xs text-neutral-500">No rows in this window.</p>
        ) : (
          <div className="overflow-x-auto">
            {dataset === 'klines' ? (
              <KlineTable rows={pager.rows as KlineRow[]} />
            ) : dataset === 'block-trades' ? (
              <BlockTable rows={pager.rows as BlockTapeRow[]} />
            ) : dataset === 'swap-rates' ? (
              <SwapTable rows={pager.rows as SwapRateRow[]} />
            ) : (
              <TapeTable rows={pager.rows as TapeRow[]} />
            )}
          </div>
        )}
        {pager.moreError !== undefined ? <ErrorBox error={pager.moreError} /> : null}
        {pager.nextCursor !== null && !pager.first.isError ? (
          <button type="button" className={`${btnGhost} mt-2`} onClick={pager.loadMore}>
            Load more
          </button>
        ) : null}
      </section>
    </div>
  );
}
