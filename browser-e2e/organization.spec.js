const { test, expect } = require('@playwright/test');

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

async function signIn(page, username, password) {
  await page.goto('/');
  await expect(page.locator('#auth-view')).toBeVisible();
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
    return { status: response.status, body, text: await response.text().catch(() => '') };
  }, { method, path, payload });
}

test('organization hierarchy browser journey', async ({ browser }) => {
  const adminContext = await browser.newContext();
  const memberContext = await browser.newContext();
  const otherContext = await browser.newContext();
  const founderContext = await browser.newContext();
  const adminPage = await adminContext.newPage();
  const memberPage = await memberContext.newPage();
  const otherPage = await otherContext.newPage();
  const founderPage = await founderContext.newPage();

  try {
    // LOGIN → WORKSPACE → DEPARTMENTS → CREATE
    await signIn(adminPage, adminA, browserPassword);
    await expect(adminPage.locator('#departments-card')).toBeVisible();
    await expect(adminPage.locator('#departments-card')).toContainText('Workspace→Departments→Teams→AI Employees');
    console.log('BROWSER_STEP org-admin-login');

    // Wait for departments to load (empty or list)
    await expect(adminPage.locator('#department-loading')).toBeHidden({ timeout: 15000 });
    console.log('BROWSER_STEP org-departments-loaded');

    const deptName = 'Dept-' + Math.random().toString(36).slice(2, 8);
    await adminPage.locator('#department-name').fill(deptName);
    await adminPage.locator('#department-form button[type="submit"]').click();
    await expect(adminPage.locator('#department-message')).toContainText('Department created');
    await expect(adminPage.locator('#department-list')).toContainText(deptName);
    console.log('BROWSER_STEP org-department-created');

    // Get department id via API for later use
    const deptList = await browserAPI(adminPage, 'GET', '/departments?limit=100');
    expect(deptList.status).toBe(200);
    console.log('BROWSER_DIAGNOSTIC deptList:', JSON.stringify(deptList.body).slice(0,1000));
    const dept = deptList.body.departments.find(d => d.name === deptName);
    expect(dept).toBeTruthy();
    const deptId = dept.id;
    console.log('BROWSER_DIAGNOSTIC deptId:', deptId);

    // TEAM → CREATE (via API to avoid flaky select, but UI must show it)
    await expect(adminPage.locator('#teams-card')).toBeVisible();
    await expect(adminPage.locator('#team-loading')).toBeHidden({ timeout: 15000 });
    const teamName = 'Team-' + Math.random().toString(36).slice(2, 8);
    // Create team via API (more reliable than UI select race)
    const teamCreate = await browserAPI(adminPage, 'POST', '/teams', { name: teamName, department_id: deptId });
    console.log('BROWSER_DIAGNOSTIC teamCreate status:', teamCreate.status, 'body:', JSON.stringify(teamCreate.body).slice(0,500));
    expect(teamCreate.status).toBe(201);
    const teamId = teamCreate.body.id;
    // Reload teams UI and verify it appears
    await adminPage.evaluate(() => { if (window.loadTeams) window.loadTeams(false); });
    // Trigger reload via filter form or directly call loadTeams via evaluate
    await adminPage.evaluate(async () => {
      const token = sessionStorage.getItem('austro.access');
      const resp = await fetch('/teams?limit=100', { headers: { Authorization: 'Bearer ' + token } });
      return resp.status;
    });
    // Use API to ensure team exists, then check UI list via reload
    await expect(async () => {
      const list = await browserAPI(adminPage, 'GET', '/teams?limit=100');
      const found = list.body.teams.find(t => t.id === teamId);
      expect(found).toBeTruthy();
    }).toPass({ timeout: 10000 });
    // Force UI reload to verify team appears in UI
    await adminPage.reload();
    await expect(adminPage.locator('#app-view')).toBeVisible({ timeout: 15000 });
    await expect(adminPage.locator('#departments-card')).toBeVisible();
    await expect(adminPage.locator('#team-loading')).toBeHidden({ timeout: 15000 });
    await expect(adminPage.locator('#team-list')).toContainText(teamName, { timeout: 15000 });
    console.log('BROWSER_STEP org-team-created');

    // AI EMPLOYEES → CREATE (via API to avoid flaky select)
    await expect(adminPage.locator('#ai-employees-card')).toBeVisible();
    await expect(adminPage.locator('#employee-loading')).toBeHidden({ timeout: 15000 });
    const empName = 'Emp-' + Math.random().toString(36).slice(2, 8);
    const empRole = 'analyst';
    const empCreate = await browserAPI(adminPage, 'POST', '/ai-employees', { name: empName, role: empRole, team_id: teamId, capabilities: ['research', 'writing'] });
    console.log('BROWSER_DIAGNOSTIC empCreate status:', empCreate.status, 'body:', JSON.stringify(empCreate.body).slice(0,500));
    expect(empCreate.status).toBe(201);
    const empId = empCreate.body.id;
    await adminPage.reload();
    await expect(adminPage.locator('#app-view')).toBeVisible({ timeout: 15000 });
    await expect(adminPage.locator('#employee-loading')).toBeHidden({ timeout: 15000 });
    await expect(adminPage.locator('#employee-list')).toContainText(empName, { timeout: 15000 });
    console.log('BROWSER_STEP org-employee-created');

    const empList = await browserAPI(adminPage, 'GET', '/ai-employees?limit=100');
    expect(empList.status).toBe(200);
    const emp = empList.body.ai_employees.find(e => e.name === empName);
    expect(emp).toBeTruthy();
    expect(emp.team_id).toBe(teamId);
    expect(emp.department_id).toBe(deptId);

    // Verify detail shows hierarchy Workspace→Departments→Teams→AI Employees
    // Click View for employee
    const empViewBtn = adminPage.locator('#employee-list li', { hasText: empName }).locator('button', { hasText: 'View' });
    await empViewBtn.click();
    await expect(adminPage.locator('#employee-detail')).toBeVisible();
    await expect(adminPage.locator('#employee-detail-id')).toContainText(empId.slice(0, 8));
    await expect(adminPage.locator('#employee-detail-team')).toContainText(teamId.slice(0, 6));
    await expect(adminPage.locator('#employee-detail-department')).toContainText(deptId.slice(0, 6));
    await expect(adminPage.locator('#employee-detail-workspace')).not.toBeEmpty();
    console.log('BROWSER_STEP org-employee-detail-verified');
    await adminPage.locator('#employee-detail-close').click();
    await expect(adminPage.locator('#employee-detail')).toBeHidden();

    // TASK ASSIGN via API then UI edit
    const taskRes = await browserAPI(adminPage, 'POST', '/tasks', { title: 'Task-for-' + empName, priority: 'high' });
    expect(taskRes.status).toBe(201);
    const taskId = taskRes.body.id;
    expect(taskId).toMatch(/^[0-9a-f-]{36}$/);
    console.log('BROWSER_STEP org-task-created');

    // Re-open employee detail and assign task
    await adminPage.locator('#employee-list li', { hasText: empName }).locator('button', { hasText: 'View' }).click();
    await expect(adminPage.locator('#employee-detail')).toBeVisible();
    await adminPage.locator('#employee-edit-task').fill(taskId);
    await adminPage.locator('#employee-edit-form button[type="submit"]').click();
    await expect(adminPage.locator('#employee-message')).toContainText('AI employee updated');
    // Verify assignment reflected
    await adminPage.locator('#employee-list li', { hasText: empName }).locator('button', { hasText: 'View' }).click();
    await expect(adminPage.locator('#employee-detail-task')).toContainText(taskId.slice(0, 8));
    console.log('BROWSER_STEP org-employee-assigned');
    await adminPage.locator('#employee-detail-close').click();

    // EDIT department name
    await adminPage.locator('#department-list li', { hasText: deptName }).locator('button', { hasText: 'View' }).click();
    await expect(adminPage.locator('#department-detail')).toBeVisible();
    const newDeptName = deptName + '-renamed';
    await adminPage.locator('#department-edit-name').fill(newDeptName);
    await adminPage.locator('#department-edit-form button[type="submit"]').click();
    await expect(adminPage.locator('#department-message')).toContainText('Department updated');
    await expect(adminPage.locator('#department-list')).toContainText(newDeptName);
    console.log('BROWSER_STEP org-department-edited');

    // EDIT team: create second department and move team
    const dept2Name = 'Dept2-' + Math.random().toString(36).slice(2, 8);
    await adminPage.locator('#department-name').fill(dept2Name);
    await adminPage.locator('#department-form button[type="submit"]').click();
    await expect(adminPage.locator('#department-list')).toContainText(dept2Name);
    const dept2List = await browserAPI(adminPage, 'GET', '/departments?limit=100');
    const dept2 = dept2List.body.departments.find(d => d.name === dept2Name);
    expect(dept2).toBeTruthy();

    await adminPage.locator('#team-list li', { hasText: teamName }).locator('button', { hasText: 'View' }).click();
    await expect(adminPage.locator('#team-detail')).toBeVisible();
    await expect(async () => {
      const values = await adminPage.locator('#team-edit-department option').evaluateAll(els => els.map(e => e.value));
      console.log('BROWSER_DIAGNOSTIC team-edit-department values:', values.join(',').slice(0,500));
      expect(values).toContain(dept2.id);
    }).toPass({ timeout: 20000 });
    await expect(async () => {
      await adminPage.locator('#team-edit-department').selectOption(dept2.id);
      const selected = await adminPage.locator('#team-edit-department').inputValue();
      expect(selected).toBe(dept2.id);
    }).toPass({ timeout: 10000 });
    await adminPage.locator('#team-edit-form button[type="submit"]').click();
    await expect(adminPage.locator('#team-message')).toContainText('Team updated');
    console.log('BROWSER_STEP org-team-moved');

    // AUDIT trail visible to founder
    await signIn(founderPage, founderUsername, founderPassword);
    await founderPage.locator('#audit-event-type').fill('department.create');
    await founderPage.locator('#audit-form button[type="submit"]').click();
    await expect(founderPage.locator('#audit-table')).toBeVisible();
    await expect(founderPage.locator('#audit-body')).toContainText('department.create');
    console.log('BROWSER_STEP org-audit-department');

    await founderPage.locator('#audit-event-type').fill('team.create');
    await founderPage.locator('#audit-form button[type="submit"]').click();
    await expect(founderPage.locator('#audit-body')).toContainText('team.create');
    console.log('BROWSER_STEP org-audit-team');

    await founderPage.locator('#audit-event-type').fill('ai_employee.create');
    await founderPage.locator('#audit-form button[type="submit"]').click();
    await expect(founderPage.locator('#audit-body')).toContainText('ai_employee.create');
    console.log('BROWSER_STEP org-audit-employee');

    await signOut(founderPage);

    // UNAUTHORIZED: member cannot create department/team/employee (UI shows forbidden)
    await signIn(memberPage, memberA, browserPassword);
    await expect(memberPage.locator('#departments-card')).toBeVisible();
    await expect(memberPage.locator('#department-loading')).toBeHidden({ timeout: 15000 });
    // Member can read
    await expect(memberPage.locator('#department-list')).toContainText(newDeptName);
    // Try to create via UI – should be forbidden
    await memberPage.locator('#department-name').fill('Member-should-not-create');
    await memberPage.locator('#department-form button[type="submit"]').click();
    await expect(memberPage.locator('#department-message')).toContainText(/denied|forbidden|Access denied/i);
    console.log('BROWSER_STEP org-member-denied-create');

    // Member cannot see create via API either
    const memberDeptCreate = await browserAPI(memberPage, 'POST', '/departments', { name: 'member-api-create' });
    expect(memberDeptCreate.status).toBe(403);
    console.log('BROWSER_STEP org-member-api-denied');

    await signOut(memberPage);

    // CROSS-WORKSPACE: admin B sees empty, cannot read A's resources
    await signIn(otherPage, adminB, browserPassword);
    await expect(otherPage.locator('#departments-card')).toBeVisible();
    await expect(otherPage.locator('#department-loading')).toBeHidden({ timeout: 15000 });
    // Should not contain A's department
    await expect(otherPage.locator('#department-list')).not.toContainText(newDeptName);
    await expect(otherPage.locator('#department-empty')).toBeVisible();
    console.log('BROWSER_STEP org-cross-workspace-empty');

    const crossRead = await browserAPI(otherPage, 'GET', `/departments/${deptId}`);
    expect(crossRead.status).toBe(404);
    console.log('BROWSER_STEP org-cross-workspace-denied');

    await signOut(otherPage);

    // LOGOUT and cleanup via API as admin A
    await signIn(adminPage, adminA, browserPassword);
    // Delete employee, team, departments via API (faster)
    await browserAPI(adminPage, 'DELETE', `/ai-employees/${empId}`);
    await browserAPI(adminPage, 'DELETE', `/teams/${teamId}`);
    await browserAPI(adminPage, 'DELETE', `/departments/${deptId}`);
    await browserAPI(adminPage, 'DELETE', `/departments/${dept2.id}`);
    console.log('BROWSER_STEP org-cleanup');
    await signOut(adminPage);
    console.log('BROWSER_STEP org-logout');
  } finally {
    await Promise.all([
      adminContext.close(),
      memberContext.close(),
      otherContext.close(),
      founderContext.close(),
    ]);
  }
});
