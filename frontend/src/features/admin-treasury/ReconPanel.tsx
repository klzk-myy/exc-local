/**
 * Reconciliation panel (Phase-10.5 Task 10.5.3.8 §2) — nostro daily
 * reconciliation (latest-run-per-account report, on-demand rerun,
 * break INVESTIGATE/RESOLVE workflow) and the PB give-up
 * reconciliation report (auto-match rate + open breaks).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { actOnBreak, fetchNostroRecon, fetchPbRecon, runNostroRecon } from './api';

export function ReconPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [date, setDate] = useState('');
  const [pbId, setPbId] = useState('');
  const [breakNotes, setBreakNotes] = useState<Record<number, string>>({});
  const [notice, setNotice] = useState<string | null>(null);

  const nostro = useQuery({
    queryKey: ['admin-nostro-recon', date],
    queryFn: () => fetchNostroRecon(adminApi, date === '' ? undefined : date),
  });
  const pb = useQuery({
    queryKey: ['admin-pb-recon', pbId, date],
    queryFn: () =>
      fetchPbRecon(adminApi, {
        pbId: pbId === '' ? undefined : Number(pbId),
        date: date === '' ? undefined : date,
      }),
  });

  const run = useMutation({
    mutationFn: () => runNostroRecon(adminApi, { date: date === '' ? undefined : date }),
    onSuccess: (rep) => {
      setNotice(
        `Run complete — ${rep.accountsReconciled} accounts, ${rep.openBreaks} open breaks.`,
      );
      void qc.invalidateQueries({ queryKey: ['admin-nostro-recon'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Run failed'),
  });
  const breakAct = useMutation({
    mutationFn: (input: { id: number; action: 'INVESTIGATE' | 'RESOLVE' }) =>
      actOnBreak(adminApi, input.id, {
        action: input.action,
        notes: breakNotes[input.id] === '' ? undefined : breakNotes[input.id],
      }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['admin-nostro-recon'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Break action failed'),
  });

  if (nostro.error !== null && isAccessDenied(nostro.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Reconciliation">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Nostro &amp; PB reconciliation</h2>
        <div className="flex items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="rc-date">
              Date
            </label>
            <input
              id="rc-date"
              type="date"
              className={inputCls}
              value={date}
              onChange={(e) => {
                setDate(e.target.value);
              }}
            />
          </div>
          <button
            type="button"
            className={btnPrimary}
            disabled={run.isPending}
            onClick={() => {
              run.mutate();
            }}
          >
            Run nostro recon
          </button>
        </div>
      </div>

      {nostro.error !== null ? <ErrorBox error={nostro.error} /> : null}
      {nostro.data !== undefined ? (
        <div aria-label="Nostro recon report">
          <p className={hintTextCls}>
            {nostro.data.date.slice(0, 10)} — {nostro.data.accountsReconciled} accounts reconciled ·{' '}
            {nostro.data.openBreaks} open breaks
            {nostro.data.thresholdBreaches > 0
              ? ` · ${nostro.data.thresholdBreaches} THRESHOLD BREACH`
              : ''}
          </p>
          {nostro.data.runs.length > 0 ? (
            <table className={tableCls} aria-label="Recon runs">
              <thead>
                <tr>
                  <th className={thCls}>Account</th>
                  <th className={thCls}>Our net</th>
                  <th className={thCls}>Statement net</th>
                  <th className={thCls}>Diff</th>
                  <th className={thCls}>Breaks</th>
                  <th className={thCls}>Status</th>
                </tr>
              </thead>
              <tbody>
                {nostro.data.runs.map((r) => (
                  <tr key={r.id}>
                    <td className={tdCls}>#{r.nostroAccountId}</td>
                    <td className={tdCls}>{r.ourNet}</td>
                    <td className={tdCls}>{r.statementNet}</td>
                    <td className={tdCls}>{r.difference}</td>
                    <td className={tdCls}>{r.breaksOpened}</td>
                    <td className={tdCls}>
                      <StatusBadge value={r.status || 'UNKNOWN'} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : null}
          {nostro.data.breaks.length > 0 ? (
            <div className="mt-2 max-h-48 overflow-y-auto">
              <table className={tableCls} aria-label="Recon breaks">
                <thead>
                  <tr>
                    <th className={thCls}>ID</th>
                    <th className={thCls}>Category</th>
                    <th className={thCls}>SWIFT ref</th>
                    <th className={thCls}>Expected</th>
                    <th className={thCls}>Actual</th>
                    <th className={thCls}>Status</th>
                    <th className={thCls}>Notes / actions</th>
                  </tr>
                </thead>
                <tbody>
                  {nostro.data.breaks.map((b) => (
                    <tr key={b.id}>
                      <td className={tdCls}>{b.id}</td>
                      <td className={tdCls}>{b.category}</td>
                      <td className={tdCls}>{b.swiftReference ?? '—'}</td>
                      <td className={tdCls}>
                        {b.expectedAmount ?? '—'} {b.currency ?? ''}
                      </td>
                      <td className={tdCls}>{b.actualAmount ?? '—'}</td>
                      <td className={tdCls}>
                        <StatusBadge value={b.status || 'UNKNOWN'} />
                      </td>
                      <td className={tdCls}>
                        {b.status === 'OPEN' || b.status === 'INVESTIGATING' ? (
                          <div className="flex items-center gap-1">
                            <input
                              aria-label={`Break ${b.id} notes`}
                              className={inputCls}
                              placeholder="notes"
                              value={breakNotes[b.id] ?? ''}
                              onChange={(e) => {
                                setBreakNotes({ ...breakNotes, [b.id]: e.target.value });
                              }}
                            />
                            {b.status === 'OPEN' ? (
                              <button
                                type="button"
                                className={btnGhost}
                                disabled={breakAct.isPending}
                                onClick={() => {
                                  breakAct.mutate({ id: b.id, action: 'INVESTIGATE' });
                                }}
                              >
                                Investigate
                              </button>
                            ) : null}
                            <button
                              type="button"
                              className={btnGhost}
                              disabled={breakAct.isPending}
                              onClick={() => {
                                breakAct.mutate({ id: b.id, action: 'RESOLVE' });
                              }}
                            >
                              Resolve
                            </button>
                          </div>
                        ) : (
                          <span className={hintTextCls}>closed</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </div>
      ) : null}

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        PB give-up reconciliation
      </h3>
      <div className="mb-2 flex items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="pb-id">
            PB id (0/blank = all)
          </label>
          <input
            id="pb-id"
            className={inputCls}
            value={pbId}
            onChange={(e) => {
              setPbId(e.target.value);
            }}
          />
        </div>
      </div>
      {pb.error !== null ? <ErrorBox error={pb.error} /> : null}
      {pb.data !== undefined ? (
        <div aria-label="PB recon report">
          <p className={hintTextCls}>
            {pb.data.date} — auto-match rate {pb.data.autoMatchRatePct}%
            {pb.data.primeBrokerId > 0 ? ` · PB #${pb.data.primeBrokerId}` : ' · all PBs'}
          </p>
          {pb.data.runs.length > 0 ? (
            <table className={tableCls} aria-label="PB recon runs">
              <thead>
                <tr>
                  <th className={thCls}>Source</th>
                  <th className={thCls}>Scanned</th>
                  <th className={thCls}>Auto-matched</th>
                  <th className={thCls}>Breaks</th>
                </tr>
              </thead>
              <tbody>
                {pb.data.runs.map((r) => (
                  <tr key={r.id}>
                    <td className={tdCls}>{r.source}</td>
                    <td className={tdCls}>{r.tradesScanned}</td>
                    <td className={tdCls}>{r.autoMatched}</td>
                    <td className={tdCls}>{r.breaksDetected}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : null}
          {pb.data.openBreaks.length > 0 ? (
            <table className={tableCls} aria-label="PB open breaks">
              <thead>
                <tr>
                  <th className={thCls}>ID</th>
                  <th className={thCls}>Type</th>
                  <th className={thCls}>Status</th>
                </tr>
              </thead>
              <tbody>
                {pb.data.openBreaks.map((b) => (
                  <tr key={b.id}>
                    <td className={tdCls}>{b.id}</td>
                    <td className={tdCls}>{b.type}</td>
                    <td className={tdCls}>
                      <StatusBadge value={b.status || 'UNKNOWN'} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <p className="text-sm text-neutral-500">No open PB breaks.</p>
          )}
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
