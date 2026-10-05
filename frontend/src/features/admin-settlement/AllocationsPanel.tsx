/**
 * Allocations panel (Phase-10.5 Task 10.5.3.9 §4) — bunched-order
 * group lifecycle: register with an eligibility set, attach fills,
 * run the allocation method, flip eligibility, submit to settlement
 * (SETTLEMENT_LOCKED), leg claim/reject/cancel/correct (correction on
 * a locked group queues dual-control — 202 renders as PENDING), and
 * the T+0 unallocated-remainder escalation sweep.
 */
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  hintTextCls,
  inputCls,
  labelCls,
  selectCls,
  StatusBadge,
} from '@/lib/ui';

import {
  attachFills,
  createAllocationGroup,
  escalateAllocations,
  fetchAllocationGroup,
  flipEligibility,
  legAction,
  runAllocation,
  submitGroup,
  type AllocationGroup,
} from './api';

export function AllocationsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [groupForm, setGroupForm] = useState({
    ref: '',
    managerId: '',
    instrumentId: '',
    side: 'BUY',
    capacity: 'AGENCY',
    method: 'MANUAL',
    eligible: '',
  });
  const [groupId, setGroupId] = useState('');
  const [fills, setFills] = useState('');
  const [legs, setLegs] = useState('');
  const [elig, setElig] = useState({ accountId: '', eligible: true });
  const [legForm, setLegForm] = useState({ allocId: '', reason: '', approverId: '' });
  const [group, setGroup] = useState<AllocationGroup | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const show = (g: AllocationGroup) => {
    setGroup(g);
    setNotice(null);
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const create = useMutation({
    mutationFn: () =>
      createAllocationGroup(adminApi, {
        groupRef: groupForm.ref,
        managerAccountId: Number(groupForm.managerId),
        instrumentId: Number(groupForm.instrumentId),
        side: groupForm.side,
        capacity: groupForm.capacity,
        method: groupForm.method,
        eligibleAccountIds: groupForm.eligible
          .split(',')
          .map((s) => Number(s.trim()))
          .filter((n) => n > 0),
      }),
    onSuccess: (g) => {
      show(g);
      setGroupId(String(g.id));
    },
    onError: onErr,
  });
  const load = useMutation({
    mutationFn: () => fetchAllocationGroup(adminApi, Number(groupId)),
    onSuccess: show,
    onError: (e) => {
      setGroup(null);
      onErr(e);
    },
  });
  const act = useMutation({
    mutationFn: (fn: () => Promise<AllocationGroup>) => fn(),
    onSuccess: show,
    onError: onErr,
  });
  const leg = useMutation({
    mutationFn: (input: {
      verb: 'claim' | 'reject' | 'cancel' | 'correct';
      body: Record<string, unknown>;
    }) => legAction(adminApi, Number(legForm.allocId), input.verb, input.body),
    onSuccess: (_r, v) => setNotice(`Allocation ${v.verb} submitted.`),
    onError: onErr,
  });
  const escalate = useMutation({
    mutationFn: () => escalateAllocations(adminApi),
    onSuccess: () => setNotice('T+0 escalation sweep complete — check compliance alerts.'),
    onError: onErr,
  });

  const gid = Number(groupId);
  const busy = create.isPending || act.isPending || leg.isPending;

  return (
    <section className={cardCls} aria-label="Allocations">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Allocation groups</h2>
        <button
          type="button"
          className={btnDanger}
          disabled={escalate.isPending}
          onClick={() => {
            escalate.mutate();
          }}
        >
          T+0 escalation sweep
        </button>
      </div>

      <form
        aria-label="Register group"
        className="grid grid-cols-2 gap-2 md:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (
            groupForm.ref !== '' &&
            Number(groupForm.managerId) > 0 &&
            Number(groupForm.instrumentId) > 0
          ) {
            create.mutate();
          }
        }}
      >
        <input
          aria-label="Group ref"
          className={inputCls}
          placeholder="group_ref"
          value={groupForm.ref}
          onChange={(e) => {
            setGroupForm({ ...groupForm, ref: e.target.value });
          }}
        />
        <input
          aria-label="Manager account"
          className={inputCls}
          placeholder="manager_account_id"
          value={groupForm.managerId}
          onChange={(e) => {
            setGroupForm({ ...groupForm, managerId: e.target.value });
          }}
        />
        <input
          aria-label="Instrument id"
          className={inputCls}
          placeholder="instrument_id"
          value={groupForm.instrumentId}
          onChange={(e) => {
            setGroupForm({ ...groupForm, instrumentId: e.target.value });
          }}
        />
        <select
          aria-label="Group side"
          className={selectCls}
          value={groupForm.side}
          onChange={(e) => {
            setGroupForm({ ...groupForm, side: e.target.value });
          }}
        >
          <option value="BUY">BUY</option>
          <option value="SELL">SELL</option>
        </select>
        <select
          aria-label="Capacity"
          className={selectCls}
          value={groupForm.capacity}
          onChange={(e) => {
            setGroupForm({ ...groupForm, capacity: e.target.value });
          }}
        >
          <option value="AGENCY">AGENCY</option>
          <option value="PRINCIPAL">PRINCIPAL</option>
          <option value="MIXED">MIXED</option>
        </select>
        <select
          aria-label="Allocation method"
          className={selectCls}
          value={groupForm.method}
          onChange={(e) => {
            setGroupForm({ ...groupForm, method: e.target.value });
          }}
        >
          <option value="MANUAL">MANUAL</option>
          <option value="PRO_RATA">PRO_RATA</option>
          <option value="RULE_BASED">RULE_BASED</option>
          <option value="EQUAL_SPLIT">EQUAL_SPLIT</option>
        </select>
        <input
          aria-label="Eligible accounts"
          className={`${inputCls} md:col-span-2`}
          placeholder="eligible account_ids (csv)"
          value={groupForm.eligible}
          onChange={(e) => {
            setGroupForm({ ...groupForm, eligible: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={busy}>
          Register group
        </button>
      </form>

      <div className="mt-3 flex items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="grp-id">
            Group id
          </label>
          <input
            id="grp-id"
            className={inputCls}
            value={groupId}
            onChange={(e) => {
              setGroupId(e.target.value);
            }}
          />
        </div>
        <button
          type="button"
          className={btnGhost}
          disabled={load.isPending || gid <= 0}
          onClick={() => {
            load.mutate();
          }}
        >
          Load evidence
        </button>
      </div>

      {group !== null ? (
        <div className="mt-2 rounded border border-neutral-700 p-2 text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <strong>
              #{group.id} {group.groupRef}
            </strong>
            <StatusBadge value={group.status || 'UNKNOWN'} />
            {group.settlementLocked ? <StatusBadge value="SETTLEMENT_LOCKED" /> : null}
            {group.escalatedAt !== undefined ? <StatusBadge value="ESCALATED" /> : null}
            <span className={hintTextCls}>
              {group.allocatedQty}/{group.totalQty} allocated · avg {group.avgPrice} ·{' '}
              {group.method}
            </span>
          </div>

          <div className="mt-2 flex flex-wrap items-end gap-2">
            <input
              aria-label="Fill trade ids"
              className={inputCls}
              placeholder="trade_ids (csv)"
              value={fills}
              onChange={(e) => {
                setFills(e.target.value);
              }}
            />
            <button
              type="button"
              className={btnGhost}
              disabled={busy || gid <= 0 || fills === ''}
              onClick={() => {
                act.mutate(() =>
                  attachFills(
                    adminApi,
                    gid,
                    fills
                      .split(',')
                      .map((s) => Number(s.trim()))
                      .filter((n) => n > 0),
                  ),
                );
              }}
            >
              Attach fills
            </button>
            <input
              aria-label="Allocation legs"
              className={inputCls}
              placeholder="acct:qty,acct:qty"
              value={legs}
              onChange={(e) => {
                setLegs(e.target.value);
              }}
            />
            <button
              type="button"
              className={btnPrimary}
              disabled={busy || gid <= 0 || legs === ''}
              onClick={() => {
                act.mutate(() =>
                  runAllocation(
                    adminApi,
                    gid,
                    legs.split(',').flatMap((p) => {
                      const [a, qty] = p.split(':').map((s) => s.trim());
                      const accountId = Number(a);
                      return accountId > 0 ? [{ accountId, quantity: qty }] : [];
                    }),
                  ),
                );
              }}
            >
              Allocate
            </button>
            <button
              type="button"
              className={btnPrimary}
              disabled={busy || gid <= 0 || group.settlementLocked}
              onClick={() => {
                act.mutate(() => submitGroup(adminApi, gid));
              }}
            >
              Submit (lock)
            </button>
          </div>
          <div className="mt-2 flex items-center gap-2">
            <input
              aria-label="Eligibility account"
              className={inputCls}
              placeholder="account_id"
              value={elig.accountId}
              onChange={(e) => {
                setElig({ ...elig, accountId: e.target.value });
              }}
            />
            <label className="flex items-center gap-1 text-sm">
              <input
                type="checkbox"
                checked={elig.eligible}
                onChange={(e) => {
                  setElig({ ...elig, eligible: e.target.checked });
                }}
              />
              eligible
            </label>
            <button
              type="button"
              className={btnGhost}
              disabled={busy || gid <= 0 || Number(elig.accountId) <= 0}
              onClick={() => {
                void flipEligibility(adminApi, gid, {
                  accountId: Number(elig.accountId),
                  eligible: elig.eligible,
                })
                  .then(() => act.mutate(() => fetchAllocationGroup(adminApi, gid)))
                  .catch(onErr);
              }}
            >
              Set eligibility
            </button>
          </div>
        </div>
      ) : null}

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        Allocation legs (fund-ops)
      </h3>
      <div className="flex flex-wrap items-end gap-2">
        <input
          aria-label="Allocation id"
          className={inputCls}
          placeholder="allocation_id"
          value={legForm.allocId}
          onChange={(e) => {
            setLegForm({ ...legForm, allocId: e.target.value });
          }}
        />
        <input
          aria-label="Leg reason"
          className={inputCls}
          placeholder="reason"
          value={legForm.reason}
          onChange={(e) => {
            setLegForm({ ...legForm, reason: e.target.value });
          }}
        />
        <input
          aria-label="Correction approver"
          className={inputCls}
          placeholder="approver_id (locked correct)"
          value={legForm.approverId}
          onChange={(e) => {
            setLegForm({ ...legForm, approverId: e.target.value });
          }}
        />
        {(['claim', 'reject', 'cancel', 'correct'] as const).map((verbName) => (
          <button
            key={verbName}
            type="button"
            className={verbName === 'reject' || verbName === 'cancel' ? btnDanger : btnGhost}
            disabled={leg.isPending || Number(legForm.allocId) <= 0}
            onClick={() => {
              const body: Record<string, unknown> = { reason: legForm.reason };
              if (verbName === 'correct' && Number(legForm.approverId) > 0) {
                body['approver_id'] = Number(legForm.approverId);
              }
              leg.mutate({ verb: verbName, body });
            }}
          >
            {verbName}
          </button>
        ))}
      </div>
      <p className={hintTextCls}>
        Corrections on SETTLEMENT_LOCKED groups carry a distinct approver_id inline or queue through
        dual-control — a 202 response means pending, not applied.
      </p>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
