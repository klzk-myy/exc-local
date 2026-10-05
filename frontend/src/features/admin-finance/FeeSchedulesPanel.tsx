/**
 * Funding-fee schedules panel (Phase-10.5 Task 10.5.3.10 §1) — the
 * versioned fee-schedule admin: list (active-only default, ?all=1 for
 * retired), create version 1, PUT a successor version (supersedes),
 * DELETE retire (stays on the audit chain), and the per-row version
 * chain drill-down.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  JsonRows,
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
  createFundingFee,
  fetchFeeVersions,
  fetchFundingFee,
  fetchFundingFees,
  retireFundingFee,
  updateFundingFee,
  type FundingFeeTier,
} from './api';

const FEE_FIELDS = [
  ['flatFee', 'Flat fee'],
  ['percentageBps', 'Bps'],
  ['minFee', 'Min fee'],
  ['maxFee', 'Max fee (blank=uncapped)'],
] as const;

export function FeeSchedulesPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [railFilter, setRailFilter] = useState('');
  const [includeAll, setIncludeAll] = useState(false);
  const [form, setForm] = useState({
    rail: 'SWIFT',
    currency: 'USD',
    direction: 'WITHDRAWAL',
    tier: '*',
    flatFee: '0',
    percentageBps: '0',
    minFee: '0',
    maxFee: '',
  });
  const [succeeding, setSucceeding] = useState<FundingFeeTier | null>(null);
  const [succ, setSucc] = useState({ flatFee: '', percentageBps: '', minFee: '', maxFee: '' });
  const [versions, setVersions] = useState<{ forId: number; rows: FundingFeeTier[] } | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [openId, setOpenId] = useState<number | null>(null);

  const list = useQuery({
    queryKey: ['admin-funding-fees', railFilter, includeAll],
    queryFn: () =>
      fetchFundingFees(adminApi, {
        rail: railFilter === '' ? undefined : railFilter,
        all: includeAll,
      }),
  });
  const detail = useQuery({
    queryKey: ['admin-funding-fees', 'detail', openId],
    queryFn: () => fetchFundingFee(adminApi, openId ?? 0),
    enabled: openId !== null,
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-funding-fees'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const create = useMutation({
    mutationFn: () =>
      createFundingFee(adminApi, {
        rail: form.rail,
        currency: form.currency,
        direction: form.direction,
        accountTier: form.tier,
        flatFee: form.flatFee,
        percentageBps: form.percentageBps,
        minFee: form.minFee,
        maxFee: form.maxFee === '' ? undefined : form.maxFee,
      }),
    onSuccess: (t) => {
      setNotice(`Schedule v${t.version} created (#${t.id}).`);
      invalidate();
    },
    onError: onErr,
  });
  const succeed = useMutation({
    mutationFn: () =>
      updateFundingFee(apiClient, adminApi.env, succeeding?.id ?? 0, {
        flatFee: succ.flatFee,
        percentageBps: succ.percentageBps,
        minFee: succ.minFee,
        maxFee: succ.maxFee === '' ? undefined : succ.maxFee,
      }),
    onSuccess: (t) => {
      setNotice(`Successor v${t.version} inserted (supersedes #${succeeding?.id ?? ''}).`);
      setSucceeding(null);
      invalidate();
    },
    onError: onErr,
  });
  const retire = useMutation({
    mutationFn: (id: number) => retireFundingFee(apiClient, adminApi.env, id),
    onSuccess: () => {
      setNotice('Schedule row retired — audit record retained.');
      invalidate();
    },
    onError: onErr,
  });
  const showVersions = useMutation({
    mutationFn: (id: number) => fetchFeeVersions(adminApi, id),
    onSuccess: (rows, id) => setVersions({ forId: id, rows }),
    onError: onErr,
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Funding fee schedules">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Funding fee schedules</h2>
        <div className="flex items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="ff-rail">
              Rail
            </label>
            <input
              id="ff-rail"
              className={inputCls}
              placeholder="all"
              value={railFilter}
              onChange={(e) => {
                setRailFilter(e.target.value);
              }}
            />
          </div>
          <label className="flex items-center gap-1 text-sm">
            <input
              type="checkbox"
              checked={includeAll}
              onChange={(e) => {
                setIncludeAll(e.target.checked);
              }}
            />
            include retired
          </label>
        </div>
      </div>

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? <p className="text-sm text-neutral-500">No schedules.</p> : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="max-h-56 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID/v</th>
                <th className={thCls}>Rail</th>
                <th className={thCls}>CCY</th>
                <th className={thCls}>Dir</th>
                <th className={thCls}>Tier</th>
                <th className={thCls}>Flat</th>
                <th className={thCls}>Bps</th>
                <th className={thCls}>Min→Max</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((t) => [
                <tr key={t.id}>
                  <td className={tdCls}>
                    <button
                      type="button"
                      className={btnGhost}
                      aria-expanded={openId === t.id}
                      onClick={() => {
                        setOpenId(openId === t.id ? null : t.id);
                      }}
                    >
                      #{t.id}/v{t.version}
                    </button>
                  </td>
                  <td className={tdCls}>{t.rail}</td>
                  <td className={tdCls}>{t.currency}</td>
                  <td className={tdCls}>{t.direction}</td>
                  <td className={tdCls}>{t.accountTier}</td>
                  <td className={tdCls}>{t.flatFee}</td>
                  <td className={tdCls}>{t.percentageBps}</td>
                  <td className={tdCls}>
                    {t.minFee}→{t.maxFee ?? '∞'}
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={t.retiredAt !== undefined ? 'RETIRED' : 'ACTIVE'} />
                  </td>
                  <td className={tdCls}>
                    <div className="flex gap-1">
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          showVersions.mutate(t.id);
                        }}
                      >
                        Versions
                      </button>
                      {t.retiredAt === undefined ? (
                        <>
                          <button
                            type="button"
                            className={btnGhost}
                            onClick={() => {
                              setSucceeding(t);
                              setSucc({
                                flatFee: t.flatFee,
                                percentageBps: t.percentageBps,
                                minFee: t.minFee,
                                maxFee: t.maxFee ?? '',
                              });
                            }}
                          >
                            Succeed…
                          </button>
                          <button
                            type="button"
                            className={btnDanger}
                            disabled={retire.isPending}
                            onClick={() => {
                              retire.mutate(t.id);
                            }}
                          >
                            Retire
                          </button>
                        </>
                      ) : null}
                    </div>
                  </td>
                </tr>,
                openId === t.id ? (
                  <tr key={`${t.id}-detail`}>
                    <td className={tdCls} colSpan={10}>
                      {detail.isPending ? (
                        <p className="text-xs text-neutral-500">Loading detail…</p>
                      ) : detail.isError ? (
                        <ErrorBox error={detail.error} />
                      ) : (
                        <JsonRows rows={[detail.data]} />
                      )}
                    </td>
                  </tr>
                ) : null,
              ])}
            </tbody>
          </table>
        </div>
      ) : null}

      {versions !== null ? (
        <div className="mt-2 rounded border border-neutral-700 p-2" aria-label="Version chain">
          <p className="mb-1 text-xs font-semibold">Version chain (group of #{versions.forId})</p>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>v</th>
                <th className={thCls}>Flat</th>
                <th className={thCls}>Bps</th>
                <th className={thCls}>Min→Max</th>
                <th className={thCls}>Supersedes</th>
                <th className={thCls}>Effective</th>
              </tr>
            </thead>
            <tbody>
              {versions.rows.map((v) => (
                <tr key={v.id}>
                  <td className={tdCls}>v{v.version}</td>
                  <td className={tdCls}>{v.flatFee}</td>
                  <td className={tdCls}>{v.percentageBps}</td>
                  <td className={tdCls}>
                    {v.minFee}→{v.maxFee ?? '∞'}
                  </td>
                  <td className={tdCls}>{v.supersedesId ?? '—'}</td>
                  <td className={tdCls}>{v.effectiveDate.slice(0, 10)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {succeeding !== null ? (
        <form
          aria-label="Successor version"
          className="mt-2 flex flex-wrap items-end gap-2 rounded border border-neutral-700 p-2"
          onSubmit={(e) => {
            e.preventDefault();
            succeed.mutate();
          }}
        >
          <span className="text-sm">Successor of #{succeeding.id}:</span>
          {feeInputs(succ, setSucc)}
          <button type="submit" className={btnPrimary} disabled={succeed.isPending}>
            Insert successor (PUT)
          </button>
          <button type="button" className={btnGhost} onClick={() => setSucceeding(null)}>
            Cancel
          </button>
        </form>
      ) : null}

      <form
        aria-label="Create fee schedule"
        className="mt-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <input
          aria-label="Fee rail"
          className={inputCls}
          placeholder="rail"
          value={form.rail}
          onChange={(e) => {
            setForm({ ...form, rail: e.target.value });
          }}
        />
        <input
          aria-label="Fee currency"
          className={inputCls}
          placeholder="currency"
          value={form.currency}
          onChange={(e) => {
            setForm({ ...form, currency: e.target.value });
          }}
        />
        <select
          aria-label="Fee direction"
          className={selectCls}
          value={form.direction}
          onChange={(e) => {
            setForm({ ...form, direction: e.target.value });
          }}
        >
          <option value="DEPOSIT">DEPOSIT</option>
          <option value="WITHDRAWAL">WITHDRAWAL</option>
        </select>
        <input
          aria-label="Account tier"
          className={inputCls}
          placeholder="tier (*|T0|T1|T2)"
          value={form.tier}
          onChange={(e) => {
            setForm({ ...form, tier: e.target.value });
          }}
        />
        {feeInputs(form, setForm)}
        <button type="submit" className={btnPrimary} disabled={create.isPending}>
          Create v1
        </button>
      </form>
      <p className={hintTextCls}>
        Schedules are versioned: PUT inserts a successor row (supersedes); retire removes the row
        from resolution but it remains on the audit record.
      </p>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}

/** Shared flat/bps/min/max fee inputs for both create + successor forms. */
function feeInputs<
  T extends { flatFee: string; percentageBps: string; minFee: string; maxFee: string },
>(form: T, set: (f: T) => void) {
  return FEE_FIELDS.map(([key, label]) => (
    <input
      key={key}
      aria-label={label}
      className={inputCls}
      placeholder={label}
      value={form[key]}
      onChange={(e) => {
        set({ ...form, [key]: e.target.value });
      }}
    />
  ));
}
