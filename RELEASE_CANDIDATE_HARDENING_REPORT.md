# AUSTRO OS — Autonomous Hardening & Release-Candidate Audit

Branch: `arena/01a09186-austro-os`
Baseline: `af34792` ("Fix Phase 1 workflow coverage regression")
Result: 10 commits — 9 engineering fixes plus this report. 42 source files changed, +1986 / −579. **Not pushed.**

---

## A. Executive summary

The baseline was green, and green hid nine real defects. Four were exploitable
or availability-relevant on their own:

1. **Refresh-token family revocation did nothing.** `revokeFamily(rt.ID)`
   looked tokens up by an ID that is unique to the presented token, so
   "revoke the whole family" revoked the one token that was already revoked.
   An attacker holding a stolen refresh token kept a working session after the
   legitimate holder rotated.
2. **The audit chain could be rewritten undetected.** The hashed pre-image
   omitted `outcome`, `constitutional_principle` and the event timestamp, so
   anyone with write access to `audit_events` could change whether an audited
   operation succeeded. The HMAC tag was computed and never verified. The test
   suite already documented this as a known gap.
3. **Row level security was provisioned best-effort.** A failure to enable RLS
   or create the policies was logged at `warn` and startup continued, leaving
   the process serving requests with an isolation boundary it believed it had.
4. **One malformed pipeline event wedged the worker queue permanently.** Every
   handler error was requeued; four error paths are structural and can never
   succeed, and with a prefetch of 1 that message blocked everything behind it.

Alongside those: an unsynchronised map behind a concurrent HTTP endpoint (a
fatal `concurrent map writes`, not just a race), two unbounded in-memory
tables, an HTTP listener with no timeouts and no graceful shutdown, a
configuration validator that accepted a one-byte JWT secret, committed default
signing secrets in docker-compose, an unauthenticated Redis holding workspace
memory, a missing CA root store in the runtime image, and CI steps that could
not fail.

All nine are fixed with regression coverage. Two findings were **deliberately
not** "fixed" because the safe fix requires infrastructure this sandbox does
not have; they are recorded in §U rather than shipped blind.

### Environment note that shaped the work

The sandbox had no Go toolchain, no PostgreSQL, no Redis, no RabbitMQ and no
Docker, and its network allowlist excluded `go.dev`, `dl.google.com`, the Go
module proxy and every package mirror. Rather than ship unverified changes, a
Go toolchain was built from source: `go1.4.3` (C, gcc) → `go1.19.13` →
`go1.21.13` → **`go1.22.12`**, with modules fetched `GOPROXY=direct` from
GitHub mirrors. That gives real `go build`, `go vet`, `go test` and
`go test -race`. It does **not** give live PostgreSQL/Redis/RabbitMQ, so the
integration suite in `tests/` still runs only in CI.

---

## B. Baseline state

- `git log -1`: `af34792`; branch `arena/01a09186-austro-os`; tree clean.
- The repository was a **shallow clone of one commit**. `6e2ea73` was not in
  the object database, so the frozen-gate verification command in the brief
  errored with `Invalid revision range`. It was unshallowed
  (`git fetch --unshallow`, 41 commits) so the check could actually be run.
- `6e2ea73c7870d1bd8f028714e58ace1c3aa56c54` is a **commit**, not a blob hash,
  and is an ancestor of HEAD (`git merge-base --is-ancestor` → true).

## C–H. Issues found, severity, root cause, fix, files, tests

| # | Issue | Sev | Root cause → fix |
|---|---|---|---|
| 1 | Refresh-token family revocation was a no-op | **Critical** | `revokeFamily(rt.ID)` used the per-token ID. → `FamilyID` seeded at login, inherited on rotation; `RevokeFamily` promoted into the `RefreshTokenStore` contract (the type assertion silently did nothing for any durable backend). |
| 2 | `memoryRefreshStore` unsynchronised | **High** | Concurrent HTTP refresh → data race and fatal concurrent map write. → `sync.RWMutex`; `Get`/`Save` copy records. |
| 3 | Refresh store grew without bound | Medium | Nothing was ever deleted. → expiry reclamation + 65 536 hard bound, batched eviction (amortised O(1)). |
| 4 | Audit chain omitted outcome/principle/timestamp | **Critical** | Format string bound only event_type, actor/target, lineage. → all bound; length-prefixed encoding (a `\|`-delimited pre-image with raw binary hash bytes is ambiguous). |
| 5 | `DigitalSignature` never verified | **High** | Chain rested on an unkeyed SHA-256. → `VerifyHashChain` requires the keyed tag *and* the chain link; empty chain now fails closed. |
| 6 | RLS provisioned best-effort | **High** | `warn` + continue. → `enableRLS` and `setupRLSPolicies` are fatal; `CREATE EXTENSION vector` is fatal (the schema declares vector columns). `pgcrypto` stays a warning (`gen_random_uuid()` is built in from PG 13). |
| 7 | `user_scope_policy` org-level branch was dead | **High** | `current_setting(name, true)` returns **NULL**, not `''`, so `= ''` was never true. → `COALESCE(...)`; policy dropped and recreated so existing databases are corrected. |
| 8 | Worker requeued permanent failures forever | **High** | Four structural error paths returned ordinary errors. → `worker.ErrPermanent` sentinel; permanent failures are logged at error level and dropped, transient ones still requeued. |
| 9 | HTTP listener: no timeouts, no graceful shutdown | **High** | `http.ListenAndServe`. → `http.Server` (10s header / 30s read / 60s write / 120s idle) + SIGTERM drain so the deferred closes run. |
| 10 | JWT secrets had no length floor | **High** | Only placeholder checks. → 32-byte floor (RFC 7518 §3.2); founder password 16-byte floor. |
| 11 | Malformed numeric settings silently defaulted | Medium | `envInt`/`envDuration` swallow parse errors. → raw text validated; validation errors sorted and de-duplicated. |
| 12 | Rate limiter never reclaimed a key | Medium | Its comment claimed lazy reset; a key was only reset when the same key returned. → timed sweep + 65 536 bound. |
| 13 | `decodeJSON` accepted trailing data | Medium | Single `Decode` call. → `dec.More()` rejection. |
| 14 | CI: `gofmt -l internal/ \|\| true` | **High** | Mask. → real gate over the whole tree (tree formatted first). |
| 15 | CI: frozen-gate guard `\|\| true` | **High** | Could not fail twice over — masked status *and* a worktree-vs-index diff is always empty on a fresh checkout. → pinned by sha256. |
| 16 | CI never ran `cmd/worker` or `infrastructure` tests; never ran `-race` | **High** | Unit job was `./internal/...` only. → both added, plus a `-race` pass. |
| 17 | 9 Phase-1 CI steps could not fail | **High** | `grep \|\| echo`, `grep \| head`, grep on a directory without `-r`, grep of `openapi.yaml` when the spec is at `api/openapi.yaml`, two pure `echo` steps. → all assert and exit non-zero. |
| 18 | Phase-1 principle check word-split | Medium | Unquoted list → checked "Vision", "First", … → 18 phrases, count asserted. |
| 19 | docker-compose shipped default signing secrets | **High** | Anyone who read the repo could forge tokens; the validator could not catch a non-placeholder. → all three required from the environment. |
| 20 | Postgres `trust` auth; Redis no password; broker default creds; all ports on every interface | **High** | → credentials required, `requirepass`, loopback-only publishing for every dependency. |
| 21 | Runtime image had no CA root store | **High** | `alpine:3.19`. → `ca-certificates`; outbound HTTPS would otherwise fail verification. |
| 22 | `IsPartitionedKey` always returned false | Medium | Compared `key[:12]` to a 10-byte prefix. → `strings.HasPrefix` on a named constant. |
| 23 | Second, divergent schema copy in `infrastructure/postgres` | Medium | Missing `memory_embeddings`/`users`; policies referenced three columns that do not exist, so the block always failed and was swallowed. Nothing called `postgres.Initialize`. → duplicate schema, unreachable `WrapQuery` and dead `disableRowSecurityOff` removed; delegates to the authoritative bootstrap. |
| 24 | No ANN index on the vector tables | Medium | Every similarity query orders by `<=>`. → HNSW on both (HNSW needs no training rows, so it builds against an empty table at first boot). Performance control, so failure warns rather than aborting startup. |
| 25 | `WithTraceID` panicked on a bad UUID; `WithCorrelationID` concatenated JSON | Medium | `uuid.MustParse`; string-built JSON. → error return; `json.Marshal`. |
| 26 | pgvector version drift (pg15 local / pg16 CI) | Low | → both pg16. |

**Files changed (production):** `internal/auth/auth.go`,
`internal/audit/audit.go`, `internal/config/config.go`,
`internal/api/handler.go`, `internal/api/ratelimit.go`,
`internal/worker/worker.go`, `cmd/worker/handler.go`, `main.go`,
`infrastructure/database/database.go`, `infrastructure/postgres/postgres.go`,
`infrastructure/redis/redis.go`, `Dockerfile`, `docker-compose.yml`,
`.env.example`, both workflows, plus 22 files touched by `gofmt -w`.

**Tests added (H):** 23 top-level test functions.

- `internal/auth/refresh_family_test.go` (new) — family revocation across a
  rotation chain, non-revocation of unrelated families, family-ID inheritance,
  concurrent access, copy-on-read, the bound, input validation.
- `internal/api/limits_test.go` (new) — limiter permit behaviour, elapsed-window
  reclamation, the bound under distinct keys, 7 strict-parsing cases, body limit.
- `internal/worker/worker_test.go` (new) — drives `processMessage` against a
  fake `amqp.Acknowledger` to assert each delivery is settled exactly once with
  the right verb; permanent-error contract; both helper fixes.
- `infrastructure/redis/redis_test.go` (new) — partitioning helpers.
- `internal/config/config_test.go` — key floor (incl. exact boundary), founder
  password floor, each unparseable numeric setting, error determinism.
- `tests/hash_chain_genesis_test.go` — 6 new tamper cases; now recomputes via
  the exported pre-image helpers instead of duplicating the format string.

**Load-bearing, not nominal.** Three fixes were re-tested against their
pre-fix implementation to prove the new tests actually catch the defect:

- removing the mutex → `WARNING: DATA RACE` (then clean);
- unbinding outcome/principle from the pre-image → 3 tamper subtests fail;
- the eviction rewrite also cut that test from **61.1 s to 1.8 s**, which is
  how an O(n²) sweep in my own first attempt was caught.

## I. Tests executed

```
gofmt -l .                                                → empty
go build ./...                                            → ok
go vet ./...                                              → ok
go test ./internal/... ./cmd/... ./infrastructure/... .   → all ok (16 pkgs)
go test ... -count=1 -race                                → all ok, no races
go test ./tests/ -run <infrastructure-free subset>        → 40 passed
```
Toolchain: `go version go1.22.12 linux/amd64` (built from source here).
Run three times consecutively for the limiter/eviction timing paths — stable.

`TestRBACCredentialsDeriveRoleAndWorkspace` and `TestRBACAuthorizationDecisions`
fail locally at `ensureIsolationRoles` because there is no PostgreSQL here.
That is environmental, not a regression.

## J. CI results

**Not run.** `DO NOT PUSH` was in force, so no workflow could be triggered.
Compensating verification performed instead:

- Both workflows parsed as valid YAML (Go `yaml.v3`, asserting jobs/steps/needs).
- Every executable Phase-1 step was extracted from the YAML and **run against
  this tree**: 15/15 passed, including the 18-principle check now matching full
  phrases.
- The new Phase-2 commands were checked by hand for shell correctness.
- CI-integrity assertions in §S.

## K. Security verification

- Deny-by-default authorisation: unchanged and re-read end to end. A request
  needs a matching rule **and** the role **and** the claim-carried permission
  **and** the workspace scope. `ScopePath` compares the captured path segment
  to `claims.WorkspaceID`; no client-supplied workspace is ever trusted.
- `validateClaimsShape` still rejects unsupported roles and founder/workspace
  pairings the database forbids.
- `RequireAuth` fails closed on a malformed bearer token (401) and only lets a
  header-less request through to the deny-by-default layer.
- `clientIP` uses `RemoteAddr` only — `X-Forwarded-For` is not trusted, so the
  rate-limit key cannot be spoofed.
- New: key-length floors, trailing-data rejection, bounded request/response
  paths, no committed signing secrets.

## L. RLS / database verification

- Authoritative schema is `infrastructure/database/database.go` (as
  `tests/schema_order_test.go` asserts). 13 tables; RLS enabled on 11;
  10 `workspace_isolation_policy` declarations — matching the counts the CI SQL
  step asserts.
- `memory_embeddings`: `vector(10)`, workspace-scoped FK, unique `memory_id`,
  isolation policy, and now an HNSW cosine index matching the `<=>` operator.
- **Verified as a genuine gap, not assumed:** `grep -rn "FORCE ROW LEVEL"`
  returns nothing anywhere in the repository, and there is no `SET ROLE` or
  `CREATE ROLE` in production code. The runtime therefore connects as the table
  owner, and PostgreSQL skips RLS for owners and superusers. See §U-1.
- Every production query path against `tasks`/`pipelines`/`publications`/
  `knowledge_documents` was confirmed to bind `app.current_workspace`
  transaction-locally before touching the table.

## M. Auth / RBAC verification

Rotation, reuse detection, logout revocation, expiry, wrong-secret, wrong
issuer/audience, unsupported role, inconsistent claims: all pass. Reuse
detection now actually revokes the chain (§C-1), proven by a test that rotates
`rt1 → rt2 → rt3`, replays `rt1`, and asserts `rt2` and `rt3` are both dead.

## N. Worker / RabbitMQ verification

Ack/nack classification proven against a fake `Acknowledger`: success → ack;
transient → nack with requeue; permanent → ack-and-drop with an error-level
log; undecodable body → ack (drained). Every delivery is settled exactly once.
Reconnect backoff is bounded at 30 s; `Qos(1)` bounds in-flight work.
Failed processing is **never** acked as success.

## O. Redis verification

Startup ping failure is fatal (fail-closed). Compose now requires
`AUSTRO_REDIS_PASSWORD` and runs `requirepass`; the port is loopback-only.
`IsPartitionedKey` fixed. No client-side timeouts are configured beyond the
library defaults — see §U-4.

## P. Audit verification

Hash chaining, genesis handling, tamper detection and the HMAC tag are all
verified by tests. **But the subsystem is not wired into the running
application** — see §U-2, the most important open item.

## Q. API / OpenAPI verification

10 registered routes; all 10 documented. The spec declares 18 operations, so 8
are documented-but-unimplemented (`/departments`, `/teams`, `/ai-employees`,
`/audit/events`, `/audit/events/{traceId}`, `/constitutional/principles`,
`/constitutional/principles/{name}`). `TestOpenAPIPrincipleReferences` pins the
count at exactly 18, so removing them would require weakening a test — left in
place and recorded in §U-3. The dangerous direction (implemented but
undocumented) is empty.

## R. Docker / Compose verification

Static review plus the fixes in §C-19…21, 26. `docker build` and
`docker compose config` could **not** be executed (no Docker); the compose file
was validated as YAML and every interpolation reviewed by hand.

## S. CI integrity verification

No `continue-on-error`, no `exit 0` masking. The only remaining `|| true` is
`gofmt -l . | grep -v ... || true`, where `grep -v` legitimately exits 1 when
it outputs nothing — that is the success path, and the step still fails when
the variable is non-empty. The Phase-1 SQL scanner is byte-for-byte unchanged,
including its single exclusion of `tests/phase1_exit_criteria_test.go`.

## T. Resource-safety verification

Request bodies 1 MiB (`MaxBytesReader`); refresh-store 65 536 records
(~13 MB) with expiry reclamation; rate-limiter 65 536 keys with timed sweep;
both evictions batched so the sweep is amortised O(1); worker concurrency
bounded by `Qos(1)`; reconnect backoff capped at 30 s; HTTP timeouts on every
axis; graceful drain capped at 75 s. Every bound has a stated justification in
a comment at its definition.

## U. Remaining limitations

1. **RLS is not enforced on the production connection.** The app connects as
   the table owner; PostgreSQL bypasses RLS for owners and superusers, and
   `FORCE ROW LEVEL SECURITY` appears nowhere in the repo. Application-level
   `WHERE workspace_id = …` guards are present on every query path as
   defence-in-depth, and the RLS integration tests pass because they create
   separate non-owner roles — which is exactly why the suite is green while the
   runtime is unprotected.
   *Why this was not changed blind:* blanket `FORCE` breaks the existing
   fixtures, which perform unbound admin seeding into `pipelines`,
   `publications`, `knowledge_documents`, `memory_embeddings` and `tasks`
   (`admin.Exec("INSERT INTO pipelines …")` with no `set_config`). With no
   PostgreSQL here, that is unverifiable. The correct fix is a role split
   (§V), not a one-line `FORCE`.
2. **The persistent audit subsystem is inert.** `grep -rn "austro-os/internal/audit"`
   matches only tests. `audit_events` is created and never written;
   authentication and memory audit go to the structured log via `auditAuth`
   and `logMemoryAudit`. The chain primitives are now cryptographically sound,
   but nothing produces a chain at runtime. Wiring this needs a durable store,
   restart-continuous chain state and transactional write guarantees — too
   large to ship without a database to verify against.
3. **8 phantom OpenAPI operations** (see §Q), pinned by a test asserting 18.
4. **No explicit Redis client timeouts or pool bounds** (`DialTimeout`,
   `ReadTimeout`, `PoolSize` rely on library defaults).
5. **No idempotency key on worker delivery.** `msg.MessageId` is logged but
   not de-duplicated, so a redelivered event can advance a pipeline twice.
6. **`memory_embeddings.memory_id` is globally unique**, not unique per
   workspace, which permits cross-workspace existence probing via a unique
   violation.
7. **Client-supplied `X-Trace-ID` / `X-Span-ID` are reflected into response
   headers** and propagated unvalidated into AMQP headers (the event envelope
   path does validate via `uuid.Parse`).

## V. Configuration-required items

- Provide `AUSTRO_JWT_SECRET`, `AUSTRO_JWT_REFRESH_SECRET`,
  `AUSTRO_FOUNDER_PASSWORD`, `AUSTRO_REDIS_PASSWORD`,
  `AUSTRO_POSTGRES_PASSWORD`, `AUSTRO_RABBITMQ_USER`,
  `AUSTRO_RABBITMQ_PASSWORD`. Compose now refuses to start without them.
- **Run the application as a dedicated database role that is not a superuser,
  has no `BYPASSRLS`, and does not own the tables**, or the workspace policies
  do not constrain it (§U-1). The new CI diagnostic prints `rolsuper`,
  `rolbypassrls`, and the RLS enabled/forced table counts on every run.
- JWT secrets must be ≥ 32 bytes; the founder password ≥ 16 bytes.
- `AUSTRO_PUBLISH_MAX_ATTEMPTS` and the two backoff settings must parse;
  a typo is now a startup failure rather than a silent default.

## W. Commits created (10, none pushed)

Nine engineering commits, each independently revertable, then this report as a
tenth:

```
67738ca Fix refresh-token family revocation, store race, and unbounded growth
eff2d04 Bind outcome, principle and timestamp into the audit chain; verify the HMAC tag
4f60080 Fail closed on short JWT secrets and malformed numeric settings
89bf782 Bound the HTTP listener, the rate limiter table, and JSON request parsing
ed371c6 Stop permanently-invalid events from wedging the worker queue
1516b48 Make RLS bootstrap fail closed, fix the dead users policy branch, drop the duplicate schema
b66f004 Format the tree with gofmt so the CI format gate can be enforced
ce30475 Remove the fake-green paths from CI and enforce what the steps claim to check
e7eb3a6 Harden the container runtime: real secrets, loopback-only dependencies, TLS roots
```

## X. Frozen gate byte identity

```
sha256      0a868d3da41f175cc263a6301f806e3bc1125093dab9cf8db98bb17b54e8769a   (matches)
git blob    5f478fdf3b1d4bdb7de1cf0b7d175f3699d99a0a                            (matches)
git diff 6e2ea73c7870d1bd8f028714e58ace1c3aa56c54..HEAD -- tests/phase1_exit_criteria_test.go
            → empty
```
`gofmt -w` was run with that file explicitly excluded and its hash re-checked
before and after.

## Y. `git diff --check`

Clean — no whitespace errors, no conflict markers (checked for both the
worktree and the index).

## Z. Working tree state

`git status --porcelain` → empty. Clean.

## AA. PUSHED = **NO**

10 commits ahead of `origin/main`; `git push` was never invoked.

---

## Release-gate summary

| Gate | Status |
|---|---|
| Build / vet / unit tests / race | **PASS** (executed locally) |
| Frozen Phase-1 gate unchanged | **PASS** (hash-verified) |
| `git diff --check` / clean tree | **PASS** |
| CI integrity (no fake-green) | **PASS** (static + steps executed locally) |
| Resource bounds | **PASS** |
| Secret handling | **PASS** |
| OpenAPI/runtime parity | **PARTIAL** — no undocumented routes; 8 declared-but-unimplemented (§U-3) |
| Fresh-DB bootstrap / RLS isolation / worker / pipeline / publishing / memory | **NOT EXECUTED HERE** — no PostgreSQL/Redis/RabbitMQ; must run in CI |
| Docker / Compose | **NOT EXECUTED HERE** — no Docker; static review only |
| RLS enforced on the runtime connection | **NOT SATISFIED** — §U-1, requires the role split in §V |
| Persistent audit records | **NOT SATISFIED** — §U-2 |

**Verdict: not yet a defensible release candidate.** The code-level defects
found are fixed and covered. Two architectural gaps (§U-1 runtime RLS
enforcement, §U-2 unwired persistent audit) are security-relevant and cannot be
closed safely from a sandbox with no database; both need CI/runtime evidence
before the repository is declared production-ready.
