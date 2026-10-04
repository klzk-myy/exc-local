/**
 * Order history / open orders (Task 10.3.27 item 1).
 *
 *   - Open / All-history tabs over GET /api/v1/orders
 *   - Filter bar consuming the input-helper framework (Task 10.3.29):
 *     SymbolAutocomplete + useValidatedField (symbol, client_order_id)
 *     + shared selects for side/status/type + RFC3339 date range.
 *   - Server-side keyset cursor pagination ("Load more").
 *   - Per-row: cancel (DELETE /orders/{id}) behind MEDIUM confirm;
 *     amend opens OrderInspectModal; audit link surfaces request_id via
 *     the row's client_order_id.
 *   - 501/error surfaces are honest — no fabricated rows.
 */
import { useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useInfiniteQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import {
  ErrorBox,
  btnDanger,
  btnGhost,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';
import { useAccountScope } from '@/lib/trading/accountScope';
import { cancelOrder } from '@/lib/trading/api';
import { isOpenOrder, type Order } from '@/lib/trading/types';
import {
  InputField,
  SymbolAutocomplete,
  UnavailablePanel,
  isNotImplemented,
  useValidatedField,
} from '@/lib/input-helpers';
import {
  ORDER_STATUSES,
  ORDER_SIDES,
  ORDER_TYPES,
  RULE_SYMBOL,
  type FieldRule,
} from '@/lib/input-helpers/validation';
import { OrderInspectModal } from '@/features/advanced-orders/OrderInspectModal';

import { fetchOrdersPage } from './api';

const CLIENT_ORDER_ID_RULE: FieldRule = { name: 'client_order_id', kind: 'string' };

function FilterBar({
  onApply,
}: {
  onApply: (f: {
    symbol?: string;
    side?: string;
    status?: string;
    type?: string;
    clientOrderId?: string;
    from?: string;
    to?: string;
  }) => void;
}) {
  const symbol = useValidatedField({ ...RULE_SYMBOL, required: false });
  const clientId = useValidatedField(CLIENT_ORDER_ID_RULE);
  const [side, setSide] = useState('');
  const [status, setStatus] = useState('');
  const [type, setType] = useState('');
  const [from, setFrom] = useState('');
  const [to, setTo] = useState('');

  return (
    <form
      className="grid grid-cols-2 gap-2 rounded-lg border border-neutral-800 bg-neutral-900 p-3 md:grid-cols-4 lg:grid-cols-8"
      aria-label="Order filters"
      onSubmit={(e) => {
        e.preventDefault();
        if (!symbol.valid || !clientId.valid) return;
        onApply({
          symbol: symbol.value === '' ? undefined : symbol.value.toUpperCase(),
          side: side === '' ? undefined : side,
          status: status === '' ? undefined : status,
          type: type === '' ? undefined : type,
          clientOrderId: clientId.value === '' ? undefined : clientId.value,
          from: from === '' ? undefined : from,
          to: to === '' ? undefined : to,
        });
      }}
    >
      <SymbolAutocomplete
        value={symbol.value}
        onChange={symbol.setValue}
        onSelect={(s) => {
          symbol.setValue(s);
        }}
        label="Pair"
        placeholder="All pairs"
      />
      <div>
        <label
          className="mb-1 block text-xs font-medium text-neutral-400"
          htmlFor="orders-filter-side"
        >
          Side
        </label>
        <select
          id="orders-filter-side"
          className={selectCls}
          value={side}
          onChange={(e) => setSide(e.target.value)}
        >
          <option value="">Any</option>
          {ORDER_SIDES.map((s) => (
            <option key={s}>{s}</option>
          ))}
        </select>
      </div>
      <div>
        <label
          className="mb-1 block text-xs font-medium text-neutral-400"
          htmlFor="orders-filter-status"
        >
          Status
        </label>
        <select
          id="orders-filter-status"
          className={selectCls}
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        >
          <option value="">Any</option>
          {ORDER_STATUSES.map((s) => (
            <option key={s}>{s}</option>
          ))}
        </select>
      </div>
      <div>
        <label
          className="mb-1 block text-xs font-medium text-neutral-400"
          htmlFor="orders-filter-type"
        >
          Type
        </label>
        <select
          id="orders-filter-type"
          className={selectCls}
          value={type}
          onChange={(e) => setType(e.target.value)}
        >
          <option value="">Any</option>
          {ORDER_TYPES.map((t) => (
            <option key={t}>{t}</option>
          ))}
        </select>
      </div>
      <div>
        <label
          className="mb-1 block text-xs font-medium text-neutral-400"
          htmlFor="orders-filter-from"
        >
          From
        </label>
        <input
          id="orders-filter-from"
          type="datetime-local"
          className={inputCls}
          value={from}
          onChange={(e) => setFrom(e.target.value)}
        />
      </div>
      <div>
        <label
          className="mb-1 block text-xs font-medium text-neutral-400"
          htmlFor="orders-filter-to"
        >
          To
        </label>
        <input
          id="orders-filter-to"
          type="datetime-local"
          className={inputCls}
          value={to}
          onChange={(e) => setTo(e.target.value)}
        />
      </div>
      <InputField field={clientId} label="Client order id" />
      <div className="flex items-end">
        <button type="submit" className={btnGhost + ' w-full'}>
          Apply
        </button>
      </div>
    </form>
  );
}

export function OrdersTable({ openOnly }: { openOnly: boolean }) {
  const scopeKey = useAccountScope((s) => s.scopeKey);
  const queryClient = useQueryClient();
  const [filter, setFilter] = useState<Parameters<typeof fetchOrdersPage>[0]>({});
  const [inspectOrder, setInspectOrder] = useState<Order | null>(null);
  const [cancelTarget, setCancelTarget] = useState<Order | null>(null);

  const effectiveFilter = useMemo(
    () => ({
      ...filter,
      // Open tab maps to the server's open-status set client-side —
      // the endpoint takes a single status, so we page the full set.
      ...(openOnly ? {} : { status: filter.status }),
      limit: 100,
    }),
    [filter, openOnly],
  );

  const q = useInfiniteQuery({
    queryKey: ['history', 'orders', scopeKey, openOnly, effectiveFilter],
    initialPageParam: '',
    queryFn: ({ pageParam }) =>
      fetchOrdersPage(
        { ...effectiveFilter, cursor: pageParam === '' ? undefined : pageParam },
        scopeKey,
        apiClient,
      ),
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    retry: (n, e) => !(e instanceof ApiError && e.status === 501) && n < 2,
  });

  const cancel = useMutation({
    mutationFn: (o: Order) => cancelOrder(o.id, apiClient),
    onSuccess: async () => {
      setCancelTarget(null);
      await queryClient.invalidateQueries({ queryKey: ['history', 'orders'] });
      await queryClient.invalidateQueries({ queryKey: ['orders'] });
    },
  });

  const orders = useMemo(() => {
    const all = q.data?.pages.flatMap((p) => p.orders) ?? [];
    return openOnly ? all.filter(isOpenOrder) : all;
  }, [q.data, openOnly]);

  if (q.isPending) return <p className="text-sm text-neutral-500">Loading orders…</p>;
  if (q.isError) {
    if (isNotImplemented(q.error)) return <UnavailablePanel feature="Order history" />;
    return <ErrorBox error={q.error} />;
  }

  return (
    <section aria-label={openOnly ? 'Open orders' : 'Order history'} className="space-y-3">
      <FilterBar onApply={setFilter} />

      {orders.length === 0 ? (
        <p className="text-sm text-neutral-500">No orders match.</p>
      ) : (
        <div className="relative overflow-x-auto" tabIndex={0}>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Time</th>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Type</th>
                <th className={thCls}>Side</th>
                <th className={thCls}>Qty</th>
                <th className={thCls}>Price</th>
                <th className={thCls}>Filled</th>
                <th className={thCls}>Status</th>
                <th className={thCls}><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {orders.map((o) => (
                <tr key={o.id}>
                  <td className={tdCls}>{o.createdAt || '—'}</td>
                  <td className={tdCls + ' font-mono'}>{o.symbol}</td>
                  <td className={tdCls}>{o.type}</td>
                  <td className={tdCls}>{o.side}</td>
                  <td className={tdCls + ' font-mono'}>{o.quantity.toString()}</td>
                  <td className={tdCls + ' font-mono'}>{o.price?.toString() ?? 'mkt'}</td>
                  <td className={tdCls + ' font-mono'}>{o.filledQty.toString()}</td>
                  <td className={tdCls}>{o.status}</td>
                  <td className={tdCls}>
                    <div className="flex gap-1">
                      {isOpenOrder(o) ? (
                        <>
                          <button
                            type="button"
                            className={btnGhost}
                            onClick={() => {
                              setInspectOrder(o);
                            }}
                          >
                            Amend
                          </button>
                          <button
                            type="button"
                            className={btnDanger}
                            onClick={() => {
                              setCancelTarget(o);
                            }}
                            disabled={cancel.isPending}
                          >
                            Cancel
                          </button>
                        </>
                      ) : null}
                      {o.clientOrderId !== '' ? (
                        <span
                          className="text-xs text-neutral-500"
                          title="Client order id (audit reference)"
                        >
                          {o.clientOrderId}
                        </span>
                      ) : null}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {q.hasNextPage ? (
        <button
          type="button"
          className={btnGhost}
          disabled={q.isFetchingNextPage}
          onClick={() => {
            void q.fetchNextPage();
          }}
        >
          {q.isFetchingNextPage ? 'Loading…' : 'Load more'}
        </button>
      ) : null}

      <OrderInspectModal
        order={inspectOrder}
        open={inspectOrder !== null}
        onClose={() => {
          setInspectOrder(null);
        }}
      />

      {cancelTarget ? (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
          role="presentation"
        >
          <div
            role="dialog"
            aria-modal="true"
            aria-label="Cancel order"
            className="w-full max-w-sm rounded-lg border border-amber-700 bg-neutral-900 p-5"
          >
            <h2 className="mb-2 text-lg font-semibold text-neutral-100">Cancel order</h2>
            <p className="text-sm text-neutral-300">
              Cancel {cancelTarget.side} {cancelTarget.quantity.toString()} {cancelTarget.symbol}{' '}
              (status {cancelTarget.status})?
            </p>
            {cancel.isError ? (
              <div className="mt-2">
                <ErrorBox error={cancel.error} />
              </div>
            ) : null}
            <div className="mt-4 flex justify-end gap-2">
              <button type="button" className={btnGhost} onClick={() => setCancelTarget(null)}>
                Keep order
              </button>
              <button
                type="button"
                className={btnDanger}
                disabled={cancel.isPending}
                onClick={() => {
                  cancel.mutate(cancelTarget);
                }}
              >
                {cancel.isPending ? 'Cancelling…' : 'Cancel order'}
              </button>
            </div>
          </div>
        </div>
      ) : null}
    </section>
  );
}
