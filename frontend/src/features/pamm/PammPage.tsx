/**
 * PAMM pool surface (Phase-14 Task 14.3.8) — browse ACTIVE pools,
 * view detail + caller allocation, invest/redeem (typed INTERNAL
 * investment movements, not fiat funding), and manager pool creation.
 * Amounts are decimal strings end-to-end.
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { UnavailablePanel } from '@/lib/input-helpers';
import { btnGhost, btnPrimary, inputCls, labelCls, tableCls, tdCls, thCls } from '@/lib/ui';
import { ErrorBox } from '@/lib/ui';

import {
  createPool,
  invest,
  listPools,
  poolDetail,
  poolStatement,
  redeem,
  type PammPool,
} from './api';

function isUnavailable(err: unknown): boolean {
  return err instanceof ApiError && err.status >= 500;
}

function CreatePoolForm() {
  const qc = useQueryClient();
  const [name, setName] = useState('');
  const [currency, setCurrency] = useState('USD');
  const [minInv, setMinInv] = useState('100');
  const create = useMutation({
    mutationFn: () => createPool(apiClient, { name, currency, minInvestment: minInv }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['pamm', 'pools'] }),
  });
  return (
    <form
      className="space-y-2 rounded border border-neutral-800 p-3"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate();
      }}
    >
      <p className="text-xs font-medium text-neutral-300">Manage a pool</p>
      <div className="flex flex-wrap items-end gap-2 text-xs">
        <label className={labelCls}>
          Name
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
            className={`${inputCls} ml-1 w-36`}
          />
        </label>
        <label className={labelCls}>
          Currency
          <input
            value={currency}
            onChange={(e) => setCurrency(e.target.value.toUpperCase())}
            required
            maxLength={4}
            className={`${inputCls} ml-1 w-16`}
          />
        </label>
        <label className={labelCls}>
          Min investment
          <input
            value={minInv}
            onChange={(e) => setMinInv(e.target.value)}
            required
            inputMode="decimal"
            className={`${inputCls} ml-1 w-24`}
          />
        </label>
        <button type="submit" disabled={create.isPending} className={btnPrimary}>
          {create.isPending ? 'Creating…' : 'Create pool'}
        </button>
      </div>
      {create.error ? <ErrorBox error={create.error} /> : null}
    </form>
  );
}

function Detail({ poolId }: { poolId: number }) {
  const qc = useQueryClient();
  const [amount, setAmount] = useState('');
  const [err, setErr] = useState<unknown>(null);
  const detail = useQuery({
    queryKey: ['pamm', 'pool', poolId],
    queryFn: () => poolDetail(apiClient, poolId),
    retry: false,
  });
  const stmt = useQuery({
    queryKey: ['pamm', 'statement', poolId],
    queryFn: () => poolStatement(apiClient, poolId),
    retry: false,
  });
  const move = useMutation({
    mutationFn: (kind: 'invest' | 'redeem') =>
      kind === 'invest' ? invest(apiClient, poolId, amount) : redeem(apiClient, poolId, amount),
    onSuccess: () => {
      setAmount('');
      void qc.invalidateQueries({ queryKey: ['pamm'] });
    },
    onError: (e) => setErr(e),
  });

  if (detail.error) {
    return isUnavailable(detail.error) ? (
      <UnavailablePanel
        feature="PAMM pool detail"
        owner="Phase-14 Task 14.3.8"
        note="Pool detail is temporarily unavailable."
      />
    ) : (
      <ErrorBox error={detail.error} />
    );
  }
  const d = detail.data;
  if (!d) return <p className="p-3 text-xs text-neutral-500">Loading…</p>;

  return (
    <div className="space-y-3 rounded border border-neutral-800 p-3">
      <div className="flex items-baseline justify-between">
        <h3 className="text-sm font-semibold text-neutral-200">{d.pool.name}</h3>
        <span className="text-xs text-neutral-500">
          {d.investorCount} investors · {d.totalInvested} {d.pool.currency} pooled
        </span>
      </div>
      {d.myAllocation ? (
        <p className="text-xs text-emerald-400">
          Your allocation: {d.myAllocation.invested} {d.pool.currency} ({d.myAllocation.status})
        </p>
      ) : (
        <p className="text-xs text-neutral-500">
          Min investment {d.pool.minInvestment} {d.pool.currency} — you hold no allocation.
        </p>
      )}
      <form
        className="flex items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
        }}
      >
        <label className={labelCls}>
          Amount ({d.pool.currency})
          <input
            value={amount}
            onChange={(e) => setAmount(e.target.value)}
            inputMode="decimal"
            required
            className={`${inputCls} ml-1 w-28`}
            data-testid="pamm-amount"
          />
        </label>
        <button
          type="button"
          disabled={move.isPending || amount.trim() === ''}
          onClick={() => {
            setErr(null);
            move.mutate('invest');
          }}
          className={btnPrimary}
        >
          Invest
        </button>
        <button
          type="button"
          disabled={move.isPending || amount.trim() === '' || d.myAllocation === null}
          onClick={() => {
            setErr(null);
            move.mutate('redeem');
          }}
          className={btnGhost}
        >
          Redeem
        </button>
      </form>
      {err !== null && <ErrorBox error={err} />}

      <div>
        <p className="mb-1 text-xs font-medium text-neutral-400">Statement</p>
        {stmt.error ? (
          <ErrorBox error={stmt.error} />
        ) : (stmt.data ?? []).length === 0 ? (
          <p className="py-2 text-center text-xs text-neutral-500">No movements.</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Type</th>
                <th className={thCls}>Dir</th>
                <th className={thCls}>Amount</th>
                <th className={thCls}>Narrative</th>
                <th className={thCls}>Posted</th>
              </tr>
            </thead>
            <tbody>
              {(stmt.data ?? []).map((e) => (
                <tr key={e.entryId}>
                  <td className={`${tdCls} font-mono`}>{e.txnType}</td>
                  <td className={tdCls}>{e.direction}</td>
                  <td className={`${tdCls} font-mono`}>
                    {e.amount} {e.currency}
                  </td>
                  <td className={`${tdCls} max-w-40 truncate`}>{e.narrative || '—'}</td>
                  <td className={tdCls}>
                    {e.postedAt
                      ? new Date(e.postedAt).toLocaleString('en-US', { hour12: false })
                      : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

function PoolRow({ pool, onSelect }: { pool: PammPool; onSelect: () => void }) {
  return (
    <tr>
      <td className={tdCls}>{pool.name}</td>
      <td className={`${tdCls} font-mono`}>{pool.currency}</td>
      <td className={`${tdCls} font-mono`}>{pool.minInvestment}</td>
      <td className={tdCls}>{pool.status}</td>
      <td className={tdCls}>
        <button type="button" onClick={onSelect} className={btnGhost}>
          Open
        </button>
      </td>
    </tr>
  );
}

export default function PammPage() {
  const [selected, setSelected] = useState<number | null>(null);
  const q = useQuery({ queryKey: ['pamm', 'pools'], queryFn: () => listPools(apiClient) });

  if (q.error) {
    return isUnavailable(q.error) ? (
      <UnavailablePanel
        feature="PAMM pools"
        owner="Phase-14 Task 14.3.8"
        note="Pool discovery is temporarily unavailable."
      />
    ) : (
      <div className="p-4">
        <ErrorBox error={q.error} />
      </div>
    );
  }
  const pools = q.data ?? [];

  return (
    <div className="mx-auto max-w-4xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">PAMM pools</h1>
        <p className="text-sm text-neutral-500">
          Pooled-investment accounts — allocations post as internal PAMM_INVEST/PAMM_REDEEM
          transfers and never touch fiat rails or withdrawal caps.
        </p>
      </header>
      <CreatePoolForm />
      {pools.length === 0 ? (
        <p className="rounded border border-neutral-800 p-6 text-center text-sm text-neutral-500">
          {q.isLoading ? 'Loading pools…' : 'No active pools.'}
        </p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Pool</th>
              <th className={thCls}>Currency</th>
              <th className={thCls}>Min invest</th>
              <th className={thCls}>Status</th>
              <th className={thCls}></th>
            </tr>
          </thead>
          <tbody>
            {pools.map((p) => (
              <PoolRow key={p.poolId} pool={p} onSelect={() => setSelected(p.poolId)} />
            ))}
          </tbody>
        </table>
      )}
      {selected !== null && <Detail poolId={selected} />}
    </div>
  );
}
