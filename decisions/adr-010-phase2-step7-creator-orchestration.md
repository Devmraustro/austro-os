# ADR-010 — Phase 2 Step 7: Creator Workflow Orchestration

- **Status**: Accepted (implementation-time design, Phase 2 Step 7 — documented before migration per ADR-003 §3.1/§3.2 and ROADMAP §6/§7)
- **Date**: Phase 2 Step 7
- **Related**: ROADMAP.md §1 (Creator end-to-end workflow), §2.1 (module map: `internal/event`, `internal/worker` EXTENDED), §3 (constituents: knowledge, campaign rules, style guides, research, documentation), §5.4 (Creator workflow: research → script → review → publish), §6 (data model), §7 (API); CONSTITUTION.md P1 (Obedience), P6 (Replaceability), P8 (Minimal Disclosure), P9 (Security by Design), P11 (Human Oversight), P13 (Observability); ADR-009
- **Authorized finalization**: ROADMAP §2.1 "Creator workflow orchestration — event-bus/worker composition that runs the pipeline" and §5.4. This ADR records the orchestration design (pipeline state machine + event-driven worker composition) finalized at implementation.

## Context

Phase 2 Step 7 composes the built pipeline services into the **Creator workflow**:
research → script → review → publish. The workload is event-driven: pipeline
stages advance asynchronously across thread boundaries (RabbitMQ), each event is
traced end-to-end, and every transition is workspace-scoped, audited, and
guarded by the state machine. The pipeline reuses the existing Task, Knowledge,
and Publishing (Step 6) capabilities behind narrow, replaceable ports (P6), so
`internal/` never depends on `infrastructure/` (criterion #32). There is no
container-orchestration platform and no real external call in Phase 2 (ROADMAP
§1, §3).

## Decision — pipeline model

A `Pipeline` aggregates the Creator workflow for one workspace. It owns a stable
`Stage` (single stage of record) and a status, and references the produced
artefacts:
- `Stage`: `research`, `script`, `review`, `publish`. Terminal exit: `complete`.
- `PipelineStatus`: `created`, `active`, `awaiting_approval`, `done`, `failed`.
- `Stage → status` invariant: research/script/review land the pipeline in
  `active` (review leaves `awaiting_approval`); publish requires an approved
  publication (Human Oversight gate, ROADMAP §5.3) and lands it in `done`;
  the `failed` status is reserved for a stage handler error.

Transitions are explicit (`CanAdvance`), no stage is skipped, and a pipeline in
a terminal status has no further transitions (duplicate-transition protection).

## Decision — module structure (`internal/orchestration`)

`internal/orchestration` holds the pipeline state machine and the event-driven
worker handler. It has **no `infrastructure/` import** (criterion #32):
- `pipeline.go` — `Pipeline` model, `Stage`/`PipelineStatus`, `CanAdvance`,
  validation and sentinels.
- `store.go` — `PipelineStore` port (workspace-scoped persistence) plus narrow
  capability ports (`ScriptGen`, `Review`, `Publish`) mapping one-to-one to the
  relevant Step 3/4/6 services, and `AuditSink`/`EventSink`.
- `service.go` — `Service` (direct/unit path) that advances stages with
  workspace ownership enforcement, valid-transition checks, the human gate, and
  trace-aware audit/structured logs.
- `handler.go` — the worker **composition**: a `Handler` bound to a pipeline
  event that invokes `Service.Advance` and republishes the next stage event,
  giving a deterministic, message-driven loop compatible with
  `internal/worker` (RabbitMQ consumer) and `internal/event`
  (`UniversalEnvelope` with `TraceID`/`SpanID`).

Persistence is provided by `infrastructure/postgres` (concrete `PipelineStore`).
Stage work is delegated to narrow capability ports whose Phase 2 implementations
wrap the existing deterministic stub services (Step 3/4/6).

## Decision — schema (`pipelines`, additive, RLS)

```sql
CREATE TABLE IF NOT EXISTS pipelines (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    goal_id UUID,
    stage TEXT NOT NULL,
    status TEXT NOT NULL,
    task_id UUID,
    publication_id UUID,
    trace_id TEXT,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

ALTER TABLE pipelines ENABLE ROW LEVEL SECURITY;

CREATE POLICY workspace_isolation_policy ON pipelines
    USING (workspace_id = current_setting('app.current_workspace', true)::UUID);

CREATE INDEX IF NOT EXISTS idx_pipelines_workspace ON pipelines(workspace_id);
CREATE INDEX IF NOT EXISTS idx_pipelines_status ON pipelines(workspace_id, status);
```

- `trace_id` records the orchestration trace for end-to-end observability (P13).
- `publication_id` links the reviewed, approved publication produced at the
  publish stage (bitemporal link to Step 6).

## Decision — capability ports (interface segregation)

Instead of importing broad service/domain types, `internal/orchestration` uses
three narrow ports so each pipeline stage depends only on the call it needs:
- `Research(ctx, ws, pipeline)` → an artefact reference.
- `ScriptGen(ctx, ws, pipeline, researchRef)` → a task/script reference.
- `Review(ctx, ws, pipeline)` → approved/rejected artefact.
- `Publish(ctx, ws, pipeline, approvedArtefact)` → publication id.

Phase 2 wires deterministic stub adapters (no AI calls, no real platform).

## Decision — API contract

Per ADR-003 §3.2 the contract is finalized here: pipeline start and advance are
driven by **internal events** (worker composition), not new HTTP endpoints this
step. No OpenAPI operation is added, keeping the Phase 1 gate
(`require.Equal(t, 13, spec.totalOps)`) intact. The event types
(`pipeline.research`, `pipeline.script`, `pipeline.review`, `pipeline.publish`,
`pipeline.complete`) are added to `internal/event` under the existing
`UniversalEnvelope` envelope. HTTP wiring, if any, is deferred to a later step.

## Security implications

- Workspace isolation: RLS first; explicit `workspace_id` guards as
  defense-in-depth in the concrete store.
- Human Oversight: the pipeline cannot reach `publish`/`done` without an
  approved publication (reuses the Step 6 approval gate).
- No real external calls, no credentials, no cost-bearing operations
  (ROADMAP §5.8/§5.9); stub adapters only.
- Every stage transition is audited with actor/workload/trace/span and
  structured JSON logs (P13).
- Idempotency: the state machine rejects duplicate/invalid stage transitions.

## Consequences

- Adds an additive, workspace-scoped, RLS-protected `pipelines` table and a
  pure-domain orchestration package with no infrastructure dependency.
- Reuses Task/Knowledge/Publishing and the dependency-inversion port pattern.
- Keeps Phase 1 green: additive; criterion #16/#31 respected; no new OpenAPI
  operations; no infra import in `internal/`.

## Out of scope (later steps)

- Real provider adapters for research/script generation; content-editing
  autonomy (ROADMAP §1: the platform orchestrates, it does not edit).
- Campaign analytics/KPI and cross-workspace scheduling.
- HTTP wiring of a `/pipelines` surface.