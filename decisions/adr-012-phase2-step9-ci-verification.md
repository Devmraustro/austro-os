# ADR-012 — Phase 2 Step 9: Phase 2 CI + Regression Verification

- **Status**: Accepted (implementation-time design, Phase 2 Step 9 — documented per ADR-003 §3.1/§3.2 and ROADMAP §9/§10)
- **Date**: Phase 2 Step 9
- **Related**: ROADMAP.md §9 (testing strategy, Phase 2 CI), §10.6 ("Separate `phase2` CI workflow; never replace the Phase 1 gate"), §12 (exit criteria); CONSTITUTION.md P13 (Observability), P17 (Explicit Decisions); Phase 1 workflow `.github/workflows/phase1-exit-criteria.yml`
- **Authorized finalization**: ROADMAP §9 "Phase 2 CI: a dedicated phase2 workflow; the Phase 1 phase1-exit-criteria.yml gate is never replaced" and implementation order step 9. This ADR records the Phase 2 CI + regression verification design.

## Context

Step 9 delivers the verification harness for Phase 2. The Phase 1 gate workflow
(`phase1-exit-criteria.yml`) is immutable and must never be replaced, skipped,
or weakened (ADR-001; ROADMAP §10.6). Phase 2 therefore needs a **dedicated,
additive `phase2` workflow** that runs the Phase 2 build, vet, unit, and
integration/regression gates, and a repeatable local verification script that
reproduces the same commands the CI runs.

## Decision — dedicated `phase2` CI workflow

A new `.github/workflows/phase2-ci.yml` with two jobs:

1. **`vet-build-unit`** (fast, no external services):
   - `go build ./...`
   - `go vet ./...`
   - `go test ./internal/...` (the pure domain packages — task, knowledge,
     memory, publish, orchestration, security, ai, etc. — which are independent
     of external infrastructure and run deterministically offline).
   - Runs under Go 1.22 with `GOFLAGS=-mod=mod`, `GOTOOLCHAIN=local`. Modules
     are fetched through the default Go module proxy (CI has no pre-mounted
     cache, unlike the local image's `-mod=mod` + `GOPROXY=off` convenience).

2. **`regression`** (full Phase 2 + Phase 1 regression against a live stack):
   - Spins up Postgres, Redis, and RabbitMQ as service containers.
   - Runs `go test ./tests/ -count=1 -timeout 600s` with the regression
     `AUSTRO_*` environment pointing at the service containers, producing all
     Phase 1 (33 criteria + 61 tests) and Phase 2 tests green.
   - The suite first performs API warm-up health calls (`/health/live`,
     `/health/ready`, expect 200) when the API container is present, matching the
     health-criteria test preconditions.

The existing `phase1-exit-criteria.yml` workflow remains untouched and runs as a
separate check; the `phase2` workflow never replaces it (ROADMAP §10.6).

## Decision — integration-test gating via job split

The DB/Redis/RabbitMQ-backed tests in `tests/` require a live stack; that is why
they are isolated into the `regression` job rather than the `vet-build-unit`
job. The unit packages under `internal/` are infrastructure-free, so the fast
job runs deterministically without services while the regression job exercises
the full stack with the `tests/` suite.

## Decision — local regression script

A repeatable `scripts/phase2-verify.sh` (and a compose profile) reproduces the
exact CI commands against the docker-compose network, so a developer (or the
exit-gate in Step 10) can run the identical verification locally. The script:
1. Waits for the services and warms the API health endpoints (expect 200).
2. Runs `go build ./...`, `go vet ./...`.
3. Runs `go test ./internal/...` (unit) and `go test ./tests/ -count=1
   -timeout 600s` (full stack regression) with the regression `AUSTRO_*` env
   (matching the regression env block used throughout the suite), asserting all
   tests pass.
4. Runs the repo scanner checks (banned tokens, criteria #31/#32) that the Phase
   2 work must not violate.

## Security / verification implications

- No new OpenAPI operations, no new secrets, no changes to Phase 1 code or
  workflows; additive only (P14 Backward Compatibility).
- Every Phase 2 exit criterion (§12) has a corresponding automated check: pipeline
  end-to-end (§12.1), build/vet/test exit 0 (§12.2), workspace-B isolation
  (§12.3), human-approval/rejection (§12.4), provider replaceability (§12.5),
  trace/span + JSON logs (§12.6), secrets-never-logged + RLS policies (§12.7),
  100% Phase 1 regression (§12.8), and deferred-bound respect (§12.11).

## Consequences

- Adds an additive, dedicated `phase2` CI workflow and a local verification
  script; the Phase 1 gate is untouched.
- Provides a single, repeatable verification entry point for the Step 10 exit
  gate and for ongoing development.
- Keeps the monolith and the Phase 1 baseline green.

## Out of scope (later steps)

- Publishing this workflow to a live CI provider run (it is committed for CI to
  consume; local verification here is via the script).
- Any change to the immutable Phase 1 gate workflow or criteria.