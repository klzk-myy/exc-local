/**
 * /settings — account settings & security center (Task 10.3.22). Tabbed
 * hub (`?tab=`) so each panel stays a shallow, testable component:
 *   profile | security | api-keys | notifications | safety
 *   delegation | trading  (Phase-10.5 Task 10.5.3.17)
 */
import { Suspense, lazy } from 'react';
import { useSearchParams } from 'react-router';

import { RequireAuth } from '@/features/auth/guards';

const ProfilePanel = lazy(() => import('./ProfilePanel'));
const SecurityPanel = lazy(() => import('./SecurityPanel'));
const ApiKeysPanel = lazy(() => import('./ApiKeysPanel'));
const NotificationsPanel = lazy(() => import('./NotificationsPanel'));
const SafetyPanel = lazy(() => import('./SafetyPanel'));
const DelegationPanel = lazy(() => import('./DelegationPanel'));
const TradingPanel = lazy(() => import('./TradingPanel'));

const TABS = [
  { id: 'profile', label: 'Profile' },
  { id: 'security', label: 'Security' },
  { id: 'api-keys', label: 'API keys' },
  { id: 'notifications', label: 'Notifications' },
  { id: 'safety', label: 'Safety & data' },
  { id: 'delegation', label: 'Delegation' },
  { id: 'trading', label: 'Trading' },
] as const;
type TabId = (typeof TABS)[number]['id'];

function tabPanel(tab: TabId) {
  switch (tab) {
    case 'profile':
      return <ProfilePanel />;
    case 'security':
      return <SecurityPanel />;
    case 'api-keys':
      return <ApiKeysPanel />;
    case 'notifications':
      return <NotificationsPanel />;
    case 'safety':
      return <SafetyPanel />;
    case 'delegation':
      return <DelegationPanel />;
    case 'trading':
      return <TradingPanel />;
  }
}

export default function SettingsPage() {
  const [params, setParams] = useSearchParams();
  const raw = params.get('tab');
  const tab: TabId = TABS.find((t) => t.id === raw)?.id ?? 'profile';

  return (
    <RequireAuth>
      <div className="mx-auto max-w-3xl p-6">
        <h1 className="mb-4 text-xl font-semibold">Account settings</h1>
        <div role="tablist" aria-label="Settings sections" className="mb-6 flex flex-wrap gap-2">
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
            {tabPanel(tab)}
          </section>
        </Suspense>
      </div>
    </RequireAuth>
  );
}
