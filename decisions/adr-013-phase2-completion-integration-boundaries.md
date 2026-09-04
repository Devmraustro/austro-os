# ADR-013 — Phase 2 Completion: Closing the External-Service Integration Boundaries

- **Status**: Accepted (documented before irreversible implementation, per ADR-003 §3.1/§3.2 and the project-completion directive)
- **Date**: Phase 2 completion
- **Related**: ROADMAP.md §5.8/§5.9 (deferred provider keys / publishing tokens), §2.2/§2.3 (deferred items); PROJECT_SCOPE.md "In Scope: Integrations"; CONSTITUTION.md P6 (Replaceability), P8 (AI Independence), P9 (Security by Design), P11 (Human Oversight); ADR-004 (AI gateway), ADR-009 (publishing), ADR-011 (security hardening)

## Context

Phase 2 correctly isolated the two external-service boundaries behind replaceable
ports (`internal/ai.Provider`, `internal/publish.Publisher`) and shipped only
deterministic, offline stub adapters. The audit that closed Phase 2 found two
gaps that prevent a deployment from genuinely exercising those boundaries:

1. There was **no config-driven adapter selection**: neither boundary had any
   configuration fields or factory; a real provider/publisher could not be
   selected or wired without changing domain code.
2. There was **no strict validation** of external-boundary settings: a real
   backend could be requested half-configured, or a real credential could be
   supplied while the stub (offline) adapter silently ignored it.

Per the completion directive, these boundaries are now **closed**: the full
integration shape (strict config schema, fail-fast validation, config-driven
adapter factories, and real HTTP adapters) is implemented now using the existing
ports, while the **external values** (API key, delivery token, endpoints) remain
`CONFIGURATION_REQUIRED` and are never needed for, nor committed to, the
repository.

## Decision — strict, additive config schema for the boundaries

`internal/config.Config` gains a small, additive set of settings with **no
insecure fallback** (the master-rule: never env-fallback a secret, never log a
secret). The default backend for each boundary remains the offline stub, so
every existing deployment continues to run unchanged (P14 Backward
Compatibility).

- AI boundary: `AUSTRO_AI_BACKEND` (`stub` | `openai-compatible`), `AUSTRO_AI_MODEL`,
  `AUSTRO_AI_BASE_URL`, `AUSTRO_AI_API_KEY`, `AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE`.
- Publishing boundary: `AUSTRO_PUBLISH_BACKEND` (`stub` | `generic-http`),
  `AUSTRO_PUBLISH_WEBHOOK_URL`, `AUSTRO_PUBLISH_TOKEN`.

## Decision — fail-fast, fail-closed validation

`Config.Validate()` (used by `LoadStrict`) now enforces cross-field rules:

- Selecting a real backend (`openai-compatible`, `generic-http`) is
  **`CONFIGURATION_REQUIRED`**: the corresponding model/endpoint/key (or
  webhook/token) must be present and secure, or startup fails fast. Errors name
  the offending setting only; the secret value is never echoed.
- Selecting the stub backend while any external credential is supplied is
  **rejected**, so a configured secret is never silently ignored.
- An unrecognized backend selector fails fast (named setting), never degrading
  silently.

## Decision — config-driven adapter factories + real HTTP adapters

- `internal/ai.NewProvider(ProviderConfig)` returns `StubProvider` for `stub`
  and an `OpenAICompatibleProvider` for `openai-compatible`, failing closed on
  unknown backends or missing credentials.
- `internal/publish.NewPublisher(PublisherConfig)` returns `StubPublisher` for
  `stub` and a `GenericHTTPPublisher` for `generic-http`, with the same
  fail-closed guarantees.
- The real adapters implement the existing ports (no caller changes) and attach
  the credential only to the outbound `Authorization` header — it is held in
  memory and never logged or returned. Both retain the boundary's workspace
  scoping and (for publishing) the Human Oversight approval gate as a second
  in-adapter guard.

## Security / verification implications

- No new OpenAPI operations, no new secrets in the repository, no changes to
  Phase 1 code, gate, or workflows; additive only.
- The default (stub) adapters remain the contract used by all offline unit and
  integration tests, so the existing suite is unaffected.
- New unit tests (positive + negative) cover: factory selection and fail-closed
  behavior; strict cfg validation of the boundary settings (including the
  never-echo-a-secret rule); and the real HTTP adapters against `httptest`
  servers (auth header, payload, refusal of unapproved publications and missing
  workspaces, and never-leak-on-error).

## Consequences

- A deployment can now genuinely select and wire a real AI or delivery backend
  by supplying the `CONFIGURATION_REQUIRED` settings, without any domain change.
- Strict validation hardens the entrypoint so a misconfigured or half-configured
  external boundary fails at startup instead of at call time, and a silently
  ignored credential is impossible.
- The stub adapters remain the deterministic default, preserving offline
  development and test determinism.

## Out of scope (documented, not implemented)

- Wiring these factories into `main.go` runtime composition and exposing the AI
  gateway over HTTP (explicitly deferred per ADR-004; boundary delivery only).
- Real platform-specific delivery adapters (e.g. a specific platform's upload
  API) — the generic HTTP adapter is the integration boundary; concrete
  platform SDKs are future work and remain `INTENTIONALLY_DEFERRED`.
- Analytics, Marketplace, and the remaining Phase 3+ roadmap items, which remain
  deferred by frozen decision.
