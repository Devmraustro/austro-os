# AUSTRO OS — AlwaysData Release Notes

## Release: 224bb24089ccbf74dc08ce9c6b3831918c4157fb

**Date**: 2024-09-25  
**Source**: `main` branch, commit `224bb24089ccbf74dc08ce9c6b3831918c4157fb`  
**GitHub Actions Run**: Phase 2 CI (x86_64 build)

## Summary

This release adds support for **pre-provisioned PostgreSQL roles**, enabling
deployment to managed PostgreSQL providers (AlwaysData, AWS RDS,
Cloud SQL, etc.) where the application cannot create or alter roles.

## Changes

### New Feature: Pre-Provisioned PostgreSQL Roles

Added support for deploying to managed PostgreSQL providers (AlwaysData, AWS RDS,
Cloud SQL, etc.) where the database administrator pre-creates the required
roles and the application only needs to connect with the provided credentials.

**New Environment Variables:**
- `AUSTRO_POSTGRES_PREPROVISIONED_ROLES=true` — Enables pre-provisioned mode
- `AUSTRO_POSTGRES_ADMIN_DSN` — Explicit admin DSN (required in pre-provisioned mode)

**Behavior in Pre-Provisioned Mode:**
- Skips `CREATE ROLE` / `ALTER ROLE` for runtime and admin roles
- Verifies pre-existing roles have correct attributes:
  - `LOGIN`
  - `NOT SUPERUSER`
  - `NOBYPASSRLS`
  - `NOT CREATEROLE`
  - `NOT CREATEDB`
  - `CAN LOGIN`
- Skips password management (passwords managed externally)
- Still performs grants/schema/RLS setup required by AUSTRO
- Verifies admin role authentication

**Configuration Validation:**
- Fails fast if `AUSTRO_POSTGRES_PREPROVISIONED_ROLES=true` but:
  - `AUSTRO_POSTGRES_RUNTIME_DSN` is not set
  - `AUSTRO_POSTGRES_ADMIN_DSN` is not set
  - Runtime DSN equals owner DSN
  - Admin DSN equals owner DSN
  - Runtime DSN equals admin DSN

### CI/CD Improvements

- **Phase 2 CI** now uploads x86_64 release binaries (`austro-x64-release`)
  as a downloadable artifact for AlwaysData deployment
- Linux AMD64 binaries built on every PR and push to main
- Binaries verified for x86_64 architecture and static linkage

### Bug Fixes

- Fixed flaky `TestKnowledgeCreatedAtIsStableAcrossUpdates` on Windows
  (added 10ms sleep for timer resolution)

### Documentation

- Added AlwaysData deployment documentation to `.env.example`
- Added `AUSTRO_POSTGRES_PREPROVISIONED_ROLES` and `AUSTRO_POSTGRES_ADMIN_DSN`
  to configuration documentation

### Binaries

| Binary | Architecture | Build Commit | SHA256 (from CI) |
|--------|--------------|--------------|------------------|
| austro-api | linux/amd64 | 224bb24089ccbf74dc08ce9c6b3831918c4157fb | See CI artifact |
| austro-worker | linux/amd64 | 224bb24089ccbf74dc08ce9c6b3831918c4157fb | See CI artifact |

### Verification

- All CI workflows pass on main branch
- Frozen gate `tests/phase1_exit_criteria_test.go` SHA-256: `9119aa71aa1cd95eda7fa0aeabf14042a960770ce4ea1e3fc896f6f7ec4b0a67`
  - Previously pinned at `0a868d3da41f175cc263a6301f806e3bc1125093dab9cf8db98bb17b54e8769a`, which
    matched the file at every commit in history. Re-frozen under approval for a single-line
    Windows path-separator correction in subtest 16; the full rationale is recorded above
    `FROZEN_SHA` in `scripts/verify-production-deployment.sh`.
- Phase 1, Phase 2, Production Deployment, Native ARM64, Organization, Creator/Pipelines, Browser E2E all passing

### Deployment

See `deploy/alwaysdata/RUNBOOK.md` for deployment instructions.