/**
 * Audit-integrity panel (Phase-10.5 Task 10.5.3.3 §1) — the WORM lane:
 * filtered audit-log explorer (same handler as the Task 10.3.6 panel,
 * mounted here for the integrity workflow), one-click hash-chain
 * verification per day (Task 7.3.3), and the raw audit_hash_chain
 * slice for forensic inspection (Task 21.3.27).
 */
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import { fetchAuditLog, type AuditRow } from '@/lib/admin/api';
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

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import { fetchAuditChain, renderScalar, verifyAuditChain } from './api';

function AuditExplorer({ adminApi }: { adminApi: BoundAdminApi }) {
  const [action, setAction] = useState('');
  const [targetType, setTargetType] = useState('');
  const [applied, setApplied] = useState({ action: '', targetType: '' });

  const query = useQuery({
    queryKey: ['admin-int', 'audit', adminApi.env, applied],
    queryFn: () =>
      fetchAuditLog(adminApi, {
        action: applied.action || undefined,
        targetType: applied.targetType || undefined,
        limit: 50,
      }),
    retry: false,
  });

  if (isAccessDenied(query.error)) {
    return <AccessDeniedCard detail="The audit surface requires an auditor-capable role." />;
  }

  const rows: AuditRow[] = query.data?.data ?? [];
  return (
    <div>
      <div className="mb-2 flex flex-wrap items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="ai-action">
            Action
          </label>
          <input
            id="ai-action"
            className={inputCls}
            value={action}
            placeholder="user.freeze"
            onChange={(e) => setAction(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="ai-target">
            Target type
          </label>
          <input
            id="ai-target"
            className={inputCls}
            value={targetType}
            placeholder="account"
            onChange={(e) => setTargetType(e.target.value)}
          />
        </div>
        <button
          type="button"
          className={btnPrimary}
          onClick={() => setApplied({ action, targetType })}
        >
          Apply
        </button>
      </div>
      {query.isError && <ErrorBox error={query.error} />}
      {query.isSuccess &&
        (rows.length === 0 ? (
          <p className="text-sm text-neutral-500">No audit rows for this filter.</p>
        ) : (
          <div className="max-h-64 overflow-y-auto">
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>ID</th>
                  <th className={thCls}>Admin</th>
                  <th className={thCls}>Action</th>
                  <th className={thCls}>Target</th>
                  <th className={thCls}>IP</th>
                  <th className={thCls}>At</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td className={tdCls}>{r.id}</td>
                    <td className={tdCls}>{r.adminUserId}</td>
                    <td className={tdCls}>{r.action}</td>
                    <td className={tdCls}>
                      {r.targetType !== undefined ? `${r.targetType}:${r.targetId ?? ''}` : '—'}
                    </td>
                    <td className={tdCls}>{r.ipAddress ?? '—'}</td>
                    <td className={tdCls}>{r.createdAt}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}
    </div>
  );
}

function ChainVerify({ adminApi }: { adminApi: BoundAdminApi }) {
  const [date, setDate] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [report, setReport] = useState<Awaited<ReturnType<typeof verifyAuditChain>> | undefined>(
    undefined,
  );

  // Explicit run only — a genesis→date replay is a bounded-but-heavy op.
  const run = async () => {
    setBusy(true);
    setError(null);
    try {
      setReport(await verifyAuditChain(adminApi, date));
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div>
      <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
        Chain verification
      </h3>
      <p className={hintTextCls}>
        Replays the hash chain genesis→end-of-day plus the stored daily Merkle root. A clean report
        is evidence; a violation is a finding, not an error.
      </p>
      <div className="mt-2 flex items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="ai-date">
            Date (UTC)
          </label>
          <input
            id="ai-date"
            type="date"
            className={inputCls}
            value={date}
            onChange={(e) => setDate(e.target.value)}
          />
        </div>
        <button
          type="button"
          className={btnPrimary}
          disabled={busy || date === ''}
          onClick={() => void run()}
        >
          {busy ? 'Verifying…' : 'Verify chain'}
        </button>
      </div>
      {error !== null && <ErrorBox error={error} />}
      {report !== undefined && (
        <div className="mt-2" data-testid="verify-report">
          <div className="flex items-center gap-2">
            <StatusBadge value={report.ok ? 'INTACT' : 'VIOLATION'} />
            <span className="text-sm text-neutral-300">
              {report.rowsChecked} rows checked — merkle{' '}
              {report.merkle.storedRoot
                ? report.merkle.verified
                  ? 'verified'
                  : 'MISMATCH'
                : 'no stored root'}
            </span>
          </div>
          {report.violations.length > 0 && (
            <ul className="mt-1 space-y-0.5 font-mono text-xs text-red-400">
              {report.violations.map((v, i) => (
                <li key={i}>{renderScalar(v)}</li>
              ))}
            </ul>
          )}
          {report.merkle.violation !== undefined && (
            <p className="mt-1 text-xs text-red-400">merkle: {report.merkle.violation}</p>
          )}
        </div>
      )}
    </div>
  );
}

function ChainSlice({ adminApi }: { adminApi: BoundAdminApi }) {
  const [show, setShow] = useState(false);
  const query = useQuery({
    queryKey: ['admin-int', 'audit-chain', adminApi.env],
    queryFn: () => fetchAuditChain(adminApi, { limit: 50 }),
    retry: false,
    enabled: show,
  });
  return (
    <div>
      <button type="button" className={btnGhost} onClick={() => setShow((s) => !s)}>
        {show ? 'Hide raw chain' : 'Show raw hash chain'}
      </button>
      {show && query.isError && <ErrorBox error={query.error} />}
      {show && query.isSuccess && (
        <ul className="mt-2 max-h-48 space-y-1 overflow-y-auto font-mono text-xs text-neutral-400">
          {query.data.length === 0 && <li>No chain rows.</li>}
          {query.data.map((row, i) => (
            <li key={i} className="break-all">
              {renderScalar(row)}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function AuditIntegrityPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  return (
    <section className={cardCls} aria-label="Audit integrity">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Audit chain</h2>
      <div className="grid gap-4 lg:grid-cols-2">
        <AuditExplorer adminApi={adminApi} />
        <div className="space-y-3">
          <ChainVerify adminApi={adminApi} />
          <ChainSlice adminApi={adminApi} />
        </div>
      </div>
    </section>
  );
}
