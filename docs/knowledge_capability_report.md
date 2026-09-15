# AUSTRO OS — Knowledge Capability Report

Branch `arena/01a09186-austro-os`, head `416d3cf`.
Verified against PostgreSQL 16.2 + pgvector 0.6.2 and Redis 6.2.14 running locally,
plus GitHub Actions for the live stack.

## Capability

Create, read, update, list, search and delete workspace knowledge documents —
campaign rules, audience profiles, style guides, brand facts — with similarity
search over their embeddings.

## Previous Gap

The domain, Postgres adapter, RLS policy and audit integration were already
built and correct. What was missing was everything a person could use:

| Layer | Before |
|---|---|
| Domain model | Present |
| Database contract | Present, but no invariants enforced |
| Store | `Create`, `Get`, `Upsert`, `Delete`, `List`, `Search` |
| Service | Same, **no `Update`** |
| Authorization | No RBAC rule existed |
| HTTP API | **None** |
| OpenAPI | **None** |
| UI | **None** |

ADR-007 named only `knowledge.create` and `knowledge.search` and deferred
routing "to keep the Phase 1 OpenAPI operation-count gate untouched". That gate
is now a floor, so the reason for the deferral no longer holds.

Two defects in what *was* there would have been inherited by any surface built
on top:

- `List` returned every document with neither `LIMIT` nor `ORDER BY` —
  unbounded and non-deterministic at the same time.
- There was no `Update`; only `Upsert`, which is `INSERT ... ON CONFLICT (id)`.
  Routing an update through it would resurrect a document deleted between the
  caller's read and the write.

## Implementation

`internal/knowledge/list.go` — cursor, filter and page-size validation,
`PageQuery`/`Page`, keyset cursor encode/decode. Bounded to 200, ordered by
`(created_at DESC, id DESC)`, fetches `limit+1` so that "there is a next page"
is known rather than inferred.

`infrastructure/postgres/knowledge.go` — `ListPage` and `Update` added.
`Update` is a real `UPDATE ... WHERE id = $1 AND workspace_id = $2` that reports
`ErrNotFound` when zero rows are affected. It re-embeds only when the content
changes, so a title-only edit does not spend an AI call to re-vectorize
unchanged input.

`Service.Update` sets `updated_at` rather than leaving it to the adapter — an
invariant only one implementation happens to enforce is not an invariant.

`MaxContentLength` exported so the HTTP boundary rejects an oversized body
before the domain sees it.

## Backend API

| Method | Path | Rule |
|---|---|---|
| `POST` | `/knowledge` | `knowledge.create` |
| `GET` | `/knowledge` | `knowledge.read` |
| `GET` | `/knowledge/{id}` | `knowledge.read` |
| `PATCH` | `/knowledge/{id}` | `knowledge.update` |
| `DELETE` | `/knowledge/{id}` | `knowledge.delete` |
| `POST` | `/knowledge/search` | `knowledge.search` |

All `ScopeSelf`, all workspace-scoped. The founder is refused on every route:
the role is organization level with no home workspace, so there is no scope to
serve.

## Database Changes

`migrateKnowledge`, run as the admin handle during bootstrap, additive and
idempotent:

- `knowledge_kind_valid` — `CHECK (kind IN ('document','campaign_rule','style_guide'))`
- `knowledge_title_not_blank` — `CHECK (btrim(title) <> '')`
- `knowledge_content_not_blank` — `CHECK (btrim(content) <> '')`
- `idx_knowledge_workspace_created` — `(workspace_id, created_at DESC, id DESC)`

No schema, column or index was altered or dropped. Verified idempotent by
running the bootstrap three times and confirming the constraint count stayed at
three. It fails loudly if pre-existing rows violate an invariant rather than
skipping the constraint, which would leave the table accepting values the domain
cannot handle.

## Security Model

- The workspace comes only from the verified token. No query parameter, header
  or body field selects or widens it; a body field naming one is an unknown
  field and is refused, as is a query parameter of the same name.
- `{id}` is the **document**, so a cross-tenant reference answers **404, not
  403** — 403 would confirm to another tenant that the document exists.
- **The embedding is never accepted and never returned.** A caller-supplied
  vector would let one workspace plant a document that another workspace's
  similarity search retrieves, which is the isolation the column exists to
  serve.
- Audit is recorded at the handler, not the service, for two structural reasons:
  `knowledge.AuditSink.Record` returns no error so a service-level record cannot
  fail closed, and the service hardcodes `ActorType: "system"` while only the
  handler holds the claims naming the person who acted.
- A successful mutation whose audit record could not be written answers **500**,
  following the convention `AuthHandler.recordAudit` set. Denials still return
  their denial.
- Query strings are parsed with `url.ParseQuery` directly. `r.URL.Query()`
  swallows its parse error, and since Go 1.17 rejects `;` as a separator, so a
  query string containing one had the offending pair silently dropped. Unknown
  parameters are refused rather than ignored; an explicitly supplied
  non-positive limit is a 400, not a silent default.

## RLS Model

Unchanged. `knowledge_documents` carries `workspace_isolation_policy` and
`FORCE ROW LEVEL SECURITY`; the runtime role owns no table and cannot bypass it.
Verified by a live test that creates a document in one workspace, then attempts
read, update and delete through a store bound to another — all refused, with the
original untouched.

## OpenAPI Changes

6 operations and 6 schemas added; `info.version` 0.5.0 → 0.6.0. The count now
stands at 24 across 18 paths. Every documented operation is backed by a
registered route and every registered route is documented, enforced by
`TestOpenAPISpecMatchesRoutes`.

## UI Changes

A knowledge card in the workspace console: create, list with the kind filter, a
cursor-driven next-page control, similarity search, edit and delete. Reuses the
existing session, CSRF token, error display and workspace selection.

Two deliberate omissions, both because there is nothing behind them:

- No status or archive control. The domain has no such concept and no soft
  delete, so `DELETE` is the only retirement path and the card offers nothing
  else.
- No document detail endpoint, so opening a document loads it through the same
  authorized `GET` as any other read.

## Tests

| Suite | Result |
|---|---|
| `internal/knowledge` | ok (52 subtests, incl. list/cursor) |
| `internal/api` | ok (41 knowledge subtests) |
| `tests/` knowledge DB suite | ok, 8 top-level tests / 16 subtests |
| OpenAPI integrity | ok |
| WebUI | ok |
| Race detector | 0 findings |

Live-API tests (`tests/knowledge_api_live_test.go`) require a running server and
execute in GitHub Actions; locally they fail on `dial tcp: lookup api … no such
host`, which is the environment, not the code.

**Mutation testing** — every security-critical rule verified by breaking it:

| Mutation | Failures |
|---|---|
| Six RBAC rules removed | 21 |
| Unknown query params ignored | 3 |
| `Update` reports success when no row matched | 1 |
| Audit failure no longer fails the request | 1 |

Sources restored byte-identically afterwards.

## Live Runtime Evidence

**VERIFIED BY RUNTIME** (PostgreSQL 16.2 + pgvector 0.6.2, Redis 6.2.14):
cursor walk covers every document exactly once; page size capped and clamping
reported; kind filter applied in SQL; cross-workspace read/update/delete all
refused with the original intact; `Update` on a nonexistent id is `ErrNotFound`;
`created_at` preserved across an update; cosine ranking returns nearest-first
with vectors differing in direction; inserts execute as the unprivileged runtime
role; that role owns no table, has no superuser and no bypass; the pagination
index exists; DB constraints reject unknown kinds and blank titles and content.
Three consecutive runs left zero leaked fixtures.

## CI Result

See the run for `416d3cf` — recorded in the PR.

## Remaining Limitations

- `List` is deprecated for API use but still used by worker paths. Removing it
  would be a refactor of the worker, not of this capability.
- `knowledge.AuditSink.Record` still returns no error, so a *service*-level
  knowledge audit cannot fail closed. The handler-level path can, and does. The
  same latent limitation exists in `internal/publish` and
  `internal/orchestration`.
- Search has no minimum similarity threshold, so a query returns its k nearest
  neighbours however distant they are.
- No document detail endpoint, so a read is the whole document.
