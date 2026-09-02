# ADR-002 — Phase 2 AI Autonomy Bound: Human-Oversight, Non-Replicating

- **Status**: Accepted (Phase 2 kickoff — Founder decision)
- **Date**: Phase 2 baseline
- **Related**: ROADMAP.md §5.3, §2.2; CONSTITUTION.md P11 Human Oversight;
  ORGANIZATION.md; COMPANY.md; PRODUCT_SCOPE.md; Phase 1 gate criterion #31

## Context

ORGANIZATION.md and COMPANY.md describe AI Employees as autonomous digital
workers that execute work within the Digital AI Company hierarchy. Phase 1's
gate criterion #31 bans "self-replicating agent" globally. Phase 2 introduces
real autonomous execution for the Creator workflow, which raises the question
of how much autonomy an AI employee may exercise.

## Decision

Phase 2 enforces a **human-oversight, non-replicating** autonomy bound:

1. **Non-replicating (hard global rule, never superseded):** no AI employee or
   pipeline step may create another agent, spawn sub-agents, or self-replicate.
   The "self-replicating agent" prohibition from gate #31 remains in force for
   every package, including Phase 2. Agents are assigned, not self-spawned.
2. **Task-level autonomy within a workspace:** an AI employee executes tasks
   assigned to it only, within its own workspace. It cannot operate outside its
   assigned responsibilities (ORGANIZATION.md §AI Employees) or its granted
   permissions (deny-by-default authz).
3. **Human approval required for critical actions (P11):** any action affecting
   external publication, finance, legal compliance, or product governance
   (approval workflows fall in this class) requires explicit human approval
   before execution. This is enforced in the publishing/approval state machine
   and tested. Nothing external is written without human approval.
4. **No unrestrained emergence:** autonomy is bounded to the frozen Phase 2
   scope; "self-improving AI workforce" and "autonomous business optimization"
   (PRODUCT_SCOPE.md Long-Term Product Direction) are deferred beyond Phase 2.

## Rationale

- Resolves the apparent tension between "autonomous AI employees" and the
  self-replication ban by separating *autonomous execution of assigned work*
  (allowed, within workspace + permissions) from *agent self-creation*
  (disallowed, globally).
- Satisfies P11, P9 (Security by Design), P10 (Privacy by Design): no
  unbounded agent spawns, no uncontrolled cross-workspace behavior, and a human
  gate on every critical/external action.
- Keeps exit criteria objective and testable: #4 (human-gate approval/rejection)
  and #3 (cross-workspace denials) both encode this bound.

## Consequences

- AI employees run within a fixed org hierarchy; there is no agent-creation
  API, event, or endpoint in Phase 2.
- The publishing flow is un-publishable without an approval record and an
  approving human identity in the audit chain.
- The Phase 2 exit-criteria test for "no self-replicating agent" scans all
  packages (supersession in ADR-001 does NOT lift this specific prohibition).
- Future autonomy expansion (self-improving workforce, organic growth,
  contractor/marketplace agents in COMPANY.md §Future Expansion) requires a new
  ADR and Founder approval; it does not ride on this decision.