/**
 * Sanctions-ops panel (Phase-10.5 Task 10.5.3.6 §4) — vendor feed
 * operations: provenance/provider-gate snapshot (shared read from the
 * compliance feature), force refresh, and the manual pending-queue
 * replay used after provider recovery. Both mutations are
 * fail-closed: a bad feed retains last-good lists, a partial replay
 * reports its backlog.
 */
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import { btnGhost, cardCls, ErrorBox, hintTextCls, StatusBadge } from '@/lib/ui';

import { fetchSanctionsStatus } from '../admin-compliance/api';
import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { refreshSanctions, replaySanctionsQueue } from './api';

export function SanctionsOpsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [notice, setNotice] = useState<string | null>(null);

  const status = useQuery({
    queryKey: ['admin-sanctions-status'],
    queryFn: () => fetchSanctionsStatus(adminApi),
  });

  const refresh = useMutation({
    mutationFn: () => refreshSanctions(adminApi),
    onSuccess: (r) => {
      const code = r['error_code'];
      setNotice(
        code === '' || code === undefined
          ? 'Refresh pass complete.'
          : `Refresh failed: ${typeof code === 'string' ? code : 'SANCTIONS_REFRESH_FAILED'}`,
      );
      void status.refetch();
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Refresh failed'),
  });

  const replay = useMutation({
    mutationFn: () => replaySanctionsQueue(adminApi),
    onSuccess: () => {
      setNotice('Replay pass complete — check pending depth.');
      void status.refetch();
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Replay failed'),
  });

  if (status.error !== null && isAccessDenied(status.error)) {
    return <AccessDeniedCard />;
  }

  const gate = status.data?.['provider_gate'];
  const gateLabel =
    typeof gate === 'object' && gate !== null && 'state' in gate
      ? String((gate as Record<string, unknown>)['state'])
      : typeof gate === 'string'
        ? gate
        : undefined;

  return (
    <section className={cardCls} aria-label="Sanctions ops">
      <h2 className="mb-2 text-sm font-semibold">Sanctions ops</h2>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className={hintTextCls}>Provider gate</span>
        {gateLabel !== undefined ? <StatusBadge value={gateLabel} /> : null}
        {typeof status.data?.['pending_screens'] === 'number' ? (
          <span className={hintTextCls}>pending {status.data['pending_screens']}</span>
        ) : null}
        <button
          type="button"
          className={btnGhost}
          disabled={refresh.isPending}
          onClick={() => refresh.mutate()}
        >
          Refresh vendor lists
        </button>
        <button
          type="button"
          className={btnGhost}
          disabled={replay.isPending}
          onClick={() => replay.mutate()}
        >
          Replay pending queue
        </button>
      </div>
      {status.error !== null ? <ErrorBox error={status.error} /> : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
