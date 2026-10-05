/**
 * Reg-reporting submissions panel (Phase-10.5 Task 10.5.3.13 §1) —
 * the merged officer queue: open reconciliation breaks + NACKED/
 * FAILED transport rows, break dispositions (RESOLVED|WONT_FIX),
 * submission register + corrected resubmission (creates a NEW
 * submission — the original stays on record), async ACK/NACK ingest,
 * party-identifier upsert, and the manual reconcile sweep.
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
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  fetchRegQueue,
  fetchRegSubmissions,
  ingestAck,
  resolveBreak,
  resubmitSubmission,
  runReconcile,
  upsertPartyIdentifiers,
} from './api';

export function SubmissionsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [resolve, setResolve] = useState({
    id: '',
    resolution: 'RESOLVED' as 'RESOLVED' | 'WONT_FIX',
    notes: '',
  });
  const [resubmit, setResubmit] = useState({ id: '', corrections: '{}' });
  const [ack, setAck] = useState({
    submissionId: '',
    externalRef: '',
    status: 'ACK',
    code: '',
    text: '',
  });
  const [party, setParty] = useState({
    accountId: '',
    lei: '',
    nationalIdType: 'NIDN',
    nationalId: '',
    decisionMakerId: '',
    decisionMakerType: 'NIDN',
  });
  const [reconcileReport, setReconcileReport] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const queue = useQuery({
    queryKey: ['admin-reg-queue'],
    queryFn: () => fetchRegQueue(adminApi),
  });
  const submissions = useQuery({
    queryKey: ['admin-reg-submissions'],
    queryFn: () => fetchRegSubmissions(adminApi),
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-reg-queue'] });
    void qc.invalidateQueries({ queryKey: ['admin-reg-submissions'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const resolveMut = useMutation({
    mutationFn: () => resolveBreak(adminApi, Number(resolve.id), resolve.resolution, resolve.notes),
    onSuccess: (applied) => {
      setNotice(
        applied
          ? `Break #${resolve.id} ${resolve.resolution}.`
          : `Break #${resolve.id} disposition recorded but not applied (already resolved?).`,
      );
      invalidate();
    },
    onError: onErr,
  });
  const resubmitMut = useMutation({
    mutationFn: () =>
      resubmitSubmission(
        adminApi,
        Number(resubmit.id),
        JSON.parse(resubmit.corrections) as Record<string, unknown>,
      ),
    onSuccess: (freshId) => {
      setNotice(`Corrected submission #${freshId} created — original stays on the audit record.`);
      invalidate();
    },
    onError: onErr,
  });
  const ackMut = useMutation({
    mutationFn: () =>
      ingestAck(adminApi, {
        reportSubmissionId: Number(ack.submissionId),
        externalRef: ack.externalRef,
        status: ack.status,
        code: ack.code,
        text: ack.text,
      }),
    onSuccess: () => {
      setNotice('ACK/NACK ingested — submission state reconciled.');
      invalidate();
    },
    onError: onErr,
  });
  const partyMut = useMutation({
    mutationFn: () =>
      upsertPartyIdentifiers(adminApi, {
        accountId: Number(party.accountId),
        lei: party.lei,
        nationalIdType: party.nationalIdType,
        nationalId: party.nationalId,
        decisionMakerId: party.decisionMakerId,
        decisionMakerType: party.decisionMakerType,
      }),
    onSuccess: () => {
      setNotice(`Party identifiers upserted for account ${party.accountId}.`);
    },
    onError: onErr,
  });
  const reconcile = useMutation({
    mutationFn: () => runReconcile(adminApi),
    onSuccess: (report) => {
      setReconcileReport(report);
      setNotice('Reconcile sweep complete.');
      invalidate();
    },
    onError: onErr,
  });

  const denied =
    (queue.error !== null && isAccessDenied(queue.error)) ||
    (submissions.error !== null && isAccessDenied(submissions.error));
  if (denied) return <AccessDeniedCard />;

  return (
    <section className={cardCls} aria-label="Reg submissions queue">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Submissions &amp; repair queue</h2>
        <button
          type="button"
          className={btnGhost}
          disabled={reconcile.isPending}
          onClick={() => {
            reconcile.mutate();
          }}
        >
          Run reconcile
        </button>
      </div>
      {queue.error !== null ? <ErrorBox error={queue.error} /> : null}
      {queue.data !== undefined ? (
        <p className={hintTextCls}>
          {queue.data.breaks.length} open breaks · {queue.data.transport.length} transport rows
          (NACKED/FAILED/REPAIRING)
        </p>
      ) : null}
      {queue.data !== undefined && queue.data.breaks.length > 0 ? (
        <div className="max-h-44 overflow-y-auto" tabIndex={0}>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Break</th>
                <th className={thCls}>Type</th>
                <th className={thCls}>Regime</th>
                <th className={thCls}>UTI</th>
                <th className={thCls}>Detected by</th>
                <th className={thCls}>SLA due</th>
              </tr>
            </thead>
            <tbody>
              {queue.data.breaks.map((b) => (
                <tr key={b.breakId}>
                  <td className={tdCls}>#{b.breakId}</td>
                  <td className={tdCls}>{b.breakType}</td>
                  <td className={tdCls}>{b.regime ?? '—'}</td>
                  <td className={`${tdCls} font-mono text-xs`}>{b.uti ?? '—'}</td>
                  <td className={tdCls}>{b.detectedBy}</td>
                  <td className={tdCls}>{b.slaDueAt.slice(0, 16)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {queue.data !== undefined && queue.data.transport.length > 0 ? (
        <div className="mt-1">
          {queue.data.transport.map((t, i) => (
            <p key={i} className="font-mono text-xs text-neutral-400">
              {JSON.stringify(t.raw)}
            </p>
          ))}
        </div>
      ) : null}

      {submissions.error !== null ? <ErrorBox error={submissions.error} /> : null}
      {submissions.data !== undefined && submissions.data.length > 0 ? (
        <div className="mt-2 max-h-44 overflow-y-auto" tabIndex={0}>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Sub</th>
                <th className={thCls}>Event</th>
                <th className={thCls}>Regime</th>
                <th className={thCls}>Dest</th>
                <th className={thCls}>Attempt</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Error</th>
              </tr>
            </thead>
            <tbody>
              {submissions.data.map((s) => (
                <tr key={s.id}>
                  <td className={tdCls}>#{s.id}</td>
                  <td className={tdCls}>{s.eventId}</td>
                  <td className={tdCls}>{s.regime}</td>
                  <td className={tdCls}>{s.destination}</td>
                  <td className={tdCls}>{s.attempt}</td>
                  <td className={tdCls}>
                    <StatusBadge value={s.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    {s.errorCode !== undefined ? `${s.errorCode} ${s.errorText ?? ''}` : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <form
        aria-label="Resolve break"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(resolve.id) > 0 && resolve.notes !== '') resolveMut.mutate();
        }}
      >
        <input
          aria-label="Break id"
          className={inputCls}
          placeholder="break_id"
          value={resolve.id}
          onChange={(e) => {
            setResolve({ ...resolve, id: e.target.value });
          }}
        />
        <select
          aria-label="Break resolution"
          className={selectCls}
          value={resolve.resolution}
          onChange={(e) => {
            setResolve({ ...resolve, resolution: e.target.value as typeof resolve.resolution });
          }}
        >
          <option value="RESOLVED">RESOLVED</option>
          <option value="WONT_FIX">WONT_FIX</option>
        </select>
        <input
          aria-label="Break notes"
          className={inputCls}
          placeholder="notes"
          value={resolve.notes}
          onChange={(e) => {
            setResolve({ ...resolve, notes: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={resolveMut.isPending}>
          Resolve
        </button>
      </form>

      <form
        aria-label="Resubmit submission"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(resubmit.id) > 0) resubmitMut.mutate();
        }}
      >
        <input
          aria-label="Submission id"
          className={inputCls}
          placeholder="submission id"
          value={resubmit.id}
          onChange={(e) => {
            setResubmit({ ...resubmit, id: e.target.value });
          }}
        />
        <input
          aria-label="Corrections JSON"
          className={inputCls}
          placeholder='corrections {"field":"value"}'
          value={resubmit.corrections}
          onChange={(e) => {
            setResubmit({ ...resubmit, corrections: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={resubmitMut.isPending}>
          Resubmit (corrected)
        </button>
      </form>
      <p className={hintTextCls}>
        Resubmission is a corrected-filing path: a new submission row is created; the NACKED
        original is preserved.
      </p>

      <form
        aria-label="Ingest ACK"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(ack.submissionId) > 0) ackMut.mutate();
        }}
      >
        {(
          [
            ['submissionId', 'submission id'],
            ['externalRef', 'external_ref'],
            ['code', 'code'],
            ['text', 'text'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`ACK ${k}`}
            className={inputCls}
            placeholder={ph}
            value={ack[k]}
            onChange={(e) => {
              setAck({ ...ack, [k]: e.target.value });
            }}
          />
        ))}
        <select
          aria-label="ACK status"
          className={selectCls}
          value={ack.status}
          onChange={(e) => {
            setAck({ ...ack, status: e.target.value });
          }}
        >
          <option value="ACK">ACK</option>
          <option value="NACK">NACK</option>
        </select>
        <button type="submit" className={btnGhost} disabled={ackMut.isPending}>
          Ingest ACK/NACK
        </button>
      </form>

      <form
        aria-label="Upsert party identifiers"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(party.accountId) > 0 && party.lei !== '') partyMut.mutate();
        }}
      >
        {(
          [
            ['accountId', 'account_id'],
            ['lei', 'LEI'],
            ['nationalIdType', 'national_id_type'],
            ['nationalId', 'national_id'],
            ['decisionMakerId', 'decision_maker_id'],
            ['decisionMakerType', 'decision_maker_type'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`Party ${k}`}
            className={inputCls}
            placeholder={ph}
            value={party[k]}
            onChange={(e) => {
              setParty({ ...party, [k]: e.target.value });
            }}
          />
        ))}
        <button type="submit" className={btnGhost} disabled={partyMut.isPending}>
          Upsert identifiers
        </button>
      </form>

      {reconcileReport !== null ? (
        <pre
          className="mt-2 max-h-40 overflow-auto rounded border border-neutral-800 p-2 font-mono text-xs"
          tabIndex={0}
        >
          {JSON.stringify(reconcileReport, null, 2)}
        </pre>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
