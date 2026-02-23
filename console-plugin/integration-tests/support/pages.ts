import { type Page, type Locator, expect } from '@playwright/test';
import { SELECTORS } from './constants';

/** Navigate to a plugin page and wait for it to load */
export async function navigateTo(page: Page, path: string): Promise<void> {
  await page.goto(path, { waitUntil: 'domcontentloaded' });
  // Wait for the page body to be present
  await page.locator('.pf-v6-c-page, .co-m-pane__body, [class*="page"]').first().waitFor({
    state: 'visible',
    timeout: 60_000,
  });
}

/** Assert the plugin page title is visible */
export async function expectPageTitle(page: Page, title: string): Promise<void> {
  await expect(
    page.locator(SELECTORS.pageTitle).filter({ hasText: title })
  ).toBeVisible({ timeout: 30_000 });
}

/** Click a tab by its accessible name */
export async function clickTab(page: Page, tabName: string): Promise<void> {
  const tab = page.getByRole('tab', { name: tabName });
  await tab.click();
  // Small wait for tab content to render
  await page.waitForTimeout(1_000);
}

/** Assert that a set of cards with the given titles are visible */
export async function expectCards(page: Page, titles: readonly string[]): Promise<void> {
  for (const title of titles) {
    await expect(
      page.locator(SELECTORS.cardTitle).filter({ hasText: title })
    ).toBeVisible({ timeout: 15_000 });
  }
}

/** Assert that a table has the expected column headers */
export async function expectTableColumns(page: Page, columns: readonly string[], tableSelector?: string): Promise<void> {
  const tableLocator = tableSelector ? page.locator(tableSelector) : page.locator('table').first();
  const headerRow = tableLocator.locator('thead tr').first();
  await headerRow.waitFor({ state: 'visible', timeout: 15_000 });

  for (const col of columns) {
    await expect(headerRow.locator('th').filter({ hasText: col })).toBeVisible();
  }
}

/** Assert that a table has at least one data row */
export async function expectTableHasRows(page: Page, tableSelector?: string): Promise<void> {
  const tableLocator = tableSelector ? page.locator(tableSelector) : page.locator('table').first();
  const rows = tableLocator.locator('tbody tr');
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });
}

/** Assert that either a table with rows or an empty state is present */
export async function expectTableOrEmptyState(page: Page): Promise<void> {
  const table = page.locator('table tbody tr');
  const emptyState = page.locator('.pf-v6-c-empty-state, [class*="empty-state"], [class*="EmptyState"]');

  // Wait for either a table row or an empty state component
  await expect(table.first().or(emptyState.first())).toBeVisible({ timeout: 30_000 });
}

/** Get a time range toggle button */
export function getTimeRangeButton(page: Page, label: string): Locator {
  return page.locator(SELECTORS.timeRangeGroup).getByText(label, { exact: true });
}

/** Check if the current URL is an IAM redirect (session expired) */
export async function checkSessionValid(page: Page): Promise<void> {
  const url = page.url();
  if (url.includes('iam.cloud.ibm.com') || url.includes('identity-') || url.includes('/login')) {
    throw new Error(
      'Session expired — IBM Cloud IAM redirect detected.\n' +
      'Re-run auth setup: make console-plugin-test-e2e-setup'
    );
  }
}
