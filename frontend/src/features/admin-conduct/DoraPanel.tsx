/**
 * Governance & DORA panel (Phase-10.5 Task 10.5.3.15 §4) —
 * governance packs (list + hash_ok re-verification + on-demand
 * rebuild + maker-checker release), quarterly recertification
 * campaigns (open + report + per-binding decisions), data-residency
 * policy map + cross-border access audit, and the DORA ICT provider
 * register (create/update/retire + due-obligation sweep + review
 * events incl. substitution tests).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
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
  fetchICTDue,
  fetchICTProviders,
  fetchPacks,
  fetchRecertReport,
  fetchResidencyAccessLog,
  fetchResidencyPolicies,
  generatePack,
  recertDecide,
  recordICTReview,
  releasePack,
  retireICTProvider,
  startRecert,
  upsertICTProvider,
} from './api';

export function DoraPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [packKind, setPackKind] = useState('CEO_DAILY');
  const [packLabel, setPackLabel] = useState('');
  const [release, setRelease] = useState({ id: '', approver: '', reason: '' });
  const [recert, setRecert] = useState({ label: '', endsAt: '' });
  const [decision, setDecision] = useState({
    campaignId: '',
    bindingId: '',
    approve: 'true',
    note: '',
  });
  const [report, setReport] = useState<Record<string, unknown> | null>(null);
  const [recertId, setRecertId] = useState('');
  const [ict, setIct] = useState({
    id: '',
    name: '',
    service: '',
    concentration: 'LOW',
    owner: '',
    renewalAt: '',
    nextReviewAt: '',
  });
  const [ictReview, setIctReview] = useState({
    id: '',
    kind: 'REVIEW',
    outcome: 'PASS',
    evidenceRef: '',
    notes: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const packs = useQuery({
    queryKey: ['admin-gov-packs'],
    queryFn: () => fetchPacks(adminApi),
  });
  const policies = useQuery({
    queryKey: ['admin-residency-policies'],
    queryFn: () => fetchResidencyPolicies(adminApi),
  });
  const accessLog = useQuery({
    queryKey: ['admin-residency-access'],
    queryFn: () => fetchResidencyAccessLog(adminApi),
  });
  const providers = useQuery({
    queryKey: ['admin-ict-providers'],
    queryFn: () => fetchICTProviders(adminApi),
  });
  const due = useQuery({
    queryKey: ['admin-ict-due'],
    queryFn: () => fetchICTDue(adminApi),
  });
  const invalidate = () => {
    for (const k of ['admin-gov-packs', 'admin-ict-providers', 'admin-ict-due']) {
      void qc.invalidateQueries({ queryKey: [k] });
    }
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const genMut = useMutation({
    mutationFn: () =>
      generatePack(adminApi, { kind: packKind, ...(packLabel !== '' ? { label: packLabel } : {}) }),
    onSuccess: () => {
      setNotice(`Governance pack generated (${packKind}).`);
      invalidate();
    },
    onError: onErr,
  });
  const releaseMut = useMutation({
    mutationFn: () =>
      releasePack(adminApi, Number(release.id), Number(release.approver), release.reason),
    onSuccess: () => {
      setNotice(`Pack #${release.id} released (maker-checker).`);
      invalidate();
    },
    onError: onErr,
  });
  const recertMut = useMutation({
    mutationFn: () => startRecert(adminApi, { label: recert.label, ends_at: recert.endsAt }),
    onSuccess: (id) => {
      setNotice(`Recertification campaign #${id} opened.`);
    },
    onError: onErr,
  });
  const reportMut = useMutation({
    mutationFn: () => fetchRecertReport(adminApi, Number(recertId)),
    onSuccess: (r) => {
      setReport(r);
    },
    onError: onErr,
  });
  const decideMut = useMutation({
    mutationFn: () =>
      recertDecide(adminApi, Number(decision.campaignId), {
        binding_id: Number(decision.bindingId),
        approve: decision.approve === 'true',
        ...(decision.note !== '' ? { note: decision.note } : {}),
      }),
    onSuccess: () => {
      setNotice(`Recert decision recorded on binding ${decision.bindingId}.`);
    },
    onError: onErr,
  });
  const ictMut = useMutation({
    mutationFn: () =>
      upsertICTProvider(apiClient, adminApi.env, ict.id === '' ? null : Number(ict.id), {
        name: ict.name,
        ict_service: ict.service,
        concentration: ict.concentration,
        ...(ict.owner !== '' ? { owner: ict.owner } : {}),
        ...(ict.renewalAt !== '' ? { renewal_at: ict.renewalAt } : {}),
        ...(ict.nextReviewAt !== '' ? { next_review_at: ict.nextReviewAt } : {}),
      }),
    onSuccess: () => {
      setNotice(ict.id === '' ? 'ICT provider registered.' : `ICT provider #${ict.id} updated.`);
      invalidate();
    },
    onError: onErr,
  });
  const retireMut = useMutation({
    mutationFn: (id: number) => retireICTProvider(apiClient, adminApi.env, id),
    onSuccess: () => {
      setNotice('Provider retired — record retained.');
      invalidate();
    },
    onError: onErr,
  });
  const ictReviewMut = useMutation({
    mutationFn: () =>
      recordICTReview(adminApi, Number(ictReview.id), {
        kind: ictReview.kind,
        outcome: ictReview.outcome,
        ...(ictReview.evidenceRef !== '' ? { evidence_ref: ictReview.evidenceRef } : {}),
        ...(ictReview.notes !== '' ? { notes: ictReview.notes } : {}),
      }),
    onSuccess: () => {
      setNotice(`Provider review recorded (#${ictReview.id}).`);
      invalidate();
    },
    onError: onErr,
  });

  const denied =
    (packs.error !== null && isAccessDenied(packs.error)) ||
    (providers.error !== null && isAccessDenied(providers.error)) ||
    (policies.error !== null && isAccessDenied(policies.error));
  if (denied) return <AccessDeniedCard />;

  return (
    <section className={cardCls} aria-label="Governance and DORA">
      <h2 className="mb-2 text-sm font-semibold">Governance packs, recert &amp; DORA</h2>

      <h3 className="mb-1 text-sm font-medium">Governance packs</h3>
      {packs.error !== null ? <ErrorBox error={packs.error} /> : null}
      {packs.data !== undefined && packs.data.length > 0 ? (
        <div className="max-h-36 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Kind</th>
                <th className={thCls}>Period</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Hash</th>
                <th className={thCls}>Released</th>
              </tr>
            </thead>
            <tbody>
              {packs.data.map((p) => (
                <tr key={p.packId}>
                  <td className={tdCls}>{p.packId}</td>
                  <td className={tdCls}>{p.kind}</td>
                  <td className={tdCls}>{p.period}</td>
                  <td className={tdCls}>
                    <StatusBadge value={p.status || 'GENERATED'} />
                  </td>
                  <td className={`${tdCls} font-mono text-xs`}>{p.contentHash.slice(0, 12)}…</td>
                  <td className={tdCls}>{p.releasedAt?.slice(0, 10) ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <div className="mt-2 flex flex-wrap items-end gap-2">
        <select
          aria-label="Pack kind"
          className={selectCls}
          value={packKind}
          onChange={(e) => {
            setPackKind(e.target.value);
          }}
        >
          {['CEO_DAILY', 'BOARD_QUARTERLY', 'BOARD_ADHOC'].map((k) => (
            <option key={k} value={k}>
              {k}
            </option>
          ))}
        </select>
        <input
          aria-label="Pack label"
          className={inputCls}
          placeholder="label/date"
          value={packLabel}
          onChange={(e) => {
            setPackLabel(e.target.value);
          }}
        />
        <button
          type="button"
          className={btnGhost}
          disabled={genMut.isPending}
          onClick={() => {
            genMut.mutate();
          }}
        >
          Generate
        </button>
        <input
          aria-label="Release pack id"
          className={inputCls}
          placeholder="pack_id"
          value={release.id}
          onChange={(e) => {
            setRelease({ ...release, id: e.target.value });
          }}
        />
        <input
          aria-label="Release approver"
          className={inputCls}
          placeholder="approver_id"
          value={release.approver}
          onChange={(e) => {
            setRelease({ ...release, approver: e.target.value });
          }}
        />
        <input
          aria-label="Release reason"
          className={inputCls}
          placeholder="reason"
          value={release.reason}
          onChange={(e) => {
            setRelease({ ...release, reason: e.target.value });
          }}
        />
        <button
          type="button"
          className={btnGhost}
          disabled={
            releaseMut.isPending || Number(release.id) <= 0 || Number(release.approver) <= 0
          }
          onClick={() => {
            releaseMut.mutate();
          }}
        >
          Release (4-eyes)
        </button>
      </div>

      <h3 className="mb-1 mt-4 text-sm font-medium">Recertification campaigns</h3>
      <div className="flex flex-wrap items-end gap-2">
        <input
          aria-label="Campaign label"
          className={inputCls}
          placeholder="label"
          value={recert.label}
          onChange={(e) => {
            setRecert({ ...recert, label: e.target.value });
          }}
        />
        <input
          aria-label="Campaign ends at"
          className={inputCls}
          type="date"
          value={recert.endsAt}
          onChange={(e) => {
            setRecert({ ...recert, endsAt: e.target.value });
          }}
        />
        <button
          type="button"
          className={btnGhost}
          disabled={recertMut.isPending || recert.label === '' || recert.endsAt === ''}
          onClick={() => {
            recertMut.mutate();
          }}
        >
          Open campaign
        </button>
        <input
          aria-label="Report campaign id"
          className={inputCls}
          placeholder="campaign_id"
          value={recertId}
          onChange={(e) => {
            setRecertId(e.target.value);
          }}
        />
        <button
          type="button"
          className={btnGhost}
          disabled={Number(recertId) <= 0}
          onClick={() => {
            reportMut.mutate();
          }}
        >
          Load report
        </button>
      </div>
      <form
        aria-label="Recert decision"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(decision.campaignId) > 0 && Number(decision.bindingId) > 0) {
            decideMut.mutate();
          }
        }}
      >
        <input
          aria-label="Decision campaign id"
          className={inputCls}
          placeholder="campaign_id"
          value={decision.campaignId}
          onChange={(e) => {
            setDecision({ ...decision, campaignId: e.target.value });
          }}
        />
        <input
          aria-label="Decision binding id"
          className={inputCls}
          placeholder="binding_id"
          value={decision.bindingId}
          onChange={(e) => {
            setDecision({ ...decision, bindingId: e.target.value });
          }}
        />
        <select
          aria-label="Decision approve"
          className={selectCls}
          value={decision.approve}
          onChange={(e) => {
            setDecision({ ...decision, approve: e.target.value });
          }}
        >
          <option value="true">APPROVE</option>
          <option value="false">REVOKE</option>
        </select>
        <input
          aria-label="Decision note"
          className={inputCls}
          placeholder="note"
          value={decision.note}
          onChange={(e) => {
            setDecision({ ...decision, note: e.target.value });
          }}
        />
        <button type="submit" className={btnGhost} disabled={decideMut.isPending}>
          Record decision
        </button>
      </form>
      {report !== null ? (
        <pre className="mt-2 max-h-40 overflow-auto rounded border border-neutral-800 p-2 font-mono text-xs">
          {JSON.stringify(report, null, 2)}
        </pre>
      ) : null}

      <h3 className="mb-1 mt-4 text-sm font-medium">Data residency</h3>
      {policies.error !== null ? <ErrorBox error={policies.error} /> : null}
      {policies.data !== undefined && policies.data.length > 0 ? (
        <div className="max-h-28 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Jurisdiction</th>
                <th className={thCls}>Home region</th>
                <th className={thCls}>KMS key</th>
                <th className={thCls}>Bucket</th>
                <th className={thCls}>Adequate</th>
                <th className={thCls}>Transfer instrument</th>
              </tr>
            </thead>
            <tbody>
              {policies.data.map((p) => (
                <tr key={p.jurisdictionCode}>
                  <td className={tdCls}>{p.jurisdictionCode}</td>
                  <td className={tdCls}>{p.homeRegion}</td>
                  <td className={tdCls}>{p.kmsKeyId}</td>
                  <td className={tdCls}>{p.s3Bucket}</td>
                  <td className={tdCls}>{p.adequate ? 'YES' : 'NO'}</td>
                  <td className={tdCls}>{p.transferInstrument}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {accessLog.data !== undefined && accessLog.data.length > 0 ? (
        <div className="mt-1 max-h-24 overflow-y-auto">
          {accessLog.data.map((row, i) => (
            <p key={i} className="font-mono text-xs text-neutral-400">
              {JSON.stringify(row)}
            </p>
          ))}
        </div>
      ) : null}

      <h3 className="mb-1 mt-4 text-sm font-medium">DORA ICT providers</h3>
      {due.data !== undefined && due.data.length > 0 ? (
        <div className="mb-1 rounded border border-amber-800/50 bg-amber-950/30 p-2">
          {due.data.map((a, i) => (
            <p key={i} className="text-xs text-amber-300">
              {a.code} · {a.name} · {a.summary} · due {a.dueAt.slice(0, 10)}
            </p>
          ))}
        </div>
      ) : null}
      {providers.error !== null ? <ErrorBox error={providers.error} /> : null}
      {providers.data !== undefined && providers.data.length > 0 ? (
        <div className="max-h-32 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Name</th>
                <th className={thCls}>Service</th>
                <th className={thCls}>Concentration</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Next review</th>
                <th className={thCls}>
                  <span className="sr-only">Retire</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {providers.data.map((p) => (
                <tr key={p.id}>
                  <td className={tdCls}>{p.id}</td>
                  <td className={tdCls}>{p.name}</td>
                  <td className={tdCls}>{p.ictService}</td>
                  <td className={tdCls}>{p.concentration}</td>
                  <td className={tdCls}>
                    <StatusBadge value={p.status || 'ACTIVE'} />
                  </td>
                  <td className={tdCls}>{p.nextReviewAt?.slice(0, 10) ?? '—'}</td>
                  <td className={tdCls}>
                    {p.status !== 'RETIRED' ? (
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          retireMut.mutate(p.id);
                        }}
                      >
                        Retire
                      </button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <form
        aria-label="ICT provider upsert"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (ict.name !== '' && ict.service !== '') ictMut.mutate();
        }}
      >
        <input
          aria-label="ICT id (blank = create)"
          className={inputCls}
          placeholder="id (blank = create)"
          value={ict.id}
          onChange={(e) => {
            setIct({ ...ict, id: e.target.value });
          }}
        />
        {(
          [
            ['name', 'name'],
            ['service', 'ict_service'],
            ['owner', 'owner'],
            ['renewalAt', 'renewal_at'],
            ['nextReviewAt', 'next_review_at'],
          ] as const
        ).map(([k, ph]) => (
          <input
            key={k}
            aria-label={`ICT ${k}`}
            className={inputCls}
            placeholder={ph}
            value={ict[k]}
            onChange={(e) => {
              setIct({ ...ict, [k]: e.target.value });
            }}
          />
        ))}
        <select
          aria-label="ICT concentration"
          className={selectCls}
          value={ict.concentration}
          onChange={(e) => {
            setIct({ ...ict, concentration: e.target.value });
          }}
        >
          {['LOW', 'MEDIUM', 'HIGH'].map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
        <button type="submit" className={btnPrimary} disabled={ictMut.isPending}>
          Register / update
        </button>
      </form>
      <form
        aria-label="ICT review event"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(ictReview.id) > 0) ictReviewMut.mutate();
        }}
      >
        <input
          aria-label="Review provider id"
          className={inputCls}
          placeholder="provider_id"
          value={ictReview.id}
          onChange={(e) => {
            setIctReview({ ...ictReview, id: e.target.value });
          }}
        />
        <select
          aria-label="Review kind"
          className={selectCls}
          value={ictReview.kind}
          onChange={(e) => {
            setIctReview({ ...ictReview, kind: e.target.value });
          }}
        >
          {['REVIEW', 'RENEWAL', 'SUBSTITUTION_TEST'].map((k) => (
            <option key={k} value={k}>
              {k}
            </option>
          ))}
        </select>
        <input
          aria-label="Review outcome"
          className={inputCls}
          placeholder="outcome"
          value={ictReview.outcome}
          onChange={(e) => {
            setIctReview({ ...ictReview, outcome: e.target.value });
          }}
        />
        <input
          aria-label="Review evidence ref"
          className={inputCls}
          placeholder="evidence_ref"
          value={ictReview.evidenceRef}
          onChange={(e) => {
            setIctReview({ ...ictReview, evidenceRef: e.target.value });
          }}
        />
        <button type="submit" className={btnGhost} disabled={ictReviewMut.isPending}>
          Record event
        </button>
      </form>
      <p className={hintTextCls}>
        DORA register covers terms, exit plan and substitution-test cadence; the due sweep flags
        overdue reviews, renewals and stale exit tests.
      </p>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
