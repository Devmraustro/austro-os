# ROADMAP

> Phase planning and roadmap for AUSTRO OS.
>
> Status: Active
> Version: 1.0

---

# Phase Overview

| Phase | Name | Status |
|-------|------|--------|
| 1 | Verified Foundation | **CLOSED** — baseline commit `8c42c4e780d257bf9685d5a15973d481a97a5ef5` |
| 2 | Autonomous Content Production | **PROPOSED** — this document is the frozen specification |
| 3+ | Analytics, Integrations, Marketplace | Deferred (see PRODUCT_SCOPE.md "Long-Term Product Direction") |

---

# Phase 2 — Autonomous Content Production

## 1. Objective (FROZEN)

Phase 1 was verified as a closed foundation that is explicitly *forbidden* from
implementing business features (Phase 1 gate criterion #14) and explicitly
*defers* content automation and social publishing (Phase 1 gate criterion #31).

Phase 2 introduces the first real business capability on top of that closed
foundation: an **end-to-end Creator content-production workflow** — plan, task,
research, script, review, and publish — executed by AI employees operating as a
structured Digital AI Company under human oversight.

Phase 2 must introduce exactly ONE production pipeline (the **Creator
workflow**) to bound scope and to make exit criterion #1 objectively testable.

## 2. Scope

### 2.1 MUST implement (FROZEN)

1. **AI provider gateway** (`internal/ai`) — a replaceable, provider-agnostic
   interface (Constitution P6 Replaceability, P8 AI Independence). No single
   provider is hard-bound. Initial adapter is a **fake/stub** used for tests and
   runtime wiring; real provider selection is a decision deferred to
   integration (see 5.7).
2. **Task management** (`internal/task`) — goal creation, project planning, task
   generation, dependencies, prioritization, scheduling, progress
   (PRODUCT_SCOPE.md §Task Management).
3. **Knowledge management** (`internal/knowledge`) — company knowledge, workspace
   knowledge, campaign rules, style guides, research, documentation
   (PRODUCT_SCOPE.md §Knowledge Management). Stored with PGVector embeddings and
   isolated by RLS per workspace (P10 Privacy by Design).
4. **Memory system** — complete the existing 4-layer `internal/memory`
   (Session/LongTerm/Workspace/Organizational) with the persistence layer
   already scaffolded (Redis workspace-partitioned keys + repository
   abstraction). Wire it into task and pipeline state.
5. **Publishing + approvals** (`internal/publish`) — a
   queue → review → approve → publish state machine. **Human approval is
   mandatory before any external publish** (P11 Human Oversight). Per-platform
   adapters live behind a replaceable interface (P6). First platform adapter is
   a stub; no real platform token is integrated in Phase 2.
6. **Creator workflow orchestration** — event-bus/worker composition that runs
   goal → plan → task → research → script → review → publish end-to-end, with
   full trace/span/correlation observability (P13 Observability).

### 2.2 MUST NOT implement (FROZEN)

- **No breaking the modular monolith.** No container-orchestration platform, no
  microservices, no Kafka, no dedicated distributed-search engine (Phase 1 gate
  criterion #16 keeps each of these out of scope). Dependency direction is
  frozen: Presentation → Application → Domain → Intelligence → Shared Services
  → Infrastructure (`.github/workflows/phase1-exit-criteria.yml`).
- **No self-replicating or unscoped autonomous agents.** Agents execute assigned
  tasks only and cannot create other agents (see ADR-002).
- **No building a foundation language model** (PRODUCT_SCOPE.md: AI Model
  Development is out of scope). The platform consumes models via the gateway.
- **No replacing video-editing software or a social platform**
  (PRODUCT_SCOPE.md Out of Scope). The platform orchestrates; it does not edit
  video in-app or host content.
- **No general-purpose chatbot** (PRODUCT_SCOPE.md Out of Scope). AUSTRO OS
  manages work, not conversations.
- **No marketplace, plugin ecosystem, multi-tenant organizations, enterprise
  edition, self-improving workforce, or autonomous business optimization** —
  these are future-version items (PRODUCT_SCOPE.md Long-Term Product Direction)
  and are explicitly deferred past Phase 2.
- **No regressing Phase 1.** All 33 Phase 1 exit criteria and all 61 Phase 1
  tests must remain green.

### 2.3 Deferred to later phases (FROZEN)

- Real external AI providers (model selection: capability/cost/latency/quality).
- Real publishing platform adapters (YouTube/TikTok/Instagram/Telegram) with
  live credentials.
- Analytics / KPI tracking / campaign analysis / continuous improvement
  (PRODUCT_SCOPE.md §Analytics).
- Non-Creator workspaces (Clipping, Marketing, Research, Learning).
- Marketplace / plugin ecosystem / multi-tenancy / enterprise edition.

## 3. Dependencies on Phase 1 (FROZEN)

| Phase 1 component | Phase 2 use |
|---|---|
| `internal/auth` (JWT + refresh rotation) | Authenticate all Phase 2 callers/agents/approvers |
| `internal/authz` (deny-by-default) | Every new Phase 2 route/action must be granted an explicit allow rule |
| `internal/config` (fail-fast `LoadStrict`) | All new Phase 2 settings validated strictly; no insecure fallback |
| `internal/organization` | Tasks/knowledge/memory/publishing attach to Workspace→...→AI Employee owners |
| `internal/audit` (hash chain + signatures) | Every Phase 2 action (task, AI decision, publish) audited with a principle |
| `internal/event` + `internal/worker` + RabbitMQ | Orchestrate pipeline steps as async, traced events |
| `infrastructure/database` + Postgres + RLS | New tables (tasks, knowledge, publications) RLS-isolated per workspace; no `SET row_security=off` (ADR-007) |
| `infrastructure/redis` | Workspace-partitioned memory/session keys |
| `internal/log` (structured JSON) | All new code; no plain `log.*`/`fmt.Fprintf` (ADR-007) |
| `internal/principlemapping` (18) | Every new event/endpoint carries a constitutional principle |
| `internal/repository` + `internal/middleware` | Port abstractions for data access + correlation/header propagation |
| `api/openapi.yaml` | Every new endpoint documented with `principles:` + `bearerAuth` |
| `internal/memory` | Completes the existing 4-layer memory system |

## 4. Architecture impact (IDENTIFY — implementation in a later phase)

Dependency direction is frozen (CI). New packages must obey it and never import
`infrastructure/`.

Proposed new/extended modules:
- `internal/ai` (NEW) — replaceable provider gateway (port), stub adapter, package interface.
- `internal/task` (NEW) — goal/plan/task domain + repository port.
- `internal/knowledge` (NEW) — document/rule/style-guide domain + embedding retrieval.
- `internal/publish` (NEW) — publication + approval domain; per-platform adapters.
- `internal/memory` (EXTEND) — wire existing layers into task/pipeline state.
- `internal/event` (EXTEND) — new pipeline event types.
- `internal/worker` (EXTEND) — new consumers for pipeline steps.
- `internal/audit` (EXTEND) — new event types (task, ai_decision, publish).
- `internal/organization` (EXTEND) — task/knowledge/publication ownership linkage.
- `api/` (EXTEND) — new route groups.
- `infrastructure/*` (EXTEND) — additive schema + repos.

## 5. Decisions and assumptions (EXPLICIT — P17)

Decisions marked **[DECIDED]** were confirmed by the Founder in the Phase 2
kickoff. Items marked **[ASSUMPTION]** are documented assumptions adopted for
this frozen spec; each must be re-confirmed at integration/implementation time
and never treated as silently required.

### 5.1 Scope basis — [DECIDED]
Freeze a spec first (this document). Implementation begins only after Founder
approval (P2 Architecture Before Implementation).

### 5.2 Gate #31 supersession — [DECIDED]
Phase 2 legitimately introduces content automation and social-publishing
orchestration. See ADR-001. The Phase 1 gate criterion #31 remains in force for
Phase 1 code; Phase 2 packages opt into the supersession explicitly.

### 5.3 AI autonomy bound — [DECIDED]
Human-oversight, non-replicating bound:
- Agents execute assigned tasks within their workspace only.
- Agents cannot create other agents (no self-replication; gate #31 preserved).
- Any external publish and any critical/financial/legal action requires explicit
  human approval (P11).
See ADR-002.

### 5.4 First pipeline — [DECIDED]
Creator workflow (research → script → review → publish), matching the Creator
Workspace in PROJECT.md.

### 5.5 Remaining UNSPECIFIED items — [DECIDED] continue with documented assumptions

### 5.6 Task/knowledge data ownership — [ASSUMPTION]
- Tasks own themselves; knowledge is shared per workspace; memory is the
  ephemeral/derived state. Knowledge and memory remain separate stores to avoid
  overlap (PRODUCT_SCOPE.md lists them as distinct capabilities).

### 5.7 AI provider abstraction — [ASSUMPTION]
- The gateway exposes capabilities (complete, embed, classify). Initial adapter
  is a deterministic stub; real provider selection (OpenAI/Anthropic/other) is
  deferred to integration and chosen by capability/cost/latency/quality (P8).
- **No provider API key is integrated in Phase 2**; no new secret is stored.

### 5.8 Secrets backend — [ASSUMPTION]
- Publishing platform tokens and real provider keys are **out of Phase 2 scope**
  because no real adapter/token is integrated. When introduced, they must go
  through fail-fast config (`LoadStrict`) or a secrets manager, never env
  fallback, and never be logged (P9/P10; matches Phase 1 criterion #23).

### 5.9 Rate/cost controls — [ASSUMPTION]
- Phase 2 stubs perform no paid external calls, so cost caps are not exercised.
- Publishing rate limits apply to the approval/publish state machine and are
  tested. Real per-provider cost limits are deferred with provider selection.

### 5.10 Publishing scope vs. phase parity — [ASSUMPTION]
- "Publishing" in Phase 2 means orchestrating and recording a publication
  through a reviewed state machine with a stub adapter — not connecting to a real
  social platform (that is deferred). This bounds scope while still making the
  publish gate (+human approval) real and testable.

## 6. Data model impact (IDENTIFY — implementation in a later phase)

Proposed additive tables (each `workspace_id`-scoped, RLS-policy'd, no
`SET row_security=off`):
- Tasks/Goals/Projects: `goals`, `plans`, `tasks` (+ status, priority, deps, scheduling).
- Knowledge: `knowledge_documents` (PGVector), `campaign_rules`, `style_guides`.
- Publishing: `publications`, `approvals`.
- Memory: existing table/Redis stores completed for the 4 layers.
- AI ledger: `ai_usage_log` (model, cost, latency, principle) for observability.
- Exact columns/FKs/policy names/indexes: **[ASSUMPTION]** — finalized during
  implementation and recorded in an ADR before migration.

## 7. API impact (IDENTIFY — implementation in a later phase)

New endpoint groups (each with `principles:` + `bearerAuth`, per Phase 1
criterion #12): `/tasks/*`, `/goals`, `/plans`, `/knowledge/*`,
`/publications/*`, `/approvals/*`, and an AI gateway proxy surface.
Exact paths/methods/schemas/operationIds: **[ASSUMPTION]** — finalized during
implementation.

## 8. Security impact (by capability)

| Capability | Auth | Authz | Isolation | Audit | Validation | Abuse controls | Secrets | Principles |
|---|---|---|---|---|---|---|---|---|
| Task mgmt | JWT | explicit rule/action | RLS per ws | task events | schema/state; cross-ws FK guard | task-create throttle | none new | 5,7,9,10,13 |
| Knowledge | JWT | read/write rules | RLS per ws; org vs ws scoping | write events | entity validation; content/XSS | upload size/embed limit | none | 5,6,7,9,10,13 |
| Memory | JWT | memory rules | ws-partitioned keys | audited | bounds; no secret in memory | memory-write throttle | none | 9,10,13,15 |
| Publishing | JWT + **human approval** | per-platform rules; **Human Oversight** gate | RLS per ws; stub adapter | full publish chain | content + campaign-rule compliance | publish-rate; delete-account abuse | **none in Phase 2** (stub) | 6,9,10,11,13 |
| AI gateway | JWT callers | deny-by-default; model scope | never leak cross-ws data to prompts | AI calls + cost/model/principle | prompt/context sanitization; no PII leak | **rate-limit + cost caps** | **none in Phase 2** | 6,7,8,9,10,13 |

Cross-cutting: secrets never logged; fail-fast config; trace/span/correlation
across async steps (P13).

## 9. Testing strategy

- **Unit**: task/knowledge/memory/publishing domain logic; authz rules per
  endpoint; config validation; AI-gateway stub replacement (Replaceability).
- **Integration (live stack)**: task workflows through Postgres/Redis/RabbitMQ;
  knowledge search over PGVector; publishing state machine + approval/rejection.
- **Runtime**: new endpoints reachable on the compose network (mirror
  `TestHealthChecks`).
- **Security**: deny-by-default on every new route; **RLS tests** proving a
  workspace-B principal cannot read/write/delete workspace-A tasks, knowledge,
  or memory; secrets never logged; rate-limit behavior.
- **Failure paths**: provider stub timeouts surface as controlled error + audit;
  approval-rejected publish path; task-dependency deadlock prevention;
  memory-store outage.
- **Regression**: all Phase 1 criteria (#01–#33) + all 61 tests stay green.
- **Phase 2 CI**: a dedicated `phase2` workflow; the Phase 1
  `phase1-exit-criteria.yml` gate is never replaced.

## 10. Migration strategy

1. Keep Phase 1 green at every step; Phase 2 is an additive layer.
2. Additive schema only (no alteration/removal of Phase 1 tables/policies).
3. New authz rules are explicit add-ons; deny-by-default keeps unconfigured
   Phase 2 routes denied by default.
4. Phase 1 endpoint behavior is unchanged (P14 Backward Compatibility).
5. Provider abstraction (gateway) introduced first, Phase 1 code untouched.
6. Separate `phase2` CI workflow; never replace the Phase 1 gate.

## 11. Implementation order proposal

0. [DECISION GATE — DONE this session] Founder resolves the UNSPECIFIED items.
1. [DONE this session] Phase 2 spec (this document) + ADR-001, ADR-002.
2. AI gateway abstraction (`internal/ai`) + stub + unit tests.
3. Task domain (`internal/task`) + schema + repos + RLS + authz + API + tests.
4. Knowledge domain (`internal/knowledge`) + PGVector + RLS + API + tests.
5. Memory completion (`internal/memory`) + wiring + tests.
6. Publishing + approvals (`internal/publish`) + stub adapter + human gate + RLS + API + tests.
7. Creator workflow orchestration (event bus/worker) + observability.
8. Security hardening + abuse controls (rate limits, cost caps).
9. Phase 2 CI workflow + full Phase 1 regression.
10. Phase 2 exit-criteria gate — pass, then close.

## 12. Phase 2 exit criteria (objective, testable — FROZEN SUBJECT TO APPROVAL)

1. **Pipeline capability**: the Creator workflow executes end-to-end
   (goal → plan → task → research → script → review → publish) through the real
   stack, producing an audited artifact.
2. **API**: every new endpoint documented in `api/openapi.yaml` with `principles:`
   + `bearerAuth`; deny-by-default enforced; `go build`/`go vet`/`go test`
   EXIT=0.
3. **Authorization**: a test proves a workspace-B principal cannot
   read/write/delete workspace-A tasks, knowledge, or memory (RLS + authz).
4. **Human Oversight**: publishing and critical actions require an explicit
   approval step; a rejection path is tested (P11).
5. **Replaceability**: AI provider is behind an interface; switching the stub for
   a separate fake proves drop-in replacement in a unit test (P6/P8).
6. **Observability**: all Phase 2 async actions carry trace_id/span_id/correlation
   and structured JSON logs; audit hash chain includes Phase 2 events.
7. **Security**: no secrets in logs; `SET row_security=off` prohibited across new
   code; `CREATE POLICY` present for every new table (ADR-007).
8. **Migration safety**: 100% Phase 1 regression — all 33 Phase 1 criteria and all
   61 Phase 1 tests still PASS; monolith preserved (no orchestration
   platform/microservices/Kafka/dedicated search engine).
9. **Documentation**: ROADMAP (this doc) and architecture docs populated; each exit
   criterion traceable to a doc.
10. **No premature later-phase features**: marketplace, plugin ecosystem,
    multi-tenancy, enterprise edition, self-improving workforce, autonomous
    business optimization demonstrably absent.
11. **Deferred-bound respect**: no real external provider/token/writing platform
    integrated; stubs only.

**Final status line**: `PHASE 2 STATUS: READY FOR PHASE 3` once all selected
criteria pass; otherwise `NOT READY`.

---

# Governance

- Changes to this roadmap require architectural review and Founder approval
  (PRODUCT_SCOPE.md Scope Governance; P17 Explicit Decisions).
- Phase 1 is closed and immutable at this baseline; Phase 2 is additive.