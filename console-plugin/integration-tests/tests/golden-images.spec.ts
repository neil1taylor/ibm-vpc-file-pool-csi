import { test, expect } from '@playwright/test';
import { GOLDEN_IMAGE_DATASOURCES, dataSourceRoute } from '../support/constants';
import { navigateTo, checkSessionValid } from '../support/pages';

test.describe('Golden Image DataSources', () => {
  for (const dsName of GOLDEN_IMAGE_DATASOURCES) {
    test(`${dsName} DataSource is present and ready`, async ({ page }) => {
      await navigateTo(page, dataSourceRoute(dsName));
      await checkSessionValid(page);

      // Assert the DataSource name is displayed on the detail page
      await expect(
        page.locator('[data-test-id="resource-title"], h1, [class*="resource-title"]')
          .filter({ hasText: dsName })
      ).toBeVisible({ timeout: 30_000 });

      // Assert a "Ready" condition or availability indicator is present
      // DataSource detail pages show conditions — look for a Ready status
      const readyIndicator = page
        .locator('.pf-v6-c-label, [class*="label"], [class*="badge"], [class*="status"], td')
        .filter({ hasText: /Ready|True|Available/i });
      await expect(readyIndicator.first()).toBeVisible({ timeout: 15_000 });
    });
  }
});
