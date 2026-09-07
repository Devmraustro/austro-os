# ADR-016 — Phase 3 Web Application (Founder-Approved Scope Decision)

- **Status**: Accepted
- **Date**: Phase 3 — initial human-facing product surface decision (post Phase 1/2 freeze, post Product Surface Audit)
- **Related**: ADR-001 (Phase 2 supersedes gate 31), ADR-002 (autonomy bound), ADR-003 (final approval and decision lock), ADR-004 (AI gateway), ADR-007 (RLS), ADR-009 (publishing approvals), ADR-010 (creator orchestration), ADR-011 (security hardening), ADR-013 (integration boundaries), ADR-014 (runtime reliability), ADR-015 (free-first completion); CONSTITUTION.md P1 (Founder Authority), P6 (Replaceability), P8 (AI Independence), P9 (Security by Design), P10 (Privacy by Design), P11 (Human Oversight); PRODUCT_SCOPE.md; ROADMAP.md; PHASE3_WEB_APPLICATION_SCOPE.md; Product Surface Audit (no frontend exists; browser UI scope previously UNSPECIFIED)

## Context

The Product Surface Audit established verified facts about the current runtime:

- No frontend, browser UI, login, registration/bootstrap, or authenticated HTTP
  application route exists.
- The runtime is backend/worker infrastructure plus a deterministic workflow
  engine (Creator orchestration has no HTTP entry point; it is driven by a seed
  mechanism and internal services).
- Phase 1 and Phase 2 are complete and frozen; the immutable Phase 1 gate must
  remain untouched; the absence of a UI is not a defect in the frozen scope.

The Founder has decided the product must eventually be a usable, secure,
browser-based product that he can open, authenticate into, and operate visually.
This ADR records that decision as **new Phase 3 scope** and fixes the minimum
coherent surface the Phase 3 web application must deliver, without any code
change at this stage.

## Decision

Founder approves **PHASE 3 — AUSTRO OS WEB APPLICATION**: a secure browser UI
for an authenticated Founder/operator that consumes the existing backend through
**explicit, authenticated HTTP APIs**. The backend remains the single source of
truth; the frontend must not duplicate business logic. Phase 3 is
**documentation/scope only in this step** — no frontend, routes, auth endpoints,
dependencies, or repositories are created yet.

### Scope

1. **Authentication** — login, secure session/token handling, logout,
   refresh/revocation, and initial Founder bootstrap.
2. **Main Dashboard** — system status, workspace context, active pipelines,
   pending approvals, recent activity.
3. **Creator** — create a content goal, view pipeline/stage/tasks/generated
   artifacts, review status, human approval, publish status.
4. **AI Employees** — list, role, status, assigned work, activity.
5. **Knowledge** — documents, search, workspace context.
6. **Memory** — workspace-scoped memory visibility where appropriate.
7. **Tasks** — task list, status, assignment, task details.
8. **Publishing** — approval queue, approval/rejection, delivery status,
   idempotency/retry visibility where safe.
9. **Audit** — recent audit events, trace/request identifiers where
   appropriate, read-only security/audit visibility.

### Non-scope (explicitly excluded from the initial Phase 3 web application)

Analytics platform, advanced KPI system, marketplace, plugin ecosystem,
enterprise multitenancy, paid-provider dependency, autonomous
self-replication, model training, and unnecessary distributed architecture.

## Security constraints

The Phase 3 UI must not weaken the existing security model. Preserve:

- deny-by-default authorization (no public application data routes; health
  endpoints may remain public)
- workspace isolation and PostgreSQL RLS
- audit integrity
- JWT security and refresh-token rotation
- human approval for real publishing
- secure secret handling and external URL restrictions
- existing AI usage limits

## Architecture constraints

- Prefer the smallest architecture compatible with the existing project.
- The Phase 3 UI consumes the backend through explicit authenticated HTTP APIs.
- Do NOT convert the modular monolith into microservices.
- Do NOT introduce unnecessary infrastructure or a second backend.
- Do NOT duplicate business logic in the frontend; the backend is the source of
  truth.

## AI and publishing constraints

- Phase 3 must work against `AUSTRO_AI_BACKEND=stub` with no OpenAI API key.
- Local AI (`local`) remains optional; external providers remain optional
  configuration.
- The UI must work against `AUSTRO_PUBLISH_BACKEND=stub`.
- Real publishing remains optional; human approval remains mandatory before
  real delivery.

## Acceptance criteria

The Phase 3 scope is accepted only when it clearly defines: authenticated
browser access; Founder bootstrap; dashboard; Creator UI; AI employee
visibility; knowledge UI; memory UI; task UI; publishing approval UI; audit
visibility; stub-mode operation; local-AI compatibility; security preservation;
backend-as-source-of-truth; no paid-service dependency; no microservice
expansion.

## Future extensions

Analytics, KPIs, marketplace/plugins, multitenancy, and paid-provider
integration remain future-version items (PRODUCT_SCOPE.md Long-Term Product
Direction; ROADMAP.md Phase 3+) and are outside the initial Phase 3 web
application scope.

## Consequences

- The browser UI becomes explicit, Founder-approved new scope (previously
  UNSPECIFIED).
- Any future implementation must be additive and preserve the deny-by-default
  enforcement point, the immutable Phase 1 gate, RLS, audit integrity, and the
  stub-mode offline guarantee.
- Sign-off: **Founder-approved.**