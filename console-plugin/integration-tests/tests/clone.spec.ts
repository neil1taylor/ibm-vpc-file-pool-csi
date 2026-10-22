import { test, expect } from '@playwright/test';
import { navigateTo, expectPageTitle, checkSessionValid } from '../support/pages';

/**
 * Clone a volume to a new claim, in the console.
 *
 * This test writes to the cluster, so it runs only against a console that has a write token limited to one
 * test namespace (release stage 16, clone part). Against any other console it is skipped.
 *
 * CLONE_SUBVOLUME  the name of the driver's SubVolume to clone
 * CLONE_NS         the namespace of its claim
 * CLONE_SOURCE     the name of the claim of that volume
 * CLONE_NAME       the name for the new claim (default: cloned-in-console)
 */
const SUBVOLUME = process.env.CLONE_SUBVOLUME;
const NS = process.env.CLONE_NS;
const SOURCE = process.env.CLONE_SOURCE;
const NAME = process.env.CLONE_NAME || 'cloned-in-console';

test.describe('Clone a volume to a new PVC', () => {
  test.skip(!SUBVOLUME || !NS, 'needs a console with a write token for one namespace (release stage 16)');
  test.setTimeout(900_000);
  test.describe.configure({ retries: 0 });

  test.beforeEach(async ({ page }) => {
    await navigateTo(page, `/vpc-file-pools/subvolumes/${SUBVOLUME}/clone`);
    await checkSessionValid(page);
    await expectPageTitle(page, 'Clone to new PVC');
  });

  test('the form has defaults taken from the volume', async ({ page }) => {
    await expect(page.locator('#clone-namespace')).toHaveText(NS as string);
    if (SOURCE) {
      await expect(page.locator('#clone-name')).toHaveValue(`${SOURCE}-clone`);
    }
    await expect(page.getByRole('button', { name: 'Clone', exact: true })).toBeEnabled();
  });

  test('a name that is already in use is refused', async ({ page }) => {
    test.skip(!SOURCE, 'needs CLONE_SOURCE, the name of a claim that exists');
    await page.locator('#clone-name').fill(SOURCE as string);
    await expect(page.getByText('A claim with this name already exists in the namespace.')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Clone', exact: true })).toBeDisabled();
  });

  test('a size below the volume is refused', async ({ page }) => {
    await page.locator('#clone-size').fill('0');
    await expect(page.getByText(/The size must be at least \d+ GiB/)).toBeVisible();
    await expect(page.getByRole('button', { name: 'Clone', exact: true })).toBeDisabled();
  });

  test('clone makes a claim and the banner follows it until it is ready', async ({ page }) => {
    await page.locator('#clone-name').fill(NAME);
    await page.getByRole('button', { name: 'Clone', exact: true }).click();

    await expect(
      page.getByText(/Cloning: copying|Cloning: the copy is starting|The cloned volume is ready/).first(),
    ).toBeVisible();
    await expect(page.getByText('The cloned volume is ready')).toBeVisible({ timeout: 600_000 });
    await expect(page.getByRole('link', { name: NAME })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Clone', exact: true })).toHaveCount(0);
  });
});
