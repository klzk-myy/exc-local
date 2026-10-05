/**
 * /funding — deposits, withdrawals, internal transfers, history
 * (Task 10.3.23). Tabbed via `?tab=`; `/funding/withdrawals/:id`-style
 * deep links are unnecessary — the pending list carries its own confirm.
 */
import { Suspense, lazy } from 'react';
import { useSearchParams } from 'react-router';

import { RequireAuth } from '@/features/auth/guards';

const DepositPanel = lazy(() => import('./DepositPanel'));
const WithdrawalPanel = lazy(() => import('./WithdrawalPanel'));
const TransferPanel = lazy(() => import('./TransferPanel'));
const AccountsPanel = lazy(() => import('./AccountsPanel'));
const HistoryPanel = lazy(() => import('./HistoryPanel'));

const TABS = [
  { id: 'deposit', label: 'Deposit' },
  { id: 'withdraw', label: 'Withdraw' },
  { id: 'transfer', label: 'Internal transfer' },
  { id: 'accounts', label: 'Bank accounts' },
  { id: 'history', label: 'History' },
] as const;
type TabId = (typeof TABS)[number]['id'];

function panel(tab: TabId) {
  switch (tab) {
    case 'deposit':
      return <DepositPanel />;
    case 'withdraw':
      return <WithdrawalPanel />;
    case 'transfer':
      return <TransferPanel />;
    case 'accounts':
      return <AccountsPanel />;
    case 'history':
      return <HistoryPanel />;
  }
}

export default function FundingPage() {
  const [params, setParams] = useSearchParams();
  const tab: TabId = TABS.find((t) => t.id === params.get('tab'))?.id ?? 'deposit';
  return (
    <RequireAuth>
      <div className="mx-auto max-w-3xl p-6">
        <h1 className="mb-4 text-xl font-semibold">Funding</h1>
        <div role="tablist" aria-label="Funding sections" className="mb-6 flex flex-wrap gap-2">
          {TABS.map((t) => (
            <button
              key={t.id}
              type="button"
              role="tab"
              aria-selected={tab === t.id}
              className={`rounded px-3 py-1.5 text-sm ${
                tab === t.id
                  ? 'bg-sky-600 font-medium text-white'
                  : 'border border-neutral-700 text-neutral-300 hover:bg-neutral-800'
              }`}
              onClick={() => {
                setParams({ tab: t.id });
              }}
            >
              {t.label}
            </button>
          ))}
        </div>
        <Suspense fallback={<p className="text-sm text-neutral-400">Loading…</p>}>
          <section role="tabpanel" aria-label={TABS.find((t) => t.id === tab)?.label}>
            {panel(tab)}
          </section>
        </Suspense>
      </div>
    </RequireAuth>
  );
}
