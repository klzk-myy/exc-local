/**
 * Swap-free verification decisions (Task 10.5.3.27 gate-coverage
 * wiring, Phase-14 Task 14.3.15) — Compliance-Officer approve / reject
 * / revoke on a verification id. The registry mounts no admin list
 * route: request ids arrive via the compliance case queue, so the
 * panel is id-driven by design.
 */
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import { btnPrimary, cardCls, hintTextCls, inputCls, labelCls, selectCls } from '@/lib/ui';

import { decideSwapFree } from './api';

export function SwapFreePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [notice, setNotice] = useState<string | null>(null);
  const [form, setForm] = useState({
    id: '',
    decision: 'approve' as 'approve' | 'reject' | 'revoke',
    reason: '',
  });
  const decide = useMutation({
    mutationFn: () =>
      decideSwapFree(
        adminApi,
        Number(form.id),
        form.decision,
        form.reason === '' ? undefined : form.reason,
      ),
    onSuccess: () => setNotice(`Swap-free #${form.id}: ${form.decision} applied`),
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Decision failed'),
  });

  return (
    <section className={cardCls} aria-label="Swap-free decisions">
      <h2 className="mb-2 text-sm font-semibold">Swap-free decisions</h2>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          decide.mutate();
        }}
      >
        <label className={labelCls}>
          Verification id
          <input
            className={inputCls}
            value={form.id}
            onChange={(e) => setForm({ ...form, id: e.target.value })}
            required
            inputMode="numeric"
          />
        </label>
        <label className={labelCls}>
          Decision
          <select
            className={selectCls}
            value={form.decision}
            onChange={(e) => setForm({ ...form, decision: e.target.value as typeof form.decision })}
          >
            <option value="approve">Approve</option>
            <option value="reject">Reject</option>
            <option value="revoke">Revoke</option>
          </select>
        </label>
        {(form.decision === 'reject' || form.decision === 'revoke') && (
          <label className={labelCls}>
            Reason
            <input
              className={inputCls}
              value={form.reason}
              onChange={(e) => setForm({ ...form, reason: e.target.value })}
              required
            />
          </label>
        )}
        <button type="submit" className={btnPrimary} disabled={decide.isPending}>
          Apply decision
        </button>
      </form>
      <p className={`mt-2 ${hintTextCls}`}>
        Revoke is the abuse guard — it reverts standing swap-free status and raises a compliance
        hold. Request ids arrive through the case queue (no admin list route mounts).
      </p>
    </section>
  );
}
