const { test, expect } = require('@playwright/test');

// Dashboard, exercised through the real browser.
//
// The dashboard is a live session summary and a capability-access view. Its
// value is that every state it shows is the server's own authorization answer
// for the signed-in caller, not a client-side guess from the role. These tests
// pin exactly that: the identity and scope come from /api/me, the API line comes
// from the health endpoints, and each capability state comes from a bounded read
// the browser actually issues -- so a role whose access changed would change
// what the page reports, rather than what the page assumes.

function env(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required for browser E2E`);
  return value;
}

const founderUsername = env('BROWSER_E2E_FOUNDER_USERNAME');
const founderPassword = env('BROWSER_E2E_FOUNDER_PASSWORD');
const adminA = env('BROWSER_E2E_ADMIN_A');
const memberA = env('BROWSER_E2E_MEMBER_A');
const browserPassword = env('BROWSER_E2E_PASSWORD');
const workspaceA = env('BROWSER_E2E_WORKSPACE_A');

// Every capability the dashboard reports. The order is the order it renders.
const capabilities = ['Workspaces', 'Departments', 'Teams', 'AI Employees',
  'Audit', 'Tasks', 'Knowledge', 'Publications', 'Pipelines'];

// The two organization-wide views are available only to the founder. A
// workspace admin and a member have identical capability access to each other:
// everything except those two.
const organizationWide = ['Workspaces', 'Audit'];
const workspaceAvailable = capabilities.filter((name) => !organizationWide.includes(name));

async function signIn(page, username, password) {
  await page.goto('/');
  await expect(page.locator('#auth-view')).toBeVisible();
  await page.locator('#username').fill(username);
  await page.locator('#password').fill(password);
  await page.locator('#login-btn').click();
  await expect(page.locator('#app-view')).toBeVisible();
  await expect(page.locator('#identity')).toContainText(username);
}

function capabilityChip(page, name) {
  return page.locator('#dashboard-capabilities li', { hasText: name }).locator('span').last();
}

async function expectCapability(page, name, state) {
  await expect.poll(async () => (await capabilityChip(page, name).innerText()).trim(), {
    timeout: 30000,
    intervals: [250, 500, 1000, 2000],
    message: `${name} capability did not settle to ${state}`,
  }).toBe(state);
}

async function expectCapabilities(page, states) {
  await expect(page.locator('#dashboard-capabilities li')).toHaveCount(capabilities.length);
  for (const name of capabilities) {
    await expectCapability(page, name, states[name]);
  }
}

test('dashboard reports live session identity, scope and server-derived access', async ({ browser }) => {
  const adminContext = await browser.newContext();
  const memberContext = await browser.newContext();
  const founderContext = await browser.newContext();
  const adminPage = await adminContext.newPage();
  const memberPage = await memberContext.newPage();
  const founderPage = await founderContext.newPage();

  try {
    // ---- a workspace admin ----------------------------------------------
    await signIn(adminPage, adminA, browserPassword);
    await expect(adminPage.locator('#dashboard-card')).toBeVisible();
    await expect(adminPage.locator('#dashboard-summary dd[data-dash="identity"]')).toContainText(adminA);
    await expect(adminPage.locator('#dashboard-summary dd[data-dash="scope"]')).toHaveText(workspaceA);
    await expect.poll(
      () => adminPage.locator('#dashboard-summary dd[data-dash="api"]').innerText(),
      { timeout: 15000, message: 'dashboard API state did not settle' },
    ).toBe('live and ready');

    const adminStates = {};
    for (const name of capabilities) adminStates[name] = workspaceAvailable.includes(name) ? 'available' : 'denied';
    await expectCapabilities(adminPage, adminStates);
    console.log('BROWSER_STEP dashboard-admin-state');

    // Refresh issues a fresh bounded read per capability rather than replaying
    // the first answer. The states must settle to the same server answers.
    await adminPage.locator('#dashboard-refresh-btn').click();
    await expectCapabilities(adminPage, adminStates);
    console.log('BROWSER_STEP dashboard-refresh');

    // ---- a workspace member sees the same server-derived access ----------
    await signIn(memberPage, memberA, browserPassword);
    await expect(memberPage.locator('#dashboard-summary dd[data-dash="scope"]')).toHaveText(workspaceA);
    const memberStates = {};
    for (const name of capabilities) memberStates[name] = workspaceAvailable.includes(name) ? 'available' : 'denied';
    await expectCapabilities(memberPage, memberStates);
    console.log('BROWSER_STEP dashboard-member-state');

    // ---- the founder is organization-scoped, not workspace-scoped --------
    await signIn(founderPage, founderUsername, founderPassword);
    await expect(founderPage.locator('#dashboard-summary dd[data-dash="scope"]'))
      .toContainText('organization-level');
    const founderStates = {};
    for (const name of capabilities) founderStates[name] = organizationWide.includes(name) ? 'available' : 'denied';
    await expectCapabilities(founderPage, founderStates);
    console.log('BROWSER_STEP dashboard-founder-state');

    // The dashboard reports access, never credentials: no session material is
    // rendered into the card.
    const cardText = await founderPage.locator('#dashboard-card').innerText();
    expect(cardText).not.toContain('eyJ');
    expect(cardText).not.toContain('Bearer');
  } finally {
    await Promise.all([adminContext.close(), memberContext.close(), founderContext.close()]);
  }
});

test('a stale access token refreshes once for the whole concurrent dashboard burst', async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    await signIn(page, adminA, browserPassword);
    await expect(page.locator('#dashboard-card')).toBeVisible();

    let refreshCalls = 0;
    page.on('request', (request) => {
      if (request.method() === 'POST' && request.url().endsWith('/api/auth/refresh')) {
        refreshCalls += 1;
      }
    });

    // Make the access token stale while the refresh token stays valid, then
    // reload the dashboard. Its nine capability probes race their 401s, which
    // is exactly the burst that used to present the same one-time-use refresh
    // token nine times and have the server revoke the session family.
    await page.evaluate(() => {
      sessionStorage.setItem('austro.access', 'stale-access-token');
    });

    const adminStates = {};
    for (const name of capabilities) adminStates[name] = workspaceAvailable.includes(name) ? 'available' : 'denied';

    await page.locator('#dashboard-refresh-btn').click();
    await expectCapabilities(page, adminStates);

    // The session survived, and the one-time-use refresh token was rotated
    // exactly once for the whole burst rather than once per 401.
    await expect(page.locator('#app-view')).toBeVisible();
    await expect(page.locator('#identity')).toContainText(adminA);
    expect(refreshCalls).toBe(1);
    console.log('BROWSER_STEP dashboard-single-refresh');
  } finally {
    await context.close();
  }
});
