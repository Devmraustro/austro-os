const { expect } = require('@playwright/test');

// Login is rate limited per client address (internal/api/ratelimit.go: at most
// 10 logins in a one-minute fixed window). A full browser run signs in far more
// often than that from the single runner address, so a sign-in that lands in an
// exhausted window is answered with the same 429 the UI renders as "too many
// requests". Rejected requests do not consume the budget, so this helper waits
// for the fixed window to roll and retries instead of failing a later,
// unrelated test. The limit itself is never weakened: the rejected attempt is
// not granted, the helper only retries an ordinary login.
const RATE_LIMIT_RETRY_INTERVAL_MS = 2000;
// A rejected attempt never advances the window, so waiting a little over one
// full window is always enough for the next attempt to be admitted.
const RATE_LIMIT_MAX_WAIT_MS = 90000;

async function signIn(page, username, password, onReady) {
  await page.goto('/');
  await expect(page.locator('#auth-view')).toBeVisible();
  if (onReady) await onReady();

  const app = page.locator('#app-view');
  const deadline = Date.now() + RATE_LIMIT_MAX_WAIT_MS;
  for (;;) {
    await page.locator('#username').fill(username);
    await page.locator('#password').fill(password);
    const responsePromise = page.waitForResponse(
      (response) => response.url().endsWith('/api/auth/login') && response.request().method() === 'POST');
    await page.locator('#login-btn').click();
    const response = await responsePromise;
    if (response.status() === 200) {
      break;
    }
    if (response.status() !== 429) {
      throw new Error(`sign in as ${username} failed with status ${response.status()}`);
    }
    if (Date.now() >= deadline) {
      throw new Error(`sign in as ${username} was still rate limited after ${RATE_LIMIT_MAX_WAIT_MS}ms`);
    }
    await page.waitForTimeout(RATE_LIMIT_RETRY_INTERVAL_MS);
  }

  await expect(app).toBeVisible();
  await expect(page.locator('#identity')).toContainText(username);
}

module.exports = { signIn };
