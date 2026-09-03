# ADR-008 — Phase 2 Step 5: Workspace-Scoped Memory Completion

- **Status**: Accepted (implementation-time design, Phase 2 Step 5 — documented before migration per ADR-003 §3.1/§3.2 and ROADMAP §6/§7)
- **Date**: Phase 2 Step 5
- **Related**: ROADMAP.md §2.1.3 (memory system), §5.6 (knowledge vs memory ownership), §6 (memory data model), §8 (memory security row), §10 (migration, additive); CONSTITUTION.md P9 (Security by Design), P10 (Privacy by Design), P13 (Observability), P15 (Minimal Disclosure); ADR-007
- **Authorized finalization**: ROADMAP §6 marks memory table/Redis columns and policy names as `[ASSUMPTION]` finalized at implementation and recorded in an ADR before migration. This ADR is that record for memory.

## Context

Phase 2 Step 5 completes the existing 4-layer `internal/memory` system
(Session/LongTerm/Workspace/Organizational, ROADMAP §2.1.3). Phase 1 already
scaffolds a `repository.Repository` port and Redis workspace-partitioned keys
(`workspace:<id>:<key>`). The goal is to wire memory into task/pipeline state
with the isolation, bounds, audit and observability required by the memory
security row (ROADMAP §8): JWT auth, explicit authz, **workspace-partitioned
isolation**, audited events, bounded keyspace, **no secret in memory**,
memory-write throttle, and principles 9,10,13,15.

## Decision — workspace-scoped memory access (deny-by-default)

The existing `internal/memory` layers derive the workspace from the process
singleton (`config.Get().WorkspaceID`). That is a cross-workspace isolation
weakness: a memory read must be scoped to the *caller's* workspace, not a
process-global. This step adds a **workspace-scoped accessor** that takes a
`WorkspaceID` explicitly on every call:

- `internal/memory.Bank` — a single facade over the four layers, keyed by an
  explicit `WorkspaceID` (never `config.Get()`). Memory cells for Session and
  Long-Term layers are stored under `workspace:<workspaceID>:<layer>:<key>`.
  Organizational memory is shared organization-wide (`org:<key>`, no part of
  any one workspace's partition). This makes a workspace-B principal unable to
  derive or read workspace-A keys (deny-by-default, minimal disclosure P15).
- Every mutation is validated at the boundary:
  - empty/blank `WorkspaceID` → `ErrWorkspaceRequired` (deny by default);
  - key bounds: non-empty, max length, must not be an already-partitioned key
    (reject nested `workspace:`/`org:` prefixes to avoid key confusion);
  - value bound: maximum bytes; **no secret**: memory refuses
    secret-shaped content (a config-flagged secret marker) — storage of
    credentials/tokens in memory is rejected (no secret in memory, ROADMAP §8);
  - TTL bound: non-negative; capped at an upper bound.
- Every write/delete emits an audit-compatible record and a structured JSON log
  with trace/span correlation (P13) and a constitutional principle (P9 security,
  P10 privacy, P15 disclosure). A memory-write throttle is applied per workspace
  at the facade boundary (rate-limit behavior, ROADMAP §9).

## Decision — memory data model (additive, no new table)

Memory is ephemeral/derived state (ROADMAP §5.6) and keeps no new table; it
reuses the Phase 1 Redis workspace-partitioned keyspace and the existing
`vector_embeddings`/`memory_embeddings` PGVector store for embeddings. No Phase 1
table is altered. This keeps Step 5 additive (ROADMAP §10) and preserves the
"knowledge and memory remain separate stores" rule (ROADMAP §5.6).

## Decision — API contract (`/memory`, finalized before endpoints)

Per ADR-003 §3.2 the contract is finalized here:

- `GET /memory/{layer}/{key}` → `readMemory` (bearerAuth; principles: Self-Ownership / Security by Design).
- `PUT /memory/{layer}/{key}` → `writeMemory` (bearerAuth; principles: Security by Design, Privacy by Design).

As with ADR-006/ADR-007 these are implemented at the **service boundary** this
step; HTTP routing is deferred to keep the Phase 1 OpenAPI operation-count gate
(`require.Equal(t, 13, spec.totalOps)`) untouched. The contract remains
authoritative for later wiring.

## Security implications

- Deny-by-default: no global `config.Get().WorkspaceID` in the workspace-scoped
  path; every call requires an explicit `WorkspaceID`.
- No secret retention: memory rejects secret-shaped content; nothing secret is
  logged.
- Rate limiting: per-workspace memory-write throttle at the facade boundary.
- Audit: every write/delete carries a principle-tagged decision; reads emit a
  lightweight audit where scoped.
- Structured JSON logs with trace/span/correlation; no plain-text logging.
- Key bounds prevent partiton-prefix confusion and oversized keys/values.

## Consequences

- Completes the 4-layer memory facade with explicit workspace isolation.
- No new table; additive reuse of Redis partitioning and the PGVector store.
- Keeps Phase 1 green: additive, no Phase 1 table/code modified, criteria #16/#31
  and dependency direction respected.

## Out of scope (later steps)

- Real per-provider cost caps (ADRs 003/005/007; stubs cost nothing).
- Persisting long-term memory to a durable relational table (roses memory as
  ephemeral per ROADMAP §5.6).
- HTTP wiring of `/memory` (deferred to keep the OpenAPI gate green).