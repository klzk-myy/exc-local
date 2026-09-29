/**
 * Order history & order-management surface (Task 10.3.27).
 * Tabs: Open orders · History · Algo orders · Order lists (OPO/OCO) ·
 * Safety (dead-man switch + order test) · Live tape.
 */
import { useState } from 'react';

import { AlgoPanel } from './AlgoPanel';
import { DeadmanSwitch } from './DeadmanSwitch';
import { LiveTape } from './LiveTape';
import { OrderListsPanel } from './OrderListsPanel';
import { OrderTestPanel } from './OrderTestPanel';
import { OrdersTable } from './OrdersTable';

const TABS = [
  { id: 'open', label: 'Open orders' },
  { id: 'history', label: 'History' },
  { id: 'algo', label: 'Algo orders' },
  { id: 'lists', label: 'Order lists' },
  { id: 'safety', label: 'Safety & test' },
  { id: 'tape', label: 'Live tape' },
] as const;
type TabId = (typeof TABS)[number]['id'];

export default function HistoryPage() {
  const [tab, setTab] = useState<TabId>('open');
  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">Orders</h1>
        <p className="text-sm text-neutral-500">
          Open orders, history, algos, order lists, dead-man switch, order test and the live tape.
        </p>
      </header>
      <div
        role="tablist"
        aria-label="Order surfaces"
        className="flex gap-1 border-b border-neutral-800"
      >
        {TABS.map((t) => (
          <button
            key={t.id}
            role="tab"
            aria-selected={tab === t.id}
            className={`px-3 py-2 text-sm ${
              tab === t.id
                ? 'border-b-2 border-sky-500 text-neutral-100'
                : 'text-neutral-400 hover:text-neutral-200'
            }`}
            onClick={() => {
              setTab(t.id);
            }}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'open' ? <OrdersTable openOnly /> : null}
      {tab === 'history' ? <OrdersTable openOnly={false} /> : null}
      {tab === 'algo' ? <AlgoPanel /> : null}
      {tab === 'lists' ? <OrderListsPanel /> : null}
      {tab === 'safety' ? (
        <div className="space-y-4">
          <DeadmanSwitch />
          <OrderTestPanel />
        </div>
      ) : null}
      {tab === 'tape' ? <LiveTape /> : null}
    </div>
  );
}
