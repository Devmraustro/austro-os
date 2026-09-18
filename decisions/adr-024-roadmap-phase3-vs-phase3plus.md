# ADR-024 — Programmatic Status Reconciliation: Phase 3 Web Application vs Deferred Phase 3+

- **Status**: Accepted (scope reminder, not new scope)
- **Date**: 2026-09-18
- **Authorship**: Authored by the AUSTRO-OS engineering agent as part of the
  Phase 3 governance-finalization landing. This record has **no Founder sign-off
  and claims none**: it does not decide or add product scope, it only reconciles
  documentation that conflated two distinct roadmap states. Wherever this record
  touches scope, it defers to and restates the existing Founder-approved records
  it cites.
- **Supersedes**: nothing
- **Related**: ADR-016 (Phase 3 web application — Founder-approved scope),
  ADR-022 (first vertical slice), ADR-023 (knowledge HTTP surface),
  PRODUCT_SCOPE.md (§Analytics, §Integrations, "Long-Term Product Direction"),
  PHASE3_WEB_APPLICATION_SCOPE.md, ROADMAP.md, FINAL_PROJECT_CERTIFICATION.md

## Context

The repository already contained two *different* phases both written as
"Phase 3":

1. **Phase 3 — AUSTRO OS Web Application**: new, separately Founder-approved
   scope, decided by ADR-016 and implemented (ADR-022, ADR-023, plus the
   completed ADR-016 areas).
2. **Phase 3+ — Analytics, Integrations, Marketplace**: deferred future-version
   items, named in ROADMAP's original phase table and in PRODUCT_SCOPE.md as
   "Long-Term Product Direction".

ROADMAP's phase table listed only "3+ | Analytics, Integrations, Marketplace |
Deferred" and had no row for the approved Phase 3 web application. That conflation
is what made the two statuses ambiguous: an operator reading the roadmap could
reasonably conclude that the Founder-approved web application was itself "deferred
Phase 3+". It was not; a Governance/roadmap-basis reader could not tell them
apart.

This ADR is a **documentation reconciliation**. It is not a product decision, not
an architecture change, and not a claim that Phase 3+ items were moved earlier or
that Phase 3 items were deferred later.

## Decision

1. ROADMAP's phase table now has a distinct row for **Phase 3 — AUSTRO OS Web
   Application (APPROVED)** and a separate row for **Phase 3+ (DEFERRED)**, so the
   two cannot be conflated again.
2. A short "Phase 3 — AUSTRO OS Web Application (APPROVED)" section in ROADMAP
   states the same distinction and points to the governing records.
3. `PHASE3_WEB_APPLICATION_SCOPE.md` §9 and `docs/FINAL_PROJECT_CERTIFICATION.md`
   record implementation status with the evidence baseline that distinguishes
   *statically verified*, *CI verified*, *locally not run*, *operator
   configuration required*, and *deferred*.

### Explicitly NOT decided here

- No new product scope is created.
- No Phase 3+ item (Analytics, Integrations, Marketplace, or ADR-016 Non-scope
  items) is moved earlier, implemented, or un-deferred.
- No Founder approval is fabricated or implied. The Founder-approved scope
  decisions referenced here are ADR-016 and ADR-022, which stand on their own.
- No change to the Phase 1 frozen gate, the parity tests, authorization rules,
  or any application code.

## Consequences

- The roadmap now tells the reader the two "Phase 3" states apart at a glance.
- Implementation status has a single, evidence-backed home
  (`docs/FINAL_PROJECT_CERTIFICATION.md`) with a status model that does not
  overstate what was actually verified.
- If the Founder later approves all or part of Phase 3+, that is a new decision
  that must be recorded as new scope; this ADR neither anticipates nor grants it.

## Out of scope

- Any Phase 3+ implementation.
- Any change of approval authority for Phase 3 scope (remains the Founder via
  ADR-016).