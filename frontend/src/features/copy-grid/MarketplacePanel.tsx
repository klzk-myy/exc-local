/**
 * Marketplace tab — the user-facing half of the strategy engine
 * (Task 16.3.21 + 14.3.14):
 *
 *   GET/POST/DELETE /strategies[/{id}] + pause/resume   scheduled strategies
 *   GET/POST          /strategy-templates + instantiate  approved catalog
 *   POST              /copy/strategies[/{id}/list]       author flow
 *   GET               /baskets/{op_id}                   basket op status
 *   GET               /promotions/{id}                   render-gated promo
 *
 * Admin-side template approval lives in admin-content (Task 10.5.3.16) —
 * nothing here duplicates it. There is no "my copy profiles" GET; the
 * author section renders only rows the session itself created/listed.
 */
import { Fragment, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import {
  ErrorBox,
  Field,
  StatusBadge,
  btnGhost,
  btnPrimary,
  cardCls,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import * as api from './api';

const FIAT = ['USD', 'EUR', 'GBP', 'CHF', 'JPY', 'CAD', 'AUD', 'SEK', 'NOK'] as const;

// ---------- scheduled strategies -------------------------------------------

function StrategyDetailRows({ id }: { id: number }) {
  const detail = useQuery({
    queryKey: ['strategies', id],
    queryFn: () => api.strategyDetail(apiClient, id),
  });
  if (detail.isPending) return <p className="text-xs text-neutral-500">Loading runs…</p>;
  if (detail.isError) return <ErrorBox error={detail.error} />;
  const runs = detail.data?.runs ?? [];
  if (runs.length === 0)
    return <p className="text-xs text-neutral-500">No runs recorded for this strategy.</p>;
  return (
    <table className={`${tableCls} mt-2`}>
      <thead>
        <tr>
          <th className={thCls}>Run</th>
          <th className={thCls}>Scheduled</th>
          <th className={thCls}>Status</th>
          <th className={thCls}>Legs</th>
          <th className={thCls}>Notional</th>
        </tr>
      </thead>
      <tbody>
        {runs.map((r) => (
          <tr key={r.run_id}>
            <td className={tdCls}>#{r.run_id}</td>
            <td className={tdCls}>
              {r.scheduled_for !== undefined ? new Date(r.scheduled_for).toLocaleString() : '—'}
            </td>
            <td className={tdCls}>
              <StatusBadge value={r.status} />
              {r.skip_reason !== undefined && r.skip_reason !== '' && (
                <span className="ml-1 text-xs text-neutral-500">{r.skip_reason}</span>
              )}
            </td>
            <td className={tdCls}>{r.legs?.length ?? r.order_ids?.length ?? 0}</td>
            <td className={tdCls}>
              {r.notional ?? '—'} {r.currency ?? ''}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function MyStrategies() {
  const qc = useQueryClient();
  const list = useQuery({
    queryKey: ['strategies', 'mine'],
    queryFn: () => api.listMyStrategies(apiClient),
  });
  const [expanded, setExpanded] = useState<number | null>(null);
  const [kind, setKind] = useState<string>('DCA');
  const [label, setLabel] = useState('');
  const [from, setFrom] = useState('USD');
  const [to, setTo] = useState('EUR');
  const [amount, setAmount] = useState('');
  const [schedule, setSchedule] = useState<string>('WEEKLY');
  const [targets, setTargets] = useState('');
  const [drift, setDrift] = useState('');

  const invalidate = () => qc.invalidateQueries({ queryKey: ['strategies'] });
  const create = useMutation({
    mutationFn: () => {
      const input: api.StrategyCreateInput = { kind, schedule };
      if (label !== '') input.label = label;
      if (kind === 'DCA') {
        input.from_currency = from;
        input.to_currency = to;
        input.amount = amount;
      } else {
        try {
          input.targets = JSON.parse(targets) as Record<string, string>;
        } catch {
          input.targets = {};
        }
        if (drift !== '') input.drift_band_pct = drift;
      }
      return api.createStrategy(apiClient, input);
    },
    onSuccess: async () => {
      setLabel('');
      setAmount('');
      setTargets('');
      setDrift('');
      await invalidate();
    },
  });
  const act = useMutation({
    mutationFn: ({ id, verb }: { id: number; verb: 'pause' | 'resume' | 'delete' }) =>
      verb === 'pause'
        ? api.pauseStrategy(apiClient, id)
        : verb === 'resume'
          ? api.resumeStrategy(apiClient, id)
          : api.cancelStrategy(apiClient, id),
    onSuccess: invalidate,
  });

  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">My strategies</h2>
      <ErrorBox error={list.error} />
      {list.isPending ? (
        <p className="text-sm text-neutral-400">Loading…</p>
      ) : (list.data ?? []).length === 0 ? (
        <p className="text-sm text-neutral-400">No strategies yet.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Strategy</th>
              <th className={thCls}>Kind</th>
              <th className={thCls}>Next run</th>
              <th className={thCls}>Runs</th>
              <th className={thCls}>Status</th>
              <th className={thCls} />
            </tr>
          </thead>
          <tbody>
            {(list.data ?? []).map((s) => (
              <Fragment key={s.strategy_id}>
                <tr>
                  <td className={tdCls}>
                    <button
                      type="button"
                      className="text-left text-sky-400 hover:underline"
                      onClick={() => {
                        setExpanded(expanded === s.strategy_id ? null : s.strategy_id);
                      }}
                    >
                      {s.label ?? `#${s.strategy_id}`}
                    </button>
                    {s.template_id !== undefined && (
                      <span className="ml-1 text-xs text-neutral-500">
                        (template #{s.template_id})
                      </span>
                    )}
                  </td>
                  <td className={tdCls}>{s.kind}</td>
                  <td className={tdCls}>
                    {s.next_run_at !== undefined ? new Date(s.next_run_at).toLocaleString() : '—'}
                  </td>
                  <td className={tdCls}>{s.run_count ?? 0}</td>
                  <td className={tdCls}>
                    <StatusBadge value={s.status} />
                  </td>
                  <td className={tdCls}>
                    <div className="flex gap-1">
                      {s.status === 'ACTIVE' && (
                        <button
                          type="button"
                          className={btnGhost}
                          disabled={act.isPending}
                          onClick={() => {
                            act.mutate({ id: s.strategy_id, verb: 'pause' });
                          }}
                        >
                          Pause
                        </button>
                      )}
                      {s.status === 'PAUSED' && (
                        <button
                          type="button"
                          className={btnGhost}
                          disabled={act.isPending}
                          onClick={() => {
                            act.mutate({ id: s.strategy_id, verb: 'resume' });
                          }}
                        >
                          Resume
                        </button>
                      )}
                      {s.status !== 'CANCELLED' && (
                        <button
                          type="button"
                          className={`${btnGhost} text-red-400`}
                          disabled={act.isPending}
                          onClick={() => {
                            act.mutate({ id: s.strategy_id, verb: 'delete' });
                          }}
                        >
                          Cancel
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
                {expanded === s.strategy_id && (
                  <tr>
                    <td colSpan={6} className={tdCls}>
                      <StrategyDetailRows id={s.strategy_id} />
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
      <ErrorBox error={create.error} />
      <ErrorBox error={act.error} />
      <form
        className="mt-3 grid gap-3 sm:grid-cols-3"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Kind">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={kind}
              onChange={(e) => {
                setKind(e.target.value);
              }}
            >
              {api.STRATEGY_KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Label">
          {(id) => (
            <input
              id={id}
              className={inputCls}
              value={label}
              onChange={(e) => {
                setLabel(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Schedule">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={schedule}
              onChange={(e) => {
                setSchedule(e.target.value);
              }}
            >
              {api.STRATEGY_SCHEDULES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          )}
        </Field>
        {kind === 'DCA' ? (
          <>
            <Field label="From">
              {(id) => (
                <select
                  id={id}
                  className={selectCls}
                  value={from}
                  onChange={(e) => {
                    setFrom(e.target.value);
                  }}
                >
                  {FIAT.map((c) => (
                    <option key={c} value={c}>
                      {c}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            <Field label="To">
              {(id) => (
                <select
                  id={id}
                  className={selectCls}
                  value={to}
                  onChange={(e) => {
                    setTo(e.target.value);
                  }}
                >
                  {FIAT.map((c) => (
                    <option key={c} value={c}>
                      {c}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            <Field label="Amount per run" required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="decimal"
                  value={amount}
                  onChange={(e) => {
                    setAmount(e.target.value);
                  }}
                />
              )}
            </Field>
          </>
        ) : (
          <>
            <Field label="Targets JSON" hint='{"EUR":"0.6","USD":"0.4"}' required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  value={targets}
                  onChange={(e) => {
                    setTargets(e.target.value);
                  }}
                />
              )}
            </Field>
            <Field label="Drift band %">
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="decimal"
                  value={drift}
                  onChange={(e) => {
                    setDrift(e.target.value);
                  }}
                />
              )}
            </Field>
          </>
        )}
        <div className="flex items-end">
          <button
            type="submit"
            className={btnPrimary}
            disabled={create.isPending || (kind === 'DCA' ? amount === '' : targets === '')}
          >
            {create.isPending ? 'Creating…' : 'Create strategy'}
          </button>
        </div>
      </form>
    </div>
  );
}

// ---------- templates --------------------------------------------------------

function Templates() {
  const qc = useQueryClient();
  const list = useQuery({
    queryKey: ['strategy-templates'],
    queryFn: () => api.listTemplates(apiClient),
  });
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [kind, setKind] = useState<string>('DCA');
  const [config, setConfig] = useState('');

  const instantiate = useMutation({
    mutationFn: (id: number) => api.instantiateTemplate(apiClient, id),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['strategies'] });
    },
  });
  const publish = useMutation({
    mutationFn: () => api.publishTemplate(apiClient, { name, description, kind, config }),
    onSuccess: async () => {
      setName('');
      setDescription('');
      setConfig('');
      await qc.invalidateQueries({ queryKey: ['strategy-templates'] });
    },
  });

  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">Strategy templates</h2>
      <p className="mb-2 text-xs text-neutral-500">
        Approved marketplace templates — instantiation copies the config into a new strategy under
        your account.
      </p>
      <ErrorBox error={list.error} />
      {list.isPending ? (
        <p className="text-sm text-neutral-400">Loading…</p>
      ) : (list.data ?? []).length === 0 ? (
        <p className="text-sm text-neutral-400">No approved templates.</p>
      ) : (
        <ul className="space-y-2">
          {(list.data ?? []).map((t) => (
            <li key={t.template_id} className="rounded border border-neutral-800 p-3">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">
                  {t.name}
                  <span className="ml-2 text-xs text-neutral-500">{t.kind}</span>
                </span>
                <div className="flex items-center gap-2">
                  <StatusBadge value={t.status} />
                  <button
                    type="button"
                    className={btnGhost}
                    disabled={instantiate.isPending}
                    onClick={() => {
                      instantiate.mutate(t.template_id);
                    }}
                  >
                    Instantiate
                  </button>
                </div>
              </div>
              <p className="mt-1 text-xs text-neutral-400">{t.description}</p>
            </li>
          ))}
        </ul>
      )}
      <ErrorBox error={instantiate.error} />
      {instantiate.isSuccess && (
        <p className="mt-2 text-sm text-emerald-400" role="status">
          Strategy #{instantiate.data.strategy_id} created from template.
        </p>
      )}

      <h3 className="mt-4 mb-1 text-xs font-medium text-neutral-400">Publish a template</h3>
      <p className="mb-2 text-xs text-neutral-500">
        Published templates enter PENDING_APPROVAL — they appear here only after admin review.
      </p>
      <form
        className="grid gap-3 sm:grid-cols-2"
        onSubmit={(e) => {
          e.preventDefault();
          publish.mutate();
        }}
      >
        <Field label="Name" required>
          {(id) => (
            <input
              id={id}
              className={inputCls}
              value={name}
              onChange={(e) => {
                setName(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Kind">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={kind}
              onChange={(e) => {
                setKind(e.target.value);
              }}
            >
              {api.STRATEGY_KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Template description" required>
          {(id) => (
            <input
              id={id}
              className={inputCls}
              value={description}
              onChange={(e) => {
                setDescription(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Config JSON" required>
          {(id) => (
            <input
              id={id}
              className={inputCls}
              value={config}
              onChange={(e) => {
                setConfig(e.target.value);
              }}
            />
          )}
        </Field>
        <div className="sm:col-span-2">
          <ErrorBox error={publish.error} />
          <button
            type="submit"
            className={btnPrimary}
            disabled={publish.isPending || name === '' || description === '' || config === ''}
          >
            {publish.isPending ? 'Publishing…' : 'Publish for review'}
          </button>
          {publish.isSuccess && (
            <p className="mt-1 text-xs text-emerald-400" role="status">
              Template #{publish.data.template_id} submitted — status {publish.data.status}.
            </p>
          )}
        </div>
      </form>
    </div>
  );
}

// ---------- copy-strategy author flow ----------------------------------------

function CopyAuthor() {
  const [created, setCreated] = useState<api.CopyProfile[]>([]);
  const [displayName, setDisplayName] = useState('');
  const [description, setDescription] = useState('');
  const [currency, setCurrency] = useState('USD');
  const [instrumentClass, setInstrumentClass] = useState('FX_SPOT');
  const [share, setShare] = useState('');

  const create = useMutation({
    mutationFn: () =>
      api.createCopyProfile(apiClient, {
        display_name: displayName,
        description,
        currency,
        instrument_class: instrumentClass,
        profit_share_pct: share,
      }),
    onSuccess: (p) => {
      setCreated((prev) => [p, ...prev]);
      setDisplayName('');
      setDescription('');
      setShare('');
    },
  });
  const list = useMutation({
    mutationFn: (id: number) => api.requestListing(apiClient, id),
    onSuccess: (p) => {
      setCreated((prev) => prev.map((x) => (x.strategy_id === p.strategy_id ? p : x)));
    },
  });

  return (
    <div className={cardCls}>
      <h2 className="mb-1 text-sm font-semibold">Offer a copy strategy</h2>
      <p className="mb-2 text-xs text-neutral-500">
        New profiles start INCUBATING — listing requires a ≥30-day track record and a passing
        appropriateness assessment, enforced server-side.
      </p>
      {created.length > 0 && (
        <ul className="mb-3 space-y-1">
          {created.map((p) => (
            <li
              key={p.strategy_id}
              className="flex items-center justify-between rounded border border-neutral-800 p-2 text-sm"
            >
              <span>
                #{p.strategy_id} {p.display_name}
              </span>
              <span className="flex items-center gap-2">
                <StatusBadge value={p.status} />
                {p.status === 'INCUBATING' && (
                  <button
                    type="button"
                    className={btnGhost}
                    disabled={list.isPending}
                    onClick={() => {
                      list.mutate(p.strategy_id);
                    }}
                  >
                    Request listing
                  </button>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
      <ErrorBox error={create.error} />
      <ErrorBox error={list.error} />
      <form
        className="grid gap-3 sm:grid-cols-2"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Display name" required>
          {(id) => (
            <input
              id={id}
              className={inputCls}
              value={displayName}
              onChange={(e) => {
                setDisplayName(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Currency">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={currency}
              onChange={(e) => {
                setCurrency(e.target.value);
              }}
            >
              {FIAT.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Instrument class">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={instrumentClass}
              onChange={(e) => {
                setInstrumentClass(e.target.value);
              }}
            >
              {['FX_SPOT', 'FX_FORWARD', 'FX_SWAP', 'FX_OPTION'].map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="Profit share %" required>
          {(id) => (
            <input
              id={id}
              className={inputCls}
              inputMode="decimal"
              placeholder="10"
              value={share}
              onChange={(e) => {
                setShare(e.target.value);
              }}
            />
          )}
        </Field>
        <Field label="Description" required>
          {(id) => (
            <input
              id={id}
              className={inputCls}
              value={description}
              onChange={(e) => {
                setDescription(e.target.value);
              }}
            />
          )}
        </Field>
        <div className="flex items-end">
          <button
            type="submit"
            className={btnPrimary}
            disabled={create.isPending || displayName === '' || description === '' || share === ''}
          >
            {create.isPending ? 'Creating…' : 'Create profile'}
          </button>
        </div>
      </form>
    </div>
  );
}

// ---------- basket + promotion viewers ---------------------------------------

function BasketLookup() {
  const [opId, setOpId] = useState('');
  const [submitted, setSubmitted] = useState('');
  const q = useQuery({
    queryKey: ['basket', submitted],
    queryFn: () => api.basketStatus(apiClient, submitted),
    enabled: submitted !== '',
    retry: false,
  });
  const d = q.data;
  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">Basket status</h2>
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(opId.trim());
        }}
      >
        <Field label="Operation ID">
          {(id) => (
            <input
              id={id}
              className={inputCls}
              placeholder="op id (e.g. 0:42 or hex)"
              value={opId}
              onChange={(e) => {
                setOpId(e.target.value);
              }}
            />
          )}
        </Field>
        <div className="flex items-end">
          <button type="submit" className={btnPrimary} disabled={opId.trim() === ''}>
            Look up
          </button>
        </div>
      </form>
      {submitted !== '' && q.isPending && <p className="mt-2 text-sm text-neutral-400">Loading…</p>}
      <ErrorBox error={q.error} />
      {d !== undefined && submitted !== '' && (
        <div className="mt-3 text-sm">
          <p className="flex items-center gap-2">
            <span className="text-neutral-400">op {d.op_id}</span>
            <StatusBadge value={d.status} />
            <span className="text-xs text-neutral-500">
              {d.legs_filled ?? 0}/{d.leg_count ?? 0} filled
              {(d.legs_unwound ?? 0) > 0 && ` · ${d.legs_unwound ?? 0} unwound`}
              {d.slippage_ticks !== undefined && ` · slip ${d.slippage_ticks}t`}
            </span>
          </p>
          {(d.legs ?? []).length > 0 && (
            <table className={`${tableCls} mt-2`}>
              <thead>
                <tr>
                  <th className={thCls}>Leg</th>
                  <th className={thCls}>Order</th>
                  <th className={thCls}>Instrument</th>
                  <th className={thCls}>Status</th>
                  <th className={thCls}>Filled</th>
                </tr>
              </thead>
              <tbody>
                {(d.legs ?? []).map((l, i) => (
                  <tr key={l.order_id ?? i}>
                    <td className={tdCls}>{l.leg_index ?? i}</td>
                    <td className={tdCls}>{l.order_id ?? '—'}</td>
                    <td className={tdCls}>{l.instrument_id ?? '—'}</td>
                    <td className={tdCls}>
                      {l.order_status !== undefined ? <StatusBadge value={l.order_status} /> : '—'}
                    </td>
                    <td className={tdCls}>{l.filled_qty ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
}

function PromotionView() {
  const [id, setId] = useState('');
  const [submitted, setSubmitted] = useState('');
  const q = useQuery({
    queryKey: ['promotion', submitted],
    queryFn: () => api.promotionView(apiClient, submitted),
    enabled: submitted !== '',
    retry: false,
  });
  const p = q.data;
  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">Promotion</h2>
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(id.trim());
        }}
      >
        <Field label="Promotion ID">
          {(id2) => (
            <input
              id={id2}
              className={inputCls}
              inputMode="numeric"
              value={id}
              onChange={(e) => {
                setId(e.target.value);
              }}
            />
          )}
        </Field>
        <div className="flex items-end">
          <button type="submit" className={btnPrimary} disabled={id.trim() === ''}>
            View
          </button>
        </div>
      </form>
      {submitted !== '' && q.isPending && <p className="mt-2 text-sm text-neutral-400">Loading…</p>}
      <ErrorBox error={q.error} />
      {p != null && submitted !== '' && (
        <div className="mt-3 rounded border border-neutral-800 p-3 text-sm" role="status">
          <p className="font-medium">
            {p.title ?? `Promotion #${p.promotion_id}`}
            {p.slug !== undefined && (
              <span className="ml-2 text-xs text-neutral-500">{p.slug}</span>
            )}
          </p>
          <p className="mt-1 text-xs text-neutral-400">
            {p.channel ?? '—'} · v{p.version ?? '?'} · body {p.body_ref ?? '—'}
            {p.approved_until !== undefined &&
              ` · approved until ${new Date(p.approved_until).toLocaleDateString()}`}
          </p>
        </div>
      )}
    </div>
  );
}

export default function MarketplacePanel() {
  return (
    <div className="space-y-4">
      <MyStrategies />
      <Templates />
      <CopyAuthor />
      <BasketLookup />
      <PromotionView />
    </div>
  );
}
