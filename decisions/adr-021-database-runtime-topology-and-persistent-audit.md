# ADR-021 — Database Runtime Topology and Persistent Audit

- **Status**: Accepted
- **Date**: Phase 3 — release-candidate hardening
- **Related**: ADR-007 (RLS), ADR-018 (superseded audit exception), ADR-020 (audit_events RLS policy), CONSTITUTION.md P9 (Security by Design), P10 (Privacy by Design)

## Context

Two defects survived a fully green test suite.

**The workspace policies constrained nothing.** The application connected to
PostgreSQL as the table owner. PostgreSQL applies row level security to every
role *except* a table's owner, a superuser, and a role holding `BYPASSRLS`, so
for the only connection that ever served a request, every policy in the schema
was inert. The integration suite stayed green because it creates separate
non-owner roles (`workspace_a_user`, `workspace_b_user`) and asserts isolation
from their point of view — which is precisely the shape of evidence that hides
the defect. There was no `FORCE ROW LEVEL SECURITY` anywhere in the repository,
and no check anywhere that the process had connected as an unprivileged role.

**The audit trail did not exist at runtime.** `audit_events` was created on
every boot and never written to. `internal/audit` implemented hash chaining and
HMAC verification correctly and had no production caller; every "audit" was a
structured log line. A log line is not append-only, is not tamper-evident, and
does not survive a rotation.

## Decision — three database principals

| Principal | Credential | Purpose |
|---|---|---|
| **owner** | `AUSTRO_POSTGRES_DSN` | Creates extensions, tables, indexes, policies and roles. Owns the tables. Closed before the process serves a request. |
| **runtime** | `AUSTRO_POSTGRES_RUNTIME_DSN` | Every request. Not a superuser, no `BYPASSRLS`, owns nothing. |
| **admin** | `AUSTRO_POSTGRES_ADMIN_USER` (default `<runtime>_admin`) | Two narrow organization-level operations. Also not a superuser and no `BYPASSRLS`. |

Nothing in the bootstrap grants superuser or `BYPASSRLS`. A principal holding
either would make every policy decorative.

The **admin** role exists because two operations cannot be expressed by a
workspace-scoped policy:

1. A founder listing or creating workspaces has no workspace to be scoped to.
2. Verifying the audit chain requires reading the whole chain, which a
   workspace-scoped reader cannot do.

Its wider reach comes from policies that name it — `CREATE POLICY ... TO
<admin> USING (true)` on `workspaces` and `audit_events` — never from skipping
row level security. It is not a member of the runtime role and the runtime role
is not a member of it, so no session state set by application code can widen
the runtime role's view: the escalation lives in a credential, not in a setting
a code path could leave behind on a pooled connection.

### FORCE ROW LEVEL SECURITY

`FORCE` is applied to all twelve protected tables. `ENABLE` alone leaves the
owner unconstrained, which is invisible in normal operation and becomes a total
isolation failure the moment any code path, migration or operator session uses
the owner credential for traffic. `FORCE` applies the policies to the owner as
well.

`FORCE` does not constrain a superuser or a `BYPASSRLS` role — neither can be
constrained by any policy. That is why the runtime role is created without
either, and why the absence of both is asserted on the live connection rather
than trusted from the bootstrap.

A useful side effect: superusers are still unaffected, which is what keeps the
administrative test fixtures working. They connect as the superuser and perform
unbound seeding inserts that a forced policy would otherwise hide.

### Startup verification

Before serving a request, the runtime pool is checked against the live
database, and any mismatch is fatal:

- the connected role is the configured runtime role;
- it is not a superuser and does not hold `BYPASSRLS`;
- it owns none of the twelve protected tables;
- every protected table has row level security both enabled and forced;
- a canary row committed in workspace A is invisible to a session bound to
  workspace B, while B's own row is visible.

The last check needs a committed row. Doing it inside one rolled-back
transaction would prove nothing, because the second read would be filtered by
the rollback rather than by the policy.

Configuration rejects using the owner DSN as the runtime DSN. When the runtime
DSN is unset it is *derived* from the owner DSN onto `austro_app`, so a
deployment that configures nothing still lands on the unprivileged path.

### Self-healing

The bootstrap re-asserts role attributes, grants and table ownership on every
boot. An operator who grants `BYPASSRLS` by hand, or a migration tool that
moves table ownership, does not leave the process unprotected after a restart.
The startup verification still runs afterwards, so a repair that could not be
applied is fatal rather than silent.

## Decision — persistent audit

`infrastructure/auditstore` writes to `audit_events` and maintains the chain:

- one transaction per append, serialised by `pg_advisory_xact_lock`, so
  concurrent writers in this process or another cannot fork the chain;
- the head is re-read under that lock, because the in-memory head is a cache;
- a restarted writer recovers the head from the table; only an empty table
  produces a genesis event, so a restart never forks the chain;
- the timestamp is stored twice. A PostgreSQL timestamp column carries
  microsecond precision while the chain pre-image binds RFC 3339 with
  nanoseconds, so reading the column back alone would recompute a different
  hash and report an intact chain as tampered;
- both the runtime and admin roles are append-only on the table. Rewriting
  history requires a superuser or the owner, which is exactly the situation the
  hash chain and HMAC tag exist to make detectable.

Wired into: bootstrap, login, refresh (with replay distinguished from an
ordinary rejection), logout, authorization denials, workspace creation, and the
publishing and orchestration decision sinks.

**Fail closed.** Authentication and workspace creation return 500 if the record
cannot be persisted. An action that cannot leave a durable audit record must not
report success. The publishing and orchestration sinks cannot fail their
operation, because `publish.AuditSink` and `orchestration.AuditSink` return
nothing; widening those interfaces touches every implementation and caller and
is recorded as a known limitation.

**Redaction.** Detail keys matching a credential-shaped denylist are replaced
with `[redacted]` before anything is written. Values are never inspected and
never copied into an error path.

## ADR-020 refinements

ADR-020 mandated `workspace_isolation_policy` on `audit_events`. That is now
implemented, with two deliberate differences from the SQL quoted there:

1. `workspace_id` is nullable, and organization-level events (bootstrap, login,
   refresh replay) have none. The policy is
   `workspace_id IS NULL OR workspace_id = current_setting(...)`, so
   authentication history stays reachable from the session it belongs to
   instead of being invisible to every workspace-scoped reader.
2. A second policy `TO <admin> USING (true)` gives chain verification the whole
   chain. Without it no role could read a chain, and a chain nobody can read
   cannot be verified.

The policy is named `audit_workspace_policy` rather than
`workspace_isolation_policy`, keeping the count of `workspace_isolation_policy`
declarations at ten.

## Consequences

- Two new required settings for deployments: `AUSTRO_POSTGRES_RUNTIME_PASSWORD`
  (docker-compose refuses to start without it) and, where the derived name is
  unsuitable, `AUSTRO_POSTGRES_RUNTIME_USER` / `AUSTRO_POSTGRES_RUNTIME_DSN`.
- The owner credential must be able to create roles (`CREATEROLE` or
  superuser). This is a bootstrap-time requirement only.
- The users policy no longer makes every identity visible to an unbound
  session. Identity resolution is narrowed to the single principal named by
  `app.auth_principal`, bound transaction-locally by the user store; an
  unbound session otherwise sees only founders.
- Every request now runs under row level security. A query that previously
  relied on being unconstrained will return no rows instead of all rows — which
  is the intended failure direction.
