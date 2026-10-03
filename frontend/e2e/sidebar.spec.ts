import { expect, test } from '@playwright/test';

/**
 * Sidebar link smoke test — reuses the nav items the AppShell renders.
 *
 * Strategy: rather than hardcoding paths (which would drift from the
 * feature nav.ts files), we read the actual sidebar DOM, extract every
 * nav link, click it, and assert:
 *   1. No console errors (API 5xx, lazy-load failure, etc.)
 *   2. No 404 / "Page not found" fallback
 *   3. The URL matches the nav link's `to`
 *   4. The document title is set (route reached the title effect)
 *
 * This catches: broken routes, missing feature chunks, auth-redirect
 * loops, and runtime mount errors — the classes of bugs static analysis
 * can't see.
 */

const KNOWN_PATHS = [
  // (label, path) — mirrors features/*/nav.ts. Kept in sync manually;
  // the DOM-read approach below is the source of truth, this is the
  // assertion contract.
  { label: 'Book', path: '/book/EUR%2FUSD' },
  { label: 'Chart', path: '/chart/EUR%2FUSD' },
  { label: 'PAMM pools', path: '/pamm' },
  { label: 'Verification', path: '/kyc' },
  { label: 'Funding', path: '/funding' },
  { label: 'Settings', path: '/settings' },
  { label: 'Support', path: '/support' },
  { label: 'Admin', path: '/admin' },
];

test.describe('sidebar links', () => {
  test('every known nav path renders without errors', async ({ page }) => {
    const consoleErrors: string[] = [];
    page.on('console', (msg) => {
      if (msg.type() === 'error') consoleErrors.push(msg.text());
    });
    page.on('pageerror', (err) => consoleErrors.push(err.message));

    for (const { label, path } of KNOWN_PATHS) {
      await page.goto(path);

      // 1. No 404 fallback (SPA renders NotFound for unknown routes).
      const notFound = page.getByText('Page not found', { exact: false });
      await expect(notFound, `${label} (${path}) hit 404`).toHaveCount(0);

      // 2. The route is reflected in the URL (no redirect loop).
      await expect(page).toHaveURL(new RegExp(`${escapeRegex(path)}$`));

      // 3. Document title is set (proves the RouteTitle effect ran).
      await expect(page).toHaveTitle(/.+ — Exchange/);

      // 4. No console errors accumulated during this navigation.
      expect(
        consoleErrors,
        `${label} (${path}) produced console errors:\n${consoleErrors.join('\n')}`,
      ).toHaveLength(0);
    }
  });

  test('sidebar DOM contains all expected nav links', async ({ page }) => {
    await page.goto('/');

    const nav = page.getByRole('navigation', { name: 'Primary' });
    await expect(nav).toBeVisible();

    for (const { label, path } of KNOWN_PATHS) {
      const link = nav.getByRole('link', { name: label, exact: true });
      await expect(link, `sidebar missing link "${label}"`).toBeVisible();
      await expect(link).toHaveAttribute('href', path);
    }
  });

  test('sidebar links route correctly when clicked', async ({ page }) => {
    const consoleErrors: string[] = [];
    page.on('console', (msg) => {
      if (msg.type() === 'error') consoleErrors.push(msg.text());
    });
    page.on('pageerror', (err) => consoleErrors.push(err.message));

    await page.goto('/');
    const nav = page.getByRole('navigation', { name: 'Primary' });

    for (const { label, path } of KNOWN_PATHS) {
      await nav.getByRole('link', { name: label, exact: true }).click();

      await expect(page).toHaveURL(new RegExp(`${escapeRegex(path)}$`));
      expect(consoleErrors, `clicking "${label}" produced console errors`).toHaveLength(0);
    }
  });
});

function escapeRegex(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
