import { test, expect } from '@playwright/test';
import {
  ROUTES,
  TAB_NAMES,
  DASHBOARD_STAT_CARDS,
  DASHBOARD_PERF_CARDS,
  DASHBOARD_CAPACITY_CARDS,
  MONITORING_STAT_CARDS,
  MONITORING_CHART_CARDS,
  POOLS_COLUMNS,
  SUBVOLUMES_COLUMNS,
  RECENT_ACTIVITY_COLUMNS,
  TIME_RANGE_LABELS,
  SELECTORS,
} from '../support/constants';
import {
  navigateTo,
  expectPageTitle,
  clickTab,
  expectCards,
  expectTableColumns,
  expectTableHasRows,
  expectTableOrEmptyState,
  getTimeRangeButton,
  checkSessionValid,
} from '../support/pages';

test.describe('Plugin Dashboard (Overview)', () => {
  test.beforeEach(async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
  });

  test('page title is visible', async ({ page }) => {
    await expectPageTitle(page, 'IBM VPC File Pools');
  });

  test('all 7 tabs are present', async ({ page }) => {
    for (const tabName of TAB_NAMES) {
      await expect(page.getByRole('tab', { name: tabName })).toBeVisible();
    }
  });

  test('stat cards are visible', async ({ page }) => {
    await expectCards(page, DASHBOARD_STAT_CARDS);
  });

  test('performance cards are visible', async ({ page }) => {
    await expectCards(page, DASHBOARD_PERF_CARDS);
  });

  test('capacity cards are visible', async ({ page }) => {
    await expectCards(page, DASHBOARD_CAPACITY_CARDS);
  });

  test('recent activity table has correct columns', async ({ page }) => {
    await expectTableColumns(page, RECENT_ACTIVITY_COLUMNS, SELECTORS.recentActivityTable);
  });
});

test.describe('Pools Tab', () => {
  test.beforeEach(async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
    await clickTab(page, 'Pools');
  });

  test('table has correct column headers', async ({ page }) => {
    await expectTableColumns(page, POOLS_COLUMNS);
  });

  test('at least one pool row is visible', async ({ page }) => {
    await expectTableHasRows(page);
  });

  test('pool name is a link', async ({ page }) => {
    const firstPoolLink = page.locator('table tbody tr').first().locator('td').first().locator('a');
    await expect(firstPoolLink).toBeVisible();
  });

  test('phase badges are present', async ({ page }) => {
    await expect(page.locator(SELECTORS.phaseLabel).first()).toBeVisible();
  });
});

test.describe('SubVolumes Tab', () => {
  test.beforeEach(async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
    await clickTab(page, 'SubVolumes');
  });

  test('table has correct column headers', async ({ page }) => {
    await expectTableColumns(page, SUBVOLUMES_COLUMNS);
  });
});

test.describe('Snapshots Tab', () => {
  test('page loads without error', async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
    await clickTab(page, 'Snapshots');
    await expectTableOrEmptyState(page);
  });
});

test.describe('Group Snapshots Tab', () => {
  test('page loads without error', async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
    await clickTab(page, 'Group Snapshots');
    await expectTableOrEmptyState(page);
  });
});

test.describe('Replication Tab', () => {
  test('page loads without error', async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
    await clickTab(page, 'Replication');
    await expectTableOrEmptyState(page);
  });
});

test.describe('Monitoring Tab', () => {
  test.beforeEach(async ({ page }) => {
    await navigateTo(page, ROUTES.dashboard);
    await checkSessionValid(page);
    await clickTab(page, 'Monitoring');
  });

  test('time range toggles are visible with 1h selected by default', async ({ page }) => {
    for (const label of TIME_RANGE_LABELS) {
      await expect(getTimeRangeButton(page, label)).toBeVisible();
    }
    // 1h should be the default selected toggle
    const btn1h = getTimeRangeButton(page, '1h');
    await expect(btn1h).toHaveAttribute('aria-pressed', 'true');
  });

  test('clicking 24h changes selected state', async ({ page }) => {
    const btn24h = getTimeRangeButton(page, '24h');
    await btn24h.click();
    await expect(btn24h).toHaveAttribute('aria-pressed', 'true');
    // 1h should no longer be selected
    const btn1h = getTimeRangeButton(page, '1h');
    await expect(btn1h).toHaveAttribute('aria-pressed', 'false');
  });

  test('stat cards are visible', async ({ page }) => {
    await expectCards(page, MONITORING_STAT_CARDS);
  });

  test('pool capacity overview card is visible', async ({ page }) => {
    await expectCards(page, ['Pool Capacity Overview']);
  });

  test('chart cards are visible', async ({ page }) => {
    await expectCards(page, MONITORING_CHART_CARDS);
  });

  test('expandable allocation activity section works', async ({ page }) => {
    const expandButton = page.getByRole('button', { name: /Show Allocation Activity/i });
    await expect(expandButton).toBeVisible();
    await expandButton.click();
    await expectCards(page, ['Allocation Rate Over Time']);
  });
});
