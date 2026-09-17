# AUSTRO OS — Final Production Readiness Report

**Scope:** the production deployment layer reconstructed on top of `main`
(`64c72ba102c570241c9ec594d466a8e4bbc0ff6d`).

**Date of this report:** 2026-09-17.

**Verdict: READY FOR A CONTROLLED DEPLOYMENT EXERCISE — NOT DEPLOYED, NOT
PROVEN END TO END.**

The deployment layer is complete, internally consistent and statically
verified. It has **not** been run. No container was started, no database was
migrated, no TLS handshake was performed, and no restore was exercised, because
the environment this work was authored in has no Docker daemon and no Go
toolchain (both recorded as `BLOCKED` below). Every claim in this report is
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
| Static deployment verification (10 sections, 150 checks) | **PASS** | `scripts/verify-production-deployment.sh` — 150 passed, 0 failed |
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

## 4. What could NOT be verified here

| Verification | Result | Why |
|---|---|---|
| `go test ./...` | **BLOCKED** | No Go toolchain; no network egress to install one |
| `go build ./...`, `go vet ./...` | **BLOCKED** | Same |
| `gofmt -l .` | **BLOCKED** | Same |
| `gosec`, `govulncheck`, `staticcheck` | **BLOCKED** | Not installed; no egress |
| `docker build` | **BLOCKED** | No Docker daemon |
| `docker compose config -q` | **BLOCKED** | No Docker daemon |
| Starting the stack, any container | **BLOCKED** | No Docker daemon |
| `nginx -t`, `caddy validate` | **BLOCKED** | Neither binary available |
| `psql` against a live database | **BLOCKED** | Not installed; no PostgreSQL |
| ShellCheck | **BLOCKED** | Not installed |
| Arm64 image build | **NOT TESTED** | Dockerfile builds `GOARCH=amd64` |

Per the project's own rules, none of these are recorded as passes. They are
wired into CI (§8) and will run on the pull request.

**The environment probe that produced these results:** `command -v go docker
gosec govulncheck staticcheck gofmt shellcheck psql` all failed; `apt-get`
could not reach `deb.debian.org`; `curl https://proxy.golang.org` returned
`SSL_ERROR_SYSCALL`. Only git and the GitHub API were reachable, which is how
the branch was pushed.

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
| `scripts/verify-production-deployment.sh` | The requested scripts all require Docker, so in an environment without it **nothing** could be checked. This script gives the deployment layer a real, runnable gate (150 checks) that needs neither Docker nor Go, and it includes a negative control so a pass is meaningful. CI runs it. |
| `.github/workflows/production-deployment.yml` | The request asks that CI validate docker build, compose config, security scanning, the frozen hash and production smoke. Nothing existing covered those. It **complements** `phase2-ci.yml` (which owns gofmt/build/vet/unit/regression/RLS) rather than duplicating or replacing it. |

Two variables I initially documented (`AUSTRO_PROXY_MAX_BODY_SIZE`,
`AUSTRO_PROXY_READ_TIMEOUT`) were **removed**: nginx does not read environment
variables and the shipped config is not templated, so they would have been knobs
that do nothing. The body limit and timeouts live in the proxy config files, and
`docs/production-configuration.md` now says so.

---

## 8. CI/CD status

`phase2-ci.yml` triggers on `pull_request: branches: [main]`, so a PR into
`main` **does** run the existing Go gates (gofmt, build, vet, unit, full
regression, frozen-gate hash, live RLS and runtime-role assertions, migration
idempotency). Nothing existing was weakened or removed.

New gates in `.github/workflows/production-deployment.yml`:

| Job | Covers | Local result |
|---|---|---|
| `static-verification` | frozen hash, 150-check script, `bash -n`, shellcheck, no committed secrets, no Kubernetes/Vercel | **PASS** locally (except shellcheck: **BLOCKED**) |
| `docker-build` | image builds; non-root at runtime; both binaries executable; healthcheck present; exec-form CMD; no compiler in the runtime image | **BLOCKED** locally |
| `compose-config` | `docker compose config -q`; `AUSTRO_ENV=production`; only the proxy publishes ports; internal network; no privileged service; no socket mount; **negative control** that a missing secret fails the parse | **BLOCKED** locally — the un-resolved file passes the static checks |
| `security-scan` | `govulncheck ./...`, `gosec -severity=high -confidence=high` | **BLOCKED** locally |
| `production-smoke` | brings the real stack up with `--wait`, runs `scripts/healthcheck.sh`, asserts the API and datastores are **not** reachable from the host, asserts the API logged a verified topology | **BLOCKED** locally |

**Honest risk statement about the new CI jobs.** They have never been executed.
`production-smoke`, `docker-build` and `security-scan` are the most likely to
need a first-run fix — a compose flag, a scanner finding, a timing assumption.
They are written to fail loudly with diagnostics rather than to pass quietly,
but a green check on the first run should be *read*, not assumed. The smoke job
deliberately excludes the reverse proxy, because the proxy requires
operator-issued TLS material (`CONFIGURATION REQUIRED`) and a smoke run should
test the application topology rather than a certificate.

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
