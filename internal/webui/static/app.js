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

  /* An authenticated request with a single refresh retry. A stale access token
   * is rotated once; if rotation fails the session is over and the caller is
   * returned to the login view rather than shown partial data. */
  function authenticated(method, path, body) {
    return request(method, path, body, true).then(function (result) {
      if (result.status !== 401) return result;
      var refresh = sessionStorage.getItem(REFRESH);
      if (!refresh) { signOut(false); return result; }
      return request("POST", "/api/auth/refresh", { refresh_token: refresh }, false)
        .then(function (rotated) {
          if (rotated.status !== 200 || !rotated.body || !rotated.body.access_token) {
            signOut(false);
            return result;
          }
          setTokens(rotated.body.access_token, rotated.body.refresh_token);
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
    request("GET", "/health/live").then(function (r) {
      var ok = r.status === 200;
      setStatus("live", ok ? "ok" : "unavailable (" + r.status + ")", ok);
    }).catch(function () { setStatus("live", "unreachable", false); });
    request("GET", "/health/ready").then(function (r) {
      var ok = r.status === 200;
      setStatus("ready", ok ? "ready" : "not ready (" + r.status + ")", ok);
    }).catch(function () { setStatus("ready", "unreachable", false); });
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
    loadWorkspaces();
    loadAudit(false);
    /* Loaded on entry so a workspace member lands on their tasks rather than an
     * empty panel with a Load button. For a founder this returns 403, which the
     * card explains instead of showing a misleading empty state. */
    loadTasks(false);
    return authenticated("GET", "/api/me").then(function (r) {
      if (r.status !== 200 || !r.body) { signOut(false); return; }
      renderIdentity(r.body);
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

  if (token()) { enterApp(); } else { initAuthView(); }
})();
