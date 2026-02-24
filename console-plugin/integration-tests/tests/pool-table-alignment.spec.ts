import { test, expect } from '@playwright/test';
import { ROUTES } from '../support/constants';
import { navigateTo, clickTab, checkSessionValid } from '../support/pages';

test.describe('Pool Table Layout', () => {
  test.beforeEach(async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
    await clickTab(page, 'Pools');
    // Wait for pool data to load (PF Table renders rows as <tr> in <tbody>)
    await expect(page.locator('table[aria-label="File Share Pools"] tbody tr').first()).toBeVisible({ timeout: 30_000 });
  });

  test('all expected column headers are present', async ({ page }) => {
    const table = page.locator('table[aria-label="File Share Pools"]');
    const headerText = await table.locator('thead').innerText();

    for (const col of ['Name', 'Zone', 'Profile', 'IOPS', 'Capacity', 'Phase']) {
      expect(headerText, `Header should contain "${col}"`).toContain(col);
    }
  });

  test('table body rows render with data', async ({ page }) => {
    const table = page.locator('table[aria-label="File Share Pools"]');
    const rows = table.locator('tbody tr');
    const rowCount = await rows.count();

    expect(rowCount, 'Table should have at least one data row').toBeGreaterThanOrEqual(1);

    // First row should contain a link (pool name)
    const firstRowLink = rows.first().locator('a').first();
    await expect(firstRowLink).toBeVisible();
    const linkText = await firstRowLink.innerText();
    expect(linkText.length).toBeGreaterThan(0);
  });

  test('capacity cell renders progress bar', async ({ page }) => {
    const table = page.locator('table[aria-label="File Share Pools"]');
    const firstRow = table.locator('tbody tr').first();

    // Capacity column contains a Progress component
    const capacityCell = firstRow.locator('td[data-label="Capacity"]');
    await expect(capacityCell).toBeVisible();

    // Progress bar renders a <div role="progressbar">
    const progressBar = capacityCell.locator('[role="progressbar"]');
    await expect(progressBar).toBeVisible();
  });
});
