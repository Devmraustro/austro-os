/* AUSTRO OS browser application (ADR-016).
 *
 * Security properties this file must preserve:
 *
 *  - The token lives in sessionStorage, so it is scoped to this tab and dies
 *    with it. It is never written to localStorage, a cookie, or the URL.
 *  - This file is not an authorization boundary. It only renders what the
 *    server returns; every call is authorized server-side, and a workspace id
 *    is only ever echoed back to the server, never used here to widen access.
 *  - Nothing credential-shaped is logged. Errors surface the server's message,
 *    never a token.
 *  - A 401 triggers exactly one refresh attempt, then a hard sign-out, so an
 *    expired session can never leave stale identity on screen.
 *
 * The Content-Security-Policy served with this page forbids inline handlers, so
 * every listener is registered here with addEventListener.
 */
(function () {
  "use strict";

  var ACCESS = "austro.access";
  var REFRESH = "austro.refresh";
  var currentRole = "";
  /* A single in-flight refresh shared by every caller that hit a 401 at the
   * same time. The server rotates refresh tokens one-time-use, so presenting
   * the same token twice revokes the whole session family; without this guard
   * the dashboard's parallel 401s would each present it and log the user out. */
  var refreshPromise = null;

  function token() { return sessionStorage.getItem(ACCESS); }
  function setTokens(access, refresh) {
    if (access) sessionStorage.setItem(ACCESS, access);
    if (refresh) sessionStorage.setItem(REFRESH, refresh);
  }
  function clearTokens() {
    sessionStorage.removeItem(ACCESS);
    sessionStorage.removeItem(REFRESH);
  }

  function el(id) { return document.getElementById(id); }

  /* ---------- dashboard ---------- */

  /* The dashboard is a live session summary and a capability-access view. Its
   * capability states come from one bounded read per capability, issued as the
   * signed-in caller, so the state shown is the server's own authorization
   * answer for this session rather than a client-side guess from the role. */
  var dashState = {};
  var dashOrder = ["Workspaces", "Departments", "Teams", "AI Employees",
    "Audit", "Tasks", "Knowledge", "Publications", "Pipelines"];

  function setCapability(name, state) {
    dashState[name] = state;
    renderDashboardCapabilities();
  }

  function capabilityFromStatus(status) {
    if (status === 200) return "available";
    if (status === 403) return "denied";
    if (status === 401) return "session expired";
    return "error";
  }

  function renderDashboardCapabilities() {
    var ul = el("dashboard-capabilities");
    if (!ul) return;
    ul.innerHTML = "";
    for (var i = 0; i < dashOrder.length; i++) {
      var name = dashOrder[i];
      var state = dashState[name] || "loading";
      var li = document.createElement("li");
      var label = document.createElement("span");
      label.textContent = name;
      var chip = document.createElement("span");
      chip.textContent = state;
      if (state === "available") chip.className = "state";
      else if (state === "loading") chip.className = "muted";
      else chip.className = "bad";
      li.appendChild(label);
      li.appendChild(chip);
      ul.appendChild(li);
    }
  }

  function setDashAPI(text) {
    var summary = el("dashboard-summary");
    if (!summary) return;
    var node = summary.querySelector('dd[data-dash="api"]');
    if (node) node.textContent = text;
  }

  /* One bounded read per capability, issued as the signed-in caller. The result
   * is the server's own authorization answer for this session, which is exactly
   * the access state the dashboard reports -- the browser never guesses it. */
  var dashProbes = [
    { name: "Workspaces", path: "/workspaces" },
    { name: "Departments", path: "/departments?limit=1" },
    { name: "Teams", path: "/teams?limit=1" },
    { name: "AI Employees", path: "/ai-employees?limit=1" },
    { name: "Audit", path: "/audit/events?limit=1" },
    { name: "Tasks", path: "/tasks?limit=1" },
    { name: "Knowledge", path: "/knowledge?limit=1" },
    { name: "Publications", path: "/publications?limit=1" },
    { name: "Pipelines", path: "/pipelines?limit=1" }
  ];

  function loadDashboard() {
    for (var i = 0; i < dashProbes.length; i++) setCapability(dashProbes[i].name, "loading");
    for (var j = 0; j < dashProbes.length; j++) {
      (function (probe) {
        authenticated("GET", probe.path).then(function (r) {
          setCapability(probe.name, capabilityFromStatus(r.status));
        }).catch(function () {
          setCapability(probe.name, "error");
        });
      })(dashProbes[j]);
    }
  }

  function setMessage(node, text, ok) {
    if (!text) { node.hidden = true; node.textContent = ""; return; }
    node.hidden = false;
    node.textContent = text;
    node.classList.toggle("ok", !!ok);
  }

  /* One JSON request. Returns {status, body}. Never throws on an HTTP error
   * status: callers branch on status so a 401 and a 500 stay distinguishable. */
  function request(method, path, body, useAuth) {
    var headers = {};
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (method === "POST" && path === "/publications" && publicationKey) {
      headers["Idempotency-Key"] = publicationKey;
    }
    if (method === "POST" && path === "/pipelines" && pipelineKey) {
      headers["Idempotency-Key"] = pipelineKey;
    }
    if (useAuth && token()) headers["Authorization"] = "Bearer " + token();
    return fetch(path, {
      method: method,
      headers: headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "omit"
    }).then(function (resp) {
      return resp.text().then(function (text) {
        var parsed = null;
        if (text) {
          try { parsed = JSON.parse(text); } catch (e) { parsed = { error: text }; }
        }
        return { status: resp.status, body: parsed };
      });
    });
  }

  function errorMessage(result) {
    if (result.body && result.body.error) return String(result.body.error);
    return "Request failed with status " + result.status;
  }

  /* Rotate the refresh token at most once at a time. Concurrent callers share
   * the same attempt; the one-time-use token is presented exactly once. On
   * success the new tokens are stored and every caller retries with them. On
   * failure the session is over and the caller is returned to the login view.
   * The promise is cleared once it settles so a later 401 can refresh again. */
  function refreshSession() {
    if (refreshPromise) return refreshPromise;
    var refresh = sessionStorage.getItem(REFRESH);
    if (!refresh) { signOut(false); return Promise.resolve(false); }
    refreshPromise = request("POST", "/api/auth/refresh", { refresh_token: refresh }, false)
      .then(function (rotated) {
        refreshPromise = null;
        if (rotated.status !== 200 || !rotated.body || !rotated.body.access_token) {
          signOut(false);
          return false;
        }
        setTokens(rotated.body.access_token, rotated.body.refresh_token);
        return true;
      })
      .catch(function () {
        refreshPromise = null;
        signOut(false);
        return false;
      });
    return refreshPromise;
  }

  /* An authenticated request with a single shared refresh retry. A stale access
   * token is rotated once; if rotation fails the session is over and the caller
   * is returned to the login view rather than shown partial data. */
  function authenticated(method, path, body) {
    return request(method, path, body, true).then(function (result) {
      if (result.status !== 401) return result;
      return refreshSession().then(function (ok) {
        if (!ok) return result;
        return request(method, path, body, true);
      });
    });
  }

  /* ---------- views ---------- */

  function showAuth() {
    el("app-view").hidden = true;
    el("auth-view").hidden = false;
    el("login-form").reset();
  }

  function showApp() {
    el("auth-view").hidden = true;
    el("app-view").hidden = false;
  }

  function signOut(callServer) {
    var refresh = sessionStorage.getItem(REFRESH);
    clearTokens();
    showAuth();
    setMessage(el("auth-message"), "", false);
    if (callServer && refresh) {
      /* Revocation is best-effort from the client's point of view: the token is
       * already dropped locally, and the server records the attempt. */
      request("POST", "/api/auth/logout", { refresh_token: refresh }, false);
    }
  }

  function renderIdentity(me) {
    currentRole = me.role || "";
    el("identity").textContent = me.username + " · " + me.role;
    var fields = {
      username: me.username,
      role: me.role,
      founder: me.is_founder ? "yes" : "no",
      workspace: me.workspace_id ? me.workspace_id : "organization-level (none)"
    };
    var nodes = el("identity-details").querySelectorAll("dd[data-field]");
    for (var i = 0; i < nodes.length; i++) {
      var key = nodes[i].getAttribute("data-field");
      nodes[i].textContent = key in fields ? fields[key] : "—";
    }
    var summary = el("dashboard-summary");
    if (summary) {
      summary.querySelector('dd[data-dash="identity"]').textContent = me.username + " · " + me.role;
      summary.querySelector('dd[data-dash="scope"]').textContent =
        me.workspace_id ? me.workspace_id : "organization-level (no workspace)";
    }
  }

  function setStatus(field, text, good) {
    var node = el("status-details").querySelector('dd[data-field="' + field + '"]');
    if (!node) return;
    node.textContent = text;
    node.className = "state" + (good === false ? " bad" : "");
  }

  function loadStatus() {
    setStatus("live", "checking…");
    setStatus("ready", "checking…");
    setDashAPI("checking…");
    var live = false;
    var ready = false;
    var settled = 0;
    function settle() {
      settled++;
      if (settled < 2) return;
      if (live && ready) setDashAPI("live and ready");
      else if (live) setDashAPI("live, not ready");
      else setDashAPI("unreachable");
    }
    request("GET", "/health/live").then(function (r) {
      live = r.status === 200;
      setStatus("live", live ? "ok" : "unavailable (" + r.status + ")", live);
      settle();
    }).catch(function () { setStatus("live", "unreachable", false); settle(); });
    request("GET", "/health/ready").then(function (r) {
      ready = r.status === 200;
      setStatus("ready", ready ? "ready" : "not ready (" + r.status + ")", ready);
      settle();
    }).catch(function () { setStatus("ready", "unreachable", false); settle(); });
  }

  function renderWorkspaces(list) {
    var ul = el("workspace-list");
    ul.innerHTML = "";
    el("workspace-empty").hidden = list.length !== 0;
    for (var i = 0; i < list.length; i++) {
      var li = document.createElement("li");
      var name = document.createElement("span");
      name.textContent = list[i].name;
      var id = document.createElement("span");
      id.className = "id";
      id.textContent = list[i].id;
      li.appendChild(name);
      li.appendChild(id);
      ul.appendChild(li);
    }
  }

  function loadWorkspaces() {
    var loading = el("workspace-loading");
    var msg = el("workspace-message");
    setMessage(msg, "", false);
    loading.hidden = false;
    authenticated("GET", "/workspaces").then(function (r) {
      loading.hidden = true;
      if (r.status === 200) {
        renderWorkspaces(Array.isArray(r.body) ? r.body : []);
        return;
      }
      el("workspace-empty").hidden = true;
      el("workspace-list").innerHTML = "";
      if (r.status === 403) {
        setMessage(msg, "Your role cannot list workspaces. The server refused the request.", false);
        return;
      }
      setMessage(msg, errorMessage(r), false);
    }).catch(function () {
      loading.hidden = true;
      setMessage(msg, "Could not reach the API.", false);
    });
  }

  /* ---------- entry ---------- */

  function enterApp() {
    showApp();
    loadStatus();
    loadDashboard();
    loadWorkspaces();
    loadDepartments(false);
    loadTeams(false);
    loadEmployees(false);
    loadAudit(false);
    loadTasks(false);
    loadKnowledge(false);
    return authenticated("GET", "/api/me").then(function (r) {
      if (r.status !== 200 || !r.body) { signOut(false); return; }
      renderIdentity(r.body);
      loadPublications(false);
      loadPipelines(false);
    });
  }

  /* Bootstrap is unauthenticated AND state-changing: on a fresh install it
   * creates the Founder, on an initialized one it writes an "already
   * initialized" audit row, and either way it spends the bootstrap rate limit.
   * So it is never issued speculatively -- merely loading this page must not
   * mutate anything. The operator reveals the panel explicitly and the call is
   * sent only from that button. There is deliberately no status probe: telling
   * an unauthenticated caller whether the organization is initialized is not
   * worth an extra public endpoint. */
  function initAuthView() {
    showAuth();
  }

  el("bootstrap-toggle").addEventListener("click", function () {
    var panel = el("bootstrap-panel");
    panel.hidden = !panel.hidden;
  });

  el("login-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var btn = el("login-btn");
    var msg = el("auth-message");
    btn.disabled = true;
    setMessage(msg, "", false);
    request("POST", "/api/auth/login", {
      username: el("username").value,
      password: el("password").value
    }, false).then(function (r) {
      btn.disabled = false;
      if (r.status === 200 && r.body && r.body.access_token) {
        setTokens(r.body.access_token, r.body.refresh_token);
        enterApp();
        return;
      }
      if (r.status === 401) {
        setMessage(msg, "Invalid username or password.", false);
        return;
      }
      setMessage(msg, errorMessage(r), false);
    }).catch(function () {
      btn.disabled = false;
      setMessage(msg, "Could not reach the API.", false);
    });
  });

  el("bootstrap-btn").addEventListener("click", function () {
    var btn = el("bootstrap-btn");
    var msg = el("auth-message");
    btn.disabled = true;
    setMessage(msg, "", false);
    request("POST", "/api/auth/bootstrap", {}, false).then(function (r) {
      btn.disabled = false;
      if (r.status === 201) {
        el("bootstrap-panel").hidden = true;
        setMessage(msg, "Founder created. Sign in with the configured credentials.", true);
        return;
      }
      if (r.status === 409) {
        el("bootstrap-panel").hidden = true;
        setMessage(msg, "The organization is already initialized. Sign in.", false);
        return;
      }
      setMessage(msg, errorMessage(r), false);
    }).catch(function () {
      btn.disabled = false;
      setMessage(msg, "Could not reach the API.", false);
    });
  });

  el("logout-btn").addEventListener("click", function () { signOut(true); });

  el("dashboard-refresh-btn").addEventListener("click", function () { loadDashboard(); });

  el("workspace-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var msg = el("workspace-message");
    setMessage(msg, "", false);
    authenticated("POST", "/workspaces", { name: el("workspace-name").value })
      .then(function (r) {
        if (r.status === 200 || r.status === 201) {
          el("workspace-form").reset();
          setMessage(msg, "Workspace created.", true);
          loadWorkspaces();
          return;
        }
        setMessage(msg, errorMessage(r), false);
      }).catch(function () { setMessage(msg, "Could not reach the API.", false); });
  });

  /* ---------- departments ---------- */
  var departmentCursor = "";
  var departmentCurrent = null;

  function renderDepartments(list) {
    var ul = el("department-list");
    ul.innerHTML = "";
    var empty = el("department-empty");
    var denied = el("department-denied");
    empty.hidden = list.length !== 0;
    denied.hidden = true;
    var deptSelects = [el("team-department"), el("team-filter-department"), el("team-edit-department")];
    for (var s = 0; s < deptSelects.length; s++) {
      if (!deptSelects[s]) continue;
      var currentVal = deptSelects[s].value;
      // Keep first option for filter, clear rest but preserve existing if list empty to avoid race clearing
      var keepFirst = deptSelects[s].id.indexOf("filter") !== -1;
      var first = keepFirst && deptSelects[s].options.length > 0 ? deptSelects[s].options[0] : null;
      // Build map of existing values to preserve if needed
      var existingMap = {};
      for (var e = 0; e < deptSelects[s].options.length; e++) {
        existingMap[deptSelects[s].options[e].value] = deptSelects[s].options[e].textContent;
      }
      deptSelects[s].innerHTML = "";
      if (first) deptSelects[s].appendChild(first);
      if (list.length === 0) {
        // If API returned empty (race), keep existing options to avoid clearing UI that already has optimistic entry
        for (var k in existingMap) {
          if (k === "" && keepFirst) continue;
          if (existingMap.hasOwnProperty(k)) {
            var optKeep = document.createElement("option");
            optKeep.value = k;
            optKeep.textContent = existingMap[k];
            deptSelects[s].appendChild(optKeep);
          }
        }
      } else {
        for (var i = 0; i < list.length; i++) {
          var opt = document.createElement("option");
          opt.value = list[i].id;
          opt.textContent = list[i].name;
          deptSelects[s].appendChild(opt);
        }
      }
      if (currentVal) deptSelects[s].value = currentVal;
    }
    for (var i = 0; i < list.length; i++) {
      (function (dept) {
        var li = document.createElement("li");
        var name = document.createElement("span");
        name.textContent = dept.name + " (" + dept.id.slice(0,8) + ")";
        var btn = document.createElement("button");
        btn.type = "button";
        btn.textContent = "View";
        btn.addEventListener("click", function () { showDepartment(dept.id); });
        li.appendChild(name);
        li.appendChild(btn);
        ul.appendChild(li);
      })(list[i]);
    }
  }

  function loadDepartments(append) {
    var loading = el("department-loading");
    var msg = el("department-message");
    var denied = el("department-denied");
    if (!append) departmentCursor = "";
    loading.hidden = false;
    setMessage(msg, "", false);
    var path = "/departments?limit=100" + (append && departmentCursor ? "&cursor=" + encodeURIComponent(departmentCursor) : "");
    return authenticated("GET", path).then(function (r) {
      loading.hidden = true;
      if (r.status === 403) {
        el("department-list").innerHTML = "";
        el("department-empty").hidden = true;
        denied.hidden = false;
        setMessage(msg, "Access denied or no workspace context.", false);
        return;
      }
      if (r.status !== 200 || !r.body) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      var depts = r.body.departments || [];
      if (!append) renderDepartments(depts);
      else {
        // append not fully supported, just re-render combined? For simplicity reload.
        loadDepartments(false);
        return;
      }
      departmentCursor = r.body.next_cursor || "";
      setMessage(msg, "", true);
    }).catch(function () {
      loading.hidden = true;
      setMessage(msg, "Could not reach API.", false);
    });
  }

  function showDepartment(id) {
    var msg = el("department-message");
    setMessage(msg, "", false);
    authenticated("GET", "/departments/" + encodeURIComponent(id)).then(function (r) {
      if (r.status !== 200 || !r.body) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      departmentCurrent = r.body;
      el("department-detail-id").textContent = r.body.id;
      el("department-detail-name").textContent = r.body.name;
      el("department-detail-workspace").textContent = r.body.workspace_id;
      el("department-edit-name").value = r.body.name;
      el("department-detail").hidden = false;
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  }

  el("department-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var msg = el("department-message");
    setMessage(msg, "", false);
    authenticated("POST", "/departments", { name: el("department-name").value }).then(function (r) {
      if (r.status !== 201) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      el("department-form").reset();
      if (r.body && r.body.id) {
        var deptSelects = [el("team-department"), el("team-filter-department"), el("team-edit-department")];
        for (var s = 0; s < deptSelects.length; s++) {
          if (!deptSelects[s]) continue;
          var exists = false;
          for (var o = 0; o < deptSelects[s].options.length; o++) {
            if (deptSelects[s].options[o].value === r.body.id) { exists = true; break; }
          }
          if (!exists) {
            var opt = document.createElement("option");
            opt.value = r.body.id;
            opt.textContent = r.body.name;
            deptSelects[s].appendChild(opt);
          }
        }
      }
      loadDepartments(false).then(function() {
        setMessage(msg, "Department created: " + r.body.name, true);
      });
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  });

  el("department-edit-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    if (!departmentCurrent) return;
    var msg = el("department-message");
    setMessage(msg, "", false);
    authenticated("PATCH", "/departments/" + encodeURIComponent(departmentCurrent.id), { name: el("department-edit-name").value }).then(function (r) {
      if (r.status !== 200) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      el("department-detail").hidden = true;
      departmentCurrent = null;
      loadDepartments(false).then(function() {
        setMessage(msg, "Department updated.", true);
      });
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  });

  el("department-edit-cancel").addEventListener("click", function () {
    el("department-detail").hidden = true;
    departmentCurrent = null;
  });
  el("department-detail-close").addEventListener("click", function () {
    el("department-detail").hidden = true;
    departmentCurrent = null;
  });

  /* ---------- teams ---------- */
  var teamCursor = "";
  var teamCurrent = null;

  function renderTeams(list) {
    var ul = el("team-list");
    ul.innerHTML = "";
    el("team-empty").hidden = list.length !== 0;
    el("team-denied").hidden = true;
    var teamSelects = [el("employee-team"), el("employee-filter-team"), el("employee-edit-team")];
    for (var s = 0; s < teamSelects.length; s++) {
      if (!teamSelects[s]) continue;
      var currentVal = teamSelects[s].value;
      var keepFirst = teamSelects[s].id.indexOf("filter") !== -1;
      var first = keepFirst && teamSelects[s].options.length > 0 ? teamSelects[s].options[0] : null;
      teamSelects[s].innerHTML = "";
      if (first) teamSelects[s].appendChild(first);
      for (var i = 0; i < list.length; i++) {
        var opt = document.createElement("option");
        opt.value = list[i].id;
        opt.textContent = list[i].name + " (" + list[i].department_id.slice(0,6) + ")";
        teamSelects[s].appendChild(opt);
      }
      if (currentVal) teamSelects[s].value = currentVal;
    }
    for (var i = 0; i < list.length; i++) {
      (function (t) {
        var li = document.createElement("li");
        var name = document.createElement("span");
        name.textContent = t.name + " [dept " + t.department_id.slice(0,6) + "] (" + t.id.slice(0,8) + ")";
        var btn = document.createElement("button");
        btn.type = "button";
        btn.textContent = "View";
        btn.addEventListener("click", function () { showTeam(t.id); });
        li.appendChild(name);
        li.appendChild(btn);
        ul.appendChild(li);
      })(list[i]);
    }
  }

  function loadTeams(append) {
    var loading = el("team-loading");
    var msg = el("team-message");
    if (!append) teamCursor = "";
    loading.hidden = false;
    setMessage(msg, "", false);
    var deptFilter = el("team-filter-department").value;
    var path = "/teams?limit=100" + (deptFilter ? "&department_id=" + encodeURIComponent(deptFilter) : "") + (append && teamCursor ? "&cursor=" + encodeURIComponent(teamCursor) : "");
    return authenticated("GET", path).then(function (r) {
      loading.hidden = true;
      if (r.status === 403) {
        el("team-list").innerHTML = "";
        el("team-empty").hidden = true;
        el("team-denied").hidden = false;
        setMessage(msg, "Access denied.", false);
        return;
      }
      if (r.status !== 200 || !r.body) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      var teams = r.body.teams || [];
      renderTeams(teams);
      teamCursor = r.body.next_cursor || "";
      setMessage(msg, "", true);
    }).catch(function () {
      loading.hidden = true;
      setMessage(msg, "Could not reach API.", false);
    });
  }

  function showTeam(id) {
    var msg = el("team-message");
    setMessage(msg, "", false);
    authenticated("GET", "/teams/" + encodeURIComponent(id)).then(function (r) {
      if (r.status !== 200 || !r.body) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      teamCurrent = r.body;
      el("team-detail-id").textContent = r.body.id;
      el("team-detail-name").textContent = r.body.name;
      el("team-detail-department").textContent = r.body.department_id;
      el("team-detail-workspace").textContent = r.body.workspace_id;
      el("team-edit-name").value = r.body.name;
      // populate edit department select with current departments
      var sel = el("team-edit-department");
      if (sel) sel.value = r.body.department_id;
      el("team-detail").hidden = false;
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  }

  el("team-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var msg = el("team-message");
    setMessage(msg, "", false);
    authenticated("POST", "/teams", { name: el("team-name").value, department_id: el("team-department").value }).then(function (r) {
      if (r.status !== 201) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      el("team-form").reset();
      if (r.body && r.body.id) {
        var teamSelects = [el("employee-team"), el("employee-filter-team"), el("employee-edit-team")];
        for (var s = 0; s < teamSelects.length; s++) {
          if (!teamSelects[s]) continue;
          var exists = false;
          for (var o = 0; o < teamSelects[s].options.length; o++) {
            if (teamSelects[s].options[o].value === r.body.id) { exists = true; break; }
          }
          if (!exists) {
            var opt = document.createElement("option");
            opt.value = r.body.id;
            opt.textContent = r.body.name + " (" + r.body.department_id.slice(0,6) + ")";
            teamSelects[s].appendChild(opt);
          }
        }
      }
      loadTeams(false).then(function() {
        setMessage(msg, "Team created: " + r.body.name, true);
      });
      loadEmployees(false);
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  });

  el("team-filter-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadTeams(false);
  });

  el("team-edit-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    if (!teamCurrent) return;
    var msg = el("team-message");
    setMessage(msg, "", false);
    var payload = {};
    var name = el("team-edit-name").value.trim();
    if (name) payload.name = name;
    var dept = el("team-edit-department").value;
    if (dept) payload.department_id = dept;
    authenticated("PATCH", "/teams/" + encodeURIComponent(teamCurrent.id), payload).then(function (r) {
      if (r.status !== 200) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      el("team-detail").hidden = true;
      teamCurrent = null;
      loadTeams(false).then(function() {
        setMessage(msg, "Team updated.", true);
      });
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  });

  el("team-edit-cancel").addEventListener("click", function () {
    el("team-detail").hidden = true;
    teamCurrent = null;
  });
  el("team-detail-close").addEventListener("click", function () {
    el("team-detail").hidden = true;
    teamCurrent = null;
  });

  /* ---------- AI employees ---------- */
  var employeeCursor = "";
  var employeeCurrent = null;

  function renderEmployees(list) {
    var ul = el("employee-list");
    ul.innerHTML = "";
    el("employee-empty").hidden = list.length !== 0;
    el("employee-denied").hidden = true;
    for (var i = 0; i < list.length; i++) {
      (function (emp) {
        var li = document.createElement("li");
        var name = document.createElement("span");
        name.textContent = emp.name + " (" + emp.role + ") [team " + emp.team_id.slice(0,6) + "] (" + emp.id.slice(0,8) + ")";
        var btn = document.createElement("button");
        btn.type = "button";
        btn.textContent = "View";
        btn.addEventListener("click", function () { showEmployee(emp.id); });
        li.appendChild(name);
        li.appendChild(btn);
        ul.appendChild(li);
      })(list[i]);
    }
  }

  function loadEmployees(append) {
    var loading = el("employee-loading");
    var msg = el("employee-message");
    if (!append) employeeCursor = "";
    loading.hidden = false;
    setMessage(msg, "", false);
    var teamFilter = el("employee-filter-team").value;
    var path = "/ai-employees?limit=100" + (teamFilter ? "&team_id=" + encodeURIComponent(teamFilter) : "") + (append && employeeCursor ? "&cursor=" + encodeURIComponent(employeeCursor) : "");
    return authenticated("GET", path).then(function (r) {
      loading.hidden = true;
      if (r.status === 403) {
        el("employee-list").innerHTML = "";
        el("employee-empty").hidden = true;
        el("employee-denied").hidden = false;
        setMessage(msg, "Access denied.", false);
        return;
      }
      if (r.status !== 200 || !r.body) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      var emps = r.body.ai_employees || [];
      renderEmployees(emps);
      employeeCursor = r.body.next_cursor || "";
      setMessage(msg, "", true);
    }).catch(function () {
      loading.hidden = true;
      setMessage(msg, "Could not reach API.", false);
    });
  }

  function showEmployee(id) {
    var msg = el("employee-message");
    setMessage(msg, "", false);
    authenticated("GET", "/ai-employees/" + encodeURIComponent(id)).then(function (r) {
      if (r.status !== 200 || !r.body) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      employeeCurrent = r.body;
      el("employee-detail-id").textContent = r.body.id;
      el("employee-detail-name").textContent = r.body.name;
      el("employee-detail-role").textContent = r.body.role;
      el("employee-detail-team").textContent = r.body.team_id;
      el("employee-detail-department").textContent = r.body.department_id;
      el("employee-detail-workspace").textContent = r.body.workspace_id;
      el("employee-detail-capabilities").textContent = (r.body.capabilities || []).join(", ") || "—";
      el("employee-detail-task").textContent = r.body.current_task_id || "—";
      el("employee-edit-name").value = r.body.name;
      el("employee-edit-role").value = r.body.role;
      el("employee-edit-capabilities").value = (r.body.capabilities || []).join(", ");
      el("employee-edit-task").value = r.body.current_task_id || "";
      var sel = el("employee-edit-team");
      if (sel) sel.value = r.body.team_id;
      el("employee-detail").hidden = false;
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  }

  el("employee-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var msg = el("employee-message");
    setMessage(msg, "", false);
    var capsRaw = el("employee-capabilities").value;
    var caps = capsRaw ? capsRaw.split(",").map(function (s) { return s.trim(); }).filter(function (s) { return s; }) : [];
    authenticated("POST", "/ai-employees", { name: el("employee-name").value, role: el("employee-role").value, team_id: el("employee-team").value, capabilities: caps }).then(function (r) {
      if (r.status !== 201) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      el("employee-form").reset();
      loadEmployees(false).then(function() {
        setMessage(msg, "AI employee created: " + r.body.name, true);
      });
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  });

  el("employee-filter-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadEmployees(false);
  });

  el("employee-edit-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    if (!employeeCurrent) return;
    var msg = el("employee-message");
    setMessage(msg, "", false);
    var capsRaw = el("employee-edit-capabilities").value;
    var caps = capsRaw ? capsRaw.split(",").map(function (s) { return s.trim(); }).filter(function (s) { return s; }) : undefined;
    var payload = {};
    var name = el("employee-edit-name").value.trim();
    if (name) payload.name = name;
    var role = el("employee-edit-role").value.trim();
    if (role) payload.role = role;
    var team = el("employee-edit-team").value;
    if (team) payload.team_id = team;
    if (caps !== undefined) payload.capabilities = caps;
    var task = el("employee-edit-task").value.trim();
    if (task !== "" || el("employee-edit-task").value === "") {
      // If user cleared, send empty string to clear; if provided, send UUID
      if (task === "") {
        payload.current_task_id = "";
      } else {
        payload.current_task_id = task;
      }
    }
    // If task field untouched and empty originally, don't send to avoid clearing unintentionally
    if (!task && !employeeCurrent.current_task_id) {
      delete payload.current_task_id;
    }
    authenticated("PATCH", "/ai-employees/" + encodeURIComponent(employeeCurrent.id), payload).then(function (r) {
      if (r.status !== 200) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      el("employee-detail").hidden = true;
      employeeCurrent = null;
      loadEmployees(false).then(function() {
        setMessage(msg, "AI employee updated.", true);
      });
    }).catch(function () {
      setMessage(msg, "Could not reach API.", false);
    });
  });

  el("employee-edit-cancel").addEventListener("click", function () {
    el("employee-detail").hidden = true;
    employeeCurrent = null;
  });
  el("employee-detail-close").addEventListener("click", function () {
    el("employee-detail").hidden = true;
    employeeCurrent = null;
  });

  /* ---------- audit trail ---------- */

  /* The cursor for "load older". It is module state rather than a DOM value so
   * that changing a filter and reloading resets the walk instead of silently
   * continuing it under different conditions. */
  var auditCursor = "";

  function auditQuery(append) {
    var parts = ["limit=" + encodeURIComponent(el("audit-limit").value)];
    var eventType = el("audit-event-type").value.trim();
    var outcome = el("audit-outcome").value;
    if (eventType) parts.push("event_type=" + encodeURIComponent(eventType));
    if (outcome) parts.push("outcome=" + encodeURIComponent(outcome));
    if (append && auditCursor) parts.push("before_seq=" + encodeURIComponent(auditCursor));
    return "/audit/events?" + parts.join("&");
  }

  /* Rows are built with createElement and textContent, never innerHTML. Audit
   * detail is recorded by whatever code produced the event, so it is exactly
   * the kind of value that must never be parsed as markup. */
  function renderAuditRows(events, append) {
    var body = el("audit-body");
    if (!append) body.innerHTML = "";
    for (var i = 0; i < events.length; i++) {
      var ev = events[i];
      var tr = document.createElement("tr");
      var cells = [
        String(ev.seq),
        ev.timestamp ? String(ev.timestamp).replace("T", " ").slice(0, 23) : "—",
        ev.event_type,
        ev.outcome,
        ev.actor_type + (ev.actor_id ? " " + String(ev.actor_id).slice(0, 8) : ""),
        ev.workspace_id ? String(ev.workspace_id).slice(0, 8) : "org",
        ev.constitutional_principle
      ];
      for (var c = 0; c < cells.length; c++) {
        var td = document.createElement("td");
        td.textContent = cells[c];
        tr.appendChild(td);
      }
      body.appendChild(tr);
    }
  }

  function loadAudit(append) {
    var loading = el("audit-loading");
    var msg = el("audit-message");
    var table = el("audit-table");
    var empty = el("audit-empty");
    var older = el("audit-older-btn");
    if (!append) { auditCursor = ""; }
    loading.hidden = false;
    setMessage(msg, "", false);
    el("audit-verification").hidden = true;

    return authenticated("GET", auditQuery(append)).then(function (r) {
      loading.hidden = true;
      if (r.status === 403) {
        table.hidden = true;
        empty.hidden = true;
        older.hidden = true;
        setMessage(msg,
          "Your role cannot read the organization-wide audit log. " +
          "This view is founder-only, because it spans every workspace.", false);
        return;
      }
      if (r.status !== 200 || !r.body) {
        setMessage(msg, errorMessage(r) + " — adjust the filters and retry.", false);
        return;
      }
      var events = r.body.events || [];
      renderAuditRows(events, !!append);
      var total = el("audit-body").children.length;
      table.hidden = total === 0;
      empty.hidden = total !== 0;
      auditCursor = r.body.next_cursor || "";
      older.hidden = auditCursor === "";
    }).catch(function () {
      loading.hidden = true;
      setMessage(msg, "Could not reach the API. Check the connection and retry.", false);
    });
  }

  el("audit-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadAudit(false);
  });

  el("audit-older-btn").addEventListener("click", function () {
    loadAudit(true);
  });

  el("audit-verify-btn").addEventListener("click", function () {
    var out = el("audit-verification");
    out.hidden = false;
    out.classList.remove("bad");
    out.textContent = "Recomputing every link in the chain…";
    authenticated("GET", "/audit/verification").then(function (r) {
      if (r.status === 403) {
        out.textContent = "Chain verification is founder-only.";
        return;
      }
      if (r.status !== 200 || !r.body) {
        out.textContent = errorMessage(r);
        return;
      }
      out.textContent = r.body.verified
        ? "Chain intact: " + r.body.events_checked + " events verified."
        : "CHAIN BROKEN: " + r.body.events_checked +
          " events checked and the hash chain did not verify.";
      if (!r.body.verified) out.classList.add("bad");
    }).catch(function () {
      out.textContent = "Could not reach the API.";
    });
  });

  /* ---------- tasks ---------- */

  /* Cursor for "load more". Module state rather than a DOM value, so changing a
   * filter and reloading resets the walk instead of continuing it under
   * different conditions. */
  var taskCursor = "";

  /* Terminal stages. A transition into one of these ends the task's useful life,
   * so the UI asks before making it. The server makes the real decision; this
   * only decides whether to interrupt the user first. */
  var terminalStatuses = { completed: 1, cancelled: 1, failed: 1, rejected: 1 };

  function taskQuery(append) {
    var parts = ["limit=" + encodeURIComponent(el("task-limit").value)];
    var status = el("task-status").value;
    if (status) parts.push("status=" + encodeURIComponent(status));
    if (append && taskCursor) parts.push("cursor=" + encodeURIComponent(taskCursor));
    return "/tasks?" + parts.join("&");
  }

  /* Rows are built with createElement and textContent, never innerHTML: a task
   * title is user-supplied text and must never be parsed as markup. */
  function renderTaskRows(tasks, append) {
    var body = el("task-body");
    if (!append) body.innerHTML = "";
    for (var i = 0; i < tasks.length; i++) {
      (function (task) {
        var tr = document.createElement("tr");

        var title = document.createElement("td");
        title.textContent = task.title;
        tr.appendChild(title);

        var status = document.createElement("td");
        status.textContent = task.status;
        tr.appendChild(status);

        var priority = document.createElement("td");
        priority.textContent = task.priority;
        tr.appendChild(priority);

        var created = document.createElement("td");
        created.textContent = task.created_at
          ? String(task.created_at).replace("T", " ").slice(0, 19)
          : "\u2014";
        tr.appendChild(created);

        /* The legal transitions are exactly the ones the server sent with this
         * row. Nothing here re-derives the state machine, so the browser cannot
         * offer a move the domain would refuse -- and if the server's answer and
         * this page disagreed, the server would still win. */
        var actions = document.createElement("td");
        var transitions = task.transitions || [];
        if (transitions.length === 0) {
          var none = document.createElement("span");
          none.className = "muted";
          none.textContent = "\u2014 (final)";
          actions.appendChild(none);
        }
        for (var j = 0; j < transitions.length; j++) {
          actions.appendChild(transitionButton(task, transitions[j]));
          if (j < transitions.length - 1) actions.appendChild(document.createTextNode(" "));
        }
        tr.appendChild(actions);

        body.appendChild(tr);
      })(tasks[i]);
    }
  }

  function transitionButton(task, status) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = status;
    btn.addEventListener("click", function () {
      /* A move into a terminal stage is not reversible from this page, so it is
       * confirmed. Forward moves are ordinary edits and are not interrupted. */
      if (terminalStatuses[status]) {
        var sure = window.confirm(
          "Move \"" + task.title + "\" to " + status + "? " +
          "This ends the task's active lifecycle."
        );
        if (!sure) return;
      }
      applyTransition(task.id, status);
    });
    return btn;
  }

  function applyTransition(id, status) {
    var msg = el("task-message");
    setMessage(msg, "", false);
    return authenticated("POST", "/tasks/" + encodeURIComponent(id) + "/transition",
      { status: status })
      .then(function (r) {
        if (r.status !== 200) {
          setMessage(msg, "Transition refused: " + errorMessage(r), false);
          return null;
        }
        setMessage(msg, "Moved to " + status + ".", true);
        /* Read the list back so what is on screen is what the server now holds,
         * rather than a locally predicted row. */
        return loadTasks(false);
      })
      .catch(function () {
        setMessage(msg, "Could not reach the API. The task was not changed.", false);
      });
  }

  function loadTasks(append) {
    var loading = el("task-loading");
    var msg = el("task-message");
    var table = el("task-table");
    var empty = el("task-empty");
    var older = el("task-older-btn");
    if (!append) { taskCursor = ""; }
    loading.hidden = false;

    return authenticated("GET", taskQuery(append)).then(function (r) {
      loading.hidden = true;
      if (r.status === 403) {
        table.hidden = true;
        empty.hidden = true;
        older.hidden = true;
        setMessage(msg,
          "Tasks are workspace-scoped, and your current session is not attached " +
          "to a workspace, so the server refused this request. Sign in as a " +
          "workspace member or administrator to use them.", false);
        return;
      }
      if (r.status === 401) {
        /* The refresh retry already happened inside authenticated(); reaching
         * here means the session is genuinely over and the user has been sent
         * back to the login view. Say so rather than showing an empty table. */
        setMessage(msg, "Your session expired. Sign in again to see tasks.", false);
        return;
      }
      if (r.status !== 200 || !r.body) {
        setMessage(msg, errorMessage(r) + " — adjust the filters and retry.", false);
        return;
      }
      renderTaskRows(r.body.tasks || [], !!append);
      var total = el("task-body").children.length;
      table.hidden = total === 0;
      empty.hidden = total !== 0;
      taskCursor = r.body.next_cursor || "";
      older.hidden = taskCursor === "";
      setMessage(msg, "", true);
    }).catch(function () {
      loading.hidden = true;
      setMessage(msg, "Could not reach the API. Check the connection and retry.", false);
    });
  }

  el("task-create-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var msg = el("task-create-message");
    setMessage(msg, "", false);
    var payload = { title: el("task-title").value };
    var description = el("task-description").value.trim();
    if (description) payload.description = description;
    payload.priority = el("task-priority").value;

    authenticated("POST", "/tasks", payload).then(function (r) {
      if (r.status === 403) {
        setMessage(msg,
          "Your role cannot create tasks in this workspace.", false);
        return;
      }
      if (r.status !== 201 || !r.body) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      setMessage(msg, "Created \"" + r.body.title + "\".", true);
      el("task-create-form").reset();
      el("task-priority").value = "normal";
      loadTasks(false);
    }).catch(function () {
      setMessage(msg, "Could not reach the API. The task was not created.", false);
    });
  });

  el("task-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadTasks(false);
  });

  el("task-older-btn").addEventListener("click", function () {
    loadTasks(true);
  });

  /* ---------- knowledge ---------- */

  /* Cursor for "load more". Module state so that changing a filter and applying
   * it resets the walk instead of continuing it under different conditions. */
  var knCursor = "";
  /* The document currently shown in the detail panel, so Edit and Delete act on
   * a known id rather than on whatever row happens to be selected. */
  var knCurrent = null;

  function knListQuery(append) {
    var parts = ["limit=" + encodeURIComponent(el("kn-limit").value)];
    var kind = el("kn-filter-kind").value;
    if (kind) parts.push("kind=" + encodeURIComponent(kind));
    if (append && knCursor) parts.push("cursor=" + encodeURIComponent(knCursor));
    return "/knowledge?" + parts.join("&");
  }

  /* Rows are built with createElement and textContent, never innerHTML: a
   * document title and body are user-supplied text and must never be parsed as
   * markup. */
  function renderKnowledgeRows(docs, append) {
    var body = el("kn-body");
    if (!append) body.innerHTML = "";
    for (var i = 0; i < docs.length; i++) {
      (function (doc) {
        var tr = document.createElement("tr");

        var title = document.createElement("td");
        title.textContent = doc.title;
        tr.appendChild(title);

        var kind = document.createElement("td");
        kind.textContent = doc.kind;
        tr.appendChild(kind);

        var updated = document.createElement("td");
        updated.textContent = doc.updated_at
          ? String(doc.updated_at).replace("T", " ").slice(0, 19)
          : "\u2014";
        tr.appendChild(updated);

        var actions = document.createElement("td");
        actions.appendChild(knButton("View", function () { showKnowledge(doc.id); }));
        actions.appendChild(document.createTextNode(" "));
        actions.appendChild(knButton("Delete", function () { deleteKnowledge(doc); }));
        tr.appendChild(actions);

        body.appendChild(tr);
      })(docs[i]);
    }
  }

  function knButton(label, onClick) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = label;
    btn.addEventListener("click", onClick);
    return btn;
  }

  /* A listing is a bounded page with a cursor; a search is a ranked result set
   * with none. Both render into the same table, and the absence of next_cursor
   * from a search response is what hides "load more". */
  function applyKnowledgeResult(r, append) {
    var msg = el("kn-message");
    var table = el("kn-table");
    var empty = el("kn-empty");
    var older = el("kn-older-btn");

    if (r.status === 403) {
      table.hidden = true;
      empty.hidden = true;
      older.hidden = true;
      setMessage(msg,
        "Knowledge is workspace-scoped, and your current session is not attached " +
        "to a workspace, so the server refused this request. Sign in as a " +
        "workspace member or administrator to use it.", false);
      return;
    }
    if (r.status === 401) {
      /* The single refresh retry already happened inside authenticated();
       * reaching here means the session is genuinely over. */
      setMessage(msg, "Your session expired. Sign in again to see knowledge.", false);
      return;
    }
    if (r.status !== 200 || !r.body) {
      setMessage(msg, errorMessage(r) + " — adjust the filters and retry.", false);
      return;
    }
    renderKnowledgeRows(r.body.documents || [], !!append);
    var total = el("kn-body").children.length;
    table.hidden = total === 0;
    empty.hidden = total !== 0;
    knCursor = r.body.next_cursor || "";
    older.hidden = knCursor === "";
    setMessage(msg, "", true);
  }

  function loadKnowledge(append) {
    var loading = el("kn-loading");
    if (!append) { knCursor = ""; }
    loading.hidden = false;

    var term = el("kn-search").value.trim();
    var req;
    if (term) {
      /* Search carries the same visible filters as the listing, so the kind and
       * page-size controls are not silently ignored in search mode. Search is
       * capped server-side at MaxSearchResults (50), so the page size is
       * clamped here rather than sent as a value the server rejects. */
      var pageSize = parseInt(el("kn-limit").value, 10) || 50;
      var body = { query: term, limit: Math.min(pageSize, 50) };
      var kind = el("kn-filter-kind").value;
      if (kind) body.kind = kind;
      req = authenticated("POST", "/knowledge/search", body);
    } else {
      req = authenticated("GET", knListQuery(append));
    }

    return req.then(function (r) {
      loading.hidden = true;
      applyKnowledgeResult(r, append);
    }).catch(function () {
      loading.hidden = true;
      setMessage(el("kn-message"),
        "Could not reach the API. Check the connection and retry.", false);
    });
  }

  function hideKnowledgeDetail() {
    el("kn-detail").hidden = true;
    el("kn-edit-form").hidden = true;
    knCurrent = null;
  }

  function showKnowledge(id) {
    var msg = el("kn-message");
    setMessage(msg, "", false);
    return authenticated("GET", "/knowledge/" + encodeURIComponent(id)).then(function (r) {
      if (r.status === 404) {
        hideKnowledgeDetail();
        setMessage(msg, "That document no longer exists.", false);
        return null;
      }
      if (r.status !== 200 || !r.body) {
        setMessage(msg, "Could not load the document: " + errorMessage(r), false);
        return null;
      }
      knCurrent = r.body;
      el("kn-detail-title").textContent = r.body.title;
      el("kn-detail-kind").textContent = r.body.kind;
      el("kn-detail-updated").textContent = r.body.updated_at
        ? String(r.body.updated_at).replace("T", " ").slice(0, 19) : "\u2014";
      /* The content is the one field long enough to matter, and it is assigned
       * through textContent so any embedded markup stays inert. */
      el("kn-detail-content").textContent = r.body.content;
      el("kn-detail").hidden = false;
      el("kn-edit-form").hidden = true;
      return r.body;
    }).catch(function () {
      setMessage(msg, "Could not reach the API.", false);
      return null;
    });
  }

  function deleteKnowledge(doc) {
    var msg = el("kn-message");
    /* Deletion is the only way to retire a document -- knowledge has no archive
     * -- so it is always confirmed rather than only sometimes. */
    var sure = window.confirm(
      "Delete \u201c" + doc.title + "\u201d permanently? " +
      "Knowledge has no archive, so this cannot be undone from this page."
    );
    if (!sure) return;
    setMessage(msg, "", false);
    return authenticated("DELETE", "/knowledge/" + encodeURIComponent(doc.id))
      .then(function (r) {
        if (r.status !== 204) {
          setMessage(msg, "Delete refused: " + errorMessage(r), false);
          return null;
        }
        if (knCurrent && knCurrent.id === doc.id) hideKnowledgeDetail();
        setMessage(msg, "Deleted \u201c" + doc.title + "\u201d.", true);
        /* Read the list back so what is on screen is what the server holds. */
        return loadKnowledge(false);
      })
      .catch(function () {
        setMessage(msg, "Could not reach the API. Nothing was deleted.", false);
      });
  }

  el("kn-create-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var msg = el("kn-create-message");
    setMessage(msg, "", false);
    authenticated("POST", "/knowledge", {
      title: el("kn-title").value,
      kind: el("kn-kind").value,
      content: el("kn-content").value
    }).then(function (r) {
      if (r.status === 403) {
        setMessage(msg, "Your role cannot create knowledge in this workspace.", false);
        return;
      }
      if (r.status !== 201 || !r.body) {
        setMessage(msg, errorMessage(r), false);
        return;
      }
      setMessage(msg, "Created \u201c" + r.body.title + "\u201d.", true);
      el("kn-create-form").reset();
      el("kn-kind").value = "document";
      loadKnowledge(false);
    }).catch(function () {
      setMessage(msg, "Could not reach the API. The document was not created.", false);
    });
  });

  el("kn-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    hideKnowledgeDetail();
    loadKnowledge(false);
  });

  el("kn-clear-btn").addEventListener("click", function () {
    el("kn-search").value = "";
    el("kn-filter-kind").value = "";
    hideKnowledgeDetail();
    loadKnowledge(false);
  });

  el("kn-older-btn").addEventListener("click", function () {
    loadKnowledge(true);
  });

  el("kn-edit-btn").addEventListener("click", function () {
    if (!knCurrent) return;
    el("kn-edit-title").value = knCurrent.title;
    el("kn-edit-kind").value = knCurrent.kind;
    el("kn-edit-content").value = knCurrent.content;
    el("kn-edit-form").hidden = false;
  });

  el("kn-edit-cancel").addEventListener("click", function () {
    el("kn-edit-form").hidden = true;
  });

  el("kn-detail-close").addEventListener("click", hideKnowledgeDetail);

  el("kn-edit-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var msg = el("kn-message");
    if (!knCurrent) return;
    setMessage(msg, "", false);
    authenticated("PATCH", "/knowledge/" + encodeURIComponent(knCurrent.id), {
      title: el("kn-edit-title").value,
      kind: el("kn-edit-kind").value,
      content: el("kn-edit-content").value
    }).then(function (r) {
      if (r.status !== 200 || !r.body) {
        setMessage(msg, "Update refused: " + errorMessage(r), false);
        return;
      }
      setMessage(msg, "Saved \u201c" + r.body.title + "\u201d.", true);
      el("kn-edit-form").hidden = true;
      /* Re-read rather than patch locally: a content change is re-embedded on
       * the server, and the panel should show what is stored. */
      showKnowledge(r.body.id);
      loadKnowledge(false);
    }).catch(function () {
      setMessage(msg, "Could not reach the API. The document was not changed.", false);
    });
  });

  /* ---------- publishing approvals ---------- */

  var publicationKey = null;

  function publicationQuery() {
    var parts = ["limit=" + encodeURIComponent(el("publication-limit").value)];
    var status = el("publication-status").value;
    if (status) parts.push("status=" + encodeURIComponent(status));
    return "/publications?" + parts.join("&");
  }

  function publicationButton(label, fn) {
    var button = document.createElement("button");
    button.type = "button";
    button.textContent = label;
    button.addEventListener("click", fn);
    return button;
  }

  function publicationAction(pub, label, path) {
    var button;
    button = publicationButton(label, function () {
      var msg = el("publication-message");
      /* Disable while the request is in flight: a second click would fire a
       * second transition for the same publication, and the server would refuse
       * the now-illegal transition, overwriting the first action's result. */
      button.disabled = true;
      setMessage(msg, "", false);
      authenticated("POST", "/publications/" + encodeURIComponent(pub.id) + path)
        .then(function (r) {
          if (r.status !== 200) {
            setMessage(msg, "Action refused: " + errorMessage(r), false);
            return;
          }
          var status = r.body && r.body.status ? r.body.status : "updated";
          /* The refresh clears the message area, so the result of the action
           * is written after the list reload rather than being wiped by it. */
          return loadPublications(false).then(function () {
            setMessage(msg, "Publication is now " + status + ".", true);
          });
        }).catch(function () {
          setMessage(msg, "Could not reach the API. The publication was not changed.", false);
        }).then(function () {
          button.disabled = false;
        });
    });
    return button;
  }

  function renderPublicationRows(publications) {
    var body = el("publication-body-rows");
    body.innerHTML = "";
    for (var i = 0; i < publications.length; i++) {
      (function (pub) {
        var row = document.createElement("tr");
        var title = document.createElement("td");
        title.textContent = pub.title;
        row.appendChild(title);
        var platform = document.createElement("td");
        platform.textContent = pub.platform;
        row.appendChild(platform);
        var status = document.createElement("td");
        status.textContent = pub.status;
        if (pub.status === "failed") {
          status.title = pub.failure_reason || "delivery failed";
          status.className = "bad";
        }
        row.appendChild(status);
        var updated = document.createElement("td");
        updated.textContent = pub.updated_at ? String(pub.updated_at).replace("T", " ").slice(0, 19) : "—";
        row.appendChild(updated);
        var actions = document.createElement("td");
        if (pub.status === "queued") {
          actions.appendChild(publicationAction(pub, "Submit", "/submit"));
        } else if (pub.status === "review" && currentRole === "workspace_admin") {
          actions.appendChild(publicationAction(pub, "Approve", "/approve"));
          actions.appendChild(document.createTextNode(" "));
          actions.appendChild(publicationAction(pub, "Reject", "/reject"));
        } else if (pub.status === "approved" && currentRole === "workspace_admin") {
          actions.appendChild(publicationAction(pub, "Publish", "/publish"));
        } else if (pub.status === "failed") {
          if (currentRole === "workspace_admin") {
            actions.appendChild(publicationAction(pub, "Retry", "/retry"));
            actions.appendChild(document.createTextNode(" "));
          }
          var failed = document.createElement("span");
          failed.className = "muted";
          failed.textContent = pub.failure_reason || "delivery failed";
          actions.appendChild(failed);
        } else {
          actions.appendChild(document.createTextNode("—"));
        }
        row.appendChild(actions);
        body.appendChild(row);
      })(publications[i]);
    }
  }

  function loadPublications() {
    var loading = el("publication-loading");
    var table = el("publication-table");
    var empty = el("publication-empty");
    loading.hidden = false;
    return authenticated("GET", publicationQuery()).then(function (r) {
      loading.hidden = true;
      if (r.status === 403) {
        table.hidden = true;
        empty.hidden = true;
        setMessage(el("publication-message"), "Publishing requires a workspace identity.", false);
        return;
      }
      if (r.status !== 200 || !r.body) {
        table.hidden = true;
        setMessage(el("publication-message"), "Could not load publications: " + errorMessage(r), false);
        return;
      }
      var publications = r.body.publications || [];
      renderPublicationRows(publications);
      table.hidden = publications.length === 0;
      empty.hidden = publications.length !== 0;
      setMessage(el("publication-message"), "", true);
    }).catch(function () {
      loading.hidden = true;
      setMessage(el("publication-message"), "Could not reach the API.", false);
    });
  }

  el("publication-create-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var btn = ev.target.querySelector("button[type=submit]");
    var msg = el("publication-create-message");
    setMessage(msg, "", false);
    btn.disabled = true;
    var key = publicationKey || (window.crypto && window.crypto.randomUUID ? window.crypto.randomUUID() : String(Date.now()));
    publicationKey = key;
    authenticated("POST", "/publications", {
      title: el("publication-title").value,
      body: el("publication-body").value,
      platform: el("publication-platform").value
    }).then(function (r) {
      btn.disabled = false;
      if (r.status !== 201 || !r.body) {
        setMessage(msg, "Draft was not saved: " + errorMessage(r), false);
        return;
      }
      publicationKey = null;
      el("publication-create-form").reset();
      el("publication-platform").value = "stub";
      setMessage(msg, "Draft saved. Submit it for review when ready.", true);
      loadPublications(false);
    }).catch(function () {
      btn.disabled = false;
      setMessage(msg, "Could not reach the API. The draft was not confirmed.", false);
    });
  });

  el("publication-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadPublications(false);
  });

  /* ---------- creator pipelines ---------- */

  var pipelineKey = null;

  function pipelineAction(pipeline, label, path) {
    var button = document.createElement("button");
    button.type = "button";
    button.textContent = label;
    button.addEventListener("click", function () {
      var msg = el("pipeline-message");
      setMessage(msg, "", false);
      authenticated("POST", "/pipelines/" + encodeURIComponent(pipeline.id) + path)
        .then(function (r) {
          if (r.status !== 200) {
            setMessage(msg, "Action refused: " + errorMessage(r), false);
            return;
          }
          var status = r.body && r.body.status ? r.body.status : "updated";
          /* The refresh clears the message area, so the result of the action
           * is written after the list reload rather than being wiped by it. */
          loadPipelines(false).then(function () {
            setMessage(msg, "Pipeline is now " + status + ".", true);
          });
        }).catch(function () {
          setMessage(msg, "Could not reach the API. The pipeline was not changed.", false);
        });
    });
    return button;
  }

  function renderPipelineRows(pipelines) {
    var body = el("pipeline-body-rows");
    body.innerHTML = "";
    for (var i = 0; i < pipelines.length; i++) {
      (function (pipeline) {
        var row = document.createElement("tr");
        var stage = document.createElement("td"); stage.textContent = pipeline.stage; row.appendChild(stage);
        var status = document.createElement("td"); status.textContent = pipeline.status;
        if (pipeline.status === "failed") { status.className = "bad"; status.title = pipeline.failure_reason || "stage failed"; }
        row.appendChild(status);
        var artifacts = document.createElement("td");
        var refs = [];
        if (pipeline.research_reference) refs.push("research");
        if (pipeline.script_reference) refs.push("script");
        if (pipeline.review_reference) refs.push("review");
        if (pipeline.publication_id) refs.push("publication");
        artifacts.textContent = refs.length ? refs.join(", ") : "—";
        if (pipeline.failure_reason) artifacts.title = pipeline.failure_reason;
        row.appendChild(artifacts);
        var updated = document.createElement("td"); updated.textContent = pipeline.updated_at ? String(pipeline.updated_at).replace("T", " ").slice(0, 19) : "—"; row.appendChild(updated);
        var actions = document.createElement("td");
        if (pipeline.status === "awaiting_approval" && currentRole === "workspace_admin") {
          actions.appendChild(pipelineAction(pipeline, "Approve", "/approve"));
        } else if (pipeline.status === "failed") {
          actions.appendChild(pipelineAction(pipeline, "Retry", "/retry"));
        } else if (pipeline.status !== "done") {
          actions.appendChild(document.createTextNode("worker-owned"));
        } else {
          actions.appendChild(document.createTextNode("—"));
        }
        row.appendChild(actions); body.appendChild(row);
      })(pipelines[i]);
    }
  }

  function loadPipelines() {
    var loading = el("pipeline-loading");
    var table = el("pipeline-table");
    var empty = el("pipeline-empty");
    loading.hidden = false;
    var limit = el("pipeline-limit").value;
    return authenticated("GET", "/pipelines?limit=" + encodeURIComponent(limit)).then(function (r) {
      loading.hidden = true;
      if (r.status === 403) { table.hidden = true; empty.hidden = true; setMessage(el("pipeline-message"), "Pipelines require a workspace identity.", false); return; }
      if (r.status !== 200 || !r.body) { table.hidden = true; setMessage(el("pipeline-message"), "Could not load pipelines: " + errorMessage(r), false); return; }
      var pipelines = r.body.pipelines || [];
      renderPipelineRows(pipelines); table.hidden = pipelines.length === 0; empty.hidden = pipelines.length !== 0; setMessage(el("pipeline-message"), "", true);
    }).catch(function () { loading.hidden = true; setMessage(el("pipeline-message"), "Could not reach the API.", false); });
  }

  el("pipeline-create-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var btn = ev.target.querySelector("button[type=submit]"); var msg = el("pipeline-create-message");
    setMessage(msg, "", false); btn.disabled = true;
    pipelineKey = pipelineKey || (window.crypto && window.crypto.randomUUID ? window.crypto.randomUUID() : String(Date.now()));
    authenticated("POST", "/pipelines", {}).then(function (r) {
      btn.disabled = false;
      if (r.status !== 201 || !r.body) { setMessage(msg, "Pipeline was not started: " + errorMessage(r), false); return; }
      pipelineKey = null; setMessage(msg, "Pipeline started. Worker progress will appear from the server.", true); loadPipelines(false);
    }).catch(function () { btn.disabled = false; setMessage(msg, "Could not reach the API. Start was not confirmed.", false); });
  });
  el("pipeline-form").addEventListener("submit", function (ev) { ev.preventDefault(); loadPipelines(false); });

  /* ---------- memory ---------- */

  function memoryPath() {
    return "/memory/" + encodeURIComponent(el("memory-layer").value) +
      "/" + encodeURIComponent(el("memory-key").value);
  }

  function renderMemoryResult(body) {
    el("memory-result").hidden = false;
    el("memory-workspace").textContent = body.workspace_id || "—";
    el("memory-result-layer").textContent = body.layer || "—";
    el("memory-result-key").textContent = body.key || "—";
    el("memory-result-ttl").textContent = body.ttl_seconds === undefined
      ? "not returned by read" : String(body.ttl_seconds) + " seconds";
    if (body.value !== undefined) el("memory-value").value = body.value;
  }

  function readMemory() {
    var msg = el("memory-message");
    setMessage(msg, "", false);
    return authenticated("GET", memoryPath()).then(function (r) {
      if (r.status === 403) {
        setMessage(msg, "Your session is not authorized for workspace memory.", false);
        return;
      }
      if (r.status === 404) {
        el("memory-result").hidden = true;
        setMessage(msg, "No memory exists for this key in your workspace.", false);
        return;
      }
      if (r.status !== 200 || !r.body) {
        setMessage(msg, "Read refused: " + errorMessage(r), false);
        return;
      }
      renderMemoryResult(r.body);
      setMessage(msg, "Memory read.", true);
    }).catch(function () {
      setMessage(msg, "Could not reach the API. The memory was not changed.", false);
    });
  }

  el("memory-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var msg = el("memory-message");
    setMessage(msg, "", false);
    var ttl = Number(el("memory-ttl").value);
    var payload = { value: el("memory-value").value, ttl_seconds: ttl };
    authenticated("PUT", memoryPath(), payload).then(function (r) {
      if (r.status === 403) {
        setMessage(msg, "Your session is not authorized for workspace memory.", false);
        return;
      }
      if (r.status !== 200 || !r.body) {
        setMessage(msg, "Save refused: " + errorMessage(r), false);
        return;
      }
      renderMemoryResult(r.body);
      setMessage(msg, "Memory saved.", true);
    }).catch(function () {
      setMessage(msg, "Could not reach the API. The memory was not changed.", false);
    });
  });

  el("memory-read-btn").addEventListener("click", function () {
    readMemory();
  });

  if (token()) { enterApp(); } else { initAuthView(); }
})();
