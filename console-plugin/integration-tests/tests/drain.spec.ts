import { test, expect } from '@playwright/test';
import { navigateTo, checkSessionValid, clickTab } from '../support/pages';

/**
 * Drain a share, and return it to service, on the pool page.
 *
 * This test changes a pool, so it runs only against a console that has a write token limited to that one pool
 * (release stage 16, drain part). Against any other console it is skipped. The stage runs it twice, with
 * DRAIN_PHASE=drain and then DRAIN_PHASE=return, and checks the pool object in between.
 *
 * DRAIN_POOL   the name of the pool
 * DRAIN_SHARE  the name of the share (the "Share Name" column)
 * DRAIN_PHASE  drain | return
 */
const POOL = process.env.DRAIN_POOL;
const SHARE = process.env.DRAIN_SHARE;
const PHASE = process.env.DRAIN_PHASE;

test.describe('Drain a share and return it to service', () => {
  test.skip(!POOL || !SHARE || !PHASE, 'needs a console with a write token for one pool (release stage 16)');
  test.setTimeout(300_000);
  // A second try would find the change of the first.
  test.describe.configure({ retries: 0 });

  test.beforeEach(async ({ page }) => {
    await navigateTo(page, `/vpc-file-pools/pools/${POOL}`);
    await checkSessionValid(page);
    await clickTab(page, 'Shares');
  });

  test('drain: the confirm step says what happens, then the share is on the drain list', async ({ page }) => {
    test.skip(PHASE !== 'drain', 'phase is not drain');
    const row = page.getByRole('row').filter({ hasText: SHARE as string });
    await row.getByRole('button', { name: 'Drain', exact: true }).click();
    await expect(page.getByRole('dialog').getByText(`Drain share ${SHARE}?`)).toBeVisible();
    await expect(page.getByRole('dialog').getByText('New volumes stop going to this share.')).toBeVisible();
    // Cancel leaves the pool as it was.
    await page.getByRole('button', { name: 'Cancel' }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(row.getByRole('button', { name: 'Drain', exact: true })).toBeVisible();

    await row.getByRole('button', { name: 'Drain', exact: true }).click();
    await page.getByRole('button', { name: 'Drain share', exact: true }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(row.getByRole('button', { name: 'Return to service', exact: true })).toBeVisible();
  });

  test('return: the share leaves the drain list', async ({ page }) => {
    test.skip(PHASE !== 'return', 'phase is not return');
    const row = page.getByRole('row').filter({ hasText: SHARE as string });
    await row.getByRole('button', { name: 'Return to service', exact: true }).click();
    await expect(page.getByRole('dialog').getByText(`Return share ${SHARE} to service?`)).toBeVisible();
    await page.getByRole('dialog').getByRole('button', { name: 'Return to service', exact: true }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(row.getByRole('button', { name: 'Drain', exact: true })).toBeVisible();
  });
});
