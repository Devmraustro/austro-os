# AUSTRO OS — Troubleshooting

Symptom → what it means → how to confirm → what to do.

Every command assumes you are in the repository root with `.env.production`
present:

```bash
DC="docker compose --env-file .env.production -f docker-compose.production.yml"
```

Start with the live check; it usually names the failing layer for you:

```bash
scripts/healthcheck.sh
```

---

## API unavailable

**Symptom:** `https://<hostname>` returns 502/504 from the proxy, or the `api`
container is restarting.

```bash
$DC ps api
$DC logs --tail=100 api
```

| Log line | Cause | Fix |
|---|---|---|
| `invalid-configuration` | A required setting is missing or still a placeholder. The message names the setting. | Fix `.env.production`; re-run `scripts/deploy.sh`. |
| `database-runtime-security-failed` | The serving role is a superuser / has `BYPASSRLS` / owns a protected table / can read across workspaces. | See *Runtime DB security invalid* below. This one is serious. |
| `audit-store-failed` | The persistent audit writer could not initialise. | Check PostgreSQL connectivity and that `audit_events` exists. |
| `event-sink-failed` | RabbitMQ unreachable at startup. | See *RabbitMQ unavailable*. |
| `webui-load-failed` | The embedded browser assets failed to load. | A build problem — rebuild the image. |
| `http-server-failed` | The listener could not bind. | Port already in use inside the container; check for a stray process. |

If the container exits immediately with none of the above, read the exit code:

```bash
docker inspect --format '{{.State.ExitCode}} {{.State.Error}}' "$($DC ps -q api)"
```

---

## Database unavailable

**Symptom:** the API logs `database-bootstrap-failed` or
`database-runtime-open-failed`, or RestartLoop on `api`.

```bash
$DC ps postgres
$DC logs --tail=100 postgres
$DC exec postgres pg_isready -U austro -d austro
$DC exec postgres psql -U austro -d austro -c 'SELECT 1'
```

| Cause | Confirm | Fix |
|---|---|---|
| Container not healthy yet | `$DC ps postgres` shows `starting` | Wait; first init takes ~30s. If it never becomes healthy, read the logs. |
| Init failed on a bad volume | logs mention `initdb` or a permission error | A pre-existing volume with unexpected ownership. Do **not** delete it casually — that is the database. Back it up first. |
| Wrong password | `password authentication failed` in logs | `AUSTRO_POSTGRES_PASSWORD` and the password the volume was initialised with must match. Changing the env var does **not** change the stored password. |
| Disk full | host `df -h`, PostgreSQL log mentions `No space left` | Free space; PostgreSQL needs room for WAL. |

---

## Redis unavailable

**Symptom:** the API logs `redis-connect-failed` and exits.

```bash
$DC exec redis sh -c 'redis-cli --no-auth-warning ping'
```

- `PONG` — Redis is fine; the application's credential is wrong. Check
  `AUSTRO_REDIS_PASSWORD` matches the `--requirepass` value the container was
  started with (both come from the same variable, so a mismatch means the
  container predates an edit — recreate it).
- `NOAUTH Authentication required` — the ping was unauthenticated. Use
  `REDISCLI_AUTH` as the healthcheck does.
- `Could not connect` — the container is down. `$DC ps redis`, `$DC logs redis`.

Note: `AUSTRO_REDIS_PASSWORD` is not checked by the configuration validator (it
is read directly at connect time), so a wrong value surfaces here rather than at
startup validation.

---

## RabbitMQ unavailable

**Symptom:** the API logs `event-sink-failed`, or the worker logs
`Failed to connect to RabbitMQ` and exits.

```bash
$DC ps rabbitmq
$DC logs --tail=100 rabbitmq
$DC exec rabbitmq rabbitmqctl status
$DC exec rabbitmq rabbitmqctl list_queues name messages
```

| Cause | Fix |
|---|---|
| Still starting | `start_period` is 45s; a broker takes a while. Wait. |
| Wrong credentials | Recreate the container after editing `AUSTRO_RABBITMQ_USER` / `_PASSWORD`: the broker is initialised with them and does not pick up changes on restart. |
| Hostname/Erlang cookie mismatch on a restored volume | Restoring `rabbitmq_data` onto a host with a different name can confuse the node. For a broker holding only transient state, recreating the volume is usually the right call — confirm with your own judgement about in-flight messages. |
| Queue missing | `worker-started` never appears; declare happens automatically at startup, so check the worker's logs. |

---

## Worker stopped

**Symptom:** the queue depth grows; nothing is processed; `worker-started` is
absent from the worker's log.

```bash
$DC ps worker
$DC logs --tail=100 worker
$DC exec rabbitmq rabbitmqctl list_queues name messages
```

| Log line | Meaning | Fix |
|---|---|---|
| `Failed to start worker` | Could not consume from the queue | Check RabbitMQ and the queue name. |
| `worker-shutting-down` | Received SIGTERM | Something stopped it — check for an OOM kill (`docker inspect` → `.State.OOMKilled`). |
| `worker-reconnected` | The broker closed the connection and the worker redialled | Informational; a burst of these means broker instability. |
| `Failed to compose runtime` | Backend selection rejected at composition | Usually an AI/publishing setting supplied while the stub backend is selected. |

The container healthcheck is a **liveness** check on PID 1, not a "consuming"
check — a running worker that is failing every message still reports healthy.
`scripts/healthcheck.sh` is what asserts on the `worker-started` log line.

---

## Migrations failed

**Symptom:** the API exits and the log shows `database-bootstrap-failed: <step>:
<error>`. The step name identifies where it stopped (`extensions`, `tables`,
`migrate-*`, `enable-rls`, `roles`, `table-ownership`, `policies`, `force-rls`).

```bash
$DC logs api | grep database-bootstrap-failed
```

| Step in the message | Likely cause |
|---|---|
| `extensions` | The `vector` or `pgcrypto` extension is unavailable. On a managed database it may need enabling by the provider. |
| `roles` | The owner role cannot `CREATE ROLE` — typical on a managed service that forbids it. This application requires the ability to create the runtime and admin roles. |
| `table-ownership` | An existing table is owned by the wrong role. Ownership separation is enforced, not assumed. |
| `policies` | A policy references a role that does not exist — usually a half-applied earlier run. Re-running the bootstrap is safe and idempotent. |

Migrations are applied automatically at every start and are idempotent, so a
transient failure is often resolved by letting the container restart. If it
persists, restore the pre-deployment backup (see
[backups-and-restore.md](backups-and-restore.md)) rather than editing the schema
by hand.

---

## Configuration invalid

**Symptom:** startup log `invalid-configuration`, or `scripts/deploy.sh` stops
with `ERROR: <NAME> is ...`.

The error names settings, never values. Common causes:

| Message mentions | Cause |
|---|---|
| `AUSTRO_POSTGRES_DSN` | Contains `localhost:5432`, `change-me`, or `example` — all rejected as insecure. |
| `AUSTRO_POSTGRES_RUNTIME_DSN` | Equal to the owner DSN. The runtime role must not be the owner. |
| `AUSTRO_JWT_SECRET` / `AUSTRO_JWT_REFRESH_SECRET` | Shorter than 32 bytes, or a placeholder. |
| `AUSTRO_FOUNDER_USERNAME/AUSTRO_POSTGRES_PASSWORD` | Only one of the founder pair was set. |
| `AUSTRO_AI_*` or `AUSTRO_PUBLISH_*` | A real backend was selected without its settings, **or** settings were supplied while the `stub` backend was selected (a configured credential is never silently ignored). |
| `AUSTRO_PUBLISH_MAX_ATTEMPTS` / `_BACKOFF_*` | Supplied but unparseable. A typo is an error, not a reason to fall back. |

Reproduce the resolution locally before deploying:

```bash
$DC config -q && echo "compose file resolves"
$DC config | grep -A5 'AUSTRO_ENV'
```

---

## Runtime DB security invalid

**Symptom:** `database-runtime-security-failed` — the process refuses to serve.

This is the single most important failure in the system, and it is a **fail
closed** by design: if the serving role is not constrained, every workspace
policy would be decorative.

```bash
$DC exec postgres psql -U austro -d austro -c "
SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname LIKE 'austro%';"

$DC exec postgres psql -U austro -d austro -c "
SELECT c.relname, r.rolname AS owner
  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  JOIN pg_roles r ON r.oid=c.relowner
 WHERE n.nspname='public' AND c.relkind='r' AND r.rolname LIKE 'austro%';"
```

| Finding | Fix |
|---|---|
| `rolsuper = true` on the runtime role | Recreate the role as a non-superuser. Do not "work around" this by granting superuser to make startup stop failing. |
| `rolbypassrls = true` | `ALTER ROLE austro_app NOBYPASSRLS;` |
| Runtime role owns a protected table | `ALTER TABLE <t> OWNER TO austro;` then re-run — the bootstrap asserts ownership as a step. |
| Fewer than 12 forced-RLS tables | A table is missing `FORCE ROW LEVEL SECURITY`. The bootstrap applies it; check for a failed `force-rls` step. |

---

## Audit failures

**Symptom:** the API logs `audit-persist-failed`, or `audit-store-failed` at
startup; or `GET /audit/verification` reports the chain is not intact.

```bash
$DC logs api | grep -E 'audit-store-failed|audit-persist-failed'
$DC exec postgres psql -U austro -d austro -c \
  "SELECT count(*), min(seq), max(seq) FROM audit_events;"
```

| Finding | Meaning |
|---|---|
| `audit-store-failed` at startup | The writer could not initialise and the process exited — durable audit evidence is a startup requirement, not optional. Check PostgreSQL. |
| `audit-persist-failed` at runtime | An audited operation could not be recorded. Login is designed to **fail closed** when the audit write fails, so a burst of login failures alongside this message is expected behaviour, not a second bug. |
| Gaps in `seq` | The chain is the authority; `seq` is a read-ordering aid. Verify through `GET /audit/verification`, which recomputes the hash chain, rather than inferring from `seq`. |

After a restore, gaps or a mismatch usually mean the dump was taken mid-write or
was restored out of order. Restore again from a verified artifact.

---

## Event sink failures

**Symptom:** `event-sink-failed`, or pipelines advance and then stall.

- At startup, `event-sink-failed` means the RabbitMQ connection could not be
  established; see *RabbitMQ unavailable*.
- At runtime, the sink is **self-supervising**: when the broker force-closes the
  connection, it redials with bounded exponential backoff (500 ms doubling to 5 s)
  rather than silently dropping pipeline-advance events. A brief gap is expected;
  sustained failure is a broker problem.
- If events are published but nothing advances, check the worker — it is the
  consumer. Confirm the queue name the API publishes to and the worker consumes
  from are identical: both default to `austro.events`, and
  `AUSTRO_RABBITMQ_QUEUE` must be the same for both (it is set once in the shared
  environment block; a per-service override is the way this breaks).

---

## HTTPS failure

**Symptom:** the proxy container restarts, or browsers report a certificate
error.

```bash
$DC ps reverse-proxy
$DC logs --tail=100 reverse-proxy
ls -l deploy/tls/
```

| Cause | Confirm | Fix |
|---|---|---|
| Certificate files missing or misnamed | `ls deploy/tls/` lacks `fullchain.pem` or `privkey.pem` | They must carry exactly those names; `deploy/nginx/nginx.conf` references them. |
| Key not readable by the nginx worker | logs mention `permission denied` | The worker runs as uid 101; make the key readable by it while keeping mode 600 where possible (`chown 101:101 privkey.pem`). |
| nginx config rejected | `nginx -t` output in the container logs | Validate with `$DC exec reverse-proxy nginx -t`. |
| Ports 80/443 already in use | logs mention `bind() ... address already in use` | Something else holds the port on the host. |
| ACME challenge failing (Caddy) | Caddy logs mention the challenge | The hostname must resolve to this host and ports 80/443 must be reachable from the internet. |
| Certificate expired | browser warning | With nginx, renewal is yours to schedule. With Caddy it is automatic — check the `caddy_data` volume still exists. |

Verify TLS directly:

```bash
curl -I https://<hostname>/health/live
openssl s_client -connect <hostname>:443 -servername <hostname> </dev/null 2>/dev/null | openssl x509 -noout -dates
```

Confirm the redirect works and the minimum version is enforced:

```bash
curl -I http://<hostname>/health/live                                   # expect 301
openssl s_client -connect <hostname>:443 -tls1_1 2>&1 | grep -i 'handshake failure\|alert'
```

---

## Login failure

**Symptom:** `POST /api/auth/login` returns 401, or 500, or works and then
immediately fails again.

| Response | Cause |
|---|---|
| **401** | Wrong credentials, **or** the founder has never been bootstrapped (`POST /api/auth/bootstrap` first, once). |
| **409** on bootstrap | A founder already exists. The endpoint is not a credential-reset path. |
| **500** alongside `audit-persist-failed` in the log | **By design.** A login whose audit record cannot be written fails closed rather than granting a session with no evidence. Fix the audit store, not the login handler. |
| Login succeeds, next request 401 | The access token expired, or `AUSTRO_JWT_SECRET` changed (which invalidates every token in circulation). Expect a fleet-wide re-login after rotating it. |
| Login succeeds, `/api/me` 403 | An authorization rule rejected the action. Rules are deny-by-default; check the RBAC rule for the route rather than assuming a session problem. |

---

## Cross-workspace access symptoms

**Symptom:** a request returns 403/404 for a record that exists, or you believe
a caller can see another workspace's data. The first is expected behaviour; the
second is an incident.

Expected behaviour to expect: the workspace is bound from **verified JWT claims**,
never from a client-supplied field. A request carrying another workspace's id in
the path or body is answered as if the resource were absent, because the store
binds the workspace inside the transaction.

If you suspect a real leak, confirm the enforcement path first:

```bash
# 1. Is RLS actually forced on every protected table?
$DC exec postgres psql -U austro -d austro -tAc "
SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relforcerowsecurity;"      # expect >= 12

# 2. Is the runtime role constrained? (should be false,false)
$DC exec postgres psql -U austro -d austro -tAc "
SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname='austro_app';"

# 3. Did the API report a verified topology at startup?
$DC logs api | grep '"message":"database-topology-ready"'
```

If (1) or (2) is wrong, treat it as the *Runtime DB security invalid* case above:
the application is designed to refuse to start in that state, so a running
process with bad values means something changed underneath it.

Run the RLS test suites against a live database to probe the behaviour directly:

```bash
go test ./tests/ -count=1 -run 'TestRuntimeRole|TestProtectedTablesHaveForcedRLS|TestBootstrapAndMigrationsAreIdempotent'
```

---

## Nothing is obviously wrong, but the stack does not come up

Work from the bottom of the dependency chain, using the container health
statuses, and let each log tell you the next step:

```bash
$DC ps
scripts/healthcheck.sh

for s in postgres redis rabbitmq api worker reverse-proxy; do
  echo "===== $s ====="
  $DC ps "$s"
  $DC logs --tail=30 "$s"
done
```

Remember the two ordering constraints that explain most "it should be up" cases:

- The API only opens its listener **after** the schema bootstrap and the runtime
  privilege verification complete. On a cold database that is the slowest part of
  a first deployment.
- The worker waits for the API to be **healthy**. If the API never becomes
  healthy, the worker never starts, and that is the intended behaviour rather
  than a second, independent fault.
