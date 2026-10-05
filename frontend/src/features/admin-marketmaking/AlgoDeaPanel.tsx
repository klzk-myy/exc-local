/**
 * Algo certification + DEA + RTS 6 self-assessment panel (Phase-10.5
 * Task 10.5.3.12 §2/§3) —
 *
 *   Algo certs : PENDING|CERTIFIED|SUSPENDED|EXPIRED|REVOKED register,
 *                certify intake (kill-button evidence mandatory),
 *                transition to CERTIFIED|SUSPENDED|REVOKED.
 *   DEA        : per-session controls read/upsert (order qty + msg-rate
 *                limits, sponsoring desk, drop-copy feed) + suspend.
 *   RTS 6      : annual self-assessment register + filing form.
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
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  certifyAlgo,
  fetchAlgoCerts,
  fetchDEAControl,
  fetchRTS6Assessments,
  fileRTS6Assessment,
  setDEAControl,
  suspendDEA,
  transitionAlgoCert,
} from './api';

export function AlgoDeaPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [certStatus, setCertStatus] = useState('');
  const [certForm, setCertForm] = useState({
    algoId: '',
    accountId: '',
    testEvidenceRef: '',
    killTested: false,
    capacityRef: '',
  });
  const [transition, setTransition] = useState({
    id: '',
    to: 'SUSPENDED' as 'CERTIFIED' | 'SUSPENDED' | 'REVOKED',
    reason: '',
  });
  const [deaSession, setDeaSession] = useState('');
  const [deaForm, setDeaForm] = useState({
    sessionId: '',
    accountId: '',
    maxOrderQty: '',
    maxMsgsPerSec: '',
    sponsoringDesk: '',
    dropCopyFeed: '',
  });
  const [deaSuspend, setDeaSuspend] = useState({ sessionId: '', reason: '' });
  const [rts6Form, setRts6Form] = useState({ periodYear: '', documentRef: '', dueAt: '' });
  const [notice, setNotice] = useState<string | null>(null);

  const certs = useQuery({
    queryKey: ['admin-algo-certs', certStatus],
    queryFn: () => fetchAlgoCerts(adminApi, certStatus === '' ? undefined : certStatus),
  });
  const assessments = useQuery({
    queryKey: ['admin-rts6'],
    queryFn: () => fetchRTS6Assessments(adminApi),
  });
  const dea = useQuery({
    queryKey: ['admin-dea', deaSession],
    queryFn: () => fetchDEAControl(adminApi, deaSession),
    enabled: deaSession !== '',
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-algo-certs'] });
    void qc.invalidateQueries({ queryKey: ['admin-rts6'] });
    void qc.invalidateQueries({ queryKey: ['admin-dea'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const certify = useMutation({
    mutationFn: () =>
      certifyAlgo(adminApi, {
        algoId: certForm.algoId,
        accountId: Number(certForm.accountId),
        testEvidenceRef: certForm.testEvidenceRef,
        killButtonTested: certForm.killTested,
        capacityAssessRef: certForm.capacityRef,
      }),
    onSuccess: () => {
      setNotice('Certification filed.');
      invalidate();
    },
    onError: onErr,
  });
  const transMut = useMutation({
    mutationFn: () =>
      transitionAlgoCert(adminApi, Number(transition.id), transition.to, transition.reason),
    onSuccess: () => {
      setNotice(`Certification #${transition.id} → ${transition.to}.`);
      invalidate();
    },
    onError: onErr,
  });
  const deaSet = useMutation({
    mutationFn: () =>
      setDEAControl(adminApi, {
        sessionId: deaForm.sessionId,
        accountId: Number(deaForm.accountId),
        maxOrderQty: deaForm.maxOrderQty,
        maxMsgsPerSec: Number(deaForm.maxMsgsPerSec),
        sponsoringDesk: deaForm.sponsoringDesk,
        dropCopyFeed: deaForm.dropCopyFeed,
      }),
    onSuccess: () => {
      setNotice('DEA limits upserted.');
      invalidate();
    },
    onError: onErr,
  });
  const deaSusp = useMutation({
    mutationFn: () => suspendDEA(adminApi, deaSuspend.sessionId, deaSuspend.reason),
    onSuccess: () => {
      setNotice(`Session ${deaSuspend.sessionId} suspended — DEA access cut.`);
      invalidate();
    },
    onError: onErr,
  });
  const rts6 = useMutation({
    mutationFn: () =>
      fileRTS6Assessment(adminApi, {
        periodYear: Number(rts6Form.periodYear),
        documentRef: rts6Form.documentRef,
        dueAt: rts6Form.dueAt,
      }),
    onSuccess: () => {
      setNotice('Self-assessment filed.');
      invalidate();
    },
    onError: onErr,
  });

  const denied =
    (certs.error !== null && isAccessDenied(certs.error)) ||
    (assessments.error !== null && isAccessDenied(assessments.error));
  if (denied) return <AccessDeniedCard />;

  return (
    <section className={cardCls} aria-label="Algo DEA RTS6 governance">
      <h2 className="mb-2 text-sm font-semibold">Algo certification, DEA &amp; RTS 6</h2>

      <div className="mb-1 flex items-center justify-between gap-2">
        <h3 className="text-sm font-semibold">Algo certifications</h3>
        <select
          aria-label="Cert status filter"
          className={selectCls}
          value={certStatus}
          onChange={(e) => {
            setCertStatus(e.target.value);
          }}
        >
          <option value="">ALL</option>
          <option value="PENDING">PENDING</option>
          <option value="CERTIFIED">CERTIFIED</option>
          <option value="SUSPENDED">SUSPENDED</option>
          <option value="EXPIRED">EXPIRED</option>
          <option value="REVOKED">REVOKED</option>
        </select>
      </div>
      {certs.error !== null ? <ErrorBox error={certs.error} /> : null}
      {certs.data !== undefined && certs.data.length > 0 ? (
        <div className="max-h-48 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Algo</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Kill btn</th>
                <th className={thCls}>Expires</th>
                <th className={thCls}>Review due</th>
              </tr>
            </thead>
            <tbody>
              {certs.data.map((c) => (
                <tr key={c.id}>
                  <td className={tdCls}>{c.id}</td>
                  <td className={tdCls}>{c.algoId}</td>
                  <td className={tdCls}>{c.accountId}</td>
                  <td className={tdCls}>
                    <StatusBadge value={c.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{c.killButtonTested ? 'tested' : '—'}</td>
                  <td className={tdCls}>{c.expiresAt?.slice(0, 10) ?? '—'}</td>
                  <td className={tdCls}>{c.reviewDueAt?.slice(0, 10) ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="Certify algo"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (certForm.algoId !== '' && Number(certForm.accountId) > 0) certify.mutate();
        }}
      >
        <input
          aria-label="Algo id"
          className={inputCls}
          placeholder="algo_id"
          value={certForm.algoId}
          onChange={(e) => {
            setCertForm({ ...certForm, algoId: e.target.value });
          }}
        />
        <input
          aria-label="Algo account"
          className={inputCls}
          placeholder="account_id"
          value={certForm.accountId}
          onChange={(e) => {
            setCertForm({ ...certForm, accountId: e.target.value });
          }}
        />
        <input
          aria-label="Test evidence ref"
          className={inputCls}
          placeholder="test_evidence_ref"
          value={certForm.testEvidenceRef}
          onChange={(e) => {
            setCertForm({ ...certForm, testEvidenceRef: e.target.value });
          }}
        />
        <input
          aria-label="Capacity assessment ref"
          className={inputCls}
          placeholder="capacity_assessment_ref"
          value={certForm.capacityRef}
          onChange={(e) => {
            setCertForm({ ...certForm, capacityRef: e.target.value });
          }}
        />
        <label className="flex items-center gap-1 text-sm">
          <input
            type="checkbox"
            checked={certForm.killTested}
            onChange={(e) => {
              setCertForm({ ...certForm, killTested: e.target.checked });
            }}
          />
          kill-button tested
        </label>
        <button type="submit" className={btnPrimary} disabled={certify.isPending}>
          Certify
        </button>
      </form>
      <form
        aria-label="Transition certification"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(transition.id) > 0 && transition.reason !== '') transMut.mutate();
        }}
      >
        <input
          aria-label="Cert id"
          className={inputCls}
          placeholder="cert id"
          value={transition.id}
          onChange={(e) => {
            setTransition({ ...transition, id: e.target.value });
          }}
        />
        <select
          aria-label="Transition to"
          className={selectCls}
          value={transition.to}
          onChange={(e) => {
            setTransition({
              ...transition,
              to: e.target.value as typeof transition.to,
            });
          }}
        >
          <option value="CERTIFIED">CERTIFIED</option>
          <option value="SUSPENDED">SUSPENDED</option>
          <option value="REVOKED">REVOKED</option>
        </select>
        <input
          aria-label="Transition reason"
          className={inputCls}
          placeholder="reason"
          value={transition.reason}
          onChange={(e) => {
            setTransition({ ...transition, reason: e.target.value });
          }}
        />
        <button
          type="submit"
          className={transition.to === 'CERTIFIED' ? btnPrimary : btnDanger}
          disabled={transMut.isPending}
        >
          Transition
        </button>
      </form>
      <p className={hintTextCls}>
        A CERTIFIED, unexpired registration is the order-path gate — uncertified algos are rejected
        ALGO_NOT_CERTIFIED (422).
      </p>

      <div className="mt-4 border-t border-neutral-800 pt-3">
        <h3 className="mb-1 text-sm font-semibold">DEA session controls</h3>
        <div className="flex flex-wrap items-end gap-2">
          <input
            aria-label="DEA session lookup"
            className={inputCls}
            placeholder="session_id"
            value={deaSession}
            onChange={(e) => {
              setDeaSession(e.target.value);
            }}
          />
          {dea.data !== null && dea.data !== undefined ? (
            <span className={hintTextCls}>
              #{dea.data.id} acct {dea.data.accountId} · qty≤{dea.data.maxOrderQty} ·{' '}
              {dea.data.maxMsgsPerSec}msg/s · desk {dea.data.sponsoringDesk} · drop-copy{' '}
              {dea.data.dropCopyFeed} · {dea.data.status}
            </span>
          ) : null}
          {dea.data === null && deaSession !== '' && !dea.isLoading ? (
            <span className={hintTextCls}>no controls on file for this session</span>
          ) : null}
        </div>
        {dea.error !== null ? <ErrorBox error={dea.error} /> : null}
        <form
          aria-label="Set DEA limits"
          className="mt-2 flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (deaForm.sessionId !== '' && Number(deaForm.accountId) > 0) deaSet.mutate();
          }}
        >
          {(
            [
              ['sessionId', 'session_id'],
              ['accountId', 'account_id'],
              ['maxOrderQty', 'max_order_qty'],
              ['maxMsgsPerSec', 'max_msgs_per_sec'],
              ['sponsoringDesk', 'sponsoring_desk'],
              ['dropCopyFeed', 'drop_copy_feed'],
            ] as const
          ).map(([k, ph]) => (
            <input
              key={k}
              aria-label={`DEA ${k}`}
              className={inputCls}
              placeholder={ph}
              value={deaForm[k]}
              onChange={(e) => {
                setDeaForm({ ...deaForm, [k]: e.target.value });
              }}
            />
          ))}
          <button type="submit" className={btnPrimary} disabled={deaSet.isPending}>
            Upsert limits
          </button>
        </form>
        <form
          aria-label="Suspend DEA session"
          className="mt-2 flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (deaSuspend.sessionId !== '') deaSusp.mutate();
          }}
        >
          <input
            aria-label="Suspend session id"
            className={inputCls}
            placeholder="session_id"
            value={deaSuspend.sessionId}
            onChange={(e) => {
              setDeaSuspend({ ...deaSuspend, sessionId: e.target.value });
            }}
          />
          <input
            aria-label="Suspend reason"
            className={inputCls}
            placeholder="reason"
            value={deaSuspend.reason}
            onChange={(e) => {
              setDeaSuspend({ ...deaSuspend, reason: e.target.value });
            }}
          />
          <button type="submit" className={btnDanger} disabled={deaSusp.isPending}>
            Suspend session
          </button>
        </form>
      </div>

      <div className="mt-4 border-t border-neutral-800 pt-3">
        <h3 className="mb-1 text-sm font-semibold">RTS 6 self-assessments</h3>
        {assessments.error !== null ? <ErrorBox error={assessments.error} /> : null}
        {assessments.data !== undefined && assessments.data.length > 0 ? (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Year</th>
                <th className={thCls}>Document</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Filed</th>
              </tr>
            </thead>
            <tbody>
              {assessments.data.map((a) => (
                <tr key={a.id}>
                  <td className={tdCls}>{a.id}</td>
                  <td className={tdCls}>{a.periodYear}</td>
                  <td className={tdCls}>{a.documentRef}</td>
                  <td className={tdCls}>
                    <StatusBadge value={a.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{a.filedAt?.slice(0, 10) ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
        <form
          aria-label="File RTS6 assessment"
          className="mt-2 flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (Number(rts6Form.periodYear) > 2000 && rts6Form.documentRef !== '') rts6.mutate();
          }}
        >
          <input
            aria-label="Period year"
            className={inputCls}
            placeholder="period_year"
            value={rts6Form.periodYear}
            onChange={(e) => {
              setRts6Form({ ...rts6Form, periodYear: e.target.value });
            }}
          />
          <input
            aria-label="Document ref"
            className={inputCls}
            placeholder="document_ref"
            value={rts6Form.documentRef}
            onChange={(e) => {
              setRts6Form({ ...rts6Form, documentRef: e.target.value });
            }}
          />
          <input
            aria-label="Due at"
            className={inputCls}
            type="date"
            value={rts6Form.dueAt}
            onChange={(e) => {
              setRts6Form({ ...rts6Form, dueAt: e.target.value });
            }}
          />
          <button type="submit" className={btnGhost} disabled={rts6.isPending}>
            File assessment
          </button>
        </form>
      </div>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
