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

### 2.6 Redis can require TLS, and `localhost` can no longer pass by accident

Two defects were found in this pass while re-reading configuration code rather
than test output. Both were silent: nothing failed, and nothing said so.

**The Redis client could not speak TLS at all.** `AUSTRO_REDIS_ADDR` was split
on `:` and dialled as plaintext. That is fine for a local container and wrong for
every managed Redis offering, because the sane ones — including AlwaysData's —
are reachable only over TLS. It also meant the deployment advice in §6/B2 had to
be "do not use managed Redis", when the real constraint was a missing client
setting.

`AUSTRO_REDIS_ADDR` is now the single connection setting, and the address alone
decides the transport:

| Value | Transport |
|---|---|
| `host:port` | plaintext, unchanged, so existing deployments keep working |
| `redis://host:port` | plaintext, now explicit rather than implied |
| `rediss://host:port` | TLS, minimum 1.2, certificate verified |

There is no `AUSTRO_REDIS_TLS` boolean, and deliberately no second host or port
setting: one address, one meaning, no way to write `rediss://` and a plaintext
port at the same time. The parser rejects a missing or out-of-range port, and it
rejects credentials, paths and query strings in the address, so an operator
cannot put a secret in `AUSTRO_REDIS_ADDR` where it will be logged. A bare
`redis:6379` is still read as host `redis`, port 6379, because a scheme is only
recognised when it is followed by `//` — the legacy form is not reinterpreted.

Certificates are verified, always: `InsecureSkipVerify` is never set, anywhere.
`AUSTRO_REDIS_CA_FILE` is an optional CA bundle for a private certificate
authority, appended to the system roots rather than replacing them. An
unreadable or invalid file is a startup failure, not a warning — silently
continuing with system roots would turn a configuration mistake into a confusing
handshake error much later. Requesting a CA file on a plaintext address is
rejected outright, since it means the operator believes they are encrypted when
they are not.

**`localhost` passed production validation.** The guard against pointing at a
development endpoint enumerated three literal addresses — `localhost:5432`,
`redis:6379`, `amqp://guest:guest@localhost` — and a service on a different port
slipped straight past it. `127.0.0.1:5433` was not on the list. Meanwhile
`AUSTRO_ALLOW_LOOPBACK` already existed and did exactly this job for every other
endpoint, so the loopback rule was simply not applied to these two.

Detection now covers `localhost`, the whole `127/8` range, `::1`, and the
unspecified addresses, and it is enforced for PostgreSQL, RabbitMQ, both DSNs and
Redis. The check inspects the address only. It does **not** resolve DNS, because
a name that resolves to `127.0.0.1` would otherwise require a resolver call
inside `config.Validate` — and a first implementation that did, with a per-call
timeout, drove `go test ./internal/config/` into a 10-minute timeout, because
enough tests each pay the timeout to turn a fast suite into a slow one. The
residual gap is named rather than hidden: a hostname that resolves to loopback is
still accepted. It is closed by refusing to resolve during validation, not by
paying for a lookup that can block startup.

`AUSTRO_ALLOW_LOOPBACK` is still the only way to run against loopback, it still
accepts exactly `1`, `true` or `yes`, and it is now declared where loopback is
the actual intent: the two CI service hosts and the native single-host
deployment. That is the point of the flag — a deliberate, greppable declaration
rather than an exemption granted silently to any address that happens to spell
`127.0.0.1`.

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

### Redis TLS and loopback pass

| Check | Result |
|---|---|
| `gofmt -l` on all changed Go files | clean |
| `go build ./...`, `go vet ./...` | pass |
| `go test ./internal/config/ -count=1` | pass — includes the new parser, CA-file and loopback tests |
| `go test ./infrastructure/redis/ -count=1 -v` | pass — TLS selection, generated-CA trust, foreign-CA rejection, wrong-hostname rejection, no silent plaintext fallback |
| Every package except `./tests`, under `-race`, `-count=1` | 27 packages `ok`, 0 failures, 0 data races |
| `go test ./tests/ -count=1 -v` | 118 failing functions, 21 failing subtests, 139 `--- FAIL` markers, 3 skips — **identical to the counts recorded in §3 above**, so this change moved no test |
| Every failing function names an unreachable service | 118/118; 0 unclassified |
| Per-cause tally | PostgreSQL 103, API 9, RabbitMQ 6, Redis 3 (121 occurrences = 117 single-cause + `TestPhase1ExitCriteria`'s four) |
| `TestPhase1ExitCriteria` subtests | 45 result lines (33 numbered criteria, criterion 22 nests twelve), **34 pass / 11 fail**; every failure is an infrastructure criterion, every static criterion passes |
| `TestConfigValidPasses`, `TestOpenAPIMatchesRegisteredRoutes` | pass, absent from the failure list |
| `Should NOT be empty` occurrences in the suite log | 0 |
| Deployment-contract tests that read the edited CI, env and deploy files | 7/7 pass, including all four subtests of `TestDeploymentArtifactsDeclareTheExecutionTransportContract` |
| `scripts/verify-production-deployment.sh` | **199 passed, 0 failed** — now on this host **without any shim**, because the interpreter probe is portable; see §6/B6 |
| Interpreter resolution exercised under all four conditions | pass — `python3`; `python` after a `python3` that fails the version probe; `py -3` after both fail; and hard failure naming all three candidates when none works (§6/B6) |
| `tests/build_identity_contract_test.go` | 22 passed, 0 failed, 2 skipped (the two manifest tests, which skip while the manifest lives outside the repository) |
| Two independent clones, four artifacts | **byte-for-byte identical** |
| Artifact inspection via `debug/elf` | 4/4 ELF64, `ET_EXEC`, correct machine, no `PT_INTERP` |
| Embedded commit, with a discriminating control | 4/4 present, 4/4 control absent — see §5 for why the first control was rejected |
| Manifest digests re-hashed from the assembled directory | 4/4 match, 4/4 distinct |
| Previous candidate after this pass | 6/6 files unchanged |
| Modified workflow YAML parse | pass, both files |
| TLS handshake against a live managed endpoint | **not run** — no TLS endpoint is reachable from here |
| `bash -n` on every tracked shell script | pass — 22/22 parse, including `deploy/native/generate-env.sh`, whose heredoc placement was previously only checked by reading. Git Bash (`C:\Program Files\Git\bin\bash.exe`) supplies the parser; the bare `bash` on this host is still the WSL shim and is not used |
| `scripts/build-release.sh` stored mode | was `100644` while `.github/workflows/native-arm64-deploy.yml` executes it directly; corrected to `100755`, see §6/B7 |

Two of these rows are corrections to this report, not new results. The earlier
§3 figure of "33 subtests" for `TestPhase1ExitCriteria` counted numbered
criteria while ignoring the twelve tamper cases nested under criterion 22, and
the subtest split was 34/11, not 34/12 — a naive `FAIL` filter also matches a
subtest *named* `23_fail-fast_on_missing_required_configuration`, which passes.
Both are fixed above and in §3.

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

### The shared-session cascade, and a correction to §10.1

An earlier pass in this report stated that all 134 failures in `austro-os/tests`
were `lookup postgres|redis|rabbitmq|api: no such host`. That was an over-claim.
22 of the 134 were not a service-unreachable diagnostic at all — they were
`Should NOT be empty`, raised against an access token in a test that had nothing
to do with the service being absent. Classifying each failure individually, as
this section now does, is what exposed why.

The cause was a test-harness defect, not a product defect:

- `tests/audit_visibility_live_test.go` established two package-wide sessions
  through `sync.Once`: `auditFounderOnce` and `auditRBACOnce`.
- Both once bodies called `testing.T`-dependent helpers. When the stack was
  unreachable, `require.NoError` inside `authJSON` (via
  `client.Do`) or inside `ensureRbacUsers` (via `ensureIsolationRoles`) called
  `FailNow`, which unwinds the goroutine with `runtime.Goexit`.
- `sync.Once` marks itself done from a `defer` in `doSlow`, so a body that never
  returned normally was still recorded as complete and was never retried.
- The error variables were therefore never assigned. `require.NoError(t,
  auditFounderErr)` saw `nil` and **passed silently**, and the only surviving
  symptom was `require.NotEmpty(t, auditFounderTok)` in a later, unrelated test.

The `nil` error passing `require.NoError` is the damaging part: the guard that
was supposed to distinguish "setup failed" from "setup succeeded" could not fail,
so the suite reported 22 assertion failures about an empty string instead of one
root cause per missing service.

Two of the four `sync.Once` bodies in the package were affected. The other two
were already correct and were left alone: `getEnv`'s `envOnce` cannot fail, and
`buildIdentityBinaries` keeps its `compileIdentityBinaries()` call free of
`*testing.T` and assigns `identityBuildErr` — that helper is the model the fix
follows.

The correction:

- `authJSON` is now a thin `testing.T` wrapper over `callAuthJSON`, which
  returns `(status, body, error)` and names the method, path and endpoint in
  every failure. The 65 existing call sites are unchanged.
- `ensureRbacUsers` is now a wrapper over `provisionRbacUsers(dsn)`, which
  returns errors instead of asserting. The 15 existing call sites are unchanged.
  The database handle is closed when provisioning returns rather than by a
  `t.Cleanup` registered on whichever test happened to run the once body first.
- `initAuditFounderSession` and `initAuditWorkspaceTokens` are plain functions
  returning `(value, error)`. Neither can return an empty token alongside a nil
  error.
- `sharedTokenFailure` is the single place a failure is surfaced, and an empty
  token with no error is reported as a harness fault rather than a product one.

No test was skipped, relaxed or removed, and no assertion was deleted to obtain a
green result. An unreachable stack still fails the suite, because this package
verifies running services and a skip would hide a broken deployment.

Evidence, from `go test ./tests/ -count=1 -v` on a machine with no stack:

| | Before the fix | After the fix |
|---|---|---|
| `--- FAIL` markers (functions + subtests) | 134 | 134 |
| `Should NOT be empty` | 22 | **0** |
| Failures naming an unreachable service | 112 | **134** |

The count is deliberately unchanged: the stack is still absent, so the suite
still fails. What changed is that every failure now names the missing endpoint.

### Reconciling the failure counts

The per-category figures previously printed in this section did not sum to the
stated total, and the reason was a real overlap rather than a missing test.

`TestPhase1ExitCriteria` is a gate that aggregates 33 numbered criteria spanning
all four services — 45 result lines in all, because criterion 22 nests twelve
tamper cases — and eleven of them need a live service. It therefore fails for
**four distinct reasons at once**. A per-cause tally counts it once in each
category it matches, so the categories sum to more than the number of failing
test functions. Exactly one function overlaps; the other 117 each have a single
cause.

Reproduce, from the repository root:

```powershell
$env:GOROOT='C:\Users\AUSTRO\AppData\Local\opencode-tools\go'
$env:PATH="$env:GOROOT\bin;$env:PATH"
go test ./tests/ -count=1 -v 2>&1 | Tee-Object tests-suite.log

# failing test FUNCTIONS: only unindented markers. Subtest cases are indented.
(Select-String -Path tests-suite.log -Pattern '^--- FAIL: ').Count        # 118
(Select-String -Path tests-suite.log -Pattern '^\s+--- FAIL: ').Count    # 21
(Select-String -Path tests-suite.log -Pattern '^--- SKIP: ').Count        # 3
```

The distinction that matters is the indentation. Go prints `=== RUN` for both a
test and its subtests without indenting, but indents the `--- PASS/FAIL/SKIP`
line for a subtest. Counting `=== RUN` lines therefore counts subtests as though
they were tests; the earlier figure of 113 was correct, but the categories
beneath it were derived by a segmentation that treated `TestPhase1ExitCriteria`'s
45 subtest result lines as independent segments, split that one function's four
causes across them, and inflated the PostgreSQL figure by one.

Corrected, with each function assigned to its first matching cause in the order
PostgreSQL, API, RabbitMQ, Redis:

| Cause | Before the policy change | After the policy change |
|---|---|---|
| PostgreSQL unreachable | 98 | 103 |
| API unreachable | 8 | 8 |
| RabbitMQ unreachable | 5 | 5 |
| Redis unreachable | 2 | 2 |
| **Failing test functions** | **113** | **118** |

Cross-check for the "after" column: 117 functions have one cause and one has
four, so per-cause occurrences are 103 + 9 + 3 + 6 = 121 = 117×1 + 1×4. No
failing function lacks a recognised cause, so nothing is unaccounted for.

A representative before/after for the same test:

```
before:  Error: Should NOT be empty
after:   shared founder session could not be established: bootstrapping the
         founder session: POST /api/auth/bootstrap against http://api:8080:
         Post "http://api:8080/api/auth/bootstrap": dial tcp: lookup api: no such host
```

`tests/shared_session_harness_test.go` holds the regression. It needs no live
infrastructure — it uses the reserved `.invalid` TLD (RFC 6761) and fixed local
responses — so a machine with no stack still catches the defect. It covers the
once-abort semantics, repeated callers receiving the original error, the
diagnostic wording, the fatal path (in a subprocess, since a failed subtest
cannot be observed from the inside), unreachable API and Postgres, a `200`
without an `access_token`, a malformed body, and DSN redaction. Verified offline
in 1.1s, and under `-race`.

### One live-infrastructure policy

The package previously mixed two opposite behaviours for the same condition.
Most tests failed when a service was unreachable, which is correct for a suite
that verifies running services. Five tests skipped instead, via a helper whose
own comment justified it as "degrades gracefully":

```go
// skipIfNoDB skips the test when the live PostgreSQL fixture is unreachable
func skipIfNoDB(t *testing.T) *sql.DB {
    ...
    t.Skipf("postgres unavailable, skipping RLS test: %v", err)
}
```

The reasoning — that a skipped environment "simply does not claim RLS coverage" —
was the flaw. A skip is not a disclaimer: in a non-verbose `go test` run a skipped
test is invisible, so the suite reported green over five security tests
(`TestExecutionRLSPoliciesEnabled`, `TestExecutionWorkspaceIsolation`,
`TestReclaimStaleRecoversCrashWithNoPendingRetry`,
`TestReclaimStaleFinalizesTheClaimedRetry`,
`TestRescheduleIsFencedAgainstThePreReclaimGeneration`) that had verified
nothing. Those five are exactly the +5 between the 113 and 118 columns above:
they moved from skip to fail and now name the missing database.

The policy is now stated once, in `tests/live_policy_test.go`, and applied
through one helper:

1. **Default: live infrastructure is required.** An unreachable service fails,
   naming the service, the endpoint, the cause, and the opt-in. A gate that
   cannot reach its datastore has verified nothing, so it must not read as green.
2. **Explicit offline mode.** `AUSTRO_TEST_MODE=offline` lets a
   infrastructure-dependent test skip with a reason naming both the dependency
   and the opt-in. It is never inferred from a failed connection and never the
   default. Only the exact value enables it — `off`, `0`, `true`, `yes` and
   `skip` all leave the strict default in place, so a typo in CI cannot convert a
   gate into a suite of skips.
3. **A gate cannot be satisfied by skipping.** `AUSTRO_REQUIRE_LIVE_TESTS=1`
   contradicts offline mode, and the contradiction is a failure rather than a
   downgrade to skips. The same variable asserts that live PostgreSQL and the
   live API were each actually contacted during the run, so a gate cannot pass
   having reached nothing.

`tests/live_policy_regression_test.go` asserts all three clauses. The
skip-versus-fail distinction cannot be observed from inside a test, because a
skipped subtest and a passed one both report success to `t.Run`, so those cases
re-execute the compiled test binary against the reserved `.invalid` TLD and
assert on its exit status and output. It also pins the strict default against
near-miss values, and scans the package for infrastructure-availability skips
outside the policy file — the check was verified to fail when the old
`skipIfNoDB` line is reintroduced.

Skips that are *not* about infrastructure availability are unaffected, and are a
different thing: `TestManifestAgreesWithBinaryIdentity` and
`TestReproducibilityClaimIsBackedByTheArtifactSet` skip when no
`release-manifest.json` exists, and the audit chain test skips when the chain is
too short to page. Those tests are inapplicable, not unverified.

Deliberately **not** done: the remaining 113 infrastructure failures were not
converted into offline-mode skips. Widening offline mode to the whole package
would make a broken deployment indistinguishable from a developer's laptop, which
is the failure this policy exists to prevent.

Three tests skip in the default run and are reported as skipped, not passed: the
two manifest-dependent ones above, and `TestReleaseGateRequiresLiveInfrastructure`,
which asserts the gate's live-contact requirement and is itself skipped unless
`AUSTRO_REQUIRE_LIVE_TESTS=1` is set.

This closes the harness defect and the policy inconsistency. It does not close
lifecycle verification: RabbitMQ ack/nack/requeue settlement, lease expiry and
stale reclaim, heartbeat cancellation, duplicate delivery, recovery,
stale-generation fencing, worker-crash recovery and a real SIGHUP shutdown are
still unproven against running services, because no stack is available here. Note
that three of those five newly-failing tests are themselves stale-reclaim and
generation-fencing tests, so their coverage is currently **known to be absent**
rather than merely unverified. Those are recorded in §10.2, and no lifecycle
result is claimed here.

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

**Current build source commit: `69d7fa726215c4099d824747686a400aafcf16ef`**
(`feat(redis): verified TLS from AUSTRO_REDIS_ADDR, and a real loopback guard`),
tree `e74b2f1f2c06bd019b88212ec72f3860f26eef83`. It supersedes the candidate
described below, which was built from `af59c4e` and is preserved untouched.

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

At the time those two builds were made, the clones had to be created with
`-c core.autocrlf=false`: this repository had no `.gitattributes` and this host has
`core.autocrlf=true`, so a default clone checked the tree out with CRLF, which made
`scripts/build-release.sh` unrunnable and left the working tree dirty for reasons
unrelated to the source.

**That is no longer true, and the difference is a repository change rather than a
command-line convenience.** `.gitattributes` now pins `* text=auto eol=lf`. A fresh
clone created with **default** settings — `core.autocrlf=true`, no `-c` override —
comes out with:

| Property of a default-settings fresh clone | Result |
|---|---|
| `git status --porcelain` before building | 0 entries |
| Tracked files containing any CR byte | **0** |
| Frozen gate SHA-256 | `9119aa71…`, matching the primary checkout |
| `scripts/build-release.sh` | runs to completion, exit 0 |
| Working tree after building | still 0 entries, when the output directory is outside the repository |

The 126 entries that `git status` reported as modified in the primary checkout
while `git diff` reported them unchanged were a stale index/stat condition left by
an earlier mixed-line-ending checkout under `core.autocrlf=true` — the working-tree
bytes already matched the index blobs (for `internal/api/handler.go` the cached index
size was 16246 against an actual 15792). They were cleared by re-adding those 126
paths explicitly, by path, which changed **no** blob: no tracked content was
renormalized.

One honest caveat about this host: 69 tracked files in the *primary working
directory* still physically hold CRLF, because they were checked out before the
policy existed and have not been re-checked-out. Git reports them clean, since the
`eol=lf` clean filter normalises CRLF on comparison and any future commit stores
LF, and the index itself contains **0** CRLF blobs. The authoritative portability
evidence is the fresh clone above, not the state of this working directory.

Built with the repository's own flags: `-trimpath -ldflags '-s -w'` plus the
linked identity, `CGO_ENABLED=0`, via `scripts/build-release.sh` - the single
definition the native ARM64 job calls. Binary names match what each install
script expects (`austro-api`/`austro-worker`).

Location: `C:\Users\AUSTRO\AppData\Local\Temp\opencode\austro-release-candidate-redistls\`
(manifest: `release-manifest.json`)

| Target | File | Bytes | SHA-256 |
|---|---|---|---|
| linux/amd64 | `austro-api` | 14,008,482 | `e5e7f3604edcdd6c50205f308680c69c0191c9371857fcd2110d3c560058f30b` |
| linux/amd64 | `austro-worker` | 12,026,018 | `2b0e9596a517d753511aea4d5dd3fd889ab32f6bc55ff101888859c0ba55712f` |
| linux/arm64 | `austro-api` | 13,172,898 | `aec8a84a9d2d6268553c1c761f391ab61ac86a33c47cbf2c1b7531eb78fd5b46` |
| linux/arm64 | `austro-worker` | 11,337,890 | `e82a448be7873c92e238eaf0246aa2bf7475a832a7ff710f9e421663718e5608` |

All four are byte-for-byte identical across the two independent clones, four
distinct digests, each ELF64 `ET_EXEC` with the expected machine and no
`PT_INTERP`, each carrying `commit=69d7fa72…` and none carrying
`commit=unknown` or `commit=dev`. Digests were recomputed from the assembled
candidate directory rather than copied from the build tree, so the manifest
describes the files that were actually shipped.

One control in that inspection was wrong at first and is worth recording. The
search for the linked commit originally used 40 ASCII zeros as its negative
control, and it matched all four binaries - which looks like a failure of the
stamp. It is not: the binary genuinely contains 40 consecutive zeros inside an
embedded HTTP response example. A control that occurs naturally in the artifact
proves nothing. The control was replaced with the previous candidate's commit
`91e9c8c3b084e3a96ebc912c9132fa458e299c2c`, which is absent from all four, so
the check now discriminates.

### Why these artifacts were not rebuilt — proven, not assumed

The hardening work that followed `69d7fa7` touched `.gitattributes`, a shell
verifier, two test files and one file mode. None of those is a compiled input, so
the candidate above stays as it is. That claim is checked rather than assumed.

A clean rebuild of `linux/amd64` from `3aa2c66` (clean tree, `vcs.modified=false`,
output written outside the repository) was compared against the shipped candidate
byte for byte:

| | `austro-api` | `austro-worker` |
|---|---|---|
| Size, candidate vs rebuild | 14,008,482 = 14,008,482 | 12,026,018 = 12,026,018 |
| Differing bytes before normalising | 233 | 230 |
| Differing bytes after rewriting commit, commit-time and module pseudo-version | 80 | 77 |
| Where those remaining bytes are | the Go build action ID at offsets 3900–4200 | same |

The build action ID is a hash *derived from* the fields that were normalised, so it
is expected to move with them. After that rewrite the two binaries are identical
outside a hash of their own provenance. **No code and no data differs.**

Two findings from this comparison are worth recording because both would have made
the comparison lie:

- The first attempt built with the output directory *inside* the clone. Go then
  stamped `build.vcs.modified=true`, and the worker showed 4434 differing bytes
  rather than 230 — the dirty-tree flag shifted a build-info region and it looked
  like a code change. Building outside the repository removed it. A build that writes
  into its own source tree is not a clean build, whatever its exit code says.
- A compiled Go binary contains many 40-character hex runs (crypto bignum constants),
  so "the embedded commit" cannot be found by pattern. It is identified as the run
  present in one binary and absent from the other — 74 runs in the API, 73 of them
  shared, leaving exactly one unique to each build.

The candidate is therefore still the artifact of `69d7fa7`, and this report does not
claim otherwise.

### Previous candidate — preserved, superseded

`C:\Users\AUSTRO\AppData\Local\Temp\opencode\austro-release-candidate\`, built
from `af59c4efc06c162790c171bca38c9b3cfe7685ba`. Its six files were fingerprinted
before this pass began and re-hashed after it finished, and all six are
unchanged. It is kept because it is the only artifact set whose full
verification, including an executed `--version` on a natively runnable
equivalent, was recorded; the Redis candidate is verified at byte level only,
for the reason given under *What remains true of any candidate* below.

### Build identity — VERIFIED, and this closes the gap the last candidate had

The previous candidate carried a standing admission: *"No binary embeds its
build identity... Identifying a running deployment requires this manifest."* That
is now false, deliberately. A released binary names its own source:

```
austro-os api    version=1.0 commit=69d7fa726215c4099d824747686a400aafcf16ef
austro-os worker version=1.0 commit=69d7fa726215c4099d824747686a400aafcf16ef
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
amd64   austro-api     REPRODUCIBLE  e5e7f3604edcdd6c50205f308680c69c0191c9371857fcd2110d3c560058f30b
amd64   austro-worker  REPRODUCIBLE  2b0e9596a517d753511aea4d5dd3fd889ab32f6bc55ff101888859c0ba55712f
arm64   austro-api     REPRODUCIBLE  aec8a84a9d2d6268553c1c761f391ab61ac86a33c47cbf2c1b7531eb78fd5b46
arm64   austro-worker  REPRODUCIBLE  e82a448be7873c92e238eaf0246aa2bf7475a832a7ff710f9e421663718e5608
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
- The `af59c4e` candidate is superseded. Every digest changed, because every
  binary carries a different linked commit. Three of the four artifacts also grew:
  `austro-api` by 45,056 bytes on amd64 and 65,536 on arm64, and `austro-worker`
  by 24,576 on amd64, which is the endpoint-parsing and loopback-detection code
  entering the linked configuration package. `austro-worker` on arm64 is the same
  size to the byte, because the worker links configuration but never dials Redis,
  so for that one artifact the only change is the commit string it carries - same
  length, different content. A digest difference with an identical size is a
  useful reminder that size is not evidence of anything.

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

### B2 — AlwaysData platform capability resolved; target account still unverified
**Severity: low for release scope, high for that target**

**Status: platform facts and process topology resolved. Account validation
outstanding.** Infrastructure-gate status, verbatim:
`INFRASTRUCTURE CONFIGURATION INCOMPLETE — BLOCKERS REMAIN` (see §10 — the
three units have not been run against live services, so the configuration is not
verified in the sense the status string requires).

This blocker was raised because the runbook assumed AlwaysData's Service Manager
could run the API and a separate `austro-worker` process, and the worker is a
RabbitMQ consumer that serves no HTTP. That was an unverified assumption about a
third-party platform, and it also contradicted the repository's own
`docs/deployment-targets.md`, which listed targets 1–6 and excluded AlwaysData
while declaring PaaS hosts NOT SUPPORTED.

**What has since been established, from AlwaysData's own documentation** (all
links are in `deploy/alwaysdata/RUNBOOK.md`, which cites each one):

- **Services are exactly the right primitive.** They are long-running custom
  programs, supervised and automatically restarted, on all paid plans from
  **Small** up. The non-HTTP, always-on consumer this blocker asked about is a
  documented capability, not a question.
- **Scheduled Tasks are not an alternative.** A Task whose previous run has not
  finished is skipped, so a Task cannot be a durable consumer.
- **Restart is by `SIGHUP`,** and repeated failures in a short period
  **automatically disable** a Service. Both binaries now handle `SIGHUP` as a
  graceful shutdown, so restarts settle in-flight requests and unacknowledged
  broker messages instead of dropping them.
- **Service ports are not private.** Ports `8300-8499` are reachable from the
  internet with **no network filtering**, and on Public Cloud they are IPv6-only
  on hosts separate from the HTTP and SSH servers.

**The last point changed the architecture, and it is worth stating plainly: the
previous runbook had the API on a public Service port behind a reverse proxy,
which exposed the API in plaintext and let anyone bypass the site's TLS.** The
API is now specified as a User Program **site** (the documented way to run a Go
HTTP app on AlwaysData, behind platform TLS termination), with the dispatch
sweep loop moved to its own Service using the **same `austro-api` binary** under
`AUSTRO_PROCESS_ROLE=dispatcher` and `AUSTRO_EXECUTION_DISPATCH_MODE=service`.
The dispatcher opens no listener and needs no port, so there is nothing left to
expose. The site runs `AUSTRO_PROCESS_ROLE=api` with
`AUSTRO_EXECUTION_DISPATCH_MODE=off`, which is a legitimate production
configuration — but only if it names the companion in
`AUSTRO_EXPECT_COMPANION_PROCESSES`, so that "the sweep lives in another
process" is a checked statement rather than an absence nobody notices. The site
cannot observe a companion on another host, so the dispatcher and worker each
write a heartbeat row to PostgreSQL and `/health/ready` answers `503` with
`companion-absent` when a declared companion stops reporting.

That replaces an earlier, stricter rule which refused `off` in production
outright. The blanket refusal was safe and also useless: it made the split
topology — the only topology that satisfies the port constraint above —
impossible to express, so the runbook had to describe a deployment the
configuration could not represent. A required declaration plus an observable
readiness verdict enforces the same invariant while letting the intended
topology be written down.

An unrecognised role or mode is rejected rather than silently treated as `api`;
a fallback there would have put the sweep loop back in the HTTP process while
reporting a healthy start. The role/mode matrix is enforced in one place
(`config.Validate`) and the entrypoints branch on the same normalised role, so
a deployment cannot pass validation and then be misread by the binary it
starts.

`docs/deployment-targets.md` now lists AlwaysData as supported target 7 **with
conditions**, and the PaaS exclusion is narrowed to what is actually true: a host
that offers neither supervised foreground processes nor TLS termination cannot
run this application.

**What is still open — none of it answerable from this repository:**

- the account's plan (Services need **Small** or above) and whether it is Public
  or Private Cloud;
- whether outbound access to the managed PostgreSQL and RabbitMQ works from the
  site *and* from both Services, which run on different hosts;
- whether a Redis option is acceptable at all. **Managed Redis is Private Cloud
  only**, and the alternative — self-hosting it on a Service port — publishes
  workspace memory on an internet-reachable, unfiltered port. This is a security
  decision for the account owner, and it is the one item that could still change
  the recommended topology. It is now a decision about the account rather than
  about this codebase: the client could not speak TLS at all when this blocker
  was written, which forced the choice between a plaintext port and no Redis. §2.6
  adds verified TLS, so an externally managed Redis over `rediss://` is a real
  option and the plaintext-port workaround is no longer forced by a client
  limitation.

**Needs:** an AlwaysData account on which the checklist in the runbook is
completed. Until then AlwaysData remains **documented but unexercised** — which
is a different and much smaller claim than the one this blocker used to make.
Targets 1/2/6 remain the supported deployment units for the release itself.

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

### B6 — RESOLVED IN THE GATE: the three "python3 unavailable" blocks were a false negative, and the probe is now portable
**Severity: none (cause fixed in the verifier, not worked around)**

`scripts/verify-production-deployment.sh` probed for `python3` by name and reported
three checks as BLOCKED:

```
FAIL  YAML structural lint could not run (python3 unavailable) — BLOCKED
FAIL  workflow lint could not run (python3 unavailable) — BLOCKED
FAIL  operator-script variable audit could not run (python3 unavailable) — BLOCKED
```

This was **not** a missing dependency. This machine has Python 3.14.7 installed at
`C:\Python314\python.exe`; it is simply not exposed under the name `python3` in Git
Bash. The gate probed by name, so it concluded Python was absent. A hard BLOCKED on
a machine that has Python is a reporting bug in the gate, so it has been fixed in
the gate rather than papered over with a shim on `PATH`.

The verifier now resolves its interpreter once, trying `python3`, then `python`,
then `py -3`, and requires a candidate that actually passes
`sys.version_info[0] >= 3` before accepting it. Two details matter:

- **The probe runs code.** A host where `python` is Python 2 fails the version check
  and moves to the next candidate, instead of letting a Python 2 run a linter
  written with f-strings and reporting the syntax error as a lint failure in
  `docker-compose.production.yml`.
- **The result is an array, not a string.** `py -3` carries an argument that must not
  be glued onto the command name. Every invocation goes through `run_python`, which
  appends its arguments after the candidate words and returns the interpreter's own
  exit code untouched.

**Static verification after the approved re-freeze: 189 passed, 0 failed.**

**After the build identity change: 199 passed, 0 failed.** Ten checks were added
for identity in the image build.

**After this fix: 199 passed, 0 failed on this host with no shim on `PATH`.** The
verifier reports which interpreter answered, so a future reader can tell a genuine
lint failure from an environment problem at a glance:

```
INFO  Python 3 for the YAML and operator-script checks: python (3.14.7)
```

The three checks genuinely execute, and they **pass**:

- `production compose parses as structurally well-formed YAML`
- `CI workflow parses as structurally well-formed YAML`
- `operator-script variable audit` — passes

The negative control still passes, which is what makes the two rows above mean
something: `compose linter rejects a deliberately broken file (negative control)`.
A linter that accepts anything would also "pass" the real file. **The
execution-transport block in `docker-compose.production.yml` is machine-validated,
not merely read over.**

`tests/verifier_python_detection_test.go` pins all four conditions by shadowing every
supported name inside a sandbox directory placed first on `PATH`, so the result does
not depend on what the host has installed:

| Condition | Result |
|---|---|
| `python3` present | uses `python3`, 199/0, exit 0 |
| `python3` fails the version probe | falls through to `python`, 199/0, exit 0 |
| both fail | uses `py -3`, argument preserved, 199/0, exit 0 |
| none usable | **exit 1**, 3 failures, 3 `BLOCKED` lines naming all three candidates, 0 lint passes |

The last row is the important one: absent Python is still a hard failure. The fix
makes the gate portable, not permissive.

### B7 — RESOLVED: a release script that CI executes directly was not executable
**Severity: latent CI failure (mode bit, pre-existing)**

`.github/workflows/native-arm64-deploy.yml` runs the release build as its single
source of truth:

```yaml
run: |
  set -euo pipefail
  scripts/build-release.sh /tmp/native arm64
```

There is no `chmod` before it, but `scripts/build-release.sh` was tracked as mode
`100644`. A file stored `100644` checks out non-executable, so on a Linux runner
that step fails with `Permission denied` under `set -euo pipefail`.

This was **born broken, not introduced by this pass**: the mode was `100644` in
`af59c4e`, the same commit that added the workflow line, and it stayed `100644`
through every commit since. It was invisible here because Git Bash on Windows does
not enforce the stored mode bit — `./scripts/build-release.sh` ran fine on this
host — so the defect could only surface on the runner that matters.

Fixed to mode `100755`. The blob is unchanged (`426f8f9c…`); only the mode moved, so
no artifact byte is affected.

The other four non-executable tracked scripts were checked and deliberately left
alone. `scripts/browser-e2e-mutation.sh` is also invoked directly, but
`.github/workflows/browser-e2e.yml` runs `chmod +x` on it first, so it works;
`scripts/phase2-verify.sh`, `scripts/publish.sh` and `scripts/verify_build.sh` have
no direct invocation anywhere in the repository.

`tests/shell_script_executable_bits_test.go` now enforces this: any tracked shell
script executed directly — outside a comment, not through `bash name.sh`, and not
chmod'd by the invoking file — must be tracked `100755`, or the test fails naming
the file and line. The guard was confirmed to have teeth by reverting the mode and
watching it report `native-arm64-deploy.yml:62`.

---

## 7. Per-phase status

| Phase | Status | Note |
|---|---|---|
| 1. Baseline: transport, providers, deployment | **DONE** | §0, §1; the deployment-layer gap and the untracked-subsystem finding were the headline results |
| 2. Explicit RabbitMQ transport configuration | **DONE** | §2.1–§2.4, §0.2; staged tree builds, vets and tests clean on its own |
| 3. Real AI provider configuration and smoke test | **PARTIAL** | Offline path verified; live path implemented but not run — B1 |
| 4. Target deployment environment verification | **PARTIAL** | Native Windows stack re-verified; Compose never executed (B3); AlwaysData platform capability and topology now resolved and documented, account still unvalidated (B2) |
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
  30 line-ending-only entries in `tests/` that an earlier pass left untouched have
  since been resolved by `.gitattributes` plus a path-by-path re-add that changed
  no blob; they are no longer carried as a known-dirty exception. The release builds
  ran in independent clones rather than in the working directory, which sidesteps the
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

B4, B5, B6 and B7 and the build identity gap are closed. The remaining work is
external, and none of it can be settled from this environment:

1. **B1** — run the live provider test (§4) with a real credential, obtained
   securely and with approval for a possibly billable request, and add the
   equivalent for the `generic-http` publishing backend.
2. **B2** — no longer needs documentation; the platform capability is settled and
   cited. What it needs is the account checklist in
   `deploy/alwaysdata/RUNBOOK.md`: confirm the plan allows Services, confirm
   outbound reachability to the managed PostgreSQL and RabbitMQ from the site and
   from both Services, and **decide the Redis question** — managed Redis is
   Private Cloud only, and the alternative publishes workspace memory on an
   internet-reachable port. If no account can be made to satisfy those, drop
   AlwaysData from the release scope and target 1/2/6, which are the documented
   supported targets.
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

**Infrastructure-gate status (B2, verbatim):**
`INFRASTRUCTURE CONFIGURATION INCOMPLETE — BLOCKERS REMAIN`

That second line is deliberately not the more flattering
`INFRASTRUCTURE CONFIGURATION VERIFIED — ACCOUNT VALIDATION PENDING`. The
difference between them is whether the three-unit topology has actually been run
against live services, and it has not: there is no PostgreSQL, Redis, RabbitMQ
or AlwaysData host reachable from this environment. What is verified is the
*configuration contract* — see the table below — not the running system. Claiming
account-validation-pending would imply everything except the account check had
been exercised, and it has not.

| | |
|---|---|
| Build source commit | `69d7fa726215c4099d824747686a400aafcf16ef` — tree `e74b2f1f2c06bd019b88212ec72f3860f26eef83`, 401 source files |
| Commit carrying this report | necessarily later, and deliberately not the build source; a file cannot contain the SHA of the commit that contains it |
| Working tree at this pass | `main`, at the commit carrying this report and therefore necessarily later than the build source. The artifacts above embed `69d7fa7`, the commit the Redis TLS and loopback work is in, and must not be described as containing any later commit. The hardening commits that follow `69d7fa7` (`.gitattributes`, the `build-release.sh` mode bit, the portable interpreter probe, two test files) change **no compiled input**, which §5 proves |
| Hardening commits after the build source | four, none of which touches a compiled input: `.gitattributes` in `3aa2c66` (tree `46851f0e…`); the `build-release.sh` mode bit and its guard test in `2411104`; the portable interpreter probe, the verifier change and their tests in the commit carrying this report. The fourth is the documentation commit `d64e8f7` |
| Line-ending policy | `.gitattributes` = `* text=auto eol=lf`. Index contains 0 CRLF blobs; a default-settings fresh clone has 0 CR bytes in any tracked file and 0 `git status` entries before and after a build |
| Artifacts | 4 (linux/amd64, linux/arm64 × api, worker), byte-reproducible across two independent builds, each carrying `version=1.0` and its own source commit |
| Build identity | embedded in every released binary, verified; see §5 |
| Per-build provenance | `release-manifest.json` (digests, toolchain, build time), written outside the repository |
| Prior release-candidate commit | `91e9c8c3b084e3a96ebc912c9132fa458e299c2c` — the 56 reviewed paths |
| Superseded candidate | `af59c4e`, preserved unmodified at `austro-release-candidate\`; verified as unchanged by re-hashing all six of its files after this pass |
| Follow-ups | documentation-only commits, which change no artifact byte; plus the build identity commit `af59c4e` and the Redis TLS commit `69d7fa7`, which do |
| Frozen architecture gate | re-frozen to `9119aa71…` under approval, byte-identical throughout this pass; static verifier 199/0 with no shim |
| Staged-tree verification | build 0, vet 0, buildinfo tests 0, identity contract suite 43 pass / 0 fail / 2 skip (22 of those are top-level functions; the rest are their subtests) |
| Full `./tests` suite after this pass | 118 failing functions, 21 failing subtests, 139 `--- FAIL` markers, 3 skips — the **identical set of test names** as before this pass, so nothing regressed |
| Regression guards added | `tests/verifier_python_detection_test.go` (4 test functions; 6 cases once the 3 subtests of the table-driven discovery test are counted) and `tests/shell_script_executable_bits_test.go` (1 test function), all passing |
| Pushed | **No** |
| Deployed | **No** |
| Open blockers | B1 live AI provider, B2 AlwaysData account unvalidated, B3 Docker/Compose unexecuted |

### 10.1 Verified in this pass (no live infrastructure required)

These are static and unit-level properties. Each has a regression test, so a
later change that reintroduces the defect fails the suite rather than quietly
restoring it.

| Property | How it is held |
|---|---|
| Role/mode matrix enforced in one place | `config.Validate`; every other pairing refused at startup |
| The API binary cannot be started as a worker | `assertServableRole`, tested across case and padding variants |
| The worker binary cannot be started as another role | `assertWorkerRole`, tested both directions |
| The entrypoints and the validator cannot disagree about the role | one `ProcessRoleName()` accessor; `isDispatcherRole` branches on role, not mode |
| Dispatcher and worker open no listener and dial no Redis | no `net.Listen`/`http.Server`/`redis.` reference in `cmd/worker`; the dispatcher returns before the HTTP path |
| Redis is an API-only dependency | `UsesRedis()` keyed on role; the other two roles validate without a Redis credential |
| Readiness checks exactly the declared companions | `readinessDeps`; an undeclared role is never queried, so a single-process deployment gains no database dependency |
| A declared-but-unreadable companion fails closed | `503` with `liveness-unavailable`, full documented body |
| The 503 body satisfies the documented schema | `ready`, `status`, `reason`, `requires` all present; asserted against `api/openapi.yaml` |
| Route parity between server and published contract | `TestOpenAPIMatchesRegisteredRoutes` |
| A shared session initializer cannot lose its error | `sync.Once` bodies are free of `*testing.T`; `tests/shared_session_harness_test.go` |
| An unreachable service is named, never reported as an empty token | `sharedTokenFailure`; every failure in `austro-os/tests` now names its endpoint |
| Unavailable infrastructure fails by default, skips only on explicit opt-in | `tests/live_policy_test.go`; `AUSTRO_TEST_MODE=offline` is the only opt-in |
| A near-miss value cannot enable offline mode | `off`, `0`, `true`, `yes`, `skip` all keep the strict default |
| The verifier's interpreter probe is portable and still fails closed | `tests/verifier_python_detection_test.go` — all three names accepted, a non-Python-3 candidate passed over, absence of Python still a hard BLOCKED |
| The verifier cannot regress to a hard-coded interpreter name | `TestVerifierSourceInvokesNoInterpreterByHardCodedName` matches `python3` in command position only |
| A shell script CI runs directly is executable in the repository | `tests/shell_script_executable_bits_test.go`, which names the invoking file and line when it is not |
| Line endings are pinned by the repository, not by a clone flag | `.gitattributes` = `* text=auto eol=lf`; a default-settings fresh clone has 0 CR bytes and 0 status entries |
| The shipped candidate still matches its stated source | §5: a clean rebuild of `3aa2c66` is identical to the `69d7fa7` artifacts outside their own provenance fields |
| A release gate cannot pass by skipping | `AUSTRO_REQUIRE_LIVE_TESTS=1` refuses offline mode and requires a recorded live contact |
| No infrastructure skip outside the policy | `TestNoInfrastructureSkipsOutsideThePolicy` scans `tests/` |
| `rediss://` produces a verified TLS client, `redis://` and `host:port` do not | `infrastructure/redis/tls_test.go`; a generated CA is trusted, a foreign CA and a wrong hostname are both refused |
| `InsecureSkipVerify` is never enabled | single `tls.Config` construction site in `infrastructure/redis/tls.go`; no such field anywhere in the tree |
| A CA file cannot be set on a plaintext address | rejected in `config.Validate`, so the setting cannot imply encryption that is not there |
| Secrets cannot enter `AUSTRO_REDIS_ADDR` | the parser rejects credentials, paths and query strings |
| Loopback is refused in production on any port or address form | `localhost`, `127/8`, `::1` and unspecified addresses, across both DSNs, RabbitMQ and Redis |
| Loopback stays possible when it is intended | `AUSTRO_ALLOW_LOOPBACK` declared by CI and the native single-host deployment |

Local suite result: `gofmt` clean on the changed files, `go build ./...` clean,
`go vet ./...` clean, and every unit-test package passing — 27 test packages
green under `-race` with zero data races, out of 38 packages in total (the other
11 have no test files). The static gates pass: OpenAPI route parity,
principle references and registry, orphan schemas, bearer-auth declarations, and
every static subtest of `TestPhase1ExitCriteria`. The only failing package is
`austro-os/tests`, which needs a running Compose stack.

That package's failures are individually classified rather than assumed, by the
reproducible commands in §3:

| Measure | Count |
|---|---|
| `--- FAIL` test functions (unindented) | 118 |
| `--- FAIL` subtest cases (indented) | 21 |
| `--- FAIL` markers total | 139 |
| `--- SKIP` test functions | 3 |
| `Should NOT be empty` | 0 |

All 118 name an unreachable service: 98 PostgreSQL, 8 API, 5 RabbitMQ, 2 Redis
after assigning the one four-service function (`TestPhase1ExitCriteria`) to
PostgreSQL by precedence; per-cause occurrences are 103/9/6/3. The three skips
are the two release-manifest-dependent tests and the gate's own live-contact
assertion, none of which is an infrastructure test. None of the 118 is a code
regression; `TestConfigValidPasses` and `TestOpenAPIMatchesRegisteredRoutes`,
which *were* real regressions earlier in this pass, now pass.

Five of the 118 are tests that previously **skipped** and reported nothing —
the RLS and stale-reclaim tests governed by the old `skipIfNoDB`. They now fail
by the stated policy, so the suite is honest about work it had been silently
declining to do.

### 10.2 Still unverified, and what would close it

- The three units have never run together against live PostgreSQL, RabbitMQ and
  Redis. Heartbeat freshness, the 503 transition, and lease/ack behaviour are
  unit-tested against scripted readers only.
- Message lifecycle against a real broker is unproven: ack, nack, requeue
  settlement, lease expiry, stale reclaim, duplicate delivery, retry and recovery,
  stale-generation fencing, and worker-crash recovery. `internal/execution` and
  `internal/worker` pass under unit tests and `-race`, and
  `cmd/worker/shutdown_signals_test.go` covers SIGHUP handling in-process, but
  none of that is a live settlement against RabbitMQ.
- Five lifecycle tests are now known **not** to have run, rather than merely
  being unverified: `TestExecutionRLSPoliciesEnabled`,
  `TestExecutionWorkspaceIsolation`, and the three stale-reclaim and
  generation-fencing tests in `execution_reclaim_live_test.go` skipped before the
  policy change and now fail. Their subject matter — that the execution
  aggregates carry forced RLS, and that a stale claim cannot be rescheduled
  across a generation — is unexercised.
- The AlwaysData account facts in §6 remain unknown: plan tier, Public vs
  Private Cloud, Service entitlement, egress, DNS/IPv6, limits, Redis.
- Redis TLS is implemented and unit-tested (§2.6), but **no live TLS endpoint has
  been connected to**, so "the handshake works against a managed provider" is not
  a claim this report makes. The gap that was here — a client with no TLS setting
  at all — is closed; what remains is a live connection and the provider's real
  certificate chain.
- Loopback detection deliberately does not resolve DNS (§2.6), so a hostname
  resolving to `127.0.0.1` is still accepted in production unless
  `AUSTRO_ALLOW_LOOPBACK` is unset. This is a known, deliberate limitation, not an
  oversight.
- Compose bring-up (B3) has never been executed; no Docker engine here.

Everything in this list needs the same missing thing: a running stack. The
harness fix in §3 means that when one is available, its first integration run
will report real failures rather than an empty-token cascade, and the policy in
§3 means that a run which reaches nothing will fail the gate instead of appearing
green. Those two changes are the prerequisite for trusting whatever the first
live run reports.

The code is in a state where a release decision can be made. The three remaining
blockers are all about *external facts that cannot be established from this
environment* — a live provider response, a hosting platform's actual capabilities,
and a working Docker engine. Each is stated with what evidence would close it
rather than left as a vague "unverified". The one self-inflicted gap in the
candidate chain — binaries that could not identify themselves — is closed.
