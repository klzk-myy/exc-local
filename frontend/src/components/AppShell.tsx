/**
 * SPA shell: left sidebar nav (auto-discovered from `features/◦/nav.ts`),
 * the Task 10.3.19 connection banner, WS status pill, FX market-hours badge,
 * and the routed <Outlet/>.
 */
import { useEffect, useState } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router';

import { navBySection } from '@/app/manifest';
import { wsClient } from '@/app/runtime';
import { ConnectionBanner, StalePricingBadge } from '@/components/ConnectionBanner';
import { useAdminRole } from '@/features/admin/adminRole';
import { NotificationBell } from '@/features/notifications/NotificationBell';
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

const NAV_LINK_CLS = ({ isActive }: { isActive: boolean }): string =>
  `block rounded px-3 py-1.5 text-sm font-medium transition-colors ${
    isActive
      ? 'bg-neutral-800 text-white'
      : 'text-neutral-400 hover:bg-neutral-900 hover:text-neutral-200'
  }`;

function NavSections({ onNavigate }: { onNavigate?: () => void }) {
  const sections = navBySection();
  // Role-filtered sidebar: the Admin section (spec §8.2 surfaces) renders
  // only when the session carries a recognized venue-admin role. Page-level
  // RequireAdmin + server-side authorization stay the real gates — this
  // just keeps non-admins from seeing links that would render denial cards.
  const adminRole = useAdminRole();
  const visibleSections =
    adminRole === null
      ? [...sections.entries()].filter(([section]) => section !== 'Admin')
      : [...sections.entries()];

  return (
    <>
      {visibleSections.map(([section, items]) => (
        <div key={section} className="mb-1">
          <div className="px-3 pb-1 pt-2 text-xs font-semibold uppercase tracking-wider text-neutral-500">
            {section}
          </div>
          {items.map(({ item }) => (
            <NavLink
              key={`${item.section}:${item.to}`}
              to={item.to}
              end={item.to === '/'}
              onClick={onNavigate}
              className={NAV_LINK_CLS}
            >
              {item.label}
            </NavLink>
          ))}
        </div>
      ))}
    </>
  );
}

function ShellBadges() {
  return (
    <div className="flex flex-col items-stretch gap-1.5 border-t border-neutral-800 p-3">
      <StalePricingBadge />
      <MarketHoursBadge />
      <div className="flex items-center gap-2">
        <NotificationBell />
        <ConnectionPill />
      </div>
    </div>
  );
}

export function AppShell() {
  const [navOpen, setNavOpen] = useState(false);
  const loc = useLocation();

  // Close the mobile drawer on route change and on Escape.
  useEffect(() => {
    setNavOpen(false);
  }, [loc.pathname]);
  useEffect(() => {
    if (!navOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setNavOpen(false);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [navOpen]);

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
      {/* Mobile top bar — sidebar becomes a drawer below lg. */}
      <header className="flex items-center gap-3 border-b border-neutral-800 bg-neutral-950 px-3 py-2 lg:hidden">
        <button
          type="button"
          aria-label={navOpen ? 'Close navigation menu' : 'Open navigation menu'}
          aria-expanded={navOpen}
          aria-controls="mobile-nav"
          onClick={() => setNavOpen((o) => !o)}
          className="rounded border border-neutral-700 p-1.5 text-neutral-200 hover:bg-neutral-800 focus-visible:ring-2 focus-visible:ring-sky-500"
        >
          <svg aria-hidden="true" className="h-6 w-6" viewBox="0 0 20 20" fill="currentColor">
            {navOpen ? (
              <path
                fillRule="evenodd"
                d="M4.3 4.3a1 1 0 011.4 0L10 8.6l4.3-4.3a1 1 0 111.4 1.4L11.4 10l4.3 4.3a1 1 0 01-1.4 1.4L10 11.4l-4.3 4.3a1 1 0 01-1.4-1.4L8.6 10 4.3 5.7a1 1 0 010-1.4z"
                clipRule="evenodd"
              />
            ) : (
              <path
                fillRule="evenodd"
                d="M3 5.5A.75.75 0 013.75 5h12.5a.75.75 0 010 1.5H3.75A.75.75 0 013 5.5zm0 4.5a.75.75 0 01.75-.75h12.5a.75.75 0 010 1.5H3.75A.75.75 0 013 10zm0 4.5a.75.75 0 01.75-.75h12.5a.75.75 0 010 1.5H3.75A.75.75 0 013 14.5z"
                clipRule="evenodd"
              />
            )}
          </svg>
        </button>
        <span className="text-lg font-bold tracking-tight">Exchange</span>
      </header>
      <div className="flex min-h-0 flex-1">
        {navOpen && (
          <div
            className="fixed inset-0 z-40 lg:hidden"
            role="dialog"
            aria-modal="true"
            aria-label="Navigation"
          >
            <div
              className="absolute inset-0 bg-black/60"
              aria-hidden="true"
              onClick={() => setNavOpen(false)}
            />
            <aside
              id="mobile-nav"
              className="absolute inset-y-0 left-0 flex w-64 flex-col border-r border-neutral-800 bg-neutral-950"
            >
              <div className="border-b border-neutral-800 px-4 py-3">
                <span className="text-lg font-bold tracking-tight">Exchange</span>
              </div>
              <nav aria-label="Primary" className="min-h-0 flex-1 overflow-y-auto p-2">
                <NavSections onNavigate={() => setNavOpen(false)} />
              </nav>
              <ShellBadges />
            </aside>
          </div>
        )}
        <aside className="hidden w-56 shrink-0 flex-col border-r border-neutral-800 bg-neutral-950 lg:flex">
          <div className="border-b border-neutral-800 px-4 py-3">
            <span className="text-lg font-bold tracking-tight">Exchange</span>
          </div>
          <nav aria-label="Primary" className="min-h-0 flex-1 overflow-y-auto p-2">
            <NavSections />
          </nav>
          <ShellBadges />
        </aside>
        <main className="min-w-0 flex-1">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
