# ADR-023 — Knowledge HTTP surface and bounded listing

- **Status**: Accepted
- **Date**: 2026-09-15
- **Supersedes**: nothing; completes the routing that ADR-007 §"API contract" explicitly deferred
- **Related**: ADR-007 (knowledge management), ADR-003 §3.2 (contract finalized before endpoints),
  ADR-004 (AI gateway / embedding), ADR-006 (task management — same surface shape),
  ADR-016 (web application), ADR-021 (runtime topology and persistent audit),
  CONSTITUTION P7 (Separation of Concerns), P9 (Security by Design), P10 (Privacy by Design),
  P13 (Observability)

## Context

ADR-007 finalized the knowledge contract at two operations — `POST /knowledge` and
`POST /knowledge/search` — and deliberately deferred HTTP routing "to keep the Phase 1
OpenAPI operation-count gate (`require.Equal(t, 13, spec.totalOps)`) untouched", noting
that "the contract remains authoritative for later wiring".

That gate is now `require.GreaterOrEqual(t, spec.totalOps, 13)`, so the constraint that
caused the deferral no longer applies. Meanwhile the domain, the persistence port and the
Postgres adapter already implement more than two operations: `Upsert`, `Get`, `List`,
`Search` and `Delete`, all workspace-scoped and RLS-bound. The capability was therefore
complete at the service boundary and unreachable from any client.

This ADR records the contract as implemented, so the surface is derived from what already
exists rather than invented.

## Contract

### Entity

`knowledge.Document`, workspace-scoped, with a PGVector embedding.

### Fields

| Field | Type | Notes |
| --- | --- | --- |
| `id` | UUID | server-generated, primary key |
| `workspace_id` | UUID | `NOT NULL`, FK → `workspaces(id) ON DELETE CASCADE`; taken from the verified token, never from the request |
| `kind` | TEXT | `document` \| `campaign_rule` \| `style_guide`; validated in the domain, now also by a CHECK |
| `title` | TEXT | `NOT NULL`, non-blank after trimming |
| `content` | TEXT | `NOT NULL`, non-blank, ≤ 64 KiB (`maxContentLength`) |
| `embedding` | `vector(10)` | produced by the AI gateway; never accepted from a client |
| `created_at`, `updated_at` | TIMESTAMP | server-set |

`embedding` is deliberately absent from every request body. A client-supplied vector would
let one workspace plant a document that another workspace's similarity search retrieves,
which is the isolation property the column exists to serve.

### Status / type

There is **no status, lifecycle or archive concept** in the knowledge domain, and this ADR
adds none. Unlike tasks (ADR-006), where a terminal `cancelled` status is the retirement
path, knowledge has exactly one retirement path and the persistence port already names it:
`Delete`. Adding an archive flag would be a new domain concept, not a completion of this one.

### Ownership and workspace scope

Scope comes only from `WorkspaceFromClaims`, which reads the verified token. There is no
query parameter, header or body field that can select or widen a workspace. Every store
operation runs in a transaction that first binds `app.current_workspace` on that connection,
so row-level security constrains it; an explicit `workspace_id` predicate is kept as
defense-in-depth.

### CRUD semantics

| Operation | Method and path | Semantics |
| --- | --- | --- |
| Create | `POST /knowledge` | validate → embed → persist. A failed embed persists nothing. |
| Read | `GET /knowledge/{id}` | one document in the caller's workspace, else 404 |
| Update | `PATCH /knowledge/{id}` | title / content / kind. Content change re-embeds. |
| Delete | `DELETE /knowledge/{id}` | hard delete; 404 if absent |
| List | `GET /knowledge` | bounded page, newest first, optional `kind` filter |
| Search | `POST /knowledge/search` | top-k by cosine similarity to the embedded query |

`{id}` is the document, so a cross-tenant reference returns **404 rather than 403**. The
identifier is not the workspace, and answering 403 would confirm to another tenant that the
document exists.

### Update semantics (new in this ADR)

The service had no `Update`; only `Upsert` existed on the port. `Upsert` is
`INSERT … ON CONFLICT (id) DO UPDATE`, so routing an update through it would resurrect a
document deleted between the read and the write. A dedicated `Update` is therefore added to
the port as a real `UPDATE … WHERE id = $1 AND workspace_id = $2` that returns
`ErrNotFound` when zero rows are affected. `Upsert` remains the create path.

Update re-embeds **only when the content changes**. Re-embedding on a title-only edit would
spend an AI call for no change in the vector's input.

### List and search semantics

`List` previously had neither `LIMIT` nor `ORDER BY`: it returned every document in the
workspace in whatever order the planner chose. That is both an unbounded response and a
non-deterministic one, so it is replaced by a bounded page.

- Ordering is `(created_at DESC, id DESC)`. The `id` tie-break matters: without it a page
  boundary falling between two rows with an identical timestamp is arbitrary, so a cursor
  walk could skip or repeat a row.
- Pagination is keyset, not offset. Offset skips or repeats rows under concurrent insert.
- The store fetches `limit + 1` so that "there is a next page" is known rather than
  inferred from a full page.
- `DefaultPageSize = 50`, `MaxPageSize = 200`. An oversized limit is clamped and the
  effective value is reported, not rejected.
- An unparseable cursor is an error, never a silent reset to the first page.
- `Search` keeps the service's existing clamp (`limit > 50 → 10`) and adds explicit
  rejection of a malformed limit rather than a silent substitution.

### Validation

Enforced in the domain (`knowledge.New`) and re-enforced at the boundary:

non-nil workspace · recognized kind · non-blank title · non-blank content ·
content ≤ 64 KiB.

Request bodies are decoded with `MaxBytesReader`, `DisallowUnknownFields` and a
trailing-data check, so an unknown field is a 400 rather than a silently ignored one.

Query strings are parsed with `url.ParseQuery` directly rather than `r.URL.Query()`. That
method swallows its parse error and returns whatever it could read, and since Go 1.17
`url.ParseQuery` rejects `;` as a separator — so a query string containing one would have
the offending pair silently dropped and the request served as though the parameter had never
been sent. This was a real defect in the task listing (ADR-006 surface) and is not repeated.

### Audit

Recorded at the **handler**, not the service. Two reasons, both structural:

`knowledge.AuditSink.Record` returns no error, so a service-level audit call cannot fail
closed — a dropped audit record would be invisible. The handler holds `*auditstore.Store`,
whose `Append` does return an error, so a security-relevant outcome that cannot be recorded
fails the request rather than reporting success.

The service also hardcodes `ActorType: "system"`. Only the handler has the verified claims,
so only the handler can name the person who acted.

Create, update, delete and search are audited, along with denials, carrying actor,
workspace, document target, outcome and trace id. The service's own audit sink stays
configured but is not the authority.

### Authorization

Six rules, present in both `Rules()` and `ImplementedRules()`, with matching grants in
`PermissionsForRole` — a rule alone does nothing, because `authz.hasPermission` checks the
token's claim-derived permissions first.

`workspace_admin` and `workspace_member` may perform all six. The founder may not: the role
is organization level with no home workspace, so there is no scope to serve, and widening to
every tenant would defeat the isolation the policy enforces.

### RLS

Unchanged. `knowledge_documents` is in `rlsTables()`, so `FORCE ROW LEVEL SECURITY` applies,
and it carries exactly one `workspace_isolation_policy`:

```sql
USING (workspace_id = NULLIF(current_setting('app.current_workspace', true), '')::UUID)
```

A permissive policy with no `WITH CHECK` clause reuses `USING` for inserts, so writes are
constrained by the same expression. The runtime role is non-superuser, has no `BYPASSRLS`,
and owns no protected table.

### Indexes and constraints

Existing: `idx_knowledge_workspace(workspace_id)`,
`idx_knowledge_kind(workspace_id, kind)`,
`idx_knowledge_documents_embedding` (HNSW, `vector_cosine_ops`).

Added by a `migrate-knowledge` bootstrap step, additive and idempotent:

- `knowledge_kind_valid` CHECK on the three recognized kinds
- `knowledge_title_not_blank` CHECK
- `knowledge_content_not_blank` CHECK
- `idx_knowledge_workspace_created ON knowledge_documents(workspace_id, created_at DESC, id DESC)`

The constraints duplicate domain validation on purpose. The service is not the only path to
the table, and a row written with a kind the domain does not recognize would later fail
every read-side classification with a confusing error.

The migration fails loudly if pre-existing rows violate an invariant rather than skipping
the constraint, which would leave the table accepting values the domain cannot handle.

## Consequences

- The knowledge surface is 6 operations, extending ADR-007's two. The extension adds no new
  domain behaviour: every operation maps to a method the persistence port already declared.
- OpenAPI gains 6 operations and 4 schemas; route/spec parity continues to hold in both
  directions.
- `DocumentStore` gains `Update` and `ListPage`. Existing implementations are updated in the
  same change, so no adapter is left non-conforming.

## Out of scope

- Real embedding providers (ADR-004 provider selection).
- Producer/consumer wiring of knowledge into the Creator pipeline (ADR-010).
- Any archive or soft-delete concept.
