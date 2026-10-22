import { test, expect } from '@playwright/test';
import { navigateTo, expectPageTitle, checkSessionValid } from '../support/pages';

/**
 * Restore a snapshot to a new claim, in the console.
 *
 * This test writes to the cluster, so it runs only against a console that has a write token limited to one
 * test namespace (release stage 16, second part). Against any other console it is skipped.
 *
 * RESTORE_NS        the namespace of the snapshot's VolumeSnapshot
 * RESTORE_SNAPSHOT  the name of the driver's Snapshot object
 * RESTORE_SOURCE    the name of the claim the snapshot was taken from
 * RESTORE_NAME      the name for the new claim (default: restored-in-console)
 */
const NS = process.env.RESTORE_NS;
const SNAPSHOT = process.env.RESTORE_SNAPSHOT;
const SOURCE = process.env.RESTORE_SOURCE;
const NAME = process.env.RESTORE_NAME || 'restored-in-console';

test.describe('Restore a snapshot to a new PVC', () => {
  test.skip(!NS || !SNAPSHOT, 'needs a console with a write token for one namespace (release stage 16)');
  test.setTimeout(900_000);
  // A second try would find the claim of the first and be refused.
  test.describe.configure({ retries: 0 });

  test.beforeEach(async ({ page }) => {
    await navigateTo(page, `/vpc-file-pools/snapshots/${SNAPSHOT}/restore`);
    await checkSessionValid(page);
    await expectPageTitle(page, 'Restore to new PVC');
  });

  test('the form has defaults taken from the snapshot', async ({ page }) => {
    await expect(page.locator('#restore-namespace')).toHaveText(NS as string);
    if (SOURCE) {
      await expect(page.locator('#restore-name')).toHaveValue(`${SOURCE}-restored`);
    }
    await expect(page.getByRole('button', { name: 'Restore', exact: true })).toBeEnabled();
  });

  test('a name that is already in use is refused', async ({ page }) => {
    test.skip(!SOURCE, 'needs RESTORE_SOURCE, the name of a claim that exists');
    await page.locator('#restore-name').fill(SOURCE as string);
    await expect(page.getByText('A claim with this name already exists in the namespace.')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Restore', exact: true })).toBeDisabled();
  });

  test('a size below the snapshot is refused', async ({ page }) => {
    await page.locator('#restore-size').fill('0');
    await expect(page.getByText(/The size must be at least \d+ GiB/)).toBeVisible();
    await expect(page.getByRole('button', { name: 'Restore', exact: true })).toBeDisabled();
  });

  test('restore makes a claim and the banner follows it until it is ready', async ({ page }) => {
    await page.locator('#restore-name').fill(NAME);
    await page.getByRole('button', { name: 'Restore', exact: true }).click();

    // One of the three states shows at once; the last one ends the wait.
    await expect(
      page.getByText(/Restoring: copying|Restoring: the copy is starting|The restored volume is ready/).first(),
    ).toBeVisible();
    await expect(page.getByText('The restored volume is ready')).toBeVisible({ timeout: 600_000 });
    await expect(page.getByRole('link', { name: NAME })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Restore', exact: true })).toHaveCount(0);
  });
});
