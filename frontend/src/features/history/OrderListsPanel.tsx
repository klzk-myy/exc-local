/**
 * OPO/OCO order lists (Task 10.3.27 item 3) — open lists + history tabs.
 * GET /api/v1/order-lists and /order-lists/history are live
 * (Phase-16 Task 16.3.20 — supersedes the earlier registered-stub note);
 * the panel renders parent/child rows from the backend.
 */
import { Fragment, useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError, newIdempotencyKey } from '@/lib/api';
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
import { UnavailablePanel, isNotImplemented } from '@/lib/input-helpers';

import {
  cancelOrderList,
  listOrderListHistory,
  listOrderLists,
  orderListDetail,
  submitOrderList,
  type OrderListSummary,
} from './api';

const PENDING_TYPES = ['LIMIT', 'STOP', 'STOP_LIMIT'] as const;

/** POST /order-lists intake (§6.10) — working BUY + pending SELL leg(s).
 * Pending quantity is intentionally absent: the server recomputes it
 * from the working fill's net proceeds (§24 #287). */
function OrderListIntake() {
  const qc = useQueryClient();
  const [contingency, setContingency] = useState<'OPO' | 'OPOCO'>('OPO');
  const [symbol, setSymbol] = useState('EUR/USD');
  const [workType, setWorkType] = useState<'LIMIT' | 'MARKET'>('LIMIT');
  const [workQty, setWorkQty] = useState('');
  const [workPrice, setWorkPrice] = useState('');
  const [legs, setLegs] = useState([
    { type: 'LIMIT', price: '', stop_price: '' },
    { type: 'STOP', price: '', stop_price: '' },
  ]);

  const submit = useMutation({
    mutationFn: () =>
      submitOrderList(apiClient, {
        contingency_type: contingency,
        symbol,
        client_order_id: newIdempotencyKey(),
        working: {
          symbol,
          side: 'BUY',
          type: workType,
          quantity: workQty,
          ...(workType === 'LIMIT' ? { price: workPrice } : {}),
        },
        pending: legs.slice(0, contingency === 'OPO' ? 1 : 2).map((l) => ({
          symbol,
          side: 'SELL',
          type: l.type,
          ...(l.type !== 'STOP' && l.price !== '' ? { price: l.price } : {}),
          ...(l.type !== 'LIMIT' && l.stop_price !== '' ? { stop_price: l.stop_price } : {}),
        })),
      }),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['history', 'order-lists'] });
      await qc.invalidateQueries({ queryKey: ['orders'] });
    },
  });

  const pendingValid = legs
    .slice(0, contingency === 'OPO' ? 1 : 2)
    .every(
      (l) => (l.type === 'STOP' || l.price !== '') && (l.type === 'LIMIT' || l.stop_price !== ''),
    );
  const valid =
    symbol !== '' && workQty !== '' && (workType === 'MARKET' || workPrice !== '') && pendingValid;

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    submit.mutate();
  };

  return (
    <form className={`${cardCls} grid gap-3 sm:grid-cols-4`} onSubmit={onSubmit}>
      <Field label="Contingency">
        {(id) => (
          <select
            id={id}
            className={selectCls}
            value={contingency}
            onChange={(e) => {
              setContingency(e.target.value as 'OPO' | 'OPOCO');
            }}
          >
            <option value="OPO">OPO (one pending)</option>
            <option value="OPOCO">OPOCO (OCO pair)</option>
          </select>
        )}
      </Field>
      <Field label="Symbol" required>
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
      <Field label="Working type">
        {(id) => (
          <select
            id={id}
            className={selectCls}
            value={workType}
            onChange={(e) => {
              setWorkType(e.target.value as 'LIMIT' | 'MARKET');
            }}
          >
            <option value="LIMIT">LIMIT</option>
            <option value="MARKET">MARKET</option>
          </select>
        )}
      </Field>
      <Field label="Working quantity (BUY)" required>
        {(id) => (
          <input
            id={id}
            className={inputCls}
            inputMode="decimal"
            value={workQty}
            onChange={(e) => {
              setWorkQty(e.target.value);
            }}
          />
        )}
      </Field>
      {workType === 'LIMIT' ? (
        <Field label="Working price" required>
          {(id) => (
            <input
              id={id}
              className={inputCls}
              inputMode="decimal"
              value={workPrice}
              onChange={(e) => {
                setWorkPrice(e.target.value);
              }}
            />
          )}
        </Field>
      ) : null}
      {legs.slice(0, contingency === 'OPO' ? 1 : 2).map((l, i) => (
        <Fragment key={i}>
          <Field label={`Pending ${i + 1} type (SELL)`}>
            {(id) => (
              <select
                id={id}
                className={selectCls}
                value={l.type}
                onChange={(e) => {
                  const t = e.target.value;
                  setLegs((ls) => ls.map((x, j) => (j === i ? { ...x, type: t } : x)));
                }}
              >
                {PENDING_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            )}
          </Field>
          {l.type !== 'STOP' ? (
            <Field label={`Pending ${i + 1} price`} required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="decimal"
                  value={l.price}
                  onChange={(e) => {
                    setLegs((ls) =>
                      ls.map((x, j) => (j === i ? { ...x, price: e.target.value } : x)),
                    );
                  }}
                />
              )}
            </Field>
          ) : null}
          {l.type !== 'LIMIT' ? (
            <Field label={`Pending ${i + 1} stop price`} required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="decimal"
                  value={l.stop_price}
                  onChange={(e) => {
                    setLegs((ls) =>
                      ls.map((x, j) => (j === i ? { ...x, stop_price: e.target.value } : x)),
                    );
                  }}
                />
              )}
            </Field>
          ) : null}
        </Fragment>
      ))}
      <div className="flex items-end gap-2 sm:col-span-4">
        <button type="submit" className={btnPrimary} disabled={!valid || submit.isPending}>
          {submit.isPending ? 'Submitting…' : `Submit ${contingency}`}
        </button>
        <p className="text-xs text-neutral-500">
          Pending SELL quantity is recomputed from the working fill&apos;s net proceeds — not sent.
        </p>
      </div>
      {submit.isError ? (
        <div className="sm:col-span-4">
          <ErrorBox error={submit.error} />
        </div>
      ) : null}
      {submit.isSuccess ? (
        <p role="status" className="text-xs text-emerald-400 sm:col-span-4">
          List accepted (202).
        </p>
      ) : null}
    </form>
  );
}

function ListDetail({ id }: { id: string }) {
  const q = useQuery({
    queryKey: ['history', 'order-list', id],
    queryFn: () => orderListDetail(apiClient, id),
  });
  if (q.isPending) return <p className="text-xs text-neutral-500">Loading detail…</p>;
  if (q.isError) return <ErrorBox error={q.error} />;
  const d = q.data;
  return (
    <div className="space-y-1 text-xs">
      <p>
        <span className="text-neutral-500">Contingency:</span> {d.contingency_type ?? '—'} ·{' '}
        <span className="text-neutral-500">state:</span> {d.state ?? '—'} ·{' '}
        <span className="text-neutral-500">working order:</span>{' '}
        <span className="font-mono">{d.working_order_id ?? '—'}</span>
      </p>
      {d.fail_reason !== undefined && d.fail_reason !== '' ? (
        <p role="alert" className="text-red-400">
          fail: {d.fail_reason}
        </p>
      ) : null}
      {(d.legs ?? []).length > 0 ? (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>#</th>
              <th className={thCls}>Role</th>
              <th className={thCls}>Order</th>
              <th className={thCls}>State</th>
              <th className={thCls}>Params</th>
            </tr>
          </thead>
          <tbody>
            {(d.legs ?? []).map((l, i) => (
              <tr key={i}>
                <td className={tdCls}>{l.leg_index ?? i}</td>
                <td className={tdCls}>{l.role ?? '—'}</td>
                <td className={tdCls + ' font-mono'}>{l.order_id ?? '—'}</td>
                <td className={tdCls}>{l.state ?? '—'}</td>
                <td className={tdCls + ' font-mono'}>
                  {l.params !== undefined ? JSON.stringify(l.params) : '—'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="text-neutral-500">No legs recorded.</p>
      )}
    </div>
  );
}

export function OrderListsPanel() {
  const qc = useQueryClient();
  const [tab, setTab] = useState<'open' | 'history'>('open');
  const [expanded, setExpanded] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<string | null>(null);
  const q = useQuery({
    queryKey: ['history', 'order-lists', tab],
    queryFn: () => (tab === 'open' ? listOrderLists(apiClient) : listOrderListHistory(apiClient)),
    retry: (n, e) => !(e instanceof ApiError && e.status === 501) && n < 2,
  });
  const cancel = useMutation({
    mutationFn: (id: string) => cancelOrderList(apiClient, id),
    onSuccess: async () => {
      setConfirm(null);
      await qc.invalidateQueries({ queryKey: ['history', 'order-lists'] });
      await qc.invalidateQueries({ queryKey: ['orders'] });
    },
  });

  return (
    <section aria-label="Order lists" className="space-y-3">
      <OrderListIntake />
      <div role="tablist" aria-label="Order list view" className="flex gap-1">
        {(['open', 'history'] as const).map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            className={`rounded px-3 py-1 text-sm ${tab === t ? 'bg-neutral-800 text-neutral-100' : 'text-neutral-400 hover:text-neutral-200'}`}
            onClick={() => {
              setTab(t);
            }}
          >
            {t === 'open' ? 'Open lists' : 'List history'}
          </button>
        ))}
      </div>

      {q.isPending ? (
        <p className="text-sm text-neutral-500">Loading order lists…</p>
      ) : q.isError ? (
        isNotImplemented(q.error) ? (
          <UnavailablePanel
            feature="OPO / OCO order lists"
            owner="Phase-16 Task 16.3.20"
            note="Parent/child list state arrives with the order-list engine."
          />
        ) : (
          <ErrorBox error={q.error} />
        )
      ) : q.data.length === 0 ? (
        <p className="text-sm text-neutral-500">No {tab} order lists.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>List</th>
              <th className={thCls}>Type</th>
              <th className={thCls}>Symbol</th>
              <th className={thCls}>Legs</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {q.data.map((l: OrderListSummary) => (
              <Fragment key={l.id}>
                <tr>
                  <td className={tdCls + ' font-mono'}>{l.id}</td>
                  <td className={tdCls}>{l.type}</td>
                  <td className={tdCls + ' font-mono'}>{l.symbol ?? '—'}</td>
                  <td className={tdCls}>{l.legs ?? '—'}</td>
                  <td className={tdCls}>{l.status}</td>
                  <td className={tdCls}>
                    <button
                      type="button"
                      className={`${btnGhost} text-xs`}
                      onClick={() => {
                        setExpanded((e) => (e === l.id ? null : l.id));
                      }}
                    >
                      {expanded === l.id ? 'Hide' : 'Detail'}
                    </button>
                    {tab === 'open' ? (
                      <button
                        type="button"
                        className={`${btnGhost} ml-1 text-xs text-red-400`}
                        disabled={cancel.isPending}
                        onClick={() => {
                          if (confirm === l.id) {
                            setConfirm(null);
                            cancel.mutate(l.id);
                          } else {
                            setConfirm(l.id);
                          }
                        }}
                      >
                        {confirm === l.id ? 'Confirm cancel' : 'Cancel'}
                      </button>
                    ) : null}
                  </td>
                </tr>
                {expanded === l.id ? (
                  <tr>
                    <td className={tdCls} colSpan={6}>
                      <ListDetail id={l.id} />
                    </td>
                  </tr>
                ) : null}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
