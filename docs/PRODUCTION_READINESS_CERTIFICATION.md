# AUSTRO OS — Production Readiness Certification

**Certified release:** `3f4baa4d628b0ab2db64411cbb21de2a80dba317` (`main`, merge
of PR #9 *"Phase 3 governance reconciliation and final certification"*,
merged 2026-09-18T21:13:50Z)

**Certification date:** 2026-09-19

**Certifying scope:** the release snapshot `3f4baa4d` plus the operations
artifacts this certification PR introduces (`scripts/monitor.sh`,
`docs/monitoring-and-alerting.md`, the `production-rehearsal` monitor step, the
monitoring variables in `.env.example`, and this document). No application code
was changed.

**Grading rules.** Every statement below carries evidence. Nothing is claimed
as executed or observed that was not. The statuses used:

| Status | Meaning |
|---|---|
| **VERIFIED** | executed or proven against the release snapshot or its CI runs, with evidence cited |
| **ENGINEERING-READY** | the repository makes the property true and CI proves the mechanism; the real-world assets it needs do not exist yet (they are OPERATOR-CONFIGURED) |
| **OPERATOR-CONFIGURED** | the property becomes true only when an operator supplies real-world values, assets or accounts at the deployment site |
| **NOT RUN — REASON** | deliberately not executed, with the reason stated; never a disguised pass |

`LIVE IN PRODUCTION` is a separate state: **nothing in this certificate
claims a production deployment.** The repository was verified, rehearsed and
certified — it was not deployed to any public host and no real user has traffic
on it. Reaching `LIVE IN PRODUCTION` is the operator's go-live decision, and
the go-live checklist in stage 18 is its definition of done.

---

## 1. Release baseline and recon

**VERIFIED.** Release = `3f4baa4d628b0ab2db64411cbb21de2a80dba317` on `main`.
Deployment tree audited: `Dockerfile`, `docker-compose.yml`,
`docker-compose.production.yml`, `deploy/caddy/Caddyfile`,
`deploy/nginx/nginx.conf`, all operator scripts, `.env.example`, and the seven
runbook documents. The frozen Phase-1 gate
`tests/phase1_exit_criteria_test.go` is byte-identical, SHA-256
`0a868d3da41f175cc263a6301f806e3bc1125093dab9cf8db98bb17b54e8769a` (blob
`5f478fdf3b1d4bdb7de1cf0b7d175f3699d99a0a`). All certification evidence below is
read from the release snapshot and its immutable CI records.

## 2. Deployment target validation

**ENGINEERING-READY.** The only supported deployment target is the shipped
Docker Compose production stack: a Linux amd64 host exposing ports 80/443 via
the reverse proxy only, with everything else on an internal network.
`docker-compose.production.yml` parses (compose-config executed in CI), the
image builds and its hardening is asserted (non-root `appuser` verified at
runtime, no privilege escalation for api/worker, read-only rootfs, named
volumes), and the full stack comes up through the operator path
(`scripts/deploy.sh`) in CI. Container orchestration platforms and vendor
serverless targets are **NOT SUPPORTED by design** and are rejected by the
static verification scan; the architecture is a modular monolith and stays one.
**OPERATOR-CONFIGURED:** the actual host, its OS, and its network ingress.

## 3. Staging environment

**VERIFIED.** The `production-rehearsal` job in
`.github/workflows/production-deployment.yml` is a real staging environment,
not a dry run: it generates per-run random credentials, builds the production
compose stack with the production compose file, deploys through
`scripts/deploy.sh`, seeds real persisted data, backs up, destroys, restores,
and verifies the result plus a full post-restore healthcheck. It executes
negative controls so a green run is evidence the checks can fail, and it uses
no mocks and no fake responses. The `production-smoke` job covers stack
bring-up with the same topology assertions in isolation. The most recent full
rehearsal on the release snapshot is workflow run `35395683793` (success; job
total 127 s).

## 4. Production configuration

**VERIFIED (validator behaviour, at the release snapshot).** Invalid
production configuration cannot start: the compose file fails the parse on
absent required values (`:?`), the application's fail-fast validator rejects
placeholders and loopback addresses at startup, and `scripts/deploy.sh` checks
placeholders and minimum lengths (passwords ≥ 16 characters, JWT signing keys
≥ 32 bytes) before starting anything. Every variable the application reads is
documented in `.env.example` and covered by the static configuration-contract
test, which passed in the release CI. **OPERATOR-CONFIGURED:** the actual
production values.

## 5. Secrets

**ENGINEERING-READY.** No `.env`, `.env.production`, private key or certificate
is tracked by git (checked by CI and by the static verification). A
secret-shaped value scan over the deployment files runs in CI and is clean at
the release snapshot. `.env.production` is git-ignored and is the only
credential source the operator scripts read; placeholders in it fail the
deploy with a message naming the file, never a confusing mid-run abort.
Credentials travel to services only through the compose env file and are never
echoed by any operator script, log line, or alert payload.
**OPERATOR-CONFIGURED:** the real values and the secrets lifecycle
(generation, delivery, rotation).

## 6. TLS and domain

**ENGINEERING-READY.** The nginx variant terminates TLS with a floor of
TLSv1.2/1.3, sends HSTS and `X-Content-Type-Options: nosniff`, redirects
plaintext HTTP to HTTPS (301), caps request bodies, and overwrites
`X-Forwarded-For` from the observed peer — it never appends a client-supplied
value. The Caddy variant (opt-in profile `reverse-proxy-caddy`) issues
automatic certificates through ACME. End to end, the CI rehearsal generates
ephemeral self-signed material and proves the proxy serves `/health/live` over
a real HTTPS request (HTTP 200) while the API is unreachable from the host.
**OPERATOR-CONFIGURED:** the real domain, its DNS, and the real certificates
(nginx key pair, or Caddy ACME with an account contact). No certificate
material is committed.

## 7. Database, row-level security, migration safety

**VERIFIED (against the live database, in CI, at the release snapshot).** The
three-principal topology is enforced: owner (schema/bootstrap), runtime (serves
every request), admin (org-level + audit chain). The runtime role is verified
against the live catalog on every deploy and after every restore: it is not a
superuser, does not hold `BYPASSRLS`, owns no protected table, and is observed
serving with its own credential (`database-topology-ready`, `runtime_role`).
All 12 protected tables have row-level security enabled and forced. The
verification queries are executed as the runtime role itself, so a pass cannot
be obtained by asking the owner about the runtime role. A runtime DSN equal to
the owner DSN is rejected, and migrations run as an idempotent bootstrap
through the owner role only.

## 8. Backup

**VERIFIED (local backup path).** `scripts/backup.sh` ran against the live
stack in the release rehearsal. The artifact was validated as non-empty, as a
valid gzip stream, as containing the PostgreSQL dump header (full-stream
consumer — no early-closing verification pipeline), and its `.sha256` checksum
was verified. Retention defaults to 14 days. **OPERATOR-CONFIGURED:** the
off-host copy (S3 bucket via `scripts/backup.sh`; not engaged in CI because CI
has no bucket by design), the backup schedule, and the backup host directory.

## 9. Disaster recovery and restore rehearsal

**VERIFIED for the automated rehearsal.** CI performed a destructive exercise:
seed → backup → destroy (all three seeded rows verified gone) → restore with
`scripts/restore.sh` (measured 21 s) → verify (all three rows back, plus
post-restore topology, plus a full post-restore healthcheck). An untested
backup is an assumption, not a control: `docs/backups-and-restore.md` and
`docs/operations.md` calendar a monthly human restore exercise into a scratch
host. **OPERATOR-CONFIGURED:** that real-host drill. **NOT RUN — REASON:** WAL
archiving and point-in-time recovery are not implemented; the shipped
configuration restores to the last backup, and that is the documented recovery
objective.

## 10. Worker and queue reliability

**VERIFIED for the shipped lifecycle.** The worker attaches to `austro.events`
(`worker-started`), redials the broker after a forced close
(`worker-reconnected`), drains on shutdown, and the healthcheck asserts the
expected queue exists. The new monitor adds the two time-series signals that
matter for sustained operation: `queue_backlog` (depth vs
`AUSTRO_QUEUE_BACKLOG_THRESHOLD`, default 100) and `worker_errors` (error-line
budget in the worker's recent log window). **OPERATOR-CONFIGURED:** broker
redundancy — the shipped stack runs a single RabbitMQ node, which is the
documented default.

## 11. Observability

**ENGINEERING-READY.** The application writes structured JSON logs
(timestamp/level/message/fields) to stdout, correlates trace and span
identifiers through middleware into the audit and memory layers, and exposes
`/health/live` and `/health/ready` by contract. This certification adds a
per-run JSONL signal stream (`scripts/monitor.sh`) with a summary line per run.
**OPERATOR-CONFIGURED:** log shipping, retention, and dashboards — the
repository deliberately ships no metrics-export or dashboard stack.

## 12. Alerting

**ENGINEERING-READY; delivery NOT RUN — REASON.** `scripts/monitor.sh` POSTs a
small JSON alert (debounced; one page per sustained failure) to
`AUSTRO_ALERT_WEBHOOK_URL` and was executed against the live stack in the
release rehearsal. The webhook branch itself cannot fire in CI because CI has
no webhook receiver by design. `docs/monitoring-and-alerting.md` documents an
operator procedure that proves the branch end to end on a staging host (with a
safe failure induced by `AUSTRO_BACKUP_MAX_AGE_HOURS=0`). **OPERATOR-CONFIGURED:**
the receiver, the schedule (cron or systemd timer), and the runbook rule that a
non-zero monitor exit is a paging trigger.

## 13. Security pre-launch audit

**PASS for the shipped surface; two recorded gaps.** Verified at the release
snapshot: 48 protected API routes ↔ 48 distinct (action, resource-pattern)
rule pairs (1:1 parity); row-level security enforced for the serving role; no
secret-shaped values in any deployment file; no tracked secrets; proxy
hardening (TLS floor, HSTS, nosniff, converted `X-Forwarded-For`, body cap);
placeholders rejected before start; and the full deployment CI matrix green
on the release main (8/8 checks, run IDs in the evidence table below).
Recorded gaps, deliberately not hidden: **no Content-Security-Policy header**
and **no login rate limiting** in the shipped configuration. Both are
documented operator advisories rather than implemented control points; neither
makes the certification dishonest, because neither is claimed.

## 14. Real user journey

**VERIFIED against real browsers.** The real-Chromium end-to-end suite plus
the creator-pipeline and organization-boundary mutation suites pass their full
matrices at the two full-bandwidth PR heads that produced this release
(31/31 at `c4e83e69dd3e`; 30/30 at `fbcd4b41facab1357a7a9b8f716d1db97bc84a73`),
and the release main itself re-ran all main workflows green (8/8). The
certification PR re-ran the journey on its own head (31/31 at
`53141089cdb26f4ccba03798f69422330202964e`), and the certification main
re-ran all main workflows green (8/8).

## 15. Performance baseline

**PARTIAL VERIFIED (operational timings); load NOT RUN — REASON.** Measured on
the release rehearsal (workflow run `35395683793`): full operator deploy
(`scripts/deploy.sh`) 81 s; deep healthcheck 3 s; restore 21 s; backup ~0.5 s
(empty-start database); smoke stack-start 57 s to full health. **NOT RUN:**
sustained load and latency-under-load measurement, because no load-generation
tooling or representative traffic exists in this environment. A load test
against a real host with real user journeys is **OPERATOR-CONFIGURED** and is a
prerequisite before announcing any capacity number.

## 16. Runbooks

**VERIFIED.** `docs/production-deployment.md` (deploy), `docs/
production-configuration.md` (config contract), `docs/backups-and-restore.md`
(backup + restore + monthly drill), `docs/operations.md` (daily/weekly/monthly
calendar, capacity signals), `docs/troubleshooting.md` (failure catalogue),
`docs/deployment-targets.md` (supported targets), and the new
`docs/monitoring-and-alerting.md` (signals, thresholds, webhook contract)
together cover operate, alert, and recover.

## 17. Release freeze discipline

**ENGAGED.** The release snapshot `3f4baa4d` is frozen as the certification
baseline. This certification PR introduces only operations artifacts
(operator script, CI step, environment template variables, documentation) and
touches no application code. The final merge SHA is recorded in the evidence
table, and the frozen gate is re-verified byte-identical on it.

## 18. Go-live checklist

**OPERATOR-CONFIGURED — all items are the operator's go-live definition of
done.** Each item is documented where indicated:

1. Provision the host (OS, Docker, 80/443 reachable, firewall closed on
   8080/5432/6379/5672/15672) — `docs/production-deployment.md`.
2. Point DNS at the host and set `AUSTRO_PUBLIC_HOSTNAME`.
3. Install the real certificates (nginx key pair, or Caddy + `AUSTRO_ACME_EMAIL`).
4. Create `.env.production` with real values (every placeholder replaced;
   passwords ≥ 16 bytes, JWT keys ≥ 32 bytes) — `docs/production-configuration.md`.
5. Rotate/reset the JWT signing keys at go-live; record who holds the founder
   credential and the Docker socket — `docs/operations.md`.
6. Configure a webhook receiver and set `AUSTRO_ALERT_WEBHOOK_URL`.
7. Schedule `scripts/monitor.sh` (systemd timer or cron) and verify it pages.
8. Schedule `scripts/backup.sh`; configure `AUSTRO_BACKUP_S3_BUCKET` for an
   off-host copy — `docs/backups-and-restore.md`.
9. Calendar the monthly restore drill — `docs/operations.md`.
10. Run `scripts/deploy.sh`; then run `scripts/healthcheck.sh`; confirm 0.
11. Confirm the API and datastores are NOT reachable from the host (external
    port scan) — the rehearsal asserts exactly this.
12. Bootstrap the founder and run the first real user journey through the proxy.

## 19. Certification

**THIS DOCUMENT.** It records engineering readiness facts, honors the
VERIFIED / ENGINEERING-READY / OPERATOR-CONFIGURED / NOT RUN distinction, and
makes no production claim it does not have evidence for.

## 20. DevOps and CI discipline

**VERIFIED.** The frozen Phase-1 gate is byte-identical at the release SHA and
re-verified again at the final merge. No workflow uses `continue-on-error` or
early-exit masking; negative controls in the rehearsal and static verification
prove the checks can fail; every workflow in the repository is green at the
release SHA; and this certification PR adds its own fresh run as further
evidence.

---

## Evidence table

| Evidence | Identifier | Verdict |
|---|---|---|
| Release commit (main) | `3f4baa4d628b0ab2db64411cbb21de2a80dba317` | certified baseline |
| Frozen gate SHA-256 | `0a868d3da41f175cc263a6301f806e3bc1125093dab9cf8db98bb17b54e8769a` | byte-identical |
| Post-merge CI on release main (8/8 checks green) | Build/Vet/Unit `105763871545`, Full Regression `105764227670`, Phase-1 gate `105763870746`, Production deployment static `105763871283`, compose-config `105763871381`, image `105763871116`, smoke `105763871655`, rehearsal `105763871394` | PASS |
| Full rehearsal workflow run (release main) | `35395683793` | PASS, deploy 81 s / restore 21 s / healthcheck 3 s / job 127 s |
| Real-Chromium E2E + mutation suites at PR head | `c4e83e69dd3e` 31/31; `fbcd4b41facab1357a7a9b8f716d1db97bc84a73` 30/30 | PASS |
| RBAC parity | 48 routes ↔ 48 (action, resource-pattern) pairs | 1:1 |
| RLS posture | 12 protected tables, enabled + forced; runtime role unprivileged (no superuser, no `BYPASSRLS`, owns nothing) | verified per deploy and per restore |
| Certification PR | PR #10, merged as `02b7086aac89d30b9c3418e5ec8b7661baa7ba38`; rehearsal ran the monitor step against the live restored stack (workflow run `35443417081`) | merged; 31/31 green on the PR head, gate re-verified |
| Certification main (post-merge, 8/8 checks green) | Build/Vet/Unit `105899429651`, Full Phase 1 + Phase 2 Regression `105899572439`, Phase-1 gate `105899429763`, Production deployment static `105899429710`, compose-config `105899429640`, image `105899429755`, smoke `105899429753`, rehearsal `105899429855` | PASS |

## What this certificate does not say

- It does **not** say the system is live. Nothing is deployed to a public host.
- It does not certify the operator site: DNS, certificates, webhook, schedules,
  and buckets all remain operator configuration.
- It does not certify load behaviour, WAL point-in-time recovery, multi-node
  broker or database clustering (all explicitly out of the shipped scope), or
  any feature of the deferred Phase 3+ roadmap (analytics, KPI platform,
  marketplace, plugin ecosystem, multitenancy). Those are documented, not
  hidden, and none is required for the certified recovery objective.