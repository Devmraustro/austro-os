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

  if (token()) { enterApp(); } else { initAuthView(); }
})();
