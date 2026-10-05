/**
 * Lifecycle actions panel (Phase-10.5 Task 10.5.3.4 §2) —
 * freeze/unfreeze (inline four-eyes approver), forced close
 * (dual-control queue — 202 means PENDING, never executed),
 * jurisdiction pin, sub-account limit and MiFID client_category.
 * Each action renders the precondition evidence the officer is meant
 * to check (open orders / locked balances from the dossier query —
 * same cache key, no duplicate request).
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import { fetchSupportAccount } from '@/lib/admin/api';
import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  ConfirmAction,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  selectCls,
} from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import {
  closeAccount,
  freezeAccount,
  pinJurisdiction,
  setClientCategory,
  setSubAccountLimit,
} from './api';

const CATEGORIES = ['RETAIL', 'PROFESSIONAL', 'ELIGIBLE_COUNTERPARTY'] as const;

type PendingConfirm = 'freeze' | 'unfreeze' | 'close' | null;

export function LifecyclePanel({
  adminApi,
  accountId,
}: {
  adminApi: BoundAdminApi;
  accountId: number;
}) {
  const qc = useQueryClient();
  // Same queryKey as DossierPanel — one request, shared evidence.
  const dossier = useQuery({
    queryKey: ['admin-crm', 'dossier', adminApi.env, accountId],
    queryFn: () => fetchSupportAccount(adminApi, accountId),
    retry: false,
  });

  const [reason, setReason] = useState('');
  const [approver, setApprover] = useState('');
  const [jurisdiction, setJurisdiction] = useState('');
  const [justification, setJustification] = useState('');
  const [maxSubs, setMaxSubs] = useState('');
  const [category, setCategory] = useState<string>(CATEGORIES[0]);
  const [evidence, setEvidence] = useState('');
  const [pending, setPending] = useState<PendingConfirm>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);

  if (isAccessDenied(dossier.error)) {
    return <AccessDeniedCard detail="Lifecycle actions require an admin role." />;
  }

  const openOrders = dossier.data?.recentOrders.filter(
    (o) => !['FILLED', 'CANCELLED', 'REJECTED', 'EXPIRED'].includes(o.status.toUpperCase()),
  ).length;
  const locked = dossier.data?.balances.filter((b) => b.locked !== '0' && b.locked !== '0.00');

  const run = async (fn: () => Promise<string>) => {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      setNotice(await fn());
      await qc.invalidateQueries({ queryKey: ['admin-crm'] });
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
      setPending(null);
    }
  };

  const approverId = Number(approver);
  const reasonOk = reason.trim() !== '';
  const approverOk = Number.isInteger(approverId) && approverId > 0;

  return (
    <section className={cardCls} aria-label="Lifecycle actions">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Lifecycle actions</h2>
      <p className={hintTextCls}>
        Freeze/unfreeze/close carry Compliance-Officer dual control; close lands in the four-eyes
        queue — a 202 means PENDING approval, not a closed account.
      </p>

      {dossier.isSuccess && (
        <p className="mb-2 text-xs text-neutral-500" data-testid="preconditions">
          Precondition evidence — {openOrders ?? 0} open order(s), {locked?.length ?? 0}{' '}
          currency(ies) with locked balance. Server enforces blockers; this is review context, not a
          guarantee.
        </p>
      )}

      {error !== null && <ErrorBox error={error} />}
      {notice !== null && (
        <p className="mb-2 text-sm text-emerald-300" role="status">
          {notice}
        </p>
      )}

      <div className="mb-3 grid gap-2 sm:grid-cols-2">
        <div>
          <label className={labelCls} htmlFor="lc-reason">
            Reason (freeze / unfreeze / close)
          </label>
          <input
            id="lc-reason"
            className={inputCls}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="lc-approver">
            Approver admin id (freeze/unfreeze, distinct)
          </label>
          <input
            id="lc-approver"
            className={inputCls}
            inputMode="numeric"
            value={approver}
            onChange={(e) => setApprover(e.target.value)}
          />
        </div>
      </div>
      <div className="mb-4 flex flex-wrap gap-2">
        <button
          type="button"
          className={btnDanger}
          disabled={!reasonOk || !approverOk || busy}
          onClick={() => setPending('freeze')}
        >
          Freeze account
        </button>
        <button
          type="button"
          className={btnGhost}
          disabled={!reasonOk || !approverOk || busy}
          onClick={() => setPending('unfreeze')}
        >
          Unfreeze
        </button>
        <button
          type="button"
          className={btnDanger}
          disabled={!reasonOk || busy}
          onClick={() => setPending('close')}
        >
          Forced close (dual-control)
        </button>
      </div>

      <div className="mb-4 border-t border-neutral-800 pt-3">
        <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
          Jurisdiction pin
        </h3>
        <div className="grid gap-2 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="lc-jur">
              Jurisdiction code
            </label>
            <input
              id="lc-jur"
              className={inputCls}
              placeholder="EU"
              value={jurisdiction}
              onChange={(e) => setJurisdiction(e.target.value)}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="lc-just">
              Justification (cross-border gate)
            </label>
            <input
              id="lc-just"
              className={inputCls}
              value={justification}
              onChange={(e) => setJustification(e.target.value)}
            />
          </div>
        </div>
        <button
          type="button"
          className={btnPrimary}
          disabled={jurisdiction.trim() === '' || busy}
          onClick={() =>
            void run(async () => {
              await pinJurisdiction(adminApi, accountId, jurisdiction.trim(), justification);
              return `Jurisdiction pinned to ${jurisdiction.trim().toUpperCase()}`;
            })
          }
        >
          Pin jurisdiction
        </button>
      </div>

      <div className="mb-4 border-t border-neutral-800 pt-3">
        <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
          Sub-account limit
        </h3>
        <div className="flex items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="lc-subs">
              Max sub-accounts
            </label>
            <input
              id="lc-subs"
              className={inputCls}
              inputMode="numeric"
              value={maxSubs}
              onChange={(e) => setMaxSubs(e.target.value)}
            />
          </div>
          <button
            type="button"
            className={btnPrimary}
            disabled={maxSubs.trim() === '' || busy}
            onClick={() =>
              void run(async () => {
                await setSubAccountLimit(apiClient, adminApi.env, accountId, Number(maxSubs));
                return `Sub-account ceiling set to ${maxSubs}`;
              })
            }
          >
            Apply limit
          </button>
        </div>
      </div>

      <div className="border-t border-neutral-800 pt-3">
        <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
          Client category (MiFID II)
        </h3>
        <div className="grid gap-2 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="lc-cat">
              Category
            </label>
            <select
              id="lc-cat"
              className={selectCls}
              value={category}
              onChange={(e) => setCategory(e.target.value)}
            >
              {CATEGORIES.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="lc-ev">
              Evidence (required on upgrade)
            </label>
            <input
              id="lc-ev"
              className={inputCls}
              value={evidence}
              onChange={(e) => setEvidence(e.target.value)}
            />
          </div>
        </div>
        <button
          type="button"
          className={btnPrimary}
          disabled={busy}
          onClick={() =>
            void run(async () => {
              await setClientCategory(apiClient, adminApi.env, accountId, category, evidence);
              return `Client category set to ${category}`;
            })
          }
        >
          Assign category
        </button>
      </div>

      {pending !== null && (
        <ConfirmAction
          message={
            pending === 'close'
              ? `Submit forced closure of account #${accountId} to the four-eyes queue? A Compliance Officer approval executes the full offboarding pipeline.`
              : `${pending === 'freeze' ? 'Freeze' : 'Unfreeze'} account #${accountId} with approver #${approverId}? Reason: "${reason.trim()}"`
          }
          confirmLabel={pending === 'close' ? 'Submit closure request' : `Confirm ${pending}`}
          busy={busy}
          danger={pending !== 'unfreeze'}
          onConfirm={() =>
            void run(async () => {
              if (pending === 'close') {
                const res = await closeAccount(adminApi, accountId, reason.trim());
                return res.requestId !== undefined
                  ? `Closure request #${res.requestId} pending approval — the account is NOT closed yet.`
                  : res.message;
              }
              await freezeAccount(
                adminApi,
                accountId,
                pending === 'freeze',
                reason.trim(),
                approverId,
              );
              return `Account ${pending === 'freeze' ? 'frozen' : 'unfrozen'} (four-eyes recorded).`;
            })
          }
          onCancel={() => setPending(null)}
        />
      )}
    </section>
  );
}
