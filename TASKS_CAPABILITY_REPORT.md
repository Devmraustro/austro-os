# Tasks capability — completion report

Branch `arena/01a09186-austro-os`, HEAD `ccf5c24`. PR #1, all three checks passing.

Every claim below is classified. **VERIFIED BY RUNTIME** means a command ran and
its output is quoted or reproduced in the evidence column. **STATICALLY
VERIFIED** means the property was read out of the source or the compiled
artefact but not executed here. **CONFIGURATION REQUIRED** means it depends on a
deployment setting. **NOT IMPLEMENTED** means it does not exist.

## 1. Previous gap

The `internal/task` package already held a complete domain model, a Postgres
store and a service with a server-side lifecycle check, and ADR-006 specified
the HTTP contract. None of it was reachable:

| Layer | Before | After |
| --- | --- | --- |
| Domain | present, incl. `CanTransition` | unchanged, plus `list.go` |
| Store | `infrastructure/postgres/tasks.go` | `+ ListPage` |
| Service | complete | `+ ListPage` |
| RBAC | **zero task rules** | 5 rules × 2 registries + grants |
| HTTP API | **none** | 5 routes |
| OpenAPI | **none** | 5 operations, 5 schemas |
| Browser UI | a `<li>` claiming it was unavailable | working card |

A user could not create, list or move a task by any route.

## 2. Backend API

Five routes, exactly the contract in ADR-006 §82. Flat, with the workspace taken
from the verified token and never from the request.

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/tasks` | create in the caller's workspace |
| `GET` | `/tasks` | bounded, newest-first listing |
| `GET` | `/tasks/{id}` | read one task |
| `PATCH` | `/tasks/{id}` | update non-lifecycle fields |
| `POST` | `/tasks/{id}/transition` | apply one lifecycle transition |

There is deliberately **no `DELETE`**. A task is retired by moving it to a
terminal status, so history is never destroyed and ADR-006 lists no delete.
CI confirms a `DELETE` is refused by the deny-by-default authorizer
(`"action":"DELETE","resource":"/tasks/{id}"` → `authz.denied`).

`{id}` is the task, so a cross-tenant reference returns **404, not 403**: the
identifier is not the workspace, and answering 403 would confirm to another
tenant that the task exists.

## 3. Database changes

`migrateTasks`, wired as a `migrate-tasks` bootstrap step. Additive only — no
column added, removed or retyped, so existing rows and queries are unaffected.

Four `CHECK` constraints (`tasks_status_valid`, `tasks_priority_valid`,
`tasks_assignee_type_valid`, `tasks_title_not_blank`) and one index
(`idx_tasks_workspace_created ON tasks(workspace_id, created_at DESC, id DESC)`).

Each is guarded by `IF NOT EXISTS`, so the migration is idempotent.

| Evidence | Result |
| --- | --- |
| Constraints present | `tasks_assignee_type_valid`, `tasks_priority_valid`, `tasks_status_valid`, `tasks_title_not_blank` — **VERIFIED BY RUNTIME** |
| Idempotency, 3 consecutive bootstraps | `ok` × 3, constraint count still exactly **4** — **VERIFIED BY RUNTIME** |
| Legacy row violating an invariant | `ADD CONSTRAINT` fails: `check constraint … is violated by some row`, and `migrateTasks` returns the error rather than skipping — **VERIFIED BY RUNTIME** (mechanism reproduced on a scratch table) |

The last row is a **CONFIGURATION REQUIRED** item for operators: on a database
that already holds an out-of-vocabulary status, startup will fail loudly until
the data is corrected. That is the intended behaviour — silently skipping the
constraint would leave the table accepting values the lifecycle cannot handle.

## 4. Security model

- **Lifecycle is server-side.** A transition request names only the destination;
  the current status is read from the database inside the same operation.
  `status` is not accepted by `PATCH`, so a field update cannot smuggle a move
  past the transition check.
- **Scope comes from claims only.** `WorkspaceFromClaims` reads the verified
  token. There is no parameter, header or body field that can widen or change it.
- **Deny by default.** The five routes exist in `Rules()` *and*
  `ImplementedRules()`, and matching patterns were added to
  `PermissionsForRole` — without the latter the claims check denies even though
  a rule allows. Founder is refused: organization level, no home workspace.
- **Client errors are 4xx.** `TaskHandler.fail` classifies every sentinel, and
  bounds are applied in `parseTaskQuery` so the classification does not depend
  on where the error surfaced.
- **Audit at the handler**, with the verified caller as actor. The service's own
  event sink stays a no-op because no task event consumer exists.
- **Store runs on the unprivileged runtime handle**, not the admin handle.

Three security-critical rules were mutation-tested, then restored byte-identically:

| Mutation | Subtests that failed |
| --- | --- |
| service skips `CanTransition` | 3 |
| task rules removed from `ImplementedRules()` | 32 |
| unknown query parameters ignored | 3 |
| all restored | 0 |

## 5. RLS model

Unchanged and already in force: `tasks` carries the single
`workspace_isolation_policy`, `FORCE ROW LEVEL SECURITY` is on, and the runtime
role is non-superuser, non-`BYPASSRLS` and owns no protected table. Confirmed
against a live PostgreSQL 16.2 rather than asserted from the schema.

| Property | Evidence |
| --- | --- |
| runtime role: `rolsuper=false`, `rolbypassrls=false`, not owner | **VERIFIED BY RUNTIME** |
| `relrowsecurity=true`, `relforcerowsecurity=true` | **VERIFIED BY RUNTIME** |
| exactly 1 policy on `tasks` | **VERIFIED BY RUNTIME** |
| workspace A cannot read / update / **delete** B's task | **VERIFIED BY RUNTIME** |
| production store's `Get`/`Update`/`Delete`/`ListPage` confined | **VERIFIED BY RUNTIME** |

Cross-workspace **delete** is newly covered: the pre-existing RLS test proved
read and update isolation but not delete, which is the most destructive of the
three and the hardest to notice afterwards, because a successful cross-tenant
delete leaves no row behind to inspect.

## 6. OpenAPI changes

5 operations and 5 schemas (`Task`, `TaskPage`, `CreateTaskRequest`,
`UpdateTaskRequest`, `TransitionTaskRequest`), every schema referenced. Repo
total is now **15 paths / 18 operations / 12 schemas**, up from 12/13/7.

Route/spec parity passes in both directions — no phantom operations, no
undocumented routes.

## 7. UI changes

A `#tasks-card` in the existing application: create, filter by status, page,
and move each task forward. `Tasks` was removed from the "not available in this
build" list, which previously claimed otherwise.

The property that shapes the card is that **the browser never re-derives the
lifecycle**. Each row renders buttons only for the `transitions` array the
server sent with that task, so the page cannot offer a move the domain would
refuse. Nothing in `app.js` computes what may follow what.

Terminal destinations (`completed`, `cancelled`, `failed`, `rejected`) are
confirmed before being sent; forward moves are not interrupted. After a
successful move the list is read back rather than patched locally.

Every state is handled distinctly: loading, empty, success, server validation
error, permission denied with an explanation, session expiry after the refresh
retry has been spent, and a network failure that says the task was not created
or changed rather than leaving the outcome ambiguous. Rows are built with
`createElement` and `textContent`, never `innerHTML`, because a task title is
user-supplied text.

Adding the card tripped the existing guard that enumerates every mutating call
site and requires each to be operator-initiated. Both new calls are, so they are
declared in that allowlist. The guard was then verified to still reject an
undeclared mutation (`DELETE "/knowledge/all"` → caught), so the allowlist entry
is precise rather than a blanket disable.

## 8. Tests

41 top-level test functions across four files, plus their subtests.

| File | Level | Focus |
| --- | --- | --- |
| `internal/task/list_test.go` | unit | cursor round-trip, 10 malformed cursors rejected, page clamping, total ordering |
| `internal/api/task_handlers_test.go` | API | real `Service` on an in-memory store — 70 subtests |
| `tests/task_store_live_test.go` | database | RLS, CHECK constraints, keyset pagination on PostgreSQL |
| `tests/task_api_live_test.go` | live HTTP | the whole journey over the running stack |

Covered at minimum: happy path, malformed input, unauthorized access,
cross-workspace access, invalid state transition, duplicate/unknown status,
oversized limit, corrupt cursor, unknown query parameter, and the audit trail.

**A pre-existing defect was found and fixed.** The RLS test deleted its seeded
rows from a handle it had already deferred a `Close()` on, and `t.Cleanup` runs
*after* those defers — so nothing was ever deleted and two rows per run
accumulated in the shared workspace fixtures. Measured: `54 → 56 → 58` before
the fix, `42 → 42 → 42` after. This is what makes an absolute count over a
shared tenant unreliable.

## 9. Live runtime evidence

From the CI log of run `34965381441`, job `104368978496`:

```
"message":"task-created","fields":{"status":"backlog","task_id":"da0651b0-…","workspace_id":"11111111-…"}
"event_type":"task.create","outcome":"success","principle":"Human Oversight"
"message":"authorization-denied","fields":{"action":"GET","resource":"/tasks"}
"event_type":"authz.denied","outcome":"denied","principle":"Security by Design"
"message":"task-transitioned","fields":{"from":"backlog","to":"planned",…}
"event_type":"task.transition","outcome":"failed"   ← refused cross-tenant move
"event_type":"task.get","outcome":"failed"          ← cross-tenant read → 404
"message":"authorization-denied","fields":{"action":"DELETE","resource":"/tasks/da0651b0-…"}
```

`tests/task_api_live_test.go` is the **only** test in the repository that
issues HTTP requests to `/tasks`, so these lines can only have come from it.
The job concluded `success`. **VERIFIED BY RUNTIME.**

Local PostgreSQL 16.2 evidence (no RabbitMQ here, so no local API server):

| Check | Result |
| --- | --- |
| `go build ./...` / `go vet ./...` | 0 / 0 |
| Unit suite (`./internal/... ./cmd/... ./infrastructure/... .`) | all `ok` |
| Race suite, same packages | **0** failures, 0 data races |
| Task DB tests | 35 top-level pass, 0 fail |
| Frozen gate at HEAD vs base `04786e5` | **identical**: 70 pass, same 4 fail |

Those 4 failures are `rabbitmq`, `api` and `redis` **DNS lookups** in a sandbox
that has no such hosts — reproduced identically at the base commit, so they are
not a regression. Redis was then started locally (6.2.14) and its subtest
passed, leaving only the RabbitMQ/API ones, which CI covers.

## 10. CI result

Run `34965381441`, PR #1:

```
Build, Vet, and Unit Tests              pass   1m39s
Full Phase 1 + Phase 2 Regression       pass   1m2s
Phase 1 Core Foundation Exit Criteria   pass   12s
```

**VERIFIED BY RUNTIME.** No workflow change was needed: the existing full-suite
step already exports `AUSTRO_POSTGRES_RUNTIME_DSN` and runs all of `./tests/`.

## 11. Defects found while testing

All three were real, found by writing the tests, and fixed:

1. **An optional field was mandatory.** An omitted `priority` was forwarded to
   the domain as `""`, which rejected it — contradicting both the OpenAPI
   contract and the column default. It now defaults to `normal` on create,
   while an explicitly blank priority on `PATCH` is refused rather than
   silently resetting the field.
2. **Mangled audit event types.** Event types were built by trimming a `task-`
   prefix the action names do not have, producing `task.create-task`. Now
   trimmed from the `-task` suffix: `task.create`, `task.update`,
   `task.transition`.
3. **A silently dropped query parameter.** `r.URL.Query()` swallows its parse
   error, and since Go 1.17 `url.ParseQuery` rejects `;` as a separator, so a
   query string containing one had that pair dropped and the request served as
   though it had never been sent. A caller whose cursor was mangled got the
   first page back and would believe it had paged forward. The task query is now
   parsed explicitly and a malformed query string is a 400.

## 12. Verification of the working tree

| Check | Result |
| --- | --- |
| `git status --short` | empty — **VERIFIED BY RUNTIME** |
| `git diff --check` | clean |
| `gofmt -l` | clean |
| frozen gate SHA-256 | `0a868d3d…8bb17b54e8769a` — matches |
| `git diff 04786e5..HEAD -- tests/phase1_exit_criteria_test.go` | empty |

Note: `git diff 6e2ea73c..HEAD` could not be run because that revision is not in
this clone's history. The SHA-256 match above is the equivalent check.

## 13. Remaining limitations

- **CONFIGURATION REQUIRED** — an existing database holding a status outside the
  vocabulary will fail startup until corrected (by design, see §3).
- **CONFIGURATION REQUIRED** — the founder cannot operate any workspace-scoped
  capability because the role has no home workspace. A platform property, not a
  Tasks gap; the UI explains it rather than showing a misleading empty state.
- No task event consumer exists, so the service event sink is a `NullEventSink`.
  Tasks therefore emit no bus events; the audit trail is the record.
- No `DELETE`, by design (§2).
- Chain hash detectability still rests on an HMAC keyed by
  `config.Get().JWTSecret`, which falls back to an insecure default if unset.
  Pre-existing, tracked in `U1_U2_RELEASE_BLOCKERS_REPORT.md` §25.
