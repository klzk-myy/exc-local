/**
 * Treasury panel (Phase-10.5 Task 10.5.3.8 §4) — house-capital
 * surfaces: the own-funds ledger with the live treasury freeze flags,
 * the contingent-capital waterfall register + commitment recorder,
 * the insurance-fund balances/transaction history, and the collateral
 * schedule editor (PUT — Risk Manager or stronger, echoed-back truth).
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
  labelCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  fetchCommitments,
  fetchInsuranceFund,
  fetchOwnFunds,
  putCollateralSchedule,
  recordCommitment,
} from './api';

export function TreasuryPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [commitForm, setCommitForm] = useState({
    provider: '',
    kind: 'HOUSE_CAPITAL',
    seq: '10',
    amount: '',
    currency: 'USD',
    trigger: '',
    window: '30',
    ref: '',
  });
  const [collateralForm, setCollateralForm] = useState({
    currency: 'USD',
    haircut: '0',
    maxConc: '100',
    eligible: true,
  });
  const [notice, setNotice] = useState<string | null>(null);

  const ownFunds = useQuery({
    queryKey: ['admin-own-funds'],
    queryFn: () => fetchOwnFunds(adminApi),
  });
  const commitments = useQuery({
    queryKey: ['admin-contingent-capital'],
    queryFn: () => fetchCommitments(adminApi),
  });
  const insurance = useQuery({
    queryKey: ['admin-insurance-fund'],
    queryFn: () => fetchInsuranceFund(adminApi),
  });

  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');
  const record = useMutation({
    mutationFn: () =>
      recordCommitment(adminApi, {
        providerName: commitForm.provider,
        kind: commitForm.kind,
        prioritySeq: Number(commitForm.seq),
        committedAmount: commitForm.amount,
        currency: commitForm.currency,
        activationTrigger: commitForm.trigger,
        drawWindowDays: Number(commitForm.window),
        agreementRef: commitForm.ref,
      }),
    onSuccess: () => {
      setNotice('Contingent-capital commitment recorded.');
      void qc.invalidateQueries({ queryKey: ['admin-contingent-capital'] });
    },
    onError: onErr,
  });
  const collateral = useMutation({
    mutationFn: () =>
      putCollateralSchedule(apiClient, adminApi.env, [
        {
          currency: collateralForm.currency,
          eligible: collateralForm.eligible,
          haircutPct: collateralForm.haircut,
          maxConcentrationPct: collateralForm.maxConc,
        },
      ]),
    onSuccess: () => setNotice('Collateral schedule updated (committed image echoed).'),
    onError: onErr,
  });

  if (ownFunds.error !== null && isAccessDenied(ownFunds.error)) {
    return <AccessDeniedCard />;
  }

  const ctrl = ownFunds.data?.controls ?? null;

  return (
    <section className={cardCls} aria-label="Treasury">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Treasury — own funds &amp; contingent capital</h2>
        {ctrl !== null ? (
          <div aria-label="Treasury controls" className="flex items-center gap-2 text-xs">
            <StatusBadge value={ctrl.discretionaryOutflowsFrozen ? 'OUTFLOWS FROZEN' : 'OPEN'} />
            <StatusBadge value={ctrl.lpCapacityBlocked ? 'LP BLOCKED' : 'LP OK'} />
            {ctrl.freezeReason !== undefined && ctrl.freezeReason !== '' ? (
              <span className={hintTextCls}>{ctrl.freezeReason}</span>
            ) : null}
          </div>
        ) : null}
      </div>

      {ownFunds.error !== null ? <ErrorBox error={ownFunds.error} /> : null}
      {ownFunds.data !== undefined && ownFunds.data.rows.length > 0 ? (
        <table className={tableCls} aria-label="Own funds">
          <thead>
            <tr>
              <th className={thCls}>Line</th>
              <th className={thCls}>CCY</th>
              <th className={thCls}>Balance</th>
              <th className={thCls}>Statement bal</th>
              <th className={thCls}>Recon</th>
            </tr>
          </thead>
          <tbody>
            {ownFunds.data.rows.map((f) => (
              <tr key={f.id}>
                <td className={tdCls}>{f.lineKind}</td>
                <td className={tdCls}>{f.currency}</td>
                <td className={tdCls}>{f.balance}</td>
                <td className={tdCls}>{f.statementBalance ?? '—'}</td>
                <td className={tdCls}>
                  <StatusBadge value={f.reconciliationStatus || 'UNKNOWN'} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="text-sm text-neutral-500">No own-funds lines.</p>
      )}

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        Contingent-capital waterfall
      </h3>
      {commitments.data !== undefined && commitments.data.length > 0 ? (
        <div className="max-h-40 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Seq</th>
                <th className={thCls}>Provider</th>
                <th className={thCls}>Kind</th>
                <th className={thCls}>Committed</th>
                <th className={thCls}>Drawn</th>
                <th className={thCls}>CCY</th>
                <th className={thCls}>Status</th>
              </tr>
            </thead>
            <tbody>
              {[...commitments.data]
                .sort((a, b) => a.prioritySeq - b.prioritySeq)
                .map((c) => (
                  <tr key={c.id}>
                    <td className={tdCls}>{c.prioritySeq}</td>
                    <td className={tdCls}>{c.providerName}</td>
                    <td className={tdCls}>{c.kind}</td>
                    <td className={tdCls}>{c.committedAmount}</td>
                    <td className={tdCls}>{c.drawnAmount}</td>
                    <td className={tdCls}>{c.currency}</td>
                    <td className={tdCls}>
                      <StatusBadge value={c.status || 'UNKNOWN'} />
                    </td>
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="text-sm text-neutral-500">No commitments registered.</p>
      )}
      <form
        aria-label="Record commitment"
        className="mt-2 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (
            commitForm.provider !== '' &&
            commitForm.amount !== '' &&
            commitForm.trigger !== '' &&
            commitForm.ref !== ''
          ) {
            record.mutate();
          }
        }}
      >
        <input
          aria-label="Provider name"
          className={inputCls}
          placeholder="provider"
          value={commitForm.provider}
          onChange={(e) => {
            setCommitForm({ ...commitForm, provider: e.target.value });
          }}
        />
        <select
          aria-label="Commitment kind"
          className={selectCls}
          value={commitForm.kind}
          onChange={(e) => {
            setCommitForm({ ...commitForm, kind: e.target.value });
          }}
        >
          <option value="HOUSE_CAPITAL">HOUSE_CAPITAL</option>
          <option value="SPONSOR">SPONSOR</option>
          <option value="CREDIT_FACILITY">CREDIT_FACILITY</option>
          <option value="INSURER">INSURER</option>
          <option value="INSURANCE_POLICY">INSURANCE_POLICY</option>
        </select>
        <input
          aria-label="Priority sequence"
          className={inputCls}
          placeholder="seq"
          value={commitForm.seq}
          onChange={(e) => {
            setCommitForm({ ...commitForm, seq: e.target.value });
          }}
        />
        <input
          aria-label="Committed amount"
          className={inputCls}
          placeholder="amount"
          value={commitForm.amount}
          onChange={(e) => {
            setCommitForm({ ...commitForm, amount: e.target.value });
          }}
        />
        <input
          aria-label="Commitment currency"
          className={inputCls}
          placeholder="ccy"
          value={commitForm.currency}
          onChange={(e) => {
            setCommitForm({ ...commitForm, currency: e.target.value });
          }}
        />
        <input
          aria-label="Activation trigger"
          className={inputCls}
          placeholder="activation_trigger"
          value={commitForm.trigger}
          onChange={(e) => {
            setCommitForm({ ...commitForm, trigger: e.target.value });
          }}
        />
        <input
          aria-label="Agreement ref"
          className={inputCls}
          placeholder="agreement_ref"
          value={commitForm.ref}
          onChange={(e) => {
            setCommitForm({ ...commitForm, ref: e.target.value });
          }}
        />
        <button type="submit" className={btnGhost} disabled={record.isPending}>
          Record
        </button>
      </form>

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        Insurance fund
      </h3>
      {insurance.error !== null ? <ErrorBox error={insurance.error} /> : null}
      {insurance.data !== undefined ? (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          <table className={tableCls} aria-label="Fund balances">
            <thead>
              <tr>
                <th className={thCls}>CCY</th>
                <th className={thCls}>Balance</th>
                <th className={thCls}>Depletion alert</th>
              </tr>
            </thead>
            <tbody>
              {insurance.data.balances.map((b) => (
                <tr key={b.currency}>
                  <td className={tdCls}>{b.currency}</td>
                  <td className={tdCls}>{b.balance}</td>
                  <td className={tdCls}>{b.depletionThreshold ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="max-h-36 overflow-y-auto">
            <table className={tableCls} aria-label="Fund transactions">
              <thead>
                <tr>
                  <th className={thCls}>Dir</th>
                  <th className={thCls}>Amount</th>
                  <th className={thCls}>Reason</th>
                  <th className={thCls}>After</th>
                </tr>
              </thead>
              <tbody>
                {insurance.data.transactions.map((t) => (
                  <tr key={t.id}>
                    <td className={tdCls}>{t.direction}</td>
                    <td className={tdCls}>
                      {t.amount} {t.currency}
                    </td>
                    <td className={tdCls}>{t.reason}</td>
                    <td className={tdCls}>{t.balanceAfter}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      ) : null}

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        Collateral schedule (Risk Manager)
      </h3>
      <form
        aria-label="Update collateral row"
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (collateralForm.currency !== '') collateral.mutate();
        }}
      >
        <div>
          <label className={labelCls} htmlFor="col-ccy">
            Currency
          </label>
          <input
            id="col-ccy"
            className={inputCls}
            value={collateralForm.currency}
            onChange={(e) => {
              setCollateralForm({ ...collateralForm, currency: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="col-hair">
            Haircut %
          </label>
          <input
            id="col-hair"
            className={inputCls}
            value={collateralForm.haircut}
            onChange={(e) => {
              setCollateralForm({ ...collateralForm, haircut: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="col-conc">
            Max concentration %
          </label>
          <input
            id="col-conc"
            className={inputCls}
            value={collateralForm.maxConc}
            onChange={(e) => {
              setCollateralForm({ ...collateralForm, maxConc: e.target.value });
            }}
          />
        </div>
        <label className="flex items-center gap-1 text-sm">
          <input
            type="checkbox"
            checked={collateralForm.eligible}
            onChange={(e) => {
              setCollateralForm({ ...collateralForm, eligible: e.target.checked });
            }}
          />
          Eligible
        </label>
        <button type="submit" className={btnPrimary} disabled={collateral.isPending}>
          Upsert row (PUT)
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
