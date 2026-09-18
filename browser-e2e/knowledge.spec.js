const { test, expect } = require('@playwright/test');

// Knowledge vertical slice, exercised through the real browser.
//
// The knowledge API and its screen already exist, and the live Go suite covers
// authorization, workspace isolation and the domain rules over HTTP. This spec
// pins the browser-level claims those tests structurally cannot see: that the
// rendered document round-trips the server's own record, that an edit is
// re-read rather than patched locally, that a delete is confirmed and then
// verified against the server's list, and that a refusal is shown as a refusal.
//
// It re-tests no server rule. It is deliberately unable to disagree with them.

function env(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required for browser E2E`);
  return value;
}

const founderUsername = env('BROWSER_E2E_FOUNDER_USERNAME');
const founderPassword = env('BROWSER_E2E_FOUNDER_PASSWORD');
const memberA = env('BROWSER_E2E_MEMBER_A');
const adminB = env('BROWSER_E2E_ADMIN_B');
const browserPassword = env('BROWSER_E2E_PASSWORD');

async function signIn(page, username, password) {
  await page.goto('/');
  await expect(page.locator('#auth-view')).toBeVisible();
  await page.locator('#username').fill(username);
  await page.locator('#password').fill(password);
  await page.locator('#login-btn').click();
  await expect(page.locator('#app-view')).toBeVisible();
  await expect(page.locator('#identity')).toContainText(username);
}

// The knowledge card loads on entry (enterApp calls loadKnowledge), so every
// assertion below waits for that first load to settle rather than racing it.
async function waitForKnowledgeLoaded(page) {
  await expect(page.locator('#knowledge-card')).toBeVisible();
  await expect(page.locator('#kn-loading')).toBeHidden({ timeout: 20000 });
}

function knowledgeRow(page, title) {
  return page.locator('#kn-body tr').filter({ hasText: title });
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

test('knowledge journey: create, view, edit and delete a real document', async ({ browser }) => {
  const memberContext = await browser.newContext();
  const otherContext = await browser.newContext();
  const memberPage = await memberContext.newPage();
  const otherPage = await otherContext.newPage();

  try {
    await signIn(memberPage, memberA, browserPassword);
    await waitForKnowledgeLoaded(memberPage);

    const title = 'Kn-UI-' + Math.random().toString(36).slice(2, 8);
    const content = 'created by the knowledge browser journey';
    const editedTitle = title + '-edited';
    const editedContent = 'rewritten by the knowledge browser journey';

    // ---- create through the rendered form -------------------------------
    await memberPage.locator('#kn-title').fill(title);
    await memberPage.locator('#kn-kind').selectOption('document');
    await memberPage.locator('#kn-content').fill(content);
    const created = memberPage.waitForResponse((response) =>
      response.url().endsWith('/knowledge') && response.request().method() === 'POST');
    await memberPage.locator('#kn-create-form button[type="submit"]').click();
    expect((await created).status()).toBe(201);

    // The success message quotes the server's own record of the document.
    await expect(memberPage.locator('#kn-create-message')).toContainText(`Created \u201c${title}\u201d.`);
    await expect(memberPage.locator('#kn-table')).toBeVisible();
    await expect(memberPage.locator('#kn-body')).toContainText(title);
    console.log('BROWSER_STEP knowledge-created');

    const list = await browserAPI(memberPage, 'GET', '/knowledge?limit=100');
    expect(list.status, `knowledge list response: ${JSON.stringify(list)}`).toBe(200);
    const doc = (list.body.documents || []).find((d) => d.title === title);
    expect(doc, `knowledge list body: ${JSON.stringify(list)}`).toBeTruthy();
    expect(doc.id).toMatch(/^[0-9a-f-]{36}$/);
    const docId = doc.id;

    // ---- view the detail panel ------------------------------------------
    const row = knowledgeRow(memberPage, title);
    await expect(row).toHaveCount(1);
    await row.locator('button', { hasText: 'View' }).click();
    await expect(memberPage.locator('#kn-detail')).toBeVisible();
    await expect(memberPage.locator('#kn-detail-title')).toHaveText(title);
    await expect(memberPage.locator('#kn-detail-kind')).toHaveText('document');
    await expect(memberPage.locator('#kn-detail-content')).toHaveText(content);
    console.log('BROWSER_STEP knowledge-viewed');

    // ---- edit, then re-read from the server ------------------------------
    await memberPage.locator('#kn-edit-btn').click();
    await expect(memberPage.locator('#kn-edit-form')).toBeVisible();
    await memberPage.locator('#kn-edit-title').fill(editedTitle);
    await memberPage.locator('#kn-edit-content').fill(editedContent);
    const edited = memberPage.waitForResponse((response) =>
      response.url().endsWith(`/knowledge/${docId}`) && response.request().method() === 'PATCH');
    await memberPage.locator('#kn-edit-form button[type="submit"]').click();
    expect((await edited).status()).toBe(200);

    // The panel is re-read, not patched locally: it shows the new stored value
    // and the list is re-fetched, so both must reflect the server's answer.
    await expect(memberPage.locator('#kn-detail-title')).toHaveText(editedTitle);
    await expect(memberPage.locator('#kn-detail-content')).toHaveText(editedContent);
    await expect(memberPage.locator('#kn-body')).toContainText(editedTitle);
    console.log('BROWSER_STEP knowledge-edited');

    await memberPage.locator('#kn-detail-close').click();
    await expect(memberPage.locator('#kn-detail')).toBeHidden();

    // ---- cross-workspace isolation --------------------------------------
    // A real authenticated admin of another workspace sees neither the list
    // entry nor the document by id.
    await signIn(otherPage, adminB, browserPassword);
    await waitForKnowledgeLoaded(otherPage);
    await expect(otherPage.locator('#kn-body')).not.toContainText(editedTitle);
    const crossRead = await browserAPI(otherPage, 'GET', `/knowledge/${docId}`);
    console.log('BROWSER_STEP knowledge-cross-workspace-denied', JSON.stringify({ status: crossRead.status }));
    expect(crossRead.status, `cross-workspace read response: ${JSON.stringify(crossRead)}`).toBe(404);

    // ---- delete is confirmed, then verified against the server ----------
    memberPage.once('dialog', (dialog) => dialog.accept());
    const deleted = memberPage.waitForResponse((response) =>
      response.url().endsWith(`/knowledge/${docId}`) && response.request().method() === 'DELETE');
    await knowledgeRow(memberPage, editedTitle).locator('button', { hasText: 'Delete' }).click();
    expect((await deleted).status()).toBe(204);

    // The list is read back from the server, so the deleted row must be gone
    // and the document must no longer resolve by id.
    await expect(knowledgeRow(memberPage, editedTitle)).toHaveCount(0);
    const afterDelete = await browserAPI(memberPage, 'GET', `/knowledge/${docId}`);
    expect(afterDelete.status, `deleted read response: ${JSON.stringify(afterDelete)}`).toBe(404);
    console.log('BROWSER_STEP knowledge-deleted');
  } finally {
    await Promise.all([memberContext.close(), otherContext.close()]);
  }
});

test('knowledge is refused without a workspace, and the page says so', async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    // The founder owns the organization but belongs to no workspace, and
    // knowledge is workspace-scoped. The server refuses; the page shows the
    // refusal instead of an empty list that reads as "you have no documents".
    await signIn(page, founderUsername, founderPassword);
    await waitForKnowledgeLoaded(page);
    console.log('BROWSER_STEP knowledge-founder-card-visible');

    await expect(page.locator('#kn-message')).toContainText('not attached to a workspace');
    await expect(page.locator('#kn-table')).toBeHidden();
    await expect(page.locator('#kn-empty')).toBeHidden();

    // A refused create must not be acknowledged. The row count is captured
    // first so an optimistically inserted row would show up here.
    const rowsBefore = await page.locator('#kn-body tr').count();
    await page.locator('#kn-title').fill('Kn-Founder-' + Math.random().toString(36).slice(2, 8));
    await page.locator('#kn-content').fill('founder cannot write knowledge');
    const refused = page.waitForResponse((response) =>
      response.url().endsWith('/knowledge') && response.request().method() === 'POST');
    await page.locator('#kn-create-form button[type="submit"]').click();
    expect((await refused).status()).toBe(403);

    await expect(page.locator('#kn-create-message')).toContainText(
      'Your role cannot create knowledge in this workspace.');
    await expect(page.locator('#kn-create-message')).not.toContainText('Created');
    expect(await page.locator('#kn-body tr').count()).toBe(rowsBefore);
    console.log('BROWSER_STEP knowledge-founder-write-refused');
  } finally {
    await context.close();
  }
});
