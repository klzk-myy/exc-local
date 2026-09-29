import { expect, test } from '@playwright/test';

/**
 * Wave-1 placeholder spec — proves the harness, preview server, and SPA
 * shell respond. Wave-2 replaces this with the Task 10.3.1 acceptance
 * smoke path: login → subscribe → place order → close position.
 */
test.describe('smoke', () => {
  test('SPA shell renders', async ({ page }) => {
    await page.goto('/');
    await expect(page).toHaveTitle(/exchange/i);
    await expect(page.locator('#root')).not.toBeEmpty();
  });
});
