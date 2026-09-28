# AUSTRO OS — Release Readiness Report

**Status: RELEASE READINESS INCOMPLETE — BLOCKERS REMAIN**

**Scope of this pass:** release-readiness review and remediation of the AI
execution transport contract, the real AI provider boundary, deployment
configuration coverage, and a reproducible release candidate.

**Hard constraints observed throughout:** no deployment, no connection to any
production or AlwaysData host, no paid provider call, no secret printed, copied
or committed, and no push.

**Date:** 2026-09-27
**Baseline commit (start of pass):** `224bb24089ccbf74dc08ce9c6b3831918c4157fb` (`main`)
**Release-candidate commit (end of pass):** `91e9c8c3b084e3a96ebc912c9132fa458e299c2c`
+ `995d07c66bfcf11fdd20e9491f685fa41940ad07`. Both local, neither pushed.

---

## 0. State reconciliation (Phase 1)

The "72 changed paths" figure reported earlier was wrong, and two of the
premises behind it needed correcting.

### The frozen "SHA" is not a commit

`0a868d3d…` and `9119aa71…` are **SHA-256 file digests**, not commit SHAs:

```
git cat-file -t 0a868d3d  ->  fatal: Not a valid object name
git log --all | grep 0a868d3d  ->  0 matches
```

The pin lives at `scripts/verify-production-deployment.sh:35` and covers
`tests/phase1_exit_criteria_test.go`. Hashing that file at every commit in
history showed `0a868d3d…` matched **all 297 commits** — the file had never been
modified. It is a source-integrity pin on a single architecture-constitution
test, not an architecture baseline and not a release commit.

### 72 dirty paths is really 56

`git status` reported 58 tracked modifications. Only **28** had any content
difference; the other **30** are `core.autocrlf` artifacts for which `git diff`
returns empty. Adding the untracked files gives the real count:

| Class | Count |
|---|---|
| Tracked with real content changes | 28 (5 of them already staged before this work began) |
| Tracked "modified" with zero content diff (CRLF/stat artifacts) | 30 |
| Untracked files | 28 (16 status entries; `internal/execution/` expands to 13) |
| **Committed in the release-candidate commit** | **56** |
| Deliberately excluded | `erl_crash.dump`, `specifications/Final_Technical_Specification.md`, the 30 CRLF artifacts |

56 rather than 28 + 28 because the 5 pre-staged AlwaysData files are counted
once, in the untracked column and the tracked column, not twice; and because
`.gitignore` picked up a 29th real tracked change after that measurement was
first taken. The committed figure is 24 modified + 32 new.

**The count was verified, not estimated.** An earlier estimate of "40 paths" was
wrong, which is why the final number comes from an explicit allowlist script
that prints every path, classifies it, and asserts the set staged in the index is
identical to the set reviewed. The script reported:

```
=== EXACT ALLOWLIST COUNT: 56 ===
=== anything staged that is NOT in the allowlist? ===
  none
=== exclusions still unstaged? ===
  correctly not staged: erl_crash.dump
  correctly not staged: specifications/Final_Technical_Specification.md
=== zero-diff artifacts present in worktree: 30
=== of those wrongly present in allowlist  : 0
```

### The execution subsystem was never in the repository

HEAD was extracted read-only (`git archive` → temp dir) and built: **exit 0**.
HEAD is a clean baseline containing **zero** references to `internal/execution`.

The entire subsystem was untracked — 13 files in `internal/execution/`, plus
`infrastructure/postgres/executions.go` (805 lines),
`infrastructure/rabbitmq/executions.go`, and
`internal/api/execution_handlers.go` — while the *tracked* wiring files
(`main.go`, `cmd/worker/main.go`, `internal/composition/composition.go`,
`internal/api/routes.go`) contain 4 references into it.

A commit containing only the tracked modifications therefore **would not
compile**. This is the fingerprint omission in its most serious form: not
merely unhashed, but load-bearing.

### Two artifacts excluded from the release commit

| File | Finding | Action |
|---|---|---|
| `erl_crash.dump` | 1.19 MB Erlang crash dump from an abnormal broker termination. Contains memory addresses and process state, and may contain connection strings from the node. Was **not** covered by `.gitignore`, so `git add .` would have committed it. | `erl_crash.dump` and `*.dump` added to `.gitignore`. File left on disk, not deleted. |
| `specifications/Final_Technical_Specification.md` | **20,695 bytes, every byte `0x00`.** Zero content, referenced nowhere. Committing a file with this name and no content would be actively misleading. | Left on disk, excluded from the commit. |

For contrast, the eight tracked `specifications/*.md` files are genuinely empty
in HEAD (0 bytes) — pre-existing placeholders, untouched by this work.

### Secret scan — clean

All 56 changed and untracked files were scanned for PEM private keys, AWS keys,
GitHub and Slack tokens, JWT literals, Bearer literals, API keys and tokens:
**zero hits**.

Credential-shaped assignments were triaged by class without printing any value:
40 placeholders, 52 initially flagged, all resolved as benign — Go test fixtures
of 6 characters that are **already present in HEAD**; `.env.example` values of
24–38 characters at entropy 3.7–4.0 (word-like, not random) that are **also all
already in HEAD**; and, in the new documents, masked DSN templates
(`host:...@`) and `<model>` placeholders.

**No real credentials are present in the working tree.**

---

## 0.1 Frozen SHA re-freeze (Phase 3, approved Option B)

The user approved re-freezing the pin, conditional on the change being only the
verified Windows path-separator fix. It is, and this was confirmed before
touching the pin:

```
git diff -U0 -- tests/phase1_exit_criteria_test.go
@@ -200 +200 @@
-			if strings.HasPrefix(filepath.ToSlash(path), root+"/.github") {
+			if strings.HasPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+"/.github") {
numstat: 1 added, 1 removed
```

One line, inside subtest 16, the scan that rejects references to V1-excluded
technologies. The banned-term list and all 32 other subtests are untouched.

**The defect, demonstrated rather than asserted.** `walkAllText` hands the
callback a native path; the callback normalised it with `filepath.ToSlash(path)`
but concatenated the root *without* normalising it. On Windows the comparison
was `C:/src/repo/.github/...` against `C:\src\repo/.github` — never equal, so the
intended exclusion silently failed and the banned-technology scan ran over the
CI workflows the exclusion exists to skip.

This report does not name the excluded technologies, because a committed
document is itself scanned by that subtest. Two workflows do name them, and
legitimately — they are the detectors that enforce the rule:

- `.github/workflows/phase1-exit-criteria.yml`
- `.github/workflows/production-deployment.yml`

This report was itself caught by that same scan during the staged-tree check
described in §0.2, which is a useful demonstration that the gate is live rather
than decorative.

---

## 0.2 The staged-tree check (Phase 2)

Before committing, the index was exported to a clean directory
(`git checkout-index -a --prefix=…`) and the **staged content alone** was built
and tested. This is a stronger check than testing the working tree, because the
working tree can satisfy a test through a file that was never staged.

| Check on the exported index | Result |
|---|---|
| Files exported | 377 |
| `go build ./...` | **exit 0** |
| `go vet ./...` | **exit 0** |
| `go test ./... -count=1` (first attempt) | **exit 1** — see below |
| After the fix below, `go test ./... -count=1` | **exit 0** — 26/26 packages |

The build and vet results confirm the self-containment claim in §0: with the
execution subsystem staged, the tracked wiring compiles. Had any of those
sources been left unstaged, the export would not have compiled at all.

The first test run failed on exactly one subtest. Reproduced verbatim it reads:

```
--- FAIL: TestPhase1ExitCriteria/16_<redacted-slug>_in_V1
    forbidden V1 technology "<redacted-term>" referenced in
    C:\...\rc-index\docs\RELEASE_READINESS_REPORT.md
```

The failing subtest is the one that rejects references to V1-excluded
technologies, and the offending file is this report. The excluded terms are
redacted above for the same reason the rest of the section is.

**The report written in the previous section violated the constitution it was
describing.** Explaining the exclusion defect required naming the excluded
technologies, and the subtest scans every `.md` file in the repository, so the
explanation disqualified the document. The section above was rewritten to
describe the mechanism without naming the terms.

This is a good result for the process rather than an embarrassment to hide: the
defect was found by the gate itself, on content that had already been reviewed by
hand and had passed the same test minutes earlier — because the earlier run tested
the working tree while the edit landed concurrently, and because the report had
not yet been written when the static verifier ran. Only testing the exact bytes to
be committed exposed it.

Two consequences are worth stating plainly:

- The pre-existing occurrences of these terms in `scripts/` and `tests/` are
  **not** violations. `walkAllText` skips both directories
  (`phase1_exit_criteria_test.go:75`), and the detector script necessarily names
  what it detects. Only `docs/` and other unscanned trees are in scope.
- The report must never name the excluded technologies, which is an
  awkward but real constraint on writing release documentation in this
  repository.

On Linux the root is already slash-separated, so the new form is a no-op and
the release platform's behaviour is unchanged.

**New pin, cross-checked by three independent implementations:**

```
sha256sum       -> 9119aa71aa1cd95eda7fa0aeabf14042a960770ce4ea1e3fc896f6f7ec4b0a67
openssl dgst    -> 9119aa71aa1cd95eda7fa0aeabf14042a960770ce4ea1e3fc896f6f7ec4b0a67
python hashlib  -> 9119aa71aa1cd95eda7fa0aeabf14042a960770ce4ea1e3fc896f6f7ec4b0a67
```

The previous digest and the full rationale are preserved as a decision-history
comment above `FROZEN_SHA` in the verification script, so the re-freeze is
auditable rather than silent.

### A regression guard, placed so the pin does not move again

`tests/phase1_path_prefix_test.go` (new, 6 tests) locks the corrected behaviour.
It deliberately lives in its own file: adding a test to the constitution file
would move the pin a second time.

It asserts that the corrected form matches a Windows-style root, that the
**pre-fix** form does not (pinning the regression itself), that the correction
is a no-op on POSIX roots, that the exclusion stays narrow across both root
styles, and that it cannot match a path outside the repository.

Writing it surfaced a **pre-existing** weakness, now documented rather than
silently left: the exclusion is a bare string prefix, so it also matches a
sibling directory whose name merely begins with `.github`, such as `.githubx`.
This is **not** a regression — the pre-fix form has the same boundary behaviour,
since both are string comparisons. It is recorded in
`TestGitHubExclusionKnownPrefixLimitation`, which asserts the current behaviour
so that a future tightening produces a deliberate failure. Tightening it would
require editing the byte-pinned file and therefore a further approved
re-freeze, so it was not done here. The gate is a repository-hygiene check, not
a trust boundary: anyone able to create a new top-level directory could equally
put the same text in a file the gate already scans.

---

## 1. The finding that drove this pass

The AI execution subsystem — dispatcher, worker, lease/fencing, retry and
recovery — was fully implemented and had been verified against a real RabbitMQ
broker in the previous production-verification pass. It was simultaneously
**absent from every deployment artifact in the repository**:

| Deployment artifact | `AUSTRO_EXECUTION_*` settings present before this pass |
|---|---|
| `docker-compose.production.yml` | none |
| `docker-compose.yml` (development) | none (correct — offline default) |
| `.env.example` | none |
| `deploy/alwaysdata/austro.env.alwaysdata` | none |
| `deploy/native/austro.env.example` | none |

The only places the subsystem appeared at all were two Markdown reports.

Consequences for a deployment built from those files, all of them silent:

- `AUSTRO_EXECUTION_TRANSPORT_BACKEND` defaulted to `stub`, so the dispatcher
  logged every claimed retry as delivered while nothing reached a broker.
- `AUSTRO_EXECUTION_WORKSPACE_IDS` was empty, so `main.go` skipped the dispatch
  loop entirely and logged `execution-dispatch-idle` — an informational line, at
  `info` level, with no health or readiness signal attached.
- Every operator-facing check still passed: `/health/live`, `/health/ready`, the
  worker container healthcheck, `scripts/healthcheck.sh`, `scripts/monitor.sh`.
  A deployment could have been in this state indefinitely without reporting a
  problem.

This is the shape of failure the release gate exists to catch, and it is why the
fix below is enforced in three independent layers rather than documented once.

---

## 2. Changes made in this pass

### 2.1 The execution queue name is now deployment configuration

`austro.executions` was a package constant (`rabbitmq.ExecutionQueue`) referenced
independently by the API's dispatcher and the worker's consumer. The two
processes are separate binaries that share no memory, so that string was the only
thing making them meet, and it could not be changed without editing two source
files — a rename applied to one side alone produces a dispatcher publishing into a
queue nothing consumes while both processes report healthy.

- New setting `AUSTRO_EXECUTION_QUEUE`, default `austro.executions`
  (`config.DefaultExecutionQueue`), read by both processes.
- `main.go` and `cmd/worker/main.go` now resolve the name from configuration.
- A blank or whitespace-only value is rejected at startup rather than defaulted at
  the point of use, where the two sides would each substitute a different value.
- The default literal is still written in two places, because `internal/config`
  cannot import `infrastructure/rabbitmq` without a cycle. That duplication is
  now pinned by `TestDefaultExecutionQueueMatchesConfigDefault` in the rabbitmq
  package, which may import config.
- `execution-dispatch-started` now logs the queue name, so a deployment can be
  confirmed from its own startup log.

### 2.2 Production refuses the deterministic stubs

Every stub adapter answers successfully without contacting a real service. A
deployment that reached production on the offline defaults would serve traffic,
answer `/health/ready`, and perform no real work — with nothing in the system
reporting it.

- When `AUSTRO_ENV` is `production` (case-insensitive, whitespace-trimmed), the
  AI, publishing and execution-transport backends may not be `stub`.
- The escape hatch is `AUSTRO_ALLOW_STUB_BACKENDS`, default `false`, for a
  deliberately offline production-shaped stack (demo, rehearsal, no-egress site).
  An unparseable value is **rejected**, not read as consent.
- The rule is scoped to the production environment, so the offline development
  default is untouched: a fresh clone still runs with no configuration, no
  account and no egress.
- Environment-label matching folds case and trims, so `Production` cannot step
  around a production-only rule.

### 2.3 A broker-backed dispatcher with nothing to sweep now fails to start

`AUSTRO_EXECUTION_TRANSPORT_BACKEND=rabbitmq` with an empty
`AUSTRO_EXECUTION_WORKSPACE_IDS` is a dispatcher that connects, logs
`execution-dispatch-started`, and never dispatches anything. In production that
is now a startup failure naming the variable. In development it remains a
deliberate no-op, and it is not required at all for the stub transport.

### 2.4 Deployment artifacts now declare the contract

- `docker-compose.production.yml` — the shared environment fragment now sets
  `AUSTRO_EXECUTION_TRANSPORT_BACKEND` (defaulting to `rabbitmq`, not the stub),
  `AUSTRO_EXECUTION_QUEUE`, `AUSTRO_EXECUTION_WORKSPACE_IDS` (passed through, not
  forced — the application is the authority on it, and forcing it here would make
  the documented offline exemption inexpressible) and `AUSTRO_ALLOW_STUB_BACKENDS`.
- `.env.example` — full execution subsystem contract documented, including the
  lease/heartbeat/window relationships and why a violation is a hard failure.
- `deploy/alwaysdata/austro.env.alwaysdata` and
  `deploy/native/austro.env.example` — both previously shipped **active**
  `AUSTRO_AI_BACKEND=stub` and `AUSTRO_PUBLISH_BACKEND=stub` alongside
  `AUSTRO_ENV=production`. Those lines are now commented out and replaced with
  the real-backend configuration, the execution transport block, and the
  exemption. Shipping an active stub there would have produced a template that
  cannot boot, and an operator "fixing" that by deleting the line would have
  fallen back to a stub that reports success while doing nothing.
- `deploy/alwaysdata/RUNBOOK.md` — troubleshooting now names both queues
  (`austro.events` and `austro.executions`) and the failure mode where the two
  processes disagree about the name.
- `docker-compose.yml` (development) is **unchanged by design**, and a test now
  asserts it stays that way.

### 2.5 Drift guards added so this cannot recur silently

- `scripts/verify-production-deployment.sh` — the configuration-contract coverage
  list now includes the execution settings, plus a new section asserting each
  production artifact declares them, that production compose defaults to
  `rabbitmq`, that the development compose stays on the offline default, and that
  no production template ships an active stub backend.
- `tests/release_configuration_contract_test.go` (new) — the same invariants as
  executable Go tests, so they run in an ordinary `go test ./...` and not only in
  the static-verification job. Includes the precise form of the rule that matters
  for `.env.example`: stubs and a production claim may never coexist in the same
  file, which is what makes it safe for the general example to keep its stubs.
- `internal/ai/live_provider_test.go` (new) — the opt-in live provider smoke test
  (see §4).
- `docs/production-configuration.md` — previously documented no execution or
  stub-exemption setting at all. It now carries an *Execution transport* section
  (all nine `AUSTRO_EXECUTION_*` variables with defaults and classes, the
  cross-field duration rules, the UUID requirement on workspace IDs, and how to
  confirm from logs that dispatch is actually live), a *production stub refusal*
  section, and entries in the "must never reach production" table. `AUSTRO_RABBITMQ_QUEUE`
  is now explicitly labelled as the event queue so it cannot be mistaken for the
  execution queue.

---

## 3. Verification performed

All of the following was executed in this pass. Nothing is carried over as
assurance without a stated basis.

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass |
| Full suite, live dependencies, `-count=1` (no cache) | **pass** — exit 0, 26/26 test packages `ok`, 0 failures |
| `go test ./internal/config/... ./infrastructure/rabbitmq/... ./internal/ai/...` (verbose) | 82 pass, 0 fail, 1 skip — includes all 13 new release-gate tests (12 config, 1 queue pin) |
| `go test ./tests/ -run <deployment contract tests>` (5 new) | pass |
| `internal/ai` live provider test without opt-in | **skips** (correct — no external call attempted) |
| `scripts/verify-production-deployment.sh` | 188 passed, 1 failed (B4 only; see §6) |
| `bash -n` syntax check on the verification script | pass |
| Compose YAML structurally linted (with a passing negative control) | pass — the new execution block is machine-validated |
| Release candidate cross-compile, `linux/amd64` + `linux/arm64` | pass |
| Candidate artifacts re-hashed against the manifest | 4/4 match, byte counts agree |
| API + worker rebuilt and restarted against the live stack | pass — `/health/live` 200, `/health/ready` 200 |
| Startup log confirms config-driven queue | pass — `execution-dispatch-started` with `"queue":"austro.executions","transport":"rabbitmq","workspaces":2` |

### Test fixtures corrected

Two live tests and the config unit fixture built `config.Config` struct literals
directly, bypassing the loader, so the new queue field was a zero value:

- `internal/config/config_test.go` — `secureCore()` now states the queue name.
- `tests/config_test.go`, `tests/audit_persistence_test.go` — same.

These are fixture corrections, not production defects: the production path always
goes through `defaults()`, which supplies the default. A hand-built `Config` with
a blank queue is precisely the condition the validation exists to catch.

One test expectation was a genuine invariant, not a fixture problem, and was
preserved: selecting a stub while supplying an AI/publish credential is rejected
("a configured key must never sit ignored in the environment"). The offline
production test now clears the credentials as well as selecting the stubs, which
is the procedure an operator actually follows.

---

## 4. AI provider boundary

### Inventory

| Backend | Selector | Required | Key required | Production status |
|---|---|---|---|---|
| Offline deterministic | `AUSTRO_AI_BACKEND=stub` | — | — | **refused** unless exempted |
| Local / keyless OpenAI-compatible | `local` | model, base URL | no | allowed |
| Real OpenAI-compatible HTTP | `openai-compatible` | model, base URL, API key | yes | allowed |

Publishing has the same shape (`stub` / `generic-http`), and the execution
transport has the same shape (`stub` / `rabbitmq`). All three are now subject to
the production refusal.

### Offline coverage — VERIFIED

The real HTTP provider path is covered without any external call:
`internal/ai/openai_compatible_test.go` drives both real constructors against
`httptest` servers and asserts request shape, the `Authorization` header (present
for `openai-compatible`, **absent** for a keyless local endpoint), optional-key
handling, empty-`choices` handling, oversized-response refusal, and that an error
never carries the key. `internal/ai/live_provider_test.go` adds a
credential-leak assertion on the configuration-error path.

### Live provider — NOT VERIFIED

`TestLiveProviderCompletion` performs one minimal real completion. It is off by
default, skips unless `AUSTRO_AI_LIVE_TEST=1` is set alongside a real backend and
credential, and **fails loudly if opted in against a stub** (a stub would "pass"
without contacting anything). It has **not been run**, because no credentials were
provided and a real call may be billed.

The honest statement for this release: the real provider is unproven against any
live endpoint. Offline tests prove the code we wrote; they cannot prove that a
vendor's endpoint accepts our request, that a credential works, or that a model
name is spelled the way an account expects.

**To clear it** (operator action, requires credential + approval for a billed
call):

```
AUSTRO_AI_LIVE_TEST=1 \
AUSTRO_AI_BACKEND=openai-compatible \
AUSTRO_AI_BASE_URL=https://<endpoint>/v1 \
AUSTRO_AI_MODEL=<model> \
AUSTRO_AI_API_KEY=<key> \
go test ./internal/ai/ -run TestLiveProvider -v
```

The same gap applies to the `generic-http` publishing backend, which has no
live-path test.

---

## 5. Release candidate

**Source: commit `995d07c66bfcf11fdd20e9491f685fa41940ad07`, tree
`8cecea10e2e5685a050a2713c1e07244c152520e`.** The source was materialised with
`git archive <commit>` into an empty directory, so the build cannot be influenced
by the working directory, by untracked files, or by `.gitignore`.

Built with the repository's own convention, matching
`.github/workflows/native-arm64-deploy.yml:60-61`:
`-trimpath -ldflags '-s -w'`, `CGO_ENABLED=0`. Binary names match what each
install script expects (`austro-api`/`austro-worker`).

Location: `C:\Users\AUSTRO\AppData\Local\Temp\opencode\austro-release-candidate\`
(manifest: `release-manifest.json`)

| Target | File | Bytes | SHA-256 |
|---|---|---|---|
| linux/amd64 | `austro-api` | 13,963,426 | `99dab4e2c7c731a578fa17434b8236224c87779f9e4ea4579dccb7d04c64515d` |
| linux/amd64 | `austro-worker` | 12,001,442 | `9dcd4b86b9ac2b482fbdff778e9ed2b00b993efbb9f242c6f559ad345b3360d2` |
| linux/arm64 | `austro-api` | 13,107,362 | `e07a5cb6d827d3d41d21a4cf7a1f82c3f4319463d7663f0e790421220f31d69b` |
| linux/arm64 | `austro-worker` | 11,337,890 | `a3b82b0fb642416fd94c0540bc389bfe7a1cbecd04414916ea1e22d82cc1d4af` |

### Reproducibility — VERIFIED, not assumed

The candidate was built **twice** from the same commit into separate output
trees. All four artifacts are byte-for-byte identical:

```
amd64   main    REPRODUCIBLE  99dab4e2c7c731a578fa17434b8236224c87779f9e4ea4579dccb7d04c64515d
amd64   worker  REPRODUCIBLE  9dcd4b86b9ac2b482fbdff778e9ed2b00b993efbb9f242c6f559ad345b3360d2
arm64   main    REPRODUCIBLE  e07a5cb6d827d3d41d21a4cf7a1f82c3f4319463d7663f0e790421220f31d69b
arm64   worker  REPRODUCIBLE  a3b82b0fb642416fd94c0540bc389bfe7a1cbecd04414916ea1e22d82cc1d4af
```

`-trimpath` is what makes this hold: it removes local filesystem paths from the
binary, so the build no longer encodes the directory it happened to run in. The
digests were then re-hashed from the delivered files and matched the manifest
again, and the manifest's `git_commit` was checked against the repository HEAD.

The earlier candidate is superseded. Its `linux/amd64` `austro-api` digest was
`985f0856…` against `99dab4e2…` now, at an identical byte count. The difference
is not noise: that build was made from a working tree whose execution subsystem
was **untracked**, so it was compiled from sources that the fingerprint did not
describe. Same size, different code.

### What remains true of any candidate

**No binary embeds its build identity.** The repository exposes no version
variable and no `-X` ldflags target, so nothing inside the artifact or in
`/health` reports which commit produced it. Identifying a running deployment
requires this manifest. Adding a `version` variable populated by ldflags is the
natural follow-up; it was not added here because it is new source and would
require re-running the whole verification chain.

Reproducibility is guaranteed **for this toolchain** (`go1.26.8`) and these
flags. A different Go release may produce different bytes, so the toolchain
version is recorded in the manifest alongside the digests.

### Build hygiene

`erl_crash.dump` and `specifications/Final_Technical_Specification.md` are
absent from the commit and from the extracted build source; both were confirmed
absent with `git cat-file` rather than assumed.

---

## 6. Blockers

Ordered by what most blocks a release decision.

### B1 — Real AI provider unverified against a live endpoint
**Severity: high (if the AI subsystem is in the release scope)**
Offline coverage is strong; no live call has been made. Needs a credential and
approval for a billed call, then the command in §4.

### B2 — AlwaysData deployment feasibility unverified
**Severity: high for that target**
`deploy/alwaysdata/RUNBOOK.md` assumes AlwaysData's Service Manager can run the
API *and* a separate `austro-worker` process with auto-restart. The worker is a
RabbitMQ consumer that serves no HTTP.

This is an unverified assumption about a third-party platform, and it runs against
the repository's own documented position. `docs/deployment-targets.md` lists
supported targets 1–6 (Docker Compose, cloud VM, air-gapped, behind a load
balancer, external managed PostgreSQL, native ARM64 systemd) and **AlwaysData is
not among them**. It states that "Serverless / PaaS function hosts" are **NOT
SUPPORTED**, because "the API and worker are long-running processes with
in-process connection pools, a RabbitMQ consumer and a graceful drain."

The PostgreSQL side of the AlwaysData path is sound and is a genuine capability:
pre-provisioned roles (`AUSTRO_POSTGRES_PREPROVISIONED_ROLES=true`) let the
application run where it cannot `CREATE ROLE`. The unresolved question is
specifically whether the platform will host a non-HTTP long-running consumer.

**Needs:** confirmation from AlwaysData's current documentation or a real account
that a supervised, non-HTTP, always-on worker process is permitted. Until then,
AlwaysData should be treated as an **undocumented, unproven target**, and targets
1/2/6 are the supported ones.

### B3 — Docker never available: Compose is statically verified only
**Severity: medium**
No Docker daemon in this environment. `docker compose config` was never run and
the stack was never brought up from the compose files. The changes to
`docker-compose.production.yml` are verified by reading, by the static gate, by
the Go contract test, and by the YAML linter (§B6) — but **not by execution**.
`docs/deployment-targets.md` records the same limitation and notes that CI's
`production-smoke` and `production-rehearsal` jobs do exercise the real stack,
including a backup and restore cycle. Those CI claims could not be observed from
here.

A lint proves the file is well-formed YAML with the expected structure. It cannot
prove the service graph actually starts, that images resolve, that health checks
pass, or that the migration runs. That gap is the whole of B3.

**Needs:** one `docker compose -f docker-compose.production.yml config` run, and
one `scripts/deploy.sh` + `scripts/healthcheck.sh` run on a real host.

### B4 — RESOLVED: frozen gate re-frozen under approval (Option B)
**Severity: none (closed in this pass)**

The mismatch was real:

```
FAIL  FROZEN GATE MODIFIED: expected 0a868d3da41f175cc263a6301f806e3bc1125093dab9cf8db98bb17b54e8769a
                            got      9119aa71aa1cd95eda7fa0aeabf14042a960770ce4ea1e3fc896f6f7ec4b0a67
```

`scripts/verify-production-deployment.sh` pins the SHA-256 of
`tests/phase1_exit_criteria_test.go`, correct at commit `224bb240` and at every
one of the 297 commits in history. The file was modified by a **previous**
session with a one-line Windows path fix.

**This was deliberately left failing until the evidence was reported and
approved.** Rewriting the pin silently would have made the check pass while
destroying its purpose — the gate exists to make an unapproved change to the
exit-criteria test impossible to miss. The user approved Option B after
reviewing the demonstration in §0.1.

Resolution: the pin is now `9119aa71…`, the previous digest and rationale are
preserved as a decision-history comment, a regression guard was added in a
separate file so the pin does not move again, and the gate is green.

**Static verification is now 189 passed, 0 failed.**

### B5 — RESOLVED: the release candidate is commit-reproducible
**Severity: none (closed in this pass)**

The work is committed, so a release candidate can be built from a reviewed
commit rather than an untracked working directory. See §0 for the inventory, the
excluded artifacts, and the demonstration that the untracked execution sources
were load-bearing for compilation.

The commit's own SHA cannot appear inside itself, so the authoritative identifier
for a rebuilt candidate is the manifest, which records the commit SHA alongside
the artifact digests.

### B6 — RESOLVED: the three "python3 unavailable" blocks were a false negative
**Severity: none (cleared in this pass)**

`scripts/verify-production-deployment.sh` probes for `python3` and reported three
checks as BLOCKED:

```
FAIL  YAML structural lint could not run (python3 unavailable) — BLOCKED
FAIL  workflow lint could not run (python3 unavailable) — BLOCKED
FAIL  operator-script variable audit could not run (python3 unavailable) — BLOCKED
```

This was **not** a missing dependency. This machine has Python 3.14.7 installed at
`C:\Python314\python.exe`; it is simply not exposed under the name `python3` in Git
Bash. The gate probes by name, so it concluded Python was absent.

Running the script with a temporary `python3` shim on `PATH` (a file in the
scratch directory, not a change to the repository or the system) executes all
three. They **pass**:

- `production compose parses as structurally well-formed YAML`
- `CI workflow parses as structurally well-formed YAML`
- `operator-script variable audit` — passes

The verification script also runs a negative control, which passed:
`compose linter rejects a deliberately broken file (negative control)`. That
matters, because a linter that accepts anything would also "pass" the real file;
the control proves the check discriminates. **The new execution-transport block in
`docker-compose.production.yml` is therefore machine-validated, not merely
read over.**

**Static verification after the approved re-freeze: 189 passed, 0 failed.**

The naming sensitivity itself is worth fixing at some point so this does not
mislead the next person: a `python3`-only probe reports a hard BLOCKED on a
machine that has Python. That is a latent reporting bug in the gate, not in the
code under test.

---

## 7. Per-phase status

| Phase | Status | Note |
|---|---|---|
| 1. Baseline: transport, providers, deployment | **DONE** | §0, §1; the deployment-layer gap and the untracked-subsystem finding were the headline results |
| 2. Explicit RabbitMQ transport configuration | **DONE** | §2.1–§2.4, §0.2; staged tree builds, vets and tests clean on its own |
| 3. Real AI provider configuration and smoke test | **PARTIAL** | Offline path verified; live path implemented but not run — B1 |
| 4. Target deployment environment verification | **PARTIAL** | Native Windows stack re-verified; Compose never executed (B3); AlwaysData feasibility unresolved (B2) |
| 5. Secrets handling | **DONE** | §0; no secret printed, copied or committed; no new secret introduced; validation never echoes values |
| 6. Smoke test of the built artifact | **PARTIAL** | Binaries rebuilt and restarted live, health 200, queue logged from config. Not a release-mode smoke test — the local run is the offline development configuration by design |
| 7. Migration and rollback | **DONE (inherited, re-checked)** | Bootstrap is idempotent, applied at startup by the owner DSN; worker is gated on API health so migrations apply once. Native path has atomic symlink switch + rollback; Compose rollback is redeploy of the previous image tag |
| 8. Test suite regression | **DONE** | §0.2; full live suite, `-count=1`, exit 0, 26/26 packages, run against the exported index as well as the working tree |
| 9. Release candidate | **DONE** | §5; 4 artifacts built twice from a commit, byte-identical, independently re-hashed |
| 10. This report | **DONE** | — |

---

## 8. What was deliberately not done

- No deployment, and no connection to any production, staging or AlwaysData host.
- No push. Two commits are local only.
- No paid or external provider call of any kind.
- No secret value read, displayed, copied into a file, or committed.
- No `git reset`, `git checkout`, `git clean` or `stash`. The pre-existing dirty
  tree was preserved; only the reviewed 56 paths were staged, by explicit path.
- No edit to the banned-term list in the frozen-gate test, and no tightening of
  the `.githubx` prefix behaviour, since either would need a further approved
  re-freeze.
- No `git add .` and no directory-level staging at any point.
- No CI run; CI results could not be observed from this environment. The claim in
  `deploy/alwaysdata/RELEASE.md` that "all CI workflows pass" is inherited from
  earlier work and is **not** verified by this pass.

---

## 9. To reach `RELEASE CANDIDATE READY — VERIFIED SCOPE`

B4 and B5 are closed. The remaining work is external, and none of it can be
settled from this environment:

1. **B1** — run the live provider test (§4) with a real credential, obtained
   securely and with approval for a possibly billable request, and add the
   equivalent for the `generic-http` publishing backend.
2. **B2** — obtain authoritative AlwaysData documentation or account evidence
   that a supervised, persistent, non-HTTP worker is supported. If that cannot
   be obtained, drop AlwaysData from the release scope and target 1/2/6, which
   are the documented supported targets.
3. **B3** — run `docker compose -f docker-compose.production.yml config` and a
   real Compose bring-up on a machine with a Docker engine.
4. Add a `version` variable populated via ldflags so a binary reports its own
   provenance instead of depending on this manifest (§5).

Items 1–3 are operator actions on real infrastructure. Nothing in this pass can
substitute for them, and no amount of local test coverage would. Item 4 is a
small code change that would close the last self-inflicted gap in the candidate
chain.

---

## 10. Final state

**Status: RELEASE READINESS INCOMPLETE — BLOCKERS REMAIN**

| | |
|---|---|
| Release-candidate commit | `91e9c8c3b084e3a96ebc912c9132fa458e299c2c` (56 files) + `995d07c66bfcf11fdd20e9491f685fa41940ad07` (evidence follow-up) |
| Built from | `995d07c66bfcf11fdd20e9491f685fa41940ad07`, tree `8cecea10e2e5685a050a2713c1e07244c152520e` |
| Artifacts | 4 (linux/amd64, linux/arm64 × api, worker), byte-reproducible across two builds |
| Frozen architecture gate | re-frozen to `9119aa71…` under approval; static verifier 189/0 |
| Staged-tree verification | build 0, vet 0, tests 0 (26/26 packages) |
| Pushed | **No** |
| Deployed | **No** |
| Open blockers | B1 live AI provider, B2 AlwaysData worker topology, B3 Docker/Compose unexecuted |

The code is in a state where a release decision can be made. The three remaining
blockers are all about *external facts that cannot be established from this
environment* — a live provider response, a hosting platform's actual capabilities,
and a working Docker engine. Each is stated with what evidence would close it
rather than left as a vague "unverified".
