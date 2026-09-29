/**
 * OrderEntry — limit/market ticket (Task 10.3.3 + 10.3.19 residual).
 *
 *   - Fields: side, type (LIMIT|MARKET), qty, price, TIF (GTC/DAY/GTD/IOC/
 *     FOK), GTD expiry, editable client_order_id
 *   - Validation: validation.ts (qty>0 + lot multiple, price tick/bounds,
 *     min notional, GTD expiry) — inline per-field errors
 *   - Review → POST /orders/test preview (binding=false estimate) →
 *     Confirm → POST /orders with `idempotent: true` (§8.8)
 *   - Optimistic: the order lands in `usePendingOrders` on submit and is
 *     resolved by REST ack / private:orders lifecycle frame / TTL timeout
 *     / re-auth flush — every rollback emits a notice with the §23 code
 *     and RFC 7807 request_id (spec §8.7)
 *   - Order-entry lock honors the Task 10.3.19 state machine: enabled in
 *     AUTHENTICATED/STALE, locked otherwise (orderEntryEnabled)
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import { apiClient, wsClient } from '@/app/runtime';
import { ApiError, NetworkError, type ApiClient } from '@/lib/api';
import { useSessionStore } from '@/lib/auth/session';
import { previewOrder, submitOrder } from '@/lib/market/api';
import { PRIVATE_CHANNELS } from '@/lib/market/channels';
import { formatPrice, formatQty, formatCurrency } from '@/lib/market/format';
import { useInstruments } from '@/lib/market/hooks';
import { usePendingOrders } from '@/lib/market/pending';
import {
  parsePrivateOrderEvent,
  type OrderPreview,
  type OrderSide,
  type TimeInForce,
} from '@/lib/market/wire';
import { useChannel, useWsStatus, type WsClient } from '@/lib/ws';

import {
  buildRequest,
  hasErrors,
  newClientOrderId,
  TIME_IN_FORCE_OPTIONS,
  validateDraft,
  type DraftErrors,
  type OrderDraft,
} from './validation';

/** Optimistic entries roll back if still unconfirmed after this TTL —
 * the private:orders ack normally lands in ms; 15s covers slow acks. */
export const PENDING_TTL_MS = 15_000;

/** §23 code → actionable hint. Everything not listed renders the server's
 * RFC 7807 message verbatim. */
const ERROR_HINTS: Readonly<Record<string, string>> = {
  PRICE_OUT_OF_RANGE: 'Price is outside the allowed range for this instrument.',
  PRICE_OUT_OF_BAND: 'Price is outside the instrument’s trading band — check the book.',
  EXECUTION_RULE_PRICE_RANGE_EXCEEDED: 'Price is outside the allowed range for this instrument.',
  INSUFFICIENT_BALANCE: 'Insufficient available balance — reduce size or free margin.',
  MARKET_SLIPPAGE_EXCEEDED: 'Market moved beyond the slippage collar — review and resubmit.',
  FOK_NOT_FILLABLE: 'Fill-or-kill cannot be filled in full at current liquidity.',
  IOC_PARTIALLY_FILLED_REMAINDER_CANCELED: 'Partially filled; the remainder was cancelled.',
  ORDER_REJECTED_NO_LIQUIDITY: 'No liquidity available at that price right now.',
  IDEMPOTENCY_KEY_COLLISION: 'Duplicate submission detected — the order was not double-sent.',
  STALE_MODIFY: 'The book changed under this request — re-check the market and retry.',
};

function errorText(code: string | undefined, message: string): string {
  const hint = code !== undefined ? ERROR_HINTS[code] : undefined;
  return hint ?? (message !== '' ? message : 'Order rejected');
}

interface FieldProps {
  label: string;
  error?: string;
  children: React.ReactNode;
}
function Field({ label, error, children }: FieldProps) {
  return (
    <label className="block">
      <span className="mb-1 block text-xs font-medium text-neutral-400">{label}</span>
      {children}
      {error !== undefined && (
        <span role="alert" className="mt-1 block text-xs text-red-400">
          {error}
        </span>
      )}
    </label>
  );
}

const inputCls =
  'w-full rounded border border-neutral-700 bg-neutral-950 px-2 py-1.5 font-mono text-sm text-neutral-100 focus:border-sky-500 focus:outline-none';

export interface OrderEntryProps {
  symbol: string;
  api?: ApiClient;
  ws?: WsClient;
  /** Book-click prefill — `tick` forces re-application on repeated clicks
   * of the same price. Clicking an ask presets BUY, a bid SELL. */
  prefill?: { price: string; from: 'bid' | 'ask'; tick: number } | null;
  /** Pending TTL override for tests. */
  pendingTtlMs?: number;
}

export function OrderEntry(props: OrderEntryProps) {
  const api = props.api ?? apiClient;
  const ws = props.ws ?? wsClient;
  const ttl = props.pendingTtlMs ?? PENDING_TTL_MS;

  const { bySymbol } = useInstruments(api);
  const inst = bySymbol.get(props.symbol);
  const status = useWsStatus(ws);
  const epoch = useSessionStore((s) => s.authEpoch);
  const pending = usePendingOrders((s) => s.pending);
  const notices = usePendingOrders((s) => s.notices);
  const dismissNotice = usePendingOrders((s) => s.dismissNotice);

  const [side, setSide] = useState<OrderSide>('BUY');
  const [type, setType] = useState<'LIMIT' | 'MARKET'>('LIMIT');
  const [price, setPrice] = useState('');
  const [quantity, setQuantity] = useState('');
  const [tif, setTif] = useState<TimeInForce>('GTC');
  const [gtdExpiry, setGtdExpiry] = useState('');
  const [clientOrderId, setClientOrderId] = useState(() => newClientOrderId());
  const [errors, setErrors] = useState<DraftErrors>({});
  const [submitError, setSubmitError] = useState<{
    code?: string;
    message: string;
    requestId?: string;
  } | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [preview, setPreview] = useState<
    { state: 'loading' } | { state: 'ok'; p: OrderPreview } | { state: 'err'; message: string }
  >({ state: 'loading' });
  const [submitting, setSubmitting] = useState(false);
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>());

  const draft: OrderDraft = {
    symbol: props.symbol,
    side,
    type,
    quantity,
    price,
    timeInForce: tif,
    gtdExpiry,
  };

  // -- optimistic-resolution plumbing --------------------------------------

  const armTimeout = useCallback(
    (cid: string) => {
      const t = setTimeout(() => {
        timers.current.delete(cid);
        if (usePendingOrders.getState().pending.some((p) => p.clientOrderId === cid)) {
          usePendingOrders.getState().timeout(cid);
        }
      }, ttl);
      timers.current.set(cid, t);
    },
    [ttl],
  );

  const clearTimer = useCallback((cid: string) => {
    const t = timers.current.get(cid);
    if (t) clearTimeout(t);
    timers.current.delete(cid);
  }, []);

  // Task 10.3.19 invariant — no optimistic order survives re-auth. The ws
  // client bumps session.authEpoch on flush-optimistic; entries keyed to
  // an older epoch are dropped + noticed here.
  useEffect(() => {
    usePendingOrders.getState().flushEpoch(epoch);
  }, [epoch]);

  // private:orders lifecycle → resolve optimistic entries by client_order_id.
  useChannel(ws, PRIVATE_CHANNELS.orders, (frame) => {
    const ev = parsePrivateOrderEvent(frame.data);
    if (!ev?.clientOrderId) return;
    const cid = ev.clientOrderId;
    switch (ev.event) {
      case 'orderAck':
        clearTimer(cid);
        usePendingOrders.getState().confirm(cid, `Order accepted (${ev.orderId})`);
        break;
      case 'orderFill':
        clearTimer(cid);
        usePendingOrders.getState().confirm(cid, 'Order filled');
        break;
      case 'orderReject': {
        clearTimer(cid);
        const reason = ev.reason ?? 'Order rejected';
        const code = /^[A-Z0-9_]+$/.test(reason) ? reason : undefined;
        usePendingOrders.getState().reject(cid, { message: reason, code });
        break;
      }
      default:
        break;
    }
  });

  // Book-click prefill (Task 10.3.3 item 4).
  useEffect(() => {
    if (!props.prefill) return;
    setPrice(props.prefill.price);
    setSide(props.prefill.from === 'ask' ? 'BUY' : 'SELL');
    setType('LIMIT');
  }, [props.prefill]);

  // -- submit flow ----------------------------------------------------------

  function onReview(): void {
    const errs = validateDraft(draft, inst);
    setErrors(errs);
    setSubmitError(null);
    if (hasErrors(errs)) return;
    setConfirming(true);
    setPreview({ state: 'loading' });
    const req = buildRequest(draft, clientOrderId);
    previewOrder(api, req)
      .then((p) => setPreview({ state: 'ok', p }))
      .catch((e: unknown) =>
        setPreview({
          state: 'err',
          message:
            e instanceof ApiError
              ? `${errorText(e.code, e.message)} (${e.code})`
              : 'Preview unavailable — you may still submit.',
        }),
      );
  }

  async function onConfirm(): Promise<void> {
    setSubmitting(true);
    setSubmitError(null);
    const cid = clientOrderId;
    const req = buildRequest(draft, cid);
    // Optimistic render — PENDING lands before the wire resolves.
    usePendingOrders.getState().add({
      clientOrderId: cid,
      symbol: draft.symbol,
      side: draft.side,
      type: draft.type,
      quantity: req.quantity ?? '0',
      price: req.price,
      epoch,
    });
    armTimeout(cid);
    try {
      const ack = await submitOrder(api, req);
      clearTimer(cid);
      usePendingOrders.getState().confirm(cid, `Order accepted (id ${ack.orderId})`);
      setConfirming(false);
      setClientOrderId(newClientOrderId()); // fresh dedup key for the next ticket
    } catch (e) {
      clearTimer(cid);
      if (e instanceof ApiError) {
        usePendingOrders.getState().reject(cid, {
          message: errorText(e.code, e.message),
          code: e.code,
          requestId: e.requestId,
        });
        setSubmitError({
          code: e.code,
          message: errorText(e.code, e.message),
          requestId: e.requestId,
        });
      } else if (e instanceof NetworkError) {
        // The order may or may not have reached the server — roll the
        // optimistic entry back with an explicit "unconfirmed" notice;
        // resubmitting is safe via the same client_order_id dedup.
        usePendingOrders.getState().reject(cid, {
          message: `Submission unconfirmed (network: ${e.message}) — rolled back; safe to retry`,
          code: e.code,
        });
        setSubmitError({
          code: e.code,
          message: 'Network error — order unconfirmed. Safe to retry.',
        });
      } else {
        usePendingOrders.getState().reject(cid, { message: 'Order rejected — rolled back' });
        setSubmitError({ message: 'Order rejected — rolled back' });
      }
    } finally {
      setSubmitting(false);
    }
  }

  const locked = !status.orderEntryEnabled;
  const lockReason =
    status.state === 'STALE' || status.state === 'AUTHENTICATED'
      ? null
      : `Order entry locked — connection ${status.state.toLowerCase()}`;

  const myPending = pending.filter((p) => p.symbol === props.symbol);

  return (
    <section
      aria-label={`Order entry ${props.symbol}`}
      className="w-full rounded-lg border border-neutral-800 bg-neutral-900"
    >
      <header className="flex items-center justify-between border-b border-neutral-800 px-3 py-2">
        <h2 className="text-sm font-semibold">Order Entry</h2>
        <span className="text-xs text-neutral-400">{props.symbol}</span>
      </header>

      <div className="space-y-3 p-3">
        {/* side toggle */}
        <div className="grid grid-cols-2 gap-1" role="group" aria-label="Side">
          {(['BUY', 'SELL'] as const).map((s) => (
            <button
              key={s}
              type="button"
              aria-pressed={side === s}
              onClick={() => setSide(s)}
              className={`rounded py-1.5 text-sm font-semibold ${
                side === s
                  ? s === 'BUY'
                    ? 'bg-emerald-600 text-white'
                    : 'bg-red-600 text-white'
                  : 'bg-neutral-800 text-neutral-400 hover:text-neutral-200'
              }`}
            >
              {s}
            </button>
          ))}
        </div>

        {/* type toggle */}
        <div className="grid grid-cols-2 gap-1" role="group" aria-label="Order type">
          {(['LIMIT', 'MARKET'] as const).map((t) => (
            <button
              key={t}
              type="button"
              aria-pressed={type === t}
              onClick={() => setType(t)}
              className={`rounded px-2 py-1 text-xs font-medium ${
                type === t
                  ? 'bg-neutral-700 text-white'
                  : 'text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200'
              }`}
            >
              {t === 'LIMIT' ? 'Limit' : 'Market'}
            </button>
          ))}
        </div>

        {type === 'LIMIT' && (
          <Field label={inst ? `Price (${inst.quoteCurrency})` : 'Price'} error={errors.price}>
            <input
              aria-label="Price"
              className={inputCls}
              inputMode="decimal"
              value={price}
              onChange={(e) => setPrice(e.target.value)}
              placeholder={inst ? `tick ${inst.tickSize}` : '0.00000'}
            />
          </Field>
        )}

        <Field
          label={inst ? `Quantity (${inst.baseCurrency})` : 'Quantity'}
          error={errors.quantity}
        >
          <input
            aria-label="Quantity"
            className={inputCls}
            inputMode="decimal"
            value={quantity}
            onChange={(e) => setQuantity(e.target.value)}
            placeholder={inst ? `lot ${inst.lotSize}` : '0'}
          />
        </Field>

        <Field label="Time in force">
          <select
            aria-label="Time in force"
            className={inputCls}
            value={tif}
            onChange={(e) => setTif(e.target.value as TimeInForce)}
          >
            {TIME_IN_FORCE_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </Field>

        {tif === 'GTD' && (
          <Field label="Expiry (GTD)" error={errors.gtdExpiry}>
            <input
              aria-label="GTD expiry"
              type="datetime-local"
              className={inputCls}
              value={gtdExpiry}
              onChange={(e) => setGtdExpiry(e.target.value)}
            />
          </Field>
        )}

        <Field label="Client order ID">
          <input
            aria-label="Client order ID"
            className={`${inputCls} text-neutral-400`}
            value={clientOrderId}
            onChange={(e) => setClientOrderId(e.target.value)}
            spellCheck={false}
          />
        </Field>

        {errors.notional !== undefined && (
          <p role="alert" className="text-xs text-red-400">
            {errors.notional}
          </p>
        )}
        {submitError !== null && (
          <p
            role="alert"
            className="rounded border border-red-900 bg-red-950/40 px-2 py-1.5 text-xs text-red-300"
          >
            {submitError.message}
            {submitError.code !== undefined && (
              <span className="ml-1 font-mono text-red-400">{submitError.code}</span>
            )}
            {submitError.requestId !== undefined && (
              <span className="ml-1 font-mono text-neutral-500">req {submitError.requestId}</span>
            )}
          </p>
        )}
        {lockReason !== null && (
          <p role="status" className="text-xs text-amber-400">
            {lockReason}
          </p>
        )}

        <button
          type="button"
          disabled={locked || submitting}
          onClick={onReview}
          className={`w-full rounded py-2 text-sm font-semibold text-white disabled:cursor-not-allowed disabled:opacity-40 ${
            side === 'BUY' ? 'bg-emerald-600 hover:bg-emerald-500' : 'bg-red-600 hover:bg-red-500'
          }`}
        >
          Review {side === 'BUY' ? 'Buy' : 'Sell'} {type === 'LIMIT' ? 'Limit' : 'Market'}
        </button>
      </div>

      {/* optimistic pending strip */}
      {myPending.length > 0 && (
        <div className="border-t border-neutral-800 px-3 py-2" data-testid="pending-orders">
          <p className="mb-1 text-[10px] font-medium uppercase tracking-wide text-neutral-500">
            Pending
          </p>
          <ul className="space-y-1">
            {myPending.map((p) => (
              <li key={p.clientOrderId} className="flex items-center justify-between text-xs">
                <span className="font-mono text-amber-400">PENDING</span>
                <span className="font-mono text-neutral-300">
                  {p.side} {p.quantity} {p.type}
                  {p.price !== undefined ? ` @ ${p.price}` : ''}
                </span>
                <span className="font-mono text-[10px] text-neutral-500">{p.clientOrderId}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* rollback / lifecycle notices */}
      {notices.length > 0 && (
        <div
          className="space-y-1 border-t border-neutral-800 px-3 py-2"
          data-testid="order-notices"
        >
          {notices.slice(-4).map((n) => (
            <div
              key={n.id}
              role={n.kind === 'confirmed' ? 'status' : 'alert'}
              className={`flex items-start justify-between gap-2 rounded px-2 py-1 text-xs ${
                n.kind === 'confirmed'
                  ? 'bg-emerald-950/50 text-emerald-300'
                  : n.kind === 'flushed'
                    ? 'bg-sky-950/50 text-sky-300'
                    : 'bg-red-950/50 text-red-300'
              }`}
            >
              <span>
                {n.message}
                {n.code !== undefined && (
                  <span className="ml-1 font-mono opacity-80">{n.code}</span>
                )}
                {n.requestId !== undefined && (
                  <span className="ml-1 font-mono text-neutral-500">req {n.requestId}</span>
                )}
              </span>
              <button
                type="button"
                aria-label="Dismiss notice"
                onClick={() => dismissNotice(n.id)}
                className="text-neutral-500 hover:text-neutral-300"
              >
                ×
              </button>
            </div>
          ))}
        </div>
      )}

      {/* confirmation modal */}
      {confirming && (
        <div
          role="dialog"
          aria-modal="true"
          aria-label="Confirm order"
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4"
        >
          <div className="w-full max-w-sm rounded-lg border border-neutral-700 bg-neutral-900 p-4 shadow-xl">
            <h3 className="text-sm font-semibold">
              Confirm {side} {type === 'LIMIT' ? 'limit' : 'market'} — {props.symbol}
            </h3>
            <dl className="mt-3 space-y-1 text-xs">
              <div className="flex justify-between">
                <dt className="text-neutral-400">Quantity</dt>
                <dd className="font-mono text-neutral-100">
                  {inst ? formatQty(quantity, inst) : quantity}
                </dd>
              </div>
              {type === 'LIMIT' && (
                <div className="flex justify-between">
                  <dt className="text-neutral-400">Price</dt>
                  <dd className="font-mono text-neutral-100">
                    {inst ? formatPrice(price, inst) : price}
                  </dd>
                </div>
              )}
              <div className="flex justify-between">
                <dt className="text-neutral-400">Time in force</dt>
                <dd className="font-mono text-neutral-100">{tif}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-neutral-400">Client ID</dt>
                <dd className="font-mono text-neutral-500">{clientOrderId}</dd>
              </div>
            </dl>

            <div className="mt-3 rounded border border-neutral-800 bg-neutral-950 p-2">
              {preview.state === 'loading' && (
                <p className="text-xs text-neutral-400" role="status">
                  Estimating execution…
                </p>
              )}
              {preview.state === 'err' && (
                <p className="text-xs text-amber-400" role="alert">
                  {preview.message}
                </p>
              )}
              {preview.state === 'ok' && (
                <dl className="space-y-1 text-xs" data-testid="order-preview">
                  <div className="flex justify-between">
                    <dt className="text-neutral-400">Est. fill</dt>
                    <dd className="font-mono text-neutral-100">
                      {preview.p.estimatedBaseQty} {inst?.baseCurrency ?? ''} ≈{' '}
                      {preview.p.estimatedQuoteQty} {inst?.quoteCurrency ?? ''}
                    </dd>
                  </div>
                  <div className="flex justify-between">
                    <dt className="text-neutral-400">Margin</dt>
                    <dd className="font-mono text-neutral-100">
                      {formatCurrency(preview.p.margin)}
                    </dd>
                  </div>
                  <div className="flex justify-between">
                    <dt className="text-neutral-400">Commission est.</dt>
                    <dd className="font-mono text-neutral-100">
                      {formatCurrency(preview.p.commissionEstimate)}
                    </dd>
                  </div>
                  <div className="flex justify-between">
                    <dt className="text-neutral-400">Risk</dt>
                    <dd
                      className={`font-mono font-semibold ${
                        preview.p.riskLevel === 'HIGH'
                          ? 'text-red-400'
                          : preview.p.riskLevel === 'MEDIUM'
                            ? 'text-amber-400'
                            : 'text-emerald-400'
                      }`}
                    >
                      {preview.p.riskLevel}
                    </dd>
                  </div>
                  {preview.p.warnings.map((w) => (
                    <p key={w} className="text-[10px] text-amber-400">
                      ⚠ {w}
                    </p>
                  ))}
                </dl>
              )}
            </div>

            <div className="mt-4 flex gap-2">
              <button
                type="button"
                onClick={() => setConfirming(false)}
                disabled={submitting}
                className="flex-1 rounded border border-neutral-700 py-1.5 text-xs text-neutral-300 hover:bg-neutral-800"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={() => void onConfirm()}
                disabled={submitting || locked}
                className={`flex-1 rounded py-1.5 text-xs font-semibold text-white disabled:opacity-40 ${
                  side === 'BUY'
                    ? 'bg-emerald-600 hover:bg-emerald-500'
                    : 'bg-red-600 hover:bg-red-500'
                }`}
              >
                {submitting ? 'Submitting…' : `Confirm ${side === 'BUY' ? 'Buy' : 'Sell'}`}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}
