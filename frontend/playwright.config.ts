import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright e2e (Phase-10 Task 10.3.1 item 6). `smoke.spec.ts` is the
 * Wave-1 shell probe; `sidebar.spec.ts` covers nav; `trade.spec.ts` is
 * the Wave-2 smoke path — login → subscribe → place order → offsetting
 * close — run against the live dev stack (gateway :8080 + engine shm +
 * PG/Redis/NATS), with the settlement leg asserted on the Balances
 * surface (PHYSICAL_DELIVERY).
 *
 * The suite targets the DOCKER frontend (nginx :3000 → gateway :8080):
 * the app-under-test is always the containerized stack. The previous
 * `vite preview` default never proxied /api or /ws (server.proxy is
 * dev-server-only), so only the shell probe could ever pass through it.
 * Start the stack first:
 *   deploy/scripts/dev_stack.sh up
 * then run `npx playwright test` (E2E_BASE_URL overrides the target).
 */
const BASE_URL = process.env['E2E_BASE_URL'] ?? 'http://localhost:3000';

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  retries: process.env['CI'] ? 1 : 0,
  reporter: process.env['CI'] ? 'github' : 'list',
  use: {
    baseURL: BASE_URL,
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
