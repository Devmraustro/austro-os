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
**Release-candidate commit:** `91e9c8c3b084e3a96ebc912c9132fa458e299c2c`
(56 reviewed paths), followed by two documentation-only commits. All local,
none pushed.

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

### Build identity pass

| Check | Result |
|---|---|
| `go build ./...`, `go vet ./...` after the identity change | pass, exit 0 |
| `go test ./internal/buildinfo -count=1` | pass, exit 0 |
| Build identity contract suite | 43 pass, 0 fail, 2 skip (manifest tests skip until a manifest exists) |
| `scripts/verify-production-deployment.sh` after the change | **199 passed, 0 failed** (up from 189) |
| Dockerfile commit validation, executed against 10 inputs | only a full 40-char lowercase SHA and the literal `unknown` accepted; branch name, `HEAD`, short SHA, empty, uppercase, trailing newline, non-hex and over-length all rejected |
| `grep -a` image assertion, run against a real built binary | finds the linked commit; does not match a control SHA |
| `bash -n` on both changed scripts | pass |
| Workflow and compose YAML parse | pass, all five files |
| Production compose build args resolve for both services | pass — single anchor, no drift possible |
| Two independent clones, four artifacts | **byte-for-byte identical** |
| Artifact inspection via `debug/elf` | 4/4 ELF64, `ET_EXEC`, correct machine, no `PT_INTERP` |
| Embedded commit, with a control that must not match | 4/4 present, 4/4 control absent — the check discriminates |
| Unstamped-claim check | 4/4 contain neither `commit=unknown` nor `commit=dev` |
| Executed `--version`, all three spellings, both entrypoints | reports the expected version and source commit |
| Executed unlinked control build | reports `version=dev commit=unknown` with the note, so the check discriminates |
| Manifest digests and sizes re-hashed from disk | 4/4 match; four distinct digests |
| Architecture gate static subtests | pass |
| Architecture gate live subtests, full live regression suite | **not run** — no PostgreSQL, Redis, RabbitMQ or running API on this host |
| Shipped Linux binaries executed | **not possible** — no qemu on this host; see §5 |

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

**Build source commit: `af59c4efc06c162790c171bca38c9b3cfe7685ba`**
(`feat(release): give every released binary a verifiable build identity`),
tree `94256cb0fd9313ceb0a8231a6c6562dece895ac8`.

That commit is the authority for what was compiled. The commit that carries
*this report* is necessarily later and is not the build source; the two are
deliberately distinguished rather than conflated. Per-build provenance that
cannot live inside a reproducible binary - digests, toolchain, build time - is
in `release-manifest.json`, which lives outside the repository because a file
cannot contain the SHA of the commit that contains it.

Each of the two builds ran in its **own clone of that commit**, created with
`--no-hardlinks` so the object stores are separate copies rather than a shared
view. Each clone's tracked tree was asserted clean before building, and
`scripts/build-release.sh` independently refuses a tracked-dirty tree. The
binaries therefore cannot claim a commit that does not describe their source.

Built with the repository's own flags: `-trimpath -ldflags '-s -w'` plus the
linked identity, `CGO_ENABLED=0`, via `scripts/build-release.sh` - the single
definition the native ARM64 job calls. Binary names match what each install
script expects (`austro-api`/`austro-worker`).

Location: `C:\Users\AUSTRO\AppData\Local\Temp\opencode\austro-release-candidate\`
(manifest: `release-manifest.json`)

| Target | File | Bytes | SHA-256 |
|---|---|---|---|
| linux/amd64 | `austro-api` | 13,963,426 | `6427c42919b4d977cdf9d02b1072fddce6a710f79bef4275b82e527217efd03c` |
| linux/amd64 | `austro-worker` | 12,001,442 | `0db93c6b3eebab1c8b0e4d378951e3bfcb4529317a261e5f2d6c4dbf6d1366c9` |
| linux/arm64 | `austro-api` | 13,107,362 | `c2a0994b1b32d13cf21bed997e3fb4692956fa31a68ff6658c222401a287d29a` |
| linux/arm64 | `austro-worker` | 11,337,890 | `56a3dc80545adfc3e915fb42512f612ce6ba850c61cad2ce02d2950204534bdf` |

### Build identity — VERIFIED, and this closes the gap the last candidate had

The previous candidate carried a standing admission: *"No binary embeds its
build identity... Identifying a running deployment requires this manifest."* That
is now false, deliberately. A released binary names its own source:

```
austro-os api    version=1.0 commit=af59c4efc06c162790c171bca38c9b3cfe7685ba
austro-os worker version=1.0 commit=af59c4efc06c162790c171bca38c9b3cfe7685ba
```

`--version`, `-version` and `version` are all accepted on both entrypoints, and
the command is answered **before** configuration is read. That ordering is the
point: a host whose environment is broken is exactly where someone needs to
identify the binary, so identifying it must not require a working environment.
An unstamped build is distinguishable rather than plausible:

```
austro-os api version=dev commit=unknown
note: development build; no release metadata was linked in at build time
```

Every release build path stamps identity, and each is covered by a test that
reads the build definition itself - a release build that quietly drops its
`-X` flags still compiles, still passes every functional test, and still
produces four working binaries, so nothing else would fail until someone needed
the answer:

| Path | How identity gets there |
|---|---|
| `scripts/build-release.sh` | the single definition; refuses a tracked-dirty tree |
| `native-arm64-deploy.yml` | calls that script, so it cannot drift from it |
| `Dockerfile` | links it, validates the commit, fails the build if the binaries lack it |
| `docker-compose.production.yml` | requires `AUSTRO_BUILD_COMMIT`; compose aborts rather than defaulting |
| `production-deployment.yml` | passes `GITHUB_SHA`, asserts label and binaries agree |
| `phase2-ci.yml` | links it into the uploaded x86_64 artifact and asserts both binaries report it |

### Defects found while wiring this up

Four things were wrong before they were right, and each is now covered by a test:

- **The image's revision label would have been empty.** An `ARG` is scoped to the
  stage that declares it, and `org.opencontainers.image.revision` is written in
  the runtime stage, which declared no `ARG` of its own. The image would have
  claimed `revision=""` while its binaries correctly reported a commit - the
  exact disagreement this work exists to prevent, reintroduced through the
  labels.
- **Neither release path passed a commit to the image build.** Both would have
  shipped `commit=unknown`. The production compose file now requires the value
  with compose's `:?` form, and CI passes the commit it checked out.
- **An empty commit would have passed the image's own check vacuously**, because
  `grep -F ""` matches every file. The Dockerfile now refuses anything that is
  neither a full 40-character lowercase SHA nor the literal `unknown`, so a
  branch name, a short SHA or an empty value fails the build instead of producing
  an image that looks stamped and cannot be traced.
- **A comment block sat inside a continued `RUN`**, which is a parse hazard:
  comment lines are removed before instructions execute, so it either broke the
  build or truncated the instruction.

One check was deliberately **not** added. The Dockerfile asserts the commit but
not the version, because grepping a Go binary for a short token like `1.0`
matches almost anything and can never fail - worse than no check, since it reads
as assurance. The version is asserted where it discriminates: by executing
`--version`.

### Reproducibility — VERIFIED, not assumed

All four artifacts are byte-for-byte identical across the two independent builds:

```
amd64   austro-api     REPRODUCIBLE  6427c42919b4d977cdf9d02b1072fddce6a710f79bef4275b82e527217efd03c
amd64   austro-worker  REPRODUCIBLE  0db93c6b3eebab1c8b0e4d378951e3bfcb4529317a261e5f2d6c4dbf6d1366c9
arm64   austro-api     REPRODUCIBLE  c2a0994b1b32d13cf21bed997e3fb4692956fa31a68ff6658c222401a287d29a
arm64   austro-worker  REPRODUCIBLE  56a3dc80545adfc3e915fb42512f612ce6ba850c61cad2ce02d2950204534bdf
```

`-trimpath` is what makes this hold: it removes local filesystem paths, so the
build no longer encodes the directory it ran in. **No build date is linked into
any artifact** - a timestamp would make two builds of one commit differ and
destroy exactly this property. Per-build timing lives in the manifest instead.

The digests were re-hashed from the delivered files and matched the manifest
again, and the manifest was checked against the repository.

### What remains true of any candidate

- **The shipped Linux binaries were not executed on this host.** It is
  `windows/amd64` with no qemu, so the `linux/amd64` and `linux/arm64` artifacts
  could not be run. Their identity is established at byte level: each is ELF64,
  `ET_EXEC`, the expected `EM_X86_64`/`EM_AARCH64` machine, carries no
  `PT_INTERP` segment, contains the exact 40-character commit, contains no
  control SHA, and contains neither `commit=unknown` nor `commit=dev`. The
  identity *mechanism* was executed: binaries built from the same commit with the
  same flags for a natively runnable target printed the expected version and
  commit for all three argument spellings on both entrypoints, while an unlinked
  control printed `version=dev commit=unknown` with the note. So a byte-level
  match plus an executed equivalent is the strongest evidence this host can
  produce - not direct execution of the released artifact. CI runs the same
  assertion against the artifacts it builds and uploads, and
  `production-deployment.yml` executes `--version` inside the image.
- Reproducibility is guaranteed **for this toolchain** (`go1.26.8`) and these
  flags. A different Go release may produce different bytes, so the toolchain is
  recorded in the manifest alongside the digests.
- The previous candidate is superseded. Every digest changed, because every
  binary now carries its own identity. The byte counts are unchanged, which is
  expected: `-s -w` was already stripping the symbol table, and the identity
  strings replace comparable bytes.

### Build hygiene

`erl_crash.dump` and `specifications/Final_Technical_Specification.md` are
absent from the commit and from every build clone; both were confirmed absent
with `git cat-file` rather than assumed. A test asserts they stay out of the
release commit.

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

**After the build identity change: 199 passed, 0 failed.** Ten checks were added
for identity in the image build, and the three `python3` BLOCKED false negatives
were cleared by running the verifier on a host that provides `python3`, which is
the harness fix the note below describes.

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
| 9. Release candidate | **DONE** | §5; 4 artifacts built twice from a commit, in independent clones, byte-identical, independently re-hashed, and each carrying its own source commit |
| 10. This report | **DONE** | — |

---

## 8. What was deliberately not done

- No deployment, and no connection to any production, staging or AlwaysData host.
- No push. All commits are local only.
- No paid or external provider call of any kind.
- No secret value read, displayed, copied into a file, or committed.
- No `git reset`, `git checkout`, `git clean` or `stash`. The pre-existing dirty
  tree was preserved; only the reviewed paths were staged, by explicit path. The
  30 line-ending-only entries in `tests/` were left untouched: they differ from
  the committed blobs only in CRLF, and "fixing" them would have put unrelated
  line-ending churn into this commit. The release builds therefore ran in
  independent clones rather than in the working directory, which sidesteps the
  question entirely and is the stronger option anyway.
- No edit to the banned-term list in the frozen-gate test, and no tightening of
  the `.githubx` prefix behaviour, since either would need a further approved
  re-freeze.
- No `git add .` and no directory-level staging at any point.
- No CI run; CI results could not be observed from this environment. The claim in
  `deploy/alwaysdata/RELEASE.md` that "all CI workflows pass" is inherited from
  earlier work and is **not** verified by this pass.
- The shipped Linux binaries were not executed. No qemu is available on this
  host, so `linux/amd64` and `linux/arm64` identity is established by byte-level
  inspection plus an executed native build from the same commit, not by running
  the artifact itself. Stating this plainly matters: it is the one place where
  the evidence is a strong proxy rather than the direct observation.
- No image was built. There is no Docker engine here, so the Dockerfile's own
  validation and label assertions are unexecuted; only their static shape is
  checked. CI runs them for real.

---

## 9. To reach `RELEASE CANDIDATE READY — VERIFIED SCOPE`

B4, B5 and the build identity gap are closed. The remaining work is external,
and none of it can be settled from this environment:

1. **B1** — run the live provider test (§4) with a real credential, obtained
   securely and with approval for a possibly billable request, and add the
   equivalent for the `generic-http` publishing backend.
2. **B2** — obtain authoritative AlwaysData documentation or account evidence
   that a supervised, persistent, non-HTTP worker is supported. If that cannot
   be obtained, drop AlwaysData from the release scope and target 1/2/6, which
   are the documented supported targets.
3. **B3** — run `docker compose -f docker-compose.production.yml config` and a
   real Compose bring-up on a machine with a Docker engine, exporting
   `AUSTRO_BUILD_COMMIT` and `AUSTRO_BUILD_VERSION` first so the image is
   stamped.
4. Run the `linux/amd64` and `linux/arm64` artifacts' `--version` on a Linux
   host, or under qemu, to close the proxy described in §8. CI already does this
   for the artifacts it builds; this is about closing it for *these* artifacts.

Items 1–4 are operator actions on real infrastructure. Nothing in a local pass
can substitute for them, and no amount of local test coverage would.

---

## 10. Final state

**Status: RELEASE READINESS INCOMPLETE — BLOCKERS REMAIN**

| | |
|---|---|
| Build source commit | `af59c4efc06c162790c171bca38c9b3cfe7685ba` — tree `94256cb0fd9313ceb0a8231a6c6562dece895ac8` |
| Commit carrying this report | necessarily later, and deliberately not the build source; a file cannot contain the SHA of the commit that contains it |
| Artifacts | 4 (linux/amd64, linux/arm64 × api, worker), byte-reproducible across two independent builds, each carrying `version=1.0` and its own source commit |
| Build identity | embedded in every released binary, verified; see §5 |
| Per-build provenance | `release-manifest.json` (digests, toolchain, build time), written outside the repository |
| Prior release-candidate commit | `91e9c8c3b084e3a96ebc912c9132fa458e299c2c` — the 56 reviewed paths |
| Follow-ups | documentation-only commits, which change no artifact byte; plus the build identity commit `af59c4e`, which does, and is the build source above |
| Frozen architecture gate | re-frozen to `9119aa71…` under approval; static verifier 199/0 |
| Staged-tree verification | build 0, vet 0, buildinfo tests 0, identity contract suite 43 pass / 0 fail / 2 skip |
| Pushed | **No** |
| Deployed | **No** |
| Open blockers | B1 live AI provider, B2 AlwaysData worker topology, B3 Docker/Compose unexecuted |

The code is in a state where a release decision can be made. The three remaining
blockers are all about *external facts that cannot be established from this
environment* — a live provider response, a hosting platform's actual capabilities,
and a working Docker engine. Each is stated with what evidence would close it
rather than left as a vague "unverified". The one self-inflicted gap in the
candidate chain — binaries that could not identify themselves — is closed.
