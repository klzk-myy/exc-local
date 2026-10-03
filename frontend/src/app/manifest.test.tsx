/**
 * Manifest auto-discovery tests — proves the glob convention picks up
 * `features/<name>/routes.ts` + `features/<name>/nav.ts` with zero
 * shared-file edits, and that discovered routes render inside AppShell
 * with the Task 10.3.19 connection surfaces wired.
 */
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import { Suspense } from 'react';
import { createMemoryRouter, RouterProvider, type RouteObject } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { MockSocket } from '@/lib/ws/testkit';

import { collectFeatureRoutes, collectNavItems, navBySection } from './manifest';
import { AppProviders } from './providers';

// The shared runtime wsClient dials whatever `WebSocket` resolves to at
// connect time — swap in the mock socket so the shell test is hermetic.
const sockets: MockSocket[] = [];
vi.stubGlobal(
  'WebSocket',
  class extends MockSocket {
    constructor(url: string | URL) {
      super(String(url));
      sockets.push(this);
    }
  },
);
vi.stubGlobal(
  'fetch',
  vi.fn(async () =>
    Promise.resolve(
      new Response(JSON.stringify({ ts_ms: 1_700_000_000_000 }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  ),
);

const flush = () => new Promise<void>((r) => setTimeout(r, 0));

afterEach(() => {
  cleanup();
});

describe('auto-discovery', () => {
  it('discovers the home feature route', () => {
    const routes = collectFeatureRoutes();
    const home = routes.find((r) => r.feature === 'home');
    expect(home?.route.index).toBe(true);
    expect(home?.route.title).toBe('Dashboard');
  });

  it('discovers the home feature nav item', () => {
    const items = collectNavItems();
    const home = items.find((i) => i.feature === 'home');
    expect(home?.item).toMatchObject({ label: 'Dashboard', to: '/', section: 'Trade' });
  });

  it('groups nav items by section', () => {
    const sections = navBySection();
    expect(sections.get('Trade')?.map((e) => e.item.to)).toContain('/');
  });

  it('renders sections in canonical product order, not alphabetically', () => {
    expect([...navBySection().keys()]).toEqual(['Trade', 'Research', 'Invest', 'Account', 'Admin']);
  });

  it('keeps deep-link and redundant pages out of the sidebar', () => {
    const tos = collectNavItems().map((e) => e.item.to);
    for (const hidden of [
      '/notifications', // NotificationBell already surfaces this
      '/account/sessions', // linked from Settings → Security
      '/fee-tiers', // linked from the fee admin surface
    ]) {
      expect(tos).not.toContain(hidden);
    }
  });

  it('keeps hidden pages routable via feature routes', () => {
    const paths = collectFeatureRoutes().map((r) => r.route.path);
    for (const route of [
      'notifications',
      'account/sessions',
      'webhooks',
      'fee-tiers',
      'workspace',
    ]) {
      expect(paths).toContain(route);
    }
  });
});

describe('shell + route rendering', () => {
  it('renders the discovered home route inside AppShell', async () => {
    const featureRoutes: RouteObject[] = collectFeatureRoutes().map(({ route }) => {
      const Element = route.element;
      return {
        ...(route.index === true ? { index: true } : { path: route.path }),
        element: (
          <Suspense fallback={null}>
            <Element />
          </Suspense>
        ),
      };
    });
    const { AppShell } = await import('@/components/AppShell');
    const router = createMemoryRouter([{ element: <AppShell />, children: featureRoutes }], {
      initialEntries: ['/'],
    });
    render(
      <AppProviders>
        <RouterProvider router={router} />
      </AppProviders>,
    );

    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Dashboard' })).toBeInTheDocument(),
    );
    // Nav label came from features/home/nav.ts — zero shared-file edits.
    expect(screen.getByRole('link', { name: 'Dashboard' })).toBeInTheDocument();

    // The shell's WS client dialed our mock — drive it to AUTHENTICATED.
    // (Pill + dashboard card both render the state name → getAllByText.)
    expect(sockets).toHaveLength(1);
    await waitFor(() => expect(screen.getAllByText('CONNECTING').length).toBeGreaterThan(0));
    await act(async () => {
      sockets[0]!.open();
      await flush();
    });
    await waitFor(() => expect(screen.getAllByText('AUTHENTICATED').length).toBeGreaterThan(0));

    // Close → Task 10.3.19 banner + locked order entry.
    act(() => {
      sockets[0]!.serverClose(1006);
    });
    await waitFor(() =>
      expect(screen.getAllByText(/DISCONNECTED|RECONNECTING/).length).toBeGreaterThan(0),
    );
    expect(screen.getByText(/Order entry locked/i)).toBeInTheDocument();
  });
});
