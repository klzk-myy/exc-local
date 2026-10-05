/**
 * Joined audit-trail search (Task 10.5.3.27 gate-coverage wiring,
 * Phase-21 Task 21.3.27) — admin_audit_log ⨝ audit_hash_chain rows
 * rendered verbatim; the mask toggle produces the auditor-shareable
 * export (truncated IPs, scrubbed PII keys — server-side).
 */
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import { btnGhost, cardCls, hintTextCls, inputCls, labelCls } from '@/lib/ui';

import { fetchAuditTrail } from './api';

export function AuditTrailPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [form, setForm] = useState({ action: '', targetType: '', adminUserId: '', mask: false });
  const [submitted, setSubmitted] = useState<typeof form | null>(null);

  const trail = useQuery({
    queryKey: ['admin-audit-trail', submitted],
    queryFn: () =>
      fetchAuditTrail(adminApi, {
        action: submitted?.action,
        targetType: submitted?.targetType,
        adminUserId: submitted?.adminUserId,
        mask: submitted?.mask,
      }),
    enabled: submitted !== null,
    retry: false,
  });

  return (
    <section className={cardCls} aria-label="Joined audit trail">
      <h2 className="mb-2 text-sm font-semibold">Joined audit trail</h2>
      <form
        className="mb-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted({ ...form });
        }}
      >
        <label className={labelCls}>
          Action
          <input
            className={inputCls}
            value={form.action}
            onChange={(e) => setForm({ ...form, action: e.target.value })}
          />
        </label>
        <label className={labelCls}>
          Target type
          <input
            className={inputCls}
            value={form.targetType}
            onChange={(e) => setForm({ ...form, targetType: e.target.value })}
          />
        </label>
        <label className={labelCls}>
          Admin user id
          <input
            className={inputCls}
            value={form.adminUserId}
            onChange={(e) => setForm({ ...form, adminUserId: e.target.value })}
            inputMode="numeric"
          />
        </label>
        <label className="flex items-center gap-2 pb-1 text-xs text-neutral-300">
          <input
            type="checkbox"
            checked={form.mask}
            onChange={(e) => setForm({ ...form, mask: e.target.checked })}
          />
          Masked export (PII-safe)
        </label>
        <button type="submit" className={btnGhost} disabled={trail.isFetching}>
          Search
        </button>
      </form>
      {trail.isError && (
        <p role="alert" className="text-xs text-rose-300">
          {trail.error.message}
        </p>
      )}
      {trail.data !== undefined && (
        <ul
          className="max-h-64 overflow-y-auto rounded border border-neutral-800 p-2 font-mono text-xs text-neutral-300"
          tabIndex={0}
        >
          {trail.data.map((row, i) => (
            <li key={i} className="py-0.5">
              {JSON.stringify(row)}
            </li>
          ))}
          {trail.data.length === 0 && <li className={hintTextCls}>No rows.</li>}
        </ul>
      )}
    </section>
  );
}
