const { test, expect } = require('@playwright/test');
const { execFileSync } = require('node:child_process');
const { appendFileSync, readFileSync } = require('node:fs');

function env(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required for browser E2E`);
  return value;
}

const founderUsername = env('BROWSER_E2E_FOUNDER_USERNAME');
const founderPassword = env('BROWSER_E2E_FOUNDER_PASSWORD');
const adminA = env('BROWSER_E2E_ADMIN_A');
const memberA = env('BROWSER_E2E_MEMBER_A');
const adminB = env('BROWSER_E2E_ADMIN_B');
const browserPassword = env('BROWSER_E2E_PASSWORD');
const workspaceAName = env('BROWSER_E2E_WORKSPACE_A_NAME');
const workspaceBName = env('BROWSER_E2E_WORKSPACE_B_NAME');

async function signIn(page, username, password, observeStates = false) {
  await page.goto('/');
  await expect(page.locator('#auth-view')).toBeVisible();
  if (observeStates) await observeHiddenStates(page);
  await page.locator('#username').fill(username);
  await page.locator('#password').fill(password);
  await page.locator('#login-btn').click();
  await expect(page.locator('#app-view')).toBeVisible();
  await expect(page.locator('#identity')).toContainText(username);
}

async function signOut(page) {
  const logoutResponse = page.waitForResponse((response) =>
    response.url().endsWith('/api/auth/logout') && response.request().method() === 'POST');
  await page.locator('#logout-btn').click();
  const response = await logoutResponse;
  expect(response.status()).toBe(204);
  await expect(page.locator('#auth-view')).toBeVisible();
  await expect(page.locator('#app-view')).toBeHidden();
}

async function refreshPipelines(page) {
  await page.locator('#pipeline-form button[type="submit"]').click();
  await expect(page.locator('#pipeline-loading')).toBeHidden();
}

async function pipelineRows(page) {
  return page.locator('#pipeline-body-rows').innerText();
}

async function waitForPipeline(page, expression, timeout = 90000) {
  await expect.poll(async () => {
    await refreshPipelines(page);
    return pipelineRows(page);
  }, {
    timeout,
    intervals: [250, 500, 1000, 2000],
    message: `rendered pipeline state did not match ${expression}`,
  }).toMatch(expression);
}

async function refreshPublications(page) {
  await page.locator('#publication-form button[type="submit"]').click();
  await expect(page.locator('#publication-loading')).toBeHidden();
}

async function waitForAPI(page) {
  await expect.poll(async () => {
    try {
      const response = await page.request.get('/health/ready', { timeout: 2000 });
      return response.status() === 200;
    } catch (_) {
      return false;
    }
  }, {
    timeout: 60000,
    intervals: [250, 500, 1000, 2000],
    message: 'API did not become ready after the browser outage check',
  }).toBe(true);
}

async function waitForAPIDown(page) {
  await expect.poll(async () => {
    try {
      const response = await page.request.get('/health/ready', { timeout: 2000 });
      return response.status() === 200;
    } catch (_) {
      return false;
    }
  }, {
    timeout: 30000,
    intervals: [100, 250, 500, 1000],
    message: 'API did not stop for the rendered server-error assertion',
  }).toBe(false);
}

function stopAPI() {
  const pid = Number(readFileSync('/tmp/austro-api.pid', 'utf8').trim());
  process.kill(pid, 'SIGTERM');
}

function startAPI() {
  execFileSync('bash', ['-c', 'nohup /tmp/austro-api > /tmp/austro-api.log 2>&1 & echo $! > /tmp/austro-api.pid'], {
    env: process.env,
    stdio: 'ignore',
    timeout: 30000,
  });
}

async function observeHiddenStates(page) {
  await page.evaluate(() => {
    window.__austroUIStates = [];
    const observer = new MutationObserver((records) => {
      for (const record of records) {
        const element = record.target;
        if (!(element instanceof HTMLElement) || !element.id) continue;
        window.__austroUIStates.push({ id: element.id, visible: !element.hidden });
      }
    });
    observer.observe(document, { subtree: true, attributes: true, attributeFilter: ['hidden'] });
    window.__austroUIStateObserver = observer;
  });
}

test('executes the real Creator/Pipeline DOM journey and security journeys', async ({ browser }) => {
  const founderContext = await browser.newContext();
  const adminContext = await browser.newContext();
  const memberContext = await browser.newContext();
  const otherWorkspaceContext = await browser.newContext();
  const staleContext = await browser.newContext();
  const founderPage = await founderContext.newPage();
  const adminPage = await adminContext.newPage();
  const memberPage = await memberContext.newPage();
  const otherWorkspacePage = await otherWorkspaceContext.newPage();
  const stalePage = await staleContext.newPage();
  const founderAuthResponses = [];
  founderPage.on('response', (response) => {
    if (response.url().includes('/api/auth/login') || response.url().endsWith('/api/me')) {
      founderAuthResponses.push({ path: new URL(response.url()).pathname, status: response.status() });
    }
  });

  try {
    // LOGIN → DASHBOARD → WORKSPACE. The first bad login exercises the real
    // rendered authentication error, then the configured Founder enters the
    // actual dashboard and sees the isolated workspaces created for this run.
    await founderPage.goto('/');
    await expect(founderPage.locator('#auth-view')).toBeVisible();
    await founderPage.locator('#username').fill(founderUsername);
    await founderPage.locator('#password').fill('wrong-browser-e2e-password');
    await founderPage.locator('#login-btn').click();
    await expect(founderPage.locator('#auth-message')).toContainText('Invalid username or password.');
    console.log('BROWSER_STEP founder-invalid-login');

    await observeHiddenStates(founderPage);
    await founderPage.locator('#password').fill(founderPassword);
    await founderPage.locator('#login-btn').click();
    console.log('BROWSER_STEP founder-login-submitted');
    try {
      await expect(founderPage.locator('#app-view')).toBeVisible();
    } catch (error) {
      const state = await founderPage.evaluate(() => ({
        authHidden: document.querySelector('#auth-view').hidden,
        appHidden: document.querySelector('#app-view').hidden,
        authMessage: document.querySelector('#auth-message').textContent,
      }));
      state.responses = founderAuthResponses;
      const diagnostic = `BROWSER_DIAGNOSTIC founder-app ${JSON.stringify(state)}; cause: ${error.message}`;
      appendFileSync('/tmp/browser-e2e-diagnostic.log', `${diagnostic}\n`);
      process.stderr.write(`${diagnostic}\n`);
      throw new Error(`BROWSER_DIAGNOSTIC founder-app ${JSON.stringify(state)}; cause: ${error.message}`);
    }
    try {
      await expect.poll(async () => founderPage.locator('#identity').innerText(), {
        timeout: 15000,
        message: `founder identity did not render for ${founderUsername}`,
      }).toContain(founderUsername);
    } catch (error) {
      const state = await founderPage.evaluate(() => ({
        authHidden: document.querySelector('#auth-view').hidden,
        appHidden: document.querySelector('#app-view').hidden,
        authMessage: document.querySelector('#auth-message').textContent,
        identity: document.querySelector('#identity').textContent,
      }));
      state.responses = founderAuthResponses;
      const diagnostic = `BROWSER_DIAGNOSTIC founder-identity ${JSON.stringify(state)}; cause: ${error.message}`;
      appendFileSync('/tmp/browser-e2e-diagnostic.log', `${diagnostic}\n`);
      process.stderr.write(`${diagnostic}\n`);
      throw new Error(`BROWSER_DIAGNOSTIC founder-identity ${JSON.stringify(state)}; cause: ${error.message}`);
    }
    console.log('BROWSER_STEP founder-authenticated');
    async function expectWorkspaceRendered(name, label) {
      try {
        await expect.poll(() => founderPage.locator('#workspace-list').innerText(), {
          timeout: 15000,
          message: `${label} did not render; expected ${name}`,
        }).toContain(name);
      } catch (error) {
        const rendered = await founderPage.locator('#workspace-list').innerText();
        throw new Error(`${label} did not render; expected ${name}; rendered workspace list: ${JSON.stringify(rendered)}; cause: ${error.message}`);
      }
    }
    await expectWorkspaceRendered(workspaceAName, 'workspace A');
    await expectWorkspaceRendered(workspaceBName, 'workspace B');
    console.log('BROWSER_STEP founder-workspaces-rendered');
    await expect(founderPage.locator('#workspace-form')).toBeVisible();

    const founderRefreshBeforeLogout = await founderPage.evaluate(() => sessionStorage.getItem('austro.refresh'));
    expect(founderRefreshBeforeLogout).toBeTruthy();
    await signOut(founderPage);
    await expect(founderPage.evaluate(() => ({
      access: sessionStorage.getItem('austro.access'),
      refresh: sessionStorage.getItem('austro.refresh'),
    }))).resolves.toEqual({ access: null, refresh: null });
    console.log('BROWSER_STEP founder-logout');

    // WORKSPACE → CREATOR/PIPELINE. The admin starts with an isolated empty
    // workspace. The UI has a real loading transition and renders the empty
    // state before a pipeline is created.
    await signIn(adminPage, adminA, browserPassword, true);
    await expect(adminPage.locator('#pipelines-card')).toBeVisible();
    await expect(adminPage.locator('#pipeline-empty')).toBeVisible();
    await expect(adminPage.locator('#pipeline-table')).toBeHidden();
    console.log('BROWSER_STEP admin-empty-state');

    const initialUIStates = await adminPage.evaluate(() => window.__austroUIStates || []);
    expect(initialUIStates).toEqual(expect.arrayContaining([
      expect.objectContaining({ id: 'pipeline-loading', visible: true }),
    ]));

    // A real browser validation error: the publication form has required
    // fields and the browser refuses the empty submission before any request.
    await adminPage.locator('#publication-create-form button[type="submit"]').click();
    await expect.poll(() => adminPage.locator('#publication-title').evaluate((input) => input.validationMessage)).not.toBe('');

    // CREATE PIPELINE. The DOM exposes no stage/status input, select, or
    // arbitrary action control; creation is the single server-owned command.
    await expect(adminPage.locator('#pipeline-create-form [name="stage"], #pipeline-create-form [name="status"]')).toHaveCount(0);
    await adminPage.locator('#pipeline-create-form button[type="submit"]').click();
    await expect(adminPage.locator('#pipeline-create-message')).toContainText('Pipeline started.');
    await expect(adminPage.locator('#pipeline-body-rows tr')).toHaveCount(1);
    await expect(adminPage.locator('#pipeline-body-rows [name="stage"], #pipeline-body-rows [name="status"]')).toHaveCount(0);
    console.log('BROWSER_STEP pipeline-created');

    // VIEW REAL STATUS → RESEARCH → SCRIPT → REVIEW. The test polls the
    // rendered table and waits for the persisted worker-owned handoff; it never
    // sleeps for a guessed duration or posts a state mutation.
    await waitForPipeline(adminPage, /review\s+awaiting_approval/s);
    await expect(adminPage.locator('#pipeline-body-rows')).toContainText('research, script, review');
    await expect(adminPage.locator('#pipeline-body-rows button', { hasText: 'Approve' })).toHaveCount(1);
    await expect(adminPage.locator('#pipeline-body-rows button', { hasText: 'Publish' })).toHaveCount(0);
    await expect(adminPage.locator('#pipeline-body-rows button', { hasText: 'Complete' })).toHaveCount(0);
    console.log('BROWSER_STEP pipeline-review');

    // SECURITY A: a member can observe the real review state but is not shown
    // an approval control. This is an actual second authenticated browser.
    await signIn(memberPage, memberA, browserPassword);
    await waitForPipeline(memberPage, /review\s+awaiting_approval/s);
    await expect(memberPage.locator('#pipeline-body-rows')).toContainText('research, script, review');
    await expect(memberPage.locator('#pipeline-body-rows button')).toHaveCount(0);
    await expect(memberPage.locator('#pipeline-body-rows')).not.toContainText('Approve');

    // SECURITY B: a different workspace's real authenticated admin sees the
    // empty state and no action for workspace A's pipeline.
    await signIn(otherWorkspacePage, adminB, browserPassword);
    await expect(otherWorkspacePage.locator('#identity-details [data-field="workspace"]')).toContainText(process.env.BROWSER_E2E_WORKSPACE_B);
    await expect(otherWorkspacePage.locator('#pipeline-empty')).toBeVisible();
    await expect(otherWorkspacePage.locator('#pipeline-body-rows tr')).toHaveCount(0);
    await expect(otherWorkspacePage.locator('#pipeline-body-rows button')).toHaveCount(0);

    // UI authorization and unsupported-action proof while the pipeline is at
    // review: only the approved action is presented to the admin, and neither
    // stage/status mutation nor publish/complete bypass is exposed.
    const adminActionLabels = await adminPage.locator('#pipeline-body-rows button').allTextContents();
    expect(adminActionLabels).toEqual(['Approve']);

    // UI SERVER-ERROR STATE. Stop the actual API, use the already-rendered
    // authenticated page, and exercise its real failed fetch path. Always
    // restore the API before continuing so later assertions use the same live
    // stack.
    let apiStopped = false;
    try {
      stopAPI();
      apiStopped = true;
      await waitForAPIDown(adminPage);
      await adminPage.locator('#pipeline-form button[type="submit"]').click();
      await expect(adminPage.locator('#pipeline-message')).toContainText('Could not reach the API.');
    } finally {
      if (apiStopped) {
        startAPI();
        await waitForAPI(adminPage);
      }
    }

    // APPROVAL → PUBLISH → COMPLETE. Approval is the only browser action; the
    // worker and publishing boundary advance the persisted aggregate.
    await refreshPipelines(adminPage);
    await expect(adminPage.locator('#pipeline-body-rows button', { hasText: 'Approve' })).toHaveCount(1);
    await adminPage.locator('#pipeline-body-rows button', { hasText: 'Approve' }).click();
    await expect(adminPage.locator('#pipeline-message')).toContainText('Pipeline is now approved.');
    await waitForPipeline(adminPage, /complete\s+done/s);
    await expect(adminPage.locator('#pipeline-body-rows')).toContainText('research, script, review, publication');
    await expect(adminPage.locator('#pipeline-body-rows button', { hasText: 'Approve' })).toHaveCount(0);
    await expect(adminPage.locator('#pipeline-body-rows button', { hasText: 'Complete' })).toHaveCount(0);

    await refreshPublications(adminPage);
    await expect(adminPage.locator('#publication-body-rows')).toContainText('published');

    // SECURITY C: replace the browser's bearer material with a stale session
    // and use the real UI refresh. The API returns 401, refresh fails, and the
    // application clears the session and returns to login rather than retaining
    // stale dashboard data.
    await stalePage.goto('/');
    await signIn(stalePage, memberA, browserPassword);
    await stalePage.evaluate(() => {
      sessionStorage.setItem('austro.access', 'stale-browser-access-token');
      sessionStorage.setItem('austro.refresh', 'stale-browser-refresh-token');
    });
    await stalePage.locator('#pipeline-form button[type="submit"]').click();
    await expect(stalePage.locator('#auth-view')).toBeVisible();
    await expect(stalePage.locator('#app-view')).toBeHidden();

    // AUDIT. The UI exposes the organization audit view to the Founder. Filter
    // and verify the real persisted pipeline events after the worker completed.
    await signIn(founderPage, founderUsername, founderPassword);
    await founderPage.locator('#audit-event-type').fill('pipeline.advance');
    await founderPage.locator('#audit-form button[type="submit"]').click();
    await expect(founderPage.locator('#audit-table')).toBeVisible();
    await expect(founderPage.locator('#audit-body')).toContainText('pipeline.advance');
    await founderPage.locator('#audit-verify-btn').click();
    await expect(founderPage.locator('#audit-verification')).toContainText('Chain intact:');

    // SECURITY D: browser logout calls the existing revocation endpoint,
    // clears sessionStorage, and leaves the page unauthenticated. The saved
    // refresh token is used only inside the browser to prove server revocation;
    // it is never logged or attached to a test error.
    const refreshBeforeLogout = await founderPage.evaluate(() => sessionStorage.getItem('austro.refresh'));
    expect(refreshBeforeLogout).toBeTruthy();
    await signOut(founderPage);
    const revokedStatus = await founderPage.evaluate(async (refreshToken) => {
      const response = await fetch('/api/auth/refresh', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: refreshToken }),
      });
      return response.status;
    }, refreshBeforeLogout);
    expect(revokedStatus).toBe(401);
    await founderPage.reload();
    await expect(founderPage.locator('#auth-view')).toBeVisible();
  } finally {
    await Promise.all([
      founderContext.close(),
      adminContext.close(),
      memberContext.close(),
      otherWorkspaceContext.close(),
      staleContext.close(),
    ]);
  }
});
