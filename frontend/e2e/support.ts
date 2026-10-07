import { expect, request, type Page } from '@playwright/test';

/**
 * Shared e2e fixtures — the one place the harness talks to the live
 * stack. Test-harness API calls bypass the SPA and carry their own
 * X-Forwarded-For so the gateway keys §8.8 edge buckets on a distinct
 * client IP (EXC_TRUST_PROXY=1): the browser (proxied as 10.90.0.1)
 * and harness (10.90.0.2) never compete for rate-limit budget.
 */
export const API = 'http://127.0.0.1:8080';
export const HARNESS_XFF = { 'x-forwarded-for': '10.90.0.2' };

export const TAKER = { email: 'e2e.taker@example.com', password: 'E2e-passphrase-9' };
export const MAKER = { email: 'e2e.maker@example.com', password: 'E2e-passphrase-9' };
// Seeded by services/cmd/seedadmin (Super Admin binding, idempotent).
export const ADMIN = { email: 'admin@exc.local', password: 'Password123!' };

/** Bearer token for harness-side REST calls (seed/teardown, not UI). */
export async function apiLogin(email: string, password: string): Promise<string> {
  const ctx = await request.newContext({
    baseURL: API,
    extraHTTPHeaders: { ...HARNESS_XFF },
  });
  const res = await ctx.post('/api/v1/auth/login', { data: { email, password } });
  expect(res.ok(), `login ${email} ${res.status()}`).toBeTruthy();
  const body = await res.json();
  await ctx.dispose();
  return body.access_token as string;
}

/** Real UI login — exercises the form the same way a user would and
 * leaves the SPA session in storage for subsequent page.goto() calls. */
export async function uiLogin(page: Page, email: string, password: string): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(email);
  await page.getByLabel('Password').fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).not.toHaveURL(/\/login/, { timeout: 15_000 });
}

/**
 * Domain-level 404s the UI treats as empty state — NOT errors. Each
 * entry must name the endpoint whose 404 is a designed response:
 *   /deposits/{ccy} → NOT_FOUND when no nostro accounts are seeded for
 *   the currency (handler's empty-state contract, api/funding.go).
 */
const TOLERATED_404 = [/^\/api\/v1\/deposits\/[A-Z]{3}$/];

/**
 * 403s the §8.2 authz stack emits by design for a session-authenticated
 * admin: routes on the flags/ip-bans/fees/chargebacks/deprecation/fix /
 * cache-warm surfaces carry a SECOND gate — requireAdmin's `admin` JWT
 * scope check (services/internal/api/ipbans.go) — which is venue-side
 * only and never lands on user-session tokens (auth/registration.go
 * userSessionScopes). The pentest documents this as intended
 * defence-in-depth (tests/pentest/authz_test.go wrapDeny commentary:
 * "admin scope required" proves the §8.2 binding resolved) and the UI
 * renders AccessDeniedCard on it.
 *
 * Safe to tolerate ONLY for the Super Admin fixture whose binding is
 * control-probed in admin.spec.ts (GET /admin/audit → 200): a binding
 * failure would trip Wrap — "no active admin role binding" / a 403 on
 * every admin route — and the probe fails first rather than hiding here.
 */
const TOLERATED_403 = [/^\/api\/v1\/admin\//];

/** Install error watchers on a page and return the live error list.
 * Resource-load console lines ("Failed to load resource …") are skipped
 * in favour of response-level tracking, which carries the URL: a failed
 * lazy chunk still surfaces via pageerror + a non-2xx response, while a
 * designed domain 404 can be whitelisted by path instead of silencing
 * the whole class. */
export function watchErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (err) => errors.push(err.message));
  page.on('console', (msg) => {
    if (msg.type() === 'error' && !msg.text().startsWith('Failed to load resource')) {
      errors.push(msg.text());
    }
  });
  page.on('response', (res) => {
    const status = res.status();
    if (status < 400) return;
    const path = new URL(res.url()).pathname;
    if (status === 404 && TOLERATED_404.some((re) => re.test(path))) return;
    if (status === 403 && TOLERATED_403.some((re) => re.test(path))) return;
    errors.push(`${status} ${res.url()}`);
  });
  return errors;
}
