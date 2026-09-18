const { test, expect } = require('@playwright/test');
const { signIn } = require('./support');

// Publishing approval vertical slice, exercised through the real browser.
//
// The publish service and its screen already exist; the live Go suite covers the
// state machine, the mandatory human-approval gate and workspace isolation over
// HTTP. This spec pins the browser-level claims those tests cannot see: that a
// draft is created and advanced only through the server's own returned status,
// that a member is shown no approval control and is refused if a request is
// manufactured, that approval then publication is a human-admin action, and that
// another workspace sees neither the list entry nor the publication by id.
//
// It re-tests no server rule. It is deliberately unable to disagree with them.

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

async function refreshPublications(page) {
  const response = page.waitForResponse((r) =>
    r.url().includes('/publications?') && r.request().method() === 'GET');
  await page.locator('#publication-form button[type="submit"]').click();
  await response;
  await expect(page.locator('#publication-loading')).toBeHidden();
}

function publicationRow(page, title) {
  return page.locator('#publication-body-rows tr').filter({ hasText: title });
}

async function rowStatus(page, title) {
  return (await publicationRow(page, title).locator('td').nth(2).innerText()).trim();
}

// The row is re-rendered from the server's answer after every action, so the
// status is polled rather than read once immediately after the request.
async function expectRowStatus(page, title, status) {
  await expect.poll(() => rowStatus(page, title), {
    timeout: 20000,
    intervals: [250, 500, 1000, 2000],
    message: `${title} did not render status ${status}`,
  }).toBe(status);
}

async function browserAPI(page, method, path, payload) {
  return page.evaluate(async ({ method, path, payload }) => {
    const headers = {};
    const accessToken = sessionStorage.getItem('austro.access');
    if (accessToken) headers.Authorization = `Bearer ${accessToken}`;
    if (payload !== undefined) headers['Content-Type'] = 'application/json';
    const response = await fetch(path, {
      method,
      headers,
      body: payload === undefined ? undefined : JSON.stringify(payload),
    });
    let body = null;
    try {
      body = await response.json();
    } catch (_) {}
    return { status: response.status, body };
  }, { method, path, payload });
}

test('publishing journey: draft, submit, human approve, publish', async ({ browser }) => {
  const adminContext = await browser.newContext();
  const memberContext = await browser.newContext();
  const otherContext = await browser.newContext();
  const founderContext = await browser.newContext();
  const adminPage = await adminContext.newPage();
  const memberPage = await memberContext.newPage();
  const otherPage = await otherContext.newPage();
  const founderPage = await founderContext.newPage();
  const title = 'Pub-UI-' + Math.random().toString(36).slice(2, 8);

  try {
    await signIn(adminPage, adminA, browserPassword);
    await expect(adminPage.locator('#publications-card')).toBeVisible();

    // ---- draft through the rendered form --------------------------------
    await adminPage.locator('#publication-title').fill(title);
    await adminPage.locator('#publication-body').fill('created by the publishing browser journey');
    const created = adminPage.waitForResponse((response) =>
      response.url().endsWith('/publications') && response.request().method() === 'POST');
    await adminPage.locator('#publication-create-form button[type="submit"]').click();
    expect((await created).status()).toBe(201);

    await expect(adminPage.locator('#publication-create-message')).toContainText('Draft saved.');
    await expect(publicationRow(adminPage, title)).toHaveCount(1);
    await expectRowStatus(adminPage, title, 'queued');
    console.log('BROWSER_STEP publishing-draft-created');

    const list = await browserAPI(adminPage, 'GET', '/publications?limit=100');
    expect(list.status, `publication list response: ${JSON.stringify(list)}`).toBe(200);
    const pub = (list.body.publications || []).find((p) => p.title === title);
    expect(pub, `publication list body: ${JSON.stringify(list)}`).toBeTruthy();
    expect(pub.id).toMatch(/^[0-9a-f-]{36}$/);
    const pubId = pub.id;

    // ---- submit for review ----------------------------------------------
    await publicationRow(adminPage, title).locator('button', { hasText: 'Submit' }).click();
    await expect(adminPage.locator('#publication-message')).toContainText('Publication is now review.');
    await expectRowStatus(adminPage, title, 'review');
    console.log('BROWSER_STEP publishing-submitted');

    // ---- a member may not approve ---------------------------------------
    // The member can observe the real review state but is shown no action. The
    // same real browser also tries the restricted command directly; the API
    // denies it even if a client manufactures the request.
    await signIn(memberPage, memberA, browserPassword);
    await refreshPublications(memberPage);
    await expectRowStatus(memberPage, title, 'review');
    await expect(publicationRow(memberPage, title).locator('button')).toHaveCount(0);
    await expect(publicationRow(memberPage, title)).not.toContainText('Approve');
    const memberApproval = await browserAPI(memberPage, 'POST', `/publications/${pubId}/approve`);
    console.log('BROWSER_STEP publishing-member-approval-denied', JSON.stringify({ status: memberApproval.status }));
    expect(memberApproval.status, `member approval response: ${JSON.stringify(memberApproval)}`).toBe(403);
    await refreshPublications(memberPage);
    await expectRowStatus(memberPage, title, 'review');
    console.log('BROWSER_STEP publishing-member-denied');

    // ---- approve, then publish ------------------------------------------
    await publicationRow(adminPage, title).locator('button', { hasText: 'Approve' }).click();
    await expect(adminPage.locator('#publication-message')).toContainText('Publication is now approved.');
    await expectRowStatus(adminPage, title, 'approved');

    await publicationRow(adminPage, title).locator('button', { hasText: 'Publish' }).click();
    await expect(adminPage.locator('#publication-message')).toContainText('Publication is now published.');
    await expectRowStatus(adminPage, title, 'published');
    console.log('BROWSER_STEP publishing-published');

    // The member now observes the published, terminal state with no controls.
    await refreshPublications(memberPage);
    await expectRowStatus(memberPage, title, 'published');
    await expect(publicationRow(memberPage, title).locator('button')).toHaveCount(0);

    // ---- another workspace is isolated ----------------------------------
    await signIn(otherPage, adminB, browserPassword);
    await refreshPublications(otherPage);
    await expect(otherPage.locator('#publication-empty')).toBeVisible();
    await expect(otherPage.locator('#publication-body-rows')).not.toContainText(title);
    const crossRead = await browserAPI(otherPage, 'GET', `/publications/${pubId}`);
    console.log('BROWSER_STEP publishing-cross-workspace-denied', JSON.stringify({ status: crossRead.status }));
    expect(crossRead.status, `cross-workspace read response: ${JSON.stringify(crossRead)}`).toBe(404);

    // ---- the founder has no workspace to publish into -------------------
    await signIn(founderPage, founderUsername, founderPassword);
    await refreshPublications(founderPage);
    await expect(founderPage.locator('#publication-message')).toContainText(
      'Publishing requires a workspace identity.');
    await expect(founderPage.locator('#publication-table')).toBeHidden();
    console.log('BROWSER_STEP publishing-founder-denied');
  } finally {
    await Promise.all([
      adminContext.close(),
      memberContext.close(),
      otherContext.close(),
      founderContext.close(),
    ]);
  }
});
