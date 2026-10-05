/**
 * Netting & rail ops panel (Task 10.5.3.27 gate-coverage wiring) —
 * bilateral netting cycles (run/batches/lines/dispatch/settle/bust,
 * Task 24.3.9), rail cut-off schedules + evaluate preview, and the
 * per-instruction roll (Task 24.3.20).
 */
import { skipToken, useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  hintTextCls,
  inputCls,
  labelCls,
  StatusBadge,
} from '@/lib/ui';

import {
  bustBatch,
  dispatchBatch,
  evaluateRail,
  fetchBatchLines,
  fetchNettingBatches,
  fetchRailSchedules,
  rollInstruction,
  runNetting,
  settleBatch,
  type NettingBatch,
} from './api';

export function NettingPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [notice, setNotice] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<number | null>(null);
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const batches = useQuery({
    queryKey: ['admin-settlement', 'netting-batches'],
    queryFn: () => fetchNettingBatches(adminApi),
    retry: false,
  });
  const lines = useQuery({
    queryKey: ['admin-settlement', 'batch-lines', expanded],
    queryFn: expanded === null ? skipToken : () => fetchBatchLines(adminApi, expanded),
    retry: false,
  });
  const schedules = useQuery({
    queryKey: ['admin-settlement', 'rail-schedules'],
    queryFn: () => fetchRailSchedules(adminApi),
    retry: false,
  });

  const [run, setRun] = useState({ cp: '', currency: 'USD', valueDate: '' });
  const doRun = useMutation({
    mutationFn: () =>
      runNetting(adminApi, {
        counterpartyAccountId: Number(run.cp),
        currency: run.currency,
        valueDate: run.valueDate,
      }),
    onSuccess: () => {
      setNotice('Netting cycle executed');
      void batches.refetch();
    },
    onError: onErr,
  });

  const [verb, setVerb] = useState({
    id: '',
    rail: 'SWIFT',
    confirmationRef: '',
    reason: '',
    ids: '',
  });
  const op = useMutation({
    mutationFn: (kind: 'dispatch' | 'settle' | 'bust') => {
      const id = Number(verb.id);
      if (kind === 'dispatch') return dispatchBatch(adminApi, id, verb.rail);
      if (kind === 'settle') return settleBatch(adminApi, id, verb.confirmationRef);
      return bustBatch(
        adminApi,
        id,
        verb.ids
          .split(',')
          .map((s) => Number(s.trim()))
          .filter((n) => Number.isFinite(n) && n > 0),
        verb.reason,
      );
    },
    onSuccess: (_r, kind) => {
      setNotice(`Batch #${verb.id} ${kind} applied`);
      void batches.refetch();
    },
    onError: onErr,
  });

  const [evalForm, setEvalForm] = useState({ rail: 'SWIFT', currency: 'USD', at: '' });
  const doEval = useMutation({
    mutationFn: () => evaluateRail(adminApi, evalForm),
    onSuccess: (res) => setNotice(`Rail evaluation — ${JSON.stringify(res)}`),
    onError: onErr,
  });
  const [rollId, setRollId] = useState('');
  const doRoll = useMutation({
    mutationFn: () => rollInstruction(adminApi, Number(rollId)),
    onSuccess: () => setNotice(`Instruction #${rollId} rolled to next value date`),
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Netting and rails">
      <h2 className="mb-2 text-sm font-semibold">Netting, rails &amp; rolls</h2>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}

      <form
        className="mb-3 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          doRun.mutate();
        }}
      >
        <label className={labelCls}>
          Counterparty account id
          <input
            className={inputCls}
            value={run.cp}
            onChange={(e) => setRun({ ...run, cp: e.target.value })}
            required
            inputMode="numeric"
          />
        </label>
        <label className={labelCls}>
          Currency
          <input
            className={inputCls}
            value={run.currency}
            onChange={(e) => setRun({ ...run, currency: e.target.value })}
            required
          />
        </label>
        <label className={labelCls}>
          Value date
          <input
            className={inputCls}
            type="date"
            value={run.valueDate}
            onChange={(e) => setRun({ ...run, valueDate: e.target.value })}
            required
          />
        </label>
        <button type="submit" className={btnPrimary} disabled={doRun.isPending}>
          Run netting cycle
        </button>
      </form>

      {batches.isPending && <p className={hintTextCls}>Loading batches…</p>}
      {batches.isError && (
        <p role="alert" className="text-xs text-rose-300">
          {batches.error.message}
        </p>
      )}
      <ul className="mb-3 divide-y divide-neutral-800 text-xs">
        {(batches.data ?? []).map((b: NettingBatch) => (
          <li key={b.id}>
            <button
              type="button"
              className="flex w-full items-center justify-between py-1.5 text-left"
              onClick={() => setExpanded(expanded === b.id ? null : b.id)}
              aria-expanded={expanded === b.id}
            >
              <span>
                #{b.id} cp {b.counterpartyAccountId} · {b.currency} · {b.valueDate}
              </span>
              <StatusBadge value={b.status} />
            </button>
            {expanded === b.id && (
              <div className="mb-2 rounded border border-neutral-800 p-2">
                {lines.isPending && <p className={hintTextCls}>Loading lines…</p>}
                {lines.isError && (
                  <p role="alert" className="text-xs text-rose-300">
                    {lines.error.message}
                  </p>
                )}
                <ul className="max-h-40 overflow-y-auto font-mono text-neutral-300" tabIndex={0}>
                  {(lines.data ?? []).map((l, i) => (
                    <li key={i} className="py-0.5">
                      {JSON.stringify(l)}
                    </li>
                  ))}
                  {lines.data?.length === 0 && <li className={hintTextCls}>No lines.</li>}
                </ul>
              </div>
            )}
          </li>
        ))}
        {batches.data?.length === 0 && <li className={hintTextCls}>No netting batches.</li>}
      </ul>

      <div className="mb-4 flex flex-wrap items-end gap-2">
        <label className={labelCls}>
          Batch id
          <input
            className={inputCls}
            value={verb.id}
            onChange={(e) => setVerb({ ...verb, id: e.target.value })}
            inputMode="numeric"
          />
        </label>
        <label className={labelCls}>
          Rail (dispatch)
          <input
            className={inputCls}
            value={verb.rail}
            onChange={(e) => setVerb({ ...verb, rail: e.target.value })}
          />
        </label>
        <label className={labelCls}>
          Confirmation ref (settle)
          <input
            className={inputCls}
            value={verb.confirmationRef}
            onChange={(e) => setVerb({ ...verb, confirmationRef: e.target.value })}
          />
        </label>
        <label className={labelCls}>
          Instruction ids (bust, csv)
          <input
            className={inputCls}
            value={verb.ids}
            onChange={(e) => setVerb({ ...verb, ids: e.target.value })}
          />
        </label>
        <label className={labelCls}>
          Reason (bust)
          <input
            className={inputCls}
            value={verb.reason}
            onChange={(e) => setVerb({ ...verb, reason: e.target.value })}
          />
        </label>
        <div className="flex gap-2">
          <button
            type="button"
            className={btnGhost}
            disabled={verb.id === '' || op.isPending}
            onClick={() => op.mutate('dispatch')}
          >
            Dispatch
          </button>
          <button
            type="button"
            className={btnGhost}
            disabled={verb.id === '' || op.isPending}
            onClick={() => op.mutate('settle')}
          >
            Settle
          </button>
          <button
            type="button"
            className={btnGhost}
            disabled={verb.id === '' || verb.reason === '' || op.isPending}
            onClick={() => op.mutate('bust')}
          >
            Bust
          </button>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Rail cut-off schedules
          </h3>
          <ul className="text-xs">
            {(schedules.data ?? []).map((s, i) => (
              <li key={i} className="py-0.5 font-mono text-neutral-300">
                {JSON.stringify(s)}
              </li>
            ))}
            {schedules.data?.length === 0 && <li className={hintTextCls}>No schedules.</li>}
          </ul>
        </div>
        <form
          className="grid content-start gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            doEval.mutate();
          }}
        >
          <h3 className="text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Evaluate cut-off
          </h3>
          <label className={labelCls}>
            Rail
            <input
              className={inputCls}
              value={evalForm.rail}
              onChange={(e) => setEvalForm({ ...evalForm, rail: e.target.value })}
              required
            />
          </label>
          <label className={labelCls}>
            Currency
            <input
              className={inputCls}
              value={evalForm.currency}
              onChange={(e) => setEvalForm({ ...evalForm, currency: e.target.value })}
              required
            />
          </label>
          <label className={labelCls}>
            At (RFC3339, optional)
            <input
              className={inputCls}
              value={evalForm.at}
              onChange={(e) => setEvalForm({ ...evalForm, at: e.target.value })}
            />
          </label>
          <button type="submit" className={btnGhost} disabled={doEval.isPending}>
            Evaluate
          </button>
        </form>
      </div>

      <form
        className="mt-4 flex items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          doRoll.mutate();
        }}
      >
        <label className={labelCls}>
          Roll settlement instruction id
          <input
            className={inputCls}
            value={rollId}
            onChange={(e) => setRollId(e.target.value)}
            required
            inputMode="numeric"
          />
        </label>
        <button type="submit" className={btnGhost} disabled={doRoll.isPending || rollId === ''}>
          Roll to next value date
        </button>
      </form>
    </section>
  );
}
