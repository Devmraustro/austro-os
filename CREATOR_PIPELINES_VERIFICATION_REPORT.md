# Creator/Pipelines Browser E2E Verification Report

**Date:** 2026-09-16 (Europe/London)
**Branch:** `arena/01a0a5ea-austro-os`
**Implementation evidence HEAD:** `8d3e0afbe60fb5f8474316041f7c30b24cdfc2d2`
**Final status:** `COMPLETE`

This report records the real browser verification requested for the existing
Creator/Pipelines application. Claims are classified as `VERIFIED BY
EXECUTION`, `CI VERIFIED`, `STATICALLY VERIFIED`, or `ENVIRONMENTALLY
UNAVAILABLE`; no browser result is inferred from HTTP-only tests.

## 1. Framework and environment

**CI VERIFIED —** the job uses the repository's smallest production-appropriate
browser stack: Playwright Test `1.63.0` with headless Chromium. The test uses
stable application selectors (`#auth-view`, `#app-view`, `#pipeline-body-rows`,
`#audit-body`, and form/button IDs), Playwright eventual assertions, and bounded
polling. It does not use arbitrary sleeps for application state.

**VERIFIED BY EXECUTION —** GitHub Actions job `Real Chromium Creator/Pipelines
E2E` runs the embedded production browser application against a real local
stack on `ubuntu-latest`:

- PostgreSQL/pgvector `pgvector/pgvector:pg16`;
- Redis `redis:7-alpine`;
- RabbitMQ `rabbitmq:3-alpine`;
- the built AUSTRO API binary;
- the built AUSTRO worker binary; and
- real Playwright Chromium.

The workflow waits for both API health endpoints and for the worker's
`worker-started` log event. It calls the real `/api/auth/bootstrap` endpoint to
initialize the configured Founder, then provisions fresh per-run workspace and
identity records through `cmd/browser-e2e-setup`. Credentials and identifiers
are generated per run and masked; no existing IDs or warm state are assumed.
The browser uses the existing login/token/refresh/logout semantics, not a fake
browser authentication mechanism.

The workflow has a 45-minute bound, a bounded Chromium install, a required
non-zero test-list guard, and no `continue-on-error`, `|| true`, or `exit 0`
shortcuts. Playwright is configured with `screenshot: only-on-failure`,
`trace: retain-on-failure`, and `video: retain-on-failure`. On failure the job
uploads `test-results/`, the Playwright HTML report, and diagnostic logs as the
`austro-browser-e2e-failure-*` artifact. Cleanup and API/worker shutdown run
with `always()`.

The local agent image has no Go or Chromium runtime, so local browser execution
was not substituted. The actual browser execution below ran in GitHub Actions.

## 2. Exact DOM journey

**VERIFIED BY EXECUTION —** `browser-e2e/creator-pipelines.spec.js` runs one
real Chromium test and completed on the final implementation HEAD. The
rendered journey is:

1. Founder opens the login view, submits a wrong password, and sees the real
   rendered validation/authentication error.
2. Founder logs in with the configured credentials, sees the dashboard identity
   and both freshly-created workspaces, then logs out; session storage is
   cleared.
3. Founder logs in again without a workspace binding, refreshes Pipelines, and
   sees the rendered authorization-denied message (`Pipelines require a
   workspace identity.`).
4. Workspace admin signs in to workspace A and sees the real Creator/Pipeline
   empty state and loading transition.
5. The admin submits the Creator/Pipeline create control. The UI shows the
   success message and one rendered pipeline row. There is no stage/status
   input or arbitrary lifecycle control.
6. Bounded polling observes the real worker-owned states in the rendered table:
   `research`, then `script`, then `review` with `awaiting_approval`, including
   research/script/review artifacts. This crosses the real API, RabbitMQ,
   worker, PostgreSQL, and orchestration state machine.
7. Workspace member A sees the review state but no Approve/Publish/Complete
   control. A same-origin browser request attempting the restricted approval
   command receives HTTP 403.
8. Workspace-admin B authenticates in workspace B and sees an empty pipeline
   view. Browser requests for workspace A's pipeline receive HTTP 404 for both
   read and approval attempts.
9. A browser request attempting to create a pipeline with client-controlled
   `stage: publish` and `status: done` receives HTTP 400. A bounded rendered
   refresh proves the original pipeline remains at review/awaiting approval.
10. Admin A sees exactly the supported `Approve` control; unsupported direct
    publish/complete controls and client state controls are absent.
11. The actual API process is stopped. The rendered admin UI exercises its real
    server-error path and shows the connection/load failure message. The API is
    started again and readiness is polled before continuing.
12. Admin approves through the rendered `Approve` button. The worker/publishing
    boundary advances the persisted pipeline through approval, publication, and
    completion. The UI observes `complete`/`done`, publication artifact state,
    and rendered `published` publication state. No browser-side status mutation
    is used.
13. A second browser logs in, replaces its bearer material with stale access and
    refresh values, refreshes Pipelines, and is returned to the rendered login
    view after the real 401/failed-refresh path.
14. Founder logs in, filters the rendered organization audit view for
    `pipeline.advance`, sees persisted audit rows, and verifies the rendered
    hash-chain result (`Chain intact:`).
15. Founder logs out through the real revocation endpoint. Both session values
    are cleared; reusing the saved refresh token receives HTTP 401, and reload
    returns to the login view.

## 3. Rendered state and security coverage

**VERIFIED BY EXECUTION —** the browser test covers the required UI states:

| State | Rendered evidence |
|---|---|
| Loading | pipeline-loading is observed during the initial admin load and each bounded refresh |
| Success | pipeline-created, approval, completion, publication, audit rows, and chain verification messages |
| Empty | isolated workspace A before creation and workspace B after cross-workspace isolation |
| Validation error | required publication title submission exposes the browser validation message |
| Authorization denied | workspace-less Founder sees the real rendered 403 message; member has no restricted control and a real approval request gets 403 |
| Server error | stopped API produces the real UI connection/load failure state, then the stack is restored |
| Session expiry | stale bearer/refresh values cause the real refresh failure and rendered return to login |

Security scenarios include unauthorized restricted action, cross-workspace
read/action denial, stale and invalid session handling, local logout plus
server refresh-token invalidation, rejection of client-controlled pipeline
stage/status, and absence of unsupported actions. The negative API probes are
same-origin `fetch` calls from the real authenticated browser contexts; they do
not mock responses. Positive operations and all state assertions use rendered
controls and DOM state.

## 4. Mutation proof

**VERIFIED BY EXECUTION —** the workflow runs the unmutated browser journey
first. `scripts/browser-e2e-mutation.sh` then makes one temporary UI
authorization mutation in `internal/webui/static/app.js`, changing the
workspace-member approval visibility guard. It creates a fresh fixture, rebuilds
and starts the real API, runs the same required Chromium test, and requires that
the mutated test fail. It then restores the exact source byte-for-byte,
rebuilds the API, and prepares a fresh fixture before returning success.

**CI VERIFIED —** the mutation step succeeded in final browser runs, so the
member approval authorization assertion was caught by the browser test. The
independent Creator/Pipelines matrix also caught and restored the route, RBAC,
duplicate, audit, retry, worker acknowledgment, workspace, approval,
transition, and client-state mutations.

## 5. CI execution evidence

**CI VERIFIED —** on implementation HEAD `8d3e0afbe60fb5f8474316041f7c30b24cdfc2d2`:

- Browser push run `35126868345`: success. Real Chromium setup, API/worker
  readiness, founder bootstrap, isolated fixture, selector guard, DOM journey,
  temporary authorization mutation, cleanup, and shutdown all succeeded.
- Browser pull-request run `35126874497`: success.
- Phase 1 run `35126874501`: success.
- Phase 2 run `35126874498`: success. Its build/vet/unit/race job and full
  Phase 1 + Phase 2 live regression both succeeded, including runtime RLS and
  persistent-audit suites.
- Independent Verification run `35126874537`: success. The independent Go
  security tooling and all ten Creator/Pipelines mutation jobs succeeded.
- Independent push verification run `35126868330`: success.

The final browser job did not merely report a zero-test success: its explicit
Playwright selector guard passed before the DOM journey. The mutation proof
also completed rather than being skipped.

## 6. Defects found and fixes

The first real browser run reached the journey but reported the Founder app as
hidden. This was classified before changing production code: API/worker
startup, isolated provisioning, Chromium installation, selector matching, and
zero-test protection had passed; the failure was authenticated fixture state.
The cause was that configured Founder credentials do not create the Founder
implicitly—the application requires the real unauthenticated bootstrap
endpoint. The workflow now calls that endpoint and verifies HTTP 201 before
browser execution. No production backend logic was changed.

During expansion of browser negative coverage, an added same-origin probe first
used `/api/pipelines`; the protected application routes are `/pipelines` (only
authentication routes carry the `/api` prefix). This was classified as a
browser-test route defect from the 403 diagnostic and corrected without
changing production code. A transient immediate DOM-cell assertion was also
replaced with the existing bounded rendered-state poll. The final push and
pull-request browser executions passed after these exact fixes.

The remaining GitHub Actions Node.js 20 deprecation messages are external
runner-action warnings, not application failures or security findings.

## 7. Required regression and repository gates

**CI VERIFIED —** the final implementation evidence includes Phase 1, Phase 2
(full regression and race detector), independent security tooling, and the
Creator/Pipelines mutation matrix listed above. The Phase 2 live run exercised
real PostgreSQL/pgvector, Redis, RabbitMQ, API, and worker behavior; runtime
role/RLS and persistent audit checks passed.

**VERIFIED BY EXECUTION —** the frozen file
`tests/phase1_exit_criteria_test.go` remains byte-identical to baseline
`6e2ea73c7870d1bd8f028714e58ace1c3aa56c54`; its requested diff is empty.
`git diff --check` passed. Changes were committed in focused commits and pushed
normally to `arena/01a0a5ea-austro-os`; no force push, reset, destructive
cleanup, or history rewrite was used.

## 8. Limitations and disposition

- Local execution of Go/Chromium was unavailable in the agent image; this is
  explicitly not used to weaken the result because real GitHub Chromium runs
  completed.
- The test uses the configured stub AI/publishing adapters in CI so the
  workflow is deterministic, but it still crosses the real HTTP, database,
  queue, worker, approval, publishing, completion, and audit boundaries. It
  does not mock the critical browser journey or replace the worker state
  machine.
- Screenshots, traces, and videos are failure artifacts by design; successful
  runs do not manufacture them. The workflow retains the diagnostic report and
  logs when a run fails.

**Final status: COMPLETE — real browser end-to-end verification passed, the
required browser mutation was caught, and the required regression/security
matrix is green.**
