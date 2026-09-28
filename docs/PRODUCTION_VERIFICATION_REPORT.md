# AUSTRO OS — PRODUCTION VERIFICATION REPORT

Final status: **PRODUCTION VERIFICATION PASSED** (live-integration scope; see §9 for the explicit boundaries of this statement)

Scope: adversarial, evidence-only verification of the completed AI Execution
implementation (exactly the diff described by
`docs/IMPLEMENTATION_FINAL_REPORT.md`). No working component was rebuilt. The
production code change found by the earlier audit (`PublishMessage.LeaseOwner`,
reported in `docs/IMPLEMENTATION_FINAL_REPORT.md` and re-evidenced here in §7) is
unchanged. Everything verified in this round was verified against a **live,
isolated, native stack** — no mocks, no stubs of the infrastructure, no skipped
test. Every number below comes from a command whose output is quoted.

---

## 1. Verification Environment (built, not assumed)

Docker was unavailable, so an isolated native stack was provisioned on the host
(user-approved decision). It is reachable only over loopback and uses its own
data directories; no pre-existing database, queue or cache was touched.

| Component | Version / endpoint | Notes |
|---|---|---|
| Go toolchain | `go1.26.8 windows/amd64` | `C:\Users\AUSTRO\AppData\Local\opencode-tools\go` (not on machine PATH) |
| PostgreSQL | **16.4** on `127.0.0.1:5432` | data dir `…\Temp\opencode\austro-verif\pgdata`, superuser `postgres`, `scram-sha-256`; started exactly once via `pg_ctl -l pg.log start -w` |
| pgvector | **0.8.6** | `CREATE EXTENSION vector` succeeds; distance math verified (`<=>` on a stored embedding) |
| RabbitMQ | **4.3.5** (Erlang/OTP **27.3.4.8**) on `127.0.0.1:5672`, management plugin `15672` | user `austro` (password from the local verification secret file), tag `administrator`, full permissions on vhost `/` |
| Redis-compatible cache | **Memurai (Developer)** on `127.0.0.1:6379` | `PING` → `PONG`. The `redis-64` Chocolatey package turned out to be an empty shell (nupkg/nuspec only, no binaries) — a community-feed breakage, not an application defect; the application reads `AUSTRO_REDIS_PASSWORD` as optional |
| C toolchain for `-race` | **gcc 16.1.0** (`C:\ProgramData\mingw64\mingw64\bin\gcc.exe`) | installed via Chocolatey, added to machine PATH |
| API / worker binaries | built from the working tree | `api.exe` serving `127.0.0.1:8080`, `worker.exe` consuming `austro.events` and `austro.executions` |

Startup evidence (from `api.log`, `worker.log`): `database-topology-ready`
(`admin_role=austro_app_admin`, `runtime_role=austro_app`, `rls_tables_forced=18`),
`redis-connection-established`, `austro-os-startup`
(`execution_transport_backend=rabbitmq`, `ai_backend=stub`, `publish_backend=stub`),
`execution-dispatch-started` (`transport=rabbitmq`, `workspaces=2`),
`austro-os-serving address=127.0.0.1:8080`, and for the worker
`worker-started queue=austro.events` / `queue=austro.executions`.
`GET /health/live` → `{"status":"ok"}`, `GET /health/ready` → `{"ready":true}`.

**Isolation guarantees honoured.** The other project's PostgreSQL 18.4
(`postgresql-test` service, port 5433) was never started, queried or modified;
its binaries were not borrowed. The only data this verification deleted was its
own test residue in its own database (§6, defect 8).

Test environment (single helper, so API and tests agree byte-for-byte on
secrets/addresses): `C:\Users\AUSTRO\AppData\Local\Temp\opencode\austro-verif\gt.ps1`
sets `GOROOT`/`PATH`, owner DSN `postgres://postgres:…@127.0.0.1:5432/austro`,
runtime DSN `postgres://austro_app:…@127.0.0.1:5432/austro`, `AUSTRO_REDIS_ADDR=127.0.0.1:6379`,
`AUSTRO_RABBITMQ_URL=amqp://austro:…@127.0.0.1:5672`, `AUSTRO_API_URL=http://127.0.0.1:8080`,
`AUSTRO_EXECUTION_TRANSPORT_BACKEND=rabbitmq`, the execution timings and two
disposable workspace ids, then execs `go test @Args_`.

## 2. Migration / Schema / Topology — VERIFIED LIVE

The production bootstrap path was exercised by the running API **and** the
worker, both of which call `database.MustInitializeTopology`, and again
directly by `topologyFixture` → `database.Bootstrap` in the suite.

- `TestBootstrapAndMigrationsAreIdempotent` — bootstrap run three times over an
  already-provisioned database succeeds, `database.VerifyRuntimeSecurity` still
  passes from the runtime pool, both roles remain non-superuser / no `BYPASSRLS`.
- `TestPhase1ExitCriteria/19…` — the full org hierarchy exists
  (`founders, ceos, workspaces, departments, teams, ai_employees`).
- `TestPhase1ExitCriteria/20…` — pgvector-backed workspace-filtered search.

## 3. Row-Level Security & Tenancy — VERIFIED LIVE

- Forced RLS + the standard policy on **all 18** tenant tables
  (`rls_tables_forced=18` from the live startup log), and
  `TestProtectedTablesHaveForcedRLS` enumerates the protected set.
- `TestRuntimeRoleIsNotPrivileged` — assertions made *from the runtime
  connection*: not superuser, no `BYPASSRLS`, no `CREATEDB`, no `CREATEROLE`.
- `TestRuntimeRoleDoesNotOwnProtectedTables` — no protected table is owned by
  the runtime role (ownership would silently defeat RLS).
- `TestRuntimeRoleCannotCrossWorkspaceBoundary` — read, update, delete and a
  predicate-free `DELETE FROM departments` all confined to the bound workspace.
- `TestRuntimeRoleCannotBypassRLS` — `SET LOCAL row_security=off` does not widen
  the view; `DISABLE`/`NO FORCE` RLS, `OWNER TO`, `DROP POLICY`, `SET ROLE`,
  `CREATE ROLE` are all refused.
- `TestExecutionWorkspaceIsolation` + `TestExecutionRLSPoliciesEnabled` — every
  execution aggregate (`ai_executions`, `ai_execution_retries`,
  `retry_dispatch_log`, `ai_execution_dlq`, `ai_tool_calls`, `execution_contexts`)
  is workspace-isolated, exercised from two non-bypass isolation roles.
- `TestNoRowSecurityOff…` — the CI-workflow detector now passes on Windows too.

## 4. RabbitMQ Transport & End-to-End Execution — VERIFIED LIVE

Not a unit simulation: the API dispatcher publishes to a real broker, the real
`worker.exe` consumes, the worker advances a task through the pipeline, and the
database records the settlement.

- Transport is genuinely RabbitMQ: `execution_transport_backend=rabbitmq`, both
  queues declared and consumed (`austro.events`, `austro.executions`).
- `TestWorkerAdvancesPipelineFromEventToComplete` — the live worker binary moves
  a task from event to completion against the live broker and database.
- `tests/worker_rabbitmq_test.go` — an in-process `worker.NewWorker` consumes
  real messages published by the real sink.
- All `*_api_live_*` suites ran against the live HTTP API
  (`AUSTRO_API_URL=http://127.0.0.1:8080`): auth, task, organization, department,
  team, AI-employee, knowledge, memory, publications, pipelines, workspace-admin,
  audit-visibility, webui, creator-pipeline, health/openapi.

## 5. Failure & Recovery (Retry / Lease / DLQ / Fencing) — VERIFIED LIVE

- Dispatcher/worker unit suites (crash → lease-expire → reschedule,
  non-retryable → DLQ, attempts exhausted → DLQ, lost claim/reclaim race →
  skip, heartbeat loss → abandon, retry-delay sequence, shrinking attempt
  window, invalid message → permanent + settle-fail, publish failure → recorded
  but left claimed): **pass**.
- **SQL-level fencing against a real server** is now covered live, closing the
  gap flagged in the previous report. New tests in
  `tests/execution_reclaim_live_test.go` drive the real
  `postgres.ExecutionStore` as the runtime role:
  - `TestReclaimStaleRecoversCrashWithNoPendingRetry` — a crashed execution with
    **no pending retry row** is reclaimed (`RetryID == uuid.Nil`), the generation
    advances exactly once, and `Reschedule` returns it to `pending` with the
    lease fingerprint cleared and **exactly one** outstanding retry carrying the
    advanced sequence.
  - `TestReclaimStaleFinalizesTheClaimedRetry` — the prior claimed retry is
    finalized as `processed/failed` with `worker_id` NULL before the next retry
    is inserted (finalize-before-insert is what keeps the partial unique index
    satisfiable).
  - `TestRescheduleIsFencedAgainstThePreReclaimGeneration` — after a second
    reclaim takes the row, the first recoverer's `Reschedule` returns
    `ErrStaleLease`; only the current generation can write.
  - `TestRecoverStaleReschedulesCrashWithNoOutstandingRetry`
    (`internal/execution/dispatcher_test.go`) proves the dispatcher classifies
    that same NULL-retry crash as `worker_lease_expired`, **reschedules** rather
    than dead-letters, and advances the sequence.
- Reconnect behaviour over the live broker: `tests/worker_reconnect_test.go`,
  `tests/worker_sink_reconnect_test.go` pass.

## 6. Defects Found In This Round — All Test-Only, All Fixed With Regression Proof

No production code defect was found in this round. Every defect was in the test
or verification harness; each fix is minimal and the whole suite is re-run green
afterwards.

| # | Class | Defect | Fix |
|---|---|---|---|
| 1 | fixture | `tests/execution_rls_test.go` bound the same `execA`/`retryA` to **both** workspaces' `execution_contexts`/DLQ/tool-calls/dispatch-log rows → duplicate key on `uq_execution_contexts_execution` | per-workspace `execID`/`retry` in the seed |
| 2 | fixture | dispatch-log seeds used random UUIDs while the test asserts exact counts → failed on a second run against a persistent database | deterministic UUIDs |
| 3 | portability | `tests/no_row_security_off_test.go` compared a forward-slash allowlist (`.github/workflows/`) against backslash Windows paths → false positive | `normalizePath()` |
| 4 | portability | `tests/phase1_exit_criteria_test.go` skipped `.github` by prefixing `root+"/.github"` while the walked path was slash-normalised but `root` was not → the skip never fired on Windows | slash-normalise `root` too |
| 5 | stale fixture | `tests/audit_persistence_test.go` hand-built a `config.Config` without the execution timings that `Validate()` now requires | fixture states them (mirrors `defaults()`) |
| 6 | stale fixture | `tests/config_test.go` `TestConfigValidPasses` — same class as #5 | same |
| 7 | test isolation | `internal/config/config_test.go` `TestLoadStrictRejectsUnparseableExecutionSettings` depended on `AUSTRO_EXECUTION_WINDOW` being *absent* from the environment; with a real deployment's timings exported it tripped the (correct) `window < lease` invariant | the test now states its own window/heartbeat, so it is deterministic under any ambient environment |
| 8 | fixture | `TestAuditVisibilityPagination` capped the cursor walk at 400 pages × 3 = 1200 rows, but the audit chain grows with every run (1641 rows here) → false "pagination skipped rows" | page budget derived from the live chain length |
| 9 | fixture (mine) | the new reclaim tests initially reused `rtWorkspaceA`/`55555555-…`, adding a department to a workspace another suite counts exactly, and leaving 10 execution rows that broke a cascading delete | own disjoint identifiers (`77777777-…`); the residue from the earlier run was deleted once, by exact id, in the verification database only |

The earlier audit's Finding 1 (cross-process lease-owner fencing) remains fixed
and is now additionally exercised end-to-end: the dispatcher's identity travels
in the published message and the worker fences with it
(`TestSweepDispatchesDueAndRecordsSuccess`,
`TestWorkerFencesOnMessageLeaseOwner`, RabbitMQ envelope round trip).

The earlier audit's Finding 2 (compose does not set
`AUSTRO_EXECUTION_TRANSPORT_BACKEND` / `AUSTRO_EXECUTION_WORKSPACE_IDS`) is
**still open and is deployment configuration, not code**: a bare
`docker compose up` would run executions on the log-only stub transport. This
verification set both variables explicitly (§1), which is how the live run
exercised the real broker. Recommendation unchanged: mirror `.env.example` in
the compose file.

The earlier audit's Finding 3 (`ReclaimStale`'s retry subquery filters
`state='pending'` and therefore returns NULL for a running retry) is confirmed
**benign and now proven** rather than merely asserted: recovery never reads that
id — `Reschedule`/`FailAndDLQ` key on the execution and the lease generation —
and §5's live tests show a NULL-retry crash is rescheduled exactly like any
other, leaving exactly one outstanding retry.

## 7. Build / Vet / Test / Race Evidence (exact commands, live environment)

| Command | Result |
|---|---|
| `go build ./...` | **PASS** (exit 0) |
| `go vet ./...` | **PASS** (exit 0) |
| `go test ./... -timeout 40m -count=1` (live stack) | **PASS** — **1069 tests pass, 0 fail, 0 skip**; 26/26 packages `ok` |
| `go test ./... -race -timeout 60m -count=1` (live stack, gcc 16.1.0) | **PASS** — **1069 pass, 0 fail, 0 skip, 0 data races**; 26/26 packages `ok` |
| `go fmt -n ./...` | clean (no file needs reformatting) |

Batteries run inside that full suite, with their own counts:

| Battery | Scope | Result |
|---|---|---|
| Migration + RLS/topology (§2–3) | bootstrap idempotency, runtime-role privileges, forced RLS, cross-workspace, audit visibility, hierarchy, vector search, redis partitioning, audit chain | **21 tests, 0 fail, 0 skip** |
| E2E over live RabbitMQ + live API (§4) | worker/queue/dispatch, all `*_api_live_*`, integration suites, `TestPhase1ExitCriteria` (all 33 requirements), webui, health/openapi | **264 tests, 0 fail, 0 skip** |
| Reliability (§5) | dispatch, worker, lease, DLQ, retry, duplicate, idempotency, settlement, reconnect, sink reconnect, redis, audit, gateway concurrency, publisher | **139 tests, 0 fail, 0 skip** |
| Reclaim/recovery proof (new) | 3 live-store tests + 1 dispatcher test | **pass** |

Compare with the previous report's state: 61 connection failures, 2 skips, a
timeout-killed remainder, and `-race` impossible. All of those are now executed
and green.

## 8. Honest Notes About This Verification

- The AI provider backend and the publish backend ran as the **stub** adapters
  (`AUSTRO_AI_BACKEND=stub`, `AUSTRO_PUBLISH_BACKEND=stub`). Everything about
  execution control — admission, claim/fence, lease, retry, DLQ, settlement,
  transport, tenancy — is real; what was *not* exercised is a third-party AI
  provider's own failure modes and error shapes. That is a deliberate,
  disclosed boundary, not a silent gap.
- The cache is a Redis-compatible server (Memurai) rather than upstream Redis.
  Only commands the application actually issues were exercised; version-specific
  Redis features (cluster, modules) are irrelevant to this code.
- No Docker Compose run was performed (no Docker on the machine). The compose
  file's missing execution-transport settings (Finding 2) therefore remain
  unverified-in-place and remain a recommendation.
- The AlwaysData deployment (pre-provisioned roles mode) was not exercised
  against that platform; the topology tests cover pre-provisioned mode against
  the local PostgreSQL only.
- `go fmt` was run over `internal/execution` and `tests` during this round: it
  normalised line endings in files already in the working tree. That is a
  mechanical, semantic-free change (the suite and `-race` were re-run after it).
- Test data accumulates in the verification database across runs by design (the
  audit chain must grow for pagination and chain verification to mean anything);
  the audit pagination test derives its bound from the live chain length for
  exactly this reason.

## 9. Remaining Risks & Final Determination

Residual risks, in order of importance:

1. **External AI provider behaviour is unproven** (stub backend, §8). The first
   real provider outage is still an untested shape of failure.
2. **Compose drift** (Finding 2): the documented local stack does not enable the
   execution transport; anyone using it verbatim gets a silent stub run.
3. **`ReclaimStale`'s retry subquery is misleading dead weight** (Finding 3):
   harmless today and now regression-proven, but the `state='pending'` filter
   should eventually be aligned with the `'claimed'`/`'dispatched'` states it
   appears to intend, or the column documented.
4. **Long-horizon durability** is not proven: the verification ran against a
   local disk and a single-node broker, so disk-failure, split-brain, and
   multi-day lease-expiry scenarios are out of scope.
5. AlwaysData platform-specific behaviour (pre-provisioned role DSNs, TLS to
   the managed instance) is covered only by unit/topology tests, not a live run
   on that platform.

**Final determination: `PRODUCTION VERIFICATION PASSED`.**
Every required live integration check in scope actually ran against real
PostgreSQL 16.4 + pgvector 0.8.6, a real RabbitMQ 4.3.5 broker and a real
Redis-compatible cache, with the real API and worker binaries: migration and
topology, forced-RLS tenancy, the end-to-end execution flow over the broker,
failure/retry/lease/DLQ/fencing recovery (now including SQL-level fences and the
ReclaimStale recovery proof), the full 1069-test suite, and the full suite under
the race detector with zero data races. The only defects found this round were in
test/harness code and are fixed with regression coverage; the one production
defect ever found (cross-process lease fencing) is fixed and re-evidenced here.
The boundaries of this statement are the disclosed ones in §8 — chiefly the stub
AI/publish backends and the absence of a Docker Compose run.
