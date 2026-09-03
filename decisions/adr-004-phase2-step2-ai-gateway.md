# ADR-004 — Phase 2 Step 2: AI Gateway Abstraction + Deterministic Stub

- **Status**: Accepted (implementation-time architectural decision, Phase 2 Step 2)
- **Date**: Phase 2 Step 2
- **Related**: ROADMAP.md §2.1 (AI provider gateway), §5.7, §8; ADR-003 §1.2, §3;
  CONSTITUTION.md P6, P8, P9, P10, P13

## Context

Phase 2 Step 2 introduces the provider-agnostic AI gateway (capabilities:
complete, embed, classify) with a deterministic stub provider only. ADR-003
locks: no real external provider, no provider API key, no paid model calls,
real provider selection deferred. This ADR records the concrete design decisions
for the gateway as an implementation-time refinement of that locked scope, and
is written before the code that relies on it (P2 Architecture Before
Implementation; P17 Explicit Decisions).

## Decisions

1. **Internal package only; no HTTP/API surface in this step.**
   The gateway is implemented as `internal/ai`, a pure domain package, and is
   NOT exposed as an HTTP endpoint yet. Rule 9 of the Step 2 brief and ROADMAP
   §7 defer the proxy surface until its authorization contract is fully
   designed. No composition into `main.go` occurs in this step.

2. **Ports/interfaces, not concrete infrastructure imports.**
   `internal/ai` imports no `infrastructure/*` and no `internal/audit`
   implementation. Dependency direction is preserved: Presentation →
   Application → Domain → Intelligence → Shared Services → Infrastructure.
   The gateway depends on:
   - a `Provider` port (the AI backend),
   - a `UsageGuard` port (configurable usage/rate/cost guard, no billing),
   - an `AuditSink` port (audit-compatible decision emission).
   Each has a deterministic in-package implementation for tests and runtime
   wiring, and each can be substituted without changing callers (P6, P8).

3. **Workspace isolation is enforced at the gateway boundary.**
   Every gateway request carries an explicit `WorkspaceID`. The stub provider is
   stateless and deterministic, so it cannot mix or leak data across workspaces.
   The gateway requires a non-empty workspace and rejects calls without one
   (deny-by-default) (P9, P10). Cross-workspace context leakage is impossible at
   this boundary because no cross-workspace state exists and outputs depend only
   on the request inputs.

4. **Audit compatibility via an injectable sink.**
   The gateway emits a decision record whose shape mirrors the existing audit
   model (`event_type`, `constitutional_principle`, `outcome`, `workspace_id`,
   trace/span). It does NOT introduce a second, incompatible audit mechanism and
   does not weaken the Phase 1 audit hash chain. The concrete connection of the
   sink to `audit.AppendEvent` is a composition decision for a later step so the
   gateway remains testable without embedding HMAC signing. Recorded outcomes
   use the established constitutional principles (e.g. Replaceability, AI
   Independence, Observability) where applicable (P13).

5. **Usage/cost guard is configurable and billing-free.**
   `ConfigurableUsageGuard` exposes configurable limits (per-operation and per
   unit-of-use counts / gauges). The stub path invokes the guard deterministically.
   There is no provider billing behavior; `NoopUsageGuard` provides an unlimited
   default for contexts where guarding is not yet configured.

6. **No new external dependencies.**
   The gateway uses only the Go standard library, `github.com/google/uuid`
   (already a project dependency), and `internal/log` (structured JSON). No new
   modules are added to `go.mod`.

7. **Deterministic stub is the only provider in this step.**
   `StubProvider` returns outputs that are a pure function of the request inputs,
   suitable for unit tests and deterministic runtime behavior until a real
   provider is selected in a later phase.

## Consequences

- Phase 1 exit criteria remain green: no Phase 1 code is modified, no forbidden
  technology strings are introduced, and the modular monolith and dependency
  direction are preserved.
- The gateway is ready for Step 3+ to compose it into task/knowledge/publishing
  flows via the defined ports.
- Any future real provider must implement the same `Provider` port; callers and
  the gateway are unchanged (P6, P8).

## Out of scope for this ADR / later steps

- HTTP proxy surface and its authorization contract (Step ≥ later).
- Task, Knowledge, Memory completion, Publishing, Creator orchestration.
- Real AI providers and billing.
- Any Phase 2 database migration or schema.