# ADR-003 — Phase 2 Final Approval and Decision Lock

- **Status**: Accepted (Founder final approval — Phase 2)
- **Date**: Phase 2 baseline (post review)
- **Related**: ROADMAP.md; ADR-001; ADR-002; CONSTITUTION.md P2, P11, P17; PRODUCT_SCOPE.md; Phase 1 gate criteria #14, #16, #31

## Summary

This ADR records the Founder's finalized Phase 2 decisions following the
Phase 2 Specification Review. It locks the overall scope, the data model and
API finalization policy, the AI provider and publishing bounds, the autonomy
boundary, rate/cost controls, the exit-criteria requirement, and Phase 1
protection. It clearly separates **Founder-approved decisions**, **deferred
capabilities**, **implementation-time design decisions**, and **hard global
prohibitions**.

Implementation of Phase 2 does not begin until this ADR is reviewed and
separately authorized.

---

## 1. Founder-approved decisions (locked)

1. **Overall Phase 2 scope: APPROVED.** The Phase 2 objective, MUST / MUST NOT /
   DEFERRED scope, and Creator-workflow-first approach as specified in
   ROADMAP.md are approved.

2. **AI provider — Phase 2 MUST use a provider-agnostic AI gateway** with a
   **deterministic stub/fake provider only**:
   - No real external AI provider.
   - No provider API key.
   - No paid model calls.
   - Real provider selection is deferred to a later phase.

3. **Publishing — Phase 2 MUST implement the publishing state machine**
   `queue → review → approve → publish` **using a stub platform adapter only**:
   - Human approval is mandatory before publish.
   - No real social platform integration.
   - No live external write.
   - No platform credentials.

4. **AI autonomy — the ADR-002 boundary remains fully active**: assigned-task
   autonomy only; workspace-bound; permission-bound; no agent creation; no
   sub-agent spawning; no self-replication; human approval for external/critical
   actions.

5. **Rate/cost controls — Phase 2 must implement/test the control mechanisms
   without real provider billing**:
   - AI gateway rate limiting.
   - Configurable cost/usage guard mechanism.
   - Task creation throttling.
   - Memory write throttling.
   - Knowledge upload/embedding limits.
   - Publishing rate limits.
   - The real provider-specific billing/cost model remains deferred.

6. **Phase 2 exit criteria — ALL 11 Phase 2 exit criteria are mandatory.**
   There are no optional or "selected" criteria. Phase 2 may be declared
   complete ONLY when:
   - 11/11 Phase 2 exit criteria PASS, AND
   - all 33 Phase 1 criteria remain PASS, AND
   - all 61 Phase 1 tests remain PASS, AND
   - build/vet/test exit codes remain 0.

7. **Phase 1 protection — Phase 1 remains closed.** Do not modify or weaken the
   Phase 1 baseline. Criterion #31 remains unchanged for Phase 1 code. ADR-001
   only supersedes the specified content-automation / social-publishing
   prohibition for Phase 2 packages.

---

## 2. Deferred capabilities (not in Phase 2)

- Real external AI provider selection and integration.
- Provider API keys and paid model calls.
- Real social platform publishing adapters (YouTube / TikTok / Instagram /
  Telegram) with live credentials.
- Live external write to any platform.
- Real provider-specific billing / cost model.
- Analytics / KPI tracking / campaign analysis / continuous improvement
  (PRODUCT_SCOPE.md §Analytics).
- Non-Creator workspaces (Clipping, Marketing, Research, Learning).
- Marketplace, plugin ecosystem, multi-tenancy, enterprise edition,
  self-improving workforce, autonomous business optimization
  (PRODUCT_SCOPE.md Long-Term Product Direction).

---

## 3. Implementation-time design decisions (authorized to finalize during
implementation, subject to constraints below)

### 3.1 Data model
Approved to finalize exact tables, columns, FKs, indexes, and RLS policy
details during implementation, **BUT**:
- No schema migration may occur before the design is documented.
- The finalized data model must be recorded in an ADR before migration.
- All Phase 2 tables must remain **additive**.
- All workspace-scoped data must use **RLS**.
- **Never use `SET row_security=off`** (ADR-007).

### 3.2 API surface
Approved to finalize exact paths, methods, schemas, and operationIds during
implementation, **BUT**:
- Contracts must be finalized **before** implementing the corresponding
  endpoint.
- Every endpoint must use **bearerAuth**.
- Every endpoint must declare **principles**.
- Every action must use **explicit deny-by-default authorization**.
- Workspace isolation must be enforced.

---

## 4. Hard global prohibitions (never superseded)

These apply to every Phase 2 package and are not lifted by any supersession:

- **No self-replicating agents, no agent creation, no sub-agent spawning.**
  The `self-replicating agent` prohibition from Phase 1 gate criterion #31
  remains in force for every package (see ADR-002).
- **No breaking the modular monolith.** No container-orchestration platform, no
  microservices, no Kafka, no dedicated distributed-search engine (Phase 1
  criterion #16 keeps each of these out of scope). Dependency direction is
  frozen: Presentation → Application → Domain → Intelligence → Shared
  Services → Infrastructure.
- **No `SET row_security=off`** (ADR-007) and no plain-text logging
  (`log.*`/`fmt.Fprintf`) in Phase 2 code (ADR-007).
- **No modifying or weakening the Phase 1 baseline** (see §1.7).

---

## 5. Documentation and governance

- Recorded per CONSTITUTION P17 (Explicit Decisions) and P2 (Architecture
  Before Implementation).
- Phase 1 baseline is closed and immutable; this ADR does not rewrite history.
- Implementation authorization is pending review of this ADR.

## 6. Phase 2 completion requirement (summary)

Phase 2 declared complete ONLY when: 11/11 Phase 2 exit criteria PASS, all 33
Phase 1 criteria PASS, all 61 Phase 1 tests PASS, and build/vet/test exit
codes remain 0. There are no optional exit criteria.