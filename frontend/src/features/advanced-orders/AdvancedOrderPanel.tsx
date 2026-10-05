/**
 * Advanced order types panel (Task 10.3.7) — one ticket covering the
 * spec §6 taxonomy: LIMIT / MARKET / STOP / STOP_LIMIT / ICEBERG /
 * TRAILING_STOP / BRACKET / OCO with every legal TIF, iceberg display
 * hint, trailing distance (PIPS|PERCENTAGE|ABSOLUTE), trigger source
 * (§6.2a), GTD expiry picker, post_only/reduce_only flags, and the
 * Task 10.3.11 percentage slider.
 *
 * Layout mirrors the classic spot ticket (Binance-style): a Buy column
 * and a Sell column rendered side by side, each carrying its own
 * price/quantity slice, per-side availability, percent slider, and a
 * coloured submit button. Kind-specific parameters (stop trigger,
 * iceberg slice, trailing distance, bracket legs, TIF/GTD, trigger
 * source, flags) are side-agnostic and stay shared above the two
 * columns. OCO keeps a single-column form — its legs already encode
 * both prices and the side applies to the pair as a unit.
 *
 * Safety contract:
 *   - order entry locks outside AUTHENTICATED/STALE (Task 10.3.19) —
 *     the submit buttons are disabled and explain why;
 *   - every submission carries `client_order_id` + Idempotency-Key
 *     (spec §8.8);
 *   - field errors surface inline per column (aria-invalid +
 *     describedby);
 *   - MARKET orders disclose that fills are at best available price.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { lazy, Suspense, useEffect, useRef, useState, type FormEvent } from 'react';

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
  useNow,
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
import { useWatchlist } from '@/lib/alerts';
import { useSessionStore } from '@/lib/auth/session';
import { isFxMarketOpen } from '@/lib/market/tradingHours';
import { useBbo } from '@/lib/trading/marketStore';
import { parseInput, qtyConstraints, splitPair } from '@/lib/trading/fx';

import { PercentSlider } from './PercentSlider';
import {
  buildOrderPayload,
  ORDER_KINDS,
  TIF_FOR_KIND,
  TRIGGER_SOURCES,
  EMPTY_FORM,
  type FieldErrors,
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

type Side = 'BUY' | 'SELL';

/** Per-column ticket slice — the only fields that differ between the
 * Binance-style Buy and Sell columns. All kind-specific parameters are
 * shared (a stop trigger or iceberg slice applies regardless of which
 * side submits). */
interface SideSlice {
  price: string;
  quantity: string;
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
  const accountId = useSessionStore((s) => s.user?.accountId ?? null);
  const watchlist = useWatchlist(accountId);
  const marketOpen = isFxMarketOpen(useNow(30_000));
  const [form, setForm] = useState<OrderFormState>(() => ({
    ...EMPTY_FORM,
    symbol: draft.symbol,
    price: draft.price,
    quantity: draft.quantity,
  }));
  const [sides, setSides] = useState<Record<Side, SideSlice>>(() => ({
    BUY: { price: draft.price, quantity: draft.quantity },
    SELL: { price: '', quantity: '' },
  }));
  /** Which column's button triggered the in-flight form submission —
   * resolved in the button's click handler so onSubmit never guesses. */
  const pendingSide = useRef<Side>('BUY');

  // Cross-panel prefill: depth-chart / chart-overlay clicks write the
  // draft store; mirror symbol into the form and price/qty into the
  // column named by draft.side (book ask click → Buy column, bid →
  // Sell column). `form.price`/`form.quantity` also mirror the draft so
  // the single-column OCO path prefills identically.
  useEffect(() => {
    const tgt: Side = draft.side === 'SELL' ? 'SELL' : 'BUY';
    setForm((f) => ({
      ...f,
      symbol: draft.symbol !== '' ? draft.symbol : f.symbol,
      price: draft.price !== '' ? draft.price : f.price,
      quantity: draft.quantity !== '' ? draft.quantity : f.quantity,
    }));
    setSides((s) => ({
      ...s,
      [tgt]: {
        price: draft.price !== '' ? draft.price : s[tgt].price,
        quantity: draft.quantity !== '' ? draft.quantity : s[tgt].quantity,
      },
    }));
  }, [draft.symbol, draft.price, draft.quantity, draft.side]);

  const patch = (p: Partial<OrderFormState>) => setForm((f) => ({ ...f, ...p }));
  const patchSide = (side: Side, p: Partial<SideSlice>) =>
    setSides((s) => ({ ...s, [side]: { ...s[side], ...p } }));

  const instrument = useInstrument(form.symbol || undefined);
  const bbo = useBbo(form.symbol || undefined);
  const leverage = Dec.of(instrument?.maxLeverage ?? 1);

  // Availability disclosure per column: Buy spends the pair's QUOTE
  // currency, Sell spends the BASE currency the account already holds.
  const balances = useBalances();
  const pair = splitPair(form.symbol);
  const baseCcy = pair?.base;
  const quoteCcy = pair?.quote;
  const quoteAvail =
    quoteCcy !== undefined
      ? (balances.data?.find((b) => b.currency === quoteCcy)?.available ?? Dec.ZERO)
      : Dec.ZERO;
  const baseAvail =
    baseCcy !== undefined
      ? (balances.data?.find((b) => b.currency === baseCcy)?.available ?? Dec.ZERO)
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
      setLastResult('Order sent — tracking under Open orders.');
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
  /** Column whose submit was attempted — a failed Buy never flags the
   * untouched Sell column (and vice versa). */
  const [triedSide, setTriedSide] = useState<Side | null>(null);

  // Default instrument: first ACTIVE listing, once — the placeholder-only
  // value read as "rejected input" next to validation chrome. Never
  // overwrite a field the user has already edited (touched). The pick is
  // also written into the shared draft so the cockpit's symbol context
  // (ticker strip, book, chart, tape) agrees with the ticket — an empty
  // draft must not leave the form trading a different pair than every
  // panel is charting.
  useEffect(() => {
    if (form.symbol !== '' || touched['symbol'] === true) return;
    const actives = (instruments.data ?? []).filter((i) => i.status === 'ACTIVE');
    // Default order: watchlist head → the venue's benchmark pair →
    // first active. Alphabetical-first would strand every fresh session
    // on an arbitrary (often illiquid) pair.
    const pick =
      actives.find((i) => i.symbol === watchlist[0]) ??
      actives.find((i) => i.symbol === 'EUR/USD') ??
      actives[0];
    if (pick !== undefined) {
      setForm((f) => (f.symbol === '' ? { ...f, symbol: pick.symbol } : f));
      setDraft({ symbol: pick.symbol });
    }
  }, [instruments.data, watchlist, form.symbol, touched, setDraft]);

  /** OCO carries its own two legs — it stays a single-column form. Every
   * other kind is directional and gets the dual Buy|Sell columns. */
  const dual = form.kind !== 'OCO';

  /** Merge the shared form with one column's price/quantity slice. */
  const sideForm = (s: Side): OrderFormState => ({
    ...form,
    side: s,
    price: sides[s].price,
    quantity: sides[s].quantity,
  });

  // One builder run per column keeps each column's inline errors
  // independent (typing a bad price in Buy never flags Sell).
  const builtByColumn: Record<Side, ReturnType<typeof buildOrderPayload>> = {
    BUY: buildOrderPayload(sideForm('BUY')),
    SELL: buildOrderPayload(sideForm('SELL')),
  };
  const built = buildOrderPayload(form); // OCO single-column path
  // Shared-field errors (stop trigger, iceberg slice, bracket legs, GTD,
  // symbol) merge both columns — a side-dependent rule like "buy bracket:
  // SL below TP" must surface even when only the Sell column trips it.
  // price/quantity keys stay per-column via colErr.
  const fieldErrors: FieldErrors = dual
    ? { ...builtByColumn.SELL.errors, ...builtByColumn.BUY.errors }
    : built.errors;
  // Validation surfaces on blur/submit, not on mount — a pristine ticket
  // should not announce errors for fields the user has not reached yet.
  const showErr = (k: string): string | undefined =>
    touched[k] === true || submitTried || triedSide !== null ? fieldErrors[k] : undefined;
  const touch = (k: string) => () => setTouched((t) => (t[k] === true ? t : { ...t, [k]: true }));
  const constraints = qtyConstraints(instrument);
  const qtyHint = constraints.step.isPositive()
    ? `lot step ${constraints.step.toDisplay()}${
        constraints.minQty.isPositive() ? ` · min ${constraints.minQty.toDisplay()}` : ''
      }`
    : undefined;

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitError(null);
    setLastResult(null);
    const s = pendingSide.current;
    if (dual) setTriedSide(s);
    else setSubmitTried(true);
    const f = dual ? sideForm(s) : form;
    const errs = dual ? builtByColumn[s].errors : fieldErrors;
    if (Object.keys(errs).length > 0) return; // inline errors shown
    submit.mutate(f);
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

  /** One Binance-style side column: availability, price (or a greyed
   * "Market" row for MARKET), quantity, % slider, estimated total, and
   * the coloured submit. */
  const renderColumn = (s: Side) => {
    const isBuy = s === 'BUY';
    const slice = sides[s];
    const col = builtByColumn[s];
    const colErr = (k: keyof FieldErrors): string | undefined =>
      touched[`${s}.${k}`] === true || triedSide === s ? col.errors[k] : undefined;
    const touchCol = (k: string) => touch(`${s}.${k}`);
    // Aggressive-side BBO for sizing + total estimate.
    const mark = isBuy ? bbo?.ask : bbo?.bid;
    // Sell-side slider sizes off BASE holdings converted at bid so the
    // usual margin→qty formula yields exactly avail×pct.
    const sliderMargin = isBuy
      ? quoteAvail
      : mark?.isPositive() === true
        ? baseAvail.mul(mark)
        : Dec.ZERO;
    const sliderLeverage = isBuy ? leverage : Dec.ONE;
    const avail = isBuy ? quoteAvail : baseAvail;
    const availCcy = isBuy ? quoteCcy : baseCcy;
    const qty = parseInput(slice.quantity);
    const effPrice = parseInput(slice.price) ?? mark;
    const total = qty !== undefined && effPrice !== undefined ? qty.mul(effPrice) : undefined;
    return (
      <div
        key={s}
        aria-label={`${isBuy ? 'Buy' : 'Sell'} ticket`}
        className={`rounded border p-2 ${isBuy ? 'border-emerald-800/60' : 'border-red-800/60'}`}
      >
        <div className="mb-2 flex items-baseline justify-between text-xs">
          <span className="text-neutral-500">Avail</span>
          <span className="font-medium text-neutral-300">
            {availCcy !== undefined ? `${avail.toDisplay(2)} ${availCcy}` : '—'}
          </span>
        </div>

        {showPrice ? (
          <Field
            label={form.kind === 'BRACKET' ? 'Entry price (empty = market entry)' : 'Price'}
            error={colErr('price') ?? null}
            required={form.kind !== 'BRACKET'}
            hint={instrument !== undefined ? `tick ${instrument.tickSize.toDisplay()}` : undefined}
          >
            {(id, describedBy, invalid) => (
              <input
                id={id}
                inputMode="decimal"
                value={slice.price}
                onChange={(e) => patchSide(s, { price: e.target.value })}
                onBlur={touchCol('price')}
                placeholder="0.00000"
                aria-invalid={invalid}
                aria-describedby={describedBy}
                className={inputCls}
              />
            )}
          </Field>
        ) : (
          form.kind === 'MARKET' && (
            <div className="mb-4">
              <span className={labelCls}>Price</span>
              <input value="Market" disabled readOnly className={`${inputCls} opacity-60`} />
            </div>
          )
        )}

        <Field label="Quantity" error={colErr('quantity') ?? null} required hint={qtyHint}>
          {(id, describedBy, invalid) => (
            <input
              id={id}
              inputMode="decimal"
              value={slice.quantity}
              onChange={(e) => {
                patchSide(s, { quantity: e.target.value });
                setDraft({ quantity: e.target.value, side: s });
              }}
              onBlur={touchCol('quantity')}
              aria-invalid={invalid}
              aria-describedby={describedBy}
              className={inputCls}
            />
          )}
        </Field>

        <div className="mb-3">
          <PercentSlider
            instrument={instrument}
            price={mark}
            freeMargin={sliderMargin}
            leverage={sliderLeverage}
            onSize={(q) => {
              patchSide(s, { quantity: q });
              setDraft({ quantity: q, side: s });
            }}
            disabled={!ws.orderEntryEnabled}
          />
        </div>

        <div className="mb-3 flex items-baseline justify-between text-xs">
          <span className="text-neutral-500">Total</span>
          <span className="font-medium text-neutral-300">
            {total !== undefined && quoteCcy !== undefined
              ? `≈ ${total.toDisplay(2)} ${quoteCcy}`
              : '—'}
          </span>
        </div>

        <button
          type="submit"
          disabled={!ws.orderEntryEnabled || submit.isPending}
          onClick={() => {
            pendingSide.current = s;
          }}
          className={`w-full rounded py-1.5 text-sm font-semibold text-white focus-visible:ring-2 focus-visible:ring-sky-500 disabled:cursor-not-allowed disabled:opacity-50 ${
            isBuy ? 'bg-emerald-700 hover:bg-emerald-600' : 'bg-red-700 hover:bg-red-600'
          }`}
        >
          {submit.isPending && pendingSide.current === s
            ? 'Submitting…'
            : `${isBuy ? 'Buy' : 'Sell'} ${baseCcy ?? ''}`}
        </button>
      </div>
    );
  };

  return (
    <form onSubmit={onSubmit} aria-label="Advanced order entry" className="p-1">
      <div className="mb-3 flex items-center justify-end">
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

      {/* Symbol + order type */}
      <div className="grid grid-cols-2 gap-2">
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
      </div>

      {/* OCO stays single-column — its legs encode both prices and the
          side applies to the pair as a unit. */}
      {!dual && (
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
                    ? 'bg-emerald-700 text-white'
                    : 'bg-red-700 text-white'
                  : 'bg-neutral-800 text-neutral-400 hover:bg-neutral-700'
              }`}
            >
              {s}
            </button>
          ))}
        </div>
      )}

      {/* Shared kind-specific parameters — a stop trigger, iceberg slice,
          or trailing distance applies to whichever column submits. */}

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

      {/* OCO single-column quantity + slider */}
      {!dual && (
        <>
          <Field label="Quantity" error={showErr('quantity') ?? null} required hint={qtyHint}>
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
              price={form.side === 'BUY' ? bbo?.ask : bbo?.bid}
              freeMargin={quoteAvail}
              leverage={leverage}
              onSize={(qty) => {
                patch({ quantity: qty });
                setDraft({ quantity: qty });
              }}
              disabled={!ws.orderEntryEnabled}
            />
          </div>
        </>
      )}

      {/* Trigger source + flags */}
      {conditional && (
        <div className="mb-4">
          <label htmlFor="trigger-source" className={labelCls}>
            Trigger source
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
            className="h-6 w-6 accent-sky-500"
          />
          Post-only
        </label>
        <label className="flex items-center gap-2 text-sm text-neutral-300">
          <input
            type="checkbox"
            checked={form.reduceOnly}
            onChange={(e) => patch({ reduceOnly: e.target.checked })}
            className="h-6 w-6 accent-sky-500"
          />
          Reduce-only
        </label>
      </div>

      {form.kind === 'MARKET' && (
        <p className="mb-3 rounded border border-amber-700/50 bg-amber-950/30 px-2 py-1.5 text-xs text-amber-300">
          Market orders fill at the best available price; slippage limits apply.
        </p>
      )}

      {!marketOpen && (
        <p
          role="status"
          className="mb-3 rounded border border-amber-700/50 bg-amber-950/30 px-2 py-1.5 text-xs text-amber-300"
        >
          FX market is closed (24/5). New orders are rejected until the Sunday 21:00 UTC open —
          cancels still go through.
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

      {dual ? (
        <div className="grid grid-cols-2 gap-2" role="group" aria-label="Order sides">
          {renderColumn('BUY')}
          {renderColumn('SELL')}
        </div>
      ) : (
        <button
          type="submit"
          disabled={!ws.orderEntryEnabled || submit.isPending}
          className={`${btnPrimary} w-full`}
        >
          {submit.isPending ? 'Submitting…' : `Submit ${KIND_LABEL[form.kind]}`}
        </button>
      )}
    </form>
  );
}
