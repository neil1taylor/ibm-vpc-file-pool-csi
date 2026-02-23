import { test, expect } from '@playwright/test';
import { ROUTES, POOLS_COLUMNS } from '../support/constants';
import { navigateTo, clickTab, expectTableHasRows, checkSessionValid } from '../support/pages';

test.describe('Pool Table Column Alignment', () => {
  test.beforeEach(async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
    await clickTab(page, 'Pools');
    await expectTableHasRows(page);
  });

  test('header and data cells are aligned within 2px tolerance', async ({ page }) => {
    const table = page.locator('table').first();
    const headerCells = table.locator('thead tr th');
    const firstDataRow = table.locator('tbody tr').first();
    const dataCells = firstDataRow.locator('td');

    const headerCount = await headerCells.count();
    const dataCount = await dataCells.count();

    // Both should have the same number of columns
    expect(headerCount).toBeGreaterThan(0);
    expect(dataCount).toBe(headerCount);

    for (let i = 0; i < headerCount; i++) {
      const thBox = await headerCells.nth(i).boundingBox();
      const tdBox = await dataCells.nth(i).boundingBox();

      // Skip if either cell is hidden (e.g. zero-width action column)
      if (!thBox || !tdBox) continue;

      // Left edges must align within 2px
      expect(
        Math.abs(thBox.x - tdBox.x),
        `Column ${i} left edge: th.x=${thBox.x} td.x=${tdBox.x}`,
      ).toBeLessThanOrEqual(2);

      // Right edges must align within 2px
      const thRight = thBox.x + thBox.width;
      const tdRight = tdBox.x + tdBox.width;
      expect(
        Math.abs(thRight - tdRight),
        `Column ${i} right edge: th.right=${thRight} td.right=${tdRight}`,
      ).toBeLessThanOrEqual(2);
    }
  });

  test('capacity cell content does not overflow into PVCs cell', async ({ page }) => {
    const table = page.locator('table').first();
    const headerCells = table.locator('thead tr th');

    // Find the Capacity and PVCs column indices
    const headerTexts: string[] = [];
    const headerCount = await headerCells.count();
    for (let i = 0; i < headerCount; i++) {
      headerTexts.push(await headerCells.nth(i).innerText());
    }
    const capacityIdx = headerTexts.findIndex((t) => t.includes('Capacity'));
    const pvcsIdx = headerTexts.findIndex((t) => t.includes('PVCs'));

    expect(capacityIdx).toBeGreaterThanOrEqual(0);
    expect(pvcsIdx).toBeGreaterThan(capacityIdx);

    // Check every visible data row
    const dataRows = table.locator('tbody tr');
    const rowCount = await dataRows.count();

    for (let r = 0; r < Math.min(rowCount, 10); r++) {
      const row = dataRows.nth(r);
      const capacityCell = row.locator('td').nth(capacityIdx);
      const pvcsCell = row.locator('td').nth(pvcsIdx);

      const capBox = await capacityCell.boundingBox();
      const pvcsBox = await pvcsCell.boundingBox();
      if (!capBox || !pvcsBox) continue;

      // The capacity cell's right edge should not exceed the PVCs cell's left edge
      const capRight = capBox.x + capBox.width;
      expect(
        capRight,
        `Row ${r}: capacity right (${capRight}) must not exceed PVCs left (${pvcsBox.x})`,
      ).toBeLessThanOrEqual(pvcsBox.x + 1); // 1px rounding tolerance
    }
  });
});
