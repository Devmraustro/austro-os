# PHASE 3 — AUSTRO OS WEB APPLICATION — FOUNDER-APPROVED SCOPE

- **Owner**: Founder
- **Status**: Accepted scope decision. Implementation has **started** — see
  "Implementation status" below. This document still defines the full approved
  scope; it has not been narrowed.
- **Decision records**: `decisions/adr-016-phase3-web-application.md` (scope),
  `decisions/adr-022-phase3-web-application-vertical-slice.md` (first
  implementation step)
- **Goal**: Provide the first real human-facing product surface for the
  already-completed AUSTRO OS engine: an authenticated Founder/operator
  interacts with the existing platform through a secure browser UI.

This document defines what Phase 3 will build. It intentionally defines scope
and constraints only; no code is written, no routes or auth endpoints are
added, and no frontend repository or dependency set is created at this step.

---

## 1. Product principle

- **Backend as source of truth.** All business logic remains in the existing
  backend/worker engine. The web application is a client of explicit,
  authenticated HTTP APIs; it must not re-implement or override engine rules.
- **No weakened security.** The Phase 3 UI adds human-facing access without
  loosening any existing control.
- **Free/stub first.** The UI works end-to-end in offline `stub` mode, with no
  paid AI account and no publish token required.

## 2. Product surface (initial coherent minimum)

### 2.1 Authentication
- Login (Founder/operator credentials against existing identity/session model).
- Secure session/token handling (JWT access tokens; refresh-token rotation and
  revocation retained).
- Logout with session/refresh revocation.
- Refresh/revocation flow as defined by the existing JWT service.
- **Initial Founder bootstrap** (first-run creation of the Founder identity).
- Headers/claims: `Authorization: Bearer <JWT>`; workspace scoping via claims,
  unchanged.

### 2.2 Main Dashboard
- System status (health/ready, backend selection, worker state).
- Workspace context (current workspace and its scope).
- Active pipelines.
- Pending approvals.
- Recent activity.

### 2.3 Creator
- Create a content goal (enter the Creator pipeline).
- View pipeline and current stage.
- View tasks.
- View generated artifacts (per approved visibility).
- Review status.
- Human approval (initiate/confirm approval; decision recorded in the engine).
- Publish status.

### 2.4 AI Employees
- List AI employees; role; status; assigned work; activity.

### 2.5 Knowledge
- Documents; search; workspace context. (Workspace-isolated per existing RLS.)

### 2.6 Memory
- Workspace-scoped memory visibility where appropriate (no cross-workspace
  leakage; Redis keys remain workspace-partitioned).

### 2.7 Tasks
- Task list; status; assignment; task details.

### 2.8 Publishing
- Approval queue; approval/rejection; delivery status; idempotency/retry
  visibility where safe.
- Real delivery requires the existing human-approval gate; the UI only surfaces
  and records the operator's decision.

### 2.9 Audit
- Recent audit events; trace/request identifiers where appropriate;
  read-only security/audit visibility (view, not amend).

## 3. Security requirements (must preserve, never weaken)

- Deny-by-default authorization: **no public application data routes**; health
  endpoints may remain public; every other route requires authentication and
  authorization.
- Workspace isolation (claims-based scope) and PostgreSQL RLS untouched.
- Audit integrity: the UI is read-only over audit records; it cannot alter the
  hash chain or event history.
- JWT security and refresh-token rotation retained (HS256 issuer/audience,
  15-minute access tokens, 30-day rotating refresh tokens).
- Human approval remains mandatory before any real publishing delivery.
- Secure secret handling; external URL restrictions retained (AI/delivery URLs
  remain operator configuration; no request-driven URLs).
- Existing AI usage limits enforced by the gateway/service layers, surfaced
  read-only in the UI.

## 4. Architecture constraints

- Smallest architecture compatible with the existing project.
- The web application consumes the existing backend through explicit
  authenticated HTTP APIs (routes to be added in a later implementation step,
  each with an explicit allow rule — never a default-allow).
- **Do not** convert the modular monolith into microservices.
- **Do not** introduce unnecessary infrastructure or a second backend.
- **Do not** duplicate business logic in the frontend.

## 5. AI and publishing constraints

- Must work against `AUSTRO_AI_BACKEND=stub` and `AUSTRO_PUBLISH_BACKEND=stub`
  with no external credentials.
- Local AI (`AUSTRO_AI_BACKEND=local`) remains optional and key-optional.
- External providers remain optional, configuration-only activation.
- No OpenAI API key required to run the UI.

## 6. Out of scope (explicitly excluded from initial Phase 3)

- Analytics platform, advanced KPI system.
- Marketplace, plugin ecosystem.
- Enterprise multitenancy.
- Paid-provider dependency.
- Autonomous self-replication.
- Model training.
- Unnecessary distributed architecture.

## 7. Acceptance criteria (Phase 3 defined scope)

Phase 3 is accepted only when its scope clearly defines:

1. authenticated browser access
2. Founder bootstrap
3. dashboard
4. Creator UI
5. AI employee visibility
6. knowledge UI
7. memory UI
8. task UI
9. publishing approval UI
10. audit visibility
11. stub-mode operation
12. local-AI compatibility
13. security preservation
14. backend-as-source-of-truth
15. no paid service dependency
16. no microservice expansion

## 8. Implementation sequencing (future, outside this step)

A later, separately approved implementation step will, in order:

1. Add explicit authenticated HTTP API routes (each with an explicit allow
   rule, claims-injection middleware, and no change to the deny-by-default
   enforcement point).
2. Add the initial Founder bootstrap flow.
3. Choose the smallest UI stack compatible with the constraints (single-page
   client consuming the authenticated APIs).
4. Verify against stub backends, the full integration suite, and the immutable
   Phase 1 gate.

Sign-off: **Founder-approved.**

---

## 9. Implementation status

Recorded by `decisions/adr-022-phase3-web-application-vertical-slice.md` (first
step) and `decisions/adr-023-knowledge-http-surface.md`. The implementation
started as a **real vertical slice**, not a mock: a browser application embedded
in and served by the existing Go binary (`internal/webui`), with no second
backend, no build step and no new dependency. It runs entirely over endpoints
that exist.

The approved Phase 3 web application is now **complete across all ADR-016
areas**:

- **2.1 Authentication** — login, logout, refresh-on-401 with token rotation,
  and first-run Founder bootstrap. Bootstrap is behind an explicit control and
  is never issued on page load, because it is unauthenticated and
  state-changing.
- **2.2 Main Dashboard** — identity and role, liveness/readiness, workspace
  context, active pipelines, pending publication approvals awaiting review,
  and recent activity, composed only from authenticated endpoints.
- **2.3 Creator** — start a pipeline, view stage, status, generated-artifact
  references and related publication reference, with approve/retry actions
  offered per server state.
- **2.4 AI Employees** — list, role, status, team/department/workplace context,
  assigned task, capabilities; create/update via authorized endpoints.
- **2.5 Knowledge** — list, search, create, update, delete, all
  workspace-scoped; embedded-vector search via the AI gateway.
- **2.6 Memory** — read/write workspace-scoped memory keys (by layer), with TTL
  surfaced from the server response.
- **2.7 Tasks** — create, list (filters, cursor paging), and status transitions
  through the authorized transition endpoint (terminal moves are confirmed).
- **2.8 Publishing** — create publications, approval queue view, submit /
  approve / reject / publish / retry actions through the server-authoritative
  publication state machine.
- **2.9 Audit** — recent audit events with filters, and verification summary,
  read-only; the UI cannot amend the hash chain.

**Workspace administration** — list and create workspaces; Founder-only,
authorized server-side. Departments, teams and AI employees carry
workspace-scoped list, create, update and detail views with cursor paging and
filtering (their delete API routes exist but are intentionally not surfaced in
the UI; delete links are exposed only for knowledge documents, which lack a
softer retirement path).

**What this plan is** — every screen is a client of the authenticated HTTP API;
the backend remains the single source of truth and the deny-by-default
enforcement point is unchanged. Nothing in the UI widens authorization.

**Ordering of API-first delivery** is enforced rather than advisory:
`api/openapi.yaml` and the registered route table are held in parity by
`tests/openapi_route_parity_test.go`, so an endpoint can no longer be published
before it exists. The current surface is 54 operations/54 routes (48
bearer-protected; the six public-by-contract operations are
`GET /health/live`, `GET /health/ready`, and the auth endpoints
`bootstrap`, `login`, `refresh`, `logout`), verified by the parity test and by
the independent parity check recorded in
`docs/FINAL_PROJECT_CERTIFICATION.md`.

**Security position** — unchanged and now structurally pinned. Three public
routes serve fixed assets only (`/`, `/assets/app.js`, `/assets/styles.css`);
every data route still returns 403 without credentials. The document is served
with `default-src 'none'` and `'self'`-only script, style and connect sources,
uses no inline code, and holds tokens in `sessionStorage` only.
`tests/webui_live_test.go` and `internal/webui/webui_test.go` assert all of it,
and browser E2E (real Chromium) exercises the surface end-to-end in CI.

All 16 acceptance criteria (§7) are met; the five status categories
(static / CI / not-run-locally / operator-configuration / deferred) are defined
with evidence in `docs/FINAL_PROJECT_CERTIFICATION.md`. Phase 3+ (Analytics,
Integrations, Marketplace) remains deferred (ROADMAP Phase Overview; ADR-016
Non-scope).
