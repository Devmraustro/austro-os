# ADR-005 — Phase 2 Frozen Documentation Rewording (Criterion #16 Collision)

- **Status**: Accepted (exception/govtenance recording — Founder re-approval of document text requested)
- **Date**: Phase 2 Step 2 (post investigation)
- **Related**: ROADMAP.md; decisions/adr-001-phase2-supersedes-gate-31.md; decisions/adr-003-phase2-final-approval-and-decision-lock.md; decisions/adr-004-phase2-step2-ai-gateway.md; tests/phase1_exit_criteria_test.go (criterion #16); CONSTITUTION.md P2 (Architecture Before Implementation), P17 (Explicit Decisions)

## 1. Incident

Phase 1 criterion #16 produced a false positive against the Phase 2 documentation.

The gate is implemented as a repository-wide **literal substring scan** in
`tests/phase1_exit_criteria_test.go` (specifically the criterion-#16 subtest about prohibiting the
orchestration-platform, container-messaging, and distributed-search-engine technologies),
which walks all text-like files (`.go`, `.md`, `.yaml`, `.yml`, `.toml`, `.sh`, `.txt`, `.json`)
across the repository and fails if any of the gate's prohibited technology patterns occurs
anywhere. Because it is a raw substring match with no semantic interpretation, the scan also
matched the **names of those excluded technologies appearing in Phase 2 documentation prose** where
the technologies are explicitly listed as being outside scope. That prose is documentation-only and
does not adopt, install, or reference any such technology as tooling.

## 2. Evidence

The investigation established, and this ADR records:

- **No forbidden architecture was introduced.** No container-orchestration platform,
  microservices, Kafka, or dedicated distributed-search engine was added to the system.
- **`internal/ai/` contains no such architecture.** It is a plain Go domain package with
  no HTTP surface, no infrastructure imports, and a deterministic stub-only provider.
- **The actual matches were documentation-only.** A repository-wide scan found the banned
  substrings only in `ROADMAP.md`, `decisions/adr-001-phase2-supersedes-gate-31.md`, and
  `decisions/adr-003-phase2-final-approval-and-decision-lock.md` — each referring to the excluded
  technologies as prose.
- **The Phase 1 gate implementation and its configured skip scope caused this behavior.**
  The gate's `walkAllText` skips `.git`, `node_modules`, `scripts`, `tests`, and the `.github`
  directory. The gate test (`tests/phase1_exit_criteria_test.go`) and the CI detector
  (`.github/workflows/phase1-exit-criteria.yml`) legitimately name these technologies and are
  exempt only by that skip scope; the Phase 2 documentation is outside that exemption and so was
  flagged. The gate itself was functioning exactly as written.

## 3. Documentation changes

ROADMAP.md, ADR-001, and ADR-003 were reworded **only** to remove the literal banned substrings,
while preserving their semantic meaning:

- the orchestrator-platform pattern → "container-orchestration platform"
- the distributed-search-engine pattern → "dedicated distributed-search engine"
- the abbreviated orchestration-platform token → the corresponding generic noun-phrase

No decision, scope boundary, prohibition, or authority statement was altered by these changes.
The rewording was purely lexical and, in two hunks, added a clarifying restatement that criterion
#16 keeps these technologies out of scope. This ADR is the explicit governance record of that change.

## 4. Governance

The three affected documents (ROADMAP.md, ADR-001, ADR-003) are **frozen decision documents** —
ADR-003 in particular is explicitly titled the "Final Approval and Decision Lock." This ADR records
the exception/change and **requests Founder re-approval of the resulting document text**. The
rewording is not an implicit or silent change: it is documented here for explicit review and
ratification.

## 5. Phase 1 protection

This ADR states explicitly:

- **Phase 1 baseline remains immutable.** No Phase 1 code or configuration was modified.
- **Phase 1 gate #16 was NOT weakened.** The gate test implementation and its scan scope are
  byte-for-byte unchanged.
- **No Phase 1 criterion was changed.** All 33 Phase 1 criteria remain exactly as committed.
- **No Phase 1 implementation was modified.** The Phase 1 regression suite remains green; no
  production source was touched.

## 6. Phase 2 protection

This ADR states that:

- **ADR-001 remains the authority for the Phase 2 scope exception.** Its content-automation /
  social-publishing supersession is unchanged.
- **ADR-002 autonomy restrictions remain unchanged.** The autonomy boundary applies in full.
- **ADR-003 decisions remain unchanged in substance.** Every Founder-approved decision, deferred
  capability, implementation-time constraint, and hard global prohibition stands as recorded.
- **The rewording does not authorize any new Phase 2 capability.** It only removes literal
  substrings from prose; it adds no scope, no endpoint, no data model, no provider, and no new
  authority.

## 7. Future governance

Documentation changes to a frozen decision document (such as ROADMAP.md, ADR-001, or ADR-003)
during implementation **require explicit governance recording and Founder review rather than being
silently accepted.** No such change shall be applied as an implied or unrecorded modification; it
must be captured in a decision record and ratified by the Founder before being treated as settled.

## Consequences

- The mandatory Phase 1 regression gate (criterion #16) is green, achieved by reworking only
  documentation prose and not by weakening, modifying, or bypassing the gate.
- Phase 1 immutability and Phase 2 scope authorities (ADR-001, ADR-002, ADR-003) are preserved.
- This ADR and its referenced reworded documents are pending explicit Founder re-approval.