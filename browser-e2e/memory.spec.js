const { test, expect } = require('@playwright/test');

// Memory vertical slice, exercised through the real browser.
//
// Memory is the smallest capability in the product: one workspace-scoped cell
// addressed by layer and key. The live Go suite covers isolation and the domain
// rules over HTTP. This spec pins the browser-level claims: that a write is
// confirmed by the server's own returned cell, that a read round-trips the
// stored value, and that a caller without a workspace is told the session is
// not authorized instead of being shown an empty result that looks like data.

function env(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required for browser E2E`);
  return value;
}

const founderUsername = env('BROWSER_E2E_FOUNDER_USERNAME');
const founderPassword = env('BROWSER_E2E_FOUNDER_PASSWORD');
const memberA = env('BROWSER_E2E_MEMBER_A');
const browserPassword = env('BROWSER_E2E_PASSWORD');
const workspaceA = env('BROWSER_E2E_WORKSPACE_A');

async function signIn(page, username, password) {
  await page.goto('/');
  await expect(page.locator('#auth-view')).toBeVisible();
  await page.locator('#username').fill(username);
  await page.locator('#password').fill(password);
  await page.locator('#login-btn').click();
  await expect(page.locator('#app-view')).toBeVisible();
  await expect(page.locator('#identity')).toContainText(username);
}

test('memory journey: save, read back, and overwrite one workspace cell', async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    await signIn(page, memberA, browserPassword);
    await expect(page.locator('#memory-card')).toBeVisible();

    const key = 'browser.e2e.' + Math.random().toString(36).slice(2, 10);
    const value = 'tone=' + Math.random().toString(36).slice(2, 8);

    // ---- save ------------------------------------------------------------
    await page.locator('#memory-layer').selectOption('workspace');
    await page.locator('#memory-key').fill(key);
    await page.locator('#memory-value').fill(value);
    const saved = page.waitForResponse((response) =>
      response.url().endsWith(`/memory/workspace/${encodeURIComponent(key)}`) &&
      response.request().method() === 'PUT');
    await page.locator('#memory-form button[type="submit"]').click();
    expect((await saved).status()).toBe(200);

    await expect(page.locator('#memory-message')).toContainText('Memory saved.');
    await expect(page.locator('#memory-result')).toBeVisible();
    // The panel shows the server's own cell identity, never a client guess.
    await expect(page.locator('#memory-result-layer')).toHaveText('workspace');
    await expect(page.locator('#memory-result-key')).toHaveText(key);
    await expect(page.locator('#memory-workspace')).toHaveText(workspaceA);
    console.log('BROWSER_STEP memory-saved');

    // ---- read back -------------------------------------------------------
    await page.locator('#memory-value').fill('');
    await page.locator('#memory-read-btn').click();
    await expect(page.locator('#memory-message')).toContainText('Memory read.');
    await expect(page.locator('#memory-value')).toHaveValue(value);
    console.log('BROWSER_STEP memory-read');

    // ---- overwrite -------------------------------------------------------
    const nextValue = 'tone=' + Math.random().toString(36).slice(2, 8);
    await page.locator('#memory-value').fill(nextValue);
    await page.locator('#memory-form button[type="submit"]').click();
    await expect(page.locator('#memory-message')).toContainText('Memory saved.');
    await page.locator('#memory-value').fill('');
    await page.locator('#memory-read-btn').click();
    await expect(page.locator('#memory-message')).toContainText('Memory read.');
    await expect(page.locator('#memory-value')).toHaveValue(nextValue);
    console.log('BROWSER_STEP memory-overwritten');
  } finally {
    await context.close();
  }
});

test('memory is refused without a workspace, and the page says so', async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    // The founder owns the organization but belongs to no workspace, and every
    // memory layer the tenant API exposes is workspace-bound. The server
    // refuses; the page names the authorization refusal rather than a not-found.
    await signIn(page, founderUsername, founderPassword);
    await expect(page.locator('#memory-card')).toBeVisible();

    const key = 'browser.e2e.denied.' + Math.random().toString(36).slice(2, 8);
    await page.locator('#memory-layer').selectOption('workspace');
    await page.locator('#memory-key').fill(key);

    await page.locator('#memory-read-btn').click();
    await expect(page.locator('#memory-message')).toContainText(
      'Your session is not authorized for workspace memory.');
    await expect(page.locator('#memory-result')).toBeHidden();
    console.log('BROWSER_STEP memory-founder-read-refused');

    await page.locator('#memory-value').fill('should not persist');
    await page.locator('#memory-form button[type="submit"]').click();
    await expect(page.locator('#memory-message')).toContainText(
      'Your session is not authorized for workspace memory.');
    await expect(page.locator('#memory-result')).toBeHidden();
    console.log('BROWSER_STEP memory-founder-write-refused');
  } finally {
    await context.close();
  }
});
