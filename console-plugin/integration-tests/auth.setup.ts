import { test as setup, expect } from '@playwright/test';
import { execSync } from 'child_process';

const STORAGE_STATE_PATH = 'storageState.json';

/**
 * Authentication setup for ROKS clusters.
 *
 * Strategy priority:
 * 1. OC_TOKEN env var — use an explicit OCP token
 * 2. `oc whoami --show-token` — extract token from an existing oc login session
 * 3. IBMCLOUD_EMAIL + IBMCLOUD_PASSWORD — attempt automated IAM login via browser
 * 4. Manual — pause for user to complete SSO in the Playwright Inspector
 *
 * For strategies 1 & 2, the token is exchanged against the Console's OAuth
 * endpoint to obtain session cookies, avoiding the IAM passkey redirect entirely.
 */
setup('authenticate with OpenShift console', async ({ page, context }) => {
  const consoleURL = process.env.CONSOLE_URL;
  if (!consoleURL) {
    throw new Error('CONSOLE_URL env var is required (e.g. https://console-openshift-console.apps.cluster.cloud.ibm.com)');
  }

  // --- Try token-based auth first (works with ROKS IAM passkey) ---
  const token = getOCPToken();
  if (token) {
    console.log('Using OCP token for authentication...');
    const success = await authenticateWithToken(page, consoleURL, token);
    if (success) {
      await page.context().storageState({ path: STORAGE_STATE_PATH });
      console.log(`Session saved to ${STORAGE_STATE_PATH}`);
      return;
    }
    console.log('Token-based auth failed, falling back to browser login...');
  }

  // --- Fallback: browser-based login ---
  await page.goto(consoleURL);

  const email = process.env.IBMCLOUD_EMAIL;
  const password = process.env.IBMCLOUD_PASSWORD;

  if (email && password) {
    // Automated IBM Cloud IAM login (may not work with passkey-only accounts)
    await page.waitForURL(/.*iam.*|.*identity.*|.*login.*/, { timeout: 30_000 }).catch(() => {});

    const emailInput = page.locator('input[name="username"], input[type="email"], #username');
    await emailInput.waitFor({ timeout: 15_000 });
    await emailInput.fill(email);

    const continueBtn = page.locator('button:has-text("Continue"), button:has-text("Next"), button[type="submit"]').first();
    await continueBtn.click().catch(() => {});

    const passwordInput = page.locator('input[name="password"], input[type="password"], #password');
    await passwordInput.waitFor({ timeout: 15_000 });
    await passwordInput.fill(password);

    const loginBtn = page.locator('button:has-text("Log in"), button:has-text("Sign in"), button[type="submit"]').first();
    await loginBtn.click();
  } else {
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

  await page.context().storageState({ path: STORAGE_STATE_PATH });
  console.log(`Session saved to ${STORAGE_STATE_PATH}`);
});

/**
 * Get an OCP token from OC_TOKEN env var or `oc whoami --show-token`.
 */
function getOCPToken(): string | null {
  if (process.env.OC_TOKEN) {
    return process.env.OC_TOKEN;
  }
  try {
    const token = execSync('oc whoami --show-token 2>/dev/null', { encoding: 'utf-8' }).trim();
    if (token) {
      console.log(`Extracted OCP token from oc login session (user: ${execSync('oc whoami 2>/dev/null', { encoding: 'utf-8' }).trim()})`);
      return token;
    }
  } catch {
    // oc not logged in or not installed
  }
  return null;
}

/**
 * Authenticate with the Console by exchanging an OCP token via the OAuth flow.
 *
 * OCP Console uses OAuth — navigating to the console URL redirects through
 * the OAuth server. We inject the token as a Bearer header to the OAuth
 * authorize endpoint, which issues session cookies.
 */
async function authenticateWithToken(
  page: import('@playwright/test').Page,
  consoleURL: string,
  token: string,
): Promise<boolean> {
  try {
    // Discover the OAuth server from the cluster's well-known endpoint
    const baseURL = new URL(consoleURL);
    const clusterDomain = baseURL.hostname.replace('console-openshift-console.', '');
    const oauthURL = `https://oauth-openshift.${clusterDomain}`;

    // Set the authorization header for the OAuth domain
    await page.context().setExtraHTTPHeaders({});

    // Navigate to the console — this triggers the OAuth redirect chain.
    // Intercept the OAuth authorize request and inject the bearer token.
    await page.route(`${oauthURL}/**`, async (route) => {
      const headers = {
        ...route.request().headers(),
        'authorization': `Bearer ${token}`,
      };
      await route.continue({ headers });
    });

    await page.goto(consoleURL, { waitUntil: 'domcontentloaded', timeout: 30_000 });

    // Remove the route handler after auth completes
    await page.unrouteAll({ behavior: 'ignoreErrors' });

    // Check if we landed on the console (not a login page)
    const isConsole = await page.locator('[data-test-id="resource-title"], .co-dashboard-body, .pf-v6-c-page').isVisible({ timeout: 15_000 }).catch(() => false);
    return isConsole;
  } catch (err) {
    console.log('Token auth error:', err instanceof Error ? err.message : err);
    return false;
  }
}
