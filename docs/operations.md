# AUSTRO OS — Operations

Day-to-day operation of a running production deployment: the commands, the
startup order, the log lines that mean something, and where to look when a
specific thing is wrong.

Every command below assumes you are in the repository root with
`.env.production` present. The compose invocation is long, so it is defined once:

```bash
cd /path/to/austro-os
DC="docker compose --env-file .env.production -f docker-compose.production.yml"
```

The `--env-file` is not optional: it is what compose interpolates `${...}`
against, and it is the same file the containers receive through `env_file:`.

---

## Startup order

```
postgres  ->  redis  ->  rabbitmq  ->  api  ->  worker  ->  reverse-proxy
```

| Step | Why it is here |
|---|---|
| `postgres`, `redis`, `rabbitmq` | The API exits at startup if it cannot reach any of the three. Starting it first only produces a crash loop. |
| `api` | Applies the schema bootstrap, provisions the RLS roles and verifies the runtime role against the live catalog before it opens its listener. |
| `worker` | Runs the same bootstrap. It waits for the API to be **healthy**, not merely started, because that bootstrap takes no advisory lock and two concurrent runs against a cold database would collide on the same DDL. |
| `reverse-proxy` | Last, so it never serves 502s while the stack is still coming up. |

`scripts/deploy.sh` performs exactly this order and waits on each container's own
healthcheck. Use it rather than typing the sequence by hand.

**Shutdown is the reverse in effect**: the proxy stops first (stop accepting new
traffic), then the worker, then the API, which drains in-flight requests for up
to 75 seconds before exiting. `stop_grace_period` is 90s on the API specifically
so `docker stop` reaches that drain instead of killing it.

---

## Everyday commands

### Status

```bash
$DC ps
docker compose --env-file .env.production -f docker-compose.production.yml ps --format 'table {{.Service}}\t{{.Status}}'
docker stats --no-stream
```

### Logs

```bash
$DC logs -f api                 # follow the API
$DC logs -f worker              # follow the worker
$DC logs --tail=200 api worker  # recent lines from both
$DC logs --since=30m api        # a time window
```

Logs are JSON on stdout and are captured by the Docker logging driver, capped at
10 MB × 3 files per container (configured in the compose file).

### Health

```bash
scripts/healthcheck.sh          # full live-topology check; exit 0 = all passed

# Individual probes, without leaving the host:
$DC exec api wget -qO- http://127.0.0.1:8080/health/live
$DC exec api wget -qO- http://127.0.0.1:8080/health/ready
curl -I https://app.yourdomain.com/health/live     # through the proxy
```

| Endpoint | Answers | Meaning |
|---|---|---|
| `/health/live` | `{"status":"ok"}` | The process is listening. |
| `/health/ready` | `{"ready":true}` | Served by the same mux once the listener is open. |

**Be accurate about what these prove:** both endpoints are shallow. The listener
only opens *after* the schema bootstrap and the runtime-privilege verification
have completed, so a 200 does mean the process got that far — but neither
endpoint re-checks PostgreSQL, Redis or RabbitMQ on each call. The per-dependency
truth is `scripts/healthcheck.sh` and the container healthchecks, which is why
those are what a deployment gates on.

### Deployment, backup, restore

```bash
scripts/deploy.sh                                  # deploy / update
scripts/backup.sh                                  # ad-hoc backup
scripts/restore.sh backups/austro-postgres-<ts>.sql.gz   # DESTRUCTIVE, asks first
```

Details: [production-deployment.md](production-deployment.md),
[backups-and-restore.md](backups-and-restore.md).

### Restart

```bash
$DC restart api                 # one service
$DC restart api worker          # both application processes
$DC restart                     # everything (rarely what you want)

# Recreate a single service after a config or image change:
$DC up -d --force-recreate --no-deps api
```

### Shutdown

```bash
$DC stop                        # stop, keep containers and volumes
$DC down                        # remove containers and network, KEEP volumes
$DC down -v                     # DESTROYS VOLUMES INCLUDING postgres_data
```

Never run `down -v` against a deployment whose data you intend to keep.

### Shell access for diagnosis

```bash
$DC exec api sh
$DC exec postgres psql -U austro -d austro
$DC exec redis sh -c 'redis-cli --no-auth-warning ping'
$DC exec rabbitmq rabbitmqctl status
```

The API container is read-only with a tmpfs at `/tmp`; nothing needs to write,
and that is deliberate rather than a limitation to work around.

---

## Log lines that mean something

The application logs structured JSON. These messages are the ones an operator
actually needs.

| Message | Meaning |
|---|---|
| `invalid-configuration` | Startup refused: required settings missing or insecure. The error names settings, never values. |
| `database-topology-ready` | Bootstrap finished and the runtime role was verified against the live catalog. Carries `runtime_role`, `admin_role`, `rls_tables_forced`. |
| `database-runtime-security-failed` | **Serious.** The serving role is a superuser, holds `BYPASSRLS`, owns a protected table, or can see another workspace's rows. The process exits. |
| `redis-connection-established` | The authenticated ping succeeded. |
| `event-sink-failed` | Could not reach RabbitMQ at startup. The process exits. |
| `audit-store-failed` | The persistent audit writer is unavailable. The process exits — the security model requires durable audit evidence. |
| `austro-os-serving` | The listener is open. Startup is complete. |
| `austro-os-shutting-down` / `austro-os-stopped` | Graceful drain began / the process exited cleanly. |
| `worker-composed` | The worker composed its runtime (carries the selected AI and publishing backends). |
| `worker-started` | The worker attached to the queue. |
| `worker-reconnected` | The worker redialled the broker after a forced close. |
| `vector-index-status`, `pgcrypto-extension-status` | Warning-level: an extension or index could not be confirmed. Not fatal, worth reading. |

Quick checks against a running stack:

```bash
$DC logs api    | grep '"message":"database-topology-ready"'
$DC logs api    | grep -c 'database-runtime-security-failed'   # expect 0
$DC logs worker | grep '"message":"worker-started"'
```

---

## Routine operations calendar

| Frequency | Task |
|---|---|
| Daily (or automated) | Confirm `scripts/healthcheck.sh` exits 0; confirm last night's backup exists and is non-empty |
| Weekly | Read `$DC ps` and disk usage; confirm no container is restart-looping (`docker inspect` restart count) |
| Monthly | **Run a restore exercise** into a scratch host — see [backups-and-restore.md](backups-and-restore.md). An untested backup is an assumption, not a control. |
| Quarterly | Rotate the JWT signing keys; review who holds the founder credential and the Docker socket |
| On every certificate expiry window | Verify renewal (automatic with Caddy; a calendar item with nginx) |
| Before every release | Confirm a rollback target exists and a fresh backup is present |

---

## Capacity signals to watch

| Signal | Where | Why it matters |
|---|---|---|
| Disk free on `/var/lib/docker` | host | PostgreSQL data, WAL and container logs all grow. Also your backups, if local. |
| PostgreSQL connection count | `$DC exec postgres psql -U austro -d austro -c 'SELECT count(*) FROM pg_stat_activity'` | Each API and worker process holds a pool. |
| Queue depth | `$DC exec rabbitmq rabbitmqctl list_queues name messages` | A steadily growing `austro.events` means the worker is not keeping up or is down. |
| Container restarts | `docker inspect --format '{{.RestartCount}}' <id>` | A crash loop is invisible in `ps` if the restart policy hides it. |
| Redis memory | `$DC exec redis sh -c 'redis-cli --no-auth-warning INFO memory'` | The memory bank grows with workspace activity. |

---

## Quick reference: what is exposed

| Port | Exposure | Service |
|---|---|---|
| 80 | public | reverse proxy — HTTP→HTTPS redirect |
| 443 | public | reverse proxy — the application |
| 8080 | **none** (compose network only) | API |
| 5432, 6379, 5672, 15672 | **none** (internal network only) | PostgreSQL, Redis, RabbitMQ, and RabbitMQ's management UI |

If a port in the second group is reachable from outside the host, something has
been changed from the shipped configuration. Treat it as an incident.
