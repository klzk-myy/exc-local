import { expect, request, test, type APIRequestContext, type Page } from '@playwright/test';

/**
 * Wave-2 acceptance smoke path (Task 10.3.1): login → subscribe → place
 * order → close position, exercised end-to-end against the real stack
 * (vite dev proxy → gateway :8080 → matching_engine shard 0 via shm IPC,
 * PostgreSQL, Redis, NATS) with the settlement FillConsumer wired to the
 * gateway's sole out-ring reader (frame tap → settlement.FuncSource).
 *
 * Product semantics: EUR/USD is a deliverable SPOT pair and the seeded
 * accounts carry settlement_intent=PHYSICAL_DELIVERY, so a fill does NOT
 * mint a `positions` row — it segregates each side's deliverable
 * (available→locked, GL 2010→2011_PENDING_SETTLEMENT_DELIVERY) ahead of
 * T+2 rail settlement. "Close position" therefore means the offsetting
 * trade: after the round trip the taker holds both legs segregated
 * pending delivery, which the test asserts on the Balances table.
 *
 * Fixture contract (bring-up: register both users via the API, then
 * `services/bin/seeddev -reset` funds through real DEPOSIT journals and
 * sets kyc T1 + PHYSICAL_DELIVERY):
 *   taker e2e.taker@example.com / maker e2e.maker@example.com,
 *   password E2e-passphrase-9.
 * beforeEach re-seeds the book through the real order API so the spec is
 * deterministic against a live (WAL-recovered) engine book.
 */

const TAKER = { email: 'e2e.taker@example.com', password: 'E2e-passphrase-9' };
const MAKER = { email: 'e2e.maker@example.com', password: 'E2e-passphrase-9' };
const SYMBOL = 'EUR/USD';
const SYMBOL_URL = `/trade/${encodeURIComponent(SYMBOL)}`;
// Test-harness calls bypass the vite proxy and carry their own XFF — the
// gateway keys §8.8 edge buckets on client IP (EXC_TRUST_PROXY=1), so the
// browser (proxied as 10.90.0.1) and harness (10.90.0.2) never compete.
const API = 'http://127.0.0.1:8080';
const HARNESS_XFF = { 'x-forwarded-for': '10.90.0.2' };

async function apiLogin(email: string, password: string): Promise<string> {
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

async function seedBook(): Promise<void> {
  const token = await apiLogin(MAKER.email, MAKER.password);
  const ctx = await request.newContext({
    baseURL: API,
    extraHTTPHeaders: { Authorization: `Bearer ${token}`, ...HARNESS_XFF },
  });
  // Clear the maker's resting orders ON THIS SYMBOL, then rest both sides.
  // The engine book is WAL-persistent — without this, leftover depth from
  // prior runs makes level assertions non-deterministic. Must use the
  // symbol-scoped endpoint (DELETE /orders?symbol=), NOT /orders/all: the
  // all-instruments variant ignores ?symbol= and would try to cancel stale
  // orders resting on shards with no running engine in the dev stack,
  // timing out the whole seed (fail-closed GATEWAY_TIMEOUT_MATCHING_ENGINE).
  const cancel = await ctx.delete(`/api/v1/orders?symbol=${encodeURIComponent(SYMBOL)}`);
  expect(cancel.ok(), `mass cancel ${cancel.status()}`).toBeTruthy();
  for (const [side, price] of [
    ['BUY', '1.09990'],
    ['SELL', '1.10010'],
  ] as const) {
    const res = await ctx.post('/api/v1/orders', {
      data: {
        symbol: SYMBOL,
        side,
        type: 'LIMIT',
        price,
        quantity: '1000',
        time_in_force: 'GTC',
      },
    });
    expect(res.ok(), `seed ${side} ${res.status()}: ${await res.text()}`).toBeTruthy();
  }
  await ctx.dispose();
}

interface Wallet {
  free: number;
  used: number;
  total: number;
}

/** Bearer token minted by the UI login — mirrors the session-store read
 * order (sessionStorage wins over localStorage, src/lib/auth/session.ts). */
async function pageToken(page: Page): Promise<string> {
  const token = await page.evaluate(() => {
    const raw =
      window.sessionStorage.getItem('exc.session.v1') ??
      window.localStorage.getItem('exc.session.v1');
    if (!raw) return null;
    try {
      return (JSON.parse(raw) as { accessToken?: string }).accessToken ?? null;
    } catch {
      return null;
    }
  });
  expect(token, 'SPA session token after login').toBeTruthy();
  return token as string;
}

function balancesCtx(token: string) {
  return request.newContext({
    baseURL: API,
    extraHTTPHeaders: { Authorization: `Bearer ${token}`, ...HARNESS_XFF },
  });
}

/** Wallet row via the same REST surface the SPA reads — used for settle
 * polling so waiting on the async fill→journal leg doesn't burn page
 * mounts against the browser's edge bucket. */
async function apiBalance(api: APIRequestContext, currency: string): Promise<Wallet> {
  const res = await api.get('/api/v1/account/balances');
  expect(res.ok(), `balances ${res.status()}`).toBeTruthy();
  const body = (await res.json()) as {
    balances: { currency: string; available: string; locked: string; total: string }[];
  };
  const row = body.balances.find((b) => b.currency === currency);
  expect(row, `balance row ${currency}`).toBeTruthy();
  return {
    free: Number(row!.available),
    used: Number(row!.locked),
    total: Number(row!.total),
  };
}

function pollApiBalance(api: APIRequestContext, currency: string, cell: 'free' | 'used') {
  return expect.poll(async () => (await apiBalance(api, currency))[cell], { timeout: 30_000 });
}

/** Read a Balances row's Free/Used cells as numbers ("1,100.10" → 1100.10).
 * Throws after ~4s if the row isn't rendered — callers wrap in expect.poll
 * so a transiently rate-limited mount retries instead of hanging. */
async function balanceRow(page: Page, currency: string) {
  const row = page
    .getByLabel('Account balances')
    .getByRole('row')
    .filter({ has: page.getByRole('cell', { name: currency, exact: true }) })
    .first();
  await row.waitFor({ state: 'attached', timeout: 4_000 });
  const cells = row.getByRole('cell');
  const parse = async (i: number) => Number((await cells.nth(i).textContent())?.replace(/,/g, ''));
  return { free: await parse(1), used: await parse(2), total: await parse(3) };
}

/** Read the row from the mounted page first (the caller may already be
 * looking at it), reloading only if the mount came up empty — Portfolio
 * seeds via REST once per mount, so a rate-limited mount renders an
 * empty table until the next mount. */
async function readBalance(page: Page, currency: string) {
  try {
    return await balanceRow(page, currency);
  } catch {
    /* not rendered — fall through to reload-poll */
  }
  let latest: Wallet | undefined;
  await expect
    .poll(
      async () => {
        await page.reload();
        latest = await balanceRow(page, currency);
        return true;
      },
      { timeout: 45_000 },
    )
    .toBe(true);
  return latest!;
}

test.describe('smoke path', () => {
  test.setTimeout(120_000); // two real round trips + settle-poll reloads
  test.beforeEach(async () => {
    await seedBook();
  });

  test('login → subscribe → place order → close position', async ({ page }) => {
    // ── 1. login ──────────────────────────────────────────────────────
    await page.goto('/login');
    await page.getByLabel('Email').fill(TAKER.email);
    await page.getByLabel('Password').fill(TAKER.password);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page).not.toHaveURL(/\/login/, { timeout: 15_000 });

    // REST context over the token the SPA minted — settle-state polling
    // rides the harness bucket, not the browser's (see HARNESS_XFF).
    const api = await balancesCtx(await pageToken(page));

    // ── 2. subscribe — the book renders WS depth frames ───────────────
    // /trade/:symbol redirects into the workspace carrying the symbol in
    // the order draft; the taker is T1 → Lite by default, so switch to
    // Pro for the full ticket + book panel.
    await page.goto(SYMBOL_URL);
    await expect(page).toHaveURL(/\/workspace$/, { timeout: 15_000 });
    await page.getByRole('button', { name: /^pro/ }).click();
    await expect(page.getByRole('rowgroup', { name: /depth$/ }).first()).toBeVisible({
      timeout: 15_000,
    });
    await expect(page.getByLabel('ask 1.10010000')).toBeVisible();
    await expect(page.getByLabel('bid 1.09990000')).toBeVisible();

    // Baseline wallet state (balances accumulate across runs — assert deltas).
    const usdBefore = await apiBalance(api, 'USD');

    // ── 3. place order — BUY LIMIT crosses the seeded ask ─────────────
    const entry = page.getByLabel('Advanced order entry');
    await entry.getByRole('button', { name: 'BUY', exact: true }).click();
    await entry.getByLabel('Order type').selectOption('LIMIT');
    await entry.getByLabel('Price').fill('1.10010');
    await entry.getByLabel('Quantity').fill('1000');
    await entry.getByRole('button', { name: 'Submit Limit' }).click();
    await expect(page.getByText(/Order accepted/i)).toBeVisible({ timeout: 15_000 });

    // Settlement proof (PHYSICAL_DELIVERY): 1000 EUR/USD @1.10010 moves
    // 1,100.10 USD available→locked pending T+2 delivery. REST waits for
    // the async journal leg; the Balances table then renders it.
    await pollApiBalance(api, 'USD', 'used').toBeCloseTo(usdBefore.used + 1100.1, 2);
    await page.goto('/portfolio');
    const usdAfterBuy = await readBalance(page, 'USD');
    expect(usdAfterBuy.used).toBeCloseTo(usdBefore.used + 1100.1, 2);
    expect(usdAfterBuy.free).toBeCloseTo(usdBefore.free - 1100.1, 2);
    expect(usdAfterBuy.total).toBeCloseTo(usdBefore.total, 2);
    const eurBefore = await readBalance(page, 'EUR');

    // ── 4. close — the offsetting SELL fills against the seeded bid ───
    await entry.getByRole('button', { name: 'SELL', exact: true }).click();
    await entry.getByLabel('Price').fill('1.09990');
    await entry.getByLabel('Quantity').fill('1000');
    await entry.getByRole('button', { name: 'Submit Limit' }).click();
    await expect(page.getByText(/Order accepted/i).last()).toBeVisible({ timeout: 15_000 });

    // The sell leg segregates the 1,000 EUR deliverable — both legs now
    // sit pending settlement, i.e. the spot exposure is closed out.
    await pollApiBalance(api, 'EUR', 'used').toBeCloseTo(eurBefore.used + 1000, 2);
    await page.goto('/portfolio');
    const eurAfterSell = await readBalance(page, 'EUR');
    expect(eurAfterSell.used).toBeCloseTo(eurBefore.used + 1000, 2);
    await api.dispose();
  });
});
