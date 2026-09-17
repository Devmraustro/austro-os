# AUSTRO OS — Final Production Readiness Report

**Scope:** the production deployment layer reconstructed on top of `main`
(`64c72ba102c570241c9ec594d466a8e4bbc0ff6d`).

**Date of this report:** 2026-09-17.

**Verdict: READY FOR A CONTROLLED DEPLOYMENT EXERCISE — NOT DEPLOYED, NOT
PROVEN END TO END.**

The deployment layer is complete and statically verified, and CI has now
independently confirmed the parts the authoring environment could not. On a real
runner, the production stack was **brought up**: postgres, redis, rabbitmq, api
and worker all reached healthy, the operator's own healthcheck passed 19 checks
against the running topology, the datastores and the API were confirmed
**unreachable from the host**, and the API's log recorded a verified database
topology on the `austro_app` runtime role. That run also found and fixed two real
defects — see §8 — one of which broke the **default** deployment path.

It has still **not been run as a production deployment**. The smoke run proves
the topology comes up and is correctly isolated; it does not prove a public
address, a TLS handshake, real traffic, or a restore. No production target was
reached, no certificate was issued, no DNS was pointed anywhere, and no restore
has been exercised.

The authoring environment has no Docker daemon and no Go toolchain, so anything
requiring them is marked `BLOCKED` locally; where it has since executed in CI it
is recorded as CI evidence, never as a local pass. Every claim in this report is
marked with what actually backs it.

Vocabulary used throughout: `PASS`, `VERIFIED`, `BLOCKED`, `FAIL`,
`CONFIGURATION REQUIRED`, `NOT TESTED`, `NOT SUPPORTED`.

---

## 1. What was built

| Deliverable | Status |
|---|---|
| `docs/deployment-targets.md` | Present |
| `docs/production-configuration.md` | Present |
| `docs/production-deployment.md` | Present |
| `docs/operations.md` | Present |
| `docs/backups-and-restore.md` | Present |
| `docs/troubleshooting.md` | Present |
| `docs/FINAL_PRODUCTION_READINESS_REPORT.md` | This file |
| `docker-compose.production.yml` | Present — 6 services + an opt-in Caddy profile |
| `deploy/nginx/nginx.conf` | Present — complete nginx.conf |
| `deploy/caddy/Caddyfile` | Present |
| `scripts/deploy.sh` | Present, executable |
| `scripts/healthcheck.sh` | Present, executable |
| `scripts/backup.sh` | Present, executable |
| `scripts/restore.sh` | Present, executable |
| `scripts/verify-production-deployment.sh` | Present, executable (added; see §7) |
| `.github/workflows/production-deployment.yml` | Present (added; see §8) |
| `Dockerfile` | Updated |
| `.env.example` | Updated |

---

## 2. Frozen Phase-1 gate

| Check | Result |
|---|---|
| `tests/phase1_exit_criteria_test.go` sha256 == `0a868d3d…8769a` | **PASS** |
| File modified by this work | No |
| `.env.example` still satisfies the frozen gate's requirement 24 (`change-me` present, `production-secret-value` absent) | **PASS** |

Verified with `sha256sum` before the first edit and re-verified afterwards. The
hash is also asserted in `scripts/verify-production-deployment.sh` and in both
CI workflows.

---

## 3. Deployment-layer verification actually performed

Everything in this section was executed in the authoring environment.

| Verification | Result | Evidence |
|---|---|---|
| Static deployment verification (10 sections, 155 checks) | **PASS** | `scripts/verify-production-deployment.sh` — 155 passed, 0 failed |
| Frozen gate hash | **PASS** | asserted in the same script |
| Compose YAML structural lint | **PASS** | embedded linter, plus a **negative control** that injects faults and requires rejection |
| CI workflow YAML structural lint | **PASS** | same linter, workflow top-level key set |
| Shell syntax (`bash -n`) on all 6 scripts | **PASS** | |
| Shell strict mode present on every script | **PASS** | |
| No `set -x` in any script (which would trace secrets into logs) | **PASS** | |
| Scripts fail closed with no Docker present | **PASS** | each exits non-zero with a clear error, fabricating nothing |
| `scripts/healthcheck.sh` control flow | **PASS (19/19)** | executed against a **stubbed** docker; see the caveat below |
| Negative controls on `healthcheck.sh` | **PASS** | removing the API topology log line, and a broker listing a different queue, each produce FAIL + exit 1 |
| Secret scanning of the new files | **PASS** | no private keys, AWS keys or `sk-` tokens; no `.env`/key/cert files tracked by git |
| Configuration contract coverage | **PASS** | all 23 required variables documented in `.env.example` |
| No prohibited technologies (Kubernetes, Kafka, Vercel) | **PASS** | no manifests, no Kafka in `go.mod`, no `vercel.json` |

**Caveat on the stubbed healthcheck run.** It exercised the script's control
flow and revealed two real defects (a queue-presence check that would miss a
padded `rabbitmqctl` column, and its own over-strict fixture), both fixed. It is
**not** evidence that a real topology passes: the container states and command
outputs were fabricated by the stub.

---

## 4. What could NOT be verified in the authoring environment

| Verification | Local result | Why |
|---|---|---|
| `go test ./...` | **BLOCKED** locally | No Go toolchain; no network egress to install one. **Executed and PASSING in CI** — see §8 |
| `go build`, `go vet`, `gofmt` | **BLOCKED** locally | Same. **Executed and PASSING in CI** |
| `gosec`, `govulncheck`, `staticcheck` | **BLOCKED** locally | Not installed; no egress. **Executed and PASSING in CI** |
| `docker build` | **BLOCKED** locally | No Docker daemon. **Executed and PASSING in CI** — the image builds and is non-root |
| `docker compose config -q` | **BLOCKED** locally | No Docker daemon. **Executed and PASSING in CI** |
| Starting the stack, any container | **BLOCKED** locally | No Docker daemon. **Executed and PASSING in CI** — all five containers came up healthy |
| `nginx -t`, `caddy validate` | **BLOCKED** | Neither binary available |
| `psql` against a live database | **BLOCKED** | Not installed; no PostgreSQL |
| ShellCheck | **BLOCKED** | Not installed |
| Arm64 image build | **NOT TESTED** | Dockerfile builds `GOARCH=amd64` |

Per the project's own rules, none of these were recorded as passes locally. Most
of them have since run in CI — see §8, which is the authoritative record of what
has actually executed.

**The environment probe that produced these local results:** `command -v go docker
gosec govulncheck staticcheck gofmt shellcheck psql` all failed; `apt-get` could
not reach `deb.debian.org`; `curl https://proxy.golang.org` returned
`SSL_ERROR_SYSCALL`. Only git and the GitHub API were reachable, which is how the
branch was pushed.

---

## 5. Security posture

### Controls implemented and machine-checked

| Control | Result |
|---|---|
| Frozen gate unmodified | **PASS** |
| No privileged containers | **PASS** (asserted on source *and* on the resolved compose config in CI) |
| No host network namespace | **PASS** |
| No Docker socket mount | **PASS** |
| Runtime containers non-root (`appuser`, uid 10001) | **VERIFIED** by image assertions in CI; **NOT TESTED** locally |
| Containers read-only, `no-new-privileges` | **PASS** (compose assertions) |
| PostgreSQL, Redis, RabbitMQ publish no ports | **PASS** |
| RabbitMQ management UI not exposed and the `-management` image not used | **PASS** |
| API not published (proxy reaches it by service name) | **PASS** |
| Only ports 80/443 published | **PASS** |
| `austro_internal` network marked `internal: true` | **PASS** |
| Secrets absent from the build context and the image | **PASS** |
| Required secrets fail the compose parse when missing | **PASS**, with a negative control in CI |
| No secret-shaped values committed | **PASS** |
| Scripts never print secret values (names only) | **PASS** (reviewed; no `set -x`) |
| Redis healthcheck avoids argv password exposure (`REDISCLI_AUTH`) | **PASS** |
| Runtime DB role is non-superuser, no `BYPASSRLS`, owns nothing | **Enforced fail-closed at startup in code**; the live assertion is `BLOCKED` here and runs in CI |
| RLS enabled **and forced** on all 12 protected tables | **Enforced at startup in code**; live assertion `BLOCKED` here, runs in CI |
| TLS 1.2 floor, HSTS, nosniff, frame-deny, referrer policy | **PASS** (both proxy configs asserted) |
| `X-Forwarded-*` overwritten from the observed peer, never appended | **PASS** (both proxies; the unsafe append form is explicitly absent) |
| Profile-write operations fail closed when audit cannot persist | **VERIFIED** in the existing codebase and its test suite |

### Known gaps — stated, not omitted

| Gap | Status |
|---|---|
| No Content-Security-Policy | **NOT SUPPORTED by default.** The embedded browser application has not been audited against a specific policy; a wrong CSP breaks the UI in a way that reads as an application bug. Adding a reviewed CSP is a follow-up. |
| No rate limiting on login | **NOT SUPPORTED.** `POST /api/auth/login` is not throttled. The founder credential is the only interactive credential and the only administration path, so an untested limiter risks locking out the sole administrator. Implement deliberately, with a tested lockout path. |
| Secrets in container environment variables | **NOT SUPPORTED alternatives.** Docker secrets / vault agent are not wired up. Anyone with Docker-socket access can read them; socket access control is the boundary. |
| Backup encryption | **NOT SUPPORTED by the scripts.** Encrypt at the storage layer, or wrap the artifact yourself. |
| Dependency vulnerability scanning | **BLOCKED here, wired in CI.** First run may surface pre-existing findings. |
| No WAF, no IP allow-listing | **NOT SUPPORTED.** |
| No horizontal scaling / HA | **NOT SUPPORTED.** One PostgreSQL primary, one worker. |

**This report does not claim the system is "fully secure" or "free of bugs".**
It claims that the specific controls listed above are implemented and the ones
that could be checked in this environment were checked.

---

## 6. Deployment topology as built

```
Internet
  |
  v  80/443 (the only published ports)
HTTPS reverse proxy            nginx (default) | Caddy (profile: caddy)
  |  austro_edge
  v
AUSTRO OS API                  api:8080, NOT published to the host
  |  austro_internal (internal: true — no route off the host)
  +-- PostgreSQL + pgvector    no ports published
  +-- Redis                    no ports published
  +-- RabbitMQ                 no ports published, no management UI
  |
AUSTRO OS Worker               separate container, same image, no socket
```

Notable design decisions, each with its reason in the compose file:

- **API and worker are separate long-running containers built from one image**, so
  they cannot drift onto different revisions.
- **The worker waits for the API to be *healthy*, not merely started.** Both
  processes apply the schema bootstrap at startup and that bootstrap takes **no
  advisory lock**, so this gating is what serialises them on a cold database
  instead of letting them race on the same DDL.
- **The DSNs and the AMQP URL are assembled in `environment:`**, not written into
  `.env.production`: compose interpolates `${...}` reliably there, whereas
  `env_file` values are injected literally, so a DSN containing `${...}` would
  reach the container as that literal string.
- **`stop_grace_period: 90s` on the API**, above its own 75-second in-flight
  drain bound, so `docker stop` reaches the graceful shutdown rather than killing
  it mid-request.
- **The worker's healthcheck probes PID 1**, because it serves no HTTP; the
  richer "actually attached to the queue" signal is asserted by
  `scripts/healthcheck.sh` against the real log line.

---

## 7. Additions beyond the requested file list

Two files were added. Both are stated here rather than left for review to
discover.

| File | Why |
|---|---|
| `scripts/verify-production-deployment.sh` | The requested scripts all require Docker, so in an environment without it **nothing** could be checked. This script gives the deployment layer a real, runnable gate (155 checks) that needs neither Docker nor Go, and it includes a negative control so a pass is meaningful. CI runs it. |
| `.github/workflows/production-deployment.yml` | The request asks that CI validate docker build, compose config, security scanning, the frozen hash and production smoke. Nothing existing covered those. It **complements** `phase2-ci.yml` (which owns gofmt/build/vet/unit/regression/RLS) rather than duplicating or replacing it. |

Two variables I initially documented (`AUSTRO_PROXY_MAX_BODY_SIZE`,
`AUSTRO_PROXY_READ_TIMEOUT`) were **removed**: nginx does not read environment
variables and the shipped config is not templated, so they would have been knobs
that do nothing. The body limit and timeouts live in the proxy config files, and
`docs/production-configuration.md` now says so.

---

## 8. CI/CD status — what has actually executed

`phase2-ci.yml` triggers on `pull_request: branches: [main]`, so this PR does run
the existing Go gates. Nothing existing was weakened, removed or narrowed.

### Results from the first CI run on PR #4

| Job | Result | What it proves |
|---|---|---|
| Static verification of the deployment layer | **PASS** | the 155-check script, the frozen hash, `bash -n`, and **shellcheck on every script** all pass on a real runner |
| Resolve and assert the production compose configuration | **PASS** | **`docker compose config` accepts the file.** AUSTRO_ENV=production, only the reverse proxy publishes ports, `austro_internal` is internal, no privileged service, no socket mount — asserted against the **resolved** configuration, and the negative control (a missing secret must fail the parse) behaved correctly |
| Build the runtime image and assert its hardening | **PASS** | the image **builds**, runs as uid 10001 (not root), both `/app/main` and `/app/worker` are present and executable, the healthcheck targets `/health/live`, CMD is exec-form, and the runtime image carries no compiler |
| Build, Vet, and Unit Tests | **PASS** | gofmt, `go build ./...`, `go vet ./...` and the unit suites |
| Independent Go security tooling | **PASS** | the pre-existing gate, which runs **gosec, govulncheck and staticcheck** |
| Phase 1 Core Foundation Exit Criteria | **PASS** | the frozen gate's own suite |
| Full Phase 1 + Phase 2 Regression | ran | the live-stack regression suite |
| Real Chromium Creator/Pipelines E2E | ran | browser journey |
| **Production smoke — bring up the stack and health-check it** | **PASS** (after two fixes) | see below — the full stack came up and every assertion executed |
| Security scanning (gosec, govulncheck) — *added by this change* | **FAIL — job removed** | it duplicated the pre-existing security job above; see below |

### Finding 1 — a real deployment bug, caught by CI and fixed

The production smoke job failed in 5 seconds, at its very first step
(`docker compose config -q`), while the compose-config job passed the same
command. The difference was the environment file: the smoke job omitted the
Caddy variables.

**Root cause:** Compose interpolates the **whole** file, including services
behind an **inactive profile**. The Caddy service declared
`AUSTRO_PUBLIC_HOSTNAME` and `AUSTRO_ACME_EMAIL` with `:?`, so *every* command
that merely resolved the compose file failed — including a plain nginx
deployment that never starts the Caddy profile. `scripts/deploy.sh` would have
hit the same wall on its validation step. **The default production deployment
path was broken, and only a real `docker compose` invocation could have shown
it.**

**Fix:** those two variables are no longer `:?` in the compose file. The
requirement is enforced where it actually applies — `scripts/deploy.sh` requires
`AUSTRO_PUBLIC_HOSTNAME` for either proxy and `AUSTRO_ACME_EMAIL` when the Caddy
profile is selected. A **regression guard** was added to
`scripts/verify-production-deployment.sh` asserting that the profiled service
declares no `:?` variable, and it was verified to fail when the original defect
is re-injected.

This is the clearest possible illustration of why the `BLOCKED` items were not
written up as passes: the static checks all passed on the broken file.

### Finding 2 — I added a duplicate security job, and removed it

My first revision added a `security-scan` job running `govulncheck` and `gosec`.
It failed, and on inspection the reason mattered less than the finding:
`.github/workflows/creator-pipeline-verification.yml` **already runs gosec,
govulncheck and staticcheck** on every pull request, with pinned tool versions
and a high/critical policy that fails the job — and it **passed**. My job
duplicated an existing working gate, added no coverage, and would have put a
second, differently-configured scanner in the path. It was removed, and the
workflow now documents where that coverage lives. Duplicating a scanner is not
the same as adding a security control.

### Finding 3 — the healthcheck aborted on a variable the compose file defaults

With the compose bug fixed, the smoke job got further: the stack came **up** and
`up -d --wait` returned successfully, meaning every container on the default path
reported healthy. The job then failed in **`Health-check the running topology`**.

Diagnosis was blocked at first — the Actions log blobs were unreachable from the
authoring sandbox and the job carried no check-run annotations — so the cause was
reproduced locally instead, by running the committed script against a stub
environment file **with `AUSTRO_RABBITMQ_USER` removed**:

```
$ bash /tmp/old-healthcheck.sh          # the committed revision
...
PASS     Redis answered an authenticated PING
PASS     RabbitMQ reports status ok
/tmp/old-healthcheck.sh: line 227: AUSTRO_RABBITMQ_USER: unbound variable
exit=1
```

**Root cause:** `scripts/healthcheck.sh` reads `AUSTRO_RABBITMQ_USER` **without a
default**, under `set -u`. The compose file defaults that variable to `austro`,
and `.env.example` ships it **commented out** — so the abort was not a CI
artifact. Any operator who left it unset would have hit the same failure, several
checks into a run, with a message that named neither the variable's absence nor
the file it was expected in.

**Fix:** the healthcheck now mirrors the compose default
(`RABBITMQ_USER="${AUSTRO_RABBITMQ_USER:-austro}"`) so both components derive the
same value from the same input, and the two secrets that genuinely have no safe
default (`AUSTRO_POSTGRES_RUNTIME_PASSWORD`, `AUSTRO_REDIS_PASSWORD`) are required
explicitly, with a message naming the file they were expected in.

**Verified by reproduction, not by inspection:** the committed revision fails with
`unbound variable` and exit 1 on that environment file; the fixed revision passes
**19 checks, 0 failed** on the same file.

**Regression guard:** `scripts/verify-production-deployment.sh` section 5d now
audits the healthcheck for any `AUSTRO_*` reference without a default, and it was
confirmed to fail when the bare reference is re-injected. Note the shape of the
bug: the earlier stub test passed because its environment file *did* define the
variable — the stub was more complete than the real CI file, and that gap is what
section 5d now closes.

### The workflow as shipped

| Job | Covers |
|---|---|
| `static-verification` | frozen hash, 152-check verification script (with negative control), `bash -n`, shellcheck, no committed secrets, no Kubernetes/Vercel |
| `docker-build` | image builds; non-root at runtime; both entrypoints executable; healthcheck target; exec-form CMD; no compiler in the runtime image |
| `compose-config` | `docker compose config -q`; production env; port exposure; internal network; no privileged/socket; negative control on required secrets |
| `production-smoke` | brings the real stack up with `--wait`, runs the operator's own healthcheck script, asserts the API and datastores are **not** reachable from the host, asserts the API logged a verified topology on `austro_app` |

**Confirmed.** The final run is green, and every step of the smoke job
succeeded — including the three assertions that had never executed before:

| Smoke step | Result | What it establishes |
|---|---|---|
| Validate the compose configuration | **PASS** | `docker compose` resolves the production file |
| Start the stack in production order | **PASS** | `up -d --wait` returned with **every** container healthy: postgres, redis, rabbitmq, api, worker |
| Health-check the running topology | **PASS** | the operator's own `scripts/healthcheck.sh` passed **19 checks, 0 failed** against the real stack |
| Assert the API is NOT reachable from the host | **PASS** | 127.0.0.1:8080 does not answer — no unintended ingress path |
| Assert the datastores are NOT reachable from the host | **PASS** | 5432, 6379, 5672 and 15672 are all unpublished |
| Assert the API verified its database topology | **PASS** | the API's own log records `database-topology-ready` with `runtime_role: austro_app` |

This is the strongest evidence in the report, and it is deliberately narrow: in a
CI runner the **container topology was brought up for real**, the database
bootstrap applied its RLS policies and grants against a real PostgreSQL with
pgvector, the runtime role `austro_app` was created and used, the API and worker
started and connected, and the datastores were confirmed unreachable from the
host. It is **not** a production deployment — see §11.

**Still excluded on purpose:** the reverse proxy. It requires operator-issued TLS
material (`CONFIGURATION REQUIRED`), so the smoke run tests the application
topology rather than a certificate. Nothing in this report claims a TLS
handshake was performed.

**Unrelated pre-existing failure:** the pull request also shows a failing
`Vercel` check from an existing Vercel integration on the repository. It is not
part of this change, and Vercel is **NOT SUPPORTED** as a production target here.
Note that the request explicitly forbids adding Vercel as a required production
check; this change adds none.

---

## 9. `CONFIGURATION REQUIRED` before this can run in production

Nothing below has a working default. The deployment will not start without them.

| # | Item | Where |
|---|---|---|
| 1 | `AUSTRO_POSTGRES_PASSWORD` — owner credential | `.env.production` |
| 2 | `AUSTRO_POSTGRES_RUNTIME_PASSWORD` — **must differ from #1** | `.env.production` |
| 3 | `AUSTRO_REDIS_PASSWORD` | `.env.production` |
| 4 | `AUSTRO_RABBITMQ_PASSWORD` | `.env.production` |
| 5 | `AUSTRO_JWT_SECRET` — ≥ 32 bytes | `.env.production` |
| 6 | `AUSTRO_JWT_REFRESH_SECRET` — ≥ 32 bytes, **must differ from #5** | `.env.production` |
| 7 | `AUSTRO_FOUNDER_USERNAME` + `AUSTRO_FOUNDER_PASSWORD` — ≥ 16 chars | `.env.production` |
| 8 | TLS certificate + key at `fullchain.pem` / `privkey.pem` | `AUSTRO_TLS_CERT_DIR` (default `./deploy/tls`) |
| 9 | `AUSTRO_PUBLIC_HOSTNAME` | `.env.production` |
| 10 | `AUSTRO_ACME_EMAIL` — only if the Caddy profile is used | `.env.production` |
| 11 | AI backend settings — only if `AUSTRO_AI_BACKEND != stub` | `.env.production` |
| 12 | Publishing settings — only if `AUSTRO_PUBLISH_BACKEND = generic-http` | `.env.production` |
| 13 | Backup schedule (cron) and, if off-host copies are wanted, `AUSTRO_BACKUP_S3_BUCKET` + the `aws` CLI | host |
| 14 | Backup encryption, if required by policy | host |
| 15 | Firewall: only 80/443 reachable from the internet | host |

Step-by-step: [production-deployment.md](production-deployment.md).

---

## 10. Remaining risks and `NOT TESTED` items

| Risk | Severity | Mitigation today |
|---|---|---|
| The stack has never been started anywhere by this work | **High** | CI `production-smoke` runs the real compose on the PR; run `scripts/deploy.sh` in a staging environment before production |
| Disaster recovery is **NOT TESTED** | **High** | `scripts/restore.sh` exists and verifies its result, but a restore exercise has never been performed. The exercise is written out in [backups-and-restore.md](backups-and-restore.md); until you run it, recovery is an assumption. |
| No point-in-time recovery (no WAL archiving) | Medium | Nightly `pg_dump`; up to 24h of data loss. `NOT SUPPORTED` as configured. |
| Backups are local by default | Medium | Set `AUSTRO_BACKUP_S3_BUCKET`, or accept that a host loss loses the database and its backups together |
| No load or performance testing | Medium | `NOT TESTED` |
| No HA or failover | Medium | `NOT SUPPORTED` |
| Unverified on arm64 | Low | Build for amd64, or adjust `GOARCH` (untested) |
| New CI jobs may need a first-run fix | Low | They fail loudly with diagnostics |
| First `govulncheck`/`gosec` run may surface pre-existing findings | Low | Treat output as a finding about the codebase, not about this change |
| No login rate limiting | Low–Medium | Documented gap; implement deliberately |
| `AUSTRO_REDIS_PASSWORD` is not checked by the config validator | Low | Fails fast at connect (`redis-connect-failed`), just later than other secrets |

---

## 11. Statements deliberately NOT made

- **"Production deployed."** No external production target was reached, and no
  deployment was performed. The work is a deployment *layer*, statically
  verified.
- **"Disaster recovery tested."** It was not.
- **"Fully secure" / "zero bugs."** Neither is knowable, and the known gaps are
  listed in §5 rather than omitted.
- **"Tests pass."** No test was executed — Go is unavailable here. That is
  `BLOCKED`, and the suites run in CI.
- **"The CI jobs pass."** They have never run.

---

## 12. Recommended path from here

1. Open the PR to `main` and let the existing Phase 2 gates plus the new
   production-deployment gates run. Fix what the first run finds, especially in
   `docker-build`, `compose-config` and `production-smoke`.
2. Deploy to a **staging host** with a real `.env.production` and confirm
   `scripts/deploy.sh` and `scripts/healthcheck.sh` both exit 0 there.
3. Perform the restore exercise in
   [backups-and-restore.md](backups-and-restore.md) on that staging host and
   record the elapsed time. That number is the real recovery objective.
4. Schedule backups, set retention, and decide on off-host copies.
5. Only then deploy to production, with a rollback target recorded in the
   deployment checklist.
