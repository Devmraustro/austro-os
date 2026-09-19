# AUSTRO OS — Free Deployment Certification (Native ARM64)

**Scope:** the zero-container deployment layer in `deploy/native/` and the CI
workflow `.github/workflows/native-arm64-deploy.yml`, built alongside the
primary Docker Compose path
([FINAL_PRODUCTION_READINESS_REPORT.md](FINAL_PRODUCTION_READINESS_REPORT.md)
is the Compose-path certification).

**Date of this report:** 2026-09-19.

**Verdict: REPOSITORY READY — the deployment layer is complete, the release is
proven buildable and CI-proven in structure, and nothing that touches a real
VM has been run. Live deployment stages are `OPERATOR CONFIGURATION REQUIRED`.**

This document certifies per capability. The vocabulary of
`FINAL_PRODUCTION_READINESS_REPORT.md` is used identically: `PASS`, `VERIFIED`,
`BLOCKED`, `FAIL`, `CONFIGURATION REQUIRED`, `NOT TESTED`, `NOT SUPPORTED`,
plus the deployment-stage statuses `REPOSITORY READY`, `STAGING VERIFIED`,
`DEPLOYMENT AUTOMATION VERIFIED`, `LIVE VM VERIFIED`, `PRODUCTION LIVE`, and
`OPERATOR CONFIGURATION REQUIRED`.

Designed for the provider's Always Free limits; subject to provider policy,
availability, and quota changes.

---

## 1. Deliverables

| Deliverable | Status |
|---|---|
| `deploy/native/` — README, env template, generated-env helper, API + worker units, bootstrap, install/rollback, nginx conf, healthcheck, backup, restore, monitor | `REPOSITORY READY` — 12 files, all created and locally validated |
| `.github/workflows/native-arm64-deploy.yml` | `REPOSITORY READY` — build/verify job + gated deploy job; PR-triggered validation pending CI on this change |
| `docs/free-native-deployment.md` | `REPOSITORY READY` |
| `docs/native-operations.md` | `REPOSITORY READY` |
| `docs/native-troubleshooting.md` | `REPOSITORY READY` |
| `docs/FREE_DEPLOYMENT_CERTIFICATION.md` | This file |
| `docs/deployment-targets.md` | Extended with the native target |

## 2. Release build

| Item | Result | Evidence |
|---|---|---|
| `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` build of `main` (API) | **VERIFIED** | Cross-built locally with the Go 1.25.13 toolchain |
| Same for `./cmd/worker` | **VERIFIED** | Same session |
| Both binaries are AArch64 ELF executables | **VERIFIED** | `file` reports `ARM aarch64`; `readelf -h` Machine `AArch64` |
| Both binaries statically linked | **VERIFIED** | No `INTERP` segment in either |
| Fully hermetic and deterministic | **REPOSITORY READY** | CI job pins Go `1.25.13`, `-trimpath`, and re-asserts the three checks above on every run; CI evidence replaces local evidence once this change is merged |

## 3. Deployment-layer validation

| Check | Local result | CI result |
|---|---|---|
| `bash -n` on all 7 `deploy/native/*.sh` | **PASS** | **PASS** (workflow re-runs it) |
| shellcheck `--severity=error` on the operator scripts | **BLOCKED locally** (no shellcheck on the authoring host) | **PASS — VERIFIED in CI** after this change lands |
| `nginx -t` against the shipped conf with real TLS material | **BLOCKED locally** (no nginx on the authoring host) | **VERIFIED in CI** — the workflow generates self-signed material and load-tests |
| Banned-technology scan (per the frozen Phase-1 gate's `walkAllText`) | **PASS** | re-checked in CI |
| Secret-shape scan over `deploy/native/` and the new workflow | **PASS** | **PASS** (workflow re-runs it) |
| No `.env`/`.pem`/`.key` files tracked by git | **PASS** | repo-wide check already exists in the Compose path's verification script |
| Env contract: all REQUIRED variables in `deploy/native/austro.env.example`, loopback addresses not rejected by `isInsecure`, runtime DSN derived/overridden correctly | **PASS** | — |
| Frozen gate `tests/phase1_exit_criteria_test.go` byte-identical | **PASS** (sha256 `0a868d3d…8769a`) | asserted by existing CI |

## 4. Deployment stages (the parts that need a real host)

| Stage | Status | Gate |
|---|---|---|
| Provision a VM (OCI Always Free ARM64, Ubuntu 24.04) | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | provider account + VM |
| `bootstrap-ubuntu24.sh` on that VM | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | VM reachable by SSH |
| `/etc/austro/austro.env` generated and backed up | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | SSH + the env contract |
| TLS material placed, nginx `-t`, nginx enabled | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | certificates |
| `install-release.sh` with a CI-built archive | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | VM + a release archive |
| `healthcheck.sh` → PASS on the live host | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | VM fully provisioned |
| `backup.sh` / `restore.sh` cycle on the live host | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | a live database to save and restore |
| `monitor.sh` → `alert_delivered`, debounced | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | webhook endpoint |
| CI `deploy` job (environment `native-prod`, `DEPLOY_ENABLED`) | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` | 4 environment secrets + repo variable |
| HTTPS serving real traffic | `PRODUCTION LIVE` — not yet; a strictly later stage | `healthcheck.sh` PASS + certificates |

Until every `OPERATOR CONFIGURATION REQUIRED` row above has actually been
performed and evidenced, this deployment is **not** certified `DEPLOYMENT
AUTOMATION VERIFIED` end-to-end and is **not** certified `PRODUCTION LIVE`. No
step in this report fabricates a VM result.

## 5. Deliberately not covered

- Not a replacement for the Compose path; the Compose path remains the primary
  deployment unit and its certification is unchanged.
- No horizontal scaling, no higher-tier shapes, no GPUs, no container
  orchestration platform (see [deployment-targets.md](deployment-targets.md)).
- Certificate renewal and DNS are operator-managed and not automated here.

## 6. How to change the verdict

To move any `OPERATOR CONFIGURATION REQUIRED` row to `VERIFIED`, an operator
must run the corresponding `deploy/native/*.sh` step on a real VM and record
the actual output of that step (not a paraphrase). The next report update must
name the person, the VM identity, and the live evidence for every promoted
stage.