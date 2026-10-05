/**
 * Public venue transparency (Task 10.5.3.23) — no auth required.
 *
 *   GET /exchange-info    venue document: symbols, permissions,
 *                         trading hours, product profiles, leverage ceilings
 *   GET /venue/best-execution/rts27[/{id}/csv]  quarterly exec quality
 *   GET /venue/best-execution/rts28[/{id}/csv]  annual top-5 venues
 *   GET /meta/pagination · /meta/rate-limits    published API contracts
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { saveBlob } from '@/lib/input-helpers';
import { ErrorBox, Field, btnGhost, cardCls, selectCls, tableCls, tdCls, thCls } from '@/lib/ui';

import {
  downloadBestExecCsv,
  fetchBestExec,
  fetchPaginationMeta,
  fetchRateLimitsMeta,
  fetchVenueInfo,
} from './api';

const retryPublic = (n: number, e: unknown) => !(e instanceof ApiError && e.status < 500) && n < 2;

const SETTLE = ['T+0', 'T+1', 'T+2'];

function VenueDocCard() {
  const q = useQuery({
    queryKey: ['public', 'venue-info'],
    queryFn: () => fetchVenueInfo(apiClient),
    refetchInterval: 60_000,
    retry: retryPublic,
  });
  if (q.isPending) return <p className="text-xs text-neutral-500">Loading venue document…</p>;
  if (q.isError) return <ErrorBox error={q.error} />;
  const doc = q.data;
  const th = doc.trading_hours;
  return (
    <div className="space-y-4">
      <section aria-label="Venue info" className={cardCls}>
        <h2 className="mb-1 text-sm font-semibold">Venue information</h2>
        <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs md:grid-cols-4">
          <dt className="text-neutral-500">Trading hours</dt>
          <dd className="font-mono">
            {th?.type ?? '—'} ({th?.weekly_open_utc ?? '—'} → {th?.weekly_close_utc ?? '—'})
          </dd>
          <dt className="text-neutral-500">Daily break</dt>
          <dd className="font-mono">{th?.daily_break_utc ?? '—'}</dd>
          <dt className="text-neutral-500">Timezone</dt>
          <dd className="font-mono">{doc.timezone ?? '—'}</dd>
          <dt className="text-neutral-500">Symbols</dt>
          <dd className="font-mono">{doc.symbols.length}</dd>
        </dl>
      </section>

      {(doc.leverage_policies ?? []).length > 0 ? (
        <section aria-label="Leverage ceilings" className={cardCls}>
          <h2 className="mb-1 text-sm font-semibold">Leverage ceilings</h2>
          <p className="mb-2 text-xs text-neutral-500">
            Effective per-entity caps published under spec §13.14 — the maximum a client of each
            category may negotiate.
          </p>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Entity</th>
                <th className={thCls}>Category</th>
                <th className={thCls}>Instrument group</th>
                <th className={thCls}>Max leverage</th>
                <th className={thCls}>Effective</th>
              </tr>
            </thead>
            <tbody>
              {(doc.leverage_policies ?? []).map((p, i) => (
                <tr key={i}>
                  <td className={`${tdCls} font-mono`}>{p.entity_code ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{p.client_category ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{p.instrument_group ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{p.max_leverage ?? '—'}:1</td>
                  <td className={`${tdCls} font-mono`}>{p.effective_from ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      ) : null}

      {(doc.product_profiles ?? []).length > 0 ? (
        <section aria-label="Product profiles" className={cardCls}>
          <h2 className="mb-1 text-sm font-semibold">Product profiles</h2>
          <ul className="space-y-1 text-xs">
            {(doc.product_profiles ?? []).map((p, i) => (
              <li key={i} className="font-mono text-neutral-300">
                {p.code ?? '—'} · {p.pricing_plan ?? '—'} · scope {p.instrument_scope ?? '—'} · min
                deposit {p.min_deposit ?? '—'}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <section aria-label="Instrument directory" className={cardCls}>
        <h2 className="mb-1 text-sm font-semibold">Instrument directory</h2>
        <div className="overflow-x-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Type</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Tick</th>
                <th className={thCls}>Lot</th>
                <th className={thCls}>Min qty</th>
                <th className={thCls}>Max leverage</th>
                <th className={thCls}>Settlement</th>
                <th className={thCls}>Orders</th>
              </tr>
            </thead>
            <tbody>
              {doc.symbols.map((s) => (
                <tr key={s.symbol}>
                  <td className={`${tdCls} font-mono`}>{s.symbol ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{s.instrument_type ?? '—'}</td>
                  <td className={tdCls}>{s.status ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{s.tick_size ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{s.lot_size ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{s.min_order_qty ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{s.max_leverage ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>
                    {s.settlement ?? SETTLE[s.settlement_cycle ?? -1] ?? '—'}
                  </td>
                  <td className={`${tdCls} text-xs`}>
                    {s.permissions?.new_orders_allowed === false
                      ? 'closed to new orders'
                      : (s.order_types ?? []).join(', ') || '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
}

function BestExecCard() {
  const [kind, setKind] = useState<'rts27' | 'rts28'>('rts27');
  const [dlErr, setDlErr] = useState<unknown>(null);
  const q = useQuery({
    queryKey: ['public', 'best-exec', kind],
    queryFn: () => fetchBestExec(kind, apiClient),
    retry: retryPublic,
  });
  return (
    <section aria-label="Best execution reports" className={cardCls}>
      <div className="mb-2 flex flex-wrap items-end gap-2">
        <h2 className="mr-auto text-sm font-semibold">Best execution (MiFID II)</h2>
        <Field label="Report type">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={kind}
              onChange={(e) => {
                setKind(e.target.value as 'rts27' | 'rts28');
              }}
            >
              <option value="rts27">RTS 27 — quarterly execution quality</option>
              <option value="rts28">RTS 28 — annual top venues</option>
            </select>
          )}
        </Field>
      </div>
      {q.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : q.isError ? (
        <ErrorBox error={q.error} />
      ) : q.data.length === 0 ? (
        <p className="text-xs text-neutral-500">
          No published reports yet — only PUBLISHED artifacts are listed here.
        </p>
      ) : (
        <div className="overflow-x-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>{kind === 'rts27' ? 'Quarter' : 'Year'}</th>
                <th className={thCls}>Class</th>
                <th className={thCls}>Version</th>
                <th className={thCls}>Status</th>
                {kind === 'rts27' ? <th className={thCls}>Days</th> : null}
                <th className={thCls}>Published</th>
                <th className={thCls}></th>
              </tr>
            </thead>
            <tbody>
              {q.data.map((r) => (
                <tr key={r.id}>
                  <td className={`${tdCls} font-mono`}>
                    {kind === 'rts27' ? (r.quarter_start ?? '—').slice(0, 10) : (r.year ?? '—')}
                    {r.zero_activity === true ? (
                      <span className="ml-1 rounded bg-neutral-700/60 px-1 text-[10px] text-neutral-400">
                        zero activity
                      </span>
                    ) : null}
                  </td>
                  <td className={`${tdCls} font-mono`}>{r.instrument_class ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>v{r.version ?? '—'}</td>
                  <td className={tdCls}>{r.status ?? '—'}</td>
                  {kind === 'rts27' ? (
                    <td className={`${tdCls} font-mono`}>{r.days_covered ?? '—'}</td>
                  ) : null}
                  <td className={`${tdCls} font-mono`}>
                    {r.published_at !== undefined ? r.published_at.slice(0, 10) : '—'}
                  </td>
                  <td className={tdCls}>
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => {
                        setDlErr(null);
                        void downloadBestExecCsv(kind, r.id).then(saveBlob).catch(setDlErr);
                      }}
                    >
                      CSV
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {dlErr !== null ? <ErrorBox error={dlErr} /> : null}
    </section>
  );
}

function MetaCard() {
  const pag = useQuery({
    queryKey: ['public', 'meta-pagination'],
    queryFn: () => fetchPaginationMeta(apiClient),
    retry: retryPublic,
  });
  const rl = useQuery({
    queryKey: ['public', 'meta-rate-limits'],
    queryFn: () => fetchRateLimitsMeta(apiClient),
    retry: retryPublic,
  });
  const tiers = Object.entries(rl.data?.tiers ?? {});
  return (
    <section aria-label="API contracts" className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">API contract</h2>
      {pag.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : pag.isError ? (
        <ErrorBox error={pag.error} />
      ) : (
        <div className="space-y-2 text-xs">
          <p className="font-mono text-neutral-400">
            envelope {pag.data.envelope?.shape ?? '—'} · order{' '}
            {pag.data.envelope?.cursor_order ?? '—'}
            {pag.data.envelope?.cursor_opaque === true ? ' · opaque cursor' : ''}
          </p>
          <div className="max-h-44 overflow-y-auto">
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>List endpoint</th>
                  <th className={thCls}>Default</th>
                  <th className={thCls}>Max</th>
                  <th className={thCls}>Filters</th>
                </tr>
              </thead>
              <tbody>
                {pag.data.endpoints.map((e, i) => (
                  <tr key={i}>
                    <td className={`${tdCls} font-mono`}>{e.path ?? '—'}</td>
                    <td className={`${tdCls} font-mono`}>{e.default_limit ?? '—'}</td>
                    <td className={`${tdCls} font-mono`}>{e.max_limit ?? '—'}</td>
                    <td className={`${tdCls} font-mono text-xs`}>
                      {(e.filterable ?? []).join(', ') || '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      {rl.isError ? (
        <ErrorBox error={rl.error} />
      ) : tiers.length > 0 ? (
        <div className="mt-3">
          <h3 className="mb-1 text-xs font-semibold text-neutral-300">Rate-limit tiers</h3>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Tier</th>
                <th className={thCls}>req/s</th>
                <th className={thCls}>burst ×</th>
                <th className={thCls}>weight/min</th>
                <th className={thCls}>keyed by</th>
              </tr>
            </thead>
            <tbody>
              {tiers.map(([name, t]) => (
                <tr key={name}>
                  <td className={`${tdCls} font-mono`}>{name}</td>
                  <td className={`${tdCls} font-mono`}>{t.rate_per_sec ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{t.burst_factor ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{t.weight_per_min ?? '—'}</td>
                  <td className={`${tdCls} font-mono`}>{t.keyed_by ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  );
}

export default function TransparencyPage() {
  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">Venue transparency</h1>
        <p className="text-sm text-neutral-500">
          Instrument directory, leverage ceilings, published MiFID II best-execution reports and the
          machine-readable API contract — all public.
        </p>
      </header>
      <VenueDocCard />
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <BestExecCard />
        <MetaCard />
      </div>
    </div>
  );
}
