# U-1 / U-2 — Closing the Two Architectural Release Blockers

Branch `arena/01a09186-austro-os` · baseline `af34792` · HEAD `75d9c46` · **PUSHED: NO**

**Verdict: READY FOR REVIEW** (rationale in §22)

---

## 0. First, a correction to the starting premise

The task stated the branch head was `fc54c24`. It was not. The sandbox had been
re-cloned as a **shallow single-commit clone at `af34792`**; the previous
session's ten commits were gone from git, although all 42 modified files had
persisted on disk. `git diff 6e2ea73..HEAD` failed with `Invalid revision range`
because `6e2ea73` was not in the object database.

Recovered first: `git fetch --unshallow origin` (41 commits), then the ten
commits re-created from the persisted tree in their original logical groupings.
Their SHAs are new (`1070682`…`fa9046b`); the content is unchanged and the
frozen gate is byte-identical. Four further commits (§18) carry this task's
work.

There was also no Go toolchain, no PostgreSQL and no Docker. Go 1.22.12 was
rebuilt from source (`go1.4.3` → `1.19.13` → `1.20.14` → `1.22.12`). For
PostgreSQL, the `pgserver` wheel on PyPI bundles a complete **PostgreSQL 16.2
distribution including pgvector 0.6.2 with HNSW** — extracted, `initdb`'d and
run on 127.0.0.1:5433 with scram-sha-256 host authentication. **Everything
below marked VERIFIED ran against that live server.**

---

## 1. U-1 root cause

The workspace policies were correct and constrained nothing.

The application connected as `austro` — the table owner. PostgreSQL applies row
level security to every role **except** a table's owner, a superuser, and a role
holding `BYPASSRLS`. So for the only connection that ever served a request,
every policy in the schema was inert.

Three things hid it:

- `grep -rn "FORCE ROW LEVEL"` returned nothing anywhere in the repository.
- The integration suite creates separate non-owner roles (`workspace_a_user`,
  `workspace_b_user`) and asserts isolation *from their point of view*. That is
  precisely the shape of evidence that conceals the defect.
- The CI database role is the Postgres superuser, which bypasses RLS outright.

Two further defects were only visible once the policies actually applied:

- **Policies were created before the role they reference existed.**
  `CREATE POLICY … TO <role>` fails if the role is absent, and provisioning ran
  after the policy step. A fresh database could not bootstrap.
- **The `users` policy leaked every identity.** Its "no workspace bound" branch
  made all rows visible, so any holder of the runtime credential could
  enumerate every user in every tenant, password hashes included.

## 2. U-1 architecture — three principals

| Principal | Credential | Purpose |
|---|---|---|
| **owner** | `AUSTRO_POSTGRES_DSN` | Extensions, tables, indexes, policies, roles. Owns the tables. Closed before the process serves a request. |
| **runtime** | `AUSTRO_POSTGRES_RUNTIME_DSN` | Every request. Not a superuser, no `BYPASSRLS`, owns nothing. |
| **admin** | `AUSTRO_POSTGRES_ADMIN_USER` (default `<runtime>_admin`) | Two narrow organization-level operations. Also not a superuser, no `BYPASSRLS`. |

Nothing grants superuser or `BYPASSRLS`.

The **admin** role exists because two operations cannot be expressed by a
workspace-scoped policy: a founder listing/creating workspaces has no workspace
to be scoped to, and chain verification needs the whole chain. Its wider reach
comes from policies that *name* it — `CREATE POLICY … TO <admin> USING (true)`
on `workspaces` and `audit_events` — never from skipping row level security. The
runtime role is not a member of it, so **no session state set by application
code can widen the runtime role's view**: the escalation lives in a credential,
not in a setting a code path could leave behind on a pooled connection.

`FORCE ROW LEVEL SECURITY` is applied to all twelve protected tables, so the
owner is constrained too. `FORCE` cannot constrain a superuser or a `BYPASSRLS`
role — which is why their absence is asserted on the live connection rather than
trusted from the bootstrap, and why the superuser test fixtures still work.

## 3. U-1 exact implementation

New: `infrastructure/database/roles.go`, `infrastructure/database/runtime_verify.go`.

- `ResolveTopology(ownerDSN, runtimeDSN)` — rejects a runtime principal equal to
  the owner; derives `austro_app` from the owner DSN when unset, so a
  deployment that configures nothing still lands on the unprivileged path.
- `Bootstrap(owner, topo)` — one ordered, error-returning sequence: extensions →
  tables → migrate users → migrate audit_events → enable RLS → **roles** →
  policies → force RLS → table ownership. Roles before policies, because the
  policies name the role.
- `provisionRoles` — `CREATE ROLE … NOSUPERUSER NOBYPASSRLS NOCREATEDB
  NOCREATEROLE`, re-asserted on every boot so out-of-band drift is corrected.
  Grants `SELECT,INSERT,UPDATE,DELETE` to runtime, then `REVOKE UPDATE, DELETE,
  TRUNCATE, REFERENCES, TRIGGER ON audit_events`.
- `assertOwnership` — returns every protected table to the owner on every boot.
- `VerifyRuntimeSecurity` — **fatal** on any of: wrong connected role; superuser;
  `BYPASSRLS`; owns a protected table; any protected table missing
  `relrowsecurity` or `relforcerowsecurity`; or a live canary that commits a row
  in workspace A and finds it visible from a session bound to B (and B's own row
  invisible, catching an over-restrictive policy too).
- `users` policy narrowed to `is_founder OR workspace_id = … OR username =
  current_setting('app.auth_principal') OR id::text = …`. The user store binds
  `app.auth_principal` transaction-locally in `ByUsername`/`ByID`.

Configuration: `PostgresRuntimeDSN` + `config.DeriveRuntimeDSN`; an explicitly
supplied runtime DSN that is insecure, or equal to the owner DSN, is rejected.

## 4. U-1 runtime evidence

All against live PostgreSQL 16.2 + pgvector 0.6.2, executed in this sandbox.

`tests/rls_runtime_role_test.go` + `tests/runtime_topology_test.go` — **assertions
made from the runtime connection, never from the privileged one.**

| Req | Test | Result |
|---|---|---|
| A | `TestRuntimeRoleIsNotPrivileged` | PASS — `rolsuper=f rolbypassrls=f rolcreatedb=f rolcreaterole=f` |
| B | `TestRuntimeRoleDoesNotOwnProtectedTables` | PASS — 0 owned of 12; all 12 exist |
| — | `TestProtectedTablesHaveForcedRLS` | PASS — 12/12 enabled **and** forced |
| C | `…CannotCrossWorkspaceBoundary/read` | PASS — A sees 1 own row, 0 of B's |
| D | `…/update` | PASS — 0 rows affected |
| E | `…/delete` | PASS — 0 rows affected |
| — | `…/unscoped_write_is_confined` | PASS — bare `DELETE FROM departments` reached 1 row, not 2 |
| F | `TestRuntimeRoleCannotBypassRLS` | PASS — `SET LOCAL row_security`, `DISABLE ROW LEVEL SECURITY`, `NO FORCE`, `OWNER TO`, `DROP POLICY`, `SET ROLE austro`, `CREATE ROLE` all refused |
| G | `TestFounderOrgLevelSemantics` | PASS — admin sees the whole org; runtime sees 1 workspace; unbound sees 0 workspaces and only the founder |
| H | `TestBootstrapAndMigrationsAreIdempotent` | PASS — 3 bootstraps, topology intact after |
| I | fresh-DB bootstrap | PASS — see binary run below |
| J/K | `TestRuntimeTopologySatisfiesProductionStartup/{api,worker}` | PASS |
| — | `TestTopologyRejectsOwnerAsRuntimeRole` | PASS |
| — | `TestVerifyRuntimeSecurityRejectsOwnedProtectedTable` | PASS |
| — | `TestBootstrapRepairsRoleAttributeDrift` | PASS |

**Real binaries, fresh database** (DB and both roles dropped first):

```
{"message":"database-topology-ready","fields":{"admin_role":"austro_app_admin",
 "rls_tables_forced":12,"runtime_role":"austro_app"}}
{"message":"redis-connect-failed", …}      <- the only failure; no Redis daemon here
```

**Negative controls on the real binary:**

| Tampering | Observed |
|---|---|
| runtime DSN = owner DSN | `invalid configuration: … AUSTRO_POSTGRES_RUNTIME_DSN` — refuses to start |
| `ALTER TABLE tasks OWNER TO austro_app` | `database-runtime-security-failed: application runtime role owns protected table(s) tasks` — refuses to serve |
| `ALTER ROLE austro_app BYPASSRLS` | silently **repaired** to `false` by the next boot (self-healing) |
| `ALTER TABLE tasks NO FORCE` | silently **repaired** by the next boot |

The last two initially looked like the guard failing to fire. It was not: the
bootstrap runs before verification and re-asserts role attributes, grants and
ownership. That is the intended behaviour — an operator who grants `BYPASSRLS`
by hand does not leave the process unprotected after a restart — and it means
the guard's teeth are for what the bootstrap cannot repair (ownership) and for a
misconfigured DSN.

## 5. U-2 root cause

`audit_events` was created on every boot and never written to. `internal/audit`
implemented hash chaining and HMAC verification correctly and had **no
production caller** — `grep -rn "austro-os/internal/audit"` matched only tests.
Every "audit" was a structured log line: not append-only, not tamper-evident,
gone on a rotation. ADR-020 had mandated RLS on `audit_events` two phases
earlier; the table had no `workspace_id` column, no policy, and no rows.

## 6. U-2 architecture

`infrastructure/auditstore` owns persistence; `internal/audit` keeps the pure
chain primitives and gains a `Sink` interface plus `RedactDetails`. The
dependency direction stays legal (`internal/` never imports `infrastructure/`).

The writer runs on the **admin** handle, because the runtime role is
deliberately append-only on `audit_events` and so cannot read back the chain it
writes.

## 7. U-2 exact implementation

- One transaction per append, serialised by `pg_advisory_xact_lock(hashtext(
  'austro.audit.chain'))` so concurrent writers in this process or another
  cannot fork the chain.
- The head is **re-read under that lock** — the in-memory head is a cache, and
  trusting it would fork the chain whenever a second process appended first.
- `New` recovers the head from the table; only an empty table produces a genesis
  event, so a restart never forks the chain.
- `timestamp_canonical TEXT` stores the timestamp exactly as bound into the
  hash. A PostgreSQL timestamp column carries microsecond precision while the
  pre-image binds RFC 3339 with nanoseconds; reading the column back alone would
  recompute a different hash and report an intact chain as tampered.
- Schema: `workspace_id UUID` (nullable), `seq BIGINT … IDENTITY UNIQUE`, both
  added by an idempotent `migrateAuditEvents` so an existing database upgrades.
- `audit_events` joins the protected set: `audit_workspace_policy` (`workspace_id
  IS NULL OR = current_setting(...)`) plus `audit_org_policy TO <admin>`.
- **Fail closed.** `recordAudit` returns an error; login, refresh, bootstrap,
  logout and workspace creation return 500 rather than reporting a success that
  left no evidence.
- **No recursion**: the writer never audits itself.
- **Redaction**: `RedactDetails` replaces any credential-shaped key with
  `[redacted]`; values are never inspected and never copied into an error path.
- **Correlation**: trace/span IDs are taken from the request but only when they
  parse as UUIDs, so a hostile header cannot smuggle text into a tamper-evident
  log.

Wired into: bootstrap, login, refresh (with `ErrRefreshTokenReused` distinguishing
replay from an ordinary rejection), logout, authorization denials, workspace
creation, and the publishing and orchestration decision sinks.

## 8. U-2 runtime evidence

`tests/audit_persistence_test.go`, live PostgreSQL.

| Req | Test | Result |
|---|---|---|
| A | `TestLoginFailsClosedWhenAuditCannotBePersisted` | PASS — real HTTP `POST /api/auth/login`; 200 + 1 row while healthy |
| B | `TestAuditEventsArePersistedAndChainVerifies` | PASS — linear chain, last row = last append |
| C | `TestAuditTamperingIsDetected` | PASS — owner-level `UPDATE outcome` breaks verification; restoring fixes it |
| D | `TestAuditRowsAreWorkspaceIsolated` | PASS — each bound session sees 0 of the other's rows |
| E | `TestLoginFailsClosedWhenAuditCannotBePersisted` | PASS — `REVOKE INSERT` from admin ⇒ **500, not 200** |
| F | `TestAuditChainSurvivesRestart` | PASS — second `Store` recovers the head, 1 genesis, links across |
| G | `Store.Verify` against persisted rows | PASS |
| H | `TestAuditNeverPersistsSecrets` | PASS — password/token absent, `[redacted]` present, non-secret kept |
| I | `TestConcurrentAuditAppendsKeepChainLinear` | PASS — 16 goroutines, no lost rows, no parent claimed twice |
| — | `TestAuditTableIsAppendOnlyForRuntimeRole` | PASS — runtime **and** admin refused `UPDATE`/`DELETE` |

**Independent SQL inspection** of the persisted table (not my Go verifier):

```
rows persisted: 45        genesis rows: 1
parents claimed more than once (fork check): 0
rows with NULL canonical timestamp: 0
seq contiguous: t         broken chain links: 0
audit_events grants:  austro_app: INSERT, SELECT   austro_app_admin: INSERT, SELECT
```

## 9. All tests

| Suite | Command | Result |
|---|---|---|
| gofmt | `gofmt -l .` | **0 files** |
| build | `go build ./...` | OK |
| vet | `go vet ./...` | OK |
| unit | `go test ./internal/... ./cmd/... ./infrastructure/... .` | **16 pkgs ok, 0 failures** |
| race | same `-race` | **0 failures, no DATA RACE** |
| runtime security | the U-1 + U-2 suites, `-v` | **40 cases PASS, 0 FAIL** |
| runtime security under race | same `-race` | ok, no races |
| full `./tests/` | `go test ./tests/ -count=1` | **11 failures**, a strict subset of the baseline's 13 — see §16 |

Redis was also brought up locally (§9.1), which is why this is 11 rather than
the 13 seen earlier in the pass.

### 9.1 Redis brought up locally

`redislite`'s PyPI wheel bundles a real **Redis 6.2.14** server binary; it is
running on 127.0.0.1:6379 with `requirepass`. Two consequences:

- The API now authenticates and gets past Redis, failing only at RabbitMQ:
  `database-topology-ready` → `redis-connection-established` →
  `event-sink-failed: rabbitmq: initial dial failed`. Two of three external
  dependencies are now proven from the real binary.
- The test harness could not authenticate: `redisClient()` never sent a
  password, so it failed against the authenticated topology compose actually
  runs while passing against an unprotected local instance — the wrong way
  round for a security test. Fixed to read `AUSTRO_REDIS_PASSWORD`.
  `TestRedisWorkspacePartitionedKeys` and
  `TestMemoryBankWorkspaceIsolationIntegration` now run and pass, and
  `TestPhase1ExitCriteria/21` with them.

`TestPhase1ExitCriteria` is down to 4 failing subtests — 05 and 29 (RabbitMQ),
13 and 25 (running API). Note it fails more subtests when run **alone** on an
empty database, because it depends on an earlier test having bootstrapped the
schema; that ordering dependency is pre-existing.

**21 new test functions** this task, across
`tests/rls_runtime_role_test.go`, `tests/runtime_topology_test.go`,
`tests/audit_persistence_test.go`, `internal/api/audit_sink_test.go`.

## 10. Race

`go test -race` over all unit packages: 0 failures, no `WARNING: DATA RACE`.
The RLS and audit suites — including 16 concurrent chain appenders — also pass
under `-race`. The audit writer's `sync.Mutex` plus the advisory lock are
exercised by `TestConcurrentAuditAppendsKeepChainLinear`.

## 11. CI

`.github/workflows/phase2-ci.yml`, regression job now 14 steps:

- API, worker and test steps set an explicit `AUSTRO_POSTGRES_RUNTIME_DSN`.
- New step reads the API's **own log** and asserts `"runtime_role":"austro_app"`
  and no `database-runtime-security-failed`.
- The former diagnostic is now a **gate**: `psql` connects *as* `austro_app` and
  raises on superuser, `BYPASSRLS`, any owned table, or fewer than 12
  enabled/forced tables.
- Schema step updated to 12 protected tables plus the forced-flag check.
- New step runs the two suites explicitly and fails if any case fails.

No `continue-on-error`, no `exit 0`. The three remaining `|| true` are `grep`
captured into a variable that is then asserted (`grep` exits 1 when it matches
nothing) — the success path, not a mask.

Verified: both workflows parse as valid YAML (Go `yaml.v3`); the new gate SQL
and the schema SQL were **executed against the live database** —
`tables=13 rls_enabled=12 rls_forced=12 workspace_isolation_policy=10`, gate
`exit=0`. **CI itself was not triggered** — pushing is forbidden (§21).

## 12. Docker / Compose

`docker-compose.yml`: `AUSTRO_POSTGRES_RUNTIME_DSN` on `api`, `worker` and
`test`, with `AUSTRO_POSTGRES_RUNTIME_PASSWORD` **required** (`:?`) rather than
defaulted. `.env.example` documents all four new settings.

**Not executed** — no Docker in this sandbox. Static review plus a
`gopkg.in/yaml.v3` parse only. `docker build` and `docker compose config` remain
unverified.

## 13. Database roles (live, direct SQL)

```
austro           superuser=true   bypassrls=true   createrole=true   <- owner, bootstrap only
austro_app       superuser=false  bypassrls=false  createrole=false  <- runtime
austro_app_admin superuser=false  bypassrls=false  createrole=false  <- admin
ownership: austro owns 13 tables (the only owner)
```

## 14. RLS (live, direct SQL)

```
enabled=12  forced=12  of 12 protected tables
enabled-but-not-forced: (none)
org policies restricted by role:
  workspaces.org_admin_policy      -> austro_app_admin
  audit_events.audit_org_policy    -> austro_app_admin
all other policies -> PUBLIC
```

## 15. Audit (live, direct SQL)

See §8. Append-only for both application roles; chain linear; exactly one
genesis; secrets redacted.

## 16. Security regressions

The baseline was run for real: a `git worktree` at `af34792`, its own schema
bootstrapped by the **baseline's** `database.Initialize`, same live server,
separate database.

```
PostgreSQL only:              baseline 13   branch 13   sets identical
PostgreSQL + Redis (auth):    baseline 13   branch 11   branch is a strict SUBSET
```

The two tests the branch fixes are `TestRedisWorkspacePartitionedKeys` and
`TestMemoryBankWorkspaceIsolationIntegration`, which fail at baseline because
its harness cannot authenticate to an authenticated Redis (§9.1). **The branch
introduces no new failure.**

With Redis also running, the remaining 11 all need RabbitMQ (the five
`TestWorker*` + `TestEventSinkReconnects…`) or a running API
(`TestAuthLiveEndToEnd`, `TestHealthChecks`, `TestWorkspaceAdminLiveEndToEnd`,
`TestRBACLiveRoleAndDenials`, and `TestPhase1ExitCriteria` subtests 05/13/25/29).

**Zero regressions.** Every Postgres-only test — `TestRLSPoliciesProper`,
`TestWorkspaceIsolation`, `TestRLSUsersRoleDoesNotWeakenScope`, the knowledge /
pipelines / publications / task / pgvector RLS and lifecycle suites,
`TestRBACAuthorizationDecisions`, `TestOrganizationHierarchyInvariants`,
`TestSecretsNeverLoggedWhilePublishing` — passes. `FORCE` did not break the
unbound admin seeding in those fixtures, because superusers bypass row level
security regardless of `FORCE`.

Previously fixed behaviour is intact: refresh-token family revocation, audit
tamper-binding, RLS fail-closed provisioning, worker malformed-message
handling, bounded in-memory structures, HTTP timeouts/shutdown, JWT secret
validation, Redis authentication, CA roots, CI fail-fast, event-bus race safety.
`TestAuditHMACAuthenticity` still pins the HMAC key source.

## 17. Remaining limitations

1. **GitHub Actions never ran.** Pushing is forbidden, so every CI change lands
   unexecuted. The new gate SQL was executed by hand against a live PostgreSQL;
   that is not the same as a green workflow run.
2. **Docker and Compose never ran.** No Docker daemon.
3. **Full API/worker startup not completed.** With Redis now running the API
   reaches `database-topology-ready` and `redis-connection-established` and
   fails only at RabbitMQ; the worker fails at RabbitMQ before its database
   stage. J and K are proven at the database layer
   (`TestRuntimeTopologySatisfiesProductionStartup`) and for two of the API's
   three external dependencies by the real binary — not end to end. **RabbitMQ
   needs Erlang and cannot be built here.**
4. **Publishing and orchestration audit cannot fail its operation.**
   `publish.AuditSink` and `orchestration.AuditSink` return nothing, so a
   persistence failure is logged, not propagated. Fixing it changes both
   interfaces and every implementation.
5. **`audit_events` has no retention or archival path.** It is append-only and
   grows without bound.
6. **The chain hash is unkeyed SHA-256**; detectability rests on the HMAC tag
   keyed by `config.Get().JWTSecret`, which falls back to an insecure default if
   configuration was never loaded.
7. **A single-process advisory lock** serialises appends. It is correct across
   processes but adds a round trip per audit event.
8. **`X-Trace-ID`/`X-Span-ID` are still reflected into response headers**
   unvalidated. They are validated before reaching an audit record, but the
   echo itself is unchanged.
9. **`AUSTRO_POSTGRES_ADMIN_USER` has no explicit password setting**; the admin
   DSN reuses the runtime password with a different role name.
10. **OpenAPI still declares 8 operations that are not registered**, pinned by
    `TestOpenAPIPrincipleReferences` asserting exactly 18.

## 18. Commits (17 ahead of `af34792`, none pushed)

Sixteen carry the work; the seventeenth is this report.

This task's four:

```
75d9c46 Count concurrent audit appends as a delta
e94ad4e Provision the runtime topology in CI, compose, and an ADR
a0d8cab Write audit events to PostgreSQL and fail closed when they cannot be
88ee44e Enforce workspace RLS on the application runtime role
```

Preceded by the ten re-created hardening commits `1070682`…`fa9046b`.

## 19. Frozen gate

```
git diff 6e2ea73c7870d1bd8f028714e58ace1c3aa56c54..HEAD -- tests/phase1_exit_criteria_test.go
  -> 0 lines
sha256  0a868d3da41f175cc263a6301f806e3bc1125093dab9cf8db98bb17b54e8769a
```

The frozen gate's own scanners were run over the new files: subtest 16 (banned
technology strings, all text files) and subtest 01 (forbidden SQL directive,
`.md` included) both PASS with this report present.

## 20. Git status

```
git status --porcelain            -> empty (clean)
git diff --check af34792..HEAD    -> exit 0 (no whitespace errors, no markers)
gofmt -l .                        -> empty
```

## 21. PUSHED = **NO**

17 commits ahead of `origin/main` — sixteen of work plus this report.
`git push` was never invoked.

## 22. Verdict — READY FOR REVIEW

U-1 and U-2 are closed with real runtime evidence: a live PostgreSQL 16.2 with
pgvector, real role topology, real persisted audit rows, real binaries, and
negative controls that prove the guard fires.

It is **not** PRODUCTION READY, because three things were never executed:
GitHub Actions (pushing is forbidden), Docker/Compose (no daemon), and the last
external dependency, RabbitMQ, which needs Erlang and cannot be built in this
sandbox. Those are exactly the gaps the CI changes in §11 are written to close,
and they need one workflow run to close them.
