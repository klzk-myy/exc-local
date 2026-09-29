import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright e2e scaffold (Phase-10 Task 10.3.1 item 6).
 * Wave-1 ships the harness + a placeholder smoke spec only; the real
 * smoke path (login → subscribe → place order → close position) and its
 * dedicated CI job land in Wave-2 alongside the feature surfaces.
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
