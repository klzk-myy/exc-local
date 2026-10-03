import { expect, test } from '@playwright/test';
import { TAKER, uiLogin } from './support';

test('light theme: market-order banner + lite-switch affordance', async ({ page }) => {
  await uiLogin(page, TAKER.email, TAKER.password);
  await page.evaluate(() => {
    localStorage.setItem('exc.ui-mode.v1', JSON.stringify({ state: { mode: 'pro' }, version: 0 }));
    localStorage.setItem('exc.theme.v1', JSON.stringify({ state: { theme: 'light' }, version: 0 }));
  });
  await page.goto('/workspace');
  await page.waitForSelector('[role="application"]', { timeout: 15000 });
  // Flip the ticket to MARKET → the slippage banner renders.
  await page.getByLabel('Order type').selectOption('MARKET');
  await expect(page.getByText(/slippage limits apply/)).toBeVisible();
  await page.screenshot({ path: '/tmp/light-banner.png', fullPage: true });

  await page.setViewportSize({ width: 480, height: 800 });
  const liteBtn = page.getByRole('button', { name: 'switch to Lite mode' });
  await expect(liteBtn).toBeVisible();
  await page.screenshot({ path: '/tmp/narrow-lite-btn.png' });
  await liteBtn.click();
  await expect(page.getByRole('application')).toBeHidden();
});
