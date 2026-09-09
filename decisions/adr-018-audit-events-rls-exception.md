# ADR-018 — Audit Events RLS Exception (Superseded)

- **Status**: Superseded
- **Date**: Phase 2 — initial audit infrastructure design
- **Related**: ADR-007 (RLS), ADR-003 (final approval and decision lock), ADR-020 (current authoritative decision), CONSTITUTION.md P9 (Security by Design), P10 (Privacy by Design)
- **Superseded by**: ADR-020

## Context

The audit_events table was designed as a append-only log for application audit trails. During initial design, it was decided that audit_events would be exempt from workspace-scoped Row-Level Security (RLS), because audit events need to be visible across workspaces for compliance, security monitoring, and cross-workspace analytics. The table stores globally-significant events (founder actions, system transitions) that should not be siloed per-workspace.

## Decision — audit_events as exception outside normal RLS

The audit_events table was intentionally configured **without** workspace-isolation RLS policy. Instead, access to audit events was controlled at the application layer through administrator roles and explicit query filtering. The `workspace_isolation_policy` was not applied to audit_events, meaning any authenticated role could potentially query all audit events regardless of workspace context.

This exception was documented to explain why audit_events does not follow the standard workspace isolation pattern applied to `tasks`, `knowledge_documents`, `publications`, and `pipelines`.

## Superseded by ADR-020

The previous exception for audit_events is superseded by ADR-020, which establishes that audit_events should follow the standard workspace-isolation RLS policy. The historical rationale for the exception (cross-workspace compliance visibility) is addressed through alternative means in the newer decision (see ADR-020).

**This decision is superseded.** Do not reference the exception configuration from ADR-018 for new implementations; instead consult ADR-020 for the current authoritative position.

## Consequences

- The `workspace_isolation_policy` is NOT applied to `audit_events` per this superseded decision.
- Access to audit events was controlled at the application layer only.
- Cross-workspace audit visibility required explicit administrator bypass.
- This configuration is now obsolete; migrate to the ADR-020 model.