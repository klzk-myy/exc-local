/**
 * Advanced order types panel (Task 10.3.7) — one ticket covering the
 * spec §6 taxonomy: LIMIT / MARKET / STOP / STOP_LIMIT / ICEBERG /
 * TRAILING_STOP / BRACKET / OCO with every legal TIF, iceberg display
 * hint, trailing distance (PIPS|PERCENTAGE|ABSOLUTE), trigger source
 * (§6.2a), GTD expiry picker, post_only/reduce_only flags, and the
 * Task 10.3.11 percentage slider.
 *
 * Safety contract:
 *   - order entry locks outside AUTHENTICATED/STALE (Task 10.3.19) —
 *     the submit button is disabled and explains why;
 *   - every submission carries `client_order_id` + Idempotency-Key
 *     (spec §8.8);
 *   - field errors surface inline (aria-invalid + describedby);
 *   - MARKET orders disclose that fills are at best available price.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { lazy, Suspense, useEffect, useState, type FormEvent } from 'react';

import { apiClient, wsClient } from '@/app/runtime';
import { useWsStatus, type WsClient } from '@/lib/ws';
import {
  ErrorBox,
  Field,
  Modal,
  btnGhost,
  btnPrimary,
  inputCls,
  labelCls,
  selectCls,
} from '@/lib/ui';
import { newIdempotencyKey } from '@/lib/api';
import { Dec } from '@/lib/decimal/decimal';
import { useAccountScope } from '@/lib/trading/accountScope';
import {
  submitAlgoOrder,
  submitBracketOrder,
  submitOcoOrder,
  submitOrder,
  type AlgoOrderBody,
  type Api,
  type BracketOrderBody,
  type OcoOrderBody,
  type SubmitOrderBody,
} from '@/lib/trading/api';
import { useBalances, useInstrument, useInstruments } from '@/lib/trading/queries';
import { useOrderDraft } from '@/lib/trading/orderDraft';
import { useBbo } from '@/lib/trading/marketStore';
import { qtyConstraints, splitPair } from '@/lib/trading/fx';

import { PercentSlider } from './PercentSlider';
import {
  buildOrderPayload,
  ORDER_KINDS,
  TIF_FOR_KIND,
  TRIGGER_SOURCES,
  EMPTY_FORM,
  type OrderFormState,
  type OrderKind,
  type Tif,
  type TrailingUnit,
} from './orderPayload';

const PositionCalculator = lazy(() =>
  import('@/features/calculator/PositionCalculator').then((m) => ({
    default: m.PositionCalculator,
  })),
);

const KIND_LABEL: Record<OrderKind, string> = {
  LIMIT: 'Limit',
  MARKET: 'Market',
  STOP: 'Stop (market)',
  STOP_LIMIT: 'Stop-limit',
  ICEBERG: 'Iceberg',
  TRAILING_STOP: 'Trailing stop',
  BRACKET: 'Bracket (entry + SL + TP)',
  OCO: 'OCO (one-cancels-other)',
};

export interface AdvancedOrderPanelProps {
  client?: WsClient;
  api?: Api;
}

export function AdvancedOrderPanel({
  client = wsClient,
  api = apiClient,
}: AdvancedOrderPanelProps) {
  const ws = useWsStatus(client);
  const instruments = useInstruments();
  const scope = useAccountScope((s) => s.scopeKey);
  const queryClient = useQueryClient();

  const draft = useOrderDraft((s) => s.draft);
  const setDraft = useOrderDraft((s) => s.setDraft);
  const [form, setForm] = useState<OrderFormState>(() => ({
    ...EMPTY_FORM,
    symbol: draft.symbol,
    price: draft.price,
    quantity: draft.quantity,
  }));

  // Cross-panel prefill: depth-chart / chart-overlay clicks write the
  // draft store; mirror price/qty/symbol into the form when they change.
  useEffect(() => {
    setForm((f) => ({
      ...f,
      symbol: draft.symbol !== '' ? draft.symbol : f.symbol,
      price: draft.price !== '' ? draft.price : f.price,
      quantity: draft.quantity !== '' ? draft.quantity : f.quantity,
    }));
  }, [draft.symbol, draft.price, draft.quantity]);

  const patch = (p: Partial<OrderFormState>) => setForm((f) => ({ ...f, ...p }));

  const instrument = useInstrument(form.symbol || undefined);
  const bbo = useBbo(form.symbol || undefined);
  const markPrice = form.side === 'BUY' ? bbo?.ask : bbo?.bid; // aggressive side for sizing
  const leverage = Dec.of(instrument?.maxLeverage ?? 1);

  // Margin basis: available balance in the pair's QUOTE currency — the
  // disclosed basis for % sizing (disclosed next to the slider label).
  const balances = useBalances();
  const quoteCcy = splitPair(form.symbol)?.quote;
  const freeMargin =
    quoteCcy !== undefined
      ? (balances.data?.find((b) => b.currency === quoteCcy)?.available ?? Dec.ZERO)
      : Dec.ZERO;

  const submit = useMutation({
    mutationFn: async (f: OrderFormState) => {
      const built = buildOrderPayload(f);
      if (Object.keys(built.errors).length > 0) {
        throw new Error('invalid form');
      }
      const clientOrderId = newIdempotencyKey();
      const body = built.body;
      if (body === undefined) throw new Error('invalid form');
      switch (built.endpoint) {
        case 'orders':
          return submitOrder({ ...(body as SubmitOrderBody), client_order_id: clientOrderId }, api);
        case 'bracket':
          return submitBracketOrder(
            { ...(body as BracketOrderBody), client_order_id: clientOrderId },
            api,
          );
        case 'oco':
          return submitOcoOrder({ ...(body as OcoOrderBody), client_order_id: clientOrderId }, api);
        case 'algo':
          return submitAlgoOrder(
            { ...(body as AlgoOrderBody), client_order_id: clientOrderId },
            api,
          );
      }
    },
    onSuccess: async () => {
      setLastResult('Order accepted — awaiting engine ack (watch private:orders).');
      await queryClient.invalidateQueries({ queryKey: ['orders', scope] });
    },
    onError: (e) => {
      setLastResult(null);
      setSubmitError(e);
    },
  });
  const [submitError, setSubmitError] = useState<unknown>(null);
  const [lastResult, setLastResult] = useState<string | null>(null);
  const [calcOpen, setCalcOpen] = useState(false);
  const [touched, setTouched] = useState<Partial<Record<string, true>>>({});
  const [submitTried, setSubmitTried] = useState(false);

  // Default instrument: first ACTIVE listing, once — the placeholder-only
  // value read as "rejected input" next to validation chrome. Never
  // overwrite a field the user has already edited (touched).
  useEffect(() => {
    if (form.symbol !== '' || touched['symbol'] === true) return;
    const first = (instruments.data ?? []).find((i) => i.status === 'ACTIVE');
    if (first !== undefined) {
      setForm((f) => (f.symbol === '' ? { ...f, symbol: first.symbol } : f));
    }
  }, [instruments.data, form.symbol, touched]);

  const built = buildOrderPayload(form);
  const fieldErrors = built.errors;
  // Validation surfaces on blur/submit, not on mount — a pristine ticket
  // should not announce errors for fields the user has not reached yet.
  const showErr = (k: string): string | undefined =>
    touched[k] === true || submitTried ? fieldErrors[k] : undefined;
  const touch = (k: string) => () => setTouched((t) => (t[k] === true ? t : { ...t, [k]: true }));
  const constraints = qtyConstraints(instrument);

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitError(null);
    setLastResult(null);
    setSubmitTried(true);
    if (Object.keys(fieldErrors).length > 0) return; // inline errors shown
    submit.mutate(form);
  };

  const tifs = TIF_FOR_KIND[form.kind];
  const showPrice =
    form.kind === 'LIMIT' ||
    form.kind === 'STOP_LIMIT' ||
    form.kind === 'ICEBERG' ||
    form.kind === 'BRACKET';
  const showStop = form.kind === 'STOP' || form.kind === 'STOP_LIMIT' || form.kind === 'OCO';
  const showTif = tifs.length > 0;
  const conditional = ['STOP', 'STOP_LIMIT', 'TRAILING_STOP', 'BRACKET', 'OCO'].includes(form.kind);

  return (
    <form
      onSubmit={onSubmit}
      aria-label="Advanced order entry"
      className="rounded-lg border border-neutral-800 bg-neutral-900 p-4"
    >
      <div className="mb-3 flex items-center justify-between">
        <h2 className="text-sm font-semibold text-neutral-200">Order ticket</h2>
        <div className="flex items-center gap-2">
          <button
            type="button"
            className={`${btnGhost} py-0.5 text-xs`}
            onClick={() => setCalcOpen(true)}
            aria-label="Open position calculator"
          >
            Calculator
          </button>
          <span
            className={`rounded px-2 py-0.5 text-xs ${ws.orderEntryEnabled ? 'bg-emerald-500/15 text-emerald-400' : 'bg-red-500/15 text-red-400'}`}
            role="status"
          >
            {ws.orderEntryEnabled ? 'order entry live' : `order entry locked (${ws.state})`}
          </span>
        </div>
      </div>

      <Modal
        open={calcOpen}
        title="Position & margin calculator"
        onClose={() => setCalcOpen(false)}
      >
        <Suspense fallback={<p className="p-4 text-sm text-neutral-500">Loading calculator…</p>}>
          <PositionCalculator initialSymbol={form.symbol !== '' ? form.symbol : 'EUR/USD'} />
        </Suspense>
      </Modal>

      {/* Side */}
      <div className="mb-3 grid grid-cols-2 gap-1" role="group" aria-label="Order side">
        {(['BUY', 'SELL'] as const).map((s) => (
          <button
            key={s}
            type="button"
            aria-pressed={form.side === s}
            onClick={() => patch({ side: s })}
            className={`rounded py-1.5 text-sm font-semibold focus-visible:ring-2 focus-visible:ring-sky-500 ${
              form.side === s
                ? s === 'BUY'
                  ? 'bg-emerald-600 text-white'
                  : 'bg-red-600 text-white'
                : 'bg-neutral-800 text-neutral-400 hover:bg-neutral-700'
            }`}
          >
            {s}
          </button>
        ))}
      </div>

      {/* Symbol */}
      <Field label="Instrument" error={showErr('symbol') ?? null} required>
        {(id, describedBy, invalid) => (
          <>
            <input
              id={id}
              list="instrument-symbols"
              value={form.symbol}
              onChange={(e) => {
                patch({ symbol: e.target.value.toUpperCase() });
                setDraft({ symbol: e.target.value.toUpperCase() });
              }}
              onBlur={touch('symbol')}
              placeholder="EUR/USD"
              aria-invalid={invalid}
              aria-describedby={describedBy}
              className={inputCls}
            />
            <datalist id="instrument-symbols">
              {(instruments.data ?? [])
                .filter((i) => i.status === 'ACTIVE')
                .map((i) => (
                  <option key={i.symbol} value={i.symbol} />
                ))}
            </datalist>
          </>
        )}
      </Field>

      {/* Order type */}
      <Field label="Order type">
        {(id) => (
          <select
            id={id}
            value={form.kind}
            onChange={(e) => {
              const kind = e.target.value as OrderKind;
              const allowed = TIF_FOR_KIND[kind];
              patch({
                kind,
                tif: allowed.includes(form.tif) ? form.tif : (allowed[0] ?? 'GTC'),
              });
            }}
            className={selectCls}
          >
            {ORDER_KINDS.map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </select>
        )}
      </Field>

      {/* Price */}
      {showPrice && (
        <Field
          label={form.kind === 'BRACKET' ? 'Entry price (empty = market entry)' : 'Price'}
          error={showErr('price') ?? null}
          required={form.kind !== 'BRACKET'}
          hint={instrument !== undefined ? `tick ${instrument.tickSize.toDisplay()}` : undefined}
        >
          {(id, describedBy, invalid) => (
            <input
              id={id}
              inputMode="decimal"
              value={form.price}
              onChange={(e) => patch({ price: e.target.value })}
              onBlur={touch('price')}
              placeholder="0.00000"
              aria-invalid={invalid}
              aria-describedby={describedBy}
              className={inputCls}
            />
          )}
        </Field>
      )}

      {/* Stop price */}
      {showStop && (
        <Field
          label={form.kind === 'OCO' ? 'Stop leg trigger' : 'Stop trigger price'}
          error={showErr('stopPrice') ?? null}
          required
        >
          {(id, describedBy, invalid) => (
            <input
              id={id}
              inputMode="decimal"
              value={form.stopPrice}
              onChange={(e) => patch({ stopPrice: e.target.value })}
              onBlur={touch('stopPrice')}
              placeholder="0.00000"
              aria-invalid={invalid}
              aria-describedby={describedBy}
              className={inputCls}
            />
          )}
        </Field>
      )}

      {/* Iceberg visible qty */}
      {form.kind === 'ICEBERG' && (
        <Field
          label="Visible quantity (iceberg display hint)"
          error={showErr('visibleQty') ?? null}
          required
          hint="Only this slice shows on the public L2 book"
        >
          {(id, describedBy, invalid) => (
            <input
              id={id}
              inputMode="decimal"
              value={form.visibleQty}
              onChange={(e) => patch({ visibleQty: e.target.value })}
              onBlur={touch('visibleQty')}
              aria-invalid={invalid}
              aria-describedby={describedBy}
              className={inputCls}
            />
          )}
        </Field>
      )}

      {/* Trailing params */}
      {form.kind === 'TRAILING_STOP' && (
        <div className="mb-4 grid grid-cols-2 gap-2">
          <div>
            <label htmlFor="trail-dist" className={labelCls}>
              Trailing distance
            </label>
            <input
              id="trail-dist"
              inputMode="decimal"
              value={form.trailingDistance}
              onChange={(e) => patch({ trailingDistance: e.target.value })}
              onBlur={touch('trailingDistance')}
              aria-invalid={showErr('trailingDistance') !== undefined}
              className={inputCls}
            />
            {showErr('trailingDistance') !== undefined && (
              <p className="mt-1 text-xs text-red-400" role="alert">
                {fieldErrors['trailingDistance']}
              </p>
            )}
          </div>
          <div>
            <label htmlFor="trail-unit" className={labelCls}>
              Distance unit
            </label>
            <select
              id="trail-unit"
              value={form.trailingUnit}
              onChange={(e) => patch({ trailingUnit: e.target.value as TrailingUnit })}
              className={selectCls}
            >
              <option value="PIPS">Pips</option>
              <option value="PERCENTAGE">Percent</option>
              <option value="ABSOLUTE">Absolute price</option>
            </select>
          </div>
        </div>
      )}

      {/* Bracket children */}
      {form.kind === 'BRACKET' && (
        <div className="mb-4 grid grid-cols-2 gap-2">
          <div>
            <label htmlFor="bracket-sl" className={labelCls}>
              Stop-loss price
            </label>
            <input
              id="bracket-sl"
              inputMode="decimal"
              value={form.bracketStop}
              onChange={(e) => patch({ bracketStop: e.target.value })}
              onBlur={touch('bracketStop')}
              aria-invalid={showErr('bracketStop') !== undefined}
              className={inputCls}
            />
          </div>
          <div>
            <label htmlFor="bracket-tp" className={labelCls}>
              Take-profit price
            </label>
            <input
              id="bracket-tp"
              inputMode="decimal"
              value={form.bracketTarget}
              onChange={(e) => patch({ bracketTarget: e.target.value })}
              onBlur={touch('bracketTarget')}
              aria-invalid={showErr('bracketTarget') !== undefined}
              className={inputCls}
            />
          </div>
          {(showErr('bracketStop') !== undefined || showErr('bracketTarget') !== undefined) && (
            <p className="col-span-2 text-xs text-red-400" role="alert">
              {showErr('bracketStop') ?? showErr('bracketTarget')}
            </p>
          )}
        </div>
      )}

      {/* OCO limit leg */}
      {form.kind === 'OCO' && (
        <Field label="Limit leg price" error={showErr('ocoLimit') ?? null} required>
          {(id, describedBy, invalid) => (
            <input
              id={id}
              inputMode="decimal"
              value={form.ocoLimit}
              onChange={(e) => patch({ ocoLimit: e.target.value })}
              onBlur={touch('ocoLimit')}
              aria-invalid={invalid}
              aria-describedby={describedBy}
              className={inputCls}
            />
          )}
        </Field>
      )}

      {/* TIF + GTD */}
      {showTif && (
        <div className="mb-4 grid grid-cols-2 gap-2">
          <div>
            <label htmlFor="tif" className={labelCls}>
              Time in force
            </label>
            <select
              id="tif"
              value={form.tif}
              onChange={(e) => patch({ tif: e.target.value as Tif })}
              className={selectCls}
            >
              {tifs.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          </div>
          {form.tif === 'GTD' && (
            <div>
              <label htmlFor="gtd" className={labelCls}>
                GTD expiry (UTC)
              </label>
              <input
                id="gtd"
                type="datetime-local"
                value={form.gtdExpiry}
                onChange={(e) => patch({ gtdExpiry: e.target.value })}
                onBlur={touch('gtdExpiry')}
                aria-invalid={showErr('gtdExpiry') !== undefined}
                className={inputCls}
              />
              {showErr('gtdExpiry') !== undefined && (
                <p className="mt-1 text-xs text-red-400" role="alert">
                  {fieldErrors['gtdExpiry']}
                </p>
              )}
            </div>
          )}
        </div>
      )}

      {/* Quantity + percent slider */}
      <Field
        label="Quantity"
        error={showErr('quantity') ?? null}
        required
        hint={
          constraints.step.isPositive()
            ? `lot step ${constraints.step.toDisplay()}${
                constraints.minQty.isPositive() ? ` · min ${constraints.minQty.toDisplay()}` : ''
              }`
            : undefined
        }
      >
        {(id, describedBy, invalid) => (
          <input
            id={id}
            inputMode="decimal"
            value={form.quantity}
            onChange={(e) => {
              patch({ quantity: e.target.value });
              setDraft({ quantity: e.target.value });
            }}
            onBlur={touch('quantity')}
            aria-invalid={invalid}
            aria-describedby={describedBy}
            className={inputCls}
          />
        )}
      </Field>

      <div className="mb-4">
        <PercentSlider
          instrument={instrument}
          price={markPrice}
          freeMargin={freeMargin}
          leverage={leverage}
          onSize={(qty) => {
            patch({ quantity: qty });
            setDraft({ quantity: qty });
          }}
          disabled={!ws.orderEntryEnabled}
        />
      </div>

      {/* Trigger source + flags */}
      {conditional && (
        <div className="mb-4">
          <label htmlFor="trigger-source" className={labelCls}>
            Trigger source (§6.2a)
          </label>
          <select
            id="trigger-source"
            value={form.triggerSource}
            onChange={(e) =>
              patch({ triggerSource: e.target.value as OrderFormState['triggerSource'] })
            }
            className={selectCls}
          >
            {TRIGGER_SOURCES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </div>
      )}

      <div className="mb-4 flex gap-4">
        <label className="flex items-center gap-2 text-sm text-neutral-300">
          <input
            type="checkbox"
            checked={form.postOnly}
            onChange={(e) => patch({ postOnly: e.target.checked })}
            className="accent-sky-500"
          />
          Post-only
        </label>
        <label className="flex items-center gap-2 text-sm text-neutral-300">
          <input
            type="checkbox"
            checked={form.reduceOnly}
            onChange={(e) => patch({ reduceOnly: e.target.checked })}
            className="accent-sky-500"
          />
          Reduce-only
        </label>
      </div>

      {form.kind === 'MARKET' && (
        <p className="mb-3 rounded border border-amber-700/50 bg-amber-950/30 px-2 py-1.5 text-xs text-amber-300">
          Market orders fill at best available price — slippage collars apply server-side (§6.6a).
        </p>
      )}

      <ErrorBox error={submitError} onDismiss={() => setSubmitError(null)} />
      <div aria-live="polite">
        {lastResult !== null && (
          <p className="mb-3 rounded border border-emerald-700/50 bg-emerald-950/30 px-2 py-1.5 text-xs text-emerald-300">
            {lastResult}
          </p>
        )}
      </div>

      <button
        type="submit"
        disabled={!ws.orderEntryEnabled || submit.isPending}
        className={`${btnPrimary} w-full`}
      >
        {submit.isPending ? 'Submitting…' : `Submit ${KIND_LABEL[form.kind]}`}
      </button>
    </form>
  );
}
