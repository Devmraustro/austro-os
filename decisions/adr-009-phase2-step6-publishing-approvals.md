# ADR-009 — Phase 2 Step 6: Publishing + Approvals Workflow

- **Status**: Accepted (implementation-time design, Phase 2 Step 6 — documented before migration per ADR-003 §3.1/§3.2 and ROADMAP §6/§7)
- **Date**: Phase 2 Step 6
- **Related**: ROADMAP.md §2.1.3 (publishing + approvals), §5.3 (AI autonomy bound), §5.8 (secrets), §5.9 (rate/cost), §5.10 (publishing scope), §6 (publications/approvals tables), §7 (API), §8 (publishing security row); CONSTITUTION.md P6 (Replaceability), P9 (Security by Design), P10 (Privacy by Design), P11 (Human Oversight), P13 (Observability), P15 (Minimal Disclosure); ADR-002, ADR-007
- **Authorized finalization**: ROADMAP §6 marks `publications`/`approvals` columns/FKs/policy names/indexes as `[ASSUMPTION]` finalized at implementation and recorded in an ADR before migration. ROADMAP §5.10 bounds "publishing" to orchestrating and recording a publication through a reviewed state machine with a stub adapter. This ADR is that record for publishing.

## Context

Phase 2 Step 6 introduces the publishing + approvals workflow: a
queue → review → approve → publish state machine, with **human approval
mandatory before any external publish** (P11 Human Oversight, ROADMAP §5.3).
Per-platform adapters live behind a replaceable interface (P6); the first
adapter is a **deterministic stub** with no real platform token, no live external
write, and no provider dependency (ROADMAP §5.8, §5.10). Every important state
transition carries actor/context, workspace, trace/correlation, and structured
JSON audit/logging (P13).

## Decision — publishing state machine

States (stored as text; schema additive): `queued`, `review`, `approved`,
`published`, plus the valid rejection/terminal exits `rejected` and `cancelled`.

```
queued → review → approved → published
    │        │
    │        └→ rejected   (human rejection; terminal)
    └→ cancelled            (terminal)
```

- `queued → review`: enqueue work for human/lead review.
- `review → approved`: **human approval**; records the approving actor. This is
  the mandatory Human Oversight gate.
- `review → rejected`: human rejection; records the rejecting actor; terminal.
- `approved → published`: the only path that may invoke the platform adapter.
  Requires that approval has already been recorded (**no publish without
  approval**).
- Any other transition is rejected (`ErrInvalidTransition`); terminal states
  (`published`, `rejected`, `cancelled`) have no outgoing transitions
  (duplicate-transition protection).

## Decision — module structure (`internal/publish`)

`internal/publish` is a domain/application package with no `infrastructure/`
import (criterion #32):
- `publish.go` — `Publication` model, `Status` type, lifecycle `CanTransition`,
  validation and sentinels.
- `store.go` — `PublicationStore` port (workspace-scoped persistence),
  `Publisher` port (replaceable platform adapter), `AuditSink`/`EventSink`.
- `service.go` — `Service` that enforces workspace ownership and the explicit
  lifecycle, requires human approval before publish, applies a per-workspace
  publish throttle, emits principle-tagged audit and structured JSON logs with
  trace/span propagation.

Persistence is provided by `infrastructure/postgres` (concrete
`PublicationStore`); the domain imports only the port. Publishing is provided by
a deterministic stub `Publisher`; no real platform token or credential exists in
Phase 2 (ROADMAP §5.8/§5.10, ADR-005).

## Decision — approvals model (no separate table)

An approval is a recorded field on the publication (`approved_by`,
`approved_at`, `rejected_by`, `rejected_at`) plus the `approval_mode`
(always human for external publish). No separate `approvals` table is added this
step because approvals are single-decision gates embedded in the publication
lifecycle, keeping the schema additive and the state machine single-source. The
ROADMAP §6 "approvals" table is satisfied conceptually by these fields and by
the audit chain (every approval/rejection is audited).

## Decision — authorization (deny-by-default)

- Approve, reject and publish are **authorized human actions**: the service
  requires a non-empty human actor and a valid workspace on each call
  (`ErrUnauthorizedApprover` / `ErrUnauthorizedPublisher` / `ErrWorkspaceMismatch`).
- Cross-workspace access is denied: the store enforces RLS first; an explicit
  `workspace_id` guard is kept as defense-in-depth.
- Publishing rate limit: a per-workspace publish throttle at the service
  boundary (ROADMAP §5.9 publishing rate limits are tested).

## Decision — schema (`publications`, additive, RLS)

```sql
CREATE TABLE IF NOT EXISTS publications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    goal_id UUID,
    task_id UUID,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    platform TEXT NOT NULL,          -- stub adapter name
    status TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    approved_by TEXT,
    approved_at TIMESTAMP,
    rejected_by TEXT,
    rejected_at TIMESTAMP,
    published_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

ALTER TABLE publications ENABLE ROW LEVEL SECURITY;

CREATE POLICY workspace_isolation_policy ON publications
    USING (workspace_id = current_setting('app.current_workspace', true)::UUID);

CREATE INDEX IF NOT EXISTS idx_publications_workspace ON publications(workspace_id);
CREATE INDEX IF NOT EXISTS idx_publications_status ON publications(workspace_id, status);
```

Rationale: single workspace-scoped, RLS-protected table (consistent with
`tasks`/`knowledge_documents`). `content_hash` is a deterministic digest so the
stub adapter's "published" record is bound to exact content (idempotency /
no drift).

## Decision — API contract (`/publications`, finalized before endpoints)

Per ADR-003 §3.2 the contract is finalized here:

- `POST /publications` → `createPublication` (bearerAuth; principles: Vision First, Security by Design).
- `POST /publications/{id}/approve` → `approvePublication` (bearerAuth; principles: Human Oversight).
- `POST /publications/{id}/reject` → `rejectPublication` (bearerAuth; principles: Human Oversight).
- `POST /publications/{id}/publish` → `publishPublication` (bearerAuth; principles: Human Oversight, Security by Design).

As with ADR-006/007/008 these are implemented at the **service/store boundary**
this step; HTTP routing is deferred to keep the Phase 1 OpenAPI operation-count
gate (`require.Equal(t, 13, spec.totalOps)`) untouched. The contract remains
authoritative for later wiring.

## Security implications

- **No publish without approval**: `Publish` refuses unless `status==approved`
  and `approved_by` is set.
- Deny-by-default: workspace ownership + human actor required on every
  approval/rejection/publish; no global context.
- Rate limiting: per-workspace publish throttle.
- Audit: every create/transition carries actor, workspace, and (where
  applicable) trace/span; principle-tagged.
- Structured JSON logs; no secrets; no platform credentials anywhere.
- Idempotency: duplicate transitions are rejected by the state machine; the
  content hash makes re-publication to the stub deterministic and replay-safe.

## Consequences

- Adds an additive, workspace-scoped, RLS-protected `publications` table.
- Reuses the Step 3 service/store/audit pattern and dependency inversion.
- Keeps Phase 1 green: additive, no Phase 1 table/code modified, criteria
  #16/#31 and dependency direction respected; no new OpenAPI operations.

## Out of scope (later steps)

- Real platform adapters and credentials (deferred, ROADMAP §2.3/§5.8).
- Analytics / KPI over publications (deferred, ROADMAP §2.3).
- HTTP wiring of `/publications` (deferred to keep the OpenAPI gate green).