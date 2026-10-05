/**
 * Server-computed pip value (Task 10.5.3.27 gate-coverage wiring,
 * Phase-03 Task 3.3.12) — GET /instruments/{symbol}/pip-value
 * ?lots=&account_currency=. Cross-currency conversion leg is shown
 * verbatim (pair/rate/divide); the local calculator above stays the
 * offline estimator — this card is the oracle-backed figure.
 */
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import { ErrorBox, btnGhost, cardCls, inputCls, labelCls } from '@/lib/ui';

interface PipValueResult {
  symbol: string;
  lots: string;
  account_currency: string;
  pip_size: string;
  contract_size: string;
  pip_value_quote_ccy: string;
  pip_value: string;
  conversion_pair: string;
  conversion_rate: string;
  conversion_divide: boolean;
  cached: boolean;
}

export function ServerPipValue({ initialSymbol }: { initialSymbol: string }) {
  const [symbol, setSymbol] = useState(initialSymbol);
  const [lots, setLots] = useState('1');
  const [ccy, setCcy] = useState('USD');

  const q = useMutation({
    mutationFn: () =>
      apiClient.get<PipValueResult>(
        `/instruments/${encodeURIComponent(symbol)}/pip-value?lots=${encodeURIComponent(lots)}&account_currency=${encodeURIComponent(ccy)}`,
      ),
  });

  const r = q.data;
  return (
    <section className={`${cardCls} mt-4`} aria-label="Server pip value">
      <h2 className="mb-1 text-sm font-semibold">Server pip value</h2>
      <p className="mb-3 text-xs text-neutral-500">
        Oracle-backed pip value with cross-currency conversion (Phase-03 Task 3.3.12).
      </p>
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          q.mutate();
        }}
      >
        <label className={labelCls}>
          Symbol
          <input
            className={inputCls}
            value={symbol}
            onChange={(e) => setSymbol(e.target.value)}
            required
          />
        </label>
        <label className={labelCls}>
          Lots
          <input
            className={inputCls}
            value={lots}
            onChange={(e) => setLots(e.target.value)}
            required
            inputMode="decimal"
          />
        </label>
        <label className={labelCls}>
          Account currency
          <input
            className={inputCls}
            value={ccy}
            onChange={(e) => setCcy(e.target.value.toUpperCase())}
            required
            maxLength={3}
          />
        </label>
        <button type="submit" className={btnGhost} disabled={q.isPending}>
          {q.isPending ? 'Computing…' : 'Compute'}
        </button>
      </form>
      {q.isError && (
        <div className="mt-2">
          <ErrorBox error={q.error} />
        </div>
      )}
      {r !== undefined && (
        <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1 text-xs md:grid-cols-3">
          <dt className="text-neutral-500">Pip size</dt>
          <dd className="font-mono">{r.pip_size}</dd>
          <dt className="text-neutral-500">Contract size</dt>
          <dd className="font-mono">{r.contract_size}</dd>
          <dt className="text-neutral-500">Value (quote ccy)</dt>
          <dd className="font-mono">{r.pip_value_quote_ccy}</dd>
          <dt className="text-neutral-500">Value ({r.account_currency})</dt>
          <dd className="font-mono font-semibold text-emerald-300">{r.pip_value}</dd>
          {r.conversion_pair !== '' && (
            <>
              <dt className="text-neutral-500">Conversion leg</dt>
              <dd className="font-mono">
                {r.conversion_pair} @ {r.conversion_rate}
                {r.conversion_divide ? ' (÷)' : ' (×)'}
              </dd>
            </>
          )}
          {r.cached && <dd className="col-span-full text-neutral-500">served from cache</dd>}
        </dl>
      )}
    </section>
  );
}
