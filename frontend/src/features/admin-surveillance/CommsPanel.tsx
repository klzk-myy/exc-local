/**
 * Comms-recording panel (Phase-10.5 Task 10.5.3.6 §5) — the MiFID II
 * taping register: WORM-sealed recordings with sha256 + day-chain
 * hashes, a per-day chain verification action, and dual-controlled
 * content retrieval (distinct approver_id + mandatory justification —
 * the server rejects self-approval with DUAL_CONTROL_VIOLATION).
 */
import { useMutation, useQuery } from '@tanstack/react-query';
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
import {
  fetchCommsRecordings,
  retrieveCommsRecording,
  verifyCommsDay,
  type RetrievedRecording,
} from './api';

export function CommsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [acctFilter, setAcctFilter] = useState('');
  const [selected, setSelected] = useState<number | null>(null);
  const [retrieveForm, setRetrieveForm] = useState({
    approverId: '',
    justification: '',
    caseRef: '',
  });
  const [retrieved, setRetrieved] = useState<RetrievedRecording | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [verifyDay, setVerifyDay] = useState('');
  const [verifyResult, setVerifyResult] = useState<string | null>(null);

  const list = useQuery({
    queryKey: ['admin-comms', acctFilter],
    queryFn: () => fetchCommsRecordings(adminApi, Number(acctFilter)),
  });

  const retrieve = useMutation({
    mutationFn: () =>
      retrieveCommsRecording(adminApi, selected ?? 0, {
        approverId: Number(retrieveForm.approverId),
        justification: retrieveForm.justification,
        caseRef: retrieveForm.caseRef === '' ? undefined : retrieveForm.caseRef,
      }),
    onSuccess: (r) => {
      setRetrieved(r);
      setNotice('Recording retrieved — access logged + audit-chained.');
    },
    onError: (e) => {
      setRetrieved(null);
      setNotice(e instanceof Error ? e.message : 'Retrieve failed');
    },
  });

  const verify = useMutation({
    mutationFn: () => verifyCommsDay(adminApi, verifyDay),
    onSuccess: (r) =>
      setVerifyResult(r.chainOk ? `${r.day}: chain INTACT` : `${r.day}: chain VIOLATION`),
    onError: (e) => setVerifyResult(e instanceof Error ? e.message : 'Verify failed'),
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Comms recordings">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Comms recordings</h2>
        <div className="flex items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="comms-acct">
              Account
            </label>
            <input
              id="comms-acct"
              className={inputCls}
              value={acctFilter}
              onChange={(e) => {
                setAcctFilter(e.target.value);
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="comms-day">
              Verify day
            </label>
            <input
              id="comms-day"
              className={inputCls}
              placeholder="YYYY-MM-DD"
              value={verifyDay}
              onChange={(e) => {
                setVerifyDay(e.target.value);
              }}
            />
          </div>
          <button
            type="button"
            className={btnGhost}
            disabled={verify.isPending || verifyDay === ''}
            onClick={() => verify.mutate()}
          >
            Verify chain
          </button>
        </div>
      </div>
      <p className={hintTextCls}>
        WORM register — retrieval is dual-controlled; every access writes an audit-chain row.
      </p>
      {verifyResult !== null ? <p className="text-sm">{verifyResult}</p> : null}

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="mt-2 text-sm text-neutral-500">No recordings.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="mt-2 max-h-56 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Channel</th>
                <th className={thCls}>Dir</th>
                <th className={thCls}>Source</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Started</th>
                <th className={thCls}>sha256</th>
                <th className={thCls}>Sealed</th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((r) => (
                <tr
                  key={r.recordingId}
                  className="cursor-pointer"
                  onClick={() => {
                    setSelected(r.recordingId);
                    setRetrieved(null);
                    setNotice(null);
                  }}
                >
                  <td className={tdCls}>{r.recordingId}</td>
                  <td className={tdCls}>{r.channel}</td>
                  <td className={tdCls}>{r.direction}</td>
                  <td className={tdCls}>{r.source}</td>
                  <td className={tdCls}>{r.accountId ?? '—'}</td>
                  <td className={tdCls}>{r.startedAt}</td>
                  <td className={tdCls} title={r.sha256}>
                    {r.sha256.slice(0, 12)}…
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={r.sealed ? 'SEALED' : 'UNSEALED'} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {selected !== null ? (
        <form
          aria-label="Retrieve recording"
          className="mt-3 space-y-2 rounded border border-neutral-700 p-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (Number(retrieveForm.approverId) > 0 && retrieveForm.justification !== '') {
              retrieve.mutate();
            }
          }}
        >
          <p className="text-sm">
            Retrieve recording <strong>#{selected}</strong> — requires a distinct approver.
          </p>
          <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
            <div>
              <label className={labelCls} htmlFor="comms-approver">
                Approver id
              </label>
              <input
                id="comms-approver"
                className={inputCls}
                value={retrieveForm.approverId}
                onChange={(e) => {
                  setRetrieveForm({ ...retrieveForm, approverId: e.target.value });
                }}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="comms-just">
                Justification
              </label>
              <input
                id="comms-just"
                className={inputCls}
                value={retrieveForm.justification}
                onChange={(e) => {
                  setRetrieveForm({ ...retrieveForm, justification: e.target.value });
                }}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="comms-case">
                Case ref
              </label>
              <input
                id="comms-case"
                className={inputCls}
                value={retrieveForm.caseRef}
                onChange={(e) => {
                  setRetrieveForm({ ...retrieveForm, caseRef: e.target.value });
                }}
              />
            </div>
          </div>
          <button type="submit" className={btnPrimary} disabled={retrieve.isPending}>
            Retrieve (dual-control)
          </button>
        </form>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
      {retrieved !== null ? (
        <div
          aria-label="Retrieved recording"
          className="mt-2 rounded border border-neutral-700 p-3 text-sm"
        >
          <p className={hintTextCls}>
            sha256 {retrieved.recording.sha256} · chain {retrieved.recording.chainHash.slice(0, 20)}
            …
          </p>
          <pre className="mt-1 max-h-36 overflow-y-auto whitespace-pre-wrap rounded bg-neutral-900 p-2 text-xs">
            {retrieved.bodyPreview === '' ? '(empty body)' : retrieved.bodyPreview}
          </pre>
        </div>
      ) : null}
    </section>
  );
}
