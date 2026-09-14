# ADR-022 — Phase 3 Web Application: First Real Vertical Slice

- **Status**: Accepted
- **Date**: Phase 3 — first implementation step (post ADR-016 scope decision, post
  ADR-021 runtime topology work)
- **Related**: ADR-016 (Phase 3 web application scope), ADR-011 (security
  hardening), ADR-020 (audit RLS policy), ADR-021 (database runtime topology and
  persistent audit), `PHASE3_WEB_APPLICATION_SCOPE.md`, CONSTITUTION.md P1
  (Founder Authority), P6 (Replaceability), P9 (Security by Design), P10
  (Privacy by Design), P11 (Human Oversight)

## Context

ADR-016 approved a nine-area browser application and explicitly deferred all
implementation. Nothing had been built since: the runtime had no frontend, no
login page and no authenticated browser route.

Two verified facts shaped this step.

1. **No frontend existed.** The Product Surface Audit finding behind ADR-016 was
   still literally true.
2. **The published API contract was not real.** `api/openapi.yaml` advertised 18
   operations while the binary registered 10. Eight documented endpoints —
   `/tasks`, `/pipelines`, `/knowledge`, `/memory`, `/audit`,
   `/api/publications`, `/api/publications/{id}/approve` and
   `/constitutional/principles` — had no handler at all. Any screen built
   against them would have rendered and then failed on first use.

ADR-016's own constraint applies directly: the frontend must consume the backend
through *explicit, authenticated HTTP APIs*, and must not duplicate business
logic. There was no honest way to satisfy nine areas when only three of them had
endpoints.

## Decision

Deliver a **real vertical slice** — a working, secure browser application over
the APIs that genuinely exist — rather than a broad mock of the approved
surface. Concretely:

- **Served by the existing Go binary.** Assets are embedded with `go:embed` in
  `internal/webui` and written by three handlers. There is no second backend, no
  Node process, no reverse proxy and no new deployment component.
- **No build step and no dependency.** One HTML document, one script, one
  stylesheet, all vanilla. `go build ./...` remains the only build. This keeps
  ADR-016's "smallest architecture compatible with the existing project" and
  CONSTITUTION P6 (Replaceability): the client can be swapped without touching
  the engine.
- **No new API surface.** The slice uses `POST /api/auth/{login,logout,refresh,
  bootstrap}`, `GET /api/me`, `GET /health/{live,ready}` and
  `GET|POST /workspaces`. Nothing was invented to make a screen look complete.
- **The gap is stated in the product, not hidden.** The signed-in view lists the
  approved-but-unimplemented areas by name. An operator sees what is missing
  instead of discovering it through a broken control.

### Delivered in this slice

| ADR-016 area | State | Notes |
| --- | --- | --- |
| 2.1 Authentication | **Delivered** | login, logout, refresh-on-401 with rotation, first-run Founder bootstrap |
| 2.2 Main Dashboard | **Partial** | identity, liveness/readiness, workspace context. Active pipelines, pending approvals and recent activity are **not** delivered — those endpoints do not exist |
| Workspaces | **Delivered** | list and create; Founder-only, enforced server-side |
| 2.3 Creator | Not delivered | no pipeline/stage/task/artifact endpoints |
| 2.4 AI Employees | Not delivered | no endpoint |
| 2.5 Knowledge | Not delivered | no endpoint |
| 2.6 Memory | Not delivered | no endpoint |
| 2.7 Tasks | Not delivered | no endpoint |
| 2.8 Publishing | Not delivered | no approval-queue endpoint; `/api/publications/{id}/approve` has a handler but is registered by no route |
| 2.9 Audit | Not delivered | no `GET /audit/events` route exists |

Each "not delivered" row requires its API first. Implementing the screen before
the endpoint is what produced the eight phantom operations, and this ADR exists
in part to not repeat that.

## Security consequences

The slice had to add public routes, which is the one place a UI can weaken a
deny-by-default system. It is constrained as follows:

- **Three new public routes, all static:** `GET /`, `GET /assets/app.js`,
  `GET /assets/styles.css`. No handler behind them reads a body, a query
  parameter, a path value or a cookie, so they cannot carry data or widen
  access. `tests/webui_live_test.go` asserts both halves: the assets are
  reachable, and every data route still returns 403 without credentials.
- **The exemption is derived, not duplicated.** `main.go` takes the public list
  from `webui.Assets()`, the same list it registers. A new asset cannot be
  exempted without being served, or served without being exempted.
- **Strict Content-Security-Policy.** `default-src 'none'` with `script-src`,
  `style-src`, `connect-src` and `img-src` limited to `'self'`, plus `base-uri
  'none'`, `form-action 'none'`, `frame-ancestors 'none'`. No `unsafe-inline`,
  no `unsafe-eval`, no wildcard. The document uses no inline script, no inline
  style and no inline event handlers, and a test fails if that changes — under
  this policy an inline handler is not a style problem, it is a control that
  silently stops working.
- **No secret material in the client.** The access and refresh tokens live in
  `sessionStorage`, scoped to the tab and gone when it closes. Nothing is
  written to `localStorage`, a cookie, the URL or the console. The document
  embeds no token.
- **Opening the page is not a side effect.** Bootstrap is unauthenticated *and*
  state-changing: on a fresh install it creates the Founder, on an initialized
  one it writes an `already_initialized` audit row, and either way it spends a
  rate-limit token. It is therefore never issued speculatively; it sits behind an
  explicit disclosure and is sent only from its button. A regression test pins
  this, because the first draft of this slice did exactly the wrong thing and
  probed bootstrap on page load.
- **The server still decides everything.** The client renders what the API
  returns and treats a 403 as a 403. Role checks in the UI are presentation, not
  enforcement.

## Alternatives considered

- **Build all nine areas against the documented spec.** Rejected: eight of those
  endpoints do not exist, so the result would be a UI that fails at runtime and
  a contract that stays a lie.
- **A separate frontend repository or SPA toolchain.** Rejected: adds a build
  step, a dependency tree and a second deployable for no current benefit,
  against ADR-016's architecture constraints.
- **Implement the missing endpoints in the same step.** Rejected: that is Phase
  3's remaining work, not a prerequisite for a first usable surface, and
  bundling it would make this change unreviewable.
- **Leave the UI out entirely and only fix the contract.** Rejected: the
  founder-approved scope calls for a usable product, and a login plus dashboard
  over real endpoints is genuinely usable today.

## Consequences

- The product has its first human-facing surface, and it is honest about its own
  boundaries.
- `api/openapi.yaml` and the registered route table are now held in parity by
  `tests/openapi_route_parity_test.go`, so a future area cannot be documented
  before it exists. The natural order is now: implement the endpoint, declare it
  in the spec, then build the screen.
- ADR-016's acceptance criteria are **partially** met: items 1, 2, 11, 13, 14,
  15 and 16 hold; items 4–10 remain open and each needs its API first.
- Audit visibility (2.9) is the highest-value remaining gap, because the audit
  chain is now durable (ADR-021) but readable only through SQL.

Sign-off: **Founder-approved scope, first implementation step.**
