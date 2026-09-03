# ADR-001 — Phase 2 Supersession of Phase 1 Gate Criterion #31

- **Status**: Accepted (Phase 2 kickoff — Founder decision)
- **Date**: Phase 2 baseline
- **Related**: ROADMAP.md §5.2, §2.2; `tests/phase1_exit_criteria_test.go` criterion #31; `.github/workflows/phase1-exit-criteria.yml`

## Context

Phase 1's gate criterion #31 (`phase1_exit_criteria_test.go:332`) bans the
strings "content automation", "social publishing", and "self-replicating agent"
from V1 `internal/` code. This was correct for Phase 1, whose scope deliberately
deferred business features (criterion #14).

Phase 2's stated objective is to introduce the first content-production
workflow, which necessarily includes content automation (plan → research →
script → publish) and social-publishing *orchestration*. An unmodified #31 would
therefore prevent Phase 2 from existing.

## Decision

Phase 2 **supersedes criterion #31** for Phase 2 packages only, as an explicit,
recorded decision (P17 Explicit Decisions; P2 Architecture Before
Implementation):

- Criterion #31 remains in force for **Phase 1 code** (packages present at the
  Phase 1 baseline) and is not deleted or edited.
- **Phase 2 packages** (new modules introduced by Phase 2: `internal/task`,
  `internal/knowledge`, `internal/publish`, `internal/ai`, and any additive
  files) are exempt from the "content automation" and "social publishing"
  prohibitions **only to the extent required by this roadmap**.
- The **"self-replicating agent" prohibition is never** superseded. It remains
  a hard global rule (see ADR-002). No Phase 2 code, in any package, may
  implement agent self-replication.
- The supersession is scoped to orchestration. AUSTRO OS does not become a
  social platform, video editor, or general chatbot (PRODUCT_SCOPE.md Out of
  Scope). Publication adapters in Phase 2 are stubs; no real platform is
  integrated and no live write occurs.

## Rationale

- Matches the product vision (PROJECT.md: "automate the entire content
  production workflow") and PRODUCT_SCOPE.md Content Production in-scope items.
- Keeps the Phase 1 gate honest: it never implied content automation is
  permanently forbidden — it deferred it to a later phase.
- The modular-monolith / technology constraints (criterion #16) are **not**
  superseded and remain global (no orchestration platform/microservices/Kafka/
  dedicated search engine).

## Consequences

- A Phase 2 CI check must confine the #31 "content automation"/"social
  publishing" scan to Phase 1 packages (or assert the Phase 2 supersession is
  recorded, as here). The Phase 1 criterion is untouched.
- Any Phase 2 code that touches campaign-rule compliance, publishing state,
  or approval workflows will legitimately reference content-automation
  concepts; this is expected post-supersession.
- No change to ADR-007 (row-security / plain-text logging prohibitions).