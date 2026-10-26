# AUSTRO OS — Production Configuration

Every setting a production deployment needs, what happens if it is missing, and
which ones must never be defaulted. `.env.example` is the template; this document
explains it.

The authoritative source is the code: `internal/config/config.go` (which reads
and validates the environment), `infrastructure/database/roles.go` (the three
database principals), `infrastructure/redis/redis.go` and
`infrastructure/rabbitmq/*` (cache and broker credentials).

---

## Classification

| Class | Meaning |
|---|---|
| **REQUIRED** | No default. The process exits at startup (`config.LoadStrict`) without it. Production compose additionally refuses to parse. |
| **CONFIGURATION REQUIRED** | The system runs without it, but the capability it names does not work until it is supplied. Never silently defaulted in production. |
| **OPTIONAL** | Safe to omit; a documented behaviour applies. |
| **SAFE DEFAULT** | The system picks a working value; overriding is unusual. |
| **DEVELOPMENT ONLY** | Acceptable locally, not in production. |
| **NOT SUPPORTED** | Deliberately unimplemented. Do not expect it to work. |

---

## Secrets

Generate every value in this section with `openssl rand -hex 32`, and never
commit the result. `.gitignore` excludes `.env.*`, so a file named
`.env.production` is not trackable.

| Variable | Class | Contract |
|---|---|---|
| `AUSTRO_POSTGRES_PASSWORD` | **REQUIRED** | Owner credential. Used only to bootstrap schema, roles and migrations. |
| `AUSTRO_POSTGRES_RUNTIME_PASSWORD` | **REQUIRED** | Password for the role that **serves traffic**. Must differ from the owner password. |
| `AUSTRO_REDIS_PASSWORD` | **REQUIRED** | Redis `requirepass` value, read by the application at connect time. |
| `AUSTRO_RABBITMQ_PASSWORD` | **REQUIRED** | Broker credential, used both to provision the broker and to build the AMQP URL. |
| `AUSTRO_JWT_SECRET` | **REQUIRED** | Access-token signing key. Minimum **32 bytes** (RFC 7518 §3.2). |
| `AUSTRO_JWT_REFRESH_SECRET` | **REQUIRED** | Refresh-token signing key. Minimum 32 bytes. Must differ from the access key. |
| `AUSTRO_FOUNDER_USERNAME` / `AUSTRO_FOUNDER_PASSWORD` | **REQUIRED** (production policy) | The single founder identity created by `POST /api/auth/bootstrap`. Both together or neither. Password minimum 16 characters. |

Why these two pairs must differ, precisely:

- **Owner vs runtime password.** PostgreSQL applies neither `ENABLE` nor `FORCE
  ROW LEVEL SECURITY` to a table's **owner**. If the serving connection were the
  owner, every workspace policy would be silently inert while the deployment
  looked correctly configured.
- **Access vs refresh JWT key.** With one shared key, a refresh token is a valid
  access token, and the shorter-lived/short-scope distinction disappears.

`scripts/deploy.sh` enforces all of the above before it starts anything, and
reports the offending setting **name** — never a value.

---

## Build identity

The production compose file will not parse without these two values. An image
that cannot name its source commit looks correct and cannot be traced later, so
a missing identity has to fail the deploy rather than produce one. Neither value
is a secret.

| Variable | Class | Contract |
|---|---|---|
| `AUSTRO_BUILD_COMMIT` | **REQUIRED** | Full 40-character lowercase git SHA of the source being built. Never a branch name, a short SHA or the literal `HEAD`. |
| `AUSTRO_BUILD_VERSION` | **REQUIRED** | Release version stamped into the image label and into both binaries. |

`docker-compose.production.yml` declares both with compose's `:?` form, so an
unset or empty value aborts the parse with an error that names the variable,
instead of building an image that reports `commit=unknown`. The Dockerfile
independently refuses an empty version, and refuses a commit that is not 40
lowercase hex characters, so a wrong value fails twice rather than once.

`scripts/deploy.sh` resolves them when the environment file leaves them unset:

- the commit comes from `git rev-parse HEAD` of the tree being deployed, and the
  deploy stops if that tree has uncommitted tracked changes — `HEAD` would not
  describe the source being built;
- the version comes from the Dockerfile's `ARG VERSION` default, which is the
  single place the release version is declared.

Set both explicitly when deploying something that is not a git checkout, such as
a release tarball, using the identity recorded for that release. CI takes the
commit from the workflow run (`github.sha`) and the version from the same
Dockerfile default, so a compose-resolved release and an image-built release
cannot disagree.

A deployed process reports what it was stamped with:

```bash
docker compose --env-file .env.production -f docker-compose.production.yml \
  exec -T api /app/main --version
# austro-os api version=1.0 commit=<40-character sha>
```

---

## Database

The application runs **three principals**; this is the security model, not an
implementation detail.

| Principal | Credential | Used for |
|---|---|---|
| owner | `AUSTRO_POSTGRES_DSN` | `CREATE EXTENSION/TABLE/INDEX/POLICY/ROLE`, migrations. Closed before the process serves a request. |
| runtime | `AUSTRO_POSTGRES_RUNTIME_DSN` | **Every request-scoped query.** Not a superuser, no `BYPASSRLS`, owns nothing. |
| admin | derived, role `AUSTRO_POSTGRES_ADMIN_USER` | Two narrow organisation-level operations a workspace-scoped policy cannot express: founder workspace list/create, and appending to / verifying the audit chain. Also not a superuser and no `BYPASSRLS`; its reach comes from explicit policies naming it. |

| Variable | Class | Notes |
|---|---|---|
| `AUSTRO_POSTGRES_DSN` | **REQUIRED** | Owner DSN. Rejected if it contains a placeholder or a loopback host. |
| `AUSTRO_POSTGRES_RUNTIME_DSN` | **REQUIRED** (derived when empty) | When unset it is derived from the owner DSN by swapping in `AUSTRO_POSTGRES_RUNTIME_USER` and `AUSTRO_POSTGRES_RUNTIME_PASSWORD`. Rejected if it equals the owner DSN. |
| `AUSTRO_POSTGRES_RUNTIME_USER` | **SAFE DEFAULT** `austro_app` | Must match `^[a-z_][a-z0-9_]{0,62}$` — role names cannot be bind parameters, so they are pattern-checked before being interpolated into DDL. |
| `AUSTRO_POSTGRES_ADMIN_USER` | **SAFE DEFAULT** `<runtime>_admin` | Same pattern rule. |
| `AUSTRO_POSTGRES_DB` | **SAFE DEFAULT** `austro` | Read by the compose file, not the application. |
| `AUSTRO_POSTGRES_USER` | **SAFE DEFAULT** `austro` | Ditto; also the owner role the container is initialised with. |

**How the DSNs are assembled in production.** Compose interpolates `${...}`
reliably in an `environment:` block, but an `env_file:` value is injected
**literally** — a DSN written as
`postgres://austro:${AUSTRO_POSTGRES_PASSWORD}@...` inside `.env.production`
would reach the container as that literal string. `docker-compose.production.yml`
therefore builds both DSNs in `environment:` from their parts, and those keys
override anything of the same name in the env file. Set them explicitly only to
override the derivation (a managed database, or TLS parameters).

**Startup verification.** Before serving, the process applies the bootstrap as
the owner, opens the runtime and admin pools, and then verifies the runtime pool
against the live catalog: not a superuser, no `BYPASSRLS`, owns no protected
table, all protected tables have RLS both enabled and **forced**, and it cannot
read another workspace's rows. Any failure terminates the process
(`database-runtime-security-failed`). This is why a compromised or
misconfigured runtime role cannot be papered over by configuration alone.

**Migrations.** There is no separate migration tool or migration container. The
bootstrap is idempotent and applied at every API and worker start — a database
created before a feature existed is upgraded on the next boot rather than left
behind. Because the bootstrap takes **no advisory lock**, two processes must not
run it concurrently against a cold database: `docker-compose.production.yml`
gates the worker on the API being healthy, which serialises them.
**CONFIGURATION REQUIRED to change this:** if you start the worker and API
simultaneously by other means, preserve that ordering yourself.

**Host authentication.** `docker-compose.production.yml` starts PostgreSQL with
`-c hba_file=/etc/postgresql/pg_hba.conf` and mounts
`deploy/postgres/pg_hba.conf` there read-only. `hba_file` is a postmaster-start
GUC, so this file is the effective host authentication at every start **and**
reload, including an already-seeded `postgres_data` volume — it does not depend
on initdb, and it survives container recreation.

The image's own initdb default hardcodes loopback TCP as `trust` no matter what
auth method you pass to it, which would silently accept *any* password on
`127.0.0.1`. That matters because `scripts/healthcheck.sh` authenticates the
runtime role over that exact path. The managed file removes the loophole:

| Rule | Method | Who uses it |
|---|---|---|
| `local all all` (Unix socket) | `trust` | Operator sessions and `scripts/backup.sh` / `scripts/restore.sh` inside the container. The socket is not reachable from outside; see `docs/backups-and-restore.md`. |
| `host all all 127.0.0.1/32`, `::1/128` | `scram-sha-256` | The healthcheck's runtime-credential check. A wrong password now fails authentication instead of round-tripping. |
| `host all all all` | `scram-sha-256` | The API and worker over the private bridge (the DSNs they actually use). |

Expected values are asserted at runtime by the production deployment workflow
(`pg_hba_file_rules` / `pg_settings.hba_file`) and in the wrong-credential
negative control.

---

## Cache and broker

| Variable | Class | Notes |
|---|---|---|
| `AUSTRO_REDIS_ADDR` | **REQUIRED** | `SAFE DEFAULT` in compose: `redis:6379`. The whole connection configuration; its form decides the transport. `host:port` and `redis://host:port` are plaintext, `rediss://host:port` is TLS 1.2+ with the certificate verified. A loopback value is rejected unless `AUSTRO_ALLOW_LOOPBACK` is set. |
| `AUSTRO_REDIS_CA_FILE` | **OPTIONAL** | PEM bundle of extra certificate authorities for a `rediss://` address issued by a private CA. Added to the system roots, never substituted for them. Rejected with a plaintext address. A missing or unusable file is fatal at startup. Certificate verification cannot be disabled by any setting. |
| `AUSTRO_ALLOW_LOOPBACK` | **OPTIONAL** | `1`/`true`/`yes` to permit a loopback endpoint, which CI needs because its databases are containers with published ports. Any other value leaves the rejection in force. |
| `AUSTRO_REDIS_PASSWORD` | **REQUIRED** | Redis `requirepass` value, held as its own setting rather than as userinfo in `AUSTRO_REDIS_ADDR` so the secret never travels inside a URL. Carried on `Config.RedisPassword` and validated by `Config.Validate`, which rejects a placeholder (`change-me`, `changeme`, `example`) but permits empty, because a server with no `requirepass` needs no credential. The value is never logged and never appears in a validation message. A wrong-but-non-placeholder value still surfaces as `redis-connect-failed` at startup: the server is the only thing that can know it is wrong. |
| `AUSTRO_RABBITMQ_URL` | **REQUIRED** | Derived in compose from `amqp://<user>:<password>@rabbitmq:5672/`. |
| `AUSTRO_RABBITMQ_USER` | **REQUIRED** | `SAFE DEFAULT` `austro`. |
| `AUSTRO_RABBITMQ_QUEUE` | **SAFE DEFAULT** `austro.events` | Declared durable and non-auto-delete by both processes. This is the **event** queue only — it is not the execution queue (see *Execution transport*). Setting it does not move AI executions. |

---

## Runtime and adapters

| Variable | Class | Notes |
|---|---|---|
| `AUSTRO_ENV` | **SAFE DEFAULT** `production` in compose | Recorded in startup logs. Does not itself change application behaviour. |
| `AUSTRO_SERVER_ADDR` | **SAFE DEFAULT** `0.0.0.0:8080` | Inside the container. Not published to the host. |
| `AUSTRO_WORKSPACE_ID` | **OPTIONAL**, default `default` | Fallback workspace when a call carries no workspace context. |
| `AUSTRO_AI_BACKEND` | **SAFE DEFAULT** `stub` | `stub` = offline deterministic adapter, no credential, no egress. |
| `AUSTRO_AI_MODEL`, `AUSTRO_AI_BASE_URL` | **CONFIGURATION REQUIRED** for `local` and `openai-compatible` | |
| `AUSTRO_AI_API_KEY` | **CONFIGURATION REQUIRED** for `openai-compatible`; **OPTIONAL** for `local` | A keyless in-house endpoint is a supported configuration. |
| `AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE` | **OPTIONAL**, `0` = no ceiling | Must parse as an unsigned integer or startup fails. |
| `AUSTRO_PUBLISH_BACKEND` | **SAFE DEFAULT** `stub` | `generic-http` selects real delivery. |
| `AUSTRO_PUBLISH_WEBHOOK_URL`, `AUSTRO_PUBLISH_TOKEN` | **CONFIGURATION REQUIRED** for `generic-http` | |
| `AUSTRO_PUBLISH_IDEMPOTENCY_FIELD` | **OPTIONAL** | Provider-specific idempotency field; omitted means a deterministic key with no provider field attached. |
| `AUSTRO_PUBLISH_MAX_ATTEMPTS`, `AUSTRO_PUBLISH_RETRY_BACKOFF_BASE`, `AUSTRO_PUBLISH_RETRY_BACKOFF_MAX` | **OPTIONAL** | Unset means one attempt and the adapter's default backoff. A supplied value that does not parse is a startup failure, not a silent fallback. |

Two strictness rules worth knowing before you set anything here:

1. Selecting a **stub** backend while its external settings are present is
   itself rejected. A configured credential is never silently ignored.
2. The validator rejects any value containing `change-me`, `changeme`,
   `example`, or a loopback host (`localhost:5432`, `localhost:6379`,
   `localhost:5672`). These placeholders cannot be deployed by accident.
3. In production, the `stub` AI, publishing and execution backends are refused
   outright unless `AUSTRO_ALLOW_STUB_BACKENDS` is explicitly true. See below.

---

## Execution transport

The AI execution subsystem is split across two processes: the **API** process
owns the dispatcher that publishes claimed work, and `cmd/worker` owns the
consumer that executes it. They are separate binaries sharing nothing but
PostgreSQL and the broker, so the queue name is the only thing joining them — and
it is configuration, not a constant, precisely so a rename can be applied to both
sides.

| Variable | Class | Notes |
|---|---|---|
| `AUSTRO_EXECUTION_TRANSPORT_BACKEND` | **SAFE DEFAULT** `stub`; **CONFIGURATION REQUIRED** in production | `stub` executes in-process and touches no broker. `rabbitmq` splits dispatch from execution across the two processes. |
| `AUSTRO_EXECUTION_QUEUE` | **SAFE DEFAULT** `austro.executions` | **Must be identical for the API and the worker.** A blank or whitespace-only value is a startup failure, not a fallback — a per-process default would let the two sides silently use different queues. Not the same setting as `AUSTRO_RABBITMQ_QUEUE`. |
| `AUSTRO_EXECUTION_WORKSPACE_IDS` | **CONFIGURATION REQUIRED** when the transport is `rabbitmq` in production | Comma-separated workspace UUIDs the dispatcher is permitted to serve. In production, a broker-backed dispatcher with this empty is a startup failure: it would connect, log `execution-dispatch-started`, and dispatch nothing. In development it is a deliberate no-op (`execution-dispatch-idle`), and it is not required for the `stub` transport. |
| `AUSTRO_EXECUTION_LEASE_TTL` | **SAFE DEFAULT** `60s` | How long a claim survives a worker that dies without releasing it. |
| `AUSTRO_EXECUTION_HEARTBEAT_INTERVAL` | **SAFE DEFAULT** `20s` | Renewal period for a live claim. Must be well under the TTL or healthy workers will lose their leases. |
| `AUSTRO_EXECUTION_WINDOW` | **SAFE DEFAULT** `50s` | How far into the future work is claimed for. Should exceed execution time and the TTL, so a slow execution is not handed to a second worker. |
| `AUSTRO_EXECUTION_SWEEP_INTERVAL` | **SAFE DEFAULT** `5s` | Cadence for reaping expired leases. |
| `AUSTRO_EXECUTION_RETRY_BACKOFF_BASE`, `AUSTRO_EXECUTION_RETRY_BACKOFF_MAX` | **SAFE DEFAULT** `2s` / `2m` | Exponential retry ceiling. Base must not exceed max. |

A duration that does not parse, is not positive, or breaks one of these
relationships is rejected at startup by name:

- `AUSTRO_EXECUTION_HEARTBEAT_INTERVAL` must be **less than** the lease TTL. A
  heartbeat at or beyond the TTL lets a claim expire underneath a worker that is
  still running, so a second worker picks up the same task.
- `AUSTRO_EXECUTION_WINDOW` must be **less than** the lease TTL, for the same
  reason: work claimed beyond the TTL can expire before anyone executes it.
- `AUSTRO_EXECUTION_RETRY_BACKOFF_BASE` must not exceed the max.
- Every entry in `AUSTRO_EXECUTION_WORKSPACE_IDS` must parse as a UUID.

The reason these are startup failures rather than warnings is that they govern
how quickly a crashed worker's task is retried and how often a *healthy* worker
loses its claim. Both failure modes are duplicated or stalled work, and both are
invisible from outside the two processes — everything keeps reporting healthy.

### Seeing whether execution is actually running

`execution-dispatch-started` logs the resolved queue, transport and workspace
count. Confirming the deployment:

```bash
docker compose logs api | grep execution-dispatch-started
# execution-dispatch-started component=execution transport=rabbitmq queue=austro.executions workspaces=2
```

`workspaces:0` with `transport:rabbitmq` means the dispatcher is up and sweeping
nothing.

---

## The production stub refusal

| Variable | Class | Notes |
|---|---|---|
| `AUSTRO_ALLOW_STUB_BACKENDS` | **SAFE DEFAULT** `false` | Set `true` only for a deliberately offline production-shaped stack — a demo, a rehearsal, or a no-egress site. An unparseable value is rejected rather than read as consent. |

A stub adapter answers **successfully without contacting anything**. A production
deployment left on the offline defaults would serve traffic, pass
`/health/ready`, perform no real work, and never report a problem — the exact
failure that a release gate exists to prevent. `AUSTRO_ENV=production`
(case-insensitive, whitespace-trimmed) therefore refuses `stub` for the AI,
publishing and execution backends together.

The offline **development** default is deliberately untouched: a fresh clone still
runs with no configuration, no account and no egress.

This is a startup failure, not a log line. Refusing is the only outcome that gets
noticed — a warning would scroll past, and the deployment would come up looking
exactly like a working one.

---

## Reverse proxy and TLS

Read by `docker-compose.production.yml`, never by the Go application.

| Variable | Class | Notes |
|---|---|---|
| `AUSTRO_TLS_CERT_DIR` | **CONFIGURATION REQUIRED** | Host directory bind-mounted read-only at `/etc/nginx/tls`. `SAFE DEFAULT` `./deploy/tls`. |
| (files inside it) | **CONFIGURATION REQUIRED** | `fullchain.pem` and `privkey.pem`, exactly these names — `deploy/nginx/nginx.conf` references them. The dir must be readable by the nginx worker (uid 101). |
| `AUSTRO_PUBLIC_HOSTNAME` | **CONFIGURATION REQUIRED** | Hostname the proxy serves. No default: a wrong value serves the wrong site or fails issuance. Required by the Caddy profile. |
| `AUSTRO_ACME_EMAIL` | **CONFIGURATION REQUIRED** when the Caddy profile is used | ACME account contact. |
| `AUSTRO_HTTP_PORT` / `AUSTRO_HTTPS_PORT` | **SAFE DEFAULT** `80` / `443` | The only publicly bound ports. |
| `AUSTRO_PROXY_SERVICE` | **OPTIONAL**, `SAFE DEFAULT` `reverse-proxy` | `reverse-proxy-caddy` selects the automatic-TLS variant (also needs the two variables above). |
| `AUSTRO_ENV_FILE` | **OPTIONAL**, `SAFE DEFAULT` `.env.production` | The file the operator scripts source and pass to compose. |
| `AUSTRO_HEALTHCHECK_SKIP_PROXY` | **OPTIONAL**, `SAFE DEFAULT` `0` | `1` skips the proxy checks in `scripts/healthcheck.sh`; used by the CI smoke job, which does not start a proxy. |
| the proxy's body limit and timeouts | **NOT SUPPORTED** as environment variables | nginx does not read environment variables and the shipped config is not templated, so the body limit (`2m` / `2MB`) and timeouts live in `deploy/nginx/nginx.conf` and `deploy/caddy/Caddyfile`. Editing the config file is the supported way to change them. |

**Certificate acquisition is your responsibility with nginx** (`CONFIGURATION
REQUIRED`). Caddy obtains and renews certificates automatically, which is why
that variant exists; select it with
`AUSTRO_PROXY_SERVICE=reverse-proxy-caddy` and the compose profile `caddy`.

---

## How secrets reach a container, and the limits of that

Values are injected as **environment variables** from `.env.production`. Be clear
about what that does and does not protect against:

- They do **not** appear in the image. Nothing is baked in, and `.dockerignore`
  keeps `.env*` out of the build context entirely.
- They are **not** written into any log line. Error messages name settings, not
  values. Scripts report checksums and paths, never credentials. Redis
  healthchecks use `REDISCLI_AUTH` specifically to keep the password out of the
  process argument list, where `ps` would show it.
- They **are** visible to anyone who can run `docker inspect` or read the compose
  file — i.e. anyone with access to the Docker socket. Access control on that
  socket is the boundary.
- **Docker secrets, a vault agent, or an external secret manager are NOT
  SUPPORTED by this compose file.** If your threat model requires secrets that
  never enter a container's environment, that is a change to the deployment
  layer, not a configuration toggle.

**Rotation.** Changing `AUSTRO_JWT_SECRET` invalidates every issued access token
(sessions end; users log in again). Changing `AUSTRO_JWT_REFRESH_SECRET`
invalidates refresh tokens. Changing a database password requires changing it in
PostgreSQL *and* in the env file in the same window. Changing
`AUSTRO_FOUNDER_PASSWORD` after bootstrap does not retroactively change the
stored credential — the bootstrap endpoint refuses once a founder exists.

---

## Development-only values that must never reach production

| Value | Where it comes from |
|---|---|
| `AUSTRO_ENV=development` | Local `.env`; the production compose forces `production`. |
| `AUSTRO_AI_BACKEND=stub`, `AUSTRO_PUBLISH_BACKEND=stub`, `AUSTRO_EXECUTION_TRANSPORT_BACKEND=stub` | Offline defaults from `.env.example`. Refused at startup in production unless `AUSTRO_ALLOW_STUB_BACKENDS=true`. |
| `AUSTRO_EXECUTION_TRANSPORT_BACKEND=rabbitmq` with empty `AUSTRO_EXECUTION_WORKSPACE_IDS` | A startup failure in production. |
| Loopback DSNs (`localhost:5432`, `localhost:6379`, `localhost:5672`) | Rejected outright by the validator. |
| `change-me*` placeholders | Rejected outright by the validator, and by `scripts/deploy.sh`. |
| Comparing `docker-compose.yml` for production use | The dev compose publishes database, cache and broker on loopback and enables no auth on Redis' peer ports. It is a development file. Production is `docker-compose.production.yml`. |
