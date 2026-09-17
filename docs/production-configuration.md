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
| `AUSTRO_REDIS_ADDR` | **REQUIRED** | `SAFE DEFAULT` in compose: `redis:6379`. A loopback value is rejected. |
| `AUSTRO_REDIS_PASSWORD` | **REQUIRED** | Read directly by `infrastructure/redis` at connect time. Note it is **not** checked by `Config.Validate`; a wrong value surfaces as `redis-connect-failed` at startup, which is still fail-fast, just later. |
| `AUSTRO_RABBITMQ_URL` | **REQUIRED** | Derived in compose from `amqp://<user>:<password>@rabbitmq:5672/`. |
| `AUSTRO_RABBITMQ_USER` | **REQUIRED** | `SAFE DEFAULT` `austro`. |
| `AUSTRO_RABBITMQ_QUEUE` | **SAFE DEFAULT** `austro.events` | Declared durable and non-auto-delete by both processes. |

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
| Loopback DSNs (`localhost:5432`, `localhost:6379`, `localhost:5672`) | Rejected outright by the validator. |
| `change-me*` placeholders | Rejected outright by the validator, and by `scripts/deploy.sh`. |
| Comparing `docker-compose.yml` for production use | The dev compose publishes database, cache and broker on loopback and enables no auth on Redis' peer ports. It is a development file. Production is `docker-compose.production.yml`. |
