# ADR-011 — Phase 2 Step 8: Security Hardening

- **Status**: Accepted (implementation-time design, Phase 2 Step 8 — documented before code per ADR-003 §3.1/§3.2 and ROADMAP §8)
- **Date**: Phase 2 Step 8
- **Related**: ROADMAP.md §8 (security impact by capability), §5.8 (secrets backend), §5.9 (rate/cost controls), §9 (testing strategy — security row); CONSTITUTION.md P8 (Minimal Disclosure), P9 (Security by Design), P10 (Privacy by Design), P13 (Observability), P15 (Minimal Disclosure); Phase 1 criterion #23; ADR-009, ADR-010
- **Authorized finalization**: ROADMAP §8 "Cross-cutting: secrets never logged; fail-fast config; trace/span/correlation across async steps" and §9 "Security: deny-by-default on every new route; RLS tests; secrets never logged; rate-limit behavior". This ADR records the cross-cutting hardening layer finalized at implementation.

## Context

Steps 4–7 each added a workspace-scoped capability with its own audit sink,
per-workspace throttle, and deny-by-default authorization. Step 8 **hardens the
shared security posture** that every Phase 2 service relies on, so the same
guards do not have to be re-derived per capability:

1. **Secrets never logged or stored** (P9/P10, criterion #23): any value shaped
   like a signing secret, token, or provider key must be redacted before it can
   reach a log line or an audit field (REDACT, §8).
2. **Minimal disclosure of raw external identifiers** (P8/P15): raw platform or
   external references are stored/emitted only as irreversible hashes where a
   plain value is not needed.
3. **Deterministic rate limiting** (§5.9): a shared, testable per-actor/workspace
   limiter used by publish and pipeline-advance paths.
4. **Deny-by-default preconditions**: a common guard requires a non-empty
   workspace id and a human actor on the appropriate transitions, so a missing
   context fails closed.

All of it lives in `internal/security`, a pure domain package with **no
`infrastructure/` import** (criterion #32) and no new OpenAPI operations.

## Decision — `internal/security` module

`internal/security` provides hardening primitives used by the Phase 2 services:
- `redact.go` — `RedactSecret(value string) string` scans a caller-provided
  string for secret-shaped substrings (opaque bearer/token markers, key-prefixed
  values such as `sk-`/`pk-`/`ghp_`/`AKIA`, PEM/private-key blocks, `=`:hex/base64
  blobs) and replaces them with a fixed marker so nothing that looks like a
  secret reaches a log or audit field. It is a filter, not an authentication
  mechanism: it guarantees the absence of secret-shaped data in the output.
- `hash.go` — `HashExternalID(raw string) (string, error)` returns an
  HMAC-SHA256 one-way digest of a raw external identifier using the configured
  HMAC key, so the plain external reference is never persisted. Deterministic,
  replay-safe.
- `throttle.go` — `Throttler`, a fixed-window per-key limiter (window/limit are
  injected), used by publish and pipeline-advance paths for consistent
  abuse-control behavior (deny-by-default when the limit is hit).
- `guard.go` — `RequireWorkspace(id)` and `RequireHuman(actor)` fail closed on
  empty/invalid preconditions, consolidating the deny-by-default checks.

## Decision — usage across capabilities

- The Publishing service (ADR-009) applies `RedactSecret` to any free-text field
  (title/body are content, but the stub's external reference is passed through
  `HashExternalID` so no raw platform reference is emitted in plain form).
- The Creator orchestration (ADR-010) applies the shared `Throttler` for the
  pipeline-advance rate limit and the same deny-by-default guards.
- Audit/structured-log emission keeps actor/workspace/trace/span; secret-shaped
  values never appear because they are redacted before the record is built.

Phase 2 remains credential-free: there are no real tokens or provider keys
(ROADMAP §5.8), so `RedactSecret` is defense-in-depth against accidental
exposure of anything that *looks* like a secret, and `HashExternalID` prevents
storing raw external references.

## Decision — no schema/API change

Step 8 adds **no database table and no OpenAPI operation**. It is a pure
cross-cutting hardening layer plus tests. The Phase 1 operation-count gate
(`require.Equal(t, 13, spec.totalOps)`) and criteria are untouched.

## Security implications

- Secrets-never-logged is enforced as a filter at the boundary (criterion #23).
- Raw external identifiers are replaced by irreversible hashes (minimal
  disclosure, P15).
- Rate limiting is shared, deterministic, and tested; excess requests are
  denied (abuse control).
- Missing workspace/actor preconditions fail closed (deny-by-default).
- Existing per-vertical RLS, audit, and approval gates are unchanged and remain
  covered by the same tests.

## Consequences

- Adds a dependency-free, pure-domain `internal/security` package.
- Strengthens the audit/log invariants without touching Phase 1 tables or code.
- Provides one consolidated security test suite (redaction, hashing,
  throttling, deny-by-default guards) alongside the existing RLS isolation tests.

## Out of scope (later steps)

- Real provider key infrastructure and a secrets manager (deferred, ROADMAP §5.8).
- Full per-route authz policy engine (authz rules remain per-capability).
- Transport-level security (TLS termination) — not affected by this layer.