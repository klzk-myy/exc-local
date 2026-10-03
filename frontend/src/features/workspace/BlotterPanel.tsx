/**
 * Blotter panel — the MT5 Toolbox / Binance bottom-tabs role in the
 * workspace: Positions · Open orders · History in one tabbed tile.
 *
 *   - Positions tab embeds the canonical PositionsPanel untouched
 *     (quick actions + confirm modals stay its own concern).
 *   - Open/History tabs are compact tables over the shared
 *     useOpenOrders/useOrders query seams — the Orders page keeps the
 *     full filter/pagination surface; this is the at-a-glance blotter.
 *   - Cancel goes through the same cancelOrder + ConfirmAction pair the
 *     Orders page uses.
 */
import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';

import { ConfirmAction, ErrorBox, Modal, btnGhost, tableCls, tdCls, thCls } from '@/lib/ui';
import { useOpenOrders, useOrders } from '@/lib/trading/queries';
import { cancelOrder } from '@/lib/trading/api';
import type { Order } from '@/lib/trading/types';
import { PositionsPanel } from '@/features/advanced-orders/PositionsPanel';

const TABS = [
  { id: 'positions', label: 'Positions' },
  { id: 'open', label: 'Open orders' },
  { id: 'history', label: 'History' },
] as const;
type TabId = (typeof TABS)[number]['id'];

function OrderRows({ orders, onCancel }: { orders: Order[]; onCancel?: (o: Order) => void }) {
  return (
    <table className={tableCls}>
      <thead>
        <tr>
          <th className={thCls}>Symbol</th>
          <th className={thCls}>Side</th>
          <th className={thCls}>Price</th>
          <th className={thCls}>Qty</th>
          <th className={thCls}>Filled</th>
          <th className={thCls}>Status</th>
          {onCancel !== undefined ? <th className={thCls} /> : null}
        </tr>
      </thead>
      <tbody>
        {orders.map((o) => (
          <tr key={o.id}>
            <td className={`${tdCls} font-mono`}>{o.symbol}</td>
            <td
              className={`${tdCls} font-mono ${o.side === 'BUY' ? 'text-emerald-400' : 'text-red-400'}`}
            >
              {o.side}
            </td>
            <td className={`${tdCls} font-mono`}>{o.price?.toString() ?? '—'}</td>
            <td className={`${tdCls} font-mono`}>{o.quantity.toString()}</td>
            <td className={`${tdCls} font-mono`}>{o.filledQty.toString()}</td>
            <td className={`${tdCls} text-neutral-400`}>{o.status}</td>
            {onCancel !== undefined ? (
              <td className={tdCls}>
                <button type="button" className={btnGhost} onClick={() => onCancel(o)}>
                  Cancel
                </button>
              </td>
            ) : null}
          </tr>
        ))}
        {orders.length === 0 && (
          <tr>
            <td className={`${tdCls} text-neutral-500`} colSpan={onCancel ? 7 : 6}>
              No orders.
            </td>
          </tr>
        )}
      </tbody>
    </table>
  );
}

export function BlotterPanel() {
  const [tab, setTab] = useState<TabId>('positions');
  const [confirm, setConfirm] = useState<Order | null>(null);
  const qc = useQueryClient();

  const open = useOpenOrders();
  const history = useOrders({ limit: 20 });
  const cancel = useMutation({
    mutationFn: (id: string) => cancelOrder(id),
    onSuccess: () => {
      setConfirm(null);
      void qc.invalidateQueries({ queryKey: ['orders'] });
      void qc.invalidateQueries({ queryKey: ['history'] });
    },
  });

  return (
    <div className="space-y-2">
      <div role="tablist" aria-label="Blotter" className="flex gap-1 border-b border-neutral-800">
        {TABS.map((t, i) => (
          <button
            key={t.id}
            role="tab"
            id={`blotter-tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls={`blotter-panel-${t.id}`}
            tabIndex={tab === t.id ? 0 : -1}
            className={`px-3 py-2 text-xs ${
              tab === t.id
                ? 'border-b-2 border-sky-500 text-neutral-100'
                : 'text-neutral-400 hover:text-neutral-200'
            }`}
            onClick={() => setTab(t.id)}
            onKeyDown={(e) => {
              // APG tabs pattern: arrows move focus+selection within the list
              const dir = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
              if (dir === 0) return;
              e.preventDefault();
              const next = TABS[(i + dir + TABS.length) % TABS.length];
              if (next === undefined) return;
              setTab(next.id);
              document.getElementById(`blotter-tab-${next.id}`)?.focus();
            }}
          >
            {t.label}
          </button>
        ))}
      </div>
      <div
        className="overflow-x-auto"
        role="tabpanel"
        id={`blotter-panel-${tab}`}
        aria-labelledby={`blotter-tab-${tab}`}
      >
        {tab === 'positions' ? <PositionsPanel bare /> : null}
        {tab === 'open' ? (
          <>
            <ErrorBox error={open.query.isError ? open.query.error : null} />
            <OrderRows orders={open.orders} onCancel={setConfirm} />
          </>
        ) : null}
        {tab === 'history' ? (
          <>
            <ErrorBox error={history.isError ? history.error : null} />
            <OrderRows orders={history.data?.orders ?? []} />
          </>
        ) : null}
      </div>
      <Modal open={confirm !== null} title="Cancel order" onClose={() => setConfirm(null)}>
        {confirm !== null && (
          <ConfirmAction
            message={
              <>
                Cancel {confirm.side} {confirm.quantity.toString()} {confirm.symbol}
                {confirm.price !== undefined ? ` @ ${confirm.price.toString()}` : ''}?
              </>
            }
            confirmLabel="Cancel order"
            busy={cancel.isPending}
            onConfirm={() => cancel.mutate(confirm.id)}
            onCancel={() => setConfirm(null)}
          />
        )}
      </Modal>
    </div>
  );
}
