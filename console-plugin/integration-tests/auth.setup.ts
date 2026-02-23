import { test as setup, expect } from '@playwright/test';

const STORAGE_STATE_PATH = 'storageState.json';

setup('authenticate with OpenShift console', async ({ page }) => {
  const consoleURL = process.env.CONSOLE_URL;
  if (!consoleURL) {
    throw new Error('CONSOLE_URL env var is required (e.g. https://console-openshift-console.apps.cluster.cloud.ibm.com)');
  }

  await page.goto(consoleURL);

  const email = process.env.IBMCLOUD_EMAIL;
  const password = process.env.IBMCLOUD_PASSWORD;

  if (email && password) {
    // Automated IBM Cloud IAM login
    // Wait for the IAM login page to load
    await page.waitForURL(/.*iam.*|.*identity.*|.*login.*/, { timeout: 30_000 }).catch(() => {
      // May already be on the login form
    });

    // Fill email
    const emailInput = page.locator('input[name="username"], input[type="email"], #username');
    await emailInput.waitFor({ timeout: 15_000 });
    await emailInput.fill(email);

    // Click continue/next if present
    const continueBtn = page.locator('button:has-text("Continue"), button:has-text("Next"), button[type="submit"]').first();
    await continueBtn.click().catch(() => {});

    // Fill password
    const passwordInput = page.locator('input[name="password"], input[type="password"], #password');
    await passwordInput.waitFor({ timeout: 15_000 });
    await passwordInput.fill(password);

    // Submit
    const loginBtn = page.locator('button:has-text("Log in"), button:has-text("Sign in"), button[type="submit"]').first();
    await loginBtn.click();
  } else {
    // Manual login: pause for the user to complete SSO
    console.log('\n╔══════════════════════════════════════════════════════╗');
    console.log('║  Manual login required                               ║');
    console.log('║                                                      ║');
    console.log('║  1. Complete IBM Cloud SSO login in the browser       ║');
    console.log('║  2. Wait until the OpenShift console dashboard loads  ║');
    console.log('║  3. Press "Resume" in the Playwright Inspector        ║');
    console.log('╚══════════════════════════════════════════════════════╝\n');
    await page.pause();
  }

  // Wait for the OpenShift console to fully load after login
  await expect(page.locator('[data-test-id="resource-title"], .co-dashboard-body, .pf-v6-c-page')).toBeVisible({
    timeout: 60_000,
  });

  // Save session state
  await page.context().storageState({ path: STORAGE_STATE_PATH });
  console.log(`\nSession saved to ${STORAGE_STATE_PATH}`);
});
