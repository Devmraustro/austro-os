# ADR-015 — Free-First Completion: Keyless Local AI Provider and Queue Default Alignment

- **Status**: Accepted
- **Date**: Free-first completion stage (post runtime-reliability hardening)
- **Related**: ADR-004 (AI gateway), ADR-013 (integration boundaries), ADR-014 (runtime reliability); CONSTITUTION.md P6 (Replaceability), P8 (Security), P14 (Backward Compatibility); PRODUCTION_READINESS_REPORT.md

## Context

The founder has no paid AI account yet, and the completion directive requires
the whole system to be fully runnable in stub/local/offline mode against zero
paid credentials, with any paid-provider activation a later configuration-only
step. Previously the only real AI backend (`openai-compatible`) made an API key
mandatory (CONFIGURATION_REQUIRED). That is correct for SaaS providers but
excludes a very common zero-cost option: a local or in-house model server that
speaks the OpenAI-compatible protocol and needs no credential at all.

A second, smaller gap surfaced during the activation audit: the API entrypoint
resolved the events queue name with `queueFor` (defaulting to `austro.events`),
while the worker entrypoint passed `cfg.RabbitMQQueue`* raw into both its
reconnecting sink and its consumer. If `AUSTRO_RABBITMQ_QUEUE` were ever unset,
the API would publish to `austro.events` while the worker consumed a server-named
queue, silently breaking the cascade.

## Decision — new "local" AI backend (no key required)

Introduce `AUSTRO_AI_BACKEND=local` (`config.AIBackendLocal`,
`ai.BackendLocal`) selecting a shared OpenAI-compatible adapter
(`ai.NewLocalProvider`) where:

- `AUSTRO_AI_MODEL` and `AUSTRO_AI_BASE_URL` remain mandatory and must be
  secure/non-placeholder (CONFIGURATION_REQUIRED).
- `AUSTRO_AI_API_KEY` is optional. When absent, no `Authorization` header is
  attached to outbound calls. When present, it must not be a placeholder and is
  sent as `Bearer` exactly like the real provider.
- The strict `openai-compatible` path (key mandatory, defense-in-depth factory
  validation) is unchanged, so the SaaS boundary is not weakened.

The default remains `stub` (offline deterministic). No paid credential is ever
fetched, fabricated, or committed; activating any paid provider remains a
configuration-only change later.

## Decision — worker queue name default aligned with the API

The worker entrypoint (`cmd/worker/main.go`) now resolves the queue via the same
`queueFor` helper the API uses: configured name, or `austro.events`. This keeps
the sibling entrypoints byte-consistent and removes the silent-disconnect class
of failure for an unset `AUSTRO_RABBITMQ_QUEUE`.

## Consequences

- A zero-cost local model server (e.g. a local inference box) can be wired with
  only two settings and no secret; local HTTP endpoints are an accepted, explicit
  operator choice and are addressed by the existing `isInsecure` endpoint checks.
- The canonical environment and `.env.example` document both the `local` choice
  and the unchanged paid-provider boundary.
- Tests: config validation for the local backend (no-key allowed, mandatory
  model/base url, placeholder key rejected), factory selection of the keyless
  provider, no-Authorization-header behavior over a live `httptest` endpoint,
  honor-a-present-key behavior, and worker queue-default resolution.

## Out of scope

SaaS free-tier providers, offline model bundling, and paid-provider pricing
decisions remain operator-time concerns (CONFIGURATION_REQUIRED later); this ADR
does not change the Phase 1 gate or any approved Phase 2 scope.