# ADR-006 — Phase 2 Step 3: Task Management Data Model, Lifecycle and API Contract

- **Status**: Accepted (implementation-time design, Phase 2 Step 3 — documented before migration per ADR-003 §3.1/§3.2 and ROADMAP §6/§7)
- **Date**: Phase 2 Step 3
- **Related**: ROADMAP.md §5.6 (task ownership), §6 (data model impact), §7 (API impact); ADR-003 §1.1/§3.1/§3.2/§4; ADR-002 (autonomy bound); CONSTITUTION.md P9 (Privacy by Design), P10 (Security by Design), P11 (Human Oversight), P13 (Observability), P16 (Simplicity), P8 (Replaceability)
- **Authorized finalization**: ROADMAP §6 marks task table columns/FKs/policy names/indexes as an `[ASSUMPTION]` to be finalized during implementation and recorded in an ADR before migration. This ADR is that record for tasks.

## Context

Phase 2 Step 3 introduces Task Management, the first domain/application aggregate in the Creator
pipeline (goal → plan → task → research → script → review → publish). Tasks are the unit of work
assigned to AI employees under the ADR-002 autonomy bound: tasks are workspace-scoped, explicit in
their lifecycle, deny-by-default in authorization, audited, and traceable. No cross-workspace
access and no hidden global task access are permitted.

## Decision — tasks table (additive, workspace-scoped, RLS)

The following table is added additively to the startup schema (idempotent `CREATE TABLE IF NOT
EXISTS`, mirroring Phase 1 conventions). It is workspace-scoped with an FK to `workspaces`,
RLS-enabled with the established `workspace_isolation_policy` name, and indexed on `workspace_id`.

```sql
CREATE TABLE IF NOT EXISTS tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    goal_id UUID,
    parent_task_id UUID,
    title TEXT NOT NULL,
    description TEXT,
    status TEXT NOT NULL,
    priority TEXT NOT NULL DEFAULT 'normal',
    assignee_type TEXT,
    assignee_id UUID,
    deadline TIMESTAMP,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

ALTER TABLE tasks ENABLE ROW LEVEL SECURITY;

CREATE POLICY workspace_isolation_policy ON tasks
    USING (workspace_id = current_setting('app.current_workspace', true)::UUID);

CREATE INDEX IF NOT EXISTS idx_tasks_workspace ON tasks(workspace_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(workspace_id, status);
CREATE INDEX IF NOT EXISTS idx_tasks_assignee ON tasks(assignee_type, assignee_id);
```

Rationale and alternatives:
- `goal_id` / `parent_task_id` are loose UUIDs (no hard FK) so that the Creator-plan linkage and
  sub-task nesting remain flexible until Step 7 orchestration finalizes plans/goals. This mirrors
  Phase 1's loose `ai_employees.current_task_id`/`memory_id` pattern and avoids premature FK
  constraints that later steps would have to migrate.
- `status` and `priority` are text with application-level enum validation (consistent with Phase 1
  using text columns), keeping schema additive and avoiding Postgres enum migration churn.
- RLS: the single policy `workspace_isolation_policy` with the standard
  `app.current_workspace` setting matches every other workspace-scoped table.

## Decision — task lifecycle (explicit)

Valid `status` values and allowed transitions:

- `backlog` → `planned` → `in_progress` → `in_review` → `completed`
- `backlog|planned|in_progress|in_review` → `cancelled`
- `in_review` → `rejected` (returns to `backlog` or `planned`)
- any active state → `failed` (terminal error)

`completed`, `cancelled`, `failed`, `rejected` are terminal; no transition out of a terminal state
is permitted. Invalid transitions are rejected at the service boundary (defensive, deterministic).

## Decision — module structure (`internal/task`)

`internal/task` is a domain/application package with no `infrastructure/` import (criterion #32):
- `task.go` — `Task` model, `Status`/`Priority` types, lifecycle validation.
- `store.go` — `TaskStore` port (workspace-scoped persistence), and `AuditSink`/`EventBus` ports.
- `service.go` — `Service` enforcing workspace ownership, lifecycle transitions, and emitting audit
  events and structured logs with trace/span propagation.

Persistence is provided by `infrastructure/postgres` (concrete `TaskStore` implementation), and the
domain imports only the port. This preserves dependency inversion (P6) and replaceability (P8).

## Decision — API contract (`/tasks`, finalized before endpoints)

Per ADR-003 §3.2, the contract is finalized here before any endpoint is implemented:

- `POST /tasks` → `createTask` (bearerAuth; principles: Vision First, Security by Design).
- `GET /tasks` → `listTasks` (bearerAuth; principles: Security by Design, Observability).
- `GET /tasks/{id}` → `getTask` (bearerAuth; principles: Security by Design, Privacy by Design).
- `PATCH /tasks/{id}` → `updateTask` (bearerAuth; principles: Security by Design, Quality Over Speed).
- `POST /tasks/{id}/transition` → `transitionTask` (bearerAuth; principles: Human Oversight,
  Quality Over Speed).

Every endpoint uses bearerAuth, denies by default (no implicit allow), is workspace-isolated via
claims/RLS, and declares `principles:`. Handlers live in `main.go` (presentation layer) and call the
`task.Service`. Rules are registered explicitly in the deny-by-default authorizer.

This step implements the contract at the **service/store boundary** (domain service + concrete RLS
`TaskStore` + lifecycle/workspace/audit unit and integration tests), matching the Step 2 AI Gateway
precedent. HTTP routing for these operations is **deferred** to keep the Phase 1 gate
`assert`-exact OpenAPI operation count (`require.Equal(t, 13, spec.totalOps)` in
`tests/health_openapi_test.go`) untouched; the master prompt forbids modifying/skipping Phase 1 gate
tests. The contract above remains authoritative for the later HTTP wiring.

## Security implications

- Deny-by-default: no endpoint responds without an explicit authz rule + bearer claims.
- Workspace isolation: all reads/writes are constrained by RLS to the caller's workspace; the
  service also validates claims workspace against task workspace where it materializes records.
- No `SET row_security=off` anywhere.
- Audit: every create/transition emits an audit event (hash-chain compatible clock is the
  responsibility of the audit sink; the service records the decision).
- Structured JSON logs with trace/span/correlation; no plain-text logging.
- No secrets; no plain credentials; no external calls.

## Consequences

- Adds a workspace-scoped, RLS-protected `tasks` table (additive; Phase 1 tables untouched).
- Establishes the domain/service port pattern reused by knowledge, memory, and publish steps.
- Keeps Phase 1 green: migrations are additive, no Phase 1 table/code is modified, gate #16/#31 and
  dependency direction are respected (no forbidden tokens in this ADR/prose).

## Out of scope (later steps)

- Creator orchestration wiring of tasks into goal/plan (Step 7).
- Knowledge/memory/publish integration (Steps 4-6).
- AI worker execution of tasks.