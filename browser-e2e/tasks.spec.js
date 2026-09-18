const { test, expect } = require('@playwright/test');
const { signIn } = require('./support');

// Tasks vertical slice, exercised through the real browser.
//
// The task API and its screen already exist; this spec pins the STATES the
// browser can be in, which is the part nothing else covers. The live Go suite
// (tests/task_api_live_test.go, tests/task_rls_test.go) proves the server's
// behaviour over HTTP: authorization, workspace isolation, the state machine and
// the audit trail. This spec does not re-test any of that. It tests the one
// thing those tests structurally cannot see -- that the page renders what the
// server actually returned, and that a refusal is shown as a refusal.
//
// Four properties are pinned, and each is a claim the UI could quietly get wrong
// while every server-side test stayed green:
//
//   * the row offers exactly the moves the server sent with it, so the browser
//     cannot advertise a transition the domain would refuse;
//   * an empty result renders as an explicit empty state, not as a blank table
//     that reads like a failure;
//   * a terminal state offers no moves at all;
//   * a caller the server refuses is told so. No row is inserted optimistically
//     and no success message is shown for something that did not happen.
//
// Nothing here re-derives the state machine, the authorization rules or the
// workspace scope. Those live in Go, and this file is deliberately unable to
// disagree with them.

function env(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required for browser E2E`);
  return value;
}

const founderUsername = env('BROWSER_E2E_FOUNDER_USERNAME');
const founderPassword = env('BROWSER_E2E_FOUNDER_PASSWORD');
const memberA = env('BROWSER_E2E_MEMBER_A');
const browserPassword = env('BROWSER_E2E_PASSWORD');

// The tasks card loads on entry (enterApp calls loadTasks), so every assertion
// below waits for that first load to settle rather than racing it.
async function waitForTasksLoaded(page) {
  await expect(page.locator('#tasks-card')).toBeVisible();
  await expect(page.locator('#task-loading')).toBeHidden({ timeout: 20000 });
}

function taskRow(page, title) {
  return page.locator('#task-body tr').filter({ hasText: title });
}

// Reads the row the way a person would: the status column, and the set of moves
// the row is offering.
async function readRow(row) {
  const cells = row.locator('td');
  return {
    status: (await cells.nth(1).innerText()).trim(),
    priority: (await cells.nth(2).innerText()).trim(),
    offered: (await cells.nth(4).locator('button').allTextContents())
      .map((s) => s.trim())
      .sort(),
    actionText: (await cells.nth(4).innerText()).trim(),
  };
}

// The reset option carries an empty value, so it is selected by label: an empty
// string is an ambiguous way to name it.
async function filterByStatus(page, option) {
  await page.locator('#task-status').selectOption(option);
  await page.locator('#task-form button[type="submit"]').click();
  await expect(page.locator('#task-loading')).toBeHidden({ timeout: 20000 });
}

// Waits for the transition request for one specific task, so an unrelated
// request cannot satisfy the assertion.
function transitionFor(page, taskId) {
  return page.waitForResponse((response) =>
    response.url().endsWith(`/tasks/${taskId}/transition`) &&
    response.request().method() === 'POST');
}

test('tasks workspace journey: real rows, empty state, and server-directed moves', async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    await signIn(page, memberA, browserPassword);
    await waitForTasksLoaded(page);
    console.log('BROWSER_STEP tasks-card-visible');

    // ---- a real task, created through the UI ----------------------------
    const title = 'Task-UI-' + Math.random().toString(36).slice(2, 8);
    await page.locator('#task-title').fill(title);
    await page.locator('#task-description').fill('created by the tasks browser journey');
    await page.locator('#task-priority').selectOption('high');

    const created = page.waitForResponse((response) =>
      response.url().endsWith('/tasks') && response.request().method() === 'POST');
    await page.locator('#task-create-form button[type="submit"]').click();
    const createdResponse = await created;
    expect(createdResponse.status()).toBe(201);
    const createdBody = await createdResponse.json();
    const taskId = createdBody.id;
    expect(taskId).toMatch(/^[0-9a-f-]{36}$/);
    expect(createdBody.status).toBe('backlog');

    // The success message quotes the server's own record of the task.
    await expect(page.locator('#task-create-message')).toContainText(`Created "${title}".`);
    await expect(page.locator('#task-table')).toBeVisible();
    await expect(page.locator('#task-body')).toContainText(title);
    console.log('BROWSER_STEP tasks-created');

    // A new task is backlog, and backlog offers exactly three moves. These are
    // the transitions the server attached to the row -- if the browser derived
    // them itself, this is where the two would drift apart.
    const row = taskRow(page, title);
    await expect(row).toHaveCount(1);
    let state = await readRow(row);
    expect(state.status).toBe('backlog');
    expect(state.priority).toBe('high');
    expect(state.offered).toEqual(['cancelled', 'failed', 'planned']);
    console.log('BROWSER_STEP tasks-row-rendered');

    // ---- empty state ----------------------------------------------------
    // No test in the suite moves a task into `rejected`, so this filter is
    // deterministic rather than merely likely.
    await filterByStatus(page, { value: 'rejected' });
    await expect(page.locator('#task-empty')).toBeVisible();
    await expect(page.locator('#task-table')).toBeHidden();
    await expect(page.locator('#task-message')).toBeHidden();
    console.log('BROWSER_STEP tasks-empty-state');

    // ---- recovery from the empty state ----------------------------------
    await filterByStatus(page, { label: 'Any' });
    await expect(page.locator('#task-table')).toBeVisible();
    await expect(page.locator('#task-empty')).toBeHidden();
    await expect(page.locator('#task-body')).toContainText(title);
    console.log('BROWSER_STEP tasks-empty-recovered');

    // ---- a forward move is applied from the server's response -----------
    const movedToPlanned = transitionFor(page, taskId);
    await row.locator('td').nth(4).locator('button', { hasText: 'planned' }).click();
    expect((await movedToPlanned).status()).toBe(200);

    await expect(row.locator('td').nth(1)).toHaveText('planned');
    state = await readRow(row);
    expect(state.status).toBe('planned');
    // The moves on offer are the ones `planned` permits, and the move just made
    // is no longer offered -- the row was re-rendered from the server's answer.
    expect(state.offered).toEqual(['cancelled', 'failed', 'in_progress']);
    console.log('BROWSER_STEP tasks-moved-planned');

    // ---- a terminal move ends the lifecycle ------------------------------
    // Reaching a terminal stage is confirmed by the page before it is sent.
    page.once('dialog', (dialog) => dialog.accept());
    const movedToFailed = transitionFor(page, taskId);
    await row.locator('td').nth(4).locator('button', { hasText: 'failed' }).click();
    expect((await movedToFailed).status()).toBe(200);

    await expect(row.locator('td').nth(1)).toHaveText('failed');
    state = await readRow(row);
    expect(state.status).toBe('failed');
    // A final state offers nothing. The absence of controls is the assertion:
    // if the page offered a move out of `failed`, it would be advertising a
    // transition the domain refuses.
    expect(state.offered).toEqual([]);
    expect(state.actionText).toContain('final');
    console.log('BROWSER_STEP tasks-terminal-no-moves');
  } finally {
    await context.close();
  }
});

test('tasks are refused without a workspace, and the page says so', async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    // The founder owns the organization but belongs to no workspace, and tasks
    // are workspace-scoped. The server refuses; this pins that the page shows
    // the refusal instead of an empty table that reads as "you have no tasks".
    await signIn(page, founderUsername, founderPassword);
    await waitForTasksLoaded(page);
    console.log('BROWSER_STEP tasks-founder-card-visible');

    await expect(page.locator('#task-message')).toContainText('not attached to a workspace');
    await expect(page.locator('#task-table')).toBeHidden();
    await expect(page.locator('#task-empty')).toBeHidden();
    console.log('BROWSER_STEP tasks-founder-denied');

    // A refused write must not be acknowledged. The row count is captured first
    // so that an optimistically inserted row would show up here.
    const rowsBefore = await page.locator('#task-body tr').count();

    await page.locator('#task-title').fill('Task-Founder-' + Math.random().toString(36).slice(2, 8));
    const refused = page.waitForResponse((response) =>
      response.url().endsWith('/tasks') && response.request().method() === 'POST');
    await page.locator('#task-create-form button[type="submit"]').click();
    expect((await refused).status()).toBe(403);

    await expect(page.locator('#task-create-message')).toContainText(
      'Your role cannot create tasks in this workspace.');
    await expect(page.locator('#task-create-message')).not.toContainText('Created');
    expect(await page.locator('#task-body tr').count()).toBe(rowsBefore);
    console.log('BROWSER_STEP tasks-founder-write-refused');
  } finally {
    await context.close();
  }
});
