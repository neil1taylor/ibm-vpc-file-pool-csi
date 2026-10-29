import { test, expect, type Page } from '@playwright/test';
import { navigateTo, checkSessionValid } from '../support/pages';

/**
 * Fail over one VM on the Recovery page, in the console of the DR (destination) cluster.
 *
 * This test changes the cluster (it fails over a VM), so it runs only against a console that has a token
 * for Failover objects (release stage 21). Against any other console it is skipped.
 *
 * RECOVERY_VM        the VM to fail over, as namespace/name
 * RECOVERY_OTHER_VM  another VM on the same copy, as namespace/name; it must stay in the table
 * RECOVERY_SOURCE    the data claim of the replication receiver that holds the copy (chosen when the
 *                    cluster has more than one receiver)
 * RECOVERY_POOL      the FileSharePool of the DR cluster for the new volumes
 * RECOVERY_PVCS      the claims the failover makes, as namespace/name, comma separated
 *
 * The VM has two disks and at least two moments to choose from. The tests run in order. The last one makes
 * one Execute; the others change nothing. A second try would find the claims of the first and be refused,
 * so there are no retries.
 */
const VM = process.env.RECOVERY_VM;
const OTHER_VM = process.env.RECOVERY_OTHER_VM;
const SOURCE = process.env.RECOVERY_SOURCE;
const POOL = process.env.RECOVERY_POOL;
const PVCS = (process.env.RECOVERY_PVCS || '').split(',').filter(Boolean);

const RECOVERY_ROUTE = '/vpc-file-pools/replication/~recovery';
// Not anchored: the alert title starts with a hidden "Info alert:" for screen readers.
const PLAN_READY = /Plan made from /;
const PLAN_TOO_OLD = 'This plan is too old to fail over from. Click Refresh.';

/**
 * Open the Recovery page and wait for a usable plan. The plan on screen is the newest one for the chosen
 * source. When asked to, or when that plan is too old, click Refresh and wait for the new one.
 */
async function openRecovery(page: Page, opts: { refresh: boolean }): Promise<void> {
  await navigateTo(page, RECOVERY_ROUTE);
  await checkSessionValid(page);
  await expect(page.locator('h1').filter({ hasText: 'Recovery' })).toBeVisible();

  // The page shows one of: the source choice (several receivers), a plan, or a plan being made.
  const source = page.locator('#failover-source');
  const banner = page.getByText(PLAN_READY).or(page.getByText(PLAN_TOO_OLD));
  await expect(
    source.or(banner).or(page.getByText(/Still working: the plan is being made/)).first(),
  ).toBeVisible({ timeout: 180_000 });
  if (await source.count()) {
    await source.selectOption(SOURCE as string);
  }
  await expect(banner.first()).toBeVisible({ timeout: 300_000 });

  const tooOld = await page.getByText(PLAN_TOO_OLD).isVisible();
  if (opts.refresh || tooOld) {
    const refresh = page.getByRole('button', { name: 'Refresh', exact: true });
    await refresh.click();
    // The button is off while the plan is made, and on again when it is ready.
    await expect(refresh).toBeDisabled();
    await expect(refresh).toBeEnabled({ timeout: 300_000 });
    await expect(page.getByText(PLAN_READY)).toBeVisible();
  }
  await expect(page.getByRole('table', { name: 'Running VMs found on the copy' })).toBeVisible();
}

async function openVM(page: Page): Promise<void> {
  const table = page.getByRole('table', { name: 'Running VMs found on the copy' });
  await table.getByRole('button', { name: VM as string, exact: true }).click();
  await expect(page.getByText(`Disks of ${VM}`)).toBeVisible();
}

/** Click "Fail over this VM" and wait for the confirm dialog of the plan the page makes for this VM. */
async function openConfirm(page: Page) {
  await page.getByRole('button', { name: 'Fail over this VM', exact: true }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog.getByText(`Fail over ${VM}?`)).toBeVisible({ timeout: 300_000 });
  return dialog;
}

test.describe('Recovery page: fail over one VM', () => {
  test.skip(!VM || !OTHER_VM || !SOURCE || !POOL || PVCS.length === 0, 'needs a console with a failover token (release stage 21)');
  test.setTimeout(1_800_000);
  test.describe.configure({ retries: 0 });

  test('Refresh makes a plan, and the table lists the VMs of the copy with their disks', async ({ page }) => {
    await openRecovery(page, { refresh: true });
    const table = page.getByRole('table', { name: 'Running VMs found on the copy' });
    for (const name of [VM as string, OTHER_VM as string]) {
      const row = table.getByRole('row').filter({ has: page.getByRole('button', { name, exact: true }) });
      await expect(row, `a row for ${name}`).toHaveCount(1);
      await expect(row.locator('td').nth(1), `${name} should show 2 disks`).toHaveText('2');
    }
    // Nothing changes until a confirm step: the page offers the buttons, and has made no Execute yet.
    await expect(page.getByRole('button', { name: 'Fail over all VMs', exact: true })).toBeVisible();
    await expect(page.getByText('The failover is done')).toHaveCount(0);
  });

  test('the detail view shows the disks of the VM and its moments, newest first', async ({ page }) => {
    await openRecovery(page, { refresh: false });
    await openVM(page);
    const disks = page.getByRole('table', { name: 'Disks of the VM' });
    await expect(disks.locator('tbody tr')).toHaveCount(2);
    await expect(disks.getByText(/Used in place\.|Copies the whole disk; takes minutes\./).first()).toBeVisible();
    const moments = page.getByRole('radio');
    expect(await moments.count(), 'the VM should have at least two moments to choose from').toBeGreaterThanOrEqual(2);
    await expect(moments.first(), 'the first moment is the newest and is chosen').toBeChecked();
    await expect(page.getByText(/- newest/)).toHaveCount(1);
    await page.getByRole('button', { name: 'Back to the VMs', exact: true }).click();
    await expect(page.getByRole('table', { name: 'Running VMs found on the copy' })).toBeVisible();
  });

  test('an older moment says its disks are rewound; a wrong name keeps the button off; Cancel makes nothing', async ({ page }) => {
    await openRecovery(page, { refresh: false });
    await openVM(page);
    await page.getByRole('radio').last().check();
    const dialog = await openConfirm(page);

    await expect(dialog.getByText(/2 disks to rewind/)).toBeVisible();
    await expect(dialog.getByText(`Only ${VM} fails over.`)).toBeVisible();
    for (const pvc of PVCS) {
      await expect(dialog.getByRole('listitem').filter({ hasText: pvc })).toBeVisible();
    }
    const go = dialog.getByRole('button', { name: 'Fail over', exact: true });
    await dialog.locator('#failover-dr-pool').selectOption({ label: POOL as string });
    await expect(go, 'nothing typed yet').toBeDisabled();
    await dialog.locator('#failover-confirm').fill(OTHER_VM as string);
    await expect(go, 'the name of another VM must not confirm').toBeDisabled();
    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    await expect(page.getByRole('dialog').filter({ hasText: `Fail over ${VM}?` })).toBeHidden();
  });

  test('typing the VM name confirms; the failover runs to Done and links the claims it made', async ({ page }) => {
    await openRecovery(page, { refresh: false });
    await openVM(page);
    // The newest moment is chosen: no disk is rewound, so this failover is quick.
    await expect(page.getByRole('radio').first()).toBeChecked();
    const dialog = await openConfirm(page);

    await expect(dialog.getByText(/0 disks to rewind/)).toBeVisible();
    const go = dialog.getByRole('button', { name: 'Fail over', exact: true });
    await dialog.locator('#failover-dr-pool').selectOption({ label: POOL as string });
    await dialog.locator('#failover-confirm').fill(VM as string);
    await expect(go).toBeEnabled();
    await go.click();
    await expect(page.getByRole('dialog').filter({ hasText: `Fail over ${VM}?` })).toBeHidden();

    // The progress banner: still working, or already done.
    await expect(
      page.getByText(/Still working: the failover is running|The failover is done/).first(),
    ).toBeVisible({ timeout: 120_000 });
    await expect(page.getByText('The failover is done')).toBeVisible({ timeout: 1_200_000 });
    await expect(page.getByText('These PVCs were made:')).toBeVisible();
    for (const pvc of PVCS) {
      await expect(page.getByRole('link', { name: pvc, exact: true })).toBeVisible();
    }
    // The page does not offer a second failover while this one is shown.
    await expect(page.getByText('The failover failed')).toHaveCount(0);
  });
});
