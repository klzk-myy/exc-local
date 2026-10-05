/**
 * Order inspect/modify/cancel modal (Task 10.3.15) — opened by clicking
 * or dragging an order overlay on the chart.
 *
 * Priority semantics are explicit per §6.9 / Phase-05 Task 5.3.37:
 *   - quantity-DOWN → `PUT /orders/{id}/amend/keep-priority` — preserves
 *     queue position;
 *   - price change or quantity-UP → `POST /orders/{id}/cancel-replace` —
 *     the modal warns that queue position is LOST;
 *   - cancel → `DELETE /orders/{id}`.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';

import { apiClient } from '@/app/runtime';
import { orderAmendments } from '@/features/history/api';
import { TcaCard } from '@/features/reports/TcaPanel';
import { ErrorBox, btnDanger, btnGhost, inputCls, labelCls, Modal } from '@/lib/ui';
import { tryDec } from '@/lib/decimal/decimal';
import { useScopeKey } from '@/lib/trading/queries';
import { amendKeepPriority, cancelOrder, cancelReplace, type Api } from '@/lib/trading/api';
import { formatPrice } from '@/lib/trading/fx';
import type { Order } from '@/lib/trading/types';

export interface OrderInspectModalProps {
  order: Order | null;
  /** Price proposed by a chart drag — pre-fills the modify form. */
  proposedPrice?: string;
  open: boolean;
  onClose: () => void;
  api?: Api;
}

export function OrderInspectModal({
  order,
  proposedPrice,
  open,
  onClose,
  api = apiClient,
}: OrderInspectModalProps) {
  const scope = useScopeKey();
  const queryClient = useQueryClient();
  const [price, setPrice] = useState('');
  const [qty, setQty] = useState('');
  const [error, setError] = useState<unknown>(null);
  const [done, setDone] = useState<string | null>(null);

  // Reset local fields when a different order opens the modal.
  const [openedFor, setOpenedFor] = useState<string | null>(null);
  if (open && order && openedFor !== order.id) {
    setOpenedFor(order.id);
    setPrice(proposedPrice ?? order.price?.toString() ?? '');
    setQty(order.quantity.toString());
    setError(null);
    setDone(null);
  }

  const newPrice = useMemo(() => (price.trim() === '' ? undefined : tryDec(price)), [price]);
  const newQty = useMemo(() => (qty.trim() === '' ? undefined : tryDec(qty)), [qty]);

  const priceChanged =
    order !== null &&
    newPrice !== undefined &&
    order.price !== undefined &&
    !newPrice.eq(order.price);
  const qtyDown =
    order !== null && newQty !== undefined && newQty.isPositive() && newQty.lt(order.quantity);
  const qtyChanged = order !== null && newQty !== undefined && !newQty.eq(order.quantity);
  /** Keep-priority is legal only for a pure quantity reduction (§6.9). */
  const keepPriority = qtyDown && !priceChanged;
  const losesPriority = priceChanged || (qtyChanged && !qtyDown);

  const act = useMutation({
    mutationFn: async () => {
      if (!order) return 'no order';
      if (keepPriority) {
        await amendKeepPriority(order.id, newQty.toString(), order.orderSeq, api);
        return `Amended to ${newQty.toDisplay()} — queue position kept`;
      }
      if (priceChanged || qtyChanged) {
        await cancelReplace(
          order.id,
          {
            order_seq: order.orderSeq,
            price: priceChanged ? newPrice.toString() : undefined,
            quantity: qtyChanged ? newQty.toString() : undefined,
            mode: 'STOP_ON_FAILURE',
          },
          api,
        );
        return 'Cancel-replace submitted — queue position lost';
      }
      return 'no change';
    },
    onSuccess: async (msg) => {
      setDone(msg);
      setError(null);
      await queryClient.invalidateQueries({ queryKey: ['orders', scope] });
    },
    onError: (e) => setError(e),
  });

  const cancel = useMutation({
    mutationFn: async () => {
      if (!order) return;
      await cancelOrder(order.id, api);
    },
    onSuccess: async () => {
      setDone('Order cancelled');
      await queryClient.invalidateQueries({ queryKey: ['orders', scope] });
    },
    onError: (e) => setError(e),
  });

  const amendable = order?.price !== undefined;

  return (
    <Modal
      open={open}
      title={order ? `Order ${order.id} — ${order.symbol}` : 'Order'}
      onClose={onClose}
    >
      {order && (
        <div className="text-sm">
          <div className="mb-3 grid grid-cols-2 gap-x-4 gap-y-1 rounded border border-neutral-800 bg-neutral-950 px-3 py-2 text-xs">
            <span className="text-neutral-500">Side / type</span>
            <span className="text-neutral-200">
              {order.side} {order.type}
            </span>
            <span className="text-neutral-500">Status</span>
            <span className="text-neutral-200">{order.status}</span>
            <span className="text-neutral-500">Price / stop</span>
            <span className="text-neutral-200">
              {formatPrice(order.symbol, order.price)}
              {order.stopPrice ? ` / trig ${formatPrice(order.symbol, order.stopPrice)}` : ''}
            </span>
            <span className="text-neutral-500">Qty / filled</span>
            <span className="text-neutral-200">
              {order.quantity.toDisplay()} / {order.filledQty.toDisplay()}
            </span>
            <span className="text-neutral-500">TIF</span>
            <span className="text-neutral-200">{order.timeInForce}</span>
          </div>

          {order.type === 'MARKET' || !amendable ? (
            <p className="mb-3 text-xs text-neutral-400">
              This order type carries no resting price to amend — cancel it instead.
            </p>
          ) : (
            <div className="mb-3 grid grid-cols-2 gap-2">
              <div>
                <label htmlFor="inspect-price" className={labelCls}>
                  New price
                </label>
                <input
                  id="inspect-price"
                  inputMode="decimal"
                  value={price}
                  onChange={(e) => setPrice(e.target.value)}
                  className={inputCls}
                />
              </div>
              <div>
                <label htmlFor="inspect-qty" className={labelCls}>
                  New quantity
                </label>
                <input
                  id="inspect-qty"
                  inputMode="decimal"
                  value={qty}
                  onChange={(e) => setQty(e.target.value)}
                  className={inputCls}
                />
              </div>
            </div>
          )}

          <div aria-live="polite">
            {losesPriority && (priceChanged || qtyChanged) && (
              <p className="mb-2 rounded border border-amber-700/50 bg-amber-950/30 px-2 py-1.5 text-xs text-amber-300">
                ⚠ {priceChanged ? 'Changing the price' : 'Increasing the quantity'} executes as
                atomic cancel-replace — <strong>queue position is lost</strong>.
              </p>
            )}
            {keepPriority && (
              <p className="mb-2 rounded border border-emerald-700/50 bg-emerald-950/30 px-2 py-1.5 text-xs text-emerald-300">
                Quantity reduction keeps your queue position (keep-priority amend).
              </p>
            )}
            {done !== null && <p className="mb-2 text-xs text-emerald-400">{done}</p>}
          </div>

          <ErrorBox error={error} onDismiss={() => setError(null)} />

          <div className="mt-4 flex justify-end gap-2">
            <button
              type="button"
              className={btnGhost}
              onClick={() => cancel.mutate()}
              disabled={cancel.isPending || act.isPending}
            >
              {cancel.isPending ? 'Cancelling…' : 'Cancel order'}
            </button>
            <button
              type="button"
              className={btnDanger}
              disabled={act.isPending || cancel.isPending || !(priceChanged || qtyChanged)}
              onClick={() => act.mutate()}
            >
              {act.isPending
                ? 'Applying…'
                : keepPriority
                  ? 'Amend (keep priority)'
                  : 'Amend (lose priority)'}
            </button>
          </div>
          <AmendmentLedger orderId={order.id} api={api} />
          <div className="mt-3 border-t border-neutral-800 pt-2">
            <TcaCard symbol={order.symbol} api={api} />
          </div>
        </div>
      )}
    </Modal>
  );
}

/** GET /orders/{id}/amendments — the amendment audit trail
 * (Task 10.5.3.20). Rows render verbatim: operation, field, old→new,
 * actor, request id, timestamp. */
function AmendmentLedger({ orderId, api }: { orderId: string; api: Api }) {
  const [show, setShow] = useState(false);
  const q = useQuery({
    queryKey: ['history', 'order-amendments', orderId],
    queryFn: () => orderAmendments(api, orderId),
    enabled: show,
  });
  return (
    <div className="mt-3 border-t border-neutral-800 pt-2">
      <button
        type="button"
        className={`${btnGhost} text-xs`}
        onClick={() => {
          setShow((s) => !s);
        }}
      >
        {show ? 'Hide amendment history' : 'Amendment history'}
      </button>
      {show ? (
        q.isPending ? (
          <p className="mt-2 text-xs text-neutral-500">Loading amendment history…</p>
        ) : q.isError ? (
          <div className="mt-2">
            <ErrorBox error={q.error} />
          </div>
        ) : q.data.length === 0 ? (
          <p className="mt-2 text-xs text-neutral-500">No amendments recorded.</p>
        ) : (
          <ul className="mt-2 space-y-1 text-xs">
            {q.data.map((a, i) => (
              <li key={i} className="rounded border border-neutral-800 px-2 py-1">
                <span className="font-medium text-neutral-200">{a.operation ?? 'AMEND'}</span>{' '}
                <span className="text-neutral-400">
                  {a.fieldName ?? ''}: {a.oldValue ?? '—'} → {a.newValue ?? '—'}
                </span>{' '}
                <span className="text-neutral-500">
                  by {a.modifiedBy ?? '—'}
                  {a.modifiedAt !== undefined ? ` · ${a.modifiedAt}` : ''}
                  {a.requestId !== undefined ? ` · req ${a.requestId}` : ''}
                </span>
              </li>
            ))}
          </ul>
        )
      ) : null}
    </div>
  );
}
