/**
 * Fee-promo windows panel (Phase-10.5 Task 10.5.3.10 §2) — promo
 * windows open as PENDING_APPROVAL and only a distinct approver's
 * POST …/approve (route-registered DualControl) applies the promo_*
 * rates to fee_tiers; reject needs a reason. PENDING is rendered as
 * pending — never as applied.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { approvePromo, createPromoWindow, fetchPromoWindows, rejectPromo } from './api';

export function PromosPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [form, setForm] = useState({
    feeTierId: '',
    makerBps: '',
    takerBps: '',
    endsAt: '',
    note: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const list = useQuery({
    queryKey: ['admin-fee-promos'],
    queryFn: () => fetchPromoWindows(adminApi),
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-fee-promos'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');
  const create = useMutation({
    mutationFn: () =>
      createPromoWindow(adminApi, {
        feeTierId: Number(form.feeTierId),
        promoMakerBps: form.makerBps === '' ? undefined : form.makerBps,
        promoTakerBps: form.takerBps === '' ? undefined : form.takerBps,
        endsAt: form.endsAt,
        note: form.note === '' ? undefined : form.note,
      }),
    onSuccess: () => {
      setNotice('Promo window opened as PENDING_APPROVAL — a distinct approver must sign.');
      invalidate();
    },
    onError: onErr,
  });
  const approve = useMutation({
    mutationFn: (id: number) => approvePromo(adminApi, id),
    onSuccess: invalidate,
    onError: onErr,
  });
  const reject = useMutation({
    mutationFn: (id: number) => rejectPromo(adminApi, id, 'rejected via ops console'),
    onSuccess: invalidate,
    onError: onErr,
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Fee promos">
      <h2 className="mb-1 text-sm font-semibold">Fee promo windows</h2>
      <p className={hintTextCls}>
        Approval is dual-control — the approver must differ from the window creator (15-minute
        approval window).
      </p>
      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="mt-2 text-sm text-neutral-500">No promo windows.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <table className={`${tableCls} mt-2`}>
          <thead>
            <tr>
              <th className={thCls}>ID</th>
              <th className={thCls}>Tier</th>
              <th className={thCls}>Maker bps</th>
              <th className={thCls}>Taker bps</th>
              <th className={thCls}>Ends</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {list.data.map((w) => (
              <tr key={w.id}>
                <td className={tdCls}>{w.id}</td>
                <td className={tdCls}>{w.feeTierId}</td>
                <td className={tdCls}>{w.promoMakerBps ?? '—'}</td>
                <td className={tdCls}>{w.promoTakerBps ?? '—'}</td>
                <td className={tdCls}>{w.endsAt.slice(0, 16)}</td>
                <td className={tdCls}>
                  <StatusBadge value={w.status || 'UNKNOWN'} />
                </td>
                <td className={tdCls}>
                  {w.status === 'PENDING_APPROVAL' ? (
                    <div className="flex gap-1">
                      <button
                        type="button"
                        className={btnPrimary}
                        disabled={approve.isPending}
                        onClick={() => {
                          approve.mutate(w.id);
                        }}
                      >
                        Approve (4-eyes)
                      </button>
                      <button
                        type="button"
                        className={btnDanger}
                        disabled={reject.isPending}
                        onClick={() => {
                          reject.mutate(w.id);
                        }}
                      >
                        Reject
                      </button>
                    </div>
                  ) : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}

      <form
        aria-label="Create promo window"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (
            Number(form.feeTierId) > 0 &&
            form.endsAt !== '' &&
            (form.makerBps !== '' || form.takerBps !== '')
          ) {
            create.mutate();
          }
        }}
      >
        <input
          aria-label="Fee tier id"
          className={inputCls}
          placeholder="fee_tier_id"
          value={form.feeTierId}
          onChange={(e) => {
            setForm({ ...form, feeTierId: e.target.value });
          }}
        />
        <input
          aria-label="Promo maker bps"
          className={inputCls}
          placeholder="promo_maker_bps"
          value={form.makerBps}
          onChange={(e) => {
            setForm({ ...form, makerBps: e.target.value });
          }}
        />
        <input
          aria-label="Promo taker bps"
          className={inputCls}
          placeholder="promo_taker_bps"
          value={form.takerBps}
          onChange={(e) => {
            setForm({ ...form, takerBps: e.target.value });
          }}
        />
        <input
          aria-label="Ends at"
          className={inputCls}
          placeholder="ends_at RFC3339"
          value={form.endsAt}
          onChange={(e) => {
            setForm({ ...form, endsAt: e.target.value });
          }}
        />
        <input
          aria-label="Promo note"
          className={inputCls}
          placeholder="note"
          value={form.note}
          onChange={(e) => {
            setForm({ ...form, note: e.target.value });
          }}
        />
        <button type="submit" className={btnGhost} disabled={create.isPending}>
          Open window
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
