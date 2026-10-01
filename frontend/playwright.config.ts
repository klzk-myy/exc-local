import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright e2e (Phase-10 Task 10.3.1 item 6). `smoke.spec.ts` is the
 * Wave-1 shell probe; `trade.spec.ts` is the Wave-2 smoke path —
 * login → subscribe → place order → offsetting close — run against the
 * live dev stack (gateway :8080 + engine shm + PG/Redis/NATS), with the
 * settlement leg asserted on the Balances surface (PHYSICAL_DELIVERY).
 * The CI job for the live-stack spec is still Wave-3 scope.
 */
export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  retries: process.env['CI'] ? 1 : 0,
  reporter: process.env['CI'] ? 'github' : 'list',
  use: {
    baseURL: process.env['E2E_BASE_URL'] ?? 'http://localhost:4173',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: 'npm run preview',
    url: 'http://localhost:4173',
    reuseExistingServer: !process.env['CI'],
  },
});
