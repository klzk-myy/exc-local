/**
 * Client-money assurance panel (Phase-10.5 Task 10.5.3.8 §3) — the
 * safeguarding engagement register: open an audit engagement,
 * assemble the system-of-record evidence pack (hashed, never manual),
 * and issue the segregation certification under §8.2 four-eyes
 * (approver_id must be a distinct principal).
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
import {
  assembleEvidencePack,
  createClientMoneyAudit,
  fetchCertifications,
  fetchClientMoneyAudits,
  issueCertification,
} from './api';

export function ClientMoneyPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [auditForm, setAuditForm] = useState({
    year: String(new Date().getUTCFullYear()),
    firm: '',
    scope: 'SAFEGUARDING',
    periodStart: '',
    periodEnd: '',
  });
  const [certForm, setCertForm] = useState({
    auditId: '',
    packId: '',
    statement: '',
    signatory: '',
    until: '',
    approverId: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const audits = useQuery({
    queryKey: ['admin-client-money-audits'],
    queryFn: () => fetchClientMoneyAudits(adminApi),
  });
  const certs = useQuery({
    queryKey: ['admin-client-money-certs'],
    queryFn: () => fetchCertifications(adminApi),
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-client-money-audits'] });
    void qc.invalidateQueries({ queryKey: ['admin-client-money-certs'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const createAudit = useMutation({
    mutationFn: () =>
      createClientMoneyAudit(adminApi, {
        engagementYear: Number(auditForm.year),
        auditorFirm: auditForm.firm,
        scope: auditForm.scope,
        periodStart: auditForm.periodStart,
        periodEnd: auditForm.periodEnd,
      }),
    onSuccess: () => {
      setNotice('Audit engagement opened.');
      invalidate();
    },
    onError: onErr,
  });
  const pack = useMutation({
    mutationFn: (auditId: number) => assembleEvidencePack(adminApi, auditId),
    onSuccess: (p) =>
      setNotice(`Evidence pack #${p.id} assembled — sha256 ${p.packSha256.slice(0, 16)}…`),
    onError: onErr,
  });
  const certify = useMutation({
    mutationFn: () =>
      issueCertification(adminApi, {
        auditId: Number(certForm.auditId),
        evidencePackId: Number(certForm.packId),
        statement: certForm.statement,
        signatory: certForm.signatory,
        publishedUntil: certForm.until,
        approverId: Number(certForm.approverId),
      }),
    onSuccess: () => {
      setNotice('Segregation certification issued (dual control applied).');
      invalidate();
    },
    onError: onErr,
  });

  if (audits.error !== null && isAccessDenied(audits.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Client money">
      <h2 className="mb-1 text-sm font-semibold">Client-money safeguarding</h2>
      <p className={hintTextCls}>
        Independent assurance register — engagements, system-assembled evidence packs and the
        dual-controlled segregation certifications (§17.9 / Task 24.3.18).
      </p>

      {audits.error !== null ? <ErrorBox error={audits.error} /> : null}
      {audits.data !== undefined && audits.data.length > 0 ? (
        <div className="mt-2 max-h-44 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Year</th>
                <th className={thCls}>Auditor</th>
                <th className={thCls}>Scope</th>
                <th className={thCls}>Period</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Independence</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {audits.data.map((a) => (
                <tr key={a.id}>
                  <td className={tdCls}>{a.id}</td>
                  <td className={tdCls}>{a.engagementYear}</td>
                  <td className={tdCls}>{a.auditorFirm}</td>
                  <td className={tdCls}>{a.scope}</td>
                  <td className={tdCls}>
                    {a.periodStart.slice(0, 10)}→{a.periodEnd.slice(0, 10)}
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={a.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={a.independenceConfirmed ? 'CONFIRMED' : 'PENDING'} />
                  </td>
                  <td className={tdCls}>
                    <button
                      type="button"
                      className={btnGhost}
                      disabled={pack.isPending}
                      onClick={() => {
                        pack.mutate(a.id);
                      }}
                    >
                      Evidence pack
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="mt-2 text-sm text-neutral-500">No audit engagements.</p>
      )}

      <form
        aria-label="Open audit engagement"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (auditForm.firm !== '' && auditForm.periodStart !== '' && auditForm.periodEnd !== '') {
            createAudit.mutate();
          }
        }}
      >
        <div>
          <label className={labelCls} htmlFor="au-year">
            Year
          </label>
          <input
            id="au-year"
            className={inputCls}
            value={auditForm.year}
            onChange={(e) => {
              setAuditForm({ ...auditForm, year: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="au-firm">
            Auditor firm
          </label>
          <input
            id="au-firm"
            className={inputCls}
            value={auditForm.firm}
            onChange={(e) => {
              setAuditForm({ ...auditForm, firm: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="au-start">
            Period start
          </label>
          <input
            id="au-start"
            type="date"
            className={inputCls}
            value={auditForm.periodStart}
            onChange={(e) => {
              setAuditForm({ ...auditForm, periodStart: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="au-end">
            Period end
          </label>
          <input
            id="au-end"
            type="date"
            className={inputCls}
            value={auditForm.periodEnd}
            onChange={(e) => {
              setAuditForm({ ...auditForm, periodEnd: e.target.value });
            }}
          />
        </div>
        <button type="submit" className={btnGhost} disabled={createAudit.isPending}>
          Open engagement
        </button>
      </form>

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        Segregation certifications
      </h3>
      {certs.data !== undefined && certs.data.length > 0 ? (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>ID</th>
              <th className={thCls}>Audit</th>
              <th className={thCls}>Pack hash</th>
              <th className={thCls}>Issuer/Approver</th>
              <th className={thCls}>Published until</th>
              <th className={thCls}>Status</th>
            </tr>
          </thead>
          <tbody>
            {certs.data.map((c) => (
              <tr key={c.id}>
                <td className={tdCls}>{c.id}</td>
                <td className={tdCls}>{c.auditId}</td>
                <td className={tdCls} title={c.packSha256}>
                  {c.packSha256.slice(0, 12)}…
                </td>
                <td className={tdCls}>
                  {c.issuedBy}/{c.approvedBy}
                </td>
                <td className={tdCls}>{c.publishedUntil.slice(0, 10)}</td>
                <td className={tdCls}>
                  <StatusBadge value={c.status || 'UNKNOWN'} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="text-sm text-neutral-500">No certifications issued.</p>
      )}
      <form
        aria-label="Issue certification"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (
            Number(certForm.auditId) > 0 &&
            Number(certForm.packId) > 0 &&
            certForm.statement !== '' &&
            certForm.signatory !== '' &&
            certForm.until !== '' &&
            Number(certForm.approverId) > 0
          ) {
            certify.mutate();
          }
        }}
      >
        <input
          aria-label="Audit id"
          className={inputCls}
          placeholder="audit_id"
          value={certForm.auditId}
          onChange={(e) => {
            setCertForm({ ...certForm, auditId: e.target.value });
          }}
        />
        <input
          aria-label="Evidence pack id"
          className={inputCls}
          placeholder="evidence_pack_id"
          value={certForm.packId}
          onChange={(e) => {
            setCertForm({ ...certForm, packId: e.target.value });
          }}
        />
        <input
          aria-label="Statement"
          className={inputCls}
          placeholder="statement"
          value={certForm.statement}
          onChange={(e) => {
            setCertForm({ ...certForm, statement: e.target.value });
          }}
        />
        <input
          aria-label="Signatory name"
          className={inputCls}
          placeholder="signatory name"
          value={certForm.signatory}
          onChange={(e) => {
            setCertForm({ ...certForm, signatory: e.target.value });
          }}
        />
        <input
          aria-label="Published until"
          className={inputCls}
          placeholder="YYYY-MM-DD"
          value={certForm.until}
          onChange={(e) => {
            setCertForm({ ...certForm, until: e.target.value });
          }}
        />
        <input
          aria-label="Approver id"
          className={inputCls}
          placeholder="approver_id (≠ you)"
          value={certForm.approverId}
          onChange={(e) => {
            setCertForm({ ...certForm, approverId: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={certify.isPending}>
          Issue (4-eyes)
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
