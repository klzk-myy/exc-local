/**
 * Surveillance tuning panel (Task 10.5.3.27 gate-coverage wiring,
 * Phase-21 Task 21.3.27) — detection-parameter versions: DRAFT
 * proposals with an FP-target budget, atomic ACTIVE swap, and the
 * FP-rate backtest calibration read.
 */
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import { btnGhost, btnPrimary, cardCls, hintTextCls, inputCls, labelCls } from '@/lib/ui';

import {
  activateTuning,
  backtestTuning,
  fetchTuning,
  proposeTuning,
  type TuningVersion,
} from './api';

export function TuningPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [notice, setNotice] = useState<string | null>(null);
  const [backtest, setBacktest] = useState<{ signal: string; raw: unknown } | null>(null);
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const tuning = useQuery({
    queryKey: ['admin-surveillance', 'tuning'],
    queryFn: () => fetchTuning(adminApi),
    retry: false,
  });

  const [form, setForm] = useState({ signalType: '', params: '{}', fpTarget: '' });
  const propose = useMutation({
    mutationFn: () =>
      proposeTuning(adminApi, {
        signalType: form.signalType,
        params: form.params,
        fpTargetPct: Number(form.fpTarget),
      }),
    onSuccess: () => {
      setNotice('Tuning proposal recorded as DRAFT');
      void tuning.refetch();
    },
    onError: onErr,
  });
  const activate = useMutation({
    mutationFn: (t: TuningVersion) => activateTuning(adminApi, t.signalType, t.version),
    onSuccess: (_r, t) => {
      setNotice(`${t.signalType} v${t.version} ACTIVE`);
      void tuning.refetch();
    },
    onError: onErr,
  });
  const bt = useMutation({
    mutationFn: (signal: string) => backtestTuning(adminApi, signal),
    onSuccess: (raw, signal) => setBacktest({ signal, raw }),
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Surveillance tuning">
      <h2 className="mb-2 text-sm font-semibold">Detection tuning</h2>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}

      {tuning.isPending && <p className={hintTextCls}>Loading…</p>}
      {tuning.isError && (
        <p role="alert" className="text-xs text-rose-300">
          {tuning.error.message}
        </p>
      )}
      <ul className="mb-3 divide-y divide-neutral-800 text-xs">
        {(tuning.data ?? []).map((t) => (
          <li
            key={`${t.signalType}:${t.version}`}
            className="flex items-center justify-between py-1.5"
          >
            <span>
              {t.signalType} v{t.version} · {t.status}
              {t.fpTargetPct !== undefined ? ` · fp≤${t.fpTargetPct}%` : ''}
            </span>
            <span className="flex gap-2">
              <button
                type="button"
                className={btnGhost}
                onClick={() => bt.mutate(t.signalType)}
                disabled={bt.isPending}
              >
                Backtest
              </button>
              {t.status !== 'ACTIVE' && (
                <button
                  type="button"
                  className={btnGhost}
                  onClick={() => activate.mutate(t)}
                  disabled={activate.isPending}
                >
                  Activate
                </button>
              )}
            </span>
          </li>
        ))}
        {tuning.data?.length === 0 && <li className={hintTextCls}>No tuning versions.</li>}
      </ul>
      {backtest !== null && (
        <pre
          aria-label={`Backtest ${backtest.signal}`}
          className="mb-3 max-h-48 overflow-y-auto rounded border border-neutral-800 p-2 text-xs text-neutral-300"
          tabIndex={0}
        >
          {JSON.stringify(backtest.raw, null, 2)}
        </pre>
      )}

      <form
        className="grid gap-2 md:grid-cols-3"
        onSubmit={(e) => {
          e.preventDefault();
          propose.mutate();
        }}
      >
        <label className={labelCls}>
          Signal type
          <input
            className={inputCls}
            value={form.signalType}
            onChange={(e) => setForm({ ...form, signalType: e.target.value })}
            required
          />
        </label>
        <label className={labelCls}>
          Params (JSON)
          <input
            className={inputCls}
            value={form.params}
            onChange={(e) => setForm({ ...form, params: e.target.value })}
            required
          />
        </label>
        <label className={labelCls}>
          FP target %
          <input
            className={inputCls}
            value={form.fpTarget}
            onChange={(e) => setForm({ ...form, fpTarget: e.target.value })}
            required
            inputMode="decimal"
          />
        </label>
        <button type="submit" className={btnPrimary} disabled={propose.isPending}>
          Propose DRAFT
        </button>
      </form>
    </section>
  );
}
