import { expect, request, test } from '@playwright/test';

import { ADMIN, API, HARNESS_XFF, apiLogin, uiLogin, watchErrors } from './support';

/**
 * Admin-console smoke — the Super Admin session renders the role-gated
 * "Admin" nav section that the taker fixture never sees
 * (AppShell NavSections filters it for non-admin sessions). This spec
 * closes the e2e gap where all /admin/* and /ops* routes were previously
 * covered only by vitest fetch-mocks, never a real browser render.
 *
 * Assertions mirror sidebar.spec.ts: no 404 fallback, URL reflects the
 * route, document title set (RouteTitle ran), zero console errors
 * (API 5xx, lazy-chunk failures, mount crashes all surface there).
 *
 * Authz model asserted by 'session admin posture': the §8.2 Wrap gate
 * resolves the admin_role_bindings row (GET /admin/audit → 200), while a
 * subset of surfaces (flags, ip-bans, fees, chargebacks…) carry
 * requireAdmin's venue-side `admin` JWT scope that interactive sessions
 * never receive — those panels render AccessDeniedCard and the 403s are
 * tolerated in support.ts, pinned by that probe so a broken binding can
 * never hide behind them.
 *
 * Fixture: admin@exc.local / Password123! seeded by `seeddev`-adjacent
 * `services/bin/seedadmin` (Super Admin binding, idempotent).
 */

const ADMIN_PATHS = [
  // Mirrors features/admin*/nav.ts + features/ops/nav.ts — the 'Admin'
  // section order is: Admin → Ops Board/Fleet/Releases → Ops Safety →
  // Compliance → Customers → Surveillance → Funding Ops → Treasury →
  // Settlement → Finance → Instruments → Market Making → Reg Reporting →
  // Venue Governance → Conduct → Content.
  { label: 'Admin', path: '/admin' },
  { label: 'Ops Board', path: '/ops' },
  { label: 'Fleet', path: '/ops/fleet' },
  { label: 'Releases', path: '/ops/releases' },
  { label: 'Ops Safety', path: '/admin/ops-safety' },
  { label: 'Compliance', path: '/admin/compliance' },
  { label: 'Customers', path: '/admin/customers' },
  { label: 'Integrity', path: '/admin/integrity' },
  { label: 'Surveillance', path: '/admin/surveillance' },
  { label: 'Funding Ops', path: '/admin/funding-ops' },
  { label: 'Treasury', path: '/admin/treasury' },
  { label: 'Settlement', path: '/admin/settlement' },
  { label: 'Finance', path: '/admin/finance' },
  { label: 'Instruments', path: '/admin/instrument-governance' },
  { label: 'Market Making', path: '/admin/market-making' },
  { label: 'Reg Reporting', path: '/admin/regreporting' },
  { label: 'Venue Governance', path: '/admin/venue' },
  { label: 'Conduct', path: '/admin/conduct' },
  { label: 'Content', path: '/admin/content' },
];

test.describe('admin console', () => {
  test.beforeEach(async ({ page }) => {
    await uiLogin(page, ADMIN.email, ADMIN.password);
  });

  test('session admin posture: binding resolves, venue-side scope gate holds', async () => {
    // Same credential path the UI uses — proves the fixture is a working
    // admin credential and that we did NOT weaken authz to get green.
    const tok = await apiLogin(ADMIN.email, ADMIN.password);
    const api = await request.newContext({
      baseURL: API,
      extraHTTPHeaders: { ...HARNESS_XFF, Authorization: `Bearer ${tok}` },
    });
    try {
      // §8.2 Wrap admits: Super Admin binding on admin_role_bindings.
      const audit = await api.get('/api/v1/admin/audit');
      expect(
        audit.status(),
        '/admin/audit must serve — Super Admin binding unresolved',
      ).toBe(200);

      // requireAdmin's venue-side `admin` scope is intentionally absent
      // from session JWTs — 403 FORBIDDEN is the designed verdict, not a
      // fixture defect. If this ever returns 2xx on a session token, the
      // scope separation collapsed.
      const flags = await api.get('/api/v1/admin/flags');
      expect(flags.status()).toBe(403);
      const flagsBody = await flags.json();
      expect(flagsBody.error ?? flagsBody.code).toBe('FORBIDDEN');
    } finally {
      await api.dispose();
    }
  });

  test('Admin nav section renders only for admin sessions', async ({ page }) => {
    await page.goto('/');
    const nav = page.getByRole('navigation', { name: 'Primary' });
    await expect(nav).toBeVisible();
    for (const { label, path } of ADMIN_PATHS) {
      const link = nav.getByRole('link', { name: label, exact: true });
      await expect(link, `admin sidebar missing link "${label}"`).toBeVisible();
      await expect(link).toHaveAttribute('href', path);
    }
  });

  test('every admin path renders without errors', async ({ page }) => {
    test.setTimeout(180_000); // 19 route mounts on a live stack
    const consoleErrors = watchErrors(page);
    // 503s on /api/v1/admin/* are collected for post-traversal
    // re-verification: the gateway mounts a designed fail-closed
    // SERVICE_DEGRADED shim ("route handler not wired in this binary")
    // when an optional dependency (WORM object store, tax venue identity)
    // is absent — see gateway.unwiredHandler. A 503 that ISN'T that shim
    // must fail, so each is re-probed after the loop.
    const degradedUrls = new Set<string>();
    const isAdminShim503 = (e: string) => /^503 \S*\/api\/v1\/admin\//.test(e);
    // Failures accumulate across the whole sweep — one run reports every
    // broken route instead of aborting on the first (enum-cast drift and
    // absent-dep shims tend to cluster).
    const routeFailures: string[] = [];

    for (const { label, path } of ADMIN_PATHS) {
      consoleErrors.length = 0;
      await page.goto(path);

      // No 404 fallback (SPA renders NotFound for unknown routes).
      if (await page.getByText('Page not found', { exact: false }).count()) {
        routeFailures.push(`${label} (${path}) hit 404 fallback`);
      }

      // The route is reflected in the URL (no redirect loop / RequireAdmin bounce).
      try {
        await expect(page).toHaveURL(new RegExp(`${escapeRegex(path)}$`), {
          timeout: 5000,
        });
      } catch {
        routeFailures.push(`${label} (${path}) bounced to ${page.url()}`);
      }

      // Document title is set (proves the RouteTitle effect ran).
      try {
        await expect(page).toHaveTitle(/.+ — Exchange/, { timeout: 5000 });
      } catch {
        routeFailures.push(`${label} (${path}) never set a document title`);
      }

      const hard = consoleErrors.filter((e) => !isAdminShim503(e));
      for (const e of consoleErrors.filter(isAdminShim503)) {
        degradedUrls.add(e.slice(e.indexOf(' ') + 1));
      }
      if (hard.length > 0) {
        routeFailures.push(`${label} (${path}):\n  ${hard.join('\n  ')}`);
      }
    }

    expect(
      routeFailures,
      `admin routes with defects:\n${routeFailures.join('\n')}`,
    ).toHaveLength(0);

    // Re-probe every 503'd admin endpoint: only the designed not-wired
    // shim (error=SERVICE_DEGRADED carrying the owning task) is tolerated.
    if (degradedUrls.size > 0) {
      const tok = await apiLogin(ADMIN.email, ADMIN.password);
      const api = await request.newContext({
        baseURL: API,
        extraHTTPHeaders: { ...HARNESS_XFF, Authorization: `Bearer ${tok}` },
      });
      try {
        for (const url of degradedUrls) {
          const u = new URL(url);
          const res = await api.get(u.pathname + u.search);
          const body = await res.json().catch(() => ({}));
          expect(
            res.status() === 503 && body.error === 'SERVICE_DEGRADED',
            `${u.pathname} returned ${res.status()} ${JSON.stringify(body)} — ` +
              `expected the designed not-wired SERVICE_DEGRADED shim`,
          ).toBeTruthy();
        }
      } finally {
        await api.dispose();
      }
    }
  });

  test('venue-side gated panels degrade to AccessDeniedCard, not a crash', async ({ page }) => {
    // /admin/ops-safety mounts FlagsPanel + IpSecurityPanel — both call
    // requireAdmin-gated endpoints and must render the designed denial
    // card (data-testid="access-denied") instead of an error boundary.
    await page.goto('/admin/ops-safety');
    await expect(page).toHaveTitle(/.+ — Exchange/);
    await expect(page.getByTestId('access-denied').first()).toBeVisible();
  });
});

function escapeRegex(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
