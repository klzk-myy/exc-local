import { expect, test } from '@playwright/test';
import { TAKER, uiLogin, watchErrors } from './support';

test('polish: embedded panels, no console errors, dark + light', async ({ page }) => {
  const errors = watchErrors(page);
  await uiLogin(page, TAKER.email, TAKER.password);
  await page.evaluate(() =>
    localStorage.setItem('exc.ui-mode.v1', JSON.stringify({ state: { mode: 'pro' }, version: 0 })));
  await page.goto('/workspace');
  await page.waitForSelector('[role="application"]', { timeout: 15000 });
  await page.waitForTimeout(800);
  await page.screenshot({ path: '/tmp/polish-dark.png', fullPage: true });

  // light theme round-trip
  await page.evaluate(() =>
    localStorage.setItem('exc.theme.v1', JSON.stringify({ state: { theme: 'light' }, version: 0 })));
  await page.reload();
  await page.waitForSelector('[role="application"]', { timeout: 15000 });
  await page.waitForTimeout(800);
  await page.screenshot({ path: '/tmp/polish-light.png', fullPage: true });

  expect(errors.filter((e) => !/favicon|Download the React DevTools/.test(e))).toEqual([]);
});
