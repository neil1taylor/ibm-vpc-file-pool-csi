import { test, expect } from '@playwright/test';
import { navigateTo, expectPageTitle, checkSessionValid } from '../support/pages';

/**
 * Migrate a claim of another storage class into a pool, in the console.
 *
 * This test writes to the cluster, so it runs only against a console that has a write token limited to one
 * test namespace (release stage 16, migrate part). Against any other console it is skipped.
 *
 * MIGRATE_NS     the namespace of the claim
 * MIGRATE_CLAIM  the name of the claim to move
 * MIGRATE_CLASS  its storage class
 * MIGRATE_POOL   the target pool
 */
const NS = process.env.MIGRATE_NS;
const CLAIM = process.env.MIGRATE_CLAIM;
const CLASS = process.env.MIGRATE_CLASS;
const POOL = process.env.MIGRATE_POOL;

test.describe('Migrate a claim into a pool', () => {
  test.skip(!NS || !CLAIM || !CLASS || !POOL, 'needs a console with a write token for one namespace (release stage 16)');
  test.setTimeout(900_000);
  test.describe.configure({ retries: 0 });

  test.beforeEach(async ({ page }) => {
    await navigateTo(page, '/vpc-file-pools/migrate');
    await checkSessionValid(page);
    await expectPageTitle(page, 'Migrate volumes');
    await page.locator('#migrate-class').selectOption(CLASS as string);
    await page.locator('#migrate-namespace').selectOption(NS as string);
  });

  test('the claim is listed, and the confirm step names the new claim and the pool', async ({ page }) => {
    const row = page.getByRole('row').filter({ hasText: CLAIM as string }).first();
    await expect(row).toBeVisible();
    await row.getByRole('button', { name: 'Migrate', exact: true }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByText(`Migrate ${CLAIM}?`)).toBeVisible();
    await expect(dialog.getByText(`A new claim ${CLAIM}-pooled is created in the pool.`)).toBeVisible();
    await expect(dialog.getByText(/GB free in pool/)).toBeVisible();
    // Cancel makes nothing.
    await dialog.getByRole('button', { name: 'Cancel' }).click();
    await expect(page.getByRole('dialog').filter({ hasText: /Migrate / })).toBeHidden();
    await expect(row.getByRole('button', { name: 'Migrate', exact: true })).toBeEnabled();
  });

  test('migrate makes the new claim and the row follows the copy until it is done', async ({ page }) => {
    const row = page.getByRole('row').filter({ hasText: CLAIM as string }).first();
    await row.getByRole('button', { name: 'Migrate', exact: true }).click();
    const dialog = page.getByRole('dialog');
    await dialog.locator('#migrate-target').selectOption({ label: POOL as string });
    await dialog.getByRole('button', { name: 'Start migration' }).click();
    await expect(page.getByRole('dialog').filter({ hasText: /Migrate / })).toBeHidden();

    await expect(
      row.getByText(/Waiting for the new claim|The new claim is Bound|Copying|Copied\./).first(),
    ).toBeVisible();
    await expect(row.getByText(`Copied. Switch your application to ${CLAIM}-pooled`)).toBeVisible({
      timeout: 600_000,
    });
  });
});
