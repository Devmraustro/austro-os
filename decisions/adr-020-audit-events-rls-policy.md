# ADR-020 — Audit Events RLS Policy (Current Authoritative Decision)

- **Status**: Accepted
- **Date**: Phase 3 — audit integrity reinforcement
- **Related**: ADR-007 (RLS), ADR-003 (final approval and decision lock), ADR-018 (superseded exception), CONSTITUTION.md P9 (Security by Design), P10 (Privacy by Design)
- **Supersedes**: ADR-018

## Context

The audit_events table stores the complete audit trail for the AUSTRO OS system, including workspace-scoped operations, founder actions, and system transitions. Initially, audit_events was configured as an exception to the standard workspace-isolation RLS policy (see ADR-018), meaning it did not enforce per-workspace row-level security. However, as the workspace-scoped RBAC model matured and the need for strict tenant isolation grew, it became clear that the audit_events exception created a security boundary gap.

## Decision — audit_events now follows standard workspace-isolation RLS policy

The `workspace_isolation_policy` is now applied to the `audit_events` table, using the standard workspace_id-based filter:

```sql
CREATE POLICY workspace_isolation_policy ON audit_events
    USING (workspace_id = current_setting('app.current_workspace', true)::UUID);
```

This policy is enabled alongside the table's ENABLE ROW LEVEL SECURITY, making audit_events consistent with the isolation guarantees provided to `tasks`, `knowledge_documents`, `publications`, and `pipelines`.

## Rationale

- **Tenant isolation**: Workspace-restricted roles (workspace_a_user, workspace_b_user) can only observe audit events belonging to their assigned workspace, preventing cross-workspace data leakage through the audit trail.
- **Defense-in-depth**: RLS on audit_events provides database-layer isolation complementary to application-layer access controls.
- **Compliance**: Audit data perworkspace boundaries align with regulatory requirements for data residency and tenant separation.
- **Consistency**: All workspace-scoped tables now follow the same RLS pattern, reducing cognitive complexity and the risk of forgotten exceptions.

The policy uses the existing `app.current_workspace` setting, which is set at connection time based on the authenticated user's workspace context. Events with no workspace_id or with a NULL workspace_id are excluded from restricted role query results, which is the standard behavior for this policy.

## Consequences

- **Positive**: Workspace-restricted roles can no longer query audit events from other workspaces. This closes a cross-workspace information leakage vector.
- **Positive**: Audit integrity is strengthened; the audit trail respect the same isolation guarantees as other workspace-scoped data.
- **Positive**: Simplified security model — all RLS-protected tables follow the same policy name (`workspace_isolation_policy`).
- **Negative**: Applications that previously relied on cross-workspace audit event querying (via restricted roles) must be updated to use administrator roles or explicit workspace filtering.
- **Negative**: The "exception" rationale documented in ADR-018 is no longer valid; the audit_events table now enforces workspace isolation.
- **Mitigation**: Founder/organization-level roles continue to have unrestricted access via the `current_setting('app.current_workspace', true) = ''` OR `is_founder` guard pattern, consistent with how other tables handle founder visibility.

## Migration

Existing deployments must apply the workspace_isolation_policy to the audit_events table. The migration is additive and idempotent:

```sql
ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation_policy ON audit_events
    USING (workspace_id = current_setting('app.current_workspace', true)::UUID);
```

This can be deployed as a Phase 3 migration step without downtime, as the policy is checked on every SELECT operation and the default behavior (no policy matching) preserves existing read access until the policy is actively in place.
## Implementation status

This decision was recorded but not implemented: `audit_events` had no
`workspace_id` column, no row level security, and no rows — nothing wrote to it.
It is implemented by ADR-021, with two refinements recorded there: `workspace_id`
is nullable so organization-level authentication events stay reachable, and a
second policy restricted to the administrative role gives chain verification the
whole chain. The policy is named `audit_workspace_policy`, so the count of
`workspace_isolation_policy` declarations remains ten.
