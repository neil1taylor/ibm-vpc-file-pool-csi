import { test } from '@playwright/test';
import path from 'path';
import { ROUTES } from '../support/constants';
import { navigateTo, checkSessionValid } from '../support/pages';

// Saves one screenshot of each plugin tab, for the release test report.
const SCREENSHOT_DIR = process.env.SCREENSHOT_DIR || path.join('..', 'test-results', 'screenshots');

test.describe('Tab screenshots', () => {
  for (const [name, route] of Object.entries(ROUTES)) {
    test(`screenshot of the ${name} tab`, async ({ page }) => {
      await navigateTo(page, route);
      await checkSessionValid(page);
      await page.waitForLoadState('networkidle').catch(() => {});
      await page.screenshot({ path: path.join(SCREENSHOT_DIR, `${name}.png`), fullPage: true });
    });
  }
});
