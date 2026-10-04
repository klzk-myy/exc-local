/**
 * Copy trading & grid bots — Task 10.3.26 page shell.
 * Tabs: Strategy browser · Grid bots (active list + wizard).
 * My-follows management (list + unfollow) is live — Phase-14 Task
 * 14.3.14 routes (GET/DELETE /api/v1/copy/follows[{id}]).
 */
import { useState } from 'react';

import { RequireAuth } from '@/features/auth/guards';

import { ActiveBotsPanel } from './ActiveBotsPanel';
import { FollowModal } from './FollowModal';
import { GridBotWizard } from './GridBotWizard';
import { MyFollowsPanel } from './MyFollowsPanel';
import { StrategyBrowser } from './StrategyBrowser';
import type { CopyStrategy } from './api';

const TABS = [
  { id: 'strategies', label: 'Copy trading' },
  { id: 'bots', label: 'Grid bots' },
] as const;
type TabId = (typeof TABS)[number]['id'];

export default function CopyGridPage() {
  const [tab, setTab] = useState<TabId>('strategies');
  const [followTarget, setFollowTarget] = useState<CopyStrategy | null>(null);
  const [followOpen, setFollowOpen] = useState(false);
  const [activeBots, setActiveBots] = useState<number | undefined>(undefined);

  return (
    <RequireAuth>
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">Copy trading &amp; grid bots</h1>
        <p className="text-sm text-neutral-500">
          Strategy copying and automated grid execution. Backend surfaces pending phases render an
          explicit unavailable state — no synthetic data.
        </p>
      </header>

      <div
        role="tablist"
        aria-label="Copy & bots sections"
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

      {tab === 'strategies' ? (
        <>
          <StrategyBrowser
            onFollow={(s) => {
              setFollowTarget(s);
              setFollowOpen(true);
            }}
          />
          <MyFollowsPanel />
        </>
      ) : (
        <div className="space-y-4">
          <ActiveBotsPanel onCount={setActiveBots} />
          <GridBotWizard activeBots={activeBots} feeBps={null} />
        </div>
      )}

      <FollowModal
        strategy={followTarget}
        open={followOpen}
        onClose={() => {
          setFollowOpen(false);
        }}
      />
    </div>
    </RequireAuth>
  );
}
