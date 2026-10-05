/**
 * Travel-rule panel (Phase-10.5 Task 10.5.3.5 §2) — the MISSING_INFO
 * cure queue. Officers supply absent originator/beneficiary fields on
 * a selected record; the server re-validates and the held transfer
 * re-evaluates on the next dispatch sweep. Records in a terminal
 * REJECTED state cannot be amended (TRAVEL_RULE_REJECTED) — the form
 * surfaces the server outcome verbatim.
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
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { fetchTravelRule, supplyTravelRule, type TravelRuleRecord } from './api';

const STATUS_FILTERS = ['', 'MISSING_INFO', 'COMPLETE', 'REJECTED'] as const;

function PartyFields({
  prefix,
  title,
  value,
  onChange,
}: {
  prefix: string;
  title: string;
  value: { name: string; accountNumber: string; address: string; country: string };
  onChange: (v: { name: string; accountNumber: string; address: string; country: string }) => void;
}) {
  return (
    <fieldset className="space-y-2 rounded border border-neutral-800 p-2">
      <legend className="px-1 text-xs text-neutral-400">{title}</legend>
      <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
        <div>
          <label className={labelCls} htmlFor={`${prefix}-name`}>
            Name
          </label>
          <input
            id={`${prefix}-name`}
            className={inputCls}
            value={value.name}
            onChange={(e) => {
              onChange({ ...value, name: e.target.value });
            }}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor={`${prefix}-acct`}>
            Account number
          </label>
          <input
            id={`${prefix}-acct`}
            className={inputCls}
            value={value.accountNumber}
            onChange={(e) => {
              onChange({ ...value, accountNumber: e.target.value });
            }}
          />
        </div>
        {prefix === 'tr-org' ? (
          <div>
            <label className={labelCls} htmlFor={`${prefix}-addr`}>
              Address
            </label>
            <input
              id={`${prefix}-addr`}
              className={inputCls}
              value={value.address}
              onChange={(e) => {
                onChange({ ...value, address: e.target.value });
              }}
            />
          </div>
        ) : null}
        <div>
          <label className={labelCls} htmlFor={`${prefix}-cty`}>
            Country
          </label>
          <input
            id={`${prefix}-cty`}
            className={inputCls}
            value={value.country}
            onChange={(e) => {
              onChange({ ...value, country: e.target.value });
            }}
          />
        </div>
      </div>
    </fieldset>
  );
}

export function TravelRulePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState<string>('MISSING_INFO');
  const [selected, setSelected] = useState<TravelRuleRecord | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const empty = { name: '', accountNumber: '', address: '', country: '' };
  const [originator, setOriginator] = useState(empty);
  const [beneficiary, setBeneficiary] = useState(empty);

  const list = useQuery({
    queryKey: ['admin-travel-rule', statusFilter],
    queryFn: () => fetchTravelRule(adminApi, statusFilter),
  });

  const supply = useMutation({
    mutationFn: () => {
      const body: Parameters<typeof supplyTravelRule>[2] = {};
      if (originator.name !== '' || originator.accountNumber !== '') {
        body.originator = {
          name: originator.name === '' ? undefined : originator.name,
          accountNumber: originator.accountNumber === '' ? undefined : originator.accountNumber,
          address: originator.address === '' ? undefined : originator.address,
          country: originator.country === '' ? undefined : originator.country,
        };
      }
      if (beneficiary.name !== '' || beneficiary.accountNumber !== '') {
        body.beneficiary = {
          name: beneficiary.name === '' ? undefined : beneficiary.name,
          accountNumber: beneficiary.accountNumber === '' ? undefined : beneficiary.accountNumber,
          country: beneficiary.country === '' ? undefined : beneficiary.country,
        };
      }
      return supplyTravelRule(adminApi, selected?.id ?? 0, body);
    },
    onSuccess: (rec) => {
      setNotice(`Record ${rec.id} → ${rec.status}`);
      setSelected(rec);
      void qc.invalidateQueries({ queryKey: ['admin-travel-rule'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Supply failed'),
  });

  if (list.error !== null && isAccessDenied(list.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Travel rule">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Travel rule</h2>
        <div className="flex items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="tr-status">
              Status
            </label>
            <select
              id="tr-status"
              className={selectCls}
              value={statusFilter}
              onChange={(e) => {
                setStatusFilter(e.target.value);
              }}
            >
              {STATUS_FILTERS.map((s) => (
                <option key={s} value={s}>
                  {s === '' ? 'All' : s}
                </option>
              ))}
            </select>
          </div>
          <button
            type="button"
            className={btnGhost}
            onClick={() => void list.refetch()}
            disabled={list.isFetching}
          >
            Refresh
          </button>
        </div>
      </div>
      <p className={hintTextCls}>
        MISSING_INFO records hold the transfer until an officer supplies the absent party fields.
      </p>

      {list.error !== null ? <ErrorBox error={list.error} /> : null}
      {list.data?.length === 0 ? (
        <p className="mt-2 text-sm text-neutral-500">No records match this filter.</p>
      ) : null}
      {list.data !== undefined && list.data.length > 0 ? (
        <div className="mt-2 max-h-64 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Transfer</th>
                <th className={thCls}>Dir</th>
                <th className={thCls}>Account</th>
                <th className={thCls}>Amount</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Missing</th>
                <th className={thCls}>Hold</th>
              </tr>
            </thead>
            <tbody>
              {list.data.map((r) => (
                <tr
                  key={r.id}
                  className="cursor-pointer"
                  onClick={() => {
                    setSelected(r);
                    setNotice(null);
                  }}
                >
                  <td className={tdCls}>{r.id}</td>
                  <td className={tdCls}>{r.transferId}</td>
                  <td className={tdCls}>{r.direction}</td>
                  <td className={tdCls}>{r.accountId}</td>
                  <td className={tdCls}>
                    {r.amount ?? '—'} {r.currency}
                  </td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    {r.missingFields.length > 0 ? r.missingFields.join(', ') : '—'}
                  </td>
                  <td className={tdCls}>{r.holdRef ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {selected !== null ? (
        <form
          aria-label="Travel rule cure"
          className="mt-4 space-y-2 border-t border-neutral-800 pt-3"
          onSubmit={(e) => {
            e.preventDefault();
            supply.mutate();
          }}
        >
          <p className="text-sm">
            Cure record <strong>#{selected.id}</strong>{' '}
            <StatusBadge value={selected.status || 'UNKNOWN'} />
          </p>
          <PartyFields
            prefix="tr-org"
            title="Originator"
            value={originator}
            onChange={setOriginator}
          />
          <PartyFields
            prefix="tr-ben"
            title="Beneficiary"
            value={beneficiary}
            onChange={setBeneficiary}
          />
          <button type="submit" className={btnPrimary} disabled={supply.isPending}>
            Supply missing info
          </button>
        </form>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
