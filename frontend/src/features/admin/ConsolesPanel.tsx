/**
 * Review consoles (Task 8) — tabbed officer surfaces over live admin
 * routes: KYC queue, webhook dead-letters, LP scorecard, RBAC bindings,
 * compliance holds. All decisions are server-side role-gated; grants,
 * revokes and hold releases are dual-controlled (202 → pending).
 */
import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';

import type { BoundAdminApi } from '@/lib/env';
import {
  ErrorBox,
  StatusBadge,
  btnDanger,
  btnGhost,
  cardCls,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';
import { ConfirmModal } from '@/lib/input-helpers';

import {
  createLP,
  decideKyc,
  escalateHold,
  grantBinding,
  listBindings,
  listDeadLetters,
  listHolds,
  listKycPending,
  listLPs,
  lpScorecard,
  placeHold,
  releaseHold,
  updateLP,
  retransmitDeadLetter,
  revokeBinding,
  type ComplianceHold,
  type KycQueueItem,
} from './consoles-api';
import { isAccessDenied, useAdminRole } from './adminRole';
import { AccessDeniedCard } from './RequireAdmin';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

type Tab = 'kyc' | 'webhooks' | 'lp' | 'rbac' | 'holds';

const TABS: { id: Tab; label: string }[] = [
  { id: 'kyc', label: 'KYC queue' },
  { id: 'webhooks', label: 'Webhook DLQ' },
  { id: 'lp', label: 'LP scorecard' },
  { id: 'rbac', label: 'RBAC bindings' },
  { id: 'holds', label: 'Compliance holds' },
];

export function ConsolesPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [tab, setTab] = useState<Tab>('kyc');
  return (
    <div className={cardCls}>
      <div className="mb-3 flex items-center gap-1 border-b border-neutral-800 pb-2">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            onClick={() => setTab(t.id)}
            className={`rounded px-2 py-1 text-xs ${
              tab === t.id
                ? 'bg-sky-600/20 text-sky-300'
                : 'text-neutral-400 hover:text-neutral-200'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>
      {tab === 'kyc' && <KycQueue adminApi={adminApi} />}
      {tab === 'webhooks' && <WebhookDlq adminApi={adminApi} />}
      {tab === 'lp' && <LPScorecards adminApi={adminApi} />}
      {tab === 'rbac' && <Bindings adminApi={adminApi} />}
      {tab === 'holds' && <Holds adminApi={adminApi} />}
    </div>
  );
}

function PanelError({ err }: { err: unknown }) {
  if (isAccessDenied(err)) return <AccessDeniedCard />;
  return <ErrorBox error={err} />;
}

// ---------------------------------------------------------------------------
// KYC review queue — GET /admin/kyc/pending + approve/reject.
// ---------------------------------------------------------------------------

function KycQueue({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const role = useAdminRole();
  const [err, setErr] = useState<unknown>(null);
  const [rejectTarget, setRejectTarget] = useState<KycQueueItem | null>(null);
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);

  const q = useQuery({
    queryKey: ['admin', 'kyc-pending', adminApi.env],
    queryFn: () => listKycPending(adminApi),
    retry: false,
  });

  const decide = async (id: number, approve: boolean, rejectReason: string) => {
    setBusy(true);
    setErr(null);
    try {
      await decideKyc(adminApi, id, approve, rejectReason);
      setRejectTarget(null);
      setReason('');
      await qc.invalidateQueries({ queryKey: ['admin', 'kyc-pending'] });
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };

  if (q.error) return <PanelError err={q.error} />;
  const rows = q.data ?? [];
  const eligible = role === 'Compliance Officer' || role === 'Super Admin';

  return (
    <section>
      <p className="mb-2 text-xs text-neutral-500">
        PENDING_REVIEW + UNDER_REVIEW submissions, oldest SLA first. Rejection requires a reason.
      </p>
      {err !== null && <ErrorBox error={err} />}
      {rows.length === 0 ? (
        <p className="py-4 text-center text-xs text-neutral-500">Queue empty.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>#</th>
              <th className={thCls}>Account</th>
              <th className={thCls}>Tier</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>Risk</th>
              <th className={thCls}>SLA due</th>
              <th className={thCls}></th>
            </tr>
          </thead>
          <tbody>
            {rows.map((s) => (
              <tr key={s.id}>
                <td className={tdCls}>{s.id}</td>
                <td className={tdCls}>{s.accountId}</td>
                <td className={tdCls}>{s.requestedTier}</td>
                <td className={tdCls}>
                  <StatusBadge value={s.status} />
                </td>
                <td className={tdCls}>{s.riskScore ?? '—'}</td>
                <td className={tdCls}>
                  {s.slaDueAt
                    ? new Date(s.slaDueAt).toLocaleString('en-US', { hour12: false })
                    : '—'}
                </td>
                <td className={tdCls}>
                  {eligible && (
                    <div className="flex gap-1">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => void decide(s.id, true, '')}
                        className={btnGhost}
                      >
                        Approve
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => setRejectTarget(s)}
                        className={btnDanger}
                      >
                        Reject
                      </button>
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {rejectTarget !== null && (
        <ConfirmModal
          open
          severity="MEDIUM"
          title={`Reject submission ${rejectTarget.id}`}
          confirmLabel="Reject"
          busy={busy}
          onCancel={() => {
            setRejectTarget(null);
            setReason('');
          }}
          onConfirm={() => void decide(rejectTarget.id, false, reason)}
        >
          <label className="block text-sm">
            Rejection reason
            <textarea
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              required
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-sm"
            />
          </label>
        </ConfirmModal>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Webhook dead-letter review — GET /admin/webhooks/dead-letters + retransmit.
// ---------------------------------------------------------------------------

function WebhookDlq({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [err, setErr] = useState<unknown>(null);
  const [busyId, setBusyId] = useState<string | null>(null);

  const q = useQuery({
    queryKey: ['admin', 'webhook-dlq', adminApi.env],
    queryFn: () => listDeadLetters(adminApi),
    retry: false,
  });

  const retransmit = async (id: string) => {
    setBusyId(id);
    setErr(null);
    try {
      await retransmitDeadLetter(adminApi, id);
      await qc.invalidateQueries({ queryKey: ['admin', 'webhook-dlq'] });
    } catch (e) {
      setErr(e);
    } finally {
      setBusyId(null);
    }
  };

  if (q.error) return <PanelError err={q.error} />;
  const rows = q.data ?? [];

  return (
    <section>
      <p className="mb-2 text-xs text-neutral-500">
        Deliveries that exhausted retries. Retransmit requeues PENDING with a fresh attempt budget
        (audit-logged).
      </p>
      {err !== null && <ErrorBox error={err} />}
      {rows.length === 0 ? (
        <p className="py-4 text-center text-xs text-neutral-500">Dead-letter queue empty.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Delivery</th>
              <th className={thCls}>Event</th>
              <th className={thCls}>Attempts</th>
              <th className={thCls}>Last HTTP</th>
              <th className={thCls}>Error</th>
              <th className={thCls}></th>
            </tr>
          </thead>
          <tbody>
            {rows.map((d) => (
              <tr key={d.id}>
                <td className={`${tdCls} font-mono`}>{d.id}</td>
                <td className={`${tdCls} font-mono`}>{d.event}</td>
                <td className={tdCls}>{d.attempts}</td>
                <td className={tdCls}>{d.lastStatusCode ?? '—'}</td>
                <td className={`${tdCls} max-w-40 truncate`}>{d.lastError ?? '—'}</td>
                <td className={tdCls}>
                  <button
                    type="button"
                    disabled={busyId === d.id}
                    onClick={() => void retransmit(d.id)}
                    className={btnGhost}
                  >
                    {busyId === d.id ? 'Queuing…' : 'Retransmit'}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// LP scorecard — GET /admin/liquidity-providers + /{id}/scorecard.
// ---------------------------------------------------------------------------

function ScorecardBlock({ adminApi, lpId }: { adminApi: BoundAdminApi; lpId: number }) {
  const sc = useQuery({
    queryKey: ['admin', 'lp-scorecard', adminApi.env, lpId],
    queryFn: () => lpScorecard(adminApi, lpId, '1h'),
    retry: false,
  });
  if (sc.error) return <PanelError err={sc.error} />;
  if (!sc.data) return <p className="p-3 text-xs text-neutral-500">Loading scorecard…</p>;
  return (
    <div className="rounded border border-neutral-800 p-3">
      <dl className="grid grid-cols-3 gap-2 text-xs">
        <Score label="Fill ratio" value={sc.data.fillRatio} pct />
        <Score label="Reject rate" value={sc.data.rejectionRate} pct />
        <Score label="Availability" value={sc.data.availabilityPct} pct />
        <Score label="Quotes" value={sc.data.quotesReceived} />
        <Score label="Fills" value={sc.data.fills} />
        <Score label="Rejections" value={sc.data.rejections} />
        <Score label="Avg response" value={sc.data.avgResponseMs} unit="ms" />
        <Score label="p99" value={sc.data.p99LatencyMs} unit="ms" />
        <Score label="Stale" value={sc.data.stale ? 'yes' : 'live'} />
      </dl>
    </div>
  );
}

const LP_STATUSES = ['ONBOARDING', 'ACTIVE', 'SUSPENDED', 'RETIRED'] as const;

function LPScorecards({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [selected, setSelected] = useState<number | null>(null);
  const [err, setErr] = useState<unknown>(null);
  const [create, setCreate] = useState({ name: '', connectionType: 'FIX' });
  const [edit, setEdit] = useState<{ lpId: number; status: string; reason: string } | null>(null);
  const lps = useQuery({
    queryKey: ['admin', 'lps', adminApi.env],
    queryFn: () => listLPs(adminApi),
    retry: false,
  });
  const invalidate = () => void qc.invalidateQueries({ queryKey: ['admin', 'lps'] });

  if (lps.error) return <PanelError err={lps.error} />;
  const rows = lps.data ?? [];

  return (
    <section>
      <p className="mb-2 text-xs text-neutral-500">
        LP inventory; select one for its 1h scorecard (persisted snapshots mark <code>stale</code> —
        never fabricated).
      </p>
      {err !== null && <ErrorBox error={err} />}
      <form
        className="mb-3 flex flex-wrap items-end gap-2"
        aria-label="Onboard liquidity provider"
        onSubmit={(e) => {
          e.preventDefault();
          setErr(null);
          createLP(adminApi, { name: create.name.trim(), connectionType: create.connectionType })
            .then(() => {
              setCreate({ name: '', connectionType: 'FIX' });
              invalidate();
            })
            .catch(setErr);
        }}
      >
        <label className="text-xs">
          <span className="text-neutral-500">LP name</span>
          <input
            required
            className={inputCls}
            value={create.name}
            onChange={(e) => setCreate({ ...create, name: e.target.value })}
          />
        </label>
        <label className="text-xs">
          <span className="text-neutral-500">connection_type</span>
          <select
            className={selectCls}
            value={create.connectionType}
            onChange={(e) => setCreate({ ...create, connectionType: e.target.value })}
          >
            {['FIX', 'REST', 'WEBSOCKET'].map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
        </label>
        <button type="submit" className={btnGhost} disabled={create.name.trim() === ''}>
          Onboard LP
        </button>
      </form>
      {rows.length === 0 ? (
        <p className="py-4 text-center text-xs text-neutral-500">No liquidity providers.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>LP</th>
              <th className={thCls}>Name</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>Connection</th>
              <th className={thCls}></th>
            </tr>
          </thead>
          <tbody>
            {rows.map((lp) => (
              <tr key={lp.lpId}>
                <td className={tdCls}>{lp.lpId}</td>
                <td className={tdCls}>{lp.name}</td>
                <td className={tdCls}>
                  <StatusBadge value={lp.status} />
                </td>
                <td className={tdCls}>{lp.connectionType}</td>
                <td className={tdCls}>
                  <div className="flex gap-1">
                    <button type="button" onClick={() => setSelected(lp.lpId)} className={btnGhost}>
                      Scorecard
                    </button>
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => setEdit({ lpId: lp.lpId, status: lp.status, reason: '' })}
                    >
                      Edit
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {selected !== null && (
        <div className="mt-3">
          <ScorecardBlock adminApi={adminApi} lpId={selected} />
        </div>
      )}
      {edit !== null && (
        <form
          className="mt-3 flex flex-wrap items-end gap-2 rounded border border-neutral-800 p-2"
          aria-label="Update liquidity provider"
          onSubmit={(e) => {
            e.preventDefault();
            setErr(null);
            updateLP(adminApi, {
              lpId: edit.lpId,
              status: edit.status,
              reason: edit.reason,
            })
              .then(() => {
                setEdit(null);
                invalidate();
              })
              .catch(setErr);
          }}
        >
          <span className="text-xs text-neutral-400">Update LP #{edit.lpId}</span>
          <label className="text-xs">
            <span className="text-neutral-500">status</span>
            <select
              className={selectCls}
              value={edit.status}
              onChange={(e) => setEdit({ ...edit, status: e.target.value })}
            >
              {LP_STATUSES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </label>
          <label className="text-xs">
            <span className="text-neutral-500">reason (audit)</span>
            <input
              className={inputCls}
              value={edit.reason}
              onChange={(e) => setEdit({ ...edit, reason: e.target.value })}
            />
          </label>
          <button type="submit" className={btnGhost}>
            Save
          </button>
          <button
            type="button"
            className={btnGhost}
            onClick={() => {
              setEdit(null);
            }}
          >
            Cancel
          </button>
        </form>
      )}
    </section>
  );
}

function Score({
  label,
  value,
  pct,
  unit,
}: {
  label: string;
  value: unknown;
  pct?: boolean;
  unit?: string;
}) {
  const display =
    typeof value === 'number'
      ? pct
        ? `${(value * 100).toFixed(1)}%`
        : `${value}${unit ?? ''}`
      : typeof value === 'string'
        ? value
        : '—';
  return (
    <div>
      <dt className="text-neutral-500">{label}</dt>
      <dd className="font-mono text-neutral-200">{display}</dd>
    </div>
  );
}

// ---------------------------------------------------------------------------
// RBAC bindings — GET /admin/bindings + dual-control grant/revoke.
// ---------------------------------------------------------------------------

function Bindings({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const role = useAdminRole();
  const [err, setErr] = useState<unknown>(null);
  const [pendingNote, setPendingNote] = useState<string | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<number | null>(null);
  const [reason, setReason] = useState('');
  const [grantUser, setGrantUser] = useState('');
  const [grantRole, setGrantRole] = useState('Read-Only Auditor');
  const [grantHours, setGrantHours] = useState('24');
  const [busy, setBusy] = useState(false);

  const q = useQuery({
    queryKey: ['admin', 'bindings', adminApi.env],
    queryFn: () => listBindings(adminApi),
    retry: false,
  });

  const submit = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setErr(null);
    setPendingNote(null);
    try {
      await fn(); // 202 → queued for four-eyes approval
      setPendingNote('Submitted to the four-eyes queue — pending Super Admin approval.');
      setRevokeTarget(null);
      setReason('');
      await qc.invalidateQueries({ queryKey: ['admin', 'bindings'] });
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };

  if (q.error) return <PanelError err={q.error} />;
  const rows = q.data ?? [];
  const isSuper = role === 'Super Admin';

  return (
    <section>
      <p className="mb-2 text-xs text-neutral-500">
        Scoped admin-role bindings — grants and revokes are dual-controlled; bindings always carry
        an expiry (§8.2b).
      </p>
      {err !== null && <ErrorBox error={err} />}
      {pendingNote !== null && <p className="mb-2 text-xs text-amber-300">{pendingNote}</p>}
      {isSuper && (
        <form
          className="mb-3 flex flex-wrap items-end gap-2 text-xs"
          onSubmit={(e) => {
            e.preventDefault();
            const uid = Number(grantUser);
            if (!Number.isFinite(uid) || uid <= 0) return;
            void submit(() =>
              grantBinding(adminApi, {
                userId: uid,
                role: grantRole,
                expiresInSeconds: Number(grantHours) * 3600,
                reason,
              }),
            );
          }}
        >
          <label>
            User ID
            <input
              value={grantUser}
              onChange={(e) => setGrantUser(e.target.value)}
              required
              inputMode="numeric"
              className="ml-1 w-24 rounded border border-neutral-700 bg-neutral-950 px-2 py-1"
            />
          </label>
          <label>
            Role
            <select
              value={grantRole}
              onChange={(e) => setGrantRole(e.target.value)}
              className="ml-1 rounded border border-neutral-700 bg-neutral-950 px-2 py-1"
            >
              {[
                'Super Admin',
                'Risk Manager',
                'Compliance Officer',
                'Finance Ops',
                'Support Agent',
                'Read-Only Auditor',
              ].map((r) => (
                <option key={r}>{r}</option>
              ))}
            </select>
          </label>
          <label>
            Hours
            <input
              value={grantHours}
              onChange={(e) => setGrantHours(e.target.value)}
              required
              inputMode="numeric"
              className="ml-1 w-16 rounded border border-neutral-700 bg-neutral-950 px-2 py-1"
            />
          </label>
          <button type="submit" disabled={busy} className={btnGhost}>
            Grant (dual control)
          </button>
        </form>
      )}
      {rows.length === 0 ? (
        <p className="py-4 text-center text-xs text-neutral-500">No bindings.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>ID</th>
              <th className={thCls}>User</th>
              <th className={thCls}>Role</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>Expires</th>
              <th className={thCls}></th>
            </tr>
          </thead>
          <tbody>
            {rows.map((b) => (
              <tr key={b.id}>
                <td className={tdCls}>{b.id}</td>
                <td className={tdCls}>{b.userId}</td>
                <td className={tdCls}>{b.role}</td>
                <td className={tdCls}>
                  <StatusBadge value={b.status} />
                </td>
                <td className={tdCls}>
                  {b.expiresAt
                    ? new Date(b.expiresAt).toLocaleString('en-US', { hour12: false })
                    : '—'}
                </td>
                <td className={tdCls}>
                  {isSuper && b.status === 'ACTIVE' && (
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => setRevokeTarget(b.id)}
                      className={btnDanger}
                    >
                      Revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {revokeTarget !== null && (
        <ConfirmModal
          open
          severity="HIGH"
          title={`Revoke binding ${revokeTarget}`}
          confirmLabel="Revoke"
          dualControl
          busy={busy}
          onCancel={() => {
            setRevokeTarget(null);
            setReason('');
          }}
          onConfirm={() => void submit(() => revokeBinding(adminApi, revokeTarget, reason))}
        >
          <label className="block text-sm">
            Revocation reason
            <textarea
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              required
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-sm"
            />
          </label>
        </ConfirmModal>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Compliance holds — GET /admin/compliance/holds + release (dual) + escalate.
// ---------------------------------------------------------------------------

function Holds({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const role = useAdminRole();
  const [err, setErr] = useState<unknown>(null);
  const [target, setTarget] = useState<{
    hold: ComplianceHold;
    op: 'release' | 'sar' | 'closure';
  } | null>(null);
  const [approverId, setApproverId] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [place, setPlace] = useState({ accountId: '', reason: '', evidenceRef: '', slaHours: '' });
  const [placeOpen, setPlaceOpen] = useState(false);

  const q = useQuery({
    queryKey: ['admin', 'holds', adminApi.env],
    queryFn: () => listHolds(adminApi),
    retry: false,
  });

  const run = async () => {
    if (!target) return;
    setBusy(true);
    setErr(null);
    setNote(null);
    try {
      if (target.op === 'release') {
        await releaseHold(adminApi, target.hold.holdId, Number(approverId), reason);
      } else {
        const res = await escalateHold(adminApi, target.hold.holdId, target.op, reason);
        const reqId =
          target.op === 'closure' && isRecord(res) ? res['dual_control_request_id'] : undefined;
        if (target.op === 'closure') {
          setNote(
            `Closure request queued for dual control${
              typeof reqId === 'number' || typeof reqId === 'string' ? ` (${String(reqId)})` : ''
            }`,
          );
        }
      }
      setTarget(null);
      setReason('');
      setApproverId('');
      await qc.invalidateQueries({ queryKey: ['admin', 'holds'] });
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  };

  if (q.error) return <PanelError err={q.error} />;
  const rows = q.data ?? [];
  const isOfficer = role === 'Compliance Officer' || role === 'Super Admin';

  return (
    <section>
      <p className="mb-2 text-xs text-neutral-500">
        Open compliance holds — release needs a distinct approver (four-eyes); escalation files a
        SAR record or queues forced closure.
      </p>
      {err !== null && <ErrorBox error={err} />}
      {note !== null && <p className="mb-2 text-xs text-amber-300">{note}</p>}
      {isOfficer && (
        <div className="mb-3">
          <button
            type="button"
            className={btnGhost}
            onClick={() => {
              setPlaceOpen(!placeOpen);
            }}
          >
            {placeOpen ? 'Hide place-hold form' : 'Place hold…'}
          </button>
          {placeOpen && (
            <form
              className="mt-2 grid gap-2 sm:grid-cols-4"
              onSubmit={(e) => {
                e.preventDefault();
                setBusy(true);
                setErr(null);
                placeHold(adminApi, {
                  accountId: Number(place.accountId),
                  reason: place.reason,
                  ...(place.evidenceRef !== '' ? { evidenceRef: place.evidenceRef } : {}),
                  ...(place.slaHours !== '' ? { slaHours: Number(place.slaHours) } : {}),
                })
                  .then(async () => {
                    setNote('Hold placed — account frozen, resting orders cancelled.');
                    setPlace({ accountId: '', reason: '', evidenceRef: '', slaHours: '' });
                    setPlaceOpen(false);
                    await qc.invalidateQueries({ queryKey: ['admin', 'holds'] });
                  })
                  .catch((e: unknown) => setErr(e))
                  .finally(() => setBusy(false));
              }}
            >
              {(
                [
                  ['accountId', 'account_id'],
                  ['reason', 'reason'],
                  ['evidenceRef', 'evidence_ref (opt)'],
                  ['slaHours', 'sla_hours (opt)'],
                ] as const
              ).map(([k, label]) => (
                <label key={k} className="block text-xs">
                  <span className="text-neutral-500">{label}</span>
                  <input
                    required={k === 'accountId' || k === 'reason'}
                    className={inputCls}
                    value={place[k]}
                    onChange={(e) => setPlace({ ...place, [k]: e.target.value })}
                  />
                </label>
              ))}
              <div className="flex items-end">
                <button
                  type="submit"
                  className={btnDanger}
                  disabled={busy || Number(place.accountId) <= 0 || place.reason.trim() === ''}
                >
                  Place hold
                </button>
              </div>
            </form>
          )}
        </div>
      )}
      {rows.length === 0 ? (
        <p className="py-4 text-center text-xs text-neutral-500">No open holds.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Hold</th>
              <th className={thCls}>Account</th>
              <th className={thCls}>Trigger</th>
              <th className={thCls}>Reason</th>
              <th className={thCls}>SLA</th>
              <th className={thCls}></th>
            </tr>
          </thead>
          <tbody>
            {rows.map((h) => (
              <tr key={h.holdId} className={h.slaBreached ? 'bg-red-950/20' : ''}>
                <td className={`${tdCls} font-mono`}>{h.holdId}</td>
                <td className={tdCls}>{h.accountId}</td>
                <td className={tdCls}>{h.trigger}</td>
                <td className={`${tdCls} max-w-40 truncate`}>{h.reason}</td>
                <td className={tdCls}>
                  {h.slaBreached ? (
                    <span className="text-red-400">BREACHED</span>
                  ) : h.slaDeadline ? (
                    new Date(h.slaDeadline).toLocaleString('en-US', { hour12: false })
                  ) : (
                    '—'
                  )}
                </td>
                <td className={tdCls}>
                  {isOfficer && (
                    <div className="flex gap-1">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => setTarget({ hold: h, op: 'release' })}
                        className={btnGhost}
                      >
                        Release
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => setTarget({ hold: h, op: 'sar' })}
                        className={btnGhost}
                      >
                        SAR
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => setTarget({ hold: h, op: 'closure' })}
                        className={btnDanger}
                      >
                        Closure
                      </button>
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {target !== null && (
        <ConfirmModal
          open
          severity={target.op === 'release' ? 'MEDIUM' : 'HIGH'}
          title={
            target.op === 'release'
              ? `Release hold ${target.hold.holdId}`
              : target.op === 'sar'
                ? `Escalate ${target.hold.holdId} to SAR`
                : `Escalate ${target.hold.holdId} to forced closure`
          }
          confirmLabel={target.op === 'release' ? 'Release' : 'Escalate'}
          dualControl={target.op !== 'sar'}
          busy={busy}
          onCancel={() => {
            setTarget(null);
            setReason('');
            setApproverId('');
          }}
          onConfirm={() => void run()}
        >
          {target.op === 'release' && (
            <label className="block text-sm">
              Approver user ID (distinct admin — four-eyes)
              <input
                value={approverId}
                onChange={(e) => setApproverId(e.target.value)}
                required
                inputMode="numeric"
                className="mt-1 w-full rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-sm"
              />
            </label>
          )}
          <label className="mt-2 block text-sm">
            Reason
            <textarea
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              required
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-sm"
            />
          </label>
        </ConfirmModal>
      )}
    </section>
  );
}
