/**
 * SPA shell: top nav (auto-discovered from `features/◦/nav.ts`), the
 * Task 10.3.19 connection banner, WS status pill, FX market-hours badge,
 * and the routed <Outlet/>.
 */
import { useEffect } from 'react';
import { NavLink, Outlet } from 'react-router';

import { navBySection } from '@/app/manifest';
import { wsClient } from '@/app/runtime';
import { ConnectionBanner, StalePricingBadge } from '@/components/ConnectionBanner';
import { isFxMarketOpen } from '@/lib/market/tradingHours';
import { useWsStatus } from '@/lib/ws';

const STATE_PILL: Record<string, string> = {
  CONNECTING: 'bg-sky-500/20 text-sky-400',
  AUTHENTICATED: 'bg-emerald-500/20 text-emerald-400',
  STALE: 'bg-amber-500/20 text-amber-400',
  DISCONNECTED: 'bg-red-500/20 text-red-400',
  RECONNECTING: 'bg-red-500/20 text-red-400',
  RESYNCING: 'bg-amber-500/20 text-amber-400',
};

function ConnectionPill() {
  const status = useWsStatus(wsClient);
  return (
    <span
      className={`rounded px-2 py-0.5 text-xs font-medium ${STATE_PILL[status.state] ?? ''}`}
      aria-live="polite"
    >
      {status.state}
    </span>
  );
}

function MarketHoursBadge() {
  // Re-render on a 30s cadence — enough for a session badge.
  const open = isFxMarketOpen(Date.now());
  return (
    <span
      className={`rounded px-2 py-0.5 text-xs font-medium ${
        open ? 'bg-emerald-500/20 text-emerald-400' : 'bg-neutral-700/40 text-neutral-400'
      }`}
      title="FX market hours: 24/5 — Sun 21:00 UTC → Fri 22:00 UTC"
    >
      {open ? 'MARKET OPEN' : 'MARKET CLOSED'}
    </span>
  );
}

export function AppShell() {
  const sections = navBySection();

  // Connect the shared WS client for the shell's lifetime; feature
  // subscriptions attach via client.subscribe().
  useEffect(() => {
    wsClient.start();
    return () => {
      wsClient.stop();
    };
  }, []);

  return (
    <div className="flex min-h-screen flex-col">
      <ConnectionBanner />
      <header className="flex items-center gap-4 border-b border-neutral-800 px-4 py-2">
        <span className="text-lg font-bold tracking-tight">Exchange</span>
        <nav aria-label="Primary" className="flex flex-1 items-center gap-1">
          {[...sections.entries()].map(([section, items]) => (
            <div key={section} className="flex items-center gap-1">
              {items.map(({ item }) => (
                <NavLink
                  key={`${item.section}:${item.to}`}
                  to={item.to}
                  end={item.to === '/'}
                  className={({ isActive }) =>
                    `rounded px-3 py-1.5 text-sm font-medium transition-colors ${
                      isActive
                        ? 'bg-neutral-800 text-white'
                        : 'text-neutral-400 hover:bg-neutral-900 hover:text-neutral-200'
                    }`
                  }
                >
                  {item.label}
                </NavLink>
              ))}
            </div>
          ))}
        </nav>
        <div className="flex items-center gap-2">
          <StalePricingBadge />
          <MarketHoursBadge />
          <ConnectionPill />
        </div>
      </header>
      <main className="flex-1">
        <Outlet />
      </main>
    </div>
  );
}
