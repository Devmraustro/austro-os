# ADR-007 — Phase 2 Step 4: Knowledge Management Data Model, Embedding and Retrieval

- **Status**: Accepted (implementation-time design, Phase 2 Step 4 — documented before migration per ADR-003 §3.1/§3.2 and ROADMAP §6/§7)
- **Date**: Phase 2 Step 4
- **Related**: ROADMAP.md §2.1.3 (knowledge management), §5.6 (ownership), §5.7 (AI embedding), §6 (knowledge tables), §7 (API), §8 (security); ADR-003; ADR-002 (autonomy bound); CONSTITUTION.md P6 (Replaceability), P7 (Separation of Concerns), P9 (Security by Design), P10 (Privacy by Design), P13 (Observability)
- **Authorized finalization**: ROADMAP §6 marks `knowledge_documents` columns/FKs/policy names/indexes as an `[ASSUMPTION]` to be finalized during implementation and recorded in an ADR before migration. This ADR is that record for knowledge.

## Context

Phase 2 Step 4 introduces Knowledge Management: workspace-scoped company knowledge, campaign rules,
style guides, research, and documentation (ROADMAP §2.1.3). Documents are stored with PGVector
embeddings and isolated by RLS per workspace (P10). Embedding is produced by the provider-agnostic
AI gateway introduced in Step 2 (ADR-004, `internal/ai` `Provider.Embed`). Knowledge is shared per
workspace while memory remains the ephemeral/derived state (ROADMAP §5.6), so knowledge and memory
stay in separate stores.

## Decision — knowledge_documents table (additive, workspace-scoped, RLS, PGVector)

The following table is added additively to the startup schema (idempotent `CREATE TABLE IF NOT
EXISTS`, mirroring `tasks` in ADR-006). It is workspace-scoped with an FK to `workspaces`,
RLS-enabled with the established `workspace_isolation_policy` name, and indexed on `workspace_id`.

```sql
CREATE TABLE IF NOT EXISTS knowledge_documents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,               -- document | campaign_rule | style_guide
    title TEXT NOT NULL,
    content TEXT NOT NULL,
    embedding vector(10),
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

ALTER TABLE knowledge_documents ENABLE ROW LEVEL SECURITY;

CREATE POLICY workspace_isolation_policy ON knowledge_documents
    USING (workspace_id = current_setting('app.current_workspace', true)::UUID);

CREATE INDEX IF NOT EXISTS idx_knowledge_workspace ON knowledge_documents(workspace_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_kind ON knowledge_documents(workspace_id, kind);
```

Rationale:
- `kind` is text with application-level enum validation (consistent with `tasks.status` in ADR-006)
  to keep schema additive and avoid Postgres enum migration churn. `document`, `campaign_rule`,
  `style_guide` map to the ROADMAP §6 knowledge table split while keeping a single RLS-protected
  table with one policy.
- `embedding vector(10)` matches the existing Phase 1 `memory_embeddings` dimension (10) so a single
  real provider stub and the PGVector similarity operator (`<=>`) are reused without a dimension
  change. Embeddings are produced by the AI gateway stub in Step 2.
- RLS: the single `workspace_isolation_policy` with the standard `app.current_workspace` setting
  matches every other workspace-scoped table.

## Decision — module structure (`internal/knowledge`)

`internal/knowledge` is a domain/application package with no `infrastructure/` import (criterion #32):
- `knowledge.go` — `Document` model, `Kind` type, lifecycle/validation and sentinels.
- `store.go` — `DocumentStore` port (workspace-scoped persistence), `Embedder` port (bound to the
  AI gateway `Provider.Embed`), and `AuditSink`/`EventSink` ports.
- `service.go` — `Service` that embeds on create, enforces workspace ownership, emits audit events
  and structured logs with trace/span propagation.

Persistence is provided by `infrastructure/postgres` (concrete `KnowledgeStore`), and the domain
imports only the port. Embedding is provided by the Step 2 AI gateway (`internal/ai.Gateway.Embed`)
via an adapter that guards workspace scope; no real provider or key is used (ADR-004, ROADMAP §5.7).

## Decision — search/retrieval contract

- `Search(ctx, workspaceID, queryEmbedding, kind, limit)` returns the top-k documents by cosine
  similarity within the caller's workspace only. RLS + an explicit `workspace_id` filter guard the
  query (defense-in-depth); no cross-workspace token/prompt content ever enters a prompt (ROADMAP
  §8 knowledge row: clean entity validation, content/XSS, upload size/embed limit).

## Decision — API contract (`/knowledge`, finalized before endpoints)

Per ADR-003 §3.2 the contract is finalized here:

- `POST /knowledge` → `createKnowledgeDocument` (bearerAuth; principles: Vision First, Security by Design).
- `POST /knowledge/search` → `searchKnowledge` (bearerAuth; principles: Security by Design, Privacy by Design).

As with ADR-006, these are implemented at the **service/store boundary** this step; HTTP routing is
deferred to keep the Phase 1 OpenAPI operation-count gate (`require.Equal(t, 13, spec.totalOps)`)
untouched. The contract remains authoritative for later wiring.

## Security implications

- Deny-by-default: create/search enforce workspace ownership; RLS is the primary isolation, explicit
  `workspace_id` filters provide defense-in-depth.
- No `SET row_security=off` anywhere (ADR-007 gate).
- Audit: every create/search emits a principle-tagged audit decision.
- Structured JSON logs with trace/span/correlation; no plain-text logging; no secrets.
- Embedding content is validated for size/empty before the (stub) provider is invoked.

## Consequences

- Adds a workspace-scoped, RLS-protected `knowledge_documents` table (additive; Phase 1 tables
  untouched).
- Reuses the Step 2 AI embedding port and the Phase 1 PGVector operator.
- Keeps Phase 1 green: additive migrations, no Phase 1 table/code modified, gate #16/#31 and
  dependency direction respected.

## Out of scope (later steps)

- Producer/consumer wiring of knowledge into the Creator pipeline (Step 7).
- Real embedding providers (with provider selection, ROADMAP §5.7).
- Analysis/continuous improvement over knowledge (deferred, ROADMAP §2.3).