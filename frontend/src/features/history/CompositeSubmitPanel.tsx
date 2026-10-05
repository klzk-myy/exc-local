/**
 * Composite order submitters (Task 10.5.3.20) — the mounted typed
 * endpoints the classic ticket does not compose:
 *
 *   POST /orders/{twap,vwap,scaled,spread}   slicing algos
 *   POST /orders/basket                    cross-shard multi-leg
 *   POST /orders/roll                      forward/swap roll (Phase-22)
 *   POST/DELETE /orders/batch              atomic batch submit/cancel
 *   DELETE /orders/all · DELETE /orders?symbol=  mass cancel
 *
 * Flat body convention (algo.ParseTypedSubmit): symbol/side/total_qty
 * plus the strategy params inline — the whole body is the params doc.
 * 202 acks render the returned view; errors surface verbatim.
 */
import { useState, type FormEvent } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ErrorBox, Field, btnGhost, btnPrimary, cardCls, inputCls, selectCls } from '@/lib/ui';

import * as api from './api';

type AlgoKind = 'twap' | 'vwap' | 'scaled' | 'spread' | 'basket' | 'roll';
const ALGO_LABEL: Record<AlgoKind, string> = {
  twap: 'TWAP',
  vwap: 'VWAP',
  scaled: 'Scaled (iceberg ladder)',
  spread: 'Spread (2 legs)',
  basket: 'Basket (multi-leg)',
  roll: 'Forward roll',
};

interface LegDraft {
  symbol: string;
  side: string;
  quantity: string;
}

const SIDE_OPTIONS = ['BUY', 'SELL'] as const;

function useInvalidate() {
  const qc = useQueryClient();
  return async () => {
    await qc.invalidateQueries({ queryKey: ['history'] });
    await qc.invalidateQueries({ queryKey: ['orders'] });
  };
}

export function CompositeSubmitPanel() {
  const invalidate = useInvalidate();
  const [kind, setKind] = useState<AlgoKind>('twap');
  const [symbol, setSymbol] = useState('EUR/USD');
  const [side, setSide] = useState<string>('BUY');
  const [qty, setQty] = useState('');
  const [intervalSecs, setIntervalSecs] = useState('60');
  const [durationSecs, setDurationSecs] = useState('');
  const [discretion, setDiscretion] = useState('');
  const [levels, setLevels] = useState('5');
  const [distribution, setDistribution] = useState('EQUAL');
  const [startPrice, setStartPrice] = useState('');
  const [spacingPips, setSpacingPips] = useState('');
  const [spreadPrice, setSpreadPrice] = useState('');
  const [legs, setLegs] = useState<LegDraft[]>([
    { symbol: 'EUR/USD', side: 'BUY', quantity: '' },
    { symbol: 'GBP/USD', side: 'SELL', quantity: '' },
  ]);
  const [contractId, setContractId] = useState('');
  const [rollDate, setRollDate] = useState('');
  const [rollTenor, setRollTenor] = useState('');
  const [rollCap, setRollCap] = useState('');

  const submit = useMutation({
    mutationFn: (body: Record<string, unknown>) => {
      switch (kind) {
        case 'twap':
        case 'vwap':
        case 'scaled':
        case 'spread':
          return api.submitTypedAlgo(apiClient, kind, body);
        case 'basket':
          return api.submitBasket(apiClient, legs);
        case 'roll':
          return api.submitRoll(apiClient, {
            contract_id: Number(contractId),
            new_value_date: rollDate !== '' ? rollDate : undefined,
            tenor: rollTenor !== '' ? rollTenor : undefined,
            max_roll_price_bps: rollCap !== '' ? rollCap : undefined,
          });
      }
    },
    onSuccess: invalidate,
  });

  const legOk = legs.every((l) => l.symbol !== '' && l.quantity !== '');
  const valid =
    kind === 'basket' || kind === 'spread'
      ? legOk && (kind === 'basket' || spreadPrice !== '')
      : kind === 'roll'
        ? contractId !== '' && (rollDate !== '' || rollTenor !== '')
        : qty !== '' && (kind === 'scaled' || durationSecs !== '');

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    const opt = (v: string, k: string, o: Record<string, unknown>) => {
      if (v !== '') o[k] = v;
    };
    switch (kind) {
      case 'twap': {
        const body: Record<string, unknown> = {
          symbol,
          side,
          total_qty: qty,
          interval_secs: Number(intervalSecs),
          duration_secs: Number(durationSecs),
        };
        opt(discretion, 'discretion_pips', body);
        submit.mutate(body);
        break;
      }
      case 'vwap': {
        const body: Record<string, unknown> = {
          symbol,
          side,
          total_qty: qty,
          duration_secs: Number(durationSecs),
        };
        opt(intervalSecs, 'interval_secs', body);
        opt(discretion, 'discretion_pips', body);
        submit.mutate(body);
        break;
      }
      case 'scaled': {
        const body: Record<string, unknown> = {
          symbol,
          side,
          total_qty: qty,
          levels: Number(levels),
          distribution,
        };
        opt(startPrice, 'start_price', body);
        opt(spacingPips, 'spacing_pips', body);
        submit.mutate(body);
        break;
      }
      case 'spread':
        submit.mutate({ legs, spread_price: spreadPrice });
        break;
      case 'basket':
      case 'roll':
        submit.mutate({});
        break;
    }
  };

  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">Composite order submit</h2>
      <form className="grid gap-3 sm:grid-cols-3" onSubmit={onSubmit}>
        <Field label="Strategy">
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={kind}
              onChange={(e) => {
                setKind(e.target.value as AlgoKind);
              }}
            >
              {(Object.keys(ALGO_LABEL) as AlgoKind[]).map((k) => (
                <option key={k} value={k}>
                  {ALGO_LABEL[k]}
                </option>
              ))}
            </select>
          )}
        </Field>
        {kind === 'twap' || kind === 'vwap' || kind === 'scaled' ? (
          <>
            <Field label="Symbol" required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  value={symbol}
                  onChange={(e) => {
                    setSymbol(e.target.value.toUpperCase());
                  }}
                />
              )}
            </Field>
            <Field label="Side">
              {(id) => (
                <select
                  id={id}
                  className={selectCls}
                  value={side}
                  onChange={(e) => {
                    setSide(e.target.value);
                  }}
                >
                  {SIDE_OPTIONS.map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            <Field label="Total quantity" required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="decimal"
                  value={qty}
                  onChange={(e) => {
                    setQty(e.target.value);
                  }}
                />
              )}
            </Field>
          </>
        ) : null}
        {kind === 'twap' || kind === 'vwap' ? (
          <>
            <Field
              label={`Interval seconds${kind === 'twap' ? ' (1–3600)' : ''}`}
              required={kind === 'twap'}
            >
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="numeric"
                  value={intervalSecs}
                  onChange={(e) => {
                    setIntervalSecs(e.target.value);
                  }}
                />
              )}
            </Field>
            <Field label="Duration seconds" required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="numeric"
                  value={durationSecs}
                  onChange={(e) => {
                    setDurationSecs(e.target.value);
                  }}
                />
              )}
            </Field>
            <Field label="Discretion pips (0–3)">
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="numeric"
                  value={discretion}
                  onChange={(e) => {
                    setDiscretion(e.target.value);
                  }}
                />
              )}
            </Field>
          </>
        ) : null}
        {kind === 'scaled' ? (
          <>
            <Field label="Levels (1–20)" required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="numeric"
                  value={levels}
                  onChange={(e) => {
                    setLevels(e.target.value);
                  }}
                />
              )}
            </Field>
            <Field label="Distribution">
              {(id) => (
                <select
                  id={id}
                  className={selectCls}
                  value={distribution}
                  onChange={(e) => {
                    setDistribution(e.target.value);
                  }}
                >
                  {['EQUAL', 'LINEAR', 'CUSTOM'].map((d) => (
                    <option key={d} value={d}>
                      {d}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            <Field label="Start price">
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="decimal"
                  placeholder="current mid"
                  value={startPrice}
                  onChange={(e) => {
                    setStartPrice(e.target.value);
                  }}
                />
              )}
            </Field>
            <Field label="Spacing (pips)">
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="decimal"
                  value={spacingPips}
                  onChange={(e) => {
                    setSpacingPips(e.target.value);
                  }}
                />
              )}
            </Field>
          </>
        ) : null}
        {kind === 'spread' || kind === 'basket' ? (
          <div className="sm:col-span-3">
            {legs.map((l, i) => (
              <div key={i} className="mb-2 grid grid-cols-[2fr_1fr_2fr_auto] items-end gap-2">
                <Field label={i === 0 ? 'Leg symbol' : `Leg ${i + 1} symbol`}>
                  {(id) => (
                    <input
                      id={id}
                      className={inputCls}
                      value={l.symbol}
                      onChange={(e) => {
                        const v = e.target.value.toUpperCase();
                        setLegs((ls) => ls.map((x, j) => (j === i ? { ...x, symbol: v } : x)));
                      }}
                    />
                  )}
                </Field>
                <Field label={i === 0 ? 'Side' : `Leg ${i + 1} side`}>
                  {(id) => (
                    <select
                      id={id}
                      className={selectCls}
                      value={l.side}
                      onChange={(e) => {
                        setLegs((ls) =>
                          ls.map((x, j) => (j === i ? { ...x, side: e.target.value } : x)),
                        );
                      }}
                    >
                      {SIDE_OPTIONS.map((s) => (
                        <option key={s} value={s}>
                          {s}
                        </option>
                      ))}
                    </select>
                  )}
                </Field>
                <Field label={i === 0 ? 'Quantity' : `Leg ${i + 1} quantity`}>
                  {(id) => (
                    <input
                      id={id}
                      className={inputCls}
                      inputMode="decimal"
                      value={l.quantity}
                      onChange={(e) => {
                        setLegs((ls) =>
                          ls.map((x, j) => (j === i ? { ...x, quantity: e.target.value } : x)),
                        );
                      }}
                    />
                  )}
                </Field>
                {kind === 'basket' && legs.length > 1 ? (
                  <button
                    type="button"
                    aria-label={`Remove leg ${i + 1}`}
                    className={`${btnGhost} mb-0.5 text-xs`}
                    onClick={() => {
                      setLegs((ls) => ls.filter((_, j) => j !== i));
                    }}
                  >
                    ✕
                  </button>
                ) : null}
              </div>
            ))}
            {kind === 'basket' ? (
              <button
                type="button"
                className={`${btnGhost} text-xs`}
                onClick={() => {
                  setLegs((ls) => [...ls, { symbol: '', side: 'BUY', quantity: '' }]);
                }}
              >
                + Add leg
              </button>
            ) : null}
            {kind === 'spread' ? (
              <Field label="Spread price" required>
                {(id) => (
                  <input
                    id={id}
                    className={inputCls}
                    inputMode="decimal"
                    value={spreadPrice}
                    onChange={(e) => {
                      setSpreadPrice(e.target.value);
                    }}
                  />
                )}
              </Field>
            ) : null}
          </div>
        ) : null}
        {kind === 'roll' ? (
          <>
            <Field label="Contract id" required>
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="numeric"
                  value={contractId}
                  onChange={(e) => {
                    setContractId(e.target.value);
                  }}
                />
              )}
            </Field>
            <Field label="New value date (or tenor)">
              {(id) => (
                <input
                  id={id}
                  type="date"
                  className={inputCls}
                  value={rollDate}
                  onChange={(e) => {
                    setRollDate(e.target.value);
                  }}
                />
              )}
            </Field>
            <Field label="Tenor (e.g. 1M)">
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  placeholder="1M"
                  value={rollTenor}
                  onChange={(e) => {
                    setRollTenor(e.target.value);
                  }}
                />
              )}
            </Field>
            <Field label="Max roll price (bps)">
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  inputMode="decimal"
                  value={rollCap}
                  onChange={(e) => {
                    setRollCap(e.target.value);
                  }}
                />
              )}
            </Field>
          </>
        ) : null}
        <div className="flex items-end gap-2 sm:col-span-3">
          <button type="submit" className={btnPrimary} disabled={!valid || submit.isPending}>
            {submit.isPending ? 'Submitting…' : `Submit ${ALGO_LABEL[kind]}`}
          </button>
          {submit.isSuccess ? (
            <span role="status" className="text-xs text-emerald-400">
              Accepted — tracking under Open orders / Algo orders.
            </span>
          ) : null}
        </div>
      </form>
      {submit.isError ? (
        <div className="mt-2">
          <ErrorBox error={submit.error} />
        </div>
      ) : null}
    </div>
  );
}

/** Batch submit/cancel + scoped & full mass cancel (Task 10.5.3.20). */
export function BatchOpsPanel() {
  const invalidate = useInvalidate();
  const [batchJson, setBatchJson] = useState('');
  const [cancelIds, setCancelIds] = useState('');
  const [cancelClientIds, setCancelClientIds] = useState('');
  const [symbol, setSymbol] = useState('');
  const [massSide, setMassSide] = useState('');
  const [confirmAll, setConfirmAll] = useState(false);

  const run = useMutation({
    mutationFn: (op: { fn: () => Promise<unknown> }) => op.fn(),
    onSuccess: invalidate,
  });
  const [jsonErr, setJsonErr] = useState<string | null>(null);

  const submitBatch = () => {
    try {
      const orders = JSON.parse(batchJson) as unknown;
      const arr = Array.isArray(orders) ? orders : (orders as Record<string, unknown>)['orders'];
      if (!Array.isArray(arr) || arr.length === 0) {
        setJsonErr('Body must be {"orders":[…]} or a bare orders array.');
        return;
      }
      setJsonErr(null);
      run.mutate({ fn: () => api.submitOrderBatch(apiClient, arr) });
    } catch {
      setJsonErr('Invalid JSON.');
    }
  };

  const cancelBatch = () => {
    const orderIds = cancelIds
      .split(/[\s,]+/)
      .filter((s) => s !== '')
      .map(Number)
      .filter((n) => Number.isFinite(n));
    const clientIds = cancelClientIds.split(/[\s,]+/).filter((s) => s !== '');
    if (orderIds.length === 0 && clientIds.length === 0) return;
    run.mutate({
      fn: () =>
        api.cancelOrderBatch(apiClient, {
          ...(orderIds.length > 0 ? { order_ids: orderIds } : {}),
          ...(clientIds.length > 0 ? { client_order_ids: clientIds } : {}),
        }),
    });
  };

  return (
    <div className={cardCls}>
      <h2 className="mb-2 text-sm font-semibold">Batch &amp; mass operations</h2>
      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <Field label='Batch submit — {"orders":[…]} or bare array'>
            {(id) => (
              <textarea
                id={id}
                rows={4}
                className={`${inputCls} font-mono text-xs`}
                placeholder='{"orders":[{"symbol":"EUR/USD",…}]}'
                value={batchJson}
                onChange={(e) => {
                  setBatchJson(e.target.value);
                }}
              />
            )}
          </Field>
          {jsonErr !== null ? <p className="mb-1 text-xs text-red-400">{jsonErr}</p> : null}
          <button type="button" className={btnPrimary} onClick={submitBatch}>
            Submit batch
          </button>
        </div>
        <div className="space-y-2">
          <Field label="Batch cancel — order ids">
            {(id) => (
              <input
                id={id}
                className={inputCls}
                placeholder="123, 124"
                value={cancelIds}
                onChange={(e) => {
                  setCancelIds(e.target.value);
                }}
              />
            )}
          </Field>
          <Field label="…or client order ids">
            {(id) => (
              <input
                id={id}
                className={inputCls}
                placeholder="cid-1, cid-2"
                value={cancelClientIds}
                onChange={(e) => {
                  setCancelClientIds(e.target.value);
                }}
              />
            )}
          </Field>
          <button
            type="button"
            className={btnGhost}
            disabled={cancelIds.trim() === '' && cancelClientIds.trim() === ''}
            onClick={cancelBatch}
          >
            Cancel batch
          </button>
          <p className="text-xs text-neutral-500">
            Atomic — one unknown or foreign order id aborts the whole batch.
          </p>
        </div>
        <div className="space-y-2 sm:col-span-2">
          <h3 className="text-xs font-semibold uppercase text-neutral-500">Mass cancel</h3>
          <div className="flex flex-wrap items-end gap-2">
            <Field label="Symbol scope (empty = all open orders)">
              {(id) => (
                <input
                  id={id}
                  className={inputCls}
                  placeholder="EUR/USD"
                  value={symbol}
                  onChange={(e) => {
                    setSymbol(e.target.value.toUpperCase());
                  }}
                />
              )}
            </Field>
            <Field label="Side filter">
              {(id) => (
                <select
                  id={id}
                  className={selectCls}
                  value={massSide}
                  onChange={(e) => {
                    setMassSide(e.target.value);
                  }}
                >
                  <option value="">—</option>
                  {SIDE_OPTIONS.map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            {symbol.trim() === '' ? (
              <button
                type="button"
                className="rounded bg-red-700 px-3 py-1.5 text-sm font-semibold text-white hover:bg-red-600 disabled:opacity-50"
                disabled={run.isPending}
                onClick={() => {
                  if (!confirmAll) {
                    setConfirmAll(true);
                    return;
                  }
                  setConfirmAll(false);
                  run.mutate({ fn: () => api.massCancelAll(apiClient) });
                }}
              >
                {confirmAll ? 'Confirm: cancel ALL open orders' : 'Cancel all open orders'}
              </button>
            ) : (
              <button
                type="button"
                className={btnGhost}
                disabled={run.isPending}
                onClick={() => {
                  run.mutate({
                    fn: () =>
                      api.massCancelScoped(apiClient, {
                        symbol: symbol.trim(),
                        side: massSide !== '' ? massSide : undefined,
                      }),
                  });
                }}
              >
                Cancel {symbol.trim() || '—'} orders
              </button>
            )}
          </div>
          {confirmAll ? (
            <p role="alert" className="text-xs text-red-400">
              This cancels every open order on the account. Click again to confirm.
            </p>
          ) : null}
        </div>
      </div>
      {run.isError ? (
        <div className="mt-2">
          <ErrorBox error={run.error} />
        </div>
      ) : null}
      {run.isSuccess ? (
        <p role="status" className="mt-2 text-xs text-emerald-400">
          Done.
        </p>
      ) : null}
    </div>
  );
}
