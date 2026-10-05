/**
 * Margin-parameter + entity-leverage governance panel (Phase-10.5 Task
 * 10.5.3.11 §3) — both mutations are maker-side only: they file
 * dual-control requests (202 PENDING). The margin-param §13.12 gate is
 * evaluated at approval time — no passing linked validation run ⇒
 * MARGIN_MODEL_UNVALIDATED and the request stays PENDING; the UI says
 * exactly that rather than implying the parameter changed.
 */
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { fetchLeveragePolicies, submitLeveragePolicy, submitMarginParamChange } from './api';

export function RiskPolicyPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [mpc, setMpc] = useState({ parameter: '', proposedValue: '', runId: '', reason: '' });
  const [lev, setLev] = useState({
    entityCode: '',
    clientCategory: 'RETAIL',
    instrumentGroup: 'MAJOR',
    maxLeverage: '30',
    effectiveFrom: '',
    reason: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const policies = useQuery({
    queryKey: ['admin-leverage-policies'],
    queryFn: () => fetchLeveragePolicies(adminApi),
  });
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Submit failed');
  const marginChange = useMutation({
    mutationFn: () =>
      submitMarginParamChange(adminApi, {
        parameter: mpc.parameter,
        proposedValue: JSON.parse(mpc.proposedValue) as unknown,
        runId: Number(mpc.runId),
        reason: mpc.reason,
      }),
    onSuccess: (p) => {
      setNotice(
        `Param change queued — request #${p.dualControlId}. The §13.12 gate runs at approval: ` +
          `without a passing linked validation run it fails MARGIN_MODEL_UNVALIDATED and stays PENDING.`,
      );
    },
    onError: onErr,
  });
  const levSubmit = useMutation({
    mutationFn: () =>
      submitLeveragePolicy(adminApi, {
        entityCode: lev.entityCode,
        clientCategory: lev.clientCategory,
        instrumentGroup: lev.instrumentGroup,
        maxLeverage: Number(lev.maxLeverage),
        effectiveFrom: lev.effectiveFrom,
        reason: lev.reason,
      }),
    onSuccess: (p) => {
      setNotice(
        `Leverage cell queued — request #${p.dualControlId} (effective-dated upsert on approval).`,
      );
    },
    onError: onErr,
  });

  if (policies.error !== null && isAccessDenied(policies.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Margin and leverage governance">
      <h2 className="mb-2 text-sm font-semibold">Margin &amp; leverage governance</h2>

      <form
        aria-label="Margin param change"
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (mpc.parameter !== '' && mpc.proposedValue !== '' && Number(mpc.runId) > 0) {
            marginChange.mutate();
          }
        }}
      >
        <input
          aria-label="Parameter name"
          className={inputCls}
          placeholder="parameter (e.g. auction_floor_pct)"
          value={mpc.parameter}
          onChange={(e) => {
            setMpc({ ...mpc, parameter: e.target.value });
          }}
        />
        <input
          aria-label="Proposed value"
          className={inputCls}
          placeholder="proposed_value (JSON)"
          value={mpc.proposedValue}
          onChange={(e) => {
            setMpc({ ...mpc, proposedValue: e.target.value });
          }}
        />
        <input
          aria-label="Validation run id"
          className={inputCls}
          placeholder="run_id (margin_model_runs)"
          value={mpc.runId}
          onChange={(e) => {
            setMpc({ ...mpc, runId: e.target.value });
          }}
        />
        <input
          aria-label="Change reason"
          className={inputCls}
          placeholder="reason"
          value={mpc.reason}
          onChange={(e) => {
            setMpc({ ...mpc, reason: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={marginChange.isPending}>
          File change (4-eyes)
        </button>
      </form>
      <p className={hintTextCls}>
        §13.12: approval-time gate requires a PASSing, independent, in-scope margin_model_runs row —
        otherwise MARGIN_MODEL_UNVALIDATED and the request never leaves PENDING.
      </p>

      <h3 className="mb-1 mt-4 text-sm font-semibold">Entity leverage matrix</h3>
      {policies.error !== null ? <ErrorBox error={policies.error} /> : null}
      {policies.data !== undefined && policies.data.length > 0 ? (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Entity</th>
              <th className={thCls}>Category</th>
              <th className={thCls}>Group</th>
              <th className={thCls}>Max leverage</th>
              <th className={thCls}>Effective from</th>
            </tr>
          </thead>
          <tbody>
            {policies.data.map((p) => (
              <tr
                key={`${p.entityCode}/${p.clientCategory}/${p.instrumentGroup}/${p.effectiveFrom}`}
              >
                <td className={tdCls}>{p.entityCode}</td>
                <td className={tdCls}>{p.clientCategory}</td>
                <td className={tdCls}>{p.instrumentGroup}</td>
                <td className={tdCls}>{p.maxLeverage}x</td>
                <td className={tdCls}>{p.effectiveFrom.slice(0, 10)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      <form
        aria-label="Leverage policy cell"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (lev.entityCode !== '' && lev.effectiveFrom !== '') levSubmit.mutate();
        }}
      >
        <input
          aria-label="Entity code"
          className={inputCls}
          placeholder="entity_code"
          value={lev.entityCode}
          onChange={(e) => {
            setLev({ ...lev, entityCode: e.target.value });
          }}
        />
        <select
          aria-label="Client category"
          className={selectCls}
          value={lev.clientCategory}
          onChange={(e) => {
            setLev({ ...lev, clientCategory: e.target.value });
          }}
        >
          <option value="RETAIL">RETAIL</option>
          <option value="PROFESSIONAL">PROFESSIONAL</option>
          <option value="ELIGIBLE_COUNTERPARTY">ELIGIBLE_COUNTERPARTY</option>
        </select>
        <select
          aria-label="Instrument group"
          className={selectCls}
          value={lev.instrumentGroup}
          onChange={(e) => {
            setLev({ ...lev, instrumentGroup: e.target.value });
          }}
        >
          <option value="MAJOR">MAJOR</option>
          <option value="MINOR">MINOR</option>
          <option value="EXOTIC">EXOTIC</option>
        </select>
        <input
          aria-label="Max leverage"
          className={inputCls}
          placeholder="max_leverage"
          value={lev.maxLeverage}
          onChange={(e) => {
            setLev({ ...lev, maxLeverage: e.target.value });
          }}
        />
        <input
          aria-label="Effective from"
          className={inputCls}
          type="date"
          value={lev.effectiveFrom}
          onChange={(e) => {
            setLev({ ...lev, effectiveFrom: e.target.value });
          }}
        />
        <input
          aria-label="Policy reason"
          className={inputCls}
          placeholder="reason"
          value={lev.reason}
          onChange={(e) => {
            setLev({ ...lev, reason: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={levSubmit.isPending}>
          File cell (4-eyes)
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
