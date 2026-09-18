const { test, expect } = require('@playwright/test');
const { signIn } = require('./support');

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

// The workflow summary must be composed only from real API responses, so these
// tests capture the live responses the page itself issues and assert the card
// says exactly what those responses contain -- never a hardcoded number. The
// card's own page-size probes (limit=1/50) never collide with these reads
// (limit=100 / status=review / workspace audit limit=1), which keeps the
// capture unambiguous.

function storeJson(response, sink) {
  response.json().then((body) => sink.push(body)).catch(() => {});
}

function pipelineStats(body) {
  let active = 0;
  let awaiting = 0;
  for (const p of (body.pipelines || [])) {
    if (p.status !== 'done' && p.status !== 'failed') active += 1;
    if (p.status === 'awaiting_approval') awaiting += 1;
  }
  return { active, awaiting };
}

function countText(n) {
  return n === 0 ? 'none' : String(n);
}

function latestAuditBody(sink) {
  for (let i = sink.length - 1; i >= 0; i--) {
    if (sink[i] && Array.isArray(sink[i].events)) return sink[i];
  }
  return null;
}

function workflowField(page, name) {
  return page.locator(`#dashboard-workflow dd[data-dash="${name}"]`);
}

async function expectWorkflowSettled(page, name) {
  await expect.poll(async () => {
    const value = (await workflowField(page, name).innerText()).trim();
    return value === '—' || value === '…' ? null : value;
  }, {
    timeout: 30000,
    intervals: [250, 500, 1000, 2000],
    message: `${name} did not settle`,
  }).not.toBeNull();
}

test('dashboard workflow summary is composed from real API responses', async ({ browser }) => {
  const adminContext = await browser.newContext();
  const founderContext = await browser.newContext();
  const adminPage = await adminContext.newPage();
  const founderPage = await founderContext.newPage();

  const adminPipelines = [];
  const adminPublications = [];
  const adminAudit = [];
  const founderPipelines = [];
  const founderPublications = [];
  const founderAudit = [];

  try {
    adminPage.on('response', (response) => {
      const url = response.url();
      if (url.endsWith('/pipelines?limit=100')) storeJson(response, adminPipelines);
      else if (url.endsWith('/publications?limit=100&status=review')) storeJson(response, adminPublications);
      else if (url.includes('/workspaces/') && url.endsWith('/audit/events?limit=1')) storeJson(response, adminAudit);
    });
    founderPage.on('response', (response) => {
      const url = response.url();
      if (url.endsWith('/pipelines?limit=100')) storeJson(response, founderPipelines);
      else if (url.endsWith('/publications?limit=100&status=review')) storeJson(response, founderPublications);
      else if (url.endsWith('/audit/events?limit=1')) storeJson(response, founderAudit);
    });

    // ---- a workspace admin ----------------------------------------------
    await signIn(adminPage, adminA, browserPassword);
    await expect(adminPage.locator('#dashboard-card')).toBeVisible();
    await expectWorkflowSettled(adminPage, 'active-pipelines');
    await expectWorkflowSettled(adminPage, 'pending-publications');
    await expectWorkflowSettled(adminPage, 'awaiting-pipelines');
    await expectWorkflowSettled(adminPage, 'recent-activity');

    await expect.poll(() => adminPipelines.some((b) => b && Array.isArray(b.pipelines)), {
      timeout: 30000, message: 'admin pipeline read missing',
    }).toBe(true);
    await expect.poll(() => adminPublications.some((b) => b && Array.isArray(b.publications)), {
      timeout: 30000, message: 'admin publication read missing',
    }).toBe(true);
    await expect.poll(() => adminAudit.length > 0, {
      timeout: 30000, message: 'admin workspace audit read missing',
    }).toBe(true);

    const pl = [...adminPipelines].reverse().find((b) => b && Array.isArray(b.pipelines));
    const stats = pipelineStats(pl);
    await expect(workflowField(adminPage, 'active-pipelines')).toHaveText(countText(stats.active));
    await expect(workflowField(adminPage, 'awaiting-pipelines')).toHaveText(countText(stats.awaiting));
    const pub = [...adminPublications].reverse().find((b) => b && Array.isArray(b.publications));
    await expect(workflowField(adminPage, 'pending-publications')).toHaveText(countText(pub.publications.length));

    const adminEv = latestAuditBody(adminAudit);
    if (adminEv && adminEv.events.length > 0) {
      await expect(workflowField(adminPage, 'recent-activity')).toContainText(adminEv.events[0].event_type);
    } else {
      await expect(workflowField(adminPage, 'recent-activity')).toHaveText('No activity recorded.');
    }
    console.log('BROWSER_STEP dashboard-workflow-admin');

    // ---- the workspace-less founder is told the reads are denied ----------
    await signIn(founderPage, founderUsername, founderPassword);
    await expect(founderPage.locator('#dashboard-card')).toBeVisible();
    await expectWorkflowSettled(founderPage, 'active-pipelines');
    await expectWorkflowSettled(founderPage, 'pending-publications');
    await expectWorkflowSettled(founderPage, 'awaiting-pipelines');
    await expectWorkflowSettled(founderPage, 'recent-activity');

    await expect(founderPage.locator('#dashboard-workflow dd[data-dash="active-pipelines"]')).toHaveText('denied');
    await expect(founderPage.locator('#dashboard-workflow dd[data-dash="pending-publications"]')).toHaveText('denied');
    await expect(founderPage.locator('#dashboard-workflow dd[data-dash="awaiting-pipelines"]')).toHaveText('denied');
    await expect.poll(() => founderAudit.some((b) => b && Array.isArray(b.events)), {
      timeout: 30000, message: 'founder org audit read missing',
    }).toBe(true);
    const founderEv = latestAuditBody(founderAudit);
    if (founderEv && founderEv.events.length > 0) {
      await expect(workflowField(founderPage, 'recent-activity')).toContainText(founderEv.events[0].event_type);
    } else {
      await expect(workflowField(founderPage, 'recent-activity')).toHaveText('No activity recorded.');
    }

    // No session material is rendered into the workflow summary.
    const cardText = await founderPage.locator('#dashboard-card').innerText();
    expect(cardText).not.toContain('eyJ');
    expect(cardText).not.toContain('Bearer');
    console.log('BROWSER_STEP dashboard-workflow-founder');
  } finally {
    await Promise.all([adminContext.close(), founderContext.close()]);
  }
});
