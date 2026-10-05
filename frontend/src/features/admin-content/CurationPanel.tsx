/**
 * Curation panel — strategy-template review queue (approve/reject; the
 * decision writes its admin_audit_log row in the mutation transaction)
 * and the Compliance-Officer copy-strategy suspend misconduct action.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { decideTemplate, fetchTemplates, suspendCopyStrategy } from './api';

const STATUSES = ['', 'PENDING', 'APPROVED', 'REJECTED'];

export function CurationPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [filter, setFilter] = useState('PENDING');
  const [suspend, setSuspend] = useState({ id: '', reason: '' });
  const [notice, setNotice] = useState<string | null>(null);

  const templates = useQuery({
    queryKey: ['admin', 'templates', adminApi.env, filter],
    queryFn: () => fetchTemplates(adminApi, filter),
    retry: false,
  });

  const invalidate = () => void qc.invalidateQueries({ queryKey: ['admin', 'templates'] });
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : String(e));

  const decideMut = useMutation({
    mutationFn: ({ id, approve, reason }: { id: number; approve: boolean; reason: string }) =>
      decideTemplate(adminApi, id, approve, reason),
    onSuccess: (_d, v) => {
      setNotice(`Template #${v.id} ${v.approve ? 'APPROVED' : 'REJECTED'}.`);
      invalidate();
    },
    onError: onErr,
  });
  const suspendMut = useMutation({
    mutationFn: () => suspendCopyStrategy(adminApi, Number(suspend.id), suspend.reason),
    onSuccess: () => {
      setNotice(`Copy strategy #${suspend.id} suspended — existing follows keep running.`);
      setSuspend({ id: '', reason: '' });
    },
    onError: onErr,
  });

  const denied = isAccessDenied(templates.error);
  if (denied)
    return <AccessDeniedCard detail="Template curation requires a Compliance Officer role." />;

  return (
    <section className={cardCls} aria-label="Curation">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium text-neutral-400">
          Strategy templates &amp; copy strategies
        </h2>
        <select
          aria-label="Template status filter"
          className={selectCls}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        >
          {STATUSES.map((s) => (
            <option key={s} value={s}>
              {s === '' ? 'all' : s}
            </option>
          ))}
        </select>
      </div>
      <ErrorBox error={templates.error} />
      {notice !== null && (
        <p className="mb-2 rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-300">
          {notice}
        </p>
      )}
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>ID</th>
            <th className={thCls}>Name</th>
            <th className={thCls}>Kind</th>
            <th className={thCls}>Publisher</th>
            <th className={thCls}>Status</th>
            <th className={thCls}></th>
          </tr>
        </thead>
        <tbody>
          {(templates.data ?? []).map((t) => (
            <tr key={t.templateId}>
              <td className={tdCls}>{t.templateId}</td>
              <td className={tdCls}>{t.name}</td>
              <td className={tdCls}>{t.kind}</td>
              <td className={tdCls}>{t.publisherAccountId}</td>
              <td className={tdCls}>
                <StatusBadge value={t.status} />
                {t.rejectReason !== undefined && t.rejectReason !== '' && (
                  <span className="ml-1 text-xs text-red-400">{t.rejectReason}</span>
                )}
              </td>
              <td className={tdCls}>
                {t.status === 'PENDING' && (
                  <div className="flex gap-1">
                    <button
                      type="button"
                      className={btnPrimary}
                      disabled={decideMut.isPending}
                      onClick={() =>
                        decideMut.mutate({ id: t.templateId, approve: true, reason: '' })
                      }
                    >
                      Approve
                    </button>
                    <button
                      type="button"
                      className={btnDanger}
                      disabled={decideMut.isPending}
                      onClick={() =>
                        decideMut.mutate({ id: t.templateId, approve: false, reason: 'rejected' })
                      }
                    >
                      Reject
                    </button>
                  </div>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {templates.data?.length === 0 && (
        <p className="py-2 text-center text-xs text-neutral-500">No templates in this state.</p>
      )}

      <h3 className="mt-6 mb-2 text-sm font-medium text-neutral-400">
        Copy-strategy suspend (misconduct)
      </h3>
      <form
        className="grid gap-2 sm:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          suspendMut.mutate();
        }}
      >
        <label className="block">
          <span className="sr-only">Strategy id</span>
          <input
            aria-label="Strategy id"
            className={inputCls}
            placeholder="strategy id"
            value={suspend.id}
            onChange={(e) => setSuspend({ ...suspend, id: e.target.value })}
          />
        </label>
        <label className="block sm:col-span-2">
          <span className="sr-only">Suspend reason</span>
          <input
            aria-label="Suspend reason"
            className={inputCls}
            placeholder="reason (scope breach / stat manipulation)"
            value={suspend.reason}
            onChange={(e) => setSuspend({ ...suspend, reason: e.target.value })}
          />
        </label>
        <button
          type="submit"
          className={btnDanger}
          disabled={suspendMut.isPending || Number(suspend.id) <= 0 || suspend.reason === ''}
        >
          Suspend strategy
        </button>
      </form>
      <p className={hintTextCls}>
        Suspend is audit-logged before the status flip; existing follows keep running under the
        investor disclosure.
      </p>
    </section>
  );
}
